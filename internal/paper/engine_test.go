package paper

import (
	"testing"
)

// TestMarketBuy_WalksTheBook verifies that MarketBuy consumes liquidity
// across multiple price levels rather than just using the top-of-book price.
func TestMarketBuy_WalksTheBook(t *testing.T) {
	engine := NewEngine(1000.0) // $1000 starting balance

	// Create order book with multiple price levels:
	// Level 1: 50 shares @ $0.45
	// Level 2: 50 shares @ $0.47
	// Level 3: 50 shares @ $0.50
	levels := []MarketLevel{
		{Price: 0.45, Size: 50},
		{Price: 0.47, Size: 50},
		{Price: 0.50, Size: 50},
	}

	// Try to buy 80 shares - should consume:
	// - 50 @ $0.45 = $22.50
	// - 30 @ $0.47 = $14.10
	// Total: $36.60, Avg price: $36.60/80 = $0.4575
	trade, avgPrice, err := engine.MarketBuy("BTC", "Up", 80, levels)

	if err != nil {
		t.Fatalf("MarketBuy failed: %v", err)
	}

	if trade == nil {
		t.Fatal("MarketBuy returned nil trade")
	}

	// Verify quantity filled
	if trade.Quantity != 80 {
		t.Errorf("Expected 80 shares filled, got %.2f", trade.Quantity)
	}

	// Verify average price (should be weighted average, not just top-of-book)
	expectedAvg := (50*0.45 + 30*0.47) / 80
	if absFloat(avgPrice-expectedAvg) > 0.001 {
		t.Errorf("Expected avg price $%.4f, got $%.4f", expectedAvg, avgPrice)
	}

	// Top-of-book would be $0.45, but we should have walked deeper
	if avgPrice == 0.45 {
		t.Error("MarketBuy used top-of-book price instead of walking the book")
	}

	// Verify balance was deducted correctly
	expectedCost := 50*0.45 + 30*0.47
	expectedBalance := 1000.0 - expectedCost
	actualBalance := engine.GetBalance()
	if absFloat(actualBalance-expectedBalance) > 0.01 {
		t.Errorf("Expected balance $%.2f, got $%.2f", expectedBalance, actualBalance)
	}
}

// TestMarketBuy_PartialFill verifies behavior when not enough liquidity exists
func TestMarketBuy_PartialFill(t *testing.T) {
	engine := NewEngine(1000.0)

	// Only 75 shares available across all levels
	levels := []MarketLevel{
		{Price: 0.45, Size: 50},
		{Price: 0.47, Size: 25},
	}

	// Try to buy 100 shares - should only get 75
	trade, _, err := engine.MarketBuy("BTC", "Up", 100, levels)

	if err != nil {
		t.Fatalf("MarketBuy failed: %v", err)
	}

	if trade == nil {
		t.Fatal("MarketBuy returned nil trade")
	}

	// Should fill only what's available (75 shares)
	if trade.Quantity != 75 {
		t.Errorf("Expected 75 shares (partial fill), got %.2f", trade.Quantity)
	}
}

// TestMarketBuy_NoLiquidity verifies error when no liquidity available
func TestMarketBuy_NoLiquidity(t *testing.T) {
	engine := NewEngine(1000.0)

	// Empty order book
	levels := []MarketLevel{}

	_, _, err := engine.MarketBuy("BTC", "Up", 100, levels)

	if err == nil {
		t.Error("Expected error for no liquidity, got nil")
	}
}

// TestMarketBuy_ZeroSizeLevels verifies it skips levels with zero size
func TestMarketBuy_ZeroSizeLevels(t *testing.T) {
	engine := NewEngine(1000.0)

	levels := []MarketLevel{
		{Price: 0.45, Size: 0},  // Empty level - should skip
		{Price: 0.47, Size: 50}, // Real liquidity
		{Price: 0.50, Size: 50},
	}

	trade, _, err := engine.MarketBuy("BTC", "Up", 80, levels)

	if err != nil {
		t.Fatalf("MarketBuy failed: %v", err)
	}

	// Should consume from levels 2 and 3 only
	if trade.Quantity != 80 {
		t.Errorf("Expected 80 shares, got %.2f", trade.Quantity)
	}
}

