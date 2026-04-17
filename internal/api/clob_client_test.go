package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPlaceOrder_FOK_Killed(t *testing.T) {
	// 1. Setup Mock Server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request path
		if r.URL.Path != "/order" {
			t.Errorf("Expected path /order, got %s", r.URL.Path)
		}

		// Return Success=true but Status="KILLED"
		// This simulates the dangerous scenario we fixed
		resp := OrderResponse{
			Success:  true,
			Status:   "KILLED",
			OrderID:  "0x123",
			ErrorMsg: "",
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	// 2. Mock the package-level httpClient
	originalClient := httpClient
	httpClient = server.Client()
	defer func() { httpClient = originalClient }()

	// 3. Create Client with Dummy Keys (valid hex for signer)
	// A valid 32-byte hex string for private key
	dummyPK := "0000000000000000000000000000000000000000000000000000000000000001"
	client, err := NewCLOBClient(dummyPK, "api-key", "api-secret", "passphrase")
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	// Override BaseURL to point to mock server
	client.BaseURL = server.URL

	// 4. Place Order
	req := &OrderRequest{
		TokenID:     "123456",
		Price:       0.5,
		Size:        10.0,
		Side:        SideBuy,
		OrderType:   OrderTypeMarket,
		TimeInForce: TIFFillOrKill,
	}

	resp, err := client.PlaceOrder(context.Background(), req)
	if err != nil {
		t.Fatalf("PlaceOrder failed: %v", err)
	}

	// 5. Assertions
	if resp.Success {
		t.Error("Expected Success=false for KILLED order, got true")
	}

	expectedErrorMsg := "Order was KILLED"
	if resp.ErrorMsg != expectedErrorMsg {
		t.Errorf("Expected ErrorMsg=%q, got %q", expectedErrorMsg, resp.ErrorMsg)
	}
}

func TestPlaceOrder_FOK_Success(t *testing.T) {
	// 1. Setup Mock Server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := OrderResponse{
			Success: true,
			Status:  "MATCHED", // or FILLED
			OrderID: "0x123",
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	// 2. Mock httpClient
	originalClient := httpClient
	httpClient = server.Client()
	defer func() { httpClient = originalClient }()

	// 3. Create Client
	dummyPK := "0000000000000000000000000000000000000000000000000000000000000001"
	client, _ := NewCLOBClient(dummyPK, "key", "secret", "pass")
	client.BaseURL = server.URL

	// 4. Place Order
	req := &OrderRequest{
		TokenID:     "123456",
		Price:       0.5,
		Size:        10.0,
		Side:        SideBuy,
		OrderType:   OrderTypeMarket,
		TimeInForce: TIFFillOrKill,
	}

	resp, err := client.PlaceOrder(context.Background(), req)
	if err != nil {
		t.Fatalf("PlaceOrder failed: %v", err)
	}

	// 5. Assertions
	if !resp.Success {
		t.Error("Expected Success=true for MATCHED order, got false")
	}
}

func TestPlaceOrder_RetriesRetryableExecutionError(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := calls.Add(1)
		if attempt == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"could not run the execution"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(OrderResponse{
			Success: true,
			Status:  "MATCHED",
			OrderID: "0xretry",
		})
	}))
	defer server.Close()

	originalClient := httpClient
	httpClient = server.Client()
	defer func() { httpClient = originalClient }()

	client, err := NewCLOBClient(strings.Repeat("1", 64), "key", "secret", "pass")
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	client.BaseURL = server.URL

	resp, err := client.PlaceOrder(context.Background(), &OrderRequest{
		TokenID:     "123456",
		Price:       0.5,
		Size:        10.0,
		Side:        SideBuy,
		OrderType:   OrderTypeMarket,
		TimeInForce: TIFFillAndKill,
	})
	if err != nil {
		t.Fatalf("PlaceOrder failed: %v", err)
	}
	if !resp.Success || resp.OrderID != "0xretry" {
		t.Fatalf("expected retry to succeed, got %+v", resp)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected exactly 2 attempts, got %d", calls.Load())
	}
}

