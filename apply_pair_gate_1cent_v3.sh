#!/usr/bin/env bash
set -euo pipefail

BOT_DIR="$HOME/KalshiArbo/kalshiarbo"
cd "$BOT_DIR"

cp config.json "config.json.bak-gate1c-$(date -u +%Y%m%dT%H%M%SZ)"

python3 - <<'PY'
import json
p='config.json'
with open(p) as f: c=json.load(f)
# Keep the locked-profit target at 3c, but allow the opposite leg to be
# up to 2c away at signal time => effective preflight cushion ~1c.
c['PAIR_ARB_MIN_LOCKED_PROFIT_CENTS']=3.0
c['PAIR_ARB_MAX_HEDGE_DISTANCE_CENTS']=2.0
# Safety: PAPER only.
c['PAPER_TRADE']=True
with open(p,'w') as f:
    json.dump(c,f,indent=2)
    f.write('\n')
PY

echo '=== GATE V3 SETTINGS ==='
grep -E '"PAPER_TRADE"|"PAIR_ARB_MIN_LOCKED_PROFIT_CENTS"|"PAIR_ARB_MAX_HEDGE_DISTANCE_CENTS"' config.json

echo
echo 'No restart performed. No live mode enabled.'
