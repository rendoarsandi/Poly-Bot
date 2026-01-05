package paper

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// Trade represents a single executed trade
type Trade struct {
	ID        int
	Timestamp time.Time
	Side      string // "buy" or "sell"
	Outcome   string // "Yes", "No", "Up", "Down"
	Price     float64
	Quantity  float64
	Value     float64 // Price * Quantity
}

// Position represents current holdings for an outcome
type Position struct {
	Outcome   string
	MarketID  string // Which market this position belongs to (e.g., "BTC", "ETH")
	Quantity  float64
	AvgPrice  float64
	TotalCost float64
}

// Stats holds all trading statistics
type Stats struct {
	TotalTrades     int
	WinningTrades   int
	LosingTrades    int
	RealizedPnL     float64
	UnrealizedPnL   float64
	MaxDrawdown     float64
	PeakBalance     float64
	CurrentBalance  float64
	StartingBalance float64
}

// Engine is the paper trading engine
type Engine struct {
	mu sync.RWMutex

	// Configuration
	startingBalance float64
	currentBalance  float64

	// Compounding multiplier - increases with profitable rounds
	compoundMultiplier float64
	roundsCompleted    int
	profitableRounds   int

	// Positions: "marketID:outcome" -> position
	positions map[string]*Position

	// Trade history (capped to prevent memory growth)
	trades    []Trade
	maxTrades int

	// Stats tracking
	totalTrades   int
	realizedPnL   float64
	peakBalance   float64
	maxDrawdown   float64
	winningTrades int
	losingTrades  int

	// Current market prices for unrealized PnL (legacy - outcome only)
	currentPrices map[string]float64
	// Bid/Ask prices for realistic taker simulation (legacy - outcome only)
	currentBids map[string]float64 // Price you get when SELLING (taker)
	currentAsks map[string]float64 // Price you pay when BUYING (taker)

	// Per-market bid/ask prices: "marketID:outcome" -> price
	marketBids map[string]float64
	marketAsks map[string]float64
}

// NewEngine creates a new paper trading engine
func NewEngine(startingBalance float64) *Engine {
	return &Engine{
		startingBalance:    startingBalance,
		currentBalance:     startingBalance,
		peakBalance:        startingBalance,
		compoundMultiplier: 1.0,  // Start at 1x
		maxTrades:          1000, // Cap trade history to prevent memory growth
		positions:          make(map[string]*Position),
		trades:             make([]Trade, 0),
		currentPrices:      make(map[string]float64),
		currentBids:        make(map[string]float64),
		currentAsks:        make(map[string]float64),
		marketBids:         make(map[string]float64),
		marketAsks:         make(map[string]float64),
	}
}

// UpdatePrice updates current market price for an outcome and recalculates drawdown
func (e *Engine) UpdatePrice(outcome string, price float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.currentPrices[outcome] = price
	e.recalculateDrawdown()
}

// UpdateMarketData performs a batch update of price and bid/ask data for an outcome
func (e *Engine) UpdateMarketData(marketID, outcome string, price, bid, ask float64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if price > 0 {
		e.currentPrices[outcome] = price
	}
	if bid > 0 {
		e.currentBids[outcome] = bid
	}
	if ask > 0 {
		e.currentAsks[outcome] = ask
	}

	if marketID != "" {
		key := marketID + ":" + outcome
		if bid > 0 {
			e.marketBids[key] = bid
		}
		if ask > 0 {
			e.marketAsks[key] = ask
		}
	}

	e.recalculateDrawdown()
}

// UpdateBidAsk updates bid/ask prices for realistic taker simulation
func (e *Engine) UpdateBidAsk(outcome string, bid, ask float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if bid > 0 {
		e.currentBids[outcome] = bid
	}
	if ask > 0 {
		e.currentAsks[outcome] = ask
	}
}

