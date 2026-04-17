package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"Market-bot/internal/core"
)

// CLOBClient handles authenticated trading operations on Polymarket CLOB
type CLOBClient struct {
	BaseURL   string
	signer    *Signer
	auth      *APIAuth
	testMode  bool
	rawLogger *rawAPILogger
}

// NewCLOBClient creates a new authenticated CLOB client
func NewCLOBClient(privateKeyHex, apiKey, apiSecret, apiPassphrase string) (*CLOBClient, error) {
	signer, err := NewSigner(privateKeyHex)
	if err != nil {
		return nil, fmt.Errorf("failed to create signer: %w", err)
	}

	return &CLOBClient{
		BaseURL:  "https://clob.polymarket.com",
		signer:   signer,
		auth:     NewAPIAuth(apiKey, apiSecret, apiPassphrase),
		testMode: false,
	}, nil
}

// NewReadOnlyCLOBClient creates an unauthenticated CLOB client for public endpoints
func NewReadOnlyCLOBClient() *CLOBClient {
	return &CLOBClient{
		BaseURL:  "https://clob.polymarket.com",
		testMode: false,
	}
}

// SetTestMode enables/disables test mode (validate orders but don't submit)
func (c *CLOBClient) SetTestMode(enabled bool) {
	c.testMode = enabled
}

// IsTestMode returns whether test mode is enabled
func (c *CLOBClient) IsTestMode() bool {
	return c.testMode
}

func (c *CLOBClient) EnableRawAPILog(path string) error {
	logger, err := newRawAPILogger(path)
	if err != nil {
		return err
	}
	if c.rawLogger != nil {
		_ = c.rawLogger.Close()
	}
	c.rawLogger = logger
	return nil
}

func (c *CLOBClient) CloseRawAPILog() error {
	if c.rawLogger == nil {
		return nil
	}
	err := c.rawLogger.Close()
	c.rawLogger = nil
	return err
}

// Address returns the wallet address
func (c *CLOBClient) Address() string {
	return c.signer.Address()
}

// GetSigner returns the internal signer
func (c *CLOBClient) GetSigner() *Signer {
	return c.signer
}

// Side represents order side
type Side string

const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

// OrderType represents order type
type OrderType string

const (
	OrderTypeLimit  OrderType = "LIMIT"
	OrderTypeMarket OrderType = "MARKET"
)

// TimeInForce represents time in force
type TimeInForce string

const (
	TIFGoodTilCancelled TimeInForce = "GTC"
	TIFFillOrKill       TimeInForce = "FOK"
	TIFFillAndKill      TimeInForce = "FAK"
)

func usesMarketLikePrecision(req *OrderRequest) bool {
	if req == nil {
		return false
	}
	if req.OrderType == OrderTypeMarket {
		return true
	}
	switch req.TimeInForce {
	case TIFFillAndKill, TIFFillOrKill:
		return true
	default:
		return false
	}
}

// OrderRequest represents a new order request
type OrderRequest struct {
	TokenID     string      `json:"tokenID"`
	Outcome     string      `json:"outcome,omitempty"`
	Price       float64     `json:"price"`
	Size        float64     `json:"size"`
	Side        Side        `json:"side"`
	OrderType   OrderType   `json:"type,omitempty"`
	TimeInForce TimeInForce `json:"timeInForce,omitempty"`
	Expiration  int64       `json:"expiration,omitempty"`
	FeeRateBps  int         `json:"feeRateBps,omitempty"`
}

// SignedOrder represents a signed order ready for submission
type SignedOrder struct {
	Order     OrderPayload `json:"order"`
	Owner     string       `json:"owner"`
	OrderType string       `json:"orderType"`
}

// OrderPayload is the order data to be signed
type OrderPayload struct {
	Salt          int64  `json:"salt"`
	Maker         string `json:"maker"`
	Signer        string `json:"signer"`
	Taker         string `json:"taker"`
	TokenID       string `json:"tokenId"`
	MakerAmount   string `json:"makerAmount"`
	TakerAmount   string `json:"takerAmount"`
	Expiration    string `json:"expiration"`
	Nonce         string `json:"nonce"`
	FeeRateBps    string `json:"feeRateBps"`
	Side          string `json:"side"` // API expects string "0" or "1"
	SignatureType int    `json:"signatureType"`
	Signature     string `json:"signature"`
}

