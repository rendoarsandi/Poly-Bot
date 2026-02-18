package markets

import (
	"strconv"

	"Market-bot/internal/api"
	"Market-bot/internal/paper"
)

// LevelsToPriceDepth converts a slice of API PriceLevels (string fields) into
// the float64-typed MarketLevel slice used throughout the paper and trading
// packages. Entries that fail to parse are skipped silently.
func LevelsToPriceDepth(levels []api.PriceLevel) []paper.MarketLevel {
	result := make([]paper.MarketLevel, 0, len(levels))
	for _, l := range levels {
		p, err := strconv.ParseFloat(l.Price, 64)
		if err != nil {
			continue
		}
		s, err := strconv.ParseFloat(l.Size, 64)
		if err != nil {
			continue
		}
		result = append(result, paper.MarketLevel{Price: p, Size: s})
	}
	return result
}
