package main

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

func TestRealbotInitBackendPaperMode(t *testing.T) {
	state, err := realbotInitBackend(context.Background(), &core.Config{
		ExecutionBackend: core.ExecutionBackendPaper,
		PaperBalance:     55.5,
		PolygonRPCURL:    "https://polygon-rpc.example",
	})
	if err != nil {
		t.Fatalf("expected embedded paper backend init to succeed, got %v", err)
	}
	if !state.embeddedPaper {
		t.Fatal("expected embedded paper backend flag to be true")
	}
	if state.trader != nil {
		t.Fatal("expected embedded paper init to defer trader creation until engine startup")
	}
	if math.Abs(state.startingBalance-55.5) > 0.000001 {
		t.Fatalf("expected embedded paper starting balance 55.5, got %.2f", state.startingBalance)
	}
	if state.polygonClient == nil {
		t.Fatal("expected polygon client to still be available for resolution checks")
	}
}

func TestApplyRealbotTUISettingsAllowsMakerPaperBackendMode(t *testing.T) {
	cfg := &core.Config{}
	applyRealbotTUISettings(cfg, paper.TUISettings{
		ExecutionBackend:     core.ExecutionBackendPaper,
		PaperArbMode:         paperArbModeMaker,
		SplitStrategyEnabled: true,
	})

	if cfg.ExecutionBackend != core.ExecutionBackendPaper {
		t.Fatalf("expected paper execution backend, got %q", cfg.ExecutionBackend)
	}
	if cfg.PaperArbMode != paperArbModeMaker {
		t.Fatalf("expected maker mode to be allowed, got %q", cfg.PaperArbMode)
	}
	if cfg.SplitStrategyEnabled {
		t.Fatal("expected split strategy to be disabled on paper backend")
	}
}

func TestRealbotBindBackendTraderUsesLiveTraderWhenPresent(t *testing.T) {
	engine := paper.NewEngine(100)
	liveTrader := &trading.RealTrader{}

	got := realbotBindBackendTrader(&core.Config{ExecutionBackend: core.ExecutionBackendLive}, engine, &realbotBackendState{
		trader: liveTrader,
	})
	if got != liveTrader {
		t.Fatal("expected live backend binding to reuse the initialized trader")
	}
}

func TestRealbotBindBackendTraderCreatesEmbeddedPaperFallback(t *testing.T) {
	engine := paper.NewEngine(100)

	got := realbotBindBackendTrader(&core.Config{ExecutionBackend: core.ExecutionBackendPaper}, engine, &realbotBackendState{
		embeddedPaper: true,
	})
	if got == nil {
		t.Fatal("expected embedded paper backend binding to create a trader")
	}
	if !got.IsEmbeddedPaperMode() {
		t.Fatal("expected embedded paper backend binding to create an embedded paper trader")
	}
}

func TestRealbotSwitchExecutionBackendBuildsLiveTraderAndResetsFlatEngine(t *testing.T) {
	engine := paper.NewEngine(25)
	paperTrader := trading.NewEmbeddedPaperRealTrader(&core.Config{ExecutionBackend: core.ExecutionBackendPaper}, engine)
	liveTrader := &trading.RealTrader{}
	cfg := &core.Config{
		ExecutionBackend: core.ExecutionBackendLive,
		PaperBalance:     25,
	}

	setupCalled := false
	state, got, err := realbotSwitchExecutionBackend(context.Background(), cfg, engine, paperTrader, func(context.Context, *core.Config) (*realbotBackendState, error) {
		setupCalled = true
		return &realbotBackendState{
			trader:          liveTrader,
			startingBalance: 88.75,
		}, nil
	})
	if err != nil {
		t.Fatalf("expected live backend switch to succeed, got %v", err)
	}
	if !setupCalled {
		t.Fatal("expected live setup to run")
	}
	if state == nil || state.trader != liveTrader {
		t.Fatalf("expected live backend state with test trader, got %+v", state)
	}
	if got != liveTrader {
		t.Fatal("expected switched trader to be the live trader")
	}
	if got.IsEmbeddedPaperMode() {
		t.Fatal("expected switched trader to use live backend")
	}
	if math.Abs(engine.GetBalance()-88.75) > 0.000001 {
		t.Fatalf("expected flat engine to reset to live balance 88.75, got %.2f", engine.GetBalance())
	}
}

