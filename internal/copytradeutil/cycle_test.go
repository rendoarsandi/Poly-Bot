package copytradeutil

import (
	"testing"
	"time"

	"Market-bot/internal/api"
)

type stubTradeSignalFetcher struct {
	pendingCalls int
	minedCalls   int
	pendingSince time.Time
	minedSince   time.Time
	pending      []api.PublicTrade
	mined        []api.PublicTrade
}

func (s *stubTradeSignalFetcher) PendingSignalsForCondition(string, time.Time) []api.PublicTrade {
	s.pendingCalls++
	return append([]api.PublicTrade(nil), s.pending...)
}

func (s *stubTradeSignalFetcher) MinedSignalsForCondition(_ string, since time.Time) []api.PublicTrade {
	s.minedCalls++
	s.minedSince = since
	return append([]api.PublicTrade(nil), s.mined...)
}

func TestRuntimeStateMarkPollBackdatesSince(t *testing.T) {
	state := NewRuntimeState()
	firstNow := time.Unix(1000, 0)
	if since, ok := state.MarkPoll(firstNow, 2*time.Second); !ok || !since.IsZero() {
		t.Fatalf("first mark poll = (%v, %v), want (zero, true)", since, ok)
	}
	if state.LastTradeFetch != firstNow {
		t.Fatalf("last trade fetch = %v, want %v", state.LastTradeFetch, firstNow)
	}

	if _, ok := state.MarkPoll(firstNow.Add(time.Second), 2*time.Second); ok {
		t.Fatal("expected poll inside interval to be skipped")
	}

	nextNow := firstNow.Add(3 * time.Second)
	if since, ok := state.MarkPoll(nextNow, 2*time.Second); !ok || !since.Equal(firstNow.Add(-10*time.Second)) {
		t.Fatalf("next mark poll = (%v, %v), want (%v, true)", since, ok, firstNow.Add(-10*time.Second))
	}
}

func TestRuntimeStatePollCycleMergesFreshAndRetryTrades(t *testing.T) {
	state := NewRuntimeState()
	state.StartedAt = time.Unix(900, 0)
	state.RetryTrades = []api.PublicTrade{{ConditionID: "cond-1", Outcome: "Up", Side: "SELL", Size: 1, Timestamp: 995}}
	fetcher := &stubTradeSignalFetcher{
		pending: []api.PublicTrade{{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Timestamp: 1001, SignalID: "pending-1", Source: "mempool"}},
		mined:   []api.PublicTrade{{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 3, Timestamp: 1002, SignalID: "mined-1", Source: "onchain"}},
	}
	nowValues := []time.Time{
		time.Unix(1000, 0),
		time.Unix(1000, 500_000_000),
		time.Unix(1001, 0),
	}
	nowIndex := 0
	nowFn := func() time.Time {
		if nowIndex >= len(nowValues) {
			return nowValues[len(nowValues)-1]
		}
		v := nowValues[nowIndex]
		nowIndex++
		return v
	}

	result := state.PollCycle(fetcher, "cond-1", CycleOptions{
		PollEvery:   2 * time.Second,
		RetryMaxAge: 30 * time.Second,
		Now:         nowFn,
		FreshTradeOptions: FreshTradeOptions{
			MinSize:                 0.01,
			DropBelowMinBeforeDedup: false,
			AllowBelowMin:           true,
			BootstrapMaxAge:         30 * time.Second,
		},
	})

	if fetcher.pendingCalls != 1 || fetcher.minedCalls != 1 {
		t.Fatalf("fetcher calls pending=%d mined=%d, want 1/1", fetcher.pendingCalls, fetcher.minedCalls)
	}
	if len(result.PolledTrades) != 2 {
		t.Fatalf("polled trades = %d, want 2", len(result.PolledTrades))
	}
	if len(result.FreshTrades) != 3 {
		t.Fatalf("fresh trades = %d, want 3", len(result.FreshTrades))
	}
	if result.FreshTrades[0].Side != "SELL" {
		t.Fatalf("expected retry trade first, got %+v", result.FreshTrades[0])
	}
	if state.LastError != "" {
		t.Fatalf("expected last error to clear, got %q", state.LastError)
	}
	if len(state.RetryTrades) != 0 {
		t.Fatalf("expected retry queue to drain, got %#v", state.RetryTrades)
	}
	if state.ObservedBuySizeCount["Up"] != 2 {
		t.Fatalf("expected observed buy count 2, got %d", state.ObservedBuySizeCount["Up"])
	}
}

func TestRuntimeStatePollCycleSkipsFetchInsideIntervalButReturnsRetries(t *testing.T) {
	state := NewRuntimeState()
	state.LastTradeFetch = time.Unix(1000, 0)
	state.RetryTrades = []api.PublicTrade{{ConditionID: "cond-1", Outcome: "Down", Side: "SELL", Size: 1, Timestamp: 1000}}
	fetcher := &stubTradeSignalFetcher{}

	result := state.PollCycle(fetcher, "cond-1", CycleOptions{
		PollEvery:   5 * time.Second,
		RetryMaxAge: 30 * time.Second,
		Now: func() time.Time {
			return time.Unix(1001, 0)
		},
	})

	if fetcher.pendingCalls != 0 || fetcher.minedCalls != 0 {
		t.Fatalf("expected no fetch calls, got pending=%d mined=%d", fetcher.pendingCalls, fetcher.minedCalls)
	}
	if len(result.PolledTrades) != 0 {
		t.Fatalf("expected no polled trades, got %+v", result.PolledTrades)
	}
	if len(result.FreshTrades) != 1 || result.FreshTrades[0].Outcome != "Down" {
		t.Fatalf("expected retry trade to return, got %+v", result.FreshTrades)
	}
}

func TestRuntimeStatePollCycleBackdatesFetcherSince(t *testing.T) {
	state := NewRuntimeState()
	state.StartedAt = time.Unix(900, 0)
	state.LastTradeFetch = time.Unix(1000, 0)
	fetcher := &stubTradeSignalFetcher{}

	state.PollCycle(fetcher, "cond-1", CycleOptions{
		PollEvery:   2 * time.Second,
		RetryMaxAge: 30 * time.Second,
		Now: func() time.Time {
			return time.Unix(1003, 0)
		},
	})

	wantSince := time.Unix(1000, 0).Add(-10 * time.Second)
	if !fetcher.minedSince.Equal(wantSince) {
		t.Fatalf("mined since = %v, want %v", fetcher.minedSince, wantSince)
	}
}
