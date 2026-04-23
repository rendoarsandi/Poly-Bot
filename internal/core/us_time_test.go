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

func TestIsLocalWeekday(t *testing.T) {
	loc := LocalLocation()

	fridayJakarta := time.Date(2026, time.March, 20, 12, 0, 0, 0, loc)
	if !IsLocalWeekday(fridayJakarta) {
		t.Fatalf("expected Friday in Jakarta timezone to be a weekday: %s", fridayJakarta)
	}

	saturdayJakarta := time.Date(2026, time.March, 21, 12, 0, 0, 0, loc)
	if IsLocalWeekday(saturdayJakarta) {
		t.Fatalf("expected Saturday in Jakarta timezone to be weekend: %s", saturdayJakarta)
	}
}

func TestNormalizeJakartaTradingHoursWindow(t *testing.T) {
	tests := map[string]string{
		"8:00-17:30":       "08:00-17:30",
		"08.00-17.30":      "08:00-17:30",
		"wib 0800-1730":    "08:00-17:30",
		"jakarta 9:05-4:0": "09:05-04:00",
	}

	for input, want := range tests {
		got, ok := NormalizeJakartaTradingHoursWindow(input)
		if !ok || got != want {
			t.Fatalf("NormalizeJakartaTradingHoursWindow(%q) = %q, %v; want %q, true", input, got, ok, want)
		}
	}
}

func TestIsTradingHourOpenUsesJakartaTime(t *testing.T) {
	loc := LocalLocation()
	openAt := time.Date(2026, time.April, 23, 9, 0, 0, 0, loc)
	if !IsTradingHourOpen(openAt, "08:00-17:00") {
		t.Fatal("expected Jakarta 09:00 to be inside 08:00-17:00 window")
	}

	closedAt := time.Date(2026, time.April, 23, 18, 0, 0, 0, loc)
	if IsTradingHourOpen(closedAt, "08:00-17:00") {
		t.Fatal("expected Jakarta 18:00 to be outside 08:00-17:00 window")
	}
}

func TestIsTradingHourOpenHandlesOvernightJakartaWindow(t *testing.T) {
	loc := LocalLocation()
	if !IsTradingHourOpen(time.Date(2026, time.April, 23, 23, 0, 0, 0, loc), "22:00-04:00") {
		t.Fatal("expected Jakarta 23:00 to be inside overnight window")
	}
	if !IsTradingHourOpen(time.Date(2026, time.April, 24, 3, 59, 0, 0, loc), "22:00-04:00") {
		t.Fatal("expected Jakarta 03:59 to be inside overnight window")
	}
	if IsTradingHourOpen(time.Date(2026, time.April, 24, 12, 0, 0, 0, loc), "22:00-04:00") {
		t.Fatal("expected Jakarta noon to be outside overnight window")
	}
}

func TestIsTradingHourOpenRejectsInvalidWindow(t *testing.T) {
	if IsTradingHourOpen(time.Now(), "not-a-window") {
		t.Fatal("expected invalid custom trading-hours mode to be closed")
	}
}
