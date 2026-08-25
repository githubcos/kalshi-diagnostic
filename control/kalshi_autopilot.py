#!/usr/bin/env python3
import json, subprocess, time
from pathlib import Path
from datetime import datetime, timezone

HOME=Path('/home/ubuntu')
REPO=HOME/'kalshi-diagnostic'
BOT=HOME/'KalshiArbo'/'kalshiarbo'
STATE=HOME/'.kalshi_autopilot_state.json'
STATUS=REPO/'docs'/'agent'/'autopilot_status.json'
REPORT=REPO/'docs'/'agent'/'parity_gate_report.txt'
POLL=20

GATES=[
 ('PARTIAL_FILL','authoritative partial-fill quantity handling',['grossFillShares','SizeMatched','getFillsTimed','ComputeBuyFeeShares']),
 ('HEDGE_TIMING','hedge admission and timing',['HedgeBy','hedge','pre-placed hedge','shouldAbortLeadWaitForFilledPreHedge']),
 ('TIMEOUT_UNWIND','timeout and unwind semantics',['timeout','cancel_timeout','unwind','residualPosition']),
 ('SETTLEMENT','settlement and final mark-to-market semantics',['settlement','markToMarket','BalancedAt','lockedProfit']),
]

def utc(): return datetime.now(timezone.utc).isoformat(timespec='seconds')
def run(cmd,cwd=None,timeout=900): return subprocess.run(cmd,cwd=cwd,text=True,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,timeout=timeout)
def load_state():
    try:return json.loads(STATE.read_text())
    except:return {'gate_index':0,'completed':{}}
def save_state(s): STATE.write_text(json.dumps(s,indent=2)+'\n')
def write_status(gate,pct,result,detail,next_step=''):
    STATUS.parent.mkdir(parents=True,exist_ok=True)
    data={'utc':utc(),'cycle':int(time.time()),'phase':gate,'percent':pct,'result':result,'detail':detail,'next':next_step}
    tmp=STATUS.with_suffix('.tmp'); tmp.write_text(json.dumps(data,indent=2)+'\n'); tmp.replace(STATUS)
def paper_alive(): return run(['bash','-lc',"ss -ltnp 2>/dev/null | grep -q ':8085'"]).returncode==0

def excerpts(path,needles,context=6):
    text=path.read_text(errors='replace').splitlines(); hits=[]; used=set()
    for needle in needles:
        for i,line in enumerate(text):
            if needle.lower() in line.lower():
                lo=max(0,i-context); hi=min(len(text),i+context+1); key=(lo,hi)
                if key in used: continue
                used.add(key); hits.append((needle,i+1,lo+1,hi,text[lo:hi]))
                if len(hits)>=18: return hits
    return hits

def write_gate_report(gate,title,hits):
    lines=[f'KALSHI FINITE PARITY GATE: {gate}',f'UTC={utc()}',f'TITLE={title}',f'PAPER_ALIVE={paper_alive()}',f'LIVE_ALLOWED=false',f'HITS={len(hits)}','']
    for needle,line,lo,hi,chunk in hits:
        lines += [f'--- {needle} at strategy/trader.go:{line} (context {lo}-{hi}) ---']
        for n,s in enumerate(chunk,start=lo): lines.append(f'{n:05d}: {s}')
        lines.append('')
    REPORT.write_text('\n'.join(lines)+'\n')

def main():
    st=load_state(); st.setdefault('completed',{})
    while True:
        idx=int(st.get('gate_index',0))
        if idx>=len(GATES):
            write_status('FINAL_REGRESSION',85,'RUNNING','All finite source-context gates captured. Running one final test/build and paper-health verification.','PAPER_EVIDENCE')
            t=run(['go','test','./...'],BOT,900); b=run(['go','build','-o','kalshiarbo','.'],BOT,900) if t.returncode==0 else None
            alive=paper_alive()
            result='PASS' if t.returncode==0 and b is not None and b.returncode==0 and alive else 'FAIL'
            write_status('PAPER_EVIDENCE',95,result,f'Finite gates captured; tests={"PASS" if t.returncode==0 else "FAIL"} build={"PASS" if b is not None and b.returncode==0 else "FAIL"} paper={"RUNNING" if alive else "DOWN"}. Awaiting fresh forensic economics snapshot.','FINAL_PARITY_REVIEW')
            time.sleep(300); continue
        gate,title,needles=GATES[idx]
        write_status(gate,10+idx*18,'RUNNING',f'Finite gate {idx+1}/4: inspecting {title}. Raw symbol-count cycling is disabled.',GATES[idx+1][0] if idx+1<len(GATES) else 'FINAL_REGRESSION')
        p=BOT/'strategy'/'trader.go'; hits=excerpts(p,needles) if p.exists() else []
        write_gate_report(gate,title,hits)
        # Gate capture is complete only as evidence collection. Semantic verdict is made from the published live context;
        # no trading code is changed here without a verified venue mismatch.
        st['completed'][gate]={'utc':utc(),'evidence_hits':len(hits),'state':'EVIDENCE_CAPTURED'}
        st['gate_index']=idx+1; save_state(st)
        write_status(gate,25+idx*18,'EVIDENCE_CAPTURED',f'{gate} live execution context published ({len(hits)} excerpts). Advancing to next finite gate; no blind patching.',GATES[idx+1][0] if idx+1<len(GATES) else 'FINAL_REGRESSION')
        time.sleep(POLL)

if __name__=='__main__': main()
