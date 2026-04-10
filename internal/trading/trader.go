package trading

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
)

const (
	realTraderBalanceCacheTTL     = 30 * time.Second
	realTraderOnChainBalanceTTL   = 20 * time.Second
	realTraderOnChainRetryBackoff = 3 * time.Second
	realTraderCTFBalanceTTL       = 15 * time.Second
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

	// GetPositions returns an authoritative external position snapshot.
	GetPositions(ctx context.Context) ([]PositionInfo, error)

	// IsPaperMode returns true if this is paper trading
	IsPaperMode() bool

	// IsTestMode returns true if in test mode (validating but not submitting orders)
	IsTestMode() bool

	// GetMarketInfo retrieves market info including resolution status
	GetMarketInfo(ctx context.Context, conditionID string) (*api.MarketInfo, error)

	// GetTradingAllowance returns the currently authorized trading allowance (USDC)
	GetTradingAllowance(ctx context.Context) (float64, error)
}

// TradeResult represents the result of a trade attempt
type TradeResult struct {
	OrderID              string
	Status               string
	Success              bool
	Message              string
	Price                float64
	Size                 float64
	Fee                  float64
	FeeRateBps           int
	Side                 string
	TokenID              string
	Outcome              string
	MakingAmount         string
	TakingAmount         string
	TransactionsHashes   []string
	TradeIDs             []string
	AcknowledgedQty      float64
	AcknowledgedNotional float64
	Timestamp            time.Time
}

// PositionInfo represents a held position
type PositionInfo struct {
	TokenID         string
	Outcome         string
	Size            float64
	AvgPrice        float64
	ConditionID     string
	Slug            string
	OppositeOutcome string
	OppositeAsset   string
}

func parseMicroAmount(raw string) float64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	val, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0
	}
	if strings.ContainsAny(raw, ".eE") {
		return val
	}
	// Polymarket responses are inconsistent here: some endpoints return integer
	// micros, others return already-decimal token/notional amounts. Large whole
	// numbers are treated as micros; small integers are treated as literal units.
	if math.Abs(val) >= 1000 {
		return val / 1e6
	}
	return val
}

func responseWasImmediatelyMatched(resp *api.OrderResponse) bool {
	if resp == nil {
		return false
	}
	status := strings.ToUpper(strings.TrimSpace(resp.Status))
	return status == "MATCHED" || status == "FILLED" || len(resp.TransactionsHashes) > 0 || len(resp.TradeIDs) > 0
}

