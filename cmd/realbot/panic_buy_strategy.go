package main

import (
	"context"
	"math"
	"sort"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/strategy"
	"Market-bot/internal/trading"
)

type realbotPanicBuyStrategyArgs struct {
	ctx                  context.Context
	marketID             string
	market               *api.Market
	outcomes             []string
	tokenToOutcome       map[string]string
	tokenBids            map[string]float64
	tokenAsks            map[string]float64
	tokenFullBids        map[string][]paper.MarketLevel
	tokenFullAsks        map[string][]paper.MarketLevel
	quoteState           map[string]realbotQuoteState
	tokenFeeRates        map[string]int
	arbMode              string
	currentBalance       float64
	executionQuoteMaxAge time.Duration
	blockNewEntries      bool
	trader               *trading.RealTrader
	engine               *paper.Engine
	riskMgr              *paper.RiskManager
	tui                  *paper.TUI
	restClient           *api.RestClient
	cfg                  *core.Config
	splitInventory       *paper.SplitInventory
	mergeCoordinator     *realbotMergeCoordinator
	refreshWalletTruth   func(time.Duration)
	entryGate            *realbotEntryGate
	entryExecutionDone   chan<- realbotAsyncEntryResult
}

type realbotPanicBuyStrategyState struct {
	lastPairUpdate          *time.Time
	ladderedEntries         *[]realbotLadderedEntry
	nextLadderedEntrySeq    *uint64
	panicBuyCooldown        *time.Time
	lastTrade               *time.Time
	lastDustRecoveryNotice  *time.Time
	entryExecutionInFlight  *bool
	ladderedStartupStableAt *time.Time
	ladderedStartupSide     *int
	ladderedStartupRung     *int
}

const realbotLadderedStartupStability = 3 * time.Second

func realbotResetLadderedStartupStability(state *realbotPanicBuyStrategyState) {
	if state == nil {
		return
	}
	if state.ladderedStartupStableAt != nil {
		*state.ladderedStartupStableAt = time.Time{}
	}
	if state.ladderedStartupSide != nil {
		*state.ladderedStartupSide = -1
	}
	if state.ladderedStartupRung != nil {
		*state.ladderedStartupRung = -1
	}
}

func realbotLadderedHasConfirmedEntries(entries []realbotLadderedEntry) bool {
	for _, entry := range entries {
		if !entry.armed {
			return true
		}
	}
	return false
}

// realbotLadderedStartupStabilityReady is a no-op: the startup stability gate
// has been removed at the operator's request so the first live rung can fire
// immediately. The function still records the observed side/rung for downstream
// state inspection but never blocks.
func realbotLadderedStartupStabilityReady(state *realbotPanicBuyStrategyState, side, rung int, now time.Time) bool {
	if state != nil {
		if state.ladderedStartupStableAt != nil && state.ladderedStartupStableAt.IsZero() {
			*state.ladderedStartupStableAt = now
		}
		if state.ladderedStartupSide != nil {
			*state.ladderedStartupSide = side
		}
		if state.ladderedStartupRung != nil {
			*state.ladderedStartupRung = rung
		}
	}
	return true
}

func realbotPairTokenIDs(tokenToOutcome map[string]string, outcomes []string) (string, string) {
	token0, token1 := "", ""
	for tid, out := range tokenToOutcome {
		if out == outcomes[0] {
			token0 = tid
		} else if len(outcomes) > 1 && out == outcomes[1] {
			token1 = tid
		}
	}
	return token0, token1
}

