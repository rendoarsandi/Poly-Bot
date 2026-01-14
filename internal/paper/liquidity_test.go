package paper

import (
	"testing"
)

// TestSafetyMarginConstant verifies the 80% safety margin constant
func TestSafetyMarginConstant(t *testing.T) {
	if SafetyMargin != 0.80 {
		t.Errorf("SafetyMargin should be 0.80, got %v", SafetyMargin)
	}
}

// TestCalculateSafeShares_LiquidityCap tests that shares are capped at 80% of liquidity
func TestCalculateSafeShares_LiquidityCap(t *testing.T) {
	tests := []struct {
		name                  string
		baseShares            float64
		margin                float64
		compoundMultiplier    float64
		totalMatchedLiquidity float64
		reduceSize            bool
		expectedShares        float64
		description           string
	}{
		{
			name:                  "10/10 liquidity caps at 8 shares",
			baseShares:            10,
			margin:                2.0, // Would scale to 20 shares
			compoundMultiplier:    1.0,
			totalMatchedLiquidity: 10,
			reduceSize:            false,
			expectedShares:        8, // 80% of 10 = 8
			description:           "When liquidity is 10, max safe shares should be 8 (80%)",
		},
		{
			name:                  "25/25 liquidity caps at 20 shares",
			baseShares:            5,
			margin:                5.0, // Would scale to 25 shares
			compoundMultiplier:    1.0,
			totalMatchedLiquidity: 25,
			reduceSize:            false,
			expectedShares:        20, // 80% of 25 = 20
			description:           "When liquidity is 25, max safe shares should be 20 (80%)",
		},
		{
			name:                  "100/100 liquidity caps at 80 shares",
			baseShares:            20,
			margin:                5.0, // Would scale to 100 shares
			compoundMultiplier:    1.0,
			totalMatchedLiquidity: 100,
			reduceSize:            false,
			expectedShares:        80, // 80% of 100 = 80
			description:           "When liquidity is 100, max safe shares should be 80 (80%)",
		},
		{
			name:                  "Small liquidity forces smaller order",
			baseShares:            10,
			margin:                2.0, // Would scale to 20 shares
			compoundMultiplier:    1.0,
			totalMatchedLiquidity: 5,
			reduceSize:            false,
			expectedShares:        4, // 80% of 5 = 4
			description:           "5 shares liquidity should cap at 4 shares",
		},
		{
			name:                  "No cap when liquidity is abundant",
			baseShares:            5,
			margin:                2.0, // Scales to 10 shares
			compoundMultiplier:    1.0,
			totalMatchedLiquidity: 50,
			reduceSize:            false,
			expectedShares:        10, // 10 < 40 (80% of 50), no cap needed
			description:           "When liquidity exceeds scaled shares, no cap is applied",
		},
		{
			name:                  "Compounding hits liquidity cap",
			baseShares:            10,
			margin:                2.0, // Scales to 20
			compoundMultiplier:    2.0, // Compounds to 40
			totalMatchedLiquidity: 25,  // 80% = 20
			reduceSize:            false,
			expectedShares:        20, // Capped at 80% of 25
			description:           "Compounding should still be capped by liquidity",
		},
		{
			name:                  "Reduce size mode uses base shares",
			baseShares:            5,
			margin:                5.0, // Would scale to 25, but reduceSize ignores scaling
			compoundMultiplier:    1.0,
			totalMatchedLiquidity: 10,
			reduceSize:            true,
			expectedShares:        5, // base shares, capped at 8 but 5 < 8
			description:           "Reduce size mode doesn't scale, uses base shares",
		},
		{
			name:                  "Reduce size mode still caps at 80%",
			baseShares:            20,
			margin:                5.0,
			compoundMultiplier:    1.0,
			totalMatchedLiquidity: 10,
			reduceSize:            true,
			expectedShares:        8, // 80% of 10 = 8, even in reduce mode
			description:           "Reduce size mode still respects liquidity cap",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := CalculateSafeShares(
				tc.baseShares,
				tc.margin,
				tc.compoundMultiplier,
				tc.totalMatchedLiquidity,
				tc.reduceSize,
			)

			if result != tc.expectedShares {
				t.Errorf("%s\nExpected %.0f shares, got %.0f shares\nInputs: base=%.0f, margin=%.1f%%, mult=%.1fx, liquidity=%.0f",
					tc.description,
					tc.expectedShares, result,
					tc.baseShares, tc.margin, tc.compoundMultiplier, tc.totalMatchedLiquidity)
			}
		})
	}
}

