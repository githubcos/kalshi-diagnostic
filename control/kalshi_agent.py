#!/usr/bin/env python3
import json, os, re, shutil, subprocess, tarfile, time, tempfile, sys
from datetime import datetime, timezone
from pathlib import Path

HOME=Path('/home/ubuntu'); REPO=HOME/'kalshi-diagnostic'; BOT=HOME/'KalshiArbo'/'kalshiarbo'; STATE=HOME/'.kalshi_agent_state.json'; HEARTBEAT=HOME/'.kalshi_agent_heartbeat.json'; BACKUPS=HOME/'kalshi-agent-backups'; RESULTS=REPO/'docs'/'agent'; PROGRESS=RESULTS/'progress.txt'; PORT='8085'; POLL=5
ALLOWED={'STATUS','BACKUP','APPLY_PATCH','GOFMT','GO_TEST','GO_BUILD','START_PAPER','STOP_PAPER','COLLECT_AUDIT','ROLLBACK','UPDATE_AUTOPILOT','UPDATE_TELEMETRY','COLLECT_PARITY_EVIDENCE'}
def utc(): return datetime.now(timezone.utc).isoformat(timespec='seconds')
def heartbeat(status='idle',job_id=''):
    try:
        tmp=HEARTBEAT.with_suffix('.tmp'); tmp.write_text(json.dumps({'utc':utc(),'epoch':time.time(),'pid':os.getpid(),'status':status,'job_id':job_id},indent=2)); tmp.replace(HEARTBEAT)
    except Exception: pass
def ensure_dirs(): BACKUPS.mkdir(exist_ok=True); RESULTS.mkdir(parents=True,exist_ok=True); (RESULTS/'history').mkdir(exist_ok=True)
def write_progress(lines): ensure_dirs(); PROGRESS.write_text('\n'.join(lines)+'\n'); heartbeat('working',next((x.split('=',1)[1] for x in lines if x.startswith('JOB_ID=')),''))
def append_progress(lines,msg): lines.append(f'[{utc()}] {msg}'); write_progress(lines)
def run(cmd,cwd=None,timeout=600):
    p=subprocess.run(cmd,cwd=cwd,text=True,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,timeout=timeout); return p.returncode,p.stdout
def run_stream(cmd,progress,cwd=None,timeout=900,label='COMMAND'):
    append_progress(progress,f'{label} START: '+' '.join(cmd)); started=time.time(); p=subprocess.Popen(cmd,cwd=cwd,text=True,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,bufsize=1); output=[]
    try:
        while True:
            heartbeat('working',next((x.split('=',1)[1] for x in progress if x.startswith('JOB_ID=')),''))
            if time.time()-started>timeout: p.kill(); raise TimeoutError(f'{label} timed out after {timeout}s')
            line=p.stdout.readline() if p.stdout else ''
            if line:
                line=line.rstrip('\n'); output.append(line); progress.append('    '+line)
                if len(progress)>500: del progress[20:100]
                write_progress(progress)
            if p.poll() is not None:
                if p.stdout:
                    for rest in p.stdout:
                        rest=rest.rstrip('\n'); output.append(rest); progress.append('    '+rest)
                break
            if not line: time.sleep(.1)
        rc=p.returncode
    finally:
        try:
            if p.stdout: p.stdout.close()
        except: pass
    append_progress(progress,f'{label} END rc={rc} duration={time.time()-started:.2f}s'); return rc,'\n'.join(output)+'\n'
def fetch_origin(): return run(['git','fetch','-q','origin','main'],REPO,120)
def read_origin_text(rel):
    rc,out=run(['git','show',f'origin/main:{rel}'],REPO,60)
    if rc: raise RuntimeError(f'unable to read origin/main:{rel}: {out.strip()}')
    return out
