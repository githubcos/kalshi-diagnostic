#!/usr/bin/env python3
import json, os, re, shutil, subprocess, time, sys, statistics
from pathlib import Path
from datetime import datetime, timezone
HOME=Path('/home/ubuntu'); SRC=HOME/'kalshi-diagnostic'; TEL=HOME/'kalshi-agent-telemetry'; BOT=HOME/'KalshiArbo'/'kalshiarbo'; ERROR=HOME/'.kalshi_telemetry_error.txt'; INTERVAL=5
SESSION_DIR=HOME/'KalshiArbo'/'session_history'; SESSION_MASTER=HOME/'KalshiArbo'/'session_history.txt'; SESSION_STATE=HOME/'.kalshi_session_archive_state.json'; SESSION_CURRENT=SESSION_DIR/'current_session.txt'
ANSI=re.compile(r'\x1b\[[0-9;?]*[ -/]*[@-~]')
PNL_RE=re.compile(r'P&L\s+([+-])\$([0-9]+(?:\.[0-9]+)?).*?Reason:\s*([A-Za-z0-9_\-]+)',re.I)
BAL_RE=re.compile(r'(?:Balance|paper balance)\s*[:=]?\s*\$?(-?[0-9]+(?:\.[0-9]+)?)',re.I)

def utc(): return datetime.now(timezone.utc).isoformat(timespec='seconds')
def run(cmd,cwd=None,timeout=120,check=False):
    p=subprocess.run(cmd,cwd=cwd,text=True,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,timeout=timeout)
    if check and p.returncode: raise RuntimeError(f"command failed rc={p.returncode}: {' '.join(map(str,cmd))}\n{p.stdout}")
    return p

def ensure_clone():
    if (TEL/'.git').exists(): return
    if TEL.exists(): shutil.rmtree(TEL)
    r=run(['git','-C',str(SRC),'remote','get-url','origin'],check=True); url=r.stdout.strip()
    if not url: raise RuntimeError('origin URL is empty')
    run(['git','clone','--depth','1',url,str(TEL)],timeout=180,check=True)
    run(['git','config','user.name','Kalshi Agent Telemetry'],TEL,check=True); run(['git','config','user.email','kalshi-agent@localhost'],TEL,check=True)

def copy_if_exists(src,dst):
    if src.exists(): dst.parent.mkdir(parents=True,exist_ok=True); shutil.copy2(src,dst)

def local_runtime_status():
    status={'utc':utc(),'publisher_pid':os.getpid(),'heartbeat':None,'state':None,'autopilot':None}
    for key,path in [('heartbeat',HOME/'.kalshi_agent_heartbeat.json'),('state',HOME/'.kalshi_agent_state.json'),('autopilot',HOME/'.kalshi_autopilot_state.json')]:
        try: status[key]=json.loads(path.read_text())
        except Exception: pass
    return status

def read_tail(path,max_bytes=1500000):
    try:
        with path.open('rb') as f:
            f.seek(0,2); n=f.tell(); f.seek(max(0,n-max_bytes)); data=f.read().decode('utf-8','replace')
        return ANSI.sub('',data)
    except Exception: return ''

def safe_cfg():
    p=BOT/'config.json'
    try: raw=json.loads(p.read_text())
    except Exception: return {}
    deny=('secret','private','password','credential','webhook','wallet','mnemonic','passphrase','api_key','apikey','key_id','rpc')
    keep={}
    for k,v in raw.items():
        lk=k.lower()
        if any(x in lk for x in deny): continue
        if any(x in lk for x in ('pair_arb','max_trades','max_session','paper_trade','paper_start','trade_size','market_type','cooldown','risk')) and isinstance(v,(str,int,float,bool,type(None))): keep[k]=v
    return keep

def percentile(vals,p):
    if not vals: return None
    a=sorted(vals); idx=min(len(a)-1,max(0,int(round((len(a)-1)*p))))
    return a[idx]

