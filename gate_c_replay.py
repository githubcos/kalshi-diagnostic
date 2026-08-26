#!/usr/bin/env python3
import argparse, json, math
from datetime import datetime


def ts(x):
    return datetime.fromisoformat(x.replace('Z','+00:00')).timestamp()

def fee(q,p,rate=0.07):
    if q<=0 or p<=0 or p>=1: return 0.0
    return math.ceil(rate*q*p*(1-p)*100-1e-9)/100.0

def sweep(levels, qty, liquidity=1.0, adverse_ticks=0):
    left=qty; raw=0.0; taken=0.0
    for row in levels or []:
        p=float(row[0]); q=float(row[1])*liquidity
        if adverse_ticks:
            p=min(0.99,p+0.01*adverse_ticks)
        if q<=0 or p<=0 or p>=1: continue
        take=min(left,q)
        raw += take*p; taken += take; left -= take
        if left<=1e-9: break
    if left>1e-9 or taken<=0: return None
    vwap=raw/taken
    return {'qty':taken,'raw':raw,'vwap':vwap,'fee':fee(taken,vwap),'cost':raw+fee(taken,vwap)}

def qty_for_budget(levels,budget,liquidity=1.0):
    # maximize quantity purchasable including fee, 0.01-contract granularity
    if budget<=0: return 0.0
    maxq=sum(float(r[1])*liquidity for r in levels or [])
    q=math.floor(maxq*100)/100
    # practical upper bound from best ask
    if levels:
        p=max(0.001,float(levels[0][0]))
        q=min(q,math.floor((budget/p)*100)/100)
    while q>0:
        z=sweep(levels,q,liquidity,0)
        if z and z['cost']<=budget+1e-9: return q
        q=round(q-0.01,2)
    return 0.0

def future_index(rows,i,delay):
    target=rows[i]['_t']+delay/1000.0
    j=i+1
    while j<len(rows) and rows[j]['_t']<target-1e-9: j+=1
    return j if j<len(rows) else None

def main():
    ap=argparse.ArgumentParser()
    ap.add_argument('jsonl')
    ap.add_argument('--lead-budget',type=float,default=5.0)
    ap.add_argument('--min-locked-per-contract',type=float,default=0.08)
    args=ap.parse_args()
    rows=[]
    with open(args.jsonl,'r',errors='replace') as f:
        for line in f:
            try:
                r=json.loads(line)
                if r.get('kind')!='SNAPSHOT': continue
                if not r.get('yes_asks') or not r.get('no_asks'): continue
                r['_t']=ts(r['ts']); rows.append(r)
            except Exception: pass
    rows.sort(key=lambda r:r['_t'])
    delays=[100,250,500]
    ticks=[0,1,2]
    liquids=[1.0,0.75,0.50]
    print('KALSHI GATE C — OFFLINE LEAD/HEDGE STRESS REPLAY')
    print(f'SNAPSHOTS={len(rows)} LEAD_BUDGET_USD={args.lead_budget:.2f} TARGET_LOCKED_PER_CONTRACT={args.min_locked_per_contract:.4f}')
    print('Method: at every snapshot, simulate BOTH lead orientations using real displayed depth; buy lead now, then equal-size hedge at the first recorded book at/after latency. Hedge-only slippage and liquidity haircuts are applied conservatively. Fees charged on both legs.')
    print()
    hdr='lat(ms) ticks liq% side attempts hedge_fill profitable target_met avg_net worst_net avg_unhedged_ms'
    print(hdr)
    all_results=[]
    for delay in delays:
      for tick in ticks:
       for liq in liquids:
        for side in ('YES','NO'):
            attempts=fills=prof=target=0; nets=[]; exposures=[]
            for i,r in enumerate(rows):
                j=future_index(rows,i,delay)
                if j is None: continue
                lead_levels=r['yes_asks'] if side=='YES' else r['no_asks']
                hedge_levels=rows[j]['no_asks'] if side=='YES' else rows[j]['yes_asks']
                q=qty_for_budget(lead_levels,args.lead_budget,1.0)
                if q<=0: continue
                lead=sweep(lead_levels,q,1.0,0)
                if not lead: continue
                attempts+=1
                hedge=sweep(hedge_levels,q,liq,tick)
                if not hedge: continue
                fills+=1
                net=q-lead['cost']-hedge['cost']
                nets.append(net)
                exposures.append(max(0.0,(rows[j]['_t']-r['_t'])*1000.0))
                if net>0: prof+=1
                if net>=args.min_locked_per_contract*q-1e-9: target+=1
            if attempts:
                avg=sum(nets)/len(nets) if nets else float('nan')
                worst=min(nets) if nets else float('nan')
                ex=sum(exposures)/len(exposures) if exposures else float('nan')
                print(f'{delay:7d} {tick:5d} {liq*100:4.0f} {side:4s} {attempts:8d} {fills:10d} {prof:10d} {target:10d} {avg:8.4f} {worst:9.4f} {ex:16.1f}')
                all_results.append((delay,tick,liq,side,attempts,fills,prof,target,avg,worst,ex))
    print('\nSUMMARY')
    if not all_results:
        print('NO_REPLAYABLE_ROWS')
        return
    base=[x for x in all_results if x[1]==0 and x[2]==1.0]
    stress=[x for x in all_results if x[1]==2 and x[2]==0.5]
    def agg(group,label):
        a=sum(x[4] for x in group); f=sum(x[5] for x in group); p=sum(x[6] for x in group); t=sum(x[7] for x in group)
        print(f'{label}: attempts={a} hedge_fill_rate={(100*f/a if a else 0):.2f}% positive_net_rate={(100*p/f if f else 0):.2f}% target_locked_rate={(100*t/f if f else 0):.2f}%')
    agg(base,'BASELINE (0 tick, 100% liquidity)')
    agg(stress,'HARSH (2 tick, 50% liquidity)')
    print('\nNOTE: This is a market-risk envelope, not a backtest of the bot signal. No strategy signal filter is applied; both possible lead sides are tested at every recorded snapshot.')

if __name__=='__main__': main()
