package main

import (
	"context"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

type realbotMarketClosureArgs struct {
	marketID           string
	market             *api.Market
	endTime            time.Time
	outcomes           []string
	tokenFeeRates      map[string]int
	trader             *trading.RealTrader
	engine             *paper.Engine
	tui                *paper.TUI
	restClient         *api.RestClient
	splitInventory     *paper.SplitInventory
	mergeCoordinator   *realbotMergeCoordinator
	makerQuotes        map[string]*realbotMakerQuote
	refreshWalletTruth func(time.Duration)
	resolutionCache    *api.ResolutionCache
}

type realbotMarketClosureState struct {
	preserveWalletTruth *bool
}

func realbotLaunchRedemptionCheck(marketID, conditionID string, outcomes []string, marketEndTime time.Time, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI, resolutionCache *api.ResolutionCache) {
	go func() {
		checkRedemption(context.Background(), marketID, conditionID, append([]string(nil), outcomes...), marketEndTime, trader, engine, tui, resolutionCache)
	}()
}

func realbotHandleClosedMarket(args realbotMarketClosureArgs, state *realbotMarketClosureState) bool {
	if !time.Now().After(args.endTime.Add(5 * time.Second)) {
		return false
	}

	args.tui.LogEvent("[%s] ⏰ Closed", args.marketID)
	cancelMakerCtx, cancelMaker := context.WithTimeout(context.Background(), 10*time.Second)
	realbotCancelAllMakerQuotes(cancelMakerCtx, args.marketID, "market closed", args.trader, args.engine, args.tui, args.makerQuotes)
	cancelMaker()

	liveCfg := args.tui.GetSettings()
	if realbotTakerCloseHoldMode(liveCfg) {
		if realbotHasEnginePositionsForMarket(args.engine, args.marketID) {
			if state != nil && state.preserveWalletTruth != nil {
				*state.preserveWalletTruth = true
			}
			args.refreshWalletTruth(5 * time.Second)
			args.tui.LogEvent("[%s] ⏳ Taker-close inventory locked in; waiting for market resolution and redemption", args.marketID)
		}
		realbotLaunchRedemptionCheck(args.marketID, args.market.ConditionID, args.outcomes, args.endTime, args.trader, args.engine, args.tui, args.resolutionCache)
		return true
	}
	if realbotLadderedHoldMode(liveCfg) {
		if realbotHasEnginePositionsForMarket(args.engine, args.marketID) {
			if state != nil && state.preserveWalletTruth != nil {
				*state.preserveWalletTruth = true
			}
			args.refreshWalletTruth(5 * time.Second)
			args.tui.LogEvent("[%s] ⏳ Laddered inventory preserved at close; waiting for resolution/redemption instead of forced cleanup", args.marketID)
		}
		realbotLaunchRedemptionCheck(args.marketID, args.market.ConditionID, args.outcomes, args.endTime, args.trader, args.engine, args.tui, args.resolutionCache)
		return true
	}
	if realbotCopytradeHoldMode(liveCfg) {
		if realbotHasEnginePositionsForMarket(args.engine, args.marketID) {
			if state != nil && state.preserveWalletTruth != nil {
				*state.preserveWalletTruth = true
			}
			args.refreshWalletTruth(5 * time.Second)
			args.tui.LogEvent("[%s] ⏳ Copytrade inventory preserved at close; waiting for resolution/redemption instead of forced cleanup", args.marketID)
		}
		realbotLaunchRedemptionCheck(args.marketID, args.market.ConditionID, args.outcomes, args.endTime, args.trader, args.engine, args.tui, args.resolutionCache)
		return true
	}

	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 45*time.Second)
	if err := settleMarketInventory(cleanupCtx, args.marketID, args.market, args.outcomes, args.tokenFeeRates, args.trader, args.engine, args.splitInventory, args.tui, args.restClient, false, args.tui.GetSettings().MinAskPrice, "POST CLOSE", realbotShouldAutoMergeBalancedInventory(args.tui.GetSettings()), args.mergeCoordinator); err != nil {
		args.tui.LogEvent("[%s] ⚠️ Post-close cleanup skipped: %v", args.marketID, err)
	}
	cleanupCancel()
	realbotLaunchRedemptionCheck(args.marketID, args.market.ConditionID, args.outcomes, args.endTime, args.trader, args.engine, args.tui, args.resolutionCache)
	return true
}

