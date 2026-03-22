package core

import (
	"sync"
	"time"
)

const USMarketTimezone = "America/New_York"

var (
	usMarketLocationOnce sync.Once
	usMarketLocation     *time.Location
)

// USMarketLocation returns the canonical US market timezone location.
func USMarketLocation() *time.Location {
	usMarketLocationOnce.Do(func() {
		loc, err := time.LoadLocation(USMarketTimezone)
		if err != nil {
			// Fallback keeps behavior deterministic even on minimal systems.
			loc = time.FixedZone("US/Eastern", -5*60*60)
		}
		usMarketLocation = loc
	})
	return usMarketLocation
}

// USTime converts a timestamp to America/New_York.
func USTime(now time.Time) time.Time {
	if now.IsZero() {
		now = time.Now()
	}
	return now.In(USMarketLocation())
}

// IsUSWeekday reports whether now is Monday-Friday in America/New_York.
func IsUSWeekday(now time.Time) bool {
	day := USTime(now).Weekday()
	return day >= time.Monday && day <= time.Friday
}

// IsUSMarketOpen reports whether now is during regular US market hours
// (Monday-Friday, 9:30 AM to 4:00 PM Eastern Time).
func IsUSMarketOpen(now time.Time) bool {
	us := USTime(now)
	day := us.Weekday()
	if day < time.Monday || day > time.Friday {
		return false
	}
	
	hour := us.Hour()
	minute := us.Minute()
	
	// Before 9:30 AM
	if hour < 9 || (hour == 9 && minute < 30) {
		return false
	}
	
	// After 4:00 PM (16:00)
	if hour >= 16 {
		return false
	}
	
	return true
}