func TestPlaceOrder_RetriesTooEarly(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := calls.Add(1)
		if attempt == 1 {
			w.WriteHeader(http.StatusTooEarly)
			_, _ = w.Write([]byte(`{"error":"matching engine restarting"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(OrderResponse{
			Success: true,
			Status:  "MATCHED",
			OrderID: "0x425",
		})
	}))
	defer server.Close()

	originalClient := httpClient
	httpClient = server.Client()
	defer func() { httpClient = originalClient }()

	client, err := NewCLOBClient(strings.Repeat("1", 64), "key", "secret", "pass")
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	client.BaseURL = server.URL

	resp, err := client.PlaceOrder(context.Background(), &OrderRequest{
		TokenID:     "123456",
		Price:       0.5,
		Size:        10.0,
		Side:        SideBuy,
		OrderType:   OrderTypeMarket,
		TimeInForce: TIFFillAndKill,
	})
	if err != nil {
		t.Fatalf("PlaceOrder failed: %v", err)
	}
	if !resp.Success || resp.OrderID != "0x425" {
		t.Fatalf("expected 425 retry to succeed, got %+v", resp)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected exactly 2 attempts, got %d", calls.Load())
	}
}

func TestPlaceOrders_RetriesRetryableExecutionError(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := calls.Add(1)
		if attempt == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`[{"error":"could not run the execution"}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"success":true,"status":"matched","orderID":"0xbatch"}]`))
	}))
	defer server.Close()

	originalClient := httpClient
	httpClient = server.Client()
	defer func() { httpClient = originalClient }()

	client, err := NewCLOBClient(strings.Repeat("1", 64), "key", "secret", "pass")
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	client.BaseURL = server.URL

	resp, err := client.PlaceOrders(context.Background(), []*OrderRequest{{
		TokenID:     "123456",
		Price:       0.5,
		Size:        10.0,
		Side:        SideBuy,
		OrderType:   OrderTypeMarket,
		TimeInForce: TIFFillAndKill,
	}})
	if err != nil {
		t.Fatalf("PlaceOrders failed: %v", err)
	}
	if len(resp) != 1 || !resp[0].Success || resp[0].OrderID != "0xbatch" {
		t.Fatalf("expected batch retry to succeed, got %+v", resp)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected exactly 2 attempts, got %d", calls.Load())
	}
}

func TestPlaceOrders_FAK_Killed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orders" {
			t.Errorf("Expected path /orders, got %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[{"success":true,"status":"KILLED","orderID":"0xabc","errorMsg":""}]`))
	}))
	defer server.Close()

	originalClient := httpClient
	httpClient = server.Client()
	defer func() { httpClient = originalClient }()

	dummyPK := "0000000000000000000000000000000000000000000000000000000000000001"
	client, err := NewCLOBClient(dummyPK, "key", "secret", "pass")
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	client.BaseURL = server.URL

	resp, err := client.PlaceOrders(context.Background(), []*OrderRequest{{
		TokenID:     "123456",
		Price:       0.5,
		Size:        10.0,
		Side:        SideBuy,
		OrderType:   OrderTypeMarket,
		TimeInForce: TIFFillAndKill,
	}})
	if err != nil {
		t.Fatalf("PlaceOrders failed: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("Expected 1 response, got %d", len(resp))
	}
	if resp[0].Success {
		t.Error("Expected Success=false for KILLED batch order, got true")
	}
	if resp[0].ErrorMsg != "Order was KILLED" {
		t.Errorf("Expected ErrorMsg=%q, got %q", "Order was KILLED", resp[0].ErrorMsg)
	}
}

