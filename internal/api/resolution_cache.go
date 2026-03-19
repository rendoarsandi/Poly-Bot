package api

import (
	"context"
	"sync"
	"time"
)

// ResolutionStatus holds the on-chain resolution state of a market condition.
type ResolutionStatus struct {
	ConditionID string
	Resolved    bool   // payoutDenominator > 0 on-chain
	Winner      string // Winning outcome (from CLOB API, if known)
	CheckedAt   time.Time
	Error       error // last error if any
}

// ResolutionCache provides thread-safe, rate-limited lookups of market resolution status.
// It caches results to avoid spamming on-chain RPC calls.
//
// Resolution checking strategy:
//   - First check uses the CLOB REST API (GetMarketInfo), which is fast and free.
//   - If CLOB says closed + has a winner → done, cache it for a long time.
//   - If CLOB says not-closed, AND market is past end time → check on-chain.
//   - Results are cached with a TTL that increases based on how many times we've
//     checked without finding resolution (exponential backoff).
type ResolutionCache struct {
	mu      sync.RWMutex
	entries map[string]*cacheEntry

	polygon    *PolygonClient
	clobClient ExchangeClient // for GetMarketInfo
	restClient *RestClient    // for fallback
}

type cacheEntry struct {
	status     ResolutionStatus
	checkCount int           // how many times we've checked this condition
	ttl        time.Duration // current TTL (increases with each miss)
}

const (
	resolutionMinTTL      = 30 * time.Second // Initial check interval
	resolutionMaxTTL      = 5 * time.Minute  // Max check interval for unresolved markets
	resolutionResolvedTTL = 24 * time.Hour   // Cache resolved status for a long time
)

// NewResolutionCache creates a new cache.
// polygon may be nil (for paperbot which doesn't do on-chain checks).
// clobClient may be nil (uses restClient fallback).
func NewResolutionCache(polygon *PolygonClient, clobClient ExchangeClient, restClient *RestClient) *ResolutionCache {
	return &ResolutionCache{
		entries:    make(map[string]*cacheEntry),
		polygon:    polygon,
		clobClient: clobClient,
		restClient: restClient,
	}
}

// GetResolution returns the cached resolution status for a condition ID.
// If the cache entry is stale or missing, it fetches fresh data.
// This method never blocks for long — if the network call fails, it returns
// the last known status (or empty/unresolved).
func (rc *ResolutionCache) GetResolution(ctx context.Context, conditionID string, outcomes []string, marketEndTime time.Time) ResolutionStatus {
	rc.mu.RLock()
	entry, exists := rc.entries[conditionID]
	rc.mu.RUnlock()

	if exists {
		// If resolved, return cached forever
		if entry.status.Resolved && entry.status.Winner != "" {
			return entry.status
		}
		// If not stale yet, return cached
		if time.Since(entry.status.CheckedAt) < entry.ttl {
			return entry.status
		}
	}

	// Don't check if market hasn't ended yet (no point)
	if time.Now().Before(marketEndTime) {
		return ResolutionStatus{
			ConditionID: conditionID,
			Resolved:    false,
			CheckedAt:   time.Now(),
		}
	}

	// Fetch fresh status in-line (with short timeout to avoid blocking)
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	status := rc.fetchResolution(checkCtx, conditionID, outcomes)

	// Update cache
	rc.mu.Lock()
	if !exists {
		entry = &cacheEntry{
			ttl: resolutionMinTTL,
		}
		rc.entries[conditionID] = entry
	}
	entry.status = status
	entry.checkCount++

	if status.Resolved && status.Winner != "" {
		entry.ttl = resolutionResolvedTTL
	} else {
		// Exponential backoff: 30s, 60s, 120s, 240s, 300s (capped)
		entry.ttl = resolutionMinTTL * time.Duration(1<<min(entry.checkCount, 4))
		if entry.ttl > resolutionMaxTTL {
			entry.ttl = resolutionMaxTTL
		}
	}
	rc.mu.Unlock()

	return status
}

