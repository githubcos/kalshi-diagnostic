#!/usr/bin/env python3
import json, subprocess, time, shutil, os
from pathlib import Path
from datetime import datetime, timezone
HOME=Path('/home/ubuntu'); REPO=HOME/'kalshi-diagnostic'; BOT=HOME/'KalshiArbo'/'kalshiarbo'; STATE=HOME/'.kalshi_autopilot_state.json'; STATUS=REPO/'docs'/'agent'/'autopilot_status.json'; REPORT=REPO/'docs'/'agent'/'parity_gate_report.txt'; POLL=20
STRESS_MARKER=HOME/'.kalshi_paper_stress_v1_applied'
GATES=[('PARTIAL_FILL','authoritative partial-fill quantity handling',['grossFillShares','SizeMatched','getFillsTimed','ComputeBuyFeeShares']),('HEDGE_TIMING','hedge admission and timing',['HedgeBy','pre-placed hedge','shouldAbortLeadWaitForFilledPreHedge','hedge']),('TIMEOUT_UNWIND','timeout and unwind semantics',['cancel_timeout','timeout','unwind','residualPosition']),('SETTLEMENT','settlement and final mark-to-market semantics',['markToMarket','settlement','BalancedAt','lockedProfit'])]
def utc(): return datetime.now(timezone.utc).isoformat(timespec='seconds')
def run(cmd,cwd=None,timeout=900): return subprocess.run(cmd,cwd=cwd,text=True,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,timeout=timeout)
def load_state():
    try:return json.loads(STATE.read_text())
    except:return {'gate_index':0,'completed':{}}
def save_state(s): STATE.write_text(json.dumps(s,indent=2)+'\n')
def write_status(gate,pct,result,detail,next_step=''):
    STATUS.parent.mkdir(parents=True,exist_ok=True); data={'utc':utc(),'cycle':int(time.time()),'phase':gate,'percent':pct,'result':result,'detail':detail,'next':next_step}; tmp=STATUS.with_suffix('.tmp'); tmp.write_text(json.dumps(data,indent=2)+'\n'); tmp.replace(STATUS)
def paper_alive(): return run(['bash','-lc',"ss -ltnp 2>/dev/null | grep -q ':8085'"]).returncode==0
def norm(s): return ''.join(ch.lower() for ch in str(s) if ch.isalnum())
def apply_paper_stress_once():
    if STRESS_MARKER.exists(): return
    cfgp=BOT/'config.json'
    if not cfgp.exists():
        write_status('PAPER_STRESS_CONFIG',0,'FAIL','config.json not found; no settings changed.','MANUAL_CHECK'); return
    data=json.loads(cfgp.read_text())
    bynorm={norm(k):k for k in data.keys()}
    wanted={
      'PaperTrade':True,
      'PairArbEnabled':True,
      'PairArbMinWindowSec':0,
      'PairArbMaxWindowSec':850,
      'PairArbMinBTCGapUSD':10,
      'PairArbMaxBTCGapUSD':0,
      'PairArbMinGapVelocityUSD':0,
      'PairArbMinTokenPrice':0.05,
      'PairArbMaxTokenPrice':0.95,
      'PairArbMinCVDBTC':0,
      'PairArbMinBookImbalance':0,
      'PairArbMinCoinbaseSpreadUSD':0,
      'PairArbMaxCoinbaseSpreadUSD':0,
      'PairArbMinCoinbaseTakerImbalance':0,
      'PairArbMLFilterEnabled':False,
    }
    required={'PaperTrade','PairArbEnabled','PairArbMinWindowSec','PairArbMaxWindowSec','PairArbMinBTCGapUSD','PairArbMaxBTCGapUSD','PairArbMinGapVelocityUSD','PairArbMinTokenPrice','PairArbMaxTokenPrice'}
    missing=[name for name in required if norm(name) not in bynorm]
    if missing:
        write_status('PAPER_STRESS_CONFIG',0,'FAIL','Required non-secret config keys not found: '+', '.join(missing)+'. No settings changed.','MANUAL_CHECK'); return
    backup=HOME/f'kalshiarbo_config_before_paper_stress_{datetime.now(timezone.utc).strftime("%Y%m%d_%H%M%S")}.json'
    shutil.copy2(cfgp,backup); os.chmod(backup,0o600)
    changed=[]
    for name,val in wanted.items():
        key= bynorm.get(norm(name))
        if key is not None:
            data[key]=val; changed.append(f'{key}={val}')
    tmp=cfgp.with_suffix('.json.tmp'); tmp.write_text(json.dumps(data,indent=2)+'\n'); os.chmod(tmp,0o600); tmp.replace(cfgp)
    STRESS_MARKER.write_text(utc()+'\n'); os.chmod(STRESS_MARKER,0o600)
    (REPO/'docs'/'agent'/'paper_stress_config.txt').write_text('UTC='+utc()+'\nLIVE_ALLOWED=false\nBACKUP='+str(backup)+'\n'+'\n'.join(changed)+'\n')
    write_status('PAPER_STRESS_CONFIG',100,'PASS','High-frequency PAPER settings applied from existing config keys; credentials untouched. Restart PAPER to load them.','START_PAPER')
