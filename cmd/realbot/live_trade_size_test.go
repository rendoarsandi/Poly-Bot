package main

import (
	"math"
	"testing"

	"Market-bot/internal/core"
	"Market-bot/internal/paper"
)

// TestRealbotLiveTradeSizeHonorsFixedUSDCMode verifies the fix for the bug
// where the realbot strategies were ignoring the operator's runtime change
// from "percent" to "usdc" sizing mode in the TUI. The helper must read
// TradeSizingMode/TradeSizeUSDC from the live TUISettings rather than the
// stale startup-time *core.Config.
func TestRealbotLiveTradeSizeHonorsFixedUSDCMode(t *testing.T) {
	live := paper.TUISettings{
		TradeSizingMode:  core.TradeSizingModeUSDC,
		TradeSizeUSDC:    7.50,
		TradeScaleFactor: 0.05, // would yield $50 in percent mode — must be ignored in USDC mode
	}
	got := realbotLiveTradeSize(1000.0, live)
	if math.Abs(got-7.50) > 1e-9 {
		t.Fatalf("expected USDC-mode trade size to be 7.50, got %.4f", got)
	}
}

// TestRealbotLiveTradeSizePercentModeUsesScaleFactor verifies percent mode is
// unaffected by the fix.
func TestRealbotLiveTradeSizePercentModeUsesScaleFactor(t *testing.T) {
	live := paper.TUISettings{
		TradeSizingMode:  core.TradeSizingModePercent,
		TradeSizeUSDC:    7.50, // must be ignored in percent mode
		TradeScaleFactor: 0.05,
	}
	got := realbotLiveTradeSize(1000.0, live)
	if math.Abs(got-50.0) > 1e-9 {
		t.Fatalf("expected percent-mode trade size to be 50.00, got %.4f", got)
	}
}

// TestRealbotLiveTradeSizeMaxTradeSizeCapAppliesInUSDCMode confirms MaxTradeSize
// caps the budget even when fixed-USDC mode is selected.
func TestRealbotLiveTradeSizeMaxTradeSizeCapAppliesInUSDCMode(t *testing.T) {
	live := paper.TUISettings{
		TradeSizingMode: core.TradeSizingModeUSDC,
		TradeSizeUSDC:   25.0,
		MaxTradeSize:    10.0,
	}
	got := realbotLiveTradeSize(1000.0, live)
	if math.Abs(got-10.0) > 1e-9 {
		t.Fatalf("expected MaxTradeSize cap to clamp to 10.00, got %.4f", got)
	}
}
