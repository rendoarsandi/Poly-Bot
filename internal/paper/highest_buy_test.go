package paper

import (
	"math"
	"testing"
)

func TestHighestBuyPriceTracking(t *testing.T) {
	// 1. Create engine
	engine := NewEngine(100.0)

	// 2. Buy at $0.40
	trade1, err := engine.BuyFilledForMarket("M1", "Yes", 4.00, 10.0)
	if err != nil {
		t.Fatalf("failed to buy: %v", err)
	}
	if trade1.Price != 0.40 {
		t.Fatalf("expected price 0.40, got %.2f", trade1.Price)
	}

	pos := engine.GetPositions()["M1:Yes"]
	if pos.HighestBuyPrice != 0.40 {
		t.Fatalf("expected HighestBuyPrice 0.40, got %.2f", pos.HighestBuyPrice)
	}

	// 3. Buy at $0.60 (new highest buy)
	_, err = engine.BuyFilledForMarket("M1", "Yes", 6.00, 10.0)
	if err != nil {
		t.Fatalf("failed to buy: %v", err)
	}

	pos = engine.GetPositions()["M1:Yes"]
	if pos.HighestBuyPrice != 0.60 {
		t.Fatalf("expected HighestBuyPrice 0.60, got %.2f", pos.HighestBuyPrice)
	}

	// 4. Buy at $0.50 (lower than highest, should keep 0.60)
	_, err = engine.BuyFilledForMarket("M1", "Yes", 5.00, 10.0)
	if err != nil {
		t.Fatalf("failed to buy: %v", err)
	}

	pos = engine.GetPositions()["M1:Yes"]
	if pos.HighestBuyPrice != 0.60 {
		t.Fatalf("expected HighestBuyPrice 0.60, got %.2f", pos.HighestBuyPrice)
	}
	if math.Abs(pos.AvgPrice-0.50) > 0.000001 {
		t.Fatalf("expected AvgPrice 0.50, got %.2f", pos.AvgPrice)
	}

	// 5. Test SyncExternalPosition
	// Sync size increase at a higher price
	changed := engine.SyncExternalPosition("M1", "Yes", 40.0, 0.70)
	if !changed {
		t.Fatalf("expected sync to return changed=true")
	}
	pos = engine.GetPositions()["M1:Yes"]
	if pos.HighestBuyPrice != 0.70 {
		t.Fatalf("expected HighestBuyPrice to update to 0.70 after sync at higher markPrice, got %.2f", pos.HighestBuyPrice)
	}

	// 6. Test SyncExternalPositionWithTotalCost
	// Sync size increase where marginal price is even higher
	// Currently at 40 shares with total cost = 15.0 (for first 30 shares) + 7.0 (added 10 shares @ 0.70) = 22.0. Let's sync to 50 shares with total cost 31.0 (marginal price = 9.0 / 10 = 0.90)
	changed = engine.SyncExternalPositionWithTotalCost("M1", "Yes", 50.0, 31.0)
	if !changed {
		t.Fatalf("expected sync to return changed=true")
	}
	pos = engine.GetPositions()["M1:Yes"]
	if math.Abs(pos.HighestBuyPrice-0.90) > 0.000001 {
		t.Fatalf("expected HighestBuyPrice to update to 0.90 after marginal increase sync, got %.4f", pos.HighestBuyPrice)
	}
}