// UpdateMarketBidAsk updates bid/ask prices for a specific market
func (e *Engine) UpdateMarketBidAsk(marketID, outcome string, bid, ask float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	key := marketID + ":" + outcome
	if bid > 0 {
		e.marketBids[key] = bid
	}
	if ask > 0 {
		e.marketAsks[key] = ask
	}
}

// GetMarketBidAsk returns current bid/ask for a market position
func (e *Engine) GetMarketBidAsk(marketID, outcome string) (bid, ask float64) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	key := marketID + ":" + outcome
	return e.marketBids[key], e.marketAsks[key]
}

// Buy executes a simulated buy order
func (e *Engine) Buy(outcome string, price, quantity float64) (*Trade, error) {
	return e.BuyForMarket("", outcome, price, quantity)
}

// BuyForMarket executes a simulated buy order for a specific market
func (e *Engine) BuyForMarket(marketID, outcome string, price, quantity float64) (*Trade, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	cost := price * quantity
	if cost > e.currentBalance {
		return nil, fmt.Errorf("insufficient balance: need %.4f, have %.4f", cost, e.currentBalance)
	}

	// Deduct from balance
	e.currentBalance -= cost

	// Create position key that includes market ID
	posKey := outcome
	if marketID != "" {
		posKey = marketID + ":" + outcome
	}

	// Update position
	pos, exists := e.positions[posKey]
	if !exists {
		pos = &Position{Outcome: outcome, MarketID: marketID}
		e.positions[posKey] = pos
	}

	// Calculate new average price
	totalQty := pos.Quantity + quantity
	if totalQty > 0 {
		pos.AvgPrice = (pos.TotalCost + cost) / totalQty
	}
	pos.Quantity = totalQty
	pos.TotalCost += cost

	// Record trade
	e.totalTrades++
	trade := Trade{
		ID:        e.totalTrades,
		Timestamp: time.Now(),
		Side:      "buy",
		Outcome:   outcome,
		Price:     price,
		Quantity:  quantity,
		Value:     cost,
	}
	e.addTrade(trade)

	return &trade, nil
}

// Sell executes a simulated sell order
func (e *Engine) Sell(outcome string, price, quantity float64) (*Trade, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	pos, exists := e.positions[outcome]
	if !exists || pos.Quantity < quantity {
		available := 0.0
		if exists {
			available = pos.Quantity
		}
		return nil, fmt.Errorf("insufficient position: need %.4f, have %.4f", quantity, available)
	}

	// Calculate PnL for this sale
	proceeds := price * quantity
	costBasis := pos.AvgPrice * quantity
	pnl := proceeds - costBasis

	// Update realized PnL
	e.realizedPnL += pnl
	if pnl > 0 {
		e.winningTrades++
	} else if pnl < 0 {
		e.losingTrades++
	}

	// Add proceeds to balance
	e.currentBalance += proceeds

	// Update position
	pos.Quantity -= quantity
	pos.TotalCost -= costBasis
	if pos.Quantity <= 0.0001 {
		delete(e.positions, outcome)
	}

	// Update peak and drawdown
	e.updateDrawdown()

	// Record trade
	e.totalTrades++
	trade := Trade{
		ID:        e.totalTrades,
		Timestamp: time.Now(),
		Side:      "sell",
		Outcome:   outcome,
		Price:     price,
		Quantity:  quantity,
		Value:     proceeds,
	}
	e.addTrade(trade)

	return &trade, nil
}

// RedemptionResult holds detailed info about a market redemption
type RedemptionResult struct {
	WinningOutcome string
	WinningShares  float64
	WinningPayout  float64
	WinningCost    float64
	WinningPnL     float64
	LosingOutcome  string
	LosingShares   float64
	LosingCost     float64
	TotalPayout    float64
	TotalPnL       float64
}

