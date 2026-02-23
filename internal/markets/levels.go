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

// ApplyDelta applies a price/size delta from a WebSocket price_change event to the current orderbook.
// If size is 0, the level is removed. If the level exists, its size is updated.
// If it's a new level, it is inserted in the correct sorted order (ascending for asks, descending for bids).
// Since the structure doesn't differentiate bid vs ask intrinsically, we assume caller handles sorting:
// Bids should be sorted descending, Asks should be sorted ascending.
// For simplicity, we just insert, sort, and return.
// isBid determines the sort order.
func ApplyDelta(book []paper.MarketLevel, price, size float64, isBid bool) []paper.MarketLevel {
	// Find if level exists
	found := false
	for i, level := range book {
		// Use a tiny epsilon for float comparison to avoid precision issues
		if price > level.Price-0.000001 && price < level.Price+0.000001 {
			found = true
			if size <= 0 {
				// Remove level
				book = append(book[:i], book[i+1:]...)
			} else {
				// Update size
				book[i].Size = size
			}
			break
		}
	}

	// Insert new level
	if !found && size > 0 {
		book = append(book, paper.MarketLevel{Price: price, Size: size})
	}

	// Resort book
	if isBid {
		// Bids: descending
		for i := 0; i < len(book)-1; i++ {
			for j := i + 1; j < len(book); j++ {
				if book[i].Price < book[j].Price {
					book[i], book[j] = book[j], book[i]
				}
			}
		}
	} else {
		// Asks: ascending
		for i := 0; i < len(book)-1; i++ {
			for j := i + 1; j < len(book); j++ {
				if book[i].Price > book[j].Price {
					book[i], book[j] = book[j], book[i]
				}
			}
		}
	}

	return book
}
