package main

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
)

func TestPaperExecutionLatencyDurations(t *testing.T) {
	base := time.Unix(1700000000, 0)
	latency := paperExecutionLatency{
		detectedAt: base,
		startedAt:  base.Add(2 * time.Millisecond),
		executedAt: base.Add(7 * time.Millisecond),
		settledAt:  base.Add(9 * time.Millisecond),
	}

	if got := latency.detectToStart(); got != 2*time.Millisecond {
		t.Fatalf("detectToStart = %v, want 2ms", got)
	}
	if got := latency.startToExecution(); got != 5*time.Millisecond {
		t.Fatalf("startToExecution = %v, want 5ms", got)
	}
	if got := latency.detectToExecution(); got != 7*time.Millisecond {
		t.Fatalf("detectToExecution = %v, want 7ms", got)
	}
	if got := latency.detectToSettlement(); got != 9*time.Millisecond {
		t.Fatalf("detectToSettlement = %v, want 9ms", got)
	}
}

func TestPaperExecutionLatencyHandlesMissingTimestamps(t *testing.T) {
	var latency paperExecutionLatency
	if got := latency.detectToStart(); got != 0 {
		t.Fatalf("detectToStart with zero timestamps = %v, want 0", got)
	}
	if got := latency.startToExecution(); got != 0 {
		t.Fatalf("startToExecution with zero timestamps = %v, want 0", got)
	}
	if got := latency.detectToExecution(); got != 0 {
		t.Fatalf("detectToExecution with zero timestamps = %v, want 0", got)
	}
	if got := latency.detectToSettlement(); got != 0 {
		t.Fatalf("detectToSettlement with zero timestamps = %v, want 0", got)
	}
}

func TestNormalizePaperArbModeDefaultsToTaker(t *testing.T) {
	if got := normalizePaperArbMode(""); got != paperArbModeTaker {
		t.Fatalf("normalizePaperArbMode(empty) = %q, want %q", got, paperArbModeTaker)
	}
	if got := normalizePaperArbMode("maker"); got != paperArbModeMaker {
		t.Fatalf("normalizePaperArbMode(maker) = %q, want %q", got, paperArbModeMaker)
	}
	if got := normalizePaperArbMode("binance-gap"); got != paperArbModeBinanceGap {
		t.Fatalf("normalizePaperArbMode(binance-gap) = %q, want %q", got, paperArbModeBinanceGap)
	}
	if got := normalizePaperArbMode("copytrade"); got != paperArbModeCopytrade {
		t.Fatalf("normalizePaperArbMode(copytrade) = %q, want %q", got, paperArbModeCopytrade)
	}
	if got := normalizePaperArbMode("weird"); got != paperArbModeTaker {
		t.Fatalf("normalizePaperArbMode(weird) = %q, want %q", got, paperArbModeTaker)
	}
}

func TestPaperbotCopytradeShouldUsePublicActivityAPI(t *testing.T) {
	wallet := "0x0000000000000000000000000000000000000001"

	if !paperbotCopytradeShouldUsePublicActivityAPI(nil) {
		t.Fatal("expected nil poller to allow public activity api")
	}

	minedOnly := &paperbotCopytradePoller{
		minedWatcher: api.NewPolymarketMinedWatcher(
			"https://polygon-mainnet.infura.io/v3/test",
			&api.PolygonClient{},
			&api.RestClient{},
			wallet,
		),
	}
	if paperbotCopytradeShouldUsePublicActivityAPI(minedOnly) {
		t.Fatal("expected mined-only watcher to disable public activity api")
	}

	pending := &paperbotCopytradePoller{
		pendingWatcher: api.NewPolymarketPendingWatcher(
			"https://polygon-mainnet.g.alchemy.com/v2/test",
			&api.RestClient{},
			&api.PolygonClient{},
			wallet,
		),
	}
	if paperbotCopytradeShouldUsePublicActivityAPI(pending) {
		t.Fatal("expected pending watcher to disable public activity api")
	}
}

func TestPaperbotCopytradeMarketSelectableAllowsFinalSeconds(t *testing.T) {
	now := time.Unix(1700000000, 0)

	if !paperbotCopytradeMarketSelectable(now, time.Time{}) {
		t.Fatal("expected zero end time to remain selectable")
	}
	if !paperbotCopytradeMarketSelectable(now, now.Add(10*time.Second)) {
		t.Fatal("expected market with 10 seconds left to remain selectable")
	}
	if paperbotCopytradeMarketSelectable(now, now.Add(-time.Second)) {
		t.Fatal("expected expired market to be rejected")
	}
}

func TestPaperbotCopytradeCanTradeUntilActualExpiry(t *testing.T) {
	if !paperbotCopytradeCanTrade(paper.MarketStateActive, 10*time.Second) {
		t.Fatal("expected active market with time left to trade")
	}
	if !paperbotCopytradeCanTrade(paper.MarketStateEnding, 10*time.Second) {
		t.Fatal("expected ending market with time left to keep trading in copytrade mode")
	}
	if paperbotCopytradeCanTrade(paper.MarketStateEnding, 0) {
		t.Fatal("expected expired market to stop trading")
	}
}

func TestComputePaperMakerArbPricesStayInsideSpreadAndMarginCap(t *testing.T) {
	price1, price2, ok := computePaperMakerArbPrices(0.43, 0.46, 0.45, 0.49, 0.97)
	if !ok {
		t.Fatal("expected maker arb prices to be quotable")
	}
	if price1 <= 0.43 || price1 >= 0.46 {
		t.Fatalf("price1 = %.3f, want inside spread (0.43, 0.46)", price1)
	}
	if price2 <= 0.45 || price2 >= 0.49 {
		t.Fatalf("price2 = %.3f, want inside spread (0.45, 0.49)", price2)
	}
	if price1+price2 > 0.97+1e-9 {
		t.Fatalf("pair sum = %.3f, want <= 0.970", price1+price2)
	}
}

func TestComputePaperMakerArbPricesRejectWhenNoMakerRoom(t *testing.T) {
	if _, _, ok := computePaperMakerArbPrices(0.489, 0.490, 0.489, 0.490, 0.97); ok {
		t.Fatal("expected maker arb pricing to fail when there is no room inside the spread")
	}
	if _, _, ok := computePaperMakerArbPrices(0.44, 0.46, 0.44, 0.46, 0.85); ok {
		t.Fatal("expected maker arb pricing to fail when margin cap is too strict")
	}
}

func TestComputePaperMakerSplitSellPricesStayInsideSpreadAndMarginFloor(t *testing.T) {
	price1, price2, ok := computePaperMakerSplitSellPrices(0.49, 0.53, 0.50, 0.55, 1.03)
	if !ok {
		t.Fatal("expected split-backed maker sell prices to be quotable")
	}
	if price1 <= 0.49 || price1 >= 0.53 {
		t.Fatalf("price1 = %.3f, want inside spread (0.49, 0.53)", price1)
	}
	if price2 <= 0.50 || price2 >= 0.55 {
		t.Fatalf("price2 = %.3f, want inside spread (0.50, 0.55)", price2)
	}
	if price1+price2 < 1.03-1e-9 {
		t.Fatalf("pair quote sum = %.3f, want at least 1.030", price1+price2)
	}
	if price1+price2 > (0.53-paperMakerQuoteStep)+(0.55-paperMakerQuoteStep)+1e-9 {
		t.Fatalf("pair quote sum = %.3f exceeded available maker room", price1+price2)
	}
}

func TestComputePaperMakerSplitSellPricesRejectWhenNoProfitableMakerRoom(t *testing.T) {
	if _, _, ok := computePaperMakerSplitSellPrices(0.44, 0.46, 0.44, 0.46, 0.92); ok {
		t.Fatal("expected split-backed maker pricing to fail when there is no room inside the spread")
	}
	if _, _, ok := computePaperMakerSplitSellPrices(0.43, 0.46, 0.45, 0.49, 1.02); ok {
		t.Fatal("expected split-backed maker pricing to fail when margin floor is too high")
	}
}

