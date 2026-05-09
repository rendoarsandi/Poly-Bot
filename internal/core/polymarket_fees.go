package core

import "math"

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
// All fees are rounded to five decimal places, with a minimum fee of 0.00001 USDC.
func PolymarketTakerFeeUSDC(shares, price float64, feeRateBps int) float64 {
	if shares <= 0 || price <= 0 || price >= 1 || feeRateBps <= 0 {
		return 0
	}
	fee := shares * PolymarketFeeRateDecimal(feeRateBps) * price * (1.0 - price)

	// Any calculated fee smaller than 0.00001 USDC rounds down to zero
	if fee < 0.00001 {
		return 0
	}

	// Round to 5 decimal places as per official docs
	return math.Round(fee*100000.0) / 100000.0
}

// PolymarketBuyFeeShares converts the taker fee to the share-denominated amount
// collected on Polymarket buy orders.
func PolymarketBuyFeeShares(shares, price float64, feeRateBps int) float64 {
	if shares <= 0 || price <= 0 || price >= 1 || feeRateBps <= 0 {
		return 0
	}
	// Fee in USDC / Price = Fee in Shares
	return PolymarketTakerFeeUSDC(shares, price, feeRateBps) / price
}
