#!/usr/bin/env bash
set -Eeuo pipefail

BOTDIR="$HOME/KalshiArbo/kalshiarbo"
REPO="$HOME/kalshi-diagnostic"
OUT="$REPO/docs/latest.txt"
STAMP="$(date -u +%Y%m%d_%H%M%S)"
TMP="$HOME/strict_arb_test_update_$STAMP.txt"

cd "$BOTDIR"

python3 - <<'PY'
from pathlib import Path
import re

p = Path('strategy/pair_arb_test.go')
s = p.read_text()

# Replace legacy test that expected old lead-entry range behavior.
pat1 = re.compile(r'func TestOnPairArbSignalRejectsLeadWhenExactHedgeBelowMarketMinimum\(t \*testing\.T\) \{.*?\n\}', re.S)
rep1 = r'''func TestOnPairArbSignalRejectsWhenStrictOrderbookUnavailable(t *testing.T) {
	trader := &Trader{
		cfg: TraderConfig{
			PaperTrade:                  true,
			TradeSizeUSD:                5,
			PairArbTradeSizeUSD:         5,
			PairArbMinLockedProfitCents: 8,
		},
		detector:     &Detector{},
		logger:       zap.NewNop(),
		paperBalance: 100,
		feeRateBps:   "0",
	}

	err := trader.OnPairArbSignal(context.Background(), Signal{
		Type:         SignalPairArbLeadYes,
		PolyYesPrice: 0.43,
		PolyNoPrice:  0.48,
	}, "yes-token", "no-token")
	if err == nil {
		t.Fatal("expected strict pair arb to reject when independent orderbook executor is unavailable")
	}
	if !strings.Contains(err.Error(), "order executor unavailable") {
		t.Fatalf("expected strict orderbook-unavailable error, got %v", err)
	}
	if trader.pairedPosition != nil {
		t.Fatal("expected no pair position to be opened")
	}
}'''
s2, n1 = pat1.subn(rep1, s, count=1)
if n1 != 1:
    raise SystemExit(f'ERROR: first legacy test block not found or ambiguous: {n1}')
s = s2

# Replace legacy directional detector test with strict complete-set economics test.
pat2 = re.compile(r'func TestEvaluatePairArbUsesNoLeadWhenGapBelowOpenAndRising\(t \*testing\.T\) \{.*?\n\}', re.S)
rep2 = r'''func TestEvaluatePairArbRequiresPositiveGrossCompleteSetEdge(t *testing.T) {
	detector := NewDetector(Params{
		PairArbEnabled:              true,
		PairArbMinWindowSec:         45,
		PairArbMaxWindowSec:         210,
		PairArbMinTokenPrice:        0.35,
		PairArbMaxTokenPrice:        0.65,
		PairArbMinBTCGapUSD:         5,
		PairArbMaxBTCGapUSD:         180,
		PairArbMinGapVelocityUSD:    0,
		PairArbMinLockedProfitCents: 8,
	}, zap.NewNop())
	now := time.Now()
	detector.SetWindow(80, now.Add(3*time.Minute))
	detector.windowStartedAt = now.Add(-60 * time.Second)
	detector.OnBitstampTrade(50, now.Add(-20*time.Second))
	detector.OnBitstampTrade(70, now)

	// 0.43 + 0.57 = 1.00, so there is no complete-set edge at all.
	detector.OnPolyYesPrice(0.43, now)
	detector.OnPolyNoPrice(0.57, now)
	if sig := detector.EvaluatePairArb(); sig.Type != SignalNone {
		t.Fatalf("expected no signal when YES+NO=1.00, got %v", sig.Type)
	}

	// 0.43 + 0.48 = 0.91, leaving 9 cents gross room. With an 8-cent
	// configured minimum this is eligible for the trader's full depth+fee preflight.
	detector.OnPolyNoPrice(0.48, now)
	sig := detector.EvaluatePairArb()
	if sig.Type == SignalNone {
		t.Fatal("expected strict complete-set candidate when YES+NO=0.91")
	}
}'''
s2, n2 = pat2.subn(rep2, s, count=1)
if n2 != 1:
    raise SystemExit(f'ERROR: second legacy test block not found or ambiguous: {n2}')
s = s2

p.write_text(s)
print('Updated legacy pair-arb tests to strict complete-set semantics.')
PY

gofmt -w strategy/pair_arb_test.go

{
  echo "=================================================="
  echo "KALSHIARBO STRICT-ARB TEST UPDATE"
  echo "UTC: $(date -u --iso-8601=seconds)"
  echo "STAMP: $STAMP"
  echo "=================================================="
  echo
  echo "=== STRICT MARKERS ==="
  grep -nE 'STRICT COMPLETE-SET|strictPairArbPreflight|STRICT executable arbitrage|LIVE pair execution blocked' strategy/signal.go strategy/trader.go || true
  echo
  echo "=== UPDATED STRICT TEST NAMES ==="
  grep -nE 'TestOnPairArbSignalRejectsWhenStrictOrderbookUnavailable|TestEvaluatePairArbRequiresPositiveGrossCompleteSetEdge' strategy/pair_arb_test.go || true
  echo
  echo "=== GO TEST ./... ==="
} > "$TMP"

set +e
go test ./... >> "$TMP" 2>&1
TEST_RC=$?
set -e
echo "GO_TEST_RC=$TEST_RC" >> "$TMP"

echo >> "$TMP"
echo "=== GO BUILD ===" >> "$TMP"
set +e
go build -o kalshiarbo . >> "$TMP" 2>&1
BUILD_RC=$?
set -e
echo "GO_BUILD_RC=$BUILD_RC" >> "$TMP"

echo >> "$TMP"
echo "=== RESULT ===" >> "$TMP"
if [ "$BUILD_RC" -eq 0 ]; then echo "BUILD_STATUS=PASS" >> "$TMP"; else echo "BUILD_STATUS=FAIL" >> "$TMP"; fi
if [ "$TEST_RC" -eq 0 ]; then echo "TEST_STATUS=PASS" >> "$TMP"; else echo "TEST_STATUS=FAIL" >> "$TMP"; fi
echo "END_UTC=$(date -u --iso-8601=seconds)" >> "$TMP"

cd "$REPO"
git pull --rebase >/dev/null 2>&1 || true
mkdir -p docs
cp "$TMP" "$OUT"
git add docs/latest.txt
git commit -m "Strict arb test update $STAMP" >/dev/null 2>&1 || true
git push >/dev/null

echo "PUBLISHED: https://githubcos.github.io/kalshi-diagnostic/latest.txt"
echo "TEST_RC=$TEST_RC BUILD_RC=$BUILD_RC"
