#!/usr/bin/env python3
import json, subprocess, time, shutil, os, re
from pathlib import Path
from datetime import datetime, timezone

HOME=Path('/home/ubuntu')
REPO=HOME/'kalshi-diagnostic'
BOT=HOME/'KalshiArbo'/'kalshiarbo'
STATUS=REPO/'docs'/'agent'/'autopilot_status.json'
REPORT=REPO/'docs'/'agent'/'paper_profit_stress.txt'
STATEP=REPO/'docs'/'agent'/'adaptive_optimizer_state.json'
RUNROOT=HOME/'kalshi-stress-runs'
MARKER=HOME/'.kalshi_adaptive_optimizer_v5'
POLL=5
TARGET_WIN_RATE=0.92
START_BALANCE=100.0
WIN_BALANCE=200.0
LOSS_BALANCE=0.0
ANSI=re.compile(r'\x1b\[[0-9;?]*[ -/]*[@-~]')
PNL_RE=re.compile(r'P&L\s+([+-])\$([0-9]+(?:\.[0-9]+)?).*?Reason:\s*([A-Za-z0-9_\-]+)',re.I)

PROFILES=[
    {'name':'STRICT','min_gap':8.0,'hedge_dist':1.0},
    {'name':'SAFE','min_gap':5.0,'hedge_dist':2.0},
    {'name':'FLOW','min_gap':1.0,'hedge_dist':2.0},
    {'name':'HIGH_FLOW','min_gap':1.0,'hedge_dist':3.0},
]
INITIAL_PROFILE=2


def utc(): return datetime.now(timezone.utc).isoformat(timespec='seconds')
def run(cmd,cwd=None,timeout=900): return subprocess.run(cmd,cwd=cwd,text=True,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,timeout=timeout)
def norm(s): return ''.join(ch.lower() for ch in str(s) if ch.isalnum())
def paper_alive(): return run(['bash','-lc',"ss -ltnp 2>/dev/null | grep -q ':8085'"]).returncode==0

def write_status(phase,pct,result,detail,next_step=''):
    STATUS.parent.mkdir(parents=True,exist_ok=True)
    data={'utc':utc(),'cycle':int(time.time()),'phase':phase,'percent':pct,'result':result,'detail':detail,'next':next_step}
    tmp=STATUS.with_suffix('.tmp'); tmp.write_text(json.dumps(data,indent=2)+'\n'); tmp.replace(STATUS)

def get_run_dir():
    RUNROOT.mkdir(parents=True,exist_ok=True)
    if MARKER.exists():
        try:
            p=Path(MARKER.read_text().strip())
            if p.exists(): return p
        except Exception: pass
    rid='adaptive_paper_v5_'+datetime.now(timezone.utc).strftime('%Y%m%d_%H%M%S')
    p=RUNROOT/rid; p.mkdir(parents=True,exist_ok=True); MARKER.write_text(str(p)+'\n'); os.chmod(MARKER,0o600)
    return p

def copy_logs(dst):
    dst.mkdir(parents=True,exist_ok=True)
    for pat in ('*.log','*.jsonl','*.csv','*.txt'):
        for src in BOT.glob(pat):
            try: shutil.copy2(src,dst/src.name)
            except Exception: pass
    arc=BOT/'archive_signals'
    if arc.exists():
        out=dst/'archive_signals'; out.mkdir(parents=True,exist_ok=True)
        for src in arc.glob('*'):
            if src.is_file():
                try: shutil.copy2(src,out/src.name)
                except Exception: pass

def load_state():
    if STATEP.exists():
        try: return json.loads(STATEP.read_text())
        except Exception: pass
    p=BOT/'polyarb.log'
    size=p.stat().st_size if p.exists() else 0
    return {
        'version':5,'started_utc':utc(),'profile_index':INITIAL_PROFILE,'profile_started_ts':time.time(),
        'log_offset':size,'log_inode':p.stat().st_ino if p.exists() else 0,
        'epoch':{'leads':0,'hedges':0,'closes':0,'wins':0,'losses':0,'pnl':0.0,'timeouts':0},
        'total':{'leads':0,'hedges':0,'closes':0,'wins':0,'losses':0,'pnl':0.0,'timeouts':0},
        'profile_history':[],'last_change_utc':None
    }

