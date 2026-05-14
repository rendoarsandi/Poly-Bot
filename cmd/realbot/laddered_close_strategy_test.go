package main

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
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

func TestRealbotLadderedOneHourCloseCandidateUsesAskWhenBidMissing(t *testing.T) {
	engine := paper.NewEngine(100)
	marketID := "btc-updown-1h-1700000000"
	if _, err := engine.BuyForMarket(marketID, "Up", 0.60, 5); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	candidate, ok := realbotLadderedOneHourCloseCandidate(
		marketID,
		[]string{"Down", "Up"},
		engine,
		nil,
		map[string]float64{"Up": realbotLadderedOneHourClosePrice},
	)
	if !ok {
		t.Fatal("expected ask-only near-winning quote to create paper .999 close candidate")
	}
	if candidate.Outcome != "Up" || math.Abs(candidate.ObservedPrice-realbotLadderedOneHourClosePrice) > 0.000001 {
		t.Fatalf("unexpected ask-only candidate: %+v", candidate)
	}
}

func TestRealbotShouldUseLadderedOneHourCloseDefaultsToSell999(t *testing.T) {
	cfg := paper.TUISettings{PaperArbMode: paperArbModeLaddered}
	if !realbotShouldUseLadderedOneHourClose("btc-updown-1h-1700000000", cfg) {
		t.Fatal("expected 1h laddered default to use .999 close")
	}
}

func TestRealbotShouldUseLadderedOneHourCloseCanWaitForResolve(t *testing.T) {
	cfg := paper.TUISettings{
		PaperArbMode:          paperArbModeLaddered,
		OneHourCryptoExitMode: core.OneHourCryptoExitWaitResolve,
	}
	if realbotShouldUseLadderedOneHourClose("btc-updown-1h-1700000000", cfg) {
		t.Fatal("expected wait-resolve mode to skip .999 close")
	}
}

func TestRealbotApplyLadderedOneHourCloseFillUpdatesProfit(t *testing.T) {
	engine := paper.NewEngine(100)
	if _, err := engine.BuyForMarket("btc-updown-1h-1700000000", "Up", 0.60, 5); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}
	tui := paper.NewTUI(engine, paper.NewOrderBook())

	mirrored := realbotApplyLadderedOneHourCloseFill(engine, tui, "btc-updown-1h-1700000000", "Up", 5, realbotLadderedOneHourClosePrice, 0, false)
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

func TestRealbotApplyLadderedOneHourCloseFillSettlesOppositeLoserInPaper(t *testing.T) {
	engine := paper.NewEngine(100)
	marketID := "btc-updown-1h-1700000000"
	if _, err := engine.BuyForMarket(marketID, "Up", 0.60, 5); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}
	if _, err := engine.BuyForMarket(marketID, "Down", 0.40, 2); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}
	tui := paper.NewTUI(engine, paper.NewOrderBook())

	mirrored := realbotApplyLadderedOneHourCloseFill(engine, tui, marketID, "Up", 5, realbotLadderedOneHourClosePrice, 0, true)
	if math.Abs(mirrored-5) > 0.000001 {
		t.Fatalf("expected mirrored sell qty 5, got %.6f", mirrored)
	}
	if realbotHasEnginePositionsForMarket(engine, marketID) {
		t.Fatalf("expected opposite loser to be cleared after confirmed paper close fill, got %+v", engine.GetPositions())
	}
	expectedPnL := (realbotLadderedOneHourClosePrice-0.60)*5 - (0.40 * 2)
	if math.Abs(engine.GetStats().RealizedPnL-expectedPnL) > 0.000001 {
		t.Fatalf("expected realized pnl %.6f after settling opposite loser, got %.6f", expectedPnL, engine.GetStats().RealizedPnL)
	}
}

