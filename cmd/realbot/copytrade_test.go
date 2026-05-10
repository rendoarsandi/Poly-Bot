package main

import (
	"errors"
	"math"
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
)

func TestRealbotCopytradeShouldUsePublicActivityAPI(t *testing.T) {
	wallet := "0x0000000000000000000000000000000000000001"

	if !realbotCopytradeShouldUsePublicActivityAPI(nil) {
		t.Fatal("expected nil poller to allow public activity api")
	}

	minedOnly := &realbotCopytradePoller{
		minedWatcher: api.NewPolymarketMinedWatcher(
			"https://polygon-mainnet.infura.io/v3/test",
			&api.PolygonClient{},
			&api.RestClient{},
			wallet,
		),
	}
	if realbotCopytradeShouldUsePublicActivityAPI(minedOnly) {
		t.Fatal("expected mined-only watcher to disable public activity api")
	}

	pending := &realbotCopytradePoller{
		pendingWatcher: api.NewPolymarketPendingWatcher(
			"https://polygon-mainnet.g.alchemy.com/v2/test",
			&api.RestClient{},
			&api.PolygonClient{},
			wallet,
		),
	}
	if realbotCopytradeShouldUsePublicActivityAPI(pending) {
		t.Fatal("expected pending watcher to disable public activity api")
	}
}

func TestRealbotCopytradeSignalSummaryIncludesSourceAndTx(t *testing.T) {
	trade := api.PublicTrade{
		Outcome:         "Up",
		Side:            "buy",
		Size:            28.45,
		Source:          "onchain",
		TransactionHash: "0x1234567890abcdef",
	}
	if got := realbotCopytradeSignalSummary(trade); got != "BUY Up | master=28.45 | source=ONCHAIN | tx=0x12345678..." {
		t.Fatalf("unexpected summary %q", got)
	}
}

func TestRealbotCopytradeSignalSummaryDefaultsToPositionSource(t *testing.T) {
	trade := api.PublicTrade{
		Outcome: "Down",
		Side:    "sell",
		Size:    3,
	}
	if got := realbotCopytradeSignalSummary(trade); got != "SELL Down | master=3 | source=POSITION" {
		t.Fatalf("unexpected summary %q", got)
	}
}

func TestRealbotCopytradeMarketSelectableAllowsFinalSeconds(t *testing.T) {
	now := time.Unix(1700000000, 0)

	if !realbotCopytradeMarketSelectable(now, time.Time{}) {
		t.Fatal("expected zero end time to remain selectable")
	}
	if !realbotCopytradeMarketSelectable(now, now.Add(10*time.Second)) {
		t.Fatal("expected market with 10 seconds left to remain selectable")
	}
	if realbotCopytradeMarketSelectable(now, now.Add(-time.Second)) {
		t.Fatal("expected expired market to be rejected")
	}
}

func TestRealbotCopytradeTargetSharesAggregatesByOutcome(t *testing.T) {
	shares := realbotCopytradeTargetShares([]api.Position{
		{Outcome: "Up", Size: 3.5},
		{Outcome: "Up", Size: 1.25},
		{Outcome: "Down", Size: 2.0},
		{Outcome: "Down", Size: 0.009},
	})
	if math.Abs(shares["Up"]-4.75) > 0.000001 {
		t.Fatalf("expected Up shares 4.75, got %.4f", shares["Up"])
	}
	if math.Abs(shares["Down"]-2.0) > 0.000001 {
		t.Fatalf("expected Down shares 2.0, got %.4f", shares["Down"])
	}
}

