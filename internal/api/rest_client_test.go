package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetMarket(t *testing.T) {
	// Mock Polymarket API
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"active":true,"condition_id":"test-condition","slug":"test-market","tokens":[{"token_id":"yes-token","outcome":"Yes"},{"token_id":"no-token","outcome":"No"}]}`))
	}))
	defer server.Close()

	client := NewRestClient("")
	client.BaseURL = server.URL
	client.GammaURL = server.URL
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
		_, _ = w.Write([]byte(`{
	                "data": [				{"market_slug": "market-1", "active": true, "closed": false},
				{"market_slug": "market-2", "active": true, "closed": false}
			]
		}`))
	}))
	defer server.Close()

	client := NewRestClient("")
	client.BaseURL = server.URL
	client.GammaURL = server.URL
	markets, err := client.ListMarkets(context.Background())
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	if len(markets) != 2 {
		t.Errorf("Expected 2 markets, got %d", len(markets))
	}
}

func TestGetMarketsByEventSlug(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			t.Fatalf("expected /events path, got %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("slug"); got != "btc-updown-15m-123" {
			t.Fatalf("expected slug query, got %q", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[
			{"slug":"btc-updown-15m-123","markets":[{"conditionId":"cond-1","slug":"btc-updown-15m-123","clobTokenIds":"[\"yes-token\",\"no-token\"]","outcomes":"[\"Up\",\"Down\"]","outcomePrices":"[\"1\",\"0\"]","umaResolutionStatus":"resolved","active":true,"closed":true}]}
		]`))
	}))
	defer server.Close()

	client := NewRestClient("")
	client.BaseURL = server.URL
	client.GammaURL = server.URL
	client.GammaURL = server.URL

	markets, err := client.GetMarketsByEventSlug(context.Background(), "btc-updown-15m-123")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(markets) != 1 {
		t.Fatalf("expected 1 market, got %d", len(markets))
	}
	if markets[0].ConditionID != "cond-1" {
		t.Fatalf("expected cond-1, got %s", markets[0].ConditionID)
	}
	if len(markets[0].Tokens) != 2 || markets[0].Tokens[0].TokenID != "yes-token" {
		t.Fatalf("unexpected market tokens: %+v", markets[0].Tokens)
	}
	if !markets[0].Tokens[0].Winner || markets[0].Tokens[1].Winner {
		t.Fatalf("unexpected winner flags: %+v", markets[0].Tokens)
	}
}

func TestGetMarketsByTimeframeFetchesSlugsConcurrently(t *testing.T) {
	var inFlight int32
	var maxInFlight int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := atomic.AddInt32(&inFlight, 1)
		for {
			prev := atomic.LoadInt32(&maxInFlight)
			if current <= prev || atomic.CompareAndSwapInt32(&maxInFlight, prev, current) {
				break
			}
		}
		defer atomic.AddInt32(&inFlight, -1)

		time.Sleep(40 * time.Millisecond)
		slug := r.URL.Query().Get("slug")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{
			"slug":"` + slug + `",
			"endDate":"2026-03-08T12:34:56Z",
			"markets":[{
				"conditionId":"` + slug + `",
				"slug":"` + slug + `",
				"clobTokenIds":"[\"yes-token\",\"no-token\"]",
				"outcomes":"[\"Yes\",\"No\"]",
				"active":true,
				"closed":false
			}]
		}]`))
	}))
	defer server.Close()

	client := NewRestClient("")
	client.GammaURL = server.URL

	markets, err := client.GetMarketsByTimeframe(context.Background(), []string{"btc", "eth"}, "15m")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(markets) != 14 {
		t.Fatalf("expected 14 markets from 2 assets across 7 windows, got %d", len(markets))
	}
	if atomic.LoadInt32(&maxInFlight) < 2 {
		t.Fatalf("expected concurrent slug fetches, max in flight was %d", maxInFlight)
	}
}

func TestParseOrderBookTimestamp(t *testing.T) {
	msTs, err := ParseOrderBookTimestamp("1710000000123")
	if err != nil {
		t.Fatalf("expected millisecond timestamp to parse, got %v", err)
	}
	if msTs.UnixMilli() != 1710000000123 {
		t.Fatalf("unexpected millisecond timestamp %d", msTs.UnixMilli())
	}

	rfcTs, err := ParseOrderBookTimestamp("2026-03-08T12:34:56.789Z")
	if err != nil {
		t.Fatalf("expected RFC3339 timestamp to parse, got %v", err)
	}
	if rfcTs.UTC().Format(time.RFC3339Nano) != "2026-03-08T12:34:56.789Z" {
		t.Fatalf("unexpected RFC timestamp %s", rfcTs.UTC().Format(time.RFC3339Nano))
	}
}

func TestOrderBookAgeAt(t *testing.T) {
	now := time.UnixMilli(1710000000500)
	book := &OrderBookResponse{Timestamp: "1710000000123"}
	age, err := OrderBookAgeAt(book, now)
	if err != nil {
		t.Fatalf("expected age calculation to succeed, got %v", err)
	}
	if age != 377*time.Millisecond {
		t.Fatalf("unexpected age %v", age)
	}

	if _, err := OrderBookAgeAt(&OrderBookResponse{Timestamp: "bad"}, now); err == nil {
		t.Fatal("expected invalid timestamp to fail")
	}
}

func TestGetCLOBBidAskFetchesOrderBooksConcurrently(t *testing.T) {
	var inflight int32
	var maxInflight int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := atomic.AddInt32(&inflight, 1)
		for {
			observed := atomic.LoadInt32(&maxInflight)
			if current <= observed || atomic.CompareAndSwapInt32(&maxInflight, observed, current) {
				break
			}
		}
		defer atomic.AddInt32(&inflight, -1)
		time.Sleep(25 * time.Millisecond)

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("token_id") {
		case "up-token":
			_, _ = w.Write([]byte(`{"asset_id":"up-token","bids":[{"price":"0.41","size":"10"}],"asks":[{"price":"0.43","size":"11"}]}`))
		case "down-token":
			_, _ = w.Write([]byte(`{"asset_id":"down-token","bids":[{"price":"0.57","size":"8"}],"asks":[{"price":"0.59","size":"9"}]}`))
		default:
			http.Error(w, "unexpected token", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewRestClient("")
	client.BaseURL = server.URL
	client.GammaURL = server.URL
	prices, err := client.GetCLOBBidAsk(context.Background(), map[string]string{
		"up-token":   "Up",
		"down-token": "Down",
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if prices["Up"].Bid != 0.41 || prices["Up"].Ask != 0.43 {
		t.Fatalf("unexpected Up price %+v", prices["Up"])
	}
	if prices["Down"].Bid != 0.57 || prices["Down"].Ask != 0.59 {
		t.Fatalf("unexpected Down price %+v", prices["Down"])
	}
	if atomic.LoadInt32(&maxInflight) < 2 {
		t.Fatalf("expected concurrent order book fetches, max inflight=%d", atomic.LoadInt32(&maxInflight))
	}
}
