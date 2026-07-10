package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

func TestRealbotUpsertPaperMakerQuote(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	makerQuotes := make(map[string]*realbotMakerQuote)

	// Create quote
	changed := realbotUpsertPaperMakerQuote("mkt-1", tui, makerQuotes, api.SideBuy, "Yes", "yes-token", 0.50, 10.0)
	if !changed {
		t.Fatal("expected quote creation to report changed")
	}

	key := realbotMakerQuoteKey(api.SideBuy, "Yes")
	q, ok := makerQuotes[key]
	if !ok || q == nil {
		t.Fatal("quote was not created in map")
	}
	if q.Price != 0.50 || q.RemainingQty != 10.0 || q.TokenID != "yes-token" {
		t.Fatalf("unexpected quote details: %+v", q)
	}

	// Upsert identical quote (no change)
	changed = realbotUpsertPaperMakerQuote("mkt-1", tui, makerQuotes, api.SideBuy, "Yes", "yes-token", 0.50, 10.0)
	if changed {
		t.Fatal("expected identical quote upsert to not report changed")
	}

	// Update quote price
	changed = realbotUpsertPaperMakerQuote("mkt-1", tui, makerQuotes, api.SideBuy, "Yes", "yes-token", 0.49, 10.0)
	if !changed {
		t.Fatal("expected price change to report changed")
	}
	if makerQuotes[key].Price != 0.49 {
		t.Fatalf("expected updated price 0.49, got %.2f", makerQuotes[key].Price)
	}

	// Upsert invalid quote value (should clean/delete existing quote)
	changed = realbotUpsertPaperMakerQuote("mkt-1", tui, makerQuotes, api.SideBuy, "Yes", "yes-token", 0.0, 0.0)
	if !changed {
		t.Fatal("expected quote deletion on invalid args to report changed")
	}
	if _, ok := makerQuotes[key]; ok {
		t.Fatal("expected quote to be deleted from map")
	}
}

func TestRealbotSimulatePaperMakerFills(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	makerQuotes := make(map[string]*realbotMakerQuote)

	// Register token paths in engine for paper trading
	engine.SyncExternalPosition("mkt-1", "Yes", 0, 0.0)
	engine.SyncExternalPosition("mkt-1", "No", 0, 0.0)

	// Set up buy and sell quotes
	realbotUpsertPaperMakerQuote("mkt-1", tui, makerQuotes, api.SideBuy, "Yes", "yes-token", 0.55, 10.0)
	realbotUpsertPaperMakerQuote("mkt-1", tui, makerQuotes, api.SideSell, "No", "no-token", 0.45, 5.0)
	engine.MakerBuyForMarket("mkt-1", "No", 0.40, 5.0) // seed some No shares so we can sell them

	tokenBids := map[string]float64{"Yes": 0.50, "No": 0.46}
	tokenAsks := map[string]float64{"Yes": 0.54, "No": 0.48}

	// Run fill simulation:
	// - YES buy quote at 0.55 should match because market ask for YES is 0.54 (ask <= buyPrice)
	// - NO sell quote at 0.45 should match because market bid for NO is 0.46 (bid >= sellPrice)
	realbotSimulatePaperMakerFills("mkt-1", engine, tui, makerQuotes, tokenBids, tokenAsks)

	yesKey := realbotMakerQuoteKey(api.SideBuy, "Yes")
	noKey := realbotMakerQuoteKey(api.SideSell, "No")

	if _, ok := makerQuotes[yesKey]; ok {
		t.Fatal("expected YES buy quote to be filled and removed")
	}
	if _, ok := makerQuotes[noKey]; ok {
		t.Fatal("expected NO sell quote to be filled and removed")
	}

	// Verify portfolio state in engine
	pos := engine.GetPositions()
	yesPos := pos["mkt-1:Yes"]
	noPos := pos["mkt-1:No"]

	if yesPos.Quantity != 10.0 || yesPos.AvgPrice != 0.55 {
		t.Fatalf("unexpected YES position: %+v", yesPos)
	}
	// NO position started at 5 shares bought at 0.40, and we sold 5 shares at 0.45
	if noPos.Quantity != 0 {
		t.Fatalf("expected NO position to be closed to 0 shares, got %.2f", noPos.Quantity)
	}
}

