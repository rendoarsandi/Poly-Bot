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

type realbotPostQuoteIterationArgs struct {
	ctx                 context.Context
	marketID            string
	market              *api.Market
	endTime             time.Time
	outcomes            []string
	tokenMap            map[string]string
	tokenToOutcome      map[string]string
	tokenBids           map[string]float64
	tokenAsks           map[string]float64
	tokenFullBids       map[string][]paper.MarketLevel
	tokenFullAsks       map[string][]paper.MarketLevel
	quoteState          map[string]realbotQuoteState
	tokenFeeRates       map[string]int
	lastPairUpdate      time.Time
	polySignalTracker   *paper.DirectionalSignalTracker
	binanceFeed         *api.BinanceFuturesPriceFeed
	getTokenID          func(string) string
	trader              *trading.RealTrader
	engine              *paper.Engine
	riskMgr             *paper.RiskManager
	tui                 *paper.TUI
	restClient          *api.RestClient
	cfg                 *core.Config
	preQuoteLiveCfg     paper.TUISettings
	entryTradingAllowed bool
	timeToExpiry        time.Duration
	wsMgr               *api.WSManager
	wsChannelClosed     bool
	killSwitchActive    bool
	copytradePoller     *realbotCopytradePoller
	copytradeState      *realbotCopytradeState
	entryGate           *realbotEntryGate
	makerQuotes         map[string]*realbotMakerQuote
	mergeCoordinator    *realbotMergeCoordinator
	embeddedPaperMode   bool
	splitInventory      *paper.SplitInventory
	splitMu             *sync.Mutex
	splitTxMu           *sync.Mutex
	globalSplitStatus   map[string]bool
	globalInitialSplits map[string]float64
	replenishCtrl       *paper.ReplenishController
	refreshWalletTruth  func(time.Duration)
	entryExecutionDone  chan<- realbotAsyncEntryResult
}

type realbotPostQuoteIterationState struct {
	currentBalance             *float64
	lastPairUpdate             *time.Time
	lastEntryBlockReason       *string
	takerCloseAttempted        *bool
	takerCloseExecutedAt       *time.Time
	lastTakerCloseLog          *time.Time
	lastTakerCloseLogKey       *string
	lastTakerCloseQuoteRefresh *time.Time
	lastForceReconnect         *time.Time
	lastTrade                  *time.Time
	lastBinanceLog             *time.Time
	lastMakerSync              *time.Time
	nextSplitAttempt           *time.Time
	lastSplitSell              *time.Time
	panicBuyCooldown           *time.Time
	nextLiveRecoveryAttempt    *time.Time
	lastDustRecoveryNotice     *time.Time
	ladderedEntries            *[]realbotLadderedEntry
	nextLadderedEntrySeq       *uint64
	entryExecutionInFlight     *bool
}

