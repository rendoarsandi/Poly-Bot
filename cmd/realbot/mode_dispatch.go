package main

import (
	"context"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

type realbotModeDispatchArgs struct {
	ctx                 context.Context
	marketID            string
	market              *api.Market
	endTime             time.Time
	outcomes            []string
	tokenBids           map[string]float64
	tokenAsks           map[string]float64
	tokenFullBids       map[string][]paper.MarketLevel
	tokenFullAsks       map[string][]paper.MarketLevel
	quoteState          map[string]realbotQuoteState
	tokenFeeRates       map[string]int
	lastPairUpdate      time.Time
	polySignalTracker   *paper.DirectionalSignalTracker
	currentBalance      float64
	binanceFeed         *api.BinanceFuturesPriceFeed
	getTokenID          func(string) string
	trader              *trading.RealTrader
	engine              *paper.Engine
	riskMgr             *paper.RiskManager
	tui                 *paper.TUI
	restClient          *api.RestClient
	cfg                 *core.Config
	liveCfg             paper.TUISettings
	primaryMode         string
	embeddedPaperMode   bool
	manualTradingPaused bool
	executionPairFresh  bool
	blockNewEntries     bool
	copytradePoller     *realbotCopytradePoller
	copytradeState      *realbotCopytradeState
	entryGate           *realbotEntryGate
	makerQuotes         map[string]*realbotMakerQuote
	lastMakerSync       *time.Time
	refreshWalletTruth  func(time.Duration)
	lastTrade           *time.Time
	lastBinanceLog      *time.Time
}

func realbotHandleModeDispatch(args realbotModeDispatchArgs) bool {
	if args.manualTradingPaused {
		pauseMakerCtx, pauseMakerCancel := context.WithTimeout(context.Background(), 5*time.Second)
		realbotCancelAllMakerQuotes(pauseMakerCtx, args.marketID, "manual trading pause active", args.trader, args.engine, args.tui, args.makerQuotes)
		pauseMakerCancel()
		time.Sleep(realbotTraderLoopInterval(args.liveCfg))
		return true
	}

	if args.primaryMode == realbotExecutionModeTakerClose {
		cancelMakerCtx, cancelMaker := context.WithTimeout(context.Background(), 5*time.Second)
		realbotCancelAllMakerQuotes(cancelMakerCtx, args.marketID, "taker close market enabled", args.trader, args.engine, args.tui, args.makerQuotes)
		cancelMaker()
		time.Sleep(realbotTraderLoopInterval(args.liveCfg))
		return true
	}

	if args.primaryMode == paperArbModeMaker {
		if !args.executionPairFresh {
			cancelMakerCtx, cancelMaker := context.WithTimeout(context.Background(), 5*time.Second)
			realbotCancelAllMakerQuotes(cancelMakerCtx, args.marketID, "waiting for fresh pair quotes", args.trader, args.engine, args.tui, args.makerQuotes)
			cancelMaker()
			time.Sleep(realbotTraderLoopInterval(args.liveCfg))
			return true
		}
		if args.blockNewEntries {
			cancelMakerCtx, cancelMaker := context.WithTimeout(context.Background(), 5*time.Second)
			realbotCancelAllMakerQuotes(cancelMakerCtx, args.marketID, "waiting for prior-round redemption", args.trader, args.engine, args.tui, args.makerQuotes)
			cancelMaker()
			time.Sleep(realbotTraderLoopInterval(args.liveCfg))
			return true
		}
		makerCtx, makerCancel := context.WithTimeout(args.ctx, 5*time.Second)
		maintainRealbotMakerQuotes(makerCtx, args.marketID, args.endTime, args.outcomes, args.getTokenID, args.tokenBids, args.tokenAsks, args.tokenFeeRates, args.trader, args.engine, args.riskMgr, args.tui, args.liveCfg, args.cfg, args.makerQuotes, args.lastMakerSync, args.binanceFeed)
		makerCancel()
		time.Sleep(realbotTraderLoopInterval(args.liveCfg))
		return true
	}

	cancelMakerCtx, cancelMaker := context.WithTimeout(context.Background(), 5*time.Second)
	realbotCancelAllMakerQuotes(cancelMakerCtx, args.marketID, "maker mode disabled", args.trader, args.engine, args.tui, args.makerQuotes)
	cancelMaker()

	if args.primaryMode == paperArbModeCopytrade {
		if args.blockNewEntries {
			time.Sleep(realbotTraderLoopInterval(args.liveCfg))
			return true
		}
		realbotHandleCopytradeMarket(args.ctx, args.marketID, args.market, args.outcomes, args.tokenBids, args.tokenAsks, args.tokenFullBids, args.tokenFullAsks, args.quoteState, args.tokenFeeRates, args.trader, args.engine, args.tui, args.restClient, args.liveCfg, args.copytradePoller, args.copytradeState, args.entryGate, args.refreshWalletTruth)
		time.Sleep(realbotTraderLoopInterval(args.liveCfg))
		return true
	}

	if args.primaryMode == paperArbModeBinanceGap {
		if !args.executionPairFresh || args.blockNewEntries {
			time.Sleep(realbotTraderLoopInterval(args.liveCfg))
			return true
		}
		realbotHandleBinanceGapMarket(args.ctx, args.marketID, args.outcomes, args.tokenBids, args.tokenAsks, args.tokenFullBids, args.tokenFullAsks, args.lastPairUpdate, args.polySignalTracker, args.tokenFeeRates, args.trader, args.engine, args.tui, args.liveCfg, args.cfg, args.currentBalance, args.binanceFeed, args.getTokenID, args.entryGate, args.lastTrade, args.lastBinanceLog)
		time.Sleep(realbotTraderLoopInterval(args.liveCfg))
		return true
	}

	if args.primaryMode != paperArbModeTaker && args.primaryMode != paperArbModeLaddered {
		time.Sleep(realbotTraderLoopInterval(args.liveCfg))
		return true
	}

	return false
}
