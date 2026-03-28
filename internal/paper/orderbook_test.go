package paper

import "testing"

func TestProcessComplementaryBuyUpdateFillsAgainstOppositeBuyInterest(t *testing.T) {
	ob := NewOrderBookWithRealism(0, 0)
	order := ob.PlaceOrder("Up", "buy", 0.92, 10, 0)

	var fills int
	var filledQty float64
	var fillPrice float64
	ob.SetFillCallback(func(order *LimitOrder, qty float64, price float64) {
		fills++
		filledQty += qty
		fillPrice = price
	})

	filled := ob.ProcessComplementaryBuyUpdate("Up", []MarketLevel{
		{Price: 0.08, Size: 6},
		{Price: 0.07, Size: 10},
	})
	if len(filled) != 1 {
		t.Fatalf("expected one order to fill, got %d", len(filled))
	}
	if order.Status != OrderStatusPartial {
		t.Fatalf("expected partial fill from complementary bid path, got %s", order.Status)
	}
	if order.FilledQty != 6 {
		t.Fatalf("expected 6 shares filled, got %.2f", order.FilledQty)
	}
	if fills != 1 || filledQty != 6 {
		t.Fatalf("expected one callback for 6 shares, got fills=%d qty=%.2f", fills, filledQty)
	}
	if fillPrice != 0.92 {
		t.Fatalf("expected maker order to fill at its resting bid, got %.3f", fillPrice)
	}
}
