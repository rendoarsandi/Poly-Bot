package strategy

import (
	"fmt"
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
