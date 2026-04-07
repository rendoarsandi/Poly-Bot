package copytradeutil

import (
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
)

func TestNewRuntimeStateInitializesMaps(t *testing.T) {
	state := NewRuntimeState()
	if state == nil {
		t.Fatal("expected runtime state")
	}
	if state.StartedAt.IsZero() {
		t.Fatal("expected started time to be set")
	}
	if state.Managed == nil || state.TargetShares == nil || state.TargetSeen == nil {
		t.Fatal("expected core state maps to be initialized")
	}
	if state.PendingSellTarget == nil || state.PendingSellPoll == nil {
		t.Fatal("expected pending sell maps to be initialized")
	}
	if state.SeenTradeKeys == nil || state.SeenTradeKeysCount == nil {
		t.Fatal("expected dedupe maps to be initialized")
	}
	if state.ObservedBuySizeSum == nil || state.ObservedBuySizeCount == nil {
		t.Fatal("expected observed buy maps to be initialized")
	}
	if state.LastLogAt == nil || state.LastLogMsg == nil {
		t.Fatal("expected log throttling maps to be initialized")
	}
}

func TestShouldLogDedupesWithinInterval(t *testing.T) {
	state := NewRuntimeState()
	if !ShouldLog(state, "copytrade", "same", 5*time.Second) {
		t.Fatal("expected first log to be emitted")
	}
	if ShouldLog(state, "copytrade", "same", 5*time.Second) {
		t.Fatal("expected duplicate log inside interval to be suppressed")
	}

	state.LastLogAt["copytrade"] = state.LastLogAt["copytrade"].Add(-6 * time.Second)
	if !ShouldLog(state, "copytrade", "same", 5*time.Second) {
		t.Fatal("expected log after interval to be emitted")
	}
	if !ShouldLog(state, "copytrade", "different", 5*time.Second) {
		t.Fatal("expected changed message to be emitted immediately")
	}
}

func TestRuntimeStateSnapshotsRoundTrip(t *testing.T) {
	state := NewRuntimeState()
	state.TradesSeeded = true
	state.TargetShares["Up"] = 5
	state.TargetSeen["Up"] = true
	state.LastTargetPoll["Up"] = time.Unix(100, 0)
	state.PendingSellTarget["Down"] = 2
	state.PendingSellPoll["Down"] = time.Unix(101, 0)
	state.ObservedBuySizeSum["Up"] = 9
	state.ObservedBuySizeCount["Up"] = 3
	state.SeenTradeKeys["sig"] = time.Unix(102, 0)
	state.SeenTradeKeysCount["sig"] = 1

	positionState := state.PositionStateSnapshot()
	positionState.TargetShares["Up"] = 7
	positionState.PendingSellTarget["Down"] = 4
	positionState.ObservedBuySizeSum["Up"] = 12
	state.ApplyPositionState(positionState)

	if state.TargetShares["Up"] != 7 || state.PendingSellTarget["Down"] != 4 {
		t.Fatalf("position state round-trip failed: %#v", state)
	}
	if state.ObservedBuySizeSum["Up"] != 12 {
		t.Fatalf("observed buy size sum = %.2f, want 12", state.ObservedBuySizeSum["Up"])
	}

	freshState := state.FreshTradeStateSnapshot()
	freshState.TradesSeeded = false
	freshState.SeenTradeKeys = map[string]time.Time{"other": time.Unix(103, 0)}
	freshState.SeenTradeKeysCount = map[string]int{"other": 2}
	state.ApplyFreshTradeState(freshState)

	if state.TradesSeeded {
		t.Fatal("expected trades seeded to update from fresh trade state")
	}
	if len(state.SeenTradeKeys) != 1 || !state.SeenTradeKeys["other"].Equal(time.Unix(103, 0)) {
		t.Fatalf("unexpected seen trade keys %#v", state.SeenTradeKeys)
	}
	if len(state.SeenTradeKeysCount) != 1 || state.SeenTradeKeysCount["other"] != 2 {
		t.Fatalf("unexpected seen trade counts %#v", state.SeenTradeKeysCount)
	}
}

func TestApplyFreshTradeStatePreservesTradeBuffer(t *testing.T) {
	state := NewRuntimeState()
	state.RetryTrades = []api.PublicTrade{{SignalID: "keep"}}

	freshState := state.FreshTradeStateSnapshot()
	state.ApplyFreshTradeState(freshState)

	if len(state.RetryTrades) != 1 || state.RetryTrades[0].SignalID != "keep" {
		t.Fatalf("expected retry buffer to remain untouched, got %#v", state.RetryTrades)
	}
}

func TestRuntimeStateTargetDeltaAppliesPositionState(t *testing.T) {
	state := NewRuntimeState()
	state.TradesSeeded = true
	pollTime := time.Unix(100, 0)

	delta, ready, pending := state.TargetDelta(" Up ", 7.25, pollTime)
	if !ready || pending || delta != 7.25 {
		t.Fatalf("TargetDelta = (%.2f, %v, %v), want (7.25, true, false)", delta, ready, pending)
	}
	if !state.TargetSeen["Up"] || state.TargetShares["Up"] != 7.25 {
		t.Fatalf("target state not applied: %#v", state)
	}
}

func TestRuntimeStatePositionSyncTradesAppliesPositionState(t *testing.T) {
	state := NewRuntimeState()
	state.TradesSeeded = true
	pollTime := time.Unix(200, 0)

	trades, deltas := state.PositionSyncTrades(
		"cond-1",
		[]string{" Up "},
		[]api.Position{{ConditionID: "cond-1", Outcome: " Up ", Size: 5}},
		pollTime,
		nil,
		core.CopytradeSizingModePercent,
	)

	if len(trades) != 1 || trades[0].Outcome != "Up" || trades[0].Side != "BUY" {
		t.Fatalf("unexpected sync trades %#v", trades)
	}
	if deltas["Up"] != 5 {
		t.Fatalf("target deltas = %#v, want Up=5", deltas)
	}
	if !state.TargetSeen["Up"] || state.TargetShares["Up"] != 5 {
		t.Fatalf("position state not applied: %#v", state)
	}
}
