package api

import (
	"fmt"
	"math/big"
	"strconv"
)

type EncodedOrderAmounts struct {
	MakerAmount string
	TakerAmount string
	MakerMicro  int64
	TakerMicro  int64
}

func ComputeOrderAmounts(req *OrderRequest) (EncodedOrderAmounts, error) {
	var amounts EncodedOrderAmounts
	if req == nil {
		return amounts, fmt.Errorf("nil order request")
	}
	if req.Price <= 0 || req.Price >= 1.0 {
		return amounts, fmt.Errorf("invalid order price %.6f", req.Price)
	}
	if req.Size <= 0 {
		return amounts, fmt.Errorf("invalid order size %.6f", req.Size)
	}

	sizeMicro := int64(req.Size*1e6 + 0.5)
	priceMicro := int64(req.Price*1e6 + 0.5)
	if sizeMicro <= 0 || priceMicro <= 0 {
		return amounts, fmt.Errorf("invalid encoded order size/price")
	}

	switch req.Side {
	case SideBuy:
		if usesMarketLikePrecision(req) {
			sizeMicro = (sizeMicro / 100) * 100
			if sizeMicro <= 0 {
				return amounts, fmt.Errorf("buy size rounds to zero at market precision")
			}

			usdcMicroBig := new(big.Int).Mul(big.NewInt(priceMicro), big.NewInt(sizeMicro))
			usdcMicroBig.Div(usdcMicroBig, big.NewInt(1e6))

			makerMicro := usdcMicroBig.Int64()
			if makerMicro%10000 != 0 {
				makerMicro = ((makerMicro / 10000) + 1) * 10000
			}

			amounts.MakerMicro = makerMicro
			amounts.TakerMicro = sizeMicro
		} else {
			usdcMicroBig := new(big.Int).Mul(big.NewInt(priceMicro), big.NewInt(sizeMicro))
			usdcMicroBig.Div(usdcMicroBig, big.NewInt(1e6))

			amounts.MakerMicro = usdcMicroBig.Int64()
			amounts.TakerMicro = sizeMicro
		}
	case SideSell:
		if usesMarketLikePrecision(req) {
			sizeMicro = (sizeMicro / 10000) * 10000
			if sizeMicro <= 0 {
				return amounts, fmt.Errorf("sell size rounds to zero at market precision")
			}

			usdcMicroBig := new(big.Int).Mul(big.NewInt(priceMicro), big.NewInt(sizeMicro))
			divisor := big.NewInt(1e6)
			remainder := new(big.Int).Mod(usdcMicroBig, divisor)
			usdcMicroBig.Div(usdcMicroBig, divisor)
			if remainder.Sign() > 0 {
				usdcMicroBig.Add(usdcMicroBig, big.NewInt(1))
			}

			takerMicro := usdcMicroBig.Int64()
			if takerMicro%100 != 0 {
				takerMicro = ((takerMicro / 100) + 1) * 100
			}

			amounts.MakerMicro = sizeMicro
			amounts.TakerMicro = takerMicro
		} else {
			usdcMicroBig := new(big.Int).Mul(big.NewInt(priceMicro), big.NewInt(sizeMicro))
			divisor := big.NewInt(1e6)
			remainder := new(big.Int).Mod(usdcMicroBig, divisor)
			usdcMicroBig.Div(usdcMicroBig, divisor)
			if remainder.Sign() > 0 {
				usdcMicroBig.Add(usdcMicroBig, big.NewInt(1))
			}

			amounts.MakerMicro = sizeMicro
			amounts.TakerMicro = usdcMicroBig.Int64()
		}
	default:
		return amounts, fmt.Errorf("unsupported order side %q", req.Side)
	}

	amounts.MakerAmount = strconv.FormatInt(amounts.MakerMicro, 10)
	amounts.TakerAmount = strconv.FormatInt(amounts.TakerMicro, 10)
	return amounts, nil
}
