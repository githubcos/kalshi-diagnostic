#!/usr/bin/env bash
set -u

BOTDIR="$HOME/KalshiArbo/kalshiarbo"
REPO="$HOME/kalshi-diagnostic"
OUT="$REPO/docs/latest.txt"
STAMP="$(date -u +%Y%m%d_%H%M%S)"

mkdir -p "$REPO/docs"
exec > >(tee "$OUT") 2>&1

echo "=================================================="
echo "KALSHIARBO STRICT-ARB PATCH TEST"
echo "UTC: $(date -u --iso-8601=seconds)"
echo "STAMP: $STAMP"
echo "=================================================="

cd "$BOTDIR" || exit 1

cp -a strategy/trader.go "strategy/trader.go.pre_nil_guard_$STAMP"

echo
echo "=== APPLY NIL-GUARD FIX ==="
python3 - <<'PY'
from pathlib import Path
p = Path('strategy/trader.go')
s = p.read_text()
needle = 'func (t *Trader) strictPairArbPreflight(ctx context.Context, yesTokenID, noTokenID string, maxSpend float64) (*strictArbPlan, error) {'
if needle not in s:
    raise SystemExit('ERROR: strictPairArbPreflight function not found')
marker = 'strict arb: order executor unavailable'
if marker in s:
    print('Nil guard already present; no change needed.')
else:
    repl = needle + '\n\tif t.orders == nil {\n\t\treturn nil, fmt.Errorf("strict arb: order executor unavailable")\n\t}'
    s = s.replace(needle, repl, 1)
    p.write_text(s)
    print('Nil guard inserted.')
PY
PATCH_RC=$?

gofmt -w strategy/trader.go

echo
echo "=== STRICT PATCH MARKERS ==="
grep -nE 'STRICT COMPLETE-SET|strictPairArbPreflight|STRICT executable arbitrage|LIVE pair execution blocked|continuous-imbalance mode is directional|order executor unavailable' strategy/signal.go strategy/trader.go kalshi/execution_adapter.go 2>/dev/null || true

echo
echo "=== GO TEST ./... ==="
go test ./...
TEST_RC=$?
echo "GO_TEST_RC=$TEST_RC"

echo
echo "=== GO BUILD ==="
go build -o kalshiarbo .
BUILD_RC=$?
echo "GO_BUILD_RC=$BUILD_RC"

echo
echo "=== RESULT ==="
if [ "$PATCH_RC" -eq 0 ] && [ "$BUILD_RC" -eq 0 ]; then
  echo "BUILD_STATUS=PASS"
else
  echo "BUILD_STATUS=FAIL"
fi
if [ "$TEST_RC" -eq 0 ]; then
  echo "TEST_STATUS=PASS"
else
  echo "TEST_STATUS=FAIL"
fi

echo "END_UTC=$(date -u --iso-8601=seconds)"

cd "$REPO" || exit 1
git pull --rebase >/dev/null 2>&1 || true
git add docs/latest.txt
git commit -m "Strict arb patch test $STAMP" >/dev/null 2>&1 || true
git push

echo "PUBLISHED=https://githubcos.github.io/kalshi-diagnostic/latest.txt"

exit 0
