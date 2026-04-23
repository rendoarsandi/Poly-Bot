package main

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

func realbotTakerCloseHoldMode(cfg paper.TUISettings) bool {
	return paper.TakerCloseModeActive(cfg)
}

func realbotTakerCloseBudget(cash, sizingCapital float64, liveCfg paper.TUISettings) float64 {
	if cash <= 0 && sizingCapital <= 0 {
		return 0
	}

	sizingBase := sizingCapital
	if sizingBase <= 0 {
		sizingBase = cash
	}
	if cash > sizingBase {
		sizingBase = cash
	}

	return core.CalculateTradeSizeForMode(
		sizingBase,
		liveCfg.TradeScaleFactor,
		liveCfg.TradeSizeUSDC,
		liveCfg.MaxTradeSize,
		liveCfg.TradeSizingMode,
	)
}

func realbotBestTakerCloseOutcomePrice(outcomes []string, bids, asks map[string]float64) (string, float64) {
	bestOutcome := ""
	highestPrice := 0.0
	for _, outcome := range outcomes {
		price := bids[outcome]
		if price <= 0 || price >= 1.0 {
			price = asks[outcome]
		}
		if price > 0 && price <= 1.0 && price > highestPrice {
			highestPrice = price
			bestOutcome = outcome
		}
	}
	return bestOutcome, highestPrice
}

func normalizedRealbotTakerCloseMinPrice(liveCfg paper.TUISettings) float64 {
	minPrice := liveCfg.TakerCloseMarketMinPrice
	if minPrice <= 0 || minPrice >= 1.0 {
		minPrice = 0.60
	}
	minPrice = math.Round(minPrice*100.0) / 100.0
	if minPrice < 0.01 {
		return 0.01
	}
	if minPrice > 0.99 {
		return 0.99
	}
	return minPrice
}

func realbotShouldLogTakerCloseState(lastAt *time.Time, lastKey *string, nextKey string, interval time.Duration) bool {
	if lastKey == nil || lastAt == nil {
		return true
	}
	if *lastKey != nextKey {
		*lastKey = nextKey
		*lastAt = time.Now()
		return true
	}
	return false
}

func realbotMarkTakerCloseStateLogged(lastAt *time.Time, lastKey *string, key string) {
	if lastKey != nil {
		*lastKey = key
	}
	if lastAt != nil {
		*lastAt = time.Now()
	}
}

type realbotTakerClosePlan struct {
	LimitPrice   float64
	MinPrice     float64
	SizingPrice  float64
	RequestedQty float64
}

func buildRealbotTakerClosePlan(budget, confirmedPrice float64, liveCfg paper.TUISettings) (realbotTakerClosePlan, error) {
	if budget <= 0 {
		return realbotTakerClosePlan{}, fmt.Errorf("budget must be positive")
	}
	if confirmedPrice <= 0 || confirmedPrice >= 1.0 {
		return realbotTakerClosePlan{}, fmt.Errorf("confirmed price %.3f is invalid", confirmedPrice)
	}

	minPrice := normalizedRealbotTakerCloseMinPrice(liveCfg)
	if confirmedPrice+1e-9 < minPrice {
		return realbotTakerClosePlan{}, fmt.Errorf("confirmed price %.3f is below taker-close min %.3f", confirmedPrice, minPrice)
	}
	limitPrice := liveCfg.TakerCloseMarketSlippage
	if limitPrice <= 0 || limitPrice >= 1.0 {
		limitPrice = 0.99
	}
	if limitPrice < minPrice {
		limitPrice = minPrice
	}

	sizingPrice := limitPrice
	if sizingPrice <= 0 || sizingPrice >= 1.0 {
		return realbotTakerClosePlan{}, fmt.Errorf("sizing price %.3f is invalid", sizingPrice)
	}

	requestedQty := normalizeMarketBuyShares(budget / sizingPrice)
	requestedQty = realbotClampBuySharesToBudget(requestedQty, budget, limitPrice)
	if requestedQty < 1 {
		return realbotTakerClosePlan{}, fmt.Errorf("budget $%.2f is too small at sizing price $%.3f", budget, sizingPrice)
	}

	return realbotTakerClosePlan{
		LimitPrice:   limitPrice,
		MinPrice:     minPrice,
		SizingPrice:  sizingPrice,
		RequestedQty: requestedQty,
	}, nil
}

