package strategy

import (
	"fmt"
	"strconv"

	"Market-bot/internal/core"
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

// CalculateTradeMetricsCurve computes trade metrics using Polymarket's documented
// taker fee formula across both legs.
func CalculateTradeMetricsCurve(shares, price1, price2 float64, feeRateBps int) TradeMetrics {
	return CalculateTradeMetricsFeeCurve(shares, price1, price2, core.PolymarketFeeCurveFromBps(feeRateBps))
}

func CalculateTradeMetricsFeeCurve(shares, price1, price2 float64, feeCurve core.PolymarketFeeCurve) TradeMetrics {
	sum := price1 + price2
	cost := shares * sum
	gross := shares * (1.0 - sum)

	overhead := core.PolymarketTakerFeeUSDCForCurve(shares, price1, feeCurve) +
		core.PolymarketTakerFeeUSDCForCurve(shares, price2, feeCurve)

	return TradeMetrics{
		Cost:     cost,
		Overhead: overhead,
		Gross:    gross,
		Net:      gross - overhead,
	}
}

// CalculateTradeMetricsFlat computes trade metrics using a simple flat fee rate
// applied to the total cost when only the combined entry price is known:
//
//	overhead = cost × (feeRateBps/10000)
//
// This remains an approximation and should only be used when per-leg prices are
// unavailable. Prefer CalculateTradeMetricsCurve whenever both prices are known.
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