type realbotMarketShutdownArgs struct {
	globalCtx          context.Context
	marketID           string
	market             *api.Market
	endTime            time.Time
	outcomes           []string
	tokenFeeRates      map[string]int
	trader             *trading.RealTrader
	engine             *paper.Engine
	tui                *paper.TUI
	restClient         *api.RestClient
	splitInventory     *paper.SplitInventory
	mergeCoordinator   *realbotMergeCoordinator
	makerQuotes        map[string]*realbotMakerQuote
	refreshWalletTruth func(time.Duration)
}

func realbotHandleMarketShutdown(args realbotMarketShutdownArgs, state *realbotMarketClosureState) bool {
	isShutdown := args.globalCtx.Err() != nil
	timeToExpiry := time.Until(args.endTime)
	liveCfg := args.tui.GetSettings()
	cancelMakerCtx, cancelMaker := context.WithTimeout(context.Background(), 10*time.Second)
	realbotCancelAllMakerQuotes(cancelMakerCtx, args.marketID, "trader stopping", args.trader, args.engine, args.tui, args.makerQuotes)
	cancelMaker()

	if realbotTakerCloseHoldMode(liveCfg) {
		if realbotHasEnginePositionsForMarket(args.engine, args.marketID) {
			if state != nil && state.preserveWalletTruth != nil {
				*state.preserveWalletTruth = true
			}
			args.tui.LogEvent("[%s] ⏳ Trader stopping: preserving taker-close inventory for post-resolution redemption", args.marketID)
		}
		return true
	}
	if realbotCopytradeHoldMode(liveCfg) {
		if realbotHasEnginePositionsForMarket(args.engine, args.marketID) {
			if state != nil && state.preserveWalletTruth != nil {
				*state.preserveWalletTruth = true
			}
			args.refreshWalletTruth(5 * time.Second)
			args.tui.LogEvent("[%s] ⏳ Trader stopping: preserving copytrade inventory for target-led exit or redemption", args.marketID)
		}
		return true
	}
	if realbotLadderedHoldMode(liveCfg) {
		if realbotHasEnginePositionsForMarket(args.engine, args.marketID) {
			if state != nil && state.preserveWalletTruth != nil {
				*state.preserveWalletTruth = true
			}
			args.refreshWalletTruth(5 * time.Second)
			args.tui.LogEvent("[%s] ⏳ Trader stopping: preserving laddered inventory for resolution/redemption", args.marketID)
		}
		return true
	}

	if !isShutdown && timeToExpiry > 30*time.Second {
		args.tui.LogEvent("[%s] ⚠️ TUI Restart: Preserving split inventory for next round", args.marketID)
		return true
	}

	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancelCleanup()
	if err := settleMarketInventory(cleanupCtx, args.marketID, args.market, args.outcomes, args.tokenFeeRates, args.trader, args.engine, args.splitInventory, args.tui, args.restClient, timeToExpiry > 2*time.Second, args.tui.GetSettings().MinAskPrice, "EMERGENCY EXIT", realbotShouldAutoMergeBalancedInventory(args.tui.GetSettings()), args.mergeCoordinator); err != nil {
		args.tui.LogEvent("[%s] ⚠️ Emergency cleanup failed: %v", args.marketID, err)
	}
	return true
}

type realbotMarketGuardArgs struct {
	marketID             string
	market               *api.Market
	endTime              time.Time
	outcomes             []string
	tokenFeeRates        map[string]int
	cfg                  *core.Config
	trader               *trading.RealTrader
	engine               *paper.Engine
	tui                  *paper.TUI
	restClient           *api.RestClient
	splitInventory       *paper.SplitInventory
	mergeCoordinator     *realbotMergeCoordinator
	takerCloseExecutedAt time.Time
}

type realbotMarketGuardState struct {
	usWeekdayGateClosedLogged *bool
	manualTradingPauseLogged  *bool
	nextNearCloseCleanup      *time.Time
	nearExpiryNoticeSent      *bool
}

type realbotMarketGuardResult struct {
	liveCfg               paper.TUISettings
	weekdayTradingAllowed bool
	manualTradingPaused   bool
	entryTradingAllowed   bool
	skip                  bool
}