func TestRealbotMakerVolatilityProtection(t *testing.T) {
	engine := paper.NewEngine(1000)
	tui := paper.NewTUI(engine, nil)
	makerQuotes := make(map[string]*realbotMakerQuote)
	cfg := &core.Config{
		MakerMinQuoteValue:        1.0,
		BinanceSignalThresholdPct: 0.20,
		BinanceSignalMaxAgeMs:     5000,
	}
	liveCfg := paper.TUISettings{}

	trader := trading.NewEmbeddedPaperRealTrader(&core.Config{ExecutionBackend: core.ExecutionBackendPaper}, engine)

	realbotUpsertPaperMakerQuote("mkt-1", tui, makerQuotes, api.SideBuy, "Yes", "yes-token", 0.50, 10.0)

	// Create binance feed trigger
	feed := api.NewBinanceFuturesPriceFeed("BTCUSDT", 2*time.Second)
	feed.SetConnectedForTest(true)

	// Simulate normal state (0.10% delta)
	baseTime := time.Now().Add(-5 * time.Second)
	feed.RecordTradeSampleForTest(100.0, baseTime)
	feed.RecordTradeSampleForTest(100.1, time.Now())

	var lastSync time.Time
	maintainRealbotMakerQuotes(
		context.Background(),
		"mkt-1",
		time.Now().Add(1*time.Hour),
		[]string{"Yes", "No"},
		func(outcome string) string { return outcome + "-token" },
		map[string]float64{"Yes": 0.49, "No": 0.49},
		map[string]float64{"Yes": 0.51, "No": 0.51},
		map[string]int{"Yes": 0, "No": 0},
		trader,
		engine,
		nil,
		tui,
		liveCfg,
		cfg,
		makerQuotes,
		&lastSync,
		feed,
	)

	// Quotes should still exist
	if len(makerQuotes) == 0 {
		t.Fatal("expected quotes to be maintained under normal volatility")
	}

	// Trigger high volatility (0.30% delta relative to baseline)
	feed.RecordTradeSampleForTest(100.3, time.Now())

	maintainRealbotMakerQuotes(
		context.Background(),
		"mkt-1",
		time.Now().Add(1*time.Hour),
		[]string{"Yes", "No"},
		func(outcome string) string { return outcome + "-token" },
		map[string]float64{"Yes": 0.49, "No": 0.49},
		map[string]float64{"Yes": 0.51, "No": 0.51},
		map[string]int{"Yes": 0, "No": 0},
		trader,
		engine,
		nil,
		tui,
		liveCfg,
		cfg,
		makerQuotes,
		&lastSync,
		feed,
	)

	// Quotes should be fully cleared
	if len(makerQuotes) != 0 {
		t.Fatal("expected quotes to be cancelled by volatility protection")
	}
}

