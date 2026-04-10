package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

type realbotPendingMerge struct {
	Qty       float64
	HoldUntil time.Time
}

type realbotMergeCoordinator struct {
	mu      sync.Mutex
	pending map[string]realbotPendingMerge
}

func newRealbotMergeCoordinator() *realbotMergeCoordinator {
	return &realbotMergeCoordinator{pending: make(map[string]realbotPendingMerge)}
}

func (c *realbotMergeCoordinator) reserve(marketID string, qty float64, hold time.Duration) bool {
	if c == nil || qty < minOnChainActionShares {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cur, ok := c.pending[marketID]; ok && time.Now().Before(cur.HoldUntil) {
		return false
	}
	c.pending[marketID] = realbotPendingMerge{Qty: qty, HoldUntil: time.Now().Add(hold)}
	return true
}

func (c *realbotMergeCoordinator) keepPending(marketID string, hold time.Duration) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cur, ok := c.pending[marketID]
	if !ok {
		return
	}
	until := time.Now().Add(hold)
	if until.After(cur.HoldUntil) {
		cur.HoldUntil = until
		c.pending[marketID] = cur
	}
}

func (c *realbotMergeCoordinator) clear(marketID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.pending, marketID)
	c.mu.Unlock()
}

func (c *realbotMergeCoordinator) pendingQty(marketID string) float64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cur, ok := c.pending[marketID]
	if !ok {
		return 0
	}
	if time.Now().After(cur.HoldUntil) {
		delete(c.pending, marketID)
		return 0
	}
	return cur.Qty
}

func launchBackgroundMerge(marketID, reason string, outcomes []string, conditionID string, mergeQty float64, numOutcomes int, trader *trading.RealTrader, engine *paper.Engine, splitInventory *paper.SplitInventory, tui *paper.TUI, coordinator *realbotMergeCoordinator) bool {
	if coordinator == nil || len(outcomes) != 2 || mergeQty < minOnChainActionShares {
		return false
	}
	if !coordinator.reserve(marketID, mergeQty, realbotMergeTimeout+45*time.Second) {
		return false
	}
	tui.LogEvent("[%s] 🔀 %s launching background merge for %.6f balanced shares; cleanup will not wait for confirmation", marketID, reason, mergeQty)
	go func() {
		mergeCtx, cancel := context.WithTimeout(context.Background(), realbotMergeTimeout)
		defer cancel()
		txHash, err := trader.MergeOnChain(mergeCtx, conditionID, mergeQty, numOutcomes)
		if err != nil {
			if txHash != "" && len(txHash) >= 10 && strings.Contains(strings.ToLower(err.Error()), "confirmation pending") {
				coordinator.keepPending(marketID, 45*time.Second)
				tui.LogEvent("[%s] ⚠️ %s background merge pending confirmation for %.6f shares | Tx: %s...", marketID, reason, mergeQty, txHash[:10])
				return
			}
			coordinator.clear(marketID)
			if txHash != "" && len(txHash) >= 10 {
				tui.LogEvent("[%s] ⚠️ %s background merge failed for %.6f shares: %v | Tx: %s...", marketID, reason, mergeQty, err, txHash[:10])
			} else {
				tui.LogEvent("[%s] ⚠️ %s background merge failed for %.6f shares: %v", marketID, reason, mergeQty, err)
			}
			return
		}
		coordinator.clear(marketID)
		result := engine.MergeForMarket(marketID, outcomes[0], outcomes[1], mergeQty)
		if splitInventory != nil {
			splitInventory.RecordMerge(marketID, outcomes[0], outcomes[1], mergeQty)
		}
		if txHash != "" && len(txHash) >= 10 {
			tui.LogEvent("[%s] 💰 %s merge confirmed for %.6f shares | Tx: %s...", marketID, reason, mergeQty, txHash[:10])
		} else {
			tui.LogEvent("[%s] 💰 %s merge confirmed for %.6f shares", marketID, reason, mergeQty)
		}
		if result != nil && result.PnL != 0 {
			tui.LogEvent("[%s] 💰 %s merge realized PnL: $%.2f", marketID, reason, result.PnL)
		}
	}()
	return true
}

func startupPositionsSummary(positions []trading.PositionInfo) string {
	totalShares := 0.0
	for _, pos := range positions {
		if pos.Size > 0 {
			totalShares += pos.Size
		}
	}
	return fmt.Sprintf("📊 Open positions: %d token(s), %.2f total shares", len(positions), totalShares)
}

func realbotNeutralRoundPnL(startingEquity, endingEquity, reconciliationDelta float64) float64 {
	return endingEquity - startingEquity - reconciliationDelta
}
