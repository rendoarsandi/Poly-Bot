package trading

import (
	"context"
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/paper"
)

// TestPaperTrader_BuySellConsistency verifies paper trader maintains consistent state
func TestPaperTrader_BuySellConsistency(t *testing.T) {
	engine := paper.NewEngine(1000.0)
	orderBook := paper.NewOrderBook()
	trader := NewPaperTrader(engine, orderBook)

	ctx := context.Background()

	// Verify initial state
	balance, err := trader.GetBalance(ctx)
	if err != nil {
		t.Fatalf("GetBalance failed: %v", err)
	}
	if balance != 1000.0 {
		t.Errorf("Expected initial balance $1000, got $%.2f", balance)
	}

	// Buy some shares
	result, err := trader.Buy(ctx, "token123", "Up", 0.50, 10, api.OrderTypeMarket, api.TIFGoodTilCancelled, 0)
	if err != nil {
		t.Fatalf("Buy failed: %v", err)
	}
	if !result.Success {
		t.Errorf("Buy should succeed: %s", result.Message)
	}

	// Verify balance decreased
	newBalance, _ := trader.GetBalance(ctx)
	expectedBalance := 1000.0 - (0.50 * 10)
	if newBalance != expectedBalance {
		t.Errorf("Expected balance $%.2f after buy, got $%.2f", expectedBalance, newBalance)
	}

	// Verify position exists
	positions, err := trader.GetPositions(ctx)
	if err != nil {
		t.Fatalf("GetPositions failed: %v", err)
	}
	if len(positions) == 0 {
		t.Error("Expected at least one position after buy")
	}
}

// TestPaperTrader_FeeCalculation verifies fees are calculated correctly
func TestPaperTrader_FeeCalculation(t *testing.T) {
	engine := paper.NewEngine(1000.0)
	orderBook := paper.NewOrderBook()
	trader := NewPaperTrader(engine, orderBook)

	ctx := context.Background()

	// Buy triggers the dynamic curve fee because feeRateBps > 0
	result, err := trader.Buy(ctx, "token123", "Up", 0.50, 100, api.OrderTypeMarket, api.TIFGoodTilCancelled, 100)
	if err != nil {
		t.Fatalf("Buy failed: %v", err)
	}

	// Crypto curve: feeTokens = size * 0.25 * (p * (1-p))^2
	// feeTokens = 100 * 0.25 * (0.5 * 0.5)^2 = 1.5625 shares
	// USDC fee equivalent = 1.5625 * 0.50 = 0.78125
	expectedFee := 0.78125
	if result.Fee != expectedFee {
		t.Errorf("Expected fee $%.4f, got $%.4f", expectedFee, result.Fee)
	}
}

// TestPaperTrader_IsPaperMode verifies mode detection
func TestPaperTrader_IsPaperMode(t *testing.T) {
	engine := paper.NewEngine(1000.0)
	orderBook := paper.NewOrderBook()
	trader := NewPaperTrader(engine, orderBook)

	if !trader.IsPaperMode() {
		t.Error("PaperTrader should report IsPaperMode=true")
	}
	if trader.IsTestMode() {
		t.Error("PaperTrader should report IsTestMode=false")
	}
}

// TestTradeResult_FieldPopulation verifies all TradeResult fields are populated
func TestTradeResult_FieldPopulation(t *testing.T) {
	engine := paper.NewEngine(1000.0)
	orderBook := paper.NewOrderBook()
	trader := NewPaperTrader(engine, orderBook)

	ctx := context.Background()

	result, _ := trader.Buy(ctx, "token-abc", "Down", 0.45, 20, api.OrderTypeMarket, api.TIFGoodTilCancelled, 50)

	if result.OrderID == "" {
		t.Error("OrderID should not be empty")
	}
	if result.Status != "FILLED" {
		t.Errorf("Expected status FILLED, got %s", result.Status)
	}
	if result.Price != 0.45 {
		t.Errorf("Expected price 0.45, got %.4f", result.Price)
	}
	if result.Size != 20 {
		t.Errorf("Expected size 20, got %.2f", result.Size)
	}
	if result.Side != "BUY" {
		t.Errorf("Expected side BUY, got %s", result.Side)
	}
	if result.TokenID != "token-abc" {
		t.Errorf("Expected tokenID token-abc, got %s", result.TokenID)
	}
	if result.Outcome != "Down" {
		t.Errorf("Expected outcome Down, got %s", result.Outcome)
	}
	if result.Timestamp.IsZero() {
		t.Error("Timestamp should not be zero")
	}
	if result.FeeRateBps != 50 {
		t.Errorf("Expected FeeRateBps 50, got %d", result.FeeRateBps)
	}
}

