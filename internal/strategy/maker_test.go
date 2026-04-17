package strategy

import (
	"math"
	"testing"
	"time"
)

var testMakerParams = MakerParams{
	QuoteStep:           0.001,
	DefaultQuoteGap:     0.008,
	InventorySkewStep:   0.020,
	QuoteSizeSkewFactor: 0.75,
	CashUsagePerOutcome: 0.35,
	MinQuoteValue:       1.0,
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

func TestComputeMakerPairBuyPricesRespectsPairCostAndBias(t *testing.T) {
	price1, price2, ok := ComputeMakerPairBuyPrices(0.92, 0.95, 0.03, 0.06, 0.98, 10, testMakerParams)
	if !ok {
		t.Fatal("expected complementary maker buy prices")
	}
	if price1+price2 > 0.98+1e-9 {
		t.Fatalf("pair cost %.3f exceeds cap", price1+price2)
	}
	if price1 >= 0.95 {
		t.Fatalf("expected heavy side quote to be reduced below max aggression, got %.3f", price1)
	}
	if math.Abs(price2-0.059) > 0.000001 {
		t.Fatalf("expected lighter side to stay near the ask guard, got %.3f", price2)
	}
}

func TestComputeMakerPairQuoteQtyRespectsCashAndMinValue(t *testing.T) {
	normalize := func(q float64) float64 { return math.Floor(q) }
	qty := ComputeMakerPairQuoteQty(20, 0, 200, 100, 0.92, 0.05, testMakerParams, normalize)
	if qty <= 0 {
		t.Fatal("expected pair quote qty")
	}
	if qty != 20 {
		t.Fatalf("expected 20 shares from pair notional sizing, got %.0f", qty)
	}
	tooSmall := ComputeMakerPairQuoteQty(0.2, 0, 200, 100, 0.92, 0.05, testMakerParams, normalize)
	if tooSmall != 0 {
		t.Fatalf("expected tiny pair quote to be blocked by min quote value, got %.2f", tooSmall)
	}
}

func TestComputeMakerQuoteSizesRespectNormalizationAndCaps(t *testing.T) {
	normalize := func(q float64) float64 { return math.Floor(q) }
	buyHeavy := ComputeMakerBuyQty(10, 18, 1.0, 20, 100, 0.49, testMakerParams, normalize)
	buyLight := ComputeMakerBuyQty(10, 2, -1.0, 20, 100, 0.49, testMakerParams, normalize)
	if buyHeavy >= buyLight {
		t.Fatalf("expected heavy inventory to quote smaller buys: heavy=%.0f light=%.0f", buyHeavy, buyLight)
	}
	sellHeavy := ComputeMakerSellQty(10, 30, 1.0, 0.50, testMakerParams, normalize)
	sellBalanced := ComputeMakerSellQty(10, 30, 0.0, 0.50, testMakerParams, normalize)
	if sellHeavy <= sellBalanced {
		t.Fatalf("expected heavy inventory to quote larger sells: heavy=%.0f balanced=%.0f", sellHeavy, sellBalanced)
	}
}

func TestComputeMakerSellFeeUsdc(t *testing.T) {
	if got := ComputeMakerSellFeeUsdc(100, 0.5, 100); got != 0.25 {
		t.Fatalf("expected fee 0.25, got %.8f", got)
	}
	if got := ComputeMakerSellFeeUsdc(100, 0.5, 0); got != 0 {
		t.Fatalf("expected zero fee when fee rate disabled, got %.8f", got)
	}
	if got := ComputeMakerSellFeeUsdc(0, 0.5, 100); got != 0 {
		t.Fatalf("expected zero fee for zero shares, got %.8f", got)
	}
}

func TestComputeMakerProtectedSellQuoteIgnoresCostFloor(t *testing.T) {
	price, ok := ComputeMakerProtectedSellQuote(0.47, 0.60, 0.52, 0.02, 0.0, 0.008, 0, time.Hour, testMakerParams)
	if !ok {
		t.Fatal("expected protected sell quote to be available")
	}
	if price < 0.54 {
		t.Fatalf("sell quote = %.3f, want at least 0.540", price)
	}
	// The implementation now ignores the cost-basis check to prevent accumulating toxic bags.
	if _, ok := ComputeMakerProtectedSellQuote(0.47, 0.54, 0.53, 0.02, 0.0, 0.008, 0, time.Hour, testMakerParams); !ok {
		t.Fatal("expected protected sell quote to succeed even when spread cannot clear cost floor")
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
