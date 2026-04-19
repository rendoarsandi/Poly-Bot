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
	realbotSetPendingLadderClose(marketID, realbotPendingLadderCloseOrder{
		Outcome: "Up",
		OrderID: "order-1",
		Price:   realbotLadderedOneHourClosePrice,
	})
	defer realbotClearPendingLadderClose(marketID)

	reason, blocked := realbotNewEntryBlockReason("eth-updown-1h-1700003600", engine, nil, paper.TUISettings{
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

func TestRealbotLadderedOneHourCloseCandidateFallsBackToAvgPriceWhenClosed(t *testing.T) {
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

	// bids and asks being nil indicates the closed market call
	candidate, ok := realbotLadderedOneHourCloseCandidate("btc-updown-1h-1700000000", []string{"Down", "Up"}, engine, nil, nil)
	if !ok {
		t.Fatal("expected one-hour ladder close candidate for closed market")
	}
	if candidate.Outcome != "Up" {
		t.Fatalf("expected Up candidate (higher avg price), got %+v", candidate)
	}
	if math.Abs(candidate.ObservedPrice-0.60) > 0.000001 {
		t.Fatalf("expected observed price to fallback to avg price 0.60, got %.6f", candidate.ObservedPrice)
	}
}
