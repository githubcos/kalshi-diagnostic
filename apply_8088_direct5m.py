#!/usr/bin/env python3
from pathlib import Path

p = Path.home()/"ArboCos"/"patched8088"/"strategy"/"trader.go"
s = p.read_text()

if "DIRECT5M: executable-pair scanner" in s:
    print("direct 5m scanner already patched")
    raise SystemExit(0)

# Add one per-Trader scanner condition marker next to the current market identifiers.
needle = "\tconvConditionID string\n"
if needle not in s:
    raise SystemExit("convConditionID field not found")
s = s.replace(needle, needle + "\tdirect5mScannerCondition string\n", 1)

# Start/restart scanner when the active market tokens are updated.
needle2 = "\tt.convConditionID = conditionID\n"
if needle2 not in s:
    raise SystemExit("SetMarketTokens condition assignment not found")
insert2 = needle2 + '''\n\t// DIRECT5M: executable-pair scanner. This bypasses the purchased directional\n\t// signal filters and watches the active BTC 5-minute YES/NO books directly.\n\t// It is PAPER-only for validation. Existing risk controls and the RealPaper v2\n\t// two-book preflight remain in force before any simulated exposure can open.\n\tif t.cfg.PaperTrade && yesTokenID != "" && noTokenID != "" && conditionID != "" && t.direct5mScannerCondition != conditionID {\n\t\tt.direct5mScannerCondition = conditionID\n\t\tgo t.runDirect5mExecutablePairScanner(yesTokenID, noTokenID, conditionID)\n\t}\n'''
s = s.replace(needle2, insert2, 1)

# Append scanner method before OnPairArbSignal so it can call that existing execution path.
anchor = "func (t *Trader) OnPairArbSignal(ctx context.Context, sig Signal, yesTokenID, noTokenID string) error {"
if anchor not in s:
    raise SystemExit("OnPairArbSignal anchor not found")
method = r'''
// DIRECT5M: executable-pair scanner for the active BTC 5-minute market.
// It does not use BTC gap/velocity/direction filters. It only emits an internal
// pair signal when the current displayed best asks already imply at least the
// configured locked-profit edge. OnPairArbSignal then performs the normal risk,
// balance and RealPaper-v2 depth/VWAP preflight before any simulated order.
func (t *Trader) runDirect5mExecutablePairScanner(yesTokenID, noTokenID, conditionID string) {
	const poll = 500 * time.Millisecond
	for {
		time.Sleep(poll)
		if !t.cfg.PaperTrade || t.convConditionID != conditionID || t.convYesTokenID != yesTokenID || t.convNoTokenID != noTokenID {
			return
		}
		if t.orders == nil || t.buyInProgress || t.pairedPosition != nil {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 1800*time.Millisecond)
		type br struct { b *polymarket.OrderBook; err error }
		yc := make(chan br, 1)
		nc := make(chan br, 1)
		go func(){ b,e := t.orders.GetOrderBook(ctx, yesTokenID); yc <- br{b,e} }()
		go func(){ b,e := t.orders.GetOrderBook(ctx, noTokenID);  nc <- br{b,e} }()
		yr, nr := <-yc, <-nc
		cancel()
		if yr.err != nil || nr.err != nil || yr.b == nil || nr.b == nil {
			continue
		}

		bestAsk := func(book *polymarket.OrderBook) (float64, bool) {
			best := 2.0
			for _, a := range book.Asks {
				p, e1 := strconv.ParseFloat(strings.TrimSpace(a.Price), 64)
				sz, e2 := strconv.ParseFloat(strings.TrimSpace(a.Size), 64)
				if e1 == nil && e2 == nil && p > 0 && p < 1 && sz > pairArbShareDust && p < best {
					best = p
				}
			}
			return best, best < 1
		}
		yesAsk, yok := bestAsk(yr.b)
		noAsk, nok := bestAsk(nr.b)
		if !yok || !nok {
			continue
		}

		minEdge := t.pairArbMinLockedProfit()
		combined := yesAsk + noAsk
		if combined > 1.0-minEdge+1e-9 {
			continue
		}

		leadType := SignalPairArbLeadYes
		if noAsk < yesAsk {
			leadType = SignalPairArbLeadNo
		}
		sig := Signal{
			Type:         leadType,
			PolyYesPrice: yesAsk,
			PolyNoPrice:  noAsk,
		}
		t.logger.Info("DIRECT5M: executable pair candidate",
			zap.Float64("yes_ask", yesAsk),
			zap.Float64("no_ask", noAsk),
			zap.Float64("combined", combined),
			zap.Float64("gross_edge_cents", (1.0-combined)*100.0),
			zap.String("condition_id", conditionID),
		)

		tradeCtx, tradeCancel := context.WithTimeout(context.Background(), 8*time.Second)
		err := t.OnPairArbSignal(tradeCtx, sig, yesTokenID, noTokenID)
		tradeCancel()
		if err != nil {
			t.logger.Info("DIRECT5M: candidate rejected by execution/risk gate", zap.Error(err))
		} else {
			t.logger.Info("DIRECT5M: candidate accepted by execution path")
		}
		// Avoid hammering the same transient quote repeatedly.
		time.Sleep(750 * time.Millisecond)
	}
}

'''
s = s.replace(anchor, method + anchor, 1)
p.write_text(s)
print(f"patched {p}")
