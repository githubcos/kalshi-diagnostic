package strategy

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestSetMarketTokensClearsPendingPairArbOnConditionChange(t *testing.T) {
	trader := &Trader{
		cfg:             TraderConfig{PaperTrade: true},
		logger:          zap.NewNop(),
		convConditionID: "old-condition",
		pendingPairArb: &pairArbPendingOrderState{
			OrderID:     "0xabc",
			TokenID:     "old-token",
			RequestName: "pair_arb_lead_buy",
			PlacedAt:    time.Now(),
		},
	}

	trader.SetMarketTokens("new-yes", "new-no", "new-condition")

	if trader.pendingPairArb != nil {
		t.Fatal("expected pending pair arb state to be cleared on market rollover")
	}
	if trader.convConditionID != "new-condition" {
		t.Fatalf("expected updated condition id, got %q", trader.convConditionID)
	}
}

func TestMaybeRebalancePairArbBalancesPaperLead(t *testing.T) {
	t.Setenv("KALSHI_TAKER_FEE_RATE", "0.07")
	now := time.Now()
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
		feeRateBps:   "1000",
		pairedPosition: &PairArbPosition{
			YesTokenID:  "yes-token",
			NoTokenID:   "no-token",
			OpenedAt:    now.Add(-20 * time.Second),
			WindowEnd:   now.Add(2 * time.Minute),
			YesShares:   10,
			YesUSDSpent: 4.30,
		},
	}

	if err := trader.maybeRebalancePairArb(context.Background(), 0.54, 0.46); err != nil {
		t.Fatalf("maybeRebalancePairArb returned error: %v", err)
	}
	if !trader.pairedPosition.isBalanced() {
		t.Fatalf("expected pair to be balanced, got yes=%.2f no=%.2f", trader.pairedPosition.YesShares, trader.pairedPosition.NoShares)
	}
	if trader.pairedPosition.BalancedAt.IsZero() {
		t.Fatal("expected BalancedAt to be recorded")
	}
	if side, shares, _ := trader.pairedPosition.residualPosition(); shares > 0.50 {
		t.Fatalf("expected small residual after exact hedge sizing, got side=%s shares=%.4f", side, shares)
	}
}

func TestSettlePairArbPositionSeparatesResidualWinner(t *testing.T) {
	now := time.Now()
	trader := &Trader{
		cfg:      TraderConfig{PaperTrade: true, PairArbTradeSizeUSD: 5, TradeSizeUSD: 5},
		detector: &Detector{},
		logger:   zap.NewNop(),
		pairedPosition: &PairArbPosition{
			OpenedAt:    now.Add(-30 * time.Second),
			WindowEnd:   now.Add(30 * time.Second),
			OpenPrice:   90000,
			YesShares:   10,
			NoShares:    8,
			YesUSDSpent: 4.30,
			NoUSDSpent:  3.84,
		},
	}

	trader.SettleExpiredPosition(context.Background(), true)

	if len(trader.journal) != 2 {
		t.Fatalf("expected 2 journal records, got %d", len(trader.journal))
	}
	matched := trader.journal[0]
	if matched.Side != "PAIR_MATCHED" || matched.Reason != "pair_window_resolved" {
		t.Fatalf("unexpected matched record: side=%q reason=%q", matched.Side, matched.Reason)
	}
	if !almostEqual(matched.Shares, 8) {
		t.Fatalf("expected 8 locked shares, got %.4f", matched.Shares)
	}
	if !almostEqual(matched.USDSpent, 7.28) {
		t.Fatalf("expected matched cost 7.28, got %.4f", matched.USDSpent)
	}
	if !almostEqual(matched.PnL, 0.72) {
		t.Fatalf("expected matched pnl 0.72, got %.4f", matched.PnL)
	}

	residual := trader.journal[1]
	if residual.Side != "PAIR_RESIDUAL_YES" || residual.Reason != "pair_unhedged_resolved" {
		t.Fatalf("unexpected residual record: side=%q reason=%q", residual.Side, residual.Reason)
	}
	if !almostEqual(residual.Shares, 2) {
		t.Fatalf("expected 2 residual shares, got %.4f", residual.Shares)
	}
	if !almostEqual(residual.SellPrice, 1.0) {
		t.Fatalf("expected residual YES winner to settle at 1.0, got %.4f", residual.SellPrice)
	}
	if !almostEqual(residual.PnL, 1.14) {
		t.Fatalf("expected residual pnl 1.14, got %.4f", residual.PnL)
	}
}

