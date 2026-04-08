package markets

import (
	"strings"

	"Market-bot/internal/api"
)

// ScopedMarketID returns the full market slug when available so logs, local
// inventory, and UI state all use the canonical round identifier instead of a
// shortened asset fingerprint. If the slug is unavailable, it falls back to a
// scoped asset fingerprint to keep rounds separated.
func ScopedMarketID(assetID string, market *api.Market) string {
	base := strings.ToUpper(strings.TrimSpace(assetID))
	if base == "" {
		base = "UNKNOWN"
	}
	if market == nil {
		return base
	}

	if slug := strings.TrimSpace(market.Slug); slug != "" {
		return slug
	}

	fingerprint := strings.TrimSpace(market.ConditionID)
	fingerprint = strings.TrimPrefix(strings.TrimPrefix(fingerprint, "0x"), "0X")
	if fingerprint == "" {
		return base
	}
	if len(fingerprint) > 8 {
		fingerprint = fingerprint[:8]
	}

	return base + "#" + strings.ToLower(fingerprint)
}
