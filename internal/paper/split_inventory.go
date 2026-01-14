package paper

import (
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
	// (we're just recovering our split cost, no profit/loss)
	s.totalSellProceeds += shares // $1 per merged pair

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

	// P&L = proceeds - cost
	// But we need to account for unsold shares still being worth their cost
	return s.totalSellProceeds - s.totalSplitCost
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

// ClearAll removes all inventory
func (s *SplitInventory) ClearAll() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.splitShares = make(map[string]float64)
	s.splitCostBasis = make(map[string]float64)
}
