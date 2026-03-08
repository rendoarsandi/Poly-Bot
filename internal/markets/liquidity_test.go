package markets

import (
	"testing"

	"Market-bot/internal/paper"
)

func TestEstimateMatchedLiquidityBuyUsesAllDepthWithinExecutionWindow(t *testing.T) {
	asks0 := []paper.MarketLevel{{Price: 0.30, Size: 2}, {Price: 0.34, Size: 3}}
	asks1 := []paper.MarketLevel{{Price: 0.60, Size: 1}, {Price: 0.65, Size: 4}}

	got := EstimateMatchedLiquidity(
		asks0,
		asks1,
		func(i, j int, levels []paper.MarketLevel) bool { return levels[i].Price < levels[j].Price },
		func(p0, p1 float64) bool { return p0+p1 <= 0.97 },
	)

	if got != 2 {
		t.Fatalf("expected 2 matched buy shares within execution window, got %.2f", got)
	}
}

func TestEstimateMatchedLiquiditySellStopsWhenBidPairDropsBelowFloor(t *testing.T) {
	bids0 := []paper.MarketLevel{{Price: 0.55, Size: 2}, {Price: 0.52, Size: 3}}
	bids1 := []paper.MarketLevel{{Price: 0.51, Size: 1}, {Price: 0.50, Size: 5}}

	got := EstimateMatchedLiquidity(
		bids0,
		bids1,
		func(i, j int, levels []paper.MarketLevel) bool { return levels[i].Price > levels[j].Price },
		func(p0, p1 float64) bool { return p0+p1 >= 1.03 },
	)

	if got != 2 {
		t.Fatalf("expected 2 matched sell shares within execution floor, got %.2f", got)
	}
}
