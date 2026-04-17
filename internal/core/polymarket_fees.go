package core

// PolymarketFeeRateDecimal converts fee rate basis points into the decimal
// multiplier used by the official taker-fee formula.
func PolymarketFeeRateDecimal(feeRateBps int) float64 {
	if feeRateBps <= 0 {
		return 0
	}
	return float64(feeRateBps) / 10000.0
}

// PolymarketTakerFeeUSDC computes taker fees using the official Polymarket docs:
// fee = C × feeRate × p × (1 - p)
// where C is shares traded, feeRate is the decimal fee rate, and p is price.
func PolymarketTakerFeeUSDC(shares, price float64, feeRateBps int) float64 {
	if shares <= 0 || price <= 0 || price >= 1 || feeRateBps <= 0 {
		return 0
	}
	return shares * PolymarketFeeRateDecimal(feeRateBps) * price * (1.0 - price)
}

// PolymarketBuyFeeShares converts the taker fee to the share-denominated amount
// collected on Polymarket buy orders.
func PolymarketBuyFeeShares(shares, price float64, feeRateBps int) float64 {
	if shares <= 0 || price <= 0 || price >= 1 || feeRateBps <= 0 {
		return 0
	}
	return PolymarketTakerFeeUSDC(shares, price, feeRateBps) / price
}
