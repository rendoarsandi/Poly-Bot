package paper

import (
	"testing"
)

// TestRiskManager_KillSwitch_ExposurePlusUnmatched verifies PRD kill switch condition
func TestRiskManager_KillSwitch_ExposurePlusUnmatched(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBookWithRealism(0, 0) // No delay for tests
	outcomes := []string{"Up", "Down"}

	config := RiskConfig{
		MaxExposure:        100.0, // Low threshold for testing
		MaxUnmatchedRatio:  0.15,  // 15% unmatched triggers (with exposure)
		MaxUnmatchedShares: 500.0,
		SkewThreshold:      0.10,
		KillSwitchDrawdown: 0.50, // High so it doesn't trigger
	}

	rm := NewRiskManager(config, engine, orderBook, outcomes)

	// Create imbalanced positions that exceed both thresholds
	// $150 exposure (> $100) AND >15% unmatched
	_, _ = engine.BuyForMarket("", "Up", 0.50, 200)   // 200 Up shares = $100
	_, _ = engine.BuyForMarket("", "Down", 0.50, 100) // 100 Down shares = $50
	// Total exposure: $150, Unmatched: 100/300 = 33%
	action, reason := rm.Evaluate()
	if action != RiskActionKillSwitch {
		t.Errorf("Expected kill switch for exposure+unmatched, got %s: %s", action, reason)
	}
}

func TestRiskManager_KillSwitchAggregatesMarketScopedPositions(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBookWithRealism(0, 0)
	outcomes := []string{"Up", "Down"}

	config := RiskConfig{
		MaxExposure:        100.0,
		MaxUnmatchedRatio:  0.15,
		MaxUnmatchedShares: 500.0,
		SkewThreshold:      0.10,
		KillSwitchDrawdown: 0.50,
	}

	rm := NewRiskManager(config, engine, orderBook, outcomes)
	_, _ = engine.BuyForMarket("btc-updown-5m-1", "Up", 0.50, 200)
	_, _ = engine.BuyForMarket("btc-updown-5m-1", "Down", 0.50, 100)

	action, reason := rm.Evaluate()
	if action != RiskActionKillSwitch {
		t.Errorf("expected kill switch for market-scoped exposure+unmatched, got %s: %s", action, reason)
	}
}

// TestRiskManager_KillSwitch_UnmatchedSharesAbsolute verifies absolute unmatched limit
func TestRiskManager_KillSwitch_UnmatchedSharesAbsolute(t *testing.T) {
	engine := NewEngine(10000.0)
	orderBook := NewOrderBookWithRealism(0, 0)
	outcomes := []string{"Up", "Down"}

	config := RiskConfig{
		MaxExposure:        5000.0,
		MaxUnmatchedRatio:  0.50,  // High so it doesn't trigger
		MaxUnmatchedShares: 100.0, // Low threshold
		SkewThreshold:      0.10,
		KillSwitchDrawdown: 0.50,
	}

	rm := NewRiskManager(config, engine, orderBook, outcomes)

	// Create >100 unmatched shares
	_, _ = engine.BuyForMarket("", "Up", 0.50, 200)  // 200 Up
	_, _ = engine.BuyForMarket("", "Down", 0.50, 50) // 50 Down
	// Unmatched: 150 > 100 threshold
	action, reason := rm.Evaluate()
	if action != RiskActionKillSwitch {
		t.Errorf("Expected kill switch for unmatched shares, got %s: %s", action, reason)
	}
}

