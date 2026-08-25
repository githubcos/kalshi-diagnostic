package kalshi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Market mirrors the market information the existing PolyArb engine needs.
// ConditionID maps to the Kalshi market ticker.
// QuestionID maps to the Kalshi event ticker.
type Market struct {
	ConditionID  string
	QuestionID   string
	Question     string
	Slug         string
	Active       bool
	Closed       bool
	EndDateISO   string
	OpenDateISO  string
	FloorStrike  float64
	Tokens       []Token
	MinTickSize  float64
	MinOrderSize float64
	Description  string

	KalshiTicker string
	EventTicker  string
	Status       string
	Result       string
	YesBid       string
	YesAsk       string
	NoBid        string
	NoAsk        string
}

// Token gives Kalshi YES and NO sides logical token IDs so the existing
// strategy can continue thinking in terms of separate YES/NO instruments.
type Token struct {
	TokenID string
	Outcome string
}

type PriceSize struct {
	Price string `json:"price"`
	Size  string `json:"size"`
}

// OrderBook is deliberately shaped like the existing Polymarket orderbook.
type OrderBook struct {
	Market    string
	AssetID   string
	Bids      []PriceSize
	Asks      []PriceSize
	Timestamp string
	Hash      string
}

type apiMarket struct {
	Ticker                   string  `json:"ticker"`
	EventTicker              string  `json:"event_ticker"`
	MarketType               string  `json:"market_type"`
	Title                    string  `json:"title"`
	Subtitle                 string  `json:"subtitle"`
	YesSubTitle              string  `json:"yes_sub_title"`
	NoSubTitle               string  `json:"no_sub_title"`
	Status                   string  `json:"status"`
	Result                   string  `json:"result"`
	CloseTime                string  `json:"close_time"`
	OpenTime                 string  `json:"open_time"`
	ExpirationTime           string  `json:"expiration_time"`
	FloorStrike              float64 `json:"floor_strike"`
	YesBidDollars            string  `json:"yes_bid_dollars"`
	YesAskDollars            string  `json:"yes_ask_dollars"`
	NoBidDollars             string  `json:"no_bid_dollars"`
	NoAskDollars             string  `json:"no_ask_dollars"`
	FractionalTradingEnabled bool    `json:"fractional_trading_enabled"`
	RulesPrimary             string  `json:"rules_primary"`
	PriceRanges              []struct {
		Start string `json:"start"`
		End   string `json:"end"`
		Step  string `json:"step"`
	} `json:"price_ranges"`
}

type marketsResponse struct {
	Markets []apiMarket `json:"markets"`
	Cursor  string      `json:"cursor"`
}

type marketResponse struct {
	Market apiMarket `json:"market"`
}

type orderbookResponse struct {
	Orderbook struct {
		YesDollars [][]string `json:"yes_dollars"`
		NoDollars  [][]string `json:"no_dollars"`
	} `json:"orderbook_fp"`
}

func logicalTokenID(ticker, outcome string) string {
	return ticker + ":" + strings.ToUpper(outcome)
}

func parseLogicalTokenID(tokenID string) (ticker, outcome string, err error) {
	i := strings.LastIndex(tokenID, ":")
	if i <= 0 || i >= len(tokenID)-1 {
		return "", "", fmt.Errorf("kalshi: invalid logical token id %q", tokenID)
	}

	ticker = tokenID[:i]
	outcome = strings.ToUpper(tokenID[i+1:])

	if outcome != "YES" && outcome != "NO" {
		return "", "", fmt.Errorf("kalshi: invalid outcome in token id %q", tokenID)
	}

	return ticker, outcome, nil
}