// Redeem simulates market resolution payout
// winningOutcome is the outcome that won (pays $1 per share)
// Polymarket charges NO fees (0% on trading, deposits, withdrawals, and payouts)
func (e *Engine) Redeem(winningOutcome string) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()

	payout := 0.0

	for _, pos := range e.positions {
		if pos.Outcome == winningOutcome {
			// Winning shares pay $1 each (no fees!)
			proceeds := pos.Quantity * 1.0
			pnl := proceeds - pos.TotalCost
			e.realizedPnL += pnl
			e.currentBalance += proceeds
			payout += proceeds

			if pnl > 0 {
				e.winningTrades++
			} else {
				e.losingTrades++
			}
		} else {
			// Losing shares are worthless
			e.realizedPnL -= pos.TotalCost
			e.losingTrades++
		}
	}

	// Clear all positions
	e.positions = make(map[string]*Position)

	e.updateDrawdown()
	return payout
}

// RedeemWithDetails simulates market resolution and returns detailed results
func (e *Engine) RedeemWithDetails(winningOutcome string) *RedemptionResult {
	e.mu.Lock()
	defer e.mu.Unlock()

	result := &RedemptionResult{
		WinningOutcome: winningOutcome,
	}

	for _, pos := range e.positions {
		// Correctly match the outcome even if the key has a "MarketID:" prefix
		if pos.Outcome == winningOutcome {
			// Winning shares pay $1 each (no fees!)
			proceeds := pos.Quantity * 1.0
			pnl := proceeds - pos.TotalCost
			e.realizedPnL += pnl
			e.currentBalance += proceeds

			result.WinningShares = pos.Quantity
			result.WinningPayout = proceeds
			result.WinningCost = pos.TotalCost
			result.WinningPnL = pnl
			result.TotalPayout += proceeds

			if pnl > 0 {
				e.winningTrades++
			} else {
				e.losingTrades++
			}
		} else {
			// Losing shares are worthless
			e.realizedPnL -= pos.TotalCost
			e.losingTrades++

			result.LosingOutcome = pos.Outcome
			result.LosingShares = pos.Quantity
			result.LosingCost = pos.TotalCost
		}
	}

	// Clear all positions
	e.positions = make(map[string]*Position)

	e.updateDrawdown()
	return result
}

// LiquidateAll sells all positions at current BID prices (taker - chasing liquidity)
// This simulates realistic emergency exit where you SELL at the BID (worse price)
func (e *Engine) LiquidateAll() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()

	totalProceeds := 0.0

	for outcome, pos := range e.positions {
		if pos.Quantity <= 0 {
			continue
		}

		// TAKER SELL: Use BID price (the price buyers are willing to pay)
		// This is worse than mid-price, simulating realistic slippage
		price := pos.AvgPrice // Fallback to cost basis

		// Price sanity bounds - reject obviously bad data
		const minSanePrice = 0.15
		const maxSanePrice = 0.85

		if bid, ok := e.currentBids[outcome]; ok && bid >= minSanePrice && bid <= maxSanePrice {
			price = bid // Use BID for taker sells
			/*
				fmt.Printf("🔴 TAKER SELL %s: %.0f shares @ BID $%.3f (chasing liquidity)\n",
					outcome, pos.Quantity, bid)
			*/
		} else if p, ok := e.currentPrices[outcome]; ok && p >= minSanePrice && p <= maxSanePrice {
			// Fallback to mid-price with simulated slippage (2% worse)
			price = p * 0.98
			/*
				fmt.Printf("🔴 TAKER SELL %s: %.0f shares @ $%.3f (mid-2%% slippage)\n",
					outcome, pos.Quantity, price)
			*/
		} else {
			// Use cost basis as last resort
			/*
				fmt.Printf("🔴 TAKER SELL %s: %.0f shares @ $%.3f (cost basis - no valid price)\n",
					outcome, pos.Quantity, price)
			*/
		}

		proceeds := pos.Quantity * price
		pnl := proceeds - pos.TotalCost
		e.realizedPnL += pnl
		e.currentBalance += proceeds
		totalProceeds += proceeds

		if pnl > 0 {
			e.winningTrades++
		} else if pnl < 0 {
			e.losingTrades++
		}

		e.totalTrades++
		trade := Trade{
			ID:        e.totalTrades,
			Timestamp: time.Now(),
			Side:      "sell",
			Outcome:   outcome,
			Price:     price,
			Quantity:  pos.Quantity,
			Value:     proceeds,
		}
		e.addTrade(trade)
	}

	// Clear all positions
	e.positions = make(map[string]*Position)
	e.recalculateDrawdown()

	return totalProceeds
}

