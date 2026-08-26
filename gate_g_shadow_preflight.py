#!/usr/bin/env python3
import argparse, json, math, os, re, time, urllib.parse, urllib.request
from datetime import datetime, timezone

BASES=['https://api.elections.kalshi.com/trade-api/v2','https://external-api.kalshi.com/trade-api/v2']
UA='kalshiarbo-gate-g-shadow/1.0'
ANSI=re.compile(r'\x1b\[[0-9;?]*[ -/]*[@-~]')
SIG=re.compile(r'PAIR ARB LEAD (YES|NO).*?\[btc=([0-9.]+) open=([0-9.]+) gap=\$(-?[0-9.]+) yes=([0-9.]+) prob=([0-9.]+) rem=([0-9]+)s\]',re.I)


def utc(): return datetime.now(timezone.utc).isoformat(timespec='milliseconds')
def get_json(path,timeout=3):
    last=None
    for base in BASES:
        try:
            req=urllib.request.Request(base+path,headers={'User-Agent':UA,'Cache-Control':'no-cache'})
            with urllib.request.urlopen(req,timeout=timeout) as r:return json.load(r)
        except Exception as e:last=e
    raise last

def levels(rows):
    out=[]
    for row in rows or []:
        try:
            p=float(row[0]); q=float(row[1])
            if 0<p<1 and q>0: out.append((p,q))
        except: pass
    return out

def asks_from_bids(yb,nb):
    return sorted([(1-p,q) for p,q in nb]),sorted([(1-p,q) for p,q in yb])

def fee(q,p):
    if q<=0 or p<=0 or p>=1:return 0.0
    return math.ceil((0.07*q*p*(1-p))*100-1e-9)/100

def sweep(ls,q):
    left=q; raw=0.0; worst=0.0
    for p,n in ls:
        if left<=1e-9:break
        take=min(left,n); raw+=take*p; worst=p; left-=take
    if left>1e-9:return None
    vwap=raw/q
    return {'q':q,'vwap':vwap,'raw':raw,'fee':fee(q,vwap),'total':raw+fee(q,vwap),'worst':worst}

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

def main():
    ap=argparse.ArgumentParser()
    ap.add_argument('--bot-log',default=os.path.expanduser('~/KalshiArbo/kalshiarbo/polyarb.log'))
    ap.add_argument('--out',default=os.path.expanduser('~/gate_g_shadow.jsonl'))
    ap.add_argument('--status',default=os.path.expanduser('~/gate_g_status.txt'))
    ap.add_argument('--series',default='KXBTC15M')
    ap.add_argument('--budget',type=float,default=5.0)
    ap.add_argument('--duration-sec',type=int,default=120)
    a=ap.parse_args()
    start=time.time(); end=start+a.duration_sec
    pos=os.path.getsize(a.bot_log) if os.path.exists(a.bot_log) else 0
    signals=0; captured=0; errors=0
    with open(a.out,'a',buffering=1) as out:
        out.write(json.dumps({'kind':'GATE_G_START','ts':utc(),'duration_sec':a.duration_sec,'budget':a.budget})+'\n')
        while time.time()<end:
            if not os.path.exists(a.bot_log):time.sleep(.1);continue
            with open(a.bot_log,'r',errors='replace') as f:
                f.seek(pos); chunk=f.read(); pos=f.tell()
            for raw in chunk.splitlines():
                line=ANSI.sub('',raw)
                m=SIG.search(line)
                if not m:continue
                signals+=1
                side=m.group(1).upper(); rec={'kind':'SIGNAL_PREFLIGHT','ts':utc(),'side':side,'signal_line':line[-1600:],'btc':float(m.group(2)),'open':float(m.group(3)),'gap':float(m.group(4)),'signal_yes':float(m.group(5)),'prob':float(m.group(6)),'rem':int(m.group(7))}
                try:
                    mk=current_market(a.series)
                    ticker=(mk or {}).get('ticker')
                    if not ticker:raise RuntimeError('no open market')
                    yb,nb,ya,na=book(ticker)
                    lead_asks=ya if side=='YES' else na
                    hedge_asks=na if side=='YES' else ya
                    if not lead_asks or not hedge_asks:raise RuntimeError('empty asks')
                    lead_best=lead_asks[0][0]
                    q=max(1,int(a.budget/max(lead_best,0.01)))
                    lead=sweep(lead_asks,float(q)); hedge=sweep(hedge_asks,float(q))
                    if not lead or not hedge:raise RuntimeError('insufficient depth')
                    total=lead['total']+hedge['total']; net=q-total; per=net/q
                    rec.update({'ticker':ticker,'q':q,'lead':lead,'hedge':hedge,'pair_total':total,'locked_net':net,'locked_per_contract':per,'thresholds':{str(x):per>=x for x in [0.00,0.02,0.04,0.06,0.08]},'yes_best_ask':ya[0][0],'no_best_ask':na[0][0]})
                    captured+=1
                except Exception as e:
                    errors+=1; rec['error']=type(e).__name__+': '+str(e)
                out.write(json.dumps(rec,separators=(',',':'))+'\n')
            status=f'KALSHI GATE G SHADOW PREFLIGHT\nUTC={utc()}\nRUNTIME_SEC={time.time()-start:.1f}\nSIGNALS={signals}\nCAPTURED={captured}\nERRORS={errors}\nLIVE_ORDERS_ALLOWED=false\n'
            tmp=a.status+'.tmp'; open(tmp,'w').write(status); os.replace(tmp,a.status)
            time.sleep(.05)
        out.write(json.dumps({'kind':'GATE_G_END','ts':utc(),'signals':signals,'captured':captured,'errors':errors})+'\n')

if __name__=='__main__':main()
