package kalshi

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Balance struct {
	Balance        int64 `json:"balance"`
	PortfolioValue int64 `json:"portfolio_value"`
	UpdatedTS      int64 `json:"updated_ts"`
}

func (b Balance) AvailableUSD() float64 {
	return float64(b.Balance) / 100.0
}

type Order struct {
	OrderID       string `json:"order_id"`
	ClientOrderID string `json:"client_order_id"`
	Ticker        string `json:"ticker"`

	Side        string `json:"side"`
	Action      string `json:"action"`
	OutcomeSide string `json:"outcome_side"`
	BookSide    string `json:"book_side"`
	Type        string `json:"type"`
	Status      string `json:"status"`

	YesPriceDollars string `json:"yes_price_dollars"`
	NoPriceDollars  string `json:"no_price_dollars"`

	FillCountFP      string `json:"fill_count_fp"`
	RemainingCountFP string `json:"remaining_count_fp"`
	InitialCountFP   string `json:"initial_count_fp"`

	TakerFillCostDollars string `json:"taker_fill_cost_dollars"`
	MakerFillCostDollars string `json:"maker_fill_cost_dollars"`
	TakerFeesDollars     string `json:"taker_fees_dollars"`
	MakerFeesDollars     string `json:"maker_fees_dollars"`

	ExpirationTime string `json:"expiration_time"`
	CreatedTime    string `json:"created_time"`
	LastUpdateTime string `json:"last_update_time"`
}

func (o Order) FilledContracts() float64 {
	v, _ := strconv.ParseFloat(o.FillCountFP, 64)
	return v
}

func (o Order) RemainingContracts() float64 {
	v, _ := strconv.ParseFloat(o.RemainingCountFP, 64)
	return v
}

func (o Order) FeesUSD() float64 {
	t, _ := strconv.ParseFloat(o.TakerFeesDollars, 64)
	m, _ := strconv.ParseFloat(o.MakerFeesDollars, 64)
	return t + m
}

type MarketPosition struct {
	Ticker                string `json:"ticker"`
	TotalTradedDollars    string `json:"total_traded_dollars"`
	PositionFP            string `json:"position_fp"`
	MarketExposureDollars string `json:"market_exposure_dollars"`
	RealizedPnLDollars    string `json:"realized_pnl_dollars"`
	RestingOrdersCount    int    `json:"resting_orders_count"`
	FeesPaidDollars       string `json:"fees_paid_dollars"`
	LastUpdatedTS         string `json:"last_updated_ts"`
}

type balanceResponse struct {
	Balance        int64 `json:"balance"`
	PortfolioValue int64 `json:"portfolio_value"`
	UpdatedTS      int64 `json:"updated_ts"`
}

type orderResponse struct {
	Order Order `json:"order"`
}

type positionsResponse struct {
	MarketPositions []MarketPosition `json:"market_positions"`
	Cursor          string           `json:"cursor"`
}

func (c *Client) GetBalance(ctx context.Context) (*Balance, error) {
	var r balanceResponse

	if err := c.Do(ctx, http.MethodGet, "/portfolio/balance", nil, &r); err != nil {
		return nil, err
	}

	return &Balance{
		Balance:        r.Balance,
		PortfolioValue: r.PortfolioValue,
		UpdatedTS:      r.UpdatedTS,
	}, nil
}

func (c *Client) GetOrder(ctx context.Context, orderID string) (*Order, error) {
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return nil, fmt.Errorf("kalshi: empty order id")
	}

	var r orderResponse
	path := "/portfolio/orders/" + url.PathEscape(orderID)

	if err := c.Do(ctx, http.MethodGet, path, nil, &r); err != nil {
		return nil, err
	}

	return &r.Order, nil
}

func (c *Client) GetPositions(ctx context.Context, ticker string) ([]MarketPosition, error) {
	path := "/portfolio/positions"

	if strings.TrimSpace(ticker) != "" {
		path += "?ticker=" + url.QueryEscape(strings.TrimSpace(ticker))
	}

	var r positionsResponse
	if err := c.Do(ctx, http.MethodGet, path, nil, &r); err != nil {
		return nil, err
	}

	return r.MarketPositions, nil
}

type CreateOrderRequest struct {
	Ticker                  string `json:"ticker"`
	ClientOrderID           string `json:"client_order_id"`
	Side                    string `json:"side"`
	Count                   string `json:"count"`
	Price                   string `json:"price"`
	TimeInForce             string `json:"time_in_force"`
	SelfTradePreventionType string `json:"self_trade_prevention_type"`

	ExpirationTime     int64 `json:"expiration_time,omitempty"`
	PostOnly           bool  `json:"post_only"`
	CancelOrderOnPause bool  `json:"cancel_order_on_pause"`
	ReduceOnly         bool  `json:"reduce_only"`
}

