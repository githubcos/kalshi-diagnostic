package strategy

import (
	"context"
	"math"
	"testing"

	"github.com/polyarb/polymarket"
)

type kalshiBookOnlyMock struct {
	books map[string]*polymarket.OrderBook
}

func (m *kalshiBookOnlyMock) GetOrderBook(_ context.Context, id string) (*polymarket.OrderBook, error) {
	return m.books[id], nil
}

func TestKalshiTakerFeeUSD(t *testing.T) {
	got := kalshiTakerFeeUSD(10, 0.50)
	if math.Abs(got-0.18) > 1e-9 {
		t.Fatalf("expected $0.18 fee, got %.4f", got)
	}
}
func TestPairKalshiMaxHedgePriceIncludesCashFees(t *testing.T) {
	// Lead effective cost 0.51 and 8c desired lock. Raw no-fee hedge cap would 0.41;
	// Kalshi fee requires a lower executable hedge limit.
	p := pairKalshiMaxHedgePrice(0.51, 10, 0.08)
	if p >= 0.41 || p <= 0 {
		t.Fatalf("expected fee-adjusted cap below .41, got %.4f", p)
	}
}
func TestPairBookLevelsExecutableDepth(t *testing.T) {
	levels := pairBookLevels([]polymarket.PriceSize{{Price: "0.42", Size: "3"}, {Price: "0.40", Size: "2"}}, true)
	if len(levels) != 2 || levels[0][0] != 0.40 || levels[1][0] != 0.42 {
		t.Fatalf("bad ask ordering: %#v", levels)
	}
}
