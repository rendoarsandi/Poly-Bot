package trading

import (
	"context"
	"fmt"
	"math/big"
	"sync"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
)

// Trader defines the interface for placing trades (paper or real)
type Trader interface {
	// Buy places a buy order
	Buy(ctx context.Context, tokenID, outcome string, price, size float64, orderType api.OrderType, tif api.TimeInForce, feeRateBps int) (*TradeResult, error)

	// Sell places a sell order
	Sell(ctx context.Context, tokenID, outcome string, price, size float64, orderType api.OrderType, tif api.TimeInForce, feeRateBps int) (*TradeResult, error)

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

	// IsTestMode returns true if in test mode (validating but not submitting orders)
	IsTestMode() bool

	// GetMarketInfo retrieves market info including resolution status
	GetMarketInfo(ctx context.Context, conditionID string) (*api.MarketInfo, error)
}

// TradeResult represents the result of a trade attempt
type TradeResult struct {
	OrderID    string
	Status     string
	Success    bool
	Message    string
	Price      float64
	Size       float64
	Fee        float64
	FeeRateBps int
	Side       string
	TokenID    string
	Outcome    string
	Timestamp  time.Time
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

func (t *PaperTrader) Buy(ctx context.Context, tokenID, outcome string, price, size float64, orderType api.OrderType, tif api.TimeInForce, feeRateBps int) (*TradeResult, error) {
	cost := price * size
	// Calculate simulated fee
	fee := 0.0
	if feeRateBps > 0 {
		fee = cost * (float64(feeRateBps) / 10000.0)
	}

	_, err := t.engine.Buy(outcome, price, size)
	if err != nil {
		return &TradeResult{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	return &TradeResult{
		OrderID:    fmt.Sprintf("paper-%d", time.Now().UnixNano()),
		Status:     "FILLED",
		Success:    true,
		Price:      price,
		Size:       size,
		Fee:        fee,
		FeeRateBps: feeRateBps,
		Side:       "BUY",
		TokenID:    tokenID,
		Outcome:    outcome,
		Timestamp:  time.Now(),
		Message:    fmt.Sprintf("Bought %.2f %s @ $%.4f (cost: $%.2f, fee: $%.4f)", size, outcome, price, cost, fee),
	}, nil
}

func (t *PaperTrader) Sell(ctx context.Context, tokenID, outcome string, price, size float64, orderType api.OrderType, tif api.TimeInForce, feeRateBps int) (*TradeResult, error) {
	// Calculate simulated fee
	fee := 0.0
	if feeRateBps > 0 {
		proceeds := price * size
		fee = proceeds * (float64(feeRateBps) / 10000.0)
	}

	_, err := t.engine.Sell(outcome, price, size)
	if err != nil {
		return &TradeResult{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	return &TradeResult{
		OrderID:    fmt.Sprintf("paper-%d", time.Now().UnixNano()),
		Status:     "FILLED",
		Success:    true,
		Price:      price,
		Size:       size,
		Fee:        fee,
		FeeRateBps: feeRateBps,
		Side:       "SELL",
		TokenID:    tokenID,
		Outcome:    outcome,
		Timestamp:  time.Now(),
		Message:    fmt.Sprintf("Sold %.2f %s @ $%.4f (fee: $%.4f)", size, outcome, price, fee),
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

func (t *PaperTrader) IsTestMode() bool {
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

	// Enable test mode if configured
	if cfg.TestMode {
		clob.SetTestMode(true)
	}

	polygon := api.NewPolygonClient(cfg.PolygonRPCURL)

	return &RealTrader{
		clob:       clob,
		polygon:    polygon,
		config:     cfg,
		startOfDay: time.Now().Truncate(24 * time.Hour),
	}, nil
}

// SetTestMode enables/disables test mode
func (t *RealTrader) SetTestMode(enabled bool) {
	t.clob.SetTestMode(enabled)
}

func (t *RealTrader) Buy(ctx context.Context, tokenID, outcome string, price, size float64, orderType api.OrderType, tif api.TimeInForce, feeRateBps int) (*TradeResult, error) {
	// Check safety limits
	cost := price * size
	// Add estimated fee to cost check
	fee := 0.0
	if feeRateBps > 0 {
		fee = cost * (float64(feeRateBps) / 10000.0)
	}

	if err := t.checkSafetyLimits(cost + fee); err != nil {
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
		OrderType:   orderType,
		TimeInForce: tif,
		FeeRateBps:  feeRateBps,
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
			t.cachedBalance -= (cost + fee)
		}
		t.mu.Unlock()
	}

	status := "PENDING"
	if t.clob.IsTestMode() {
		status = "TEST"
	}

	return &TradeResult{
		OrderID:    resp.OrderID,
		Status:     status,
		Success:    resp.Success,
		Price:      price,
		Size:       size,
		Fee:        fee,
		FeeRateBps: feeRateBps,
		Side:       "BUY",
		TokenID:    tokenID,
		Outcome:    outcome,
		Timestamp:  time.Now(),
		Message:    resp.ErrorMsg,
	}, nil
}

func (t *RealTrader) Sell(ctx context.Context, tokenID, outcome string, price, size float64, orderType api.OrderType, tif api.TimeInForce, feeRateBps int) (*TradeResult, error) {
	resp, err := t.clob.PlaceOrder(ctx, &api.OrderRequest{
		TokenID:     tokenID,
		Price:       price,
		Size:        size,
		Side:        api.SideSell,
		OrderType:   orderType,
		TimeInForce: tif,
		FeeRateBps:  feeRateBps,
	})
	if err != nil {
		return &TradeResult{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	fee := 0.0
	proceeds := price * size
	if feeRateBps > 0 {
		fee = proceeds * (float64(feeRateBps) / 10000.0)
	}

	if resp.Success {
		t.mu.Lock()
		if t.cachedBalance > 0 {
			t.cachedBalance += (proceeds - fee)
		}
		t.mu.Unlock()
	}

	status := "PENDING"
	if t.clob.IsTestMode() {
		status = "TEST"
	}

	return &TradeResult{
		OrderID:    resp.OrderID,
		Status:     status,
		Success:    resp.Success,
		Price:      price,
		Size:       size,
		Fee:        fee,
		FeeRateBps: feeRateBps,
		Side:       "SELL",
		TokenID:    tokenID,
		Outcome:    outcome,
		Timestamp:  time.Now(),
		Message:    resp.ErrorMsg,
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

	// Only poll every 5 seconds to avoid rate limits, but fast enough for trading
	// (Reduced from 30s for more accurate balance tracking during active trading)
	if time.Since(t.lastBalanceUpdate) < 5*time.Second && t.lastBalanceUpdate.IsZero() == false {
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

// GetBalanceSnapshot returns on-chain (Polygon) and off-chain (CLOB) balances.
// This helps distinguish on-chain USDC from CLOB allowance balance for sizing decisions.
func (t *RealTrader) GetBalanceSnapshot(ctx context.Context) (onChain float64, offChain float64, offChainOK bool, err error) {
	onChain, err = t.GetBalance(ctx)
	if err != nil {
		return 0, 0, false, err
	}

	allowance, offErr := t.clob.GetBalanceAllowance(ctx)
	if offErr != nil {
		return onChain, 0, false, nil
	}

	return onChain, allowance.Balance, true, nil
}

// ForceRefreshBalance clears the cache and fetches fresh balance
// Use this after trades to ensure accurate balance
func (t *RealTrader) ForceRefreshBalance(ctx context.Context) (float64, error) {
	t.mu.Lock()
	t.lastBalanceUpdate = time.Time{} // Clear cache
	t.mu.Unlock()
	return t.GetBalance(ctx)
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

func (t *RealTrader) IsTestMode() bool {
	return t.clob.IsTestMode()
}

func (t *RealTrader) GetMarketInfo(ctx context.Context, conditionID string) (*api.MarketInfo, error) {
	return t.clob.GetMarketInfo(ctx, conditionID)
}

// RedeemOnChain performs the on-chain redemption of winning tokens
func (t *RealTrader) RedeemOnChain(ctx context.Context, conditionID string) (string, error) {
	// First check if resolved on-chain (FREE READ)
	resolved, err := t.polygon.IsMarketResolved(ctx, conditionID)
	if err != nil {
		return "", fmt.Errorf("on-chain resolution check failed: %w", err)
	}

	if !resolved {
		return "", fmt.Errorf("market not yet resolved on-chain")
	}

	// Get signer from clob (we need to export it or add a helper)
	return t.polygon.RedeemPositions(ctx, t.clob.GetSigner(), conditionID)
}

// retryOnChainTx executes an on-chain transaction with retry logic and confirmation waiting.
// txName is used for error messages (e.g., "merge", "split").
// txFunc is the function that sends the transaction and returns (txHash, error).
// Returns txHash only after transaction is confirmed on-chain.
// Retries up to 3 times on failure with exponential backoff.
func (t *RealTrader) retryOnChainTx(ctx context.Context, txName string, txFunc func() (string, error)) (string, error) {
	var lastErr error
	var txHash string

	// Retry up to 3 times with exponential backoff
	for attempt := 1; attempt <= 3; attempt++ {
		// Check context before each attempt
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		txHash, lastErr = txFunc()
		if lastErr != nil {
			// Failed to send tx - retry after backoff
			if attempt < 3 {
				time.Sleep(time.Duration(attempt) * 2 * time.Second)
				continue
			}
			return "", fmt.Errorf("failed to send %s tx after %d attempts: %w", txName, attempt, lastErr)
		}

		// Wait for transaction confirmation
		success, err := t.polygon.WaitForTransaction(ctx, txHash)
		if err != nil {
			lastErr = fmt.Errorf("%s tx %s failed: %w", txName, txHash, err)
			// Tx sent but failed on-chain - don't retry same tx, try new one
			if attempt < 3 {
				time.Sleep(time.Duration(attempt) * 2 * time.Second)
				continue
			}
			return txHash, lastErr
		}

		if !success {
			lastErr = fmt.Errorf("%s tx %s reverted on-chain", txName, txHash)
			if attempt < 3 {
				time.Sleep(time.Duration(attempt) * 2 * time.Second)
				continue
			}
			return txHash, lastErr
		}

		// Success!
		return txHash, nil
	}

	return txHash, lastErr
}

// MergeOnChain burns equal YES+NO tokens to reclaim USDC immediately
// This works ANYTIME - no need to wait for market resolution.
// Use this immediately after buying both sides of an arb to capture profit instantly.
// Returns txHash only after transaction is confirmed on-chain.
// Retries up to 3 times on failure with exponential backoff.
func (t *RealTrader) MergeOnChain(ctx context.Context, conditionID string, shares float64) (string, error) {
	// CTF tokens use 6 decimals (same as USDC)
	// Convert shares to the proper amount with decimals
	amount := new(big.Int)
	// shares * 1e6 for 6 decimal places
	amountFloat := shares * 1e6
	amount.SetInt64(int64(amountFloat))

	return t.retryOnChainTx(ctx, "merge", func() (string, error) {
		return t.polygon.MergePositions(ctx, t.clob.GetSigner(), conditionID, amount)
	})
}

// SplitOnChain converts USDC into YES+NO token pairs
// This is the inverse of MergeOnChain - use to create inventory for panic selling.
// 1 USDC → 1 YES token + 1 NO token
// Use this to build inventory, then sell when bid_sum > $1.03 for profit.
// Returns txHash only after transaction is confirmed on-chain.
// Retries up to 3 times on failure with exponential backoff.
func (t *RealTrader) SplitOnChain(ctx context.Context, conditionID string, usdcAmount float64) (string, error) {
	// CTF tokens use 6 decimals (same as USDC)
	amount := new(big.Int)
	amountFloat := usdcAmount * 1e6
	amount.SetInt64(int64(amountFloat))

	return t.retryOnChainTx(ctx, "split", func() (string, error) {
		return t.polygon.SplitPositions(ctx, t.clob.GetSigner(), conditionID, amount)
	})
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
func (t *RealTrader) BuyWithConfirmation(ctx context.Context, tokenID, outcome string, price, size float64, orderType api.OrderType, tif api.TimeInForce, feeRateBps int, fillTimeout time.Duration) (*TradeResult, bool, error) {
	result, err := t.Buy(ctx, tokenID, outcome, price, size, orderType, tif, feeRateBps)
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
