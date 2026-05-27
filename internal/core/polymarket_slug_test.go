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

func TestPolymarketTimeframeFromSlugAdditionalTimeframes(t *testing.T) {
	tests := []struct {
		slug         string
		wantTf       string
		wantDuration time.Duration
	}{
		{"btc-updown-4h-1768032000", "4h", 4 * time.Hour},
		{"eth-updown-1d-1768032000", "1d", 24 * time.Hour},
		{"sol-updown-1D-1768032000", "1d", 24 * time.Hour}, // Check case insensitivity
	}

	for _, tc := range tests {
		t.Run(tc.slug, func(t *testing.T) {
			gotTf := PolymarketTimeframeFromSlug(tc.slug)
			if gotTf != tc.wantTf {
				t.Errorf("PolymarketTimeframeFromSlug(%q) = %q, want %q", tc.slug, gotTf, tc.wantTf)
			}
			gotDuration := PolymarketWindowDurationFromSlug(tc.slug)
			if gotDuration != tc.wantDuration {
				t.Errorf("PolymarketWindowDurationFromSlug(%q) = %v, want %v", tc.slug, gotDuration, tc.wantDuration)
			}
		})
	}
}

func TestPolymarketDailyEventSlugFormattingAndParsing(t *testing.T) {
	windowStart := time.Date(2026, time.May, 27, 0, 0, 0, 0, time.UTC)
	got := PolymarketDailyEventSlug("btc", windowStart)
	want := "bitcoin-up-or-down-on-may-27-2026"
	if got != want {
		t.Fatalf("expected daily slug %q, got %q", want, got)
	}

	// Test parse/end time estimation
	endTime, err := ParsePolymarketEndTimeFromSlug(got)
	if err != nil {
		t.Fatalf("expected daily slug parse to succeed, got %v", err)
	}
	// The resolved time (noon ET on May 27) = 16:00:00 UTC on May 27
	wantEnd := time.Date(2026, time.May, 27, 16, 0, 0, 0, time.UTC)
	if !endTime.Equal(wantEnd) {
		t.Fatalf("expected end time %s, got %s", wantEnd, endTime)
	}

	if tf := PolymarketTimeframeFromSlug(got); tf != "1d" {
		t.Fatalf("expected daily timeframe, got %q", tf)
	}
	if gotDur := PolymarketWindowDurationFromSlug(got); gotDur != 24*time.Hour {
		t.Fatalf("expected 24h duration, got %v", gotDur)
	}
	if gotKey := PolymarketSeriesKeyFromSlug(got); gotKey != "bitcoin-up-or-down-1d" {
		t.Fatalf("expected daily series key, got %q", gotKey)
	}
}
