package paper

import (
	"testing"
)

func TestSplitInventory_RecordSplit(t *testing.T) {
	inv := NewSplitInventory()

	// Split $10 USDC into 10 YES + 10 NO shares
	inv.RecordSplit("BTC", "Up", "Down", 10.0)

	// Check shares
	upShares := inv.GetSplitShares("BTC", "Up")
	downShares := inv.GetSplitShares("BTC", "Down")

	if upShares != 10.0 {
		t.Errorf("Expected 10 Up shares, got %.2f", upShares)
	}
	if downShares != 10.0 {
		t.Errorf("Expected 10 Down shares, got %.2f", downShares)
	}
}

func TestSplitInventory_GetMinSplitShares(t *testing.T) {
	inv := NewSplitInventory()

	inv.RecordSplit("BTC", "Up", "Down", 10.0)

	// Sell some Up shares
	inv.RecordSell("BTC", "Up", 3.0, 0.55)

	// Min should now be 7 (Up has 7, Down has 10)
	min := inv.GetMinSplitShares("BTC", "Up", "Down")
	if min != 7.0 {
		t.Errorf("Expected min 7 shares, got %.2f", min)
	}
}

func TestSplitInventory_RecordSell(t *testing.T) {
	inv := NewSplitInventory()

	inv.RecordSplit("ETH", "Up", "Down", 20.0)

	// Sell 5 Up shares at $0.60 (cost basis was $0.50)
	profit := inv.RecordSell("ETH", "Up", 5.0, 0.60)

	// Profit = 5 * ($0.60 - $0.50) = $0.50
	expectedProfit := 0.50
	if profit != expectedProfit {
		t.Errorf("Expected profit $%.2f, got $%.2f", expectedProfit, profit)
	}

	// Check remaining shares
	remaining := inv.GetSplitShares("ETH", "Up")
	if remaining != 15.0 {
		t.Errorf("Expected 15 remaining Up shares, got %.2f", remaining)
	}
}

func TestSplitInventory_RecordMerge(t *testing.T) {
	inv := NewSplitInventory()

	inv.RecordSplit("SOL", "Up", "Down", 30.0)

	// Merge 10 pairs
	merged := inv.RecordMerge("SOL", "Up", "Down", 10.0)

	if merged != 10.0 {
		t.Errorf("Expected 10 merged, got %.2f", merged)
	}

	// Both sides should have 20 remaining
	upShares := inv.GetSplitShares("SOL", "Up")
	downShares := inv.GetSplitShares("SOL", "Down")

	if upShares != 20.0 {
		t.Errorf("Expected 20 Up shares, got %.2f", upShares)
	}
	if downShares != 20.0 {
		t.Errorf("Expected 20 Down shares, got %.2f", downShares)
	}
}

func TestSplitInventory_MergeWithUnbalanced(t *testing.T) {
	inv := NewSplitInventory()

	inv.RecordSplit("XRP", "Up", "Down", 20.0)

	// Sell some Up shares (creating imbalance)
	inv.RecordSell("XRP", "Up", 15.0, 0.55)

	// Try to merge 10 - should only merge 5 (the min available)
	merged := inv.RecordMerge("XRP", "Up", "Down", 10.0)

	if merged != 5.0 {
		t.Errorf("Expected 5 merged (limited by Up side), got %.2f", merged)
	}
}

func TestSplitInventory_NeedsReplenish(t *testing.T) {
	inv := NewSplitInventory()

	inv.RecordSplit("BTC", "Up", "Down", 100.0)

	// Sell down to 40 shares on Up side
	inv.RecordSell("BTC", "Up", 60.0, 0.55)

	// Check if needs replenish with threshold of 50
	needs := inv.NeedsReplenish("BTC", "Up", "Down", 50.0)
	if !needs {
		t.Error("Expected NeedsReplenish=true when below threshold")
	}

	// Split more
	inv.RecordSplit("BTC", "Up", "Down", 20.0)

	// Now should have 60 Up, 120 Down - min is 60, above threshold
	needs = inv.NeedsReplenish("BTC", "Up", "Down", 50.0)
	if needs {
		t.Error("Expected NeedsReplenish=false when above threshold")
	}
}

func TestSplitInventory_Clear(t *testing.T) {
	inv := NewSplitInventory()

	inv.RecordSplit("BTC", "Up", "Down", 50.0)
	inv.RecordSplit("ETH", "Up", "Down", 30.0)

	// Clear BTC
	inv.Clear("BTC", "Up", "Down")

	// BTC should be zero
	if inv.GetSplitShares("BTC", "Up") != 0 {
		t.Error("Expected BTC Up shares to be 0 after clear")
	}

	// ETH should still have shares
	if inv.GetSplitShares("ETH", "Up") != 30.0 {
		t.Error("Expected ETH Up shares to still be 30")
	}
}

