package core

import (
	"testing"
	"time"
)

func TestPolymarketHourlyEventSlugUsesUSEasternHour(t *testing.T) {
	windowStart := time.Date(2026, time.April, 19, 6, 0, 0, 0, time.UTC)
	got := PolymarketHourlyEventSlug("btc", windowStart)
	want := "bitcoin-up-or-down-april-19-2026-2am-et"
	if got != want {
		t.Fatalf("expected hourly slug %q, got %q", want, got)
	}
}

func TestParsePolymarketEndTimeFromHumanReadableHourlySlug(t *testing.T) {
	slug := "bitcoin-up-or-down-april-19-2026-2am-et"
	endTime, err := ParsePolymarketEndTimeFromSlug(slug)
	if err != nil {
		t.Fatalf("expected hourly slug parse to succeed, got %v", err)
	}
	want := time.Date(2026, time.April, 19, 7, 0, 0, 0, time.UTC)
	if !endTime.Equal(want) {
		t.Fatalf("expected end time %s, got %s", want, endTime)
	}
	if tf := PolymarketTimeframeFromSlug(slug); tf != "1h" {
		t.Fatalf("expected hourly timeframe, got %q", tf)
	}
	if got := PolymarketWindowDurationFromSlug(slug); got != time.Hour {
		t.Fatalf("expected 1h duration, got %v", got)
	}
	if got := PolymarketSeriesKeyFromSlug(slug); got != "bitcoin-up-or-down-1h" {
		t.Fatalf("expected hourly series key, got %q", got)
	}
}
