package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

type rewriteRoundTripper struct {
	base   http.RoundTripper
	target *url.URL
}

func (r rewriteRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = r.target.Scheme
	clone.URL.Host = r.target.Host
	return r.base.RoundTrip(clone)
}

func TestRealbotHandleCopytradeMarket_RESTFallback(t *testing.T) {
	// 1. Setup mock Server for REST Client
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/trades":
			w.Write([]byte(`[{"conditionId":"cond-1","outcome":"Up","side":"BUY","size":10.5,"price":0.45,"asset":"asset-up","timestamp":1700000000,"transactionHash":"0xabc"}]`))
		case "/positions":
			w.Write([]byte(`[]`))
		case "/book":
			w.Write([]byte(`{"bids":[{"price":"0.44","size":"100"}],"asks":[{"price":"0.46","size":"100"}]}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer server.Close()

	targetURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}

	// Override default httpClient
	originalClient := api.GetHTTPClientForTesting()
	api.SetHTTPClientForTesting(&http.Client{
		Transport: rewriteRoundTripper{
			base:   server.Client().Transport,
			target: targetURL,
		},
	})
	defer api.SetHTTPClientForTesting(originalClient)

	restClient := api.NewRestClient("polymarket")

	// 2. Setup strategy dependencies
	engine := paper.NewEngine(100.0)
	cfg := &core.Config{
		ExecutionBackend: core.ExecutionBackendPaper,
	}
	trader := trading.NewEmbeddedPaperRealTrader(cfg, engine)
	tui := paper.NewTUI(engine, nil)

	trader.RegisterPaperToken("token-up", "market-1", "Up")
	trader.RegisterPaperToken("token-down", "market-1", "Down")

	market := &api.Market{
		ConditionID: "cond-1",
		Slug:        "test-market",
		Tokens: []api.Token{
			{TokenID: "token-up", Outcome: "Up"},
			{TokenID: "token-down", Outcome: "Down"},
		},
	}

	outcomes := []string{"Up", "Down"}
	tokenBids := map[string]float64{"Up": 0.44, "Down": 0.54}
	tokenAsks := map[string]float64{"Up": 0.46, "Down": 0.56}
	tokenFullBids := map[string][]paper.MarketLevel{"Up": {{Price: 0.44, Size: 100}}}
	tokenFullAsks := map[string][]paper.MarketLevel{"Up": {{Price: 0.46, Size: 100}}}
	quoteState := map[string]realbotQuoteState{
		"Up":   {UpdatedAt: time.Now()},
		"Down": {UpdatedAt: time.Now()},
	}

	liveCfg := paper.TUISettings{
		PaperArbMode:            "copytrade",
		CopytradeWatcherMode:    "public-api",
		CopytradePollIntervalMs: 100,
		CopytradeSizeUSDC:       10.0,
		CopytradeSizingMode:     "usdc",
		CopytradeMaxSlippagePct: 0.05,
		MaxAskPrice:             0.99,
		MinAskPrice:             0.01,
	}

	// Create a poller that does NOT have an onchain watcher, triggering the REST fallback
	poller := newRealbotCopytradePoller("0x1111111111111111111111111111111111111111", []string{"cond-1"})
	state := newRealbotCopytradeState()
	state.startedAt = time.Unix(1699999000, 0) // Start before the trade timestamp
	entryGate := newRealbotEntryGate()

	// Call the strategy logic
	realbotHandleCopytradeMarket(
		context.Background(),
		"market-1",
		market,
		outcomes,
		tokenBids,
		tokenAsks,
		tokenFullBids,
		tokenFullAsks,
		quoteState,
		map[string]int{"Up": 0, "Down": 0},
		trader,
		engine,
		tui,
		restClient,
		liveCfg,
		poller,
		state,
		entryGate,
		func(d time.Duration) {},
	)

	t.Logf("State last error: %q", state.lastError)
	t.Logf("State retry trades count: %d, retry trades: %+v", len(state.retryTrades), state.retryTrades)

	// Check if the position was created
	positions := engine.GetPositions()
	if len(positions) == 0 {
		t.Fatal("expected at least one position to be opened in REST fallback mode")
	}

	pos, ok := positions["market-1:Up"]
	if !ok {
		t.Fatalf("expected position for key market-1:Up, got positions: %+v", positions)
	}

	if pos.Quantity <= 0 {
		t.Fatalf("expected positive quantity in position, got %.4f", pos.Quantity)
	}
}
