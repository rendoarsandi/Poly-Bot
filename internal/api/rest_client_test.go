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

func TestGetRecentUpDownMarketsFiltersAndParses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/markets" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[
			{"conditionId":"cond-1","slug":"eth-updown-15m-123","clobTokenIds":"[\"1\",\"2\"]","outcomes":"[\"Up\",\"Down\"]","active":false,"closed":true},
			{"conditionId":"cond-2","slug":"btc-updown-15m-456","clobTokenIds":"[\"3\",\"4\"]","outcomes":"[\"Up\",\"Down\"]","active":true,"closed":false},
			{"conditionId":"cond-3","slug":"other-market","clobTokenIds":"[\"5\",\"6\"]","outcomes":"[\"Yes\",\"No\"]","active":true,"closed":false}
		]`))
	}))
	defer server.Close()

	client := NewRestClient("https://clob.polymarket.com")
	client.GammaURL = server.URL

	markets, err := client.GetRecentUpDownMarkets(context.Background(), []string{"eth"}, 100, 1)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	if len(markets) != 1 {
		t.Fatalf("Expected 1 market, got %d", len(markets))
	}
	if markets[0].ConditionID != "cond-1" {
		t.Fatalf("Expected condition cond-1, got %s", markets[0].ConditionID)
	}
	if len(markets[0].Tokens) != 2 || markets[0].Tokens[0].TokenID != "1" {
		t.Fatalf("Expected parsed token IDs, got %+v", markets[0].Tokens)
	}
}
