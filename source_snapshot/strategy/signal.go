// Package strategy implements the core trading logic for the polyarb bot.
//
// ── STRATEGY STATUS ────────────────────────────────────────────────────────
//
// Strategy availability is runtime-configured. Reversal snipe remains the main
// documented live path, but this package also contains optional detector logic
// for conviction, resolution, pair-arb, scalp, and other experiments.
//
// Why: A $5 entry at 0.25¢/share = 20 shares. When the token resolves at $0.99
// that is $19.80 returned on a $5 bet — nearly 4× the stake. No other strategy
// in this codebase offers that risk/reward profile.
//
// How it works:
//
//   - Window: fires only in the FINAL 10–40 seconds of a 5-minute BTC market.
//   - Token is still CHEAP (0.25–0.45) because the market hasn't priced the move.
//   - Two independent microstructure signals must BOTH confirm the flip direction:
//     1. Coinbase spot price has already moved >$8 against the BTC open price
//     (our faster feed is leading the slow Chainlink oracle).
//     2. Binance 30-second rolling CVD (net buyer-aggressor BTC) exceeds ±15 BTC
//     (institutional smart-money is already positioned in that direction).
//   - Entry is taken at market limit; position holds to window resolution.
//
// ── OPTIONAL STRATEGIES ─────────────────────────────────────────────────────
//
// The following code paths exist behind config flags and should only be enabled
// deliberately after validation:
//
//   - Conviction (SignalConvictionBuyYes/No) — mid-window entry, token 55–78%,
//     high win-prob vs BTC open. Controlled by CONVICTION_ENABLED (default false).
//     Reason disabled: requires active BTC-reversal management; whipsaw losses
//     dominate when the exit guards (MinHoldSec, MinCrossUSD) are not perfectly tuned.
//
//   - Resolution snipe (SignalResolutionBuyYes/No) — last 8–60 s, token already
//     at 0.93–0.97. Controlled by RESOLUTION_ENABLED (default false).
//     Reason disabled by default: token is already expensive, so this only makes
//     sense when the user explicitly wants late winner-following instead of reversal capture.
//
//   - Lag / flash strategies (SignalBuy, SignalBuyNo, SignalFlash*) — original
//     Chainlink-latency arb. These fire only when MinChainlinkLagUSD > 0, which
//     is not set in the active config.
package strategy

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// SignalType is a trading decision.
type SignalType int

const (
	SignalNone                  SignalType = iota
	SignalBuy                              // Enter a long position on YES (lag strategy)
	SignalBuyNo                            // Enter a long position on NO  (BTC < open)
	SignalFlashBuyYes                      // YES dropped fast → buy YES (flash snap-back up)
	SignalFlashBuyNo                       // YES spiked fast → buy NO  (flash snap-back down)
	SignalResolutionBuyYes                 // Token ≥0.93 in final ~30s, BTC above open → buy YES
	SignalResolutionBuyNo                  // Token ≥0.93 in final ~30s, BTC below open → buy NO
	SignalResolutionEarlyBuyYes            // Token 0.85–0.93, 61–120s remaining, BTC above open → early-tier YES
	SignalResolutionEarlyBuyNo             // Token 0.85–0.93, 61–120s remaining, BTC below open → early-tier NO
	SignalConvictionBuyYes                 // Mid-window conviction: 60-90s left, token 60-70, high win-prob → buy YES
	SignalConvictionBuyNo                  // Mid-window conviction: 60-90s left, token 60-70, high win-prob → buy NO
	SignalPairArbLeadYes                   // Paired arb: BTC is above open (YES favored) → buy YES lead leg while cheap, hedge NO after
	SignalPairArbLeadNo                    // Paired arb: BTC is below open (NO favored) → buy NO lead leg while cheap, hedge YES after
	SignalPairArbReverseLeadYes            // Paired arb reverse: BTC below open but gap is shrinking/reversing → buy YES lead first
	SignalPairArbReverseLeadNo             // Paired arb reverse: BTC above open but gap is shrinking/reversing → buy NO lead first
	SignalPairArbPreOpenYes                // Pre-market-open carry: prev window closed YES-biased; enter YES in first few seconds of next window
	SignalPairArbPreOpenNo                 // Pre-market-open carry: prev window closed NO-biased; enter NO in first few seconds of next window
	SignalPairArbTruePreOpenYes            // True pre-open: discover next market early, buy YES on next window BEFORE current window ends
	SignalPairArbTruePreOpenNo             // True pre-open: discover next market early, buy NO on next window BEFORE current window ends
	SignalPairArbCVDMomentumYes            // CVD momentum: early-window low-gap, CVD>0 → buy YES lead leg
	SignalPairArbCVDMomentumNo             // CVD momentum: early-window low-gap, CVD<0 → buy NO lead leg
	SignalReversalSnipeYes                 // Last 10-40s: coinbase_spread > +threshold AND cvd > +threshold → buy YES (reversal flip up)
	SignalReversalSnipeNo                  // Last 10-40s: coinbase_spread < -threshold AND cvd < -threshold → buy NO  (reversal flip down)
	SignalCollapseSnipeYes                 // Last 4-9s: near-settled NO (≤6¢), overwhelming Coinbase/CVD crush → BTC flips up
	SignalCollapseSnipeNo                  // Last 4-9s: near-settled YES (≤6¢), overwhelming Coinbase/CVD crash → BTC flips down
	SignalDeepDiscountFadeYes              // 40-120s: YES 2-5¢ while BTC below open → contrarian fade (rebound)
	SignalDeepDiscountFadeNo               // 40-120s: NO 2-5¢ while BTC above open → contrarian fade (revert)
	SignalMidFlipYes                       // 40-120s: cheap side, gap shrinking fast → buy YES
	SignalMidFlipNo                        // 40-120s: cheap side, gap shrinking fast → buy NO
	SignalLateFlipYes                      // 15-30s: strong Coinbase + win_prob, no CVD → buy YES
	SignalLateFlipNo                       // 15-30s: strong Coinbase + win_prob, no CVD → buy NO
	SignalScalpYes                         // Mid-window: win-prob + gap + 55-75 token → quick scalp YES
	SignalScalpNo                          // Mid-window: win-prob + gap + 55-75 token → quick scalp NO
	SignalPennyBuyYes                      // 90+s remaining: YES at $0.01 → buy (floor snap-back; asymmetric payoff)
	SignalPennyBuyNo                       // 90+s remaining: NO at $0.01 (YES=0.99) → buy (floor snap-back)
	SignalDCAHedgeYes                      // DCA+hedge: token-price-drift entry, moved side is YES → buy both legs simultaneously
	SignalDCAHedgeNo                       // DCA+hedge: token-price-drift entry, moved side is NO → buy both legs simultaneously
)

func (t SignalType) String() string {
	switch t {
	case SignalBuy:
		return "BUY_YES"
	case SignalBuyNo:
		return "BUY_NO"
	case SignalFlashBuyYes:
		return "FLASH_BUY_YES"
	case SignalFlashBuyNo:
		return "FLASH_BUY_NO"
	case SignalResolutionBuyYes:
		return "RESOLUTION_BUY_YES"
	case SignalResolutionBuyNo:
		return "RESOLUTION_BUY_NO"
	case SignalResolutionEarlyBuyYes:
		return "RESOLUTION_EARLY_BUY_YES"
	case SignalResolutionEarlyBuyNo:
		return "RESOLUTION_EARLY_BUY_NO"
	case SignalConvictionBuyYes:
		return "CONVICTION_BUY_YES"
	case SignalConvictionBuyNo:
		return "CONVICTION_BUY_NO"
	case SignalPairArbLeadYes:
		return "PAIR_ARB_LEAD_YES"
	case SignalPairArbLeadNo:
		return "PAIR_ARB_LEAD_NO"
	case SignalPairArbReverseLeadYes:
		return "PAIR_ARB_REVERSE_LEAD_YES"
	case SignalPairArbReverseLeadNo:
		return "PAIR_ARB_REVERSE_LEAD_NO"
	case SignalPairArbPreOpenYes:
		return "PAIR_ARB_PRE_OPEN_YES"
	case SignalPairArbPreOpenNo:
		return "PAIR_ARB_PRE_OPEN_NO"
	case SignalPairArbTruePreOpenYes:
		return "PAIR_ARB_TRUE_PRE_OPEN_YES"
	case SignalPairArbTruePreOpenNo:
		return "PAIR_ARB_TRUE_PRE_OPEN_NO"
	case SignalReversalSnipeYes:
		return "REVERSAL_SNIPE_YES"
	case SignalReversalSnipeNo:
		return "REVERSAL_SNIPE_NO"
	case SignalCollapseSnipeYes:
		return "COLLAPSE_SNIPE_YES"
	case SignalCollapseSnipeNo:
		return "COLLAPSE_SNIPE_NO"
	case SignalDeepDiscountFadeYes:
		return "DEEP_DISCOUNT_FADE_YES"
	case SignalDeepDiscountFadeNo:
		return "DEEP_DISCOUNT_FADE_NO"
	case SignalMidFlipYes:
		return "MID_FLIP_YES"
	case SignalMidFlipNo:
		return "MID_FLIP_NO"
	case SignalLateFlipYes:
		return "LATE_FLIP_YES"
	case SignalLateFlipNo:
		return "LATE_FLIP_NO"
	case SignalScalpYes:
		return "SCALP_YES"
	case SignalScalpNo:
		return "SCALP_NO"
	case SignalPennyBuyYes:
		return "PENNY_BUY_YES"
	case SignalPennyBuyNo:
		return "PENNY_BUY_NO"
	case SignalDCAHedgeYes:
		return "DCA_HEDGE_YES"
	case SignalDCAHedgeNo:
		return "DCA_HEDGE_NO"
	default:
		return "NONE"
	}
}

// Signal carries a trading decision with supporting context.
type Signal struct {
	Type SignalType

	// Price context at signal time
	BitstampPrice   float64 // live BTC/USD from Bitstamp
	BitstampRisePct float64 // % rise over observation window
	ChainlinkPrice  float64 // latest Chainlink BTC/USD
	ChainlinkLag    float64 // bitstampPrice - chainlinkPrice ($)
	OpenPrice       float64 // "price to beat" for YES to win
	PolyYesPrice    float64 // current YES token price on Polymarket
	PolyNoPrice     float64 // current NO token price (1 - YES)
	WindowRemaining float64 // seconds remaining in the 5-min market window
	WinProb         float64 // Gaussian win probability at signal time [0..1]

	// IsOrderFlowLed: true when this conviction signal fired from the CVD+book-imbalance
	// early-entry path rather than the standard BTC-gap path. Logged in trades.jsonl.
	IsOrderFlowLed bool
	// OverrideConditionID, if non-empty, is used as pairedPosition.ConditionID instead
	// of the trader's current convConditionID. Set for true pre-open signals where the
	// order is placed on the NEXT window's market before the current window ends.
	OverrideConditionID string
	// OverrideWindowEnd, if non-zero, is used as pairedPosition.WindowEnd instead of
	// detector.WindowEnd(). Set for true pre-open signals so the position is not
	// prematurely settled when the current window ends.
	OverrideWindowEnd time.Time

	// DCA+Hedge entry fields (set when Type is SignalDCAHedgeYes/No)
	DCAHedgeMovedSide    string  // "YES" or "NO" — which side moved up >= MoveTrigger
	DCAHedgeMovedShares  float64 // shares to buy of the moved side
	DCAHedgeOppShares    float64 // shares to buy of the opposite side
	DCAHedgeTriggerPrice float64 // moved-side price at the moment the trigger fired

	// Flash-reversal fields (set when Type is SignalFlashBuyYes/No)
	FlashOldestYes     float64 // YES price at time of peak spread (before manipulation)
	FlashTargetYes     float64 // expected YES price if manipulation completes
	FlashMovePct       float64 // spread collapse % (for compat)
	FlashSpreadPeakUSD float64 // peak BTC-vs-open spread ($)
	FlashSpreadNowUSD  float64 // current spread at signal time ($)
	FlashCollapsePct   float64 // fraction of peak spread consumed (0 → 1+)
	FlashBookConfirmed bool    // orderbook collapse detected (wall pull confirmed)

	At time.Time
}

func (s Signal) String() string {
	switch s.Type {
	case SignalBuy:
		return fmt.Sprintf(
			"BUY_YES [stamp=%.2f cl=%.2f open=%.2f lag=+$%.2f rise=+%.3f%% yes=%.4f win_rem=%.0fs]",
			s.BitstampPrice, s.ChainlinkPrice, s.OpenPrice,
			s.ChainlinkLag, s.BitstampRisePct, s.PolyYesPrice, s.WindowRemaining,
		)
	case SignalBuyNo:
		return fmt.Sprintf(
			"BUY_NO  [stamp=%.2f cl=%.2f open=%.2f lag=$%.2f no=%.4f win_rem=%.0fs]",
			s.BitstampPrice, s.ChainlinkPrice, s.OpenPrice,
			s.OpenPrice-s.BitstampPrice, s.PolyNoPrice, s.WindowRemaining,
		)
	case SignalFlashBuyYes:
		return fmt.Sprintf(
			"FLASH_BUY_YES [spread_peak=$%.1f now=$%.1f collapse=%.0f%% book=%v btc=%.2f open=%.2f yes=%.4f target_yes=%.4f rem=%.0fs]",
			s.FlashSpreadPeakUSD, s.FlashSpreadNowUSD, s.FlashCollapsePct*100,
			s.FlashBookConfirmed, s.BitstampPrice, s.OpenPrice, s.PolyYesPrice, s.FlashTargetYes, s.WindowRemaining,
		)
	case SignalFlashBuyNo:
		return fmt.Sprintf(
			"FLASH_BUY_NO  [spread_peak=$%.1f now=$%.1f collapse=%.0f%% book=%v btc=%.2f open=%.2f yes=%.4f target_yes=%.4f rem=%.0fs]",
			s.FlashSpreadPeakUSD, s.FlashSpreadNowUSD, s.FlashCollapsePct*100,
			s.FlashBookConfirmed, s.BitstampPrice, s.OpenPrice, s.PolyYesPrice, s.FlashTargetYes, s.WindowRemaining,
		)
	case SignalReversalSnipeNo:
		return fmt.Sprintf(
			"[btc=%.2f open=%.2f gap=$%.1f no=%.4f win_prob=%.2f rem=%.0fs]",
			s.BitstampPrice, s.OpenPrice, s.BitstampPrice-s.OpenPrice,
			s.PolyNoPrice, s.WinProb, s.WindowRemaining,
		)
	case SignalPairArbLeadYes:
		return fmt.Sprintf(
			"PAIR_LEAD_YES [btc=%.2f open=%.2f gap=$%.1f yes=%.4f prob=%.2f rem=%.0fs]",
			s.BitstampPrice, s.OpenPrice, s.BitstampPrice-s.OpenPrice,
			s.PolyYesPrice, s.WinProb, s.WindowRemaining,
		)
	case SignalPairArbLeadNo:
		return fmt.Sprintf(
			"PAIR_LEAD_NO [btc=%.2f open=%.2f gap=$%.1f no=%.4f prob=%.2f rem=%.0fs]",
			s.BitstampPrice, s.OpenPrice, s.BitstampPrice-s.OpenPrice,
			s.PolyNoPrice, s.WinProb, s.WindowRemaining,
		)
	case SignalPairArbReverseLeadYes:
		return fmt.Sprintf(
			"PAIR_REVERSE_LEAD_YES [btc=%.2f open=%.2f gap=$%.1f yes=%.4f prob=%.2f rem=%.0fs]",
			s.BitstampPrice, s.OpenPrice, s.BitstampPrice-s.OpenPrice,
			s.PolyYesPrice, s.WinProb, s.WindowRemaining,
		)
	case SignalPairArbReverseLeadNo:
		return fmt.Sprintf(
			"PAIR_REVERSE_LEAD_NO [btc=%.2f open=%.2f gap=$%.1f no=%.4f prob=%.2f rem=%.0fs]",
			s.BitstampPrice, s.OpenPrice, s.BitstampPrice-s.OpenPrice,
			s.PolyNoPrice, s.WinProb, s.WindowRemaining,
		)
	case SignalReversalSnipeYes:
		return fmt.Sprintf(
			"[btc=%.2f open=%.2f gap=$%.1f yes=%.4f win_prob=%.2f rem=%.0fs]",
			s.BitstampPrice, s.OpenPrice, s.BitstampPrice-s.OpenPrice,
			s.PolyYesPrice, s.WinProb, s.WindowRemaining,
		)
	case SignalCollapseSnipeNo:
		return fmt.Sprintf(
			"COLLAPSE [btc=%.2f open=%.2f gap=$%.1f no=%.4f rem=%.0fs]",
			s.BitstampPrice, s.OpenPrice, s.BitstampPrice-s.OpenPrice,
			s.PolyNoPrice, s.WindowRemaining,
		)
	case SignalCollapseSnipeYes:
		return fmt.Sprintf(
			"COLLAPSE [btc=%.2f open=%.2f gap=$%.1f yes=%.4f rem=%.0fs]",
			s.BitstampPrice, s.OpenPrice, s.BitstampPrice-s.OpenPrice,
			s.PolyYesPrice, s.WindowRemaining,
		)
	case SignalScalpYes:
		return fmt.Sprintf(
			"SCALP_YES [btc=%.2f open=%.2f gap=$%.1f yes=%.4f prob=%.2f rem=%.0fs]",
			s.BitstampPrice, s.OpenPrice, s.BitstampPrice-s.OpenPrice,
			s.PolyYesPrice, s.WinProb, s.WindowRemaining,
		)
	case SignalScalpNo:
		return fmt.Sprintf(
			"SCALP_NO [btc=%.2f open=%.2f gap=$%.1f no=%.4f prob=%.2f rem=%.0fs]",
			s.BitstampPrice, s.OpenPrice, s.BitstampPrice-s.OpenPrice,
			s.PolyNoPrice, s.WinProb, s.WindowRemaining,
		)
	case SignalResolutionBuyYes:
		return fmt.Sprintf(
			"RESOLUTION_YES [btc=%.2f open=%.2f gap=$%.1f yes=%.4f prob=%.2f rem=%.0fs]",
			s.BitstampPrice, s.OpenPrice, s.BitstampPrice-s.OpenPrice,
			s.PolyYesPrice, s.WinProb, s.WindowRemaining,
		)
	case SignalResolutionBuyNo:
		return fmt.Sprintf(
			"RESOLUTION_NO [btc=%.2f open=%.2f gap=$%.1f no=%.4f prob=%.2f rem=%.0fs]",
			s.BitstampPrice, s.OpenPrice, s.BitstampPrice-s.OpenPrice,
			s.PolyNoPrice, s.WinProb, s.WindowRemaining,
		)
	}
	return "NONE"
}

