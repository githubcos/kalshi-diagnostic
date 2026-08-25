#!/usr/bin/env python3
import json, os, shutil, subprocess, time
from pathlib import Path
from datetime import datetime, timezone

HOME=Path('/home/ubuntu')
SRC=HOME/'kalshi-diagnostic'
TEL=HOME/'kalshi-agent-telemetry'
INTERVAL=5

def utc(): return datetime.now(timezone.utc).isoformat(timespec='seconds')

def run(cmd,cwd=None,timeout=120):
    return subprocess.run(cmd,cwd=cwd,text=True,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,timeout=timeout)

def ensure_clone():
    if (TEL/'.git').exists():
        return
    if TEL.exists(): shutil.rmtree(TEL)
    r=run(['git','-C',str(SRC),'remote','get-url','origin'])
    if r.returncode: raise RuntimeError(r.stdout.strip())
    url=r.stdout.strip()
    r=run(['git','clone','--depth','1',url,str(TEL)],timeout=180)
    if r.returncode: raise RuntimeError('clone failed: '+r.stdout.strip())
    run(['git','config','user.name','Kalshi Agent Telemetry'],TEL)
    run(['git','config','user.email','kalshi-agent@localhost'],TEL)

def copy_if_exists(src,dst):
    if src.exists():
        dst.parent.mkdir(parents=True,exist_ok=True)
        shutil.copy2(src,dst)

def sync_once():
    ensure_clone()
    # Bring telemetry clone to latest remote state. Dedicated clone has no human edits.
    run(['git','fetch','-q','origin','main'],TEL)
    run(['git','reset','--hard','origin/main'],TEL)

    src_agent=SRC/'docs'/'agent'
    dst_agent=TEL/'docs'/'agent'
    dst_agent.mkdir(parents=True,exist_ok=True)
    for name in ['progress.txt','latest.txt','local_error.txt','self_update_error.txt']:
        copy_if_exists(src_agent/name,dst_agent/name)
    if (src_agent/'history').exists():
        (dst_agent/'history').mkdir(parents=True,exist_ok=True)
        for p in (src_agent/'history').glob('*.txt'):
            copy_if_exists(p,dst_agent/'history'/p.name)

    status={
        'utc':utc(),
        'heartbeat':None,
        'state':None,
    }
    for key,path in [('heartbeat',HOME/'.kalshi_agent_heartbeat.json'),('state',HOME/'.kalshi_agent_state.json')]:
        try: status[key]=json.loads(path.read_text())
        except Exception: pass
    (dst_agent/'runtime_status.json').write_text(json.dumps(status,indent=2)+'\n')

    run(['git','add','docs/agent'],TEL)
    diff=run(['git','diff','--cached','--quiet'],TEL)
    if diff.returncode==0:
        return
    run(['git','commit','-m','Sync Kalshi agent telemetry '+datetime.now(timezone.utc).strftime('%H:%M:%S')],TEL)
    # Remote may move between reset and push. Rebase once and retry.
    p=run(['git','push','origin','HEAD:main'],TEL,180)
    if p.returncode:
        run(['git','pull','--rebase','origin','main'],TEL,180)
        p=run(['git','push','origin','HEAD:main'],TEL,180)
        if p.returncode: raise RuntimeError('push failed: '+p.stdout.strip())

def main():
    while True:
        try:
            sync_once()
        except Exception as e:
            (HOME/'.kalshi_telemetry_error.txt').write_text(f'{utc()} {type(e).__name__}: {e}\n')
        time.sleep(INTERVAL)

if __name__=='__main__': main()
