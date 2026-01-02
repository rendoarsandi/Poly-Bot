package paper

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// RiskConfig configures risk management parameters
type RiskConfig struct {
	MaxExposure          float64 // Maximum total exposure in dollars
	MaxUnmatchedRatio    float64 // Maximum unmatched inventory ratio (e.g., 0.20 = 20%)
	MaxUnmatchedShares   float64 // Maximum unmatched shares on one side
	SkewThreshold        float64 // Threshold to trigger rebalancing (e.g., 0.10 = 10%)
	KillSwitchDrawdown   float64 // Max drawdown to trigger kill switch (e.g., 0.10 = 10%)
}

// DefaultRiskConfig returns default risk configuration
func DefaultRiskConfig() RiskConfig {
	return RiskConfig{
		MaxExposure:          500.0,  // $500 max exposure
		MaxUnmatchedRatio:    0.20,   // 20% max unmatched
		MaxUnmatchedShares:   500.0,  // 500 shares max on one side
		SkewThreshold:        0.10,   // 10% skew triggers rebalance
		KillSwitchDrawdown:   0.10,   // 10% drawdown triggers kill
	}
}

// RiskAction represents an action the risk manager wants to take
type RiskAction string

const (
	RiskActionNone       RiskAction = "none"
	RiskActionRebalance  RiskAction = "rebalance"
	RiskActionReduceSize RiskAction = "reduce_size"
	RiskActionKillSwitch RiskAction = "kill_switch"
)

// RiskAlert represents a risk alert
type RiskAlert struct {
	Timestamp time.Time
	Action    RiskAction
	Reason    string
	Details   map[string]float64
}

// RiskManager manages trading risk
type RiskManager struct {
	mu           sync.RWMutex
	config       RiskConfig
	engine       *Engine
	orderBook    *OrderBook
	outcomes     []string

	// State
	killSwitchTriggered bool
	alerts              []RiskAlert
}

// NewRiskManager creates a new risk manager
func NewRiskManager(config RiskConfig, engine *Engine, orderBook *OrderBook, outcomes []string) *RiskManager {
	return &RiskManager{
		config:    config,
		engine:    engine,
		orderBook: orderBook,
		outcomes:  outcomes,
		alerts:    make([]RiskAlert, 0),
	}
}

// IsKillSwitchTriggered returns if kill switch has been activated
func (rm *RiskManager) IsKillSwitchTriggered() bool {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	return rm.killSwitchTriggered
}

// Evaluate checks current risk state and returns recommended action
func (rm *RiskManager) Evaluate() (RiskAction, string) {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	if rm.killSwitchTriggered {
		return RiskActionKillSwitch, "kill switch already triggered"
	}

	// Check drawdown - always triggers kill switch
	stats := rm.engine.GetStats()
	if stats.MaxDrawdown > rm.config.KillSwitchDrawdown*100 {
		rm.triggerKillSwitch(fmt.Sprintf("max drawdown %.2f%% exceeded threshold %.2f%%",
			stats.MaxDrawdown, rm.config.KillSwitchDrawdown*100))
		return RiskActionKillSwitch, "max drawdown exceeded"
	}

	// Get exposure info
	totalExposure, _ := rm.engine.GetExposure()
	openOrderValue := rm.orderBook.GetOpenOrderValue()
	combinedExposure := totalExposure + openOrderValue

	// Get unmatched info
	positions := rm.engine.GetPositions()
	unmatchedRatio := 0.0
	unmatched := 0.0

	if len(positions) > 0 && len(rm.outcomes) == 2 {
		pos1 := positions[rm.outcomes[0]]
		pos2 := positions[rm.outcomes[1]]
		unmatched = math.Abs(pos1.Quantity - pos2.Quantity)
		totalShares := pos1.Quantity + pos2.Quantity
		if totalShares > 0 {
			unmatchedRatio = unmatched / totalShares
		}
	}

	// PRD Kill Switch: Open_Positions > $500 AND Unmatched_Sides > 20%
	if totalExposure > rm.config.MaxExposure && unmatchedRatio > rm.config.MaxUnmatchedRatio {
		rm.triggerKillSwitch(fmt.Sprintf("exposure $%.2f AND unmatched %.1f%% both exceed limits",
			totalExposure, unmatchedRatio*100))
		return RiskActionKillSwitch, "exposure AND unmatched both exceeded (PRD kill switch)"
	}

	// Also trigger if unmatched shares exceed absolute limit
	if unmatched > rm.config.MaxUnmatchedShares {
		rm.triggerKillSwitch(fmt.Sprintf("unmatched shares %.0f exceeds max %.0f",
			unmatched, rm.config.MaxUnmatchedShares))
		return RiskActionKillSwitch, "unmatched inventory exceeded"
	}

	// Reduce size if exposure is high (but not kill)
	if combinedExposure > rm.config.MaxExposure {
		return RiskActionReduceSize, fmt.Sprintf("exposure $%.2f exceeds max $%.2f",
			combinedExposure, rm.config.MaxExposure)
	}

	// Check inventory skew for rebalancing
	skew, heavySide := rm.calculateInventorySkew()
	if skew > rm.config.SkewThreshold {
		return RiskActionRebalance, fmt.Sprintf("inventory skew %.1f%% on %s side", skew*100, heavySide)
	}

	return RiskActionNone, ""
}