func TestComputePaperMakerInventorySkewClampsToRange(t *testing.T) {
	if got := computePaperMakerInventorySkew(40, 0, 10); got != 1.0 {
		t.Fatalf("computePaperMakerInventorySkew long-heavy = %.2f, want 1.00", got)
	}
	if got := computePaperMakerInventorySkew(0, 40, 10); got != -1.0 {
		t.Fatalf("computePaperMakerInventorySkew short-heavy = %.2f, want -1.00", got)
	}
	if got := computePaperMakerInventorySkew(12, 8, 20); got != 0.2 {
		t.Fatalf("computePaperMakerInventorySkew balanced-ish = %.2f, want 0.20", got)
	}
}

func TestComputePaperMakerSkewedQuoteChangesAggressiveness(t *testing.T) {
	buyLong, ok := computePaperMakerSkewedQuote("buy", 0.47, 0.53, 1.0, 0.008, paperMakerStrategyParams)
	if !ok {
		t.Fatal("expected buy quote for long-heavy inventory")
	}
	buyShort, ok := computePaperMakerSkewedQuote("buy", 0.47, 0.53, -1.0, 0.008, paperMakerStrategyParams)
	if !ok {
		t.Fatal("expected buy quote for short-heavy inventory")
	}
	if buyLong >= buyShort {
		t.Fatalf("buy quote should be less aggressive when inventory is heavy: long=%.3f short=%.3f", buyLong, buyShort)
	}

	sellLong, ok := computePaperMakerSkewedQuote("sell", 0.47, 0.53, 1.0, 0.008, paperMakerStrategyParams)
	if !ok {
		t.Fatal("expected sell quote for long-heavy inventory")
	}
	sellShort, ok := computePaperMakerSkewedQuote("sell", 0.47, 0.53, -1.0, 0.008, paperMakerStrategyParams)
	if !ok {
		t.Fatal("expected sell quote for short-heavy inventory")
	}
	if sellLong >= sellShort {
		t.Fatalf("sell quote should be more aggressive when inventory is heavy: long=%.3f short=%.3f", sellLong, sellShort)
	}
}

func TestPaperbotCopytradeTargetSharesAggregatesByOutcome(t *testing.T) {
	shares := paperbotCopytradeTargetShares([]api.Position{
		{Outcome: "Up", Size: 2.25},
		{Outcome: "Up", Size: 0.75},
		{Outcome: "Down", Size: 4.0},
		{Outcome: "Down", Size: 0.009},
	})
	if shares["Up"] != 3.0 {
		t.Fatalf("expected Up shares 3.0, got %.4f", shares["Up"])
	}
	if shares["Down"] != 4.0 {
		t.Fatalf("expected Down shares 4.0, got %.4f", shares["Down"])
	}
}

func TestPaperbotCopytradeTargetSharesForConditionFiltersOtherMarkets(t *testing.T) {
	shares := paperbotCopytradeTargetSharesForCondition([]api.Position{
		{ConditionID: "cond-1", Outcome: "Up", Size: 2.25},
		{ConditionID: "cond-2", Outcome: "Up", Size: 9.0},
		{ConditionID: "cond-1", Outcome: "Down", Size: 4.0},
	}, "cond-1")
	if shares["Up"] != 2.25 {
		t.Fatalf("expected cond-1 Up shares 2.25, got %.4f", shares["Up"])
	}
	if shares["Down"] != 4.0 {
		t.Fatalf("expected cond-1 Down shares 4.0, got %.4f", shares["Down"])
	}
}

func TestPaperbotCopytradeSharesByConditionAggregatesPerMarket(t *testing.T) {
	sharesByCondition := paperbotCopytradeSharesByCondition([]api.Position{
		{ConditionID: "cond-1", Outcome: "Up", Size: 2.25},
		{ConditionID: "cond-1", Outcome: "Up", Size: 0.75},
		{ConditionID: "cond-1", Outcome: "Down", Size: 4.0},
		{ConditionID: "cond-2", Outcome: "Up", Size: 1.5},
		{ConditionID: "cond-2", Outcome: "Down", Size: 0.009},
	})
	if sharesByCondition["cond-1"]["Up"] != 3.0 {
		t.Fatalf("expected cond-1 Up shares 3.0, got %.4f", sharesByCondition["cond-1"]["Up"])
	}
	if sharesByCondition["cond-1"]["Down"] != 4.0 {
		t.Fatalf("expected cond-1 Down shares 4.0, got %.4f", sharesByCondition["cond-1"]["Down"])
	}
	if sharesByCondition["cond-2"]["Up"] != 1.5 {
		t.Fatalf("expected cond-2 Up shares 1.5, got %.4f", sharesByCondition["cond-2"]["Up"])
	}
	if sharesByCondition["cond-2"]["Down"] != 0 {
		t.Fatalf("expected cond-2 Down shares to ignore dust, got %.4f", sharesByCondition["cond-2"]["Down"])
	}
}

func TestPaperbotCopytradeTargetDeltaSkipsInitialSnapshotThenTracksNetChange(t *testing.T) {
	state := newPaperbotCopytradeState()
	t0 := time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)

	if delta, ready, pending := paperbotCopytradeTargetDelta(state, "Up", 25, t0); ready || pending || delta != 0 {
		t.Fatalf("initial snapshot should seed only, got delta=%.4f ready=%v pending=%v", delta, ready, pending)
	}
	if delta, ready, pending := paperbotCopytradeTargetDelta(state, "Up", 28.5, t0.Add(2*time.Second)); !ready || pending || delta != 3.5 {
		t.Fatalf("expected +3.5 delta after increase, got delta=%.4f ready=%v pending=%v", delta, ready, pending)
	}
	if delta, ready, pending := paperbotCopytradeTargetDelta(state, "Up", 26.0, t0.Add(4*time.Second)); ready || !pending || delta != 0 {
		t.Fatalf("expected first lower snapshot to wait for confirmation, got delta=%.4f ready=%v pending=%v", delta, ready, pending)
	}
	if delta, ready, pending := paperbotCopytradeTargetDelta(state, "Up", 26.0, t0.Add(6*time.Second)); !ready || pending || delta != -2.5 {
		t.Fatalf("expected -2.5 delta after second lower snapshot, got delta=%.4f ready=%v pending=%v", delta, ready, pending)
	}
}

func TestPaperbotCopytradeTargetDeltaSeedsVisiblePositionAfterTradesSeeded(t *testing.T) {
	state := newPaperbotCopytradeState()
	state.tradesSeeded = true
	t0 := time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)

	if delta, ready, pending := paperbotCopytradeTargetDelta(state, "Up", 7.25, t0); !ready || pending || math.Abs(delta-7.25) > 0.000001 {
		t.Fatalf("expected seeded trades to surface +7.25 delta, got delta=%.4f ready=%v pending=%v", delta, ready, pending)
	}
}

func TestPaperbotCopytradePositionSyncTradesCreatesFallbackSellWhenTargetFlats(t *testing.T) {
	state := newPaperbotCopytradeState()
	t0 := time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)

	if trades, deltas := paperbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		[]api.Position{{ConditionID: "cond-1", Outcome: "Up", Size: 5.51}},
		t0,
		nil,
		core.CopytradeSizingModePercent,
	); len(trades) != 0 || len(deltas) != 0 {
		t.Fatalf("initial seed should not emit sync trades, got trades=%d deltas=%d", len(trades), len(deltas))
	}

	trades, deltas := paperbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		nil,
		t0.Add(2*time.Second),
		nil,
		core.CopytradeSizingModePercent,
	)
	if len(trades) != 0 {
		t.Fatalf("first lower snapshot should wait for confirmation, got %+v", trades)
	}
	if len(deltas) != 0 {
		t.Fatalf("first lower snapshot should not surface a delta yet, got %+v", deltas)
	}

	trades, deltas = paperbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		nil,
		t0.Add(4*time.Second),
		nil,
		core.CopytradeSizingModePercent,
	)
	if len(trades) != 1 {
		t.Fatalf("expected one fallback sell trade, got %d", len(trades))
	}
	if trades[0].Side != "SELL" || trades[0].Outcome != "Up" || trades[0].Source != "position" {
		t.Fatalf("unexpected fallback sell trade: %+v", trades[0])
	}
	if math.Abs(trades[0].Size-5.51) > 0.000001 {
		t.Fatalf("expected fallback sell size 5.51, got %.4f", trades[0].Size)
	}
	if got := deltas["Up"]; math.Abs(got+5.51) > 0.000001 {
		t.Fatalf("expected target delta -5.51, got %.4f", got)
	}
}

