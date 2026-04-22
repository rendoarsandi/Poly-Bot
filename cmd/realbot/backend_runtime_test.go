package main

import (
	"context"
	"math"
	"testing"

	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

func TestRealbotSwitchExecutionBackendAllowsLiveToPaperWithOpenEngine(t *testing.T) {
	engine := paper.NewEngine(25)
	if _, err := engine.BuyForMarket("btc-updown-15m-1776383100", "Up", 0.50, 1); err != nil {
		t.Fatalf("failed to seed open inventory: %v", err)
	}
	liveTrader := &trading.RealTrader{}
	cfg := &core.Config{
		ExecutionBackend: core.ExecutionBackendPaper,
		PaperBalance:     55.50,
	}

	state, got, err := realbotSwitchExecutionBackend(context.Background(), cfg, engine, liveTrader, nil)
	if err != nil {
		t.Fatalf("expected live-to-paper backend switch with open inventory to succeed, got %v", err)
	}
	if state == nil || !state.embeddedPaper {
		t.Fatalf("expected embedded paper backend state, got %+v", state)
	}
	if got == nil || !got.IsEmbeddedPaperMode() {
		t.Fatal("expected switched trader to be embedded paper")
	}
	if len(engine.GetPositions()) == 0 {
		t.Fatal("expected open inventory to be preserved when leaving live backend")
	}
	if math.Abs(engine.GetBalance()-55.50) > 0.000001 {
		t.Fatalf("expected paper cash to rebase to 55.50, got %.2f", engine.GetBalance())
	}
}
