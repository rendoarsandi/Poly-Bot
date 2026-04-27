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

func realbotAssetPrefix(marketID string) string {
	marketID = strings.TrimSpace(marketID)
	if idx := strings.Index(marketID, "#"); idx >= 0 {
		marketID = marketID[:idx]
	}
	if idx := strings.Index(marketID, "-"); idx >= 0 {
		marketID = marketID[:idx]
	}
	return strings.ToUpper(strings.TrimSpace(marketID))
}

func realbotBinanceSymbolForMarket(marketID string, cfg *core.Config) string {
	if cfg == nil {
		return ""
	}
	asset := realbotAssetPrefix(marketID)
	if asset == "" || asset == "UNKNOWN" {
		return ""
	}
	return asset + strings.ToUpper(strings.TrimSpace(cfg.BinanceQuoteAsset))
}

func realbotResolveDirectionalOutcomes(outcomes []string) (paper.DirectionalOutcomes, bool) {
	mapping := paper.DirectionalOutcomes{}
	for _, outcome := range outcomes {
		switch strings.ToLower(strings.TrimSpace(outcome)) {
		case "up", "yes":
			mapping.Up = outcome
		case "down", "no":
			mapping.Down = outcome
		}
	}
	return mapping, mapping.Up != "" && mapping.Down != ""
}

func realbotBinanceGapBuyLimitPrice(ask, maxAskPrice float64) float64 {
	return realbotDirectionalBuyLimitPrice(ask, maxAskPrice, binanceGapMaxSlippageCents)
}

