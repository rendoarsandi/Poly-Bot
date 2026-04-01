package api

import (
	"context"
	"testing"
	"time"
)

func TestResolvePolygonWSURL(t *testing.T) {
	t.Run("normalizes https fallback", func(t *testing.T) {
		got := ResolvePolygonWSURL("", "https://polygon-mainnet.g.alchemy.com/v2/key")
		want := "wss://polygon-mainnet.g.alchemy.com/v2/key"
		if got != want {
			t.Fatalf("unexpected resolved ws url %q want %q", got, want)
		}
	})

	t.Run("prefers explicit ws url", func(t *testing.T) {
		got := ResolvePolygonWSURL("wss://rpc.example/ws", "https://polygon-mainnet.g.alchemy.com/v2/key")
		want := "wss://rpc.example/ws"
		if got != want {
			t.Fatalf("unexpected resolved ws url %q want %q", got, want)
		}
	})
}

func TestPolymarketMinedWatcherPrimeTrackedMarkets(t *testing.T) {
	watcher := &PolymarketMinedWatcher{
		tokenCache: make(map[string]pendingResolvedToken),
	}
	watcher.PrimeTrackedMarkets([]*Market{
		{
			ConditionID: "cond-1",
			Slug:        "btc-updown",
			Tokens: []Token{
				{TokenID: "token-up", Outcome: "Up"},
				{TokenID: "token-down", Outcome: "Down"},
			},
		},
	})

	resolved, err := watcher.resolveToken(context.Background(), "token-down")
	if err != nil {
		t.Fatalf("resolveToken failed: %v", err)
	}
	if resolved.market.ConditionID != "cond-1" {
		t.Fatalf("unexpected condition id %q", resolved.market.ConditionID)
	}
	if resolved.market.Slug != "btc-updown" {
		t.Fatalf("unexpected slug %q", resolved.market.Slug)
	}
	if resolved.outcome != "Down" {
		t.Fatalf("unexpected outcome %q", resolved.outcome)
	}
}

func TestPolymarketMinedWatcherStoreSignalDedupes(t *testing.T) {
	watcher := &PolymarketMinedWatcher{
		seen: make(map[string]time.Time),
	}
	sig := MinedPolymarketSignal{
		SignalID: "tx:token:BUY",
		TxHash:   "0xtx",
		TokenID:  "token",
		Outcome:  "Up",
		Side:     "BUY",
		Size:     5,
	}

	if stored := watcher.storeSignal(sig); !stored {
		t.Fatalf("expected first signal to be stored")
	}
	if stored := watcher.storeSignal(sig); stored {
		t.Fatalf("expected duplicate signal to be ignored")
	}
	if len(watcher.recent) != 1 {
		t.Fatalf("unexpected recent signal count %d", len(watcher.recent))
	}
}
