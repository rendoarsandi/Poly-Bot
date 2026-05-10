package trading

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
)

type stubExchangeClient struct {
	balanceAllowance *api.BalanceAllowance
	address          string
	positions        []api.Position
	positionsErr     error
	lastOrderRequest *api.OrderRequest
	orderResponse    *api.OrderResponse
}

func (s *stubExchangeClient) PlaceOrder(ctx context.Context, req *api.OrderRequest) (*api.OrderResponse, error) {
	if req != nil {
		copied := *req
		s.lastOrderRequest = &copied
	}
	if s.orderResponse != nil {
		resp := *s.orderResponse
		return &resp, nil
	}
	return nil, fmt.Errorf("not implemented")
}

func (s *stubExchangeClient) CancelOrder(ctx context.Context, orderID string) error {
	return nil
}

func (s *stubExchangeClient) CancelAllOrders(ctx context.Context) error {
	return nil
}

func (s *stubExchangeClient) GetPositions(ctx context.Context) ([]api.Position, error) {
	if s.positionsErr != nil {
		return nil, s.positionsErr
	}
	if s.positions != nil {
		return append([]api.Position(nil), s.positions...), nil
	}
	return nil, fmt.Errorf("not implemented")
}

func (s *stubExchangeClient) GetOrder(ctx context.Context, orderID string) (*api.OpenOrder, error) {
	return nil, fmt.Errorf("not implemented")
}

func (s *stubExchangeClient) GetOpenOrders(ctx context.Context) ([]api.OpenOrder, error) {
	return nil, fmt.Errorf("not implemented")
}

func (s *stubExchangeClient) GetBalanceAllowance(ctx context.Context) (*api.BalanceAllowance, error) {
	if s.balanceAllowance == nil {
		return nil, fmt.Errorf("not configured")
	}
	return s.balanceAllowance, nil
}

func (s *stubExchangeClient) UpdateBalanceAllowance(ctx context.Context) error {
	return nil
}

func (s *stubExchangeClient) GetMarketInfo(ctx context.Context, conditionID string) (*api.MarketInfo, error) {
	return nil, fmt.Errorf("not implemented")
}

func (s *stubExchangeClient) SetTestMode(enabled bool) {}

func (s *stubExchangeClient) IsTestMode() bool {
	return false
}

func (s *stubExchangeClient) GetSigner() *api.Signer {
	return nil
}

func (s *stubExchangeClient) Address() string {
	return s.address
}

func (s *stubExchangeClient) EnableRawAPILog(path string) error {
	return nil
}

func (s *stubExchangeClient) CloseRawAPILog() error {
	return nil
}

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

	// Buy fee uses the documented Polymarket formula.
	result, err := trader.Buy(ctx, "token123", "Up", 0.50, 100, api.OrderTypeMarket, api.TIFGoodTilCancelled, 100)
	if err != nil {
		t.Fatalf("Buy failed: %v", err)
	}

	// fee_usdc = shares * feeRate * price * (1-price)
	//          = 100 * 0.01 * 0.50 * 0.50 = 0.25
	expectedFee := 0.25
	if result.Fee != expectedFee {
		t.Errorf("Expected fee $%.4f, got $%.4f", expectedFee, result.Fee)
	}

	positions := engine.GetPositions()
	pos, ok := positions["Up"]
	if !ok {
		t.Fatal("expected fee-aware paper buy to create an Up position")
	}
	if pos.Quantity >= 100.0 {
		t.Fatalf("expected fee-aware paper buy quantity below 100 shares, got %.6f", pos.Quantity)
	}
}