func TestRealbotSwitchExecutionBackendToLiveClearsPaperInventory(t *testing.T) {
	engine := paper.NewEngine(25)
	if _, err := engine.BuyForMarket("btc-updown-15m-1", "Up", 0.50, 1); err != nil {
		t.Fatalf("failed to seed open paper inventory: %v", err)
	}
	paperTrader := trading.NewEmbeddedPaperRealTrader(&core.Config{ExecutionBackend: core.ExecutionBackendPaper}, engine)
	cfg := &core.Config{
		ExecutionBackend: core.ExecutionBackendLive,
		PaperBalance:     25,
	}

	setupCalled := false
	state, got, err := realbotSwitchExecutionBackend(context.Background(), cfg, engine, paperTrader, func(context.Context, *core.Config) (*realbotBackendState, error) {
		setupCalled = true
		return &realbotBackendState{trader: &trading.RealTrader{}, startingBalance: 100}, nil
	})
	if err != nil {
		t.Fatalf("expected backend switch to discard paper inventory after live setup, got %v", err)
	}
	if !setupCalled {
		t.Fatal("expected live setup to run")
	}
	if state == nil || state.trader == nil {
		t.Fatalf("expected live backend state, got %+v", state)
	}
	if got == paperTrader || got == nil || got.IsEmbeddedPaperMode() {
		t.Fatal("expected switched trader to be the live trader")
	}
	if len(engine.GetPositions()) != 0 {
		t.Fatal("expected simulated paper inventory to be cleared when switching to live")
	}
	if math.Abs(engine.GetBalance()-100) > 0.000001 {
		t.Fatalf("expected engine balance to reset to live balance 100, got %.2f", engine.GetBalance())
	}
}

func TestRealbotSwitchExecutionBackendCreatesEmbeddedPaperTrader(t *testing.T) {
	engine := paper.NewEngine(100)
	liveTrader := &trading.RealTrader{}
	cfg := &core.Config{
		ExecutionBackend: core.ExecutionBackendPaper,
		PaperBalance:     55.50,
	}

	state, got, err := realbotSwitchExecutionBackend(context.Background(), cfg, engine, liveTrader, nil)
	if err != nil {
		t.Fatalf("expected paper backend switch to succeed, got %v", err)
	}
	if state == nil || !state.embeddedPaper {
		t.Fatalf("expected embedded paper backend state, got %+v", state)
	}
	if got == nil || !got.IsEmbeddedPaperMode() {
		t.Fatal("expected switched trader to be embedded paper")
	}
	if math.Abs(engine.GetBalance()-55.50) > 0.000001 {
		t.Fatalf("expected flat engine to reset to configured paper balance 55.50, got %.2f", engine.GetBalance())
	}
}

func TestNormalizePaperArbModeSupportsBinanceGap(t *testing.T) {
	if got := normalizePaperArbMode("binance-gap"); got != paperArbModeBinanceGap {
		t.Fatalf("normalizePaperArbMode(binance-gap) = %q, want %q", got, paperArbModeBinanceGap)
	}
}

func TestRealbotRoundSnapshotPnLUsesNeutralizedBookDeltaForLiveBackend(t *testing.T) {
	engine := paper.NewEngine(100.0)
	engine.AddRealizedPnL(0.29)

	snapshot := realbotRoundSnapshot{
		startingEquity: 33.38,
		startRealized:  0.00,
	}

	got := realbotRoundSnapshotPnL(&trading.RealTrader{}, engine, snapshot, 33.66, 0.02)
	if math.Abs(got-0.26) > 0.000001 {
		t.Fatalf("expected live round snapshot pnl to follow neutralized book delta 0.26, got %.4f", got)
	}
}

func TestRealbotRoundSnapshotPnLUsesNeutralizedBookDeltaForPaperMode(t *testing.T) {
	engine := paper.NewEngine(100.0)
	engine.AddRealizedPnL(4.50)
	snapshot := realbotRoundSnapshot{
		startingEquity: 64.67,
		startRealized:  1.23,
	}

	got := realbotRoundSnapshotPnL(nil, engine, snapshot, 74.13, 9.46)
	if math.Abs(got) > 0.000001 {
		t.Fatalf("expected paper-mode snapshot pnl to follow neutralized book delta 0.00, got %.4f", got)
	}
}

func TestCheckRedemptionEmbeddedPaperSettlesResolvedPayout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/markets/cond-1" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"condition_id":"cond-1","closed":true,"tokens":[{"token_id":"down-token","outcome":"Down","winner":false},{"token_id":"up-token","outcome":"Up","winner":true}]}`))
	}))
	defer server.Close()

	engine := paper.NewEngine(100)
	if _, err := engine.BuyForMarket("BTC", "Up", 0.60, 5); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	tui := paper.NewTUI(engine, paper.NewOrderBook())
	tui.RecordRound(100, 97, -3, 1, engine.GetPositions(), nil)

	restClient := api.NewRestClient("polymarket")
	restClient.BaseURL = server.URL
	resCache := api.NewResolutionCache(nil, nil, restClient)
	trader := trading.NewEmbeddedPaperRealTrader(&core.Config{ExecutionBackend: core.ExecutionBackendPaper}, engine)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	checkRedemption(ctx, "BTC", "cond-1", []string{"Down", "Up"}, time.Now().Add(-time.Minute), trader, engine, tui, resCache)

	if got := engine.GetPendingRedemptions()["BTC"]; got != 0 {
		t.Fatalf("expected no pending redemption after embedded paper settle, got %.2f", got)
	}
	if got := engine.GetBalance(); math.Abs(got-102.0) > 0.000001 {
		t.Fatalf("expected embedded paper balance 102.00 after settlement, got %.2f", got)
	}

	history := tui.GetRoundHistory()
	if len(history) != 1 {
		t.Fatalf("expected one round history entry, got %d", len(history))
	}
	if got := history[0].EndingEquity; math.Abs(got-99.0) > 0.000001 {
		t.Fatalf("expected round ending equity 99.00 after redemption delta, got %.2f", got)
	}
	if got := history[0].PnL; math.Abs(got-(-1.0)) > 0.000001 {
		t.Fatalf("expected round pnl -1.00 after redemption delta, got %.2f", got)
	}
}