// TestCalculateAggregatedLiquidity_Basic tests basic liquidity aggregation
func TestCalculateAggregatedLiquidity_Basic(t *testing.T) {
	tests := []struct {
		name            string
		asks1           []MarketLevel
		asks2           []MarketLevel
		maxSum          float64
		expectedMatched float64
		expectedSafe    float64
		expectedCumLiq1 float64
		expectedCumLiq2 float64
		expectedLevels  int
	}{
		{
			name: "Simple 10/10 liquidity",
			asks1: []MarketLevel{
				{Price: 0.48, Size: 10},
			},
			asks2: []MarketLevel{
				{Price: 0.50, Size: 10},
			},
			maxSum:          0.99, // 2% margin
			expectedMatched: 10,
			expectedSafe:    8, // 80% of 10
			expectedCumLiq1: 10,
			expectedCumLiq2: 10,
			expectedLevels:  1,
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
			expectedMatched: 10, // min(20, 10) = 10
			expectedSafe:    8,  // 80% of 10
			expectedCumLiq1: 10,
			expectedCumLiq2: 10,
			expectedLevels:  1,
		},
		{
			name: "Multi-level aggregation",
			asks1: []MarketLevel{
				{Price: 0.45, Size: 10},
				{Price: 0.48, Size: 15},
			},
			asks2: []MarketLevel{
				{Price: 0.50, Size: 10},
				{Price: 0.52, Size: 20},
			},
			maxSum:          0.99, // 0.45+0.50=0.95 OK, 0.48+0.52=1.00 NOT OK
			expectedMatched: 10,   // Only first levels match under threshold
			expectedSafe:    8,
			expectedCumLiq1: 10,
			expectedCumLiq2: 10,
			expectedLevels:  1,
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
			maxSum:          1.00, // 0.45+0.50=0.95 OK, 0.48+0.51=0.99 OK
			expectedMatched: 25,   // 10 from level 1 + 15 from level 2 (limited by asks1)
			expectedSafe:    20,   // 80% of 25
			expectedCumLiq1: 25,
			expectedCumLiq2: 25,
			expectedLevels:  2, // Processes 2 levels
		},
		{
			name: "Price exceeds threshold immediately",
			asks1: []MarketLevel{
				{Price: 0.60, Size: 10},
			},
			asks2: []MarketLevel{
				{Price: 0.50, Size: 10},
			},
			maxSum:          0.99, // 0.60+0.50=1.10 > 0.99
			expectedMatched: 0,
			expectedSafe:    0,
			expectedCumLiq1: 0,
			expectedCumLiq2: 0,
			expectedLevels:  0,
		},
		{
			name:            "Empty asks",
			asks1:           []MarketLevel{},
			asks2:           []MarketLevel{{Price: 0.50, Size: 10}},
			maxSum:          0.99,
			expectedMatched: 0,
			expectedSafe:    0,
			expectedCumLiq1: 0,
			expectedCumLiq2: 0,
			expectedLevels:  0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := CalculateAggregatedLiquidity(tc.asks1, tc.asks2, tc.maxSum)

			if result.TotalMatchedLiquidity != tc.expectedMatched {
				t.Errorf("TotalMatchedLiquidity: expected %.0f, got %.0f",
					tc.expectedMatched, result.TotalMatchedLiquidity)
			}

			if result.MaxSafeShares != tc.expectedSafe {
				t.Errorf("MaxSafeShares: expected %.0f, got %.0f (should be 80%% of %.0f)",
					tc.expectedSafe, result.MaxSafeShares, tc.expectedMatched)
			}

			if result.CumLiq1 != tc.expectedCumLiq1 {
				t.Errorf("CumLiq1: expected %.0f, got %.0f",
					tc.expectedCumLiq1, result.CumLiq1)
			}

			if result.CumLiq2 != tc.expectedCumLiq2 {
				t.Errorf("CumLiq2: expected %.0f, got %.0f",
					tc.expectedCumLiq2, result.CumLiq2)
			}

			if result.LevelsProcessed != tc.expectedLevels {
				t.Errorf("LevelsProcessed: expected %d, got %d",
					tc.expectedLevels, result.LevelsProcessed)
			}
		})
	}
}