def save_state(st):
    STATEP.parent.mkdir(parents=True,exist_ok=True)
    tmp=STATEP.with_suffix('.tmp'); tmp.write_text(json.dumps(st,indent=2)+'\n'); tmp.replace(STATEP)

def config_update(profile_idx, reason, rundir):
    cfgp=BOT/'config.json'
    if not cfgp.exists(): raise RuntimeError('config.json not found')
    data=json.loads(cfgp.read_text()); bynorm={norm(k):k for k in data.keys()}
    p=PROFILES[profile_idx]
    wanted={
        'PaperTrade':True,'PaperStartBalance':100,'PairArbEnabled':True,
        'PairArbMinWindowSec':0,'PairArbMaxWindowSec':850,
        'PairArbMinBTCGapUSD':p['min_gap'],'PairArbSignalMinGapUSD':1,
        'PairArbMaxBTCGapUSD':0,'PairArbMinGapHoldSec':0,'PairArbMinGapVelocityUSD':0,
        'PairArbMinTokenPrice':0.10,'PairArbMaxTokenPrice':0.90,'PairArbMaxSignalAgeSec':3,
        'PairArbMinLockedProfitCents':1,'PairArbMaxHedgeDistanceCents':p['hedge_dist'],
        'PairArbHedgeTimeoutSec':20,'PairArbMaxAdverseYesDriftCents':2,
        'PairArbStopLossCents':2,'PairArbStopLossMinHoldSec':1,'PairArbStopLossMinGapAgainstUSD':0,
        'PairArbUnprofitableAbortGraceSec':6,'PairArbUnprofitableAbortMinGapAgainstUSD':2,
        'PairArbStopCooldownSec':0,'MaxTradesPerSession':10000,
        'MaxSessionLossUSD':100,'MaxSessionProfitUSD':100,
        'PairArbMinCVDBTC':0,'PairArbMinBookImbalance':0,
        'PairArbMinCoinbaseSpreadUSD':0,'PairArbMaxCoinbaseSpreadUSD':0,
        'PairArbMinCoinbaseTakerImbalance':0,'PairArbMLFilterEnabled':False,
        'PairArbTradeSizeUSD':5
    }
    missing=[name for name in wanted if norm(name) not in bynorm]
    # PairArbSignalMinGapUSD may be absent in older config; it is not required to proceed.
    missing_required=[x for x in missing if x!='PairArbSignalMinGapUSD']
    if missing_required: raise RuntimeError('missing config keys: '+', '.join(missing_required))
    stamp=datetime.now(timezone.utc).strftime('%Y%m%d_%H%M%S')
    shutil.copy2(cfgp,rundir/f'config_before_profile_{profile_idx}_{stamp}.json')
    changed=[]
    for name,val in wanted.items():
        k=bynorm.get(norm(name))
        if k is not None:
            data[k]=val; changed.append(f'{k}={val}')
    tmp=cfgp.with_suffix('.json.tmp'); tmp.write_text(json.dumps(data,indent=2)+'\n'); os.chmod(tmp,0o600); tmp.replace(cfgp)
    (rundir/f'profile_{profile_idx}_{stamp}.txt').write_text('UTC='+utc()+'\nPROFILE='+p['name']+'\nREASON='+reason+'\n'+'\n'.join(changed)+'\n')
    return changed

def restart_paper():
    run(['bash','-lc',"pkill -INT -f '/home/ubuntu/KalshiArbo/kalshiarbo/kalshiarbo -port 8085' 2>/dev/null || true; sleep 2; pkill -TERM -f '/home/ubuntu/KalshiArbo/kalshiarbo/kalshiarbo -port 8085' 2>/dev/null || true; sleep 1"],timeout=20)
    run(['bash','-lc',"rm -f .bot.lock kalshiarbo.pid; nohup ./kalshiarbo -port 8085 >> polyarb.log 2>&1 & echo $! > kalshiarbo.pid"],BOT,20)
    time.sleep(4)
    return paper_alive()