func (e *Engine) updateDrawdown() {
	e.recalculateDrawdown()
}

// addTrade records a trade and trims history if needed (must be called with lock held)
func (e *Engine) addTrade(trade Trade) {
	e.trades = append(e.trades, trade)
	// Trim trade history if it exceeds max to prevent memory growth
	if len(e.trades) > e.maxTrades {
		e.trades = e.trades[len(e.trades)-e.maxTrades:]
	}
}

// recalculateDrawdown updates max drawdown based on current equity
func (e *Engine) recalculateDrawdown() {
	totalEquity := e.currentBalance + e.getUnrealizedValue()
	if totalEquity > e.peakBalance {
		e.peakBalance = totalEquity
	}
	if e.peakBalance > 0 {
		drawdown := (e.peakBalance - totalEquity) / e.peakBalance
		if drawdown > e.maxDrawdown {
			e.maxDrawdown = drawdown
		}
	}
}

// GetEquity returns total equity (balance + unrealized value)
func (e *Engine) GetEquity() float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.currentBalance + e.getUnrealizedValue()
}

func (e *Engine) getUnrealizedValue() float64 {
	value := 0.0
	for outcome, pos := range e.positions {
		// Use BID price for valuation (what we could sell for)
		// This is more conservative and realistic
		if bid, ok := e.currentBids[outcome]; ok && bid > 0 {
			value += pos.Quantity * bid
		} else if price, ok := e.currentPrices[outcome]; ok {
			value += pos.Quantity * price
		} else {
			// Use cost basis if no current price
			value += pos.TotalCost
		}
	}
	return value
}

// GetUnrealizedPnL returns unrealized profit/loss
func (e *Engine) GetUnrealizedPnL() float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.getUnrealizedPnL()
}

func (e *Engine) getUnrealizedPnL() float64 {
	unrealized := 0.0
	for outcome, pos := range e.positions {
		// Use BID price for valuation (what we could sell for)
		if bid, ok := e.currentBids[outcome]; ok && bid > 0 {
			currentValue := pos.Quantity * bid
			unrealized += currentValue - pos.TotalCost
		} else if price, ok := e.currentPrices[outcome]; ok {
			currentValue := pos.Quantity * price
			unrealized += currentValue - pos.TotalCost
		}
	}
	return unrealized
}

// GetStats returns current trading statistics
func (e *Engine) GetStats() Stats {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return Stats{
		TotalTrades:     e.totalTrades,
		WinningTrades:   e.winningTrades,
		LosingTrades:    e.losingTrades,
		RealizedPnL:     e.realizedPnL,
		UnrealizedPnL:   e.getUnrealizedPnL(),
		MaxDrawdown:     e.maxDrawdown * 100, // as percentage
		PeakBalance:     e.peakBalance,
		CurrentBalance:  e.currentBalance,
		StartingBalance: e.startingBalance,
	}
}

// GetPositions returns current positions
func (e *Engine) GetPositions() map[string]Position {
	e.mu.RLock()
	defer e.mu.RUnlock()

	result := make(map[string]Position)
	for k, v := range e.positions {
		result[k] = *v
	}
	return result
}

// PositionPnL contains real-time P&L info for a position
type PositionPnL struct {
	Position
	CurrentBid    float64 // Current bid price (what we can sell for)
	CurrentAsk    float64 // Current ask price
	MarketValue   float64 // Current value if sold at bid
	UnrealizedPnL float64 // Current P&L if sold now
	LockedPnL     float64 // Guaranteed P&L if held to resolution ($1 payout)
}

