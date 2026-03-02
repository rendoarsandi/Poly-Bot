package paper

import (
	"sync"
	"testing"
)

func TestReplenishController_CheckReplenish_LowInventoryGoodMargin(t *testing.T) {
	ctrl := NewReplenishController()

	params := ReplenishParams{
		CurrentShares:      20,  // Low inventory
		TargetBuffer:       100, // Target is 100, threshold is 40
		SellMargin:         3.5, // Above minimum
		MinMarginThreshold: 2.0, // Minimum is 2%
		CurrentBalance:     1000,
		ReplenishAmount:    50,
		MaxBalancePercent:  0.30, // 30% cap = $300
	}

	decision := ctrl.CheckReplenish(params)

	if !decision.ShouldReplenish {
		t.Errorf("Expected ShouldReplenish=true, got false. Reason: %s", decision.Reason)
	}
	if decision.Amount != 50 {
		t.Errorf("Expected Amount=50, got %.2f", decision.Amount)
	}
	if decision.Reason != "low inventory with good margin" {
		t.Errorf("Unexpected reason: %s", decision.Reason)
	}
}

func TestReplenishController_CheckReplenish_InventoryAboveThreshold(t *testing.T) {
	ctrl := NewReplenishController()

	params := ReplenishParams{
		CurrentShares:      50, // Above threshold (40% of 100 = 40)
		TargetBuffer:       100,
		SellMargin:         3.5,
		MinMarginThreshold: 2.0,
		CurrentBalance:     1000,
		ReplenishAmount:    50,
		MaxBalancePercent:  0.30,
	}

	decision := ctrl.CheckReplenish(params)

	if decision.ShouldReplenish {
		t.Error("Expected ShouldReplenish=false when inventory above threshold")
	}
	if decision.Reason != "inventory above threshold" {
		t.Errorf("Expected reason 'inventory above threshold', got '%s'", decision.Reason)
	}
}

func TestReplenishController_CheckReplenish_MarginBelowThreshold(t *testing.T) {
	ctrl := NewReplenishController()

	params := ReplenishParams{
		CurrentShares:      20,
		TargetBuffer:       100,
		SellMargin:         1.5, // Below minimum of 2.0
		MinMarginThreshold: 2.0,
		CurrentBalance:     1000,
		ReplenishAmount:    50,
		MaxBalancePercent:  0.30,
	}

	decision := ctrl.CheckReplenish(params)

	if decision.ShouldReplenish {
		t.Error("Expected ShouldReplenish=false when margin below threshold")
	}
	if decision.Reason != "margin below threshold" {
		t.Errorf("Expected reason 'margin below threshold', got '%s'", decision.Reason)
	}
}

func TestReplenishController_CheckReplenish_WouldExceedBalanceCap(t *testing.T) {
	ctrl := NewReplenishController()

	params := ReplenishParams{
		CurrentShares:      280, // Already near cap
		TargetBuffer:       1000,
		SellMargin:         5.0,
		MinMarginThreshold: 2.0,
		CurrentBalance:     1000,
		ReplenishAmount:    50, // Would push to 330, over 300 cap
		MaxBalancePercent:  0.30,
	}

	decision := ctrl.CheckReplenish(params)

	if decision.ShouldReplenish {
		t.Error("Expected ShouldReplenish=false when would exceed balance cap")
	}
	if decision.Reason != "would exceed balance cap" {
		t.Errorf("Expected reason 'would exceed balance cap', got '%s'", decision.Reason)
	}
}

func TestReplenishController_CheckReplenish_AlreadyInProgress(t *testing.T) {
	ctrl := NewReplenishController()

	// Simulate replenish in progress
	ctrl.MarkInProgress()

	params := ReplenishParams{
		CurrentShares:      20,
		TargetBuffer:       100,
		SellMargin:         5.0,
		MinMarginThreshold: 2.0,
		CurrentBalance:     1000,
		ReplenishAmount:    50,
		MaxBalancePercent:  0.30,
	}

	decision := ctrl.CheckReplenish(params)

	if decision.ShouldReplenish {
		t.Error("Expected ShouldReplenish=false when replenish already in progress")
	}
	if decision.Reason != "replenish already in progress" {
		t.Errorf("Expected reason 'replenish already in progress', got '%s'", decision.Reason)
	}
}

