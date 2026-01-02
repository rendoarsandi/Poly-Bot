package strategy

import (
	"testing"
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
