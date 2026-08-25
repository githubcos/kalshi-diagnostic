package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	binapkg "github.com/polyarb/binance"
	bybitpkg "github.com/polyarb/bybit"
	"github.com/polyarb/chainlink"
	coinbasepkg "github.com/polyarb/coinbase"
	"github.com/polyarb/config"
	deribitpkg "github.com/polyarb/deribit"
	"github.com/polyarb/discord"
	"github.com/polyarb/display"
	"github.com/polyarb/exchangefeed"
	"github.com/polyarb/kalshi"
	"github.com/polyarb/market"
	"github.com/polyarb/polymarket"
	"github.com/polyarb/strategy"
	webpkg "github.com/polyarb/web"
)

const signalEventsRotateMaxBytes int64 = 64 * 1024 * 1024

func attachTradeCloseHandlers(trader *strategy.Trader, store *webpkg.Store, logger *zap.Logger, cfg *config.Config, metrics *strategy.BotMetrics) {
	notifier := discord.NewNotifier(cfg.DiscordTradeWebhookURL)
	mode := "LIVE"
	if cfg.PaperTrade {
		mode = "PAPER"
	}

	type pairOutcomeState struct {
		Merged strategy.TradeRecord
	}
	pairOutcomeByOpenedAt := make(map[int64]*pairOutcomeState)

	trader.OnTradeClose = func(rec strategy.TradeRecord) {
		if metrics != nil {
			metrics.Record("trade_close", 0)
		}

		// The Trader journal / SessionStats() is the authoritative source
		// for session P&L, trade count, wins and losses. Do NOT independently
		// mutate those counters here, otherwise pair-arb legs can be counted
		// differently from CollapseTradeRecords().
		if store != nil {
			store.AddTrade(webpkg.TradeEntry{
				OpenedAt:  rec.OpenedAt,
				ClosedAt:  rec.ClosedAt,
				Strategy:  rec.Strategy,
				Side:      rec.Side,
				BuyPrice:  rec.BuyPrice,
				SellPrice: rec.SellPrice,
				Shares:    rec.Shares,
				USDSpent:  rec.USDSpent,
				PnL:       rec.PnL,
				Reason:    rec.Reason,
				HeldSec:   rec.HeldSec,
			})

			// All-time P&L is still maintained here because SessionStats()
			// represents only the current runtime session.
			store.Update(func(s *webpkg.BotState) {
				s.AllTimePnL += rec.PnL
			})
		}

		if !cfg.DiscordTradeWebhookEnabled || !notifier.Enabled() {
			return
		}

		if rec.Strategy != "pair_arb" {
			go func(rec strategy.TradeRecord) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := notifier.SendTradeClose(ctx, mode, rec); err != nil {
					logger.Warn("discord trade webhook failed",
						zap.String("strategy", rec.Strategy),
						zap.String("side", rec.Side),
						zap.Error(err),
					)
				}
			}(rec)
			return
		}

		// Pair-arb can close as multiple journal legs. Merge them for one
		// Discord notification; SessionStats() separately collapses them
		// for dashboard accounting.
		key := rec.OpenedAt.UnixNano()
		state := pairOutcomeByOpenedAt[key]
		if state == nil {
			state = &pairOutcomeState{}
			pairOutcomeByOpenedAt[key] = state
		}

		state.Merged = strategy.MergeTradeRecords(state.Merged, rec)
		toSend := state.Merged

		go func(r strategy.TradeRecord) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := notifier.SendTradeClose(ctx, mode, r); err != nil {
				logger.Warn("discord trade webhook failed",
					zap.String("strategy", r.Strategy),
					zap.String("side", r.Side),
					zap.Error(err),
				)
			}
		}(toSend)
	}
}

func logRuntimeStrategyConfig(logger *zap.Logger, cfg *config.Config, envFile string, liveFlag bool) {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	mode := "PAPER"
	if !cfg.PaperTrade {
		mode = "LIVE"
	}

	logger.Info("runtime strategy config",
		zap.String("cwd", cwd),
		zap.String("config_json", filepath.Join(cwd, "config.json")),
		zap.String("env_file", filepath.Join(cwd, envFile)),
		zap.String("runtime_mode", mode),
		zap.Bool("live_flag", liveFlag),
		zap.Bool("paper_trade", cfg.PaperTrade),
		zap.String("market_type", cfg.MarketType),
		zap.Bool("pair_arb_enabled", cfg.PairArbEnabled),
		zap.Bool("pair_arb_sell_at_99", cfg.PairArbSellAt99),
		zap.String("pair_arb_schedule_mode", cfg.PairArbScheduleMode),
		zap.String("pair_arb_schedule_windows_utc", cfg.PairArbScheduleWindowsUTC),
	)
}

func runtimeConfigSource(envFile string) string {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	return fmt.Sprintf("CONFIG SOURCE cwd=%s config=%s env=%s", cwd, filepath.Join(cwd, "config.json"), filepath.Join(cwd, envFile))
}

func activeStrategyFlagsSummary(cfg *config.Config) string {
	flags := []string{
		fmt.Sprintf("paper=%t", cfg.PaperTrade),
		fmt.Sprintf("market=%s", cfg.MarketType),
		fmt.Sprintf("pair_arb=%t", cfg.PairArbEnabled),
		fmt.Sprintf("pair_arb_sell_at_99=%t", cfg.PairArbSellAt99),
		fmt.Sprintf("ml_filter=%t", cfg.PairArbMLFilterEnabled),
	}
	return "ACTIVE FLAGS " + strings.Join(flags, " ")
}

func runMLFilterTraining(ctx context.Context, logger *zap.Logger, cfg *config.Config) (string, error) {
	out, err := webpkg.RunGoMLFilterTraining(ctx, webpkg.GoMLTrainOptions{
		Hours:             0,
		TestFrac:          0.3,
		Reverse:           cfg.PairArbReverseSignalEnabled,
		SignalFile:        cfg.SignalEventsFile,
		ArchiveGlob:       filepath.Join("archive_signals", "signals_*.jsonl"),
		StateFile:         cfg.PairArbMLFilterStateFile,
		ScoreThreshold:    cfg.PairArbMLFilterThreshold,
		LabelThresholdUSD: cfg.PairArbMLFilterLabelThresholdUSD,
		MinSamples:        cfg.PairArbMLFilterMinSamples,
	})
	out = strings.TrimSpace(out)
	if err != nil {
		if out != "" {
			logger.Warn("ml filter train failed", zap.Error(err), zap.String("output", out))
		} else {
			logger.Warn("ml filter train failed", zap.Error(err))
		}
		return out, err
	}
	if out != "" {
		logger.Info("ml filter train completed", zap.String("output", out))
	}
	return out, nil
}

func pruneArchivedSignalsOlderThan(hours int) (int, error) {
	if hours <= 0 {
		return 0, nil
	}
	paths, err := filepath.Glob(filepath.Join("archive_signals", "signals_*.jsonl"))
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour)
	removed := 0
	for _, path := range paths {
		base := filepath.Base(path)
		ts := strings.TrimPrefix(base, "signals_")
		ts = strings.TrimSuffix(ts, ".jsonl")
		fileTime, parseErr := time.Parse("20060102_150405", ts)
		if parseErr != nil {
			fi, statErr := os.Stat(path)
			if statErr != nil {
				continue
			}
			fileTime = fi.ModTime()
		}
		if fileTime.Before(cutoff) {
			if err := os.Remove(path); err == nil {
				removed++
			}
		}
	}
	return removed, nil
}

func isExpectedPairArbEntryMiss(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "wallet credit not confirmed yet") ||
		strings.Contains(s, "wallet credit not visible yet") ||
		strings.Contains(s, "wallet still empty; waiting") ||
		strings.Contains(s, "lead matched but wallet credit pending") ||
		strings.Contains(s, "lead buy not filled") ||
		(strings.Contains(s, "pending hedge order") && strings.Contains(s, "waiting before new lead")) ||
		strings.Contains(s, "fak not filled") ||
		strings.Contains(s, "order rejected") ||
		strings.Contains(s, "no edge after slippage") ||
		strings.Contains(s, "too early in window") ||
		strings.Contains(s, "too late in window") ||
		strings.Contains(s, "gap too small") ||
		strings.Contains(s, "gap too large") ||
		strings.Contains(s, "invalid entry prices")
}

func snapshotStrategyGates(detector *strategy.Detector, inPosition, buyInProgress bool) []webpkg.StrategyGate {
	if detector == nil {
		return nil
	}
	gateStatuses := detector.ActiveStrategyGates(inPosition, buyInProgress)
	webGates := make([]webpkg.StrategyGate, 0, len(gateStatuses))
	for _, gate := range gateStatuses {
		webGates = append(webGates, webpkg.StrategyGate{
			Key:     gate.Key,
			Name:    gate.Name,
			Status:  gate.Status,
			Ready:   gate.Ready,
			Blocker: gate.Blocker,
		})
	}
	return webGates
}

func snapshotPairArbIndicators(cfg *config.Config, detector *strategy.Detector, inPosition, buyInProgress bool) ([]webpkg.WindowIndicator, string, time.Time) {
	if cfg == nil || detector == nil {
		return nil, "", time.Time{}
	}
	snap := detector.PairArbSnapshot()
	indicators := make([]webpkg.WindowIndicator, 0, 14)
	blocker := ""
	setBlocker := func(reason string) {
		if blocker == "" {
			blocker = reason
		}
	}
	add := func(key, label, value, threshold string, active bool) {
		if !active {
			setBlocker(label)
		}
		indicators = append(indicators, webpkg.WindowIndicator{
			Key:       key,
			Label:     label,
			Value:     value,
			Threshold: threshold,
			Active:    active,
		})
	}

	if !cfg.PairArbEnabled {
		add("pair_arb_enabled", "Pair Arb Enabled", "false", "must be true", false)
		return indicators, "strategy disabled", snap.At
	}
	add("pair_arb_enabled", "Pair Arb Enabled", "true", "must be true", true)

	if inPosition {
		setBlocker("in position")
	}
	if buyInProgress {
		setBlocker("buy in progress")
	}

	windowRem := snap.WindowRemSec
	if windowRem < 0 {
		windowRem = 0
	}
	elapsed := detector.WindowElapsedSec()
	if elapsed < 0 {
		elapsed = 0
	}
	windowOK := elapsed >= float64(cfg.PairArbMinWindowSec) && elapsed <= float64(cfg.PairArbMaxWindowSec)
	add(
		"window_elapsed",
		"Window Elapsed",
		fmt.Sprintf("%.1fs", elapsed),
		fmt.Sprintf("%d-%ds", cfg.PairArbMinWindowSec, cfg.PairArbMaxWindowSec),
		windowOK,
	)

	if snap.GapUSD == 0 {
		setBlocker("gap=0")
	}
	leadSide := "YES"
	leadPrice := snap.YesPrice
	if snap.GapUSD < 0 {
		leadSide = "NO"
		leadPrice = snap.NoPrice
	}
	absGap := math.Abs(snap.GapUSD)
	minGap := cfg.PairArbMinBTCGapUSD
	minVel := cfg.PairArbMinGapVelocityUSD
	if cfg.PairArbEarlyWindowMaxSec > 0 && elapsed <= float64(cfg.PairArbEarlyWindowMaxSec) {
		if cfg.PairArbEarlyMinGapUSD > 0 && cfg.PairArbEarlyMinGapUSD < minGap {
			minGap = cfg.PairArbEarlyMinGapUSD
		}
		if cfg.PairArbEarlyMinVelocityUSD > 0 {
			minVel = cfg.PairArbEarlyMinVelocityUSD
		}
	}
	gapOK := absGap >= minGap
	if cfg.PairArbMaxBTCGapUSD > 0 && absGap > cfg.PairArbMaxBTCGapUSD {
		gapOK = false
	}
	gapThreshold := fmt.Sprintf(">= $%.2f", minGap)
	if cfg.PairArbMaxBTCGapUSD > 0 {
		gapThreshold = fmt.Sprintf(">= $%.2f and <= $%.2f", minGap, cfg.PairArbMaxBTCGapUSD)
	}
	add("gap_abs", "|BTC-open| Gap", fmt.Sprintf("$%.2f", absGap), gapThreshold, gapOK)

	velOK := true
	velThreshold := "off"
	if minVel > 0 {
		if snap.GapUSD >= 0 {
			velOK = snap.GapVelocity >= minVel
			velThreshold = fmt.Sprintf(">= +%.2f/s", minVel)
		} else {
			velOK = snap.GapVelocity <= -minVel
			velThreshold = fmt.Sprintf("<= -%.2f/s", minVel)
		}
	}
	add("gap_velocity", "Gap Velocity", fmt.Sprintf("%.2f/s", snap.GapVelocity), velThreshold, velOK)

	tokenOK := leadPrice >= cfg.PairArbMinTokenPrice && leadPrice <= cfg.PairArbMaxTokenPrice
	add(
		"lead_token_price",
		"Lead Token Price",
		fmt.Sprintf("%s %.2f", leadSide, leadPrice),
		fmt.Sprintf("[%.2f, %.2f]", cfg.PairArbMinTokenPrice, cfg.PairArbMaxTokenPrice),
		tokenOK,
	)

	cvdOK := true
	cvdThreshold := "off"
	if cfg.PairArbMinCVDBTC > 0 {
		if cfg.PairArbFlowMovementMode {
			cvdOK = math.Abs(snap.CVDBTC) >= cfg.PairArbMinCVDBTC
			cvdThreshold = fmt.Sprintf("|CVD| >= %.2f BTC", cfg.PairArbMinCVDBTC)
		} else {
			if snap.GapUSD >= 0 {
				cvdOK = snap.CVDBTC >= cfg.PairArbMinCVDBTC
				cvdThreshold = fmt.Sprintf(">= +%.2f BTC", cfg.PairArbMinCVDBTC)
			} else {
				cvdOK = snap.CVDBTC <= -cfg.PairArbMinCVDBTC
				cvdThreshold = fmt.Sprintf("<= -%.2f BTC", cfg.PairArbMinCVDBTC)
			}
		}
	}
	add("cvd", "CVD", fmt.Sprintf("%.2f BTC", snap.CVDBTC), cvdThreshold, cvdOK)

	bookOK := true
	bookThreshold := "off"
	if cfg.PairArbMinBookImbalance > 0 {
		if cfg.PairArbFlowMovementMode {
			bookOK = math.Abs(snap.BookImbalance) >= cfg.PairArbMinBookImbalance
			bookThreshold = fmt.Sprintf("|imb| >= %.2f", cfg.PairArbMinBookImbalance)
		} else {
			if snap.GapUSD >= 0 {
				bookOK = snap.BookImbalance >= cfg.PairArbMinBookImbalance
				bookThreshold = fmt.Sprintf(">= +%.2f", cfg.PairArbMinBookImbalance)
			} else {
				bookOK = snap.BookImbalance <= -cfg.PairArbMinBookImbalance
				bookThreshold = fmt.Sprintf("<= -%.2f", cfg.PairArbMinBookImbalance)
			}
		}
	}
	add("book_imbalance", "Book Imbalance", fmt.Sprintf("%.2f", snap.BookImbalance), bookThreshold, bookOK)

	spreadOK := true
	spreadThreshold := "off"
	spreadThresholds := make([]string, 0, 2)
	if cfg.PairArbMinCoinbaseSpreadUSD > 0 {
		if cfg.PairArbFlowMovementMode {
			spreadOK = spreadOK && math.Abs(snap.CoinbaseSpread) >= cfg.PairArbMinCoinbaseSpreadUSD
			spreadThresholds = append(spreadThresholds, fmt.Sprintf("|spread| >= %.2f", cfg.PairArbMinCoinbaseSpreadUSD))
		} else {
			if snap.GapUSD >= 0 {
				spreadOK = spreadOK && snap.CoinbaseSpread >= cfg.PairArbMinCoinbaseSpreadUSD
				spreadThresholds = append(spreadThresholds, fmt.Sprintf(">= +%.2f", cfg.PairArbMinCoinbaseSpreadUSD))
			} else {
				spreadOK = spreadOK && snap.CoinbaseSpread <= -cfg.PairArbMinCoinbaseSpreadUSD
				spreadThresholds = append(spreadThresholds, fmt.Sprintf("<= -%.2f", cfg.PairArbMinCoinbaseSpreadUSD))
			}
		}
	}
	if cfg.PairArbMaxCoinbaseSpreadUSD > 0 {
		if cfg.PairArbFlowMovementMode {
			spreadOK = spreadOK && math.Abs(snap.CoinbaseSpread) <= cfg.PairArbMaxCoinbaseSpreadUSD
			spreadThresholds = append(spreadThresholds, fmt.Sprintf("|spread| <= %.2f", cfg.PairArbMaxCoinbaseSpreadUSD))
		} else {
			if snap.GapUSD >= 0 {
				spreadOK = spreadOK && snap.CoinbaseSpread <= cfg.PairArbMaxCoinbaseSpreadUSD
				spreadThresholds = append(spreadThresholds, fmt.Sprintf("<= +%.2f", cfg.PairArbMaxCoinbaseSpreadUSD))
			} else {
				spreadOK = spreadOK && snap.CoinbaseSpread >= -cfg.PairArbMaxCoinbaseSpreadUSD
				spreadThresholds = append(spreadThresholds, fmt.Sprintf(">= -%.2f", cfg.PairArbMaxCoinbaseSpreadUSD))
			}
		}
	}
	if len(spreadThresholds) > 0 {
		spreadThreshold = strings.Join(spreadThresholds, " & ")
	}
	add("coinbase_spread", "Coinbase Spread", fmt.Sprintf("%+.2f", snap.CoinbaseSpread), spreadThreshold, spreadOK)

	takerOK := true
	takerThreshold := "off"
	if cfg.PairArbMinCoinbaseTakerImbalance > 0 {
		if snap.GapUSD >= 0 {
			takerOK = snap.CoinbaseTakerImb >= cfg.PairArbMinCoinbaseTakerImbalance
			takerThreshold = fmt.Sprintf(">= +%.2f", cfg.PairArbMinCoinbaseTakerImbalance)
		} else {
			takerOK = snap.CoinbaseTakerImb <= -cfg.PairArbMinCoinbaseTakerImbalance
			takerThreshold = fmt.Sprintf("<= -%.2f", cfg.PairArbMinCoinbaseTakerImbalance)
		}
	}
	add("coinbase_taker_imbalance", "Coinbase Taker Imb", fmt.Sprintf("%+.2f", snap.CoinbaseTakerImb), takerThreshold, takerOK)

	gapHoldOK := true
	gapHoldThreshold := "off"
	if cfg.PairArbMinGapHoldSec > 0 {
		gapHoldThreshold = fmt.Sprintf(">= %ds", cfg.PairArbMinGapHoldSec)
		gapHoldOK = snap.GapHoldSec >= float64(cfg.PairArbMinGapHoldSec)
	}
	add("gap_hold_sec", "Gap Hold", fmt.Sprintf("%.1fs", snap.GapHoldSec), gapHoldThreshold, gapHoldOK)

	tickRunOK := true
	tickRunThreshold := "off"
	if cfg.PairArbMinBTCTickRun > 0 {
		tickRunThreshold = fmt.Sprintf(">= %d", cfg.PairArbMinBTCTickRun)
		tickRunOK = snap.BTCTickRun >= cfg.PairArbMinBTCTickRun
	}
	add("btc_tick_run", "BTC Tick Run", fmt.Sprintf("%d", snap.BTCTickRun), tickRunThreshold, tickRunOK)

	clAgeOK := true
	clAgeThreshold := "off"
	if cfg.PairArbMaxCLAgeSec > 0 {
		clAgeThreshold = fmt.Sprintf("<= %ds", cfg.PairArbMaxCLAgeSec)
		clAgeOK = snap.ChainlinkAgeSec <= float64(cfg.PairArbMaxCLAgeSec)
	}
	add("chainlink_age", "Chainlink Age", fmt.Sprintf("%.1fs", snap.ChainlinkAgeSec), clAgeThreshold, clAgeOK)

	mlValue := "off"
	mlThreshold := "off"
	mlActive := true
	if cfg.PairArbMLFilterEnabled {
		mlValue = fmt.Sprintf("score=%.2f n=%d", snap.BrainScore, snap.BrainLabeled)
		mlThreshold = fmt.Sprintf(">= %.2f (n>=%d)", cfg.PairArbMLFilterThreshold, cfg.PairArbMLFilterMinSamples)
		mlActive = snap.BrainScore >= cfg.PairArbMLFilterThreshold
	}
	add("ml_filter", "ML Filter", mlValue, mlThreshold, mlActive)

	if cfg.PairArbCVDMomentumEnabled {
		cvdMomMaxEl := float64(cfg.PairArbCVDMomentumMaxElapsedSec)
		if cvdMomMaxEl <= 0 {
			cvdMomMaxEl = 60.0
		}
		cvdMomMaxGap := cfg.PairArbCVDMomentumMaxGapUSD
		if cvdMomMaxGap <= 0 {
			cvdMomMaxGap = 15.0
		}
		cvdMomMinCVD := cfg.PairArbCVDMomentumMinCVDBTC
		// CVD momentum is an independent OR-path: do not affect the main blocker.
		indicators = append(indicators, webpkg.WindowIndicator{
			Key:       "cvd_mom_elapsed",
			Label:     "CVD Mom. Elapsed",
			Value:     fmt.Sprintf("%.1fs", elapsed),
			Threshold: fmt.Sprintf("<= %.0fs", cvdMomMaxEl),
			Active:    elapsed <= cvdMomMaxEl,
		})
		indicators = append(indicators, webpkg.WindowIndicator{
			Key:       "cvd_mom_gap",
			Label:     "CVD Mom. Gap",
			Value:     fmt.Sprintf("$%.2f", absGap),
			Threshold: fmt.Sprintf("< $%.2f", cvdMomMaxGap),
			Active:    absGap < cvdMomMaxGap,
		})
		if cvdMomMinCVD > 0 {
			cvdAbs := math.Abs(snap.CVDBTC)
			indicators = append(indicators, webpkg.WindowIndicator{
				Key:       "cvd_mom_cvd",
				Label:     "CVD Mom. |CVD|",
				Value:     fmt.Sprintf("%.2f BTC", cvdAbs),
				Threshold: fmt.Sprintf(">= %.2f BTC", cvdMomMinCVD),
				Active:    cvdAbs >= cvdMomMinCVD,
			})
		}
	}

	if inPosition || buyInProgress {
		for i := range indicators {
			if indicators[i].Key == "window_elapsed" || indicators[i].Key == "gap_abs" || indicators[i].Key == "gap_velocity" || indicators[i].Key == "lead_token_price" {
				continue
			}
		}
	}

	return indicators, blocker, snap.At
}

