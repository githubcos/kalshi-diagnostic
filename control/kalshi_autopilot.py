#!/usr/bin/env python3
import json, os, subprocess, time, shutil
from pathlib import Path
from datetime import datetime, timezone

HOME=Path('/home/ubuntu')
REPO=HOME/'kalshi-diagnostic'
BOT=HOME/'KalshiArbo'/'kalshiarbo'
STATE=HOME/'.kalshi_autopilot_state.json'
STATUS=REPO/'docs'/'agent'/'autopilot_status.json'
REPORT=REPO/'docs'/'agent'/'autopilot_report.txt'
POLL=5
CYCLE_DELAY=20

FORBIDDEN=[
 ('polymarket.ComputeBuyFeeShares','Polymarket fee-share math still present'),
 ('polymarket.MinMarketOrderNotionalUSD','Polymarket minimum-notional constant still present'),
 ('polymarket.MinOrderShares','Polymarket minimum-share constant still present'),
]

def utc(): return datetime.now(timezone.utc).isoformat(timespec='seconds')

def run(cmd,cwd=None,timeout=900):
    return subprocess.run(cmd,cwd=cwd,text=True,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,timeout=timeout)

def load_state():
    try:return json.loads(STATE.read_text())
    except:return {'cycle':0,'phase':'STARTING','last_result':'UNKNOWN'}

def save_state(s): STATE.write_text(json.dumps(s,indent=2)+'\n')

def write_status(s):
    STATUS.parent.mkdir(parents=True,exist_ok=True)
    tmp=STATUS.with_suffix('.tmp')
    tmp.write_text(json.dumps(s,indent=2)+'\n')
    tmp.replace(STATUS)

def status(cycle,phase,pct,result='RUNNING',detail='',next_step=''):
    s={'utc':utc(),'cycle':cycle,'phase':phase,'percent':pct,'result':result,'detail':detail,'next':next_step}
    write_status(s); return s

def source_audit():
    findings=[]
    targets=[BOT/'strategy'/'trader.go',BOT/'strategy'/'signal.go',BOT/'kalshi'/'execution_adapter.go',BOT/'kalshi'/'orders.go']
    for p in targets:
        if not p.exists(): continue
        text=p.read_text(errors='replace')
        for needle,msg in FORBIDDEN:
            count=text.count(needle)
            if count:
                findings.append({'file':str(p.relative_to(BOT)),'needle':needle,'count':count,'message':msg})
    return findings

def paper_alive():
    p=run(['bash','-lc',"ss -ltnp 2>/dev/null | grep -q ':8085'"])
    return p.returncode==0

def write_report(cycle,findings,test_out,build_out):
    lines=[f'KALSHI AUTOPILOT REPORT cycle={cycle}',f'UTC={utc()}',f'PAPER_ALIVE={paper_alive()}',f'FINDINGS={len(findings)}']
    for f in findings: lines.append(f"FINDING {f['file']} {f['needle']} count={f['count']} :: {f['message']}")
    lines += ['','=== GO TEST TAIL ==='] + test_out.splitlines()[-30:] + ['','=== GO BUILD TAIL ==='] + build_out.splitlines()[-20:]
    REPORT.write_text('\n'.join(lines)+'\n')

def main():
    st=load_state()
    while True:
        st['cycle']=int(st.get('cycle',0))+1; c=st['cycle']; save_state(st)
        try:
            status(c,'SOURCE_AUDIT',10,detail='Scanning live Kalshi execution source for remaining Polymarket venue assumptions.',next_step='GO_TEST')
            findings=source_audit()
            time.sleep(1)

            status(c,'GO_TEST',35,detail=f'Source scan complete: {len(findings)} flagged venue-pattern occurrences. Running full Go test suite.',next_step='GO_BUILD')
            t=run(['go','test','./...'],BOT,900)
            if t.returncode:
                write_report(c,findings,t.stdout,'')
                status(c,'GO_TEST',35,'FAIL','Regression tests failed; report published. Autopilot will retry after delay.','RETRY')
                time.sleep(CYCLE_DELAY); continue

            status(c,'GO_BUILD',60,detail='All tests passed. Building current KalshiArbo binary.',next_step='PAPER_HEALTH')
            b=run(['go','build','-o','kalshiarbo','.'],BOT,900)
            if b.returncode:
                write_report(c,findings,t.stdout,b.stdout)
                status(c,'GO_BUILD',60,'FAIL','Build failed; report published. Autopilot will retry after delay.','RETRY')
                time.sleep(CYCLE_DELAY); continue

            alive=paper_alive()
            status(c,'PAPER_HEALTH',80,detail='Build passed. Checking paper bot health on port 8085.',next_step='REPORT')
            if not alive:
                # keep LIVE blocked; only restart paper binary
                run(['bash','-lc',f"cd {BOT} && (nohup ./kalshiarbo -port 8085 >> polyarb.log 2>&1 &)"] ,timeout=30)
                time.sleep(3); alive=paper_alive()

            write_report(c,findings,t.stdout,b.stdout)
            detail=f'Cycle complete. tests=PASS build=PASS paper={"RUNNING" if alive else "DOWN"} findings={len(findings)}.'
            status(c,'CYCLE_COMPLETE',100,'PASS' if alive else 'WARN',detail,'NEXT_CYCLE')
            st['last_result']='PASS' if alive else 'WARN'; st['last_cycle_utc']=utc(); save_state(st)
            time.sleep(CYCLE_DELAY)
        except Exception as e:
            status(c,'AUTOPILOT_ERROR',0,'FAIL',f'{type(e).__name__}: {e}','RETRY')
            time.sleep(CYCLE_DELAY)

if __name__=='__main__': main()
