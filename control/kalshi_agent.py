#!/usr/bin/env python3
import json, os, re, shutil, subprocess, tarfile, time
from datetime import datetime, timezone
from pathlib import Path

HOME=Path('/home/ubuntu')
REPO=HOME/'kalshi-diagnostic'
BOT=HOME/'KalshiArbo'/'kalshiarbo'
JOB=REPO/'control'/'job.json'
STATE=HOME/'.kalshi_agent_state.json'
BACKUPS=HOME/'kalshi-agent-backups'
RESULTS=REPO/'docs'/'agent'
PORT='8085'
POLL=15
ALLOWED={'STATUS','BACKUP','APPLY_PATCH','GOFMT','GO_TEST','GO_BUILD','START_PAPER','STOP_PAPER','COLLECT_AUDIT','ROLLBACK'}

def utc(): return datetime.now(timezone.utc).isoformat(timespec='seconds')
def run(cmd,cwd=None,timeout=600):
    p=subprocess.run(cmd,cwd=cwd,text=True,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,timeout=timeout)
    return p.returncode,p.stdout

def git_sync():
    run(['git','pull','--rebase'],REPO,120)

def load_state():
    try:return json.loads(STATE.read_text())
    except:return {'last_job_id':None,'last_backup':None}

def save_state(s): STATE.write_text(json.dumps(s,indent=2))

def safe_patch_path(rel):
    p=(REPO/rel).resolve(); root=(REPO/'control'/'patches').resolve()
    if root not in p.parents or p.suffix!='.patch': raise ValueError('patch path must be under control/patches/*.patch')
    return p

def backup(state,log):
    BACKUPS.mkdir(exist_ok=True)
    stamp=datetime.now(timezone.utc).strftime('%Y%m%d_%H%M%S')
    dest=BACKUPS/f'kalshiarbo_{stamp}.tar.gz'
    with tarfile.open(dest,'w:gz') as t:
        for p in BOT.rglob('*'):
            rel=p.relative_to(BOT)
            if any(part in {'.git','logs','node_modules'} for part in rel.parts): continue
            if p.name in {'config.json','.bot.lock','kalshiarbo'} or p.suffix in {'.env','.pem','.key'}: continue
            if p.is_file(): t.add(p,arcname=str(rel))
    state['last_backup']=str(dest); save_state(state); log.append(f'BACKUP={dest}')
    return dest

def restore(path,log):
    p=Path(path)
    if not p.exists() or p.parent!=BACKUPS: raise ValueError('invalid backup')
    with tarfile.open(p,'r:gz') as t:
        # Validate archive traversal
        for m in t.getmembers():
            target=(BOT/m.name).resolve()
            if BOT.resolve() not in target.parents and target!=BOT.resolve(): raise ValueError('unsafe archive path')
        t.extractall(BOT)
    log.append(f'ROLLBACK={p}')

def stop_paper(log):
    rc,out=run(['pkill','-INT','-f',str(BOT/'kalshiarbo')],timeout=20); time.sleep(2)
    run(['pkill','-TERM','-f',str(BOT/'kalshiarbo')],timeout=20)
    try:(BOT/'.bot.lock').unlink()
    except FileNotFoundError: pass
    log.append('STOP_PAPER requested')

def start_paper(log):
    # Hard invariant: never invoke -live.
    stop_paper(log)
    lf=(BOT/'polyarb.log').open('a')
    p=subprocess.Popen([str(BOT/'kalshiarbo'),'-port',PORT],cwd=BOT,stdout=lf,stderr=subprocess.STDOUT,start_new_session=True)
    (BOT/'kalshiarbo.pid').write_text(str(p.pid))
    time.sleep(3)
    log.append(f'START_PAPER pid={p.pid} port={PORT}')

def status(log):
    for cmd in (["ss","-ltnp"],["pgrep","-af",str(BOT/'kalshiarbo')]):
        rc,out=run(cmd,timeout=20); log.append('$ '+' '.join(cmd)+'\n'+out)
    rc,out=run(['tail','-n','100','polyarb.log'],BOT,20); log.append('LOG_TAIL\n'+out)