func TestPaperbotCopytradePositionSyncTradesSkipsFallbackSellWhenFreshSellExists(t *testing.T) {
	state := newPaperbotCopytradeState()
	t0 := time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)

	paperbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		[]api.Position{{ConditionID: "cond-1", Outcome: "Up", Size: 5.51}},
		t0,
		nil,
		core.CopytradeSizingModePercent,
	)

	trades, deltas := paperbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		nil,
		t0.Add(2*time.Second),
		[]api.PublicTrade{{ConditionID: "cond-1", Outcome: "Up", Side: "SELL", Size: 5.51, Timestamp: t0.Add(2 * time.Second).Unix()}},
		core.CopytradeSizingModePercent,
	)
	if len(trades) != 0 {
		t.Fatalf("expected fresh sell to suppress fallback sell, got %+v", trades)
	}
	if len(deltas) != 0 {
		t.Fatalf("first lower snapshot should still be pending, got %+v", deltas)
	}
}

func TestPaperbotCopytradePositionSyncTradesCreatesEstimatedBuyWithoutFreshTrade(t *testing.T) {
	state := newPaperbotCopytradeState()
	t0 := time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)

	paperbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		nil,
		t0,
		nil,
		core.CopytradeSizingModePercent,
	)

	trades, deltas := paperbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		[]api.Position{{ConditionID: "cond-1", Outcome: "Up", Size: 7.25}},
		t0.Add(2*time.Second),
		nil,
		core.CopytradeSizingModePercent,
	)
	if len(trades) != 1 {
		t.Fatalf("expected one estimated buy trade, got %d", len(trades))
	}
	if trades[0].Side != "BUY" || trades[0].Outcome != "Up" {
		t.Fatalf("unexpected estimated buy trade: %+v", trades[0])
	}
	if trades[0].Source != "position" {
		t.Fatalf("expected position source, got %q", trades[0].Source)
	}
	if math.Abs(trades[0].Size-7.25) > 0.000001 {
		t.Fatalf("expected estimated buy size 7.25, got %.4f", trades[0].Size)
	}
	if got := deltas["Up"]; math.Abs(got-7.25) > 0.000001 {
		t.Fatalf("expected target delta 7.25, got %.4f", got)
	}
}

func TestPaperbotCopytradePositionSyncTradesCreatesResidualBuyWhenFreshBuyIsPartial(t *testing.T) {
	state := newPaperbotCopytradeState()
	t0 := time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)

	paperbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		nil,
		t0,
		nil,
		core.CopytradeSizingModePercent,
	)

	trades, deltas := paperbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		[]api.Position{{ConditionID: "cond-1", Outcome: "Up", Size: 10}},
		t0.Add(2*time.Second),
		[]api.PublicTrade{{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 4, Timestamp: t0.Add(2 * time.Second).Unix()}},
		core.CopytradeSizingModePercent,
	)
	if len(trades) != 1 {
		t.Fatalf("expected one residual estimated buy, got %d", len(trades))
	}
	if trades[0].Side != "BUY" || trades[0].Source != "position" {
		t.Fatalf("unexpected residual buy trade: %+v", trades[0])
	}
	if math.Abs(trades[0].Size-6) > 0.000001 {
		t.Fatalf("expected residual buy size 6, got %.4f", trades[0].Size)
	}
	if got := deltas["Up"]; math.Abs(got-10) > 0.000001 {
		t.Fatalf("expected full target delta 10, got %.4f", got)
	}
}

func TestPaperbotCopytradePositionSyncTradesCreatesBuyCatchupWhileHoldingBothOutcomes(t *testing.T) {
	state := newPaperbotCopytradeState()
	t0 := time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)

	paperbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		[]api.Position{
			{ConditionID: "cond-1", Outcome: "Up", Size: 100},
			{ConditionID: "cond-1", Outcome: "Down", Size: 100},
		},
		t0,
		nil,
		core.CopytradeSizingModePercent,
	)

	trades, deltas := paperbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		[]api.Position{
			{ConditionID: "cond-1", Outcome: "Up", Size: 129},
			{ConditionID: "cond-1", Outcome: "Down", Size: 100},
		},
		t0.Add(2*time.Second),
		nil,
		core.CopytradeSizingModePercent,
	)
	if len(trades) != 1 {
		t.Fatalf("expected one buy catch-up trade while holding both outcomes, got %d", len(trades))
	}
	if trades[0].Outcome != "Up" || trades[0].Side != "BUY" || trades[0].Source != "position" {
		t.Fatalf("unexpected buy catch-up trade: %+v", trades[0])
	}
	if math.Abs(trades[0].Size-29) > 0.000001 {
		t.Fatalf("expected 29-share catch-up buy, got %.4f", trades[0].Size)
	}
	if got := deltas["Up"]; math.Abs(got-29) > 0.000001 {
		t.Fatalf("expected up delta 29, got %.4f", got)
	}
}

func TestPaperbotCopytradePositionSyncTradesIgnoresFixedSizeModes(t *testing.T) {
	state := newPaperbotCopytradeState()
	t0 := time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)

	trades, deltas := paperbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		[]api.Position{{ConditionID: "cond-1", Outcome: "Up", Size: 7.25}},
		t0,
		nil,
		core.CopytradeSizingModeShares,
	)
	if len(trades) != 0 || len(deltas) != 0 {
		t.Fatalf("shares mode should ignore position sync, got trades=%d deltas=%d", len(trades), len(deltas))
	}

	trades, deltas = paperbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		[]api.Position{{ConditionID: "cond-1", Outcome: "Up", Size: 7.25}},
		t0,
		nil,
		core.CopytradeSizingModeUSDC,
	)
	if len(trades) != 0 || len(deltas) != 0 {
		t.Fatalf("usdc mode should ignore position sync, got trades=%d deltas=%d", len(trades), len(deltas))
	}
}

func TestPaperbotCopytradeFreshTradesIgnoresPreStartHistoryThenDedupes(t *testing.T) {
	state := newPaperbotCopytradeState()
	state.startedAt = time.Unix(1500, 0)
	initial := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 5.51, Timestamp: 1000, TransactionHash: "0x1"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "SELL", Size: 5.51, Timestamp: 2000, TransactionHash: "0x2"},
	}
	if got := paperbotCopytradeFreshTrades(state, initial, "cond-1"); len(got) != 1 {
		t.Fatalf("expected initial snapshot to ignore pre-start history and keep one post-start signal, got %d", len(got))
	}
	if got := paperbotCopytradeFreshTrades(state, initial, "cond-1"); len(got) != 0 {
		t.Fatalf("expected repeated trade snapshot to stay deduped, got %d", len(got))
	}

	next := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 5.51, Timestamp: 1000, TransactionHash: "0x1"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "SELL", Size: 5.51, Timestamp: 2000, TransactionHash: "0x2"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 1.25, Timestamp: 3000, TransactionHash: "0x3"},
	}
	got := paperbotCopytradeFreshTrades(state, next, "cond-1")
	if len(got) != 1 {
		t.Fatalf("expected exactly one fresh trade, got %d", len(got))
	}
	if got[0].Side != "BUY" || got[0].Timestamp != 3000 {
		t.Fatalf("unexpected fresh trade: %+v", got[0])
	}
}

func TestPaperbotCopytradeBootstrapStartTimestamp(t *testing.T) {
	if got := paperbotCopytradeBootstrapStartTimestamp(time.Unix(1500, 0)); got != 1500 {
		t.Fatalf("exact-second bootstrap timestamp = %d, want 1500", got)
	}
	if got := paperbotCopytradeBootstrapStartTimestamp(time.Unix(1500, 250_000_000)); got != 1499 {
		t.Fatalf("sub-second bootstrap timestamp = %d, want 1499", got)
	}
}