func TestOrderResponseUnmarshal_PolymarketSchema(t *testing.T) {
	var resp OrderResponse
	if err := json.Unmarshal([]byte(`{
		"success": true,
		"orderID": "0xabcdef1234",
		"status": "matched",
		"makingAmount": "100000000",
		"takingAmount": "200000000",
		"transactionsHashes": ["0xhash"],
		"tradeIDs": ["trade-123"],
		"errorMsg": ""
	}`), &resp); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if !resp.Success || resp.OrderID != "0xabcdef1234" || resp.Status != "matched" {
		t.Fatalf("unexpected decoded response: %+v", resp)
	}
	if resp.MakingAmount != "100000000" || resp.TakingAmount != "200000000" {
		t.Fatalf("unexpected amounts: %+v", resp)
	}
	if len(resp.TransactionsHashes) != 1 || resp.TransactionsHashes[0] != "0xhash" {
		t.Fatalf("unexpected tx hashes: %+v", resp.TransactionsHashes)
	}
	if len(resp.TradeIDs) != 1 || resp.TradeIDs[0] != "trade-123" {
		t.Fatalf("unexpected trade ids: %+v", resp.TradeIDs)
	}
}

func TestPlaceOrders_DocBatchResponseDecodes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orders" {
			t.Fatalf("Expected path /orders, got %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[
			{"success":true,"orderID":"0xabcdef1234567890abcdef1234567890abcdef12","status":"live","makingAmount":"100000000","takingAmount":"200000000","errorMsg":""},
			{"success":true,"orderID":"0xfedcba0987654321fedcba0987654321fedcba09","status":"matched","makingAmount":"200000000","takingAmount":"100000000","transactionsHashes":["0x1234567890abcdef1234567890abcdef12345678"],"tradeIDs":["trade-123"],"errorMsg":""}
		]`))
	}))
	defer server.Close()

	originalClient := httpClient
	httpClient = server.Client()
	defer func() { httpClient = originalClient }()

	client, err := NewCLOBClient(strings.Repeat("1", 64), "key", "secret", "pass")
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	client.BaseURL = server.URL

	resp, err := client.PlaceOrders(context.Background(), []*OrderRequest{{
		TokenID:     "123456",
		Price:       0.48,
		Size:        5,
		Side:        SideBuy,
		OrderType:   OrderTypeLimit,
		TimeInForce: TIFGoodTilCancelled,
	}, {
		TokenID:     "654321",
		Price:       0.52,
		Size:        5,
		Side:        SideSell,
		OrderType:   OrderTypeLimit,
		TimeInForce: TIFGoodTilCancelled,
	}})
	if err != nil {
		t.Fatalf("PlaceOrders failed: %v", err)
	}
	if len(resp) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(resp))
	}
	if resp[0].OrderID == "" || resp[0].Status != "live" {
		t.Fatalf("first batch response did not decode correctly: %+v", resp[0])
	}
	if resp[1].OrderID == "" || resp[1].Status != "matched" {
		t.Fatalf("second batch response did not decode correctly: %+v", resp[1])
	}
	if len(resp[1].TransactionsHashes) != 1 || len(resp[1].TradeIDs) != 1 {
		t.Fatalf("matched batch metadata missing: %+v", resp[1])
	}
}

func TestPlaceOrders_SignatureUsesPayloadSalt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload []struct {
			Order OrderPayload `json:"order"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request failed: %v", err)
		}
		if len(payload) != 1 {
			t.Fatalf("expected 1 order in batch payload, got %d", len(payload))
		}

		order := payload[0].Order
		expectedSigner, err := NewSigner(strings.Repeat("1", 64))
		if err != nil {
			t.Fatalf("NewSigner failed: %v", err)
		}
		expectedSig, err := expectedSigner.SignOrder(&OrderData{
			Salt:          strconv.FormatInt(order.Salt, 10),
			Maker:         order.Maker,
			Signer:        order.Signer,
			Taker:         order.Taker,
			TokenID:       order.TokenID,
			MakerAmount:   order.MakerAmount,
			TakerAmount:   order.TakerAmount,
			Expiration:    order.Expiration,
			Nonce:         order.Nonce,
			FeeRateBps:    order.FeeRateBps,
			Side:          0,
			SignatureType: order.SignatureType,
		})
		if err != nil {
			t.Fatalf("SignOrder failed: %v", err)
		}
		if order.Signature != expectedSig {
			t.Fatalf("expected signature to match payload salt; got %s want %s", order.Signature, expectedSig)
		}

		_, _ = w.Write([]byte(`[{"success":true,"orderID":"0xabc","status":"matched","errorMsg":""}]`))
	}))
	defer server.Close()

	originalClient := httpClient
	httpClient = server.Client()
	defer func() { httpClient = originalClient }()

	client, err := NewCLOBClient(strings.Repeat("1", 64), "key", "secret", "pass")
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	client.BaseURL = server.URL

	_, err = client.PlaceOrders(context.Background(), []*OrderRequest{{
		TokenID:     "123456",
		Price:       0.5,
		Size:        1,
		Side:        SideBuy,
		OrderType:   OrderTypeMarket,
		TimeInForce: TIFFillAndKill,
	}})
	if err != nil {
		t.Fatalf("PlaceOrders failed: %v", err)
	}
}

