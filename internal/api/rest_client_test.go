package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

func TestGetGammaMarketBySlugMarksResolvedWinner(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/markets" || r.URL.Query().Get("slug") != "btc-updown-1h-1700000000" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"conditionId":"cond-btc-1","slug":"btc-updown-1h-1700000000","endDate":"2026-05-13T10:00:00Z","clobTokenIds":"[\"down-token\",\"up-token\"]","outcomes":"[\"Down\",\"Up\"]","outcomePrices":"[\"0\",\"1\"]","umaResolutionStatus":"resolved","active":false,"closed":true}]`))
	}))
	defer server.Close()

	client := NewRestClient("polymarket")
	client.GammaURL = server.URL
	market, err := client.GetGammaMarketBySlug(context.Background(), "btc-updown-1h-1700000000")
	if err != nil {
		t.Fatalf("expected gamma market lookup to succeed, got %v", err)
	}
	if market.ConditionID != "cond-btc-1" || market.Slug != "btc-updown-1h-1700000000" {
		t.Fatalf("unexpected gamma market: %+v", market)
	}
	if len(market.Tokens) != 2 || market.Tokens[0].Winner || !market.Tokens[1].Winner {
		t.Fatalf("expected Up winner from resolved outcome prices, got %+v", market.Tokens)
	}
}

func TestGetGammaMarketBySlugMarksProposedHourlyCryptoFinalAfterCustomLiveness(t *testing.T) {
	endDate := time.Now().Add(-11 * time.Minute).UTC().Format(time.RFC3339)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/markets" || r.URL.Query().Get("slug") != "bitcoin-up-or-down-may-14-2026-10am-et" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"conditionId":"cond-btc-1","slug":"bitcoin-up-or-down-may-14-2026-10am-et","endDate":"` + endDate + `","clobTokenIds":"[\"up-token\",\"down-token\"]","outcomes":"[\"Up\",\"Down\"]","outcomePrices":"[\"0.9995\",\"0.0005\"]","umaResolutionStatus":"proposed","customLiveness":600,"active":true,"closed":false,"events":[{"eventMetadata":{"finalPrice":80964.85,"priceToBeat":79701.73}}]}]`))
	}))
	defer server.Close()

	client := NewRestClient("polymarket")
	client.GammaURL = server.URL
	market, err := client.GetGammaMarketBySlug(context.Background(), "bitcoin-up-or-down-may-14-2026-10am-et")
	if err != nil {
		t.Fatalf("expected gamma market lookup to succeed, got %v", err)
	}
	if !market.Closed {
		t.Fatalf("expected proposed custom-liveness market to be treated as closed/final, got %+v", market)
	}
	if len(market.Tokens) != 2 || !market.Tokens[0].Winner || market.Tokens[1].Winner {
		t.Fatalf("expected Up winner from finalPrice >= priceToBeat, got %+v", market.Tokens)
	}
}

func TestGetGammaMarketBySlugDoesNotMarkProposedBeforeCustomLiveness(t *testing.T) {
	endDate := time.Now().Add(-9 * time.Minute).UTC().Format(time.RFC3339)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/markets" || r.URL.Query().Get("slug") != "bitcoin-up-or-down-may-14-2026-10am-et" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"conditionId":"cond-btc-1","slug":"bitcoin-up-or-down-may-14-2026-10am-et","endDate":"` + endDate + `","clobTokenIds":"[\"up-token\",\"down-token\"]","outcomes":"[\"Up\",\"Down\"]","outcomePrices":"[\"0.9995\",\"0.0005\"]","umaResolutionStatus":"proposed","customLiveness":600,"active":true,"closed":false,"events":[{"eventMetadata":{"finalPrice":80964.85,"priceToBeat":79701.73}}]}]`))
	}))
	defer server.Close()

	client := NewRestClient("polymarket")
	client.GammaURL = server.URL
	market, err := client.GetGammaMarketBySlug(context.Background(), "bitcoin-up-or-down-may-14-2026-10am-et")
	if err != nil {
		t.Fatalf("expected gamma market lookup to succeed, got %v", err)
	}
	if market.Closed {
		t.Fatalf("expected proposed market inside custom liveness to remain unresolved, got %+v", market)
	}
	if len(market.Tokens) != 2 || market.Tokens[0].Winner || market.Tokens[1].Winner {
		t.Fatalf("expected no winner before custom liveness elapses, got %+v", market.Tokens)
	}
}

func TestResolutionCacheUsesGammaConditionFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/markets/cond-btc-1" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.URL.Path != "/markets" || r.URL.Query().Get("condition_ids") != "cond-btc-1" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"conditionId":"cond-btc-1","slug":"btc-updown-1h-1700000000","clobTokenIds":"[\"down-token\",\"up-token\"]","outcomes":"[\"Down\",\"Up\"]","outcomePrices":"[\"0\",\"1\"]","umaResolutionStatus":"resolved","active":false,"closed":true}]`))
	}))
	defer server.Close()

	client := NewRestClient("polymarket")
	client.BaseURL = server.URL
	client.GammaURL = server.URL
	cache := NewResolutionCache(nil, nil, client)

	status := cache.GetResolution(context.Background(), "cond-btc-1", []string{"Down", "Up"}, time.Now().Add(-time.Minute))
	if !status.Resolved || status.Winner != "Up" {
		t.Fatalf("expected Gamma condition fallback to resolve Up, got %+v", status)
	}
}

