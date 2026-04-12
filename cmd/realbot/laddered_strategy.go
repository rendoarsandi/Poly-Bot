package main

import (
	"context"
	"math"
	"strings"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

func realbotLadderedRequestedQty(pairSum float64, liveCfg paper.TUISettings, ask, limitPrice float64) float64 {
	if strings.EqualFold(strings.TrimSpace(liveCfg.LadderedTakerSizingMode), core.LadderedTakerSizingModeShares) {
		return normalizeMarketBuyShares(core.CalculateLadderedTakerSharesForMode(
			pairSum,
			liveCfg.LadderedTakerSizeUSDC,
			liveCfg.LadderedTakerSizeShares,
			liveCfg.MaxTradeSize,
			liveCfg.LadderedTakerSizingMode,
		))
	}

	budget := liveCfg.LadderedTakerSizeUSDC
	if budget <= 0 {
		budget = 0.1
	}
	if liveCfg.MaxTradeSize > 0 && budget > liveCfg.MaxTradeSize {
		budget = liveCfg.MaxTradeSize
	}

	sizingPrice := limitPrice
	if sizingPrice <= 0 {
		sizingPrice = ask
	}
	if sizingPrice <= 0 {
		return 0
	}

	requestedQty := normalizeMarketBuyShares(budget / sizingPrice)
	return realbotClampSingleBuySharesToBudget(requestedQty, budget, sizingPrice)
}

func realbotLadderedRecoveredFillQty(ladderedDirection int, requestedQty, acquired0, acquired1 float64) float64 {
	recoveredQty := acquired0
	if ladderedDirection == 1 {
		recoveredQty = acquired1
	}
	return clampRequestedExecutionQty(recoveredQty, requestedQty)
}

func realbotRecoverLateLadderedBuyFill(ctx context.Context, trader *trading.RealTrader, token0, token1 string, initial0, initial1 float64, ladderedDirection int, requestedQty float64) (float64, string, error) {
	acquired0, acquired1, _, _, source, err := reconcileBoughtPairBalances(ctx, trader, token0, token1, initial0, initial1, true)
	recoveredQty := realbotLadderedRecoveredFillQty(ladderedDirection, requestedQty, acquired0, acquired1)
	if hasConfirmedExecutedQty(api.SideBuy, recoveredQty) {
		return recoveredQty, source, nil
	}
	if err != nil {
		return 0, source, err
	}
	return 0, source, nil
}

func ladderedTakerAskBounds(minAsk, maxAsk float64) (float64, float64) {
	if maxAsk > ladderedTakerMaxAsk || maxAsk <= 0 {
		maxAsk = ladderedTakerMaxAsk
	}
	if minAsk < ladderedTakerMinAsk {
		minAsk = ladderedTakerMinAsk
	}
	if minAsk > maxAsk {
		minAsk = maxAsk
	}
	return minAsk, maxAsk
}

func ladderedTakerEntryEligible(ask0, ask1 float64) bool {
	sum := ask0 + ask1
	if sum <= 0 || sum > ladderedTakerMaxPairSum+1e-9 {
		return false
	}
	if math.Abs(ask0-ask1) < ladderedTakerMinSkew-1e-9 {
		return false
	}
	return true
}

func ladderedTakerDirectionalSide(entries []realbotLadderedEntry, ask0, ask1, moveCents float64) (int, bool) {
	if len(entries) == 0 {
		switch {
		case ask0 > ask1+1e-9:
			return 0, true
		case ask1 > ask0+1e-9:
			return 1, true
		default:
			return -1, false
		}
	}
	lastAsk0 := entries[len(entries)-1].ask0
	lastAsk1 := entries[len(entries)-1].ask1
	threshold := realbotLadderedMoveThreshold(moveCents)

	move0 := ask0 - lastAsk0
	move1 := ask1 - lastAsk1
	switch {
	case move0 >= threshold-1e-9 && move0 > move1+1e-9:
		return 0, true
	case move1 >= threshold-1e-9 && move1 > move0+1e-9:
		return 1, true
	case move0 >= threshold-1e-9 && move1 >= threshold-1e-9:
		if ask0 > ask1+1e-9 {
			return 0, true
		}
		if ask1 > ask0+1e-9 {
			return 1, true
		}
	}
	return -1, false
}

func realbotPendingLadderedEntry(entries []realbotLadderedEntry, seq uint64, ask0, ask1, moveCents float64) realbotLadderedEntry {
	pendingAsk0 := ask0
	pendingAsk1 := ask1
	if len(entries) > 0 {
		last := entries[len(entries)-1]
		threshold := realbotLadderedMoveThreshold(moveCents)
		if ask0 > last.ask0+threshold {
			pendingAsk0 = last.ask0 + threshold
		}
		if ask1 > last.ask1+threshold {
			pendingAsk1 = last.ask1 + threshold
		}
	}
	return realbotLadderedEntry{seq: seq, ask0: pendingAsk0, ask1: pendingAsk1}
}

func realbotResolveLadderedEntry(entries []realbotLadderedEntry, seq uint64, confirmed bool) []realbotLadderedEntry {
	if seq == 0 || confirmed {
		return entries
	}
	for i := range entries {
		if entries[i].seq != seq {
			continue
		}
		return append(entries[:i], entries[i+1:]...)
	}
	return entries
}

func realbotLadderedMoveThreshold(moveCents float64) float64 {
	threshold := moveCents
	switch {
	case threshold <= 0:
		threshold = 1.0
	case threshold < 1.0:
		threshold = 1.0
	case threshold > 25.0:
		threshold = 25.0
	}
	return threshold / 100.0
}

func realbotShouldAdvanceLadderedEntry(requestedQty, filledQty float64) bool {
	if requestedQty <= 0 {
		return false
	}
	// Keep ladder anchor behavior aligned with paperbot: any actionable fill should
	// update the last-entry anchor so re-entry direction uses the most recent market state.
	return filledQty >= minOnChainActionShares-1e-9
}