func deriveAcknowledgedExecution(resp *api.OrderResponse, side api.Side) (qty float64, notional float64) {
	if !responseWasImmediatelyMatched(resp) {
		return 0, 0
	}
	making := parseMicroAmount(resp.MakingAmount)
	taking := parseMicroAmount(resp.TakingAmount)
	if side == api.SideBuy {
		return taking, making
	}
	return making, taking
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
	// Calculate simulated fee using Polymarket dynamic curve
	fee := 0.0
	if feeRateBps > 0 {
		feeRate := 0.25
		exponent := 2.0
		// curve fee = shares * feeRate * (p * (1-p))^exponent
		feeTokens := size * feeRate * math.Pow(price*(1.0-price), exponent)
		// For buy orders, fee is collected in shares. We convert to USDC equivalent.
		fee = feeTokens * price
	}

	_, err := t.engine.Buy(outcome, price, size)
	if err != nil {
		return &TradeResult{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	// The engine handles fee deduction internally (reducing net shares on buy, reducing proceeds on sell)
	// We do not deduct USDC fees here to avoid double-charging in the simulation.

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
	// Calculate simulated fee using Polymarket dynamic curve
	fee := 0.0
	if feeRateBps > 0 {
		feeRate := 0.25
		exponent := 2.0
		// For sell orders, fees are collected directly in USDC.
		// curve fee = shares * feeRate * (p * (1-p))^exponent
		feeTokens := size * feeRate * math.Pow(price*(1.0-price), exponent)
		fee = feeTokens
	}

	_, err := t.engine.Sell(outcome, price, size)
	if err != nil {
		return &TradeResult{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	// The engine handles fee deduction internally (reducing net shares on buy, reducing proceeds on sell)
	// We do not deduct USDC fees here to avoid double-charging in the simulation.

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

func (t *PaperTrader) GetTradingAllowance(ctx context.Context) (float64, error) {
	return math.MaxFloat64, nil
}

// RealTrader implements Trader for real Polymarket trading
type RealTrader struct {
	client    api.ExchangeClient
	polygon   *api.PolygonClient
	config    *core.Config
	userWS    *api.UserWSClient
	mu        sync.Mutex
	onChainMu sync.Mutex // Mutex for on-chain transactions (Split, Merge, Redeem)

	dailyLoss  float64
	startOfDay time.Time

	cachedBalance     float64
	lastBalanceUpdate time.Time
	cachedOnChainUSDC float64
	lastOnChainUSDC   time.Time
	lastOnChainTry    time.Time
	hasOnChainUSDC    bool

	ctfBalanceCache      map[string]float64
	lastCTFBalanceUpdate map[string]time.Time
	lastCTFBalanceTry    map[string]time.Time
	ctfMu                sync.Mutex

	livePositions       map[string]float64
	confirmedOrderFills map[string]float64
	paperEngine         *paper.Engine
	paperTokenMeta      map[string]paperTokenMeta
	posMu               sync.Mutex
}

type paperTokenMeta struct {
	MarketID string
	Outcome  string
}

// NewRealTrader creates a new real trader
func NewRealTrader(cfg *core.Config) (*RealTrader, error) {
	if err := cfg.ValidateForRealTrading(); err != nil {
		return nil, err
	}

	var client api.ExchangeClient
	var err error

	if cfg.Exchange == "kalshi" {
		client, err = api.NewKalshiClient(cfg.KalshiAPIKey, cfg.KalshiPK)
		if err != nil {
			return nil, fmt.Errorf("failed to create Kalshi client: %w", err)
		}
	} else {
		client, err = api.NewCLOBClient(cfg.PK, cfg.APIKey, cfg.APISecret, cfg.APIPassphrase)
		if err != nil {
			return nil, fmt.Errorf("failed to create CLOB client: %w", err)
		}
	}

	polygon := api.NewPolygonClient(cfg.PolygonRPCURL)

	trader := &RealTrader{
		client:               client,
		polygon:              polygon,
		config:               cfg,
		startOfDay:           time.Now().Truncate(24 * time.Hour),
		ctfBalanceCache:      make(map[string]float64),
		lastCTFBalanceUpdate: make(map[string]time.Time),
		lastCTFBalanceTry:    make(map[string]time.Time),
		livePositions:        make(map[string]float64),
		confirmedOrderFills:  make(map[string]float64),
	}

	// Initialize User WebSocket for real-time fills
	// Kalshi user WS logic to be implemented, fallback to polling or ignore for now if kalshi
	if cfg.Exchange != "kalshi" {
		trader.userWS = api.NewUserWSClient(cfg.APIKey, cfg.APISecret, cfg.APIPassphrase)
		trader.userWS.SetOnFill(func(fill api.OrderFillData) {
			trader.applyLiveFill(fill)
		})
	}

	return trader, nil
}

func NewEmbeddedPaperRealTrader(cfg *core.Config, engine *paper.Engine) *RealTrader {
	if cfg == nil {
		cfg = &core.Config{}
	}
	return &RealTrader{
		config:               cfg,
		startOfDay:           time.Now().Truncate(24 * time.Hour),
		ctfBalanceCache:      make(map[string]float64),
		lastCTFBalanceUpdate: make(map[string]time.Time),
		lastCTFBalanceTry:    make(map[string]time.Time),
		livePositions:        make(map[string]float64),
		confirmedOrderFills:  make(map[string]float64),
		paperEngine:          engine,
		paperTokenMeta:       make(map[string]paperTokenMeta),
	}
}

func (t *RealTrader) IsEmbeddedPaperMode() bool {
	return t != nil && t.paperEngine != nil
}

func (t *RealTrader) RegisterPaperToken(tokenID, marketID, outcome string) {
	if t == nil || t.paperEngine == nil || tokenID == "" || marketID == "" || outcome == "" {
		return
	}
	seed := 0.0
	for _, pos := range t.paperEngine.GetPositions() {
		if pos.MarketID == marketID && pos.Outcome == outcome && pos.Quantity > 0 {
			seed += pos.Quantity
		}
	}
	t.posMu.Lock()
	if t.paperTokenMeta == nil {
		t.paperTokenMeta = make(map[string]paperTokenMeta)
	}
	t.paperTokenMeta[tokenID] = paperTokenMeta{MarketID: marketID, Outcome: outcome}
	t.livePositions[tokenID] = seed
	t.posMu.Unlock()
}

func (t *RealTrader) paperPositionsSnapshot() []PositionInfo {
	t.posMu.Lock()
	defer t.posMu.Unlock()

	positions := make([]PositionInfo, 0, len(t.livePositions))
	for tokenID, size := range t.livePositions {
		if size <= 0 {
			continue
		}
		info := PositionInfo{
			TokenID: tokenID,
			Size:    size,
		}
		if meta, ok := t.paperTokenMeta[tokenID]; ok {
			info.Outcome = meta.Outcome
		}
		positions = append(positions, info)
	}
	return positions
}

func (t *RealTrader) resolveEmbeddedPaperExecutionPrice(side api.Side, marketID, outcome string, limitPrice float64) (float64, error) {
	if t == nil || t.paperEngine == nil {
		return 0, fmt.Errorf("paper engine not initialized")
	}

	bid, ask := t.paperEngine.GetMarketBidAsk(marketID, outcome)
	switch side {
	case api.SideBuy:
		if ask > 0 {
			if ask > limitPrice+1e-9 {
				return 0, fmt.Errorf("paper buy not marketable: best ask %.4f above limit %.4f", ask, limitPrice)
			}
			return ask, nil
		}
	case api.SideSell:
		if bid > 0 {
			if bid+1e-9 < limitPrice {
				return 0, fmt.Errorf("paper sell not marketable: best bid %.4f below limit %.4f", bid, limitPrice)
			}
			return bid, nil
		}
	}

	return limitPrice, nil
}

func (t *RealTrader) simulatePaperOrder(side api.Side, tokenID, outcome string, price, size float64, feeRateBps int) (*TradeResult, error) {
	if t.paperEngine == nil {
		return nil, fmt.Errorf("paper engine not initialized")
	}
	if price <= 0 || price >= 1.0 || size <= 0 {
		return &TradeResult{
			Success: false,
			Message: "invalid paper order",
			Price:   price,
			Size:    size,
			Side:    string(side),
			TokenID: tokenID,
			Outcome: outcome,
		}, nil
	}
	safetyAmount := price * size
	if side == api.SideBuy && feeRateBps > 0 {
		safetyAmount += safetyAmount * (float64(feeRateBps) / 10000.0)
	}
	if err := t.checkSafetyLimits(safetyAmount); err != nil {
		return &TradeResult{
			Success: false,
			Message: err.Error(),
			Price:   price,
			Size:    size,
			Side:    string(side),
			TokenID: tokenID,
			Outcome: outcome,
		}, nil
	}

	meta := paperTokenMeta{}
	if existing, ok := t.paperTokenMeta[tokenID]; ok {
		meta = existing
	}
	if strings.TrimSpace(outcome) == "" {
		outcome = meta.Outcome
	}
	execPrice, err := t.resolveEmbeddedPaperExecutionPrice(side, meta.MarketID, outcome, price)
	if err != nil {
		return &TradeResult{
			Success: false,
			Message: err.Error(),
			Price:   price,
			Size:    size,
			Side:    string(side),
			TokenID: tokenID,
			Outcome: outcome,
		}, nil
	}
	orderID := fmt.Sprintf("paper-%d", time.Now().UnixNano())
	t.posMu.Lock()
	if side == api.SideBuy {
		t.paperEngine.SetFeeRateBps(feeRateBps)
		trade, err := t.paperEngine.BuyForMarket(meta.MarketID, outcome, execPrice, size)
		if err != nil {
			t.posMu.Unlock()
			return &TradeResult{
				Success: false,
				Message: err.Error(),
				Price:   price,
				Size:    size,
				Side:    string(side),
				TokenID: tokenID,
				Outcome: outcome,
			}, nil
		}
		t.livePositions[tokenID] += trade.Quantity
		t.confirmedOrderFills[orderID] = trade.Quantity
		t.posMu.Unlock()
		fee := math.Max(0, (size-trade.Quantity)*execPrice)
		return &TradeResult{
			OrderID:              orderID,
			Status:               "FILLED",
			Success:              true,
			Price:                execPrice,
			Size:                 size,
			Fee:                  fee,
			FeeRateBps:           feeRateBps,
			Side:                 string(side),
			TokenID:              tokenID,
			Outcome:              outcome,
			AcknowledgedQty:      trade.Quantity,
			AcknowledgedNotional: trade.Value,
			Timestamp:            time.Now(),
		}, nil
	} else {
		if t.livePositions[tokenID]+1e-9 < size {
			available := t.livePositions[tokenID]
			t.posMu.Unlock()
			return &TradeResult{
				Success: false,
				Message: fmt.Sprintf("insufficient position for paper sell: have %.4f need %.4f", available, size),
				Price:   price,
				Size:    size,
				Side:    string(side),
				TokenID: tokenID,
				Outcome: outcome,
			}, nil
		}
		t.paperEngine.SetFeeRateBps(feeRateBps)
		trade, err := t.paperEngine.SellForMarket(meta.MarketID, outcome, execPrice, size)
		if err != nil {
			t.posMu.Unlock()
			return &TradeResult{
				Success: false,
				Message: err.Error(),
				Price:   price,
				Size:    size,
				Side:    string(side),
				TokenID: tokenID,
				Outcome: outcome,
			}, nil
		}
		t.livePositions[tokenID] -= trade.Quantity
		if t.livePositions[tokenID] < 0 {
			t.livePositions[tokenID] = 0
		}
		t.confirmedOrderFills[orderID] = trade.Quantity
		t.posMu.Unlock()
		fee := math.Max(0, (execPrice*trade.Quantity)-trade.Value)
		return &TradeResult{
			OrderID:              orderID,
			Status:               "FILLED",
			Success:              true,
			Price:                execPrice,
			Size:                 size,
			Fee:                  fee,
			FeeRateBps:           feeRateBps,
			Side:                 string(side),
			TokenID:              tokenID,
			Outcome:              outcome,
			AcknowledgedQty:      trade.Quantity,
			AcknowledgedNotional: trade.Value,
			Timestamp:            time.Now(),
		}, nil
	}
}

func (t *RealTrader) applyLiveFill(fill api.OrderFillData) {
	size, err := strconv.ParseFloat(fill.Size, 64)
	if err != nil || size <= 0 {
		return
	}

	t.posMu.Lock()
	defer t.posMu.Unlock()

	if fill.Side == "BUY" {
		t.livePositions[fill.AssetID] += size
	} else if fill.Side == "SELL" {
		t.livePositions[fill.AssetID] -= size
		if t.livePositions[fill.AssetID] < 0 {
			t.livePositions[fill.AssetID] = 0
		}
	}
	if fill.OrderID != "" {
		t.confirmedOrderFills[fill.OrderID] += size
	}
}

// Exchange returns the underlying ExchangeClient for direct API access.
func (t *RealTrader) Exchange() api.ExchangeClient {
	if t.paperEngine != nil {
		return nil
	}
	return t.client
}

// GetConfirmedFillSize returns the cumulative WS-confirmed fill quantity for an order.
func (t *RealTrader) GetConfirmedFillSize(orderID string) float64 {
	if orderID == "" {
		return 0
	}
	t.posMu.Lock()
	defer t.posMu.Unlock()
	return t.confirmedOrderFills[orderID]
}

// ResetConfirmedFill forgets any cached WS-confirmed fill quantity for an order.
func (t *RealTrader) ResetConfirmedFill(orderID string) {
	if orderID == "" {
		return
	}
	t.posMu.Lock()
	defer t.posMu.Unlock()
	delete(t.confirmedOrderFills, orderID)
}

// GetLivePositionSize returns the latest websocket-backed position size for a token.
func (t *RealTrader) GetLivePositionSize(tokenID string) float64 {
	t.posMu.Lock()
	defer t.posMu.Unlock()
	return t.livePositions[tokenID]
}

// GetLivePositionsSnapshot returns the current websocket-backed position cache.
// This is a fast local hint only, not authoritative external truth.
func (t *RealTrader) GetLivePositionsSnapshot() []PositionInfo {
	t.posMu.Lock()
	defer t.posMu.Unlock()

	result := make([]PositionInfo, 0, len(t.livePositions))
	for tokenID, size := range t.livePositions {
		if size <= 0 {
			continue
		}
		result = append(result, PositionInfo{TokenID: tokenID, Size: size})
	}
	return result
}

// WaitForLivePairPositions watches the websocket-backed position cache for a
// complementary pair and returns as soon as both sides have at least minShares.
//
// This is intentionally WS-only so callers can react to confirmed fills without
// blocking on slower on-chain settlement checks.
func (t *RealTrader) WaitForLivePairPositions(ctx context.Context, token0, token1 string, minShares float64, timeout time.Duration) (bal0, bal1 float64, ready bool, err error) {
	if minShares <= 0 {
		minShares = 0.01
	}
	if timeout <= 0 {
		bal0 = t.GetLivePositionSize(token0)
		bal1 = t.GetLivePositionSize(token1)
		return bal0, bal1, bal0 >= minShares && bal1 >= minShares, nil
	}

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		bal0 = t.GetLivePositionSize(token0)
		bal1 = t.GetLivePositionSize(token1)
		if bal0 >= minShares && bal1 >= minShares {
			return bal0, bal1, true, nil
		}
		if time.Now().After(deadline) {
			return bal0, bal1, false, nil
		}

		select {
		case <-ctx.Done():
			return bal0, bal1, false, ctx.Err()
		case <-ticker.C:
		}
	}
}

// StartUserWS connects the user websocket and primes the position cache
func (t *RealTrader) StartUserWS(ctx context.Context) error {
	if t.paperEngine != nil {
		return nil
	}
	// Prime the cache with a REST call
	initialPos, err := t.client.GetPositions(ctx)
	conditionSet := make(map[string]struct{})
	if err == nil {
		t.posMu.Lock()
		for _, p := range initialPos {
			t.livePositions[p.TokenID] = p.Size
			if p.ConditionID != "" {
				conditionSet[p.ConditionID] = struct{}{}
			}
		}
		t.posMu.Unlock()
	}

	conditionIDs := make([]string, 0, len(conditionSet))
	for conditionID := range conditionSet {
		conditionIDs = append(conditionIDs, conditionID)
	}

	if t.userWS == nil {
		return nil
	}
	return t.userWS.SubscribeMarkets(ctx, conditionIDs)
}

// SubscribeUserWSMarkets ensures the user websocket is subscribed to the
// provided condition IDs so live trade updates are received for active markets.
func (t *RealTrader) SubscribeUserWSMarkets(ctx context.Context, conditionIDs ...string) error {
	if t.paperEngine != nil {
		return nil
	}
	if t.userWS == nil {
		return nil
	}
	return t.userWS.SubscribeMarkets(ctx, conditionIDs)
}

// SetTestMode enables/disables test mode
func (t *RealTrader) SetTestMode(enabled bool) {
	if t.client == nil {
		return
	}
	t.client.SetTestMode(enabled)
}

// GetSigner returns the internal signer
func (t *RealTrader) GetSigner() *api.Signer {
	if t.client == nil {
		return nil
	}
	return t.client.GetSigner()
}

// ExecuteBatch places multiple orders in a single HTTP request (e.g. for panic buys or split sells)
func (t *RealTrader) ExecuteBatch(ctx context.Context, reqs []*api.OrderRequest) ([]*TradeResult, error) {
	if t.paperEngine != nil {
		totalCost := 0.0
		for _, req := range reqs {
			if req == nil || req.Side != api.SideBuy {
				continue
			}
			cost := req.Price * req.Size
			if req.FeeRateBps > 0 {
				cost += cost * (float64(req.FeeRateBps) / 10000.0)
			}
			totalCost += cost
		}
		if totalCost > 0 {
			if err := t.checkSafetyLimits(totalCost); err != nil {
				return nil, err
			}
		}
		results := make([]*TradeResult, len(reqs))
		for i, req := range reqs {
			if req == nil {
				results[i] = &TradeResult{Success: false, Message: "missing paper batch request"}
				continue
			}
			result, err := t.simulatePaperOrder(req.Side, req.TokenID, req.Outcome, req.Price, req.Size, req.FeeRateBps)
			if err != nil {
				return nil, err
			}
			results[i] = result
		}
		return results, nil
	}

	totalCost := 0.0
	for _, req := range reqs {
		if req.Side == api.SideBuy {
			cost := req.Price * req.Size
			fee := 0.0
			if req.FeeRateBps > 0 {
				fee = cost * (float64(req.FeeRateBps) / 10000.0)
			}
			totalCost += (cost + fee)
		}
	}

	if totalCost > 0 {
		if err := t.checkSafetyLimits(totalCost); err != nil {
			return nil, err
		}
	}

	resps := make([]*api.OrderResponse, len(reqs))
	errs := make([]error, len(reqs))
	var wg sync.WaitGroup
	for i, req := range reqs {
		i, req := i, req
		wg.Add(1)
		go func() {
			defer wg.Done()
			resps[i], errs[i] = t.client.PlaceOrder(ctx, req)
		}()
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}

	results := make([]*TradeResult, len(resps))
	for i, resp := range resps {
		success := resp.Success
		message := strings.TrimSpace(resp.ErrorMsg)
		status := strings.TrimSpace(resp.Status)
		if success && strings.TrimSpace(resp.OrderID) == "" && status == "" {
			success = false
			if message == "" {
				message = "empty batch response from exchange"
			}
		}
		fee := 0.0
		if reqs[i].FeeRateBps > 0 {
			fee = reqs[i].Price * reqs[i].Size * (float64(reqs[i].FeeRateBps) / 10000.0)
		}

		results[i] = &TradeResult{
			Success:            success,
			OrderID:            resp.OrderID,
			Status:             status,
			Message:            message,
			Price:              reqs[i].Price,
			Size:               reqs[i].Size,
			Side:               string(reqs[i].Side),
			TokenID:            reqs[i].TokenID,
			Fee:                fee,
			MakingAmount:       resp.MakingAmount,
			TakingAmount:       resp.TakingAmount,
			TransactionsHashes: append([]string(nil), resp.TransactionsHashes...),
			TradeIDs:           append([]string(nil), resp.TradeIDs...),
			Timestamp:          time.Now(),
		}
		results[i].AcknowledgedQty, results[i].AcknowledgedNotional = deriveAcknowledgedExecution(resp, reqs[i].Side)

		// If any buy order was successful, optimistically mark balance as stale
		if success && reqs[i].Side == api.SideBuy {
			t.mu.Lock()
			t.lastBalanceUpdate = time.Time{}
			t.mu.Unlock()
		}
	}

	return results, nil
}

func (t *RealTrader) Buy(ctx context.Context, tokenID, outcome string, price, size float64, orderType api.OrderType, tif api.TimeInForce, feeRateBps int) (*TradeResult, error) {
	if t.paperEngine != nil {
		return t.simulatePaperOrder(api.SideBuy, tokenID, outcome, price, size, feeRateBps)
	}
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

	resp, err := t.client.PlaceOrder(ctx, &api.OrderRequest{
		TokenID:     tokenID,
		Outcome:     outcome,
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
		// Optimistically update cached balance but mark it stale so the
		// background ticker refreshes it with the actual on-chain value.
		// This prevents over-spending on the next order if partial fill occurred.
		if t.cachedBalance > 0 {
			t.cachedBalance -= (cost + fee)
			if t.cachedBalance < 0 {
				t.cachedBalance = 0
			}
		}
		t.lastBalanceUpdate = time.Time{} // Mark stale for next GetBalance
		t.mu.Unlock()
	}

	status := strings.TrimSpace(resp.Status)
	if status == "" {
		status = "PENDING"
	}
	if t.client.IsTestMode() {
		status = "TEST"
	}
	ackQty, ackNotional := deriveAcknowledgedExecution(resp, api.SideBuy)

	return &TradeResult{
		OrderID:              resp.OrderID,
		Status:               status,
		Success:              resp.Success,
		Price:                price,
		Size:                 size,
		Fee:                  fee,
		FeeRateBps:           feeRateBps,
		Side:                 "BUY",
		TokenID:              tokenID,
		Outcome:              outcome,
		MakingAmount:         resp.MakingAmount,
		TakingAmount:         resp.TakingAmount,
		TransactionsHashes:   append([]string(nil), resp.TransactionsHashes...),
		TradeIDs:             append([]string(nil), resp.TradeIDs...),
		AcknowledgedQty:      ackQty,
		AcknowledgedNotional: ackNotional,
		Timestamp:            time.Now(),
		Message:              resp.ErrorMsg,
	}, nil
}

func (t *RealTrader) Sell(ctx context.Context, tokenID, outcome string, price, size float64, orderType api.OrderType, tif api.TimeInForce, feeRateBps int) (*TradeResult, error) {
	if t.paperEngine != nil {
		return t.simulatePaperOrder(api.SideSell, tokenID, outcome, price, size, feeRateBps)
	}
	// Check safety limits (same as Buy)
	proceeds := price * size
	if err := t.checkSafetyLimits(proceeds); err != nil {
		return &TradeResult{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	resp, err := t.client.PlaceOrder(ctx, &api.OrderRequest{
		TokenID:     tokenID,
		Outcome:     outcome,
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
	if feeRateBps > 0 {
		fee = proceeds * (float64(feeRateBps) / 10000.0)
	}

	if resp.Success {
		t.mu.Lock()
		// Optimistically update cached balance but mark stale for refresh
		if t.cachedBalance > 0 {
			t.cachedBalance += (proceeds - fee)
		}
		t.lastBalanceUpdate = time.Time{} // Mark stale for next GetBalance
		t.mu.Unlock()
	}

	status := strings.TrimSpace(resp.Status)
	if status == "" {
		status = "PENDING"
	}
	if t.client.IsTestMode() {
		status = "TEST"
	}
	ackQty, ackNotional := deriveAcknowledgedExecution(resp, api.SideSell)

	return &TradeResult{
		OrderID:              resp.OrderID,
		Status:               status,
		Success:              resp.Success,
		Price:                price,
		Size:                 size,
		Fee:                  fee,
		FeeRateBps:           feeRateBps,
		Side:                 "SELL",
		TokenID:              tokenID,
		Outcome:              outcome,
		MakingAmount:         resp.MakingAmount,
		TakingAmount:         resp.TakingAmount,
		TransactionsHashes:   append([]string(nil), resp.TransactionsHashes...),
		TradeIDs:             append([]string(nil), resp.TradeIDs...),
		AcknowledgedQty:      ackQty,
		AcknowledgedNotional: ackNotional,
		Timestamp:            time.Now(),
		Message:              resp.ErrorMsg,
	}, nil
}

func (t *RealTrader) CancelOrder(ctx context.Context, orderID string) error {
	if t.paperEngine != nil {
		return nil
	}
	return t.client.CancelOrder(ctx, orderID)
}

func (t *RealTrader) CancelAll(ctx context.Context) error {
	if t.paperEngine != nil {
		return nil
	}
	return t.client.CancelAllOrders(ctx)
}

func (t *RealTrader) GetBalance(ctx context.Context) (float64, error) {
	if t.paperEngine != nil {
		return t.paperEngine.GetBalance(), nil
	}
	t.mu.Lock()
	// If background sync is keeping this fresh, we can rely on it.
	// We use a 30s TTL here so if the background ticker is delayed, we still use the cache instead of blocking the WS loop.
	if time.Since(t.lastBalanceUpdate) < realTraderBalanceCacheTTL && !t.lastBalanceUpdate.IsZero() {
		bal := t.cachedBalance
		t.mu.Unlock()
		return bal, nil
	}
	cachedBal := t.cachedBalance
	hasCache := !t.lastBalanceUpdate.IsZero()

	// Prevent cache stampede by temporarily marking as updated
	t.lastBalanceUpdate = time.Now()
	t.mu.Unlock()

	bal, err := t.fetchLiveBalance(ctx)
	if err != nil {
		// Clear temporary marker so callers can retry without waiting for cache TTL.
		t.mu.Lock()
		t.lastBalanceUpdate = time.Time{}
		t.mu.Unlock()

		// Return cached balance on error if available
		if hasCache {
			return cachedBal, nil
		}
		return 0, err
	}

	t.mu.Lock()
	t.cachedBalance = bal
	t.lastBalanceUpdate = time.Now()
	t.mu.Unlock()

	return bal, nil
}

func (t *RealTrader) fetchLiveBalance(ctx context.Context) (float64, error) {
	if t.paperEngine != nil {
		return t.paperEngine.GetBalance(), nil
	}
	if t.client == nil {
		return 0, fmt.Errorf("exchange client not initialized")
	}

	if t.config != nil && strings.EqualFold(t.config.Exchange, "kalshi") {
		ba, err := t.client.GetBalanceAllowance(ctx)
		if err != nil {
			return 0, err
		}
		return ba.Balance, nil
	}

	if t.polygon != nil {
		onChainBalance, onChainErr := t.getOnChainUSDCBalance(ctx)
		ba, baErr := t.client.GetBalanceAllowance(ctx)
		if onChainErr == nil && baErr == nil {
			// Use the more conservative value to avoid overstating spendable cash
			// when exchange balance and on-chain wallet balance briefly diverge.
			if ba.Balance > 0 && (onChainBalance <= 0 || ba.Balance < onChainBalance) {
				return ba.Balance, nil
			}
			return onChainBalance, nil
		}
		if onChainErr == nil {
			return onChainBalance, nil
		}
		if baErr == nil {
			return ba.Balance, nil
		}
	}

	ba, err := t.client.GetBalanceAllowance(ctx)
	if err != nil {
		return 0, err
	}
	return ba.Balance, nil
}

func (t *RealTrader) getOnChainUSDCBalance(ctx context.Context) (float64, error) {
	if t.paperEngine != nil {
		return t.paperEngine.GetBalance(), nil
	}
	if t.polygon == nil {
		return 0, fmt.Errorf("polygon client not initialized")
	}
	if t.client == nil {
		return 0, fmt.Errorf("exchange client not initialized")
	}

	now := time.Now()
	t.mu.Lock()
	cached := t.cachedOnChainUSDC
	hasCached := t.hasOnChainUSDC
	lastUpdate := t.lastOnChainUSDC
	lastTry := t.lastOnChainTry
	if hasCached && !lastUpdate.IsZero() && now.Sub(lastUpdate) < realTraderOnChainBalanceTTL {
		t.mu.Unlock()
		return cached, nil
	}
	if !lastTry.IsZero() && now.Sub(lastTry) < realTraderOnChainRetryBackoff {
		// We recently attempted chain refresh; avoid tight retry loops.
		t.mu.Unlock()
		if hasCached {
			return cached, nil
		}
		return 0, fmt.Errorf("on-chain balance refresh throttled")
	}
	t.lastOnChainTry = now
	t.mu.Unlock()

	bal, err := t.polygon.GetUSDCBalance(ctx, t.client.Address())
	if err != nil {
		if hasCached {
			return cached, nil
		}
		return 0, err
	}

	t.mu.Lock()
	t.cachedOnChainUSDC = bal
	t.lastOnChainUSDC = time.Now()
	t.hasOnChainUSDC = true
	t.mu.Unlock()
	return bal, nil
}

func (t *RealTrader) GetOnChainUSDCBalance(ctx context.Context) (float64, error) {
	return t.getOnChainUSDCBalance(ctx)
}

func (t *RealTrader) ForceRefreshOnChainUSDCBalance(ctx context.Context) (float64, error) {
	t.invalidateOnChainBalanceCache()
	return t.getOnChainUSDCBalance(ctx)
}

func (t *RealTrader) invalidateOnChainBalanceCache() {
	t.mu.Lock()
	t.cachedOnChainUSDC = 0
	t.lastOnChainUSDC = time.Time{}
	t.lastOnChainTry = time.Time{}
	t.hasOnChainUSDC = false
	t.mu.Unlock()
}

// ForceRefreshBalance clears the cache and fetches fresh balance
// Use this after trades to ensure accurate balance
func (t *RealTrader) ForceRefreshBalance(ctx context.Context) (float64, error) {
	t.mu.Lock()
	t.lastBalanceUpdate = time.Time{} // Clear cache
	t.mu.Unlock()
	return t.GetBalance(ctx)
}

// InvalidateCTFBalanceCache clears cached on-chain CTF balances so the next
// read is forced to hit chain state.
func (t *RealTrader) InvalidateCTFBalanceCache(tokenIDs ...string) {
	t.ctfMu.Lock()
	defer t.ctfMu.Unlock()

	if len(tokenIDs) == 0 {
		t.ctfBalanceCache = make(map[string]float64)
		t.lastCTFBalanceUpdate = make(map[string]time.Time)
		t.lastCTFBalanceTry = make(map[string]time.Time)
		return
	}

	for _, tokenID := range tokenIDs {
		delete(t.ctfBalanceCache, tokenID)
		delete(t.lastCTFBalanceUpdate, tokenID)
		delete(t.lastCTFBalanceTry, tokenID)
	}
}

// UpdateBalanceAllowance syncs the CLOB's cached allowance with on-chain state.
// Call this before trading to ensure the CLOB knows about unlimited on-chain allowance.
func (t *RealTrader) UpdateBalanceAllowance(ctx context.Context) error {
	if t.paperEngine != nil {
		return nil
	}
	return t.client.UpdateBalanceAllowance(ctx)
}

// ForceRefreshPositions fetches an authoritative external position snapshot and
// refreshes the local WS-backed cache to match it.
func (t *RealTrader) ForceRefreshPositions(ctx context.Context) ([]PositionInfo, error) {
	if t.paperEngine != nil {
		return t.paperPositionsSnapshot(), nil
	}
	if t.client == nil {
		return nil, fmt.Errorf("clob client not initialized")
	}
	positions, err := t.client.GetPositions(ctx)
	if err != nil {
		return nil, err
	}

	t.posMu.Lock()
	// Clear and rebuild cache
	t.livePositions = make(map[string]float64)
	result := make([]PositionInfo, len(positions))
	for i, pos := range positions {
		t.livePositions[pos.TokenID] = pos.Size
		result[i] = PositionInfo{
			TokenID:         pos.TokenID,
			Size:            pos.Size,
			AvgPrice:        pos.AvgPrice,
			Outcome:         pos.Outcome,
			ConditionID:     pos.ConditionID,
			Slug:            pos.Slug,
			OppositeOutcome: pos.OppositeOutcome,
			OppositeAsset:   pos.OppositeAsset,
		}
	}
	t.posMu.Unlock()

	return result, nil
}

func (t *RealTrader) GetPositions(ctx context.Context) ([]PositionInfo, error) {
	if t.paperEngine != nil {
		return t.paperPositionsSnapshot(), nil
	}
	return t.ForceRefreshPositions(ctx)
}

func (t *RealTrader) IsPaperMode() bool {
	return t.paperEngine != nil
}

func (t *RealTrader) IsTestMode() bool {
	if t.paperEngine != nil || t.client == nil {
		return false
	}
	return t.client.IsTestMode()
}

func (t *RealTrader) GetMarketInfo(ctx context.Context, conditionID string) (*api.MarketInfo, error) {
	if t.paperEngine != nil || t.client == nil {
		return nil, fmt.Errorf("market info unavailable in embedded paper mode")
	}
	return t.client.GetMarketInfo(ctx, conditionID)
}

func (t *RealTrader) GetTradingAllowance(ctx context.Context) (float64, error) {
	if t.paperEngine != nil {
		return math.MaxFloat64, nil
	}
	res, err := t.client.GetBalanceAllowance(ctx)
	if err != nil {
		return 0, err
	}
	return res.Allowance, nil
}

func (t *RealTrader) refreshStateAfterRedeem(ctx context.Context) {
	t.InvalidateCTFBalanceCache()
	t.invalidateOnChainBalanceCache()

	const maxAttempts = 3
	const refreshDelay = 250 * time.Millisecond

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if _, err := t.ForceRefreshBalance(ctx); err == nil {
			return
		}
		if attempt == maxAttempts {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(refreshDelay):
		}
	}
}

func (t *RealTrader) GetOnChainTxState(ctx context.Context, txHash string) (string, error) {
	if t.paperEngine != nil {
		return "success", nil
	}
	txHash = strings.TrimSpace(txHash)
	if txHash == "" {
		return "", fmt.Errorf("tx hash is empty")
	}

	receipt, err := t.polygon.GetTransactionReceipt(ctx, txHash)
	if err != nil {
		return "", err
	}
	if receipt != nil {
		if receipt.Status == "0x1" {
			return "success", nil
		}
		return "reverted", nil
	}

	tx, err := t.polygon.GetTransactionByHash(ctx, txHash)
	if err != nil {
		return "", err
	}
	if tx == nil {
		return "dropped", nil
	}
	if tx.BlockNumber == "" || tx.BlockNumber == "0x" || tx.BlockNumber == "0x0" {
		return "pending", nil
	}

	return "pending", nil
}

// RedeemOnChain performs the on-chain redemption of winning tokens
func (t *RealTrader) RedeemOnChain(ctx context.Context, conditionID string, numOutcomes int) (string, error) {
	if t.config.Exchange == "kalshi" {
		return "", fmt.Errorf("redeem not supported/needed on kalshi")
	}

	// First check if resolved on-chain (FREE READ)
	resolved, err := t.polygon.IsMarketResolved(ctx, conditionID)
	if err != nil {
		return "", fmt.Errorf("on-chain resolution check failed: %w", err)
	}

	if !resolved {
		return "", fmt.Errorf("market not yet resolved on-chain (payouts not reported)")
	}

	txHash, err := t.retryOnChainTx(ctx, "redeem", func() (string, error) {
		return t.polygon.RedeemPositions(ctx, t.client.GetSigner(), conditionID, numOutcomes)
	})
	if err != nil {
		return txHash, err
	}

	t.refreshStateAfterRedeem(ctx)
	return txHash, nil
}

// RedeemOnChainForce submits the redeem transaction without waiting for the
// on-chain resolved flag to propagate. Use only when an authoritative winner is
// already known and you want to mirror mergeredeem's force path.
func (t *RealTrader) RedeemOnChainForce(ctx context.Context, conditionID string, numOutcomes int) (string, error) {
	if t.config.Exchange == "kalshi" {
		return "", fmt.Errorf("redeem not supported/needed on kalshi")
	}
	txHash, err := t.retryOnChainTx(ctx, "redeem", func() (string, error) {
		return t.polygon.RedeemPositions(ctx, t.client.GetSigner(), conditionID, numOutcomes)
	})
	if err != nil {
		return txHash, err
	}

	t.refreshStateAfterRedeem(ctx)
	return txHash, nil
}

// SubmitRedeemOnChainForce sends the redeem transaction immediately using a
// normal gas profile and returns once the tx is accepted by the RPC.
// Confirmation is left to the caller.
func (t *RealTrader) SubmitRedeemOnChainForce(ctx context.Context, conditionID string, numOutcomes int) (string, error) {
	if t.config.Exchange == "kalshi" {
		return "", fmt.Errorf("redeem not supported/needed on kalshi")
	}
	return t.submitOnChainTx(ctx, "redeem", func() (string, error) {
		return t.polygon.RedeemPositions(ctx, t.client.GetSigner(), conditionID, numOutcomes)
	})
}

// retryOnChainTx executes an on-chain transaction with retry logic and confirmation waiting.
// txName is used for error messages (e.g., "merge", "split").
// txFunc is the function that sends the transaction and returns (txHash, error).
// Returns txHash only after transaction is confirmed on-chain.
// Retries up to 3 times on failure with exponential backoff.
func (t *RealTrader) retryOnChainTx(ctx context.Context, txName string, txFunc func() (string, error)) (string, error) {
	txHash, lastErr := t.submitOnChainTx(ctx, txName, txFunc)
	if lastErr != nil {
		return txHash, lastErr
	}

	// Retry up to 3 times with exponential backoff
	for attempt := 1; attempt <= 3; attempt++ {
		// Wait for transaction confirmation
		success, err := t.polygon.WaitForTransaction(ctx, txHash)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled) {
				return txHash, fmt.Errorf("%s tx %s confirmation pending: %w", txName, txHash, err)
			}
			lastErr = fmt.Errorf("%s tx %s failed: %w", txName, txHash, err)
			// Tx sent but failed on-chain - don't retry same tx, try new one
			if attempt < 3 {
				time.Sleep(time.Duration(attempt) * 2 * time.Second)
				txHash, lastErr = t.submitOnChainTx(ctx, txName, txFunc)
				if lastErr == nil {
					continue
				}
			}
			return txHash, lastErr
		}

		if !success {
			lastErr = fmt.Errorf("%s tx %s reverted on-chain", txName, txHash)
			if attempt < 3 {
				time.Sleep(time.Duration(attempt) * 2 * time.Second)
				txHash, lastErr = t.submitOnChainTx(ctx, txName, txFunc)
				if lastErr == nil {
					continue
				}
			}
			return txHash, lastErr
		}

		// Success!
		return txHash, nil
	}

	return txHash, lastErr
}

func (t *RealTrader) submitOnChainTx(ctx context.Context, txName string, txFunc func() (string, error)) (string, error) {
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

		// Lock globally only during transaction submission to prevent nonce collisions
		t.onChainMu.Lock()
		txHash, lastErr = txFunc()
		t.onChainMu.Unlock()

		if lastErr != nil {
			// Failed to send tx - retry after backoff
			if attempt < 3 {
				time.Sleep(time.Duration(attempt) * 2 * time.Second)
				continue
			}
			return "", fmt.Errorf("failed to send %s tx after %d attempts: %w", txName, attempt, lastErr)
		}
		return txHash, nil
	}

	return txHash, lastErr
}

// MergeOnChain burns equal YES+NO tokens to reclaim USDC immediately
// This works ANYTIME - no need to wait for market resolution.
// Use this immediately after buying both sides of an arb to capture profit instantly.
// Returns txHash only after transaction is confirmed on-chain.
// Retries up to 3 times on failure with exponential backoff.
func (t *RealTrader) MergeOnChain(ctx context.Context, conditionID string, shares float64, numOutcomes int) (string, error) {
	if t.paperEngine != nil {
		return fmt.Sprintf("paper-merge-%d", time.Now().UnixNano()), nil
	}
	if t.config.Exchange == "kalshi" {
		return "", fmt.Errorf("merge not supported on kalshi")
	}

	// CTF tokens use 6 decimals (same as USDC)
	// Convert shares to the proper amount with decimals
	amount := new(big.Int)
	// shares * 1e6 for 6 decimal places
	amountFloat := shares * 1e6
	amount.SetInt64(int64(math.Round(amountFloat)))

	txHash, err := t.retryOnChainTx(ctx, "merge", func() (string, error) {
		return t.polygon.MergePositions(ctx, t.client.GetSigner(), conditionID, amount, numOutcomes)
	})
	if err != nil {
		return txHash, err
	}
	t.invalidateOnChainBalanceCache()
	return txHash, nil
}

// SplitOnChain converts USDC into YES+NO token pairs
// This is the inverse of MergeOnChain - use to create inventory for panic selling.
// 1 USDC → 1 YES token + 1 NO token
// Use this to build inventory, then sell when bid_sum > $1.03 for profit.
// Returns txHash only after transaction is confirmed on-chain.
// Retries up to 3 times on failure with exponential backoff.
func (t *RealTrader) SplitOnChain(ctx context.Context, conditionID string, usdcAmount float64, numOutcomes int) (string, error) {
	if t.paperEngine != nil {
		return fmt.Sprintf("paper-split-%d", time.Now().UnixNano()), nil
	}
	if t.config.Exchange == "kalshi" {
		return "", fmt.Errorf("split not supported on kalshi")
	}

	// CTF tokens use 6 decimals (same as USDC)
	amount := new(big.Int)
	amountFloat := usdcAmount * 1e6
	amount.SetInt64(int64(math.Round(amountFloat)))

	txHash, err := t.retryOnChainTx(ctx, "split", func() (string, error) {
		return t.polygon.SplitPositions(ctx, t.client.GetSigner(), conditionID, amount, numOutcomes)
	})
	if err != nil {
		return txHash, err
	}
	t.invalidateOnChainBalanceCache()
	return txHash, nil
}

// retryRPC retries a function that returns (T, error) upon rate limit errors
func retryRPC[T any](ctx context.Context, op func() (T, error)) (T, error) {
	var zero T
	for i := 0; i < 5; i++ {
		res, err := op()
		if err == nil {
			return res, nil
		}
		// Check for rate limit errors
		errStr := err.Error()
		if strings.Contains(errStr, "429") || strings.Contains(errStr, "limit") || strings.Contains(errStr, "exhausted") {
			select {
			case <-ctx.Done():
				return zero, ctx.Err()
			case <-time.After(time.Duration(2*(i+1)) * time.Second): // Exponential backoff: 2s, 4s, 6s...
				continue
			}
		}
		return zero, err
	}
	return zero, fmt.Errorf("max retries exceeded")
}

// ApproveTrading checks and approves all necessary contracts for trading
// Returns true if any approval transaction was sent
func (t *RealTrader) ApproveTrading(ctx context.Context) (bool, error) {
	if t.config.Exchange == "kalshi" {
		return false, nil // Kalshi operates in cash, no approvals needed
	}

	t.onChainMu.Lock()
	defer t.onChainMu.Unlock()

	signer := t.client.GetSigner()
	if signer == nil {
		return false, fmt.Errorf("signer is nil, cannot approve trading")
	}
	address := signer.Address()
	sentTx := false

	// Helper to check allowance with retry
	checkAllowance := func(spender string) (*big.Int, error) {
		return retryRPC(ctx, func() (*big.Int, error) {
			return t.polygon.GetUSDCAllowance(ctx, address, spender)
		})
	}

	// Helper to check CTF approval with retry
	checkApproval := func(operator string) (bool, error) {
		return retryRPC(ctx, func() (bool, error) {
			return t.polygon.IsCTFApproved(ctx, address, operator)
		})
	}

	// Require at least $10,000 USDC allowance to avoid frequent re-approvals
	minAllowance := new(big.Int).SetUint64(10000 * 1000000)

	// 1. Approve USDC for Legacy Exchange (Binary Markets)
	allowanceLegacy, err := checkAllowance(api.CTFExchange)
	if err != nil {
		return false, fmt.Errorf("failed to check legacy allowance: %w", err)
	}
	if allowanceLegacy.Cmp(minAllowance) < 0 {
		fmt.Println("🔓 Approving USDC for Legacy Exchange...")
		// Approve max uint256
		maxUint256 := new(big.Int).Sub(new(big.Int).Exp(big.NewInt(2), big.NewInt(256), nil), big.NewInt(1))
		tx, err := t.polygon.ApproveUSDC(ctx, signer, api.CTFExchange, maxUint256)
		if err != nil {
			return false, fmt.Errorf("failed to approve legacy exchange: %w", err)
		}
		fmt.Printf("   Tx sent: %s\n", tx)
		if _, err := t.polygon.WaitForTransaction(ctx, tx); err != nil {
			return false, fmt.Errorf("approval tx failed: %w", err)
		}
		sentTx = true
		time.Sleep(2 * time.Second) // Rate limit buffer
	}

	// 2. Approve CTF Operator for Legacy Exchange
	isApprovedLegacy, err := checkApproval(api.CTFExchange)
	if err != nil {
		return false, fmt.Errorf("failed to check legacy CTF approval: %w", err)
	}
	if !isApprovedLegacy {
		fmt.Println("🔓 Approving CTF Operator for Legacy Exchange...")
		tx, err := t.polygon.ApproveCTF(ctx, signer, api.CTFExchange, true)
		if err != nil {
			return false, fmt.Errorf("failed to approve legacy CTF operator: %w", err)
		}
		fmt.Printf("   Tx sent: %s\n", tx)
		if _, err := t.polygon.WaitForTransaction(ctx, tx); err != nil {
			return false, fmt.Errorf("approval tx failed: %w", err)
		}
		sentTx = true
		time.Sleep(2 * time.Second)
	}

	// 3. Approve USDC for NegRisk Exchange (Multi-Outcome)
	allowanceNegRisk, err := checkAllowance(api.NegRiskExchange)
	if err != nil {
		return false, fmt.Errorf("failed to check NegRisk allowance: %w", err)
	}
	if allowanceNegRisk.Cmp(minAllowance) < 0 {
		fmt.Println("🔓 Approving USDC for NegRisk Exchange...")
		maxUint256 := new(big.Int).Sub(new(big.Int).Exp(big.NewInt(2), big.NewInt(256), nil), big.NewInt(1))
		tx, err := t.polygon.ApproveUSDC(ctx, signer, api.NegRiskExchange, maxUint256)
		if err != nil {
			return false, fmt.Errorf("failed to approve NegRisk exchange: %w", err)
		}
		fmt.Printf("   Tx sent: %s\n", tx)
		if _, err := t.polygon.WaitForTransaction(ctx, tx); err != nil {
			return false, fmt.Errorf("approval tx failed: %w", err)
		}
		sentTx = true
		time.Sleep(2 * time.Second)
	}

	// 4. Approve CTF Operator for NegRisk Exchange
	isApprovedNegRisk, err := checkApproval(api.NegRiskExchange)
	if err != nil {
		return false, fmt.Errorf("failed to check NegRisk CTF approval: %w", err)
	}
	if !isApprovedNegRisk {
		fmt.Println("🔓 Approving CTF Operator for NegRisk Exchange...")
		tx, err := t.polygon.ApproveCTF(ctx, signer, api.NegRiskExchange, true)
		if err != nil {
			return false, fmt.Errorf("failed to approve NegRisk CTF operator: %w", err)
		}
		fmt.Printf("   Tx sent: %s\n", tx)
		if _, err := t.polygon.WaitForTransaction(ctx, tx); err != nil {
			return false, fmt.Errorf("approval tx failed: %w", err)
		}
		sentTx = true
	}

	return sentTx, nil
}

// checkSafetyLimits verifies the trade doesn't exceed safety limits
func (t *RealTrader) checkSafetyLimits(tradeAmount float64) error {
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

// RecordLoss records a loss for daily tracking.
// Positive amount = loss, negative amount = gain (reduces daily loss counter).
func (t *RealTrader) RecordLoss(amount float64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Reset daily loss if new day (mirrors checkSafetyLimits)
	if time.Now().Truncate(24*time.Hour) != t.startOfDay {
		t.dailyLoss = 0
		t.startOfDay = time.Now().Truncate(24 * time.Hour)
	}

	t.dailyLoss += amount
	if t.dailyLoss < 0 {
		t.dailyLoss = 0 // Don't let gains create negative loss
	}
}

// Address returns the wallet address
func (t *RealTrader) Address() string {
	if t.paperEngine != nil {
		return "paper"
	}
	return t.client.Address()
}

// GetCTFBalanceFloat returns the on-chain CTF token balance as a float64 (human-readable shares)
func (t *RealTrader) GetCTFBalanceFloat(ctx context.Context, tokenID string) (float64, error) {
	if t.paperEngine != nil {
		return t.GetLivePositionSize(tokenID), nil
	}
	if t.polygon == nil {
		return 0, fmt.Errorf("polygon client not initialized")
	}
	if t.client == nil {
		return 0, fmt.Errorf("exchange client not initialized")
	}

	now := time.Now()
	t.ctfMu.Lock()
	if t.ctfBalanceCache == nil {
		t.ctfBalanceCache = make(map[string]float64)
	}
	if t.lastCTFBalanceUpdate == nil {
		t.lastCTFBalanceUpdate = make(map[string]time.Time)
	}
	if t.lastCTFBalanceTry == nil {
		t.lastCTFBalanceTry = make(map[string]time.Time)
	}

	cachedBal, hasCache := t.ctfBalanceCache[tokenID]
	lastUpdate := t.lastCTFBalanceUpdate[tokenID]
	lastTry := t.lastCTFBalanceTry[tokenID]

	if hasCache && !lastUpdate.IsZero() && now.Sub(lastUpdate) < realTraderCTFBalanceTTL {
		t.ctfMu.Unlock()
		return cachedBal, nil
	}
	if !lastTry.IsZero() && now.Sub(lastTry) < realTraderOnChainRetryBackoff {
		t.ctfMu.Unlock()
		if hasCache {
			return cachedBal, nil
		}
		return 0, fmt.Errorf("ctf balance refresh throttled")
	}
	t.lastCTFBalanceTry[tokenID] = now
	t.ctfMu.Unlock()

	tid := new(big.Int)
	if _, ok := tid.SetString(tokenID, 10); !ok {
		return 0, fmt.Errorf("invalid token id: %s", tokenID)
	}

	bal, err := t.polygon.GetCTFBalance(ctx, t.client.Address(), tid)
	if err != nil {
		if hasCache {
			return cachedBal, nil
		}
		return 0, err
	}

	shares := new(big.Float).SetInt(bal)
	shares = shares.Quo(shares, big.NewFloat(1e6))
	s, _ := shares.Float64()

	t.ctfMu.Lock()
	t.ctfBalanceCache[tokenID] = s
	t.lastCTFBalanceUpdate[tokenID] = time.Now()
	t.ctfMu.Unlock()

	return s, nil
}

func (t *RealTrader) ForceRefreshCTFBalanceFloat(ctx context.Context, tokenID string) (float64, error) {
	t.InvalidateCTFBalanceCache(tokenID)
	return t.GetCTFBalanceFloat(ctx, tokenID)
}

// WaitForFill waits for an order to be filled
func (t *RealTrader) WaitForFill(ctx context.Context, orderID string, timeout time.Duration) (bool, error) {
	if t.paperEngine != nil {
		return t.GetConfirmedFillSize(orderID) > 0, nil
	}
	if orderID == "" {
		return false, nil
	}
	if t.GetConfirmedFillSize(orderID) > 0 {
		return true, nil
	}

	deadline := time.Now().Add(timeout)
	wsTicker := time.NewTicker(25 * time.Millisecond)
	defer wsTicker.Stop()

	for {
		if time.Now().After(deadline) {
			return false, nil
		}

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-wsTicker.C:
			if t.GetConfirmedFillSize(orderID) > 0 {
				return true, nil
			}
		}
	}
}

func (t *RealTrader) EnableRawAPILog(path string) error {
	if t.paperEngine != nil || t.client == nil {
		return nil
	}
	return t.client.EnableRawAPILog(path)
}

func (t *RealTrader) CloseRawAPILog() error {
	if t.paperEngine != nil || t.client == nil {
		return nil
	}
	return t.client.CloseRawAPILog()
}

// GetOpenOrders returns all open orders
func (t *RealTrader) GetOpenOrders(ctx context.Context) ([]api.OpenOrder, error) {
	if t.paperEngine != nil {
		return nil, nil
	}
	return t.client.GetOpenOrders(ctx)
}

// CancelOrder cancels a specific order
func (t *RealTrader) CancelOrderByID(ctx context.Context, orderID string) error {
	if t.paperEngine != nil {
		return nil
	}
	return t.client.CancelOrder(ctx, orderID)
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

// QueryBalancedCTFBalances polls the on-chain CTF contract for the balances of
// two complementary tokens, retrying until both sides are settled and balanced
// (within dust tolerance) or expectedShares are available on each side.
//
// This mirrors the queryBalancedCTFBalances helper that previously existed in
// both cmd/realbot and cmd/util, consolidated here so both binaries share the
// same logic.
func (t *RealTrader) QueryBalancedCTFBalances(
	ctx context.Context,
	token0, token1 string,
	expectedShares float64,
) (bal0, bal1 float64, err0, err1 error) {
	if t.paperEngine != nil {
		return t.GetLivePositionSize(token0), t.GetLivePositionSize(token1), nil, nil
	}
	// Increase attempts from 8 to 20, wait 2.5 seconds between attempts (total 50s timeout)
	// Position updates via REST/WS might be slightly delayed after on-chain mints
	const maxAttempts = 20
	const settleDelay = 2500 * time.Millisecond

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		bal0, err0 = t.GetCTFBalanceFloat(ctx, token0)
		bal1, err1 = t.GetCTFBalanceFloat(ctx, token1)

		if err0 == nil && err1 == nil {
			minBal := math.Min(bal0, bal1)
			if minBal >= 0.000001 {
				// Accept if balanced within dust, or if min balance meets expected shares.
				if math.Abs(bal0-bal1) <= 0.000001 || minBal >= expectedShares-0.05 {
					return
				}
			}
		}

		if attempt < maxAttempts {
			select {
			case <-ctx.Done():
				return bal0, bal1, ctx.Err(), ctx.Err()
			case <-time.After(settleDelay):
			}
		}
	}

	return
}

// QueryBalancedCTFBalanceDelta polls the on-chain CTF balances and returns both
// the latest absolute balances and the incremental balances acquired since the
// provided initial snapshot. This is useful when there is already pre-existing
// inventory and we only want to merge newly bought balanced pairs.
func (t *RealTrader) QueryBalancedCTFBalanceDelta(
	ctx context.Context,
	token0, token1 string,
	initial0, initial1, expectedDelta float64,
) (delta0, delta1, bal0, bal1 float64, err0, err1 error) {
	const maxAttempts = 20
	const settleDelay = 2500 * time.Millisecond

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		bal0, err0 = t.GetCTFBalanceFloat(ctx, token0)
		bal1, err1 = t.GetCTFBalanceFloat(ctx, token1)

		if err0 == nil && err1 == nil {
			delta0 = math.Max(0, bal0-initial0)
			delta1 = math.Max(0, bal1-initial1)
			minDelta := math.Min(delta0, delta1)
			if minDelta >= 0.000001 {
				if math.Abs(delta0-delta1) <= 0.000001 || minDelta >= expectedDelta-0.05 {
					return
				}
			}
		}

		if attempt < maxAttempts {
			select {
			case <-ctx.Done():
				return delta0, delta1, bal0, bal1, ctx.Err(), ctx.Err()
			case <-time.After(settleDelay):
			}
		}
	}

	return
}

// NewTrader creates the appropriate trader based on config
func NewTrader(cfg *core.Config, engine *paper.Engine, orderBook *paper.OrderBook) (Trader, error) {
	if cfg.IsPaperMode() {
		return NewPaperTrader(engine, orderBook), nil
	}
	return NewRealTrader(cfg)
}
