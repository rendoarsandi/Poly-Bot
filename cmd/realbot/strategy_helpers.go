package main

import (
	"fmt"
	"math"

	"Market-bot/internal/paper"
)

func pairMarginPercent(sum float64) float64 {
	return (1.0 - sum) * 100.0
}

func localBoughtPositionAvg(engine *paper.Engine, marketID, outcome string) (qty, avgPrice float64) {
	if engine == nil || marketID == "" || outcome == "" {
		return 0, 0
	}
	positions := engine.GetPositions()
	totalCost := 0.0
	for _, pos := range positions {
		if pos.MarketID != marketID || pos.Outcome != outcome || pos.Quantity <= 0 {
			continue
		}
		qty += pos.Quantity
		totalCost += pos.TotalCost
	}
	if qty <= 0 {
		return 0, 0
	}
	return qty, totalCost / qty
}

func realbotPanicBuyCompletionGuard(engine *paper.Engine, marketID, outcome0, outcome1 string, ask0, ask1, minMarginPercent float64) (bool, string) {
	if engine == nil {
		return false, ""
	}
	maxCompletionSum := 1.0 - (minMarginPercent / 100.0)
	if maxCompletionSum > 1.0 {
		maxCompletionSum = 1.0
	}
	if maxCompletionSum < 0 {
		maxCompletionSum = 0
	}

	qty0, avg0 := localBoughtPositionAvg(engine, marketID, outcome0)
	qty1, avg1 := localBoughtPositionAvg(engine, marketID, outcome1)

	if excess0 := qty0 - qty1; excess0 > 1e-6 && avg0 > 0 && ask1 > 0 {
		completionSum := avg0 + ask1
		if completionSum > maxCompletionSum+1e-9 {
			return true, fmt.Sprintf("existing %s imbalance %s @ avg %.3f would complete via %s ask %.3f at $%.3f, above $%.3f target", outcome0, formatShareQty(excess0), avg0, outcome1, ask1, completionSum, maxCompletionSum)
		}
	}
	if excess1 := qty1 - qty0; excess1 > 1e-6 && avg1 > 0 && ask0 > 0 {
		completionSum := avg1 + ask0
		if completionSum > maxCompletionSum+1e-9 {
			return true, fmt.Sprintf("existing %s imbalance %s @ avg %.3f would complete via %s ask %.3f at $%.3f, above $%.3f target", outcome1, formatShareQty(excess1), avg1, outcome0, ask0, completionSum, maxCompletionSum)
		}
	}
	return false, ""
}

func clampExecutionMarginFloor(minMarginPercent, executionFloorPercent float64) float64 {
	if executionFloorPercent > minMarginPercent {
		return minMarginPercent
	}
	return executionFloorPercent
}

func maxExecutablePairSum(executionFloorPercent, maxAskPrice float64) float64 {
	maxSum := 1.0 - (executionFloorPercent / 100.0)
	if maxAskPrice > 0 {
		capSum := maxAskPrice * 2.0
		if maxSum > capSum {
			maxSum = capSum
		}
	}
	if maxSum < 0 {
		return 0
	}
	return maxSum
}

func minExecutablePairSum(executionFloorPercent, minAskPrice float64) float64 {
	minSum := 1.0 + (executionFloorPercent / 100.0)
	if minAskPrice > 0 {
		floorSum := minAskPrice * 2.0
		if minSum < floorSum {
			minSum = floorSum
		}
	}
	if minSum > 2.0 {
		return 2.0
	}
	return minSum
}

func normalizeExecutionToleranceFraction(raw float64) float64 {
	raw = math.Abs(raw)
	switch {
	case raw == 0:
		return 0
	case raw <= 0.25:
		return raw
	default:
		return raw / 100.0
	}
}