type marketInfoOnlyExchange struct {
	ExchangeClient
	info *MarketInfo
}

func (s marketInfoOnlyExchange) GetMarketInfo(ctx context.Context, conditionID string) (*MarketInfo, error) {
	return s.info, nil
}

func TestResolutionCacheUsesGammaProposedFinalWhenClobHasNoWinner(t *testing.T) {
	endDate := time.Now().Add(-11 * time.Minute).UTC().Format(time.RFC3339)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/markets/cond-btc-1" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.URL.Path != "/markets" || r.URL.Query().Get("condition_ids") != "cond-btc-1" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"conditionId":"cond-btc-1","slug":"bitcoin-up-or-down-may-14-2026-10am-et","endDate":"` + endDate + `","clobTokenIds":"[\"up-token\",\"down-token\"]","outcomes":"[\"Up\",\"Down\"]","outcomePrices":"[\"0.9995\",\"0.0005\"]","umaResolutionStatus":"proposed","customLiveness":600,"active":true,"closed":false,"events":[{"eventMetadata":{"finalPrice":80964.85,"priceToBeat":79701.73}}]}]`))
	}))
	defer server.Close()

	client := NewRestClient("polymarket")
	client.BaseURL = server.URL
	client.GammaURL = server.URL
	clob := marketInfoOnlyExchange{info: &MarketInfo{
		ConditionID: "cond-btc-1",
		Closed:      false,
		Tokens: []struct {
			TokenID string      `json:"token_id"`
			Outcome string      `json:"outcome"`
			Winner  bool        `json:"winner"`
			Price   interface{} `json:"price"`
		}{
			{TokenID: "up-token", Outcome: "Up"},
			{TokenID: "down-token", Outcome: "Down"},
		},
	}}
	cache := NewResolutionCache(nil, clob, client)

	status := cache.GetResolution(context.Background(), "cond-btc-1", []string{"Up", "Down"}, time.Now().Add(-time.Minute))
	if !status.Resolved || status.Winner != "Up" {
		t.Fatalf("expected Gamma proposed-final fallback to resolve Up, got %+v", status)
	}
}

func TestNewRestClientDefault(t *testing.T) {
	client := NewRestClient("")
	if client.BaseURL != "https://clob.polymarket.com" {
		t.Errorf("Expected default BaseURL, got %s", client.BaseURL)
	}
}

func TestGetFeeRateParsesBaseFee(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fee-rate" {
			t.Fatalf("expected /fee-rate path, got %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("token_id"); got != "token-down" {
			t.Fatalf("expected token_id query, got %q", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"base_fee":1000}`))
	}))
	defer server.Close()

	client := NewRestClient("")
	client.BaseURL = server.URL

	got, err := client.GetFeeRate(context.Background(), "token-down")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got != 1000 {
		t.Fatalf("expected 1000 bps, got %d", got)
	}
}

func TestGetFeeRateParsesLegacyFeeRateBps(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"fee_rate_bps":30}`))
	}))
	defer server.Close()

	client := NewRestClient("")
	client.BaseURL = server.URL

	got, err := client.GetFeeRate(context.Background(), "token-up")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got != 30 {
		t.Fatalf("expected 30 bps, got %d", got)
	}
}

func TestGetFeeRateRejectsUnknownObject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"unexpected_fee":1000}`))
	}))
	defer server.Close()

	client := NewRestClient("")
	client.BaseURL = server.URL

	_, err := client.GetFeeRate(context.Background(), "token-up")
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !strings.Contains(err.Error(), "missing base_fee or fee_rate_bps") {
		t.Fatalf("expected missing fee field error, got %v", err)
	}
}

