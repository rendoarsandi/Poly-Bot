package paper

import (
	"strings"
	"sync"
)

// ShareSource identifies where shares came from
type ShareSource string

const (
	// SourceBought - shares bought from market (for MERGE strategy)
	SourceBought ShareSource = "bought"
	// SourceSplit - shares created via SPLIT (for SELL strategy)
	SourceSplit ShareSource = "split"
)

// SplitInventory tracks shares from SPLIT operations separately from bought shares
// This prevents strategy overlap:
// - Bought shares (panic buy) → only used for MERGE
// - Split shares (panic sell) → only used for SELL
type SplitInventory struct {
	mu sync.RWMutex

	// Split shares available for selling (created via SPLIT)
	// Key: "marketID:outcome" (e.g., "BTC:Up", "ETH:Down")
	splitShares map[string]float64

	// Cost basis for split shares (always $0.50 per share since 1 USDC = 1 YES + 1 NO)
	splitCostBasis map[string]float64

	// Total USDC used for splits (for P&L tracking)
	totalSplitCost float64

	// Total proceeds from selling split shares
	totalSellProceeds float64
}

// NewSplitInventory creates a new split inventory tracker
func NewSplitInventory() *SplitInventory {
	return &SplitInventory{
		splitShares:    make(map[string]float64),
		splitCostBasis: make(map[string]float64),
	}
}

// RecordSplit records shares created from a SPLIT operation
// usdcAmount is the USDC spent, which creates equal YES and NO shares
func (s *SplitInventory) RecordSplit(marketID, outcome1, outcome2 string, usdcAmount float64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Each USDC creates 1 share of each outcome
	shares := usdcAmount

	key1 := marketID + ":" + outcome1
	key2 := marketID + ":" + outcome2

	s.splitShares[key1] += shares
	s.splitShares[key2] += shares

	// Cost basis is $0.50 per share (since $1 = 1 YES + 1 NO)
	s.splitCostBasis[key1] = 0.50
	s.splitCostBasis[key2] = 0.50

	s.totalSplitCost += usdcAmount
}

// GetSplitShares returns available split shares for an outcome
func (s *SplitInventory) GetSplitShares(marketID, outcome string) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	key := marketID + ":" + outcome
	return s.splitShares[key]
}

// GetMinSplitShares returns the minimum split shares across both outcomes
// (needed for selling pairs)
func (s *SplitInventory) GetMinSplitShares(marketID, outcome1, outcome2 string) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	key1 := marketID + ":" + outcome1
	key2 := marketID + ":" + outcome2

	shares1 := s.splitShares[key1]
	shares2 := s.splitShares[key2]

	if shares1 < shares2 {
		return shares1
	}
	return shares2
}

// RecordSell records shares sold from split inventory
// Returns the profit from this sale (proceeds - cost basis)
func (s *SplitInventory) RecordSell(marketID, outcome string, shares, price float64) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := marketID + ":" + outcome

	available := s.splitShares[key]
	if shares > available {
		shares = available
	}

	if shares <= 0 {
		return 0
	}

	proceeds := shares * price
	costBasis := shares * s.splitCostBasis[key]
	profit := proceeds - costBasis

	s.splitShares[key] -= shares
	s.totalSellProceeds += proceeds

	return profit
}

// RecordMerge records split shares merged back to USDC (before expiry)
// Returns the shares actually merged
func (s *SplitInventory) RecordMerge(marketID, outcome1, outcome2 string, shares float64) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	key1 := marketID + ":" + outcome1
	key2 := marketID + ":" + outcome2

	// Can only merge the minimum of both sides
	available1 := s.splitShares[key1]
	available2 := s.splitShares[key2]

	minAvailable := available1
	if available2 < minAvailable {
		minAvailable = available2
	}

	if shares > minAvailable {
		shares = minAvailable
	}

	if shares <= 0 {
		return 0
	}

	s.splitShares[key1] -= shares
	s.splitShares[key2] -= shares

	// Merge returns $1 per pair, cost was $1 per pair, so break-even
	// Unlike selling, merging doesn't generate profit/loss - it just recovers capital
	// So we DON'T add to totalSellProceeds (that would incorrectly count as profit)

	return shares
}

