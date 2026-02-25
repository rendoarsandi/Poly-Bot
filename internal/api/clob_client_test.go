package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		json.NewEncoder(w).Encode(resp)
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
			OrderID:  "0x123",
		}
		json.NewEncoder(w).Encode(resp)
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

func TestPlaceOrder_MarketSellPrecision(t *testing.T) {
	var makerAmount, takerAmount string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody struct {
			Order OrderPayload `json:"order"`
		}
		json.NewDecoder(r.Body).Decode(&reqBody)
		makerAmount = reqBody.Order.MakerAmount
		takerAmount = reqBody.Order.TakerAmount

		resp := OrderResponse{
			Success: true,
			Status:  "MATCHED",
			OrderID: "0x123",
		}
		json.NewEncoder(w).Encode(resp)
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

	// Shares (Maker for SELL): 10.123456 -> 4 decimals -> 10.1234 -> 10123400 micro
	if makerAmount != "10123400" {
		t.Errorf("Expected makerAmount (shares) 10123400, got %s", makerAmount)
	}

	// USDC (Taker for SELL): 10.1234 * 0.5 = 5.0617 USDC
	// Round up to nearest 2 decimals = 5.07 USDC -> 5070000 micro
	if takerAmount != "5070000" {
		t.Errorf("Expected takerAmount (USDC) 5070000, got %s", takerAmount)
	}
}