// TestRiskManager_ReduceSize verifies reduce size recommendation
func TestRiskManager_ReduceSize(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBookWithRealism(0, 0)
	outcomes := []string{"Up", "Down"}

	config := RiskConfig{
		MaxExposure:        100.0,
		MaxUnmatchedRatio:  0.50, // High so PRD kill doesn't trigger
		MaxUnmatchedShares: 500.0,
		SkewThreshold:      0.10,
		KillSwitchDrawdown: 0.50,
	}

	rm := NewRiskManager(config, engine, orderBook, outcomes)

	// Create balanced positions that exceed exposure only
	_, _ = engine.BuyForMarket("", "Up", 0.50, 100)   // $50
	_, _ = engine.BuyForMarket("", "Down", 0.50, 100) // $50
	// Total: $100, but with open orders it will exceed
	// Add open order value
	orderBook.PlaceOrder("Up", "buy", 0.50, 50, 1) // $25 more

	action, _ := rm.Evaluate()
	if action != RiskActionReduceSize {
		t.Errorf("Expected reduce size action, got %s", action)
	}
}

// TestRiskManager_Rebalance verifies skew detection
func TestRiskManager_Rebalance(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBookWithRealism(0, 0)
	outcomes := []string{"Up", "Down"}

	config := RiskConfig{
		MaxExposure:        1000.0, // High so it doesn't trigger
		MaxUnmatchedRatio:  0.50,
		MaxUnmatchedShares: 500.0,
		SkewThreshold:      0.10, // 10% skew triggers rebalance
		KillSwitchDrawdown: 0.50,
	}

	rm := NewRiskManager(config, engine, orderBook, outcomes)

	// Create skewed positions (>10% imbalance)
	_, _ = engine.BuyForMarket("", "Up", 0.50, 100)  // 100 Up
	_, _ = engine.BuyForMarket("", "Down", 0.50, 70) // 70 Down
	// Skew: 30/170 = 17.6% > 10%
	action, reason := rm.Evaluate()
	if action != RiskActionRebalance {
		t.Errorf("Expected rebalance action, got %s: %s", action, reason)
	}
}

// TestRiskManager_NoAction verifies healthy state returns no action
func TestRiskManager_NoAction(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBookWithRealism(0, 0)
	outcomes := []string{"Up", "Down"}

	config := DefaultRiskConfig()
	rm := NewRiskManager(config, engine, orderBook, outcomes)

	// Create small, balanced positions
	_, _ = engine.BuyForMarket("", "Up", 0.50, 10)   // $5
	_, _ = engine.BuyForMarket("", "Down", 0.50, 10) // $5

	action, _ := rm.Evaluate()
	if action != RiskActionNone {
		t.Errorf("Expected no action for healthy state, got %s", action)
	}
}

// TestRiskManager_CanPlaceOrder verifies order placement check
func TestRiskManager_CanPlaceOrder(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBookWithRealism(0, 0)
	outcomes := []string{"Up", "Down"}

	config := RiskConfig{
		MaxExposure:        100.0,
		MaxUnmatchedRatio:  0.50,
		MaxUnmatchedShares: 500.0,
		SkewThreshold:      0.10,
		KillSwitchDrawdown: 0.50,
	}

	rm := NewRiskManager(config, engine, orderBook, outcomes)

	// Should allow small order
	if !rm.CanPlaceOrder(10.0) {
		t.Error("Should allow $10 order when exposure is low")
	}

	// Add existing exposure
	_, _ = engine.BuyForMarket("", "Up", 0.50, 180) // $90 exposure

	// Should reject order that would exceed limit
	if rm.CanPlaceOrder(20.0) {
		t.Error("Should reject $20 order when it would exceed $100 limit")
	}

	// Should allow small order that fits
	if !rm.CanPlaceOrder(5.0) {
		t.Error("Should allow $5 order that fits within limit")
	}
}

