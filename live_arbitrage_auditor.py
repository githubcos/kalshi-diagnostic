#!/usr/bin/env python3
import re, sys, time, urllib.request, urllib.parse
from datetime import datetime, timezone

URL = "https://githubcos.github.io/kalshi-diagnostic/latest.txt"
POLL_SEC = 10
ANSI = re.compile(r"\x1b\[[0-?]*[ -/]*[@-~]")

NUM = r"(-?\d+(?:\.\d+)?)"

def fetch():
    u = URL + "?t=" + str(int(time.time()))
    req = urllib.request.Request(u, headers={"User-Agent":"kalshiarbo-live-auditor/1.0","Cache-Control":"no-cache"})
    with urllib.request.urlopen(req, timeout=10) as r:
        return ANSI.sub("", r.read().decode("utf-8", "replace"))

def f1(pat, text, flags=re.I):
    m = re.search(pat, text, flags)
    return float(m.group(1)) if m else None

def s1(pat, text, flags=re.I):
    m = re.search(pat, text, flags)
    return m.group(1).strip() if m else None

def norm_price(x):
    if x is None: return None
    if x > 1.0 and x <= 100.0: return x/100.0
    return x

def explicit_fee_rate_bps(text):
    v = f1(r"Fee Rate\s+"+NUM+r"\s*bps", text)
    if v is None: v = f1(r"fee[_ ]?rate(?:_bps)?[=: ]+"+NUM, text)
    return v

def classify_block(block, global_fee_bps):
    low = block.lower()
    yes = None; no = None
    yq = None; nq = None
    fees = None

    # Prefer actual fills, then executable asks/prices.
    for pat in [r"yes[_ ]?(?:fill|filled_price|fill_price)[=: ]+"+NUM,
                r"YES[^\n]{0,40}fill(?:ed)?[^0-9]{0,10}"+NUM,
                r"yes[_ ]?(?:ask|price)[=: ]+"+NUM,
                r"YES\s+(?:ask|price)\s*[:=]?\s*"+NUM]:
        yes = f1(pat, block)
        if yes is not None: break
    for pat in [r"no[_ ]?(?:fill|filled_price|fill_price)[=: ]+"+NUM,
                r"NO[^\n]{0,40}fill(?:ed)?[^0-9]{0,10}"+NUM,
                r"no[_ ]?(?:ask|price)[=: ]+"+NUM,
                r"NO\s+(?:ask|price)\s*[:=]?\s*"+NUM]:
        no = f1(pat, block)
        if no is not None: break
    yes, no = norm_price(yes), norm_price(no)

    for pat in [r"yes[_ ]?(?:qty|quantity|shares|size)[=: ]+"+NUM,
                r"YES[^\n]{0,25}(?:shares|qty|size)[=: ]+"+NUM]:
        yq = f1(pat, block)
        if yq is not None: break
    for pat in [r"no[_ ]?(?:qty|quantity|shares|size)[=: ]+"+NUM,
                r"NO[^\n]{0,25}(?:shares|qty|size)[=: ]+"+NUM]:
        nq = f1(pat, block)
        if nq is not None: break

    fees = f1(r"(?:total_)?fees?[=: ]+\$?"+NUM, block)
    bot_edge = f1(r"(?:net[_ ]?edge|locked[_ ]?profit|arb[_ ]?edge)[=: ]+\$?"+NUM, block)
    action = "UNKNOWN"
    if re.search(r"\b(entered|execute|executed|buy|bought|filled|trade open|pair lead|pair hedge)\b", low): action = "ENTER"
    if re.search(r"\b(reject|rejected|skip|skipped|blocked|no trade)\b", low): action = "SKIP"

    if yes is None or no is None:
        return {"status":"INSUFFICIENT_DATA","reason":"Both explicit executable YES and NO prices are not present in this record. Auditor refuses to infer NO as 1-YES.","action":action,"block":block[-1000:]}

    # Per matched contract. If quantities are available, prove total matched economics too.
    qty = min(yq, nq) if yq is not None and nq is not None else 1.0
    depth_proven = yq is not None and nq is not None and qty > 0
    gross_cost = qty*(yes+no)
    payout = qty*1.0

    fee_known = fees is not None or global_fee_bps is not None
    if fees is None:
        if global_fee_bps is not None:
            fees = gross_cost * global_fee_bps / 10000.0
        else:
            fees = 0.0
    net = payout - gross_cost - fees
    roi = (net/gross_cost*100.0) if gross_cost > 0 else 0.0

    if not fee_known:
        status = "UNPROVEN_FEES"
        reason = "YES/NO prices found, but fee burden is not explicit; cannot certify net arbitrage."
    elif not depth_proven:
        status = "PRICE_EDGE_ONLY" if net > 0 else "NO_ARBITRAGE"
        reason = "Positive per-contract price edge, but executable depth/quantity is not proven." if net > 0 else "Combined executable prices plus fees do not produce positive edge."
    elif net > 0:
        status = "VALID_ARBITRAGE"
        reason = "Strictly positive guaranteed net edge at matched executable quantity."
    else:
        status = "NO_ARBITRAGE"
        reason = "Guaranteed payout does not exceed executable acquisition cost plus fees."

    compare = "UNDETERMINED"
    if action == "ENTER":
        compare = "BOT_CORRECT" if status == "VALID_ARBITRAGE" else "BOT_FALSE_POSITIVE"
    elif action == "SKIP":
        compare = "BOT_MISSED_ARBITRAGE" if status == "VALID_ARBITRAGE" else "BOT_CORRECT_REJECTION"

    return {"status":status,"reason":reason,"action":action,"compare":compare,"yes":yes,"no":no,"yq":yq,"nq":nq,"qty":qty,
            "gross_cost":gross_cost,"payout":payout,"fees":fees,"net":net,"roi":roi,"bot_edge":bot_edge,"block":block[-1000:]}

