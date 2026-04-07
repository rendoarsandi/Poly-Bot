package quoteutil

import (
	"testing"
	"time"
)

func validPair(_ []string, bids, asks map[string]float64) bool {
	return bids["Yes"] > 0 && bids["No"] > 0 && asks["Yes"] > bids["Yes"] && asks["No"] > bids["No"]
}

func terminalPair(_ []string, bids, asks map[string]float64) bool {
	return bids["Yes"] >= 0.985 || asks["No"] <= 0.015
}

func highBidPair(_ []string, bids, _ map[string]float64) bool {
	return bids["Yes"] >= 0.60 || bids["No"] >= 0.60
}

func reasonPair(outcomes []string, bids, asks map[string]float64) string {
	if validPair(outcomes, bids, asks) {
		return ""
	}
	return "bad quote"
}

func TestLatestUpdate(t *testing.T) {
	base := time.Date(2026, 4, 7, 10, 0, 0, 0, time.UTC)
	state := map[string]State{
		"No":  {UpdatedAt: base.Add(150 * time.Millisecond), Source: "ws"},
		"Yes": {UpdatedAt: base.Add(350 * time.Millisecond), Source: "rest"},
	}

	updatedAt, source := LatestUpdate([]string{"No", "Yes"}, state)
	if !updatedAt.Equal(base.Add(350 * time.Millisecond)) {
		t.Fatalf("expected freshest timestamp %s, got %s", base.Add(350*time.Millisecond), updatedAt)
	}
	if source != "rest" {
		t.Fatalf("expected freshest source rest, got %q", source)
	}
}

func TestNormalizeDisplaySource(t *testing.T) {
	if got := NormalizeDisplaySource("ws-bbo"); got != "WS" {
		t.Fatalf("expected WS, got %q", got)
	}
	if got := NormalizeDisplaySource("rest-exec"); got != "REST" {
		t.Fatalf("expected REST, got %q", got)
	}
	if got := NormalizeDisplaySource("unknown"); got != "WS" {
		t.Fatalf("expected WS default, got %q", got)
	}
}

func TestSyncDisplayQuotes(t *testing.T) {
	outcomes := []string{"Yes", "No"}
	displayBids := map[string]float64{"Yes": 0.4, "No": 0.5}
	displayAsks := map[string]float64{"Yes": 0.42, "No": 0.52}
	liveBids := map[string]float64{"Yes": 0.43, "No": 0.54}
	liveAsks := map[string]float64{"Yes": 0.44, "No": 0.55}

	if !SyncDisplayQuotes(outcomes, liveBids, liveAsks, displayBids, displayAsks, false, validPair, terminalPair) {
		t.Fatal("expected sane quotes to update display")
	}
	if displayBids["Yes"] != 0.43 || displayAsks["No"] != 0.55 {
		t.Fatalf("expected display to copy sane live quotes, got bids=%v asks=%v", displayBids, displayAsks)
	}

	if SyncDisplayQuotes(outcomes, liveBids, liveAsks, displayBids, displayAsks, false, validPair, terminalPair) {
		t.Fatal("expected identical quotes to avoid redundant display updates")
	}
}

func TestShouldClearLocalPairQuotes(t *testing.T) {
	outcomes := []string{"Yes", "No"}
	bids := map[string]float64{"Yes": 0.48, "No": 0.49}
	asks := map[string]float64{"Yes": 0.50, "No": 0.49}

	if !ShouldClearLocalPairQuotes(outcomes, bids, asks, validPair, terminalPair, highBidPair) {
		t.Fatal("expected invalid non-terminal quotes without high bid to be cleared")
	}

	bids["Yes"] = 0.61
	if ShouldClearLocalPairQuotes(outcomes, bids, asks, validPair, terminalPair, highBidPair) {
		t.Fatal("expected high bid regime to preserve local quotes")
	}
}

func TestShouldReconnectWS(t *testing.T) {
	outcomes := []string{"Yes", "No"}
	validBids := map[string]float64{"Yes": 0.48, "No": 0.49}
	validAsks := map[string]float64{"Yes": 0.50, "No": 0.51}
	invalidAsks := map[string]float64{"Yes": 0.50, "No": 0}

	if ShouldReconnectWS(outcomes, validBids, validAsks, 30*time.Second, 15*time.Second, false, reasonPair) {
		t.Fatal("expected valid stale book to avoid reconnect")
	}
	if !ShouldReconnectWS(outcomes, validBids, invalidAsks, 30*time.Second, 15*time.Second, false, reasonPair) {
		t.Fatal("expected invalid stale book to reconnect")
	}
	if ShouldReconnectWS(outcomes, validBids, invalidAsks, 30*time.Second, 15*time.Second, true, reasonPair) {
		t.Fatal("expected terminal book state to suppress reconnect")
	}
}

func TestClampGuardAge(t *testing.T) {
	hardMax := 1500 * time.Millisecond
	if got := ClampGuardAge(3*time.Second, hardMax); got != hardMax {
		t.Fatalf("expected cap at hard max %s, got %s", hardMax, got)
	}
	if got := ClampGuardAge(400*time.Millisecond, hardMax); got != 400*time.Millisecond {
		t.Fatalf("expected stricter local age to pass through, got %s", got)
	}
}

func TestPairQuoteAge(t *testing.T) {
	now := time.Now()
	state := map[string]State{
		"Yes": {UpdatedAt: now.Add(-100 * time.Millisecond)},
		"No":  {UpdatedAt: now.Add(-350 * time.Millisecond)},
	}
	if got := PairQuoteAge(now, []string{"Yes", "No"}, state); got < 300*time.Millisecond || got > 400*time.Millisecond {
		t.Fatalf("expected max observed age near 350ms, got %s", got)
	}

	delete(state, "No")
	if got := PairQuoteAge(now, []string{"Yes", "No"}, state); got < time.Hour {
		t.Fatalf("expected missing quote timestamp to count as stale, got %s", got)
	}
}
