package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
)

const KalshiBaseURL = "https://trading-api.kalshi.com/trade-api/v2"

// KalshiClient implements ExchangeClient for the Kalshi exchange
type KalshiClient struct {
	baseURL string
	signer  *KalshiSigner
	testMode bool
}

// NewKalshiClient creates a new Kalshi client
func NewKalshiClient(accessKey, privateKeyPEM string) (*KalshiClient, error) {
	signer, err := NewKalshiSigner(accessKey, privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("failed to create kalshi signer: %w", err)
	}

	return &KalshiClient{
		baseURL: KalshiBaseURL,
		signer:  signer,
	}, nil
}

func (c *KalshiClient) doRequest(ctx context.Context, method, path string, body interface{}, result interface{}) error {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return err
	}

	// Kalshi requires the /trade-api/v2 prefix in the signature path
	fullPath := "/trade-api/v2" + path
	timestamp, signature, err := c.signer.SignRequest(method, fullPath)
	if err != nil {
		return err
	}

	// Kalshi API requires precise header names (Go's Header.Set canonicalizes them, which might fail)
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header["KALSHI-ACCESS-KEY"] = []string{c.signer.AccessKey}
	req.Header["KALSHI-ACCESS-SIGNATURE"] = []string{signature}
	req.Header["KALSHI-ACCESS-TIMESTAMP"] = []string{timestamp}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("kalshi API error: %d %s", resp.StatusCode, string(b))
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return err
		}
	}

	return nil
}

func (c *KalshiClient) PlaceOrder(ctx context.Context, req *OrderRequest) (*OrderResponse, error) {
	// Kalshi requires whole integer sizes for contracts.
	// We round to the nearest integer and reject if the result is less than 1.
	count := int(math.Round(req.Size))
	if count < 1 {
		return nil, fmt.Errorf("order size of %.2f is less than the minimum of 1 contract for Kalshi", req.Size)
	}

	// Normalizing Price: Poly is 0.01 - 0.99. Kalshi is cents 1 - 99.
	priceCents := int(math.Round(req.Price * 100))

	// Note: You must map 'req.TokenID' or similar to Kalshi 'ticker'.
	// Strip -YES or -NO suffixes if present
	ticker := req.TokenID
	if strings.HasSuffix(ticker, "-YES") {
		ticker = strings.TrimSuffix(ticker, "-YES")
	} else if strings.HasSuffix(ticker, "-NO") {
		ticker = strings.TrimSuffix(ticker, "-NO")
	}

	kalshiSide := "yes"
	if strings.EqualFold(req.Outcome, "no") {
		kalshiSide = "no"
	}

	kalshiAction := "buy"
	if req.Side == SideSell {
		kalshiAction = "sell"
	}

	kalshiReq := map[string]interface{}{
		"ticker": ticker,
		"action": kalshiAction,
		"side":   kalshiSide,
		"count":  count,
	}
	
	// In Kalshi V2, 'type' is not explicitly required/supported in the payload.
	// It infers limit orders if yes_price or no_price is provided.
	// We only pass price for limit orders.
	if req.OrderType != OrderTypeMarket {
		if kalshiSide == "yes" {
			kalshiReq["yes_price"] = priceCents
		} else {
			kalshiReq["no_price"] = priceCents
		}
	}

	var kalshiResp struct {
		Order struct {
			OrderId string `json:"order_id"`
			Status  string `json:"status"`
		} `json:"order"`
	}

	err := c.doRequest(ctx, "POST", "/portfolio/orders", kalshiReq, &kalshiResp)
	if err != nil {
		return nil, err
	}

	return &OrderResponse{
		OrderID: kalshiResp.Order.OrderId,
		Success: true,
	}, nil
}

func (c *KalshiClient) CancelOrder(ctx context.Context, orderID string) error {
	return c.doRequest(ctx, "DELETE", "/portfolio/orders/"+orderID, nil, nil)
}

