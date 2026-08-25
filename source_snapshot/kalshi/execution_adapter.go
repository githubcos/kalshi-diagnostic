package kalshi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/polyarb/polymarket"
)

// ExecutionAdapter translates the original PolyArb logical order operations
// into Kalshi V2 operations.
//
// Strategy logic remains unchanged. Only exchange execution is translated.
type ExecutionAdapter struct {
	Client *Client
}

func NewExecutionAdapter(client *Client) *ExecutionAdapter {
	return &ExecutionAdapter{Client: client}
}

func (a *ExecutionAdapter) requireClient() error {
	if a == nil || a.Client == nil {
		return fmt.Errorf("kalshi execution: nil client")
	}
	return nil
}

func parseFloat(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}

func (a *ExecutionAdapter) PlaceOrder(
	ctx context.Context,
	req *polymarket.NewOrderRequest,
) (*polymarket.OrderResponse, error) {
	if err := a.requireClient(); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, fmt.Errorf("kalshi execution: nil order request")
	}

	price, err := strconv.ParseFloat(strings.TrimSpace(req.Price), 64)
	if err != nil {
		return nil, fmt.Errorf("kalshi execution: invalid price %q: %w", req.Price, err)
	}

	size, err := strconv.ParseFloat(strings.TrimSpace(req.Size), 64)
	if err != nil {
		return nil, fmt.Errorf("kalshi execution: invalid size %q: %w", req.Size, err)
	}

	action := "BUY"
	if req.Side == polymarket.SideSell {
		action = "SELL"
	}

	logical := LogicalOrderRequest{
		TokenID:    req.TokenID,
		Action:     action,
		OrderType:  string(req.OrderType),
		Price:      price,
		Size:       size,
		Expiration: req.Expiration,
	}

	kr, err := BuildCreateOrderRequest(logical)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	out, err := a.Client.CreateOrder(ctx, kr)
	httpMs := time.Since(start).Milliseconds()

	if err != nil {
		return nil, err
	}

	status := "LIVE"

	fillCount := parseFloat(out.FillCount)
	remaining := parseFloat(out.RemainingCount)

	if fillCount > 0 && remaining <= 0 {
		status = "MATCHED"
	}

	return &polymarket.OrderResponse{
		Success:  true,
		OrderID:  out.OrderID,
		Status:   status,
		HTTPMs:   httpMs,
		Attempts: 1,
	}, nil
}

func (a *ExecutionAdapter) CancelOrder(
	ctx context.Context,
	orderID string,
) error {
	if err := a.requireClient(); err != nil {
		return err
	}

	_, err := a.Client.CancelOrderV2(ctx, orderID)
	if err != nil {
		return err
	}

	return nil
}

func (a *ExecutionAdapter) getKalshiOrderResilient(
	ctx context.Context,
	orderID string,
	ticker string,
) (*Order, error) {
	if err := a.requireClient(); err != nil {
		return nil, err
	}

	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return nil, fmt.Errorf("kalshi execution: empty order id")
	}

	// Fast path.
	o, directErr := a.Client.GetOrder(ctx, orderID)
	if directErr == nil && o != nil {
		return o, nil
	}

	// Kalshi's single-order endpoint can briefly return 404 directly
	// after an IOC/FAK execution. During that visibility gap, recover
	// the exact order from recent portfolio orders.
	for attempt := 0; attempt < 8; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		orders, listErr := a.Client.GetOrders(ctx, ticker, 200)
		if listErr == nil {
			for i := range orders {
				if strings.EqualFold(
					strings.TrimSpace(orders[i].OrderID),
					orderID,
				) {
					return &orders[i], nil
				}
			}
		}

		if attempt < 7 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(150 * time.Millisecond):
			}
		}
	}

	// Preserve the original direct lookup error if recovery failed.
	if directErr != nil {
		return nil, directErr
	}

	return nil, nil
}

func (a *ExecutionAdapter) GetOrder(
	ctx context.Context,
	orderID string,
) (*polymarket.GetOrderResponse, error) {
	if err := a.requireClient(); err != nil {
		return nil, err
	}

	o, err := a.getKalshiOrderResilient(ctx, orderID, "")
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, nil
	}

	tokenID := logicalTokenID(o.Ticker, "YES")
	if strings.EqualFold(o.OutcomeSide, "no") {
		tokenID = logicalTokenID(o.Ticker, "NO")
	}

	return a.kalshiOrderToCompat(o, tokenID)
}

func (a *ExecutionAdapter) GetFillsByTakerOrderID(
	ctx context.Context,
	orderID string,
	tokenID string,
) (avgPrice float64, grossShares float64, err error) {
	if err := a.requireClient(); err != nil {
		return 0, 0, err
	}

	ticker, _, parseErr := parseLogicalTokenID(tokenID)
	if parseErr != nil {
		return 0, 0, parseErr
	}

	o, err := a.getKalshiOrderResilient(ctx, orderID, ticker)
	if err != nil {
		return 0, 0, err
	}
	if o == nil {
		return 0, 0, nil
	}

	n, err := o.NormalizeForLogicalToken(tokenID)
	if err != nil {
		return 0, 0, err
	}

	if n.FilledContracts <= 0 {
		return 0, 0, nil
	}

	return n.AverageFillPrice, n.FilledContracts, nil
}

