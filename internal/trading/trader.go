package trading

import (
	"context"
	"fmt"
	"sync"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
)

// Trader defines the interface for placing trades (paper or real)
type Trader interface {
	// Buy places a buy order
	Buy(ctx context.Context, tokenID, outcome string, price, size float64, tif api.TimeInForce) (*TradeResult, error)

	// Sell places a sell order
	Sell(ctx context.Context, tokenID, outcome string, price, size float64, tif api.TimeInForce) (*TradeResult, error)

	// CancelOrder cancels an existing order
	CancelOrder(ctx context.Context, orderID string) error

	// CancelAll cancels all open orders
	CancelAll(ctx context.Context) error

	// GetBalance returns the current available balance
	GetBalance(ctx context.Context) (float64, error)

	// GetPositions returns current positions
	GetPositions(ctx context.Context) ([]PositionInfo, error)

	// IsPaperMode returns true if this is paper trading
	IsPaperMode() bool

	// IsDryRun returns true if in dry-run mode (simulating real API calls)
	IsDryRun() bool

	// GetMarketInfo retrieves market info including resolution status
	GetMarketInfo(ctx context.Context, conditionID string) (*api.MarketInfo, error)
}

// TradeResult represents the result of a trade attempt
type TradeResult struct {
	OrderID   string
	Status    string
	Success   bool
	Message   string
	Price     float64
	Size      float64
	Side      string
	TokenID   string
	Outcome   string
	Timestamp time.Time
}

// PositionInfo represents a position
type PositionInfo struct {
	TokenID  string
	Outcome  string
	Size     float64
	AvgPrice float64
}

// PaperTrader implements Trader for paper trading
type PaperTrader struct {
	engine    *paper.Engine
	orderBook *paper.OrderBook
}

// NewPaperTrader creates a new paper trader
func NewPaperTrader(engine *paper.Engine, orderBook *paper.OrderBook) *PaperTrader {
	return &PaperTrader{
		engine:    engine,
		orderBook: orderBook,
	}
}

