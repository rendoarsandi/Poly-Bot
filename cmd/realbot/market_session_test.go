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
		FeeRateBps:       312,
	}
	engine := paper.NewEngine(100)
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

	got := realbotLoadMarketFeeRates(context.Background(), "BTC", restClient, map[string]string{
		"token-up":   "Up",
		"token-down": "Down",
	}, cfg, trader, tui)

	if atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("expected embedded paper mode to skip remote fee fetch, got %d hits", hits)
	}
	if got["Up"] != 312 || got["Down"] != 312 {
		t.Fatalf("expected configured paper fee rate for both outcomes, got %+v", got)
	}
}