func convertMarket(m apiMarket) Market {
	minTick := 0.01

	if len(m.PriceRanges) > 0 && m.PriceRanges[0].Step != "" {
		if v, err := strconv.ParseFloat(m.PriceRanges[0].Step, 64); err == nil && v > 0 {
			minTick = v
		}
	}

	endDate := m.CloseTime
	if endDate == "" {
		endDate = m.ExpirationTime
	}

	description := m.RulesPrimary
	if description == "" {
		description = m.Subtitle
	}

	return Market{
		ConditionID: m.Ticker,
		QuestionID:  m.EventTicker,
		Question:    m.Title,
		Slug:        m.Ticker,
		Active:      m.Status == "open",
		Closed:      m.Status == "closed" || m.Status == "settled" || m.Status == "finalized",
		EndDateISO:  endDate,
		OpenDateISO: m.OpenTime,
		FloorStrike: m.FloorStrike,
		Tokens: []Token{
			{
				TokenID: logicalTokenID(m.Ticker, "YES"),
				Outcome: "Yes",
			},
			{
				TokenID: logicalTokenID(m.Ticker, "NO"),
				Outcome: "No",
			},
		},
		MinTickSize:  minTick,
		MinOrderSize: 0,
		Description:  description,

		KalshiTicker: m.Ticker,
		EventTicker:  m.EventTicker,
		Status:       m.Status,
		Result:       m.Result,
		YesBid:       m.YesBidDollars,
		YesAsk:       m.YesAskDollars,
		NoBid:        m.NoBidDollars,
		NoAsk:        m.NoAskDollars,
	}
}

func (c *Client) ListMarkets(
	ctx context.Context,
	status string,
	seriesTicker string,
	limit int,
) ([]Market, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}

	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	q.Set("mve_filter", "exclude")

	if status != "" {
		q.Set("status", status)
	}

	if seriesTicker != "" {
		q.Set("series_ticker", seriesTicker)
	}

	path := "/markets?" + q.Encode()

	var response marketsResponse
	if err := c.DoPublic(ctx, http.MethodGet, path, &response); err != nil {
		return nil, err
	}

	markets := make([]Market, 0, len(response.Markets))
	for _, m := range response.Markets {
		markets = append(markets, convertMarket(m))
	}

	return markets, nil
}

func (c *Client) GetMarket(ctx context.Context, ticker string) (*Market, error) {
	path := "/markets/" + url.PathEscape(ticker)

	var response marketResponse
	if err := c.DoPublic(ctx, http.MethodGet, path, &response); err != nil {
		return nil, err
	}

	m := convertMarket(response.Market)
	return &m, nil
}

// GetOrderBook accepts the logical YES/NO token IDs used by the adapter.
//
// Kalshi only publishes bids:
//
//	YES bid P  == NO ask 1-P
//	NO bid P   == YES ask 1-P
//
// This function reconstructs a conventional bids/asks book so the existing
// PolyArb strategy can continue using the same market-price logic.
func (c *Client) GetOrderBook(ctx context.Context, tokenID string) (*OrderBook, error) {
	ticker, outcome, err := parseLogicalTokenID(tokenID)
	if err != nil {
		return nil, err
	}

	path := "/markets/" + url.PathEscape(ticker) + "/orderbook"

	var response orderbookResponse
	if err := c.DoPublic(ctx, http.MethodGet, path, &response); err != nil {
		return nil, err
	}

	var ownBids [][]string
	var oppositeBids [][]string

	if outcome == "YES" {
		ownBids = response.Orderbook.YesDollars
		oppositeBids = response.Orderbook.NoDollars
	} else {
		ownBids = response.Orderbook.NoDollars
		oppositeBids = response.Orderbook.YesDollars
	}

	book := &OrderBook{
		Market:  ticker,
		AssetID: tokenID,
		Bids:    make([]PriceSize, 0, len(ownBids)),
		Asks:    make([]PriceSize, 0, len(oppositeBids)),
	}

	for _, level := range ownBids {
		if len(level) < 2 {
			continue
		}

		book.Bids = append(book.Bids, PriceSize{
			Price: level[0],
			Size:  level[1],
		})
	}

	for _, level := range oppositeBids {
		if len(level) < 2 {
			continue
		}

		p, err := strconv.ParseFloat(level[0], 64)
		if err != nil {
			continue
		}

		ask := 1.0 - p

		book.Asks = append(book.Asks, PriceSize{
			Price: strconv.FormatFloat(ask, 'f', 4, 64),
			Size:  level[1],
		})
	}

	sort.Slice(book.Bids, func(i, j int) bool {
		pi, _ := strconv.ParseFloat(book.Bids[i].Price, 64)
		pj, _ := strconv.ParseFloat(book.Bids[j].Price, 64)
		return pi > pj
	})

	sort.Slice(book.Asks, func(i, j int) bool {
		pi, _ := strconv.ParseFloat(book.Asks[i].Price, 64)
		pj, _ := strconv.ParseFloat(book.Asks[j].Price, 64)
		return pi < pj
	})

	return book, nil
}

