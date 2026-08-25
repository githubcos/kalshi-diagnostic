package kalshi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/polyarb/exchangefeed"
	"go.uber.org/zap"
)

const defaultWSURL = "wss://external-api-ws.kalshi.com/trade-api/ws/v2"

type FeedClient struct {
	client      *Client
	tokenIDs    []string
	logger      *zap.Logger
	mu          sync.RWMutex
	latestPrice map[string]float64
	PriceC      chan exchangefeed.PriceEvent
}

func NewFeedClient(client *Client, tokenIDs []string, logger *zap.Logger) *FeedClient {
	return &FeedClient{client: client, tokenIDs: tokenIDs, logger: logger, latestPrice: make(map[string]float64), PriceC: make(chan exchangefeed.PriceEvent, 512)}
}
func (f *FeedClient) LatestPrice(tokenID string) float64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.latestPrice[tokenID]
}
func (f *FeedClient) Events() <-chan exchangefeed.PriceEvent { return f.PriceC }
func (f *FeedClient) update(tokenID string, price, size float64, side string) {
	if price <= 0 || price >= 1 {
		return
	}
	f.mu.Lock()
	old := f.latestPrice[tokenID]
	if old == price {
		f.mu.Unlock()
		return
	}
	f.latestPrice[tokenID] = price
	f.mu.Unlock()
	ev := exchangefeed.PriceEvent{TokenID: tokenID, Price: price, Size: size, Side: side, At: time.Now()}
	select {
	case f.PriceC <- ev:
	default:
		if f.logger != nil {
			f.logger.Warn("kalshi feed: PriceC buffer full")
		}
	}
}
func bestAsk(book *OrderBook) (float64, float64) {
	if book == nil || len(book.Asks) == 0 {
		return 0, 0
	}
	p, _ := strconv.ParseFloat(book.Asks[0].Price, 64)
	q, _ := strconv.ParseFloat(book.Asks[0].Size, 64)
	return p, q
}
func (f *FeedClient) pollOnce(ctx context.Context) {
	for _, id := range f.tokenIDs {
		b, e := f.client.GetOrderBook(ctx, id)
		if e != nil {
			continue
		}
		p, q := bestAsk(b)
		if p > 0 {
			f.update(id, p, q, "SELL")
		}
	}
}

func (f *FeedClient) logicalIDs() (ticker, yesID, noID string, err error) {
	for _, id := range f.tokenIDs {
		t, o, e := parseLogicalTokenID(id)
		if e != nil {
			continue
		}
		if ticker == "" {
			ticker = t
		}
		if t != ticker {
			return "", "", "", fmt.Errorf("kalshi feed: multiple tickers not supported")
		}
		if o == "YES" {
			yesID = id
		} else if o == "NO" {
			noID = id
		}
	}
	if ticker == "" {
		return "", "", "", fmt.Errorf("kalshi feed: no valid token IDs")
	}
	return ticker, yesID, noID, nil
}

type wsEnvelope struct {
	Type string          `json:"type"`
	Msg  json.RawMessage `json:"msg"`
}
type wsSnapshot struct {
	MarketTicker string     `json:"market_ticker"`
	Yes          [][]string `json:"yes_dollars_fp"`
	No           [][]string `json:"no_dollars_fp"`
}
type wsDelta struct {
	MarketTicker string `json:"market_ticker"`
	Price        string `json:"price_dollars"`
	Delta        string `json:"delta_fp"`
	Side         string `json:"side"`
}

func applySnapshot(dst map[float64]float64, levels [][]string) {
	clear(dst)
	for _, l := range levels {
		if len(l) < 2 {
			continue
		}
		p, e1 := strconv.ParseFloat(l[0], 64)
		q, e2 := strconv.ParseFloat(l[1], 64)
		if e1 == nil && e2 == nil && p > 0 && q > 0 {
			dst[p] = q
		}
	}
}
func applyDelta(dst map[float64]float64, p, q float64) {
	if p <= 0 {
		return
	}
	n := dst[p] + q
	if n <= 1e-9 {
		delete(dst, p)
	} else {
		dst[p] = n
	}
}
func maxLevel(m map[float64]float64) (float64, float64) {
	p, q := 0.0, 0.0
	for x, n := range m {
		if n > 0 && x > p {
			p, q = x, n
		}
	}
	return p, q
}