func applyRuntimeMode(cfg *config.Config, live bool) {
	if live {
		cfg.PaperTrade = false
		return
	}
	cfg.PaperTrade = true
}

func buildDetectorParams(cfg *config.Config) strategy.Params {
	return strategy.Params{
		PairArbEnabled:                      cfg.PairArbEnabled,
		PairArbMinWindowSec:                 cfg.PairArbMinWindowSec,
		PairArbMaxWindowSec:                 cfg.PairArbMaxWindowSec,
		PairArbMinTokenPrice:                cfg.PairArbMinTokenPrice,
		PairArbMaxTokenPrice:                cfg.PairArbMaxTokenPrice,
		PairArbMinBTCGapUSD:                 cfg.PairArbMinBTCGapUSD,
		PairArbMaxBTCGapUSD:                 cfg.PairArbMaxBTCGapUSD,
		PairArbCarryEnabled:                 cfg.PairArbCarryEnabled,
		PairArbCarryEarlySec:                cfg.PairArbCarryEarlySec,
		PairArbCarryMinBTCGapUSD:            cfg.PairArbCarryMinBTCGapUSD,
		PairArbCarryMinPrevGapUSD:           cfg.PairArbCarryMinPrevGapUSD,
		PairArbMinGapVelocityUSD:            cfg.PairArbMinGapVelocityUSD,
		PairArbEarlyWindowMaxSec:            cfg.PairArbEarlyWindowMaxSec,
		PairArbEarlyMinGapUSD:               cfg.PairArbEarlyMinGapUSD,
		PairArbEarlyMinVelocityUSD:          cfg.PairArbEarlyMinVelocityUSD,
		PairArbReverseSignalEnabled:         cfg.PairArbReverseSignalEnabled,
		PairArbReverseSignalMinGapUSD:       cfg.PairArbReverseSignalMinGapUSD,
		PairArbReverseSignalMaxGapUSD:       cfg.PairArbReverseSignalMaxGapUSD,
		PairArbReverseSignalMinVelocityUSD:  cfg.PairArbReverseSignalMinVelocityUSD,
		PairArbReverseSignalLookbackSec:     cfg.PairArbReverseSignalLookbackSec,
		PairArbReverseSignalMinShrinkUSD:    cfg.PairArbReverseSignalMinShrinkUSD,
		PairArbEarlyPrevDirConfirmSec:       cfg.PairArbEarlyPrevDirConfirmSec,
		PairArbEarlyMinPrevDirGapUSD:        cfg.PairArbEarlyMinPrevDirGapUSD,
		PairArbPreOpenEnabled:               cfg.PairArbPreOpenEnabled,
		PairArbPreOpenEntrySec:              cfg.PairArbPreOpenEntrySec,
		PairArbPreOpenSrcWindowSec:          cfg.PairArbPreOpenSrcWindowSec,
		PairArbPreOpenMinGapUSD:             cfg.PairArbPreOpenMinGapUSD,
		PairArbPreOpenMinTokenPrice:         cfg.PairArbPreOpenMinTokenPrice,
		PairArbPreOpenMaxTokenPrice:         cfg.PairArbPreOpenMaxTokenPrice,
		PairArbTruePreOpenEnabled:           cfg.PairArbTruePreOpenEnabled,
		PairArbTruePreOpenLeadSec:           cfg.PairArbTruePreOpenLeadSec,
		PairArbCVDMomentumEnabled:           cfg.PairArbCVDMomentumEnabled,
		PairArbCVDMomentumMaxElapsedSec:     cfg.PairArbCVDMomentumMaxElapsedSec,
		PairArbCVDMomentumMaxGapUSD:         cfg.PairArbCVDMomentumMaxGapUSD,
		PairArbCVDMomentumMinCVDBTC:         cfg.PairArbCVDMomentumMinCVDBTC,
		PairArbCVDMomentumLockedProfitCents: cfg.PairArbCVDMomentumLockedProfitCents,
		PairArbCVDMomentumMaxLeadDriftCents: cfg.PairArbCVDMomentumMaxLeadDriftCents,
		PairArbMinLockedProfitCents:         cfg.PairArbMinLockedProfitCents,
		PairArbMaxHedgeDistanceCents:        cfg.PairArbMaxHedgeDistanceCents,
		PairArbMinCVDBTC:                    cfg.PairArbMinCVDBTC,
		PairArbMinBookImbalance:             cfg.PairArbMinBookImbalance,
		PairArbMinCoinbaseSpreadUSD:         cfg.PairArbMinCoinbaseSpreadUSD,
		PairArbMaxCoinbaseSpreadUSD:         cfg.PairArbMaxCoinbaseSpreadUSD,
		PairArbFlowMovementMode:             cfg.PairArbFlowMovementMode,
		PairArbMinCoinbaseTakerImbalance:    cfg.PairArbMinCoinbaseTakerImbalance,
		PairArbMinOIDeltaUSD:                cfg.PairArbMinOIDeltaUSD,
		PairArbMaxContraOIDeltaUSD:          cfg.PairArbMaxContraOIDeltaUSD,
		PairArbMaxAdverseYesDriftCents:      cfg.PairArbMaxAdverseYesDriftCents,
		PairArbYesDriftWindowSec:            cfg.PairArbYesDriftWindowSec,
		PairArbMinGapHoldSec:                cfg.PairArbMinGapHoldSec,
		PairArbMinBTCTickRun:                cfg.PairArbMinBTCTickRun,
		PairArbSessionTrendFilterUSD:        cfg.PairArbSessionTrendFilterUSD,
		PairArbSessionTrendBuckets:          cfg.PairArbSessionTrendBuckets,
		PairArbDirectionMode:                cfg.PairArbDirectionMode,
		PairArbElapsedSkipFromSec:           cfg.PairArbElapsedSkipFromSec,
		PairArbElapsedSkipToSec:             cfg.PairArbElapsedSkipToSec,
		PairArbMaxCVDBTC:                    cfg.PairArbMaxCVDBTC,
		PairArbPrevWinGapSkipFrom:           cfg.PairArbPrevWinGapSkipFrom,
		PairArbPrevWinGapSkipTo:             cfg.PairArbPrevWinGapSkipTo,
		PairArbVelGapRatioSkipFrom:          cfg.PairArbVelGapRatioSkipFrom,
		PairArbVelGapRatioSkipTo:            cfg.PairArbVelGapRatioSkipTo,
		PairArbMaxVelGapRatio:               cfg.PairArbMaxVelGapRatio,
		PairArbCVDRangeSkipFrom:             cfg.PairArbCVDRangeSkipFrom,
		PairArbCVDRangeSkipTo:               cfg.PairArbCVDRangeSkipTo,
		PairArbYesDriftSkipFromCents:        cfg.PairArbYesDriftSkipFromCents,
		PairArbYesDriftSkipToCents:          cfg.PairArbYesDriftSkipToCents,
		PairArbTakerImbSkipFrom:             cfg.PairArbTakerImbSkipFrom,
		PairArbTakerImbSkipTo:               cfg.PairArbTakerImbSkipTo,
		PairArbEarlyElapsedTickRunSkipSec:   cfg.PairArbEarlyElapsedTickRunSkipSec,

		// DCA+Hedge.
		DCAHedgeEnabled:            cfg.DCAHedgeEnabled,
		DCAHedgeMoveTrigger:        cfg.DCAHedgeMoveTrigger,
		DCAHedgeMaxEntrySum:        cfg.DCAHedgeMaxEntrySum,
		DCAHedgeBaseShares:         cfg.DCAHedgeBaseShares,
		DCAHedgeMaxShares:          cfg.DCAHedgeMaxShares,
		DCAHedgeDCAReversal:        cfg.DCAHedgeDCAReversal,
		DCAHedgeDCAAddShares:       cfg.DCAHedgeDCAAddShares,
		DCAHedgeUseDynamicSizing:   cfg.DCAHedgeUseDynamicSizing,
		DCAHedgeSwingTortuosityMin: cfg.DCAHedgeSwingTortuosityMin,
		DCAHedgeSwingOppRiseMin:    cfg.DCAHedgeSwingOppRiseMin,
		DCAHedgeMinElapsedSec:      cfg.DCAHedgeMinElapsedSec,
	}
}

