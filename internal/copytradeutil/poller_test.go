package copytradeutil

import (
	"context"
	"errors"
	"testing"
	"time"

	"Market-bot/internal/api"
)

type stubActivitySnapshotFetcher struct {
	snapshot api.PublicActivitySnapshot
	calls    int
}

func (s *stubActivitySnapshotFetcher) GetPublicActivitySnapshotWithFallback(context.Context, string, []string, int, float64, int, []api.Position, bool, time.Duration, time.Duration) api.PublicActivitySnapshot {
	s.calls++
	return s.snapshot
}

func TestNewPollerState(t *testing.T) {
	state := NewPollerState(" 0xabc ", []string{" cond-1 ", "", "cond-2", "cond-1"})
	if state == nil {
		t.Fatal("expected poller state")
	}
	if state.Wallet != "0xabc" {
		t.Fatalf("wallet = %q, want 0xabc", state.Wallet)
	}
	if len(state.ConditionIDs) != 2 || state.ConditionIDs[0] != "cond-1" || state.ConditionIDs[1] != "cond-2" {
		t.Fatalf("condition ids = %#v", state.ConditionIDs)
	}
	if got := NewPollerState("   ", nil); got != nil {
		t.Fatalf("expected empty wallet to return nil, got %#v", got)
	}
}

func TestCachedSnapshotFiltersByCondition(t *testing.T) {
	state := &PollerState{
		LastSnapshot: api.PublicActivitySnapshot{
			Trades: []api.PublicTrade{
				{ConditionID: "cond-1", Outcome: "Up"},
				{ConditionID: "cond-2", Outcome: "Down"},
			},
			Positions: []api.Position{
				{ConditionID: "cond-1", Outcome: "Up", Size: 1},
				{ConditionID: "cond-2", Outcome: "Down", Size: 1},
			},
			TradesErr:    errors.New("trade err"),
			PositionsErr: errors.New("position err"),
		},
		LastPollStartedAt:      time.Unix(100, 0),
		LastPoll:               time.Unix(101, 0),
		LastPositionsRefreshAt: time.Unix(102, 0),
	}

	got := CachedSnapshot(state, "cond-1")
	if len(got.Trades) != 1 || got.Trades[0].ConditionID != "cond-1" {
		t.Fatalf("trades = %#v", got.Trades)
	}
	if len(got.Positions) != 1 || got.Positions[0].ConditionID != "cond-1" {
		t.Fatalf("positions = %#v", got.Positions)
	}
	if got.TradesErr == nil || got.PositionsErr == nil {
		t.Fatal("expected cached snapshot errors to be preserved")
	}
}

func TestSnapshotForConditionFetchesAndCaches(t *testing.T) {
	state := NewPollerState("0xabc", []string{"cond-1", "cond-2"})
	fetcher := &stubActivitySnapshotFetcher{
		snapshot: api.PublicActivitySnapshot{
			Trades: []api.PublicTrade{
				{ConditionID: "cond-1", Outcome: "Up", Size: 2},
				{ConditionID: "cond-2", Outcome: "Down", Size: 3},
			},
			Positions: []api.Position{
				{ConditionID: "cond-1", Outcome: "Up", Size: 1},
				{ConditionID: "cond-2", Outcome: "Down", Size: 4},
			},
		},
	}

	got, err := SnapshotForCondition(context.Background(), state, fetcher, 10*time.Second, "cond-1")
	if err != nil {
		t.Fatalf("SnapshotForCondition returned error: %v", err)
	}
	if fetcher.calls != 1 {
		t.Fatalf("fetch calls = %d, want 1", fetcher.calls)
	}
	if len(got.Trades) != 1 || got.Trades[0].ConditionID != "cond-1" {
		t.Fatalf("filtered trades = %#v", got.Trades)
	}
	if len(got.Positions) != 1 || got.Positions[0].ConditionID != "cond-1" {
		t.Fatalf("filtered positions = %#v", got.Positions)
	}

	got, err = SnapshotForCondition(context.Background(), state, fetcher, 10*time.Second, "cond-1")
	if err != nil {
		t.Fatalf("SnapshotForCondition cached returned error: %v", err)
	}
	if fetcher.calls != 1 {
		t.Fatalf("expected cached snapshot reuse, fetch calls = %d", fetcher.calls)
	}
	if len(got.Trades) != 1 || got.Trades[0].ConditionID != "cond-1" {
		t.Fatalf("cached filtered trades = %#v", got.Trades)
	}
}

func TestSnapshotForConditionBacksOffAfterRateLimit(t *testing.T) {
	state := NewPollerState("0xabc", []string{"cond-1"})
	fetcher := &stubActivitySnapshotFetcher{
		snapshot: api.PublicActivitySnapshot{
			TradesErr: errors.New("get public trades failed with status 429: error code: 1015"),
		},
	}

	if _, err := SnapshotForCondition(context.Background(), state, fetcher, time.Second, "cond-1"); err != nil {
		t.Fatalf("SnapshotForCondition rate-limit returned error: %v", err)
	}
	if fetcher.calls != 1 {
		t.Fatalf("fetch calls = %d, want 1", fetcher.calls)
	}
	if state.RateLimitUntil.IsZero() {
		t.Fatal("expected rate-limit backoff to be set")
	}

	if _, err := SnapshotForCondition(context.Background(), state, fetcher, time.Second, "cond-1"); err != nil {
		t.Fatalf("SnapshotForCondition cached under rate-limit returned error: %v", err)
	}
	if fetcher.calls != 1 {
		t.Fatalf("expected cached snapshot during rate-limit, fetch calls = %d", fetcher.calls)
	}
}