// Params configures the signal detector.
type Params struct {
	// MinRisePct is the minimum BTC price rise (%) over the observation window.
	MinRisePct float64
	// ObservationWindow is the rolling window used to measure momentum.
	ObservationWindow time.Duration
	// MaxHoldDuration caps how long a position can be held (failsafe).
	MaxHoldDuration time.Duration

	// MinChainlinkLagUSD is the minimum dollar difference (bitstamp - chainlink)
	// required to consider the oracle as lagging. e.g. 20.0 = $20 behind.
	MinChainlinkLagUSD float64

	// MinWindowRemaining is the minimum time left in the window to allow entry.
	// Trades entered with less time remaining may not be safely exitable.
	MinWindowRemaining time.Duration
	// MaxWindowRemaining is the maximum time left in the window to allow entry.
	// 0 = no cap. Set to e.g. 150s to restrict entries to the last 2.5 minutes
	// when the market is converging toward resolution.
	MaxWindowRemaining time.Duration

	// SafeExitBuffer is how far before window end to force-close open positions.
	SafeExitBuffer time.Duration

	// MaxEntryYesPrice caps the YES token price we'll accept on entry.
	MaxEntryYesPrice float64
	// MinEntryYesPrice floors the YES price we'll accept on entry.
	MinEntryYesPrice float64

	// CooldownDuration is the mandatory pause between consecutive BUY signals.
	CooldownDuration time.Duration

	// MinEdgeUSD is the minimum |BTC - open| required to enter. Prevents entering
	// on razor-thin edges where a small BTC move flips the outcome.
	MinEdgeUSD float64

	// MinBookDepthBTC is the minimum near-mid BTC orderbook depth on the
	// supporting side to allow entry. If bids (for YES) or asks (for NO) are
	// below this threshold the book is too thin to safely scalp.
	MinBookDepthBTC float64

	// MaxRecentRangeUSD is the maximum allowed BTC price range over the
	// observation window. If max-min exceeds this, the market is too volatile.
	// Acts as a hard cap independent of ATR.
	MaxRecentRangeUSD float64

	// MinATRUSD is the minimum Average True Range (mean tick-to-tick move) over
	// the observation window to allow entry. Prevents trading in dead-calm
	// markets where the lag signal is likely noise. 0 = disabled.
	MinATRUSD float64

	// MaxATRUSD is the maximum Average True Range over the observation window.
	// Prevents trading in wildly chaotic markets where the lag signal can
	// reverse before any fill settles. 0 = disabled.
	MaxATRUSD float64

	// ── Flash-reversal parameters (spread-aware detection) ──────────
	// FlashCooldown is the mandatory pause between consecutive flash signals.
	FlashCooldown time.Duration
	// FlashWindow is the rolling duration for YES price sample eviction.
	FlashWindow time.Duration
	// FlashMinWindowRemaining: minimum time left to allow flash entry (don't enter too late).
	FlashMinWindowRemaining time.Duration
	// FlashMaxWindowRemaining: flash only evaluates when window remaining <= this.
	// Set to 90s to restrict flash to the last 90 seconds of the window.
	FlashMaxWindowRemaining time.Duration
	// FlashMinYesPrice: minimum YES price to allow SignalFlashBuyYes entry.
	FlashMinYesPrice float64
	// FlashMaxYesPrice: maximum YES price to allow SignalFlashBuyNo entry.
	FlashMaxYesPrice float64

	// ── Spread-collapse detection ────────────────────────────────
	// FlashSpreadMinUSD: minimum peak BTC-vs-open spread (USD) to track.
	// If BTC never moved significantly from open, nothing to protect.
	FlashSpreadMinUSD float64
	// FlashSpreadCollapsePct: fraction of peak spread consumed to trigger WITH book confirmation.
	// e.g. 0.50 = fire when 50% of peak spread wiped out AND orderbook support was pulled.
	FlashSpreadCollapsePct float64
	// FlashSpreadCollapseBlindPct: fraction of peak spread consumed to trigger WITHOUT book confirmation.
	// e.g. 0.80 = fire on price action alone when 80% of spread consumed.
	FlashSpreadCollapseBlindPct float64

	// ── Bid/Ask floor-gone trigger (proactive — fires BEFORE price collapses) ────
	// FlashBidFloorGoneThreshold: fraction of peak spread-zone bid support remaining
	// to fire a proactive BUY NO. e.g. 0.15 = fire when <15% of peak bid floor is left
	// (85% has been pulled). Triggers at ANY window time (not restricted to last 90s).
	// 0 = disabled.
	FlashBidFloorGoneThreshold float64
	// FlashBidFloorGoneWindowSec: max seconds remaining for the bid-floor trigger to fire.
	// Default 300 = the entire 5-minute window. Set to your window duration.
	FlashBidFloorGoneWindowSec int
	// FlashBidFloorMinSpreadUSD: minimum peak BTC-vs-open spread to qualify for the
	// bid-floor trigger. Lower than FlashSpreadMinUSD since floor can be pulled on any
	// spread. Default 10.
	FlashBidFloorMinSpreadUSD float64

	// ── Bitstamp orderbook spoof detection ─────────────────────
	// NearMidUSD: USD band around BTC price for near-mid liquidity tracking.
	NearMidUSD float64
	// MinSpoofDepthBTC: minimum BTC volume on a side for a collapse to be meaningful.
	MinSpoofDepthBTC float64
	// MinWallSizeBTC: minimum absolute BTC volume that must vanish to count as a wall pull.
	MinWallSizeBTC float64
	// DepthCollapseThreshold: new depth < old * this → collapse (default 0.20 = 80% vanish).
	DepthCollapseThreshold float64
	// DepthCollapseArmedFor: how long a detected collapse stays "armed" (default 5s).
	DepthCollapseArmedFor time.Duration
	// FlashSupportCollapsePct: fraction of spread-zone depth that must vanish to flag (default 0.50).
	FlashSupportCollapsePct float64
	// FlashMinSupportBTC: minimum BTC depth in the spread zone to track collapse (default 0.5).
	FlashMinSupportBTC float64

	// ── Resolution-snipe parameters ─────────────────────────────────
	// ResolutionEnabled enables the resolution-snipe strategy (default false).
	// When enabled, fires in the final ResolutionWindowSec seconds when a token
	// is priced between ResolutionMinTokenPrice and ResolutionMaxTokenPrice.
	ResolutionEnabled bool
	// ResolutionWindowSec: how many seconds from window end to activate (default 38).
	ResolutionWindowSec int
	// ResolutionMinTokenPrice: minimum token price to enter (default 0.93).
	// e.g. 0.93 = only buy when token is at 93 or higher.
	ResolutionMinTokenPrice float64
	// ResolutionMaxTokenPrice: maximum token price to enter (default 0.97).
	// Buying above 0.97 leaves <=2c gross spread to 0.99 which fees consume.
	ResolutionMaxTokenPrice float64
	// ResolutionMinWindowSec: minimum seconds remaining at which the snipe still fires (default 8).
	// Active window = [ResolutionMinWindowSec, ResolutionWindowSec] seconds remaining.
	ResolutionMinWindowSec int
	// ResolutionMinBTCGapUSD: minimum absolute BTC-vs-open gap (USD) to enter (default 75).
	// A gap >$75 confirms strongly convicted direction; below this the flip risk is too high.
	ResolutionMinBTCGapUSD float64
	// ResolutionMinWinProb: minimum Gaussian P(win) required to enter a resolution snipe (default 0.85).
	// P = 0.5*(2-erfc(gap / (9*sqrt(sec_remaining)) / sqrt(2)))
	// Adapts gap requirement to time remaining: 60s needs ~$75; 10s needs ~$30.
	// Set to 0 to disable and rely on ResolutionMinBTCGapUSD alone.
	ResolutionMinWinProb float64
	// ── Early-tier resolution snipe (90–120s remaining, token 0.85–0.93) ────────
	// Captures the escalation phase before the token reaches the 0.93+ snipe band.
	// Carries a price stop-loss (unlike late snipes which hold to settlement).
	ResolutionEarlyEnabled       bool
	ResolutionEarlyWindowSec     int     // max seconds remaining to enter early tier (default 120)
	ResolutionEarlyMinWindowSec  int     // min seconds remaining for early tier (default 61)
	ResolutionEarlyMinTokenPrice float64 // floor of early entry band (default 0.85)
	ResolutionEarlyMaxTokenPrice float64 // ceiling of early entry band (default 0.93)
	ResolutionEarlyMinBTCGapUSD  float64 // min BTC-vs-open gap for early tier (default 50)
	// ── Velocity lockout ─────────────────────────────────────────────────────────
	// Prevents new lag/flash entries after a sudden BTC price spike (micro flash crash).
	// Does NOT apply to resolution snipes (those intentionally fire in volatile moments).
	// VelocityLockoutUSD: move threshold in USD to trigger lockout (0 = disabled, default 30).
	VelocityLockoutUSD float64
	// VelocityLockoutWindowMs: how fast the move must happen to count (default 3000ms).
	VelocityLockoutWindowMs int
	// VelocityLockoutSec: how long to block new entries after a velocity spike (default 2s).
	VelocityLockoutSec int
	// PostWinCooldownDuration: after a scalp_target or quicksell WIN close, block new
	// lag/flash entries for this duration (beyond the normal CooldownDuration).
	// Prevents re-entering the same window when the captured edge is already reversing.
	// 0 = disabled (use normal CooldownDuration for all closes).
	PostWinCooldownDuration time.Duration

	// ── Conviction snipe (mid-window, undecided market) ───────────────────────
	// ConvictionEnabled: enable the mid-window conviction strategy (default false).
	// Fires when the market is undecided (token near 60–70) but BTC has a strong
	// directional edge, giving high Gaussian win probability.
	ConvictionEnabled bool
	// PairArbEnabled enables the paired zigzag arbitrage lead-leg detector.
	// It buys the side that benefits from the BTC gap shrinking back toward zero,
	// then waits to match the other side only when the total pair cost locks the spread.
	PairArbEnabled bool
	// PairArbMinWindowSec and PairArbMaxWindowSec define the active window for
	// opening the lead leg, measured as elapsed seconds since window start.
	// Example for a 5m market: min=45, max=210 allows entries only between
	// 00:45 and 03:30 into the window (blocks first 45s and last 90s).
	PairArbMinWindowSec int
	PairArbMaxWindowSec int
	// PairArbMinTokenPrice and PairArbMaxTokenPrice constrain the lead-leg token
	// band. The lead should still be reasonably priced so the hedge can be added later.
	PairArbMinTokenPrice float64
	PairArbMaxTokenPrice float64
	// PairArbMinBTCGapUSD and PairArbMaxBTCGapUSD require the BTC-vs-open move to
	// be meaningful but not already too extended.
	PairArbMinBTCGapUSD float64
	PairArbMaxBTCGapUSD float64
	// PairArbCarryEnabled keeps short memory across window rollovers.
	// When enabled, the first PairArbCarryEarlySec seconds of a new window may
	// use PairArbCarryMinBTCGapUSD (instead of PairArbMinBTCGapUSD) if current
	// gap direction matches the previous window close momentum and the prior
	// close gap magnitude exceeded PairArbCarryMinPrevGapUSD.
	PairArbCarryEnabled       bool
	PairArbCarryEarlySec      int
	PairArbCarryMinBTCGapUSD  float64
	PairArbCarryMinPrevGapUSD float64
	// PairArbMinGapVelocityUSD requires the BTC gap to be shrinking with enough speed.
	PairArbMinGapVelocityUSD float64
	// PairArbMinLockedProfitCents is the target locked spread used by pair-arb entry gating.
	PairArbMinLockedProfitCents float64
	// PairArbMaxHedgeDistanceCents caps how far above immediate hedgeability the opposite leg
	// may be when opening a lead. 0 = disabled.
	PairArbMaxHedgeDistanceCents float64
	// PairArbMinCVDBTC requires aligned Binance CVD for pair-arb lead entry. 0 = disabled.
	// YES lead (gap<0) requires CVD >= +min; NO lead (gap>0) requires CVD <= -min.
	PairArbMinCVDBTC float64
	// PairArbMinBookImbalance requires aligned order-book imbalance for pair-arb lead entry.
	// YES lead (gap<0) requires imbalance >= +min; NO lead (gap>0) requires <= -min.
	PairArbMinBookImbalance float64
	// PairArbMinCoinbaseSpreadUSD requires aligned Coinbase-vs-fast-BTC spread.
	// YES lead (gap<0) requires spread >= +min; NO lead (gap>0) requires <= -min.
	PairArbMinCoinbaseSpreadUSD float64
	// PairArbMaxCoinbaseSpreadUSD caps aligned Coinbase-vs-fast-BTC spread magnitude.
	// YES lead (gap<0) requires spread <= +max; NO lead (gap>0) requires >= -max.
	// 0 = disabled.
	PairArbMaxCoinbaseSpreadUSD float64
	// PairArbFlowMovementMode switches selected flow gates (CVD, book imbalance,
	// Coinbase spread) from direction-aligned checks to absolute-magnitude checks.
	PairArbFlowMovementMode bool
	// PairArbMinCoinbaseTakerImbalance requires aligned Coinbase taker imbalance
	// over the rolling 30s window. YES lead (gap<0) requires imbalance >= +min;
	// NO lead (gap>0) requires imbalance <= -min. 0 = disabled.
	PairArbMinCoinbaseTakerImbalance float64
	// PairArbMinOIDeltaUSD requires OI delta alignment for pair-arb lead entries.
	// YES lead (gap<0) requires oi_delta >= +min; NO lead (gap>0) requires
	// oi_delta <= -min. 0 = disabled.
	PairArbMinOIDeltaUSD float64
	// PairArbMaxContraOIDeltaUSD blocks pair-arb entries when OI delta is strongly
	// contra-directional beyond this absolute USD threshold. 0 = disabled.
	PairArbMaxContraOIDeltaUSD float64
	// PairArbMaxAdverseYesDriftCents blocks pair-arb lead entries when YES price
	// has moved against the intended side by more than this threshold over the
	// recent PairArbYesDriftWindowSec lookback. 0 = disabled.
	PairArbMaxAdverseYesDriftCents float64
	// PairArbYesDriftWindowSec is the lookback window in seconds for adverse YES
	// drift gating. 0 uses 10 seconds.
	PairArbYesDriftWindowSec int
	// PairArbMinGapHoldSec requires the |BTC-open| gap to have been continuously
	// above PairArbMinBTCGapUSD in the same direction for at least this many seconds
	// before a lead-leg entry fires. 0 = disabled.
	PairArbMinGapHoldSec int
	// PairArbMinBTCTickRun requires the last N BTC ticks to keep moving in the
	// gap direction before allowing entry. 0 = disabled.
	PairArbMinBTCTickRun int
	// PairArbEarlyWindowMaxSec enables a dedicated early-entry tier for the first N
	// seconds after each window opens. During this tier the bot targets cheap lead
	// tokens while the gap is still small. When > 0 and elapsed <= this value,
	// PairArbEarlyMinGapUSD and PairArbEarlyMinVelocityUSD override the standard
	// gap/velocity thresholds. 0 = disabled.
	PairArbEarlyWindowMaxSec int
	// PairArbEarlyMinGapUSD is the minimum |BTC-open| gap for early-window entries.
	// Typically lower than PairArbMinBTCGapUSD. 0 = use PairArbMinBTCGapUSD.
	PairArbEarlyMinGapUSD float64
	// PairArbEarlyMinVelocityUSD is the directional gap velocity ($/s) required
	// during the early window. If 0, falls back to PairArbMinGapVelocityUSD.
	PairArbEarlyMinVelocityUSD float64
	// PairArbReverseSignal* enable a reversal lead mode that buys the opposite
	// side when the BTC-open gap is shrinking quickly.
	PairArbReverseSignalEnabled        bool
	PairArbReverseSignalMinGapUSD      float64
	PairArbReverseSignalMaxGapUSD      float64
	PairArbReverseSignalMinVelocityUSD float64
	PairArbReverseSignalLookbackSec    int
	PairArbReverseSignalMinShrinkUSD   float64
	// PairArbEarlyPrevDirConfirmSec: when > 0, entries during the first N seconds of
	// a window require the current gap direction to MATCH the prior window's close
	// gap direction (provided |prevGap| >= PairArbEarlyMinPrevDirGapUSD). This
	// blocks fleeting spike-and-reverse entries at window open when momentum was
	// already running the other way. 0 = disabled.
	PairArbEarlyPrevDirConfirmSec int
	// PairArbEarlyMinPrevDirGapUSD: minimum |prior-window close gap| that activates
	// the PairArbEarlyPrevDirConfirmSec filter. Defaults to 5.0 when 0.
	PairArbEarlyMinPrevDirGapUSD float64
	// PairArbPreOpen* enable a pre-market-open carry strategy.
	// In the first PairArbPreOpenEntrySec seconds of a new window, buy the momentum
	// side when the previous window's last PairArbPreOpenSrcWindowSec seconds showed
	// ≥80% directional agreement with |gap| ≥ PairArbPreOpenMinGapUSD.
	// The only entry gate is a lead-token price in [PreOpenMinTokenPrice, PreOpenMaxTokenPrice].
	PairArbPreOpenEnabled       bool
	PairArbPreOpenEntrySec      int     // default 5
	PairArbPreOpenSrcWindowSec  int     // default 15
	PairArbPreOpenMinGapUSD     float64 // default 3.0
	PairArbPreOpenMinTokenPrice float64 // default 0.46
	PairArbPreOpenMaxTokenPrice float64 // default 0.54
	// PairArbTruePreOpen* pre-connect to the NEXT window's market before the current
	// window ends, then buy the next market's shares using current window's momentum.
	PairArbTruePreOpenEnabled bool
	PairArbTruePreOpenLeadSec int // seconds before win.End to start next-market discovery (default 15)
	// PairArbCVDMomentum* enable early-window entries directed by BTC CVD in low-gap markets.
	// When elapsed <= MaxElapsedSec AND |gap| < MaxGapUSD AND |CVD| >= MinCVDBTC, enter in the
	// CVD-directed side. Standard gap/velocity gates are bypassed; only token-price range applies.
	PairArbCVDMomentumEnabled           bool
	PairArbCVDMomentumMaxElapsedSec     int     // default 60
	PairArbCVDMomentumMaxGapUSD         float64 // default 15
	PairArbCVDMomentumMinCVDBTC         float64 // default 10
	PairArbCVDMomentumLockedProfitCents float64 // default 3
	// PairArbCVDMomentumMaxLeadDriftCents blocks CVD momentum entries when the Polymarket lead
	// token has already moved > N cents in the entry direction over the last 10 seconds.
	// 0 = disabled. Set to 1 to reject "buying the top" entries where Poly already caught up.
	PairArbCVDMomentumMaxLeadDriftCents float64 // default 0 (disabled)
	// PairArbSessionTrendFilterUSD blocks pair-arb entries whose direction opposes a
	// sustained multi-window BTC trend. When > 0, if the BTC open price drifted more
	// than this threshold (USD) over the last PairArbSessionTrendBuckets windows, entries
	// that contradict the trend are blocked.
	// BTC trending up (drift > threshold) → block NO leads.
	// BTC trending down (drift < -threshold) → block YES leads.
	// 0 = disabled (default). Recommended: 10.
	PairArbSessionTrendFilterUSD float64
	// PairArbSessionTrendBuckets is the number of past 5m windows to look back when
	// computing the session BTC drift. Default 6 (= 30 minutes).
	PairArbSessionTrendBuckets int
	// PairArbDirectionMode restricts which lead side may be entered.
	// "YES" = only YES-lead entries (gap > 0). "NO" = only NO-lead entries (gap < 0).
	// "BOTH" or empty = no restriction (default).
	PairArbDirectionMode string
	// PairArbElapsedSkipFromSec / PairArbElapsedSkipToSec: skip entries when
	// elapsed is in [From, To] (inclusive). 0/0 = disabled.
	PairArbElapsedSkipFromSec float64
	PairArbElapsedSkipToSec   float64
	// PairArbMaxCVDBTC: skip when CVD30s exceeds this threshold in the entry
	// direction (YES: cvd>max; NO: cvd<-max). 0 = disabled.
	PairArbMaxCVDBTC float64
	// PairArbPrevWinGapSkipFrom / To: block all entries in the current window
	// when the prior window closed with a gap in [From, To) USD. 0/0 = disabled.
	PairArbPrevWinGapSkipFrom float64
	PairArbPrevWinGapSkipTo   float64
	// PairArbVelGapRatioSkipFrom / To: skip when |gapVelocity|/absGap is in
	// [From, To). 0/0 = disabled.
	PairArbVelGapRatioSkipFrom float64
	PairArbVelGapRatioSkipTo   float64
	// PairArbMaxVelGapRatio: skip when |gapVelocity|/absGap exceeds this.
	// 0 = disabled.
	PairArbMaxVelGapRatio float64
	// PairArbCVDRangeSkipFrom / To: skip when CVD30s is in [From, To) for YES
	// lead or [-To, -From) for NO lead. 0/0 = disabled.
	PairArbCVDRangeSkipFrom float64
	PairArbCVDRangeSkipTo   float64
	// PairArbYesDriftSkipFromCents / To: skip when yes_drift_cents is in
	// [From, To) regardless of lead direction. 0/0 = disabled.
	PairArbYesDriftSkipFromCents float64
	PairArbYesDriftSkipToCents   float64
	// PairArbTakerImbSkipFrom / To: skip when cbTakerImbalance is in
	// [From, To) for YES lead or [-To, -From) for NO lead. 0/0 = disabled.
	PairArbTakerImbSkipFrom float64
	PairArbTakerImbSkipTo   float64
	// PairArbEarlyElapsedTickRunSkipSec: within the first N seconds of the
	// window, require >=1 BTC tick in the entry direction. 0 = disabled.
	PairArbEarlyElapsedTickRunSkipSec float64
	// ConvictionMaxWindowSec: upper bound on window time remaining (default 90).
	ConvictionMaxWindowSec int
	// ConvictionMinWindowSec: lower bound on window time remaining (default 60).
	ConvictionMinWindowSec int
	// ConvictionMinTokenPrice: lower bound of the "undecided" token price band (default 0.60).
	ConvictionMinTokenPrice float64
	// ConvictionMaxTokenPrice: upper bound of the "undecided" token price band (default 0.70).
	ConvictionMaxTokenPrice float64
	// ConvictionMinBTCGapUSD: minimum |BTC − open| in dollars required (default 40).
	ConvictionMinBTCGapUSD float64
	// ConvictionMinWinProb: Gaussian win probability threshold (default 0.80).
	ConvictionMinWinProb float64
	// ConvictionCooldownDuration: mandatory pause between consecutive conviction signals.
	ConvictionCooldownDuration time.Duration
	// ConvictionMaxYesDriftCents: block conviction entry if the YES token price has moved
	// this many cents AGAINST the intended direction over the most recent FlashWindow (default
	// 10 s). A falling YES while wanting to buy YES means Polymarket smart money is already
	// repositioning for a BTC reversal — even when the Gaussian model still shows high win
	// probability. 0 = disabled.
	ConvictionMaxYesDriftCents float64

	// ── Cross-exchange microstructure (Binance perpetual futures) ─────────────────────

	// CVDMinDivergenceBTC: block conviction entry when the 30-second Binance rolling
	// Cumulative Volume Delta (net buyer-aggressor BTC − seller-aggressor BTC) contradicts
	// the trade direction by more than this threshold. A strongly negative CVD while BTC is
	// above open means large sellers are absorbing bids without moving price — classic
	// distribution before a reversal (smart money entering short). 0 = disabled.
	CVDMinDivergenceBTC float64

	// TakerBuyRatioMin: in the order-flow early-entry path, require taker_buy_vol /
	// (buy+sell) >= this fraction. >0.65 = aggressive buy-sweeping the ask. 0 = disabled.
	TakerBuyRatioMin float64

	// BookImbalanceMin: minimum absolute normalised bid/ask imbalance from the Deribit
	// near-mid order book required to contradict entry. Ratio = (bids-asks)/(bids+asks)
	// in the NearMidUSD band. For a YES buy the ratio must be > -BookImbalanceMin (i.e.
	// bids not dominated by asks). 0 = disabled (recommended when NearMidUSD is narrow).
	BookImbalanceMin float64

	// FundingRateMinYesBuy: skip YES/BUY entries when the BTC-PERPETUAL 8h funding rate
	// is below this threshold (negative funding = shorts are paid, aggressive downside pressure).
	// Units: per-8h rate, e.g. -0.0015 = -0.15%/8h. 0 = disabled (default).
	FundingRateMinYesBuy float64
	// FundingRateMaxNoBuy: skip NO/BUY entries when the 8h funding rate exceeds this
	// threshold (positive funding = longs are paid, crowded upside that may snap back).
	// Units: per-8h rate, e.g. 0.0015 = +0.15%/8h. 0 = disabled (default).
	FundingRateMaxNoBuy float64

	// ── High-edge conviction window override ──────────────────────────────────
	// When gap >= ConvictionHighEdgeMinUSD AND win prob >= ConvictionHighEdgeProbThreshold,
	// relax the minimum window time to ConvictionHighEdgeMinWindowSec. Catches late-session
	// setups where BTC surges hard (e.g. +$50–$90) with only 30–55s remaining — cases where
	// the ConvictionMinWindowSec guard becomes overly conservative.
	// Set ConvictionHighEdgeMinUSD=0 to disable (default enabled at 50.0).
	ConvictionHighEdgeMinUSD float64
	// ConvictionHighEdgeMinWindowSec: absolute minimum window seconds when override is active.
	// Must be >= FlashMinWindowRemaining to leave time to exit. Default 30.
	ConvictionHighEdgeMinWindowSec int
	// ConvictionHighEdgeProbThreshold: win probability required for the override path.
	// Higher than ConvictionMinWinProb to require a stronger signal. Default 0.88.
	ConvictionHighEdgeProbThreshold float64
	// ConvictionHighEdgeMaxTokenPrice: max YES/NO token price permitted when high-edge conditions
	// are met (gap >= ConvictionHighEdgeMinUSD AND prob >= ConvictionMinWinProb). Allows entries
	// on partially priced-in markets like YES=0.87 when BTC gap is $66 and prob=86.8%.
	// Default 0.92. 0 = disabled (always use ConvictionMaxTokenPrice).
	ConvictionHighEdgeMaxTokenPrice float64

	// ConvictionGapVelocityWindowSec: look-back window (seconds) for computing the rate of
	// change of the BTC-vs-open gap. Default 30. 0 = guard disabled.
	ConvictionGapVelocityWindowSec int
	// ConvictionMaxGapShrinkRateUSD: block conviction entry when the absolute gap is collapsing
	// toward open faster than this many USD per second over ConvictionGapVelocityWindowSec.
	// Example: 1.5 $/s blocks when gap fell >$45 in 30s — BTC is actively mean-reverting.
	// 0 = disabled (default).
	ConvictionMaxGapShrinkRateUSD float64
	// ConvictionMinGapGrowthRateUSD: require the BTC-vs-open gap to be expanding at LEAST
	// this many USD per second before allowing a conviction entry. This is the mirror of
	// ConvictionMaxGapShrinkRateUSD: that gate blocks reversals; this gate ensures BTC is
	// actively trending in the entry direction — not hovering at a stale gap that is likely
	// to reverse. A value of 0.5–1.5 filters flat setups while catching fast momentum moves.
	// Key insight: other participants track BTC velocity directly and enter early on momentum;
	// this gate ensures we enter on the same confirmatory signal. 0 = disabled (default).
	ConvictionMinGapGrowthRateUSD float64

	// ── Order-flow-led entry (CVD + book imbalance) ───────────────────────────
	// Detected pattern: Polymarket YES/NO token prices move 10–30 seconds AHEAD
	// of BTC/ChainLink confirmation because smart money reads Binance net-aggressor
	// flow (CVD) and Deribit near-mid order book imbalance directly.
	// Example: CVD=+55 BTC, imb=+0.33, YES=0.22 — BTC still $12 below open —
	// then YES jumped to 0.59 while CL was still showing -37 edge.
	// This path bypasses the BTC-gap and win-probability gates entirely.

	// ConvictionOrderFlowEnabled: enable the CVD+book early-entry path.
	ConvictionOrderFlowEnabled bool
	// ConvictionOrderFlowMinCVD: minimum absolute 30s Binance net-aggressor BTC
	// to treat as a directional signal. +ve value triggers YES; -ve triggers NO.
	// Suggested 30.0: 30 net BTC of buy/sell aggressor flow in 30s is institutional.
	ConvictionOrderFlowMinCVD float64
	// ConvictionOrderFlowMinImbalance: minimum absolute Deribit near-mid book
	// imbalance aligned with CVD direction. Confirms options-market agrees with
	// perp flow. Suggested 0.20 (bids 20% heavier than asks in direction).
	ConvictionOrderFlowMinImbalance float64
	// ConvictionOrderFlowMaxTokenPrice: only enter when the token is still cheap,
	// meaning the market hasn't yet priced in the order flow signal. Once YES is
	// already 0.65+, the edge from leading the move is gone. Suggested 0.42.
	ConvictionOrderFlowMaxTokenPrice float64
	// ConvictionOrderFlowMaxAdverseBTCGap: skip order-flow entry when BTC is
	// already more than this many USD on the WRONG side of open. Prevents buying
	// YES when BTC is $50 below open even with strong bullish flow. Suggested 25.0.
	// 0 = disabled (pure order-flow entry regardless of BTC position).
	ConvictionOrderFlowMaxAdverseBTCGap float64

	// CoinbaseSpreadMinUSD: in the order-flow entry path, optionally require that
	// Coinbase BTC-USD spot price exceeds the BTC reference price by at least this
	// many USD (positive = US institutional spot premium = accumulation signal). 0 = off.
	CoinbaseSpreadMinUSD float64
	// Skew25dMinYesBuy: require the Deribit 25-delta risk-reversal (call IV - put IV)
	// to be ≥ this value before YES order-flow entries fire. Positive skew = options
	// market paying up for upside = strong YES pre-signal. 0 = disabled.
	Skew25dMinYesBuy float64
	// CoinbaseCVDMinBTC: require the rolling 30-second net-aggressor BTC volume on
	// Coinbase BTC-USD spot to be ≥ this threshold for YES order-flow entries (negated
	// for NO). This validates Binance CVD with an independent institutional spot signal:
	// if Coinbase is also net-buying, the institutional conviction is cross-venue confirmed.
	// 0 = disabled. Typical: 3.0 (3 BTC net spot buying in 30s on Coinbase).
	CoinbaseCVDMinBTC float64

	// ── Reversal snipe (last N seconds of the window) ─────────────────────────
	// Fires in the final SnipeMaxWindowSec–SnipeMinWindowSec seconds when two
	// indicators simultaneously signal an imminent crossing of the open price:
	//   1. Coinbase spot price is >SnipeCoinbaseSpreadUSD below (for NO) or above
	//      (for YES) the fast BTC reference price — our faster feed is already pricing
	//      the move before Chainlink catches up.
	//   2. Binance 30-second rolling CVD is >SnipeCVDThresholdBTC net-sellers (NO)
	//      or net-buyers (YES) — microstructure confirms smart-money positioning.
	//
	// Positions entered via the snipe always hold to window resolution (no scalp exit).

	// SnipeEnabled: enable the reversal-snipe strategy (default false).
	SnipeEnabled bool
	// SnipeMinWindowSec: minimum seconds remaining to allow snipe entry (default 10).
	SnipeMinWindowSec int
	// SnipeMaxWindowSec: maximum seconds remaining to allow snipe entry (default 40).
	SnipeMaxWindowSec int
	// SnipeCoinbaseSpreadUSD: minimum |coinbasePrice − fastBTCPrice| required (default 8.0).
	// For NO: coinbasePrice < fastBTCPrice − threshold. For YES: coinbasePrice > fastBTCPrice + threshold.
	SnipeCoinbaseSpreadUSD float64
	// SnipeCVDThresholdBTC: minimum absolute Binance 30s CVD required to confirm direction (default 15.0).
	// For NO: cvd < −threshold. For YES: cvd > +threshold.
	SnipeCVDThresholdBTC float64
	// SnipeMaxNoEntryPrice: maximum NO token price to allow entry (default 0.75).
	// Don't buy NO when it's already at 75 — most edge is already priced in.
	SnipeMaxNoEntryPrice float64
	// SnipeMaxYesEntryPrice: maximum YES token price to allow snipe YES entry (default 0.75).
	SnipeMaxYesEntryPrice float64
	// SnipeMinNoEntryPrice: minimum NO token price to allow entry (default 0.25).
	// Rejects near-worthless NO tokens where the gap is already so large a reversal is implausible.
	SnipeMinNoEntryPrice float64
	// SnipeMinYesEntryPrice: minimum YES token price to allow snipe YES entry (default 0.25).
	SnipeMinYesEntryPrice float64
	// SnipeMinBTCGapUSD: minimum |BTC − open| gap required to fire the snipe (default 10.0).
	// Ensures the cheap token price is because BTC is meaningfully on the wrong side, not just
	// market uncertainty. Avoids borderline setups where a $2 move suffices — no real edge there.
	SnipeMinBTCGapUSD float64
	// SnipeMaxBTCGapUSD: optional user cap on |BTC − open| gap for reversal snipes (default 0 = disabled).
	// Use this to reject overstretched moves even before the built-in time-based max-flippable guard.
	SnipeMaxBTCGapUSD float64
	// SnipeMinWinProb: minimum Gaussian win probability required to fire the snipe (default 0.55).
	// winProb is already computed from gap + time remaining; this gate rejects setups where the
	// flip is statistically marginal even though Coinbase/CVD show momentum.
	SnipeMinWinProb float64
	// SnipeDynamicATRRefUSD: when set > 0, scale snipe thresholds by ATR/ref.
	// Values below the ref reduce thresholds; values above the ref increase them.
	SnipeDynamicATRRefUSD float64
	// SnipeDynamicATRMinScale clamps the ATR scale factor floor (e.g. 0.6).
	SnipeDynamicATRMinScale float64
	// SnipeDynamicATRMaxScale clamps the ATR scale factor ceiling (e.g. 1.4).
	SnipeDynamicATRMaxScale float64

	// ── Last-second collapse snipe ───────────────────────────────────────────────
	// A separate entry regime for near-settled markets (NO or YES ≤ 6¢) where BTC
	// suddenly reverses in the final 4–9 seconds, paying ~50× on a small bet.
	// Because the market is already 94¢+ priced on one side and the crash is already
	// in motion (CVD/Coinbase confirmed), the win_prob Gaussian gate is bypassed.
	//
	// SnipeCollapseEnabled: enable the last-second collapse extension (default false).
	SnipeCollapseEnabled bool
	// SnipeCollapseMinWindowSec: minimum seconds remaining to fire the collapse snipe (default 4).
	SnipeCollapseMinWindowSec int
	// SnipeCollapseMaxWindowSec: maximum seconds remaining for collapse snipe (default 9).
	// Deliberately below SnipeMinWindowSec so the two modes never overlap.
	SnipeCollapseMaxWindowSec int
	// SnipeCollapseMinTokenPrice: minimum cheap-side token price for collapse entry (default 0 = disabled).
	// Blocks 0.01-cent tokens where the market has already settled with near-certainty.
	// Data: all confirmed 0.01 EP tokens lost; both wins were at 0.02+ EP.
	SnipeCollapseMinTokenPrice float64
	// SnipeCollapseMaxTokenPrice: maximum cheap-side token price for collapse entry (default 0.06).
	// Only enter when the market has priced the other side at ≥94¢.
	SnipeCollapseMaxTokenPrice float64
	// SnipeCollapseCVDThresholdBTC: minimum |CVD| to confirm the crash (default 14.0).
	// Slightly below SnipeCVDThresholdBTC to catch the rem=5 tick (CVD was −14.3 in live data).
	SnipeCollapseCVDThresholdBTC float64
	// SnipeCollapseCoinbaseSpreadUSD: minimum |coinbase spread| for collapse snipe (default 15.0).
	// Higher than the normal snipe threshold — needs a strong Coinbase signal for this last-second bet.
	SnipeCollapseCoinbaseSpreadUSD float64
	// SnipeCollapseMinBTCGapUSD: minimum |gap| at fire time (default 10.0).
	// Gap will be shrinking fast; ensures BTC was meaningfully on the other side.
	SnipeCollapseMinBTCGapUSD float64
	// SnipeCollapseRequireCrossUSD: for NO collapse, only fire if the gap at window open was
	// ≤ this threshold (BTC was NOT already above open when the window started).
	// Eliminates persistent-elevation losses where BTC has been above open for the full 5 minutes.
	// Set to 0 to disable (default 0 = disabled).
	SnipeCollapseRequireCrossUSD float64
	// SnipeCollapseMaxBTCGapUSD: maximum |gap| allowed for collapse snipe (default 50.0).
	// A gap larger than this cannot realistically be closed in 4–9 seconds (~$7–12/s required).
	// Backtest shows all false signals had |gap| > $50; both confirmed wins had |gap| ≤ $40.
	// 0 = no maximum (disabled).
	SnipeCollapseMaxBTCGapUSD float64

	// ── Mid-window flip (gap shrinking fast) ────────────────────────────────────

	MidFlipEnabled              bool
	MidFlipMinWindowSec         int
	MidFlipMaxWindowSec         int
	MidFlipMinTokenPrice        float64
	MidFlipMaxTokenPrice        float64
	MidFlipMinBTCGapUSD         float64
	MidFlipMaxBTCGapUSD         float64
	MidFlipMinWinProb           float64
	MidFlipGapVelocityWindowSec int
	MidFlipMinGapShrinkRateUSD  float64

	// ── Late flip (no CVD gate) ────────────────────────────────────────────────
	LateFlipEnabled       bool
	LateFlipMinWindowSec  int
	LateFlipMaxWindowSec  int
	LateFlipMinTokenPrice float64
	LateFlipMaxTokenPrice float64
	LateFlipMinBTCGapUSD  float64
	// LateFlipMaxBTCGapUSD: maximum |gap| for late-flip entry (0 = no limit).
	// Data: all 4 wins had |gap| ≤ 42; 7 of 10 losses had |gap| > 50.
	LateFlipMaxBTCGapUSD         float64
	LateFlipMinWinProb           float64
	LateFlipMinCoinbaseSpreadUSD float64

	// ── Scalp (prob + gap, mid token) ──────────────────────────────────────────
	ScalpEnabled       bool
	ScalpMinWindowSec  int
	ScalpMaxWindowSec  int
	ScalpMinTokenPrice float64
	ScalpMaxTokenPrice float64
	ScalpMinBTCGapUSD  float64
	ScalpMaxBTCGapUSD  float64
	ScalpMinWinProb    float64

	// ── Deep-discount fade (contrarian 2-5c tokens) ─────────────────────────────
	// High-risk: the market is strongly pricing one side. This path intentionally
	// fades extreme pricing when there is still enough time for a snap-back.
	DeepDiscountFadeEnabled       bool
	DeepDiscountFadeMinWindowSec  int
	DeepDiscountFadeMaxWindowSec  int
	DeepDiscountFadeMinTokenPrice float64
	DeepDiscountFadeMaxTokenPrice float64
	DeepDiscountFadeMinBTCGapUSD  float64
	DeepDiscountFadeMaxBTCGapUSD  float64

	// ── Penny-buy (absolute floor at $0.01) ──────────────────────────────────────
	// Fires when YES or NO hits $0.01 with ≥PennyBuyMinWindowSec seconds left.
	// No BTC/CVD gate — pure price asymmetry: cheap side holds to resolution.
	PennyBuyEnabled       bool
	PennyBuyMinWindowSec  int     // minimum seconds remaining (default 90)
	PennyBuyMaxWindowSec  int     // maximum seconds remaining; set to 240 to enforce ≥60s elapsed (default 240)
	PennyBuyMaxTokenPrice float64 // token price ceiling to enter (default 0.01)
	PennyBuyMaxBTCGapUSD  float64 // maximum |BTC-open| gap to allow (0 = no limit)

	// ── DCA+Hedge strategy (simultaneous dual-leg entry on token-price drift) ────────────────────
	// Fires when (yes + no) < DCAHedgeMaxEntrySum AND one side has risen >= DCAHedgeMoveTrigger
	// from the window-open price. Both the moved side and the opposite leg are bought together.
	// After entry, if the moved side drops DCAHedgeDCAReversal from the trigger price, an
	// additional DCAHedgeDCAAddShares buy is placed on that side (one DCA per entry).
	// Dynamic sizing (when DCAHedgeUseDynamicSizing = true): btcTickRun>0 ×3.0, elapsed<20s ×1.5,
	// |BTC-open|<$4 ×1.5; shares = max(DCAHedgeBaseShares, round-to-5), capped at DCAHedgeMaxShares.
	DCAHedgeEnabled            bool
	DCAHedgeMoveTrigger        float64 // price rise (from window open) to trigger entry (default 0.10)
	DCAHedgeMaxEntrySum        float64 // max yes+no sum to allow entry (default 0.98)
	DCAHedgeBaseShares         float64 // base shares per leg (default 5)
	DCAHedgeMaxShares          float64 // max shares cap with dynamic sizing (default 50)
	DCAHedgeDCAReversal        float64 // moved-side must drop this many cents for DCA fill (default 0.20)
	DCAHedgeDCAAddShares       float64 // additional shares on DCA fill (default 5)
	DCAHedgeUseDynamicSizing   bool    // apply btcTickRun/elapsed/gap multipliers to base shares
	DCAHedgeSwingTortuosityMin float64 // swing filter: require total_path_var/net_rise >= this; 0=disabled
	DCAHedgeSwingOppRiseMin    float64 // swing filter: require opp side had a prior rise >= this in window; 0=disabled
	DCAHedgeMinElapsedSec      float64 // swing filter: block trigger if fired within N seconds of window start; 0=disabled
}

