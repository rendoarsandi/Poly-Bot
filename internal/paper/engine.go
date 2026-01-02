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
	Side      string  // "buy" or "sell"
	Outcome   string  // "Yes", "No", "Up", "Down"
	Price     float64
	Quantity  float64
	Value     float64 // Price * Quantity
}

// Position represents current holdings for an outcome
type Position struct {
	Outcome   string
	Quantity  float64
	AvgPrice  float64
	TotalCost float64
}

// Stats holds all trading statistics
type Stats struct {
	TotalTrades    int
	WinningTrades  int
	LosingTrades   int
	RealizedPnL    float64
	UnrealizedPnL  float64
	MaxDrawdown    float64
	PeakBalance    float64
	CurrentBalance float64
	StartingBalance float64
}

// Engine is the paper trading engine
type Engine struct {
	mu sync.RWMutex

	// Configuration
	startingBalance float64
	currentBalance  float64

	// Positions: outcome -> position
	positions map[string]*Position

	// Trade history
	trades []Trade

	// Stats tracking
	totalTrades   int
	realizedPnL   float64
	peakBalance   float64
	maxDrawdown   float64
	winningTrades int
	losingTrades  int

	// Current market prices for unrealized PnL
	currentPrices map[string]float64
}

// NewEngine creates a new paper trading engine
func NewEngine(startingBalance float64) *Engine {
	return &Engine{
		startingBalance: startingBalance,
		currentBalance:  startingBalance,
		peakBalance:     startingBalance,
		positions:       make(map[string]*Position),
		trades:          make([]Trade, 0),
		currentPrices:   make(map[string]float64),
	}
}

// UpdatePrice updates current market price for an outcome and recalculates drawdown
func (e *Engine) UpdatePrice(outcome string, price float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.currentPrices[outcome] = price
	e.recalculateDrawdown()
}

// Buy executes a simulated buy order
func (e *Engine) Buy(outcome string, price, quantity float64) (*Trade, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	cost := price * quantity
	if cost > e.currentBalance {
		return nil, fmt.Errorf("insufficient balance: need %.4f, have %.4f", cost, e.currentBalance)
	}

	// Deduct from balance
	e.currentBalance -= cost

	// Update position
	pos, exists := e.positions[outcome]
	if !exists {
		pos = &Position{Outcome: outcome}
		e.positions[outcome] = pos
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
	e.trades = append(e.trades, trade)

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
	e.trades = append(e.trades, trade)

	return &trade, nil
}

// Redeem simulates market resolution payout
// winningOutcome is the outcome that won (pays $1 per share)
func (e *Engine) Redeem(winningOutcome string) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()

	payout := 0.0

	for outcome, pos := range e.positions {
		if outcome == winningOutcome {
			// Winning shares pay $1 each
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

// LiquidateAll sells all positions at current market prices (emergency exit)
func (e *Engine) LiquidateAll() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()

	totalProceeds := 0.0

	for outcome, pos := range e.positions {
		if pos.Quantity <= 0 {
			continue
		}

		// Get current price, default to cost basis if not available
		price := pos.AvgPrice
		if p, ok := e.currentPrices[outcome]; ok && p > 0 {
			price = p
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
		e.trades = append(e.trades, trade)
	}

	// Clear all positions
	e.positions = make(map[string]*Position)
	e.recalculateDrawdown()

	return totalProceeds
}

func (e *Engine) updateDrawdown() {
	e.recalculateDrawdown()
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
		if price, ok := e.currentPrices[outcome]; ok {
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

	unrealized := 0.0
	for outcome, pos := range e.positions {
		if price, ok := e.currentPrices[outcome]; ok {
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
		UnrealizedPnL:   e.GetUnrealizedPnL(),
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
