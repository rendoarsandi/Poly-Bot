// Package markets provides shared helpers for discovering and interacting
// with Polymarket 15-minute binary markets. It is imported by all cmd binaries
// so that common logic (market search, outcome extraction, level conversion)
// is maintained in one place without duplicating across paperbot / realbot / utilbot.
package markets

import (
	"context"
	"strings"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/paper"
)

// FindMarkets polls the REST API until at least one active BTC or ETH 15-minute
// market is found, then returns a map keyed by asset (e.g. "BTC", "ETH").
//
// logFn is optional; pass nil to suppress log output (utilbot style).
// Markets that are already expired or expire in < 30 seconds are skipped.
func FindMarkets(
	ctx context.Context,
	restClient *api.RestClient,
	logFn func(format string, args ...interface{}),
) map[string]*api.Market {
	found := make(map[string]*api.Market)
	assets := []string{"btc", "eth"}

	// Fast polling for new markets - check every 500 ms for first 30 seconds,
	// then slow down to every 2 seconds.
	const maxFastAttempts = 60 // 30 s
	const maxSlowAttempts = 60 // 2 more minutes
	lastLogTime := time.Now()

	for attempts := 0; attempts < maxFastAttempts+maxSlowAttempts; attempts++ {
		select {
		case <-ctx.Done():
			return found
		default:
		}

		markets, err := restClient.Get15mMarkets(ctx, nil)
		if err != nil {
			if attempts == 0 && logFn != nil {
				logFn("⚠️ Market fetch error: %v, retrying...", err)
			}
			select {
			case <-ctx.Done():
				return found
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}

		for _, m := range markets {
			endTime, err := paper.ParseEndTimeFromSlug(m.Slug)
			if err == nil && time.Now().After(endTime) {
				continue // already expired
			}
			if err == nil && time.Until(endTime) < 30*time.Second {
				continue // expiring too soon
			}

			slug := strings.ToLower(m.Slug)
			is15m := strings.Contains(slug, "15m") || strings.Contains(slug, "updown")

			for _, asset := range assets {
				key := strings.ToUpper(asset)
				if _, exists := found[key]; !exists && strings.Contains(slug, asset) && is15m {
					mCopy := m
					found[key] = &mCopy
				}
			}
		}

		if len(found) > 0 {
			return found
		}

		if logFn != nil && time.Since(lastLogTime) >= 5*time.Second {
			logFn("🔍 Waiting for new markets... (%ds)", attempts/2)
			lastLogTime = time.Now()
		}

		sleepDuration := 500 * time.Millisecond
		if attempts >= maxFastAttempts {
			sleepDuration = 2 * time.Second
		}

		select {
		case <-ctx.Done():
			return found
		case <-time.After(sleepDuration):
		}
	}

	if logFn != nil {
		logFn("⚠️ No 15m markets found after polling")
	}
	return found
}
