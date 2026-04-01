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

func requestedAssetsFromSettings(cfg paper.TUISettings) []string {
	marketSlug := strings.TrimSpace(cfg.MarketSlug)
	if marketSlug == "" || strings.EqualFold(marketSlug, "ALL") {
		return []string{"btc", "eth", "sol", "xrp"}
	}

	assets := make([]string, 0, 4)
	seen := make(map[string]struct{}, 4)
	for _, p := range strings.Split(marketSlug, ",") {
		asset := strings.ToLower(strings.TrimSpace(p))
		if asset == "" {
			continue
		}
		if _, ok := seen[asset]; ok {
			continue
		}
		seen[asset] = struct{}{}
		assets = append(assets, asset)
	}
	if len(assets) == 0 {
		return []string{"btc", "eth", "sol", "xrp"}
	}
	return assets
}

// FindMarkets polls the REST API until at least one active BTC or ETH 15-minute
// market is found, then returns a map keyed by asset (e.g. "BTC", "ETH").
//
// logFn is optional; pass nil to suppress log output (utilbot style).
// Markets that are already expired or expire in < 30 seconds are skipped.
func FindMarkets(
	ctx context.Context,
	restClient *api.RestClient,
	getConfig func() paper.TUISettings,
	logFn func(format string, args ...interface{}),
) map[string]*api.Market {
	found := make(map[string]*api.Market)

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

		cfg := getConfig()
		assets := requestedAssetsFromSettings(cfg)
		timeframe := cfg.Timeframe
		if timeframe == "" {
			timeframe = "15m"
		}
		maxMarkets := cfg.MaxMarkets
		if maxMarkets <= 0 {
			maxMarkets = 4 // Default to 4
		}

		var markets []api.Market
		var exactMarkets []api.Market

		// Fetch the primary requested timeframe first so it takes priority
		primaryMarkets, errPrimary := restClient.GetMarketsByTimeframe(ctx, assets, timeframe)
		if errPrimary == nil {
			markets = append(markets, primaryMarkets...)
		}

		// If the user didn't request a specific timeframe, or if they requested 15m/5m,
		// we fetch the other one to provide maximum coverage for copytrading.
		secondaryTimeframe := ""
		if timeframe == "15m" {
			secondaryTimeframe = "5m"
		} else if timeframe == "5m" {
			secondaryTimeframe = "15m"
		}

		var errSecondary error
		if secondaryTimeframe != "" {
			secondaryMarkets, errSec := restClient.GetMarketsByTimeframe(ctx, assets, secondaryTimeframe)
			errSecondary = errSec
			if errSecondary == nil {
				markets = append(markets, secondaryMarkets...)
			}
		}

		if errPrimary != nil && (secondaryTimeframe == "" || errSecondary != nil) {
			if attempts == 0 && logFn != nil {
				logFn("⚠️ Market fetch error: primary=%v, secondary=%v, retrying...", errPrimary, errSecondary)
			}
			// Don't immediately continue, allow exact slug fallback
		}

		// Fallback: If no markets found via timeframe logic, and the user provided a specific slug
		// that isn't a generic asset like "btc", try to fetch it as an exact slug.
		if len(markets) == 0 && len(assets) > 0 {
			for _, asset := range assets {
				// Only treat as exact slug if it's longer than a typical ticker
				if len(asset) > 5 {
					if exactMkt, exactErr := restClient.GetMarket(ctx, asset); exactErr == nil && exactMkt != nil {
						exactMarkets = append(exactMarkets, *exactMkt)
					}
				}
			}
		}

		markets = append(markets, exactMarkets...)

		for _, m := range markets {
			endTime := m.EndTime
			var err error

			// If EndTime isn't set (e.g. from an older API or simple slug), try to parse it
			if endTime.IsZero() {
				endTime, err = paper.ParseEndTimeFromSlug(m.Slug)
			}

			if err == nil && !endTime.IsZero() {
				if time.Now().After(endTime) {
					continue // already expired
				}
				if time.Until(endTime) < 30*time.Second {
					continue // expiring too soon
				}
			}

			slug := strings.ToLower(m.Slug)
			// Ensure matching for both 5m and 15m timeframes to provide maximum coverage
			// For Kalshi, we bypass this check since timeframe isn't directly in the slug like this
			isTargetTimeframe := strings.Contains(slug, "-15m-") || strings.Contains(slug, "-5m-") || restClient.Exchange == "kalshi"

			// If it's an exact market, bypass the strict name checks
			isExactMatch := false
			for _, exact := range exactMarkets {
				if strings.ToLower(exact.Slug) == slug {
					isExactMatch = true
					break
				}
			}

			for _, asset := range assets {
				// If it's an exact match, register it directly using the slug as the key
				if isExactMatch && strings.ToLower(asset) == slug {
					key := strings.ToUpper(asset)
					mCopy := m
					found[key] = &mCopy
					if len(found) >= maxMarkets {
						return found
					}
					break // Move to next market
				}

				// Otherwise, use the standard timeframe pattern matching
				if !isExactMatch && strings.Contains(slug, strings.ToLower(asset)) && isTargetTimeframe {
					tfSuffix := "15m"
					if strings.Contains(slug, "-5m-") {
						tfSuffix = "5m"
					}
					key := strings.ToUpper(asset) + "-" + tfSuffix
					if _, exists := found[key]; !exists {
						mCopy := m
						found[key] = &mCopy
						if len(found) >= maxMarkets {
							return found
						}
					}
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