func (t *PaperTrader) Buy(ctx context.Context, tokenID, outcome string, price, size float64, tif api.TimeInForce) (*TradeResult, error) {
	cost := price * size
	_, err := t.engine.Buy(outcome, price, size)
	if err != nil {
		return &TradeResult{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	return &TradeResult{
		OrderID:   fmt.Sprintf("paper-%d", time.Now().UnixNano()),
		Status:    "FILLED",
		Success:   true,
		Price:     price,
		Size:      size,
		Side:      "BUY",
		TokenID:   tokenID,
		Outcome:   outcome,
		Timestamp: time.Now(),
		Message:   fmt.Sprintf("Bought %.2f %s @ $%.4f (cost: $%.2f)", size, outcome, price, cost),
	}, nil
}

func (t *PaperTrader) Sell(ctx context.Context, tokenID, outcome string, price, size float64, tif api.TimeInForce) (*TradeResult, error) {
	_, err := t.engine.Sell(outcome, price, size)
	if err != nil {
		return &TradeResult{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	return &TradeResult{
		OrderID:   fmt.Sprintf("paper-%d", time.Now().UnixNano()),
		Status:    "FILLED",
		Success:   true,
		Price:     price,
		Size:      size,
		Side:      "SELL",
		TokenID:   tokenID,
		Outcome:   outcome,
		Timestamp: time.Now(),
		Message:   fmt.Sprintf("Sold %.2f %s @ $%.4f", size, outcome, price),
	}, nil
}

func (t *PaperTrader) CancelOrder(ctx context.Context, orderID string) error {
	// Paper trading doesn't track individual orders in the same way
	return nil
}

func (t *PaperTrader) CancelAll(ctx context.Context) error {
	return nil
}

func (t *PaperTrader) GetBalance(ctx context.Context) (float64, error) {
	return t.engine.GetBalance(), nil
}

func (t *PaperTrader) GetPositions(ctx context.Context) ([]PositionInfo, error) {
	enginePositions := t.engine.GetPositions()

	positions := make([]PositionInfo, 0, len(enginePositions))
	for _, pos := range enginePositions {
		positions = append(positions, PositionInfo{
			Outcome:  pos.Outcome,
			Size:     pos.Quantity,
			AvgPrice: pos.AvgPrice,
		})
	}
	return positions, nil
}

func (t *PaperTrader) IsPaperMode() bool {
	return true
}

func (t *PaperTrader) IsDryRun() bool {
	return false
}

func (t *PaperTrader) GetMarketInfo(ctx context.Context, conditionID string) (*api.MarketInfo, error) {
	// Paper trader doesn't have real market info access
	return nil, fmt.Errorf("not implemented for paper trader")
}

// RealTrader implements Trader for real Polymarket trading
type RealTrader struct {
	clob              *api.CLOBClient
	polygon           *api.PolygonClient
	config            *core.Config
	mu                sync.Mutex
	dailyLoss         float64
	startOfDay        time.Time
	cachedBalance     float64
	lastBalanceUpdate time.Time
}

// NewRealTrader creates a new real trader
func NewRealTrader(cfg *core.Config) (*RealTrader, error) {
	if err := cfg.ValidateForRealTrading(); err != nil {
		return nil, err
	}

	clob, err := api.NewCLOBClient(cfg.PK, cfg.APIKey, cfg.APISecret, cfg.APIPassphrase)
	if err != nil {
		return nil, fmt.Errorf("failed to create CLOB client: %w", err)
	}

	// Enable dry-run mode if configured
	if cfg.DryRunFirst {
		clob.SetDryRun(true)
	}

	polygon := api.NewPolygonClient(cfg.PolygonRPCURL)

	return &RealTrader{
		clob:       clob,
		polygon:    polygon,
		config:     cfg,
		startOfDay: time.Now().Truncate(24 * time.Hour),
	}, nil
}

// SetDryRun enables/disables dry run mode
func (t *RealTrader) SetDryRun(enabled bool) {
	t.clob.SetDryRun(enabled)
}

func (t *RealTrader) Buy(ctx context.Context, tokenID, outcome string, price, size float64, tif api.TimeInForce) (*TradeResult, error) {
	// Check safety limits
	cost := price * size
	if err := t.checkSafetyLimits(cost); err != nil {
		return &TradeResult{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	resp, err := t.clob.PlaceOrder(ctx, &api.OrderRequest{
		TokenID:     tokenID,
		Price:       price,
		Size:        size,
		Side:        api.SideBuy,
		OrderType:   api.OrderTypeLimit,
		TimeInForce: tif,
	})
	if err != nil {
		return &TradeResult{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	if resp.Success {
		t.mu.Lock()
		if t.cachedBalance > 0 {
			t.cachedBalance -= cost
		}
		t.mu.Unlock()
	}

	status := "PENDING"
	if t.clob.IsDryRun() {
		status = "DRY_RUN"
	}

	return &TradeResult{
		OrderID:   resp.OrderID,
		Status:    status,
		Success:   resp.Success,
		Price:     price,
		Size:      size,
		Side:      "BUY",
		TokenID:   tokenID,
		Outcome:   outcome,
		Timestamp: time.Now(),
		Message:   resp.ErrorMsg,
	}, nil
}

func (t *RealTrader) Sell(ctx context.Context, tokenID, outcome string, price, size float64, tif api.TimeInForce) (*TradeResult, error) {
	resp, err := t.clob.PlaceOrder(ctx, &api.OrderRequest{
		TokenID:     tokenID,
		Price:       price,
		Size:        size,
		Side:        api.SideSell,
		OrderType:   api.OrderTypeLimit,
		TimeInForce: tif,
	})
	if err != nil {
		return &TradeResult{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	if resp.Success {
		t.mu.Lock()
		if t.cachedBalance > 0 {
			t.cachedBalance += (price * size)
		}
		t.mu.Unlock()
	}

	status := "PENDING"
	if t.clob.IsDryRun() {
		status = "DRY_RUN"
	}

	return &TradeResult{
		OrderID:   resp.OrderID,
		Status:    status,
		Success:   resp.Success,
		Price:     price,
		Size:      size,
		Side:      "SELL",
		TokenID:   tokenID,
		Outcome:   outcome,
		Timestamp: time.Now(),
		Message:   resp.ErrorMsg,
	}, nil
}

func (t *RealTrader) CancelOrder(ctx context.Context, orderID string) error {
	return t.clob.CancelOrder(ctx, orderID)
}

func (t *RealTrader) CancelAll(ctx context.Context) error {
	return t.clob.CancelAllOrders(ctx)
}

func (t *RealTrader) GetBalance(ctx context.Context) (float64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Only poll every 30 seconds to avoid rate limits, or if never polled
	if time.Since(t.lastBalanceUpdate) < 30*time.Second && t.lastBalanceUpdate.IsZero() == false {
		return t.cachedBalance, nil
	}

	bal, err := t.polygon.GetUSDCBalance(ctx, t.clob.Address())
	if err != nil {
		// Return cached balance on error if available
		if !t.lastBalanceUpdate.IsZero() {
			return t.cachedBalance, nil
		}
		return 0, err
	}

	t.cachedBalance = bal
	t.lastBalanceUpdate = time.Now()
	return bal, nil
}

func (t *RealTrader) GetPositions(ctx context.Context) ([]PositionInfo, error) {
	positions, err := t.clob.GetPositions(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]PositionInfo, len(positions))
	for i, pos := range positions {
		result[i] = PositionInfo{
			TokenID:  pos.TokenID,
			Size:     pos.Size,
			AvgPrice: pos.AvgPrice,
		}
	}
	return result, nil
}

func (t *RealTrader) IsPaperMode() bool {
	return false
}

func (t *RealTrader) IsDryRun() bool {
	return t.clob.IsDryRun()
}

func (t *RealTrader) GetMarketInfo(ctx context.Context, conditionID string) (*api.MarketInfo, error) {
	return t.clob.GetMarketInfo(ctx, conditionID)
}

// checkSafetyLimits verifies the trade doesn't exceed safety limits
func (t *RealTrader) checkSafetyLimits(tradeAmount float64) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Reset daily loss if new day
	if time.Now().Truncate(24*time.Hour) != t.startOfDay {
		t.dailyLoss = 0
		t.startOfDay = time.Now().Truncate(24 * time.Hour)
	}

	// Check max trade size
	if t.config.MaxTradeSize > 0 && tradeAmount > t.config.MaxTradeSize {
		return fmt.Errorf("trade amount $%.2f exceeds max trade size $%.2f", tradeAmount, t.config.MaxTradeSize)
	}

	// Check daily loss limit
	if t.config.MaxDailyLoss > 0 && t.dailyLoss >= t.config.MaxDailyLoss {
		return fmt.Errorf("daily loss limit of $%.2f reached", t.config.MaxDailyLoss)
	}

	return nil
}

// RecordLoss records a loss for daily tracking
func (t *RealTrader) RecordLoss(amount float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dailyLoss += amount
}

// Address returns the wallet address
func (t *RealTrader) Address() string {
	return t.clob.Address()
}

// WaitForFill waits for an order to be filled
func (t *RealTrader) WaitForFill(ctx context.Context, orderID string, timeout time.Duration) (bool, error) {
	return t.clob.WaitForFill(ctx, orderID, timeout)
}

// GetOpenOrders returns all open orders
func (t *RealTrader) GetOpenOrders(ctx context.Context) ([]api.OpenOrder, error) {
	return t.clob.GetOpenOrders(ctx)
}

// CancelOrder cancels a specific order
func (t *RealTrader) CancelOrderByID(ctx context.Context, orderID string) error {
	return t.clob.CancelOrder(ctx, orderID)
}

// BuyWithConfirmation places a buy order and waits for fill confirmation
// Returns the result and whether the order was confirmed filled
func (t *RealTrader) BuyWithConfirmation(ctx context.Context, tokenID, outcome string, price, size float64, tif api.TimeInForce, fillTimeout time.Duration) (*TradeResult, bool, error) {
	result, err := t.Buy(ctx, tokenID, outcome, price, size, tif)
	if err != nil {
		return result, false, err
	}

	if !result.Success {
		return result, false, nil
	}

	// Wait for fill confirmation
	filled, err := t.WaitForFill(ctx, result.OrderID, fillTimeout)
	if err != nil {
		return result, false, err
	}

	if !filled {
		// Order didn't fill in time - cancel it
		_ = t.CancelOrderByID(ctx, result.OrderID)
		result.Success = false
		result.Status = "TIMEOUT"
		result.Message = "Order did not fill within timeout, cancelled"
	}

	return result, filled, nil
}

// NewTrader creates the appropriate trader based on config
func NewTrader(cfg *core.Config, engine *paper.Engine, orderBook *paper.OrderBook) (Trader, error) {
	if cfg.IsPaperMode() {
		return NewPaperTrader(engine, orderBook), nil
	}
	return NewRealTrader(cfg)
}
