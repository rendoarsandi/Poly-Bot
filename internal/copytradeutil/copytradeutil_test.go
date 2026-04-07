package copytradeutil

import (
	"errors"
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
)

func TestTargetSharesHelpers(t *testing.T) {
	positions := []api.Position{
		{ConditionID: "cond-1", Outcome: "Up", Size: 2.25},
		{ConditionID: "cond-1", Outcome: "Up", Size: 0.75},
		{ConditionID: "cond-1", Outcome: "Down", Size: 4.0},
		{ConditionID: "cond-2", Outcome: "Up", Size: 1.5},
		{ConditionID: "cond-2", Outcome: "Down", Size: 0.009},
		{ConditionID: "cond-3", Outcome: "  ", Size: 5},
	}

	shares := TargetShares(positions)
	if shares["Up"] != 4.5 {
		t.Fatalf("TargetShares Up = %.4f, want 4.5", shares["Up"])
	}
	if shares["Down"] != 4.0 {
		t.Fatalf("TargetShares Down = %.4f, want 4.0", shares["Down"])
	}

	cond1 := TargetSharesForCondition(positions, "cond-1")
	if cond1["Up"] != 3.0 {
		t.Fatalf("TargetSharesForCondition cond-1 Up = %.4f, want 3.0", cond1["Up"])
	}
	if cond1["Down"] != 4.0 {
		t.Fatalf("TargetSharesForCondition cond-1 Down = %.4f, want 4.0", cond1["Down"])
	}

	byCondition := SharesByCondition(positions)
	if byCondition["cond-1"]["Up"] != 3.0 {
		t.Fatalf("SharesByCondition cond-1 Up = %.4f, want 3.0", byCondition["cond-1"]["Up"])
	}
	if byCondition["cond-2"]["Down"] != 0 {
		t.Fatalf("SharesByCondition cond-2 Down = %.4f, want 0", byCondition["cond-2"]["Down"])
	}
}

func TestInventoryAndConditionHelpers(t *testing.T) {
	if !HoldsBothOutcomes(map[string]float64{"Up": 5, "Down": 2}) {
		t.Fatal("expected both-sided inventory to be detected")
	}
	if HoldsBothOutcomes(map[string]float64{"Up": 5, "Down": 0.009}) {
		t.Fatal("expected dust on one side not to count")
	}

	positions := []api.Position{
		{ConditionID: "cond-1", Outcome: "Up", Size: 3, Mergeable: true},
		{ConditionID: "cond-2", Outcome: "Down", Size: 4},
	}
	if !HasAmbiguousPositionExit(positions, "cond-1") {
		t.Fatal("expected mergeable target inventory to block exit")
	}
	if HasAmbiguousPositionExit(positions, "cond-2") {
		t.Fatal("expected unrelated non-mergeable market not to block exit")
	}

	normalized := NormalizeConditionIDs([]string{" cond-1 ", "", "cond-2", "cond-1"})
	if len(normalized) != 2 || normalized[0] != "cond-1" || normalized[1] != "cond-2" {
		t.Fatalf("NormalizeConditionIDs = %#v, want [cond-1 cond-2]", normalized)
	}

	original := map[string]float64{"Up": 2}
	cloned := CloneOutcomeShares(original)
	cloned["Up"] = 5
	if original["Up"] != 2 {
		t.Fatalf("CloneOutcomeShares mutated original map: %#v", original)
	}
}

