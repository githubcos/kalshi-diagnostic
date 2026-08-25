#!/usr/bin/env python3
import argparse, json, math, os, re, sys, time, urllib.parse, urllib.request
from datetime import datetime, timezone

BASE="https://external-api.kalshi.com/trade-api/v2"
ANSI=re.compile(r'\x1b\[[0-9;]*[A-Za-z]')

def utc(): return datetime.now(timezone.utc).isoformat()
def get_json(url, timeout=4):
    req=urllib.request.Request(url, headers={"User-Agent":"kalshiarbo-independent-auditor/1.0"})
    with urllib.request.urlopen(req, timeout=timeout) as r: return json.load(r)
def fee(q,p):
    if q<=0 or p<=0 or p>=1: return 0.0
    return math.ceil((0.07*q*p*(1-p))*100-1e-9)/100.0

def parse_levels(x):
    out=[]
    for row in x or []:
        try:
            p=float(row[0]); q=float(row[1])
            if 0<p<1 and q>0: out.append((p,q))
        except Exception: pass
    return out

def asks_from_bids(yes_bids,no_bids):
    ya=sorted([(1-p,q) for p,q in no_bids], key=lambda z:z[0])
    na=sorted([(1-p,q) for p,q in yes_bids], key=lambda z:z[0])
    return ya,na

def sweep(levels,q):
    left=q; cost=0.0; worst=0.0
    for p,n in levels:
        if left<=1e-9: break
        take=min(left,n); cost += take*p; worst=p; left -= take
    return (cost,worst,left<=1e-9)

def best_plan(ya,na,max_spend,min_profit_per_contract):
    maxq=int(math.floor(min(sum(q for _,q in ya),sum(q for _,q in na),10000)))
    best=None
    for qi in range(1,maxq+1):
        q=float(qi)
        yc,yw,yok=sweep(ya,q); nc,nw,nok=sweep(na,q)
        if not yok or not nok: break
        yv=yc/q; nv=nc/q
        fees=fee(q,yv)+fee(q,nv)
        total=yc+nc+fees
        if max_spend>0 and total>max_spend: continue
        net=q-total
        if net<=0 or net < min_profit_per_contract*q: continue
        plan={"q":q,"yes_vwap":yv,"no_vwap":nv,"yes_worst":yw,"no_worst":nw,
              "yes_cost":yc,"no_cost":nc,"fees":fees,"total_cost":total,
              "payout":q,"net":net,"roi":net/total if total else 0.0}
        if best is None or plan["net"]>best["net"]: best=plan
    return best

def current_market():
    q=urllib.parse.urlencode({"status":"open","series_ticker":"KXBTC15M","limit":100})
    d=get_json(BASE+"/markets?"+q)
    ms=d.get("markets",[])
    if not ms: return None
    def key(m):
        return m.get("close_time") or m.get("expiration_time") or "9999"
    return sorted(ms,key=key)[0]

def orderbook(ticker):
    d=get_json(BASE+"/markets/"+urllib.parse.quote(ticker,safe='')+"/orderbook")
    ob=d.get("orderbook_fp") or d.get("orderbook") or {}
    y=parse_levels(ob.get("yes_dollars") or ob.get("yes") or [])
    n=parse_levels(ob.get("no_dollars") or ob.get("no") or [])
    return y,n

def strip(s): return ANSI.sub('',s)

def write_atomic(path,text):
    tmp=path+".tmp"; open(tmp,"w").write(text); os.replace(tmp,path)

