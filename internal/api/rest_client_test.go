package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetMarket(t *testing.T) {
	// Mock Polymarket API
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"active":true,"condition_id":"test-condition","slug":"test-market","tokens":[{"token_id":"yes-token","outcome":"Yes"},{"token_id":"no-token","outcome":"No"}]}`))
	}))
	defer server.Close()

	client := NewRestClient(server.URL)
	market, err := client.GetMarket(context.Background(), "test-market")
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	if market.ConditionID != "test-condition" {
		t.Errorf("Expected ConditionID 'test-condition', got %s", market.ConditionID)
	}

	if len(market.Tokens) != 2 {
		t.Errorf("Expected 2 tokens, got %d", len(market.Tokens))
	}
}

func TestNewRestClientDefault(t *testing.T) {
	client := NewRestClient("")
	if client.BaseURL != "https://clob.polymarket.com" {
		t.Errorf("Expected default BaseURL, got %s", client.BaseURL)
	}
}

func TestListMarkets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"data": [
				{"market_slug": "market-1", "active": true, "closed": false},
				{"market_slug": "market-2", "active": true, "closed": false}
			]
		}`))
	}))
	defer server.Close()

	client := NewRestClient(server.URL)
	markets, err := client.ListMarkets(context.Background())
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	if len(markets) != 2 {
		t.Errorf("Expected 2 markets, got %d", len(markets))
	}
}

func TestGetNegRisk(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/book":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"market":"condition-1","asset_id":"token-1","timestamp":"0","bids":[],"asks":[]}`))
		case r.URL.Path == "/markets":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`[
				{
					"condition_id":"condition-1",
					"neg_risk": true,
					"neg_risk_market_id": "neg-market-1",
					"neg_risk_exchange_address": "0xabc",
					"tokens": [{"token_id":"token-1","neg_risk": true}]
				}
			]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewRestClient(server.URL)
	client.GammaURL = server.URL

	info, err := client.GetNegRisk(context.Background(), "token-1")
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	if !info.NegRisk {
		t.Fatalf("Expected neg-risk token")
	}
	if info.ExchangeAddress != "0xabc" {
		t.Fatalf("Expected exchange address 0xabc, got %s", info.ExchangeAddress)
	}
	if info.NegRiskMarketID != "neg-market-1" {
		t.Fatalf("Expected neg-risk market id neg-market-1, got %s", info.NegRiskMarketID)
	}
}
