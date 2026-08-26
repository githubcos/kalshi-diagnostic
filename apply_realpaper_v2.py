from pathlib import Path
import sys

p = Path.home() / "ArboCos" / "realpaper" / "strategy" / "trader.go"
if not p.exists():
    raise SystemExit(f"missing {p}")

s = p.read_text()

if "REALPAPER v2: two-book preflight" in s:
    print("RealPaper v2 already applied")
    sys.exit(0)

backup = p.with_name("trader.go.realpaper-v1-backup")
if not backup.exists():
    backup.write_text(s)
    print(f"backup: {backup}")

helper_anchor = '''func (t *Trader) clearPrePlacedHedgeOrder() {
'''
idx = s.find(helper_anchor)
if idx < 0:
    raise SystemExit("could not find clearPrePlacedHedgeOrder")

# Insert helper immediately before executeDualPrePlacePairArb, after clearPrePlacedHedgeOrder.
insert_anchor = '''// executeDualPrePlacePairArb places the lead buy AND the hedge buy simultaneously as
'''
pos = s.find(insert_anchor)
if pos < 0:
    raise SystemExit("could not find executeDualPrePlacePairArb anchor")

helper = r'''
// REALPAPER v2: two-book preflight. In PAPER mode, prove that both legs have
// enough currently displayed Polymarket ask depth for the intended gross share
// quantities before allowing the first simulated leg to open. This does not make
// two exchange orders atomic; it is a conservative diagnostic guard against
// predictable lead-only exposure.
func (t *Trader) realpaperPairPreflight(
	ctx context.Context,
	leadTokenID, hedgeTokenID string,
	leadLimitPrice, hedgeLimitPrice float64,
	leadRawShares, hedgeRawShares float64,
	maxCombinedCost float64,
) (bool, string, float64, float64, float64, error) {
	if !t.cfg.PaperTrade {
		return true, "live mode bypass", 0, 0, 0, nil
	}
	if t.orders == nil {
		return false, "nil orders client", 0, 0, 0, fmt.Errorf("realpaper v2: nil orders client")
	}
	if leadTokenID == "" || hedgeTokenID == "" {
		return false, "missing token id", 0, 0, 0, nil
	}
	if leadRawShares <= pairArbShareDust || hedgeRawShares <= pairArbShareDust {
		return false, "invalid share sizing", 0, 0, 0, nil
	}
	if leadLimitPrice <= 0 || leadLimitPrice >= 1 || hedgeLimitPrice <= 0 || hedgeLimitPrice >= 1 {
		return false, "invalid limit price", 0, 0, 0, nil
	}
	if maxCombinedCost <= 0 || maxCombinedCost >= 1 {
		return false, "invalid combined-cost cap", 0, 0, 0, nil
	}

	bookCtx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()

	type bookResult struct {
		book *polymarket.OrderBook
		err  error
	}
	leadCh := make(chan bookResult, 1)
	hedgeCh := make(chan bookResult, 1)
	go func() {
		b, err := t.orders.GetOrderBook(bookCtx, leadTokenID)
		leadCh <- bookResult{book: b, err: err}
	}()
	go func() {
		b, err := t.orders.GetOrderBook(bookCtx, hedgeTokenID)
		hedgeCh <- bookResult{book: b, err: err}
	}()

	leadRes := <-leadCh
	hedgeRes := <-hedgeCh
	if leadRes.err != nil {
		return false, "lead book fetch failed", 0, 0, 0, fmt.Errorf("realpaper v2: lead orderbook: %w", leadRes.err)
	}
	if hedgeRes.err != nil {
		return false, "hedge book fetch failed", 0, 0, 0, fmt.Errorf("realpaper v2: hedge orderbook: %w", hedgeRes.err)
	}

	walk := func(book *polymarket.OrderBook, limitPrice, rawShares float64) (vwap, netShares, cost float64, ok bool) {
		if book == nil || len(book.Asks) == 0 {
			return 0, 0, 0, false
		}
		type level struct{ price, size float64 }
		levels := make([]level, 0, len(book.Asks))
		for _, a := range book.Asks {
			price, errP := strconv.ParseFloat(strings.TrimSpace(a.Price), 64)
			size, errS := strconv.ParseFloat(strings.TrimSpace(a.Size), 64)
			if errP != nil || errS != nil || price <= 0 || size <= 0 || price > limitPrice+1e-9 {
				continue
			}
			levels = append(levels, level{price: price, size: size})
		}
		// Small manual sort avoids adding another import to the purchased source.
		for i := 0; i < len(levels); i++ {
			for j := i + 1; j < len(levels); j++ {
				if levels[j].price < levels[i].price {
					levels[i], levels[j] = levels[j], levels[i]
				}
			}
		}
		remaining := rawShares
		for _, lv := range levels {
			if remaining <= pairArbShareDust {
				break
			}
			take := math.Min(remaining, lv.size)
			cost += take * lv.price
			remaining -= take
		}
		if remaining > pairArbShareDust || cost <= 0 {
			return 0, 0, 0, false
		}
		vwap = cost / rawShares
		feeShares := polymarket.ComputeBuyFeeShares(rawShares, vwap, t.feeRateBps)
		netShares = math.Floor((rawShares-feeShares)*100) / 100
		if netShares <= pairArbShareDust {
			return 0, 0, 0, false
		}
		return vwap, netShares, cost, true
	}

	leadVWAP, leadNet, leadCost, leadOK := walk(leadRes.book, leadLimitPrice, leadRawShares)
	if !leadOK {
		return false, "lead depth insufficient at limit", 0, 0, 0, nil
	}
	hedgeVWAP, hedgeNet, hedgeCost, hedgeOK := walk(hedgeRes.book, hedgeLimitPrice, hedgeRawShares)
	if !hedgeOK {
		return false, "hedge depth insufficient at limit", leadVWAP, 0, 0, nil
	}

	matched := math.Min(leadNet, hedgeNet)
	if matched <= pairArbShareDust {
		return false, "no matched net shares", leadVWAP, hedgeVWAP, 0, nil
	}
	// More than two hundredths of a share mismatch means the preflight would
	// knowingly create a residual even if both simulated orders filled.
	if math.Abs(leadNet-hedgeNet) > 0.02+1e-9 {
		combined := (leadCost + hedgeCost) / matched
		return false, fmt.Sprintf("net share mismatch lead=%.2f hedge=%.2f", leadNet, hedgeNet), leadVWAP, hedgeVWAP, combined, nil
	}

	// Conservative: charge all gross spend against the matched net shares.
	combined := (leadCost + hedgeCost) / matched
	if combined > maxCombinedCost+1e-9 {
		return false, fmt.Sprintf("combined executable cost %.4f exceeds cap %.4f", combined, maxCombinedCost), leadVWAP, hedgeVWAP, combined, nil
	}
	return true, "approved", leadVWAP, hedgeVWAP, combined, nil
}

'''