func TestPaperTrader_FeeAwareRoundTripHitsRealizedPnL(t *testing.T) {
	engine := paper.NewEngine(100.0)
	orderBook := paper.NewOrderBook()
	trader := NewPaperTrader(engine, orderBook)

	ctx := context.Background()

	buy, err := trader.Buy(ctx, "token123", "Up", 0.50, 10, api.OrderTypeMarket, api.TIFGoodTilCancelled, 100)
	if err != nil {
		t.Fatalf("buy failed: %v", err)
	}
	if !buy.Success {
		t.Fatalf("expected buy success, got %+v", buy)
	}

	positions := engine.GetPositions()
	pos, ok := positions["Up"]
	if !ok {
		t.Fatal("expected Up position after fee-aware buy")
	}

	sell, err := trader.Sell(ctx, "token123", "Up", 0.50, pos.Quantity, api.OrderTypeMarket, api.TIFFillAndKill, 100)
	if err != nil {
		t.Fatalf("sell failed: %v", err)
	}
	if !sell.Success {
		t.Fatalf("expected sell success, got %+v", sell)
	}

	stats := engine.GetStats()
	if stats.RealizedPnL >= 0 {
		t.Fatalf("expected round-trip realized pnl to include fees, got %.6f", stats.RealizedPnL)
	}
	if math.Abs((engine.GetBookEquity()-stats.StartingBalance)-stats.RealizedPnL) > 0.000001 {
		t.Fatalf("expected realized pnl %.6f to match equity change %.6f", stats.RealizedPnL, engine.GetBookEquity()-stats.StartingBalance)
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
	expectedSize := 20 - core.PolymarketBuyFeeShares(20, 0.45, 50)
	if math.Abs(result.Size-expectedSize) > 1e-9 {
		t.Errorf("Expected net size %.6f, got %.6f", expectedSize, result.Size)
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

func TestRealTraderLiveBuyClearsV2FeeRate(t *testing.T) {
	client := &stubExchangeClient{
		orderResponse: &api.OrderResponse{Success: true, OrderID: "ord-1", Status: "live"},
	}
	trader := &RealTrader{
		client:              client,
		config:              &core.Config{MaxTradeSize: 10},
		startOfDay:          time.Now().Truncate(24 * time.Hour),
		livePositions:       make(map[string]float64),
		confirmedOrderFills: make(map[string]float64),
	}

	result, err := trader.Buy(context.Background(), "token-up", "Up", 0.50, 1.02, api.OrderTypeLimit, api.TIFGoodTilCancelled, 500)
	if err != nil {
		t.Fatalf("live buy failed: %v", err)
	}
	if result == nil || !result.Success {
		t.Fatalf("expected live buy success, got %+v", result)
	}
	if client.lastOrderRequest == nil {
		t.Fatal("expected live order request to be captured")
	}
	if client.lastOrderRequest.Size != 1.02 {
		t.Fatalf("expected live V2 order to submit requested 1.02 shares, got %.6f", client.lastOrderRequest.Size)
	}
	if client.lastOrderRequest.FeeRateBps != 0 {
		t.Fatalf("expected live V2 order to clear manual fee bps, got %d", client.lastOrderRequest.FeeRateBps)
	}
	if result.FeeRateBps != 0 || result.Fee != 0 {
		t.Fatalf("expected live V2 trade result to report no submitted manual fee, got feeRate=%d fee=%.6f", result.FeeRateBps, result.Fee)
	}
}

func TestEmbeddedPaperRealTraderSimulatesDirectFills(t *testing.T) {
	engine := paper.NewEngine(100.0)
	cfg := &core.Config{MaxTradeSize: 10}
	trader := NewEmbeddedPaperRealTrader(cfg, engine)
	trader.RegisterPaperToken("token-up", "BTC", "Up")
	engine.UpdateMarketBidAsk("BTC", "Up", 0.54, 0.55)

	if !trader.IsPaperMode() {
		t.Fatal("embedded paper real trader should report paper mode")
	}

	buy, err := trader.Buy(context.Background(), "token-up", "Up", 0.55, 3, api.OrderTypeLimit, api.TIFGoodTilCancelled, 1000)
	if err != nil {
		t.Fatalf("embedded paper buy failed: %v", err)
	}
	if !buy.Success {
		t.Fatalf("embedded paper buy should succeed: %+v", buy)
	}
	if math.Abs(buy.Price-0.55) > 1e-9 {
		t.Fatalf("expected embedded paper buy to use live ask 0.55, got %.4f", buy.Price)
	}
	expectedBuyQty := 3 - core.PolymarketBuyFeeShares(3, 0.55, 1000)
	if math.Abs(buy.AcknowledgedQty-3) > 1e-9 {
		t.Fatalf("expected gross acknowledged qty %.4f, got %.4f", 3.0, buy.AcknowledgedQty)
	}
	if math.Abs(buy.Size-3) > 1e-9 {
		t.Fatalf("expected gross result size %.4f, got %.4f", 3.0, buy.Size)
	}
	if got := trader.GetLivePositionSize("token-up"); math.Abs(got-expectedBuyQty) > 1e-9 {
		t.Fatalf("expected live position to be %.4f, got %.4f", expectedBuyQty, got)
	}
	if filled, err := trader.WaitForFill(context.Background(), buy.OrderID, time.Second); err != nil || !filled {
		t.Fatalf("expected embedded paper order to confirm immediately, filled=%v err=%v", filled, err)
	}

	positions, err := trader.ForceRefreshPositions(context.Background())
	if err != nil {
		t.Fatalf("embedded paper ForceRefreshPositions failed: %v", err)
	}
	if len(positions) != 1 || positions[0].TokenID != "token-up" || math.Abs(positions[0].Size-expectedBuyQty) > 1e-9 {
		t.Fatalf("unexpected embedded paper positions snapshot: %+v", positions)
	}

	sell, err := trader.Sell(context.Background(), "token-up", "Up", 0.50, 1.5, api.OrderTypeLimit, api.TIFFillAndKill, 1000)
	if err != nil {
		t.Fatalf("embedded paper sell failed: %v", err)
	}
	if !sell.Success {
		t.Fatalf("embedded paper sell should succeed: %+v", sell)
	}
	if math.Abs(sell.Price-0.54) > 1e-9 {
		t.Fatalf("expected embedded paper sell to use live bid 0.54, got %.4f", sell.Price)
	}
	expectedRemaining := expectedBuyQty - 1.5
	if got := trader.GetLivePositionSize("token-up"); math.Abs(got-expectedRemaining) > 1e-9 {
		t.Fatalf("expected live position to drop to %.4f, got %.4f", expectedRemaining, got)
	}
}

func TestEmbeddedPaperRealTraderZeroFeeKeepsRequestedBuyQuantity(t *testing.T) {
	engine := paper.NewEngine(100.0)
	cfg := &core.Config{MaxTradeSize: 10}
	trader := NewEmbeddedPaperRealTrader(cfg, engine)
	trader.RegisterPaperToken("token-up", "BTC", "Up")
	engine.UpdateMarketBidAsk("BTC", "Up", 0.43, 0.44)

	const requestedQty = 1.02
	buy, err := trader.Buy(context.Background(), "token-up", "Up", 0.44, requestedQty, api.OrderTypeLimit, api.TIFGoodTilCancelled, 0)
	if err != nil {
		t.Fatalf("embedded paper buy failed: %v", err)
	}
	if !buy.Success {
		t.Fatalf("embedded paper buy should succeed: %+v", buy)
	}
	if math.Abs(buy.AcknowledgedQty-requestedQty) > 1e-9 {
		t.Fatalf("expected zero-fee acknowledged qty %.4f, got %.4f", requestedQty, buy.AcknowledgedQty)
	}
	if got := trader.GetLivePositionSize("token-up"); math.Abs(got-requestedQty) > 1e-9 {
		t.Fatalf("expected live paper position %.4f, got %.4f", requestedQty, got)
	}
}

func TestEmbeddedPaperRealTraderUsesRegisteredFeeCurveInsteadOfSubmittedFeeCap(t *testing.T) {
	engine := paper.NewEngine(100.0)
	cfg := &core.Config{MaxTradeSize: 10}
	trader := NewEmbeddedPaperRealTrader(cfg, engine)
	trader.RegisterPaperToken("token-up", "BTC", "Up")
	trader.RegisterPaperTokenFeeCurve("token-up", core.PolymarketFeeCurve{})
	engine.UpdateMarketBidAsk("BTC", "Up", 0.43, 0.44)

	const requestedQty = 2.0
	buy, err := trader.Buy(context.Background(), "token-up", "Up", 0.44, requestedQty, api.OrderTypeLimit, api.TIFGoodTilCancelled, 1000)
	if err != nil {
		t.Fatalf("embedded paper buy failed: %v", err)
	}
	if !buy.Success {
		t.Fatalf("embedded paper buy should succeed: %+v", buy)
	}
	if buy.FeeRateBps != 0 {
		t.Fatalf("expected embedded paper to apply configured fee rate 0, got %d", buy.FeeRateBps)
	}
	if math.Abs(buy.AcknowledgedQty-requestedQty) > 1e-9 {
		t.Fatalf("expected embedded paper qty %.4f with zero configured fee, got %.4f", requestedQty, buy.AcknowledgedQty)
	}
}

func TestEmbeddedPaperRealTraderRejectsNonMarketableLimitAgainstLiveQuote(t *testing.T) {
	engine := paper.NewEngine(100.0)
	cfg := &core.Config{MaxTradeSize: 10}
	trader := NewEmbeddedPaperRealTrader(cfg, engine)
	trader.RegisterPaperToken("token-up", "BTC", "Up")
	engine.UpdateMarketBidAsk("BTC", "Up", 0.54, 0.55)

	buy, err := trader.Buy(context.Background(), "token-up", "Up", 0.50, 1.0, api.OrderTypeLimit, api.TIFGoodTilCancelled, 0)
	if err != nil {
		t.Fatalf("embedded paper buy returned unexpected error: %v", err)
	}
	if buy.Success {
		t.Fatalf("expected non-marketable embedded paper buy to fail, got %+v", buy)
	}
	if !strings.Contains(buy.Message, "not marketable") {
		t.Fatalf("expected non-marketable rejection, got %q", buy.Message)
	}
}

func TestEmbeddedPaperRealTraderRejectsOversell(t *testing.T) {
	engine := paper.NewEngine(100.0)
	cfg := &core.Config{MaxTradeSize: 10}
	trader := NewEmbeddedPaperRealTrader(cfg, engine)
	trader.RegisterPaperToken("token-up", "BTC", "Up")

	buy, err := trader.Buy(context.Background(), "token-up", "Up", 0.55, 1.5, api.OrderTypeLimit, api.TIFGoodTilCancelled, 1000)
	if err != nil {
		t.Fatalf("embedded paper buy failed: %v", err)
	}
	if !buy.Success {
		t.Fatalf("embedded paper buy should succeed: %+v", buy)
	}
	expectedRemaining := trader.GetLivePositionSize("token-up")

	sell, err := trader.Sell(context.Background(), "token-up", "Up", 0.60, 2.0, api.OrderTypeLimit, api.TIFFillAndKill, 1000)
	if err != nil {
		t.Fatalf("embedded paper oversell returned unexpected error: %v", err)
	}
	if sell.Success {
		t.Fatalf("expected embedded paper oversell to fail, got %+v", sell)
	}
	if !strings.Contains(sell.Message, "insufficient position") {
		t.Fatalf("expected insufficient position failure, got %q", sell.Message)
	}
	if got := trader.GetLivePositionSize("token-up"); math.Abs(got-expectedRemaining) > 1e-9 {
		t.Fatalf("expected live position to remain %.4f after rejected sell, got %.4f", expectedRemaining, got)
	}
}

func TestEmbeddedPaperRealTraderRejectsBuyAboveBalance(t *testing.T) {
	engine := paper.NewEngine(0.50)
	cfg := &core.Config{MaxTradeSize: 10}
	trader := NewEmbeddedPaperRealTrader(cfg, engine)
	trader.RegisterPaperToken("token-up", "BTC", "Up")

	buy, err := trader.Buy(context.Background(), "token-up", "Up", 0.60, 1.0, api.OrderTypeLimit, api.TIFGoodTilCancelled, 0)
	if err != nil {
		t.Fatalf("embedded paper buy returned unexpected error: %v", err)
	}
	if buy.Success {
		t.Fatalf("expected embedded paper buy above cash balance to fail, got %+v", buy)
	}
	if !strings.Contains(buy.Message, "insufficient balance") {
		t.Fatalf("expected insufficient balance failure, got %q", buy.Message)
	}
	if got := trader.GetLivePositionSize("token-up"); got != 0 {
		t.Fatalf("expected no live position after rejected buy, got %.2f", got)
	}
}

func TestEmbeddedPaperRealTraderBuySafetyUsesGrossNotionalAndNetsShares(t *testing.T) {
	engine := paper.NewEngine(10.0)
	cfg := &core.Config{MaxTradeSize: 1.00}
	trader := NewEmbeddedPaperRealTrader(cfg, engine)
	trader.RegisterPaperToken("token-up", "BTC", "Up")

	buy, err := trader.Buy(context.Background(), "token-up", "Up", 0.499, 2.0, api.OrderTypeLimit, api.TIFGoodTilCancelled, 1000)
	if err != nil {
		t.Fatalf("embedded paper fee-aware buy returned unexpected error: %v", err)
	}
	if !buy.Success {
		t.Fatalf("expected buy whose gross notional is below max trade size to succeed, got %+v", buy)
	}
	expectedQty := 2.0 - core.PolymarketBuyFeeShares(2.0, buy.Price, buy.FeeRateBps)
	if math.Abs(buy.Size-2.0) > 1e-9 || math.Abs(buy.AcknowledgedQty-2.0) > 1e-9 {
		t.Fatalf("expected gross shares in result, got size %.6f ack %.6f", buy.Size, buy.AcknowledgedQty)
	}
	if got := trader.GetLivePositionSize("token-up"); math.Abs(got-expectedQty) > 1e-9 {
		t.Fatalf("expected live paper position %.6f, got %.6f", expectedQty, got)
	}
}

func TestEmbeddedPaperExecuteBatchChecksAggregateBuySafety(t *testing.T) {
	engine := paper.NewEngine(10.0)
	cfg := &core.Config{MaxTradeSize: 1.00}
	trader := NewEmbeddedPaperRealTrader(cfg, engine)
	trader.RegisterPaperToken("token-down", "BTC", "Down")
	trader.RegisterPaperToken("token-up", "BTC", "Up")

	_, err := trader.ExecuteBatch(context.Background(), []*api.OrderRequest{
		{TokenID: "token-down", Outcome: "Down", Price: 0.30, Size: 2.0, Side: api.SideBuy, FeeRateBps: 0},
		{TokenID: "token-up", Outcome: "Up", Price: 0.30, Size: 2.0, Side: api.SideBuy, FeeRateBps: 0},
	})
	if err == nil {
		t.Fatal("expected embedded paper batch to enforce aggregate safety limits")
	}
	if !strings.Contains(err.Error(), "exceeds max trade size") {
		t.Fatalf("expected aggregate max trade size rejection, got %v", err)
	}
}

func TestEmbeddedPaperExecuteBatchUsesRegisteredFeeCurveForSafety(t *testing.T) {
	engine := paper.NewEngine(10.0)
	cfg := &core.Config{MaxTradeSize: 1.00}
	trader := NewEmbeddedPaperRealTrader(cfg, engine)
	trader.RegisterPaperToken("token-up", "BTC", "Up")
	trader.RegisterPaperTokenFeeCurve("token-up", core.PolymarketFeeCurve{})
	engine.UpdateMarketBidAsk("BTC", "Up", 0.49, 0.499)

	results, err := trader.ExecuteBatch(context.Background(), []*api.OrderRequest{
		{TokenID: "token-up", Outcome: "Up", Price: 0.499, Size: 2.0, Side: api.SideBuy, FeeRateBps: 1000},
	})
	if err != nil {
		t.Fatalf("expected embedded paper batch to use configured zero fee, got %v", err)
	}
	if len(results) != 1 || results[0] == nil || !results[0].Success {
		t.Fatalf("expected embedded paper batch buy to succeed, got %+v", results)
	}
	if math.Abs(results[0].AcknowledgedQty-2.0) > 1e-9 {
		t.Fatalf("expected batch acknowledged qty 2.0, got %.4f", results[0].AcknowledgedQty)
	}
}

func TestEmbeddedPaperRealTraderUsesRegisteredPolymarketFeeCurve(t *testing.T) {
	engine := paper.NewEngine(100.0)
	cfg := &core.Config{MaxTradeSize: 10}
	trader := NewEmbeddedPaperRealTrader(cfg, engine)
	trader.RegisterPaperToken("token-up", "BTC", "Up")
	trader.RegisterPaperTokenFeeCurve("token-up", core.PolymarketFeeCurve{Rate: 0.05, Exponent: 1})
	engine.UpdateMarketBidAsk("BTC", "Up", 0.49, 0.50)

	const requestedQty = 1.02
	buy, err := trader.Buy(context.Background(), "token-up", "Up", 0.50, requestedQty, api.OrderTypeLimit, api.TIFGoodTilCancelled, 3)
	if err != nil {
		t.Fatalf("embedded paper buy failed: %v", err)
	}
	if !buy.Success {
		t.Fatalf("embedded paper buy should succeed: %+v", buy)
	}
	expectedQty := requestedQty - core.PolymarketBuyFeeSharesForCurve(requestedQty, 0.50, core.PolymarketFeeCurve{Rate: 0.05, Exponent: 1})
	if math.Abs(buy.AcknowledgedQty-requestedQty) > 1e-9 {
		t.Fatalf("expected gross acknowledged qty %.6f, got %.6f", requestedQty, buy.AcknowledgedQty)
	}
	if got := trader.GetLivePositionSize("token-up"); math.Abs(got-expectedQty) > 1e-9 {
		t.Fatalf("expected fee-adjusted paper inventory %.6f, got %.6f", expectedQty, got)
	}
	if math.Abs(trader.GetConfirmedFillSize(buy.OrderID)-requestedQty) > 1e-9 {
		t.Fatalf("expected gross confirmed fill %.6f, got %.6f", requestedQty, trader.GetConfirmedFillSize(buy.OrderID))
	}
	if buy.FeeRateBps != 500 {
		t.Fatalf("expected displayed fee rate 500 bps from curve theta, got %d", buy.FeeRateBps)
	}
}

func TestRealTraderExecutionCostBasisTracksBuysAndSells(t *testing.T) {
	trader := &RealTrader{}

	trader.RecordExecutionBuy("token-up", 2.0, 0.62)
	trader.RecordExecutionBuy("token-up", 1.0, 0.45)

	qty, totalCost, avgPrice, ok := trader.GetPositionCostBasis("token-up")
	if !ok {
		t.Fatal("expected token-up cost basis to exist after buys")
	}
	if math.Abs(qty-3.0) > 0.000001 {
		t.Fatalf("expected quantity 3.0, got %.6f", qty)
	}
	if math.Abs(totalCost-1.07) > 0.000001 {
		t.Fatalf("expected total cost 1.07, got %.6f", totalCost)
	}
	if math.Abs(avgPrice-(1.07/3.0)) > 0.000001 {
		t.Fatalf("expected avg price %.6f, got %.6f", 1.07/3.0, avgPrice)
	}

	trader.RecordExecutionSell("token-up", 1.5)
	qty, totalCost, avgPrice, ok = trader.GetPositionCostBasis("token-up")
	if !ok {
		t.Fatal("expected remaining token-up cost basis after partial sell")
	}
	if math.Abs(qty-1.5) > 0.000001 {
		t.Fatalf("expected quantity 1.5 after sell, got %.6f", qty)
	}
	if math.Abs(totalCost-0.535) > 0.000001 {
		t.Fatalf("expected remaining total cost 0.535, got %.6f", totalCost)
	}
	if math.Abs(avgPrice-(1.07/3.0)) > 0.000001 {
		t.Fatalf("expected avg price %.6f after sell, got %.6f", 1.07/3.0, avgPrice)
	}

	trader.RecordExecutionSell("token-up", 1.5)
	if _, _, _, ok := trader.GetPositionCostBasis("token-up"); ok {
		t.Fatal("expected cost basis to clear after fully selling token-up")
	}
}

func TestDeriveAcknowledgedExecutionForMatchedBuy(t *testing.T) {
	resp := &api.OrderResponse{
		Status:       "matched",
		MakingAmount: "2150000",
		TakingAmount: "7885631",
	}

	qty, notional := deriveAcknowledgedExecution(resp, api.SideBuy)
	if qty != 7.885631 {
		t.Fatalf("expected acknowledged qty 7.885631, got %.6f", qty)
	}
	if notional != 2.15 {
		t.Fatalf("expected acknowledged notional 2.15, got %.6f", notional)
	}
}

func TestDeriveAcknowledgedExecutionForMatchedSell(t *testing.T) {
	resp := &api.OrderResponse{
		Status:       "MATCHED",
		MakingAmount: "7880000",
		TakingAmount: "3401000",
	}

	qty, notional := deriveAcknowledgedExecution(resp, api.SideSell)
	if qty != 7.88 {
		t.Fatalf("expected acknowledged sell qty 7.88, got %.6f", qty)
	}
	if notional != 3.401 {
		t.Fatalf("expected acknowledged sell notional 3.401, got %.6f", notional)
	}
}

func TestDeriveAcknowledgedExecutionIgnoresLiveOrder(t *testing.T) {
	resp := &api.OrderResponse{
		Status:       "live",
		MakingAmount: "2150000",
		TakingAmount: "5000000",
	}

	qty, notional := deriveAcknowledgedExecution(resp, api.SideBuy)
	if qty != 0 || notional != 0 {
		t.Fatalf("expected live order to have no acknowledged execution, got qty=%.6f notional=%.6f", qty, notional)
	}
}

func TestDeriveAcknowledgedExecutionAcceptsDecimalAmounts(t *testing.T) {
	resp := &api.OrderResponse{
		Status:       "matched",
		MakingAmount: "3.189998",
		TakingAmount: "3.2551",
	}

	qty, notional := deriveAcknowledgedExecution(resp, api.SideBuy)
	if qty != 3.2551 {
		t.Fatalf("expected acknowledged qty 3.2551, got %.6f", qty)
	}
	if notional != 3.189998 {
		t.Fatalf("expected acknowledged notional 3.189998, got %.6f", notional)
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

func TestRealTraderWaitForFill_UsesWSOnly(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(api.OpenOrder{
			OrderID:       "order-1",
			Status:        "CONFIRMED",
			OriginalSize:  5,
			RemainingSize: 0,
		})
	}))
	defer server.Close()

	client, err := api.NewCLOBClient("0000000000000000000000000000000000000000000000000000000000000001", "key", "secret", "pass")
	if err != nil {
		t.Fatalf("NewCLOBClient failed: %v", err)
	}
	client.BaseURL = server.URL

	trader := &RealTrader{
		client:              client,
		livePositions:       make(map[string]float64),
		confirmedOrderFills: make(map[string]float64),
	}

	go func() {
		time.Sleep(15 * time.Millisecond)
		trader.applyLiveFill(api.OrderFillData{OrderID: "order-1", AssetID: "asset-1", Side: "BUY", Size: "2"})
	}()

	filled, err := trader.WaitForFill(context.Background(), "order-1", 300*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForFill failed: %v", err)
	}
	if !filled {
		t.Fatal("expected websocket-confirmed fill to satisfy wait")
	}
	if requests.Load() != 0 {
		t.Fatalf("expected no order polling while waiting on WS fill, got %d requests", requests.Load())
	}
}

func TestRealTraderWaitForFillTimeoutDoesNotPollREST(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(api.OpenOrder{
			OrderID:       "order-2",
			Status:        "OPEN",
			OriginalSize:  5,
			RemainingSize: 5,
		})
	}))
	defer server.Close()

	client, err := api.NewCLOBClient("0000000000000000000000000000000000000000000000000000000000000001", "key", "secret", "pass")
	if err != nil {
		t.Fatalf("NewCLOBClient failed: %v", err)
	}
	client.BaseURL = server.URL

	trader := &RealTrader{
		client:              client,
		livePositions:       make(map[string]float64),
		confirmedOrderFills: make(map[string]float64),
	}

	filled, err := trader.WaitForFill(context.Background(), "order-2", 75*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForFill failed: %v", err)
	}
	if filled {
		t.Fatal("expected timeout without websocket-confirmed fill")
	}
	if requests.Load() != 0 {
		t.Fatalf("expected WS-only timeout with no order polling, got %d requests", requests.Load())
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

	now := time.Now().UTC()
	startOfDay := now.Truncate(24 * time.Hour)

	// Verify truncation works as expected
	if startOfDay.Hour() != 0 || startOfDay.Minute() != 0 || startOfDay.Second() != 0 {
		t.Errorf("Truncate should give start of day (00:00:00), got %02d:%02d:%02d",
			startOfDay.Hour(), startOfDay.Minute(), startOfDay.Second())
	}

	// Next day should be different
	tomorrow := now.Add(24 * time.Hour).Truncate(24 * time.Hour)
	if tomorrow.Equal(startOfDay) {
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

	positions := trader.GetLivePositionsSnapshot()

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
		t.Fatalf("expected only one positive position from live WS cache, got %d", len(positions))
	}
	if positions[0].TokenID != "asset-1" || positions[0].Size != 1 {
		t.Fatalf("unexpected live WS cache payload: %+v", positions[0])
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

	positions := trader.GetLivePositionsSnapshot()
	if len(positions) != 1 || positions[0].TokenID != "asset-1" || positions[0].Size != 2 {
		t.Fatalf("expected unchanged live WS cache position, got %+v", positions)
	}
	if got := trader.GetConfirmedFillSize("bad-order"); got != 0 {
		t.Fatalf("expected invalid fill not to be recorded, got %.2f", got)
	}
}

func TestRealTrader_GetPositionsRequiresExternalSnapshot(t *testing.T) {
	trader := &RealTrader{}

	if _, err := trader.GetPositions(context.Background()); err == nil {
		t.Fatal("expected GetPositions to require an external snapshot source")
	}
}

func TestRealTrader_GetBalancePrefersOnChainUSDC(t *testing.T) {
	usdcRaw := big.NewInt(68284432)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":"0x%064x"}`, usdcRaw)))
	}))
	defer server.Close()

	trader := &RealTrader{
		client: &stubExchangeClient{
			balanceAllowance: &api.BalanceAllowance{
				Balance:   99999999,
				Allowance: 99999999,
			},
			address: "0x1111111111111111111111111111111111111111",
		},
		polygon: api.NewPolygonClient(server.URL),
		config:  &core.Config{Exchange: "polymarket"},
	}

	balance, err := trader.GetBalance(context.Background())
	if err != nil {
		t.Fatalf("GetBalance failed: %v", err)
	}
	if balance != 68.284432 {
		t.Fatalf("expected on-chain USDC balance 68.284432, got %.6f", balance)
	}
}

