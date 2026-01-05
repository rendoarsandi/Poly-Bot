package paper

import (
	"fmt"
	"time"
)

// MarketState represents the current state of a market
type MarketState string

const (
	MarketStateActive   MarketState = "active"
	MarketStateEnding   MarketState = "ending" // Last few minutes
	MarketStateResolved MarketState = "resolved"
	MarketStatePaused   MarketState = "paused"
)

// MarketInfo holds information about the current market
type MarketInfo struct {
	Slug           string
	ConditionID    string
	Outcomes       []string
	EndTime        time.Time
	State          MarketState
	WinningOutcome string // Set when resolved
}

// MarketMonitor monitors market state and handles resolution
type MarketMonitor struct {
	engine    *Engine
	orderBook *OrderBook
	ladderMgr *LadderManager
	riskMgr   *RiskManager

	currentMarket *MarketInfo

	// Track if we already printed end message
	endMessagePrinted bool

	// Callbacks
	onResolution func(winningOutcome string, payout float64)
	onMarketEnd  func()
}

// NewMarketMonitor creates a new market monitor
func NewMarketMonitor(engine *Engine, orderBook *OrderBook, ladderMgr *LadderManager, riskMgr *RiskManager) *MarketMonitor {
	return &MarketMonitor{
		engine:    engine,
		orderBook: orderBook,
		ladderMgr: ladderMgr,
		riskMgr:   riskMgr,
	}
}

// SetMarket sets the current market being monitored
func (mm *MarketMonitor) SetMarket(slug, conditionID string, outcomes []string, endTime time.Time) {
	mm.currentMarket = &MarketInfo{
		Slug:        slug,
		ConditionID: conditionID,
		Outcomes:    outcomes,
		EndTime:     endTime,
		State:       MarketStateActive,
	}
}

// SetResolutionCallback sets callback for market resolution
func (mm *MarketMonitor) SetResolutionCallback(cb func(winningOutcome string, payout float64)) {
	mm.onResolution = cb
}

// SetMarketEndCallback sets callback for when market is about to end
func (mm *MarketMonitor) SetMarketEndCallback(cb func()) {
	mm.onMarketEnd = cb
}

// CheckState checks current market state based on time
func (mm *MarketMonitor) CheckState() MarketState {
	if mm.currentMarket == nil {
		return MarketStatePaused
	}

	now := time.Now()
	timeToEnd := mm.currentMarket.EndTime.Sub(now)

	// Already resolved
	if mm.currentMarket.State == MarketStateResolved {
		return MarketStateResolved
	}

	// Market has ended - wait for resolution
	if timeToEnd <= 0 {
		if mm.currentMarket.State != MarketStateResolved {
			mm.currentMarket.State = MarketStateEnding
			// Cancel all orders when market ends
			mm.prepareForResolution()
		}
		return MarketStateEnding
	}

	// Last 30 seconds - stop placing new orders (but don't trigger resolution yet)
	if timeToEnd < 30*time.Second {
		if mm.currentMarket.State == MarketStateActive {
			mm.currentMarket.State = MarketStateEnding
			// Removed direct Printf to avoid TUI interference
			if mm.onMarketEnd != nil {
				mm.onMarketEnd()
			}
		}
		return MarketStateEnding
	}

	return MarketStateActive
}

// prepareForResolution prepares for market resolution
func (mm *MarketMonitor) prepareForResolution() {
	if mm.endMessagePrinted {
		return
	}
	mm.endMessagePrinted = true

	// Cancel all open orders
	mm.ladderMgr.CancelAllLadders()
}

// SimulateResolution simulates market resolution (for paper trading)
// In real trading, this would come from the API
func (mm *MarketMonitor) SimulateResolution(winningOutcome string) {
	if mm.currentMarket == nil {
		return
	}

	mm.currentMarket.State = MarketStateResolved
	mm.currentMarket.WinningOutcome = winningOutcome

	// Redeem positions
	payout := mm.engine.Redeem(winningOutcome)

	if mm.onResolution != nil {
		mm.onResolution(winningOutcome, payout)
	}
}

// GetTimeToEnd returns time remaining until market ends
func (mm *MarketMonitor) GetTimeToEnd() time.Duration {
	if mm.currentMarket == nil {
		return 0
	}
	remaining := mm.currentMarket.EndTime.Sub(time.Now())
	if remaining < 0 {
		return 0
	}
	return remaining
}

// IsActive returns true if market is still active for trading
func (mm *MarketMonitor) IsActive() bool {
	return mm.CheckState() == MarketStateActive
}

// GetMarketInfo returns current market info
func (mm *MarketMonitor) GetMarketInfo() *MarketInfo {
	return mm.currentMarket
}

// PrintStatus prints current market status
func (mm *MarketMonitor) PrintStatus() {
	/*
		if mm.currentMarket == nil {
			fmt.Println("📊 No market loaded")
			return
		}

		remaining := mm.GetTimeToEnd()
		state := mm.CheckState()

		stateEmoji := "🟢"
		switch state {
		case MarketStateEnding:
			stateEmoji = "🟡"
		case MarketStateResolved:
			stateEmoji = "✅"
		case MarketStatePaused:
			stateEmoji = "⏸️"
		}

		fmt.Printf("%s Market: %s | Time remaining: %v | State: %s\n",
			stateEmoji, mm.currentMarket.Slug, remaining.Round(time.Second), state)
	*/
}

// ParseEndTimeFromSlug extracts end time from market slug (e.g., btc-updown-15m-1767358800)
// The slug contains the START timestamp, so we add 15 minutes to get END time
func ParseEndTimeFromSlug(slug string) (time.Time, error) {
	// The slug format ends with the window START timestamp
	// e.g., btc-updown-15m-1767358800 -> starts at 1767358800, ends at 1767358800 + 900
	var timestamp int64
	_, err := fmt.Sscanf(slug[len(slug)-10:], "%d", &timestamp)
	if err != nil {
		// Try to find timestamp in slug
		for i := len(slug) - 1; i >= 0; i-- {
			if slug[i] == '-' {
				_, err = fmt.Sscanf(slug[i+1:], "%d", &timestamp)
				if err == nil && timestamp > 1700000000 { // Reasonable unix timestamp
					break
				}
			}
		}
	}

	if timestamp == 0 {
		return time.Time{}, fmt.Errorf("could not parse timestamp from slug: %s", slug)
	}

	// Add 15 minutes (900 seconds) to get the END time
	endTimestamp := timestamp + 900
	return time.Unix(endTimestamp, 0), nil
}
