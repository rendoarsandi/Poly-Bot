package main

import (
	"math"

	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

func realbotCostBasisTotalCostForQuantity(trader *trading.RealTrader, tokenID string, desiredQty, fallbackMarkPrice float64) (float64, bool) {
	if trader == nil || desiredQty <= 1e-9 {
		return 0, false
	}
	ledgerQty, ledgerCost, avgPrice, ok := trader.GetPositionCostBasis(tokenID)
	if !ok || ledgerQty <= 1e-9 {
		return 0, false
	}
	if ledgerCost <= 0 {
		return 0, false
	}
	if desiredQty <= ledgerQty+1e-9 {
		return ledgerCost * (desiredQty / ledgerQty), true
	}

	extraQty := desiredQty - ledgerQty
	extraPrice := fallbackMarkPrice
	if extraPrice <= 0 && avgPrice > 0 {
		extraPrice = avgPrice
	}
	if extraPrice <= 0 {
		extraPrice = 0.5
	}
	return ledgerCost + (extraQty * extraPrice), true
}

func realbotSyncExternalPositionWithCostBasis(trader *trading.RealTrader, engine *paper.Engine, marketID, outcome, tokenID string, desiredQty, fallbackMarkPrice float64) bool {
	if engine == nil {
		return false
	}
	if desiredQty <= 1e-9 {
		return engine.SyncExternalPositionWithTotalCost(marketID, outcome, 0, 0)
	}
	if totalCost, ok := realbotCostBasisTotalCostForQuantity(trader, tokenID, desiredQty, fallbackMarkPrice); ok && totalCost > 0 {
		return engine.SyncExternalPositionWithTotalCost(marketID, outcome, desiredQty, math.Max(0, totalCost))
	}
	return engine.SyncExternalPosition(marketID, outcome, desiredQty, fallbackMarkPrice)
}