func TestRealTrader_GetBalancePrefersLowerExchangeBalanceWhenOnChainIsHigher(t *testing.T) {
	usdcRaw := big.NewInt(68284432)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":"0x%064x"}`, usdcRaw)))
	}))
	defer server.Close()

	trader := &RealTrader{
		client: &stubExchangeClient{
			balanceAllowance: &api.BalanceAllowance{
				Balance:   49.93,
				Allowance: 99999999,
			},
			address: "0x1111111111111111111111111111111111111111",
		},
		polygon: api.NewPolygonClient(server.URL),
		config:  &core.Config{Exchange: "polymarket"},
	}

	balance, err := trader.ForceRefreshBalance(context.Background())
	if err != nil {
		t.Fatalf("GetBalance failed: %v", err)
	}
	if balance != 49.93 {
		t.Fatalf("expected conservative balance 49.93, got %.6f", balance)
	}
}

func TestRealTrader_GetOnChainTxStatePending(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req api.RPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		switch req.Method {
		case "eth_getTransactionReceipt":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":null}`))
		case "eth_getTransactionByHash":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"hash":"0xabc","blockNumber":"0x"}}`))
		default:
			t.Fatalf("unexpected method %s", req.Method)
		}
	}))
	defer server.Close()

	trader := &RealTrader{polygon: api.NewPolygonClient(server.URL)}
	state, err := trader.GetOnChainTxState(context.Background(), "0xabc")
	if err != nil {
		t.Fatalf("GetOnChainTxState failed: %v", err)
	}
	if state != "pending" {
		t.Fatalf("expected pending state, got %q", state)
	}
}

func TestRealTrader_GetOnChainTxStateSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req api.RPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Method != "eth_getTransactionReceipt" {
			t.Fatalf("unexpected method %s", req.Method)
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"status":"0x1","blockNumber":"0x1","gasUsed":"0x5208","transactionHash":"0xabc","logs":[]}}`))
	}))
	defer server.Close()

	trader := &RealTrader{polygon: api.NewPolygonClient(server.URL)}
	state, err := trader.GetOnChainTxState(context.Background(), "0xabc")
	if err != nil {
		t.Fatalf("GetOnChainTxState failed: %v", err)
	}
	if state != "success" {
		t.Fatalf("expected success state, got %q", state)
	}
}