// TestMarketBuy_InsufficientBalance verifies balance check
func TestMarketBuy_InsufficientBalance(t *testing.T) {
	engine := NewEngine(10.0) // Only $10 balance

	levels := []MarketLevel{
		{Price: 0.50, Size: 100}, // $50 worth of shares
	}

	// Try to buy $50 worth with only $10
	_, _, err := engine.MarketBuy("BTC", "Up", 100, levels)

	if err == nil {
		t.Error("Expected insufficient balance error, got nil")
	}
}

// TestMarketBuy_UpdatesPosition verifies position is created/updated correctly
func TestMarketBuy_UpdatesPosition(t *testing.T) {
	engine := NewEngine(1000.0)

	levels := []MarketLevel{
		{Price: 0.45, Size: 50},
		{Price: 0.47, Size: 50},
	}

	// First buy
	_, _, _ = engine.MarketBuy("BTC", "Up", 50, levels)

	positions := engine.GetPositions()
	pos, ok := positions["BTC:Up"]
	if !ok {
		t.Fatal("Position not created for BTC:Up")
	}

	if pos.Quantity != 50 {
		t.Errorf("Expected 50 shares, got %.2f", pos.Quantity)
	}

	if pos.AvgPrice != 0.45 {
		t.Errorf("Expected avg price $0.45, got $%.4f", pos.AvgPrice)
	}

	// Second buy - should update position with weighted average
	levels2 := []MarketLevel{
		{Price: 0.50, Size: 50},
	}
	_, _, _ = engine.MarketBuy("BTC", "Up", 50, levels2)

	positions = engine.GetPositions()
	pos = positions["BTC:Up"]

	if pos.Quantity != 100 {
		t.Errorf("Expected 100 shares after second buy, got %.2f", pos.Quantity)
	}

	// Weighted avg: (50*0.45 + 50*0.50) / 100 = 0.475
	expectedAvg := 0.475
	if absFloat(pos.AvgPrice-expectedAvg) > 0.001 {
		t.Errorf("Expected avg price $%.4f after second buy, got $%.4f", expectedAvg, pos.AvgPrice)
	}
}

// TestMarketBuy_ComparedToSinglePrice demonstrates the difference from single-price orders
func TestMarketBuy_ComparedToSinglePrice(t *testing.T) {
	// This test shows why MarketBuy is better than BuyForMarket
	// when you need to fill a large order across multiple price levels

	// Setup: Need to buy 100 shares, but only 50 available at best price
	levels := []MarketLevel{
		{Price: 0.45, Size: 50},  // Best price, limited qty
		{Price: 0.47, Size: 100}, // More liquidity at worse price
	}

	// Using MarketBuy: walks the book and fills the full order
	engine1 := NewEngine(1000.0)
	trade, avgPrice, err := engine1.MarketBuy("BTC", "Up", 100, levels)

	if err != nil {
		t.Fatalf("MarketBuy failed: %v", err)
	}

	// Should fill full 100 shares
	if trade.Quantity != 100 {
		t.Errorf("MarketBuy: Expected 100 shares, got %.2f", trade.Quantity)
	}

	// Average price should be weighted: (50*0.45 + 50*0.47) / 100 = 0.46
	expectedAvg := (50*0.45 + 50*0.47) / 100
	if absFloat(avgPrice-expectedAvg) > 0.001 {
		t.Errorf("MarketBuy: Expected avg $%.4f, got $%.4f", expectedAvg, avgPrice)
	}

	// Using BuyForMarket: only uses single price (would need external sizing)
	engine2 := NewEngine(1000.0)
	trade2, err2 := engine2.BuyForMarket("BTC", "Up", 0.45, 100)

	if err2 != nil {
		t.Fatalf("BuyForMarket failed: %v", err2)
	}

	// BuyForMarket assumes the price is valid and fills at that price
	// In reality, only 50 shares were available at $0.45!
	// This is the "legging risk" - the trade assumes success without walking the book
	if trade2.Price != 0.45 {
		t.Errorf("BuyForMarket: Expected price $0.45, got $%.4f", trade2.Price)
	}

	t.Log("MarketBuy walks the book (realistic), BuyForMarket assumes single price (optimistic)")
	t.Logf("MarketBuy: 100 shares @ avg $%.4f = $%.2f total", avgPrice, avgPrice*100)
	t.Logf("BuyForMarket: 100 shares @ $0.45 = $45.00 (but only 50 were actually available!)")
}