func buildTraderConfig(cfg *config.Config) strategy.TraderConfig {
	return strategy.TraderConfig{
		TradeSizeUSD:    cfg.TradeSizeUSD,
		MaxHoldDuration: time.Duration(cfg.MaxHoldSec) * time.Second,
		PaperTrade:      cfg.PaperTrade,
		PaperStartBalance: func() float64 {
			if cfg.PaperTrade {
				return cfg.PaperStartBalance
			}
			return 0
		}(),
		JournalFile:                                        cfg.JournalFile,
		MaxSessionLossUSD:                                  cfg.MaxSessionLossUSD,
		MaxSessionProfitUSD:                                cfg.MaxSessionProfitUSD,
		MaxTradesPerSession:                                cfg.MaxTradesPerSession,
		MaxConsecutiveLosses:                               cfg.MaxConsecutiveLosses,
		PairArbTradeSizeUSD:                                cfg.PairArbTradeSizeUSD,
		PairArbMinTokenPrice:                               cfg.PairArbMinTokenPrice,
		PairArbMaxTokenPrice:                               cfg.PairArbMaxTokenPrice,
		PairArbCarryEarlySec:                               cfg.PairArbCarryEarlySec,
		PairArbCarryOppDiscountCents:                       cfg.PairArbCarryOppDiscountCents,
		PairArbMinLockedProfitCents:                        cfg.PairArbMinLockedProfitCents,
		PairArbCVDMomentumLockedProfitCents:                cfg.PairArbCVDMomentumLockedProfitCents,
		PairArbHedgeTimeoutSec:                             cfg.PairArbHedgeTimeoutSec,
		PairArbStopLossCents:                               cfg.PairArbStopLossCents,
		PairArbStopLossMinHoldSec:                          cfg.PairArbStopLossMinHoldSec,
		PairArbStopLossMinGapAgainstUSD:                    cfg.PairArbStopLossMinGapAgainstUSD,
		PairArbUnprofitableAbortGraceSec:                   cfg.PairArbUnprofitableAbortGraceSec,
		PairArbUnprofitableAbortMinGapAgainstUSD:           cfg.PairArbUnprofitableAbortMinGapAgainstUSD,
		PairArbLeadBuySlipTicks:                            cfg.PairArbLeadBuySlipTicks,
		PairArbLeadBuyTimeoutSec:                           cfg.PairArbLeadBuyTimeoutSec,
		PairArbLeadOrderType:                               polymarket.OrderType(strings.ToUpper(strings.TrimSpace(cfg.PairArbLeadOrderType))),
		PairArbDualPrePlace:                                cfg.PairArbDualPrePlace,
		PairArbHedgePreOffsetCents:                         cfg.PairArbHedgePreOffsetCents,
		PairArbMaxSignalAgeSec:                             cfg.PairArbMaxSignalAgeSec,
		PairArbMaxCLAgeSec:                                 cfg.PairArbMaxCLAgeSec,
		PairArbSellAt99:                                    cfg.PairArbSellAt99,
		PairArbContinuousImbalanceEnabled:                  cfg.PairArbContinuousImbalanceEnabled,
		PairArbContinuousImbalanceTradeSizeUSD:             cfg.PairArbContinuousImbalanceTradeSizeUSD,
		PairArbContinuousImbalanceMinSignalGapUSD:          cfg.PairArbContinuousImbalanceMinSignalGapUSD,
		PairArbContinuousImbalanceMinPriceImprovementCents: cfg.PairArbContinuousImbalanceMinPriceImprovementCents,
		PairArbContinuousImbalanceAllowMomentum:            cfg.PairArbContinuousImbalanceAllowMomentum,
		PairArbContinuousImbalanceCooldownSec:              cfg.PairArbContinuousImbalanceCooldownSec,
		PairArbContinuousImbalanceMaxAdds:                  cfg.PairArbContinuousImbalanceMaxAdds,
		PairArbContinuousImbalanceMaxUSDPerSide:            cfg.PairArbContinuousImbalanceMaxUSDPerSide,
		PairArbContinuousImbalanceMaxGapUSD:                cfg.PairArbContinuousImbalanceMaxGapUSD,
		PairArbStopCooldownSec:                             cfg.PairArbStopCooldownSec,
		PairArbScheduleMode:                                cfg.PairArbScheduleMode,
		PairArbScheduleWindowsUTC:                          cfg.PairArbScheduleWindowsUTC,

		// DCA+Hedge strategy.
		DCAHedgeTradeSizeUSD:    cfg.DCAHedgeTradeSizeUSD,
		DCAHedgeDCAReversal:     cfg.DCAHedgeDCAReversal,
		DCAHedgeDCAAddShares:    cfg.DCAHedgeDCAAddShares,
		DCAHedgeOppLegSlipTicks: cfg.DCAHedgeOppLegSlipTicks,
		UsePolymarketClaiming:   cfg.UsePolymarketClaiming,
	}
}

func buildRuntimeComponents(ctx context.Context, cfg *config.Config, ordersClient strategy.OrderExecutor, logger *zap.Logger) (*strategy.Detector, *strategy.Trader) {
	detector := strategy.NewDetector(buildDetectorParams(cfg), logger)
	if cfg.PairArbMLFilterEnabled {
		brain := strategy.NewRegimeBrain(strategy.BrainConfig{
			Enabled:           true,
			StateFile:         cfg.PairArbMLFilterStateFile,
			LabelThresholdUSD: cfg.PairArbMLFilterLabelThresholdUSD,
			MinSamplesScore:   cfg.PairArbMLFilterMinSamples,
			ScoreThreshold:    cfg.PairArbMLFilterThreshold,
		})
		detector.SetBrain(brain)
		if n := brain.TotalLabeled(); n > 0 {
			logger.Info("ml filter model loaded", zap.Int("labeled_examples", n), zap.String("state_file", cfg.PairArbMLFilterStateFile))
		} else {
			logger.Info("ml filter enabled (warming up)", zap.Int("min_samples", cfg.PairArbMLFilterMinSamples), zap.String("state_file", cfg.PairArbMLFilterStateFile))
		}
	}
	trader := strategy.NewTrader(buildTraderConfig(cfg), ordersClient, detector, logger)
	if trader.RestorePosition(ctx) {
		logger.Warn("startup: active position restored from previous run; will manage until closed")
	}
	return detector, trader
}

func main() {
	live := flag.Bool("live", false, "disable paper-trade mode and submit real orders")
	port := flag.String("port", "8084", "HTTP port for the web dashboard (default 8084)")
	flag.Parse()

	listenAddr := ":" + *port

	envFile := "all.env"

	logger := buildLogger()
	defer logger.Sync() //nolint:errcheck

	cfg, err := config.Load(envFile)
	if err != nil {
		logger.Fatal("config load", zap.Error(err))
	}

	// sigCh is declared early so both setup mode and normal mode share one channel.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	//  Setup mode
	// If required credentials are missing (first run or blank all.env), start
	// the web dashboard in setup mode. When the user saves credentials via the
	// wizard we reload config and continue into trader startup immediately
	// no manual restart required.
	var setupWebStore *webpkg.Store // non-nil when we transitioned from setup mode
	if !cfg.IsConfigured() {
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "  KalshiArb Pro  First-run setup                       ")
		fmt.Fprintln(os.Stderr, "                                                     ")
		fmt.Fprintln(os.Stderr, "  Required credentials are missing from all.env.     ")
		fmt.Fprintln(os.Stderr, "  Open the setup wizard to configure the bot:        ")
		fmt.Fprintln(os.Stderr, "                                                     ")
		fmt.Fprintf(os.Stderr, "       http://localhost:%s/wizard                  \n", *port)
		fmt.Fprintln(os.Stderr, "                                                     ")
		fmt.Fprintln(os.Stderr, "  Default password: changeme  (change on first login)")
		fmt.Fprintln(os.Stderr, "  Press Ctrl-C to exit.                              ")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "")

		setupWebStore = webpkg.NewStore()
		setupWebStore.SetSetupMode(true)
		webSrv, webErr := webpkg.NewServer(listenAddr, setupWebStore)
		if webErr != nil {
			logger.Fatal("web server init failed", zap.Error(webErr))
		}
		go func() {
			if serveErr := webSrv.Run(); serveErr != nil {
				logger.Warn("web server stopped", zap.Error(serveErr))
			}
		}()

		select {
		case <-sigCh:
			fmt.Fprintln(os.Stderr, "\nSetup mode: exiting.")
			return
		case <-setupWebStore.WaitForStart():
			// User saved credentials via the wizard  reload config and continue.
			fmt.Fprintln(os.Stderr, "\n  Credentials saved  starting the bot...")
			newCfg, reloadErr := config.Load(envFile)
			if reloadErr != nil {
				logger.Fatal("config reload failed after wizard save", zap.Error(reloadErr))
			}
			if !newCfg.IsConfigured() {
				logger.Fatal("config still incomplete after wizard save; check all.env")
			}
			cfg = newCfg
			setupWebStore.SetSetupMode(false)
		}
	}
	//  End setup mode

	// --live flag always wins; omitting it defaults to paper=true regardless of
	// what PAPER_TRADE is set to in the env file.
	applyRuntimeMode(cfg, *live)
	logRuntimeStrategyConfig(logger, cfg, envFile, *live)

	//  Splash screen
	display.Banner(cfg.PaperTrade, cfg.ProxyWalletAddress, cfg.TradeSizeUSD, cfg.PaperStartBalance)

	//  Singleton lock guard  prevents two bot instances running simultaneously
	const lockFile = ".bot.lock"
	if _, statErr := os.Stat(lockFile); statErr == nil {
		logger.Fatal("another KalshiArb Pro instance is already running (or crashed cleanly)",
			zap.String("hint", "rm "+lockFile+" to force-start"))
	}
	if werr := os.WriteFile(lockFile, []byte(time.Now().Format(time.RFC3339)), 0o600); werr != nil {
		logger.Warn("could not write lock file", zap.Error(werr))
	} else {
		defer os.Remove(lockFile)
	}

	//
	// Global clients (live across all windows)
	//
	// KalshiArbo migration build.
	// Paper mode must not require any Polymarket wallet/API credentials.
	//
	// Live execution remains deliberately blocked until the Kalshi order
	// adapter replaces the Polymarket OrdersClient completely.
	if !cfg.PaperTrade {
		logger.Info("Kalshi LIVE execution enabled through Kalshi ExecutionAdapter")
	}

	var restClient *polymarket.RESTClient // legacy compatibility only; always nil on Kalshi
	var ordersClient strategy.OrderExecutor

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Kalshi public market-data client.
	// Authentication will be added later when the live order adapter is ready.
	kalshiClient, err := kalshi.NewClient(
		"",
		cfg.KalshiAPIKeyID,
		[]byte(cfg.KalshiPrivateKeyPEM),
	)
	if err != nil {
		logger.Fatal("kalshi client init", zap.Error(err))
	}

	// Kalshi execution adapter.
	// LIVE mode remains blocked elsewhere until the migration safety checks
	// and Kalshi-specific settlement/accounting paths are complete.
	ordersClient = kalshi.NewExecutionAdapter(kalshiClient)

	// Kalshi account balance check.
	if !cfg.PaperTrade {
		bal, balErr := kalshiClient.GetBalance(ctx)
		if balErr != nil {
			logger.Fatal("Kalshi balance check failed", zap.Error(balErr))
		}

		available := bal.AvailableUSD()
		if available < cfg.TradeSizeUSD {
			logger.Fatal(
				"insufficient Kalshi balance",
				zap.Float64("available_usd", available),
				zap.Float64("trade_size_usd", cfg.TradeSizeUSD),
			)
		}

		display.Info(fmt.Sprintf("Kalshi available balance: $%.2f", available))
	} else {
		display.Info(fmt.Sprintf("paper balance: $%.2f", cfg.PaperStartBalance))
	}

	// Polymarket live authentication/prewarm removed from KalshiArbo migration build.

	detector, trader := buildRuntimeComponents(ctx, cfg, ordersClient, logger)

	if !cfg.PaperTrade {
		// Seed the dashboard/risk cache immediately from Kalshi.
		trader.RefreshLiveBalance(ctx)
	}

	// Attach a rolling-latency metrics collector so the /metrics dashboard page has data.
	botMetrics := strategy.NewBotMetrics()
	trader.SetMetrics(botMetrics)

	// Deribit WS stream  primary BTC price source, order book, and DVOL.
	// Deribit BTC-PERPETUAL book feed (replaces Bitstamp WS). No authentication required.
	globalStop := make(chan struct{})
	// Web dashboard  create the store early so stream goroutines below can update it.
	var webStore *webpkg.Store
	if setupWebStore != nil {
		webStore = setupWebStore
		display.Info(fmt.Sprintf("Dashboard (setup -> live): http://localhost:%s", *port))
	} else {
		webStore = webpkg.NewStore()
		if webSrv, webErr := webpkg.NewServer(listenAddr, webStore); webErr != nil {
			logger.Warn("web dashboard init failed", zap.Error(webErr))
		} else {
			go func() {
				display.Info(fmt.Sprintf("Dashboard: http://localhost:%s", *port))
				if serveErr := webSrv.Run(); serveErr != nil {
					logger.Warn("web dashboard stopped", zap.Error(serveErr))
				}
			}()
		}
	}
	webStore.Update(func(s *webpkg.BotState) {
		s.PaperTrade = cfg.PaperTrade
		s.WalletAddress = cfg.ProxyWalletAddress
	})
	// Expose rolling latency metrics to the web metrics page.
	webStore.SetMetricsFunc(func() []byte { return botMetrics.Snapshot() })

	// Mirror terminal output into the dashboard activity log.
	display.SetLogHook(func(level, msg string) {
		webStore.AddLog(level, msg)
	})
	display.Info("LOGGING ENABLED")
	display.Info(runtimeConfigSource(envFile))
	display.Info(activeStrategyFlagsSummary(cfg))

	// Live-feed completed trades into the dashboard and seed from disk on startup.
	// Legacy Polymarket wallet balance display removed for Kalshi.

	// Periodically refresh balance and push it to the dashboard.

	// KALSHI LIVE WEB BALANCE REFRESH
	// Keep BotState.Balance synchronized directly with the authenticated
	// Kalshi portfolio balance. The dashboard/SSE reads this store field.
	if !cfg.PaperTrade && kalshiClient != nil {
		refreshKalshiWebBalance := func() {
			bal, err := kalshiClient.GetBalance(ctx)
			if err != nil {
				logger.Warn("dashboard: Kalshi balance refresh failed", zap.Error(err))
				return
			}

			available := bal.AvailableUSD()

			webStore.Update(func(st *webpkg.BotState) {
				st.Balance = available
			})
		}

		// Seed immediately so the dashboard does not start at $0.00.
		refreshKalshiWebBalance()

		go func() {
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return

				case <-ticker.C:
					refreshKalshiWebBalance()
				}
			}
		}()
	}
	go func() {
		tick := time.NewTicker(60 * time.Second)
		defer tick.Stop()

		fetchBal := func() {
			// Legacy Polymarket wallet balance display removed for Kalshi.

			// Legacy Polymarket balance display removed.

		}

		fetchBal() // immediate update on startup
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				fetchBal()
			}
		}
	}()

	deribitStream := deribitpkg.NewStreamClient(logger)
	go deribitStream.Run(ctx)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case tick, ok := <-deribitStream.TickC:
				if !ok {
					return
				}
				detector.OnDeribitStream(tick.DVOL, tick.MarkPrice, tick.FundingRate, tick.OpenInterest)
				webStore.Update(func(s *webpkg.BotState) {
					s.BTCPrice = tick.MarkPrice
					s.FeedDeribit = true
				})
				logger.Debug("deribit stream tick",
					zap.Float64("dvol_pct", tick.DVOL),
					zap.Float64("mark", tick.MarkPrice),
					zap.Float64("index", tick.IndexPrice),
				)
			}
		}
	}()
	display.Connected("Deribit WS (BTC spot index + order book + DVOL)")

	// Binance BTCUSDT spot stream — CVD detection.
	// Provides rolling 30s Cumulative Volume Delta: net buy/sell aggressor volume.
	// Large net sellers while BTC holds elevated = distribution before reversal.
	binanceStream := binapkg.NewStreamClient(logger)
	go binanceStream.Run(ctx)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case t, ok := <-binanceStream.TradeC:
				if !ok {
					return
				}
				detector.OnBinanceTrade(t.IsSellAggressor, t.BTCQty)
				webStore.Update(func(s *webpkg.BotState) { s.FeedBinance = true })
			}
		}
	}()
	display.Connected("Binance Spot WS (BTCUSDT CVD)")

	// Coinbase Advanced Trade WS  BTC-USD spot taker flow and institutional spread.
	// The CoinbaseDeribit price spread is a leading institutional sentiment signal.
	coinbaseStream := coinbasepkg.NewStreamClient(logger)
	go coinbaseStream.Run(ctx)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case t, ok := <-coinbaseStream.TradeC:
				if !ok {
					return
				}
				detector.OnCoinbaseTrade(t.Price, t.BTCQty, t.IsSellAggressor)
				webStore.Update(func(s *webpkg.BotState) { s.FeedCoinbase = true })
			}
		}
	}()
	display.Connected("Coinbase Advanced Trade WS (BTC-USD spot price + institutional spread)")

	// Bybit V5 linear WS  BTCUSDT perpetual OI delta (second venue alongside Deribit).
	bybitStream := bybitpkg.NewStreamClient(logger)
	go bybitStream.Run(ctx)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case tick, ok := <-bybitStream.TickC:
				if !ok {
					return
				}
				if tick.OpenInterestUSD > 0 {
					detector.OnBybitOI(tick.OpenInterestUSD)
					webStore.Update(func(s *webpkg.BotState) { s.FeedBybit = true })
				}
			}
		}
	}()
	display.Connected("Bybit V5 WS (BTCUSDT OI delta)")

	// Outer restart loop � user can click Stop then Start again without restarting the process.
