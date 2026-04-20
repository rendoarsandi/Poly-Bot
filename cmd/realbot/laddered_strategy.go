package main

import (
	"context"
	"math"
	"strings"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

type realbotPairSnapshotLoader func(context.Context) (float64, float64, string, error)

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

	sizingPrice := ask
	if sizingPrice <= 0 {
		sizingPrice = limitPrice
	}
	if sizingPrice <= 0 {
		return 0
	}

	requestedQty := normalizeMarketBuyShares(budget / sizingPrice)
	// We size against the expected ask price to get the true notional amount requested,
	// rather than artificially shrinking the size due to high slippage limits.
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

func realbotVerifiedLadderedBuyFill(requestedQty, optimisticQty, recoveredQty float64, recoverErr error) (filledQty float64, confirmed bool, authoritative bool) {
	if hasConfirmedExecutedQty(api.SideBuy, recoveredQty) {
		return clampRequestedExecutionQty(recoveredQty, requestedQty), true, true
	}
	if recoverErr == nil {
		return 0, false, true
	}
	optimisticQty = clampRequestedExecutionQty(optimisticQty, requestedQty)
	return optimisticQty, hasConfirmedExecutedQty(api.SideBuy, optimisticQty), false
}

func realbotResolveInitialPairSnapshot(ctx context.Context, ladderedMode bool, live0, live1 float64, loader realbotPairSnapshotLoader) (bal0, bal1 float64, source string, err error) {
	bal0, bal1, source = live0, live1, "live WS cache"
	if !ladderedMode || loader == nil {
		return bal0, bal1, source, nil
	}
	auth0, auth1, authSource, authErr := loader(ctx)
	if authErr != nil {
		return bal0, bal1, source, authErr
	}
	return auth0, auth1, authSource, nil
}

func realbotShouldRetryLadderedBuyFailure(exec directMarketExecution) bool {
	if exec.Success || exec.ExecutedQty > 0 || exec.AcknowledgedQty > 0 || exec.AcknowledgedNotional > 0 {
		return false
	}
	if exec.Result != nil {
		if strings.TrimSpace(exec.Result.OrderID) != "" || len(exec.Result.TransactionsHashes) > 0 || len(exec.Result.TradeIDs) > 0 {
			return false
		}
		switch strings.ToUpper(strings.TrimSpace(exec.Result.Status)) {
		case "KILLED", "CANCELLED", "EXPIRED", "REJECTED":
			return false
		}
	}

	combined := ""
	if exec.Err != nil {
		combined = exec.Err.Error()
	}
	if exec.Result != nil {
		combined = strings.TrimSpace(combined + " " + exec.Result.Message + " " + exec.Result.Status)
	}
	message := strings.ToLower(strings.TrimSpace(combined))
	if message == "" {
		return false
	}

	retryableFragments := []string{
		"could not run the execution",
		"matching engine restarting",
		"temporarily unavailable",
		"temporary unavailable",
		"too many requests",
		"rate limit",
		"timeout",
		"deadline exceeded",
		"connection reset",
		"connection refused",
		"broken pipe",
		"eof",
		"http 429",
		"http 500",
		"http 502",
		"http 503",
		"http 504",
		"status 429",
		"status 500",
		"status 502",
		"status 503",
		"status 504",
	}
	for _, fragment := range retryableFragments {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func realbotInitialPairSnapshot(ctx context.Context, trader *trading.RealTrader, token0, token1 string, ladderedMode bool) (bal0, bal1 float64, source string, err error) {
	if trader == nil {
		return 0, 0, "live WS cache", nil
	}
	live0 := trader.GetLivePositionSize(token0)
	live1 := trader.GetLivePositionSize(token1)

	snapshotCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()

	return realbotResolveInitialPairSnapshot(snapshotCtx, ladderedMode, live0, live1, func(loadCtx context.Context) (float64, float64, string, error) {
		if !trader.IsPaperMode() {
			trader.InvalidateCTFBalanceCache(token0, token1)
		}
		return loadPairBalancesWSFirst(loadCtx, trader, token0, token1)
	})
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

func ladderedTakerDirectionalSide(entries []realbotLadderedEntry, ask0, ask1, moveCents float64) (int, int, bool) {
	if len(entries) == 0 {
		switch {
		case ask0 > ask1+1e-9:
			return 0, 1, true
		case ask1 > ask0+1e-9:
			return 1, 1, true
		default:
			return -1, 0, false
		}
	}
	lastAsk0 := entries[len(entries)-1].ask0
	lastAsk1 := entries[len(entries)-1].ask1
	threshold := realbotLadderedMoveThreshold(moveCents)

	move0 := ask0 - lastAsk0
	move1 := ask1 - lastAsk1

	switch {
	case move0 >= threshold-1e-9 && move0 > move1+1e-9:
		return 0, 1, true
	case move1 >= threshold-1e-9 && move1 > move0+1e-9:
		return 1, 1, true
	case move0 >= threshold-1e-9 && move1 >= threshold-1e-9:
		if ask0 > ask1+1e-9 {
			return 0, 1, true
		}
		if ask1 > ask0+1e-9 {
			return 1, 1, true
		}
	}
	return -1, 0, false
}

func realbotPendingLadderedEntry(_ []realbotLadderedEntry, seq uint64, ask0, ask1, _ float64) realbotLadderedEntry {
	return realbotLadderedEntry{seq: seq, ask0: ask0, ask1: ask1}
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