// OrderResponse represents the API response for order placement
type OrderResponse struct {
	OrderID            string   `json:"orderID"`
	Status             string   `json:"status"`
	ErrorMsg           string   `json:"errorMsg,omitempty"`
	Success            bool     `json:"success"`
	MakingAmount       string   `json:"makingAmount,omitempty"`
	TakingAmount       string   `json:"takingAmount,omitempty"`
	TransactionsHashes []string `json:"transactionsHashes,omitempty"`
	TradeIDs           []string `json:"tradeIDs,omitempty"`
}

// UnmarshalJSON accepts the documented Polymarket response schema plus a few
// observed field-name variants so single and batch order decoding stay robust.
func (r *OrderResponse) UnmarshalJSON(data []byte) error {
	type rawOrderResponse OrderResponse
	var aux struct {
		rawOrderResponse
		OrderIDAlt            string   `json:"orderId"`
		ErrorMsgAlt           string   `json:"errorMsg"`
		ErrorAlt              string   `json:"error"`
		TransactionsHashesAlt []string `json:"transactionHashes"`
		TradeIDsAlt           []string `json:"tradeIds"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*r = OrderResponse(aux.rawOrderResponse)
	if r.OrderID == "" {
		r.OrderID = aux.OrderIDAlt
	}
	if r.ErrorMsg == "" {
		switch {
		case aux.ErrorMsgAlt != "":
			r.ErrorMsg = aux.ErrorMsgAlt
		case aux.ErrorAlt != "":
			r.ErrorMsg = aux.ErrorAlt
		}
	}
	if len(r.TransactionsHashes) == 0 && len(aux.TransactionsHashesAlt) > 0 {
		r.TransactionsHashes = aux.TransactionsHashesAlt
	}
	if len(r.TradeIDs) == 0 && len(aux.TradeIDsAlt) > 0 {
		r.TradeIDs = aux.TradeIDsAlt
	}
	return nil
}

// HeartbeatResponse represents the authenticated open-order heartbeat response.
type HeartbeatResponse struct {
	Status string `json:"status"`
}

// PlaceOrder places a new limit order
func (c *CLOBClient) PlaceOrder(ctx context.Context, req *OrderRequest) (*OrderResponse, error) {
	// Generate random salt
	salt := generateSalt()

	// Calculate amounts based on order side
	// BUY and SELL have different decimal precision requirements per Polymarket API
	var makerAmount, takerAmount string

	if req.Side == SideBuy {
		// BUY: makerAmount = USDC (what we pay), takerAmount = shares (what we receive)

		sizeMicro := int64(req.Size*1e6 + 0.5)
		priceMicro := int64(req.Price*1e6 + 0.5)

		if usesMarketLikePrecision(req) {
			// Market Buy Restrictions (per API error):
			// - Maker (USDC): Max 2 decimals (multiple of 10000 units)
			// - Taker (Shares): Max 4 decimals (multiple of 100 units)

			// Truncate size (taker) to 4 decimals
			sizeMicro = (sizeMicro / 100) * 100

			// Calculate USDC cost with truncated size
			usdcMicroBig := new(big.Int).Mul(big.NewInt(priceMicro), big.NewInt(sizeMicro))
			usdcMicroBig.Div(usdcMicroBig, big.NewInt(1e6))

			// Round up USDC (maker) to nearest 2 decimals (multiple of 10000 units)
			// to ensure implied price remains >= limit price
			usdcVal := usdcMicroBig.Int64()
			if usdcVal%10000 != 0 {
				usdcVal = ((usdcVal / 10000) + 1) * 10000
			}
			usdcMicroBig.SetInt64(usdcVal)

			makerAmount = usdcMicroBig.String()
			takerAmount = strconv.FormatInt(sizeMicro, 10)
		} else {
			// Limit Buy: Supports full 6 decimal precision
			usdcMicroBig := new(big.Int).Mul(big.NewInt(priceMicro), big.NewInt(sizeMicro))
			usdcMicroBig.Div(usdcMicroBig, big.NewInt(1e6))

			makerAmount = usdcMicroBig.String()
			takerAmount = strconv.FormatInt(sizeMicro, 10)
		}

		// Debug log removed for production
	} else {
		// SELL: makerAmount = shares (what we give), takerAmount = USDC (what we receive)
		// This ensures the API computes price correctly as takerAmount/makerAmount = USDC/shares = Price
		sizeMicro := int64(req.Size*1e6 + 0.5)
		priceMicro := int64(req.Price*1e6 + 0.5)

		if usesMarketLikePrecision(req) {
			// Market Sell Restrictions (per API error):
			// - Maker (Shares): Max 2 decimals (multiple of 10,000 units)
			// - Taker (USDC): Max 4 decimals (multiple of 100 units)

			// Truncate size (Shares) to 2 decimals
			sizeMicro = (sizeMicro / 10000) * 10000

			usdcMicroBig := new(big.Int).Mul(big.NewInt(priceMicro), big.NewInt(sizeMicro))

			divisor := big.NewInt(1e6)
			remainder := new(big.Int).Mod(usdcMicroBig, divisor)
			usdcMicroBig.Div(usdcMicroBig, divisor)
			if remainder.Sign() > 0 {
				usdcMicroBig.Add(usdcMicroBig, big.NewInt(1))
			}

			// Round USDC up to nearest 4 decimals (multiple of 100 units)
			// to ensure implied price remains >= limit price
			usdcVal := usdcMicroBig.Int64()
			if usdcVal%100 != 0 {
				usdcVal = ((usdcVal / 100) + 1) * 100
			}
			usdcMicroBig.SetInt64(usdcVal)

			makerAmount = strconv.FormatInt(sizeMicro, 10)
			takerAmount = usdcMicroBig.String()
		} else {
			usdcMicroBig := new(big.Int).Mul(big.NewInt(priceMicro), big.NewInt(sizeMicro))

			// Use Ceiling division for SELL orders to ensure implied price >= limit price.
			// Truncation (floor) can result in implied price < limit price, causing API rejection.
			divisor := big.NewInt(1e6)
			remainder := new(big.Int).Mod(usdcMicroBig, divisor)
			usdcMicroBig.Div(usdcMicroBig, divisor)
			if remainder.Sign() > 0 {
				usdcMicroBig.Add(usdcMicroBig, big.NewInt(1))
			}

			// Correct assignment: makerAmount = shares, takerAmount = USDC
			makerAmount = strconv.FormatInt(sizeMicro, 10)
			takerAmount = usdcMicroBig.String()
		}

		// Debug log removed for production
	}

	// Polymarket rejects non-zero expiration for non-GTD orders.
	// We only send a non-zero expiration when explicitly provided.
	expirationStr := "0"
	if req.Expiration > 0 {
		expirationStr = strconv.FormatInt(req.Expiration, 10)
	}

	submitSigned := func(salt int64, makerAmt, takerAmt string, sideInt int) (*OrderResponse, error) {
		orderData := &OrderData{
			Salt:          strconv.FormatInt(salt, 10),
			Maker:         c.signer.Address(),
			Signer:        c.signer.Address(),
			Taker:         "0x0000000000000000000000000000000000000000", // Any taker
			TokenID:       req.TokenID,
			MakerAmount:   makerAmt,
			TakerAmount:   takerAmt,
			Expiration:    expirationStr,
			Nonce:         "0",
			FeeRateBps:    strconv.Itoa(req.FeeRateBps),
			Side:          sideInt,
			SignatureType: 0, // EOA signature
		}

		signStart := time.Now()
		signature, err := c.signer.SignOrder(orderData)
		if err != nil {
			return nil, fmt.Errorf("failed to sign order: %w", err)
		}

		signedOrder := &SignedOrder{
			Order: OrderPayload{
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
				Side:          string(req.Side), // Send "BUY" or "SELL"
				SignatureType: orderData.SignatureType,
				Signature:     signature,
			},
			Owner:     c.auth.APIKey,
			OrderType: string(req.OrderType),
		}

		latencyMs := c.newLatencyMetrics()
		if latencyMs != nil {
			latencyMs["sign_ms"] = time.Since(signStart).Milliseconds()
		}
		return c.submitOrder(ctx, signedOrder, req.TimeInForce, req.Price, req.Side, latencyMs)
	}

	sideInt := 0
	if req.Side == SideSell {
		sideInt = 1
	}

	resp, err := submitSigned(salt, makerAmount, takerAmount, sideInt)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// submitOrder sends the signed order to the CLOB API
func (c *CLOBClient) submitOrder(ctx context.Context, signedOrder *SignedOrder, tif TimeInForce, price float64, side Side, latencyMs map[string]int64) (*OrderResponse, error) {
	// Build the payload (needed for both test mode validation and real submission)
	payload := make(map[string]interface{})

	payload["order"] = signedOrder.Order
	payload["owner"] = c.auth.APIKey

	// Polymarket's orderType field at the top level
	if tif != "" {
		payload["orderType"] = string(tif)
	} else {
		payload["orderType"] = signedOrder.OrderType
	}

	// Match official CLOB client payload shape: signed order + owner + orderType.
	// (No top-level side/price fields.)

	marshalStart := time.Now()
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal order: %w", err)
	}
	captureLatency(latencyMs, "marshal_ms", marshalStart)

	path := "/order"

	// In test mode: validate everything but don't actually submit
	if c.testMode {
		authStart := time.Now()
		timestamp, signature := c.auth.SignL2Request("POST", path, string(body))
		captureLatency(latencyMs, "auth_ms", authStart)
		// Verify we can build the request (validates auth setup)
		req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+path, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("test mode: failed to build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("POLY_API_KEY", c.auth.APIKey)
		req.Header.Set("POLY_ADDRESS", c.signer.Address())
		req.Header.Set("POLY_PASSPHRASE", c.auth.Passphrase)
		req.Header.Set("POLY_TIMESTAMP", timestamp)
		req.Header.Set("POLY_SIGNATURE", signature)

		// Validate balance is sufficient by checking allowance
		allowance, err := c.GetBalanceAllowance(ctx)
		if err != nil {
			return &OrderResponse{
				OrderID:  fmt.Sprintf("test-%d", time.Now().UnixNano()),
				Success:  false,
				ErrorMsg: fmt.Sprintf("test mode: balance check failed: %v", err),
			}, nil
		}

		// Parse maker amount to check against balance
		makerAmount, _ := strconv.ParseFloat(signedOrder.Order.MakerAmount, 64)
		makerAmountUSDC := makerAmount / 1e6 // Convert from base units

		if signedOrder.Order.Side == "0" && (allowance.Balance < makerAmountUSDC || allowance.Allowance < makerAmountUSDC) {
			return &OrderResponse{
				OrderID:  fmt.Sprintf("test-%d", time.Now().UnixNano()),
				Success:  false,
				ErrorMsg: fmt.Sprintf("test mode: insufficient balance or allowance (Balance: $%.2f, Allowance: $%.2f < $%.2f needed)", allowance.Balance, allowance.Allowance, makerAmountUSDC),
			}, nil
		}

		// All validations passed
		return &OrderResponse{
			OrderID:  fmt.Sprintf("test-%d", time.Now().UnixNano()),
			Success:  true,
			ErrorMsg: fmt.Sprintf("test mode: order validated (signed, balance OK: $%.2f)", allowance.Balance),
		}, nil
	}

	// Real submission helper
	doSubmit := func(body []byte) (int, []byte, error) {
		authStart := time.Now()
		timestamp, signature := c.auth.SignL2Request("POST", path, string(body))
		captureLatency(latencyMs, "auth_ms", authStart)
		req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+path, bytes.NewReader(body))
		if err != nil {
			return 0, nil, err
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("POLY_API_KEY", c.auth.APIKey)
		req.Header.Set("POLY_ADDRESS", c.signer.Address())
		req.Header.Set("POLY_PASSPHRASE", c.auth.Passphrase)
		req.Header.Set("POLY_TIMESTAMP", timestamp)
		req.Header.Set("POLY_SIGNATURE", signature)

		postStart := time.Now()
		resp, err := httpClient.Do(req)
		captureLatency(latencyMs, "post_ms", postStart)
		if err != nil {
			return 0, nil, fmt.Errorf("failed to submit order: %w", err)
		}
		defer resp.Body.Close()
		readStart := time.Now()
		bodyBytes, _ := io.ReadAll(resp.Body)
		captureLatency(latencyMs, "read_ms", readStart)
		return resp.StatusCode, bodyBytes, nil
	}

	submitStart := time.Now()
	statusCode, bodyBytes, err := doSubmit(body)
	captureLatency(latencyMs, "submit_ms", submitStart)
	if err != nil {
		c.logRawOrderDebug("POST", path, body, nil, 0, err, "submit_error")
		c.logRawLatencyDebug(path, latencyMs, "submit_error")
		return nil, err
	}

	if statusCode != http.StatusOK && statusCode != http.StatusCreated {
		c.logRawOrderDebug("POST", path, body, bodyBytes, statusCode, nil, "http_error")
		c.logRawLatencyDebug(path, latencyMs, "http_error")
		// Suppress raw HTTP logging in TUI mode to avoid breaking the UI layout
		// The error will be passed back in the ErrorMsg field and logged cleanly by the TUI.
		// log.Printf("[CLOB] API error: HTTP %d | Body: %s", statusCode, string(bodyBytes))

		var result OrderResponse
		if err := json.Unmarshal(bodyBytes, &result); err != nil {
			return &OrderResponse{
				Success:  false,
				ErrorMsg: fmt.Sprintf("HTTP %d: %s", statusCode, string(bodyBytes)),
			}, nil
		}
		result.Success = false
		return &result, nil
	}

	var result OrderResponse
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		c.logRawOrderDebug("POST", path, body, bodyBytes, statusCode, err, "decode_error")
		c.logRawLatencyDebug(path, latencyMs, "decode_error")
		return &OrderResponse{
			Success:  true,
			ErrorMsg: fmt.Sprintf("Success but decode failed: %v", err),
		}, nil
	}
	// CRITICAL: Trust the API's success field initially, but override if Status indicates failure.
	// FOK/FAK orders can return success=true (request accepted) but status="KILLED" (execution failed).
	if result.Success {
		switch result.Status {
		case "KILLED", "CANCELLED", "EXPIRED", "REJECTED":
			result.Success = false
			if result.ErrorMsg == "" {
				result.ErrorMsg = fmt.Sprintf("Order was %s", result.Status)
			}
		}
	}
	if !result.Success {
		outcome := result.Status
		if outcome == "" {
			outcome = "order_unsuccessful"
		}
		c.logRawOrderDebug("POST", path, body, bodyBytes, statusCode, nil, outcome)
		c.logRawLatencyDebug(path, latencyMs, outcome)
		return &result, nil
	}
	c.logRawLatencyDebug(path, latencyMs, "success")

	return &result, nil
}

func (c *CLOBClient) logRawOrderDebug(method, path string, requestBody, responseBody []byte, statusCode int, err error, outcome string) {
	if c.rawLogger == nil {
		return
	}
	entry := rawAPILogEntry{
		Source:       "clob",
		Method:       method,
		Path:         path,
		StatusCode:   statusCode,
		RequestBody:  string(requestBody),
		ResponseBody: string(responseBody),
		Outcome:      outcome,
	}
	if err != nil {
		entry.Error = err.Error()
	}
	c.rawLogger.Log(entry)
}

func (c *CLOBClient) logRawLatencyDebug(path string, latencyMs map[string]int64, outcome string) {
	if c.rawLogger == nil || len(latencyMs) == 0 {
		return
	}
	c.rawLogger.Log(rawAPILogEntry{
		Source:    "clob",
		Method:    "LATENCY",
		Path:      path,
		Outcome:   outcome,
		LatencyMs: latencyMs,
	})
}

func (c *CLOBClient) newLatencyMetrics() map[string]int64 {
	if c.rawLogger == nil {
		return nil
	}
	return make(map[string]int64, 6)
}

func captureLatency(latencyMs map[string]int64, key string, start time.Time) {
	if latencyMs == nil {
		return
	}
	latencyMs[key] = time.Since(start).Milliseconds()
}

// CancelOrder cancels an existing order
func (c *CLOBClient) CancelOrder(ctx context.Context, orderID string) error {
	if c.testMode {
		return nil
	}

	path := "/order/" + orderID
	timestamp, signature := c.auth.SignL2Request("DELETE", path, "")

	req, err := http.NewRequestWithContext(ctx, "DELETE", c.BaseURL+path, nil)
	if err != nil {
		return err
	}

	req.Header.Set("POLY_API_KEY", c.auth.APIKey)
	req.Header.Set("POLY_ADDRESS", c.signer.Address())
	req.Header.Set("POLY_PASSPHRASE", c.auth.Passphrase)
	req.Header.Set("POLY_TIMESTAMP", timestamp)
	req.Header.Set("POLY_SIGNATURE", signature)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to cancel order: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("cancel order failed with status %d", resp.StatusCode)
	}

	return nil
}

// CancelAllOrders cancels all open orders
func (c *CLOBClient) CancelAllOrders(ctx context.Context) error {
	if c.testMode {
		return nil
	}

	path := "/orders"
	timestamp, signature := c.auth.SignL2Request("DELETE", path, "")

	req, err := http.NewRequestWithContext(ctx, "DELETE", c.BaseURL+path, nil)
	if err != nil {
		return err
	}

	req.Header.Set("POLY_API_KEY", c.auth.APIKey)
	req.Header.Set("POLY_ADDRESS", c.signer.Address())
	req.Header.Set("POLY_PASSPHRASE", c.auth.Passphrase)
	req.Header.Set("POLY_TIMESTAMP", timestamp)
	req.Header.Set("POLY_SIGNATURE", signature)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to cancel all orders: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("cancel all orders failed with status %d", resp.StatusCode)
	}

	return nil
}

// SendHeartbeat keeps authenticated order sessions alive for open orders.
func (c *CLOBClient) SendHeartbeat(ctx context.Context) (*HeartbeatResponse, error) {
	if c.testMode {
		return &HeartbeatResponse{Status: "ok"}, nil
	}

	path := "/heartbeats"
	timestamp, signature := c.auth.SignL2Request("POST", path, "")

	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("POLY_API_KEY", c.auth.APIKey)
	req.Header.Set("POLY_ADDRESS", c.signer.Address())
	req.Header.Set("POLY_PASSPHRASE", c.auth.Passphrase)
	req.Header.Set("POLY_TIMESTAMP", timestamp)
	req.Header.Set("POLY_SIGNATURE", signature)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send heartbeat: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("send heartbeat failed with status %d: %s", resp.StatusCode, string(body))
	}

	var result HeartbeatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil && err != io.EOF {
		return nil, fmt.Errorf("failed to decode heartbeat response: %w", err)
	}
	if result.Status == "" {
		result.Status = "ok"
	}

	return &result, nil
}

// OpenOrder represents an open order
type OpenOrder struct {
	OrderID       string  `json:"orderID"`
	TokenID       string  `json:"tokenID"`
	Side          string  `json:"side"`
	Price         float64 `json:"price"`
	OriginalSize  float64 `json:"originalSize"`
	RemainingSize float64 `json:"remainingSize"`
	Status        string  `json:"status"`
	CreatedAt     string  `json:"createdAt"`
}

// GetOpenOrders retrieves all open orders
func (c *CLOBClient) GetOpenOrders(ctx context.Context) ([]OpenOrder, error) {
	path := "/orders"
	timestamp, signature := c.auth.SignL2Request("GET", path, "")

	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("POLY_API_KEY", c.auth.APIKey)
	req.Header.Set("POLY_ADDRESS", c.signer.Address())
	req.Header.Set("POLY_PASSPHRASE", c.auth.Passphrase)
	req.Header.Set("POLY_TIMESTAMP", timestamp)
	req.Header.Set("POLY_SIGNATURE", signature)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get open orders: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get open orders failed with status %d", resp.StatusCode)
	}

	var orders []OpenOrder
	if err := json.NewDecoder(resp.Body).Decode(&orders); err != nil {
		return nil, fmt.Errorf("failed to decode orders: %w", err)
	}

	return orders, nil
}

// GetOrder retrieves a single order by ID
func (c *CLOBClient) GetOrder(ctx context.Context, orderID string) (*OpenOrder, error) {
	path := "/order/" + orderID
	timestamp, signature := c.auth.SignL2Request("GET", path, "")

	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("POLY_API_KEY", c.auth.APIKey)
	req.Header.Set("POLY_ADDRESS", c.signer.Address())
	req.Header.Set("POLY_PASSPHRASE", c.auth.Passphrase)
	req.Header.Set("POLY_TIMESTAMP", timestamp)
	req.Header.Set("POLY_SIGNATURE", signature)

	resp, err := httpClient.Do(req)
	if err != nil {
		c.logRawOrderDebug("GET", path, nil, nil, 0, err, "get_order_error")
		return nil, fmt.Errorf("failed to get order: %w", err)
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		// Do NOT treat a missing order as FILLED. FAK/FOK orders can disappear after
		// being killed or partially matched, so callers must rely on explicit fill
		// signals (WS/position deltas) rather than a phantom 404 fill assumption.
		return &OpenOrder{OrderID: orderID, Status: "NOT_FOUND"}, nil
	}

	if resp.StatusCode != http.StatusOK {
		c.logRawOrderDebug("GET", path, nil, bodyBytes, resp.StatusCode, nil, "get_order_http_error")
		return nil, fmt.Errorf("get order failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	var order OpenOrder
	if err := json.Unmarshal(bodyBytes, &order); err != nil {
		c.logRawOrderDebug("GET", path, nil, bodyBytes, resp.StatusCode, err, "get_order_decode_error")
		return nil, fmt.Errorf("failed to decode order: %w", err)
	}
	if order.Status == "FAILED" || order.Status == "CANCELLED" || order.Status == "EXPIRED" || order.Status == "REJECTED" {
		c.logRawOrderDebug("GET", path, nil, bodyBytes, resp.StatusCode, nil, "order_"+strings.ToLower(order.Status))
	}

	return &order, nil
}

// WaitForFill waits for an order to be filled or times out
// Returns true if filled, false if timed out or cancelled
func (c *CLOBClient) WaitForFill(ctx context.Context, orderID string, timeout time.Duration) (bool, error) {
	if c.testMode {
		return true, nil // Dry run always "fills"
	}

	deadline := time.Now().Add(timeout)
	checkInterval := 100 * time.Millisecond

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		default:
		}

		order, err := c.GetOrder(ctx, orderID)
		if err != nil {
			// On error, wait and retry
			time.Sleep(checkInterval)
			continue
		}

		// Check status
		switch strings.ToUpper(strings.TrimSpace(order.Status)) {
		case "FILLED", "CONFIRMED":
			return true, nil
		case "NOT_FOUND":
			return false, nil
		case "FAILED", "CANCELLED", "EXPIRED":
			return false, nil
		case "MATCHED", "MINED", "RETRYING", "DELAYED", "UNMATCHED":
			// Trade/order is still in flight; keep polling until terminal.
		case "OPEN", "LIVE":
			// Check if fully filled (remainingSize == 0)
			if order.RemainingSize == 0 && order.OriginalSize > 0 {
				return true, nil
			}
		}

		time.Sleep(checkInterval)
	}

	return false, nil // Timed out
}

// Position represents a position in a market
type Position struct {
	TokenID         string  `json:"asset"`
	Size            float64 `json:"size"`
	AvgPrice        float64 `json:"avgPrice"`
	Redeemable      bool    `json:"redeemable"`
	Mergeable       bool    `json:"mergeable"`
	Outcome         string  `json:"outcome"`
	ConditionID     string  `json:"conditionId"`
	Title           string  `json:"title"`
	Slug            string  `json:"slug"`
	EventSlug       string  `json:"eventSlug"`
	Icon            string  `json:"icon"`
	EndDate         string  `json:"endDate"`
	OppositeOutcome string  `json:"oppositeOutcome"`
	OppositeAsset   string  `json:"oppositeAsset"`
}

// BalanceAllowance represents USDC balance and allowance info
type BalanceAllowance struct {
	Balance   float64 `json:"balance,string"`
	Allowance float64 `json:"allowance,string"`
}

const usdcBaseUnitsPerToken = 1e6

// GetBalanceAllowance retrieves USDC balance and allowance from CLOB
func (c *CLOBClient) GetBalanceAllowance(ctx context.Context) (*BalanceAllowance, error) {
	path := "/balance-allowance?asset_type=COLLATERAL"
	timestamp, signature := c.auth.SignL2Request("GET", path, "")

	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("POLY_API_KEY", c.auth.APIKey)
	req.Header.Set("POLY_ADDRESS", c.signer.Address())
	req.Header.Set("POLY_PASSPHRASE", c.auth.Passphrase)
	req.Header.Set("POLY_TIMESTAMP", timestamp)
	req.Header.Set("POLY_SIGNATURE", signature)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get balance: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get balance failed with status %d", resp.StatusCode)
	}

	var result BalanceAllowance
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode balance: %w", err)
	}
	// Polymarket reports collateral amounts in 6-decimal base units.
	result.Balance /= usdcBaseUnitsPerToken
	result.Allowance /= usdcBaseUnitsPerToken

	return &result, nil
}

// UpdateBalanceAllowance syncs the CLOB's cached view of on-chain allowance.
// Must be called after on-chain approve to enable trading.
func (c *CLOBClient) UpdateBalanceAllowance(ctx context.Context) error {
	path := "/balance-allowance/update?asset_type=COLLATERAL&signature_type=0"
	timestamp, signature := c.auth.SignL2Request("GET", path, "")

	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+path, nil)
	if err != nil {
		return fmt.Errorf("failed to build update-balance-allowance request: %w", err)
	}

	req.Header.Set("POLY_API_KEY", c.auth.APIKey)
	req.Header.Set("POLY_ADDRESS", c.signer.Address())
	req.Header.Set("POLY_PASSPHRASE", c.auth.Passphrase)
	req.Header.Set("POLY_TIMESTAMP", timestamp)
	req.Header.Set("POLY_SIGNATURE", signature)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to update balance allowance: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("update balance allowance failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// GetPositions retrieves all positions
func (c *CLOBClient) GetPositions(ctx context.Context) ([]Position, error) {
	url := fmt.Sprintf("https://data-api.polymarket.com/positions?user=%s", c.signer.Address())

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get positions from Data API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return []Position{}, nil
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get positions failed with status %d", resp.StatusCode)
	}

	var positions []Position
	if err := json.NewDecoder(resp.Body).Decode(&positions); err != nil {
		return nil, fmt.Errorf("failed to decode positions: %w", err)
	}

	return positions, nil
}

// TradeHistory represents a historical trade
type TradeHistory struct {
	ID        string  `json:"id"`
	TokenID   string  `json:"asset_id"`
	Side      string  `json:"side"`
	Price     float64 `json:"price,string"`
	Size      float64 `json:"size,string"`
	Fee       float64 `json:"fee,string"`
	Timestamp string  `json:"timestamp"`
	Status    string  `json:"status"`
}

// GetTradeHistory retrieves trade history
func (c *CLOBClient) GetTradeHistory(ctx context.Context) ([]TradeHistory, error) {
	path := "/trades"
	timestamp, signature := c.auth.SignL2Request("GET", path, "")

	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("POLY_API_KEY", c.auth.APIKey)
	req.Header.Set("POLY_ADDRESS", c.signer.Address())
	req.Header.Set("POLY_PASSPHRASE", c.auth.Passphrase)
	req.Header.Set("POLY_TIMESTAMP", timestamp)
	req.Header.Set("POLY_SIGNATURE", signature)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get trades: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get trades failed with status %d", resp.StatusCode)
	}

	var response struct {
		Data       []TradeHistory `json:"data"`
		NextCursor string         `json:"next_cursor"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, fmt.Errorf("failed to decode trades: %w", err)
	}

	return response.Data, nil
}