func TestRealbotMaintainMakerQuotesPaperMode(t *testing.T) {
	engine := paper.NewEngine(1000)
	tui := paper.NewTUI(engine, nil)
	trader := trading.NewEmbeddedPaperRealTrader(&core.Config{ExecutionBackend: core.ExecutionBackendPaper}, engine)
	trader.RegisterPaperToken("yes-token", "mkt-1", "Yes")
	trader.RegisterPaperToken("no-token", "mkt-1", "No")

	makerQuotes := make(map[string]*realbotMakerQuote)
	cfg := &core.Config{
		MakerMinQuoteValue:        5.0,
		MakerInventoryTargetMult:  2.0,
		MakerInventoryCapMult:     4.0,
		MakerMergeBufferSeconds:   30,
		BinanceSignalThresholdPct: 0.30,
		BinanceSignalMaxAgeMs:     5000,
	}

	// Configure live settings
	liveCfg := paper.TUISettings{
		MinAskPrice:           0.01,
		MaxAskPrice:           0.99,
		MinMarginPercent:      1.0, // 1% edge = maxPairCost of 0.99
		LadderedTakerSizeUSDC: 10.0,
	}

	// Spreads wide enough to satisfy 1% pair edge (0.45 bid + 0.45 bid = 0.90, asks 0.55 + 0.55 = 1.10)
	// Bids sum to 0.90, ask sum is 1.10.
	tokenBids := map[string]float64{"Yes": 0.45, "No": 0.45}
	tokenAsks := map[string]float64{"Yes": 0.55, "No": 0.55}

	var lastSync time.Time
	maintainRealbotMakerQuotes(
		context.Background(),
		"mkt-1",
		time.Now().Add(1*time.Hour),
		[]string{"Yes", "No"},
		func(outcome string) string { return outcome + "-token" },
		tokenBids,
		tokenAsks,
		map[string]int{"Yes": 0, "No": 0},
		trader,
		engine,
		nil,
		tui,
		liveCfg,
		cfg,
		makerQuotes,
		&lastSync,
		nil,
	)

	t.Logf("lastSync: %v", lastSync)
	t.Logf("makerQuotes length: %d", len(makerQuotes))
	for k, v := range makerQuotes {
		t.Logf("  makerQuotes[%q] = %+v", k, v)
	}

	// Since we hold no inventory, buyPrice0 and buyPrice1 should be set
	yesBuyKey := realbotMakerQuoteKey(api.SideBuy, "Yes")
	noBuyKey := realbotMakerQuoteKey(api.SideBuy, "No")

	qYes, okYes := makerQuotes[yesBuyKey]
	qNo, okNo := makerQuotes[noBuyKey]
	if !okYes || qYes == nil || !okNo || qNo == nil {
		t.Fatal("expected buy quotes for both outcomes to be placed")
	}

	// Verify quantities are > 0 and prices sum to <= 0.99
	if qYes.RequestedQty <= 0 || qNo.RequestedQty <= 0 {
		t.Fatalf("expected positive quantities, got Yes=%.2f, No=%.2f", qYes.RequestedQty, qNo.RequestedQty)
	}
	if qYes.Price+qNo.Price > 0.99 {
		t.Fatalf("expected sum of prices to be <= 0.99 (maxPairCost), got %.3f", qYes.Price+qNo.Price)
	}
}

type mockExchangeClientForOpenOrdersErr struct {
	api.ExchangeClient
}

func (m *mockExchangeClientForOpenOrdersErr) GetOpenOrders(ctx context.Context) ([]api.OpenOrder, error) {
	return nil, errors.New("simulated API error")
}

func TestRealbotMaintainMakerQuotesOpenOrdersFailure(t *testing.T) {
	engine := paper.NewEngine(1000)
	tui := paper.NewTUI(engine, nil)
	trader := &trading.RealTrader{}
	mockClient := &mockExchangeClientForOpenOrdersErr{}
	trader.SetClient(mockClient)

	// Create pre-existing maker quotes in the map
	makerQuotes := map[string]*realbotMakerQuote{
		realbotMakerQuoteKey(api.SideBuy, "Yes"): {
			OrderID:      "order-123",
			TokenID:      "yes-token",
			Outcome:      "Yes",
			Side:         api.SideBuy,
			Price:        0.50,
			RequestedQty: 10.0,
			RemainingQty: 10.0,
		},
	}

	cfg := &core.Config{}
	liveCfg := paper.TUISettings{}

	var lastSync time.Time
	// Call maintainRealbotMakerQuotes, which should encounter an error on GetOpenOrders,
	// log it, and return early.
	maintainRealbotMakerQuotes(
		context.Background(),
		"mkt-1",
		time.Now().Add(1*time.Hour),
		[]string{"Yes", "No"},
		func(outcome string) string { return outcome + "-token" },
		map[string]float64{"Yes": 0.49, "No": 0.49},
		map[string]float64{"Yes": 0.51, "No": 0.51},
		map[string]int{"Yes": 0, "No": 0},
		trader,
		engine,
		nil,
		tui,
		liveCfg,
		cfg,
		makerQuotes,
		&lastSync,
		nil,
	)

	// Verify that the existing quote was NOT deleted or orphaned on failure
	key := realbotMakerQuoteKey(api.SideBuy, "Yes")
	q, ok := makerQuotes[key]
	if !ok || q == nil {
		t.Fatal("expected existing quote to be preserved on GetOpenOrders failure, but it was deleted")
	}
	if q.OrderID != "order-123" {
		t.Fatalf("expected order ID 'order-123', got %q", q.OrderID)
	}
}
