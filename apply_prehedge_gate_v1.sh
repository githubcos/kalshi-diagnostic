#!/usr/bin/env bash
set -euo pipefail
BOTDIR="$HOME/KalshiArbo/kalshiarbo"
PATCH="$HOME/kalshi-termux-publisher/control/patches/kalshi_restore_prehedge_gate_v1.patch"
STAMP="$(date -u +%Y%m%d_%H%M%S)"

cd "$BOTDIR"
cp -a strategy/signal.go "strategy/signal.go.pre_prehedge_$STAMP"
cp -a config.json "config.json.pre_prehedge_$STAMP" 2>/dev/null || true

# Apply only once.
if grep -q 'Kalshi safety gate: do not open a directional first leg' strategy/signal.go; then
  echo 'PREHEDGE_PATCH_ALREADY_PRESENT=true'
else
  git apply --check "$PATCH"
  git apply "$PATCH"
fi

gofmt -w strategy/signal.go

# Keep PAPER mode. Configure a 2-cent effective pre-entry cushion by using the
# existing 8-cent locked target with a 6-cent maximum hedge-distance allowance.
# If keys already exist they are replaced; if absent they are added.
python3 - <<'PY'
import json
p='config.json'
with open(p) as f:d=json.load(f)
d['PAPER_TRADE']=True
d['PAIR_ARB_MIN_LOCKED_PROFIT_CENTS']=8
d['PAIR_ARB_MAX_HEDGE_DISTANCE_CENTS']=6
with open(p,'w') as f:json.dump(d,f,indent=2);f.write('\n')
PY

echo '=== PATCH MARKER ==='
grep -n -A14 -B4 'Kalshi safety gate' strategy/signal.go

echo '=== SAFETY CONFIG ==='
grep -E '"PAPER_TRADE"|"PAIR_ARB_MIN_LOCKED_PROFIT_CENTS"|"PAIR_ARB_MAX_HEDGE_DISTANCE_CENTS"' config.json || true

echo '=== TEST ==='
go test ./... -count=1

echo '=== BUILD ==='
go build -o kalshiarbo .

echo 'PATCH_BUILD_STATUS=PASS'
echo "BACKUP=strategy/signal.go.pre_prehedge_$STAMP"