func TestRealbotSubmitLadderedOneHourCloseOrderEmbeddedPaperDoesNotDoubleSell(t *testing.T) {
	engine := paper.NewEngine(100)
	marketID := "btc-updown-1h-1700000000"
	if _, err := engine.BuyForMarket(marketID, "Up", 0.60, 5); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}
	if _, err := engine.BuyForMarket(marketID, "Down", 0.40, 2); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}
	engine.UpdateMarketBidAsk(marketID, "Up", realbotLadderedOneHourClosePrice, 1.0)
	engine.UpdateMarketBidAsk(marketID, "Down", 0.001, 0.01)
	tui := paper.NewTUI(engine, paper.NewOrderBook())
	tui.AddMarket(marketID, marketID, []string{"Down", "Up"}, time.Now().Add(5*time.Second))
	trader := trading.NewEmbeddedPaperRealTrader(&core.Config{ExecutionBackend: core.ExecutionBackendPaper}, engine)
	trader.RegisterPaperToken("up-token", marketID, "Up")
	ladderState := newRealbotLadderCloseState()
	market := &api.Market{
		Slug: marketID,
		Tokens: []api.Token{
			{TokenID: "down-token", Outcome: "Down"},
			{TokenID: "up-token", Outcome: "Up"},
		},
	}

	handled := realbotSubmitLadderedOneHourCloseOrder(
		context.Background(),
		context.Background(),
		ladderState,
		marketID,
		market,
		[]string{"Down", "Up"},
		map[string]float64{"Up": realbotLadderedOneHourClosePrice, "Down": 0.001},
		map[string]float64{"Up": 1.0, "Down": 0.01},
		map[string]int{"Up": 1000},
		trader,
		engine,
		tui,
	)
	if !handled {
		t.Fatal("expected embedded paper close order to be handled")
	}
	if _, ok := ladderState.get(marketID); ok {
		t.Fatal("expected filled embedded paper close to clear pending ladder state")
	}
	if realbotHasEnginePositionsForMarket(engine, marketID) {
		t.Fatalf("expected embedded paper close to clear all market inventory, got %+v", engine.GetPositions())
	}
	if got := engine.GetSettledLoserShares(marketID, "Down"); math.Abs(got-2) > 0.000001 {
		t.Fatalf("expected opposite loser settlement of 2 shares, got %.6f", got)
	}

	expectedFee := core.PolymarketTakerFeeUSDC(5, realbotLadderedOneHourClosePrice, 1000)
	expectedNetProceeds := (5 * realbotLadderedOneHourClosePrice) - expectedFee
	expectedPnL := expectedNetProceeds - (0.60 * 5) - (0.40 * 2)
	if got := engine.GetStats().RealizedPnL; math.Abs(got-expectedPnL) > 0.000001 {
		t.Fatalf("expected realized pnl %.6f with simulated sell fee and loser settlement, got %.6f", expectedPnL, got)
	}

	history := tui.GetOrderHistory()
	if len(history) != 1 {
		t.Fatalf("expected one recorded sell, got %+v", history)
	}
	if history[0].Side != "SELL" || history[0].Status != "FILLED" || history[0].ExecutionMode != paperArbModeLaddered {
		t.Fatalf("unexpected order history entry: %+v", history[0])
	}
	if math.Abs(history[0].Cost-expectedNetProceeds) > 0.000001 {
		t.Fatalf("expected sell history net proceeds %.6f, got %.6f", expectedNetProceeds, history[0].Cost)
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

func TestRealbotLadderedOneHourCloseCandidateIgnoresHighAskLowBidLoser(t *testing.T) {
	engine := paper.NewEngine(100)
	marketID := "btc-updown-1h-1700000000"
	if _, err := engine.BuyForMarket(marketID, "Down", 0.24, 5); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	bids := map[string]float64{"Down": 0.24}
	asks := map[string]float64{"Down": 0.99}
	if candidate, ok := realbotLadderedOneHourCloseCandidate(marketID, []string{"Down", "Up"}, engine, bids, asks); ok {
		t.Fatalf("expected low-bid loser to be skipped despite high ask, got %+v", candidate)
	}
}

func TestRealbotHandleLadderedOneHourCloseWindowSettlesHeldLoserWhenOppositeNearWin(t *testing.T) {
	engine := paper.NewEngine(100)
	marketID := "bitcoin-up-or-down-april-19-2026-2am-et"
	if _, err := engine.BuyForMarket(marketID, "Down", 0.40, 5); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}
	tui := paper.NewTUI(engine, paper.NewOrderBook())
	ladderState := newRealbotLadderCloseState()

	handled := realbotHandleLadderedOneHourCloseWindow(
		context.Background(),
		ladderState,
		marketID,
		nil,
		[]string{"Down", "Up"},
		map[string]float64{"Up": 0.99},
		map[string]float64{"Up": 0.995},
		nil,
		paper.TUISettings{PaperArbMode: paperArbModeLaddered},
		5*time.Second,
		nil,
		engine,
		tui,
	)
	if !handled {
		t.Fatal("expected near-winning opposite quote to settle held loser")
	}
	if realbotHasEnginePositionsForMarket(engine, marketID) {
		t.Fatalf("expected held loser to be cleared, got %+v", engine.GetPositions())
	}
	if got := engine.GetSettledLoserShares(marketID, "Down"); math.Abs(got-5) > 0.000001 {
		t.Fatalf("expected settled loser shares 5, got %.6f", got)
	}
}