func TestSettlePairArbPositionSeparatesResidualLoss(t *testing.T) {
	now := time.Now()
	trader := &Trader{
		cfg:      TraderConfig{PaperTrade: true, PairArbTradeSizeUSD: 5, TradeSizeUSD: 5},
		detector: &Detector{},
		logger:   zap.NewNop(),
		pairedPosition: &PairArbPosition{
			OpenedAt:    now.Add(-30 * time.Second),
			WindowEnd:   now.Add(30 * time.Second),
			OpenPrice:   90000,
			YesShares:   10,
			NoShares:    8,
			YesUSDSpent: 4.30,
			NoUSDSpent:  3.84,
		},
	}

	trader.SettleExpiredPosition(context.Background(), false)

	if len(trader.journal) != 2 {
		t.Fatalf("expected 2 journal records, got %d", len(trader.journal))
	}
	matched := trader.journal[0]
	if !almostEqual(matched.PnL, 0.72) {
		t.Fatalf("expected matched pnl 0.72, got %.4f", matched.PnL)
	}
	residual := trader.journal[1]
	if residual.Side != "PAIR_RESIDUAL_YES" || residual.Reason != "pair_unhedged_resolved" {
		t.Fatalf("unexpected residual record: side=%q reason=%q", residual.Side, residual.Reason)
	}
	if !almostEqual(residual.SellPrice, 0.0) {
		t.Fatalf("expected losing residual YES leg to settle at 0.0, got %.4f", residual.SellPrice)
	}
	if !almostEqual(residual.PnL, -0.86) {
		t.Fatalf("expected residual loss -0.86, got %.4f", residual.PnL)
	}
}

func TestExecutePairLimitBuyRejectsOrderBelowMarketMinimum(t *testing.T) {
	trader := &Trader{
		cfg:          TraderConfig{PaperTrade: true},
		logger:       zap.NewNop(),
		paperBalance: 100,
		feeRateBps:   "0",
	}

	_, _, _, _, _, _, err := trader.executePairLimitBuy(context.Background(), "pair_arb_hedge_buy", "no-token", 0.05, 0.05, 5)
	if err == nil {
		t.Fatal("expected limit buy below $1 minimum to be rejected")
	}
	if !strings.Contains(err.Error(), "below $1.00 minimum") {
		t.Fatalf("expected minimum-notional error, got %v", err)
	}
}

func TestMaybeRebalancePairArbSkipsExactHedgeBelowMarketMinimum(t *testing.T) {
	now := time.Now()
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
		pairedPosition: &PairArbPosition{
			YesTokenID:  "yes-token",
			NoTokenID:   "no-token",
			OpenedAt:    now.Add(-20 * time.Second),
			WindowEnd:   now.Add(2 * time.Minute),
			YesShares:   5,
			YesUSDSpent: 4.75,
		},
	}

	if err := trader.maybeRebalancePairArb(context.Background(), 0.95, 0.05); err != nil {
		t.Fatalf("maybeRebalancePairArb returned error: %v", err)
	}
	if trader.pairedPosition.NoShares != 0 {
		t.Fatalf("expected no hedge fill when exact hedge is below market minimum, got %.2f NO shares", trader.pairedPosition.NoShares)
	}
	if trader.pairedPosition.isBalanced() {
		t.Fatal("expected pair to remain unbalanced")
	}
}