func TestFilterAndTimeoutHelpers(t *testing.T) {
	trades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up"},
		{ConditionID: "cond-2", Outcome: "Down"},
	}
	filteredTrades := FilterTradesByCondition(trades, "cond-1")
	if len(filteredTrades) != 1 || filteredTrades[0].ConditionID != "cond-1" {
		t.Fatalf("FilterTradesByCondition = %#v", filteredTrades)
	}

	positions := []api.Position{
		{ConditionID: "cond-1", Outcome: "Up", Size: 1},
		{ConditionID: "cond-2", Outcome: "Down", Size: 1},
	}
	filteredPositions := FilterPositionsByCondition(positions, "cond-2")
	if len(filteredPositions) != 1 || filteredPositions[0].ConditionID != "cond-2" {
		t.Fatalf("FilterPositionsByCondition = %#v", filteredPositions)
	}

	if got := TradeFetchTimeout(100 * time.Millisecond); got != 1500*time.Millisecond {
		t.Fatalf("TradeFetchTimeout(min) = %v, want 1.5s", got)
	}
	if got := TradeFetchTimeout(2 * time.Second); got != 2500*time.Millisecond {
		t.Fatalf("TradeFetchTimeout(max) = %v, want 2.5s", got)
	}
	if got := PositionFetchTimeout(100 * time.Millisecond); got != 4*time.Second {
		t.Fatalf("PositionFetchTimeout(min) = %v, want 4s", got)
	}
	if got := PositionFetchTimeout(10 * time.Second); got != 5*time.Second {
		t.Fatalf("PositionFetchTimeout(mid) = %v, want 5s", got)
	}

	now := time.Unix(5000, 0)
	if !CanReusePositions(now, now.Add(-4*time.Second), 200*time.Millisecond) {
		t.Fatal("expected fresh positions inside min reuse window to be reusable")
	}
	if CanReusePositions(now, now.Add(-6*time.Second), 200*time.Millisecond) {
		t.Fatal("expected stale positions outside min reuse window to be rejected")
	}
	if !CanReusePositions(now, now.Add(-14*time.Second), 10*time.Second) {
		t.Fatal("expected positions inside max reuse clamp to be reusable")
	}
	if CanReusePositions(now, now.Add(-16*time.Second), 10*time.Second) {
		t.Fatal("expected positions outside max reuse clamp to be rejected")
	}
}

func TestSignalIdentityHelpers(t *testing.T) {
	trade := api.PublicTrade{
		ConditionID:     "cond-1",
		Outcome:         "Up",
		Side:            "buy",
		Size:            2,
		Price:           0.44,
		Asset:           "asset-a",
		Timestamp:       1001,
		ObservedAt:      1002,
		TransactionHash: "0xtx",
		SignalID:        "signal-1",
	}

	if got := TradeKey(trade); got != "signal|signal-1" {
		t.Fatalf("TradeKey = %q, want signal|signal-1", got)
	}
	if got := EffectiveTimestamp(trade); got != 1002 {
		t.Fatalf("EffectiveTimestamp = %d, want 1002", got)
	}
	if got := SignalSource(api.PublicTrade{}); got != "position" {
		t.Fatalf("SignalSource(default) = %q, want position", got)
	}
	if got := SignalSource(api.PublicTrade{Timestamp: 1}); got != "trade" {
		t.Fatalf("SignalSource(trade) = %q, want trade", got)
	}
	if got := SignalSource(api.PublicTrade{Source: "mempool"}); got != "mempool" {
		t.Fatalf("SignalSource(explicit) = %q, want mempool", got)
	}
	if got := SignalSourceLabel(api.PublicTrade{Source: "position-estimate"}); got != "POSITION" {
		t.Fatalf("SignalSourceLabel(position-estimate) = %q, want POSITION", got)
	}
	if got := SignalSourceLabel(api.PublicTrade{Source: "public"}); got != "PUBLIC" {
		t.Fatalf("SignalSourceLabel(public) = %q, want PUBLIC", got)
	}
	if got := NormalizeSignalID(api.PublicTrade{TransactionHash: "0xtx", Asset: "asset-a", Side: "buy"}); got != "0xtx:asset-a:BUY" {
		t.Fatalf("NormalizeSignalID(fallback) = %q", got)
	}
	if got := NormalizeSignalID(api.PublicTrade{SignalID: "signal-1"}); got != "signal-1" {
		t.Fatalf("NormalizeSignalID(explicit) = %q", got)
	}
	if got := NormalizeSignalID(api.PublicTrade{TransactionHash: "0xtx", Side: "BUY"}); got != "" {
		t.Fatalf("NormalizeSignalID(missing asset) = %q, want empty", got)
	}
}

