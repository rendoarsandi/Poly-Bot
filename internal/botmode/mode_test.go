package botmode

import (
	"reflect"
	"testing"
)

func TestNormalizeArbMode(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty defaults to taker", in: "", want: ArbModeTaker},
		{name: "maker", in: "maker", want: ArbModeMaker},
		{name: "copytrade", in: "copytrade", want: ArbModeCopytrade},
		{name: "laddered", in: "laddered-taker", want: ArbModeLadderedTaker},
		{name: "binance gap", in: "binance-gap", want: ArbModeBinanceGap},
		{name: "trim and lowercase", in: "  MaKeR ", want: ArbModeMaker},
		{name: "unknown defaults to taker", in: "weird", want: ArbModeTaker},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeArbMode(tt.in); got != tt.want {
				t.Fatalf("NormalizeArbMode(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestTakerCloseModeActive(t *testing.T) {
	if !TakerCloseModeActive(ArbModeTaker, true) {
		t.Fatal("expected taker-close to be active in taker mode")
	}

	for _, mode := range []string{ArbModeMaker, ArbModeCopytrade, ArbModeBinanceGap, ArbModeLadderedTaker} {
		if TakerCloseModeActive(mode, true) {
			t.Fatalf("expected taker-close to be inactive in %s mode", mode)
		}
	}

	if TakerCloseModeActive(ArbModeTaker, false) {
		t.Fatal("expected taker-close to remain inactive when disabled")
	}
}

func TestSplitStrategyAllowed(t *testing.T) {
	if !SplitStrategyAllowed(ArbModeTaker, false, "polymarket") {
		t.Fatal("expected split strategy to be allowed for plain taker mode")
	}
	if !SplitStrategyActive(ArbModeTaker, false, "polymarket", true) {
		t.Fatal("expected active split strategy when enabled in plain taker mode")
	}

	for _, tc := range []struct {
		name     string
		mode     string
		taker    bool
		exchange string
	}{
		{name: "laddered", mode: ArbModeLadderedTaker, exchange: "polymarket"},
		{name: "binance gap", mode: ArbModeBinanceGap, exchange: "polymarket"},
		{name: "copytrade", mode: ArbModeCopytrade, exchange: "polymarket"},
		{name: "maker", mode: ArbModeMaker, exchange: "polymarket"},
		{name: "taker close", mode: ArbModeTaker, taker: true, exchange: "polymarket"},
		{name: "kalshi", mode: ArbModeTaker, exchange: "kalshi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if SplitStrategyAllowed(tc.mode, tc.taker, tc.exchange) {
				t.Fatalf("expected split strategy to be disallowed for mode=%q takerClose=%v exchange=%q", tc.mode, tc.taker, tc.exchange)
			}
			if SplitStrategyActive(tc.mode, tc.taker, tc.exchange, true) {
				t.Fatalf("expected split strategy to stay inactive for mode=%q takerClose=%v exchange=%q", tc.mode, tc.taker, tc.exchange)
			}
		})
	}

	if SplitStrategyActive(ArbModeTaker, false, "polymarket", false) {
		t.Fatal("expected disabled split strategy to stay inactive")
	}
}

func TestModesOrder(t *testing.T) {
	want := []string{
		ArbModeTaker,
		ArbModeLadderedTaker,
		ArbModeBinanceGap,
		ArbModeCopytrade,
		ArbModeMaker,
	}
	if got := Modes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Modes() = %v, want %v", got, want)
	}
}
