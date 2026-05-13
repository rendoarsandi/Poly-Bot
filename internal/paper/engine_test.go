package paper

import (
	"errors"
	"math"
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

func TestBuyFilledForMarketUsesExactObservedCostAndQuantity(t *testing.T) {
	engine := NewEngine(100.0)

	trade, err := engine.BuyFilledForMarket("BTC", "Up", 10.50, 10.0)
	if err != nil {
		t.Fatalf("BuyFilledForMarket failed: %v", err)
	}
	if math.Abs(trade.Quantity-10.0) > 0.000001 {
		t.Fatalf("expected exact net quantity 10, got %.6f", trade.Quantity)
	}
	if math.Abs(trade.Value-10.50) > 0.000001 {
		t.Fatalf("expected exact cost 10.50, got %.6f", trade.Value)
	}
	pos := engine.GetPositions()["BTC:Up"]
	if math.Abs(pos.AvgPrice-1.05) > 0.000001 {
		t.Fatalf("expected fee-inclusive avg cost 1.05, got %.6f", pos.AvgPrice)
	}
}

func TestSellFilledForMarketUsesExactObservedProceedsInRealizedPnL(t *testing.T) {
	engine := NewEngine(100.0)
	if _, err := engine.BuyFilledForMarket("BTC", "Up", 10.50, 10.0); err != nil {
		t.Fatalf("BuyFilledForMarket failed: %v", err)
	}

	trade, err := engine.SellFilledForMarket("BTC", "Up", 7.25, 10.0)
	if err != nil {
		t.Fatalf("SellFilledForMarket failed: %v", err)
	}
	if math.Abs(trade.Value-7.25) > 0.000001 {
		t.Fatalf("expected exact proceeds 7.25, got %.6f", trade.Value)
	}
	if got := engine.GetStats().RealizedPnL; math.Abs(got+3.25) > 0.000001 {
		t.Fatalf("expected realized PnL -3.25, got %.6f", got)
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

func TestEngine_SyncExternalPosition(t *testing.T) {
	engine := NewEngine(100.0)

	changed := engine.SyncExternalPosition("BTC", "Down", 2.0, 0.65)
	if !changed {
		t.Fatal("expected initial sync to report a change")
	}

	positions := engine.GetPositions()
	pos, ok := positions["BTC:Down"]
	if !ok {
		t.Fatal("expected BTC:Down position to exist after sync")
	}
	if absFloat(pos.Quantity-2.0) > 0.0001 {
		t.Fatalf("expected quantity 2.0, got %.4f", pos.Quantity)
	}
	if absFloat(pos.AvgPrice-0.65) > 0.0001 {
		t.Fatalf("expected avg price 0.65, got %.4f", pos.AvgPrice)
	}
	if absFloat(engine.GetBalance()-100.0) > 0.0001 {
		t.Fatalf("expected balance to stay unchanged, got %.4f", engine.GetBalance())
	}

	changed = engine.SyncExternalPosition("BTC", "Down", 3.0, 0.80)
	if !changed {
		t.Fatal("expected quantity increase sync to report a change")
	}

	positions = engine.GetPositions()
	pos = positions["BTC:Down"]
	if absFloat(pos.Quantity-3.0) > 0.0001 {
		t.Fatalf("expected quantity 3.0 after increase, got %.4f", pos.Quantity)
	}
	expectedCost := (2.0 * 0.65) + (1.0 * 0.80)
	if absFloat(pos.TotalCost-expectedCost) > 0.0001 {
		t.Fatalf("expected total cost %.4f, got %.4f", expectedCost, pos.TotalCost)
	}

	stats := engine.GetStats()
	if absFloat(stats.StartingBalance-102.1) > 0.0001 {
		t.Fatalf("expected pnl baseline 102.10 after increase, got %.4f", stats.StartingBalance)
	}
	if absFloat(engine.GetSizingBalance()-102.1) > 0.0001 {
		t.Fatalf("expected sizing balance 102.10 after increase, got %.4f", engine.GetSizingBalance())
	}

	changed = engine.SyncExternalPosition("BTC", "Down", 1.5, 0.80)
	if !changed {
		t.Fatal("expected quantity decrease sync to report a change")
	}

	positions = engine.GetPositions()
	pos = positions["BTC:Down"]
	if absFloat(pos.Quantity-1.5) > 0.0001 {
		t.Fatalf("expected quantity 1.5 after trim, got %.4f", pos.Quantity)
	}
	if absFloat(engine.GetBalance()-100.0) > 0.0001 {
		t.Fatalf("expected balance to remain unchanged after trim, got %.4f", engine.GetBalance())
	}
	expectedTrimmedCost := expectedCost * (1.5 / 3.0)
	if absFloat(pos.TotalCost-expectedTrimmedCost) > 0.0001 {
		t.Fatalf("expected trimmed total cost %.4f, got %.4f", expectedTrimmedCost, pos.TotalCost)
	}
	stats = engine.GetStats()
	if absFloat(stats.StartingBalance-101.05) > 0.0001 {
		t.Fatalf("expected pnl baseline 101.05 after trim, got %.4f", stats.StartingBalance)
	}
	if absFloat(engine.GetSizingBalance()-101.05) > 0.0001 {
		t.Fatalf("expected sizing balance 101.05 after trim, got %.4f", engine.GetSizingBalance())
	}
}

func TestEngine_SyncExternalPositionKeepsImportedCarryNeutralInBookPnL(t *testing.T) {
	engine := NewEngine(65.47)

	if !engine.SyncExternalPosition("BTC", "Up", 3.3029, 0.99) {
		t.Fatal("expected external sync to import carry position")
	}

	stats := engine.GetStats()
	if absFloat(stats.StartingBalance-68.739871) > 0.000001 {
		t.Fatalf("expected pnl baseline 68.739871 after import, got %.6f", stats.StartingBalance)
	}
	if absFloat(engine.GetBookEquity()-stats.StartingBalance) > 0.000001 {
		t.Fatalf("expected imported carry to remain neutral, book equity %.6f baseline %.6f", engine.GetBookEquity(), stats.StartingBalance)
	}
}

func TestEngine_SyncExternalPositionTrimsWithoutChangingRealizedPnL(t *testing.T) {
	engine := NewEngine(100.0)
	if _, err := engine.BuyForMarket("BTC", "Up", 0.99, 3.0); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	if !engine.SyncExternalPosition("BTC", "Up", 1.5, 0.50) {
		t.Fatal("expected sync trim to report a change")
	}

	stats := engine.GetStats()
	if absFloat(stats.RealizedPnL) > 0.000001 {
		t.Fatalf("expected sync trim to avoid local realized pnl, got %.6f", stats.RealizedPnL)
	}
}

func TestEngine_SyncExternalPositionWithTotalCostUsesExactBasis(t *testing.T) {
	engine := NewEngine(100.0)

	if !engine.SyncExternalPositionWithTotalCost("BTC", "Up", 2.0, 0.62) {
		t.Fatal("expected exact-cost sync to import carry position")
	}

	pos := engine.GetPositions()["BTC:Up"]
	if absFloat(pos.Quantity-2.0) > 0.000001 {
		t.Fatalf("expected quantity 2.0, got %.6f", pos.Quantity)
	}
	if absFloat(pos.TotalCost-0.62) > 0.000001 {
		t.Fatalf("expected total cost 0.62, got %.6f", pos.TotalCost)
	}
	if absFloat(pos.AvgPrice-0.31) > 0.000001 {
		t.Fatalf("expected avg price 0.31, got %.6f", pos.AvgPrice)
	}

	if !engine.SyncExternalPositionWithTotalCost("BTC", "Up", 1.25, 0.3875) {
		t.Fatal("expected exact-cost trim to report a change")
	}

	pos = engine.GetPositions()["BTC:Up"]
	if absFloat(pos.Quantity-1.25) > 0.000001 {
		t.Fatalf("expected quantity 1.25 after trim, got %.6f", pos.Quantity)
	}
	if absFloat(pos.TotalCost-0.3875) > 0.000001 {
		t.Fatalf("expected total cost 0.3875 after trim, got %.6f", pos.TotalCost)
	}
	if absFloat(pos.AvgPrice-0.31) > 0.000001 {
		t.Fatalf("expected avg price 0.31 after trim, got %.6f", pos.AvgPrice)
	}
}

func TestEngine_SyncExternalPositionCarryRoundTripRestoresSizingBase(t *testing.T) {
	engine := NewEngine(62.24)

	if !engine.SyncExternalPosition("BTC", "Up", 3.3029, 0.99) {
		t.Fatal("expected sync to import carry position")
	}

	expectedImportedSizing := 62.24 + (3.3029 * 0.99)
	if absFloat(engine.GetSizingBalance()-expectedImportedSizing) > 0.0001 {
		t.Fatalf("expected sizing balance %.4f after import, got %.4f", expectedImportedSizing, engine.GetSizingBalance())
	}

	if !engine.SyncExternalPosition("BTC", "Up", 0.0, 0.99) {
		t.Fatal("expected sync to clear imported carry position")
	}

	if absFloat(engine.GetSizingBalance()-62.24) > 0.0001 {
		t.Fatalf("expected sizing balance to return to 62.24 after carry clear, got %.4f", engine.GetSizingBalance())
	}

	stats := engine.GetStats()
	if absFloat(stats.StartingBalance-62.24) > 0.0001 {
		t.Fatalf("expected pnl baseline to return to 62.24, got %.4f", stats.StartingBalance)
	}
	if absFloat(stats.RealizedPnL) > 0.000001 {
		t.Fatalf("expected carry sync round-trip to stay neutral, got realized pnl %.6f", stats.RealizedPnL)
	}
}

func TestEngine_SyncExternalPositionCarryRoundTripPreservesProfitHighWater(t *testing.T) {
	engine := NewEngine(100.0)
	engine.UpdateCompoundMultiplier(20.0, 100.0)

	if absFloat(engine.GetSizingBalance()-120.0) > 0.0001 {
		t.Fatalf("expected initial profit high-water sizing 120.00, got %.4f", engine.GetSizingBalance())
	}

	if !engine.SyncExternalPosition("BTC", "Up", 10.0, 0.50) {
		t.Fatal("expected sync to import carry position")
	}
	if absFloat(engine.GetSizingBalance()-125.0) > 0.0001 {
		t.Fatalf("expected carry import to temporarily lift sizing to 125.00, got %.4f", engine.GetSizingBalance())
	}

	if !engine.SyncExternalPosition("BTC", "Up", 0.0, 0.50) {
		t.Fatal("expected sync to clear carry position")
	}
	if absFloat(engine.GetSizingBalance()-120.0) > 0.0001 {
		t.Fatalf("expected sizing to return to prior profit high-water 120.00, got %.4f", engine.GetSizingBalance())
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

func TestEngine_MaxDrawdownCashPreservesActualLossAcrossNewPeaks(t *testing.T) {
	engine := NewEngine(100.0)

	engine.SetBalance(90.0)
	engine.RecalculateDrawdown()

	stats := engine.GetStats()
	if absFloat(stats.MaxDrawdown-10.0) > 0.0001 {
		t.Fatalf("expected max drawdown 10.0%%, got %.4f", stats.MaxDrawdown)
	}
	if absFloat(stats.MaxDrawdownCash-10.0) > 0.0001 {
		t.Fatalf("expected max drawdown cash $10.00, got %.4f", stats.MaxDrawdownCash)
	}

	engine.SetBalance(200.0)
	engine.RecalculateDrawdown()

	stats = engine.GetStats()
	if absFloat(stats.MaxDrawdown-10.0) > 0.0001 {
		t.Fatalf("expected max drawdown percent to remain 10.0%%, got %.4f", stats.MaxDrawdown)
	}
	if absFloat(stats.MaxDrawdownCash-10.0) > 0.0001 {
		t.Fatalf("expected max drawdown cash to remain the original $10.00 loss, got %.4f", stats.MaxDrawdownCash)
	}
}

func TestEngine_RedeemWithDetailsRecordsEconomicDrawdownWhilePayoutPending(t *testing.T) {
	engine := NewEngine(20.73)

	if _, err := engine.BuyForMarket("m1", "Up", 0.38, 5.83802); err != nil {
		t.Fatalf("winning buy failed: %v", err)
	}
	if _, err := engine.BuyForMarket("m1", "Down", 0.76, 14.01275); err != nil {
		t.Fatalf("losing buy failed: %v", err)
	}

	res := engine.RedeemWithDetails("m1", "Up")
	if res.TotalPnL >= 0 {
		t.Fatalf("expected local redemption to be an economic loss before live cash correction, got %.4f", res.TotalPnL)
	}
	if got := engine.GetPendingRedemptions()["m1"]; got <= 0 {
		t.Fatalf("expected pending redemption payout, got %.4f", got)
	}

	stats := engine.GetStats()
	if absFloat(stats.MaxDrawdownCash-7.0301176) > 0.0001 {
		t.Fatalf("expected pending redemption to record economic drawdown 7.0301, got %.4f", stats.MaxDrawdownCash)
	}
}

func TestEngine_SettlePendingRedemptionRecordsDrawdownAfterPayoutFinalizes(t *testing.T) {
	engine := NewEngine(100.0)

	if _, err := engine.BuyForMarket("m1", "Up", 0.90, 10.0); err != nil {
		t.Fatalf("winning buy failed: %v", err)
	}
	if _, err := engine.BuyForMarket("m1", "Down", 0.90, 80.0); err != nil {
		t.Fatalf("losing buy failed: %v", err)
	}

	res := engine.RedeemWithDetails("m1", "Up")
	if absFloat(res.TotalPnL+71.0) > 0.0001 {
		t.Fatalf("expected local redemption loss -71.00, got %.4f", res.TotalPnL)
	}
	if got := engine.GetStats().MaxDrawdownCash; absFloat(got-71.0) > 0.0001 {
		t.Fatalf("expected max drawdown before pending payout settles, got %.4f", got)
	}

	settled := engine.SettlePendingRedemption("m1")
	if absFloat(settled-10.0) > 0.0001 {
		t.Fatalf("expected settled payout 10.00, got %.4f", settled)
	}
	stats := engine.GetStats()
	if absFloat(stats.MaxDrawdownCash-71.0) > 0.0001 {
		t.Fatalf("expected settled loss to record max drawdown 71.00, got %.4f", stats.MaxDrawdownCash)
	}
}

func TestEngine_PeakExposurePreservesSessionHigh(t *testing.T) {
	engine := NewEngine(100.0)

	if !engine.SyncExternalPosition("m1", "Up", 20, 0.5) {
		t.Fatal("expected initial external position sync to change exposure")
	}

	stats := engine.GetStats()
	if absFloat(stats.PeakExposure-10.0) > 0.0001 {
		t.Fatalf("expected peak exposure $10.00, got %.4f", stats.PeakExposure)
	}

	if !engine.SyncExternalPosition("m1", "Up", 4, 0.5) {
		t.Fatal("expected reduced external position sync to change exposure")
	}

	stats = engine.GetStats()
	if absFloat(stats.PeakExposure-10.0) > 0.0001 {
		t.Fatalf("expected peak exposure to preserve the session high $10.00, got %.4f", stats.PeakExposure)
	}
}

func TestEngine_ResetPaperSession(t *testing.T) {
	engine := NewEngine(100.0)

	engine.AddRealizedPnL(12.5)
	engine.UpdateCompoundMultiplier(15.0, 100.0)
	engine.SetBalance(87.4)

	if err := engine.ResetPaperSession(250.25); err != nil {
		t.Fatalf("ResetPaperSession failed: %v", err)
	}

	stats := engine.GetStats()
	if absFloat(stats.CurrentBalance-250.25) > 0.0001 {
		t.Fatalf("expected reset balance 250.25, got %.4f", stats.CurrentBalance)
	}
	if absFloat(stats.StartingBalance-250.25) > 0.0001 {
		t.Fatalf("expected reset baseline 250.25, got %.4f", stats.StartingBalance)
	}
	if absFloat(stats.RealizedPnL) > 0.0001 {
		t.Fatalf("expected realized pnl reset to 0, got %.4f", stats.RealizedPnL)
	}
	if stats.TotalTrades != 0 || stats.WinningTrades != 0 || stats.LosingTrades != 0 {
		t.Fatalf("expected trade stats reset, got %+v", stats)
	}
	if got := engine.GetCompoundMultiplier(); absFloat(got-1.0) > 0.0001 {
		t.Fatalf("expected compound multiplier reset to 1.0, got %.4f", got)
	}
	if _, rounds, profitable, losing, sizing := engine.GetCompoundStats(); rounds != 0 || profitable != 0 || losing != 0 || absFloat(sizing-250.25) > 0.0001 {
		t.Fatalf("unexpected compound stats after reset: rounds=%d profitable=%d losing=%d sizing=%.4f", rounds, profitable, losing, sizing)
	}
}

func TestEngine_ResetPaperSessionRequiresFlatBook(t *testing.T) {
	engine := NewEngine(100.0)
	if _, err := engine.BuyForMarket("BTC#m1", "Up", 0.5, 10); err != nil {
		t.Fatalf("buy failed: %v", err)
	}

	err := engine.ResetPaperSession(200.0)
	if !errors.Is(err, ErrPaperSessionNotFlat) {
		t.Fatalf("expected ErrPaperSessionNotFlat, got %v", err)
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
	// This simulates the actual flow in cmd/realbot/main.go
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

func TestEngine_GetBookEquityKeepsOpenPositionsAtCostBasis(t *testing.T) {
	engine := NewEngine(100.0)
	if _, err := engine.BuyForMarket("ETH", "Up", 0.99, 3); err != nil {
		t.Fatalf("BuyForMarket failed: %v", err)
	}
	engine.UpdateMarketBidAsk("ETH", "Up", 0.08, 0.09)

	if got := engine.GetEquity(); math.Abs(got-97.27) > 0.000001 {
		t.Fatalf("expected marked equity 97.27 after bid drop, got %.2f", got)
	}
	if got := engine.GetBookEquity(); math.Abs(got-100.0) > 0.000001 {
		t.Fatalf("expected book equity to stay neutral at cost basis, got %.2f", got)
	}
}

func TestEngine_GetBookEquityKeepsMatchedPairsNeutralUntilRealized(t *testing.T) {
	engine := NewEngine(100.0)
	if _, err := engine.BuyForMarket("ETH", "Up", 0.48, 3.1); err != nil {
		t.Fatalf("BuyForMarket Up failed: %v", err)
	}
	if _, err := engine.BuyForMarket("ETH", "Down", 0.49, 3.1); err != nil {
		t.Fatalf("BuyForMarket Down failed: %v", err)
	}

	expected := 100.0
	if got := engine.GetBookEquity(); math.Abs(got-expected) > 0.000001 {
		t.Fatalf("expected matched pair book equity %.6f until realized, got %.6f", expected, got)
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

func TestEngine_GetPositionsWithPnL_KeepsSameAssetRoundsSeparate(t *testing.T) {
	engine := NewEngine(1000.0)

	if _, err := engine.BuyForMarket("BTC#old12345", "Up", 0.40, 10); err != nil {
		t.Fatalf("BuyForMarket old round failed: %v", err)
	}
	if _, err := engine.BuyForMarket("BTC#new67890", "Up", 0.60, 10); err != nil {
		t.Fatalf("BuyForMarket new round failed: %v", err)
	}

	engine.UpdateMarketBidAsk("BTC#old12345", "Up", 0.70, 0.71)
	engine.UpdateMarketBidAsk("BTC#new67890", "Up", 0.20, 0.21)

	positions := engine.GetPositionsWithPnL()

	oldPos, ok := positions["BTC#old12345:Up"]
	if !ok {
		t.Fatalf("missing old-round position: %+v", positions)
	}
	newPos, ok := positions["BTC#new67890:Up"]
	if !ok {
		t.Fatalf("missing new-round position: %+v", positions)
	}

	if absFloat(oldPos.CurrentBid-0.70) > 1e-9 {
		t.Fatalf("expected old round bid 0.70, got %.4f", oldPos.CurrentBid)
	}
	if absFloat(newPos.CurrentBid-0.20) > 1e-9 {
		t.Fatalf("expected new round bid 0.20, got %.4f", newPos.CurrentBid)
	}
	if absFloat(oldPos.UnrealizedPnL-3.0) > 1e-9 {
		t.Fatalf("expected old round pnl +3.0, got %.4f", oldPos.UnrealizedPnL)
	}
	if absFloat(newPos.UnrealizedPnL+4.0) > 1e-9 {
		t.Fatalf("expected new round pnl -4.0, got %.4f", newPos.UnrealizedPnL)
	}
}

func TestEngine_GetPositionsWithPnL_ClearedMarketBidSuppressesOutcomeFallback(t *testing.T) {
	engine := NewEngine(1000.0)

	if _, err := engine.BuyForMarket("BTC#stale", "Down", 0.44, 10); err != nil {
		t.Fatalf("BuyForMarket failed: %v", err)
	}

	engine.UpdateBidAsk("Down", 0.20, 0.21)
	engine.UpdateMarketBidAsk("BTC#stale", "Down", 0, 0.01)

	positions := engine.GetPositionsWithPnL()
	pos, ok := positions["BTC#stale:Down"]
	if !ok {
		t.Fatalf("missing position with pnl: %+v", positions)
	}

	if pos.CurrentBid != 0 {
		t.Fatalf("expected cleared market bid to suppress stale fallback, got %.4f", pos.CurrentBid)
	}
	if absFloat(pos.CurrentAsk-0.01) > 1e-9 {
		t.Fatalf("expected ask to remain visible after bid clear, got %.4f", pos.CurrentAsk)
	}
	if pos.MarketValue != 0 || pos.UnrealizedPnL != 0 {
		t.Fatalf("expected cleared bid to avoid stale MTM, got value=%.4f pnl=%.4f", pos.MarketValue, pos.UnrealizedPnL)
	}
	if absFloat(engine.GetUnrealizedPnL()) > 1e-9 {
		t.Fatalf("expected engine unrealized pnl to ignore cleared bid, got %.4f", engine.GetUnrealizedPnL())
	}
	if absFloat(engine.GetEquity()-1000.0) > 1e-9 {
		t.Fatalf("expected equity to fall back to cost basis after bid clear, got %.4f", engine.GetEquity())
	}
}

func TestEngine_ClearMarketDataPreservesOpenInventoryAndPendingRedemptions(t *testing.T) {
	engine := NewEngine(100.0)
	if _, err := engine.BuyForMarket("BTC#1775911200", "Up", 0.60, 5); err != nil {
		t.Fatalf("BuyForMarket failed: %v", err)
	}
	engine.UpdateMarketBidAsk("BTC#1775911200", "Up", 0.70, 0.71)
	engine.SetPendingRedemption("BTC#old", 3.25)

	engine.ClearMarketData()

	positions := engine.GetPositions()
	pos, ok := positions["BTC#1775911200:Up"]
	if !ok {
		t.Fatalf("expected unresolved position to survive round cleanup, got %+v", positions)
	}
	if absFloat(pos.Quantity-5.0) > 1e-9 {
		t.Fatalf("expected unresolved quantity 5.0 after cleanup, got %.4f", pos.Quantity)
	}

	pending := engine.GetPendingRedemptions()
	if got := pending["BTC#old"]; absFloat(got-3.25) > 1e-9 {
		t.Fatalf("expected pending redemption 3.25 to survive cleanup, got %.4f", got)
	}

	withPnL := engine.GetPositionsWithPnL()
	if got := withPnL["BTC#1775911200:Up"].CurrentBid; absFloat(got-0.70) > 1e-9 {
		t.Fatalf("expected active market bid 0.70 to survive cleanup, got %.4f", got)
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

func TestEngine_RedeemDoesNotClearMultipleMarkets(t *testing.T) {
	engine := NewEngine(100.0)
	if _, err := engine.BuyForMarket("m1", "Up", 0.50, 10); err != nil {
		t.Fatalf("buy m1 failed: %v", err)
	}
	if _, err := engine.BuyForMarket("m2", "Down", 0.50, 10); err != nil {
		t.Fatalf("buy m2 failed: %v", err)
	}

	if payout := engine.Redeem("Up"); payout != 0 {
		t.Fatalf("expected ambiguous legacy redeem to no-op, got payout %.2f", payout)
	}
	if got := len(engine.GetPositions()); got != 2 {
		t.Fatalf("expected both market positions to remain, got %d", got)
	}
}

func TestEngine_GetStatsRealizedPnLIgnoresBalanceSyncWhileFlat(t *testing.T) {
	engine := NewEngine(100.0)

	engine.SetBalance(80.0)

	stats := engine.GetStats()
	if absFloat(stats.RealizedPnL) > 0.0001 {
		t.Fatalf("expected realized pnl to stay 0.00 while flat, got %.4f", stats.RealizedPnL)
	}
}

func TestEngine_BuySellWithExplicitFeeRateKeepsRealizedAlignedWithEquity(t *testing.T) {
	engine := NewEngine(30.0)

	// Buy 10 shares at 0.50 with 100 bps fee (1.0%)
	// fee_usdc = 10 * 0.01 * 0.5 * 0.5 = 0.025
	// fee_shares = 0.025 / 0.5 = 0.05
	// net_shares = 10 - 0.05 = 9.95
	// cost = 5.0
	buy, err := engine.BuyForMarketWithFeeRate("m1", "Up", 0.50, 10.0, 100)
	if err != nil {
		t.Fatalf("buy failed: %v", err)
	}
	if buy.Quantity >= 10.0 {
		t.Fatalf("expected buy quantity to be reduced by fee, got %.6f", buy.Quantity)
	}
	if absFloat(buy.Quantity-9.95) > 0.000001 {
		t.Fatalf("expected net shares 9.95, got %.6f", buy.Quantity)
	}
	if absFloat(engine.GetBalance()-25.0) > 0.000001 {
		t.Fatalf("expected balance after buy to be 25.0, got %.6f", engine.GetBalance())
	}

	// Sell 9.95 shares at 0.60 with 100 bps fee
	// fee_usdc = 9.95 * 0.01 * 0.6 * 0.4 = 0.02388
	// proceeds = (9.95 * 0.6) - 0.02388 = 5.97 - 0.02388 = 5.94612
	sell, err := engine.SellForMarketWithFeeRate("m1", "Up", 0.60, buy.Quantity, 100)
	if err != nil {
		t.Fatalf("sell failed: %v", err)
	}

	stats := engine.GetStats()
	equity := engine.GetBookEquity()
	netChange := equity - stats.StartingBalance
	// Realized PnL = proceeds (5.94612) - costBasis (5.0) = 0.94612
	if absFloat(stats.RealizedPnL-0.94612) > 0.000001 {
		t.Fatalf("expected realized pnl 0.94612, got %.6f", stats.RealizedPnL)
	}
	if absFloat(stats.RealizedPnL-netChange) > 0.000001 {
		t.Fatalf("expected realized pnl %.6f to match net change %.6f after fee-aware round-trip", stats.RealizedPnL, netChange)
	}
	if absFloat(sell.Value-(engine.GetBalance()-25.0)) > 0.000001 {
		t.Fatalf("expected sell proceeds %.6f to reconcile with ending cash %.6f", sell.Value, engine.GetBalance())
	}
}

func TestEngine_SyncBalanceNeutralBooksFlatCashDriftAsRealizedPnL(t *testing.T) {
	engine := NewEngine(100.0)

	neutralized := engine.SyncBalanceNeutral(130.0)
	if absFloat(neutralized) > 0.0001 {
		t.Fatalf("expected flat drift not to be auto-neutralized, got %.4f", neutralized)
	}

	stats := engine.GetStats()
	if absFloat(stats.StartingBalance-100.0) > 0.0001 {
		t.Fatalf("expected flat sync to keep baseline at 100.00, got %.4f", stats.StartingBalance)
	}
	if absFloat(stats.RealizedPnL-30.0) > 0.0001 {
		t.Fatalf("expected flat sync drift to be realized as +30.00, got %.4f", stats.RealizedPnL)
	}
	if absFloat(engine.GetBookEquity()-130.0) > 0.0001 {
		t.Fatalf("expected book equity to follow synced cash 130.00, got %.4f", engine.GetBookEquity())
	}
}

func TestEngine_SyncBalanceNeutralDetailedReportsAccountingPath(t *testing.T) {
	engine := NewEngine(100.0)

	result := engine.SyncBalanceNeutralDetailed(99.25)

	if absFloat(result.Delta+0.75) > 0.0001 {
		t.Fatalf("expected raw delta -0.75, got %.4f", result.Delta)
	}
	if result.ShouldNeutralize {
		t.Fatal("expected flat balance drift to use realized path")
	}
	if absFloat(result.RealizedDelta+0.75) > 0.0001 {
		t.Fatalf("expected realized delta -0.75, got %.4f", result.RealizedDelta)
	}
	if absFloat(result.NeutralizedDelta) > 0.0001 {
		t.Fatalf("expected no neutralized delta, got %.4f", result.NeutralizedDelta)
	}
	if absFloat(result.BookEquity-99.25) > 0.0001 {
		t.Fatalf("expected result book equity 99.25, got %.4f", result.BookEquity)
	}
	if absFloat(result.RealizedPnL+0.75) > 0.0001 {
		t.Fatalf("expected result realized pnl -0.75, got %.4f", result.RealizedPnL)
	}
}

func TestEngine_RebaseBalanceKeepsFlatCashTransfersNeutral(t *testing.T) {
	engine := NewEngine(100.0)

	neutralized := engine.RebaseBalance(130.0)
	if absFloat(neutralized-30.0) > 0.0001 {
		t.Fatalf("expected deposit rebased by 30.00, got %.4f", neutralized)
	}

	stats := engine.GetStats()
	if absFloat(stats.StartingBalance-130.0) > 0.0001 {
		t.Fatalf("expected baseline rebased to 130.00 after deposit, got %.4f", stats.StartingBalance)
	}
	if absFloat(stats.RealizedPnL) > 0.0001 {
		t.Fatalf("expected explicit rebase to stay out of realized pnl, got %.4f", stats.RealizedPnL)
	}

	neutralized = engine.RebaseBalance(120.0)
	if absFloat(neutralized+10.0) > 0.0001 {
		t.Fatalf("expected withdrawal rebased by -10.00, got %.4f", neutralized)
	}

	stats = engine.GetStats()
	if absFloat(stats.StartingBalance-120.0) > 0.0001 {
		t.Fatalf("expected starting balance rebased to 120.00 after withdrawal, got %.4f", stats.StartingBalance)
	}
	if absFloat(engine.GetBookEquity()-stats.StartingBalance) > 0.0001 {
		t.Fatalf("expected flat cash transfer to keep book equity %.4f neutral to baseline %.4f", engine.GetBookEquity(), stats.StartingBalance)
	}
}

func TestEngine_GetStatsRealizedPnLStaysNeutralForOpenInventory(t *testing.T) {
	engine := NewEngine(100.0)

	if _, err := engine.BuyForMarket("m1", "Up", 0.60, 10.0); err != nil {
		t.Fatalf("buy failed: %v", err)
	}

	stats := engine.GetStats()
	if absFloat(stats.RealizedPnL) > 0.0001 {
		t.Fatalf("expected realized pnl to stay neutral with open inventory, got %.4f", stats.RealizedPnL)
	}
}

func TestEngine_SyncBalanceNeutralKeepsOpenInventorySessionNeutral(t *testing.T) {
	engine := NewEngine(65.08)

	if _, err := engine.BuyForMarket("m1", "Down", 3.25/3.5816, 3.5816); err != nil {
		t.Fatalf("buy failed: %v", err)
	}

	neutralized := engine.SyncBalanceNeutral(64.77)
	if absFloat(neutralized-2.94) > 0.0001 {
		t.Fatalf("expected neutralized balance delta 2.94, got %.4f", neutralized)
	}

	stats := engine.GetStats()
	if absFloat(engine.GetBookEquity()-stats.StartingBalance) > 0.0001 {
		t.Fatalf("expected balance sync to keep open inventory neutral, equity %.4f baseline %.4f", engine.GetBookEquity(), stats.StartingBalance)
	}
	if absFloat(engine.GetCompoundMultiplier()-1.0) > 0.0001 {
		t.Fatalf("expected neutral sync not to ratchet compounding, got %.4f", engine.GetCompoundMultiplier())
	}
}

func TestEngine_SyncBalanceNeutralDoesNotLowerHighWaterOnNegativeDelta(t *testing.T) {
	engine := NewEngine(100.0)

	engine.UpdateCompoundMultiplier(20.0, 100.0)
	if got := engine.GetSizingBalance(); absFloat(got-120.0) > 0.0001 {
		t.Fatalf("expected initial high-water sizing 120.00, got %.4f", got)
	}

	if _, err := engine.BuyForMarket("m1", "Up", 0.50, 10.0); err != nil {
		t.Fatalf("buy failed: %v", err)
	}

	postBuyBalance := engine.GetBalance()
	neutralized := engine.SyncBalanceNeutral(postBuyBalance - 5.0)
	if absFloat(neutralized+5.0) > 0.0001 {
		t.Fatalf("expected neutralized negative delta -5.00, got %.4f", neutralized)
	}
	if got := engine.GetSizingBalance(); absFloat(got-120.0) > 0.0001 {
		t.Fatalf("expected negative neutral sync to preserve high-water 120.00, got %.4f", got)
	}
}

func TestEngine_RestoreSizingFloorRaisesHighWaterWithoutChangingCash(t *testing.T) {
	engine := NewEngine(75.0)
	if got := engine.GetSizingBalance(); absFloat(got-75.0) > 0.0001 {
		t.Fatalf("expected initial sizing 75.00, got %.4f", got)
	}

	if !engine.RestoreSizingFloor(82.5) {
		t.Fatal("expected restore sizing floor to report changed")
	}
	if got := engine.GetSizingBalance(); absFloat(got-82.5) > 0.0001 {
		t.Fatalf("expected sizing floor 82.50 after restore, got %.4f", got)
	}
	if got := engine.GetBalance(); absFloat(got-75.0) > 0.0001 {
		t.Fatalf("expected restore sizing floor not to change cash, got %.4f", got)
	}
}

func TestEngine_GetResolutionPnLRangeSingleSidedPosition(t *testing.T) {
	engine := NewEngine(100.0)

	if _, err := engine.BuyForMarket("m1", "Up", 3.1/3.5, 3.5); err != nil {
		t.Fatalf("buy failed: %v", err)
	}

	best, worst := engine.GetResolutionPnLRange()
	if absFloat(best-0.4) > 0.0001 {
		t.Fatalf("expected best resolution pnl +0.40, got %.4f", best)
	}
	if absFloat(worst+3.1) > 0.0001 {
		t.Fatalf("expected worst resolution pnl -3.10, got %.4f", worst)
	}

	stats := engine.GetStats()
	if absFloat(stats.RealizedPnL) > 0.0001 {
		t.Fatalf("expected unresolved single-sided position to remain unrealized, got %.4f", stats.RealizedPnL)
	}
}

func TestEngine_GetSizingBalanceDiscountsWorstCaseUnresolvedRisk(t *testing.T) {
	engine := NewEngine(100.0)

	if _, err := engine.BuyForMarket("m1", "Up", 0.50, 20.0); err != nil {
		t.Fatalf("buy failed: %v", err)
	}

	got := engine.GetSizingBalance()
	// No longer discounted
	if absFloat(got-100.0) > 0.0001 {
		t.Fatalf("expected sizing balance 100.00 after removing worst-case unresolved discount, got %.4f", got)
	}
}

func TestEngine_GetSizingBalanceIncludesLockedPairUpside(t *testing.T) {
	engine := NewEngine(100.0)

	if _, err := engine.BuyForMarket("m1", "Up", 0.40, 10.0); err != nil {
		t.Fatalf("up buy failed: %v", err)
	}
	if _, err := engine.BuyForMarket("m1", "Down", 0.55, 10.0); err != nil {
		t.Fatalf("down buy failed: %v", err)
	}

	best, worst := engine.GetResolutionPnLRange()
	if absFloat(best-0.5) > 0.0001 || absFloat(worst-0.5) > 0.0001 {
		t.Fatalf("expected locked pair resolution pnl +0.50/+0.50, got best=%.4f worst=%.4f", best, worst)
	}

	got := engine.GetSizingBalance()
	if absFloat(got-100.0) > 0.0001 {
		t.Fatalf("expected sizing balance 100.00 for locked pair (no longer adds worst-case), got %.4f", got)
	}
}

func TestEngine_GetStatsRealizedPnLIncludesPendingRedemption(t *testing.T) {
	engine := NewEngine(100.0)

	if _, err := engine.BuyForMarket("m1", "Up", 0.60, 10.0); err != nil {
		t.Fatalf("winning buy failed: %v", err)
	}
	if _, err := engine.BuyForMarket("m1", "Down", 0.40, 5.0); err != nil {
		t.Fatalf("losing buy failed: %v", err)
	}

	res := engine.RedeemWithDetails("m1", "Up")
	if absFloat(res.TotalPnL-2.0) > 0.0001 {
		t.Fatalf("expected total pnl 2.00, got %.4f", res.TotalPnL)
	}

	stats := engine.GetStats()
	if absFloat(stats.RealizedPnL-2.0) > 0.0001 {
		t.Fatalf("expected realized pnl 2.00 with pending redemption, got %.4f", stats.RealizedPnL)
	}

	engine.SetBalance(102.0)
	engine.ClearPendingRedemption("m1")

	stats = engine.GetStats()
	if absFloat(stats.RealizedPnL-2.0) > 0.0001 {
		t.Fatalf("expected realized pnl 2.00 after cash settles, got %.4f", stats.RealizedPnL)
	}
}

func TestEngine_SettlePendingRedemptionSettlesExactPayout(t *testing.T) {
	engine := NewEngine(100.0)

	if _, err := engine.BuyForMarket("m1", "Up", 0.60, 10.0); err != nil {
		t.Fatalf("winning buy failed: %v", err)
	}
	if _, err := engine.BuyForMarket("m1", "Down", 0.40, 5.0); err != nil {
		t.Fatalf("losing buy failed: %v", err)
	}

	res := engine.RedeemWithDetails("m1", "Up")
	if absFloat(res.WinningPayout-10.0) > 0.0001 {
		t.Fatalf("expected winning payout 10.00, got %.4f", res.WinningPayout)
	}
	if absFloat(engine.GetBalance()-92.0) > 0.0001 {
		t.Fatalf("expected unresolved cash balance 92.00, got %.4f", engine.GetBalance())
	}
	if absFloat(engine.GetBookEquity()-102.0) > 0.0001 {
		t.Fatalf("expected pending redemption book equity 102.00, got %.4f", engine.GetBookEquity())
	}

	settled := engine.SettlePendingRedemption("m1")
	if absFloat(settled-10.0) > 0.0001 {
		t.Fatalf("expected settled payout 10.00, got %.4f", settled)
	}
	if absFloat(engine.GetBalance()-102.0) > 0.0001 {
		t.Fatalf("expected settled cash balance 102.00, got %.4f", engine.GetBalance())
	}
	if absFloat(engine.GetBookEquity()-102.0) > 0.0001 {
		t.Fatalf("expected settled book equity 102.00, got %.4f", engine.GetBookEquity())
	}
}

func TestEngine_RedeemWithDetailsSplitPayoutRemainsPendingUntilSettled(t *testing.T) {
	engine := NewEngine(100.0)
	inv := NewSplitInventory()
	engine.RegisterSplitInventory(inv)

	inv.RecordSplit("m1", "Up", "Down", 10.0)
	engine.DeductBalance(10.0)

	res := engine.RedeemWithDetails("m1", "Up")
	if absFloat(res.WinningPayout-10.0) > 0.0001 {
		t.Fatalf("expected split winning payout 10.00, got %.4f", res.WinningPayout)
	}
	if absFloat(res.TotalPnL-0.0) > 0.0001 {
		t.Fatalf("expected split redemption pnl 0.00, got %.4f", res.TotalPnL)
	}
	if absFloat(engine.GetBalance()-90.0) > 0.0001 {
		t.Fatalf("expected split payout to remain pending before settlement, got cash %.4f", engine.GetBalance())
	}
	if absFloat(engine.GetBookEquity()-100.0) > 0.0001 {
		t.Fatalf("expected pending split payout to keep equity at 100.00, got %.4f", engine.GetBookEquity())
	}

	settled := engine.SettlePendingRedemption("m1")
	if absFloat(settled-10.0) > 0.0001 {
		t.Fatalf("expected settled split payout 10.00, got %.4f", settled)
	}
	if absFloat(engine.GetBalance()-100.0) > 0.0001 {
		t.Fatalf("expected settled split cash 100.00, got %.4f", engine.GetBalance())
	}
	if absFloat(engine.GetBookEquity()-100.0) > 0.0001 {
		t.Fatalf("expected settled split equity 100.00, got %.4f", engine.GetBookEquity())
	}
}

func TestEngine_ImportedCarryOnlyRealizesAtResolution(t *testing.T) {
	t.Run("profit", func(t *testing.T) {
		engine := NewEngine(95.0)

		if !engine.SyncExternalPosition("m1", "Up", 10.0, 0.50) {
			t.Fatal("expected external carry import")
		}
		engine.UpdateMarketBidAsk("m1", "Up", 0.20, 0.21)

		stats := engine.GetStats()
		if absFloat(stats.RealizedPnL) > 0.0001 {
			t.Fatalf("expected unresolved imported carry to stay unrealized, got %.4f", stats.RealizedPnL)
		}
		if absFloat(engine.GetBookEquity()-100.0) > 0.0001 {
			t.Fatalf("expected unresolved book equity 100.00, got %.4f", engine.GetBookEquity())
		}

		res := engine.RedeemWithDetails("m1", "Up")
		if absFloat(res.TotalPnL-5.0) > 0.0001 {
			t.Fatalf("expected winning resolution pnl 5.00, got %.4f", res.TotalPnL)
		}

		stats = engine.GetStats()
		if absFloat(stats.RealizedPnL-5.0) > 0.0001 {
			t.Fatalf("expected realized pnl 5.00 after resolution, got %.4f", stats.RealizedPnL)
		}
		if absFloat(engine.GetBookEquity()-105.0) > 0.0001 {
			t.Fatalf("expected pending redemption to lift book equity to 105.00, got %.4f", engine.GetBookEquity())
		}

		engine.SetBalance(105.0)
		engine.ClearPendingRedemption("m1")

		stats = engine.GetStats()
		if absFloat(stats.RealizedPnL-5.0) > 0.0001 {
			t.Fatalf("expected realized pnl to remain 5.00 after settlement, got %.4f", stats.RealizedPnL)
		}
	})

	t.Run("loss", func(t *testing.T) {
		engine := NewEngine(95.0)

		if !engine.SyncExternalPosition("m2", "Up", 10.0, 0.50) {
			t.Fatal("expected external carry import")
		}
		engine.UpdateMarketBidAsk("m2", "Up", 0.20, 0.21)

		stats := engine.GetStats()
		if absFloat(stats.RealizedPnL) > 0.0001 {
			t.Fatalf("expected unresolved imported carry to stay unrealized, got %.4f", stats.RealizedPnL)
		}

		res := engine.RedeemWithDetails("m2", "Down")
		if absFloat(res.TotalPnL+5.0) > 0.0001 {
			t.Fatalf("expected losing resolution pnl -5.00, got %.4f", res.TotalPnL)
		}

		stats = engine.GetStats()
		if absFloat(stats.RealizedPnL+5.0) > 0.0001 {
			t.Fatalf("expected realized pnl -5.00 after losing resolution, got %.4f", stats.RealizedPnL)
		}
		if absFloat(engine.GetBookEquity()-95.0) > 0.0001 {
			t.Fatalf("expected book equity 95.00 after losing resolution, got %.4f", engine.GetBookEquity())
		}
	})
}

func TestEngine_UpdateCompoundMultiplierKeepsSizingHighWaterAfterLosses(t *testing.T) {
	engine := NewEngine(100.0)

	engine.UpdateCompoundMultiplier(20.0, 100.0)

	if got := engine.GetSizingBalance(); absFloat(got-120.0) > 0.0001 {
		t.Fatalf("expected sizing balance 120.00 after winning round, got %.4f", got)
	}
	if got := engine.GetCompoundMultiplier(); absFloat(got-1.2) > 0.0001 {
		t.Fatalf("expected multiplier 1.20 after winning round, got %.4f", got)
	}

	engine.UpdateCompoundMultiplier(-20.0, 120.0)

	if got := engine.GetSizingBalance(); absFloat(got-120.0) > 0.0001 {
		t.Fatalf("expected sizing balance to stay at 120.00 after drawdown, got %.4f", got)
	}
	if got := engine.GetCompoundMultiplier(); absFloat(got-1.2) > 0.0001 {
		t.Fatalf("expected multiplier to stay at 1.20 after drawdown, got %.4f", got)
	}

	engine.UpdateCompoundMultiplier(10.0, 100.0)

	if got := engine.GetSizingBalance(); absFloat(got-120.0) > 0.0001 {
		t.Fatalf("expected smaller recovery not to exceed prior high-water, got %.4f", got)
	}

	engine.UpdateCompoundMultiplier(15.0, 120.0)

	if got := engine.GetSizingBalance(); absFloat(got-135.0) > 0.0001 {
		t.Fatalf("expected new high-water sizing balance 135.00, got %.4f", got)
	}
	if got := engine.GetCompoundMultiplier(); absFloat(got-1.35) > 0.0001 {
		t.Fatalf("expected multiplier 1.35 at new high-water, got %.4f", got)
	}
}

func TestEngine_GetCompoundStatsUsesEffectiveSizingBalance(t *testing.T) {
	engine := NewEngine(100.0)

	engine.SetBalance(120.0)
	_, _, _, _, sizing := engine.GetCompoundStats()
	if absFloat(sizing-120.0) > 0.0001 {
		t.Fatalf("expected compound stats sizing balance to grow with current balance, got %.4f", sizing)
	}

	engine.SetBalance(90.0)
	_, _, _, _, sizing = engine.GetCompoundStats()
	if absFloat(sizing-100.0) > 0.0001 {
		t.Fatalf("expected compound stats sizing balance to keep high-water floor, got %.4f", sizing)
	}
}

func TestRemovePositionForSettlement_DoesNotAdjustBaseline(t *testing.T) {
	engine := NewEngine(100)
	// Buy a position so the engine has inventory.
	engine.SyncExternalPosition("mkt1", "Down", 20, 0.60)
	baselineBefore := engine.GetStartingBalance()

	removed := engine.RemovePositionForSettlement("mkt1", "Down")
	if !removed {
		t.Fatal("expected RemovePositionForSettlement to return true")
	}
	baselineAfter := engine.GetStartingBalance()
	if absFloat(baselineAfter-baselineBefore) > 0.0001 {
		t.Fatalf("pnlBaseline should not change after RemovePositionForSettlement: before=%.4f after=%.4f", baselineBefore, baselineAfter)
	}
	// Position should be gone.
	positions := engine.GetPositions()
	for _, pos := range positions {
		if pos.MarketID == "mkt1" && pos.Outcome == "Down" {
			t.Fatal("expected position to be removed")
		}
	}
}

func TestRemovePositionForSettlement_ReturnsFalseWhenMissing(t *testing.T) {
	engine := NewEngine(100)
	if engine.RemovePositionForSettlement("mkt1", "Down") {
		t.Fatal("expected false for missing position")
	}
}

func TestSyncExternalPosition_DoesAdjustBaseline(t *testing.T) {
	// Confirm SyncExternalPosition reduces pnlBaseline (existing behaviour)
	// so we know RemovePositionForSettlement is the correct alternative.
	engine := NewEngine(100)
	engine.SyncExternalPosition("mkt1", "Down", 20, 0.60)
	baselineBefore := engine.GetStartingBalance() // 100 + 12 = 112

	engine.SyncExternalPosition("mkt1", "Down", 0, 0)
	baselineAfter := engine.GetStartingBalance() // 112 - 12 = 100
	delta := baselineBefore - baselineAfter
	if delta < 11.99 {
		t.Fatalf("expected SyncExternalPosition(0,0) to reduce baseline by ~12, delta=%.4f", delta)
	}
}
