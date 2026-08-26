#!/usr/bin/env python3
import argparse, json, math, os, re, time, urllib.parse, urllib.request
from collections import deque
from datetime import datetime, timezone

BASE='https://api.elections.kalshi.com/trade-api/v2'
# Fallback host if the public API alias above is unavailable.
BASE_FALLBACK='https://external-api.kalshi.com/trade-api/v2'
UA='kalshiarbo-gate-b-recorder/1.0'
ANSI=re.compile(r'\x1b\[[0-9;?]*[ -/]*[@-~]')
LEAD_RE=re.compile(r'pair arb: Kalshi paper fill.*request[=: ]+"?pair_arb_lead_buy',re.I)
HEDGE_RE=re.compile(r'pair arb: Kalshi paper fill.*request[=: ]+"?pair_arb_hedge',re.I)
PAIR_RE=re.compile(r'PAIR|pair arb',re.I)


def utc(): return datetime.now(timezone.utc).isoformat(timespec='milliseconds')
def get_json(path, timeout=3):
    last=None
    for base in (BASE,BASE_FALLBACK):
        try:
            req=urllib.request.Request(base+path,headers={'User-Agent':UA,'Cache-Control':'no-cache'})
            with urllib.request.urlopen(req,timeout=timeout) as r: return json.load(r)
        except Exception as e: last=e
    raise last

def levels(rows):
    out=[]
    for row in rows or []:
        try:
            p=float(row[0]); q=float(row[1])
            if 0<p<1 and q>0: out.append((p,q))
        except Exception: pass
    return out

def asks_from_bids(yb,nb):
    return sorted([(1-p,q) for p,q in nb]), sorted([(1-p,q) for p,q in yb])

def fee(q,p):
    if q<=0 or p<=0 or p>=1:return 0.0
    return math.ceil((0.07*q*p*(1-p))*100-1e-9)/100

def sweep(levels_,q):
    left=q; raw=0.0; worst=0.0
    for p,n in levels_:
        if left<=1e-9: break
        take=min(left,n); raw+=take*p; worst=p; left-=take
    if left>1e-9:return None
    vwap=raw/q; return {'q':q,'vwap':vwap,'raw':raw,'fee':fee(q,vwap),'total':raw+fee(q,vwap),'worst':worst}

def best_complete_set(ya,na,max_spend):
    maxq=int(min(sum(q for _,q in ya),sum(q for _,q in na),250))
    best=None
    for qi in range(1,maxq+1):
        y=sweep(ya,float(qi)); n=sweep(na,float(qi))
        if not y or not n: break
        total=y['total']+n['total']; net=qi-total
        if total<=max_spend+1e-9 and net>0:
            r={'q':qi,'yes':y,'no':n,'total':total,'net':net,'roi':net/total}
            if best is None or r['net']>best['net']:best=r
    return best

def current_market(series):
    q=urllib.parse.urlencode({'status':'open','series_ticker':series,'limit':100})
    d=get_json('/markets?'+q); ms=d.get('markets',[])
    if not ms:return None
    return sorted(ms,key=lambda m:m.get('close_time') or m.get('expiration_time') or '9999')[0]

def book(ticker):
    d=get_json('/markets/'+urllib.parse.quote(ticker,safe='')+'/orderbook')
    ob=d.get('orderbook_fp') or d.get('orderbook') or {}
    yb=levels(ob.get('yes_dollars') or ob.get('yes') or [])
    nb=levels(ob.get('no_dollars') or ob.get('no') or [])
    ya,na=asks_from_bids(yb,nb)
    return yb,nb,ya,na

def tail_new(path,pos):
    if not os.path.exists(path): return pos,[]
    with open(path,'r',errors='replace') as f:
        f.seek(pos); chunk=f.read(); pos=f.tell()
    return pos,[ANSI.sub('',x) for x in chunk.splitlines() if x.strip()]