func TestPaperbotCopytradeFreshTradesSortsUnorderedHistoryChronologically(t *testing.T) {
	state := newPaperbotCopytradeState()
	state.startedAt = time.Unix(1000, 500_000_000)
	trades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 1, Timestamp: 1001, TransactionHash: "0xb"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 1, Timestamp: 1000, TransactionHash: "0xc"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 1, Timestamp: 1001, TransactionHash: "0xa"},
	}

	got := paperbotCopytradeFreshTrades(state, trades, "cond-1")
	if len(got) != 3 {
		t.Fatalf("expected three bootstrap trades, got %d", len(got))
	}
	if got[0].Timestamp != 1000 || got[0].TransactionHash != "0xc" {
		t.Fatalf("first trade = %+v, want timestamp 1000 tx 0xc", got[0])
	}
	if got[1].Timestamp != 1001 || got[1].TransactionHash != "0xa" {
		t.Fatalf("second trade = %+v, want timestamp 1001 tx 0xa", got[1])
	}
	if got[2].Timestamp != 1001 || got[2].TransactionHash != "0xb" {
		t.Fatalf("third trade = %+v, want timestamp 1001 tx 0xb", got[2])
	}
}

func TestPaperbotCopytradeFreshTradesBootstrapUsesObservedAtForOnchainSignals(t *testing.T) {
	state := newPaperbotCopytradeState()
	state.startedAt = time.Unix(1000, 0)
	trades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Timestamp: 999, ObservedAt: 1001, TransactionHash: "0xtx", Source: "onchain"},
	}

	got := paperbotCopytradeFreshTrades(state, trades, "cond-1")
	if len(got) != 1 {
		t.Fatalf("expected onchain signal observed after start to survive bootstrap, got %d", len(got))
	}
}

func TestPaperbotCopytradeFreshTradesBootstrapKeepsRecentWatcherSignalsBeforeStart(t *testing.T) {
	state := newPaperbotCopytradeState()
	state.startedAt = time.Unix(1000, 500_000_000)
	trades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Down", Side: "BUY", Size: 2, Timestamp: 981, TransactionHash: "0xon", Source: "onchain"},
		{ConditionID: "cond-1", Outcome: "Down", Side: "BUY", Size: 2, Timestamp: 981, TransactionHash: "0xmem", Source: "mempool", SignalID: "0xmem:1"},
	}

	got := paperbotCopytradeFreshTrades(state, trades, "cond-1")
	if len(got) != 2 {
		t.Fatalf("expected recent watcher signals just before start to survive bootstrap, got %d", len(got))
	}
}

func TestPaperbotCopytradeFreshTradesBootstrapDropsRecentPublicSignalsBeforeStart(t *testing.T) {
	state := newPaperbotCopytradeState()
	state.startedAt = time.Unix(1000, 500_000_000)
	trades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Down", Side: "BUY", Size: 2, Timestamp: 981, TransactionHash: "0xpub", Source: "public"},
	}

	got := paperbotCopytradeFreshTrades(state, trades, "cond-1")
	if len(got) != 0 {
		t.Fatalf("expected pre-start public signal to be dropped during bootstrap, got %d", len(got))
	}
}

func TestPaperbotCopytradeFreshTradesKeepsDistinctMempoolSignalsSameTx(t *testing.T) {
	state := newPaperbotCopytradeState()
	state.startedAt = time.Unix(1000, 0)
	trades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Timestamp: 1001, TransactionHash: "0xtx", Source: "mempool", SignalID: "0xtx:1"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Timestamp: 1001, TransactionHash: "0xtx", Source: "mempool", SignalID: "0xtx:2"},
	}

	got := paperbotCopytradeFreshTrades(state, trades, "cond-1")
	if len(got) != 2 {
		t.Fatalf("expected two distinct mempool signals, got %d", len(got))
	}
}

func TestPaperbotMergeCopytradeTradesDedupesWatcherAndPublicSameTx(t *testing.T) {
	watcherTrades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx", Source: "mempool", SignalID: "0xtx:asset-a:BUY"},
	}
	publicTrades := paperbotPrepareCopytradeTrades([]api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Asset: "asset-a", Timestamp: 1002, TransactionHash: "0xtx"},
	}, "public")

	got := paperbotMergeCopytradeTrades(watcherTrades, publicTrades)
	if len(got) != 1 {
		t.Fatalf("expected watcher/public duplicate tx to merge into one trade, got %d", len(got))
	}
	if got[0].Source != "mempool" {
		t.Fatalf("expected watcher trade to win merge precedence, got source %q", got[0].Source)
	}
}

func TestPaperbotCopytradeFreshTradesDetectsAdditionalPublicFillSameSignalAcrossPolls(t *testing.T) {
	state := newPaperbotCopytradeState()
	state.startedAt = time.Unix(1000, 0)

	poll1 := paperbotMergeCopytradeTrades(paperbotPrepareCopytradeTrades([]api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
	}, "public"))
	if got := paperbotCopytradeFreshTrades(state, poll1, "cond-1"); len(got) != 1 {
		t.Fatalf("Poll 1: expected 1 fresh trade, got %d", len(got))
	}

	poll2 := paperbotMergeCopytradeTrades(paperbotPrepareCopytradeTrades([]api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 3, Price: 0.46, Asset: "asset-a", Timestamp: 1002, TransactionHash: "0xtx"},
	}, "public"))
	got := paperbotCopytradeFreshTrades(state, poll2, "cond-1")
	if len(got) != 1 {
		t.Fatalf("Poll 2: expected 1 newly backfilled trade, got %d", len(got))
	}
	if got[0].Size != 3 || got[0].Timestamp != 1002 {
		t.Fatalf("unexpected backfilled trade: %+v", got[0])
	}
}

func TestPaperbotCopytradeFreshTradesKeepsDistinctPublicTradesSameTx(t *testing.T) {
	state := newPaperbotCopytradeState()
	state.startedAt = time.Unix(1000, 0)
	trades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.45, Asset: "asset-b", Timestamp: 1001, TransactionHash: "0xtx"},
	}

	got := paperbotCopytradeFreshTrades(state, trades, "cond-1")
	if len(got) != 2 {
		t.Fatalf("expected two distinct public trades from same tx, got %d", len(got))
	}
}

func TestPaperbotCopytradeFreshTradesKeepsIdenticalPublicTradesSameTx(t *testing.T) {
	state := newPaperbotCopytradeState()
	state.startedAt = time.Unix(1000, 0)
	trades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
	}

	got := paperbotCopytradeFreshTrades(state, trades, "cond-1")
	if len(got) != 2 {
		t.Fatalf("expected two identical fills from same tx to stay distinct, got %d", len(got))
	}
	if again := paperbotCopytradeFreshTrades(state, trades, "cond-1"); len(again) != 0 {
		t.Fatalf("expected repeated identical snapshot to stay deduped, got %d", len(again))
	}
}

func TestPaperbotCopytradeFreshTradesKeepsIdenticalPublicTradesAcrossPolls(t *testing.T) {
	state := newPaperbotCopytradeState()
	state.startedAt = time.Unix(1000, 0)

	// Poll 1: sees 3 identical trades
	trades1 := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
	}

	got1 := paperbotCopytradeFreshTrades(state, trades1, "cond-1")
	if len(got1) != 3 {
		t.Fatalf("Poll 1: expected 3 fresh trades, got %d", len(got1))
	}

	// Poll 2: sees 5 identical trades (2 new ones added to the batch)
	trades2 := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
	}

	got2 := paperbotCopytradeFreshTrades(state, trades2, "cond-1")
	if len(got2) != 2 {
		t.Fatalf("Poll 2: expected 2 fresh trades (3 already seen, 5 total in poll), got %d", len(got2))
	}
}

