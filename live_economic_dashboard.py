#!/usr/bin/env python3
import json, math, os, re, time, urllib.request
from datetime import datetime, timezone

LATEST_URL="https://githubcos.github.io/kalshi-diagnostic/latest.txt"
KALSHI_BASE="https://external-api.kalshi.com/trade-api/v2"
LOCAL_LOG=os.path.expanduser("~/KalshiArbo/kalshiarbo/polyarb.log")
JOURNAL=os.path.expanduser("~/kalshiarbo_live_economic_dashboard.jsonl")
POLL=2.0
DEPTH=100
TAKER_COEFF=0.07
ANSI=re.compile(r"\x1b\[[0-?]*[ -/]*[@-~]")
TICKER_RE=re.compile(r"\b(KXBTC15M-[A-Z0-9-]+)\b")
ENTRY_RE=re.compile(r"pair arb.*(?:entered|entering|entry|lead.*fill|hedge.*fill|both legs|dual.*fill|opened|buy)",re.I)
NOENTRY_RE=re.compile(r"pair arb: no entry snapshot",re.I)


def now(): return datetime.now(timezone.utc).isoformat()
def clean(s): return ANSI.sub("",s).replace("\r","")
def money(x): return "n/a" if x is None else f"${x:,.4f}"
def pct(x): return "n/a" if x is None else f"{100*x:,.2f}%"

def get_text(url,timeout=5):
    req=urllib.request.Request(url,headers={"User-Agent":"kalshiarbo-live-economic-dashboard/1.0","Cache-Control":"no-cache"})
    with urllib.request.urlopen(req,timeout=timeout) as r:
        return r.read().decode("utf-8","replace")
def get_json(url): return json.loads(get_text(url))

def extract_ticker(t):
    t=clean(t)
    s=[x for x in re.findall(r"Slug\s+([A-Z0-9-]+)",t) if x.startswith("KXBTC15M-")]
    if s:return s[-1]
    h=TICKER_RE.findall(t); return h[-1] if h else None

def trade_size(t):
    v=re.findall(r"Trade Size\s+\$([0-9.]+)",clean(t),re.I)
    return float(v[-1]) if v else 5.0

def start_balance(t):
    v=re.findall(r"Balance\s+\$([0-9.]+)\s+starting",clean(t),re.I)
    return float(v[-1]) if v else 20.0

def current_balance(t):
    t=clean(t)
    # Fast ticker lines end with current paper bankroll, e.g. ... | $19.81
    vals=[]
    for ln in t.splitlines():
        if "YES " in ln and "edge" in ln:
            m=re.findall(r"\$([0-9]+(?:\.[0-9]+)?)",ln)
            if m:
                try: vals.append(float(m[-1]))
                except: pass
    if vals:return vals[-1]
    # Fallback to explicit balance lines if dashboard/log format changes.
    v=re.findall(r"(?:paper\s+balance|balance)\s*[:=]?\s*\$([0-9.]+)",t,re.I)
    return float(v[-1]) if v else None

def bot_quote(t):
    t=clean(t)
    lines=[ln for ln in t.splitlines() if "YES " in ln and "edge" in ln]
    ln=lines[-1] if lines else ""
    yes=None
    m=re.search(r"YES\s+([0-9.]+)",ln)
    if m:
        try: yes=float(m.group(1))
        except: pass
    flat="-- flat --" in ln
    return yes,flat,ln[-320:]

def parse_book(raw):
    ob=raw.get("orderbook_fp") or raw.get("orderbook") or {}
    def lv(key):
        out=[]
        for row in ob.get(key,[]) or []:
            try: out.append((float(row[0]),float(row[1])))
            except: pass
        return sorted(out,reverse=True)
    yb=lv("yes_dollars"); nb=lv("no_dollars")
    # Kalshi returns bids. Opposite bid gives executable ask.
    ya=sorted([(round(1-p,10),q) for p,q in nb])
    na=sorted([(round(1-p,10),q) for p,q in yb])
    return yb,nb,ya,na

def sweep(levels,q):
    rem=q; cost=0.; pieces=[]
    for p,a in levels:
        x=min(rem,a)
        if x>1e-12:
            cost+=p*x; pieces.append((p,x)); rem-=x
        if rem<=1e-12: break
    return None if rem>1e-9 else (cost,pieces)