func TestPrepareAndMergeTrades(t *testing.T) {
	publicTrades := PrepareTrades([]api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Asset: "asset-a", Timestamp: 1002, TransactionHash: "0xtx"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Asset: "asset-b", Timestamp: 1003, TransactionHash: "0xtx", Source: "mempool"},
	}, "public")
	if publicTrades[0].Source != "public" {
		t.Fatalf("PrepareTrades should set missing source, got %q", publicTrades[0].Source)
	}
	if publicTrades[0].SignalID != "0xtx:asset-a:BUY" {
		t.Fatalf("PrepareTrades should normalize signal id, got %q", publicTrades[0].SignalID)
	}
	if publicTrades[1].Source != "mempool" {
		t.Fatalf("PrepareTrades should preserve existing source, got %q", publicTrades[1].Source)
	}

	watcherTrades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx", Source: "mempool", SignalID: "0xtx:asset-a:BUY"},
	}
	merged := MergeTrades(watcherTrades, publicTrades[:1])
	if len(merged) != 1 {
		t.Fatalf("MergeTrades cross-source dedupe len = %d, want 1", len(merged))
	}
	if merged[0].Source != "mempool" {
		t.Fatalf("MergeTrades should keep first-source trade, got %q", merged[0].Source)
	}

	identicalPublic := MergeTrades(publicTrades[:1], publicTrades[:1])
	if len(identicalPublic) != 2 {
		t.Fatalf("MergeTrades same-source fills len = %d, want 2", len(identicalPublic))
	}
}

func TestBootstrapAndRetryHelpers(t *testing.T) {
	if got := BootstrapStartTimestamp(time.Unix(1500, 0)); got != 1500 {
		t.Fatalf("BootstrapStartTimestamp(exact) = %d, want 1500", got)
	}
	startedAt := time.Unix(1500, 250_000_000)
	if got := BootstrapStartTimestamp(startedAt); got != 1499 {
		t.Fatalf("BootstrapStartTimestamp(sub-second) = %d, want 1499", got)
	}

	if !BootstrapAcceptsTrade(startedAt, 20*time.Second, api.PublicTrade{Timestamp: 1500}) {
		t.Fatal("expected trade at or after bootstrap boundary to be accepted")
	}
	if BootstrapAcceptsTrade(startedAt, 20*time.Second, api.PublicTrade{Timestamp: 1498, Source: "public"}) {
		t.Fatal("expected pre-start public trade to be rejected")
	}
	if !BootstrapAcceptsTrade(startedAt, 20*time.Second, api.PublicTrade{Timestamp: 1481, Source: "onchain"}) {
		t.Fatal("expected recent onchain watcher trade before start to be accepted")
	}
	if BootstrapAcceptsTrade(startedAt, 20*time.Second, api.PublicTrade{Timestamp: 1479, Source: "mempool"}) {
		t.Fatal("expected stale watcher trade before retry window to be rejected")
	}
	if !BootstrapAcceptsTrade(startedAt, 20*time.Second, api.PublicTrade{Timestamp: 1490, ObservedAt: 1501, Source: "onchain"}) {
		t.Fatal("expected ObservedAt after start to win over block timestamp")
	}

	now := time.Unix(5000, 0)
	if !RetrySignalFresh(now, 20*time.Second, api.PublicTrade{Timestamp: 0}) {
		t.Fatal("expected timeless retry signal to stay fresh")
	}
	if !RetrySignalFresh(now, 20*time.Second, api.PublicTrade{Timestamp: 5005}) {
		t.Fatal("expected future-dated retry signal to stay fresh")
	}
	if !RetrySignalFresh(now, 20*time.Second, api.PublicTrade{Timestamp: 4981}) {
		t.Fatal("expected recent retry signal to stay fresh")
	}
	if RetrySignalFresh(now, 20*time.Second, api.PublicTrade{Timestamp: 4979}) {
		t.Fatal("expected stale retry signal to be dropped")
	}
}

func TestRateLimitDetection(t *testing.T) {
	if !IsRateLimited(errors.New("get public trades failed with status 429: error code: 1015")) {
		t.Fatal("expected combined 429/1015 error to be rate limited")
	}
	if IsRateLimited(errors.New("context deadline exceeded")) {
		t.Fatal("expected non-rate-limit error not to match")
	}
	if got := RateLimitBackoff(0); got != 0 {
		t.Fatalf("RateLimitBackoff(0) = %v, want 0", got)
	}
	if got := RateLimitBackoff(1); got != time.Second {
		t.Fatalf("RateLimitBackoff(1) = %v, want 1s", got)
	}
	if got := RateLimitBackoff(5); got != 8*time.Second {
		t.Fatalf("RateLimitBackoff(5) = %v, want 8s", got)
	}
}