func realbotHandlePostQuoteIteration(args realbotPostQuoteIterationArgs, state *realbotPostQuoteIterationState) bool {
	blockNewEntriesReason, blockNewEntries := realbotEntryBlockReason(args.marketID, args.engine, args.splitInventory, args.preQuoteLiveCfg)
	realbotHandleEntryBlockNotice(args.marketID, blockNewEntries, blockNewEntriesReason, args.tui, state.lastEntryBlockReason)

	if realbotHandleTakerCloseWindow(realbotTakerCloseStrategyArgs{
		ctx:                 args.ctx,
		marketID:            args.marketID,
		market:              args.market,
		outcomes:            args.outcomes,
		tokenMap:            args.tokenMap,
		tokenToOutcome:      args.tokenToOutcome,
		tokenBids:           args.tokenBids,
		tokenAsks:           args.tokenAsks,
		tokenFullAsks:       args.tokenFullAsks,
		quoteState:          args.quoteState,
		tokenFeeRates:       args.tokenFeeRates,
		liveCfg:             args.preQuoteLiveCfg,
		timeToExpiry:        args.timeToExpiry,
		entryTradingAllowed: args.entryTradingAllowed,
		blockNewEntries:     blockNewEntries,
		wsMgr:               args.wsMgr,
		wsChannelClosed:     args.wsChannelClosed,
		trader:              args.trader,
		engine:              args.engine,
		tui:                 args.tui,
		restClient:          args.restClient,
		entryGate:           args.entryGate,
		refreshWalletTruth:  args.refreshWalletTruth,
	}, &realbotTakerCloseStrategyState{
		takerCloseAttempted:        state.takerCloseAttempted,
		takerCloseExecutedAt:       state.takerCloseExecutedAt,
		lastTakerCloseLog:          state.lastTakerCloseLog,
		lastTakerCloseLogKey:       state.lastTakerCloseLogKey,
		lastTakerCloseQuoteRefresh: state.lastTakerCloseQuoteRefresh,
		lastForceReconnect:         state.lastForceReconnect,
	}) {
		return true
	}

	if args.killSwitchActive {
		return realbotPauseMarketLoop(args.marketID, "risk pause active", args.trader, args.engine, args.tui, args.makerQuotes, args.tui.GetSettings())
	}

	liveCfg := args.tui.GetSettings()
	arbMode := normalizePaperArbMode(liveCfg.PaperArbMode)
	primaryMode := realbotPrimaryExecutionMode(liveCfg)
	executionQuoteMaxAge := realbotExecutionQuoteGuardAge(core.ResolveExecutionLocalQuoteMaxAge(args.cfg))
	executionPairFresh := realbotShouldUseLocalPair(args.outcomes, args.tokenBids, args.tokenAsks, args.lastPairUpdate, executionQuoteMaxAge, time.Now())
	weekdayTradingAllowed := realbotTradingHoursAllowed(liveCfg)
	if !weekdayTradingAllowed {
		return realbotPauseMarketLoop(args.marketID, "trading gate closed", args.trader, args.engine, args.tui, args.makerQuotes, liveCfg)
	}
	manualTradingPaused := args.tui.IsTradingPaused()

	if realbotHandleLiveRecovery(realbotLiveRecoveryArgs{
		ctx:                args.ctx,
		marketID:           args.marketID,
		market:             args.market,
		outcomes:           args.outcomes,
		tokenFeeRates:      args.tokenFeeRates,
		primaryMode:        primaryMode,
		trader:             args.trader,
		engine:             args.engine,
		splitInventory:     args.splitInventory,
		tui:                args.tui,
		restClient:         args.restClient,
		liveCfg:            liveCfg,
		mergeCoordinator:   args.mergeCoordinator,
		refreshWalletTruth: args.refreshWalletTruth,
		lastTrade:          derefTime(state.lastTrade),
	}, &realbotLiveRecoveryState{
		currentBalance:          state.currentBalance,
		nextLiveRecoveryAttempt: state.nextLiveRecoveryAttempt,
		panicBuyCooldown:        state.panicBuyCooldown,
		lastDustRecoveryNotice:  state.lastDustRecoveryNotice,
	}) {
		return true
	}

	if realbotHandleModeDispatch(realbotModeDispatchArgs{
		ctx:                 args.ctx,
		marketID:            args.marketID,
		market:              args.market,
		endTime:             args.endTime,
		outcomes:            args.outcomes,
		tokenBids:           args.tokenBids,
		tokenAsks:           args.tokenAsks,
		tokenFullBids:       args.tokenFullBids,
		tokenFullAsks:       args.tokenFullAsks,
		quoteState:          args.quoteState,
		tokenFeeRates:       args.tokenFeeRates,
		lastPairUpdate:      args.lastPairUpdate,
		polySignalTracker:   args.polySignalTracker,
		currentBalance:      derefFloat(state.currentBalance),
		binanceFeed:         args.binanceFeed,
		getTokenID:          args.getTokenID,
		trader:              args.trader,
		engine:              args.engine,
		riskMgr:             args.riskMgr,
		tui:                 args.tui,
		restClient:          args.restClient,
		cfg:                 args.cfg,
		liveCfg:             liveCfg,
		primaryMode:         primaryMode,
		embeddedPaperMode:   args.embeddedPaperMode,
		manualTradingPaused: manualTradingPaused,
		executionPairFresh:  executionPairFresh,
		blockNewEntries:     blockNewEntries,
		copytradePoller:     args.copytradePoller,
		copytradeState:      args.copytradeState,
		entryGate:           args.entryGate,
		makerQuotes:         args.makerQuotes,
		lastMakerSync:       state.lastMakerSync,
		refreshWalletTruth:  args.refreshWalletTruth,
		lastTrade:           state.lastTrade,
		lastBinanceLog:      state.lastBinanceLog,
	}) {
		return true
	}

	skipPanicBuy := realbotHandleSplitStrategy(realbotSplitStrategyArgs{
		ctx:                  args.ctx,
		marketID:             args.marketID,
		conditionID:          args.market.ConditionID,
		outcomes:             args.outcomes,
		tokenBids:            args.tokenBids,
		tokenAsks:            args.tokenAsks,
		tokenFullBids:        args.tokenFullBids,
		tokenFeeRates:        args.tokenFeeRates,
		lastPairUpdate:       args.lastPairUpdate,
		currentBalance:       derefFloat(state.currentBalance),
		executionQuoteMaxAge: executionQuoteMaxAge,
		liveCfg:              liveCfg,
		cfg:                  args.cfg,
		trader:               args.trader,
		engine:               args.engine,
		tui:                  args.tui,
		embeddedPaperMode:    args.embeddedPaperMode,
		splitInventory:       args.splitInventory,
		splitMu:              args.splitMu,
		splitTxMu:            args.splitTxMu,
		globalSplitStatus:    args.globalSplitStatus,
		globalInitialSplits:  args.globalInitialSplits,
		replenishCtrl:        args.replenishCtrl,
		getTokenID:           args.getTokenID,
		refreshWalletTruth:   args.refreshWalletTruth,
		blockNewEntries:      blockNewEntries,
	}, &realbotSplitStrategyState{
		nextSplitAttempt: state.nextSplitAttempt,
		lastSplitSell:    state.lastSplitSell,
	})
	if skipPanicBuy {
		return true
	}

	if realbotHandlePanicBuyStrategy(realbotPanicBuyStrategyArgs{
		ctx:                  args.ctx,
		marketID:             args.marketID,
		market:               args.market,
		outcomes:             args.outcomes,
		tokenToOutcome:       args.tokenToOutcome,
		tokenBids:            args.tokenBids,
		tokenAsks:            args.tokenAsks,
		tokenFullBids:        args.tokenFullBids,
		tokenFullAsks:        args.tokenFullAsks,
		quoteState:           args.quoteState,
		tokenFeeRates:        args.tokenFeeRates,
		arbMode:              arbMode,
		currentBalance:       derefFloat(state.currentBalance),
		executionQuoteMaxAge: executionQuoteMaxAge,
		blockNewEntries:      blockNewEntries,
		trader:               args.trader,
		engine:               args.engine,
		riskMgr:              args.riskMgr,
		tui:                  args.tui,
		restClient:           args.restClient,
		cfg:                  args.cfg,
		splitInventory:       args.splitInventory,
		mergeCoordinator:     args.mergeCoordinator,
		refreshWalletTruth:   args.refreshWalletTruth,
		entryGate:            args.entryGate,
		entryExecutionDone:   args.entryExecutionDone,
	}, &realbotPanicBuyStrategyState{
		lastPairUpdate:         state.lastPairUpdate,
		ladderedEntries:        state.ladderedEntries,
		nextLadderedEntrySeq:   state.nextLadderedEntrySeq,
		panicBuyCooldown:       state.panicBuyCooldown,
		lastTrade:              state.lastTrade,
		lastDustRecoveryNotice: state.lastDustRecoveryNotice,
		entryExecutionInFlight: state.entryExecutionInFlight,
	}) {
		return true
	}

	return false
}

func derefFloat(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}
