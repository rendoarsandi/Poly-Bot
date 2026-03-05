package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
)

// PlaceOrders places multiple new limit/market orders in a single request.
func (c *CLOBClient) PlaceOrders(ctx context.Context, reqs []*OrderRequest) ([]*OrderResponse, error) {
	if len(reqs) == 0 {
		return nil, nil
	}

	payloads := make([]map[string]interface{}, 0, len(reqs))

	for _, req := range reqs {
		salt := generateSalt()
		var makerAmount, takerAmount string

		if req.Side == SideBuy {
			sizeMicro := int64(req.Size*1e6 + 0.5)
			priceMicro := int64(req.Price*1e6 + 0.5)

			if req.OrderType == OrderTypeMarket {
				sizeMicro = (sizeMicro / 10000) * 10000
				usdcMicroBig := new(big.Int).Mul(big.NewInt(priceMicro), big.NewInt(sizeMicro))
				usdcMicroBig.Div(usdcMicroBig, big.NewInt(1e6))
				usdcVal := usdcMicroBig.Int64()
				if usdcVal%100 != 0 {
					usdcVal = ((usdcVal / 100) + 1) * 100
				}
				usdcMicroBig.SetInt64(usdcVal)
				makerAmount = usdcMicroBig.String()
				takerAmount = strconv.FormatInt(sizeMicro, 10)
			} else {
				usdcMicroBig := new(big.Int).Mul(big.NewInt(priceMicro), big.NewInt(sizeMicro))
				usdcMicroBig.Div(usdcMicroBig, big.NewInt(1e6))
				makerAmount = usdcMicroBig.String()
				takerAmount = strconv.FormatInt(sizeMicro, 10)
			}
		} else {
			sizeMicro := int64(req.Size*1e6 + 0.5)
			priceMicro := int64(req.Price*1e6 + 0.5)

			if req.OrderType == OrderTypeMarket {
				sizeMicro = (sizeMicro / 10000) * 10000
				usdcMicroBig := new(big.Int).Mul(big.NewInt(priceMicro), big.NewInt(sizeMicro))
				divisor := big.NewInt(1e6)
				remainder := new(big.Int).Mod(usdcMicroBig, divisor)
				usdcMicroBig.Div(usdcMicroBig, divisor)
				if remainder.Sign() > 0 {
					usdcMicroBig.Add(usdcMicroBig, big.NewInt(1))
				}
				usdcVal := usdcMicroBig.Int64()
				if usdcVal%100 != 0 {
					usdcVal = ((usdcVal / 100) + 1) * 100
				}
				usdcMicroBig.SetInt64(usdcVal)
				makerAmount = strconv.FormatInt(sizeMicro, 10)
				takerAmount = usdcMicroBig.String()
			} else {
				usdcMicroBig := new(big.Int).Mul(big.NewInt(priceMicro), big.NewInt(sizeMicro))
				divisor := big.NewInt(1e6)
				remainder := new(big.Int).Mod(usdcMicroBig, divisor)
				usdcMicroBig.Div(usdcMicroBig, divisor)
				if remainder.Sign() > 0 {
					usdcMicroBig.Add(usdcMicroBig, big.NewInt(1))
				}
				makerAmount = strconv.FormatInt(sizeMicro, 10)
				takerAmount = usdcMicroBig.String()
			}
		}

		expirationStr := "0"
		if req.Expiration > 0 {
			expirationStr = strconv.FormatInt(req.Expiration, 10)
		}

		sideInt := 0
		if req.Side == SideSell {
			sideInt = 1
		}

		orderData := &OrderData{
			Maker:         c.signer.Address(),
			Signer:        c.signer.Address(),
			Taker:         "0x0000000000000000000000000000000000000000",
			TokenID:       req.TokenID,
			MakerAmount:   makerAmount,
			TakerAmount:   takerAmount,
			Expiration:    expirationStr,
			Nonce:         "0",
			FeeRateBps:    strconv.Itoa(req.FeeRateBps),
			Side:          sideInt,
			SignatureType: 0,
		}

		signature, err := c.signer.SignOrder(orderData)
		if err != nil {
			return nil, fmt.Errorf("failed to sign order: %w", err)
		}

		orderPayload := OrderPayload{
			Salt:          salt,
			Maker:         orderData.Maker,
			Signer:        orderData.Signer,
			Taker:         orderData.Taker,
			TokenID:       req.TokenID,
			MakerAmount:   orderData.MakerAmount,
			TakerAmount:   orderData.TakerAmount,
			Expiration:    orderData.Expiration,
			Nonce:         orderData.Nonce,
			FeeRateBps:    strconv.Itoa(req.FeeRateBps),
			Side:          string(req.Side),
			SignatureType: orderData.SignatureType,
			Signature:     signature,
		}

		payload := make(map[string]interface{})
		payload["order"] = orderPayload
		payload["owner"] = c.auth.APIKey

		if req.TimeInForce != "" {
			payload["orderType"] = string(req.TimeInForce)
		} else {
			payload["orderType"] = string(req.OrderType)
		}

		payloads = append(payloads, payload)
	}

	body, err := json.Marshal(payloads)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal orders: %w", err)
	}

	path := "/orders"
	timestamp, signature := c.auth.SignL2Request("POST", path, string(body))

	if c.testMode {
		return nil, fmt.Errorf("batch orders not fully supported in testMode")
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("POLY_API_KEY", c.auth.APIKey)
	req.Header.Set("POLY_ADDRESS", c.signer.Address())
	req.Header.Set("POLY_PASSPHRASE", c.auth.Passphrase)
	req.Header.Set("POLY_TIMESTAMP", timestamp)
	req.Header.Set("POLY_SIGNATURE", signature)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to submit batch orders: %w", err)
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		var result []OrderResponse
		if err := json.Unmarshal(bodyBytes, &result); err != nil {
			var singleResp OrderResponse
			if err := json.Unmarshal(bodyBytes, &singleResp); err == nil {
				singleResp.Success = false
				return []*OrderResponse{&singleResp}, nil
			}
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(bodyBytes))
		}
		resPtrs := make([]*OrderResponse, len(result))
		for i := range result {
			result[i].Success = false
			resPtrs[i] = &result[i]
		}
		return resPtrs, nil
	}

	var result []OrderResponse
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		var singleResp OrderResponse
		if err2 := json.Unmarshal(bodyBytes, &singleResp); err2 == nil {
			singleResp.Success = (singleResp.ErrorMsg == "")
			return []*OrderResponse{&singleResp}, nil
		}
		return nil, fmt.Errorf("failed to decode response: %w (body: %s)", err, string(bodyBytes))
	}

	resPtrs := make([]*OrderResponse, len(result))
	for i := range result {
		resPtrs[i] = &result[i]
		if resPtrs[i].ErrorMsg == "" && resPtrs[i].OrderID != "" {
			resPtrs[i].Success = true
		}
	}

	return resPtrs, nil
}
