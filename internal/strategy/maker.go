package strategy

import "math"

type MakerParams struct {
	QuoteStep           float64
	DefaultQuoteGap     float64
	InventorySkewStep   float64
	QuoteSizeSkewFactor float64
	CashUsagePerOutcome float64
	MinQuoteShares      float64
}

func ComputeMakerSellFeeUsdc(shares, price float64, feeRateBps int) float64 {
	if feeRateBps <= 0 || shares <= 0 || price <= 0 {
		return 0
	}
	return shares * 0.25 * math.Pow(price*(1.0-price), 2.0) * price
}

func ComputeMakerInventorySkew(positionShares, peerShares, targetShares float64) float64 {
	if targetShares <= 0 {
		return 0
	}
	skew := (positionShares - peerShares) / targetShares
	return clampFloat64(skew, -1.0, 1.0)
}

func ComputeMakerSkewedQuote(isBuy bool, bid, ask, skew, quoteGap float64, params MakerParams) (float64, bool) {
	if ask <= 0 || ask-bid <= params.QuoteStep*2 {
		return 0, false
	}
	if quoteGap <= 0 {
		quoteGap = params.DefaultQuoteGap
	}

	var minPrice, maxPrice float64
	if isBuy {
		minPrice = params.QuoteStep
		maxPrice = ask - params.QuoteStep
	} else {
		minPrice = params.QuoteStep
		if bid > 0 {
			minPrice = bid + params.QuoteStep
		}
		maxPrice = 1.0 - params.QuoteStep
	}

	if maxPrice < minPrice {
		return 0, false
	}

	mid := (bid + ask) / 2
	base := mid
	if isBuy {
		base = mid - quoteGap - (skew * params.InventorySkewStep)
	} else {
		base = mid + quoteGap - (skew * params.InventorySkewStep)
	}

	price := roundToStep(clampFloat64(base, minPrice, maxPrice), params.QuoteStep)
	if price < minPrice || price > maxPrice {
		return 0, false
	}
	return price, true
}

func ComputeMakerBuyQty(baseShares, positionShares, skew, maxInventory, cash, price float64, params MakerParams, normalizeQty func(float64) float64) float64 {
	if price <= 0 || cash <= 0 || positionShares >= maxInventory {
		return 0
	}

	// PROTECT AGAINST ADVERSE SELECTION: Stop accumulating toxic bags.
	// If our inventory is already heavily skewed to this side, do not buy more.
	if skew >= 0.8 {
		return 0
	}

	qty := baseShares * (1.0 - math.Max(0, skew)*params.QuoteSizeSkewFactor)
	remainingInventory := maxInventory - positionShares
	if qty > remainingInventory {
		qty = remainingInventory
	}
	affordable := (cash * params.CashUsagePerOutcome) / price
	if qty > affordable {
		qty = affordable
	}
	if normalizeQty != nil {
		qty = normalizeQty(qty)
	}
	if qty < params.MinQuoteShares {
		return 0
	}
	return qty
}

func ComputeMakerSellQty(baseShares, positionShares, skew float64, params MakerParams, normalizeQty func(float64) float64) float64 {
	if positionShares <= 0 {
		return 0
	}
	qty := baseShares * (1.0 + math.Max(0, skew)*params.QuoteSizeSkewFactor)
	if qty > positionShares {
		qty = positionShares
	}
	if normalizeQty != nil {
		qty = normalizeQty(qty)
	}
	if qty < params.MinQuoteShares {
		return 0
	}
	return qty
}

func ComputeMakerProtectedSellQuote(bid, ask, avgCost, minEdge, skew, quoteGap float64, feeRateBps int, params MakerParams) (float64, bool) {
	price, ok := ComputeMakerSkewedQuote(false, bid, ask, skew, quoteGap, params)
	if !ok {
		return 0, false
	}

	minPrice := params.QuoteStep
	if bid > 0 {
		minPrice = roundToStep(bid+params.QuoteStep, params.QuoteStep)
	}
	maxPrice := 1.0 - params.QuoteStep
	if maxPrice < minPrice {
		return 0, false
	}

	// Rely purely on inventory skew instead of a stubborn cost-basis check.
	// This prevents accumulating toxic bags of losing outcomes.
	price = roundToStep(clampFloat64(price, minPrice, maxPrice), params.QuoteStep)
	return price, true
}

func ShouldMakerBlockBuy(positionShares float64, sellOK bool, peerShares, peerAvgCost, price, minEdge float64) bool {
	if price <= 0 {
		return true
	}

	// ADVERSE SELECTION PROTECTION:
	// If our position is significantly larger than the peer position, block further
	// buying. This prevents us from accumulating a massive "trash bag" when the
	// market trends strongly one way and our sells aren't getting filled.
	if positionShares > 0 && positionShares >= peerShares*2.0 && positionShares >= 10.0 {
		return true
	}

	if positionShares > 0 && !sellOK && positionShares >= peerShares {
		return true
	}
	if peerShares > positionShares && peerAvgCost > 0 {
		maxCompletionPrice := (1.0 - minEdge) - peerAvgCost
		if price > maxCompletionPrice+1e-9 {
			return true
		}
	}
	return false
}

func clampFloat64(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func roundToStep(v, step float64) float64 {
	if step <= 0 {
		return v
	}
	return math.Round(v/step) * step
}