// DefaultParams returns sensible defaults for the MVP.
func DefaultParams() Params {
	return Params{
		MinRisePct:         0.0,
		ObservationWindow:  30 * time.Second,
		MaxHoldDuration:    3 * time.Minute,
		MinChainlinkLagUSD: 0.0,
		MinWindowRemaining: 90 * time.Second,
		MaxWindowRemaining: 0, // no cap by default
		SafeExitBuffer:     30 * time.Second,
		MaxEntryYesPrice:   0.55,
		MinEntryYesPrice:   0.10,
		CooldownDuration:   20 * time.Second,
		// Flash (spread-aware)
		FlashCooldown:           15 * time.Second,
		FlashWindow:             10 * time.Second,
		FlashMinWindowRemaining: 10 * time.Second,
		FlashMaxWindowRemaining: 90 * time.Second,
		FlashMinYesPrice:        0.15,
		FlashMaxYesPrice:        0.85,
		// Spread-collapse (big moves only)
		FlashSpreadMinUSD:           50.0,
		FlashSpreadCollapsePct:      0.70,
		FlashSpreadCollapseBlindPct: 0.90,
		// Bid-floor-gone proactive trigger: fires anywhere in the window
		FlashBidFloorGoneThreshold: 0.15, // fire when <15% of peak bid floor remains (85% pulled)
		FlashBidFloorGoneWindowSec: 300,  // active for entire 5-minute window
		FlashBidFloorMinSpreadUSD:  10.0, // lower bar than FlashSpreadMinUSD — floor can be pulled on any spread
		// Orderbook
		NearMidUSD:              50.0,
		MinSpoofDepthBTC:        2.0,
		MinWallSizeBTC:          1.5,
		DepthCollapseThreshold:  0.20,
		DepthCollapseArmedFor:   5 * time.Second,
		FlashSupportCollapsePct: 0.50,
		FlashMinSupportBTC:      0.5,
		// Resolution snipe (disabled by default)
		ResolutionEnabled:       false,
		ResolutionWindowSec:     60,
		ResolutionMinWindowSec:  8,
		ResolutionMinTokenPrice: 0.93,
		ResolutionMaxTokenPrice: 0.97,
		ResolutionMinBTCGapUSD:  75.0,
		ResolutionMinWinProb:    0.85,
		// Early-tier resolution (disabled by default)
		ResolutionEarlyEnabled:       false,
		ResolutionEarlyWindowSec:     120,
		ResolutionEarlyMinWindowSec:  61,
		ResolutionEarlyMinTokenPrice: 0.85,
		ResolutionEarlyMaxTokenPrice: 0.93,
		ResolutionEarlyMinBTCGapUSD:  50.0,
		// Velocity lockout: $30 in 3s triggers a 2s entry block
		VelocityLockoutUSD:      30.0,
		VelocityLockoutWindowMs: 3000,
		VelocityLockoutSec:      2,
		// Conviction snipe (disabled by default)
		ConvictionEnabled:                  false,
		PairArbReverseSignalEnabled:        false,
		PairArbReverseSignalMinGapUSD:      20.0,
		PairArbReverseSignalMaxGapUSD:      0,
		PairArbReverseSignalMinVelocityUSD: 0.25,
		PairArbReverseSignalLookbackSec:    8,
		PairArbReverseSignalMinShrinkUSD:   2.0,
		ConvictionMaxWindowSec:             90,
		ConvictionMinWindowSec:             60,
		ConvictionMinTokenPrice:            0.60,
		ConvictionMaxTokenPrice:            0.70,
		ConvictionMinBTCGapUSD:             40.0,
		ConvictionMinWinProb:               0.80,
		ConvictionCooldownDuration:         5 * time.Second,
		ConvictionMaxYesDriftCents:         3.0,
		// Cross-exchange microstructure defaults (Binance CVD)
		CVDMinDivergenceBTC: 5.0, // 5 BTC net imbalance in 30s is a meaningful signal
		TakerBuyRatioMin:    0,   // disabled by default; 0.62 adds buy-ratio gate to order-flow
		BookImbalanceMin:    0,   // disabled by default (Deribit near-mid band too narrow)
		// High-edge conviction window override
		ConvictionHighEdgeMinUSD:            50.0,  // gap > $50 + overwhelming prob → relax window gate
		ConvictionHighEdgeMinWindowSec:      30,    // absolute floor with override active
		ConvictionHighEdgeProbThreshold:     0.88,  // stronger prob required for the relaxed window
		ConvictionHighEdgeMaxTokenPrice:     0.80,  // allow YES/NO up to 80 when gap≥$50 and prob≥HighEdgeProbThreshold
		ConvictionGapVelocityWindowSec:      30,    // 30-second look-back for gap velocity
		ConvictionMaxGapShrinkRateUSD:       0,     // disabled by default; set 0.6 in all.env
		ConvictionMinGapGrowthRateUSD:       0,     // disabled by default; set 0.8 in all.env
		ConvictionOrderFlowEnabled:          false, // disabled by default
		ConvictionOrderFlowMinCVD:           30.0,  // 30 net BTC aggressor in 30s
		ConvictionOrderFlowMinImbalance:     0.20,  // bids 20%+ heavier than asks
		ConvictionOrderFlowMaxTokenPrice:    0.42,  // only when token is still "cheap"
		ConvictionOrderFlowMaxAdverseBTCGap: 25.0,  // no fight if BTC >$25 wrong way
		CoinbaseSpreadMinUSD:                0,     // off; set +15 to require US inst. premium for YES OF entry
		Skew25dMinYesBuy:                    0,     // off; set +0.01 to require 1 vol pt bullish skew
		CoinbaseCVDMinBTC:                   0,     // off; set 3.0 to require 3 BTC net Coinbase spot buying in 30s
		// Reversal snipe (disabled by default)
		SnipeEnabled:            false,
		SnipeMinWindowSec:       10,
		SnipeMaxWindowSec:       40,
		SnipeCoinbaseSpreadUSD:  8.0,
		SnipeCVDThresholdBTC:    15.0,
		SnipeMaxNoEntryPrice:    0.75,
		SnipeMaxYesEntryPrice:   0.75,
		SnipeMinNoEntryPrice:    0.25,
		SnipeMinYesEntryPrice:   0.25,
		SnipeMinBTCGapUSD:       10.0,
		SnipeMaxBTCGapUSD:       0,
		SnipeMinWinProb:         0.55,
		SnipeDynamicATRRefUSD:   0,
		SnipeDynamicATRMinScale: 0,
		SnipeDynamicATRMaxScale: 0,
		// Last-second collapse snipe (disabled by default)
		SnipeCollapseEnabled:           false,
		SnipeCollapseMinWindowSec:      4,
		SnipeCollapseMaxWindowSec:      9,
		SnipeCollapseMinTokenPrice:     0.0, // 0 = disabled; set e.g. 0.015 to block 0.01 tokens
		SnipeCollapseMaxTokenPrice:     0.10,
		SnipeCollapseCVDThresholdBTC:   14.0,
		SnipeCollapseCoinbaseSpreadUSD: 15.0,
		SnipeCollapseMinBTCGapUSD:      10.0,
		SnipeCollapseRequireCrossUSD:   0, // 0 = disabled; set e.g. 10 to require within-window cross
		SnipeCollapseMaxBTCGapUSD:      0, // 0 = disabled
		// Deep-discount fade (disabled by default)
		DeepDiscountFadeEnabled:       false,
		DeepDiscountFadeMinWindowSec:  40,
		DeepDiscountFadeMaxWindowSec:  120,
		DeepDiscountFadeMinTokenPrice: 0.02,
		DeepDiscountFadeMaxTokenPrice: 0.05,
		DeepDiscountFadeMinBTCGapUSD:  80.0,
		DeepDiscountFadeMaxBTCGapUSD:  0,
		// Penny-buy (disabled by default)
		PennyBuyEnabled:       false,
		PennyBuyMinWindowSec:  90,
		PennyBuyMaxWindowSec:  240, // 300s window − 60s min elapsed = entries only after 1st minute
		PennyBuyMaxTokenPrice: 0.01,
		PennyBuyMaxBTCGapUSD:  0,
		// Mid-window flip (disabled by default)
		MidFlipEnabled:              false,
		MidFlipMinWindowSec:         40,
		MidFlipMaxWindowSec:         120,
		MidFlipMinTokenPrice:        0.02,
		MidFlipMaxTokenPrice:        0.10,
		MidFlipMinBTCGapUSD:         20.0,
		MidFlipMaxBTCGapUSD:         60.0,
		MidFlipMinWinProb:           0.75,
		MidFlipGapVelocityWindowSec: 30,
		MidFlipMinGapShrinkRateUSD:  1.0,
		// Late flip (disabled by default)
		LateFlipEnabled:              false,
		LateFlipMinWindowSec:         15,
		LateFlipMaxWindowSec:         30,
		LateFlipMinTokenPrice:        0.02,
		LateFlipMaxTokenPrice:        0.15,
		LateFlipMinBTCGapUSD:         10.0,
		LateFlipMaxBTCGapUSD:         0, // 0 = disabled
		LateFlipMinWinProb:           0.70,
		LateFlipMinCoinbaseSpreadUSD: 12.0,
		// Scalp (disabled by default)
		ScalpEnabled:       false,
		ScalpMinWindowSec:  0,
		ScalpMaxWindowSec:  0,
		ScalpMinTokenPrice: 0.55,
		ScalpMaxTokenPrice: 0.75,
		ScalpMinBTCGapUSD:  20.0,
		ScalpMaxBTCGapUSD:  0,
		ScalpMinWinProb:    0.75,
		// DCA+Hedge (disabled by default)
		DCAHedgeMoveTrigger:  0.10,
		DCAHedgeMaxEntrySum:  0.98,
		DCAHedgeBaseShares:   5,
		DCAHedgeMaxShares:    50,
		DCAHedgeDCAReversal:  0.20,
		DCAHedgeDCAAddShares: 5,
	}
}

// priceSample is a timestamped BTC price from Bitstamp or BRTI.
type priceSample struct {
	price float64
	at    time.Time
}

// cvdSample is one aggTrade's net BTC contribution to the rolling Binance CVD window.
// Positive = buyer was the aggressor; negative = seller was the aggressor.
type cvdSample struct {
	buyBTC  float64 // taker aggressor buy volume (IsSellAggressor = false)
	sellBTC float64 // taker aggressor sell volume (IsSellAggressor = true)
	at      time.Time
}

// Detector evaluates real-time data and fires BUY signals when the edge is present.
type Detector struct {
	params Params
	logger *zap.Logger

	mu sync.Mutex

	// Bitstamp rolling samples (lag strategy, fallback when BRTI unavailable)
	btcSamples []priceSample
	// btcGapSamples is a dedicated 90-second retention window of BTC prices used
	// exclusively by EvaluateConviction for gap velocity checks. Kept separate from
	// btcSamples (ObservationWindow=15s) to avoid affecting lag strategy ATR.
	btcGapSamples []priceSample
	// YES token price samples (kept for potential logging; not used for flash detection)
	yesSamples []priceSample

	// Bitstamp orderbook depth state (spoof detection – near-mid)
	lastBidDepthBTC float64   // near-mid bid depth in BTC
	lastAskDepthBTC float64   // near-mid ask depth in BTC
	bidCollapseAt   time.Time // when bid-side depth last collapsed
	askCollapseAt   time.Time // when ask-side depth last collapsed
	latestBTCPrice  float64   // last known Bitstamp BTC price

	// Spread tracking (flash strategy – spread-aware detection)
	peakAboveOpenUSD float64 // max(btcPrice - openPrice) when > 0
	peakBelowOpenUSD float64 // max(openPrice - btcPrice) when > 0
	yesAtPeakAbove   float64 // YES token price when peakAboveOpenUSD was set
	yesAtPeakBelow   float64 // YES token price when peakBelowOpenUSD was set

	// Orderbook depth in the spread zone (between BTC price and open price)
	spreadZoneBidBTC float64   // bids between openPrice and btcPrice (support when BTC > open)
	spreadZoneAskBTC float64   // asks between btcPrice and openPrice (resist when BTC < open)
	peakZoneBidBTC   float64   // peak support depth seen this window
	peakZoneAskBTC   float64   // peak resistance depth seen this window
	zoneBidPulledAt  time.Time // when spread-zone bid support was massively pulled
	zoneAskPulledAt  time.Time // when spread-zone ask resistance was massively pulled

	// Latest values from external feeds
	chainlinkPrice      float64
	bitstampAt          time.Time // when the latest BTC tick used by the detector arrived
	chainlinkAt         time.Time // when we last received a Chainlink update (WS ReceivedAt or RPC poll time)
	polyYesPrice        float64
	polyYesAt           time.Time // when the latest RTDS YES tick arrived
	polyNoPrice         float64
	polyNoAt            time.Time // when the latest RTDS NO tick arrived
	openPrice           float64   // "price to beat" for the current window
	windowEnd           time.Time // end of current 5-min window
	windowOpenAnchorBTC float64   // BTC snapshot captured when the window starts; used to refresh open-relative metrics if the confirmed open arrives late
	gapAtWindowOpen     float64   // btcPrice − openPrice at the moment SetWindow was called
	windowStartedAt     time.Time // current market window start time (derived from windowEnd)
	prevWindowCloseGap  float64   // final BTC-open gap of the previous window (captured at rollover)
	prevWindowClosedAt  time.Time // when previous window context was captured
	preOpenCarryDir     int       // carry direction distilled at window close: +1 YES, -1 NO, 0 none

	// gapHeldSince tracks when |gap| first crossed PairArbMinBTCGapUSD in gapHeldDir.
	// Reset on direction change, gap drop below threshold, or new window.
	gapHeldSince time.Time
	gapHeldDir   int // +1 = YES direction (gap>0), -1 = NO direction (gap<0), 0 = not held

	// sessionBTCOpens is a rolling buffer of the last PairArbSessionTrendBuckets window
	// open prices (Chainlink BTC open at each window start). Used by the session trend
	// filter to detect sustained directional moves and block contra-trend leads.
	// NOT cleared on window change — persists across the trading session.
	sessionBTCOpens []float64

	inPosition   bool
	lastSignalAt time.Time
	lastFlashAt  time.Time

	// blockedUntil prevents any new signal until this time.
	// Set via BlockUntil() after a buy attempt fails to prevent rapid retries.
	blockedUntil time.Time

	// velocityLockedUntil blocks new lag/flash entries after a sudden BTC price spike.
	velocityLastPrice   float64
	velocityLastAt      time.Time
	velocityLockedUntil time.Time

	// Conviction snipe cooldown tracker.
	lastConvictionAt time.Time // last time a conviction signal fired

	// Deribit real-time stream values – fed by OnDeribitStream.
	// When non-zero, dynamically calibrate winProbGaussian sigma
	// instead of the hardcoded 9.0 per-√s coefficient.
	dvolAnnual    float64 // 30-day annualised vol % from DVOL index (e.g. 61.5)
	dvolMarkPrice float64 // BTC-PERPETUAL mark price at the last DVOL update

	// Perpetual microstructure — fed by OnDeribitStream.
	fundingRate      float64 // current 8h funding rate (positive = longs pay shorts; 0 = unknown)
	openInterest     float64 // total BTC-PERPETUAL open interest in USD (0 = unknown)
	prevOpenInterest float64 // previous OI reading — used to compute oiDeltaUSD
	oiDeltaUSD       float64 // OI change vs prev update: +ve = new longs opening; -ve = positions closing

	// Cross-venue spot microstructure.
	coinbasePrice float64     // latest Coinbase BTC-USD spot price (0 = no data yet)
	cbCVDSamples  []cvdSample // rolling 30s Coinbase spot taker aggressor volume (same struct as Binance)
	// Bybit BTCUSDT perpetual OI — independent second venue for OI delta confirmation.
	bybitOI      float64 // latest Bybit open interest in USD (0 = no data)
	bybitOIDelta float64 // Bybit OI change vs prev update
	// Deribit 25-delta risk-reversal skew (positive = call premium = bullish).
	skew25dVol float64 // IV(25Δ call) - IV(25Δ put), updated on each options snapshot

	// Binance cross-exchange order-flow microstructure — fed by OnBinanceTrade.
	// cvdSamples is a rolling 30-second window tracking buy and sell aggressor volume separately.
	cvdSamples []cvdSample

	// brain is the optional online-learning regime classifier. Nil = disabled.
	brain *RegimeBrain

	// DCA+Hedge per-window state.
	// dcaHedgeOpenYes/No capture the first YES/NO tick after window start (window-open baseline).
	// dcaHedgeFiredThisWindow prevents re-entry after a signal fires within the same window.
	// dcaHedgeYesTotalVar/NoTotalVar accumulate sum(|delta|) of each leg's price since window open —
	// used for the tortuosity (swing) filter: total_var / net_rise > threshold means the price
	// oscillated on its way up (swing market) rather than trending straight up.
	// dcaHedgeYesWindowMax/NoWindowMax track the per-window peak of each leg — used for the
	// opp-side-rise filter: if the opp side's peak exceeds open+threshold, the market oscillated.
	dcaHedgeOpenYes         float64
	dcaHedgeOpenNo          float64
	dcaHedgeFiredThisWindow bool
	dcaHedgeYesTotalVar     float64 // running sum of |yesPrice[i] - yesPrice[i-1]| since window open
	dcaHedgeNoTotalVar      float64 // running sum of |noPrice[i] - noPrice[i-1]| since window open
	dcaHedgeYesPrev         float64 // last YES price seen (for delta computation)
	dcaHedgeNoPrev          float64 // last NO price seen (for delta computation)
	dcaHedgeYesWindowMax    float64 // peak YES price seen since window open
	dcaHedgeNoWindowMax     float64 // peak NO price seen since window open
}

// SetBrain attaches a RegimeBrain to the Detector so EvaluatePairArb can gate
// entries on the learned regime score and feed observations back for training.
func (d *Detector) SetBrain(b *RegimeBrain) {
	d.mu.Lock()
	d.brain = b
	d.mu.Unlock()
}