outer:
	for {
		// Wait for the user to click ? Start in the dashboard before entering the trading loop.
		webStore.AddLog("info", "Ready � click ? Start in the dashboard to begin trading")
		display.Info(fmt.Sprintf("Waiting for Start command from dashboard: http://localhost:%s", *port))
		select {
		case <-sigCh:
			return
		case <-webStore.WaitForBotStart():
		}

		reloadedCfg, reloadErr := config.Load(envFile)
		if reloadErr != nil {
			logger.Error("config reload before start failed", zap.Error(reloadErr))
			webStore.AddLog("error", "Config reload failed. Fix the configuration and try Start again.")
			webStore.ResetBotChannels()
			continue outer
		}
		applyRuntimeMode(reloadedCfg, *live)
		cfg = reloadedCfg
		logRuntimeStrategyConfig(logger, cfg, envFile, *live)
		display.Info(runtimeConfigSource(envFile))
		display.Info(activeStrategyFlagsSummary(cfg))
		detector, trader = buildRuntimeComponents(ctx, cfg, ordersClient, logger)
		trader.SetMetrics(botMetrics)
		initialGates := snapshotStrategyGates(detector, false, false)

		if webStore != nil {
			attachTradeCloseHandlers(trader, webStore, logger, cfg, botMetrics)
			webStore.Update(func(s *webpkg.BotState) {
				s.PaperTrade = cfg.PaperTrade
				s.WalletAddress = cfg.ProxyWalletAddress
				s.SessionPnL = 0
				s.SessionTrades = 0
				s.SessionWins = 0
				s.SessionLosses = 0
				s.LastSignal = ""
				s.LastSignalAt = time.Time{}
				s.SignalStatus = ""
				s.HasPosition = false
				s.PositionSide = ""
				s.PositionType = ""
				s.PositionBuyPrice = 0
				s.PositionShares = 0
				s.PositionUSDSpent = 0
				s.PositionUnrealizedPnL = 0
				s.PositionHeldSec = 0
				s.PairActive = false
				s.PairLeadSide = ""
				s.PairYesFilled = false
				s.PairNoFilled = false
				s.PairYesAvgPrice = 0
				s.PairNoAvgPrice = 0
				s.PairYesShares = 0
				s.PairNoShares = 0
				s.PairYesSpent = 0
				s.PairNoSpent = 0
				s.PairLockedShares = 0
				s.PairMatchedCost = 0
				s.PairExpectedProfitUSD = 0
				s.PairExpectedROIPct = 0
				s.PairResidualSide = ""
				s.PairResidualShares = 0
				s.PairHedgeSide = ""
				s.PairHedgeMaxPrice = 0
				s.PairMarkToMarket = 0
				s.StrategyGates = initialGates
			})
		}
		webStore.Update(func(s *webpkg.BotState) {
			s.Running = true
			s.StartedAt = time.Now()
		})

		// Create a combined shutdown channel: fires on OS signal OR dashboard Stop button.
		// userStop is set to true when the Stop button (not an OS signal) fired.
		var userStop bool
		shutdownCh := make(chan struct{})
		go func() {
			select {
			case <-sigCh:
			case <-webStore.WaitForBotStop():
				userStop = true
			}
			close(shutdownCh)
		}()

		if cfg.PairArbMLFilterAutoTrain {
			intervalMin := cfg.PairArbMLFilterAutoTrainIntervalMin
			if intervalMin <= 0 {
				intervalMin = 60
			}
			go func() {
				runOnce := func(trigger string) {
					if keepHours := cfg.PairArbSignalsRetentionHours; keepHours > 0 {
						if removed, err := pruneArchivedSignalsOlderThan(keepHours); err != nil {
							logger.Warn("signals prune failed", zap.Error(err), zap.Int("keep_hours", keepHours), zap.String("trigger", trigger))
						} else if removed > 0 {
							logger.Info("signals pruned", zap.Int("removed_files", removed), zap.Int("keep_hours", keepHours), zap.String("trigger", trigger))
						}
					}
					trainCtx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
					defer cancel()
					out, err := runMLFilterTraining(trainCtx, logger, cfg)
					if err != nil {
						logger.Warn("auto-train failed", zap.Error(err), zap.String("trigger", trigger))
						return
					}
					modeHint := "momentum"
					if cfg.PairArbReverseSignalEnabled {
						modeHint = "reverse"
					}
					if metrics, parseErr := webpkg.ParseMLFilterTrainingOutput(out, modeHint); parseErr == nil {
						metrics.Source = "auto:" + trigger
						webStore.SetMLFilterMetrics(metrics)
					}
					logger.Info("auto-train completed", zap.String("trigger", trigger))
				}

				runOnce("startup")
				ticker := time.NewTicker(time.Duration(intervalMin) * time.Minute)
				defer ticker.Stop()
				for {
					select {
					case <-shutdownCh:
						return
					case <-ticker.C:
						runOnce("interval")
					}
				}
			}()
		}

		windowsCompleted := 0

		//
		// Per-window session loop
		//
		for {
			select {
			case <-shutdownCh:
				display.Shutdown()
				trader.PrintSessionSummary()
				if userStop {
					webStore.Update(func(s *webpkg.BotState) {
						s.Running = false
						s.StrategyGates = nil
						s.SignalStatus = ""
					})
					webStore.ResetBotChannels()
					continue outer
				}
				close(globalStop)
				return
			default:
			}

			// Discover current Kalshi BTC 15-minute market first.
			// Kalshi open_time / close_time are the authoritative window boundaries.
			//
			// Do not use the legacy epoch-floored scheduler for the active runtime.
			//
			// IMPORTANT: during migration this runtime path is paper-only.
			// Live order submission remains disabled until the Kalshi order adapter
			// completely replaces the Polymarket OrdersClient.
			if !cfg.PaperTrade {

			}

			mkt, err := discoverKalshiBTC15M(ctx, kalshiClient, logger)
			if err != nil {
				logger.Error("kalshi market discovery failed",
					zap.Error(err),
				)

				select {
				case <-shutdownCh:
					continue
				case <-time.After(2 * time.Second):
				}
				continue
			}

			kalshiOpen, err := time.Parse(time.RFC3339, mkt.OpenDateISO)
			if err != nil {
				logger.Error("invalid Kalshi open_time",
					zap.String("ticker", mkt.ConditionID),
					zap.String("open_time", mkt.OpenDateISO),
					zap.Error(err),
				)
				time.Sleep(time.Second)
				continue
			}

			kalshiClose, err := time.Parse(time.RFC3339, mkt.EndDateISO)
			if err != nil {
				logger.Error("invalid Kalshi close_time",
					zap.String("ticker", mkt.ConditionID),
					zap.String("close_time", mkt.EndDateISO),
					zap.Error(err),
				)
				time.Sleep(time.Second)
				continue
			}

			win := market.Window{
				Start:     kalshiOpen.UTC(),
				End:       kalshiClose.UTC(),
				Slug:      mkt.ConditionID,
				StartUnix: kalshiOpen.Unix(),
				Type:      market.Market15m,
			}

			logger.Debug("new Kalshi market window",
				zap.String("ticker", mkt.ConditionID),
				zap.Time("start", win.Start),
				zap.Time("end", win.End),
				zap.Duration("remaining", time.Until(win.End)),
			)

			slug := mkt.Slug
			yesToken, noToken := extractTokens(mkt)
			if yesToken == "" {
				logger.Error("no YES contract found", zap.String("slug", slug))
				waitForWindowEnd(win, shutdownCh, logger)
				continue
			}
			if cfg.PairArbEnabled && noToken == "" {
				logger.Error("pair arb requires both YES and NO contracts; no NO contract found", zap.String("slug", slug))
				waitForWindowEnd(win, shutdownCh, logger)
				continue
			}
			logger.Debug("market ready",
				zap.String("slug", slug),
				zap.String("condition_id", mkt.ConditionID),
				zap.String("yes_token", yesToken),
				zap.String("no_token", noToken),
			)
			// Polymarket order-submit prewarm is not used by Kalshi.

			// KalshiArbo migration:
			// Keep the original trader fee interface, but do not query the
			// Polymarket fee endpoint with a Kalshi contract ID.
			// Zero is temporary for paper feed/strategy parity testing.
			feeBps := "0"
			trader.SetFeeRate(feeBps)

			windowOpenPrice := mkt.FloorStrike
			if windowOpenPrice <= 0 {
				logger.Error("Kalshi market has no valid floor strike",
					zap.String("ticker", mkt.ConditionID),
					zap.Float64("floor_strike", mkt.FloorStrike),
				)
				waitForWindowEnd(win, shutdownCh, logger)
				continue
			}

			logger.Info("using Kalshi contract target price",
				zap.String("ticker", mkt.ConditionID),
				zap.Float64("target_price", windowOpenPrice),
			)

			var confirmedOpenCh <-chan *polymarket.CryptoPriceResponse
			display.WindowHeader(slug, windowOpenPrice, yesToken, feeBps, time.Until(win.End))
			webStore.Update(func(s *webpkg.BotState) {
				s.CurrentSlug = slug
				s.OpenPrice = windowOpenPrice
				s.WindowEnd = win.End
			})

			// Tell the detector about the new window and clear the trade journal
			detector.SetWindow(windowOpenPrice, win.End)

			// Kalshi supplies the authoritative market open timestamp.
			detector.SetWindowStart(win.Start)

			trader.ResetJournal()

			// Per-window goroutines
			windowStop := make(chan struct{})
			rtdsTokens := []string{yesToken}
			if noToken != "" {
				rtdsTokens = append(rtdsTokens, noToken)
			}
			rtdsClient := kalshi.NewFeedClient(kalshiClient, rtdsTokens, logger)
			clClient := chainlink.NewClient(cfg.EthRPCURL, cfg.ChainlinkContract, logger)
			go rtdsClient.Run(windowStop)
			go clClient.Run(windowStop)

			// Chainlink on-chain RPC poller  fallback if the WS feed stops delivering.
			// Kicks in after 15 s of no WS data, polls every 5 s.
			go func() {
				select {
				case <-windowStop:
					return
				case <-time.After(15 * time.Second):
				}

				poll := func() {
					// Use RPC when the WS has never delivered data OR when the last
					// received price is more than 60 s stale (silent WS stall).
					if clClient.LatestValue() != 0 && clClient.LagSeconds() < 60 {
						return
					}
					price, err := chainlink.FetchChainlinkBTCPrice(ctx, cfg.EthRPCURL, cfg.ChainlinkContract)
					if err != nil {
						logger.Warn("chainlink RPC fallback failed", zap.Error(err))
						return
					}
					if price > 0 {
						now := time.Now()
						detector.OnChainlinkPrice(price, now)
						if err := trader.OnChainlinkPrice(ctx, price); err != nil {
							logger.Error("trader.OnChainlinkPrice (RPC fallback)", zap.Error(err))
						}
					}
				}

				ticker := time.NewTicker(5 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-windowStop:
						return
					case <-ticker.C:
						poll()
					}
				}
			}()

			// Store token IDs and condition ID on trader so flipConvictionPosition can re-enter
			// the opposing side, and SettleExpiredPosition can settle it using the Kalshi result.
			trader.HardResetForNewWindow(win.End)
			trader.SetMarketTokens(yesToken, noToken, mkt.ConditionID)
			// Run the event loop for this window
			done := runWindowLoop(ctx, win, mkt.ConditionID, win.Start, cfg, restClient, kalshiClient, detector, trader, deribitStream, rtdsClient, clClient, yesToken, noToken, confirmedOpenCh, logger, shutdownCh, webStore.WaitForBotStop(), webStore)

			// Tear down per-window goroutines
			close(windowStop)

			if done {
				trader.PrintSessionSummary()
				if userStop {
					// User clicked Stop during an active window - the run loop already force-closed.
					webStore.Update(func(s *webpkg.BotState) {
						s.Running = false
						s.StrategyGates = nil
						s.SignalStatus = ""
						s.HasPosition = false
						s.PositionSide = ""
						s.PositionType = ""
						s.PositionBuyPrice = 0
						s.PositionShares = 0
						s.PositionUSDSpent = 0
						s.PositionUnrealizedPnL = 0
						s.PositionHeldSec = 0
						s.PairActive = false
						s.PairLeadSide = ""
						s.PairYesFilled = false
						s.PairNoFilled = false
						s.PairYesAvgPrice = 0
						s.PairNoAvgPrice = 0
						s.PairYesShares = 0
						s.PairNoShares = 0
						s.PairYesSpent = 0
						s.PairNoSpent = 0
						s.PairLockedShares = 0
						s.PairMatchedCost = 0
						s.PairExpectedProfitUSD = 0
						s.PairExpectedROIPct = 0
						s.PairResidualSide = ""
						s.PairResidualShares = 0
						s.PairHedgeSide = ""
						s.PairHedgeMaxPrice = 0
						s.PairMarkToMarket = 0
					})
					webStore.ResetBotChannels()
					continue outer
				}
				close(globalStop)
				return
			}

			// fetchResolvedYes queries Kalshi directly for the authoritative
			// finalized YES/NO result for the current market.
			//
			// Kalshi may not publish Result immediately at close, so poll for
			// up to 60 seconds before using the detector snapshot as a temporary
			// fallback.
			fetchResolvedYes := func() bool {
				for attempt := 0; attempt < 20; attempt++ {
					resolvedYes, known, apiErr := kalshiClient.FetchResolution(ctx, mkt.ConditionID)
					if apiErr == nil && known {
						logger.Info("post-window: Kalshi official result received",
							zap.String("ticker", mkt.ConditionID),
							zap.Bool("resolved_yes", resolvedYes),
						)
						return resolvedYes
					}

					if apiErr != nil {
						logger.Warn("post-window: Kalshi result lookup failed",
							zap.String("ticker", mkt.ConditionID),
							zap.Int("attempt", attempt+1),
							zap.Error(apiErr),
						)
					}

					if attempt < 19 {
						select {
						case <-ctx.Done():
							break
						case <-time.After(3 * time.Second):
						}
					}
				}

				logger.Warn("post-window: Kalshi result not finalized after retries; falling back to BTC snapshot",
					zap.String("ticker", mkt.ConditionID),
				)
				btcFb, _, _, openFb, _, _ := detector.Snapshot()
				return btcFb > openFb
			}

			// Pair arb settlement MUST use Kalshi's authoritative finalized result.
			// Kalshi resolves these contracts from the CF Benchmarks BRTI averaging rule;
			// a single local BTC snapshot is not equivalent and can mis-account P&L.
			// EXCEPTION: a true-pre-open position on the NEXT market carries forward.
			if !trader.IsFlat() && trader.HasPairArbPosition() {
				if posCondID := trader.PairArbPositionConditionID(); posCondID == "" || posCondID == mkt.ConditionID {
					trader.SettleExpiredPosition(ctx, fetchResolvedYes())
				} else {
					logger.Info("true pre-open: carrying next-window pair position across window boundary",
						zap.String("position_condition_id", posCondID),
						zap.String("current_condition_id", mkt.ConditionID),
					)
				}
			}

			// Post-close grace sell: the CLOB stays open for ~45s after window close
			// and orders can still match.  If we are on the winning side we attempt to
			// sell at 0.99 to capture near-full value rather than waiting for contract
			// settlement.  Paper mode is skipped — SettleExpiredPosition handles it.
			if !trader.IsFlat() && !trader.HasPairArbPosition() {
				if err := trader.PostCloseGraceSell(ctx, fetchResolvedYes(), 45*time.Second); err != nil {
					logger.Warn("post-close grace sell error", zap.Error(err))
				}
			}

			// Settle any conviction position that survived the grace window at the resolution price.
			if !trader.IsFlat() && !trader.HasPairArbPosition() {
				trader.SettleExpiredPosition(ctx, fetchResolvedYes())
			}

			// End-of-window P&L summary
			trader.PrintWindowSummary(slug, detector.OpenPrice())

			// Record market result for the dashboard Markets tab.
			// Call WindowTrades() BEFORE the outer loop's ResetJournal() clears the slice.
			// Resolution is populated asynchronously so the next window can start
			// immediately instead of waiting up to ~60s for the API to finalize.
			{
				windowTrades := trader.WindowTrades()
				endBTC, _, _, endOpen, _, _ := detector.Snapshot()
				mktResult := webpkg.MarketResult{
					WindowEnd:     win.End,
					Slug:          slug,
					OpenPrice:     detector.OpenPrice(),
					ResolvedKnown: false,
				}
				for _, tr := range windowTrades {
					mktResult.Entered = true
					mktResult.Side = tr.Side
					mktResult.PnL += tr.PnL
				}
				mktResult.Trades = len(windowTrades)
				webStore.AddMarket(mktResult)
				go func(window market.Window, windowSlug, ticker string, fallbackBTC, fallbackOpen float64, store *webpkg.Store) {
					bgCtx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
					defer cancel()

					resolvedYes := false
					known := false

					for attempt := 0; attempt < 20; attempt++ {
						var apiErr error
						resolvedYes, known, apiErr = kalshiClient.FetchResolution(bgCtx, ticker)

						if apiErr == nil && known {
							break
						}

						if apiErr != nil {
							logger.Warn("post-window: async Kalshi result lookup failed",
								zap.String("ticker", ticker),
								zap.Int("attempt", attempt+1),
								zap.Error(apiErr),
							)
						}

						if attempt < 19 {
							select {
							case <-bgCtx.Done():
								break
							case <-time.After(3 * time.Second):
							}
						}
					}

					if !known {
						logger.Warn("post-window: async Kalshi result unavailable; falling back to BTC snapshot",
							zap.String("ticker", ticker),
						)
						resolvedYes = fallbackBTC > fallbackOpen
					}

					if store != nil {
						store.UpdateMarketResolution(window.End, windowSlug, resolvedYes, true)
					}
				}(win, slug, mkt.ConditionID, endBTC, endOpen, webStore)
			}

			// Stop-loss circuit breaker: halt the session if the configured loss cap
			// has been breached. Returns to the "waiting for Start" state.
			if trader.IsSessionHalted() {
				display.Warn(fmt.Sprintf("Session stop-loss triggered (limit $%.2f) � halting.", cfg.MaxSessionLossUSD))
				webStore.AddLog("warn", fmt.Sprintf("Stop-loss triggered: session loss exceeded $%.2f. Bot stopped.", cfg.MaxSessionLossUSD))
				trader.PrintSessionSummary()
				webStore.Update(func(s *webpkg.BotState) {
					s.Running = false
					s.StrategyGates = nil
					s.SignalStatus = ""
				})
				webStore.ResetBotChannels()
				continue outer
			}

			// Brief pause then let the outer loop pick up the next window.
			// Pass shutdownCh so Stop takes effect immediately instead of
			// waiting up to 5 minutes for the next window to start.
			waitForNextWindow(win, shutdownCh, logger)

			//  Adaptive learning: record resolved outcome, retune every 6 windows
			// Run in a background goroutine so the main loop starts the next window
			// immediately. FetchResolution polls up to 153s = 45s  blocking here would
			// eat the first 45 seconds of the next market window.
			windowsCompleted++
			_ = windowsCompleted
		}
	} // outer for
}

