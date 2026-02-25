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
		{Price: 0.50, Size: 100.5},
		{Price: 0.55, Size: 200.0},
	}

	result := LevelsToPriceDepth(apiLevels)

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
