//go:build !debug
// +build !debug

// depth_test.go - Core depth aggregation tests
// Run debug tests with: go test -tags=debug ./internal/paper/

package paper

import (
	"testing"
)

// TestDepthAggregation verifies that liquidity is correctly aggregated across all valid levels
func TestDepthAggregation(t *testing.T) {
	tests := []struct {
		name           string
		asks1          []MarketLevel // Up side
		asks2          []MarketLevel // Down side
		maxSum         float64       // e.g., 0.98 for 2% min margin
		wantRawLiq1    float64
		wantRawLiq2    float64
		wantMatched    float64
		wantValidLvl1  int
		wantValidLvl2  int
	}{
		{
			name: "Single level valid - prices jump too fast",
			asks1: []MarketLevel{
				{Price: 0.62, Size: 270},
				{Price: 0.70, Size: 100}, // 0.70 + 0.32 = 1.02 > 0.98
			},
			asks2: []MarketLevel{
				{Price: 0.32, Size: 26},
				{Price: 0.37, Size: 50}, // 0.62 + 0.37 = 0.99 > 0.98 - NOW invalid
			},
			maxSum:        0.98,
			wantRawLiq1:   270,
			wantRawLiq2:   26,
			wantMatched:   26,
			wantValidLvl1: 1,
			wantValidLvl2: 1,
		},
		{
			name: "Multiple levels valid - gradual price increase",
			asks1: []MarketLevel{
				{Price: 0.48, Size: 40},
				{Price: 0.49, Size: 35},
				{Price: 0.50, Size: 30},
				{Price: 0.55, Size: 100}, // too expensive
			},
			asks2: []MarketLevel{
				{Price: 0.46, Size: 50},
				{Price: 0.47, Size: 45},
				{Price: 0.48, Size: 40},
				{Price: 0.52, Size: 100}, // too expensive
			},
			maxSum:        0.98,
			wantRawLiq1:   40 + 35 + 30, // = 105 (3 levels)
			wantRawLiq2:   50 + 45 + 40, // = 135 (3 levels)
			wantMatched:   40 + 35 + 30, // matched = min at each combo
			wantValidLvl1: 3,
			wantValidLvl2: 3,
		},
		{
			name: "6% margin at top, should read down to 2%",
			asks1: []MarketLevel{
				{Price: 0.47, Size: 100}, // 0.47+0.47=0.94 (6%)
				{Price: 0.48, Size: 80},  // 0.48+0.48=0.96 (4%)
				{Price: 0.49, Size: 60},  // 0.49+0.49=0.98 (2%) - still valid!
				{Price: 0.50, Size: 50},  // 0.50+0.50=1.00 - invalid
			},
			asks2: []MarketLevel{
				{Price: 0.47, Size: 100},
				{Price: 0.48, Size: 80},
				{Price: 0.49, Size: 60},
				{Price: 0.50, Size: 50},
			},
			maxSum:        0.98,
			wantRawLiq1:   100 + 80 + 60, // = 240 (3 levels: 6%, 4%, 2%)
			wantRawLiq2:   100 + 80 + 60, // = 240
			wantMatched:   100 + 80 + 60, // = 240 (equal sizes)
			wantValidLvl1: 3,
			wantValidLvl2: 3,
		},
		{
			name: "Uneven liquidity - one side exhausted first",
			asks1: []MarketLevel{
				{Price: 0.48, Size: 200}, // large liquidity
			},
			asks2: []MarketLevel{
				{Price: 0.48, Size: 30},
				{Price: 0.49, Size: 40},
				{Price: 0.50, Size: 50}, // 0.48+0.50=0.98 still valid
				{Price: 0.51, Size: 60}, // 0.48+0.51=0.99 invalid
			},
			maxSum:        0.98,
			wantRawLiq1:   200,
			wantRawLiq2:   30 + 40 + 50, // = 120 (3 levels on Down)
			wantMatched:   30 + 40 + 50, // = 120 (limited by Down side)
			wantValidLvl1: 1,            // only 1 level on Up
			wantValidLvl2: 3,            // 3 levels on Down
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Copy slices to avoid mutation
			asks1 := make([]MarketLevel, len(tt.asks1))
			copy(asks1, tt.asks1)
			asks2 := make([]MarketLevel, len(tt.asks2))
			copy(asks2, tt.asks2)

			// Run the same algorithm as the bot
			var totalMatchedLiquidity float64
			var rawLiq1, rawLiq2 float64
			var maxValidI, maxValidJ int

			i, j := 0, 0
			for i < len(asks1) && j < len(asks2) {
				p1 := asks1[i].Price
				p2 := asks2[j].Price

				if p1+p2 > tt.maxSum {
					break
				}

				if i+1 > maxValidI {
					maxValidI = i + 1
					rawLiq1 += asks1[i].Size
				}
				if j+1 > maxValidJ {
					maxValidJ = j + 1
					rawLiq2 += asks2[j].Size
				}

				levelLiq1 := asks1[i].Size
				levelLiq2 := asks2[j].Size

				matchedAtLevel := levelLiq1
				if levelLiq2 < matchedAtLevel {
					matchedAtLevel = levelLiq2
				}

				totalMatchedLiquidity += matchedAtLevel

				remaining1 := levelLiq1 - matchedAtLevel
				remaining2 := levelLiq2 - matchedAtLevel

				if remaining1 <= 0 {
					i++
				} else {
					asks1[i].Size = remaining1
				}
				if remaining2 <= 0 {
					j++
				} else {
					asks2[j].Size = remaining2
				}
			}

			// Verify results
			if rawLiq1 != tt.wantRawLiq1 {
				t.Errorf("rawLiq1 = %.0f, want %.0f", rawLiq1, tt.wantRawLiq1)
			}
			if rawLiq2 != tt.wantRawLiq2 {
				t.Errorf("rawLiq2 = %.0f, want %.0f", rawLiq2, tt.wantRawLiq2)
			}
			if totalMatchedLiquidity != tt.wantMatched {
				t.Errorf("totalMatchedLiquidity = %.0f, want %.0f", totalMatchedLiquidity, tt.wantMatched)
			}
			if maxValidI != tt.wantValidLvl1 {
				t.Errorf("maxValidI = %d, want %d", maxValidI, tt.wantValidLvl1)
			}
			if maxValidJ != tt.wantValidLvl2 {
				t.Errorf("maxValidJ = %d, want %d", maxValidJ, tt.wantValidLvl2)
			}
		})
	}
}