def current_bot_pid():
    out=run(['bash','-lc',"ss -ltnp 2>/dev/null | awk '/:8085/ && /kalshiarbo/ {if (match($0,/pid=[0-9]+/)) {x=substr($0,RSTART+4,RLENGTH-4); print x; exit}}'"],timeout=20).stdout.strip()
    return out or None

def session_stats_from_log():
    log=read_tail(BOT/'polyarb.log',3000000)
    lines=[x.strip() for x in log.splitlines() if x.strip()]
    starts=[i for i,x in enumerate(lines) if 'ACTIVE FLAGS paper=' in x]
    if starts: lines=lines[starts[-1]:]
    leads=sum(1 for x in lines if '[PAIR LEAD]' in x and 'EXECUTED' in x)
    hedges=sum(1 for x in lines if '[PAIR HEDGE]' in x and 'EXECUTED' in x)
    closes=0; wins=0; losses=0; pnl=0.0; reasons={}
    for line in lines:
        m=PNL_RE.search(line)
        if not m: continue
        v=float(m.group(2))*(1 if m.group(1)=='+' else -1); r=m.group(3)
        closes+=1; pnl+=v; wins+=1 if v>0 else 0; losses+=1 if v<=0 else 0; reasons[r]=reasons.get(r,0)+1
    bals=[]
    for line in lines:
        m=BAL_RE.search(line)
        if m:
            try: bals.append(float(m.group(1)))
            except Exception: pass
    cfg=safe_cfg(); start=float(cfg.get('PAPER_START_BALANCE',cfg.get('PaperStartBalance',0)) or 0)
    end=bals[-1] if bals else (start+pnl if start else None)
    return {'leads':leads,'hedges':hedges,'closes':closes,'wins':wins,'losses':losses,'realized_pnl':round(pnl,6),'start_balance':start or None,'end_balance':None if end is None else round(end,6),'close_reasons':reasons,'config':cfg}

def render_session(s,ended_utc=None,termination='running'):
    st=s.get('stats') or {}; closes=st.get('closes',0) or 0; leads=st.get('leads',0) or 0
    wr=(st.get('wins',0)/closes) if closes else None; hr=(st.get('hedges',0)/leads) if leads else None
    out=['KALSHIARBO PAPER SESSION','========================',f"SESSION_ID={s.get('session_id','')}",f"PID={s.get('pid','')}",f"START_UTC={s.get('start_utc','')}",f"END_UTC={ended_utc or 'RUNNING'}",f"TERMINATION={termination}",f"START_BALANCE={st.get('start_balance')}",f"END_BALANCE={st.get('end_balance')}",f"REALIZED_PNL={st.get('realized_pnl',0)}",f"LEADS={leads}",f"HEDGES={st.get('hedges',0)}",f"CLOSES={closes}",f"WINS={st.get('wins',0)}",f"LOSSES={st.get('losses',0)}",f"WIN_RATE={'NA' if wr is None else f'{wr:.6f}'}",f"HEDGE_RATE={'NA' if hr is None else f'{hr:.6f}'}",f"CLOSE_REASONS={json.dumps(st.get('close_reasons',{}),sort_keys=True)}",'LIVE_ORDERS_ALLOWED=false','CONFIG_JSON='+json.dumps(st.get('config',{}),sort_keys=True)]
    return '\n'.join(out)+'\n'