func TestRealbotCopytradeTargetSharesForConditionFiltersOtherMarkets(t *testing.T) {
	shares := realbotCopytradeTargetSharesForCondition([]api.Position{
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

func TestRealbotCopytradeSharesByConditionAggregatesPerMarket(t *testing.T) {
	sharesByCondition := realbotCopytradeSharesByCondition([]api.Position{
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

func TestRealbotCopytradeTargetDeltaSkipsInitialSnapshotThenTracksNetChange(t *testing.T) {
	state := newRealbotCopytradeState()
	t0 := time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)

	if delta, ready, pending := realbotCopytradeTargetDelta(state, "Up", 25, t0); ready || pending || delta != 0 {
		t.Fatalf("initial snapshot should seed only, got delta=%.4f ready=%v pending=%v", delta, ready, pending)
	}
	if delta, ready, pending := realbotCopytradeTargetDelta(state, "Up", 28.5, t0.Add(2*time.Second)); !ready || pending || delta != 3.5 {
		t.Fatalf("expected +3.5 delta after increase, got delta=%.4f ready=%v pending=%v", delta, ready, pending)
	}
	if delta, ready, pending := realbotCopytradeTargetDelta(state, "Up", 26.0, t0.Add(4*time.Second)); ready || !pending || delta != 0 {
		t.Fatalf("expected first lower snapshot to wait for confirmation, got delta=%.4f ready=%v pending=%v", delta, ready, pending)
	}
	if delta, ready, pending := realbotCopytradeTargetDelta(state, "Up", 26.0, t0.Add(6*time.Second)); !ready || pending || delta != -2.5 {
		t.Fatalf("expected -2.5 delta after second lower snapshot, got delta=%.4f ready=%v pending=%v", delta, ready, pending)
	}
}

func TestRealbotCopytradeTargetDeltaSeedsVisiblePositionAfterTradesSeeded(t *testing.T) {
	state := newRealbotCopytradeState()
	state.tradesSeeded = true
	t0 := time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)

	if delta, ready, pending := realbotCopytradeTargetDelta(state, "Up", 7.25, t0); !ready || pending || math.Abs(delta-7.25) > 0.000001 {
		t.Fatalf("expected seeded trades to surface +7.25 delta, got delta=%.4f ready=%v pending=%v", delta, ready, pending)
	}
}

func TestRealbotCopytradePositionSyncTradesCreatesFallbackSellWhenTargetFlats(t *testing.T) {
	state := newRealbotCopytradeState()
	t0 := time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)

	if trades, deltas := realbotCopytradePositionSyncTrades(
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

	trades, deltas := realbotCopytradePositionSyncTrades(
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

	trades, deltas = realbotCopytradePositionSyncTrades(
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

func TestRealbotCopytradePositionSyncTradesSkipsFallbackSellWhenFreshSellExists(t *testing.T) {
	state := newRealbotCopytradeState()
	t0 := time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)

	realbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		[]api.Position{{ConditionID: "cond-1", Outcome: "Up", Size: 5.51}},
		t0,
		nil,
		core.CopytradeSizingModePercent,
	)

	trades, deltas := realbotCopytradePositionSyncTrades(
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

func TestRealbotCopytradePositionSyncTradesCreatesEstimatedBuyWithoutFreshTrade(t *testing.T) {
	state := newRealbotCopytradeState()
	t0 := time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)

	realbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		nil,
		t0,
		nil,
		core.CopytradeSizingModePercent,
	)

	trades, deltas := realbotCopytradePositionSyncTrades(
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

func TestRealbotCopytradePositionSyncTradesCreatesResidualBuyWhenFreshBuyIsPartial(t *testing.T) {
	state := newRealbotCopytradeState()
	t0 := time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)

	realbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		nil,
		t0,
		nil,
		core.CopytradeSizingModePercent,
	)

	trades, deltas := realbotCopytradePositionSyncTrades(
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

func TestRealbotCopytradePositionSyncTradesCreatesBuyCatchupWhileHoldingBothOutcomes(t *testing.T) {
	state := newRealbotCopytradeState()
	t0 := time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)

	realbotCopytradePositionSyncTrades(
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

	trades, deltas := realbotCopytradePositionSyncTrades(
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

func TestRealbotCopytradePositionSyncTradesIgnoresFixedSizeModes(t *testing.T) {
	state := newRealbotCopytradeState()
	t0 := time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)

	trades, deltas := realbotCopytradePositionSyncTrades(
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

	trades, deltas = realbotCopytradePositionSyncTrades(
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

func TestRealbotCopytradeFreshTradesIgnoresPreStartHistoryThenDedupes(t *testing.T) {
	state := newRealbotCopytradeState()
	state.startedAt = time.Unix(1500, 0)
	initial := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 5.51, Timestamp: 1000, TransactionHash: "0x1"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "SELL", Size: 5.51, Timestamp: 2000, TransactionHash: "0x2"},
	}
	if got := realbotCopytradeFreshTrades(state, initial, "cond-1", "shares"); len(got) != 1 {
		t.Fatalf("expected initial snapshot to ignore pre-start history and keep one post-start signal, got %d", len(got))
	}
	if got := realbotCopytradeFreshTrades(state, initial, "cond-1", "shares"); len(got) != 0 {
		t.Fatalf("expected repeated trade snapshot to stay deduped, got %d", len(got))
	}

	next := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 5.51, Timestamp: 1000, TransactionHash: "0x1"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "SELL", Size: 5.51, Timestamp: 2000, TransactionHash: "0x2"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 1.25, Timestamp: 3000, TransactionHash: "0x3"},
	}
	got := realbotCopytradeFreshTrades(state, next, "cond-1", "shares")
	if len(got) != 1 {
		t.Fatalf("expected exactly one fresh trade, got %d", len(got))
	}
	if got[0].Side != "BUY" || got[0].Timestamp != 3000 {
		t.Fatalf("unexpected fresh trade: %+v", got[0])
	}
}

func TestRealbotCopytradeBootstrapStartTimestamp(t *testing.T) {
	if got := realbotCopytradeBootstrapStartTimestamp(time.Unix(1500, 0)); got != 1500 {
		t.Fatalf("exact-second bootstrap timestamp = %d, want 1500", got)
	}
	if got := realbotCopytradeBootstrapStartTimestamp(time.Unix(1500, 250_000_000)); got != 1499 {
		t.Fatalf("sub-second bootstrap timestamp = %d, want 1499", got)
	}
}

func TestRealbotCopytradeFreshTradesSortsUnorderedHistoryChronologically(t *testing.T) {
	state := newRealbotCopytradeState()
	state.startedAt = time.Unix(1000, 500_000_000)
	trades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 1, Timestamp: 1001, TransactionHash: "0xb"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 1, Timestamp: 1000, TransactionHash: "0xc"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 1, Timestamp: 1001, TransactionHash: "0xa"},
	}

	got := realbotCopytradeFreshTrades(state, trades, "cond-1", "shares")
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

func TestRealbotCopytradeFreshTradesBootstrapUsesObservedAtForOnchainSignals(t *testing.T) {
	state := newRealbotCopytradeState()
	state.startedAt = time.Unix(1000, 0)
	trades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Timestamp: 999, ObservedAt: 1001, TransactionHash: "0xtx", Source: "onchain"},
	}

	got := realbotCopytradeFreshTrades(state, trades, "cond-1", "shares")
	if len(got) != 1 {
		t.Fatalf("expected onchain signal observed after start to survive bootstrap, got %d", len(got))
	}
}

func TestRealbotCopytradeFreshTradesBootstrapKeepsRecentWatcherSignalsBeforeStart(t *testing.T) {
	state := newRealbotCopytradeState()
	state.startedAt = time.Unix(1000, 500_000_000)
	trades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Down", Side: "BUY", Size: 2, Timestamp: 981, TransactionHash: "0xon", Source: "onchain"},
		{ConditionID: "cond-1", Outcome: "Down", Side: "BUY", Size: 2, Timestamp: 981, TransactionHash: "0xmem", Source: "mempool", SignalID: "0xmem:1"},
	}

	got := realbotCopytradeFreshTrades(state, trades, "cond-1", "shares")
	if len(got) != 2 {
		t.Fatalf("expected recent watcher signals just before start to survive bootstrap, got %d", len(got))
	}
}

func TestRealbotCopytradeFreshTradesBootstrapDropsRecentPublicSignalsBeforeStart(t *testing.T) {
	state := newRealbotCopytradeState()
	state.startedAt = time.Unix(1000, 500_000_000)
	trades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Down", Side: "BUY", Size: 2, Timestamp: 981, TransactionHash: "0xpub", Source: "public"},
	}

	got := realbotCopytradeFreshTrades(state, trades, "cond-1", "shares")
	if len(got) != 0 {
		t.Fatalf("expected pre-start public signal to be dropped during bootstrap, got %d", len(got))
	}
}

func TestRealbotCopytradeFreshTradesKeepsDistinctMempoolSignalsSameTx(t *testing.T) {
	state := newRealbotCopytradeState()
	state.startedAt = time.Unix(1000, 0)
	trades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Timestamp: 1001, TransactionHash: "0xtx", Source: "mempool", SignalID: "0xtx:1"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Timestamp: 1001, TransactionHash: "0xtx", Source: "mempool", SignalID: "0xtx:2"},
	}

	got := realbotCopytradeFreshTrades(state, trades, "cond-1", "shares")
	if len(got) != 2 {
		t.Fatalf("expected two distinct mempool signals, got %d", len(got))
	}
}