// GetStats returns inventory statistics
func (s *SplitInventory) GetStats() (totalCost, totalProceeds, unrealizedValue float64) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Count remaining shares at cost basis
	for key, shares := range s.splitShares {
		costBasis := s.splitCostBasis[key]
		unrealizedValue += shares * costBasis
	}

	return s.totalSplitCost, s.totalSellProceeds, unrealizedValue
}

// GetRealizedPnL returns the realized P&L from selling split shares
func (s *SplitInventory) GetRealizedPnL() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	unrealizedCost := 0.0
	for key, shares := range s.splitShares {
		unrealizedCost += shares * s.splitCostBasis[key]
	}

	// Cost of shares sold = total cost - cost of remaining shares
	costOfSold := s.totalSplitCost - unrealizedCost

	return s.totalSellProceeds - costOfSold
}

// NeedsReplenish checks if inventory is below threshold for a given margin target
// marginTarget: e.g., 0.06 for 6% margin reserve
// minShares: minimum shares needed to capture that margin
func (s *SplitInventory) NeedsReplenish(marketID, outcome1, outcome2 string, minShares float64) bool {
	available := s.GetMinSplitShares(marketID, outcome1, outcome2)
	return available < minShares
}

// Clear removes all inventory for a market (use after market ends)
func (s *SplitInventory) Clear(marketID, outcome1, outcome2 string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key1 := marketID + ":" + outcome1
	key2 := marketID + ":" + outcome2

	delete(s.splitShares, key1)
	delete(s.splitShares, key2)
	delete(s.splitCostBasis, key1)
	delete(s.splitCostBasis, key2)
}

// Redeem removes inventory for a market at expiration and returns the payout.
// The winning outcome pays $1.00 per share. The losing outcome pays $0.
// Also returns the PnL of this redemption (payout - remaining cost basis).
func (s *SplitInventory) Redeem(marketID, winningOutcome string) (payout, pnl float64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var totalPayout float64
	var totalCost float64

	prefix := marketID + ":"
	for key, shares := range s.splitShares {
		// Only process keys for this market
		if strings.HasPrefix(key, prefix) {
			outcome := strings.TrimPrefix(key, prefix)
			cost := shares * s.splitCostBasis[key]
			totalCost += cost

			if strings.EqualFold(strings.TrimSpace(outcome), strings.TrimSpace(winningOutcome)) {
				totalPayout += shares * 1.0
			}

			// Remove from inventory
			delete(s.splitShares, key)
			delete(s.splitCostBasis, key)
		}
	}

	return totalPayout, totalPayout - totalCost
}

// ClearAll removes all inventory
func (s *SplitInventory) ClearAll() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.splitShares = make(map[string]float64)
	s.splitCostBasis = make(map[string]float64)
}

// SplitPosition represents a split inventory position for display
type SplitPosition struct {
	MarketID  string
	Outcome   string
	Shares    float64
	CostBasis float64
}

// GetAllPositions returns all split positions for display
func (s *SplitInventory) GetAllPositions() []SplitPosition {
	s.mu.RLock()
	defer s.mu.RUnlock()

	positions := make([]SplitPosition, 0, len(s.splitShares))
	for key, shares := range s.splitShares {
		if shares <= 0 {
			continue
		}
		// Parse key "marketID:outcome"
		parts := splitKey(key)
		if len(parts) != 2 {
			continue
		}
		positions = append(positions, SplitPosition{
			MarketID:  parts[0],
			Outcome:   parts[1],
			Shares:    shares,
			CostBasis: s.splitCostBasis[key],
		})
	}
	return positions
}

// splitKey splits "marketID:outcome" into parts
func splitKey(key string) []string {
	return strings.SplitN(key, ":", 2)
}
