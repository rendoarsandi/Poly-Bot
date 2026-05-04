package main

import (
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/paper"
)

func TestRealbotHandlePostQuoteIterationWarmup(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, paper.NewOrderBook())

	now := time.Now()
	state := &realbotPostQuoteIterationState{
		lastReconnectTime: &now,
	}
	args := realbotPostQuoteIterationArgs{
		marketID: "test-market",
		engine:   engine,
		tui:      tui,
		market:   &api.Market{ConditionID: "test-cond"},
	}

	// Case 1: Within warmup period (should return true to skip)
	if !realbotHandlePostQuoteIteration(args, state) {
		t.Error("expected realbotHandlePostQuoteIteration to return true (skip) during warmup")
	}

	// Case 2: No warmup set (should proceed, might return false if nothing else stops it)
	state.lastReconnectTime = nil
	// We need to provide enough args so it doesn't panic when it proceeds
	args.ladderCloseState = newRealbotLadderCloseState()

	// Should NOT return true due to warmup now.
	// If it returns false, it means it proceeded past all checks.
	if realbotHandlePostQuoteIteration(args, state) {
		t.Error("expected realbotHandlePostQuoteIteration to return false (proceed) when no warmup is set")
	}

	// Case 3: After warmup duration
	past := now.Add(-wsWarmupDuration - time.Second)
	state.lastReconnectTime = &past
	if realbotHandlePostQuoteIteration(args, state) {
		t.Error("expected realbotHandlePostQuoteIteration to return false (proceed) after warmup duration")
	}
}