def process_new_log(st):
    p=BOT/'polyarb.log'
    if not p.exists(): return
    inode=p.stat().st_ino; size=p.stat().st_size
    if st.get('log_inode')!=inode or st.get('log_offset',0)>size:
        st['log_inode']=inode; st['log_offset']=0
    with p.open('rb') as f:
        f.seek(st.get('log_offset',0)); raw=f.read(); st['log_offset']=f.tell()
    if not raw: return
    text=ANSI.sub('',raw.decode('utf-8','replace'))
    lead=text.count('[PAIR LEAD] EXECUTED')
    hedge=text.count('[PAIR HEDGE] EXECUTED')
    for bucket in ('epoch','total'):
        st[bucket]['leads']+=lead; st[bucket]['hedges']+=hedge
    for line in text.splitlines():
        m=PNL_RE.search(line)
        if not m: continue
        pnl=float(m.group(2))*(1 if m.group(1)=='+' else -1); reason=m.group(3)
        for bucket in ('epoch','total'):
            st[bucket]['closes']+=1; st[bucket]['pnl']+=pnl
            if pnl>0: st[bucket]['wins']+=1
            else: st[bucket]['losses']+=1
            if reason=='pair_fill_leg_timeout': st[bucket]['timeouts']+=1

def metrics(e, elapsed):
    closes=e['closes']; leads=e['leads']
    wr=e['wins']/closes if closes else None
    hedge_rate=e['hedges']/leads if leads else None
    timeout_rate=e['timeouts']/closes if closes else 0.0
    tpm=closes/max(elapsed/60.0,1/60) if closes else 0.0
    ev=e['pnl']/closes if closes else None
    return wr,hedge_rate,timeout_rate,tpm,ev

def choose_action(st):
    elapsed=time.time()-st['profile_started_ts']; e=st['epoch']; idx=st['profile_index']
    wr,hr,tr,tpm,ev=metrics(e,elapsed)
    # Catastrophic execution regression: immediately tighten one level after enough evidence.
    if e['closes']>=5 and (tr>0.20 or (hr is not None and hr<0.80) or (ev is not None and ev<0)):
        if idx>0: return idx-1, f'TIGHTEN bad execution: closes={e["closes"]} wr={wr} hedge_rate={hr} timeout_rate={tr:.3f} ev={ev}'
        return idx, None
    # User diagnostic target: after a minimally useful sample, <92% win rate is a warning; tighten unless EV/hedging are excellent.
    if e['closes']>=25 and wr is not None and wr<TARGET_WIN_RATE:
        if idx>0: return idx-1, f'TIGHTEN win_rate {wr:.3f} below target {TARGET_WIN_RATE:.2f}'
    # Throughput expansion: no trade for 2 minutes, or <0.5 closes/min after 5 minutes, loosen one step.
    if elapsed>=120 and e['closes']==0 and idx<len(PROFILES)-1:
        return idx+1, f'LOOSEN zero closes in {elapsed:.0f}s'
    if elapsed>=300 and tpm<0.5 and idx<len(PROFILES)-1:
        return idx+1, f'LOOSEN throughput {tpm:.2f}/min below 0.50/min'
    # If the profile is proving strong, allow more flow.
    if e['closes']>=25 and wr is not None and wr>=TARGET_WIN_RATE and ev is not None and ev>0 and tr<=0.05 and idx<len(PROFILES)-1:
        return idx+1, f'LOOSEN strong sample wr={wr:.3f} ev={ev:.4f} timeout={tr:.3f}'
    return idx, None

def latest_balance():
    p=BOT/'polyarb.log'
    if not p.exists(): return None
    try: data=ANSI.sub('',p.read_bytes()[-1500000:].decode('utf-8','replace'))
    except Exception: return None
    vals=[]
    for pat in (r'Balance:\s*\$(-?\d+(?:\.\d+)?)',r'paper balance:\s*\$(-?\d+(?:\.\d+)?)'):
        vals += [float(x) for x in re.findall(pat,data,re.I)]
    return vals[-1] if vals else None