func TestRealbotMergeCopytradeTradesDedupesWatcherAndPublicSameTx(t *testing.T) {
	watcherTrades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx", Source: "mempool", SignalID: "0xtx:asset-a:BUY"},
	}
	publicTrades := realbotPrepareCopytradeTrades([]api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Asset: "asset-a", Timestamp: 1002, TransactionHash: "0xtx"},
	}, "public", paper.TUISettings{})

	got := realbotMergeCopytradeTrades(watcherTrades, publicTrades)
	if len(got) != 1 {
		t.Fatalf("expected watcher/public duplicate tx to merge into one trade, got %d", len(got))
	}
	if got[0].Source != "mempool" {
		t.Fatalf("expected watcher trade to win merge precedence, got source %q", got[0].Source)
	}
}

func TestRealbotCopytradeFreshTradesDetectsAdditionalPublicFillSameSignalAcrossPolls(t *testing.T) {
	state := newRealbotCopytradeState()
	state.startedAt = time.Unix(1000, 0)

	poll1 := realbotMergeCopytradeTrades(realbotPrepareCopytradeTrades([]api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
	}, "public", paper.TUISettings{}))
	if got := realbotCopytradeFreshTrades(state, poll1, "cond-1", "shares"); len(got) != 1 {
		t.Fatalf("Poll 1: expected 1 fresh trade, got %d", len(got))
	}

	poll2 := realbotMergeCopytradeTrades(realbotPrepareCopytradeTrades([]api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 3, Price: 0.46, Asset: "asset-a", Timestamp: 1002, TransactionHash: "0xtx"},
	}, "public", paper.TUISettings{}))
	got := realbotCopytradeFreshTrades(state, poll2, "cond-1", "shares")
	if len(got) != 1 {
		t.Fatalf("Poll 2: expected 1 newly backfilled trade, got %d", len(got))
	}
	if got[0].Size != 3 || got[0].Timestamp != 1002 {
		t.Fatalf("unexpected backfilled trade: %+v", got[0])
	}
}