// absFloat returns the absolute value (named to avoid conflict with tui.go's abs)
func absFloat(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// TestEngine_RegisterSplitInventory verifies split inventory registration
func TestEngine_RegisterSplitInventory(t *testing.T) {
	engine := NewEngine(1000.0)
	inv := NewSplitInventory()

	inv.RecordSplit("BTC", "Up", "Down", 100.0)

	// Register the split inventory
	engine.RegisterSplitInventory(inv)

	// Verify it was registered by checking equity includes split value
	equity := engine.GetEquity()
	// Balance $1000 + split value $100 (at cost) = $1100
	if equity != 1100.0 {
		t.Errorf("Expected equity $1100.00 (balance + split), got $%.2f", equity)
	}
}

// TestEngine_DeductBalance verifies balance deduction
func TestEngine_DeductBalance(t *testing.T) {
	engine := NewEngine(1000.0)

	engine.DeductBalance(100.0)
	if engine.GetBalance() != 900.0 {
		t.Errorf("Expected balance $900.00 after deduct, got $%.2f", engine.GetBalance())
	}

	// Test deduct more than balance caps at zero
	engine.DeductBalance(1000.0)
	if engine.GetBalance() != 0.0 {
		t.Errorf("Expected balance $0.00 after over-deduct, got $%.2f", engine.GetBalance())
	}
}

// TestEngine_AddBalance verifies balance addition and peak tracking
func TestEngine_AddBalance(t *testing.T) {
	engine := NewEngine(1000.0)

	engine.AddBalance(100.0)
	engine.RecalculateDrawdown()
	if engine.GetBalance() != 1100.0 {
		t.Errorf("Expected balance $1100.00 after add, got $%.2f", engine.GetBalance())
	}

	stats := engine.GetStats()
	if stats.PeakBalance != 1100.0 {
		t.Errorf("Expected peak balance $1100.00, got $%.2f", stats.PeakBalance)
	}
}

// TestEngine_SetBalance verifies balance setting for on-chain sync
func TestEngine_SetBalance(t *testing.T) {
	engine := NewEngine(1000.0)

	// Simulate on-chain balance update
	engine.SetBalance(850.0)
	engine.RecalculateDrawdown()
	if engine.GetBalance() != 850.0 {
		t.Errorf("Expected balance $850.00 after set, got $%.2f", engine.GetBalance())
	}

	// Test peak balance updates
	engine.SetBalance(1200.0)
	engine.RecalculateDrawdown()
	stats := engine.GetStats()
	if stats.PeakBalance != 1200.0 {
		t.Errorf("Expected peak balance $1200.00, got $%.2f", stats.PeakBalance)
	}
}

// TestEngine_GetEquity_WithSplitInventory verifies equity calculation includes splits
func TestEngine_GetEquity_WithSplitInventory(t *testing.T) {
	engine := NewEngine(1000.0)
	inv := NewSplitInventory()

	// Initially equity = balance
	if engine.GetEquity() != 1000.0 {
		t.Errorf("Expected initial equity $1000.00, got $%.2f", engine.GetEquity())
	}

	// Record a split
	inv.RecordSplit("BTC", "Up", "Down", 100.0)
	engine.RegisterSplitInventory(inv)

	// Equity should now include split value at cost basis ($50 + $50 = $100)
	equity := engine.GetEquity()
	if equity != 1100.0 {
		t.Errorf("Expected equity $1100.00 after split, got $%.2f", equity)
	}

	// Sell some shares (this updates split inventory but not engine balance)
	inv.RecordSell("BTC", "Up", 50.0, 0.55) // Profit: 50 * ($0.55 - $0.50) = $2.50

	// Remaining: Up=50, Down=100
	// Unrealized value: 50 * $0.50 + 100 * $0.50 = $75
	// Equity = $1000 (balance unchanged) + $75 (split value) = $1075
	equity = engine.GetEquity()
	if equity != 1075.0 {
		t.Errorf("Expected equity $1075.00 after sell, got $%.2f", equity)
	}

	// In real usage, proceeds would be added via AddBalance
	// This simulates the actual flow in cmd/paperbot/main.go
	proceeds := 50.0 * 0.55 // Sold 50 shares at $0.55
	engine.AddBalance(proceeds)

	// Now equity = $1027.50 (balance) + $75 (split) - $27.50 (realized profit already counted)
	// Actually realized P&L is separate from equity calculation
	// Equity = balance + unrealized positions + unrealized split value
	equity = engine.GetEquity()
	expectedEquity := 1000.0 + proceeds + 75.0 // Balance + proceeds + remaining split value
	if equity != expectedEquity {
		t.Errorf("Expected equity $%.2f after adding proceeds, got $%.2f", expectedEquity, equity)
	}
}

// TestEngine_MultipleSplitInventories verifies handling multiple inventories
func TestEngine_MultipleSplitInventories(t *testing.T) {
	engine := NewEngine(1000.0)

	inv1 := NewSplitInventory()
	inv1.RecordSplit("BTC", "Up", "Down", 50.0)

	inv2 := NewSplitInventory()
	inv2.RecordSplit("ETH", "Yes", "No", 30.0)

	engine.RegisterSplitInventory(inv1)
	engine.RegisterSplitInventory(inv2)

	// Equity = $1000 + $50 (BTC) + $30 (ETH) = $1080
	equity := engine.GetEquity()
	if equity != 1080.0 {
		t.Errorf("Expected equity $1080.00 with multiple inventories, got $%.2f", equity)
	}
}

// TestEngine_getSplitInventoryValue_ThreadSafe verifies thread-safe access
func TestEngine_getSplitInventoryValue_ThreadSafe(t *testing.T) {
	engine := NewEngine(1000.0)
	inv := NewSplitInventory()
	inv.RecordSplit("BTC", "Up", "Down", 100.0)
	engine.RegisterSplitInventory(inv)

	done := make(chan bool, 2)

	// Concurrent equity reads
	go func() {
		for i := 0; i < 100; i++ {
			_ = engine.GetEquity()
		}
		done <- true
	}()

	// Concurrent balance modification
	go func() {
		for i := 0; i < 100; i++ {
			engine.AddBalance(1.0)
		}
		done <- true
	}()

	<-done
	<-done

	// Verify final state is consistent
	if engine.GetEquity() != 1200.0 { // 1000 + 100 (split) + 100 (added)
		t.Errorf("Expected final equity $1200.00, got $%.2f", engine.GetEquity())
	}
}

func TestEngine_SellForMarket(t *testing.T) {
	engine := NewEngine(1000.0)
	if _, err := engine.BuyForMarket("XRP", "Up", 0.40, 5); err != nil {
		t.Fatalf("BuyForMarket failed: %v", err)
	}

	trade, err := engine.SellForMarket("XRP", "Up", 0.42, 2)
	if err != nil {
		t.Fatalf("SellForMarket failed: %v", err)
	}
	if trade == nil || trade.Outcome != "Up" || trade.Quantity != 2 {
		t.Fatalf("unexpected trade: %+v", trade)
	}

	positions := engine.GetPositions()
	if len(positions) != 1 {
		t.Fatalf("expected 1 remaining position, got %d", len(positions))
	}
	pos, ok := positions["XRP:Up"]
	if !ok {
		t.Fatalf("expected XRP:Up position, got %+v", positions)
	}
	if pos.MarketID != "XRP" || pos.Outcome != "Up" || absFloat(pos.Quantity-3) > 1e-9 {
		t.Fatalf("unexpected remaining position: %+v", pos)
	}
}

func TestEngine_RedeemWithDetails(t *testing.T) {
	engine := NewEngine(100.0)

	// Add winning position: 10 shares @ $0.60
	engine.positions["m1:Up"] = &Position{
		MarketID:  "m1",
		Outcome:   "Up",
		Quantity:  10.0,
		TotalCost: 6.0,
	}

	// Add losing position: 5 shares @ $0.40
	engine.positions["m1:Down"] = &Position{
		MarketID:  "m1",
		Outcome:   "Down",
		Quantity:  5.0,
		TotalCost: 2.0,
	}

	res := engine.RedeemWithDetails("m1", "Up")

	if res.WinningShares != 10.0 {
		t.Errorf("Expected 10 winning shares, got %f", res.WinningShares)
	}
	if res.LosingShares != 5.0 {
		t.Errorf("Expected 5 losing shares, got %f", res.LosingShares)
	}
	if res.WinningPayout != 10.0 {
		t.Errorf("Expected winning payout $10, got %f", res.WinningPayout)
	}
	if res.WinningCost != 6.0 {
		t.Errorf("Expected winning cost $6, got %f", res.WinningCost)
	}
	if res.LosingCost != 2.0 {
		t.Errorf("Expected losing cost $2, got %f", res.LosingCost)
	}
	if res.WinningPnL != 4.0 {
		t.Errorf("Expected winning PnL $4, got %f", res.WinningPnL)
	}
	if res.TotalPnL != 2.0 {
		t.Errorf("Expected total PnL $2, got %f", res.TotalPnL)
	}
}