// BlockUntil prevents any new signal from Evaluate() until t has passed.
// Call this after a buy attempt error to impose a hard re-entry blackout.
func (d *Detector) BlockUntil(t time.Time) {
	d.mu.Lock()
	if t.After(d.blockedUntil) {
		d.blockedUntil = t
	}
	d.mu.Unlock()
}

// NoteWin informs the detector that the last close was a scalp WIN (scalp_target or quicksell).
// If PostWinCooldownDuration > 0, extends the re-entry blackout to prevent re-entering
// the same window when the captured edge is already reversing.
func (d *Detector) NoteWin(now time.Time) {
	d.mu.Lock()
	if d.params.PostWinCooldownDuration > 0 {
		until := now.Add(d.params.PostWinCooldownDuration)
		if until.After(d.blockedUntil) {
			d.blockedUntil = until
		}
	}
	d.mu.Unlock()
}

// OnDeribitStream ingests a live tick from the Deribit WebSocket stream.
// All values are the latest known state carried by the StreamTick (zeroed fields
// mean the stream has not yet delivered that data).
func (d *Detector) OnDeribitStream(dvol, markPrice, fundingRate, openInterest float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if dvol > 0 {
		d.dvolAnnual = dvol
	}
	if markPrice > 0 {
		d.dvolMarkPrice = markPrice
	}
	// fundingRate==0 means the stream hasn't sent a value yet; preserve existing.
	if fundingRate != 0 {
		d.fundingRate = fundingRate
	}
	if openInterest > 0 {
		if d.openInterest > 0 {
			d.oiDeltaUSD = openInterest - d.openInterest
		}
		d.openInterest = openInterest
	}
}

// DVOLSnapshot returns the latest DVOL and mark price received from the stream.
func (d *Detector) DVOLSnapshot() (dvolAnnual, markPrice float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dvolAnnual, d.dvolMarkPrice
}

// OnBinanceTrade ingests a single Binance BTCUSDT aggTrade event into the rolling CVD window.
// isSellAggressor = true when the taker was the seller (m=true in Binance wire format).
// Must be called from a single goroutine or with external synchronisation.
func (d *Detector) OnBinanceTrade(isSellAggressor bool, btcQty float64) {
	if btcQty <= 0 {
		return
	}
	s := cvdSample{at: time.Now()}
	if isSellAggressor {
		s.sellBTC = btcQty
	} else {
		s.buyBTC = btcQty
	}
	d.mu.Lock()
	d.cvdSamples = append(d.cvdSamples, s)
	d.evictCVD(s.at)
	d.mu.Unlock()
}

// OnCoinbaseTrade ingests a Coinbase BTC-USD spot trade.
// price: execution price in USD.
// qty: trade size in BTC (0 is accepted but produces no CVD contribution).
// isSell: true when the taker was the seller (hit the bid); false = buyer lifted the ask.
//
// Maintains two independent signals:
//  1. Latest spot price → Coinbase–Binance spread (institutional premium).
//  2. Rolling 30s CVD on Coinbase → cross-venue confirmation of Binance aggressive flow.
func (d *Detector) OnCoinbaseTrade(price float64, qty float64, isSell bool) {
	if price <= 0 {
		return
	}
	d.mu.Lock()
	d.coinbasePrice = price
	if qty > 0 {
		s := cvdSample{at: time.Now()}
		if isSell {
			s.sellBTC = qty
		} else {
			s.buyBTC = qty
		}
		d.cbCVDSamples = append(d.cbCVDSamples, s)
		d.evictCBCVD(s.at)
	}
	d.mu.Unlock()
}

// OnBybitOI ingests a Bybit BTCUSDT ticker update containing open interest in USD.
// Provides a second independent OI venue alongside Deribit for OI-delta analysis.
func (d *Detector) OnBybitOI(oiUSD float64) {
	if oiUSD <= 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.bybitOI > 0 {
		d.bybitOIDelta = oiUSD - d.bybitOI
	}
	d.bybitOI = oiUSD
}

// OnDeribitSkew25d ingests the Deribit 25-delta risk-reversal skew computed from
// the options vol surface (call_IV_25d - put_IV_25d in decimal vol units).
// Positive = call premium = options market paying for upside = bullish leading signal.
func (d *Detector) OnDeribitSkew25d(skewVol float64) {
	d.mu.Lock()
	d.skew25dVol = skewVol
	d.mu.Unlock()
}

// computeSigmaCoeff returns the per-√second BTC sigma in USD for use in the
// Gaussian win-probability model. Uses live DVOL when available; falls back to
// the hardcoded 9.0 calibration (≈ 77% annualised vol at $65 k).
//
// Must be called with d.mu held (reads dvolAnnual / dvolMarkPrice directly).
func computeSigmaCoeff(dvolAnnual, dvolMarkPrice float64) float64 {
	const defaultCoeff = 9.0 // calibrated empirically for 5-min BTC windows
	const secsPerYear = 365.25 * 24 * 3600
	if dvolAnnual > 0 && dvolMarkPrice > 0 {
		coeff := (dvolAnnual / 100.0) * dvolMarkPrice / math.Sqrt(secsPerYear)
		if coeff > 1.0 { // sanity floor: never below $1/√s
			return coeff
		}
	}
	return defaultCoeff
}

// winProbGaussian returns the Gaussian probability that BTC remains on its current
// side of the open price for the rest of the window.
//
// Model: sigma = 9.0 * sqrt(windowRemSec) — BTC intra-window volatility (USD/sqrt-sec).
func winProbGaussian(gapUSD, windowRemSec float64) float64 {
	return winProbGaussianSigma(gapUSD, windowRemSec, 9.0)
}

// winProbGaussianSigma is the same model with a caller-supplied sigma coefficient.
// sigmaCoeff is in USD/√second (e.g. 9.0 means σ = $9 per √s of remaining time).
func winProbGaussianSigma(gapUSD, windowRemSec, sigmaCoeff float64) float64 {
	if windowRemSec <= 0 {
		if gapUSD > 0 {
			return 1.0
		}
		return 0.0
	}
	sigma := sigmaCoeff * math.Sqrt(windowRemSec)
	z := gapUSD / sigma
	return 0.5 * (2.0 - math.Erfc(z/math.Sqrt2))
}

// updateVelocityLockout arms the velocity lockout if the latest price constitutes
// a micro flash-crash spike. Must be called with mu held.
func (d *Detector) updateVelocityLockout(price float64, at time.Time) {
	if d.params.VelocityLockoutUSD <= 0 {
		return
	}
	if d.velocityLastPrice > 0 {
		delta := math.Abs(price - d.velocityLastPrice)
		window := time.Duration(d.params.VelocityLockoutWindowMs) * time.Millisecond
		if delta >= d.params.VelocityLockoutUSD && at.Sub(d.velocityLastAt) < window {
			lockUntil := at.Add(time.Duration(d.params.VelocityLockoutSec) * time.Second)
			if lockUntil.After(d.velocityLockedUntil) {
				d.velocityLockedUntil = lockUntil
				d.logger.Info("velocity lockout triggered",
					zap.Float64("delta_usd", delta),
					zap.Duration("elapsed", at.Sub(d.velocityLastAt)),
					zap.Time("locked_until", lockUntil),
				)
			}
		}
	}
	d.velocityLastPrice = price
	d.velocityLastAt = at
}

// NewDetector creates a Detector with the given parameters.
func NewDetector(params Params, logger *zap.Logger) *Detector {
	return &Detector{
		params: params,
		logger: logger,
	}
}

// SetWindow updates the current market window's end time and open price.
// Must be called whenever a new 5-minute window starts.
// SetWindowStart overrides the legacy derived start time with the
// exchange's authoritative market open timestamp.
func (d *Detector) SetWindowStart(windowStart time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !windowStart.IsZero() {
		d.windowStartedAt = windowStart
	}
}

func (d *Detector) SetWindow(openPrice float64, windowEnd time.Time) {
	d.mu.Lock()
	if d.openPrice > 0 && d.latestBTCPrice > 0 && !d.windowEnd.IsZero() {
		d.prevWindowCloseGap = d.latestBTCPrice - d.openPrice
		d.prevWindowClosedAt = time.Now()
	}
	d.windowOpenAnchorBTC = d.latestBTCPrice
	d.openPrice = openPrice
	d.windowEnd = windowEnd
	// Session trend: record this window's BTC open price in the rolling buffer.
	if d.params.PairArbSessionTrendFilterUSD > 0 && openPrice > 0 {
		maxBuckets := d.params.PairArbSessionTrendBuckets
		if maxBuckets <= 0 {
			maxBuckets = 6
		}
		d.sessionBTCOpens = append(d.sessionBTCOpens, openPrice)
		if len(d.sessionBTCOpens) > maxBuckets {
			d.sessionBTCOpens = d.sessionBTCOpens[len(d.sessionBTCOpens)-maxBuckets:]
		}
	}
	// Anchor elapsed-window gates to real market time, not SetWindow call time.
	// BTC 5m markets have fixed 300s windows.
	d.windowStartedAt = windowEnd.Add(-5 * time.Minute)
	d.chainlinkPrice = openPrice // seed with open until first live update arrives
	d.bitstampAt = time.Time{}
	d.chainlinkAt = time.Time{}
	d.polyYesPrice = 0
	d.polyYesAt = time.Time{}
	d.polyNoPrice = 0
	d.polyNoAt = time.Time{}
	// Record the BTC gap at window open so collapse snipe can require a within-window cross.
	if d.windowOpenAnchorBTC != 0 {
		d.gapAtWindowOpen = d.windowOpenAnchorBTC - openPrice
	} else {
		d.gapAtWindowOpen = 0
	}
	// Each new window is a fresh market contract. Any conviction position from
	// the prior window is for a different token and should already be cleared
	// by OnPolyPrice's stale-position guard. Reset inPosition here as a safety
	// net so a failed close in the prior window can't block the entire session.
	d.inPosition = false
	d.btcSamples = d.btcSamples[:0]
	// Pre-open carry: sample the closing window's last ticks for direction BEFORE wiping buffers.
	if d.params.PairArbPreOpenEnabled && d.params.PairArbPreOpenSrcWindowSec > 0 &&
		d.openPrice > 0 && len(d.btcGapSamples) > 0 {
		d.preOpenCarryDir = d.computePreOpenCarryDir()
	} else {
		d.preOpenCarryDir = 0
	}
	d.btcGapSamples = d.btcGapSamples[:0]
	d.yesSamples = d.yesSamples[:0]
	d.blockedUntil = time.Time{} // reset blackout on new window
	d.lastBidDepthBTC = 0
	d.lastAskDepthBTC = 0
	d.bidCollapseAt = time.Time{}
	d.askCollapseAt = time.Time{}
	d.latestBTCPrice = 0
	d.peakAboveOpenUSD = 0
	d.peakBelowOpenUSD = 0
	d.yesAtPeakAbove = 0
	d.yesAtPeakBelow = 0
	d.spreadZoneBidBTC = 0
	d.spreadZoneAskBTC = 0
	d.peakZoneBidBTC = 0
	d.peakZoneAskBTC = 0
	d.zoneBidPulledAt = time.Time{}
	d.zoneAskPulledAt = time.Time{}
	d.gapHeldSince = time.Time{}
	d.gapHeldDir = 0
	d.dcaHedgeOpenYes = 0
	d.dcaHedgeOpenNo = 0
	d.dcaHedgeFiredThisWindow = false
	d.dcaHedgeYesTotalVar = 0
	d.dcaHedgeNoTotalVar = 0
	d.dcaHedgeYesPrev = 0
	d.dcaHedgeNoPrev = 0
	d.dcaHedgeYesWindowMax = 0
	d.dcaHedgeNoWindowMax = 0
	d.mu.Unlock()
	d.logger.Info("detector: window updated",
		zap.Float64("open_price", openPrice),
		zap.Time("window_end", windowEnd),
		zap.Duration("remaining", time.Until(windowEnd).Round(time.Second)),
	)
}

// UpdateWindowOpenPrice replaces the current window's reference open price without
// resetting the rest of the detector state. This is used when the bot seeds a
// new window with a provisional open and the authoritative Polymarket open price
// arrives a few seconds later.
func (d *Detector) UpdateWindowOpenPrice(openPrice float64, windowEnd time.Time) (previous float64, updated bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.windowEnd.IsZero() || !d.windowEnd.Equal(windowEnd) || openPrice <= 0 {
		return 0, false
	}
	previous = d.openPrice
	d.openPrice = openPrice
	if d.chainlinkAt.IsZero() {
		d.chainlinkPrice = openPrice
	}
	if d.windowOpenAnchorBTC != 0 {
		d.gapAtWindowOpen = d.windowOpenAnchorBTC - openPrice
	} else {
		d.gapAtWindowOpen = 0
	}
	return previous, true
}

// computePreOpenCarryDir inspects the last PairArbPreOpenSrcWindowSec seconds of
// btcGapSamples and returns the carry direction for the next market window.
// Returns +1 (YES), -1 (NO), or 0 (no clear signal).
// Caller must hold d.mu and must call this BEFORE btcGapSamples is wiped.
func (d *Detector) computePreOpenCarryDir() int {
	if d.openPrice <= 0 || len(d.btcGapSamples) == 0 {
		return 0
	}
	srcSec := float64(d.params.PairArbPreOpenSrcWindowSec)
	minGap := d.params.PairArbPreOpenMinGapUSD
	if minGap <= 0 {
		minGap = 1.0
	}
	cutoff := time.Now().Add(-time.Duration(float64(time.Second) * srcSec))
	var pos, neg, total int
	var lastGap float64
	for _, s := range d.btcGapSamples {
		if s.at.Before(cutoff) {
			continue
		}
		g := s.price - d.openPrice
		total++
		if g >= minGap {
			pos++
		} else if g <= -minGap {
			neg++
		}
		lastGap = g
	}
	if total == 0 {
		return 0
	}
	if float64(pos)/float64(total) >= 0.8 && lastGap >= minGap {
		return 1
	}
	if float64(neg)/float64(total) >= 0.8 && lastGap <= -minGap {
		return -1
	}
	return 0
}

// ComputePreOpenCarryDirNow returns the carry direction (+1 YES, -1 NO, 0 none)
// computed from the current window's live BTC gap samples. Unlike computePreOpenCarryDir
// which is called during SetWindow, this method acquires the lock and can be called
// at any point while the current window is still active (e.g. by the true pre-open
// goroutine a few seconds before win.End).
func (d *Detector) ComputePreOpenCarryDirNow() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.computePreOpenCarryDir()
}

// EvaluatePreOpenCarry checks whether a pre-market-open carry entry should fire.
// It returns a signal in the first PairArbPreOpenEntrySec seconds of a new window
// when the previous window closed with a strong directional bias and the current
// lead token is in the pre-open price range. No gap/CVD/velocity filters apply.
// The carry direction is consumed after the first signal so it fires at most once.
func (d *Detector) EvaluatePreOpenCarry() Signal {
	if !d.params.PairArbPreOpenEnabled {
		return Signal{Type: SignalNone}
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.inPosition || d.preOpenCarryDir == 0 {
		return Signal{Type: SignalNone}
	}
	now := time.Now()
	if !d.blockedUntil.IsZero() && now.Before(d.blockedUntil) {
		return Signal{Type: SignalNone}
	}
	if d.windowEnd.IsZero() || d.windowStartedAt.IsZero() {
		return Signal{Type: SignalNone}
	}
	elapsedSec := now.Sub(d.windowStartedAt).Seconds()
	maxEntry := float64(d.params.PairArbPreOpenEntrySec)
	if maxEntry <= 0 {
		maxEntry = 5
	}
	if elapsedSec > maxEntry {
		// Window for pre-open entry has passed; discard signal.
		d.preOpenCarryDir = 0
		return Signal{Type: SignalNone}
	}

	yesPrice := d.polyYesPrice
	noPrice := d.polyNoPrice
	if yesPrice <= 0 || noPrice <= 0 {
		return Signal{Type: SignalNone}
	}

	minTok := d.params.PairArbPreOpenMinTokenPrice
	maxTok := d.params.PairArbPreOpenMaxTokenPrice
	if minTok <= 0 {
		minTok = 0.46
	}
	if maxTok <= 0 {
		maxTok = 0.54
	}

	sigType := SignalPairArbPreOpenNo
	tokenPrice := noPrice
	if d.preOpenCarryDir == 1 {
		sigType = SignalPairArbPreOpenYes
		tokenPrice = yesPrice
	}
	if tokenPrice < minTok || tokenPrice > maxTok {
		return Signal{Type: SignalNone}
	}

	windowRemSec := d.windowEnd.Sub(now).Seconds()
	// Consume the carry signal so we only fire once per window.
	d.preOpenCarryDir = 0

	return Signal{
		Type:            sigType,
		BitstampPrice:   d.latestBTCPrice,
		ChainlinkPrice:  d.chainlinkPrice,
		OpenPrice:       d.openPrice,
		PolyYesPrice:    yesPrice,
		PolyNoPrice:     noPrice,
		WindowRemaining: windowRemSec,
		At:              now,
	}
}

// OnBitstampTrade ingests a new BTC/USD price from Bitstamp.
func (d *Detector) OnBitstampTrade(price float64, at time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.btcSamples = append(d.btcSamples, priceSample{price, at})
	d.btcGapSamples = append(d.btcGapSamples, priceSample{price, at})
	d.latestBTCPrice = price
	d.bitstampAt = at
	d.evictBTC(at)
	d.evictBTCGap(at)
	d.updateVelocityLockout(price, at)

	// Track peak BTC-vs-open spread for flash strategy
	if d.openPrice > 0 {
		if above := price - d.openPrice; above > d.peakAboveOpenUSD {
			d.peakAboveOpenUSD = above
			d.yesAtPeakAbove = d.polyYesPrice
		}
		if below := d.openPrice - price; below > d.peakBelowOpenUSD {
			d.peakBelowOpenUSD = below
			if d.brain != nil {
				d.brain.MaybeLabel(price, at)
			}

			if d.params.PairArbEnabled && d.params.PairArbMinGapHoldSec > 0 && d.openPrice > 0 {
				gapNow := d.latestBTCPrice - d.openPrice
				absGapNow := math.Abs(gapNow)
				dir := 0
				if gapNow > 0 {
					dir = 1
				} else if gapNow < 0 {
					dir = -1
				}
				if absGapNow >= d.params.PairArbMinBTCGapUSD && dir != 0 {
					if dir != d.gapHeldDir {
						d.gapHeldSince = at
						d.gapHeldDir = dir
					}
				} else {
					d.gapHeldSince = time.Time{}
					d.gapHeldDir = 0
				}
			}
			d.yesAtPeakBelow = d.polyYesPrice
		}
	}
}

// OnChainlinkPrice ingests the latest Chainlink BTC/USD price from the activity WS.
func (d *Detector) OnChainlinkPrice(price float64, at ...time.Time) {
	d.mu.Lock()
	d.chainlinkPrice = price
	if len(at) > 0 && !at[0].IsZero() {
		d.chainlinkAt = at[0]
	}
	d.mu.Unlock()
}

// OnPolyPrice ingests the latest YES token price from the Polymarket RTDS socket.
func (d *Detector) OnPolyPrice(price float64) {
	d.OnPolyYesPrice(price, time.Now())
}

// OnPolyYesPrice ingests the latest YES token price from the Polymarket RTDS socket.
func (d *Detector) OnPolyYesPrice(price float64, at time.Time) {
	d.mu.Lock()
	d.polyYesPrice = price
	if at.IsZero() {
		at = time.Now()
	}
	d.polyYesAt = at
	// DCA+Hedge: capture the first YES price tick after each window reset as the
	// window-open baseline. SetWindow() clears dcaHedgeOpenYes to 0 so this fires once.
	if d.dcaHedgeOpenYes == 0 && price > 0 {
		d.dcaHedgeOpenYes = price
		d.dcaHedgeYesPrev = price
		d.dcaHedgeYesWindowMax = price
	} else if d.dcaHedgeYesPrev > 0 && price > 0 {
		// Accumulate total path variation for tortuosity (swing) filter.
		delta := price - d.dcaHedgeYesPrev
		if delta < 0 {
			delta = -delta
		}
		d.dcaHedgeYesTotalVar += delta
		d.dcaHedgeYesPrev = price
		if price > d.dcaHedgeYesWindowMax {
			d.dcaHedgeYesWindowMax = price
		}
	}
	d.mu.Unlock()
}

// OnPolyNoPrice ingests the latest NO token price from the Polymarket RTDS socket.
func (d *Detector) OnPolyNoPrice(price float64, at time.Time) {
	d.mu.Lock()
	d.polyNoPrice = price
	if at.IsZero() {
		at = time.Now()
	}
	d.polyNoAt = at
	// DCA+Hedge: capture the first NO price tick after each window reset.
	if d.dcaHedgeOpenNo == 0 && price > 0 {
		d.dcaHedgeOpenNo = price
		d.dcaHedgeNoPrev = price
		d.dcaHedgeNoWindowMax = price
	} else if d.dcaHedgeNoPrev > 0 && price > 0 {
		// Accumulate total path variation for tortuosity (swing) filter.
		delta := price - d.dcaHedgeNoPrev
		if delta < 0 {
			delta = -delta
		}
		d.dcaHedgeNoTotalVar += delta
		d.dcaHedgeNoPrev = price
		if price > d.dcaHedgeNoWindowMax {
			d.dcaHedgeNoWindowMax = price
		}
	}
	d.mu.Unlock()
}

// OnPolyPriceSample ingests a YES price tick into the flash-reversal rolling window.
// Call this on every RTDS YES price event (in addition to OnPolyPrice).
func (d *Detector) OnPolyPriceSample(price float64, at time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.yesSamples = append(d.yesSamples, priceSample{price, at})
	d.polyYesAt = at
	d.evictYES(at)
}

// SetInPosition updates whether the bot currently holds an open position.
func (d *Detector) SetInPosition(v bool) {
	d.mu.Lock()
	d.inPosition = v
	d.mu.Unlock()
}

// WindowEnd returns the current window end time (zero value if not set).
func (d *Detector) WindowEnd() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.windowEnd
}

// OpenPrice returns the "price to beat" for the current window (0 if not set).
func (d *Detector) OpenPrice() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.openPrice
}

// Snapshot returns key state values atomically for logging/status output.
// PairArbEntryLimits returns the configured elapsed-time and gap thresholds used
// to gate pair-arb entries. minWindowSec / maxWindowSec are elapsed-seconds bounds;
// minGapUSD / maxGapUSD are |BTC - openPrice| bounds.
func (d *Detector) PairArbEntryLimits() (minWindowSec, maxWindowSec int, minGapUSD, maxGapUSD float64) {
	return d.params.PairArbMinWindowSec, d.params.PairArbMaxWindowSec,
		d.params.PairArbMinBTCGapUSD, d.params.PairArbMaxBTCGapUSD
}

// WindowElapsedSec returns the number of seconds elapsed since the current window
// started, based on the tracked windowStartedAt timestamp. Returns 0 if the window
// start is not yet known. This is more reliable than deriving elapsed time from the
// window-end timestamp heuristic (pairWindowDuration), which can misidentify 5-minute
// windows that end exactly on an hour boundary as 1-hour windows.
func (d *Detector) WindowElapsedSec() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.windowStartedAt.IsZero() {
		return 0
	}
	return time.Since(d.windowStartedAt).Seconds()
}

func (d *Detector) Snapshot() (btcLatest, clPrice, yesPrice, openPrice, windowRem, clAgeSec float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	btcLatest = d.latestBTCPrice
	if btcLatest <= 0 && d.dvolMarkPrice > 0 {
		// Deribit SpotC can briefly lag while Mark price ticks continue.
		// Use mark as a safe fallback so status/dashboard don't collapse to BTC=0.
		btcLatest = d.dvolMarkPrice
	}
	clPrice = d.chainlinkPrice
	yesPrice = d.polyYesPrice
	openPrice = d.openPrice
	windowRem = time.Until(d.windowEnd).Seconds()
	if !d.chainlinkAt.IsZero() {
		clAgeSec = time.Since(d.chainlinkAt).Seconds()
	}
	return
}

// ATRSnapshot returns the current average tick-to-tick BTC move (ATR) computed
// from the rolling observation window. Uses BRTI samples when available, otherwise
// falls back to Bitstamp. Safe to call concurrently.
func (d *Detector) ATRSnapshot() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return computeATR(d.btcSamples)
}

// BookDepthSnapshot returns the current near-mid orderbook depth + recent BTC
// price range (max-min over the observation window). All under mu.
func (d *Detector) BookDepthSnapshot() (bidDepthBTC, askDepthBTC, recentRangeUSD float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	bidDepthBTC = d.lastBidDepthBTC
	askDepthBTC = d.lastAskDepthBTC
	if len(d.btcSamples) >= 2 {
		lo, hi := d.btcSamples[0].price, d.btcSamples[0].price
		for _, s := range d.btcSamples[1:] {
			if s.price < lo {
				lo = s.price
			}
			if s.price > hi {
				hi = s.price
			}
		}
		recentRangeUSD = hi - lo
	}
	return
}

// ── ConvictionSnapshot ────────────────────────────────────────────────────────

