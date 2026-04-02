package api

import (
	"context"
	"fmt"
	"math"
	"strings"
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

	t.Run("normalizes infura fallback", func(t *testing.T) {
		got := ResolvePolygonWSURL("", "https://polygon-mainnet.infura.io/v3/key")
		want := "wss://polygon-mainnet.infura.io/ws/v3/key"
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

func TestPolymarketMinedWatcherAggregatesSplitTransferSignals(t *testing.T) {
	var logged []string
	watcher := &PolymarketMinedWatcher{
		seen:             make(map[string]time.Time),
		pendingTransfers: make(map[string]MinedPolymarketSignal),
		logf: func(format string, args ...interface{}) {
			logged = append(logged, fmt.Sprintf(format, args...))
		},
	}

	base := time.Now()
	first := MinedPolymarketSignal{
		ObservedAt:  base,
		SignalID:    "tx:token:BUY",
		TxHash:      "0xtx",
		TokenID:     "token",
		ConditionID: "cond-1",
		Slug:        "btc-updown",
		Outcome:     "Up",
		Side:        "BUY",
		Size:        27.0,
	}
	second := first
	second.ObservedAt = base.Add(10 * time.Millisecond)
	second.Size = 1.5744

	watcher.queueTransferSignal(first)
	watcher.queueTransferSignal(second)

	if len(watcher.recent) != 0 {
		t.Fatalf("expected transfer pieces to remain buffered before flush, got %d recent signals", len(watcher.recent))
	}

	watcher.flushReadyTransferSignals(base.Add(polymarketMinedTransferAggregateWindow + time.Millisecond))

	if len(watcher.recent) != 1 {
		t.Fatalf("expected 1 aggregated signal, got %d", len(watcher.recent))
	}
	if math.Abs(watcher.recent[0].Size-28.5744) > 1e-9 {
		t.Fatalf("unexpected aggregated size %.6f", watcher.recent[0].Size)
	}
	if len(logged) != 1 {
		t.Fatalf("expected 1 log line, got %d", len(logged))
	}
	if !strings.Contains(logged[0], "28.57") {
		t.Fatalf("expected aggregated log size, got %q", logged[0])
	}
}

func TestMinedWatcherSelectBlockRange(t *testing.T) {
	t.Run("bootstraps from latest head only", func(t *testing.T) {
		start, end, truncated, ok := minedWatcherSelectBlockRange(0, 123)
		if !ok {
			t.Fatal("expected valid range")
		}
		if start != 123 || end != 123 {
			t.Fatalf("unexpected range start=%d end=%d", start, end)
		}
		if truncated != 0 {
			t.Fatalf("unexpected truncation %d", truncated)
		}
	})

	t.Run("ignores already processed head", func(t *testing.T) {
		if _, _, _, ok := minedWatcherSelectBlockRange(100, 100); ok {
			t.Fatal("expected no range when head is already processed")
		}
	})

	t.Run("processes full small gap", func(t *testing.T) {
		start, end, truncated, ok := minedWatcherSelectBlockRange(100, 103)
		if !ok {
			t.Fatal("expected valid range")
		}
		if start != 101 || end != 103 {
			t.Fatalf("unexpected range start=%d end=%d", start, end)
		}
		if truncated != 0 {
			t.Fatalf("unexpected truncation %d", truncated)
		}
	})

	t.Run("caps reconnect replay window", func(t *testing.T) {
		start, end, truncated, ok := minedWatcherSelectBlockRange(100, 120)
		if !ok {
			t.Fatal("expected valid range")
		}
		if start != 113 || end != 120 {
			t.Fatalf("unexpected range start=%d end=%d", start, end)
		}
		if truncated != 12 {
			t.Fatalf("unexpected truncation %d", truncated)
		}
	})
}

func TestNormalizeCopytradeMinedWatcherMode(t *testing.T) {
	tests := map[string]string{
		"":         CopytradeMinedWatcherModeFallback,
		"fallback": CopytradeMinedWatcherModeFallback,
		"always":   CopytradeMinedWatcherModeAlways,
		"1":        CopytradeMinedWatcherModeAlways,
		"off":      CopytradeMinedWatcherModeOff,
		"0":        CopytradeMinedWatcherModeOff,
		"weird":    CopytradeMinedWatcherModeFallback,
	}
	for input, want := range tests {
		if got := NormalizeCopytradeMinedWatcherMode(input); got != want {
			t.Fatalf("NormalizeCopytradeMinedWatcherMode(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestShouldEnableCopytradeMinedWatcher(t *testing.T) {
	alchemyPending := "https://polygon-mainnet.g.alchemy.com/v2/key"
	infuraPending := "https://polygon-mainnet.infura.io/v3/key"

	if !ShouldEnableCopytradeMinedWatcher("", alchemyPending) {
		t.Fatal("expected fallback mode to keep mined watcher enabled alongside best-effort pending watcher")
	}
	if !ShouldEnableCopytradeMinedWatcher("", infuraPending) {
		t.Fatal("expected fallback mode to enable mined watcher when pending filtering is unavailable")
	}
	if !ShouldEnableCopytradeMinedWatcher("always", alchemyPending) {
		t.Fatal("expected always mode to force mined watcher on")
	}
	if ShouldEnableCopytradeMinedWatcher("off", infuraPending) {
		t.Fatal("expected off mode to disable mined watcher")
	}
}

func TestDecodeTransferSingleLog(t *testing.T) {
	tokenIDs, sizes, err := decodeTransferSingleLog("0x" + minedTestHexWord(12345) + minedTestHexWord(2500000))
	if err != nil {
		t.Fatalf("decodeTransferSingleLog failed: %v", err)
	}
	if len(tokenIDs) != 1 || tokenIDs[0] != "12345" {
		t.Fatalf("unexpected token ids %#v", tokenIDs)
	}
	if len(sizes) != 1 || math.Abs(sizes[0]-2.5) > 1e-9 {
		t.Fatalf("unexpected sizes %#v", sizes)
	}
}

func TestDecodeTransferBatchLog(t *testing.T) {
	data := "0x" +
		minedTestHexWord(64) +
		minedTestHexWord(160) +
		minedTestHexWord(2) +
		minedTestHexWord(111) +
		minedTestHexWord(222) +
		minedTestHexWord(2) +
		minedTestHexWord(1500000) +
		minedTestHexWord(2750000)

	tokenIDs, sizes, err := decodeTransferBatchLog(data)
	if err != nil {
		t.Fatalf("decodeTransferBatchLog failed: %v", err)
	}
	if len(tokenIDs) != 2 || tokenIDs[0] != "111" || tokenIDs[1] != "222" {
		t.Fatalf("unexpected token ids %#v", tokenIDs)
	}
	if len(sizes) != 2 || math.Abs(sizes[0]-1.5) > 1e-9 || math.Abs(sizes[1]-2.75) > 1e-9 {
		t.Fatalf("unexpected sizes %#v", sizes)
	}
}

func TestPolymarketMinedWatcherHandleTransferLogStoresSignal(t *testing.T) {
	target := "0x00000000000000000000000000000000000000aa"
	watcher := &PolymarketMinedWatcher{
		targetWallet: NormalizeWalletAddress(target),
		seen:         make(map[string]time.Time),
		tokenCache: map[string]pendingResolvedToken{
			"12345": {
				market:  Market{ConditionID: "cond-1", Slug: "btc-updown"},
				outcome: "Up",
			},
		},
	}

	watcher.handleTransferLog(context.Background(), polymarketTransferLog{
		Topics: []string{
			"0xc3d58168c5ae7397731d063d5bbf3d657854427343f4c083240f7aacaa2d0f62",
			polygonLogTopicAddress(CTFExchange),
			polygonLogTopicAddress("0x00000000000000000000000000000000000000bb"),
			polygonLogTopicAddress(target),
		},
		Data:            "0x" + minedTestHexWord(12345) + minedTestHexWord(3000000),
		BlockNumber:     "0x20",
		TransactionHash: "0xabc",
	})
	watcher.flushReadyTransferSignals(time.Now().Add(polymarketMinedTransferAggregateWindow + time.Millisecond))

	if len(watcher.recent) != 1 {
		t.Fatalf("expected 1 stored signal, got %d", len(watcher.recent))
	}
	sig := watcher.recent[0]
	if sig.Side != "BUY" {
		t.Fatalf("unexpected side %q", sig.Side)
	}
	if sig.TokenID != "12345" {
		t.Fatalf("unexpected token id %q", sig.TokenID)
	}
	if math.Abs(sig.Size-3.0) > 1e-9 {
		t.Fatalf("unexpected size %.6f", sig.Size)
	}
	if sig.BlockNumber != 32 {
		t.Fatalf("unexpected block number %d", sig.BlockNumber)
	}
}

func minedTestHexWord(n int64) string {
	return fmt.Sprintf("%064x", n)
}