// fetchResolution does the actual network calls to determine resolution status.
func (rc *ResolutionCache) fetchResolution(ctx context.Context, conditionID string, outcomes []string) ResolutionStatus {
	status := ResolutionStatus{
		ConditionID: conditionID,
		CheckedAt:   time.Now(),
	}

	// Step 1: Try CLOB API first (fast, free)
	if rc.clobClient != nil {
		info, err := rc.clobClient.GetMarketInfo(ctx, conditionID)
		if err == nil {
			if info.Closed {
				// Market is closed — check for winner
				for _, token := range info.Tokens {
					if token.Winner {
						status.Resolved = true
						status.Winner = token.Outcome
						return status
					}
				}
				// Closed but no winner tagged yet (might be settling)
				status.Resolved = true
			}
			// If market is not closed per API, still check on-chain if past end time
		}
	}

	// Step 2: Try REST API fallback for market info
	if !status.Resolved && rc.restClient != nil {
		// Use a REST market info query via the Gamma API
		info, err := rc.resolveViaREST(ctx, conditionID, outcomes)
		if err == nil && info != nil {
			if info.Closed {
				for _, token := range info.Tokens {
					if token.Winner {
						status.Resolved = true
						status.Winner = token.Outcome
						return status
					}
				}
				status.Resolved = true
			}
		}
	}

	// Step 3: Check on-chain (only if we have a polygon client)
	if rc.polygon != nil {
		resolved, err := rc.polygon.IsMarketResolved(ctx, conditionID)
		if err != nil {
			status.Error = err
			return status
		}
		if resolved {
			status.Resolved = true
			if status.Winner == "" {
				if winner, winErr := rc.polygon.GetWinningOutcome(ctx, conditionID, outcomes); winErr != nil {
					status.Error = winErr
				} else if winner != "" {
					status.Winner = winner
				}
			}
			// On-chain says resolved but we still don't know the winner.
			// Try CLOB once more before returning an empty winner.
			if status.Winner == "" && rc.clobClient != nil {
				if info, err := rc.clobClient.GetMarketInfo(ctx, conditionID); err == nil {
					for _, token := range info.Tokens {
						if token.Winner {
							status.Winner = token.Outcome
							break
						}
					}
				}
			}
		}
	}

	return status
}

// resolveViaREST tries to get market info via REST client's GetMarket endpoint.
// This is the fallback path when clobClient is nil (e.g., paperbot).
// Note: REST GetMarket only provides Active/Closed state, not winner info.
func (rc *ResolutionCache) resolveViaREST(ctx context.Context, conditionID string, outcomes []string) (*MarketInfo, error) {
	if rc.restClient == nil {
		return nil, nil
	}
	// RestClient.GetMarket returns a Market struct; convert to MarketInfo
	market, err := rc.restClient.GetMarket(ctx, conditionID)
	if err != nil {
		return nil, err
	}
	info := &MarketInfo{
		ConditionID: market.ConditionID,
		Active:      market.Active,
		Closed:      market.Closed,
	}
	// REST Token doesn't have Winner field — only outcome/tokenID.
	// Winner detection requires CLOB API or on-chain check.
	for _, token := range market.Tokens {
		info.Tokens = append(info.Tokens, struct {
			TokenID string      `json:"token_id"`
			Outcome string      `json:"outcome"`
			Winner  bool        `json:"winner"`
			Price   interface{} `json:"price"`
		}{
			TokenID: token.TokenID,
			Outcome: token.Outcome,
			Winner:  false, // REST doesn't provide winner info
		})
	}
	return info, nil
}

// IsResolved is a convenience method that returns just the resolved bool.
func (rc *ResolutionCache) IsResolved(ctx context.Context, conditionID string, outcomes []string, marketEndTime time.Time) bool {
	return rc.GetResolution(ctx, conditionID, outcomes, marketEndTime).Resolved
}

// GetWinner returns the winning outcome if the market is resolved, or empty string.
func (rc *ResolutionCache) GetWinner(ctx context.Context, conditionID string, outcomes []string, marketEndTime time.Time) string {
	return rc.GetResolution(ctx, conditionID, outcomes, marketEndTime).Winner
}

// ForceRefresh clears the cache entry for a condition ID, forcing the next
// GetResolution call to fetch fresh data.
func (rc *ResolutionCache) ForceRefresh(conditionID string) {
	rc.mu.Lock()
	delete(rc.entries, conditionID)
	rc.mu.Unlock()
}
