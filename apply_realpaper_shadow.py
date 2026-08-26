from pathlib import Path

p = Path.home() / "ArboCos" / "realpaper" / "strategy" / "trader.go"
s = p.read_text()

marker = "// REALPAPER SHADOW: continuously test executable two-book depth without opening positions."
if marker in s:
    print("Shadow patch already present")
    raise SystemExit(0)

needle = '''\t_, hedgeNotional, hedgeAllowed := pairArbExactHedgeSizing(estimatedActualShares, hedgePrice, t.feeRateBps)\n\tif !hedgeAllowed {\n\t\treturn fmt.Errorf("pair arb: exact %s hedge would be $%.4f at %.4f, below $%.2f FAK minimum", hedgeSide, hedgeNotional, hedgePrice, polymarket.MinMarketOrderNotionalUSD)\n\t}\n\n\t// Pre-flight balance check: ensure we can afford both legs before committing to the lead.\n'''

replacement = '''\t_, hedgeNotional, hedgeAllowed := pairArbExactHedgeSizing(estimatedActualShares, hedgePrice, t.feeRateBps)\n\tif !hedgeAllowed {\n\t\treturn fmt.Errorf("pair arb: exact %s hedge would be $%.4f at %.4f, below $%.2f FAK minimum", hedgeSide, hedgeNotional, hedgePrice, polymarket.MinMarketOrderNotionalUSD)\n\t}\n\n\t// REALPAPER SHADOW: continuously test executable two-book depth without opening positions.\n\t// This diagnostic runs only in PAPER mode. Every entry candidate that survives the\n\t// purchased strategy gates is checked against simultaneous YES/NO displayed asks,\n\t// then deliberately returned without creating any simulated position.\n\tif t.cfg.PaperTrade {\n\t\tmaxHedgePrice := math.Round((1.0-leadPrice-t.pairArbMinLockedProfit())*100) / 100\n\t\tif maxHedgePrice <= 0 || maxHedgePrice >= 1.0 {\n\t\t\tt.logger.Info("realpaper shadow: REJECT",\n\t\t\t\tzap.String("reason", "invalid hedge cap"),\n\t\t\t\tzap.String("lead_side", leadSide),\n\t\t\t\tzap.Float64("lead_price", leadPrice),\n\t\t\t\tzap.Float64("hedge_quote", hedgePrice),\n\t\t\t\tzap.Float64("max_hedge_price", maxHedgePrice),\n\t\t\t)\n\t\t\treturn nil\n\t\t}\n\n\t\tif hedgePrice > maxHedgePrice+1e-9 {\n\t\t\tt.logger.Info("realpaper shadow: REJECT",\n\t\t\t\tzap.String("reason", "hedge quote exceeds locked-profit cap"),\n\t\t\t\tzap.String("lead_side", leadSide),\n\t\t\t\tzap.Float64("lead_price", leadPrice),\n\t\t\t\tzap.Float64("hedge_quote", hedgePrice),\n\t\t\t\tzap.Float64("max_hedge_price", maxHedgePrice),\n\t\t\t\tzap.Float64("quoted_edge_cents", (1.0-leadPrice-hedgePrice)*100.0),\n\t\t\t)\n\t\t\treturn nil\n\t\t}\n\n\t\thedgeTokenID := noTokenID\n\t\tif isNoLead {\n\t\t\thedgeTokenID = yesTokenID\n\t\t}\n\t\t// Size to the maximum permitted hedge price so the depth test remains\n\t\t// conservative for equal-share completion inside the configured edge cap.\n\t\tshadowHedgeRaw, _, shadowHedgeAllowed := pairArbExactHedgeSizing(estimatedActualShares, maxHedgePrice, t.feeRateBps)\n\t\tif !shadowHedgeAllowed || shadowHedgeRaw <= 0 {\n\t\t\tt.logger.Info("realpaper shadow: REJECT",\n\t\t\t\tzap.String("reason", "shadow hedge sizing unavailable"),\n\t\t\t\tzap.String("lead_side", leadSide),\n\t\t\t\tzap.Float64("max_hedge_price", maxHedgePrice),\n\t\t\t)\n\t\t\treturn nil\n\t\t}\n\n\t\tok, reason, leadVWAP, hedgeVWAP, combined, preErr := t.realpaperPairPreflight(\n\t\t\tctx, tokenID, hedgeTokenID, limitPrice, maxHedgePrice, rawShares, shadowHedgeRaw, 1.0-t.pairArbMinLockedProfit(),\n\t\t)\n\t\tif preErr != nil {\n\t\t\tt.logger.Warn("realpaper shadow: ERROR",\n\t\t\t\tzap.Error(preErr),\n\t\t\t\tzap.String("lead_side", leadSide),\n\t\t\t)\n\t\t\treturn nil\n\t\t}\n\t\tstatus := "REJECT"\n\t\tif ok {\n\t\t\tstatus = "APPROVE"\n\t\t}\n\t\tt.logger.Info("realpaper shadow: "+status,\n\t\t\tzap.String("reason", reason),\n\t\t\tzap.String("lead_side", leadSide),\n\t\t\tzap.Float64("lead_quote", leadPrice),\n\t\t\tzap.Float64("hedge_quote", hedgePrice),\n\t\t\tzap.Float64("lead_vwap", leadVWAP),\n\t\t\tzap.Float64("hedge_vwap", hedgeVWAP),\n\t\t\tzap.Float64("combined_cost", combined),\n\t\t\tzap.Float64("required_edge_cents", t.pairArbMinLockedProfit()*100.0),\n\t\t\tzap.Float64("executable_edge_cents", (1.0-combined)*100.0),\n\t\t)\n\t\treturn nil\n\t}\n\n\t// Pre-flight balance check: ensure we can afford both legs before committing to the lead.\n'''

if needle not in s:
    raise SystemExit("Target block not found; source changed, refusing to patch")

backup = p.with_name("trader.go.pre-shadow")
if not backup.exists():
    backup.write_text(s)

p.write_text(s.replace(needle, replacement, 1))
print("Applied RealPaper shadow collector patch")
print("Backup:", backup)
print("Source:", p)
