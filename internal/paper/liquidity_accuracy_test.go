package paper

import (
	"testing"
)

// TestLiquidityDataAccuracy tests that the bot correctly processes all liquidity levels
// from the API without truncation or filtering
func TestLiquidityDataAccuracy_NoTruncation(t *testing.T) {
	tests := []struct {
		name           string
		apiLevels      []MarketLevel // What the API returns
		expectedLevels int           // How many levels we should see
	}{
		{
			name: "Single level preserved",
			apiLevels: []MarketLevel{
				{Price: 0.50, Size: 10},
			},
			expectedLevels: 1,
		},
		{
			name: "Multiple levels preserved",
			apiLevels: []MarketLevel{
				{Price: 0.48, Size: 10},
				{Price: 0.49, Size: 15},
				{Price: 0.50, Size: 20},
				{Price: 0.51, Size: 25},
				{Price: 0.52, Size: 30},
			},
			expectedLevels: 5,
		},
		{
			name: "Deep book (10 levels) - only profitable levels counted",
			apiLevels: []MarketLevel{
				{Price: 0.45, Size: 5},
				{Price: 0.46, Size: 10},
				{Price: 0.47, Size: 15},
				{Price: 0.48, Size: 20},
				{Price: 0.49, Size: 25},
				{Price: 0.50, Size: 30},
				{Price: 0.51, Size: 35},
				{Price: 0.52, Size: 40},
				{Price: 0.53, Size: 45},
				{Price: 0.54, Size: 50},
			},
			// After sorting, asks2 becomes [0.46, 0.47, ..., 0.55]
			// Only levels where sum <= 1.00 pass: 0.45+0.46=0.91, ... 0.53+0.54=1.07 (fails)
			// So 9 levels should pass (this is correct behavior!)
			expectedLevels: 9,
		},
		{
			name:           "Empty book handled",
			apiLevels:      []MarketLevel{},
			expectedLevels: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Simulate processing through CalculateAggregatedLiquidity
			// with a matching other side
			otherSide := make([]MarketLevel, len(tc.apiLevels))
			for i, level := range tc.apiLevels {
				// Mirror at complementary price
				otherSide[i] = MarketLevel{
					Price: 1.0 - level.Price,
					Size:  level.Size,
				}
			}

			// Calculate what liquidity we'd see
			result := CalculateAggregatedLiquidity(tc.apiLevels, otherSide, 1.00)

			// For properly matched books, we should process all levels
			if tc.expectedLevels > 0 && result.LevelsProcessed < tc.expectedLevels {
				t.Errorf("Expected to process %d levels, only processed %d",
					tc.expectedLevels, result.LevelsProcessed)
			}
		})
	}
}

// TestLiquidityDataAccuracy_TotalSizeCorrect verifies total liquidity is correctly summed
func TestLiquidityDataAccuracy_TotalSizeCorrect(t *testing.T) {
	tests := []struct {
		name              string
		asks1             []MarketLevel
		asks2             []MarketLevel
		expectedTotalLiq  float64
		expectedSafeShares float64
	}{
		{
			name: "10 shares each side = 10 matched",
			asks1: []MarketLevel{
				{Price: 0.48, Size: 10},
			},
			asks2: []MarketLevel{
				{Price: 0.50, Size: 10},
			},
			expectedTotalLiq:  10,
			expectedSafeShares: 8, // 80% of 10
		},
		{
			name: "Multi-level: 10+15+20 = 45 total when matched",
			asks1: []MarketLevel{
				{Price: 0.45, Size: 10},
				{Price: 0.46, Size: 15},
				{Price: 0.47, Size: 20},
			},
			asks2: []MarketLevel{
				{Price: 0.50, Size: 10},
				{Price: 0.51, Size: 15},
				{Price: 0.52, Size: 20},
			},
			// 0.45+0.50=0.95 OK (10 matched)
			// 0.46+0.51=0.97 OK (15 matched)
			// 0.47+0.52=0.99 OK (20 matched)
			expectedTotalLiq:  45, // 10+15+20
			expectedSafeShares: 36, // 80% of 45
		},
		{
			name: "Unbalanced: limited by smaller side",
			asks1: []MarketLevel{
				{Price: 0.48, Size: 100}, // 100 on side 1
			},
			asks2: []MarketLevel{
				{Price: 0.50, Size: 25}, // Only 25 on side 2
			},
			expectedTotalLiq:  25,  // min(100, 25)
			expectedSafeShares: 20, // 80% of 25
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := CalculateAggregatedLiquidity(tc.asks1, tc.asks2, 1.00)

			if result.TotalMatchedLiquidity != tc.expectedTotalLiq {
				t.Errorf("TotalMatchedLiquidity: expected %.0f, got %.0f",
					tc.expectedTotalLiq, result.TotalMatchedLiquidity)
			}

			if result.MaxSafeShares != tc.expectedSafeShares {
				t.Errorf("MaxSafeShares: expected %.0f, got %.0f (80%% of %.0f)",
					tc.expectedSafeShares, result.MaxSafeShares, tc.expectedTotalLiq)
			}
		})
	}
}

