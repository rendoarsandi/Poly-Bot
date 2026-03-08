package main

import "testing"

func TestManualbotEmergencySellPriceKeepsConfiguredFloorWhenBidHigher(t *testing.T) {
	if got := manualbotEmergencySellPrice(0.05); got != 0.03 {
		t.Fatalf("expected configured floor 0.03, got %.3f", got)
	}
}

func TestManualbotEmergencySellPriceRepricesToLiveBidWhenLower(t *testing.T) {
	if got := manualbotEmergencySellPrice(0.02); got != 0.02 {
		t.Fatalf("expected live bid 0.02, got %.3f", got)
	}
}

func TestManualbotEmergencySellPriceHonorsMinimumTick(t *testing.T) {
	if got := manualbotEmergencySellPrice(0.001); got != 0.01 {
		t.Fatalf("expected minimum tick 0.01, got %.3f", got)
	}
}
