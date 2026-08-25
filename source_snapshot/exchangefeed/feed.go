package exchangefeed

import "time"

// PriceEvent is the exchange-neutral price event consumed by the original
// PolyArb runtime.
type PriceEvent struct {
	TokenID string
	Price   float64
	Size    float64
	Side    string
	At      time.Time
}

// Feed is the minimal live-price interface required by the PolyArb runtime.
type Feed interface {
	Run(stopCh <-chan struct{})
	LatestPrice(tokenID string) float64
	Events() <-chan PriceEvent
}
