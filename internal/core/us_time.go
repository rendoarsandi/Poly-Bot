package core

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const USMarketTimezone = "America/New_York"
const TradingHoursModeOff = "off"
const TradingHoursModeWeekdays = "weekdays trade only"
const TradingHoursModeUSOpen = "us open only"

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

const LocalTimezone = "Asia/Jakarta"

var (
	localLocationOnce sync.Once
	localLocation     *time.Location
)

// LocalLocation returns the canonical local market timezone location (Jakarta/GMT+7).
func LocalLocation() *time.Location {
	localLocationOnce.Do(func() {
		loc, err := time.LoadLocation(LocalTimezone)
		if err != nil {
			// Fallback to GMT+7 if LoadLocation fails
			loc = time.FixedZone("WIB", 7*60*60)
		}
		localLocation = loc
	})
	return localLocation
}

// LocalTime converts a timestamp to local time (Jakarta).
func LocalTime(now time.Time) time.Time {
	if now.IsZero() {
		now = time.Now()
	}
	return now.In(LocalLocation())
}

// IsLocalWeekday reports whether now is Monday-Friday in Asia/Jakarta.
func IsLocalWeekday(now time.Time) bool {
	day := LocalTime(now).Weekday()
	return day >= time.Monday && day <= time.Friday
}

// NormalizeTradingHoursMode returns a canonical trading-hours mode. Custom
// Jakarta windows use "HH:MM-HH:MM", for example "08:00-17:00".
func NormalizeTradingHoursMode(mode string) (string, bool) {
	trimmed := strings.TrimSpace(mode)
	canonical := strings.ToLower(strings.Join(strings.Fields(trimmed), " "))
	switch canonical {
	case "", TradingHoursModeOff, "24/7", "24x7", "always":
		return TradingHoursModeOff, true
	case TradingHoursModeWeekdays, "weekday trade only", "weekdays", "weekday":
		return TradingHoursModeWeekdays, true
	case TradingHoursModeUSOpen, "us open", "us market open", "market open":
		return TradingHoursModeUSOpen, true
	}
	return NormalizeJakartaTradingHoursWindow(trimmed)
}

// NormalizeJakartaTradingHoursWindow normalizes a Jakarta trading window to
// "HH:MM-HH:MM". It accepts ":" or "." between hour/minute and compact HHMM.
func NormalizeJakartaTradingHoursWindow(mode string) (string, bool) {
	mode = strings.TrimSpace(mode)
	lower := strings.ToLower(mode)
	if strings.HasPrefix(lower, "wib ") {
		mode = strings.TrimSpace(mode[4:])
	} else if strings.HasPrefix(lower, "jakarta ") {
		mode = strings.TrimSpace(mode[8:])
	}

	parts := strings.Split(mode, "-")
	if len(parts) != 2 {
		return "", false
	}
	startMinute, ok := parseTradingClockMinute(parts[0])
	if !ok {
		return "", false
	}
	endMinute, ok := parseTradingClockMinute(parts[1])
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%02d:%02d-%02d:%02d", startMinute/60, startMinute%60, endMinute/60, endMinute%60), true
}

func parseTradingClockMinute(value string) (int, bool) {
	value = strings.TrimSpace(strings.ReplaceAll(value, ".", ":"))
	if !strings.Contains(value, ":") && len(value) == 4 {
		value = value[:2] + ":" + value[2:]
	}
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return 0, false
	}
	hour, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, false
	}
	minute, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, false
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, false
	}
	return hour*60 + minute, true
}

// IsTradingHourOpen reports whether now is within the custom trading hour range.
// The mode string should be in the format "HH:MM-HH:MM", e.g., "08:00-17:00".
// If the format is unrecognized, it returns false.
func IsTradingHourOpen(now time.Time, mode string) bool {
	mode, ok := NormalizeTradingHoursMode(mode)
	if !ok {
		return false
	}
	switch mode {
	case TradingHoursModeOff, TradingHoursModeWeekdays, TradingHoursModeUSOpen:
		return true
	}

	parts := strings.Split(mode, "-")
	startMinute, ok := parseTradingClockMinute(parts[0])
	if !ok {
		return false
	}
	endMinute, ok := parseTradingClockMinute(parts[1])
	if !ok {
		return false
	}

	local := LocalTime(now)
	currentMinute := local.Hour()*60 + local.Minute()

	if startMinute <= endMinute {
		return currentMinute >= startMinute && currentMinute <= endMinute
	}
	// Handle overnight ranges (e.g. "22:00-04:00")
	return currentMinute >= startMinute || currentMinute <= endMinute
}
