package core

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

var polymarketHourlyEventSlugPattern = regexp.MustCompile(`^([a-z0-9-]+)-up-or-down-([a-z]+)-([0-9]{1,2})-([0-9]{4})-([0-9]{1,2})(am|pm)-et$`)
var polymarketDailyEventSlugPattern = regexp.MustCompile(`^([a-z0-9-]+)-up-or-down-on-([a-z]+)-([0-9]{1,2})-([0-9]{4})$`)

// PolymarketTimeframeFromSlug returns the timeframe bucket encoded in a market
// slug. It supports both legacy timestamp slugs and the newer human-readable
// hourly/daily event slugs.
func PolymarketTimeframeFromSlug(slug string) string {
	slug = strings.ToLower(strings.TrimSpace(slug))
	for _, timeframe := range []string{"5m", "15m", "1h", "4h", "1d"} {
		if strings.Contains(slug, "-"+timeframe+"-") || strings.HasSuffix(slug, "-"+timeframe) {
			return timeframe
		}
	}
	if strings.Contains(slug, "-24h-") || strings.HasSuffix(slug, "-24h") {
		return "1d"
	}
	if _, ok := parsePolymarketHourlyEventSlug(slug); ok {
		return "1h"
	}
	if _, ok := parsePolymarketDailyEventSlug(slug); ok {
		return "1d"
	}
	return ""
}

// PolymarketWindowDurationFromSlug returns the market duration encoded in a
// slug when it can be inferred locally.
func PolymarketWindowDurationFromSlug(slug string) time.Duration {
	switch PolymarketTimeframeFromSlug(slug) {
	case "5m":
		return 5 * time.Minute
	case "15m":
		return 15 * time.Minute
	case "1h":
		return time.Hour
	case "4h":
		return 4 * time.Hour
	case "1d":
		return 24 * time.Hour
	default:
		return 0
	}
}

// ParsePolymarketEndTimeFromSlug returns the market end time derived from a
// Polymarket slug when the slug shape carries enough information.
func ParsePolymarketEndTimeFromSlug(slug string) (time.Time, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return time.Time{}, fmt.Errorf("empty slug")
	}

	if endTime, ok := parseTimestampWindowEndTime(slug); ok {
		return endTime, nil
	}

	if startTime, ok := parsePolymarketHourlyEventSlug(slug); ok {
		return startTime.Add(time.Hour).UTC(), nil
	}

	if startTime, ok := parsePolymarketDailyEventSlug(slug); ok {
		return startTime.Add(24 * time.Hour).UTC(), nil
	}

	return time.Time{}, fmt.Errorf("could not parse timestamp from slug: %s", slug)
}

// PolymarketSeriesKeyFromSlug collapses per-round slugs into a stable family
// identifier used for inventory and redemption bookkeeping.
func PolymarketSeriesKeyFromSlug(slug string) string {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		return ""
	}

	if tf := PolymarketTimeframeFromSlug(slug); tf != "" {
		if strings.Contains(slug, "-"+tf+"-") {
			parts := strings.SplitN(slug, "-"+tf+"-", 2)
			if len(parts) == 2 && parts[0] != "" {
				return parts[0] + "-" + tf
			}
		}
		if strings.HasSuffix(slug, "-"+tf) {
			return strings.TrimSuffix(slug, "-"+tf) + "-" + tf
		}
		if tf == "1h" && strings.Contains(slug, "-up-or-down-") {
			parts := strings.SplitN(slug, "-up-or-down-", 2)
			if len(parts) == 2 && parts[0] != "" {
				return parts[0] + "-up-or-down-1h"
			}
		}
		if tf == "1d" && strings.Contains(slug, "-up-or-down-on-") {
			parts := strings.SplitN(slug, "-up-or-down-on-", 2)
			if len(parts) == 2 && parts[0] != "" {
				return parts[0] + "-up-or-down-1d"
			}
		}
	}

	idx := strings.LastIndex(slug, "-")
	if idx <= 0 {
		return slug
	}
	return slug[:idx]
}