def records(text):
    # Candidate/execution-oriented windows. Avoid treating fast dashboard ticker lines as full arb proofs.
    lines = text.splitlines()
    idx = []
    kw = re.compile(r"arbitrage|pair arb|pair_arb|candidate|locked_profit|locked profit|lead_fill|hedge_fill|filled|rejected|skip", re.I)
    for i,l in enumerate(lines):
        if kw.search(l): idx.append(i)
    out=[]; seen=set()
    for i in idx:
        a=max(0,i-4); b=min(len(lines),i+7)
        block="\n".join(lines[a:b])
        k=re.sub(r"\s+"," ",block)[-500:]
        if k not in seen:
            seen.add(k); out.append(block)
    return out[-50:]

def latest_dashboard(text):
    # Show current state, but never claim it proves an arb if NO is absent.
    ls=[l for l in text.splitlines() if "BTC $" in l and "YES" in l]
    return ls[-1] if ls else None

def draw(text):
    run=s1(r"^RUN=(.+)$", text, re.M)
    elapsed=s1(r"^ELAPSED_MIN=(.+)$", text, re.M)
    process=s1(r"^PROCESS=(.+)$", text, re.M)
    fee_bps=explicit_fee_rate_bps(text)
    rs=records(text)
    results=[classify_block(b, fee_bps) for b in rs]

    valid=sum(r.get("status")=="VALID_ARBITRAGE" for r in results)
    falsep=sum(r.get("compare")=="BOT_FALSE_POSITIVE" for r in results)
    missed=sum(r.get("compare")=="BOT_MISSED_ARBITRAGE" for r in results)
    insufficient=sum(r.get("status") in ("INSUFFICIENT_DATA","UNPROVEN_FEES","PRICE_EDGE_ONLY") for r in results)

    sys.stdout.write("\033[2J\033[H")
    print("KALSHIARBO INDEPENDENT LIVE ARBITRAGE AUDITOR")
    print("="*72)
    print("UTC:", datetime.now(timezone.utc).isoformat(timespec="seconds"))
    print("Source:", URL)
    print("Run:", run or "?", "Elapsed:", elapsed or "?", "Process:", process or "?")
    print("Fee rate seen:", (str(fee_bps)+" bps") if fee_bps is not None else "UNKNOWN")
    print()
    print("STRICT PROOF: net = matched_qty*(1 - YES - NO) - fees")
    print("REAL ARBITRAGE only if net > 0 AND both prices, fees, complementarity, and executable depth are proven.")
    print("NO is NEVER inferred as 1-YES.")
    print()
    dash=latest_dashboard(text)
    if dash:
        print("LIVE BOT TICKER:")
        print(dash[-150:])
        print("Ticker alone is NOT an arbitrage proof unless explicit executable NO data is present.")
        print()
    print(f"Records audited: {len(results)} | Proven valid: {valid} | False positives: {falsep} | Missed: {missed} | Unproven/partial: {insufficient}")
    print("-"*72)

    show=results[-8:]
    if not show:
        print("No candidate/fill records yet. Waiting for arbitrage events...")
    for n,r in enumerate(show,1):
        print(f"[{n}] {r['status']}  bot_action={r.get('action')}  comparison={r.get('compare','UNDETERMINED')}")
        if r.get("yes") is not None:
            print(f"    YES={r['yes']:.4f}  NO={r['no']:.4f}  matched_qty={r['qty']:.4f}")
            print(f"    cost={r['gross_cost']:.6f}  fees={r['fees']:.6f}  payout={r['payout']:.6f}")
            print(f"    NET LOCKED PROFIT={r['net']:+.6f}  ROI={r['roi']:+.3f}%")
            if r.get('bot_edge') is not None: print(f"    bot-reported edge/profit={r['bot_edge']}")
        print("    WHY:", r['reason'])
        print()
    print("Refresh every", POLL_SEC, "seconds. Ctrl+C stops AUDITOR only; KalshiArbo keeps running.")
    sys.stdout.flush()


def main():
    last_err=None
    while True:
        try:
            text=fetch(); draw(text); last_err=None
        except KeyboardInterrupt:
            print("\nAuditor stopped. Bot untouched."); return
        except Exception as e:
            if str(e)!=last_err:
                print("Auditor fetch/parse error:",e,file=sys.stderr); last_err=str(e)
        time.sleep(POLL_SEC)

if __name__ == "__main__": main()
