package main

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	mkt "Market-bot/internal/markets"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

func realbotCopytradeHoldMode(cfg paper.TUISettings) bool {
	return strings.EqualFold(normalizePaperArbMode(cfg.PaperArbMode), paperArbModeCopytrade)
}

func realbotCopytradePollEvery(settings paper.TUISettings) time.Duration {
	pollEvery := time.Duration(settings.CopytradePollIntervalMs) * time.Millisecond
	if pollEvery <= 0 {
		pollEvery = 2 * time.Second
	}
	return pollEvery
}

func realbotNormalizedCopytradeUSDCBudget(liveCfg paper.TUISettings) float64 {
	budget := math.Round(liveCfg.CopytradeSizeUSDC*100.0) / 100.0
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

func realbotCanUseLocalCopytradeSellQuote(now time.Time, outcome string, tokenBids, tokenAsks map[string]float64, tokenFullBids map[string][]paper.MarketLevel, quoteState map[string]realbotQuoteState, maxAge time.Duration) (float64, string, bool) {
	bid := tokenBids[outcome]
	if bid <= 0 || bid >= 1.0 {
		return 0, fmt.Sprintf("missing local bid for %s", outcome), false
	}
	depth := tokenFullBids[outcome]
	if len(depth) == 0 {
		return 0, fmt.Sprintf("missing local bid depth for %s", outcome), false
	}
	bestBid, ok := realbotBestBidFromLevels(depth)
	if !ok || bestBid <= 0 || bestBid >= 1.0 {
		return 0, fmt.Sprintf("invalid local bid depth for %s", outcome), false
	}
	if math.Abs(bid-bestBid) > 0.0005 {
		return 0, fmt.Sprintf("local bid %.3f mismatches depth %.3f for %s", bid, bestBid, outcome), false
	}
	state, ok := quoteState[outcome]
	if !ok || state.UpdatedAt.IsZero() {
		return 0, fmt.Sprintf("missing quote timestamp for %s", outcome), false
	}
	if age := now.Sub(state.UpdatedAt); age > maxAge {
		return 0, fmt.Sprintf("%s quote age %s > %s", outcome, age.Round(time.Millisecond), maxAge), false
	}
	ask := tokenAsks[outcome]
	if ask > 0 && !realbotHasSaneTopOfBook(bid, ask) {
		return 0, fmt.Sprintf("crossed local quote for %s (bid %.3f >= ask %.3f)", outcome, bid, ask), false
	}
	return bid, "", true
}

func realbotHandleCopytradeMarket(ctx context.Context, marketID string, market *api.Market, outcomes []string, tokenBids, tokenAsks map[string]float64, tokenFullBids, tokenFullAsks map[string][]paper.MarketLevel, quoteState map[string]realbotQuoteState, tokenFeeRates map[string]int, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI, restClient *api.RestClient, liveCfg paper.TUISettings, poller *realbotCopytradePoller, state *realbotCopytradeState, entryGate *realbotEntryGate, refreshWalletTruth func(time.Duration)) {
	if restClient == nil || trader == nil || engine == nil || market == nil || state == nil || poller == nil {
		return
	}
	// If the user did not choose public-api but watchers are down, abort copytrading rather than silently falling back.
	if core.NormalizeCopytradeWatcherMode(liveCfg.CopytradeWatcherMode) != "public-api" && !realbotCopytradeHasWatcher(poller) {
		if state.lastError != "watchers_down" {
			state.lastError = "watchers_down"
			if tui != nil {
				tui.LogEvent("⚠️ Copytrade suspended: Watchers are inactive and public-api fallback is disabled")
			}
		}
		return
	}

	pollEvery := realbotCopytradePollEvery(liveCfg)
	polledTrades := make([]api.PublicTrade, 0)
	targetDeltas := make(map[string]float64)

	if realbotCopytradeShouldUsePublicActivityAPI(poller, liveCfg.CopytradeWatcherMode) {
		snapshot, err := poller.snapshotForCondition(ctx, restClient, pollEvery, market.ConditionID)
		if err != nil {
			state.lastError = fmt.Sprintf("snapshot: %v", err)
		} else {
			if snapshot.TradesErr != nil {
				state.lastError = fmt.Sprintf("trades: %v", snapshot.TradesErr)
			} else if snapshot.PositionsErr != nil {
				state.lastError = fmt.Sprintf("positions: %v", snapshot.PositionsErr)
			} else {
				state.lastError = ""
			}

			var freshTrades []api.PublicTrade
			if len(snapshot.Trades) > 0 {
				preparedTrades := realbotPrepareCopytradeTrades(snapshot.Trades, "public", liveCfg)
				freshTrades = realbotCopytradeFreshTrades(state, preparedTrades, market.ConditionID, liveCfg.CopytradeSizingMode)
			}

			syncTrades, deltas := realbotCopytradePositionSyncTrades(
				state,
				market.ConditionID,
				outcomes,
				snapshot.Positions,
				snapshot.PositionsPolledAt,
				freshTrades,
				liveCfg.CopytradeSizingMode,
			)
			polledTrades = append(freshTrades, syncTrades...)
			targetDeltas = deltas
		}
	} else {
		shouldPoll := state.lastTradeFetch.IsZero() || time.Since(state.lastTradeFetch) >= pollEvery
		if shouldPoll {
			since := state.lastTradeFetch
			state.lastTradeFetch = time.Now()
			if !since.IsZero() {
				since = since.Add(-10 * time.Second)
			}
			minedTrades := poller.minedSignalsForCondition(market.ConditionID, since)
			pendingTrades := poller.pendingSignalsForCondition(market.ConditionID, since)
			combinedTrades := realbotMergeCopytradeTrades(pendingTrades, minedTrades)
			if len(combinedTrades) > 0 {
				state.lastError = ""
				polledTrades = realbotCopytradeFreshTrades(state, combinedTrades, market.ConditionID, liveCfg.CopytradeSizingMode)
			}
		}

		if strings.EqualFold(liveCfg.CopytradeSizingMode, core.CopytradeSizingModePercent) {
			snapshot, err := poller.snapshotForCondition(ctx, restClient, pollEvery, market.ConditionID)
			if err == nil && snapshot.PositionsErr == nil {
				syncTrades, deltas := realbotCopytradePositionSyncTrades(
					state,
					market.ConditionID,
					outcomes,
					snapshot.Positions,
					snapshot.PositionsPolledAt,
					polledTrades,
					liveCfg.CopytradeSizingMode,
				)
				if len(syncTrades) > 0 {
					polledTrades = append(polledTrades, syncTrades...)
				}
				targetDeltas = deltas
			}
		}
	}

	for _, trade := range polledTrades {
		realbotObserveCopytradeBuySignal(state, trade)
	}

	freshTrades := make([]api.PublicTrade, 0, len(state.retryTrades)+len(polledTrades))
	if retries := realbotCopytradeTakeRetryTrades(state, time.Now()); len(retries) > 0 {
		freshTrades = append(freshTrades, retries...)
	}
	if len(polledTrades) > 0 {
		freshTrades = append(freshTrades, polledTrades...)
	}
	if len(freshTrades) == 0 {
		return
	}

	retryTrades := make([]api.PublicTrade, 0)
	requeueTrade := func(trade api.PublicTrade) {
		retryTrades = append(retryTrades, trade)
	}

	for _, trade := range freshTrades {
		outcome := core.SanitizeString(trade.Outcome)
		if outcome == "" {
			realbotLogCopytradeSignalResult(tui, marketID, trade, "⛔", "skipped: empty outcome")
			continue
		}
		localQty, avgPrice := localBoughtPositionAvg(engine, marketID, outcome)
		managed := state.managed[outcome]
		if localQty > 0.01 {
			managed = true
			state.managed[outcome] = true
		}
		tokenID := mkt.GetTokenIDForOutcome(market, outcome)
		if tokenID == "" {
			realbotLogCopytradeSignalResult(tui, marketID, trade, "⛔", fmt.Sprintf("skipped: outcome %s is not mapped to a token", outcome))
			continue
		}
		tradeSide := strings.ToUpper(strings.TrimSpace(trade.Side))
		tradeSize := math.Max(0, trade.Size)
		if tradeSize <= 0.01 && !strings.EqualFold(liveCfg.CopytradeSizingMode, core.CopytradeSizingModeShares) && !strings.EqualFold(liveCfg.CopytradeSizingMode, core.CopytradeSizingModeUSDC) {
			realbotLogCopytradeSignalResult(tui, marketID, trade, "⛔", fmt.Sprintf("skipped: master size %s is below %.2f share", formatShareQty(tradeSize), minOnChainActionShares))
			continue
		}

		if tradeSide == "BUY" {
			feeRate := realbotResolveFeeRateBps(tokenFeeRates, outcome, nil)
			ask := 0.0
			if localAsk, _, ok := realbotCanUseLocalTakerCloseQuote(time.Now(), outcome, tokenBids, tokenAsks, tokenFullAsks, quoteState, realbotTakerCloseLocalMaxAge); ok {
				ask = localAsk
			} else {
				restCtx, restCancel := context.WithTimeout(ctx, realbotTakerCloseRESTTimeout)
				_, restAsk, restErr := restClient.GetBestBidAsk(restCtx, tokenID)
				restCancel()
				if restErr != nil {
					realbotLogCopytradeSignalResult(tui, marketID, trade, "↩", fmt.Sprintf("requeued: quote refresh failed: %v", restErr))
					requeueTrade(trade)
					continue
				}
				ask = restAsk
			}
			if ask <= 0 || ask >= 1.0 {
				realbotLogCopytradeSignalResult(tui, marketID, trade, "↩", "requeued: missing valid ask")
				requeueTrade(trade)
				continue
			}
			if !realbotPriceWithinConfiguredRange(ask, liveCfg) {
				realbotLogCopytradeSignalResult(tui, marketID, trade, "⛔", "skipped: "+realbotConfiguredRangeReason("ask", ask, liveCfg))
				continue
			}
			if entryGate != nil && !entryGate.TryAcquire() {
				realbotLogCopytradeSignalResult(tui, marketID, trade, "↩", "requeued: another market is executing a live entry")
				requeueTrade(trade)
				continue
			}

			func() {
				if entryGate != nil {
					defer entryGate.Release()
				}
				submitPrice := realbotDirectionalBuyLimitPrice(ask, liveCfg.MaxAskPrice, liveCfg.CopytradeMaxSlippagePct)
				if submitPrice <= 0 || submitPrice >= 1.0 {
					realbotLogCopytradeSignalResult(tui, marketID, trade, "⛔", fmt.Sprintf("skipped: invalid slippage cap from ask $%.3f", ask))
					return
				}

				budgetShares := core.CalculateCopytradeSharesForMode(tradeSize, submitPrice, liveCfg.CopytradeSizeUSDC, liveCfg.CopytradeSizeShares, liveCfg.CopytradeSizePercent, liveCfg.MaxTradeSize, liveCfg.CopytradeSizingMode)
				requestedQty := normalizeMarketBuyShares(budgetShares)
				buyReq := directMarketOrderSignalRequest{
					Side:           api.SideBuy,
					TokenID:        tokenID,
					Outcome:        outcome,
					Price:          submitPrice,
					Size:           requestedQty,
					FeeRateBps:     feeRate,
					InitialBalance: trader.GetLivePositionSize(tokenID),
					ExactShares:    true,
				}
				if strings.EqualFold(liveCfg.CopytradeSizingMode, core.CopytradeSizingModeUSDC) {
					buyReq.Size = realbotNormalizedCopytradeUSDCBudget(liveCfg)
					buyReq.ExactShares = false
					requestedQty = directRequestedShareCap(buyReq)
				}
				if requestedQty < minOnChainActionShares {
					realbotLogCopytradeSignalResult(tui, marketID, trade, "⛔", fmt.Sprintf("skipped: actionable size %s is below %.2f share at cap $%.3f", formatShareQty(requestedQty), minOnChainActionShares, submitPrice))
					return
				}

				tradeCtx, tradeCancel := context.WithTimeout(ctx, 4*time.Second)
				exec := executeMarketOrderRequestWithSignals(tradeCtx, trader, buyReq, 2500*time.Millisecond)
				tradeCancel()
				logDirectExecutionAudit(tui, marketID, "Copytrade BUY", requestedQty, submitPrice, exec)
				if !exec.Success {
					if exec.Err != nil {
						realbotLogCopytradeSignalResult(tui, marketID, trade, "❌", fmt.Sprintf("failed: %v", exec.Err))
					} else if exec.Result != nil && exec.Result.Message != "" {
						realbotLogCopytradeSignalResult(tui, marketID, trade, "❌", fmt.Sprintf("failed: %s", exec.Result.Message))
					} else {
						realbotLogCopytradeSignalResult(tui, marketID, trade, "❌", "failed: execution did not succeed")
					}
					requeueTrade(trade)
					return
				}

				execQty := attributedBuyFill(exec, requestedQty, 0, false)
				if !hasConfirmedExecutedQty(api.SideBuy, execQty) {
					realbotLogCopytradeSignalResult(tui, marketID, trade, "⛔", "skipped: lacked confirmed fill")
					requeueTrade(trade)
					return
				}

				execPrice := venueExecutionEffectivePrice(exec)
				if execPrice <= 0 {
					execPrice = ask
				}
				execCost := reportedBuyCost(exec, execPrice, execQty, requestedQty)
				if realbotShouldMirrorExecutionIntoEngine(trader) {
					trader.RecordExecutionBuy(tokenID, execQty, execCost)
					if _, buyErr := realbotMirrorLiveBuyIntoEngine(engine, marketID, outcome, execCost, execQty); buyErr != nil {
						tui.LogEvent("[%s] ⚠️ Copytrade local buy sync failed for %s: %v", marketID, outcome, buyErr)
					}
				}
				state.managed[outcome] = true
				tui.RecordOrderWithMode(marketID, outcome, "BUY", execQty, execPrice, execCost, 0.0, 0.0, "copytrade", "FILLED")
				realbotLogCopytradeSignalResult(tui, marketID, trade, "✅", fmt.Sprintf("bought %s at $%.3f", formatShareQty(execQty), execPrice))
				if refreshWalletTruth != nil {
					refreshWalletTruth(5 * time.Second)
				}
			}()
			continue
		}

		if tradeSide == "SELL" {
			if !managed || localQty <= 0.01 {
				realbotLogCopytradeSignalResult(tui, marketID, trade, "⛔", "skipped: no managed local position to sell")
				continue
			}
			feeRate := realbotResolveFeeRateBps(tokenFeeRates, outcome, nil)
			bid := 0.0
			if localBid, _, ok := realbotCanUseLocalCopytradeSellQuote(time.Now(), outcome, tokenBids, tokenAsks, tokenFullBids, quoteState, realbotTakerCloseLocalMaxAge); ok {
				bid = localBid
			} else {
				restCtx, restCancel := context.WithTimeout(ctx, realbotTakerCloseRESTTimeout)
				restBid, _, restErr := restClient.GetBestBidAsk(restCtx, tokenID)
				restCancel()
				if restErr != nil {
					realbotLogCopytradeSignalResult(tui, marketID, trade, "↩", fmt.Sprintf("requeued: quote refresh failed: %v", restErr))
					requeueTrade(trade)
					continue
				}
				bid = restBid
			}
			if bid <= 0 || bid >= 1.0 {
				realbotLogCopytradeSignalResult(tui, marketID, trade, "↩", "requeued: missing valid bid")
				requeueTrade(trade)
				continue
			}
			if !realbotPriceWithinConfiguredRange(bid, liveCfg) {
				realbotLogCopytradeSignalResult(tui, marketID, trade, "⛔", "skipped: "+realbotConfiguredRangeReason("bid", bid, liveCfg))
				continue
			}
			submitPrice := realbotDirectionalSellFloorPrice(bid, liveCfg.MinAskPrice, liveCfg.CopytradeMaxSlippagePct)
			if submitPrice <= 0 || submitPrice >= 1.0 {
				realbotLogCopytradeSignalResult(tui, marketID, trade, "⛔", fmt.Sprintf("skipped: invalid slippage floor from bid $%.3f", bid))
				continue
			}

			requestedQty := 0.0
			targetQty := 0.0
			targetDelta := -tradeSize
			positionSignal := strings.HasPrefix(strings.ToLower(strings.TrimSpace(trade.Source)), "position")
			if state.targetSeen[outcome] {
				targetQty = state.targetShares[outcome]
				if positionSignal {
					targetDelta = -tradeSize
				} else if delta, ok := targetDeltas[outcome]; ok && delta < -0.01 {
					targetDelta = delta
					delete(targetDeltas, outcome)
				}
				requestedQty = normalizeMarketSellShares(core.CalculateCopytradeSellSharesForMode(localQty, targetQty, targetDelta, submitPrice, liveCfg.CopytradeSizeUSDC, liveCfg.CopytradeSizeShares, liveCfg.CopytradeSizePercent, liveCfg.MaxTradeSize, liveCfg.CopytradeSizingMode))
			} else {
				requestedQty = normalizeMarketSellShares(core.CalculateCopytradeSharesForMode(tradeSize, submitPrice, liveCfg.CopytradeSizeUSDC, liveCfg.CopytradeSizeShares, liveCfg.CopytradeSizePercent, liveCfg.MaxTradeSize, liveCfg.CopytradeSizingMode))
			}
			if requestedQty > localQty {
				requestedQty = normalizeMarketSellShares(localQty)
			}
			if requestedQty < minOnChainActionShares {
				realbotLogCopytradeSignalResult(tui, marketID, trade, "⛔", fmt.Sprintf("skipped: actionable size %s is below %.2f share", formatShareQty(requestedQty), minOnChainActionShares))
				continue
			}

			initialPosition := trader.GetLivePositionSize(tokenID)
			tradeCtx, tradeCancel := context.WithTimeout(ctx, 4*time.Second)
			exec := executeMarketOrderWithSignals(tradeCtx, trader, api.SideSell, tokenID, outcome, submitPrice, requestedQty, feeRate, initialPosition, 2500*time.Millisecond)
			tradeCancel()
			logDirectExecutionAudit(tui, marketID, "Copytrade SELL", requestedQty, submitPrice, exec)
			if !exec.Success {
				if exec.Err != nil {
					realbotLogCopytradeSignalResult(tui, marketID, trade, "❌", fmt.Sprintf("failed: %v", exec.Err))
				} else if exec.Result != nil && exec.Result.Message != "" {
					realbotLogCopytradeSignalResult(tui, marketID, trade, "❌", fmt.Sprintf("failed: %s", exec.Result.Message))
				} else {
					realbotLogCopytradeSignalResult(tui, marketID, trade, "❌", "failed: execution did not succeed")
				}
				requeueTrade(trade)
				continue
			}

			execQty := attributedSellFill(exec, requestedQty)
			if !hasConfirmedExecutedQty(api.SideSell, execQty) {
				realbotLogCopytradeSignalResult(tui, marketID, trade, "⛔", "skipped: lacked confirmed fill")
				requeueTrade(trade)
				continue
			}

			execPrice := venueExecutionEffectivePrice(exec)
			if execPrice <= 0 {
				execPrice = bid
			}
			execProceeds := reportedSellProceeds(exec, execPrice, execQty, requestedQty)
			if realbotShouldMirrorExecutionIntoEngine(trader) {
				trader.RecordExecutionSell(tokenID, execQty)
				if _, sellErr := realbotMirrorLiveSellIntoEngine(engine, marketID, outcome, execProceeds, execQty); sellErr != nil {
					tui.LogEvent("[%s] ⚠️ Copytrade local sell sync failed for %s: %v", marketID, outcome, sellErr)
				}
			}
			profit := execProceeds - (avgPrice * execQty)
			tui.RecordOrderWithMode(marketID, outcome, "SELL", execQty, execPrice, execProceeds, 0.0, profit, "copytrade", "FILLED")
			realbotLogCopytradeSignalResult(tui, marketID, trade, "✅", fmt.Sprintf("sold %s at $%.3f", formatShareQty(execQty), execPrice))
			if positionSignal {
				remainingSize := normalizeMarketSellShares(requestedQty - execQty)
				if remainingSize >= minOnChainActionShares {
					requeueTrade(api.PublicTrade{
						ConditionID: strings.TrimSpace(trade.ConditionID),
						Outcome:     outcome,
						Side:        "SELL",
						Size:        remainingSize,
						Timestamp:   trade.Timestamp,
						Source:      trade.Source,
					})
				}
			}
			if remainingQty, _ := localBoughtPositionAvg(engine, marketID, outcome); remainingQty <= 0.01 {
				state.managed[outcome] = false
			}
			if refreshWalletTruth != nil {
				refreshWalletTruth(5 * time.Second)
			}
			continue
		}

		realbotLogCopytradeSignalResult(tui, marketID, trade, "⛔", fmt.Sprintf("skipped: unsupported side %q", tradeSide))
	}
	realbotCopytradeQueueRetryTrades(state, retryTrades)
}
