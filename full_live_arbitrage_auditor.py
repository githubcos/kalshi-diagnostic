#!/usr/bin/env python3
import json, math, os, re, sys, time, urllib.request
from datetime import datetime, timezone

LATEST_URL = "https://githubcos.github.io/kalshi-diagnostic/latest.txt"
KALSHI_BASE = "https://external-api.kalshi.com/trade-api/v2"
LOCAL_LOG = os.path.expanduser("~/KalshiArbo/kalshiarbo/polyarb.log")
OUT_JSONL = os.path.expanduser("~/kalshiarbo_full_audit.jsonl")
POLL_SEC = 2.0
DEPTH = 100
GENERAL_TAKER_RATE = 0.07   # official general Kalshi formula coefficient
ANSI = re.compile(r"\x1b\[[0-?]*[ -/]*[@-~]")
TICKER_RE = re.compile(r"\b(KXBTC15M-[A-Z0-9-]+)\b")


def now_iso():
    return datetime.now(timezone.utc).isoformat()


def get_text(url, timeout=5):
    req = urllib.request.Request(url, headers={"User-Agent": "kalshiarbo-independent-auditor/1.0", "Cache-Control": "no-cache"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return r.read().decode("utf-8", "replace")


def get_json(url, timeout=5):
    return json.loads(get_text(url, timeout))


def clean(s):
    return ANSI.sub("", s).replace("\r", "")


def extract_ticker(text):
    text = clean(text)
    # Prefer explicit Slug lines, newest occurrence wins.
    slugs = re.findall(r"Slug\s+([A-Z0-9-]+)", text)
    slugs = [s for s in slugs if s.startswith("KXBTC15M-")]
    if slugs:
        return slugs[-1]
    hits = TICKER_RE.findall(text)
    return hits[-1] if hits else None


def extract_bot_fee_bps(text):
    vals = re.findall(r"Fee Rate\s+([0-9.]+)\s*bps", clean(text), re.I)
    return float(vals[-1]) if vals else None


def recent_bot_events(github_text):
    lines = clean(github_text).splitlines()
    pat = re.compile(r"arb|pair|signal|candidate|enter|entry|reject|skip|fill|filled|hedge|locked|profit|position|orphan|timeout|abort|cooldown|error|warn", re.I)
    out = [ln.strip() for ln in lines if pat.search(ln)]
    return out[-20:]


def recent_local_events():
    if not os.path.exists(LOCAL_LOG):
        return []
    try:
        with open(LOCAL_LOG, "rb") as f:
            f.seek(0, os.SEEK_END)
            size = f.tell()
            f.seek(max(0, size - 120000))
            txt = f.read().decode("utf-8", "replace")
        pat = re.compile(r"arb|pair|signal|candidate|enter|entry|reject|skip|fill|filled|hedge|locked|profit|position|orphan|timeout|abort|cooldown|error|warn", re.I)
        return [ln.strip() for ln in clean(txt).splitlines() if pat.search(ln)][-20:]
    except Exception:
        return []


def parse_levels(orderbook):
    ob = orderbook.get("orderbook_fp") or orderbook.get("orderbook") or {}
    yes_bids = []
    no_bids = []
    for p, q, *rest in ob.get("yes_dollars", []) or []:
        try: yes_bids.append((float(p), float(q)))
        except: pass
    for p, q, *rest in ob.get("no_dollars", []) or []:
        try: no_bids.append((float(p), float(q)))
        except: pass
    yes_bids.sort(reverse=True)  # highest YES bid first
    no_bids.sort(reverse=True)   # highest NO bid first
    # Kalshi orderbook returns bids only. Opposite bid implies executable ask:
    # buy YES ask = 1 - NO bid; buy NO ask = 1 - YES bid.
    yes_asks = sorted([(round(1.0-p, 10), q) for p, q in no_bids], key=lambda x:x[0])
    no_asks = sorted([(round(1.0-p, 10), q) for p, q in yes_bids], key=lambda x:x[0])
    return yes_bids, no_bids, yes_asks, no_asks


def sweep(levels, qty):
    remaining = qty
    cost = 0.0
    pieces = []
    for price, avail in levels:
        take = min(remaining, avail)
        if take > 1e-12:
            cost += take * price
            pieces.append((price, take))
            remaining -= take
        if remaining <= 1e-12:
            break
    if remaining > 1e-9:
        return None
    return cost, pieces


def ceil_cent(x):
    # Official schedule says round up to the next cent.
    return math.ceil((x - 1e-12) * 100.0) / 100.0


def conservative_taker_fee(pieces):
    # Conservative interpretation: apply the published fee formula to each executed price level,
    # then round each component upward to cents. This can overstate fees, never understate them.
    fee = 0.0
    for p, c in pieces:
        raw = GENERAL_TAKER_RATE * c * p * (1.0-p)
        fee += ceil_cent(raw)
    return fee


def fee_if_bps(cost, bps):
    if bps is None:
        return None
    return cost * bps / 10000.0


def candidate_quantities(yes_asks, no_asks):
    maxq = min(sum(q for _,q in yes_asks), sum(q for _,q in no_asks))
    if maxq <= 0: return []
    points = {1.0, maxq}
    cum=0.0
    for _,q in yes_asks:
        cum += q
        if cum <= maxq+1e-9: points.add(cum)
    cum=0.0
    for _,q in no_asks:
        cum += q
        if cum <= maxq+1e-9: points.add(cum)
    # Useful small sizes around the bot's nominal $5 leg size.
    for q in (2,3,4,5,6,8,10,12,15,20,25,50,100):
        if q <= maxq: points.add(float(q))
    return sorted(q for q in points if q > 0 and q <= maxq+1e-9)


def evaluate_qty(qty, yes_asks, no_asks, bot_fee_bps):
    ys = sweep(yes_asks, qty); ns = sweep(no_asks, qty)
    if not ys or not ns: return None
    ycost, ypieces = ys; ncost, npieces = ns
    gross_cost = ycost + ncost
    payout = qty
    gross_profit = payout - gross_cost
    taker_fee = conservative_taker_fee(ypieces) + conservative_taker_fee(npieces)
    net_profit_conservative = gross_profit - taker_fee
    bot_bps_fee = fee_if_bps(gross_cost, bot_fee_bps)
    net_profit_bot_fee = None if bot_bps_fee is None else gross_profit - bot_bps_fee
    return {
        "qty": qty,
        "yes_cost": ycost, "no_cost": ncost, "gross_cost": gross_cost,
        "yes_vwap": ycost/qty, "no_vwap": ncost/qty,
        "payout": payout, "gross_profit": gross_profit,
        "conservative_taker_fee": taker_fee,
        "net_profit_conservative": net_profit_conservative,
        "gross_roi": gross_profit/gross_cost if gross_cost>0 else None,
        "net_roi_conservative": net_profit_conservative/(gross_cost+taker_fee) if gross_cost+taker_fee>0 else None,
        "bot_fee_bps": bot_fee_bps,
        "bot_bps_fee": bot_bps_fee,
        "net_profit_bot_fee": net_profit_bot_fee,
        "yes_pieces": ypieces, "no_pieces": npieces,
    }


def classify(rows):
    if not rows:
        return "NO_TWO_SIDED_LIQUIDITY", None
    best_cons = max(rows, key=lambda r:r["net_profit_conservative"])
    positive_cons = [r for r in rows if r["net_profit_conservative"] > 1e-9]
    positive_gross = [r for r in rows if r["gross_profit"] > 1e-9]
    if positive_cons:
        best = max(positive_cons, key=lambda r:r["net_profit_conservative"])
        return "PROVEN_POSITIVE_ARBITRAGE", best
    if positive_gross:
        best = max(positive_gross, key=lambda r:r["gross_profit"])
        return "GROSS_EDGE_BUT_FEE_DEPENDENT", best
    return "NO_ARBITRAGE", best_cons


def bot_state_from_text(text):
    t = clean(text)
    # Most recent status line wins.
    lines = [ln for ln in t.splitlines() if "YES " in ln and "edge" in ln]
    latest = lines[-1] if lines else ""
    flat = "-- flat --" in latest
    return {"flat": flat, "ticker_line": latest[-350:]}


def compare_bot(classification, bot_state, bot_events):
    entered = any(re.search(r"enter|entry|filled|pair lead|pair dual|buy", e, re.I) for e in bot_events[-8:])
    if classification == "PROVEN_POSITIVE_ARBITRAGE":
        if entered:
            return "BOT_APPEARS_TO_ACT_ON_REAL_ARB"
        if bot_state.get("flat"):
            return "POTENTIAL_MISSED_REAL_ARB_CHECK_TIMESTAMP"
        return "REAL_ARB_EXISTS_BOT_STATE_UNCLEAR"
    if classification in ("NO_ARBITRAGE", "GROSS_EDGE_BUT_FEE_DEPENDENT") and entered:
        return "POTENTIAL_FALSE_POSITIVE_OR_DIRECTIONAL_ENTRY"
    return "NO_CONTRADICTION_OBSERVED"


def fmt_money(x):
    return "n/a" if x is None else f"${x:.4f}"


def render(ticker, market, yb, nb, ya, na, classification, best, botcmp, bot_state, bot_events, age_note):
    os.system("clear" if os.name != "nt" else "cls")
    print("="*88)
    print(" KALSHIARBO INDEPENDENT FULL ARBITRAGE AUDITOR — READ ONLY")
    print("="*88)
    print(f"UTC: {now_iso()}")
    print(f"Ticker: {ticker}   Market status: {market.get('status','?')}   {age_note}")
    print()
    print("ARBITRAGE DEFINITION: guaranteed payout - executable YES cost - executable NO cost - fees > 0")
    print("Market truth source: live Kalshi public market + full orderbook API")
    print("Bot source: GitHub latest.txt mirror + local polyarb.log when present")
    print()
    print("TOP EXECUTABLE BOOK")
    print(f"  YES asks: {ya[:5]}")
    print(f"  NO  asks: {na[:5]}")
    print(f"  YES bids: {yb[:5]}")
    print(f"  NO  bids: {nb[:5]}")
    print()
    print(f"INDEPENDENT VERDICT: {classification}")
    if best:
        print(f"  matched qty              : {best['qty']:.4f}")
        print(f"  YES VWAP / cost          : {best['yes_vwap']:.4f} / {fmt_money(best['yes_cost'])}")
        print(f"  NO  VWAP / cost          : {best['no_vwap']:.4f} / {fmt_money(best['no_cost'])}")
        print(f"  total executable cost    : {fmt_money(best['gross_cost'])}")
        print(f"  guaranteed payout        : {fmt_money(best['payout'])}")
        print(f"  gross locked profit      : {fmt_money(best['gross_profit'])}")
        print(f"  conservative taker fees  : {fmt_money(best['conservative_taker_fee'])}")
        print(f"  NET locked profit        : {fmt_money(best['net_profit_conservative'])}")
        roi = best['net_roi_conservative']
        print(f"  NET ROI                  : {'n/a' if roi is None else f'{roi*100:.4f}%'}")
        if best['bot_fee_bps'] is not None:
            print(f"  bot banner fee rate      : {best['bot_fee_bps']:.4f} bps")
            print(f"  net using bot fee banner : {fmt_money(best['net_profit_bot_fee'])}")
        print(f"  YES sweep                : {best['yes_pieces']}")
        print(f"  NO sweep                 : {best['no_pieces']}")
    print()
    print(f"BOT COMPARISON: {botcmp}")
    if bot_state.get('ticker_line'):
        print("  latest bot ticker:", bot_state['ticker_line'])
    print()
    print("RECENT BOT ARBITRAGE/EXECUTION EVENTS")
    for e in bot_events[-10:]: print("  ", e[:240])
    print()
    print("STRICT LIVE-READINESS RULE:")
    print("  Any bot entry labeled arbitrage with independently computed NET locked profit <= 0 => FAIL")
    print("  Any unmatched leg / stale book / missing executable depth => NOT PROVEN SAFE")
    print("  A directional probability edge is NOT arbitrage.")
    print("="*88)


def main():
    print("Starting independent auditor. Ctrl+C stops only this auditor; it never sends orders.")
    last_ticker = None
    while True:
        ts = time.time()
        try:
            # cache-buster ensures we do not repeatedly receive a stale Pages object.
            gh = get_text(LATEST_URL + f"?t={int(ts*1000)}")
            ticker = extract_ticker(gh)
            if not ticker:
                print(f"[{now_iso()}] Waiting: no KXBTC15M ticker found in latest.txt")
                time.sleep(POLL_SEC); continue
            bot_fee_bps = extract_bot_fee_bps(gh)
            market = get_json(f"{KALSHI_BASE}/markets/{ticker}").get("market", {})
            orderbook = get_json(f"{KALSHI_BASE}/markets/{ticker}/orderbook?depth={DEPTH}")
            yb, nb, ya, na = parse_levels(orderbook)
            rows = [r for q in candidate_quantities(ya, na) if (r := evaluate_qty(q, ya, na, bot_fee_bps))]
            classification, best = classify(rows)
            gh_events = recent_bot_events(gh)
            local_events = recent_local_events()
            bot_events = local_events if local_events else gh_events
            bot_state = bot_state_from_text(gh)
            botcmp = compare_bot(classification, bot_state, bot_events)
            age_note = "GitHub mirror polled now"
            record = {
                "ts": now_iso(), "ticker": ticker, "market": market,
                "yes_bids": yb, "no_bids": nb, "yes_asks": ya, "no_asks": na,
                "classification": classification, "best": best,
                "bot_compare": botcmp, "bot_state": bot_state,
                "recent_bot_events": bot_events[-20:],
            }
            with open(OUT_JSONL, "a", encoding="utf-8") as f:
                f.write(json.dumps(record, separators=(",",":")) + "\n")
            render(ticker, market, yb, nb, ya, na, classification, best, botcmp, bot_state, bot_events, age_note)
            last_ticker = ticker
        except KeyboardInterrupt:
            print(f"\nStopped auditor only. Data saved to {OUT_JSONL}")
            return
        except Exception as e:
            print(f"[{now_iso()}] AUDITOR ERROR: {type(e).__name__}: {e}")
        elapsed = time.time()-ts
        time.sleep(max(0.2, POLL_SEC-elapsed))

if __name__ == "__main__":
    main()