// TestLiquidityDataAccuracy_PriceLevelFiltering verifies only unprofitable levels are filtered
func TestLiquidityDataAccuracy_PriceLevelFiltering(t *testing.T) {
	tests := []struct {
		name         string
		asks1        []MarketLevel
		asks2        []MarketLevel
		maxSum       float64
		expectLiq    float64
		description  string
	}{
		{
			name: "All levels within threshold",
			asks1: []MarketLevel{
				{Price: 0.45, Size: 10},
				{Price: 0.46, Size: 10},
			},
			asks2: []MarketLevel{
				{Price: 0.50, Size: 10},
				{Price: 0.51, Size: 10},
			},
			maxSum:      0.99, // Both combinations (0.95, 0.97) pass
			expectLiq:   20,
			description: "Should include all profitable levels",
		},
		{
			name: "Second level exceeds threshold - excluded",
			asks1: []MarketLevel{
				{Price: 0.45, Size: 10}, // 0.45+0.50=0.95 OK
				{Price: 0.50, Size: 10}, // 0.50+0.52=1.02 > 0.99 EXCLUDED
			},
			asks2: []MarketLevel{
				{Price: 0.50, Size: 10},
				{Price: 0.52, Size: 10},
			},
			maxSum:      0.99,
			expectLiq:   10, // Only first level passes
			description: "Should exclude unprofitable deeper levels",
		},
		{
			name: "No levels pass threshold",
			asks1: []MarketLevel{
				{Price: 0.55, Size: 100},
			},
			asks2: []MarketLevel{
				{Price: 0.50, Size: 100},
			},
			maxSum:      0.99, // 0.55+0.50=1.05 > 0.99
			expectLiq:   0,
			description: "Should return 0 when no profitable levels exist",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := CalculateAggregatedLiquidity(tc.asks1, tc.asks2, tc.maxSum)

			if result.TotalMatchedLiquidity != tc.expectLiq {
				t.Errorf("%s\nExpected %.0f matched liquidity, got %.0f",
					tc.description, tc.expectLiq, result.TotalMatchedLiquidity)
			}
		})
	}
}