func TestRealbotHandleLadderedOneHourCloseWindowWaitResolveSkipsSell(t *testing.T) {
	engine := paper.NewEngine(100)
	marketID := "bitcoin-up-or-down-april-19-2026-2am-et"
	if _, err := engine.BuyForMarket(marketID, "Up", 0.60, 5); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}
	tui := paper.NewTUI(engine, paper.NewOrderBook())
	tui.AddMarket(marketID, marketID, []string{"Down", "Up"}, time.Now().Add(5*time.Second))
	ladderState := newRealbotLadderCloseState()

	handled := realbotHandleLadderedOneHourCloseWindow(
		context.Background(),
		ladderState,
		marketID,
		nil,
		[]string{"Down", "Up"},
		map[string]float64{"Up": 0.99},
		map[string]float64{"Up": 0.995},
		nil,
		paper.TUISettings{
			PaperArbMode:          paperArbModeLaddered,
			OneHourCryptoExitMode: core.OneHourCryptoExitWaitResolve,
		},
		5*time.Second,
		nil,
		engine,
		tui,
	)
	if !handled {
		t.Fatal("expected wait-resolve mode to take over near close")
	}
	if _, ok := ladderState.get(marketID); ok {
		t.Fatal("expected wait-resolve mode not to create a pending sell")
	}
	if !realbotHasEnginePositionsForMarket(engine, marketID) {
		t.Fatal("expected wait-resolve mode to preserve inventory")
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

func TestRealbotPaperGTCSellRestsWhenBidBelowLimit(t *testing.T) {
	engine := paper.NewEngine(100)
	marketID := "btc-updown-1h-1700000000"
	if _, err := engine.BuyForMarket(marketID, "Up", 0.60, 5); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}
	// Set bid below the 0.999 limit but above the 0.985 trigger.
	engine.UpdateMarketBidAsk(marketID, "Up", 0.99, 0.995)
	engine.UpdateMarketBidAsk(marketID, "Down", 0.005, 0.01)
	tui := paper.NewTUI(engine, paper.NewOrderBook())
	tui.AddMarket(marketID, marketID, []string{"Down", "Up"}, time.Now().Add(10*time.Second))
	trader := trading.NewEmbeddedPaperRealTrader(&core.Config{ExecutionBackend: core.ExecutionBackendPaper}, engine)
	trader.RegisterPaperToken("up-token", marketID, "Up")
	ladderState := newRealbotLadderCloseState()
	market := &api.Market{
		Slug: marketID,
		Tokens: []api.Token{
			{TokenID: "down-token", Outcome: "Down"},
			{TokenID: "up-token", Outcome: "Up"},
		},
	}

	handled := realbotSubmitLadderedOneHourCloseOrder(
		context.Background(),
		context.Background(),
		ladderState,
		marketID,
		market,
		[]string{"Down", "Up"},
		map[string]float64{"Up": 0.99, "Down": 0.005},
		map[string]float64{"Up": 0.995, "Down": 0.01},
		map[string]int{"Up": 1000},
		trader,
		engine,
		tui,
	)
	if !handled {
		t.Fatal("expected paper GTC sell to rest as pending when bid < limit")
	}
	pending, ok := ladderState.get(marketID)
	if !ok {
		t.Fatal("expected pending paper sell to be recorded in ladder state")
	}
	if pending.Outcome != "Up" {
		t.Fatalf("expected pending outcome Up, got %q", pending.Outcome)
	}
	if pending.OrderID != "" {
		t.Fatalf("expected empty OrderID for resting paper sell, got %q", pending.OrderID)
	}
	if math.Abs(pending.Price-realbotLadderedOneHourClosePrice) > 0.000001 {
		t.Fatalf("expected pending price %.3f, got %.6f", realbotLadderedOneHourClosePrice, pending.Price)
	}
	// Position should still be held
	if !realbotHasActionableEnginePositionsForMarket(engine, marketID) {
		t.Fatal("expected position to still be held while paper sell is resting")
	}
}

