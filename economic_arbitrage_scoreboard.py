#!/usr/bin/env python3
import json, math, os, re, time, urllib.request
from datetime import datetime, timezone

LATEST_URL="https://githubcos.github.io/kalshi-diagnostic/latest.txt"
KALSHI_BASE="https://external-api.kalshi.com/trade-api/v2"
LOCAL_LOG=os.path.expanduser("~/KalshiArbo/kalshiarbo/polyarb.log")
JOURNAL=os.path.expanduser("~/kalshiarbo_economic_audit.jsonl")
POLL=2.0
DEPTH=100
TAKER_COEFF=0.07
ANSI=re.compile(r"\x1b\[[0-?]*[ -/]*[@-~]")
TICKER_RE=re.compile(r"\b(KXBTC15M-[A-Z0-9-]+)\b")
ENTRY_RE=re.compile(r"pair arb.*(?:entered|entering|entry|lead.*fill|hedge.*fill|both legs|dual.*fill|opened)",re.I)
NOENTRY_RE=re.compile(r"no entry",re.I)


def now(): return datetime.now(timezone.utc).isoformat()
def clean(s): return ANSI.sub("",s).replace("\r","")
def get_text(url,timeout=5):
    req=urllib.request.Request(url,headers={"User-Agent":"kalshiarbo-economic-auditor/2.0","Cache-Control":"no-cache"})
    with urllib.request.urlopen(req,timeout=timeout) as r:return r.read().decode("utf-8","replace")
def get_json(url): return json.loads(get_text(url))
def money(x): return "n/a" if x is None else f"${x:,.4f}"
def pct(x): return "n/a" if x is None else f"{100*x:,.2f}%"

def extract_ticker(t):
    t=clean(t); s=re.findall(r"Slug\s+([A-Z0-9-]+)",t)
    s=[x for x in s if x.startswith("KXBTC15M-")]
    if s:return s[-1]
    h=TICKER_RE.findall(t); return h[-1] if h else None

def trade_size(t):
    v=re.findall(r"Trade Size\s+\$([0-9.]+)",clean(t),re.I)
    return float(v[-1]) if v else 5.0

def parse_book(raw):
    ob=raw.get("orderbook_fp") or raw.get("orderbook") or {}
    def levels(key):
        out=[]
        for row in ob.get(key,[]) or []:
            try: out.append((float(row[0]),float(row[1])))
            except: pass
        return sorted(out,reverse=True)
    yb=levels("yes_dollars"); nb=levels("no_dollars")
    ya=sorted([(1-p,q) for p,q in nb])
    na=sorted([(1-p,q) for p,q in yb])
    return yb,nb,ya,na

def sweep(levels,q):
    rem=q; cost=0.; pieces=[]
    for p,a in levels:
        x=min(rem,a)
        if x>1e-12: cost+=p*x; pieces.append((p,x)); rem-=x
        if rem<=1e-12: break
    return None if rem>1e-9 else (cost,pieces)

def fee(pieces):
    # Conservative application of Kalshi's general taker coefficient, rounded upward by level.
    z=0.
    for p,q in pieces:
        z+=math.ceil(max(0,TAKER_COEFF*q*p*(1-p))*100-1e-12)/100
    return z

def evaluate(q,ya,na):
    a=sweep(ya,q); b=sweep(na,q)
    if not a or not b:return None
    yc,yp=a; nc,np=b; f=fee(yp)+fee(np); cost=yc+nc; payout=q
    net=payout-cost-f
    return dict(q=q,yes_vwap=yc/q,no_vwap=nc/q,yes_cost=yc,no_cost=nc,fees=f,cost=cost,payout=payout,gross=payout-cost,net=net,roi=(net/(cost+f) if cost+f>0 else None),yes_pieces=yp,no_pieces=np)

def candidates(ya,na,budget):
    mx=min(sum(q for _,q in ya),sum(q for _,q in na))
    pts={1.0}
    cy=cn=0.
    for _,q in ya: cy+=q; pts.add(min(cy,mx))
    for _,q in na: cn+=q; pts.add(min(cn,mx))
    for q in [2,3,4,5,6,8,10,12,15,20,25,50,100]:
        if q<=mx: pts.add(float(q))
    rows=[]
    for q in sorted(x for x in pts if x>0 and x<=mx):
        r=evaluate(q,ya,na)
        if r and r["cost"]+r["fees"]<=budget+1e-9: rows.append(r)
    return rows

