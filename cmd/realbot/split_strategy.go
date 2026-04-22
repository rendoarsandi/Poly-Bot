package main

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

type realbotSplitStrategyArgs struct {
	ctx                  context.Context
	marketID             string
	conditionID          string
	outcomes             []string
	tokenBids            map[string]float64
	tokenAsks            map[string]float64
	tokenFullBids        map[string][]paper.MarketLevel
	tokenFeeRates        map[string]int
	lastPairUpdate       time.Time
	currentBalance       float64
	executionQuoteMaxAge time.Duration
	liveCfg              paper.TUISettings
	cfg                  *core.Config
	trader               *trading.RealTrader
	engine               *paper.Engine
	tui                  *paper.TUI
	embeddedPaperMode    bool
	splitInventory       *paper.SplitInventory
	splitMu              *sync.Mutex
	splitTxMu            *sync.Mutex
	globalSplitStatus    map[string]bool
	globalInitialSplits  map[string]float64
	replenishCtrl        *paper.ReplenishController
	getTokenID           func(string) string
	refreshWalletTruth   func(time.Duration)
	blockNewEntries      bool
}

type realbotSplitStrategyState struct {
	nextSplitAttempt *time.Time
	lastSplitSell    *time.Time
}

func realbotApplySplitSellAccounting(engine *paper.Engine, splitInventory *paper.SplitInventory, marketID, outcome string, soldQty, price, proceeds float64, kalshiHoldMode bool) float64 {
	if soldQty <= 0 {
		return 0
	}

	profit := 0.0
	if kalshiHoldMode {
		profit = (price - 0.5) * soldQty
	} else if splitInventory != nil {
		profit = splitInventory.RecordSell(marketID, outcome, soldQty, price)
	}

	if engine != nil {
		if proceeds > 0 {
			engine.AddBalance(proceeds)
		}
		if math.Abs(profit) >= 0.000001 {
			engine.AddRealizedPnL(profit)
		}
	}

	return profit
}

