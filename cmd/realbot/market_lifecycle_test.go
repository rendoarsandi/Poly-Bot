package main

import (
	"context"
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/paper"
)

func TestRealbotHandleClosedMarketIgnoresActiveMarket(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	preserveWalletTruth := false

	handled := realbotHandleClosedMarket(realbotMarketClosureArgs{
		ladderCloseState: newRealbotLadderCloseState(),
		marketID:         "BTC",
		market:           &api.Market{ConditionID: "cond-1"},
		endTime:          time.Now().Add(30 * time.Second),
		tui:              tui,
		engine:           engine,
	}, &realbotMarketClosureState{
		preserveWalletTruth: &preserveWalletTruth,
	})
	if handled {
		t.Fatal("expected active market to bypass closed-market handler")
	}
	if preserveWalletTruth {
		t.Fatal("expected active market to leave preserveWalletTruth unchanged")
	}
}

func TestRealbotHandleMarketShutdownPreservesTakerCloseInventory(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	tui.InitSettings(paper.TUISettings{TakerCloseMarket: true}, nil)
	if _, err := engine.BuyForMarket("BTC", "Up", 0.71, 3); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	preserveWalletTruth := false
	handled := realbotHandleMarketShutdown(realbotMarketShutdownArgs{
		globalCtx: context.Background(),
		marketID:  "BTC",
		endTime:   time.Now().Add(time.Minute),
		engine:    engine,
		tui:       tui,
	}, &realbotMarketClosureState{
		preserveWalletTruth: &preserveWalletTruth,
	})
	if !handled {
		t.Fatal("expected shutdown handler to take over")
	}
	if !preserveWalletTruth {
		t.Fatal("expected taker-close shutdown to preserve wallet-truth inventory")
	}
}

func TestRealbotHandleClosedMarketDropsDustInsteadOfWaitingForResolution(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	tui.InitSettings(paper.TUISettings{TakerCloseMarket: true}, nil)
	if _, err := engine.BuyForMarket("BTC", "Up", 0.71, 0.009); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	preserveWalletTruth := false
	handled := realbotHandleClosedMarket(realbotMarketClosureArgs{
		ladderCloseState: newRealbotLadderCloseState(),
		marketID:         "BTC",
		market:           &api.Market{ConditionID: "cond-1"},
		endTime:          time.Now().Add(-time.Minute),
		outcomes:         []string{"Down", "Up"},
		engine:           engine,
		tui:              tui,
		refreshWalletTruth: func(time.Duration) {
		},
	}, &realbotMarketClosureState{
		preserveWalletTruth: &preserveWalletTruth,
	})
	if !handled {
		t.Fatal("expected closed market handler to take over")
	}
	if preserveWalletTruth {
		t.Fatal("expected dust-only inventory not to be preserved for redemption")
	}
	if realbotHasEnginePositionsForMarket(engine, "BTC") {
		t.Fatal("expected dust-only closed-market inventory to be cleared")
	}
}

func TestRealbotHandleClosedMarketClearsPendingOneHourLadderClose(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	tui.InitSettings(paper.TUISettings{PaperArbMode: paperArbModeLaddered}, nil)
	marketID := "bitcoin-up-or-down-april-22-2026-4pm-et"
	if _, err := engine.BuyForMarket(marketID, "Up", 0.57, 2.9); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	ladderState := newRealbotLadderCloseState()
	ladderState.set(marketID, realbotPendingLadderCloseOrder{
		Outcome:      "Up",
		OrderID:      "order-1",
		Price:        realbotLadderedOneHourClosePrice,
		SubmittedAt:  time.Now().Add(-time.Minute),
		RequestedQty: 2.9,
	})
	tui.AddMarket(marketID, marketID, []string{"Down", "Up"}, time.Now().Add(-time.Minute))
	tui.SetMarketInventoryStatus(marketID, "WAITING TO SELL")

	preserveWalletTruth := false
	handled := realbotHandleClosedMarket(realbotMarketClosureArgs{
		ladderCloseState: ladderState,
		marketID:         marketID,
		market:           &api.Market{ConditionID: "cond-1"},
		endTime:          time.Now().Add(-time.Minute),
		outcomes:         []string{"Down", "Up"},
		engine:           engine,
		tui:              tui,
		refreshWalletTruth: func(time.Duration) {
		},
	}, &realbotMarketClosureState{
		preserveWalletTruth: &preserveWalletTruth,
	})
	if !handled {
		t.Fatal("expected closed market handler to take over")
	}
	if !preserveWalletTruth {
		t.Fatal("expected actionable laddered inventory to be preserved for redemption")
	}
	if reason, ok := ladderState.reason(marketID); ok {
		t.Fatalf("expected closed market to clear pending ladder close, got %q", reason)
	}
}

func TestRealbotHandleMarketShutdownDropsDustInsteadOfPreservingIt(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	tui.InitSettings(paper.TUISettings{TakerCloseMarket: true}, nil)
	if _, err := engine.BuyForMarket("BTC", "Up", 0.71, 0.009); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	preserveWalletTruth := false
	handled := realbotHandleMarketShutdown(realbotMarketShutdownArgs{
		globalCtx: context.Background(),
		marketID:  "BTC",
		endTime:   time.Now().Add(time.Minute),
		engine:    engine,
		tui:       tui,
	}, &realbotMarketClosureState{
		preserveWalletTruth: &preserveWalletTruth,
	})
	if !handled {
		t.Fatal("expected shutdown handler to take over")
	}
	if preserveWalletTruth {
		t.Fatal("expected dust-only shutdown inventory not to be preserved")
	}
	if realbotHasEnginePositionsForMarket(engine, "BTC") {
		t.Fatal("expected dust-only shutdown inventory to be cleared")
	}
}

func TestRealbotMarketWindowHelpersSupportHumanReadableHourlySlug(t *testing.T) {
	marketID := "bitcoin-up-or-down-april-19-2026-2am-et"

	if got := realbotMarketWindowDuration(marketID); got != time.Hour {
		t.Fatalf("expected hourly window duration, got %v", got)
	}
	start, ok := realbotMarketWindowStart(marketID)
	if !ok {
		t.Fatal("expected to parse hourly human-readable market window start")
	}
	wantStart := time.Date(2026, time.April, 19, 6, 0, 0, 0, time.UTC)
	if !start.Equal(wantStart) {
		t.Fatalf("expected hourly window start %s, got %s", wantStart, start)
	}
	if got := realbotMarketSeriesKey(marketID); got != "bitcoin-up-or-down-1h" {
		t.Fatalf("expected hourly series key, got %q", got)
	}
}
