#!/usr/bin/env python3
import json, os, re, shutil, subprocess, time, sys
from pathlib import Path
from datetime import datetime, timezone

HOME=Path('/home/ubuntu')
SRC=HOME/'kalshi-diagnostic'
TEL=HOME/'kalshi-agent-telemetry'
BOT=HOME/'KalshiArbo'/'kalshiarbo'
ERROR=HOME/'.kalshi_telemetry_error.txt'
INTERVAL=5
ANSI=re.compile(r'\x1b\[[0-9;?]*[ -/]*[@-~]')

def utc(): return datetime.now(timezone.utc).isoformat(timespec='seconds')
def run(cmd,cwd=None,timeout=60):
    return subprocess.run(cmd,cwd=cwd,text=True,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,timeout=timeout)
def clean(s): return ANSI.sub('',s or '')

def ensure_clone():
    if (TEL/'.git').exists(): return
    if TEL.exists(): shutil.rmtree(TEL)
    url=run(['git','-C',str(SRC),'remote','get-url','origin']).stdout.strip()
    if not url: raise RuntimeError('origin URL unavailable')
    p=run(['git','clone','--depth','1',url,str(TEL)],timeout=180)
    if p.returncode: raise RuntimeError(p.stdout)
    run(['git','config','user.name','Kalshi Agent Telemetry'],TEL)
    run(['git','config','user.email','kalshi-agent@localhost'],TEL)

def safe_config():
    p=BOT/'config.json'
    try: raw=json.loads(p.read_text())
    except Exception: return {}
    keep={}
    for k,v in raw.items():
        lk=k.lower()
        if any(x in lk for x in ('secret','private','password','credential','webhook','wallet','mnemonic','passphrase','api_key','apikey','key_id','rpc')): continue
        if any(x in lk for x in ('pairarb','pair_arb','paper','trade_size','maxtrades','max_trades','maxsession','max_session','market_type','cooldown','risk')) and isinstance(v,(str,int,float,bool,type(None))): keep[k]=v
    return keep

def process_and_port():
    proc=run(['pgrep','-af','kalshiarbo'],timeout=20).stdout.strip()
    port=run(['bash','-lc',"ss -ltnp 2>/dev/null | grep ':8085' || true"],timeout=20).stdout.strip()
    pid=None
    m=re.search(r'pid=(\d+)',port)
    if m: pid=m.group(1)
    if not pid:
        for line in proc.splitlines():
            if '/KalshiArbo/kalshiarbo/kalshiarbo' in line and '-port 8085' in line:
                pid=line.split()[0]; break
    return pid,proc,port

def http_probe():
    results=[]
    for path in ['/api/status','/api/bot/status','/api/state','/status']:
        p=run(['curl','-sS','--max-time','2','-w','\nHTTP_CODE=%{http_code}\n','http://127.0.0.1:8085'+path],timeout=5)
        text=clean(p.stdout).strip()
        m=re.search(r'HTTP_CODE=(\d+)',text); code=m.group(1) if m else ''
        body=re.sub(r'\n?HTTP_CODE=\d+\s*$','',text).strip()[:2000]
        results.append((path,code,body))
        if code=='200' and body: break
    return results

def engine_state(probes):
    s='\n'.join(x[2] for x in probes).lower()
    if not s: return 'UNKNOWN'
    if any(x in s for x in ('"running":false','"is_running":false','"status":"stopped"','"state":"stopped"')): return 'STOPPED'
    if any(x in s for x in ('"running":true','"is_running":true','"status":"running"','"state":"running"')): return 'RUNNING'
    return 'UNKNOWN'

def recent_log():
    p=BOT/'polyarb.log'
    if not p.exists(): return 'NO polyarb.log'
    try:
        with p.open('rb') as f:
            f.seek(0,2); n=f.tell(); f.seek(max(0,n-300000)); text=clean(f.read().decode('utf-8','replace'))
    except Exception as e: return 'LOG READ ERROR: '+repr(e)
    needles=('active flags','paper','pair','hedge','locked','balance','p&l','risk controls','kalshi feed','websocket','error','warn','timeout','abort','started','stopped')
    out=[line.strip() for line in text.splitlines() if any(n in line.lower() for n in needles)]
    return '\n'.join(out[-80:])

def sync_once():
    ensure_clone(); run(['git','fetch','-q','origin','main'],TEL,120); run(['git','reset','--hard','origin/main'],TEL,120)
    now=utc(); pid,proc,port=process_and_port(); probes=http_probe(); engine=engine_state(probes); cfg=safe_config(); log=recent_log()
    lines=['KALSHIARBO — CURRENT EC2 STATUS','===============================',f'UPDATED_UTC={now}','MODE=PAPER','LIVE_ORDERS_ALLOWED=false',f'PROCESS={"ALIVE" if pid else "DOWN"}',f'PID={pid or "none"}',f'PORT_8085={"LISTENING" if port else "DOWN"}',f'TRADING_ENGINE={engine}','','=== PROCESS ===',proc or 'NO KALSHIARBO PROCESS','','=== PORT 8085 ===',port or 'PORT 8085 NOT LISTENING','','=== HTTP / DASHBOARD STATE ===']
    for path,code,body in probes: lines += [f'{path} HTTP={code or "NO_RESPONSE"}',body or '(empty)']
    lines += ['', '=== SAFE PAPER SETTINGS ===']+[f'{k}={cfg[k]}' for k in sorted(cfg)]
    lines += ['', '=== RECENT BOT EVIDENCE ===',log]
    latest='\n'.join(lines)+'\n'
    docs=TEL/'docs'; agent=docs/'agent'; docs.mkdir(parents=True,exist_ok=True); agent.mkdir(parents=True,exist_ok=True)
    (docs/'latest.txt').write_text(latest)
    data={'utc':now,'mode':'PAPER','live_orders_allowed':False,'kalshiarbo_pid':pid,'process_present':bool(pid),'port_8085_listening':bool(port),'trading_engine_state':engine,'http_probes':[{'path':p,'code':c,'body':b[:500]} for p,c,b in probes],'safe_config':cfg}
    (agent/'runtime_status.json').write_text(json.dumps(data,indent=2)+'\n')
    (agent/'telemetry_status.txt').write_text(f'KALSHI TELEMETRY ONLINE\nUPDATED_UTC={now}\nPUBLIC_LATEST=docs/latest.txt\nTRADING_ENGINE={engine}\nPORT_8085={bool(port)}\n')
    run(['git','add','docs/latest.txt','docs/agent/runtime_status.json','docs/agent/telemetry_status.txt'],TEL,30)
    if run(['git','diff','--cached','--quiet'],TEL,30).returncode==0: return
    p=run(['git','commit','-m','Sync current EC2 status '+datetime.now(timezone.utc).strftime('%H:%M:%S')],TEL,60)
    if p.returncode: raise RuntimeError(p.stdout)
    p=run(['git','push','origin','HEAD:main'],TEL,180)
    if p.returncode:
        run(['git','pull','--rebase','origin','main'],TEL,180)
        p=run(['git','push','origin','HEAD:main'],TEL,180)
        if p.returncode: raise RuntimeError(p.stdout)
    ERROR.unlink(missing_ok=True)

def main():
    while True:
        try: sync_once()
        except Exception as e:
            msg=f'{utc()} {type(e).__name__}: {e}'; ERROR.write_text(msg+'\n'); print(msg,file=sys.stderr,flush=True)
        time.sleep(INTERVAL)
if __name__=='__main__': main()