// MarketInfo represents market resolution info
type MarketInfo struct {
	ConditionID     string `json:"condition_id"`
	QuestionID      string `json:"question_id"`
	Active          bool   `json:"active"`
	Closed          bool   `json:"closed"`
	AcceptingOrders bool   `json:"accepting_orders"`
	EndDateISO      string `json:"end_date_iso"`
	GameStartTime   string `json:"game_start_time"`
	Tokens          []struct {
		TokenID string      `json:"token_id"`
		Outcome string      `json:"outcome"`
		Winner  bool        `json:"winner"`
		Price   interface{} `json:"price"` // Can be string, number, or null
	} `json:"tokens"`
}

// GetMarketInfo retrieves market info including resolution status
func (c *CLOBClient) GetMarketInfo(ctx context.Context, conditionID string) (*MarketInfo, error) {
	path := "/markets/" + conditionID

	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get market info: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get market info failed with status %d", resp.StatusCode)
	}

	var info MarketInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("failed to decode market info: %w", err)
	}

	for i := range info.Tokens {
		info.Tokens[i].Outcome = core.SanitizeString(info.Tokens[i].Outcome)
	}

	return &info, nil
}

// RedeemPositions redeems winning positions for a resolved market
// Note: This requires interaction with the CTF contract on Polygon
// The CLOB API handles this automatically for most cases
func (c *CLOBClient) RedeemPositions(ctx context.Context, conditionID string, numOutcomes int) error {
	if c.testMode {
		return nil
	}

	// Check market resolution status
	info, err := c.GetMarketInfo(ctx, conditionID)
	if err != nil {
		return fmt.Errorf("failed to get market info: %w", err)
	}

	if !info.Closed {
		return fmt.Errorf("market is not yet resolved")
	}

	// Find winning token
	var winnerTokenID string
	for _, token := range info.Tokens {
		if token.Winner {
			winnerTokenID = token.TokenID
			break
		}
	}

	if winnerTokenID == "" {
		return fmt.Errorf("no winning outcome found")
	}

	// The CLOB auto-redeems in most cases, but we can trigger via merge endpoint
	// For binary markets, winning positions are automatically credited
	return nil
}

// generateSalt generates a random salt for order signing
func generateSalt() int64 {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	// Clear highest bit to ensure it fits in a positive int64
	b[0] &= 0x7f
	return new(big.Int).SetBytes(b).Int64()
}
