package paper

import (
	"testing"
)

// TestSafetyMarginConstant verifies the safety margin constant
func TestSafetyMarginConstant(t *testing.T) {
	if SafetyMargin != 1.00 {
		t.Errorf("SafetyMargin should be 1.00, got %v", SafetyMargin)
	}
}

// TestCalculateSafeShares tests that shares are scaled by compoundMultiplier and capped by liquidity
func TestCalculateSafeShares(t *testing.T) {
	tests := []struct {
		name                  string
		baseShares            float64
		compoundMultiplier    float64
		totalMatchedLiquidity float64
		expectedShares        float64
	}{
		{
			name:                  "Simple case below liquidity",
			baseShares:            10,
			compoundMultiplier:    1.0,
			totalMatchedLiquidity: 20,
			expectedShares:        10,
		},
		{
			name:                  "Compounding multiplier below liquidity",
			baseShares:            10,
			compoundMultiplier:    1.5,
			totalMatchedLiquidity: 20,
			expectedShares:        15,
		},
		{
			name:                  "Capped by liquidity",
			baseShares:            10,
			compoundMultiplier:    1.0,
			totalMatchedLiquidity: 8,
			expectedShares:        8,
		},
		{
			name:                  "Compounding capped by liquidity",
			baseShares:            10,
			compoundMultiplier:    2.0,
			totalMatchedLiquidity: 15,
			expectedShares:        15,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := CalculateSafeShares(
				tc.baseShares,
				2.0, // margin is ignored
				tc.compoundMultiplier,
				tc.totalMatchedLiquidity,
				false, // reduceSize is ignored
			)

			if result != tc.expectedShares {
				t.Errorf("Expected %.0f shares, got %.0f shares", tc.expectedShares, result)
			}
		})
	}
}

func TestCalculateAggregatedLiquidity_Basic(t *testing.T) {
	tests := []struct {
		name            string
		asks1           []MarketLevel
		asks2           []MarketLevel
		maxSum          float64
		expectedMatched float64
	}{
		{
			name: "Simple 10/10 liquidity",
			asks1: []MarketLevel{
				{Price: 0.48, Size: 10},
			},
			asks2: []MarketLevel{
				{Price: 0.50, Size: 10},
			},
			maxSum:          0.99,
			expectedMatched: 10,
		},
		{
			name: "Unequal liquidity takes minimum",
			asks1: []MarketLevel{
				{Price: 0.45, Size: 20},
			},
			asks2: []MarketLevel{
				{Price: 0.50, Size: 10},
			},
			maxSum:          0.99,
			expectedMatched: 10,
		},
		{
			name: "Multi-level aggregation with higher threshold",
			asks1: []MarketLevel{
				{Price: 0.45, Size: 10},
				{Price: 0.48, Size: 15},
			},
			asks2: []MarketLevel{
				{Price: 0.50, Size: 10},
				{Price: 0.51, Size: 20},
			},
			maxSum:          1.00,
			expectedMatched: 25,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := CalculateAggregatedLiquidity(tc.asks1, tc.asks2, tc.maxSum)
			if result.TotalMatchedLiquidity != tc.expectedMatched {
				t.Errorf("Expected %.0f, got %.0f", tc.expectedMatched, result.TotalMatchedLiquidity)
			}
		})
	}
}

func TestMinFloat(t *testing.T) {
	if MinFloat(10, 20) != 10 {
		t.Error("Expected 10")
	}
	if MinFloat(20, 10) != 10 {
		t.Error("Expected 10")
	}
}