func TestRealbotPaperGTCSellFillsWhenBidReachesTrigger(t *testing.T) {
	engine := paper.NewEngine(100)
	marketID := "btc-updown-1h-1700000000"
	if _, err := engine.BuyForMarket(marketID, "Up", 0.60, 5); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}
	tui := paper.NewTUI(engine, paper.NewOrderBook())
	ladderState := newRealbotLadderCloseState()

	// Record a resting paper sell (simulating the state after submit with bid below limit).
	ladderState.set(marketID, realbotPendingLadderCloseOrder{
		Outcome:      "Up",
		Price:        realbotLadderedOneHourClosePrice,
		SubmittedAt:  time.Now(),
		RequestedQty: 5,
		FeeRate:      1000,
	})

	// Bid well below trigger (0.985) — poll should not fill.
	filled := realbotPollPaperLadderCloseFill(
		ladderState, marketID,
		map[string]float64{"Up": 0.97},
		engine, tui,
	)
	if filled {
		t.Fatal("expected poll not to fill when bid below trigger price")
	}
	if _, ok := ladderState.get(marketID); !ok {
		t.Fatal("expected pending sell to remain after poll with low bid")
	}

	// Bid at trigger (0.985) — paper poll should fill since the outcome is
	// clearly winning and a real GTC at 0.999 would fill before resolution.
	engine.UpdateMarketBidAsk(marketID, "Up", realbotLadderedOneHourCloseTriggerPrice, 0.995)
	filled = realbotPollPaperLadderCloseFill(
		ladderState, marketID,
		map[string]float64{"Up": realbotLadderedOneHourCloseTriggerPrice},
		engine, tui,
	)
	if !filled {
		t.Fatal("expected poll to fill when bid reaches trigger price")
	}
	if _, ok := ladderState.get(marketID); ok {
		t.Fatal("expected pending sell to be cleared after fill")
	}
	if realbotHasActionableEnginePositionsForMarket(engine, marketID) {
		t.Fatalf("expected position to be cleared after paper sell fill, got %+v", engine.GetPositions())
	}
}

func TestRealbotHandleLadderedOneHourCloseWindowPaperRestAndFill(t *testing.T) {
	engine := paper.NewEngine(100)
	marketID := "bitcoin-up-or-down-april-19-2026-2am-et"
	if _, err := engine.BuyForMarket(marketID, "Up", 0.60, 5); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}
	// Bid above trigger (0.985) but below sell limit (0.999).
	engine.UpdateMarketBidAsk(marketID, "Up", 0.99, 0.995)
	engine.UpdateMarketBidAsk(marketID, "Down", 0.005, 0.01)
	tui := paper.NewTUI(engine, paper.NewOrderBook())
	tui.AddMarket(marketID, marketID, []string{"Down", "Up"}, time.Now().Add(10*time.Second))
	trader := trading.NewEmbeddedPaperRealTrader(&core.Config{ExecutionBackend: core.ExecutionBackendPaper}, engine)
	trader.RegisterPaperToken("up-token", marketID, "Up")
	ladderState := newRealbotLadderCloseState()
	market := &api.Market{
		Slug: marketID,
		Tokens: []api.Token{
			{TokenID: "down-token", Outcome: "Down"},
			{TokenID: "up-token", Outcome: "Up"},
		},
	}

	// First call: bid is below 0.999, so the paper sell should rest.
	handled := realbotHandleLadderedOneHourCloseWindow(
		context.Background(),
		ladderState,
		marketID,
		market,
		[]string{"Down", "Up"},
		map[string]float64{"Up": 0.99, "Down": 0.005},
		map[string]float64{"Up": 0.995, "Down": 0.01},
		map[string]int{"Up": 1000},
		paper.TUISettings{PaperArbMode: paperArbModeLaddered},
		5*time.Second,
		trader,
		engine,
		tui,
	)
	if !handled {
		t.Fatal("expected first call to handle (resting paper sell)")
	}
	if _, ok := ladderState.get(marketID); !ok {
		t.Fatal("expected pending paper sell in ladder state after first call")
	}
	if !realbotHasActionableEnginePositionsForMarket(engine, marketID) {
		t.Fatal("expected position to still be held after first call")
	}

	// Second call: bid rises to 0.999, so the paper sell should fill.
	engine.UpdateMarketBidAsk(marketID, "Up", realbotLadderedOneHourClosePrice, 1.0)
	handled = realbotHandleLadderedOneHourCloseWindow(
		context.Background(),
		ladderState,
		marketID,
		market,
		[]string{"Down", "Up"},
		map[string]float64{"Up": realbotLadderedOneHourClosePrice, "Down": 0.001},
		map[string]float64{"Up": 1.0, "Down": 0.01},
		map[string]int{"Up": 1000},
		paper.TUISettings{PaperArbMode: paperArbModeLaddered},
		4*time.Second,
		trader,
		engine,
		tui,
	)
	if !handled {
		t.Fatal("expected second call to handle (fill paper sell)")
	}
	if _, ok := ladderState.get(marketID); ok {
		t.Fatal("expected pending paper sell to be cleared after fill")
	}
	if realbotHasActionableEnginePositionsForMarket(engine, marketID) {
		t.Fatalf("expected position to be cleared after paper sell fill, got %+v", engine.GetPositions())
	}
}

