package strategy

import (
	"math"
	"time"
)

// BookLevel is a single price level in an orderbook snapshot.
type BookLevel struct {
	Price float64 // USD
	Size  float64 // BTC
}

// BookSnapshot is an orderbook snapshot (Deribit BTC-PERPETUAL feed).
type BookSnapshot struct {
	Bids []BookLevel // best bids, descending by price
	Asks []BookLevel // best asks, ascending by price
	At   time.Time
}

// ── Rolling-window eviction helpers ──────────────────────────────────────────
// These methods trim the head of each sliding-window slice, discarding entries
// older than the retention window. All callers hold d.mu.

// evictCVD removes Binance CVD samples older than 30 seconds.
func (d *Detector) evictCVD(now time.Time) {
	cutoff := now.Add(-30 * time.Second)
	i := 0
	for i < len(d.cvdSamples) && d.cvdSamples[i].at.Before(cutoff) {
		i++
	}
	if i > 0 {
		d.cvdSamples = d.cvdSamples[i:]
	}
}

// evictCBCVD removes Coinbase CVD samples older than 30 seconds.
func (d *Detector) evictCBCVD(now time.Time) {
	cutoff := now.Add(-30 * time.Second)
	i := 0
	for i < len(d.cbCVDSamples) && d.cbCVDSamples[i].at.Before(cutoff) {
		i++
	}
	if i > 0 {
		d.cbCVDSamples = d.cbCVDSamples[i:]
	}
}

// evictBTC removes BTC price samples older than 90 seconds.
func (d *Detector) evictBTC(now time.Time) {
	cutoff := now.Add(-90 * time.Second)
	i := 0
	for i < len(d.btcSamples) && d.btcSamples[i].at.Before(cutoff) {
		i++
	}
	if i > 0 {
		d.btcSamples = d.btcSamples[i:]
	}
}

// evictBTCGap removes gap-velocity BTC samples older than 90 seconds.
func (d *Detector) evictBTCGap(now time.Time) {
	cutoff := now.Add(-90 * time.Second)
	i := 0
	for i < len(d.btcGapSamples) && d.btcGapSamples[i].at.Before(cutoff) {
		i++
	}
	if i > 0 {
		d.btcGapSamples = d.btcGapSamples[i:]
	}
}

// evictYES removes YES price samples older than 30 seconds.
func (d *Detector) evictYES(now time.Time) {
	cutoff := now.Add(-30 * time.Second)
	i := 0
	for i < len(d.yesSamples) && d.yesSamples[i].at.Before(cutoff) {
		i++
	}
	if i > 0 {
		d.yesSamples = d.yesSamples[i:]
	}
}

// ── ATR (Average True Range) ─────────────────────────────────────────────────

// computeATR returns the average absolute tick-to-tick BTC move across samples.
// Returns 0 when fewer than two samples are available.
func computeATR(samples []priceSample) float64 {
	if len(samples) < 2 {
		return 0
	}
	var sum float64
	for i := 1; i < len(samples); i++ {
		sum += math.Abs(samples[i].price - samples[i-1].price)
	}
	return sum / float64(len(samples)-1)
}

// ── Microstructure helpers (called from ConvictionSnapshot) ──────────────────

// cvdBTC returns the net BTC Cumulative Volume Delta over the current 30s window.
// Positive = net buying aggressor pressure; negative = net selling pressure.
// Caller must hold d.mu.
func (d *Detector) cvdBTC() float64 {
	var net float64
	for _, s := range d.cvdSamples {
		net += s.buyBTC - s.sellBTC
	}
	return net
}

// cbCVDBTC returns the net Coinbase spot BTC CVD over the current 30s window.
// Caller must hold d.mu.
func (d *Detector) cbCVDBTC() float64 {
	var net float64
	for _, s := range d.cbCVDSamples {
		net += s.buyBTC - s.sellBTC
	}
	return net
}

// cbTakerImbalance returns Coinbase taker imbalance over the rolling 30s window:
// (buy - sell) / (buy + sell). Range [-1, 1].
// Caller must hold d.mu.
func (d *Detector) cbTakerImbalance() float64 {
	var buy, sell float64
	for _, s := range d.cbCVDSamples {
		buy += s.buyBTC
		sell += s.sellBTC
	}
	total := buy + sell
	if total <= 0 {
		return 0
	}
	return (buy - sell) / total
}

// bookImbalance returns the normalised Deribit near-mid bid/ask imbalance:
// (bidBTC - askBTC) / (bidBTC + askBTC). Range [-1, 1]; 0 when no data.
// Caller must hold d.mu.
func (d *Detector) bookImbalance() float64 {
	total := d.lastBidDepthBTC + d.lastAskDepthBTC
	if total <= 0 {
		return 0
	}
	return (d.lastBidDepthBTC - d.lastAskDepthBTC) / total
}

// takerBuyRatio returns the fraction of 30s Binance volume that was buy-side.
// Returns 0 when no trades are recorded. Caller must hold d.mu.
func (d *Detector) takerBuyRatio() float64 {
	var buy, total float64
	for _, s := range d.cvdSamples {
		buy += s.buyBTC
		total += s.buyBTC + s.sellBTC
	}
	if total <= 0 {
		return 0
	}
	return buy / total
}

// yesDrift returns the YES token price change (newest − oldest) over the last
// 10 seconds. Positive = rising; negative = falling. Returns 0 if insufficient
// samples. Caller must hold d.mu.
func (d *Detector) yesDrift() float64 {
	return d.yesDriftOver(10)
}