def fee(pieces):
    z=0.
    for p,q in pieces:
        z+=math.ceil(max(0,TAKER_COEFF*q*p*(1-p))*100-1e-12)/100
    return z

def evaluate(q,ya,na):
    a=sweep(ya,q); b=sweep(na,q)
    if not a or not b:return None
    yc,yp=a; nc,np=b; f=fee(yp)+fee(np); acq=yc+nc+f; payout=q; net=payout-acq
    return {"q":q,"yes_vwap":yc/q,"no_vwap":nc/q,"yes_cost":yc,"no_cost":nc,"fees":f,"acq":acq,"payout":payout,"net":net,"roi":net/acq if acq>0 else None,"yes_pieces":yp,"no_pieces":np}

def candidates(ya,na,budget):
    mx=min(sum(q for _,q in ya),sum(q for _,q in na))
    if mx<=0:return []
    pts={1.0,mx}; cy=cn=0.
    for _,q in ya: cy+=q; pts.add(min(cy,mx))
    for _,q in na: cn+=q; pts.add(min(cn,mx))
    for q in (2,3,4,5,6,8,10,12,15,20,25,50,100):
        if q<=mx: pts.add(float(q))
    out=[]
    for q in sorted(x for x in pts if x>0 and x<=mx):
        r=evaluate(q,ya,na)
        if r and r["acq"]<=budget+1e-9: out.append(r)
    return out

def read_log_tail():
    if not os.path.exists(LOCAL_LOG):return []
    try:
        with open(LOCAL_LOG,"rb") as f:
            f.seek(0,2); n=f.tell(); f.seek(max(0,n-500000)); t=f.read().decode("utf-8","replace")
        return clean(t).splitlines()
    except:return []

def entries(lines):
    return [x.strip() for x in lines if ENTRY_RE.search(x) and not NOENTRY_RE.search(x)]
def noentries(lines):
    return [x.strip() for x in lines if NOENTRY_RE.search(x)]

class State:
    def __init__(self,start_bal):
        self.start=time.time(); self.start_bal=start_bal; self.current=None; self.episodes=[]
        self.seen_entry=set(); self.bot_entries=[]; self.false_entries=[]
    def episodes_all(self):return self.episodes+([self.current] if self.current else [])
    def close(self):
        if self.current:
            self.current["end"]=now(); self.episodes.append(self.current); self.current=None
    def observe_market(self,ticker,best):
        positive=best is not None and best["net"]>1e-9
        if positive:
            if self.current is None or self.current["ticker"]!=ticker:
                self.close(); self.current={"id":len(self.episodes)+1,"ticker":ticker,"start":now(),"best":best,"captured":False,"entry_line":None}
            elif best["net"]>self.current["best"]["net"]:
                self.current["best"]=best
        elif self.current:
            self.close()
    def observe_bot(self,lines,ticker,positive):
        new=[]
        for e in entries(lines):
            if e in self.seen_entry:continue
            self.seen_entry.add(e); new.append(e)
            rec={"ts":now(),"ticker":ticker,"line":e,"independent_positive":positive}
            self.bot_entries.append(rec)
            if positive and self.current:
                self.current["captured"]=True; self.current["entry_line"]=e
            else:self.false_entries.append(rec)
        return new

def decision_reason(best,bot_flat,new_entries,bot_yes,ind_yes_ask,ind_no_ask,latest_noentry):
    positive=best is not None and best["net"]>1e-9
    entered=bool(new_entries)
    if positive and entered:
        return "CORRECT: bot entered while independent executable NET arbitrage was positive."
    if positive and not entered:
        reasons=[]
        if bot_flat: reasons.append("bot remains FLAT")
        if bot_yes is not None and ind_yes_ask is not None and abs(bot_yes-ind_yes_ask)>=0.02:
            reasons.append(f"bot YES quote {bot_yes:.3f} differs from live executable YES ask {ind_yes_ask:.3f}")
        if latest_noentry: reasons.append("bot logged 'no entry snapshot'")
        if not reasons: reasons.append("no recognized arb entry despite positive independent net edge")
        return "MISSED ARBITRAGE: " + "; ".join(reasons) + "."
    if (not positive) and entered:
        return "ERROR/FALSE POSITIVE: bot entered but independent executable NET profit was not > 0. This is not proven arbitrage."
    return "CORRECT NO-TRADE: independent executable NET arbitrage is not positive, so staying out is economically correct."