func realbotCanUseLocalTakerCloseQuote(now time.Time, outcome string, tokenBids, tokenAsks map[string]float64, tokenFullAsks map[string][]paper.MarketLevel, quoteState map[string]realbotQuoteState, maxAge time.Duration) (float64, string, bool) {
	ask := tokenAsks[outcome]
	if ask <= 0 || ask >= 1.0 {
		return 0, fmt.Sprintf("missing local ask for %s", outcome), false
	}
	depth := tokenFullAsks[outcome]
	if len(depth) == 0 {
		return 0, fmt.Sprintf("missing local ask depth for %s", outcome), false
	}
	bestAsk, ok := realbotBestAskFromLevels(depth)
	if !ok || bestAsk <= 0 || bestAsk >= 1.0 {
		return 0, fmt.Sprintf("invalid local ask depth for %s", outcome), false
	}
	if math.Abs(bestAsk-ask) > 0.0005 {
		return 0, fmt.Sprintf("local ask %.3f mismatches depth %.3f for %s", ask, bestAsk, outcome), false
	}
	state, ok := quoteState[outcome]
	if !ok || state.UpdatedAt.IsZero() {
		return 0, fmt.Sprintf("missing quote timestamp for %s", outcome), false
	}
	age := now.Sub(state.UpdatedAt)
	if age > maxAge {
		return 0, fmt.Sprintf("%s quote age %s > %s", outcome, age.Round(time.Millisecond), maxAge), false
	}
	source := strings.ToLower(strings.TrimSpace(state.Source))
	if source != "ws" && source != "ws-bbo" {
		return 0, fmt.Sprintf("quote source %s not aggressive-safe for %s", state.Source, outcome), false
	}
	bid := tokenBids[outcome]
	if bid > 0 && !realbotHasSaneTopOfBook(bid, ask) {
		return 0, fmt.Sprintf("crossed local quote for %s (bid %.3f >= ask %.3f)", outcome, bid, ask), false
	}
	return ask, "", true
}

type realbotTakerCloseStrategyArgs struct {
	ctx                 context.Context
	marketID            string
	market              *api.Market
	outcomes            []string
	tokenMap            map[string]string
	tokenToOutcome      map[string]string
	tokenBids           map[string]float64
	tokenAsks           map[string]float64
	tokenFullAsks       map[string][]paper.MarketLevel
	quoteState          map[string]realbotQuoteState
	tokenFeeRates       map[string]int
	liveCfg             paper.TUISettings
	timeToExpiry        time.Duration
	entryTradingAllowed bool
	blockNewEntries     bool
	wsMgr               *api.WSManager
	wsChannelClosed     bool
	trader              *trading.RealTrader
	engine              *paper.Engine
	tui                 *paper.TUI
	restClient          *api.RestClient
	entryGate           *realbotEntryGate
	refreshWalletTruth  func(time.Duration)
}

type realbotTakerCloseStrategyState struct {
	takerCloseAttempted        *bool
	takerCloseExecutedAt       *time.Time
	lastTakerCloseLog          *time.Time
	lastTakerCloseLogKey       *string
	lastTakerCloseQuoteRefresh *time.Time
	lastForceReconnect         *time.Time
}

func realbotTokenIDForOutcome(tokenMap map[string]string, outcome string) string {
	for tokenID, mappedOutcome := range tokenMap {
		if mappedOutcome == outcome {
			return tokenID
		}
	}
	return ""
}

