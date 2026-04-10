package main

import (
	"context"
	"fmt"
	"math"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

func settleMarketInventory(
	ctx context.Context,
	id string,
	market *api.Market,
	outcomes []string,
	tokenFeeRates map[string]int,
	trader *trading.RealTrader,
	engine *paper.Engine,
	splitInventory *paper.SplitInventory,
	tui *paper.TUI,
	restClient *api.RestClient,
	allowSell bool,
	sellCap float64,
	reason string,
	allowMerge bool,
	mergeCoordinator *realbotMergeCoordinator,
) error {
	if len(outcomes) != 2 || len(market.Tokens) != 2 {
		return nil
	}

	token0 := market.Tokens[0].TokenID
	token1 := market.Tokens[1].TokenID
	bal0, bal1, balanceSource, err := loadPairBalancesForCleanupVerification(ctx, trader, token0, token1)
	if err != nil {
		return err
	}
	pendingMergeQty := 0.0
	if allowMerge && mergeCoordinator != nil {
		pendingMergeQty = mergeCoordinator.pendingQty(id)
		if pendingMergeQty >= minOnChainActionShares {
			bal0, bal1 = subtractMergedPairBalances(bal0, bal1, pendingMergeQty)
			tui.LogEvent("[%s] 🔀 %s merge already pending for %.6f balanced shares; cleanup will focus only on excess inventory", id, reason, pendingMergeQty)
		}
	}

	minQty := math.Min(bal0, bal1)
	if minQty >= minOnChainActionShares {
		tui.LogEvent("[%s] 🔍 %s inventory snapshot (%s): %s=%.6f, %s=%.6f", id, reason, balanceSource, outcomes[0], bal0, outcomes[1], bal1)
		if !allowMerge {
			bal0, bal1 = subtractMergedPairBalances(bal0, bal1, minQty)
			tui.LogEvent("[%s] 🪜 %s keeping %.6f balanced shares parked; auto-merge is disabled", id, reason, minQty)
		} else if launchBackgroundMerge(id, reason, outcomes, market.ConditionID, minQty, len(market.Tokens), trader, engine, splitInventory, tui, mergeCoordinator) {
			pendingMergeQty += minQty
			bal0, bal1 = subtractMergedPairBalances(bal0, bal1, minQty)
		} else if pendingMergeQty < minOnChainActionShares {
			tui.LogEvent("[%s] ⚠️ %s merge not relaunched because another merge slot is already busy; excess cleanup will continue", id, reason)
		}
	}

	if !allowSell {
		return nil
	}

	balances := []struct {
		tokenID string
		outcome string
		qty     float64
	}{
		{tokenID: token0, outcome: outcomes[0], qty: bal0},
		{tokenID: token1, outcome: outcomes[1], qty: bal1},
	}

	for _, side := range balances {
		if isDustCleanupRemainder(side.qty) {
			tui.LogEvent("[%s] ℹ️ %s leaving dust remainder for %s: %.4f shares below %.2f-share cleanup minimum", id, reason, side.outcome, side.qty, minOnChainActionShares)
			continue
		}
		if !hasActionableCleanupRemainder(side.qty) {
			continue
		}
		rate := tokenFeeRates[side.outcome]
		if rate == 0 {
			rate = 1000
		}

		aggressiveDumpPrice := core.CleanupSellLimitPrice(sellCap)
		quoteCtx, cancelQuote := context.WithTimeout(ctx, realbotExecQuoteTimeout)
		cleanupQuote, quoteErr := realbotBuildCleanupSellQuote(quoteCtx, restClient, side.tokenID, side.qty, sellCap)
		cancelQuote()
		if quoteErr != nil {
			tui.LogEvent("[%s] ⚠️ %s cleanup quote unavailable for %s: %v", id, reason, side.outcome, quoteErr)
			continue
		}
		if cleanupQuote.SubmitPrice+1e-9 < aggressiveDumpPrice {
			tui.LogEvent("[%s] 📡 %s repriced %s cleanup to live bid floor $%.3f (best bid $%.3f, age %s)", id, reason, side.outcome, cleanupQuote.SubmitPrice, cleanupQuote.BestBid, cleanupQuote.BookAge.Round(time.Millisecond))
		}
		if cleanupQuote.ExecutableQty+1e-9 < side.qty {
			tui.LogEvent("[%s] ⚡ %s capped %s cleanup %s→%s on live bid liquidity %s", id, reason, side.outcome, formatShareQty(side.qty), formatShareQty(cleanupQuote.ExecutableQty), formatShareQty(cleanupQuote.TotalBidLiquidity))
		}

		exec := executeMarketOrderWithSignals(ctx, trader, api.SideSell, side.tokenID, side.outcome, cleanupQuote.SubmitPrice, cleanupQuote.ExecutableQty, rate, side.qty, 2*time.Second)
		if !exec.Success {
			if exec.Result != nil && isMinSizeRejectionMessage(exec.Result.Message) {
				tui.LogEvent("[%s] ⚠️ %s: %s", id, reason, cleanupRejectionMessage(cleanupQuote.ExecutableQty, side.outcome, exec.Result.Message))
				continue
			}
			if exec.Err != nil {
				tui.LogEvent("[%s] ⚠️ %s sell cleanup failed for %s: %v", id, reason, side.outcome, exec.Err)
			} else if exec.Result != nil && exec.Result.Message != "" {
				tui.LogEvent("[%s] ⚠️ %s sell cleanup failed for %s: %s", id, reason, side.outcome, exec.Result.Message)
			} else {
				tui.LogEvent("[%s] ⚠️ %s sell cleanup failed for %s", id, reason, side.outcome)
			}
			continue
		}
		tui.LogEvent("[%s] 📉 %s sold %s unbalanced shares of %s", id, reason, formatShareQty(exec.ExecutedQty), side.outcome)
	}

	verifyTTL := realbotCleanupVerifyTTL
	if pendingMergeQty >= minOnChainActionShares {
		verifyTTL = realbotFastVerifyTTL
	}
	verifyCtx, cancelVerify := context.WithTimeout(context.Background(), verifyTTL)
	remaining0, remaining1, verifySource, verifyErr := waitForPairFlatBalances(verifyCtx, trader, token0, token1)
	cancelVerify()
	effectiveRemaining0, effectiveRemaining1 := remaining0, remaining1
	if !allowMerge {
		if parkedQty := math.Min(remaining0, remaining1); parkedQty >= minOnChainActionShares {
			effectiveRemaining0, effectiveRemaining1 = subtractMergedPairBalances(remaining0, remaining1, parkedQty)
		}
	} else if pendingVerifyQty := mergeCoordinator.pendingQty(id); pendingVerifyQty >= minOnChainActionShares {
		effectiveRemaining0, effectiveRemaining1 = subtractMergedPairBalances(remaining0, remaining1, pendingVerifyQty)
	}
	if (hasActionableCleanupRemainder(effectiveRemaining0) || hasActionableCleanupRemainder(effectiveRemaining1)) && verifyErr != nil {
		return fmt.Errorf("cleanup still unresolved (%s): %s=%.4f, %s=%.4f (%w)", verifySource, outcomes[0], effectiveRemaining0, outcomes[1], effectiveRemaining1, verifyErr)
	}
	if hasActionableCleanupRemainder(effectiveRemaining0) || hasActionableCleanupRemainder(effectiveRemaining1) {
		return fmt.Errorf("cleanup still holding inventory (%s): %s=%.4f, %s=%.4f", verifySource, outcomes[0], effectiveRemaining0, outcomes[1], effectiveRemaining1)
	}

	return nil
}

func combineCleanupVerificationBalances(live0, live1, pos0, pos1, onChain0, onChain1 float64, posErr, onChainErr error) (bal0, bal1 float64, source string, err error) {
	hasLive := shouldAttemptCleanupSell(live0) || shouldAttemptCleanupSell(live1)
	hasPos := posErr == nil && (shouldAttemptCleanupSell(pos0) || shouldAttemptCleanupSell(pos1))

	if onChainErr == nil {
		return onChain0, onChain1, "on-chain truth", nil
	}
	if posErr == nil {
		bal0, bal1 = preferLivePairBalances(live0, live1, pos0, pos1)
		source = "external position snapshot"
		switch {
		case hasLive && hasPos:
			source = "live WS + external position snapshot"
		case hasLive:
			source = "live WS"
		}
		return bal0, bal1, source, nil
	}
	if hasLive {
		return live0, live1, "live WS", nil
	}
	return 0, 0, "", fmt.Errorf("external position snapshot failed (%v); on-chain truth failed (%v)", posErr, onChainErr)
}

func loadPairBalancesForCleanupVerification(ctx context.Context, trader *trading.RealTrader, token0, token1 string) (bal0, bal1 float64, source string, err error) {
	live0 := trader.GetLivePositionSize(token0)
	live1 := trader.GetLivePositionSize(token1)
	pos0, pos1, posErr := loadPairPositionBalances(ctx, trader, token0, token1)
	onChain0, onChain1, onChainErr := loadPairOnChainBalances(ctx, trader, token0, token1)
	return combineCleanupVerificationBalances(live0, live1, pos0, pos1, onChain0, onChain1, posErr, onChainErr)
}

func loadAcquiredPairBalances(ctx context.Context, trader *trading.RealTrader, token0, token1 string, initial0, initial1 float64, haveInitialSnapshot bool) (acquired0, acquired1, bal0, bal1 float64, source string, err error) {
	bal0, bal1, source, err = loadPairBalancesForCleanupVerification(ctx, trader, token0, token1)
	if err != nil {
		return 0, 0, 0, 0, source, err
	}
	acquired0, acquired1 = acquiredPairBalances(initial0, initial1, bal0, bal1, haveInitialSnapshot)
	return acquired0, acquired1, bal0, bal1, source, nil
}

func reducedPairBalances(initial0, initial1, current0, current1 float64, haveInitialSnapshot bool) (sold0, sold1 float64) {
	if !haveInitialSnapshot {
		return 0, 0
	}
	if current0 < initial0 {
		sold0 = initial0 - current0
	}
	if current1 < initial1 {
		sold1 = initial1 - current1
	}
	return sold0, sold1
}

func loadReducedPairBalances(ctx context.Context, trader *trading.RealTrader, token0, token1 string, initial0, initial1 float64, haveInitialSnapshot bool) (sold0, sold1, bal0, bal1 float64, source string, err error) {
	bal0, bal1, source, err = loadPairBalancesForCleanupVerification(ctx, trader, token0, token1)
	if err != nil {
		return 0, 0, 0, 0, source, err
	}
	sold0, sold1 = reducedPairBalances(initial0, initial1, bal0, bal1, haveInitialSnapshot)
	return sold0, sold1, bal0, bal1, source, nil
}

func waitForPairSellBalanceReduction(ctx context.Context, trader *trading.RealTrader, token0, token1 string, initial0, initial1 float64, haveInitialSnapshot bool, waitFor0, waitFor1 bool) (sold0, sold1, bal0, bal1 float64, source string, err error) {
	bestSold0, bestSold1 := 0.0, 0.0
	bestBal0, bestBal1 := initial0, initial1
	bestSource := ""
	for {
		sold0, sold1, bal0, bal1, source, err = loadReducedPairBalances(ctx, trader, token0, token1, initial0, initial1, haveInitialSnapshot)
		if sold0 > bestSold0 {
			bestSold0 = sold0
			bestBal0 = bal0
		}
		if sold1 > bestSold1 {
			bestSold1 = sold1
			bestBal1 = bal1
		}
		if source != "" {
			bestSource = source
		}
		if err == nil && (!waitFor0 || hasConfirmedExecutedQty(api.SideSell, sold0)) && (!waitFor1 || hasConfirmedExecutedQty(api.SideSell, sold1)) {
			return sold0, sold1, bal0, bal1, source, nil
		}
		select {
		case <-ctx.Done():
			if bestSource == "" {
				bestSource = source
			}
			return bestSold0, bestSold1, bestBal0, bestBal1, bestSource, err
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func waitForAcquiredCleanupResolution(ctx context.Context, trader *trading.RealTrader, token0, token1 string, initial0, initial1 float64, haveInitialSnapshot bool) (remaining0, remaining1, bal0, bal1 float64, source string, err error) {
	for {
		remaining0, remaining1, bal0, bal1, source, err = loadAcquiredPairBalances(ctx, trader, token0, token1, initial0, initial1, haveInitialSnapshot)
		if err == nil && !hasActionableCleanupRemainder(remaining0) && !hasActionableCleanupRemainder(remaining1) {
			return remaining0, remaining1, bal0, bal1, source, nil
		}
		select {
		case <-ctx.Done():
			return remaining0, remaining1, bal0, bal1, source, err
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func waitForPairFlatBalances(ctx context.Context, trader *trading.RealTrader, token0, token1 string) (bal0, bal1 float64, source string, err error) {
	for {
		bal0, bal1, source, err = loadPairBalancesForCleanupVerification(ctx, trader, token0, token1)
		if err == nil && !hasActionableCleanupRemainder(bal0) && !hasActionableCleanupRemainder(bal1) {
			return bal0, bal1, source, nil
		}
		select {
		case <-ctx.Done():
			return bal0, bal1, source, err
		case <-time.After(500 * time.Millisecond):
		}
	}
}

type realbotLiveRecoveryArgs struct {
	ctx                context.Context
	marketID           string
	market             *api.Market
	outcomes           []string
	tokenFeeRates      map[string]int
	primaryMode        string
	trader             *trading.RealTrader
	engine             *paper.Engine
	splitInventory     *paper.SplitInventory
	tui                *paper.TUI
	restClient         *api.RestClient
	liveCfg            paper.TUISettings
	mergeCoordinator   *realbotMergeCoordinator
	refreshWalletTruth func(time.Duration)
	lastTrade          time.Time
}

type realbotLiveRecoveryState struct {
	currentBalance          *float64
	nextLiveRecoveryAttempt *time.Time
	panicBuyCooldown        *time.Time
	lastDustRecoveryNotice  *time.Time
}

func realbotHandleLiveRecovery(args realbotLiveRecoveryArgs, state *realbotLiveRecoveryState) bool {
	if args.primaryMode == paperArbModeCopytrade || args.primaryMode == paperArbModeLaddered || args.primaryMode == realbotExecutionModeTakerClose {
		return false
	}
	if len(args.outcomes) != 2 || args.market == nil || len(args.market.Tokens) < 2 {
		return false
	}
	if time.Since(args.lastTrade) <= 5*time.Second {
		return false
	}
	if state == nil || state.nextLiveRecoveryAttempt == nil || time.Now().Before(*state.nextLiveRecoveryAttempt) {
		return false
	}

	recoveryCheckCtx, cancelRecoveryCheck := context.WithTimeout(context.Background(), 3*time.Second)
	pendingRecovery0, pendingRecovery1, recoverySource, recoveryCheckErr := pendingPairRecoveryBalances(recoveryCheckCtx, args.marketID, args.market.Tokens[0].TokenID, args.market.Tokens[1].TokenID, args.outcomes, args.trader, args.engine, args.splitInventory)
	cancelRecoveryCheck()

	if recoveryCheckErr == nil && (hasActionableCleanupRemainder(pendingRecovery0) || hasActionableCleanupRemainder(pendingRecovery1)) {
		args.tui.LogEvent("[%s] 🔄 Pending inventory detected (%s): %s=%.4f, %s=%.4f — attempting live recovery...", args.marketID, recoverySource, args.outcomes[0], pendingRecovery0, args.outcomes[1], pendingRecovery1)
		recoveryCtx, cancelRecovery := context.WithTimeout(context.Background(), 45*time.Second)
		recoveryErr := settleMarketInventory(recoveryCtx, args.marketID, args.market, args.outcomes, args.tokenFeeRates, args.trader, args.engine, args.splitInventory, args.tui, args.restClient, true, args.liveCfg.MinAskPrice, "LIVE RECOVERY", realbotShouldAutoMergeBalancedInventory(args.liveCfg), args.mergeCoordinator)
		trimmed, trimErr := reconcileLocalBoughtPositionsToWalletTruth(recoveryCtx, args.marketID, args.market.Tokens[0].TokenID, args.market.Tokens[1].TokenID, args.outcomes, args.trader, args.engine, args.splitInventory, args.tui)
		cancelRecovery()
		args.refreshWalletTruth(5 * time.Second)
		if newBal, err := args.trader.GetBalance(args.ctx); err == nil {
			if state.currentBalance != nil {
				*state.currentBalance = newBal
			}
			args.engine.SyncBalanceNeutral(newBal)
			args.engine.RecalculateDrawdown()
			realbotRefreshWalletCashDisplay(args.ctx, args.trader, args.tui, 8*time.Second)
		}
		switch {
		case trimErr != nil:
			args.tui.LogEvent("[%s] ⚠️ Live recovery wallet-truth sync failed: %v", args.marketID, trimErr)
		case trimmed:
			args.tui.LogEvent("[%s] ✅ Live recovery synchronized local inventory to wallet truth.", args.marketID)
		}
		if recoveryErr != nil {
			args.tui.LogEvent("[%s] ⚠️ Live recovery incomplete: %v", args.marketID, recoveryErr)
			*state.nextLiveRecoveryAttempt = time.Now().Add(10 * time.Second)
			if state.panicBuyCooldown != nil && state.panicBuyCooldown.Before(time.Now().Add(15*time.Second)) {
				*state.panicBuyCooldown = time.Now().Add(15 * time.Second)
			}
			return false
		}
		*state.nextLiveRecoveryAttempt = time.Now().Add(15 * time.Second)
		return true
	}

	if recoveryCheckErr == nil && (isDustCleanupRemainder(pendingRecovery0) || isDustCleanupRemainder(pendingRecovery1)) {
		if state.lastDustRecoveryNotice != nil && time.Since(*state.lastDustRecoveryNotice) > 45*time.Second {
			args.tui.LogEvent("[%s] ℹ️ Residual dust below %.2f-share cleanup minimum (%s): %s=%.4f, %s=%.4f — skipping live recovery retries for now", args.marketID, minOnChainActionShares, recoverySource, args.outcomes[0], pendingRecovery0, args.outcomes[1], pendingRecovery1)
			*state.lastDustRecoveryNotice = time.Now()
		}
		*state.nextLiveRecoveryAttempt = time.Now().Add(60 * time.Second)
		return false
	}

	*state.nextLiveRecoveryAttempt = time.Now().Add(5 * time.Second)
	return false
}
