package main

import (
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