// calculateInventorySkew returns the skew ratio and which side is heavy
func (rm *RiskManager) calculateInventorySkew() (float64, string) {
	if len(rm.outcomes) != 2 {
		return 0, ""
	}

	positions := rm.engine.GetPositions()
	pos1 := positions[rm.outcomes[0]]
	pos2 := positions[rm.outcomes[1]]

	total := pos1.Quantity + pos2.Quantity
	if total == 0 {
		return 0, ""
	}

	diff := math.Abs(pos1.Quantity - pos2.Quantity)
	skew := diff / total

	heavySide := rm.outcomes[0]
	if pos2.Quantity > pos1.Quantity {
		heavySide = rm.outcomes[1]
	}

	return skew, heavySide
}

// GetSkewAdjustment returns how much to adjust ladder prices to rebalance
// Returns: (light side outcome, price adjustment)
func (rm *RiskManager) GetSkewAdjustment() (string, float64) {
	if len(rm.outcomes) != 2 {
		return "", 0
	}

	positions := rm.engine.GetPositions()
	pos1 := positions[rm.outcomes[0]]
	pos2 := positions[rm.outcomes[1]]

	diff := pos1.Quantity - pos2.Quantity

	if math.Abs(diff) < 10 { // Less than 10 shares difference, no adjustment needed
		return "", 0
	}

	// If outcome[0] is heavy, we need to bid more aggressively for outcome[1]
	if diff > 0 {
		// Increase bid for outcome[1] (the light side)
		adjustment := 0.01 * (diff / 100) // Increase by 1 cent per 100 shares imbalance
		if adjustment > 0.05 {
			adjustment = 0.05 // Cap at 5 cents
		}
		return rm.outcomes[1], adjustment
	} else {
		// Increase bid for outcome[0]
		adjustment := 0.01 * (-diff / 100)
		if adjustment > 0.05 {
			adjustment = 0.05
		}
		return rm.outcomes[0], adjustment
	}
}

// triggerKillSwitch activates kill switch
func (rm *RiskManager) triggerKillSwitch(reason string) {
	rm.killSwitchTriggered = true
	alert := RiskAlert{
		Timestamp: time.Now(),
		Action:    RiskActionKillSwitch,
		Reason:    reason,
	}
	rm.alerts = append(rm.alerts, alert)
}

// ExecuteKillSwitch cancels all orders and prepares for shutdown
func (rm *RiskManager) ExecuteKillSwitch() {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	fmt.Println()
	fmt.Println("🚨🚨🚨 KILL SWITCH ACTIVATED 🚨🚨🚨")
	fmt.Println("Reason:", rm.alerts[len(rm.alerts)-1].Reason)

	// Cancel all open orders
	cancelled := rm.orderBook.CancelAllOrders()
	fmt.Printf("Cancelled %d open orders\n", cancelled)

	// Print final positions
	positions := rm.engine.GetPositions()
	if len(positions) > 0 {
		fmt.Println("Remaining positions (manual intervention required):")
		for outcome, pos := range positions {
			fmt.Printf("  %s: %.0f shares @ $%.3f avg\n", outcome, pos.Quantity, pos.AvgPrice)
		}
	}

	fmt.Println("🛑 Bot halted. Please review positions manually.")
}

// Reset resets the kill switch (use with caution)
func (rm *RiskManager) Reset() {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.killSwitchTriggered = false
	rm.alerts = make([]RiskAlert, 0)
}

// GetAlerts returns all risk alerts
func (rm *RiskManager) GetAlerts() []RiskAlert {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	result := make([]RiskAlert, len(rm.alerts))
	copy(result, rm.alerts)
	return result
}

// CanPlaceOrder checks if a new order can be placed within risk limits
func (rm *RiskManager) CanPlaceOrder(orderValue float64) bool {
	if rm.IsKillSwitchTriggered() {
		return false
	}

	totalExposure, _ := rm.engine.GetExposure()
	openOrderValue := rm.orderBook.GetOpenOrderValue()

	return (totalExposure + openOrderValue + orderValue) <= rm.config.MaxExposure
}

// PrintStatus prints current risk status
func (rm *RiskManager) PrintStatus() {
	positions := rm.engine.GetPositions()
	totalExposure, _ := rm.engine.GetExposure()
	openOrderValue := rm.orderBook.GetOpenOrderValue()
	skew, heavySide := rm.calculateInventorySkew()
	stats := rm.engine.GetStats()

	fmt.Println("⚠️  Risk Status:")
	fmt.Printf("  Exposure: $%.2f / $%.2f (%.1f%%)\n",
		totalExposure+openOrderValue, rm.config.MaxExposure,
		(totalExposure+openOrderValue)/rm.config.MaxExposure*100)
	fmt.Printf("  Max Drawdown: %.2f%% / %.2f%%\n",
		stats.MaxDrawdown, rm.config.KillSwitchDrawdown*100)

	if len(positions) == 2 && len(rm.outcomes) == 2 {
		pos1 := positions[rm.outcomes[0]]
		pos2 := positions[rm.outcomes[1]]
		unmatched := math.Abs(pos1.Quantity - pos2.Quantity)
		fmt.Printf("  Inventory: %s=%.0f, %s=%.0f (unmatched: %.0f)\n",
			rm.outcomes[0], pos1.Quantity, rm.outcomes[1], pos2.Quantity, unmatched)
		fmt.Printf("  Skew: %.1f%% (%s heavy)\n", skew*100, heavySide)
	}

	if rm.killSwitchTriggered {
		fmt.Println("  🚨 KILL SWITCH: ACTIVE")
	}
}
