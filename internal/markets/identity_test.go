package markets

import (
	"testing"

	"Market-bot/internal/api"
)

func TestScopedMarketID_UsesConditionIDToSeparateRounds(t *testing.T) {
	m1 := &api.Market{ConditionID: "0xabcdef1234567890"}
	m2 := &api.Market{ConditionID: "0x1234567890abcdef"}

	id1 := ScopedMarketID("btc", m1)
	id2 := ScopedMarketID("btc", m2)

	if id1 != "BTC#abcdef12" {
		t.Fatalf("expected first scoped id BTC#abcdef12, got %q", id1)
	}
	if id2 != "BTC#12345678" {
		t.Fatalf("expected second scoped id BTC#12345678, got %q", id2)
	}
	if id1 == id2 {
		t.Fatalf("expected different rounds to produce different scoped ids, got %q", id1)
	}
}

func TestScopedMarketID_FallsBackToAssetWhenNoFingerprintExists(t *testing.T) {
	id := ScopedMarketID("eth", &api.Market{})
	if id != "ETH" {
		t.Fatalf("expected fallback id ETH, got %q", id)
	}
}
