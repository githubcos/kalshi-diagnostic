#!/usr/bin/env python3
import json, os, shutil, subprocess, time, sys
from pathlib import Path
from datetime import datetime, timezone

HOME=Path('/home/ubuntu')
SRC=HOME/'kalshi-diagnostic'
TEL=HOME/'kalshi-agent-telemetry'
ERROR=HOME/'.kalshi_telemetry_error.txt'
INTERVAL=5

def utc(): return datetime.now(timezone.utc).isoformat(timespec='seconds')

def run(cmd,cwd=None,timeout=120,check=False):
    p=subprocess.run(cmd,cwd=cwd,text=True,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,timeout=timeout)
    if check and p.returncode:
        raise RuntimeError(f"command failed rc={p.returncode}: {' '.join(map(str,cmd))}\n{p.stdout}")
    return p

def log(msg):
    print(f'[{utc()}] {msg}',flush=True)

def ensure_clone():
    if (TEL/'.git').exists():
        return
    if TEL.exists(): shutil.rmtree(TEL)
    r=run(['git','-C',str(SRC),'remote','get-url','origin'],check=True)
    url=r.stdout.strip()
    if not url: raise RuntimeError('origin URL is empty')
    run(['git','clone','--depth','1',url,str(TEL)],timeout=180,check=True)
    run(['git','config','user.name','Kalshi Agent Telemetry'],TEL,check=True)
    run(['git','config','user.email','kalshi-agent@localhost'],TEL,check=True)
    log('clean telemetry clone created')

def copy_if_exists(src,dst):
    if src.exists():
        dst.parent.mkdir(parents=True,exist_ok=True)
        shutil.copy2(src,dst)

def local_runtime_status():
    status={'utc':utc(),'publisher_pid':os.getpid(),'heartbeat':None,'state':None}
    for key,path in [('heartbeat',HOME/'.kalshi_agent_heartbeat.json'),('state',HOME/'.kalshi_agent_state.json')]:
        try: status[key]=json.loads(path.read_text())
        except Exception: pass
    return status

def sync_once():
    ensure_clone()
    # Dedicated clone has no human edits; always start from latest origin/main.
    run(['git','fetch','-q','origin','main'],TEL,check=True)
    run(['git','reset','--hard','origin/main'],TEL,check=True)

    src_agent=SRC/'docs'/'agent'
    dst_agent=TEL/'docs'/'agent'
    dst_agent.mkdir(parents=True,exist_ok=True)
    for name in ['progress.txt','latest.txt','local_error.txt','self_update_error.txt']:
        copy_if_exists(src_agent/name,dst_agent/name)
    if (src_agent/'history').exists():
        (dst_agent/'history').mkdir(parents=True,exist_ok=True)
        for p in (src_agent/'history').glob('*.txt'):
            copy_if_exists(p,dst_agent/'history'/p.name)

    status=local_runtime_status()
    (dst_agent/'runtime_status.json').write_text(json.dumps(status,indent=2)+'\n')
    state=status.get('state') or {}
    hb=status.get('heartbeat') or {}
    tel_text=(
        'KALSHI TELEMETRY ONLINE\n'
        f"UPDATED_UTC={status['utc']}\n"
        f"PUBLISHER_PID={status['publisher_pid']}\n"
        f"AGENT_HEARTBEAT_UTC={hb.get('utc','unknown')}\n"
        f"AGENT_STATUS={hb.get('status','unknown')}\n"
        f"AGENT_JOB={hb.get('job_id','')}\n"
        f"LAST_JOB_ID={state.get('last_job_id','')}\n"
        f"LAST_RESULT={state.get('last_result','')}\n"
        f"LAST_RUN_UTC={state.get('last_run_utc','')}\n"
    )
    (dst_agent/'telemetry_status.txt').write_text(tel_text)

    run(['git','add','docs/agent'],TEL,check=True)
    diff=run(['git','diff','--cached','--quiet'],TEL)
    if diff.returncode==0:
        return False
    run(['git','commit','-m','Sync Kalshi telemetry '+datetime.now(timezone.utc).strftime('%H:%M:%S')],TEL,check=True)
    p=run(['git','push','origin','HEAD:main'],TEL,180)
    if p.returncode:
        log('push raced with remote; rebasing once')
        run(['git','pull','--rebase','origin','main'],TEL,180,check=True)
        run(['git','push','origin','HEAD:main'],TEL,180,check=True)
    ERROR.unlink(missing_ok=True)
    log(f"published agent={hb.get('status','unknown')} job={state.get('last_job_id','')} result={state.get('last_result','')}")
    return True

def main():
    log('telemetry publisher started')
    while True:
        try:
            sync_once()
        except Exception as e:
            msg=f'{utc()} {type(e).__name__}: {e}'
            ERROR.write_text(msg+'\n')
            print(msg,file=sys.stderr,flush=True)
        time.sleep(INTERVAL)

if __name__=='__main__': main()