def main():
    ap=argparse.ArgumentParser()
    ap.add_argument('--bot-log',default=os.path.expanduser('~/KalshiArbo/kalshiarbo/polyarb.log'))
    ap.add_argument('--out',default=os.path.expanduser('~/gate_b_market.jsonl'))
    ap.add_argument('--status',default=os.path.expanduser('~/gate_b_status.txt'))
    ap.add_argument('--series',default='KXBTC15M')
    ap.add_argument('--interval-ms',type=int,default=250)
    ap.add_argument('--duration-sec',type=int,default=21600)
    ap.add_argument('--pair-budget',type=float,default=10.0)
    a=ap.parse_args()
    interval=max(0.1,a.interval_ms/1000)
    start=time.time(); pos=os.path.getsize(a.bot_log) if os.path.exists(a.bot_log) else 0
    snaps=0; errors=0; lead_events=0; hedge_events=0; last_ticker=''; last_best=None
    recent=deque(maxlen=20)
    with open(a.out,'a',buffering=1) as out:
        out.write(json.dumps({'kind':'GATE_B_START','ts':utc(),'interval_ms':a.interval_ms,'duration_sec':a.duration_sec,'pair_budget':a.pair_budget,'series':a.series,'live_orders_allowed':False})+'\n')
        while time.time()-start<a.duration_sec:
            t0=time.time(); now=utc(); rec={'kind':'SNAPSHOT','ts':now}
            try:
                m=current_market(a.series) if not last_ticker or snaps%20==0 else {'ticker':last_ticker}
                ticker=(m or {}).get('ticker','')
                if not ticker: raise RuntimeError('no open market')
                last_ticker=ticker
                yb,nb,ya,na=book(ticker)
                best=best_complete_set(ya,na,a.pair_budget); last_best=best
                rec.update({'ticker':ticker,'yes_bids':yb[:25],'no_bids':nb[:25],'yes_asks':ya[:25],'no_asks':na[:25],
                            'yes_best_ask':ya[0][0] if ya else None,'no_best_ask':na[0][0] if na else None,
                            'complete_set':best})
                errors=0; snaps+=1
            except Exception as e:
                errors+=1; rec.update({'error':type(e).__name__+': '+str(e)})
            pos,lines=tail_new(a.bot_log,pos)
            events=[]
            for line in lines:
                if not PAIR_RE.search(line): continue
                kind='BOT_PAIR'
                if LEAD_RE.search(line):kind='BOT_LEAD_FILL'; lead_events+=1
                elif HEDGE_RE.search(line):kind='BOT_HEDGE_FILL'; hedge_events+=1
                ev={'kind':kind,'ts':utc(),'ticker':last_ticker,'line':line[-1400:]}
                events.append(ev); recent.append(ev)
            rec['bot_events']=events
            out.write(json.dumps(rec,separators=(',',':'))+'\n')
            runtime=time.time()-start
            status=[
                'KALSHI PROFESSIONAL PAPER TEST — GATE B RECORDER',
                f'UTC={utc()}',f'RUNTIME_SEC={runtime:.1f}',f'LIVE_ORDERS_ALLOWED=false',
                f'MARKET={last_ticker or "unknown"}',f'SNAPSHOTS={snaps}',f'API_ERRORS_STREAK={errors}',
                f'BOT_LEAD_FILLS={lead_events}',f'BOT_HEDGE_FILLS={hedge_events}',
                f'POLL_INTERVAL_MS={a.interval_ms}',f'PAIR_BUDGET_USD={a.pair_budget:.2f}',
            ]
            if last_best:
                status += [f'CURRENT_COMPLETE_SET_Q={last_best["q"]}',f'CURRENT_COMPLETE_SET_NET={last_best["net"]:.6f}',f'CURRENT_COMPLETE_SET_ROI={last_best["roi"]*100:.4f}%']
            else: status += ['CURRENT_COMPLETE_SET=NONE']
            status += ['','RECENT BOT PAIR EVENTS']+[x['line'] for x in list(recent)[-8:]]
            tmp=a.status+'.tmp'; open(tmp,'w').write('\n'.join(status)+'\n'); os.replace(tmp,a.status)
            time.sleep(max(0.02,interval-(time.time()-t0)))
        out.write(json.dumps({'kind':'GATE_B_END','ts':utc(),'snapshots':snaps,'lead_events':lead_events,'hedge_events':hedge_events})+'\n')

if __name__=='__main__': main()