// init enables ANSI escape processing on Windows terminals.
func init() {
	enableWindowsANSI()
}

// runWindowLoop runs the event loop for a single 5-minute market window.
// Returns true if an OS shutdown signal was received (caller should exit).
func runWindowLoop(
	ctx context.Context,
	win market.Window,
	currentMarketID string,
	currentMarketOpen time.Time,
	cfg *config.Config,
	restClient *polymarket.RESTClient,
	kalshiClient *kalshi.Client,
	detector *strategy.Detector,
	trader *strategy.Trader,
	deribitStream *deribitpkg.StreamClient,
	rtdsClient exchangefeed.Feed,
	clClient *chainlink.Client,
	yesToken string,
	noToken string,
	confirmedOpenCh <-chan *polymarket.CryptoPriceResponse,
	logger *zap.Logger,
	shutdownCh <-chan struct{},
	userStopCh <-chan struct{},
	store *webpkg.Store,
) bool {
	// We stop accepting new entries a bit before the window ends.
	// We hard-stop the loop (force-close) at window.End.
	safeExitBuffer := time.Duration(cfg.SafeExitBufferSec) * time.Second
	hardDeadline := win.End

	evalTicker := time.NewTicker(5 * time.Second)
	statusTicker := time.NewTicker(500 * time.Millisecond)
	expireTicker := time.NewTicker(5 * time.Second)
	positionReconcileTicker := time.NewTicker(120 * time.Second)
	defer evalTicker.Stop()
	defer statusTicker.Stop()
	defer expireTicker.Stop()
	defer positionReconcileTicker.Stop()

	// True pre-open: fire a timer N seconds before win.End to start next-market discovery.
	var truePreOpenFireCh <-chan time.Time
	if cfg.PairArbEnabled && cfg.PairArbTruePreOpenEnabled {
		leadSec := cfg.PairArbTruePreOpenLeadSec
		if leadSec <= 0 {
			leadSec = 15
		}
		leadDur := time.Until(win.End) - time.Duration(leadSec)*time.Second
		if leadDur > 0 {
			truePreOpenFireCh = time.After(leadDur)
		}
		// leadDur <= 0 means we're already in the pre-open window (unlikely but safe: skip)
	}

	// Next-window preview: show next window prices greyed-out on the dashboard
	// 30 s before the current window ends (independent of the trade timer).
	var previewFireCh <-chan time.Time
	if previewLeadDur := time.Until(win.End) - 30*time.Second; previewLeadDur > 0 {
		previewFireCh = time.After(previewLeadDur)
	}

	// Clear any stale next-window preview from a previous window.
	if store != nil {
		store.Update(func(s *webpkg.BotState) {
			s.NextWindowPreviewActive = false
			s.NextYesPrice = 0
			s.NextNoPrice = 0
		})
	}

	keyCh := startKeyReader()
	showLog := true // H toggles this
	var lastPairSignalAt time.Time
	var lastPairNoEntryLogAt time.Time
	const pairSignalCooldown = 5 * time.Second
	const pairNoEntryLogCooldown = 20 * time.Second
	var pairArbStatusMu sync.RWMutex
	var pairArbNoSignalReason string
	var pairArbExecStatus string
	var pairArbExecAt time.Time
	setPairArbNoSignal := func(reason string) {
		pairArbStatusMu.Lock()
		pairArbNoSignalReason = reason
		pairArbStatusMu.Unlock()
	}
	setPairArbExecStatus := func(status string) {
		pairArbStatusMu.Lock()
		pairArbExecStatus = status
		pairArbExecAt = time.Now()
		pairArbStatusMu.Unlock()
	}
	composePairArbStatus := func(pairBlocker string) string {
		pairArbStatusMu.RLock()
		noSignal := pairArbNoSignalReason
		exec := pairArbExecStatus
		execAt := pairArbExecAt
		pairArbStatusMu.RUnlock()
		parts := make([]string, 0, 2)
		if pairBlocker != "" {
			parts = append(parts, "detector blocked: "+pairBlocker)
		} else if noSignal != "" {
			parts = append(parts, "detector: "+noSignal)
		}
		if exec != "" {
			if !execAt.IsZero() {
				parts = append(parts, fmt.Sprintf("execution: %s (%s)", exec, execAt.Format("15:04:05")))
			} else {
				parts = append(parts, "execution: "+exec)
			}
		}
		if len(parts) == 0 {
			return "pair arb idle"
		}
		return strings.Join(parts, " | ")
	}
	deriveRuntimePairArbBlocker := func(pairBlocker string) (string, *webpkg.WindowIndicator) {
		if pairBlocker != "" {
			return pairBlocker, nil
		}
		pairArbStatusMu.RLock()
		noSignal := strings.TrimSpace(pairArbNoSignalReason)
		exec := strings.TrimSpace(pairArbExecStatus)
		pairArbStatusMu.RUnlock()

		blocker := ""
		switch noSignal {
		case "waiting for flat state", "true pre-open flow in progress", "signal cooldown active", "waiting for valid YES/NO prices":
			blocker = noSignal
		}
		if blocker == "" {
			if strings.HasPrefix(exec, "signal skipped:") || strings.HasPrefix(exec, "signal error:") {
				blocker = exec
			}
		}
		if blocker == "" && noSignal == "no signal emitted on current tick" {
			blocker = "detector produced no signal (untracked gate or mode)"
		}
		if blocker == "" {
			return "", nil
		}
		ind := &webpkg.WindowIndicator{
			Key:       "runtime_guard",
			Label:     "Runtime Guard",
			Value:     blocker,
			Threshold: "must be clear",
			Active:    false,
		}
		return blocker, ind
	}
	var polyManageInFlight int32
	var expiryManageInFlight int32
	var externalReconcileInFlight int32
	// truePreOpenInFlight is set to 1 when the true pre-open goroutine is active
	// (from discovery start until order placement or abort). While set, runEval
	// skips the regular pair arb path so we never get both strategies entering
	// the same next window.
	var truePreOpenInFlight int32
	// dualEntryInFlight is set to 1 when the imbalance dual-entry goroutine has been
	// dispatched for this window. Reset to 0 only on an expected entry miss so the
	// next tick can retry. Stays 1 on success (IsFlat guards re-entry thereafter).
	var dualEntryInFlight int32

	// Pre-fetch the live USDC balance once at window start so the pair-arb
	// pre-flight check has a cached value ready before any signal fires.
	// This is the only blocking balance call; all subsequent refreshes are
	// dispatched as background goroutines from the expireTicker.
	if cfg.PairArbEnabled && !cfg.PaperTrade {
		balCtx, balCancel := context.WithTimeout(ctx, 5*time.Second)
		trader.RefreshLiveBalance(balCtx)
		balCancel()
	}

	var lastStatusAt time.Time
	printStatus := func() {
		now := time.Now()
		if now.Sub(lastStatusAt) < 500*time.Millisecond {
			return // throttle to avoid flooding
		}
		lastStatusAt = now
		btc, cl, yes, open, winRem, clAge := detector.Snapshot()
		fairProb := 0.0 // Deribit options fair-prob removed; not currently computed
		bidD, askD, rng := detector.BookDepthSnapshot()
		lag := btc - cl
		var posSide string
		var posShares, posBuy, posPnL float64
		if !trader.IsFlat() {
			posSide, posShares, posBuy, posPnL = trader.PositionSnapshot(yes)
		}
		if showLog {
			display.Status(win.Slug, btc, cl, open, yes, lag, btc-open, winRem, clAge,
				bidD, askD, rng,
				!trader.IsFlat(), posSide, posShares, posBuy, posPnL,
				trader.PaperBalance(), fairProb)
		} else {
			display.StatusCompact(btc, cl, open, yes, lag, btc-open, winRem, clAge,
				bidD, askD, rng,
				!trader.IsFlat(), posSide, posShares, posBuy, posPnL,
				trader.PaperBalance(), fairProb)
		}
	}

	syncDashboardState := func() {
		if store == nil {
			return
		}
		btcSnap, clSnap, yesSnap, openSnap, winRemSnap, _ := detector.Snapshot()
		ss := trader.SessionStats()
		posFlat := trader.IsFlat()
		posSnap := trader.DashboardPositionSnapshot(yesSnap)
		claimSnap := trader.DashboardClaimSnapshot()
		webGates := snapshotStrategyGates(detector, !posFlat, trader.BuyInProgress())
		pairIndicators, pairBlocker, pairSignalAt := snapshotPairArbIndicators(cfg, detector, !posFlat, trader.BuyInProgress())
		effectivePairBlocker := pairBlocker
		if runtimeBlocker, runtimeInd := deriveRuntimePairArbBlocker(pairBlocker); runtimeBlocker != "" {
			effectivePairBlocker = runtimeBlocker
			if runtimeInd != nil {
				pairIndicators = append(pairIndicators, *runtimeInd)
			}
		}
		store.Update(func(s *webpkg.BotState) {
			s.BTCPrice = btcSnap
			s.ChainlinkPrice = clSnap
			s.YesPrice = yesSnap

			// Kalshi YES and NO are independent executable contracts.
			// Never synthesize NO as 1-YES; use the live Kalshi NO ask.
			noSnap := 0.0
			if noToken != "" {
				noSnap = rtdsClient.LatestPrice(noToken)
			}
			s.NoPrice = noSnap

			s.OpenPrice = openSnap
			s.WindowEnd = win.End
			s.WindowRemainingS = winRemSnap
			s.WindowElapsedS = detector.WindowElapsedSec()

			// Show the actual Kalshi market ticker instead of the old
			// Polymarket-style scheduler slug.
			s.CurrentSlug = currentMarketID
			s.BTCEdgeUSD = btcSnap - openSnap
			s.SessionPnL = ss.TotalPnL
			s.SessionTrades = ss.TotalTrades
			s.SessionWins = ss.Wins
			s.SessionLosses = ss.Losses
			if cfg.PaperTrade {
				if cfg.PaperTrade {
					s.Balance = trader.PaperBalance()
				} else if kalshiClient != nil {
					if bal, err := kalshiClient.GetBalance(ctx); err == nil {
						s.Balance = bal.AvailableUSD()
					} else {
						s.Balance = trader.CurrentBalance()
					}
				} else {
					s.Balance = trader.CurrentBalance()
				}
			}
			s.HasPosition = posSnap.HasPosition
			s.PositionSide = posSnap.Side
			s.PositionType = posSnap.Type
			s.PositionBuyPrice = posSnap.BuyPrice
			s.PositionShares = posSnap.Shares
			s.PositionUSDSpent = posSnap.USDSpent
			s.PositionUnrealizedPnL = posSnap.UnrealizedPnL
			s.PositionHeldSec = posSnap.HeldSec
			s.PairActive = posSnap.PairActive
			s.PairLeadSide = posSnap.PairLeadSide
			s.PairYesFilled = posSnap.PairYesFilled
			s.PairNoFilled = posSnap.PairNoFilled
			s.PairYesAvgPrice = posSnap.PairYesAvgPrice
			s.PairNoAvgPrice = posSnap.PairNoAvgPrice
			s.PairYesShares = posSnap.PairYesShares
			s.PairNoShares = posSnap.PairNoShares
			s.PairYesSpent = posSnap.PairYesSpent
			s.PairNoSpent = posSnap.PairNoSpent
			s.PairYesWalletConfirmed = posSnap.PairYesWalletConfirmed
			s.PairNoWalletConfirmed = posSnap.PairNoWalletConfirmed
			s.PairLockedShares = posSnap.PairLockedShares
			s.PairMatchedCost = posSnap.PairMatchedCost
			s.PairExpectedProfitUSD = posSnap.PairExpectedProfit
			s.PairExpectedROIPct = posSnap.PairExpectedROIPct
			s.PairResidualSide = posSnap.PairResidualSide
			s.PairResidualShares = posSnap.PairResidualShares
			s.PairHedgeSide = posSnap.PairHedgeSide
			s.PairHedgeMaxPrice = posSnap.PairHedgeMaxPrice
			s.PairMarkToMarket = posSnap.PairMarkToMarket
			s.PairExitState = posSnap.PairExitState
			s.PairExitStateNote = posSnap.PairExitStateNote
			s.PairYesExitOrderID = posSnap.PairYesExitOrderID
			s.PairNoExitOrderID = posSnap.PairNoExitOrderID
			s.PairExitPlacedAt = posSnap.PairExitPlacedAt
			s.ClaimPendingCount = claimSnap.PendingCount
			s.ClaimFailedCount = claimSnap.FailedCount
			s.ClaimLastStatus = claimSnap.LastStatus
			s.ClaimLastMessage = claimSnap.LastMessage
			s.ClaimLastConditionID = claimSnap.LastConditionID
			s.ClaimLastSide = claimSnap.LastSide
			s.ClaimLastUpdatedAt = claimSnap.LastUpdatedAt
			s.ClaimLastAttempt = claimSnap.LastAttempt
			s.ClaimNextRetryAt = claimSnap.NextRetryAt
			s.FeedKalshi = (yesSnap > 0)
			s.FeedChainlink = (clSnap > 0)
			s.StrategyGates = webGates
			s.PairArbIndicators = pairIndicators
			s.PairArbBlocker = effectivePairBlocker
			s.PairArbSignalAt = pairSignalAt
			s.SignalStatus = composePairArbStatus(effectivePairBlocker)
		})
	}

	// runEval performs signal detection. It is called on every BTC and YES price
	// event for low-latency entry, with the 5-second evalTicker as a safety fallback.
	//
	// Detection (Evaluate*) runs synchronously  it is instant read-only state.
	// Execution (OnSignal / OnFlashSignal / OnResolutionSignal) is dispatched into
	// a goroutine so the select loop never blocks during PlaceOrder + fill polling
	// + WaitForSettledBalance (which can collectively take 1020s). The display
	// and all price event processing continue uninterrupted during a buy.
	// Re-entry is prevented by the trader's buyInProgress flag, not by blocking here.
	runEval := func() {
		select {
		case <-shutdownCh:
			return
		default:
		}
		// setLastSignal records the most recent signal name to the web dashboard.
		setLastSignal := func(name string) {
			if store != nil {
				now := time.Now()
				store.Update(func(s *webpkg.BotState) {
					s.LastSignal = name
					s.LastSignalAt = now
				})
			}
		}
		if cfg.PairArbEnabled {
			// ── Dual-entry imbalance mode ─────────────────────────────────────────────
			// When enabled, enter both YES+NO legs unconditionally on the first valid
			// price tick of each new window — no gap/velocity signal required.
			// The regular signal-based path is bypassed entirely while this mode is on.
			if cfg.PairArbContinuousImbalanceEnabled {
				if trader.BuyInProgress() || !trader.IsFlat() {
					setPairArbNoSignal("waiting for flat state")
					return
				}
				if atomic.CompareAndSwapInt32(&dualEntryInFlight, 0, 1) {
					btcP, _, yesP, openP, winRem, _ := detector.Snapshot()
					noP := rtdsClient.LatestPrice(noToken)
					if noP <= 0 && yesP > 0 && yesP < 1.0 {
						noP = math.Round((1.0-yesP)*100) / 100
					}
					if yesP > 0 && yesP < 1.0 && noP > 0 && noP < 1.0 {
						display.SignalDetected("PAIR ARB DUAL ENTRY", fmt.Sprintf("yes=%.3f no=%.3f", yesP, noP))
						setLastSignal("PAIR ARB DUAL ENTRY")
						setPairArbNoSignal("")
						setPairArbExecStatus("signal dispatched: dual entry")
						go func(y, n, op, btc, wr float64) {
							defer syncDashboardState()
							select {
							case <-shutdownCh:
								atomic.StoreInt32(&dualEntryInFlight, 0)
								return
							default:
							}
							autoSig := strategy.Signal{
								Type:            strategy.SignalPairArbLeadYes,
								PolyYesPrice:    y,
								PolyNoPrice:     n,
								OpenPrice:       op,
								BitstampPrice:   btc,
								WindowRemaining: wr,
							}
							if err := trader.OnPairArbSignal(ctx, autoSig, yesToken, noToken); err != nil {
								if isExpectedPairArbEntryMiss(err) {
									logger.Warn("dual auto-entry skipped", zap.Error(err))
									display.Warn("[PAIR ARB DUAL ENTRY] " + err.Error())
									setPairArbExecStatus("signal skipped: " + err.Error())
									atomic.StoreInt32(&dualEntryInFlight, 0) // allow retry next tick
									return
								}
								logger.Error("dual auto-entry failed", zap.Error(err))
								display.Error("[PAIR ARB DUAL ENTRY] " + err.Error())
								setPairArbExecStatus("signal error: " + err.Error())
								return
							}
							setPairArbExecStatus("signal executed")
						}(yesP, noP, openP, btcP, winRem)
					} else {
						// Prices not yet valid; reset so next tick retries.
						setPairArbNoSignal("waiting for valid YES/NO prices")
						atomic.StoreInt32(&dualEntryInFlight, 0)
					}
				}
				return // never fall through to signal-based detection in imbalance mode
			}
			// ─────────────────────────────────────────────────────────────────────────

			if trader.BuyInProgress() || !trader.IsFlat() {
				setPairArbNoSignal("waiting for flat state")
				return
			}
			// Skip regular pair arb while the true pre-open goroutine is running.
			if atomic.LoadInt32(&truePreOpenInFlight) != 0 {
				setPairArbNoSignal("true pre-open flow in progress")
				return
			}
			if !lastPairSignalAt.IsZero() && time.Since(lastPairSignalAt) < pairSignalCooldown {
				setPairArbNoSignal("signal cooldown active")
				return
			}
			// Pre-market-open carry: fires in the first few seconds of a new window when
			// the previous window closed with a strong directional bias. Token-price gate only.
			if cfg.PairArbPreOpenEnabled {
				if presig := detector.EvaluatePreOpenCarry(); presig.Type != strategy.SignalNone {
					lastPairSignalAt = time.Now()
					label := "PAIR ARB PRE-OPEN YES"
					if presig.Type == strategy.SignalPairArbPreOpenNo {
						label = "PAIR ARB PRE-OPEN NO"
					}
					display.SignalDetected(label, presig.String())
					setLastSignal(label)
					setPairArbNoSignal("")
					setPairArbExecStatus("signal dispatched: " + strings.ToLower(label))
					detector.BlockUntil(time.Now().Add(5 * time.Second))
					go func(s strategy.Signal) {
						defer syncDashboardState()
						select {
						case <-shutdownCh:
							return
						default:
						}
						if err := trader.OnPairArbSignal(ctx, s, yesToken, noToken); err != nil {
							if isExpectedPairArbEntryMiss(err) {
								logger.Warn("trader.OnPairArbSignal(PreOpen) skipped", zap.Error(err))
								display.Warn("[" + label + "] " + err.Error())
								setPairArbExecStatus("signal skipped: " + err.Error())
								return
							}
							logger.Error("trader.OnPairArbSignal(PreOpen)", zap.Error(err))
							display.Error("[" + label + "] " + err.Error())
							setPairArbExecStatus("signal error: " + err.Error())
							return
						}
						setPairArbExecStatus("signal executed")
					}(presig)
					return
				}
			}
			asig := detector.EvaluatePairArb()
			switch asig.Type {
			case strategy.SignalPairArbLeadYes:
				lastPairSignalAt = time.Now()
				display.SignalDetected("PAIR ARB LEAD YES", asig.String())
				setLastSignal("PAIR ARB LEAD YES")
				setPairArbNoSignal("")
				setPairArbExecStatus("signal dispatched: pair arb lead yes")
				detector.BlockUntil(time.Now().Add(5 * time.Second))
				go func(s strategy.Signal) {
					defer syncDashboardState()
					select {
					case <-shutdownCh:
						return
					default:
					}
					if err := trader.OnPairArbSignal(ctx, s, yesToken, noToken); err != nil {
						if isExpectedPairArbEntryMiss(err) {
							logger.Warn("trader.OnPairArbSignal(PairArbLeadYes) skipped",
								zap.Error(err),
							)
							display.Warn("[PAIR ARB LEAD YES] " + err.Error())
							setPairArbExecStatus("signal skipped: " + err.Error())
							return
						}
						logger.Error("trader.OnPairArbSignal(PairArbLeadYes)", zap.Error(err))
						display.Error("[PAIR ARB LEAD YES] " + err.Error())
						setPairArbExecStatus("signal error: " + err.Error())
						return
					}
					setPairArbExecStatus("signal executed")
				}(asig)
				return
			case strategy.SignalPairArbLeadNo:
				lastPairSignalAt = time.Now()
				display.SignalDetected("PAIR ARB LEAD NO", asig.String())
				setLastSignal("PAIR ARB LEAD NO")
				setPairArbNoSignal("")
				setPairArbExecStatus("signal dispatched: pair arb lead no")
				detector.BlockUntil(time.Now().Add(5 * time.Second))
				go func(s strategy.Signal) {
					defer syncDashboardState()
					select {
					case <-shutdownCh:
						return
					default:
					}
					if err := trader.OnPairArbSignal(ctx, s, yesToken, noToken); err != nil {
						if isExpectedPairArbEntryMiss(err) {
							logger.Warn("trader.OnPairArbSignal(PairArbLeadNo) skipped",
								zap.Error(err),
							)
							display.Warn("[PAIR ARB LEAD NO] " + err.Error())
							setPairArbExecStatus("signal skipped: " + err.Error())
							return
						}
						logger.Error("trader.OnPairArbSignal(PairArbLeadNo)", zap.Error(err))
						display.Error("[PAIR ARB LEAD NO] " + err.Error())
						setPairArbExecStatus("signal error: " + err.Error())
						return
					}
					setPairArbExecStatus("signal executed")
				}(asig)
				return
			case strategy.SignalPairArbReverseLeadYes:
				lastPairSignalAt = time.Now()
				display.SignalDetected("PAIR ARB REVERSE LEAD YES", asig.String())
				setLastSignal("PAIR ARB REVERSE LEAD YES")
				setPairArbNoSignal("")
				setPairArbExecStatus("signal dispatched: pair arb reverse lead yes")
				detector.BlockUntil(time.Now().Add(5 * time.Second))
				go func(s strategy.Signal) {
					defer syncDashboardState()
					select {
					case <-shutdownCh:
						return
					default:
					}
					if err := trader.OnPairArbSignal(ctx, s, yesToken, noToken); err != nil {
						if isExpectedPairArbEntryMiss(err) {
							logger.Warn("trader.OnPairArbSignal(PairArbReverseLeadYes) skipped",
								zap.Error(err),
							)
							display.Warn("[PAIR ARB REVERSE LEAD YES] " + err.Error())
							setPairArbExecStatus("signal skipped: " + err.Error())
							return
						}
						logger.Error("trader.OnPairArbSignal(PairArbReverseLeadYes)", zap.Error(err))
						display.Error("[PAIR ARB REVERSE LEAD YES] " + err.Error())
						setPairArbExecStatus("signal error: " + err.Error())
						return
					}
					setPairArbExecStatus("signal executed")
				}(asig)
				return
			case strategy.SignalPairArbReverseLeadNo:
				lastPairSignalAt = time.Now()
				display.SignalDetected("PAIR ARB REVERSE LEAD NO", asig.String())
				setLastSignal("PAIR ARB REVERSE LEAD NO")
				setPairArbNoSignal("")
				setPairArbExecStatus("signal dispatched: pair arb reverse lead no")
				detector.BlockUntil(time.Now().Add(5 * time.Second))
				go func(s strategy.Signal) {
					defer syncDashboardState()
					select {
					case <-shutdownCh:
						return
					default:
					}
					if err := trader.OnPairArbSignal(ctx, s, yesToken, noToken); err != nil {
						if isExpectedPairArbEntryMiss(err) {
							logger.Warn("trader.OnPairArbSignal(PairArbReverseLeadNo) skipped",
								zap.Error(err),
							)
							display.Warn("[PAIR ARB REVERSE LEAD NO] " + err.Error())
							setPairArbExecStatus("signal skipped: " + err.Error())
							return
						}
						logger.Error("trader.OnPairArbSignal(PairArbReverseLeadNo)", zap.Error(err))
						display.Error("[PAIR ARB REVERSE LEAD NO] " + err.Error())
						setPairArbExecStatus("signal error: " + err.Error())
						return
					}
					setPairArbExecStatus("signal executed")
				}(asig)
				return
			default:
				setPairArbNoSignal("no signal emitted on current tick")
				if time.Since(lastPairNoEntryLogAt) >= pairNoEntryLogCooldown {
					lastPairNoEntryLogAt = time.Now()
					snap := detector.PairArbSnapshot()
					logger.Info("pair arb: no entry snapshot",
						zap.Float64("btc", snap.BTCPrice),
						zap.Float64("open", snap.OpenPrice),
						zap.Float64("gap_usd", snap.GapUSD),
						zap.Float64("gap_velocity", snap.GapVelocity),
						zap.Float64("yes", snap.YesPrice),
						zap.Float64("no", snap.NoPrice),
						zap.Float64("window_rem_sec", snap.WindowRemSec),
						zap.Float64("cvd_btc", snap.CVDBTC),
						zap.Float64("book_imbalance", snap.BookImbalance),
						zap.Float64("coinbase_spread", snap.CoinbaseSpread),
						zap.Float64("coinbase_taker_imbalance", snap.CoinbaseTakerImb),
						zap.Float64("gap_hold_sec", snap.GapHoldSec),
						zap.Int("btc_tick_run", snap.BTCTickRun),
						zap.Float64("brain_score", snap.BrainScore),
						zap.Int("brain_labeled", snap.BrainLabeled),
						zap.Float64("min_gap_usd_cfg", cfg.PairArbMinBTCGapUSD),
						zap.Float64("max_gap_usd_cfg", cfg.PairArbMaxBTCGapUSD),
						zap.Float64("min_gap_velocity_cfg", cfg.PairArbMinGapVelocityUSD),
						zap.Float64("min_cvd_cfg", cfg.PairArbMinCVDBTC),
						zap.Float64("min_book_imb_cfg", cfg.PairArbMinBookImbalance),
						zap.Float64("min_cb_spread_cfg", cfg.PairArbMinCoinbaseSpreadUSD),
						zap.Float64("max_cb_spread_cfg", cfg.PairArbMaxCoinbaseSpreadUSD),
						zap.Float64("min_cb_taker_imb_cfg", cfg.PairArbMinCoinbaseTakerImbalance),
						zap.Int("min_gap_hold_sec_cfg", cfg.PairArbMinGapHoldSec),
						zap.Int("min_btc_tick_run_cfg", cfg.PairArbMinBTCTickRun),
						zap.Bool("ml_filter_enabled", cfg.PairArbMLFilterEnabled),
						zap.Float64("ml_filter_threshold_cfg", cfg.PairArbMLFilterThreshold),
					)
				}
			}
		}
		// DCA+Hedge signal evaluation. Runs independently of pair-arb; fires when one
		// Polymarket side has risen >= DCAHedgeMoveTrigger from its window-open baseline
		// and the YES+NO sum is still below DCAHedgeMaxEntrySum.
		if cfg.DCAHedgeEnabled && !trader.BuyInProgress() && trader.IsFlat() {
			if lastPairSignalAt.IsZero() || time.Since(lastPairSignalAt) >= pairSignalCooldown {
				dsig := detector.EvaluateDCAHedge()
				if dsig.Type == strategy.SignalDCAHedgeYes || dsig.Type == strategy.SignalDCAHedgeNo {
					lastPairSignalAt = time.Now()
					label := "DCA HEDGE YES"
					if dsig.Type == strategy.SignalDCAHedgeNo {
						label = "DCA HEDGE NO"
					}
					display.SignalDetected(label, fmt.Sprintf("moved=%s trig=%.3f shares=%.0f",
						dsig.DCAHedgeMovedSide, dsig.DCAHedgeTriggerPrice, dsig.DCAHedgeMovedShares))
					setLastSignal(label)
					setPairArbNoSignal("")
					setPairArbExecStatus("signal dispatched: " + strings.ToLower(label))
					detector.BlockUntil(time.Now().Add(5 * time.Second))
					go func(s strategy.Signal, lbl string) {
						defer syncDashboardState()
						select {
						case <-shutdownCh:
							return
						default:
						}
						if err := trader.OnDCAHedgeSignal(ctx, s, yesToken, noToken); err != nil {
							if isExpectedPairArbEntryMiss(err) {
								logger.Warn("trader.OnDCAHedgeSignal skipped", zap.Error(err))
								display.Warn("[" + lbl + "] " + err.Error())
								setPairArbExecStatus("signal skipped: " + err.Error())
								return
							}
							logger.Error("trader.OnDCAHedgeSignal", zap.Error(err))
							display.Error("[" + lbl + "] " + err.Error())
							setPairArbExecStatus("signal error: " + err.Error())
							return
						}
						setPairArbExecStatus("signal executed")
					}(dsig, label)
				}
			}
		}
	}

	for {
		select {
		case openPriceResp, ok := <-confirmedOpenCh:
			if !ok {
				confirmedOpenCh = nil
				continue
			}
			if openPriceResp != nil {
				previousOpen, updated := detector.UpdateWindowOpenPrice(openPriceResp.OpenPrice, win.End)
				if updated {
					logger.Info("confirmed window open price received",
						zap.Float64("provisional_open", previousOpen),
						zap.Float64("confirmed_open", openPriceResp.OpenPrice),
						zap.Float64("delta_usd", openPriceResp.OpenPrice-previousOpen),
						zap.Bool("completed", openPriceResp.Completed),
					)
					display.Warn(fmt.Sprintf("confirmed window open updated from $%.2f to $%.2f", previousOpen, openPriceResp.OpenPrice))
					if store != nil {
						store.Update(func(s *webpkg.BotState) {
							if s.WindowEnd.Equal(win.End) {
								s.OpenPrice = openPriceResp.OpenPrice
								s.BTCEdgeUSD = s.BTCPrice - openPriceResp.OpenPrice
							}
						})
					}
				}
			}
			confirmedOpenCh = nil

		//  Keyboard input
		case key, ok := <-keyCh:
			if !ok {
				keyCh = nil // channel closed, ignore
				continue
			}
			if key == 'h' || key == 'H' {
				showLog = !showLog
				if showLog {
					display.StatusResume()
					display.LogResumed()
				} else {
					display.LogPaused()
				}
			}

		case <-shutdownCh:
			display.Shutdown()
			logger.Debug("shutdown signal")
			// Force-close any open position at current YES price.
			// Use "user_stop" when the dashboard Stop button fired so ForceClose
			// sells both pair-arb legs at market instead of saving state for restart.
			currentYes := rtdsClient.LatestPrice(yesToken)
			if !trader.IsFlat() {
				isUserStop := false
				select {
				case <-userStopCh:
					isUserStop = true
				default:
				}
				reason := "shutdown"
				if isUserStop {
					reason = "user_stop"
				}
				if err := trader.ForceClose(ctx, currentYes, reason); err != nil {
					logger.Error("force close on shutdown", zap.Error(err))
				}
			}
			return true // caller closes globalStop

		//  Deribit BTC spot index price (primary BTC price source)
		case price, ok := <-deribitStream.SpotC:
			if !ok {
				continue
			}
			detector.OnBitstampTrade(price, time.Now())
			printStatus()
			runEval()
		//  Deribit BTC-PERPETUAL order book (spoof / depth detection)
		case dsnap, ok := <-deribitStream.BookC:
			if !ok {
				continue
			}
			bs := strategy.BookSnapshot{At: dsnap.At}
			for _, l := range dsnap.Bids {
				bs.Bids = append(bs.Bids, strategy.BookLevel{Price: l.Price, Size: l.Size})
			}
			for _, l := range dsnap.Asks {
				bs.Asks = append(bs.Asks, strategy.BookLevel{Price: l.Price, Size: l.Size})
			}
			detector.OnBitstampBook(bs)
			printStatus()
		//  Kalshi YES/NO market price feed
		case ev, ok := <-rtdsClient.Events():
			if !ok {
				continue
			}
			if ev.TokenID == yesToken {
				detector.OnPolyYesPrice(ev.Price, ev.At)
				detector.OnPolyPriceSample(ev.Price, ev.At)
				if cfg.PairArbEnabled {
					appendSignalLine(cfg.SignalEventsFile, detector.PairArbSnapshot())
				}
				currentNo := 0.0
				if noToken != "" {
					currentNo = rtdsClient.LatestPrice(noToken)
				}
				if atomic.CompareAndSwapInt32(&polyManageInFlight, 0, 1) {
					go func(yesPrice float64, noPrice float64) {
						defer atomic.StoreInt32(&polyManageInFlight, 0)
						if err := trader.OnPolyPrice(ctx, yesPrice, noPrice); err != nil {
							logger.Error("trader.OnPolyPrice", zap.Error(err))
						}
						syncDashboardState()
					}(ev.Price, currentNo)
				}
				printStatus()
				runEval()
			} else if ev.TokenID == noToken && noToken != "" {
				detector.OnPolyNoPrice(ev.Price, ev.At)
				if cfg.PairArbEnabled {
					appendSignalLine(cfg.SignalEventsFile, detector.PairArbSnapshot())
				}
				currentYes := rtdsClient.LatestPrice(yesToken)
				if currentYes <= 0 {
					currentYes = 1.0 - ev.Price
				}
				if atomic.CompareAndSwapInt32(&polyManageInFlight, 0, 1) {
					go func(yesPrice float64, noPrice float64) {
						defer atomic.StoreInt32(&polyManageInFlight, 0)
						if err := trader.OnPolyPrice(ctx, yesPrice, noPrice); err != nil {
							logger.Error("trader.OnPolyPrice (from NO tick)", zap.Error(err))
						}
						syncDashboardState()
					}(currentYes, ev.Price)
				}
				printStatus()
				runEval()
			}

		//  Chainlink WS BTC/USD price
		case cp, ok := <-clClient.PriceC:
			if !ok {
				continue
			}
			// Use ReceivedAt (WS push time ≈ now) so chainlinkAt tracks feed freshness,
			// not oracle on-chain write frequency (which can be 60–300s between updates).
			detector.OnChainlinkPrice(cp.Value, cp.ReceivedAt)
			if cfg.PairArbEnabled {
				appendSignalLine(cfg.SignalEventsFile, detector.PairArbSnapshot())
			}
			if err := trader.OnChainlinkPrice(ctx, cp.Value); err != nil {
				logger.Error("trader.OnChainlinkPrice", zap.Error(err))
			}
			printStatus()

		//  Entry signal evaluation (5-second fallback; primary path is event-driven)
		case <-evalTicker.C:
			runEval()

		//  Expiry / window-end check (every 5s)
		case <-expireTicker.C:
			// Refresh the cached live balance in the background so OnPairArbSignal
			// always has a fresh value without blocking the signal hot path.
			if cfg.PairArbEnabled && !cfg.PaperTrade {
				go func() {
					rCtx, rCancel := context.WithTimeout(ctx, 4*time.Second)
					defer rCancel()
					trader.RefreshLiveBalance(rCtx)
				}()
			}
			currentYes := rtdsClient.LatestPrice(yesToken)
			if currentYes > 0 && atomic.CompareAndSwapInt32(&expiryManageInFlight, 0, 1) {
				go func(yesPrice float64) {
					defer atomic.StoreInt32(&expiryManageInFlight, 0)
					if err := trader.CheckExpiry(ctx, yesPrice, safeExitBuffer); err != nil {
						logger.Error("trader.CheckExpiry", zap.Error(err))
						display.Error("[EXPIRY SELL] " + err.Error())
					}
					syncDashboardState()
				}(currentYes)
			}
			// Has the hard window deadline passed? (shouldn't normally get here)
			if time.Now().After(hardDeadline) {
				display.Warn("window hard deadline reached, ending session: " + win.Slug)
				if !trader.IsFlat() {
					if err := trader.ForceClose(ctx, rtdsClient.LatestPrice(yesToken), "window_expired"); err != nil {
						logger.Error("force close at window expiry", zap.Error(err))
					}
				}
				return false
			}

		// External Polymarket position reconciliation (every 120s)
		case <-positionReconcileTicker.C:
			if cfg.PaperTrade {
				continue
			}
			if atomic.CompareAndSwapInt32(&externalReconcileInFlight, 0, 1) {
				go func() {
					defer atomic.StoreInt32(&externalReconcileInFlight, 0)
					user := reconcileUserAddress(cfg)
					if user == "" {
						return
					}
					// Legacy Polymarket reconciliation timeout removed.
					currentPositions := []polymarket.UserPosition{}
					var curErr error
					if curErr != nil {
						logger.Warn("external reconcile: current positions fetch failed", zap.Error(curErr))
						return
					}
					claimsQueued := trader.ReconcileExternalPositions(currentPositions)
					if claimsQueued > 0 {
						logger.Info("external reconcile: claims queued",
							zap.Int("open_positions", len(currentPositions)),
							zap.Int("claims_queued", claimsQueued),
						)
					}
					syncDashboardState()
				}()
			}

		//  Status log and dashboard sync (every 500ms)
		case <-statusTicker.C:
			printStatus()
			syncDashboardState()

			// Session end: if the window expired, return to outer loop
			if time.Now().After(hardDeadline) {
				return false
			}

		// True pre-open: fires once, N seconds before win.End.
		// Discovers the next market, warms up its RTDS, and places a lead buy
		// on the next window's tokens using the current window's momentum direction.
		case <-previewFireCh:
			previewFireCh = nil // single-fire
			// Show the next window's YES/NO prices greyed-out on the dashboard.
			// Fresh BTC up/down markets open at ~0.50; the true pre-open goroutine
			// will overwrite these with RTDS data if it fires later.
			if store != nil {
				store.Update(func(s *webpkg.BotState) {
					s.NextWindowPreviewActive = true
					s.NextYesPrice = 0.50
					s.NextNoPrice = 0.50
				})
			}

		case <-truePreOpenFireCh:
			truePreOpenFireCh = nil // single-fire
			if !cfg.PairArbEnabled || !cfg.PairArbTruePreOpenEnabled {
				continue
			}
			if trader.BuyInProgress() {
				logger.Info("true pre-open timer fired but buy in progress, skipping")
				continue
			}
			// Note: we intentionally start the goroutine even when !trader.IsFlat().
			// Discovery + RTDS warmup run during the lead window; if a position is still
			// open it will settle at win.End (claim is background), and the goroutine
			// waits for IsFlat before placing the order.
			go func() {
				atomic.StoreInt32(&truePreOpenInFlight, 1)
				defer atomic.StoreInt32(&truePreOpenInFlight, 0)
				nextWin := win.Next()

				logger.Info("true pre-open: discovering next Kalshi market",
					zap.Time("current_win_end", win.End),
					zap.Time("next_window_end", nextWin.End),
				)

				nextKMkt, err := kalshiClient.GetNextSeriesMarket(
					ctx,
					"KXBTC15M",
					win.End,
				)
				if err != nil {
					logger.Warn(
						"true pre-open: next Kalshi market discovery failed",
						zap.Error(err),
					)
					return
				}

				nextYesToken := ""
				nextNoToken := ""

				for _, tok := range nextKMkt.Tokens {
					switch strings.ToLower(tok.Outcome) {
					case "yes":
						nextYesToken = tok.TokenID
					case "no":
						nextNoToken = tok.TokenID
					}
				}

				if nextYesToken == "" || nextNoToken == "" {
					logger.Warn(
						"true pre-open: next Kalshi market tokens not found",
						zap.String("ticker", nextKMkt.KalshiTicker),
					)
					return
				}

				logger.Info(
					"true pre-open: next Kalshi market discovered",
					zap.String("ticker", nextKMkt.KalshiTicker),
					zap.String("condition_id", nextKMkt.ConditionID),
					zap.String("yes_token", nextYesToken),
					zap.String("no_token", nextNoToken),
				)

				nextRTDSStop := make(chan struct{})
				defer close(nextRTDSStop)

				nextRTDS := kalshi.NewFeedClient(
					kalshiClient,
					[]string{nextYesToken, nextNoToken},
					logger,
				)

				go nextRTDS.Run(nextRTDSStop)

				// Wait for next market prices from RTDS. Give it the full lead duration
				// from goroutine start — the same N seconds as PAIR_ARB_TRUE_PRE_OPEN_LEAD_SEC.
				// The next market is already open and trading; RTDS delivers a tick within
				// 1-2 s once the WS handshake completes. We only fall back to 0.50 if
				// the entire warmup window elapses with no tick.
				var nextYesPrice, nextNoPrice float64
				warmDeadline := time.Now().Add(time.Duration(cfg.PairArbTruePreOpenLeadSec) * time.Second)
				for time.Now().Before(warmDeadline) {
					nextYesPrice = nextRTDS.LatestPrice(nextYesToken)
					nextNoPrice = nextRTDS.LatestPrice(nextNoToken)
					if nextYesPrice > 0 && nextNoPrice > 0 {
						break
					}
					time.Sleep(100 * time.Millisecond)
				}
				rtdsHadPrices := nextYesPrice > 0 && nextNoPrice > 0
				if !rtdsHadPrices {
					// Use 0.50 neutral fallback — order will be placed at 0.50+slippage
					// which fills immediately on the opening orderbook.
					nextYesPrice = 0.50
					nextNoPrice = 0.50
					logger.Info("true pre-open: RTDS no tick in warmup, using 0.50 fallback")
				} else {
					logger.Info("true pre-open: next market RTDS warmed",
						zap.Float64("yes", nextYesPrice),
						zap.Float64("no", nextNoPrice),
					)
				}

				// Update dashboard with actual RTDS prices (overwrites the 0.50 placeholder).
				if store != nil {
					store.Update(func(s *webpkg.BotState) {
						s.NextWindowPreviewActive = true
						s.NextYesPrice = nextYesPrice
						s.NextNoPrice = nextNoPrice
					})
				}

				// Note: we do NOT abort if win.End has passed — the order targets the NEXT
				// window's market, so placing it slightly after current window close is fine.

				// Compute carry direction from the CURRENT window's live BTC gap samples.
				dir := detector.ComputePreOpenCarryDirNow()
				if dir == 0 {
					logger.Info("true pre-open: no carry direction from current window data")
					return
				}

				// Token price range check — uses dedicated true pre-open bounds (wider than
				// the regular pre-open 0.46/0.54 because the next market's price has already
				// moved; the displacement IS the edge).
				minTok := cfg.PairArbTruePreOpenMinTokenPrice
				maxTok := cfg.PairArbTruePreOpenMaxTokenPrice
				if minTok <= 0 {
					minTok = 0.35
				}
				if maxTok <= 0 {
					maxTok = 0.65
				}
				var sigType strategy.SignalType
				var tokenPrice float64
				if dir == 1 {
					sigType = strategy.SignalPairArbTruePreOpenYes
					tokenPrice = nextYesPrice
				} else {
					sigType = strategy.SignalPairArbTruePreOpenNo
					tokenPrice = nextNoPrice
				}
				if tokenPrice < minTok || tokenPrice > maxTok {
					logger.Info("true pre-open: next market token price out of range",
						zap.Float64("price", tokenPrice),
						zap.Float64("min", minTok),
						zap.Float64("max", maxTok),
					)
					return
				}

				// If still in position (current window not yet settled), wait for the
				// position to clear. Settlement happens in the outer loop immediately
				// after runWindowLoop returns at win.End; the claim runs in the
				// background so IsFlat() becomes true within milliseconds of window end.
				if !trader.IsFlat() {
					logger.Info("true pre-open: position open, waiting for settlement before placing order")
					waitStart := time.Now()
					waitDeadline := nextWin.End
					for !trader.IsFlat() && time.Now().Before(waitDeadline) {
						time.Sleep(50 * time.Millisecond)
					}
					if !trader.IsFlat() {
						logger.Warn("true pre-open: position still open at next window end, aborting")
						return
					}
					logger.Info("true pre-open: position settled, proceeding with order",
						zap.Duration("waited", time.Since(waitStart).Round(time.Millisecond)))
				}

				// Final safety: abort if next window has also already ended (extreme delay).
				if time.Now().After(nextWin.End) {
					logger.Warn("true pre-open: even next window has ended, aborting")
					return
				}
				if trader.BuyInProgress() {
					logger.Info("true pre-open: buy in progress at signal time, skipping")
					return
				}

				btcSnap, clSnap, _, openSnap, _, _ := detector.Snapshot()
				label := "PAIR ARB TRUE PRE-OPEN YES"
				if sigType == strategy.SignalPairArbTruePreOpenNo {
					label = "PAIR ARB TRUE PRE-OPEN NO"
				}
				sig := strategy.Signal{
					Type:                sigType,
					BitstampPrice:       btcSnap,
					ChainlinkPrice:      clSnap,
					OpenPrice:           openSnap,
					PolyYesPrice:        nextYesPrice,
					PolyNoPrice:         nextNoPrice,
					WindowRemaining:     time.Until(win.End).Seconds(),
					At:                  time.Now(),
					OverrideConditionID: nextKMkt.ConditionID,
					OverrideWindowEnd:   nextWin.End,
				}
				display.SignalDetected(label, sig.String())
				if store != nil {
					now := time.Now()
					store.Update(func(s *webpkg.BotState) {
						s.LastSignal = label
						s.LastSignalAt = now
					})
				}
				if err := trader.OnPairArbSignal(ctx, sig, nextYesToken, nextNoToken); err != nil {
					if isExpectedPairArbEntryMiss(err) {
						logger.Warn("true pre-open: OnPairArbSignal skipped", zap.Error(err))
						display.Warn("[" + label + "] " + err.Error())
						return
					}
					logger.Error("true pre-open: OnPairArbSignal failed", zap.Error(err))
					display.Error("[" + label + "] " + err.Error())
					return
				}
				syncDashboardState()
			}()
		}
	}
}

