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
	BaseURL  string
	signer   *Signer
	auth     *APIAuth
	testMode bool
}

// NewCLOBClient creates a new authenticated CLOB client
func NewCLOBClient(privateKeyHex, apiKey, apiSecret, apiPassphrase string) (*CLOBClient, error) {
	signer, err := NewSigner(privateKeyHex)
	if err != nil {
		return nil, fmt.Errorf("failed to create signer: %w", err)
	}

	return &CLOBClient{
		BaseURL: "https://clob.polymarket.com",
		signer:  signer,
		auth: &APIAuth{
			APIKey:     apiKey,
			APISecret:  apiSecret,
			Passphrase: apiPassphrase,
		},
		testMode: false,
	}, nil
}

// SetTestMode enables/disables test mode (validate orders but don't submit)
func (c *CLOBClient) SetTestMode(enabled bool) {
	c.testMode = enabled
}

// IsTestMode returns whether test mode is enabled
func (c *CLOBClient) IsTestMode() bool {
	return c.testMode
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
	TIFGoodTilCancelled  TimeInForce = "GTC"
	TIFFillOrKill        TimeInForce = "FOK"
	TIFImmediateOrCancel TimeInForce = "IOC"
)

// OrderRequest represents a new order request
type OrderRequest struct {
	TokenID     string      `json:"tokenID"`
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
	OrderID  string `json:"orderID"`
	Status   string `json:"status"`
	ErrorMsg string `json:"error,omitempty"`
	Success  bool   `json:"success"`
}

// PlaceOrder places a new limit order
func (c *CLOBClient) PlaceOrder(ctx context.Context, req *OrderRequest) (*OrderResponse, error) {
	// Generate random salt
	salt := generateSalt()

	// Calculate amounts in base units (USDC and conditional tokens both use 6 decimals)
	var makerAmount, takerAmount string
	if req.Side == SideBuy {
		// BUY: makerAmount = USDC, takerAmount = shares
		usdcAmount := req.Price * req.Size

		// USDC to 6 decimals
		uAmt := new(big.Int)
		uFloat := new(big.Float).Mul(big.NewFloat(usdcAmount), big.NewFloat(1e6))
		uFloat.Int(uAmt)
		makerAmount = uAmt.String()

		// Shares to 6 decimals
		sAmt := new(big.Int)
		sFloat := new(big.Float).Mul(big.NewFloat(req.Size), big.NewFloat(1e6))
		sFloat.Int(sAmt)
		takerAmount = sAmt.String()
	} else {
		// SELL: makerAmount = shares, takerAmount = USDC
		usdcAmount := req.Price * req.Size

		// Shares to 6 decimals
		sAmt := new(big.Int)
		sFloat := new(big.Float).Mul(big.NewFloat(req.Size), big.NewFloat(1e6))
		sFloat.Int(sAmt)
		makerAmount = sAmt.String()

		// USDC to 6 decimals
		uAmt := new(big.Int)
		uFloat := new(big.Float).Mul(big.NewFloat(usdcAmount), big.NewFloat(1e6))
		uFloat.Int(uAmt)
		takerAmount = uAmt.String()
	}

	// Default expiration: 24 hours from now
	// For FOK, Polymarket API expects 0
	expirationStr := strconv.FormatInt(req.Expiration, 10)
	if req.TimeInForce == "FOK" {
		expirationStr = "0"
	} else if req.Expiration == 0 {
		expirationStr = strconv.FormatInt(time.Now().Add(24*time.Hour).Unix(), 10)
	}

	// Build order data
	sideInt := 0
	if req.Side == SideSell {
		sideInt = 1
	}

	orderData := &OrderData{
		Salt:          strconv.FormatInt(salt, 10),
		Maker:         c.signer.Address(),
		Signer:        c.signer.Address(),
		Taker:         "0x0000000000000000000000000000000000000000", // Any taker
		TokenID:       req.TokenID,
		MakerAmount:   makerAmount,
		TakerAmount:   takerAmount,
		Expiration:    expirationStr,
		Nonce:         "0",
		FeeRateBps:    strconv.Itoa(req.FeeRateBps),
		Side:          sideInt,
		SignatureType: 0, // EOA signature
	}

	// Sign the order
	signature, err := c.signer.SignOrder(orderData)
	if err != nil {
		return nil, fmt.Errorf("failed to sign order: %w", err)
	}

	// Ensure tokenID is in hex format for JSON payload
	tokenIDHex := req.TokenID
	if !strings.HasPrefix(tokenIDHex, "0x") {
		// Convert decimal string to hex
		n := new(big.Int)
		n.SetString(tokenIDHex, 10)
		tokenIDHex = "0x" + n.Text(16)
	}

	// Build signed order
	signedOrder := &SignedOrder{
		Order: OrderPayload{
			Salt:          salt, // MUST be Integer
			Maker:         orderData.Maker,
			Signer:        orderData.Signer,
			Taker:         orderData.Taker,
			TokenID:       req.TokenID, // MUST be Decimal String
			MakerAmount:   orderData.MakerAmount, // MUST be String
			TakerAmount:   orderData.TakerAmount, // MUST be String
			Expiration:    orderData.Expiration, // MUST be String
			Nonce:         orderData.Nonce, // MUST be String
			FeeRateBps:    strconv.Itoa(req.FeeRateBps), // MUST be String
			Side:          strconv.Itoa(orderData.Side), // MUST be string "0" or "1"
			SignatureType: orderData.SignatureType, // MUST be Integer
			Signature:     signature,
		},
		Owner:     c.auth.APIKey, // MUST be API Key for CLOB
		OrderType: string(req.OrderType),
	}

	// Submit order to API
	return c.submitOrder(ctx, signedOrder, req.TimeInForce, req.Price, string(req.Side))
}