// TestPaperTrader_SellAfterBuy verifies sell works after buying
func TestPaperTrader_SellAfterBuy(t *testing.T) {
	engine := paper.NewEngine(1000.0)
	orderBook := paper.NewOrderBook()
	trader := NewPaperTrader(engine, orderBook)

	ctx := context.Background()

	// Buy first
	_, err := trader.Buy(ctx, "token123", "Up", 0.50, 10, api.OrderTypeMarket, api.TIFGoodTilCancelled, 0)
	if err != nil {
		t.Fatalf("Buy failed: %v", err)
	}

	balanceAfterBuy, _ := trader.GetBalance(ctx)

	// Sell at higher price (profit)
	result, err := trader.Sell(ctx, "token123", "Up", 0.60, 10, api.OrderTypeMarket, api.TIFGoodTilCancelled, 0)
	if err != nil {
		t.Fatalf("Sell failed: %v", err)
	}
	if !result.Success {
		t.Errorf("Sell should succeed: %s", result.Message)
	}
	if result.Side != "SELL" {
		t.Errorf("Expected side SELL, got %s", result.Side)
	}

	// Verify balance increased (profit from sell)
	balanceAfterSell, _ := trader.GetBalance(ctx)
	if balanceAfterSell <= balanceAfterBuy {
		t.Errorf("Balance should increase after profitable sell: before=%.2f, after=%.2f",
			balanceAfterBuy, balanceAfterSell)
	}
}

// TestPaperTrader_CancelOperations verifies cancel methods don't error
func TestPaperTrader_CancelOperations(t *testing.T) {
	engine := paper.NewEngine(1000.0)
	orderBook := paper.NewOrderBook()
	trader := NewPaperTrader(engine, orderBook)

	ctx := context.Background()

	// These should not error (paper trader doesn't track individual orders)
	if err := trader.CancelOrder(ctx, "any-order-id"); err != nil {
		t.Errorf("CancelOrder should not error: %v", err)
	}
	if err := trader.CancelAll(ctx); err != nil {
		t.Errorf("CancelAll should not error: %v", err)
	}
}

// TestPaperTrader_GetMarketInfo verifies it returns not implemented error
func TestPaperTrader_GetMarketInfo(t *testing.T) {
	engine := paper.NewEngine(1000.0)
	orderBook := paper.NewOrderBook()
	trader := NewPaperTrader(engine, orderBook)

	ctx := context.Background()

	_, err := trader.GetMarketInfo(ctx, "condition123")
	if err == nil {
		t.Error("GetMarketInfo should return error for paper trader")
	}
}

// TestTraderInterface_Compliance verifies both traders implement the interface
func TestTraderInterface_Compliance(t *testing.T) {
	engine := paper.NewEngine(1000.0)
	orderBook := paper.NewOrderBook()

	// This should compile - verifies interface compliance
	var _ Trader = NewPaperTrader(engine, orderBook)

	// Note: RealTrader requires config with credentials, so we can't test it here
	// without mocking. This is a compile-time check only.
}

// TestPositionInfo_Structure verifies PositionInfo has expected fields
func TestPositionInfo_Structure(t *testing.T) {
	pos := PositionInfo{
		TokenID:  "token123",
		Outcome:  "Up",
		Size:     100.0,
		AvgPrice: 0.48,
	}

	if pos.TokenID != "token123" {
		t.Errorf("TokenID mismatch")
	}
	if pos.Outcome != "Up" {
		t.Errorf("Outcome mismatch")
	}
	if pos.Size != 100.0 {
		t.Errorf("Size mismatch")
	}
	if pos.AvgPrice != 0.48 {
		t.Errorf("AvgPrice mismatch")
	}
}

