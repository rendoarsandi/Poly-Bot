package core

import "fmt"

func ClampExecutionMarginFloor(minMarginPercent, executionFloorPercent float64) float64 {
	if executionFloorPercent > minMarginPercent {
		return minMarginPercent
	}
	return executionFloorPercent
}

func MaxExecutablePairSum(executionFloorPercent, maxAskPrice float64) float64 {
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

func MinExecutablePairSum(executionFloorPercent, minAskPrice float64) float64 {
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

func BuyExecutionLimitPrices(ask0, ask1, minAskPrice, maxAskPrice, executionFloorPercent float64) (float64, float64, error) {
	if minAskPrice <= 0 || minAskPrice >= 1 {
		return 0, 0, fmt.Errorf("invalid MinAskPrice %.6f", minAskPrice)
	}
	if maxAskPrice <= 0 || maxAskPrice >= 1 {
		return 0, 0, fmt.Errorf("invalid MaxAskPrice %.6f", maxAskPrice)
	}
	if minAskPrice > maxAskPrice {
		return 0, 0, fmt.Errorf("invalid ask range %.6f > %.6f", minAskPrice, maxAskPrice)
	}
	if ask0 <= 0 || ask0 >= 1 || ask1 <= 0 || ask1 >= 1 {
		return 0, 0, fmt.Errorf("invalid asks %.6f / %.6f", ask0, ask1)
	}
	if ask0 < minAskPrice || ask0 > maxAskPrice || ask1 < minAskPrice || ask1 > maxAskPrice {
		return 0, 0, fmt.Errorf("asks %.3f / %.3f outside configured range %.3f-%.3f", ask0, ask1, minAskPrice, maxAskPrice)
	}

	maxSum := MaxExecutablePairSum(executionFloorPercent, maxAskPrice)
	if maxSum <= 0 {
		return 0, 0, fmt.Errorf("invalid max executable pair sum %.6f", maxSum)
	}
	if ask0+ask1 > maxSum+1e-9 {
		return 0, 0, fmt.Errorf("pair ask sum %.3f exceeds execution max %.3f", ask0+ask1, maxSum)
	}

	cap0 := maxSum - ask1
	if cap0 < ask0 {
		cap0 = ask0
	}
	if cap0 > maxAskPrice {
		cap0 = maxAskPrice
	}

	cap1 := maxSum - ask0
	if cap1 < ask1 {
		cap1 = ask1
	}
	if cap1 > maxAskPrice {
		cap1 = maxAskPrice
	}

	return cap0, cap1, nil
}

func CleanupSellLimitPrice(minAskPrice float64) float64 {
	if minAskPrice <= 0 {
		return 0.01
	}
	if minAskPrice >= 1 {
		return 0.99
	}
	return minAskPrice
}
