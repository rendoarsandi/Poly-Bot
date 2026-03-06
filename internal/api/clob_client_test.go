package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func TestPlaceOrders_FAK_Killed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orders" {
			t.Errorf("Expected path /orders, got %s", r.URL.Path)
		}
		resp := []OrderResponse{{
			Success: true,
			Status:  "KILLED",
			OrderID: "0xabc",
		}}
		_ = json.NewEncoder(w).Encode(resp)
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