def render(st,ticker,tr,budget,bal,best,new_entries,bot_yes,bot_flat,bot_line,latest_noentry,ind_yes_ask,ind_no_ask):
    os.system("clear")
    eps=st.episodes_all(); theo=len(eps); captured=sum(1 for e in eps if e.get("captured")); missed=max(0,theo-captured)
    ent=len(st.bot_entries); false=len(st.false_entries); precision=(ent-false)/ent if ent else None
    perfect_profit=sum(max(0,e["best"]["net"]) for e in eps)
    captured_profit=sum(max(0,e["best"]["net"]) for e in eps if e.get("captured"))
    caprate=captured/theo if theo else None; missrate=missed/theo if theo else None; eff=captured_profit/perfect_profit if perfect_profit>0 else None
    actual_pnl=(bal-st.start_bal) if bal is not None else None
    actual_roi=(actual_pnl/st.start_bal) if actual_pnl is not None and st.start_bal>0 else None
    perfect_bal=st.start_bal+perfect_profit
    perfect_roi=perfect_profit/st.start_bal if st.start_bal>0 else None
    current_positive=best is not None and best["net"]>1e-9
    reason=decision_reason(best,bot_flat,new_entries,bot_yes,ind_yes_ask,ind_no_ask,latest_noentry)

    print("="*96)
    print(" KALSHIARBO LIVE ECONOMIC AUDIT — PAPER BANKROLL vs PERFECT EXECUTABLE ARBITRAGE")
    print("="*96)
    print(f" UTC {now()} | Market {ticker or 'waiting'} | refresh {POLL:.0f}s")
    print()
    print(" PAPER BANKROLL")
    print(f"   Starting bankroll              : {money(st.start_bal)}")
    print(f"   Current bot bankroll           : {money(bal)}")
    print(f"   Bot session P&L                : {money(actual_pnl)}   ({pct(actual_roi)})")
    print(f"   Perfect-arbitrage bankroll*    : {money(perfect_bal)}")
    print(f"   Perfect theoretical P&L*       : {money(perfect_profit)}   ({pct(perfect_roi)})")
    print("   *same observed markets, executable depth, fees, and comparison budget")
    print()
    print(" LIVE MATHEMATICAL PROOF")
    print("   NET = Q - YES_cost(Q) - NO_cost(Q) - fees(Q)")
    print("   REAL ARBITRAGE iff NET > 0 and both legs are executable for the same Q")
    if best:
        print(f"   Q                               : {best['q']:.4f}")
        print(f"   YES executable VWAP             : {best['yes_vwap']:.4f}  cost {money(best['yes_cost'])}")
        print(f"   NO  executable VWAP             : {best['no_vwap']:.4f}  cost {money(best['no_cost'])}")
        print(f"   Fees                            : {money(best['fees'])}")
        print(f"   Total acquisition              : {money(best['acq'])}")
        print(f"   Guaranteed payout              : {money(best['payout'])}")
        print(f"   NET LOCKED PROFIT               : {money(best['net'])}")
        print(f"   NET ROI                         : {pct(best['roi'])}")
        print(f"   THEORETICAL ACTION              : {'BUY BOTH / ARBITRAGE' if current_positive else 'DO NOT TRADE'}")
    else:
        print("   No two-sided executable quantity exists inside the comparison budget.")
        print("   THEORETICAL ACTION              : DO NOT TRADE")
    print()
    print(" BOT vs ECONOMICALLY CORRECT ACTION")
    print(f"   Bot state                       : {'FLAT' if bot_flat else 'IN POSITION / UNKNOWN'}")
    print(f"   New recognized bot entry        : {'YES' if new_entries else 'NO'}")
    print(f"   Bot displayed YES               : {'n/a' if bot_yes is None else f'{bot_yes:.4f}'}")
    print(f"   Independent YES ask             : {'n/a' if ind_yes_ask is None else f'{ind_yes_ask:.4f}'}")
    print(f"   Independent NO ask              : {'n/a' if ind_no_ask is None else f'{ind_no_ask:.4f}'}")
    print(f"   EVALUATION                      : {reason}")
    print()
    print(" PERFORMANCE vs PERFECT ARBITRAGE")
    print(f"   Theoretical positive opportunities: {theo:5d}")
    print(f"   Captured by bot                  : {captured:5d}   CAPTURE RATE {pct(caprate)}")
    print(f"   Missed by bot                    : {missed:5d}   MISS RATE    {pct(missrate)}")
    print(f"   Recognized bot arb entries       : {ent:5d}")
    print(f"   False-positive entries           : {false:5d}   FALSE RATE   {pct(false/ent if ent else None)}")
    print(f"   Entry precision                  : {'':5s}   {pct(precision)}")
    print(f"   Profit opportunity captured      : {pct(eff)}")
    print()
    print(" LAST 5 THEORETICAL OPPORTUNITIES")
    for e in eps[-5:]:
        b=e["best"]; mark="CAPTURED" if e.get("captured") else "MISSED/OPEN"
        print(f"   #{e['id']:03d} net {money(b['net']):>10} ROI {pct(b['roi']):>8} Q {b['q']:6.2f}  {mark}")
    if not eps: print("   none yet")
    print()
    print(" WHY BOT DID / DID NOT TRADE")
    print("   "+reason)
    if latest_noentry:
        print("   Latest bot no-entry evidence:")
        print("   "+latest_noentry[-220:])
    print()
    print(" LIVE-READINESS")
    if false:
        print("   FAIL — bot has entered at least once without independently proven positive NET arbitrage.")
    elif missed:
        print("   NOT READY — real executable arbitrage has been missed; scanner/filters/timing need explanation.")
    elif ent and captured:
        print("   PROVISIONAL PASS — recognized entries align with positive arbitrage so far; continue sample.")
    else:
        print("   OBSERVING — not enough actual arbitrage/entry events yet to prove live readiness.")
    print(f" Evidence journal: {JOURNAL}")
    print("="*96)


