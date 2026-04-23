package main

import (
	"context"
	"time"

	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

type realbotAsyncEntryState struct {
	entryExecutionInFlight *bool
	ladderedEntries        *[]realbotLadderedEntry
	lastTrade              *time.Time
	panicBuyCooldown       *time.Time
}

func realbotApplyAsyncEntryResult(result realbotAsyncEntryResult, state *realbotAsyncEntryState) {
	if state == nil {
		return
	}
	if state.entryExecutionInFlight != nil {
		*state.entryExecutionInFlight = false
	}
	if state.ladderedEntries != nil {
		*state.ladderedEntries = realbotResolveLadderedEntry(*state.ladderedEntries, result.ladderedEntrySeq, result.ladderedEntryConfirmed)
	}
	if state.lastTrade != nil && !result.lastTradeAt.IsZero() {
		*state.lastTrade = result.lastTradeAt
	}
	if state.panicBuyCooldown != nil && !result.cooldownUntil.IsZero() && state.panicBuyCooldown.Before(result.cooldownUntil) {
		*state.panicBuyCooldown = result.cooldownUntil
	}
}

func realbotConsumeAsyncEntryResult(entryExecutionDone <-chan realbotAsyncEntryResult, state *realbotAsyncEntryState) {
	if entryExecutionDone == nil || state == nil {
		return
	}
	for {
		select {
		case result, ok := <-entryExecutionDone:
			if !ok {
				return
			}
			realbotApplyAsyncEntryResult(result, state)
		default:
			return
		}
	}
}

func realbotTradingHoursAllowed(liveCfg paper.TUISettings) bool {
	mode, ok := core.NormalizeTradingHoursMode(liveCfg.TradingHoursMode)
	if !ok {
		return false
	}
	now := time.Now()
	switch mode {
	case core.TradingHoursModeWeekdays:
		return core.IsLocalWeekday(now)
	case core.TradingHoursModeUSOpen:
		return core.IsUSMarketOpen(now)
	case core.TradingHoursModeOff:
		return true
	default:
		return core.IsTradingHourOpen(now, mode)
	}
}

func realbotTradingHoursClock(liveCfg paper.TUISettings, now time.Time) string {
	mode, ok := core.NormalizeTradingHoursMode(liveCfg.TradingHoursMode)
	if ok && mode == core.TradingHoursModeUSOpen {
		return core.USTime(now).Format("Mon 2006-01-02 15:04:05 MST")
	}
	return core.LocalTime(now).Format("Mon 2006-01-02 15:04:05 MST")
}

func realbotHandleEntryBlockNotice(marketID string, blocked bool, reason string, tui *paper.TUI, lastReason *string) {
	if lastReason == nil {
		return
	}
	if blocked {
		if reason != *lastReason {
			tui.LogEvent("[%s] ⏸️ New entries blocked: %s", marketID, reason)
			*lastReason = reason
		}
		return
	}
	*lastReason = ""
}

func realbotPauseMarketLoop(marketID, reason string, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI, makerQuotes map[string]*realbotMakerQuote, liveCfg paper.TUISettings) bool {
	pauseMakerCtx, pauseMakerCancel := context.WithTimeout(context.Background(), 5*time.Second)
	realbotCancelAllMakerQuotes(pauseMakerCtx, marketID, reason, trader, engine, tui, makerQuotes)
	pauseMakerCancel()
	time.Sleep(realbotTraderLoopInterval(liveCfg))
	return true
}
