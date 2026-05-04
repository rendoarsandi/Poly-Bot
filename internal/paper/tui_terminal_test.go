package paper

import "testing"

func TestLooksTerminalBookRecognizesOneSidedHighAskEndState(t *testing.T) {
	if !looksTerminalBook(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0, "Up": 0},
		map[string]float64{"Down": 1.00, "Up": 0.01},
	) {
		t.Fatal("expected high-ask/low-ask terminal book to be recognized")
	}
}
