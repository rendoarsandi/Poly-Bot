package main

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

type realbotPairSnapshotLoader func(context.Context) (float64, float64, string, error)

func realbotNormalizedLadderedUSDCBudget(liveCfg paper.TUISettings) float64 {
	budget := math.Round(liveCfg.LadderedTakerSizeUSDC*100.0) / 100.0
	if budget < realbotMinDirectOrderValue {
		budget = realbotMinDirectOrderValue
	}
	if liveCfg.MaxTradeSize > 0 {
		maxTradeSize := math.Round(liveCfg.MaxTradeSize*100.0) / 100.0
		if maxTradeSize < realbotMinDirectOrderValue {
			maxTradeSize = realbotMinDirectOrderValue
		}
		if budget > maxTradeSize {
			budget = maxTradeSize
		}
	}
	return budget
}

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

	budget := realbotNormalizedLadderedUSDCBudget(liveCfg)

	sizingPrice := ask
	if sizingPrice <= 0 {
		sizingPrice = limitPrice
	}
	if sizingPrice <= 0 {
		return 0
	}

	requestedQty := normalizeMarketBuyShares(budget / sizingPrice)
	// The venue validates buy amounts against the submitted cap, not the
	// observed ask. Clamp to the actual limit price so the order stays inside
	// budget and lands on a venue-compatible share increment.
	return realbotClampSingleBuySharesToBudget(requestedQty, budget, limitPrice)
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

func realbotLadderedLeaderSide(ask0, ask1 float64) int {
	switch {
	case ask0 > ask1+1e-9:
		return 0
	case ask1 > ask0+1e-9:
		return 1
	default:
		return -1
	}
}

func realbotLadderedBasePrice(minAsk float64) float64 {
	if minAsk < ladderedTakerMinAsk {
		return ladderedTakerMinAsk
	}
	if minAsk >= ladderedTakerMaxAsk {
		return ladderedTakerMaxAsk
	}
	return minAsk
}

func realbotLadderedRungIndex(ask, basePrice, moveCents float64) int {
	basePrice = realbotLadderedBasePrice(basePrice)
	if ask <= basePrice {
		return 0
	}
	threshold := realbotLadderedMoveThreshold(moveCents)
	if threshold <= 0 {
		return 0
	}
	rung := int(math.Floor(((ask - basePrice) / threshold) + 1e-9))
	if rung < 0 {
		return 0
	}
	return rung
}

func realbotLadderedEntrySideRung(entry realbotLadderedEntry, basePrice, moveCents float64) (int, int, bool) {
	if (entry.side == 0 || entry.side == 1) && (entry.rung > 0 || entry.armed) {
		return entry.side, entry.rung, true
	}
	side := realbotLadderedLeaderSide(entry.ask0, entry.ask1)
	if side < 0 {
		return -1, 0, false
	}
	ask := entry.ask0
	if side == 1 {
		ask = entry.ask1
	}
	return side, realbotLadderedRungIndex(ask, basePrice, moveCents), true
}

func realbotLadderedMaxRungs(entries []realbotLadderedEntry, basePrice, moveCents float64) [2]int {
	maxRungs := [2]int{-1, -1}
	for _, entry := range entries {
		side, rung, ok := realbotLadderedEntrySideRung(entry, basePrice, moveCents)
		if !ok || side < 0 || side > 1 {
			continue
		}
		if entry.armed {
			maxRungs[side] = rung
			continue
		}
		if rung > maxRungs[side] {
			maxRungs[side] = rung
		}
	}
	return maxRungs
}

func realbotArmInitialLadderedEntries(entries []realbotLadderedEntry, ask0, ask1, basePrice, moveCents float64) []realbotLadderedEntry {
	if len(entries) > 0 {
		return entries
	}
	baseRung0 := realbotLadderedRungIndex(basePrice, basePrice, moveCents)
	return append(entries,
		realbotLadderedEntry{seq: 0, ask0: ask0, ask1: ask1, side: 0, rung: baseRung0, armed: true},
		realbotLadderedEntry{seq: 0, ask0: ask0, ask1: ask1, side: 1, rung: baseRung0, armed: true},
	)
}

func realbotRefreshLadderedEntries(entries []realbotLadderedEntry, ask0, ask1, basePrice, moveCents float64) []realbotLadderedEntry {
	if len(entries) == 0 {
		return entries
	}

	currentRungs := [2]int{
		realbotLadderedRungIndex(ask0, basePrice, moveCents),
		realbotLadderedRungIndex(ask1, basePrice, moveCents),
	}
	maxRungs := realbotLadderedMaxRungs(entries, basePrice, moveCents)
	updated := entries
	for side := 0; side < len(currentRungs); side++ {
		if currentRungs[side] != 0 || maxRungs[side] <= 0 {
			continue
		}
		updated = append(updated, realbotLadderedEntry{
			seq:   0,
			ask0:  ask0,
			ask1:  ask1,
			side:  side,
			rung:  0,
			armed: true,
		})
		maxRungs[side] = 0
	}
	return updated
}

func ladderedTakerDirectionalSide(entries []realbotLadderedEntry, ask0, ask1, basePrice, moveCents float64) (int, int, bool) {
	leader := realbotLadderedLeaderSide(ask0, ask1)
	if leader < 0 {
		return -1, 0, false
	}

	leaderAsk := ask0
	if leader == 1 {
		leaderAsk = ask1
	}
	leaderRung := realbotLadderedRungIndex(leaderAsk, basePrice, moveCents)
	if len(entries) == 0 {
		return leader, 1, true
	}
	maxRungs := realbotLadderedMaxRungs(entries, basePrice, moveCents)
	if leaderRung <= maxRungs[leader] {
		return -1, 0, false
	}
	return leader, 1, true
}

