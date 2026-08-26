#!/usr/bin/env python3
import json, subprocess, time, shutil, os, re
from pathlib import Path
from datetime import datetime, timezone
HOME=Path('/home/ubuntu'); REPO=HOME/'kalshi-diagnostic'; BOT=HOME/'KalshiArbo'/'kalshiarbo'; STATUS=REPO/'docs'/'agent'/'autopilot_status.json'; REPORT=REPO/'docs'/'agent'/'paper_profit_stress.txt'; MARKER=HOME/'.kalshi_paper_profit_stress_v1_applied'; RUNROOT=HOME/'kalshi-stress-runs'; POLL=10
START_BALANCE=100.0; WIN_BALANCE=200.0; LOSS_BALANCE=0.0
ANSI=re.compile(r'\x1b\[[0-9;?]*[ -/]*[@-~]')
def utc(): return datetime.now(timezone.utc).isoformat(timespec='seconds')
def run(cmd,cwd=None,timeout=900): return subprocess.run(cmd,cwd=cwd,text=True,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,timeout=timeout)
def norm(s): return ''.join(ch.lower() for ch in str(s) if ch.isalnum())
def paper_alive(): return run(['bash','-lc',"ss -ltnp 2>/dev/null | grep -q ':8085'"]).returncode==0
def write_status(phase,pct,result,detail,next_step=''):
    STATUS.parent.mkdir(parents=True,exist_ok=True); data={'utc':utc(),'cycle':int(time.time()),'phase':phase,'percent':pct,'result':result,'detail':detail,'next':next_step}; tmp=STATUS.with_suffix('.tmp'); tmp.write_text(json.dumps(data,indent=2)+'\n'); tmp.replace(STATUS)
def get_run_dir():
    RUNROOT.mkdir(parents=True,exist_ok=True)
    if MARKER.exists():
        try:
            p=Path(MARKER.read_text().strip())
            if p.exists(): return p
        except Exception: pass
    rid='paper_profit_v1_'+datetime.now(timezone.utc).strftime('%Y%m%d_%H%M%S'); p=RUNROOT/rid; p.mkdir(parents=True,exist_ok=True); MARKER.write_text(str(p)+'\n'); os.chmod(MARKER,0o600); return p
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
def apply_profile_once():
    rundir=get_run_dir(); sentinel=rundir/'profile_applied_high_volume_v2'
    if sentinel.exists(): return rundir
    cfgp=BOT/'config.json'
    if not cfgp.exists(): write_status('PAPER_STRESS_CONFIG',0,'FAIL','config.json not found; no settings changed.','MANUAL_CHECK'); return rundir
    copy_logs(rundir/'before_high_volume'); data=json.loads(cfgp.read_text()); bynorm={norm(k):k for k in data.keys()}
    wanted={'PaperTrade':True,'PaperStartBalance':100,'PairArbEnabled':True,'PairArbMinWindowSec':0,'PairArbMaxWindowSec':850,'PairArbMinBTCGapUSD':5,'PairArbMaxBTCGapUSD':0,'PairArbMinGapHoldSec':0,'PairArbMinGapVelocityUSD':0,'PairArbMinTokenPrice':0.05,'PairArbMaxTokenPrice':0.95,'PairArbMaxSignalAgeSec':10,'PairArbMinLockedProfitCents':3,'PairArbHedgeTimeoutSec':8,'PairArbMaxAdverseYesDriftCents':3,'PairArbStopCooldownSec':0,'MaxTradesPerSession':10000,'MaxSessionLossUSD':100,'MaxSessionProfitUSD':100,'PairArbMinCVDBTC':0,'PairArbMinBookImbalance':0,'PairArbMinCoinbaseSpreadUSD':0,'PairArbMaxCoinbaseSpreadUSD':0,'PairArbMinCoinbaseTakerImbalance':0,'PairArbMLFilterEnabled':False,'PairArbTradeSizeUSD':0.5}
    required=set(wanted.keys()); missing=[name for name in required if norm(name) not in bynorm]
    if missing: write_status('PAPER_STRESS_CONFIG',0,'FAIL','Required config keys not found: '+', '.join(missing)+'. No settings changed.','MANUAL_CHECK'); return rundir
    backup=rundir/'config_before_high_volume.json'; shutil.copy2(cfgp,backup); os.chmod(backup,0o600); changed=[]
    for name,val in wanted.items():
        key=bynorm[norm(name)]; data[key]=val; changed.append(f'{key}={val}')
    tmp=cfgp.with_suffix('.json.tmp'); tmp.write_text(json.dumps(data,indent=2)+'\n'); os.chmod(tmp,0o600); tmp.replace(cfgp)
    (rundir/'settings_high_volume.txt').write_text('UTC='+utc()+'\nMODE=PAPER_HIGH_VOLUME_DATA_COLLECTION\nSTART_BALANCE=100\nWIN_BALANCE=200\nLOSS_BALANCE=0\n'+'\n'.join(changed)+'\n'); sentinel.write_text(utc()+'\n')
    (REPO/'docs'/'agent'/'paper_stress_config.txt').write_text('UTC='+utc()+'\nLIVE_ALLOWED=false\nRUN_DIR='+str(rundir)+'\nPROFILE=HIGH_VOLUME_DATA_COLLECTION\n'+'\n'.join(changed)+'\n'); write_status('PAPER_STRESS_CONFIG',100,'PASS','High-volume PAPER data profile applied: broad gates, zero cooldown, $0.50 trades, 8s hedge timeout. Start=$100, terminal=$200/$0, logs preserved.','START_PAPER'); return rundir