// GetCurrentSeriesMarket returns the currently open market from a recurring
// Kalshi series such as KXBTC15M. When several markets are returned, it chooses
// the one closing soonest.
func (c *Client) GetCurrentSeriesMarket(
	ctx context.Context,
	seriesTicker string,
) (*Market, error) {

	markets, err := c.ListMarkets(ctx, "open", seriesTicker, 100)
	if err != nil {
		return nil, err
	}

	if len(markets) == 0 {
		return nil, fmt.Errorf(
			"kalshi: no open markets for series %s",
			seriesTicker,
		)
	}

	sort.Slice(markets, func(i, j int) bool {
		ti, errI := time.Parse(time.RFC3339, markets[i].EndDateISO)
		tj, errJ := time.Parse(time.RFC3339, markets[j].EndDateISO)

		if errI != nil {
			return false
		}
		if errJ != nil {
			return true
		}

		return ti.Before(tj)
	})

	now := time.Now().UTC()

	for i := range markets {
		t, err := time.Parse(time.RFC3339, markets[i].EndDateISO)
		if err != nil {
			continue
		}

		if t.After(now) {
			return &markets[i], nil
		}
	}

	return &markets[0], nil
}

// GetNextSeriesMarket returns the next market in a recurring Kalshi series
// whose close time is later than currentClose.
func (c *Client) GetNextSeriesMarket(
	ctx context.Context,
	seriesTicker string,
	currentClose time.Time,
) (*Market, error) {

	markets, err := c.ListMarkets(ctx, "", seriesTicker, 100)
	if err != nil {
		return nil, err
	}

	type candidate struct {
		m Market
		t time.Time
	}

	var candidates []candidate

	for _, m := range markets {
		t, err := time.Parse(time.RFC3339, m.EndDateISO)
		if err != nil {
			continue
		}

		if t.After(currentClose) {
			candidates = append(candidates, candidate{
				m: m,
				t: t,
			})
		}
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf(
			"kalshi: no next market found for series %s after %s",
			seriesTicker,
			currentClose.Format(time.RFC3339),
		)
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].t.Before(candidates[j].t)
	})

	return &candidates[0].m, nil
}

// FetchResolution returns the authoritative Kalshi settlement result
// for a market ticker.
// resolvedYes is true when Kalshi officially resolves the market YES.
// known is false while the market has not published a final YES/NO result.
func (c *Client) FetchResolution(
	ctx context.Context,
	ticker string,
) (resolvedYes bool, known bool, err error) {
	m, err := c.GetMarket(ctx, ticker)
	if err != nil {
		return false, false, err
	}

	switch strings.ToLower(strings.TrimSpace(m.Result)) {
	case "yes":
		return true, true, nil
	case "no":
		return false, true, nil
	case "":
		return false, false, nil
	default:
		return false, false, fmt.Errorf(
			"kalshi: unsupported market result %q for %s",
			m.Result,
			ticker,
		)
	}
}