func (f *FeedClient) publishFromBids(yesBids, noBids map[float64]float64, yesID, noID string) {
	yb, yq := maxLevel(yesBids)
	nb, nq := maxLevel(noBids)
	if yesID != "" && nb > 0 {
		f.update(yesID, 1.0-nb, nq, "SELL")
	}
	if noID != "" && yb > 0 {
		f.update(noID, 1.0-yb, yq, "SELL")
	}
}

func (f *FeedClient) runWS(stopCh <-chan struct{}) error {
	if f.client == nil || f.client.PrivateKey == nil || strings.TrimSpace(f.client.APIKeyID) == "" {
		return fmt.Errorf("kalshi feed: websocket auth unavailable")
	}
	ticker, yesID, noID, err := f.logicalIDs()
	if err != nil {
		return err
	}
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	sig, err := f.client.sign(ts, http.MethodGet, "/trade-api/ws/v2")
	if err != nil {
		return err
	}
	h := http.Header{}
	h.Set("KALSHI-ACCESS-KEY", f.client.APIKeyID)
	h.Set("KALSHI-ACCESS-TIMESTAMP", ts)
	h.Set("KALSHI-ACCESS-SIGNATURE", sig)
	d := websocket.Dialer{HandshakeTimeout: 8 * time.Second}
	conn, _, err := d.Dial(defaultWSURL, h)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(35 * time.Second))
	conn.SetPongHandler(func(string) error { _ = conn.SetReadDeadline(time.Now().Add(35 * time.Second)); return nil })
	sub := map[string]any{"id": 1, "cmd": "subscribe", "params": map[string]any{"channels": []string{"orderbook_delta"}, "market_tickers": []string{ticker}, "use_yes_price": false}}
	if err := conn.WriteJSON(sub); err != nil {
		return err
	}
	if f.logger != nil {
		f.logger.Info("kalshi feed: websocket orderbook connected", zap.String("ticker", ticker))
	}
	yesBids := map[float64]float64{}
	noBids := map[float64]float64{}
	done := make(chan struct{})
	go func() {
		select {
		case <-stopCh:
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "stop"), time.Now().Add(time.Second))
			_ = conn.Close()
		case <-done:
		}
	}()
	defer close(done)
	for {
		var env wsEnvelope
		if err := conn.ReadJSON(&env); err != nil {
			select {
			case <-stopCh:
				return nil
			default:
				return err
			}
		}
		_ = conn.SetReadDeadline(time.Now().Add(35 * time.Second))
		switch env.Type {
		case "orderbook_snapshot":
			var m wsSnapshot
			if json.Unmarshal(env.Msg, &m) == nil {
				applySnapshot(yesBids, m.Yes)
				applySnapshot(noBids, m.No)
				f.publishFromBids(yesBids, noBids, yesID, noID)
			}
		case "orderbook_delta":
			var m wsDelta
			if json.Unmarshal(env.Msg, &m) == nil {
				p, _ := strconv.ParseFloat(m.Price, 64)
				q, _ := strconv.ParseFloat(m.Delta, 64)
				if strings.EqualFold(m.Side, "yes") {
					applyDelta(yesBids, p, q)
				} else if strings.EqualFold(m.Side, "no") {
					applyDelta(noBids, p, q)
				}
				f.publishFromBids(yesBids, noBids, yesID, noID)
			}
		case "error":
			return fmt.Errorf("kalshi feed websocket error: %s", string(env.Msg))
		}
	}
}

func (f *FeedClient) Run(stopCh <-chan struct{}) {
	// Prime immediately through REST so the detector has prices before WS snapshot.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	f.pollOnce(ctx)
	cancel()
	backoff := 250 * time.Millisecond
	for {
		select {
		case <-stopCh:
			return
		default:
		}
		err := f.runWS(stopCh)
		if err == nil {
			return
		}
		if f.logger != nil {
			f.logger.Warn("kalshi feed: websocket disconnected; REST fallback + reconnect", zap.Error(err))
		}
		// Short REST fallback during reconnect; never becomes the primary data path.
		deadline := time.Now().Add(backoff)
		for time.Now().Before(deadline) {
			ctx, c := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			f.pollOnce(ctx)
			c()
			select {
			case <-stopCh:
				return
			case <-time.After(250 * time.Millisecond):
			}
		}
		if backoff < 4*time.Second {
			backoff *= 2
		}
	}
}