def maybe_self_update():
    try:
        remote=read_origin_text('control/kalshi_agent.py'); current=Path(__file__).read_text()
        if remote==current:return False
        fd,tmp=tempfile.mkstemp(prefix='kalshi-agent-selfupdate-',suffix='.py'); os.close(fd); tp=Path(tmp); tp.write_text(remote); rc,out=run([sys.executable,'-m','py_compile',str(tp)],timeout=30)
        if rc: tp.unlink(missing_ok=True); (RESULTS/'self_update_error.txt').write_text(f'{utc()} remote agent syntax check failed\n{out}\n'); return False
        dst=Path(__file__).resolve(); os.chmod(tp,0o700); os.replace(tp,dst); heartbeat('self-updating'); os.execv(sys.executable,[sys.executable,str(dst)])
    except Exception as e: ensure_dirs(); (RESULTS/'self_update_error.txt').write_text(f'{utc()} {type(e).__name__}: {e}\n'); return False
def load_remote_job():
    try:return json.loads(read_origin_text('control/job.json'))
    except:return None
def materialize_remote_patch(rel):
    rel=str(rel or '').strip()
    if not rel.startswith('control/patches/') or not rel.endswith('.patch') or '..' in Path(rel).parts: raise ValueError('patch path must be under control/patches/*.patch')
    text=read_origin_text(rel); fd,tmp=tempfile.mkstemp(prefix='kalshi-agent-',suffix='.patch'); os.close(fd); Path(tmp).write_text(text); return Path(tmp)
def load_state():
    try:return json.loads(STATE.read_text())
    except:return {'last_job_id':None,'last_backup':None}
def save_state(s): STATE.write_text(json.dumps(s,indent=2))
def backup(state,log,progress):
    ensure_dirs(); stamp=datetime.now(timezone.utc).strftime('%Y%m%d_%H%M%S'); dest=BACKUPS/f'kalshiarbo_{stamp}.tar.gz'; append_progress(progress,f'BACKUP START -> {dest}'); count=0
    with tarfile.open(dest,'w:gz') as t:
        for p in BOT.rglob('*'):
            rel=p.relative_to(BOT)
            if any(part in {'.git','logs','node_modules'} for part in rel.parts): continue
            if p.name in {'config.json','.bot.lock','kalshiarbo'} or p.suffix in {'.env','.pem','.key'}: continue
            if p.is_file(): t.add(p,arcname=str(rel)); count+=1
    state['last_backup']=str(dest); save_state(state); log.append(f'BACKUP={dest}'); append_progress(progress,f'BACKUP PASS files={count}'); return dest
def restore(path,log,progress):
    p=Path(path)
    if not p.exists() or p.parent!=BACKUPS: raise ValueError('invalid backup')
    with tarfile.open(p,'r:gz') as t: t.extractall(BOT)
    log.append(f'ROLLBACK={p}'); append_progress(progress,'ROLLBACK PASS')
def stop_paper(log,progress):
    append_progress(progress,'STOP_PAPER START')
    pattern=r'(^|/|\s)kalshiarbo(\s|$).*[-]port[ =]8085|/KalshiArbo/kalshiarbo/kalshiarbo.*[-]port[ =]8085'
    run(['pkill','-INT','-f',pattern],timeout=20); time.sleep(2); run(['pkill','-TERM','-f',pattern],timeout=20); time.sleep(1)
    rc,out=run(['bash','-lc',"ss -ltnp 2>/dev/null | grep ':8085' || true"],timeout=20)
    if out.strip():
        log.append('STOP_PAPER port still occupied after targeted kill:\n'+out)
        raise RuntimeError('paper port 8085 still occupied after stop')
    try:(BOT/'.bot.lock').unlink()
    except FileNotFoundError: pass
    append_progress(progress,'STOP_PAPER PASS')
def start_paper(log,progress):
    stop_paper(log,progress); lf=(BOT/'polyarb.log').open('a'); p=subprocess.Popen([str(BOT/'kalshiarbo'),'-port',PORT],cwd=BOT,stdout=lf,stderr=subprocess.STDOUT,start_new_session=True); (BOT/'kalshiarbo.pid').write_text(str(p.pid)); time.sleep(3)
    rc,out=run(['bash','-lc',f"ss -ltnp 2>/dev/null | grep ':8085' || true"],timeout=20)
    if not out.strip() or str(p.pid) not in out:
        raise RuntimeError(f'new paper process pid={p.pid} did not acquire port {PORT}: {out.strip()}')
    log.append(f'START_PAPER pid={p.pid} port={PORT}\n'+out); append_progress(progress,'START_PAPER PASS')