func TestRealbotCopytradeFreshTradesKeepsDistinctPublicTradesSameTx(t *testing.T) {
	state := newRealbotCopytradeState()
	state.startedAt = time.Unix(1000, 0)
	trades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.45, Asset: "asset-b", Timestamp: 1001, TransactionHash: "0xtx"},
	}

	got := realbotCopytradeFreshTrades(state, trades, "cond-1", "shares")
	if len(got) != 2 {
		t.Fatalf("expected two distinct public trades from same tx, got %d", len(got))
	}
}

func TestRealbotCopytradeFreshTradesKeepsIdenticalPublicTradesSameTx(t *testing.T) {
	state := newRealbotCopytradeState()
	state.startedAt = time.Unix(1000, 0)
	trades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
	}

	got := realbotCopytradeFreshTrades(state, trades, "cond-1", "shares")
	if len(got) != 2 {
		t.Fatalf("expected two identical fills from same tx to stay distinct, got %d", len(got))
	}
	if again := realbotCopytradeFreshTrades(state, trades, "cond-1", "shares"); len(again) != 0 {
		t.Fatalf("expected repeated identical snapshot to stay deduped, got %d", len(again))
	}
}

func TestRealbotCopytradeFreshTradesKeepsIdenticalPublicTradesAcrossPolls(t *testing.T) {
	state := newRealbotCopytradeState()
	state.startedAt = time.Unix(1000, 0)

	// Poll 1: sees 3 identical trades
	trades1 := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
	}

	got1 := realbotCopytradeFreshTrades(state, trades1, "cond-1", "shares")
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

	got2 := realbotCopytradeFreshTrades(state, trades2, "cond-1", "shares")
	if len(got2) != 2 {
		t.Fatalf("Poll 2: expected 2 fresh trades (3 already seen, 5 total in poll), got %d", len(got2))
	}
}

func TestRealbotCopytradeTakeRetryTradesDropsStaleTimestampedSignals(t *testing.T) {
	state := newRealbotCopytradeState()
	now := time.Unix(5000, 0)
	state.retryTrades = []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 1, Timestamp: now.Add(-25 * time.Second).Unix()},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 1, Timestamp: now.Add(-5 * time.Second).Unix()},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 1, Timestamp: 0},
	}

	got := realbotCopytradeTakeRetryTrades(state, now)
	if len(got) != 2 {
		t.Fatalf("expected stale retries to be filtered, got %d", len(got))
	}
	if len(state.retryTrades) != 0 {
		t.Fatalf("expected retry queue to be drained after take, got %d", len(state.retryTrades))
	}
}