// currentWindow returns the active market window for the configured market type.
func currentWindow(cfg *config.Config) market.Window {
	return market.Current(market.MarketType(cfg.MarketType))
}

func reconcileUserAddress(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	if addr := strings.TrimSpace(cfg.ProxyWalletAddress); addr != "" {
		return addr
	}
	return strings.TrimSpace(cfg.FunderAddress)
}

func provisionalWindowOpenPrice(btcLatest, chainlinkPrice float64) float64 {
	if btcLatest > 0 {
		return btcLatest
	}
	if chainlinkPrice > 0 {
		return chainlinkPrice
	}
	return 0
}

// discoverMarket finds the CLOB market for the given slug with up to 3 retries.
func discoverMarket(ctx context.Context, client *polymarket.RESTClient, slug string, logger *zap.Logger) (*polymarket.Market, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		m, err := client.GetMarketBySlug(ctx, slug)
		if err == nil {
			return m, nil
		}
		lastErr = err
		logger.Warn("market discovery attempt failed",
			zap.String("slug", slug),
			zap.Int("attempt", attempt),
			zap.Error(err),
		)
		time.Sleep(time.Duration(attempt) * 5 * time.Second)
	}
	return nil, fmt.Errorf("market discovery exhausted: %w", lastErr)
}

// extractTokens returns (yesTokenID, noTokenID) from a Market.
// For BTC up/down markets the CLOB may label tokens "Yes"/"No" or "Up"/"Down".
func extractTokens(m *polymarket.Market) (string, string) {
	var yes, no string
	for _, t := range m.Tokens {
		switch t.Outcome {
		case "Yes", "YES", "Up", "UP":
			yes = t.TokenID
		case "No", "NO", "Down", "DOWN":
			no = t.TokenID
		}
	}
	return yes, no
}