func (c *KalshiClient) CancelAllOrders(ctx context.Context) error {
	orders, err := c.GetOpenOrders(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch open orders for cancellation: %w", err)
	}

	var lastErr error
	for _, o := range orders {
		if err := c.CancelOrder(ctx, o.OrderID); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

func (c *KalshiClient) GetPositions(ctx context.Context) ([]Position, error) {
	var resp struct {
		MarketPositions []struct {
			Ticker     string `json:"ticker"`
			PositionFp string `json:"position_fp"`
		} `json:"market_positions"`
	}
	err := c.doRequest(ctx, "GET", "/portfolio/positions", nil, &resp)
	if err != nil {
		return nil, err
	}

	var positions []Position
	for _, p := range resp.MarketPositions {
		if p.PositionFp == "" || p.PositionFp == "0" || p.PositionFp == "0.00" {
			continue
		}
		
		var size float64
		_, err := fmt.Sscanf(p.PositionFp, "%f", &size)
		if err != nil {
			continue
		}

		if size == 0 {
			continue
		}

		outcome := "Yes"
		if size < 0 {
			outcome = "No"
			size = -size
		}

		tokenID := p.Ticker
		if outcome == "Yes" {
			tokenID += "-YES"
		} else {
			tokenID += "-NO"
		}

		positions = append(positions, Position{
			TokenID: tokenID,
			Size:    size,
			Outcome: outcome,
		})
	}
	return positions, nil
}

func (c *KalshiClient) GetOrder(ctx context.Context, orderID string) (*OpenOrder, error) {
	var resp struct {
		Order struct {
			Status string `json:"status"`
		} `json:"order"`
	}
	err := c.doRequest(ctx, "GET", "/portfolio/orders/"+orderID, nil, &resp)
	if err != nil {
		return nil, err
	}

	return &OpenOrder{
		OrderID: orderID,
		Status:  resp.Order.Status,
	}, nil
}

func (c *KalshiClient) GetOpenOrders(ctx context.Context) ([]OpenOrder, error) {
	var resp struct {
		Orders []struct {
			OrderId string `json:"order_id"`
			Status  string `json:"status"`
			Ticker  string `json:"ticker"`
			Side    string `json:"side"`
		} `json:"orders"`
	}
	err := c.doRequest(ctx, "GET", "/portfolio/orders?status=resting", nil, &resp)
	if err != nil {
		return nil, err
	}

	var orders []OpenOrder
	for _, o := range resp.Orders {
		tokenID := o.Ticker
		if strings.EqualFold(o.Side, "yes") {
			tokenID += "-YES"
		} else if strings.EqualFold(o.Side, "no") {
			tokenID += "-NO"
		}
		orders = append(orders, OpenOrder{
			OrderID: o.OrderId,
			Status:  o.Status,
			TokenID: tokenID,
		})
	}
	return orders, nil
}

func (c *KalshiClient) GetBalanceAllowance(ctx context.Context) (*BalanceAllowance, error) {
	var resp struct {
		Balance int64 `json:"balance"` // cents
	}
	err := c.doRequest(ctx, "GET", "/portfolio/balance", nil, &resp)
	if err != nil {
		return nil, err
	}

	return &BalanceAllowance{
		Balance:   float64(resp.Balance) / 100.0, // Convert to dollars
		Allowance: 1000000.0, // Kalshi uses cash, no allowance
	}, nil
}

func (c *KalshiClient) UpdateBalanceAllowance(ctx context.Context) error {
	return nil
}

func (c *KalshiClient) GetMarketInfo(ctx context.Context, conditionID string) (*MarketInfo, error) {
	var resp struct {
		Market struct {
			Status string `json:"status"`
		} `json:"market"`
	}
	err := c.doRequest(ctx, "GET", "/markets/"+conditionID, nil, &resp)
	if err != nil {
		return nil, err
	}
	resolved := resp.Market.Status == "closed" || resp.Market.Status == "settled"

	return &MarketInfo{
		ConditionID: conditionID,
		Closed:      resolved,
	}, nil
}

func (c *KalshiClient) SetTestMode(enabled bool) {
	c.testMode = enabled
}

func (c *KalshiClient) IsTestMode() bool {
	return c.testMode
}

func (c *KalshiClient) GetSigner() *Signer {
	return nil // Kalshi doesn't use the Polygon EVM signer
}

func (c *KalshiClient) Address() string {
	return ""
}

func (c *KalshiClient) EnableRawAPILog(path string) error {
	return nil
}

func (c *KalshiClient) CloseRawAPILog() error {
	return nil
}

var _ ExchangeClient = (*KalshiClient)(nil)
