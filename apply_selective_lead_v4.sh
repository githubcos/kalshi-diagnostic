#!/usr/bin/env bash
set -euo pipefail

BOT_DIR="$HOME/KalshiArbo/kalshiarbo"
cd "$BOT_DIR"

STAMP=$(date -u +%Y%m%dT%H%M%SZ)
cp config.json "config.json.bak-selective-v4-$STAMP"

python3 - <<'PY'
import json
p='config.json'
with open(p) as f:
    c=json.load(f)

# PAPER-ONLY selective lead-and-hedge experiment based on the 110-trade history.
c['PAPER_TRADE'] = True

# Historical hypothesis zone: strongest narrow cluster in the 110-trade classifier.
c['PAIR_ARB_MIN_BTC_GAP_USD'] = 30.0
c['PAIR_ARB_MAX_BTC_GAP_USD'] = 40.0
c['PAIR_ARB_MIN_TOKEN_PRICE'] = 0.70
c['PAIR_ARB_MAX_TOKEN_PRICE'] = 0.80

# Re-enable selective lead-first behavior by disabling the strict immediate-hedge gate.
c['PAIR_ARB_MAX_HEDGE_DISTANCE_CENTS'] = 0.0

# Short exposure window: hedge quickly or flatten.
c['PAIR_ARB_HEDGE_TIMEOUT_SEC'] = 3
c['PAIR_ARB_STOP_LOSS_CENTS'] = 1
c['PAIR_ARB_STOP_LOSS_MIN_HOLD_SEC'] = 1
c['PAIR_ARB_UNPROFITABLE_ABORT_GRACE_SEC'] = 1
c['PAIR_ARB_UNPROFITABLE_ABORT_MIN_GAP_AGAINST_USD'] = 2

# Keep size small for the PAPER experiment.
c['PAIR_ARB_TRADE_SIZE_USD'] = 5
c['TRADE_SIZE_USD'] = 5

with open(p,'w') as f:
    json.dump(c,f,indent=2)
    f.write('\n')
PY

echo '=== SELECTIVE LEAD V4 SETTINGS ==='
grep -E '"PAPER_TRADE"|"PAIR_ARB_MIN_BTC_GAP_USD"|"PAIR_ARB_MAX_BTC_GAP_USD"|"PAIR_ARB_MIN_TOKEN_PRICE"|"PAIR_ARB_MAX_TOKEN_PRICE"|"PAIR_ARB_MAX_HEDGE_DISTANCE_CENTS"|"PAIR_ARB_HEDGE_TIMEOUT_SEC"|"PAIR_ARB_STOP_LOSS_CENTS"|"PAIR_ARB_UNPROFITABLE_ABORT_GRACE_SEC"|"PAIR_ARB_TRADE_SIZE_USD"' config.json

echo
echo "Backup: config.json.bak-selective-v4-$STAMP"
echo 'No restart performed. No live mode enabled.'