func realbotHandleBinanceGapMarket(ctx context.Context, id string, outcomes []string, tokenBids, tokenAsks map[string]float64, tokenFullBids, tokenFullAsks map[string][]paper.MarketLevel, lastPairUpdate time.Time, polyTracker *paper.DirectionalSignalTracker, tokenFeeRates map[string]int, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI, liveCfg paper.TUISettings, cfg *core.Config, currentBalance float64, binanceFeed *api.BinanceFuturesPriceFeed, getTokenID func(string) string, entryGate *realbotEntryGate, lastTrade *time.Time, lastBinanceLog *time.Time) {
	logThrottled := func(format string, args ...interface{}) {
		if lastBinanceLog == nil {
			tui.LogEvent(format, args...)
			return
		}
		if lastBinanceLog.IsZero() || time.Since(*lastBinanceLog) >= 5*time.Second {
			tui.LogEvent(format, args...)
			*lastBinanceLog = time.Now()
		}
	}
	status := paper.MarketBinanceSignal{
		Enabled:   true,
		Status:    "waiting",
		Reason:    "awaiting Binance signal",
		UpdatedAt: time.Now(),
	}
	setStatusSnapshot := func(snap api.BinanceFuturesSignalSnapshot) {
		status.Symbol = snap.Symbol
		status.Price = snap.Price
		status.DeltaPercent = snap.DeltaPercent
	}
	setStatusSignal := func(signal paper.BinanceGapSignal) {
		status.TargetOutcome = signal.TargetOutcome
		status.SignalLabel = signal.SignalLabel
		status.EffectiveGapPercent = signal.EffectiveGapPercent
		status.PolyFavorableMoveCents = signal.PolyFavorableMoveCents
		status.PolyAdverseMoveCents = signal.PolyAdverseMoveCents
		status.TargetSpreadCents = signal.TargetSpreadCents
		status.TargetBookImbalance = signal.TargetBookImbalance
		status.OppositeBookImbalance = signal.OppositeBookImbalance
		status.DirectionalBookScore = signal.DirectionalBookScore
	}
	defer func() {
		if tui != nil {
			status.UpdatedAt = time.Now()
			tui.SetMarketBinanceSignal(id, status)
		}
	}()

	mapping, ok := realbotResolveDirectionalOutcomes(outcomes)
	if !ok {
		status.Status = "inactive"
		status.Reason = "outcomes are not Up/Down or Yes/No"
		logThrottled("[%s] ℹ️ Binance gap mode skipped: outcomes are not Up/Down or Yes/No", id)
		return
	}

	if upTokenID := getTokenID(mapping.Up); upTokenID != "" {
		realbotSyncExternalPositionWithCostBasis(trader, engine, id, mapping.Up, upTokenID, trader.GetLivePositionSize(upTokenID), tokenAsks[mapping.Up])
	}
	if downTokenID := getTokenID(mapping.Down); downTokenID != "" {
		realbotSyncExternalPositionWithCostBasis(trader, engine, id, mapping.Down, downTokenID, trader.GetLivePositionSize(downTokenID), tokenAsks[mapping.Down])
	}

	if binanceFeed == nil {
		status.Status = "inactive"
		status.Reason = "no Binance futures feed configured"
		logThrottled("[%s] ℹ️ Binance gap mode skipped: no Binance futures feed configured", id)
		return
	}

	maxQuoteAge := realbotExecutionQuoteGuardAge(core.ResolveExecutionLocalQuoteMaxAge(cfg))
	profitTargetPct := liveCfg.MinMarginPercent
	if profitTargetPct < 0 {
		profitTargetPct = 0
	}

	snap := binanceFeed.Snapshot(time.Now())
	setStatusSnapshot(snap)
	if errMsg := strings.TrimSpace(snap.LastError); errMsg != "" {
		status.Status = "error"
		status.Reason = "Binance WS error"
		logThrottled("[%s] ⚠️ Binance gap feed error on %s: %s", id, snap.Symbol, errMsg)
		return
	}
	if !snap.Connected && snap.UpdatedAt.IsZero() {
		status.Status = "connecting"
		status.Reason = fmt.Sprintf("connecting to Binance on %s", snap.Symbol)
		return
	}
	if !snap.Ready {
		status.Status = "warmup"
		status.Reason = fmt.Sprintf("building lookback window on %s", snap.Symbol)
		return
	}
	maxSignalAge := core.ResolveBinanceSignalMaxAge(cfg)
	if snap.UpdatedAt.IsZero() || time.Since(snap.UpdatedAt) > maxSignalAge {
		status.Status = "waiting"
		status.Reason = fmt.Sprintf("waiting for fresh WS signal on %s", snap.Symbol)
		return
	}
	signal, signalReason := paper.EvaluateBinanceGapSignal(time.Now(), mapping, tokenBids, tokenAsks, tokenFullBids, tokenFullAsks, snap, polyTracker, maxSignalAge)
	setStatusSignal(signal)

	threshold := 0.03

	upQty, _ := localBoughtPositionAvg(engine, id, mapping.Up)
	downQty, _ := localBoughtPositionAvg(engine, id, mapping.Down)
	if upQty > 0 && downQty > 0 {
		logThrottled("[%s] ⚠️ Binance gap mode holding both sides locally; managing independently without awaiting merge", id)
	}

	cooldown := core.ResolveBinanceSignalCooldown(cfg)
	if lastTrade != nil && !lastTrade.IsZero() && time.Since(*lastTrade) < cooldown {
		status.Status = "cooldown"
		status.Reason = fmt.Sprintf("cooldown %s", cooldown.Round(time.Millisecond))
		return
	}

	if signalReason != "" {
		status.Status = "waiting"
		status.Reason = signalReason
		return
	}
	if signal.EffectiveGapPercent < threshold {
		status.Status = "idle"
		status.Reason = fmt.Sprintf("cross-market gap %.3f%% is below the %.3f%% trigger", signal.EffectiveGapPercent, threshold)
		return
	}

	targetOutcome := signal.TargetOutcome
	signalLabel := signal.SignalLabel
	status.Ready = true
	status.Status = "ready"
	status.Reason = "signal ready"
	if ok, reason := realbotCanUseLocalDirectionalBuyQuote(time.Now(), targetOutcome, tokenBids, tokenAsks, tokenFullAsks, lastPairUpdate, maxQuoteAge); !ok {
		status.Ready = false
		status.Status = "waiting"
		status.Reason = reason
		logThrottled("[%s] ⏳ Binance entry waiting for fresh %s quote (%s)", id, targetOutcome, reason)
		return
	}
	ask := tokenAsks[targetOutcome]
	if ask < liveCfg.MinAskPrice || ask > liveCfg.MaxAskPrice {
		status.Ready = false
		status.Status = "blocked"
		status.Reason = fmt.Sprintf("%s ask $%.3f outside %.3f-%.3f", targetOutcome, ask, liveCfg.MinAskPrice, liveCfg.MaxAskPrice)
		return
	}
	limitPrice := realbotBinanceGapBuyLimitPrice(ask, liveCfg.MaxAskPrice)
	if limitPrice <= 0 {
		return
	}
	tradeBudget := cfg.CalculateTradeSize(realbotSizingCapitalForTrade(engine, liveCfg))
	liq := realbotAskLiquidityAtOrBelow(tokenFullAsks[targetOutcome], limitPrice)
	shares := normalizeMarketBuyShares(math.Min(tradeBudget/limitPrice, liq))
	shares = realbotClampSingleBuySharesToBudget(shares, tradeBudget, limitPrice)
	balanceBudget := currentBalance
	if balanceBudget <= 0 {
		balanceBudget = tradeBudget
	}
	if safeShares := realbotClampSingleBuySharesToBudget(shares, balanceBudget, limitPrice); safeShares < shares {
		shares = safeShares
	}
	if shares < 1 {
		status.Ready = false
		status.Status = "blocked"
		status.Reason = fmt.Sprintf("actionable size below 1 share for %s", targetOutcome)
		logThrottled("[%s] ⚠️ Binance entry skipped: actionable size below 1 share for %s", id, targetOutcome)
		return
	}
	if entryGate != nil && !entryGate.TryAcquire() {
		status.Ready = false
		status.Status = "waiting"
		status.Reason = "another market is executing a live entry"
		logThrottled("[%s] ⏳ Binance entry waiting: another market is executing a live entry", id)
		return
	}
	defer entryGate.Release()

	tokenID := getTokenID(targetOutcome)
	if tokenID == "" {
		status.Ready = false
		status.Status = "blocked"
		status.Reason = fmt.Sprintf("missing token id for %s", targetOutcome)
		logThrottled("[%s] ⚠️ Binance entry skipped: missing token id for %s", id, targetOutcome)
		return
	}
	initialPosition := trader.GetLivePositionSize(tokenID)
	rate := realbotResolveFeeRateBps(tokenFeeRates, targetOutcome, cfg)
	exec := executeMarketOrderWithSignals(ctx, trader, api.SideBuy, tokenID, targetOutcome, limitPrice, shares, rate, initialPosition, 2500*time.Millisecond)
	if !exec.Success {
		status.Ready = false
		status.Status = "blocked"
		status.Reason = fmt.Sprintf("entry failed for %s", targetOutcome)
		if exec.Err != nil {
			status.Reason = exec.Err.Error()
			logThrottled("[%s] ⚠️ Binance entry failed for %s: %v", id, targetOutcome, exec.Err)
		} else if exec.Result != nil && exec.Result.Message != "" {
			status.Reason = exec.Result.Message
			logThrottled("[%s] ⚠️ Binance entry failed for %s: %s", id, targetOutcome, exec.Result.Message)
		}
		return
	}
	buyQty := exec.ExecutedQty
	if buyQty <= 0 {
		buyQty = exec.AcknowledgedQty
	}
	buyQty = clampRequestedExecutionQty(buyQty, shares)
	if buyQty < minOnChainActionShares {
		status.Ready = false
		status.Status = "blocked"
		status.Reason = fmt.Sprintf("entry confirmation too small for %s", targetOutcome)
		logThrottled("[%s] ⚠️ Binance entry confirmation too small for %s", id, targetOutcome)
		return
	}
	buyPrice := venueExecutionEffectivePrice(exec)
	if buyPrice <= 0 {
		buyPrice = limitPrice
	}
	cost := reportedBuyCost(exec, buyPrice, buyQty, shares)
	if realbotShouldMirrorExecutionIntoEngine(trader) {
		trader.RecordExecutionBuy(tokenID, buyQty, cost)
		if _, err := realbotMirrorLiveBuyIntoEngine(engine, id, targetOutcome, cost, buyQty); err != nil {
			status.Ready = false
			status.Status = "blocked"
			status.Reason = err.Error()
			logThrottled("[%s] ⚠️ Binance entry engine sync failed for %s: %v", id, targetOutcome, err)
			return
		}
	}
	status.Status = "triggered"
	status.Reason = fmt.Sprintf("bought %.2f %s @ $%.3f", buyQty, targetOutcome, buyPrice)
	tui.LogEvent("[%s] ⚡ BINANCE %s SIGNAL %s %.2f @ $%.3f | Δ %.3f%% over %s (%s) | poly catch-up %.2fc adverse %.2fc spread %.2fc", id, signalLabel, targetOutcome, buyQty, buyPrice, snap.DeltaPercent, core.ResolveBinanceSignalLookback(cfg), snap.Symbol, signal.PolyFavorableMoveCents, signal.PolyAdverseMoveCents, signal.TargetSpreadCents)
	tui.RecordOrderWithMode(id, targetOutcome, "BUY", buyQty, buyPrice, cost, snap.DeltaPercent, 0.0, paperArbModeBinanceGap, "FILLED")
	if lastTrade != nil {
		*lastTrade = time.Now()
	}
}