func TestPaperbotCopytradeTakeRetryTradesDropsStaleTimestampedSignals(t *testing.T) {
	state := newPaperbotCopytradeState()
	now := time.Unix(5000, 0)
	state.retryTrades = []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 1, Timestamp: now.Add(-25 * time.Second).Unix()},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 1, Timestamp: now.Add(-5 * time.Second).Unix()},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 1, Timestamp: 0},
	}

	got := paperbotCopytradeTakeRetryTrades(state, now)
	if len(got) != 2 {
		t.Fatalf("expected stale retries to be filtered, got %d", len(got))
	}
	if len(state.retryTrades) != 0 {
		t.Fatalf("expected retry queue to be drained after take, got %d", len(state.retryTrades))
	}
}

func TestPaperbotCopytradeQueueRetryTradesCapsQueueLength(t *testing.T) {
	state := newPaperbotCopytradeState()
	retries := make([]api.PublicTrade, paperCopytradeRetryQueueCap+8)
	for i := range retries {
		retries[i] = api.PublicTrade{
			ConditionID: "cond-1",
			Outcome:     "Up",
			Side:        "BUY",
			Size:        1,
			Timestamp:   int64(1000 + i),
		}
	}

	paperbotCopytradeQueueRetryTrades(state, retries)
	if len(state.retryTrades) != paperCopytradeRetryQueueCap {
		t.Fatalf("expected retry queue cap %d, got %d", paperCopytradeRetryQueueCap, len(state.retryTrades))
	}
	wantFirst := retries[len(retries)-paperCopytradeRetryQueueCap].Timestamp
	if state.retryTrades[0].Timestamp != wantFirst {
		t.Fatalf("expected queue to keep newest retries starting at %d, got %d", wantFirst, state.retryTrades[0].Timestamp)
	}
}

func TestPaperbotCopytradeTradeKeyPrefersSignalID(t *testing.T) {
	trade := api.PublicTrade{
		ConditionID:     "cond-1",
		Outcome:         "Up",
		Side:            "BUY",
		Size:            2,
		TransactionHash: "0xtx",
		Source:          "onchain",
		SignalID:        "0xtx:1",
	}
	if got := paperbotCopytradeTradeKey(trade); got != "signal|0xtx:1" {
		t.Fatalf("unexpected trade key %q", got)
	}
}

func TestPaperbotCopytradeSignalSummaryIncludesSourceAndTx(t *testing.T) {
	trade := api.PublicTrade{
		Outcome:         "Up",
		Side:            "buy",
		Size:            28.45,
		Source:          "onchain",
		TransactionHash: "0x1234567890abcdef",
	}
	if got := paperbotCopytradeSignalSummary(trade); got != "BUY Up | master=28.45 | source=ONCHAIN | tx=0x12345678..." {
		t.Fatalf("unexpected summary %q", got)
	}
}

func TestPaperbotCopytradeSignalSummaryDefaultsToPositionSource(t *testing.T) {
	trade := api.PublicTrade{
		Outcome: "Down",
		Side:    "sell",
		Size:    3,
	}
	if got := paperbotCopytradeSignalSummary(trade); got != "SELL Down | master=3 | source=POSITION" {
		t.Fatalf("unexpected summary %q", got)
	}
}

func TestPaperbotEstimatedPositionBuySignalsSplitsUsingObservedTradeSize(t *testing.T) {
	state := newPaperbotCopytradeState()
	paperbotObserveCopytradeBuySignal(state, api.PublicTrade{Outcome: "Up", Side: "BUY", Size: 10, Timestamp: 1001})
	paperbotObserveCopytradeBuySignal(state, api.PublicTrade{Outcome: "Up", Side: "BUY", Size: 10, Timestamp: 1002})

	got := paperbotEstimatedPositionBuySignals(state, "cond-1", "Up", 50, core.CopytradeSizingModeUSDC)
	if len(got) != 5 {
		t.Fatalf("expected 5 estimated position buys, got %d", len(got))
	}
	total := 0.0
	for _, sig := range got {
		total += sig.Size
		if sig.Source != "position-estimate" {
			t.Fatalf("expected position-estimate source, got %q", sig.Source)
		}
	}
	if total < 49.99 || total > 50.01 {
		t.Fatalf("unexpected total estimated delta %.6f", total)
	}
}

func TestPaperbotEstimatedPositionBuySignalsKeepsSinglePercentSignal(t *testing.T) {
	state := newPaperbotCopytradeState()
	got := paperbotEstimatedPositionBuySignals(state, "cond-1", "Up", 50, core.CopytradeSizingModePercent)
	if len(got) != 1 {
		t.Fatalf("expected 1 percent-mode signal, got %d", len(got))
	}
	if got[0].Source != "position" {
		t.Fatalf("expected position source, got %q", got[0].Source)
	}
}

func TestPaperbotCopytradeIsRateLimited(t *testing.T) {
	if !paperbotCopytradeIsRateLimited(errors.New("get public trades failed with status 429: error code: 1015")) {
		t.Fatal("expected 429/1015 error to be treated as rate limit")
	}
	if paperbotCopytradeIsRateLimited(errors.New("context deadline exceeded")) {
		t.Fatal("expected non-429 timeout error not to be treated as rate limit")
	}
}

func TestPaperbotCopytradeRateLimitBackoffCaps(t *testing.T) {
	if got := paperbotCopytradeRateLimitBackoff(1); got != time.Second {
		t.Fatalf("first backoff = %v, want 1s", got)
	}
	if got := paperbotCopytradeRateLimitBackoff(2); got != 2*time.Second {
		t.Fatalf("second backoff = %v, want 2s", got)
	}
	if got := paperbotCopytradeRateLimitBackoff(5); got != 8*time.Second {
		t.Fatalf("capped backoff = %v, want 8s", got)
	}
}

func TestPaperbotCopytradeHoldsBothOutcomes(t *testing.T) {
	if !paperbotCopytradeHoldsBothOutcomes(map[string]float64{"Up": 10, "Down": 5}) {
		t.Fatal("expected both-sided target inventory to be detected")
	}
	if paperbotCopytradeHoldsBothOutcomes(map[string]float64{"Up": 10, "Down": 0.009}) {
		t.Fatal("expected dust on second side not to count as both-sided inventory")
	}
}

func TestPaperbotCopytradeHasAmbiguousPositionExit(t *testing.T) {
	positions := []api.Position{
		{ConditionID: "cond-1", Outcome: "Up", Size: 10, Mergeable: true},
		{ConditionID: "cond-2", Outcome: "Down", Size: 10},
	}
	if !paperbotCopytradeHasAmbiguousPositionExit(positions, "cond-1") {
		t.Fatal("expected mergeable target inventory to block position-only sell fallback")
	}
	if paperbotCopytradeHasAmbiguousPositionExit(positions, "cond-2") {
		t.Fatal("expected unrelated non-mergeable market not to be blocked")
	}
}

func TestPaperbotFormatShareQtyKeepsFiveDecimalInventoryPrecision(t *testing.T) {
	if got := paperbotFormatShareQty(1.234567); got != "1.23457" {
		t.Fatalf("expected 5-decimal share precision, got %q", got)
	}
}

func TestPaperbotTraderLoopIntervalUsesSlowerCadenceForCopytrade(t *testing.T) {
	if got := paperbotTraderLoopInterval(paper.TUISettings{PaperArbMode: "copytrade", CopytradePollIntervalMs: 250}); got != 125*time.Millisecond {
		t.Fatalf("expected copytrade loop interval 125ms, got %s", got)
	}
	if got := paperbotTraderLoopInterval(paper.TUISettings{PaperArbMode: "maker"}); got != paperMainLoopInterval {
		t.Fatalf("expected default loop interval %s, got %s", paperMainLoopInterval, got)
	}
}

func TestPaperbotUIIntervalUsesSlowerCadenceForCopytrade(t *testing.T) {
	if got := paperbotUIInterval(paper.TUISettings{PaperArbMode: "copytrade", CopytradePollIntervalMs: 250}); got != 250*time.Millisecond {
		t.Fatalf("expected copytrade UI interval 250ms, got %s", got)
	}
	if got := paperbotUIInterval(paper.TUISettings{PaperArbMode: "maker"}); got != paperUIRefreshInterval {
		t.Fatalf("expected default UI interval %s, got %s", paperUIRefreshInterval, got)
	}
}