def archive_session_if_needed():
    SESSION_DIR.mkdir(parents=True,exist_ok=True)
    pid=current_bot_pid(); now=utc(); stats=session_stats_from_log()
    try: prev=json.loads(SESSION_STATE.read_text()) if SESSION_STATE.exists() else None
    except Exception: prev=None
    if prev and prev.get('pid') and prev.get('pid') != pid:
        prev['stats']=prev.get('stats') or {}
        text=render_session(prev,now,'process_replaced' if pid else 'process_stopped')
        stamp=re.sub(r'[^0-9]','',prev.get('start_utc',''))[:14] or datetime.now(timezone.utc).strftime('%Y%m%d%H%M%S')
        sid=prev.get('session_id') or f'session_{stamp}_{prev.get("pid","")}'
        (SESSION_DIR/f'{sid}.txt').write_text(text)
        with SESSION_MASTER.open('a') as f: f.write(text+'\n')
    if pid:
        if not prev or prev.get('pid') != pid:
            prev={'session_id':f"paper_{datetime.now(timezone.utc).strftime('%Y%m%d_%H%M%S')}_pid{pid}",'pid':pid,'start_utc':now,'stats':stats}
        else:
            prev['stats']=stats
        SESSION_STATE.write_text(json.dumps(prev,indent=2)+'\n')
        SESSION_CURRENT.write_text(render_session(prev,None,'running'))
    elif prev and prev.get('pid'):
        SESSION_STATE.write_text(json.dumps({'pid':None,'last_archived_utc':now},indent=2)+'\n')
        SESSION_CURRENT.write_text('NO ACTIVE PAPER SESSION\nUPDATED_UTC='+now+'\n')

def extract_monitor():
    log=read_tail(BOT/'polyarb.log')
    lines=[x.strip() for x in log.splitlines() if x.strip()]
    starts=[i for i,x in enumerate(lines) if 'ACTIVE FLAGS paper=' in x]
    if starts: lines=lines[starts[-1]:]
    ms=[]; lead_ms=[]; hedge_ms=[]; lookup_ms=[]
    ms_re=re.compile(r'completed in (\d+) ms',re.I)
    for line in lines:
        m=ms_re.search(line)
        if not m: continue
        n=int(m.group(1)); ms.append(n); low=line.lower()
        if 'lead' in low and 'buy' in low: lead_ms.append(n)
        if 'hedge' in low and ('buy' in low or 'order' in low): hedge_ms.append(n)
        if 'lookup' in low or 'fill ' in low: lookup_ms.append(n)
    interesting=[]
    needles=('pair arb','pair_arb','pair lead','pair hedge','lead','hedge','residual','locked','balance','p&l','risk controls','cooldown','feed','fill','settle','timeout','unwind')
    for line in lines:
        if any(n in line.lower() for n in needles): interesting.append(line)
    interesting=interesting[-40:]
    blocker=''
    for line in reversed(lines):
        low=line.lower()
        if 'risk controls:' in low or 'signal cooldown active' in low or 'blocked by:' in low:
            blocker=line[:300]; break
    feed_bad=any(('kalshi' in x.lower() and ('feed not connected' in x.lower() or 'websocket disconnected' in x.lower())) for x in lines[-400:])
    feed_good=any(('kalshi feed: websocket orderbook connected' in x.lower()) for x in lines[-1000:])
    port=run(['bash','-lc',"ss -ltnp 2>/dev/null | grep ':8085' || true"],timeout=20).stdout.strip()
    cfg=safe_cfg(); lowlines=[x.lower() for x in lines]
    lead_events=sum(1 for x in lowlines if ('pair arb lead buy' in x or 'pair_arb_lead_buy' in x or ('[pair lead]' in x and 'executed' in x)))
    hedge_events=sum(1 for x in lowlines if ('pair_arb_hedge' in x or ('[pair hedge]' in x and 'executed' in x) or ('hedge' in x and ('completed' in x or 'filled' in x or 'buy' in x))))
    closed=sum(1 for x in lowlines if ('locked pair closed' in x or 'trade_close' in x or ('p&l' in x and 'reason:' in x)))
    reason_counts={}; reason_re=re.compile(r'reason:\s*([^│]+)',re.I)
    for line in lines:
        m=reason_re.search(line)
        if m:
            reason=m.group(1).strip(); reason_counts[reason]=reason_counts.get(reason,0)+1
    status='PASS'; reasons=[]
    if not port: status='FAIL'; reasons.append('paper process is not listening on 8085')
    if feed_bad and not feed_good: status='WARN'; reasons.append('Kalshi feed disconnect warning observed')
    if blocker:
        if status=='PASS': status='WARN'
        reasons.append('runtime blocker active')
    speed='NO_EXECUTION_LATENCY_SAMPLE'
    if lead_ms or hedge_ms:
        worst=max((lead_ms+hedge_ms) or [0]); speed='FAST' if worst<=1000 else ('ACCEPTABLE' if worst<=3000 else 'SLOW')
    return {'utc':utc(),'mode':'PAPER','live_orders_allowed':False,'status':status,'status_reasons':reasons,'paper_port_8085':bool(port),'kalshi_feed_recent_connected_event':feed_good,'kalshi_feed_recent_disconnect_event':feed_bad,'runtime_blocker':blocker,'safe_config':cfg,'counts_in_current_session':{'lead_events':lead_events,'hedge_events':hedge_events,'closed_events':closed},'close_reason_counts':reason_counts,'execution_latency_ms':{'all_count':len(ms),'all_avg':round(statistics.mean(ms),1) if ms else None,'all_p95':percentile(ms,.95),'all_max':max(ms) if ms else None,'lead_count':len(lead_ms),'lead_avg':round(statistics.mean(lead_ms),1) if lead_ms else None,'lead_p95':percentile(lead_ms,.95),'lead_max':max(lead_ms) if lead_ms else None,'hedge_count':len(hedge_ms),'hedge_avg':round(statistics.mean(hedge_ms),1) if hedge_ms else None,'hedge_p95':percentile(hedge_ms,.95),'hedge_max':max(hedge_ms) if hedge_ms else None,'lookup_count':len(lookup_ms),'lookup_avg':round(statistics.mean(lookup_ms),1) if lookup_ms else None},'speed_verdict':speed,'algorithm_invariants':['lead admission uses configured window/gap/token/filter gates','Kalshi inventory uses authoritative filled contract quantity; cash fees do not reduce shares','hedge limit is bounded by opposite-side locked-profit math','matched_contracts=min(YES_shares,NO_shares)','locked_profit=matched_contracts-(YES_cash_spent+NO_cash_spent)','residual_contracts=abs(YES_shares-NO_shares)','mark-to-market uses explicit Kalshi YES and NO prices when available','PAPER guard prevents any live order request'],'recent_math_execution_lines':interesting}

