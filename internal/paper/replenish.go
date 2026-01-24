package paper

import (
	"sync/atomic"
)

// ReplenishDecision contains the result of a replenishment check
type ReplenishDecision struct {
	ShouldReplenish bool
	Amount          float64
	Reason          string
}

// ReplenishParams contains all inputs needed for the replenishment decision
type ReplenishParams struct {
	CurrentShares      float64 // Current inventory level
	TargetBuffer       float64 // Target inventory (baseTradeSize * MaxAggressionMultiplier)
	SellMargin         float64 // Current sell margin percentage
	MinMarginThreshold float64 // Minimum margin to consider replenishing (SplitMinMarginSell - 1.0)
	CurrentBalance     float64 // Current account balance
	ReplenishAmount    float64 // Amount to replenish (baseTradeSize * 2.0)
	MaxBalancePercent  float64 // Max percentage of balance allowed in inventory (0.30 = 30%)
}

// ReplenishController manages replenishment state and decisions
type ReplenishController struct {
	inProgress atomic.Bool
}

// NewReplenishController creates a new replenishment controller
func NewReplenishController() *ReplenishController {
	return &ReplenishController{}
}

// CheckReplenish evaluates whether inventory should be replenished
// Returns a decision with reasoning
func (r *ReplenishController) CheckReplenish(params ReplenishParams) ReplenishDecision {
	// Check if replenishment is already in progress
	if r.inProgress.Load() {
		return ReplenishDecision{
			ShouldReplenish: false,
			Amount:          0,
			Reason:          "replenish already in progress",
		}
	}

	// Check if inventory is low enough to warrant replenishment
	threshold := params.TargetBuffer * 0.4
	if params.CurrentShares >= threshold {
		return ReplenishDecision{
			ShouldReplenish: false,
			Amount:          0,
			Reason:          "inventory above threshold",
		}
	}

	// Check if margin is high enough to justify replenishment
	if params.SellMargin < params.MinMarginThreshold {
		return ReplenishDecision{
			ShouldReplenish: false,
			Amount:          0,
			Reason:          "margin below threshold",
		}
	}

	// Check balance cap - don't exceed MaxBalancePercent of balance in inventory
	maxAllowed := params.CurrentBalance * params.MaxBalancePercent
	projectedInventory := params.CurrentShares + params.ReplenishAmount
	if projectedInventory >= maxAllowed {
		return ReplenishDecision{
			ShouldReplenish: false,
			Amount:          0,
			Reason:          "would exceed balance cap",
		}
	}

	return ReplenishDecision{
		ShouldReplenish: true,
		Amount:          params.ReplenishAmount,
		Reason:          "low inventory with good margin",
	}
}

// MarkInProgress sets the in-progress flag (call before starting replenish)
// Returns false if already in progress (atomically checked)
func (r *ReplenishController) MarkInProgress() bool {
	return r.inProgress.CompareAndSwap(false, true)
}

// MarkComplete clears the in-progress flag (call when replenish finishes)
func (r *ReplenishController) MarkComplete() {
	r.inProgress.Store(false)
}

// IsInProgress returns whether a replenishment is currently in progress
func (r *ReplenishController) IsInProgress() bool {
	return r.inProgress.Load()
}
