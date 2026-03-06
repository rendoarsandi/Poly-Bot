package main

import (
	"math"
	"testing"
)

func TestNormalizePanicBuySharesPerSide(t *testing.T) {
	got, bumped := normalizePanicBuySharesPerSide(1)
	want := 1.05 / 0.99
	if !bumped {
		t.Fatal("expected small buy to be bumped to the minimum notional size")
	}
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("expected %.12f, got %.12f", want, got)
	}

	got, bumped = normalizePanicBuySharesPerSide(2)
	if bumped {
		t.Fatal("did not expect larger buy size to be adjusted")
	}
	if got != 2 {
		t.Fatalf("expected 2, got %.12f", got)
	}
}

func TestIncrementalBalancedPairs(t *testing.T) {
	got := incrementalBalancedPairs(0.966094, 0.966094, 2.0267, 2.0267)
	want := 1.060606
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("expected %.6f newly acquired balanced pairs, got %.6f", want, got)
	}
}