func TestShouldPaperReconnectWSOnlyForInvalidStaleBooks(t *testing.T) {
	outcomes := []string{"Down", "Up"}
	validBids := map[string]float64{"Down": 0.48, "Up": 0.49}
	validAsks := map[string]float64{"Down": 0.50, "Up": 0.51}

	if shouldPaperReconnectWS(outcomes, validBids, validAsks, 30*time.Second, 15*time.Second, false) {
		t.Fatal("expected quiet but valid WS book to stay connected")
	}

	invalidAsks := map[string]float64{"Down": 0.50, "Up": 0}
	if !shouldPaperReconnectWS(outcomes, validBids, invalidAsks, 30*time.Second, 15*time.Second, false) {
		t.Fatal("expected missing side on a stale local book to trigger reconnect")
	}

	if shouldPaperReconnectWS(outcomes, validBids, invalidAsks, 30*time.Second, 15*time.Second, true) {
		t.Fatal("expected terminal-looking book to suppress reconnects")
	}
}

func TestPaperLooksLikeTerminalBookRecognizesRoundedPinnedEndState(t *testing.T) {
	terminal := paperLooksLikeTerminalBook(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0.989, "Up": 0},
		map[string]float64{"Down": 0, "Up": 0.011},
	)
	if !terminal {
		t.Fatal("expected rounded 0.99/0.01 terminal book to count as terminal-looking")
	}
}

func TestPaperLooksLikeTerminalBookRejectsOrdinaryOneSidedBook(t *testing.T) {
	terminal := paperLooksLikeTerminalBook(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0.64, "Up": 0},
		map[string]float64{"Down": 0, "Up": 0.36},
	)
	if terminal {
		t.Fatal("expected ordinary one-sided book to require freshness checks")
	}
}

func TestPaperPairQuoteAgeTreatsZeroAsStale(t *testing.T) {
	age := paperPairQuoteAge(time.Time{}, time.Now())
	if age <= 3*time.Second {
		t.Fatalf("expected zero pair update time to be treated as stale, got %v", age)
	}
}

func TestShouldUseLocalPaperPairRequiresFreshValidBothSides(t *testing.T) {
	now := time.Now()
	outcomes := []string{"Yes", "No"}
	bids := map[string]float64{"Yes": 0.41, "No": 0.57}
	asks := map[string]float64{"Yes": 0.43, "No": 0.59}

	if !shouldUseLocalPaperPair(outcomes, bids, asks, now.Add(-500*time.Millisecond), 750*time.Millisecond, now) {
		t.Fatal("expected recent valid pair quotes to be usable")
	}
	if shouldUseLocalPaperPair(outcomes, bids, asks, now.Add(-2*time.Second), 750*time.Millisecond, now) {
		t.Fatal("expected stale pair quotes to be rejected")
	}
	asks["No"] = 0
	if shouldUseLocalPaperPair(outcomes, bids, asks, now.Add(-100*time.Millisecond), 750*time.Millisecond, now) {
		t.Fatal("expected missing quote on one side to invalidate local pair")
	}
}

func TestPaperExecutionQuoteGuardAgeCapsAtAwaitingWindow(t *testing.T) {
	if got := paperExecutionQuoteGuardAge(3 * time.Second); got != paperExecutionGuardQuoteMaxAge {
		t.Fatalf("expected execution guard to cap at %s, got %s", paperExecutionGuardQuoteMaxAge, got)
	}
}

func TestPaperExecutionQuoteGuardAgePreservesStricterConfig(t *testing.T) {
	if got := paperExecutionQuoteGuardAge(400 * time.Millisecond); got != 400*time.Millisecond {
		t.Fatalf("expected stricter configured age to pass through, got %s", got)
	}
}

func TestSummarizePaperRoundUsesSharedEngineDelta(t *testing.T) {
	engine := paper.NewEngine(100.0)
	engine.AddBalance(4.65)
	engine.AddRealizedPnL(4.65)

	roundPnL, totalEquity, roundTrades, stats := summarizePaperRound(engine, 100.0, 0)
	if abs := roundPnL - 4.65; abs < -0.000001 || abs > 0.000001 {
		t.Fatalf("expected round PnL 4.65, got %.2f", roundPnL)
	}
	if abs := totalEquity - 104.65; abs < -0.000001 || abs > 0.000001 {
		t.Fatalf("expected total equity 104.65, got %.2f", totalEquity)
	}
	if roundTrades != stats.TotalTrades {
		t.Fatalf("expected round trades to mirror stats delta, got %d vs %d", roundTrades, stats.TotalTrades)
	}
}

func TestSummarizePaperRoundKeepsOpenInventoryNeutralAtRotation(t *testing.T) {
	engine := paper.NewEngine(100.0)
	if _, err := engine.BuyForMarket("m1", "Up", 0.60, 10.0); err != nil {
		t.Fatalf("buy failed: %v", err)
	}
	engine.UpdateMarketBidAsk("m1", "Up", 0.30, 0.31)

	roundPnL, totalEquity, _, _ := summarizePaperRound(engine, 100.0, 0)
	if roundPnL != 0 {
		t.Fatalf("expected unresolved inventory to stay neutral at rotation, got %.2f", roundPnL)
	}
	if totalEquity != 100.0 {
		t.Fatalf("expected round equity to use book equity 100.00, got %.2f", totalEquity)
	}
}

func TestSummarizePaperRoundCountsLockedPairProfitAtRotation(t *testing.T) {
	engine := paper.NewEngine(100.0)
	if _, err := engine.BuyForMarket("m1", "Up", 0.48, 3.1); err != nil {
		t.Fatalf("buy up failed: %v", err)
	}
	if _, err := engine.BuyForMarket("m1", "Down", 0.49, 3.1); err != nil {
		t.Fatalf("buy down failed: %v", err)
	}

	roundPnL, totalEquity, _, _ := summarizePaperRound(engine, 100.0, 0)
	if abs := roundPnL - 0.093; abs < -0.000001 || abs > 0.000001 {
		t.Fatalf("expected locked round pnl 0.093, got %.6f", roundPnL)
	}
	if abs := totalEquity - 100.093; abs < -0.000001 || abs > 0.000001 {
		t.Fatalf("expected total equity 100.093, got %.6f", totalEquity)
	}
}

func TestPaperTradeSizeTracksResolutionOutcomeAfterCarrySettles(t *testing.T) {
	cfg := &core.Config{TradeScaleFactor: 0.05}

	t.Run("unresolved carry stays neutral", func(t *testing.T) {
		engine := paper.NewEngine(95.0)
		if !engine.SyncExternalPosition("m1", "Up", 10.0, 0.50) {
			t.Fatal("expected imported carry")
		}

		if got := cfg.CalculateTradeSize(engine.GetBookEquity()); got != 5.0 {
			t.Fatalf("expected 5%% trade size to stay $5.00 while unresolved, got %.2f", got)
		}
	})

	t.Run("matched unresolved pair uses locked profit immediately", func(t *testing.T) {
		engine := paper.NewEngine(100.0)
		if _, err := engine.BuyForMarket("m1", "Up", 0.48, 3.1); err != nil {
			t.Fatalf("buy up failed: %v", err)
		}
		if _, err := engine.BuyForMarket("m1", "Down", 0.49, 3.1); err != nil {
			t.Fatalf("buy down failed: %v", err)
		}

		if got := cfg.CalculateTradeSize(engine.GetBookEquity()); got < 5.004 || got > 5.005 {
			t.Fatalf("expected 5%% trade size to include locked pair profit, got %.6f", got)
		}
	})

	t.Run("winning resolution increases next trade size", func(t *testing.T) {
		engine := paper.NewEngine(95.0)
		if !engine.SyncExternalPosition("m1", "Up", 10.0, 0.50) {
			t.Fatal("expected imported carry")
		}

		res := engine.RedeemWithDetails("m1", "Up")
		if res.TotalPnL != 5.0 {
			t.Fatalf("expected total pnl 5.00, got %.2f", res.TotalPnL)
		}
		engine.SetBalance(105.0)
		engine.ClearPendingRedemption("m1")

		if got := cfg.CalculateTradeSize(engine.GetBookEquity()); got != 5.25 {
			t.Fatalf("expected 5%% trade size to become $5.25 after winning resolution, got %.2f", got)
		}
	})

	t.Run("losing resolution reduces next trade size", func(t *testing.T) {
		engine := paper.NewEngine(95.0)
		if !engine.SyncExternalPosition("m2", "Up", 10.0, 0.50) {
			t.Fatal("expected imported carry")
		}

		res := engine.RedeemWithDetails("m2", "Down")
		if res.TotalPnL != -5.0 {
			t.Fatalf("expected total pnl -5.00, got %.2f", res.TotalPnL)
		}

		if got := cfg.CalculateTradeSize(engine.GetBookEquity()); got != 4.75 {
			t.Fatalf("expected 5%% trade size to become $4.75 after losing resolution, got %.2f", got)
		}
	})
}

