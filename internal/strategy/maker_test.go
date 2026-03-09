package strategy

import (
	"math"
	"testing"
)

var testMakerParams = MakerParams{
	QuoteStep:           0.001,
	DefaultQuoteGap:     0.008,
	InventorySkewStep:   0.020,
	QuoteSizeSkewFactor: 0.75,
	CashUsagePerOutcome: 0.35,
	MinQuoteShares:      1.0,
}

func TestComputeMakerInventorySkewClampsToRange(t *testing.T) {
	if got := ComputeMakerInventorySkew(40, 0, 10); got != 1.0 {
		t.Fatalf("long-heavy skew = %.2f, want 1.00", got)
	}
	if got := ComputeMakerInventorySkew(0, 40, 10); got != -1.0 {
		t.Fatalf("short-heavy skew = %.2f, want -1.00", got)
	}
	if got := ComputeMakerInventorySkew(12, 8, 20); got != 0.2 {
		t.Fatalf("balanced skew = %.2f, want 0.20", got)
	}
}

func TestComputeMakerSkewedQuoteRespectsConfiguredGap(t *testing.T) {
	tight, ok := ComputeMakerSkewedQuote(true, 0.47, 0.53, 0.0, 0.003, testMakerParams)
	if !ok {
		t.Fatal("expected tight maker buy quote")
	}
	wide, ok := ComputeMakerSkewedQuote(true, 0.47, 0.53, 0.0, 0.012, testMakerParams)
	if !ok {
		t.Fatal("expected wide maker buy quote")
	}
	if tight <= wide {
		t.Fatalf("expected tighter gap to quote closer to ask: tight=%.3f wide=%.3f", tight, wide)
	}
}

func TestComputeMakerQuoteSizesRespectNormalizationAndCaps(t *testing.T) {
	normalize := func(q float64) float64 { return math.Floor(q) }
	buyHeavy := ComputeMakerBuyQty(10, 18, 1.0, 20, 100, 0.49, testMakerParams, normalize)
	buyLight := ComputeMakerBuyQty(10, 2, -1.0, 20, 100, 0.49, testMakerParams, normalize)
	if buyHeavy >= buyLight {
		t.Fatalf("expected heavy inventory to quote smaller buys: heavy=%.0f light=%.0f", buyHeavy, buyLight)
	}
	sellHeavy := ComputeMakerSellQty(10, 30, 1.0, testMakerParams, normalize)
	sellBalanced := ComputeMakerSellQty(10, 30, 0.0, testMakerParams, normalize)
	if sellHeavy <= sellBalanced {
		t.Fatalf("expected heavy inventory to quote larger sells: heavy=%.0f balanced=%.0f", sellHeavy, sellBalanced)
	}
}

func TestComputeMakerProtectedSellQuoteHonorsCostFloor(t *testing.T) {
	price, ok := ComputeMakerProtectedSellQuote(0.47, 0.60, 0.52, 0.02, 0.0, 0.008, 0, testMakerParams)
	if !ok {
		t.Fatal("expected protected sell quote to be available")
	}
	if price < 0.54 {
		t.Fatalf("sell quote = %.3f, want at least 0.540", price)
	}
	if _, ok := ComputeMakerProtectedSellQuote(0.47, 0.54, 0.53, 0.02, 0.0, 0.008, 0, testMakerParams); ok {
		t.Fatal("expected protected sell quote to fail when spread cannot clear cost floor")
	}
}

func TestShouldMakerBlockBuy(t *testing.T) {
	if !ShouldMakerBlockBuy(12, false, 8, 0.44, 0.43, 0.02) {
		t.Fatal("expected heavy leg without protected sell to block maker buy")
	}
	if !ShouldMakerBlockBuy(3, true, 10, 0.62, 0.39, 0.02) {
		t.Fatal("expected expensive completion path to block maker buy")
	}
	if ShouldMakerBlockBuy(3, true, 10, 0.50, 0.39, 0.02) {
		t.Fatal("expected affordable completion path to pass")
	}
}