type CreateOrderResponse struct {
	OrderID          string `json:"order_id"`
	ClientOrderID    string `json:"client_order_id"`
	FillCount        string `json:"fill_count"`
	RemainingCount   string `json:"remaining_count"`
	AverageFillPrice string `json:"average_fill_price,omitempty"`
	AverageFeePaid   string `json:"average_fee_paid,omitempty"`
	TSMS             int64  `json:"ts_ms"`
}

type CancelOrderResponse struct {
	OrderID       string `json:"order_id"`
	ClientOrderID string `json:"client_order_id"`
	ReducedBy     string `json:"reduced_by"`
	TSMS          int64  `json:"ts_ms"`
}

func (c *Client) CreateOrder(
	ctx context.Context,
	req CreateOrderRequest,
) (*CreateOrderResponse, error) {
	if strings.TrimSpace(req.Ticker) == "" {
		return nil, fmt.Errorf("kalshi: empty ticker")
	}
	if strings.TrimSpace(req.ClientOrderID) == "" {
		return nil, fmt.Errorf("kalshi: empty client order id")
	}
	if req.Side != "bid" && req.Side != "ask" {
		return nil, fmt.Errorf("kalshi: invalid V2 side %q", req.Side)
	}
	if strings.TrimSpace(req.Count) == "" {
		return nil, fmt.Errorf("kalshi: empty order count")
	}
	if strings.TrimSpace(req.Price) == "" {
		return nil, fmt.Errorf("kalshi: empty order price")
	}

	switch req.TimeInForce {
	case "good_till_canceled", "fill_or_kill", "immediate_or_cancel":
	default:
		return nil, fmt.Errorf(
			"kalshi: invalid time_in_force %q",
			req.TimeInForce,
		)
	}

	if req.SelfTradePreventionType == "" {
		req.SelfTradePreventionType = "taker_at_cross"
	}

	var out CreateOrderResponse
	if err := c.Do(
		ctx,
		http.MethodPost,
		"/portfolio/events/orders",
		req,
		&out,
	); err != nil {
		return nil, err
	}

	if strings.TrimSpace(out.OrderID) == "" {
		return nil, fmt.Errorf("kalshi: create order returned empty order_id")
	}

	return &out, nil
}

func (c *Client) CancelOrderV2(
	ctx context.Context,
	orderID string,
) (*CancelOrderResponse, error) {
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return nil, fmt.Errorf("kalshi: empty order id")
	}

	var out CancelOrderResponse
	path := "/portfolio/events/orders/" + url.PathEscape(orderID)

	if err := c.Do(
		ctx,
		http.MethodDelete,
		path,
		nil,
		&out,
	); err != nil {
		return nil, err
	}

	return &out, nil
}

func LogicalOrderToV2(
	tokenID string,
	action string,
	price float64,
) (ticker, bookSide string, kalshiPrice float64, err error) {
	ticker, outcome, err := parseLogicalTokenID(tokenID)
	if err != nil {
		return "", "", 0, err
	}

	action = strings.ToUpper(strings.TrimSpace(action))

	if action != "BUY" && action != "SELL" {
		return "", "", 0, fmt.Errorf("kalshi: invalid logical action %q", action)
	}

	if price <= 0 || price >= 1 {
		return "", "", 0, fmt.Errorf("kalshi: invalid logical price %.4f", price)
	}

	// Kalshi adapter uses a YES bid/ask book:
	//
	// BUY YES  P  -> BID YES P
	// SELL YES P  -> ASK YES P
	//
	// A NO contract at P is economically the opposite YES contract at 1-P:
	//
	// BUY NO   P  -> ASK YES (1-P)
	// SELL NO  P  -> BID YES (1-P)
	//
	// This complement is essential. A logical BUY NO @ 0.67 must become
	// ASK YES @ 0.33, not ASK YES @ 0.67.
	yesPrice := price
	side := ""

	switch outcome {
	case "YES":
		if action == "BUY" {
			side = "bid"
		} else {
			side = "ask"
		}

	case "NO":
		yesPrice = 1.0 - price

		if action == "BUY" {
			side = "ask"
		} else {
			side = "bid"
		}

	default:
		return "", "", 0, fmt.Errorf("kalshi: unsupported outcome %q", outcome)
	}

	yesPrice = math.Round(yesPrice*10000) / 10000

	if yesPrice <= 0 || yesPrice >= 1 {
		return "", "", 0, fmt.Errorf(
			"kalshi: invalid translated YES price %.4f",
			yesPrice,
		)
	}

	return ticker, side, yesPrice, nil
}