func (a *ExecutionAdapter) GetTickSize(
	ctx context.Context,
	tokenID string,
) (string, error) {
	if err := a.requireClient(); err != nil {
		return "", err
	}

	ticker, _, err := parseLogicalTokenID(tokenID)
	if err != nil {
		return "", err
	}

	m, err := a.Client.GetMarket(ctx, ticker)
	if err != nil {
		return "", err
	}

	if m.MinTickSize <= 0 {
		return "0.01", nil
	}

	return strconv.FormatFloat(m.MinTickSize, 'f', -1, 64), nil
}

func (a *ExecutionAdapter) FetchUSDCBalance(
	ctx context.Context,
) (float64, error) {
	if err := a.requireClient(); err != nil {
		return 0, err
	}

	bal, err := a.Client.GetBalance(ctx)
	if err != nil {
		return 0, err
	}

	return bal.AvailableUSD(), nil
}

// These two methods are used only for ambiguous-submit recovery.
// We will implement them using Kalshi order-history queries before enabling LIVE.
func (a *ExecutionAdapter) FindRecentOrder(
	ctx context.Context,
	tokenID string,
) (*polymarket.GetOrderResponse, error) {
	if err := a.requireClient(); err != nil {
		return nil, err
	}

	ticker, outcome, err := parseLogicalTokenID(tokenID)
	if err != nil {
		return nil, err
	}

	orders, err := a.Client.GetOrders(ctx, ticker, 100)
	if err != nil {
		return nil, err
	}

	for _, o := range orders {
		if !strings.EqualFold(o.Ticker, ticker) {
			continue
		}

		if outcome != "" && o.OutcomeSide != "" &&
			!strings.EqualFold(o.OutcomeSide, outcome) {
			continue
		}

		return a.kalshiOrderToCompat(&o, tokenID)
	}

	return nil, nil
}

func (a *ExecutionAdapter) FindRecentTrade(
	ctx context.Context,
	tokenID string,
) (*polymarket.GetOrderResponse, error) {
	if err := a.requireClient(); err != nil {
		return nil, err
	}

	ticker, outcome, err := parseLogicalTokenID(tokenID)
	if err != nil {
		return nil, err
	}

	orders, err := a.Client.GetOrders(ctx, ticker, 100)
	if err != nil {
		return nil, err
	}

	for _, o := range orders {
		if !strings.EqualFold(o.Ticker, ticker) {
			continue
		}
		if parseFloat(o.FillCountFP) <= 0 {
			continue
		}
		if outcome != "" && o.OutcomeSide != "" &&
			!strings.EqualFold(o.OutcomeSide, outcome) {
			continue
		}

		return a.kalshiOrderToCompat(&o, tokenID)
	}

	return nil, nil
}

func (a *ExecutionAdapter) kalshiOrderToCompat(
	o *Order,
	logicalTokenIDHint string,
) (*polymarket.GetOrderResponse, error) {
	if o == nil {
		return nil, nil
	}

	tokenID := logicalTokenIDHint
	if tokenID == "" {
		outcome := strings.ToUpper(strings.TrimSpace(o.OutcomeSide))
		if outcome != "NO" {
			outcome = "YES"
		}
		tokenID = logicalTokenID(o.Ticker, outcome)
	}

	_, outcome, err := parseLogicalTokenID(tokenID)
	if err != nil {
		return nil, err
	}

	price := o.YesPriceDollars
	if outcome == "NO" {
		price = o.NoPriceDollars
		if parseFloat(price) <= 0 {
			yp := parseFloat(o.YesPriceDollars)
			if yp > 0 {
				price = strconv.FormatFloat(1.0-yp, 'f', 4, 64)
			}
		}
	}

	status := strings.ToUpper(strings.TrimSpace(o.Status))
	switch status {
	case "RESTING", "OPEN", "LIVE":
		status = "LIVE"
	case "EXECUTED", "FILLED":
		status = "MATCHED"
	case "CANCELED", "CANCELLED":
		status = "CANCELED"
	}

	side := strings.ToUpper(strings.TrimSpace(o.Action))
	if side == "" {
		if strings.EqualFold(o.OutcomeSide, outcome) {
			side = "BUY"
		}
	}

	return &polymarket.GetOrderResponse{
		ID:            o.OrderID,
		Status:        status,
		Market:        o.Ticker,
		TokenID:       tokenID,
		Side:          side,
		Price:         price,
		OriginalSize:  o.InitialCountFP,
		SizeMatched:   o.FillCountFP,
		SizeRemaining: o.RemainingCountFP,
	}, nil
}

