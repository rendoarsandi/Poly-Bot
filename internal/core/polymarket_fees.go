package core

import "math"

type PolymarketFeeCurve struct {
	Rate     float64
	Exponent float64
}

func (c PolymarketFeeCurve) normalized() PolymarketFeeCurve {
	if c.Rate <= 0 {
		return PolymarketFeeCurve{}
	}
	if c.Exponent <= 0 {
		c.Exponent = 1
	}
	return c
}

func PolymarketFeeCurveFromBps(feeRateBps int) PolymarketFeeCurve {
	if feeRateBps <= 0 {
		return PolymarketFeeCurve{}
	}
	return PolymarketFeeCurve{Rate: float64(feeRateBps) / 10000.0, Exponent: 1}
}

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
// where C is shares traded, feeRate is Polymarket's fee coefficient, and p is price.
// All fees are rounded to five decimal places, with a minimum fee of 0.00001 USDC.
func PolymarketTakerFeeUSDC(shares, price float64, feeRateBps int) float64 {
	return PolymarketTakerFeeUSDCForCurve(shares, price, PolymarketFeeCurveFromBps(feeRateBps))
}

func PolymarketTakerFeeUSDCForCurve(shares, price float64, curve PolymarketFeeCurve) float64 {
	curve = curve.normalized()
	if shares <= 0 || price <= 0 || price >= 1 || curve.Rate <= 0 {
		return 0
	}
	fee := shares * curve.Rate * math.Pow(price*(1.0-price), curve.Exponent)

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
	return PolymarketBuyFeeSharesForCurve(shares, price, PolymarketFeeCurveFromBps(feeRateBps))
}

func PolymarketBuyFeeSharesForCurve(shares, price float64, curve PolymarketFeeCurve) float64 {
	if shares <= 0 || price <= 0 || price >= 1 {
		return 0
	}
	// Fee in USDC / Price = Fee in Shares
	return PolymarketTakerFeeUSDCForCurve(shares, price, curve) / price
}