func TestRetryQueueHelpers(t *testing.T) {
	now := time.Unix(5000, 0)
	retries := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 1, Timestamp: now.Add(-25 * time.Second).Unix()},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 1, Timestamp: now.Add(-5 * time.Second).Unix()},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 1, Timestamp: 0},
	}
	fresh := TakeRetryTrades(retries, now, 20*time.Second)
	if len(fresh) != 2 {
		t.Fatalf("TakeRetryTrades len = %d, want 2", len(fresh))
	}

	queued := QueueRetryTrades(nil, retries, 2)
	if len(queued) != 2 {
		t.Fatalf("QueueRetryTrades initial len = %d, want 2", len(queued))
	}
	if queued[0].Timestamp != retries[1].Timestamp || queued[1].Timestamp != retries[2].Timestamp {
		t.Fatalf("QueueRetryTrades kept wrong entries: %#v", queued)
	}

	more := []api.PublicTrade{
		{Timestamp: 10},
		{Timestamp: 11},
	}
	queued = QueueRetryTrades(queued, more, 3)
	if len(queued) != 3 {
		t.Fatalf("QueueRetryTrades append len = %d, want 3", len(queued))
	}
	if queued[0].Timestamp != retries[2].Timestamp || queued[1].Timestamp != 10 || queued[2].Timestamp != 11 {
		t.Fatalf("QueueRetryTrades append order wrong: %#v", queued)
	}
}

func TestFreshTradesOptions(t *testing.T) {
	now := time.Unix(2000, 0)
	baseTrades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 0.005, Timestamp: 1001, TransactionHash: "0xdust"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Timestamp: 1002, TransactionHash: "0xfull"},
		{ConditionID: "cond-2", Outcome: "Down", Side: "BUY", Size: 4, Timestamp: 1003, TransactionHash: "0xother"},
	}

	paperState := &FreshTradeState{
		StartedAt:          time.Unix(1000, 0),
		SeenTradeKeys:      make(map[string]time.Time),
		SeenTradeKeysCount: make(map[string]int),
	}
	paperFresh := FreshTrades(paperState, baseTrades, FreshTradeOptions{
		Now:                     now,
		ConditionID:             "cond-1",
		MinSize:                 0.01,
		DropBelowMinBeforeDedup: true,
		BootstrapMaxAge:         20 * time.Second,
	})
	if len(paperFresh) != 1 || paperFresh[0].TransactionHash != "0xfull" {
		t.Fatalf("paper-style FreshTrades = %#v, want only full trade", paperFresh)
	}
	if len(FreshTrades(paperState, baseTrades, FreshTradeOptions{
		Now:                     now.Add(time.Second),
		ConditionID:             "cond-1",
		MinSize:                 0.01,
		DropBelowMinBeforeDedup: true,
		BootstrapMaxAge:         20 * time.Second,
	})) != 0 {
		t.Fatal("paper-style FreshTrades should stay deduped on repeated poll")
	}

	realState := &FreshTradeState{
		StartedAt:          time.Unix(1000, 0),
		SeenTradeKeys:      make(map[string]time.Time),
		SeenTradeKeysCount: make(map[string]int),
	}
	realFresh := FreshTrades(realState, baseTrades, FreshTradeOptions{
		Now:             now,
		ConditionID:     "cond-1",
		MinSize:         0.01,
		AllowBelowMin:   true,
		BootstrapMaxAge: 20 * time.Second,
	})
	if len(realFresh) != 2 {
		t.Fatalf("real-style FreshTrades len = %d, want 2", len(realFresh))
	}
	if realFresh[0].TransactionHash != "0xdust" || realFresh[1].TransactionHash != "0xfull" {
		t.Fatalf("real-style FreshTrades order wrong: %#v", realFresh)
	}
}

