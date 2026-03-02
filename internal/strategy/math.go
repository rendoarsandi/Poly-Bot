package strategy

import (
	"fmt"
	"math"
	"strconv"
)

// CalculateDiscountSum converts string prices to float64 and returns their sum.
func CalculateDiscountSum(priceYes, priceNo string) (float64, error) {
	pYes, err := strconv.ParseFloat(priceYes, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid Yes price: %w", err)
	}

	pNo, err := strconv.ParseFloat(priceNo, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid No price: %w", err)
	}

	return pYes + pNo, nil
}

// TradeMetrics holds computed financial metrics for one arbitrage trade leg.
type TradeMetrics struct {
	Cost     float64 // Total cost to enter: shares × (price1 + price2)
	Overhead float64 // Estimated fee cost
	Gross    float64 // Gross profit ignoring fees: shares × (1 − sum)
	Net      float64 // Net profit after fees: Gross − Overhead
}

// CalculateTradeMetricsCurve computes trade metrics using Polymarket's
// price-curve fee formula:
//
//	fee_per_side = shares × feeRate × (p × (1−p))^exponent
//
// This accurately models the actual on-chain fee for crypto markets
// where feeRate = 0.25, exponent = 2.0. The curve is lowest near 0/1 and highest at 0.5.
func CalculateTradeMetricsCurve(shares, price1, price2 float64, feeRateBps int) TradeMetrics {
	sum := price1 + price2
	cost := shares * sum
	gross := shares * (1.0 - sum)

	overhead := 0.0
	if feeRateBps > 0 {
		feeRate := 0.25
		exponent := 2.0

		fee1Tokens := shares * feeRate * math.Pow(price1*(1.0-price1), exponent)
		fee2Tokens := shares * feeRate * math.Pow(price2*(1.0-price2), exponent)

		fee1Usdc := fee1Tokens * price1
		fee2Usdc := fee2Tokens * price2

		overhead = fee1Usdc + fee2Usdc
	}

	return TradeMetrics{
		Cost:     cost,
		Overhead: overhead,
		Gross:    gross,
		Net:      gross - overhead,
	}
}

// CalculateTradeMetricsFlat computes trade metrics using a simple flat fee rate
// applied to the total cost:
//
//	overhead = cost × (feeRateBps/10000)
//
// This is a conservative approximation used in real-bot order sizing where the
// per-side curve fee is not known precisely at order time.
func CalculateTradeMetricsFlat(shares, sum float64, feeRateBps int) TradeMetrics {
	cost := shares * sum
	gross := shares * (1.0 - sum)

	overhead := 0.0
	if feeRateBps > 0 {
		overhead = cost * (float64(feeRateBps) / 10000.0)
	}

	return TradeMetrics{
		Cost:     cost,
		Overhead: overhead,
		Gross:    gross,
		Net:      gross - overhead,
	}
}