func realbotHandleMarketGuards(args realbotMarketGuardArgs, state *realbotMarketGuardState) realbotMarketGuardResult {
	liveCfg := args.tui.GetSettings()
	usNow := core.USTime(time.Now())

	weekdayTradingAllowed := realbotTradingHoursAllowed(liveCfg)

	if !weekdayTradingAllowed {
		if state != nil && state.usWeekdayGateClosedLogged != nil && !*state.usWeekdayGateClosedLogged {
			args.tui.LogEvent("[%s] 🗓️ Trading gate closed at %s - new trades paused", args.marketID, usNow.Format("Mon 2006-01-02 15:04:05 MST"))
			*state.usWeekdayGateClosedLogged = true
		}
	} else if state != nil && state.usWeekdayGateClosedLogged != nil && *state.usWeekdayGateClosedLogged {
		args.tui.LogEvent("[%s] ✅ Trading gate open at %s - trading resumed", args.marketID, usNow.Format("Mon 2006-01-02 15:04:05 MST"))
		*state.usWeekdayGateClosedLogged = false
	}

	manualTradingPaused := args.tui.IsTradingPaused()
	if manualTradingPaused {
		if state != nil && state.manualTradingPauseLogged != nil && !*state.manualTradingPauseLogged {
			args.tui.LogEvent("[%s] ⏸️ Manual trading pause enabled - new trades paused", args.marketID)
			*state.manualTradingPauseLogged = true
		}
	} else if state != nil && state.manualTradingPauseLogged != nil && *state.manualTradingPauseLogged {
		args.tui.LogEvent("[%s] ▶️ Manual trading pause disabled - trading resumed", args.marketID)
		*state.manualTradingPauseLogged = false
	}

	entryTradingAllowed := weekdayTradingAllowed && !manualTradingPaused
	mergeBuffer := time.Duration(args.cfg.SplitMergeBufferSeconds) * time.Second
	timeToExpiry := time.Until(args.endTime)
	if weekdayTradingAllowed && !realbotCopytradeHoldMode(liveCfg) && !realbotLadderedHoldMode(liveCfg) && realbotShouldRunNearExpiryCleanup(liveCfg, timeToExpiry, mergeBuffer) {
		takerCloseCooldownActive := !args.takerCloseExecutedAt.IsZero() && time.Since(args.takerCloseExecutedAt) < 15*time.Second
		allowCleanupSell := !takerCloseCooldownActive
		if state != nil && state.nextNearCloseCleanup != nil && time.Now().After(*state.nextNearCloseCleanup) {
			if state.nearExpiryNoticeSent != nil && !*state.nearExpiryNoticeSent {
				if takerCloseCooldownActive {
					args.tui.LogEvent("[%s] ⏳ Near expiry: cleanup sell paused during taker close cooldown", args.marketID)
				} else {
					args.tui.LogEvent("[%s] ⏳ Near expiry: settling only", args.marketID)
				}
				*state.nearExpiryNoticeSent = true
			}
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := settleMarketInventory(cleanupCtx, args.marketID, args.market, args.outcomes, args.tokenFeeRates, args.trader, args.engine, args.splitInventory, args.tui, args.restClient, allowCleanupSell, args.tui.GetSettings().MinAskPrice, "NEAR EXPIRY", realbotShouldAutoMergeBalancedInventory(args.tui.GetSettings()), args.mergeCoordinator); err != nil {
				args.tui.LogEvent("[%s] ⚠️ Near-expiry cleanup failed: %v", args.marketID, err)
			}
			cleanupCancel()
			*state.nextNearCloseCleanup = time.Now().Add(5 * time.Second)
		}
		time.Sleep(250 * time.Millisecond)
		return realbotMarketGuardResult{
			liveCfg:               liveCfg,
			weekdayTradingAllowed: weekdayTradingAllowed,
			manualTradingPaused:   manualTradingPaused,
			entryTradingAllowed:   entryTradingAllowed,
			skip:                  true,
		}
	}

	return realbotMarketGuardResult{
		liveCfg:               liveCfg,
		weekdayTradingAllowed: weekdayTradingAllowed,
		manualTradingPaused:   manualTradingPaused,
		entryTradingAllowed:   entryTradingAllowed,
	}
}
