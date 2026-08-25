package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/polyarb/display"
	"github.com/polyarb/polymarket"
)

// isNetworkError returns true for errors that indicate the HTTP request was sent
// but the response was never received (timeout, EOF, connection reset). In these
// cases a PlaceOrder POST may have reached the server and created an order on the
// CLOB even though our client got an error  requiring orphan recovery.
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	if strings.Contains(s, "context deadline exceeded") ||
		strings.Contains(s, "Client.Timeout") ||
		strings.Contains(s, "EOF") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "i/o timeout") {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

func isAmbiguousPlaceOrderParseError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "parse order response") {
		return true
	}
	return strings.Contains(s, "unexpected end of json input")
}

func isOrderLookupNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "get order api error 404")
}

type pairArbUTCMinuteWindow struct {
	start int
	end   int
}

func normalizePairArbScheduleMode(raw string) string {
	mode := strings.ToUpper(strings.TrimSpace(raw))
	switch mode {
	case "", "CONTINUOUS":
		return "CONTINUOUS"
	case "TIMEZONES", "TIMEZONE", "WINDOWS", "SCHEDULED":
		return "TIMEZONES"
	default:
		return "CONTINUOUS"
	}
}

func parsePairArbScheduleMinute(raw string, allow24 bool) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("empty time value")
	}
	hourStr := raw
	minuteStr := "0"
	if strings.Contains(raw, ":") {
		parts := strings.SplitN(raw, ":", 2)
		hourStr = strings.TrimSpace(parts[0])
		minuteStr = strings.TrimSpace(parts[1])
	}
	hour, err := strconv.Atoi(hourStr)
	if err != nil {
		return 0, fmt.Errorf("invalid hour %q", raw)
	}
	minute, err := strconv.Atoi(minuteStr)
	if err != nil {
		return 0, fmt.Errorf("invalid minute %q", raw)
	}
	if hour == 24 && minute == 0 && allow24 {
		return 24 * 60, nil
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, fmt.Errorf("time out of range %q", raw)
	}
	return hour*60 + minute, nil
}

func parsePairArbScheduleWindowsUTC(raw string) ([]pairArbUTCMinuteWindow, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("no UTC schedule windows configured")
	}
	tokens := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r'
	})
	windows := make([]pairArbUTCMinuteWindow, 0, len(tokens))
	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		parts := strings.SplitN(token, "-", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid schedule window %q (expected HH:MM-HH:MM)", token)
		}
		start, err := parsePairArbScheduleMinute(parts[0], false)
		if err != nil {
			return nil, fmt.Errorf("invalid schedule window %q: %w", token, err)
		}
		end, err := parsePairArbScheduleMinute(parts[1], true)
		if err != nil {
			return nil, fmt.Errorf("invalid schedule window %q: %w", token, err)
		}
		if start == end {
			return []pairArbUTCMinuteWindow{{start: 0, end: 24 * 60}}, nil
		}
		if end > start {
			windows = append(windows, pairArbUTCMinuteWindow{start: start, end: end})
			continue
		}
		windows = append(windows,
			pairArbUTCMinuteWindow{start: start, end: 24 * 60},
			pairArbUTCMinuteWindow{start: 0, end: end},
		)
	}
	if len(windows) == 0 {
		return nil, fmt.Errorf("no UTC schedule windows configured")
	}
	return windows, nil
}

func pairArbScheduleAllowsEntry(modeRaw, windowsRaw string, at time.Time) (bool, string) {
	mode := normalizePairArbScheduleMode(modeRaw)
	if mode != "TIMEZONES" {
		return true, ""
	}
	windows, err := parsePairArbScheduleWindowsUTC(windowsRaw)
	if err != nil {
		return false, fmt.Sprintf("invalid schedule windows: %v", err)
	}
	if at.IsZero() {
		at = time.Now().UTC()
	} else {
		at = at.UTC()
	}
	minuteOfDay := at.Hour()*60 + at.Minute()
	for _, w := range windows {
		if minuteOfDay >= w.start && minuteOfDay < w.end {
			return true, ""
		}
	}
	return false, fmt.Sprintf("outside UTC schedule at %s (mode=%s windows=%q)", at.Format("15:04"), mode, strings.TrimSpace(windowsRaw))
}

// Position tracks an open trade.
type Position struct {
	OrderID           string
	TokenID           string
	ConditionID       string
	StrategyName      string
	IsNoSide          bool
	BuyPrice          float64
	Shares            float64
	USDSpent          float64
	FeeShares         float64
	TargetPrice       float64
	OpenPrice         float64
	WindowEnd         time.Time
	OpenedAt          time.Time
	ExpiresAt         time.Time
	SellPending       bool
	SellDelayUntil    time.Time
	ActiveSellOrderID string
	LastSellLookupAt  time.Time `json:"-"`
	IsResolutionSnipe bool      `json:"is_resolution_snipe,omitempty"`
	IsEarlyResolution bool      `json:"is_early_resolution,omitempty"`
	IsConviction      bool      `json:"is_conviction,omitempty"`
	IsReversalSnipe   bool      `json:"is_reversal_snipe,omitempty"`
	IsPennyBuy        bool      `json:"is_penny_buy,omitempty"`
	IsScalp           bool      `json:"is_scalp,omitempty"`
	IsFlipped         bool      `json:"is_flipped,omitempty"`
	IsOrderFlowLed    bool      `json:"is_order_flow_led,omitempty"`
	EntryBTCPrice     float64
	EntryEdgeUSD      float64
	EntryCLLagUSD     float64
	EntryATR          float64
	EntryWindowRemSec float64
	EntryWinProb      float64
}

func (p *Position) Unrealized(currentPrice float64) float64 {
	if p == nil {
		return 0
	}
	return p.Shares*currentPrice - p.USDSpent
}

type PairArbPosition struct {
	ConditionID string    `json:"condition_id"`
	YesTokenID  string    `json:"yes_token_id"`
	NoTokenID   string    `json:"no_token_id"`
	LeadSide    string    `json:"lead_side"`
	OpenPrice   float64   `json:"open_price"`
	WindowEnd   time.Time `json:"window_end"`
	OpenedAt    time.Time `json:"opened_at"`
	HedgeBy     time.Time `json:"hedge_by,omitempty"`
	BalancedAt  time.Time `json:"balanced_at,omitempty"`
	// LockedProfitCents overrides the default hedge locked-profit target for this position.
	// Non-zero for CVD momentum entries (typically 3¢ vs the standard 8¢).
	LockedProfitCents  float64 `json:"locked_profit_cents,omitempty"`
	YesShares          float64 `json:"yes_shares"`
	NoShares           float64 `json:"no_shares"`
	YesUSDSpent        float64 `json:"yes_usd_spent"`
	NoUSDSpent         float64 `json:"no_usd_spent"`
	YesWalletConfirmed bool    `json:"yes_wallet_confirmed,omitempty"`
	NoWalletConfirmed  bool    `json:"no_wallet_confirmed,omitempty"`

	YesExitOrderID     string    `json:"yes_exit_order_id,omitempty"`
	NoExitOrderID      string    `json:"no_exit_order_id,omitempty"`
	YesExitOrderShares float64   `json:"yes_exit_order_shares,omitempty"`
	NoExitOrderShares  float64   `json:"no_exit_order_shares,omitempty"`
	ExitOrdersPlacedAt time.Time `json:"exit_orders_placed_at,omitempty"`
	ExitState          string    `json:"exit_state,omitempty"`
	ExitStateNote      string    `json:"exit_state_note,omitempty"`
	ExitStateUpdatedAt time.Time `json:"exit_state_updated_at,omitempty"`
	ImbalanceAdds      int       `json:"imbalance_adds,omitempty"`
	LastImbalanceAt    time.Time `json:"last_imbalance_at,omitempty"`
	LastExitPollAt     time.Time `json:"-"`

	// DCA+Hedge state: set by OnDCAHedgeSignal; checked in managePairArbPosition.
	DCAHedgeMovedSide    string  `json:"dca_hedge_moved_side,omitempty"`    // "YES" or "NO"
	DCAHedgeTriggerPrice float64 `json:"dca_hedge_trigger_price,omitempty"` // moved-side price at trigger
	DCAHedgeDCAFired     bool    `json:"dca_hedge_dca_fired,omitempty"`     // true after the DCA add executes
}

func (p *PairArbPosition) hasBothLegs() bool { return p != nil && p.YesShares > 0 && p.NoShares > 0 }

func (p *PairArbPosition) isBalanced() bool {
	if p == nil || p.YesShares <= 0 || p.NoShares <= 0 {
		return false
	}
	return p.lockedShares() >= p.totalSpent()
}

func (p *PairArbPosition) totalSpent() float64 {
	if p == nil {
		return 0
	}
	return p.YesUSDSpent + p.NoUSDSpent
}

func (p *PairArbPosition) lockedShares() float64 {
	if p == nil {
		return 0
	}
	return math.Min(p.YesShares, p.NoShares)
}

func (p *PairArbPosition) lockedProfit() float64 {
	if p == nil {
		return 0
	}
	return p.lockedShares() - p.totalSpent()
}

func (p *PairArbPosition) meetsLockedProfitTarget(targetPerShare float64) (bool, float64, float64) {
	if p == nil || p.lockedShares() <= pairArbShareDust {
		return false, 0, 0
	}
	if targetPerShare < 0 {
		targetPerShare = 0
	}

	lockedProfit := p.lockedProfit()
	requiredProfit := p.lockedShares() * targetPerShare

	// Tiny epsilon only protects against floating-point representation.
	return lockedProfit+1e-9 >= requiredProfit, lockedProfit, requiredProfit
}

func (p *PairArbPosition) matchedCost() float64 {
	if p == nil {
		return 0
	}
	ls := p.lockedShares()
	if ls <= 0 {
		return 0
	}
	return ls * (p.sideAveragePrice("YES") + p.sideAveragePrice("NO"))
}

func (p *PairArbPosition) residualPosition() (side string, shares float64, avgPrice float64) {
	if p == nil {
		return "", 0, 0
	}
	if p.YesShares > p.NoShares {
		return "YES", p.YesShares - p.NoShares, p.sideAveragePrice("YES")
	}
	if p.NoShares > p.YesShares {
		return "NO", p.NoShares - p.YesShares, p.sideAveragePrice("NO")
	}
	return "", 0, 0
}

func (p *PairArbPosition) sideAveragePrice(side string) float64 {
	if p == nil {
		return 0
	}
	if strings.EqualFold(side, "YES") {
		if p.YesShares <= 0 {
			return 0
		}
		return p.YesUSDSpent / p.YesShares
	}
	if p.NoShares <= 0 {
		return 0
	}
	return p.NoUSDSpent / p.NoShares
}

func (p *PairArbPosition) rebalanceState() (buySide string, deficit float64, anchorAvgPrice float64) {
	if p == nil {
		return "", 0, 0
	}
	if p.YesShares < p.NoShares {
		return "YES", p.NoShares - p.YesShares, p.sideAveragePrice("NO")
	}
	if p.NoShares < p.YesShares {
		return "NO", p.YesShares - p.NoShares, p.sideAveragePrice("YES")
	}
	return "", 0, 0
}

func (p *PairArbPosition) markToMarket(yesPrice float64, noPrices ...float64) float64 {
	if p == nil || yesPrice <= 0 {
		return 0
	}

	noPrice := 0.0
	if len(noPrices) > 0 {
		noPrice = noPrices[0]
	}

	if noPrice <= 0 {
		// Legacy callers still only supply YES.
		// LIVE remains blocked until every such caller carries Kalshi NO.
		noPrice = math.Round((1.0-yesPrice)*100) / 100
	}

	return p.YesShares*yesPrice + p.NoShares*noPrice - p.totalSpent()
}

type TraderConfig struct {
	TradeSizeUSD                 float64
	MaxHoldDuration              time.Duration
	PaperTrade                   bool
	PaperStartBalance            float64
	JournalFile                  string
	CfgMinEdgeUSD                float64
	StopLossCents                float64
	ScalpTargetCents             float64
	ConvictionTradeSizeUSD       float64
	ConvictionScalpTargetCents   float64
	ConvictionStopLossCents      float64
	ConvictionLastSecSec         int
	ProfitTakeThreshold          float64
	QuickSellThreshold           float64
	MaxSessionLossUSD            float64
	MaxSessionProfitUSD          float64
	MaxTradesPerSession          int
	MaxConsecutiveLosses         int
	PairArbTradeSizeUSD          float64
	PairArbMinTokenPrice         float64
	PairArbMaxTokenPrice         float64
	PairArbCarryEarlySec         int
	PairArbCarryOppDiscountCents float64
	PairArbMinLockedProfitCents  float64
	// PairArbCVDMomentumLockedProfitCents is the looser hedge target (in cents) used for
	// CVD momentum entries where the opposite token starts near 0.50. 0 = use default.
	PairArbCVDMomentumLockedProfitCents                float64
	PairArbHedgeTimeoutSec                             int
	PairArbStopLossCents                               float64
	PairArbStopLossMinHoldSec                          int
	PairArbStopLossMinGapAgainstUSD                    float64
	PairArbUnprofitableAbortGraceSec                   int
	PairArbUnprofitableAbortMinGapAgainstUSD           float64
	PairArbLeadBuySlipTicks                            int
	PairArbLeadBuyTimeoutSec                           int
	PairArbLeadOrderType                               polymarket.OrderType
	PairArbDualPrePlace                                bool
	PairArbHedgePreOffsetCents                         float64
	PairArbMaxSignalAgeSec                             int
	PairArbMaxCLAgeSec                                 int
	PairArbSellAt99                                    bool
	PairArbContinuousImbalanceEnabled                  bool
	PairArbContinuousImbalanceTradeSizeUSD             float64
	PairArbContinuousImbalanceMinSignalGapUSD          float64
	PairArbContinuousImbalanceMinPriceImprovementCents float64
	PairArbContinuousImbalanceAllowMomentum            bool
	PairArbContinuousImbalanceCooldownSec              int
	PairArbContinuousImbalanceMaxAdds                  int
	PairArbContinuousImbalanceMaxUSDPerSide            float64
	PairArbContinuousImbalanceMaxGapUSD                float64
	PairArbStopCooldownSec                             int
	PairArbScheduleMode                                string
	PairArbScheduleWindowsUTC                          string
	ResolutionTradeSizeUSD                             float64
	ResolutionOrderType                                polymarket.OrderType
	ResolutionBuyLimitPrice                            float64

	// DCA+Hedge strategy config.
	DCAHedgeTradeSizeUSD    float64 // per-leg USD limit (informational; actual size driven by signal shares)
	DCAHedgeDCAReversal     float64 // price drop from trigger that fires the DCA add (default 0.20)
	DCAHedgeDCAAddShares    float64 // shares to buy on DCA add (default 5)
	DCAHedgeOppLegSlipTicks int     // slip ticks for the opp (non-moved) leg; -1 = use PairArbLeadBuySlipTicks

	// UsePolymarketClaiming delegates CTF redemption to Polymarket's own claiming
	// infrastructure. When true enqueueClaimRetry is a no-op.
	UsePolymarketClaiming bool
}

// pendingBuyState captures an in-flight buy order that was placed but not yet recorded
// as an open position. Written to disk immediately after PlaceOrder succeeds so a crash
// between PlaceOrder and position-open can be recovered on restart.
type pendingBuyState struct {
	OrderID      string    `json:"order_id"`
	TokenID      string    `json:"token_id"`
	IsNoSide     bool      `json:"is_no_side"`
	FillPrice    float64   `json:"fill_price"`
	RawShares    float64   `json:"raw_shares"`
	FeeShares    float64   `json:"fee_shares"`
	ActualShares float64   `json:"actual_shares"`
	TargetPrice  float64   `json:"target_price"`
	FeeRateBps   string    `json:"fee_rate_bps"`
	WindowEnd    time.Time `json:"window_end"`
	ExpiresAt    time.Time `json:"expires_at"`
	PlacedAt     time.Time `json:"placed_at"`
}

// pairArbPendingOrderState tracks an ambiguous pair-arb buy submit where the
// exchange accepted the request but fill ownership could not be confirmed.
// While present, new pair-arb entries are blocked until reconciliation succeeds.
type pairArbPendingOrderState struct {
	OrderID       string    `json:"order_id"`
	TokenID       string    `json:"token_id"`
	RequestName   string    `json:"request_name"`
	Origin        string    `json:"origin,omitempty"`
	ConditionID   string    `json:"condition_id,omitempty"`
	WindowEnd     time.Time `json:"window_end,omitempty"`
	YesTokenID    string    `json:"yes_token_id,omitempty"`
	NoTokenID     string    `json:"no_token_id,omitempty"`
	LeadSide      string    `json:"lead_side,omitempty"`
	PlacedAt      time.Time `json:"placed_at"`
	RequestedSize float64   `json:"requested_size,omitempty"`
}

// TradeRecord captures a completed round-trip (entry + exit) for the session journal.
// All fields are JSON-tagged so the record can be appended as a JSONL line for
// off-line analysis via `polyarbpro --analyze`.
type TradeRecord struct {
	OpenedAt time.Time `json:"opened_at"`
	ClosedAt time.Time `json:"closed_at"`
	HeldSec  float64   `json:"held_sec"`
	Strategy string    `json:"strategy"` // "lag", "resolution", "early_resolution", "flash"
	Side     string    `json:"side"`     // "YES", "NO", or pair labels like "PAIR_MATCHED"
	// BTC & market context at entry
	EntryBTCPrice     float64 `json:"entry_btc_price"`
	EntryEdgeUSD      float64 `json:"entry_edge_usd"`   // |BTC Ãƒâ€¹Ã¢â‚¬Â  open| at entry
	EntryCLLagUSD     float64 `json:"entry_cl_lag_usd"` // Chainlink lag at entry
	EntryATR          float64 `json:"entry_atr"`        // ATR over observation window at entry
	EntryWindowRemSec float64 `json:"entry_window_rem_sec"`
	EntryOpenPrice    float64 `json:"entry_open_price"`
	// Gaussian win probability at entry (conviction trades only)
	EntryWinProb float64 `json:"entry_win_prob,omitempty"`
	// Fill & outcome
	BuyPrice  float64 `json:"buy_price"`
	SellPrice float64 `json:"sell_price"`
	Shares    float64 `json:"shares"`
	USDSpent  float64 `json:"usd_spent"`
	PnL       float64 `json:"pnl"`
	Reason    string  `json:"reason"` // why the position was closed
	// Config snapshot at trade time (for counterfactual analysis)
	CfgMinEdgeUSD             float64 `json:"cfg_min_edge_usd"`
	CfgScalpTargetCents       float64 `json:"cfg_scalp_target_cents"`
	CfgStopLossCents          float64 `json:"cfg_stop_loss_cents"`
	CfgTradeSizeUSD           float64 `json:"cfg_trade_size_usd"`
	CfgConvictionTradeSizeUSD float64 `json:"cfg_conviction_trade_size_usd,omitempty"` // non-zero when conviction path used a separate size
}

type DashboardPositionSnapshot struct {
	HasPosition            bool
	Side                   string
	Type                   string
	BuyPrice               float64
	Shares                 float64
	USDSpent               float64
	UnrealizedPnL          float64
	HeldSec                float64
	PairActive             bool
	PairLeadSide           string
	PairYesFilled          bool
	PairNoFilled           bool
	PairYesAvgPrice        float64
	PairNoAvgPrice         float64
	PairYesShares          float64
	PairNoShares           float64
	PairYesSpent           float64
	PairNoSpent            float64
	PairYesWalletConfirmed bool
	PairNoWalletConfirmed  bool
	PairLockedShares       float64
	PairMatchedCost        float64
	PairExpectedProfit     float64
	PairExpectedROIPct     float64
	PairResidualSide       string
	PairResidualShares     float64
	PairHedgeSide          string
	PairHedgeMaxPrice      float64
	PairMarkToMarket       float64
	PairExitState          string
	PairExitStateNote      string
	PairYesExitOrderID     string
	PairNoExitOrderID      string
	PairExitPlacedAt       time.Time
}

type DashboardClaimSnapshot struct {
	PendingCount    int
	FailedCount     int
	LastStatus      string
	LastMessage     string
	LastConditionID string
	LastSide        string
	LastUpdatedAt   time.Time
	LastAttempt     int
	NextRetryAt     time.Time
}

type pendingClaim struct {
	ConditionID   string    `json:"condition_id"`
	StrategyName  string    `json:"strategy_name"`
	IsNoSide      bool      `json:"is_no_side"`
	Shares        float64   `json:"shares"`
	WindowEnd     time.Time `json:"window_end"`
	AddedAt       time.Time `json:"added_at"`
	LastAttemptAt time.Time `json:"last_attempt_at,omitempty"`
	NextRetryAt   time.Time `json:"next_retry_at,omitempty"`
	AttemptCount  int       `json:"attempt_count,omitempty"`
	Claimable     bool      `json:"claimable"`
	Status        string    `json:"status,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
}

type pairExitOrderStatusSnapshot struct {
	CanceledOrExpired bool
	UpdatedAt         time.Time
}

type tickSizeSnapshot struct {
	TickSize  string
	FetchedAt time.Time
}

type pairCostBasisSample struct {
	YesSize float64
	NoSize  float64
	YesAvg  float64
	NoAvg   float64
}

func parseOrderShares(raw string) float64 {
	if raw == "" {
		return 0
	}
	shares, err := strconv.ParseFloat(raw, 64)
	if err != nil || shares <= 0 {
		return 0
	}
	return shares
}

func parseConditionalTokenShares(raw string) float64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	rawI, ok := new(big.Int).SetString(raw, 10)
	if !ok || rawI.Sign() <= 0 {
		return 0
	}
	shares, _ := new(big.Rat).SetFrac(rawI, big.NewInt(1_000_000)).Float64()
	if shares <= 0 {
		return 0
	}
	return math.Floor(shares*100) / 100
}

func activityRequestLabel(requestName string) string {
	return strings.ReplaceAll(requestName, "_", " ")
}

func logTimedActivityInfo(msg string) {
	display.Info(msg)
}

func logTimedActivityWarn(msg string) {
	display.Warn(msg)
}

func isRetryableFAKNoMatchError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no orders found to match") && strings.Contains(message, "fak order")
}

func computeFAKBuySizing(tradeSizeUSD, limitPrice, fillPrice float64, feeRateBps string, enforceMinOrder bool) (rawShares, feeShares, actualShares float64, err error) {
	if tradeSizeUSD <= 0 || limitPrice <= 0 || fillPrice <= 0 {
		return 0, 0, 0, fmt.Errorf("invalid FAK buy sizing inputs")
	}
	rawShares = tradeSizeUSD / limitPrice
	rawShares = math.Round(rawShares*100) / 100
	feeShares = polymarket.ComputeBuyFeeShares(rawShares, fillPrice, feeRateBps)
	actualShares = rawShares - feeShares
	if actualShares <= 0 {
		return 0, 0, 0, fmt.Errorf("fee eats all shares (raw=%.2f fee=%.4f)", rawShares, feeShares)
	}
	if enforceMinOrder && actualShares < polymarket.MinOrderShares {
		feeRate := 0.0
		if bps, parseErr := strconv.ParseFloat(feeRateBps, 64); parseErr == nil && bps > 0 {
			feeRate = bps / 10000.0
		}
		minRaw := polymarket.MinOrderShares / (1.0 - feeRate*(1.0-fillPrice))
		rawShares = math.Ceil(minRaw*100) / 100
		feeShares = polymarket.ComputeBuyFeeShares(rawShares, fillPrice, feeRateBps)
		actualShares = rawShares - feeShares
		if actualShares <= 0 {
			return 0, 0, 0, fmt.Errorf("fee eats all shares after min-order bump (raw=%.2f fee=%.4f)", rawShares, feeShares)
		}
	}
	actualShares = math.Floor(actualShares*100) / 100
	return rawShares, feeShares, actualShares, nil
}

func marketBuyOrderNotional(rawShares, limitPrice float64) float64 {
	if rawShares <= 0 || limitPrice <= 0 {
		return 0
	}
	return math.Round(rawShares*limitPrice*10000) / 10000
}

var clobCollateralShortfallPattern = regexp.MustCompile(`(?i)balance:\s*(\d+)\s*,\s*sum of matched orders:\s*(\d+)\s*,\s*order amount:\s*(\d+)`)

func parseCLOBCollateralShortfall(err error) (balanceRaw, matchedRaw, orderRaw int64, ok bool) {
	if err == nil {
		return 0, 0, 0, false
	}
	msg := err.Error()
	if !strings.Contains(strings.ToLower(msg), "not enough balance / allowance") {
		return 0, 0, 0, false
	}
	m := clobCollateralShortfallPattern.FindStringSubmatch(msg)
	if len(m) != 4 {
		return 0, 0, 0, false
	}
	b, bErr := strconv.ParseInt(m[1], 10, 64)
	mv, mErr := strconv.ParseInt(m[2], 10, 64)
	o, oErr := strconv.ParseInt(m[3], 10, 64)
	if bErr != nil || mErr != nil || oErr != nil || o <= 0 {
		return 0, 0, 0, false
	}
	return b, mv, o, true
}

func suggestedBuyResizeFromCollateralShortfall(err error, currentRawShares float64, orderType polymarket.OrderType, price float64) (float64, bool) {
	if currentRawShares <= 0 {
		return 0, false
	}
	balanceRaw, matchedRaw, orderRaw, ok := parseCLOBCollateralShortfall(err)
	if !ok {
		return 0, false
	}
	availableRaw := balanceRaw - matchedRaw
	if availableRaw <= 0 {
		return 0, false
	}
	ratio := float64(availableRaw) / float64(orderRaw)
	if ratio <= 0 {
		return 0, false
	}
	// Keep a small safety margin so rounding does not re-trigger the same reject.
	resized := math.Floor(currentRawShares*ratio*0.995*100) / 100
	if resized <= 0 || resized >= currentRawShares {
		return 0, false
	}
	if orderType == polymarket.OrderTypeGTC || orderType == polymarket.OrderTypeGTD {
		if resized+0.0000001 < polymarket.MinOrderShares {
			return 0, false
		}
	}
	if orderType == polymarket.OrderTypeFAK || orderType == polymarket.OrderTypeFOK {
		if marketBuyOrderNotional(resized, price)+0.0000001 < polymarket.MinMarketOrderNotionalUSD {
			return 0, false
		}
	}
	return resized, true
}

func pairArbExactHedgeSizing(targetActualShares, hedgePrice float64, feeRateBps string) (rawShares, notional float64, ok bool) {
	rawShares = solveRawSharesForTargetActual(targetActualShares, hedgePrice, feeRateBps)
	if rawShares <= 0 {
		return 0, 0, false
	}
	notional = marketBuyOrderNotional(rawShares, hedgePrice)
	return rawShares, notional, notional+0.0000001 >= polymarket.MinMarketOrderNotionalUSD && rawShares >= polymarket.MinOrderShares
}

func solveRawSharesForTargetActual(targetActualShares, price float64, feeRateBps string) float64 {
	if targetActualShares <= 0 || price <= 0 || price >= 1.0 {
		return 0
	}
	rawShares := math.Ceil(targetActualShares*100) / 100
	bps, err := strconv.ParseFloat(feeRateBps, 64)
	if err == nil && bps > 0 {
		rate := bps / 10000.0
		pq := price * (1.0 - price)
		denom := 1.0 - rate*pq*pq
		if denom > 0 {
			rawShares = math.Ceil((targetActualShares/denom)*100) / 100
		}
	}
	for attempt := 0; attempt < 12; attempt++ {
		feeShares := polymarket.ComputeBuyFeeShares(rawShares, price, feeRateBps)
		actualShares := math.Floor((rawShares-feeShares)*100) / 100
		if actualShares+0.0001 >= targetActualShares {
			return rawShares
		}
		rawShares = math.Round((rawShares+0.01)*100) / 100
	}
	return rawShares
}

func isRetryablePairArbHedgeError(err error) bool {
	if err == nil {
		return false
	}
	if isNetworkError(err) {
		return true
	}
	message := strings.ToLower(err.Error())
	// GTC limit order timed out without fill Ã¢â‚¬â€ harmless to retry (order was cancelled).
	if strings.Contains(message, "gtc limit buy not filled") {
		return true
	}
	return strings.Contains(message, "pair arb: fak not filled") ||
		strings.Contains(message, "pair arb: lead buy not filled")
}

const (
	pairArbPostSubmitLookupTimeout = 200 * time.Millisecond
	pairArbLeadSubmitTimeout       = 4000 * time.Millisecond
	pairArbHedgeSubmitTimeout      = 2500 * time.Millisecond
	// GTC limit orders: single submit attempt only Ã¢â‚¬â€ retrying a GTC submit risks
	// double-placing if the first request reached the server but the response was lost.
	pairArbLeadMaxAttempts  = 1
	pairArbHedgeMaxAttempts = 1
	// pairArbLimitBuySlip is the default price offset above the signal price for GTC
	// limit buy orders.  Crossing the spread by 2 cents ensures near-instant fill
	// while avoiding the all-or-nothing failure mode of FAK orders.
	pairArbLimitBuySlip = 0.02
	// pairArbGTCPollDefaultTimeout is the fallback maximum time to wait for a LIVE GTC
	// lead buy to fill when PAIR_ARB_LEAD_BUY_TIMEOUT_SEC is not configured.
	pairArbGTCPollDefaultTimeout = 3 * time.Second
	pairArbGTCPollInterval       = 250 * time.Millisecond
)

func pairArbSubmitTimeoutForRequest(requestName string) time.Duration {
	if strings.Contains(requestName, "lead") {
		return pairArbLeadSubmitTimeout
	}
	if strings.Contains(requestName, "hedge") {
		return pairArbHedgeSubmitTimeout
	}
	return pairArbHedgeSubmitTimeout
}

func pairArbSubmitMaxAttemptsForRequest(requestName string) int {
	if strings.Contains(requestName, "lead") {
		return pairArbLeadMaxAttempts
	}
	if strings.Contains(requestName, "hedge") {
		return pairArbHedgeMaxAttempts
	}
	return pairArbHedgeMaxAttempts
}

type pairArbBuyOutcome string

const (
	pairArbBuyOutcomeFilled    pairArbBuyOutcome = "filled"
	pairArbBuyOutcomeNotFilled pairArbBuyOutcome = "not_filled"
	pairArbBuyOutcomeHardError pairArbBuyOutcome = "hard_error"
)

func isPairArbLeadBuyRequestName(requestName string) bool {
	req := strings.ToLower(strings.TrimSpace(requestName))
	return strings.Contains(req, "pair_arb_lead_buy")
}

func (t *Trader) asyncPairArbBuyReconcile(orderID, tokenID, requestName, requestID string) {
	if orderID == "" || tokenID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		var avgFill float64
		var grossShares float64
		if gr, err := t.getOrderTimed(ctx, requestName+"_async_lookup", orderID, zap.String("request_id", requestID)); err == nil && gr != nil {
			if prc, prcErr := strconv.ParseFloat(gr.Price, 64); prcErr == nil && prc > 0 && prc < 1.0 {
				avgFill = prc
			}
			if sm, smErr := strconv.ParseFloat(gr.SizeMatched, 64); smErr == nil && sm > 0 {
				grossShares = sm
			}
		}
		if avgFill <= 0 || grossShares <= 0 {
			if fillsAvg, fillsGross, err := t.getFillsTimed(ctx, requestName+"_async_fills", orderID, tokenID, zap.String("request_id", requestID)); err == nil {
				if avgFill <= 0 && fillsAvg > 0 {
					avgFill = fillsAvg
				}
				if grossShares <= 0 && fillsGross > 0 {
					grossShares = fillsGross
				}
			}
		}
		if avgFill > 0 || grossShares > 0 {
			t.logger.Debug("trader: pair arb buy async reconciliation",
				zap.String("request", requestName),
				zap.String("request_id", requestID),
				zap.String("order_id", orderID),
				zap.Float64("avg_fill", avgFill),
				zap.Float64("gross_shares", grossShares),
			)
		}
	}()
}

// asyncPairArbLeadCreditConfirm keeps lead-credit validation off the hot entry path.
// If credit appears after the synchronous path returned not-filled, immediately
// run pending-order reconciliation so the lead can be recovered and hedged.
func (t *Trader) asyncPairArbLeadCreditConfirm(orderID, tokenID, requestName, requestID string, minExpected float64) {
	if t.cfg.PaperTrade || orderID == "" || tokenID == "" {
		return
	}
	if minExpected < pairArbShareDust {
		minExpected = pairArbShareDust
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), pairArbLeadCreditGrace)
		defer cancel()

		credited, bal, err := t.pairArbBuyCreditConfirmedWithRetry(ctx, tokenID, minExpected, 20, 500*time.Millisecond)
		if err != nil {
			t.logger.Warn("trader: async lead credit probe errored; keeping pending order for reconciliation",
				zap.String("request", requestName),
				zap.String("request_id", requestID),
				zap.String("order_id", orderID),
				zap.Error(err),
			)
			t.asyncPairArbLeadSettleWatch(orderID, tokenID, requestName, requestID)
			return
		}
		if credited {
			t.logger.Info("trader: async lead credit confirmed",
				zap.String("request", requestName),
				zap.String("request_id", requestID),
				zap.String("order_id", orderID),
				zap.Float64("wallet_balance", bal),
				zap.Float64("min_expected", minExpected),
			)
			if t.pendingPairArb != nil && t.pendingPairArb.OrderID == orderID {
				reconcileCtx, reconcileCancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer reconcileCancel()
				if recErr := t.reconcilePendingPairArbOrder(reconcileCtx); recErr != nil {
					t.logger.Warn("trader: async lead credit reconcile failed",
						zap.String("request", requestName),
						zap.String("request_id", requestID),
						zap.String("order_id", orderID),
						zap.Error(recErr),
					)
				}
			}
			return
		}
		t.logger.Warn("trader: async lead credit not confirmed within grace; pending reconciliation remains active",
			zap.String("request", requestName),
			zap.String("request_id", requestID),
			zap.String("order_id", orderID),
			zap.Float64("wallet_balance", bal),
			zap.Float64("min_expected", minExpected),
		)
		t.asyncPairArbLeadSettleWatch(orderID, tokenID, requestName, requestID)
	}()
}

// pairArbBookProvider is intentionally optional. Kalshi implements it; legacy
// unit-test mocks do not need to. It lets paper mode simulate the actual CLOB
// depth rather than assuming every signal price fills.
type pairArbBookProvider interface {
	GetOrderBook(context.Context, string) (*polymarket.OrderBook, error)
}

// kalshiTakerFeeUSD implements the standard Kalshi event-contract taker fee:
// ceil-to-cent(0.07 * contracts * p * (1-p)). The rate is configurable for
// paper experiments because some series can have a special fee schedule.
func kalshiTakerFeeUSD(contracts, price float64) float64 {
	if contracts <= 0 || price <= 0 || price >= 1 {
		return 0
	}
	rate := 0.07
	if raw := strings.TrimSpace(os.Getenv("KALSHI_TAKER_FEE_RATE")); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && v >= 0 {
			rate = v
		}
	}
	return math.Ceil((rate*contracts*price*(1.0-price))*100.0-1e-9) / 100.0
}

func pairBookLevels(levels []polymarket.PriceSize, ascending bool) [][2]float64 {
	out := make([][2]float64, 0, len(levels))
	for _, l := range levels {
		p, e1 := strconv.ParseFloat(strings.TrimSpace(l.Price), 64)
		q, e2 := strconv.ParseFloat(strings.TrimSpace(l.Size), 64)
		if e1 == nil && e2 == nil && p > 0 && p < 1 && q > 0 {
			out = append(out, [2]float64{p, q})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if ascending {
			return out[i][0] < out[j][0]
		}
		return out[i][0] > out[j][0]
	})
	return out
}

// pairPaperBuyQuote sweeps real executable asks up to the order's limit price.
// totalCost includes the conservative Kalshi taker fee.
func (t *Trader) pairPaperBuyQuote(ctx context.Context, tokenID string, qty, limitPrice, fallbackPrice float64) (vwap, totalCost, fee float64, ok bool) {
	if qty <= 0 || limitPrice <= 0 {
		return 0, 0, 0, false
	}
	if bp, yes := t.orders.(pairArbBookProvider); yes && bp != nil {
		book, err := bp.GetOrderBook(ctx, tokenID)
		if err == nil && book != nil {
			levels := pairBookLevels(book.Asks, true)
			left, raw := qty, 0.0
			for _, l := range levels {
				if left <= 1e-9 {
					break
				}
				if l[0] > limitPrice+1e-9 {
					break
				}
				take := math.Min(left, l[1])
				raw += take * l[0]
				left -= take
			}
			if left > 1e-9 {
				return 0, 0, 0, false
			}
			vwap = raw / qty
			fee = kalshiTakerFeeUSD(qty, vwap)
			return vwap, raw + fee, fee, true
		}
	}
	// Unit tests and non-Kalshi mocks use the historical signal-price fallback.
	if fallbackPrice <= 0 || fallbackPrice > limitPrice+1e-9 {
		return 0, 0, 0, false
	}
	vwap = fallbackPrice
	fee = kalshiTakerFeeUSD(qty, vwap)
	return vwap, qty*vwap + fee, fee, true
}

// pairPaperSellQuote sweeps executable bids. Proceeds are net of Kalshi fee.
func (t *Trader) pairPaperSellQuote(ctx context.Context, tokenID string, qty, fallbackPrice float64) (vwap, proceeds, fee float64, ok bool) {
	if qty <= 0 {
		return 0, 0, 0, false
	}
	if bp, yes := t.orders.(pairArbBookProvider); yes && bp != nil {
		book, err := bp.GetOrderBook(ctx, tokenID)
		if err == nil && book != nil {
			levels := pairBookLevels(book.Bids, false)
			left, raw := qty, 0.0
			for _, l := range levels {
				if left <= 1e-9 {
					break
				}
				take := math.Min(left, l[1])
				raw += take * l[0]
				left -= take
			}
			if left > 1e-9 {
				return 0, 0, 0, false
			}
			vwap = raw / qty
			fee = kalshiTakerFeeUSD(qty, vwap)
			return vwap, raw - fee, fee, true
		}
	}
	if fallbackPrice <= 0 {
		return 0, 0, 0, false
	}
	vwap = fallbackPrice
	fee = kalshiTakerFeeUSD(qty, vwap)
	return vwap, qty*vwap - fee, fee, true
}

// pairKalshiMaxHedgePrice solves the original PolyArb lock equation after
// Kalshi cash fees. anchorEffective is the already-paid lead cost per contract,
// including the lead fee folded into USDSpent.
func pairKalshiMaxHedgePrice(anchorEffective, qty, targetPerShare float64) float64 {
	if anchorEffective <= 0 || qty <= 0 {
		return 0
	}
	budgetPerShare := 1.0 - targetPerShare - anchorEffective
	if budgetPerShare <= 0 {
		return 0
	}
	best := 0.0
	// The original strategy is cent-based; test all valid cent hedge limits.
	for c := 1; c <= 99; c++ {
		p := float64(c) / 100.0
		eff := p + kalshiTakerFeeUSD(qty, p)/qty
		if eff <= budgetPerShare+1e-9 {
			best = p
		}
	}
	return best
}

func pairKalshiSharesForBudget(budget, price float64) float64 {
	if budget <= 0 || price <= 0 {
		return 0
	}
	// Fractional contracts are supported; search downward in 0.01-contract steps.
	q := math.Floor((budget/price)*100) / 100
	for q > 0 {
		if q*price+kalshiTakerFeeUSD(q, price) <= budget+1e-9 {
			return q
		}
		q = math.Round((q-0.01)*100) / 100
	}
	return 0
}

// executePairLimitBuy submits a buy and returns an explicit execution outcome:
// filled, not_filled, or hard_error.
func (t *Trader) executePairLimitBuy(ctx context.Context, requestName, tokenID string, signalPrice, limitPrice, rawShares float64) (outcome pairArbBuyOutcome, orderID string, fillPrice, actualShares, feeShares, usdSpent float64, err error) {
	entryStart := time.Now()
	requestID := fmt.Sprintf("%s-%d", requestName, time.Now().UnixNano())
	if tokenID == "" {
		return pairArbBuyOutcomeHardError, "", 0, 0, 0, 0, fmt.Errorf("pair arb: missing token id")
	}
	if signalPrice <= 0 || limitPrice <= 0 || rawShares <= 0 {
		return pairArbBuyOutcomeHardError, "", 0, 0, 0, 0, fmt.Errorf("pair arb: invalid buy inputs")
	}
	usdSpent = marketBuyOrderNotional(rawShares, limitPrice)
	if usdSpent+0.0000001 < polymarket.MinMarketOrderNotionalUSD {
		return pairArbBuyOutcomeHardError, "", 0, 0, 0, 0, fmt.Errorf("pair arb: limit buy notional $%.4f below $%.2f minimum", usdSpent, polymarket.MinMarketOrderNotionalUSD)
	}
	fillPrice = math.Round(signalPrice*100) / 100
	feeShares = polymarket.ComputeBuyFeeShares(rawShares, fillPrice, t.feeRateBps)
	actualShares = math.Floor((rawShares-feeShares)*100) / 100
	if actualShares <= 0 {
		return pairArbBuyOutcomeHardError, "", 0, 0, 0, 0, fmt.Errorf("pair arb: fee eats all shares")
	}
	preflightMS := time.Since(entryStart).Milliseconds()

	priceStr := fmt.Sprintf("%.2f", limitPrice)
	if t.cfg.PaperTrade {
		// Kalshi paper mode: sweep the actual executable ask depth and charge the
		// venue fee in CASH. Unlike Polymarket, fees do not reduce contract count.
		vwap, paperCost, paperFee, filled := t.pairPaperBuyQuote(ctx, tokenID, rawShares, limitPrice, fillPrice)
		if !filled {
			return pairArbBuyOutcomeNotFilled, "", 0, 0, 0, 0, nil
		}
		if paperCost > t.paperBalance+1e-9 {
			return pairArbBuyOutcomeHardError, "", 0, 0, 0, 0, fmt.Errorf("pair arb: insufficient paper balance ($%.2f) for Kalshi trade ($%.2f incl fee)", t.paperBalance, paperCost)
		}
		fillPrice = vwap
		feeShares = 0
		actualShares = rawShares
		t.paperBalance -= paperCost
		t.logger.Info("pair arb: Kalshi paper fill", zap.String("request", requestName), zap.Float64("vwap", vwap), zap.Float64("contracts", rawShares), zap.Float64("fee_usd", paperFee), zap.Float64("total_cost_usd", paperCost))
		if t.metrics != nil {
			entryToPositionMs := time.Since(entryStart).Milliseconds()
			t.metrics.Record("order_place", entryToPositionMs)
			t.metrics.Record("entry_preflight", preflightMS)
			t.metrics.Record("entry_to_position", entryToPositionMs)
		}
		return pairArbBuyOutcomeFilled, fmt.Sprintf("PAPER-%s-%d", strings.ToUpper(requestName), time.Now().UnixMilli()), fillPrice, actualShares, feeShares, paperCost, nil
	}

	submitTimeout := pairArbSubmitTimeoutForRequest(requestName)
	maxAttempts := pairArbSubmitMaxAttemptsForRequest(requestName)
	orderType := polymarket.OrderTypeGTC
	if strings.Contains(requestName, "lead") || strings.Contains(requestName, "hedge_buy") {
		// Respect configured pair-arb entry execution mode (FAK/FOK/GTC).
		orderType = t.pairArbLeadOrderType()
	}
	submitBuy := func(shares float64) (*polymarket.OrderResponse, error) {
		return t.placeOrderTimed(ctx, requestName, &polymarket.NewOrderRequest{
			OrderType:      orderType,
			TokenID:        tokenID,
			Side:           polymarket.SideBuy,
			Price:          priceStr,
			Size:           fmt.Sprintf("%.2f", shares),
			Nonce:          polymarket.MakeNonce(),
			FeeRateBps:     t.feeRateBps,
			AttemptTimeout: submitTimeout,
			MaxAttempts:    maxAttempts,
		}, zap.String("request_id", requestID), zap.Int64("preflight_ms", preflightMS), zap.Int64("submit_timeout_ms", submitTimeout.Milliseconds()), zap.Int("submit_max_attempts", maxAttempts))
	}

	resp, placeErr := submitBuy(rawShares)
	if placeErr != nil {
		allowAdaptiveResize := !strings.Contains(strings.ToLower(requestName), "hedge")
		if allowAdaptiveResize {
			if resizedShares, resizedOK := suggestedBuyResizeFromCollateralShortfall(placeErr, rawShares, orderType, limitPrice); resizedOK {
				t.logger.Warn("trader: pair arb buy collateral shortfall; retrying with reduced size",
					zap.String("request", requestName),
					zap.String("token_id", tokenID),
					zap.Float64("price", limitPrice),
					zap.Float64("raw_shares_original", rawShares),
					zap.Float64("raw_shares_retry", resizedShares),
				)
				rawShares = resizedShares
				usdSpent = marketBuyOrderNotional(rawShares, limitPrice)
				feeShares = polymarket.ComputeBuyFeeShares(rawShares, fillPrice, t.feeRateBps)
				actualShares = math.Floor((rawShares-feeShares)*100) / 100
				if actualShares <= 0 {
					return pairArbBuyOutcomeHardError, "", 0, 0, 0, 0, fmt.Errorf("pair arb: collateral-resized buy has non-positive shares")
				}
				resp, placeErr = submitBuy(rawShares)
			}
		}
	}
	if placeErr != nil {
		return pairArbBuyOutcomeHardError, "", 0, 0, 0, 0, placeErr
	}
	if !resp.Success {
		return pairArbBuyOutcomeHardError, "", 0, 0, 0, 0, fmt.Errorf("pair arb: order rejected: %s", resp.ErrorMsg)
	}
	orderID = resp.OrderID

	var correctedFill float64
	var grossFillShares float64
	fillSource := "submit_response"
	keepPendingForReconcile := false
	postSubmitLookupMS := int64(0)

	submitStatus := strings.ToLower(strings.TrimSpace(resp.Status))
	switch {
	case submitStatus == "matched" || submitStatus == "duplicated":
		// Submit response is definitive: immediate execution happened.
		t.setPendingPairArbOrder(orderID, tokenID, requestName)
		// Order crossed the spread and filled immediately.
		if resp.TakingAmount != "" && resp.MakingAmount != "" {
			takingRaw, okT := new(big.Int).SetString(resp.TakingAmount, 10)
			makingRaw, okM := new(big.Int).SetString(resp.MakingAmount, 10)
			if okT && okM && takingRaw.Sign() > 0 && makingRaw.Sign() > 0 {
				actualRat := new(big.Rat).SetFrac(takingRaw, big.NewInt(1_000_000))
				if actualFillShares, _ := actualRat.Float64(); actualFillShares > 0 {
					grossFillShares = actualFillShares
				}
				priceRat := new(big.Rat).SetFrac(makingRaw, takingRaw)
				if actualFillPrice, _ := priceRat.Float64(); actualFillPrice > 0 && actualFillPrice < 1.0 {
					correctedFill = actualFillPrice
				}
			}
		}
		if correctedFill <= 0 || grossFillShares <= 0 {
			// API is eventually consistent Ã¢â‚¬â€ hit a stale node.  Short parallel lookup.
			lookupStart := time.Now()
			lookupCtx, cancelLookup := context.WithTimeout(ctx, pairArbPostSubmitLookupTimeout)

			type lookupResult struct {
				source string
				fill   float64
				gross  float64
				err    error
			}
			lookupResults := make(chan lookupResult, 2)

			go func() {
				gr, grErr := t.getOrderTimed(lookupCtx, requestName+"_lookup", resp.OrderID, zap.String("request_id", requestID))
				res := lookupResult{source: "get_order", err: grErr}
				if grErr == nil && gr != nil {
					if prc, prcErr := strconv.ParseFloat(gr.Price, 64); prcErr == nil && prc > 0 && prc < 1.0 {
						res.fill = prc
					}
					if sm, smErr := strconv.ParseFloat(gr.SizeMatched, 64); smErr == nil && sm > 0 {
						res.gross = sm
					}
				}
				lookupResults <- res
			}()

			go func() {
				avg, gross, trErr := t.getFillsTimed(lookupCtx, requestName+"_fills", resp.OrderID, tokenID, zap.String("request_id", requestID))
				res := lookupResult{source: "get_fills", err: trErr}
				if trErr == nil {
					if avg > 0 {
						res.fill = avg
					}
					if gross > 0 {
						res.gross = gross
					}
				}
				lookupResults <- res
			}()

			collected := 0
			for collected < 2 {
				select {
				case res := <-lookupResults:
					collected++
					if res.err != nil {
						continue
					}
					if correctedFill <= 0 && res.fill > 0 {
						correctedFill = res.fill
						fillSource = res.source
					}
					if grossFillShares <= 0 && res.gross > 0 {
						grossFillShares = res.gross
						fillSource = res.source
					}
					if correctedFill > 0 && grossFillShares > 0 {
						collected = 2
					}
				case <-lookupCtx.Done():
					collected = 2
				}
			}
			cancelLookup()
			postSubmitLookupMS = time.Since(lookupStart).Milliseconds()
			if correctedFill <= 0 || grossFillShares <= 0 {
				// KALSHI SAFETY: MATCHED status without authoritative price AND
				// quantity is not sufficient to record inventory.
				t.logger.Warn("trader: Kalshi matched order missing authoritative fill data; deferring to reconciliation",
					zap.String("request", requestName),
					zap.String("order_id", orderID),
					zap.Float64("resolved_price", correctedFill),
					zap.Float64("resolved_shares", grossFillShares),
					zap.Int64("post_submit_lookup_ms", postSubmitLookupMS),
				)
				t.setPendingPairArbOrder(orderID, tokenID, requestName)
				t.asyncPairArbBuyReconcile(orderID, tokenID, requestName, requestID)
				return pairArbBuyOutcomeHardError, orderID, 0, 0, 0, 0,
					fmt.Errorf("pair arb: Kalshi order %s matched but authoritative fill data is not available yet", orderID)
			}
		}

	case submitStatus == "live" || submitStatus == "delayed":
		if submitStatus == "live" && orderType != polymarket.OrderTypeGTC {
			// Immediate order types (FAK/FOK) should not return LIVE, but API races can
			// produce transient LIVE responses. Poll/reconcile instead of treating this
			// as deterministic no-fill so we never lose a late/partial fill.
			t.logger.Warn("trader: pair arb immediate order returned LIVE; entering reconcile poll",
				zap.String("request", requestName),
				zap.String("order_id", orderID),
				zap.String("order_type", string(orderType)),
			)
		}
		// GTC LIVE (resting in book) or DELAYED (processing deferred): poll until matched or timeout.
		t.setPendingPairArbOrder(orderID, tokenID, requestName)
		pollTimeout := pairArbGTCPollDefaultTimeout
		if s := t.cfg.PairArbLeadBuyTimeoutSec; s > 0 {
			pollTimeout = time.Duration(s) * time.Second
		}
		if strings.Contains(requestName, "urgent_rescue") {
			// Hedge leg already matched and we are trying to rescue the opposite lead.
			// Keep this window very short to avoid carrying one-sided exposure.
			pollTimeout = 1500 * time.Millisecond
		}
		pollStart := time.Now()
		pollDeadline := pollStart.Add(pollTimeout)
		hedgeAbortTriggered := false
		hedgeAbortOrderID := ""
		gapGuardCancelled := false
		lastHedgeGuardProbe := time.Time{}
		for time.Now().Before(pollDeadline) {
			select {
			case <-ctx.Done():
				_ = t.cancelOrderTimed(ctx, requestName+"_cancel_ctx", orderID)
				t.clearPendingPairArbOrder()
				return pairArbBuyOutcomeHardError, "", 0, 0, 0, 0, ctx.Err()
			case <-time.After(pairArbGTCPollInterval):
			}
			gr, grErr := t.getOrderTimed(ctx, requestName+"_gtc_poll", orderID)
			if grErr != nil || gr == nil {
				// Matched orders may disappear from order lookup quickly; reconcile
				// with trades to avoid false timeout/cancel on already-filled buys.
				if fillsAvg, fillsGross, fillsErr := t.getFillsTimed(ctx, requestName+"_gtc_poll_fills", orderID, tokenID); fillsErr == nil && fillsGross > 0 {
					grossFillShares = fillsGross
					if fillsAvg > 0 {
						correctedFill = fillsAvg
					}
					fillSource = "gtc_poll_fills"
					break
				}
				continue
			}
			pollStatus := strings.ToLower(strings.TrimSpace(gr.Status))
			if pollStatus == "matched" || pollStatus == "filled" {
				if prc, prcErr := strconv.ParseFloat(gr.Price, 64); prcErr == nil && prc > 0 && prc < 1.0 {
					correctedFill = prc
					fillSource = "gtc_poll"
				}
				if sm, smErr := strconv.ParseFloat(gr.SizeMatched, 64); smErr == nil && sm > 0 {
					grossFillShares = sm
					fillSource = "gtc_poll"
				}
				// The order-status SizeMatched field reflects CLOB-matched size, not actual
				// conditional tokens delivered (fees are deducted downstream by the CTF contract).
				// Cross-check against the fills endpoint which reports actual token delivery.
				// This is the same verification the MATCHED-on-submit path performs.
				if fillsAvg, fillsGross, fillsErr := t.getFillsTimed(ctx, requestName+"_gtc_poll_fills", orderID, tokenID); fillsErr == nil && fillsGross > 0 {
					grossFillShares = fillsGross
					if fillsAvg > 0 {
						correctedFill = fillsAvg
					}
					fillSource = "gtc_poll_fills"
				}
				break
			}
			if pollStatus == "unmatched" || pollStatus == "canceled" || pollStatus == "cancelled" || pollStatus == "expired" {
				if fillsAvg, fillsGross, fillsErr := t.getFillsTimed(ctx, requestName+"_gtc_poll_fills", orderID, tokenID); fillsErr == nil && fillsGross > 0 {
					grossFillShares = fillsGross
					if fillsAvg > 0 {
						correctedFill = fillsAvg
					}
					fillSource = "gtc_poll_fills"
					break
				}
				logTimedActivityWarn(fmt.Sprintf("BUY %s %s not filled: order became %s order=%s", orderType, activityRequestLabel(requestName), strings.ToUpper(pollStatus), orderID))
				t.clearPendingPairArbOrder()
				return pairArbBuyOutcomeNotFilled, orderID, 0, 0, 0, 0, nil
			}
			if !hedgeAbortTriggered && !gapGuardCancelled && pollStatus == "live" && isPairArbLeadBuyRequestName(requestName) {
				now := time.Now()
				if lastHedgeGuardProbe.IsZero() || now.Sub(lastHedgeGuardProbe) >= 750*time.Millisecond {
					lastHedgeGuardProbe = now
					// Gap guard: cancel lead if the BTC-vs-open gap has reverted below entry threshold.
					if guardFn := t.pairArbLeadGapGuardFn; guardFn != nil {
						if shouldCancel, guardReason := guardFn(); shouldCancel {
							gapGuardCancelled = true
							t.logger.Warn("trader: pair arb lead GTC cancelled: signal gap closed",
								zap.String("request", requestName),
								zap.String("reason", guardReason),
								zap.String("order_id", orderID),
							)
							_ = t.cancelOrderTimed(ctx, requestName+"_cancel_gap_closed", orderID)
							break
						}
					}
					hedgeFilled, hedgeOrderID := t.shouldAbortLeadWaitForFilledPreHedge(ctx, requestName)
					if hedgeFilled {
						hedgeAbortTriggered = true
						hedgeAbortOrderID = hedgeOrderID
						t.logger.Error("trader: pair arb lead wait aborted: pre-placed hedge filled while lead was still LIVE",
							zap.String("request", requestName),
							zap.String("lead_order_id", orderID),
							zap.String("hedge_order_id", hedgeOrderID),
						)
						_ = t.cancelOrderTimed(ctx, requestName+"_cancel_hedge_filled", orderID)
						break
					}
				}
			}
		}
		postSubmitLookupMS = time.Since(pollStart).Milliseconds()
		if correctedFill <= 0 && grossFillShares <= 0 {
			_ = t.cancelOrderTimed(ctx, requestName+"_cancel_timeout", orderID)
			// Cancel-vs-match race hardening: the order can be MATCHED while the cancel
			// request is in-flight or while status propagation lags. Reconcile once more
			// before declaring timeout so we can continue hedge flow immediately.
			if gr, grErr := t.getOrderTimed(ctx, requestName+"_post_cancel_lookup", orderID); grErr == nil && gr != nil {
				pcStatus := strings.ToLower(strings.TrimSpace(gr.Status))
				if pcStatus == "matched" || pcStatus == "filled" {
					if prc, prcErr := strconv.ParseFloat(gr.Price, 64); prcErr == nil && prc > 0 && prc < 1.0 {
						correctedFill = prc
						fillSource = "post_cancel_lookup"
					}
					if sm, smErr := strconv.ParseFloat(gr.SizeMatched, 64); smErr == nil && sm > pairArbShareDust {
						grossFillShares = sm
						fillSource = "post_cancel_lookup"
					}
					if fillsAvg, fillsGross, fillsErr := t.getFillsTimed(ctx, requestName+"_post_cancel_fills", orderID, tokenID); fillsErr == nil && fillsGross > 0 {
						grossFillShares = fillsGross
						if fillsAvg > 0 {
							correctedFill = fillsAvg
						}
						fillSource = "post_cancel_fills"
					}
				}
				if correctedFill <= 0 && grossFillShares <= 0 {
					// Partial-fill hardening: canceled GTC orders can still report matched
					// shares even when /data/trades lags. Treat non-zero size_matched as
					// authoritative so we do not place a second lead buy on top.
					if sm, smErr := strconv.ParseFloat(gr.SizeMatched, 64); smErr == nil && sm > pairArbShareDust {
						grossFillShares = sm
						if prc, prcErr := strconv.ParseFloat(gr.Price, 64); prcErr == nil && prc > 0 && prc < 1.0 {
							correctedFill = prc
						}
						fillSource = "post_cancel_size_matched"
						t.logger.Warn("trader: pair arb buy recovered partial fill from post-cancel size_matched",
							zap.String("request", requestName),
							zap.String("order_id", orderID),
							zap.String("status", gr.Status),
							zap.String("size_matched", gr.SizeMatched),
						)
					}
				}
			}
			if correctedFill <= 0 && grossFillShares <= 0 {
				if fillsAvg, fillsGross, fillsErr := t.getFillsTimed(ctx, requestName+"_post_cancel_fills", orderID, tokenID); fillsErr == nil && fillsGross > 0 {
					grossFillShares = fillsGross
					if fillsAvg > 0 {
						correctedFill = fillsAvg
					}
					fillSource = "post_cancel_fills"
				}
			}
			if correctedFill <= 0 && grossFillShares <= 0 {
				if hedgeAbortTriggered {
					abortReason := "pre-placed hedge filled first"
					if hedgeAbortOrderID != "" {
						abortReason = fmt.Sprintf("pre-placed hedge %s filled first", hedgeAbortOrderID)
					}
					logTimedActivityWarn(fmt.Sprintf("BUY %s %s not filled: aborted lead wait because %s order=%s", orderType, activityRequestLabel(requestName), abortReason, orderID))
				} else if gapGuardCancelled {
					logTimedActivityWarn(fmt.Sprintf("BUY %s %s not filled: cancelled because gap closed while order was LIVE order=%s", orderType, activityRequestLabel(requestName), orderID))
				} else {
					logTimedActivityWarn(fmt.Sprintf("BUY %s %s not filled: timed out after %d ms and no post-cancel fill data order=%s", orderType, activityRequestLabel(requestName), postSubmitLookupMS, orderID))
				}
				t.clearPendingPairArbOrder()
				return pairArbBuyOutcomeNotFilled, orderID, 0, 0, 0, 0, nil
			}
		}

	case submitStatus == "unmatched":
		// FAK/FOK expected not-filled path.
		logTimedActivityWarn(fmt.Sprintf("BUY %s %s not filled: submit status UNMATCHED order=%s", orderType, activityRequestLabel(requestName), orderID))
		return pairArbBuyOutcomeNotFilled, orderID, 0, 0, 0, 0, nil

	default:
		// Any non-matched status on immediate orders is a deterministic no-fill.
		logTimedActivityWarn(fmt.Sprintf("BUY %s %s not filled: submit status %s order=%s", orderType, activityRequestLabel(requestName), strings.ToUpper(submitStatus), orderID))
		return pairArbBuyOutcomeNotFilled, orderID, 0, 0, 0, 0, nil
	}
	if correctedFill > 0 {
		fillPrice = math.Round(correctedFill*100) / 100
	}
	if grossFillShares > 0 {
		feeShares = polymarket.ComputeBuyFeeShares(grossFillShares, fillPrice, t.feeRateBps)
		actualShares = math.Floor((grossFillShares-feeShares)*100) / 100
	}
	if actualShares <= 0 {
		// KALSHI SAFETY: never manufacture filled inventory from requested size.
		// Require authoritative SizeMatched/fills evidence.
		t.logger.Warn("trader: Kalshi buy has no authoritative filled quantity",
			zap.String("request", requestName),
			zap.String("order_id", orderID),
			zap.Float64("gross_fill_shares", grossFillShares),
		)
		return pairArbBuyOutcomeHardError, orderID, 0, 0, 0, 0,
			fmt.Errorf("pair arb: Kalshi order %s has no authoritative filled quantity yet", orderID)
	}

	// Recalculate usdSpent using the actual fill price (GTC orders fill at market price,
	// which may be lower than the limit ceiling used for the preflight estimate).
	// This ensures the journal cost basis and PnL are accurate.
	effectiveShares := grossFillShares
	if effectiveShares <= 0 {
		effectiveShares = rawShares
	}
	usdSpent = marketBuyOrderNotional(effectiveShares, fillPrice)
	if !t.cfg.PaperTrade {
		// Doc-backed execution model: insert status/trade rows can appear before the
		// settled token balance is visible, so ownership is only finalized after wallet credit.
		minExpected := actualShares - 0.02
		if minExpected < pairArbShareDust {
			minExpected = pairArbShareDust
		}
		// Fast path: fills API already confirmed the trade (non-zero avg + gross).
		// Polymarket's fills endpoint is authoritative — if it returned data, the order
		// settled on-chain. Use a short check so we can place the hedge immediately.
		// Tiers:
		//   submit_response — FOK/FAK fill confirmed directly by the exchange response;
		//     1 immediate probe then proceed regardless (wallet_credit_pending handles lag).
		//   non-preflight  — fills API confirmed but via a secondary lookup; 3 probes ≤1.5s.
		//   preflight_fallback — fill data uncertain; keep full 20-probe / 10s wait.
		creditAttempts := 20
		creditInterval := 500 * time.Millisecond
		if fillSource == "submit_response" && grossFillShares > 0 && fillPrice > 0 {
			creditAttempts = 1
		} else if fillSource != "preflight_fallback" && grossFillShares > 0 && fillPrice > 0 {
			creditAttempts = 3
			creditInterval = 500 * time.Millisecond
		}
		credited, bal, cErr := t.pairArbBuyCreditConfirmedWithRetry(ctx, tokenID, minExpected, creditAttempts, creditInterval)
		if cErr != nil {
			// Keep pendingPairArb set so entry remains blocked until reconcilePendingPairArbOrder
			// can verify whether the MATCHED buy eventually credited to the wallet.
			t.logger.Warn("trader: pair arb buy wallet-credit probe error; keeping pending order for reconciliation",
				zap.String("order_id", orderID),
				zap.Error(cErr),
			)
			if requestName == "pair_arb_lead_buy" {
				t.asyncPairArbLeadCreditConfirm(orderID, tokenID, requestName, requestID, minExpected)
			}
			return pairArbBuyOutcomeHardError, "", 0, 0, 0, 0, cErr
		}
		if !credited {
			// Order is matched and money is spent. If we have any shares in the wallet
			// (just below the expected threshold due to API lag or rounding), use the
			// actual balance as shares rather than erroring and leaving pendingPairArb set.
			if bal >= pairArbShareDust {
				t.logger.Warn("trader: pair arb buy wallet credit short; using actual wallet balance as shares",
					zap.String("order_id", orderID),
					zap.Float64("expected", minExpected),
					zap.Float64("actual_balance", bal),
				)
				effectiveGrossShares := grossFillShares
				if effectiveGrossShares <= pairArbShareDust {
					effectiveGrossShares = rawShares
				}
				if fillPrice <= 0 {
					fillPrice = math.Round(limitPrice*100) / 100
				}
				feeShares = polymarket.ComputeBuyFeeShares(effectiveGrossShares, fillPrice, t.feeRateBps)
				actualShares = bal
				usdSpent = marketBuyOrderNotional(effectiveGrossShares, fillPrice)
				if usdSpent <= 0 {
					usdSpent = marketBuyOrderNotional(actualShares, fillPrice)
				}
			} else {
				// Credit can lag the MATCHED status by a few seconds. Continue using
				// estimated shares so hedge logic runs immediately, while leaving pending
				// set so reconciliation still guards future lead entries.
				t.logger.Warn("trader: pair arb buy matched but wallet has no credited shares yet; proceeding with estimated shares and keeping pending order",
					zap.String("order_id", orderID),
					zap.Float64("wallet_balance", bal),
					zap.Float64("min_expected", minExpected),
				)
				if requestName == "pair_arb_lead_buy" {
					// Never open/hedge a lead until wallet credit is visible. Keep the
					// pending order for reconcilePendingPairArbOrder() recovery instead.
					logTimedActivityWarn(fmt.Sprintf("BUY %s %s delayed: MATCHED but wallet credit pending (balance=%.2f expected>=%.2f) order=%s", orderType, activityRequestLabel(requestName), bal, minExpected, orderID))
					t.asyncPairArbLeadCreditConfirm(orderID, tokenID, requestName, requestID, minExpected)
					keepPendingForReconcile = true
					return pairArbBuyOutcomeNotFilled, orderID, 0, 0, 0, 0, nil
				}
				effectiveGrossShares := grossFillShares
				if effectiveGrossShares <= pairArbShareDust {
					effectiveGrossShares = rawShares
				}
				if fillPrice <= 0 {
					fillPrice = math.Round(limitPrice*100) / 100
				}
				feeShares = polymarket.ComputeBuyFeeShares(effectiveGrossShares, fillPrice, t.feeRateBps)
				actualShares = math.Floor((effectiveGrossShares-feeShares)*100) / 100
				if actualShares <= pairArbShareDust {
					actualShares = math.Floor((rawShares-polymarket.ComputeBuyFeeShares(rawShares, fillPrice, t.feeRateBps))*100) / 100
				}
				if actualShares <= pairArbShareDust {
					actualShares = pairArbShareDust
				}
				usdSpent = marketBuyOrderNotional(effectiveGrossShares, fillPrice)
				if usdSpent <= 0 {
					usdSpent = marketBuyOrderNotional(rawShares, fillPrice)
				}
				fillSource = "wallet_credit_pending"
				keepPendingForReconcile = true
			}
		}
	}
	if !keepPendingForReconcile {
		t.clearPendingPairArbOrder()
	}
	entryToPositionMs := time.Since(entryStart).Milliseconds()
	t.logger.Info("trader: pair arb buy latency",
		zap.String("request", requestName),
		zap.String("request_id", requestID),
		zap.String("order_id", orderID),
		zap.Int64("preflight_ms", preflightMS),
		zap.Int64("sign_ms", resp.SignMs),
		zap.Int64("http_ms", resp.HTTPMs),
		zap.Int64("post_submit_lookup_ms", postSubmitLookupMS),
		zap.Int64("entry_to_position_ms", entryToPositionMs),
		zap.String("fill_source", fillSource),
	)
	if t.metrics != nil {
		t.metrics.Record("entry_preflight", preflightMS)
		t.metrics.Record("entry_to_position", entryToPositionMs)
	}
	return pairArbBuyOutcomeFilled, orderID, fillPrice, actualShares, feeShares, usdSpent, nil
}

func (t *Trader) shouldAbortLeadWaitForFilledPreHedge(ctx context.Context, requestName string) (bool, string) {
	if t.cfg.PaperTrade || !isPairArbLeadBuyRequestName(requestName) {
		return false, ""
	}
	pending := t.pendingHedgePrePlace
	if pending == nil {
		return false, ""
	}
	hedgeOrderID := strings.TrimSpace(pending.OrderID)
	hedgeTokenID := strings.TrimSpace(pending.TokenID)
	if hedgeOrderID == "" || hedgeTokenID == "" {
		return false, hedgeOrderID
	}
	minExpected := pending.RequestedSize - pairArbWalletReconcileDelta
	if minExpected < pairArbShareDust {
		minExpected = pairArbShareDust
	}
	if gr, grErr := t.getOrderTimed(ctx, requestName+"_hedge_guard", hedgeOrderID); grErr == nil && gr != nil {
		status := strings.ToLower(strings.TrimSpace(gr.Status))
		matched := parseOrderSize(gr.SizeMatched)
		remaining := parseOrderSize(gr.SizeRemaining)
		if pending.RequestedSize > 0 {
			if matched >= minExpected {
				return true, hedgeOrderID
			}
			if matched > pairArbShareDust && remaining <= pairArbShareDust {
				return true, hedgeOrderID
			}
			if (status == "matched" || status == "filled") && matched > pairArbShareDust {
				t.logger.Warn("trader: pair arb hedge guard saw partial pre-hedge fill; continuing lead wait",
					zap.String("request", requestName),
					zap.String("hedge_order_id", hedgeOrderID),
					zap.Float64("matched_shares", matched),
					zap.Float64("remaining_shares", remaining),
					zap.Float64("requested_shares", pending.RequestedSize),
				)
			}
			return false, hedgeOrderID
		}
		if status == "matched" || status == "filled" {
			return true, hedgeOrderID
		}
		if matched > pairArbShareDust {
			return true, hedgeOrderID
		}
		return false, hedgeOrderID
	}
	if _, gross, fillsErr := t.getFillsTimed(ctx, requestName+"_hedge_guard_fills", hedgeOrderID, hedgeTokenID); fillsErr == nil {
		if pending.RequestedSize > 0 {
			return gross >= minExpected, hedgeOrderID
		}
		if gross > pairArbShareDust {
			return true, hedgeOrderID
		}
	}
	return false, hedgeOrderID
}

// setPrePlacedHedgeOrder tracks a GTC hedge order that was placed before the lead filled.
func (t *Trader) setPrePlacedHedgeOrder(orderID, tokenID, requestName string, requestedSize float64) {
	if t.cfg.PaperTrade || orderID == "" || tokenID == "" {
		return
	}
	t.pendingHedgePrePlace = &pairArbPendingOrderState{
		OrderID:       orderID,
		TokenID:       tokenID,
		RequestName:   requestName,
		PlacedAt:      time.Now(),
		RequestedSize: requestedSize,
	}
	t.savePositionState()
}

func (t *Trader) clearPrePlacedHedgeOrder() {
	if t.pendingHedgePrePlace == nil {
		return
	}
	t.pendingHedgePrePlace = nil
	t.savePositionState()
}

// executeDualPrePlacePairArb places the lead buy AND the hedge buy simultaneously as
// resting GTC maker orders. The hedge rests at a passive price so when the market moves
// both legs can fill in the same second. Execution cases:
//   - Both fill:        open balanced position immediately, HedgeBy cleared.
//   - Lead fills only:  hedge order stays resting as pendingHedgePrePlace; normal hedge
//     management (maybeRebalancePairArb) will absorb it when polled.
//   - Neither fills:    cancel pre-placed hedge, return error (no position).
//   - Hedge only:       unexpected (NO collapsed without YES rising); cancel hedge, return error.
func (t *Trader) executeDualPrePlacePairArb(
	ctx context.Context,
	sig Signal,
	yesTokenID, noTokenID string,
	leadSide string,
	isNoLead bool,
	leadTokenID string,
	hedgeSide string,
	leadPrice, leadLimitPrice, rawLeadShares float64,
	currentHedgePrice, maxHedgePrice float64,
) error {
	// ── Compute hedge pre-place limit price ────────────────────────────────
	// Passive offset: place the hedge bid BELOW the current price so it rests as a maker.
	hedgeLimitPrice := currentHedgePrice
	if off := t.cfg.PairArbHedgePreOffsetCents; off > 0 {
		hedgeLimitPrice = math.Round((currentHedgePrice-off/100.0)*100) / 100
	}
	if hedgeLimitPrice <= 0 || hedgeLimitPrice >= 1.0 {
		return fmt.Errorf("pair arb: dual pre-place hedge limit price %.4f out of range", hedgeLimitPrice)
	}
	if hedgeLimitPrice > maxHedgePrice {
		hedgeLimitPrice = math.Round(maxHedgePrice*100) / 100
	}
	hedgeTokenID := yesTokenID
	if !isNoLead {
		hedgeTokenID = noTokenID // hedge is NO when lead is YES
	}

	// ── Size the hedge leg based on estimated lead actual shares ───────────
	estimatedLeadActual := math.Floor((rawLeadShares-polymarket.ComputeBuyFeeShares(rawLeadShares, leadPrice, t.feeRateBps))*100) / 100
	hedgeRawShares, _, hedgeAllowed := pairArbExactHedgeSizing(estimatedLeadActual, hedgeLimitPrice, t.feeRateBps)
	if !hedgeAllowed || hedgeRawShares <= 0 {
		// Hedge sizing fails (e.g. price too close to max); fall back to sequential.
		t.logger.Warn("trader: dual pre-place hedge sizing failed, falling back to sequential",
			zap.Float64("hedge_limit", hedgeLimitPrice),
			zap.Float64("max_hedge_price", maxHedgePrice),
			zap.Float64("est_lead_actual", estimatedLeadActual),
		)
		outcome, orderID, fillPrice, actualShares, feeShares, usdSpent, err := t.executePairLimitBuy(ctx, "pair_arb_lead_buy", leadTokenID, leadPrice, leadLimitPrice, rawLeadShares)
		if err != nil {
			t.triggerPairArbAmbiguousRecovery(yesTokenID, noTokenID, sig.PolyYesPrice, err)
			return err
		}
		if outcome != pairArbBuyOutcomeFilled {
			pairConditionID := t.convConditionID
			if sig.OverrideConditionID != "" {
				pairConditionID = sig.OverrideConditionID
			}
			pairWindowEnd := t.detector.WindowEnd()
			if !sig.OverrideWindowEnd.IsZero() {
				pairWindowEnd = sig.OverrideWindowEnd
			}
			t.setPendingPairArbContext(orderID, pairConditionID, yesTokenID, noTokenID, leadSide, pairWindowEnd)
			return fmt.Errorf("pair arb: lead buy not filled")
		}
		return t.openLeadOnlyPosition(ctx, sig, yesTokenID, noTokenID, leadSide, isNoLead, orderID, fillPrice, actualShares, feeShares, usdSpent)
	}
	if t.cfg.PaperTrade {
		// In paper mode we never place a live pre-hedge order.
		// Reuse the sequential path so lead/hedge are both simulated.
		outcome, orderID, fillPrice, actualShares, feeShares, usdSpent, err := t.executePairLimitBuy(ctx, "pair_arb_lead_buy", leadTokenID, leadPrice, leadLimitPrice, rawLeadShares)
		if err != nil {
			t.triggerPairArbAmbiguousRecovery(yesTokenID, noTokenID, sig.PolyYesPrice, err)
			return err
		}
		if outcome != pairArbBuyOutcomeFilled {
			pairConditionID := t.convConditionID
			if sig.OverrideConditionID != "" {
				pairConditionID = sig.OverrideConditionID
			}
			pairWindowEnd := t.detector.WindowEnd()
			if !sig.OverrideWindowEnd.IsZero() {
				pairWindowEnd = sig.OverrideWindowEnd
			}
			t.setPendingPairArbContext(orderID, pairConditionID, yesTokenID, noTokenID, leadSide, pairWindowEnd)
			return fmt.Errorf("pair arb: lead buy not filled")
		}
		return t.openLeadOnlyPosition(ctx, sig, yesTokenID, noTokenID, leadSide, isNoLead, orderID, fillPrice, actualShares, feeShares, usdSpent)
	}

	// ── Submit hedge GTC order immediately (non-blocking — rests in book) ──
	// Using placeOrderTimed directly (not executePairLimitBuy) so we don't poll
	// and auto-cancel on a fill-wait timeout.  The hedge stays in the book until
	// the market moves to its price, or forceClosePairArb cancels it.
	hedgeOrderID := ""
	hedgeSubmittedLive := false
	hedgeAlreadyFilled := false
	submitPreHedge := func(rawShares float64) (*polymarket.OrderResponse, error) {
		return t.placeOrderTimed(ctx, "pair_arb_hedge_pre", &polymarket.NewOrderRequest{
			OrderType:      polymarket.OrderTypeGTC,
			TokenID:        hedgeTokenID,
			Side:           polymarket.SideBuy,
			Price:          fmt.Sprintf("%.2f", hedgeLimitPrice),
			Size:           fmt.Sprintf("%.2f", rawShares),
			Nonce:          polymarket.MakeNonce(),
			FeeRateBps:     t.feeRateBps,
			AttemptTimeout: pairArbHedgeSubmitTimeout,
			MaxAttempts:    pairArbHedgeMaxAttempts,
		})
	}

	hedgeResp, hedgePlaceErr := submitPreHedge(hedgeRawShares)
	if hedgePlaceErr == nil && hedgeResp != nil && hedgeResp.Success {
		hedgeOrderID = hedgeResp.OrderID
		status := strings.ToLower(strings.TrimSpace(hedgeResp.Status))
		switch status {
		case "matched", "duplicated":
			hedgeAlreadyFilled = true
		case "live":
			hedgeSubmittedLive = true
		default:
			t.logger.Warn("trader: dual pre-place hedge unexpected submit status",
				zap.String("status", hedgeResp.Status),
				zap.String("order_id", hedgeOrderID),
			)
		}
		t.logger.Info("trader: dual pre-place: hedge GTC order submitted",
			zap.String("hedge_order_id", hedgeOrderID),
			zap.String("hedge_status", hedgeResp.Status),
			zap.Float64("hedge_limit", hedgeLimitPrice),
			zap.String("hedge_token", hedgeTokenID),
		)

		// KALSHI LOCKED-ARB SAFETY:
		// Do not buy the lead while the opposite leg is merely resting.
		// A resting hedge is only a hoped-for arbitrage, not a locked one.
		//
		// Continue to the lead only when the hedge has actual fill evidence.
		if hedgeSubmittedLive && !hedgeAlreadyFilled {
			t.logger.Info("trader: locked-arb entry skipped; hedge not yet filled",
				zap.String("hedge_order_id", hedgeOrderID),
				zap.Float64("hedge_limit", hedgeLimitPrice),
				zap.String("hedge_token", hedgeTokenID),
			)

			if hedgeOrderID != "" {
				if err := t.cancelOrderTimed(
					ctx,
					"pair_arb_locked_entry_cancel_unfilled_hedge",
					hedgeOrderID,
				); err != nil {
					t.logger.Warn(
						"trader: locked-arb unfilled hedge cancel failed; block entry",
						zap.String("hedge_order_id", hedgeOrderID),
						zap.Error(err),
					)
					t.setPrePlacedHedgeOrder(
						hedgeOrderID,
						hedgeTokenID,
						"pair_arb_hedge_pre",
						hedgeRawShares,
					)
					return fmt.Errorf("pair arb: unfilled hedge could not be safely cancelled")
				}
			}

			t.clearPrePlacedHedgeOrder()
			return nil
		}
		// Persist hedge order ID immediately for crash recovery.
		// If we crash before lead fills, RestorePosition will see no PairPosition and cancel it.
		if hedgeSubmittedLive {
			t.setPrePlacedHedgeOrder(hedgeOrderID, hedgeTokenID, "pair_arb_hedge_pre", hedgeRawShares)
		}
	} else {
		t.logger.Warn("trader: dual pre-place: hedge submit failed; will proceed sequentially after lead fills",
			zap.NamedError("hedge_err", hedgePlaceErr),
		)
	}

	// ── Execute lead buy (blocks until fill or PairArbLeadBuyTimeoutSec) ───
	leadRequestName := "pair_arb_lead_buy"
	leadExecLimitPrice := leadLimitPrice

	// If the pre-placed hedge filled first, establish its REAL execution price
	// before buying the complementary lead. The rescue price is economically
	// bounded so completing the pair cannot knowingly violate the configured
	// locked-profit requirement.
	preLeadHedgeFillPrice := 0.0
	preLeadHedgeGrossShares := 0.0

	if hedgeAlreadyFilled {
		avg, gross, fErr := t.getFillsTimed(
			ctx,
			"pair_arb_hedge_pre_rescue_fills",
			hedgeOrderID,
			hedgeTokenID,
		)
		if fErr != nil || avg <= 0 || gross <= pairArbShareDust {
			t.logger.Error(
				"trader: hedge filled first but authoritative hedge fill unavailable; refusing unbounded lead rescue",
				zap.String("hedge_order_id", hedgeOrderID),
				zap.Error(fErr),
				zap.Float64("hedge_fill_price", avg),
				zap.Float64("hedge_fill_shares", gross),
			)

			t.clearPrePlacedHedgeOrder()
			t.recoverDualPrePlaceOrphanedExposure(
				yesTokenID,
				noTokenID,
				hedgeTokenID,
				hedgeOrderID,
				sig.PolyYesPrice,
				true,
			)

			return fmt.Errorf(
				"pair arb: hedge %s filled first but authoritative fill details are unavailable; lead rescue refused",
				hedgeOrderID,
			)
		}

		preLeadHedgeFillPrice = avg
		preLeadHedgeGrossShares = gross

		requiredLockedProfit := t.pairArbMinLockedProfit()

		// Floor to the exchange cent tick. Never round upward: doing so could
		// spend one cent more than the locked-profit inequality permits.
		maxProfitableLeadPrice :=
			math.Floor((1.0-preLeadHedgeFillPrice-requiredLockedProfit)*100) / 100

		if maxProfitableLeadPrice <= 0 || maxProfitableLeadPrice >= 1.0 {
			t.logger.Error(
				"trader: hedge-first rescue has no profitable lead price",
				zap.String("hedge_order_id", hedgeOrderID),
				zap.Float64("hedge_fill_price", preLeadHedgeFillPrice),
				zap.Float64("required_locked_profit", requiredLockedProfit),
				zap.Float64("max_profitable_lead_price", maxProfitableLeadPrice),
			)

			t.clearPrePlacedHedgeOrder()
			t.recoverDualPrePlaceOrphanedExposure(
				yesTokenID,
				noTokenID,
				hedgeTokenID,
				hedgeOrderID,
				sig.PolyYesPrice,
				true,
			)

			return fmt.Errorf(
				"pair arb: hedge-first rescue cannot preserve locked profit",
			)
		}

		leadRequestName = "pair_arb_lead_buy_urgent_rescue"

		// The old rescue paid an arbitrary +3c. Keep that only as an aggression
		// candidate, but HARD-CAP it at the mathematically profitable ceiling.
		aggressiveCandidate :=
			math.Round(math.Min(0.99, leadLimitPrice+0.03)*100) / 100

		leadExecLimitPrice = math.Min(
			aggressiveCandidate,
			maxProfitableLeadPrice,
		)

		// A limit below the quoted/current lead does not justify crossing beyond
		// the profit ceiling. Let the immediate order fail instead of converting
		// the arbitrage into a directional loss.
		t.logger.Warn(
			"trader: dual pre-place hedge filled before lead; bounded urgent rescue",
			zap.String("hedge_order_id", hedgeOrderID),
			zap.Float64("hedge_fill_price", preLeadHedgeFillPrice),
			zap.Float64("hedge_fill_shares", preLeadHedgeGrossShares),
			zap.Float64("required_locked_profit", requiredLockedProfit),
			zap.Float64("original_lead_limit", leadLimitPrice),
			zap.Float64("aggressive_candidate", aggressiveCandidate),
			zap.Float64("max_profitable_lead_price", maxProfitableLeadPrice),
			zap.Float64("urgent_lead_limit", leadExecLimitPrice),
			zap.String("lead_order_type", string(t.pairArbLeadOrderType())),
		)
	}
	leadOutcome, leadOrderID, leadFill, leadActual, leadFeeShares, leadUSD, leadErr := t.executePairLimitBuy(ctx, leadRequestName, leadTokenID, leadPrice, leadExecLimitPrice, rawLeadShares)
	if leadErr != nil {
		t.triggerPairArbAmbiguousRecovery(yesTokenID, noTokenID, sig.PolyYesPrice, leadErr)
		// Lead didn't fill — cancel any hedge order still in the book.
		orphanExposureLikely := hedgeAlreadyFilled
		if hedgeOrderID != "" && (hedgeSubmittedLive || hedgeAlreadyFilled) {
			if cancelErr := t.cancelOrderTimed(ctx, "pair_arb_dual_cancel_orphan_hedge", hedgeOrderID); cancelErr != nil {
				if errors.Is(cancelErr, polymarket.ErrOrderNotCancellable) {
					orphanExposureLikely = true
				}
			}
		}
		t.clearPrePlacedHedgeOrder()
		t.recoverDualPrePlaceOrphanedExposure(yesTokenID, noTokenID, hedgeTokenID, hedgeOrderID, sig.PolyYesPrice, orphanExposureLikely)
		return leadErr
	}
	if leadOutcome != pairArbBuyOutcomeFilled {
		pairConditionID := t.convConditionID
		if sig.OverrideConditionID != "" {
			pairConditionID = sig.OverrideConditionID
		}
		pairWindowEnd := t.detector.WindowEnd()
		if !sig.OverrideWindowEnd.IsZero() {
			pairWindowEnd = sig.OverrideWindowEnd
		}
		t.setPendingPairArbContext(leadOrderID, pairConditionID, yesTokenID, noTokenID, leadSide, pairWindowEnd)
		if leadOrderID != "" && t.pendingPairArb != nil && strings.EqualFold(strings.TrimSpace(t.pendingPairArb.OrderID), strings.TrimSpace(leadOrderID)) {
			// Lead reported MATCHED but wallet credit is still pending. Keep hedge tracking
			// alive and let reconciliation recover/open safely instead of canceling as no-fill.
			if hedgeOrderID != "" {
				t.setPrePlacedHedgeOrder(hedgeOrderID, hedgeTokenID, "pair_arb_hedge_pre", hedgeRawShares)
			}
			return fmt.Errorf("pair arb: lead matched but wallet credit pending; waiting for reconciliation")
		}
		if hedgeOrderID != "" && (hedgeSubmittedLive || hedgeAlreadyFilled) {
			if cancelErr := t.cancelOrderTimed(ctx, "pair_arb_dual_cancel_orphan_hedge", hedgeOrderID); cancelErr != nil {
				if errors.Is(cancelErr, polymarket.ErrOrderNotCancellable) {
					hedgeAlreadyFilled = true
				}
			}
		}
		t.clearPrePlacedHedgeOrder()
		// Lead did not fill, but the pre-placed hedge may have matched during cancel races.
		// Probe both order/fills and wallet exposure and liquidate immediately if needed.
		t.recoverDualPrePlaceOrphanedExposure(yesTokenID, noTokenID, hedgeTokenID, hedgeOrderID, sig.PolyYesPrice, hedgeAlreadyFilled)
		return fmt.Errorf("pair arb: lead buy not filled")
	}

	// ── Lead filled — check if hedge also filled ────────────────────────────
	hedgeFilledActual := 0.0
	hedgeFilledFeeShares := 0.0
	hedgeFilledUSD := 0.0
	hedgeFilledPrice := 0.0

	if hedgeOrderID != "" {
		if hedgeAlreadyFilled {
			// Hedge execution was already authoritatively resolved before the
			// bounded lead rescue. Reuse that exact fill evidence.
			if preLeadHedgeFillPrice > 0 && preLeadHedgeGrossShares > pairArbShareDust {
				hedgeFilledPrice = preLeadHedgeFillPrice
				hedgeFilledFeeShares = polymarket.ComputeBuyFeeShares(
					preLeadHedgeGrossShares,
					preLeadHedgeFillPrice,
					t.feeRateBps,
				)
				hedgeFilledActual = math.Floor(
					(preLeadHedgeGrossShares-hedgeFilledFeeShares)*100,
				) / 100
				hedgeFilledUSD = marketBuyOrderNotional(
					preLeadHedgeGrossShares,
					preLeadHedgeFillPrice,
				)
			}
		} else if hedgeSubmittedLive {
			// Check if the hedge filled during the lead wait.
			if gr, grErr := t.getOrderTimed(ctx, "pair_arb_hedge_pre_poll", hedgeOrderID); grErr == nil && gr != nil {
				pollStatus := strings.ToLower(strings.TrimSpace(gr.Status))
				switch pollStatus {
				case "matched", "filled":
					avg, gross, fErr := t.getFillsTimed(ctx, "pair_arb_hedge_pre_fills", hedgeOrderID, hedgeTokenID)
					if fErr != nil || gross <= 0 || avg <= 0 {
						t.logger.Warn(
							"trader: Kalshi hedge reports filled but authoritative fill price/quantity is not available yet",
							zap.String("hedge_order_id", hedgeOrderID),
							zap.Error(fErr),
						)
					} else {
						hedgeFilledPrice = avg
						hedgeFilledFeeShares = polymarket.ComputeBuyFeeShares(gross, avg, t.feeRateBps)
						hedgeFilledActual = math.Floor((gross-hedgeFilledFeeShares)*100) / 100
						hedgeFilledUSD = marketBuyOrderNotional(gross, avg)
					}
				case "live":
					// Still resting — keep pendingHedgePrePlace (already saved above)
					t.logger.Info("trader: dual pre-place: lead filled, hedge still resting in book",
						zap.String("lead_order_id", leadOrderID),
						zap.String("hedge_order_id", hedgeOrderID),
						zap.Float64("lead_fill", leadFill),
						zap.Float64("hedge_limit", hedgeLimitPrice),
					)
				default:
					// Hedge was cancelled/expired — clear tracking, will retry normally
					t.logger.Warn("trader: dual pre-place: lead filled but pre-placed hedge cancelled",
						zap.String("hedge_status", gr.Status),
						zap.String("hedge_order_id", hedgeOrderID),
					)
					t.clearPrePlacedHedgeOrder()
				}
			}
		}
	}

	// ── Both filled simultaneously ──────────────────────────────────────────
	if hedgeFilledActual > 0 {
		minHedgeShares := hedgeFilledActual - 0.02
		if minHedgeShares < pairArbShareDust {
			minHedgeShares = pairArbShareDust
		}
		credited, bal, cErr := t.pairArbBuyCreditConfirmed(ctx, hedgeTokenID, minHedgeShares)
		if cErr != nil {
			return cErr
		}
		if !credited {
			t.logger.Warn("trader: dual pre-place hedge reported filled but wallet credit not confirmed; treating as pending",
				zap.String("hedge_order_id", hedgeOrderID),
				zap.String("hedge_token", hedgeTokenID),
				zap.Float64("expected_min_shares", minHedgeShares),
				zap.Float64("wallet_shares", bal),
			)
			t.setPrePlacedHedgeOrder(hedgeOrderID, hedgeTokenID, "pair_arb_hedge_pre", hedgeRawShares)
			t.clearPendingPairArbOrder()
			return t.openLeadOnlyPosition(ctx, sig, yesTokenID, noTokenID, leadSide, isNoLead, leadOrderID, leadFill, leadActual, leadFeeShares, leadUSD)
		}
		// If the hedge order only partially filled (Polymarket reported "matched" for the
		// immediate taker portion while leaving a maker remainder resting in the book),
		// cancel the original order NOW before we forget its ID.  Without this cancel the
		// resting remainder silently fills later, lands in the wallet as untracked shares,
		// gets absorbed by reconcileLivePairArbState, and triggers a runaway rebalance
		// cycle (extra YES → buy NO → isBalanced still false → timeout force-close).
		hedgeLeadImbalance := math.Abs(hedgeFilledActual - leadActual)
		if hedgeOrderID != "" && hedgeLeadImbalance > pairArbWalletReconcileDelta {
			t.logger.Warn("trader: dual pre-place: partial taker fill detected; cancelling GTC remainder to prevent ghost fills",
				zap.String("hedge_order_id", hedgeOrderID),
				zap.Float64("filled_actual", hedgeFilledActual),
				zap.Float64("lead_actual", leadActual),
				zap.Float64("hedge_lead_imbalance", hedgeLeadImbalance),
				zap.Float64("remainder_raw", hedgeRawShares-(hedgeFilledActual+hedgeFilledFeeShares)),
			)
			_ = t.cancelOrderTimed(ctx, "pair_arb_dual_cancel_partial_hedge_remainder", hedgeOrderID)
		}
		t.clearPrePlacedHedgeOrder() // no longer pending
		now := time.Now()
		pairConditionID := t.convConditionID
		if sig.OverrideConditionID != "" {
			pairConditionID = sig.OverrideConditionID
		}
		pairWindowEnd := t.detector.WindowEnd()
		if !sig.OverrideWindowEnd.IsZero() {
			pairWindowEnd = sig.OverrideWindowEnd
		}
		pair := &PairArbPosition{
			ConditionID: pairConditionID,
			YesTokenID:  yesTokenID,
			NoTokenID:   noTokenID,
			LeadSide:    leadSide,
			OpenPrice:   sig.OpenPrice,
			WindowEnd:   pairWindowEnd,
			OpenedAt:    now,
		}
		if isNoLead {
			pair.NoShares = leadActual
			pair.NoUSDSpent = leadUSD
			pair.YesShares = hedgeFilledActual
			pair.YesUSDSpent = hedgeFilledUSD
			pair.NoWalletConfirmed = true
			pair.YesWalletConfirmed = true
		} else {
			pair.YesShares = leadActual
			pair.YesUSDSpent = leadUSD
			pair.NoShares = hedgeFilledActual
			pair.NoUSDSpent = hedgeFilledUSD
			pair.YesWalletConfirmed = true
			pair.NoWalletConfirmed = true
		}
		targetMet, actualLockedProfit, requiredLockedProfit :=
			pair.meetsLockedProfitTarget(t.pairArbMinLockedProfit())

		if pair.isBalanced() {
			// The complete position is economically protected. Do NOT create
			// additional market risk merely because execution finished below
			// the preferred locked-profit target.
			pair.BalancedAt = now
			pair.HedgeBy = time.Time{}

			if !targetMet {
				t.logger.Warn(
					"trader: pair protected but post-fill locked-profit target was NOT achieved",
					zap.Float64("locked_shares", pair.lockedShares()),
					zap.Float64("total_spent", pair.totalSpent()),
					zap.Float64("actual_locked_profit", actualLockedProfit),
					zap.Float64("required_locked_profit", requiredLockedProfit),
					zap.Float64("actual_profit_per_share",
						actualLockedProfit/pair.lockedShares()),
					zap.Float64("required_profit_per_share",
						t.pairArbMinLockedProfit()),
				)
			}
		} else {
			pair.HedgeBy = now.Add(t.pairArbHedgeTimeout())
			if !pair.WindowEnd.IsZero() && pair.HedgeBy.After(pair.WindowEnd) {
				pair.HedgeBy = pair.WindowEnd
			}
		}
		t.pairedPosition = pair
		t.rememberBotConditionID(pair.ConditionID)
		t.detector.SetInPosition(true)
		t.savePositionState()
		t.logger.Info("trader: dual pre-place: both legs have fill evidence",
			zap.String("lead_side", leadSide),
			zap.String("lead_order", leadOrderID),
			zap.Float64("lead_fill", leadFill),
			zap.String("hedge_order", hedgeOrderID),
			zap.Float64("hedge_fill", hedgeFilledPrice),
			zap.Float64("hedge_lead_imbalance", hedgeLeadImbalance),
			zap.Bool("share_balanced", hedgeLeadImbalance <= pairArbWalletReconcileDelta),
			zap.Float64("locked_shares", pair.lockedShares()),
			zap.Float64("locked_profit", actualLockedProfit),
			zap.Float64("required_locked_profit", requiredLockedProfit),
			zap.Bool("locked_profit_target_met", targetMet),
		)
		display.TradeOpen(leadSide+" [PAIR LEAD]", leadFill, leadActual, leadFeeShares, leadUSD, 1.0-leadFill-t.pairArbMinLockedProfit(), t.cfg.PaperTrade)
		display.TradeOpen(hedgeSide+" [PAIR HEDGE]", hedgeFilledPrice, hedgeFilledActual, hedgeFilledFeeShares, hedgeFilledUSD, 0, t.cfg.PaperTrade)
		return t.managePairArbPosition(ctx, sig.PolyYesPrice, sig.PolyNoPrice, 0)
	}

	// ── Lead filled only; hedge is resting or not submitted ─────────────────
	// pendingHedgePrePlace is already set (if hedge is live) by setPrePlacedHedgeOrder above.
	// openLeadOnlyPosition → managePairArbPosition → maybeRebalancePairArb will poll it.
	return t.openLeadOnlyPosition(ctx, sig, yesTokenID, noTokenID, leadSide, isNoLead, leadOrderID, leadFill, leadActual, leadFeeShares, leadUSD)
}

// enterDualPairArb is the entry path used when PairArbContinuousImbalanceEnabled is true.
// It buys both YES and NO sides simultaneously with equal share counts (sized off the
// average of both prices) so the position is balanced from the first tick.  Ongoing
// dip-adds are handled by maybeStartContinuousImbalance on every subsequent price tick.
func (t *Trader) enterDualPairArb(ctx context.Context, sig Signal, yesTokenID, noTokenID string) error {
	yesPrice := sig.PolyYesPrice
	noPrice := sig.PolyNoPrice
	if noPrice <= 0 && yesPrice > 0 && yesPrice < 1.0 {
		noPrice = math.Round((1.0-yesPrice)*100) / 100
	}
	if yesPrice <= 0 || yesPrice >= 1.0 || noPrice <= 0 || noPrice >= 1.0 {
		return fmt.Errorf("pair arb dual: invalid entry prices yes=%.4f no=%.4f", yesPrice, noPrice)
	}

	// Window-time gate: PairArbMinWindowSec / PairArbMaxWindowSec are elapsed-seconds bounds.
	// Reject if we are too early (prices not settled) or too late (market outcome likely decided).
	// Use WindowElapsedSec() rather than deriving start from pairWindowDuration(windowEnd), which
	// misidentifies 5-minute windows ending exactly on an hour boundary as 1-hour windows.
	minWindowSec, maxWindowSec, minGapUSD, maxGapUSD := t.detector.PairArbEntryLimits()
	elapsedSec := t.detector.WindowElapsedSec()
	if elapsedSec > 0 {
		if minWindowSec > 0 && elapsedSec < float64(minWindowSec) {
			return fmt.Errorf("pair arb dual: too early in window (%.0fs elapsed < min %ds)", elapsedSec, minWindowSec)
		}
		if maxWindowSec > 0 && elapsedSec > float64(maxWindowSec) {
			return fmt.Errorf("pair arb dual: too late in window (%.0fs elapsed > max %ds); market likely decided", elapsedSec, maxWindowSec)
		}
	}
	// Gap gate: reject if BTC has already moved so far that the market outcome is decided.
	if sig.BitstampPrice > 0 && sig.OpenPrice > 0 {
		gapAbs := math.Abs(sig.BitstampPrice - sig.OpenPrice)
		if minGapUSD > 0 && gapAbs < minGapUSD {
			return fmt.Errorf("pair arb dual: gap too small (%.2f < min %.2f)", gapAbs, minGapUSD)
		}
		if maxGapUSD > 0 && gapAbs > maxGapUSD {
			return fmt.Errorf("pair arb dual: gap too large / market decided (%.2f > max %.2f)", gapAbs, maxGapUSD)
		}
	}

	slipTicks := t.cfg.PairArbLeadBuySlipTicks
	if slipTicks < 0 {
		slipTicks = int(pairArbLimitBuySlip * 100)
	}
	yesLimit := math.Round((yesPrice+float64(slipTicks)*0.01)*100) / 100
	noLimit := math.Round((noPrice+float64(slipTicks)*0.01)*100) / 100
	if yesLimit >= 1.0 {
		yesLimit = 0.99
	}
	if noLimit >= 1.0 {
		noLimit = 0.99
	}
	if yesLimit+noLimit >= 1.0 {
		return fmt.Errorf("pair arb dual: no edge after slippage — yes=%.4f no=%.4f combined=%.4f >= 1.00", yesLimit, noLimit, yesLimit+noLimit)
	}

	tradeSize := t.pairArbTradeSizeUSD()

	// Balance-aware sizing: cap tradeSize so both legs fit within available funds.
	// Keep YES and NO shares equal so the position stays hedged from entry.
	// Each leg costs roughly tradeSize (at the average limit price), so the pair
	// needs ~2×tradeSize in total. If the available balance is less, scale down
	// to half of what is available (leaving a small reserve for fees/slippage).
	var available float64
	if t.cfg.PaperTrade {
		available = t.paperBalance
	} else {
		available = math.Float64frombits(atomic.LoadUint64(&t.cachedLiveBalance))
	}
	if available > 0 {
		// Reserve 5% for fees/slippage; each leg gets half of what remains.
		maxPerLeg := math.Floor((available*0.95/2)*100) / 100
		if maxPerLeg < tradeSize {
			if maxPerLeg < polymarket.MinMarketOrderNotionalUSD {
				return fmt.Errorf("pair arb dual: insufficient balance ($%.2f) — too small to trade", available)
			}
			t.logger.Info("pair arb dual: scaling down trade size to fit balance",
				zap.Float64("configured", tradeSize),
				zap.Float64("scaled_to", maxPerLeg),
				zap.Float64("available", available),
			)
			tradeSize = maxPerLeg
		}
	}

	// Use average limit price to size equal shares on both sides.
	avgLimit := (yesLimit + noLimit) / 2
	rawShares := math.Round((tradeSize/avgLimit)*100) / 100
	if rawShares <= 0 {
		return fmt.Errorf("pair arb dual: invalid share sizing tradeSize=%.2f avg=%.4f", tradeSize, avgLimit)
	}

	// Final pre-flight balance check (post-scaling).
	yesCost := marketBuyOrderNotional(rawShares, yesLimit)
	noCost := marketBuyOrderNotional(rawShares, noLimit)
	totalRequired := yesCost + noCost
	if available > 0 && available < totalRequired {
		return fmt.Errorf("pair arb dual: insufficient balance ($%.2f) for both legs ($%.2f total)", available, totalRequired)
	}

	// Buy YES leg.
	t.buyInProgress = true
	outcomeYes, orderIDYes, fillPriceYes, actualSharesYes, feeSharesYes, usdSpentYes, errYes := t.executePairLimitBuy(ctx, "pair_arb_dual_yes_buy", yesTokenID, yesPrice, yesLimit, rawShares)
	t.buyInProgress = false
	if errYes != nil {
		t.triggerPairArbAmbiguousRecovery(yesTokenID, noTokenID, sig.PolyYesPrice, errYes)
		return errYes
	}
	if outcomeYes != pairArbBuyOutcomeFilled || actualSharesYes <= pairArbShareDust {
		t.logger.Debug("pair arb dual: YES leg not filled; skipping signal")
		return nil
	}

	now := time.Now()
	pairConditionID := t.convConditionID
	if sig.OverrideConditionID != "" {
		pairConditionID = sig.OverrideConditionID
	}
	pairWindowEnd := t.detector.WindowEnd()
	if !sig.OverrideWindowEnd.IsZero() {
		pairWindowEnd = sig.OverrideWindowEnd
	}
	pair := &PairArbPosition{
		ConditionID: pairConditionID,
		YesTokenID:  yesTokenID,
		NoTokenID:   noTokenID,
		LeadSide:    "YES",
		OpenPrice:   sig.OpenPrice,
		WindowEnd:   pairWindowEnd,
		OpenedAt:    now,
		HedgeBy:     now.Add(t.pairArbHedgeTimeout()), // safety net if NO leg fails
		YesShares:   actualSharesYes,
		YesUSDSpent: usdSpentYes,
	}
	if !pair.WindowEnd.IsZero() && pair.HedgeBy.After(pair.WindowEnd) {
		pair.HedgeBy = pair.WindowEnd
	}
	t.pairedPosition = pair
	t.rememberBotConditionID(pairConditionID)
	t.detector.SetInPosition(true)
	t.savePositionState()
	t.logger.Info("trader: pair arb dual: YES leg opened",
		zap.String("order_id", orderIDYes),
		zap.Float64("fill_price", fillPriceYes),
		zap.Float64("shares", actualSharesYes),
		zap.Float64("usd_spent", usdSpentYes),
	)
	display.TradeOpen("YES [PAIR DUAL]", fillPriceYes, actualSharesYes, feeSharesYes, usdSpentYes, 1.0-fillPriceYes-noPrice, t.cfg.PaperTrade)

	// Buy NO leg.
	outcomeNo, orderIDNo, fillPriceNo, actualSharesNo, feeSharesNo, usdSpentNo, errNo := t.executePairLimitBuy(ctx, "pair_arb_dual_no_buy", noTokenID, noPrice, noLimit, rawShares)
	if errNo != nil || outcomeNo != pairArbBuyOutcomeFilled || actualSharesNo <= pairArbShareDust {
		t.logger.Warn("trader: pair arb dual: NO leg failed; falling back to hedge retry loop",
			zap.Error(errNo),
		)
		// YES is open; the normal hedge-retry path in managePairArbPosition will recover NO.
		_, _, liveYes, _, _, _ := t.detector.Snapshot()
		if liveYes <= 0 || liveYes >= 1.0 {
			liveYes = sig.PolyYesPrice
		}
		liveNo := math.Round((1.0-liveYes)*100) / 100
		return t.managePairArbPosition(ctx, liveYes, liveNo, 0)
	}

	pair.NoShares = actualSharesNo
	pair.NoUSDSpent = usdSpentNo
	pair.BalancedAt = now
	pair.HedgeBy = time.Time{} // both legs present; no hedge deadline needed
	t.savePositionState()
	t.logger.Info("trader: pair arb dual: NO leg opened; fully hedged from entry",
		zap.String("order_id", orderIDNo),
		zap.Float64("fill_price", fillPriceNo),
		zap.Float64("shares", actualSharesNo),
		zap.Float64("usd_spent", usdSpentNo),
		zap.Float64("edge_per_share", 1.0-fillPriceYes-fillPriceNo),
	)
	display.TradeOpen("NO [PAIR DUAL]", fillPriceNo, actualSharesNo, feeSharesNo, usdSpentNo, 1.0-fillPriceYes-fillPriceNo, t.cfg.PaperTrade)

	_, _, finalYes, _, _, _ := t.detector.Snapshot()
	if finalYes <= 0 || finalYes >= 1.0 {
		finalYes = sig.PolyYesPrice
	}
	finalNo := math.Round((1.0-finalYes)*100) / 100
	return t.managePairArbPosition(ctx, finalYes, finalNo, 0)
}

// openLeadOnlyPosition is called after the lead buy fills (in sequential or dual-pre mode)
// to record the one-sided position and kick off hedge management.
func (t *Trader) openLeadOnlyPosition(ctx context.Context, sig Signal, yesTokenID, noTokenID, leadSide string, isNoLead bool, orderID string, fillPrice, actualShares, feeShares, usdSpent float64) error {
	now := time.Now()
	pairConditionID := t.convConditionID
	if sig.OverrideConditionID != "" {
		pairConditionID = sig.OverrideConditionID
	}
	pairWindowEnd := t.detector.WindowEnd()
	if !sig.OverrideWindowEnd.IsZero() {
		pairWindowEnd = sig.OverrideWindowEnd
	}
	pair := &PairArbPosition{
		ConditionID: pairConditionID,
		YesTokenID:  yesTokenID,
		NoTokenID:   noTokenID,
		LeadSide:    leadSide,
		OpenPrice:   sig.OpenPrice,
		WindowEnd:   pairWindowEnd,
		OpenedAt:    now,
		HedgeBy:     now.Add(t.pairArbHedgeTimeout()),
	}
	if !pair.WindowEnd.IsZero() && pair.HedgeBy.After(pair.WindowEnd) {
		pair.HedgeBy = pair.WindowEnd
	}
	if isNoLead {
		pair.NoShares = actualShares
		pair.NoUSDSpent = usdSpent
	} else {
		pair.YesShares = actualShares
		pair.YesUSDSpent = usdSpent
	}
	t.pairedPosition = pair
	t.rememberBotConditionID(pair.ConditionID)
	t.detector.SetInPosition(true)
	t.savePositionState()
	t.logger.Info("trader: pair arb lead leg opened",
		zap.String("order_id", orderID),
		zap.String("lead_side", leadSide),
		zap.Float64("fill_price", fillPrice),
		zap.Float64("shares", actualShares),
		zap.Float64("usd_spent", usdSpent),
		zap.Float64("target_locked_profit", t.pairArbMinLockedProfit()),
	)
	display.TradeOpen(leadSide+" [PAIR LEAD]", fillPrice, actualShares, feeShares, usdSpent, 1.0-fillPrice-t.pairArbMinLockedProfit(), t.cfg.PaperTrade)
	return t.managePairArbPosition(ctx, sig.PolyYesPrice, sig.PolyNoPrice, 0)
}

// executeFOKHedgeAfterLeadFill is used when the lead order type is FOK.
// It attempts up to maxFOKHedgeAttempts immediate FOK buys on the hedge leg at
// hedgeLimitPrice (= 1 - leadAvg - minLockedProfit).  On first fill it records
// a balanced position and calls managePairArbPosition.  If all attempts miss
// (e.g. hedge asks are above the locked-profit limit), it calls forceClosePairArb
// to unwind the lead immediately.  There is no resting order and no timeout loop.
const maxFOKHedgeAttempts = 3

func (t *Trader) executeFOKHedgeAfterLeadFill(ctx context.Context, sig Signal, isNoLead bool, leadSide string) error {
	pair := t.pairedPosition
	if pair == nil {
		return nil
	}

	buySide, deficitShares, anchorAvgPrice := pair.rebalanceState()
	if buySide == "" || deficitShares <= 0 || anchorAvgPrice <= 0 {
		// Already balanced (shouldn't happen right after lead fill, but handle gracefully).
		fs := t.detector.PairArbSnapshot()
		finalYes, finalNo := fs.YesPrice, fs.NoPrice
		if finalYes <= 0 || finalYes >= 1.0 {
			finalYes = sig.PolyYesPrice
		}
		if finalNo <= 0 || finalNo >= 1.0 {
			finalNo = sig.PolyNoPrice
		}
		return t.managePairArbPosition(ctx, finalYes, finalNo, 0)
	}

	hedgeTokenID := pair.YesTokenID // lead=NO → hedge=YES
	if !isNoLead {
		hedgeTokenID = pair.NoTokenID // lead=YES → hedge=NO
	}
	if hedgeTokenID == "" {
		t.logger.Warn("trader: FOK hedge token ID missing; force-closing lead", zap.String("lead_side", leadSide))
		_, _, liveYes, _, _, _ := t.detector.Snapshot()
		if liveYes <= 0 || liveYes >= 1.0 {
			liveYes = sig.PolyYesPrice
		}
		return t.forceClosePairArb(ctx, liveYes, "pair_fok_hedge_no_token")
	}

	hedgeLimitPrice := pairKalshiMaxHedgePrice(anchorAvgPrice, deficitShares, t.pairArbMinLockedProfit())
	forceLiveYes := func() float64 {
		_, _, y, _, _, _ := t.detector.Snapshot()
		if y <= 0 || y >= 1.0 {
			return sig.PolyYesPrice
		}
		return y
	}
	if hedgeLimitPrice <= 0 || hedgeLimitPrice >= 1.0 {
		t.logger.Warn("trader: FOK hedge limit price out of range; force-closing lead",
			zap.String("lead_side", leadSide),
			zap.Float64("anchor_avg", anchorAvgPrice),
			zap.Float64("hedge_limit", hedgeLimitPrice),
		)
		return t.forceClosePairArb(ctx, forceLiveYes(), "pair_fok_hedge_limit_invalid")
	}

	rawShares, _, hedgeAllowed := pairArbExactHedgeSizing(deficitShares, hedgeLimitPrice, t.feeRateBps)
	if !hedgeAllowed || rawShares <= 0 {
		t.logger.Warn("trader: FOK hedge sizing failed; force-closing lead",
			zap.String("lead_side", leadSide),
			zap.Float64("deficit_shares", deficitShares),
			zap.Float64("hedge_limit", hedgeLimitPrice),
		)
		return t.forceClosePairArb(ctx, forceLiveYes(), "pair_fok_hedge_sizing_failed")
	}

	const retryDelay = 150 * time.Millisecond
	for attempt := 0; attempt < maxFOKHedgeAttempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryDelay):
			}
		}

		// Request name contains "hedge_buy" → executePairLimitBuy uses pairArbLeadOrderType() = FOK.
		ho, _, hFill, hActual, hFeeShares, hUSD, hErr := t.executePairLimitBuy(
			ctx, "pair_arb_hedge_buy_fok", hedgeTokenID,
			hedgeLimitPrice, hedgeLimitPrice, rawShares,
		)
		if hErr != nil {
			t.logger.Warn("trader: FOK hedge attempt error",
				zap.Int("attempt", attempt+1),
				zap.Error(hErr),
			)
			continue
		}
		if ho == pairArbBuyOutcomeFilled && hActual > 0 {
			if isNoLead {
				pair.YesShares += hActual
				pair.YesUSDSpent += hUSD
			} else {
				pair.NoShares += hActual
				pair.NoUSDSpent += hUSD
			}
			if pair.isBalanced() {
				pair.BalancedAt = time.Now()
				pair.HedgeBy = time.Time{}
			}
			t.clearPendingPairArbOrder()
			t.savePositionState()
			lockedPerShare := 1.0 - pair.sideAveragePrice("YES") - pair.sideAveragePrice("NO")
			display.TradeOpen(buySide+" [PAIR HEDGE]", hFill, hActual, hFeeShares, hUSD, lockedPerShare, t.cfg.PaperTrade)
			t.logger.Info("trader: FOK hedge filled; pair balanced",
				zap.String("hedge_side", buySide),
				zap.Float64("hedge_fill", hFill),
				zap.Float64("hedge_shares", hActual),
				zap.Float64("locked_profit_per_share", lockedPerShare),
				zap.Int("attempt", attempt+1),
			)
			fs := t.detector.PairArbSnapshot()
			finalYes, finalNo := fs.YesPrice, fs.NoPrice
			if finalYes <= 0 || finalYes >= 1.0 {
				finalYes = sig.PolyYesPrice
			}
			if finalNo <= 0 || finalNo >= 1.0 {
				finalNo = sig.PolyNoPrice
			}
			return t.managePairArbPosition(ctx, finalYes, finalNo, 0)
		}
		t.logger.Info("trader: FOK hedge not filled; retrying",
			zap.Int("attempt", attempt+1),
			zap.Float64("hedge_limit", hedgeLimitPrice),
			zap.String("hedge_side", buySide),
		)
	}

	// All FOK hedge attempts failed — unwind the lead leg immediately.
	t.logger.Warn("trader: FOK hedge not filled after all attempts; force-closing lead",
		zap.String("lead_side", leadSide),
		zap.Int("attempts", maxFOKHedgeAttempts),
		zap.Float64("hedge_limit", hedgeLimitPrice),
	)
	return t.forceClosePairArb(ctx, forceLiveYes(), "pair_fok_hedge_not_filled")
}

func (t *Trader) pairArbTradeSizeUSD() float64 {
	if t.cfg.PairArbTradeSizeUSD > 0 {
		return t.cfg.PairArbTradeSizeUSD
	}
	return t.cfg.TradeSizeUSD
}

func (t *Trader) pairArbMinLockedProfit() float64 {
	// CVD momentum positions store a per-position override in LockedProfitCents.
	if t.pairedPosition != nil && t.pairedPosition.LockedProfitCents > 0 {
		return t.pairedPosition.LockedProfitCents / 100.0
	}
	if t.cfg.PairArbMinLockedProfitCents <= 0 {
		return 0.08
	}
	return t.cfg.PairArbMinLockedProfitCents / 100.0
}

func (t *Trader) pairArbSellPrice() float64 {
	return 0.99
}

// makePairArbLeadGapGuard returns a function that is checked on each GTC poll tick.
// It returns (true, reason) when the BTC-vs-open gap has reverted below the configured
// entry minimum, indicating the arbitrage edge that fired the original signal is gone.
// The caller cancels the resting lead order immediately.
// Returns nil (disabled) when PairArbMinBTCGapUSD is 0.
func (t *Trader) makePairArbLeadGapGuard(isNoLead bool) func() (bool, string) {
	_, _, minGap, _ := t.detector.PairArbEntryLimits()
	if minGap <= 0 {
		return nil
	}
	return func() (bool, string) {
		snap := t.detector.PairArbSnapshot()
		if isNoLead {
			// NO lead: signal required gap < -minGap (BTC below open).
			// Cancel when gap reverts above -minGap.
			if snap.GapUSD > -minGap {
				return true, fmt.Sprintf("gap reverted: btc_gap=%.2f above NO threshold -%.2f", snap.GapUSD, minGap)
			}
		} else {
			// YES lead: signal required gap > +minGap (BTC above open).
			// Cancel when gap falls below minGap.
			if snap.GapUSD < minGap {
				return true, fmt.Sprintf("gap reverted: btc_gap=%.2f below YES threshold +%.2f", snap.GapUSD, minGap)
			}
		}
		return false, ""
	}
}

// shouldAbortUnprofitableUnhedgedPairArb returns true when an unhedged pair position
// should be force-closed before HedgeBy because the lead is already losing and the
// original signal has weakened, reversed momentum, or flipped direction.
//
// It applies PairArbUnprofitableAbortGraceSec and PairArbUnprofitableAbortMinGapAgainstUSD.
func (t *Trader) shouldAbortUnprofitableUnhedgedPairArb(pair *PairArbPosition, currentYesPrice float64, currentNoPrice float64, now time.Time) (bool, float64, string) {
	if pair == nil || pair.isBalanced() || t.detector == nil {
		return false, 0, ""
	}
	if grace := t.cfg.PairArbUnprofitableAbortGraceSec; grace > 0 {
		if now.Sub(pair.OpenedAt) < time.Duration(grace)*time.Second {
			return false, 0, ""
		}
	}

	isNoLead := strings.EqualFold(pair.LeadSide, "NO")
	leadNow := currentYesPrice
	if isNoLead {
		leadNow = currentNoPrice
		if leadNow <= 0 && currentYesPrice > 0 && currentYesPrice < 1.0 {
			leadNow = math.Round((1.0-currentYesPrice)*100) / 100
		}
	}
	leadAvg := pair.sideAveragePrice(pair.LeadSide)
	if leadAvg <= 0 || leadNow <= 0 {
		return false, 0, ""
	}
	// This guard is specifically for unprofitable, still-unhedged exposure.
	if leadNow >= leadAvg {
		return false, 0, ""
	}

	snap := t.detector.PairArbSnapshot()
	if snap.BTCPrice <= 0 || snap.OpenPrice <= 0 {
		return false, 0, ""
	}
	directionFlipped := (!isNoLead && snap.GapUSD <= 0) || (isNoLead && snap.GapUSD >= 0)
	momentumAgainst := (!isNoLead && snap.GapVelocity < 0) || (isNoLead && snap.GapVelocity > 0)
	_, _, minGap, _ := t.detector.PairArbEntryLimits()
	gapStrengthLost := false
	if minGap > 0 {
		gapStrengthLost = (!isNoLead && snap.GapUSD < minGap) || (isNoLead && snap.GapUSD > -minGap)
	}
	if !directionFlipped && !momentumAgainst && !gapStrengthLost {
		return false, 0, ""
	}

	adverseGap := 0.0
	if !isNoLead {
		if snap.GapUSD < 0 {
			adverseGap = -snap.GapUSD
		}
	} else {
		if snap.GapUSD > 0 {
			adverseGap = snap.GapUSD
		}
	}
	if minGapAgainst := t.cfg.PairArbUnprofitableAbortMinGapAgainstUSD; minGapAgainst > 0 {
		// Direction flip is always actionable; for softer deterioration signals
		// require a minimum adverse BTC/open move.
		if !directionFlipped && adverseGap < minGapAgainst {
			return false, 0, ""
		}
	}

	trigger := "signal_deteriorated"
	if directionFlipped {
		trigger = "direction_flipped"
	} else if gapStrengthLost {
		trigger = "gap_strength_lost"
	} else if momentumAgainst {
		trigger = "momentum_reversed"
	}
	reason := fmt.Sprintf(
		"%s lead=%s lead_now=%.4f lead_avg=%.4f gap=%.2f vel=%.3f adverse_gap=%.2f",
		trigger, pair.LeadSide, leadNow, leadAvg, snap.GapUSD, snap.GapVelocity, adverseGap,
	)
	return true, leadNow, reason
}

func (t *Trader) pairArbLockedPayoutTarget() float64 {
	if t.cfg.PairArbSellAt99 {
		return t.pairArbSellPrice()
	}
	return 1.0
}

func (t *Trader) pairArbLeadOrderType() polymarket.OrderType {
	switch strings.ToUpper(strings.TrimSpace(string(t.cfg.PairArbLeadOrderType))) {
	case string(polymarket.OrderTypeFOK):
		return polymarket.OrderTypeFOK
	case string(polymarket.OrderTypeGTC):
		return polymarket.OrderTypeGTC
	default:
		return polymarket.OrderTypeFAK
	}
}

func (t *Trader) pairArbHedgeTimeout() time.Duration {
	if t.cfg.PairArbHedgeTimeoutSec <= 0 {
		return 5 * time.Second
	}
	return time.Duration(t.cfg.PairArbHedgeTimeoutSec) * time.Second
}

func (t *Trader) pairArbHedgeDeadline(windowEnd time.Time) time.Time {
	deadline := time.Now().Add(t.pairArbHedgeTimeout())
	if !windowEnd.IsZero() && deadline.After(windowEnd) {
		return windowEnd
	}
	return deadline
}

func recoveredOrderMatchesRequest(gr *polymarket.GetOrderResponse, req *polymarket.NewOrderRequest) bool {
	if gr == nil || req == nil || gr.ID == "" {
		return false
	}
	if req.TokenID != "" && !strings.EqualFold(strings.TrimSpace(gr.TokenID), strings.TrimSpace(req.TokenID)) {
		return false
	}
	if req.Side != "" && !strings.EqualFold(strings.TrimSpace(gr.Side), string(req.Side)) {
		return false
	}
	if req.Price != "" {
		expected, err := strconv.ParseFloat(req.Price, 64)
		if err == nil && expected > 0 {
			seen, pErr := strconv.ParseFloat(gr.Price, 64)
			if pErr == nil && seen > 0 && math.Abs(seen-expected) > 0.03 {
				return false
			}
		}
	}
	if req.Size != "" {
		expected, err := strconv.ParseFloat(req.Size, 64)
		if err == nil && expected > 0 {
			sizeRaw := strings.TrimSpace(gr.OriginalSize)
			if sizeRaw == "" {
				sizeRaw = strings.TrimSpace(gr.SizeMatched)
			}
			if sizeRaw != "" {
				seen, sErr := strconv.ParseFloat(sizeRaw, 64)
				if sErr == nil && seen > 0 {
					allowedDelta := math.Max(1.0, expected*0.35)
					if math.Abs(seen-expected) > allowedDelta {
						return false
					}
				}
			}
		}
	}
	return true
}

func (t *Trader) recoverAmbiguousSubmitOrder(ctx context.Context, req *polymarket.NewOrderRequest, originalErr error) (*polymarket.OrderResponse, error) {
	if t.orders == nil || req == nil || req.TokenID == "" {
		return nil, originalErr
	}
	recoverTimeout := ambiguousSubmitRecoverTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remain := time.Until(deadline); remain > 0 && remain < recoverTimeout {
			recoverTimeout = remain
		}
	}
	recoverCtx, cancel := context.WithTimeout(context.Background(), recoverTimeout)
	defer cancel()
	ticker := time.NewTicker(ambiguousSubmitRecoverPoll)
	defer ticker.Stop()

	for {
		if recovered, err := t.orders.FindRecentOrder(recoverCtx, req.TokenID); err == nil && recoveredOrderMatchesRequest(recovered, req) {
			status := strings.TrimSpace(recovered.Status)
			if status == "" {
				status = "LIVE"
			}
			return &polymarket.OrderResponse{Success: true, Status: status, OrderID: recovered.ID}, nil
		}
		if recoveredTrade, err := t.orders.FindRecentTrade(recoverCtx, req.TokenID); err == nil && recoveredOrderMatchesRequest(recoveredTrade, req) {
			return &polymarket.OrderResponse{Success: true, Status: "MATCHED", OrderID: recoveredTrade.ID}, nil
		}
		select {
		case <-recoverCtx.Done():
			return nil, originalErr
		case <-ticker.C:
		}
	}
}

func (t *Trader) placeOrderTimed(ctx context.Context, requestName string, req *polymarket.NewOrderRequest, extraFields ...zap.Field) (*polymarket.OrderResponse, error) {
	if t.cfg.PaperTrade {
		err := fmt.Errorf("paper mode guard blocked live order request: %s", requestName)
		logTimedActivityWarn(fmt.Sprintf("PAPER guard blocked live %s request", activityRequestLabel(requestName)))
		t.logger.Error("trader: paper mode guard blocked live order request",
			append([]zap.Field{
				zap.String("request", requestName),
				zap.String("side", string(req.Side)),
				zap.String("order_type", string(req.OrderType)),
				zap.String("token_id", req.TokenID),
				zap.String("price", req.Price),
				zap.String("size", req.Size),
			}, extraFields...)...,
		)
		return nil, err
	}

	start := time.Now()
	resp, err := t.orders.PlaceOrder(ctx, req)
	duration := time.Since(start)
	if t.metrics != nil {
		t.metrics.Record("order_place", duration.Milliseconds())
		if err == nil && resp != nil {
			if resp.SignMs > 0 {
				t.metrics.Record("order_sign", resp.SignMs)
			}
			if resp.HTTPMs > 0 {
				t.metrics.Record("order_http", resp.HTTPMs)
			}
			if resp.HTTPDNSMs > 0 {
				t.metrics.Record("order_http_dns", resp.HTTPDNSMs)
			}
			if resp.HTTPConnectMs > 0 {
				t.metrics.Record("order_http_connect", resp.HTTPConnectMs)
			}
			if resp.HTTPTLSMs > 0 {
				t.metrics.Record("order_http_tls", resp.HTTPTLSMs)
			}
			if resp.HTTPTTFBMs > 0 {
				t.metrics.Record("order_http_ttfb", resp.HTTPTTFBMs)
			}
		}
	}
	activityLabel := activityRequestLabel(requestName)
	fields := []zap.Field{
		zap.String("request", requestName),
		zap.String("side", string(req.Side)),
		zap.String("order_type", string(req.OrderType)),
		zap.String("token_id", req.TokenID),
		zap.String("price", req.Price),
		zap.String("size", req.Size),
		zap.Int64("request_ms", duration.Milliseconds()),
	}
	fields = append(fields, extraFields...)
	if err != nil {
		if isAmbiguousPlaceOrderParseError(err) || isNetworkError(err) {
			if recoveredResp, recoverErr := t.recoverAmbiguousSubmitOrder(ctx, req, err); recoverErr == nil && recoveredResp != nil && recoveredResp.Success {
				t.logger.Warn("trader: recovered ambiguous order submit via post-submit lookup",
					append(fields,
						zap.String("recovered_order_id", recoveredResp.OrderID),
						zap.String("recovered_status", recoveredResp.Status),
						zap.Error(err),
					)...,
				)
				return recoveredResp, nil
			}
		}
		if req.OrderType == polymarket.OrderTypeFAK && req.Side == polymarket.SideBuy && isRetryableFAKNoMatchError(err) {
			t.logger.Debug("trader: FAK buy found no immediate matching liquidity", append(fields, zap.Error(err))...)
			return &polymarket.OrderResponse{
				Success:  true,
				Status:   "UNMATCHED",
				ErrorMsg: err.Error(),
			}, nil
		}
		logTimedActivityWarn(fmt.Sprintf("%s %s %s failed in %d ms: %v", strings.ToUpper(string(req.Side)), req.OrderType, activityLabel, duration.Milliseconds(), err))
		t.logger.Warn("trader: order request failed", append(fields, zap.Error(err))...)
		return nil, err
	}
	if resp == nil {
		logTimedActivityWarn(fmt.Sprintf("%s %s %s returned nil response in %d ms", strings.ToUpper(string(req.Side)), req.OrderType, activityLabel, duration.Milliseconds()))
		t.logger.Warn("trader: order request returned nil response", fields...)
		return nil, nil
	}
	resultFields := append(fields,
		zap.Bool("success", resp.Success),
		zap.String("status", resp.Status),
	)
	if resp.SignMs > 0 {
		resultFields = append(resultFields, zap.Int64("sign_ms", resp.SignMs))
	}
	if resp.HTTPMs > 0 {
		resultFields = append(resultFields, zap.Int64("http_ms", resp.HTTPMs))
	}
	if resp.HTTPDNSMs > 0 {
		resultFields = append(resultFields, zap.Int64("http_dns_ms", resp.HTTPDNSMs))
	}
	if resp.HTTPConnectMs > 0 {
		resultFields = append(resultFields, zap.Int64("http_connect_ms", resp.HTTPConnectMs))
	}
	if resp.HTTPTLSMs > 0 {
		resultFields = append(resultFields, zap.Int64("http_tls_ms", resp.HTTPTLSMs))
	}
	if resp.HTTPTTFBMs > 0 {
		resultFields = append(resultFields, zap.Int64("http_ttfb_ms", resp.HTTPTTFBMs))
	}
	if resp.Attempts > 0 {
		resultFields = append(resultFields, zap.Int("attempts", resp.Attempts))
	}
	if resp.OrderID != "" {
		resultFields = append(resultFields, zap.String("order_id", resp.OrderID))
	}
	if !resp.Success {
		resultFields = append(resultFields, zap.String("error", resp.ErrorMsg))
		logTimedActivityWarn(fmt.Sprintf("%s %s %s rejected in %d ms: status=%s error=%s", strings.ToUpper(string(req.Side)), req.OrderType, activityLabel, duration.Milliseconds(), resp.Status, resp.ErrorMsg))
		t.logger.Warn("trader: order request rejected", resultFields...)
		return resp, nil
	}
	logTimedActivityInfo(fmt.Sprintf("%s %s %s completed in %d ms at %s x %s", strings.ToUpper(string(req.Side)), req.OrderType, activityLabel, duration.Milliseconds(), req.Price, req.Size))
	t.logger.Info("trader: order request completed", resultFields...)
	return resp, nil
}

func (t *Trader) getOrderTimed(ctx context.Context, requestName, orderID string, extraFields ...zap.Field) (*polymarket.GetOrderResponse, error) {
	start := time.Now()
	resp, err := t.orders.GetOrder(ctx, orderID)
	duration := time.Since(start)
	if t.metrics != nil {
		t.metrics.Record("order_get", duration.Milliseconds())
	}
	activityLabel := activityRequestLabel(requestName)
	fields := []zap.Field{
		zap.String("request", requestName),
		zap.String("order_id", orderID),
		zap.Int64("request_ms", duration.Milliseconds()),
	}
	fields = append(fields, extraFields...)
	if err != nil {
		if isOrderLookupNotFoundError(err) {
			t.logger.Debug("trader: order lookup not found",
				append(fields, zap.Error(err))...,
			)
			return nil, nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "context canceled") {
			t.logger.Debug("trader: order lookup canceled", append(fields, zap.Error(err))...)
			return nil, err
		}
		logTimedActivityWarn(fmt.Sprintf("order lookup %s failed in %d ms: %v", activityLabel, duration.Milliseconds(), err))
		t.logger.Warn("trader: order lookup failed", append(fields, zap.Error(err))...)
		return nil, err
	}
	if resp == nil {
		logTimedActivityWarn(fmt.Sprintf("order lookup %s returned nil response in %d ms", activityLabel, duration.Milliseconds()))
		t.logger.Warn("trader: order lookup returned nil response", fields...)
		return nil, nil
	}
	resultFields := append(fields,
		zap.String("status", resp.Status),
	)
	if resp.SizeMatched != "" {
		resultFields = append(resultFields, zap.String("size_matched", resp.SizeMatched))
	}
	if requestName == "conviction_sell_lookup" && strings.EqualFold(resp.Status, "LIVE") {
		t.logger.Debug("trader: order lookup completed", resultFields...)
		return resp, nil
	}
	t.logger.Debug("trader: order lookup completed", resultFields...)
	return resp, nil
}

func (t *Trader) cancelOrderTimed(ctx context.Context, requestName, orderID string, extraFields ...zap.Field) error {
	start := time.Now()
	err := t.orders.CancelOrder(ctx, orderID)
	duration := time.Since(start)
	if t.metrics != nil {
		t.metrics.Record("order_cancel", duration.Milliseconds())
	}
	activityLabel := activityRequestLabel(requestName)
	fields := []zap.Field{
		zap.String("request", requestName),
		zap.String("order_id", orderID),
		zap.Int64("request_ms", duration.Milliseconds()),
	}
	fields = append(fields, extraFields...)
	if err != nil {
		logTimedActivityWarn(fmt.Sprintf("cancel %s failed in %d ms: %v", activityLabel, duration.Milliseconds(), err))
		t.logger.Warn("trader: cancel request failed", append(fields, zap.Error(err))...)
		return err
	}
	logTimedActivityInfo(fmt.Sprintf("cancel %s completed in %d ms", activityLabel, duration.Milliseconds()))
	t.logger.Info("trader: cancel request completed", fields...)
	return nil
}

func (t *Trader) getFillsTimed(ctx context.Context, requestName, orderID, tokenID string, extraFields ...zap.Field) (float64, float64, error) {
	start := time.Now()
	avgPrice, grossShares, err := t.orders.GetFillsByTakerOrderID(ctx, orderID, tokenID)
	duration := time.Since(start)
	if t.metrics != nil {
		t.metrics.Record("fills_get", duration.Milliseconds())
	}
	activityLabel := activityRequestLabel(requestName)
	fields := []zap.Field{
		zap.String("request", requestName),
		zap.String("order_id", orderID),
		zap.String("token_id", tokenID),
		zap.Int64("request_ms", duration.Milliseconds()),
	}
	fields = append(fields, extraFields...)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "context canceled") {
			t.logger.Debug("trader: fill lookup canceled", append(fields, zap.Error(err))...)
			return 0, 0, err
		}
		logTimedActivityWarn(fmt.Sprintf("fill lookup %s failed in %d ms: %v", activityLabel, duration.Milliseconds(), err))
		t.logger.Warn("trader: fill lookup failed", append(fields, zap.Error(err))...)
		return 0, 0, err
	}
	resultFields := append(fields,
		zap.Float64("avg_price", avgPrice),
		zap.Float64("gross_shares", grossShares),
	)
	t.logger.Debug("trader: fill lookup completed", resultFields...)
	return avgPrice, grossShares, nil
}

func (t *Trader) waitForGTCSellFillEvidence(ctx context.Context, orderID, tokenID string, expectedShares float64) (float64, float64, bool, error) {
	if orderID == "" {
		return 0, 0, false, nil
	}

	deadline := time.Now().Add(4 * time.Second)
	avgPrice := 0.0
	grossFilled := 0.0

	for {
		if err := ctx.Err(); err != nil {
			return avgPrice, grossFilled, false, err
		}

		complete := false
		if gr, grErr := t.getOrderTimed(ctx, "sell_gtc_poll", orderID); grErr == nil && gr != nil {
			matched := parseOrderSize(gr.SizeMatched)
			remaining := parseOrderSize(gr.SizeRemaining)
			status := strings.ToLower(strings.TrimSpace(gr.Status))
			if matched > grossFilled {
				grossFilled = matched
			}
			if matched > pairArbShareDust && remaining <= pairArbShareDust {
				complete = true
			}
			if status == "matched" || status == "filled" {
				if expectedShares <= 0 || matched >= expectedShares-pairArbShareDust {
					complete = true
				}
			}
		}

		if avg, gross, fillsErr := t.getFillsTimed(ctx, "sell_gtc_poll_fills", orderID, tokenID); fillsErr == nil {
			if gross > grossFilled {
				grossFilled = gross
			}
			if gross > pairArbShareDust && avg > 0 {
				avgPrice = avg
			}
			if expectedShares > 0 && gross >= expectedShares-pairArbShareDust {
				complete = true
			}
		}

		if complete {
			return avgPrice, grossFilled, true, nil
		}
		if time.Now().After(deadline) {
			if grossFilled > pairArbShareDust {
				return avgPrice, grossFilled, false, nil
			}
			return 0, 0, false, nil
		}

		select {
		case <-ctx.Done():
			return avgPrice, grossFilled, false, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (cfg TraderConfig) normalizedResolutionOrderType() polymarket.OrderType {
	if strings.EqualFold(string(cfg.ResolutionOrderType), string(polymarket.OrderTypeFAK)) {
		return polymarket.OrderTypeFAK
	}
	return polymarket.OrderTypeGTC
}

func (cfg TraderConfig) normalizedResolutionBuyLimitPrice() float64 {
	if cfg.ResolutionBuyLimitPrice <= 0 {
		return 0.999
	}
	if cfg.ResolutionBuyLimitPrice >= 1.0 {
		return 0.999
	}
	return math.Round(cfg.ResolutionBuyLimitPrice*1000) / 1000
}

func (cfg TraderConfig) usesManagedResolutionTickFlow() bool {
	return cfg.normalizedResolutionOrderType() == polymarket.OrderTypeGTC && cfg.ResolutionBuyLimitPrice >= 1.0
}

func formatResolutionLimitPrice(limitPrice float64) string {
	price := limitPrice
	if price <= 0 {
		price = 0.999
	}
	if price >= 1.0 {
		price = 0.999
	}
	return strconv.FormatFloat(price, 'f', 3, 64)
}

func normalizeFilledBuyPrice(price float64) float64 {
	if price <= 0 {
		return 0.01
	}
	if price > 1.0 {
		price = 1.0
	}
	return math.Round(price*1000) / 1000
}

func isResolutionClaimWaitLimit(limitPrice float64) bool {
	return math.Abs(limitPrice-0.99) < 0.0005
}

func (t *Trader) cacheTickSize(tokenID, tickSize string) {
	if tokenID == "" || tickSize == "" {
		return
	}
	t.tickSizeMu.Lock()
	t.tickSizeByToken[tokenID] = tickSizeSnapshot{TickSize: tickSize, FetchedAt: time.Now()}
	t.tickSizeMu.Unlock()
}

func (t *Trader) cachedTickSize(tokenID string, maxAge time.Duration) (string, bool) {
	t.tickSizeMu.RLock()
	snapshot, ok := t.tickSizeByToken[tokenID]
	t.tickSizeMu.RUnlock()
	if !ok || snapshot.TickSize == "" {
		return "", false
	}
	if maxAge > 0 && time.Since(snapshot.FetchedAt) > maxAge {
		return "", false
	}
	return snapshot.TickSize, true
}

func (t *Trader) fetchResolutionTickSizeWithCache(ctx context.Context, tokenID string, useCache bool) (string, error) {
	if useCache {
		if tickSize, ok := t.cachedTickSize(tokenID, 15*time.Second); ok {
			return tickSize, nil
		}
	}
	tickSize, err := t.orders.GetTickSize(ctx, tokenID)
	if err != nil {
		return "", err
	}
	if tickSize == "" {
		return "", fmt.Errorf("empty tick size")
	}
	t.cacheTickSize(tokenID, tickSize)
	return tickSize, nil
}

func (t *Trader) fetchResolutionTickSize(ctx context.Context, tokenID string) (string, error) {
	return t.fetchResolutionTickSizeWithCache(ctx, tokenID, true)
}

func (t *Trader) refreshResolutionTickSize(ctx context.Context, tokenID string) (string, error) {
	return t.fetchResolutionTickSizeWithCache(ctx, tokenID, false)
}

func (t *Trader) inferredResolutionWinner() (string, string, bool) {
	btc, _, _, openPrice, _, _ := t.detector.Snapshot()
	if btc < openPrice {
		return t.convNoTokenID, "NO", true
	}
	return t.convYesTokenID, "YES", false
}

func (t *Trader) resolvedResolutionWinner(resolvedYes bool) (string, string, bool) {
	if resolvedYes {
		return t.convYesTokenID, "YES", false
	}
	return t.convNoTokenID, "NO", true
}

func (t *Trader) openResolutionPositionAfterFill(resp *polymarket.OrderResponse, tokenID string, isNoSide bool, fillPrice, actualShares, feeShares, usdSpent float64, windowEnd time.Time) {
	now := time.Now()
	t.position = &Position{
		OrderID:           resp.OrderID,
		TokenID:           tokenID,
		ConditionID:       t.convConditionID,
		StrategyName:      "resolution",
		IsNoSide:          isNoSide,
		BuyPrice:          fillPrice,
		Shares:            actualShares,
		USDSpent:          usdSpent,
		FeeShares:         feeShares,
		TargetPrice:       1.0,
		OpenPrice:         t.detector.OpenPrice(),
		WindowEnd:         windowEnd,
		OpenedAt:          now,
		ExpiresAt:         windowEnd,
		IsResolutionSnipe: true,
	}
	t.rememberBotConditionID(t.convConditionID)
	t.pendingBuy = nil
	t.detector.SetInPosition(true)
	t.savePositionState()
}

func (t *Trader) attemptPreCloseResolutionEntry(ctx context.Context, tokenID, label, tickSize string, isNoSide bool, windowEnd time.Time, resolveWinningSide func(context.Context) (bool, error)) bool {
	if t.cfg.normalizedResolutionOrderType() != polymarket.OrderTypeGTC {
		return false
	}
	if !t.IsFlat() || t.buyInProgress {
		display.Warn(fmt.Sprintf("resolution pre-close entry skipped for %s: trader already busy", label))
		return true
	}
	t.buyInProgress = true
	defer func() { t.buyInProgress = false }()
	limitPrice := polymarket.NormalizePriceToTick(t.cfg.normalizedResolutionBuyLimitPrice(), tickSize)
	if limitPrice <= 0 {
		display.Warn(fmt.Sprintf("resolution pre-close entry skipped for %s: invalid configured limit", label))
		return false
	}
	tradeSize := t.cfg.ResolutionTradeSizeUSD
	if tradeSize <= 0 {
		tradeSize = t.cfg.TradeSizeUSD
	}
	fillEstimate := limitPrice
	rawShares := math.Round((tradeSize/limitPrice)*100) / 100
	feeShares := polymarket.ComputeBuyFeeShares(rawShares, fillEstimate, t.feeRateBps)
	actualShares := rawShares - feeShares
	if actualShares < polymarket.MinOrderShares {
		feeRate := 0.0
		if bps, err := strconv.ParseFloat(t.feeRateBps, 64); err == nil && bps > 0 {
			feeRate = bps / 10000.0
		}
		minRaw := polymarket.MinOrderShares / (1.0 - feeRate*(1.0-fillEstimate))
		rawShares = math.Ceil(minRaw*100) / 100
		feeShares = polymarket.ComputeBuyFeeShares(rawShares, fillEstimate, t.feeRateBps)
		actualShares = rawShares - feeShares
	}
	actualShares = math.Floor(actualShares*100) / 100
	t.pendingBuy = &pendingBuyState{
		TokenID:      tokenID,
		IsNoSide:     isNoSide,
		FillPrice:    fillEstimate,
		RawShares:    rawShares,
		FeeShares:    feeShares,
		ActualShares: actualShares,
		TargetPrice:  1.0,
		FeeRateBps:   t.feeRateBps,
		WindowEnd:    windowEnd,
		ExpiresAt:    windowEnd.Add(60 * time.Second),
		PlacedAt:     time.Now(),
	}
	t.savePositionState()
	resp, fillPrice, actualShares, feeShares, err := t.placeResolutionGTCBuy(ctx, tokenID, tickSize, limitPrice, fillEstimate, rawShares, actualShares, feeShares, windowEnd.Add(60*time.Second))
	if err != nil {
		t.pendingBuy = nil
		t.savePositionState()
		display.Warn(fmt.Sprintf("resolution pre-close entry failed for %s at %s: %v", label, polymarket.FormatPriceForTick(limitPrice, tickSize), err))
		return false
	}
	if resp == nil || !resp.Success || actualShares <= 0 {
		t.pendingBuy = nil
		t.savePositionState()
		display.Warn(fmt.Sprintf("resolution pre-close entry rejected for %s at %s", label, polymarket.FormatPriceForTick(limitPrice, tickSize)))
		return false
	}
	if t.pendingBuy != nil {
		t.pendingBuy.OrderID = resp.OrderID
		t.pendingBuy.FillPrice = fillPrice
		t.pendingBuy.FeeShares = feeShares
		t.pendingBuy.ActualShares = actualShares
		t.savePositionState()
	}
	usdSpent := rawShares * limitPrice
	t.openResolutionPositionAfterFill(resp, tokenID, isNoSide, fillPrice, actualShares, feeShares, usdSpent, windowEnd)
	display.Info(fmt.Sprintf("resolution pre-close entry filled for %s at %.3f", label, fillPrice))
	if time.Now().After(windowEnd) {
		resolveCtx, resolveCancel := context.WithTimeout(context.Background(), 65*time.Second)
		resolvedYes, resolveErr := resolveWinningSide(resolveCtx)
		resolveCancel()
		if resolveErr != nil {
			display.Warn(fmt.Sprintf("resolution pre-close settlement fallback used for %s: %v", label, resolveErr))
		}
		t.SettleExpiredPosition(ctx, resolvedYes)
	}
	return true
}

func (t *Trader) attemptPostCloseResolutionEntry(ctx context.Context, tokenID, label, tickSize string, isNoSide bool, resolvedYes bool, windowEnd time.Time) {
	if tickSize != "0.001" {
		display.Info(fmt.Sprintf("resolution post-close entry skipped for %s: tick %s is not 0.001", label, tickSize))
		return
	}
	if !t.IsFlat() {
		display.Warn(fmt.Sprintf("resolution post-close entry skipped for %s: position already open", label))
		return
	}
	limitPrice := polymarket.NormalizePriceToTick(t.cfg.normalizedResolutionBuyLimitPrice(), tickSize)
	if limitPrice <= 0 {
		display.Warn(fmt.Sprintf("resolution post-close entry skipped for %s: invalid configured limit", label))
		return
	}
	tradeSize := t.cfg.ResolutionTradeSizeUSD
	if tradeSize <= 0 {
		tradeSize = t.cfg.TradeSizeUSD
	}
	fillEstimate := limitPrice
	rawShares := math.Round((tradeSize/limitPrice)*100) / 100
	feeShares := polymarket.ComputeBuyFeeShares(rawShares, fillEstimate, t.feeRateBps)
	actualShares := math.Floor((rawShares-feeShares)*100) / 100
	if actualShares < polymarket.MinOrderShares {
		feeRate := 0.0
		if bps, err := strconv.ParseFloat(t.feeRateBps, 64); err == nil && bps > 0 {
			feeRate = bps / 10000.0
		}
		minRaw := polymarket.MinOrderShares / (1.0 - feeRate*(1.0-fillEstimate))
		rawShares = math.Ceil(minRaw*100) / 100
		feeShares = polymarket.ComputeBuyFeeShares(rawShares, fillEstimate, t.feeRateBps)
		actualShares = math.Floor((rawShares-feeShares)*100) / 100
	}
	resp, fillPrice, actualShares, feeShares, err := t.placeResolutionGTCBuy(ctx, tokenID, tickSize, limitPrice, fillEstimate, rawShares, actualShares, feeShares, time.Now().Add(15*time.Second))
	if err != nil {
		display.Warn(fmt.Sprintf("resolution post-close entry failed for %s at %s: %v", label, polymarket.FormatPriceForTick(limitPrice, tickSize), err))
		return
	}
	if resp == nil || !resp.Success || actualShares <= 0 {
		display.Warn(fmt.Sprintf("resolution post-close entry rejected for %s at %s", label, polymarket.FormatPriceForTick(limitPrice, tickSize)))
		return
	}
	now := time.Now()
	t.position = &Position{
		OrderID:           resp.OrderID,
		TokenID:           tokenID,
		ConditionID:       t.convConditionID,
		StrategyName:      "resolution",
		IsNoSide:          isNoSide,
		BuyPrice:          fillPrice,
		Shares:            actualShares,
		USDSpent:          rawShares * limitPrice,
		FeeShares:         feeShares,
		TargetPrice:       1.0,
		OpenPrice:         t.detector.OpenPrice(),
		WindowEnd:         windowEnd,
		OpenedAt:          now,
		ExpiresAt:         now,
		IsResolutionSnipe: true,
	}
	t.rememberBotConditionID(t.convConditionID)
	t.detector.SetInPosition(true)
	t.savePositionState()
	display.Info(fmt.Sprintf("resolution post-close entry filled for %s at %.3f, queueing claim", label, fillPrice))
	t.SettleExpiredPosition(ctx, resolvedYes)
}

func (t *Trader) runResolutionTickValidation(windowEnd time.Time, resolveWinningSide func(context.Context) (bool, error), shutdownCh <-chan struct{}) {
	waitUntil := func(deadline time.Time) bool {
		if !time.Now().Before(deadline) {
			return true
		}
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		select {
		case <-shutdownCh:
			return false
		case <-timer.C:
			return true
		}
	}
	preTokenID, preLabel, _ := t.inferredResolutionWinner()
	if preTokenID == "" {
		return
	}
	if !waitUntil(windowEnd.Add(-5 * time.Second)) {
		return
	}
	preCtx, preCancel := context.WithTimeout(context.Background(), 10*time.Second)
	preTick, preErr := t.refreshResolutionTickSize(preCtx, preTokenID)
	preCancel()
	if preErr != nil {
		display.Warn(fmt.Sprintf("resolution tick pre-close fetch failed for %s: %v", preLabel, preErr))
	} else {
		display.Info(fmt.Sprintf("resolution tick pre-close for %s: %s", preLabel, preTick))
		if !t.cfg.PaperTrade && preTick == "0.001" {
			if t.attemptPreCloseResolutionEntry(context.Background(), preTokenID, preLabel, preTick, preLabel == "NO", windowEnd, resolveWinningSide) {
				return
			}
		}
	}
	if !waitUntil(windowEnd) {
		return
	}
	resolveCtx, resolveCancel := context.WithTimeout(context.Background(), 65*time.Second)
	resolvedYes, resolveErr := resolveWinningSide(resolveCtx)
	resolveCancel()
	winningTokenID, winningLabel, winningIsNo := t.resolvedResolutionWinner(resolvedYes)
	if winningTokenID == "" {
		return
	}
	if resolveErr != nil {
		display.Warn(fmt.Sprintf("resolution winner confirmation fallback used for %s: %v", winningLabel, resolveErr))
	}
	if preLabel != winningLabel {
		display.Warn(fmt.Sprintf("resolution winner changed from %s pre-close to %s post-close", preLabel, winningLabel))
	}
	deadline := windowEnd.Add(60 * time.Second)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	lastTick := ""
	entryAttempted := false
	for {
		postCtx, postCancel := context.WithTimeout(context.Background(), 10*time.Second)
		currentTick, tickErr := t.refreshResolutionTickSize(postCtx, winningTokenID)
		postCancel()
		if tickErr != nil {
			display.Warn(fmt.Sprintf("resolution tick post-close fetch failed for %s: %v", winningLabel, tickErr))
		} else {
			if currentTick != lastTick {
				display.Info(fmt.Sprintf("resolution tick changed for %s: %s -> %s", winningLabel, lastTick, currentTick))
				lastTick = currentTick
			} else if currentTick != "" {
				display.Info(fmt.Sprintf("resolution tick post-close for %s: %s", winningLabel, currentTick))
			}
			if !entryAttempted && !t.cfg.PaperTrade && currentTick == "0.001" {
				entryCtx, entryCancel := context.WithTimeout(context.Background(), 20*time.Second)
				t.attemptPostCloseResolutionEntry(entryCtx, winningTokenID, winningLabel, currentTick, winningIsNo, resolvedYes, windowEnd)
				entryCancel()
				entryAttempted = true
				return
			}
		}
		if !time.Now().Before(deadline) {
			if !entryAttempted {
				display.Warn(fmt.Sprintf("resolution post-close entry not attempted for %s within 60s", winningLabel))
			}
			return
		}
		select {
		case <-shutdownCh:
			return
		case <-ticker.C:
		}
	}
}

func (t *Trader) StartResolutionTickValidation(windowEnd time.Time, resolveWinningSide func(context.Context) (bool, error), shutdownCh <-chan struct{}) {
	if !t.cfg.usesManagedResolutionTickFlow() {
		return
	}
	go t.runResolutionTickValidation(windowEnd, resolveWinningSide, shutdownCh)
}

func applyFAKBuyFill(resp *polymarket.OrderResponse, fillPrice, grossShares float64) (float64, float64, error) {
	if resp == nil {
		return 0, 0, fmt.Errorf("missing order response")
	}
	if !strings.EqualFold(resp.Status, "matched") {
		return 0, 0, fmt.Errorf("FAK buy not filled")
	}
	updatedFillPrice := fillPrice
	updatedGrossShares := grossShares
	if resp.TakingAmount != "" && resp.MakingAmount != "" {
		takingRaw, okT := new(big.Int).SetString(resp.TakingAmount, 10)
		makingRaw, okM := new(big.Int).SetString(resp.MakingAmount, 10)
		if okT && okM && takingRaw.Sign() > 0 && makingRaw.Sign() > 0 {
			actualRat := new(big.Rat).SetFrac(takingRaw, big.NewInt(1_000_000))
			if actualFill, _ := actualRat.Float64(); actualFill > 0 {
				updatedGrossShares = math.Floor(actualFill*100) / 100
			}
			priceRat := new(big.Rat).SetFrac(makingRaw, takingRaw)
			if actualPrice, _ := priceRat.Float64(); actualPrice > 0 && actualPrice <= 1.0 {
				updatedFillPrice = normalizeFilledBuyPrice(actualPrice)
			}
		}
	} else if resp.TakingAmount != "" {
		if takingRaw, ok := new(big.Int).SetString(resp.TakingAmount, 10); ok && takingRaw.Sign() > 0 {
			actualRat := new(big.Rat).SetFrac(takingRaw, big.NewInt(1_000_000))
			if actualFill, _ := actualRat.Float64(); actualFill > 0 {
				updatedGrossShares = math.Floor(actualFill*100) / 100
			}
		}
	}
	if updatedGrossShares <= 0 {
		return 0, 0, fmt.Errorf("FAK buy filled zero shares")
	}
	return updatedFillPrice, updatedGrossShares, nil
}

func (t *Trader) placeResolutionFAKBuy(ctx context.Context, tokenID, tickSize string, limitPrice, signalPrice, rawShares, estimatedShares, estimatedFeeShares float64) (*polymarket.OrderResponse, float64, float64, float64, error) {
	priceStr := polymarket.FormatPriceForTick(limitPrice, tickSize)
	sharesStr := fmt.Sprintf("%.2f", rawShares)
	resp, err := t.placeOrderTimed(ctx, "resolution_buy_fak", &polymarket.NewOrderRequest{
		OrderType:  polymarket.OrderTypeFAK,
		TokenID:    tokenID,
		Side:       polymarket.SideBuy,
		Price:      priceStr,
		Size:       sharesStr,
		TickSize:   tickSize,
		Nonce:      polymarket.MakeNonce(),
		FeeRateBps: t.feeRateBps,
	})
	if err != nil {
		return nil, 0, 0, 0, err
	}
	if !resp.Success {
		return nil, 0, 0, 0, fmt.Errorf("order rejected: %s", resp.ErrorMsg)
	}
	grossShares, netShares := estimatedShares+estimatedFeeShares, estimatedShares
	fillPrice := signalPrice
	fillPrice, grossShares, err = applyFAKBuyFill(resp, signalPrice, grossShares)
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("resolution: %w", err)
	}
	feeShares := estimatedFeeShares
	if grossShares > 0 {
		if computedFee := polymarket.ComputeBuyFeeShares(grossShares, fillPrice, t.feeRateBps); computedFee >= 0 {
			feeShares = computedFee
		}
		netShares = math.Floor((grossShares-feeShares)*100) / 100
	}
	if netShares <= 0 {
		return nil, 0, 0, 0, fmt.Errorf("resolution: FAK buy filled zero net shares")
	}
	return resp, fillPrice, netShares, feeShares, nil
}

func (t *Trader) placeResolutionGTCBuy(ctx context.Context, tokenID, tickSize string, limitPrice, signalPrice, rawShares, estimatedShares, estimatedFeeShares float64, windowEnd time.Time) (*polymarket.OrderResponse, float64, float64, float64, error) {
	priceStr := polymarket.FormatPriceForTick(limitPrice, tickSize)
	sharesStr := fmt.Sprintf("%.2f", rawShares)
	resp, err := t.placeOrderTimed(ctx, "resolution_buy_gtc", &polymarket.NewOrderRequest{
		OrderType:  polymarket.OrderTypeGTC,
		TokenID:    tokenID,
		Side:       polymarket.SideBuy,
		Price:      priceStr,
		Size:       sharesStr,
		TickSize:   tickSize,
		Nonce:      polymarket.MakeNonce(),
		FeeRateBps: t.feeRateBps,
	})
	if err != nil {
		return nil, 0, 0, 0, err
	}
	if !resp.Success {
		return nil, 0, 0, 0, fmt.Errorf("order rejected: %s", resp.ErrorMsg)
	}

	fillPrice := normalizeFilledBuyPrice(signalPrice)
	actualShares := estimatedShares
	feeShares := estimatedFeeShares
	matchedShares := 0.0
	lastStatus := resp.Status
	pollDeadline := windowEnd.Add(2 * time.Second)
	if isResolutionClaimWaitLimit(limitPrice) {
		pollDeadline = windowEnd.Add(30 * time.Second)
	}
	minimumPollDeadline := time.Now().Add(4 * time.Second)
	if pollDeadline.Before(minimumPollDeadline) {
		pollDeadline = minimumPollDeadline
	}

	applyFill := func(avgPrice, grossShares float64) {
		if avgPrice > 0 {
			fillPrice = normalizeFilledBuyPrice(avgPrice)
		}
		if grossShares > 0 {
			matchedShares = grossShares
			feeShares = polymarket.ComputeBuyFeeShares(grossShares, fillPrice, t.feeRateBps)
			actualShares = math.Floor((grossShares-feeShares)*100) / 100
		}
	}

	for {
		var statusMatched bool
		var avgPrice float64
		var grossShares float64

		gr, grErr := t.getOrderTimed(ctx, "resolution_buy_lookup", resp.OrderID)
		if grErr == nil {
			lastStatus = gr.Status
			if prc, prcErr := strconv.ParseFloat(gr.Price, 64); prcErr == nil && prc > 0 && prc < 1.0 {
				avgPrice = prc
			}
			grossShares = parseOrderShares(gr.SizeMatched)
			if grossShares > 0 || avgPrice > 0 {
				applyFill(avgPrice, grossShares)
			}
			if strings.EqualFold(gr.Status, "MATCHED") || strings.EqualFold(gr.Status, "FILLED") {
				statusMatched = true
			}
		} else {
			avgPrice, grossShares, _ = t.getFillsTimed(ctx, "resolution_buy_fills", resp.OrderID, tokenID)
			if grossShares > 0 || avgPrice > 0 {
				applyFill(avgPrice, grossShares)
				// If the order no longer exists but trades do, treat it as fully matched.
				statusMatched = true
				lastStatus = "MATCHED"
			}
		}

		if statusMatched && actualShares > 0 {
			return resp, fillPrice, actualShares, feeShares, nil
		}

		if time.Now().After(pollDeadline) {
			if matchedShares > 0 {
				if cancelErr := t.cancelOrderTimed(ctx, "resolution_buy_cancel_partial", resp.OrderID); cancelErr != nil && !errors.Is(cancelErr, polymarket.ErrOrderNotCancellable) {
					t.logger.Warn("trader: resolution GTC buy partial fill cancel failed",
						zap.String("order_id", resp.OrderID),
						zap.Error(cancelErr),
					)
				}
				t.logger.Info("trader: resolution GTC buy partially filled before market close",
					zap.String("order_id", resp.OrderID),
					zap.Float64("fill_price", fillPrice),
					zap.Float64("shares_owned", actualShares),
				)
				return resp, fillPrice, actualShares, feeShares, nil
			}
			if cancelErr := t.cancelOrderTimed(ctx, "resolution_buy_cancel_unfilled", resp.OrderID); cancelErr != nil && !errors.Is(cancelErr, polymarket.ErrOrderNotCancellable) {
				t.logger.Warn("trader: resolution GTC buy cancel failed",
					zap.String("order_id", resp.OrderID),
					zap.Error(cancelErr),
				)
			}
			deadlineLabel := "market close"
			if isResolutionClaimWaitLimit(limitPrice) {
				deadlineLabel = "claim wait deadline"
			}
			return nil, 0, 0, 0, fmt.Errorf("resolution: GTC buy not filled before %s (status=%s)", deadlineLabel, lastStatus)
		}

		select {
		case <-ctx.Done():
			return nil, 0, 0, 0, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func journalStrategyForSignal(sig Signal) string {
	switch sig.Type {
	case SignalReversalSnipeYes, SignalReversalSnipeNo:
		return "reversal_snipe"
	case SignalCollapseSnipeYes, SignalCollapseSnipeNo:
		return "collapse"
	case SignalDeepDiscountFadeYes, SignalDeepDiscountFadeNo:
		return "deep_discount_fade"
	case SignalMidFlipYes, SignalMidFlipNo:
		return "mid_flip"
	case SignalLateFlipYes, SignalLateFlipNo:
		return "late_flip"
	case SignalScalpYes, SignalScalpNo:
		return "scalp"
	case SignalPennyBuyYes, SignalPennyBuyNo:
		return "penny_buy"
	case SignalResolutionEarlyBuyYes, SignalResolutionEarlyBuyNo:
		return "early_resolution"
	case SignalResolutionBuyYes, SignalResolutionBuyNo:
		return "resolution"
	case SignalConvictionBuyYes, SignalConvictionBuyNo:
		return "conviction"
	default:
		return "lag"
	}
}

func journalStrategyForPosition(pos *Position) string {
	if pos == nil {
		return ""
	}
	if pos.StrategyName != "" {
		return pos.StrategyName
	}
	if pos.IsResolutionSnipe {
		if pos.IsEarlyResolution {
			return "early_resolution"
		}
		return "resolution"
	}
	if pos.IsPennyBuy {
		return "penny_buy"
	}
	if pos.IsScalp {
		return "scalp"
	}
	if pos.IsConviction {
		return "conviction"
	}
	return "lag"
}

func isCleanupTradeReason(reason string) bool {
	return strings.Contains(reason, "stale_abandoned") || reason == "window_resolved" || reason == "window_resolved_claim_pending" || reason == "post_close_grace_sell"
}

func mergeTradeRecords(base, extra TradeRecord) TradeRecord {
	return MergeTradeRecords(base, extra)
}

// MergeTradeRecords combines two TradeRecord entries into one, accumulating
// shares, costs, PnL, and picking the latest timestamps. Used to collapse
// multi-leg pair-arb journal entries into a single consolidated record.
func MergeTradeRecords(base, extra TradeRecord) TradeRecord {
	merged := base
	if extra.ClosedAt.After(merged.ClosedAt) {
		merged.ClosedAt = extra.ClosedAt
	}
	if extra.HeldSec > merged.HeldSec {
		merged.HeldSec = extra.HeldSec
	}
	if merged.Strategy == "" || merged.Strategy == "conviction" || merged.Strategy == "lag" {
		if extra.Strategy != "" && extra.Strategy != merged.Strategy {
			merged.Strategy = extra.Strategy
		}
	}
	if merged.Side == "" {
		merged.Side = extra.Side
	}
	// Normalise pair-arb side: always show "PAIR" in consolidated records.
	if merged.Strategy == "pair_arb" {
		merged.Side = "PAIR"
	}
	totalSpent := base.USDSpent + extra.USDSpent
	totalShares := base.Shares + extra.Shares
	if totalSpent > 0 {
		merged.BuyPrice = ((base.BuyPrice * base.USDSpent) + (extra.BuyPrice * extra.USDSpent)) / totalSpent
	}
	if totalShares > 0 {
		merged.SellPrice = ((base.SellPrice * base.Shares) + (extra.SellPrice * extra.Shares)) / totalShares
	}
	merged.Shares = totalShares
	merged.USDSpent = totalSpent
	merged.PnL = base.PnL + extra.PnL
	if merged.EntryBTCPrice == 0 {
		merged.EntryBTCPrice = extra.EntryBTCPrice
	}
	if merged.EntryEdgeUSD == 0 {
		merged.EntryEdgeUSD = extra.EntryEdgeUSD
	}
	if merged.EntryCLLagUSD == 0 {
		merged.EntryCLLagUSD = extra.EntryCLLagUSD
	}
	if merged.EntryATR == 0 {
		merged.EntryATR = extra.EntryATR
	}
	if merged.EntryWindowRemSec == 0 {
		merged.EntryWindowRemSec = extra.EntryWindowRemSec
	}
	if merged.EntryOpenPrice == 0 {
		merged.EntryOpenPrice = extra.EntryOpenPrice
	}
	if merged.EntryWinProb == 0 {
		merged.EntryWinProb = extra.EntryWinProb
	}
	if isCleanupTradeReason(merged.Reason) && !isCleanupTradeReason(extra.Reason) {
		merged.Reason = extra.Reason
	}
	if merged.Reason == "" || (isCleanupTradeReason(extra.Reason) && extra.ClosedAt.After(base.ClosedAt) && isCleanupTradeReason(merged.Reason)) {
		merged.Reason = extra.Reason
	}
	return merged
}

func CollapseTradeRecords(records []TradeRecord) []TradeRecord {
	if len(records) < 2 {
		out := make([]TradeRecord, len(records))
		copy(out, records)
		return out
	}
	type tradeKey struct {
		openedAt int64
		side     string
	}
	collapsed := make([]TradeRecord, 0, len(records))
	indices := make(map[tradeKey]int, len(records))
	for _, rec := range records {
		// Pair-arb positions emit multiple records (PAIR_MATCHED + PAIR_RESIDUAL_*),
		// all with the same OpenedAt. Key only on openedAt so all legs are merged
		// into one record Ã¢â‚¬â€ win/loss is then counted on the combined net PnL.
		side := rec.Side
		if rec.Strategy == "pair_arb" {
			side = "pair_arb"
		}
		key := tradeKey{openedAt: rec.OpenedAt.UnixNano(), side: side}
		if idx, ok := indices[key]; ok {
			collapsed[idx] = mergeTradeRecords(collapsed[idx], rec)
			continue
		}
		indices[key] = len(collapsed)
		collapsed = append(collapsed, rec)
	}
	return collapsed
}

// OrderExecutor is the exchange-execution surface used by Trader.
//
// The strategy continues using the original Polymarket-compatible request and
// response shapes. Exchange adapters translate those logical operations to the
// venue-specific API underneath.
type OrderExecutor interface {
	PlaceOrder(context.Context, *polymarket.NewOrderRequest) (*polymarket.OrderResponse, error)
	CancelOrder(context.Context, string) error
	GetOrder(context.Context, string) (*polymarket.GetOrderResponse, error)
	GetFillsByTakerOrderID(context.Context, string, string) (float64, float64, error)
	GetTickSize(context.Context, string) (string, error)
	FindRecentOrder(context.Context, string) (*polymarket.GetOrderResponse, error)
	FindRecentTrade(context.Context, string) (*polymarket.GetOrderResponse, error)
	FetchCurrentPositions(context.Context, int, int) ([]polymarket.UserPosition, error)
	FetchUSDCBalance(context.Context) (float64, error)
	ResolveMarket(context.Context, string) (bool, bool, error)

	// Legacy Polymarket settlement/allowance methods.
	// Kalshi adapter will explicitly reject these; Kalshi does not use CTF,
	// Polygon token allowances, or claim redemption.
	FetchConditionalBalanceAllowance(context.Context, string) (*polymarket.BalanceAllowanceResponse, error)
	HasCTFBalance(context.Context, string) (bool, error)
	RedeemCTFPosition(context.Context, string, bool) error
	TriggerConditionalApproval(context.Context, string, time.Duration) error
	WaitForConditionalAllowance(context.Context, string, time.Duration) error
	WaitForSettledBalance(context.Context, string, float64, time.Duration) error
}

// Trader executes trades and monitors open positions.
type Trader struct {
	cfg       TraderConfig
	orders    OrderExecutor
	detector  *Detector
	logger    *zap.Logger
	stateFile string // path to JSON state file for crash recovery

	position       *Position // nil when flat
	pairedPosition *PairArbPosition
	pendingBuy     *pendingBuyState // non-nil between PlaceOrder and position-open
	pendingPairArb *pairArbPendingOrderState
	// pendingHedgePrePlace tracks a hedge GTC order placed simultaneously with the
	// lead buy (dual pre-place mode). It may arrive before or after the lead fill.
	pendingHedgePrePlace *pairArbPendingOrderState

	paperBalance float64 // simulated USD balance (paper mode only)

	feeRateBps string // taker fee for current window's token (e.g. "175")

	// buyInProgress is true while a buy attempt is in-flight (PlaceOrder Ã‚Â  fill poll).
	// Prevents concurrent or rapid-fire re-entry from multiple eval ticks.
	buyInProgress bool

	// convYesTokenID / convNoTokenID / convConditionID are the market identifiers for the current window.
	// Set via SetMarketTokens at window start; used by flipConvictionPosition and CTF redemption.
	convYesTokenID  string
	convNoTokenID   string
	convConditionID string

	journal        []TradeRecord // completed trades this window
	sessionJournal []TradeRecord // ALL completed trades across the session
	windowsPlayed  int           // number of windows completed
	startedAt      time.Time     // session start time

	//  Session-level risk counters 	// Updated inside appendJournalLine on every completed trade.
	sessionPnL        float64 // cumulative P&L since the session started
	totalTrades       int     // total trades completed this session
	consecutiveLosses int     // how many trades in a row closed at a loss

	// convictionStopLossAtomicCents holds the live-tuned conviction stop-loss
	// in cents as a uint64 (math.Float64bits encoding) for safe concurrent
	// writes from the auto-tuner goroutine and reads from the trading loop.
	convictionStopLossAtomicCents uint64

	// allowanceInFlight tracks token IDs that have an active WaitForConditionalAllowance
	// goroutine running. Prevents multiple concurrent approval goroutines hammering
	// /balance-allowance and triggering HTTP 429 rate limits.
	allowanceInFlight           sync.Map // key: tokenID (string) Ã‚Â  struct{}{}
	claimInFlight               sync.Map // key: conditionID:side (string) -> struct{}{}
	pairClaimSafetyScheduled    sync.Map // key: conditionID:windowEndUnix -> struct{}{}
	claimMu                     sync.Mutex
	claimLastStatus             string
	claimLastMessage            string
	claimLastConditionID        string
	claimLastSide               string
	claimLastUpdatedAt          time.Time
	claimLastAttempt            int
	claimLastNextRetryAt        time.Time
	pairExitMu                  sync.Mutex
	pairExitManageInFlight      bool
	pairExitStatusMu            sync.Mutex
	pairExitStatusRefreshActive bool
	pairExitOrderStatus         map[string]pairExitOrderStatusSnapshot
	pairLeadEntryMu             sync.Mutex
	pairLeadEntryInFlight       bool
	pairRebalanceMu             sync.Mutex
	pairRebalanceInFlight       bool
	pendingClaims               []pendingClaim
	tickSizeMu                  sync.RWMutex
	tickSizeByToken             map[string]tickSizeSnapshot
	pairArbRetryAfter           time.Time
	pairArbForceCloseRetryAfter time.Time
	// lastPairArbCloseAt is stamped whenever a pair arb position is legitimately closed.
	// Used to suppress the orphan-liquidation trigger when the wallet API still shows
	// the just-sold shares (settlement / indexing lag).
	lastPairArbCloseAt time.Time
	// lastPairArbCloseConditionID is the condition ID tied to lastPairArbCloseAt.
	// Used to scope short post-close re-entry holds to the same market only.
	lastPairArbCloseConditionID string
	// pairArbHedgeOrphanFlag is set when forceClosePairArb fires while a pre-placed hedge
	// order was still tracked (pendingHedgePrePlace != nil). In that case the hedge may
	// have matched and its tokens are in the wallet without a tracked position. This flag
	// bypasses the orphan-cooldown suppression so liquidation runs immediately.
	pairArbHedgeOrphanFlag bool
	// lastPairArbStopLossAt is stamped when a stop-loss exit completes. Used to enforce
	// PairArbStopCooldownSec so the bot cannot immediately re-enter the same (or any)
	// market before conditional allowances are back on-chain.
	lastPairArbStopLossAt time.Time
	// pairArbLeadGapGuardFn, when non-nil, is evaluated on each GTC poll tick while the
	// lead order is resting LIVE. Returning (true, reason) cancels the order immediately
	// rather than waiting for the full timeout. Set by OnPairArbSignal before executing
	// the lead buy and cleared (via defer) when OnPairArbSignal returns.
	pairArbLeadGapGuardFn func() (shouldCancel bool, reason string)

	// botConditionIDs tracks condition IDs this bot has actually traded/managed.
	// External reconciliation is restricted to this set.
	botConditionIDs map[string]struct{}

	// cachedLiveBalance holds the most recently fetched on-chain USDC balance
	// (math.Float64bits encoding). Written by RefreshLiveBalance at window start
	// and refreshed in the background every 5 s. Zero means unknown/unset.
	cachedLiveBalance uint64
	pairCostReconcile sync.Mutex
	lastPairCostProbe time.Time
	pendingOrderWatch sync.Map // key: watch kind + orderID + tokenID -> struct{}{}
	isolatedPendingMu sync.Mutex
	isolatedPending   []pairArbPendingOrderState

	// OnTradeClose, if non-nil, is called immediately after each completed trade
	// record is written to disk. Set from main to feed the web dashboard in real-time
	// without creating an import cycle.
	OnTradeClose func(TradeRecord)

	// metrics, if non-nil, collects rolling latency stats exposed on the web metrics page.
	metrics *BotMetrics
}

// SetMetrics attaches a rolling latency metrics collector.
func (t *Trader) SetMetrics(m *BotMetrics) { t.metrics = m }

// NewTrader creates a Trader.
func NewTrader(
	cfg TraderConfig,
	orders OrderExecutor,
	detector *Detector,
	logger *zap.Logger,
) *Trader {
	pb := 0.0
	if cfg.PaperTrade {
		pb = cfg.PaperStartBalance
	}
	return &Trader{
		cfg:                           cfg,
		orders:                        orders,
		detector:                      detector,
		logger:                        logger,
		stateFile:                     "position_state.json",
		paperBalance:                  pb,
		journal:                       make([]TradeRecord, 0, 8),
		sessionJournal:                make([]TradeRecord, 0, 32),
		startedAt:                     time.Now(),
		tickSizeByToken:               make(map[string]tickSizeSnapshot),
		pairExitOrderStatus:           make(map[string]pairExitOrderStatusSnapshot),
		botConditionIDs:               make(map[string]struct{}),
		convictionStopLossAtomicCents: math.Float64bits(cfg.ConvictionStopLossCents),
	}
}

func normalizeConditionID(conditionID string) string {
	return strings.ToLower(strings.TrimSpace(conditionID))
}

func (t *Trader) isTokenTrackedByOpenPosition(tokenID string) bool {
	tokenID = strings.TrimSpace(tokenID)
	if tokenID == "" {
		return false
	}
	if t.position != nil && strings.EqualFold(strings.TrimSpace(t.position.TokenID), tokenID) {
		return true
	}
	if t.pairedPosition != nil {
		if strings.EqualFold(strings.TrimSpace(t.pairedPosition.YesTokenID), tokenID) ||
			strings.EqualFold(strings.TrimSpace(t.pairedPosition.NoTokenID), tokenID) {
			return true
		}
	}
	return false
}

func (t *Trader) registerIsolatedPending(po pairArbPendingOrderState) {
	orderID := strings.TrimSpace(po.OrderID)
	tokenID := strings.TrimSpace(po.TokenID)
	if orderID == "" || tokenID == "" {
		return
	}
	po.OrderID = orderID
	po.TokenID = tokenID

	t.isolatedPendingMu.Lock()
	for _, existing := range t.isolatedPending {
		if strings.EqualFold(strings.TrimSpace(existing.OrderID), orderID) &&
			strings.EqualFold(strings.TrimSpace(existing.TokenID), tokenID) {
			t.isolatedPendingMu.Unlock()
			return
		}
	}
	t.isolatedPending = append(t.isolatedPending, po)
	t.isolatedPendingMu.Unlock()
	t.savePositionState()
}

func (t *Trader) unregisterIsolatedPending(orderID, tokenID string) {
	orderID = strings.TrimSpace(orderID)
	tokenID = strings.TrimSpace(tokenID)
	if orderID == "" || tokenID == "" {
		return
	}
	t.isolatedPendingMu.Lock()
	idx := -1
	for i := range t.isolatedPending {
		entry := t.isolatedPending[i]
		if strings.EqualFold(strings.TrimSpace(entry.OrderID), orderID) &&
			strings.EqualFold(strings.TrimSpace(entry.TokenID), tokenID) {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.isolatedPendingMu.Unlock()
		return
	}
	t.isolatedPending = append(t.isolatedPending[:idx], t.isolatedPending[idx+1:]...)
	t.isolatedPendingMu.Unlock()
	t.savePositionState()
}

func (t *Trader) watchIsolatedPendingOrder(po *pairArbPendingOrderState, reason string) {
	if t.cfg.PaperTrade || po == nil || strings.TrimSpace(po.OrderID) == "" || strings.TrimSpace(po.TokenID) == "" {
		return
	}
	orderID := strings.TrimSpace(po.OrderID)
	tokenID := strings.TrimSpace(po.TokenID)
	requestName := strings.TrimSpace(po.RequestName)
	placedAt := po.PlacedAt
	watchKey := fmt.Sprintf("isolated|%s|%s", strings.ToLower(orderID), strings.ToLower(tokenID))
	if _, loaded := t.pendingOrderWatch.LoadOrStore(watchKey, struct{}{}); loaded {
		return
	}
	t.registerIsolatedPending(pairArbPendingOrderState{
		OrderID:     orderID,
		TokenID:     tokenID,
		RequestName: requestName,
		Origin:      reason,
		PlacedAt:    placedAt,
	})
	go func() {
		defer t.pendingOrderWatch.Delete(watchKey)
		defer t.unregisterIsolatedPending(orderID, tokenID)

		watchBudget := pairArbIsolatedWatchTimeout
		if !placedAt.IsZero() {
			age := time.Since(placedAt)
			if age < 0 {
				age = 0
			}
			if age < watchBudget {
				watchBudget -= age
			} else {
				watchBudget = 2 * time.Minute
			}
		}
		if watchBudget < 2*time.Minute {
			watchBudget = 2 * time.Minute
		}

		ctx, cancel := context.WithTimeout(context.Background(), watchBudget)
		defer cancel()

		fallbackPrice := 0.50
		if avg, gross, err := t.getFillsTimed(ctx, requestName+"_isolated_watch_fills", orderID, tokenID); err == nil && gross > pairArbShareDust && avg > 0 {
			fallbackPrice = avg
		}

		t.logger.Warn("trader: started isolated pending-order ownership watch",
			zap.String("order_id", orderID),
			zap.String("token_id", tokenID),
			zap.String("request", requestName),
			zap.String("reason", reason),
			zap.Duration("watch_budget", watchBudget),
		)

		ticker := time.NewTicker(pairArbIsolatedWatchPoll)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				break
			}
			if t.isTokenTrackedByOpenPosition(tokenID) {
				t.logger.Info("trader: isolated pending-order watch stopping; token is now tracked by open position",
					zap.String("order_id", orderID),
					zap.String("token_id", tokenID),
				)
				return
			}
			bal, balErr := t.tokenBalanceShares(ctx, tokenID)
			if balErr == nil && bal > pairArbShareDust {
				sellable, sellableErr := t.resolveSellableShares(ctx, tokenID, bal)
				if sellableErr != nil {
					t.logger.Warn("trader: isolated pending-order watch saw credited shares but sellable lookup failed",
						zap.String("order_id", orderID),
						zap.String("token_id", tokenID),
						zap.Float64("wallet_balance", bal),
						zap.Error(sellableErr),
					)
				} else if sellable >= polymarket.MinOrderShares {
					fillPrice, filledShares, filled, sellErr := t.attemptSellWithFallback(ctx, tokenID, fallbackPrice, fmt.Sprintf("%.2f", sellable))
					if sellErr != nil {
						t.logger.Error("trader: isolated pending-order emergency liquidation failed",
							zap.String("order_id", orderID),
							zap.String("token_id", tokenID),
							zap.Float64("sellable", sellable),
							zap.Error(sellErr),
						)
					} else if filled {
						logTimedActivityWarn(fmt.Sprintf("ORPHAN RECOVERY sold %.2f shares from isolated pending order %s @ %.4f", filledShares, orderID, fillPrice))
						t.logger.Warn("trader: isolated pending-order credited later; emergency liquidation executed",
							zap.String("order_id", orderID),
							zap.String("token_id", tokenID),
							zap.Float64("wallet_balance", bal),
							zap.Float64("sellable", sellable),
							zap.Float64("filled_shares", filledShares),
							zap.Float64("fill_price", fillPrice),
						)
						return
					}
				} else {
					t.logger.Warn("trader: isolated pending-order credited below sell minimum; continuing watch",
						zap.String("order_id", orderID),
						zap.String("token_id", tokenID),
						zap.Float64("wallet_balance", bal),
						zap.Float64("sellable", sellable),
					)
				}
			}

			select {
			case <-ctx.Done():
				break
			case <-ticker.C:
			}
		}

		t.logger.Warn("trader: isolated pending-order ownership watch expired",
			zap.String("order_id", orderID),
			zap.String("token_id", tokenID),
			zap.String("request", requestName),
		)
	}()
}

func (t *Trader) asyncPairArbLeadSettleWatch(orderID, tokenID, requestName, requestID string) {
	if t.cfg.PaperTrade || strings.TrimSpace(orderID) == "" || strings.TrimSpace(tokenID) == "" {
		return
	}
	watchKey := fmt.Sprintf("lead_settle|%s|%s", strings.ToLower(strings.TrimSpace(orderID)), strings.ToLower(strings.TrimSpace(tokenID)))
	if _, loaded := t.pendingOrderWatch.LoadOrStore(watchKey, struct{}{}); loaded {
		return
	}
	go func() {
		defer t.pendingOrderWatch.Delete(watchKey)

		ctx, cancel := context.WithTimeout(context.Background(), pairArbHedgeSettleGrace)
		defer cancel()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			if t.pendingPairArb == nil || !strings.EqualFold(strings.TrimSpace(t.pendingPairArb.OrderID), strings.TrimSpace(orderID)) {
				return
			}
			bal, balErr := t.tokenBalanceShares(ctx, tokenID)
			if balErr == nil && bal > pairArbShareDust {
				reconcileCtx, reconcileCancel := context.WithTimeout(context.Background(), pairArbReconcileLookupTimeout)
				recErr := t.reconcilePendingPairArbOrder(reconcileCtx)
				reconcileCancel()
				if recErr != nil {
					t.logger.Warn("trader: async lead settle watch reconcile failed after wallet credit appeared",
						zap.String("request", requestName),
						zap.String("request_id", requestID),
						zap.String("order_id", orderID),
						zap.Float64("wallet_balance", bal),
						zap.Error(recErr),
					)
				}
				return
			}
			select {
			case <-ctx.Done():
				if t.pendingPairArb != nil && strings.EqualFold(strings.TrimSpace(t.pendingPairArb.OrderID), strings.TrimSpace(orderID)) {
					reconcileCtx, reconcileCancel := context.WithTimeout(context.Background(), pairArbReconcileLookupTimeout)
					recErr := t.reconcilePendingPairArbOrder(reconcileCtx)
					reconcileCancel()
					if recErr != nil {
						t.logger.Warn("trader: async lead settle watch timed out; reconcile still unresolved",
							zap.String("request", requestName),
							zap.String("request_id", requestID),
							zap.String("order_id", orderID),
							zap.Error(recErr),
						)
					}
				}
				return
			case <-ticker.C:
			}
		}
	}()
}

func (t *Trader) rememberBotConditionID(conditionID string) {
	key := normalizeConditionID(conditionID)
	if key == "" {
		return
	}
	t.botConditionIDs[key] = struct{}{}
}

func (t *Trader) isBotConditionID(conditionID string) bool {
	key := normalizeConditionID(conditionID)
	if key == "" {
		return false
	}
	_, ok := t.botConditionIDs[key]
	return ok
}

func parseExternalPositionEndDate(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if ts, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return ts
	}
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts
	}
	return time.Time{}
}

func normalizeOutcomeLabel(outcome string) string {
	n := strings.ToLower(strings.TrimSpace(outcome))
	n = strings.ReplaceAll(n, "_", " ")
	n = strings.ReplaceAll(n, "-", " ")
	n = strings.Join(strings.Fields(n), " ")
	return n
}

func isNoOutcomeLabel(outcome string) bool {
	n := normalizeOutcomeLabel(outcome)
	if n == "" {
		return false
	}
	if n == "no" || n == "down" || n == "false" || n == "under" || n == "below" || n == "bear" {
		return true
	}
	if strings.HasPrefix(n, "no ") || strings.HasSuffix(n, " no") {
		return true
	}
	return false
}

func inferNoSideFromOutcomes(outcome, opposite string, outcomeIndex int) (bool, bool) {
	if label := normalizeOutcomeLabel(outcome); label != "" {
		if isNoOutcomeLabel(label) {
			return true, true
		}
		if label == "yes" || label == "up" || label == "true" || label == "over" || label == "above" || label == "bull" {
			return false, true
		}
	}

	if opp := normalizeOutcomeLabel(opposite); opp != "" {
		if isNoOutcomeLabel(opp) {
			return false, true
		}
		if opp == "yes" || opp == "up" || opp == "true" || opp == "over" || opp == "above" || opp == "bull" {
			return true, true
		}
	}

	if outcomeIndex == 0 {
		return false, true
	}
	if outcomeIndex == 1 {
		return true, true
	}

	return false, false
}

// ReconcileExternalPositions queues CTF redemption claims for any redeemable
// open positions that the bot previously traded.
func (t *Trader) ReconcileExternalPositions(current []polymarket.UserPosition) int {
	if t.cfg.PaperTrade {
		return 0
	}
	claimsQueued := 0
	for _, pos := range current {
		if strings.TrimSpace(pos.ConditionID) == "" || pos.Size <= pairArbShareDust {
			continue
		}
		if !t.isBotConditionID(pos.ConditionID) {
			continue
		}
		if !pos.Redeemable {
			continue
		}
		isNo, ok := inferNoSideFromOutcomes(pos.Outcome, pos.OppositeOutcome, pos.OutcomeIndex)
		if !ok {
			t.logger.Warn("trader: external reconcile: skipping claim due to ambiguous outcome mapping",
				zap.String("condition_id", pos.ConditionID),
				zap.String("outcome", pos.Outcome),
				zap.String("opposite_outcome", pos.OppositeOutcome),
				zap.Int("outcome_index", pos.OutcomeIndex),
			)
			continue
		}
		claim := pendingClaim{
			ConditionID:  pos.ConditionID,
			StrategyName: "external_reconcile",
			IsNoSide:     isNo,
			Shares:       pos.Size,
			WindowEnd:    parseExternalPositionEndDate(pos.EndDate),
			Claimable:    pos.Redeemable,
			Status:       "queued_external_positions",
		}
		t.enqueueClaimRetry(claim)
		claimsQueued++
	}
	return claimsQueued
}

// RefreshLiveBalance fetches the current on-chain USDC balance and caches it
// so that OnPairArbSignal can do the pre-flight balance check without making
// a blocking RPC call in the hot path.  Call this once at window start (it is
// safe to call synchronously there) and then periodically in the background.
// No-op in paper-trade mode (paperBalance is tracked in-process).
func (t *Trader) RefreshLiveBalance(ctx context.Context) {
	if t.cfg.PaperTrade || t.orders == nil {
		return
	}
	bal, err := t.orders.FetchUSDCBalance(ctx)
	if err != nil {
		t.logger.Warn("pair arb: live balance refresh failed", zap.Error(err))
		return
	}
	atomic.StoreUint64(&t.cachedLiveBalance, math.Float64bits(bal))
	t.logger.Debug("pair arb: cached live balance refreshed", zap.Float64("usdc", bal))
}

const (
	claimRetryInterval            = 20 * time.Second
	claimRetryMediumInterval      = 1 * time.Minute
	claimRetrySlowInterval        = 5 * time.Minute
	convictionSellLookupInterval  = 2 * time.Second
	sellableBalancePollInterval   = 1 * time.Second
	pairArbExposureProbeInterval  = 350 * time.Millisecond
	pairArbOrderProbeInterval     = 300 * time.Millisecond
	pairArbSellRetryInterval      = 500 * time.Millisecond
	pairArbSellRetryWindow        = 12 * time.Second
	pairArbSellPollInterval       = 2 * time.Second
	pairArbExitStatusTTL          = 10 * time.Second
	pairArbExitStatusProbeTimeout = 1200 * time.Millisecond
	pairArbSellClaimGrace         = 45 * time.Second
	pairArbSellArmLead            = 10 * time.Second
	pairArbSellAggressiveLead     = 3 * time.Second
	ambiguousSubmitRecoverTimeout = 18 * time.Second
	ambiguousSubmitRecoverPoll    = 500 * time.Millisecond
	pairCostProbeInterval         = 15 * time.Second
	pairCostProbeSampleDelay      = 160 * time.Millisecond
	pairArbIsolatedWatchTimeout   = 12 * time.Minute
	pairArbIsolatedWatchPoll      = 2 * time.Second
)

const (
	pairExitStateIdle           = "idle"
	pairExitStatePlacingOrders  = "placing_orders"
	pairExitStateOrdersLive     = "orders_live"
	pairExitStateGraceWait      = "expiry_grace_wait"
	pairExitStateWinnerFilled   = "winner_filled"
	pairExitStateCancelingLoser = "canceling_loser"
	pairExitStateClaimQueued    = "claim_queued"
	pairExitStateResolvedCLOB   = "resolved_clob"
	pairExitStateResolvedClaim  = "resolved_claim"
	pairExitStateCleanup        = "stale_cleanup"
)

const (
	pairArbExposureProbeAttempts   = 6
	pairArbExposureZeroConfirmMin  = 2
	pairArbOrderProbeAttempts      = 8
	pairArbShareDust               = 0.01
	pairArbWalletReconcileDelta    = 0.02
	pairArbSettleBalanceDust       = 0.05
	pairArbUnprofitableAbortGrace  = 12 * time.Second
	pairArbUnprofitableAbortExcess = 0.02
	pairArbHedgeSettleGrace        = 90 * time.Second
	pairArbLeadCreditGrace         = 20 * time.Second
	pairArbOrphanRecoveryWatch     = pairArbLeadCreditGrace + 12*time.Second
	pairArbReconcileLookupTimeout  = 8 * time.Second
	// pairArbInventoryEntryBlock is the threshold above which residual wallet
	// inventory blocks a new lead entry. Small partial-fill remnants (e.g. 0.05
	// shares left after a GTC residual sell) are acceptable and should not gate
	// the next signal.
	pairArbInventoryEntryBlock = 0.25
)

func (t *Trader) setPendingPairArbOrder(orderID, tokenID, requestName string) {
	if t.cfg.PaperTrade || orderID == "" || tokenID == "" {
		return
	}
	t.pendingPairArb = &pairArbPendingOrderState{
		OrderID:     orderID,
		TokenID:     tokenID,
		RequestName: requestName,
		Origin:      "runtime_submit",
		PlacedAt:    time.Now(),
	}
	t.savePositionState()
}

func (t *Trader) clearPendingPairArbOrder() {
	if t.pendingPairArb == nil {
		return
	}
	t.pendingPairArb = nil
	t.savePositionState()
}

func (t *Trader) setPendingPairArbContext(orderID, conditionID, yesTokenID, noTokenID, leadSide string, windowEnd time.Time) {
	if t.cfg.PaperTrade || t.pendingPairArb == nil || strings.TrimSpace(orderID) == "" {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(t.pendingPairArb.OrderID), strings.TrimSpace(orderID)) {
		return
	}
	if conditionID != "" {
		t.pendingPairArb.ConditionID = conditionID
	}
	if !windowEnd.IsZero() {
		t.pendingPairArb.WindowEnd = windowEnd
	}
	if yesTokenID != "" {
		t.pendingPairArb.YesTokenID = yesTokenID
	}
	if noTokenID != "" {
		t.pendingPairArb.NoTokenID = noTokenID
	}
	if side := strings.ToUpper(strings.TrimSpace(leadSide)); side == "YES" || side == "NO" {
		t.pendingPairArb.LeadSide = side
	}
	t.savePositionState()
}

func (t *Trader) cancelStaleOrderAtWindowReset(orderID, requestName string) bool {
	if t.cfg.PaperTrade || strings.TrimSpace(orderID) == "" {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	if err := t.orders.CancelOrder(ctx, orderID); err != nil {
		if errors.Is(err, polymarket.ErrOrderNotCancellable) {
			t.logger.Debug("trader: stale carried order already not cancellable",
				zap.String("order_id", orderID),
				zap.String("request", requestName),
			)
			return false
		}
		t.logger.Warn("trader: stale carried order cancel failed",
			zap.String("order_id", orderID),
			zap.String("request", requestName),
			zap.Error(err),
		)
		return false
	}
	t.logger.Info("trader: stale carried order canceled at window reset",
		zap.String("order_id", orderID),
		zap.String("request", requestName),
	)
	return true
}

func (t *Trader) pairArbBuyCreditConfirmed(
	ctx context.Context,
	tokenID string,
	minShares float64,
) (bool, float64, error) {
	if minShares < pairArbShareDust {
		minShares = pairArbShareDust
	}

	if t.cfg.PaperTrade {
		return true, minShares, nil
	}

	// Kalshi execution is confirmed from authenticated order/fill data.
	// There is no Polymarket ERC1155 conditional-token wallet credit to
	// wait for after a Kalshi trade.
	return true, minShares, nil
}

func (t *Trader) pairArbBuyCreditConfirmedWithRetry(
	ctx context.Context,
	tokenID string,
	minShares float64,
	attempts int,
	interval time.Duration,
) (bool, float64, error) {
	if minShares < pairArbShareDust {
		minShares = pairArbShareDust
	}

	if t.cfg.PaperTrade {
		return true, minShares, nil
	}

	// Kalshi has no ERC1155 token-credit propagation stage.
	// executePairLimitBuy reaches this function only after exchange
	// fill evidence has already been obtained.
	return true, minShares, nil
}

func isPendingHedgeCreditProbe(requestName string) bool {
	return strings.HasPrefix(requestName, "pair_arb_hedge_credit_probe")
}

func isPendingHedgeBuy(requestName string) bool {
	return strings.HasPrefix(requestName, "pair_arb_hedge_buy")
}

func (t *Trader) pendingHedgeSettlementActive(now time.Time) bool {
	if t.pendingHedgePrePlace != nil {
		if t.pendingHedgePrePlace.PlacedAt.IsZero() || now.Sub(t.pendingHedgePrePlace.PlacedAt) < pairArbHedgeSettleGrace {
			return true
		}
	}
	if t.pendingPairArb != nil && (isPendingHedgeCreditProbe(t.pendingPairArb.RequestName) || isPendingHedgeBuy(t.pendingPairArb.RequestName)) {
		if t.pendingPairArb.PlacedAt.IsZero() || now.Sub(t.pendingPairArb.PlacedAt) < pairArbHedgeSettleGrace {
			return true
		}
	}
	return false
}

func (t *Trader) reconcilePendingPairArbHedgeCredit(ctx context.Context, po *pairArbPendingOrderState) error {
	if po == nil {
		return nil
	}
	pair := t.pairedPosition
	if pair == nil {
		t.clearPendingPairArbOrder()
		return nil
	}

	requestName := po.RequestName
	if requestName == "" {
		requestName = "pair_arb_hedge_credit_probe"
	}
	gr, grErr := t.getOrderTimed(ctx, requestName+"_reconcile", po.OrderID)
	status := ""
	if gr != nil {
		status = strings.ToLower(strings.TrimSpace(gr.Status))
	}
	if status == "unmatched" || status == "canceled" || status == "cancelled" || status == "expired" {
		t.logger.Warn("trader: pending hedge credit probe order is not filled; clearing pending state",
			zap.String("order_id", po.OrderID),
			zap.String("status", gr.Status),
		)
		t.clearPendingPairArbOrder()
		return nil
	}

	buySide, _, _ := pair.rebalanceState()
	if po.TokenID != "" {
		if po.TokenID == pair.YesTokenID {
			buySide = "YES"
		} else if po.TokenID == pair.NoTokenID {
			buySide = "NO"
		}
	}
	if buySide == "" {
		t.clearPendingPairArbOrder()
		return nil
	}
	trackedBefore := pair.YesShares
	if buySide != "YES" {
		trackedBefore = pair.NoShares
	}

	avg, gross, fillsErr := t.getFillsTimed(ctx, requestName+"_reconcile_fills", po.OrderID, po.TokenID)
	if (fillsErr != nil || gross <= pairArbShareDust || avg <= 0) && gr != nil {
		if prc, prcErr := strconv.ParseFloat(gr.Price, 64); prcErr == nil && prc > 0 && prc < 1.0 {
			avg = prc
		}
		if sm, smErr := strconv.ParseFloat(gr.SizeMatched, 64); smErr == nil && sm > pairArbShareDust {
			gross = sm
		}
	}
	if status == "live" && gross <= pairArbShareDust {
		return fmt.Errorf("pair arb: pending hedge order %s still live; waiting for fill", po.OrderID)
	}
	if gross <= pairArbShareDust || avg <= 0 {
		if status == "matched" || status == "filled" {
			// KALSHI SAFETY:
			// MATCHED/FILLED status alone is not sufficient accounting evidence.
			// Do not synthesize shares from the old Polymarket wallet-credit path.
			// Wait until GetFills or SizeMatched provides an authoritative quantity.
			return fmt.Errorf(
				"pair arb: Kalshi order %s reports %s but fill quantity/price is not available yet; waiting for authoritative fill evidence",
				po.OrderID,
				status,
			)
		}
		if grErr != nil {
			return fmt.Errorf("pair arb: pending hedge credit probe lookup failed: %w", grErr)
		}
		if fillsErr != nil {
			return fmt.Errorf("pair arb: pending hedge credit probe fill lookup failed: %w", fillsErr)
		}
		return fmt.Errorf("pair arb: pending hedge order %s matched state unresolved; waiting for fill details", po.OrderID)
	}

	feeShares := polymarket.ComputeBuyFeeShares(gross, avg, t.feeRateBps)
	actualShares := math.Floor((gross-feeShares)*100) / 100
	if actualShares < pairArbShareDust {
		actualShares = pairArbShareDust
	}
	minExpected := trackedBefore + actualShares - 0.02
	if minExpected < pairArbShareDust {
		minExpected = pairArbShareDust
	}
	credited, bal, cErr := t.pairArbBuyCreditConfirmed(ctx, po.TokenID, minExpected)
	if cErr != nil {
		return cErr
	}
	if !credited {
		return fmt.Errorf("pair arb: pending hedge order %s filled but wallet credit not confirmed yet (have %.2f need %.2f)", po.OrderID, bal, minExpected)
	}

	usdSpent := marketBuyOrderNotional(gross, avg)
	if buySide == "YES" {
		pair.YesShares += actualShares
		pair.YesUSDSpent += usdSpent
	} else {
		pair.NoShares += actualShares
		pair.NoUSDSpent += usdSpent
	}
	if pair.isBalanced() {
		pair.BalancedAt = time.Now()
		pair.HedgeBy = time.Time{}
	}
	t.clearPendingPairArbOrder()
	t.savePositionState()
	t.logger.Info("trader: pending hedge credit probe reconciled and absorbed",
		zap.String("order_id", po.OrderID),
		zap.String("hedge_side", buySide),
		zap.Float64("fill_price", avg),
		zap.Float64("shares", actualShares),
		zap.Float64("locked_shares", pair.lockedShares()),
	)
	return nil
}

func (t *Trader) tokenBalanceShares(ctx context.Context, tokenID string) (float64, error) {
	tokenID = strings.TrimSpace(tokenID)
	if tokenID == "" {
		return 0, fmt.Errorf("kalshi position lookup: empty token id")
	}

	// Kalshi logical token IDs are encoded as:
	//     <ticker>:YES
	//     <ticker>:NO
	//
	// Unlike Polymarket, Kalshi does not expose independent ERC1155 token
	// balances. Its authenticated portfolio endpoint reports the net market
	// position. FetchCurrentPositions() normalizes that signed Kalshi position
	// into a logical YES or NO UserPosition for the strategy layer.
	i := strings.LastIndex(tokenID, ":")
	if i <= 0 || i >= len(tokenID)-1 {
		return 0, fmt.Errorf("kalshi position lookup: invalid logical token id %q", tokenID)
	}

	conditionID := strings.TrimSpace(tokenID[:i])
	wantSide := strings.ToUpper(strings.TrimSpace(tokenID[i+1:]))
	if wantSide != "YES" && wantSide != "NO" {
		return 0, fmt.Errorf("kalshi position lookup: invalid outcome in token id %q", tokenID)
	}

	if t.orders == nil {
		return 0, fmt.Errorf("kalshi position lookup: nil order executor")
	}

	positions, err := t.orders.FetchCurrentPositions(ctx, 500, 0)
	if err != nil {
		return 0, fmt.Errorf("kalshi position lookup failed for %s: %w", tokenID, err)
	}

	best := 0.0
	for i := range positions {
		pos := &positions[i]

		if !strings.EqualFold(
			strings.TrimSpace(pos.ConditionID),
			conditionID,
		) {
			continue
		}

		if pos.Size <= pairArbShareDust {
			continue
		}

		isNo, ok := inferNoSideFromOutcomes(
			pos.Outcome,
			pos.OppositeOutcome,
			pos.OutcomeIndex,
		)
		if !ok {
			continue
		}

		matches := (wantSide == "NO" && isNo) ||
			(wantSide == "YES" && !isNo)

		if matches && pos.Size > best {
			best = pos.Size
		}
	}

	return best, nil
}

func (t *Trader) ensureNoPairArbInventory(ctx context.Context, yesTokenID, noTokenID string) error {
	if t.cfg.PaperTrade {
		return nil
	}
	zeroStreak := 0
	var lastErr error
	var lastYes, lastNo float64
	for attempt := 1; attempt <= pairArbExposureProbeAttempts; attempt++ {
		yesShares, yesErr := t.tokenBalanceShares(ctx, yesTokenID)
		noShares, noErr := t.tokenBalanceShares(ctx, noTokenID)
		if yesErr != nil || noErr != nil {
			if yesErr != nil {
				lastErr = yesErr
			} else {
				lastErr = noErr
			}
			zeroStreak = 0
		} else {
			lastYes, lastNo = yesShares, noShares
			if yesShares <= pairArbShareDust && noShares <= pairArbShareDust {
				zeroStreak++
				if zeroStreak >= pairArbExposureZeroConfirmMin {
					return nil
				}
			} else {
				zeroStreak = 0
			}
		}
		if attempt < pairArbExposureProbeAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(pairArbExposureProbeInterval):
			}
		}
	}
	if lastYes > pairArbInventoryEntryBlock || lastNo > pairArbInventoryEntryBlock {
		return fmt.Errorf("pair arb: existing wallet inventory detected (YES=%.2f NO=%.2f); refusing new lead", lastYes, lastNo)
	}
	if lastErr != nil {
		return fmt.Errorf("pair arb: unable to verify token inventory: %w", lastErr)
	}
	return fmt.Errorf("pair arb: unable to confidently verify zero inventory")
}

func (t *Trader) recoverPairArbAmbiguousSubmit(ctx context.Context, yesTokenID, noTokenID string, approxYesPrice float64, cause error) {
	if !(isAmbiguousPlaceOrderParseError(cause) || isNetworkError(cause)) || t.cfg.PaperTrade {
		return
	}
	t.logger.Error("pair arb: ambiguous/timeout submit response; probing wallet inventory immediately",
		zap.Error(cause),
		zap.String("yes_token", yesTokenID),
		zap.String("no_token", noTokenID),
	)
	invErr := t.ensureNoPairArbInventory(ctx, yesTokenID, noTokenID)
	if invErr == nil {
		return
	}
	if !strings.Contains(invErr.Error(), "existing wallet inventory detected") {
		t.logger.Warn("pair arb: ambiguous submit inventory probe failed", zap.Error(invErr))
		return
	}
	liqCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t.liquidateOrphanedPairArbInventory(liqCtx, yesTokenID, noTokenID, approxYesPrice)
	if retryErr := t.ensureNoPairArbInventory(liqCtx, yesTokenID, noTokenID); retryErr != nil {
		t.logger.Error("pair arb: wallet inventory still present after ambiguous-submit liquidation",
			zap.Error(retryErr),
		)
	}
}

func (t *Trader) triggerPairArbAmbiguousRecovery(yesTokenID, noTokenID string, approxYesPrice float64, cause error) {
	if cause == nil || t.cfg.PaperTrade {
		return
	}
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		t.recoverPairArbAmbiguousSubmit(bgCtx, yesTokenID, noTokenID, approxYesPrice, cause)
	}()
}

// liquidateOrphanedPairArbInventory sells any untracked YES/NO token inventory found in
// the wallet without a corresponding tracked position. This recovers capital from orders
// that were placed (HTTP 200) but whose response was lost before fill data could be parsed.
// Safe to call from a goroutine; each leg independently checks the on-chain balance.
func (t *Trader) liquidateOrphanedPairArbInventory(ctx context.Context, yesTokenID, noTokenID string, approxYesPrice float64) {
	if t.cfg.PaperTrade {
		return
	}
	if approxYesPrice <= 0 || approxYesPrice >= 1.0 {
		approxYesPrice = 0.50
	}
	approxNoPrice := math.Round((1.0-approxYesPrice)*100) / 100

	liquidateSide := func(side, tokenID string, approxPrice float64) {
		shares, err := t.tokenBalanceShares(ctx, tokenID)
		if err != nil {
			t.logger.Error("trader: orphan liquidation: balance lookup failed",
				zap.String("side", side), zap.Error(err))
			return
		}
		if shares <= pairArbShareDust {
			return
		}
		sellable, err := t.resolveSellableShares(ctx, tokenID, shares)
		if err != nil {
			t.logger.Error("trader: orphan liquidation: resolve sellable failed",
				zap.String("side", side), zap.Error(err))
			return
		}
		if sellable < polymarket.MinOrderShares {
			t.logger.Warn("trader: orphan liquidation: shares below CLOB minimum, will settle via CTF",
				zap.String("side", side), zap.Float64("shares", sellable))
			return
		}
		sharesStr := fmt.Sprintf("%.2f", sellable)
		fillPrice, filledShares, filled, sellErr := t.attemptSellWithFallback(ctx, tokenID, approxPrice, sharesStr)
		if sellErr != nil {
			t.logger.Error("trader: orphan liquidation: sell error",
				zap.String("side", side), zap.Error(sellErr))
			return
		}
		if !filled {
			t.logger.Error("trader: orphan liquidation: sell did not fill",
				zap.String("side", side), zap.Float64("shares", sellable))
			return
		}
		logTimedActivityInfo(fmt.Sprintf("ORPHAN LIQUIDATION %s %.2f shares @ %.4f", side, filledShares, fillPrice))
		t.logger.Warn("trader: orphaned pair arb inventory liquidated",
			zap.String("side", side),
			zap.Float64("shares", filledShares),
			zap.Float64("sell_price", fillPrice),
		)
	}

	liquidateSide("YES", yesTokenID, approxYesPrice)
	liquidateSide("NO", noTokenID, approxNoPrice)
}

// monitorAndLiquidateOrphanedPairArbInventory keeps probing wallet balances after
// a dual pre-place failure because token credits can appear seconds after the hedge
// order is reported as matched. It retries liquidation until balances are gone.
func (t *Trader) monitorAndLiquidateOrphanedPairArbInventory(yesTokenID, noTokenID string, approxYesPrice float64) {
	if t.cfg.PaperTrade {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), pairArbOrphanRecoveryWatch)
	defer cancel()

	zeroStreak := 0
	for {
		yesShares, yesErr := t.tokenBalanceShares(ctx, yesTokenID)
		noShares, noErr := t.tokenBalanceShares(ctx, noTokenID)
		if yesErr == nil && noErr == nil {
			if yesShares <= pairArbShareDust && noShares <= pairArbShareDust {
				zeroStreak++
				if zeroStreak >= pairArbExposureZeroConfirmMin {
					t.pairArbHedgeOrphanFlag = false
					return
				}
			} else {
				zeroStreak = 0
				t.logger.Warn("pair arb: delayed orphan inventory detected after dual pre-place failure; retrying liquidation",
					zap.Float64("yes_shares", yesShares),
					zap.Float64("no_shares", noShares),
				)
				t.liquidateOrphanedPairArbInventory(ctx, yesTokenID, noTokenID, approxYesPrice)
			}
		} else {
			zeroStreak = 0
			t.logger.Warn("pair arb: delayed orphan inventory probe failed",
				zap.NamedError("yes_err", yesErr),
				zap.NamedError("no_err", noErr),
			)
		}

		select {
		case <-ctx.Done():
			finalCtx, finalCancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer finalCancel()
			if err := t.ensureNoPairArbInventory(finalCtx, yesTokenID, noTokenID); err == nil {
				t.pairArbHedgeOrphanFlag = false
			} else {
				t.logger.Error("pair arb: orphan exposure still present after delayed liquidation watch",
					zap.Error(err),
				)
			}
			return
		case <-time.After(pairArbExposureProbeInterval):
		}
	}
}

func (t *Trader) recoverDualPrePlaceOrphanedExposure(yesTokenID, noTokenID, hedgeTokenID, hedgeOrderID string, approxYesPrice float64, exposureLikely bool) {
	if t.cfg.PaperTrade {
		t.clearPendingPairArbOrder()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	leadOrderID := ""
	leadTokenID := ""
	if po := t.pendingPairArb; po != nil {
		leadOrderID = po.OrderID
		leadTokenID = po.TokenID
	}

	orderShowsFill := func(requestName, orderID, tokenID string) bool {
		if orderID == "" {
			return false
		}
		if gr, grErr := t.getOrderTimed(ctx, requestName+"_status", orderID); grErr == nil && gr != nil {
			status := strings.ToLower(strings.TrimSpace(gr.Status))
			if status == "matched" || status == "filled" {
				return true
			}
			if sm, smErr := strconv.ParseFloat(gr.SizeMatched, 64); smErr == nil && sm > pairArbShareDust {
				return true
			}
		}
		if _, gross, fillsErr := t.getFillsTimed(ctx, requestName+"_fills", orderID, tokenID); fillsErr == nil && gross > pairArbShareDust {
			return true
		}
		return false
	}

	exposureSeen := exposureLikely ||
		orderShowsFill("pair_arb_dual_recover_lead", leadOrderID, leadTokenID) ||
		orderShowsFill("pair_arb_dual_recover_hedge", hedgeOrderID, hedgeTokenID)

	for attempt := 0; !exposureSeen && attempt < pairArbExposureProbeAttempts; attempt++ {
		yesShares, yesErr := t.tokenBalanceShares(ctx, yesTokenID)
		noShares, noErr := t.tokenBalanceShares(ctx, noTokenID)
		if yesErr == nil && noErr == nil && (yesShares > pairArbShareDust || noShares > pairArbShareDust) {
			exposureSeen = true
			break
		}
		if attempt < pairArbExposureProbeAttempts-1 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(pairArbExposureProbeInterval):
			}
		}
	}

	if !exposureSeen {
		if leadOrderID != "" {
			if gr, grErr := t.getOrderTimed(ctx, "pair_arb_dual_recover_lead_final", leadOrderID); grErr == nil && gr != nil {
				status := strings.ToLower(strings.TrimSpace(gr.Status))
				if status == "unmatched" || status == "canceled" || status == "cancelled" || status == "expired" {
					t.clearPendingPairArbOrder()
				}
			}
		}
		return
	}

	t.pairArbHedgeOrphanFlag = true
	t.logger.Error("pair arb: dual pre-place failure left wallet exposure; liquidating immediately",
		zap.String("lead_order_id", leadOrderID),
		zap.String("hedge_order_id", hedgeOrderID),
		zap.String("yes_token", yesTokenID),
		zap.String("no_token", noTokenID),
	)
	t.liquidateOrphanedPairArbInventory(ctx, yesTokenID, noTokenID, approxYesPrice)
	// Always clear the pending order after liquidation: the sell has been submitted so the
	// order-ID no longer represents live exposure. ensureNoPairArbInventory may fail due to
	// settlement lag even after a successful sell, which previously left pendingPairArb set
	// forever and blocked every future entry with "pending order is MATCHED".
	t.clearPendingPairArbOrder()
	go t.monitorAndLiquidateOrphanedPairArbInventory(yesTokenID, noTokenID, approxYesPrice)
}

func (t *Trader) reconcilePendingPairArbOrder(ctx context.Context) error {
	if t.cfg.PaperTrade || t.pendingPairArb == nil {
		return nil
	}
	po := t.pendingPairArb
	age := time.Since(po.PlacedAt)
	if po.PlacedAt.IsZero() || age < 0 {
		age = 0
	}
	if isPendingHedgeCreditProbe(po.RequestName) {
		return t.reconcilePendingPairArbHedgeCredit(ctx, po)
	}
	if isPendingHedgeBuy(po.RequestName) && t.pairedPosition != nil {
		return t.reconcilePendingPairArbHedgeCredit(ctx, po)
	}
	requestName := po.RequestName
	if requestName == "" {
		requestName = "pair_arb_unknown_buy"
	}
	var lastErr error
	sawOrderState := false
	sawFillEvidence := false
	for attempt := 1; attempt <= pairArbOrderProbeAttempts; attempt++ {
		gr, grErr := t.getOrderTimed(ctx, requestName+"_reconcile", po.OrderID)
		if grErr == nil && gr != nil {
			sawOrderState = true
			status := strings.ToLower(strings.TrimSpace(gr.Status))
			if status == "matched" || status == "filled" {
				if po.TokenID == "" {
					// No token ID to recover from — cannot open position safely, just clear.
					t.clearPendingPairArbOrder()
					return nil
				}
				walletBal, walletErr := t.tokenBalanceShares(ctx, po.TokenID)
				if walletErr == nil && walletBal <= pairArbShareDust {
					age := time.Since(po.PlacedAt)
					if po.PlacedAt.IsZero() {
						age = 0
					}
					if age < pairArbLeadCreditGrace {
						return fmt.Errorf("pair arb: pending order %s is %s but wallet credit not visible yet (age=%s); waiting", po.OrderID, gr.Status, age.Round(time.Second))
					}
					avg, gross, fillsErr := t.getFillsTimed(ctx, requestName+"_reconcile_fills", po.OrderID, po.TokenID)
					if fillsErr == nil && gross > pairArbShareDust {
						if age >= pairArbHedgeSettleGrace && t.pairedPosition == nil && t.position == nil {
							t.logger.Error("trader: fail-closed: matched pending order has fills but wallet credit still missing; keeping entry blocked",
								zap.String("order_id", po.OrderID),
								zap.Float64("fills_gross", gross),
								zap.Float64("fills_avg", avg),
								zap.Float64("wallet_balance", walletBal),
								zap.Duration("age", age),
							)
						}
						return fmt.Errorf("pair arb: pending order %s is %s with fills %.2f @ %.4f but wallet still empty; waiting", po.OrderID, gr.Status, gross, avg)
					}
					// Fail-closed: never auto-clear matched pending ownership when wallet credit is absent.
					t.logger.Error("trader: fail-closed: pending pair arb order remained MATCHED without wallet credit; keeping pending blocked",
						zap.String("order_id", po.OrderID),
						zap.Float64("wallet_balance", walletBal),
						zap.Duration("age", age),
					)
					return fmt.Errorf("pair arb: pending order %s is %s but wallet credit absent after grace (age=%s); fail-closed block remains", po.OrderID, gr.Status, age.Round(time.Second))
				}
				if walletErr == nil && walletBal > pairArbShareDust && t.pairedPosition == nil {
					// Shares are confirmed in wallet but position was never opened (wallet-credit
					// timeout on the original submitPath). Recover: open a lead-only position now.
					fillPrice, _ := strconv.ParseFloat(gr.Price, 64)
					fillsAvg, fillsGross, fillsErr := t.getFillsTimed(ctx, requestName+"_reconcile_fills", po.OrderID, po.TokenID)
					if fillsErr == nil && fillsAvg > 0 {
						fillPrice = fillsAvg
					}
					if fillPrice <= 0 {
						fillPrice = 0.50
					}
					effectiveGrossShares := walletBal
					if fillsErr == nil && fillsGross > pairArbShareDust {
						effectiveGrossShares = fillsGross
					}
					feeShares := polymarket.ComputeBuyFeeShares(effectiveGrossShares, fillPrice, t.feeRateBps)
					usdSpent := marketBuyOrderNotional(effectiveGrossShares, fillPrice)
					t.logger.Warn("trader: recovering untracked lead position from wallet balance",
						zap.String("order_id", po.OrderID),
						zap.Float64("wallet_balance", walletBal),
						zap.Float64("fills_gross_shares", fillsGross),
						zap.Float64("fill_price", fillPrice),
					)
					// Determine which side this lead was for from persisted metadata,
					// then fall back to token-ID matching.
					leadSide := strings.ToUpper(strings.TrimSpace(po.LeadSide))
					pairConditionID := strings.TrimSpace(po.ConditionID)
					if pairConditionID == "" {
						pairConditionID = t.convConditionID
					}
					pairWindowEnd := po.WindowEnd
					if pairWindowEnd.IsZero() {
						pairWindowEnd = t.detector.WindowEnd()
					}
					yesID, noID := t.convYesTokenID, t.convNoTokenID
					if po.YesTokenID != "" {
						yesID = po.YesTokenID
					}
					if po.NoTokenID != "" {
						noID = po.NoTokenID
					}
					if yesID == "" || noID == "" {
						if strings.EqualFold(leadSide, "YES") {
							yesID = po.TokenID
						} else {
							noID = po.TokenID
						}
					}
					if leadSide != "YES" && leadSide != "NO" {
						if noID != "" && strings.EqualFold(po.TokenID, noID) {
							leadSide = "NO"
						} else {
							leadSide = "YES"
						}
					}
					isNoLead := strings.EqualFold(leadSide, "NO")
					// Clear pending BEFORE opening position so openLeadOnlyPosition can proceed.
					t.clearPendingPairArbOrder()
					pair := &PairArbPosition{
						ConditionID: pairConditionID,
						YesTokenID:  yesID,
						NoTokenID:   noID,
						LeadSide:    leadSide,
						WindowEnd:   pairWindowEnd,
						OpenedAt:    po.PlacedAt,
						HedgeBy:     time.Now().Add(t.pairArbHedgeTimeout()),
					}
					if !pair.WindowEnd.IsZero() && pair.HedgeBy.After(pair.WindowEnd) {
						pair.HedgeBy = pair.WindowEnd
					}
					if isNoLead {
						pair.NoShares = walletBal
						pair.NoUSDSpent = usdSpent
						pair.NoWalletConfirmed = true
					} else {
						pair.YesShares = walletBal
						pair.YesUSDSpent = usdSpent
						pair.YesWalletConfirmed = true
					}
					t.pairedPosition = pair
					t.detector.SetInPosition(true)
					t.savePositionState()
					display.TradeOpen(leadSide+" [PAIR LEAD RECOVERED]", fillPrice, walletBal, feeShares, usdSpent, 0, t.cfg.PaperTrade)
					_, _, liveYes, _, _, _ := t.detector.Snapshot()
					liveNo := math.Round((1.0-liveYes)*100) / 100
					if liveYes <= 0 || liveYes >= 1.0 {
						if isNoLead {
							liveNo = fillPrice
							liveYes = math.Round((1.0-liveNo)*100) / 100
						} else {
							liveYes = fillPrice
							liveNo = math.Round((1.0-liveYes)*100) / 100
						}
					}
					if rbErr := t.maybeRebalancePairArb(ctx, liveYes, liveNo); rbErr != nil {
						t.logger.Warn("trader: recovered lead position but immediate hedge attempt failed",
							zap.String("order_id", po.OrderID),
							zap.Error(rbErr),
						)
					}
					return nil
				}
				// Position already exists or wallet unreadable — keep blocking.
				return fmt.Errorf("pair arb: pending order %s is %s and may hold shares; resolve/close it before new entries", po.OrderID, gr.Status)
			}
			if status == "live" {
				_ = t.cancelOrderTimed(ctx, requestName+"_reconcile_cancel", po.OrderID)
			}
			if status == "unmatched" || status == "canceled" || status == "cancelled" || status == "expired" {
				t.logger.Info("trader: pending pair arb order reconciled as not-filled",
					zap.String("order_id", po.OrderID),
					zap.String("status", gr.Status),
				)
				t.clearPendingPairArbOrder()
				return nil
			}
		}
		avg, gross, fillsErr := t.getFillsTimed(ctx, requestName+"_reconcile_fills", po.OrderID, po.TokenID)
		if fillsErr == nil && gross > pairArbShareDust && avg > 0 {
			sawFillEvidence = true
			if t.pairedPosition == nil && t.position == nil && po.TokenID != "" {
				if walletBal, walletErr := t.tokenBalanceShares(ctx, po.TokenID); walletErr == nil && walletBal <= pairArbShareDust {
					if age >= pairArbHedgeSettleGrace {
						t.logger.Error("trader: fail-closed: pending order has historical fills but wallet empty; keeping entry blocked",
							zap.String("order_id", po.OrderID),
							zap.Float64("filled_gross", gross),
							zap.Float64("filled_avg", avg),
							zap.Float64("wallet_balance", walletBal),
							zap.Duration("age", age),
						)
					}
					return fmt.Errorf("pair arb: pending order %s has fills %.2f @ %.4f but wallet credit not visible yet (age=%s); waiting", po.OrderID, gross, avg, age.Round(time.Second))
				}
			}
			return fmt.Errorf("pair arb: pending order %s has %.2f filled shares at %.4f; resolve/close before new entries", po.OrderID, gross, avg)
		}
		if grErr != nil {
			lastErr = grErr
		}
		if fillsErr != nil {
			lastErr = fillsErr
		}
		if attempt < pairArbOrderProbeAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(pairArbOrderProbeInterval):
			}
		}
	}
	if !sawOrderState && !sawFillEvidence {
		if age < pairArbLeadCreditGrace {
			return fmt.Errorf("pair arb: pending order %s has no visible status/fills yet (age=%s); waiting", po.OrderID, age.Round(time.Second))
		}
		if po.TokenID == "" {
			return fmt.Errorf("pair arb: pending order %s missing token metadata; fail-closed block remains", po.OrderID)
		}
		if t.pairedPosition == nil && t.position == nil {
			if walletBal, walletErr := t.tokenBalanceShares(ctx, po.TokenID); walletErr == nil {
				if walletBal <= pairArbShareDust {
					t.logger.Error("trader: fail-closed: pending order has no status/fills after grace; keeping entry blocked",
						zap.String("order_id", po.OrderID),
						zap.String("request", po.RequestName),
						zap.String("token_id", po.TokenID),
						zap.Duration("age", age),
					)
					return fmt.Errorf("pair arb: pending order %s has no visible status/fills and no wallet credit after grace (age=%s); fail-closed block remains", po.OrderID, age.Round(time.Second))
				}
				return fmt.Errorf("pair arb: pending order %s unresolved and wallet has %.2f shares; resolve inventory before new lead", po.OrderID, walletBal)
			} else {
				lastErr = walletErr
			}
		}
	}
	if lastErr != nil {
		return fmt.Errorf("pair arb: unable to reconcile pending order %s: %w", po.OrderID, lastErr)
	}
	return fmt.Errorf("pair arb: pending order %s unresolved; waiting for reconciliation data", po.OrderID)
}

func (t *Trader) reconcilePendingHedgePrePlaceWhenFlat(ctx context.Context) error {
	if t.cfg.PaperTrade || t.pendingHedgePrePlace == nil {
		return nil
	}
	if t.pairedPosition != nil || t.position != nil {
		return fmt.Errorf("pair arb: pending hedge order %s unresolved; waiting before new lead", t.pendingHedgePrePlace.OrderID)
	}

	po := t.pendingHedgePrePlace
	requestName := po.RequestName
	if requestName == "" {
		requestName = "pair_arb_hedge_pre"
	}
	age := time.Since(po.PlacedAt)
	if po.PlacedAt.IsZero() || age < 0 {
		age = 0
	}

	gr, grErr := t.getOrderTimed(ctx, requestName+"_reconcile", po.OrderID)
	status := ""
	if gr != nil {
		status = strings.ToLower(strings.TrimSpace(gr.Status))
	}

	if status == "unmatched" || status == "canceled" || status == "cancelled" || status == "expired" {
		t.clearPrePlacedHedgeOrder()
		return nil
	}
	if status == "live" {
		_ = t.cancelOrderTimed(ctx, requestName+"_reconcile_cancel", po.OrderID)
		postStatus := ""
		if postOrder, postErr := t.getOrderTimed(ctx, requestName+"_reconcile_post_cancel", po.OrderID); postErr == nil && postOrder != nil {
			postStatus = strings.ToLower(strings.TrimSpace(postOrder.Status))
		}
		if postStatus == "unmatched" || postStatus == "canceled" || postStatus == "cancelled" || postStatus == "expired" {
			t.clearPrePlacedHedgeOrder()
			return nil
		}
		postAvg, postGross, postFillsErr := t.getFillsTimed(ctx, requestName+"_reconcile_post_cancel_fills", po.OrderID, po.TokenID)
		if postFillsErr == nil && postGross > pairArbShareDust && postAvg > 0 {
			return fmt.Errorf("pair arb: pending hedge order %s has %.2f filled shares @ %.4f; resolve inventory before new lead", po.OrderID, postGross, postAvg)
		}
		if postStatus == "matched" || postStatus == "filled" {
			if po.TokenID != "" {
				if bal, balErr := t.tokenBalanceShares(ctx, po.TokenID); balErr == nil && bal > pairArbShareDust {
					return fmt.Errorf("pair arb: pending hedge order %s matched and wallet has %.2f shares; resolve inventory before new lead", po.OrderID, bal)
				}
			}
			if age >= pairArbHedgeSettleGrace {
				t.logger.Error("trader: fail-closed: pre-hedge cancel race stayed matched without wallet credit; keeping entry blocked",
					zap.String("order_id", po.OrderID),
					zap.Duration("age", age),
				)
				return fmt.Errorf("pair arb: pending hedge order %s matched after cancel race with unresolved ownership (age=%s); fail-closed block remains", po.OrderID, age.Round(time.Second))
			}
			return fmt.Errorf("pair arb: pending hedge order %s unresolved; waiting before new lead", po.OrderID)
		}
		if age >= pairArbHedgeSettleGrace {
			t.logger.Error("trader: fail-closed: pre-hedge live-cancel had no terminal ownership signal; keeping entry blocked",
				zap.String("order_id", po.OrderID),
				zap.Duration("age", age),
			)
			return fmt.Errorf("pair arb: pending hedge order %s unresolved after live-cancel with no terminal state (age=%s); fail-closed block remains", po.OrderID, age.Round(time.Second))
		}
		return fmt.Errorf("pair arb: pending hedge order %s unresolved; waiting before new lead", po.OrderID)
	}

	avg, gross, fillsErr := t.getFillsTimed(ctx, requestName+"_reconcile_fills", po.OrderID, po.TokenID)
	if fillsErr == nil && gross > pairArbShareDust && avg > 0 {
		return fmt.Errorf("pair arb: pending hedge order %s has %.2f filled shares @ %.4f; resolve inventory before new lead", po.OrderID, gross, avg)
	}

	if status == "matched" || status == "filled" {
		if po.TokenID != "" {
			if bal, balErr := t.tokenBalanceShares(ctx, po.TokenID); balErr == nil && bal > pairArbShareDust {
				return fmt.Errorf("pair arb: pending hedge order %s matched and wallet has %.2f shares; resolve inventory before new lead", po.OrderID, bal)
			}
		}
		if age >= pairArbHedgeSettleGrace {
			t.logger.Error("trader: fail-closed: matched pre-hedge unresolved after grace; keeping entry blocked",
				zap.String("order_id", po.OrderID),
				zap.Duration("age", age),
			)
			return fmt.Errorf("pair arb: pending hedge order %s matched but ownership unresolved after grace (age=%s); fail-closed block remains", po.OrderID, age.Round(time.Second))
		}
		return fmt.Errorf("pair arb: pending hedge order %s unresolved; waiting before new lead", po.OrderID)
	}

	if age >= pairArbHedgeSettleGrace {
		t.logger.Error("trader: fail-closed: pre-hedge reconcile timed out; keeping entry blocked",
			zap.String("order_id", po.OrderID),
			zap.Duration("age", age),
			zap.Error(grErr),
			zap.Error(fillsErr),
		)
		return fmt.Errorf("pair arb: pending hedge order %s unresolved after reconcile timeout (age=%s); fail-closed block remains", po.OrderID, age.Round(time.Second))
	}

	if grErr != nil {
		return fmt.Errorf("pair arb: pending hedge order %s unresolved; waiting before new lead", po.OrderID)
	}
	return fmt.Errorf("pair arb: pending hedge order %s unresolved; waiting before new lead", po.OrderID)
}

func shouldSkipPairArbWalletDeltaAbsorb(tokenID, pendingPreHedgeTokenID, pendingHedgeCreditTokenID string) bool {
	tokenID = strings.TrimSpace(tokenID)
	if tokenID == "" {
		return true
	}
	if pendingPreHedgeTokenID != "" && strings.EqualFold(tokenID, strings.TrimSpace(pendingPreHedgeTokenID)) {
		return true
	}
	if pendingHedgeCreditTokenID != "" && strings.EqualFold(tokenID, strings.TrimSpace(pendingHedgeCreditTokenID)) {
		return true
	}
	return false
}

func (t *Trader) reconcileLivePairArbState(ctx context.Context, pair *PairArbPosition, currentYesPrice float64, currentNoPrice float64) {
	if pair == nil || t.cfg.PaperTrade {
		return
	}

	if t.pendingPairArb != nil && (isPendingHedgeCreditProbe(t.pendingPairArb.RequestName) || isPendingHedgeBuy(t.pendingPairArb.RequestName)) {
		pendingOrderID := t.pendingPairArb.OrderID
		pendingRequest := t.pendingPairArb.RequestName
		reconcileCtx, cancel := context.WithTimeout(ctx, pairArbReconcileLookupTimeout)
		err := t.reconcilePendingPairArbOrder(reconcileCtx)
		cancel()
		if err != nil {
			t.logger.Debug("trader: pending hedge reconciliation during live pair position still in progress",
				zap.String("order_id", pendingOrderID),
				zap.String("request", pendingRequest),
				zap.Error(err),
			)
		}
	}

	pair = t.pairedPosition
	if pair == nil {
		return
	}

	// Skip the wallet-delta probes entirely for brand-new positions: the
	// pairArbBuyCreditConfirmedWithRetry call that ran moments ago already
	// confirmed the balance. Nothing new could have appeared in < 500ms.
	if time.Since(pair.OpenedAt) < 500*time.Millisecond {
		return
	}

	pendingPreHedgeTokenID := ""
	if php := t.pendingHedgePrePlace; php != nil {
		pendingPreHedgeTokenID = strings.TrimSpace(php.TokenID)
	}
	pendingHedgeCreditTokenID := ""
	if ppo := t.pendingPairArb; ppo != nil && (isPendingHedgeCreditProbe(ppo.RequestName) || isPendingHedgeBuy(ppo.RequestName)) {
		pendingHedgeCreditTokenID = strings.TrimSpace(ppo.TokenID)
	}

	type deltaResult struct {
		nextShares   float64
		usdDelta     float64
		changed      bool
		probeOK      bool
		walletShares float64
	}
	applyWalletDelta := func(side string, tokenID string, trackedShares float64) deltaResult {
		if tokenID == "" {
			return deltaResult{nextShares: trackedShares}
		}
		if shouldSkipPairArbWalletDeltaAbsorb(tokenID, pendingPreHedgeTokenID, pendingHedgeCreditTokenID) {
			// This side is currently under pending order reconciliation; absorbing
			// wallet delta here can double-count fills and trigger spurious rebalances.
			return deltaResult{nextShares: trackedShares}
		}
		bal, err := t.tokenBalanceShares(ctx, tokenID)
		if err != nil {
			t.logger.Debug("trader: live pair wallet reconcile balance probe failed",
				zap.String("side", side),
				zap.String("token_id", tokenID),
				zap.Error(err),
			)
			return deltaResult{nextShares: trackedShares}
		}
		result := deltaResult{nextShares: trackedShares, probeOK: true, walletShares: bal}
		delta := math.Floor((bal-trackedShares)*100) / 100
		if delta < pairArbWalletReconcileDelta {
			return result
		}
		price := pair.sideAveragePrice(side)
		if price <= 0 || price >= 1.0 {
			if side == "YES" {
				price = currentYesPrice
			} else {
				price = currentNoPrice
				if price <= 0 || price >= 1.0 {
					price = math.Round((1.0-currentYesPrice)*100) / 100
				}
			}
		}
		if price <= 0 || price >= 1.0 {
			price = 0.50
		}
		usdSpentDelta := marketBuyOrderNotional(delta, price)
		t.logger.Warn("trader: absorbed untracked live pair hedge shares from wallet",
			zap.String("side", side),
			zap.String("token_id", tokenID),
			zap.Float64("tracked_shares", trackedShares),
			zap.Float64("wallet_shares", bal),
			zap.Float64("delta_shares", delta),
			zap.Float64("assumed_price", price),
		)
		result.nextShares = trackedShares + delta
		result.usdDelta = usdSpentDelta
		result.changed = true
		return result
	}

	// Run YES and NO balance probes in parallel — they are independent token reads.
	yesCh := make(chan deltaResult, 1)
	noCh := make(chan deltaResult, 1)
	go func() { yesCh <- applyWalletDelta("YES", pair.YesTokenID, pair.YesShares) }()
	go func() { noCh <- applyWalletDelta("NO", pair.NoTokenID, pair.NoShares) }()
	yr := <-yesCh
	nr := <-noCh

	changed := false
	confirmChanged := false
	if yr.changed {
		pair.YesShares = yr.nextShares
		pair.YesUSDSpent += yr.usdDelta
		changed = true
	}
	if nr.changed {
		pair.NoShares = nr.nextShares
		pair.NoUSDSpent += nr.usdDelta
		changed = true
	}
	if yr.probeOK && pair.YesShares > pairArbShareDust && yr.walletShares+pairArbShareDust >= pair.YesShares-pairArbWalletReconcileDelta {
		if !pair.YesWalletConfirmed {
			pair.YesWalletConfirmed = true
			confirmChanged = true
		}
	}
	if nr.probeOK && pair.NoShares > pairArbShareDust && nr.walletShares+pairArbShareDust >= pair.NoShares-pairArbWalletReconcileDelta {
		if !pair.NoWalletConfirmed {
			pair.NoWalletConfirmed = true
			confirmChanged = true
		}
	}
	if !changed && !confirmChanged {
		if pair.isBalanced() {
			t.reconcileBalancedPairCostBasis(ctx, pair)
		}
		return
	}
	if pair.isBalanced() {
		pair.BalancedAt = time.Now()
		pair.HedgeBy = time.Time{}
	}
	t.savePositionState()

	pair = t.pairedPosition
	if pair == nil || !pair.isBalanced() {
		return
	}
	t.reconcileBalancedPairCostBasis(ctx, pair)
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

func round4(v float64) float64 {
	return math.Round(v*10000) / 10000
}

func (t *Trader) stablePairCostSample(ctx context.Context, pair *PairArbPosition) (pairCostBasisSample, bool) {
	if pair == nil || pair.ConditionID == "" || t.orders == nil {
		return pairCostBasisSample{}, false
	}
	positions, err := t.orders.FetchCurrentPositions(ctx, 500, 0)
	if err != nil {
		return pairCostBasisSample{}, false
	}
	var yesPos *polymarket.UserPosition
	var noPos *polymarket.UserPosition
	for i := range positions {
		pos := &positions[i]
		if !strings.EqualFold(strings.TrimSpace(pos.ConditionID), strings.TrimSpace(pair.ConditionID)) {
			continue
		}
		if pos.Size <= pairArbShareDust || pos.AvgPrice <= 0 || pos.AvgPrice >= 1.0 {
			continue
		}
		isNo, ok := inferNoSideFromOutcomes(pos.Outcome, pos.OppositeOutcome, pos.OutcomeIndex)
		if !ok {
			continue
		}
		if isNo {
			if noPos == nil || pos.Size > noPos.Size {
				noPos = pos
			}
		} else {
			if yesPos == nil || pos.Size > yesPos.Size {
				yesPos = pos
			}
		}
	}
	if yesPos == nil || noPos == nil {
		return pairCostBasisSample{}, false
	}
	return pairCostBasisSample{
		YesSize: round2(yesPos.Size),
		NoSize:  round2(noPos.Size),
		YesAvg:  round4(yesPos.AvgPrice),
		NoAvg:   round4(noPos.AvgPrice),
	}, true
}

func pairCostSamplesMatch(a, b pairCostBasisSample) bool {
	return math.Abs(a.YesSize-b.YesSize) <= 0.01 &&
		math.Abs(a.NoSize-b.NoSize) <= 0.01 &&
		math.Abs(a.YesAvg-b.YesAvg) <= 0.0015 &&
		math.Abs(a.NoAvg-b.NoAvg) <= 0.0015
}

func (t *Trader) reconcileBalancedPairCostBasis(ctx context.Context, pair *PairArbPosition) {
	// Kalshi portfolio positions do not expose independent YES/NO
	// average cost bases. Preserve confirmed order-fill accounting.
	return
}

func claimRetryDelay(attempt int) time.Duration {
	if attempt <= 5 {
		return claimRetryInterval
	}
	if attempt <= 20 {
		return claimRetryMediumInterval
	}
	return claimRetrySlowInterval
}

func isClaimNotYetAvailableErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not yet") ||
		strings.Contains(s, "not resolved") ||
		strings.Contains(s, "not claimable") ||
		strings.Contains(s, "payout") ||
		strings.Contains(s, "condition not") ||
		strings.Contains(s, "invalid index set") ||
		strings.Contains(s, "execution reverted")
}

func (t *Trader) addPendingClaim(claim pendingClaim) {
	if claim.ConditionID == "" || t.cfg.PaperTrade {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(claim.StrategyName), "external_reconcile") {
		t.rememberBotConditionID(claim.ConditionID)
	}
	if claim.AddedAt.IsZero() {
		claim.AddedAt = time.Now()
	}
	if claim.Status == "" {
		claim.Status = "queued"
	}
	key := claimRetryKey(claim.ConditionID, claim.IsNoSide)
	t.claimMu.Lock()
	for _, existing := range t.pendingClaims {
		if claimRetryKey(existing.ConditionID, existing.IsNoSide) == key {
			t.claimMu.Unlock()
			return
		}
	}
	t.pendingClaims = append(t.pendingClaims, claim)
	t.setClaimDashboardStatusLocked("queued", claim.ConditionID, claim.IsNoSide, claim.AttemptCount, claim.NextRetryAt, "claim queued")
	t.claimMu.Unlock()
	t.savePositionState()
}

func claimSideName(isNoSide bool) string {
	if isNoSide {
		return "NO"
	}
	return "YES"
}

func (t *Trader) setClaimDashboardStatusLocked(status, conditionID string, isNoSide bool, attempt int, nextRetryAt time.Time, message string) {
	t.claimLastStatus = strings.TrimSpace(status)
	t.claimLastConditionID = strings.TrimSpace(conditionID)
	t.claimLastSide = claimSideName(isNoSide)
	t.claimLastAttempt = attempt
	t.claimLastNextRetryAt = nextRetryAt
	t.claimLastMessage = strings.TrimSpace(message)
	t.claimLastUpdatedAt = time.Now()
}

func (t *Trader) markPendingClaimAttempt(conditionID string, isNoSide bool, attempt int, nextRetry time.Time, claimable bool, status, lastError string) {
	key := claimRetryKey(conditionID, isNoSide)
	t.claimMu.Lock()
	updated := false
	for i := range t.pendingClaims {
		claim := &t.pendingClaims[i]
		if claimRetryKey(claim.ConditionID, claim.IsNoSide) != key {
			continue
		}
		claim.AttemptCount = attempt
		claim.LastAttemptAt = time.Now()
		claim.NextRetryAt = nextRetry
		claim.Claimable = claimable
		claim.Status = status
		claim.LastError = lastError
		t.setClaimDashboardStatusLocked(status, conditionID, isNoSide, attempt, nextRetry, lastError)
		updated = true
		break
	}
	t.claimMu.Unlock()
	if updated {
		t.savePositionState()
	}
}

func (t *Trader) removePendingClaim(conditionID string, isNoSide bool) {
	key := claimRetryKey(conditionID, isNoSide)
	t.claimMu.Lock()
	if len(t.pendingClaims) == 0 {
		t.claimMu.Unlock()
		return
	}
	out := t.pendingClaims[:0]
	removed := false
	for _, claim := range t.pendingClaims {
		if claimRetryKey(claim.ConditionID, claim.IsNoSide) == key {
			removed = true
			continue
		}
		out = append(out, claim)
	}
	t.pendingClaims = out
	t.claimMu.Unlock()
	if removed {
		t.savePositionState()
	}
}

func claimRetryKey(conditionID string, isNoSide bool) string {
	side := "yes"
	if isNoSide {
		side = "no"
	}
	return conditionID + ":" + side
}

func (t *Trader) enqueueClaimRetry(claim pendingClaim) {
	// Kalshi settlement is automatic after market finalization.
	return
}

func (t *Trader) DashboardClaimSnapshot() DashboardClaimSnapshot {
	t.claimMu.Lock()
	defer t.claimMu.Unlock()

	snap := DashboardClaimSnapshot{
		LastStatus:      t.claimLastStatus,
		LastMessage:     t.claimLastMessage,
		LastConditionID: t.claimLastConditionID,
		LastSide:        t.claimLastSide,
		LastUpdatedAt:   t.claimLastUpdatedAt,
		LastAttempt:     t.claimLastAttempt,
		NextRetryAt:     t.claimLastNextRetryAt,
	}

	for _, claim := range t.pendingClaims {
		snap.PendingCount++
		if strings.EqualFold(strings.TrimSpace(claim.Status), "claimable_retry_failed") {
			snap.FailedCount++
		}
		if !claim.NextRetryAt.IsZero() && (snap.NextRetryAt.IsZero() || claim.NextRetryAt.Before(snap.NextRetryAt)) {
			snap.NextRetryAt = claim.NextRetryAt
		}
	}

	if snap.LastStatus == "" {
		if snap.PendingCount > 0 {
			snap.LastStatus = "in_progress"
		} else {
			snap.LastStatus = "idle"
		}
	}

	return snap
}

// UpdateConvictionStopLoss updates the live conviction token stop-loss in cents.
// Safe to call from a background goroutine (the auto-tuner) while the trading
// loop reads it via convictionStopLossCents().
func (t *Trader) UpdateConvictionStopLoss(cents float64) {
	atomic.StoreUint64(&t.convictionStopLossAtomicCents, math.Float64bits(cents))
}

// convictionStopLossCents returns the current live conviction stop-loss in cents.
// Reads the atomically maintained value set by UpdateConvictionStopLoss.
func (t *Trader) convictionStopLossCents() float64 {
	return math.Float64frombits(atomic.LoadUint64(&t.convictionStopLossAtomicCents))
}

func isExternalReconcileRecord(rec TradeRecord) bool {
	if strings.EqualFold(strings.TrimSpace(rec.Strategy), "external_reconcile") {
		return true
	}
	reason := strings.ToLower(strings.TrimSpace(rec.Reason))
	return strings.HasPrefix(reason, "external_closed_position_reconcile:")
}

// appendJournalLine records a completed trade to the in-memory session journal
// and, if JournalFile is configured, appends a JSON line to the file on disk.
// File writes are fire-and-forget (errors are logged but do not abort trading).
func (t *Trader) appendJournalLine(rec TradeRecord) {
	externalRecord := isExternalReconcileRecord(rec)
	if !externalRecord {
		t.journal = append(t.journal, rec)

		// Update session-level risk counters.
		t.sessionPnL += rec.PnL
		t.totalTrades++
		if rec.PnL < 0 {
			t.consecutiveLosses++
		} else {
			t.consecutiveLosses = 0
		}
	}
	if t.cfg.JournalFile == "" {
		return
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.logger.Warn("journal: marshal failed", zap.Error(err))
		return
	}
	f, err := os.OpenFile(t.cfg.JournalFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.logger.Warn("journal: open file failed",
			zap.String("path", t.cfg.JournalFile),
			zap.Error(err),
		)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))

	// External reconciliation rows are historical corrections, not live trades.
	if !externalRecord && t.OnTradeClose != nil {
		t.OnTradeClose(rec)
	}
}

func pairArbJournalSideLabel(side string) string {
	if strings.EqualFold(side, "PAIR") {
		return "PAIR_MATCHED"
	}
	side = strings.ToUpper(strings.TrimSpace(side))
	if side == "YES" || side == "NO" {
		return "PAIR_RESIDUAL_" + side
	}
	return side
}

// IsFlat returns true when there is no open single-leg or paired position.
// HasPairArbPosition returns true when an active pair-arb position is held.
// Safe to call without a lock (same pattern as IsFlat).
func (t *Trader) HasPairArbPosition() bool { return t.pairedPosition != nil }

// PairArbPositionConditionID returns the condition ID of the active pair-arb
// position, or "" when no position is held. Used to detect cross-window
// true-pre-open positions (whose conditionID belongs to the next market).
func (t *Trader) PairArbPositionConditionID() string {
	if t.pairedPosition == nil {
		return ""
	}
	return t.pairedPosition.ConditionID
}

// marketTypeVariant converts a window duration to the Polymarket crypto-price
// API variant string used by FetchResolution.
func marketTypeVariant(dur time.Duration) string {
	switch {
	case dur >= 60*time.Minute:
		return "onehour"
	case dur >= 15*time.Minute:
		return "fifteen"
	default:
		return "fiveminute"
	}
}

// IsFlat returns true when the trader holds no open position (conviction or pair-arb).
func (t *Trader) IsFlat() bool { return t.position == nil && t.pairedPosition == nil }

func (t *Trader) HasPairedPosition() bool { return t.pairedPosition != nil }

// BuyInProgress returns true while a buy attempt is in-flight (PlaceOrder Ã¢â€ â€™ fill poll).
func (t *Trader) BuyInProgress() bool { return t.buyInProgress }

// HardResetForNewWindow drops any carried transient state at second 0 of a new
// market window so execution always starts from a clean slate.
func (t *Trader) HardResetForNewWindow(windowEnd time.Time) {
	now := time.Now()
	cleared := false

	if t.pendingBuy != nil {
		t.logger.Warn("trader: dropping carried pending buy at window start",
			zap.String("order_id", t.pendingBuy.OrderID),
		)
		_ = t.cancelStaleOrderAtWindowReset(t.pendingBuy.OrderID, "pending_buy")
		t.pendingBuy = nil
		cleared = true
	}
	if t.pendingPairArb != nil {
		pendingReq := strings.TrimSpace(t.pendingPairArb.RequestName)
		if (isPendingHedgeCreditProbe(pendingReq) || isPendingHedgeBuy(pendingReq)) && t.pairedPosition != nil {
			t.logger.Info("trader: preserving pending pair-arb hedge reconciliation state for carried pair position",
				zap.String("order_id", t.pendingPairArb.OrderID),
				zap.String("request", t.pendingPairArb.RequestName),
			)
		} else {
			t.logger.Warn("trader: dropping carried pending pair arb order at window start",
				zap.String("order_id", t.pendingPairArb.OrderID),
				zap.String("request", t.pendingPairArb.RequestName),
			)
			t.watchIsolatedPendingOrder(t.pendingPairArb, "window_start_reset")
			_ = t.cancelStaleOrderAtWindowReset(t.pendingPairArb.OrderID, t.pendingPairArb.RequestName)
			t.pendingPairArb = nil
			cleared = true
		}
	}
	if t.pendingHedgePrePlace != nil {
		// True pre-open positions place the hedge on the NEXT window's market (different
		// conditionID). HardResetForNewWindow is called before SetMarketTokens, so
		// t.convConditionID is still the OLD window's ID here. If the carried pair
		// position's conditionID differs, this hedge belongs to the new window — preserve it.
		isCrossWindowHedge := t.pairedPosition != nil &&
			!strings.EqualFold(strings.TrimSpace(t.pairedPosition.ConditionID), strings.TrimSpace(t.convConditionID))
		if isCrossWindowHedge {
			t.logger.Info("trader: preserving pre-placed hedge for cross-window true pre-open position",
				zap.String("order_id", t.pendingHedgePrePlace.OrderID),
				zap.String("pair_condition_id", t.pairedPosition.ConditionID),
				zap.String("old_condition_id", t.convConditionID),
			)
		} else {
			t.logger.Warn("trader: dropping carried pre-placed hedge at window start",
				zap.String("order_id", t.pendingHedgePrePlace.OrderID),
				zap.String("request", t.pendingHedgePrePlace.RequestName),
			)
			t.watchIsolatedPendingOrder(t.pendingHedgePrePlace, "window_start_prehedge_reset")
			_ = t.cancelStaleOrderAtWindowReset(t.pendingHedgePrePlace.OrderID, t.pendingHedgePrePlace.RequestName)
			t.pendingHedgePrePlace = nil
			cleared = true
		}
	}
	if t.position != nil {
		t.logger.Warn("trader: dropping carried single-leg position at window start",
			zap.String("order_id", t.position.OrderID),
			zap.Time("position_window_end", t.position.WindowEnd),
		)
		t.position = nil
		cleared = true
	}
	if t.pairedPosition != nil {
		// Keep unresolved pair positions across window rollover so exit fill
		// confirmation and close journaling can complete in the next window.
		t.logger.Info("trader: preserving carried pair position at window start",
			zap.Time("pair_window_end", t.pairedPosition.WindowEnd),
			zap.String("pair_condition_id", t.pairedPosition.ConditionID),
			zap.String("yes_exit_order_id", t.pairedPosition.YesExitOrderID),
			zap.String("no_exit_order_id", t.pairedPosition.NoExitOrderID),
		)
	}

	if t.buyInProgress {
		t.logger.Warn("trader: clearing carried buyInProgress gate at window start")
		t.buyInProgress = false
		cleared = true
	}
	if !t.pairArbRetryAfter.IsZero() {
		t.pairArbRetryAfter = time.Time{}
		cleared = true
	}
	if !t.pairArbForceCloseRetryAfter.IsZero() {
		t.pairArbForceCloseRetryAfter = time.Time{}
		cleared = true
	}
	if t.pairArbHedgeOrphanFlag {
		t.pairArbHedgeOrphanFlag = false
		cleared = true
	}

	t.pairExitMu.Lock()
	if t.pairExitManageInFlight {
		t.pairExitManageInFlight = false
		cleared = true
	}
	t.pairExitMu.Unlock()

	t.pairLeadEntryMu.Lock()
	if t.pairLeadEntryInFlight {
		t.pairLeadEntryInFlight = false
		cleared = true
	}
	t.pairLeadEntryMu.Unlock()

	t.pairRebalanceMu.Lock()
	if t.pairRebalanceInFlight {
		t.pairRebalanceInFlight = false
		cleared = true
	}
	t.pairRebalanceMu.Unlock()

	if cleared {
		t.savePositionState()
		t.logger.Info("trader: hard reset applied at new window boundary",
			zap.Time("window_end", windowEnd),
			zap.Time("applied_at", now),
		)
	}
}

// SetMarketTokens records the YES token ID, NO token ID, and condition ID for the current market window.
// Must be called once per window before runWindowLoop so flipConvictionPosition knows which token
// to buy on a BTC-reversal flip, and so SettleExpiredPosition can redeem via CTF if needed.
func (t *Trader) SetMarketTokens(yesTokenID, noTokenID, conditionID string) {
	if conditionID != "" && t.convConditionID != "" && !strings.EqualFold(conditionID, t.convConditionID) && t.pendingPairArb != nil {
		pendingReq := strings.TrimSpace(t.pendingPairArb.RequestName)
		if (isPendingHedgeCreditProbe(pendingReq) || isPendingHedgeBuy(pendingReq)) && t.pairedPosition != nil {
			t.logger.Info("trader: preserving pending pair-arb hedge reconciliation state on market rollover",
				zap.String("old_condition_id", t.convConditionID),
				zap.String("new_condition_id", conditionID),
				zap.String("order_id", t.pendingPairArb.OrderID),
				zap.String("request", t.pendingPairArb.RequestName),
			)
		} else {
			t.logger.Warn("trader: clearing pending pair arb order on market rollover",
				zap.String("old_condition_id", t.convConditionID),
				zap.String("new_condition_id", conditionID),
				zap.String("order_id", t.pendingPairArb.OrderID),
			)
			t.watchIsolatedPendingOrder(t.pendingPairArb, "market_rollover_reset")
			_ = t.cancelStaleOrderAtWindowReset(t.pendingPairArb.OrderID, t.pendingPairArb.RequestName)
			t.pendingPairArb = nil
			t.savePositionState()
		}
	}
	t.convYesTokenID = yesTokenID
	t.convNoTokenID = noTokenID
	t.convConditionID = conditionID
}

// riskBlocked returns (true, reason) when session-level risk controls prevent
// opening any new position.  It must be called before every entry attempt.
func (t *Trader) riskBlocked() (bool, string) {
	if t.cfg.MaxSessionLossUSD > 0 && t.sessionPnL <= -t.cfg.MaxSessionLossUSD {
		return true, fmt.Sprintf(
			"drawdown cap hit: session P&L $%.2f Ã‚Â°Ã‚Â¤ -$%.2f",
			t.sessionPnL, t.cfg.MaxSessionLossUSD,
		)
	}
	if t.cfg.MaxSessionProfitUSD > 0 && t.sessionPnL >= t.cfg.MaxSessionProfitUSD {
		return true, fmt.Sprintf(
			"profit cap hit: session P&L $%.2f Ã¢â€°Â¥ $%.2f",
			t.sessionPnL, t.cfg.MaxSessionProfitUSD,
		)
	}
	if t.cfg.MaxTradesPerSession > 0 && t.totalTrades >= t.cfg.MaxTradesPerSession {
		return true, fmt.Sprintf(
			"position limit: %d/%d trades completed this session",
			t.totalTrades, t.cfg.MaxTradesPerSession,
		)
	}
	if t.cfg.MaxConsecutiveLosses > 0 && t.consecutiveLosses >= t.cfg.MaxConsecutiveLosses {
		return true, fmt.Sprintf(
			"consecutive loss gate: %d losses in a row (limit %d)",
			t.consecutiveLosses, t.cfg.MaxConsecutiveLosses,
		)
	}
	return false, ""
}

// IsSessionHalted returns true when the session-loss cap has been breached.
// The main loop checks this after each window to stop trading cleanly.
func (t *Trader) IsSessionHalted() bool {
	if t.cfg.MaxSessionLossUSD > 0 && t.sessionPnL <= -t.cfg.MaxSessionLossUSD {
		return true
	}
	if t.cfg.MaxSessionProfitUSD > 0 && t.sessionPnL >= t.cfg.MaxSessionProfitUSD {
		return true
	}
	return false
}

// PaperBalance returns the current simulated USD balance.
// In live mode, it returns 0 to avoid showing paper balances in the UI.
func (t *Trader) PaperBalance() float64 {
	if !t.cfg.PaperTrade {
		return 0
	}
	return t.paperBalance
}

// CurrentBalance returns the balance appropriate for the active runtime.
// PAPER uses the simulated balance; LIVE uses the latest authenticated
// exchange balance cached by RefreshLiveBalance.
func (t *Trader) CurrentBalance() float64 {
	if t.cfg.PaperTrade {
		return t.paperBalance
	}
	return math.Float64frombits(atomic.LoadUint64(&t.cachedLiveBalance))
}

// SetFeeRate stores the taker fee for the current window's tokens.
// Must be called once per window after discovering the market.
func (t *Trader) SetFeeRate(bps string) {
	t.feeRateBps = bps
	t.logger.Debug("trader: fee rate set", zap.String("fee_rate_bps", bps))
}

func (t *Trader) FeeRateBps() string {
	return t.feeRateBps
}

// OnPolyPrice is called on every new Polymarket price event.
// It checks if the position has hit the profit target or timed out.
func (t *Trader) OnPolyPrice(ctx context.Context, yesPrice float64, noPrice float64) error {
	if t.pairedPosition != nil {
		return t.managePairArbPosition(ctx, yesPrice, noPrice, 0)
	}
	return nil
}

// closePosition places the exit SELL order and clears the position state.
func (t *Trader) closePosition(ctx context.Context, currentPrice float64, reason string) error {
	pos := t.position

	// Prevent double-sell: once a sell has been submitted, don't fire another.
	if pos.SellPending {
		t.logger.Warn("trader: sell already pending, skipping duplicate", zap.String("reason", reason))
		return nil
	}
	// Rate-limit retries: if a previous attempt failed recently, wait before re-trying
	// to avoid flooding the CLOB with repeated 400 errors every 500ms.
	if !pos.SellDelayUntil.IsZero() && time.Now().Before(pos.SellDelayUntil) {
		return nil
	}

	side := "YES"
	if pos.IsNoSide {
		side = "NO"
	}
	strategyForJournal := journalStrategyForPosition(pos)

	sellPrice := math.Round((currentPrice-0.01)*100) / 100
	if pos.IsResolutionSnipe {
		// Resolution snipes must NOT markdown 1: the entire profit margin between
		// buy price (~0.930.96) and the 0.99 target is precious, and near expiry
		// the token is converging to $1.00  placing at currentPrice (not -1) gives
		// the order the best chance of filling at or above the target.
		sellPrice = math.Round(currentPrice*100) / 100
	}
	if sellPrice <= 0 {
		sellPrice = 0.01
	}
	clearResidualDust := func(trigger string, shares float64) error {
		t.logger.Warn("trader: residual position below minimum order size; clearing tracked position",
			zap.String("reason", reason),
			zap.String("trigger", trigger),
			zap.Float64("remaining_shares", shares),
			zap.Float64("min_order_shares", polymarket.MinOrderShares),
		)
		pos.SellPending = false
		pos.ActiveSellOrderID = ""
		t.position = nil
		t.detector.SetInPosition(false)
		t.savePositionState()
		return nil
	}

	// Always start the sell ladder at the current market price regardless of P&L.
	// attemptSellWithFallback steps down: sellPrice Ã‚Â  sellPriceÃƒâ€¹Ã¢â‚¬Â 2 Ã‚Â  0.01 (floor).
	// Jumping straight to 0.01 collapses all three price tiers into one.
	worstPrice := sellPrice

	priceStr := fmt.Sprintf("%.2f", worstPrice)

	// Conviction: cancel the resting GTC sell BEFORE querying the balance.
	// The GTC order escrows shares on the CLOB; querying balance while the GTC is
	// live returns only the free (non-locked) shares, which can be significantly
	// less than the actual position size. Cancelling first  and waiting briefly
	// for the CLOB to release the lock  ensures resolveSellableShares sees the
	// full position.
	if pos.IsConviction && pos.ActiveSellOrderID != "" {
		if cancelErr := t.cancelOrderTimed(ctx, "conviction_sell_cancel", pos.ActiveSellOrderID); cancelErr != nil {
			t.logger.Warn("trader: conviction GTC cancel error (proceeding with market sell)",
				zap.String("order_id", pos.ActiveSellOrderID),
				zap.Error(cancelErr),
			)
		} else {
			t.logger.Info("trader: conviction GTC sell cancelled, switching to market sell",
				zap.String("order_id", pos.ActiveSellOrderID),
				zap.String("reason", reason),
			)
			// Brief pause: let the CLOB process the cancel and unlock the escrowed
			// tokens before we query the available balance. Without this, the balance
			// API still shows the locked shares as unavailable (causing a silent clamp
			// that leaves shares stranded on the shelf).
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(400 * time.Millisecond):
			}
		}
		pos.ActiveSellOrderID = ""
	}

	// Sync sell size against live conditional-token balance/allowance.
	// In volatile markets, estimated buy shares can differ from settled shares,
	// causing SELL to fail with "not enough balance / allowance".
	// Paper mode: tokens are never minted on-chain  skip the balance API entirely
	// to avoid a 5-second blocking poll that stalls the main event loop.
	// Use math.MaxFloat64 so the full on-chain balance (including stranded shares
	// from any previous session on this token) is sold, not just pos.Shares.
	sellShares := pos.Shares
	if !t.cfg.PaperTrade {
		var syncErr error
		sellShares, syncErr = t.resolveSellableShares(ctx, pos.TokenID, math.MaxFloat64)
		if syncErr != nil {
			if pos.Shares > 0 && pos.Shares < polymarket.MinOrderShares {
				return clearResidualDust("pre_sell_sync", pos.Shares)
			}
			t.logger.Warn("trader: unable to pre-sync sellable shares; using tracked position size",
				zap.Error(syncErr),
				zap.Float64("tracked_shares", pos.Shares),
			)
			sellShares = pos.Shares
		}
		if sellShares <= 0 {
			pos.SellPending = false
			// Shares not yet visible in balance API (common if settlement is delayed).
			// Back off for 2s to avoid hammering the API on every 500ms tick.
			pos.SellDelayUntil = time.Now().Add(2 * time.Second)
			t.savePositionState()
			return fmt.Errorf("place sell order: no sellable conditional token balance available (will retry in 2s)")
		}
		if sellShares < pos.Shares {
			// Do NOT clamp pos.Shares here. Sell only what's available now; the
			// residual stays in the position and will be retried on the next tick.
			t.logger.Warn("trader: partial balance available Ã¯Â¿Â½ will sell available shares, keep residual",
				zap.Float64("tracked_shares", pos.Shares),
				zap.Float64("sellable_shares", sellShares),
			)
		}
	}

	// sharesStr uses sellShares (not pos.Shares) so we only sell what's settled.
	sharesStr := fmt.Sprintf("%.2f", sellShares)

	// Compute sell fee for PnL accuracy: sell fee is deducted in USDC
	sellFeeUSD := polymarket.ComputeSellFeeUSDC(pos.Shares, sellPrice, t.feeRateBps)
	grossPnL := pos.Unrealized(currentPrice)
	netPnL := grossPnL - sellFeeUSD

	t.logger.Debug("trader: closing position",
		zap.String("reason", reason),
		zap.String("sell_price", priceStr),
		zap.Float64("shares", pos.Shares),
		zap.Float64("gross_pnl", grossPnL),
		zap.Float64("sell_fee_usd", sellFeeUSD),
		zap.Float64("net_pnl", netPnL),
		zap.Bool("paper", t.cfg.PaperTrade),
	)

	if t.cfg.PaperTrade {
		// Credit proceeds back to paper balance: shares  sellPrice - sellFee
		proceeds := pos.Shares*currentPrice - sellFeeUSD
		t.paperBalance += proceeds
		t.logger.Debug("trader: PAPER position closed",
			zap.Float64("net_pnl_usd", netPnL),
			zap.Float64("buy_price", pos.BuyPrice),
			zap.Float64("sell_price", currentPrice),
			zap.Float64("shares", pos.Shares),
			zap.Float64("fee_shares_buy", pos.FeeShares),
			zap.Float64("fee_usd_sell", sellFeeUSD),
			zap.Float64("proceeds", proceeds),
			zap.Float64("paper_balance", t.paperBalance),
			zap.String("reason", reason),
		)
		closedAt := time.Now()
		cfgScalpCents := t.cfg.ScalpTargetCents
		if pos.IsConviction {
			cfgScalpCents = t.cfg.ConvictionScalpTargetCents
		}
		t.appendJournalLine(TradeRecord{
			OpenedAt:                  pos.OpenedAt,
			ClosedAt:                  closedAt,
			HeldSec:                   closedAt.Sub(pos.OpenedAt).Seconds(),
			Strategy:                  strategyForJournal,
			Side:                      side,
			EntryBTCPrice:             pos.EntryBTCPrice,
			EntryEdgeUSD:              pos.EntryEdgeUSD,
			EntryCLLagUSD:             pos.EntryCLLagUSD,
			EntryATR:                  pos.EntryATR,
			EntryWindowRemSec:         pos.EntryWindowRemSec,
			EntryWinProb:              pos.EntryWinProb,
			EntryOpenPrice:            pos.OpenPrice,
			BuyPrice:                  pos.BuyPrice,
			SellPrice:                 currentPrice,
			Shares:                    pos.Shares,
			USDSpent:                  pos.USDSpent,
			PnL:                       netPnL,
			Reason:                    reason,
			CfgMinEdgeUSD:             t.cfg.CfgMinEdgeUSD,
			CfgScalpTargetCents:       cfgScalpCents,
			CfgStopLossCents:          t.cfg.StopLossCents,
			CfgTradeSizeUSD:           t.cfg.TradeSizeUSD,
			CfgConvictionTradeSizeUSD: t.cfg.ConvictionTradeSizeUSD,
		})
		display.TradeClose(side, pos.BuyPrice, currentPrice, pos.Shares, netPnL, sellFeeUSD, reason, time.Since(pos.OpenedAt).Round(time.Second), true)
		// PostWinCooldown blocks same-window re-entry on the lag strategy only.
		// Conviction positions use their own lastConvictionAt cooldown and hold to expiry;
		// calling NoteWin would bleed a 60s blockedUntil into the next window.
		if !pos.IsConviction && (strings.HasPrefix(reason, "scalp_target") || strings.HasPrefix(reason, "quicksell")) {
			t.detector.NoteWin(time.Now())
		}
		t.position = nil
		t.detector.SetInPosition(false)
		t.savePositionState()
		return nil
	}

	// Mark sell pending BEFORE placing order to prevent races.
	pos.SellPending = true

	// Conviction: cancel the resting GTC sell before placing the market-sell at 0.01.
	sellStart := time.Now()
	actualFillPrice, filledShares, filled, sellErr := t.attemptSellWithFallback(ctx, pos.TokenID, worstPrice, sharesStr)
	// ActiveSellOrderID is cleared by attemptSellWithFallback on cancel/timeout;
	// ensure it's cleared here too in case of an early return path.
	pos.ActiveSellOrderID = ""
	if sellErr != nil {
		pos.SellPending = false
		t.savePositionState()
		return fmt.Errorf("place sell order: %w", sellErr)
	}
	if !filled {
		// attemptSellWithFallback already spent up to 25s retrying allowance errors.
		// If it still returned false, there's genuinely no liquidity right now.
		// Back off briefly and let the next tick retry.
		pos.SellPending = false
		pos.SellDelayUntil = time.Now().Add(500 * time.Millisecond)
		t.savePositionState()
		return fmt.Errorf("sell order not filled after all attempts; retrying in 500ms")
	}
	if filledShares <= pairArbShareDust {
		pos.SellPending = false
		pos.SellDelayUntil = time.Now().Add(500 * time.Millisecond)
		t.savePositionState()
		return fmt.Errorf("sell order returned no fill evidence; retrying in 500ms")
	}
	if filledShares > sellShares {
		filledShares = sellShares
	}

	// Recalculate PnL and fee using the actual fill price (not the trigger price).
	// Use a proportional cost basis when only part of the position was sold.
	soldFrac := filledShares / pos.Shares // 1.0 for a full close
	proRataUSDSpent := pos.USDSpent * soldFrac
	actualSellFeeUSD := polymarket.ComputeSellFeeUSDC(filledShares, actualFillPrice, t.feeRateBps)
	actualGrossPnL := filledShares*actualFillPrice - proRataUSDSpent
	actualNetPnL := actualGrossPnL - actualSellFeeUSD

	t.logger.Info("trader: live SELL completed",
		zap.String("side", side),
		zap.Float64("fill_price", actualFillPrice),
		zap.Float64("trigger_price", currentPrice),
		zap.Float64("net_pnl_usd", actualNetPnL),
		zap.String("reason", reason),
		zap.Int64("sell_ms", time.Since(sellStart).Milliseconds()),
	)
	closedAt := time.Now()
	cfgScalpCents := t.cfg.ScalpTargetCents
	if pos.IsConviction {
		cfgScalpCents = t.cfg.ConvictionScalpTargetCents
	}
	t.appendJournalLine(TradeRecord{
		OpenedAt:                  pos.OpenedAt,
		ClosedAt:                  closedAt,
		HeldSec:                   closedAt.Sub(pos.OpenedAt).Seconds(),
		Strategy:                  strategyForJournal,
		Side:                      side,
		EntryBTCPrice:             pos.EntryBTCPrice,
		EntryEdgeUSD:              pos.EntryEdgeUSD,
		EntryCLLagUSD:             pos.EntryCLLagUSD,
		EntryATR:                  pos.EntryATR,
		EntryWindowRemSec:         pos.EntryWindowRemSec,
		EntryWinProb:              pos.EntryWinProb,
		EntryOpenPrice:            pos.OpenPrice,
		BuyPrice:                  pos.BuyPrice,
		SellPrice:                 actualFillPrice,
		Shares:                    filledShares,    // shares actually sold this fill
		USDSpent:                  proRataUSDSpent, // proportional cost for this tranche
		PnL:                       actualNetPnL,
		Reason:                    reason,
		CfgMinEdgeUSD:             t.cfg.CfgMinEdgeUSD,
		CfgScalpTargetCents:       cfgScalpCents,
		CfgStopLossCents:          t.cfg.StopLossCents,
		CfgTradeSizeUSD:           t.cfg.TradeSizeUSD,
		CfgConvictionTradeSizeUSD: t.cfg.ConvictionTradeSizeUSD,
	})
	display.TradeClose(side, pos.BuyPrice, actualFillPrice, filledShares, actualNetPnL, actualSellFeeUSD, reason, time.Since(pos.OpenedAt).Round(time.Second), false)

	// Partial sell: residual shares remain on-chain Ã¯Â¿Â½ keep the position open and
	// retry on the next price tick (after a brief hold-off).
	if filledShares < pos.Shares-0.005 {
		pos.Shares -= filledShares
		pos.USDSpent -= proRataUSDSpent
		pos.SellPending = false
		if pos.Shares > 0 && pos.Shares < polymarket.MinOrderShares {
			return clearResidualDust("post_partial_sell", pos.Shares)
		}
		pos.SellDelayUntil = time.Now().Add(1 * time.Second)
		t.savePositionState()
		t.logger.Warn("trader: partial sell recorded Ã¯Â¿Â½ residual position kept open",
			zap.Float64("sold_shares", filledShares),
			zap.Float64("remaining_shares", pos.Shares),
			zap.Float64("remaining_usd_spent", pos.USDSpent),
		)
		return nil
	}

	// Full close.
	if !pos.IsConviction && (strings.HasPrefix(reason, "scalp_target") || strings.HasPrefix(reason, "quicksell")) {
		t.detector.NoteWin(time.Now())
	}
	t.position = nil
	t.detector.SetInPosition(false)
	t.savePositionState()
	return nil
}

// resolveSellableShares returns the currently sellable conditional-token shares,
// clamped to 2 decimals for order sizing.
// It uses CLOB balance/allowance and can trigger gasless approval when allowance is zero.
func (t *Trader) resolveSellableShares(ctx context.Context, tokenID string, desiredShares float64) (float64, error) {
	if desiredShares <= 0 {
		return 0, nil
	}

	shares := math.Floor(desiredShares*100) / 100
	if shares <= pairArbShareDust {
		return 0, nil
	}

	return shares, nil
}

// attemptSellWithFallback places a single GTC SELL at 0.01, sweeping all available bids
// highest-to-lowest in one round trip. Any unfilled remainder rests on the book at 0.01
// and will auto-fill as market makers arrive -- no per-tier round-trip penalty.
// startPrice is used only as a fallback for fill-price resolution.
// On "not enough balance/allowance" errors it retries in a tight loop (300ms gap) for up to
// sellAllowanceTimeout -- Polygon usually confirms the conditional approval within 2-8 seconds.
// Returns (actualFillPrice, true, nil) once the order is accepted, (0, false, nil) on persistent
// allowance failure, or (0, false, err) on a hard API/context error.
const sellAllowanceTimeout = 60 * time.Second

func (t *Trader) attemptSellWithFallback(
	ctx context.Context,
	tokenID string,
	startPrice float64,
	sharesStr string,
) (float64, float64, bool, error) {
	requestedShares, err := strconv.ParseFloat(strings.TrimSpace(sharesStr), 64)
	if err != nil || requestedShares <= pairArbShareDust {
		return 0, 0, false, nil
	}

	remaining := math.Floor(requestedShares*100) / 100
	if remaining <= pairArbShareDust {
		return 0, 0, false, nil
	}

	basePrice := math.Round(startPrice*100) / 100
	if basePrice <= 0 || basePrice >= 1.0 {
		return 0, 0, false, fmt.Errorf("Kalshi IOC sell invalid reference price %.4f", startPrice)
	}

	totalFilled := 0.0
	totalProceeds := 0.0

	// A few short IOC attempts are safer than a resting emergency order.
	// Each attempt can only reduce the existing Kalshi position.
	const maxAttempts = 4

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return 0, totalFilled, totalFilled > pairArbShareDust, err
		}

		remaining = math.Floor(remaining*100) / 100
		if remaining <= pairArbShareDust {
			break
		}

		sizeStr := strconv.FormatFloat(remaining, 'f', 2, 64)

		// Bounded IOC liquidation:
		// attempt 1 = current/reference price
		// attempts 2-4 = at most 1c, 2c, 3c below it.
		// Never turn an ordinary risk exit into a 1-cent fire sale.
		slippage := float64(attempt-1) * 0.01
		limitPrice := math.Round(math.Max(0.01, basePrice-slippage)*100) / 100

		resp, placeErr := t.placeOrderTimed(
			ctx,
			"sell_ioc",
			&polymarket.NewOrderRequest{
				OrderType:  polymarket.OrderTypeFAK,
				TokenID:    tokenID,
				Side:       polymarket.SideSell,
				Price:      fmt.Sprintf("%.2f", limitPrice),
				Size:       sizeStr,
				Nonce:      polymarket.MakeNonce(),
				FeeRateBps: t.feeRateBps,
			},
			zap.Int("attempt", attempt),
			zap.Float64("remaining_shares", remaining),
			zap.Float64("limit_price", limitPrice),
		)
		if placeErr != nil {
			return 0, totalFilled, totalFilled > pairArbShareDust, placeErr
		}
		if resp == nil || !resp.Success {
			errMsg := ""
			if resp != nil {
				errMsg = strings.TrimSpace(resp.ErrorMsg)
			}
			if errMsg == "" {
				errMsg = "Kalshi IOC sell rejected"
			}
			return 0, totalFilled, totalFilled > pairArbShareDust, fmt.Errorf("%s", errMsg)
		}

		orderID := strings.TrimSpace(resp.OrderID)
		if orderID == "" {
			return 0, totalFilled, totalFilled > pairArbShareDust,
				fmt.Errorf("Kalshi IOC sell accepted without order id")
		}

		// IOC is already terminal after exchange processing. We only poll
		// briefly to accommodate Kalshi's order-visibility delay.
		deadline := time.Now().Add(2500 * time.Millisecond)

		attemptFilled := 0.0
		attemptAvg := 0.0

		for {
			if err := ctx.Err(); err != nil {
				return 0, totalFilled, totalFilled > pairArbShareDust, err
			}

			if gr, getErr := t.getOrderTimed(
				ctx,
				"sell_ioc_lookup",
				orderID,
				zap.Int("attempt", attempt),
			); getErr == nil && gr != nil {
				matched := parseOrderSize(gr.SizeMatched)
				if matched > attemptFilled {
					attemptFilled = matched
				}
			}

			if avg, gross, fillsErr := t.getFillsTimed(
				ctx,
				"sell_ioc_fills",
				orderID,
				tokenID,
				zap.Int("attempt", attempt),
			); fillsErr == nil {
				if gross > attemptFilled {
					attemptFilled = gross
				}
				if gross > pairArbShareDust && avg > 0 {
					attemptAvg = avg
				}
			}

			if attemptFilled > remaining {
				attemptFilled = remaining
			}

			if attemptFilled >= remaining-pairArbShareDust {
				break
			}

			if time.Now().After(deadline) {
				break
			}

			select {
			case <-ctx.Done():
				return 0, totalFilled, totalFilled > pairArbShareDust, ctx.Err()
			case <-time.After(150 * time.Millisecond):
			}
		}

		if attemptFilled > pairArbShareDust {
			if attemptAvg <= 0 {
				attemptAvg = startPrice
			}

			totalFilled += attemptFilled
			totalProceeds += attemptFilled * attemptAvg
			remaining = requestedShares - totalFilled

			t.logger.Info(
				"trader: Kalshi IOC emergency sell filled",
				zap.String("order_id", orderID),
				zap.Int("attempt", attempt),
				zap.Float64("filled_shares", attemptFilled),
				zap.Float64("avg_price", attemptAvg),
				zap.Float64("total_filled", totalFilled),
				zap.Float64("remaining_shares", math.Max(0, remaining)),
			)
		} else {
			t.logger.Warn(
				"trader: Kalshi IOC emergency sell produced no fill",
				zap.String("order_id", orderID),
				zap.Int("attempt", attempt),
				zap.Float64("remaining_shares", remaining),
			)
		}

		if remaining <= pairArbShareDust {
			break
		}

		if attempt < maxAttempts {
			select {
			case <-ctx.Done():
				return 0, totalFilled, totalFilled > pairArbShareDust, ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
	}

	if totalFilled <= pairArbShareDust {
		return 0, 0, false, nil
	}

	avgPrice := startPrice
	if totalProceeds > 0 {
		avgPrice = totalProceeds / totalFilled
	}

	if totalFilled > requestedShares {
		totalFilled = requestedShares
	}

	fullyFilled := totalFilled >= requestedShares-pairArbShareDust

	if !fullyFilled {
		t.logger.Warn(
			"trader: Kalshi IOC emergency sell left residual position",
			zap.Float64("requested_shares", requestedShares),
			zap.Float64("filled_shares", totalFilled),
			zap.Float64("remaining_shares", math.Max(0, requestedShares-totalFilled)),
		)
	}

	return avgPrice, totalFilled, true, nil
}

func isBalanceAllowanceError(errMsg string) bool {
	return false
}

func isTransientSellPlacementError(err error) bool {
	if err == nil {
		return false
	}

	// Kalshi has no Polymarket conditional-token allowance propagation.
	// Invalid-order HTTP 400 responses must not be retried indefinitely.
	errMsg := strings.ToLower(strings.TrimSpace(err.Error()))
	if errMsg == "" {
		return false
	}

	return isBalanceAllowanceError(errMsg)
}

func parseOrderSize(value string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return 0
	}
	return v
}

func (t *Trader) clearPairArbExitOrders(pair *PairArbPosition) {
	if pair == nil {
		return
	}
	t.clearPairExitOrderStatus(pair.YesExitOrderID)
	t.clearPairExitOrderStatus(pair.NoExitOrderID)
	pair.YesExitOrderID = ""
	pair.NoExitOrderID = ""
	pair.YesExitOrderShares = 0
	pair.NoExitOrderShares = 0
	pair.ExitOrdersPlacedAt = time.Time{}
	pair.LastExitPollAt = time.Time{}
}

func (t *Trader) setPairExitState(pair *PairArbPosition, state string, note string) {
	if pair == nil {
		return
	}
	if pair.ExitState == state && pair.ExitStateNote == note {
		return
	}
	pair.ExitState = state
	pair.ExitStateNote = note
	pair.ExitStateUpdatedAt = time.Now()
	t.logger.Info("trader: pair arb exit state updated",
		zap.String("condition_id", pair.ConditionID),
		zap.String("state", state),
		zap.String("note", note),
	)
	t.savePositionState()
}

func (t *Trader) tryBeginPairExitManagement() bool {
	t.pairExitMu.Lock()
	defer t.pairExitMu.Unlock()
	if t.pairExitManageInFlight {
		return false
	}
	t.pairExitManageInFlight = true
	return true
}

func (t *Trader) finishPairExitManagement() {
	t.pairExitMu.Lock()
	t.pairExitManageInFlight = false
	t.pairExitMu.Unlock()
}

func (t *Trader) isPairExitManagementInFlight() bool {
	t.pairExitMu.Lock()
	inFlight := t.pairExitManageInFlight
	t.pairExitMu.Unlock()
	return inFlight
}

func (t *Trader) tryBeginPairLeadEntry() bool {
	t.pairLeadEntryMu.Lock()
	defer t.pairLeadEntryMu.Unlock()
	if t.pairLeadEntryInFlight {
		return false
	}
	t.pairLeadEntryInFlight = true
	return true
}

func (t *Trader) finishPairLeadEntry() {
	t.pairLeadEntryMu.Lock()
	t.pairLeadEntryInFlight = false
	t.pairLeadEntryMu.Unlock()
}

func (t *Trader) tryBeginPairRebalance() bool {
	t.pairRebalanceMu.Lock()
	defer t.pairRebalanceMu.Unlock()
	if t.pairRebalanceInFlight {
		return false
	}
	t.pairRebalanceInFlight = true
	return true
}

func (t *Trader) finishPairRebalance() {
	t.pairRebalanceMu.Lock()
	t.pairRebalanceInFlight = false
	t.pairRebalanceMu.Unlock()
}

func pairWindowDuration(windowEnd time.Time) time.Duration {
	switch ts := windowEnd.Unix(); {
	case ts%(60*60) == 0:
		return 60 * time.Minute
	case ts%(15*60) == 0:
		return 15 * time.Minute
	default:
		return 5 * time.Minute
	}
}

func (t *Trader) resolvePairArbResolvedYes(ctx context.Context, pair *PairArbPosition, fallbackResolvedYes bool) bool {
	if pair == nil || pair.ConditionID == "" || t.orders == nil {
		return fallbackResolvedYes
	}

	resolvedYes, known, err := t.orders.ResolveMarket(ctx, pair.ConditionID)
	if err != nil {
		t.logger.Warn(
			"trader: Kalshi pair resolution unavailable; using fallback",
			zap.String("condition_id", pair.ConditionID),
			zap.Error(err),
		)
		return fallbackResolvedYes
	}

	if !known {
		return fallbackResolvedYes
	}

	return resolvedYes
}

func (t *Trader) placePairArbSellAt99WithRetry(ctx context.Context, conditionID string, side string, tokenID string, desiredShares float64) (string, float64, string) {
	if tokenID == "" || desiredShares < polymarket.MinOrderShares {
		return "", 0, ""
	}

	deadline := time.Now().Add(pairArbSellRetryWindow)
	lastErr := ""

	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", 0, err.Error()
		}

		sellable, sellableErr := t.resolveSellableShares(ctx, tokenID, desiredShares)
		if sellableErr != nil {
			lastErr = fmt.Sprintf("resolveSellableShares: %v", sellableErr)
		} else if sellable >= polymarket.MinOrderShares {
			sharesStr := fmt.Sprintf("%.2f", sellable)
			t.logger.Info("trader: pair arb sell-at-99: placing GTC sell",
				zap.String("condition_id", conditionID),
				zap.String("side", side),
				zap.String("token_id", tokenID),
				zap.String("shares", sharesStr),
				zap.Int("attempt", attempt),
			)
			resp, sellErr := t.placeOrderTimed(ctx, "pair_arb_sell_99_"+strings.ToLower(side), &polymarket.NewOrderRequest{
				OrderType:  polymarket.OrderTypeGTC,
				TokenID:    tokenID,
				Side:       polymarket.SideSell,
				Price:      "0.99",
				Size:       sharesStr,
				Nonce:      polymarket.MakeNonce(),
				FeeRateBps: t.feeRateBps,
			})
			if sellErr == nil && resp != nil && resp.Success {
				logTimedActivityInfo(fmt.Sprintf("SELL GTC pair arb sell 99 %s OK order=%s", side, resp.OrderID))
				t.logger.Info("trader: pair arb sell-at-99: GTC sell placed",
					zap.String("order_id", resp.OrderID),
					zap.String("side", side),
					zap.String("condition_id", conditionID),
					zap.Int("attempt", attempt),
				)
				return resp.OrderID, sellable, ""
			}

			if sellErr != nil {
				lastErr = sellErr.Error()
			} else if resp != nil {
				lastErr = resp.ErrorMsg
			} else {
				lastErr = "nil sell response"
			}
		} else {
			lastErr = fmt.Sprintf("sellable shares %.2f below minimum order size %.2f", sellable, polymarket.MinOrderShares)
		}

		if !isBalanceAllowanceError(lastErr) && !strings.Contains(strings.ToLower(lastErr), "sellable shares") {
			return "", 0, lastErr
		}
		if time.Now().After(deadline) {
			return "", 0, lastErr
		}

		select {
		case <-ctx.Done():
			return "", 0, ctx.Err().Error()
		case <-time.After(pairArbSellRetryInterval):
		}
	}
}

func (t *Trader) pairArbExitOrderFilled(ctx context.Context, orderID, tokenID string, orderedShares float64, requestName string) (bool, bool, error) {
	if orderID == "" {
		return false, false, nil
	}
	gr, err := t.getOrderTimed(ctx, requestName, orderID)
	if err != nil {
		return false, false, err
	}
	if gr == nil {
		return false, false, nil
	}
	status := strings.ToLower(strings.TrimSpace(gr.Status))
	if status == "canceled" {
		return false, true, nil
	}
	remaining := parseOrderSize(gr.SizeRemaining)
	matched := parseOrderSize(gr.SizeMatched)
	if remaining > 0 && remaining <= pairArbShareDust && matched > pairArbShareDust {
		return true, false, nil
	}
	if status == "matched" || status == "filled" {
		// Be strict here: status alone is not enough because some responses can have
		// incomplete size fields; require matched-size evidence.
		if orderedShares > 0 {
			if matched >= orderedShares-pairArbShareDust {
				return true, false, nil
			}
		} else if matched > pairArbShareDust && remaining <= pairArbShareDust {
			return true, false, nil
		}
	}
	if avg, gross, fillsErr := t.getFillsTimed(ctx, requestName+"_fills", orderID, tokenID); fillsErr == nil && avg > 0 {
		if orderedShares <= 0 || gross >= orderedShares-pairArbShareDust {
			return true, false, nil
		}
	}
	return false, false, nil
}

func (t *Trader) pairArbExitOrderCanceledOrExpired(ctx context.Context, orderID, requestName string) (bool, error) {
	if orderID == "" {
		return false, nil
	}
	gr, err := t.getOrderTimed(ctx, requestName, orderID)
	if err != nil {
		return false, err
	}
	if gr == nil {
		return false, nil
	}
	status := strings.ToLower(strings.TrimSpace(gr.Status))
	return status == "canceled" || status == "cancelled" || status == "expired", nil
}

func (t *Trader) setPairExitOrderStatus(orderID string, canceledOrExpired bool) {
	if orderID == "" {
		return
	}
	t.pairExitStatusMu.Lock()
	t.pairExitOrderStatus[orderID] = pairExitOrderStatusSnapshot{CanceledOrExpired: canceledOrExpired, UpdatedAt: time.Now()}
	t.pairExitStatusMu.Unlock()
}

func (t *Trader) getPairExitOrderStatus(orderID string) (canceledOrExpired bool, known bool) {
	if orderID == "" {
		return false, false
	}
	t.pairExitStatusMu.Lock()
	snap, ok := t.pairExitOrderStatus[orderID]
	t.pairExitStatusMu.Unlock()
	if !ok {
		return false, false
	}
	if time.Since(snap.UpdatedAt) > pairArbExitStatusTTL {
		return false, false
	}
	return snap.CanceledOrExpired, true
}

func (t *Trader) clearPairExitOrderStatus(orderID string) {
	if orderID == "" {
		return
	}
	t.pairExitStatusMu.Lock()
	delete(t.pairExitOrderStatus, orderID)
	t.pairExitStatusMu.Unlock()
}

func (t *Trader) schedulePairExitOrderStatusRefresh(pair *PairArbPosition) {
	if pair == nil {
		return
	}
	yesID := strings.TrimSpace(pair.YesExitOrderID)
	noID := strings.TrimSpace(pair.NoExitOrderID)
	if yesID == "" && noID == "" {
		return
	}
	t.pairExitStatusMu.Lock()
	if t.pairExitStatusRefreshActive {
		t.pairExitStatusMu.Unlock()
		return
	}
	t.pairExitStatusRefreshActive = true
	t.pairExitStatusMu.Unlock()

	go func(yesOrderID, noOrderID string) {
		defer func() {
			t.pairExitStatusMu.Lock()
			t.pairExitStatusRefreshActive = false
			t.pairExitStatusMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), pairArbExitStatusProbeTimeout)
		defer cancel()
		if yesOrderID != "" {
			yesCanceled, err := t.pairArbExitOrderCanceledOrExpired(ctx, yesOrderID, "pair_arb_sell_99_yes_bg_status")
			if err == nil {
				t.setPairExitOrderStatus(yesOrderID, yesCanceled)
			} else {
				t.logger.Debug("trader: pair arb sell-at-99 background YES status probe failed", zap.Error(err))
			}
		}
		if noOrderID != "" {
			noCanceled, err := t.pairArbExitOrderCanceledOrExpired(ctx, noOrderID, "pair_arb_sell_99_no_bg_status")
			if err == nil {
				t.setPairExitOrderStatus(noOrderID, noCanceled)
			} else {
				t.logger.Debug("trader: pair arb sell-at-99 background NO status probe failed", zap.Error(err))
			}
		}
	}(yesID, noID)
}

func (t *Trader) cancelPairArbExitOrder(ctx context.Context, orderID string, requestName string) error {
	if orderID == "" {
		return nil
	}
	err := t.cancelOrderTimed(ctx, requestName, orderID)
	if err != nil && !errors.Is(err, polymarket.ErrOrderNotCancellable) {
		return err
	}
	return nil
}

func (t *Trader) pairArbLegSettled(ctx context.Context, side string, tokenID string) (bool, float64, error) {
	if tokenID == "" {
		return false, 0, nil
	}
	const (
		settleProbeAttempts = 6
		settleProbeInterval = 500 * time.Millisecond
	)
	lastBal := 0.0
	lastErr := error(nil)
	for attempt := 1; attempt <= settleProbeAttempts; attempt++ {
		if ctx.Err() != nil {
			return false, lastBal, ctx.Err()
		}
		bal, err := t.tokenBalanceShares(ctx, tokenID)
		if err != nil {
			lastErr = err
		} else {
			lastBal = bal
			if bal <= pairArbSettleBalanceDust {
				return true, bal, nil
			}
		}
		if attempt < settleProbeAttempts {
			select {
			case <-ctx.Done():
				return false, lastBal, ctx.Err()
			case <-time.After(settleProbeInterval):
			}
		}
	}
	if lastErr != nil {
		t.logger.Warn("trader: pair arb sell-at-99 settlement probe error",
			zap.String("side", side),
			zap.String("token_id", tokenID),
			zap.Float64("last_balance", lastBal),
			zap.Error(lastErr),
		)
	}
	return false, lastBal, nil
}

func (t *Trader) pairArbFilledLegsSettled(ctx context.Context, pair *PairArbPosition, yesFilled, noFilled bool) (bool, error) {
	if pair == nil {
		return false, nil
	}
	if yesFilled {
		yesSettled, bal, err := t.pairArbLegSettled(ctx, "YES", pair.YesTokenID)
		if err != nil {
			return false, err
		}
		if !yesSettled {
			t.logger.Warn("trader: pair arb sell-at-99 YES leg reported filled but balance still present; waiting",
				zap.Float64("yes_balance", bal),
				zap.String("order_id", pair.YesExitOrderID),
			)
			return false, nil
		}
	}
	if noFilled {
		noSettled, bal, err := t.pairArbLegSettled(ctx, "NO", pair.NoTokenID)
		if err != nil {
			return false, err
		}
		if !noSettled {
			t.logger.Warn("trader: pair arb sell-at-99 NO leg reported filled but balance still present; waiting",
				zap.Float64("no_balance", bal),
				zap.String("order_id", pair.NoExitOrderID),
			)
			return false, nil
		}
	}
	return true, nil
}

func (t *Trader) resetUnsettledPairExitLegs(ctx context.Context, pair *PairArbPosition, yesFilled bool, noFilled bool) {
	if pair == nil {
		return
	}
	resetIfResidual := func(side string, tokenID string, filled bool, orderID *string, orderShares *float64, trackedShares *float64) {
		if !filled || tokenID == "" || orderID == nil || *orderID == "" {
			return
		}
		bal, err := t.tokenBalanceShares(ctx, tokenID)
		if err != nil {
			return
		}
		if bal <= pairArbSettleBalanceDust {
			return
		}
		if trackedShares != nil && bal > *trackedShares {
			*trackedShares = math.Floor(bal*100) / 100
		}
		t.clearPairExitOrderStatus(*orderID)
		*orderID = ""
		if orderShares != nil {
			*orderShares = 0
		}
		t.logger.Warn("trader: pair arb sell-at-99 residual inventory detected after fill; re-arming exit order",
			zap.String("side", side),
			zap.Float64("wallet_balance", bal),
		)
	}

	resetIfResidual("YES", pair.YesTokenID, yesFilled, &pair.YesExitOrderID, &pair.YesExitOrderShares, &pair.YesShares)
	resetIfResidual("NO", pair.NoTokenID, noFilled, &pair.NoExitOrderID, &pair.NoExitOrderShares, &pair.NoShares)
}

func (t *Trader) pairArbWalletResiduals(ctx context.Context, pair *PairArbPosition) (float64, float64, error) {
	if pair == nil {
		return 0, 0, nil
	}
	yesBal := 0.0
	if pair.YesTokenID != "" {
		bal, err := t.tokenBalanceShares(ctx, pair.YesTokenID)
		if err != nil {
			return 0, 0, err
		}
		yesBal = bal
	}
	noBal := 0.0
	if pair.NoTokenID != "" {
		bal, err := t.tokenBalanceShares(ctx, pair.NoTokenID)
		if err != nil {
			return 0, 0, err
		}
		noBal = bal
	}
	return yesBal, noBal, nil
}

func (t *Trader) pairArbWalletExitConfirmed(ctx context.Context, pair *PairArbPosition) (bool, float64, float64, error) {
	yesBal, noBal, err := t.pairArbWalletResiduals(ctx, pair)
	if err != nil {
		return false, yesBal, noBal, err
	}
	return yesBal <= pairArbSettleBalanceDust && noBal <= pairArbSettleBalanceDust, yesBal, noBal, nil
}

func (t *Trader) aggressiveRepostPairArbExitOrders(ctx context.Context, pair *PairArbPosition, remaining time.Duration) {
	if pair == nil || remaining <= 0 || remaining > pairArbSellAggressiveLead {
		return
	}
	if !pair.ExitOrdersPlacedAt.IsZero() && time.Since(pair.ExitOrdersPlacedAt) < 1200*time.Millisecond {
		return
	}
	yesBal, noBal, err := t.pairArbWalletResiduals(ctx, pair)
	if err != nil {
		t.logger.Warn("trader: pair arb final-3s aggressive exit probe failed", zap.Error(err))
		return
	}
	changed := false
	repost := func(side string, tokenID string, bal float64, orderID *string, orderShares *float64, trackedShares *float64) {
		if tokenID == "" || bal <= pairArbSettleBalanceDust {
			return
		}
		if trackedShares != nil && bal > *trackedShares {
			*trackedShares = math.Floor(bal*100) / 100
		}
		if orderID != nil && *orderID != "" {
			if err := t.cancelPairArbExitOrder(ctx, *orderID, "pair_arb_sell_99_aggressive_cancel_"+strings.ToLower(side)); err != nil {
				t.logger.Warn("trader: pair arb final-3s aggressive cancel failed",
					zap.String("side", side),
					zap.String("order_id", *orderID),
					zap.Error(err),
				)
			}
			t.clearPairExitOrderStatus(*orderID)
			*orderID = ""
			if orderShares != nil {
				*orderShares = 0
			}
			changed = true
		}
		if bal < polymarket.MinOrderShares {
			return
		}
		newOrderID, sellable, errMsg := t.placePairArbSellAt99WithRetry(ctx, pair.ConditionID, side, tokenID, bal)
		if newOrderID != "" {
			*orderID = newOrderID
			if orderShares != nil {
				*orderShares = sellable
			}
			changed = true
			return
		}
		if errMsg != "" {
			t.logger.Warn("trader: pair arb final-3s aggressive repost failed",
				zap.String("side", side),
				zap.Float64("wallet_balance", bal),
				zap.String("error", errMsg),
			)
		}
	}

	repost("YES", pair.YesTokenID, yesBal, &pair.YesExitOrderID, &pair.YesExitOrderShares, &pair.YesShares)
	repost("NO", pair.NoTokenID, noBal, &pair.NoExitOrderID, &pair.NoExitOrderShares, &pair.NoShares)
	if changed {
		pair.ExitOrdersPlacedAt = time.Now()
		t.setPairExitState(pair, pairExitStatePlacingOrders, "final-3s aggressive cancel/repost for residual shares")
		t.savePositionState()
	}
}

func (t *Trader) manageBalancedPairArbSellAt99(ctx context.Context, pair *PairArbPosition) error {
	if pair == nil {
		return nil
	}
	if !t.cfg.PairArbSellAt99 {
		if pair.YesExitOrderID != "" {
			if err := t.cancelPairArbExitOrder(ctx, pair.YesExitOrderID, "pair_arb_hold_claim_cancel_yes"); err != nil {
				t.logger.Warn("trader: hold-claim mode: cancel stale YES sell-at-99 order failed", zap.Error(err))
			}
		}
		if pair.NoExitOrderID != "" {
			if err := t.cancelPairArbExitOrder(ctx, pair.NoExitOrderID, "pair_arb_hold_claim_cancel_no"); err != nil {
				t.logger.Warn("trader: hold-claim mode: cancel stale NO sell-at-99 order failed", zap.Error(err))
			}
		}
		t.clearPairArbExitOrders(pair)
		t.setPairExitState(pair, pairExitStateClaimQueued, "hold-to-claim mode active; sell-at-99 disabled")
		t.savePositionState()
		return nil
	}
	t.reconcileLivePairArbState(ctx, pair, 0, 0)
	pair = t.pairedPosition
	if pair == nil {
		return nil
	}
	if !t.tryBeginPairExitManagement() {
		return nil
	}
	defer t.finishPairExitManagement()
	if t.cfg.PaperTrade {
		_, _, liveYes, _, _, _ := t.detector.Snapshot()
		liveNo := math.Round((1.0-liveYes)*100) / 100
		yesTouched := liveYes >= 0.99
		noTouched := liveNo >= 0.99
		if !yesTouched && !noTouched {
			t.setPairExitState(pair, pairExitStateOrdersLive, fmt.Sprintf("paper sell-at-99 waiting for touch (YES=%.2f NO=%.2f)", liveYes, liveNo))
			return nil
		}
		residualExitPrice := 0.0
		if residualSide, _, _ := pair.residualPosition(); residualSide != "" {
			residualExitPrice = 0.99
		}
		hitSide := "YES"
		if noTouched && !yesTouched {
			hitSide = "NO"
		}
		t.setPairExitState(pair, pairExitStateResolvedCLOB, "paper sell-at-99 touched by "+hitSide)
		t.closePairArbPosition(pair, 0.99, residualExitPrice, "pair_sell_at_99", "pair_sell_at_99")
		return nil
	}
	now := time.Now()
	if t.cfg.PairArbSellAt99 && !pair.WindowEnd.IsZero() {
		remaining := pair.WindowEnd.Sub(now)
		if remaining > pairArbSellArmLead {
			t.setPairExitState(pair, pairExitStateGraceWait, fmt.Sprintf("balanced; arming dual 0.99 sells in final %.0fs", pairArbSellArmLead.Seconds()))
			return nil
		}
		if remaining > 0 && remaining <= pairArbSellArmLead && (pair.YesExitOrderID == "" || pair.NoExitOrderID == "") {
			// Entering the final arming window: force one immediate placement pass
			// if either leg is still missing.
			if !pair.LastExitPollAt.IsZero() && now.Sub(pair.LastExitPollAt) < pairArbSellPollInterval {
				pair.LastExitPollAt = time.Time{}
			}
			t.logger.Warn("trader: pair arb sell-at-99 final window recheck: missing exit order; forcing placement",
				zap.Duration("window_remaining", remaining.Round(time.Second)),
				zap.String("yes_exit_order_id", pair.YesExitOrderID),
				zap.String("no_exit_order_id", pair.NoExitOrderID),
			)
		}
	}
	if !pair.LastExitPollAt.IsZero() && now.Sub(pair.LastExitPollAt) < pairArbSellPollInterval {
		return nil
	}
	pair.LastExitPollAt = now

	t.setPairExitState(pair, pairExitStatePlacingOrders, "verifying balances and placing dual 0.99 GTC sells")
	placedAny := false
	if pair.YesExitOrderID == "" {
		orderID, sellable, errMsg := t.placePairArbSellAt99WithRetry(ctx, pair.ConditionID, "YES", pair.YesTokenID, pair.YesShares)
		if orderID != "" {
			pair.YesExitOrderID = orderID
			pair.YesExitOrderShares = sellable
			placedAny = true
			if pair.ExitOrdersPlacedAt.IsZero() {
				pair.ExitOrdersPlacedAt = now
			}
			t.savePositionState()
		} else if errMsg != "" {
			t.setPairExitState(pair, pairExitStatePlacingOrders, "YES leg waiting for settled sellable balance")
			t.logger.Warn("trader: pair arb sell-at-99: YES order not ready; will retry",
				zap.String("condition_id", pair.ConditionID),
				zap.String("error", errMsg),
			)
		}
	}
	if pair.NoExitOrderID == "" {
		orderID, sellable, errMsg := t.placePairArbSellAt99WithRetry(ctx, pair.ConditionID, "NO", pair.NoTokenID, pair.NoShares)
		if orderID != "" {
			pair.NoExitOrderID = orderID
			pair.NoExitOrderShares = sellable
			placedAny = true
			if pair.ExitOrdersPlacedAt.IsZero() {
				pair.ExitOrdersPlacedAt = now
			}
			t.savePositionState()
		} else if errMsg != "" {
			t.setPairExitState(pair, pairExitStatePlacingOrders, "NO leg waiting for settled sellable balance")
			t.logger.Warn("trader: pair arb sell-at-99: NO order not ready; will retry",
				zap.String("condition_id", pair.ConditionID),
				zap.String("error", errMsg),
			)
		}
	}
	if placedAny || pair.YesExitOrderID != "" || pair.NoExitOrderID != "" {
		t.setPairExitState(pair, pairExitStateOrdersLive, fmt.Sprintf("YES=%s NO=%s", pair.YesExitOrderID, pair.NoExitOrderID))
	}

	// Waiting-mode policy: once both 0.99 GTC exits are live, wait for the market
	// window to complete. Keep checking that both exits still exist because the
	// exchange can cancel/expire one leg before close.
	if pair.YesExitOrderID != "" && pair.NoExitOrderID != "" {
		t.schedulePairExitOrderStatusRefresh(pair)
		yesCanceled, yesKnown := t.getPairExitOrderStatus(pair.YesExitOrderID)
		noCanceled, noKnown := t.getPairExitOrderStatus(pair.NoExitOrderID)
		if yesCanceled {
			t.clearPairExitOrderStatus(pair.YesExitOrderID)
			pair.YesExitOrderID = ""
			pair.YesExitOrderShares = 0
		}
		if noCanceled {
			t.clearPairExitOrderStatus(pair.NoExitOrderID)
			pair.NoExitOrderID = ""
			pair.NoExitOrderShares = 0
		}
		if (yesKnown && yesCanceled) || (noKnown && noCanceled) {
			t.setPairExitState(pair, pairExitStatePlacingOrders, "replacing canceled pair exit order")
			t.savePositionState()
			return nil
		}
		if !pair.WindowEnd.IsZero() && now.Before(pair.WindowEnd) {
			remaining := pair.WindowEnd.Sub(now)
			if remaining > 0 && remaining <= pairArbSellAggressiveLead {
				t.aggressiveRepostPairArbExitOrders(ctx, pair, remaining)
			}
			t.setPairExitState(pair, pairExitStateOrdersLive, "orders live; waiting for window completion")
			return nil
		}
		yesFilled, _, yesErr := t.pairArbExitOrderFilled(ctx, pair.YesExitOrderID, pair.YesTokenID, pair.YesExitOrderShares, "pair_arb_sell_99_yes_window_end")
		if yesErr != nil {
			return yesErr
		}
		noFilled, _, noErr := t.pairArbExitOrderFilled(ctx, pair.NoExitOrderID, pair.NoTokenID, pair.NoExitOrderShares, "pair_arb_sell_99_no_window_end")
		if noErr != nil {
			return noErr
		}
		if yesFilled || noFilled {
			settled, settleErr := t.pairArbFilledLegsSettled(ctx, pair, yesFilled, noFilled)
			if settleErr != nil {
				return settleErr
			}
			if !settled {
				t.resetUnsettledPairExitLegs(ctx, pair, yesFilled, noFilled)
				t.setPairExitState(pair, pairExitStateOrdersLive, "window complete; fill seen, awaiting settlement confirmation")
				t.savePositionState()
				return nil
			}
			walletFlat, yesBal, noBal, balErr := t.pairArbWalletExitConfirmed(ctx, pair)
			if balErr != nil {
				return balErr
			}
			if !walletFlat {
				t.setPairExitState(pair, pairExitStateOrdersLive, fmt.Sprintf("window complete; waiting full-size exit confirmation (YES=%.2f NO=%.2f)", yesBal, noBal))
				return nil
			}
			residualExitPrice := 0.0
			if residualSide, _, _ := pair.residualPosition(); residualSide != "" {
				residualExitPrice = 0.99
			}
			t.setPairExitState(pair, pairExitStateResolvedCLOB, "window complete; sell-at-99 fill confirmed")
			t.clearPairArbExitOrders(pair)
			t.closePairArbPosition(pair, 0.99, residualExitPrice, "pair_sell_at_99", "pair_sell_at_99")
			return nil
		}
		t.setPairExitState(pair, pairExitStateGraceWait, "window complete; exit fills not confirmed yet")
		return nil
	}

	yesFilled, yesCanceled, yesErr := t.pairArbExitOrderFilled(ctx, pair.YesExitOrderID, pair.YesTokenID, pair.YesExitOrderShares, "pair_arb_sell_99_yes_lookup")
	if yesErr != nil {
		return yesErr
	}
	noFilled, noCanceled, noErr := t.pairArbExitOrderFilled(ctx, pair.NoExitOrderID, pair.NoTokenID, pair.NoExitOrderShares, "pair_arb_sell_99_no_lookup")
	if noErr != nil {
		return noErr
	}
	if yesCanceled {
		pair.YesExitOrderID = ""
		pair.YesExitOrderShares = 0
	}
	if noCanceled {
		pair.NoExitOrderID = ""
		pair.NoExitOrderShares = 0
	}
	if yesCanceled || noCanceled {
		t.setPairExitState(pair, pairExitStatePlacingOrders, "replacing canceled pair exit order")
		t.savePositionState()
	}
	if !pair.WindowEnd.IsZero() && now.Before(pair.WindowEnd) {
		t.setPairExitState(pair, pairExitStateOrdersLive, "final-10s arming active; waiting for both exits or window completion")
		return nil
	}
	if !yesFilled && !noFilled {
		return nil
	}
	winningSide := "YES"
	if noFilled && !yesFilled {
		winningSide = "NO"
	}
	settled, settleErr := t.pairArbFilledLegsSettled(ctx, pair, yesFilled, noFilled)
	if settleErr != nil {
		return settleErr
	}
	if !settled {
		t.resetUnsettledPairExitLegs(ctx, pair, yesFilled, noFilled)
		t.setPairExitState(pair, pairExitStateOrdersLive, "fill reported; awaiting balance settlement confirmation")
		t.savePositionState()
		return nil
	}
	walletFlat, yesBal, noBal, balErr := t.pairArbWalletExitConfirmed(ctx, pair)
	if balErr != nil {
		return balErr
	}
	if !walletFlat {
		t.setPairExitState(pair, pairExitStateOrdersLive, fmt.Sprintf("waiting full-size exit confirmation (YES=%.2f NO=%.2f)", yesBal, noBal))
		return nil
	}
	t.setPairExitState(pair, pairExitStateWinnerFilled, winningSide+" exit order filled")

	if yesFilled && pair.NoExitOrderID != "" {
		t.setPairExitState(pair, pairExitStateCancelingLoser, "canceling NO exit order after YES fill")
		if err := t.cancelPairArbExitOrder(ctx, pair.NoExitOrderID, "pair_arb_sell_99_cancel_no"); err != nil {
			return err
		}
	}
	if noFilled && pair.YesExitOrderID != "" {
		t.setPairExitState(pair, pairExitStateCancelingLoser, "canceling YES exit order after NO fill")
		if err := t.cancelPairArbExitOrder(ctx, pair.YesExitOrderID, "pair_arb_sell_99_cancel_yes"); err != nil {
			return err
		}
	}
	residualExitPrice := 0.0
	if residualSide, _, _ := pair.residualPosition(); residualSide != "" {
		residualExitPrice = 0.99
	}
	t.setPairExitState(pair, pairExitStateResolvedCLOB, winningSide+" leg filled at 0.99; loser order canceled")
	t.clearPairArbExitOrders(pair)
	t.closePairArbPosition(pair, 0.99, residualExitPrice, "pair_sell_at_99", "pair_sell_at_99")
	return nil
}

// OnChainlinkPrice is called on every Chainlink price update.
// If we're winning (chainlink > open), we HOLD to maximize payout near $1.00.
// We only exit via chainlink if we're LOSING (chainlink moved against us while
// we expected YES to win) as a cut-loss signal.
func (t *Trader) OnChainlinkPrice(ctx context.Context, chainlinkPrice float64) error {
	if t.IsFlat() {
		return nil
	}
	// Chainlink above open = YES is winning = HOLD (don't sell).
	// We only bought YES, so this is always good for us.
	return nil
}

// CheckExpiry should be called periodically to force-close positions past their
// max hold time OR approaching the market window end.
// currentYesPrice is always the YES token price; converted internally for NO.
func (t *Trader) CheckExpiry(ctx context.Context, currentYesPrice float64, safeExitBuffer time.Duration) error {
	if t.pairedPosition != nil {
		return t.managePairArbPosition(ctx, currentYesPrice, 0, safeExitBuffer)
	}
	if t.IsFlat() {
		return nil
	}
	now := time.Now()
	pos := t.position
	if pos.IsConviction && !pos.WindowEnd.IsZero() && now.After(pos.WindowEnd.Add(2*time.Second)) {
		return t.abandonStaleConvictionPosition(currentYesPrice, "conviction_stale_abandoned")
	}

	// Check if we're on the winning side (direction-aware)
	btcLatest, _, _, openPrice, _, _ := t.detector.Snapshot()
	var winning bool
	if pos.IsNoSide {
		winning = btcLatest > 0 && openPrice > 0 && btcLatest < openPrice
	} else {
		winning = btcLatest > 0 && openPrice > 0 && btcLatest > openPrice
	}

	// For NO positions, derive the token price from YES price
	tokenPrice := currentYesPrice
	if pos.IsNoSide {
		tokenPrice = math.Round((1.0-currentYesPrice)*100) / 100
	}

	// Near window end: if winning, quicksell at 0.99 rather than dumping at market
	if !pos.WindowEnd.IsZero() && now.Add(safeExitBuffer).After(pos.WindowEnd) {
		if pos.IsResolutionSnipe {
			if !winning {
				return t.closePosition(ctx, tokenPrice,
					fmt.Sprintf("resolution_snipe_losing_end (rem=%.0fs)", time.Until(pos.WindowEnd).Seconds()))
			}
			// Winning resolution snipe: sell at 0.99 now rather than waiting for CTF
			// settlement (which takes 1-2 minutes after window close).
			return t.closePosition(ctx, 0.99,
				fmt.Sprintf("resolution_snipe_winning_sell99 (btc=%.2f, open=%.2f, rem=%.0fs)",
					btcLatest, openPrice, time.Until(pos.WindowEnd).Seconds()))
		}
		// Conviction positions manage their own expiry via the 5s gate below;
		// do NOT apply the normal safeExitBuffer close here (would fire too early).
		if !pos.IsConviction {
			if pos.IsScalp && winning {
				t.logger.Debug("trader: winning scalp held for post-close grace sell",
					zap.Float64("btc", btcLatest),
					zap.Float64("open", openPrice),
					zap.Float64("token_price", tokenPrice),
					zap.Duration("window_remaining", time.Until(pos.WindowEnd).Round(time.Second)),
				)
				return nil
			}
			if winning {
				return t.closePosition(ctx, 0.99,
					fmt.Sprintf("quicksell_at_99_window_end (btc=%.2f, open=%.2f, rem=%.0fs)",
						btcLatest, openPrice, time.Until(pos.WindowEnd).Seconds()))
			}
			return t.closePosition(ctx, tokenPrice,
				fmt.Sprintf("window_ending_soon (rem=%.0fs)", time.Until(pos.WindowEnd).Seconds()),
			)
		}
	}

	//  Conviction: sell at last N seconds (ConvictionLastSecSec, default 5)
	// The resting GTC sell is cancelled and a market-sell at 0.01 is placed.
	// Use ProfitTakeThreshold only when winning; when losing use the actual token price
	// so paper-mode PnL reflects reality.
	convictionLastSecThreshold := time.Duration(t.cfg.ConvictionLastSecSec) * time.Second
	if convictionLastSecThreshold <= 0 {
		convictionLastSecThreshold = 5 * time.Second
	}
	remaining := time.Until(pos.WindowEnd)
	// IsReversalSnipe positions (snipe, flip, collapse) are designed to hold to window
	// resolution Ã¢â‚¬â€ they must NOT be force-exited in the final seconds. Those positions
	// are held alive until WindowEnd (expiryDeadline below), after which max_hold_timeout
	// or conviction_stale_abandoned cleans them up at the correct settled price.
	if pos.IsConviction && !pos.IsReversalSnipe && !pos.WindowEnd.IsZero() && remaining >= 0 && remaining <= convictionLastSecThreshold {
		convSide := "YES"
		if pos.IsNoSide {
			convSide = "NO"
		}
		exitPrice := tokenPrice
		profitTake := t.cfg.ProfitTakeThreshold
		if profitTake <= 0 {
			profitTake = 0.99
		}
		if winning {
			exitPrice = profitTake
		}
		return t.closePosition(ctx, exitPrice,
			fmt.Sprintf("conviction_last_%ds_%s (rem=%.1fs, btc=%.2f, open=%.2f, winning=%v)",
				t.cfg.ConvictionLastSecSec, convSide, remaining.Seconds(), btcLatest, openPrice, winning))
	}

	// Force-close on max hold timeout.
	// Scalp positions always respect MaxHoldSec, even if currently winning.
	// Conviction/resolution/snipe-style positions keep their existing hold-to-resolution logic.
	if now.After(pos.ExpiresAt) {
		if pos.IsScalp {
			return t.closePosition(ctx, tokenPrice, "max_hold_timeout")
		}
		if winning {
			if pos.IsResolutionSnipe {
				// Winning resolution positions hold through settlement and claim.
				return nil
			}
			if pos.IsConviction {
				// Hold-to-resolution entries exit at 0.99 near expiry once they are winning.
				reason := "conviction_expiry_sell99"
				if pos.IsReversalSnipe {
					reason = "snipe_expiry_sell99"
				}
				return t.closePosition(ctx, 0.99, reason)
			}
			// Lag position winning Ã‚Â  extend hold, don't close on timeout
			t.logger.Debug("trader: max hold timeout but winning, holding",
				zap.Float64("btc", btcLatest),
				zap.Float64("open", openPrice),
				zap.Float64("token_price", tokenPrice),
			)
			return nil
		}
		return t.closePosition(ctx, tokenPrice, "max_hold_timeout")
	}
	return nil
}

// abandonStaleConvictionPosition forcefully clears a conviction position once
// its market window has closed and normal order-based exit is no longer viable.
func (t *Trader) abandonStaleConvictionPosition(currentYesPrice float64, reason string) error {
	pos := t.position
	if pos == nil || !pos.IsConviction {
		return nil
	}

	// Use the last observed token price for P&L recording.
	// Both call sites (OnPolyPrice and CheckExpiry) operate within the active
	// window's event loop and filter by the current window's YES token ID, so
	// currentYesPrice always reflects the correct market  NOT the next window.
	// Previously this was hardcoded to 0.00 ("total loss"), which caused
	// paper-mode conviction trades to record a phantom 100% loss when the stale
	// guard fired just after WindowEnd while the price was still at the target
	// (e.g. 0.99). For live mode it also incorrectly masked genuine winning exits
	// where the GTC fill detection raced with the window deadline.
	staleSide := "YES"
	var stalePrice float64
	if pos.IsNoSide {
		staleSide = "NO"
		stalePrice = math.Round((1.0-currentYesPrice)*100) / 100
	} else {
		stalePrice = math.Round(currentYesPrice*100) / 100
	}
	if stalePrice <= 0 || stalePrice > 1.0 {
		stalePrice = 0.00 // genuine bad-data fallback only
	}
	stallFeeUSD := polymarket.ComputeSellFeeUSDC(pos.Shares, stalePrice, t.feeRateBps)
	staleNetPnL := pos.Unrealized(stalePrice) - stallFeeUSD
	closedAt := time.Now()
	t.logger.Warn("trader: stale conviction position abandoned  window expired, CLOB closed",
		zap.String("side", staleSide),
		zap.Time("window_end", pos.WindowEnd),
		zap.Float64("buy_price", pos.BuyPrice),
		zap.Float64("last_price", stalePrice),
		zap.Float64("net_pnl", staleNetPnL),
		zap.Float64("shares", pos.Shares),
		zap.Duration("expired_ago", time.Since(pos.WindowEnd).Round(time.Second)),
	)
	t.appendJournalLine(TradeRecord{
		OpenedAt:                  pos.OpenedAt,
		ClosedAt:                  closedAt,
		HeldSec:                   closedAt.Sub(pos.OpenedAt).Seconds(),
		Strategy:                  journalStrategyForPosition(pos),
		Side:                      staleSide,
		EntryBTCPrice:             pos.EntryBTCPrice,
		EntryEdgeUSD:              pos.EntryEdgeUSD,
		EntryCLLagUSD:             pos.EntryCLLagUSD,
		EntryATR:                  pos.EntryATR,
		EntryWindowRemSec:         pos.EntryWindowRemSec,
		EntryWinProb:              pos.EntryWinProb,
		EntryOpenPrice:            pos.OpenPrice,
		BuyPrice:                  pos.BuyPrice,
		SellPrice:                 stalePrice,
		Shares:                    pos.Shares,
		USDSpent:                  pos.USDSpent,
		PnL:                       staleNetPnL,
		Reason:                    reason,
		CfgMinEdgeUSD:             t.cfg.CfgMinEdgeUSD,
		CfgScalpTargetCents:       t.cfg.ConvictionScalpTargetCents,
		CfgStopLossCents:          t.cfg.StopLossCents,
		CfgTradeSizeUSD:           t.cfg.TradeSizeUSD,
		CfgConvictionTradeSizeUSD: t.cfg.ConvictionTradeSizeUSD,
	})
	display.TradeClose(staleSide, pos.BuyPrice, stalePrice, pos.Shares, staleNetPnL, stallFeeUSD, reason, time.Since(pos.OpenedAt).Round(time.Second), false)
	t.position = nil
	t.detector.SetInPosition(false)
	t.savePositionState()
	return nil
}

// ForceClose immediately closes any open position regardless of P&L.
// currentPrice is always the YES token price; converted internally for NO positions.
func (t *Trader) ForceClose(ctx context.Context, currentYesPrice float64, reason string) error {
	if t.IsFlat() {
		return nil
	}
	if t.pairedPosition != nil {
		if reason == "window_expired" {
			return nil
		}
		if !t.cfg.PaperTrade {
			if reason == "shutdown" {
				t.savePositionState()
				t.logger.Warn("trader: leaving pair arb position open across shutdown for restore on restart",
					zap.String("reason", reason),
				)
				return nil
			}
			return t.forceClosePairArb(ctx, currentYesPrice, "force_close:"+reason)
		}
		return t.forceClosePairArb(ctx, currentYesPrice, "force_close:"+reason)
	}
	tokenPrice := currentYesPrice
	if t.position.IsNoSide {
		tokenPrice = math.Round((1.0-currentYesPrice)*100) / 100
	}
	return t.closePosition(ctx, tokenPrice, "force_close:"+reason)
}

// PostCloseGraceSell attempts to sell any remaining winning position on the CLOB
// during the ~30-60 second grace window that remains open after the market closes.
// The CLOB typically matches orders at 0.99 for the winning side during this period.
//
// It is a no-op for:
//   - flat positions (nothing to sell)
//   - paper mode (settlement is handled by SettleExpiredPosition)
//   - the losing side (tokens will be worth $0.00; selling at 0.99 would be a loss)
//
// resolvedYes should reflect the final BTC vs open-price comparison.
// Any shares not sold within graceDuration are left for SettleExpiredPosition.
func (t *Trader) PostCloseGraceSell(ctx context.Context, resolvedYes bool, graceDuration time.Duration) error {
	if t.pairedPosition != nil {
		return nil
	}
	if t.IsFlat() {
		return nil
	}
	if t.cfg.PaperTrade {
		return nil
	}
	pos := t.position
	if pos.IsResolutionSnipe {
		t.logger.Info("trader: post-close grace sell skipped for resolution position; waiting for settlement claim",
			zap.String("condition_id", pos.ConditionID),
			zap.Bool("is_no_side", pos.IsNoSide),
		)
		return nil
	}
	onWinningSide := (!pos.IsNoSide && resolvedYes) || (pos.IsNoSide && !resolvedYes)
	if !onWinningSide {
		t.logger.Info("trader: post-close grace sell skipped Ã¯Â¿Â½ not on winning side",
			zap.Bool("is_no_side", pos.IsNoSide),
			zap.Bool("resolved_yes", resolvedYes),
		)
		return nil
	}

	side := "YES"
	if pos.IsNoSide {
		side = "NO"
	}
	strategyForJournal := journalStrategyForPosition(pos)

	t.logger.Info("trader: starting post-close grace sell window",
		zap.Duration("grace_duration", graceDuration),
		zap.String("side", side),
		zap.Float64("shares", pos.Shares),
	)

	// Cancel any resting GTC sell order before querying the balance.
	// Conviction positions place a GTC sell at entry which escrows the shares;
	// the CLOB balance API returns escrowed shares as unavailable, causing
	// resolveSellableShares to return 0 and the grace sell to loop uselessly.
	if pos.IsConviction && pos.ActiveSellOrderID != "" {
		if cancelErr := t.cancelOrderTimed(ctx, "post_close_sell_cancel", pos.ActiveSellOrderID); cancelErr != nil {
			t.logger.Warn("trader: post-close grace sell: GTC cancel error (proceeding anyway)",
				zap.String("order_id", pos.ActiveSellOrderID),
				zap.Error(cancelErr),
			)
		} else {
			t.logger.Info("trader: post-close grace sell: GTC sell cancelled; shares now free",
				zap.String("order_id", pos.ActiveSellOrderID),
			)
			// Brief pause: let the CLOB release the escrowed shares before
			// resolveSellableShares reads the balance.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(400 * time.Millisecond):
			}
		}
		pos.ActiveSellOrderID = ""
		t.savePositionState()
	}

	const graceSellPrice = 0.99
	deadline := time.Now().Add(graceDuration)

	for time.Now().Before(deadline) && !t.IsFlat() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		pos = t.position

		// Use math.MaxFloat64 so the full balance (including any stranded shares from
		// previous sessions on this token) is sold, not just the in-memory pos.Shares.
		sellShares, err := t.resolveSellableShares(ctx, pos.TokenID, math.MaxFloat64)
		if err != nil {
			t.logger.Warn("trader: post-close grace sell: resolveSellableShares failed",
				zap.Error(err),
				zap.Duration("remaining", time.Until(deadline)),
			)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
			continue
		}
		if sellShares <= 0 {
			t.logger.Warn("trader: post-close grace sell: no sellable shares; waiting for on-chain settlement",
				zap.Duration("remaining", time.Until(deadline)),
			)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
			continue
		}

		sharesStr := fmt.Sprintf("%.2f", sellShares)
		t.logger.Info("trader: post-close grace sell attempt",
			zap.String("side", side),
			zap.Float64("sell_price", graceSellPrice),
			zap.String("shares", sharesStr),
			zap.Duration("grace_remaining", time.Until(deadline)),
		)

		actualFillPrice, filledShares, filled, sellErr := t.attemptSellWithFallback(ctx, pos.TokenID, graceSellPrice, sharesStr)
		if sellErr != nil {
			t.logger.Warn("trader: post-close grace sell attempt error",
				zap.Error(sellErr),
				zap.Duration("remaining", time.Until(deadline)),
			)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
			continue
		}
		if !filled {
			t.logger.Warn("trader: post-close grace sell not filled; retrying",
				zap.Duration("remaining", time.Until(deadline)),
			)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
			continue
		}
		if filledShares <= pairArbShareDust {
			t.logger.Warn("trader: post-close grace sell returned no fill evidence; retrying",
				zap.Duration("remaining", time.Until(deadline)),
			)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
			continue
		}
		if filledShares > sellShares {
			filledShares = sellShares
		}

		// Filled Ã¯Â¿Â½ compute proportional PnL (handles partial fills correctly).
		soldFrac := filledShares / pos.Shares
		if soldFrac > 1.0 {
			soldFrac = 1.0
		}
		proRataUSDSpent := pos.USDSpent * soldFrac
		actualSellFeeUSD := polymarket.ComputeSellFeeUSDC(filledShares, actualFillPrice, t.feeRateBps)
		actualGrossPnL := filledShares*actualFillPrice - proRataUSDSpent
		actualNetPnL := actualGrossPnL - actualSellFeeUSD

		t.logger.Info("trader: post-close grace sell FILLED",
			zap.String("side", side),
			zap.Float64("fill_price", actualFillPrice),
			zap.Float64("shares_sold", filledShares),
			zap.Float64("net_pnl", actualNetPnL),
		)

		closedAt := time.Now()
		cfgScalpCents := t.cfg.ScalpTargetCents
		if pos.IsConviction {
			cfgScalpCents = t.cfg.ConvictionScalpTargetCents
		}
		t.appendJournalLine(TradeRecord{
			OpenedAt:                  pos.OpenedAt,
			ClosedAt:                  closedAt,
			HeldSec:                   closedAt.Sub(pos.OpenedAt).Seconds(),
			Strategy:                  strategyForJournal,
			Side:                      side,
			EntryBTCPrice:             pos.EntryBTCPrice,
			EntryEdgeUSD:              pos.EntryEdgeUSD,
			EntryCLLagUSD:             pos.EntryCLLagUSD,
			EntryATR:                  pos.EntryATR,
			EntryWindowRemSec:         pos.EntryWindowRemSec,
			EntryWinProb:              pos.EntryWinProb,
			EntryOpenPrice:            pos.OpenPrice,
			BuyPrice:                  pos.BuyPrice,
			SellPrice:                 actualFillPrice,
			Shares:                    filledShares,
			USDSpent:                  proRataUSDSpent,
			PnL:                       actualNetPnL,
			Reason:                    "post_close_grace_sell",
			CfgMinEdgeUSD:             t.cfg.CfgMinEdgeUSD,
			CfgScalpTargetCents:       cfgScalpCents,
			CfgStopLossCents:          t.cfg.StopLossCents,
			CfgTradeSizeUSD:           t.cfg.TradeSizeUSD,
			CfgConvictionTradeSizeUSD: t.cfg.ConvictionTradeSizeUSD,
		})
		display.TradeClose(side, pos.BuyPrice, actualFillPrice, filledShares, actualNetPnL, actualSellFeeUSD,
			"post_close_grace_sell", time.Since(pos.OpenedAt).Round(time.Second), false)

		// Partial fill: update position and loop to sell the residual.
		if filledShares < pos.Shares-0.005 {
			pos.Shares -= filledShares
			pos.USDSpent -= proRataUSDSpent
			pos.SellPending = false
			t.savePositionState()
			t.logger.Warn("trader: post-close grace partial sell Ã¯Â¿Â½ residual remains",
				zap.Float64("sold_shares", filledShares),
				zap.Float64("remaining_shares", pos.Shares),
			)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
			continue
		}

		// Full close.
		t.position = nil
		t.detector.SetInPosition(false)
		t.savePositionState()
		t.logger.Info("trader: post-close grace sell: position fully closed")
		return nil
	}

	if !t.IsFlat() {
		t.logger.Warn("trader: post-close grace sell window expired; remaining shares passed to SettleExpiredPosition",
			zap.Float64("remaining_shares", t.position.Shares),
		)
	}
	return nil
}

// SettleExpiredPosition force-closes any surviving conviction (or other) position
// using the correct resolution price ($1.00 for winner, $0.00 for loser).
// Must be called after the window loop ends BEFORE moving to the next window,
// while the detector's BTC/open state still reflects the just-ended market.
// resolvedYes is true when BTC closed above the market's open price (YES wins).
// For live (non-paper) wins the function attempts CTF redeemPositions on-chain
// so shares are automatically converted to USDC when the CLOB is no longer available.
func (t *Trader) SettleExpiredPosition(ctx context.Context, resolvedYes bool) {
	if t.IsFlat() {
		return
	}
	if t.pairedPosition != nil {
		t.settlePairArbPosition(ctx, resolvedYes)
		return
	}
	pos := t.position

	// Determine the correct settlement value for the token we held:
	//   Held YES + resolution YES Ã‚Â  $1.00  (win)
	//   Held YES + resolution NO  Ã‚Â  $0.00  (loss)
	//   Held NO  + resolution YES Ã‚Â  $0.00  (loss)
	//   Held NO  + resolution NO  Ã‚Â  $1.00  (win)
	var resolvedPrice float64
	if pos.IsNoSide {
		if resolvedYes {
			resolvedPrice = 0.00
		} else {
			resolvedPrice = 1.00
		}
	} else {
		if resolvedYes {
			resolvedPrice = 1.00
		} else {
			resolvedPrice = 0.00
		}
	}

	sideName := "YES"
	if pos.IsNoSide {
		sideName = "NO"
	}
	strategyForJournal := journalStrategyForPosition(pos)

	sellFeeUSD := polymarket.ComputeSellFeeUSDC(pos.Shares, resolvedPrice, t.feeRateBps)
	netPnL := pos.Unrealized(resolvedPrice) - sellFeeUSD
	closedAt := time.Now()

	t.logger.Warn("trader: settling expired position at resolution price",
		zap.String("side", sideName),
		zap.String("strategy", strategyForJournal),
		zap.Float64("buy_price", pos.BuyPrice),
		zap.Float64("resolved_price", resolvedPrice),
		zap.Bool("resolved_yes", resolvedYes),
		zap.Float64("net_pnl", netPnL),
		zap.Float64("shares", pos.Shares),
		zap.Time("window_end", pos.WindowEnd),
	)

	// Kalshi settlement is automatic after finalization.
	// No CTF redemption transaction or claim retry is required.
	if t.cfg.PaperTrade {
		proceeds := pos.Shares*resolvedPrice - sellFeeUSD
		if proceeds < 0 {
			proceeds = 0
		}
		t.paperBalance += proceeds
	}

	cfgScalpCentsResolved := t.cfg.ScalpTargetCents
	if pos.IsConviction {
		cfgScalpCentsResolved = t.cfg.ConvictionScalpTargetCents
	}
	t.appendJournalLine(TradeRecord{
		OpenedAt:          pos.OpenedAt,
		ClosedAt:          closedAt,
		HeldSec:           closedAt.Sub(pos.OpenedAt).Seconds(),
		Strategy:          strategyForJournal,
		Side:              sideName,
		EntryBTCPrice:     pos.EntryBTCPrice,
		EntryEdgeUSD:      pos.EntryEdgeUSD,
		EntryCLLagUSD:     pos.EntryCLLagUSD,
		EntryATR:          pos.EntryATR,
		EntryWindowRemSec: pos.EntryWindowRemSec,
		EntryWinProb:      pos.EntryWinProb,
		EntryOpenPrice:    pos.OpenPrice,
		BuyPrice:          pos.BuyPrice,
		SellPrice:         resolvedPrice,
		Shares:            pos.Shares,
		USDSpent:          pos.USDSpent,
		PnL:               netPnL,
		Reason: func() string {
			if pos.IsResolutionSnipe && resolvedPrice == 1.00 {
				return "window_resolved_claim_pending"
			}
			return "window_resolved"
		}(),
		CfgMinEdgeUSD:             t.cfg.CfgMinEdgeUSD,
		CfgScalpTargetCents:       cfgScalpCentsResolved,
		CfgStopLossCents:          t.cfg.StopLossCents,
		CfgTradeSizeUSD:           t.cfg.TradeSizeUSD,
		CfgConvictionTradeSizeUSD: t.cfg.ConvictionTradeSizeUSD,
	})
	display.TradeClose(sideName, pos.BuyPrice, resolvedPrice, pos.Shares, netPnL, sellFeeUSD,
		func() string {
			if pos.IsResolutionSnipe && resolvedPrice == 1.00 {
				return "window_resolved_claim_pending"
			}
			return "window_resolved"
		}(), time.Since(pos.OpenedAt).Round(time.Second), t.cfg.PaperTrade)

	t.position = nil
	t.detector.SetInPosition(false)
	t.savePositionState()
}

// FormatPosition returns a human-readable position summary, or "" if flat.
// currentYesPrice is always the YES token price.
func (t *Trader) FormatPosition(currentYesPrice float64) string {
	if t.IsFlat() {
		return ""
	}
	if t.pairedPosition != nil {
		p := t.pairedPosition
		lockedShares := p.lockedShares()
		avgCost := 0.0
		if lockedShares > 0 {
			avgCost = p.totalSpent() / lockedShares
		}
		pnl := p.markToMarket(currentYesPrice)
		return fmt.Sprintf(
			"POSITION [PAIR locked=%.2f avg_cost=%.4f yes=%.2f no=%.2f pnl=$%.4f held=%s win_rem=%s]",
			lockedShares,
			avgCost,
			p.YesShares,
			p.NoShares,
			pnl,
			time.Since(p.OpenedAt).Round(time.Second),
			time.Until(p.WindowEnd).Round(time.Second),
		)
	}
	p := t.position
	tokenPrice := currentYesPrice
	if p.IsNoSide {
		tokenPrice = math.Round((1.0-currentYesPrice)*100) / 100
	}
	side := "YES"
	if p.IsNoSide {
		side = "NO"
	}
	pnl := p.Unrealized(tokenPrice)
	holdTime := time.Since(p.OpenedAt).Round(time.Second)
	winRem := time.Until(p.WindowEnd).Round(time.Second)
	pnlUSD, _ := strconv.ParseFloat(fmt.Sprintf("%.4f", pnl), 64)
	return fmt.Sprintf(
		"POSITION [%s buy=%.4f target=%.4f open_price=%.2f current=%.4f shares=%.2f pnl=$%.4f held=%s win_rem=%s]",
		side, p.BuyPrice, p.TargetPrice, p.OpenPrice, tokenPrice, p.Shares, pnlUSD, holdTime, winRem,
	)
}

// PositionSnapshot returns (side, shares, buyPrice, unrealizedPnL) for the display.
// currentYesPrice is always the YES token price.
func (t *Trader) PositionSnapshot(currentYesPrice float64) (string, float64, float64, float64) {
	if t.IsFlat() {
		return "", 0, 0, 0
	}
	if t.pairedPosition != nil {
		p := t.pairedPosition
		lockedShares := p.lockedShares()
		avgCost := 0.0
		if lockedShares > 0 {
			avgCost = p.totalSpent() / lockedShares
		}
		return "PAIR", lockedShares, avgCost, p.markToMarket(currentYesPrice)
	}
	p := t.position
	tokenPrice := currentYesPrice
	side := "YES"
	if p.IsNoSide {
		tokenPrice = math.Round((1.0-currentYesPrice)*100) / 100
		side = "NO"
	}
	return side, p.Shares, p.BuyPrice, p.Unrealized(tokenPrice)
}

func (t *Trader) DashboardPositionSnapshot(currentYesPrice float64) DashboardPositionSnapshot {
	if t.IsFlat() {
		return DashboardPositionSnapshot{}
	}
	if t.pairedPosition != nil {
		pair := t.pairedPosition
		lockedShares := pair.lockedShares()
		matchedCost := pair.matchedCost()
		expectedProfit := 0.0
		expectedROIPct := 0.0
		if lockedShares > 0 {
			expectedProfit = lockedShares - matchedCost
			if matchedCost > 0 {
				expectedROIPct = (expectedProfit / matchedCost) * 100
			}
		}
		residualSide, residualShares, _ := pair.residualPosition()
		hedgeSide, _, anchorAvg := pair.rebalanceState()
		hedgeMaxPrice := 0.0
		if hedgeSide != "" && anchorAvg > 0 {
			hedgeMaxPrice = t.pairArbLockedPayoutTarget() - t.pairArbMinLockedProfit() - anchorAvg
			if hedgeMaxPrice < 0 {
				hedgeMaxPrice = 0
			}
		}
		avgCost := 0.0
		if lockedShares > 0 {
			avgCost = matchedCost / lockedShares
		}
		return DashboardPositionSnapshot{
			HasPosition:            true,
			Side:                   "PAIR",
			Type:                   "pair_arb",
			BuyPrice:               avgCost,
			Shares:                 lockedShares,
			USDSpent:               pair.totalSpent(),
			UnrealizedPnL:          pair.markToMarket(currentYesPrice),
			HeldSec:                time.Since(pair.OpenedAt).Seconds(),
			PairActive:             true,
			PairLeadSide:           pair.LeadSide,
			PairYesFilled:          pair.YesShares > 0,
			PairNoFilled:           pair.NoShares > 0,
			PairYesAvgPrice:        pair.sideAveragePrice("YES"),
			PairNoAvgPrice:         pair.sideAveragePrice("NO"),
			PairYesShares:          pair.YesShares,
			PairNoShares:           pair.NoShares,
			PairYesSpent:           pair.YesUSDSpent,
			PairNoSpent:            pair.NoUSDSpent,
			PairYesWalletConfirmed: pair.YesWalletConfirmed || t.cfg.PaperTrade,
			PairNoWalletConfirmed:  pair.NoWalletConfirmed || t.cfg.PaperTrade,
			PairLockedShares:       lockedShares,
			PairMatchedCost:        matchedCost,
			PairExpectedProfit:     expectedProfit,
			PairExpectedROIPct:     expectedROIPct,
			PairResidualSide:       residualSide,
			PairResidualShares:     residualShares,
			PairHedgeSide:          hedgeSide,
			PairHedgeMaxPrice:      hedgeMaxPrice,
			PairMarkToMarket:       pair.markToMarket(currentYesPrice),
			PairExitState:          pair.ExitState,
			PairExitStateNote:      pair.ExitStateNote,
			PairYesExitOrderID:     pair.YesExitOrderID,
			PairNoExitOrderID:      pair.NoExitOrderID,
			PairExitPlacedAt:       pair.ExitOrdersPlacedAt,
		}
	}
	position := t.position
	tokenPrice := currentYesPrice
	side := "YES"
	if position.IsNoSide {
		tokenPrice = math.Round((1.0-currentYesPrice)*100) / 100
		side = "NO"
	}
	positionType := journalStrategyForPosition(position)
	return DashboardPositionSnapshot{
		HasPosition:   true,
		Side:          side,
		Type:          positionType,
		BuyPrice:      position.BuyPrice,
		Shares:        position.Shares,
		USDSpent:      position.USDSpent,
		UnrealizedPnL: position.Unrealized(tokenPrice),
		HeldSec:       time.Since(position.OpenedAt).Seconds(),
	}
}

// PositionHeldSec returns how many seconds the current position has been held.
// Returns 0 when flat.
func (t *Trader) PositionHeldSec() float64 {
	if t.IsFlat() {
		return 0
	}
	if t.pairedPosition != nil {
		return time.Since(t.pairedPosition.OpenedAt).Seconds()
	}
	return time.Since(t.position.OpenedAt).Seconds()
}

// OnDCAHedgeSignal executes the DCA+Hedge simultaneous dual-leg entry. It buys both the
// moved side and the opposite side in share quantities supplied by EvaluateDCAHedge().
// Unlike the standard pair-arb flow, window-time and BTC-gap gates are intentionally
// bypassed — the strategy is purely price-level driven.  After both legs open, the
// normal managePairArbPosition loop handles hedge completion, stop-loss, and settlement.
func (t *Trader) OnDCAHedgeSignal(ctx context.Context, sig Signal, yesTokenID, noTokenID string) error {
	if !t.tryBeginPairLeadEntry() {
		return fmt.Errorf("dca hedge: lead entry already in progress, ignoring")
	}
	defer t.finishPairLeadEntry()

	if !t.IsFlat() {
		return fmt.Errorf("dca hedge: already in position, ignoring")
	}
	if t.buyInProgress {
		return fmt.Errorf("dca hedge: buy in progress, ignoring")
	}
	if blocked, reason := t.riskBlocked(); blocked {
		return fmt.Errorf("dca hedge: risk controls: %s", reason)
	}

	yesPrice := sig.PolyYesPrice
	noPrice := sig.PolyNoPrice
	if yesPrice <= 0 || yesPrice >= 1.0 || noPrice <= 0 || noPrice >= 1.0 {
		return fmt.Errorf("dca hedge: invalid entry prices yes=%.4f no=%.4f", yesPrice, noPrice)
	}

	slipTicks := t.cfg.PairArbLeadBuySlipTicks
	if slipTicks < 0 {
		slipTicks = int(pairArbLimitBuySlip * 100)
	}
	oppSlipTicks := t.cfg.DCAHedgeOppLegSlipTicks
	if oppSlipTicks < 0 {
		oppSlipTicks = slipTicks // fallback to lead slip
	}
	yesLimit := math.Round((yesPrice+float64(slipTicks)*0.01)*100) / 100
	noLimit := math.Round((noPrice+float64(oppSlipTicks)*0.01)*100) / 100
	if yesLimit >= 1.0 {
		yesLimit = 0.99
	}
	if noLimit >= 1.0 {
		noLimit = 0.99
	}

	rawShares := sig.DCAHedgeMovedShares
	if rawShares <= 0 {
		rawShares = 5
	}

	// Balance check: ensure we can afford both legs.
	var available float64
	if t.cfg.PaperTrade {
		available = t.paperBalance
	} else {
		available = math.Float64frombits(atomic.LoadUint64(&t.cachedLiveBalance))
	}
	if available > 0 {
		yesCost := marketBuyOrderNotional(rawShares, yesLimit)
		noCost := marketBuyOrderNotional(rawShares, noLimit)
		totalRequired := yesCost + noCost
		if available < totalRequired {
			return fmt.Errorf("dca hedge: insufficient balance ($%.2f) for both legs ($%.2f total)", available, totalRequired)
		}
	}

	// Execute YES leg.
	t.buyInProgress = true
	outcomeYes, orderIDYes, fillPriceYes, actualSharesYes, feeSharesYes, usdSpentYes, errYes := t.executePairLimitBuy(ctx, "dca_hedge_yes_buy", yesTokenID, yesPrice, yesLimit, rawShares)
	t.buyInProgress = false
	if errYes != nil {
		t.triggerPairArbAmbiguousRecovery(yesTokenID, noTokenID, sig.PolyYesPrice, errYes)
		return errYes
	}
	if outcomeYes != pairArbBuyOutcomeFilled || actualSharesYes <= pairArbShareDust {
		t.logger.Debug("dca hedge: YES leg not filled; skipping signal")
		return nil
	}

	now := time.Now()
	pairConditionID := t.convConditionID
	pairWindowEnd := t.detector.WindowEnd()
	pair := &PairArbPosition{
		ConditionID:          pairConditionID,
		YesTokenID:           yesTokenID,
		NoTokenID:            noTokenID,
		LeadSide:             "YES",
		OpenPrice:            sig.OpenPrice,
		WindowEnd:            pairWindowEnd,
		OpenedAt:             now,
		HedgeBy:              now.Add(t.pairArbHedgeTimeout()),
		YesShares:            actualSharesYes,
		YesUSDSpent:          usdSpentYes,
		DCAHedgeMovedSide:    sig.DCAHedgeMovedSide,
		DCAHedgeTriggerPrice: sig.DCAHedgeTriggerPrice,
	}
	if !pair.WindowEnd.IsZero() && pair.HedgeBy.After(pair.WindowEnd) {
		pair.HedgeBy = pair.WindowEnd
	}
	t.pairedPosition = pair
	t.rememberBotConditionID(pairConditionID)
	t.detector.SetInPosition(true)
	t.savePositionState()
	t.logger.Info("trader: dca hedge: YES leg opened",
		zap.String("order_id", orderIDYes),
		zap.Float64("fill_price", fillPriceYes),
		zap.Float64("shares", actualSharesYes),
		zap.Float64("usd_spent", usdSpentYes),
		zap.String("moved_side", sig.DCAHedgeMovedSide),
		zap.Float64("trigger_price", sig.DCAHedgeTriggerPrice),
	)
	display.TradeOpen("YES [DCA HEDGE]", fillPriceYes, actualSharesYes, feeSharesYes, usdSpentYes, 1.0-fillPriceYes-noPrice, t.cfg.PaperTrade)

	// Execute NO leg.
	outcomeNo, orderIDNo, fillPriceNo, actualSharesNo, feeSharesNo, usdSpentNo, errNo := t.executePairLimitBuy(ctx, "dca_hedge_no_buy", noTokenID, noPrice, noLimit, rawShares)
	if errNo != nil || outcomeNo != pairArbBuyOutcomeFilled || actualSharesNo <= pairArbShareDust {
		// NO leg failed (timed out or price ran away). For DCA+Hedge the two legs are a
		// simultaneous package — there is no directional thesis on the YES-only position.
		// Immediately sell the YES leg back rather than entering the hedge retry loop.
		// In the backtest this trade would have been counted as fill_failed at zero cost.
		t.logger.Warn("trader: dca hedge: NO leg failed; selling YES leg back to abort",
			zap.Error(errNo),
		)
		_, _, liveYes, _, _, _ := t.detector.Snapshot()
		if liveYes <= 0 || liveYes >= 1.0 {
			liveYes = sig.PolyYesPrice
		}
		return t.forceClosePairArb(ctx, liveYes, "dca_hedge_no_leg_failed")
	}

	pair.NoShares = actualSharesNo
	pair.NoUSDSpent = usdSpentNo
	pair.BalancedAt = now
	pair.HedgeBy = time.Time{}
	t.savePositionState()
	t.logger.Info("trader: dca hedge: NO leg opened; fully hedged from entry",
		zap.String("order_id", orderIDNo),
		zap.Float64("fill_price", fillPriceNo),
		zap.Float64("shares", actualSharesNo),
		zap.Float64("usd_spent", usdSpentNo),
		zap.Float64("edge_per_share", 1.0-fillPriceYes-fillPriceNo),
	)
	display.TradeOpen("NO [DCA HEDGE]", fillPriceNo, actualSharesNo, feeSharesNo, usdSpentNo, 1.0-fillPriceYes-fillPriceNo, t.cfg.PaperTrade)

	_, _, finalYes, _, _, _ := t.detector.Snapshot()
	if finalYes <= 0 || finalYes >= 1.0 {
		finalYes = sig.PolyYesPrice
	}
	finalNo := math.Round((1.0-finalYes)*100) / 100
	return t.managePairArbPosition(ctx, finalYes, finalNo, 0)
}

func (t *Trader) OnPairArbSignal(ctx context.Context, sig Signal, yesTokenID, noTokenID string) error {
	if !t.tryBeginPairLeadEntry() {
		return fmt.Errorf("pair arb: lead entry already in progress, ignoring")
	}
	defer t.finishPairLeadEntry()

	if allowed, reason := pairArbScheduleAllowsEntry(t.cfg.PairArbScheduleMode, t.cfg.PairArbScheduleWindowsUTC, sig.At); !allowed {
		t.logger.Warn("pair arb: schedule gate blocked entry",
			zap.String("mode", normalizePairArbScheduleMode(t.cfg.PairArbScheduleMode)),
			zap.String("windows_utc", strings.TrimSpace(t.cfg.PairArbScheduleWindowsUTC)),
			zap.String("reason", reason))
		return fmt.Errorf("pair arb: %s", reason)
	}

	if maxAge := t.cfg.PairArbMaxSignalAgeSec; maxAge > 0 && !sig.At.IsZero() {
		if age := time.Since(sig.At); age > time.Duration(maxAge)*time.Second {
			t.logger.Warn("pair arb: signal too stale, skipping entry",
				zap.Duration("age", age),
				zap.Int("max_age_sec", maxAge))
			return fmt.Errorf("pair arb: signal age %v exceeds max %ds", age.Round(time.Millisecond), maxAge)
		}
	}
	if maxCLAge := t.cfg.PairArbMaxCLAgeSec; maxCLAge > 0 {
		_, _, _, _, _, clAgeSec := t.detector.Snapshot()
		if clAgeSec > float64(maxCLAge) {
			t.logger.Warn("pair arb: Chainlink data too stale, skipping entry",
				zap.Float64("cl_age_sec", clAgeSec),
				zap.Int("max_cl_age_sec", maxCLAge))
			return fmt.Errorf("pair arb: Chainlink age %.0fs exceeds max %ds", clAgeSec, maxCLAge)
		}
	}
	if !t.IsFlat() {
		return fmt.Errorf("pair arb: already in position, ignoring")
	}
	if t.pendingPairArb != nil {
		pendingAge := time.Since(t.pendingPairArb.PlacedAt)
		if t.pendingPairArb.PlacedAt.IsZero() || pendingAge < 0 {
			pendingAge = 0
		}
		t.logger.Warn("pair arb: pending order exists before new lead; starting reconcile",
			zap.String("order_id", t.pendingPairArb.OrderID),
			zap.String("request", t.pendingPairArb.RequestName),
			zap.String("origin", t.pendingPairArb.Origin),
			zap.Time("placed_at", t.pendingPairArb.PlacedAt),
			zap.Duration("age", pendingAge),
			zap.Int("probe_attempts", pairArbOrderProbeAttempts),
			zap.Duration("lookup_timeout", pairArbReconcileLookupTimeout),
		)
		reconcileCtx, cancel := context.WithTimeout(ctx, pairArbReconcileLookupTimeout)
		err := t.reconcilePendingPairArbOrder(reconcileCtx)
		cancel()
		if err != nil {
			fields := []zap.Field{
				zap.Error(err),
				zap.Int("probe_attempts", pairArbOrderProbeAttempts),
				zap.Duration("lookup_timeout", pairArbReconcileLookupTimeout),
			}
			if po := t.pendingPairArb; po != nil {
				pendingAge := time.Since(po.PlacedAt)
				if po.PlacedAt.IsZero() || pendingAge < 0 {
					pendingAge = 0
				}
				fields = append(fields,
					zap.String("order_id", po.OrderID),
					zap.String("request", po.RequestName),
					zap.String("origin", po.Origin),
					zap.Time("placed_at", po.PlacedAt),
					zap.Duration("age", pendingAge),
				)
			}
			t.logger.Warn("pair arb: pending order reconciliation blocking new lead", fields...)
			return err
		}
		if t.pendingPairArb != nil {
			return fmt.Errorf("pair arb: pending order %s unresolved; waiting before new lead", t.pendingPairArb.OrderID)
		}
	}
	if t.pendingHedgePrePlace != nil {
		reconcileCtx, cancel := context.WithTimeout(ctx, pairArbReconcileLookupTimeout)
		err := t.reconcilePendingHedgePrePlaceWhenFlat(reconcileCtx)
		cancel()
		if err != nil {
			return err
		}
		if t.pendingHedgePrePlace != nil {
			return fmt.Errorf("pair arb: pending hedge order %s unresolved; waiting before new lead", t.pendingHedgePrePlace.OrderID)
		}
	}
	// Pending-order reconciliation above can recover/open a live pair position
	// from wallet state. Re-check before attempting any new lead entry.
	if !t.IsFlat() {
		return fmt.Errorf("pair arb: already in position, ignoring")
	}
	isTruePreOpen := sig.Type == SignalPairArbTruePreOpenYes || sig.Type == SignalPairArbTruePreOpenNo

	if !t.cfg.PaperTrade {
		// True pre-open targets a different market (different conditionID), so there
		// is no inventory to check on those tokens — skip the inventory check.
		if !isTruePreOpen {
			invCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := t.ensureNoPairArbInventory(invCtx, yesTokenID, noTokenID)
			cancel()
			if err != nil {
				if strings.Contains(err.Error(), "existing wallet inventory detected") {
					t.logger.Error("pair arb: untracked wallet inventory detected before new lead; attempting autonomous liquidation",
						zap.Error(err),
					)
					liqCtx, liqCancel := context.WithTimeout(context.Background(), 30*time.Second)
					t.liquidateOrphanedPairArbInventory(liqCtx, yesTokenID, noTokenID, sig.PolyYesPrice)
					retryErr := t.ensureNoPairArbInventory(liqCtx, yesTokenID, noTokenID)
					liqCancel()
					if retryErr != nil {
						return retryErr
					}
				} else {
					return err
				}
			}
		}
	}
	if t.buyInProgress {
		return fmt.Errorf("pair arb: buy in progress, ignoring")
	}
	// True pre-open targets a fresh market on a different conditionID — the 5s
	// post-close hold (designed to guard against re-entering the same market before
	// inventory clears) does not apply here.
	if !isTruePreOpen && !t.lastPairArbCloseAt.IsZero() {
		if elapsed := time.Since(t.lastPairArbCloseAt); elapsed < 5*time.Second {
			// Apply this hold only when re-entering the SAME market/condition.
			currentCond := normalizeConditionID(t.convConditionID)
			lastCond := normalizeConditionID(t.lastPairArbCloseConditionID)
			if currentCond != "" && currentCond == lastCond {
				remaining := 5*time.Second - elapsed
				t.logger.Warn("pair arb: post-close validation hold active, skipping entry",
					zap.Duration("elapsed", elapsed.Round(time.Millisecond)),
					zap.Duration("remaining", remaining.Round(time.Second)),
					zap.String("condition_id", currentCond))
				return fmt.Errorf("pair arb: post-close validation hold active for %.0fs more", remaining.Seconds())
			}
		}
	}
	if cooldown := t.cfg.PairArbStopCooldownSec; cooldown > 0 && !t.lastPairArbStopLossAt.IsZero() {
		if elapsed := time.Since(t.lastPairArbStopLossAt); elapsed < time.Duration(cooldown)*time.Second {
			remaining := time.Duration(cooldown)*time.Second - elapsed
			t.logger.Warn("pair arb: stop-loss cooldown active, skipping entry",
				zap.Duration("elapsed", elapsed.Round(time.Millisecond)),
				zap.Duration("remaining", remaining.Round(time.Second)),
				zap.Int("cooldown_sec", cooldown))
			return fmt.Errorf("pair arb: stop-loss cooldown active for %.0fs more", remaining.Seconds())
		}
	}
	if blocked, reason := t.riskBlocked(); blocked {
		t.logger.Warn("pair arb entry blocked by risk controls", zap.String("reason", reason))
		return fmt.Errorf("risk controls: %s", reason)
	}
	tradeSize := t.pairArbTradeSizeUSD()
	if tradeSize <= 0 {
		return fmt.Errorf("pair arb: trade size not configured")
	}

	// Paper parity first. Live is opt-in only after the Kalshi paper audit passes.
	if !t.cfg.PaperTrade && !strings.EqualFold(strings.TrimSpace(os.Getenv("KALSHI_PAIR_LIVE_ENABLED")), "true") {
		return fmt.Errorf("pair arb: Kalshi live execution disabled until paper parity audit passes; set KALSHI_PAIR_LIVE_ENABLED=true only after approval")
	}

	// Dual-entry mode: when continuous imbalance is enabled, buy both sides simultaneously
	// instead of the normal lead→hedge flow.
	if t.cfg.PairArbContinuousImbalanceEnabled {
		return t.enterDualPairArb(ctx, sig, yesTokenID, noTokenID)
	}

	isNoLead := sig.Type == SignalPairArbLeadNo || sig.Type == SignalPairArbReverseLeadNo || sig.Type == SignalPairArbPreOpenNo || sig.Type == SignalPairArbTruePreOpenNo || sig.Type == SignalPairArbCVDMomentumNo
	leadSide := "YES"
	tokenID := yesTokenID
	leadPrice := sig.PolyYesPrice
	if isNoLead {
		leadSide = "NO"
		tokenID = noTokenID
		leadPrice = sig.PolyNoPrice
	}
	if tokenID == "" {
		return fmt.Errorf("pair arb: no %s token ID available", leadSide)
	}
	if leadPrice <= 0 || leadPrice >= 1.0 {
		return fmt.Errorf("pair arb: refusing lead buy at extreme price %.4f", leadPrice)
	}
	minLeadPrice := t.cfg.PairArbMinTokenPrice
	if minLeadPrice <= 0 {
		minLeadPrice = 0.01
	}
	maxLeadPrice := t.cfg.PairArbMaxTokenPrice
	if maxLeadPrice <= 0 || maxLeadPrice >= 1.0 {
		maxLeadPrice = 0.99
	}
	if minLeadPrice > maxLeadPrice {
		minLeadPrice, maxLeadPrice = maxLeadPrice, minLeadPrice
	}
	if leadPrice < minLeadPrice || leadPrice > maxLeadPrice {
		return fmt.Errorf("pair arb: lead price %.4f outside configured range [%.2f, %.2f]", leadPrice, minLeadPrice, maxLeadPrice)
	}

	// Default to +0.02 above signal price (crosses the spread, nearly instant fill).
	// Set PairArbLeadBuySlipTicks=0 for a passive/maker resting order at signal price.
	// Pair with PairArbLeadBuyTimeoutSec to give the resting order time to fill.
	slipTicks := t.cfg.PairArbLeadBuySlipTicks
	if slipTicks < 0 {
		slipTicks = int(pairArbLimitBuySlip * 100)
	}
	limitPrice := math.Round((leadPrice+float64(slipTicks)*0.01)*100) / 100
	if limitPrice >= 1.0 {
		limitPrice = 0.99
	}
	rawShares := math.Round((tradeSize/limitPrice)*100) / 100
	if t.cfg.PaperTrade {
		rawShares = pairKalshiSharesForBudget(tradeSize, limitPrice)
	}
	if rawShares <= 0 {
		return fmt.Errorf("pair arb: invalid raw shares sizing")
	}
	estimatedActualShares := math.Floor((rawShares-polymarket.ComputeBuyFeeShares(rawShares, leadPrice, t.feeRateBps))*100) / 100
	if t.cfg.PaperTrade {
		estimatedActualShares = rawShares
	}
	if estimatedActualShares <= 0 {
		return fmt.Errorf("pair arb: fee eats all lead shares")
	}
	hedgeSide := "NO"
	hedgePrice := sig.PolyNoPrice
	if isNoLead {
		hedgeSide = "YES"
		hedgePrice = sig.PolyYesPrice
	}
	_, hedgeNotional, hedgeAllowed := pairArbExactHedgeSizing(estimatedActualShares, hedgePrice, t.feeRateBps)
	if !hedgeAllowed {
		return fmt.Errorf("pair arb: exact %s hedge would be $%.4f at %.4f, below $%.2f FAK minimum", hedgeSide, hedgeNotional, hedgePrice, polymarket.MinMarketOrderNotionalUSD)
	}

	// Pre-flight balance check: ensure we can afford both legs before committing to the lead.
	// Use the cached balance (pre-fetched at window start, refreshed every 5 s) so this
	// path is non-blocking Ã¢â‚¬â€ every millisecond matters for FAK fill probability.
	leadCost := marketBuyOrderNotional(rawShares, limitPrice)
	if t.cfg.PaperTrade {
		leadCost += kalshiTakerFeeUSD(rawShares, limitPrice)
		hedgeNotional += kalshiTakerFeeUSD(estimatedActualShares, hedgePrice)
	}
	totalRequired := leadCost + hedgeNotional
	if t.cfg.PaperTrade {
		if t.paperBalance < totalRequired {
			return fmt.Errorf("pair arb: insufficient paper balance ($%.2f) for both legs ($%.2f lead + $%.2f hedge = $%.2f)",
				t.paperBalance, leadCost, hedgeNotional, totalRequired)
		}
	} else {
		available := math.Float64frombits(atomic.LoadUint64(&t.cachedLiveBalance))
		if available <= 0 {
			// Balance has not been fetched yet or last fetch failed Ã¢â‚¬â€ allow entry.
			t.logger.Warn("pair arb: cached balance unavailable, skipping pre-flight check")
		} else if available < totalRequired {
			return fmt.Errorf("pair arb: insufficient balance ($%.2f) for both legs ($%.2f lead + $%.2f hedge = $%.2f)",
				available, leadCost, hedgeNotional, totalRequired)
		}
	}

	// Gap guard: cancel the resting lead GTC order if the BTC-vs-open gap closes while
	// the order is waiting to fill. This prevents fills on stale signals (e.g. BTC spike
	// reversed before the order matched, so there is no longer any arbitrage edge).
	t.pairArbLeadGapGuardFn = t.makePairArbLeadGapGuard(isNoLead)
	defer func() { t.pairArbLeadGapGuardFn = nil }()

	if t.cfg.PairArbDualPrePlace {
		if t.pairArbLeadOrderType() != polymarket.OrderTypeGTC {
			t.logger.Warn("pair arb: dual pre-place enabled but lead order type is not GTC; falling back to sequential flow",
				zap.String("lead_order_type", string(t.pairArbLeadOrderType())),
			)
		} else {
			maxHedgePrice := math.Round((1.0-leadPrice-t.pairArbMinLockedProfit())*100) / 100
			if t.cfg.PaperTrade {
				leadEff := leadPrice + kalshiTakerFeeUSD(estimatedActualShares, leadPrice)/estimatedActualShares
				maxHedgePrice = pairKalshiMaxHedgePrice(leadEff, estimatedActualShares, t.pairArbMinLockedProfit())
			}
			if maxHedgePrice <= 0 || maxHedgePrice >= 1.0 {
				return fmt.Errorf("pair arb: dual pre-place max hedge price %.4f out of range", maxHedgePrice)
			}
			t.buyInProgress = true
			err := t.executeDualPrePlacePairArb(
				ctx,
				sig,
				yesTokenID,
				noTokenID,
				leadSide,
				isNoLead,
				tokenID,
				hedgeSide,
				leadPrice,
				limitPrice,
				rawShares,
				hedgePrice,
				maxHedgePrice,
			)
			t.buyInProgress = false
			if err != nil {
				t.triggerPairArbAmbiguousRecovery(yesTokenID, noTokenID, sig.PolyYesPrice, err)
			}
			return err
		}
	}

	t.buyInProgress = true
	outcome, orderID, fillPrice, actualShares, feeShares, usdSpent, err := t.executePairLimitBuy(ctx, "pair_arb_lead_buy", tokenID, leadPrice, limitPrice, rawShares)
	t.buyInProgress = false
	if err != nil {
		t.triggerPairArbAmbiguousRecovery(yesTokenID, noTokenID, sig.PolyYesPrice, err)
		return err
	}
	if outcome != pairArbBuyOutcomeFilled {
		pairConditionID := t.convConditionID
		if sig.OverrideConditionID != "" {
			pairConditionID = sig.OverrideConditionID
		}
		pairWindowEnd := t.detector.WindowEnd()
		if !sig.OverrideWindowEnd.IsZero() {
			pairWindowEnd = sig.OverrideWindowEnd
		}
		t.setPendingPairArbContext(orderID, pairConditionID, yesTokenID, noTokenID, leadSide, pairWindowEnd)
		t.logger.Debug("pair arb: lead buy not filled; skipping signal",
			zap.String("lead_side", leadSide),
			zap.String("order_id", orderID),
		)
		return nil
	}

	now := time.Now()
	pairConditionID := t.convConditionID
	if sig.OverrideConditionID != "" {
		pairConditionID = sig.OverrideConditionID
	}
	pairWindowEnd := t.detector.WindowEnd()
	if !sig.OverrideWindowEnd.IsZero() {
		pairWindowEnd = sig.OverrideWindowEnd
	}
	pair := &PairArbPosition{
		ConditionID: pairConditionID,
		YesTokenID:  yesTokenID,
		NoTokenID:   noTokenID,
		LeadSide:    leadSide,
		OpenPrice:   sig.OpenPrice,
		WindowEnd:   pairWindowEnd,
		OpenedAt:    now,
		HedgeBy:     now.Add(t.pairArbHedgeTimeout()),
	}
	if sig.Type == SignalPairArbCVDMomentumYes || sig.Type == SignalPairArbCVDMomentumNo {
		if t.cfg.PairArbCVDMomentumLockedProfitCents > 0 {
			pair.LockedProfitCents = t.cfg.PairArbCVDMomentumLockedProfitCents
		} else {
			pair.LockedProfitCents = 3.0 // default 3¢ for CVD momentum
		}
	}
	if !pair.WindowEnd.IsZero() && pair.HedgeBy.After(pair.WindowEnd) {
		pair.HedgeBy = pair.WindowEnd
	}
	if isNoLead {
		pair.NoShares = actualShares
		pair.NoUSDSpent = usdSpent
	} else {
		pair.YesShares = actualShares
		pair.YesUSDSpent = usdSpent
	}
	t.pairedPosition = pair
	t.detector.SetInPosition(true)
	t.savePositionState()
	t.logger.Info("trader: pair arb lead leg opened",
		zap.String("order_id", orderID),
		zap.String("lead_side", leadSide),
		zap.Float64("fill_price", fillPrice),
		zap.Float64("shares", actualShares),
		zap.Float64("usd_spent", usdSpent),
		zap.Float64("target_locked_profit", t.pairArbMinLockedProfit()),
	)
	display.TradeOpen(leadSide+" [PAIR LEAD]", fillPrice, actualShares, feeShares, usdSpent, 1.0-fillPrice-t.pairArbMinLockedProfit(), t.cfg.PaperTrade)
	// FOK mode: place an immediate FOK hedge; unwind lead if all attempts miss. No
	// resting orders, no timeout loop — either both legs fill atomically or lead is sold.
	if t.pairArbLeadOrderType() == polymarket.OrderTypeFOK {
		return t.executeFOKHedgeAfterLeadFill(ctx, sig, isNoLead, leadSide)
	}
	// GTC / FAK mode: place a resting GTC hedge and poll until HedgeBy.
	// Bounded fill-leg routine: try to complete the hedge for up to 20 seconds.
	for time.Now().Before(pair.HedgeBy) {
		if t.pairedPosition == nil || t.pairedPosition.isBalanced() {
			break
		}
		snapNow := t.detector.PairArbSnapshot()
		liveYes, liveNo := snapNow.YesPrice, snapNow.NoPrice
		if liveYes <= 0 || liveYes >= 1.0 {
			liveYes = sig.PolyYesPrice
		}
		if liveNo <= 0 || liveNo >= 1.0 {
			liveNo = sig.PolyNoPrice
		}
		if err := t.maybeRebalancePairArb(ctx, liveYes, liveNo); err != nil {
			return err
		}
		if t.pairedPosition != nil && t.pairedPosition.isBalanced() {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	finalSnap := t.detector.PairArbSnapshot()
	finalYes, finalNo := finalSnap.YesPrice, finalSnap.NoPrice
	if finalYes <= 0 || finalYes >= 1.0 {
		finalYes = sig.PolyYesPrice
	}
	if finalNo <= 0 || finalNo >= 1.0 {
		finalNo = sig.PolyNoPrice
	}
	return t.managePairArbPosition(ctx, finalYes, finalNo, 0)
}

func (t *Trader) managePairArbPosition(ctx context.Context, currentYesPrice float64, currentNoPrice float64, safeExitBuffer time.Duration) error {
	pair := t.pairedPosition
	if pair == nil {
		return nil
	}
	t.reconcileLivePairArbState(ctx, pair, currentYesPrice, currentNoPrice)
	pair = t.pairedPosition
	if pair == nil {
		return nil
	}

	// DCA+Hedge: when the moved side has fallen DCAHedgeDCAReversal from its trigger price,
	// buy an additional DCAHedgeDCAAddShares of that side as the DCA add.
	if pair.DCAHedgeMovedSide != "" && !pair.DCAHedgeDCAFired &&
		t.cfg.DCAHedgeDCAReversal > 0 && pair.hasBothLegs() {
		movedNow := currentYesPrice
		if strings.EqualFold(pair.DCAHedgeMovedSide, "NO") {
			movedNow = currentNoPrice
		}
		if movedNow > 0 && pair.DCAHedgeTriggerPrice > 0 &&
			movedNow <= pair.DCAHedgeTriggerPrice-t.cfg.DCAHedgeDCAReversal {
			pair.DCAHedgeDCAFired = true
			t.savePositionState()
			addShares := t.cfg.DCAHedgeDCAAddShares
			if addShares <= 0 {
				addShares = 5
			}
			dcaTokenID := pair.YesTokenID
			if strings.EqualFold(pair.DCAHedgeMovedSide, "NO") {
				dcaTokenID = pair.NoTokenID
			}
			limitPrice := math.Round((movedNow+0.02)*100) / 100
			if limitPrice >= 1.0 {
				limitPrice = 0.99
			}
			t.logger.Info("trader: dca hedge: DCA add triggered",
				zap.String("moved_side", pair.DCAHedgeMovedSide),
				zap.Float64("trigger_price", pair.DCAHedgeTriggerPrice),
				zap.Float64("current_price", movedNow),
				zap.Float64("drop", pair.DCAHedgeTriggerPrice-movedNow),
				zap.Float64("add_shares", addShares),
				zap.Float64("limit_price", limitPrice),
			)
			reqName := "dca_hedge_dca_add_yes"
			if strings.EqualFold(pair.DCAHedgeMovedSide, "NO") {
				reqName = "dca_hedge_dca_add_no"
			}
			_, _, fillPrice, actualShares, feeShares, usdSpent, errDCA := t.executePairLimitBuy(ctx, reqName, dcaTokenID, movedNow, limitPrice, addShares)
			if errDCA != nil {
				t.logger.Warn("trader: dca hedge: DCA add failed", zap.Error(errDCA))
			} else if actualShares > pairArbShareDust {
				if strings.EqualFold(pair.DCAHedgeMovedSide, "YES") {
					pair.YesShares += actualShares
					pair.YesUSDSpent += usdSpent
				} else {
					pair.NoShares += actualShares
					pair.NoUSDSpent += usdSpent
				}
				t.savePositionState()
				t.logger.Info("trader: dca hedge: DCA add filled",
					zap.Float64("fill_price", fillPrice),
					zap.Float64("shares", actualShares),
					zap.Float64("usd_spent", usdSpent),
				)
				display.TradeOpen(pair.DCAHedgeMovedSide+" [DCA ADD]", fillPrice, actualShares, feeShares, usdSpent, 0, t.cfg.PaperTrade)
			}
		}
	}

	dualMode := t.cfg.PairArbContinuousImbalanceEnabled && pair.hasBothLegs()
	if pair.isBalanced() || dualMode {
		if err := t.maybeStartContinuousImbalance(ctx, currentYesPrice, currentNoPrice); err != nil {
			return err
		}
		pair = t.pairedPosition
		if pair == nil {
			return nil
		}
		if !pair.isBalanced() && !dualMode {
			return nil
		}
		if !t.cfg.PairArbSellAt99 {
			t.setPairExitState(pair, pairExitStateClaimQueued, "balanced; hold-to-claim mode active")
			return nil
		}
		return t.manageBalancedPairArbSellAt99(ctx, pair)
	}
	if pair.HedgeBy.IsZero() {
		pair.HedgeBy = pair.OpenedAt.Add(t.pairArbHedgeTimeout())
		if !pair.WindowEnd.IsZero() && pair.HedgeBy.After(pair.WindowEnd) {
			pair.HedgeBy = pair.WindowEnd
		}
		t.savePositionState()
	}
	now := time.Now()
	// DCA+Hedge fully-hedged positions must NOT be force-closed by the safe-exit buffer:
	// both legs are committed to resolution and shares are redeemed by settlePairArbPosition.
	// Force-closing clears in-memory state without selling shares, leaving them stranded.
	dcaHedgeFullyHedged := pair.DCAHedgeMovedSide != "" && pair.hasBothLegs()
	if safeExitBuffer > 0 && !dcaHedgeFullyHedged && !pair.WindowEnd.IsZero() && now.Add(safeExitBuffer).After(pair.WindowEnd) {
		if err := t.forceClosePairArb(ctx, currentYesPrice, "pair_unhedged_safe_exit"); err != nil {
			t.logger.Warn("trader: pair arb safe-exit close failed; will retry",
				zap.Error(err),
			)
			return nil
		}
		return nil
	}
	if shouldAbort, leadNow, abortReason := t.shouldAbortUnprofitableUnhedgedPairArb(pair, currentYesPrice, currentNoPrice, now); shouldAbort {
		t.logger.Warn("trader: pair arb unhedged abort triggered",
			zap.String("reason", abortReason),
			zap.String("lead_side", pair.LeadSide),
			zap.Float64("yes_shares", pair.YesShares),
			zap.Float64("no_shares", pair.NoShares),
		)
		if err := t.forceClosePairArb(ctx, leadNow, "pair_unprofitable_abort"); err != nil {
			t.logger.Warn("trader: pair arb unprofitable abort close failed; will retry", zap.Error(err))
			return nil
		}
		return nil
	}
	// DCA+Hedge positions are direction-neutral: YES dropping means NO is rising and the
	// hedge is intact. A stop-loss on the lead leg would break the hedge by exiting one
	// side prematurely. Skip entirely for all DCA+Hedge positions.
	if t.cfg.PairArbStopLossCents > 0 && pair.DCAHedgeMovedSide == "" {
		leadAvg := pair.sideAveragePrice(pair.LeadSide)
		leadNow := currentYesPrice
		if strings.EqualFold(pair.LeadSide, "NO") {
			leadNow = currentNoPrice
			if leadNow <= 0 && currentYesPrice > 0 && currentYesPrice < 1.0 {
				leadNow = math.Round((1.0-currentYesPrice)*100) / 100
			}
		}
		if leadAvg > 0 && leadNow > 0 {
			stopDrop := t.cfg.PairArbStopLossCents / 100.0
			if leadNow <= leadAvg-stopDrop {
				triggered := true
				if minHold := t.cfg.PairArbStopLossMinHoldSec; minHold > 0 {
					if now.Sub(pair.OpenedAt).Seconds() < float64(minHold) {
						triggered = false
					}
				}
				if triggered {
					if minGapAgainst := t.cfg.PairArbStopLossMinGapAgainstUSD; minGapAgainst > 0 {
						btc, _, _, open, _, _ := t.detector.Snapshot()
						if btc > 0 && open > 0 {
							adverseGap := 0.0
							if strings.EqualFold(pair.LeadSide, "YES") {
								if open > btc {
									adverseGap = open - btc
								}
							} else {
								if btc > open {
									adverseGap = btc - open
								}
							}
							if adverseGap < minGapAgainst {
								triggered = false
							}
						}
					}
				}
				if triggered {
					if err := t.forceClosePairArb(ctx, leadNow, "pair_stop_loss"); err != nil {
						t.logger.Warn("trader: pair arb stop-loss close failed; will retry", zap.Error(err))
						return nil
					}
					return nil
				}
			}
		}
	}
	if pair.DCAHedgeMovedSide == "" && !pair.HedgeBy.IsZero() && !now.Before(pair.HedgeBy) {
		if err := t.forceClosePairArb(ctx, currentYesPrice, "pair_fill_leg_timeout"); err != nil {
			t.logger.Warn("trader: pair arb timeout close failed; will retry",
				zap.Error(err),
			)
			return nil
		}
		return nil
	}
	if err := t.maybeRebalancePairArb(ctx, currentYesPrice, currentNoPrice); err != nil {
		return err
	}
	pair = t.pairedPosition
	if pair == nil {
		return nil
	}
	t.reconcileLivePairArbState(ctx, pair, currentYesPrice, currentNoPrice)
	pair = t.pairedPosition
	if pair == nil {
		return nil
	}
	if pair.isBalanced() {
		if err := t.maybeStartContinuousImbalance(ctx, currentYesPrice, currentNoPrice); err != nil {
			return err
		}
		pair = t.pairedPosition
		if pair == nil {
			return nil
		}
		if !pair.isBalanced() {
			return nil
		}
		if !t.cfg.PairArbSellAt99 {
			t.setPairExitState(pair, pairExitStateClaimQueued, "balanced; hold-to-claim mode active")
			return nil
		}
		return t.manageBalancedPairArbSellAt99(ctx, pair)
	}
	now = time.Now()
	if pair.DCAHedgeMovedSide == "" && !pair.HedgeBy.IsZero() && !now.Before(pair.HedgeBy) {
		if err := t.forceClosePairArb(ctx, currentYesPrice, "pair_fill_leg_timeout"); err != nil {
			t.logger.Warn("trader: pair arb timeout close failed; will retry",
				zap.Error(err),
			)
			return nil
		}
		return nil
	}
	return nil
}

func (t *Trader) maybeStartContinuousImbalance(ctx context.Context, currentYesPrice float64, currentNoPrice float64) error {
	pair := t.pairedPosition
	dualMode := t.cfg.PairArbContinuousImbalanceEnabled && pair.hasBothLegs()
	if pair == nil || (!pair.isBalanced() && !dualMode) || t.buyInProgress {
		return nil
	}
	if !t.cfg.PairArbContinuousImbalanceEnabled || t.cfg.PairArbSellAt99 {
		return nil
	}
	if t.isPairExitManagementInFlight() {
		return nil
	}
	if t.pendingPairArb != nil || t.pendingHedgePrePlace != nil {
		return nil
	}
	now := time.Now()
	if cooldown := t.cfg.PairArbContinuousImbalanceCooldownSec; cooldown > 0 {
		if !pair.LastImbalanceAt.IsZero() && now.Sub(pair.LastImbalanceAt) < time.Duration(cooldown)*time.Second {
			return nil
		}
	}
	if capAdds := t.cfg.PairArbContinuousImbalanceMaxAdds; capAdds > 0 && pair.ImbalanceAdds >= capAdds {
		return nil
	}
	btcPrice, _, _, openPrice, _, _ := t.detector.Snapshot()
	if btcPrice <= 0 || openPrice <= 0 {
		return nil
	}
	gap := btcPrice - openPrice
	// MaxGapUSD: stop adds when BTC has moved too far in one direction (market too directional).
	if maxGap := t.cfg.PairArbContinuousImbalanceMaxGapUSD; maxGap > 0 && math.Abs(gap) > maxGap {
		return nil
	}

	// Select the dip side: buy whichever leg is trading BELOW its average cost.
	// This averages down the blended cost per share pair. The direction comes from
	// price-vs-average, NOT from the current BTC gap — the winning side right now
	// is expensive; the cheap side is always the one that lost ground.
	// After the add, maybeRebalancePairArb places a resting GTC hedge limit order
	// (1 - dip_avg - min_profit); no same-tick hedge required.
	yesAvg := pair.sideAveragePrice("YES")
	noAvg := pair.sideAveragePrice("NO")
	minImprove := t.cfg.PairArbContinuousImbalanceMinPriceImprovementCents / 100.0
	if minImprove <= 0 {
		minImprove = 0.01
	}
	yesDiscount := yesAvg - currentYesPrice // >0 = YES is below our average (dip)
	noDiscount := noAvg - currentNoPrice    // >0 = NO is below our average (dip)

	// Trend priority: lower threshold for the side that BTC trend is pushing down.
	yesThreshold := minImprove
	noThreshold := minImprove
	if gap > 5 { // BTC trending up: NO is being pushed down, prioritize NO adds.
		noThreshold = math.Round(minImprove*0.6*100) / 100
	} else if gap < -5 { // BTC trending down: YES is being pushed down, prioritize YES adds.
		yesThreshold = math.Round(minImprove*0.6*100) / 100
	}

	var buySide string
	var buyPrice float64
	var buyTokenID string
	if t.cfg.PairArbContinuousImbalanceAllowMomentum {
		// Momentum path: add on the side that moved furthest from average in either direction.
		if math.Abs(yesDiscount) >= math.Abs(noDiscount) && math.Abs(yesDiscount) >= yesThreshold {
			buySide = "YES"
		} else if math.Abs(noDiscount) >= noThreshold {
			buySide = "NO"
		}
	} else {
		// Default dip-only path: average down — only add when price is BELOW our average.
		if yesDiscount >= yesThreshold && yesDiscount >= noDiscount {
			buySide = "YES"
		} else if noDiscount >= noThreshold {
			buySide = "NO"
		}
	}
	if buySide == "" {
		return nil
	}
	// Never increase an existing share imbalance — that turns pair-arb into a
	// directional bet on the losing side. Only add to a side when it has equal
	// or fewer shares than the other side.
	if buySide == "YES" && pair.YesShares > pair.NoShares {
		return nil
	}
	if buySide == "NO" && pair.NoShares > pair.YesShares {
		return nil
	}
	// MaxUSDPerSide cap: don't add to a side that has already reached its limit.
	if maxPerSide := t.cfg.PairArbContinuousImbalanceMaxUSDPerSide; maxPerSide > 0 {
		if buySide == "YES" && pair.YesUSDSpent >= maxPerSide {
			return nil
		}
		if buySide == "NO" && pair.NoUSDSpent >= maxPerSide {
			return nil
		}
	}
	if buySide == "YES" {
		buyPrice = currentYesPrice
		buyTokenID = pair.YesTokenID
	} else {
		buyPrice = currentNoPrice
		buyTokenID = pair.NoTokenID
		if buyPrice <= 0 && currentYesPrice > 0 && currentYesPrice < 1.0 {
			buyPrice = math.Round((1.0-currentYesPrice)*100) / 100
		}
	}
	if buyPrice <= 0 || buyPrice >= 1.0 || buyTokenID == "" {
		return nil
	}

	if !t.tryBeginPairRebalance() {
		return nil
	}
	defer t.finishPairRebalance()

	tradeUSD := t.cfg.PairArbContinuousImbalanceTradeSizeUSD
	if tradeUSD <= 0 {
		tradeUSD = t.pairArbTradeSizeUSD() * 0.5
	}
	if tradeUSD <= 0 {
		return nil
	}

	// Balance check: don't add if we can't afford it.
	var availBal float64
	if t.cfg.PaperTrade {
		availBal = t.paperBalance
	} else {
		availBal = math.Float64frombits(atomic.LoadUint64(&t.cachedLiveBalance))
	}
	if availBal > 0 {
		// Cap the add size to 90% of available balance (keep a small cushion).
		maxAdd := math.Floor((availBal*0.90)*100) / 100
		if maxAdd < polymarket.MinMarketOrderNotionalUSD {
			t.logger.Debug("trader: continuous imbalance add skipped: insufficient balance",
				zap.Float64("available", availBal),
			)
			return nil
		}
		if maxAdd < tradeUSD {
			tradeUSD = maxAdd
		}
	}

	rawShares := math.Round((tradeUSD/buyPrice)*100) / 100
	notional := marketBuyOrderNotional(rawShares, buyPrice)
	if rawShares <= 0 || notional+0.0000001 < polymarket.MinMarketOrderNotionalUSD {
		return nil
	}

	limitPrice := buyPrice
	if slipTicks := t.cfg.PairArbLeadBuySlipTicks; slipTicks > 0 {
		limitPrice = math.Round((buyPrice+0.01*float64(slipTicks))*100) / 100
	}
	if limitPrice <= 0 || limitPrice >= 1.0 {
		return nil
	}

	requestName := "pair_arb_imbalance_yes_buy"
	if buySide == "NO" {
		requestName = "pair_arb_imbalance_no_buy"
	}
	outcome, orderID, fillPrice, actualShares, feeShares, usdSpent, err := t.executePairLimitBuy(ctx, requestName, buyTokenID, buyPrice, limitPrice, rawShares)
	if err != nil {
		t.logger.Warn("trader: continuous imbalance add failed",
			zap.String("side", buySide),
			zap.Error(err),
		)
		return nil
	}
	if outcome != pairArbBuyOutcomeFilled || actualShares <= pairArbShareDust {
		return nil
	}

	if buySide == "YES" {
		pair.YesShares += actualShares
		pair.YesUSDSpent += usdSpent
	} else {
		pair.NoShares += actualShares
		pair.NoUSDSpent += usdSpent
	}
	pair.HedgeBy = t.pairArbHedgeDeadline(pair.WindowEnd)
	pair.ImbalanceAdds++
	pair.LastImbalanceAt = now
	t.savePositionState()

	edgePerShare := t.pairArbLockedPayoutTarget() - pair.sideAveragePrice("YES") - pair.sideAveragePrice("NO")
	t.logger.Info("trader: continuous imbalance add filled",
		zap.String("order_id", orderID),
		zap.String("side", buySide),
		zap.Float64("signal_gap_usd", gap),
		zap.Float64("signal_price", buyPrice),
		zap.Float64("fill_price", fillPrice),
		zap.Float64("shares", actualShares),
		zap.Float64("locked_edge_per_share", edgePerShare),
		zap.Int("imbalance_adds", pair.ImbalanceAdds),
	)
	display.TradeOpen(buySide+" [PAIR IMBALANCE]", fillPrice, actualShares, feeShares, usdSpent, edgePerShare, t.cfg.PaperTrade)
	return nil
}

func (t *Trader) maybeRebalancePairArb(ctx context.Context, currentYesPrice float64, currentNoPrice float64) error {
	pair := t.pairedPosition
	if pair == nil || pair.isBalanced() || t.buyInProgress {
		return nil
	}
	if t.isPairExitManagementInFlight() {
		return nil
	}
	if !t.tryBeginPairRebalance() {
		return nil
	}
	defer t.finishPairRebalance()
	buySide, deficitShares, anchorAvgPrice := pair.rebalanceState()
	if buySide == "" || deficitShares < 0.01 || anchorAvgPrice <= 0 {
		return nil
	}
	// Requested mode: second-leg hedge limit uses opposite-side lock math.
	// Example: lead 0.55 and min locked profit 0.05 => hedge limit 1 - 0.55 - 0.05 = 0.40.
	hedgeLimitPrice := pairKalshiMaxHedgePrice(anchorAvgPrice, deficitShares, t.pairArbMinLockedProfit())
	if hedgeLimitPrice <= 0 || hedgeLimitPrice >= 1.0 {
		return nil
	}
	hedgePrice := currentYesPrice
	tokenID := pair.YesTokenID
	if buySide == "NO" {
		tokenID = pair.NoTokenID
		hedgePrice = currentNoPrice
		if hedgePrice <= 0 {
			hedgePrice = math.Round((1.0-currentYesPrice)*100) / 100
		}
	}
	if tokenID == "" || hedgePrice <= 0 || hedgePrice >= 1.0 {
		return nil
	}

	// Post-lead hedge model: place a resting GTC order at 1-lead-minus-profit,
	// then wait up to HedgeBy for it to fill before force-closing the lead leg.
	rawShares, hedgeNotional, hedgeAllowed := pairArbExactHedgeSizing(deficitShares, hedgeLimitPrice, t.feeRateBps)
	if !hedgeAllowed {
		t.logger.Debug("trader: pair arb exact hedge below market minimum",
			zap.String("hedge_side", buySide),
			zap.Float64("deficit_shares", deficitShares),
			zap.Float64("hedge_price", hedgeLimitPrice),
			zap.Float64("notional", hedgeNotional),
		)
		return nil
	}
	if rawShares <= 0 {
		return nil
	}

	if t.cfg.PaperTrade {
		// Use live Kalshi executable depth, not a synthetic 1-YES hedge.
		fillPrice, usdSpent, feeUSD, filled := t.pairPaperBuyQuote(ctx, tokenID, rawShares, hedgeLimitPrice, hedgePrice)
		if !filled || fillPrice <= 0 || fillPrice >= 1.0 {
			return nil
		}
		feeShares := 0.0
		actualShares := rawShares
		if actualShares <= pairArbShareDust {
			return nil
		}
		if usdSpent > t.paperBalance {
			t.logger.Warn("trader: pair arb paper hedge skipped due to insufficient paper balance",
				zap.String("hedge_side", buySide),
				zap.Float64("required_usd", usdSpent),
				zap.Float64("paper_balance", t.paperBalance),
			)
			return nil
		}
		t.paperBalance -= usdSpent
		if buySide == "YES" {
			pair.YesShares += actualShares
			pair.YesUSDSpent += usdSpent
			pair.YesWalletConfirmed = true
		} else {
			pair.NoShares += actualShares
			pair.NoUSDSpent += usdSpent
			pair.NoWalletConfirmed = true
		}
		if pair.isBalanced() {
			pair.BalancedAt = time.Now()
			pair.HedgeBy = time.Time{}
		}
		t.savePositionState()
		t.logger.Info("trader: pair arb hedge leg filled (paper simulation)",
			zap.String("hedge_side", buySide),
			zap.Float64("fill_price", fillPrice),
			zap.Float64("shares", actualShares),
			zap.Float64("locked_shares", pair.lockedShares()),
			zap.Float64("total_spent", pair.totalSpent()),
			zap.Float64("kalshi_fee_usd", feeUSD),
		)
		display.TradeOpen(buySide+" [PAIR HEDGE]", fillPrice, actualShares, feeShares, usdSpent, 1.0-pair.sideAveragePrice("YES")-pair.sideAveragePrice("NO"), true)
		return nil
	}

	if php := t.pendingHedgePrePlace; php != nil {
		if php.TokenID != tokenID {
			t.logger.Warn("trader: clearing stale pending hedge order on wrong token",
				zap.String("order_id", php.OrderID),
				zap.String("pending_token", php.TokenID),
				zap.String("expected_token", tokenID),
			)
			t.clearPrePlacedHedgeOrder()
		} else {
			gr, grErr := t.getOrderTimed(ctx, "pair_arb_hedge_pre_poll_live", php.OrderID)
			if grErr != nil {
				return nil
			}
			if gr == nil {
				// Matched hedge orders can disappear from GET /order quickly.
				// Probe fills directly so we can absorb the hedge at actual price.
				avg, gross, fErr := t.getFillsTimed(ctx, "pair_arb_hedge_pre_fills_live", php.OrderID, tokenID)
				if fErr != nil || gross <= 0 || avg <= 0 {
					return nil
				}
				// Sanity-check: reject contaminated fill responses where the fills API returned
				// far more shares than were actually ordered (Polymarket API contamination bug).
				if php.RequestedSize > 0 && gross > php.RequestedSize*1.1 {
					t.logger.Warn("trader: fills response grossly exceeds order size; likely API contamination — skipping fill credit",
						zap.String("order_id", php.OrderID),
						zap.Float64("gross_shares", gross),
						zap.Float64("requested_size", php.RequestedSize),
					)
					return nil
				}
				feeShares := polymarket.ComputeBuyFeeShares(gross, avg, t.feeRateBps)
				actualShares := math.Floor((gross-feeShares)*100) / 100
				usdSpent := marketBuyOrderNotional(gross, avg)
				if actualShares <= 0 {
					return nil
				}
				if buySide == "YES" {
					pair.YesShares += actualShares
					pair.YesUSDSpent += usdSpent
					pair.YesWalletConfirmed = false
				} else {
					pair.NoShares += actualShares
					pair.NoUSDSpent += usdSpent
					pair.NoWalletConfirmed = false
				}
				if pair.isBalanced() {
					pair.BalancedAt = time.Now()
					pair.HedgeBy = time.Time{}
				}
				t.clearPrePlacedHedgeOrder()
				t.savePositionState()
				t.logger.Info("trader: pair arb hedge leg filled from fills lookup",
					zap.String("order_id", php.OrderID),
					zap.String("hedge_side", buySide),
					zap.Float64("fill_price", avg),
					zap.Float64("shares", actualShares),
					zap.Float64("locked_shares", pair.lockedShares()),
					zap.Float64("total_spent", pair.totalSpent()),
				)
				display.TradeOpen(buySide+" [PAIR HEDGE]", avg, actualShares, feeShares, usdSpent, 1.0-pair.sideAveragePrice("YES")-pair.sideAveragePrice("NO"), t.cfg.PaperTrade)
				return nil
			}
			status := strings.ToLower(strings.TrimSpace(gr.Status))
			switch status {
			case "matched", "filled":
				// KALSHI SAFETY: status and order limit are not execution-price
				// accounting. Require authoritative fills before absorbing inventory.
				avg, gross, fErr := t.getFillsTimed(
					ctx,
					"pair_arb_hedge_pre_fills_live",
					php.OrderID,
					tokenID,
				)
				if fErr != nil || gross <= 0 || avg <= 0 {
					return nil
				}
				// Sanity-check: reject contaminated fill responses where the fills API returned
				// far more shares than were actually ordered (Polymarket API contamination bug).
				if php.RequestedSize > 0 && gross > php.RequestedSize*1.1 {
					t.logger.Warn("trader: fills response grossly exceeds order size; likely API contamination — skipping fill credit",
						zap.String("order_id", php.OrderID),
						zap.Float64("gross_shares", gross),
						zap.Float64("requested_size", php.RequestedSize),
					)
					return nil
				}
				feeShares := polymarket.ComputeBuyFeeShares(gross, avg, t.feeRateBps)
				actualShares := math.Floor((gross-feeShares)*100) / 100
				usdSpent := marketBuyOrderNotional(gross, avg)
				if actualShares <= 0 {
					return nil
				}
				if buySide == "YES" {
					pair.YesShares += actualShares
					pair.YesUSDSpent += usdSpent
					pair.YesWalletConfirmed = false
				} else {
					pair.NoShares += actualShares
					pair.NoUSDSpent += usdSpent
					pair.NoWalletConfirmed = false
				}
				if pair.isBalanced() {
					pair.BalancedAt = time.Now()
					pair.HedgeBy = time.Time{}
				}
				t.clearPrePlacedHedgeOrder()
				t.savePositionState()
				t.logger.Info("trader: pair arb hedge leg filled",
					zap.String("order_id", php.OrderID),
					zap.String("hedge_side", buySide),
					zap.Float64("fill_price", avg),
					zap.Float64("shares", actualShares),
					zap.Float64("locked_shares", pair.lockedShares()),
					zap.Float64("total_spent", pair.totalSpent()),
				)
				display.TradeOpen(buySide+" [PAIR HEDGE]", avg, actualShares, feeShares, usdSpent, 1.0-pair.sideAveragePrice("YES")-pair.sideAveragePrice("NO"), t.cfg.PaperTrade)
				return nil
			case "live":
				return nil
			case "unmatched", "canceled", "cancelled", "expired":
				t.clearPrePlacedHedgeOrder()
			default:
				return nil
			}
		}
	}

	resp, placeErr := t.placeOrderTimed(ctx, "pair_arb_hedge_pre", &polymarket.NewOrderRequest{
		OrderType:      polymarket.OrderTypeGTC,
		TokenID:        tokenID,
		Side:           polymarket.SideBuy,
		Price:          fmt.Sprintf("%.2f", hedgeLimitPrice),
		Size:           fmt.Sprintf("%.2f", rawShares),
		Nonce:          polymarket.MakeNonce(),
		FeeRateBps:     t.feeRateBps,
		AttemptTimeout: pairArbHedgeSubmitTimeout,
		MaxAttempts:    pairArbHedgeMaxAttempts,
	})
	if placeErr != nil {
		return nil
	}
	if resp == nil || !resp.Success || resp.OrderID == "" {
		return nil
	}

	// If the hedge was instantly matched on submit, credit it now — no polling needed.
	hedgeSubmitStatus := strings.ToLower(strings.TrimSpace(resp.Status))
	if hedgeSubmitStatus == "matched" {
		avg, gross, fErr := t.getFillsTimed(ctx, "pair_arb_hedge_submit_fills", resp.OrderID, tokenID)
		if fErr != nil || gross <= 0 || avg <= 0 {
			// Fills not yet propagated — try MakingAmount/TakingAmount from the response body.
			if resp.TakingAmount != "" && resp.MakingAmount != "" {
				takingRaw, okT := new(big.Int).SetString(resp.TakingAmount, 10)
				makingRaw, okM := new(big.Int).SetString(resp.MakingAmount, 10)
				if okT && okM && takingRaw.Sign() > 0 && makingRaw.Sign() > 0 {
					actualRat := new(big.Rat).SetFrac(takingRaw, big.NewInt(1_000_000))
					if g, _ := actualRat.Float64(); g > 0 {
						gross = g
					}
					priceRat := new(big.Rat).SetFrac(makingRaw, takingRaw)
					if p, _ := priceRat.Float64(); p > 0 && p < 1.0 {
						avg = p
					}
				}
			}
		}
		if avg > 0 && gross > 0 {
			if rawShares > 0 && gross > rawShares*1.1 {
				t.logger.Warn("trader: hedge submit matched — fills grossly exceed order size; API contamination likely — storing for poll",
					zap.String("order_id", resp.OrderID),
					zap.Float64("gross_shares", gross),
					zap.Float64("requested_size", rawShares),
				)
			} else {
				feeShares := polymarket.ComputeBuyFeeShares(gross, avg, t.feeRateBps)
				actualShares := math.Floor((gross-feeShares)*100) / 100
				usdSpent := marketBuyOrderNotional(gross, avg)
				if actualShares > 0 {
					if buySide == "YES" {
						pair.YesShares += actualShares
						pair.YesUSDSpent += usdSpent
						pair.YesWalletConfirmed = false
					} else {
						pair.NoShares += actualShares
						pair.NoUSDSpent += usdSpent
						pair.NoWalletConfirmed = false
					}
					if pair.isBalanced() {
						pair.BalancedAt = time.Now()
						pair.HedgeBy = time.Time{}
					}
					t.savePositionState()
					t.logger.Info("trader: pair arb hedge leg filled on submit",
						zap.String("order_id", resp.OrderID),
						zap.String("hedge_side", buySide),
						zap.Float64("fill_price", avg),
						zap.Float64("shares", actualShares),
						zap.Float64("locked_shares", pair.lockedShares()),
						zap.Float64("total_spent", pair.totalSpent()),
					)
					display.TradeOpen(buySide+" [PAIR HEDGE]", avg, actualShares, feeShares, usdSpent, 1.0-pair.sideAveragePrice("YES")-pair.sideAveragePrice("NO"), t.cfg.PaperTrade)
					return nil
				}
			}
		}
		// Fills unavailable yet — fall through to pendingHedgePrePlace so it's polled on the next iteration.
		t.logger.Info("trader: pair arb hedge matched on submit but fills unavailable; storing for immediate poll",
			zap.String("order_id", resp.OrderID),
			zap.String("hedge_side", buySide),
		)
	}

	t.setPrePlacedHedgeOrder(resp.OrderID, tokenID, "pair_arb_hedge_pre", rawShares)
	t.logger.Info("trader: pair arb hedge GTC submitted",
		zap.String("order_id", resp.OrderID),
		zap.String("hedge_side", buySide),
		zap.Float64("hedge_limit", hedgeLimitPrice),
		zap.Float64("shares", rawShares),
		zap.Float64("lead_avg", anchorAvgPrice),
		zap.Float64("locked_profit_target", t.pairArbMinLockedProfit()),
	)
	return nil
}

func (t *Trader) appendPairArbTrade(pair *PairArbPosition, closedAt time.Time, side string, shares float64, costBasis float64, exitPrice float64, proceeds float64, sellFeeUSD float64, reason string) {
	if pair == nil || shares <= 0 {
		return
	}
	buyPrice := 0.0
	if shares > 0 {
		buyPrice = costBasis / shares
	}
	t.appendJournalLine(TradeRecord{
		OpenedAt:          pair.OpenedAt,
		ClosedAt:          closedAt,
		HeldSec:           closedAt.Sub(pair.OpenedAt).Seconds(),
		Strategy:          "pair_arb",
		Side:              pairArbJournalSideLabel(side),
		EntryBTCPrice:     0,
		EntryEdgeUSD:      0,
		EntryCLLagUSD:     0,
		EntryATR:          0,
		EntryWindowRemSec: pair.WindowEnd.Sub(pair.OpenedAt).Seconds(),
		EntryOpenPrice:    pair.OpenPrice,
		BuyPrice:          buyPrice,
		SellPrice:         exitPrice,
		Shares:            shares,
		USDSpent:          costBasis,
		PnL:               proceeds - costBasis - sellFeeUSD,
		Reason:            reason,
		CfgMinEdgeUSD:     t.cfg.CfgMinEdgeUSD,
		CfgStopLossCents:  t.cfg.PairArbStopLossCents,
		CfgTradeSizeUSD:   t.pairArbTradeSizeUSD(),
	})
}

func (t *Trader) closePairArbPosition(pair *PairArbPosition, matchedExitPrice float64, residualExitPrice float64, matchedReason string, residualReason string) {
	if pair == nil {
		return
	}
	// Kalshi settles finalized contracts automatically.
	// No Polygon/CTF post-window claim safety check is required.
	closedAt := time.Now()
	lockedShares := pair.lockedShares()
	matchedCost := pair.matchedCost()
	matchedProceeds := lockedShares * matchedExitPrice
	if t.cfg.PaperTrade {
		t.paperBalance += matchedProceeds
	}
	t.appendPairArbTrade(pair, closedAt, "PAIR", lockedShares, matchedCost, matchedExitPrice, matchedProceeds, 0, matchedReason)

	residualSide, residualShares, residualAvgPrice := pair.residualPosition()
	if residualShares > 0 {
		residualCost := residualShares * residualAvgPrice
		residualProceeds := residualShares * residualExitPrice
		if t.cfg.PaperTrade {
			t.paperBalance += residualProceeds
		}
		t.appendPairArbTrade(pair, closedAt, residualSide, residualShares, residualCost, residualExitPrice, residualProceeds, 0, residualReason)
	}

	display.TradeClose("PAIR", matchedCost, matchedExitPrice, lockedShares, matchedProceeds-matchedCost, 0, matchedReason, time.Since(pair.OpenedAt).Round(time.Second), t.cfg.PaperTrade)
	t.pairedPosition = nil
	t.lastPairArbCloseAt = time.Now()
	t.detector.SetInPosition(false)
	t.savePositionState()
}

func (t *Trader) schedulePairArbPostWindowClaimSafetyCheck(pair *PairArbPosition) {
	// Legacy Polymarket CTF safety check.
	// Kalshi settlement is automatic.
	return
}

func (t *Trader) closePairArbResidual(ctx context.Context, currentYesPrice float64, reason string) error {
	pair := t.pairedPosition
	if pair == nil {
		return nil
	}
	residualSide, residualShares, residualAvgPrice := pair.residualPosition()
	if residualSide == "" || residualShares <= 0 {
		return nil
	}
	if residualShares < polymarket.MinOrderShares {
		return nil
	}
	residualTokenID := pair.YesTokenID
	residualExitPrice := math.Round(currentYesPrice*100) / 100
	if residualSide == "NO" {
		residualTokenID = pair.NoTokenID
		residualExitPrice = math.Round((1.0-currentYesPrice)*100) / 100
	}
	sellFeeUSD := 0.0
	proceeds := residualShares * residualExitPrice
	actualExitPrice := residualExitPrice
	if t.cfg.PaperTrade {
		if vwap, net, fee, filled := t.pairPaperSellQuote(ctx, residualTokenID, residualShares, residualExitPrice); filled {
			actualExitPrice, proceeds, sellFeeUSD = vwap, net, fee
		} else {
			return fmt.Errorf("pair arb: paper residual has insufficient executable bid depth")
		}
		t.paperBalance += proceeds
	} else {
		sellableShares, err := t.resolveSellableShares(ctx, residualTokenID, residualShares)
		if err != nil {
			return err
		}
		if sellableShares < polymarket.MinOrderShares {
			return nil
		}
		var (
			fillPrice float64
			filled    bool
		)
		var filledShares float64
		fillPrice, filledShares, filled, err = t.attemptSellWithFallback(ctx, residualTokenID, residualExitPrice, fmt.Sprintf("%.2f", sellableShares))
		if err != nil {
			return err
		}
		if !filled {
			return fmt.Errorf("pair arb: residual sell did not fill")
		}
		if filledShares <= pairArbShareDust {
			return fmt.Errorf("pair arb: residual sell returned zero fill")
		}
		if filledShares > sellableShares {
			filledShares = sellableShares
		}
		actualExitPrice = fillPrice
		sellFeeUSD = polymarket.ComputeSellFeeUSDC(filledShares, actualExitPrice, t.feeRateBps)
		proceeds = filledShares*actualExitPrice - sellFeeUSD
		residualShares = filledShares
	}
	closedAt := time.Now()
	residualCost := residualShares * residualAvgPrice
	t.appendPairArbTrade(pair, closedAt, residualSide, residualShares, residualCost, actualExitPrice, proceeds+sellFeeUSD, sellFeeUSD, reason)
	display.TradeClose(residualSide+" [PAIR RESIDUAL]", residualAvgPrice, actualExitPrice, residualShares, proceeds-residualCost, sellFeeUSD, reason, time.Since(pair.OpenedAt).Round(time.Second), t.cfg.PaperTrade)
	if residualSide == "YES" {
		pair.YesShares -= residualShares
		pair.YesUSDSpent -= residualCost
		if pair.YesShares < 0.01 {
			pair.YesShares = 0
			pair.YesUSDSpent = 0
		}
	} else {
		pair.NoShares -= residualShares
		pair.NoUSDSpent -= residualCost
		if pair.NoShares < 0.01 {
			pair.NoShares = 0
			pair.NoUSDSpent = 0
		}
	}
	if pair.lockedShares() <= 0 {
		t.pairedPosition = nil
		t.lastPairArbCloseAt = time.Now()
		t.detector.SetInPosition(false)
	} else {
		pair.BalancedAt = time.Now()
		pair.HedgeBy = time.Time{}
	}
	t.savePositionState()
	return nil
}

func (t *Trader) exitBothLegsPairArb(ctx context.Context, currentYesPrice, currentNoPrice float64, reason string) error {
	pair := t.pairedPosition
	if pair == nil {
		return nil
	}
	if currentNoPrice <= 0 {
		currentNoPrice = math.Round((1.0-currentYesPrice)*100) / 100
	}
	closedAt := time.Now()
	held := closedAt.Sub(pair.OpenedAt).Round(time.Second)

	sellSide := func(side, tokenID string, shares, avgEntryPrice, marketPrice float64) error {
		if shares <= 0 || tokenID == "" {
			return nil
		}
		startPrice := math.Round(marketPrice*100) / 100
		if t.cfg.PaperTrade {
			// Kalshi paper exits must use executable bid depth and Kalshi cash fees,
			// exactly like residual exits. Never invent a midpoint/signal-price fill.
			fillPrice, netProceeds, sellFee, filled := t.pairPaperSellQuote(ctx, tokenID, shares, startPrice)
			if !filled {
				return fmt.Errorf("pair exit %s: insufficient executable Kalshi bid depth", side)
			}
			grossProceeds := netProceeds + sellFee
			costBasis := shares * avgEntryPrice
			netPnL := netProceeds - costBasis
			t.paperBalance += netProceeds
			t.appendPairArbTrade(pair, closedAt, side, shares, costBasis, fillPrice, grossProceeds, sellFee, reason)
			display.TradeClose(side+" [PAIR EXIT]", avgEntryPrice, fillPrice, shares, netPnL, sellFee, reason, held, true)
			return nil
		}
		if shares < polymarket.MinOrderShares {
			return nil
		}
		sellable, err := t.resolveSellableShares(ctx, tokenID, shares)
		if err != nil {
			return fmt.Errorf("pair exit %s balance: %w", side, err)
		}
		if sellable < polymarket.MinOrderShares {
			return nil
		}
		fillPrice, filledShares, filled, err := t.attemptSellWithFallback(ctx, tokenID, startPrice, fmt.Sprintf("%.2f", sellable))
		if err != nil {
			return fmt.Errorf("pair exit %s sell: %w", side, err)
		}
		if !filled {
			return fmt.Errorf("pair exit %s sell did not fill", side)
		}
		if filledShares <= pairArbShareDust {
			return fmt.Errorf("pair exit %s sell returned zero fill", side)
		}
		if filledShares > sellable {
			filledShares = sellable
		}
		if filledShares < sellable-pairArbShareDust {
			return fmt.Errorf("pair exit %s partial fill %.2f/%.2f", side, filledShares, sellable)
		}
		sellFee := polymarket.ComputeSellFeeUSDC(filledShares, fillPrice, t.feeRateBps)
		grossProceeds := filledShares * fillPrice
		netProceeds := grossProceeds - sellFee
		costBasis := filledShares * avgEntryPrice
		netPnL := netProceeds - costBasis
		t.appendPairArbTrade(pair, closedAt, side, filledShares, costBasis, fillPrice, grossProceeds, sellFee, reason)
		display.TradeClose(side+" [PAIR EXIT]", avgEntryPrice, fillPrice, filledShares, netPnL, sellFee, reason, held, false)
		return nil
	}

	if err := sellSide("YES", pair.YesTokenID, pair.YesShares, pair.sideAveragePrice("YES"), currentYesPrice); err != nil {
		return err
	}
	if err := sellSide("NO", pair.NoTokenID, pair.NoShares, pair.sideAveragePrice("NO"), currentNoPrice); err != nil {
		return err
	}

	t.pairedPosition = nil
	t.lastPairArbCloseAt = time.Now()
	t.detector.SetInPosition(false)
	t.savePositionState()
	return nil
}

func (t *Trader) forceClosePairArb(ctx context.Context, currentYesPrice float64, reason string) error {
	if !t.tryBeginPairExitManagement() {
		// Another close path is already running (e.g. timeout ticks arriving while
		// a residual sell is in-flight). Skip to avoid duplicate SELL submissions.
		return nil
	}
	defer t.finishPairExitManagement()

	pair := t.pairedPosition
	if pair == nil {
		return nil
	}
	// Before force-closing, attempt to absorb any pre-placed hedge that may already be
	// matched. This is critical: if both legs are filled we must NOT sell one and leave
	// the other stranded. Absorbing the hedge fill makes the position balanced so the
	// normal close path handles both legs together (held to resolution or CLOB exit).
	if php := t.pendingHedgePrePlace; php != nil {
		absorbed := false
		if gr, grErr := t.getOrderTimed(ctx, "pair_arb_hedge_pre_poll_fc", php.OrderID); grErr == nil && gr != nil {
			fcStatus := strings.ToLower(strings.TrimSpace(gr.Status))
			if fcStatus == "matched" || fcStatus == "filled" {
				avg, gross, fErr := t.getFillsTimed(ctx, "pair_arb_hedge_pre_fills_fc", php.OrderID, php.TokenID)
				if fErr == nil && gross > 0 && avg > 0 {
					fee := polymarket.ComputeBuyFeeShares(gross, avg, t.feeRateBps)
					actual := math.Floor((gross-fee)*100) / 100
					usd := marketBuyOrderNotional(gross, avg)
					buySide, _, _ := pair.rebalanceState()
					if buySide == "YES" {
						pair.YesShares += actual
						pair.YesUSDSpent += usd
					} else if buySide == "NO" {
						pair.NoShares += actual
						pair.NoUSDSpent += usd
					}
					if pair.isBalanced() {
						pair.BalancedAt = time.Now()
						pair.HedgeBy = time.Time{}
					}
					t.clearPrePlacedHedgeOrder()
					t.savePositionState()
					absorbed = true
					t.logger.Info("trader: force-close: absorbed pre-placed hedge before closing — position is now balanced",
						zap.String("order_id", php.OrderID),
						zap.String("hedge_side", buySide),
						zap.Float64("fill_price", avg),
						zap.Float64("shares", actual),
						zap.Float64("locked_shares", pair.lockedShares()),
					)
				}
			}
		}
		if !absorbed {
			// Hedge not absorbed (still live → cancel it; or data unavailable).
			// Set orphan flag in case the cancel fails (already matched — tokens in wallet).
			t.pairArbHedgeOrphanFlag = true
			cancelErr := t.cancelOrderTimed(ctx, "pair_arb_dual_cancel_pre_place_"+reason, php.OrderID)
			if cancelErr != nil && errors.Is(cancelErr, polymarket.ErrOrderNotCancellable) {
				// Cancel race: order likely matched while we were closing. Queue explicit
				// hedge-credit reconciliation so the next lead is blocked until resolved.
				t.setPendingPairArbOrder(php.OrderID, php.TokenID, "pair_arb_hedge_credit_probe_force_close")
				t.logger.Warn("trader: force-close: pre-placed hedge not cancellable; queued credit reconciliation",
					zap.String("order_id", php.OrderID),
					zap.String("token_id", php.TokenID),
				)
			} else if cancelErr != nil {
				t.logger.Warn("trader: force-close: pre-placed hedge cancel failed; blocking next lead until reconciled",
					zap.Error(cancelErr),
					zap.String("order_id", php.OrderID),
				)
				t.setPendingPairArbOrder(php.OrderID, php.TokenID, "pair_arb_hedge_credit_probe_force_close")
			} else if gr, grErr := t.getOrderTimed(ctx, "pair_arb_hedge_pre_post_cancel_poll", php.OrderID); grErr == nil && gr != nil {
				postStatus := strings.ToLower(strings.TrimSpace(gr.Status))
				if postStatus == "matched" || postStatus == "filled" {
					t.setPendingPairArbOrder(php.OrderID, php.TokenID, "pair_arb_hedge_credit_probe_force_close")
					t.logger.Warn("trader: force-close: pre-placed hedge matched during cancel; queued credit reconciliation",
						zap.String("order_id", php.OrderID),
					)
				}
			}
			t.clearPrePlacedHedgeOrder()
		}
	}
	if residualSide, residualShares, _ := pair.residualPosition(); residualSide != "" && residualShares > 0 {
		if err := t.closePairArbResidual(ctx, currentYesPrice, reason); err != nil {
			return err
		}
		pair = t.pairedPosition
		if pair == nil {
			return nil
		}
		if remainingSide, remainingShares, _ := pair.residualPosition(); remainingSide != "" && remainingShares > pairArbShareDust {
			return fmt.Errorf("pair arb: forced close residual %s %.2f remains unsold", remainingSide, remainingShares)
		}
	}
	matchedExitPrice := math.Round((currentYesPrice+(1.0-currentYesPrice))*100) / 100
	t.closePairArbPosition(pair, matchedExitPrice, 0, reason, reason)
	t.lastPairArbCloseConditionID = pair.ConditionID
	if reason == "pair_stop_loss" {
		t.lastPairArbStopLossAt = time.Now()
	}
	return nil
}

func (t *Trader) enqueuePairArbClaimRetry(conditionID string, windowEnd time.Time, yesShares float64, noShares float64, winningIsNo bool, logReason string) {
	// Legacy Polymarket CTF claim retry.
	// Kalshi requires no claim transaction.
	return
}

func (t *Trader) ReconcilePairArbExitState(ctx context.Context) error {
	pair := t.pairedPosition
	if pair == nil || t.cfg.PaperTrade {
		return nil
	}
	if pair.WindowEnd.IsZero() || time.Now().Before(pair.WindowEnd.Add(pairArbSellClaimGrace)) {
		return nil
	}
	reconcileCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	resolvedYes := t.resolvePairArbResolvedYes(reconcileCtx, pair, false)
	t.setPairExitState(pair, pairExitStateCleanup, "reconciling stale pair exit state from persisted orders")
	t.settlePairArbPosition(reconcileCtx, resolvedYes)
	return nil
}

func (t *Trader) settlePairArbPosition(ctx context.Context, resolvedYes bool) {
	pair := t.pairedPosition
	if pair == nil {
		return
	}
	// Record settlement and close position accounting immediately Ã¢â‚¬â€ do NOT wait for
	// the on-chain CTF redemption, which can block the main loop for 60-90 seconds
	// (builder-relayer Node.js script + Polygon tx confirmation). Fire it in the
	// background; the USDC will arrive in the wallet regardless of timing.
	residualExitPrice := 0.0
	residualSide, _, _ := pair.residualPosition()
	if residualSide == "YES" && resolvedYes {
		residualExitPrice = 1.0
	}
	if residualSide == "NO" && !resolvedYes {
		residualExitPrice = 1.0
	}
	if t.cfg.PairArbSellAt99 {
		// Waiting-mode semantics: once both 0.99 GTC exits are live, do not
		// assume fills at window end. Require confirmation to prevent premature
		// close accounting and accidental re-entry while exits are unresolved.
		if pair.YesExitOrderID != "" && pair.NoExitOrderID != "" {
			t.schedulePairExitOrderStatusRefresh(pair)
			yesCanceled, yesKnown := t.getPairExitOrderStatus(pair.YesExitOrderID)
			noCanceled, noKnown := t.getPairExitOrderStatus(pair.NoExitOrderID)
			if yesCanceled {
				t.clearPairExitOrderStatus(pair.YesExitOrderID)
				pair.YesExitOrderID = ""
				pair.YesExitOrderShares = 0
			}
			if noCanceled {
				t.clearPairExitOrderStatus(pair.NoExitOrderID)
				pair.NoExitOrderID = ""
				pair.NoExitOrderShares = 0
			}
			if (yesKnown && yesCanceled) || (noKnown && noCanceled) {
				t.setPairExitState(pair, pairExitStatePlacingOrders, "replacing canceled pair exit order")
				t.savePositionState()
				if err := t.manageBalancedPairArbSellAt99(ctx, pair); err != nil {
					t.logger.Warn("trader: pair arb settle: unable to replace canceled sell-at-99 order", zap.Error(err))
				}
				return
			}
			if !pair.WindowEnd.IsZero() && time.Now().Before(pair.WindowEnd) {
				remaining := pair.WindowEnd.Sub(time.Now())
				if remaining > 0 && remaining <= pairArbSellAggressiveLead {
					t.aggressiveRepostPairArbExitOrders(ctx, pair, remaining)
				}
				t.setPairExitState(pair, pairExitStateOrdersLive, "orders live; waiting for window completion")
				return
			}
			yesFilled, _, yesErr := t.pairArbExitOrderFilled(ctx, pair.YesExitOrderID, pair.YesTokenID, pair.YesExitOrderShares, "pair_arb_sell_99_yes_window_end_settle")
			if yesErr != nil {
				t.logger.Warn("trader: pair arb settle: YES window-end sell-at-99 lookup failed", zap.Error(yesErr))
				return
			}
			noFilled, _, noErr := t.pairArbExitOrderFilled(ctx, pair.NoExitOrderID, pair.NoTokenID, pair.NoExitOrderShares, "pair_arb_sell_99_no_window_end_settle")
			if noErr != nil {
				t.logger.Warn("trader: pair arb settle: NO window-end sell-at-99 lookup failed", zap.Error(noErr))
				return
			}
			if yesFilled || noFilled {
				settled, settleErr := t.pairArbFilledLegsSettled(ctx, pair, yesFilled, noFilled)
				if settleErr != nil {
					t.logger.Warn("trader: pair arb settle: window-end settlement probe failed", zap.Error(settleErr))
					return
				}
				if !settled {
					t.setPairExitState(pair, pairExitStateGraceWait, "window complete; fill seen, awaiting settlement confirmation")
					return
				}
				walletFlat, yesBal, noBal, balErr := t.pairArbWalletExitConfirmed(ctx, pair)
				if balErr != nil {
					t.logger.Warn("trader: pair arb settle: wallet exit confirmation failed", zap.Error(balErr))
					return
				}
				if !walletFlat {
					winningBal := yesBal
					losingBal := noBal
					if !resolvedYes {
						winningBal = noBal
						losingBal = yesBal
					}
					if winningBal <= pairArbSettleBalanceDust && losingBal > pairArbSettleBalanceDust {
						t.setPairExitState(pair, pairExitStateResolvedCLOB, fmt.Sprintf("resolved winner settled; closing with losing residual marked to $0 (YES=%.2f NO=%.2f)", yesBal, noBal))
						t.clearPairArbExitOrders(pair)
						t.closePairArbPosition(pair, 0.99, 0.0, "pair_sell_at_99", "pair_sell_at_99")
						return
					}
					t.setPairExitState(pair, pairExitStateGraceWait, fmt.Sprintf("window complete; waiting full-size exit confirmation (YES=%.2f NO=%.2f)", yesBal, noBal))
					return
				}
				if residualExitPrice == 1.0 {
					residualExitPrice = 0.99
				}
				t.setPairExitState(pair, pairExitStateResolvedCLOB, "window complete; sell-at-99 fill confirmed")
				t.clearPairArbExitOrders(pair)
				t.closePairArbPosition(pair, 0.99, residualExitPrice, "pair_sell_at_99", "pair_sell_at_99")
				return
			}
			t.setPairExitState(pair, pairExitStateGraceWait, "window complete; exit fills not confirmed yet")
			return
		}
		if pair.YesExitOrderID == "" || pair.NoExitOrderID == "" {
			if err := t.manageBalancedPairArbSellAt99(ctx, pair); err != nil {
				t.logger.Warn("trader: pair arb settle: unable to refresh sell-at-99 orders", zap.Error(err))
			}
		}
		yesFilled, _, yesErr := t.pairArbExitOrderFilled(ctx, pair.YesExitOrderID, pair.YesTokenID, pair.YesExitOrderShares, "pair_arb_sell_99_yes_settle")
		if yesErr != nil {
			t.logger.Warn("trader: pair arb settle: YES sell-at-99 lookup failed", zap.Error(yesErr))
		}
		noFilled, _, noErr := t.pairArbExitOrderFilled(ctx, pair.NoExitOrderID, pair.NoTokenID, pair.NoExitOrderShares, "pair_arb_sell_99_no_settle")
		if noErr != nil {
			t.logger.Warn("trader: pair arb settle: NO sell-at-99 lookup failed", zap.Error(noErr))
		}
		if yesFilled || noFilled {
			settled, settleErr := t.pairArbFilledLegsSettled(ctx, pair, yesFilled, noFilled)
			if settleErr != nil {
				t.logger.Warn("trader: pair arb settle: balance settlement probe failed", zap.Error(settleErr))
				return
			}
			if !settled {
				t.setPairExitState(pair, pairExitStateGraceWait, "fill reported; awaiting balance settlement confirmation")
				// Do not return here: if settlement never converges due to tiny dust,
				// we still want the grace-expiry fallback below to fire.
			} else {
				walletFlat, yesBal, noBal, balErr := t.pairArbWalletExitConfirmed(ctx, pair)
				if balErr != nil {
					t.logger.Warn("trader: pair arb settle: wallet exit confirmation failed", zap.Error(balErr))
					return
				}
				if !walletFlat {
					winningBal := yesBal
					losingBal := noBal
					if !resolvedYes {
						winningBal = noBal
						losingBal = yesBal
					}
					if winningBal <= pairArbSettleBalanceDust && losingBal > pairArbSettleBalanceDust {
						t.setPairExitState(pair, pairExitStateResolvedCLOB, fmt.Sprintf("resolved winner settled; closing with losing residual marked to $0 (YES=%.2f NO=%.2f)", yesBal, noBal))
						t.clearPairArbExitOrders(pair)
						t.closePairArbPosition(pair, 0.99, 0.0, "pair_sell_at_99", "pair_sell_at_99")
						return
					}
					t.setPairExitState(pair, pairExitStateGraceWait, fmt.Sprintf("waiting full-size exit confirmation (YES=%.2f NO=%.2f)", yesBal, noBal))
					return
				}
				if residualExitPrice == 1.0 {
					residualExitPrice = 0.99
				}
				t.setPairExitState(pair, pairExitStateResolvedCLOB, "sell-at-99 fill confirmed during settlement")
				t.clearPairArbExitOrders(pair)
				t.closePairArbPosition(pair, 0.99, residualExitPrice, "pair_sell_at_99", "pair_sell_at_99")
				return
			}
		}
		graceStart := pair.ExitOrdersPlacedAt
		if graceStart.IsZero() {
			graceStart = pair.WindowEnd
		}
		if !graceStart.IsZero() && time.Since(graceStart) < pairArbSellClaimGrace {
			remaining := pairArbSellClaimGrace - time.Since(graceStart)
			if remaining < 0 {
				remaining = 0
			}
			t.setPairExitState(pair, pairExitStateGraceWait, fmt.Sprintf("waiting %.0fs more before claim fallback", remaining.Seconds()))
			return
		}
		t.setPairExitState(pair, pairExitStateCleanup, "sell-at-99 grace expired; canceling live orders before claim")
		if err := t.cancelPairArbExitOrder(ctx, pair.YesExitOrderID, "pair_arb_sell_99_settle_cancel_yes"); err != nil {
			t.logger.Warn("trader: pair arb settle: cancel YES sell-at-99 failed", zap.Error(err))
		}
		if err := t.cancelPairArbExitOrder(ctx, pair.NoExitOrderID, "pair_arb_sell_99_settle_cancel_no"); err != nil {
			t.logger.Warn("trader: pair arb settle: cancel NO sell-at-99 failed", zap.Error(err))
		}
		t.clearPairArbExitOrders(pair)
		t.setPairExitState(pair, pairExitStateResolvedClaim, "Kalshi market finalized; accounting at settlement value")
		t.closePairArbPosition(pair, 1.0, residualExitPrice, "pair_window_resolved", "pair_unhedged_resolved")
		return
	}

	// Kalshi hold-to-settlement path.
	// Kalshi credits finalized contracts automatically; there is no CTF claim.
	t.setPairExitState(pair, pairExitStateResolvedClaim, "Kalshi market finalized; settlement automatic")
	t.closePairArbPosition(pair, 1.0, residualExitPrice, "pair_window_resolved", "pair_unhedged_resolved")
}

//
// Per-window trade journal
//

// ResetJournal archives current window trades into the session journal
// and clears the per-window log for the next window.
func (t *Trader) ResetJournal() {
	t.sessionJournal = append(t.sessionJournal, t.journal...)
	t.windowsPlayed++
	t.journal = t.journal[:0]
}

// WindowTrades returns a snapshot of completed trades from the current window.
// Call this BEFORE the next window's ResetJournal wipes the slice  i.e. right
// after PrintWindowSummary and before waitForNextWindow returns.
func (t *Trader) WindowTrades() []TradeRecord {
	return CollapseTradeRecords(t.journal)
}

// PrintWindowSummary emits a visual end-of-window P&L summary.
// Call this just before tearing down per-window resources.
func (t *Trader) PrintWindowSummary(slug string, openPrice float64) {
	collapsed := CollapseTradeRecords(t.journal)
	trades := make([]display.TradeInfo, len(collapsed))
	for i, tr := range collapsed {
		trades[i] = display.TradeInfo{
			Num:       i + 1,
			Strategy:  tr.Strategy,
			Side:      tr.Side,
			BuyPrice:  tr.BuyPrice,
			SellPrice: tr.SellPrice,
			Shares:    tr.Shares,
			USDSpent:  tr.USDSpent,
			PnL:       tr.PnL,
			Reason:    tr.Reason,
			Held:      time.Duration(tr.HeldSec * float64(time.Second)),
		}
	}
	display.WindowSummary(slug, openPrice, trades)
}

// SessionStats returns cumulative session statistics for the session summary.
func (t *Trader) SessionStats() display.SessionStats {
	// Combine already-archived trades + current window's trades
	all := make([]TradeRecord, 0, len(t.sessionJournal)+len(t.journal))
	for _, rec := range t.sessionJournal {
		if isExternalReconcileRecord(rec) {
			continue
		}
		all = append(all, rec)
	}
	for _, rec := range t.journal {
		if isExternalReconcileRecord(rec) {
			continue
		}
		all = append(all, rec)
	}
	all = CollapseTradeRecords(all)

	stats := display.SessionStats{
		StartedAt:     t.startedAt,
		WindowsPlayed: t.windowsPlayed,
		TotalTrades:   len(all),
		PaperBalance:  0,
		PaperStart:    0,
	}
	if t.cfg.PaperTrade {
		stats.PaperBalance = t.paperBalance
		stats.PaperStart = t.cfg.PaperStartBalance
	}

	if len(all) == 0 {
		return stats
	}

	var totalHeld time.Duration
	stats.BestTradePnL = all[0].PnL
	stats.WorstTradePnL = all[0].PnL

	for _, tr := range all {
		stats.TotalPnL += tr.PnL
		stats.TotalSpent += tr.USDSpent
		totalHeld += time.Duration(tr.HeldSec * float64(time.Second))
		if tr.PnL > 0 {
			stats.Wins++
		} else {
			stats.Losses++
		}
		if tr.PnL > stats.BestTradePnL {
			stats.BestTradePnL = tr.PnL
		}
		if tr.PnL < stats.WorstTradePnL {
			stats.WorstTradePnL = tr.PnL
		}
	}
	stats.AvgHeld = totalHeld / time.Duration(len(all))
	return stats
}

// PrintSessionSummary prints the cumulative session overview.
func (t *Trader) PrintSessionSummary() {
	display.SessionSummary(t.SessionStats())
}

// savePositionState persists the full trading state to disk atomically (write-tmp + rename).
// When position is nil (flat) the file reflects that so RestorePosition returns clean.
// No-op in paper-trade mode.
func (t *Trader) savePositionState() {
	if t.cfg.PaperTrade {
		return
	}
	t.claimMu.Lock()
	pendingClaims := append([]pendingClaim(nil), t.pendingClaims...)
	t.claimMu.Unlock()
	t.isolatedPendingMu.Lock()
	isolatedPending := append([]pairArbPendingOrderState(nil), t.isolatedPending...)
	t.isolatedPendingMu.Unlock()
	type posState struct {
		Position             *Position                  `json:"position"`
		PairPosition         *PairArbPosition           `json:"pair_position,omitempty"`
		PendingBuy           *pendingBuyState           `json:"pending_buy,omitempty"`
		PendingPairArb       *pairArbPendingOrderState  `json:"pending_pair_arb,omitempty"`
		PendingHedgePrePlace *pairArbPendingOrderState  `json:"pending_hedge_pre_place,omitempty"`
		IsolatedPending      []pairArbPendingOrderState `json:"isolated_pending_orders,omitempty"`
		PendingClaims        []pendingClaim             `json:"pending_claims,omitempty"`
		BotConditionIDs      []string                   `json:"bot_condition_ids,omitempty"`
		FeeRateBps           string                     `json:"fee_rate_bps"`
	}
	botConditions := make([]string, 0, len(t.botConditionIDs))
	for key := range t.botConditionIDs {
		botConditions = append(botConditions, key)
	}
	b, err := json.Marshal(posState{
		Position:             t.position,
		PairPosition:         t.pairedPosition,
		PendingBuy:           t.pendingBuy,
		PendingPairArb:       t.pendingPairArb,
		PendingHedgePrePlace: t.pendingHedgePrePlace,
		IsolatedPending:      isolatedPending,
		PendingClaims:        pendingClaims,
		BotConditionIDs:      botConditions,
		FeeRateBps:           t.feeRateBps,
	})
	if err != nil {
		t.logger.Error("trader: failed to marshal position state", zap.Error(err))
		return
	}
	// Atomic write: write to .tmp then rename to avoid partial-write corruption.
	tmp := t.stateFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		t.logger.Error("trader: failed to write position state tmp file",
			zap.String("file", tmp), zap.Error(err))
		return
	}
	if err := os.Rename(tmp, t.stateFile); err != nil {
		t.logger.Error("trader: failed to rename position state file",
			zap.String("file", t.stateFile), zap.Error(err))
	}
}

// RestorePosition loads state persisted by a previous run and re-activates any open position.
// Handles four scenarios:
//  1. Clean restart (no state file)          Ã‚Â  returns false, no-op
//  2. PendingBuy present (crash between PlaceOrder and position-open)
//     queries CLOB; reconstructs position if filled, discards if not
//  3. Position present (crash while holding)
//     cancels any lingering sell order, validates balance, restores
//  4. FlashPosition present
//     restores without affecting the lag detector flag
//
// Call once after NewTrader, before the main event loop.
func (t *Trader) RestorePosition(ctx context.Context) bool {
	if t.cfg.PaperTrade {
		return false
	}
	b, err := os.ReadFile(t.stateFile)
	if err != nil {
		return false // clean start (no state file)
	}
	type posState struct {
		Position             *Position                  `json:"position"`
		PairPosition         *PairArbPosition           `json:"pair_position,omitempty"`
		PendingBuy           *pendingBuyState           `json:"pending_buy,omitempty"`
		PendingPairArb       *pairArbPendingOrderState  `json:"pending_pair_arb,omitempty"`
		PendingHedgePrePlace *pairArbPendingOrderState  `json:"pending_hedge_pre_place,omitempty"`
		IsolatedPending      []pairArbPendingOrderState `json:"isolated_pending_orders,omitempty"`
		PendingClaims        []pendingClaim             `json:"pending_claims,omitempty"`
		BotConditionIDs      []string                   `json:"bot_condition_ids,omitempty"`
		FeeRateBps           string                     `json:"fee_rate_bps"`
	}
	var s posState
	if err := json.Unmarshal(b, &s); err != nil {
		t.logger.Error("trader: corrupt position state file; ignoring",
			zap.String("file", t.stateFile), zap.Error(err))
		return false
	}
	if s.FeeRateBps != "" {
		t.feeRateBps = s.FeeRateBps
	}
	if len(s.BotConditionIDs) > 0 {
		for _, key := range s.BotConditionIDs {
			k := normalizeConditionID(key)
			if k == "" {
				continue
			}
			t.botConditionIDs[k] = struct{}{}
		}
	}
	t.pendingPairArb = s.PendingPairArb
	if t.pendingPairArb != nil {
		if strings.TrimSpace(t.pendingPairArb.Origin) == "" {
			t.pendingPairArb.Origin = "restored_from_state"
			t.savePositionState()
		}
		pendingAge := time.Since(t.pendingPairArb.PlacedAt)
		if t.pendingPairArb.PlacedAt.IsZero() || pendingAge < 0 {
			pendingAge = 0
		}
		t.logger.Warn("trader: restored pending pair arb order from previous run",
			zap.String("order_id", t.pendingPairArb.OrderID),
			zap.String("token_id", t.pendingPairArb.TokenID),
			zap.String("request", t.pendingPairArb.RequestName),
			zap.String("origin", t.pendingPairArb.Origin),
			zap.Time("placed_at", t.pendingPairArb.PlacedAt),
			zap.Duration("age", pendingAge),
		)
	}
	t.pendingHedgePrePlace = s.PendingHedgePrePlace
	if t.pendingHedgePrePlace != nil {
		t.logger.Warn("trader: restored pending hedge pre-place order from previous run",
			zap.String("order_id", t.pendingHedgePrePlace.OrderID),
			zap.String("token_id", t.pendingHedgePrePlace.TokenID),
		)
		// If there is no paired position, this is an orphan (crash between hedge submit and
		// lead fill / position-open).  Cancel the order immediately so it doesn't fill into
		// a position we know nothing about.
		if s.PairPosition == nil {
			_ = t.cancelOrderTimed(ctx, "pair_arb_hedge_pre_cancel_orphan", t.pendingHedgePrePlace.OrderID)
			t.pendingHedgePrePlace = nil
			t.savePositionState()
		}
	}
	if len(s.IsolatedPending) > 0 {
		restored := 0
		for i := range s.IsolatedPending {
			entry := s.IsolatedPending[i]
			if strings.TrimSpace(entry.OrderID) == "" || strings.TrimSpace(entry.TokenID) == "" {
				continue
			}
			t.watchIsolatedPendingOrder(&entry, "restore_isolated_pending")
			restored++
		}
		if restored > 0 {
			t.logger.Warn("trader: restored isolated pending ownership watches from previous run",
				zap.Int("isolated_pending", restored),
			)
		}
	}
	if len(s.PendingClaims) > 0 {
		t.logger.Info(
			"trader: clearing legacy Polymarket pending claims from Kalshi state",
			zap.Int("pending_claims", len(s.PendingClaims)),
		)
		s.PendingClaims = nil
		t.pendingClaims = nil
		t.savePositionState()
	}
	if s.PairPosition != nil {
		pair := s.PairPosition
		t.rememberBotConditionID(pair.ConditionID)
		t.pairedPosition = pair
		t.detector.SetInPosition(true)
		if err := t.ReconcilePairArbExitState(ctx); err != nil {
			t.logger.Warn("trader: restored pair arb reconciliation failed", zap.Error(err))
		}
		if t.pairedPosition == nil {
			return false
		}
		pair = t.pairedPosition
		if !pair.WindowEnd.IsZero() && time.Now().After(pair.WindowEnd.Add(10*time.Minute)) {
			t.logger.Warn("trader: restored pair arb window ended >10m ago; discarding unresolved state",
				zap.Time("window_end", pair.WindowEnd),
			)
			t.pairedPosition = nil
			t.savePositionState()
			return false
		}
		t.savePositionState()
		t.logger.Warn("trader: restored paired arbitrage position from previous run",
			zap.String("lead_side", pair.LeadSide),
			zap.Float64("yes_shares", pair.YesShares),
			zap.Float64("no_shares", pair.NoShares),
			zap.Time("window_end", pair.WindowEnd),
		)
		return true
	}

	//  Scenario 2: PendingBuy  buy was placed but crash happened before position saved
	if s.PendingBuy != nil && s.Position == nil {
		pb := s.PendingBuy
		t.logger.Warn("trader: found pending buy intent from previous run; querying CLOB",
			zap.String("order_id", pb.OrderID))
		gr, grErr := t.getOrderTimed(ctx, "pending_buy_lookup", pb.OrderID)
		if grErr == nil && (strings.EqualFold(gr.Status, "MATCHED") || strings.EqualFold(gr.Status, "FILLED")) {
			// The buy filled during the crash window  reconstruct the position.
			t.logger.Warn("trader: pending buy was filled; reconstructing position",
				zap.String("order_id", pb.OrderID),
				zap.Float64("shares", pb.ActualShares))
			// Kalshi confirmed fill requires no Polygon settlement wait.
			s.Position = &Position{
				OrderID:     pb.OrderID,
				TokenID:     pb.TokenID,
				IsNoSide:    pb.IsNoSide,
				BuyPrice:    pb.FillPrice,
				Shares:      pb.ActualShares,
				USDSpent:    pb.RawShares * pb.FillPrice,
				FeeShares:   pb.FeeShares,
				TargetPrice: pb.TargetPrice,
				OpenPrice:   0, // unknown after crash; conservative (won't trigger chainlink exit)
				WindowEnd:   pb.WindowEnd,
				OpenedAt:    pb.PlacedAt,
				ExpiresAt:   pb.ExpiresAt,
			}
		} else {
			// Not filled (LIVE, CANCELED, or error)  cancel if live and discard.
			if grErr == nil && strings.EqualFold(gr.Status, "LIVE") {
				_ = t.cancelOrderTimed(ctx, "pending_buy_cancel", pb.OrderID)
			}
			t.logger.Warn("trader: pending buy not filled; discarded",
				zap.String("order_id", pb.OrderID),
				zap.String("status", func() string {
					if grErr != nil {
						return "query_error"
					}
					return gr.Status
				}()))
			t.pendingBuy = nil
			t.savePositionState() // clear pending buy from file
			return false          // nothing to restore
		}
	}

	pos := s.Position
	if pos == nil {
		return false // no position in state file
	}
	t.rememberBotConditionID(pos.ConditionID)
	// Cancel any lingering GTC sell order from before the crash.
	if pos.ActiveSellOrderID != "" {
		_ = t.cancelOrderTimed(ctx, "restore_cancel_stale_sell", pos.ActiveSellOrderID)
		pos.ActiveSellOrderID = ""
	}
	// Reset in-flight sell flags  their outcome is unknown after a restart.
	pos.SellPending = false
	pos.SellDelayUntil = time.Time{}

	//  Zero-shares guard
	// Shares==0 means the FAK buy was placed but never filled; the position state
	// was written before fill confirmation. There are no tokens to sell Ã‚Â  discard.
	if pos.Shares == 0 {
		t.logger.Warn("trader: restored position has Shares=0 (buy never filled); discarding",
			zap.String("order_id", pos.OrderID),
			zap.String("token_id", pos.TokenID),
		)
		t.savePositionState()
		return false
	}

	//  Stale window guard
	// If the position's window ended more than 10 minutes ago, the market has
	// irrevocably resolved (CTF settlement). The CLOB won't accept a sell;
	// discard immediately rather than looping forever.
	if !pos.WindowEnd.IsZero() && time.Now().After(pos.WindowEnd.Add(10*time.Minute)) {
		t.logger.Warn("trader: restored position window ended >10m ago (market resolved); discarding",
			zap.String("token_id", pos.TokenID),
			zap.Time("window_end", pos.WindowEnd),
		)
		t.savePositionState()
		return false
	}

	// Kalshi restart ghost check.
	// Verify that the authoritative Kalshi portfolio still reports exposure
	// on the expected outcome. This replaces the Polymarket ERC1155 balance check.
	if t.orders != nil && pos.ConditionID != "" {
		if positions, pErr := t.orders.FetchCurrentPositions(ctx, 500, 0); pErr == nil {
			foundShares := 0.0

			for i := range positions {
				p := &positions[i]

				if !strings.EqualFold(
					strings.TrimSpace(p.ConditionID),
					strings.TrimSpace(pos.ConditionID),
				) {
					continue
				}

				isNo, ok := inferNoSideFromOutcomes(
					p.Outcome,
					p.OppositeOutcome,
					p.OutcomeIndex,
				)
				if !ok || isNo != pos.IsNoSide {
					continue
				}

				if p.Size > foundShares {
					foundShares = p.Size
				}
			}

			if foundShares < pos.Shares*0.5 {
				t.logger.Warn(
					"trader: restored Kalshi position not found in portfolio; discarding stale state",
					zap.String("condition_id", pos.ConditionID),
					zap.Float64("expected_shares", pos.Shares),
					zap.Float64("portfolio_shares", foundShares),
				)
				t.position = nil
				t.savePositionState()
				return false
			}
		} else {
			t.logger.Warn(
				"trader: Kalshi portfolio verification unavailable during restore; preserving position conservatively",
				zap.Error(pErr),
			)
		}
	}

	t.position = pos
	t.detector.SetInPosition(true)
	t.savePositionState() // commit cleaned-up state (no pending sell order ID)
	t.logger.Warn("trader: RESTORED open lag position from previous run",
		zap.String("token_id", pos.TokenID),
		zap.Float64("shares", pos.Shares),
		zap.Float64("buy_price", pos.BuyPrice),
		zap.Time("expires_at", pos.ExpiresAt),
	)
	return true
}