func TestReplenishController_MarkInProgress_Atomic(t *testing.T) {
	ctrl := NewReplenishController()

	// First call should succeed
	if !ctrl.MarkInProgress() {
		t.Error("First MarkInProgress should return true")
	}

	// Second call should fail (already in progress)
	if ctrl.MarkInProgress() {
		t.Error("Second MarkInProgress should return false")
	}

	// After marking complete, should succeed again
	ctrl.MarkComplete()
	if !ctrl.MarkInProgress() {
		t.Error("MarkInProgress after MarkComplete should return true")
	}
}

func TestReplenishController_ConcurrentAccess(t *testing.T) {
	ctrl := NewReplenishController()

	// Simulate concurrent replenishment attempts
	successCount := 0
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ctrl.MarkInProgress() {
				mu.Lock()
				successCount++
				mu.Unlock()
				// Simulate some work
				ctrl.MarkComplete()
			}
		}()
	}

	wg.Wait()

	// At least one should have succeeded
	if successCount == 0 {
		t.Error("Expected at least one successful MarkInProgress")
	}
	// Not all should succeed (due to contention)
	// But this is probabilistic, so we just verify the mechanism works
	t.Logf("Concurrent test: %d/100 goroutines successfully marked in progress", successCount)
}

func TestReplenishController_EdgeCases(t *testing.T) {
	ctrl := NewReplenishController()

	tests := []struct {
		name     string
		params   ReplenishParams
		expected bool
		reason   string
	}{
		{
			name: "exactly at threshold",
			params: ReplenishParams{
				CurrentShares:      40, // Exactly at 40% of 100
				TargetBuffer:       100,
				SellMargin:         5.0,
				MinMarginThreshold: 2.0,
				CurrentBalance:     1000,
				ReplenishAmount:    50,
				MaxBalancePercent:  0.30,
			},
			expected: false, // >= threshold means no replenish
			reason:   "inventory above threshold",
		},
		{
			name: "exactly at margin threshold",
			params: ReplenishParams{
				CurrentShares:      20,
				TargetBuffer:       100,
				SellMargin:         2.0, // Exactly at minimum
				MinMarginThreshold: 2.0,
				CurrentBalance:     1000,
				ReplenishAmount:    50,
				MaxBalancePercent:  0.30,
			},
			expected: true,
			reason:   "low inventory with good margin",
		},
		{
			name: "exactly at balance cap",
			params: ReplenishParams{
				CurrentShares:      250,
				TargetBuffer:       1000,
				SellMargin:         5.0,
				MinMarginThreshold: 2.0,
				CurrentBalance:     1000,
				ReplenishAmount:    50, // Would hit exactly 300 (30% of 1000)
				MaxBalancePercent:  0.30,
			},
			expected: false, // >= cap means no replenish
			reason:   "would exceed balance cap",
		},
		{
			name: "zero inventory",
			params: ReplenishParams{
				CurrentShares:      0,
				TargetBuffer:       100,
				SellMargin:         5.0,
				MinMarginThreshold: 2.0,
				CurrentBalance:     1000,
				ReplenishAmount:    50,
				MaxBalancePercent:  0.30,
			},
			expected: true,
			reason:   "low inventory with good margin",
		},
		{
			name: "zero balance",
			params: ReplenishParams{
				CurrentShares:      0,
				TargetBuffer:       100,
				SellMargin:         5.0,
				MinMarginThreshold: 2.0,
				CurrentBalance:     0, // No balance
				ReplenishAmount:    50,
				MaxBalancePercent:  0.30,
			},
			expected: false, // 0 * 0.30 = 0, can't replenish
			reason:   "would exceed balance cap",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decision := ctrl.CheckReplenish(tc.params)
			if decision.ShouldReplenish != tc.expected {
				t.Errorf("Expected ShouldReplenish=%v, got %v. Reason: %s",
					tc.expected, decision.ShouldReplenish, decision.Reason)
			}
			if decision.Reason != tc.reason {
				t.Errorf("Expected reason '%s', got '%s'", tc.reason, decision.Reason)
			}
		})
	}
}