func TestManagePairArbPositionTimesOutUnhedgedLead(t *testing.T) {
	t.Setenv("KALSHI_TAKER_FEE_RATE", "0.07")
	now := time.Now()
	trader := &Trader{
		cfg: TraderConfig{
			PaperTrade:                  true,
			TradeSizeUSD:                5,
			PairArbTradeSizeUSD:         5,
			PairArbMinLockedProfitCents: 8,
			PairArbHedgeTimeoutSec:      5,
		},
		detector:     &Detector{},
		logger:       zap.NewNop(),
		paperBalance: 100,
		feeRateBps:   "0",
		pairedPosition: &PairArbPosition{
			YesTokenID:  "yes-token",
			NoTokenID:   "no-token",
			LeadSide:    "YES",
			OpenedAt:    now.Add(-6 * time.Second),
			WindowEnd:   now.Add(90 * time.Second),
			HedgeBy:     now.Add(-1 * time.Second),
			YesShares:   10,
			YesUSDSpent: 4.30,
		},
	}

	err := trader.managePairArbPosition(context.Background(), 0.20, 0, 0)
	if err != nil {
		t.Fatalf("managePairArbPosition returned error: %v", err)
	}
	if trader.pairedPosition != nil {
		t.Fatal("expected fully unhedged pair position to be closed")
	}
	if len(trader.journal) != 1 {
		t.Fatalf("expected 1 journal record, got %d", len(trader.journal))
	}
	rec := trader.journal[0]
	if rec.Side != "PAIR_RESIDUAL_YES" {
		t.Fatalf("expected PAIR_RESIDUAL_YES, got %q", rec.Side)
	}
	if rec.Reason != "pair_unhedged_timeout" && rec.Reason != "pair_fill_leg_timeout" {
		t.Fatalf("expected unhedged timeout reason, got %q", rec.Reason)
	}
	if !almostEqual(rec.PnL, -2.42) {
		t.Fatalf("expected fee-aware pnl -2.42, got %.4f", rec.PnL)
	}
}

func TestManagePairArbPositionAbortsUnprofitableOnDirectionFlip(t *testing.T) {
	now := time.Now()
	det := &Detector{}
	det.openPrice = 100.0
	det.latestBTCPrice = 99.4 // against YES lead: gap flipped negative

	trader := &Trader{
		cfg: TraderConfig{
			PaperTrade:                               true,
			PairArbMinLockedProfitCents:              8,
			PairArbHedgeTimeoutSec:                   90,
			PairArbUnprofitableAbortGraceSec:         3,
			PairArbUnprofitableAbortMinGapAgainstUSD: 2,
		},
		detector:     det,
		logger:       zap.NewNop(),
		paperBalance: 100,
		feeRateBps:   "0",
		pairedPosition: &PairArbPosition{
			YesTokenID:  "yes-token",
			NoTokenID:   "no-token",
			LeadSide:    "YES",
			OpenedAt:    now.Add(-10 * time.Second),
			WindowEnd:   now.Add(2 * time.Minute),
			HedgeBy:     now.Add(40 * time.Second),
			YesShares:   10,
			YesUSDSpent: 5.20, // avg 0.52
		},
	}

	err := trader.managePairArbPosition(context.Background(), 0.44, 0.56, 0)
	if err != nil {
		t.Fatalf("managePairArbPosition returned error: %v", err)
	}
	if trader.pairedPosition != nil {
		t.Fatal("expected unprofitable unhedged pair to be closed on direction flip")
	}
	if len(trader.journal) != 1 {
		t.Fatalf("expected 1 journal record, got %d", len(trader.journal))
	}
	if trader.journal[0].Reason != "pair_unprofitable_abort" {
		t.Fatalf("expected pair_unprofitable_abort reason, got %q", trader.journal[0].Reason)
	}
}

func TestManagePairArbPositionUnprofitableAbortHonorsMinAdverseGap(t *testing.T) {
	now := time.Now()
	det := &Detector{}
	det.openPrice = 100.0
	det.latestBTCPrice = 101.0           // still YES direction, not adverse
	det.params.PairArbMinBTCGapUSD = 5.0 // makes current +1 gap count as weakened

	trader := &Trader{
		cfg: TraderConfig{
			PaperTrade:                               true,
			PairArbMinLockedProfitCents:              8,
			PairArbHedgeTimeoutSec:                   90,
			PairArbUnprofitableAbortGraceSec:         0,
			PairArbUnprofitableAbortMinGapAgainstUSD: 2,
		},
		detector:     det,
		logger:       zap.NewNop(),
		paperBalance: 100,
		feeRateBps:   "0",
		pairedPosition: &PairArbPosition{
			YesTokenID:  "yes-token",
			NoTokenID:   "no-token",
			LeadSide:    "YES",
			OpenedAt:    now.Add(-15 * time.Second),
			WindowEnd:   now.Add(2 * time.Minute),
			HedgeBy:     now.Add(40 * time.Second),
			YesShares:   10,
			YesUSDSpent: 5.20, // avg 0.52
		},
	}

	err := trader.managePairArbPosition(context.Background(), 0.50, 0.50, 0)
	if err != nil {
		t.Fatalf("managePairArbPosition returned error: %v", err)
	}
	if trader.pairedPosition == nil {
		t.Fatal("expected position to remain open when adverse gap threshold is not met")
	}
}