func TestPlaceOrder_MarketSellPrecision(t *testing.T) {
	var makerAmount, takerAmount string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody struct {
			Order OrderPayload `json:"order"`
		}
		_ = json.NewDecoder(r.Body).Decode(&reqBody)
		makerAmount = reqBody.Order.MakerAmount
		takerAmount = reqBody.Order.TakerAmount

		resp := OrderResponse{
			Success: true,
			Status:  "MATCHED",
			OrderID: "0x123",
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	originalClient := httpClient
	httpClient = server.Client()
	defer func() { httpClient = originalClient }()

	dummyPK := "0000000000000000000000000000000000000000000000000000000000000001"
	client, _ := NewCLOBClient(dummyPK, "key", "secret", "pass")
	client.BaseURL = server.URL

	// Unbalanced shares like 10.123456
	req := &OrderRequest{
		TokenID:     "123456",
		Price:       0.5,
		Size:        10.123456, // Market sell: size represents shares to sell
		Side:        SideSell,
		OrderType:   OrderTypeMarket,
		TimeInForce: TIFFillOrKill,
	}

	_, err := client.PlaceOrder(context.Background(), req)
	if err != nil {
		t.Fatalf("PlaceOrder failed: %v", err)
	}

	// Shares (Maker for SELL): 10.123456 -> 2 decimals (truncate) -> 10.12 -> 10120000 micro
	if makerAmount != "10120000" {
		t.Errorf("Expected makerAmount (shares) 10120000, got %s", makerAmount)
	}

	// USDC (Taker for SELL): 10.12 * 0.5 = 5.06 USDC
	// Round up to nearest 4 decimals = 5.06 USDC -> 5060000 micro
	if takerAmount != "5060000" {
		t.Errorf("Expected takerAmount (USDC) 5060000, got %s", takerAmount)
	}
}

func TestUsesMarketLikePrecision(t *testing.T) {
	if !usesMarketLikePrecision(&OrderRequest{OrderType: OrderTypeLimit, TimeInForce: TIFFillAndKill}) {
		t.Fatal("expected LIMIT+FAK to use market-like precision")
	}
	if !usesMarketLikePrecision(&OrderRequest{OrderType: OrderTypeLimit, TimeInForce: TIFFillOrKill}) {
		t.Fatal("expected LIMIT+FOK to use market-like precision")
	}
	if usesMarketLikePrecision(&OrderRequest{OrderType: OrderTypeLimit, TimeInForce: TIFGoodTilCancelled}) {
		t.Fatal("expected LIMIT+GTC to keep limit precision")
	}
}

func TestPlaceOrder_LimitFAKBuyUsesMarketPrecision(t *testing.T) {
	var makerAmount, takerAmount string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody struct {
			Order OrderPayload `json:"order"`
		}
		_ = json.NewDecoder(r.Body).Decode(&reqBody)
		makerAmount = reqBody.Order.MakerAmount
		takerAmount = reqBody.Order.TakerAmount

		resp := OrderResponse{Success: true, Status: "MATCHED", OrderID: "0x123"}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	originalClient := httpClient
	httpClient = server.Client()
	defer func() { httpClient = originalClient }()

	client, _ := NewCLOBClient(strings.Repeat("1", 64), "key", "secret", "pass")
	client.BaseURL = server.URL

	_, err := client.PlaceOrder(context.Background(), &OrderRequest{
		TokenID:     "123456",
		Price:       0.5,
		Size:        10.123456,
		Side:        SideBuy,
		OrderType:   OrderTypeLimit,
		TimeInForce: TIFFillAndKill,
	})
	if err != nil {
		t.Fatalf("PlaceOrder failed: %v", err)
	}

	if takerAmount != "10123400" {
		t.Fatalf("expected takerAmount (shares) 10123400, got %s", takerAmount)
	}
	if makerAmount != "5070000" {
		t.Fatalf("expected makerAmount (USDC) 5070000, got %s", makerAmount)
	}
}

func TestSendHeartbeat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/heartbeats" {
			t.Fatalf("expected path /heartbeats, got %s", r.URL.Path)
		}
		if strings.TrimSpace(r.Header.Get("POLY_API_KEY")) == "" {
			t.Fatal("expected POLY_API_KEY header")
		}
		_ = json.NewEncoder(w).Encode(HeartbeatResponse{Status: "ok"})
	}))
	defer server.Close()

	originalClient := httpClient
	httpClient = server.Client()
	defer func() { httpClient = originalClient }()

	dummyPK := "0000000000000000000000000000000000000000000000000000000000000001"
	client, err := NewCLOBClient(dummyPK, "key", "secret", "pass")
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	client.BaseURL = server.URL

	resp, err := client.SendHeartbeat(context.Background())
	if err != nil {
		t.Fatalf("SendHeartbeat failed: %v", err)
	}
	if resp.Status != "ok" {
		t.Fatalf("expected status ok, got %q", resp.Status)
	}
}