// Position cost-basis reconciliation requires additional Kalshi position fields.
// Do not synthesize cost basis.
func (a *ExecutionAdapter) FetchCurrentPositions(
	ctx context.Context,
	limit int,
	offset int,
) ([]polymarket.UserPosition, error) {
	if err := a.requireClient(); err != nil {
		return nil, err
	}

	items, err := a.Client.GetPositions(ctx, "")
	if err != nil {
		return nil, err
	}

	out := make([]polymarket.UserPosition, 0, len(items))

	for _, kp := range items {
		raw, err := strconv.ParseFloat(strings.TrimSpace(kp.PositionFP), 64)
		if err != nil || math.Abs(raw) <= 0.000001 {
			continue
		}

		isNo := raw < 0
		size := math.Abs(raw)

		outcome := "Yes"
		opposite := "No"
		index := 0

		if isNo {
			outcome = "No"
			opposite = "Yes"
			index = 1
		}

		currentValue, _ := strconv.ParseFloat(
			strings.TrimSpace(kp.MarketExposureDollars),
			64,
		)

		out = append(out, polymarket.UserPosition{
			ConditionID:     kp.Ticker,
			Size:            size,
			AvgPrice:        0,
			CurrentValue:    currentValue,
			Outcome:         outcome,
			OutcomeIndex:    index,
			OppositeOutcome: opposite,
		})
	}

	return out, nil
}

// -----------------------------------------------------------------------------
// Polymarket-only operations.
// Kalshi has no CTF, Polygon ERC-1155 allowance, or manual redemption.
// -----------------------------------------------------------------------------

func (a *ExecutionAdapter) FetchConditionalBalanceAllowance(
	ctx context.Context,
	tokenID string,
) (*polymarket.BalanceAllowanceResponse, error) {
	return nil, errors.New("kalshi execution: conditional token allowance is not applicable")
}

func (a *ExecutionAdapter) HasCTFBalance(
	ctx context.Context,
	conditionID string,
) (bool, error) {
	return false, errors.New("kalshi execution: CTF balance is not applicable")
}

func (a *ExecutionAdapter) RedeemCTFPosition(
	ctx context.Context,
	conditionID string,
	isNoSide bool,
) error {
	return errors.New("kalshi execution: CTF redemption is not applicable")
}

func (a *ExecutionAdapter) TriggerConditionalApproval(
	ctx context.Context,
	tokenID string,
	retryDelay time.Duration,
) error {
	return errors.New("kalshi execution: conditional approval is not applicable")
}

func (a *ExecutionAdapter) WaitForConditionalAllowance(
	ctx context.Context,
	tokenID string,
	timeout time.Duration,
) error {
	return errors.New("kalshi execution: conditional allowance is not applicable")
}

func (a *ExecutionAdapter) WaitForSettledBalance(
	ctx context.Context,
	tokenID string,
	minShares float64,
	timeout time.Duration,
) error {
	return errors.New("kalshi execution: Polygon settlement balance is not applicable")
}

// Compile-time protection: this must continue satisfying the Trader interface
// shape as the migration progresses.

// ResolveMarket returns the authoritative finalized Kalshi result.
// known=false means the market has not finalized yet.
func (a *ExecutionAdapter) GetOrderBook(ctx context.Context, tokenID string) (*polymarket.OrderBook, error) {
	if err := a.requireClient(); err != nil {
		return nil, err
	}
	kb, err := a.Client.GetOrderBook(ctx, tokenID)
	if err != nil {
		return nil, err
	}
	out := &polymarket.OrderBook{Market: kb.Market, AssetID: kb.AssetID, Timestamp: kb.Timestamp, Hash: kb.Hash}
	out.Bids = make([]polymarket.PriceSize, 0, len(kb.Bids))
	out.Asks = make([]polymarket.PriceSize, 0, len(kb.Asks))
	for _, l := range kb.Bids {
		out.Bids = append(out.Bids, polymarket.PriceSize{Price: l.Price, Size: l.Size})
	}
	for _, l := range kb.Asks {
		out.Asks = append(out.Asks, polymarket.PriceSize{Price: l.Price, Size: l.Size})
	}
	return out, nil
}

func (a *ExecutionAdapter) ResolveMarket(
	ctx context.Context,
	conditionID string,
) (resolvedYes bool, known bool, err error) {
	if err := a.requireClient(); err != nil {
		return false, false, err
	}

	return a.Client.FetchResolution(ctx, conditionID)
}

var _ interface {
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
	FetchConditionalBalanceAllowance(context.Context, string) (*polymarket.BalanceAllowanceResponse, error)
	HasCTFBalance(context.Context, string) (bool, error)
	RedeemCTFPosition(context.Context, string, bool) error
	TriggerConditionalApproval(context.Context, string, time.Duration) error
	WaitForConditionalAllowance(context.Context, string, time.Duration) error
	WaitForSettledBalance(context.Context, string, float64, time.Duration) error
} = (*ExecutionAdapter)(nil)

// Silence math import until later Kalshi position normalization uses it.
var _ = math.Abs
