package main

import (
	"context"
	"fmt"
	"math"
	"sort"
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

func realbotLadderedBasePrice(basePrice float64) float64 {
	if basePrice <= 0 || basePrice < ladderedTakerMinAsk {
		return ladderedTakerMinAsk
	}
	if basePrice > ladderedTakerMaxAsk {
		return ladderedTakerMaxAsk
	}
	return basePrice
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

// realbotLadderedWorstPnLFloor returns the worst-case resolve PnL floor used
// to gate new ladder rungs. A configured value of 0 (within rounding) means
// the safety guard is DISABLED and 0 is returned to signal the caller to skip
// the check entirely. Any non-zero configured value is normalized to its
// negative form and returned as-is.
func realbotLadderedWorstPnLFloor(_ float64, configuredFloor float64) float64 {
	configuredFloor = math.Round(configuredFloor*100.0) / 100.0
	if math.Abs(configuredFloor) < 0.005 {
		return 0
	}
	if configuredFloor > 0 {
		configuredFloor = -configuredFloor
	}
	return configuredFloor
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
	// A floor of 0 means the operator disabled the safety guard entirely.
	if worstPnLFloor == 0 {
		return false, ""
	}
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

func realbotLadderedClampQtyForGuard(engine *paper.Engine, marketID string, outcomes []string, side int, requestedQty, price float64, guardMode string, configuredWorstPnLFloor, configuredMaxProfitPnL float64) float64 {
	if engine == nil || len(outcomes) != 2 || side < 0 || side > 1 || requestedQty <= 0 || price <= 0 {
		return requestedQty
	}
	if !strings.EqualFold(strings.TrimSpace(guardMode), core.LadderedTakerPnLGuardMaxProfit) {
		return requestedQty
	}
	maxProfitCap := realbotLadderedMaxProfitPnLCap(configuredMaxProfitPnL)
	if maxProfitCap <= 0 {
		return requestedQty
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
	totalCost := costByOutcome[outcomes[0]] + costByOutcome[outcomes[1]]
	currentResolvePnL := qtyByOutcome[activeOutcome] - totalCost

	room := maxProfitCap - currentResolvePnL
	if room <= 0 {
		return 0 // No room left
	}
	// We want: currentResolvePnL + qty * (1 - price) <= maxProfitCap
	allowedQty := room / (1.0 - price)
	if allowedQty < requestedQty {
		return allowedQty
	}
	return requestedQty
}

func realbotRefreshLadderedPreTradeQuote(args realbotPanicBuyStrategyArgs, state *realbotPanicBuyStrategyState, setCooldown func(time.Duration)) bool {
	pairUpdatePtr := (*time.Time)(nil)
	lastPairUpdate := time.Time{}
	if state != nil && state.lastPairUpdate != nil {
		pairUpdatePtr = state.lastPairUpdate
		lastPairUpdate = *state.lastPairUpdate
	}

	if args.restClient == nil || args.market == nil {
		maxAge := realbotExecutionQuoteGuardAge(args.executionQuoteMaxAge)
		fresh, _, reason := realbotCanUseLocalBuyQuote(time.Now(), args.outcomes, args.tokenBids, args.tokenAsks, args.tokenFullAsks, lastPairUpdate, maxAge)
		if fresh {
			return true
		}
		if args.tui != nil {
			args.tui.LogEvent("[%s] ⚠️ Skipping ladder buy: fresh execution quote unavailable (%s)", args.marketID, reason)
		}
		setCooldown(500 * time.Millisecond)
		return false
	}

	ctx := args.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	quoteCtx, cancelQuote := context.WithTimeout(ctx, realbotExecQuoteTimeout)
	defer cancelQuote()
	if _, err := realbotRefreshExecutionBooks(
		quoteCtx,
		args.restClient,
		args.market,
		args.outcomes,
		args.tokenBids,
		args.tokenAsks,
		args.tokenFullBids,
		args.tokenFullAsks,
		args.quoteState,
		pairUpdatePtr,
	); err != nil {
		if args.tui != nil {
			args.tui.LogEvent("[%s] ⚠️ Skipping ladder buy: REST quote confirmation failed (%v)", args.marketID, err)
		}
		setCooldown(500 * time.Millisecond)
		return false
	}
	return true
}

// realbotHandleLadderedStrategy executes the laddered taker entry path. It is
// intentionally separate from realbotHandlePanicBuyStrategy so the two
// strategies do not share branching logic and can evolve independently.
func realbotHandleLadderedStrategy(args realbotPanicBuyStrategyArgs, state *realbotPanicBuyStrategyState) bool {
	if len(args.outcomes) != 2 || len(args.tokenAsks) < 2 {
		return false
	}

	ask1 := args.tokenAsks[args.outcomes[0]]
	ask2 := args.tokenAsks[args.outcomes[1]]
	bid1 := args.tokenBids[args.outcomes[0]]
	bid2 := args.tokenBids[args.outcomes[1]]

	realbotCfg := args.tui.GetSettings()
	rMinAsk, rMaxAsk := ladderedTakerAskBounds(realbotCfg.MinAskPrice, realbotCfg.MaxAskPrice)
	ladderBasePrice := rMinAsk
	moveCents := realbotCfg.LadderedTakerReentryMoveCents

	if ask1 <= bid1 || ask2 <= bid2 {
		return true
	}

	setEntryCooldown := func(d time.Duration) {
		if state == nil || state.panicBuyCooldown == nil {
			return
		}
		*state.panicBuyCooldown = time.Now().Add(d)
	}

	if state != nil && state.panicBuyCooldown != nil && time.Now().Before(*state.panicBuyCooldown) {
		return true
	}

	if ask1 < rMinAsk || ask1 > rMaxAsk || ask2 < rMinAsk || ask2 > rMaxAsk {
		return false
	}

	if !ladderedTakerEntryEligible(ask1, ask2) {
		realbotResetLadderedStartupStability(state)
		return false
	}

	if state != nil && state.ladderedEntries != nil && len(*state.ladderedEntries) == 0 {
		*state.ladderedEntries = realbotArmInitialLadderedEntries(*state.ladderedEntries, ask1, ask2, ladderBasePrice, moveCents)
		realbotResetLadderedStartupStability(state)
		args.tui.LogEventDedup("ladder-arm:"+args.marketID, 30*time.Second,
			"[%s] 🪜 Ladder fresh market: anchored at $%.3f for both sides (live asks: %s=$%.3f, %s=$%.3f)",
			args.marketID, ladderBasePrice, args.outcomes[0], ask1, args.outcomes[1], ask2)
		return true
	}
	if state != nil && state.ladderedEntries != nil {
		*state.ladderedEntries = realbotRefreshLadderedEntries(*state.ladderedEntries, ask1, ask2, ladderBasePrice, moveCents)
	}

	if state != nil && state.entryExecutionInFlight != nil && *state.entryExecutionInFlight {
		return true
	}
	if args.blockNewEntries {
		setEntryCooldown(500 * time.Millisecond)
		return true
	}

	riskAction, riskReason := args.riskMgr.Evaluate()
	if riskAction == paper.RiskActionKillSwitch {
		args.tui.LogEvent("[%s] 🛑 RISK: Kill switch - %s (pausing, not exiting)", args.marketID, riskReason)
		return true
	}

	if !realbotRefreshLadderedPreTradeQuote(args, state, setEntryCooldown) {
		return true
	}

	// Re-read confirmed quotes in case any blocking work above let them drift.
	ask1 = args.tokenAsks[args.outcomes[0]]
	ask2 = args.tokenAsks[args.outcomes[1]]
	bid1 = args.tokenBids[args.outcomes[0]]
	bid2 = args.tokenBids[args.outcomes[1]]
	if ask1 <= bid1 || ask2 <= bid2 {
		setEntryCooldown(500 * time.Millisecond)
		return true
	}
	if ask1 < rMinAsk || ask1 > rMaxAsk || ask2 < rMinAsk || ask2 > rMaxAsk {
		setEntryCooldown(500 * time.Millisecond)
		return true
	}
	if !ladderedTakerEntryEligible(ask1, ask2) {
		realbotResetLadderedStartupStability(state)
		setEntryCooldown(500 * time.Millisecond)
		return true
	}
	sum := ask1 + ask2

	shares := normalizeMarketBuyShares(core.CalculateLadderedTakerSharesForMode(sum, realbotCfg.LadderedTakerSizeUSDC, realbotCfg.LadderedTakerSizeShares, realbotCfg.MaxTradeSize, realbotCfg.LadderedTakerSizingMode))
	requestedShares := shares

	asks1 := append([]paper.MarketLevel(nil), args.tokenFullAsks[args.outcomes[0]]...)
	asks1 = realbotEnsureTopAskLevel(asks1, ask1, requestedShares)
	sort.Slice(asks1, func(i, j int) bool { return asks1[i].Price < asks1[j].Price })

	asks2 := append([]paper.MarketLevel(nil), args.tokenFullAsks[args.outcomes[1]]...)
	asks2 = realbotEnsureTopAskLevel(asks2, ask2, requestedShares)
	sort.Slice(asks2, func(i, j int) bool { return asks2[i].Price < asks2[j].Price })

	minEntryShares := minOnChainActionShares
	if shares < minEntryShares {
		args.tui.LogEvent("[%s] ⚠️ Actionable laddered size below %.2f share minimum: %.4f", args.marketID, minEntryShares, shares)
		return true
	}

	limitPrice1 := realbotDirectionalBuyLimitPrice(ask1, rMaxAsk, realbotCfg.LadderedTakerMaxSlippagePct)
	limitPrice2 := realbotDirectionalBuyLimitPrice(ask2, rMaxAsk, realbotCfg.LadderedTakerMaxSlippagePct)

	currentEntries := derefLadderedEntries(stateEntries(state))
	ladderedDirection, _, directionalReady := ladderedTakerDirectionalSide(currentEntries, ask1, ask2, ladderBasePrice, moveCents)
	if !directionalReady {
		if !realbotLadderedHasConfirmedEntries(currentEntries) {
			realbotResetLadderedStartupStability(state)
		}
		return true
	}
	if !realbotLadderedHasConfirmedEntries(currentEntries) {
		candidate := realbotPendingLadderedEntry(currentEntries, 0, ask1, ask2, ladderBasePrice, moveCents)
		if !realbotLadderedStartupStabilityReady(state, candidate.side, candidate.rung, time.Now()) {
			return true
		}
	}

	var ladderedEntrySeq uint64
	if state != nil && state.nextLadderedEntrySeq != nil {
		*state.nextLadderedEntrySeq = *state.nextLadderedEntrySeq + 1
		ladderedEntrySeq = *state.nextLadderedEntrySeq
	}
	pendingLadderedEntry := realbotPendingLadderedEntry(derefLadderedEntries(stateEntries(state)), ladderedEntrySeq, ask1, ask2, ladderBasePrice, moveCents)

	requestSize1, requestSize2 := 0.0, 0.0
	if ladderedDirection == 1 {
		requestSize2 = realbotLadderedRequestedQty(sum, realbotCfg, ask2, limitPrice2)
	} else {
		requestSize1 = realbotLadderedRequestedQty(sum, realbotCfg, ask1, limitPrice1)
	}
	activeSize := requestSize1
	activePrice := limitPrice1
	if ladderedDirection == 1 {
		activeSize = requestSize2
		activePrice = limitPrice2
	}

	activeSize = realbotLadderedClampQtyForGuard(
		args.engine,
		args.marketID,
		args.outcomes,
		ladderedDirection,
		activeSize,
		activePrice,
		realbotCfg.LadderedTakerPnLGuardMode,
		realbotCfg.LadderedTakerWorstPnLFloor,
		realbotCfg.LadderedTakerMaxProfitPnL,
	)

	if activeSize < minEntryShares {
		if activeSize > 0 {
			args.tui.LogEventDedup("ladder-min-size:"+args.marketID, 60*time.Second,
				"[%s] ⚠️ Actionable laddered leg below %.2f share minimum: %s", args.marketID, minEntryShares, formatShareQty(activeSize))
		}
		return true
	}

	if !realbotLadderedSyncPreTradeInventory(args, ladderedDirection, activeSize, activePrice, realbotCfg, setEntryCooldown) {
		return true
	}
	shares = activeSize
	if ladderedDirection == 1 {
		requestSize2 = shares
	} else {
		requestSize1 = shares
	}

	cost := requestSize1 * limitPrice1
	if ladderedDirection == 1 {
		cost = requestSize2 * limitPrice2
	}
	if !args.riskMgr.CanPlaceOrder(cost) {
		args.tui.LogEvent("[%s] ⚠️ Risk limit exceeded for cost $%.2f", args.marketID, cost)
		return true
	}

	safeShares := realbotClampBuySharesToBudget(shares, args.currentBalance, activePrice)
	if safeShares < shares {
		args.tui.LogEvent("[%s] 📉 Downscaling ladder chunk from %s to %s shares to fit $%.2f balance limit", args.marketID, formatShareQty(shares), formatShareQty(safeShares), args.currentBalance)
		shares = safeShares
		if ladderedDirection == 1 {
			requestSize2 = shares
		} else {
			requestSize1 = shares
		}
	}
	if shares < minEntryShares {
		if state != nil && state.lastDustRecoveryNotice != nil && time.Since(*state.lastDustRecoveryNotice) > 60*time.Second {
			args.tui.LogEvent("[%s] ⚠️ Skipping buy: ladder chunk no longer fits available balance", args.marketID)
			*state.lastDustRecoveryNotice = time.Now()
		}
		return true
	}

	side1Requested := ladderedDirection == 0
	side2Requested := ladderedDirection == 1
	side1Req := directMarketOrderSignalRequest{Side: api.SideBuy, Outcome: args.outcomes[0], Price: limitPrice1, Size: requestSize1, ExactShares: true}
	side2Req := directMarketOrderSignalRequest{Side: api.SideBuy, Outcome: args.outcomes[1], Price: limitPrice2, Size: requestSize2, ExactShares: true}
	if side1Requested && !hasActionableSubmittedDirectOrderValue(side1Req) {
		args.tui.LogEvent("[%s] ⚠️ Skipping buy: %s leg submitted size is below Polymarket $%.2f minimum (%s)",
			args.marketID, args.outcomes[0], realbotMinDirectOrderValue, directSubmittedOrderSummary(side1Req))
		return true
	}
	if side2Requested && !hasActionableSubmittedDirectOrderValue(side2Req) {
		args.tui.LogEvent("[%s] ⚠️ Skipping buy: %s leg submitted size is below Polymarket $%.2f minimum (%s)",
			args.marketID, args.outcomes[1], realbotMinDirectOrderValue, directSubmittedOrderSummary(side2Req))
		return true
	}

	token0, token1 := realbotPairTokenIDs(args.tokenToOutcome, args.outcomes)

	if ladderedEntrySeq != 0 && state != nil && state.ladderedEntries != nil {
		*state.ladderedEntries = realbotTrimLadderedEntries(append(*state.ladderedEntries, pendingLadderedEntry))
	}
	if state != nil && state.entryExecutionInFlight != nil {
		*state.entryExecutionInFlight = true
	}

	workerOutcomes := append([]string(nil), args.outcomes...)
	observedMargin := pairMarginPercent(sum)
	go realbotExecuteAggressiveEntry(
		args.ctx,
		args.marketID,
		args.market,
		workerOutcomes,
		ask1,
		ask2,
		requestSize1,
		requestSize2,
		limitPrice1,
		limitPrice2,
		observedMargin,
		true,
		ladderedDirection,
		token0,
		token1,
		side1Requested,
		side2Requested,
		args.tokenFeeRates,
		args.trader,
		args.engine,
		args.tui,
		args.cfg,
		realbotCfg,
		rMinAsk,
		args.splitInventory,
		args.restClient,
		args.mergeCoordinator,
		args.refreshWalletTruth,
		args.entryGate,
		args.entryExecutionDone,
		false,
		ladderedEntrySeq,
	)
	return true
}

// realbotLadderedSyncPreTradeInventory pulls the authoritative pair snapshot,
// reconciles the local engine to it, and then runs the inventory cap guard.
// Returns true when the caller should keep going, false when it should yield
// the iteration (handler returns true after this call). setCooldown is invoked
// with the desired cooldown when the function rejects the trade.
func realbotLadderedSyncPreTradeInventory(args realbotPanicBuyStrategyArgs, ladderedDirection int, activeSize, activePrice float64, realbotCfg paper.TUISettings, setCooldown func(time.Duration)) bool {
	syncCtx, cancelSync := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelSync()
	tokenToOutcome := make(map[string]string)
	if args.market != nil {
		for _, t := range args.market.Tokens {
			if t.TokenID != "" && t.Outcome != "" {
				tokenToOutcome[t.TokenID] = t.Outcome
			}
		}
	}
	token0, token1 := realbotPairTokenIDs(tokenToOutcome, args.outcomes)
	initial0, initial1, _, err := realbotInitialPairSnapshot(syncCtx, args.trader, token0, token1, true)
	if err != nil {
		args.tui.LogEvent("[%s] ⚠️ Skipping ladder buy: pre-trade inventory check failed (%v)", args.marketID, err)
		setCooldown(2 * time.Second)
		return false
	}

	local0, local1 := localBoughtPairBalances(args.engine, args.marketID, args.outcomes[0], args.outcomes[1])
	split0, split1 := 0.0, 0.0
	if args.splitInventory != nil {
		split0 = args.splitInventory.GetSplitShares(args.marketID, args.outcomes[0])
		split1 = args.splitInventory.GetSplitShares(args.marketID, args.outcomes[1])
	}
	desired0 := math.Max(0, initial0-split0)
	desired1 := math.Max(0, initial1-split1)

	if math.Abs(local0-desired0) > 1e-4 {
		markPrice0 := walletTruthSyncMarkPrice(args.engine, args.marketID, args.outcomes[0])
		if realbotSyncExternalPositionWithCostBasis(args.trader, args.engine, args.marketID, args.outcomes[0], token0, desired0, markPrice0) {
			realbotRecordWalletTruthAdjustment(args.tui, args.marketID, args.outcomes[0], desired0-local0, local0, initial0, split0, markPrice0, "restored")
		}
	}
	if math.Abs(local1-desired1) > 1e-4 {
		markPrice1 := walletTruthSyncMarkPrice(args.engine, args.marketID, args.outcomes[1])
		if realbotSyncExternalPositionWithCostBasis(args.trader, args.engine, args.marketID, args.outcomes[1], token1, desired1, markPrice1) {
			realbotRecordWalletTruthAdjustment(args.tui, args.marketID, args.outcomes[1], desired1-local1, local1, initial1, split1, markPrice1, "restored")
		}
	}

	if blocked, _ := realbotLadderedInventoryCapReached(
		args.engine,
		args.marketID,
		args.outcomes,
		ladderedDirection,
		activeSize,
		activePrice,
		realbotCfg.LadderedTakerPnLGuardMode,
		realbotCfg.LadderedTakerWorstPnLFloor,
		realbotCfg.LadderedTakerMaxProfitPnL,
	); blocked {
		setCooldown(500 * time.Millisecond)
		return false
	}
	return true
}
