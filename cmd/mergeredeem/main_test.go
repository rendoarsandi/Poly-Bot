package main

import (
	"errors"
	"math"
	"testing"
)

func TestMergeablePairsAllowsDecimalPairs(t *testing.T) {
	got := mergeablePairs([]float64{0.966094, 1.250000})
	if math.Abs(got-0.966094) > 1e-9 {
		t.Fatalf("expected decimal balanced pairs to be mergeable, got %.6f", got)
	}

	if got := mergeablePairs([]float64{0.0000001, 1}); got != 0 {
		t.Fatalf("expected dust balance to be ignored, got %.9f", got)
	}
}

func TestIsSkippableRedeemError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "resolved pending", err: errors.New("market not yet resolved on-chain"), want: true},
		{name: "evm revert", err: errors.New("execution reverted"), want: true},
		{name: "payouts missing", err: errors.New("payouts not reported yet"), want: true},
		{name: "other error", err: errors.New("network timeout"), want: false},
	}

	for _, tt := range tests {
		if got := isSkippableRedeemError(tt.err); got != tt.want {
			t.Fatalf("%s: got %v want %v", tt.name, got, tt.want)
		}
	}
}