// GetPositionsWithPnL returns positions with real-time P&L calculations
func (e *Engine) GetPositionsWithPnL() map[string]PositionPnL {
	e.mu.RLock()
	defer e.mu.RUnlock()

	result := make(map[string]PositionPnL)
	for k, pos := range e.positions {
		pnl := PositionPnL{
			Position: *pos,
		}

		// Get current bid/ask for this position
		key := k // Already includes marketID:outcome
		if bid, ok := e.marketBids[key]; ok && bid > 0 {
			pnl.CurrentBid = bid
		} else if bid, ok := e.currentBids[pos.Outcome]; ok && bid > 0 {
			pnl.CurrentBid = bid
		}

		if ask, ok := e.marketAsks[key]; ok && ask > 0 {
			pnl.CurrentAsk = ask
		} else if ask, ok := e.currentAsks[pos.Outcome]; ok && ask > 0 {
			pnl.CurrentAsk = ask
		}

		// Calculate market value (what we could sell for now)
		if pnl.CurrentBid > 0 {
			pnl.MarketValue = pos.Quantity * pnl.CurrentBid
			pnl.UnrealizedPnL = pnl.MarketValue - pos.TotalCost
		}

		// Locked P&L assumes $1 payout at resolution
		// This is only meaningful for matched pairs
		pnl.LockedPnL = (pos.Quantity * 1.0) - pos.TotalCost

		result[k] = pnl
	}
	return result
}

// GetRecentTrades returns the last n trades
func (e *Engine) GetRecentTrades(n int) []Trade {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if n > len(e.trades) {
		n = len(e.trades)
	}
	start := len(e.trades) - n
	result := make([]Trade, n)
	copy(result, e.trades[start:])
	return result
}

// GetBalance returns current cash balance
func (e *Engine) GetBalance() float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.currentBalance
}

// GetExposure returns exposure metrics
func (e *Engine) GetExposure() (totalExposure float64, maxSingleExposure float64) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	for _, pos := range e.positions {
		totalExposure += pos.TotalCost
		if pos.TotalCost > maxSingleExposure {
			maxSingleExposure = pos.TotalCost
		}
	}
	return
}

// Round helper for display
func Round(val float64, precision int) float64 {
	ratio := math.Pow(10, float64(precision))
	return math.Round(val*ratio) / ratio
}

// GetCompoundMultiplier returns the current compounding multiplier
func (e *Engine) GetCompoundMultiplier() float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.compoundMultiplier
}

// UpdateCompoundMultiplier updates the multiplier based on round profit/loss
// If profitable, multiplier increases proportionally (e.g., 5% profit = 1.05x)
// If loss, multiplier resets to 1.0
func (e *Engine) UpdateCompoundMultiplier(roundPnL float64, startingEquity float64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.roundsCompleted++

	if roundPnL > 0 && startingEquity > 0 {
		// Calculate profit percentage
		profitPercent := roundPnL / startingEquity

		// Increase multiplier by profit percentage (compounding)
		e.compoundMultiplier *= (1.0 + profitPercent)

		// Cap at 3x to prevent excessive risk
		if e.compoundMultiplier > 3.0 {
			e.compoundMultiplier = 3.0
		}

		e.profitableRounds++
	} else if roundPnL < 0 {
		// On loss, reduce multiplier but don't go below 1.0
		// Lose 50% of the bonus (multiplier - 1.0)
		bonus := e.compoundMultiplier - 1.0
		if bonus > 0 {
			e.compoundMultiplier = 1.0 + (bonus * 0.5)
		}
		// If still losing, reset to 1.0
		if e.compoundMultiplier < 1.0 {
			e.compoundMultiplier = 1.0
		}
	}
	// If breakeven (roundPnL == 0), keep multiplier unchanged
}

// GetCompoundStats returns compounding statistics
func (e *Engine) GetCompoundStats() (multiplier float64, rounds int, profitable int) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.compoundMultiplier, e.roundsCompleted, e.profitableRounds
}
