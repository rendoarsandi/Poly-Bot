package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
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
	negRisk   sync.Map
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
	if req.Side == SideBuy && req.UseMarketBuyPrecision {
		return true
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
	// Internal-only hint for exact-share buys that should still obey the
	// market-buy amount precision rules required by the venue.
	UseMarketBuyPrecision bool `json:"-"`
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
	Taker         string `json:"taker,omitempty"`
	TokenID       string `json:"tokenId"`
	MakerAmount   string `json:"makerAmount"`
	TakerAmount   string `json:"takerAmount"`
	Side          string `json:"side"`
	SignatureType int    `json:"signatureType"`
	Timestamp     string `json:"timestamp"`
	Expiration    string `json:"expiration"`
	Metadata      string `json:"metadata"`
	Builder       string `json:"builder"`
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

const (
	openOrdersInitialCursor = "MA=="
	openOrdersEndCursor     = "LTE="
	zeroBytes32             = "0x0000000000000000000000000000000000000000000000000000000000000000"
)

// PlaceOrder places a new limit order
func (c *CLOBClient) PlaceOrder(ctx context.Context, req *OrderRequest) (*OrderResponse, error) {
	// Generate random salt
	salt := generateSalt()

	// Calculate amounts based on order side
	// BUY and SELL have different decimal precision requirements per Polymarket API
	amounts, err := ComputeOrderAmounts(req)
	if err != nil {
		return nil, err
	}

	// Polymarket rejects non-zero expiration for non-GTD orders.
	// We only send a non-zero expiration when explicitly provided.
	expirationStr := "0"
	if req.Expiration > 0 {
		expirationStr = strconv.FormatInt(req.Expiration, 10)
	}

	verifyingContract, err := c.getExchangeVerifyingContract(ctx, req.TokenID)
	if err != nil {
		return nil, err
	}
	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)

	submitSigned := func(salt int64, makerAmt, takerAmt string, sideInt int) (*OrderResponse, error) {
		orderData := &OrderData{
			Salt:              strconv.FormatInt(salt, 10),
			Maker:             c.signer.Address(),
			Signer:            c.signer.Address(),
			TokenID:           req.TokenID,
			MakerAmount:       makerAmt,
			TakerAmount:       takerAmt,
			Timestamp:         timestamp,
			Expiration:        expirationStr,
			Metadata:          zeroBytes32,
			Builder:           zeroBytes32,
			VerifyingContract: verifyingContract,
			Side:              sideInt,
			SignatureType:     0, // EOA signature
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
				TokenID:       req.TokenID,
				MakerAmount:   orderData.MakerAmount,
				TakerAmount:   orderData.TakerAmount,
				Side:          string(req.Side), // Send "BUY" or "SELL"
				SignatureType: orderData.SignatureType,
				Timestamp:     orderData.Timestamp,
				Expiration:    orderData.Expiration,
				Metadata:      orderData.Metadata,
				Builder:       orderData.Builder,
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

	resp, err := submitSigned(salt, amounts.MakerAmount, amounts.TakerAmount, sideInt)
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

		if signedOrder.Order.Side == string(SideBuy) && (allowance.Balance < makerAmountUSDC || allowance.Allowance < makerAmountUSDC) {
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

	path := "/order"
	payload := map[string]string{"orderID": orderID}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal cancel order payload: %w", err)
	}
	timestamp, signature := c.auth.SignL2Request("DELETE", path, string(body))

	req, err := http.NewRequestWithContext(ctx, "DELETE", c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
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

	path := "/cancel-all"
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

func (o *OpenOrder) UnmarshalJSON(data []byte) error {
	var aux struct {
		OrderID         string          `json:"orderID"`
		ID              string          `json:"id"`
		TokenID         string          `json:"tokenID"`
		AssetID         string          `json:"asset_id"`
		Side            string          `json:"side"`
		Status          string          `json:"status"`
		OriginalSize    json.RawMessage `json:"original_size"`
		SizeMatched     json.RawMessage `json:"size_matched"`
		PriceRaw        json.RawMessage `json:"price"`
		CreatedAtRaw    json.RawMessage `json:"created_at"`
		CreatedAtAlt    json.RawMessage `json:"createdAt"`
		RemainingSize   json.RawMessage `json:"remaining_size"`
		RemainingAlt    json.RawMessage `json:"remainingSize"`
		OriginalSizeAlt json.RawMessage `json:"originalSize"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	o.OrderID = aux.OrderID
	if o.OrderID == "" {
		o.OrderID = aux.ID
	}
	o.TokenID = aux.TokenID
	if o.TokenID == "" {
		o.TokenID = aux.AssetID
	}
	o.Side = aux.Side
	o.Status = normalizeOpenOrderStatus(aux.Status)
	price, _ := parseFlexibleFloat(aux.PriceRaw)
	if price != 0 || len(aux.PriceRaw) > 0 {
		o.Price = price
	}

	originalSize := 0.0
	if size, ok := parseHumanOrderSize(aux.OriginalSizeAlt); ok {
		originalSize = size
	}
	if size, ok := parseBaseUnitOrderSize(aux.OriginalSize); ok {
		originalSize = size
	}
	o.OriginalSize = originalSize

	remainingSize := 0.0
	if size, ok := parseHumanOrderSize(aux.RemainingAlt); ok {
		remainingSize = size
	}
	if size, ok := parseBaseUnitOrderSize(aux.RemainingSize); ok {
		remainingSize = size
	}
	if matched, ok := parseBaseUnitOrderSize(aux.SizeMatched); ok && o.OriginalSize >= matched {
		remainingSize = math.Max(o.OriginalSize-matched, 0)
	}
	o.RemainingSize = remainingSize

	if createdAt := parseFlexibleString(aux.CreatedAtRaw); createdAt != "" {
		o.CreatedAt = createdAt
	}
	if o.CreatedAt == "" {
		o.CreatedAt = parseFlexibleString(aux.CreatedAtAlt)
	}
	return nil
}

// GetOpenOrders retrieves all open orders
func (c *CLOBClient) GetOpenOrders(ctx context.Context) ([]OpenOrder, error) {
	path := "/data/orders"
	timestamp, signature := c.auth.SignL2Request("GET", path, "")
	nextCursor := openOrdersInitialCursor
	orders := make([]OpenOrder, 0)

	for nextCursor != openOrdersEndCursor {
		reqURL := c.BaseURL + path
		if nextCursor != "" {
			values := url.Values{}
			values.Set("next_cursor", nextCursor)
			reqURL += "?" + values.Encode()
		}

		req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
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

		if resp.StatusCode != http.StatusOK {
			bodyBytes, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return nil, fmt.Errorf("get open orders failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
		}

		var page struct {
			Data       []OpenOrder `json:"data"`
			NextCursor string      `json:"next_cursor"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("failed to decode orders: %w", err)
		}
		_ = resp.Body.Close()

		orders = append(orders, page.Data...)
		if page.NextCursor == "" || page.NextCursor == nextCursor {
			break
		}
		nextCursor = page.NextCursor
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
	if order.Status == "FAILED" || order.Status == "CANCELLED" || order.Status == "EXPIRED" || order.Status == "REJECTED" || order.Status == "INVALID" {
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
	path := "/data/trades"
	timestamp, signature := c.auth.SignL2Request("GET", path, "")
	trades := make([]TradeHistory, 0)
	nextCursor := openOrdersInitialCursor

	for nextCursor != openOrdersEndCursor {
		reqURL := c.BaseURL + path
		if nextCursor != "" {
			values := url.Values{}
			values.Set("next_cursor", nextCursor)
			reqURL += "?" + values.Encode()
		}

		req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
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

		if resp.StatusCode != http.StatusOK {
			bodyBytes, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return nil, fmt.Errorf("get trades failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
		}

		var response struct {
			Data       []TradeHistory `json:"data"`
			NextCursor string         `json:"next_cursor"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("failed to decode trades: %w", err)
		}
		_ = resp.Body.Close()

		trades = append(trades, response.Data...)
		if response.NextCursor == "" || response.NextCursor == nextCursor {
			break
		}
		nextCursor = response.NextCursor
	}

	return trades, nil
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

func normalizeOpenOrderStatus(status string) string {
	normalized := strings.ToUpper(strings.TrimSpace(status))
	normalized = strings.TrimPrefix(normalized, "ORDER_STATUS_")
	switch normalized {
	case "CANCELED":
		return "CANCELLED"
	case "CANCELED_MARKET_RESOLVED":
		return "CANCELLED"
	default:
		return normalized
	}
}

func parseFlexibleFloat(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var asFloat float64
	if err := json.Unmarshal(raw, &asFloat); err == nil {
		return asFloat, true
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		asString = strings.TrimSpace(asString)
		if asString == "" {
			return 0, false
		}
		value, err := strconv.ParseFloat(asString, 64)
		if err == nil {
			return value, true
		}
	}
	return 0, false
}

func parseFlexibleString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return strings.TrimSpace(asString)
	}
	var asInt int64
	if err := json.Unmarshal(raw, &asInt); err == nil {
		return strconv.FormatInt(asInt, 10)
	}
	var asFloat float64
	if err := json.Unmarshal(raw, &asFloat); err == nil {
		return strconv.FormatInt(int64(asFloat), 10)
	}
	return strings.TrimSpace(string(raw))
}

func parseBaseUnitOrderSize(raw json.RawMessage) (float64, bool) {
	value, ok := parseFlexibleFloat(raw)
	if !ok {
		return 0, false
	}
	return value / usdcBaseUnitsPerToken, true
}

func parseHumanOrderSize(raw json.RawMessage) (float64, bool) {
	return parseFlexibleFloat(raw)
}

func (c *CLOBClient) getExchangeVerifyingContract(ctx context.Context, tokenID string) (string, error) {
	negRisk, err := c.getNegRisk(ctx, tokenID)
	if err != nil {
		return "", err
	}
	if negRisk {
		return NegRiskExchange, nil
	}
	return CTFExchange, nil
}

func (c *CLOBClient) getNegRisk(ctx context.Context, tokenID string) (bool, error) {
	if tokenID == "" {
		return false, nil
	}
	if cached, ok := c.negRisk.Load(tokenID); ok {
		if negRisk, ok := cached.(bool); ok {
			return negRisk, nil
		}
	}

	values := url.Values{}
	values.Set("token_id", tokenID)
	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/neg-risk?"+values.Encode(), nil)
	if err != nil {
		return false, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("failed to resolve neg-risk market for token %s: %w", tokenID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("neg-risk lookup failed with status %d for token %s: %s", resp.StatusCode, tokenID, strings.TrimSpace(string(bodyBytes)))
	}

	var result struct {
		NegRisk bool `json:"neg_risk"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, fmt.Errorf("failed to decode neg-risk lookup for token %s: %w", tokenID, err)
	}
	c.negRisk.Store(tokenID, result.NegRisk)
	return result.NegRisk, nil
}

// GetClobMarketInfo fetches CLOB-level market metadata, including V2 neg-risk routing.
func (c *CLOBClient) GetClobMarketInfo(ctx context.Context, conditionID string) (*ClobMarketInfo, error) {
	conditionID = strings.TrimSpace(conditionID)
	if conditionID == "" {
		return nil, fmt.Errorf("condition ID is required")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/clob-markets/"+url.PathEscape(conditionID), nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch clob market info: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
		return nil, fmt.Errorf("failed to fetch clob market info: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var info ClobMarketInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBodySize)).Decode(&info); err != nil {
		return nil, fmt.Errorf("failed to decode clob market info: %w", err)
	}
	return &info, nil
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
	// The official SDK sends salt as a JS number, so keep it within the
	// IEEE-754 safe integer range to avoid precision loss on the wire.
	const maxSafeInt53 = (1 << 53) - 1
	salt := binary.BigEndian.Uint64(b) & maxSafeInt53
	if salt == 0 {
		salt = 1
	}
	return int64(salt)
}