func MapTimeInForce(orderType string, expiration int64) (string, int64, error) {
	switch strings.ToUpper(strings.TrimSpace(orderType)) {
	case "GTC":
		return "good_till_canceled", 0, nil

	case "GTD":
		if expiration <= 0 {
			return "", 0, fmt.Errorf("kalshi: GTD requires expiration")
		}
		return "good_till_canceled", expiration, nil

	case "FAK":
		return "immediate_or_cancel", 0, nil

	case "FOK":
		return "fill_or_kill", 0, nil

	default:
		return "", 0, fmt.Errorf(
			"kalshi: unsupported order type %q",
			orderType,
		)
	}
}

type LogicalOrderRequest struct {
	TokenID       string
	Action        string
	OrderType     string
	Price         float64
	Size          float64
	Expiration    int64
	ClientOrderID string
}

func BuildCreateOrderRequest(req LogicalOrderRequest) (CreateOrderRequest, error) {
	if req.Size <= 0 {
		return CreateOrderRequest{}, fmt.Errorf("kalshi: invalid size %.4f", req.Size)
	}

	ticker, side, yesPrice, err := LogicalOrderToV2(
		req.TokenID,
		req.Action,
		req.Price,
	)
	if err != nil {
		return CreateOrderRequest{}, err
	}

	tif, expiration, err := MapTimeInForce(
		req.OrderType,
		req.Expiration,
	)
	if err != nil {
		return CreateOrderRequest{}, err
	}

	clientID := strings.TrimSpace(req.ClientOrderID)
	if clientID == "" {
		clientID = fmt.Sprintf("kalshiarbo-%d", time.Now().UnixNano())
	}

	count := strconv.FormatFloat(req.Size, 'f', 2, 64)
	price := strconv.FormatFloat(yesPrice, 'f', 4, 64)

	out := CreateOrderRequest{
		Ticker:                  ticker,
		ClientOrderID:           clientID,
		Side:                    side,
		Count:                   count,
		Price:                   price,
		TimeInForce:             tif,
		SelfTradePreventionType: "taker_at_cross",
		PostOnly:                false,
		CancelOrderOnPause:      true,
		ReduceOnly:              strings.EqualFold(req.Action, "SELL") && tif == "immediate_or_cancel",
	}

	if expiration > 0 {
		out.ExpirationTime = expiration
	}

	out.ReduceOnly = strings.EqualFold(req.Action, "SELL") && tif == "immediate_or_cancel"

	return out, nil
}

type NormalizedOrder struct {
	OrderID          string
	Status           string
	FilledContracts  float64
	Remaining        float64
	AverageFillPrice float64
	FeesUSD          float64
}

func (o Order) NormalizeForLogicalToken(tokenID string) (NormalizedOrder, error) {
	_, outcome, err := parseLogicalTokenID(tokenID)
	if err != nil {
		return NormalizedOrder{}, err
	}

	filled, _ := strconv.ParseFloat(o.FillCountFP, 64)
	remaining, _ := strconv.ParseFloat(o.RemainingCountFP, 64)

	yesPrice, _ := strconv.ParseFloat(o.YesPriceDollars, 64)
	noPrice, _ := strconv.ParseFloat(o.NoPriceDollars, 64)

	price := yesPrice
	if outcome == "NO" {
		price = noPrice
		if price <= 0 && yesPrice > 0 {
			price = 1.0 - yesPrice
		}
	}

	return NormalizedOrder{
		OrderID:          o.OrderID,
		Status:           o.Status,
		FilledContracts:  filled,
		Remaining:        remaining,
		AverageFillPrice: price,
		FeesUSD:          o.FeesUSD(),
	}, nil
}

type ordersResponse struct {
	Orders []Order `json:"orders"`
	Cursor string  `json:"cursor"`
}

func (c *Client) GetOrders(
	ctx context.Context,
	ticker string,
	limit int,
) ([]Order, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))

	if strings.TrimSpace(ticker) != "" {
		q.Set("ticker", strings.TrimSpace(ticker))
	}

	path := "/portfolio/orders?" + q.Encode()

	var r ordersResponse
	if err := c.Do(ctx, http.MethodGet, path, nil, &r); err != nil {
		return nil, err
	}

	return r.Orders, nil
}

type NettingConfig struct {
	SubaccountNumber int  `json:"subaccount_number"`
	Enabled          bool `json:"enabled"`
	ExchangeIndex    int  `json:"exchange_index"`
}

type nettingResponse struct {
	NettingConfigs []NettingConfig `json:"netting_configs"`
}

func (c *Client) GetNettingConfigs(ctx context.Context) ([]NettingConfig, error) {
	var out nettingResponse

	if err := c.Do(
		ctx,
		http.MethodGet,
		"/portfolio/subaccounts/netting",
		nil,
		&out,
	); err != nil {
		return nil, err
	}

	return out.NettingConfigs, nil
}
