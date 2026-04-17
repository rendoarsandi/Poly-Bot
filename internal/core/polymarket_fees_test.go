package core

import "testing"

func TestPolymarketTakerFeeUSDC(t *testing.T) {
	got := PolymarketTakerFeeUSDC(100, 0.5, 100)
	want := 0.25
	if got != want {
		t.Fatalf("fee = %.8f, want %.8f", got, want)
	}
}

func TestPolymarketBuyFeeShares(t *testing.T) {
	got := PolymarketBuyFeeShares(100, 0.5, 100)
	want := 0.5
	if got != want {
		t.Fatalf("fee shares = %.8f, want %.8f", got, want)
	}
}