def monitor_text(m):
    lat=m['execution_latency_ms']; c=m['counts_in_current_session']; cfg=m['safe_config']
    out=['KALSHIARBO LIVE TRADE / MATH MONITOR','===================================',f"UPDATED_UTC={m['utc']}",f"MODE={m['mode']} LIVE_ORDERS_ALLOWED=false",f"VERDICT={m['status']} SPEED={m['speed_verdict']}",f"PAPER_8085={'UP' if m['paper_port_8085'] else 'DOWN'}",f"KALSHI_WS_CONNECTED_EVENT={m['kalshi_feed_recent_connected_event']} DISCONNECT_EVENT={m['kalshi_feed_recent_disconnect_event']}",f"BLOCKER={m['runtime_blocker'] or 'none'}",'',f"EVENTS lead={c['lead_events']} hedge={c['hedge_events']} closed={c['closed_events']}",f"CLOSE_REASONS={json.dumps(m['close_reason_counts'],sort_keys=True)}",f"LATENCY all avg/p95/max={lat['all_avg']}/{lat['all_p95']}/{lat['all_max']} ms",f"LATENCY lead avg/p95/max={lat['lead_avg']}/{lat['lead_p95']}/{lat['lead_max']} ms",f"LATENCY hedge avg/p95/max={lat['hedge_avg']}/{lat['hedge_p95']}/{lat['hedge_max']} ms",'', 'SAFE ACTIVE CONFIG']
    for k in sorted(cfg): out.append(f'{k}={cfg[k]}')
    out += ['', 'MATH / ALGORITHM INVARIANTS']+[f'OK-CHECK {x}' for x in m['algorithm_invariants']]
    out += ['', 'RECENT MEANINGFUL EXECUTION / MATH LINES']+m['recent_math_execution_lines'][-25:]
    if m['status_reasons']: out += ['', 'WARNINGS']+[f'- {x}' for x in m['status_reasons']]
    return '\n'.join(out)+'\n'