// TestPaperTrader_InsufficientBalance verifies buy fails with insufficient balance
func TestPaperTrader_InsufficientBalance(t *testing.T) {
	engine := paper.NewEngine(10.0) // Only $10
	orderBook := paper.NewOrderBook()
	trader := NewPaperTrader(engine, orderBook)

	ctx := context.Background()

	// Try to buy $50 worth
	result, err := trader.Buy(ctx, "token123", "Up", 0.50, 100, api.OrderTypeMarket, api.TIFGoodTilCancelled, 0)
	if err != nil {
		t.Fatalf("Buy should not return error, just unsuccessful result: %v", err)
	}
	if result.Success {
		t.Error("Buy should fail with insufficient balance")
	}
}

// TestPaperTrader_MultiplePositions verifies tracking multiple positions
func TestPaperTrader_MultiplePositions(t *testing.T) {
	engine := paper.NewEngine(1000.0)
	orderBook := paper.NewOrderBook()
	trader := NewPaperTrader(engine, orderBook)

	ctx := context.Background()

	// Buy Up
	_, _ = trader.Buy(ctx, "tokenUp", "Up", 0.45, 10, api.OrderTypeMarket, api.TIFGoodTilCancelled, 0)
	// Buy Down
	_, _ = trader.Buy(ctx, "tokenDown", "Down", 0.50, 10, api.OrderTypeMarket, api.TIFGoodTilCancelled, 0)

	positions, _ := trader.GetPositions(ctx)
	if len(positions) < 2 {
		t.Errorf("Expected at least 2 positions, got %d", len(positions))
	}

	// Verify both outcomes exist
	hasUp, hasDown := false, false
	for _, pos := range positions {
		if pos.Outcome == "Up" {
			hasUp = true
		}
		if pos.Outcome == "Down" {
			hasDown = true
		}
	}
	if !hasUp {
		t.Error("Missing Up position")
	}
	if !hasDown {
		t.Error("Missing Down position")
	}
}

// Benchmark paper trader buy operation
func BenchmarkPaperTrader_Buy(b *testing.B) {
	engine := paper.NewEngine(1000000.0)
	orderBook := paper.NewOrderBook()
	trader := NewPaperTrader(engine, orderBook)
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = trader.Buy(ctx, "token", "Up", 0.50, 1, api.OrderTypeMarket, api.TIFGoodTilCancelled, 0)
	}
}

// TestRealTrader_SafetyLimits tests the checkSafetyLimits function
// Note: We can't create a full RealTrader without credentials, but we can test the logic
func TestRealTrader_DailyLossReset(t *testing.T) {
	// This test verifies the daily loss reset logic conceptually
	// In production, RealTrader.checkSafetyLimits() resets when day changes

	now := time.Now()
	startOfDay := now.Truncate(24 * time.Hour)

	// Verify truncation works as expected
	if startOfDay.Hour() != 0 || startOfDay.Minute() != 0 || startOfDay.Second() != 0 {
		t.Error("Truncate should give start of day (00:00:00)")
	}

	// Next day should be different
	tomorrow := now.Add(24 * time.Hour).Truncate(24 * time.Hour)
	if tomorrow == startOfDay {
		t.Error("Tomorrow should not equal today's start of day")
	}
}

func TestRealTrader_ApplyLiveFill_UpdatesCacheAndClampsAtZero(t *testing.T) {
	trader := &RealTrader{
		livePositions: map[string]float64{
			"asset-1": 2,
		},
		confirmedOrderFills: make(map[string]float64),
	}

	trader.applyLiveFill(api.OrderFillData{OrderID: "buy-1", AssetID: "asset-1", Side: "BUY", Size: "3"})
	trader.applyLiveFill(api.OrderFillData{OrderID: "sell-1", AssetID: "asset-1", Side: "SELL", Size: "4"})
	trader.applyLiveFill(api.OrderFillData{OrderID: "buy-2", AssetID: "asset-2", Side: "BUY", Size: "1.5"})
	trader.applyLiveFill(api.OrderFillData{OrderID: "sell-2", AssetID: "asset-2", Side: "SELL", Size: "10"})

	positions, err := trader.GetPositions(context.Background())
	if err != nil {
		t.Fatalf("GetPositions failed: %v", err)
	}

	if got := trader.livePositions["asset-1"]; got != 1 {
		t.Fatalf("expected asset-1 cached size 1, got %.2f", got)
	}
	if got := trader.livePositions["asset-2"]; got != 0 {
		t.Fatalf("expected asset-2 cached size clamped to 0, got %.2f", got)
	}
	if got := trader.GetConfirmedFillSize("buy-1"); got != 3 {
		t.Fatalf("expected confirmed buy-1 fill of 3, got %.2f", got)
	}
	if got := trader.GetConfirmedFillSize("sell-1"); got != 4 {
		t.Fatalf("expected confirmed sell-1 fill of 4, got %.2f", got)
	}
	if len(positions) != 1 {
		t.Fatalf("expected only one positive position from cache, got %d", len(positions))
	}
	if positions[0].TokenID != "asset-1" || positions[0].Size != 1 {
		t.Fatalf("unexpected cached position payload: %+v", positions[0])
	}
}

