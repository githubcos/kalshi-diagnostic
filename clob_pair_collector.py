#!/usr/bin/env python3
import json, re, time, urllib.parse, urllib.request
from pathlib import Path
from datetime import datetime, timezone

BASE = "https://clob.polymarket.com/book?token_id="
LOG = Path.home() / "ArboCos" / "realpaper" / "polyarb.log"
OUT = Path.home() / "ArboCos" / "realpaper" / "clob_pair_samples.jsonl"
USD = 5.0
POLL_SEC = 1.0

pat = re.compile(r'market tokens updated.*?yes_token_id[^0-9]*(\d+).*?no_token_id[^0-9]*(\d+).*?condition_id[^0-9A-Za-z]*(0x[0-9a-fA-F]+)', re.I)

def latest_tokens():
    try:
        lines = LOG.read_text(errors="ignore").splitlines()
    except FileNotFoundError:
        return None
    for line in reversed(lines[-5000:]):
        m = pat.search(line)
        if m:
            return m.group(1), m.group(2), m.group(3)
    return None

def get_book(token):
    req = urllib.request.Request(BASE + urllib.parse.quote(token), headers={"User-Agent":"arbocos-clob-probe/1.0"})
    with urllib.request.urlopen(req, timeout=3) as r:
        return json.load(r)

def levels(book):
    out=[]
    for a in book.get("asks",[]) or []:
        try:
            p=float(a.get("price")); s=float(a.get("size"))
            if p>0 and s>0: out.append((p,s))
        except Exception: pass
    return sorted(out)

def buy_usd(book, usd):
    spent=0.0; shares=0.0; worst=None
    for p,s in levels(book):
        if spent >= usd-1e-12: break
        take=min(s,(usd-spent)/p)
        if take<=0: continue
        spent += take*p; shares += take; worst=p
    if spent < usd-1e-6 or shares<=0:
        return None
    return {"usd":spent,"shares":shares,"vwap":spent/shares,"worst_ask":worst}

def equal_share_cost(yb, nb, target_usd):
    y=buy_usd(yb,target_usd)
    n=buy_usd(nb,target_usd)
    if not y or not n: return None
    q=min(y["shares"],n["shares"])
    if q<=0: return None
    def cost_for(book,q):
        rem=q; cost=0.0; worst=None
        for p,s in levels(book):
            take=min(rem,s)
            cost += take*p; rem -= take; worst=p
            if rem<=1e-9: break
        if rem>1e-6: return None
        return cost,worst
    yc=cost_for(yb,q); nc=cost_for(nb,q)
    if not yc or not nc: return None
    total=yc[0]+nc[0]
    return {"shares":q,"yes_cost":yc[0],"no_cost":nc[0],"combined_cost_per_share":total/q,"locked_edge_cents":(1-total/q)*100,"yes_worst":yc[1],"no_worst":nc[1]}

def main():
    print("CLOB collector running; Ctrl-C to stop")
    current=None
    while True:
        tok=latest_tokens()
        if not tok:
            print("waiting for market token log...")
            time.sleep(POLL_SEC); continue
        if tok!=current:
            current=tok; print("tracking new market",tok[2])
        y,n,c=tok
        row={"at":datetime.now(timezone.utc).isoformat(),"yes_token_id":y,"no_token_id":n,"condition_id":c}
        try:
            yb=get_book(y); nb=get_book(n)
            row["yes_best_ask"]=levels(yb)[0][0] if levels(yb) else None
            row["no_best_ask"]=levels(nb)[0][0] if levels(nb) else None
            row["pair_5usd"]=equal_share_cost(yb,nb,USD)
        except Exception as e:
            row["error"]=str(e)
        with OUT.open("a") as f:
            f.write(json.dumps(row,separators=(",",":"))+"\n")
        if row.get("pair_5usd"):
            p=row["pair_5usd"]
            print(row["at"],f'edge={p["locked_edge_cents"]:.2f}c combined={p["combined_cost_per_share"]:.4f} shares={p["shares"]:.2f}')
        else:
            print(row["at"],row.get("error","insufficient depth"))
        time.sleep(POLL_SEC)

if __name__=="__main__": main()
