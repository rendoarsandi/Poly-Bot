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

func realbotExecuteAggressiveEntry(
	ctx context.Context,
	id string,
	market *api.Market,
	outcomes []string,
	ask1, ask2 float64,
	requestSize1, requestSize2 float64,
	limitPrice1, limitPrice2 float64,
	observedMargin float64,
	ladderedMode bool,
	ladderedDirection int,
	token0, token1 string,
	side1Requested, side2Requested bool,
	tokenFeeRates map[string]int,
	trader *trading.RealTrader,
	engine *paper.Engine,
	tui *paper.TUI,
	cfg *core.Config,
	realbotCfg paper.TUISettings,
	rMinAsk float64,
	splitInventory *paper.SplitInventory,
	restClient *api.RestClient,
	mergeCoordinator *realbotMergeCoordinator,
	refreshWalletTruth func(time.Duration),
	entryGate *realbotEntryGate,
	entryExecutionDone chan<- realbotAsyncEntryResult,
	acquiredGate bool,
	ladderedEntrySeq uint64,
) {
	asyncResult := realbotAsyncEntryResult{ladderedEntrySeq: ladderedEntrySeq}
	defer func() {
		if r := recover(); r != nil {
			asyncResult.cooldownUntil = time.Now().Add(2 * time.Second)
			if tui != nil {
				tui.LogEvent("[%s] ⚠️ Aggressive entry recovered from panic: %v", id, r)
			}
		}
		asyncResult.lastTradeAt = time.Now()
		if acquiredGate && entryGate != nil {
			entryGate.Release()
		}
		select {
		case entryExecutionDone <- asyncResult:
		case <-ctx.Done():
		}
	}()

	var res1, res2 *trading.TradeResult
	var err1, err2 error
	initialSnapshot0, initialSnapshot1, initialSnapshotSource, initialSnapshotErr := realbotInitialPairSnapshot(ctx, trader, token0, token1, ladderedMode)
	haveInitialSnapshot := true
	initialBal0 := initialSnapshot0
	initialBal1 := initialSnapshot1
	if ladderedMode {
		if initialSnapshotErr != nil {
			tui.LogEvent("[%s] ⚠️ Skipping ladder buy: authoritative pre-trade snapshot unavailable (%v)", id, initialSnapshotErr)
			asyncResult.cooldownUntil = time.Now().Add(2 * time.Second)
			return
		}
	}

	rate1 := realbotResolveFeeRateBps(tokenFeeRates, outcomes[0], cfg)
	rate2 := realbotResolveFeeRateBps(tokenFeeRates, outcomes[1], cfg)
	ladderedUSDCBuy := ladderedMode && strings.EqualFold(realbotCfg.LadderedTakerSizingMode, core.LadderedTakerSizingModeUSDC)
	ladderedUSDCBudget := 0.0
	if ladderedUSDCBuy {
		ladderedUSDCBudget = realbotNormalizedLadderedUSDCBudget(realbotCfg)
	}

	var requests []directMarketOrderSignalRequest
	if side1Requested {
		req := directMarketOrderSignalRequest{
			Side:           api.SideBuy,
			TokenID:        token0,
			Outcome:        outcomes[0],
			Price:          limitPrice1,
			Size:           requestSize1,
			FeeRateBps:     rate1,
			InitialBalance: initialBal0,
			ExactShares:    true,
		}
		if ladderedUSDCBuy {
			req.Size = directUSDCAmountForBuyShareCap(requestSize1, limitPrice1, ladderedUSDCBudget)
			req.ExactShares = false
			requestSize1 = directRequestedShareCap(req)
		}
		requests = append(requests, req)
	}
	if side2Requested {
		req := directMarketOrderSignalRequest{
			Side:           api.SideBuy,
			TokenID:        token1,
			Outcome:        outcomes[1],
			Price:          limitPrice2,
			Size:           requestSize2,
			FeeRateBps:     rate2,
			InitialBalance: initialBal1,
			ExactShares:    true,
		}
		if ladderedUSDCBuy {
			req.Size = directUSDCAmountForBuyShareCap(requestSize2, limitPrice2, ladderedUSDCBudget)
			req.ExactShares = false
			requestSize2 = directRequestedShareCap(req)
		}
		requests = append(requests, req)
	}

	batchExecs := executeMarketOrderBatchWithSignals(ctx, trader, requests, realbotBatchBuyConfirmTimeout)
	var exec1, exec2 directMarketExecution
	if ladderedMode {
		if ladderedDirection == 0 {
			exec1 = batchExecs[0]
			exec2 = directMarketExecution{Success: false}
		} else {
			exec1 = directMarketExecution{Success: false}
			exec2 = batchExecs[0]
		}
	} else {
		exec1, exec2 = batchExecs[0], batchExecs[1]
	}

	res1, err1 = exec1.Result, exec1.Err
	res2, err2 = exec2.Result, exec2.Err
	rawFilled1, rawFilled2 := exec1.ExecutedQty, exec2.ExecutedQty
	filled1, filled2 := rawFilled1, rawFilled2
	side1Success, side2Success := exec1.Success, exec2.Success
	logDirectExecutionAudit(tui, id, "Side 1 BUY", requestSize1, limitPrice1, exec1)
	logDirectExecutionAudit(tui, id, "Side 2 BUY", requestSize2, limitPrice2, exec2)
	if _, _, _, verifyErr := loadPairBalancesWSFirst(ctx, trader, token0, token1); verifyErr != nil && !ladderedMode {
		tui.LogEvent("[%s] ⚠️ External position snapshot unavailable after direct buy: %v", id, verifyErr)
	}

	attributionTrusted := false
	recoveredLateLadderFill := false
	recoveredLateLadderSource := ""
	if haveInitialSnapshot && !ladderedMode {
		prevSide1Success, prevSide2Success := side1Success, side2Success
		rawAttribution1 := attributedBuyFill(exec1, requestSize1, 0, false)
		rawAttribution2 := attributedBuyFill(exec2, requestSize2, 0, false)
		attrCtx, cancelAttr := context.WithTimeout(ctx, realbotBuyAttributionTimeout)
		acquired0, acquired1, absBal0, absBal1, attrSource, attrErr := reconcileBoughtPairBalances(attrCtx, trader, token0, token1, initialSnapshot0, initialSnapshot1, true)
		cancelAttr()
		if attrErr == nil || shouldAttemptCleanupSell(acquired0) || shouldAttemptCleanupSell(acquired1) {
			attributionTrusted = true
			filled1 = attributedBuyFill(exec1, requestSize1, acquired0, true)
			filled2 = attributedBuyFill(exec2, requestSize2, acquired1, true)
			side1Success = hasConfirmedExecutedQty(api.SideBuy, filled1)
			side2Success = hasConfirmedExecutedQty(api.SideBuy, filled2)
			if !side1Success && prevSide1Success && hasConfirmedExecutedQty(api.SideBuy, rawAttribution1) {
				filled1 = rawAttribution1
				side1Success = true
				if !ladderedMode {
					tui.LogEvent("[%s] ℹ️ Side 1 buy confirmation fell back to venue/WS ack while balance attribution lagged (%s)", id, attrSource)
				}
			}
			if !side2Success && prevSide2Success && hasConfirmedExecutedQty(api.SideBuy, rawAttribution2) {
				filled2 = rawAttribution2
				side2Success = true
				if !ladderedMode {
					tui.LogEvent("[%s] ℹ️ Side 2 buy confirmation fell back to venue/WS ack while balance attribution lagged (%s)", id, attrSource)
				}
			}
			if !ladderedMode && (math.Abs(rawFilled1-filled1) > 0.25 || math.Abs(rawFilled2-filled2) > 0.25) {
				tui.LogEvent("[%s] 🧾 PANIC BUY attribution (%s): %s abs=%.4f Δ=%.4f, %s abs=%.4f Δ=%.4f", id, attrSource, outcomes[0], absBal0, filled1, outcomes[1], absBal1, filled2)
			}
		} else if !ladderedMode {
			tui.LogEvent("[%s] ⚠️ PANIC BUY attribution unavailable; using capped order confirmation only: %v", id, attrErr)
		}
	}
	if !attributionTrusted {
		filled1 = attributedBuyFill(exec1, requestSize1, 0, false)
		filled2 = attributedBuyFill(exec2, requestSize2, 0, false)
		side1Success = side1Success && hasConfirmedExecutedQty(api.SideBuy, filled1)
		side2Success = side2Success && hasConfirmedExecutedQty(api.SideBuy, filled2)
	} else {
		if !side1Success && exec1.Success && res1 != nil && strings.TrimSpace(res1.Message) == "" {
			res1.Message = "No fresh buy delta attributable after snapshot verification"
		}
		if !side2Success && exec2.Success && res2 != nil && strings.TrimSpace(res2.Message) == "" {
			res2.Message = "No fresh buy delta attributable after snapshot verification"
		}
	}
	if ladderedMode {
		requestedQty := requestSize1
		optimisticFilled := filled1
		confirmed := side1Success
		activeOutcome := outcomes[0]
		activeRes := res1
		if ladderedDirection == 1 {
			requestedQty = requestSize2
			optimisticFilled = filled2
			confirmed = side2Success
			activeOutcome = outcomes[1]
			activeRes = res2
		}
		recoverCtx, cancelRecover := context.WithTimeout(ctx, 3*time.Second)
		recoveredQty, recoverSource, recoverErr := realbotRecoverLateLadderedBuyFill(recoverCtx, trader, token0, token1, initialSnapshot0, initialSnapshot1, ladderedDirection, requestedQty)
		cancelRecover()
		verifiedQty, verifiedConfirmed, authoritativeRecovery := realbotVerifiedLadderedBuyFill(requestedQty, optimisticFilled, recoveredQty, recoverErr)
		if ladderedDirection == 1 {
			filled2 = verifiedQty
			side2Success = verifiedConfirmed
		} else {
			filled1 = verifiedQty
			side1Success = verifiedConfirmed
		}
		_ = activeRes
		switch {
		case authoritativeRecovery && verifiedConfirmed && !confirmed:
			recoveredLateLadderFill = true
			recoveredLateLadderSource = recoverSource
		case authoritativeRecovery && verifiedConfirmed && math.Abs(verifiedQty-optimisticFilled) > 0.000001:
			tui.LogEvent("[%s] 🧾 Ladder fill adjusted via %s: %s %s→%s", id, recoverSource, activeOutcome, formatShareQty(optimisticFilled), formatShareQty(verifiedQty))
		case authoritativeRecovery && !verifiedConfirmed:
			// Disabled per user request:
			// if activeRes != nil && strings.TrimSpace(activeRes.Message) == "" {
			// 	activeRes.Message = "No fresh ladder buy delta attributable after verification"
			// }
		case recoverErr != nil && !confirmed:
			tui.LogEvent("[%s] ⚠️ Ladder late-fill check failed: %v", id, recoverErr)
		}
	}

	cost1 := reportedBuyCost(exec1, ask1, filled1, requestSize1)
	cost2 := reportedBuyCost(exec2, ask2, filled2, requestSize2)
	executionMode := paperArbModeTaker
	if ladderedMode {
		executionMode = paperArbModeLaddered
	}
	shouldMirrorEngine := realbotShouldMirrorExecutionIntoEngine(trader)

	if side1Requested && side1Success {
		if !ladderedMode {
			tui.LogEvent("[%s] ✅ Side 1 MARKET: %s (Observed $%.3f, Filled: %.2f/%.2f)", id, outcomes[0], ask1, filled1, requestSize1)
		}
		tui.RecordOrderWithMode(id, outcomes[0], "BUY", filled1, ask1, cost1, observedMargin, 0.0, executionMode, "FILLED")
	} else if side1Requested {
		isRoutineLadderFail := ladderedMode && err1 == nil && (res1 == nil || strings.TrimSpace(res1.Message) == "")
		if !isRoutineLadderFail {
			if err1 != nil {
				tui.LogEvent("[%s] ❌ Side 1 MARKET Fail: %v", id, err1)
			} else if res1 != nil && res1.Message != "" {
				tui.LogEvent("[%s] ❌ Side 1 MARKET Fail: %s", id, res1.Message)
			} else if res1 == nil {
				tui.LogEvent("[%s] ❌ Side 1 MARKET Fail: nil response", id)
			} else {
				tui.LogEvent("[%s] ❌ Side 1 MARKET Fail: unknown error (res=%v)", id, res1)
			}
			tui.LogEvent("[%s] 🧾 Side 1 submit: %s | fee=%dbps", id, directSubmittedOrderSummary(directMarketOrderSignalRequest{
				Side:        api.SideBuy,
				TokenID:     token0,
				Outcome:     outcomes[0],
				Price:       limitPrice1,
				Size:        func() float64 { if ladderedUSDCBuy { return directUSDCAmountForBuyShareCap(requestSize1, limitPrice1, ladderedUSDCBudget) }; return requestSize1 }(),
				FeeRateBps:  rate1,
				ExactShares: !ladderedUSDCBuy,
			}), rate1)
		}
		tui.RecordOrderWithMode(id, outcomes[0], "BUY", requestSize1, ask1, cost1, observedMargin, 0.0, executionMode, "FAILED")
	}

	if side2Requested && side2Success {
		if !ladderedMode {
			tui.LogEvent("[%s] ✅ Side 2 MARKET: %s (Observed $%.3f, Filled: %.2f/%.2f)", id, outcomes[1], ask2, filled2, requestSize2)
		}
		tui.RecordOrderWithMode(id, outcomes[1], "BUY", filled2, ask2, cost2, observedMargin, 0.0, executionMode, "FILLED")
	} else if side2Requested {
		isRoutineLadderFail := ladderedMode && err2 == nil && (res2 == nil || strings.TrimSpace(res2.Message) == "")
		if !isRoutineLadderFail {
			if err2 != nil {
				tui.LogEvent("[%s] ❌ Side 2 MARKET Fail: %v", id, err2)
			} else if res2 != nil && res2.Message != "" {
				tui.LogEvent("[%s] ❌ Side 2 MARKET Fail: %s", id, res2.Message)
			} else if res2 == nil {
				tui.LogEvent("[%s] ❌ Side 2 MARKET Fail: nil response", id)
			} else {
				tui.LogEvent("[%s] ❌ Side 2 MARKET Fail: unknown error (res=%v)", id, res2)
			}
			tui.LogEvent("[%s] 🧾 Side 2 submit: %s | fee=%dbps", id, directSubmittedOrderSummary(directMarketOrderSignalRequest{
				Side:        api.SideBuy,
				TokenID:     token1,
				Outcome:     outcomes[1],
				Price:       limitPrice2,
				Size:        func() float64 { if ladderedUSDCBuy { return directUSDCAmountForBuyShareCap(requestSize2, limitPrice2, ladderedUSDCBudget) }; return requestSize2 }(),
				FeeRateBps:  rate2,
				ExactShares: !ladderedUSDCBuy,
			}), rate2)
		}
		tui.RecordOrderWithMode(id, outcomes[1], "BUY", requestSize2, ask2, cost2, observedMargin, 0.0, executionMode, "FAILED")
	}

	if ladderedMode {
		activeOutcome := outcomes[0]
		activeExec := exec1
		activeSuccess := side1Success
		if ladderedDirection == 1 {
			activeOutcome = outcomes[1]
			activeExec = exec2
			activeSuccess = side2Success
		}
		if !activeSuccess && realbotShouldRetryLadderedBuyFailure(activeExec) {
			retryAt := time.Now().Add(realbotLadderedRetryInterval)
			if asyncResult.cooldownUntil.Before(retryAt) {
				asyncResult.cooldownUntil = retryAt
			}
			tui.LogEvent("[%s] 🔁 Ladder BUY retry armed for %s in %s after transient venue failure", id, activeOutcome, realbotLadderedRetryInterval)
		}
	}

	if !ladderedMode && side1Success != side2Success {
		if haveInitialSnapshot {
			tui.LogEvent("[%s] 🧾 Pre-trade share snapshot (%s): %s=%.4f, %s=%.4f", id, initialSnapshotSource, outcomes[0], initialSnapshot0, outcomes[1], initialSnapshot1)
		}
		tui.LogEvent("[%s] ⚠️ ARB LEGGED: %s=%v %s=%v — waiting 2s then re-verifying...",
			id, outcomes[0], side1Success, outcomes[1], side2Success)
		time.Sleep(2 * time.Second)

		var leggedAcquired0, leggedAcquired1, leggedBal0, leggedBal1 float64
		var leggedSource string
		reverifyCtx, cancelReverify := context.WithTimeout(ctx, 12*time.Second)
		var leggedErr error
		leggedAcquired0, leggedAcquired1, leggedBal0, leggedBal1, leggedSource, leggedErr = reconcileBoughtPairBalances(reverifyCtx, trader, token0, token1, initialSnapshot0, initialSnapshot1, haveInitialSnapshot)
		cancelReverify()
		if leggedErr != nil {
			tui.LogEvent("[%s] ⚠️ Re-verify failed: %v", id, leggedErr)
		}
		prevSide1, prevSide2 := side1Success, side2Success
		side1Success = prevSide1 || shouldAttemptCleanupSell(leggedAcquired0)
		side2Success = prevSide2 || shouldAttemptCleanupSell(leggedAcquired1)
		if shouldAttemptCleanupSell(leggedAcquired0) {
			filled1 = math.Max(filled1, leggedAcquired0)
		}
		if shouldAttemptCleanupSell(leggedAcquired1) {
			filled2 = math.Max(filled2, leggedAcquired1)
		}
		tui.LogEvent("[%s] 🔍 Re-verify after delay (%s): %s abs=%.4f Δ=%.4f (%v→%v), %s abs=%.4f Δ=%.4f (%v→%v)",
			id, leggedSource,
			outcomes[0], leggedBal0, leggedAcquired0, prevSide1, side1Success,
			outcomes[1], leggedBal1, leggedAcquired1, prevSide2, side2Success)

		if side1Success != side2Success {
			failedSide := outcomes[1]
			if !side1Success {
				failedSide = outcomes[0]
			}
			tui.LogEvent("[%s] ⚠️ ARB UNBALANCED: %s still not filled (legging to auto-cleanup)", id, failedSide)
		} else if side1Success && side2Success {
			tui.LogEvent("[%s] ✅ Legged position recovered via delayed settlement — both sides now filled (%.2f vs %.2f)", id, filled1, filled2)
		}
	}

	if ladderedMode {
		if ladderedDirection == 0 && side1Success {
			if shouldMirrorEngine {
				trader.RecordExecutionBuy(token0, filled1, cost1)
				_, _ = realbotMirrorLiveBuyIntoEngine(engine, id, outcomes[0], cost1, filled1)
			}
			advancedAnchor := realbotShouldAdvanceLadderedEntry(requestSize1, filled1)
			anchorNote := "anchor advanced"
			if !advancedAnchor {
				anchorNote = "anchor unchanged"
			}
			asyncResult.ladderedEntryConfirmed = advancedAnchor
			tui.LogEvent("[%s] 🪜 Ladder BUY confirmed: %s %s/%s @ $%.3f (%s)", id, outcomes[0], formatShareQty(filled1), formatShareQty(requestSize1), ask1, anchorNote)
			if recoveredLateLadderFill {
				tui.LogEvent("[%s] 🔄 Ladder late fill recovered via %s: %s %s", id, recoveredLateLadderSource, outcomes[0], formatShareQty(filled1))
			}
			if newBal, err := trader.ForceRefreshBalance(ctx); err == nil {
				realbotApplyRuntimeBalanceSync(engine, tui, newBal)
				realbotRefreshWalletCashDisplay(ctx, trader, tui, 8*time.Second)
			}
			refreshWalletTruth(5 * time.Second)
		} else if ladderedDirection == 1 && side2Success {
			if shouldMirrorEngine {
				trader.RecordExecutionBuy(token1, filled2, cost2)
				_, _ = realbotMirrorLiveBuyIntoEngine(engine, id, outcomes[1], cost2, filled2)
			}
			advancedAnchor := realbotShouldAdvanceLadderedEntry(requestSize2, filled2)
			anchorNote := "anchor advanced"
			if !advancedAnchor {
				anchorNote = "anchor unchanged"
			}
			asyncResult.ladderedEntryConfirmed = advancedAnchor
			tui.LogEvent("[%s] 🪜 Ladder BUY confirmed: %s %s/%s @ $%.3f (%s)", id, outcomes[1], formatShareQty(filled2), formatShareQty(requestSize2), ask2, anchorNote)
			if recoveredLateLadderFill {
				tui.LogEvent("[%s] 🔄 Ladder late fill recovered via %s: %s %s", id, recoveredLateLadderSource, outcomes[1], formatShareQty(filled2))
			}
			if newBal, err := trader.ForceRefreshBalance(ctx); err == nil {
				realbotApplyRuntimeBalanceSync(engine, tui, newBal)
				realbotRefreshWalletCashDisplay(ctx, trader, tui, 8*time.Second)
			}
			refreshWalletTruth(5 * time.Second)
		}
	} else if side1Success && side2Success {
		if shouldMirrorEngine {
			trader.RecordExecutionBuy(token0, filled1, cost1)
			trader.RecordExecutionBuy(token1, filled2, cost2)
			_, _ = realbotMirrorLiveBuyIntoEngine(engine, id, outcomes[0], cost1, filled1)
			_, _ = realbotMirrorLiveBuyIntoEngine(engine, id, outcomes[1], cost2, filled2)
		}

		settleCtx, settleCancel := context.WithTimeout(context.Background(), 12*time.Second)
		settleErr := settleMarketInventory(settleCtx, id, market, outcomes, tokenFeeRates, trader, engine, splitInventory, tui, restClient, true, rMinAsk, "POST BUY", realbotShouldAutoMergeBalancedInventory(realbotCfg), mergeCoordinator)
		settleCancel()
		if settleErr != nil {
			tui.LogEvent("[%s] ⚠️ Post-buy settlement still pending: %v", id, settleErr)
			asyncResult.cooldownUntil = time.Now().Add(10 * time.Second)
		} else if mergeCoordinator.pendingQty(id) >= minOnChainActionShares {
			tui.LogEvent("[%s] ✅ Buys verified. Merge continues in background while cleanup handles only the excess inventory.", id)
		} else {
			tui.LogEvent("[%s] ✅ Execution complete after verified buys. Applying 5s cooldown...", id)
		}

		if newBal, err := trader.ForceRefreshBalance(ctx); err == nil {
			realbotApplyRuntimeBalanceSync(engine, tui, newBal)
			realbotRefreshWalletCashDisplay(ctx, trader, tui, 8*time.Second)
		}
		refreshWalletTruth(5 * time.Second)
		time.Sleep(5 * time.Second)
	} else if side1Success || side2Success {
		if side1Success {
			if shouldMirrorEngine {
				trader.RecordExecutionBuy(token0, filled1, cost1)
				_, _ = realbotMirrorLiveBuyIntoEngine(engine, id, outcomes[0], cost1, filled1)
			}
			tui.LogEvent("[%s] ⚠️ Engine: Recording unbalanced position (only %s)", id, outcomes[0])
		}
		if side2Success {
			if shouldMirrorEngine {
				trader.RecordExecutionBuy(token1, filled2, cost2)
				_, _ = realbotMirrorLiveBuyIntoEngine(engine, id, outcomes[1], cost2, filled2)
			}
			tui.LogEvent("[%s] ⚠️ Engine: Recording unbalanced position (only %s)", id, outcomes[1])
		}

		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 60*time.Second)
		tui.LogEvent("[%s] ⚠️ Legged trade detected! Re-checking live/on-chain balances before cleanup...", id)

		acquired0, acquired1, bal0, bal1, balanceSource, balanceErr := reconcileBoughtPairBalances(cleanupCtx, trader, token0, token1, initialSnapshot0, initialSnapshot1, haveInitialSnapshot)
		if balanceErr != nil {
			tui.LogEvent("[%s] ⚠️ Cleanup balance reconciliation warning: %v", id, balanceErr)
		}
		if acquired0 >= minOnChainActionShares && acquired1 >= minOnChainActionShares {
			tui.LogEvent("[%s] 🟢 Cleanup balances ready (%s): %s abs=%.4f Δ=%.4f, %s abs=%.4f Δ=%.4f. Auto-merge disabled; proceeding to sell cleanup only.", id, balanceSource, outcomes[0], bal0, acquired0, outcomes[1], bal1, acquired1)
		}

		tui.LogEvent("[%s] 🧹 Auto-cleanup: Checking newly acquired shares to sell (%s)... %s abs=%.4f Δ=%.4f, %s abs=%.4f Δ=%.4f", id, balanceSource, outcomes[0], bal0, acquired0, outcomes[1], bal1, acquired1)

		cleanupSellPrice := core.CleanupSellLimitPrice(rMinAsk)
		var sell0Exec, sell1Exec directMarketExecution
		attemptSell0 := hasActionableCleanupRemainder(acquired0)
		attemptSell1 := hasActionableCleanupRemainder(acquired1)
		if attemptSell0 {
			quoteCtx, cancelQuote := context.WithTimeout(cleanupCtx, realbotExecQuoteTimeout)
			cleanupQuote, quoteErr := realbotBuildCleanupSellQuote(quoteCtx, restClient, token0, acquired0, rMinAsk)
			cancelQuote()
			if quoteErr != nil {
				tui.LogEvent("[%s] ⚠️ Auto-cleanup quote unavailable for %s: %v", id, outcomes[0], quoteErr)
			} else {
				if cleanupQuote.SubmitPrice+1e-9 < cleanupSellPrice {
					tui.LogEvent("[%s] 📡 Auto-cleanup repriced %s to live bid floor $%.3f (best bid $%.3f, age %s)", id, outcomes[0], cleanupQuote.SubmitPrice, cleanupQuote.BestBid, cleanupQuote.BookAge.Round(time.Millisecond))
				}
				if cleanupQuote.ExecutableQty+1e-9 < acquired0 {
					tui.LogEvent("[%s] ⚡ Auto-cleanup capped %s %s→%s on live bid liquidity %s", id, outcomes[0], formatShareQty(acquired0), formatShareQty(cleanupQuote.ExecutableQty), formatShareQty(cleanupQuote.TotalBidLiquidity))
				}
				tui.LogEvent("[%s] 🧹 Auto-cleanup: Market selling %s %s shares", id, formatShareQty(cleanupQuote.ExecutableQty), outcomes[0])
				sell0Exec = executeMarketOrderWithSignals(cleanupCtx, trader, api.SideSell, token0, outcomes[0], cleanupQuote.SubmitPrice, cleanupQuote.ExecutableQty, 0, acquired0, 2*time.Second)
			}
		}
		if attemptSell1 {
			quoteCtx, cancelQuote := context.WithTimeout(cleanupCtx, realbotExecQuoteTimeout)
			cleanupQuote, quoteErr := realbotBuildCleanupSellQuote(quoteCtx, restClient, token1, acquired1, rMinAsk)
			cancelQuote()
			if quoteErr != nil {
				tui.LogEvent("[%s] ⚠️ Auto-cleanup quote unavailable for %s: %v", id, outcomes[1], quoteErr)
			} else {
				if cleanupQuote.SubmitPrice+1e-9 < cleanupSellPrice {
					tui.LogEvent("[%s] 📡 Auto-cleanup repriced %s to live bid floor $%.3f (best bid $%.3f, age %s)", id, outcomes[1], cleanupQuote.SubmitPrice, cleanupQuote.BestBid, cleanupQuote.BookAge.Round(time.Millisecond))
				}
				if cleanupQuote.ExecutableQty+1e-9 < acquired1 {
					tui.LogEvent("[%s] ⚡ Auto-cleanup capped %s %s→%s on live bid liquidity %s", id, outcomes[1], formatShareQty(acquired1), formatShareQty(cleanupQuote.ExecutableQty), formatShareQty(cleanupQuote.TotalBidLiquidity))
				}
				tui.LogEvent("[%s] 🧹 Auto-cleanup: Market selling %s %s shares", id, formatShareQty(cleanupQuote.ExecutableQty), outcomes[1])
				sell1Exec = executeMarketOrderWithSignals(cleanupCtx, trader, api.SideSell, token1, outcomes[1], cleanupQuote.SubmitPrice, cleanupQuote.ExecutableQty, 0, acquired1, 2*time.Second)
			}
		}

		verifyCleanupCtx, cancelVerifyCleanup := context.WithTimeout(context.Background(), realbotCleanupVerifyTTL)
		remaining0, remaining1, resolvedBal0, resolvedBal1, resolvedSource, resolvedErr := waitForAcquiredCleanupResolution(verifyCleanupCtx, trader, token0, token1, initialSnapshot0, initialSnapshot1, haveInitialSnapshot)
		cancelVerifyCleanup()
		actualSold0 := math.Max(0, acquired0-remaining0)
		actualSold1 := math.Max(0, acquired1-remaining1)

		if hasActionableCleanupRemainder(actualSold0) {
			if shouldMirrorEngine {
				trader.RecordExecutionSell(token0, actualSold0)
				sellPrice0 := venueExecutionEffectivePrice(sell0Exec)
				if sellPrice0 <= 0 {
					sellPrice0 = cleanupSellPrice
				}
				proceeds0 := reportedSellProceeds(sell0Exec, sellPrice0, actualSold0, acquired0)
				if _, sellErr := realbotMirrorLiveSellIntoEngine(engine, id, outcomes[0], proceeds0, actualSold0); sellErr != nil {
					tui.LogEvent("[%s] ⚠️ Engine cleanup sync failed for %s: %v", id, outcomes[0], sellErr)
				}
			}
		}
		if hasActionableCleanupRemainder(actualSold1) {
			if shouldMirrorEngine {
				trader.RecordExecutionSell(token1, actualSold1)
				sellPrice1 := venueExecutionEffectivePrice(sell1Exec)
				if sellPrice1 <= 0 {
					sellPrice1 = cleanupSellPrice
				}
				proceeds1 := reportedSellProceeds(sell1Exec, sellPrice1, actualSold1, acquired1)
				if _, sellErr := realbotMirrorLiveSellIntoEngine(engine, id, outcomes[1], proceeds1, actualSold1); sellErr != nil {
					tui.LogEvent("[%s] ⚠️ Engine cleanup sync failed for %s: %v", id, outcomes[1], sellErr)
				}
			}
		}

		cleanupLoss := 0.0
		if hasActionableCleanupRemainder(actualSold0) {
			cleanupLoss += actualSold0 * (ask1 - cleanupSellPrice)
		}
		if hasActionableCleanupRemainder(actualSold1) {
			cleanupLoss += actualSold1 * (ask2 - cleanupSellPrice)
		}
		if cleanupLoss > 0 {
			trader.RecordLoss(cleanupLoss)
			tui.LogEvent("[%s] 📉 Cleanup loss recorded: $%.2f", id, cleanupLoss)
		}

		if hasActionableCleanupRemainder(remaining0) || hasActionableCleanupRemainder(remaining1) {
			if attemptSell0 && !sell0Exec.Success && sell0Exec.Result != nil && sell0Exec.Result.Message != "" {
				tui.LogEvent("[%s] ⚠️ Auto-cleanup sell still pending for %s: %s", id, outcomes[0], sell0Exec.Result.Message)
			}
			if attemptSell1 && !sell1Exec.Success && sell1Exec.Result != nil && sell1Exec.Result.Message != "" {
				tui.LogEvent("[%s] ⚠️ Auto-cleanup sell still pending for %s: %s", id, outcomes[1], sell1Exec.Result.Message)
			}
			if resolvedErr != nil {
				tui.LogEvent("[%s] ⚠️ Auto-cleanup balance recheck warning: %v", id, resolvedErr)
			}
			tui.LogEvent("[%s] 🚫 Auto-cleanup unresolved (%s): %s abs=%.4f Δ=%.4f, %s abs=%.4f Δ=%.4f. Applying 2m cooldown.", id, resolvedSource, outcomes[0], resolvedBal0, remaining0, outcomes[1], resolvedBal1, remaining1)
			asyncResult.cooldownUntil = time.Now().Add(120 * time.Second)
		} else {
			tui.LogEvent("[%s] ✅ Auto-cleanup verified flat (%s). Applying 30s cooldown before unblocking.", id, resolvedSource)
			asyncResult.cooldownUntil = time.Now().Add(30 * time.Second)
		}
		cancelCleanup()
	}

	if newBal, err := trader.ForceRefreshBalance(ctx); err == nil {
		realbotApplyRuntimeBalanceSync(engine, tui, newBal)
		realbotRefreshWalletCashDisplay(ctx, trader, tui, 8*time.Second)
	}
	refreshWalletTruth(5 * time.Second)
}