func TestRealbotCopytradeQueueRetryTradesCapsQueueLength(t *testing.T) {
	state := newRealbotCopytradeState()
	retries := make([]api.PublicTrade, realbotCopytradeRetryQueueCap+8)
	for i := range retries {
		retries[i] = api.PublicTrade{
			ConditionID: "cond-1",
			Outcome:     "Up",
			Side:        "BUY",
			Size:        1,
			Timestamp:   int64(1000 + i),
		}
	}

	realbotCopytradeQueueRetryTrades(state, retries)
	if len(state.retryTrades) != realbotCopytradeRetryQueueCap {
		t.Fatalf("expected retry queue cap %d, got %d", realbotCopytradeRetryQueueCap, len(state.retryTrades))
	}
	wantFirst := retries[len(retries)-realbotCopytradeRetryQueueCap].Timestamp
	if state.retryTrades[0].Timestamp != wantFirst {
		t.Fatalf("expected queue to keep newest retries starting at %d, got %d", wantFirst, state.retryTrades[0].Timestamp)
	}
}

func TestRealbotCopytradeTradeKeyPrefersSignalID(t *testing.T) {
	trade := api.PublicTrade{
		ConditionID:     "cond-1",
		Outcome:         "Up",
		Side:            "BUY",
		Size:            2,
		TransactionHash: "0xtx",
		Source:          "onchain",
		SignalID:        "0xtx:1",
	}
	if got := realbotCopytradeTradeKey(trade); got != "signal|0xtx:1" {
		t.Fatalf("unexpected trade key %q", got)
	}
}

func TestRealbotCopytradeIsRateLimited(t *testing.T) {
	if !realbotCopytradeIsRateLimited(errors.New("get public trades failed with status 429: error code: 1015")) {
		t.Fatal("expected 429/1015 error to be treated as rate limit")
	}
	if realbotCopytradeIsRateLimited(errors.New("context deadline exceeded")) {
		t.Fatal("expected non-429 timeout error not to be treated as rate limit")
	}
}

func TestRealbotCopytradeRateLimitBackoffCaps(t *testing.T) {
	if got := realbotCopytradeRateLimitBackoff(1); got != time.Second {
		t.Fatalf("first backoff = %v, want 1s", got)
	}
	if got := realbotCopytradeRateLimitBackoff(2); got != 2*time.Second {
		t.Fatalf("second backoff = %v, want 2s", got)
	}
	if got := realbotCopytradeRateLimitBackoff(5); got != 8*time.Second {
		t.Fatalf("capped backoff = %v, want 8s", got)
	}
}

func TestRealbotCopytradeHoldsBothOutcomes(t *testing.T) {
	if !realbotCopytradeHoldsBothOutcomes(map[string]float64{"Up": 10, "Down": 5}) {
		t.Fatal("expected both-sided target inventory to be detected")
	}
	if realbotCopytradeHoldsBothOutcomes(map[string]float64{"Up": 10, "Down": 0.009}) {
		t.Fatal("expected dust on second side not to count as both-sided inventory")
	}
}

func TestRealbotCopytradeHasAmbiguousPositionExit(t *testing.T) {
	positions := []api.Position{
		{ConditionID: "cond-1", Outcome: "Up", Size: 10, Mergeable: true},
		{ConditionID: "cond-2", Outcome: "Down", Size: 10},
	}
	if !realbotCopytradeHasAmbiguousPositionExit(positions, "cond-1") {
		t.Fatal("expected mergeable target inventory to block position-only sell fallback")
	}
	if realbotCopytradeHasAmbiguousPositionExit(positions, "cond-2") {
		t.Fatal("expected unrelated non-mergeable market not to be blocked")
	}
}

func TestRealbotTraderLoopIntervalUsesSlowerCadenceForCopytrade(t *testing.T) {
	if got := realbotTraderLoopInterval(paper.TUISettings{PaperArbMode: "copytrade", CopytradePollIntervalMs: 250}); got != 125*time.Millisecond {
		t.Fatalf("expected copytrade loop interval 125ms, got %s", got)
	}
	if got := realbotTraderLoopInterval(paper.TUISettings{PaperArbMode: "maker"}); got != realbotMainLoopInterval {
		t.Fatalf("expected default loop interval %s, got %s", realbotMainLoopInterval, got)
	}
}

func TestRealbotUIIntervalUsesSlowerCadenceForCopytrade(t *testing.T) {
	if got := realbotUIInterval(paper.TUISettings{PaperArbMode: "copytrade", CopytradePollIntervalMs: 250}); got != 1000*time.Millisecond {
		t.Fatalf("expected copytrade UI interval 1000ms, got %s", got)
	}
	if got := realbotUIInterval(paper.TUISettings{PaperArbMode: "maker"}); got != realbotUIRefreshInterval {
		t.Fatalf("expected default UI interval %s, got %s", realbotUIRefreshInterval, got)
	}
}
