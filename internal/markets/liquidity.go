package markets

import (
	"math"
	"sort"

	"Market-bot/internal/paper"
)

// EstimateMatchedLiquidity computes how many shares can be simultaneously
// filled on both sides of a binary market while satisfying priceCheck.
//
// levels1/levels2 are copied before sorting; the originals are NOT mutated.
//
// less defines the sort order:
//   - ascending (asks): func(i,j int, s []paper.MarketLevel) bool { return s[i].Price < s[j].Price }
//   - descending (bids): func(i,j int, s []paper.MarketLevel) bool { return s[i].Price > s[j].Price }
//
// priceCheck determines whether a price-pair is still within the profitability
// threshold:
//   - buy arb: func(p1, p2 float64) bool { return p1+p2 <= maxSum }
//   - sell arb: func(p1, p2 float64) bool { return p1+p2 >= minSum }
func EstimateMatchedLiquidity(
	levels1, levels2 []paper.MarketLevel,
	less func(i, j int, levels []paper.MarketLevel) bool,
	priceCheck func(p1, p2 float64) bool,
) float64 {
	// Work on copies so callers' slices are not modified.
	ls1 := make([]paper.MarketLevel, len(levels1))
	copy(ls1, levels1)
	ls2 := make([]paper.MarketLevel, len(levels2))
	copy(ls2, levels2)

	sort.Slice(ls1, func(i, j int) bool { return less(i, j, ls1) })
	sort.Slice(ls2, func(i, j int) bool { return less(i, j, ls2) })

	totalLiq := 0.0
	i, j := 0, 0
	for i < len(ls1) && j < len(ls2) {
		if !priceCheck(ls1[i].Price, ls2[j].Price) {
			break
		}
		matched := math.Min(ls1[i].Size, ls2[j].Size)
		totalLiq += matched
		if ls1[i].Size <= ls2[j].Size {
			ls2[j].Size -= ls1[i].Size
			i++
		} else {
			ls1[i].Size -= ls2[j].Size
			j++
		}
	}
	return totalLiq
}