def excerpts(path,needles,context=6):
    text=path.read_text(errors='replace').splitlines(); hits=[]; used=set()
    for needle in needles:
        for i,line in enumerate(text):
            if needle.lower() in line.lower():
                lo=max(0,i-context); hi=min(len(text),i+context+1); key=(lo,hi)
                if key in used: continue
                used.add(key); hits.append((needle,i+1,lo+1,hi,text[lo:hi]))
                if len(hits)>=20:return hits
    return hits
def publish_evidence(msg):
    run(['git','add','docs/agent/parity_gate_report.txt','docs/agent/autopilot_status.json','docs/agent/paper_stress_config.txt'],REPO,30)
    if run(['git','diff','--cached','--quiet'],REPO,30).returncode==0:return
    run(['git','commit','-m',msg],REPO,60); run(['git','pull','--rebase','--autostash','origin','main'],REPO,120); run(['git','push','origin','HEAD:main'],REPO,120)
def write_gate_report(gate,title,hits):
    lines=[f'KALSHI FINITE PARITY GATE: {gate}',f'UTC={utc()}',f'TITLE={title}',f'PAPER_ALIVE={paper_alive()}',f'LIVE_ALLOWED=false',f'HITS={len(hits)}','']
    for needle,line,lo,hi,chunk in hits:
        lines += [f'--- {needle} at strategy/trader.go:{line} (context {lo}-{hi}) ---']
        for n,s in enumerate(chunk,start=lo): lines.append(f'{n:05d}: {s}')
        lines.append('')
    REPORT.write_text('\n'.join(lines)+'\n')
def main():
    apply_paper_stress_once(); publish_evidence('Apply high-frequency PAPER stress settings')
    st=load_state(); st.setdefault('completed',{})
    while True:
        idx=int(st.get('gate_index',0))
        if idx>=len(GATES):
            write_status('FINAL_REGRESSION',85,'RUNNING','All finite source-context gates captured. Running one final test/build and paper-health verification.','PAPER_EVIDENCE')
            t=run(['go','test','./...'],BOT,900); b=run(['go','build','-o','kalshiarbo','.'],BOT,900) if t.returncode==0 else None; alive=paper_alive(); result='PASS' if t.returncode==0 and b is not None and b.returncode==0 and alive else 'FAIL'
            write_status('PAPER_EVIDENCE',95,result,f'Finite gates captured; tests={"PASS" if t.returncode==0 else "FAIL"} build={"PASS" if b is not None and b.returncode==0 else "FAIL"} paper={"RUNNING" if alive else "DOWN"}. High-frequency PAPER profile remains configured.','OBSERVE_PAPER_TRADES'); publish_evidence('Finite parity regression status'); time.sleep(300); continue
        gate,title,needles=GATES[idx]; nxt=GATES[idx+1][0] if idx+1<len(GATES) else 'FINAL_REGRESSION'
        write_status(gate,10+idx*18,'RUNNING',f'Finite gate {idx+1}/4: inspecting {title}. High-frequency PAPER profile configured.',nxt)
        p=BOT/'strategy'/'trader.go'; hits=excerpts(p,needles) if p.exists() else []; write_gate_report(gate,title,hits)
        st['completed'][gate]={'utc':utc(),'evidence_hits':len(hits),'state':'EVIDENCE_CAPTURED'}; st['gate_index']=idx+1; save_state(st)
        write_status(gate,25+idx*18,'EVIDENCE_CAPTURED',f'{gate} live execution context published ({len(hits)} excerpts).',nxt); publish_evidence(f'Capture Kalshi parity gate {gate}'); time.sleep(POLL)
if __name__=='__main__': main()
