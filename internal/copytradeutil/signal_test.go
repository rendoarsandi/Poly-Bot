package copytradeutil

import (
	"testing"

	"Market-bot/internal/api"
)

func TestParseSignal(t *testing.T) {
	got := ParseSignal(api.PublicTrade{
		Outcome: " Up ",
		Side:    " buy ",
		Size:    -3,
		Source:  "position-sync",
	})

	if got.Outcome != "Up" {
		t.Fatalf("outcome = %q, want Up", got.Outcome)
	}
	if got.Side != "BUY" {
		t.Fatalf("side = %q, want BUY", got.Side)
	}
	if got.Size != 0 {
		t.Fatalf("size = %.2f, want 0", got.Size)
	}
	if !got.PositionSignal {
		t.Fatal("expected position signal")
	}
}

func TestSignalSideHelpers(t *testing.T) {
	if !(Signal{Side: "BUY"}).IsBuy() || (Signal{Side: "BUY"}).IsSell() {
		t.Fatal("buy side helpers incorrect")
	}
	if !(Signal{Side: "SELL"}).IsSell() || (Signal{Side: "SELL"}).IsBuy() {
		t.Fatal("sell side helpers incorrect")
	}
	if !(Signal{Side: "BUY"}).SupportedSide() || !(Signal{Side: "SELL"}).SupportedSide() || (Signal{Side: "HOLD"}).SupportedSide() {
		t.Fatal("supported side helper incorrect")
	}
}

func TestSignalBelowMin(t *testing.T) {
	if !(Signal{Size: 0.01}).BelowMin(0.01, false) {
		t.Fatal("expected size at min to be blocked when not allowed")
	}
	if (Signal{Size: 0.01}).BelowMin(0.01, true) {
		t.Fatal("expected allowBelowMin to bypass min-size gate")
	}
}

func TestBuildRetrySellTrade(t *testing.T) {
	got := BuildRetrySellTrade(api.PublicTrade{
		ConditionID: " cond-1 ",
		Timestamp:   123,
		Source:      "position",
	}, "Up", 4.5)

	if got.ConditionID != "cond-1" || got.Outcome != "Up" || got.Side != "SELL" || got.Size != 4.5 || got.Timestamp != 123 || got.Source != "position" {
		t.Fatalf("unexpected retry trade %+v", got)
	}
}