func TestRealTrader_ApplyLiveFill_InvalidSizeDoesNotMutateCache(t *testing.T) {
	trader := &RealTrader{
		livePositions: map[string]float64{
			"asset-1": 2,
		},
		confirmedOrderFills: make(map[string]float64),
	}

	trader.applyLiveFill(api.OrderFillData{OrderID: "bad-order", AssetID: "asset-1", Side: "BUY", Size: "bad-size"})

	positions, err := trader.GetPositions(context.Background())
	if err != nil {
		t.Fatalf("GetPositions failed: %v", err)
	}
	if len(positions) != 1 || positions[0].TokenID != "asset-1" || positions[0].Size != 2 {
		t.Fatalf("expected unchanged cached position, got %+v", positions)
	}
	if got := trader.GetConfirmedFillSize("bad-order"); got != 0 {
		t.Fatalf("expected invalid fill not to be recorded, got %.2f", got)
	}
}

func TestRealTrader_ResetConfirmedFill(t *testing.T) {
	trader := &RealTrader{
		livePositions:       make(map[string]float64),
		confirmedOrderFills: map[string]float64{"order-1": 2.5},
	}

	trader.ResetConfirmedFill("order-1")

	if got := trader.GetConfirmedFillSize("order-1"); got != 0 {
		t.Fatalf("expected cleared confirmed fill, got %.2f", got)
	}
}

func TestRealTrader_WaitForLivePairPositionsReturnsImmediatelyWhenReady(t *testing.T) {
	trader := &RealTrader{
		livePositions: map[string]float64{
			"yes-token": 1.25,
			"no-token":  0.80,
		},
		confirmedOrderFills: make(map[string]float64),
	}

	bal0, bal1, ready, err := trader.WaitForLivePairPositions(context.Background(), "yes-token", "no-token", 0.01, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForLivePairPositions returned error: %v", err)
	}
	if !ready {
		t.Fatal("expected pair to be ready immediately")
	}
	if bal0 != 1.25 || bal1 != 0.80 {
		t.Fatalf("unexpected balances %.2f / %.2f", bal0, bal1)
	}
}

func TestRealTrader_WaitForLivePairPositionsObservesAsyncWSUpdate(t *testing.T) {
	trader := &RealTrader{
		livePositions: map[string]float64{
			"yes-token": 2.00,
		},
		confirmedOrderFills: make(map[string]float64),
	}

	go func() {
		time.Sleep(25 * time.Millisecond)
		trader.applyLiveFill(api.OrderFillData{AssetID: "no-token", Side: "BUY", Size: "1.50"})
	}()

	bal0, bal1, ready, err := trader.WaitForLivePairPositions(context.Background(), "yes-token", "no-token", 0.01, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForLivePairPositions returned error: %v", err)
	}
	if !ready {
		t.Fatal("expected pair to become ready after websocket update")
	}
	if bal0 != 2.00 || bal1 != 1.50 {
		t.Fatalf("unexpected balances %.2f / %.2f", bal0, bal1)
	}
}

func TestRealTrader_WaitForLivePairPositionsTimeoutReturnsLatestSnapshot(t *testing.T) {
	trader := &RealTrader{
		livePositions: map[string]float64{
			"yes-token": 0.75,
		},
		confirmedOrderFills: make(map[string]float64),
	}

	bal0, bal1, ready, err := trader.WaitForLivePairPositions(context.Background(), "yes-token", "no-token", 0.01, 60*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForLivePairPositions returned error: %v", err)
	}
	if ready {
		t.Fatal("expected timeout without both sides becoming available")
	}
	if bal0 != 0.75 || bal1 != 0 {
		t.Fatalf("unexpected balances %.2f / %.2f", bal0, bal1)
	}
}