func TestRealTrader_RefreshStateAfterRedeemClearsCTFCacheAndRefreshesBalance(t *testing.T) {
	trader := &RealTrader{
		client: &stubExchangeClient{
			balanceAllowance: &api.BalanceAllowance{
				Balance:   42.5,
				Allowance: 42.5,
			},
			address: "0x1111111111111111111111111111111111111111",
		},
		config:            &core.Config{Exchange: "polymarket"},
		cachedBalance:     10.0,
		lastBalanceUpdate: time.Now(),
		ctfBalanceCache: map[string]float64{
			"token-a": 3.0,
		},
		lastCTFBalanceUpdate: map[string]time.Time{
			"token-a": time.Now(),
		},
	}

	trader.refreshStateAfterRedeem(context.Background())

	if got := len(trader.ctfBalanceCache); got != 0 {
		t.Fatalf("expected CTF balance cache to be cleared, got %d entries", got)
	}
	if got := len(trader.lastCTFBalanceUpdate); got != 0 {
		t.Fatalf("expected CTF balance timestamps to be cleared, got %d entries", got)
	}
	if trader.lastBalanceUpdate.IsZero() {
		t.Fatal("expected balance refresh to repopulate lastBalanceUpdate")
	}
	if trader.cachedBalance != 42.5 {
		t.Fatalf("expected refreshed cached balance 42.5, got %.2f", trader.cachedBalance)
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

func TestRealTrader_ApplyLiveAssetBalanceOverridesDriftedWSSize(t *testing.T) {
	trader := &RealTrader{
		livePositions: map[string]float64{
			"yes-token": 2.00,
		},
		confirmedOrderFills: make(map[string]float64),
	}

	trader.applyLiveFill(api.OrderFillData{AssetID: "yes-token", Side: "BUY", Size: "1.50"})
	if got := trader.GetLivePositionSize("yes-token"); got != 3.50 {
		t.Fatalf("expected incremental fill update to reach 3.50, got %.2f", got)
	}

	trader.applyLiveAssetBalance("yes-token", "2.25")
	if got := trader.GetLivePositionSize("yes-token"); got != 2.25 {
		t.Fatalf("expected asset balance snapshot to replace live size with 2.25, got %.2f", got)
	}

	trader.applyLiveAssetBalance("yes-token", "-1")
	if got := trader.GetLivePositionSize("yes-token"); got != 0 {
		t.Fatalf("expected negative asset balance to clamp to 0, got %.2f", got)
	}
}

func TestRealTrader_ForceRefreshPositionsRebuildsLiveCacheFromExternalSnapshot(t *testing.T) {
	trader := &RealTrader{
		client: &stubExchangeClient{
			positions: []api.Position{
				{TokenID: "yes-token", Size: 1.25, Outcome: "Yes", ConditionID: "cond-1"},
				{TokenID: "no-token", Size: 0.75, Outcome: "No", ConditionID: "cond-1"},
			},
		},
		livePositions: map[string]float64{
			"stale-token": 99,
		},
		confirmedOrderFills: make(map[string]float64),
	}

	positions, err := trader.ForceRefreshPositions(context.Background())
	if err != nil {
		t.Fatalf("ForceRefreshPositions failed: %v", err)
	}
	if len(positions) != 2 {
		t.Fatalf("expected 2 refreshed positions, got %d", len(positions))
	}
	if got := trader.GetLivePositionSize("yes-token"); got != 1.25 {
		t.Fatalf("expected yes-token size 1.25 after refresh, got %.2f", got)
	}
	if got := trader.GetLivePositionSize("no-token"); got != 0.75 {
		t.Fatalf("expected no-token size 0.75 after refresh, got %.2f", got)
	}
	if got := trader.GetLivePositionSize("stale-token"); got != 0 {
		t.Fatalf("expected stale-token to be cleared by external refresh, got %.2f", got)
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

func TestRealTrader_ForceRefreshBalanceThrottlesOnChainUSDCReads(t *testing.T) {
	var rpcCalls int32
	usdcRaw := big.NewInt(68284432)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&rpcCalls, 1)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":"0x%064x"}`, usdcRaw)))
	}))
	defer server.Close()

	trader := &RealTrader{
		client: &stubExchangeClient{
			balanceAllowance: &api.BalanceAllowance{
				Balance:   99999999,
				Allowance: 99999999,
			},
			address: "0x1111111111111111111111111111111111111111",
		},
		polygon: api.NewPolygonClient(server.URL),
		config:  &core.Config{Exchange: "polymarket"},
	}

	for i := 0; i < 5; i++ {
		balance, err := trader.ForceRefreshBalance(context.Background())
		if err != nil {
			t.Fatalf("ForceRefreshBalance failed on iteration %d: %v", i, err)
		}
		if balance != 68.284432 {
			t.Fatalf("expected on-chain USDC balance 68.284432, got %.6f", balance)
		}
	}

	if calls := atomic.LoadInt32(&rpcCalls); calls != 1 {
		t.Fatalf("expected exactly 1 on-chain balance RPC, got %d", calls)
	}
}

