package copytradeutil

import (
	"context"
	"testing"
	"time"

	"Market-bot/internal/api"
)

func TestNewPoller(t *testing.T) {
	poller := NewPoller(" 0xabc ", []string{" cond-1 "})
	if poller == nil {
		t.Fatal("expected poller")
	}
	if poller.State == nil {
		t.Fatal("expected poller state")
	}
	if poller.State.Wallet != "0xabc" {
		t.Fatalf("wallet = %q, want 0xabc", poller.State.Wallet)
	}
	if got := NewPoller("   ", nil); got != nil {
		t.Fatalf("expected empty wallet to return nil, got %#v", got)
	}
}

func TestPendingSignalsToTrades(t *testing.T) {
	observedAt := time.Unix(1700000000, 0)
	trades := PendingSignalsToTrades([]api.PendingPolymarketSignal{{
		ObservedAt:  observedAt,
		SignalID:    "sig-1",
		TxHash:      "0xabc",
		ConditionID: "cond-1",
		Slug:        "btc-up",
		Outcome:     "Up",
		Side:        "buy",
		Size:        12.5,
	}})

	if len(trades) != 1 {
		t.Fatalf("trade count = %d, want 1", len(trades))
	}
	if trades[0].Source != "mempool" {
		t.Fatalf("source = %q, want mempool", trades[0].Source)
	}
	if trades[0].Timestamp != observedAt.Unix() || trades[0].ObservedAt != observedAt.Unix() {
		t.Fatalf("unexpected timestamps %#v", trades[0])
	}
}

func TestMinedSignalsToTrades(t *testing.T) {
	observedAt := time.Unix(1700000001, 0)
	trades := MinedSignalsToTrades([]api.MinedPolymarketSignal{{
		ObservedAt:     observedAt,
		SignalID:       "sig-2",
		TxHash:         "0xdef",
		ConditionID:    "cond-2",
		Slug:           "eth-down",
		Outcome:        "Down",
		Side:           "sell",
		Size:           7.25,
		BlockTimestamp: 1700000000,
	}})

	if len(trades) != 1 {
		t.Fatalf("trade count = %d, want 1", len(trades))
	}
	if trades[0].Source != "onchain" {
		t.Fatalf("source = %q, want onchain", trades[0].Source)
	}
	if trades[0].Timestamp != 1700000000 || trades[0].ObservedAt != observedAt.Unix() {
		t.Fatalf("unexpected timestamps %#v", trades[0])
	}
}

func TestWatcherSetAttachAndWatcherFlags(t *testing.T) {
	wallet := "0x0000000000000000000000000000000000000001"
	poller := NewPoller(wallet, []string{"cond-1"})
	watchers := &WatcherSet{
		PendingWatcher: api.NewPolymarketPendingWatcher(
			"https://polygon-mainnet.g.alchemy.com/v2/test",
			&api.RestClient{},
			&api.PolygonClient{},
			wallet,
		),
		MinedWatcher: api.NewPolymarketMinedWatcher(
			"https://polygon-mainnet.infura.io/v3/test",
			&api.PolygonClient{},
			&api.RestClient{},
			wallet,
		),
	}

	watchers.Attach(poller)

	if !HasOnchainWatcher(poller) {
		t.Fatal("expected attached watchers to count as onchain watchers")
	}
	if !HasPendingWatcher(poller) {
		t.Fatal("expected attached pending watcher to be detected")
	}
	if ShouldUsePublicActivityAPI(poller) {
		t.Fatal("expected onchain watchers to disable public activity api")
	}
}

func TestWatcherSetStopClearsCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	watchers := &WatcherSet{cancel: cancel}

	watchers.Stop()

	if watchers.cancel != nil {
		t.Fatal("expected stop to clear cancel func")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("expected stop to cancel watcher context")
	}
}

func TestEnsureWatcherSetStopsWhenWalletCleared(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	current := &WatcherSet{
		Wallet: "0xabc",
		cancel: cancel,
	}

	next := EnsureWatcherSet(context.Background(), current, "   ", "", "", nil, nil, nil, "", func(string, ...interface{}) {})

	if next != nil {
		t.Fatalf("expected nil watcher set, got %#v", next)
	}
	if current.cancel != nil {
		t.Fatal("expected cleared wallet to stop current watcher set")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("expected cleared wallet to cancel watcher context")
	}
}