def main():
    ap=argparse.ArgumentParser()
    ap.add_argument('--bot-log',required=True); ap.add_argument('--status',required=True); ap.add_argument('--events',required=True)
    ap.add_argument('--duration',type=int,default=2400); ap.add_argument('--interval',type=float,default=2.0)
    ap.add_argument('--max-spend',type=float,default=10.0); ap.add_argument('--min-profit-per-contract',type=float,default=0.0)
    ap.add_argument('--start-bankroll',type=float,default=20.0)
    a=ap.parse_args()
    start=time.time(); pos=0; current_balance=a.start_bankroll
    opp_active=False; opp_id=0; opp_start=None; opp_best=None; opp_captured=False
    opportunities=0; captured=0; missed=0; false_entries=0; bot_entries=0; bot_proofs=0; residuals=0; completed=0
    perfect_profit=0.0; captured_theoretical_profit=0.0; last_reason="No mismatch yet."
    last_bot_events=[]; last_ticker=None; api_errors=0
    eventsf=open(a.events,'a',buffering=1)

    def emit(kind,**kw):
        rec={"ts":utc(),"kind":kind,**kw}; eventsf.write(json.dumps(rec,separators=(',',':'))+'\n')

    while time.time()-start < a.duration:
        cycle=time.time(); now=utc(); new_entry=False; strict_proof=False
        try:
            if os.path.exists(a.bot_log):
                with open(a.bot_log,'r',errors='replace') as f:
                    f.seek(pos); chunk=f.read(); pos=f.tell()
                for raw in chunk.splitlines():
                    line=strip(raw)
                    if not line.strip(): continue
                    if re.search(r'STRICT executable arbitrage proven',line,re.I):
                        bot_proofs+=1; strict_proof=True; last_bot_events.append(line[-500:]); emit('BOT_STRICT_PROOF',line=line[-1000:])
                    if re.search(r'BUY (YES|NO) \[PAIR',line,re.I):
                        bot_entries+=1; new_entry=True; last_bot_events.append(line[-500:]); emit('BOT_PAIR_ENTRY',line=line[-1000:])
                    if 'PAIR RESIDUAL' in line.upper():
                        residuals+=1; last_bot_events.append(line[-500:]); emit('BOT_RESIDUAL',line=line[-1000:])
                    if re.search(r'SELL PAIR EXECUTED',line,re.I):
                        completed+=1; last_bot_events.append(line[-500:]); emit('BOT_PAIR_COMPLETE',line=line[-1000:])
                    if re.search(r'risk controls|strict preflight rejected|position limit|orderbook|insufficient|blocked|timeout|abort',line,re.I):
                        last_bot_events.append(line[-500:])
                    m=re.search(r'\$([0-9]+(?:\.[0-9]+)?)\s*$',line)
                    if m and ('BTC ' in line or 'flat' in line or 'PAIR' in line):
                        try: current_balance=float(m.group(1))
                        except: pass
                last_bot_events=last_bot_events[-12:]
        except Exception as e:
            last_bot_events.append('bot-log-read-error: '+repr(e))

        market=None; plan=None; classify='NO_DATA'; ybest=nbest=None
        try:
            market=current_market()
            if market:
                ticker=market.get('ticker','')
                yb,nb=orderbook(ticker); ya,na=asks_from_bids(yb,nb)
                ybest=ya[0][0] if ya else None; nbest=na[0][0] if na else None
                plan=best_plan(ya,na,a.max_spend,a.min_profit_per_contract)
                classify='REAL_EXECUTABLE_ARB' if plan else 'NO_NET_ARB'
                last_ticker=ticker
            api_errors=0
        except Exception as e:
            api_errors+=1; classify='API_ERROR'; emit('API_ERROR',error=repr(e)); plan=None

        if plan and not opp_active:
            opp_active=True; opp_id+=1; opportunities+=1; opp_start=now; opp_best=plan.copy(); opp_captured=False
            emit('THEORETICAL_ARB_START',opportunity_id=opp_id,ticker=last_ticker,plan=plan)
        elif plan and opp_active and plan['net'] > (opp_best or {}).get('net',-1):
            opp_best=plan.copy()
        elif not plan and opp_active:
            perfect_profit += (opp_best or {}).get('net',0.0)
            if opp_captured:
                captured+=1; captured_theoretical_profit += (opp_best or {}).get('net',0.0)
            else:
                missed+=1
                last_reason='MISSED: independent book proved positive net edge during opportunity episode, but no bot pair entry was observed.'
            emit('THEORETICAL_ARB_END',opportunity_id=opp_id,captured=opp_captured,best=opp_best,start=opp_start,end=now)
            opp_active=False; opp_best=None; opp_start=None; opp_captured=False

        if new_entry:
            if plan or opp_active:
                if not opp_captured:
                    opp_captured=True
                last_reason='CORRECT: bot entry coincided with independently positive executable complete-set edge.'
            else:
                false_entries+=1
                last_reason='ERROR: bot entered PAIR while independent orderbook did not prove positive net arbitrage.'
        elif plan and not new_entry:
            # Explain only after enough time for bot to react.
            last_reason='WAITING: real executable arbitrage exists now; checking whether bot captures it. ' + (last_bot_events[-1] if last_bot_events else 'No bot rejection logged yet.')
        elif not plan:
            last_reason='CORRECT TO STAY FLAT: independent net edge is not positive after depth and estimated fees.'

        open_perfect=perfect_profit + ((opp_best or {}).get('net',0.0) if opp_active else 0.0)
        cap_rate=(captured/opportunities*100) if opportunities else 0.0
        miss_rate=(missed/opportunities*100) if opportunities else 0.0
        precision=((bot_entries-false_entries)/bot_entries*100) if bot_entries else 100.0
        profit_eff=(captured_theoretical_profit/perfect_profit*100) if perfect_profit>0 else 0.0
        bot_pnl=current_balance-a.start_bankroll; bot_pct=bot_pnl/a.start_bankroll*100 if a.start_bankroll else 0
        perfect_bankroll=a.start_bankroll+open_perfect
        lines=[]
        lines += ['KALSHIARBO STRICT ARBITRAGE — LIVE INDEPENDENT AUDIT',f'UTC: {now}',f'Market: {last_ticker or "unknown"}',f'Source: Kalshi public full orderbook, independent of bot','']
        lines += ['MATHEMATICAL RULE','NET(q) = q - YES_cost(q) - NO_cost(q) - fees(q)','REAL ARBITRAGE iff NET(q) > 0 at executable depth','']
        lines += ['BANKROLL / PERFORMANCE',f'Paper start bankroll          ${a.start_bankroll:10.4f}',f'Paper current bankroll        ${current_balance:10.4f}',f'Bot session P&L               ${bot_pnl:+10.4f}  ({bot_pct:+7.2f}%)',f'Perfect episode bankroll      ${perfect_bankroll:10.4f}',f'Perfect episode P&L           ${open_perfect:+10.4f}  ({open_perfect/a.start_bankroll*100:+7.2f}%)','']
        lines += ['OPPORTUNITY SCORECARD',f'Theoretical arb episodes      {opportunities}',f'Captured completed episodes   {captured}',f'Missed completed episodes     {missed}',f'Capture rate                  {cap_rate:7.2f}%',f'Miss rate                     {miss_rate:7.2f}%',f'Bot pair entries              {bot_entries}',f'Bot strict proofs             {bot_proofs}',f'False-positive entries        {false_entries}',f'Entry precision               {precision:7.2f}%',f'Residual/orphan events        {residuals}',f'Completed pair events         {completed}',f'Profit capture efficiency*    {profit_eff:7.2f}%','* episode-level counterfactual; one fill per continuous opportunity episode','']
        lines += ['CURRENT MARKET MATH',f'Best YES executable ask       {ybest if ybest is not None else "N/A"}',f'Best NO executable ask        {nbest if nbest is not None else "N/A"}',f'Classification                {classify}']
        if plan:
            lines += [f'Optimal q under ${a.max_spend:.2f} budget   {plan["q"]:.0f}',f'YES VWAP / cost               {plan["yes_vwap"]:.4f} / ${plan["yes_cost"]:.4f}',f'NO  VWAP / cost               {plan["no_vwap"]:.4f} / ${plan["no_cost"]:.4f}',f'Estimated fees                ${plan["fees"]:.4f}',f'Total acquisition             ${plan["total_cost"]:.4f}',f'Guaranteed payout             ${plan["payout"]:.4f}',f'NET LOCKED PROFIT             ${plan["net"]:+.4f}',f'NET ROI                       {plan["roi"]*100:+.3f}%']
        else:
            lines += ['Optimal executable arb         NONE']
        lines += ['', 'BOT VS CORRECT ACTION', last_reason, '', 'RECENT IMPORTANT BOT EVENTS']
        lines += (last_bot_events[-8:] if last_bot_events else ['None'])
        write_atomic(a.status,'\n'.join(lines)+'\n')
        # screen live
        sys.stdout.write('\033[2J\033[H'+'\n'.join(lines)+'\n'); sys.stdout.flush()
        emit('SNAPSHOT',ticker=last_ticker,classify=classify,plan=plan,balance=current_balance,opportunity_id=(opp_id if opp_active else None),bot_entries=bot_entries,false_entries=false_entries)
        time.sleep(max(0.05,a.interval-(time.time()-cycle)))

    if opp_active:
        perfect_profit += (opp_best or {}).get('net',0.0)
        if opp_captured:
            captured+=1; captured_theoretical_profit += (opp_best or {}).get('net',0.0)
        else: missed+=1
        emit('THEORETICAL_ARB_END',opportunity_id=opp_id,captured=opp_captured,best=opp_best,start=opp_start,end=utc(),forced_end=True)
    emit('AUDIT_END',opportunities=opportunities,captured=captured,missed=missed,bot_entries=bot_entries,false_entries=false_entries,residuals=residuals,completed=completed,perfect_profit=perfect_profit,captured_theoretical_profit=captured_theoretical_profit,current_balance=current_balance)
    eventsf.close()

if __name__=='__main__': main()
