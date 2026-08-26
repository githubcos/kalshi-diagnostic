#!/usr/bin/env python3
import json, time, urllib.parse, urllib.request
from pathlib import Path
from datetime import datetime, timezone

GAMMA = "https://gamma-api.polymarket.com/markets?slug="
CLOB = "https://clob.polymarket.com/book?token_id="
OUT = Path.home() / "ArboCos" / "realpaper" / "clob_pair_samples_v2.jsonl"
USD = 5.0
POLL_SEC = 1.0
UA = {"User-Agent":"arbocos-clob-probe/2.0"}

def get_json(url, timeout=4):
    req = urllib.request.Request(url, headers=UA)
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.load(r)

def current_start():
    now = int(time.time())
    return now - (now % 300)

def parse_jsonish(v):
    if isinstance(v, list):
        return v
    if isinstance(v, str):
        try: return json.loads(v)
        except Exception: return []
    return []

def discover():
    # Try current window, then adjacent windows in case Polymarket pre-creates or rolls late.
    base = current_start()
    for ts in (base, base+300, base-300):
        slug = f"btc-updown-5m-{ts}"
        data = get_json(GAMMA + urllib.parse.quote(slug))
        if isinstance(data, dict):
            data = [data]
        for m in data or []:
            if m.get("slug") != slug:
                continue
            toks = parse_jsonish(m.get("clobTokenIds"))
            outs = parse_jsonish(m.get("outcomes"))
            if len(toks) < 2:
                continue
            # Polymarket uses Up/Down for these markets. Keep mapping explicit.
            mapping = {str(o).strip().lower(): str(t) for o,t in zip(outs,toks)}
            up = mapping.get("up") or str(toks[0])
            down = mapping.get("down") or str(toks[1])
            cond = str(m.get("conditionId") or m.get("condition_id") or "")
            return slug, up, down, cond
    return None

def get_book(token):
    return get_json(CLOB + urllib.parse.quote(token), timeout=3)

def levels(book):
    out=[]
    for a in (book.get("asks",[]) or []):
        try:
            p=float(a.get("price")); s=float(a.get("size"))
            if p>0 and s>0: out.append((p,s))
        except Exception:
            pass
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

def cost_for(book,q):
    rem=q; cost=0.0; worst=None
    for p,s in levels(book):
        take=min(rem,s)
        cost += take*p; rem -= take; worst=p
        if rem<=1e-9: break
    if rem>1e-6: return None
    return cost,worst

def equal_share_cost(ub, db, target_usd):
    u=buy_usd(ub,target_usd); d=buy_usd(db,target_usd)
    if not u or not d: return None
    q=min(u["shares"],d["shares"])
    if q<=0: return None
    uc=cost_for(ub,q); dc=cost_for(db,q)
    if not uc or not dc: return None
    total=uc[0]+dc[0]
    return {"shares":q,"up_cost":uc[0],"down_cost":dc[0],"combined_cost_per_share":total/q,
            "locked_edge_cents":(1-total/q)*100,"up_worst":uc[1],"down_worst":dc[1]}

def main():
    print("CLOB collector v2 running; self-discovering BTC 5m markets; Ctrl-C to stop", flush=True)
    current=None
    while True:
        try:
            found=discover()
            if not found:
                print("market discovery returned no active BTC 5m market", flush=True)
                time.sleep(POLL_SEC); continue
            slug,u,d,c=found
            if slug!=current:
                current=slug
                print("tracking",slug,"condition",c, flush=True)
            ub=get_book(u); db=get_book(d)
            pair=equal_share_cost(ub,db,USD)
            row={"at":datetime.now(timezone.utc).isoformat(),"slug":slug,"up_token_id":u,
                 "down_token_id":d,"condition_id":c,
                 "up_best_ask":levels(ub)[0][0] if levels(ub) else None,
                 "down_best_ask":levels(db)[0][0] if levels(db) else None,
                 "pair_5usd":pair}
            with OUT.open("a") as f:
                f.write(json.dumps(row,separators=(",",":"))+"\n")
            if pair:
                print(row["at"],f'edge={pair["locked_edge_cents"]:.2f}c combined={pair["combined_cost_per_share"]:.4f} shares={pair["shares"]:.2f}', flush=True)
            else:
                print(row["at"],"insufficient depth", flush=True)
        except Exception as e:
            print("collector error:",repr(e), flush=True)
        time.sleep(POLL_SEC)

if __name__=="__main__":
    main()