def read_log_tail():
    if not os.path.exists(LOCAL_LOG): return []
    try:
        with open(LOCAL_LOG,"rb") as f:
            f.seek(0,2); n=f.tell(); f.seek(max(0,n-300000)); t=f.read().decode("utf-8","replace")
        return clean(t).splitlines()
    except:return []

def entry_events(lines):
    out=[]
    for x in lines:
        if NOENTRY_RE.search(x): continue
        if ENTRY_RE.search(x): out.append(x.strip())
    return out

def bot_noentry(lines):
    return [x.strip() for x in lines if re.search(r"pair arb: no entry snapshot",x,re.I)]

class State:
    def __init__(self):
        self.start=time.time(); self.current=None; self.episodes=[]; self.bot_seen=set(); self.bot_entries=[]; self.false_entries=[]; self.captured=set(); self.last_entry_count=0
    def add_snapshot(self,ticker,best):
        positive=best is not None and best["net"]>1e-9
        if positive:
            if self.current is None or self.current["ticker"]!=ticker:
                if self.current:self.close_episode()
                self.current={"id":len(self.episodes)+1,"ticker":ticker,"start":now(),"best":best,"captured":False}
            elif best["net"]>self.current["best"]["net"]:
                self.current["best"]=best
        elif self.current:
            self.close_episode()
    def close_episode(self):
        if self.current:
            self.current["end"]=now(); self.episodes.append(self.current); self.current=None
    def all_eps(self): return self.episodes+([self.current] if self.current else [])
    def observe_entries(self,events,current_positive,current_ticker):
        for e in events:
            if e in self.bot_seen: continue
            self.bot_seen.add(e); self.bot_entries.append({"ts":now(),"line":e,"ticker":current_ticker,"valid_market_now":current_positive})
            if current_positive and self.current:
                self.current["captured"]=True; self.captured.add(self.current["id"])
            else:self.false_entries.append(e)