def main():
    st=None
    while True:
        t0=time.time()
        try:
            gh=get_text(LATEST_URL+f"?t={int(t0*1000)}")
            sb=start_balance(gh)
            if st is None:st=State(sb)
            ticker=extract_ticker(gh); tr=trade_size(gh); budget=2*tr; bal=current_balance(gh)
            b_yes,bot_flat,bot_line=bot_quote(gh)
            if not ticker:
                render(st,None,tr,budget,bal,None,[],b_yes,bot_flat,bot_line,None,None,None)
                time.sleep(POLL); continue
            raw=get_json(f"{KALSHI_BASE}/markets/{ticker}/orderbook?depth={DEPTH}")
            _,_,ya,na=parse_book(raw)
            rows=candidates(ya,na,budget)
            best=max(rows,key=lambda r:r["net"]) if rows else None
            st.observe_market(ticker,best)
            lines=read_log_tail(); new=st.observe_bot(lines,ticker,best is not None and best["net"]>1e-9)
            nes=noentries(lines); latest_ne=nes[-1] if nes else None
            ind_yes=ya[0][0] if ya else None; ind_no=na[0][0] if na else None
            rec={"ts":now(),"ticker":ticker,"start_balance":st.start_bal,"current_balance":bal,"bot_pnl":None if bal is None else bal-st.start_bal,"trade_size":tr,"pair_budget":budget,"best":best,"bot_yes":b_yes,"bot_flat":bot_flat,"new_entries":new,"reason":decision_reason(best,bot_flat,new,b_yes,ind_yes,ind_no,latest_ne),"theoretical_opportunities":len(st.episodes_all()),"captured":sum(1 for e in st.episodes_all() if e.get('captured')),"false_entries":len(st.false_entries)}
            with open(JOURNAL,"a") as f:f.write(json.dumps(rec,separators=(",",":"))+"\n")
            render(st,ticker,tr,budget,bal,best,new,b_yes,bot_flat,bot_line,latest_ne,ind_yes,ind_no)
        except KeyboardInterrupt:
            if st:st.close()
            print(f"\nStopped auditor only. KalshiArbo remains untouched. Evidence: {JOURNAL}")
            return
        except Exception as e:
            os.system("clear"); print("AUDITOR ERROR:",type(e).__name__,e); print("KalshiArbo is untouched; retrying...")
        time.sleep(max(.2,POLL-(time.time()-t0)))

if __name__=="__main__":main()
