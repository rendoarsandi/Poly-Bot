package core

import "testing"

func TestPolymarketTakerFeeUSDC(t *testing.T) {
	tests := []struct {
		name       string
		shares     float64
		price      float64
		feeRateBps int
		want       float64
	}{
		{
			name:       "Standard 3bps Sports Fee at 0.50",
			shares:     100,
			price:      0.5,
			feeRateBps: 3,
			want:       0.00750, // 100 * 0.0003 * 0.5 * 0.5 = 0.0075
		},
		{
			name:       "Standard 7.2bps Crypto Fee at 0.50",
			shares:     100,
			price:      0.5,
			feeRateBps: 7, // Rounded to 7 for bps int
			want:       0.01750, // 100 * 0.0007 * 0.5 * 0.5 = 0.0175
		},
		{
			name:       "Rounding to 5 decimal places",
			shares:     100,
			price:      0.3333,
			feeRateBps: 7,
			want:       0.01555, // 100 * 0.0007 * 0.3333 * 0.6667 = 0.015554 -> 0.01555
		},
		{
			name:       "Fees < 0.00001 round down to zero",
			shares:     0.01,
			price:      0.5,
			feeRateBps: 1,
			want:       0, // 0.01 * 0.0001 * 0.25 = 0.00000025 -> 0 (min threshold)
		},
		{
			name:       "Zero fee for geopolitical",
			shares:     100,
			price:      0.5,
			feeRateBps: 0,
			want:       0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PolymarketTakerFeeUSDC(tt.shares, tt.price, tt.feeRateBps)
			if got != tt.want {
				t.Errorf("PolymarketTakerFeeUSDC() = %v, want %v", got, tt.want)
			}
		})
	}
}
