package main

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"Market-bot/internal/core"
	"Market-bot/internal/paper"
)

func paperbotHandleBinanceGapMarket(ctx context.Context, t *MarketTrader, liveCfg paper.TUISettings, cfg *core.Config) {
	logThrottled := func(format string, args ...interface{}) {
		if t.LastBinanceLog == nil {
			now := time.Now()
			t.LastBinanceLog = &now
			t.TUI.LogEvent(format, args...)
			return
		}
		if time.Since(*t.LastBinanceLog) >= 5*time.Second {
			t.TUI.LogEvent(format, args...)
			now := time.Now()
			t.LastBinanceLog = &now
		}
	}
	status := paper.MarketBinanceSignal{
		Enabled:   true,
		Status:    "waiting",
		Reason:    "awaiting Binance signal",
		UpdatedAt: time.Now(),
	}
	defer func() {
		if t.TUI != nil {
			status.UpdatedAt = time.Now()
			t.TUI.SetMarketBinanceSignal(t.ID, status)
		}
	}()

	mapping := paper.DirectionalOutcomes{}
	for _, outcome := range t.Outcomes {
		switch strings.ToLower(strings.TrimSpace(outcome)) {
		case "up", "yes":
			mapping.Up = outcome
		case "down", "no":
			mapping.Down = outcome
		}
	}
	if mapping.Up == "" || mapping.Down == "" {
		status.Status = "inactive"
		status.Reason = "outcomes are not Up/Down or Yes/No"
		logThrottled("[%s] ℹ️ Binance gap mode skipped: outcomes are not Up/Down or Yes/No", t.ID)
		return
	}
	if t.BinanceFeed == nil {
		status.Status = "inactive"
		status.Reason = "no Binance futures feed configured"
		logThrottled("[%s] ℹ️ Binance gap mode skipped: no Binance futures feed configured", t.ID)
		return
	}

	snap := t.BinanceFeed.Snapshot(time.Now())
	status.Symbol = snap.Symbol
	status.Price = snap.Price
	status.DeltaPercent = snap.DeltaPercent

	maxSignalAge := core.ResolveBinanceSignalMaxAge(cfg)
	signal, reason := paper.EvaluateBinanceGapSignal(time.Now(), mapping, t.TokenBids, t.TokenAsks, snap, t.PolySignalTracker, maxSignalAge)

	status.TargetOutcome = signal.TargetOutcome
	status.SignalLabel = signal.SignalLabel
	status.PolyFavorableMoveCents = signal.PolyFavorableMoveCents
	status.PolyAdverseMoveCents = signal.PolyAdverseMoveCents
	status.TargetSpreadCents = signal.TargetSpreadCents

	if reason != "" {
		status.Status = "waiting"
		status.Reason = reason
		logThrottled("[%s] ⏳ Binance gap mode %s", t.ID, reason)
		return
	}

	polyCatchupMax := cfg.BinanceSignalPolyMaxMoveCents
	if polyCatchupMax <= 0 {
		polyCatchupMax = paper.DefaultBinanceSignalPolyMaxMoveCents
	}
	if signal.PolyFavorableMoveCents > polyCatchupMax {
		status.Status = "blocked"
		status.Reason = fmt.Sprintf("%s already caught up %.2fc > %.2fc", signal.TargetOutcome, signal.PolyFavorableMoveCents, polyCatchupMax)
		logThrottled("[%s] ⚠️ Binance entry skipped: %s already caught up %.2fc > %.2fc", t.ID, signal.TargetOutcome, signal.PolyFavorableMoveCents, polyCatchupMax)
		return
	}

	polyAdverseMax := cfg.BinanceSignalPolyAdverseMoveCents
	if polyAdverseMax <= 0 {
		polyAdverseMax = paper.DefaultBinanceSignalPolyAdverseMoveCents
	}
	if signal.PolyAdverseMoveCents > polyAdverseMax {
		status.Status = "blocked"
		status.Reason = fmt.Sprintf("Polymarket moved against %s by %.2fc > %.2fc", signal.SignalLabel, signal.PolyAdverseMoveCents, polyAdverseMax)
		logThrottled("[%s] ⚠️ Binance entry skipped: Polymarket moved against %s by %.2fc > %.2fc", t.ID, signal.SignalLabel, signal.PolyAdverseMoveCents, polyAdverseMax)
		return
	}

	spreadMax := cfg.BinanceSignalSpreadMaxCents
	if spreadMax <= 0 {
		spreadMax = paper.DefaultBinanceSignalSpreadMaxCents
	}
	if signal.TargetSpreadCents > spreadMax {
		status.Status = "blocked"
		status.Reason = fmt.Sprintf("%s spread %.2fc > %.2fc", signal.TargetOutcome, signal.TargetSpreadCents, spreadMax)
		logThrottled("[%s] ⚠️ Binance entry skipped: %s spread %.2fc > %.2fc", t.ID, signal.TargetOutcome, signal.TargetSpreadCents, spreadMax)
		return
	}

	targetOutcome := signal.TargetOutcome
	ask := t.TokenAsks[targetOutcome]
	if ask < liveCfg.MinAskPrice || ask > liveCfg.MaxAskPrice {
		status.Ready = false
		status.Status = "blocked"
		status.Reason = fmt.Sprintf("%s ask $%.3f outside %.3f-%.3f", targetOutcome, ask, liveCfg.MinAskPrice, liveCfg.MaxAskPrice)
		logThrottled("[%s] ⚠️ Binance entry skipped: %s ask $%.3f outside configured range %.3f-%.3f", t.ID, targetOutcome, ask, liveCfg.MinAskPrice, liveCfg.MaxAskPrice)
		return
	}

	// This is a simulation, we just fire the log event that the condition passed
	status.Ready = true
	status.Status = "triggered"
	status.Reason = "Simulated Buy Triggered"
	tradeBudget := cfg.CalculateTradeSize(t.Engine.GetSizingBalance())
	buyQty := math.Max(1, math.Floor(tradeBudget/ask))

	// Prevent duplicate rapid-fire logging of triggers by checking a cooldown in paperbot
	// A simple approach is relying on actual execution preventing re-entry, but since paperbot
	// may not execute the complex cleanup loop, we just rate limit the "Triggered" message.
	logThrottled("[%s] 🚀 BINANCE SIMULATED TRIGGER: %s Move %.2f%%. Buy %.0f %s at $%.3f", t.ID, signal.SignalLabel, snap.DeltaPercent, buyQty, targetOutcome, ask)
}
