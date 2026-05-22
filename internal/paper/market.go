package paper

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"Market-bot/internal/core"
)

// ParseEndTimeFromSlug extracts end time from market slug (e.g., btc-updown-15m-1767358800)
// The slug contains the START timestamp, so we add the duration to get END time
func ParseEndTimeFromSlug(slug string) (time.Time, error) {
	if endTime, err := core.ParsePolymarketEndTimeFromSlug(slug); err == nil {
		return endTime, nil
	}

	// Legacy fallback keeps old local-only slugs working even if they do not
	// match the shared parser exactly.
	var timestamp int64
	var err error
	if len(slug) >= 10 {
		_, err = fmt.Sscanf(slug[len(slug)-10:], "%d", &timestamp)
	} else {
		err = fmt.Errorf("slug too short")
	}
	if err != nil {
		for i := len(slug) - 1; i >= 0; i-- {
			if slug[i] != '-' {
				continue
			}
			_, err = fmt.Sscanf(slug[i+1:], "%d", &timestamp)
			if err == nil && timestamp > 1700000000 {
				break
			}
		}
	}
	if timestamp == 0 {
		return time.Time{}, fmt.Errorf("could not parse timestamp from slug: %s", slug)
	}

	durationSeconds := int64(900)
	parts := strings.Split(slug, "-")
	for _, part := range parts {
		if len(part) <= 1 || (!strings.HasSuffix(part, "m") && !strings.HasSuffix(part, "h") && !strings.HasSuffix(part, "d")) {
			continue
		}
		valStr := part[:len(part)-1]
		val, parseErr := strconv.Atoi(valStr)
		if parseErr != nil {
			continue
		}
		switch part[len(part)-1] {
		case 'm':
			durationSeconds = int64(val) * 60
		case 'h':
			durationSeconds = int64(val) * 3600
		case 'd':
			durationSeconds = int64(val) * 86400
		}
		break
	}

	return time.Unix(timestamp+durationSeconds, 0), nil
}
