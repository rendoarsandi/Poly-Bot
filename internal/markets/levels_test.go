package markets

import (
	"reflect"
	"testing"

	"Market-bot/internal/api"
	"Market-bot/internal/paper"
)

func TestLevelsToPriceDepth(t *testing.T) {
	apiLevels := []api.PriceLevel{
		{Price: "0.50", Size: "100.5"},
		{Price: "0.55", Size: "200.0"},
		{Price: "invalid", Size: "10"},
		{Price: "0.60", Size: "invalid"},
	}

	expected := []paper.MarketLevel{
		{Price: 0.55, Size: 200.0},
		{Price: 0.50, Size: 100.5},
	}

	result := LevelsToPriceDepth(apiLevels, true)

	if !reflect.DeepEqual(result, expected) {
		t.Errorf("LevelsToPriceDepth failed. Expected %v, got %v", expected, result)
	}
}

func TestApplyDelta(t *testing.T) {
	// Initial book for Asks (ascending)
	initialAsks := []paper.MarketLevel{
		{Price: 0.40, Size: 100.0},
		{Price: 0.50, Size: 200.0},
		{Price: 0.60, Size: 300.0},
	}

	// Update existing level
	updatedAsks := ApplyDelta(initialAsks, 0.50, 250.0, false)
	if len(updatedAsks) != 3 || updatedAsks[1].Size != 250.0 {
		t.Errorf("ApplyDelta failed to update size: %v", updatedAsks)
	}

	// Add new level
	updatedAsks = ApplyDelta(updatedAsks, 0.45, 150.0, false)
	if len(updatedAsks) != 4 || updatedAsks[1].Price != 0.45 || updatedAsks[1].Size != 150.0 {
		t.Errorf("ApplyDelta failed to insert level correctly: %v", updatedAsks)
	}

	// Remove level
	updatedAsks = ApplyDelta(updatedAsks, 0.40, 0.0, false)
	if len(updatedAsks) != 3 || updatedAsks[0].Price != 0.45 {
		t.Errorf("ApplyDelta failed to remove level: %v", updatedAsks)
	}

	// Initial book for Bids (descending)
	initialBids := []paper.MarketLevel{
		{Price: 0.60, Size: 300.0},
		{Price: 0.50, Size: 200.0},
		{Price: 0.40, Size: 100.0},
	}

	// Insert new level
	updatedBids := ApplyDelta(initialBids, 0.55, 150.0, true)
	if len(updatedBids) != 4 || updatedBids[1].Price != 0.55 {
		t.Errorf("ApplyDelta failed to insert bid level correctly: %v", updatedBids)
	}
}

func TestRefreshTopOfBookFromDepthUsesOutcomeKeys(t *testing.T) {
	outcomes := []string{"Down", "Up"}
	bidDepth := map[string][]paper.MarketLevel{
		"Down": {{Price: 0.42, Size: 4}, {Price: 0.41, Size: 2}},
		"Up":   {{Price: 0.58, Size: 3}},
	}
	askDepth := map[string][]paper.MarketLevel{
		"Down": {{Price: 0.44, Size: 5}},
		"Up":   {{Price: 0.61, Size: 7}, {Price: 0.63, Size: 1}},
	}
	bestBids := map[string]float64{"token-down": 0.99}
	bestAsks := map[string]float64{"token-up": 0.99}

	RefreshTopOfBookFromDepth(outcomes, bidDepth, askDepth, bestBids, bestAsks)

	if got := bestBids["Down"]; got != 0.42 {
		t.Fatalf("expected Down best bid 0.42, got %.2f", got)
	}
	if got := bestAsks["Down"]; got != 0.44 {
		t.Fatalf("expected Down best ask 0.44, got %.2f", got)
	}
	if got := bestBids["Up"]; got != 0.58 {
		t.Fatalf("expected Up best bid 0.58, got %.2f", got)
	}
	if got := bestAsks["Up"]; got != 0.61 {
		t.Fatalf("expected Up best ask 0.61, got %.2f", got)
	}
	if got := bestBids["token-down"]; got != 0.99 {
		t.Fatalf("expected unrelated token-id key to remain untouched, got %.2f", got)
	}
}