// ConvictionSnapshot holds a read-only view of every conviction gate's current
// value plus a Blocker string naming the first gate that is not satisfied.
// Blocker == "" means every gate passes and a signal could fire right now.
// ConvictionSnapshot holds a read-only view of every conviction gate.
// All fields carry JSON tags so the struct can be serialized directly to
// signals.jsonl for offline parameter-sweep analysis (--signals flag).
type ConvictionSnapshot struct {
	At                    time.Time `json:"at"`
	Enabled               bool      `json:"enabled"`
	WindowRem             float64   `json:"window_rem_sec"` // seconds remaining in the current market window
	BTCPrice              float64   `json:"btc_price"`
	OpenPrice             float64   `json:"open_price"`
	GapUSD                float64   `json:"gap_usd"`  // BTC − open (positive = YES direction)
	WinProb               float64   `json:"win_prob"` // Gaussian win probability [0..1]
	YesPrice              float64   `json:"yes_price"`
	NoPrice               float64   `json:"no_price"`
	Blocker               string    `json:"blocker,omitempty"`                  // first failing gate, or "" when all gates pass and signal fires
	CooldownRem           float64   `json:"cooldown_rem_sec,omitempty"`         // seconds of conviction cooldown remaining
	DVOL                  float64   `json:"dvol,omitempty"`                     // 30-day annualised implied vol from Deribit (%, 0 = not connected)
	SigmaCoeff            float64   `json:"sigma_coeff"`                        // per-√s sigma coefficient used for WinProb
	FundingRate           float64   `json:"funding_rate,omitempty"`             // current BTC-PERPETUAL 8h funding rate (0 = not yet received)
	OpenInterest          float64   `json:"open_interest_usd,omitempty"`        // BTC-PERPETUAL OI in USD
	BitstampAgeSec        float64   `json:"bitstamp_age_sec,omitempty"`         // age of the latest BTC tick consumed by the detector
	ChainlinkAgeSec       float64   `json:"chainlink_age_sec,omitempty"`        // age of the latest Chainlink BTC/USD sample
	RTDSAgeSec            float64   `json:"rtds_age_sec,omitempty"`             // age of the latest RTDS YES price tick
	YesDriftCents         float64   `json:"yes_drift_cents,omitempty"`          // YES price change (cents) over last FlashWindow (negative = falling)
	CVDBuyBTC             float64   `json:"cvd_btc,omitempty"`                  // Binance net aggressor BTC in last 30s (+ve = buyers)
	BookImbalance         float64   `json:"book_imbalance,omitempty"`           // Deribit near-mid (bidBTC-askBTC)/(bidBTC+askBTC)
	GapVelocity           float64   `json:"gap_velocity_usd_per_sec,omitempty"` // USD/s rate of change of gap: positive = widening; negative = collapsing toward open
	TakerBuyRatio         float64   `json:"taker_buy_ratio,omitempty"`          // rolling 30s taker_buy_vol/total_vol: >0.65 = aggressive ask-sweeping
	OIDeltaUSD            float64   `json:"oi_delta_usd,omitempty"`             // Deribit OI change vs prev update: +ve = new positions opening; -ve = de-leveraging
	CoinbaseBinanceSpread float64   `json:"coinbase_spread_usd,omitempty"`      // Coinbase BTC-USD minus BTC ref price: +ve = US institutional spot premium
	CoinbaseCVDBTC        float64   `json:"coinbase_cvd_btc,omitempty"`         // Coinbase spot 30s net-aggressor BTC (+ve = institutional buying, -ve = distribution)
	BybitOIDeltaUSD       float64   `json:"bybit_oi_delta_usd,omitempty"`       // Bybit OI change vs prev: +ve = new longs; -ve = de-leveraging
	Skew25dVol            float64   `json:"skew_25d_vol,omitempty"`             // Deribit 25Δ RR skew in decimal vol: +ve = call prem (bullish), -ve = put prem (bearish)
	FeeRateBps            string    `json:"fee_rate_bps,omitempty"`
	// Resolved is set on rows where the outcome is known: "YES" when yes_price ≥ 0.99,
	// "NO" when yes_price ≤ 0.01. All rows in the same window (same open_price) share
	// this outcome on the final tick(s), enabling direct supervised ML labeling.
	Resolved string `json:"resolved,omitempty"`
}

// StrategyGateStatus summarizes whether an enabled strategy could enter right now
// and, if not, which first blocker is stopping it.
type StrategyGateStatus struct {
	Key     string
	Name    string
	Status  string
	Ready   bool
	Blocker string
}

// ConvictionSnapshot returns a consistent read-only snapshot of all conviction
// gate values without modifying detector state. Safe to call every tick.
func (d *Detector) ConvictionSnapshot() ConvictionSnapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()

	snap := ConvictionSnapshot{
		At:        now,
		Enabled:   d.params.ConvictionEnabled,
		BTCPrice:  d.latestBTCPrice,
		OpenPrice: d.openPrice,
		YesPrice:  d.polyYesPrice,
		NoPrice:   d.polyNoPrice,
	}
	if snap.NoPrice <= 0 && snap.YesPrice > 0 {
		snap.NoPrice = math.Round((1.0-snap.YesPrice)*100) / 100
	}
	if !d.windowEnd.IsZero() {
		snap.WindowRem = d.windowEnd.Sub(now).Seconds()
	}
	if snap.BTCPrice > 0 && snap.OpenPrice > 0 {
		snap.GapUSD = snap.BTCPrice - snap.OpenPrice
	}
	if !d.bitstampAt.IsZero() {
		snap.BitstampAgeSec = now.Sub(d.bitstampAt).Seconds()
	}
	if !d.chainlinkAt.IsZero() {
		snap.ChainlinkAgeSec = now.Sub(d.chainlinkAt).Seconds()
	}
	if !d.polyYesAt.IsZero() {
		snap.RTDSAgeSec = now.Sub(d.polyYesAt).Seconds()
	}
	snap.DVOL = d.dvolAnnual
	snap.SigmaCoeff = computeSigmaCoeff(d.dvolAnnual, d.dvolMarkPrice)
	snap.FundingRate = d.fundingRate
	snap.OpenInterest = d.openInterest
	snap.YesDriftCents = d.yesDrift() * 100
	snap.CVDBuyBTC = d.cvdBTC()
	snap.BookImbalance = d.bookImbalance()
	d.evictCBCVD(time.Now())
	snap.TakerBuyRatio = d.takerBuyRatio()
	snap.OIDeltaUSD = d.oiDeltaUSD
	if d.coinbasePrice > 0 && d.latestBTCPrice > 0 {
		snap.CoinbaseBinanceSpread = d.coinbasePrice - d.latestBTCPrice
	}
	snap.CoinbaseCVDBTC = d.cbCVDBTC()
	snap.BybitOIDeltaUSD = d.bybitOIDelta
	snap.Skew25dVol = d.skew25dVol
	snap.GapVelocity = d.btcGapVelocityUSDPerSec(float64(d.params.ConvictionGapVelocityWindowSec))
	if snap.BTCPrice > 0 && snap.OpenPrice > 0 {
		snap.WinProb = winProbGaussianSigma(math.Abs(snap.GapUSD), snap.WindowRem, snap.SigmaCoeff)
	}

	// Stamp resolution only in the final second of the window, when the price
	// reflects the true outcome and not a mid-window extreme.
	if snap.WindowRem < 1 {
		if snap.YesPrice >= 0.99 {
			snap.Resolved = "YES"
		} else if snap.YesPrice > 0 && snap.YesPrice <= 0.01 {
			snap.Resolved = "NO"
		}
	}

	switch {
	case !d.params.ConvictionEnabled:
		snap.Blocker = "disabled"
	case d.inPosition:
		snap.Blocker = "in_position"
	case !d.blockedUntil.IsZero() && now.Before(d.blockedUntil):
		snap.Blocker = fmt.Sprintf("blocked(%.0fs)", d.blockedUntil.Sub(now).Seconds())
	case !d.lastConvictionAt.IsZero() && now.Sub(d.lastConvictionAt) < d.params.ConvictionCooldownDuration:
		rem := d.params.ConvictionCooldownDuration - now.Sub(d.lastConvictionAt)
		snap.CooldownRem = rem.Seconds()
		snap.Blocker = fmt.Sprintf("cooldown(%.0fs)", rem.Seconds())
	case d.openPrice == 0 || d.windowEnd.IsZero():
		snap.Blocker = "no_window"
	case snap.WindowRem < 0:
		snap.Blocker = "window_ended"
	// This case only triggers when override does NOT allow entry:
	// either the edge is not big enough / prob not high enough, or window < absolute floor.
	case snap.WindowRem < float64(d.params.ConvictionMinWindowSec) &&
		!(d.params.ConvictionHighEdgeMinUSD > 0 &&
			math.Abs(snap.GapUSD) >= d.params.ConvictionHighEdgeMinUSD &&
			snap.WinProb >= d.params.ConvictionHighEdgeProbThreshold &&
			snap.WindowRem >= float64(d.params.ConvictionHighEdgeMinWindowSec)):
		snap.Blocker = fmt.Sprintf("win<%.0fs(need %d-%ds)", snap.WindowRem, d.params.ConvictionMinWindowSec, d.params.ConvictionMaxWindowSec)
	case snap.WindowRem > float64(d.params.ConvictionMaxWindowSec):
		snap.Blocker = fmt.Sprintf("win>%.0fs(need %d-%ds)", snap.WindowRem, d.params.ConvictionMinWindowSec, d.params.ConvictionMaxWindowSec)
	case math.Abs(snap.GapUSD) < d.params.ConvictionMinBTCGapUSD:
		snap.Blocker = fmt.Sprintf("gap=$%.0f<$%.0f", math.Abs(snap.GapUSD), d.params.ConvictionMinBTCGapUSD)
	case snap.WinProb < d.params.ConvictionMinWinProb:
		snap.Blocker = fmt.Sprintf("prob=%.1f%%<%.0f%%", snap.WinProb*100, d.params.ConvictionMinWinProb*100)
	case d.params.FundingRateMinYesBuy != 0 && snap.GapUSD > 0 && d.fundingRate != 0 && d.fundingRate < d.params.FundingRateMinYesBuy:
		snap.Blocker = fmt.Sprintf("fund=%.4f%%<min(%.4f%%)", d.fundingRate*100, d.params.FundingRateMinYesBuy*100)
	case d.params.FundingRateMaxNoBuy != 0 && snap.GapUSD < 0 && d.fundingRate != 0 && d.fundingRate > d.params.FundingRateMaxNoBuy:
		snap.Blocker = fmt.Sprintf("fund=%.4f%%>max(%.4f%%)", d.fundingRate*100, d.params.FundingRateMaxNoBuy*100)
	case d.params.ConvictionMaxYesDriftCents > 0 && snap.GapUSD > 0 && snap.YesDriftCents < -d.params.ConvictionMaxYesDriftCents:
		snap.Blocker = fmt.Sprintf("yes_drift=%.1fc↓ (limit -%.0fc)", snap.YesDriftCents, d.params.ConvictionMaxYesDriftCents)
	case d.params.ConvictionMaxYesDriftCents > 0 && snap.GapUSD < 0 && snap.YesDriftCents > d.params.ConvictionMaxYesDriftCents:
		snap.Blocker = fmt.Sprintf("yes_drift=+%.1fc↑ (limit +%.0fc)", snap.YesDriftCents, d.params.ConvictionMaxYesDriftCents)
	case d.params.CVDMinDivergenceBTC > 0 && snap.GapUSD > 0 && snap.CVDBuyBTC < -d.params.CVDMinDivergenceBTC:
		snap.Blocker = fmt.Sprintf("cvd=%.1f BTC↓ (limit -%.0f)", snap.CVDBuyBTC, d.params.CVDMinDivergenceBTC)
	case d.params.CVDMinDivergenceBTC > 0 && snap.GapUSD < 0 && snap.CVDBuyBTC > d.params.CVDMinDivergenceBTC:
		snap.Blocker = fmt.Sprintf("cvd=+%.1f BTC↑ (limit +%.0f)", snap.CVDBuyBTC, d.params.CVDMinDivergenceBTC)
	case d.params.ConvictionMaxGapShrinkRateUSD > 0 && d.params.ConvictionGapVelocityWindowSec > 0 &&
		snap.GapUSD > 0 && snap.GapVelocity < -d.params.ConvictionMaxGapShrinkRateUSD:
		snap.Blocker = fmt.Sprintf("gap_vel=%.1f$/s↑ (shrinking>%.1f)", snap.GapVelocity, d.params.ConvictionMaxGapShrinkRateUSD)
	case d.params.ConvictionMaxGapShrinkRateUSD > 0 && d.params.ConvictionGapVelocityWindowSec > 0 &&
		snap.GapUSD < 0 && snap.GapVelocity > d.params.ConvictionMaxGapShrinkRateUSD:
		snap.Blocker = fmt.Sprintf("gap_vel=+%.1f$/s↓ (shrinking>%.1f)", snap.GapVelocity, d.params.ConvictionMaxGapShrinkRateUSD)
	case d.params.ConvictionMinGapGrowthRateUSD > 0 && d.params.ConvictionGapVelocityWindowSec > 0 &&
		snap.GapUSD > 0 && snap.GapVelocity < d.params.ConvictionMinGapGrowthRateUSD:
		snap.Blocker = fmt.Sprintf("gap_momentum=+%.2f$/s (need>+%.1f, btc stale)", snap.GapVelocity, d.params.ConvictionMinGapGrowthRateUSD)
	case d.params.ConvictionMinGapGrowthRateUSD > 0 && d.params.ConvictionGapVelocityWindowSec > 0 &&
		snap.GapUSD < 0 && snap.GapVelocity > -d.params.ConvictionMinGapGrowthRateUSD:
		snap.Blocker = fmt.Sprintf("gap_momentum=-%.2f$/s (need<-%.1f, btc stale)", snap.GapVelocity, d.params.ConvictionMinGapGrowthRateUSD)
	case d.params.BookImbalanceMin > 0 && snap.GapUSD > 0 && snap.BookImbalance < -d.params.BookImbalanceMin:
		snap.Blocker = fmt.Sprintf("book_imb=%.2f↓ (need>-%.2f)", snap.BookImbalance, d.params.BookImbalanceMin)
	case d.params.BookImbalanceMin > 0 && snap.GapUSD < 0 && snap.BookImbalance > d.params.BookImbalanceMin:
		snap.Blocker = fmt.Sprintf("book_imb=+%.2f↑ (need<+%.2f)", snap.BookImbalance, d.params.BookImbalanceMin)
	default:
		// Compute effective token price cap (same logic as EvaluateConviction).
		snapEffectiveMaxToken := d.params.ConvictionMaxTokenPrice
		if snap.GapUSD > 0 {
			if snap.YesPrice < d.params.ConvictionMinTokenPrice || snap.YesPrice > snapEffectiveMaxToken {
				snap.Blocker = fmt.Sprintf("YES=%.2f oob[%.2f-%.2f]", snap.YesPrice, d.params.ConvictionMinTokenPrice, snapEffectiveMaxToken)
			}
		} else if snap.GapUSD < 0 {
			if snap.NoPrice < d.params.ConvictionMinTokenPrice || snap.NoPrice > snapEffectiveMaxToken {
				snap.Blocker = fmt.Sprintf("NO=%.2f oob[%.2f-%.2f]", snap.NoPrice, d.params.ConvictionMinTokenPrice, snapEffectiveMaxToken)
			}
		} else {
			snap.Blocker = "gap=0"
		}
	}

	return snap
}

// PairArbSnapshot is a lightweight, pair-arb-focused market state snapshot
// logged to signals.jsonl on every YES/NO/Chainlink tick.
type PairArbSnapshot struct {
	At               time.Time `json:"at"`
	BTCPrice         float64   `json:"btc_price"`
	OpenPrice        float64   `json:"open_price"`
	GapUSD           float64   `json:"gap_usd"`
	GapVelocity      float64   `json:"gap_velocity"`
	YesPrice         float64   `json:"yes_price"`
	NoPrice          float64   `json:"no_price"`
	WindowRemSec     float64   `json:"window_rem_sec"`
	CVDBTC           float64   `json:"cvd_btc"`
	BookImbalance    float64   `json:"book_imbalance"`
	CoinbaseSpread   float64   `json:"coinbase_spread"`
	CoinbaseTakerImb float64   `json:"coinbase_taker_imbalance"`
	TickRate5s       float64   `json:"tick_rate_5s"`
	BrainScore       float64   `json:"brain_score"`
	BrainLabeled     int       `json:"brain_labeled"`
	ChainlinkAgeSec  float64   `json:"cl_age_sec"`
	PrevWindowGapUSD float64   `json:"prev_window_gap_usd"`
	GapHoldSec       float64   `json:"gap_hold_sec"`
	BTCTickRun       int       `json:"btc_tick_run"`
}

// PairArbSnapshot returns a lightweight pair-arb-focused state snapshot.
// Thread-safe; intended for high-frequency logging on every tick.
func (d *Detector) PairArbSnapshot() PairArbSnapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()

	snap := PairArbSnapshot{
		At:               now,
		BTCPrice:         d.latestBTCPrice,
		OpenPrice:        d.openPrice,
		YesPrice:         d.polyYesPrice,
		NoPrice:          d.polyNoPrice,
		CVDBTC:           d.cvdBTC(),
		BookImbalance:    d.bookImbalance(),
		TickRate5s:       d.btcTickRate5s(),
		PrevWindowGapUSD: d.prevWindowCloseGap,
	}
	if snap.BTCPrice > 0 && snap.OpenPrice > 0 {
		snap.GapUSD = snap.BTCPrice - snap.OpenPrice
	}
	if !d.windowEnd.IsZero() {
		snap.WindowRemSec = d.windowEnd.Sub(now).Seconds()
	}
	if !d.chainlinkAt.IsZero() {
		snap.ChainlinkAgeSec = now.Sub(d.chainlinkAt).Seconds()
	}
	snap.GapVelocity = d.btcGapVelocityUSDPerSec(20)
	if !d.gapHeldSince.IsZero() {
		snap.GapHoldSec = now.Sub(d.gapHeldSince).Seconds()
	}
	gapDir := 1
	if snap.GapUSD < 0 {
		gapDir = -1
	}
	snap.BTCTickRun = d.btcTickRun(gapDir)
	if d.coinbasePrice > 0 && d.latestBTCPrice > 0 {
		snap.CoinbaseSpread = d.coinbasePrice - d.latestBTCPrice
	}
	snap.CoinbaseTakerImb = d.cbTakerImbalance()
	if d.brain != nil {
		var wf float64
		if total := snap.WindowRemSec + (now.Sub(d.windowStartedAt).Seconds()); total > 0 {
			wf = now.Sub(d.windowStartedAt).Seconds() / total
		}
		snap.BrainScore = d.brain.Score(BrainFeatures{
			GapVelocity:    snap.GapVelocity,
			AbsGap:         math.Abs(snap.GapUSD),
			CVD30s:         snap.CVDBTC,
			BookImbalance:  snap.BookImbalance,
			TickRate5s:     snap.TickRate5s,
			WindowFraction: wf,
		})
		snap.BrainLabeled = d.brain.TotalLabeled()
	}
	return snap
}

// EvaluateConviction checks all mid-window conviction gates via ConvictionSnapshot and
// returns a tradeable Signal when all gates pass. Returns SignalNone if any gate fails
// or conviction is disabled.
func (d *Detector) EvaluateConviction() Signal {
	snap := d.ConvictionSnapshot() // acquires/releases d.mu internally
	if snap.Blocker != "" {
		return Signal{Type: SignalNone}
	}
	// Stamp cooldown so subsequent ticks skip re-entry.
	d.mu.Lock()
	d.lastConvictionAt = snap.At
	d.mu.Unlock()

	sigType := SignalConvictionBuyNo
	if snap.GapUSD > 0 {
		sigType = SignalConvictionBuyYes
	}
	return Signal{
		Type:            sigType,
		BitstampPrice:   snap.BTCPrice,
		ChainlinkPrice:  snap.BTCPrice,
		OpenPrice:       snap.OpenPrice,
		PolyYesPrice:    snap.YesPrice,
		PolyNoPrice:     snap.NoPrice,
		WindowRemaining: snap.WindowRem,
		WinProb:         snap.WinProb,
		At:              snap.At,
	}
}