func realbotHandleSplitStrategy(args realbotSplitStrategyArgs, state *realbotSplitStrategyState) bool {
	// SPLIT STRATEGY: Sell to panic buyers when bid_sum > $1.03.
	// This is separate from the panic-buy path; split inventory is only for sells.
	kalshiHoldMode := args.liveCfg.Exchange == "kalshi"
	if args.embeddedPaperMode && args.liveCfg.SplitStrategyEnabled {
		args.tui.LogEventDedup("paper-split-disabled:"+args.marketID, 30*time.Second,
			"[%s] ⚠️ Split strategy is disabled on the embedded paper backend.", args.marketID)
		return false
	}

	if args.embeddedPaperMode || (!args.liveCfg.SplitStrategyEnabled && !kalshiHoldMode) || len(args.tokenBids) < 2 || len(args.outcomes) != 2 {
		return false
	}

	bid1 := args.tokenBids[args.outcomes[0]]
	bid2 := args.tokenBids[args.outcomes[1]]

	// Initial split: create inventory if not done yet. Move to background to avoid
	// blocking the main trading loop.
	args.splitMu.Lock()
	isSplit := args.globalSplitStatus[args.conditionID]
	shouldSplit := !isSplit && time.Now().After(*state.nextSplitAttempt)
	if args.blockNewEntries {
		shouldSplit = false
	}
	if shouldSplit {
		if kalshiHoldMode {
			shouldSplit = false
		} else {
			// Optimistically mark as split to prevent concurrent duplicate attempts.
			args.globalSplitStatus[args.conditionID] = true
		}
	}
	args.splitMu.Unlock()

	if shouldSplit && args.replenishCtrl.MarkInProgress() {
		baseTradeSize := args.cfg.CalculateTradeSize(realbotSizingCapitalForTrade(args.engine, args.liveCfg))

		// Scale initial buffer based on balance: 2x trade size, but at least $2 and at most 25% of balance.
		initialBuffer := baseTradeSize * 2.0
		if initialBuffer < 2.0 {
			initialBuffer = 2.0
		}

		maxInitial := args.currentBalance * args.cfg.SplitInitialCapPct
		splitAmount := initialBuffer
		if splitAmount > maxInitial {
			splitAmount = maxInitial
		}

		// Lower threshold to $1.0 to support testing with small balances.
		if splitAmount >= 1.0 {
			args.tui.LogEvent("[%s] 🔀 SPLIT: Creating inventory ($%.2f) in background...", args.marketID, splitAmount)

			go func(marketID, conditionID, out0, out1 string, amount float64) {
				defer args.replenishCtrl.MarkComplete()
				splitCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer cancel()

				args.splitTxMu.Lock()
				txHash, err := args.trader.SplitOnChain(splitCtx, conditionID, amount, len(args.outcomes))
				args.splitTxMu.Unlock()

				args.splitMu.Lock()
				defer args.splitMu.Unlock()

				if err != nil {
					args.tui.LogEvent("[%s] ⚠️ SPLIT: Background initial split failed: %v (will retry in 15s)", marketID, err)
					*state.nextSplitAttempt = time.Now().Add(15 * time.Second)
					args.globalSplitStatus[conditionID] = false
					return
				}

				args.splitInventory.RecordSplit(marketID, out0, out1, amount)
				args.engine.DeductBalance(amount)
				args.engine.RecalculateDrawdown()

				if txHash != "" && len(txHash) >= 10 {
					args.tui.LogEvent("[%s] ✅ SPLIT: Created %.0f shares | Tx: %s...", marketID, amount, txHash[:10])
				} else {
					args.tui.LogEvent("[%s] ✅ SPLIT: Created %.0f shares", marketID, amount)
				}

				args.globalSplitStatus[conditionID] = true
				args.globalInitialSplits[conditionID] = amount
			}(args.marketID, args.conditionID, args.outcomes[0], args.outcomes[1], splitAmount)
		} else {
			args.replenishCtrl.MarkComplete()
			args.splitMu.Lock()
			if !args.globalSplitStatus[args.conditionID] {
				args.tui.LogEvent("[%s] ⚠️ SPLIT: Balance too low for split ($%.2f < $1.00)", args.marketID, splitAmount)
				args.globalSplitStatus[args.conditionID] = true
			}
			args.splitMu.Unlock()
		}
	}

	if bid1 < args.liveCfg.MinAskPrice || bid2 < args.liveCfg.MinAskPrice || bid1 > args.liveCfg.MaxAskPrice || bid2 > args.liveCfg.MaxAskPrice {
		return false
	}

	bidSum := bid1 + bid2
	sellMargin := (bidSum - 1.0) * 100

	baseTradeSize := args.cfg.CalculateTradeSize(realbotSizingCapitalForTrade(args.engine, args.liveCfg))
	targetBuffer := baseTradeSize * args.cfg.MaxAggressionMultiplier
	currentShares := args.splitInventory.GetMinSplitShares(args.marketID, args.outcomes[0], args.outcomes[1])
	replenishAmount := baseTradeSize * 2.0
	args.splitMu.Lock()
	initialSplitAmount := args.globalInitialSplits[args.conditionID]
	args.splitMu.Unlock()

	decision := args.replenishCtrl.CheckReplenish(paper.ReplenishParams{
		CurrentShares:      currentShares,
		TargetBuffer:       targetBuffer,
		InitialShares:      initialSplitAmount,
		SellMargin:         sellMargin,
		MinMarginThreshold: args.cfg.SplitMinMarginSell - 1.0,
		CurrentBalance:     args.currentBalance,
		ReplenishAmount:    replenishAmount,
		MaxBalancePercent:  args.cfg.SplitReplenishCapPct,
	})

	if decision.ShouldReplenish && args.replenishCtrl.MarkInProgress() {
		if kalshiHoldMode {
			args.replenishCtrl.MarkComplete()
		} else {
			args.tui.LogEvent("[%s] 🔄 SPLIT: Low inventory (%.0f/%.0f), replenishing +%.0f shares...", args.marketID, currentShares, initialSplitAmount, decision.Amount)
			go func(marketID, conditionID, out0, out1 string, amount float64, targetShares float64) {
				defer args.replenishCtrl.MarkComplete()
				bgCtx, bgCancel := context.WithTimeout(args.ctx, 60*time.Second)
				defer bgCancel()

				args.splitTxMu.Lock()
				_, bgErr := args.trader.SplitOnChain(bgCtx, conditionID, amount, len(args.outcomes))
				args.splitTxMu.Unlock()

				if bgErr == nil {
					args.splitInventory.RecordSplit(marketID, out0, out1, amount)
					args.engine.DeductBalance(amount)
					args.engine.RecalculateDrawdown()
					args.tui.LogEvent("[%s] ✅ SPLIT: Replenished to %.0f shares (+%.0f)", marketID, targetShares, amount)
				} else {
					args.tui.LogEvent("[%s] ⚠️ SPLIT: Background replenish failed: %v", marketID, bgErr)
				}
			}(args.marketID, args.conditionID, args.outcomes[0], args.outcomes[1], decision.Amount, initialSplitAmount)
		}
	}

	if sellMargin < args.cfg.SplitMinMarginSell-1e-4 || time.Since(*state.lastSplitSell) <= 2*time.Second {
		return false
	}

	// Prevent the default taker/laddered buy path from firing in the same pass
	// once the split strategy has its own actionable sell trigger.
	requestedShares := args.currentBalance * args.cfg.SplitInitialCapPct

	var availableShares float64
	if kalshiHoldMode {
		availableShares = requestedShares
	} else {
		availableShares = args.splitInventory.GetMinSplitShares(args.marketID, args.outcomes[0], args.outcomes[1])
	}
	sharesToSell := requestedShares
	if sharesToSell > availableShares {
		if availableShares >= minOnChainActionShares {
			args.tui.LogEvent("[%s] ⚠️ SPLIT: Capped sell at available inventory (%s/%s)", args.marketID, formatShareQty(availableShares), formatShareQty(requestedShares))
			sharesToSell = availableShares
		} else {
			sharesToSell = 0
		}
	}

	if sharesToSell < minOnChainActionShares {
		return true
	}

	if sharesToSell > 250 {
		sharesToSell = 250
	}

	bids1 := args.tokenFullBids[args.outcomes[0]]
	bids2 := args.tokenFullBids[args.outcomes[1]]
	bookDepth1, bookDepth2 := len(bids1), len(bids2)
	executionMarginFloor := clampExecutionMarginFloor(args.liveCfg.SplitMinMarginSell, args.liveCfg.BuyExecutionMarginFloorPercent)
	minSum := minExecutablePairSum(executionMarginFloor, args.liveCfg.MinAskPrice)

	sortedBids1 := make([]paper.MarketLevel, len(bids1))
	copy(sortedBids1, bids1)
	hasBid1 := false
	for _, bid := range sortedBids1 {
		if bid.Price >= bid1-1e-6 {
			hasBid1 = true
			break
		}
	}
	if !hasBid1 {
		sortedBids1 = append(sortedBids1, paper.MarketLevel{Price: bid1, Size: sharesToSell})
	}
	sort.Slice(sortedBids1, func(i, j int) bool { return sortedBids1[i].Price > sortedBids1[j].Price })

	sortedBids2 := make([]paper.MarketLevel, len(bids2))
	copy(sortedBids2, bids2)
	hasBid2 := false
	for _, bid := range sortedBids2 {
		if bid.Price >= bid2-1e-6 {
			hasBid2 = true
			break
		}
	}
	if !hasBid2 {
		sortedBids2 = append(sortedBids2, paper.MarketLevel{Price: bid2, Size: sharesToSell})
	}
	sort.Slice(sortedBids2, func(i, j int) bool { return sortedBids2[i].Price > sortedBids2[j].Price })

	var rawLiq1, rawLiq2, matchedBidLiq float64
	var maxValidI, maxValidJ int

	for bi, bj := 0, 0; bi < len(sortedBids1) && bj < len(sortedBids2); {
		if sortedBids1[bi].Price+sortedBids2[bj].Price < minSum-1e-6 {
			break
		}
		if bi+1 > maxValidI {
			maxValidI = bi + 1
			rawLiq1 += sortedBids1[bi].Size
		}
		if bj+1 > maxValidJ {
			maxValidJ = bj + 1
			rawLiq2 += sortedBids2[bj].Size
		}
		matched := sortedBids1[bi].Size
		if sortedBids2[bj].Size < matched {
			matched = sortedBids2[bj].Size
		}
		matchedBidLiq += matched
		if sortedBids1[bi].Size <= sortedBids2[bj].Size {
			sortedBids2[bj].Size -= sortedBids1[bi].Size
			bi++
		} else {
			sortedBids1[bi].Size -= sortedBids2[bj].Size
			bj++
		}
	}

	if sharesToSell > matchedBidLiq {
		sharesToSell = matchedBidLiq
	}

	sharesToSell = normalizeMarketSellShares(sharesToSell)
	if kalshiHoldMode {
		sharesToSell = math.Floor(sharesToSell)
	}

	if sharesToSell < minOnChainActionShares || sharesToSell > availableShares+1e-6 {
		return true
	}

	args.tui.LogEvent("[%s] 📈 SPLIT SELL candidate %s@$%.2f + %s@$%.2f = $%.3f (%.1f%% observed, %.1f%% execution floor) | %s shares [liq: %.0f/%.0f, levels used: %d/%d (total depth: %d/%d)]",
		args.marketID, args.outcomes[0], bid1, args.outcomes[1], bid2, bidSum, sellMargin, executionMarginFloor, formatShareQty(sharesToSell),
		rawLiq1, rawLiq2, maxValidI, maxValidJ, bookDepth1, bookDepth2)

	freshLocalSellQuote, _, localSellQuoteReason := realbotCanUseLocalSellQuote(time.Now(), args.outcomes, args.tokenBids, args.tokenAsks, args.tokenFullBids, args.lastPairUpdate, args.executionQuoteMaxAge)
	if !freshLocalSellQuote {
		args.tui.LogEvent("[%s] ⚠️ Split-sell paused: awaiting fresh local quote (%s)", args.marketID, localSellQuoteReason)
		return true
	}

	bid1 = args.tokenBids[args.outcomes[0]]
	bid2 = args.tokenBids[args.outcomes[1]]
	bidSum = bid1 + bid2
	sellMargin = (bidSum - 1.0) * 100
	if sellMargin < args.cfg.SplitMinMarginSell-1e-4 {
		args.tui.LogEvent("[%s] ⚠️ Local sell quote moved away: %s=%.3f, %s=%.3f (%.1f%% < %.1f%% trigger)", args.marketID, args.outcomes[0], bid1, args.outcomes[1], bid2, sellMargin, args.cfg.SplitMinMarginSell)
		return true
	}

	freshMatchedLiquidity := realbotMatchedBidLiquidity(args.tokenFullBids[args.outcomes[0]], args.tokenFullBids[args.outcomes[1]], minSum)
	if sharesToSell > freshMatchedLiquidity {
		args.tui.LogEvent("[%s] ⚡ Local sell quote capped shares %s→%s using local matched liquidity %s", args.marketID, formatShareQty(sharesToSell), formatShareQty(freshMatchedLiquidity), formatShareQty(freshMatchedLiquidity))
		sharesToSell = freshMatchedLiquidity
	}
	sharesToSell = normalizeMarketSellShares(sharesToSell)
	if sharesToSell < minOnChainActionShares {
		args.tui.LogEvent("[%s] ⚠️ Local sell quote left less than %.2f share actionable liquidity: %.4f", args.marketID, minOnChainActionShares, sharesToSell)
		return true
	}

	token0 := args.getTokenID(args.outcomes[0])
	token1 := args.getTokenID(args.outcomes[1])
	if token0 == "" || token1 == "" {
		args.tui.LogEvent("[%s] ⚠️ SPLIT: Token ID not found for %s/%s", args.marketID, args.outcomes[0], args.outcomes[1])
		return true
	}

	initialSnapshot0 := args.trader.GetLivePositionSize(token0)
	initialSnapshot1 := args.trader.GetLivePositionSize(token1)
	initialBal0 := initialSnapshot0
	initialBal1 := initialSnapshot1
	haveInitialSnapshot := true

	rate1 := realbotResolveFeeRateBps(args.tokenFeeRates, args.outcomes[0], nil)
	rate2 := realbotResolveFeeRateBps(args.tokenFeeRates, args.outcomes[1], nil)

	batchExecs := executeMarketOrderBatchWithSignals(args.ctx, args.trader, []directMarketOrderSignalRequest{
		{
			Side:           api.SideSell,
			TokenID:        token0,
			Outcome:        args.outcomes[0],
			Price:          args.liveCfg.MinAskPrice,
			Size:           sharesToSell,
			FeeRateBps:     rate1,
			InitialBalance: initialBal0,
		},
		{
			Side:           api.SideSell,
			TokenID:        token1,
			Outcome:        args.outcomes[1],
			Price:          args.liveCfg.MinAskPrice,
			Size:           sharesToSell,
			FeeRateBps:     rate2,
			InitialBalance: initialBal1,
		},
	}, 2*time.Second)
	exec1, exec2 := batchExecs[0], batchExecs[1]

	sold1, sold2 := exec1.ExecutedQty, exec2.ExecutedQty
	side1Success, side2Success := exec1.Success, exec2.Success
	price1, price2 := bid1, bid2
	if eff := venueExecutionEffectivePrice(exec1); eff > 0 {
		price1 = eff
	}
	if eff := venueExecutionEffectivePrice(exec2); eff > 0 {
		price2 = eff
	}
	proceeds1 := reportedSellProceeds(exec1, price1, sold1, sharesToSell)
	proceeds2 := reportedSellProceeds(exec2, price2, sold2, sharesToSell)

	if haveInitialSnapshot && (side1Success || side2Success) {
		verifyCtx, cancelVerify := context.WithTimeout(context.Background(), realbotCleanupVerifyTTL)
		verifiedSold0, verifiedSold1, verifyBal0, verifyBal1, verifySource, verifyErr := waitForPairSellBalanceReduction(verifyCtx, args.trader, token0, token1, initialSnapshot0, initialSnapshot1, haveInitialSnapshot, side1Success, side2Success)
		cancelVerify()
		if side1Success {
			sold1 = math.Min(verifiedSold0, sharesToSell)
		}
		if side2Success {
			sold2 = math.Min(verifiedSold1, sharesToSell)
		}
		if verifyErr != nil && ((!side1Success || hasConfirmedExecutedQty(api.SideSell, sold1)) && (!side2Success || hasConfirmedExecutedQty(api.SideSell, sold2))) {
			args.tui.LogEvent("[%s] ⚠️ Split-sell balance verification warning: %v", args.marketID, verifyErr)
		} else if verifyErr != nil {
			args.tui.LogEvent("[%s] ⚠️ Split-sell balance verification still pending (%s): %s=%.4f, %s=%.4f", args.marketID, verifySource, args.outcomes[0], verifyBal0, args.outcomes[1], verifyBal1)
		}
		if side1Success && !hasConfirmedExecutedQty(api.SideSell, sold1) {
			args.tui.LogEvent("[%s] ⚠️ Split-sell for %s lacked wallet-truth reduction (%s snapshot from %s); leaving inventory unchanged", args.marketID, args.outcomes[0], formatShareQty(verifyBal0), verifySource)
			side1Success = false
		}
		if side2Success && !hasConfirmedExecutedQty(api.SideSell, sold2) {
			args.tui.LogEvent("[%s] ⚠️ Split-sell for %s lacked wallet-truth reduction (%s snapshot from %s); leaving inventory unchanged", args.marketID, args.outcomes[1], formatShareQty(verifyBal1), verifySource)
			side2Success = false
		}
	} else if side1Success || side2Success {
		args.tui.LogEvent("[%s] ⚠️ Split-sell balance verification unavailable (initial snapshot missing); using direct execution signals only", args.marketID)
	}

	if side1Success != side2Success {
		failedOutcome := args.outcomes[1]
		if !side1Success {
			failedOutcome = args.outcomes[0]
		}
		args.tui.LogEvent("[%s] ⚠️ SPLIT LEGGED: %s still not sold (leaving for cleanup path)", args.marketID, failedOutcome)
	}

	if side1Success && side2Success {
		var totalProfit float64
		var profit1, profit2 float64
		if kalshiHoldMode {
			profit1 = realbotApplySplitSellAccounting(args.engine, args.splitInventory, args.marketID, args.outcomes[0], sold1, price1, proceeds1, true)
			profit2 = realbotApplySplitSellAccounting(args.engine, args.splitInventory, args.marketID, args.outcomes[1], sold2, price2, proceeds2, true)
			totalProfit = profit1 + profit2
			args.tui.LogEvent("[%s] ✅ PANIC SOLD! %s: %.2f, %s: %.2f | Profit: ~+$%.2f", args.marketID, args.outcomes[0], sold1, args.outcomes[1], sold2, totalProfit)
		} else {
			profit1 = realbotApplySplitSellAccounting(args.engine, args.splitInventory, args.marketID, args.outcomes[0], sold1, price1, proceeds1, false)
			profit2 = realbotApplySplitSellAccounting(args.engine, args.splitInventory, args.marketID, args.outcomes[1], sold2, price2, proceeds2, false)
			totalProfit = profit1 + profit2
			args.tui.LogEvent("[%s] ✅ SPLIT SOLD! %s: %.2f, %s: %.2f | Profit: +$%.2f", args.marketID, args.outcomes[0], sold1, args.outcomes[1], sold2, totalProfit)
		}

		args.tui.RecordOrder(args.marketID, args.outcomes[0], "SELL", sold1, price1, proceeds1, sellMargin, profit1, "FILLED")
		args.tui.RecordOrder(args.marketID, args.outcomes[1], "SELL", sold2, price2, proceeds2, sellMargin, profit2, "FILLED")
		_, _ = args.trader.ForceRefreshBalance(args.ctx)
		args.tui.LogEvent("[%s] ✅ Execution complete after successful panic/split sell.", args.marketID)
	} else {
		if side1Success {
			_ = realbotApplySplitSellAccounting(args.engine, args.splitInventory, args.marketID, args.outcomes[0], sold1, price1, proceeds1, kalshiHoldMode)
			args.tui.LogEvent("[%s] ⚠️ SELL: Only %s sold %.2f (one-shot)", args.marketID, args.outcomes[0], sold1)
		}
		if side2Success {
			_ = realbotApplySplitSellAccounting(args.engine, args.splitInventory, args.marketID, args.outcomes[1], sold2, price2, proceeds2, kalshiHoldMode)
			args.tui.LogEvent("[%s] ⚠️ SELL: Only %s sold %.2f (one-shot)", args.marketID, args.outcomes[1], sold2)
		}
	}

	args.refreshWalletTruth(5 * time.Second)
	*state.lastSplitSell = time.Now()
	return true
}
