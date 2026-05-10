package strategy

import (
	"testing"

	"Market-bot/internal/core"
)

func TestCalculateDiscountSum(t *testing.T) {
	tests := []struct {
		name     string
		priceYes string
		priceNo  string
		expected float64
	}{
		{"Standard Case", "0.48", "0.48", 0.96},
		{"Zero Case", "0.00", "0.00", 0.00},
		{"Full Dollar", "0.50", "0.50", 1.00},
		{"Profit Case", "0.45", "0.45", 0.90},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CalculateDiscountSum(tt.priceYes, tt.priceNo)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if got != tt.expected {
				t.Errorf("CalculateDiscountSum(%s, %s) = %f; expected %f", tt.priceYes, tt.priceNo, got, tt.expected)
			}
		})
	}
}

func TestCalculateDiscountSumError(t *testing.T) {
	_, err := CalculateDiscountSum("invalid", "0.48")
	if err == nil {
		t.Error("Expected error for invalid Yes input, got nil")
	}

	_, err = CalculateDiscountSum("0.48", "invalid")
	if err == nil {
		t.Error("Expected error for invalid No input, got nil")
	}
}

func TestCalculateTradeMetricsFeeCurve(t *testing.T) {
	got := CalculateTradeMetricsFeeCurve(100, 0.50, 0.50, core.PolymarketFeeCurve{Rate: 0.05, Exponent: 1})
	if got.Overhead != 2.5 {
		t.Fatalf("expected two 100-share 50c legs at 5%% theta to cost $2.50 in fees, got %.5f", got.Overhead)
	}
	if got.Net != -2.5 {
		t.Fatalf("expected net -2.50, got %.5f", got.Net)
	}
}