// btcTickRate5s returns the number of Bitstamp BTC price ticks received in the
// last 5 seconds divided by 5, giving ticks-per-second. Used by RegimeBrain as
// a burst-activity feature. Caller must hold d.mu.
func (d *Detector) btcTickRate5s() float64 {
	cutoff := time.Now().Add(-5 * time.Second)
	count := 0
	for i := len(d.btcGapSamples) - 1; i >= 0; i-- {
		if d.btcGapSamples[i].at.Before(cutoff) {
			break
		}
		count++
	}
	return float64(count) / 5.0
}

// btcTickRun counts consecutive recent BTC price ticks (from btcGapSamples)
// moving in direction dir: +1 = ascending, -1 = descending.
// Caller must hold d.mu.
func (d *Detector) btcTickRun(dir int) int {
	n := len(d.btcGapSamples)
	if n < 2 {
		return 0
	}
	count := 0
	for i := n - 1; i >= 1; i-- {
		delta := d.btcGapSamples[i].price - d.btcGapSamples[i-1].price
		if dir > 0 && delta >= 0 {
			count++
		} else if dir < 0 && delta <= 0 {
			count++
		} else {
			break
		}
	}
	return count
}

// yesDriftOver returns the YES token price change (newest - oldest) over the
// requested lookback window in seconds. Positive = rising; negative = falling.
// Returns 0 if insufficient samples. Caller must hold d.mu.
func (d *Detector) yesDriftOver(windowSec float64) float64 {
	if len(d.yesSamples) < 2 {
		return 0
	}
	if windowSec <= 0 {
		windowSec = 10
	}
	cutoff := time.Now().Add(-time.Duration(windowSec * float64(time.Second)))
	var oldest float64
	for _, s := range d.yesSamples {
		if !s.at.Before(cutoff) {
			oldest = s.price
			break
		}
	}
	if oldest == 0 {
		return 0
	}
	return d.yesSamples[len(d.yesSamples)-1].price - oldest
}

// btcGapVelocityUSDPerSec returns the rate of change of (BTC − open) over the
// specified windowSec. Positive = gap growing (BTC moving away from open);
// negative = gap shrinking. Returns 0 when not enough data. Caller must hold d.mu.
func (d *Detector) btcGapVelocityUSDPerSec(windowSec float64) float64 {
	if d.openPrice == 0 || windowSec <= 0 || len(d.btcGapSamples) < 2 {
		return 0
	}
	cutoff := time.Now().Add(-time.Duration(float64(time.Second) * windowSec))
	var oldest priceSample
	for _, s := range d.btcGapSamples {
		if !s.at.Before(cutoff) {
			oldest = s
			break
		}
	}
	if oldest.price == 0 {
		return 0
	}
	newest := d.btcGapSamples[len(d.btcGapSamples)-1]
	elapsedSec := newest.at.Sub(oldest.at).Seconds()
	if elapsedSec <= 0 {
		return 0
	}
	oldGap := oldest.price - d.openPrice
	newGap := newest.price - d.openPrice
	return (newGap - oldGap) / elapsedSec
}

// btcGapDeltaUSD returns the raw change of (BTC - open) over the requested
// lookback window in USD. Positive = gap moved upward; negative = downward.
// Returns 0 when insufficient samples. Caller must hold d.mu.
func (d *Detector) btcGapDeltaUSD(windowSec float64) float64 {
	if d.openPrice == 0 || windowSec <= 0 || len(d.btcGapSamples) < 2 {
		return 0
	}
	cutoff := time.Now().Add(-time.Duration(float64(time.Second) * windowSec))
	var oldest priceSample
	for _, s := range d.btcGapSamples {
		if !s.at.Before(cutoff) {
			oldest = s
			break
		}
	}
	if oldest.price == 0 {
		return 0
	}
	newest := d.btcGapSamples[len(d.btcGapSamples)-1]
	oldGap := oldest.price - d.openPrice
	newGap := newest.price - d.openPrice
	return newGap - oldGap
}

// ── Orderbook depth (Deribit/Bitstamp book snapshot) ─────────────────────────

// OnBitstampBook ingests a Deribit or Bitstamp orderbook snapshot and updates
// the near-mid bid/ask depth state used by bookImbalance().
// NearMidUSD defines the depth band; hardcoded to 50 USD for snipe-only mode.
func (d *Detector) OnBitstampBook(snap BookSnapshot) {
	const nearMidUSD = 50.0
	d.mu.Lock()
	defer d.mu.Unlock()

	if len(snap.Bids) == 0 && len(snap.Asks) == 0 {
		return
	}

	// Estimate mid price from best bid / best ask.
	var mid float64
	if len(snap.Bids) > 0 && len(snap.Asks) > 0 {
		mid = (snap.Bids[0].Price + snap.Asks[0].Price) / 2.0
	} else if len(snap.Bids) > 0 {
		mid = snap.Bids[0].Price
	} else {
		mid = snap.Asks[0].Price
	}

	var bidBTC, askBTC float64
	for _, b := range snap.Bids {
		if mid-b.Price <= nearMidUSD {
			bidBTC += b.Size
		}
	}
	for _, a := range snap.Asks {
		if a.Price-mid <= nearMidUSD {
			askBTC += a.Size
		}
	}
	d.lastBidDepthBTC = bidBTC
	d.lastAskDepthBTC = askBTC
}
