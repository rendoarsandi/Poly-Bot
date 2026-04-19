package paper

import (
	"testing"
	"time"
)

func TestParseEndTimeFromSlugSupportsHumanReadableHourlySlug(t *testing.T) {
	endTime, err := ParseEndTimeFromSlug("bitcoin-up-or-down-april-19-2026-2am-et")
	if err != nil {
		t.Fatalf("expected hourly slug parse to succeed, got %v", err)
	}

	want := time.Date(2026, time.April, 19, 7, 0, 0, 0, time.UTC)
	if !endTime.Equal(want) {
		t.Fatalf("expected end time %s, got %s", want, endTime)
	}
}