func TestGetBalanceAllowance_NormalizesBaseUnits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/balance-allowance" {
			t.Fatalf("expected /balance-allowance, got %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"balance":"68284432","allowance":"100000000"}`))
	}))
	defer server.Close()

	originalClient := httpClient
	httpClient = server.Client()
	defer func() { httpClient = originalClient }()

	client, err := NewCLOBClient(strings.Repeat("1", 64), "key", "secret", "pass")
	if err != nil {
		t.Fatalf("NewCLOBClient failed: %v", err)
	}
	client.BaseURL = server.URL

	ba, err := client.GetBalanceAllowance(context.Background())
	if err != nil {
		t.Fatalf("GetBalanceAllowance failed: %v", err)
	}

	if math.Abs(ba.Balance-68.284432) > 0.000001 {
		t.Fatalf("expected normalized balance 68.284432, got %.6f", ba.Balance)
	}
	if math.Abs(ba.Allowance-100.0) > 0.000001 {
		t.Fatalf("expected normalized allowance 100.0, got %.6f", ba.Allowance)
	}
}

func TestPlaceOrder_WritesRawDebugLogOnKilled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := OrderResponse{Success: true, Status: "KILLED", OrderID: "0x123"}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	originalClient := httpClient
	httpClient = server.Client()
	defer func() { httpClient = originalClient }()

	client, err := NewCLOBClient(strings.Repeat("1", 64), "api-key-12345678", "secret", "pass")
	if err != nil {
		t.Fatalf("NewCLOBClient failed: %v", err)
	}
	client.BaseURL = server.URL

	logPath := filepath.Join(t.TempDir(), "raw.jsonl")
	if err := client.EnableRawAPILog(logPath); err != nil {
		t.Fatalf("EnableRawAPILog failed: %v", err)
	}
	defer func() { _ = client.CloseRawAPILog() }()

	_, err = client.PlaceOrder(context.Background(), &OrderRequest{
		TokenID:     "123456",
		Price:       0.5,
		Size:        10,
		Side:        SideBuy,
		OrderType:   OrderTypeMarket,
		TimeInForce: TIFFillAndKill,
	})
	if err != nil {
		t.Fatalf("PlaceOrder failed: %v", err)
	}

	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, `"path":"/order"`) {
		t.Fatalf("expected raw log to include /order path, got %s", text)
	}
	if !strings.Contains(text, `"outcome":"KILLED"`) {
		t.Fatalf("expected raw log to include killed outcome, got %s", text)
	}
	if !strings.Contains(text, `[redacted-signature]`) {
		t.Fatalf("expected raw log to redact signature, got %s", text)
	}
	if !strings.Contains(text, `"latency_ms"`) {
		t.Fatalf("expected raw log to include latency metrics, got %s", text)
	}
}

