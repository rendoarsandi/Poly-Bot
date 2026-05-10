package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

func TestRealbotLoadMarketFeeRatesUsesClobFeeCurveForEmbeddedPaper(t *testing.T) {
	cfg := &core.Config{
		ExecutionBackend: core.ExecutionBackendPaper,
	}
	engine := paper.NewEngine(100)
	trader := trading.NewEmbeddedPaperRealTrader(cfg, engine)
	trader.RegisterPaperToken("token-up", "BTC", "Up")
	tui := paper.NewTUI(engine, nil)
	restClient := api.NewRestClient("polymarket")

	var clobMarketHits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/clob-markets/cond-1" {
			t.Fatalf("expected /clob-markets/cond-1, got %s", r.URL.Path)
		}
		atomic.AddInt32(&clobMarketHits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"c":"cond-1","t":[{"t":"token-up","o":"Up"},{"t":"token-down","o":"Down"}],"mts":0.01,"nr":false,"fd":{"r":0.05,"e":1,"to":true}}`))
	}))
	defer server.Close()
	restClient.BaseURL = server.URL

	got := realbotLoadMarketFeeRates(context.Background(), "BTC", &api.Market{ConditionID: "cond-1"}, restClient, map[string]string{
		"token-up":   "Up",
		"token-down": "Down",
	}, cfg, trader, tui)

	if atomic.LoadInt32(&clobMarketHits) != 1 {
		t.Fatalf("expected one clob-market lookup, got %d", clobMarketHits)
	}
	if got["Up"] != 500 || got["Down"] != 500 {
		t.Fatalf("expected clob fee curve rate for both outcomes, got %+v", got)
	}
	buy, err := trader.Buy(context.Background(), "token-up", "Up", 0.50, 1.02, api.OrderTypeLimit, api.TIFGoodTilCancelled, 3)
	if err != nil {
		t.Fatalf("embedded paper buy failed: %v", err)
	}
	if !buy.Success {
		t.Fatalf("embedded paper buy should succeed: %+v", buy)
	}
	if buy.AcknowledgedQty >= 1.0 {
		t.Fatalf("expected registered 5%% theta curve to deduct realistic shares, got %.6f", buy.AcknowledgedQty)
	}
}

func TestRealbotLoadMarketFeeRatesLiveSkipsManualFeeRates(t *testing.T) {
	cfg := &core.Config{}
	tui := paper.NewTUI(paper.NewEngine(100), nil)
	restClient := api.NewRestClient("polymarket")

	var clobMarketHits int32
	var feeRateHits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/clob-markets/cond-1":
			atomic.AddInt32(&clobMarketHits, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"c":"cond-1","t":[{"t":"token-up","o":"Up"},{"t":"token-down","o":"Down"}],"mts":0.01,"nr":false,"fd":{"r":0.05,"e":1}}`))
		case "/fee-rate":
			atomic.AddInt32(&feeRateHits, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`1000`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	restClient.BaseURL = server.URL

	got := realbotLoadMarketFeeRates(context.Background(), "BTC", &api.Market{ConditionID: "cond-1"}, restClient, map[string]string{
		"token-up":   "Up",
		"token-down": "Down",
	}, cfg, nil, tui)

	if atomic.LoadInt32(&clobMarketHits) != 1 {
		t.Fatalf("expected one clob-market lookup, got %d", clobMarketHits)
	}
	if atomic.LoadInt32(&feeRateHits) != 0 {
		t.Fatalf("expected fee-rate fallback to be skipped, got %d hits", feeRateHits)
	}
	if got["Up"] != 500 || got["Down"] != 500 {
		t.Fatalf("expected fee rates to be available for local simulation/risk math, got %+v", got)
	}
}