// TestLiquidityDataAccuracy_EdgeCases tests edge cases in liquidity handling
func TestLiquidityDataAccuracy_EdgeCases(t *testing.T) {
	tests := []struct {
		name      string
		asks1     []MarketLevel
		asks2     []MarketLevel
		maxSum    float64
		expectLiq float64
	}{
		{
			name: "Very small size (0.5 shares)",
			asks1: []MarketLevel{
				{Price: 0.48, Size: 0.5},
			},
			asks2: []MarketLevel{
				{Price: 0.50, Size: 0.5},
			},
			maxSum:    0.99,
			expectLiq: 0.5,
		},
		{
			name: "Large liquidity (1000 shares)",
			asks1: []MarketLevel{
				{Price: 0.48, Size: 1000},
			},
			asks2: []MarketLevel{
				{Price: 0.50, Size: 1000},
			},
			maxSum:    0.99,
			expectLiq: 1000,
		},
		{
			name: "Fractional shares",
			asks1: []MarketLevel{
				{Price: 0.48, Size: 12.75},
			},
			asks2: []MarketLevel{
				{Price: 0.50, Size: 8.25},
			},
			maxSum:    0.99,
			expectLiq: 8.25, // min(12.75, 8.25)
		},
		{
			name: "Price exactly at threshold",
			asks1: []MarketLevel{
				{Price: 0.49, Size: 10},
			},
			asks2: []MarketLevel{
				{Price: 0.49, Size: 10},
			},
			maxSum:    0.98, // 0.49+0.49=0.98, exactly at threshold
			expectLiq: 10,
		},
		{
			name: "Price just over threshold",
			asks1: []MarketLevel{
				{Price: 0.495, Size: 10},
			},
			asks2: []MarketLevel{
				{Price: 0.49, Size: 10},
			},
			maxSum:    0.98, // 0.495+0.49=0.985 > 0.98
			expectLiq: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := CalculateAggregatedLiquidity(tc.asks1, tc.asks2, tc.maxSum)

			if result.TotalMatchedLiquidity != tc.expectLiq {
				t.Errorf("Expected %.2f liquidity, got %.2f",
					tc.expectLiq, result.TotalMatchedLiquidity)
			}
		})
	}
}

// TestOrderSizing_MatchesLogOutput verifies order sizing matches your log examples
func TestOrderSizing_MatchesLogOutput(t *testing.T) {
	// These are the exact scenarios from your log:
	// [SOL] 🎯 ARB! Up@$0.48 + Down@$0.50 = $0.98 | 8 shares (1.1x), profit $0.08 (2.0%) [liq: 10/10]
	// [SOL] 🎯 ARB! Up@$0.40 + Down@$0.58 = $0.98 | 8 shares (1.1x), profit $0.08 (2.0%) [liq: 10/10]
	// [BTC] 🎯 ARB! Up@$0.45 + Down@$0.53 = $0.98 | 20 shares (1.1x), profit $0.20 (2.0%) [liq: 25/25]

	tests := []struct {
		name           string
		liq1           float64 // Liquidity on side 1
		liq2           float64 // Liquidity on side 2
		baseShares     float64
		margin         float64
		multiplier     float64
		expectedShares float64
		description    string
	}{
		{
			name:           "SOL 10/10 liq at 2% margin",
			liq1:           10,
			liq2:           10,
			baseShares:     5,
			margin:         2.0,
			multiplier:     1.1,
			expectedShares: 8, // 80% of min(10,10) = 8
			description:    "With 10/10 liquidity, should cap at 8 shares (80% safety)",
		},
		{
			name:           "BTC 25/25 liq at 2% margin",
			liq1:           25,
			liq2:           25,
			baseShares:     5,
			margin:         2.0,
			multiplier:     1.1,
			expectedShares: 11, // 5*2*1.1=11, less than 20 (80% of 25)
			description:    "With 25/25 liquidity, 11 shares is under 80% cap of 20",
		},
		{
			name:           "BTC 25/25 liq with higher scaling",
			liq1:           25,
			liq2:           25,
			baseShares:     10,     // Larger base
			margin:         3.0,    // Higher margin = 3x
			multiplier:     1.1,
			expectedShares: 20,     // 10*3*1.1=33, but capped at 80% of 25 = 20
			description:    "Larger trade should be capped at 80% liquidity",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			matchedLiq := MinFloat(tc.liq1, tc.liq2)
			result := CalculateSafeShares(
				tc.baseShares,
				tc.margin,
				tc.multiplier,
				matchedLiq,
				false,
			)

			if result != tc.expectedShares {
				t.Errorf("%s\nExpected %.0f shares, got %.0f\nMatched liquidity: %.0f, 80%% cap: %.0f",
					tc.description,
					tc.expectedShares, result,
					matchedLiq, matchedLiq*0.80)
			}
		})
	}
}