def status(log,progress):
    for cmd in (["ss","-ltnp"],["pgrep","-af","kalshiarbo"]):
        rc,out=run(cmd,timeout=20); log.append('$ '+' '.join(cmd)+'\n'+out)
    rc,out=run(['tail','-n','120','polyarb.log'],BOT,20); log.append('LOG_TAIL\n'+out); append_progress(progress,'STATUS PASS')
def install_python_control(rel,dst,service,log,progress):
    remote=read_origin_text(rel); fd,tmp=tempfile.mkstemp(prefix='kalshi-control-',suffix='.py'); os.close(fd); tp=Path(tmp); tp.write_text(remote); rc,out=run([sys.executable,'-m','py_compile',str(tp)],timeout=30)
    if rc: tp.unlink(missing_ok=True); raise RuntimeError(f'{rel} syntax validation failed: '+out.strip())
    os.chmod(tp,0o700); os.replace(tp,HOME/dst); rc,out=run(['sudo','systemctl','restart',service],timeout=30)
    if rc: raise RuntimeError(f'unable to restart {service}: '+out.strip())
    log.append(f'UPDATED {rel} -> {dst}'); append_progress(progress,f'UPDATE {service} PASS')
def update_autopilot(log,progress):
    try:(HOME/'.kalshi_autopilot_state.json').unlink()
    except FileNotFoundError: pass
    install_python_control('control/kalshi_autopilot.py','kalshi_autopilot.py','kalshi-autopilot.service',log,progress)
def update_telemetry(log,progress): install_python_control('control/kalshi_telemetry_sync.py','kalshi_telemetry_sync.py','kalshi-agent-telemetry.service',log,progress)
def collect_parity_evidence(log,progress):
    p=BOT/'strategy'/'trader.go'; lines=p.read_text(errors='replace').splitlines(); needles=['grossFillShares','SizeMatched','getFillsTimed','ComputeBuyFeeShares','HedgeBy','shouldAbortLeadWaitForFilledPreHedge','cancel_timeout','residualPosition','markToMarket','settlement','BalancedAt','lockedProfit']; out=[f'LIVE PARITY SOURCE EVIDENCE UTC={utc()}','LIVE_ALLOWED=false']
    seen=[]
    for needle in needles:
        for i,line in enumerate(lines):
            if needle.lower() in line.lower():
                lo=max(0,i-8); hi=min(len(lines),i+9); key=(lo,hi)
                if key in seen: continue
                seen.append(key); out.append(f'\n--- {needle} @ strategy/trader.go:{i+1} context {lo+1}-{hi} ---')
                out.extend(f'{n+1:05d}: {lines[n]}' for n in range(lo,hi))
                if len(seen)>=40: break
        if len(seen)>=40: break
    (RESULTS/'parity_live_evidence.txt').write_text('\n'.join(out)+'\n'); log.append(f'PARITY_EVIDENCE excerpts={len(seen)}'); append_progress(progress,f'COLLECT_PARITY_EVIDENCE PASS excerpts={len(seen)}')
def publish(jobid,text):
    ensure_dirs(); (RESULTS/'latest.txt').write_text(text); (RESULTS/'history'/f'{jobid}.txt').write_text(text); run(['git','add','docs/agent'],REPO,30); run(['git','commit','-m',f'Agent result {jobid}'],REPO,60); run(['git','pull','--rebase','--autostash'],REPO,120); run(['git','push'],REPO,120)
