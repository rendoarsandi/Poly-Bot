package paper

import "sort"

// SafetyMargin is the percentage of liquidity we consider safe to trade (100%)
const SafetyMargin = 1.00

// MarketLevel is defined in orderbook.go - reused here for liquidity calculations

// LiquidityResult holds the result of liquidity aggregation
type LiquidityResult struct {
	TotalMatchedLiquidity float64 // Total matched liquidity across all valid levels
	MaxSafeShares         float64 // 80% of matched liquidity (safe to trade)
	CumLiq1               float64 // Cumulative liquidity from side 1
	CumLiq2               float64 // Cumulative liquidity from side 2
	LevelsProcessed       int     // Number of price levels processed
}

// CalculateAggregatedLiquidity calculates the total matched liquidity across
// price levels where the combined price maintains the minimum margin.
// Returns matched liquidity at each price level until the sum exceeds maxSum.
func CalculateAggregatedLiquidity(asks1, asks2 []MarketLevel, maxSum float64) LiquidityResult {
	if len(asks1) == 0 || len(asks2) == 0 {
		return LiquidityResult{}
	}

	// Sort asks by price ascending (lowest first)
	sortedAsks1 := make([]MarketLevel, len(asks1))
	copy(sortedAsks1, asks1)
	sort.Slice(sortedAsks1, func(i, j int) bool { return sortedAsks1[i].Price < sortedAsks1[j].Price })

	sortedAsks2 := make([]MarketLevel, len(asks2))
	copy(sortedAsks2, asks2)
	sort.Slice(sortedAsks2, func(i, j int) bool { return sortedAsks2[i].Price < sortedAsks2[j].Price })

	result := LiquidityResult{}

	i, j := 0, 0
	for i < len(sortedAsks1) && j < len(sortedAsks2) {
		p1 := sortedAsks1[i].Price
		p2 := sortedAsks2[j].Price

		// Check if this combination maintains minimum margin
		if p1+p2 > maxSum {
			break // Can't go deeper, would exceed margin threshold
		}

		// Get liquidity at current levels
		liq1 := sortedAsks1[i].Size
		liq2 := sortedAsks2[j].Size

		// Matched liquidity = min of both sides (arbitrage requires equal shares)
		matchedAtLevel := liq1
		if liq2 < matchedAtLevel {
			matchedAtLevel = liq2
		}

		// Track cumulative liquidity
		result.CumLiq1 += matchedAtLevel
		result.CumLiq2 += matchedAtLevel
		result.TotalMatchedLiquidity += matchedAtLevel
		result.LevelsProcessed++

		// Move pointer on the side with less remaining liquidity
		remaining1 := liq1 - matchedAtLevel
		remaining2 := liq2 - matchedAtLevel

		if remaining1 <= 0 {
			i++
		} else {
			sortedAsks1[i].Size = remaining1
		}
		if remaining2 <= 0 {
			j++
		} else {
			sortedAsks2[j].Size = remaining2
		}
	}

	// apply 100% safety margin
	result.MaxSafeShares = result.TotalMatchedLiquidity * SafetyMargin

	return result
}

// CalculateSafeShares determines the final share count after applying
// margin scaling, compounding, and the 80% liquidity safety cap.
func CalculateSafeShares(
	baseShares float64,
	margin float64,
	compoundMultiplier float64,
	totalMatchedLiquidity float64,
	reduceSize bool,
) float64 {
	shares := baseShares

	// Apply compounding multiplier from profitable rounds
	shares = shares * compoundMultiplier

	// FINAL LIQUIDITY CAP: Ensure shares never exceed 100% of available liquidity
	// This must be checked AFTER all scaling (margin scaling + compounding)
	maxSafeShares := totalMatchedLiquidity * SafetyMargin
	if shares > maxSafeShares {
		shares = maxSafeShares
	}

	return shares
}

// MinFloat returns the smaller of two float64 values
func MinFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
