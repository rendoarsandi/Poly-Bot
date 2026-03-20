package main

import (
	"testing"
	"time"
)

func TestShouldPaperReconnectWSIgnoresQuietValidPair(t *testing.T) {
	outcomes := []string{"Down", "Up"}
	bids := map[string]float64{"Down": 0.44, "Up": 0.53}
	asks := map[string]float64{"Down": 0.46, "Up": 0.55}

	if shouldPaperReconnectWS(outcomes, bids, asks, 20*time.Second, false) {
		t.Fatal("expected a valid quiet pair to remain WS-only without reconnect churn")
	}

	asks["Up"] = 0
	if !shouldPaperReconnectWS(outcomes, bids, asks, 20*time.Second, false) {
		t.Fatal("expected reconnect when a stale WS book loses one side")
	}
}