func realbotHandlePanicBuyStrategy(args realbotPanicBuyStrategyArgs, state *realbotPanicBuyStrategyState) bool {
	if len(args.outcomes) != 2 || len(args.tokenAsks) < 2 {
		return false
	}

	ask1 := args.tokenAsks[args.outcomes[0]]
	ask2 := args.tokenAsks[args.outcomes[1]]
	bid1 := args.tokenBids[args.outcomes[0]]
	bid2 := args.tokenBids[args.outcomes[1]]

	realbotCfg := args.tui.GetSettings()
	rMinAsk := realbotCfg.MinAskPrice
	rMaxAsk := realbotCfg.MaxAskPrice
	ladderBasePrice := rMinAsk
	ladderedMode := args.arbMode == paperArbModeLaddered
	if ladderedMode {
		rMinAsk, rMaxAsk = ladderedTakerAskBounds(rMinAsk, rMaxAsk)
		// Pin the ladder rung-zero anchor at $0.50 for both sides instead of
		// tracking the operator's MinAskPrice. This makes the ladder symmetric
		// and predictable: rung 0 fires at <= $0.50 on either outcome, rung N
		// fires N re-entry moves above that anchor.
		ladderBasePrice = ladderedTakerBaseRungPrice
	}

	if ask1 <= bid1 || ask2 <= bid2 || (!ladderedMode && (bid1 <= 0 || bid2 <= 0)) {
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

	sum := ask1 + ask2
	observedMargin := pairMarginPercent(sum)
	executionMarginFloor := clampExecutionMarginFloor(realbotCfg.MinMarginPercent, realbotCfg.BuyExecutionMarginFloorPercent)
	executionPriceCap := normalizedRealbotExecutionPriceCap(realbotCfg)
	maxExecutionSum := maxExecutablePairSum(executionMarginFloor, executionPriceCap)
	if ladderedMode {
		maxExecutionSum = ladderedTakerMaxPairSum
	}

	entryReady := observedMargin >= realbotCfg.MinMarginPercent-1e-4
	if ladderedMode {
		entryReady = ladderedTakerEntryEligible(ask1, ask2)
	}
	if !entryReady {
		if ladderedMode {
			realbotResetLadderedStartupStability(state)
		}
		return false
	}
	if ladderedMode && state != nil && state.ladderedEntries != nil && len(*state.ladderedEntries) == 0 {
		*state.ladderedEntries = realbotArmInitialLadderedEntries(*state.ladderedEntries, ask1, ask2, ladderBasePrice, realbotCfg.LadderedTakerReentryMoveCents)
		realbotResetLadderedStartupStability(state)
		args.tui.LogEventDedup("ladder-arm:"+args.marketID, 30*time.Second,
			"[%s] 🪜 Ladder fresh market: anchored at $%.3f for both sides (live asks: %s=$%.3f, %s=$%.3f)",
			args.marketID, ladderBasePrice, args.outcomes[0], ask1, args.outcomes[1], ask2)
		return true
	}
	if ladderedMode && state != nil && state.ladderedEntries != nil {
		*state.ladderedEntries = realbotRefreshLadderedEntries(*state.ladderedEntries, ask1, ask2, ladderBasePrice, realbotCfg.LadderedTakerReentryMoveCents)
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

	tradeSize := realbotLiveTradeSize(realbotSizingCapitalForTrade(args.engine, realbotCfg), realbotCfg)

	maxFeeRateBps := 0
	if rate1 := args.tokenFeeRates[args.outcomes[0]]; rate1 > maxFeeRateBps {
		maxFeeRateBps = rate1
	}
	if rate2 := args.tokenFeeRates[args.outcomes[1]]; rate2 > maxFeeRateBps {
		maxFeeRateBps = rate2
	}

	shares := normalizeMarketBuyShares(tradeSize / sum)
	if ladderedMode {
		shares = normalizeMarketBuyShares(core.CalculateLadderedTakerSharesForMode(sum, realbotCfg.LadderedTakerSizeUSDC, realbotCfg.LadderedTakerSizeShares, realbotCfg.MaxTradeSize, realbotCfg.LadderedTakerSizingMode))
	}
	requestedShares := shares

	pairUpdatePtr := (*time.Time)(nil)
	if state != nil {
		pairUpdatePtr = state.lastPairUpdate
	}

	if !ladderedMode {
		buyQuoteCtx, cancelBuyQuote := context.WithTimeout(args.ctx, realbotExecQuoteTimeout)
		_, _, _, buyQuoteErr := realbotEnsureFreshBuyExecutionQuote(
			buyQuoteCtx,
			args.restClient,
			args.market,
			args.outcomes,
			args.tokenBids,
			args.tokenAsks,
			args.tokenFullBids,
			args.tokenFullAsks,
			args.quoteState,
			derefTime(pairUpdatePtr),
			args.executionQuoteMaxAge,
			pairUpdatePtr,
		)
		cancelBuyQuote()
		if buyQuoteErr != nil {
			args.tui.LogEvent("[%s] ⚠️ Skipping buy: fresh execution quote unavailable (%v)", args.marketID, buyQuoteErr)
			setEntryCooldown(500 * time.Millisecond)
			return true
		}
	}

	ask1 = args.tokenAsks[args.outcomes[0]]
	ask2 = args.tokenAsks[args.outcomes[1]]
	if ask1 < rMinAsk || ask1 > rMaxAsk || ask2 < rMinAsk || ask2 > rMaxAsk {
		setEntryCooldown(500 * time.Millisecond)
		return true
	}
	sum = ask1 + ask2
	observedMargin = pairMarginPercent(sum)
	if !ladderedMode && observedMargin < realbotCfg.MinMarginPercent-1e-4 {
		args.tui.LogEvent("[%s] ⚠️ Skipping buy: local pair margin %.2f%% below configured %.2f%%", args.marketID, observedMargin, realbotCfg.MinMarginPercent)
		setEntryCooldown(500 * time.Millisecond)
		return true
	}
	if ladderedMode && !ladderedTakerEntryEligible(ask1, ask2) {
		realbotResetLadderedStartupStability(state)
		setEntryCooldown(500 * time.Millisecond)
		return true
	}

	shares = normalizeMarketBuyShares(tradeSize / sum)
	if ladderedMode {
		shares = normalizeMarketBuyShares(core.CalculateLadderedTakerSharesForMode(sum, realbotCfg.LadderedTakerSizeUSDC, realbotCfg.LadderedTakerSizeShares, realbotCfg.MaxTradeSize, realbotCfg.LadderedTakerSizingMode))
	}
	requestedShares = shares

	if !ladderedMode {
		if block, reason := realbotPanicBuyCompletionGuard(args.engine, args.marketID, args.outcomes[0], args.outcomes[1], ask1, ask2, realbotCfg.MinMarginPercent); block {
			args.tui.LogEvent("[%s] ⚠️ Skipping buy: %s", args.marketID, reason)
			setEntryCooldown(500 * time.Millisecond)
			return true
		}
	}

	asks1 := append([]paper.MarketLevel(nil), args.tokenFullAsks[args.outcomes[0]]...)
	asks1 = realbotEnsureTopAskLevel(asks1, ask1, requestedShares)
	sort.Slice(asks1, func(i, j int) bool { return asks1[i].Price < asks1[j].Price })

	asks2 := append([]paper.MarketLevel(nil), args.tokenFullAsks[args.outcomes[1]]...)
	asks2 = realbotEnsureTopAskLevel(asks2, ask2, requestedShares)
	sort.Slice(asks2, func(i, j int) bool { return asks2[i].Price < asks2[j].Price })

	totalMatchedLiquidity := 0.0
	rawLiq1, rawLiq2 := 0.0, 0.0
	maxValidI, maxValidJ := 0, 0
	for i, j := 0, 0; i < len(asks1) && j < len(asks2); {
		p1 := asks1[i].Price
		p2 := asks2[j].Price
		if p1+p2 > maxExecutionSum+1e-6 {
			break
		}

		levelLiq1 := asks1[i].Size
		levelLiq2 := asks2[j].Size
		matchedAtLevel := levelLiq1
		if levelLiq2 < matchedAtLevel {
			matchedAtLevel = levelLiq2
		}

		if i+1 > maxValidI {
			maxValidI = i + 1
			rawLiq1 += asks1[i].Size
		}
		if j+1 > maxValidJ {
			maxValidJ = j + 1
			rawLiq2 += asks2[j].Size
		}
		totalMatchedLiquidity += matchedAtLevel

		remaining1 := levelLiq1 - matchedAtLevel
		remaining2 := levelLiq2 - matchedAtLevel
		if remaining1 <= 0 {
			i++
		} else {
			asks1[i].Size = remaining1
		}
		if remaining2 <= 0 {
			j++
		} else {
			asks2[j].Size = remaining2
		}
	}

	liq1 := rawLiq1
	liq2 := rawLiq2
	minLiquidity := totalMatchedLiquidity
	bookDepth1 := len(args.tokenFullAsks[args.outcomes[0]])
	bookDepth2 := len(args.tokenFullAsks[args.outcomes[1]])

	if !ladderedMode && requestedShares > minLiquidity+1e-6 {
		setEntryCooldown(500 * time.Millisecond)
		return true
	}

	minEntryShares := 1.0
	if ladderedMode {
		minEntryShares = minOnChainActionShares
	}
	if shares < minEntryShares {
		args.tui.LogEvent("[%s] ⚠️ Actionable matched liquidity below %.2f share minimum: %.4f", args.marketID, minEntryShares, shares)
		return true
	}
	if !ladderedMode && state != nil && state.lastTrade != nil && time.Since(*state.lastTrade) <= 2*time.Second {
		return true
	}

	limitPrice1 := 0.0
	limitPrice2 := 0.0
	if ladderedMode {
		limitPrice1 = realbotDirectionalBuyLimitPrice(ask1, rMaxAsk, realbotCfg.LadderedTakerMaxSlippagePct)
		limitPrice2 = realbotDirectionalBuyLimitPrice(ask2, rMaxAsk, realbotCfg.LadderedTakerMaxSlippagePct)
	} else {
		var capErr error
		limitPrice1, limitPrice2, capErr = core.BuyExecutionLimitPrices(ask1, ask2, rMinAsk, executionPriceCap, executionMarginFloor)
		if capErr != nil {
			args.tui.LogEvent("[%s] ⚠️ Skipping trade: %v", args.marketID, capErr)
			return true
		}
	}

	ladderedDirection := -1
	ladderedEntrySeq := uint64(0)
	var pendingLadderedEntry realbotLadderedEntry
	if ladderedMode {
		var directionalReady bool
		currentEntries := derefLadderedEntries(stateEntries(state))
		ladderedDirection, _, directionalReady = ladderedTakerDirectionalSide(currentEntries, ask1, ask2, ladderBasePrice, realbotCfg.LadderedTakerReentryMoveCents)
		if !directionalReady {
			if !realbotLadderedHasConfirmedEntries(currentEntries) {
				realbotResetLadderedStartupStability(state)
			}
			return true
		}
		if !realbotLadderedHasConfirmedEntries(currentEntries) {
			candidate := realbotPendingLadderedEntry(currentEntries, 0, ask1, ask2, ladderBasePrice, realbotCfg.LadderedTakerReentryMoveCents)
			if !realbotLadderedStartupStabilityReady(state, candidate.side, candidate.rung, time.Now()) {
				return true
			}
		}

		if state != nil && state.nextLadderedEntrySeq != nil {
			*state.nextLadderedEntrySeq = *state.nextLadderedEntrySeq + 1
			ladderedEntrySeq = *state.nextLadderedEntrySeq
		}
		// Reset the ladder anchor to the current live quote after each actionable re-entry
		// so large gaps do not trigger a backlog of catch-up buys at worse prices.
		pendingLadderedEntry = realbotPendingLadderedEntry(derefLadderedEntries(stateEntries(state)), ladderedEntrySeq, ask1, ask2, ladderBasePrice, realbotCfg.LadderedTakerReentryMoveCents)
	}

	requestSize1, requestSize2 := shares, shares
	if ladderedMode {
		requestSize1, requestSize2 = 0, 0
		if ladderedDirection == 1 {
			requestSize2 = realbotLadderedRequestedQty(sum, realbotCfg, ask2, limitPrice2)
		} else {
			requestSize1 = realbotLadderedRequestedQty(sum, realbotCfg, ask1, limitPrice1)
		}
		activeSize := requestSize1
		if ladderedDirection == 1 {
			activeSize = requestSize2
		}
		if activeSize < minEntryShares {
			args.tui.LogEvent("[%s] ⚠️ Actionable laddered leg below %.2f share minimum: %s", args.marketID, minEntryShares, formatShareQty(activeSize))
			return true
		}
		activePrice := limitPrice1
		if ladderedDirection == 1 {
			activePrice = limitPrice2
		}

		// Synchronize the local engine position using the authoritative pre-trade snapshot
		// before checking the inventory cap and profit floors. This prevents "false negative"
		// rejections (like an ignored API error on a previous ladder step) from causing runaway
		// buys or bad profit calculations, since the engine learns about the true on-chain
		// balance before calculating the next step.
		syncCtx, cancelSync := context.WithTimeout(context.Background(), 2*time.Second)
		tokenToOutcome := make(map[string]string)
		for _, t := range args.market.Tokens {
			if t.TokenID != "" && t.Outcome != "" {
				tokenToOutcome[t.TokenID] = t.Outcome
			}
		}
		token0, token1 := realbotPairTokenIDs(tokenToOutcome, args.outcomes)
		initial0, initial1, _, err := realbotInitialPairSnapshot(syncCtx, args.trader, token0, token1, ladderedMode)
		cancelSync()
		if err != nil {
			args.tui.LogEvent("[%s] ⚠️ Skipping ladder buy: pre-trade inventory check failed (%v)", args.marketID, err)
			setEntryCooldown(2 * time.Second)
			return true
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
			setEntryCooldown(500 * time.Millisecond)
			return true
		}
		shares = activeSize
		if ladderedDirection == 1 {
			requestSize2 = shares
		} else {
			requestSize1 = shares
		}
	} else if requestSize1 < minEntryShares || requestSize2 < minEntryShares {
		args.tui.LogEvent("[%s] ⚠️ Actionable arb legs below %.2f share minimum: %s/%s", args.marketID, minEntryShares, formatShareQty(requestSize1), formatShareQty(requestSize2))
		return true
	}

	cost := strategy.CalculateTradeMetricsFlat(shares, maxExecutionSum, maxFeeRateBps).Cost
	if ladderedMode {
		cost = requestSize1 * limitPrice1
		if ladderedDirection == 1 {
			cost = requestSize2 * limitPrice2
		}
	}
	if !args.riskMgr.CanPlaceOrder(cost) {
		args.tui.LogEvent("[%s] ⚠️ Risk limit exceeded for cost $%.2f", args.marketID, cost)
		return true
	}

	if !ladderedMode {
		budgetCappedShares := realbotClampBuySharesToBudget(shares, tradeSize, limitPrice1, limitPrice2)
		if budgetCappedShares < shares {
			args.tui.LogEvent("[%s] 📉 Downscaling from %s to %s shares to stay within $%.2f trade budget at live caps", args.marketID, formatShareQty(shares), formatShareQty(budgetCappedShares), tradeSize)
			shares = budgetCappedShares
			requestSize1 = shares
			requestSize2 = shares
		}
		if shares < minEntryShares {
			args.tui.LogEvent("[%s] ⚠️ Actionable size fell below %.2f share after cap-based budget clamp", args.marketID, minEntryShares)
			return true
		}
	}

	if ladderedMode {
		var activeLimitPrice float64
		if ladderedDirection == 1 {
			activeLimitPrice = limitPrice2
		} else {
			activeLimitPrice = limitPrice1
		}
		safeShares := realbotClampBuySharesToBudget(shares, args.currentBalance, activeLimitPrice)
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
	} else {
		safeShares := realbotClampBuySharesToBudget(shares, args.currentBalance, limitPrice1, limitPrice2)
		if safeShares < shares {
			args.tui.LogEvent("[%s] 📉 Downscaling from %s to %s shares to fit $%.2f balance limit", args.marketID, formatShareQty(shares), formatShareQty(safeShares), args.currentBalance)
			shares = safeShares
			requestSize1 = shares
			requestSize2 = shares
		}
		if shares < minEntryShares {
			if state != nil && state.lastDustRecoveryNotice != nil && time.Since(*state.lastDustRecoveryNotice) > 60*time.Second {
				args.tui.LogEvent("[%s] ⚠️ Skipping buy: capped share size no longer fits available balance", args.marketID)
				*state.lastDustRecoveryNotice = time.Now()
			}
			return true
		}
	}

	side1Requested := !ladderedMode || ladderedDirection == 0
	side2Requested := !ladderedMode || ladderedDirection == 1
	side1Req := directMarketOrderSignalRequest{
		Side:        api.SideBuy,
		Outcome:     args.outcomes[0],
		Price:       limitPrice1,
		Size:        requestSize1,
		ExactShares: true,
	}
	side2Req := directMarketOrderSignalRequest{
		Side:        api.SideBuy,
		Outcome:     args.outcomes[1],
		Price:       limitPrice2,
		Size:        requestSize2,
		ExactShares: true,
	}
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

	if !ladderedMode {
		args.tui.LogEvent("[%s] 🎯 ARB candidate %s@$%.3f→%.3f (%s sh) + %s@$%.3f→%.3f (%s sh) = $%.3f (%.1f%% observed, %.1f%% execution floor) [liq: %.0f/%.0f, levels used: %d/%d (total depth: %d/%d)]",
			args.marketID, args.outcomes[0], ask1, limitPrice1, formatShareQty(requestSize1), args.outcomes[1], ask2, limitPrice2, formatShareQty(requestSize2), sum, observedMargin, executionMarginFloor, liq1, liq2, maxValidI, maxValidJ, bookDepth1, bookDepth2)
	}

	token0, token1 := realbotPairTokenIDs(args.tokenToOutcome, args.outcomes)

	acquiredGate := false
	if !ladderedMode && args.entryGate != nil {
		if !args.entryGate.TryAcquire() {
			setEntryCooldown(500 * time.Millisecond)
			args.tui.LogEvent("[%s] ⏳ Skipping buy: another market is executing a live entry", args.marketID)
			return true
		}
		acquiredGate = true
	}

	if ladderedMode && ladderedEntrySeq != 0 && state != nil && state.ladderedEntries != nil {
		*state.ladderedEntries = realbotTrimLadderedEntries(append(*state.ladderedEntries, pendingLadderedEntry))
	}
	if state != nil && state.entryExecutionInFlight != nil {
		*state.entryExecutionInFlight = true
	}

	workerOutcomes := append([]string(nil), args.outcomes...)
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
		ladderedMode,
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
		acquiredGate,
		ladderedEntrySeq,
	)
	return true
}

func derefTime(v *time.Time) time.Time {
	if v == nil {
		return time.Time{}
	}
	return *v
}

func derefLadderedEntries(v *[]realbotLadderedEntry) []realbotLadderedEntry {
	if v == nil {
		return nil
	}
	return *v
}

func stateEntries(state *realbotPanicBuyStrategyState) *[]realbotLadderedEntry {
	if state == nil {
		return nil
	}
	return state.ladderedEntries
}