// TestRiskManager_CanPlaceOrder_AfterKillSwitch verifies orders blocked after kill switch
func TestRiskManager_CanPlaceOrder_AfterKillSwitch(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBookWithRealism(0, 0)
	outcomes := []string{"Up", "Down"}

	config := RiskConfig{
		MaxExposure:        100.0,
		MaxUnmatchedRatio:  0.10, // Low threshold
		MaxUnmatchedShares: 50.0,
		SkewThreshold:      0.10,
		KillSwitchDrawdown: 0.50,
	}

	rm := NewRiskManager(config, engine, orderBook, outcomes)

	// Trigger kill switch
	_, _ = engine.BuyForMarket("", "Up", 0.50, 200)  // 200 Up
	_, _ = engine.BuyForMarket("", "Down", 0.50, 50) // 50 Down
	rm.Evaluate()                                    // This should trigger kill switch

	// Now no orders should be allowed
	if rm.CanPlaceOrder(1.0) {
		t.Error("Should reject all orders after kill switch")
	}
}

// TestRiskManager_KillSwitchDisabled verifies kill switch conditions degrade to non-kill actions when disabled.
func TestRiskManager_KillSwitchDisabled(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBookWithRealism(0, 0)
	outcomes := []string{"Up", "Down"}

	config := RiskConfig{
		DisableKillSwitch:  true,
		MaxExposure:        100.0,
		MaxUnmatchedRatio:  0.10,
		MaxUnmatchedShares: 50.0,
		SkewThreshold:      0.10,
		KillSwitchDrawdown: 0.01,
	}

	rm := NewRiskManager(config, engine, orderBook, outcomes)

	_, _ = engine.BuyForMarket("", "Up", 0.50, 200)
	_, _ = engine.BuyForMarket("", "Down", 0.50, 50)

	action, _ := rm.Evaluate()
	if action == RiskActionKillSwitch {
		t.Fatal("Kill switch should not trigger when disabled")
	}
	if rm.IsKillSwitchTriggered() {
		t.Fatal("Kill switch state should remain inactive when disabled")
	}
	if action != RiskActionReduceSize {
		t.Fatalf("Expected reduce size when kill switch is disabled, got %s", action)
	}
}

// TestRiskManager_GetSkewAdjustment verifies skew adjustment calculation
func TestRiskManager_GetSkewAdjustment(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBookWithRealism(0, 0)
	outcomes := []string{"Up", "Down"}

	config := DefaultRiskConfig()
	rm := NewRiskManager(config, engine, orderBook, outcomes)

	// Create imbalanced positions
	_, _ = engine.BuyForMarket("", "Up", 0.50, 150) // Heavy on Up
	_, _ = engine.BuyForMarket("", "Down", 0.50, 50)

	lightSide, adjustment := rm.GetSkewAdjustment()

	// Should recommend bidding more for Down (the light side)
	if lightSide != "Down" {
		t.Errorf("Expected light side Down, got %s", lightSide)
	}
	if adjustment <= 0 {
		t.Errorf("Expected positive adjustment, got %.4f", adjustment)
	}
	if adjustment > 0.05 {
		t.Errorf("Adjustment should be capped at 0.05, got %.4f", adjustment)
	}
}

// TestRiskManager_GetSkewAdjustment_Balanced verifies no adjustment when balanced
func TestRiskManager_GetSkewAdjustment_Balanced(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBookWithRealism(0, 0)
	outcomes := []string{"Up", "Down"}

	config := DefaultRiskConfig()
	rm := NewRiskManager(config, engine, orderBook, outcomes)

	// Create balanced positions
	_, _ = engine.BuyForMarket("", "Up", 0.50, 100)
	_, _ = engine.BuyForMarket("", "Down", 0.50, 100)

	_, adjustment := rm.GetSkewAdjustment()

	if adjustment != 0 {
		t.Errorf("Expected no adjustment for balanced positions, got %.4f", adjustment)
	}
}

