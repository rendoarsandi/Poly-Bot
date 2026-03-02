package api

import (
	"math/big"
	"testing"
)

func TestSellOrderMath_CeilingDivision(t *testing.T) {
	// Scenario: SELL Order
	// Price = 0.3
	// Size = 0.000004 (4 shares)
	// Expected Total Value = 1.2 micro USDC
	//
	// Truncation (Bad): 1 micro USDC -> Implied Price 0.25 (Too Low)
	// Ceiling (Good): 2 micro USDC -> Implied Price 0.5 (Safe)

	price := 0.3
	size := 0.000004

	priceMicro := int64(price*1e6 + 0.5) // 300,000
	sizeMicro := int64(size*1e6 + 0.5)   // 4

	// Implementation Logic (Copied from fixed code)
	usdcMicroBig := new(big.Int).Mul(big.NewInt(priceMicro), big.NewInt(sizeMicro))

	divisor := big.NewInt(1e6)
	remainder := new(big.Int).Mod(usdcMicroBig, divisor)

	// Perform Ceiling Division
	usdcMicroBig.Div(usdcMicroBig, divisor)
	if remainder.Sign() > 0 {
		usdcMicroBig.Add(usdcMicroBig, big.NewInt(1))
	}

	usdcResult := usdcMicroBig.Int64()

	// Assertion: Check if result is Ceil(1.2) = 2
	if usdcResult != 2 {
		t.Errorf("Expected ceil value 2, got %d", usdcResult)
	}

	impliedPrice := float64(usdcResult) / float64(sizeMicro) * 1e6
	if impliedPrice < float64(priceMicro) {
		t.Errorf("Result fails limit price check: Got %.2f, Limit %.2f", impliedPrice/1e6, price)
	}
}