func TestTargetDeltaAndPositionSignals(t *testing.T) {
	state := &PositionState{
		TargetShares:         make(map[string]float64),
		TargetSeen:           make(map[string]bool),
		LastTargetPoll:       make(map[string]time.Time),
		PendingSellTarget:    make(map[string]float64),
		PendingSellPoll:      make(map[string]time.Time),
		ObservedBuySizeSum:   make(map[string]float64),
		ObservedBuySizeCount: make(map[string]int),
	}
	t0 := time.Unix(1000, 0)

	if delta, ready, pending := TargetDelta(state, "Up", 25, t0); ready || pending || delta != 0 {
		t.Fatalf("initial TargetDelta = %.4f ready=%v pending=%v, want 0 false false", delta, ready, pending)
	}
	if delta, ready, pending := TargetDelta(state, "Up", 28.5, t0.Add(2*time.Second)); !ready || pending || delta != 3.5 {
		t.Fatalf("increase TargetDelta = %.4f ready=%v pending=%v, want 3.5 true false", delta, ready, pending)
	}
	if delta, ready, pending := TargetDelta(state, "Up", 26, t0.Add(4*time.Second)); ready || !pending || delta != 0 {
		t.Fatalf("first lower TargetDelta = %.4f ready=%v pending=%v, want 0 false true", delta, ready, pending)
	}
	if delta, ready, pending := TargetDelta(state, "Up", 26, t0.Add(6*time.Second)); !ready || pending || delta != -2.5 {
		t.Fatalf("confirmed lower TargetDelta = %.4f ready=%v pending=%v, want -2.5 true false", delta, ready, pending)
	}

	ObserveBuySignal(state, api.PublicTrade{Outcome: "Up", Side: "BUY", Size: 10, Timestamp: 1001})
	ObserveBuySignal(state, api.PublicTrade{Outcome: "Up", Side: "BUY", Size: 10, Timestamp: 1002})
	signals := EstimatedPositionBuySignals(state, "cond-1", "Up", 50, "usdc")
	if len(signals) != 5 {
		t.Fatalf("EstimatedPositionBuySignals len = %d, want 5", len(signals))
	}
	total := 0.0
	for _, sig := range signals {
		total += sig.Size
		if sig.Source != "position-estimate" {
			t.Fatalf("EstimatedPositionBuySignals source = %q, want position-estimate", sig.Source)
		}
	}
	if total < 49.99 || total > 50.01 {
		t.Fatalf("EstimatedPositionBuySignals total = %.6f, want 50", total)
	}
}

func TestPositionSyncTrades(t *testing.T) {
	state := &PositionState{
		TargetShares:         make(map[string]float64),
		TargetSeen:           make(map[string]bool),
		LastTargetPoll:       make(map[string]time.Time),
		PendingSellTarget:    make(map[string]float64),
		PendingSellPoll:      make(map[string]time.Time),
		ObservedBuySizeSum:   make(map[string]float64),
		ObservedBuySizeCount: make(map[string]int),
	}
	t0 := time.Unix(1000, 0)

	trades, deltas := PositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		[]api.Position{{ConditionID: "cond-1", Outcome: "Up", Size: 5.51}},
		t0,
		nil,
		core.CopytradeSizingModePercent,
	)
	if len(trades) != 0 || len(deltas) != 0 {
		t.Fatalf("initial PositionSyncTrades should seed only, got trades=%d deltas=%d", len(trades), len(deltas))
	}

	trades, deltas = PositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		nil,
		t0.Add(2*time.Second),
		nil,
		core.CopytradeSizingModePercent,
	)
	if len(trades) != 0 || len(deltas) != 0 {
		t.Fatalf("first lower PositionSyncTrades should wait, got trades=%d deltas=%d", len(trades), len(deltas))
	}

	trades, deltas = PositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		nil,
		t0.Add(4*time.Second),
		nil,
		core.CopytradeSizingModePercent,
	)
	if len(trades) != 1 || trades[0].Side != "SELL" || trades[0].Source != "position" {
		t.Fatalf("PositionSyncTrades fallback sell = %#v", trades)
	}
	if got := deltas["Up"]; got > -5.50 || got < -5.52 {
		t.Fatalf("PositionSyncTrades delta = %.4f, want about -5.51", got)
	}
}