// TestCalculateAggregatedLiquidity_RealWorldScenarios tests realistic trading scenarios
func TestCalculateAggregatedLiquidity_RealWorldScenarios(t *testing.T) {
	tests := []struct {
		name           string
		asks1          []MarketLevel
		asks2          []MarketLevel
		maxSum         float64
		expectedShares float64
		description    string
	}{
		{
			name: "BTC scenario: Up@$0.45 + Down@$0.53 = $0.98",
			asks1: []MarketLevel{
				{Price: 0.45, Size: 25},
			},
			asks2: []MarketLevel{
				{Price: 0.53, Size: 25},
			},
			maxSum:         0.99,
			expectedShares: 20, // 80% of 25 = 20
			description:    "BTC market with 25/25 liquidity should allow 20 shares",
		},
		{
			name: "SOL scenario: Up@$0.40 + Down@$0.58 = $0.98",
			asks1: []MarketLevel{
				{Price: 0.40, Size: 10},
			},
			asks2: []MarketLevel{
				{Price: 0.58, Size: 10},
			},
			maxSum:         0.99,
			expectedShares: 8, // 80% of 10 = 8
			description:    "SOL market with 10/10 liquidity should allow 8 shares",
		},
		{
			name: "Thin liquidity on one side",
			asks1: []MarketLevel{
				{Price: 0.48, Size: 100},
			},
			asks2: []MarketLevel{
				{Price: 0.50, Size: 5},
			},
			maxSum:         0.99,
			expectedShares: 4, // 80% of min(100, 5) = 80% of 5 = 4
			description:    "Matched liquidity limited by thin side",
		},
		{
			name: "Deep book aggregation",
			asks1: []MarketLevel{
				{Price: 0.45, Size: 10},
				{Price: 0.46, Size: 10},
				{Price: 0.47, Size: 10},
			},
			asks2: []MarketLevel{
				{Price: 0.50, Size: 10},
				{Price: 0.51, Size: 10},
				{Price: 0.52, Size: 10},
			},
			maxSum:         0.98, // Only 0.45+0.50=0.95 and 0.46+0.51=0.97 pass
			expectedShares: 16,   // 80% of 20 (first two levels match)
			description:    "Aggregates across multiple price levels within threshold",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := CalculateAggregatedLiquidity(tc.asks1, tc.asks2, tc.maxSum)

			if result.MaxSafeShares != tc.expectedShares {
				t.Errorf("%s\nExpected %.0f safe shares, got %.0f\nMatched liquidity: %.0f",
					tc.description,
					tc.expectedShares, result.MaxSafeShares,
					result.TotalMatchedLiquidity)
			}
		})
	}
}

// TestCalculateSafeShares_MarginScaling verifies margin-based scaling before cap
func TestCalculateSafeShares_MarginScaling(t *testing.T) {
	tests := []struct {
		name           string
		baseShares     float64
		margin         float64
		liquidity      float64
		expectedShares float64
	}{
		{
			name:           "1% margin = 1x base",
			baseShares:     5,
			margin:         1.0,
			liquidity:      100, // No cap
			expectedShares: 5,
		},
		{
			name:           "2% margin = 2x base",
			baseShares:     5,
			margin:         2.0,
			liquidity:      100,
			expectedShares: 10,
		},
		{
			name:           "3% margin = 3x base",
			baseShares:     5,
			margin:         3.0,
			liquidity:      100,
			expectedShares: 15,
		},
		{
			name:           "4% margin = 4x base",
			baseShares:     5,
			margin:         4.0,
			liquidity:      100,
			expectedShares: 20,
		},
		{
			name:           "5% margin = 5x base",
			baseShares:     5,
			margin:         5.0,
			liquidity:      100,
			expectedShares: 25,
		},
		{
			name:           "6% margin = 5x base (max)",
			baseShares:     5,
			margin:         6.0,
			liquidity:      100,
			expectedShares: 25, // Caps at 5x
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := CalculateSafeShares(
				tc.baseShares,
				tc.margin,
				1.0, // No compounding
				tc.liquidity,
				false,
			)

			if result != tc.expectedShares {
				t.Errorf("Expected %.0f shares for %.1f%% margin, got %.0f",
					tc.expectedShares, tc.margin, result)
			}
		})
	}
}

