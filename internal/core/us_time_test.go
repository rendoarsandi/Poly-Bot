package core

import (
	"testing"
	"time"
)

func TestIsUSWeekday(t *testing.T) {
	loc := USMarketLocation()

	fridayUS := time.Date(2026, time.March, 20, 12, 0, 0, 0, loc)
	if !IsUSWeekday(fridayUS) {
		t.Fatalf("expected Friday in US timezone to be a weekday: %s", fridayUS)
	}

	saturdayUS := time.Date(2026, time.March, 21, 12, 0, 0, 0, loc)
	if IsUSWeekday(saturdayUS) {
		t.Fatalf("expected Saturday in US timezone to be weekend: %s", saturdayUS)
	}
}

func TestUSTimeUsesUSMarketLocation(t *testing.T) {
	utc := time.Date(2026, time.March, 20, 16, 0, 0, 0, time.UTC)
	us := USTime(utc)
	if us.Location().String() != USMarketLocation().String() {
		t.Fatalf("expected US location %s, got %s", USMarketLocation(), us.Location())
	}
	if us.Weekday() != time.Friday {
		t.Fatalf("expected converted day Friday, got %s", us.Weekday())
	}
}