// TestRiskManager_Reset verifies reset clears kill switch
func TestRiskManager_Reset(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBookWithRealism(0, 0)
	outcomes := []string{"Up", "Down"}

	config := RiskConfig{
		MaxExposure:        10.0, // Very low to trigger easily
		MaxUnmatchedRatio:  0.01,
		MaxUnmatchedShares: 10.0,
		SkewThreshold:      0.01,
		KillSwitchDrawdown: 0.01,
	}

	rm := NewRiskManager(config, engine, orderBook, outcomes)

	// Trigger kill switch
	_, _ = engine.BuyForMarket("", "Up", 0.50, 100)
	rm.Evaluate()

	if !rm.IsKillSwitchTriggered() {
		t.Error("Kill switch should be triggered")
	}

	// Reset
	rm.Reset()

	if rm.IsKillSwitchTriggered() {
		t.Error("Kill switch should be cleared after reset")
	}

	// Alerts should also be cleared
	if len(rm.GetAlerts()) != 0 {
		t.Error("Alerts should be cleared after reset")
	}
}

// TestRiskManager_ExecuteKillSwitch verifies kill switch execution
func TestRiskManager_ExecuteKillSwitch(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBookWithRealism(0, 0) // No delay
	outcomes := []string{"Up", "Down"}

	config := DefaultRiskConfig()
	rm := NewRiskManager(config, engine, orderBook, outcomes)

	// Add some open orders
	orderBook.PlaceOrder("Up", "buy", 0.50, 100, 1)
	orderBook.PlaceOrder("Down", "buy", 0.48, 100, 2)

	openBefore := len(orderBook.GetOpenOrders())
	if openBefore != 2 {
		t.Errorf("Expected 2 open orders before kill switch, got %d", openBefore)
	}

	// Execute kill switch
	rm.ExecuteKillSwitch()

	openAfter := len(orderBook.GetOpenOrders())
	if openAfter != 0 {
		t.Errorf("Expected 0 open orders after kill switch, got %d", openAfter)
	}
}

// TestDefaultRiskConfig verifies default values
func TestDefaultRiskConfig(t *testing.T) {
	config := DefaultRiskConfig()

	if config.MaxExposure != 500.0 {
		t.Errorf("Default MaxExposure should be $500, got $%.2f", config.MaxExposure)
	}
	if config.DisableKillSwitch {
		t.Error("Default DisableKillSwitch should be false")
	}
	if config.MaxUnmatchedRatio != 0.20 {
		t.Errorf("Default MaxUnmatchedRatio should be 0.20, got %.2f", config.MaxUnmatchedRatio)
	}
	if config.MaxUnmatchedShares != 500.0 {
		t.Errorf("Default MaxUnmatchedShares should be 500, got %.0f", config.MaxUnmatchedShares)
	}
	if config.SkewThreshold != 0.10 {
		t.Errorf("Default SkewThreshold should be 0.10, got %.2f", config.SkewThreshold)
	}
	if config.KillSwitchDrawdown != 0.10 {
		t.Errorf("Default KillSwitchDrawdown should be 0.10, got %.2f", config.KillSwitchDrawdown)
	}
}

// TestRiskAction_Values verifies RiskAction constants
func TestRiskAction_Values(t *testing.T) {
	if RiskActionNone != "none" {
		t.Error("RiskActionNone should be 'none'")
	}
	if RiskActionRebalance != "rebalance" {
		t.Error("RiskActionRebalance should be 'rebalance'")
	}
	if RiskActionReduceSize != "reduce_size" {
		t.Error("RiskActionReduceSize should be 'reduce_size'")
	}
	if RiskActionKillSwitch != "kill_switch" {
		t.Error("RiskActionKillSwitch should be 'kill_switch'")
	}
}

// Benchmark risk evaluation
func BenchmarkRiskManager_Evaluate(b *testing.B) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBookWithRealism(0, 0)
	outcomes := []string{"Up", "Down"}

	config := DefaultRiskConfig()
	rm := NewRiskManager(config, engine, orderBook, outcomes)

	_, _ = engine.BuyForMarket("", "Up", 0.50, 50)
	_, _ = engine.BuyForMarket("", "Down", 0.50, 50)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rm.Evaluate()
	}
}