func TestGetClobMarketInfo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/clob-markets/cond-1" {
			t.Fatalf("expected /clob-markets/cond-1, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"c":"cond-1",
			"t":[{"t":"token-yes","o":"Yes"},{"t":"token-no","o":"No"}],
			"mts":0.01,
			"nr":true,
			"fd":{"r":0.05,"e":1}
		}`))
	}))
	defer server.Close()

	client := NewRestClient("")
	client.BaseURL = server.URL

	info, err := client.GetClobMarketInfo(context.Background(), "cond-1")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if info.ConditionID != "cond-1" || !info.NegRisk || info.FeeDetails == nil || info.FeeDetails.Rate != 0.05 {
		t.Fatalf("unexpected clob market info %+v", info)
	}
	if len(info.Tokens) != 2 || info.Tokens[0].TokenID != "token-yes" {
		t.Fatalf("unexpected tokens %+v", info.Tokens)
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

func TestGetMarketsByTimeframeSupportsOneHourWindows(t *testing.T) {
	var requested []string
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slug := r.URL.Query().Get("slug")
		mu.Lock()
		requested = append(requested, slug)
		mu.Unlock()

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

	markets, err := client.GetMarketsByTimeframe(context.Background(), []string{"btc"}, "1h")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(markets) != 7 {
		t.Fatalf("expected 7 markets from 1 asset across 7 one-hour windows, got %d", len(markets))
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requested) != 7 {
		t.Fatalf("expected 7 one-hour slug lookups, got %d", len(requested))
	}
	for _, slug := range requested {
		if !strings.HasPrefix(slug, "bitcoin-up-or-down-") || !strings.HasSuffix(slug, "-et") {
			t.Fatalf("expected human-readable one-hour slug lookup, got %q", slug)
		}
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

func TestGetOrderBookRetriesTransientStatus(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := atomic.AddInt32(&attempts, 1)
		if r.URL.Path != "/book" {
			t.Fatalf("expected /book path, got %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("token_id"); got != "up-token" {
			t.Fatalf("expected token_id query, got %q", got)
		}
		if attempt == 1 {
			http.Error(w, "slow down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"asset_id":"up-token","bids":[{"price":"0.41","size":"10"}],"asks":[{"price":"0.43","size":"11"}],"timestamp":"1710000000123"}`))
	}))
	defer server.Close()

	client := NewRestClient("")
	client.BaseURL = server.URL
	book, err := client.GetOrderBook(context.Background(), "up-token")
	if err != nil {
		t.Fatalf("expected retry to succeed, got %v", err)
	}
	if atomic.LoadInt32(&attempts) != 2 {
		t.Fatalf("expected two attempts, got %d", atomic.LoadInt32(&attempts))
	}
	if len(book.Bids) != 1 || book.Bids[0].Price != "0.41" || len(book.Asks) != 1 || book.Asks[0].Price != "0.43" {
		t.Fatalf("unexpected order book %+v", book)
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

func TestGetMarketsByTimeframe_XRP_and_1D_Candidates(t *testing.T) {
	var requestedSlugs []string
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slug := r.URL.Query().Get("slug")
		mu.Lock()
		requestedSlugs = append(requestedSlugs, slug)
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	client := NewRestClient("")
	client.GammaURL = server.URL

	// 1. Verify XRP 1h timeframes query both hourly (xrp-up-or-down) and legacy (ripple-updown / xrp-updown)
	_, err := client.GetMarketsByTimeframe(context.Background(), []string{"xrp"}, "1h")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	mu.Lock()
	xrp1hSlugs := make([]string, len(requestedSlugs))
	copy(xrp1hSlugs, requestedSlugs)
	requestedSlugs = nil
	mu.Unlock()

	hasXRPHourly := false
	hasRippleLegacy := false
	for _, slug := range xrp1hSlugs {
		if strings.Contains(slug, "xrp-up-or-down-") {
			hasXRPHourly = true
		}
		if strings.Contains(slug, "ripple-updown-1h-") {
			hasRippleLegacy = true
		}
	}
	if !hasXRPHourly {
		t.Errorf("expected XRP 1h to query hourly event slug candidates starting with 'xrp-up-or-down-', got %v", xrp1hSlugs)
	}
	if !hasRippleLegacy {
		t.Errorf("expected XRP 1h to query legacy candidates starting with 'ripple-updown-1h-', got %v", xrp1hSlugs)
	}

	// 2. Verify 1d timeframe queries both lowercase '1d' and uppercase '1D' legacy slugs
	_, err = client.GetMarketsByTimeframe(context.Background(), []string{"btc"}, "1d")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	mu.Lock()
	btc1dSlugs := requestedSlugs
	mu.Unlock()

	has1dLower := false
	has1dUpper := false
	has24h := false
	for _, slug := range btc1dSlugs {
		if strings.Contains(slug, "-updown-1d-") {
			has1dLower = true
		}
		if strings.Contains(slug, "-updown-1D-") {
			has1dUpper = true
		}
		if strings.Contains(slug, "-updown-24h-") {
			has24h = true
		}
	}
	if !has1dLower {
		t.Errorf("expected 1d to query lowercase '1d' slug candidates, got %v", btc1dSlugs)
	}
	if !has1dUpper {
		t.Errorf("expected 1d to query uppercase '1D' slug candidates, got %v", btc1dSlugs)
	}
	if !has24h {
		t.Errorf("expected 1d to query '24h' slug candidates, got %v", btc1dSlugs)
	}
}
