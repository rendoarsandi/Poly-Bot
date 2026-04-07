package copytradeutil

import (
	"math"
	"testing"
)

func TestBuyRequestedQtyUsesSharedModeConfig(t *testing.T) {
	got := BuyRequestedQty(12, 0.4, SizingConfig{
		SizeUSDC:    3,
		SizeShares:  8,
		SizePercent: 10,
		SizingMode:  "usdc",
	})
	if got != 7.5 {
		t.Fatalf("buy requested qty = %.4f, want 7.5", got)
	}
}

func TestBuildSellSizingPlanUsesTargetDeltaAndConsumesSharedDelta(t *testing.T) {
	state := NewRuntimeState()
	state.TargetSeen["Up"] = true
	state.TargetShares["Up"] = 50
	targetDeltas := map[string]float64{"Up": -7.25}

	plan := BuildSellSizingPlan(state, "Up", 20, 3, 0.5, "trade", targetDeltas, SizingConfig{
		SizePercent: 10,
		SizingMode:  "percent",
	})

	if !state.TargetSeen["Up"] || plan.TargetQty != 50 {
		t.Fatalf("unexpected target info %#v", plan)
	}
	if math.Abs(plan.TargetDelta+7.25) > 0.000001 {
		t.Fatalf("target delta = %.4f, want -7.25", plan.TargetDelta)
	}
	if _, exists := targetDeltas["Up"]; exists {
		t.Fatal("expected consumed target delta to be removed")
	}
}

func TestBuildSellSizingPlanPositionSignalUsesMasterTradeSize(t *testing.T) {
	state := NewRuntimeState()
	state.TargetSeen["Down"] = true
	state.TargetShares["Down"] = 25
	targetDeltas := map[string]float64{"Down": -9}

	plan := BuildSellSizingPlan(state, "Down", 20, 4, 0.5, "position", targetDeltas, SizingConfig{
		SizePercent: 10,
		SizingMode:  "percent",
	})

	if !plan.PositionSignal {
		t.Fatal("expected position signal")
	}
	if math.Abs(plan.TargetDelta+4) > 0.000001 {
		t.Fatalf("target delta = %.4f, want -4", plan.TargetDelta)
	}
	if _, exists := targetDeltas["Down"]; !exists {
		t.Fatal("expected target deltas to remain untouched for position signals")
	}
}

func TestBuildSellSizingPlanFallsBackWithoutTargetStateAndCapsLocalQty(t *testing.T) {
	plan := BuildSellSizingPlan(nil, "Up", 2.25, 10, 0.4, "trade", nil, SizingConfig{
		SizeShares:   50,
		SizingMode:   "shares",
		MaxTradeSize: 0,
	})

	if plan.TargetQty != 0 {
		t.Fatalf("target qty = %.4f, want 0", plan.TargetQty)
	}
	if math.Abs(plan.RequestedQty-2.25) > 0.000001 {
		t.Fatalf("requested qty = %.4f, want capped local qty 2.25", plan.RequestedQty)
	}
}