// TestRealbotSettleLadderedOneHourOppositeLosersPreservesBaseline verifies that
// settling losing positions in the ladder close does NOT reduce pnlBaseline,
// so the equity delta displayed to the user remains correct.
func TestRealbotSettleLadderedOneHourOppositeLosersPreservesBaseline(t *testing.T) {
	const startBal = 100.0
	engine := paper.NewEngine(startBal)
	tui := paper.NewTUI(engine, nil)
	marketID := "test-pnl-baseline"

	// Simulate buying both sides.
	winAvg := 0.84
	winQty := 27.0
	winCost := winAvg * winQty // ~22.68
	loseAvg := 0.58
	loseQty := 30.0
	loseCost := loseAvg * loseQty // ~17.40

	if _, err := engine.BuyForMarketWithFeeRate(marketID, "Up", winAvg, winQty, 0); err != nil {
		t.Fatalf("buy Up: %v", err)
	}
	if _, err := engine.BuyForMarketWithFeeRate(marketID, "Down", loseAvg, loseQty, 0); err != nil {
		t.Fatalf("buy Down: %v", err)
	}

	// Sell the winning side at $0.999 (simulates ladder close fill).
	sellPrice := 0.999
	trade, err := engine.SellForMarketWithFeeRate(marketID, "Up", sellPrice, winQty, 0)
	if err != nil {
		t.Fatalf("sell failed: %v", err)
	}

	// Record the baseline BEFORE loser settlement.
	baselineBefore := engine.GetStartingBalance()

	// Settle losing side.
	result := realbotSettleLadderedOneHourOppositeLosers(engine, tui, marketID, "Up")
	if result == nil {
		t.Fatal("expected non-nil settlement result")
	}

	// Baseline should be unchanged — the loss is captured in realizedPnL, not pnlBaseline.
	baselineAfter := engine.GetStartingBalance()
	if math.Abs(baselineAfter-baselineBefore) > 0.01 {
		t.Fatalf("pnlBaseline shifted by %.4f after settling losers; should remain stable (before=%.4f after=%.4f)",
			baselineAfter-baselineBefore, baselineBefore, baselineAfter)
	}

	// Verify the equity delta reflects the real net loss.
	equity := engine.GetEquity()
	stats := engine.GetStats()
	netChange := equity - stats.StartingBalance
	sellProfit := trade.Value - winCost
	expectedNetChange := sellProfit - loseCost // positive sell PnL minus loser cost (negative overall)
	if math.Abs(netChange-expectedNetChange) > 0.02 {
		t.Fatalf("equity delta %.4f does not match expected %.4f (sell profit %.4f - loser cost %.4f)",
			netChange, expectedNetChange, sellProfit, loseCost)
	}
}