def latest_balance():
    p=BOT/'polyarb.log'
    if not p.exists(): return None
    try: data=ANSI.sub('',p.read_bytes()[-2000000:].decode('utf-8','replace'))
    except Exception: return None
    vals=[]
    for pat in (r'Balance:\s*\$(-?\d+(?:\.\d+)?)',r'paper balance:\s*\$(-?\d+(?:\.\d+)?)'): vals += [float(x) for x in re.findall(pat,data,re.I)]
    return vals[-1] if vals else None
def write_report(rundir,balance,state):
    copy_logs(rundir/'live'); lines=['KALSHIARBO PAPER HIGH-VOLUME DATA STRESS TEST','==============================================',f'UPDATED_UTC={utc()}','LIVE_ALLOWED=false',f'RUN_DIR={rundir}',f'START_BALANCE={START_BALANCE:.2f}',f'WIN_BALANCE={WIN_BALANCE:.2f}',f'LOSS_BALANCE={LOSS_BALANCE:.2f}',f'CURRENT_BALANCE={"unknown" if balance is None else f"{balance:.2f}"}',f'STATE={state}',f'PAPER_8085={"UP" if paper_alive() else "DOWN"}','','PROFILE=min_gap $5; max_gap off; hold 0s; velocity off; token 0.05-0.95; signal_age 10s; locked_profit 3c; hedge_timeout 8s; cooldown 0s; trade_size $0.50','PURPOSE=generate thousands of PAPER execution observations; this profile is for data collection, not a profitability recommendation','LOGS=EC2 run directory keeps pre-run and continuously refreshed live *.log/*.jsonl/*.csv/*.txt plus archive_signals']; REPORT.write_text('\n'.join(lines)+'\n')
def publish():
    run(['git','add','docs/agent/autopilot_status.json','docs/agent/paper_stress_config.txt','docs/agent/paper_profit_stress.txt'],REPO,30)
    if run(['git','diff','--cached','--quiet'],REPO,30).returncode==0:return
    run(['git','commit','-m','Update PAPER high-volume stress telemetry'],REPO,60); run(['git','pull','--rebase','--autostash','origin','main'],REPO,120); run(['git','push','origin','HEAD:main'],REPO,120)
def main():
    rundir=apply_profile_once(); publish()
    while True:
        bal=latest_balance(); state='RUNNING'
        if bal is not None and bal>=WIN_BALANCE: state='COMPLETE_WIN'
        elif bal is not None and bal<=LOSS_BALANCE+1e-9: state='COMPLETE_LOSS'
        write_report(rundir,bal,state)
        if state=='RUNNING': write_status('PAPER_HIGH_VOLUME_STRESS',50,'RUNNING',f'High-volume PAPER data collection running; current={"unknown" if bal is None else f"${bal:.2f}"}. Logs preserved.','CONTINUE_UNTIL_200_OR_0')
        else: write_status('PAPER_HIGH_VOLUME_STRESS',100,'PASS',f'PAPER stress terminal reached: {state}; final balance=${bal:.2f}. Full logs preserved at {rundir}.','ANALYZE_FINAL_LOGS')
        publish(); time.sleep(60 if state!='RUNNING' else POLL)
if __name__=='__main__': main()