func TestRealTrader_ForceRefreshBalanceBacksOffOnChainRetryAfterFailure(t *testing.T) {
	var rpcCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&rpcCalls, 1)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"rate limit"}}`))
	}))
	defer server.Close()

	trader := &RealTrader{
		client: &stubExchangeClient{
			balanceAllowance: &api.BalanceAllowance{
				Balance:   42.5,
				Allowance: 42.5,
			},
			address: "0x1111111111111111111111111111111111111111",
		},
		polygon: api.NewPolygonClient(server.URL),
		config:  &core.Config{Exchange: "polymarket"},
	}

	for i := 0; i < 2; i++ {
		balance, err := trader.ForceRefreshBalance(context.Background())
		if err != nil {
			t.Fatalf("ForceRefreshBalance failed on iteration %d: %v", i, err)
		}
		if balance != 42.5 {
			t.Fatalf("expected exchange fallback balance 42.5, got %.2f", balance)
		}
	}

	if calls := atomic.LoadInt32(&rpcCalls); calls != 1 {
		t.Fatalf("expected on-chain retry backoff to limit calls to 1, got %d", calls)
	}
}

func TestRealTrader_ForceRefreshCTFBalanceFloatBypassesCacheTTL(t *testing.T) {
	var rpcCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := atomic.AddInt32(&rpcCalls, 1)
		rawBalance := big.NewInt(2500000)
		if call > 1 {
			rawBalance = big.NewInt(3500000)
		}
		_, _ = w.Write([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":"0x%064x"}`, rawBalance)))
	}))
	defer server.Close()

	trader := &RealTrader{
		client:  &stubExchangeClient{address: "0x1111111111111111111111111111111111111111"},
		polygon: api.NewPolygonClient(server.URL),
	}

	bal1, err := trader.GetCTFBalanceFloat(context.Background(), "12345")
	if err != nil {
		t.Fatalf("GetCTFBalanceFloat failed: %v", err)
	}
	if bal1 != 2.5 {
		t.Fatalf("expected cached balance 2.5 shares, got %.6f", bal1)
	}

	bal2, err := trader.ForceRefreshCTFBalanceFloat(context.Background(), "12345")
	if err != nil {
		t.Fatalf("ForceRefreshCTFBalanceFloat failed: %v", err)
	}
	if bal2 != 3.5 {
		t.Fatalf("expected refreshed balance 3.5 shares, got %.6f", bal2)
	}

	if calls := atomic.LoadInt32(&rpcCalls); calls != 2 {
		t.Fatalf("expected force refresh to trigger a second RPC call, got %d", calls)
	}
}