func TestSplitInventory_SeparateFromBoughtShares(t *testing.T) {
	// This test documents the separation between split and bought shares
	// Split shares: created via SPLIT, used for SELL
	// Bought shares: bought from market, used for MERGE

	inv := NewSplitInventory()

	// Simulate: Split $100 USDC
	inv.RecordSplit("BTC", "Up", "Down", 100.0)

	// Sell both sides at 3% profit (bid_sum = $1.03)
	profit1 := inv.RecordSell("BTC", "Up", 100.0, 0.515)   // $0.515 per Up share
	profit2 := inv.RecordSell("BTC", "Down", 100.0, 0.515) // $0.515 per Down share

	totalProfit := profit1 + profit2
	// Cost was $0.50 per share, sold at $0.515
	// Profit = 100 * (0.515 - 0.50) + 100 * (0.515 - 0.50) = $3.00
	expectedProfit := 3.0

	if totalProfit != expectedProfit {
		t.Errorf("Expected total profit $%.2f, got $%.2f", expectedProfit, totalProfit)
	}

	// Verify no shares remain
	if inv.GetMinSplitShares("BTC", "Up", "Down") != 0 {
		t.Error("Expected no remaining shares after selling all")
	}
}

func TestSplitInventory_GetAllPositions(t *testing.T) {
	inv := NewSplitInventory()

	// Initially should be empty
	positions := inv.GetAllPositions()
	if len(positions) != 0 {
		t.Errorf("Expected 0 positions initially, got %d", len(positions))
	}

	// Split for BTC
	inv.RecordSplit("BTC", "Up", "Down", 50.0)

	// Split for ETH
	inv.RecordSplit("ETH", "Yes", "No", 30.0)

	// Get positions
	positions = inv.GetAllPositions()

	// Should have 4 positions (2 markets x 2 outcomes each)
	if len(positions) != 4 {
		t.Errorf("Expected 4 positions, got %d", len(positions))
	}

	// Build a map for easier verification
	posMap := make(map[string]SplitPosition)
	for _, p := range positions {
		key := p.MarketID + ":" + p.Outcome
		posMap[key] = p
	}

	// Verify BTC positions
	if btcUp, ok := posMap["BTC:Up"]; !ok {
		t.Error("Expected BTC:Up position")
	} else {
		if btcUp.Shares != 50.0 {
			t.Errorf("Expected BTC:Up shares = 50, got %.2f", btcUp.Shares)
		}
		if btcUp.CostBasis != 0.50 {
			t.Errorf("Expected BTC:Up cost basis = 0.50, got %.2f", btcUp.CostBasis)
		}
	}

	if btcDown, ok := posMap["BTC:Down"]; !ok {
		t.Error("Expected BTC:Down position")
	} else {
		if btcDown.Shares != 50.0 {
			t.Errorf("Expected BTC:Down shares = 50, got %.2f", btcDown.Shares)
		}
	}

	// Verify ETH positions
	if ethYes, ok := posMap["ETH:Yes"]; !ok {
		t.Error("Expected ETH:Yes position")
	} else {
		if ethYes.Shares != 30.0 {
			t.Errorf("Expected ETH:Yes shares = 30, got %.2f", ethYes.Shares)
		}
	}

	// Sell some shares and verify positions update
	inv.RecordSell("BTC", "Up", 20.0, 0.55)

	positions = inv.GetAllPositions()
	if len(positions) != 4 {
		t.Errorf("Expected 4 positions after sell, got %d", len(positions))
	}

	// Check that zero shares are filtered out
	inv.RecordSell("BTC", "Up", 30.0, 0.55) // Sell remaining
	positions = inv.GetAllPositions()
	posMap = make(map[string]SplitPosition)
	for _, p := range positions {
		key := p.MarketID + ":" + p.Outcome
		posMap[key] = p
	}

	if _, ok := posMap["BTC:Up"]; ok {
		t.Error("BTC:Up should not appear in positions when shares = 0")
	}

	// BTC:Down should still exist
	if btcDown, ok := posMap["BTC:Down"]; !ok {
		t.Error("Expected BTC:Down position to still exist")
	} else if btcDown.Shares != 50.0 {
		t.Errorf("Expected BTC:Down shares = 50, got %.2f", btcDown.Shares)
	}
}

func TestSplitInventory_GetAllPositions_EmptyAndNegative(t *testing.T) {
	inv := NewSplitInventory()

	// Empty inventory should return empty slice (not nil)
	positions := inv.GetAllPositions()
	if positions == nil {
		t.Error("GetAllPositions should return empty slice, not nil")
	}
	if len(positions) != 0 {
		t.Errorf("Expected 0 positions, got %d", len(positions))
	}

	// Split then merge all should result in no positions
	inv.RecordSplit("BTC", "Up", "Down", 10.0)
	inv.RecordMerge("BTC", "Up", "Down", 10.0)

	positions = inv.GetAllPositions()
	if len(positions) != 0 {
		t.Errorf("Expected 0 positions after merging all, got %d", len(positions))
	}
}

func TestSplitInventory_splitKey(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		expected []string
	}{
		{
			name:     "standard key",
			key:      "BTC:Up",
			expected: []string{"BTC", "Up"},
		},
		{
			name:     "key with colon in outcome",
			key:      "market:Yes:Above",
			expected: []string{"market", "Yes:Above"},
		},
		{
			name:     "key without colon",
			key:      "invalidkey",
			expected: []string{"invalidkey"},
		},
		{
			name:     "empty key",
			key:      "",
			expected: []string{""},
		},
		{
			name:     "multiple colons",
			key:      "a:b:c:d",
			expected: []string{"a", "b:c:d"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := splitKey(tc.key)
			if len(result) != len(tc.expected) {
				t.Errorf("Expected %d parts, got %d", len(tc.expected), len(result))
				return
			}
			for i := range result {
				if result[i] != tc.expected[i] {
					t.Errorf("Part %d: expected %q, got %q", i, tc.expected[i], result[i])
				}
			}
		})
	}
}