func TestReplenishController_CheckReplenish_InitialShares(t *testing.T) {
	ctrl := NewReplenishController()

	tests := []struct {
		name           string
		currentShares  float64
		initialShares  float64
		expectedReplen bool
		expectedAmount float64
		expectedReason string
	}{
		{
			name:           "below initial, should replenish exact difference",
			currentShares:  8,
			initialShares:  15,
			expectedReplen: true,
			expectedAmount: 7, // 15 - 8 = 7
			expectedReason: "low inventory with good margin",
		},
		{
			name:           "at initial, should not replenish",
			currentShares:  15,
			initialShares:  15,
			expectedReplen: false,
			expectedAmount: 0,
			expectedReason: "inventory at or above initial amount",
		},
		{
			name:           "above initial, should not replenish",
			currentShares:  20,
			initialShares:  15,
			expectedReplen: false,
			expectedAmount: 0,
			expectedReason: "inventory at or above initial amount",
		},
		{
			name:           "zero shares remaining, replenish full initial",
			currentShares:  0,
			initialShares:  15,
			expectedReplen: true,
			expectedAmount: 15, // Full replenish
			expectedReason: "low inventory with good margin",
		},
		{
			name:           "just below initial, replenish small amount",
			currentShares:  14,
			initialShares:  15,
			expectedReplen: true,
			expectedAmount: 1,
			expectedReason: "low inventory with good margin",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params := ReplenishParams{
				CurrentShares:      tc.currentShares,
				TargetBuffer:       100,
				InitialShares:      tc.initialShares,
				SellMargin:         5.0, // Good margin
				MinMarginThreshold: 2.0,
				CurrentBalance:     1000,
				ReplenishAmount:    50, // Ignored when InitialShares > 0
				MaxBalancePercent:  0.30,
			}

			decision := ctrl.CheckReplenish(params)

			if decision.ShouldReplenish != tc.expectedReplen {
				t.Errorf("Expected ShouldReplenish=%v, got %v. Reason: %s",
					tc.expectedReplen, decision.ShouldReplenish, decision.Reason)
			}
			if decision.Amount != tc.expectedAmount {
				t.Errorf("Expected Amount=%.2f, got %.2f", tc.expectedAmount, decision.Amount)
			}
			if decision.Reason != tc.expectedReason {
				t.Errorf("Expected reason '%s', got '%s'", tc.expectedReason, decision.Reason)
			}
		})
	}
}

func TestReplenishController_CheckReplenish_InitialShares_BalanceCap(t *testing.T) {
	ctrl := NewReplenishController()

	// Test that balance cap is still respected with InitialShares
	params := ReplenishParams{
		CurrentShares:      5,
		TargetBuffer:       100,
		InitialShares:      50, // Want to replenish 45 shares
		SellMargin:         5.0,
		MinMarginThreshold: 2.0,
		CurrentBalance:     100,  // Small balance
		ReplenishAmount:    50,   // Ignored
		MaxBalancePercent:  0.30, // Cap at 30 shares (100 * 0.30)
	}

	decision := ctrl.CheckReplenish(params)

	// 5 + 45 = 50, which exceeds 30 cap
	if decision.ShouldReplenish {
		t.Error("Expected ShouldReplenish=false when would exceed balance cap")
	}
	if decision.Reason != "would exceed balance cap" {
		t.Errorf("Expected reason 'would exceed balance cap', got '%s'", decision.Reason)
	}
}

func TestReplenishController_CheckReplenish_InitialShares_MarginCheck(t *testing.T) {
	ctrl := NewReplenishController()

	// Test that margin threshold is still checked with InitialShares
	params := ReplenishParams{
		CurrentShares:      5,
		TargetBuffer:       100,
		InitialShares:      15,
		SellMargin:         1.0, // Below threshold
		MinMarginThreshold: 2.0,
		CurrentBalance:     1000,
		ReplenishAmount:    50,
		MaxBalancePercent:  0.30,
	}

	decision := ctrl.CheckReplenish(params)

	if decision.ShouldReplenish {
		t.Error("Expected ShouldReplenish=false when margin below threshold")
	}
	if decision.Reason != "margin below threshold" {
		t.Errorf("Expected reason 'margin below threshold', got '%s'", decision.Reason)
	}
}