func realbotHandleTakerCloseWindow(args realbotTakerCloseStrategyArgs, state *realbotTakerCloseStrategyState) bool {
	takerCloseTime := time.Duration(args.liveCfg.TakerCloseMarketTime) * time.Second
	if !args.entryTradingAllowed || !realbotTakerCloseHoldMode(args.liveCfg) || args.timeToExpiry <= 0 || args.timeToExpiry > takerCloseTime {
		return false
	}
	if args.blockNewEntries {
		return true
	}
	if state == nil || state.takerCloseAttempted == nil || *state.takerCloseAttempted {
		return false
	}

	bestOutcome, highestPrice := realbotBestTakerCloseOutcomePrice(args.outcomes, args.tokenBids, args.tokenAsks)
	minPrice := normalizedRealbotTakerCloseMinPrice(args.liveCfg)
	if bestOutcome == "" && state.lastTakerCloseQuoteRefresh != nil && time.Since(*state.lastTakerCloseQuoteRefresh) > realbotTakerCloseQuoteRefresh {
		*state.lastTakerCloseQuoteRefresh = time.Now()
		if args.wsMgr != nil && args.wsMgr.IsConnected() && !args.wsChannelClosed && state.lastForceReconnect != nil && time.Since(*state.lastForceReconnect) > realbotWSForceReconnect {
			*state.lastForceReconnect = time.Now()
			args.wsMgr.ForceReconnect()
		}
	}
	if bestOutcome == "" || highestPrice < minPrice {
		if highestPrice <= 0 {
			if realbotShouldLogTakerCloseState(state.lastTakerCloseLog, state.lastTakerCloseLogKey, "waiting", realbotTakerCloseLogInterval) {
				args.tui.LogEvent("[%s] ⏳ Taker close awaiting valid quote (needs >= $%.3f)", args.marketID, minPrice)
			}
		} else if realbotShouldLogTakerCloseState(state.lastTakerCloseLog, state.lastTakerCloseLogKey, "waiting", realbotTakerCloseLogInterval) {
			args.tui.LogEvent("[%s] ⏳ Taker close waiting: highest price is $%.3f (needs >= $%.3f)", args.marketID, highestPrice, minPrice)
		}
		return true
	}

	confirmPrice := highestPrice
	confirmSource := "WS"
	localConfirmPrice, localReason, localConfirmOK := realbotCanUseLocalTakerCloseQuote(time.Now(), bestOutcome, args.tokenBids, args.tokenAsks, args.tokenFullAsks, args.quoteState, realbotTakerCloseLocalMaxAge)
	if localConfirmOK {
		confirmPrice = localConfirmPrice
	} else {
		confirmSource = "REST"
		restConfirmOK := true
		for _, token := range args.market.Tokens {
			outcome := args.tokenToOutcome[token.TokenID]
			if outcome != bestOutcome {
				continue
			}
			checkCtx, cancelCheck := context.WithTimeout(args.ctx, realbotTakerCloseRESTTimeout)
			restBid, restAsk, restErr := args.restClient.GetBestBidAsk(checkCtx, token.TokenID)
			cancelCheck()
			if restErr != nil {
				logKey := "rest-confirm-failed:" + outcome
				if realbotShouldLogTakerCloseState(state.lastTakerCloseLog, state.lastTakerCloseLogKey, logKey, realbotTakerCloseLogInterval) {
					args.tui.LogEvent("[%s] ⚠️ Taker close REST confirm failed for %s after local=%s: %v — skipping this tick", args.marketID, outcome, localReason, restErr)
				}
				restConfirmOK = false
				break
			}
			confirmPrice = restAsk
			if confirmPrice <= 0 || confirmPrice >= 1.0 {
				confirmPrice = restBid
			}
			if confirmPrice <= 0 || confirmPrice >= 1.0 {
				restConfirmOK = false
			}
			break
		}
		if !restConfirmOK {
			return true
		}
	}
	if confirmPrice < minPrice {
		if realbotShouldLogTakerCloseState(state.lastTakerCloseLog, state.lastTakerCloseLogKey, "waiting", realbotTakerCloseLogInterval) {
			args.tui.LogEvent("[%s] ⏳ Taker close waiting: %s confirm $%.3f is below min $%.3f (WS trigger $%.3f)", args.marketID, confirmSource, confirmPrice, minPrice, highestPrice)
		}
		return true
	}

	tokenID := realbotTokenIDForOutcome(args.tokenMap, bestOutcome)
	if tokenID == "" {
		if realbotShouldLogTakerCloseState(state.lastTakerCloseLog, state.lastTakerCloseLogKey, "missing-token-id:"+bestOutcome, realbotTakerCloseLogInterval) {
			args.tui.LogEvent("[%s] ⚠️ Taker close skipped: missing token id for %s", args.marketID, bestOutcome)
		}
		return true
	}

	budget := realbotTakerCloseBudget(args.engine.GetBalance(), realbotSizingCapitalForTrade(args.engine, args.liveCfg), args.liveCfg)
	plan, planErr := buildRealbotTakerClosePlan(budget, confirmPrice, args.liveCfg)
	if planErr != nil {
		if realbotShouldLogTakerCloseState(state.lastTakerCloseLog, state.lastTakerCloseLogKey, "plan-rejected:"+strings.TrimSpace(planErr.Error()), realbotTakerCloseLogInterval) {
			args.tui.LogEvent("[%s] ⚠️ Taker close plan rejected: %v", args.marketID, planErr)
		}
		*state.takerCloseAttempted = true
		return true
	}

	if args.entryGate != nil && !args.entryGate.TryAcquire() {
		if realbotShouldLogTakerCloseState(state.lastTakerCloseLog, state.lastTakerCloseLogKey, "entry-gate-busy", realbotTakerCloseLogInterval) {
			args.tui.LogEvent("[%s] ⏳ Taker close paused: another market is executing a live entry", args.marketID)
		}
		return true
	}

	initialPosition := args.trader.GetLivePositionSize(tokenID)
	args.tui.LogEvent("[%s] ⚡ Taker close submit: %s %s shares cap $%.3f (WS $%.3f, %s $%.3f, budget $%.2f)",
		args.marketID, bestOutcome, formatShareQty(plan.RequestedQty), plan.LimitPrice, highestPrice, confirmSource, confirmPrice, budget)
	realbotMarkTakerCloseStateLogged(state.lastTakerCloseLog, state.lastTakerCloseLogKey, "submitted")

	*state.takerCloseAttempted = true
	tradeCtx, cancelTrade := context.WithTimeout(args.ctx, 4*time.Second)
	exec := executeMarketOrderWithSignals(tradeCtx, args.trader, api.SideBuy, tokenID, bestOutcome, plan.LimitPrice, plan.RequestedQty, args.tokenFeeRates[bestOutcome], initialPosition, 2500*time.Millisecond)
	cancelTrade()
	logDirectExecutionAudit(args.tui, args.marketID, "Taker Close BUY", plan.RequestedQty, plan.LimitPrice, exec)

	recoveredLateFill := false
	if !exec.Success {
		if recoveredQty, recoverErr := realbotRecoverLateBuyFill(args.trader, tokenID, initialPosition, plan.RequestedQty); recoverErr == nil && hasConfirmedExecutedQty(api.SideBuy, recoveredQty) {
			exec.ExecutedQty = recoveredQty
			exec.Success = true
			exec.VerifyErr = nil
			recoveredLateFill = true
		} else if recoverErr != nil {
			args.tui.LogEvent("[%s] ⚠️ Taker close late-fill check failed: %v", args.marketID, recoverErr)
		}
	}

	if !exec.Success {
		if exec.Err != nil {
			args.tui.LogEvent("[%s] ❌ Taker close buy failed: %v", args.marketID, exec.Err)
		} else if exec.Result != nil && exec.Result.Message != "" {
			args.tui.LogEvent("[%s] ⚠️ Taker close buy not filled: %s", args.marketID, exec.Result.Message)
		} else {
			args.tui.LogEvent("[%s] ⚠️ Taker close buy not filled before timeout at cap $%.3f", args.marketID, plan.LimitPrice)
		}
		if args.entryGate != nil {
			args.entryGate.Release()
		}
		return true
	}

	execQty := attributedBuyFill(exec, plan.RequestedQty, 0, false)
	if !hasConfirmedExecutedQty(api.SideBuy, execQty) {
		args.tui.LogEvent("[%s] ⚠️ Taker close execution below confirmation threshold: %s shares", args.marketID, formatShareQty(execQty))
		if args.entryGate != nil {
			args.entryGate.Release()
		}
		return true
	}

	execPrice := venueExecutionEffectivePrice(exec)
	if execPrice <= 0 {
		execPrice = plan.LimitPrice
	}
	shouldMirrorEngine := realbotShouldMirrorExecutionIntoEngine(args.trader)
	preLocalQty, _ := localBoughtPositionAvg(args.engine, args.marketID, bestOutcome)
	execCost := reportedBuyCost(exec, execPrice, execQty, plan.RequestedQty)
	if shouldMirrorEngine {
		if _, buyErr := args.engine.BuyForMarketWithFeeRate(args.marketID, bestOutcome, execPrice, execQty, args.tokenFeeRates[bestOutcome]); buyErr != nil {
			args.tui.LogEvent("[%s] ⚠️ Taker close local inventory sync failed after confirmed fill: %v", args.marketID, buyErr)
		}
	}
	postBuyLocalQty, _ := localBoughtPositionAvg(args.engine, args.marketID, bestOutcome)
	if !shouldMirrorEngine {
		preLocalQty = math.Max(0, postBuyLocalQty-execQty)
	}
	args.tui.RecordOrder(args.marketID, bestOutcome, "BUY", execQty, execPrice, execCost, 0.0, 0.0, "FILLED")
	if recoveredLateFill {
		args.tui.LogEvent("[%s] 🔄 Taker close recovered delayed fill: bought %s %s after post-timeout refresh", args.marketID, formatShareQty(execQty), bestOutcome)
	}

	if execPrice+1e-9 < plan.MinPrice {
		args.tui.LogEvent("[%s] ℹ️ Taker close filled below the trigger price ($%.3f < $%.3f); the min-price gate only decides when to enter, and the venue matched at a better price", args.marketID, execPrice, plan.MinPrice)
	}

	if state.takerCloseExecutedAt != nil {
		*state.takerCloseExecutedAt = time.Now()
	}
	args.tui.LogEvent("[%s] ✅ Taker close confirmed: bought %s %s at $%.3f (cap $%.3f)",
		args.marketID, formatShareQty(execQty), bestOutcome, execPrice, plan.LimitPrice)
	args.tui.LogEvent("[%s] 🧾 Taker close position delta: %s %s | local position %.4f → %.4f | spend $%.4f",
		args.marketID, formatShareQty(execQty), bestOutcome, preLocalQty, postBuyLocalQty, execCost)

	args.refreshWalletTruth(5 * time.Second)
	if args.entryGate != nil {
		args.entryGate.Release()
	}
	return false
}