func TestComputePaperMakerQuoteSizesRespectSkewAndCaps(t *testing.T) {
	buyHeavy := computePaperMakerBuyQty(10, 18, 1.0, 20, 100, 0.49, paperMakerStrategyParams)
	buyLight := computePaperMakerBuyQty(10, 2, -1.0, 20, 100, 0.49, paperMakerStrategyParams)
	if buyHeavy >= buyLight {
		t.Fatalf("expected long-heavy inventory to quote smaller buys: heavy=%.0f light=%.0f", buyHeavy, buyLight)
	}

	sellHeavy := computePaperMakerSellQty(10, 30, 1.0, 0.50, paperMakerStrategyParams)
	sellBalanced := computePaperMakerSellQty(10, 30, 0.0, 0.50, paperMakerStrategyParams)
	if sellHeavy <= sellBalanced {
		t.Fatalf("expected long-heavy inventory to quote larger sells: heavy=%.0f balanced=%.0f", sellHeavy, sellBalanced)
	}
	if capped := computePaperMakerSellQty(10, 4, 1.0, 0.50, paperMakerStrategyParams); capped != 4 {
		t.Fatalf("expected sell qty to cap at available inventory, got %.0f want 4", capped)
	}
}

func TestComputePaperMakerSkewedQuoteRespectsConfiguredGap(t *testing.T) {
	tight, ok := computePaperMakerSkewedQuote("buy", 0.47, 0.53, 0.0, 0.003, paperMakerStrategyParams)
	if !ok {
		t.Fatal("expected tight maker quote")
	}
	wide, ok := computePaperMakerSkewedQuote("buy", 0.47, 0.53, 0.0, 0.012, paperMakerStrategyParams)
	if !ok {
		t.Fatal("expected wide maker quote")
	}
	if tight <= wide {
		t.Fatalf("expected tighter gap to place buy closer to ask: tight=%.3f wide=%.3f", tight, wide)
	}

	tightSell, ok := computePaperMakerSkewedQuote("sell", 0.47, 0.53, 0.0, 0.003, paperMakerStrategyParams)
	if !ok {
		t.Fatal("expected tight maker sell quote")
	}
	wideSell, ok := computePaperMakerSkewedQuote("sell", 0.47, 0.53, 0.0, 0.012, paperMakerStrategyParams)
	if !ok {
		t.Fatal("expected wide maker sell quote")
	}
	if tightSell >= wideSell {
		t.Fatalf("expected tighter gap to place sell closer to bid: tight=%.3f wide=%.3f", tightSell, wideSell)
	}
}

func TestComputePaperMakerProtectedSellQuoteIgnoresCostFloor(t *testing.T) {
	price, ok := computePaperMakerProtectedSellQuote(0.47, 0.60, 0.52, 0.02, 0.0, 0.008, 0, time.Hour, paperMakerStrategyParams)
	if !ok {
		t.Fatal("expected protected sell quote to be available")
	}
	if price < 0.54 {
		t.Fatalf("sell quote = %.3f, want at least 0.540 to clear cost floor", price)
	}
	if price >= 0.60 {
		t.Fatalf("sell quote = %.3f, want inside spread", price)
	}
}

func TestComputePaperMakerProtectedSellQuoteSucceedsEvenWhenNoProfitableRoom(t *testing.T) {
	if _, ok := computePaperMakerProtectedSellQuote(0.47, 0.54, 0.53, 0.02, 0.0, 0.008, 0, time.Hour, paperMakerStrategyParams); !ok {
		t.Fatal("expected protected sell quote to succeed even when spread cannot clear cost floor")
	}
}

func TestShouldPaperMakerBlockBuyRejectsBadPairCompletion(t *testing.T) {
	if !shouldPaperMakerBlockBuy(0, true, 24, 0.77, 0.46, 0.02) {
		t.Fatal("expected buy to be blocked when completing peer inventory would lock a bad pair")
	}
	if shouldPaperMakerBlockBuy(0, true, 24, 0.77, 0.20, 0.02) {
		t.Fatal("expected cheap enough completion buy to remain allowed")
	}
}

func TestShouldPaperMakerBlockBuyRejectsNoExitAddOn(t *testing.T) {
	if !shouldPaperMakerBlockBuy(20, false, 8, 0.35, 0.69, 0.02) {
		t.Fatal("expected add-on buy to be blocked when the current side is already heavy with no profitable sell")
	}
	if shouldPaperMakerBlockBuy(8, false, 20, 0.35, 0.20, 0.02) {
		t.Fatal("expected underweight side to remain buyable when completion price is safe")
	}
}

func TestEstimatePaperWinnerPrefersHighestBid(t *testing.T) {
	winner, prob := estimatePaperWinner(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0.99, "Up": 0.01},
		map[string]float64{"Down": 0.99, "Up": 0.01},
		map[string]float64{"Down": 0.99, "Up": 0.01},
	)
	if winner != "Down" {
		t.Fatalf("winner = %q, want Down", winner)
	}
	if prob != 0.99 {
		t.Fatalf("prob = %.3f, want 0.990", prob)
	}
}

func TestEstimatePaperWinnerFallsBackToAskThenMid(t *testing.T) {
	winner, prob := estimatePaperWinner(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0, "Up": 0},
		map[string]float64{"Down": 0.98, "Up": 0},
		map[string]float64{"Down": 0.50, "Up": 0.75},
	)
	if winner != "Down" {
		t.Fatalf("winner = %q, want Down", winner)
	}
	if prob != 0.97 {
		t.Fatalf("prob = %.3f, want 0.970", prob)
	}

	winner, prob = estimatePaperWinner(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0, "Up": 0},
		map[string]float64{"Down": 0, "Up": 0},
		map[string]float64{"Down": 0.40, "Up": 0.60},
	)
	if winner != "Up" {
		t.Fatalf("winner = %q, want Up", winner)
	}
	if prob != 0.60 {
		t.Fatalf("prob = %.3f, want 0.600", prob)
	}
}

func TestDetectTerminalWinnerFromPricesAcceptsPinnedBidWithMissingPeerQuotes(t *testing.T) {
	winner, prob, source, ok := detectTerminalWinnerFromPrices(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0.99, "Up": 0},
		map[string]float64{"Down": 0, "Up": 0},
		map[string]float64{"Down": 0, "Up": 0},
	)
	if !ok {
		t.Fatal("expected 0.99 pinned side to be accepted as terminal winner")
	}
	if winner != "Down" {
		t.Fatalf("winner = %q, want Down", winner)
	}
	if prob != 0.99 {
		t.Fatalf("prob = %.3f, want 0.990", prob)
	}
	if source != "bid" {
		t.Fatalf("source = %q, want bid", source)
	}
}

