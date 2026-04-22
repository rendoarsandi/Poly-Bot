package main

import (
	"math"
	"strings"
	"testing"

	"Market-bot/internal/paper"
)

func TestRealbotLadderedOneHourCloseCandidatePrefersHigherPricedHeldOutcome(t *testing.T) {
	engine := paper.NewEngine(100)
	if _, err := engine.BuyForMarket("btc-updown-1h-1700000000", "Up", 0.60, 5); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}
	engine.UpdateMarketBidAsk("btc-updown-1h-1700000000", "Up", 0.99, 0.999)
	engine.UpdateMarketBidAsk("btc-updown-1h-1700000000", "Down", 0.01, 0.02)

	candidate, ok := realbotLadderedOneHourCloseCandidate("btc-updown-1h-1700000000", []string{"Down", "Up"}, engine, nil, nil)
	if !ok {
		t.Fatal("expected one-hour ladder close candidate")
	}
	if candidate.Outcome != "Up" {
		t.Fatalf("expected Up candidate, got %+v", candidate)
	}
	if math.Abs(candidate.Qty-5) > 0.000001 {
		t.Fatalf("expected 5-share candidate, got %.6f", candidate.Qty)
	}
}

func TestRealbotApplyLadderedOneHourCloseFillUpdatesProfit(t *testing.T) {
	engine := paper.NewEngine(100)
	if _, err := engine.BuyForMarket("btc-updown-1h-1700000000", "Up", 0.60, 5); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}
	tui := paper.NewTUI(engine, paper.NewOrderBook())

	mirrored := realbotApplyLadderedOneHourCloseFill(engine, tui, "btc-updown-1h-1700000000", "Up", 5, realbotLadderedOneHourClosePrice, 0)
	if math.Abs(mirrored-5) > 0.000001 {
		t.Fatalf("expected mirrored sell qty 5, got %.6f", mirrored)
	}
	if realbotHasEnginePositionsForMarket(engine, "btc-updown-1h-1700000000") {
		t.Fatal("expected local position to clear after mirrored one-hour close fill")
	}

	history := tui.GetOrderHistory()
	if len(history) != 1 {
		t.Fatalf("expected one sell history entry, got %+v", history)
	}
	if history[0].Side != "SELL" || history[0].Status != "FILLED" {
		t.Fatalf("unexpected sell history entry: %+v", history[0])
	}
	if math.Abs(history[0].Profit-1.995) > 0.000001 {
		t.Fatalf("expected realized profit 1.995, got %.6f", history[0].Profit)
	}
	if math.Abs(engine.GetStats().RealizedPnL-1.995) > 0.000001 {
		t.Fatalf("expected engine realized pnl 1.995, got %.6f", engine.GetStats().RealizedPnL)
	}
}

func TestRealbotNewEntryBlockReasonUsesWaitingToSellForPendingLadderClose(t *testing.T) {
	engine := paper.NewEngine(100)
	marketID := "btc-updown-1h-1700000000"
	if _, err := engine.BuyForMarket(marketID, "Up", 0.60, 5); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}
	ladderState := newRealbotLadderCloseState()
	ladderState.set(marketID, realbotPendingLadderCloseOrder{
		Outcome: "Up",
		OrderID: "order-1",
		Price:   realbotLadderedOneHourClosePrice,
	})
	defer ladderState.clear(marketID)

	reason, blocked := realbotNewEntryBlockReason(ladderState, "eth-updown-1h-1700003600", engine, nil, paper.TUISettings{
		PaperArbMode:                       paperArbModeLaddered,
		BlockNewEntriesOnPendingRedemption: true,
	})
	if !blocked {
		t.Fatal("expected pending ladder close to block new entries")
	}
	if !strings.Contains(reason, "waiting to sell") || !strings.Contains(reason, marketID) {
		t.Fatalf("expected waiting-to-sell reason, got %q", reason)
	}
}

func TestRealbotLadderedOneHourCloseCandidateRequiresLiveNearWinningQuote(t *testing.T) {
	engine := paper.NewEngine(100)
	if _, err := engine.BuyForMarket("btc-updown-1h-1700000000", "Up", 0.60, 5); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}
	if _, err := engine.BuyForMarket("btc-updown-1h-1700000000", "Down", 0.40, 2); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	// For a closed market, quotes are cleared
	engine.UpdateMarketBidAsk("btc-updown-1h-1700000000", "Up", 0, 0)
	engine.UpdateMarketBidAsk("btc-updown-1h-1700000000", "Down", 0, 0)

	// Missing quotes means we no longer know which side is near-winning. Do not
	// create a fresh .999 sell from cost basis, because that can target a loser.
	if candidate, ok := realbotLadderedOneHourCloseCandidate("btc-updown-1h-1700000000", []string{"Down", "Up"}, engine, nil, nil); ok {
		t.Fatalf("expected no close candidate without a live near-winning quote, got %+v", candidate)
	}
}

func TestRealbotLadderCloseStateMonitorLifecycle(t *testing.T) {
	ladderState := newRealbotLadderCloseState()
	marketID := "btc-updown-1h-1700000000"
	ladderState.set(marketID, realbotPendingLadderCloseOrder{
		Outcome:      "Up",
		OrderID:      "order-1",
		Price:        realbotLadderedOneHourClosePrice,
		RequestedQty: 5,
		MirroredQty:  1,
		FeeRate:      1000,
	})

	pending, ok := ladderState.startMonitor(marketID)
	if !ok {
		t.Fatal("expected first monitor acquisition to succeed")
	}
	if !pending.MonitorActive {
		t.Fatal("expected returned pending order to be marked monitor-active")
	}
	if _, ok := ladderState.startMonitor(marketID); ok {
		t.Fatal("expected duplicate monitor acquisition to be rejected")
	}

	ladderState.stopMonitor(marketID)
	pending, ok = ladderState.get(marketID)
	if !ok {
		t.Fatal("expected pending ladder close to remain after stopping monitor")
	}
	if pending.MonitorActive {
		t.Fatal("expected stopMonitor to clear the active flag")
	}
	if math.Abs(pending.MirroredQty-1) > 0.000001 {
		t.Fatalf("expected mirrored qty to be preserved, got %.6f", pending.MirroredQty)
	}
}