// PolymarketHourlyEventSlug formats the current live Polymarket hourly event
// slug for a given asset/window start.
func PolymarketHourlyEventSlug(asset string, windowStart time.Time) string {
	name := strings.ToLower(strings.TrimSpace(asset))
	switch name {
	case "btc", "bitcoin":
		name = "bitcoin"
	case "eth", "ethereum":
		name = "ethereum"
	case "sol", "solana":
		name = "solana"
	case "xrp", "ripple":
		name = "xrp"
	default:
		name = SanitizeString(name)
	}

	us := USTime(windowStart).Truncate(time.Hour)
	hour := us.Format("3pm")
	hour = strings.ToLower(strings.TrimLeft(hour, "0"))
	if hour == "" {
		hour = "12am"
	}

	return fmt.Sprintf(
		"%s-up-or-down-%s-%d-%d-%s-et",
		name,
		strings.ToLower(us.Month().String()),
		us.Day(),
		us.Year(),
		hour,
	)
}

// PolymarketDailyEventSlug formats the current live Polymarket daily event
// slug for a given asset/window start.
func PolymarketDailyEventSlug(asset string, windowStart time.Time) string {
	name := strings.ToLower(strings.TrimSpace(asset))
	switch name {
	case "btc", "bitcoin":
		name = "bitcoin"
	case "eth", "ethereum":
		name = "ethereum"
	case "sol", "solana":
		name = "solana"
	case "xrp", "ripple":
		name = "xrp"
	default:
		name = SanitizeString(name)
	}

	// Calculate a point that is guaranteed to be on the same calendar day in ET
	// as windowStart represents. Add 12 hours to midnight UTC.
	us := USTime(windowStart.Add(12 * time.Hour))
	return fmt.Sprintf(
		"%s-up-or-down-on-%s-%d-%d",
		name,
		strings.ToLower(us.Month().String()),
		us.Day(),
		us.Year(),
	)
}

func parseTimestampWindowEndTime(slug string) (time.Time, bool) {
	var timestamp int64
	var err error

	if len(slug) >= 10 {
		_, err = fmt.Sscanf(slug[len(slug)-10:], "%d", &timestamp)
	}
	if err != nil || timestamp < 1700000000 {
		timestamp = 0
		for i := len(slug) - 1; i >= 0; i-- {
			if slug[i] != '-' {
				continue
			}
			var candidate int64
			_, err = fmt.Sscanf(slug[i+1:], "%d", &candidate)
			if err == nil && candidate > 1700000000 {
				timestamp = candidate
				break
			}
		}
	}
	if timestamp == 0 {
		return time.Time{}, false
	}

	duration := PolymarketWindowDurationFromSlug(slug)
	if duration <= 0 {
		duration = 15 * time.Minute
	}
	return time.Unix(timestamp, 0).UTC().Add(duration), true
}

func parsePolymarketHourlyEventSlug(slug string) (time.Time, bool) {
	matches := polymarketHourlyEventSlugPattern.FindStringSubmatch(strings.ToLower(strings.TrimSpace(slug)))
	if len(matches) != 7 {
		return time.Time{}, false
	}

	month := matches[2]
	day := matches[3]
	year := matches[4]
	hour := matches[5]
	ampm := matches[6]

	if month == "" || day == "" || year == "" || hour == "" || ampm == "" {
		return time.Time{}, false
	}

	month = strings.ToUpper(month[:1]) + month[1:]
	parsed, err := time.ParseInLocation("January-2-2006-3pm", month+"-"+day+"-"+year+"-"+hour+ampm, USMarketLocation())
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

func parsePolymarketDailyEventSlug(slug string) (time.Time, bool) {
	matches := polymarketDailyEventSlugPattern.FindStringSubmatch(strings.ToLower(strings.TrimSpace(slug)))
	if len(matches) != 5 {
		return time.Time{}, false
	}

	month := matches[2]
	day := matches[3]
	year := matches[4]

	if month == "" || day == "" || year == "" {
		return time.Time{}, false
	}

	month = strings.ToUpper(month[:1]) + month[1:]
	parsed, err := time.ParseInLocation("January-2-2006", month+"-"+day+"-"+year, USMarketLocation())
	if err != nil {
		return time.Time{}, false
	}
	// Noon ET on this day is when it resolves. The window started at noon ET the previous day.
	resolvedTime := time.Date(parsed.Year(), parsed.Month(), parsed.Day(), 12, 0, 0, 0, USMarketLocation())
	return resolvedTime.Add(-24 * time.Hour).UTC(), true
}
