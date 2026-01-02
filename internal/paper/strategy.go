package paper

import (
	"fmt"
	"strconv"
	"time"
)

// Strategy implements the Gabagool paper trading strategy
type Strategy struct {
	engine  *Engine
	display *Display

	// Configuration
	targetMargin   float64 // Minimum profit margin to trade (e.g., 0.02 = 2%)
	orderSize      float64 // Shares per order
	maxExposure    float64 // Maximum total exposure
	cooldownPeriod time.Duration

	// State
	lastTradeTime time.Time
	outcomes      []string // The two outcome names (Yes/No or Up/Down)
}

// NewStrategy creates a new paper trading strategy
func NewStrategy(engine *Engine, display *Display, outcomes []string) *Strategy {
	return &Strategy{
		engine:         engine,
		display:        display,
		targetMargin:   0.02,  // 2% minimum margin
		orderSize:      100,   // 100 shares per side
		maxExposure:    500,   // $500 max exposure
		cooldownPeriod: 5 * time.Second,
		outcomes:       outcomes,
	}
}

// SetParameters configures strategy parameters
func (s *Strategy) SetParameters(targetMargin, orderSize, maxExposure float64) {
	s.targetMargin = targetMargin
	s.orderSize = orderSize
	s.maxExposure = maxExposure
}

// Evaluate checks if there's a trading opportunity and executes if profitable
func (s *Strategy) Evaluate(prices map[string]string) (traded bool, err error) {
	if len(s.outcomes) != 2 {
		return false, fmt.Errorf("need exactly 2 outcomes, got %d", len(s.outcomes))
	}

	// Check cooldown
	if time.Since(s.lastTradeTime) < s.cooldownPeriod {
		return false, nil
	}

	// Get prices for both outcomes
	price1Str := prices[s.outcomes[0]]
	price2Str := prices[s.outcomes[1]]

	if price1Str == "" || price2Str == "" {
		return false, nil
	}

	price1, err := strconv.ParseFloat(price1Str, 64)
	if err != nil {
		return false, fmt.Errorf("invalid price for %s: %v", s.outcomes[0], err)
	}

	price2, err := strconv.ParseFloat(price2Str, 64)
	if err != nil {
		return false, fmt.Errorf("invalid price for %s: %v", s.outcomes[1], err)
	}

	// Update current prices in engine for unrealized PnL
	s.engine.UpdatePrice(s.outcomes[0], price1)
	s.engine.UpdatePrice(s.outcomes[1], price2)

	// Calculate discount sum
	sum := price1 + price2
	margin := 1.0 - sum

	// Check if profitable opportunity
	if margin < s.targetMargin {
		return false, nil
	}

	// Check exposure limits
	totalExposure, _ := s.engine.GetExposure()
	tradeCost := (price1 + price2) * s.orderSize

	if totalExposure+tradeCost > s.maxExposure {
		return false, nil
	}

	// Check balance
	if s.engine.GetBalance() < tradeCost {
		return false, nil
	}

	// Execute trades on both sides (the arbitrage)
	trade1, err := s.engine.Buy(s.outcomes[0], price1, s.orderSize)
	if err != nil {
		return false, fmt.Errorf("failed to buy %s: %w", s.outcomes[0], err)
	}
	s.display.PrintTrade(trade1)

	trade2, err := s.engine.Buy(s.outcomes[1], price2, s.orderSize)
	if err != nil {
		return false, fmt.Errorf("failed to buy %s: %w", s.outcomes[1], err)
	}
	s.display.PrintTrade(trade2)

	// Log the arbitrage
	fmt.Printf("✅ ARBITRAGE EXECUTED: Bought %.0f of each side @ sum=%.4f (%.2f%% margin)\n",
		s.orderSize, sum, margin*100)

	s.lastTradeTime = time.Now()
	return true, nil
}

// SimulateResolution simulates market resolution (for testing)
func (s *Strategy) SimulateResolution(winningOutcome string) {
	payout := s.engine.Redeem(winningOutcome)
	fmt.Printf("🏆 MARKET RESOLVED: %s won! Payout: $%.2f\n", winningOutcome, payout)
}