// TestCalculateSafeShares_CompoundingEffect tests compounding multiplier effect
func TestCalculateSafeShares_CompoundingEffect(t *testing.T) {
	tests := []struct {
		name           string
		baseShares     float64
		multiplier     float64
		liquidity      float64
		expectedShares float64
	}{
		{
			name:           "1.0x multiplier = no change",
			baseShares:     10,
			multiplier:     1.0,
			liquidity:      100,
			expectedShares: 10,
		},
		{
			name:           "1.5x multiplier",
			baseShares:     10,
			multiplier:     1.5,
			liquidity:      100,
			expectedShares: 15, // int(10 * 1.5) = 15
		},
		{
			name:           "2.0x multiplier",
			baseShares:     10,
			multiplier:     2.0,
			liquidity:      100,
			expectedShares: 20,
		},
		{
			name:           "3.0x multiplier (max)",
			baseShares:     10,
			multiplier:     3.0,
			liquidity:      100,
			expectedShares: 30,
		},
		{
			name:           "Multiplier capped by liquidity",
			baseShares:     10,
			multiplier:     3.0,
			liquidity:      20, // 80% = 16
			expectedShares: 16,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := CalculateSafeShares(
				tc.baseShares,
				1.0, // 1% margin = 1x
				tc.multiplier,
				tc.liquidity,
				false,
			)

			if result != tc.expectedShares {
				t.Errorf("Expected %.0f shares for %.1fx multiplier, got %.0f",
					tc.expectedShares, tc.multiplier, result)
			}
		})
	}
}

// TestMinFloat tests the MinFloat helper
func TestMinFloat(t *testing.T) {
	tests := []struct {
		a, b     float64
		expected float64
	}{
		{10, 20, 10},
		{20, 10, 10},
		{10, 10, 10},
		{0, 10, 0},
		{-5, 10, -5},
	}

	for _, tc := range tests {
		result := MinFloat(tc.a, tc.b)
		if result != tc.expected {
			t.Errorf("MinFloat(%.0f, %.0f) = %.0f, expected %.0f",
				tc.a, tc.b, result, tc.expected)
		}
	}
}

// Benchmark for liquidity calculation performance
func BenchmarkCalculateAggregatedLiquidity(b *testing.B) {
	asks1 := []MarketLevel{
		{Price: 0.45, Size: 100},
		{Price: 0.46, Size: 100},
		{Price: 0.47, Size: 100},
	}
	asks2 := []MarketLevel{
		{Price: 0.50, Size: 100},
		{Price: 0.51, Size: 100},
		{Price: 0.52, Size: 100},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		CalculateAggregatedLiquidity(asks1, asks2, 0.99)
	}
}

func BenchmarkCalculateSafeShares(b *testing.B) {
	for i := 0; i < b.N; i++ {
		CalculateSafeShares(10, 3.0, 1.5, 25, false)
	}
}

// === Merged from liquidity_accuracy_test.go ===

// TestLiquidityEdgeCases tests boundary conditions in liquidity handling
func TestLiquidityEdgeCases(t *testing.T) {
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

// TestOrderSizing_MatchesRealScenarios verifies order sizing matches production scenarios
func TestOrderSizing_MatchesRealScenarios(t *testing.T) {
	tests := []struct {
		name           string
		liq1           float64
		liq2           float64
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
			baseShares:     10,
			margin:         3.0,
			multiplier:     1.1,
			expectedShares: 20, // 10*3*1.1=33, but capped at 80% of 25 = 20
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