def render(st,ticker,trade,budget,best,bot_events,noentry,book_age="LIVE"):
    os.system("clear")
    eps=st.all_eps(); theo=len(eps); captured=sum(1 for e in eps if e.get("captured")); missed=max(0,theo-captured)
    entries=len(st.bot_entries); false=len(st.false_entries); valid_entries=max(0,entries-false)
    cap_rate=captured/theo if theo else None; precision=valid_entries/entries if entries else None
    perfect_profit=sum(max(0,e["best"]["net"]) for e in eps)
    captured_profit=sum(max(0,e["best"]["net"]) for e in eps if e.get("captured"))
    profit_eff=captured_profit/perfect_profit if perfect_profit>0 else None
    runtime=(time.time()-st.start)/60
    print("="*92)
    print(" KALSHIARBO — ECONOMIC ARBITRAGE AUDIT SCOREBOARD (READ ONLY)")
    print("="*92)
    print(f" Runtime {runtime:6.1f} min | Market {ticker or 'waiting'} | Source {book_age}")
    print(f" Bot trade size ${trade:.2f}/entry | Fair comparison pair-budget ${budget:.2f}")
    print()
    print(" MATHEMATICAL RULE")
    print("   NET PROFIT = Q - YES_cost(Q) - NO_cost(Q) - fees(Q)")
    print("   REAL ARBITRAGE iff NET PROFIT > 0, with both legs executable at Q.")
    print("   ROI = NET PROFIT / [YES_cost + NO_cost + fees]")
    print()
    print(" PERFORMANCE VS PERFECT EXECUTABLE ARBITRAGE (same observed market, same budget)")
    print(f"   Theoretical arb opportunities : {theo:5d}")
    print(f"   Bot captured                 : {captured:5d}   capture rate {pct(cap_rate)}")
    print(f"   Bot missed                   : {missed:5d}   miss rate    {pct(missed/theo if theo else None)}")
    print(f"   Bot arb-entry events         : {entries:5d}")
    print(f"   False-positive entries       : {false:5d}   false rate   {pct(false/entries if entries else None)}")
    print(f"   Entry precision              : {'':5s}   {pct(precision)}")
    print()
    print(f"   Perfect theoretical profit   : {money(perfect_profit)}")
    print(f"   Profit attached to captures  : {money(captured_profit)}")
    print(f"   Profit-capture efficiency    : {pct(profit_eff)}")
    print("   NOTE: captured-profit above is opportunity profit, not claimed actual bot fill P&L.")
    print()
    print(" CURRENT PERFECT ARBITRAGE CALCULATION")
    if best and best["net"]>0:
        print(f"   Q matched                    : {best['q']:.4f}")
        print(f"   YES VWAP × Q                 : {best['yes_vwap']:.4f} × {best['q']:.4f} = {money(best['yes_cost'])}")
        print(f"   NO  VWAP × Q                 : {best['no_vwap']:.4f} × {best['q']:.4f} = {money(best['no_cost'])}")
        print(f"   Fees                         : {money(best['fees'])}")
        print(f"   Total acquisition            : {money(best['cost']+best['fees'])}")
        print(f"   Guaranteed payout            : {money(best['payout'])}")
        print(f"   NET LOCKED PROFIT            : {money(best['net'])}")
        print(f"   NET ROI                      : {pct(best['roi'])}")
        print("   VERDICT                      : REAL EXECUTABLE ARBITRAGE")
    elif best:
        print(f"   Best available Q             : {best['q']:.4f}")
        print(f"   YES + NO + fees cost         : {money(best['cost']+best['fees'])}")
        print(f"   Guaranteed payout            : {money(best['payout'])}")
        print(f"   NET                          : {money(best['net'])} ({pct(best['roi'])})")
        print("   VERDICT                      : NO ARBITRAGE")
    else: print("   No two-sided executable quantity inside comparison budget.")
    print()
    print(" LAST THEORETICAL OPPORTUNITIES")
    for e in eps[-5:]:
        b=e['best']; mark="CAPTURED" if e.get('captured') else "MISSED/OPEN"
        print(f"   #{e['id']:03d} {e['ticker'][-12:]:12s} net {money(b['net']):>10} ROI {pct(b['roi']):>8} Q {b['q']:6.2f}  {mark}")
    print()
    print(" LAST BOT ARBITRAGE ENTRIES")
    if not st.bot_entries: print("   none recognized yet")
    for x in st.bot_entries[-5:]: print("  ",x['line'][:180])
    print()
    print(" AUDIT STATUS")
    if false: print("   FAIL: at least one bot arb-entry occurred while independent executable net edge was not positive.")
    elif entries and captured: print("   OBSERVING: recognized bot entries currently consistent with positive-edge episodes.")
    else: print("   OBSERVING: insufficient bot-entry events so far for live-readiness proof.")
    print(f" Evidence journal: {JOURNAL}")
    print("="*92)


def main():
    st=State()
    while True:
        t0=time.time()
        try:
            gh=get_text(LATEST_URL+f"?t={int(t0*1000)}")
            ticker=extract_ticker(gh)
            if not ticker:
                render(st,None,trade_size(gh),2*trade_size(gh),None,[],[],"WAITING FOR TICKER"); time.sleep(POLL); continue
            tr=trade_size(gh); budget=2*tr
            raw=get_json(f"{KALSHI_BASE}/markets/{ticker}/orderbook?depth={DEPTH}")
            _,_,ya,na=parse_book(raw)
            rows=candidates(ya,na,budget)
            best=max(rows,key=lambda r:r['net']) if rows else None
            st.add_snapshot(ticker,best)
            lines=read_log_tail(); events=entry_events(lines); ne=bot_noentry(lines)
            positive=best is not None and best['net']>1e-9
            st.observe_entries(events,positive,ticker)
            rec={"ts":now(),"ticker":ticker,"trade_size":tr,"pair_budget":budget,"best":best,"theoretical_positive":positive,"bot_entries_total":len(st.bot_entries),"episodes_total":len(st.all_eps()),"captured_total":sum(1 for e in st.all_eps() if e.get('captured')),"false_entries_total":len(st.false_entries)}
            with open(JOURNAL,"a") as f:f.write(json.dumps(rec,separators=(",",":"))+"\n")
            render(st,ticker,tr,budget,best,events,ne)
        except KeyboardInterrupt:
            st.close_episode(); print(f"\nStopped auditor only. Evidence saved: {JOURNAL}"); return
        except Exception as e:
            os.system("clear"); print("AUDITOR ERROR",type(e).__name__,e); print("KalshiArbo is untouched.")
        time.sleep(max(.2,POLL-(time.time()-t0)))

if __name__=="__main__": main()
