package paper

import (
	"math"
	"testing"
	"time"

	"Market-bot/internal/api"
)

func TestPaperDirectionalSignalTrackerSnapshotUsesLookbackMid(t *testing.T) {
	tracker := NewDirectionalSignalTracker(1500*time.Millisecond, []string{"Yes"})
	base := time.Unix(1700000000, 0)
	tracker.Record("Yes", 0.44, 0.46, base)
	tracker.Record("Yes", 0.445, 0.465, base.Add(800*time.Millisecond))
	tracker.Record("Yes", 0.45, 0.47, base.Add(1700*time.Millisecond))

	snap := tracker.Snapshot("Yes", base.Add(1700*time.Millisecond))
	if !snap.Ready {
		t.Fatal("expected snapshot to be ready once lookback history exists")
	}
	if math.Abs(snap.BaselineMid-0.45) > 0.000001 {
		t.Fatalf("expected baseline mid 0.45, got %.4f", snap.BaselineMid)
	}
	if math.Abs(snap.DeltaCents-1.0) > 0.000001 {
		t.Fatalf("expected delta 1.0c, got %.4f", snap.DeltaCents)
	}
}

func TestPaperDirectionalSignalTrackerSnapshotResetsAfterGap(t *testing.T) {
	tracker := NewDirectionalSignalTracker(1500*time.Millisecond, []string{"Yes"})
	base := time.Unix(1700000000, 0)
	tracker.Record("Yes", 0.44, 0.46, base)
	tracker.Record("Yes", 0.47, 0.49, base.Add(4*time.Second))

	snap := tracker.Snapshot("Yes", base.Add(4*time.Second))
	if snap.Ready {
		t.Fatal("expected quote tracker to stay unready after a quote gap until new history is collected")
	}

	tracker.Record("Yes", 0.475, 0.495, base.Add(4800*time.Millisecond))
	tracker.Record("Yes", 0.48, 0.50, base.Add(5600*time.Millisecond))
	snap = tracker.Snapshot("Yes", base.Add(5600*time.Millisecond))
	if !snap.Ready {
		t.Fatal("expected quote tracker to become ready after a full fresh post-gap window")
	}
	if math.Abs(snap.BaselineMid-0.48) > 0.000001 {
		t.Fatalf("expected post-gap baseline mid 0.48, got %.4f", snap.BaselineMid)
	}
}

func TestPaperEvaluateBinanceGapSignalUpDirection(t *testing.T) {
	tracker := NewDirectionalSignalTracker(1500*time.Millisecond, []string{"Yes", "No"})
	base := time.Unix(1700000000, 0)
	end := base.Add(1700 * time.Millisecond)

	tracker.Record("Yes", 0.44, 0.46, base)
	tracker.Record("No", 0.54, 0.56, base)
	tracker.Record("Yes", 0.448, 0.452, base.Add(800*time.Millisecond))
	tracker.Record("No", 0.548, 0.552, base.Add(800*time.Millisecond))
	tracker.Record("Yes", 0.456, 0.460, end)
	tracker.Record("No", 0.536, 0.540, end)

	signal, reason := EvaluateBinanceGapSignal(end, DirectionalOutcomes{Up: "Yes", Down: "No"}, map[string]float64{"Yes": 0.456, "No": 0.536}, map[string]float64{"Yes": 0.460, "No": 0.540}, api.BinanceFuturesSignalSnapshot{
		Symbol:       "BTCUSDT",
		DeltaPercent: 0.65,
		UpdatedAt:    end,
		Ready:        true,
	}, tracker, 3*time.Second)
	if reason != "" {
		t.Fatalf("expected signal to be ready, got reason %q", reason)
	}
	if signal.TargetOutcome != "Yes" || signal.SignalLabel != "UP" {
		t.Fatalf("unexpected direction: %#v", signal)
	}
	if math.Abs(signal.PolyTargetMoveCents-0.8) > 0.000001 {
		t.Fatalf("expected target move 0.8c, got %.4f", signal.PolyTargetMoveCents)
	}
	if math.Abs(signal.PolyOppositeMoveCents-1.2) > 0.000001 {
		t.Fatalf("expected opposite move 1.2c, got %.4f", signal.PolyOppositeMoveCents)
	}
	if math.Abs(signal.PolyFavorableMoveCents-1.2) > 0.000001 {
		t.Fatalf("expected favorable move 1.2c, got %.4f", signal.PolyFavorableMoveCents)
	}
	if signal.PolyAdverseMoveCents != 0 {
		t.Fatalf("expected no adverse move, got %.4f", signal.PolyAdverseMoveCents)
	}
	if math.Abs(signal.TargetSpreadCents-0.4) > 0.000001 {
		t.Fatalf("expected spread 0.4c, got %.4f", signal.TargetSpreadCents)
	}
}

func TestPaperEvaluateBinanceGapSignalRequiresPolyHistory(t *testing.T) {
	base := time.Unix(1700000000, 0)
	signal, reason := EvaluateBinanceGapSignal(base, DirectionalOutcomes{Up: "Yes", Down: "No"}, map[string]float64{"Yes": 0.45}, map[string]float64{"Yes": 0.46}, api.BinanceFuturesSignalSnapshot{
		Symbol:       "BTCUSDT",
		DeltaPercent: 0.4,
		UpdatedAt:    base,
		Ready:        true,
	}, NewDirectionalSignalTracker(1500*time.Millisecond, []string{"Yes", "No"}), 3*time.Second)
	if reason == "" {
		t.Fatalf("expected missing poly history to block signal: %#v", signal)
	}
}