func TestForceClosePairArbKeepsResidualWhenUnsold(t *testing.T) {
	now := time.Now()
	trader := &Trader{
		cfg:      TraderConfig{PaperTrade: true},
		detector: &Detector{},
		logger:   zap.NewNop(),
		pairedPosition: &PairArbPosition{
			YesTokenID:  "yes-token",
			NoTokenID:   "no-token",
			OpenedAt:    now.Add(-20 * time.Second),
			WindowEnd:   now.Add(2 * time.Minute),
			YesShares:   10.0,
			NoShares:    6.5,
			YesUSDSpent: 4.0,
			NoUSDSpent:  2.6,
		},
	}

	err := trader.forceClosePairArb(context.Background(), 0.42, "pair_fill_leg_timeout")
	if err == nil {
		t.Fatal("expected forceClosePairArb to fail when residual shares remain unsold")
	}
	if !strings.Contains(err.Error(), "remains unsold") {
		t.Fatalf("expected residual-unsold error, got %v", err)
	}
	if trader.pairedPosition == nil {
		t.Fatal("expected pair position to remain open when residual cannot be sold")
	}
	if len(trader.journal) != 0 {
		t.Fatalf("expected no close journal records, got %d", len(trader.journal))
	}
	side, shares, _ := trader.pairedPosition.residualPosition()
	if side != "YES" {
		t.Fatalf("expected YES residual side, got %q", side)
	}
	if !almostEqual(shares, 3.5) {
		t.Fatalf("expected residual shares 3.5, got %.4f", shares)
	}
}

func TestShouldSkipPairArbWalletDeltaAbsorb(t *testing.T) {
	tests := []struct {
		name          string
		tokenID       string
		pendingPre    string
		pendingCredit string
		want          bool
	}{
		{name: "empty token", tokenID: "", pendingPre: "yes", pendingCredit: "no", want: true},
		{name: "matches pending pre hedge", tokenID: "YES_TOKEN", pendingPre: "yes_token", pendingCredit: "", want: true},
		{name: "matches pending hedge credit", tokenID: "no_token", pendingPre: "", pendingCredit: "NO_TOKEN", want: true},
		{name: "no pending match", tokenID: "yes_token", pendingPre: "other", pendingCredit: "different", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldSkipPairArbWalletDeltaAbsorb(tc.tokenID, tc.pendingPre, tc.pendingCredit)
			if got != tc.want {
				t.Fatalf("shouldSkipPairArbWalletDeltaAbsorb(%q, %q, %q) = %v, want %v", tc.tokenID, tc.pendingPre, tc.pendingCredit, got, tc.want)
			}
		})
	}
}

func TestIsPairArbLeadBuyRequestName(t *testing.T) {
	tests := []struct {
		name        string
		requestName string
		want        bool
	}{
		{name: "lead buy", requestName: "pair_arb_lead_buy", want: true},
		{name: "urgent lead rescue", requestName: "pair_arb_lead_buy_urgent_rescue", want: true},
		{name: "hedge buy", requestName: "pair_arb_hedge_buy", want: false},
		{name: "random", requestName: "something_else", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isPairArbLeadBuyRequestName(tc.requestName)
			if got != tc.want {
				t.Fatalf("isPairArbLeadBuyRequestName(%q) = %v, want %v", tc.requestName, got, tc.want)
			}
		})
	}
}

func TestManagePairArbPositionStartsBalancedPairSellAt99(t *testing.T) {
	now := time.Now()
	trader := &Trader{
		cfg: TraderConfig{
			PaperTrade:      true,
			PairArbSellAt99: true,
		},
		detector:     &Detector{},
		logger:       zap.NewNop(),
		paperBalance: 100,
		feeRateBps:   "1000",
		pairedPosition: &PairArbPosition{
			ConditionID: "cond-1",
			YesTokenID:  "yes-token",
			NoTokenID:   "no-token",
			OpenedAt:    now.Add(-20 * time.Second),
			WindowEnd:   now.Add(2 * time.Minute),
			YesShares:   10,
			NoShares:    10,
			YesUSDSpent: 4.30,
			NoUSDSpent:  4.80,
		},
	}

	err := trader.managePairArbPosition(context.Background(), 0.99, 0.01, 0)
	if err != nil {
		t.Fatalf("managePairArbPosition returned error: %v", err)
	}
	if trader.pairedPosition != nil {
		t.Fatal("expected balanced pair to be closed at 99")
	}
	if len(trader.journal) != 1 {
		t.Fatalf("expected 1 journal record, got %d", len(trader.journal))
	}
	if trader.journal[0].Reason != "pair_sell_at_99" {
		t.Fatalf("expected pair_sell_at_99 reason, got %q", trader.journal[0].Reason)
	}
	if trader.journal[0].Side != "PAIR_MATCHED" {
		t.Fatalf("expected PAIR_MATCHED exit record, got %q", trader.journal[0].Side)
	}
}