def execute(job,state):
    jid=str(job.get('id','')).strip(); heartbeat('working',jid); progress=['KALSHI AUTONOMOUS AGENT — LIVE PROGRESS',f'JOB_ID={jid}',f'JOB_START_UTC={utc()}','LIVE_ALLOWED=false']; write_progress(progress); log=['KALSHI EC2 AGENT',f'JOB_ID={jid}',f'START_UTC={utc()}','LIVE_ALLOWED=false']; ok=True
    try:
        if not jid: raise ValueError('missing job id')
        if job.get('live') not in (None,False): raise ValueError('LIVE IS HARD-BLOCKED')
        actions=job.get('actions',[])
        for idx,a in enumerate(actions,1):
            if isinstance(a,str): name=a; args={}
            else:name=str(a.get('name','')); args=a
            if name not in ALLOWED: raise ValueError(f'action not allowed: {name}')
            append_progress(progress,f'ACTION {idx}/{len(actions)} {name} START'); log.append(f'\n=== {name} ===')
            if name=='STATUS': status(log,progress)
            elif name=='BACKUP': backup(state,log,progress)
            elif name=='UPDATE_AUTOPILOT': update_autopilot(log,progress)
            elif name=='UPDATE_TELEMETRY': update_telemetry(log,progress)
            elif name=='COLLECT_PARITY_EVIDENCE': collect_parity_evidence(log,progress)
            elif name=='APPLY_PATCH':
                patch=materialize_remote_patch(args.get('path',''))
                try:
                    backup(state,log,progress); strip=str(int(args.get('strip',1))); rc,out=run_stream(['patch','--dry-run','--batch','-p'+strip,'-i',str(patch)],progress,BOT,120,'PATCH_DRY_RUN'); log.append(out)
                    if rc: raise RuntimeError('patch dry-run failed')
                    rc,out=run_stream(['patch','--batch','-p'+strip,'-i',str(patch)],progress,BOT,120,'PATCH_APPLY'); log.append(out)
                    if rc: raise RuntimeError('patch apply failed')
                finally: patch.unlink(missing_ok=True)
            elif name=='GOFMT':
                files=args.get('files',[]) or ['strategy/trader.go']; valid=[]
                for f in files:
                    p=(BOT/f).resolve()
                    if BOT.resolve() not in p.parents or p.suffix!='.go': raise ValueError('unsafe gofmt path')
                    if p.exists(): valid.append(str(p))
                rc,out=run_stream(['gofmt','-w']+valid,progress,BOT,120,'GOFMT'); log.append(out)
                if rc: raise RuntimeError('gofmt failed')
            elif name=='GO_TEST':
                rc,out=run_stream(['go','test','./...'],progress,BOT,900,'GO_TEST'); log.append(out)
                if rc: raise RuntimeError('tests failed')
            elif name=='GO_BUILD':
                rc,out=run_stream(['go','build','-o','kalshiarbo','.'],progress,BOT,900,'GO_BUILD'); log.append(out)
                if rc: raise RuntimeError('build failed')
            elif name=='START_PAPER': start_paper(log,progress)
            elif name=='STOP_PAPER': stop_paper(log,progress)
            elif name=='COLLECT_AUDIT': status(log,progress)
            elif name=='ROLLBACK': restore(args.get('backup') or state.get('last_backup'),log,progress)
            append_progress(progress,f'ACTION {idx}/{len(actions)} {name} PASS')
        log.append('\nRESULT=PASS'); append_progress(progress,'JOB RESULT=PASS')
    except Exception as e:
        ok=False; log.append(f'\nRESULT=FAIL\nERROR={type(e).__name__}: {e}'); append_progress(progress,f'JOB RESULT=FAIL ERROR={type(e).__name__}: {e}')
    log.append(f'END_UTC={utc()}'); heartbeat('idle',''); return ok,'\n'.join(log)+'\n'
def main():
    ensure_dirs(); heartbeat('starting')
    while True:
        try:
            heartbeat('polling'); rc,_=fetch_origin()
            if rc==0:
                maybe_self_update(); state=load_state(); job=load_remote_job()
                if job:
                    jid=str(job.get('id','')).strip()
                    if job.get('enabled',False) and jid and jid!=state.get('last_job_id'):
                        ok,text=execute(job,state); state['last_job_id']=jid; state['last_result']='PASS' if ok else 'FAIL'; state['last_run_utc']=utc(); save_state(state); publish(jid,text)
            heartbeat('idle')
        except Exception as e: ensure_dirs(); heartbeat('error'); (RESULTS/'local_error.txt').write_text(f'AGENT LOOP ERROR {utc()} {type(e).__name__}: {e}\n')
        time.sleep(POLL)
if __name__=='__main__': main()
