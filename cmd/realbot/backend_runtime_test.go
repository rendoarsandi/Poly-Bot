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

func TestRealbotApplyRuntimeBalanceSyncBooksFlatDriftAsRealizedPnL(t *testing.T) {
	engine := paper.NewEngine(100)

	result := realbotApplyRuntimeBalanceSync(engine, nil, 99.40)

	if math.Abs(result.RealizedDelta+0.60) > 0.000001 {
		t.Fatalf("expected flat drift to be realized as -0.60, got %.4f", result.RealizedDelta)
	}
	if math.Abs(result.NeutralizedDelta) > 0.000001 {
		t.Fatalf("expected flat drift not to be neutralized, got %.4f", result.NeutralizedDelta)
	}
	if got := engine.GetStats().RealizedPnL; math.Abs(got+0.60) > 0.000001 {
		t.Fatalf("expected engine realized pnl -0.60, got %.4f", got)
	}
}

func TestRealbotApplyRuntimeBalanceSyncNeutralizesOpenInventoryDrift(t *testing.T) {
	engine := paper.NewEngine(100)
	if _, err := engine.BuyForMarket("BTC", "Up", 0.50, 10); err != nil {
		t.Fatalf("failed to seed open inventory: %v", err)
	}

	result := realbotApplyRuntimeBalanceSync(engine, nil, engine.GetBalance()-0.75)

	if math.Abs(result.NeutralizedDelta+0.75) > 0.000001 {
		t.Fatalf("expected open-inventory drift to be neutralized as -0.75, got %.4f", result.NeutralizedDelta)
	}
	if math.Abs(result.RealizedDelta) > 0.000001 {
		t.Fatalf("expected open-inventory drift not to be realized, got %.4f", result.RealizedDelta)
	}
	if got := engine.GetStats().RealizedPnL; math.Abs(got) > 0.000001 {
		t.Fatalf("expected realized pnl to remain neutral while inventory is open, got %.4f", got)
	}
}