// waitForWindowEnd blocks until win.End or an OS signal is received.
func waitForWindowEnd(win market.Window, shutdownCh <-chan struct{}, logger *zap.Logger) {
	rem := time.Until(win.End)
	if rem <= 0 {
		return
	}
	display.Warn(fmt.Sprintf("waiting for window to end (%s remaining)", rem.Round(time.Second)))
	select {
	case <-time.After(rem):
	case <-shutdownCh:
	}
}

// waitForNextWindow sleeps until the next window starts (i.e. the current window.End).
// Returns early if shutdownCh is closed so Stop takes immediate effect.
func waitForNextWindow(win market.Window, shutdownCh <-chan struct{}, logger *zap.Logger) {
	rem := time.Until(win.End)
	if rem > 0 {
		display.WindowWait(win.Next().Slug, rem)
		select {
		case <-time.After(rem):
		case <-shutdownCh:
		}
	}
}

// buildLogger routes all log output to hourly files in logs/ so the ANSI terminal
// display is never interrupted by zap lines.  FATAL messages are tee'd to
// stderr so a startup failure is always visible before os.Exit fires.
func isReplayExternalReconcileRecord(rec strategy.TradeRecord) bool {
	if strings.EqualFold(strings.TrimSpace(rec.Strategy), "external_reconcile") {
		return true
	}
	reason := strings.ToLower(strings.TrimSpace(rec.Reason))
	return strings.HasPrefix(reason, "external_closed_position_reconcile:")
}