func TestWaitForFill_IgnoresMatchedUntilConfirmed(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/order/order-1" {
			t.Fatalf("expected path /order/order-1, got %s", r.URL.Path)
		}

		status := "CONFIRMED"
		if requests.Add(1) == 1 {
			status = "MATCHED"
		}

		_ = json.NewEncoder(w).Encode(OpenOrder{
			OrderID:       "order-1",
			Status:        status,
			OriginalSize:  5,
			RemainingSize: 0,
		})
	}))
	defer server.Close()

	originalClient := httpClient
	httpClient = server.Client()
	defer func() { httpClient = originalClient }()

	dummyPK := "0000000000000000000000000000000000000000000000000000000000000001"
	client, err := NewCLOBClient(dummyPK, "key", "secret", "pass")
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	client.BaseURL = server.URL

	filled, err := client.WaitForFill(context.Background(), "order-1", 350*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForFill failed: %v", err)
	}
	if !filled {
		t.Fatal("expected fill once order reached CONFIRMED")
	}
	if requests.Load() < 2 {
		t.Fatalf("expected WaitForFill to keep polling after MATCHED, got %d requests", requests.Load())
	}
}

func TestWaitForFill_FailedReturnsNotFilled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(OpenOrder{
			OrderID:       "order-1",
			Status:        "FAILED",
			OriginalSize:  5,
			RemainingSize: 0,
		})
	}))
	defer server.Close()

	originalClient := httpClient
	httpClient = server.Client()
	defer func() { httpClient = originalClient }()

	dummyPK := "0000000000000000000000000000000000000000000000000000000000000001"
	client, err := NewCLOBClient(dummyPK, "key", "secret", "pass")
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	client.BaseURL = server.URL

	filled, err := client.WaitForFill(context.Background(), "order-1", 200*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForFill failed: %v", err)
	}
	if filled {
		t.Fatal("expected FAILED order to return not filled")
	}
}

func TestGetOrder_NotFoundDoesNotPretendFilled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()

	originalClient := httpClient
	httpClient = server.Client()
	defer func() { httpClient = originalClient }()

	dummyPK := "0000000000000000000000000000000000000000000000000000000000000001"
	client, err := NewCLOBClient(dummyPK, "key", "secret", "pass")
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	client.BaseURL = server.URL

	order, err := client.GetOrder(context.Background(), "order-404")
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if order.Status != "NOT_FOUND" {
		t.Fatalf("expected NOT_FOUND status for 404, got %q", order.Status)
	}
}

func TestWaitForFill_NotFoundReturnsNotFilled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()

	originalClient := httpClient
	httpClient = server.Client()
	defer func() { httpClient = originalClient }()

	dummyPK := "0000000000000000000000000000000000000000000000000000000000000001"
	client, err := NewCLOBClient(dummyPK, "key", "secret", "pass")
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	client.BaseURL = server.URL

	filled, err := client.WaitForFill(context.Background(), "order-404", 200*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForFill failed: %v", err)
	}
	if filled {
		t.Fatal("expected NOT_FOUND order to return not filled")
	}
}