def publish(st,rundir):
    e=st['epoch']; elapsed=time.time()-st['profile_started_ts']; wr,hr,tr,tpm,ev=metrics(e,elapsed); p=PROFILES[st['profile_index']]
    bal=latest_balance()
    lines=[
        'KALSHIARBO ADAPTIVE PAPER OPTIMIZER V5','====================================',
        f'UPDATED_UTC={utc()}','LIVE_ALLOWED=false',f'RUN_DIR={rundir}',f'PROFILE={p["name"]}',
        f'MIN_GAP_USD={p["min_gap"]}',f'MAX_HEDGE_DISTANCE_CENTS={p["hedge_dist"]}',
        f'CURRENT_BALANCE={"unknown" if bal is None else f"{bal:.2f}"}',
        f'EPOCH_LEADS={e["leads"]}',f'EPOCH_HEDGES={e["hedges"]}',f'EPOCH_CLOSES={e["closes"]}',
        f'EPOCH_WINS={e["wins"]}',f'EPOCH_LOSSES={e["losses"]}',f'EPOCH_PNL={e["pnl"]:.4f}',
        f'EPOCH_WIN_RATE={"NA" if wr is None else f"{wr:.4f}"}',
        f'EPOCH_HEDGE_RATE={"NA" if hr is None else f"{hr:.4f}"}',f'EPOCH_TIMEOUT_RATE={tr:.4f}',
        f'EPOCH_TRADES_PER_MIN={tpm:.4f}',f'EPOCH_EV_PER_CLOSE={"NA" if ev is None else f"{ev:.6f}"}',
        f'TARGET_WIN_RATE={TARGET_WIN_RATE:.2f}',
        'OPTIMIZATION_PRIORITY=positive expectancy and hedge reliability first; 92% win rate is a diagnostic target, not permission to hide tail losses',
        'BOUNDS=max hedge distance never above 3c; locked profit 1c; 20s hedge timeout; 2c stop loss; $5 paper size; live orders forbidden'
    ]
    REPORT.write_text('\n'.join(lines)+'\n'); save_state(st)
    write_status('ADAPTIVE_PAPER_OPTIMIZER',50,'RUNNING',f'{p["name"]}: closes={e["closes"]} wr={"NA" if wr is None else f"{wr:.1%}"} pnl=${e["pnl"]:.3f} flow={tpm:.2f}/min hedge={"NA" if hr is None else f"{hr:.1%}"} timeout={tr:.1%}','AUTO_TUNE_WITHIN_BOUNDS')
    run(['git','add','docs/agent/autopilot_status.json','docs/agent/paper_profit_stress.txt','docs/agent/adaptive_optimizer_state.json'],REPO,30)
    if run(['git','diff','--cached','--quiet'],REPO,30).returncode!=0:
        run(['git','commit','-m','Update adaptive PAPER optimizer telemetry'],REPO,60)
        run(['git','pull','--rebase','--autostash','origin','main'],REPO,120)
        run(['git','push','origin','HEAD:main'],REPO,120)

def main():
    rundir=get_run_dir(); st=load_state()
    # First activation: apply the high-throughput but hedge-bounded profile immediately.
    if not st.get('initialized'):
        copy_logs(rundir/'before_adaptive_v5')
        config_update(st['profile_index'],'INITIAL adaptive high-throughput profile',rundir)
        if not restart_paper():
            write_status('ADAPTIVE_PAPER_OPTIMIZER',0,'FAIL','Paper restart failed after initial profile','CHECK_PAPER')
            return
        p=BOT/'polyarb.log'; st['log_inode']=p.stat().st_ino if p.exists() else 0; st['log_offset']=p.stat().st_size if p.exists() else 0
        st['profile_started_ts']=time.time(); st['initialized']=True; st['last_change_utc']=utc(); save_state(st)
    while True:
        process_new_log(st)
        new_idx,reason=choose_action(st)
        if reason and new_idx!=st['profile_index']:
            old=st['profile_index']; snap={'utc':utc(),'from':PROFILES[old]['name'],'to':PROFILES[new_idx]['name'],'reason':reason,'epoch':dict(st['epoch'])}
            st['profile_history'].append(snap); copy_logs(rundir/f'epoch_{len(st["profile_history"]):04d}_before_change')
            config_update(new_idx,reason,rundir)
            st['profile_index']=new_idx; st['epoch']={'leads':0,'hedges':0,'closes':0,'wins':0,'losses':0,'pnl':0.0,'timeouts':0}
            st['profile_started_ts']=time.time(); st['last_change_utc']=utc()
            restart_paper(); p=BOT/'polyarb.log'; st['log_inode']=p.stat().st_ino if p.exists() else 0; st['log_offset']=p.stat().st_size if p.exists() else 0
        publish(st,rundir)
        copy_logs(rundir/'live')
        time.sleep(POLL)

if __name__=='__main__': main()