func TestEmbeddedPaperResolutionSweepSettlesHeldExpiredMarketBySlug(t *testing.T) {
	marketID := "btc-updown-5m-1700000000"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/markets/"+marketID {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"condition_id":"cond-btc-1","slug":"` + marketID + `","closed":true,"tokens":[{"token_id":"down-token","outcome":"Down","winner":false},{"token_id":"up-token","outcome":"Up","winner":true}]}`))
	}))
	defer server.Close()

	engine := paper.NewEngine(100)
	if _, err := engine.BuyForMarket(marketID, "Up", 0.60, 5); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}
	engine.UpdateMarketBidAsk(marketID, "Up", 0.99, 1.00)
	engine.UpdateMarketBidAsk(marketID, "Down", 0.01, 0.02)

	tui := paper.NewTUI(engine, paper.NewOrderBook())
	restClient := api.NewRestClient("polymarket")
	restClient.BaseURL = server.URL
	resCache := api.NewResolutionCache(nil, nil, restClient)
	trader := trading.NewEmbeddedPaperRealTrader(&core.Config{ExecutionBackend: core.ExecutionBackendPaper}, engine)

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	realbotStartEmbeddedPaperResolutionSweep(ctx, trader, engine, tui, restClient, resCache)

	deadline := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := engine.GetBalance(); math.Abs(got-102.0) <= 0.000001 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	if got := engine.GetBalance(); math.Abs(got-102.0) > 0.000001 {
		t.Fatalf("expected background embedded-paper sweep to settle to 102.00, got %.2f", got)
	}
	if got := engine.GetPendingRedemptions()[marketID]; got != 0 {
		t.Fatalf("expected no pending redemption after background settle, got %.2f", got)
	}
	if realbotHasEnginePositionsForMarket(engine, marketID) {
		t.Fatal("expected held expired market inventory to be redeemed and cleared")
	}
}

func TestEmbeddedPaperResolutionSweepSettlesHeldExpiredMarketByGammaSlug(t *testing.T) {
	marketID := "bitcoin-up-or-down-april-19-2026-2am-et"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/markets/"+marketID:
			http.Error(w, "not found", http.StatusNotFound)
		case r.URL.Path == "/markets" && r.URL.Query().Get("slug") == marketID:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"conditionId":"cond-btc-1","slug":"` + marketID + `","clobTokenIds":"[\"down-token\",\"up-token\"]","outcomes":"[\"Down\",\"Up\"]","outcomePrices":"[\"0\",\"1\"]","umaResolutionStatus":"resolved","active":false,"closed":true}]`))
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()

	engine := paper.NewEngine(100)
	if _, err := engine.BuyForMarket(marketID, "Up", 0.60, 5); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}
	engine.UpdateMarketBidAsk(marketID, "Up", 0.99, 1.00)
	engine.UpdateMarketBidAsk(marketID, "Down", 0.01, 0.02)

	tui := paper.NewTUI(engine, paper.NewOrderBook())
	restClient := api.NewRestClient("polymarket")
	restClient.BaseURL = server.URL
	restClient.GammaURL = server.URL
	resCache := api.NewResolutionCache(nil, nil, restClient)
	trader := trading.NewEmbeddedPaperRealTrader(&core.Config{ExecutionBackend: core.ExecutionBackendPaper}, engine)

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	realbotStartEmbeddedPaperResolutionSweep(ctx, trader, engine, tui, restClient, resCache)

	deadline := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := engine.GetBalance(); math.Abs(got-102.0) <= 0.000001 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	if got := engine.GetBalance(); math.Abs(got-102.0) > 0.000001 {
		t.Fatalf("expected gamma-backed embedded-paper sweep to settle to 102.00, got %.2f", got)
	}
	if realbotHasEnginePositionsForMarket(engine, marketID) {
		t.Fatal("expected gamma-backed sweep to redeem and clear inventory")
	}
}

func TestNormalizePaperArbModeDefaultsToTaker(t *testing.T) {
	if got := normalizePaperArbMode(""); got != paperArbModeTaker {
		t.Fatalf("expected empty mode to normalize to taker, got %q", got)
	}
	if got := normalizePaperArbMode("maker"); got != paperArbModeMaker {
		t.Fatalf("expected maker to remain maker, got %q", got)
	}
	if got := normalizePaperArbMode("copytrade"); got != paperArbModeCopytrade {
		t.Fatalf("expected copytrade to remain copytrade, got %q", got)
	}
}
