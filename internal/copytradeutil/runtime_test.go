package copytradeutil

import (
	"context"
	"testing"

	"Market-bot/internal/api"
)

func TestConditionIDsForMarkets(t *testing.T) {
	markets := map[string]*api.Market{
		"a": {ConditionID: " cond-1 "},
		"b": nil,
		"c": {ConditionID: "cond-2"},
		"d": {ConditionID: "cond-1"},
		"e": {ConditionID: "   "},
	}

	got := ConditionIDsForMarkets(markets)
	if len(got) != 2 {
		t.Fatalf("condition id count = %d, want 2 (%#v)", len(got), got)
	}
	seen := map[string]bool{}
	for _, conditionID := range got {
		seen[conditionID] = true
	}
	if !seen["cond-1"] || !seen["cond-2"] {
		t.Fatalf("condition ids = %#v, want cond-1 and cond-2", got)
	}
}

func TestTrackedMarketsSkipsNilEntries(t *testing.T) {
	markets := map[string]*api.Market{
		"a": {ConditionID: "cond-1"},
		"b": nil,
		"c": {ConditionID: "cond-2"},
	}

	got := TrackedMarkets(markets)
	if len(got) != 2 {
		t.Fatalf("tracked market count = %d, want 2", len(got))
	}
	for _, market := range got {
		if market == nil {
			t.Fatal("tracked markets should not contain nil entries")
		}
	}
}

func TestSetupRuntimeStopsCurrentWatchersWhenWalletCleared(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	current := &WatcherSet{
		Wallet: "0xabc",
		cancel: cancel,
	}

	setup := SetupRuntime(RuntimeSetupOptions{
		ParentCtx:       context.Background(),
		CurrentWatchers: current,
		Wallet:          "   ",
	})

	if setup.Poller != nil {
		t.Fatalf("expected nil poller, got %#v", setup.Poller)
	}
	if setup.Watchers != nil {
		t.Fatalf("expected nil watchers, got %#v", setup.Watchers)
	}
	if current.cancel != nil {
		t.Fatal("expected current watchers to stop when wallet is cleared")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("expected watcher context to be canceled")
	}
}

func TestSetupRuntimeBuildsPollerAndKeepsConditionIDs(t *testing.T) {
	markets := map[string]*api.Market{
		"a": {ConditionID: "cond-1"},
		"b": {ConditionID: "cond-2"},
	}

	setup := SetupRuntime(RuntimeSetupOptions{
		ParentCtx: context.Background(),
		Wallet:    " 0xabc ",
		Markets:   markets,
	})

	if setup.Poller == nil || setup.Poller.State == nil {
		t.Fatal("expected poller state")
	}
	if setup.Poller.State.Wallet != "0xabc" {
		t.Fatalf("wallet = %q, want 0xabc", setup.Poller.State.Wallet)
	}
	if len(setup.ConditionIDs) != 2 {
		t.Fatalf("condition id count = %d, want 2", len(setup.ConditionIDs))
	}
	if len(setup.TrackedMarkets) != 2 {
		t.Fatalf("tracked market count = %d, want 2", len(setup.TrackedMarkets))
	}
	if setup.Watchers != nil {
		t.Fatalf("expected nil watchers without watcher clients, got %#v", setup.Watchers)
	}
}
