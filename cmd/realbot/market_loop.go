package main

import (
	"context"
	"sync"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

func realbotWaitForMarketWake(args realbotMarketQuoteArgs, timeout time.Duration, lastPairUpdate *time.Time, entryExecutionDone <-chan realbotAsyncEntryResult, entryState *realbotAsyncEntryState, wsChannelClosed *bool) {
	if timeout <= 0 {
		timeout = time.Millisecond
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-args.ctx.Done():
		return
	case result, ok := <-entryExecutionDone:
		if ok {
			realbotApplyAsyncEntryResult(result, entryState)
		}
		return
	case msg, ok := <-args.wsMsgChan:
		if !ok {
			if wsChannelClosed != nil {
				*wsChannelClosed = true
			}
			return
		}
		if wsChannelClosed != nil {
			*wsChannelClosed = false
		}
		realbotHandleMarketWSMessage(args, msg, lastPairUpdate)
		return
	case <-timer.C:
		return
	}
}

func tradeMarket(globalCtx context.Context, ctx context.Context, id string, market *api.Market, endTime time.Time,
	trader *trading.RealTrader, engine *paper.Engine, orderBook *paper.OrderBook,
	riskMgr *paper.RiskManager, tui *paper.TUI, restClient *api.RestClient, cfg *core.Config, startingBalance float64,
	copytradePoller *realbotCopytradePoller,
	globalSplitStatus map[string]bool, globalSplitInventories map[string]*paper.SplitInventory, globalInitialSplits map[string]float64, splitMu *sync.Mutex, splitTxMu *sync.Mutex, entryGate *realbotEntryGate, ladderCloseState *realbotLadderCloseState, resolutionCache *api.ResolutionCache) {

	session, err := realbotInitMarketSession(ctx, id, market, trader, restClient, cfg, tui)
	if err != nil {
		return
	}
	defer session.wsMgr.Close()

	tokenMap := session.tokenMap
	tokenToOutcome := session.tokenToOutcome
	outcomeToToken := session.outcomeToToken
	outcomes := session.outcomes
	wsMgr := session.wsMgr
	wsMsgChan := session.wsMsgChan
	tokenFeeRates := session.tokenFeeRates
	runtimeState := realbotInitMarketRuntime(ctx, id, market.ConditionID, tokenToOutcome, trader, engine, tui, cfg, globalSplitInventories, splitMu, ladderCloseState)

	tokenBids := make(map[string]float64)
	tokenAsks := make(map[string]float64)
	tokenFullBids := make(map[string][]paper.MarketLevel)
	tokenFullAsks := make(map[string][]paper.MarketLevel)
	displayBids := make(map[string]float64)
	displayAsks := make(map[string]float64)
	publishedBids := make(map[string]float64)
	publishedAsks := make(map[string]float64)
	quoteState := make(map[string]realbotQuoteState)
	polySignalTracker := paper.NewDirectionalSignalTracker(core.ResolveBinanceSignalLookback(cfg), outcomes)
	lastPublishedQuoteAt := time.Time{}
	lastTrade := time.Time{}
	lastPairUpdate := time.Time{}
	var ladderedEntries []realbotLadderedEntry
	var nextLadderedEntrySeq uint64
	ladderedStartupStableAt := time.Time{}
	ladderedStartupSide := -1
	ladderedStartupRung := -1
	lastBinanceLog := time.Time{}
	lastSplitSell := time.Time{}
	nextSplitAttempt := time.Time{}
	var panicBuyCooldown time.Time
	var nextLiveRecoveryAttempt time.Time
	var lastDustRecoveryNotice time.Time
	entryExecutionDone := runtimeState.entryExecutionDone
	entryExecutionInFlight := false
	makerQuotes := make(map[string]*realbotMakerQuote)
	lastMakerSync := time.Time{}
	mergeCoordinator := runtimeState.mergeCoordinator
	lastEntryBlockReason := ""

	currentBalance := startingBalance

	getTokenID := func(outcome string) string { return outcomeToToken[outcome] }
	embeddedPaperMode := runtimeState.embeddedPaperMode
	binanceFeed := runtimeState.binanceFeed
	splitInventory := runtimeState.splitInventory
	takerCloseAttempted := false
	var takerCloseExecutedAt time.Time
	var lastTakerCloseLog time.Time
	var lastTakerCloseLogKey string
	var lastTakerCloseQuoteRefresh time.Time
	tradingGateClosedLogged := false
	manualTradingPauseLogged := false
	preserveWalletTruth := false
	defer func() {
		realbotClearWalletTruthOnExit(tui, id, preserveWalletTruth)
	}()
	replenishCtrl := runtimeState.replenishCtrl
	var nextNearCloseCleanup time.Time
	var nearExpiryNoticeSent bool
	refreshWalletTruth := runtimeState.refreshWalletTruth
	copytradeState := runtimeState.copytradeState

	lastReconnectCount := int32(0)
	lastWsWarnTime := time.Time{}
	lastForceReconnect := time.Time{}
	lastRestFallbackPoll := time.Time{}
	lastTelemetryUpdate := time.Time{}
	restFallbackActive := false
	restRecoveryLogged := false
	wsChannelClosed := false
	restFallbackQuoteAge := runtimeState.restFallbackQuoteAge
	restFallbackPollInterval := runtimeState.restFallbackPollPeriod
	lastDecisionEvalAt := time.Time{}
	lastDecisionQuoteAt := time.Time{}

	for {
		currentBalance = engine.GetBalance()
		realbotConsumeAsyncEntryResult(entryExecutionDone, &realbotAsyncEntryState{
			entryExecutionInFlight: &entryExecutionInFlight,
			ladderedEntries:        &ladderedEntries,
			lastTrade:              &lastTrade,
			panicBuyCooldown:       &panicBuyCooldown,
		})
		select {
		case <-ctx.Done():
			realbotHandleMarketShutdown(realbotMarketShutdownArgs{
				globalCtx:          globalCtx,
				marketID:           id,
				market:             market,
				endTime:            endTime,
				outcomes:           outcomes,
				tokenFeeRates:      tokenFeeRates,
				trader:             trader,
				engine:             engine,
				tui:                tui,
				restClient:         restClient,
				splitInventory:     splitInventory,
				mergeCoordinator:   mergeCoordinator,
				makerQuotes:        makerQuotes,
				refreshWalletTruth: refreshWalletTruth,
			}, &realbotMarketClosureState{
				preserveWalletTruth: &preserveWalletTruth,
			})
			return
		default:
		}

		if realbotHandleClosedMarket(realbotMarketClosureArgs{
			ladderCloseState:   runtimeState.ladderCloseState,
			marketID:           id,
			market:             market,
			endTime:            endTime,
			outcomes:           outcomes,
			tokenFeeRates:      tokenFeeRates,
			trader:             trader,
			engine:             engine,
			tui:                tui,
			restClient:         restClient,
			splitInventory:     splitInventory,
			mergeCoordinator:   mergeCoordinator,
			makerQuotes:        makerQuotes,
			refreshWalletTruth: refreshWalletTruth,
			resolutionCache:    resolutionCache,
		}, &realbotMarketClosureState{
			preserveWalletTruth: &preserveWalletTruth,
		}) {
			return
		}

		timeToExpiry := time.Until(endTime)
		guardResult := realbotHandleMarketGuards(realbotMarketGuardArgs{
			marketID:             id,
			market:               market,
			endTime:              endTime,
			outcomes:             outcomes,
			tokenFeeRates:        tokenFeeRates,
			cfg:                  cfg,
			trader:               trader,
			engine:               engine,
			tui:                  tui,
			restClient:           restClient,
			splitInventory:       splitInventory,
			mergeCoordinator:     mergeCoordinator,
			takerCloseExecutedAt: takerCloseExecutedAt,
		}, &realbotMarketGuardState{
			tradingGateClosedLogged:  &tradingGateClosedLogged,
			manualTradingPauseLogged: &manualTradingPauseLogged,
			nextNearCloseCleanup:     &nextNearCloseCleanup,
			nearExpiryNoticeSent:     &nearExpiryNoticeSent,
		})
		liveCfg := guardResult.liveCfg
		entryTradingAllowed := guardResult.entryTradingAllowed
		if guardResult.skip {
			continue
		}

		killSwitchActive := riskMgr.IsKillSwitchTriggered()

		quoteArgs := realbotMarketQuoteArgs{
			ctx:                    ctx,
			marketID:               id,
			wsMgr:                  wsMgr,
			wsMsgChan:              wsMsgChan,
			tokenMap:               tokenMap,
			tokenToOutcome:         tokenToOutcome,
			outcomes:               outcomes,
			tokenBids:              tokenBids,
			tokenAsks:              tokenAsks,
			tokenFullBids:          tokenFullBids,
			tokenFullAsks:          tokenFullAsks,
			displayBids:            displayBids,
			displayAsks:            displayAsks,
			publishedBids:          publishedBids,
			publishedAsks:          publishedAsks,
			quoteState:             quoteState,
			polySignalTracker:      polySignalTracker,
			engine:                 engine,
			restClient:             restClient,
			tui:                    tui,
			restFallbackQuoteAge:   restFallbackQuoteAge,
			restFallbackPollPeriod: restFallbackPollInterval,
		}
		if realbotProcessMarketQuotes(quoteArgs, realbotMarketQuoteRuntime{
			lastPairUpdate:       &lastPairUpdate,
			lastPublishedQuoteAt: &lastPublishedQuoteAt,
			lastReconnectCount:   &lastReconnectCount,
			lastWsWarnTime:       &lastWsWarnTime,
			lastForceReconnect:   &lastForceReconnect,
			lastRestFallbackPoll: &lastRestFallbackPoll,
			lastTelemetryUpdate:  &lastTelemetryUpdate,
			restFallbackActive:   &restFallbackActive,
			restRecoveryLogged:   &restRecoveryLogged,
			wsChannelClosed:      &wsChannelClosed,
		}) {
			return
		}

		now := time.Now()
		latestQuoteAt, _ := realbotLatestQuoteUpdate(outcomes, quoteState)
		decisionInterval := realbotDecisionEvalInterval(liveCfg, timeToExpiry, entryExecutionInFlight)
		if !realbotShouldRunDecisionLoop(now, lastDecisionEvalAt, lastDecisionQuoteAt, latestQuoteAt, decisionInterval) {
			realbotWaitForMarketWake(quoteArgs, decisionInterval, &lastPairUpdate, entryExecutionDone, &realbotAsyncEntryState{
				entryExecutionInFlight: &entryExecutionInFlight,
				ladderedEntries:        &ladderedEntries,
				lastTrade:              &lastTrade,
				panicBuyCooldown:       &panicBuyCooldown,
			}, &wsChannelClosed)
			continue
		}
		lastDecisionEvalAt = now
		if latestQuoteAt.After(lastDecisionQuoteAt) {
			lastDecisionQuoteAt = latestQuoteAt
		}

		if realbotHandlePostQuoteIteration(realbotPostQuoteIterationArgs{
			ctx:                 ctx,
			ladderCloseState:    runtimeState.ladderCloseState,
			marketID:            id,
			market:              market,
			endTime:             endTime,
			outcomes:            outcomes,
			tokenMap:            tokenMap,
			tokenToOutcome:      tokenToOutcome,
			tokenBids:           tokenBids,
			tokenAsks:           tokenAsks,
			tokenFullBids:       tokenFullBids,
			tokenFullAsks:       tokenFullAsks,
			quoteState:          quoteState,
			tokenFeeRates:       tokenFeeRates,
			lastPairUpdate:      lastPairUpdate,
			polySignalTracker:   polySignalTracker,
			binanceFeed:         binanceFeed,
			getTokenID:          getTokenID,
			trader:              trader,
			engine:              engine,
			riskMgr:             riskMgr,
			tui:                 tui,
			restClient:          restClient,
			cfg:                 cfg,
			preQuoteLiveCfg:     liveCfg,
			entryTradingAllowed: entryTradingAllowed,
			timeToExpiry:        timeToExpiry,
			wsMgr:               wsMgr,
			wsChannelClosed:     wsChannelClosed,
			killSwitchActive:    killSwitchActive,
			copytradePoller:     copytradePoller,
			copytradeState:      copytradeState,
			entryGate:           entryGate,
			makerQuotes:         makerQuotes,
			mergeCoordinator:    mergeCoordinator,
			embeddedPaperMode:   embeddedPaperMode,
			splitInventory:      splitInventory,
			splitMu:             splitMu,
			splitTxMu:           splitTxMu,
			globalSplitStatus:   globalSplitStatus,
			globalInitialSplits: globalInitialSplits,
			replenishCtrl:       replenishCtrl,
			refreshWalletTruth:  refreshWalletTruth,
			entryExecutionDone:  entryExecutionDone,
		}, &realbotPostQuoteIterationState{
			currentBalance:             &currentBalance,
			lastPairUpdate:             &lastPairUpdate,
			lastEntryBlockReason:       &lastEntryBlockReason,
			takerCloseAttempted:        &takerCloseAttempted,
			takerCloseExecutedAt:       &takerCloseExecutedAt,
			lastTakerCloseLog:          &lastTakerCloseLog,
			lastTakerCloseLogKey:       &lastTakerCloseLogKey,
			lastTakerCloseQuoteRefresh: &lastTakerCloseQuoteRefresh,
			lastForceReconnect:         &lastForceReconnect,
			lastTrade:                  &lastTrade,
			lastBinanceLog:             &lastBinanceLog,
			lastMakerSync:              &lastMakerSync,
			nextSplitAttempt:           &nextSplitAttempt,
			lastSplitSell:              &lastSplitSell,
			panicBuyCooldown:           &panicBuyCooldown,
			nextLiveRecoveryAttempt:    &nextLiveRecoveryAttempt,
			lastDustRecoveryNotice:     &lastDustRecoveryNotice,
			ladderedEntries:            &ladderedEntries,
			nextLadderedEntrySeq:       &nextLadderedEntrySeq,
			entryExecutionInFlight:     &entryExecutionInFlight,
			ladderedStartupStableAt:    &ladderedStartupStableAt,
			ladderedStartupSide:        &ladderedStartupSide,
			ladderedStartupRung:        &ladderedStartupRung,
		}) {
			continue
		}

		realbotWaitForMarketWake(quoteArgs, decisionInterval, &lastPairUpdate, entryExecutionDone, &realbotAsyncEntryState{
			entryExecutionInFlight: &entryExecutionInFlight,
			ladderedEntries:        &ladderedEntries,
			lastTrade:              &lastTrade,
			panicBuyCooldown:       &panicBuyCooldown,
		}, &wsChannelClosed)
	}
}