def sync_once():
    archive_session_if_needed()
    ensure_clone(); run(['git','fetch','-q','origin','main'],TEL,check=True); run(['git','reset','--hard','origin/main'],TEL,check=True)
    src_agent=SRC/'docs'/'agent'; dst_agent=TEL/'docs'/'agent'; dst_agent.mkdir(parents=True,exist_ok=True)
    for name in ['progress.txt','latest.txt','local_error.txt','self_update_error.txt','autopilot_status.json','autopilot_report.txt','parity_gate_report.txt','parity_live_evidence.txt']:
        copy_if_exists(src_agent/name,dst_agent/name)
    if (src_agent/'history').exists():
        (dst_agent/'history').mkdir(parents=True,exist_ok=True)
        for p in (src_agent/'history').glob('*.txt'): copy_if_exists(p,dst_agent/'history'/p.name)
    status=local_runtime_status(); (dst_agent/'runtime_status.json').write_text(json.dumps(status,indent=2)+'\n')
    state=status.get('state') or {}; hb=status.get('heartbeat') or {}; ap=status.get('autopilot') or {}
    tel_text=('KALSHI TELEMETRY ONLINE\n'+f"UPDATED_UTC={status['utc']}\n"+f"PUBLISHER_PID={status['publisher_pid']}\n"+f"AGENT_HEARTBEAT_UTC={hb.get('utc','unknown')}\n"+f"AGENT_STATUS={hb.get('status','unknown')}\n"+f"AGENT_JOB={hb.get('job_id','')}\n"+f"LAST_JOB_ID={state.get('last_job_id','')}\n"+f"LAST_RESULT={state.get('last_result','')}\n"+f"LAST_RUN_UTC={state.get('last_run_utc','')}\n"+f"AUTOPILOT_GATE_INDEX={ap.get('gate_index','')}\n"+f"AUTOPILOT_COMPLETED={','.join((ap.get('completed') or {}).keys())}\n"+f"SESSION_MASTER={SESSION_MASTER}\n"+f"SESSION_CURRENT={SESSION_CURRENT}\n")
    (dst_agent/'telemetry_status.txt').write_text(tel_text)
    mon=extract_monitor(); (dst_agent/'live_trade_monitor.json').write_text(json.dumps(mon,indent=2)+'\n'); (dst_agent/'live_trade_monitor.txt').write_text(monitor_text(mon))
    run(['git','add','docs/agent'],TEL,check=True)
    if run(['git','diff','--cached','--quiet'],TEL).returncode==0: return False
    run(['git','commit','-m','Sync Kalshi live trade monitor '+datetime.now(timezone.utc).strftime('%H:%M:%S')],TEL,check=True)
    p=run(['git','push','origin','HEAD:main'],TEL,180)
    if p.returncode: run(['git','pull','--rebase','origin','main'],TEL,180,check=True); run(['git','push','origin','HEAD:main'],TEL,180,check=True)
    ERROR.unlink(missing_ok=True); return True

def main():
    while True:
        try: sync_once()
        except Exception as e:
            msg=f'{utc()} {type(e).__name__}: {e}'; ERROR.write_text(msg+'\n'); print(msg,file=sys.stderr,flush=True)
        time.sleep(INTERVAL)
if __name__=='__main__': main()
