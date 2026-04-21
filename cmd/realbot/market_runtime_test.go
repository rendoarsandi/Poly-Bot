package main

import (
	"context"
	"sync"
	"testing"

	"Market-bot/internal/core"
	"Market-bot/internal/paper"
)

func TestRealbotInitMarketRuntimeUsesSharedLadderCloseState(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, paper.NewOrderBook())
	cfg := &core.Config{}
	globalSplitInventories := make(map[string]*paper.SplitInventory)
	sharedState := newRealbotLadderCloseState()
	var splitMu sync.Mutex

	runtimeA := realbotInitMarketRuntime(context.Background(), "btc-updown-1h-1700000000", "cond-1", map[string]string{}, nil, engine, tui, cfg, globalSplitInventories, &splitMu, sharedState)
	runtimeB := realbotInitMarketRuntime(context.Background(), "btc-updown-1h-1700003600", "cond-2", map[string]string{}, nil, engine, tui, cfg, globalSplitInventories, &splitMu, sharedState)

	if runtimeA.ladderCloseState != sharedState {
		t.Fatal("expected first market runtime to reuse shared ladder-close state")
	}
	if runtimeB.ladderCloseState != sharedState {
		t.Fatal("expected second market runtime to reuse shared ladder-close state")
	}
}
