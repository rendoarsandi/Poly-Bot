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

func TestRealbotLoadMarketFeeRatesUsesConfiguredRateForEmbeddedPaper(t *testing.T) {
	cfg := &core.Config{
		ExecutionBackend: core.ExecutionBackendPaper,
		// FeeRateBps removed
	}
	engine := paper.NewEngine(100)
	engine.SetFeeRateBps(312)
	trader := trading.NewEmbeddedPaperRealTrader(cfg, engine)
	tui := paper.NewTUI(engine, nil)
	restClient := api.NewRestClient("polymarket")

	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("1000"))
	}))
	defer server.Close()
	restClient.BaseURL = server.URL

	got := realbotLoadMarketFeeRates(context.Background(), "BTC", nil, restClient, map[string]string{
		"token-up":   "Up",
		"token-down": "Down",
	}, cfg, trader, tui)

	if atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("expected embedded paper mode to skip remote fee fetch, got %d hits", hits)
	}
	if got["Up"] != 3 || got["Down"] != 3 {
		t.Fatalf("expected configured paper fee rate for both outcomes, got %+v", got)
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
			_, _ = w.Write([]byte(`{"c":"cond-1","t":[{"t":"token-up","o":"Up"},{"t":"token-down","o":"Down"}],"mts":0.01,"nr":false,"fd":{"r":42,"e":6}}`))
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
	if got["Up"] != 0 || got["Down"] != 0 {
		t.Fatalf("expected live V2 order fee bps to stay zero, got %+v", got)
	}
}
