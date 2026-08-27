#!/usr/bin/env python3
from pathlib import Path
import json

p = Path.home() / "ArboCos" / "patched8088" / "config.json"
if not p.exists():
    raise SystemExit(f"missing {p}")

c = json.loads(p.read_text())

def put(key, value):
    old = c.get(key)
    if isinstance(old, str):
        if isinstance(value, bool):
            c[key] = "true" if value else "false"
        else:
            c[key] = str(value)
    else:
        c[key] = value

# 8088 only: BTC 5m candidate settings derived from the live CLOB sample.
# Keep paper mode while validating the execution patch.
put("PAPER_TRADE", True)

# 8c was never observed executable in the live sample; 2c was the observed maximum.
# Use 2c as a candidate floor, while the v2 CLOB preflight remains the hard safety gate.
put("PAIR_ARB_MIN_LOCKED_PROFIT_CENTS", 2.0)

# Keep a bounded band, but include the observed 0.68/0.30 executable pair.
put("PAIR_ARB_MIN_TOKEN_PRICE", 0.25)
put("PAIR_ARB_MAX_TOKEN_PRICE", 0.75)

# Use the simultaneous/pre-place path where supported. Do not deliberately shade the hedge
# away from the current executable price; that was increasing second-leg miss risk.
put("PAIR_ARB_DUAL_PREPLACE", True)
put("PAIR_ARB_HEDGE_PRE_OFFSET_CENTS", 0.0)

# Do not add extra lead price slippage on a strategy whose gross edge is only a few cents.
put("PAIR_ARB_LEAD_BUY_SLIP_TICKS", 0)

p.write_text(json.dumps(c, indent=2) + "\n")

print("PATCHED 8088 BTC 5M CONFIG")
for k in [
    "PAPER_TRADE",
    "PAIR_ARB_MIN_LOCKED_PROFIT_CENTS",
    "PAIR_ARB_MIN_TOKEN_PRICE",
    "PAIR_ARB_MAX_TOKEN_PRICE",
    "PAIR_ARB_DUAL_PREPLACE",
    "PAIR_ARB_HEDGE_PRE_OFFSET_CENTS",
    "PAIR_ARB_LEAD_BUY_SLIP_TICKS",
]:
    print(f"{k}={c.get(k)!r}")
