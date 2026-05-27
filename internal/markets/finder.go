// Package markets provides shared helpers for discovering and interacting
// with Polymarket crypto binary markets across supported expiry buckets. It is imported by all cmd binaries
// so that common logic (market search, outcome extraction, level conversion)
// is maintained in one place without duplicating across paperbot / realbot / utilbot.
package markets

import (
	"context"
	"strings"
	"sync"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
)

const finderTimeframeLookupTimeout = 5000 * time.Millisecond

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

func finderGetMarketsByTimeframe(ctx context.Context, restClient *api.RestClient, assets []string, timeframe string) ([]api.Market, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, finderTimeframeLookupTimeout)
	defer cancel()
	return restClient.GetMarketsByTimeframe(lookupCtx, assets, timeframe)
}

func finderMarketTimeframeSuffix(slug string) string {
	return core.PolymarketTimeframeFromSlug(slug)
}

func finderAssetSlugAliases(asset string) []string {
	asset = strings.ToLower(strings.TrimSpace(asset))
	switch asset {
	case "btc", "bitcoin":
		return []string{"btc", "bitcoin"}
	case "eth", "ethereum":
		return []string{"eth", "ethereum"}
	case "sol", "solana":
		return []string{"sol", "solana"}
	case "xrp", "ripple":
		return []string{"xrp", "ripple"}
	default:
		if asset == "" {
			return nil
		}
		return []string{asset}
	}
}

func finderSlugMatchesAsset(slug, asset string) bool {
	slug = strings.ToLower(strings.TrimSpace(slug))
	for _, alias := range finderAssetSlugAliases(asset) {
		if alias != "" && strings.Contains(slug, alias) {
			return true
		}
	}
	return false
}

// FindMarkets polls the REST API until at least one active market matching the
// configured assets and timeframe is found, then returns a map keyed by asset
// and expiry bucket (for example "BTC-15m" or "BTC-1h").
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
		var primaryMarkets []api.Market
		var secondaryMarkets []api.Market
		var errPrimary error

		// Only copytrade needs cross-timeframe discovery coverage. Normal trading
		// should stick to the requested bucket so 5m rounds do not keep scanning
		// 15m markets as well.
		secondaryTimeframe := ""
		if strings.EqualFold(strings.TrimSpace(cfg.PaperArbMode), "copytrade") {
			if timeframe == "15m" {
				secondaryTimeframe = "5m"
			} else if timeframe == "5m" {
				secondaryTimeframe = "15m"
			}
		}

		var errSecondary error
		if secondaryTimeframe != "" {
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				primaryMarkets, errPrimary = finderGetMarketsByTimeframe(ctx, restClient, assets, timeframe)
			}()
			go func() {
				defer wg.Done()
				secondaryMarkets, errSecondary = finderGetMarketsByTimeframe(ctx, restClient, assets, secondaryTimeframe)
			}()
			wg.Wait()
		} else {
			primaryMarkets, errPrimary = finderGetMarketsByTimeframe(ctx, restClient, assets, timeframe)
		}
		if errPrimary == nil {
			markets = append(markets, primaryMarkets...)
		}
		if errSecondary == nil {
			markets = append(markets, secondaryMarkets...)
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

			if logFn != nil {
				logFn("🔍 Finder checking market: %s, EndTime: %s, TimeLeft: %s", m.Slug, endTime.Format(time.RFC3339), time.Until(endTime))
			}

			if err == nil && !endTime.IsZero() {
				if time.Now().After(endTime) {
					if logFn != nil {
						logFn("⚠️ Market %s skipped: already expired (endTime: %s)", m.Slug, endTime.Format(time.RFC3339))
					}
					continue // already expired
				}
				if time.Until(endTime) < 30*time.Second {
					if logFn != nil {
						logFn("⚠️ Market %s skipped: expiring in < 30s (%s)", m.Slug, time.Until(endTime))
					}
					continue // expiring too soon
				}
			}

			slug := strings.ToLower(m.Slug)
			// Ensure the market belongs to a supported timeframe bucket.
			// For Kalshi, we bypass this check since timeframe isn't directly in the slug this way.
			tfSuffix := finderMarketTimeframeSuffix(slug)
			isTargetTimeframe := tfSuffix != "" || restClient.Exchange == "kalshi"

			if logFn != nil {
				logFn("🔍 Market %s: tfSuffix=%s, isTargetTimeframe=%t", m.Slug, tfSuffix, isTargetTimeframe)
			}

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
					if logFn != nil {
						logFn("✅ Selected exact market for key %s: %s", key, m.Slug)
					}
					if len(found) >= maxMarkets {
						return found
					}
					break // Move to next market
				}

				// Otherwise, use the standard timeframe pattern matching
				if !isExactMatch && finderSlugMatchesAsset(slug, asset) && isTargetTimeframe {
					if tfSuffix == "" {
						tfSuffix = timeframe
					}
					key := strings.ToUpper(asset) + "-" + tfSuffix
					if _, exists := found[key]; !exists {
						mCopy := m
						found[key] = &mCopy
						if logFn != nil {
							logFn("✅ Selected market for key %s: %s (endTime: %s)", key, m.Slug, endTime.Format(time.RFC3339))
						}
						if len(found) >= maxMarkets {
							return found
						}
					} else {
						if logFn != nil {
							logFn("ℹ️ Key %s already exists, skipping market %s", key, m.Slug)
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
		logFn("⚠️ No markets found after polling")
	}
	return found
}