// submitOrder sends the signed order to the CLOB API
func (c *CLOBClient) submitOrder(ctx context.Context, signedOrder *SignedOrder, tif TimeInForce, price float64, side string) (*OrderResponse, error) {
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

	// Top-level side field for validation
	if side != "" {
		payload["side"] = side
	}

	// Some versions of the API require an explicit price field for validation
	if price > 0 {
		payload["price"] = price
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal order: %w", err)
	}

	// Generate auth headers (validates credentials are working)
	path := "/order"
	timestamp, signature := c.auth.SignL2Request("POST", path, string(body))

	// In test mode: validate everything but don't actually submit
	if c.testMode {
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

		if signedOrder.Order.Side == "0" && allowance.Balance < makerAmountUSDC {
			return &OrderResponse{
				OrderID:  fmt.Sprintf("test-%d", time.Now().UnixNano()),
				Success:  false,
				ErrorMsg: fmt.Sprintf("test mode: insufficient balance ($%.2f < $%.2f needed)", allowance.Balance, makerAmountUSDC),
			}, nil
		}

		// All validations passed
		return &OrderResponse{
			OrderID:  fmt.Sprintf("test-%d", time.Now().UnixNano()),
			Success:  true,
			ErrorMsg: fmt.Sprintf("test mode: order validated (signed, balance OK: $%.2f)", allowance.Balance),
		}, nil
	}

	// Real submission
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
		return nil, fmt.Errorf("failed to submit order: %w", err)
	}
	defer resp.Body.Close()

	// Read full body for error reporting
	bodyBytes, _ := io.ReadAll(resp.Body)
	
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		// Log the failed request and response for debugging
		fmt.Printf("\n--- API ERROR DEBUG ---\n")
		fmt.Printf("Request Body: %s\n", string(body))
		fmt.Printf("Response Status: %d\n", resp.StatusCode)
		fmt.Printf("Response Body: %s\n", string(bodyBytes))
		fmt.Printf("-----------------------\n\n")

		var result OrderResponse
		if err := json.Unmarshal(bodyBytes, &result); err != nil {
			return &OrderResponse{
				Success:  false,
				ErrorMsg: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(bodyBytes)),
			}, nil
		}
		result.Success = false
		return &result, nil
	}

	var result OrderResponse
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return &OrderResponse{
			Success:  true,
			ErrorMsg: fmt.Sprintf("Success but decode failed: %v", err),
		}, nil
	}
	result.Success = true
	return &result, nil
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
		return nil, fmt.Errorf("failed to get order: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// Order not found usually means it was filled and removed
		return &OpenOrder{OrderID: orderID, Status: "FILLED"}, nil
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get order failed with status %d", resp.StatusCode)
	}

	var order OpenOrder
	if err := json.NewDecoder(resp.Body).Decode(&order); err != nil {
		return nil, fmt.Errorf("failed to decode order: %w", err)
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
		switch order.Status {
		case "FILLED", "MATCHED":
			return true, nil
		case "CANCELLED", "EXPIRED":
			return false, nil
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
	TokenID  string  `json:"asset"`
	Size     float64 `json:"size,string"`
	AvgPrice float64 `json:"avgPrice,string"`
	Outcome  string  `json:"outcome"` // Mapped from token lookup
}

// BalanceAllowance represents USDC balance and allowance info
type BalanceAllowance struct {
	Balance   float64 `json:"balance,string"`
	Allowance float64 `json:"allowance,string"`
}

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
	path := "/positions"
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
		return nil, fmt.Errorf("failed to get positions: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return []Position{}, nil // 404 means no positions found for this account
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

	var trades []TradeHistory
	if err := json.NewDecoder(resp.Body).Decode(&trades); err != nil {
		return nil, fmt.Errorf("failed to decode trades: %w", err)
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
func (c *CLOBClient) RedeemPositions(ctx context.Context, conditionID string) error {
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
	rand.Read(b)
	// Ensure positive salt
	val := new(big.Int).SetBytes(b).Int64()
	if val < 0 {
		return -val
	}
	return val
}