func (d *Detector) EvaluatePairArb() Signal {
	if !d.params.PairArbEnabled {
		return Signal{Type: SignalNone}
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.inPosition {
		return Signal{Type: SignalNone}
	}
	now := time.Now()
	if !d.blockedUntil.IsZero() && now.Before(d.blockedUntil) {
		return Signal{Type: SignalNone}
	}
	if d.openPrice == 0 || d.windowEnd.IsZero() {
		return Signal{Type: SignalNone}
	}

	windowRemSec := d.windowEnd.Sub(now).Seconds()
	elapsedSec := 0.0
	if !d.windowStartedAt.IsZero() {
		elapsedSec = now.Sub(d.windowStartedAt).Seconds()
	}
	if elapsedSec < float64(d.params.PairArbMinWindowSec) || elapsedSec > float64(d.params.PairArbMaxWindowSec) {
		return Signal{Type: SignalNone}
	}
	// Elapsed skip range: block entries whose elapsed falls in [From, To].
	if d.params.PairArbElapsedSkipFromSec > 0 && d.params.PairArbElapsedSkipToSec > d.params.PairArbElapsedSkipFromSec {
		if elapsedSec >= d.params.PairArbElapsedSkipFromSec && elapsedSec <= d.params.PairArbElapsedSkipToSec {
			return Signal{Type: SignalNone}
		}
	}
	// Prev-window gap skip: if the prior window closed with prevWinGap in [From, To),
	// suppress all entries in the current window.
	if d.params.PairArbPrevWinGapSkipFrom < d.params.PairArbPrevWinGapSkipTo {
		pwg := d.prevWindowCloseGap
		if pwg >= d.params.PairArbPrevWinGapSkipFrom && pwg < d.params.PairArbPrevWinGapSkipTo {
			return Signal{Type: SignalNone}
		}
	}

	btcPrice := d.latestBTCPrice
	if btcPrice <= 0 {
		return Signal{Type: SignalNone}
	}
	yesPrice := d.polyYesPrice
	noPrice := d.polyNoPrice
	if yesPrice <= 0 || noPrice <= 0 {
		return Signal{Type: SignalNone}
	}
	gapUSD := btcPrice - d.openPrice
	absGap := math.Abs(gapUSD)

	// CVD Momentum: early-window low-gap entry directed by BTC 30s CVD.
	// Fires BEFORE the standard gap/velocity path and returns early.
	// When in the CVD momentum zone (elapsed <= max, |gap| < max_gap), only
	// CVD momentum entries are considered; the standard path is skipped.
	if d.params.PairArbCVDMomentumEnabled {
		cvdMomMaxEl := float64(d.params.PairArbCVDMomentumMaxElapsedSec)
		if cvdMomMaxEl <= 0 {
			cvdMomMaxEl = 60.0
		}
		cvdMomMaxGap := d.params.PairArbCVDMomentumMaxGapUSD
		if cvdMomMaxGap <= 0 {
			cvdMomMaxGap = 15.0
		}
		if elapsedSec <= cvdMomMaxEl && absGap < cvdMomMaxGap {
			// Inside CVD momentum zone.
			cvdMomMinCVD := d.params.PairArbCVDMomentumMinCVDBTC
			if cvdMomMinCVD > 0 {
				cvd := d.cvdBTC()
				if math.Abs(cvd) >= cvdMomMinCVD {
					// Gap velocity decides direction — CVD magnitude confirms order flow exists,
					// but the gap direction (rising vs falling) tells us which side is winning now.
					// A rising gap (+$12→+$20) → buy YES; a falling gap (+$20→+$10) → buy NO,
					// even when CVD is still net-positive (30s backward-looking window).
					gapVelNow := d.btcGapVelocityUSDPerSec(20)
					if gapVelNow == 0 {
						return Signal{Type: SignalNone}
					}
					isYes := gapVelNow > 0
					// Max lead-drift filter: skip when the Polymarket token has already
					// moved too far in the entry direction — we want to enter while Poly
					// is still lagging BTC, not after it has already caught up.
					if d.params.PairArbCVDMomentumMaxLeadDriftCents > 0 {
						yesDriftCents := d.yesDrift() * 100
						cap := d.params.PairArbCVDMomentumMaxLeadDriftCents
						if isYes && yesDriftCents > cap {
							return Signal{Type: SignalNone}
						}
						if !isYes && yesDriftCents < -cap {
							return Signal{Type: SignalNone}
						}
					}
					tokenP := yesPrice
					oppP := noPrice
					sigT := SignalPairArbCVDMomentumYes
					if !isYes {
						tokenP = noPrice
						oppP = yesPrice
						sigT = SignalPairArbCVDMomentumNo
					}
					if tokenP >= d.params.PairArbMinTokenPrice && tokenP <= d.params.PairArbMaxTokenPrice {
						if d.params.PairArbMaxHedgeDistanceCents > 0 {
							lockedP := d.params.PairArbCVDMomentumLockedProfitCents / 100.0
							if lockedP <= 0 {
								lockedP = 0.03
							}
							maxHedge := 1.0 - lockedP - tokenP
							if oppP-maxHedge > d.params.PairArbMaxHedgeDistanceCents/100.0 {
								return Signal{Type: SignalNone}
							}
						}
						windowRemSecNow := d.windowEnd.Sub(now).Seconds()
						sigCoeffNow := computeSigmaCoeff(d.dvolAnnual, d.dvolMarkPrice)
						return Signal{
							Type:            sigT,
							BitstampPrice:   btcPrice,
							ChainlinkPrice:  d.chainlinkPrice,
							OpenPrice:       d.openPrice,
							PolyYesPrice:    yesPrice,
							PolyNoPrice:     noPrice,
							WindowRemaining: windowRemSecNow,
							WinProb:         winProbGaussianSigma(absGap, windowRemSecNow, sigCoeffNow),
							At:              now,
						}
					}
				}
			}
			// In CVD zone but conditions not met — skip standard pair-arb path.
			return Signal{Type: SignalNone}
		}
	}

	if gapUSD == 0 {
		return Signal{Type: SignalNone}
	}
	gapVelocity := d.btcGapVelocityUSDPerSec(20)
	cvdNow := d.cvdBTC()
	bookImbNow := d.bookImbalance()
	tickRateNow := d.btcTickRate5s()
	if d.brain != nil {
		var windowFraction float64
		if windowTotal := elapsedSec + windowRemSec; windowTotal > 0 {
			windowFraction = elapsedSec / windowTotal
		}
		brainF := BrainFeatures{
			GapVelocity:    gapVelocity,
			AbsGap:         absGap,
			CVD30s:         cvdNow,
			BookImbalance:  bookImbNow,
			TickRate5s:     tickRateNow,
			WindowFraction: windowFraction,
		}
		// Record observations on every pair-arb evaluation tick so the model can
		// warm up even when static gates currently block entries.
		d.brain.AddObservation(brainF, btcPrice, d.openPrice, now)
	}
	minGapUSD := d.params.PairArbMinBTCGapUSD
	// Prior-window direction confirm: for the first PairArbEarlyPrevDirConfirmSec
	// seconds, if the prior window closed with a meaningful gap in the OPPOSITE
	// direction, block the entry. This prevents entering on a transient reversal
	// spike right at window open when momentum was already running the other way.
	// (When direction ALIGNS, the carry feature below further eases the threshold.)
	if d.params.PairArbEarlyPrevDirConfirmSec > 0 && elapsedSec < float64(d.params.PairArbEarlyPrevDirConfirmSec) {
		prevGap := d.prevWindowCloseGap
		minPrevGap := d.params.PairArbEarlyMinPrevDirGapUSD
		if minPrevGap <= 0 {
			minPrevGap = 5.0
		}
		if math.Abs(prevGap) >= minPrevGap {
			if (gapUSD > 0 && prevGap < 0) || (gapUSD < 0 && prevGap > 0) {
				return Signal{Type: SignalNone}
			}
		}
	}

	// Early-window tier: for the first PairArbEarlyWindowMaxSec seconds of each
	// window, use a lower gap threshold and adjusted velocity to capture entries
	// while the lead token is cheapest and the gap is still building.
	// After the early window, standard thresholds apply.
	effectiveMinVelocity := d.params.PairArbMinGapVelocityUSD
	if d.params.PairArbEarlyWindowMaxSec > 0 && elapsedSec <= float64(d.params.PairArbEarlyWindowMaxSec) {
		if d.params.PairArbEarlyMinGapUSD > 0 && d.params.PairArbEarlyMinGapUSD < minGapUSD {
			minGapUSD = d.params.PairArbEarlyMinGapUSD
		}
		if d.params.PairArbEarlyMinVelocityUSD > 0 {
			effectiveMinVelocity = d.params.PairArbEarlyMinVelocityUSD
		}
	}
	if absGap < minGapUSD {
		return Signal{Type: SignalNone}
	}
	if d.params.PairArbMaxBTCGapUSD > 0 && absGap > d.params.PairArbMaxBTCGapUSD {
		return Signal{Type: SignalNone}
	}
	// Vel/gap ratio filters: skip when |gapVelocity|/absGap is in the skip range
	// or exceeds the max ratio.
	if absGap > 0 {
		ratio := math.Abs(gapVelocity) / absGap
		if d.params.PairArbVelGapRatioSkipFrom > 0 && d.params.PairArbVelGapRatioSkipTo > d.params.PairArbVelGapRatioSkipFrom {
			if ratio >= d.params.PairArbVelGapRatioSkipFrom && ratio < d.params.PairArbVelGapRatioSkipTo {
				return Signal{Type: SignalNone}
			}
		}
		if d.params.PairArbMaxVelGapRatio > 0 && ratio > d.params.PairArbMaxVelGapRatio {
			return Signal{Type: SignalNone}
		}
	}

	reverseMode := d.params.PairArbReverseSignalEnabled
	if reverseMode {
		reverseMinGap := d.params.PairArbReverseSignalMinGapUSD
		if reverseMinGap <= 0 {
			reverseMinGap = minGapUSD
		}
		if absGap < reverseMinGap {
			return Signal{Type: SignalNone}
		}
		reverseMaxGap := d.params.PairArbReverseSignalMaxGapUSD
		if reverseMaxGap > 0 && absGap > reverseMaxGap {
			return Signal{Type: SignalNone}
		}
		reverseLookbackSec := d.params.PairArbReverseSignalLookbackSec
		if reverseLookbackSec <= 0 {
			reverseLookbackSec = 8
		}
		gapDelta := d.btcGapDeltaUSD(float64(reverseLookbackSec))
		reverseMinShrink := d.params.PairArbReverseSignalMinShrinkUSD
		if reverseMinShrink <= 0 {
			reverseMinShrink = 2.0
		}
		if gapUSD > 0 && gapDelta > -reverseMinShrink {
			return Signal{Type: SignalNone}
		}
		if gapUSD < 0 && gapDelta < reverseMinShrink {
			return Signal{Type: SignalNone}
		}
		reverseMinVel := d.params.PairArbReverseSignalMinVelocityUSD
		if reverseMinVel <= 0 {
			reverseMinVel = effectiveMinVelocity
		}
		if reverseMinVel > 0 {
			if gapUSD > 0 && gapVelocity > -reverseMinVel {
				return Signal{Type: SignalNone}
			}
			if gapUSD < 0 && gapVelocity < reverseMinVel {
				return Signal{Type: SignalNone}
			}
		}
	} else if effectiveMinVelocity > 0 {
		if gapUSD > 0 && gapVelocity < effectiveMinVelocity {
			return Signal{Type: SignalNone}
		}
		if gapUSD < 0 && gapVelocity > -effectiveMinVelocity {
			return Signal{Type: SignalNone}
		}
	}

	isYesLead := gapUSD > 0
	if reverseMode {
		isYesLead = !isYesLead
	}

	if d.params.PairArbMinCVDBTC > 0 {
		cvd := cvdNow
		if d.params.PairArbFlowMovementMode {
			if math.Abs(cvd) < d.params.PairArbMinCVDBTC {
				return Signal{Type: SignalNone}
			}
		} else {
			if isYesLead && cvd < d.params.PairArbMinCVDBTC {
				return Signal{Type: SignalNone}
			}
			if !isYesLead && cvd > -d.params.PairArbMinCVDBTC {
				return Signal{Type: SignalNone}
			}
		}
	}
	// Max CVD filter: skip when CVD exceeds the cap in the entry direction.
	if d.params.PairArbMaxCVDBTC > 0 {
		cvd := cvdNow
		if isYesLead && cvd > d.params.PairArbMaxCVDBTC {
			return Signal{Type: SignalNone}
		}
		if !isYesLead && cvd < -d.params.PairArbMaxCVDBTC {
			return Signal{Type: SignalNone}
		}
	}
	// CVD range skip: block entries when CVD falls in [From, To) for YES lead
	// or [-To, -From) for NO lead.
	if d.params.PairArbCVDRangeSkipFrom < d.params.PairArbCVDRangeSkipTo {
		cvd := cvdNow
		if isYesLead && cvd >= d.params.PairArbCVDRangeSkipFrom && cvd < d.params.PairArbCVDRangeSkipTo {
			return Signal{Type: SignalNone}
		}
		if !isYesLead && cvd >= -d.params.PairArbCVDRangeSkipTo && cvd < -d.params.PairArbCVDRangeSkipFrom {
			return Signal{Type: SignalNone}
		}
	}
	if d.params.PairArbMinBookImbalance > 0 {
		imb := bookImbNow
		if d.params.PairArbFlowMovementMode {
			if math.Abs(imb) < d.params.PairArbMinBookImbalance {
				return Signal{Type: SignalNone}
			}
		} else {
			if isYesLead && imb < d.params.PairArbMinBookImbalance {
				return Signal{Type: SignalNone}
			}
			if !isYesLead && imb > -d.params.PairArbMinBookImbalance {
				return Signal{Type: SignalNone}
			}
		}
	}
	if d.params.PairArbMinCoinbaseSpreadUSD > 0 || d.params.PairArbMaxCoinbaseSpreadUSD > 0 {
		spread := d.coinbasePrice - btcPrice
		if d.params.PairArbMinCoinbaseSpreadUSD > 0 {
			if d.params.PairArbFlowMovementMode {
				if math.Abs(spread) < d.params.PairArbMinCoinbaseSpreadUSD {
					return Signal{Type: SignalNone}
				}
			} else {
				if isYesLead && spread < d.params.PairArbMinCoinbaseSpreadUSD {
					return Signal{Type: SignalNone}
				}
				if !isYesLead && spread > -d.params.PairArbMinCoinbaseSpreadUSD {
					return Signal{Type: SignalNone}
				}
			}
		}
		if d.params.PairArbMaxCoinbaseSpreadUSD > 0 {
			if d.params.PairArbFlowMovementMode {
				if math.Abs(spread) > d.params.PairArbMaxCoinbaseSpreadUSD {
					return Signal{Type: SignalNone}
				}
			} else {
				if isYesLead && spread > d.params.PairArbMaxCoinbaseSpreadUSD {
					return Signal{Type: SignalNone}
				}
				if !isYesLead && spread < -d.params.PairArbMaxCoinbaseSpreadUSD {
					return Signal{Type: SignalNone}
				}
			}
		}
	}
	if d.params.PairArbMinCoinbaseTakerImbalance > 0 {
		cbImb := d.cbTakerImbalance()
		if isYesLead && cbImb < d.params.PairArbMinCoinbaseTakerImbalance {
			return Signal{Type: SignalNone}
		}
		if !isYesLead && cbImb > -d.params.PairArbMinCoinbaseTakerImbalance {
			return Signal{Type: SignalNone}
		}
	}
	// Taker-imbalance skip range: block when cbTakerImbalance is in [From, To)
	// for YES lead, or in [-To, -From) for NO lead.
	if d.params.PairArbTakerImbSkipFrom < d.params.PairArbTakerImbSkipTo {
		cbImb := d.cbTakerImbalance()
		if isYesLead && cbImb >= d.params.PairArbTakerImbSkipFrom && cbImb < d.params.PairArbTakerImbSkipTo {
			return Signal{Type: SignalNone}
		}
		if !isYesLead && cbImb > -d.params.PairArbTakerImbSkipTo && cbImb <= -d.params.PairArbTakerImbSkipFrom {
			return Signal{Type: SignalNone}
		}
	}
	if d.params.PairArbMinOIDeltaUSD > 0 {
		oiDelta := d.oiDeltaUSD
		if isYesLead && oiDelta < d.params.PairArbMinOIDeltaUSD {
			return Signal{Type: SignalNone}
		}
		if !isYesLead && oiDelta > -d.params.PairArbMinOIDeltaUSD {
			return Signal{Type: SignalNone}
		}
	}
	if d.params.PairArbMaxContraOIDeltaUSD > 0 {
		oiDelta := d.oiDeltaUSD
		if isYesLead && oiDelta < -d.params.PairArbMaxContraOIDeltaUSD {
			return Signal{Type: SignalNone}
		}
		if !isYesLead && oiDelta > d.params.PairArbMaxContraOIDeltaUSD {
			return Signal{Type: SignalNone}
		}
	}
	if d.params.PairArbMaxAdverseYesDriftCents > 0 {
		driftCents := d.yesDriftOver(float64(d.params.PairArbYesDriftWindowSec)) * 100.0
		if isYesLead && driftCents < -d.params.PairArbMaxAdverseYesDriftCents {
			return Signal{Type: SignalNone}
		}
		if !isYesLead && driftCents > d.params.PairArbMaxAdverseYesDriftCents {
			return Signal{Type: SignalNone}
		}
	}
	// Yes-drift skip range: block when yes_drift_cents is in [From, To),
	// direction-independent (applied regardless of YES or NO lead).
	if d.params.PairArbYesDriftSkipFromCents < d.params.PairArbYesDriftSkipToCents {
		driftCents := d.yesDrift() * 100.0
		if driftCents >= d.params.PairArbYesDriftSkipFromCents && driftCents < d.params.PairArbYesDriftSkipToCents {
			return Signal{Type: SignalNone}
		}
	}

	if !reverseMode && d.params.PairArbMinGapHoldSec > 0 {
		holdDir := 1
		if gapUSD < 0 {
			holdDir = -1
		}
		if d.gapHeldDir != holdDir || d.gapHeldSince.IsZero() ||
			now.Sub(d.gapHeldSince).Seconds() < float64(d.params.PairArbMinGapHoldSec) {
			return Signal{Type: SignalNone}
		}
	}
	if !reverseMode && d.params.PairArbMinBTCTickRun > 0 {
		tickDir := 1
		if gapUSD < 0 {
			tickDir = -1
		}
		if d.btcTickRun(tickDir) < d.params.PairArbMinBTCTickRun {
			return Signal{Type: SignalNone}
		}
	}
	// Early elapsed tick-run filter: within the first N seconds of the window,
	// require at least 1 BTC tick in the entry direction.
	if d.params.PairArbEarlyElapsedTickRunSkipSec > 0 && elapsedSec < d.params.PairArbEarlyElapsedTickRunSkipSec {
		tickDir := 1
		if gapUSD < 0 {
			tickDir = -1
		}
		if d.btcTickRun(tickDir) < 1 {
			return Signal{Type: SignalNone}
		}
	}

	sigCoeff := computeSigmaCoeff(d.dvolAnnual, d.dvolMarkPrice)
	winProb := winProbGaussianSigma(absGap, windowRemSec, sigCoeff)
	// Momentum mode buys WITH gap direction; reversal mode buys the opposite side
	// when the gap is actively shrinking back toward zero.
	sigType := SignalPairArbLeadNo
	tokenPrice := noPrice
	oppPrice := yesPrice
	if isYesLead {
		tokenPrice = yesPrice
		oppPrice = noPrice
		if reverseMode {
			sigType = SignalPairArbReverseLeadYes
		} else {
			sigType = SignalPairArbLeadYes
		}
	} else if reverseMode {
		sigType = SignalPairArbReverseLeadNo
	}
	if tokenPrice < d.params.PairArbMinTokenPrice || tokenPrice > d.params.PairArbMaxTokenPrice {
		return Signal{Type: SignalNone}
	}
	if d.params.PairArbMaxHedgeDistanceCents > 0 {
		lockedProfit := d.params.PairArbMinLockedProfitCents / 100.0
		maxHedgePrice := 1.0 - lockedProfit - tokenPrice
		if oppPrice-maxHedgePrice > d.params.PairArbMaxHedgeDistanceCents/100.0 {
			return Signal{Type: SignalNone}
		}
	}

	if d.brain != nil {
		var windowFraction float64
		if windowTotal := elapsedSec + windowRemSec; windowTotal > 0 {
			windowFraction = elapsedSec / windowTotal
		}
		if score := d.brain.Score(BrainFeatures{
			GapVelocity:    gapVelocity,
			AbsGap:         absGap,
			CVD30s:         cvdNow,
			BookImbalance:  bookImbNow,
			TickRate5s:     tickRateNow,
			WindowFraction: windowFraction,
		}); score < d.brain.ScoreThreshold() {
			return Signal{Type: SignalNone}
		}
	}

	// Session trend filter: block entries that contradict a sustained multi-window BTC drift.
	// Compares the current window's open price against the oldest entry in the rolling buffer.
	// BTC open drifted up > threshold → block NO leads (BTC trending up, NO bets revert).
	// BTC open drifted down > threshold → block YES leads (BTC trending down).
	if d.params.PairArbSessionTrendFilterUSD > 0 {
		maxBuckets := d.params.PairArbSessionTrendBuckets
		if maxBuckets <= 0 {
			maxBuckets = 6
		}
		if len(d.sessionBTCOpens) >= maxBuckets {
			oldest := d.sessionBTCOpens[len(d.sessionBTCOpens)-maxBuckets]
			drift := d.openPrice - oldest
			if drift > d.params.PairArbSessionTrendFilterUSD && !isYesLead {
				return Signal{Type: SignalNone} // BTC trending up: block NO leads
			}
			if drift < -d.params.PairArbSessionTrendFilterUSD && isYesLead {
				return Signal{Type: SignalNone} // BTC trending down: block YES leads
			}
		}
	}

	// Direction mode: optionally restrict to YES-lead or NO-lead entries only.
	if mode := d.params.PairArbDirectionMode; mode != "" && !strings.EqualFold(mode, "BOTH") {
		if strings.EqualFold(mode, "YES") && !isYesLead {
			return Signal{Type: SignalNone}
		}
		if strings.EqualFold(mode, "NO") && isYesLead {
			return Signal{Type: SignalNone}
		}
	}

	return Signal{
		Type:            sigType,
		BitstampPrice:   btcPrice,
		ChainlinkPrice:  d.chainlinkPrice,
		OpenPrice:       d.openPrice,
		PolyYesPrice:    yesPrice,
		PolyNoPrice:     noPrice,
		WindowRemaining: windowRemSec,
		WinProb:         winProb,
		At:              now,
	}
}

func (d *Detector) EvaluateDCAHedge() Signal {
	if !d.params.DCAHedgeEnabled {
		return Signal{Type: SignalNone}
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.inPosition || d.dcaHedgeFiredThisWindow {
		return Signal{Type: SignalNone}
	}
	now := time.Now()
	if !d.blockedUntil.IsZero() && now.Before(d.blockedUntil) {
		return Signal{Type: SignalNone}
	}
	if d.windowEnd.IsZero() || d.openPrice == 0 {
		return Signal{Type: SignalNone}
	}

	yesPrice := d.polyYesPrice
	noPrice := d.polyNoPrice
	openYes := d.dcaHedgeOpenYes
	openNo := d.dcaHedgeOpenNo
	if yesPrice <= 0 || noPrice <= 0 || openYes <= 0 || openNo <= 0 {
		return Signal{Type: SignalNone}
	}

	// Entry-sum filter: there must be a gap to $1.00.
	maxSum := d.params.DCAHedgeMaxEntrySum
	if maxSum <= 0 {
		maxSum = 0.98
	}
	if yesPrice+noPrice >= maxSum {
		return Signal{Type: SignalNone}
	}

	// Check which side moved enough from its window-open baseline.
	moveTrig := d.params.DCAHedgeMoveTrigger
	if moveTrig <= 0 {
		moveTrig = 0.10
	}
	yesRise := yesPrice - openYes
	noRise := noPrice - openNo

	var movedSide string
	var movedPrice, oppPrice float64
	var sigType SignalType

	switch {
	case yesRise >= moveTrig && yesRise >= noRise:
		movedSide = "YES"
		movedPrice = yesPrice
		oppPrice = noPrice
		sigType = SignalDCAHedgeYes
	case noRise >= moveTrig:
		movedSide = "NO"
		movedPrice = noPrice
		oppPrice = yesPrice
		sigType = SignalDCAHedgeNo
	default:
		return Signal{Type: SignalNone}
	}

	// Swing / tortuosity filter: block entries where the moved side ran straight up
	// (a directional trend) rather than oscillating (a swing market).
	// tortuosity = total_path_variation / net_rise
	//   ≈ 1.0  → smooth trend (block)
	//   > 1.0  → price bounced on the way up (swing → allow)
	swingMin := d.params.DCAHedgeSwingTortuosityMin
	if swingMin > 0 {
		var totalVar, netRise float64
		if movedSide == "YES" {
			totalVar = d.dcaHedgeYesTotalVar
			netRise = yesPrice - openYes
		} else {
			totalVar = d.dcaHedgeNoTotalVar
			netRise = noPrice - openNo
		}
		if netRise > 0.001 {
			if totalVar/netRise < swingMin {
				return Signal{Type: SignalNone}
			}
		}
	}

	// Swing / opp-side-rise filter: require the OPP (non-moved) side to have had a
	// prior upward move >= DCAHedgeSwingOppRiseMin since window open. If both sides have
	// shown upward momentum at some point, the market is oscillating (uncertain). If only
	// the moved side ran up and the opp side only fell, it's a directional trend → block.
	oppRiseMin := d.params.DCAHedgeSwingOppRiseMin
	if oppRiseMin > 0 {
		var oppWindowMax, openOpp float64
		if movedSide == "YES" {
			oppWindowMax = d.dcaHedgeNoWindowMax
			openOpp = openNo
		} else {
			oppWindowMax = d.dcaHedgeYesWindowMax
			openOpp = openYes
		}
		if oppWindowMax-openOpp < oppRiseMin {
			return Signal{Type: SignalNone}
		}
	}

	// Elapsed-time gate: block if the trigger fires within DCAHedgeMinElapsedSec of
	// window start. Very fast triggers indicate a directional trend, not an oscillation.
	minElapsedSec := d.params.DCAHedgeMinElapsedSec
	if minElapsedSec > 0 {
		elapsed := now.Sub(d.windowStartedAt).Seconds()
		if elapsed < minElapsedSec {
			return Signal{Type: SignalNone}
		}
	}

	// Compute share count.
	baseShares := d.params.DCAHedgeBaseShares
	if baseShares <= 0 {
		baseShares = 5
	}
	maxSharesCap := d.params.DCAHedgeMaxShares
	if maxSharesCap <= 0 {
		maxSharesCap = 50
	}
	shares := baseShares
	if d.params.DCAHedgeUseDynamicSizing {
		gapDir := 1
		if d.latestBTCPrice < d.openPrice {
			gapDir = -1
		}
		tickRun := d.btcTickRun(gapDir)
		elapsed := now.Sub(d.windowStartedAt).Seconds()
		absGap := math.Abs(d.latestBTCPrice - d.openPrice)

		mult := 1.0
		if tickRun > 0 {
			mult *= 3.0
		}
		if elapsed < 20 {
			mult *= 1.5
		}
		if absGap < 4 {
			mult *= 1.5
		}
		// Round to nearest 5, minimum baseShares.
		raw := math.Round(baseShares*mult/5) * 5
		if raw < baseShares {
			raw = baseShares
		}
		shares = raw
	}
	if shares > maxSharesCap {
		shares = maxSharesCap
	}

	// Both legs must be worth at least $1 (Polymarket minimum).
	if movedPrice*shares < 1.0 || oppPrice*shares < 1.0 {
		return Signal{Type: SignalNone}
	}

	d.dcaHedgeFiredThisWindow = true
	windowRemSec := d.windowEnd.Sub(now).Seconds()

	return Signal{
		Type:                 sigType,
		BitstampPrice:        d.latestBTCPrice,
		ChainlinkPrice:       d.chainlinkPrice,
		OpenPrice:            d.openPrice,
		PolyYesPrice:         yesPrice,
		PolyNoPrice:          noPrice,
		WindowRemaining:      windowRemSec,
		DCAHedgeMovedSide:    movedSide,
		DCAHedgeMovedShares:  shares,
		DCAHedgeOppShares:    shares,
		DCAHedgeTriggerPrice: movedPrice,
		At:                   now,
	}
}

func (d *Detector) EvaluateResolutionSnipe() Signal {
	if !d.params.ResolutionEnabled {
		return Signal{Type: SignalNone}
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.inPosition {
		return Signal{Type: SignalNone}
	}
	now := time.Now()
	if !d.blockedUntil.IsZero() && now.Before(d.blockedUntil) {
		return Signal{Type: SignalNone}
	}
	if d.openPrice == 0 || d.windowEnd.IsZero() {
		return Signal{Type: SignalNone}
	}

	windowRemSec := d.windowEnd.Sub(now).Seconds()
	if windowRemSec < float64(d.params.ResolutionMinWindowSec) || windowRemSec > float64(d.params.ResolutionWindowSec) {
		return Signal{Type: SignalNone}
	}

	btcPrice := d.latestBTCPrice
	if btcPrice == 0 {
		return Signal{Type: SignalNone}
	}
	yesPrice := d.polyYesPrice
	if yesPrice <= 0 {
		return Signal{Type: SignalNone}
	}
	noPrice := math.Round((1.0-yesPrice)*100) / 100

	gapUSD := btcPrice - d.openPrice
	if gapUSD == 0 {
		return Signal{Type: SignalNone}
	}
	absGap := math.Abs(gapUSD)
	if d.params.ResolutionMinBTCGapUSD > 0 && absGap < d.params.ResolutionMinBTCGapUSD {
		return Signal{Type: SignalNone}
	}

	sigCoeff := computeSigmaCoeff(d.dvolAnnual, d.dvolMarkPrice)
	winProb := winProbGaussianSigma(absGap, windowRemSec, sigCoeff)
	if d.params.ResolutionMinWinProb > 0 && winProb < d.params.ResolutionMinWinProb {
		return Signal{Type: SignalNone}
	}

	sigType := SignalResolutionBuyNo
	tokenPrice := noPrice
	if gapUSD > 0 {
		sigType = SignalResolutionBuyYes
		tokenPrice = yesPrice
	}
	if tokenPrice < d.params.ResolutionMinTokenPrice || tokenPrice > d.params.ResolutionMaxTokenPrice {
		return Signal{Type: SignalNone}
	}

	d.lastConvictionAt = now
	d.logger.Info("detector.EvaluateResolutionSnipe: RESOLUTION",
		zap.String("side", map[bool]string{true: "YES", false: "NO"}[gapUSD > 0]),
		zap.Float64("window_rem_s", windowRemSec),
		zap.Float64("token_price", tokenPrice),
		zap.Float64("gap_usd", gapUSD),
		zap.Float64("win_prob", winProb),
	)
	return Signal{
		Type:            sigType,
		BitstampPrice:   btcPrice,
		ChainlinkPrice:  d.chainlinkPrice,
		OpenPrice:       d.openPrice,
		PolyYesPrice:    yesPrice,
		PolyNoPrice:     noPrice,
		WindowRemaining: windowRemSec,
		WinProb:         winProb,
		At:              now,
	}
}

// SignalDiagnostic returns a compact human-readable status string showing the
// current gate conditions for the active strategy. Thread-safe; designed for
// real-time display on the web dashboard.
func (d *Detector) SignalDiagnostic() string {
	cs := d.ConvictionSnapshot() // acquires/releases mu internally

	d.mu.Lock()
	now := time.Now()
	windowRemSec := 0.0
	if !d.windowEnd.IsZero() {
		windowRemSec = d.windowEnd.Sub(now).Seconds()
	}
	yesPrice := d.polyYesPrice
	noPrice := math.Round((1.0-yesPrice)*100) / 100
	coinbaseSpread := d.coinbasePrice - d.latestBTCPrice
	cvd := d.cvdBTC()
	gapUSD := cs.GapUSD
	absGap := math.Abs(gapUSD)
	p := d.params
	inPosition := d.inPosition
	d.mu.Unlock()

	if cs.BTCPrice == 0 || yesPrice == 0 {
		return "waiting for price data"
	}
	if cs.OpenPrice == 0 || windowRemSec <= 0 {
		return fmt.Sprintf("no_window  gap=%+.0f  wp=%.0f%%", gapUSD, cs.WinProb*100)
	}

	prefix := fmt.Sprintf("rem=%.0fs  gap=%+.0f  cb=%+.1f  cvd=%.1f  wp=%.0f%%",
		windowRemSec, gapUSD, coinbaseSpread, cvd, cs.WinProb*100)

	if inPosition {
		return prefix + "  |  in_position"
	}

	// Detect which strategy window(s) we are currently in, in priority order.
	inCollapseWindow := p.SnipeCollapseEnabled &&
		windowRemSec >= float64(p.SnipeCollapseMinWindowSec) &&
		windowRemSec <= float64(p.SnipeCollapseMaxWindowSec)
	inLateWindow := p.LateFlipEnabled &&
		windowRemSec >= float64(p.LateFlipMinWindowSec) &&
		windowRemSec <= float64(p.LateFlipMaxWindowSec)
	inSnipeWindow := p.SnipeEnabled &&
		windowRemSec >= float64(p.SnipeMinWindowSec) &&
		windowRemSec <= float64(p.SnipeMaxWindowSec)
	inMidWindow := p.MidFlipEnabled &&
		windowRemSec >= float64(p.MidFlipMinWindowSec) &&
		windowRemSec <= float64(p.MidFlipMaxWindowSec)

	// snipeGate returns the first failing gate label for snipe-style strategies.
	snipeGate := func(name, side string, tokenPrice, minGap, maxGap, cbThresh, cvdThresh, minTok, maxTok, minWP float64) string {
		gapOK := absGap >= minGap && (maxGap == 0 || absGap <= maxGap)
		cbOK := (gapUSD > 0 && coinbaseSpread < -cbThresh) || (gapUSD < 0 && coinbaseSpread > cbThresh)
		cvdOK := cvdThresh == 0 || (gapUSD > 0 && cvd < -cvdThresh) || (gapUSD < 0 && cvd > cvdThresh)
		wpOK := minWP == 0 || cs.WinProb >= minWP
		tokOK := tokenPrice >= minTok && tokenPrice <= maxTok
		var sb strings.Builder
		fmt.Fprintf(&sb, "%s [%s]", name, side)
		switch {
		case !gapOK:
			if maxGap > 0 {
				fmt.Fprintf(&sb, ": gap=$%.0f need $%.0f-%.0f", absGap, minGap, maxGap)
			} else {
				fmt.Fprintf(&sb, ": gap=$%.0f need >$%.0f", absGap, minGap)
			}
		case !cbOK:
			fmt.Fprintf(&sb, ": cb=%+.1f need %+.1f", coinbaseSpread, cbThresh)
		case !cvdOK:
			fmt.Fprintf(&sb, ": cvd=%.1f need %.1f", cvd, cvdThresh)
		case !wpOK:
			fmt.Fprintf(&sb, ": wp=%.0f%% need %.0f%%", cs.WinProb*100, minWP*100)
		case !tokOK:
			fmt.Fprintf(&sb, ": tok=%.3f oob[%.3f-%.3f]", tokenPrice, minTok, maxTok)
		default:
			sb.WriteString(": READY!")
		}
		return sb.String()
	}

	side := "NO"
	if gapUSD < 0 {
		side = "YES"
	}
	tokenPrice := noPrice
	if side == "YES" {
		tokenPrice = yesPrice
	}

	switch {
	case inCollapseWindow:
		return prefix + "  |  " + snipeGate("COLLAPSE", side,
			tokenPrice,
			p.SnipeCollapseMinBTCGapUSD, p.SnipeCollapseMaxBTCGapUSD,
			p.SnipeCollapseCoinbaseSpreadUSD, p.SnipeCollapseCVDThresholdBTC,
			p.SnipeCollapseMinTokenPrice, p.SnipeCollapseMaxTokenPrice,
			0)
	case inLateWindow:
		return prefix + "  |  " + snipeGate("LATE FLIP", side,
			tokenPrice,
			p.LateFlipMinBTCGapUSD, p.LateFlipMaxBTCGapUSD,
			p.LateFlipMinCoinbaseSpreadUSD, 0, // no CVD gate for late flip
			p.LateFlipMinTokenPrice, p.LateFlipMaxTokenPrice,
			p.LateFlipMinWinProb)
	case inSnipeWindow:
		minTok := p.SnipeMinNoEntryPrice
		maxTok := p.SnipeMaxNoEntryPrice
		if side == "YES" {
			minTok = p.SnipeMinYesEntryPrice
			maxTok = p.SnipeMaxYesEntryPrice
		}
		return prefix + "  |  " + snipeGate("SNIPE", side,
			tokenPrice,
			p.SnipeMinBTCGapUSD, p.SnipeMaxBTCGapUSD,
			p.SnipeCoinbaseSpreadUSD, p.SnipeCVDThresholdBTC,
			minTok, maxTok,
			0)
	case inMidWindow:
		return prefix + "  |  " + snipeGate("MID FLIP", side,
			tokenPrice,
			p.MidFlipMinBTCGapUSD, p.MidFlipMaxBTCGapUSD,
			0, 0, // mid-flip uses gap velocity, not CB/CVD spread
			p.MidFlipMinTokenPrice, p.MidFlipMaxTokenPrice,
			p.MidFlipMinWinProb)
	default:
		// Outside all snipe windows — show arb strategy gate status.
		if cs.Blocker != "" {
			return prefix + "  |  ARB: " + cs.Blocker
		}
		return prefix + "  |  ARB: READY!"
	}
}

// ActiveStrategyGates returns live gate status for each enabled strategy.
// It is read-only and safe to call every dashboard tick.
func (d *Detector) ActiveStrategyGates(inPosition, buyInProgress bool) []StrategyGateStatus {
	conv := d.ConvictionSnapshot()
	pbAllowed, pbReason := d.PennyBuyGateStatus(inPosition, buyInProgress)

	d.mu.Lock()
	now := time.Now()
	p := d.params
	hasWindow := !d.windowEnd.IsZero()
	windowRemSec := 0.0
	if !d.windowEnd.IsZero() {
		windowRemSec = d.windowEnd.Sub(now).Seconds()
	}
	btcPrice := d.latestBTCPrice
	openPrice := d.openPrice
	yesPrice := d.polyYesPrice
	noPrice := math.Round((1.0-yesPrice)*100) / 100
	gapUSD := btcPrice - openPrice
	absGap := math.Abs(gapUSD)
	coinbaseSpread := d.coinbasePrice - btcPrice
	cvd := d.cvdBTC()
	gapVelMid := d.btcGapVelocityUSDPerSec(float64(p.MidFlipGapVelocityWindowSec))
	atr := computeATR(d.btcSamples)
	blockedRem := 0.0
	if !d.blockedUntil.IsZero() && now.Before(d.blockedUntil) {
		blockedRem = d.blockedUntil.Sub(now).Seconds()
	}
	d.mu.Unlock()

	snipeScale := 1.0
	if p.SnipeDynamicATRRefUSD > 0 && atr > 0 {
		snipeScale = atr / p.SnipeDynamicATRRefUSD
		if p.SnipeDynamicATRMinScale > 0 && snipeScale < p.SnipeDynamicATRMinScale {
			snipeScale = p.SnipeDynamicATRMinScale
		}
		if p.SnipeDynamicATRMaxScale > 0 && snipeScale > p.SnipeDynamicATRMaxScale {
			snipeScale = p.SnipeDynamicATRMaxScale
		}
	}
	snipeMinGapUSD := p.SnipeMinBTCGapUSD * snipeScale
	snipeCoinbaseSpreadUSD := p.SnipeCoinbaseSpreadUSD * snipeScale
	snipeCVDThresholdBTC := p.SnipeCVDThresholdBTC * snipeScale

	baseBlocker := func() string {
		switch {
		case inPosition:
			return "in position"
		case buyInProgress:
			return "buy in progress"
		case blockedRem > 0:
			return fmt.Sprintf("cooldown %.0fs", blockedRem)
		case openPrice == 0 || !hasWindow:
			return "no active window"
		case windowRemSec <= 0:
			return "window ended"
		default:
			return ""
		}
	}
	appendGate := func(out []StrategyGateStatus, key, name, blocker string) []StrategyGateStatus {
		status := "BLOCKED"
		ready := false
		if blocker == "" {
			status = "READY"
			ready = true
		}
		return append(out, StrategyGateStatus{Key: key, Name: name, Status: status, Ready: ready, Blocker: blocker})
	}
	windowBlocker := func(rem float64, minSec, maxSec int) string {
		if minSec > 0 && rem < float64(minSec) {
			return fmt.Sprintf("%.0fs rem (need ≥%ds)", rem, minSec)
		}
		if maxSec > 0 && rem > float64(maxSec) {
			return fmt.Sprintf("%.0fs rem (need ≤%ds)", rem, maxSec)
		}
		return ""
	}
	maxFlippableGap := func(rem float64) float64 {
		switch {
		case rem >= 35:
			return 80
		case rem >= 27:
			return 60
		default:
			return 40
		}
	}
	sideForGap := func() (string, float64) {
		if gapUSD > 0 {
			return "NO", noPrice
		}
		if gapUSD < 0 {
			return "YES", yesPrice
		}
		return "", 0
	}

	out := make([]StrategyGateStatus, 0, 9)
	if p.ResolutionEnabled {
		blocker := baseBlocker()
		if blocker == "" {
			blocker = windowBlocker(windowRemSec, p.ResolutionMinWindowSec, p.ResolutionWindowSec)
		}
		if blocker == "" {
			if gapUSD == 0 {
				blocker = "gap=0"
			} else if absGap < p.ResolutionMinBTCGapUSD {
				blocker = fmt.Sprintf("gap=$%.1f < $%.1f", absGap, p.ResolutionMinBTCGapUSD)
			} else if conv.WinProb < p.ResolutionMinWinProb {
				blocker = fmt.Sprintf("prob=%.1f%% < %.0f%%", conv.WinProb*100, p.ResolutionMinWinProb*100)
			} else {
				side, tokenPrice := sideForGap()
				if side == "" {
					blocker = "gap=0"
				} else if tokenPrice < p.ResolutionMinTokenPrice || tokenPrice > p.ResolutionMaxTokenPrice {
					blocker = fmt.Sprintf("%s=%.2f oob[%.2f-%.2f]", side, tokenPrice, p.ResolutionMinTokenPrice, p.ResolutionMaxTokenPrice)
				}
			}
		}
		out = appendGate(out, "resolution", "Resolution Snipe", blocker)
	}
	if p.SnipeEnabled {
		blocker := baseBlocker()
		if blocker == "" {
			blocker = windowBlocker(windowRemSec, p.SnipeMinWindowSec, p.SnipeMaxWindowSec)
		}
		if blocker == "" {
			if absGap >= maxFlippableGap(windowRemSec) {
				blocker = fmt.Sprintf("gap=$%.1f too large to flip", absGap)
			} else if p.SnipeMaxBTCGapUSD > 0 && absGap > p.SnipeMaxBTCGapUSD {
				blocker = fmt.Sprintf("gap=$%.1f > $%.1f", absGap, p.SnipeMaxBTCGapUSD)
			} else if absGap < snipeMinGapUSD {
				blocker = fmt.Sprintf("gap=$%.1f < $%.1f", absGap, snipeMinGapUSD)
			} else if conv.WinProb < p.SnipeMinWinProb {
				blocker = fmt.Sprintf("prob=%.1f%% < %.0f%%", conv.WinProb*100, p.SnipeMinWinProb*100)
			} else {
				side, tokenPrice := sideForGap()
				if side == "" {
					blocker = "gap=0"
				} else if side == "NO" {
					switch {
					case coinbaseSpread >= -snipeCoinbaseSpreadUSD:
						blocker = fmt.Sprintf("cb=%+.1f need < -%.1f", coinbaseSpread, snipeCoinbaseSpreadUSD)
					case cvd >= -snipeCVDThresholdBTC:
						blocker = fmt.Sprintf("cvd=%.1f need < -%.1f", cvd, snipeCVDThresholdBTC)
					case tokenPrice < p.SnipeMinNoEntryPrice || tokenPrice > p.SnipeMaxNoEntryPrice:
						blocker = fmt.Sprintf("NO=%.2f oob[%.2f-%.2f]", tokenPrice, p.SnipeMinNoEntryPrice, p.SnipeMaxNoEntryPrice)
					}
				} else {
					switch {
					case coinbaseSpread <= snipeCoinbaseSpreadUSD:
						blocker = fmt.Sprintf("cb=%+.1f need > +%.1f", coinbaseSpread, snipeCoinbaseSpreadUSD)
					case cvd <= snipeCVDThresholdBTC:
						blocker = fmt.Sprintf("cvd=%.1f need > +%.1f", cvd, snipeCVDThresholdBTC)
					case tokenPrice < p.SnipeMinYesEntryPrice || tokenPrice > p.SnipeMaxYesEntryPrice:
						blocker = fmt.Sprintf("YES=%.2f oob[%.2f-%.2f]", tokenPrice, p.SnipeMinYesEntryPrice, p.SnipeMaxYesEntryPrice)
					}
				}
			}
		}
		out = appendGate(out, "reversal_snipe", "Reversal Snipe", blocker)
	}
	if p.SnipeCollapseEnabled {
		blocker := baseBlocker()
		if blocker == "" {
			blocker = windowBlocker(windowRemSec, p.SnipeCollapseMinWindowSec, p.SnipeCollapseMaxWindowSec)
		}
		if blocker == "" {
			if absGap < p.SnipeCollapseMinBTCGapUSD {
				blocker = fmt.Sprintf("gap=$%.1f < $%.1f", absGap, p.SnipeCollapseMinBTCGapUSD)
			} else if p.SnipeCollapseMaxBTCGapUSD > 0 && absGap > p.SnipeCollapseMaxBTCGapUSD {
				blocker = fmt.Sprintf("gap=$%.1f > $%.1f", absGap, p.SnipeCollapseMaxBTCGapUSD)
			} else {
				side, tokenPrice := sideForGap()
				if side == "" {
					blocker = "gap=0"
				} else if side == "NO" {
					switch {
					case coinbaseSpread >= -p.SnipeCollapseCoinbaseSpreadUSD:
						blocker = fmt.Sprintf("cb=%+.1f need < -%.1f", coinbaseSpread, p.SnipeCollapseCoinbaseSpreadUSD)
					case cvd >= -p.SnipeCollapseCVDThresholdBTC:
						blocker = fmt.Sprintf("cvd=%.1f need < -%.1f", cvd, p.SnipeCollapseCVDThresholdBTC)
					case tokenPrice < p.SnipeCollapseMinTokenPrice || tokenPrice > p.SnipeCollapseMaxTokenPrice:
						blocker = fmt.Sprintf("NO=%.2f oob[%.2f-%.2f]", tokenPrice, p.SnipeCollapseMinTokenPrice, p.SnipeCollapseMaxTokenPrice)
					}
				} else {
					switch {
					case coinbaseSpread <= p.SnipeCollapseCoinbaseSpreadUSD:
						blocker = fmt.Sprintf("cb=%+.1f need > +%.1f", coinbaseSpread, p.SnipeCollapseCoinbaseSpreadUSD)
					case cvd <= p.SnipeCollapseCVDThresholdBTC:
						blocker = fmt.Sprintf("cvd=%.1f need > +%.1f", cvd, p.SnipeCollapseCVDThresholdBTC)
					case tokenPrice < p.SnipeCollapseMinTokenPrice || tokenPrice > p.SnipeCollapseMaxTokenPrice:
						blocker = fmt.Sprintf("YES=%.2f oob[%.2f-%.2f]", tokenPrice, p.SnipeCollapseMinTokenPrice, p.SnipeCollapseMaxTokenPrice)
					}
				}
			}
		}
		out = appendGate(out, "collapse", "Collapse Snipe", blocker)
	}
	if p.DeepDiscountFadeEnabled {
		blocker := baseBlocker()
		if blocker == "" {
			blocker = windowBlocker(windowRemSec, p.DeepDiscountFadeMinWindowSec, p.DeepDiscountFadeMaxWindowSec)
		}
		if blocker == "" {
			if absGap < p.DeepDiscountFadeMinBTCGapUSD {
				blocker = fmt.Sprintf("gap=$%.1f < $%.1f", absGap, p.DeepDiscountFadeMinBTCGapUSD)
			} else if p.DeepDiscountFadeMaxBTCGapUSD > 0 && absGap > p.DeepDiscountFadeMaxBTCGapUSD {
				blocker = fmt.Sprintf("gap=$%.1f > $%.1f", absGap, p.DeepDiscountFadeMaxBTCGapUSD)
			} else {
				side, tokenPrice := sideForGap()
				if side == "" {
					blocker = "gap=0"
				} else if tokenPrice < p.DeepDiscountFadeMinTokenPrice || tokenPrice > p.DeepDiscountFadeMaxTokenPrice {
					blocker = fmt.Sprintf("%s=%.2f oob[%.2f-%.2f]", side, tokenPrice, p.DeepDiscountFadeMinTokenPrice, p.DeepDiscountFadeMaxTokenPrice)
				}
			}
		}
		out = appendGate(out, "deep_discount_fade", "Deep Discount Fade", blocker)
	}
	if p.MidFlipEnabled {
		blocker := baseBlocker()
		if blocker == "" {
			blocker = windowBlocker(windowRemSec, p.MidFlipMinWindowSec, p.MidFlipMaxWindowSec)
		}
		if blocker == "" {
			if absGap < p.MidFlipMinBTCGapUSD {
				blocker = fmt.Sprintf("gap=$%.1f < $%.1f", absGap, p.MidFlipMinBTCGapUSD)
			} else if p.MidFlipMaxBTCGapUSD > 0 && absGap > p.MidFlipMaxBTCGapUSD {
				blocker = fmt.Sprintf("gap=$%.1f > $%.1f", absGap, p.MidFlipMaxBTCGapUSD)
			} else if conv.WinProb < p.MidFlipMinWinProb {
				blocker = fmt.Sprintf("prob=%.1f%% < %.0f%%", conv.WinProb*100, p.MidFlipMinWinProb*100)
			} else if gapUSD == 0 {
				blocker = "gap=0"
			} else if gapVelMid == 0 {
				blocker = "gap velocity unavailable"
			} else {
				shrink := (gapUSD < 0 && gapVelMid >= p.MidFlipMinGapShrinkRateUSD) || (gapUSD > 0 && gapVelMid <= -p.MidFlipMinGapShrinkRateUSD)
				if !shrink {
					blocker = fmt.Sprintf("gap_vel=%.2f need |%.2f|", gapVelMid, p.MidFlipMinGapShrinkRateUSD)
				} else {
					side, tokenPrice := sideForGap()
					if tokenPrice < p.MidFlipMinTokenPrice || tokenPrice > p.MidFlipMaxTokenPrice {
						blocker = fmt.Sprintf("%s=%.2f oob[%.2f-%.2f]", side, tokenPrice, p.MidFlipMinTokenPrice, p.MidFlipMaxTokenPrice)
					}
				}
			}
		}
		out = appendGate(out, "mid_flip", "Mid Flip", blocker)
	}
	if p.LateFlipEnabled {
		blocker := baseBlocker()
		if blocker == "" {
			blocker = windowBlocker(windowRemSec, p.LateFlipMinWindowSec, p.LateFlipMaxWindowSec)
		}
		if blocker == "" {
			if absGap < p.LateFlipMinBTCGapUSD {
				blocker = fmt.Sprintf("gap=$%.1f < $%.1f", absGap, p.LateFlipMinBTCGapUSD)
			} else if p.LateFlipMaxBTCGapUSD > 0 && absGap > p.LateFlipMaxBTCGapUSD {
				blocker = fmt.Sprintf("gap=$%.1f > $%.1f", absGap, p.LateFlipMaxBTCGapUSD)
			} else if conv.WinProb < p.LateFlipMinWinProb {
				blocker = fmt.Sprintf("prob=%.1f%% < %.0f%%", conv.WinProb*100, p.LateFlipMinWinProb*100)
			} else {
				side, tokenPrice := sideForGap()
				if side == "" {
					blocker = "gap=0"
				} else if side == "NO" {
					switch {
					case coinbaseSpread >= -p.LateFlipMinCoinbaseSpreadUSD:
						blocker = fmt.Sprintf("cb=%+.1f need < -%.1f", coinbaseSpread, p.LateFlipMinCoinbaseSpreadUSD)
					case tokenPrice < p.LateFlipMinTokenPrice || tokenPrice > p.LateFlipMaxTokenPrice:
						blocker = fmt.Sprintf("NO=%.2f oob[%.2f-%.2f]", tokenPrice, p.LateFlipMinTokenPrice, p.LateFlipMaxTokenPrice)
					}
				} else {
					switch {
					case coinbaseSpread <= p.LateFlipMinCoinbaseSpreadUSD:
						blocker = fmt.Sprintf("cb=%+.1f need > +%.1f", coinbaseSpread, p.LateFlipMinCoinbaseSpreadUSD)
					case tokenPrice < p.LateFlipMinTokenPrice || tokenPrice > p.LateFlipMaxTokenPrice:
						blocker = fmt.Sprintf("YES=%.2f oob[%.2f-%.2f]", tokenPrice, p.LateFlipMinTokenPrice, p.LateFlipMaxTokenPrice)
					}
				}
			}
		}
		out = appendGate(out, "late_flip", "Late Flip", blocker)
	}
	if p.PennyBuyEnabled {
		blocker := pbReason
		if pbAllowed {
			blocker = ""
		}
		out = appendGate(out, "penny_buy", "Penny Buy", blocker)
	}
	if p.ScalpEnabled {
		blocker := baseBlocker()
		if blocker == "" {
			blocker = windowBlocker(windowRemSec, p.ScalpMinWindowSec, p.ScalpMaxWindowSec)
		}
		if blocker == "" {
			if gapUSD == 0 {
				blocker = "gap=0"
			} else if absGap < p.ScalpMinBTCGapUSD {
				blocker = fmt.Sprintf("gap=$%.1f < $%.1f", absGap, p.ScalpMinBTCGapUSD)
			} else if p.ScalpMaxBTCGapUSD > 0 && absGap > p.ScalpMaxBTCGapUSD {
				blocker = fmt.Sprintf("gap=$%.1f > $%.1f", absGap, p.ScalpMaxBTCGapUSD)
			} else if conv.WinProb < p.ScalpMinWinProb {
				blocker = fmt.Sprintf("prob=%.1f%% < %.0f%%", conv.WinProb*100, p.ScalpMinWinProb*100)
			} else {
				side, tokenPrice := sideForGap()
				if tokenPrice < p.ScalpMinTokenPrice || (p.ScalpMaxTokenPrice > 0 && tokenPrice > p.ScalpMaxTokenPrice) {
					blocker = fmt.Sprintf("%s=%.2f oob[%.2f-%.2f]", side, tokenPrice, p.ScalpMinTokenPrice, p.ScalpMaxTokenPrice)
				}
			}
		}
		out = appendGate(out, "scalp", "Scalp", blocker)
	}
	if p.ConvictionEnabled {
		blocker := conv.Blocker
		out = appendGate(out, "conviction", "Conviction", blocker)
	}
	return out
}

// PennyBuyGateStatus returns whether the penny-buy entry gate is currently open,
// along with a human-readable reason string. Read-only — no side effects.
// inPosition and buyInProgress are sourced from the Trader (not Detector state).
func (d *Detector) PennyBuyGateStatus(inPosition, buyInProgress bool) (allowed bool, reason string) {
	if !d.params.PennyBuyEnabled {
		return false, "strategy disabled"
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if inPosition {
		return false, "in position"
	}
	if buyInProgress {
		return false, "buy in progress"
	}
	now := time.Now()
	if !d.blockedUntil.IsZero() && now.Before(d.blockedUntil) {
		return false, fmt.Sprintf("cooldown %.0fs", d.blockedUntil.Sub(now).Seconds())
	}
	if d.openPrice == 0 || d.windowEnd.IsZero() {
		return false, "no active window"
	}
	windowRemSec := d.windowEnd.Sub(now).Seconds()
	if windowRemSec <= 0 {
		return false, "window ended"
	}
	minSec := float64(d.params.PennyBuyMinWindowSec)
	if minSec <= 0 {
		minSec = 90
	}
	if windowRemSec < minSec {
		return false, fmt.Sprintf("%.0fs rem (need ≥%.0fs)", windowRemSec, minSec)
	}
	if d.params.PennyBuyMaxWindowSec > 0 && windowRemSec > float64(d.params.PennyBuyMaxWindowSec) {
		if d.params.PennyBuyMaxBTCGapUSD > 0 {
			gapUSD := math.Abs(d.latestBTCPrice - d.openPrice)
			if gapUSD > d.params.PennyBuyMaxBTCGapUSD {
				return false, fmt.Sprintf("|gap|=$%.1f (need ≤$%.1f)", gapUSD, d.params.PennyBuyMaxBTCGapUSD)
			}
		}
		return false, fmt.Sprintf("%.0fs rem (need ≤%.0fs — not yet 60s into window)", windowRemSec, float64(d.params.PennyBuyMaxWindowSec))
	}
	maxPenny := d.params.PennyBuyMaxTokenPrice
	if maxPenny <= 0 {
		maxPenny = 0.01
	}
	yesPrice := d.polyYesPrice
	noPrice := math.Round((1.0-yesPrice)*100) / 100
	if yesPrice <= maxPenny {
		return true, fmt.Sprintf("READY — YES at %.0f¢  rem=%.0fs", yesPrice*100, windowRemSec)
	}
	if noPrice <= maxPenny {
		return true, fmt.Sprintf("READY — NO at %.0f¢  rem=%.0fs", noPrice*100, windowRemSec)
	}
	return false, fmt.Sprintf("YES=%.0f¢  NO=%.0f¢  (need ≤%.0f¢)", yesPrice*100, noPrice*100, maxPenny*100)
}

// EvaluateScalp checks a lightweight win-prob + gap gate with mid token pricing
// for quick scalps. Returns SignalNone if any gate fails or scalp is disabled.
func (d *Detector) EvaluateScalp() Signal {
	if !d.params.ScalpEnabled {
		return Signal{Type: SignalNone}
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.inPosition {
		return Signal{Type: SignalNone}
	}
	now := time.Now()
	if !d.blockedUntil.IsZero() && now.Before(d.blockedUntil) {
		return Signal{Type: SignalNone}
	}
	if d.openPrice == 0 || d.windowEnd.IsZero() {
		return Signal{Type: SignalNone}
	}
	windowRemSec := d.windowEnd.Sub(now).Seconds()
	if d.params.ScalpMinWindowSec > 0 && windowRemSec < float64(d.params.ScalpMinWindowSec) {
		return Signal{Type: SignalNone}
	}
	if d.params.ScalpMaxWindowSec > 0 && windowRemSec > float64(d.params.ScalpMaxWindowSec) {
		return Signal{Type: SignalNone}
	}

	btcPrice := d.latestBTCPrice
	if btcPrice == 0 {
		return Signal{Type: SignalNone}
	}
	yesPrice := d.polyYesPrice
	if yesPrice <= 0 {
		return Signal{Type: SignalNone}
	}
	noPrice := math.Round((1.0-yesPrice)*100) / 100

	gapUSD := btcPrice - d.openPrice
	absGap := math.Abs(gapUSD)
	if d.params.ScalpMinBTCGapUSD > 0 && absGap < d.params.ScalpMinBTCGapUSD {
		return Signal{Type: SignalNone}
	}
	if d.params.ScalpMaxBTCGapUSD > 0 && absGap > d.params.ScalpMaxBTCGapUSD {
		return Signal{Type: SignalNone}
	}

	sigCoeff := computeSigmaCoeff(d.dvolAnnual, d.dvolMarkPrice)
	winProb := winProbGaussianSigma(absGap, windowRemSec, sigCoeff)
	if d.params.ScalpMinWinProb > 0 && winProb < d.params.ScalpMinWinProb {
		return Signal{Type: SignalNone}
	}

	minTok := d.params.ScalpMinTokenPrice
	maxTok := d.params.ScalpMaxTokenPrice
	if gapUSD > 0 {
		if yesPrice >= minTok && (maxTok == 0 || yesPrice <= maxTok) {
			d.logger.Info("detector.EvaluateScalp: SCALP YES",
				zap.Float64("window_rem_s", windowRemSec),
				zap.Float64("yes_price", yesPrice),
				zap.Float64("gap_usd", gapUSD),
				zap.Float64("win_prob", winProb),
			)
			return Signal{
				Type:            SignalScalpYes,
				BitstampPrice:   btcPrice,
				ChainlinkPrice:  d.chainlinkPrice,
				OpenPrice:       d.openPrice,
				PolyYesPrice:    yesPrice,
				PolyNoPrice:     noPrice,
				WindowRemaining: windowRemSec,
				WinProb:         winProb,
				At:              now,
			}
		}
	}
	if gapUSD < 0 {
		if noPrice >= minTok && (maxTok == 0 || noPrice <= maxTok) {
			d.logger.Info("detector.EvaluateScalp: SCALP NO",
				zap.Float64("window_rem_s", windowRemSec),
				zap.Float64("no_price", noPrice),
				zap.Float64("gap_usd", gapUSD),
				zap.Float64("win_prob", winProb),
			)
			return Signal{
				Type:            SignalScalpNo,
				BitstampPrice:   btcPrice,
				ChainlinkPrice:  d.chainlinkPrice,
				OpenPrice:       d.openPrice,
				PolyYesPrice:    yesPrice,
				PolyNoPrice:     noPrice,
				WindowRemaining: windowRemSec,
				WinProb:         winProb,
				At:              now,
			}
		}
	}

	return Signal{Type: SignalNone}
}

// EvaluateReversalSnipe detects last-second (10–40s) reversal setups where our faster
// price feeds (Coinbase + Binance CVD) have already priced in the directional flip before
// the Chainlink oracle and the Polymarket CLOB have caught up.
//
// Both conditions must hold simultaneously:
//
//	NO snipe:  coinbasePrice < fastBTCPrice − SnipeCoinbaseSpreadUSD
//	           AND Binance 30s CVD < −SnipeCVDThresholdBTC
//	           AND no_price ≤ SnipeMaxNoEntryPrice
//
//	YES snipe: coinbasePrice > fastBTCPrice + SnipeCoinbaseSpreadUSD
//	           AND Binance 30s CVD > +SnipeCVDThresholdBTC
//	           AND yes_price ≤ SnipeMaxYesEntryPrice
//
// Positions entered via this signal always HOLD TO WINDOW RESOLUTION — no scalp exits.
func (d *Detector) EvaluateReversalSnipe() Signal {
	anyEnabled := d.params.SnipeEnabled || d.params.SnipeCollapseEnabled ||
		d.params.DeepDiscountFadeEnabled || d.params.MidFlipEnabled ||
		d.params.LateFlipEnabled || d.params.PennyBuyEnabled
	if !anyEnabled {
		return Signal{Type: SignalNone}
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.inPosition {
		return Signal{Type: SignalNone}
	}
	now := time.Now()
	if !d.blockedUntil.IsZero() && now.Before(d.blockedUntil) {
		return Signal{Type: SignalNone}
	}
	if d.openPrice == 0 || d.windowEnd.IsZero() {
		return Signal{Type: SignalNone}
	}

	windowRem := d.windowEnd.Sub(now)
	windowRemSec := windowRem.Seconds()

	inCollapseWindow := d.params.SnipeCollapseEnabled &&
		windowRemSec >= float64(d.params.SnipeCollapseMinWindowSec) &&
		windowRemSec <= float64(d.params.SnipeCollapseMaxWindowSec)
	inSnipeWindow := d.params.SnipeEnabled &&
		windowRemSec >= float64(d.params.SnipeMinWindowSec) &&
		windowRemSec <= float64(d.params.SnipeMaxWindowSec)
	inFadeWindow := d.params.DeepDiscountFadeEnabled &&
		windowRemSec >= float64(d.params.DeepDiscountFadeMinWindowSec) &&
		windowRemSec <= float64(d.params.DeepDiscountFadeMaxWindowSec)
	inMidWindow := d.params.MidFlipEnabled &&
		windowRemSec >= float64(d.params.MidFlipMinWindowSec) &&
		windowRemSec <= float64(d.params.MidFlipMaxWindowSec)
	inLateWindow := d.params.LateFlipEnabled &&
		windowRemSec >= float64(d.params.LateFlipMinWindowSec) &&
		windowRemSec <= float64(d.params.LateFlipMaxWindowSec)
	inPennyWindow := d.params.PennyBuyEnabled &&
		windowRemSec >= float64(d.params.PennyBuyMinWindowSec) &&
		(d.params.PennyBuyMaxWindowSec == 0 || windowRemSec <= float64(d.params.PennyBuyMaxWindowSec))
	if !inCollapseWindow && !inSnipeWindow && !inFadeWindow && !inMidWindow && !inLateWindow && !inPennyWindow {
		return Signal{Type: SignalNone}
	}

	btcPrice := d.latestBTCPrice
	if btcPrice == 0 || d.coinbasePrice == 0 {
		return Signal{Type: SignalNone}
	}
	yesPrice := d.polyYesPrice
	if yesPrice <= 0 {
		return Signal{Type: SignalNone}
	}
	noPrice := math.Round((1.0-yesPrice)*100) / 100

	openPrice := d.openPrice
	gapUSD := btcPrice - openPrice
	absGap := math.Abs(gapUSD)

	atr := computeATR(d.btcSamples)
	snipeScale := 1.0
	if d.params.SnipeDynamicATRRefUSD > 0 && atr > 0 {
		snipeScale = atr / d.params.SnipeDynamicATRRefUSD
		if d.params.SnipeDynamicATRMinScale > 0 && snipeScale < d.params.SnipeDynamicATRMinScale {
			snipeScale = d.params.SnipeDynamicATRMinScale
		}
		if d.params.SnipeDynamicATRMaxScale > 0 && snipeScale > d.params.SnipeDynamicATRMaxScale {
			snipeScale = d.params.SnipeDynamicATRMaxScale
		}
	}
	snipeMinGapUSD := d.params.SnipeMinBTCGapUSD * snipeScale
	snipeCoinbaseSpreadUSD := d.params.SnipeCoinbaseSpreadUSD * snipeScale
	snipeCVDThresholdBTC := d.params.SnipeCVDThresholdBTC * snipeScale

	// coinbaseSpread = coinbasePrice − fastBTCPrice
	// negative → Coinbase is LOWER (sees falling BTC before Chainlink does)
	coinbaseSpread := d.coinbasePrice - btcPrice
	cvd := d.cvdBTC()

	// ── Penny-buy (floor snap-back at $0.01) ─────────────────────────────────────
	// Fires when either side reaches $0.01 with ≥90s remaining. No BTC/CVD
	// conditions — pure asymmetric payoff: 200 shares × $1.00 = $200 on a $2 entry.
	if inPennyWindow {
		if d.params.PennyBuyMaxBTCGapUSD > 0 && absGap > d.params.PennyBuyMaxBTCGapUSD {
			return Signal{Type: SignalNone}
		}
		maxPenny := d.params.PennyBuyMaxTokenPrice
		if maxPenny <= 0 {
			maxPenny = 0.01
		}
		if yesPrice <= maxPenny {
			d.logger.Info("detector.EvaluateReversalSnipe: PENNY BUY YES",
				zap.Float64("window_rem_s", windowRemSec),
				zap.Float64("yes_price", yesPrice),
			)
			d.lastConvictionAt = now
			return Signal{
				Type:            SignalPennyBuyYes,
				BitstampPrice:   btcPrice,
				ChainlinkPrice:  d.chainlinkPrice,
				OpenPrice:       openPrice,
				PolyYesPrice:    yesPrice,
				PolyNoPrice:     noPrice,
				WindowRemaining: windowRemSec,
				At:              now,
			}
		}
		if noPrice <= maxPenny {
			d.logger.Info("detector.EvaluateReversalSnipe: PENNY BUY NO",
				zap.Float64("window_rem_s", windowRemSec),
				zap.Float64("no_price", noPrice),
			)
			d.lastConvictionAt = now
			return Signal{
				Type:            SignalPennyBuyNo,
				BitstampPrice:   btcPrice,
				ChainlinkPrice:  d.chainlinkPrice,
				OpenPrice:       openPrice,
				PolyYesPrice:    yesPrice,
				PolyNoPrice:     noPrice,
				WindowRemaining: windowRemSec,
				At:              now,
			}
		}
	}

	// ── Last-second collapse snipe ───────────────────────────────────────────────
	// Fires in the final 4–9 seconds when:
	//   • The market has almost fully settled one way (cheap token ≤ SnipeCollapseMaxTokenPrice)
	//   • Coinbase AND Binance CVD both suddenly confirm the other direction overwhelmingly
	//   • BTC still has a meaningful gap to cross
	// No win_prob gate — the crash is already in motion; statistical models don't apply here.
	// Payoff is ~50× (buying a 2¢ token that resolves $1).
	if inCollapseWindow {
		collapseGapOK := absGap >= d.params.SnipeCollapseMinBTCGapUSD &&
			(d.params.SnipeCollapseMaxBTCGapUSD == 0 || absGap <= d.params.SnipeCollapseMaxBTCGapUSD)
		// crossOK: if SnipeCollapseRequireCrossUSD > 0, only fire NO when BTC was at/below open
		// at window open (it crossed from below to above during this window = snap-back candidate).
		// Both data-backed losses had gapAtWindowOpen > 0 (persistent elevation); both wins had it ≤ 0.
		crossThresh := d.params.SnipeCollapseRequireCrossUSD
		noCollapseOK := crossThresh == 0 || d.gapAtWindowOpen <= crossThresh
		// ── Collapse NO: YES near-settled, BTC crashing back below open ──────────
		if collapseGapOK && noCollapseOK &&
			gapUSD > 0 &&
			coinbaseSpread < -d.params.SnipeCollapseCoinbaseSpreadUSD &&
			cvd < -d.params.SnipeCollapseCVDThresholdBTC &&
			noPrice > 0 &&
			(d.params.SnipeCollapseMinTokenPrice == 0 || noPrice >= d.params.SnipeCollapseMinTokenPrice) &&
			noPrice <= d.params.SnipeCollapseMaxTokenPrice {

			d.logger.Info("detector.EvaluateReversalSnipe: COLLAPSE NO signal",
				zap.Float64("window_rem_s", windowRemSec),
				zap.Float64("no_price", noPrice),
				zap.Float64("gap_usd", gapUSD),
				zap.Float64("coinbase_spread", coinbaseSpread),
				zap.Float64("cvd_btc", cvd),
			)
			d.lastConvictionAt = now
			return Signal{
				Type:            SignalCollapseSnipeNo,
				BitstampPrice:   btcPrice,
				ChainlinkPrice:  d.chainlinkPrice,
				OpenPrice:       openPrice,
				PolyYesPrice:    yesPrice,
				PolyNoPrice:     noPrice,
				WindowRemaining: windowRemSec,
				At:              now,
			}
		}
		// ── Collapse YES: NO near-settled, BTC surging back above open ───────────
		if collapseGapOK &&
			gapUSD < 0 &&
			coinbaseSpread > d.params.SnipeCollapseCoinbaseSpreadUSD &&
			cvd > d.params.SnipeCollapseCVDThresholdBTC &&
			yesPrice > 0 &&
			(d.params.SnipeCollapseMinTokenPrice == 0 || yesPrice >= d.params.SnipeCollapseMinTokenPrice) &&
			yesPrice <= d.params.SnipeCollapseMaxTokenPrice {

			d.logger.Info("detector.EvaluateReversalSnipe: COLLAPSE YES signal",
				zap.Float64("window_rem_s", windowRemSec),
				zap.Float64("yes_price", yesPrice),
				zap.Float64("gap_usd", gapUSD),
				zap.Float64("coinbase_spread", coinbaseSpread),
				zap.Float64("cvd_btc", cvd),
			)
			d.lastConvictionAt = now
			return Signal{
				Type:            SignalCollapseSnipeYes,
				BitstampPrice:   btcPrice,
				ChainlinkPrice:  d.chainlinkPrice,
				OpenPrice:       openPrice,
				PolyYesPrice:    yesPrice,
				PolyNoPrice:     noPrice,
				WindowRemaining: windowRemSec,
				At:              now,
			}
		}
		if !inFadeWindow && !inMidWindow && !inLateWindow && !inSnipeWindow {
			return Signal{Type: SignalNone}
		}
	}

	// Precompute win probability when needed by mid/late/snipe.
	var winProb float64
	if inMidWindow || inLateWindow || inSnipeWindow {
		sigCoeff := computeSigmaCoeff(d.dvolAnnual, d.dvolMarkPrice)
		winProb = winProbGaussianSigma(absGap, windowRemSec, sigCoeff)
	}

	// ── Mid-window flip (gap shrinking fast) ────────────────────────────────────
	if inMidWindow {
		midGapOK := absGap >= d.params.MidFlipMinBTCGapUSD &&
			(d.params.MidFlipMaxBTCGapUSD == 0 || absGap <= d.params.MidFlipMaxBTCGapUSD)
		gapVel := d.btcGapVelocityUSDPerSec(float64(d.params.MidFlipGapVelocityWindowSec))
		gapShrinking := (gapUSD < 0 && gapVel >= d.params.MidFlipMinGapShrinkRateUSD) ||
			(gapUSD > 0 && gapVel <= -d.params.MidFlipMinGapShrinkRateUSD)
		if midGapOK && gapShrinking && winProb >= d.params.MidFlipMinWinProb {
			if gapUSD < 0 && yesPrice >= d.params.MidFlipMinTokenPrice && yesPrice <= d.params.MidFlipMaxTokenPrice {
				d.logger.Info("detector.EvaluateReversalSnipe: MID FLIP YES",
					zap.Float64("window_rem_s", windowRemSec),
					zap.Float64("yes_price", yesPrice),
					zap.Float64("gap_usd", gapUSD),
					zap.Float64("gap_vel", gapVel),
					zap.Float64("win_prob", winProb),
				)
				d.lastConvictionAt = now
				return Signal{
					Type:            SignalMidFlipYes,
					BitstampPrice:   btcPrice,
					ChainlinkPrice:  d.chainlinkPrice,
					OpenPrice:       openPrice,
					PolyYesPrice:    yesPrice,
					PolyNoPrice:     noPrice,
					WindowRemaining: windowRemSec,
					WinProb:         winProb,
					At:              now,
				}
			}
			if gapUSD > 0 && noPrice >= d.params.MidFlipMinTokenPrice && noPrice <= d.params.MidFlipMaxTokenPrice {
				d.logger.Info("detector.EvaluateReversalSnipe: MID FLIP NO",
					zap.Float64("window_rem_s", windowRemSec),
					zap.Float64("no_price", noPrice),
					zap.Float64("gap_usd", gapUSD),
					zap.Float64("gap_vel", gapVel),
					zap.Float64("win_prob", winProb),
				)
				d.lastConvictionAt = now
				return Signal{
					Type:            SignalMidFlipNo,
					BitstampPrice:   btcPrice,
					ChainlinkPrice:  d.chainlinkPrice,
					OpenPrice:       openPrice,
					PolyYesPrice:    yesPrice,
					PolyNoPrice:     noPrice,
					WindowRemaining: windowRemSec,
					WinProb:         winProb,
					At:              now,
				}
			}
		}
	}

	// ── Late flip (strong Coinbase, no CVD gate) ──────────────────────────────
	if inLateWindow {
		lateGapOK := absGap >= d.params.LateFlipMinBTCGapUSD &&
			(d.params.LateFlipMaxBTCGapUSD == 0 || absGap <= d.params.LateFlipMaxBTCGapUSD)
		if lateGapOK && winProb >= d.params.LateFlipMinWinProb {
			if gapUSD < 0 &&
				coinbaseSpread > d.params.LateFlipMinCoinbaseSpreadUSD &&
				yesPrice >= d.params.LateFlipMinTokenPrice &&
				yesPrice <= d.params.LateFlipMaxTokenPrice {

				d.logger.Info("detector.EvaluateReversalSnipe: LATE FLIP YES",
					zap.Float64("window_rem_s", windowRemSec),
					zap.Float64("yes_price", yesPrice),
					zap.Float64("gap_usd", gapUSD),
					zap.Float64("coinbase_spread", coinbaseSpread),
					zap.Float64("win_prob", winProb),
				)
				d.lastConvictionAt = now
				return Signal{
					Type:            SignalLateFlipYes,
					BitstampPrice:   btcPrice,
					ChainlinkPrice:  d.chainlinkPrice,
					OpenPrice:       openPrice,
					PolyYesPrice:    yesPrice,
					PolyNoPrice:     noPrice,
					WindowRemaining: windowRemSec,
					WinProb:         winProb,
					At:              now,
				}
			}
			if gapUSD > 0 &&
				coinbaseSpread < -d.params.LateFlipMinCoinbaseSpreadUSD &&
				noPrice >= d.params.LateFlipMinTokenPrice &&
				noPrice <= d.params.LateFlipMaxTokenPrice {

				d.logger.Info("detector.EvaluateReversalSnipe: LATE FLIP NO",
					zap.Float64("window_rem_s", windowRemSec),
					zap.Float64("no_price", noPrice),
					zap.Float64("gap_usd", gapUSD),
					zap.Float64("coinbase_spread", coinbaseSpread),
					zap.Float64("win_prob", winProb),
				)
				d.lastConvictionAt = now
				return Signal{
					Type:            SignalLateFlipNo,
					BitstampPrice:   btcPrice,
					ChainlinkPrice:  d.chainlinkPrice,
					OpenPrice:       openPrice,
					PolyYesPrice:    yesPrice,
					PolyNoPrice:     noPrice,
					WindowRemaining: windowRemSec,
					WinProb:         winProb,
					At:              now,
				}
			}
		}
		if !inFadeWindow && !inSnipeWindow {
			return Signal{Type: SignalNone}
		}
	}

	// ── Deep-discount fade (contrarian 2–5c tokens) ─────────────────────────────
	// Fades extreme pricing when there is still time for a snap-back.
	if inFadeWindow {
		fadeGapOK := absGap >= d.params.DeepDiscountFadeMinBTCGapUSD &&
			(d.params.DeepDiscountFadeMaxBTCGapUSD == 0 || absGap <= d.params.DeepDiscountFadeMaxBTCGapUSD)
		if fadeGapOK &&
			gapUSD < 0 &&
			yesPrice >= d.params.DeepDiscountFadeMinTokenPrice &&
			yesPrice <= d.params.DeepDiscountFadeMaxTokenPrice {

			d.logger.Info("detector.EvaluateReversalSnipe: DEEP DISCOUNT YES fade",
				zap.Float64("window_rem_s", windowRemSec),
				zap.Float64("yes_price", yesPrice),
				zap.Float64("gap_usd", gapUSD),
				zap.Float64("coinbase_spread", coinbaseSpread),
				zap.Float64("cvd_btc", cvd),
			)
			d.lastConvictionAt = now
			return Signal{
				Type:            SignalDeepDiscountFadeYes,
				BitstampPrice:   btcPrice,
				ChainlinkPrice:  d.chainlinkPrice,
				OpenPrice:       openPrice,
				PolyYesPrice:    yesPrice,
				PolyNoPrice:     noPrice,
				WindowRemaining: windowRemSec,
				At:              now,
			}
		}
		if fadeGapOK &&
			gapUSD > 0 &&
			noPrice >= d.params.DeepDiscountFadeMinTokenPrice &&
			noPrice <= d.params.DeepDiscountFadeMaxTokenPrice {

			d.logger.Info("detector.EvaluateReversalSnipe: DEEP DISCOUNT NO fade",
				zap.Float64("window_rem_s", windowRemSec),
				zap.Float64("no_price", noPrice),
				zap.Float64("gap_usd", gapUSD),
				zap.Float64("coinbase_spread", coinbaseSpread),
				zap.Float64("cvd_btc", cvd),
			)
			d.lastConvictionAt = now
			return Signal{
				Type:            SignalDeepDiscountFadeNo,
				BitstampPrice:   btcPrice,
				ChainlinkPrice:  d.chainlinkPrice,
				OpenPrice:       openPrice,
				PolyYesPrice:    yesPrice,
				PolyNoPrice:     noPrice,
				WindowRemaining: windowRemSec,
				At:              now,
			}
		}
		if !inSnipeWindow {
			return Signal{Type: SignalNone}
		}
	}

	// ── Normal reversal snipe (rem 10–40s) ──────────────────────────────────────
	// Block entries where the gap is already too large to flip in the remaining time.
	// Thresholds: ≥35s → <$80, ≥27s → <$60, else → <$40.
	// This rejects ~$2/s+ required-velocity situations that are statistically unflippable.
	var maxFlippableGap float64
	switch {
	case windowRemSec >= 35:
		maxFlippableGap = 80
	case windowRemSec >= 27:
		maxFlippableGap = 60
	default:
		maxFlippableGap = 40
	}
	if absGap >= maxFlippableGap {
		return Signal{Type: SignalNone}
	}
	if d.params.SnipeMaxBTCGapUSD > 0 && absGap > d.params.SnipeMaxBTCGapUSD {
		return Signal{Type: SignalNone}
	}
	// Require a meaningful gap — the cheap token must reflect BTC being on the wrong side,
	// not just market uncertainty at near-fair-value.
	if snipeMinGapUSD > 0 && absGap < snipeMinGapUSD {
		return Signal{Type: SignalNone}
	}

	// Reject statistically marginal flips — even with momentum, we need >55% to justify fees.
	if d.params.SnipeMinWinProb > 0 && winProb < d.params.SnipeMinWinProb {
		return Signal{Type: SignalNone}
	}

	// ── NO snipe ────────────────────────────────────────────────────────────────
	// Coinbase already seeing BTC lower than the fast feed (real BTC heading down),
	// Binance net-sellers confirming, and the CLOB hasn't fully repriced yet.
	// gapUSD > 0: BTC is above open (YES winning, NO is cheap) — reversal means BTC
	// crosses below open, confirming NO. If BTC is already below open, NO is expensive
	// and the price gate blocks it anyway; the explicit check makes the intent clear.
	if gapUSD > 0 &&
		coinbaseSpread < -snipeCoinbaseSpreadUSD &&
		cvd < -snipeCVDThresholdBTC &&
		noPrice >= d.params.SnipeMinNoEntryPrice &&
		noPrice <= d.params.SnipeMaxNoEntryPrice {

		d.logger.Info("detector.EvaluateReversalSnipe: NO signal",
			zap.Float64("window_rem_s", windowRemSec),
			zap.Float64("no_price", noPrice),
			zap.Float64("gap_usd", gapUSD),
			zap.Float64("coinbase_spread", coinbaseSpread),
			zap.Float64("cvd_btc", cvd),
			zap.Float64("win_prob", winProb),
		)
		d.lastConvictionAt = now
		return Signal{
			Type:            SignalReversalSnipeNo,
			BitstampPrice:   btcPrice,
			ChainlinkPrice:  d.chainlinkPrice,
			OpenPrice:       openPrice,
			PolyYesPrice:    yesPrice,
			PolyNoPrice:     noPrice,
			WindowRemaining: windowRemSec,
			WinProb:         winProb,
			At:              now,
		}
	}

	// ── YES snipe ───────────────────────────────────────────────────────────────
	// Coinbase already pricing BTC higher, Binance net-buyers confirming.
	// gapUSD < 0: BTC is below open (NO winning, YES is cheap) — reversal means BTC
	// crosses above open, confirming YES.
	if gapUSD < 0 &&
		coinbaseSpread > snipeCoinbaseSpreadUSD &&
		cvd > snipeCVDThresholdBTC &&
		yesPrice >= d.params.SnipeMinYesEntryPrice &&
		yesPrice <= d.params.SnipeMaxYesEntryPrice {

		d.logger.Info("detector.EvaluateReversalSnipe: YES signal",
			zap.Float64("window_rem_s", windowRemSec),
			zap.Float64("yes_price", yesPrice),
			zap.Float64("gap_usd", gapUSD),
			zap.Float64("coinbase_spread", coinbaseSpread),
			zap.Float64("cvd_btc", cvd),
			zap.Float64("win_prob", winProb),
		)
		d.lastConvictionAt = now
		return Signal{
			Type:            SignalReversalSnipeYes,
			BitstampPrice:   btcPrice,
			ChainlinkPrice:  d.chainlinkPrice,
			OpenPrice:       openPrice,
			PolyYesPrice:    yesPrice,
			PolyNoPrice:     noPrice,
			WindowRemaining: windowRemSec,
			WinProb:         winProb,
			At:              now,
		}
	}

	return Signal{Type: SignalNone}
}