func TestDetectTerminalWinnerFromPricesUsesPeerAskNearZeroFallback(t *testing.T) {
	winner, prob, source, ok := detectTerminalWinnerFromPrices(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0, "Up": 0},
		map[string]float64{"Down": 0, "Up": 0.01},
		map[string]float64{"Down": 0, "Up": 0},
	)
	if ok || winner != "" || prob != 0 || source != "" {
		t.Fatalf("expected peer ask alone to stay unresolved, got winner=%q prob=%.3f source=%q ok=%v", winner, prob, source, ok)
	}
}

func TestDetectTerminalWinnerFromPricesRejectsNonTerminalLevels(t *testing.T) {
	winner, _, _, ok := detectTerminalWinnerFromPrices(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0.94, "Up": 0.06},
		map[string]float64{"Down": 0, "Up": 0},
		map[string]float64{"Down": 0, "Up": 0},
	)
	if ok || winner != "" {
		t.Fatalf("expected no terminal winner, got winner=%q ok=%v", winner, ok)
	}
}

func TestDetectTerminalWinnerFromPricesRejectsTiesAtTerminalLevel(t *testing.T) {
	winner, _, _, ok := detectTerminalWinnerFromPrices(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0.99, "Up": 0.99},
		map[string]float64{"Down": 0, "Up": 0},
		map[string]float64{"Down": 0, "Up": 0},
	)
	if ok || winner != "" {
		t.Fatalf("expected tie to be unresolved, got winner=%q ok=%v", winner, ok)
	}
}

func TestDetectTerminalWinnerFromPricesRejectsAskOnlyTerminalLevels(t *testing.T) {
	winner, _, source, ok := detectTerminalWinnerFromPrices(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0, "Up": 0},
		map[string]float64{"Down": 0.99, "Up": 0.01},
		map[string]float64{"Down": 0.995, "Up": 0.005},
	)
	if ok || winner != "" || source != "" {
		t.Fatalf("expected ask/mid-only terminal signal to stay unresolved, got winner=%q source=%q ok=%v", winner, source, ok)
	}
}

func TestDetectTerminalWinnerFromPricesUsesPinnedBidWithPeerAskConfirmation(t *testing.T) {
	winner, prob, source, ok := detectTerminalWinnerFromPrices(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0.985, "Up": 0},
		map[string]float64{"Down": 0, "Up": 0.01},
		map[string]float64{"Down": 0, "Up": 0},
	)
	if !ok {
		t.Fatal("expected pinned bid plus peer ask confirmation to infer terminal winner")
	}
	if winner != "Down" {
		t.Fatalf("winner = %q, want Down", winner)
	}
	if prob != 0.985 {
		t.Fatalf("prob = %.3f, want 0.985", prob)
	}
	if source != "bid+peer_ask" {
		t.Fatalf("source = %q, want bid+peer_ask", source)
	}
}

func TestDetectExpiryWinnerFromPricesUsesAskFallbackAtClose(t *testing.T) {
	winner, prob, source, ok := detectExpiryWinnerFromPrices(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0, "Up": 0},
		map[string]float64{"Down": 0.98, "Up": 0.03},
		map[string]float64{"Down": 0, "Up": 0},
	)
	if !ok {
		t.Fatal("expected ask fallback to infer close winner")
	}
	if winner != "Down" {
		t.Fatalf("winner = %q, want Down", winner)
	}
	if prob != 0.97 {
		t.Fatalf("prob = %.3f, want 0.970", prob)
	}
	if source != "ask" {
		t.Fatalf("source = %q, want ask", source)
	}
}

func TestDetermineWinnerUsesImmediateCloseSnapshot(t *testing.T) {
	engine := paper.NewEngine(100.0)
	trader := &MarketTrader{
		ID:        "BTC#close",
		Engine:    engine,
		TUI:       paper.NewTUI(engine, nil),
		Outcomes:  []string{"Down", "Up"},
		EndTime:   time.Now().Add(-100 * time.Millisecond),
		TokenBids: map[string]float64{"Down": 0.96, "Up": 0.02},
		TokenAsks: map[string]float64{"Down": 0.98, "Up": 0.03},
		FloatPrices: map[string]float64{
			"Down": 0.97,
			"Up":   0.025,
		},
		LastUpdate: time.Now().Add(-50 * time.Millisecond),
	}

	winner := trader.determineWinner()
	if winner != "Down" {
		t.Fatalf("winner = %q, want Down", winner)
	}
}

func TestDetermineWinnerRejectsStaleCloseSnapshot(t *testing.T) {
	engine := paper.NewEngine(100.0)
	trader := &MarketTrader{
		ID:        "BTC#stale",
		Engine:    engine,
		TUI:       paper.NewTUI(engine, nil),
		Outcomes:  []string{"Down", "Up"},
		EndTime:   time.Now().Add(-5 * time.Second),
		TokenBids: map[string]float64{"Down": 0.99, "Up": 0.01},
		TokenAsks: map[string]float64{"Down": 0.99, "Up": 0.01},
		FloatPrices: map[string]float64{
			"Down": 0.99,
			"Up":   0.01,
		},
		LastUpdate: time.Now().Add(-10 * time.Second),
	}

	winner := trader.determineWinner()
	if winner != "" {
		t.Fatalf("winner = %q, want unresolved stale snapshot", winner)
	}
}

func TestPaperPostExpiryResolutionStateKeepsFastScanThroughPlusFiveSeconds(t *testing.T) {
	endTime := time.Unix(1_700_000_000, 0)

	if interval, refresh := paperPostExpiryResolutionState(endTime, endTime); interval != paperPostExpiryWinnerPoll || !refresh {
		t.Fatalf("at expiry got interval=%v refresh=%v, want %v true", interval, refresh, paperPostExpiryWinnerPoll)
	}
	if interval, refresh := paperPostExpiryResolutionState(endTime.Add(4*time.Second), endTime); interval != paperPostExpiryWinnerPoll || !refresh {
		t.Fatalf("at +4s got interval=%v refresh=%v, want %v true", interval, refresh, paperPostExpiryWinnerPoll)
	}
	if interval, refresh := paperPostExpiryResolutionState(endTime.Add(5*time.Second), endTime); interval != paperResolutionRefreshInterval || !refresh {
		t.Fatalf("at +5s got interval=%v refresh=%v, want %v true", interval, refresh, paperResolutionRefreshInterval)
	}
}

func TestRefreshWinnerQuotesFromRESTDetectsSparseTerminalWinner(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenID := r.URL.Query().Get("token_id")
		w.Header().Set("Content-Type", "application/json")
		switch tokenID {
		case "token-down":
			_, _ = w.Write([]byte(`{"market":"m1","asset_id":"token-down","timestamp":"1700000000000","bids":[{"price":"0.99","size":"100"}],"asks":[]}`))
		case "token-up":
			_, _ = w.Write([]byte(`{"market":"m1","asset_id":"token-up","timestamp":"1700000000000","bids":[],"asks":[{"price":"0.01","size":"100"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	restClient := api.NewRestClient("polymarket")
	restClient.BaseURL = server.URL

	engine := paper.NewEngine(100.0)
	trader := &MarketTrader{
		ID:            "BTC#test",
		Engine:        engine,
		RestClient:    restClient,
		TUI:           paper.NewTUI(engine, nil),
		Outcomes:      []string{"Down", "Up"},
		TokenMap:      map[string]string{"token-down": "Down", "token-up": "Up"},
		TokenBids:     make(map[string]float64),
		TokenAsks:     make(map[string]float64),
		TokenFullBids: make(map[string][]paper.MarketLevel),
		TokenFullAsks: make(map[string][]paper.MarketLevel),
		FloatPrices:   make(map[string]float64),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := trader.refreshWinnerQuotesFromREST(ctx); err != nil {
		t.Fatalf("refreshWinnerQuotesFromREST failed: %v", err)
	}

	if got := trader.TokenBids["Down"]; got != 0.99 {
		t.Fatalf("Down bid = %.3f, want 0.990", got)
	}
	if got := trader.TokenAsks["Up"]; got != 0.01 {
		t.Fatalf("Up ask = %.3f, want 0.010", got)
	}

	winner := trader.determineWinner()
	if winner != "Down" {
		t.Fatalf("winner = %q, want Down", winner)
	}
}
