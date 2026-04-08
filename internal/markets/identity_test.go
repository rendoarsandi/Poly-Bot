package markets

import (
	"testing"

	"Market-bot/internal/api"
)

func TestScopedMarketID_PrefersFullSlug(t *testing.T) {
	m1 := &api.Market{
		Slug:        "btc-updown-5m-1775645400",
		ConditionID: "0xabcdef1234567890",
	}
	m2 := &api.Market{
		Slug:        "btc-updown-5m-1775645700",
		ConditionID: "0x1234567890abcdef",
	}

	id1 := ScopedMarketID("btc", m1)
	id2 := ScopedMarketID("btc", m2)

	if id1 != "btc-updown-5m-1775645400" {
		t.Fatalf("expected first market slug id, got %q", id1)
	}
	if id2 != "btc-updown-5m-1775645700" {
		t.Fatalf("expected second market slug id, got %q", id2)
	}
	if id1 == id2 {
		t.Fatalf("expected different rounds to produce different slug ids, got %q", id1)
	}
}

func TestScopedMarketID_FallsBackToConditionFingerprintWithoutSlug(t *testing.T) {
	id := ScopedMarketID("btc", &api.Market{ConditionID: "0xabcdef1234567890"})
	if id != "BTC#abcdef12" {
		t.Fatalf("expected condition fallback id BTC#abcdef12, got %q", id)
	}
}

func TestScopedMarketID_FallsBackToAssetWhenNoSlugOrFingerprintExists(t *testing.T) {
	id := ScopedMarketID("eth", &api.Market{})
	if id != "ETH" {
		t.Fatalf("expected fallback id ETH, got %q", id)
	}
}