// replayTradeHistory reads trades.jsonl and seeds the web store with:
//   - the most recent completed trades (up to 50) for the trades table
//   - market windows reconstructed by grouping trades into 5-min UTC brackets
//   - the all-time P&L (sum of every trade in the file)
//
// This restores dashboard state across bot restarts.
func replayTradeHistory(journalFile string, store *webpkg.Store) {
	if journalFile == "" || store == nil {
		return
	}
	f, err := os.Open(journalFile)
	if err != nil {
		return // file doesn't exist yet on first run
	}
	defer f.Close()

	var records []strategy.TradeRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var rec strategy.TradeRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err == nil {
			records = append(records, rec)
		}
	}

	if len(records) == 0 {
		return
	}

	records = strategy.CollapseTradeRecords(records)

	// -- All-time P&L ---------------------------------------------------------
	var allTimePnL float64
	for _, rec := range records {
		if isReplayExternalReconcileRecord(rec) {
			continue
		}
		allTimePnL += rec.PnL
	}
	store.Update(func(s *webpkg.BotState) { s.AllTimePnL = allTimePnL })

	// -- Recent trades table (newest-first, capped at 50) ---------------------
	const maxReplay = 50
	start := 0
	if len(records) > maxReplay {
		start = len(records) - maxReplay
	}
	for i := start; i < len(records); i++ {
		rec := records[i]
		store.AddTrade(webpkg.TradeEntry{
			OpenedAt:  rec.OpenedAt,
			ClosedAt:  rec.ClosedAt,
			Strategy:  rec.Strategy,
			Side:      rec.Side,
			BuyPrice:  rec.BuyPrice,
			SellPrice: rec.SellPrice,
			Shares:    rec.Shares,
			USDSpent:  rec.USDSpent,
			PnL:       rec.PnL,
			Reason:    rec.Reason,
			HeldSec:   rec.HeldSec,
		})
	}

	// -- Market windows (group by 5-min UTC bracket of opened_at) -------------
	type windowKey struct {
		day  int // Unix day (truncated to day)
		slot int // five-minute slot index within the day
	}
	type windowData struct {
		windowEnd time.Time
		openPrice float64
		trades    []strategy.TradeRecord
	}

	windowMap := make(map[windowKey]*windowData)
	var windowOrder []windowKey

	for _, rec := range records {
		if isReplayExternalReconcileRecord(rec) {
			continue
		}
		t := rec.OpenedAt.UTC()
		// 5-min aligned window end = next multiple of 5 minutes
		wEnd := t.Truncate(5 * time.Minute).Add(5 * time.Minute)
		pivot := wEnd.Unix()
		key := windowKey{day: int(pivot / 86400), slot: int((pivot % 86400) / 300)}
		if _, ok := windowMap[key]; !ok {
			windowMap[key] = &windowData{
				windowEnd: wEnd,
				openPrice: rec.EntryOpenPrice,
			}
			windowOrder = append(windowOrder, key)
		}
		windowMap[key].trades = append(windowMap[key].trades, rec)
	}

	// Feed oldest-first so that AddMarket (which prepends) leaves newest at index 0.
	for _, key := range windowOrder {
		wd := windowMap[key]
		mkt := webpkg.MarketResult{
			WindowEnd:     wd.windowEnd,
			Slug:          fmt.Sprintf("BTC@%.2f", wd.openPrice),
			OpenPrice:     wd.openPrice,
			ResolvedKnown: false, // resolution data not stored in trades.jsonl
		}
		if len(wd.trades) > 0 {
			mkt.Entered = true
			mkt.Side = wd.trades[0].Side
			for _, tr := range wd.trades {
				mkt.PnL += tr.PnL
			}
			mkt.Trades = len(wd.trades)
		}
		store.AddMarket(mkt)
	}
}

// appendSignalLine serialises v as a JSON line and appends it to path.
// Failures are silently ignored so the trading loop is never blocked.
func appendSignalLine(path string, v interface{}) {
	if path == "" {
		return
	}
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	rotateSignalFileIfNeeded(path)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

func rotateSignalFileIfNeeded(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if info.Size() < signalEventsRotateMaxBytes {
		return
	}
	dir := filepath.Dir(path)
	archiveDir := filepath.Join(dir, "archive_signals")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		return
	}
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	ext := filepath.Ext(path)
	stamp := time.Now().UTC().Format("20060102_150405")
	archivePath := filepath.Join(archiveDir, fmt.Sprintf("%s_%s%s", base, stamp, ext))
	for suffix := 1; ; suffix++ {
		if _, err := os.Stat(archivePath); os.IsNotExist(err) {
			break
		}
		archivePath = filepath.Join(archiveDir, fmt.Sprintf("%s_%s_%02d%s", base, stamp, suffix, ext))
	}
	_ = os.Rename(path, archivePath)
}

func buildLogger() *zap.Logger {
	encCfg := zap.NewProductionEncoderConfig()
	encCfg.EncodeTime = zapcore.TimeEncoderOfLayout("15:04:05.000")
	encCfg.EncodeLevel = zapcore.CapitalLevelEncoder
	encCfg.CallerKey = "" // omit noisy caller path from log lines

	retentionHours := readLogRetentionHours()
	writer, err := newHourlyLogWriter("logs", "polyarb", retentionHours)
	if err != nil {
		// Cannot open log file  route fatal-only to stderr, silence the rest.
		enc := zapcore.NewConsoleEncoder(encCfg)
		fatalCore := zapcore.NewCore(enc, zapcore.AddSync(os.Stderr), zapcore.FatalLevel)
		return zap.New(fatalCore)
	}

	enc := zapcore.NewConsoleEncoder(encCfg)
	fileCore := zapcore.NewCore(enc, zapcore.AddSync(writer), zapcore.DebugLevel)
	fatalCore := zapcore.NewCore(enc, zapcore.AddSync(os.Stderr), zapcore.FatalLevel)

	return zap.New(zapcore.NewTee(fileCore, fatalCore))
}

type hourlyLogWriter struct {
	mu             sync.Mutex
	dir            string
	prefix         string
	retentionHours int
	file           *os.File
	hourStamp      string
}

func newHourlyLogWriter(dir, prefix string, retentionHours int) (*hourlyLogWriter, error) {
	if retentionHours <= 0 {
		retentionHours = 24
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	w := &hourlyLogWriter{dir: dir, prefix: prefix, retentionHours: retentionHours}
	if err := w.rotateLocked(time.Now()); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *hourlyLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.rotateLocked(time.Now()); err != nil {
		return 0, err
	}
	return w.file.Write(p)
}

func (w *hourlyLogWriter) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	return w.file.Sync()
}

func (w *hourlyLogWriter) rotateLocked(now time.Time) error {
	stamp := now.Local().Format("20060102_15")
	if w.file != nil && stamp == w.hourStamp {
		return nil
	}
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	path := filepath.Join(w.dir, fmt.Sprintf("%s_%s.log", w.prefix, stamp))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	w.file = f
	w.hourStamp = stamp
	w.pruneLocked(now)
	return nil
}

func (w *hourlyLogWriter) pruneLocked(now time.Time) {
	if w.retentionHours <= 0 {
		return
	}
	cutoff := now.Add(-time.Duration(w.retentionHours) * time.Hour)
	pattern := filepath.Join(w.dir, fmt.Sprintf("%s_*.log", w.prefix))
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return
	}
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".log")
		ts := strings.TrimPrefix(name, w.prefix+"_")
		fileTime, parseErr := time.ParseInLocation("20060102_15", ts, time.Local)
		if parseErr != nil {
			if fi, statErr := os.Stat(path); statErr == nil {
				fileTime = fi.ModTime()
			} else {
				continue
			}
		}
		if fileTime.Before(cutoff) {
			_ = os.Remove(path)
		}
	}
}

func readLogRetentionHours() int {
	value := strings.TrimSpace(os.Getenv("LOG_RETENTION_HOURS"))
	if value != "" {
		if n, err := strconv.Atoi(value); err == nil && n > 0 {
			return n
		}
	}
	data, err := os.ReadFile("config.json")
	if err != nil {
		return 24
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return 24
	}
	if n, err := strconv.Atoi(strings.TrimSpace(m["LOG_RETENTION_HOURS"])); err == nil && n > 0 {
		return n
	}
	return 24
}

// discoverKalshiBTC15M adapts the current Kalshi BTC 15-minute market into
// the market shape already consumed by the original PolyArb runtime.
func discoverKalshiBTC15M(
	ctx context.Context,
	client *kalshi.Client,
	logger *zap.Logger,
) (*polymarket.Market, error) {

	km, err := client.GetCurrentSeriesMarket(ctx, "KXBTC15M")
	if err != nil {
		return nil, err
	}

	tokens := make([]polymarket.Token, 0, len(km.Tokens))
	for _, t := range km.Tokens {
		tokens = append(tokens, polymarket.Token{
			TokenID: t.TokenID,
			Outcome: t.Outcome,
		})
	}

	logger.Info(
		"kalshi market discovered",
		zap.String("ticker", km.KalshiTicker),
		zap.String("event", km.EventTicker),
		zap.String("question", km.Question),
		zap.String("close_time", km.EndDateISO),
	)

	return &polymarket.Market{
		ConditionID:  km.ConditionID,
		QuestionID:   km.QuestionID,
		Question:     km.Question,
		Slug:         km.Slug,
		Active:       km.Active,
		Closed:       km.Closed,
		EndDateISO:   km.EndDateISO,
		OpenDateISO:  km.OpenDateISO,
		FloorStrike:  km.FloorStrike,
		Tokens:       tokens,
		MinTickSize:  km.MinTickSize,
		MinOrderSize: km.MinOrderSize,
		Description:  km.Description,
	}, nil
}