func realbotPendingLadderedEntry(_ []realbotLadderedEntry, seq uint64, ask0, ask1, basePrice, moveCents float64) realbotLadderedEntry {
	side := realbotLadderedLeaderSide(ask0, ask1)
	ask := ask0
	if side == 1 {
		ask = ask1
	}
	return realbotLadderedEntry{seq: seq, ask0: ask0, ask1: ask1, side: side, rung: realbotLadderedRungIndex(ask, basePrice, moveCents)}
}

func realbotTrimLadderedEntries(entries []realbotLadderedEntry) []realbotLadderedEntry {
	const maxLadderedEntries = 256
	if len(entries) <= maxLadderedEntries {
		return entries
	}
	return append([]realbotLadderedEntry(nil), entries[len(entries)-maxLadderedEntries:]...)
}

func realbotResolveLadderedEntry(entries []realbotLadderedEntry, seq uint64, confirmed bool) []realbotLadderedEntry {
	if seq == 0 || confirmed {
		return realbotTrimLadderedEntries(entries)
	}
	for i := range entries {
		if entries[i].seq != seq {
			continue
		}
		return realbotTrimLadderedEntries(append(entries[:i], entries[i+1:]...))
	}
	return realbotTrimLadderedEntries(entries)
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
	return filledQty >= minOnChainActionShares-1e-9
}

func realbotLadderedWorstPnLFloor(projectedCost, configuredFloor float64) float64 {
	configuredFloor = math.Round(configuredFloor*100.0) / 100.0
	if math.Abs(configuredFloor) >= 0.005 {
		return configuredFloor
	}
	return -math.Max(projectedCost*2.0, minOnChainActionShares)
}

func realbotLadderedMaxProfitPnLCap(configuredCap float64) float64 {
	configuredCap = math.Round(configuredCap*100.0) / 100.0
	if math.Abs(configuredCap) < 0.005 {
		return 0
	}
	if configuredCap < 0 {
		return 0
	}
	return configuredCap
}

func realbotFormatSignedUSD(v float64) string {
	if v < 0 {
		return fmt.Sprintf("-$%.2f", math.Abs(v))
	}
	return fmt.Sprintf("$%.2f", v)
}

func realbotLadderedInventoryCapReached(engine *paper.Engine, marketID string, outcomes []string, side int, requestedQty, price float64, guardMode string, configuredWorstPnLFloor, configuredMaxProfitPnL float64) (bool, string) {
	if engine == nil || len(outcomes) != 2 || side < 0 || side > 1 || requestedQty <= 0 || price <= 0 {
		return false, ""
	}

	positions := engine.GetPositions()
	qtyByOutcome := map[string]float64{
		outcomes[0]: 0,
		outcomes[1]: 0,
	}
	costByOutcome := map[string]float64{
		outcomes[0]: 0,
		outcomes[1]: 0,
	}
	for _, pos := range positions {
		if pos.MarketID != marketID || pos.Quantity <= 0 {
			continue
		}
		if _, ok := qtyByOutcome[pos.Outcome]; !ok {
			continue
		}
		qtyByOutcome[pos.Outcome] += pos.Quantity
		costByOutcome[pos.Outcome] += pos.TotalCost
	}

	activeOutcome := outcomes[side]
	projectedCost := requestedQty * price
	qtyByOutcome[activeOutcome] += requestedQty
	costByOutcome[activeOutcome] += projectedCost

	totalCost := costByOutcome[outcomes[0]] + costByOutcome[outcomes[1]]
	resolvePnL0 := qtyByOutcome[outcomes[0]] - totalCost
	resolvePnL1 := qtyByOutcome[outcomes[1]] - totalCost
	worstOutcome := outcomes[0]
	worstResolvePnL := resolvePnL0
	bestOutcome := outcomes[1]
	bestResolvePnL := resolvePnL1
	activeResolvePnL := resolvePnL0
	if side == 1 {
		activeResolvePnL = resolvePnL1
	}
	if resolvePnL1 < resolvePnL0 {
		worstOutcome = outcomes[1]
		worstResolvePnL = resolvePnL1
		bestOutcome = outcomes[0]
		bestResolvePnL = resolvePnL0
	}

	if strings.EqualFold(strings.TrimSpace(guardMode), core.LadderedTakerPnLGuardMaxProfit) {
		maxProfitCap := realbotLadderedMaxProfitPnLCap(configuredMaxProfitPnL)

		if maxProfitCap > 0 && activeResolvePnL > maxProfitCap+1e-9 {
			return true, fmt.Sprintf("projected active-side resolve PnL would rise to %s for %s above cap %s",
				realbotFormatSignedUSD(activeResolvePnL),
				activeOutcome,
				realbotFormatSignedUSD(maxProfitCap),
			)
		}
		return false, ""
	}

	worstPnLFloor := realbotLadderedWorstPnLFloor(projectedCost, configuredWorstPnLFloor)
	if worstResolvePnL >= worstPnLFloor-1e-9 {
		return false, ""
	}

	return true, fmt.Sprintf("projected worst-case resolve PnL would fall to %s if %s wins (best %s=%s, floor %s)",
		realbotFormatSignedUSD(worstResolvePnL),
		worstOutcome,
		bestOutcome,
		realbotFormatSignedUSD(bestResolvePnL),
		realbotFormatSignedUSD(worstPnLFloor),
	)
}