// TestDepthAggregationRealistic tests with fine-grained prices like real order book
func TestDepthAggregationRealistic(t *testing.T) {
	// Realistic order book with $0.01 price increments
	// This simulates all margin levels: 6%, 5%, 4%, 3%, 2%
	asks1 := []MarketLevel{
		{Price: 0.47, Size: 20},  // 0.47+0.47=0.94 (6%)
		{Price: 0.475, Size: 15}, // 0.475+0.475=0.95 (5%)
		{Price: 0.48, Size: 25},  // 0.48+0.48=0.96 (4%)
		{Price: 0.485, Size: 18}, // 0.485+0.485=0.97 (3%)
		{Price: 0.49, Size: 30},  // 0.49+0.49=0.98 (2%)
		{Price: 0.495, Size: 40}, // 0.495+0.495=0.99 (1%) - invalid
		{Price: 0.50, Size: 50},  // invalid
	}
	asks2 := []MarketLevel{
		{Price: 0.47, Size: 20},
		{Price: 0.475, Size: 15},
		{Price: 0.48, Size: 25},
		{Price: 0.485, Size: 18},
		{Price: 0.49, Size: 30},
		{Price: 0.495, Size: 40},
		{Price: 0.50, Size: 50},
	}
	maxSum := 0.98

	var totalMatchedLiquidity float64
	var rawLiq1, rawLiq2 float64
	var maxValidI, maxValidJ int

	i, j := 0, 0
	for i < len(asks1) && j < len(asks2) {
		p1 := asks1[i].Price
		p2 := asks2[j].Price
		sum := p1 + p2

		if sum > maxSum {
			break
		}

		if i+1 > maxValidI {
			maxValidI = i + 1
			rawLiq1 += asks1[i].Size
		}
		if j+1 > maxValidJ {
			maxValidJ = j + 1
			rawLiq2 += asks2[j].Size
		}

		levelLiq1 := asks1[i].Size
		levelLiq2 := asks2[j].Size

		matchedAtLevel := levelLiq1
		if levelLiq2 < matchedAtLevel {
			matchedAtLevel = levelLiq2
		}

		totalMatchedLiquidity += matchedAtLevel

		remaining1 := levelLiq1 - matchedAtLevel
		remaining2 := levelLiq2 - matchedAtLevel

		if remaining1 <= 0 {
			i++
		} else {
			asks1[i].Size = remaining1
		}
		if remaining2 <= 0 {
			j++
		} else {
			asks2[j].Size = remaining2
		}
	}

	// Expected: 5 levels on each side (6%, 5%, 4%, 3%, 2%)
	expectedLiq := 20.0 + 15 + 25 + 18 + 30 // = 108
	if maxValidI != 5 || maxValidJ != 5 {
		t.Errorf("Expected 5/5 valid levels, got %d/%d", maxValidI, maxValidJ)
	}
	if rawLiq1 != expectedLiq || rawLiq2 != expectedLiq {
		t.Errorf("Expected rawLiq %.0f/%.0f, got %.0f/%.0f", expectedLiq, expectedLiq, rawLiq1, rawLiq2)
	}
	if totalMatchedLiquidity != expectedLiq {
		t.Errorf("Expected matched %.0f, got %.0f", expectedLiq, totalMatchedLiquidity)
	}
}