func TestRealTrader_GetCTFBalanceFloatUsesCacheTTL(t *testing.T) {
	var rpcCalls int32
	rawBalance := big.NewInt(2500000) // 2.5 shares in 6-decimal units
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&rpcCalls, 1)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":"0x%064x"}`, rawBalance)))
	}))
	defer server.Close()

	trader := &RealTrader{
		client:  &stubExchangeClient{address: "0x1111111111111111111111111111111111111111"},
		polygon: api.NewPolygonClient(server.URL),
	}

	for i := 0; i < 3; i++ {
		bal, err := trader.GetCTFBalanceFloat(context.Background(), "12345")
		if err != nil {
			t.Fatalf("GetCTFBalanceFloat failed on iteration %d: %v", i, err)
		}
		if bal != 2.5 {
			t.Fatalf("expected 2.5 shares, got %.6f", bal)
		}
	}

	if calls := atomic.LoadInt32(&rpcCalls); calls != 1 {
		t.Fatalf("expected 1 RPC call due CTF cache TTL, got %d", calls)
	}
}

func TestRealTrader_GetCTFBalanceFloatBacksOffAfterFailure(t *testing.T) {
	var rpcCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&rpcCalls, 1)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"rate limit"}}`))
	}))
	defer server.Close()

	trader := &RealTrader{
		client:  &stubExchangeClient{address: "0x1111111111111111111111111111111111111111"},
		polygon: api.NewPolygonClient(server.URL),
	}

	if _, err := trader.GetCTFBalanceFloat(context.Background(), "54321"); err == nil {
		t.Fatal("expected first CTF balance fetch to fail")
	}
	if _, err := trader.GetCTFBalanceFloat(context.Background(), "54321"); err == nil {
		t.Fatal("expected second CTF balance fetch to be throttled/fail")
	}

	if calls := atomic.LoadInt32(&rpcCalls); calls != 1 {
		t.Fatalf("expected retry backoff to limit CTF RPC calls to 1, got %d", calls)
	}
}