func almostEqual(left, right float64) bool {
	return math.Abs(left-right) < 0.0001
}

func TestEvaluatePairArbPreservesOriginalLeadThenHedgeSignal(t *testing.T) {
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

	// Original PolyArbPro behavior: the BTC/oracle lag may trigger a lead even
	// when YES+NO is exactly 1.00. Profit is not locked until the later hedge.
	detector.OnPolyYesPrice(0.43, now)
	detector.OnPolyNoPrice(0.57, now)
	sig := detector.EvaluatePairArb()
	if sig.Type == SignalNone {
		t.Fatal("expected lead-then-hedge signal when directional lag gates are satisfied")
	}

	// A cheaper opposite leg remains eligible; execution code decides whether
	// the fee-aware hedge can ultimately lock the configured profit.
	detector.OnPolyNoPrice(0.48, now)
	if sig := detector.EvaluatePairArb(); sig.Type == SignalNone {
		t.Fatal("expected lead candidate with cheaper opposite leg")
	}
}

func TestEvaluatePairArbRequiresActualOppositeLegPrice(t *testing.T) {
	detector := NewDetector(Params{
		PairArbEnabled:              true,
		PairArbMinWindowSec:         45,
		PairArbMaxWindowSec:         210,
		PairArbMinTokenPrice:        0.35,
		PairArbMaxTokenPrice:        0.65,
		PairArbMinBTCGapUSD:         5,
		PairArbMaxBTCGapUSD:         180,
		PairArbMinGapVelocityUSD:    0.5,
		PairArbMinLockedProfitCents: 8,
	}, zap.NewNop())
	now := time.Now()
	detector.SetWindow(80, now.Add(3*time.Minute))
	detector.windowStartedAt = now.Add(-60 * time.Second)
	detector.OnBitstampTrade(50, now.Add(-20*time.Second))
	detector.OnBitstampTrade(70, now)
	detector.OnPolyYesPrice(0.43, now)

	sig := detector.EvaluatePairArb()
	if sig.Type != SignalNone {
		t.Fatalf("expected SignalNone without actual opposite-leg price, got %v", sig.Type)
	}
}

func TestEvaluatePairArbRespectsConfigurableElapsedWindow(t *testing.T) {
	detector := NewDetector(Params{
		PairArbEnabled:              true,
		PairArbMinWindowSec:         10,
		PairArbMaxWindowSec:         30,
		PairArbMinTokenPrice:        0.35,
		PairArbMaxTokenPrice:        0.65,
		PairArbMinBTCGapUSD:         5,
		PairArbMaxBTCGapUSD:         180,
		PairArbMinGapVelocityUSD:    0.5,
		PairArbMinLockedProfitCents: 8,
	}, zap.NewNop())
	now := time.Now()
	detector.SetWindow(80, now.Add(4*time.Minute))
	// Simulate 31 seconds elapsed since the window started; this is above
	// PairArbMaxWindowSec=30 and must be blocked by config (no hardcoded cutoff).
	detector.windowStartedAt = now.Add(-31 * time.Second)
	detector.OnBitstampTrade(50, now.Add(-20*time.Second))
	detector.OnBitstampTrade(70, now)
	detector.OnPolyYesPrice(0.43, now)
	detector.OnPolyNoPrice(0.57, now)

	sig := detector.EvaluatePairArb()
	if sig.Type != SignalNone {
		t.Fatalf("expected SignalNone when elapsed exceeds configured max window sec, got %v", sig.Type)
	}
}