s = s[:pos] + helper + s[pos:]

needle = '''\testimatedLeadActual := math.Floor((rawLeadShares-polymarket.ComputeBuyFeeShares(rawLeadShares, leadPrice, t.feeRateBps))*100) / 100
\thedgeRawShares, _, hedgeAllowed := pairArbExactHedgeSizing(estimatedLeadActual, hedgeLimitPrice, t.feeRateBps)
\tif !hedgeAllowed || hedgeRawShares <= 0 {
'''
replacement = '''\testimatedLeadActual := math.Floor((rawLeadShares-polymarket.ComputeBuyFeeShares(rawLeadShares, leadPrice, t.feeRateBps))*100) / 100
\thedgeRawShares, _, hedgeAllowed := pairArbExactHedgeSizing(estimatedLeadActual, hedgeLimitPrice, t.feeRateBps)

\t// REALPAPER v2: two-book preflight happens before any lead exposure exists.
\tif t.cfg.PaperTrade {
\t\tif !hedgeAllowed || hedgeRawShares <= 0 {
\t\t\tt.logger.Info("realpaper v2: pair preflight rejected",
\t\t\t\tzap.String("reason", "hedge sizing unavailable"),
\t\t\t\tzap.String("lead_side", leadSide),
\t\t\t\tzap.Float64("lead_price", leadPrice),
\t\t\t\tzap.Float64("hedge_limit", hedgeLimitPrice),
\t\t\t)
\t\t\treturn fmt.Errorf("pair arb: realpaper v2 preflight rejected: hedge sizing unavailable")
\t\t}
\t\thedgeTokenIDForPreflight := yesTokenID
\t\tif !isNoLead {
\t\t\thedgeTokenIDForPreflight = noTokenID
\t\t}
\t\tmaxCombinedCost := leadPrice + maxHedgePrice
\t\tok, reason, leadVWAP, hedgeVWAP, combined, preErr := t.realpaperPairPreflight(
\t\t\tctx,
\t\t\tleadTokenID,
\t\t\thedgeTokenIDForPreflight,
\t\t\tleadLimitPrice,
\t\t\thedgeLimitPrice,
\t\t\trawLeadShares,
\t\t\thedgeRawShares,
\t\t\tmaxCombinedCost,
\t\t)
\t\tif preErr != nil {
\t\t\tt.logger.Warn("realpaper v2: pair preflight error", zap.Error(preErr))
\t\t\treturn preErr
\t\t}
\t\tif !ok {
\t\t\tt.logger.Info("realpaper v2: pair preflight rejected",
\t\t\t\tzap.String("reason", reason),
\t\t\t\tzap.String("lead_side", leadSide),
\t\t\t\tzap.Float64("lead_vwap", leadVWAP),
\t\t\t\tzap.Float64("hedge_vwap", hedgeVWAP),
\t\t\t\tzap.Float64("combined_cost", combined),
\t\t\t\tzap.Float64("combined_cap", maxCombinedCost),
\t\t\t)
\t\t\treturn fmt.Errorf("pair arb: realpaper v2 preflight rejected: %s", reason)
\t\t}
\t\tt.logger.Info("realpaper v2: pair preflight approved",
\t\t\tzap.String("lead_side", leadSide),
\t\t\tzap.Float64("lead_vwap", leadVWAP),
\t\t\tzap.Float64("hedge_vwap", hedgeVWAP),
\t\t\tzap.Float64("combined_cost", combined),
\t\t\tzap.Float64("combined_cap", maxCombinedCost),
\t\t\tzap.Float64("locked_edge_cents", (1.0-combined)*100.0),
\t\t)
\t}

\tif !hedgeAllowed || hedgeRawShares <= 0 {
'''

if needle not in s:
    raise SystemExit("could not find hedge sizing anchor; source differs from expected RealPaper v1")

s = s.replace(needle, replacement, 1)
p.write_text(s)
print("Applied REALPAPER v2 two-book preflight to", p)
print("Original RealPaper v1 backup at", backup)