def publish(jobid,text):
    RESULTS.mkdir(parents=True,exist_ok=True); (RESULTS/'history').mkdir(exist_ok=True)
    (RESULTS/'latest.txt').write_text(text)
    (RESULTS/'history'/f'{jobid}.txt').write_text(text)
    run(['git','pull','--rebase'],REPO,120)
    run(['git','add','docs/agent'],REPO,30)
    run(['git','commit','-m',f'Agent result {jobid}'],REPO,60)
    run(['git','push'],REPO,120)

def execute(job,state):
    jid=str(job.get('id','')).strip()
    log=[f'KALSHI EC2 AGENT',f'JOB_ID={jid}',f'START_UTC={utc()}',f'LIVE_ALLOWED=false']
    ok=True
    try:
        if not jid: raise ValueError('missing job id')
        if job.get('live') not in (None,False): raise ValueError('LIVE IS HARD-BLOCKED')
        actions=job.get('actions',[])
        if not isinstance(actions,list): raise ValueError('actions must be list')
        for a in actions:
            if isinstance(a,str): name=a; args={}
            else: name=str(a.get('name','')); args=a
            if name not in ALLOWED: raise ValueError(f'action not allowed: {name}')
            log.append(f'\n=== {name} ===')
            if name=='STATUS': status(log)
            elif name=='BACKUP': backup(state,log)
            elif name=='APPLY_PATCH':
                patch=safe_patch_path(args.get('path',''))
                backup(state,log)
                strip=str(int(args.get('strip',1)))
                rc,out=run(['patch','--dry-run','--batch','-p'+strip,'-i',str(patch)],BOT,120); log.append(out)
                if rc: raise RuntimeError('patch dry-run failed')
                rc,out=run(['patch','--batch','-p'+strip,'-i',str(patch)],BOT,120); log.append(out)
                if rc: raise RuntimeError('patch apply failed')
            elif name=='GOFMT':
                files=args.get('files',[])
                if not files: files=['strategy/trader.go','strategy/signal.go','kalshi/feed.go','main.go']
                valid=[]
                for f in files:
                    p=(BOT/f).resolve()
                    if BOT.resolve() not in p.parents or p.suffix!='.go': raise ValueError('unsafe gofmt path')
                    if p.exists(): valid.append(str(p))
                rc,out=run(['gofmt','-w']+valid,BOT,120); log.append(out)
                if rc: raise RuntimeError('gofmt failed')
            elif name=='GO_TEST':
                rc,out=run(['go','test','./...'],BOT,900); log.append(out)
                if rc: raise RuntimeError('tests failed')
            elif name=='GO_BUILD':
                rc,out=run(['go','build','-o','kalshiarbo','.'],BOT,900); log.append(out)
                if rc: raise RuntimeError('build failed')
            elif name=='START_PAPER': start_paper(log)
            elif name=='STOP_PAPER': stop_paper(log)
            elif name=='COLLECT_AUDIT': status(log)
            elif name=='ROLLBACK': restore(args.get('backup') or state.get('last_backup'),log)
        log.append('\nRESULT=PASS')
    except Exception as e:
        ok=False; log.append(f'\nRESULT=FAIL\nERROR={type(e).__name__}: {e}')
        # Never auto-restore unless a patch/build pipeline explicitly failed after a backup.
    log.append(f'END_UTC={utc()}')
    return ok,'\n'.join(log)+'\n'

def main():
    BACKUPS.mkdir(exist_ok=True); RESULTS.mkdir(parents=True,exist_ok=True)
    while True:
        try:
            git_sync(); state=load_state()
            if JOB.exists():
                job=json.loads(JOB.read_text())
                jid=str(job.get('id','')).strip()
                if job.get('enabled',False) and jid and jid!=state.get('last_job_id'):
                    ok,text=execute(job,state)
                    state['last_job_id']=jid; state['last_result']='PASS' if ok else 'FAIL'; state['last_run_utc']=utc(); save_state(state)
                    publish(jid,text)
        except Exception as e:
            err=f'AGENT LOOP ERROR {utc()} {type(e).__name__}: {e}\n'
            try:(RESULTS/'local_error.txt').write_text(err)
            except:pass
        time.sleep(POLL)

if __name__=='__main__': main()