func TestEvaluatePairArbBlocksAdverseYesDrift(t *testing.T) {
	detector := NewDetector(Params{
		PairArbEnabled:                 true,
		PairArbMinWindowSec:            45,
		PairArbMaxWindowSec:            210,
		PairArbMinTokenPrice:           0.35,
		PairArbMaxTokenPrice:           0.65,
		PairArbMinBTCGapUSD:            5,
		PairArbMaxBTCGapUSD:            180,
		PairArbMinGapVelocityUSD:       0.5,
		PairArbMinLockedProfitCents:    8,
		PairArbMaxAdverseYesDriftCents: 3.0,
		PairArbYesDriftWindowSec:       10,
	}, zap.NewNop())
	now := time.Now()
	detector.SetWindow(80, now.Add(3*time.Minute))
	detector.windowStartedAt = now.Add(-60 * time.Second)
	detector.OnBitstampTrade(50, now.Add(-20*time.Second))
	detector.OnBitstampTrade(70, now)
	detector.OnPolyPriceSample(0.47, now.Add(-5*time.Second))
	detector.OnPolyPriceSample(0.41, now)
	detector.OnPolyYesPrice(0.41, now)
	detector.OnPolyNoPrice(0.59, now)

	sig := detector.EvaluatePairArb()
	if sig.Type != SignalNone {
		t.Fatalf("expected SignalNone when YES drift is adverse for YES lead, got %v", sig.Type)
	}
}

func TestEvaluatePairArbBlocksCoinbaseTakerImbalance(t *testing.T) {
	detector := NewDetector(Params{
		PairArbEnabled:                   true,
		PairArbMinWindowSec:              45,
		PairArbMaxWindowSec:              210,
		PairArbMinTokenPrice:             0.35,
		PairArbMaxTokenPrice:             0.65,
		PairArbMinBTCGapUSD:              5,
		PairArbMaxBTCGapUSD:              180,
		PairArbMinGapVelocityUSD:         0.5,
		PairArbMinLockedProfitCents:      8,
		PairArbMinCoinbaseTakerImbalance: 0.20,
	}, zap.NewNop())
	now := time.Now()
	detector.SetWindow(80, now.Add(3*time.Minute))
	detector.windowStartedAt = now.Add(-60 * time.Second)
	detector.OnBitstampTrade(50, now.Add(-20*time.Second))
	detector.OnBitstampTrade(70, now)
	detector.OnPolyYesPrice(0.43, now)
	detector.OnPolyNoPrice(0.57, now)
	// YES lead setup (gap<0) but Coinbase taker flow is net selling.
	detector.OnCoinbaseTrade(70, 2.0, true)
	detector.OnCoinbaseTrade(70, 0.5, false)

	sig := detector.EvaluatePairArb()
	if sig.Type != SignalNone {
		t.Fatalf("expected SignalNone when Coinbase taker imbalance is adverse for YES lead, got %v", sig.Type)
	}
}

func TestEvaluatePairArbBlocksContraOIDelta(t *testing.T) {
	detector := NewDetector(Params{
		PairArbEnabled:              true,
		PairArbMinWindowSec:         45,
		PairArbMaxWindowSec:         210,
		PairArbMinTokenPrice:        0.35,
		PairArbMaxTokenPrice:        0.65,
		PairArbMinBTCGapUSD:         5,
		PairArbMaxBTCGapUSD:         180,
		PairArbMinGapVelocityUSD:    0.5,
		PairArbMinLockedProfitCents: 8,
		PairArbMinOIDeltaUSD:        100000,
		PairArbMaxContraOIDeltaUSD:  200000,
	}, zap.NewNop())
	now := time.Now()
	detector.SetWindow(80, now.Add(3*time.Minute))
	detector.windowStartedAt = now.Add(-60 * time.Second)
	detector.OnBitstampTrade(50, now.Add(-20*time.Second))
	detector.OnBitstampTrade(70, now)
	detector.OnPolyYesPrice(0.43, now)
	detector.OnPolyNoPrice(0.57, now)
	// Build a strongly negative OI delta which is contra for YES lead (gap<0).
	detector.OnDeribitStream(0, 0, 0, 100_000_000)
	detector.OnDeribitStream(0, 0, 0, 99_500_000)

	sig := detector.EvaluatePairArb()
	if sig.Type != SignalNone {
		t.Fatalf("expected SignalNone when OI delta is contra for YES lead, got %v", sig.Type)
	}
}
