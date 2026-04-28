package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

func normalizeBatchOrderResponse(resp *OrderResponse) {
	if resp == nil {
		return
	}
	status := strings.ToUpper(strings.TrimSpace(resp.Status))
	errMsg := strings.TrimSpace(resp.ErrorMsg)
	switch status {
	case "KILLED", "CANCELLED", "EXPIRED", "REJECTED":
		resp.Success = false
		if resp.ErrorMsg == "" {
			resp.ErrorMsg = fmt.Sprintf("Order was %s", status)
		}
		return
	}
	if errMsg != "" && strings.Contains(strings.ToLower(errMsg), "error") {
		resp.Success = false
		if resp.ErrorMsg == "" {
			resp.ErrorMsg = errMsg
		}
		return
	}
	if !resp.Success && resp.ErrorMsg == "" && resp.OrderID != "" {
		resp.Success = true
	}
}

// PlaceOrders places multiple new limit/market orders in a single request.
func (c *CLOBClient) PlaceOrders(ctx context.Context, reqs []*OrderRequest) ([]*OrderResponse, error) {
	if len(reqs) == 0 {
		return nil, nil
	}

	type preparedBatchPayload struct {
		payload   map[string]interface{}
		signMs    int64
		computeMs int64
	}

	prepared := make([]preparedBatchPayload, len(reqs))
	prepStart := time.Now()
	var wg sync.WaitGroup
	errCh := make(chan error, len(reqs))

	for idx, req := range reqs {
		idx, req := idx, req
		wg.Add(1)
		go func() {
			defer wg.Done()
			computeStart := time.Now()
			salt := generateSalt()
			amounts, err := ComputeOrderAmounts(req)
			if err != nil {
				errCh <- err
				return
			}
			computeMs := time.Since(computeStart).Milliseconds()

			expirationStr := "0"
			if req.Expiration > 0 {
				expirationStr = strconv.FormatInt(req.Expiration, 10)
			}

			sideInt := 0
			if req.Side == SideSell {
				sideInt = 1
			}

			verifyingContract, err := c.getExchangeVerifyingContract(ctx, req.TokenID)
			if err != nil {
				errCh <- err
				return
			}

			orderData := &OrderData{
				Salt:              strconv.FormatInt(salt, 10),
				Maker:             c.signer.Address(),
				Signer:            c.signer.Address(),
				TokenID:           req.TokenID,
				MakerAmount:       amounts.MakerAmount,
				TakerAmount:       amounts.TakerAmount,
				Timestamp:         strconv.FormatInt(time.Now().UnixMilli(), 10),
				Expiration:        expirationStr,
				Metadata:          zeroBytes32,
				Builder:           zeroBytes32,
				VerifyingContract: verifyingContract,
				Side:              sideInt,
				SignatureType:     0,
			}

			signStart := time.Now()
			signature, err := c.signer.SignOrder(orderData)
			if err != nil {
				errCh <- fmt.Errorf("failed to sign order: %w", err)
				return
			}
			signMs := time.Since(signStart).Milliseconds()

			orderPayload := OrderPayload{
				Salt:          salt,
				Maker:         orderData.Maker,
				Signer:        orderData.Signer,
				TokenID:       req.TokenID,
				MakerAmount:   orderData.MakerAmount,
				TakerAmount:   orderData.TakerAmount,
				Side:          string(req.Side),
				SignatureType: orderData.SignatureType,
				Timestamp:     orderData.Timestamp,
				Expiration:    orderData.Expiration,
				Metadata:      orderData.Metadata,
				Builder:       orderData.Builder,
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

			prepared[idx] = preparedBatchPayload{
				payload:   payload,
				signMs:    signMs,
				computeMs: computeMs,
			}
		}()
	}

	wg.Wait()
	close(errCh)
	if err, ok := <-errCh; ok {
		return nil, err
	}

	payloads := make([]map[string]interface{}, 0, len(prepared))
	latencyMs := c.newLatencyMetrics()
	for _, item := range prepared {
		payloads = append(payloads, item.payload)
		if latencyMs != nil {
			latencyMs["prep_compute_ms"] += item.computeMs
			latencyMs["sign_ms"] += item.signMs
		}
	}
	captureLatency(latencyMs, "prep_wall_ms", prepStart)

	marshalStart := time.Now()
	body, err := json.Marshal(payloads)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal orders: %w", err)
	}
	captureLatency(latencyMs, "marshal_ms", marshalStart)

	path := "/orders"

	if c.testMode {
		return nil, fmt.Errorf("batch orders not fully supported in testMode")
	}

	authStart := time.Now()
	timestamp, signature := c.auth.SignL2Request("POST", path, string(body))
	captureLatency(latencyMs, "auth_ms", authStart)
	postStart := time.Now()
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
	captureLatency(latencyMs, "post_ms", postStart)
	if err != nil {
		c.logRawLatencyDebug(path, latencyMs, "submit_error")
		return nil, fmt.Errorf("failed to submit batch orders: %w", err)
	}
	readStart := time.Now()
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	captureLatency(latencyMs, "read_ms", readStart)
	statusCode := resp.StatusCode

	if statusCode != http.StatusOK && statusCode != http.StatusCreated {
		c.logRawLatencyDebug(path, latencyMs, "http_error")
		var result []OrderResponse
		if err := json.Unmarshal(bodyBytes, &result); err != nil {
			var singleResp OrderResponse
			if err := json.Unmarshal(bodyBytes, &singleResp); err == nil {
				singleResp.Success = false
				return []*OrderResponse{&singleResp}, nil
			}
			return nil, fmt.Errorf("HTTP %d: %s", statusCode, string(bodyBytes))
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
		c.logRawLatencyDebug(path, latencyMs, "decode_error")
		var singleResp OrderResponse
		if err2 := json.Unmarshal(bodyBytes, &singleResp); err2 == nil {
			normalizeBatchOrderResponse(&singleResp)
			return []*OrderResponse{&singleResp}, nil
		}
		return nil, fmt.Errorf("failed to decode response: %w (body: %s)", err, string(bodyBytes))
	}

	resPtrs := make([]*OrderResponse, len(result))
	outcome := "success"
	for i := range result {
		resPtrs[i] = &result[i]
		normalizeBatchOrderResponse(resPtrs[i])
		if !resPtrs[i].Success {
			outcome = "order_unsuccessful"
		}
	}
	c.logRawLatencyDebug(path, latencyMs, outcome)

	return resPtrs, nil
}
