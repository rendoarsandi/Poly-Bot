package core

import (
	"math"
	"testing"
)

func TestBuyExecutionLimitPricesUsesExecutionFloorHeadroom(t *testing.T) {
	cap0, cap1, err := BuyExecutionLimitPrices(0.36, 0.65, 0.10, 0.90, -3.0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if math.Abs(cap0-0.38) > 0.000001 || math.Abs(cap1-0.67) > 0.000001 {
		t.Fatalf("got caps %.3f / %.3f want 0.380 / 0.670", cap0, cap1)
	}
}

func TestBuyExecutionLimitPricesRejectsOutOfRangePair(t *testing.T) {
	_, _, err := BuyExecutionLimitPrices(0.36, 0.66, 0.10, 0.90, -1.0)
	if err == nil {
		t.Fatal("expected pair above execution max to be rejected")
	}
}

func TestCleanupSellLimitPriceUsesConfiguredFloor(t *testing.T) {
	if got := CleanupSellLimitPrice(0.10); got != 0.10 {
		t.Fatalf("expected configured cleanup floor, got %.2f", got)
	}
	if got := CleanupSellLimitPrice(0); got != 0.01 {
		t.Fatalf("expected fallback cleanup floor, got %.2f", got)
	}
}
