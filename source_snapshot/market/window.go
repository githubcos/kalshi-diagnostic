// Package market provides multi-duration window scheduling for Polymarket BTC
// up/down prediction markets.  Supported window lengths: 5-min, 15-min, 1-hour.
//
// Slug formats:
//
//	5m  → "btc-updown-5m-{unix_start}"
//	15m → "btc-updown-15m-{unix_start}"
//	1h  → "btc-updown-1h-{unix_start}"
package market

import (
	"fmt"
	"time"
)

// MarketType identifies the duration class of a Polymarket BTC up/down market.
type MarketType string

const (
	Market5m  MarketType = "5m"
	Market15m MarketType = "15m"
	Market1h  MarketType = "1h"
)

// Duration returns the window length for this market type.
func (mt MarketType) Duration() time.Duration {
	switch mt {
	case Market15m:
		return 15 * time.Minute
	case Market1h:
		return 60 * time.Minute
	default:
		return 5 * time.Minute
	}
}

// boundary returns the Unix epoch interval size in seconds (floor divisor).
func (mt MarketType) boundary() int64 {
	return int64(mt.Duration().Seconds())
}

// SlugPrefix returns the Polymarket slug prefix for this type.
func (mt MarketType) SlugPrefix() string {
	switch mt {
	case Market15m:
		return "btc-updown-15m"
	case Market1h:
		return "btc-updown-1h"
	default:
		return "btc-updown-5m"
	}
}

// CryptoPriceVariant returns the `variant=` value for the Polymarket crypto-price API.
func (mt MarketType) CryptoPriceVariant() string {
	switch mt {
	case Market15m:
		return "fifteen"
	case Market1h:
		return "onehour"
	default:
		return "fiveminute"
	}
}

// Window represents one active BTC prediction market window.
type Window struct {
	// Start is the UTC time this window opened (floored to the boundary).
	Start time.Time
	// End is Start + Duration – resolution/settlement time.
	End time.Time
	// Slug is the Polymarket event slug, e.g. "btc-updown-15m-1771702200".
	Slug string
	// StartUnix is Start as Unix seconds – used in the slug and API calls.
	StartUnix int64
	// Type is the market duration class.
	Type MarketType
}

// Current returns the window that is active right now (UTC) for the given market type.
func Current(mt MarketType) Window {
	return ForTime(time.Now().UTC(), mt)
}

// Next returns the window that immediately follows w.
func (w Window) Next() Window {
	return ForTime(w.End.Add(time.Second), w.Type)
}

// ForTime computes the market window containing t for the given type.
func ForTime(t time.Time, mt MarketType) Window {
	t = t.UTC()
	b := mt.boundary()
	startUnix := (t.Unix() / b) * b
	start := time.Unix(startUnix, 0).UTC()
	return Window{
		Start:     start,
		End:       start.Add(mt.Duration()),
		Slug:      fmt.Sprintf("%s-%d", mt.SlugPrefix(), startUnix),
		StartUnix: startUnix,
		Type:      mt,
	}
}

// SecondsRemaining returns how many seconds are left in w (0 if already expired).
func (w Window) SecondsRemaining() float64 {
	rem := time.Until(w.End).Seconds()
	if rem < 0 {
		return 0
	}
	return rem
}

// IsExpired reports whether this window's end time has passed.
func (w Window) IsExpired() bool {
	return time.Now().UTC().After(w.End)
}

// String returns a human-readable summary of the window.
func (w Window) String() string {
	return fmt.Sprintf("%s  [%s → %s  (%.0fs remaining)]",
		w.Slug,
		w.Start.Format("15:04:05Z"),
		w.End.Format("15:04:05Z"),
		w.SecondsRemaining(),
	)
}
