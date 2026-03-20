package markets

import (
	"strings"

	"Market-bot/internal/api"
)

// ScopedMarketID returns a stable per-market identifier that still preserves the
// human-readable asset prefix. This prevents consecutive rounds of the same
// asset from sharing the same local position/pricing bucket.
func ScopedMarketID(assetID string, market *api.Market) string {
	base := strings.ToUpper(strings.TrimSpace(assetID))
	if base == "" {
		base = "UNKNOWN"
	}
	if market == nil {
		return base
	}

	fingerprint := strings.TrimSpace(market.ConditionID)
	fingerprint = strings.TrimPrefix(strings.TrimPrefix(fingerprint, "0x"), "0X")
	if fingerprint == "" {
		fingerprint = strings.TrimSpace(market.Slug)
	}
	if fingerprint == "" {
		return base
	}
	if len(fingerprint) > 8 {
		fingerprint = fingerprint[:8]
	}

	return base + "#" + strings.ToLower(fingerprint)
}
