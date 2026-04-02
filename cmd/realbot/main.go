package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	mkt "Market-bot/internal/markets"
	"Market-bot/internal/paper"
	"Market-bot/internal/setup"
	"Market-bot/internal/strategy"
	"Market-bot/internal/trading"
)

const (
	UseLiveUI                        = true // Set to false for traditional logging
	paperArbModeTaker                = "taker"
	paperArbModeBinanceGap           = "binance-gap"
	paperArbModeCopytrade            = "copytrade"
	paperArbModeMaker                = "maker"
	terminalBidFloor                 = 0.985
	terminalAskCeil                  = 0.015
	realbotExecQuoteTimeout          = 1500 * time.Millisecond
	realbotOrderWarmTimeout          = 1500 * time.Millisecond
	realbotRestBookMaxAge            = 2 * time.Second
	realbotTakerCloseRESTTimeout     = 1200 * time.Millisecond
	realbotWSWarnInterval            = 10 * time.Second
	realbotWSForceReconnect          = 10 * time.Second
	realbotMergeTimeout              = 120 * time.Second
	realbotCleanupVerifyTTL          = 20 * time.Second
	realbotFastVerifyTTL             = 6 * time.Second
	minOnChainActionShares           = 0.01
	realbotUIRefreshInterval         = 500 * time.Millisecond
	realbotMainLoopInterval          = 10 * time.Millisecond
	realbotCopytradeLoopIntervalMin  = 100 * time.Millisecond
	realbotCopytradeLoopIntervalMax  = 250 * time.Millisecond
	realbotCopytradeUIRefreshMin     = 500 * time.Millisecond
	realbotCopytradeUIRefreshMax     = 1 * time.Second
	realbotCopytradeRetryQueueCap    = 256
	realbotCopytradeRetryMaxAge      = 20 * time.Second
	realbotFillPollInterval          = 50 * time.Millisecond
	realbotTakerCloseQuoteRefresh    = 500 * time.Millisecond
	realbotTakerCloseLogInterval     = 5 * time.Second
	realbotTakerCloseLocalMaxAge     = 350 * time.Millisecond
	realbotRedeemConfirmTimeout      = 120 * time.Second
	realbotRedeemProbeTimeout        = 10 * time.Second
	realbotRedeemRetryInterval       = 10 * time.Second
	realbotWalletTruthLogMinDelta    = 0.25
	realbotMaxSaneOutcomeSpread      = 0.10
	realbotMaxSaneAskPairSum         = 1.10
	realbotMinSaneBidPairSum         = 0.90
	realbotExecutionGuardQuoteMaxAge = 1500 * time.Millisecond
	realbotBalanceSyncInterval       = 60 * time.Second
	realbotBalanceSyncTimeout        = 8 * time.Second
	realbotMakerQuoteStep            = 0.001
	realbotMakerBaseOffset           = 0.008
	realbotMakerInventorySkewStep    = 0.020
	realbotMakerInventoryTargetMult  = 2.5
	realbotMakerInventoryCapMult     = 5.0
	realbotMakerQuoteSizeSkewFactor  = 0.75
	realbotMakerRequoteInterval      = 500 * time.Millisecond
	realbotMakerMinQuoteValue        = 5.0
	realbotMakerCashUsagePerOutcome  = 0.35
)

var globalResWatcher *api.ResolutionWatcher

var realbotMakerStrategyParams = strategy.MakerParams{
	QuoteStep:           realbotMakerQuoteStep,
	DefaultQuoteGap:     realbotMakerBaseOffset,
	InventorySkewStep:   realbotMakerInventorySkewStep,
	QuoteSizeSkewFactor: realbotMakerQuoteSizeSkewFactor,
	CashUsagePerOutcome: realbotMakerCashUsagePerOutcome,
	MinQuoteValue:       realbotMakerMinQuoteValue,
}

// realbotEntryGate ensures only one aggressive live entry (panic-buy/taker-close)
// is submitted at a time across all concurrent market goroutines.
type realbotEntryGate struct {
	token chan struct{}
}

func newRealbotEntryGate() *realbotEntryGate {
	g := &realbotEntryGate{token: make(chan struct{}, 1)}
	g.token <- struct{}{}
	return g
}

func (g *realbotEntryGate) TryAcquire() bool {
	if g == nil {
		return true
	}
	select {
	case <-g.token:
		return true
	default:
		return false
	}
}

func (g *realbotEntryGate) Release() {
	if g == nil {
		return
	}
	select {
	case g.token <- struct{}{}:
	default:
	}
}

type realbotOrderPathWarmer interface {
	GetTradingAllowance(ctx context.Context) (float64, error)
}

func primeRealbotOrderPath(parentCtx context.Context, warmer realbotOrderPathWarmer) {
	if warmer == nil {
		return
	}
	go func() {
		warmCtx, cancel := context.WithTimeout(parentCtx, realbotOrderWarmTimeout)
		defer cancel()
		_, _ = warmer.GetTradingAllowance(warmCtx)
	}()
}

func realbotShouldReconnectWS(outcomes []string, bids, asks map[string]float64, pairQuoteAge, staleThreshold time.Duration, terminalBookState bool) bool {
	if staleThreshold <= 0 {
		staleThreshold = 15 * time.Second
	}
	if terminalBookState || pairQuoteAge <= staleThreshold {
		return false
	}
	reason := realbotLocalQuoteSanityReason(outcomes, bids, asks)
	return reason != ""
}

func realbotTakerCloseHoldMode(cfg paper.TUISettings) bool {
	return paper.TakerCloseModeActive(cfg)
}

func realbotCopytradeHoldMode(cfg paper.TUISettings) bool {
	return strings.EqualFold(normalizePaperArbMode(cfg.PaperArbMode), paperArbModeCopytrade)
}

func realbotHasEnginePositionsForMarket(engine *paper.Engine, marketID string) bool {
	if engine == nil || marketID == "" {
		return false
	}
	for _, pos := range engine.GetPositions() {
		if pos.MarketID == marketID && pos.Quantity > 0 {
			return true
		}
	}
	return false
}

func realbotWalletTruthPositionsForRedemption(ctx context.Context, marketID, conditionID string, trader *trading.RealTrader, engine *paper.Engine) ([]paper.WalletTruthPosition, error) {
	if trader == nil || engine == nil || marketID == "" || conditionID == "" {
		return nil, nil
	}

	info, err := trader.GetMarketInfo(ctx, conditionID)
	if err != nil {
		return nil, err
	}

	localByOutcome := make(map[string]float64)
	for _, pos := range engine.GetPositions() {
		if pos.MarketID != marketID || pos.Quantity <= 0 {
			continue
		}
		localByOutcome[pos.Outcome] += pos.Quantity
	}

	positions := make([]paper.WalletTruthPosition, 0, len(info.Tokens))
	for _, token := range info.Tokens {
		if token.TokenID == "" || token.Outcome == "" {
			continue
		}
		onChainShares, err := trader.GetCTFBalanceFloat(ctx, token.TokenID)
		if err != nil {
			return nil, err
		}
		localShares := localByOutcome[token.Outcome]
		if localShares <= 0 && onChainShares <= 0 {
			continue
		}
		positions = append(positions, paper.WalletTruthPosition{
			MarketID:      marketID,
			Outcome:       token.Outcome,
			LocalShares:   localShares,
			OnChainShares: onChainShares,
			Drift:         onChainShares - localShares,
		})
	}
	sort.Slice(positions, func(i, j int) bool {
		if positions[i].MarketID == positions[j].MarketID {
			return positions[i].Outcome < positions[j].Outcome
		}
		return positions[i].MarketID < positions[j].MarketID
	})
	return positions, nil
}

func realbotSyncEngineToWalletTruthForResolution(engine *paper.Engine, marketID string, positions []paper.WalletTruthPosition) (adjusted int, missingCostBasis []string) {
	if engine == nil || marketID == "" {
		return 0, nil
	}
	enginePositions := engine.GetPositions()
	for _, wt := range positions {
		if wt.MarketID != marketID || wt.OnChainShares <= 0 {
			continue
		}
		key := marketID + ":" + wt.Outcome
		pos, exists := enginePositions[key]
		if !exists || pos.Quantity <= 0 {
			missingCostBasis = append(missingCostBasis, wt.Outcome)
			continue
		}
		markPrice := pos.AvgPrice
		if markPrice <= 0 && pos.Quantity > 0 {
			markPrice = pos.TotalCost / pos.Quantity
		}
		if markPrice <= 0 {
			markPrice = 0.5
		}
		if engine.SyncExternalPosition(marketID, wt.Outcome, wt.OnChainShares, markPrice) {
			adjusted++
		}
	}
	sort.Strings(missingCostBasis)
	return adjusted, missingCostBasis
}

func refreshWalletTruthForRedemption(ctx context.Context, marketID, conditionID string, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI) error {
	positions, err := realbotWalletTruthPositionsForRedemption(ctx, marketID, conditionID, trader, engine)
	if err != nil {
		return err
	}
	tui.SetWalletTruthPositions(marketID, positions)
	return nil
}

func realbotShouldRunNearExpiryCleanup(cfg paper.TUISettings, timeToExpiry, mergeBuffer time.Duration) bool {
	if realbotTakerCloseHoldMode(cfg) || strings.EqualFold(normalizePaperArbMode(cfg.PaperArbMode), paperArbModeCopytrade) {
		return false
	}
	return timeToExpiry > 0 && timeToExpiry <= mergeBuffer
}

func realbotTakerCloseBudget(cash, sizingCapital float64, liveCfg paper.TUISettings) float64 {
	if cash <= 0 && sizingCapital <= 0 {
		return 0
	}

	// Taker-close sizing follows current live equity/book value so late-market
	// entries do not keep trading off a stale high-water budget.
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

func realbotCurrentSizingCapital(engine *paper.Engine) float64 {
	if engine == nil {
		return 0
	}
	sizing := engine.GetBookEquity()
	if sizing < 0 {
		return 0
	}
	if cash := engine.GetBalance(); cash > sizing {
		return cash
	}
	return sizing
}

func realbotSizingCapitalForTrade(engine *paper.Engine, liveCfg paper.TUISettings) float64 {
	if engine == nil {
		return 0
	}
	if realbotTakerCloseHoldMode(liveCfg) {
		return realbotCurrentSizingCapital(engine)
	}
	sizing := engine.GetSizingBalance()
	if sizing < 0 {
		return 0
	}
	return sizing
}

func realbotBestTakerCloseOutcomePrice(outcomes []string, bids, asks map[string]float64) (string, float64) {
	bestOutcome := ""
	highestPrice := 0.0
	for _, outcome := range outcomes {
		price := asks[outcome]
		if price <= 0 || price >= 1.0 {
			price = bids[outcome]
		}
		if price > 0 && price <= 1.0 && price > highestPrice {
			highestPrice = price
			bestOutcome = outcome
		}
	}
	return bestOutcome, highestPrice
}

func realbotCanonicalizeMarketTokens(market *api.Market, info *api.MarketInfo) (changed bool, matched int) {
	if market == nil || info == nil {
		return false, 0
	}
	canonicalOutcomes := make(map[string]string, len(info.Tokens))
	for _, token := range info.Tokens {
		outcome := core.SanitizeString(token.Outcome)
		if token.TokenID == "" || outcome == "" {
			continue
		}
		canonicalOutcomes[token.TokenID] = outcome
	}
	if len(canonicalOutcomes) == 0 {
		return false, 0
	}
	for i := range market.Tokens {
		outcome, ok := canonicalOutcomes[market.Tokens[i].TokenID]
		if !ok {
			continue
		}
		matched++
		if market.Tokens[i].Outcome != outcome {
			market.Tokens[i].Outcome = outcome
			changed = true
		}
	}
	return changed, matched
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

func normalizedRealbotExecutionPriceCap(liveCfg paper.TUISettings) float64 {
	limitPrice := liveCfg.TakerCloseMarketSlippage
	if limitPrice <= 0 || limitPrice >= 1.0 {
		return 0.99
	}
	return limitPrice
}

func normalizePaperArbMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case paperArbModeBinanceGap:
		return paperArbModeBinanceGap
	case paperArbModeCopytrade:
		return paperArbModeCopytrade
	case paperArbModeMaker:
		return paperArbModeMaker
	default:
		return paperArbModeTaker
	}
}

func realbotCopytradePollEvery(settings paper.TUISettings) time.Duration {
	pollEvery := time.Duration(settings.CopytradePollIntervalMs) * time.Millisecond
	if pollEvery <= 0 {
		pollEvery = 2 * time.Second
	}
	return pollEvery
}

func realbotTraderLoopInterval(settings paper.TUISettings) time.Duration {
	if normalizePaperArbMode(settings.PaperArbMode) == paperArbModeCopytrade {
		interval := realbotCopytradePollEvery(settings) / 2
		if interval < realbotCopytradeLoopIntervalMin {
			interval = realbotCopytradeLoopIntervalMin
		}
		if interval > realbotCopytradeLoopIntervalMax {
			interval = realbotCopytradeLoopIntervalMax
		}
		return interval
	}
	return realbotMainLoopInterval
}

func realbotUIInterval(settings paper.TUISettings) time.Duration {
	if normalizePaperArbMode(settings.PaperArbMode) == paperArbModeCopytrade {
		interval := realbotCopytradePollEvery(settings) / 2
		if interval < realbotCopytradeUIRefreshMin {
			interval = realbotCopytradeUIRefreshMin
		}
		if interval > realbotCopytradeUIRefreshMax {
			interval = realbotCopytradeUIRefreshMax
		}
		return interval
	}
	return realbotUIRefreshInterval
}

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

func realbotDirectionalProfitTargetPrice(avgPrice, profitTargetPct float64) float64 {
	target := avgPrice * (1.0 + (profitTargetPct / 100.0))
	if target < 0.01 {
		return 0.01
	}
	if target > 0.99 {
		return 0.99
	}
	return target
}

func realbotDirectionalBuyLimitPrice(ask, maxAskPrice float64) float64 {
	if ask <= 0 {
		return 0
	}
	limit := ask + 0.01
	if maxAskPrice > 0 && maxAskPrice < limit {
		limit = maxAskPrice
	}
	if limit < ask {
		limit = ask
	}
	if limit > 0.99 {
		limit = 0.99
	}
	return limit
}

func realbotAskLiquidityAtOrBelow(levels []paper.MarketLevel, maxPrice float64) float64 {
	total := 0.0
	for _, lvl := range levels {
		if lvl.Size <= 0 {
			continue
		}
		if lvl.Price <= maxPrice+1e-9 {
			total += lvl.Size
		}
	}
	return total
}

func realbotBidLiquidityAtOrAbove(levels []paper.MarketLevel, minPrice float64) float64 {
	total := 0.0
	for _, lvl := range levels {
		if lvl.Size <= 0 {
			continue
		}
		if lvl.Price+1e-9 >= minPrice {
			total += lvl.Size
		}
	}
	return total
}

func realbotClampSingleBuySharesToBudget(requestedShares, budget, limitPrice float64) float64 {
	qty := normalizeMarketBuyShares(requestedShares)
	if qty <= 0 || budget <= 0 || limitPrice <= 0 {
		return 0
	}
	if affordable := normalizeMarketBuyShares(budget / limitPrice); affordable < qty {
		qty = affordable
	}
	for qty >= 0.0001 {
		if cost := realbotRoundedLimitBuyCost(limitPrice, qty); cost > 0 && cost <= budget+1e-9 {
			return qty
		}
		qty = normalizeMarketBuyShares(qty - 0.0001)
	}
	return 0
}

func realbotCanUseLocalDirectionalBuyQuote(now time.Time, outcome string, tokenBids, tokenAsks map[string]float64, tokenFullAsks map[string][]paper.MarketLevel, quoteState map[string]realbotQuoteState, maxAge time.Duration) (bool, string) {
	ask := tokenAsks[outcome]
	if ask <= 0 || ask >= 1.0 {
		return false, fmt.Sprintf("missing local ask for %s", outcome)
	}
	if len(tokenFullAsks[outcome]) == 0 {
		return false, fmt.Sprintf("missing local ask depth for %s", outcome)
	}
	state, ok := quoteState[outcome]
	if !ok || state.UpdatedAt.IsZero() {
		return false, fmt.Sprintf("missing quote timestamp for %s", outcome)
	}
	age := now.Sub(state.UpdatedAt)
	if age > maxAge {
		return false, fmt.Sprintf("%s buy quote age %s > %s", outcome, age.Round(time.Millisecond), maxAge)
	}
	bid := tokenBids[outcome]
	if bid > 0 && !realbotHasSaneTopOfBook(bid, ask) {
		return false, fmt.Sprintf("crossed local quote for %s (bid %.3f >= ask %.3f)", outcome, bid, ask)
	}
	return true, ""
}

func realbotCanUseLocalDirectionalSellQuote(now time.Time, outcome string, tokenBids, tokenAsks map[string]float64, tokenFullBids map[string][]paper.MarketLevel, quoteState map[string]realbotQuoteState, maxAge time.Duration) (bool, string) {
	bid := tokenBids[outcome]
	if bid <= 0 || bid >= 1.0 {
		return false, fmt.Sprintf("missing local bid for %s", outcome)
	}
	if len(tokenFullBids[outcome]) == 0 {
		return false, fmt.Sprintf("missing local bid depth for %s", outcome)
	}
	state, ok := quoteState[outcome]
	if !ok || state.UpdatedAt.IsZero() {
		return false, fmt.Sprintf("missing quote timestamp for %s", outcome)
	}
	age := now.Sub(state.UpdatedAt)
	if age > maxAge {
		return false, fmt.Sprintf("%s sell quote age %s > %s", outcome, age.Round(time.Millisecond), maxAge)
	}
	ask := tokenAsks[outcome]
	if ask > 0 && !realbotHasSaneTopOfBook(bid, ask) {
		return false, fmt.Sprintf("crossed local quote for %s (bid %.3f >= ask %.3f)", outcome, bid, ask)
	}
	return true, ""
}

func realbotHandleBinanceGapMarket(ctx context.Context, id string, outcomes []string, tokenBids, tokenAsks map[string]float64, tokenFullBids, tokenFullAsks map[string][]paper.MarketLevel, quoteState map[string]realbotQuoteState, polyTracker *paper.DirectionalSignalTracker, tokenFeeRates map[string]int, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI, liveCfg paper.TUISettings, cfg *core.Config, currentBalance float64, binanceFeed *api.BinanceFuturesPriceFeed, getTokenID func(string) string, entryGate *realbotEntryGate, lastTrade *time.Time, lastBinanceLog *time.Time) {
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

	upQty, upAvg := localBoughtPositionAvg(engine, id, mapping.Up)
	downQty, downAvg := localBoughtPositionAvg(engine, id, mapping.Down)
	if upQty > 0 && downQty > 0 {
		status.Status = "exit"
		status.Reason = "holding both outcomes; waiting for cleanup"
		logThrottled("[%s] ⚠️ Binance gap mode holding both sides locally; waiting for existing recovery/cleanup before new entries", id)
		return
	}

	heldOutcome := ""
	heldQty := 0.0
	heldAvg := 0.0
	if upQty > 0 {
		heldOutcome, heldQty, heldAvg = mapping.Up, upQty, upAvg
	} else if downQty > 0 {
		heldOutcome, heldQty, heldAvg = mapping.Down, downQty, downAvg
	}
	if heldOutcome != "" {
		status.TargetOutcome = heldOutcome
		status.Status = "exit"
		status.Reason = "managing existing position"
		if ok, reason := realbotCanUseLocalDirectionalSellQuote(time.Now(), heldOutcome, tokenBids, tokenAsks, tokenFullBids, quoteState, maxQuoteAge); !ok {
			status.Reason = reason
			logThrottled("[%s] ⏳ Binance exit waiting for fresh %s quote (%s)", id, heldOutcome, reason)
			return
		}
		bid := tokenBids[heldOutcome]
		targetBid := realbotDirectionalProfitTargetPrice(heldAvg, profitTargetPct)
		if bid+1e-9 < targetBid {
			return
		}
		tokenID := getTokenID(heldOutcome)
		if tokenID == "" {
			logThrottled("[%s] ⚠️ Binance exit skipped: missing token id for %s", id, heldOutcome)
			return
		}
		liq := realbotBidLiquidityAtOrAbove(tokenFullBids[heldOutcome], bid)
		qtyToSell := normalizeMarketSellShares(math.Min(heldQty, liq))
		if qtyToSell < minOnChainActionShares {
			logThrottled("[%s] ⏳ Binance exit waiting for bid liquidity on %s at $%.3f", id, heldOutcome, bid)
			return
		}
		initialPosition := trader.GetLivePositionSize(tokenID)
		rate := tokenFeeRates[heldOutcome]
		if rate == 0 {
			rate = 1000
		}
		exec := executeMarketOrderWithSignals(ctx, trader, api.SideSell, tokenID, heldOutcome, bid, qtyToSell, rate, initialPosition, 2*time.Second)
		if !exec.Success {
			if exec.Err != nil {
				logThrottled("[%s] ⚠️ Binance exit failed for %s: %v", id, heldOutcome, exec.Err)
			} else if exec.Result != nil && exec.Result.Message != "" {
				logThrottled("[%s] ⚠️ Binance exit failed for %s: %s", id, heldOutcome, exec.Result.Message)
			}
			return
		}
		soldQty := exec.ExecutedQty
		if soldQty <= 0 {
			soldQty = exec.AcknowledgedQty
		}
		soldQty = math.Min(soldQty, heldQty)
		if soldQty < minOnChainActionShares {
			logThrottled("[%s] ⚠️ Binance exit confirmation too small for %s", id, heldOutcome)
			return
		}
		sellPrice := venueExecutionEffectivePrice(exec)
		if sellPrice <= 0 {
			sellPrice = bid
		}
		trade, err := engine.SellForMarket(id, heldOutcome, sellPrice, soldQty)
		if err != nil {
			logThrottled("[%s] ⚠️ Binance exit engine sync failed for %s: %v", id, heldOutcome, err)
			return
		}
		profit := trade.Value - (heldAvg * soldQty)
		tui.LogEvent("[%s] ✅ BINANCE EXIT %s %.2f @ $%.3f (target $%.3f, pnl $%.2f)", id, heldOutcome, soldQty, sellPrice, targetBid, profit)
		tui.RecordOrderWithMode(id, heldOutcome, "SELL", soldQty, sellPrice, trade.Value, 0.0, profit, paperArbModeBinanceGap, "FILLED")
		if lastTrade != nil {
			*lastTrade = time.Now()
		}
		return
	}

	cooldown := core.ResolveBinanceSignalCooldown(cfg)
	if lastTrade != nil && !lastTrade.IsZero() && time.Since(*lastTrade) < cooldown {
		status.Status = "cooldown"
		status.Reason = fmt.Sprintf("cooldown %s", cooldown.Round(time.Millisecond))
		return
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
	signal, reason := paper.EvaluateBinanceGapSignal(time.Now(), mapping, tokenBids, tokenAsks, tokenFullBids, tokenFullAsks, snap, polyTracker, maxSignalAge)
	setStatusSignal(signal)
	if reason != "" {
		status.Status = "waiting"
		status.Reason = reason
		return
	}
	threshold := cfg.BinanceSignalThresholdPct
	if threshold <= 0 {
		threshold = 0.02
	}
	if signal.EffectiveGapPercent < threshold {
		status.Status = "idle"
		status.Reason = fmt.Sprintf("cross-market gap %.3f%% is below the %.3f%% trigger", signal.EffectiveGapPercent, threshold)
		return
	}
	if signal.DirectionalBookScore <= -0.35 {
		status.Status = "blocked"
		status.Reason = fmt.Sprintf("local book opposes %s signal (score %.2f)", signal.SignalLabel, signal.DirectionalBookScore)
		return
	}
	polyCatchupMax := cfg.BinanceSignalPolyMaxMoveCents
	if polyCatchupMax > 0 && signal.PolyFavorableMoveCents > polyCatchupMax {
		status.Status = "blocked"
		status.Reason = fmt.Sprintf("%s already caught up %.2fc > %.2fc", signal.TargetOutcome, signal.PolyFavorableMoveCents, polyCatchupMax)
		logThrottled("[%s] ⚠️ Binance entry skipped: %s already caught up %.2fc > %.2fc", id, signal.TargetOutcome, signal.PolyFavorableMoveCents, polyCatchupMax)
		return
	}
	polyAdverseMax := cfg.BinanceSignalPolyAdverseMoveCents
	if polyAdverseMax > 0 && signal.PolyAdverseMoveCents > polyAdverseMax {
		status.Status = "blocked"
		status.Reason = fmt.Sprintf("Polymarket moved against %s by %.2fc > %.2fc", signal.SignalLabel, signal.PolyAdverseMoveCents, polyAdverseMax)
		logThrottled("[%s] ⚠️ Binance entry skipped: Polymarket moved against %s by %.2fc > %.2fc", id, signal.SignalLabel, signal.PolyAdverseMoveCents, polyAdverseMax)
		return
	}
	spreadMax := cfg.BinanceSignalSpreadMaxCents
	if spreadMax <= 0 {
		spreadMax = paper.DefaultBinanceSignalSpreadMaxCents
	}
	if signal.TargetSpreadCents > spreadMax {
		status.Status = "blocked"
		status.Reason = fmt.Sprintf("%s spread %.2fc > %.2fc", signal.TargetOutcome, signal.TargetSpreadCents, spreadMax)
		logThrottled("[%s] ⚠️ Binance entry skipped: %s spread %.2fc > %.2fc", id, signal.TargetOutcome, signal.TargetSpreadCents, spreadMax)
		return
	}
	targetOutcome := signal.TargetOutcome
	signalLabel := signal.SignalLabel
	status.Ready = true
	status.Status = "ready"
	status.Reason = "signal ready"
	if ok, reason := realbotCanUseLocalDirectionalBuyQuote(time.Now(), targetOutcome, tokenBids, tokenAsks, tokenFullAsks, quoteState, maxQuoteAge); !ok {
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
		logThrottled("[%s] ⚠️ Binance entry skipped: %s ask $%.3f outside configured range %.3f-%.3f", id, targetOutcome, ask, liveCfg.MinAskPrice, liveCfg.MaxAskPrice)
		return
	}
	limitPrice := realbotDirectionalBuyLimitPrice(ask, liveCfg.MaxAskPrice)
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
	rate := tokenFeeRates[targetOutcome]
	if rate == 0 {
		rate = 1000
	}
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
	if _, err := engine.BuyForMarket(id, targetOutcome, buyPrice, buyQty); err != nil {
		status.Ready = false
		status.Status = "blocked"
		status.Reason = err.Error()
		logThrottled("[%s] ⚠️ Binance entry engine sync failed for %s: %v", id, targetOutcome, err)
		return
	}
	status.Status = "triggered"
	status.Reason = fmt.Sprintf("bought %.2f %s @ $%.3f", buyQty, targetOutcome, buyPrice)
	tui.LogEvent("[%s] ⚡ BINANCE %s SIGNAL %s %.2f @ $%.3f | Δ %.3f%% over %s (%s) | poly catch-up %.2fc adverse %.2fc spread %.2fc", id, signalLabel, targetOutcome, buyQty, buyPrice, snap.DeltaPercent, core.ResolveBinanceSignalLookback(cfg), snap.Symbol, signal.PolyFavorableMoveCents, signal.PolyAdverseMoveCents, signal.TargetSpreadCents)
	tui.RecordOrderWithMode(id, targetOutcome, "BUY", buyQty, buyPrice, cost, snap.DeltaPercent, 0.0, paperArbModeBinanceGap, "FILLED")
	if lastTrade != nil {
		*lastTrade = time.Now()
	}
}

func realbotTUISettingsFromConfig(cfg *core.Config) paper.TUISettings {
	return paper.TUISettings{
		Exchange:                       cfg.Exchange,
		MarketSlug:                     cfg.MarketSlug,
		MaxMarkets:                     cfg.MaxMarkets,
		Timeframe:                      cfg.Timeframe,
		TradeSizingMode:                cfg.TradeSizingMode,
		TradeScaleFactor:               cfg.TradeScaleFactor,
		TradeSizeUSDC:                  cfg.TradeSizeUSDC,
		MinMarginPercent:               cfg.MinMarginPercent,
		BinanceSignalThresholdPct:      cfg.BinanceSignalThresholdPct,
		PaperArbMode:                   normalizePaperArbMode(cfg.PaperArbMode),
		CopytradeTarget:                cfg.CopytradeTarget,
		CopytradePollIntervalMs:        cfg.CopytradePollIntervalMs,
		CopytradeSizingMode:            cfg.CopytradeSizingMode,
		CopytradeSizeUSDC:              cfg.CopytradeSizeUSDC,
		CopytradeSizeShares:            cfg.CopytradeSizeShares,
		CopytradeSizePercent:           cfg.CopytradeSizePercent,
		CopytradeMaxSlippagePct:        cfg.CopytradeMaxSlippagePct,
		BuyExecutionMarginFloorPercent: cfg.BuyExecutionMarginFloorPercent,
		SplitMinMarginSell:             cfg.SplitMinMarginSell,
		SplitStrategyEnabled:           cfg.SplitStrategyEnabled,
		SplitInitialCapPct:             cfg.SplitInitialCapPct,
		SplitReplenishCapPct:           cfg.SplitReplenishCapPct,
		MakerMergeBufferSeconds:        cfg.MakerMergeBufferSeconds,
		MakerQuoteGap:                  cfg.MakerQuoteGap,
		MakerInventoryTargetMult:       cfg.MakerInventoryTargetMult,
		MakerInventoryCapMult:          cfg.MakerInventoryCapMult,
		MakerMinQuoteValue:             cfg.MakerMinQuoteValue,
		MinAskPrice:                    cfg.MinAskPrice,
		MaxAskPrice:                    cfg.MaxAskPrice,
		MaxTradeSize:                   cfg.MaxTradeSize,
		MaxDailyLoss:                   cfg.MaxDailyLoss,
		TakerCloseMarket:               cfg.TakerCloseMarket,
		TakerCloseMarketTime:           cfg.TakerCloseMarketTime,
		TakerCloseMarketSlippage:       cfg.TakerCloseMarketSlippage,
		TakerCloseMarketMinPrice:       cfg.TakerCloseMarketMinPrice,
		TradingHoursMode:               cfg.TradingHoursMode,
	}
}

func applyRealbotTUISettings(cfg *core.Config, s paper.TUISettings) {
	cfg.Exchange = s.Exchange
	cfg.MarketSlug = s.MarketSlug
	cfg.MaxMarkets = s.MaxMarkets
	cfg.Timeframe = s.Timeframe
	cfg.TradeSizingMode = s.TradeSizingMode
	cfg.TradeScaleFactor = s.TradeScaleFactor
	cfg.TradeSizeUSDC = s.TradeSizeUSDC
	cfg.MinMarginPercent = s.MinMarginPercent
	cfg.BinanceSignalThresholdPct = s.BinanceSignalThresholdPct
	cfg.PaperArbMode = normalizePaperArbMode(s.PaperArbMode)
	cfg.CopytradeTarget = strings.TrimSpace(s.CopytradeTarget)
	cfg.CopytradePollIntervalMs = s.CopytradePollIntervalMs
	cfg.CopytradeSizingMode = s.CopytradeSizingMode
	cfg.CopytradeSizeUSDC = s.CopytradeSizeUSDC
	cfg.CopytradeSizeShares = s.CopytradeSizeShares
	cfg.CopytradeSizePercent = s.CopytradeSizePercent
	cfg.CopytradeMaxSlippagePct = s.CopytradeMaxSlippagePct
	cfg.BuyExecutionMarginFloorPercent = s.BuyExecutionMarginFloorPercent
	cfg.SplitMinMarginSell = s.SplitMinMarginSell
	cfg.SplitStrategyEnabled = s.SplitStrategyEnabled
	cfg.SplitInitialCapPct = s.SplitInitialCapPct
	cfg.SplitReplenishCapPct = s.SplitReplenishCapPct
	cfg.MakerMergeBufferSeconds = s.MakerMergeBufferSeconds
	cfg.MakerQuoteGap = s.MakerQuoteGap
	cfg.MakerInventoryTargetMult = s.MakerInventoryTargetMult
	cfg.MakerInventoryCapMult = s.MakerInventoryCapMult
	cfg.MakerMinQuoteValue = s.MakerMinQuoteValue
	cfg.MinAskPrice = s.MinAskPrice
	cfg.MaxAskPrice = s.MaxAskPrice
	cfg.MaxTradeSize = s.MaxTradeSize
	cfg.MaxDailyLoss = s.MaxDailyLoss
	cfg.TakerCloseMarket = s.TakerCloseMarket
	cfg.TakerCloseMarketTime = s.TakerCloseMarketTime
	cfg.TakerCloseMarketSlippage = s.TakerCloseMarketSlippage
	cfg.TakerCloseMarketMinPrice = s.TakerCloseMarketMinPrice
	cfg.TradingHoursMode = s.TradingHoursMode
	if cfg.Exchange == "kalshi" {
		cfg.SplitStrategyEnabled = false
		cfg.MakerMergeBufferSeconds = 0
	}
}

func realbotRoundedLimitBuyCost(price, qty float64) float64 {
	if price <= 0 || price >= 1.0 || qty <= 0 {
		return 0
	}

	sizeMicro := int64(qty*1e6 + 0.5)
	sizeMicro = (sizeMicro / 100) * 100
	if sizeMicro <= 0 {
		return 0
	}

	priceMicro := int64(price*1e6 + 0.5)
	usdcMicro := (priceMicro * sizeMicro) / 1e6
	if usdcMicro%10000 != 0 {
		usdcMicro = ((usdcMicro / 10000) + 1) * 10000
	}

	return float64(usdcMicro) / 1e6
}

func realbotClampBuySharesToBudget(requestedShares, budget float64, prices ...float64) float64 {
	qty := normalizeMarketBuyShares(requestedShares)
	if qty <= 0 || budget <= 0 {
		return 0
	}

	totalPrice := 0.0
	for _, price := range prices {
		if price <= 0 {
			return 0
		}
		totalPrice += price
	}
	if totalPrice <= 0 {
		return 0
	}

	// Start from the cheaper of requested size and the raw pair-sum affordability.
	// Venue cent-rounding may still push the true cost slightly above budget, so we
	// walk down from there at market-like 4 decimal precision.
	if affordable := normalizeMarketBuyShares(budget / totalPrice); affordable < qty {
		qty = affordable
	}

	for qty >= 0.0001 {
		totalCost := 0.0
		valid := true
		for _, price := range prices {
			cost := realbotRoundedLimitBuyCost(price, qty)
			if cost <= 0 {
				valid = false
				break
			}
			totalCost += cost
		}
		if valid && totalCost <= budget+1e-9 {
			return qty
		}
		qty = normalizeMarketBuyShares(qty - 0.0001)
	}
	return 0
}

func realbotQuoteTimestampOrNow(raw string) time.Time {
	ts, err := api.ParseOrderBookTimestamp(raw)
	if err != nil || ts.IsZero() {
		return time.Now()
	}
	return ts
}

func parseWSQuotedPrice(raw string) (float64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 || v > 1.0 {
		return 0, false
	}
	return v, true
}

func realbotShouldSkipStaleQuoteUpdate(quoteState map[string]realbotQuoteState, outcome string, updatedAt time.Time, currentBid, currentAsk float64) bool {
	if updatedAt.IsZero() {
		return false
	}
	state, ok := quoteState[outcome]
	if !ok || state.UpdatedAt.IsZero() {
		return false
	}
	if !updatedAt.Before(state.UpdatedAt) {
		return false
	}
	return realbotHasSaneTopOfBook(currentBid, currentAsk)
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

func realbotWinningOnChainShares(positions []paper.WalletTruthPosition, winner string) float64 {
	if winner == "" {
		return 0
	}
	total := 0.0
	for _, pos := range positions {
		if strings.EqualFold(pos.Outcome, winner) && pos.OnChainShares > 0 {
			total += pos.OnChainShares
		}
	}
	return total
}

func realbotRecoverLateBuyFill(trader *trading.RealTrader, tokenID string, initialPosition, requestedQty float64) (float64, error) {
	refreshCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	positions, err := trader.ForceRefreshPositions(refreshCtx)
	if err != nil {
		return 0, err
	}
	qty := executionDeltaFromPositions(positions, tokenID, initialPosition, api.SideBuy)
	qty = clampRequestedExecutionQty(qty, requestedQty)
	if !hasConfirmedExecutedQty(api.SideBuy, qty) {
		return 0, nil
	}
	return qty, nil
}

func realbotShortTxHash(txHash string) string {
	txHash = strings.TrimSpace(txHash)
	if len(txHash) > 10 {
		return txHash[:10] + "..."
	}
	return txHash
}

func realbotShouldKeepPendingRedeemTx(txHash string, err error) bool {
	if strings.TrimSpace(txHash) == "" || err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "confirmation pending") || strings.Contains(errStr, "timeout waiting for transaction")

}

func launchRealbotRedeemRetryLoop(marketID, conditionID, winner string, numOutcomes int, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI) {
	go func() {
		attempt := 0
		pendingTxHash := ""
		for {
			attempt++
			skipSubmit := false

			if pendingTxHash != "" {
				probeCtx, probeCancel := context.WithTimeout(context.Background(), realbotRedeemProbeTimeout)
				txState, probeErr := trader.GetOnChainTxState(probeCtx, pendingTxHash)
				probeCancel()

				if probeErr != nil {
					tui.LogEvent("[%s] ⚠️ Redeem tx %s probe failed: %v", marketID, realbotShortTxHash(pendingTxHash), probeErr)
					skipSubmit = true
				} else {
					switch txState {
					case "success":
						tui.LogEvent("[%s] ✅ Redeem tx confirmed: %s", marketID, realbotShortTxHash(pendingTxHash))
						pendingTxHash = ""
						skipSubmit = true
					case "reverted":
						tui.LogEvent("[%s] ⚠️ Redeem tx reverted on-chain: %s", marketID, realbotShortTxHash(pendingTxHash))
						pendingTxHash = ""
					case "dropped":
						tui.LogEvent("[%s] ⚠️ Redeem tx dropped from RPC: %s", marketID, realbotShortTxHash(pendingTxHash))
						pendingTxHash = ""
					default:
						tui.LogEvent("[%s] ⏳ Redeem tx still pending: %s", marketID, realbotShortTxHash(pendingTxHash))
						skipSubmit = true
					}
				}
			}

			if !skipSubmit && pendingTxHash == "" {
				redeemCtx, cancel := context.WithTimeout(context.Background(), realbotRedeemConfirmTimeout)
				txHash, err := trader.RedeemOnChainForce(redeemCtx, conditionID, numOutcomes)
				cancel()

				if err == nil {
					tui.LogEvent("[%s] ✅ REDEEMED! Tx: %s", marketID, realbotShortTxHash(txHash))
				} else if realbotShouldKeepPendingRedeemTx(txHash, err) {
					pendingTxHash = txHash
					tui.LogEvent("[%s] ⏳ Redeem attempt %d submitted, waiting on-chain: %s", marketID, attempt, realbotShortTxHash(txHash))
				} else {
					tui.LogEvent("[%s] ⚠️ Redeem attempt %d failed: %v", marketID, attempt, err)
				}
			}

			refreshCtx, refreshCancel := context.WithTimeout(context.Background(), 20*time.Second)
			refreshErr := refreshWalletTruthForRedemption(refreshCtx, marketID, conditionID, trader, engine, tui)
			positions, positionsErr := realbotWalletTruthPositionsForRedemption(refreshCtx, marketID, conditionID, trader, engine)
			refreshCancel()

			if refreshErr != nil {
				tui.LogEvent("[%s] ⚠️ Post-redeem wallet-truth refresh failed: %v", marketID, refreshErr)
			} else {
				tui.UpdateWalletTruthResolution(marketID, true, winner)
			}

			balanceCtx, balanceCancel := context.WithTimeout(context.Background(), 10*time.Second)
			if newBal, balErr := trader.ForceRefreshBalance(balanceCtx); balErr != nil {
				tui.LogEvent("[%s] ⚠️ Post-redeem balance refresh failed: %v", marketID, balErr)
			} else {
				engine.SyncBalanceNeutral(newBal)
				engine.RecalculateDrawdown()
			}
			balanceCancel()

			if positionsErr == nil && realbotWinningOnChainShares(positions, winner) <= 0.000001 {
				engine.ClearPendingRedemption(marketID)
				return
			}

			time.Sleep(realbotRedeemRetryInterval)
		}
	}()
}

func realbotQuoteMapsEqual(outcomes []string, bidsA, asksA, bidsB, asksB map[string]float64) bool {
	for _, outcome := range outcomes {
		if math.Abs(bidsA[outcome]-bidsB[outcome]) > 1e-9 {
			return false
		}
		if math.Abs(asksA[outcome]-asksB[outcome]) > 1e-9 {
			return false
		}
	}
	return true
}

func realbotShouldClearLocalPairQuotes(outcomes []string, bids, asks map[string]float64) bool {
	if realbotHasSanePairQuotes(outcomes, bids, asks) || realbotLooksLikeTerminalBook(outcomes, bids, asks) {
		return false
	}
	// In high-price regimes (any bid ≥ 0.60), transient one-sided gaps are
	// expected due to thin complement-side books. Preserve whatever data we
	// have and let the WS/REST recovery fill in the missing side, instead of
	// nuking all quotes and showing "awaiting liquidity".
	if realbotPairHasHighBid(outcomes, bids) {
		return false
	}
	return true
}

func realbotStorePublishedQuotes(outcomes []string, srcBids, srcAsks, dstBids, dstAsks map[string]float64) {
	for _, outcome := range outcomes {
		dstBids[outcome] = srcBids[outcome]
		dstAsks[outcome] = srcAsks[outcome]
	}
}

func realbotLatestQuoteUpdate(outcomes []string, quoteState map[string]realbotQuoteState) (time.Time, string) {
	latest := time.Time{}
	latestSource := ""
	for _, outcome := range outcomes {
		state, ok := quoteState[outcome]
		if !ok || state.UpdatedAt.IsZero() {
			continue
		}
		if latest.IsZero() || state.UpdatedAt.After(latest) {
			latest = state.UpdatedAt
			latestSource = state.Source
		}
	}
	return latest, latestSource
}

func realbotNormalizeDisplaySource(raw string) string {
	source := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case strings.HasPrefix(source, "rest"):
		return "REST"
	case strings.HasPrefix(source, "ws"):
		return "WS"
	default:
		return "WS"
	}
}

func realbotDisplayHasUsableQuotes(outcomes []string, bids, asks map[string]float64) bool {
	return realbotHasSanePairQuotes(outcomes, bids, asks) || realbotLooksLikeTerminalBook(outcomes, bids, asks)
}

func realbotSyncDisplayQuotes(outcomes []string, liveBids, liveAsks, displayBids, displayAsks map[string]float64, authoritative bool) bool {
	nextBids := make(map[string]float64, len(outcomes))
	nextAsks := make(map[string]float64, len(outcomes))
	for _, outcome := range outcomes {
		nextBids[outcome] = displayBids[outcome]
		nextAsks[outcome] = displayAsks[outcome]
	}

	switch {
	case realbotHasSanePairQuotes(outcomes, liveBids, liveAsks):
		realbotStorePublishedQuotes(outcomes, liveBids, liveAsks, nextBids, nextAsks)
	case realbotLooksLikeTerminalBook(outcomes, liveBids, liveAsks):
		for _, outcome := range outcomes {
			if liveBids[outcome] > 0 {
				nextBids[outcome] = liveBids[outcome]
			}
			if liveAsks[outcome] > 0 {
				nextAsks[outcome] = liveAsks[outcome]
			}
		}
	case authoritative:
		realbotStorePublishedQuotes(outcomes, liveBids, liveAsks, nextBids, nextAsks)
	default:
		return false
	}

	if realbotQuoteMapsEqual(outcomes, nextBids, nextAsks, displayBids, displayAsks) {
		return false
	}
	realbotStorePublishedQuotes(outcomes, nextBids, nextAsks, displayBids, displayAsks)
	return true
}

func resolveRealbotMakerQuoteGap(liveCfg paper.TUISettings, cfg *core.Config) float64 {
	if liveCfg.MakerQuoteGap > 0 {
		return liveCfg.MakerQuoteGap
	}
	if cfg != nil && cfg.MakerQuoteGap > 0 {
		return cfg.MakerQuoteGap
	}
	return realbotMakerBaseOffset
}

type realbotQuoteState struct {
	UpdatedAt time.Time
	Source    string
}

type realbotMakerQuote struct {
	OrderID       string
	TokenID       string
	Outcome       string
	Side          api.Side
	Price         float64
	RequestedQty  float64
	RemainingQty  float64
	AccountedFill float64
	FeeRateBps    int
}

func realbotMakerQuoteKey(side api.Side, outcome string) string {
	return strings.ToLower(strings.TrimSpace(string(side))) + ":" + outcome
}

type realbotPendingMerge struct {
	Qty       float64
	HoldUntil time.Time
}

type realbotMergeCoordinator struct {
	mu      sync.Mutex
	pending map[string]realbotPendingMerge
}

func newRealbotMergeCoordinator() *realbotMergeCoordinator {
	return &realbotMergeCoordinator{pending: make(map[string]realbotPendingMerge)}
}

func (c *realbotMergeCoordinator) reserve(marketID string, qty float64, hold time.Duration) bool {
	if c == nil || qty < minOnChainActionShares {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cur, ok := c.pending[marketID]; ok && time.Now().Before(cur.HoldUntil) {
		return false
	}
	c.pending[marketID] = realbotPendingMerge{Qty: qty, HoldUntil: time.Now().Add(hold)}
	return true
}

func (c *realbotMergeCoordinator) keepPending(marketID string, hold time.Duration) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cur, ok := c.pending[marketID]
	if !ok {
		return
	}
	until := time.Now().Add(hold)
	if until.After(cur.HoldUntil) {
		cur.HoldUntil = until
		c.pending[marketID] = cur
	}
}

func (c *realbotMergeCoordinator) clear(marketID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.pending, marketID)
	c.mu.Unlock()
}

func (c *realbotMergeCoordinator) pendingQty(marketID string) float64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cur, ok := c.pending[marketID]
	if !ok {
		return 0
	}
	if time.Now().After(cur.HoldUntil) {
		delete(c.pending, marketID)
		return 0
	}
	return cur.Qty
}

func launchBackgroundMerge(marketID, reason string, outcomes []string, conditionID string, mergeQty float64, numOutcomes int, trader *trading.RealTrader, engine *paper.Engine, splitInventory *paper.SplitInventory, tui *paper.TUI, coordinator *realbotMergeCoordinator) bool {
	if coordinator == nil || len(outcomes) != 2 || mergeQty < minOnChainActionShares {
		return false
	}
	if !coordinator.reserve(marketID, mergeQty, realbotMergeTimeout+45*time.Second) {
		return false
	}
	tui.LogEvent("[%s] 🔀 %s launching background merge for %.6f balanced shares; cleanup will not wait for confirmation", marketID, reason, mergeQty)
	go func() {
		mergeCtx, cancel := context.WithTimeout(context.Background(), realbotMergeTimeout)
		defer cancel()
		txHash, err := trader.MergeOnChain(mergeCtx, conditionID, mergeQty, numOutcomes)
		if err != nil {
			if txHash != "" && len(txHash) >= 10 && strings.Contains(strings.ToLower(err.Error()), "confirmation pending") {
				coordinator.keepPending(marketID, 45*time.Second)
				tui.LogEvent("[%s] ⚠️ %s background merge pending confirmation for %.6f shares | Tx: %s...", marketID, reason, mergeQty, txHash[:10])
				return
			}
			coordinator.clear(marketID)
			if txHash != "" && len(txHash) >= 10 {
				tui.LogEvent("[%s] ⚠️ %s background merge failed for %.6f shares: %v | Tx: %s...", marketID, reason, mergeQty, err, txHash[:10])
			} else {
				tui.LogEvent("[%s] ⚠️ %s background merge failed for %.6f shares: %v", marketID, reason, mergeQty, err)
			}
			return
		}
		coordinator.clear(marketID)
		result := engine.MergeForMarket(marketID, outcomes[0], outcomes[1], mergeQty)
		if splitInventory != nil {
			splitInventory.RecordMerge(marketID, outcomes[0], outcomes[1], mergeQty)
		}
		if txHash != "" && len(txHash) >= 10 {
			tui.LogEvent("[%s] 💰 %s merge confirmed for %.6f shares | Tx: %s...", marketID, reason, mergeQty, txHash[:10])
		} else {
			tui.LogEvent("[%s] 💰 %s merge confirmed for %.6f shares", marketID, reason, mergeQty)
		}
		if result != nil && result.PnL != 0 {
			tui.LogEvent("[%s] 💰 %s merge realized PnL: $%.2f", marketID, reason, result.PnL)
		}
	}()
	return true
}

func startupPositionsSummary(positions []trading.PositionInfo) string {
	totalShares := 0.0
	for _, pos := range positions {
		if pos.Size > 0 {
			totalShares += pos.Size
		}
	}
	return fmt.Sprintf("📊 Open positions: %d token(s), %.2f total shares", len(positions), totalShares)
}

func realbotNeutralRoundPnL(startingEquity, endingEquity, reconciliationDelta float64) float64 {
	return endingEquity - startingEquity - reconciliationDelta
}

func realbotPairQuoteAge(now time.Time, outcomes []string, quoteState map[string]realbotQuoteState) time.Duration {
	maxAge := time.Duration(0)
	sawMissing := false
	for _, outcome := range outcomes {
		updatedAt := quoteState[outcome].UpdatedAt
		if updatedAt.IsZero() {
			sawMissing = true
			continue
		}
		age := now.Sub(updatedAt)
		if age > maxAge {
			maxAge = age
		}
	}
	if sawMissing {
		return 24 * time.Hour
	}
	return maxAge
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("Error: %v", err)
	}
}

type realbotCLOBWarmer struct {
	client *api.RestClient
	trader *trading.RealTrader
}

func run() error {
	startTime := time.Now()
	fmt.Print("\033[H\033[2J") // Clear screen

	fmt.Println("╔═══════════════════════════════════════════════════════╗")
	fmt.Println("║     POLYMARKET REAL TRADING BOT                       ║")
	fmt.Println("║     ⚠️  WARNING: This uses REAL money! ⚠️              ║")
	fmt.Println("╚═══════════════════════════════════════════════════════╝")
	fmt.Printf("⏰ Started at: %s\n", startTime.Format("2006-01-02 15:04:05"))
	fmt.Println()

	// Load realbot settings + env-backed secrets
	cfg, err := core.LoadBotConfig("realbot")
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Setup signal handling FIRST so Ctrl+C works during prompts
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.RequireConfirm || !cfg.StartupWizardSeen {
		startupSettings, confirmed, err := paper.RunStartupWizard(paper.StartupWizardOptions{
			Title:          "REALBOT STARTUP",
			ProfileLabel:   "real wallet, live orders",
			Settings:       realbotTUISettingsFromConfig(cfg),
			FirstRun:       !cfg.StartupWizardSeen,
			RequireConfirm: cfg.RequireConfirm,
		})
		if err != nil {
			return fmt.Errorf("startup wizard failed: %w", err)
		}
		if !confirmed {
			fmt.Println("Startup cancelled.")
			return nil
		}
		applyRealbotTUISettings(cfg, startupSettings)
		cfg.StartupWizardSeen = true
		if err := cfg.SaveSettings(); err != nil {
			return fmt.Errorf("failed to save startup settings: %w", err)
		}
	}

	// Create real trader and auto-setup credentials/allowances if missing
	setupCtx, cancelSetup := context.WithTimeout(ctx, 2*time.Minute)
	realTrader, err := setup.EnsureRealTradingSetup(setupCtx, cfg)
	if err != nil {
		cancelSetup()
		return fmt.Errorf("failed to setup or create trader: %w", err)
	}
	cancelSetup() // Done with initial setup queries

	// Sync CLOB cached allowance with on-chain state
	fmt.Println("🔄 Syncing CLOB balance allowance...")
	if err := realTrader.UpdateBalanceAllowance(ctx); err != nil {
		fmt.Printf("⚠️  Failed to update balance allowance: %v\n", err)
	} else {
		fmt.Println("✅ CLOB balance allowance synced")
	}

	// Start real-time User WebSocket for instant fill tracking
	fmt.Println("🔌 Preparing User WebSocket for real-time fills...")
	if err := realTrader.StartUserWS(ctx); err != nil {
		fmt.Printf("⚠️  Failed to connect User WS (fill confirmation will wait on WS timeout only): %v\n", err)
	} else {
		fmt.Println("✅ User WebSocket ready")
	}

	// Display wallet info
	fmt.Println()
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Printf("🔑 Wallet: %s\n", realTrader.Address())

	// Use a short context for these initial balance checks
	initCtx, cancelInit := context.WithTimeout(ctx, 30*time.Second)

	// Get balance from CLOB API
	balance, err := realTrader.ForceRefreshBalance(initCtx)
	if err != nil {
		fmt.Printf("⚠️  Could not fetch balance: %v\n", err)
	} else {
		fmt.Printf("💵 Available Balance: $%.2f USDC\n", balance)
	}

	// Get positions
	positions, err := realTrader.GetPositions(initCtx)
	if err != nil {
		fmt.Printf("⚠️  Could not fetch positions: %v\n", err)
	} else if len(positions) > 0 {
		fmt.Println()
		fmt.Println(startupPositionsSummary(positions))
	} else {
		fmt.Println("📊 No open positions")
	}

	// Check MATIC for gas
	polygonClient := api.NewPolygonClient(cfg.PolygonRPCURL)
	maticBalance, err := polygonClient.GetMATICBalance(initCtx, realTrader.Address())
	if err != nil {
		fmt.Printf("⚠️  Could not fetch MATIC balance: %v\n", err)
	} else {
		fmt.Printf("⛽ Gas Balance: %.4f MATIC\n", maticBalance)
		if maticBalance < 0.1 {
			fmt.Println("   ⚠️  Low MATIC - you may need more for gas")
		}
	}

	fmt.Println("═══════════════════════════════════════════════════════")
	cancelInit() // Done with initial queries

	// Display safety settings
	fmt.Println()
	fmt.Println("🛡️  Safety Settings:")
	fmt.Printf("   • Max trade size: $%.2f\n", cfg.MaxTradeSize)
	if cfg.MaxDailyLoss > 0 {
		fmt.Printf("   • Max daily loss: $%.2f\n", cfg.MaxDailyLoss)
	} else {
		fmt.Println("   • Max daily loss: disabled (using 10% drawdown kill switch)")
	}
	fmt.Printf("   • Buy/sell execution margin floor: %.1f%%\n", cfg.BuyExecutionMarginFloorPercent)
	fmt.Println()

	restClient := api.NewRestClient(cfg.Exchange)

	// Resolution cache for on-chain market resolution checking (shared across all traders)
	// Realbot has polygon client for on-chain checks and exchange client for CLOB API
	resolutionCache := api.NewResolutionCache(polygonClient, realTrader.Exchange(), restClient)

	// emergencyCleanup ensures we don't leave hanging orders or unmerged positions
	emergencyCleanup := func() {
		// Give the overall cleanup up to 45 seconds, but each merge gets its own context
		overallCtx, cancelAll := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancelAll()

		fmt.Println("\n🧹 Running emergency cleanup...")

		// 1. Cancel all open orders
		if err := realTrader.CancelAll(overallCtx); err != nil {
			fmt.Printf("⚠️  Failed to cancel orders: %v\n", err)
		} else {
			fmt.Println("✅ All orders cancelled")
		}

		// 2. Identify and merge balanced positions
		positions, err := realTrader.GetPositions(overallCtx)
		if err != nil {
			fmt.Printf("⚠️  Could not fetch positions for merge: %v\n", err)
		} else if len(positions) > 0 {
			// Map positions by ConditionID
			condToPos := make(map[string][]trading.PositionInfo)
			for _, pos := range positions {
				if pos.ConditionID != "" {
					condToPos[pos.ConditionID] = append(condToPos[pos.ConditionID], pos)
				}
			}

			var wg sync.WaitGroup
			for condID, poses := range condToPos {
				if len(poses) < 2 {
					continue
				}

				minQty := poses[0].Size
				for _, p := range poses {
					if p.Size < minQty {
						minQty = p.Size
					}
				}

				if minQty >= minOnChainActionShares {
					// We need the number of outcomes to merge, fetch market info
					mInfo, err := realTrader.GetMarketInfo(overallCtx, condID)
					if err != nil {
						fmt.Printf("⚠️  Could not fetch market info for %s: %v\n", condID[:10], err)
						continue
					}

					// Realbot primarily trades markets where we hold all outcomes to merge
					if len(poses) < len(mInfo.Tokens) {
						continue
					}

					wg.Add(1)
					go func(cID string, mq float64, numOutcomes int) {
						defer wg.Done()
						fmt.Printf("💰 Merging %.6f pairs for market %s...\n", mq, cID[:10])
						// Independent 30s timeout per merge
						mergeCtx, mergeCancel := context.WithTimeout(context.Background(), 30*time.Second)
						defer mergeCancel()

						_, err := realTrader.MergeOnChain(mergeCtx, cID, mq, numOutcomes)
						if err != nil {
							fmt.Printf("❌ Merge failed for %s: %v\n", cID[:10], err)
						} else {
							fmt.Printf("✅ Merge successful for %s\n", cID[:10])
						}
					}(condID, minQty, len(mInfo.Tokens))
				}
			}

			// Wait for all concurrent merges to finish or overall timeout
			done := make(chan struct{})
			go func() {
				wg.Wait()
				close(done)
			}()

			select {
			case <-done:
				fmt.Println("✅ All emergency merges completed")
			case <-overallCtx.Done():
				fmt.Println("⚠️ Emergency cleanup timed out waiting for some merges")
			}
		}
	}

	// Panic recovery
	defer func() {
		if r := recover(); r != nil {
			core.RestoreTerminal()
			stack := make([]byte, 4096)
			length := runtime.Stack(stack, false)
			fmt.Printf("\n🚨 PANIC: %v\n%s\n", r, stack[:length])

			// Run emergency cleanup on panic
			emergencyCleanup()
		}
	}()

	// Watchdog for graceful shutdown
	go func() {
		<-ctx.Done()
		// If we receive a second interrupt during cleanup, force exit.
		// Use a separate signal channel since ctx is already cancelled.
		forceCh := make(chan os.Signal, 1)
		signal.Notify(forceCh, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-forceCh
			core.RestoreTerminal()
			fmt.Println("\n⚠️ Force exit requested")
			os.Exit(1)
		}()

		time.Sleep(10 * time.Second) // Give cleanup more time
		core.RestoreTerminal()
		fmt.Println("\n⚠️ Force exit: cleanup timed out")
		os.Exit(1)
	}()

	// Disable terminal echo
	disableEcho := exec.Command("stty", "-echo", "-icanon")
	disableEcho.Stdin = os.Stdin
	_ = disableEcho.Run()
	defer core.RestoreTerminal()

	engine := paper.NewEngine(balance)
	orderBook := paper.NewOrderBook()
	tui := paper.NewTUI(engine, orderBook)
	tui.SetMode("Real") // Show "Real Trading Mode" in footer (not "Paper Trading Mode")

	globalResWatcher = api.NewResolutionWatcher(cfg.PolygonRPCURL)
	if globalResWatcher != nil {
		globalResWatcher.Start(context.Background(), func(format string, args ...interface{}) {
			tui.LogEvent(format, args...)
		})
	}

	invWatcher := api.NewInventoryWatcher(cfg.PolygonRPCURL, realTrader.Address())
	if invWatcher != nil {
		invWatcher.Start(context.Background(), func(format string, args ...interface{}) {
			tui.LogEvent(format, args...)
		})
		invWatcher.RegisterCallback(func() {
			realTrader.InvalidateCTFBalanceCache()
		})
	}
	if err := os.MkdirAll("logs", 0o755); err != nil {
		fmt.Printf("⚠️  Could not create logs directory: %v\n", err)
	} else {
		issueLogPath := filepath.Join("logs", "realbot-issues.csv")
		issueLogger, logErr := core.NewCSVLogger(issueLogPath)
		if logErr != nil {
			fmt.Printf("⚠️  Could not start critical issue logger: %v\n", logErr)
		} else {
			tui.SetIssueLogger(issueLogger)
			defer tui.CloseIssueLogger()
			fmt.Printf("📝 Critical issue log: %s\n", issueLogPath)
		}
	}
	if cfg.EnableRawAPILog {
		rawAPILogPath := filepath.Join("logs", "realbot-polymarket-raw.jsonl")
		if err := realTrader.EnableRawAPILog(rawAPILogPath); err != nil {
			fmt.Printf("⚠️  Could not start raw Polymarket API log: %v\n", err)
		} else {
			defer func() { _ = realTrader.CloseRawAPILog() }()
			fmt.Printf("🧾 Raw Polymarket debug log: %s\n", rawAPILogPath)
		}
	} else {
		fmt.Println("⚡ Raw Polymarket API debug log disabled for lower latency")
	}

	// Seed settings panel with values from config (.env)
	tui.InitSettings(realbotTUISettingsFromConfig(cfg), func(s paper.TUISettings) {
		applyRealbotTUISettings(cfg, s)
		// Update the REST client exchange if it changed
		if restClient.Exchange != s.Exchange {
			restClient.Exchange = s.Exchange
		}

		_ = cfg.SaveSettings()
	})
	tui.SetTradeFactor(cfg.TradeScaleFactor)
	tui.SetMode("Real")

	// Start TUI — pass stop so a single Ctrl+C / [q] quits cleanly.
	if UseLiveUI {
		tui.StartRenderLoop(realbotUIInterval(tui.GetSettings()), stop)
		defer tui.Stop()
	}

	// Network health monitor
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				start := time.Now()
				// Use a lightweight check for latency
				pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
				_, err := restClient.GetMarketsByTimeframe(pingCtx, []string{"btc"}, "15m")
				cancel()
				if err == nil {
					tui.UpdateLatency(time.Since(start))
				}
			}
		}
	}()

	// Balance sync heartbeat keeps UI cash/equity aligned with live wallet state
	// even during quiet periods with no recent executions.
	go func() {
		ticker := time.NewTicker(realbotBalanceSyncInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				balanceCtx, cancel := context.WithTimeout(ctx, realbotBalanceSyncTimeout)
				newBal, balErr := realTrader.ForceRefreshBalance(balanceCtx)
				cancel()
				if balErr != nil {
					continue
				}
				engine.SyncBalanceNeutral(newBal)
				engine.RecalculateDrawdown()
			}
		}
	}()

	// Main trading loop - Keep running: after each round of markets ends, search for new ones.
	globalSplitStatus := make(map[string]bool)
	globalSplitInventories := make(map[string]*paper.SplitInventory)
	globalInitialSplits := make(map[string]float64)
	var splitMu sync.Mutex
	var splitTxMu sync.Mutex
	entryGate := newRealbotEntryGate()
	currentBalance := balance // Seed with the pre-fetched balance
	var copytradeWatchers *realbotCopytradeWatcherSet
	defer func() {
		if copytradeWatchers != nil {
			copytradeWatchers.stop()
		}
	}()

	for {
		// Check for shutdown signal before starting a new round
		select {
		case <-ctx.Done():
			goto shutdown
		default:
		}

		roundStartTime := time.Now()

		// Refresh balance at the start of each round for compounding
		{
			balCtx, balFn := context.WithTimeout(ctx, 10*time.Second)
			newBal, balErr := realTrader.ForceRefreshBalance(balCtx)
			balFn()
			if balErr != nil {
				tui.LogEvent("⚠️ Could not refresh balance: %v", balErr)
				// keep currentBalance from last known value
			} else {
				currentBalance = newBal
				engine.SyncBalanceNeutral(currentBalance)
				engine.RecalculateDrawdown()
			}
		}

		// Track starting equity for this round's PnL calculation
		startingEquity := engine.GetBookEquity()
		compoundMultiplier := engine.GetCompoundMultiplier()
		tui.LogEvent("📊 Balance $%.2f | %.2fx", currentBalance, compoundMultiplier)

		liveSettings := tui.GetSettings()
		arbMode := normalizePaperArbMode(liveSettings.PaperArbMode)
		copytradeTarget := realbotCopytradeTarget{}
		if arbMode != paperArbModeCopytrade && copytradeWatchers != nil {
			copytradeWatchers.stop()
			copytradeWatchers = nil
		}

		// Find markets
		tui.LogEvent("🔍 Scanning markets...")
		var markets map[string]*api.Market
		if arbMode == paperArbModeCopytrade {
			resolveCtx, resolveCancel := context.WithTimeout(ctx, 5*time.Second)
			target, targetErr := realbotResolveCopytradeTarget(resolveCtx, restClient, liveSettings)
			resolveCancel()
			if targetErr != nil {
				if copytradeWatchers != nil {
					copytradeWatchers.stop()
					copytradeWatchers = nil
				}
				tui.LogEvent("⚠️ Copytrade target unavailable: %v", targetErr)
				select {
				case <-time.After(10 * time.Second):
					continue
				case <-ctx.Done():
					goto shutdown
				}
			}
			copytradeTarget = target
			tui.LogEvent("🪞 Copytrade target %s → %s", target.Raw, target.Wallet)
			markets = mkt.FindMarkets(ctx, restClient, tui.GetSettings, func(format string, args ...interface{}) {
				tui.LogEvent(format, args...)
			})
		} else {
			markets = mkt.FindMarkets(ctx, restClient, tui.GetSettings, func(format string, args ...interface{}) {
				tui.LogEvent(format, args...)
			})
		}
		if len(markets) == 0 {
			if copytradeWatchers != nil {
				copytradeWatchers.stop()
				copytradeWatchers = nil
			}
			tui.LogEvent("⏳ No active markets found, waiting 30s before retry...")
			select {
			case <-time.After(30 * time.Second):
				continue // loop back and search again
			case <-ctx.Done():
				goto shutdown
			}
		}

		condSet := make(map[string]struct{}, len(markets))
		condIDs := make([]string, 0, len(markets))
		for _, market := range markets {
			if market.ConditionID == "" {
				continue
			}
			if _, exists := condSet[market.ConditionID]; exists {
				continue
			}
			condSet[market.ConditionID] = struct{}{}
			condIDs = append(condIDs, market.ConditionID)
		}
		if err := realTrader.SubscribeUserWSMarkets(ctx, condIDs...); err != nil {
			tui.LogEvent("⚠️ User WS subscription update failed: %v", err)
		}

		// Create a context for this specific round of trading.
		// Copytrade watchers are tied to this round context so they are stopped on
		// restart/round completion instead of stacking across rounds.
		roundCtx, roundCancel := context.WithCancel(ctx)

		copytradePoller := (*realbotCopytradePoller)(nil)
		if arbMode == paperArbModeCopytrade {
			copytradePoller = newRealbotCopytradePoller(copytradeTarget.Wallet, condIDs)
			if copytradePoller != nil {
				trackedMarkets := make([]*api.Market, 0, len(markets))
				for _, market := range markets {
					if market != nil {
						trackedMarkets = append(trackedMarkets, market)
					}
				}
				chainWSURL := api.ResolvePolygonWSURL(os.Getenv("POLYGON_WS_URL"), cfg.PolygonRPCURL)
				pendingWSURL := api.ResolvePolymarketPendingWSURL(os.Getenv("COPYTRADE_PENDING_WS_URL"), cfg.PolygonRPCURL)
				copytradeWatchers = ensureRealbotCopytradeWatcherSet(
					ctx,
					copytradeWatchers,
					copytradeTarget.Wallet,
					chainWSURL,
					pendingWSURL,
					polygonClient,
					restClient,
					trackedMarkets,
					func(format string, args ...interface{}) {
						tui.LogEvent(format, args...)
					},
				)
				if copytradeWatchers != nil {
					copytradeWatchers.attach(copytradePoller)
				}
				if !realbotCopytradeHasOnchainWatcher(copytradePoller) {
					tui.LogEvent("⚠️ Copytrade disabled: Polygon WS RPC watcher is required; public trades/positions API fallback is off")
				} else {
					tui.LogEvent("ℹ️ Copytrade WS-only mode active; public trades/positions API disabled")
					if !realbotCopytradeHasPendingWatcher(copytradePoller) {
						tui.LogEvent("ℹ️ Copytrade running in mined/onchain mode only; pending filtering requires Alchemy, so fills can trail the master")
					}
				}
			} else if copytradeWatchers != nil {
				copytradeWatchers.stop()
				copytradeWatchers = nil
			}
		}

		// Trade each market in parallel
		var wg sync.WaitGroup
		for assetID, market := range markets {
			marketID := mkt.ScopedMarketID(assetID, market)
			endTime, _ := paper.ParseEndTimeFromSlug(market.Slug)
			if mInfo, err := realTrader.GetMarketInfo(ctx, market.ConditionID); err == nil && mInfo.EndDateISO != "" {
				if parsed, err := time.Parse(time.RFC3339, mInfo.EndDateISO); err == nil {
					// Only override with API date if it's actually in the future OR if the market is already marked closed
					if parsed.After(time.Now()) || mInfo.Closed {
						endTime = parsed
					}
				}
			}
			outcomes := mkt.GetOutcomes(market)
			tui.AddMarket(marketID, market.Slug, outcomes, endTime)
			tui.LogEvent("🚀 %s → %s", marketID, endTime.Format("15:04"))

			// Create per-market Risk Manager
			riskConfig := paper.RiskConfig{
				DisableKillSwitch:  true,
				MaxExposure:        math.MaxFloat64, // Unlimited exposure (rely on kill switch for safety)
				MaxUnmatchedRatio:  0.20,            // 20% max unmatched
				MaxUnmatchedShares: 500.0,           // 500 shares max on one side
				SkewThreshold:      0.10,            // 10% skew triggers rebalance
				KillSwitchDrawdown: 0.10,            // 10% drawdown triggers kill switch (real money protection)
			}
			marketRiskMgr := paper.NewRiskManager(riskConfig, engine, orderBook, outcomes)

			wg.Add(1)
			go func(id string, m *api.Market, end time.Time, r *paper.RiskManager, bal float64, poller *realbotCopytradePoller) {
				defer wg.Done()
				// Create a sub-context for this specific trader to prevent goroutine leaks
				tCtx, tCancel := context.WithCancel(roundCtx)
				defer tCancel()

				defer func() {
					if r := recover(); r != nil {
						core.RestoreTerminal()
						stack := make([]byte, 4096)
						length := runtime.Stack(stack, false)
						fmt.Printf("\n🚨 TRADER PANIC [%s]: %v\n%s\n", id, r, stack[:length])
						emergencyCleanup()
					}
				}()
				tradeMarket(ctx, tCtx, id, m, end, realTrader, engine, orderBook, r, tui, restClient, cfg, bal, poller, globalSplitStatus, globalSplitInventories, globalInitialSplits, &splitMu, &splitTxMu, entryGate, resolutionCache)
			}(marketID, market, endTime, marketRiskMgr, currentBalance, copytradePoller)
		}

		// Goroutine to monitor for TUI restart requests
		go func() {
			ticker := time.NewTicker(1 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-roundCtx.Done():
					return
				case <-ticker.C:
					if tui.GetAndClearRestart() {
						tui.LogEvent("🔄 Settings saved. Restarting trading loop...")
						roundCancel() // This cancels the roundCtx, stopping all current traders
						return
					}
				}
			}
		}()

		// Wait for all markets in this round to finish
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
			tui.LogEvent("✅ Markets closed")
		case <-ctx.Done():
			goto shutdown
		case <-roundCtx.Done():
			// Round cancelled (e.g. via settings restart)
			tui.LogEvent("⚠️ Traders stopped for restart...")
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
		}

		// Ensure round is cancelled even if it finished normally
		roundCancel()

		// Sync engine with on-chain balance before calculating round PnL
		balanceSyncDelta := 0.0
		{
			endBalCtx, endBalFn := context.WithTimeout(ctx, 10*time.Second)
			if endBal, endBalErr := realTrader.ForceRefreshBalance(endBalCtx); endBalErr == nil {
				balanceSyncDelta = engine.SyncBalanceNeutral(endBal)
				engine.RecalculateDrawdown()
			} else {
				tui.LogEvent("⚠️ Round-end balance sync failed: %v", endBalErr)
			}
			endBalFn()
		}
		reconciliationDelta := 0.0
		{
			preReconcileBookEquity := engine.GetBookEquity()
			reconcileCtx, reconcileCancel := context.WithTimeout(ctx, 20*time.Second)
			if changed, reconcileErr := realbotReconcileTrackedRoundWalletTruth(reconcileCtx, markets, realTrader, engine, globalSplitInventories, &splitMu, tui); reconcileErr != nil {
				tui.LogEvent("⚠️ Round-end wallet-truth reconciliation incomplete: %v", reconcileErr)
			} else if changed > 0 {
				tui.LogEvent("🧾 Round-end wallet-truth reconciliation restored %d tracked market(s)", changed)
			}
			reconcileCancel()
			reconciliationDelta = engine.GetBookEquity() - preReconcileBookEquity
			if math.Abs(reconciliationDelta) >= 0.005 {
				tui.LogEvent("🧮 Excluding wallet-truth sync delta %+0.2f from round PnL", reconciliationDelta)
			}
		}

		// Calculate round PnL from settled/book equity so unresolved carry stays neutral
		// until it is actually sold, merged, or redeemed.
		roundPnL := realbotNeutralRoundPnL(startingEquity, engine.GetBookEquity(), reconciliationDelta+balanceSyncDelta)
		engine.UpdateCompoundMultiplier(roundPnL, startingEquity)
		if roundPnL > 0 {
			tui.LogEvent("📈 PROFIT! Round PnL: +$%.2f", roundPnL)
		} else if roundPnL < 0 {
			tui.LogEvent("📉 Loss. Round PnL: $%.2f", roundPnL)
		} else {
			tui.LogEvent("✅ No change")
		}
		tui.LogEvent("🔄 Next round")

		// Release stale keep-alive connections before the next search phase.
		restClient.CloseIdleConnections()
		tui.ClearMarkets()
		orderBook.CancelAllOrders()
		engine.ClearMarketData()

		// Prevent tight loops if all markets exit instantly
		if elapsed := time.Since(roundStartTime); elapsed < 10*time.Second {
			select {
			case <-time.After(10*time.Second - elapsed):
			case <-ctx.Done():
				goto shutdown
			}
		}
	} // End of main round loop

shutdown:
	tui.Stop()
	fmt.Println("\n👋 Bot stopped.")
	emergencyCleanup()
	return nil
}

func tradeMarket(globalCtx context.Context, ctx context.Context, id string, market *api.Market, endTime time.Time,
	trader *trading.RealTrader, engine *paper.Engine, orderBook *paper.OrderBook,
	riskMgr *paper.RiskManager, tui *paper.TUI, restClient *api.RestClient, cfg *core.Config, startingBalance float64,
	copytradePoller *realbotCopytradePoller,
	globalSplitStatus map[string]bool, globalSplitInventories map[string]*paper.SplitInventory, globalInitialSplits map[string]float64, splitMu *sync.Mutex, splitTxMu *sync.Mutex, entryGate *realbotEntryGate, resolutionCache *api.ResolutionCache) {

	if market != nil && market.ConditionID != "" {
		infoCtx, infoCancel := context.WithTimeout(ctx, 3*time.Second)
		info, err := trader.GetMarketInfo(infoCtx, market.ConditionID)
		infoCancel()
		if err == nil {
			if changed, matched := realbotCanonicalizeMarketTokens(market, info); changed {
				tui.LogEvent("[%s] ℹ️ Canonicalized token mapping from CLOB market info (%d/%d tokens matched)", id, matched, len(market.Tokens))
			}
		}
	}

	tokenMap := make(map[string]string)
	tokenToOutcome := make(map[string]string)
	for _, token := range market.Tokens {
		tokenMap[token.TokenID] = token.Outcome
		tokenToOutcome[token.TokenID] = token.Outcome
	}

	outcomes := mkt.GetOutcomes(market)

	// Setup WebSocket
	wsMgr := api.NewWSManager(cfg.Exchange, cfg.KalshiAPIKey, cfg.KalshiPK, "")
	if err := wsMgr.Connect(ctx); err != nil {
		tui.LogEvent("[%s] ❌ WS connect failed: %v", id, err)
		return
	}
	defer wsMgr.Close()

	// Subscribe to order books
	var assetIDs []string
	for _, token := range market.Tokens {
		assetIDs = append(assetIDs, token.TokenID)
	}
	sub := map[string]interface{}{
		"type":                   "market",
		"assets_ids":             assetIDs,
		"custom_feature_enabled": true,
	}
	if err := wsMgr.Subscribe(ctx, sub); err != nil {
		tui.LogEvent("[%s] ❌ Subscribe failed: %v", id, err)
		return
	}

	wsMsgChan := wsMgr.StartStreaming(ctx)
	// Fetch fee rates for the tokens
	tokenFeeRates := make(map[string]int)
	for tid, outcome := range tokenMap {
		// Retry fee fetch a few times at startup
		var rate int
		var err error
		for attempt := 1; attempt <= 3; attempt++ {
			rate, err = restClient.GetFeeRate(ctx, tid)
			if err == nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}

		if err == nil {
			tokenFeeRates[outcome] = rate
			// 15m markets require 1000 bps authorization even if endpoint returns 0
			if rate == 0 {
				tokenFeeRates[outcome] = 1000
			} else {
				tui.LogEvent("[%s] ℹ️ Fee rate for %s: %.2f%% (%d bps)", id, outcome, float64(rate)/100.0, rate)
			}
		} else {
			// If API fails, use 1000 bps (10%) which is the standard taker fee for 15m markets
			tokenFeeRates[outcome] = 1000
			tui.LogEvent("[%s] ⚠️ Fee fetch failed, using default 1000 bps", id)
		}
	}

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
	lastBinanceLog := time.Time{}
	lastSplitSell := time.Time{}    // Track last split sell to avoid rapid-fire
	nextSplitAttempt := time.Time{} // Cooldown for retrying failed splits
	var panicBuyCooldown time.Time  // Cooldown for panic buys after successful auto-cleanup
	var nextLiveRecoveryAttempt time.Time
	var lastDustRecoveryNotice time.Time
	makerQuotes := make(map[string]*realbotMakerQuote)
	lastMakerSync := time.Time{}
	mergeCoordinator := newRealbotMergeCoordinator()

	// Initial balance tracking
	currentBalance := startingBalance
	// currentCash := startingBalance // Unused after removing balance checks

	// The global balance sync ticker handles balance and allowance updates.
	// We no longer run a per-market ticker here to avoid RPC spam.

	// Helper to get token ID from outcome
	getTokenID := func(outcome string) string {
		for tid, out := range tokenToOutcome {
			if out == outcome {
				return tid
			}
		}
		return ""
	}

	var binanceFeed *api.BinanceFuturesPriceFeed
	if symbol := realbotBinanceSymbolForMarket(id, cfg); symbol != "" {
		binanceFeed = api.NewBinanceFuturesPriceFeed(symbol, core.ResolveBinanceSignalLookback(cfg))
		binanceFeed.Start(ctx)
	}

	// ═══════════════════════════════════════════════════════════════════════════
	// SPLIT STRATEGY INITIALIZATION
	// Create split inventory tracker (separate from bought shares)
	// ═══════════════════════════════════════════════════════════════════════════
	splitMu.Lock()
	splitInventory, exists := globalSplitInventories[market.ConditionID]
	if !exists {
		splitInventory = paper.NewSplitInventory()
		globalSplitInventories[market.ConditionID] = splitInventory
	}
	splitMu.Unlock()

	engine.RegisterSplitInventory(splitInventory) // Register for equity calculation
	tui.RegisterSplitInventory(splitInventory)    // Register for TUI display
	takerCloseAttempted := false
	var takerCloseExecutedAt time.Time // When taker close buy was confirmed (for merge-buffer cooldown)
	var lastTakerCloseLog time.Time
	var lastTakerCloseLogKey string
	var lastTakerCloseQuoteRefresh time.Time
	usWeekdayGateClosedLogged := false
	preserveWalletTruth := false
	defer func() {
		if !preserveWalletTruth {
			tui.ClearWalletTruthPositions(id)
		}
	}()
	replenishCtrl := paper.NewReplenishController() // Debounce replenish goroutines
	var nextNearCloseCleanup time.Time
	var nearExpiryNoticeSent bool

	walletTruthTokenIDs := make([]string, 0, len(tokenToOutcome))
	for tokenID := range tokenToOutcome {
		walletTruthTokenIDs = append(walletTruthTokenIDs, tokenID)
	}
	sort.Strings(walletTruthTokenIDs)

	refreshWalletTruth := func(timeout time.Duration) {
		if len(walletTruthTokenIDs) > 0 {
			trader.InvalidateCTFBalanceCache(walletTruthTokenIDs...)
		}
		truthCtx, truthCancel := context.WithTimeout(ctx, timeout)
		defer truthCancel()
		_, _ = syncWalletTruthPositions(truthCtx, id, tokenToOutcome, trader, engine, splitInventory, tui)
	}
	refreshWalletTruth(5 * time.Second)
	copytradeState := newRealbotCopytradeState()

	lastReconnectCount := int32(0)
	lastWsWarnTime := time.Time{}
	lastForceReconnect := time.Time{}
	lastRestFallbackPoll := time.Time{}
	restFallbackActive := false
	restRecoveryLogged := false
	wsChannelClosed := false
	restFallbackQuoteAge := core.ResolveRestFallbackQuoteAge(cfg)
	restFallbackPollInterval := core.ResolveRestFallbackPollInterval(cfg)

	for {
		select {
		case <-ctx.Done():
			isShutdown := globalCtx.Err() != nil
			timeToExpiry := time.Until(endTime)
			liveCfg := tui.GetSettings()
			cancelMakerCtx, cancelMaker := context.WithTimeout(context.Background(), 10*time.Second)
			realbotCancelAllMakerQuotes(cancelMakerCtx, id, "trader stopping", trader, engine, tui, makerQuotes)
			cancelMaker()

			if realbotTakerCloseHoldMode(liveCfg) {
				if realbotHasEnginePositionsForMarket(engine, id) {
					preserveWalletTruth = true
					tui.LogEvent("[%s] ⏳ Trader stopping: preserving taker-close inventory for post-resolution redemption", id)
				}
				return
			}
			if realbotCopytradeHoldMode(liveCfg) {
				if realbotHasEnginePositionsForMarket(engine, id) {
					preserveWalletTruth = true
					refreshWalletTruth(5 * time.Second)
					tui.LogEvent("[%s] ⏳ Trader stopping: preserving copytrade inventory for target-led exit or redemption", id)
				}
				return
			}

			// TUI Restart logic: Preserve inventory if active
			if !isShutdown && timeToExpiry > 30*time.Second {
				tui.LogEvent("[%s] ⚠️ TUI Restart: Preserving split inventory for next round", id)
				return
			}

			cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancelCleanup()
			if err := settleMarketInventory(cleanupCtx, id, market, outcomes, tokenFeeRates, trader, engine, splitInventory, tui, restClient, timeToExpiry > 2*time.Second, tui.GetSettings().MinAskPrice, "EMERGENCY EXIT", mergeCoordinator); err != nil {
				tui.LogEvent("[%s] ⚠️ Emergency cleanup failed: %v", id, err)
			}
			return
		default:
		}

		// Check if market ended
		if time.Now().After(endTime.Add(5 * time.Second)) {
			tui.LogEvent("[%s] ⏰ Closed", id)
			cancelMakerCtx, cancelMaker := context.WithTimeout(context.Background(), 10*time.Second)
			realbotCancelAllMakerQuotes(cancelMakerCtx, id, "market closed", trader, engine, tui, makerQuotes)
			cancelMaker()
			liveCfg := tui.GetSettings()
			if realbotTakerCloseHoldMode(liveCfg) {
				if realbotHasEnginePositionsForMarket(engine, id) {
					preserveWalletTruth = true
					refreshWalletTruth(5 * time.Second)
					tui.LogEvent("[%s] ⏳ Taker-close inventory locked in; waiting for market resolution and redemption", id)
				}
				go func(marketID, condID string, marketOutcomes []string, marketEndTime time.Time) {
					checkRedemption(context.Background(), marketID, condID, marketOutcomes, marketEndTime, trader, engine, tui, resolutionCache)
				}(id, market.ConditionID, append([]string(nil), outcomes...), endTime)
				return
			}
			if realbotCopytradeHoldMode(liveCfg) {
				if realbotHasEnginePositionsForMarket(engine, id) {
					preserveWalletTruth = true
					refreshWalletTruth(5 * time.Second)
					tui.LogEvent("[%s] ⏳ Copytrade inventory preserved at close; waiting for resolution/redemption instead of forced cleanup", id)
				}
				go func(marketID, condID string, marketOutcomes []string, marketEndTime time.Time) {
					checkRedemption(context.Background(), marketID, condID, marketOutcomes, marketEndTime, trader, engine, tui, resolutionCache)
				}(id, market.ConditionID, append([]string(nil), outcomes...), endTime)
				return
			}
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 45*time.Second)
			if err := settleMarketInventory(cleanupCtx, id, market, outcomes, tokenFeeRates, trader, engine, splitInventory, tui, restClient, false, tui.GetSettings().MinAskPrice, "POST CLOSE", mergeCoordinator); err != nil {
				tui.LogEvent("[%s] ⚠️ Post-close cleanup skipped: %v", id, err)
			}
			cleanupCancel()
			go func(marketID, condID string, marketOutcomes []string, marketEndTime time.Time) {
				checkRedemption(context.Background(), marketID, condID, marketOutcomes, marketEndTime, trader, engine, tui, resolutionCache)
			}(id, market.ConditionID, append([]string(nil), outcomes...), endTime)
			return
		}

		timeToExpiry := time.Until(endTime)

		liveCfg := tui.GetSettings()
		usNow := core.USTime(time.Now())

		weekdayTradingAllowed := true
		if liveCfg.TradingHoursMode == "weekdays trade only" {
			weekdayTradingAllowed = core.IsUSWeekday(usNow)
		} else if liveCfg.TradingHoursMode == "us open only" {
			weekdayTradingAllowed = core.IsUSMarketOpen(time.Now())
		}

		if !weekdayTradingAllowed {
			if !usWeekdayGateClosedLogged {
				tui.LogEvent("[%s] 🗓️ Trading gate closed at %s - new trades paused", id, usNow.Format("Mon 2006-01-02 15:04:05 MST"))
				usWeekdayGateClosedLogged = true
			}
		} else if usWeekdayGateClosedLogged {
			tui.LogEvent("[%s] ✅ Trading gate open at %s - trading resumed", id, usNow.Format("Mon 2006-01-02 15:04:05 MST"))
			usWeekdayGateClosedLogged = false
		}
		mergeBuffer := time.Duration(cfg.SplitMergeBufferSeconds) * time.Second
		if weekdayTradingAllowed && !realbotCopytradeHoldMode(liveCfg) && realbotShouldRunNearExpiryCleanup(liveCfg, timeToExpiry, mergeBuffer) {
			// If taker close just fired, suppress sell actions for 15s to prevent racing
			// against the just-placed GTC buy order. The merge buffer cleanup would
			// otherwise sell the shares we just bought before the order fully fills.
			takerCloseCooldownActive := !takerCloseExecutedAt.IsZero() && time.Since(takerCloseExecutedAt) < 15*time.Second
			allowCleanupSell := !takerCloseCooldownActive
			if time.Now().After(nextNearCloseCleanup) {
				if !nearExpiryNoticeSent {
					if takerCloseCooldownActive {
						tui.LogEvent("[%s] ⏳ Near expiry: merge-only (taker close cooldown active)", id)
					} else {
						tui.LogEvent("[%s] ⏳ Near expiry: settling only", id)
					}
					nearExpiryNoticeSent = true
				}
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
				if err := settleMarketInventory(cleanupCtx, id, market, outcomes, tokenFeeRates, trader, engine, splitInventory, tui, restClient, allowCleanupSell, tui.GetSettings().MinAskPrice, "NEAR EXPIRY", mergeCoordinator); err != nil {
					tui.LogEvent("[%s] ⚠️ Near-expiry cleanup failed: %v", id, err)
				}
				cleanupCancel()
				nextNearCloseCleanup = time.Now().Add(5 * time.Second)
			}
			time.Sleep(250 * time.Millisecond)
			continue
		}

		// Check kill switch - DON'T EXIT, just pause trading
		// Exiting would leave positions unmatched; better to hold until expiration
		killSwitchActive := riskMgr.IsKillSwitchTriggered()

		_, _, reconnects, _ := wsMgr.GetStats()
		if reconnects > lastReconnectCount {
			tui.LogEvent("[%s] 🔄 WebSocket reconnected (attempt #%d)", id, reconnects)
			lastReconnectCount = reconnects
			wsChannelClosed = false
		}

		// ============ FAST WEBSOCKET PROCESSING ============
		messagesProcessed := 0
		for {
			select {
			case msg, ok := <-wsMsgChan:
				if !ok {
					// Channel closed - this only happens when context is cancelled
					// Check if we should exit or if it's a reconnection scenario
					select {
					case <-ctx.Done():
						tui.LogEvent("[%s] ⚠️ WS closed (context cancelled)", id)
						return
					default:
						// Context still active but channel closed unexpectedly.
						// Treat this as a reconnect condition instead of continuing silently.
						wsChannelClosed = true
						goto doneWS
					}
				}
				wsChannelClosed = false
				messagesProcessed++

				// Parse and process WebSocket message immediately.
				//
				// Polymarket CLOB WS sends:
				//   1. Book snapshots ("book") on subscribe/reconnect.
				//   2. Price-change deltas ("price_change") with changed levels and
				//      explicit best_bid / best_ask values.
				//   3. Best-bid-ask updates ("best_bid_ask") when subscribed with
				//      custom_feature_enabled.
				//
				// IMPORTANT: changed levels still update the local depth cache, but
				// explicit best_bid / best_ask fields now take priority for BBO so
				// one-sided book removals do not leave stale top-of-book behind.
				if books, err := api.ParseOrderBooks(msg); err == nil && len(books) > 0 {
					for _, b := range books {
						outcome := tokenToOutcome[b.AssetID]
						if outcome == "" {
							continue
						}
						updatedAt := realbotQuoteTimestampOrNow(b.Timestamp)
						if realbotShouldSkipStaleQuoteUpdate(quoteState, outcome, updatedAt, tokenBids[outcome], tokenAsks[outcome]) {
							continue
						}

						bid, ask := 0.0, 0.0
						for _, order := range b.Bids {
							p, err := strconv.ParseFloat(order.Price, 64)
							if err != nil {
								continue
							}
							if p > 0 && p <= 1.0 && p > bid {
								bid = p
							}
						}
						for _, order := range b.Asks {
							p, err := strconv.ParseFloat(order.Price, 64)
							if err != nil {
								continue
							}
							if p > 0 && p <= 1.0 && (ask == 0 || p < ask) {
								ask = p
							}
						}

						// WS Snapshot is absolute state.
						if bid > 0 && ask > 0 && !realbotHasSaneTopOfBook(bid, ask) {
							// Reject crossed/wide snapshot and clear state.
							tokenBids[outcome] = 0
							tokenAsks[outcome] = 0
							tokenFullBids[outcome] = nil
							tokenFullAsks[outcome] = nil
							continue
						}

						tokenBids[outcome] = bid
						tokenAsks[outcome] = ask

						// Always update full depth from snapshots
						tokenFullBids[outcome] = mkt.LevelsToPriceDepth(b.Bids, true)
						tokenFullAsks[outcome] = mkt.LevelsToPriceDepth(b.Asks, false)
						quoteState[outcome] = realbotQuoteState{UpdatedAt: updatedAt, Source: "ws"}

						if bid > 0 && ask > 0 {
							mid := (bid + ask) / 2
							engine.UpdateMarketData(id, outcome, mid, bid, ask)
							polySignalTracker.Record(outcome, bid, ask, updatedAt)
						}
					}
				} else if update, err := api.ParsePriceUpdate(msg); err == nil && len(update.PriceChanges) > 0 {
					// ── Price-change delta ─────────────────────────────────
					foundForThisMarket := false
					touchedOutcomes := make(map[string]bool)
					type explicitTopOfBook struct {
						bid    float64
						ask    float64
						hasBid bool
						hasAsk bool
					}
					explicitTopByOutcome := make(map[string]explicitTopOfBook)

					for _, pc := range update.PriceChanges {
						outcome := tokenToOutcome[pc.AssetID]
						if outcome == "" {
							continue
						}
						foundForThisMarket = true
						touchedOutcomes[outcome] = true
						p, errP := strconv.ParseFloat(pc.Price, 64)
						s, errS := strconv.ParseFloat(pc.Size, 64)
						if errP != nil || errS != nil || p <= 0 {
							continue
						}

						switch pc.Side {
						case "BUY":
							tokenFullBids[outcome] = mkt.ApplyDelta(tokenFullBids[outcome], p, s, true)
						case "SELL":
							tokenFullAsks[outcome] = mkt.ApplyDelta(tokenFullAsks[outcome], p, s, false)
						}

						top := explicitTopByOutcome[outcome]
						if bestBid, ok := parseWSQuotedPrice(pc.BestBid); ok {
							top.bid = bestBid
							top.hasBid = true
						}
						if bestAsk, ok := parseWSQuotedPrice(pc.BestAsk); ok {
							top.ask = bestAsk
							top.hasAsk = true
						}
						if top.hasBid || top.hasAsk {
							explicitTopByOutcome[outcome] = top
						}
					}

					// Update best bids/asks based on the new full depth
					mkt.RefreshTopOfBookFromDepth(outcomes, tokenFullBids, tokenFullAsks, tokenBids, tokenAsks)
					for _, outcome := range outcomes {
						if top, ok := explicitTopByOutcome[outcome]; ok {
							if top.hasBid {
								tokenBids[outcome] = top.bid
							}
							if top.hasAsk {
								tokenAsks[outcome] = top.ask
							}
						}

						if tokenBids[outcome] > 0 && tokenAsks[outcome] > 0 {
							// Check for crossed/wide local book (WS state corruption or missing delete delta)
							if !realbotHasSaneTopOfBook(tokenBids[outcome], tokenAsks[outcome]) {
								// Clear corrupted data
								tokenBids[outcome] = 0
								tokenAsks[outcome] = 0
								tokenFullBids[outcome] = nil
								tokenFullAsks[outcome] = nil
								continue
							}

							mid := (tokenBids[outcome] + tokenAsks[outcome]) / 2
							engine.UpdateMarketData(id, outcome, mid, tokenBids[outcome], tokenAsks[outcome])
							polySignalTracker.Record(outcome, tokenBids[outcome], tokenAsks[outcome], time.Now())
						}
					}

					if foundForThisMarket {
						now := time.Now()
						for outcome := range touchedOutcomes {
							quoteState[outcome] = realbotQuoteState{UpdatedAt: now, Source: "ws"}
						}
					}
				} else if bbo, err := api.ParseBestBidAsk(msg); err == nil && strings.EqualFold(strings.TrimSpace(bbo.EventType), "best_bid_ask") && bbo.AssetID != "" {
					outcome := tokenToOutcome[bbo.AssetID]
					if outcome == "" {
						continue
					}
					updatedAt := realbotQuoteTimestampOrNow(bbo.Timestamp)
					if realbotShouldSkipStaleQuoteUpdate(quoteState, outcome, updatedAt, tokenBids[outcome], tokenAsks[outcome]) {
						continue
					}
					if bestBid, ok := parseWSQuotedPrice(bbo.BestBid); ok {
						tokenBids[outcome] = bestBid
					}
					if bestAsk, ok := parseWSQuotedPrice(bbo.BestAsk); ok {
						tokenAsks[outcome] = bestAsk
					}
					if tokenBids[outcome] > 0 && tokenAsks[outcome] > 0 && !realbotHasSaneTopOfBook(tokenBids[outcome], tokenAsks[outcome]) {
						tokenBids[outcome] = 0
						tokenAsks[outcome] = 0
					}
					if tokenBids[outcome] > 0 && tokenAsks[outcome] > 0 {
						mid := (tokenBids[outcome] + tokenAsks[outcome]) / 2
						engine.UpdateMarketData(id, outcome, mid, tokenBids[outcome], tokenAsks[outcome])
						polySignalTracker.Record(outcome, tokenBids[outcome], tokenAsks[outcome], updatedAt)
					}
					quoteState[outcome] = realbotQuoteState{UpdatedAt: updatedAt, Source: "ws-bbo"}
				} else if book, err := api.ParseOrderBook(msg); err == nil && book.AssetID != "" {
					// ── Book snapshot (single object) ──────────────────────
					bid, ask := 0.0, 0.0
					for _, b := range book.Bids {
						p, _ := strconv.ParseFloat(b.Price, 64)
						if p > 0 && p <= 1.0 && p > bid {
							bid = p
						}
					}
					for _, order := range book.Asks {
						p, _ := strconv.ParseFloat(order.Price, 64)
						if p > 0 && p <= 1.0 && (ask == 0 || p < ask) {
							ask = p
						}
					}

					if bid > 0 && ask > 0 && !realbotHasSaneTopOfBook(bid, ask) {
						continue // Reject crossed snapshot
					}

					outcome := tokenToOutcome[book.AssetID]
					if outcome != "" {
						updatedAt := realbotQuoteTimestampOrNow(book.Timestamp)
						if realbotShouldSkipStaleQuoteUpdate(quoteState, outcome, updatedAt, tokenBids[outcome], tokenAsks[outcome]) {
							continue
						}
						// Guard: only persist valid (non-zero) prices.
						if bid > 0 {
							tokenBids[outcome] = bid
						}
						if ask > 0 {
							tokenAsks[outcome] = ask
						}
						if bid > 0 && ask > 0 {
							mid := (bid + ask) / 2
							engine.UpdateMarketData(id, outcome, mid, bid, ask)
							polySignalTracker.Record(outcome, bid, ask, updatedAt)
						}
						tokenFullBids[outcome] = mkt.LevelsToPriceDepth(book.Bids, true)
						tokenFullAsks[outcome] = mkt.LevelsToPriceDepth(book.Asks, false)
						quoteState[outcome] = realbotQuoteState{UpdatedAt: updatedAt, Source: "ws"}
					}
				}
			default:
				goto doneWS
			}
		}
	doneWS:

		// Final safety check: scrub any crossed/wide books that survived the WS loop.
		for _, outcome := range outcomes {
			if tokenBids[outcome] > 0 && tokenAsks[outcome] > 0 && !realbotHasSaneTopOfBook(tokenBids[outcome], tokenAsks[outcome]) {
				tokenBids[outcome] = 0
				tokenAsks[outcome] = 0
				tokenFullBids[outcome] = nil
				tokenFullAsks[outcome] = nil
			}
		}
		// Only clear pair quotes when no outcome has a high bid. When one side
		// has a valid high-price bid (≥0.60), the complement naturally has sparse
		// or empty asks — this is normal market microstructure, not a WS error.
		// Clearing both outcomes here would cause the "awaiting liquidity" freeze
		// during high-volatility conditions near extreme prices.
		if realbotShouldClearLocalPairQuotes(outcomes, tokenBids, tokenAsks) {
			for _, outcome := range outcomes {
				tokenBids[outcome] = 0
				tokenAsks[outcome] = 0
				tokenFullBids[outcome] = nil
				tokenFullAsks[outcome] = nil
			}
		}
		realbotSyncDisplayQuotes(outcomes, tokenBids, tokenAsks, displayBids, displayAsks, false)

		// Also update order book depth for live display
		bidDepth := make(map[string][]paper.MarketLevel)
		askDepth := make(map[string][]paper.MarketLevel)

		for _, outcome := range outcomes {
			if bids, ok := tokenFullBids[outcome]; ok {
				bidDepth[outcome] = append([]paper.MarketLevel(nil), bids...)
			}
			if asks, ok := tokenFullAsks[outcome]; ok {
				askDepth[outcome] = append([]paper.MarketLevel(nil), asks...)
			}
		}
		tui.UpdateOrderBookDepth(id, bidDepth, askDepth)

		// Track feed age in the UI, but do not treat quiet books as a transport failure.
		wsTimeSinceMsg := wsMgr.TimeSinceLastDataMessage()
		tui.UpdateWSLatency(wsTimeSinceMsg)
		tui.UpdateWSPingLatency(wsMgr.PingLatency())
		terminalBookState := realbotLooksLikeTerminalBook(outcomes, tokenBids, tokenAsks)
		pairQuoteAge := realbotPairQuoteAge(time.Now(), outcomes, quoteState)
		needsWSReconnect := realbotShouldReconnectWS(outcomes, tokenBids, tokenAsks, pairQuoteAge, restFallbackQuoteAge, terminalBookState)
		localPairSane := realbotHasSanePairQuotes(outcomes, tokenBids, tokenAsks)
		shouldRestFallback := !terminalBookState &&
			!localPairSane &&
			pairQuoteAge > restFallbackQuoteAge &&
			time.Since(lastRestFallbackPoll) >= restFallbackPollInterval

		if shouldRestFallback {
			wasFallbackActive := restFallbackActive
			restFallbackActive = true
			recovered := handleRestFallbackWithDepth(ctx, id, pairQuoteAge, tokenMap, tokenBids, tokenAsks, displayBids, displayAsks, tokenFullBids, tokenFullAsks, quoteState, polySignalTracker, engine, restClient, tui, wasFallbackActive && !restRecoveryLogged)
			lastRestFallbackPoll = time.Now()
			if recovered {
				restFallbackActive = false
				restRecoveryLogged = false
			} else if pairQuoteAge >= 10*time.Second {
				restRecoveryLogged = true
			}
		} else {
			restFallbackActive = false
			restRecoveryLogged = false
		}

		quotesChanged := !realbotQuoteMapsEqual(outcomes, displayBids, displayAsks, publishedBids, publishedAsks)
		latestQuoteAt, latestQuoteSource := realbotLatestQuoteUpdate(outcomes, quoteState)
		displayUsable := realbotDisplayHasUsableQuotes(outcomes, displayBids, displayAsks)
		freshnessAdvanced := displayUsable && !latestQuoteAt.IsZero() && latestQuoteAt.After(lastPublishedQuoteAt)
		if quotesChanged || freshnessAdvanced {
			tui.UpdateMarketPricesWithSourceAt(id, displayBids, displayAsks, realbotNormalizeDisplaySource(latestQuoteSource), latestQuoteAt)
			realbotStorePublishedQuotes(outcomes, displayBids, displayAsks, publishedBids, publishedAsks)
			if freshnessAdvanced {
				lastPublishedQuoteAt = latestQuoteAt
			}
		}

		if needsWSReconnect && wsMgr.IsConnected() && !wsChannelClosed && time.Since(lastForceReconnect) > realbotWSForceReconnect {
			lastForceReconnect = time.Now()
			wsMgr.ForceReconnect()
			if time.Since(lastWsWarnTime) > realbotWSWarnInterval {
				tui.LogEvent("[%s] 🔄 WS local book invalid - reconnecting...", id)
				lastWsWarnTime = time.Now()
			}
		}

		if !wsMgr.IsConnected() && !wsChannelClosed && time.Since(lastForceReconnect) > realbotWSForceReconnect {
			lastForceReconnect = time.Now()
			wsMgr.ForceReconnect()
			if time.Since(lastWsWarnTime) > realbotWSWarnInterval {
				tui.LogEvent("[%s] 🔌 WS disconnected - reconnecting...", id)
				lastWsWarnTime = time.Now()
			}
		}

		if wsChannelClosed && time.Since(lastWsWarnTime) > realbotWSWarnInterval {
			tui.LogEvent("[%s] ⚠️ WebSocket closed - attempting reconnect", id)
			lastWsWarnTime = time.Now()
			lastForceReconnect = time.Now()
			wsMgr.ForceReconnect()
		}

		// --- TAKER CLOSE MARKET LOGIC ---
		// React only after we have drained the current WS queue so the decision
		// follows the latest local WS book state.
		takerCloseTime := time.Duration(liveCfg.TakerCloseMarketTime) * time.Second
		if weekdayTradingAllowed && realbotTakerCloseHoldMode(liveCfg) && timeToExpiry > 0 && timeToExpiry <= takerCloseTime {
			if !takerCloseAttempted {
				bestOutcome, highestPrice := realbotBestTakerCloseOutcomePrice(outcomes, tokenBids, tokenAsks)
				minPrice := normalizedRealbotTakerCloseMinPrice(liveCfg)
				if bestOutcome == "" && time.Since(lastTakerCloseQuoteRefresh) > realbotTakerCloseQuoteRefresh {
					lastTakerCloseQuoteRefresh = time.Now()
					if wsMgr.IsConnected() && !wsChannelClosed && time.Since(lastForceReconnect) > realbotWSForceReconnect {
						lastForceReconnect = time.Now()
						wsMgr.ForceReconnect()
					}
				}
				if bestOutcome == "" || highestPrice < minPrice {
					if highestPrice <= 0 {
						if realbotShouldLogTakerCloseState(&lastTakerCloseLog, &lastTakerCloseLogKey, "waiting", realbotTakerCloseLogInterval) {
							tui.LogEvent("[%s] ⏳ Taker close awaiting valid quote (needs >= $%.3f)", id, minPrice)
						}
					} else if realbotShouldLogTakerCloseState(&lastTakerCloseLog, &lastTakerCloseLogKey, "waiting", realbotTakerCloseLogInterval) {
						tui.LogEvent("[%s] ⏳ Taker close waiting: highest price is $%.3f (needs >= $%.3f)", id, highestPrice, minPrice)
					}
					continue
				}
				if bestOutcome != "" {

					confirmPrice := highestPrice
					confirmSource := "WS"
					localConfirmPrice, localReason, localConfirmOK := realbotCanUseLocalTakerCloseQuote(time.Now(), bestOutcome, tokenBids, tokenAsks, tokenFullAsks, quoteState, realbotTakerCloseLocalMaxAge)
					if localConfirmOK {
						confirmPrice = localConfirmPrice
					} else {
						confirmSource = "REST"
						restConfirmOK := true
						for _, token := range market.Tokens {
							outcome := tokenToOutcome[token.TokenID]
							if outcome != bestOutcome {
								continue
							}
							checkCtx, cancelCheck := context.WithTimeout(ctx, realbotTakerCloseRESTTimeout)
							restBid, restAsk, restErr := restClient.GetBestBidAsk(checkCtx, token.TokenID)
							cancelCheck()
							if restErr != nil {
								logKey := "rest-confirm-failed:" + outcome
								if realbotShouldLogTakerCloseState(&lastTakerCloseLog, &lastTakerCloseLogKey, logKey, realbotTakerCloseLogInterval) {
									tui.LogEvent("[%s] ⚠️ Taker close REST confirm failed for %s after local=%s: %v — skipping this tick", id, outcome, localReason, restErr)
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
							continue
						}
					}
					if confirmPrice < minPrice {
						if realbotShouldLogTakerCloseState(&lastTakerCloseLog, &lastTakerCloseLogKey, "waiting", realbotTakerCloseLogInterval) {
							tui.LogEvent("[%s] ⏳ Taker close waiting: %s confirm $%.3f is below min $%.3f (WS trigger $%.3f)", id, confirmSource, confirmPrice, minPrice, highestPrice)
						}
						continue
					}

					tokenID := ""
					for k, v := range tokenMap {
						if v == bestOutcome {
							tokenID = k
							break
						}
					}
					if tokenID == "" {
						if realbotShouldLogTakerCloseState(&lastTakerCloseLog, &lastTakerCloseLogKey, "missing-token-id:"+bestOutcome, realbotTakerCloseLogInterval) {
							tui.LogEvent("[%s] ⚠️ Taker close skipped: missing token id for %s", id, bestOutcome)
						}
						continue
					}

					budget := realbotTakerCloseBudget(engine.GetBalance(), realbotSizingCapitalForTrade(engine, liveCfg), liveCfg)
					plan, planErr := buildRealbotTakerClosePlan(budget, confirmPrice, liveCfg)
					if planErr != nil {
						if realbotShouldLogTakerCloseState(&lastTakerCloseLog, &lastTakerCloseLogKey, "plan-rejected:"+strings.TrimSpace(planErr.Error()), realbotTakerCloseLogInterval) {
							tui.LogEvent("[%s] ⚠️ Taker close plan rejected: %v", id, planErr)
						}
						takerCloseAttempted = true
						continue
					}

					if entryGate != nil && !entryGate.TryAcquire() {
						if realbotShouldLogTakerCloseState(&lastTakerCloseLog, &lastTakerCloseLogKey, "entry-gate-busy", realbotTakerCloseLogInterval) {
							tui.LogEvent("[%s] ⏳ Taker close paused: another market is executing a live entry", id)
						}
						continue
					}

					initialPosition := trader.GetLivePositionSize(tokenID)
					tui.LogEvent("[%s] ⚡ Taker close submit: %s %s shares cap $%.3f (WS $%.3f, %s $%.3f, budget $%.2f)",
						id, bestOutcome, formatShareQty(plan.RequestedQty), plan.LimitPrice, highestPrice, confirmSource, confirmPrice, budget)
					realbotMarkTakerCloseStateLogged(&lastTakerCloseLog, &lastTakerCloseLogKey, "submitted")

					takerCloseAttempted = true
					tradeCtx, cancelTrade := context.WithTimeout(ctx, 4*time.Second)
					exec := executeMarketOrderWithSignals(tradeCtx, trader, api.SideBuy, tokenID, bestOutcome, plan.LimitPrice, plan.RequestedQty, tokenFeeRates[bestOutcome], initialPosition, 2500*time.Millisecond)
					cancelTrade()
					logDirectExecutionAudit(tui, id, "Taker Close BUY", plan.RequestedQty, plan.LimitPrice, exec)

					recoveredLateFill := false
					if !exec.Success {
						if recoveredQty, recoverErr := realbotRecoverLateBuyFill(trader, tokenID, initialPosition, plan.RequestedQty); recoverErr == nil && hasConfirmedExecutedQty(api.SideBuy, recoveredQty) {
							exec.ExecutedQty = recoveredQty
							exec.Success = true
							exec.VerifyErr = nil
							recoveredLateFill = true
						} else if recoverErr != nil {
							tui.LogEvent("[%s] ⚠️ Taker close late-fill check failed: %v", id, recoverErr)
						}
					}

					if !exec.Success {
						if exec.Err != nil {
							tui.LogEvent("[%s] ❌ Taker close buy failed: %v", id, exec.Err)
						} else if exec.Result != nil && exec.Result.Message != "" {
							tui.LogEvent("[%s] ⚠️ Taker close buy not filled: %s", id, exec.Result.Message)
						} else {
							tui.LogEvent("[%s] ⚠️ Taker close buy not filled before timeout at cap $%.3f", id, plan.LimitPrice)
						}
						if entryGate != nil {
							entryGate.Release()
						}
						continue
					}

					execQty := attributedBuyFill(exec, plan.RequestedQty, 0, false)
					if !hasConfirmedExecutedQty(api.SideBuy, execQty) {
						tui.LogEvent("[%s] ⚠️ Taker close execution below confirmation threshold: %s shares", id, formatShareQty(execQty))
						if entryGate != nil {
							entryGate.Release()
						}
						continue
					}

					execPrice := venueExecutionEffectivePrice(exec)
					if execPrice <= 0 {
						execPrice = plan.LimitPrice
					}
					preLocalQty, _ := localBoughtPositionAvg(engine, id, bestOutcome)
					execCost := reportedBuyCost(exec, execPrice, execQty, plan.RequestedQty)
					if _, buyErr := engine.BuyForMarket(id, bestOutcome, execPrice, execQty); buyErr != nil {
						tui.LogEvent("[%s] ⚠️ Taker close local inventory sync failed after confirmed fill: %v", id, buyErr)
					}
					postBuyLocalQty, _ := localBoughtPositionAvg(engine, id, bestOutcome)
					tui.RecordOrder(id, bestOutcome, "BUY", execQty, execPrice, execCost, 0.0, 0.0, "FILLED")
					if recoveredLateFill {
						tui.LogEvent("[%s] 🔄 Taker close recovered delayed fill: bought %s %s after post-timeout refresh", id, formatShareQty(execQty), bestOutcome)
					}

					if execPrice+1e-9 < plan.MinPrice {
						tui.LogEvent("[%s] ℹ️ Taker close filled below the trigger price ($%.3f < $%.3f); the min-price gate only decides when to enter, and the venue matched at a better price", id, execPrice, plan.MinPrice)
					}

					takerCloseExecutedAt = time.Now()
					tui.LogEvent("[%s] ✅ Taker close confirmed: bought %s %s at $%.3f (cap $%.3f)",
						id, formatShareQty(execQty), bestOutcome, execPrice, plan.LimitPrice)
					tui.LogEvent("[%s] 🧾 Taker close position delta: %s %s | local position %.4f → %.4f | spend $%.4f",
						id, formatShareQty(execQty), bestOutcome, preLocalQty, postBuyLocalQty, execCost)

					refreshWalletTruth(5 * time.Second)
					if entryGate != nil {
						entryGate.Release()
					}
				}
			}
		}
		// --------------------------------

		// ============ TRADING LOGIC ============
		// Skip new trades if kill switch active, but keep monitoring (don't exit)
		if killSwitchActive {
			pauseMakerCtx, pauseMakerCancel := context.WithTimeout(context.Background(), 5*time.Second)
			realbotCancelAllMakerQuotes(pauseMakerCtx, id, "risk pause active", trader, engine, tui, makerQuotes)
			pauseMakerCancel()
			time.Sleep(realbotTraderLoopInterval(tui.GetSettings()))
			continue
		}

		liveCfg = tui.GetSettings()
		arbMode := normalizePaperArbMode(liveCfg.PaperArbMode)
		takerCloseMode := paper.TakerCloseModeActive(liveCfg)
		weekdayTradingAllowed = true
		if liveCfg.TradingHoursMode == "weekdays trade only" {
			weekdayTradingAllowed = core.IsUSWeekday(core.USTime(time.Now()))
		} else if liveCfg.TradingHoursMode == "us open only" {
			weekdayTradingAllowed = core.IsUSMarketOpen(time.Now())
		}
		if !weekdayTradingAllowed {
			pauseMakerCtx, pauseMakerCancel := context.WithTimeout(context.Background(), 5*time.Second)
			realbotCancelAllMakerQuotes(pauseMakerCtx, id, "trading gate closed", trader, engine, tui, makerQuotes)
			pauseMakerCancel()
			time.Sleep(realbotTraderLoopInterval(liveCfg))
			continue
		}

		if arbMode != paperArbModeCopytrade && !takerCloseMode && len(outcomes) == 2 && time.Since(lastTrade) > 5*time.Second && time.Now().After(nextLiveRecoveryAttempt) {
			recoveryCheckCtx, cancelRecoveryCheck := context.WithTimeout(context.Background(), 3*time.Second)
			pendingRecovery0, pendingRecovery1, recoverySource, recoveryCheckErr := pendingPairRecoveryBalances(recoveryCheckCtx, id, market.Tokens[0].TokenID, market.Tokens[1].TokenID, outcomes, trader, engine, splitInventory)
			cancelRecoveryCheck()
			if recoveryCheckErr == nil && (hasActionableCleanupRemainder(pendingRecovery0) || hasActionableCleanupRemainder(pendingRecovery1)) {
				tui.LogEvent("[%s] 🔄 Pending inventory detected (%s): %s=%.4f, %s=%.4f — attempting live recovery...", id, recoverySource, outcomes[0], pendingRecovery0, outcomes[1], pendingRecovery1)
				recoveryCtx, cancelRecovery := context.WithTimeout(context.Background(), 45*time.Second)
				recoveryErr := settleMarketInventory(recoveryCtx, id, market, outcomes, tokenFeeRates, trader, engine, splitInventory, tui, restClient, true, liveCfg.MinAskPrice, "LIVE RECOVERY", mergeCoordinator)
				trimmed, trimErr := reconcileLocalBoughtPositionsToWalletTruth(recoveryCtx, id, market.Tokens[0].TokenID, market.Tokens[1].TokenID, outcomes, trader, engine, splitInventory, tui)
				cancelRecovery()
				refreshWalletTruth(5 * time.Second)
				if newBal, err := trader.GetBalance(ctx); err == nil {
					currentBalance = newBal
					engine.SyncBalanceNeutral(currentBalance)
					engine.RecalculateDrawdown()
				}
				switch {
				case trimErr != nil:
					tui.LogEvent("[%s] ⚠️ Live recovery wallet-truth sync failed: %v", id, trimErr)
				case trimmed:
					tui.LogEvent("[%s] ✅ Live recovery synchronized local inventory to wallet truth.", id)
				}
				if recoveryErr != nil {
					tui.LogEvent("[%s] ⚠️ Live recovery incomplete: %v", id, recoveryErr)
					nextLiveRecoveryAttempt = time.Now().Add(10 * time.Second)
					if panicBuyCooldown.Before(time.Now().Add(15 * time.Second)) {
						panicBuyCooldown = time.Now().Add(15 * time.Second)
					}
				} else {
					nextLiveRecoveryAttempt = time.Now().Add(15 * time.Second)
					continue
				}
			} else if recoveryCheckErr == nil && (isDustCleanupRemainder(pendingRecovery0) || isDustCleanupRemainder(pendingRecovery1)) {
				if time.Since(lastDustRecoveryNotice) > 45*time.Second {
					tui.LogEvent("[%s] ℹ️ Residual dust below %.2f-share cleanup minimum (%s): %s=%.4f, %s=%.4f — skipping live recovery retries for now", id, minOnChainActionShares, recoverySource, outcomes[0], pendingRecovery0, outcomes[1], pendingRecovery1)
					lastDustRecoveryNotice = time.Now()
				}
				nextLiveRecoveryAttempt = time.Now().Add(60 * time.Second)
			} else {
				nextLiveRecoveryAttempt = time.Now().Add(5 * time.Second)
			}
		}

		// Skip normal trading completely if TakerCloseMarket is enabled
		if takerCloseMode {
			cancelMakerCtx, cancelMaker := context.WithTimeout(context.Background(), 5*time.Second)
			realbotCancelAllMakerQuotes(cancelMakerCtx, id, "taker close market enabled", trader, engine, tui, makerQuotes)
			cancelMaker()
			time.Sleep(realbotTraderLoopInterval(liveCfg))
			continue
		}

		if arbMode == paperArbModeMaker {
			makerCtx, makerCancel := context.WithTimeout(ctx, 5*time.Second)
			maintainRealbotMakerQuotes(makerCtx, id, endTime, outcomes, getTokenID, tokenBids, tokenAsks, tokenFeeRates, trader, engine, riskMgr, tui, liveCfg, cfg, makerQuotes, &lastMakerSync)
			makerCancel()
			time.Sleep(realbotTraderLoopInterval(liveCfg))
			continue
		}
		cancelMakerCtx, cancelMaker := context.WithTimeout(context.Background(), 5*time.Second)
		realbotCancelAllMakerQuotes(cancelMakerCtx, id, "maker mode disabled", trader, engine, tui, makerQuotes)
		cancelMaker()
		if arbMode == paperArbModeCopytrade {
			realbotHandleCopytradeMarket(ctx, id, market, outcomes, tokenBids, tokenAsks, tokenFullBids, tokenFullAsks, quoteState, tokenFeeRates, trader, engine, tui, restClient, liveCfg, copytradePoller, copytradeState, entryGate, refreshWalletTruth)
			time.Sleep(realbotTraderLoopInterval(liveCfg))
			continue
		}
		if arbMode == paperArbModeBinanceGap {
			realbotHandleBinanceGapMarket(ctx, id, outcomes, tokenBids, tokenAsks, tokenFullBids, tokenFullAsks, quoteState, polySignalTracker, tokenFeeRates, trader, engine, tui, liveCfg, cfg, currentBalance, binanceFeed, getTokenID, entryGate, &lastTrade, &lastBinanceLog)
			time.Sleep(realbotTraderLoopInterval(liveCfg))
			continue
		}

		// ═══════════════════════════════════════════════════════════════════════════
		// SPLIT STRATEGY: Sell to panic buyers when bid_sum > $1.03
		// This is SEPARATE from the panic buy strategy (buy when ask_sum < $0.98)
		// Split shares are ONLY for selling, bought shares are ONLY for merging
		// ═══════════════════════════════════════════════════════════════════════════
		skipPanicBuy := false
		kalshiHoldMode := liveCfg.Exchange == "kalshi"

		if (liveCfg.SplitStrategyEnabled || kalshiHoldMode) && len(tokenBids) >= 2 && len(outcomes) == 2 {
			bid1 := tokenBids[outcomes[0]]
			bid2 := tokenBids[outcomes[1]]

			// Initial split: create inventory if not done yet
			// Move to BACKGROUND to prevent blocking the main trading loop
			splitMu.Lock()
			isSplit := globalSplitStatus[market.ConditionID]

			shouldSplit := !isSplit && time.Now().After(nextSplitAttempt)
			if shouldSplit {
				if kalshiHoldMode {
					shouldSplit = false
				} else {
					// Optimistically mark as split to prevent concurrent duplicate attempts
					globalSplitStatus[market.ConditionID] = true
				}
			}
			splitMu.Unlock()

			if shouldSplit && replenishCtrl.MarkInProgress() {
				baseTradeSize := cfg.CalculateTradeSize(realbotSizingCapitalForTrade(engine, liveCfg))

				// Scale initial buffer based on balance: 2x trade size, but at least $2 and at most 25% of balance
				initialBuffer := baseTradeSize * 2.0
				if initialBuffer < 2.0 {
					initialBuffer = 2.0
				}

				maxInitial := currentBalance * cfg.SplitInitialCapPct
				splitAmount := initialBuffer
				if splitAmount > maxInitial {
					splitAmount = maxInitial
				}

				// Lower threshold to $1.0 to support testing with small balances (like $5)
				if splitAmount >= 1.0 {
					tui.LogEvent("[%s] 🔀 SPLIT: Creating inventory ($%.2f) in background...", id, splitAmount)

					go func(mID, condID, out0, out1 string, amt float64) {
						defer replenishCtrl.MarkComplete()
						// Increase timeout to 120s to be more resilient to Polygon congestion
						splitCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
						defer cancel()

						splitTxMu.Lock()
						txHash, err := trader.SplitOnChain(splitCtx, condID, amt, len(outcomes))
						splitTxMu.Unlock()

						splitMu.Lock() // Re-acquire lock to update shared state
						if err != nil {
							tui.LogEvent("[%s] ⚠️ SPLIT: Background initial split failed: %v (will retry in 15s)", mID, err)
							// Set cooldown on failure to prevent RPC spam and nonce issues
							nextSplitAttempt = time.Now().Add(15 * time.Second)

							// Revert optimistic split status so it can be retried
							globalSplitStatus[condID] = false
						} else {
							// Update engine simulation immediately
							splitInventory.RecordSplit(mID, out0, out1, amt)
							engine.DeductBalance(amt)
							engine.RecalculateDrawdown()

							if txHash != "" && len(txHash) >= 10 {
								tui.LogEvent("[%s] ✅ SPLIT: Created %.0f shares | Tx: %s...", mID, amt, txHash[:10])
							} else {
								tui.LogEvent("[%s] ✅ SPLIT: Created %.0f shares", mID, amt)
							}

							// Only mark as initialized on SUCCESS (globally)
							globalSplitStatus[condID] = true
							globalInitialSplits[condID] = amt
						}
						splitMu.Unlock()
					}(id, market.ConditionID, outcomes[0], outcomes[1], splitAmount)
				} else {
					// Not enough balance to split even $1
					replenishCtrl.MarkComplete()
					splitMu.Lock()
					if !globalSplitStatus[market.ConditionID] {
						tui.LogEvent("[%s] ⚠️ SPLIT: Balance too low for split ($%.2f < $1.00)", id, splitAmount)
						globalSplitStatus[market.ConditionID] = true // Mark true to stop spamming, even if skipped
					}
					splitMu.Unlock()
				}
			}

			// Check for panic sell opportunity: bid_sum > $1.00 + minMargin
			if bid1 >= liveCfg.MinAskPrice && bid2 >= liveCfg.MinAskPrice && bid1 <= liveCfg.MaxAskPrice && bid2 <= liveCfg.MaxAskPrice {
				bidSum := bid1 + bid2
				sellMargin := (bidSum - 1.0) * 100 // Profit margin from selling

				// BACKGROUND REPLENISHMENT
				baseTradeSize := cfg.CalculateTradeSize(realbotSizingCapitalForTrade(engine, liveCfg))
				targetBuffer := baseTradeSize * cfg.MaxAggressionMultiplier
				currentShares := splitInventory.GetMinSplitShares(id, outcomes[0], outcomes[1])
				replenishAmount := baseTradeSize * 2.0
				splitMu.Lock()
				initialSplitAmount := globalInitialSplits[market.ConditionID]
				splitMu.Unlock()

				decision := replenishCtrl.CheckReplenish(paper.ReplenishParams{
					CurrentShares:      currentShares,
					TargetBuffer:       targetBuffer,
					InitialShares:      initialSplitAmount, // Replenish back to initial amount
					SellMargin:         sellMargin,
					MinMarginThreshold: cfg.SplitMinMarginSell - 1.0,
					CurrentBalance:     currentBalance,
					ReplenishAmount:    replenishAmount,
					MaxBalancePercent:  cfg.SplitReplenishCapPct,
				})

				if decision.ShouldReplenish && replenishCtrl.MarkInProgress() {
					if kalshiHoldMode {
						replenishCtrl.MarkComplete()
					} else {
						tui.LogEvent("[%s] 🔄 SPLIT: Low inventory (%.0f/%.0f), replenishing +%.0f shares...", id, currentShares, initialSplitAmount, decision.Amount)
						go func(mID, condID, out0, out1 string, amt float64, targetShares float64) {
							defer replenishCtrl.MarkComplete()
							// Use derived context for proper shutdown propagation
							bgCtx, bgCancel := context.WithTimeout(ctx, 60*time.Second)
							defer bgCancel()

							splitTxMu.Lock()
							_, bgErr := trader.SplitOnChain(bgCtx, condID, amt, len(outcomes))
							splitTxMu.Unlock()

							if bgErr == nil {
								// Update engine simulation immediately
								splitInventory.RecordSplit(mID, out0, out1, amt)
								engine.DeductBalance(amt)
								engine.RecalculateDrawdown()
								tui.LogEvent("[%s] ✅ SPLIT: Replenished to %.0f shares (+%.0f)", mID, targetShares, amt)
							} else {
								tui.LogEvent("[%s] ⚠️ SPLIT: Background replenish failed: %v", mID, bgErr)
							}
						}(id, market.ConditionID, outcomes[0], outcomes[1], decision.Amount, initialSplitAmount)
					}
				}

				if sellMargin >= cfg.SplitMinMarginSell-1e-4 && time.Since(lastSplitSell) > 2*time.Second {
					// DETERMINISTIC AGGRESSION
					// Use SplitInitialCapPct to determine the number of shares to sell
					requestedShares := currentBalance * cfg.SplitInitialCapPct

					// GRACEFUL SELL: Sell what we have
					var availableShares float64
					if kalshiHoldMode {
						// Kalshi nets positions; bypass min constraint to allow selling to open
						availableShares = requestedShares
					} else {
						availableShares = splitInventory.GetMinSplitShares(id, outcomes[0], outcomes[1])
					}
					sharesToSell := requestedShares
					if sharesToSell > availableShares {
						if availableShares >= minOnChainActionShares {
							tui.LogEvent("[%s] ⚠️ SPLIT: Capped sell at available inventory (%s/%s)", id, formatShareQty(availableShares), formatShareQty(requestedShares))
							sharesToSell = availableShares
						} else {
							sharesToSell = 0
						}
					}

					if sharesToSell >= minOnChainActionShares {
						// Hard safety cap
						if sharesToSell > 250 {
							sharesToSell = 250
						}

						// ═══════════════════════════════════════════════════════════════
						// MATCHED BID LIQUIDITY: Walk bid levels (price descending) and
						// only count pairs where bid1+bid2 >= minSum (the profitability
						// threshold). This mirrors utilbot's estimateMatchedLiquidity and
						// ensures we never order more than what can actually be filled at
						// a profitable price. Used for BOTH sizing and display.
						// ═══════════════════════════════════════════════════════════════
						bids1 := tokenFullBids[outcomes[0]]
						bids2 := tokenFullBids[outcomes[1]]
						bookDepth1, bookDepth2 := len(bids1), len(bids2)
						executionMarginFloor := clampExecutionMarginFloor(liveCfg.SplitMinMarginSell, liveCfg.BuyExecutionMarginFloorPercent)
						minSum := minExecutablePairSum(executionMarginFloor, liveCfg.MinAskPrice)

						sortedBids1 := make([]paper.MarketLevel, len(bids1))
						copy(sortedBids1, bids1)
						// Inject BBO if missing due to orderbook lag to prevent liq: 0/0
						hasBid1 := false
						for _, b := range sortedBids1 {
							if b.Price >= bid1-1e-6 {
								hasBid1 = true
								break
							}
						}
						if !hasBid1 {
							sortedBids1 = append(sortedBids1, paper.MarketLevel{Price: bid1, Size: sharesToSell})
						}
						sort.Slice(sortedBids1, func(a, b int) bool { return sortedBids1[a].Price > sortedBids1[b].Price })

						sortedBids2 := make([]paper.MarketLevel, len(bids2))
						copy(sortedBids2, bids2)
						hasBid2 := false
						for _, b := range sortedBids2 {
							if b.Price >= bid2-1e-6 {
								hasBid2 = true
								break
							}
						}
						if !hasBid2 {
							sortedBids2 = append(sortedBids2, paper.MarketLevel{Price: bid2, Size: sharesToSell})
						}
						sort.Slice(sortedBids2, func(a, b int) bool { return sortedBids2[a].Price > sortedBids2[b].Price })
						var rawLiq1, rawLiq2, matchedBidLiq float64
						var maxValidI, maxValidJ int

						for bi, bj := 0, 0; bi < len(sortedBids1) && bj < len(sortedBids2); {
							if sortedBids1[bi].Price+sortedBids2[bj].Price < minSum-1e-6 {
								break // below shared execution floor — stop
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

						// Cap to matched bid liquidity (follows utilbot's approach exactly)
						if sharesToSell > matchedBidLiq {
							sharesToSell = matchedBidLiq
						}

						sharesToSell = normalizeMarketSellShares(sharesToSell)
						if kalshiHoldMode {
							sharesToSell = math.Floor(sharesToSell)
						}

						if sharesToSell >= minOnChainActionShares && sharesToSell <= availableShares+1e-6 {
							// Enhanced log with liquidity and depth info (same format as paper bot)
							tui.LogEvent("[%s] 📈 SPLIT SELL candidate %s@$%.2f + %s@$%.2f = $%.3f (%.1f%% observed, %.1f%% execution floor) | %s shares [liq: %.0f/%.0f, levels used: %d/%d (total depth: %d/%d)]",
								id, outcomes[0], bid1, outcomes[1], bid2, bidSum, sellMargin, executionMarginFloor, formatShareQty(sharesToSell),
								rawLiq1, rawLiq2, maxValidI, maxValidJ, bookDepth1, bookDepth2)
							executionQuoteMaxAge := realbotExecutionQuoteGuardAge(core.ResolveExecutionLocalQuoteMaxAge(cfg))
							freshLocalSellQuote, _, localSellQuoteReason := realbotCanUseLocalSellQuote(time.Now(), outcomes, tokenBids, tokenAsks, tokenFullBids, quoteState, executionQuoteMaxAge)
							if !freshLocalSellQuote {
								tui.LogEvent("[%s] ⚠️ Split-sell paused: awaiting fresh local quote (%s)", id, localSellQuoteReason)
								continue
							}
							bid1 = tokenBids[outcomes[0]]
							bid2 = tokenBids[outcomes[1]]
							bidSum = bid1 + bid2
							sellMargin = (bidSum - 1.0) * 100
							if sellMargin < cfg.SplitMinMarginSell-1e-4 {
								tui.LogEvent("[%s] ⚠️ Local sell quote moved away: %s=%.3f, %s=%.3f (%.1f%% < %.1f%% trigger)", id, outcomes[0], bid1, outcomes[1], bid2, sellMargin, cfg.SplitMinMarginSell)
								continue
							}
							freshMatchedLiquidity := realbotMatchedBidLiquidity(tokenFullBids[outcomes[0]], tokenFullBids[outcomes[1]], minSum)
							if sharesToSell > freshMatchedLiquidity {
								tui.LogEvent("[%s] ⚡ Local sell quote capped shares %s→%s using local matched liquidity %s", id, formatShareQty(sharesToSell), formatShareQty(freshMatchedLiquidity), formatShareQty(freshMatchedLiquidity))
								sharesToSell = freshMatchedLiquidity
							}
							sharesToSell = normalizeMarketSellShares(sharesToSell)
							if sharesToSell < minOnChainActionShares {
								tui.LogEvent("[%s] ⚠️ Local sell quote left less than %.2f share actionable liquidity: %.4f", id, minOnChainActionShares, sharesToSell)
								continue
							}

							// Sell both sides in parallel
							token0 := getTokenID(outcomes[0])
							token1 := getTokenID(outcomes[1])

							// Validate token IDs before trading
							if token0 == "" || token1 == "" {
								tui.LogEvent("[%s] ⚠️ SPLIT: Token ID not found for %s/%s", id, outcomes[0], outcomes[1])
								continue
							}

							// Sync CLOB allowance with on-chain state right before trading.
							// This is the root cause of "insufficient balance/allowance" errors:
							// the CLOB loses sync with on-chain state between startup and trade time.
							// Background ticker keeps allowance synced.

							// Capture an instant websocket-backed baseline so the split-sell legs can
							// be submitted immediately without waiting on slow on-chain snapshots.
							initialSnapshot0 := trader.GetLivePositionSize(token0)
							initialSnapshot1 := trader.GetLivePositionSize(token1)
							initialBal0 := initialSnapshot0
							initialBal1 := initialSnapshot1
							haveInitialSnapshot := true

							rate1 := tokenFeeRates[outcomes[0]]
							if rate1 == 0 {
								rate1 = 1000
							}
							rate2 := tokenFeeRates[outcomes[1]]
							if rate2 == 0 {
								rate2 = 1000
							}

							batchExecs := executeMarketOrderBatchWithSignals(ctx, trader, []directMarketOrderSignalRequest{
								{
									Side:           api.SideSell,
									TokenID:        token0,
									Outcome:        outcomes[0],
									Price:          liveCfg.MinAskPrice,
									Size:           sharesToSell,
									FeeRateBps:     rate1,
									InitialBalance: initialBal0,
								},
								{
									Side:           api.SideSell,
									TokenID:        token1,
									Outcome:        outcomes[1],
									Price:          liveCfg.MinAskPrice,
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
							if haveInitialSnapshot && (side1Success || side2Success) {
								verifyCtx, cancelVerify := context.WithTimeout(context.Background(), realbotCleanupVerifyTTL)
								verifiedSold0, verifiedSold1, verifyBal0, verifyBal1, verifySource, verifyErr := waitForPairSellBalanceReduction(verifyCtx, trader, token0, token1, initialSnapshot0, initialSnapshot1, haveInitialSnapshot, side1Success, side2Success)
								cancelVerify()
								if side1Success {
									sold1 = math.Min(verifiedSold0, sharesToSell)
								}
								if side2Success {
									sold2 = math.Min(verifiedSold1, sharesToSell)
								}
								if verifyErr != nil && ((!side1Success || hasConfirmedExecutedQty(api.SideSell, sold1)) && (!side2Success || hasConfirmedExecutedQty(api.SideSell, sold2))) {
									tui.LogEvent("[%s] ⚠️ Split-sell balance verification warning: %v", id, verifyErr)
								} else if verifyErr != nil {
									tui.LogEvent("[%s] ⚠️ Split-sell balance verification still pending (%s): %s=%.4f, %s=%.4f", id, verifySource, outcomes[0], verifyBal0, outcomes[1], verifyBal1)
								}
								if side1Success && !hasConfirmedExecutedQty(api.SideSell, sold1) {
									tui.LogEvent("[%s] ⚠️ Split-sell for %s lacked wallet-truth reduction (%s snapshot from %s); leaving inventory unchanged", id, outcomes[0], formatShareQty(verifyBal0), verifySource)
									side1Success = false
								}
								if side2Success && !hasConfirmedExecutedQty(api.SideSell, sold2) {
									tui.LogEvent("[%s] ⚠️ Split-sell for %s lacked wallet-truth reduction (%s snapshot from %s); leaving inventory unchanged", id, outcomes[1], formatShareQty(verifyBal1), verifySource)
									side2Success = false
								}
							} else if side1Success || side2Success {
								tui.LogEvent("[%s] ⚠️ Split-sell balance verification unavailable (initial snapshot missing); using direct execution signals only", id)
							}

							// ═══════════════════════════════════════════════════════════════
							// LEGGED SPLIT SELL VERIFICATION: If one side sold and the other
							// didn't, do not retry here. Leave the remainder for cleanup.
							// ═══════════════════════════════════════════════════════════════
							if side1Success != side2Success {
								failedOutcome := outcomes[1]
								if !side1Success {
									failedOutcome = outcomes[0]
								}
								tui.LogEvent("[%s] ⚠️ SPLIT LEGGED: %s still not sold (leaving for cleanup path)", id, failedOutcome)
							}

							if side1Success && side2Success {
								var totalProfit float64
								var profit1, profit2 float64
								if kalshiHoldMode {
									// In kalshi, just deduct cost basis roughly for PNL logging
									profit1 = (price1 - 0.5) * sold1
									profit2 = (price2 - 0.5) * sold2
									totalProfit = profit1 + profit2
									engine.AddRealizedPnL(totalProfit)
									tui.LogEvent("[%s] ✅ PANIC SOLD! %s: %.2f, %s: %.2f | Profit: ~+$%.2f", id, outcomes[0], sold1, outcomes[1], sold2, totalProfit)
								} else {
									// Both sides sold - record in split inventory using actual sold amounts
									profit1 = splitInventory.RecordSell(id, outcomes[0], sold1, price1)
									profit2 = splitInventory.RecordSell(id, outcomes[1], sold2, price2)
									totalProfit = profit1 + profit2
									engine.AddRealizedPnL(totalProfit)
									tui.LogEvent("[%s] ✅ SPLIT SOLD! %s: %.2f, %s: %.2f | Profit: +$%.2f", id, outcomes[0], sold1, outcomes[1], sold2, totalProfit)
								}

								tui.RecordOrder(id, outcomes[0], "SELL", sold1, price1, sold1*price1, sellMargin, profit1, "FILLED")
								tui.RecordOrder(id, outcomes[1], "SELL", sold2, price2, sold2*price2, sellMargin, profit2, "FILLED")

								// Refresh balance after successful sell (cash increased)
								_, _ = trader.ForceRefreshBalance(ctx)

								tui.LogEvent("[%s] ✅ Execution complete after successful panic/split sell.", id)
							} else {
								// Partial success - record to keep inventory accurate
								if side1Success {
									if !kalshiHoldMode {
										splitInventory.RecordSell(id, outcomes[0], sold1, price1)
									}
									tui.LogEvent("[%s] ⚠️ SELL: Only %s sold %.2f (one-shot)", id, outcomes[0], sold1)
								}
								if side2Success {
									if !kalshiHoldMode {
										splitInventory.RecordSell(id, outcomes[1], sold2, price2)
									}
									tui.LogEvent("[%s] ⚠️ SELL: Only %s sold %.2f (one-shot)", id, outcomes[1], sold2)
								}
							}
							refreshWalletTruth(5 * time.Second)

							lastSplitSell = time.Now()
						}
					}
				}
			}
		}
		// ═══════════════════════════════════════════════════════════════════════════
		// PANIC BUY STRATEGY: Buy when ask_sum < $0.98, then merge for instant profit
		// These shares are SEPARATE from split shares - they go straight to merge
		// ═══════════════════════════════════════════════════════════════════════════
		if skipPanicBuy {
			continue
		}
		if time.Now().Before(panicBuyCooldown) {
			continue
		}
		if len(tokenAsks) >= 2 && len(outcomes) == 2 {
			ask1 := tokenAsks[outcomes[0]]
			ask2 := tokenAsks[outcomes[1]]
			bid1 := tokenBids[outcomes[0]]
			bid2 := tokenBids[outcomes[1]]

			// Prevent trading on transient WS glitches where the book is one-sided or crossed
			if bid1 <= 0 || bid2 <= 0 || ask1 <= bid1 || ask2 <= bid2 {
				continue
			}

			// Read live price-range filter from settings panel (adjustable at runtime)
			realbotCfg := tui.GetSettings()
			rMinAsk := realbotCfg.MinAskPrice
			rMaxAsk := realbotCfg.MaxAskPrice

			if ask1 >= rMinAsk && ask1 <= rMaxAsk && ask2 >= rMinAsk && ask2 <= rMaxAsk {
				sum := ask1 + ask2
				observedMargin := pairMarginPercent(sum)
				executionMarginFloor := clampExecutionMarginFloor(realbotCfg.MinMarginPercent, realbotCfg.BuyExecutionMarginFloorPercent)
				executionPriceCap := normalizedRealbotExecutionPriceCap(realbotCfg)
				maxExecutionSum := maxExecutablePairSum(executionMarginFloor, executionPriceCap)

				if observedMargin >= realbotCfg.MinMarginPercent-1e-4 {
					// Evaluate risk
					riskAction, riskReason := riskMgr.Evaluate()
					if riskAction == paper.RiskActionKillSwitch {
						tui.LogEvent("[%s] 🛑 RISK: Kill switch - %s (pausing, not exiting)", id, riskReason)
						continue
					}

					// Normal real-mode sizing uses the ratcheting high-water sizing base so
					// above-water drawdowns or withdrawals do not shrink trade budgets mid-session.
					tradeSize := cfg.CalculateTradeSize(realbotSizingCapitalForTrade(engine, realbotCfg))

					// Get max fee rate for conservative margin calculation
					maxFeeRateBps := 0
					if rate1, ok := tokenFeeRates[outcomes[0]]; ok && rate1 > maxFeeRateBps {
						maxFeeRateBps = rate1
					}
					if rate2, ok := tokenFeeRates[outcomes[1]]; ok && rate2 > maxFeeRateBps {
						maxFeeRateBps = rate2
					}

					// Scale shares based on margin (User requested NO fee buffer deduction)
					shares := normalizeMarketBuyShares(tradeSize / sum)
					requestedShares := shares
					// Fee estimation and balance check logging removed per user request.
					executionQuoteMaxAge := realbotExecutionQuoteGuardAge(core.ResolveExecutionLocalQuoteMaxAge(cfg))
					freshLocalBuyQuote, _, localBuyQuoteReason := realbotCanUseLocalBuyQuote(time.Now(), outcomes, tokenBids, tokenAsks, tokenFullAsks, quoteState, executionQuoteMaxAge)
					if !freshLocalBuyQuote {
						tui.LogEvent("[%s] ⚠️ Skipping buy: awaiting fresh local quote (%s)", id, localBuyQuoteReason)
						panicBuyCooldown = time.Now().Add(500 * time.Millisecond)
						continue
					}

					ask1 = tokenAsks[outcomes[0]]
					ask2 = tokenAsks[outcomes[1]]
					if ask1 < rMinAsk || ask1 > rMaxAsk || ask2 < rMinAsk || ask2 > rMaxAsk {
						tui.LogEvent("[%s] ⚠️ Skipping buy: local asks %.3f / %.3f outside configured range %.3f-%.3f", id, ask1, ask2, rMinAsk, rMaxAsk)
						panicBuyCooldown = time.Now().Add(500 * time.Millisecond)
						continue
					}
					sum = ask1 + ask2
					observedMargin = pairMarginPercent(sum)
					if observedMargin < realbotCfg.MinMarginPercent-1e-4 {
						tui.LogEvent("[%s] ⚠️ Skipping buy: local pair margin %.2f%% below configured %.2f%%", id, observedMargin, realbotCfg.MinMarginPercent)
						panicBuyCooldown = time.Now().Add(500 * time.Millisecond)
						continue
					}

					// Recalculate shares based on the fresh, confirmed sum to prevent over-execution from transient WS glitches
					shares = normalizeMarketBuyShares(tradeSize / sum)
					requestedShares = shares

					if block, reason := realbotPanicBuyCompletionGuard(engine, id, outcomes[0], outcomes[1], ask1, ask2, realbotCfg.MinMarginPercent); block {
						tui.LogEvent("[%s] ⚠️ Skipping buy: %s", id, reason)
						panicBuyCooldown = time.Now().Add(500 * time.Millisecond)
						continue
					}

					// AGGREGATED LIQUIDITY: Calculate total matched liquidity across all price levels
					// that remain acceptable under the configured execution margin floor. This lets
					// panic buys consume deeper liquidity to reduce misses/legging, while still
					// stopping before the pair gets worse than the allowed post-slip margin.
					maxSum := maxExecutionSum

					// Copy and sort asks by price ascending for both outcomes
					asks1 := make([]paper.MarketLevel, len(tokenFullAsks[outcomes[0]]))
					copy(asks1, tokenFullAsks[outcomes[0]])
					sort.Slice(asks1, func(i, j int) bool { return asks1[i].Price < asks1[j].Price })

					asks2 := make([]paper.MarketLevel, len(tokenFullAsks[outcomes[1]]))
					copy(asks2, tokenFullAsks[outcomes[1]])
					sort.Slice(asks2, func(i, j int) bool { return asks2[i].Price < asks2[j].Price })

					// Calculate aggregated matched liquidity across valid price levels
					var totalMatchedLiquidity float64
					var rawLiq1, rawLiq2 float64
					var maxValidI, maxValidJ int

					i, j := 0, 0
					for i < len(asks1) && j < len(asks2) {
						p1 := asks1[i].Price
						p2 := asks2[j].Price

						// Check if this combination stays within the allowed execution floor.
						if p1+p2 > maxSum+1e-6 {
							break // Can't go deeper, would exceed the post-slip execution floor.
						}

						// Get liquidity at current levels
						levelLiq1 := asks1[i].Size
						levelLiq2 := asks2[j].Size

						// Matched liquidity = min of both sides (arbitrage requires equal shares)
						matchedAtLevel := levelLiq1
						if levelLiq2 < matchedAtLevel {
							matchedAtLevel = levelLiq2
						}

						if i+1 > maxValidI {
							maxValidI = i + 1
							rawLiq1 += asks1[i].Size
						}
						if j+1 > maxValidJ {
							maxValidJ = j + 1
							rawLiq2 += asks2[j].Size
						}
						totalMatchedLiquidity += matchedAtLevel

						// Move pointer on the side with less remaining liquidity
						remaining1 := levelLiq1 - matchedAtLevel
						remaining2 := levelLiq2 - matchedAtLevel

						if remaining1 <= 0 {
							i++
						} else {
							asks1[i].Size = remaining1
						}
						if remaining2 <= 0 {
							j++
						} else {
							asks2[j].Size = remaining2
						}
					}

					// Use aggregated liquidity for display
					liq1 := rawLiq1
					liq2 := rawLiq2
					minLiquidity := totalMatchedLiquidity
					bookDepth1 := len(tokenFullAsks[outcomes[0]])
					bookDepth2 := len(tokenFullAsks[outcomes[1]])

					// Require local WS depth inside the configured execution floor to cover the
					// requested trade size before we attempt entry. This avoids late REST requotes
					// and prevents entering on incomplete BBO-only depth.
					if requestedShares > minLiquidity+1e-6 {
						tui.LogEvent("[%s] ⚠️ WS executable ask depth inside %.1f%% window covers %s/%s shares — skipping", id, executionMarginFloor, formatShareQty(minLiquidity), formatShareQty(requestedShares))
						panicBuyCooldown = time.Now().Add(500 * time.Millisecond)
						continue
					}

					// Risk checks should use the worst price sum the bot is willing to execute through.
					cost := strategy.CalculateTradeMetricsFlat(shares, maxExecutionSum, maxFeeRateBps).Cost

					// Use the last known cached balance here; a fresh RPC can add avoidable
					// latency right when we need to submit the panic-buy legs.

					// Check risk limits only (Balance check disabled per user request to match utilbot behavior)
					if !riskMgr.CanPlaceOrder(cost) {
						tui.LogEvent("[%s] ⚠️ Risk limit exceeded for cost $%.2f", id, cost)
						continue
					}

					// Skipping conservative balance checks (costWithBuffer > currentCash) to allow max execution.
					// If balance is insufficient, the API call will fail naturally.

					// Check why we might skip trading
					if shares < 1.0 {
						tui.LogEvent("[%s] ⚠️ Actionable matched liquidity below 1.0 share minimum: %.2f", id, shares)
						continue
					}
					if time.Since(lastTrade) <= 2*time.Second {
						// Cooldown - don't spam logs, just skip silently
						continue
					}

					if true { // Always execute if we got here
						limitPrice1, limitPrice2, capErr := core.BuyExecutionLimitPrices(ask1, ask2, rMinAsk, executionPriceCap, executionMarginFloor)
						if capErr != nil {
							tui.LogEvent("[%s] ⚠️ Skipping trade: %v", id, capErr)
							continue
						}
						budgetCappedShares := realbotClampBuySharesToBudget(shares, tradeSize, limitPrice1, limitPrice2)
						if budgetCappedShares < shares {
							tui.LogEvent("[%s] 📉 Downscaling from %s to %s shares to stay within $%.2f trade budget at live caps", id, formatShareQty(shares), formatShareQty(budgetCappedShares), tradeSize)
							shares = budgetCappedShares
						}
						if shares < 1 {
							tui.LogEvent("[%s] ⚠️ Actionable size fell below 1 share after cap-based budget clamp", id)
							continue
						}
						tui.LogEvent("[%s] 🎯 ARB candidate %s@$%.3f→%.3f + %s@$%.3f→%.3f = $%.3f (%.1f%% observed, %.1f%% execution floor) [liq: %.0f/%.0f, levels used: %d/%d (total depth: %d/%d)]",
							id, outcomes[0], ask1, limitPrice1, outcomes[1], ask2, limitPrice2, sum, observedMargin, executionMarginFloor, liq1, liq2, maxValidI, maxValidJ, bookDepth1, bookDepth2)

						// Map tokens
						token0, token1 := "", ""
						for tid, out := range tokenToOutcome {
							if out == outcomes[0] {
								token0 = tid
							} else if out == outcomes[1] {
								token1 = tid
							}
						}

						// Ensure the actual market-like buy payload still fits the latest cash snapshot.
						safeShares := realbotClampBuySharesToBudget(shares, currentBalance, limitPrice1, limitPrice2)
						if safeShares < shares {
							tui.LogEvent("[%s] 📉 Downscaling from %s to %s shares to fit $%.2f balance limit", id, formatShareQty(shares), formatShareQty(safeShares), currentBalance)
							shares = safeShares
						}
						if shares < 1 {
							tui.LogEvent("[%s] ⚠️ Skipping buy: capped share size no longer fits available balance", id)
							continue
						}

						if entryGate != nil && !entryGate.TryAcquire() {
							panicBuyCooldown = time.Now().Add(500 * time.Millisecond)
							tui.LogEvent("[%s] ⏳ Skipping buy: another market is executing a live entry", id)
							continue
						}

						// Sync CLOB allowance with on-chain state right before trading.
						// Root cause of "insufficient balance/allowance" errors in realbot:
						// allowance synced once at startup can go stale by the time an arb opportunity arrives.
						// Background ticker keeps allowance synced.
						var res1, res2 *trading.TradeResult
						var err1, err2 error
						// Capture an instant websocket-backed baseline so the panic-buy legs can
						// be submitted immediately without waiting on slow on-chain snapshots.
						initialSnapshot0 := trader.GetLivePositionSize(token0)
						initialSnapshot1 := trader.GetLivePositionSize(token1)
						initialSnapshotSource := "live WS cache"
						haveInitialSnapshot := true
						initialBal0 := initialSnapshot0
						initialBal1 := initialSnapshot1

						rate1 := tokenFeeRates[outcomes[0]]
						if rate1 == 0 {
							rate1 = 1000
						}
						rate2 := tokenFeeRates[outcomes[1]]
						if rate2 == 0 {
							rate2 = 1000
						}

						batchExecs := executeMarketOrderBatchWithSignals(ctx, trader, []directMarketOrderSignalRequest{
							{
								Side:           api.SideBuy,
								TokenID:        token0,
								Outcome:        outcomes[0],
								Price:          limitPrice1,
								Size:           shares,
								FeeRateBps:     rate1,
								InitialBalance: initialBal0,
							},
							{
								Side:           api.SideBuy,
								TokenID:        token1,
								Outcome:        outcomes[1],
								Price:          limitPrice2,
								Size:           shares,
								FeeRateBps:     rate2,
								InitialBalance: initialBal1,
							},
						}, 2*time.Second)
						exec1, exec2 := batchExecs[0], batchExecs[1]

						res1, err1 = exec1.Result, exec1.Err
						res2, err2 = exec2.Result, exec2.Err
						rawFilled1, rawFilled2 := exec1.ExecutedQty, exec2.ExecutedQty
						filled1, filled2 := rawFilled1, rawFilled2
						side1Success, side2Success := exec1.Success, exec2.Success
						logDirectExecutionAudit(tui, id, "Side 1 BUY", shares, limitPrice1, exec1)
						logDirectExecutionAudit(tui, id, "Side 2 BUY", shares, limitPrice2, exec2)
						if bal0, bal1, verifySource, verifyErr := loadPairBalancesWSFirst(ctx, trader, token0, token1); verifyErr == nil {
							tui.LogEvent("[%s] 🔍 Verify Positions (%s): %s=%.4f, %s=%.4f (Target: %.0f)", id, verifySource, outcomes[0], bal0, outcomes[1], bal1, shares)
						} else {
							tui.LogEvent("[%s] ⚠️ External position snapshot unavailable after direct buy: %v", id, verifyErr)
						}

						attributionTrusted := false
						if haveInitialSnapshot {
							attrCtx, cancelAttr := context.WithTimeout(ctx, 8*time.Second)
							acquired0, acquired1, absBal0, absBal1, attrSource, attrErr := reconcileBoughtPairBalances(attrCtx, trader, token0, token1, initialSnapshot0, initialSnapshot1, true)
							cancelAttr()
							if attrErr == nil || shouldAttemptCleanupSell(acquired0) || shouldAttemptCleanupSell(acquired1) {
								attributionTrusted = true
								filled1 = attributedBuyFill(exec1, shares, acquired0, true)
								filled2 = attributedBuyFill(exec2, shares, acquired1, true)
								side1Success = hasConfirmedExecutedQty(api.SideBuy, filled1)
								side2Success = hasConfirmedExecutedQty(api.SideBuy, filled2)
								if shouldAttemptCleanupSell(initialSnapshot0) || shouldAttemptCleanupSell(initialSnapshot1) || math.Abs(rawFilled1-filled1) > 0.01 || math.Abs(rawFilled2-filled2) > 0.01 {
									tui.LogEvent("[%s] 🧾 PANIC BUY attribution (%s): %s abs=%.4f Δ=%.4f, %s abs=%.4f Δ=%.4f", id, attrSource, outcomes[0], absBal0, filled1, outcomes[1], absBal1, filled2)
								}
							} else {
								tui.LogEvent("[%s] ⚠️ PANIC BUY attribution unavailable; using capped order confirmation only: %v", id, attrErr)
							}
						}
						if !attributionTrusted {
							filled1 = attributedBuyFill(exec1, shares, 0, false)
							filled2 = attributedBuyFill(exec2, shares, 0, false)
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

						// Calculate costs using the observed trigger prices for reporting.
						// Polymarket does not expose exact per-leg execution price through this path.
						cost1 := reportedBuyCost(exec1, ask1, filled1, shares)
						cost2 := reportedBuyCost(exec2, ask2, filled2, shares)

						// Log results based on VERIFIED state
						if side1Success {
							tui.LogEvent("[%s] ✅ Side 1 MARKET: %s (Observed $%.3f, Filled: %.2f/%.2f)", id, outcomes[0], ask1, filled1, shares)
							tui.RecordOrder(id, outcomes[0], "BUY", filled1, ask1, cost1, observedMargin, 0.0, "FILLED")
						} else {
							// Log the actual failure reason (err or res.Message)
							if err1 != nil {
								tui.LogEvent("[%s] ❌ Side 1 MARKET Fail: %v", id, err1)
							} else if res1 != nil && res1.Message != "" {
								tui.LogEvent("[%s] ❌ Side 1 MARKET Fail: %s", id, res1.Message)
							} else if res1 == nil {
								tui.LogEvent("[%s] ❌ Side 1 MARKET Fail: nil response", id)
							} else {
								tui.LogEvent("[%s] ❌ Side 1 MARKET Fail: unknown error (res=%v)", id, res1)
							}
							tui.RecordOrder(id, outcomes[0], "BUY", shares, ask1, cost1, observedMargin, 0.0, "FAILED")
						}

						if side2Success {
							tui.LogEvent("[%s] ✅ Side 2 MARKET: %s (Observed $%.3f, Filled: %.2f/%.2f)", id, outcomes[1], ask2, filled2, shares)
							tui.RecordOrder(id, outcomes[1], "BUY", filled2, ask2, cost2, observedMargin, 0.0, "FILLED")
						} else {
							// Log the actual failure reason (err or res.Message)
							if err2 != nil {
								tui.LogEvent("[%s] ❌ Side 2 MARKET Fail: %v", id, err2)
							} else if res2 != nil && res2.Message != "" {
								tui.LogEvent("[%s] ❌ Side 2 MARKET Fail: %s", id, res2.Message)
							} else if res2 == nil {
								tui.LogEvent("[%s] ❌ Side 2 MARKET Fail: nil response", id)
							} else {
								tui.LogEvent("[%s] ❌ Side 2 MARKET Fail: unknown error (res=%v)", id, res2)
							}
							tui.RecordOrder(id, outcomes[1], "BUY", shares, ask2, cost2, observedMargin, 0.0, "FAILED")
						}

						// ═══════════════════════════════════════════════════════════════
						// LEGGED SHARE VERIFICATION: If one side filled and the other didn't,
						// wait 2 seconds for late settlement and re-verify only.
						// Do not retry buys here to avoid accidental spam-buys.
						// ═══════════════════════════════════════════════════════════════
						if side1Success != side2Success {
							if haveInitialSnapshot {
								tui.LogEvent("[%s] 🧾 Pre-trade share snapshot (%s): %s=%.4f, %s=%.4f", id, initialSnapshotSource, outcomes[0], initialSnapshot0, outcomes[1], initialSnapshot1)
							}
							tui.LogEvent("[%s] ⚠️ ARB LEGGED: %s=%v %s=%v — waiting 2s then re-verifying...",
								id, outcomes[0], side1Success, outcomes[1], side2Success)
							time.Sleep(2 * time.Second)

							var leggedAcquired0, leggedAcquired1, leggedBal0, leggedBal1 float64
							var leggedSource string
							reverifyCtx, cancelReverify := context.WithTimeout(ctx, 12*time.Second)
							leggedAcquired0, leggedAcquired1, leggedBal0, leggedBal1, leggedSource, _ = reconcileBoughtPairBalances(reverifyCtx, trader, token0, token1, initialSnapshot0, initialSnapshot1, haveInitialSnapshot)
							cancelReverify()
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

							// Final status after verification
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

						// NOW record to engine - only record positions that actually succeeded
						// This ensures engine state matches reality for accurate drawdown calculation
						if side1Success && side2Success {
							// Both sides filled (either initially or via recovery) - record both
							_, _ = engine.BuyForMarket(id, outcomes[0], ask1, filled1)
							_, _ = engine.BuyForMarket(id, outcomes[1], ask2, filled2)

							settleCtx, settleCancel := context.WithTimeout(context.Background(), 12*time.Second)
							settleErr := settleMarketInventory(settleCtx, id, market, outcomes, tokenFeeRates, trader, engine, splitInventory, tui, restClient, true, rMinAsk, "POST BUY", mergeCoordinator)
							settleCancel()
							if settleErr != nil {
								tui.LogEvent("[%s] ⚠️ Post-buy settlement still pending: %v", id, settleErr)
								panicBuyCooldown = time.Now().Add(10 * time.Second)
							} else if mergeCoordinator.pendingQty(id) >= minOnChainActionShares {
								tui.LogEvent("[%s] ✅ Buys verified. Merge continues in background while cleanup handles only the excess inventory.", id)
							} else {
								tui.LogEvent("[%s] ✅ Execution complete after verified buys. Applying 5s cooldown...", id)
							}

							// Refresh balance for next trade
							if newBal, err := trader.ForceRefreshBalance(ctx); err == nil {
								currentBalance = newBal
								engine.SyncBalanceNeutral(currentBalance)
								engine.RecalculateDrawdown()
							}
							refreshWalletTruth(5 * time.Second)
							time.Sleep(5 * time.Second)
						} else if side1Success || side2Success {
							// Only one side filled — record the unbalanced position and
							// temporarily block further panic buys to prevent exposure accumulation.
							if side1Success {
								_, _ = engine.BuyForMarket(id, outcomes[0], ask1, filled1)
								tui.LogEvent("[%s] ⚠️ Engine: Recording unbalanced position (only %s)", id, outcomes[0])
							}
							if side2Success {
								_, _ = engine.BuyForMarket(id, outcomes[1], ask2, filled2)
								tui.LogEvent("[%s] ⚠️ Engine: Recording unbalanced position (only %s)", id, outcomes[1])
							}

							cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 60*time.Second)

							tui.LogEvent("[%s] ⚠️ Legged trade detected! Re-checking live/on-chain balances before cleanup...", id)

							acquired0, acquired1, bal0, bal1, balanceSource, balanceErr := reconcileBoughtPairBalances(cleanupCtx, trader, token0, token1, initialSnapshot0, initialSnapshot1, haveInitialSnapshot)
							if balanceErr != nil {
								tui.LogEvent("[%s] ⚠️ Cleanup balance reconciliation warning: %v", id, balanceErr)
							}

							if acquired0 >= minOnChainActionShares && acquired1 >= minOnChainActionShares {
								tui.LogEvent("[%s] 🟢 Cleanup balances ready (%s): %s abs=%.4f Δ=%.4f, %s abs=%.4f Δ=%.4f. Attempting Merge!", id, balanceSource, outcomes[0], bal0, acquired0, outcomes[1], bal1, acquired1)
								mergeQty, _, _, _, err := mergeBalancedPositionWSFirst(cleanupCtx, trader, market.ConditionID, token0, token1, math.Min(math.Min(acquired0, acquired1), shares), len(market.Tokens))
								if err != nil {
									tui.LogEvent("[%s] ⚠️ Delayed Merge failed: %v", id, err)
									// Fallback to sell below using the live WS position cache.
								} else {
									tui.LogEvent("[%s] ✅ Delayed Merge successful! Applying 30s cooldown.", id)
									acquired0, acquired1 = subtractMergedPairBalances(acquired0, acquired1, mergeQty)
								}
							}

							// If not settled via merge, or if dust remains, clean it up via Market Sell
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
									sell0Exec = executeMarketOrderWithSignals(cleanupCtx, trader, api.SideSell, token0, outcomes[0], cleanupQuote.SubmitPrice, cleanupQuote.ExecutableQty, cfg.FeeRateBps, acquired0, 2*time.Second)
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
									sell1Exec = executeMarketOrderWithSignals(cleanupCtx, trader, api.SideSell, token1, outcomes[1], cleanupQuote.SubmitPrice, cleanupQuote.ExecutableQty, cfg.FeeRateBps, acquired1, 2*time.Second)
								}
							}

							verifyCleanupCtx, cancelVerifyCleanup := context.WithTimeout(context.Background(), realbotCleanupVerifyTTL)
							remaining0, remaining1, resolvedBal0, resolvedBal1, resolvedSource, resolvedErr := waitForAcquiredCleanupResolution(verifyCleanupCtx, trader, token0, token1, initialSnapshot0, initialSnapshot1, haveInitialSnapshot)
							cancelVerifyCleanup()
							actualSold0 := math.Max(0, acquired0-remaining0)
							actualSold1 := math.Max(0, acquired1-remaining1)

							if hasActionableCleanupRemainder(actualSold0) {
								if _, sellErr := engine.SellForMarket(id, outcomes[0], cleanupSellPrice, actualSold0); sellErr != nil {
									tui.LogEvent("[%s] ⚠️ Engine cleanup sync failed for %s: %v", id, outcomes[0], sellErr)
								}
							}
							if hasActionableCleanupRemainder(actualSold1) {
								if _, sellErr := engine.SellForMarket(id, outcomes[1], cleanupSellPrice, actualSold1); sellErr != nil {
									tui.LogEvent("[%s] ⚠️ Engine cleanup sync failed for %s: %v", id, outcomes[1], sellErr)
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
								panicBuyCooldown = time.Now().Add(120 * time.Second)
							} else {
								tui.LogEvent("[%s] ✅ Auto-cleanup verified flat (%s). Applying 30s cooldown before unblocking.", id, resolvedSource)
								panicBuyCooldown = time.Now().Add(30 * time.Second)
							}
							cancelCleanup() // Release cleanup context resources
						} // If both failed, nothing to record

						// Force refresh balance after trade to ensure accurate tracking
						if newBal, err := trader.ForceRefreshBalance(ctx); err == nil {
							currentBalance = newBal
							engine.SyncBalanceNeutral(currentBalance)
							engine.RecalculateDrawdown()
						}
						refreshWalletTruth(5 * time.Second)
						if entryGate != nil {
							entryGate.Release()
						}

						lastTrade = time.Now()
					}
				}
			}
		}

		time.Sleep(realbotTraderLoopInterval(liveCfg))
	}
}

type directMarketExecution struct {
	Result               *trading.TradeResult
	Err                  error
	ExecutedQty          float64
	AcknowledgedQty      float64
	AcknowledgedNotional float64
	Success              bool
	WSConfirmed          bool
	OrderConfirmed       bool
	VerifyErr            error
}

type directMarketOrderSignalRequest struct {
	Side           api.Side
	TokenID        string
	Outcome        string
	Price          float64
	Size           float64
	FeeRateBps     int
	InitialBalance float64
}

func isMinSizeRejectionMessage(message string) bool {
	return strings.Contains(strings.ToLower(message), "min size")
}

func cleanupRejectionMessage(qty float64, outcome, venueMessage string) string {
	message := strings.TrimSpace(venueMessage)
	if message == "" {
		return fmt.Sprintf("Cleanup attempt rejected for %s %s shares after placing the order; keeping remainder for now", formatShareQty(qty), outcome)
	}
	return fmt.Sprintf("Cleanup attempt rejected for %s %s shares after placing the order; keeping remainder for now: %s", formatShareQty(qty), outcome, message)
}

func shouldAttemptCleanupSell(qty float64) bool {
	return qty > 0.000001
}

func isDustCleanupRemainder(qty float64) bool {
	return shouldAttemptCleanupSell(qty) && !hasActionableCleanupRemainder(qty)
}

func hasActionableCleanupRemainder(qty float64) bool {
	return qty >= (minOnChainActionShares - 1e-9)
}

func normalizeMarketSellShares(qty float64) float64 {
	if qty <= 0 {
		return 0
	}
	return math.Floor((qty*100)+1e-9) / 100
}

func normalizeMarketBuyShares(qty float64) float64 {
	if qty <= 0 {
		return 0
	}
	return math.Floor((qty*10000)+1e-9) / 10000
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

func hasConfirmedExecutedQty(side api.Side, qty float64) bool {
	if side == api.SideSell {
		return qty > 0.000001
	}
	return qty > 0.01
}

func formatShareQty(qty float64) string {
	if math.Abs(qty-math.Round(qty)) < 1e-9 {
		return fmt.Sprintf("%.0f", qty)
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.5f", qty), "0"), ".")
}

func venueExecutionEffectivePrice(exec directMarketExecution) float64 {
	if exec.AcknowledgedQty <= 0 || exec.AcknowledgedNotional <= 0 {
		return 0
	}
	return exec.AcknowledgedNotional / exec.AcknowledgedQty
}

func clampRequestedExecutionQty(qty, requestedQty float64) float64 {
	if qty < 0 {
		return 0
	}
	if requestedQty > 0 && qty > requestedQty {
		return requestedQty
	}
	return qty
}

func attributedBuyFill(exec directMarketExecution, requestedQty, acquiredQty float64, trustAcquired bool) float64 {
	if trustAcquired {
		return clampRequestedExecutionQty(acquiredQty, requestedQty)
	}
	qty := exec.ExecutedQty
	if qty <= 0 && exec.AcknowledgedQty > 0 {
		qty = exec.AcknowledgedQty
	}
	return clampRequestedExecutionQty(qty, requestedQty)
}

func attributedSellFill(exec directMarketExecution, requestedQty float64) float64 {
	qty := exec.ExecutedQty
	if qty <= 0 && exec.AcknowledgedQty > 0 {
		qty = exec.AcknowledgedQty
	}
	return clampRequestedExecutionQty(qty, requestedQty)
}

func ackNotionalMatchesAttributedBuy(exec directMarketExecution, attributedQty float64) bool {
	if exec.AcknowledgedQty <= 0 || exec.AcknowledgedNotional <= 0 || attributedQty <= 0 {
		return false
	}
	diff := math.Abs(exec.AcknowledgedQty - attributedQty)
	return diff <= math.Max(0.02, attributedQty*0.02)
}

func reportedBuyCost(exec directMarketExecution, observedPrice, attributedQty, requestedQty float64) float64 {
	qty := clampRequestedExecutionQty(attributedQty, requestedQty)
	if ackNotionalMatchesAttributedBuy(exec, qty) {
		return exec.AcknowledgedNotional
	}
	return qty * observedPrice
}

func directExecutionTxSummary(exec directMarketExecution) string {
	if exec.Result == nil || len(exec.Result.TransactionsHashes) == 0 {
		return ""
	}
	hashes := exec.Result.TransactionsHashes
	shortened := make([]string, 0, len(hashes))
	for i, tx := range hashes {
		if i >= 3 {
			break
		}
		if len(tx) > 12 {
			shortened = append(shortened, tx[:12]+"...")
			continue
		}
		shortened = append(shortened, tx)
	}
	summary := strings.Join(shortened, ", ")
	if extra := len(hashes) - len(shortened); extra > 0 {
		summary = fmt.Sprintf("%s (+%d more)", summary, extra)
	}
	if len(hashes) == 1 {
		return summary
	}
	return fmt.Sprintf("%d txs [%s]", len(hashes), summary)
}

func directExecutionHasSizingDrift(exec directMarketExecution, requestedQty float64) bool {
	if exec.AcknowledgedQty <= 0 || requestedQty <= 0 {
		return false
	}
	drift := math.Abs(exec.AcknowledgedQty - requestedQty)
	return drift > math.Max(0.02, requestedQty*0.02)
}

func logDirectExecutionAudit(tui *paper.TUI, id, label string, requestedQty, limitPrice float64, exec directMarketExecution) {
	if tui == nil || exec.Result == nil {
		return
	}
	if exec.AcknowledgedQty <= 0 && exec.AcknowledgedNotional <= 0 && len(exec.Result.TransactionsHashes) == 0 {
		return
	}
	effectivePrice := venueExecutionEffectivePrice(exec)
	txSummary := directExecutionTxSummary(exec)
	tui.LogEvent("[%s] 🧾 %s venue ack: req=%s lim=$%.3f ackQty=%s ackNotional=$%.4f eff=$%.4f maker=%s taker=%s tx=%s",
		id,
		label,
		formatShareQty(requestedQty),
		limitPrice,
		formatShareQty(exec.AcknowledgedQty),
		exec.AcknowledgedNotional,
		effectivePrice,
		exec.Result.MakingAmount,
		exec.Result.TakingAmount,
		txSummary,
	)
	if directExecutionHasSizingDrift(exec, requestedQty) {
		driftPct := ((exec.AcknowledgedQty / requestedQty) - 1.0) * 100.0
		tui.LogEvent("[%s] 🚨 %s sizing drift: requested %s shares but venue acknowledged %s (%+.1f%%) at cap $%.3f (effective $%.4f) tx=%s",
			id,
			label,
			formatShareQty(requestedQty),
			formatShareQty(exec.AcknowledgedQty),
			driftPct,
			limitPrice,
			effectivePrice,
			txSummary,
		)
	}
}

func buildDirectMarketOrderRequest(req directMarketOrderSignalRequest) *api.OrderRequest {
	return &api.OrderRequest{
		TokenID:     req.TokenID,
		Price:       req.Price,
		Size:        req.Size,
		Side:        req.Side,
		OrderType:   api.OrderTypeLimit,
		TimeInForce: api.TIFFillAndKill,
		FeeRateBps:  req.FeeRateBps,
	}
}

func hydrateDirectMarketTradeResult(req directMarketOrderSignalRequest, result *trading.TradeResult) *trading.TradeResult {
	if result == nil {
		result = &trading.TradeResult{}
	}
	result.Price = req.Price
	result.Size = req.Size
	result.Side = string(req.Side)
	result.TokenID = req.TokenID
	result.Outcome = req.Outcome
	result.FeeRateBps = req.FeeRateBps
	if result.Timestamp.IsZero() {
		result.Timestamp = time.Now()
	}
	return result
}

func shouldSkipImmediateExecutionConfirmation(result *trading.TradeResult, err error) bool {
	if err != nil || result == nil {
		return false
	}
	if result.Success {
		return false
	}
	if result.AcknowledgedQty > 0 || result.AcknowledgedNotional > 0 || len(result.TransactionsHashes) > 0 || len(result.TradeIDs) > 0 {
		return false
	}

	status := strings.ToUpper(strings.TrimSpace(result.Status))
	switch status {
	case "KILLED", "CANCELLED", "EXPIRED", "REJECTED":
		return true
	}

	msg := strings.ToLower(strings.TrimSpace(result.Message))
	if msg == "" {
		return false
	}
	if strings.Contains(msg, "no orders found to match with fak order") {
		return true
	}
	if strings.Contains(msg, "order was killed") || strings.Contains(msg, "order was cancelled") || strings.Contains(msg, "order was expired") || strings.Contains(msg, "order was rejected") {
		return true
	}
	return false
}

func finalizeDirectMarketExecutionWithSignals(ctx context.Context, trader *trading.RealTrader, req directMarketOrderSignalRequest, confirmTimeout time.Duration, result *trading.TradeResult, err error) directMarketExecution {
	result = hydrateDirectMarketTradeResult(req, result)
	orderID := result.OrderID
	acknowledgedQty := result.AcknowledgedQty
	acknowledgedNotional := result.AcknowledgedNotional

	if shouldSkipImmediateExecutionConfirmation(result, err) {
		return directMarketExecution{
			Result:               result,
			Err:                  err,
			ExecutedQty:          0,
			AcknowledgedQty:      acknowledgedQty,
			AcknowledgedNotional: acknowledgedNotional,
			Success:              false,
		}
	}

	executedQty, wsConfirmed, orderConfirmed, verifyErr := confirmMarketOrderExecution(ctx, trader, req.Side, orderID, req.TokenID, req.InitialBalance, confirmTimeout)
	if acknowledgedQty > executedQty {
		executedQty = acknowledgedQty
	}
	executedQty = clampRequestedExecutionQty(executedQty, req.Size)
	success := hasConfirmedExecutedQty(req.Side, executedQty) || orderConfirmed

	if success {
		result.Success = true
		if orderConfirmed {
			result.Status = "FILLED"
		} else if wsConfirmed {
			result.Status = "CONFIRMED"
		}
	} else if err == nil && result.Message == "" {
		if verifyErr != nil {
			result.Message = fmt.Sprintf("No confirmed fill before timeout (%v)", verifyErr)
		} else {
			result.Message = "No confirmed fill before timeout at configured cap"
		}
	}

	return directMarketExecution{
		Result:               result,
		Err:                  err,
		ExecutedQty:          executedQty,
		AcknowledgedQty:      acknowledgedQty,
		AcknowledgedNotional: acknowledgedNotional,
		Success:              success,
		WSConfirmed:          wsConfirmed,
		OrderConfirmed:       orderConfirmed,
		VerifyErr:            verifyErr,
	}
}

func executeMarketOrderBatchWithSignals(ctx context.Context, trader *trading.RealTrader, reqs []directMarketOrderSignalRequest, confirmTimeout time.Duration) []directMarketExecution {
	if len(reqs) == 0 {
		return nil
	}

	primeRealbotOrderPath(ctx, trader)

	batchReqs := make([]*api.OrderRequest, len(reqs))
	for i, req := range reqs {
		batchReqs[i] = buildDirectMarketOrderRequest(req)
	}

	results, err := trader.ExecuteBatch(ctx, batchReqs)
	execs := make([]directMarketExecution, len(reqs))
	var wg sync.WaitGroup
	wg.Add(len(reqs))
	for i := range reqs {
		i := i
		go func() {
			defer wg.Done()
			var result *trading.TradeResult
			if i < len(results) {
				result = results[i]
			} else if err == nil {
				result = &trading.TradeResult{Message: "missing batch response from exchange"}
			}
			execs[i] = finalizeDirectMarketExecutionWithSignals(ctx, trader, reqs[i], confirmTimeout, result, err)
		}()
	}
	wg.Wait()
	return execs
}

func executeMarketOrderWithSignals(ctx context.Context, trader *trading.RealTrader, side api.Side, tokenID, outcome string, price, size float64, feeRateBps int, initialBalance float64, confirmTimeout time.Duration) directMarketExecution {
	req := directMarketOrderSignalRequest{
		Side:           side,
		TokenID:        tokenID,
		Outcome:        outcome,
		Price:          price,
		Size:           size,
		FeeRateBps:     feeRateBps,
		InitialBalance: initialBalance,
	}
	result, err := submitDirectMarketOrder(ctx, trader, side, tokenID, outcome, price, size, feeRateBps)
	return finalizeDirectMarketExecutionWithSignals(ctx, trader, req, confirmTimeout, result, err)
}

func submitDirectMarketOrder(ctx context.Context, trader *trading.RealTrader, side api.Side, tokenID, outcome string, price, size float64, feeRateBps int) (*trading.TradeResult, error) {
	primeRealbotOrderPath(ctx, trader)

	if side == api.SideSell {
		return trader.Sell(ctx, tokenID, outcome, price, size, api.OrderTypeLimit, api.TIFFillAndKill, feeRateBps)
	}
	return trader.Buy(ctx, tokenID, outcome, price, size, api.OrderTypeLimit, api.TIFFillAndKill, feeRateBps)
}

func confirmMarketOrderExecution(ctx context.Context, trader *trading.RealTrader, side api.Side, orderID, tokenID string, initialBalance float64, timeout time.Duration) (executedQty float64, wsConfirmed bool, orderConfirmed bool, verifyErr error) {
	if orderID != "" {
		defer trader.ResetConfirmedFill(orderID)
	}

	type orderFillResult struct {
		filled bool
		err    error
	}
	orderFilledCh := make(chan orderFillResult, 1)
	if orderID != "" {
		waitCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			filled, err := trader.WaitForFill(waitCtx, orderID, timeout)
			orderFilledCh <- orderFillResult{filled: filled, err: err}
		}()
	}

	deadline := time.Now().Add(timeout)
	for {
		select {
		case orderFill := <-orderFilledCh:
			if orderFill.filled {
				orderConfirmed = true
			}
			if orderFill.err != nil && verifyErr == nil && !strings.Contains(orderFill.err.Error(), "context canceled") {
				verifyErr = orderFill.err
			}
		default:
		}

		if orderID != "" {
			if wsQty := trader.GetConfirmedFillSize(orderID); wsQty > executedQty {
				executedQty = wsQty
				wsConfirmed = hasConfirmedExecutedQty(side, wsQty)
			}
		}

		liveBalance := trader.GetLivePositionSize(tokenID)
		if delta := executionDeltaFromLiveBalance(liveBalance, initialBalance, side); delta > executedQty {
			executedQty = delta
		}

		if hasConfirmedExecutedQty(side, executedQty) || time.Now().After(deadline) {
			break
		}
		time.Sleep(realbotFillPollInterval)
	}

	if positions, err := trader.ForceRefreshPositions(ctx); err == nil {
		if delta := executionDeltaFromPositions(positions, tokenID, initialBalance, side); delta > executedQty {
			executedQty = delta
		}
		verifyErr = nil
	}
	if orderID != "" {
		if wsQty := trader.GetConfirmedFillSize(orderID); wsQty > executedQty {
			executedQty = wsQty
			wsConfirmed = hasConfirmedExecutedQty(side, wsQty)
		}
	}
	if hasConfirmedExecutedQty(side, executedQty) {
		verifyErr = nil
	}
	return executedQty, wsConfirmed, orderConfirmed, verifyErr
}

func executionDeltaFromPositions(positions []trading.PositionInfo, tokenID string, initialBalance float64, side api.Side) float64 {
	current := 0.0
	for _, pos := range positions {
		if pos.TokenID == tokenID {
			current = pos.Size
			break
		}
	}
	if side == api.SideSell {
		delta := initialBalance - current
		if delta < 0 {
			return 0
		}
		return delta
	}
	delta := current - initialBalance
	if delta < 0 {
		return 0
	}
	return delta
}

func executionDeltaFromLiveBalance(current, initialBalance float64, side api.Side) float64 {
	if side == api.SideSell {
		delta := initialBalance - current
		if delta < 0 {
			return 0
		}
		return delta
	}
	delta := current - initialBalance
	if delta < 0 {
		return 0
	}
	return delta
}

func pairBalancesFromPositions(positions []trading.PositionInfo, token0, token1 string) (float64, float64) {
	var bal0, bal1 float64
	for _, pos := range positions {
		switch pos.TokenID {
		case token0:
			bal0 = pos.Size
		case token1:
			bal1 = pos.Size
		}
	}
	return bal0, bal1
}

func pairMarginPercent(sum float64) float64 {
	return (1.0 - sum) * 100.0
}

func computeRealbotMakerInventorySkew(positionShares, peerShares, targetShares float64) float64 {
	return strategy.ComputeMakerInventorySkew(positionShares, peerShares, targetShares)
}

func computeRealbotMakerSkewedQuote(side api.Side, bid, ask, skew, quoteGap float64, params strategy.MakerParams) (float64, bool) {
	return strategy.ComputeMakerSkewedQuote(side == api.SideBuy, bid, ask, skew, quoteGap, params)
}

func computeRealbotMakerPairBuyPrices(bid0, ask0, bid1, ask1, maxPairCost, inventoryDelta float64, params strategy.MakerParams) (float64, float64, bool) {
	return strategy.ComputeMakerPairBuyPrices(bid0, ask0, bid1, ask1, maxPairCost, inventoryDelta, params)
}

func computeRealbotMakerPairQuoteQty(baseTradeValue, pairedShares, maxInventoryValue, cash, price0, price1 float64, params strategy.MakerParams) float64 {
	return strategy.ComputeMakerPairQuoteQty(baseTradeValue, pairedShares, maxInventoryValue, cash, price0, price1, params, normalizeMarketSellShares)
}

func computeRealbotMakerBuyQty(baseShares, positionShares, skew, maxInventory, cash, price float64, params strategy.MakerParams) float64 {
	return strategy.ComputeMakerBuyQty(baseShares, positionShares, skew, maxInventory, cash, price, params, normalizeMarketSellShares)
}

func computeRealbotMakerSellQty(baseShares, positionShares, skew, price float64, params strategy.MakerParams) float64 {
	return strategy.ComputeMakerSellQty(baseShares, positionShares, skew, price, params, normalizeMarketSellShares)
}

func computeRealbotMakerProtectedSellQuote(bid, ask, avgCost, minEdge, skew, quoteGap float64, feeRateBps int, timeRemaining time.Duration, params strategy.MakerParams) (float64, bool) {
	return strategy.ComputeMakerProtectedSellQuote(bid, ask, avgCost, minEdge, skew, quoteGap, feeRateBps, timeRemaining, params)
}

func shouldRealbotMakerBlockBuy(positionShares float64, sellOK bool, peerShares, peerAvgCost, price, minEdge float64) bool {
	return strategy.ShouldMakerBlockBuy(positionShares, sellOK, peerShares, peerAvgCost, price, minEdge)
}

func realbotMakerReservedBuyNotional(makerQuotes map[string]*realbotMakerQuote) float64 {
	total := 0.0
	for _, quote := range makerQuotes {
		if quote == nil || quote.Side != api.SideBuy || quote.RemainingQty <= 0 || quote.Price <= 0 {
			continue
		}
		total += quote.RemainingQty * quote.Price
	}
	return total
}

func realbotUpdateMakerPendingOrders(marketID string, makerQuotes map[string]*realbotMakerQuote, tui *paper.TUI) {
	pending := make(map[string][]paper.PendingOrder)
	keys := make([]string, 0, len(makerQuotes))
	for key := range makerQuotes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		quote := makerQuotes[key]
		if quote == nil || quote.RemainingQty*quote.Price < 1.0 || quote.Price <= 0 {
			continue
		}
		pending[quote.Outcome] = append(pending[quote.Outcome], paper.PendingOrder{
			MarketID: marketID,
			Outcome:  quote.Outcome,
			Price:    quote.Price,
			Qty:      quote.RemainingQty,
			Side:     string(quote.Side),
		})
	}
	tui.SetPendingOrders(marketID, pending)
}

func realbotSyncMakerQuoteFills(marketID string, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI, makerQuotes map[string]*realbotMakerQuote, openByID map[string]api.OpenOrder) {
	keys := make([]string, 0, len(makerQuotes))
	for key := range makerQuotes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		quote := makerQuotes[key]
		if quote == nil || quote.OrderID == "" {
			delete(makerQuotes, key)
			continue
		}
		confirmed := trader.GetConfirmedFillSize(quote.OrderID)
		delta := confirmed - quote.AccountedFill
		if delta > 1e-6 {
			if quote.Side == api.SideBuy {
				if _, err := engine.MakerBuyForMarket(marketID, quote.Outcome, quote.Price, delta); err != nil {
					tui.LogEvent("[%s] ⚠️ Maker buy fill sync failed for %s %.4f @ $%.3f: %v", marketID, quote.Outcome, delta, quote.Price, err)
				} else {
					tui.LogEvent("[%s] ✅ Maker BUY fill: %s %.2f @ $%.3f", marketID, quote.Outcome, delta, quote.Price)
					tui.RecordOrderWithMode(marketID, quote.Outcome, "BUY", delta, quote.Price, delta*quote.Price, 0.0, 0.0, "maker", "FILLED")
				}
			} else {
				if _, err := engine.MakerSellForMarket(marketID, quote.Outcome, quote.Price, delta); err != nil {
					tui.LogEvent("[%s] ⚠️ Maker sell fill sync failed for %s %.4f @ $%.3f: %v", marketID, quote.Outcome, delta, quote.Price, err)
				} else {
					tui.LogEvent("[%s] ✅ Maker SELL fill: %s %.2f @ $%.3f", marketID, quote.Outcome, delta, quote.Price)
					tui.RecordOrderWithMode(marketID, quote.Outcome, "SELL", delta, quote.Price, delta*quote.Price, 0.0, 0.0, "maker", "FILLED")
				}
			}
			quote.AccountedFill = confirmed
		}
		if open, ok := openByID[quote.OrderID]; ok {
			quote.RemainingQty = normalizeMarketSellShares(math.Max(0, open.RemainingSize))
			if open.Price > 0 {
				quote.Price = open.Price
			}
			if quote.RemainingQty*quote.Price < 1.0 {
				delete(makerQuotes, key)
			}
			continue
		}
		quote.RemainingQty = normalizeMarketSellShares(math.Max(0, quote.RequestedQty-quote.AccountedFill))
		if quote.RemainingQty*quote.Price < 1.0 {
			delete(makerQuotes, key)
		}
	}
}

func realbotCancelMakerQuote(ctx context.Context, trader *trading.RealTrader, quote *realbotMakerQuote) {
	if trader == nil || quote == nil || quote.OrderID == "" {
		return
	}
	_ = trader.CancelOrderByID(ctx, quote.OrderID)
}

func realbotCancelAllMakerQuotes(ctx context.Context, marketID, reason string, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI, makerQuotes map[string]*realbotMakerQuote) bool {
	if len(makerQuotes) == 0 {
		realbotUpdateMakerPendingOrders(marketID, makerQuotes, tui)
		return false
	}
	realbotSyncMakerQuoteFills(marketID, trader, engine, tui, makerQuotes, nil)
	for key, quote := range makerQuotes {
		realbotCancelMakerQuote(ctx, trader, quote)
		delete(makerQuotes, key)
	}
	if reason != "" {
		tui.LogEvent("[%s] 🧹 Maker quotes cancelled: %s", marketID, reason)
	}
	realbotUpdateMakerPendingOrders(marketID, makerQuotes, tui)
	return true
}

func realbotUpsertMakerQuote(ctx context.Context, marketID string, trader *trading.RealTrader, riskMgr *paper.RiskManager, tui *paper.TUI, makerQuotes map[string]*realbotMakerQuote, openByID map[string]api.OpenOrder, side api.Side, outcome, tokenID string, price, qty float64, feeRateBps int) bool {
	key := realbotMakerQuoteKey(side, outcome)
	existing := makerQuotes[key]
	qty = normalizeMarketSellShares(qty)

	orderValue := qty * price

	// We want to use the config, so we will pass it in or rely on the fact that
	// the upstream calculation correctly bounded it, so we just enforce $1 minimum for safety.
	if orderValue < 1.0 || price <= 0 || tokenID == "" {
		if existing != nil {
			realbotCancelMakerQuote(ctx, trader, existing)
			delete(makerQuotes, key)
			return true
		}
		return false
	}
	if existing != nil {
		if openByID != nil {
			if _, ok := openByID[existing.OrderID]; !ok {
				delete(makerQuotes, key)
				existing = nil
			}
		}
	}
	if existing != nil {
		remaining := existing.RemainingQty
		if remaining <= 0 {
			remaining = normalizeMarketSellShares(math.Max(0, existing.RequestedQty-existing.AccountedFill))
		}
		if math.Abs(existing.Price-price) < 1e-9 && math.Abs(remaining-qty) < 0.01 {
			return false
		}
		realbotCancelMakerQuote(ctx, trader, existing)
		delete(makerQuotes, key)
	}
	if side == api.SideBuy && riskMgr != nil && !riskMgr.CanPlaceOrder(price*qty) {
		tui.LogEvent("[%s] ⚠️ Skipping maker buy %s %s @ $%.3f: risk limit exceeded", marketID, outcome, formatShareQty(qty), price)
		return false
	}
	var (
		res *trading.TradeResult
		err error
	)
	if side == api.SideBuy {
		res, err = trader.Buy(ctx, tokenID, outcome, price, qty, api.OrderTypeLimit, api.TIFGoodTilCancelled, feeRateBps)
	} else {
		res, err = trader.Sell(ctx, tokenID, outcome, price, qty, api.OrderTypeLimit, api.TIFGoodTilCancelled, feeRateBps)
	}
	if err != nil {
		tui.LogEvent("[%s] ⚠️ Maker %s quote failed for %s %s @ $%.3f: %v", marketID, strings.ToLower(string(side)), outcome, formatShareQty(qty), price, err)
		return false
	}
	if res == nil || !res.Success || res.OrderID == "" {
		if res != nil && res.Message != "" {
			tui.LogEvent("[%s] ⚠️ Maker %s quote rejected for %s %s @ $%.3f: %s", marketID, strings.ToLower(string(side)), outcome, formatShareQty(qty), price, res.Message)
		} else {
			tui.LogEvent("[%s] ⚠️ Maker %s quote rejected for %s %s @ $%.3f", marketID, strings.ToLower(string(side)), outcome, formatShareQty(qty), price)
		}
		return false
	}
	makerQuotes[key] = &realbotMakerQuote{
		OrderID:       res.OrderID,
		TokenID:       tokenID,
		Outcome:       outcome,
		Side:          side,
		Price:         price,
		RequestedQty:  qty,
		RemainingQty:  qty,
		AccountedFill: trader.GetConfirmedFillSize(res.OrderID),
		FeeRateBps:    feeRateBps,
	}
	return true
}

func maintainRealbotMakerQuotes(ctx context.Context, marketID string, endTime time.Time, outcomes []string, getTokenID func(string) string, tokenBids, tokenAsks map[string]float64, tokenFeeRates map[string]int, trader *trading.RealTrader, engine *paper.Engine, riskMgr *paper.RiskManager, tui *paper.TUI, liveCfg paper.TUISettings, cfg *core.Config, makerQuotes map[string]*realbotMakerQuote, lastMakerSync *time.Time) {
	if len(outcomes) != 2 {
		realbotCancelAllMakerQuotes(ctx, marketID, "maker mode requires exactly 2 outcomes", trader, engine, tui, makerQuotes)
		return
	}
	openByID := make(map[string]api.OpenOrder)
	if len(makerQuotes) > 0 {
		openOrders, err := trader.GetOpenOrders(ctx)
		if err != nil {
			tui.LogEvent("[%s] ⚠️ Maker open-order refresh failed: %v", marketID, err)
		} else {
			for _, order := range openOrders {
				openByID[order.OrderID] = order
			}
		}
	}
	realbotSyncMakerQuoteFills(marketID, trader, engine, tui, makerQuotes, openByID)
	if lastMakerSync != nil && !lastMakerSync.IsZero() && time.Since(*lastMakerSync) < realbotMakerRequoteInterval {
		realbotUpdateMakerPendingOrders(marketID, makerQuotes, tui)
		return
	}

	timeToEnd := time.Until(endTime)
	mergeBuffer := 30 * time.Second
	if liveCfg.MakerMergeBufferSeconds > 0 {
		mergeBuffer = time.Duration(liveCfg.MakerMergeBufferSeconds) * time.Second
	} else if cfg.MakerMergeBufferSeconds > 0 {
		mergeBuffer = time.Duration(cfg.MakerMergeBufferSeconds) * time.Second
	}
	if timeToEnd > 0 && timeToEnd < mergeBuffer {
		realbotCancelAllMakerQuotes(ctx, marketID, "near expiry cleanup", trader, engine, tui, makerQuotes)
		return
	}

	bid0, ask0 := tokenBids[outcomes[0]], tokenAsks[outcomes[0]]
	bid1, ask1 := tokenBids[outcomes[1]], tokenAsks[outcomes[1]]
	if bid0 <= 0 || ask0 <= 0 || bid1 <= 0 || ask1 <= 0 {
		realbotCancelAllMakerQuotes(ctx, marketID, "waiting for valid bid/ask on both outcomes", trader, engine, tui, makerQuotes)
		return
	}

	shares0, avg0 := localBoughtPositionAvg(engine, marketID, outcomes[0])
	shares1, avg1 := localBoughtPositionAvg(engine, marketID, outcomes[1])

	// Auto-merge delta-neutral inventory to free up capital and permanently lock in the spread profit
	if shares0 > 0 && shares1 > 0 {
		mergeQty := math.Min(shares0, shares1)
		if mergeQty >= 1.0 {
			engine.MergeForMarket(marketID, outcomes[0], outcomes[1], mergeQty)
			// Re-fetch after merge
			shares0, avg0 = localBoughtPositionAvg(engine, marketID, outcomes[0])
			shares1, avg1 = localBoughtPositionAvg(engine, marketID, outcomes[1])
		}
	}

	currentCash := engine.GetBalance()
	reservedBuyNotional := realbotMakerReservedBuyNotional(makerQuotes)
	quoteCash := math.Max(0, currentCash-reservedBuyNotional)

	minQuoteValue := cfg.MakerMinQuoteValue
	if liveCfg.MakerMinQuoteValue > 0 {
		minQuoteValue = liveCfg.MakerMinQuoteValue
	}
	if minQuoteValue <= 0 {
		minQuoteValue = realbotMakerMinQuoteValue
	}
	targetMult := cfg.MakerInventoryTargetMult
	if liveCfg.MakerInventoryTargetMult > 0 {
		targetMult = liveCfg.MakerInventoryTargetMult
	}
	if targetMult <= 0 {
		targetMult = realbotMakerInventoryTargetMult
	}
	capMult := cfg.MakerInventoryCapMult
	if liveCfg.MakerInventoryCapMult > 0 {
		capMult = liveCfg.MakerInventoryCapMult
	}
	if capMult <= 0 {
		capMult = realbotMakerInventoryCapMult
	}

	baseTradeValue := cfg.CalculateTradeSize(realbotSizingCapitalForTrade(engine, liveCfg))
	// We no longer clamp baseTradeValue up to minQuoteValue to avoid forcing users
	// to trade larger amounts than their configured TradeScaleFactor. If baseTradeValue
	// is too small, strategy.ComputeMakerBuyQty will return 0 and skip quoting.

	targetValue := math.Max(minQuoteValue, baseTradeValue*targetMult)
	maxInventoryValue := math.Max(targetValue, baseTradeValue*capMult)
	minPairEdge := liveCfg.MinMarginPercent / 100.0
	maxPairCost := 1.0 - minPairEdge

	makerParams := realbotMakerStrategyParams
	makerParams.MinQuoteValue = minQuoteValue

	pairedShares := math.Min(shares0, shares1)
	inventoryDelta := shares0 - shares1
	buyPrice0, buyPrice1, buyOK := computeRealbotMakerPairBuyPrices(bid0, ask0, bid1, ask1, maxPairCost, inventoryDelta, makerParams)
	maxMakerBuyPrice := liveCfg.MaxAskPrice
	if maxMakerBuyPrice <= 0 || maxMakerBuyPrice > 0.99 {
		maxMakerBuyPrice = 0.99
	}
	minMakerBuyPrice := liveCfg.MinAskPrice
	if !buyOK || buyPrice0 > maxMakerBuyPrice || buyPrice0 < minMakerBuyPrice || buyPrice1 > maxMakerBuyPrice || buyPrice1 < minMakerBuyPrice {
		buyOK = false
	}
	buyQty0 := 0.0
	buyQty1 := 0.0
	if buyOK {
		pairQuoteQty := computeRealbotMakerPairQuoteQty(baseTradeValue, pairedShares, maxInventoryValue, quoteCash, buyPrice0, buyPrice1, makerParams)
		maxImbalanceShares := normalizeMarketSellShares(math.Max(1.0, baseTradeValue/math.Max(maxPairCost, realbotMakerQuoteStep)))
		if pairQuoteQty > maxImbalanceShares {
			maxImbalanceShares = pairQuoteQty
		}

		buyQty0 = pairQuoteQty
		buyQty1 = pairQuoteQty
		if inventoryDelta > 0 {
			heavyScale := math.Min(1.0, inventoryDelta/math.Max(maxImbalanceShares, 1.0))
			buyQty0 = normalizeMarketSellShares(pairQuoteQty * (1.0 - heavyScale))
			if avg0 > 0 && buyPrice1+avg0 > maxPairCost+1e-9 {
				buyQty1 = 0
			}
		} else if inventoryDelta < 0 {
			heavyScale := math.Min(1.0, (-inventoryDelta)/math.Max(maxImbalanceShares, 1.0))
			buyQty1 = normalizeMarketSellShares(pairQuoteQty * (1.0 - heavyScale))
			if avg1 > 0 && buyPrice0+avg1 > maxPairCost+1e-9 {
				buyQty0 = 0
			}
		}
	}

	changed := false
	if realbotUpsertMakerQuote(ctx, marketID, trader, riskMgr, tui, makerQuotes, openByID, api.SideBuy, outcomes[0], getTokenID(outcomes[0]), buyPrice0, buyQty0, tokenFeeRates[outcomes[0]]) {
		changed = true
	}
	if realbotUpsertMakerQuote(ctx, marketID, trader, riskMgr, tui, makerQuotes, openByID, api.SideBuy, outcomes[1], getTokenID(outcomes[1]), buyPrice1, buyQty1, tokenFeeRates[outcomes[1]]) {
		changed = true
	}
	if realbotUpsertMakerQuote(ctx, marketID, trader, riskMgr, tui, makerQuotes, openByID, api.SideSell, outcomes[0], getTokenID(outcomes[0]), 0, 0, tokenFeeRates[outcomes[0]]) {
		changed = true
	}
	if realbotUpsertMakerQuote(ctx, marketID, trader, riskMgr, tui, makerQuotes, openByID, api.SideSell, outcomes[1], getTokenID(outcomes[1]), 0, 0, tokenFeeRates[outcomes[1]]) {
		changed = true
	}

	if lastMakerSync != nil {
		*lastMakerSync = time.Now()
	}
	if changed {
		tui.LogEvent("[%s] 🧾 Live maker pair bids refreshed: %s buy@$%.3f x %.0f | %s buy@$%.3f x %.0f | pair=$%.3f",
			marketID,
			outcomes[0], buyPrice0, buyQty0,
			outcomes[1], buyPrice1, buyQty1,
			buyPrice0+buyPrice1,
		)
	}
	realbotUpdateMakerPendingOrders(marketID, makerQuotes, tui)
}

func localBoughtPositionAvg(engine *paper.Engine, marketID, outcome string) (qty, avgPrice float64) {
	if engine == nil || marketID == "" || outcome == "" {
		return 0, 0
	}
	positions := engine.GetPositions()
	totalCost := 0.0
	for _, pos := range positions {
		if pos.MarketID != marketID || pos.Outcome != outcome || pos.Quantity <= 0 {
			continue
		}
		qty += pos.Quantity
		totalCost += pos.TotalCost
	}
	if qty <= 0 {
		return 0, 0
	}
	return qty, totalCost / qty
}

func realbotPanicBuyCompletionGuard(engine *paper.Engine, marketID, outcome0, outcome1 string, ask0, ask1, minMarginPercent float64) (bool, string) {
	if engine == nil {
		return false, ""
	}
	maxCompletionSum := 1.0 - (minMarginPercent / 100.0)
	if maxCompletionSum > 1.0 {
		maxCompletionSum = 1.0
	}
	if maxCompletionSum < 0 {
		maxCompletionSum = 0
	}

	qty0, avg0 := localBoughtPositionAvg(engine, marketID, outcome0)
	qty1, avg1 := localBoughtPositionAvg(engine, marketID, outcome1)

	if excess0 := qty0 - qty1; excess0 > 1e-6 && avg0 > 0 && ask1 > 0 {
		completionSum := avg0 + ask1
		if completionSum > maxCompletionSum+1e-9 {
			return true, fmt.Sprintf("existing %s imbalance %s @ avg %.3f would complete via %s ask %.3f at $%.3f, above $%.3f target", outcome0, formatShareQty(excess0), avg0, outcome1, ask1, completionSum, maxCompletionSum)
		}
	}
	if excess1 := qty1 - qty0; excess1 > 1e-6 && avg1 > 0 && ask0 > 0 {
		completionSum := avg1 + ask0
		if completionSum > maxCompletionSum+1e-9 {
			return true, fmt.Sprintf("existing %s imbalance %s @ avg %.3f would complete via %s ask %.3f at $%.3f, above $%.3f target", outcome1, formatShareQty(excess1), avg1, outcome0, ask0, completionSum, maxCompletionSum)
		}
	}
	return false, ""
}

func clampExecutionMarginFloor(minMarginPercent, executionFloorPercent float64) float64 {
	if executionFloorPercent > minMarginPercent {
		return minMarginPercent
	}
	return executionFloorPercent
}

func maxExecutablePairSum(executionFloorPercent, maxAskPrice float64) float64 {
	maxSum := 1.0 - (executionFloorPercent / 100.0)
	if maxAskPrice > 0 {
		capSum := maxAskPrice * 2.0
		if maxSum > capSum {
			maxSum = capSum
		}
	}
	if maxSum < 0 {
		return 0
	}
	return maxSum
}

func minExecutablePairSum(executionFloorPercent, minAskPrice float64) float64 {
	minSum := 1.0 + (executionFloorPercent / 100.0)
	if minAskPrice > 0 {
		floorSum := minAskPrice * 2.0
		if minSum < floorSum {
			minSum = floorSum
		}
	}
	if minSum > 2.0 {
		return 2.0
	}
	return minSum
}

func normalizeExecutionToleranceFraction(raw float64) float64 {
	raw = math.Abs(raw)
	switch {
	case raw == 0:
		return 0
	case raw <= 0.25:
		return raw
	default:
		return raw / 100.0
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

	// Size against the cap we are actually willing to pay so the submitted order
	// cannot exceed the configured trade budget when the live price is lower than
	// the close-market limit.
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

func realbotLooksLikeTerminalBook(outcomes []string, tokenBids, tokenAsks map[string]float64) bool {
	if len(outcomes) == 0 {
		return false
	}

	sawExtreme := false
	for _, outcome := range outcomes {
		bid := tokenBids[outcome]
		ask := tokenAsks[outcome]

		if bid > 0 && bid < terminalBidFloor {
			return false
		}
		if ask > 0 && ask > terminalAskCeil {
			return false
		}
		if bid >= terminalBidFloor || (ask > 0 && ask <= terminalAskCeil) {
			sawExtreme = true
		}
	}

	return sawExtreme
}

func realbotHasSaneTopOfBook(bid, ask float64) bool {
	if bid <= 0 || ask <= 0 || bid >= ask {
		return false
	}
	if bid >= terminalBidFloor || ask <= terminalAskCeil {
		return true
	}
	return (ask - bid) <= realbotMaxSaneOutcomeSpread
}

// realbotPairHasHighBid returns true if either outcome in the pair has a
// valid bid at or above the given threshold. This signals a high-price
// market regime where the complement naturally has sparse liquidity.
const realbotHighBidThreshold = 0.60

func realbotPairHasHighBid(outcomes []string, tokenBids map[string]float64) bool {
	for _, out := range outcomes {
		if tokenBids[out] >= realbotHighBidThreshold {
			return true
		}
	}
	return false
}

func realbotLocalQuoteSanityReason(outcomes []string, tokenBids, tokenAsks map[string]float64) string {
	highBidPresent := realbotPairHasHighBid(outcomes, tokenBids)

	for _, out := range outcomes {
		bid := tokenBids[out]
		ask := tokenAsks[out]
		if !realbotHasSaneTopOfBook(bid, ask) {
			if bid <= 0 || ask <= 0 {
				// In high-price regimes the complement side naturally has
				// sparse or missing asks. Tolerate a one-sided book when
				// the pair has a high bid so we don't keep clearing data.
				if highBidPresent {
					continue
				}
				return fmt.Sprintf("missing two-sided quote for %s", out)
			}
			if bid >= ask {
				return fmt.Sprintf("crossed local quote for %s (bid %.3f >= ask %.3f)", out, bid, ask)
			}
			return fmt.Sprintf("wide local spread for %s (bid %.3f ask %.3f spread %.3f > %.3f)", out, bid, ask, ask-bid, realbotMaxSaneOutcomeSpread)
		}
	}

	if len(outcomes) == 2 && !realbotLooksLikeTerminalBook(outcomes, tokenBids, tokenAsks) {
		askSum := tokenAsks[outcomes[0]] + tokenAsks[outcomes[1]]
		// When one side has a high bid, the complementary ask is near zero
		// by definition, so an ask sum > threshold should only be enforced
		// in balanced-market conditions.
		if !highBidPresent && askSum > realbotMaxSaneAskPairSum {
			return fmt.Sprintf("ask pair sum %.3f > %.3f", askSum, realbotMaxSaneAskPairSum)
		}
		bidSum := tokenBids[outcomes[0]] + tokenBids[outcomes[1]]
		if bidSum < realbotMinSaneBidPairSum {
			return fmt.Sprintf("bid pair sum %.3f < %.3f", bidSum, realbotMinSaneBidPairSum)
		}
	}

	return ""
}

func realbotHasSanePairQuotes(outcomes []string, tokenBids, tokenAsks map[string]float64) bool {
	return realbotLocalQuoteSanityReason(outcomes, tokenBids, tokenAsks) == ""
}

func realbotExecutionQuoteGuardAge(localQuoteMaxAge time.Duration) time.Duration {
	if localQuoteMaxAge <= 0 || localQuoteMaxAge > realbotExecutionGuardQuoteMaxAge {
		return realbotExecutionGuardQuoteMaxAge
	}
	return localQuoteMaxAge
}

func realbotBestAskFromLevels(levels []paper.MarketLevel) (float64, bool) {
	bestAsk := 1.0
	found := false
	for _, lvl := range levels {
		if lvl.Price > 0 && lvl.Price < bestAsk {
			bestAsk = lvl.Price
			found = true
		}
	}
	if !found {
		return 0, false
	}
	return bestAsk, true
}

func realbotBestBidFromLevels(levels []paper.MarketLevel) (float64, bool) {
	bestBid := 0.0
	found := false
	for _, lvl := range levels {
		if lvl.Price > bestBid {
			bestBid = lvl.Price
			found = true
		}
	}
	if !found {
		return 0, false
	}
	return bestBid, true
}

func realbotCanUseLocalBuyQuote(now time.Time, outcomes []string, tokenBids, tokenAsks map[string]float64, tokenFullAsks map[string][]paper.MarketLevel, quoteState map[string]realbotQuoteState, maxAge time.Duration) (bool, time.Duration, string) {
	maxObservedAge := time.Duration(0)
	for _, out := range outcomes {
		if tokenAsks[out] <= 0 {
			return false, maxObservedAge, fmt.Sprintf("missing local ask for %s", out)
		}
		if len(tokenFullAsks[out]) == 0 {
			return false, maxObservedAge, fmt.Sprintf("missing local ask depth for %s", out)
		}
		state, ok := quoteState[out]
		if !ok || state.UpdatedAt.IsZero() {
			return false, maxObservedAge, fmt.Sprintf("missing quote timestamp for %s", out)
		}
		age := now.Sub(state.UpdatedAt)
		if age > maxObservedAge {
			maxObservedAge = age
		}
		if age > maxAge {
			return false, maxObservedAge, fmt.Sprintf("%s quote age %s > %s", out, age.Round(time.Millisecond), maxAge)
		}
	}
	if reason := realbotLocalQuoteSanityReason(outcomes, tokenBids, tokenAsks); reason != "" {
		return false, maxObservedAge, reason
	}
	return true, maxObservedAge, ""
}

func realbotCanUseLocalSellQuote(now time.Time, outcomes []string, tokenBids, tokenAsks map[string]float64, tokenFullBids map[string][]paper.MarketLevel, quoteState map[string]realbotQuoteState, maxAge time.Duration) (bool, time.Duration, string) {
	maxObservedAge := time.Duration(0)
	for _, out := range outcomes {
		if tokenBids[out] <= 0 {
			return false, maxObservedAge, fmt.Sprintf("missing local bid for %s", out)
		}
		if len(tokenFullBids[out]) == 0 {
			return false, maxObservedAge, fmt.Sprintf("missing local bid depth for %s", out)
		}
		state, ok := quoteState[out]
		if !ok || state.UpdatedAt.IsZero() {
			return false, maxObservedAge, fmt.Sprintf("missing quote timestamp for %s", out)
		}
		age := now.Sub(state.UpdatedAt)
		if age > maxObservedAge {
			maxObservedAge = age
		}
		if age > maxAge {
			return false, maxObservedAge, fmt.Sprintf("%s quote age %s > %s", out, age.Round(time.Millisecond), maxAge)
		}
	}
	if reason := realbotLocalQuoteSanityReason(outcomes, tokenBids, tokenAsks); reason != "" {
		return false, maxObservedAge, reason
	}
	return true, maxObservedAge, ""
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

func realbotRefreshExecutionBooks(ctx context.Context, restClient *api.RestClient, market *api.Market, outcomes []string, tokenBids, tokenAsks map[string]float64, tokenFullBids, tokenFullAsks map[string][]paper.MarketLevel, quoteState map[string]realbotQuoteState) (time.Duration, error) {
	type quoteResult struct {
		outcome string
		bids    []paper.MarketLevel
		asks    []paper.MarketLevel
		latency time.Duration
		err     error
	}

	results := make(chan quoteResult, len(outcomes))
	var wg sync.WaitGroup
	for _, out := range outcomes {
		tokenID := mkt.GetTokenIDForOutcome(market, out)
		if tokenID == "" {
			return 0, fmt.Errorf("missing token id for outcome %s", out)
		}
		wg.Add(1)
		go func(outcome, token string) {
			defer wg.Done()
			start := time.Now()
			book, err := restClient.GetOrderBook(ctx, token)
			latency := time.Since(start)
			if err != nil {
				results <- quoteResult{outcome: outcome, latency: latency, err: err}
				return
			}
			age, ageErr := api.OrderBookAgeAt(book, time.Now())
			if ageErr != nil {
				results <- quoteResult{outcome: outcome, latency: latency, err: fmt.Errorf("invalid order book timestamp: %w", ageErr)}
				return
			}
			if age > realbotRestBookMaxAge {
				results <- quoteResult{outcome: outcome, latency: latency, err: fmt.Errorf("stale order book age %s > %s", age.Round(time.Millisecond), realbotRestBookMaxAge)}
				return
			}
			results <- quoteResult{
				outcome: outcome,
				bids:    mkt.LevelsToPriceDepth(book.Bids, true),
				asks:    mkt.LevelsToPriceDepth(book.Asks, false),
				latency: latency,
			}
		}(out, tokenID)
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	var maxLatency time.Duration
	for res := range results {
		if res.latency > maxLatency {
			maxLatency = res.latency
		}
		if res.err != nil {
			return maxLatency, fmt.Errorf("fetching fresh order book for %s failed: %w", res.outcome, res.err)
		}
		tokenFullBids[res.outcome] = res.bids
		tokenFullAsks[res.outcome] = res.asks
		bestBid, hasBid := realbotBestBidFromLevels(res.bids)
		bestAsk, hasAsk := realbotBestAskFromLevels(res.asks)
		if !hasBid || !hasAsk || !realbotHasSaneTopOfBook(bestBid, bestAsk) {
			return maxLatency, fmt.Errorf("invalid refreshed book for %s", res.outcome)
		}
		tokenBids[res.outcome] = bestBid
		tokenAsks[res.outcome] = bestAsk
		quoteState[res.outcome] = realbotQuoteState{UpdatedAt: time.Now(), Source: "rest-exec"}
	}
	if reason := realbotLocalQuoteSanityReason(outcomes, tokenBids, tokenAsks); reason != "" {
		return maxLatency, fmt.Errorf("invalid refreshed pair quote: %s", reason)
	}
	return maxLatency, nil
}

func realbotEnsureFreshBuyExecutionQuote(ctx context.Context, restClient *api.RestClient, market *api.Market, outcomes []string, tokenBids, tokenAsks map[string]float64, tokenFullBids, tokenFullAsks map[string][]paper.MarketLevel, quoteState map[string]realbotQuoteState, localQuoteMaxAge time.Duration) (source string, metric time.Duration, detail string, err error) {
	now := time.Now()
	fresh, age, reason := realbotCanUseLocalBuyQuote(now, outcomes, tokenBids, tokenAsks, tokenFullAsks, quoteState, localQuoteMaxAge)
	if fresh {
		return "local", age, "", nil
	}
	latency, refreshErr := realbotRefreshExecutionBooks(ctx, restClient, market, outcomes, tokenBids, tokenAsks, tokenFullBids, tokenFullAsks, quoteState)
	if refreshErr != nil {
		return "rest", latency, reason, fmt.Errorf("local quote unavailable (%s): %w", reason, refreshErr)
	}
	return "rest", latency, reason, nil
}

func realbotEnsureFreshSellExecutionQuote(ctx context.Context, restClient *api.RestClient, market *api.Market, outcomes []string, tokenBids, tokenAsks map[string]float64, tokenFullBids, tokenFullAsks map[string][]paper.MarketLevel, quoteState map[string]realbotQuoteState, localQuoteMaxAge time.Duration) (source string, metric time.Duration, detail string, err error) {
	now := time.Now()
	fresh, age, reason := realbotCanUseLocalSellQuote(now, outcomes, tokenBids, tokenAsks, tokenFullBids, quoteState, localQuoteMaxAge)
	if fresh {
		return "local", age, "", nil
	}
	latency, refreshErr := realbotRefreshExecutionBooks(ctx, restClient, market, outcomes, tokenBids, tokenAsks, tokenFullBids, tokenFullAsks, quoteState)
	if refreshErr != nil {
		return "rest", latency, reason, fmt.Errorf("local quote unavailable (%s): %w", reason, refreshErr)
	}
	return "rest", latency, reason, nil
}

type realbotCleanupSellQuote struct {
	SubmitPrice       float64
	BestBid           float64
	ExecutableQty     float64
	BookAge           time.Duration
	FetchLatency      time.Duration
	TotalBidLiquidity float64
}

func realbotBuildCleanupSellQuote(ctx context.Context, restClient *api.RestClient, tokenID string, requestedQty, configuredFloor float64) (realbotCleanupSellQuote, error) {
	start := time.Now()
	book, err := restClient.GetOrderBook(ctx, tokenID)
	latency := time.Since(start)
	if err != nil {
		return realbotCleanupSellQuote{}, err
	}
	age, err := api.OrderBookAgeAt(book, time.Now())
	if err != nil {
		return realbotCleanupSellQuote{}, err
	}
	if age > realbotRestBookMaxAge {
		return realbotCleanupSellQuote{}, fmt.Errorf("stale order book age %s > %s", age.Round(time.Millisecond), realbotRestBookMaxAge)
	}
	bids := mkt.LevelsToPriceDepth(book.Bids, true)
	bestBid, hasBid := realbotBestBidFromLevels(bids)
	if !hasBid || bestBid <= 0 {
		return realbotCleanupSellQuote{}, fmt.Errorf("no live bid found")
	}
	submitPrice := core.CleanupSellLimitPrice(configuredFloor)
	if bestBid < submitPrice {
		submitPrice = bestBid
	}
	totalBidLiquidity := 0.0
	for _, lvl := range bids {
		if lvl.Price+1e-9 >= submitPrice {
			totalBidLiquidity += lvl.Size
		}
	}
	executableQty := normalizeMarketSellShares(math.Min(requestedQty, totalBidLiquidity))
	if executableQty < minOnChainActionShares {
		return realbotCleanupSellQuote{}, fmt.Errorf("live bid liquidity %.4f below %.2f shares at $%.3f", totalBidLiquidity, minOnChainActionShares, submitPrice)
	}
	return realbotCleanupSellQuote{
		SubmitPrice:       submitPrice,
		BestBid:           bestBid,
		ExecutableQty:     executableQty,
		BookAge:           age,
		FetchLatency:      latency,
		TotalBidLiquidity: totalBidLiquidity,
	}, nil
}

func realbotMatchedAskLiquidity(asks0, asks1 []paper.MarketLevel, maxExecutionSum float64) float64 {
	return mkt.EstimateMatchedLiquidity(
		append([]paper.MarketLevel(nil), asks0...),
		append([]paper.MarketLevel(nil), asks1...),
		func(i, j int, levels []paper.MarketLevel) bool { return levels[i].Price < levels[j].Price },
		func(p1, p2 float64) bool { return p1+p2 <= maxExecutionSum },
	)
}

func realbotMatchedBidLiquidity(bids0, bids1 []paper.MarketLevel, minExecutionSum float64) float64 {
	return mkt.EstimateMatchedLiquidity(
		append([]paper.MarketLevel(nil), bids0...),
		append([]paper.MarketLevel(nil), bids1...),
		func(i, j int, levels []paper.MarketLevel) bool { return levels[i].Price > levels[j].Price },
		func(p1, p2 float64) bool { return p1+p2 >= minExecutionSum },
	)
}

func subtractMergedPairBalances(bal0, bal1, mergeQty float64) (float64, float64) {
	if mergeQty <= 0 {
		return bal0, bal1
	}
	return math.Max(0, bal0-mergeQty), math.Max(0, bal1-mergeQty)
}

func preferLivePairBalances(live0, live1, backup0, backup1 float64) (float64, float64) {
	return math.Max(live0, backup0), math.Max(live1, backup1)
}

func combinePairBalanceSnapshots(live0, live1, backup0, backup1 float64, backupErr error) (bal0, bal1 float64, source string, err error) {
	hasLive := shouldAttemptCleanupSell(live0) || shouldAttemptCleanupSell(live1)
	hasBackup := shouldAttemptCleanupSell(backup0) || shouldAttemptCleanupSell(backup1)

	if backupErr != nil {
		if hasLive {
			return live0, live1, "live WS", nil
		}
		return 0, 0, "", backupErr
	}

	bal0, bal1 = preferLivePairBalances(live0, live1, backup0, backup1)
	source = "live WS"
	switch {
	case hasLive && hasBackup:
		source = "live WS + on-chain backup"
	case hasBackup:
		source = "on-chain backup"
	}
	return bal0, bal1, source, nil
}

func loadPairBalancesWSFirst(ctx context.Context, trader *trading.RealTrader, token0, token1 string) (bal0, bal1 float64, source string, err error) {
	live0 := trader.GetLivePositionSize(token0)
	live1 := trader.GetLivePositionSize(token1)
	backup0, backup1, backupErr := loadPairBalances(ctx, trader, token0, token1)
	return combinePairBalanceSnapshots(live0, live1, backup0, backup1, backupErr)
}

func loadPairBalances(ctx context.Context, trader *trading.RealTrader, token0, token1 string) (float64, float64, error) {
	pos0, pos1, posErr := loadPairPositionBalances(ctx, trader, token0, token1)
	onChain0, onChain1, onChainErr := loadPairOnChainBalances(ctx, trader, token0, token1)

	switch {
	case posErr == nil && onChainErr == nil:
		bal0, bal1 := preferLivePairBalances(pos0, pos1, onChain0, onChain1)
		return bal0, bal1, nil
	case onChainErr == nil:
		return onChain0, onChain1, nil
	case posErr == nil:
		return pos0, pos1, nil
	default:
		return 0, 0, fmt.Errorf("external position snapshot failed (%v); on-chain backup failed (%v)", posErr, onChainErr)
	}
}

func loadPairPositionBalances(ctx context.Context, trader *trading.RealTrader, token0, token1 string) (float64, float64, error) {
	positions, err := trader.GetPositions(ctx)
	if err != nil {
		return 0, 0, err
	}
	bal0, bal1 := pairBalancesFromPositions(positions, token0, token1)
	return bal0, bal1, nil
}

func loadPairOnChainBalances(ctx context.Context, trader *trading.RealTrader, token0, token1 string) (float64, float64, error) {
	bal0, err0 := trader.GetCTFBalanceFloat(ctx, token0)
	bal1, err1 := trader.GetCTFBalanceFloat(ctx, token1)
	if err0 != nil || err1 != nil {
		return bal0, bal1, fmt.Errorf("on-chain balance check failed (err0=%v err1=%v)", err0, err1)
	}
	return bal0, bal1, nil
}

func incrementalBalance(initial, current float64) float64 {
	if current <= initial {
		return 0
	}
	return current - initial
}

func acquiredPairBalances(initial0, initial1, current0, current1 float64, haveInitialSnapshot bool) (float64, float64) {
	if !haveInitialSnapshot {
		return current0, current1
	}
	return incrementalBalance(initial0, current0), incrementalBalance(initial1, current1)
}

func queryLivePairBalanceDelta(ctx context.Context, trader *trading.RealTrader, token0, token1 string, initial0, initial1 float64, haveInitialSnapshot bool) (acquired0, acquired1, bal0, bal1 float64) {
	for {
		bal0 = trader.GetLivePositionSize(token0)
		bal1 = trader.GetLivePositionSize(token1)
		acquired0, acquired1 = acquiredPairBalances(initial0, initial1, bal0, bal1, haveInitialSnapshot)
		if shouldAttemptCleanupSell(acquired0) || shouldAttemptCleanupSell(acquired1) {
			return acquired0, acquired1, bal0, bal1
		}
		select {
		case <-ctx.Done():
			return acquired0, acquired1, bal0, bal1
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func queryOnChainPairBalanceDelta(ctx context.Context, trader *trading.RealTrader, token0, token1 string, initial0, initial1 float64, haveInitialSnapshot bool) (acquired0, acquired1, bal0, bal1 float64, err error) {
	for {
		bal0, bal1, err = loadPairOnChainBalances(ctx, trader, token0, token1)
		if err == nil {
			acquired0, acquired1 = acquiredPairBalances(initial0, initial1, bal0, bal1, haveInitialSnapshot)
			if shouldAttemptCleanupSell(acquired0) || shouldAttemptCleanupSell(acquired1) {
				return acquired0, acquired1, bal0, bal1, nil
			}
		}
		select {
		case <-ctx.Done():
			return acquired0, acquired1, bal0, bal1, err
		case <-time.After(750 * time.Millisecond):
		}
	}
}

func reconcileBoughtPairBalances(ctx context.Context, trader *trading.RealTrader, token0, token1 string, initial0, initial1 float64, haveInitialSnapshot bool) (acquired0, acquired1, bal0, bal1 float64, source string, err error) {
	liveWindow := 2 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < liveWindow {
			liveWindow = remaining
		}
	}
	if liveWindow < 0 {
		liveWindow = 0
	}

	var live0, live1 float64
	if liveWindow > 0 {
		liveCtx, cancel := context.WithTimeout(ctx, liveWindow)
		defer cancel()
		acquired0, acquired1, live0, live1 = queryLivePairBalanceDelta(liveCtx, trader, token0, token1, initial0, initial1, haveInitialSnapshot)
		source = "live WS"
	}

	onChainAcquired0, onChainAcquired1, onChain0, onChain1, onChainErr := queryOnChainPairBalanceDelta(ctx, trader, token0, token1, initial0, initial1, haveInitialSnapshot)
	if onChainErr == nil {
		acquired0 = math.Max(acquired0, onChainAcquired0)
		acquired1 = math.Max(acquired1, onChainAcquired1)
		bal0, bal1 = preferLivePairBalances(live0, live1, onChain0, onChain1)
		if source == "" {
			source = "on-chain delta"
		} else {
			source += " + on-chain delta"
		}
		return acquired0, acquired1, bal0, bal1, source, nil
	}

	if shouldAttemptCleanupSell(acquired0) || shouldAttemptCleanupSell(acquired1) {
		return acquired0, acquired1, live0, live1, source, nil
	}
	if source == "" {
		source = "unavailable"
	}
	return acquired0, acquired1, live0, live1, source, onChainErr
}

func syncWalletTruthPositions(ctx context.Context, marketID string, tokenToOutcome map[string]string, trader *trading.RealTrader, engine *paper.Engine, splitInventory *paper.SplitInventory, tui *paper.TUI) (bool, error) {
	enginePositions := engine.GetPositions()
	localByOutcome := make(map[string]float64)
	for _, pos := range enginePositions {
		if pos.MarketID != marketID {
			continue
		}
		localByOutcome[pos.Outcome] += pos.Quantity
	}

	positions := make([]paper.WalletTruthPosition, 0, len(tokenToOutcome))
	changed := false
	for tokenID, outcome := range tokenToOutcome {
		if tokenID == "" || outcome == "" {
			continue
		}
		onChainShares, err := trader.GetCTFBalanceFloat(ctx, tokenID)
		if err != nil {
			return changed, err
		}
		localBoughtShares := localByOutcome[outcome]
		splitShares := 0.0
		if splitInventory != nil {
			splitShares = splitInventory.GetSplitShares(marketID, outcome)
		}
		desiredBoughtShares := math.Max(0, onChainShares-splitShares)
		if desiredBoughtShares > localBoughtShares+1e-6 {
			addQty := desiredBoughtShares - localBoughtShares
			if engine.SyncExternalPosition(marketID, outcome, desiredBoughtShares, walletTruthSyncMarkPrice(engine, marketID, outcome)) {
				if addQty >= realbotWalletTruthLogMinDelta {
					tui.LogEvent("[%s] 🧾 Wallet-truth sync restored missing %s inventory by %s (local %.4f → on-chain %.4f, split %.4f)", marketID, outcome, formatShareQty(addQty), localBoughtShares, onChainShares, splitShares)
				}
				changed = true
			}
			localBoughtShares = desiredBoughtShares
		}
		localShares := localBoughtShares + splitShares
		positions = append(positions, paper.WalletTruthPosition{
			MarketID:      marketID,
			Outcome:       outcome,
			LocalShares:   localShares,
			OnChainShares: onChainShares,
			Drift:         onChainShares - localShares,
		})
	}
	sort.Slice(positions, func(i, j int) bool {
		if positions[i].MarketID == positions[j].MarketID {
			return positions[i].Outcome < positions[j].Outcome
		}
		return positions[i].MarketID < positions[j].MarketID
	})
	tui.SetWalletTruthPositions(marketID, positions)
	return changed, nil
}

func realbotReconcileTrackedRoundWalletTruth(ctx context.Context, markets map[string]*api.Market, trader *trading.RealTrader, engine *paper.Engine, splitInventories map[string]*paper.SplitInventory, splitMu *sync.Mutex, tui *paper.TUI) (int, error) {
	if trader == nil || engine == nil || len(markets) == 0 {
		return 0, nil
	}

	changedMarkets := 0
	var firstErr error

	for assetID, market := range markets {
		if market == nil {
			continue
		}

		tokenToOutcome := make(map[string]string)
		for _, token := range market.Tokens {
			if token.TokenID == "" || token.Outcome == "" {
				continue
			}
			tokenToOutcome[token.TokenID] = token.Outcome
		}
		if len(tokenToOutcome) == 0 {
			continue
		}

		marketID := mkt.ScopedMarketID(assetID, market)
		var splitInventory *paper.SplitInventory
		if splitMu != nil {
			splitMu.Lock()
			splitInventory = splitInventories[market.ConditionID]
			splitMu.Unlock()
		} else {
			splitInventory = splitInventories[market.ConditionID]
		}

		marketCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		changed, err := syncWalletTruthPositions(marketCtx, marketID, tokenToOutcome, trader, engine, splitInventory, tui)
		cancel()
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", marketID, err)
			}
			continue
		}
		if changed {
			changedMarkets++
		}
	}

	return changedMarkets, firstErr
}

func localBoughtPairBalances(engine *paper.Engine, marketID, outcome0, outcome1 string) (bal0, bal1 float64) {
	positions := engine.GetPositions()
	for _, pos := range positions {
		if pos.MarketID != marketID || pos.Quantity <= 0 {
			continue
		}
		switch pos.Outcome {
		case outcome0:
			bal0 += pos.Quantity
		case outcome1:
			bal1 += pos.Quantity
		}
	}
	return bal0, bal1
}

func pendingPairRecoveryBalances(ctx context.Context, marketID, token0, token1 string, outcomes []string, trader *trading.RealTrader, engine *paper.Engine, splitInventory *paper.SplitInventory) (bal0, bal1 float64, source string, err error) {
	if len(outcomes) != 2 {
		return 0, 0, "", nil
	}
	local0, local1 := localBoughtPairBalances(engine, marketID, outcomes[0], outcomes[1])
	if hasActionableCleanupRemainder(local0) || hasActionableCleanupRemainder(local1) {
		return local0, local1, "local engine", nil
	}
	onChain0, onChain1, err := loadPairOnChainBalances(ctx, trader, token0, token1)
	if err != nil {
		return 0, 0, "", err
	}
	split0, split1 := 0.0, 0.0
	if splitInventory != nil {
		split0 = splitInventory.GetSplitShares(marketID, outcomes[0])
		split1 = splitInventory.GetSplitShares(marketID, outcomes[1])
	}
	return math.Max(0, onChain0-split0), math.Max(0, onChain1-split1), "on-chain truth", nil
}

func walletTruthSyncMarkPrice(engine *paper.Engine, marketID, outcome string) float64 {
	bid, ask := engine.GetMarketBidAsk(marketID, outcome)
	if bid >= 0.01 {
		return bid
	}
	if ask >= 0.01 {
		return ask
	}
	return 0.50
}

func reconcileLocalBoughtPositionsToWalletTruth(ctx context.Context, marketID, token0, token1 string, outcomes []string, trader *trading.RealTrader, engine *paper.Engine, splitInventory *paper.SplitInventory, tui *paper.TUI) (bool, error) {
	if len(outcomes) != 2 {
		return false, nil
	}
	onChain0, onChain1, err := loadPairOnChainBalances(ctx, trader, token0, token1)
	if err != nil {
		return false, err
	}
	local0, local1 := localBoughtPairBalances(engine, marketID, outcomes[0], outcomes[1])
	split0, split1 := 0.0, 0.0
	if splitInventory != nil {
		split0 = splitInventory.GetSplitShares(marketID, outcomes[0])
		split1 = splitInventory.GetSplitShares(marketID, outcomes[1])
	}
	desired0 := math.Max(0, onChain0-split0)
	desired1 := math.Max(0, onChain1-split1)
	changed := false
	if local0 > desired0+1e-6 {
		trimQty := local0 - desired0
		if engine.SyncExternalPosition(marketID, outcomes[0], desired0, walletTruthSyncMarkPrice(engine, marketID, outcomes[0])) {
			if trimQty >= realbotWalletTruthLogMinDelta {
				tui.LogEvent("[%s] 🧾 Wallet-truth sync trimmed stale %s inventory by %s (local %.4f → on-chain %.4f, split %.4f)", marketID, outcomes[0], formatShareQty(trimQty), local0, onChain0, split0)
			}
			changed = true
		}
	} else if desired0 > local0+1e-6 {
		addQty := desired0 - local0
		if engine.SyncExternalPosition(marketID, outcomes[0], desired0, walletTruthSyncMarkPrice(engine, marketID, outcomes[0])) {
			if addQty >= realbotWalletTruthLogMinDelta {
				tui.LogEvent("[%s] 🧾 Wallet-truth sync restored missing %s inventory by %s (local %.4f → on-chain %.4f, split %.4f)", marketID, outcomes[0], formatShareQty(addQty), local0, onChain0, split0)
			}
			changed = true
		}
	}
	if local1 > desired1+1e-6 {
		trimQty := local1 - desired1
		if engine.SyncExternalPosition(marketID, outcomes[1], desired1, walletTruthSyncMarkPrice(engine, marketID, outcomes[1])) {
			if trimQty >= realbotWalletTruthLogMinDelta {
				tui.LogEvent("[%s] 🧾 Wallet-truth sync trimmed stale %s inventory by %s (local %.4f → on-chain %.4f, split %.4f)", marketID, outcomes[1], formatShareQty(trimQty), local1, onChain1, split1)
			}
			changed = true
		}
	} else if desired1 > local1+1e-6 {
		addQty := desired1 - local1
		if engine.SyncExternalPosition(marketID, outcomes[1], desired1, walletTruthSyncMarkPrice(engine, marketID, outcomes[1])) {
			if addQty >= realbotWalletTruthLogMinDelta {
				tui.LogEvent("[%s] 🧾 Wallet-truth sync restored missing %s inventory by %s (local %.4f → on-chain %.4f, split %.4f)", marketID, outcomes[1], formatShareQty(addQty), local1, onChain1, split1)
			}
			changed = true
		}
	}
	return changed, nil
}

func mergeBalancedPositionWSFirst(ctx context.Context, trader *trading.RealTrader, conditionID, token0, token1 string, requestedQty float64, numOutcomes int) (mergeQty, settled0, settled1 float64, txHash string, err error) {
	if requestedQty < minOnChainActionShares {
		return 0, 0, 0, "", fmt.Errorf("merge skipped: %.6f shares is below %.2f minimum", requestedQty, minOnChainActionShares)
	}

	settled0, settled1, err0, err1 := trader.QueryBalancedCTFBalances(ctx, token0, token1, requestedQty)
	if err0 != nil || err1 != nil {
		return 0, settled0, settled1, "", fmt.Errorf("on-chain settlement check failed (err0=%v err1=%v)", err0, err1)
	}

	mergeQty = math.Min(math.Min(settled0, settled1), requestedQty)
	if mergeQty < minOnChainActionShares {
		return 0, settled0, settled1, "", fmt.Errorf("merge skipped: settled balanced size %.6f is below %.2f minimum", mergeQty, minOnChainActionShares)
	}

	txHash, err = trader.MergeOnChain(ctx, conditionID, mergeQty, numOutcomes)
	if err != nil {
		return 0, settled0, settled1, txHash, err
	}
	return mergeQty, settled0, settled1, txHash, nil
}

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
	if mergeCoordinator != nil {
		pendingMergeQty = mergeCoordinator.pendingQty(id)
		if pendingMergeQty >= minOnChainActionShares {
			bal0, bal1 = subtractMergedPairBalances(bal0, bal1, pendingMergeQty)
			tui.LogEvent("[%s] 🔀 %s merge already pending for %.6f balanced shares; cleanup will focus only on excess inventory", id, reason, pendingMergeQty)
		}
	}

	minQty := math.Min(bal0, bal1)
	if minQty >= minOnChainActionShares {
		tui.LogEvent("[%s] 🔍 %s inventory snapshot (%s): %s=%.6f, %s=%.6f", id, reason, balanceSource, outcomes[0], bal0, outcomes[1], bal1)
		if launchBackgroundMerge(id, reason, outcomes, market.ConditionID, minQty, len(market.Tokens), trader, engine, splitInventory, tui, mergeCoordinator) {
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

		// Use the configured cleanup floor from settings/.env so sell cleanup behavior
		// stays aligned with runtime execution controls instead of a hidden dump price.
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
	if pendingVerifyQty := mergeCoordinator.pendingQty(id); pendingVerifyQty >= minOnChainActionShares {
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

func handleRestFallbackWithDepth(ctx context.Context, id string, staleTime time.Duration, tokenMap map[string]string, bids, asks, displayBids, displayAsks map[string]float64, fullBids, fullAsks map[string][]paper.MarketLevel, quoteState map[string]realbotQuoteState, polyTracker *paper.DirectionalSignalTracker, engine *paper.Engine, restClient *api.RestClient, tui *paper.TUI, logRecovery bool) bool {
	success := false
	staleSeconds := int(staleTime.Seconds())
	restErrors := 0
	restEmpty := 0
	var lastErr error
	outcomes := make([]string, 0, len(tokenMap))
	for _, outcome := range tokenMap {
		outcomes = append(outcomes, outcome)
	}
	for tokenID, outcome := range tokenMap {
		start := time.Now()
		// Use a short 2s timeout for fallback to prevent freezing the main loop when internet is down
		reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		book, err := restClient.GetOrderBook(reqCtx, tokenID)
		latency := time.Since(start)
		cancel()

		// Update TUI with real REST latency
		tui.UpdateRestLatency(latency)

		if err != nil {
			restErrors++
			lastErr = fmt.Errorf("fetching %s book after %s: %w", outcome, latency.Round(time.Millisecond), err)
			// If one request fails (likely due to no internet), break immediately to prevent further blocking
			break
		}

		// REST is authoritative state. If both sides are empty, clear stale local quotes.
		updatedAt := realbotQuoteTimestampOrNow(book.Timestamp)
		now := time.Now()
		if realbotShouldSkipStaleQuoteUpdate(quoteState, outcome, updatedAt, bids[outcome], asks[outcome]) {
			success = true
			if state, ok := quoteState[outcome]; ok {
				state.UpdatedAt = now
				quoteState[outcome] = state
			}
			continue
		}
		updatedAt = now
		if len(book.Bids) == 0 && len(book.Asks) == 0 {
			restEmpty++
			bids[outcome] = 0
			asks[outcome] = 0
			fullBids[outcome] = nil
			fullAsks[outcome] = nil
			quoteState[outcome] = realbotQuoteState{UpdatedAt: updatedAt, Source: "rest"}
			success = true
			continue
		}

		bid, ask := 0.0, 0.0
		for _, b := range book.Bids {
			p, _ := strconv.ParseFloat(b.Price, 64)
			if p > 0 && p <= 1.0 && p > bid {
				bid = p
			}
		}
		for _, a := range book.Asks {
			p, _ := strconv.ParseFloat(a.Price, 64)
			if p > 0 && p <= 1.0 && (ask == 0 || p < ask) {
				ask = p
			}
		}

		// Reject crossed/wide books from REST.
		if bid > 0 && ask > 0 && !realbotHasSaneTopOfBook(bid, ask) {
			// Clear invalid book state.
			bids[outcome] = 0
			asks[outcome] = 0
			fullBids[outcome] = nil
			fullAsks[outcome] = nil
			quoteState[outcome] = realbotQuoteState{UpdatedAt: updatedAt, Source: "rest"}
			success = true // Important: ensure UI updates to 0 (--.-)
			continue
		}

		// REST is absolute state. If it's missing a side, that side is 0.
		bids[outcome] = bid
		asks[outcome] = ask
		success = true

		if bid > 0 && ask > 0 {
			mid := (bid + ask) / 2
			engine.UpdateMarketData(id, outcome, mid, bid, ask)
			polyTracker.Record(outcome, bid, ask, updatedAt)
		}
		// ALWAYS update full depth (liquidity) if newer, as REST is our primary source
		// for recovering from stale or dropped WS states.
		fullBids[outcome] = mkt.LevelsToPriceDepth(book.Bids, true)
		fullAsks[outcome] = mkt.LevelsToPriceDepth(book.Asks, false)
		quoteState[outcome] = realbotQuoteState{UpdatedAt: updatedAt, Source: "rest"}
	}
	realbotSyncDisplayQuotes(outcomes, bids, asks, displayBids, displayAsks, true)
	if success && realbotShouldClearLocalPairQuotes(outcomes, bids, asks) {
		for _, outcome := range tokenMap {
			bids[outcome] = 0
			asks[outcome] = 0
			fullBids[outcome] = nil
			fullAsks[outcome] = nil
			quoteState[outcome] = realbotQuoteState{UpdatedAt: time.Now(), Source: "rest"}
		}
	}
	if success {
		if logRecovery && staleSeconds >= 10 {
			tui.LogEvent("[%s] ✅ REST recovered after %ds", id, staleSeconds)
		}
	} else if restErrors > 0 {
		if staleSeconds%10 == 0 || staleSeconds == 10 {
			tui.LogEvent("[%s] ❌ REST fallback failed after %ds: %v", id, staleSeconds, lastErr)
		}
	} else if restEmpty == len(tokenMap) && len(tokenMap) > 0 {
		if staleSeconds%10 == 0 || staleSeconds == 10 {
			tui.LogEvent("[%s] 📭 REST returned empty books after %ds", id, staleSeconds)
		}
	}
	return success
}

func checkRedemption(ctx context.Context, id, conditionID string, outcomes []string, marketEndTime time.Time, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI, resCache *api.ResolutionCache) {
	if err := refreshWalletTruthForRedemption(ctx, id, conditionID, trader, engine, tui); err != nil {
		if !realbotHasEnginePositionsForMarket(engine, id) {
			return
		}
		tui.LogEvent("[%s] ⚠️ Initial redemption wallet-truth refresh failed: %v", id, err)
	} else {
		positions, refreshErr := realbotWalletTruthPositionsForRedemption(ctx, id, conditionID, trader, engine)
		if refreshErr == nil && len(positions) == 0 {
			return
		}
	}

	numOutcomes := len(outcomes)

	wsResCh := make(chan struct{}, 1)
	if globalResWatcher != nil {
		globalResWatcher.RegisterCallback(func(eventCondID string) {
			if strings.EqualFold(eventCondID, conditionID) {
				select {
				case wsResCh <- struct{}{}:
				default:
				}
			}
		})
	}

	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	checkRound := 0

	for {
		if checkRound > 0 {
			select {
			case <-ctx.Done():
				return
			case <-wsResCh:
				tui.LogEvent("[%s] ⚡ WebSocket: ConditionResolved event detected on-chain!", id)
			case <-ticker.C:
			}
		}
		checkRound++

		resolved := false
		winner := ""
		if resCache != nil {
			resCache.ForceRefresh(conditionID)
			status := resCache.GetResolution(ctx, conditionID, outcomes, marketEndTime)
			if status.Error != nil {
				tui.LogEvent("[%s] ⚠️ Resolution check failed: %v", id, status.Error)
			}
			resolved = status.Resolved
			winner = status.Winner
		}

		if numOutcomes == 0 || winner == "" {
			info, err := trader.GetMarketInfo(ctx, conditionID)
			if err != nil {
				if !resolved {
					tui.LogEvent("[%s] ⚠️ Resolution check failed: %v", id, err)
					continue
				}
			} else {
				if len(info.Tokens) > numOutcomes {
					numOutcomes = len(info.Tokens)
				}
				for _, token := range info.Tokens {
					if token.Winner {
						winner = token.Outcome
						break
					}
				}
			}
		}

		if winner != "" {
			walletTruthWinningShares := 0.0
			missingCostBasis := []string(nil)
			if positions, positionsErr := realbotWalletTruthPositionsForRedemption(ctx, id, conditionID, trader, engine); positionsErr != nil {
				tui.LogEvent("[%s] ⚠️ Wallet-truth refresh before resolution settlement failed: %v", id, positionsErr)
			} else {
				walletTruthWinningShares = realbotWinningOnChainShares(positions, winner)
				tui.SetWalletTruthPositions(id, positions)
				if adjusted, missing := realbotSyncEngineToWalletTruthForResolution(engine, id, positions); adjusted > 0 {
					tui.LogEvent("[%s] 🔄 Synced local resolution inventory to on-chain balances (%d outcomes adjusted)", id, adjusted)
				} else if len(missing) > 0 {
					missingCostBasis = append(missingCostBasis, missing...)
					tui.LogEvent("[%s] ⚠️ Resolution inventory drift detected with no local cost basis for: %s", id, strings.Join(missingCostBasis, ", "))
				}
			}
			result := engine.RedeemWithDetails(id, winner)
			if err := refreshWalletTruthForRedemption(ctx, id, conditionID, trader, engine, tui); err != nil {
				tui.LogEvent("[%s] ⚠️ Wallet-truth refresh after winner update failed: %v", id, err)
			}
			tui.UpdateWalletTruthResolution(id, true, winner)
			if result.WinningShares > 0 || result.LosingShares > 0 || result.TotalPayout > 0 || result.TotalPnL != 0 || walletTruthWinningShares > 0.000001 {
				pnlSign := "+"
				pnlEmoji := "💰"
				if result.TotalPnL < 0 {
					pnlSign = ""
					pnlEmoji = "💸"
				}
				if result.WinningShares > 0 || result.LosingShares > 0 || result.TotalPayout > 0 || result.TotalPnL != 0 {
					tui.LogEvent("[%s] %s RESOLVED: %s won | PnL: %s$%.2f", id, pnlEmoji, winner, pnlSign, result.TotalPnL)
				} else {
					tui.LogEvent("[%s] ⏳ RESOLVED: %s won | wallet-truth redeemable %s shares (cost basis unavailable: %s)", id, winner, formatShareQty(walletTruthWinningShares), strings.Join(missingCostBasis, ", "))
				}

				// Record loss for safety limits
				if result.TotalPnL < 0 && trader != nil {
					trader.RecordLoss(-result.TotalPnL)
				}

				tui.LogEvent("[%s] ⏳ Starting forced on-chain redemption retry loop (every %s)...", id, realbotRedeemRetryInterval)
				launchRealbotRedeemRetryLoop(id, conditionID, winner, numOutcomes, trader, engine, tui)
			} else {
				tui.LogEvent("[%s] 📭 Market resolved: %s (no positions)", id, winner)
			}
			return
		}

		if resolved {
			if err := refreshWalletTruthForRedemption(ctx, id, conditionID, trader, engine, tui); err != nil {
				tui.LogEvent("[%s] ⚠️ Wallet-truth refresh during resolved-pending state failed: %v", id, err)
			}
			tui.UpdateWalletTruthResolution(id, true, "")
			tui.LogEvent("[%s] ⏳ Market resolved on-chain, winner still pending...", id)
			continue
		}

		// Update TUI to show positions are still unresolved
		if err := refreshWalletTruthForRedemption(ctx, id, conditionID, trader, engine, tui); err != nil {
			tui.LogEvent("[%s] ⚠️ Wallet-truth refresh during pending resolution failed: %v", id, err)
		}
		tui.UpdateWalletTruthResolution(id, false, "")
		tui.LogEvent("[%s] ⏳ Resolution pending...", id)
	}

}

type realbotCopytradeTarget struct {
	Raw    string
	Wallet string
	Label  string
}

type realbotCopytradeState struct {
	startedAt            time.Time
	lastError            string
	managed              map[string]bool
	targetShares         map[string]float64
	targetSeen           map[string]bool
	lastTargetPoll       map[string]time.Time
	pendingSellTarget    map[string]float64
	pendingSellPoll      map[string]time.Time
	lastTradeFetch       time.Time
	tradesSeeded         bool
	seenTradeKeys        map[string]time.Time
	seenTradeKeysCount   map[string]int
	retryTrades          []api.PublicTrade
	observedBuySizeSum   map[string]float64
	observedBuySizeCount map[string]int
	lastLogAt            map[string]time.Time
	lastLogMsg           map[string]string
}

type realbotCopytradePoller struct {
	wallet                 string
	conditionIDs           []string
	mu                     sync.Mutex
	lastPoll               time.Time
	lastPositionsRefreshAt time.Time
	fetching               bool
	waitCh                 chan struct{}
	lastPollStartedAt      time.Time
	lastSnapshot           api.PublicActivitySnapshot
	rateLimitUntil         time.Time
	rateLimitStreak        int
	pendingWatcher         *api.PolymarketPendingWatcher
	minedWatcher           *api.PolymarketMinedWatcher
}

type realbotCopytradeWatcherSet struct {
	wallet         string
	chainWSURL     string
	pendingWSURL   string
	cancel         context.CancelFunc
	pendingWatcher *api.PolymarketPendingWatcher
	minedWatcher   *api.PolymarketMinedWatcher
}

func realbotCopytradeTradeFetchTimeout(pollEvery time.Duration) time.Duration {
	if pollEvery < 250*time.Millisecond {
		pollEvery = 250 * time.Millisecond
	}
	timeout := pollEvery * 4
	if timeout < 1500*time.Millisecond {
		timeout = 1500 * time.Millisecond
	}
	if timeout > 2500*time.Millisecond {
		timeout = 2500 * time.Millisecond
	}
	return timeout
}

func realbotCopytradePositionFetchTimeout(pollEvery time.Duration) time.Duration {
	timeout := realbotCopytradeTradeFetchTimeout(pollEvery) * 2
	if timeout < 4*time.Second {
		timeout = 4 * time.Second
	}
	if timeout > 8*time.Second {
		timeout = 8 * time.Second
	}
	return timeout
}

func realbotCopytradeCanReusePositions(lastRefresh time.Time, pollEvery time.Duration) bool {
	if lastRefresh.IsZero() {
		return false
	}
	maxAge := pollEvery * 3
	if maxAge < 5*time.Second {
		maxAge = 5 * time.Second
	}
	if maxAge > 15*time.Second {
		maxAge = 15 * time.Second
	}
	return time.Since(lastRefresh) <= maxAge
}

func newRealbotCopytradeState() *realbotCopytradeState {
	return &realbotCopytradeState{
		startedAt:            time.Now(),
		managed:              make(map[string]bool),
		targetShares:         make(map[string]float64),
		targetSeen:           make(map[string]bool),
		lastTargetPoll:       make(map[string]time.Time),
		pendingSellTarget:    make(map[string]float64),
		pendingSellPoll:      make(map[string]time.Time),
		seenTradeKeys:        make(map[string]time.Time),
		seenTradeKeysCount:   make(map[string]int),
		observedBuySizeSum:   make(map[string]float64),
		observedBuySizeCount: make(map[string]int),
		lastLogAt:            make(map[string]time.Time),
		lastLogMsg:           make(map[string]string),
	}
}

func newRealbotCopytradePoller(wallet string, conditionIDs []string) *realbotCopytradePoller {
	wallet = strings.TrimSpace(wallet)
	if wallet == "" {
		return nil
	}
	return &realbotCopytradePoller{
		wallet:       wallet,
		conditionIDs: normalizeRealbotCopytradeConditionIDs(conditionIDs),
	}
}

func (w *realbotCopytradeWatcherSet) stop() {
	if w == nil || w.cancel == nil {
		return
	}
	w.cancel()
	w.cancel = nil
}

func (w *realbotCopytradeWatcherSet) primeTrackedMarkets(markets []*api.Market) {
	if w == nil {
		return
	}
	if w.minedWatcher != nil {
		w.minedWatcher.PrimeTrackedMarkets(markets)
	}
	if w.pendingWatcher != nil {
		w.pendingWatcher.PrimeTrackedMarkets(markets)
	}
}

func (w *realbotCopytradeWatcherSet) attach(poller *realbotCopytradePoller) {
	if w == nil || poller == nil {
		return
	}
	poller.pendingWatcher = w.pendingWatcher
	poller.minedWatcher = w.minedWatcher
}

func ensureRealbotCopytradeWatcherSet(parentCtx context.Context, current *realbotCopytradeWatcherSet, wallet, chainWSURL, pendingWSURL string, polygonClient *api.PolygonClient, restClient *api.RestClient, trackedMarkets []*api.Market, logf func(string, ...interface{})) *realbotCopytradeWatcherSet {
	wallet = strings.TrimSpace(wallet)
	chainWSURL = strings.TrimSpace(chainWSURL)
	pendingWSURL = strings.TrimSpace(pendingWSURL)
	minedWatcherMode := api.NormalizeCopytradeMinedWatcherMode(os.Getenv("COPYTRADE_MINED_WATCHER_MODE"))
	if wallet == "" {
		if current != nil {
			current.stop()
		}
		return nil
	}

	if current != nil &&
		strings.EqualFold(current.wallet, wallet) &&
		current.chainWSURL == chainWSURL &&
		current.pendingWSURL == pendingWSURL {
		current.primeTrackedMarkets(trackedMarkets)
		return current
	}

	if current != nil {
		current.stop()
	}

	watcherCtx, cancel := context.WithCancel(parentCtx)
	next := &realbotCopytradeWatcherSet{
		wallet:       wallet,
		chainWSURL:   chainWSURL,
		pendingWSURL: pendingWSURL,
		cancel:       cancel,
	}

	pendingSupported := api.SupportsPolymarketPendingWSURL(pendingWSURL)
	if api.ShouldEnableCopytradeMinedWatcher(minedWatcherMode, pendingWSURL) {
		if watcher := api.NewPolymarketMinedWatcher(chainWSURL, polygonClient, restClient, wallet); watcher != nil {
			watcher.PrimeTrackedMarkets(trackedMarkets)
			watcher.Start(watcherCtx, logf)
			next.minedWatcher = watcher
			logf("⛓️ Copytrade onchain watcher enabled for %s", wallet)
		}
	} else {
		switch {
		case minedWatcherMode == api.CopytradeMinedWatcherModeOff:
			logf("ℹ️ Copytrade onchain watcher disabled by COPYTRADE_MINED_WATCHER_MODE=off")
		case pendingSupported:
			logf("ℹ️ Copytrade onchain watcher skipped: pending watcher available, reducing Polygon RPC usage")
		default:
			logf("ℹ️ Copytrade onchain watcher skipped")
		}
	}
	if pendingSupported {
		if watcher := api.NewPolymarketPendingWatcher(pendingWSURL, restClient, polygonClient, wallet); watcher != nil {
			watcher.PrimeTrackedMarkets(trackedMarkets)
			watcher.Start(watcherCtx, logf)
			next.pendingWatcher = watcher
			logf("🛰️ Copytrade mempool watcher enabled for %s", wallet)
		}
	} else if pendingWSURL != "" {
		logf("ℹ️ Copytrade mempool watcher skipped: pending filtering requires Alchemy; using standard Polygon WS for onchain watcher only")
	}

	if next.pendingWatcher == nil && next.minedWatcher == nil {
		next.stop()
		return nil
	}
	return next
}

type realbotCopytradeMarketSnapshot struct {
	Trades            []api.PublicTrade
	Positions         []api.Position
	TradesErr         error
	PositionsErr      error
	PollStartedAt     time.Time
	PolledAt          time.Time
	PositionsPolledAt time.Time
}

func realbotCopytradeShouldLog(state *realbotCopytradeState, key, msg string, interval time.Duration) bool {
	if state == nil {
		return true
	}
	if interval <= 0 {
		interval = 10 * time.Second
	}
	lastMsg := state.lastLogMsg[key]
	lastAt := state.lastLogAt[key]
	if msg == lastMsg && !lastAt.IsZero() && time.Since(lastAt) < interval {
		return false
	}
	state.lastLogMsg[key] = msg
	state.lastLogAt[key] = time.Now()
	return true
}

func realbotResolveCopytradeTarget(ctx context.Context, restClient *api.RestClient, liveCfg paper.TUISettings) (realbotCopytradeTarget, error) {
	raw := strings.TrimSpace(liveCfg.CopytradeTarget)
	if raw == "" {
		return realbotCopytradeTarget{}, fmt.Errorf("copytrade target is empty")
	}
	wallet, profile, err := restClient.ResolvePublicProfileTarget(ctx, raw)
	if err != nil {
		return realbotCopytradeTarget{}, err
	}

	label := wallet
	if profile != nil {
		switch {
		case strings.TrimSpace(profile.Name) != "":
			label = profile.Name
		case strings.TrimSpace(profile.Pseudonym) != "":
			label = profile.Pseudonym
		case strings.TrimSpace(profile.Referral) != "":
			label = "@" + strings.TrimPrefix(profile.Referral, "@")
		}
	}
	return realbotCopytradeTarget{
		Raw:    raw,
		Wallet: wallet,
		Label:  label,
	}, nil
}

func realbotCopytradeLabelFromHint(slug, title string) string {
	if slug = core.SanitizeString(slug); slug != "" {
		parts := strings.Split(slug, "-")
		if len(parts) > 0 && strings.TrimSpace(parts[0]) != "" {
			return strings.ToUpper(strings.TrimSpace(parts[0]))
		}
		return strings.ToUpper(slug)
	}
	title = core.SanitizeString(title)
	if title == "" {
		return "COPY"
	}
	title = strings.ToUpper(title)
	if len(title) > 12 {
		title = title[:12]
	}
	return title
}

func parseCopytradeEndTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return parsed
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed
	}
	return time.Time{}
}

func buildCopytradeMarketFromPosition(pos api.Position) *api.Market {
	if pos.ConditionID == "" || pos.TokenID == "" || pos.Outcome == "" {
		return nil
	}
	market := &api.Market{
		ConditionID: pos.ConditionID,
		Slug:        core.SanitizeString(pos.Slug),
		EndTime:     parseCopytradeEndTime(pos.EndDate),
		Tokens: []api.Token{
			{TokenID: pos.TokenID, Outcome: core.SanitizeString(pos.Outcome)},
		},
	}
	if pos.OppositeAsset != "" && pos.OppositeOutcome != "" {
		market.Tokens = append(market.Tokens, api.Token{
			TokenID: pos.OppositeAsset,
			Outcome: core.SanitizeString(pos.OppositeOutcome),
		})
	}
	return market
}

func buildCopytradeMarketFromTrade(ctx context.Context, restClient *api.RestClient, trade api.PublicTrade) *api.Market {
	if restClient == nil || trade.ConditionID == "" {
		return nil
	}
	market, err := restClient.GetMarket(ctx, trade.ConditionID)
	if err == nil && market != nil {
		return market
	}
	return nil
}

func realbotFindCopytradeMarkets(ctx context.Context, restClient *api.RestClient, wallet string, maxMarkets int) (map[string]*api.Market, error) {
	if restClient == nil {
		return nil, fmt.Errorf("rest client is nil")
	}
	if maxMarkets <= 0 {
		maxMarkets = 4
	}

	found := make(map[string]*api.Market)
	seen := make(map[string]struct{})

	addMarket := func(label string, market *api.Market) bool {
		if market == nil || market.ConditionID == "" {
			return false
		}
		if _, ok := seen[market.ConditionID]; ok {
			return false
		}
		if !market.EndTime.IsZero() {
			if time.Now().After(market.EndTime) || time.Until(market.EndTime) < 30*time.Second {
				return false
			}
		}
		if label == "" {
			label = realbotCopytradeLabelFromHint(market.Slug, "")
		}
		if _, exists := found[label]; exists {
			fingerprint := strings.TrimPrefix(strings.TrimPrefix(market.ConditionID, "0x"), "0X")
			if len(fingerprint) > 6 {
				fingerprint = fingerprint[:6]
			}
			if fingerprint == "" {
				fingerprint = "mkt"
			}
			label = label + "-" + strings.ToUpper(fingerprint)
		}
		seen[market.ConditionID] = struct{}{}
		found[label] = market
		return len(found) >= maxMarkets
	}

	positions, posErr := restClient.GetPublicPositions(ctx, wallet, nil, 0.01, maxMarkets*8)
	if posErr == nil {
		for _, pos := range positions {
			if pos.Size <= 0.01 {
				continue
			}
			if addMarket(realbotCopytradeLabelFromHint(pos.Slug, pos.Title), buildCopytradeMarketFromPosition(pos)) {
				return found, nil
			}
		}
	}

	trades, tradeErr := restClient.GetPublicTrades(ctx, wallet, nil, maxMarkets*8)
	if tradeErr == nil {
		sort.Slice(trades, func(i, j int) bool {
			return trades[i].Timestamp > trades[j].Timestamp
		})
		for _, trade := range trades {
			if addMarket(realbotCopytradeLabelFromHint(trade.Slug, trade.Title), buildCopytradeMarketFromTrade(ctx, restClient, trade)) {
				return found, nil
			}
		}
	}

	if len(found) == 0 {
		switch {
		case posErr != nil && tradeErr != nil:
			return nil, fmt.Errorf("positions: %v | trades: %v", posErr, tradeErr)
		case posErr != nil:
			return nil, posErr
		case tradeErr != nil:
			return nil, tradeErr
		}
	}
	return found, nil
}

func realbotCopytradeHeldOutcomes(positions []api.Position) map[string]api.Position {
	held := make(map[string]api.Position, len(positions))
	for _, pos := range positions {
		outcome := core.SanitizeString(pos.Outcome)
		if outcome == "" || pos.Size <= 0.01 {
			continue
		}
		held[outcome] = pos
	}
	return held
}

func realbotCopytradeTargetShares(positions []api.Position) map[string]float64 {
	return realbotCopytradeTargetSharesForCondition(positions, "")
}

func realbotCopytradeSharesByCondition(positions []api.Position) map[string]map[string]float64 {
	sharesByCondition := make(map[string]map[string]float64)
	for _, pos := range positions {
		conditionID := strings.TrimSpace(pos.ConditionID)
		outcome := core.SanitizeString(pos.Outcome)
		if conditionID == "" || outcome == "" || pos.Size <= 0.01 {
			continue
		}
		outcomeShares := sharesByCondition[conditionID]
		if outcomeShares == nil {
			outcomeShares = make(map[string]float64)
			sharesByCondition[conditionID] = outcomeShares
		}
		outcomeShares[outcome] += pos.Size
	}
	return sharesByCondition
}

func realbotCopytradeTargetSharesForCondition(positions []api.Position, conditionID string) map[string]float64 {
	shares := make(map[string]float64, len(positions))
	conditionID = strings.TrimSpace(conditionID)
	for _, pos := range positions {
		if conditionID != "" && !strings.EqualFold(strings.TrimSpace(pos.ConditionID), conditionID) {
			continue
		}
		outcome := core.SanitizeString(pos.Outcome)
		if outcome == "" || pos.Size <= 0.01 {
			continue
		}
		shares[outcome] += pos.Size
	}
	return shares
}

func realbotCopytradeHoldsBothOutcomes(targetShares map[string]float64) bool {
	held := 0
	for _, qty := range targetShares {
		if qty > 0.01 {
			held++
			if held >= 2 {
				return true
			}
		}
	}
	return false
}

func realbotCopytradeHasAmbiguousPositionExit(positions []api.Position, conditionID string) bool {
	conditionID = strings.TrimSpace(conditionID)
	for _, pos := range positions {
		if conditionID != "" && !strings.EqualFold(strings.TrimSpace(pos.ConditionID), conditionID) {
			continue
		}
		if pos.Size <= 0.01 {
			continue
		}
		if pos.Mergeable || pos.Redeemable {
			return true
		}
	}
	return false
}

func normalizeRealbotCopytradeConditionIDs(conditionIDs []string) []string {
	seen := make(map[string]struct{}, len(conditionIDs))
	normalized := make([]string, 0, len(conditionIDs))
	for _, conditionID := range conditionIDs {
		conditionID = strings.TrimSpace(conditionID)
		if conditionID == "" {
			continue
		}
		if _, exists := seen[conditionID]; exists {
			continue
		}
		seen[conditionID] = struct{}{}
		normalized = append(normalized, conditionID)
	}
	return normalized
}

func realbotCopytradeIsRateLimited(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "status 429") || strings.Contains(msg, "error code: 1015")
}

func realbotCopytradeRateLimitBackoff(streak int) time.Duration {
	if streak < 1 {
		return 0
	}
	backoff := time.Second
	for i := 1; i < streak; i++ {
		backoff *= 2
		if backoff >= 8*time.Second {
			return 8 * time.Second
		}
	}
	return backoff
}

func filterRealbotCopytradeTradesByCondition(trades []api.PublicTrade, conditionID string) []api.PublicTrade {
	if strings.TrimSpace(conditionID) == "" || len(trades) == 0 {
		return trades
	}
	filtered := make([]api.PublicTrade, 0, len(trades))
	for _, trade := range trades {
		if strings.EqualFold(strings.TrimSpace(trade.ConditionID), conditionID) {
			filtered = append(filtered, trade)
		}
	}
	return filtered
}

func filterRealbotCopytradePositionsByCondition(positions []api.Position, conditionID string) []api.Position {
	if strings.TrimSpace(conditionID) == "" || len(positions) == 0 {
		return positions
	}
	filtered := make([]api.Position, 0, len(positions))
	for _, pos := range positions {
		if strings.EqualFold(strings.TrimSpace(pos.ConditionID), conditionID) {
			filtered = append(filtered, pos)
		}
	}
	return filtered
}

func (p *realbotCopytradePoller) pendingSignalsForCondition(conditionID string, since time.Time) []api.PublicTrade {
	if p == nil || p.pendingWatcher == nil {
		return nil
	}
	signals := p.pendingWatcher.SignalsSince(conditionID, since)
	if len(signals) == 0 {
		return nil
	}
	trades := make([]api.PublicTrade, 0, len(signals))
	for _, sig := range signals {
		trades = append(trades, api.PublicTrade{
			ConditionID:     sig.ConditionID,
			Outcome:         sig.Outcome,
			Side:            sig.Side,
			Size:            sig.Size,
			Timestamp:       sig.ObservedAt.Unix(),
			TransactionHash: sig.TxHash,
			Source:          "mempool",
			SignalID:        sig.SignalID,
			Slug:            sig.Slug,
		})
	}
	return trades
}

func (p *realbotCopytradePoller) minedSignalsForCondition(conditionID string, since time.Time) []api.PublicTrade {
	if p == nil || p.minedWatcher == nil {
		return nil
	}
	signals := p.minedWatcher.SignalsSince(conditionID, since)
	if len(signals) == 0 {
		return nil
	}
	trades := make([]api.PublicTrade, 0, len(signals))
	for _, sig := range signals {
		trades = append(trades, api.PublicTrade{
			ConditionID:     sig.ConditionID,
			Outcome:         sig.Outcome,
			Side:            sig.Side,
			Size:            sig.Size,
			Timestamp:       sig.BlockTimestamp,
			TransactionHash: sig.TxHash,
			Source:          "onchain",
			SignalID:        sig.SignalID,
			Slug:            sig.Slug,
		})
	}
	return trades
}

func realbotCopytradeHasOnchainWatcher(p *realbotCopytradePoller) bool {
	return p != nil && ((p.pendingWatcher != nil && p.pendingWatcher.Enabled()) || (p.minedWatcher != nil && p.minedWatcher.Enabled()))
}

func realbotCopytradeHasPendingWatcher(p *realbotCopytradePoller) bool {
	return p != nil && p.pendingWatcher != nil && p.pendingWatcher.Enabled()
}

func realbotCopytradeShouldUsePublicActivityAPI(p *realbotCopytradePoller) bool {
	return !realbotCopytradeHasOnchainWatcher(p)
}

func (p *realbotCopytradePoller) cachedSnapshotForCondition(conditionID string) realbotCopytradeMarketSnapshot {
	if p == nil {
		return realbotCopytradeMarketSnapshot{}
	}
	return realbotCopytradeMarketSnapshot{
		Trades:            filterRealbotCopytradeTradesByCondition(p.lastSnapshot.Trades, conditionID),
		Positions:         filterRealbotCopytradePositionsByCondition(p.lastSnapshot.Positions, conditionID),
		TradesErr:         p.lastSnapshot.TradesErr,
		PositionsErr:      p.lastSnapshot.PositionsErr,
		PollStartedAt:     p.lastPollStartedAt,
		PolledAt:          p.lastPoll,
		PositionsPolledAt: p.lastPositionsRefreshAt,
	}
}

func (p *realbotCopytradePoller) snapshotForCondition(ctx context.Context, restClient *api.RestClient, pollEvery time.Duration, conditionID string) (realbotCopytradeMarketSnapshot, error) {
	if p == nil || restClient == nil {
		return realbotCopytradeMarketSnapshot{}, fmt.Errorf("copytrade poller unavailable")
	}
	if pollEvery <= 0 {
		pollEvery = 2 * time.Second
	}
	conditionID = strings.TrimSpace(conditionID)

	for {
		p.mu.Lock()
		if !p.lastPoll.IsZero() && time.Since(p.lastPoll) < pollEvery {
			snapshot := p.cachedSnapshotForCondition(conditionID)
			p.mu.Unlock()
			return snapshot, nil
		}
		if !p.rateLimitUntil.IsZero() && time.Now().Before(p.rateLimitUntil) && !p.lastPoll.IsZero() {
			snapshot := p.cachedSnapshotForCondition(conditionID)
			p.mu.Unlock()
			return snapshot, nil
		}
		if p.fetching {
			waitCh := p.waitCh
			p.mu.Unlock()
			select {
			case <-ctx.Done():
				return realbotCopytradeMarketSnapshot{}, ctx.Err()
			case <-waitCh:
				continue
			}
		}

		p.fetching = true
		p.waitCh = make(chan struct{})
		wallet := p.wallet
		conditionIDs := append([]string(nil), p.conditionIDs...)
		pollStartedAt := time.Now()
		p.mu.Unlock()

		tradeLimit := len(conditionIDs) * 64
		if tradeLimit < 128 {
			tradeLimit = 128
		}
		if tradeLimit > 1000 {
			tradeLimit = 1000
		}
		positionLimit := len(conditionIDs) * 8
		if positionLimit < 16 {
			positionLimit = 16
		}
		if positionLimit > 500 {
			positionLimit = 500
		}
		tradeTimeout := realbotCopytradeTradeFetchTimeout(pollEvery)
		positionTimeout := realbotCopytradePositionFetchTimeout(pollEvery)
		cachedPositions := append([]api.Position(nil), p.lastSnapshot.Positions...)
		cachedPositionsValid := p.lastSnapshot.PositionsErr == nil && realbotCopytradeCanReusePositions(p.lastPositionsRefreshAt, pollEvery)
		snapshot := restClient.GetPublicActivitySnapshotWithFallback(
			ctx,
			wallet,
			conditionIDs,
			tradeLimit,
			0.01,
			positionLimit,
			cachedPositions,
			cachedPositionsValid,
			tradeTimeout,
			positionTimeout,
		)
		now := time.Now()

		p.mu.Lock()
		if snapshot.TradesErr == nil {
			p.lastSnapshot.Trades = snapshot.Trades
			p.lastSnapshot.TradesErr = nil
		} else {
			p.lastSnapshot.TradesErr = snapshot.TradesErr
		}
		if snapshot.PositionsErr == nil {
			p.lastSnapshot.Positions = snapshot.Positions
			p.lastSnapshot.PositionsErr = nil
			if !snapshot.PositionsCached {
				p.lastPositionsRefreshAt = now
			}
		} else {
			p.lastSnapshot.PositionsErr = snapshot.PositionsErr
		}
		if realbotCopytradeIsRateLimited(snapshot.TradesErr) {
			p.rateLimitStreak++
			p.rateLimitUntil = now.Add(realbotCopytradeRateLimitBackoff(p.rateLimitStreak))
		} else {
			p.rateLimitStreak = 0
			p.rateLimitUntil = time.Time{}
		}
		p.lastPollStartedAt = pollStartedAt
		p.lastPoll = now
		waitCh := p.waitCh
		p.fetching = false
		p.waitCh = nil
		filtered := p.cachedSnapshotForCondition(conditionID)
		p.mu.Unlock()
		close(waitCh)

		return filtered, nil
	}
}

func realbotCopytradePositionSyncTrades(state *realbotCopytradeState, conditionID string, outcomes []string, positions []api.Position, pollTime time.Time, freshTrades []api.PublicTrade, sizingMode string) ([]api.PublicTrade, map[string]float64) {
	if state == nil || pollTime.IsZero() {
		return nil, nil
	}
	if !strings.EqualFold(sizingMode, core.CopytradeSizingModePercent) {
		return nil, nil
	}

	targetShares := realbotCopytradeTargetSharesForCondition(positions, conditionID)
	holdsBoth := realbotCopytradeHoldsBothOutcomes(targetShares)
	ambiguousExit := realbotCopytradeHasAmbiguousPositionExit(positions, conditionID)

	freshBuySize := make(map[string]float64)
	freshSell := make(map[string]bool)
	for _, trade := range freshTrades {
		outcome := core.SanitizeString(trade.Outcome)
		if outcome == "" {
			continue
		}
		switch strings.ToUpper(strings.TrimSpace(trade.Side)) {
		case "BUY":
			freshBuySize[outcome] += math.Max(0, trade.Size)
		case "SELL":
			freshSell[outcome] = true
		}
	}

	relevantOutcomes := make(map[string]struct{})
	for _, outcome := range outcomes {
		outcome = core.SanitizeString(outcome)
		if outcome != "" {
			relevantOutcomes[outcome] = struct{}{}
		}
	}
	for outcome := range targetShares {
		relevantOutcomes[outcome] = struct{}{}
	}
	for outcome := range state.targetSeen {
		if outcome != "" {
			relevantOutcomes[outcome] = struct{}{}
		}
	}
	if len(relevantOutcomes) == 0 {
		return nil, nil
	}

	targetDeltas := make(map[string]float64)
	syncTrades := make([]api.PublicTrade, 0)
	for outcome := range relevantOutcomes {
		targetQty := targetShares[outcome]
		delta, ready, pending := realbotCopytradeTargetDelta(state, outcome, targetQty, pollTime)
		if !ready || pending || math.Abs(delta) <= 0.01 {
			continue
		}
		targetDeltas[outcome] = delta
		switch {
		case delta > 0:
			if remaining := delta - freshBuySize[outcome]; remaining > 0.01 {
				syncTrades = append(syncTrades, realbotEstimatedPositionBuySignals(state, strings.TrimSpace(conditionID), outcome, remaining, sizingMode)...)
			}
		case delta < 0 && !freshSell[outcome] && !holdsBoth && !ambiguousExit:
			syncTrades = append(syncTrades, api.PublicTrade{
				ConditionID: strings.TrimSpace(conditionID),
				Outcome:     outcome,
				Side:        "SELL",
				Size:        -delta,
				Timestamp:   pollTime.Unix(),
				Source:      "position",
			})
		}
	}

	sort.Slice(syncTrades, func(i, j int) bool {
		if syncTrades[i].Outcome == syncTrades[j].Outcome {
			return syncTrades[i].Side < syncTrades[j].Side
		}
		return syncTrades[i].Outcome < syncTrades[j].Outcome
	})

	return syncTrades, targetDeltas
}

func realbotClearPendingCopytradeSell(state *realbotCopytradeState, outcome string) {
	if state == nil || outcome == "" {
		return
	}
	delete(state.pendingSellTarget, outcome)
	delete(state.pendingSellPoll, outcome)
}

func realbotCopytradeTargetDelta(state *realbotCopytradeState, outcome string, targetQty float64, pollTime time.Time) (float64, bool, bool) {
	if state == nil {
		return 0, false, false
	}
	outcome = core.SanitizeString(outcome)
	if outcome == "" {
		return 0, false, false
	}
	if !state.targetSeen[outcome] {
		state.targetSeen[outcome] = true
		state.targetShares[outcome] = targetQty
		state.lastTargetPoll[outcome] = pollTime
		realbotClearPendingCopytradeSell(state, outcome)
		if state.tradesSeeded {
			return targetQty, true, false
		}
		return 0, false, false
	}
	if lastPoll := state.lastTargetPoll[outcome]; !lastPoll.IsZero() && lastPoll.Equal(pollTime) {
		return 0, false, false
	}
	state.lastTargetPoll[outcome] = pollTime

	prev := state.targetShares[outcome]
	if targetQty > prev+0.01 {
		state.targetShares[outcome] = targetQty
		realbotClearPendingCopytradeSell(state, outcome)
		return targetQty - prev, true, false
	}
	if targetQty >= prev-0.01 {
		state.targetShares[outcome] = targetQty
		realbotClearPendingCopytradeSell(state, outcome)
		return 0, false, false
	}
	if _, waiting := state.pendingSellPoll[outcome]; waiting {
		state.targetShares[outcome] = targetQty
		realbotClearPendingCopytradeSell(state, outcome)
		return targetQty - prev, true, false
	}
	state.pendingSellTarget[outcome] = targetQty
	state.pendingSellPoll[outcome] = pollTime
	return 0, false, true
}

func realbotCopytradeTradeKey(trade api.PublicTrade) string {
	if signalID := strings.TrimSpace(trade.SignalID); signalID != "" {
		return "signal|" + signalID
	}
	txHash := strings.TrimSpace(trade.TransactionHash)
	if txHash != "" {
		return fmt.Sprintf("%s|%s|%s|%.6f|%.6f|%s|%d|%s", strings.TrimSpace(trade.ConditionID), core.SanitizeString(trade.Outcome), strings.ToUpper(strings.TrimSpace(trade.Side)), trade.Size, trade.Price, strings.TrimSpace(trade.Asset), trade.Timestamp, txHash)
	}
	return fmt.Sprintf("%s|%d|%s|%s|%.6f", strings.TrimSpace(trade.ConditionID), trade.Timestamp, core.SanitizeString(trade.Outcome), strings.ToUpper(strings.TrimSpace(trade.Side)), trade.Size)
}

func realbotCopytradeSignalSource(trade api.PublicTrade) string {
	label := strings.TrimSpace(trade.Source)
	if label != "" {
		return label
	}
	if trade.Timestamp == 0 {
		return "position"
	}
	return "trade"
}

func realbotCopytradeSignalSummary(trade api.PublicTrade) string {
	side := strings.ToUpper(strings.TrimSpace(trade.Side))
	if side == "" {
		side = "?"
	}
	outcome := core.SanitizeString(trade.Outcome)
	if outcome == "" {
		outcome = "?"
	}
	parts := []string{
		fmt.Sprintf("%s %s", side, outcome),
		fmt.Sprintf("master=%s", formatShareQty(math.Max(0, trade.Size))),
		fmt.Sprintf("source=%s", realbotCopytradeSignalSource(trade)),
	}
	if txHash := realbotShortTxHash(trade.TransactionHash); txHash != "" {
		parts = append(parts, "tx="+txHash)
	}
	return strings.Join(parts, " | ")
}

func realbotLogCopytradeSignalResult(tui *paper.TUI, marketID string, trade api.PublicTrade, status, result string) {
	if tui == nil {
		return
	}
	tui.LogEvent("[%s] %s Copytrade signal %s -> %s", marketID, status, realbotCopytradeSignalSummary(trade), result)
}

func realbotNormalizeCopytradeSignalID(trade api.PublicTrade) string {
	if signalID := strings.TrimSpace(trade.SignalID); signalID != "" {
		return signalID
	}
	txHash := strings.TrimSpace(trade.TransactionHash)
	asset := strings.TrimSpace(trade.Asset)
	side := strings.ToUpper(strings.TrimSpace(trade.Side))
	if txHash == "" || asset == "" || side == "" {
		return ""
	}
	return fmt.Sprintf("%s:%s:%s", txHash, asset, side)
}

func realbotPrepareCopytradeTrades(trades []api.PublicTrade, source string) []api.PublicTrade {
	if len(trades) == 0 {
		return nil
	}
	prepared := make([]api.PublicTrade, 0, len(trades))
	for _, trade := range trades {
		normalized := trade
		if strings.TrimSpace(normalized.Source) == "" && strings.TrimSpace(source) != "" {
			normalized.Source = source
		}
		normalized.SignalID = realbotNormalizeCopytradeSignalID(normalized)
		prepared = append(prepared, normalized)
	}
	return prepared
}

func realbotMergeCopytradeTrades(groups ...[]api.PublicTrade) []api.PublicTrade {
	total := 0
	for _, group := range groups {
		total += len(group)
	}
	if total == 0 {
		return nil
	}

	merged := make([]api.PublicTrade, 0, total)
	seenSignals := make(map[string]string, total)
	for _, group := range groups {
		for _, trade := range group {
			key := realbotNormalizeCopytradeSignalID(trade)
			if key != "" {
				source := strings.TrimSpace(trade.Source)
				if seenSource, exists := seenSignals[key]; exists {
					if seenSource != source {
						continue
					}
				} else {
					seenSignals[key] = source
				}
			}
			merged = append(merged, trade)
		}
	}
	return merged
}
func realbotObserveCopytradeBuySignal(state *realbotCopytradeState, trade api.PublicTrade) {
	if state == nil {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(trade.Side), "BUY") {
		return
	}
	if trade.Size <= 0.01 {
		return
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(trade.Source)), "position") {
		return
	}
	outcome := core.SanitizeString(trade.Outcome)
	if outcome == "" {
		return
	}
	state.observedBuySizeSum[outcome] += trade.Size
	state.observedBuySizeCount[outcome]++
}

func realbotEstimatedPositionBuySignals(state *realbotCopytradeState, conditionID, outcome string, delta float64, mode string) []api.PublicTrade {
	outcome = core.SanitizeString(outcome)
	if outcome == "" || delta <= 0.01 {
		return nil
	}
	if strings.EqualFold(mode, core.CopytradeSizingModePercent) {
		return []api.PublicTrade{{
			ConditionID: strings.TrimSpace(conditionID),
			Outcome:     outcome,
			Side:        "BUY",
			Size:        delta,
			Source:      "position",
		}}
	}

	estimatedTrades := 1
	if state != nil {
		if count := state.observedBuySizeCount[outcome]; count > 0 {
			avg := state.observedBuySizeSum[outcome] / float64(count)
			if avg > 0.01 {
				estimatedTrades = int(math.Ceil(delta / avg))
			}
		}
	}
	if estimatedTrades < 1 {
		estimatedTrades = 1
	}
	if estimatedTrades > 16 {
		estimatedTrades = 16
	}

	signals := make([]api.PublicTrade, 0, estimatedTrades)
	remaining := delta
	for i := 0; i < estimatedTrades; i++ {
		chunk := remaining / float64(estimatedTrades-i)
		if chunk <= 0.01 {
			continue
		}
		signals = append(signals, api.PublicTrade{
			ConditionID: strings.TrimSpace(conditionID),
			Outcome:     outcome,
			Side:        "BUY",
			Size:        chunk,
			Source:      "position-estimate",
		})
		remaining -= chunk
	}
	if len(signals) == 0 {
		return nil
	}
	return signals
}

func realbotCopytradeBootstrapStartTimestamp(startedAt time.Time) int64 {
	if startedAt.IsZero() {
		return 0
	}
	startTs := startedAt.Unix()
	if startedAt.Nanosecond() != 0 {
		startTs--
	}
	return startTs
}

func realbotCopytradeRetrySignalFresh(now time.Time, trade api.PublicTrade) bool {
	if trade.Timestamp <= 0 {
		return true
	}
	tradeAt := time.Unix(trade.Timestamp, 0)
	if now.Before(tradeAt) {
		return true
	}
	return now.Sub(tradeAt) <= realbotCopytradeRetryMaxAge
}

func realbotCopytradeTakeRetryTrades(state *realbotCopytradeState, now time.Time) []api.PublicTrade {
	if state == nil || len(state.retryTrades) == 0 {
		return nil
	}
	retries := state.retryTrades
	state.retryTrades = nil
	if now.IsZero() {
		now = time.Now()
	}
	fresh := make([]api.PublicTrade, 0, len(retries))
	for _, trade := range retries {
		if realbotCopytradeRetrySignalFresh(now, trade) {
			fresh = append(fresh, trade)
		}
	}
	return fresh
}

func realbotCopytradeQueueRetryTrades(state *realbotCopytradeState, retries []api.PublicTrade) {
	if state == nil || len(retries) == 0 {
		return
	}
	if len(retries) > realbotCopytradeRetryQueueCap {
		retries = retries[len(retries)-realbotCopytradeRetryQueueCap:]
	}
	state.retryTrades = append(state.retryTrades, retries...)
	if len(state.retryTrades) > realbotCopytradeRetryQueueCap {
		state.retryTrades = append([]api.PublicTrade(nil), state.retryTrades[len(state.retryTrades)-realbotCopytradeRetryQueueCap:]...)
	}
}

func realbotCopytradeFreshTrades(state *realbotCopytradeState, trades []api.PublicTrade, conditionID string, sizingMode string) []api.PublicTrade {
	if state == nil || len(trades) == 0 {
		return nil
	}
	conditionID = strings.TrimSpace(conditionID)
	now := time.Now()
	for key, seenAt := range state.seenTradeKeys {
		if now.Sub(seenAt) > 15*time.Minute {
			delete(state.seenTradeKeys, key)
		}
	}

	filtered := make([]api.PublicTrade, 0, len(trades))
	for _, trade := range trades {
		if conditionID != "" && !strings.EqualFold(strings.TrimSpace(trade.ConditionID), conditionID) {
			continue
		}
		if core.SanitizeString(trade.Outcome) == "" {
			continue
		}
		filtered = append(filtered, trade)
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].Timestamp == filtered[j].Timestamp {
			return realbotCopytradeTradeKey(filtered[i]) < realbotCopytradeTradeKey(filtered[j])
		}
		return filtered[i].Timestamp < filtered[j].Timestamp
	})

	fresh := make([]api.PublicTrade, 0, len(filtered))
	currentPollCounts := make(map[string]int)
	for _, trade := range filtered {
		baseKey := realbotCopytradeTradeKey(trade)
		currentPollCounts[baseKey]++

		totalSeen := state.seenTradeKeysCount[baseKey]
		if currentPollCounts[baseKey] > totalSeen {
			state.seenTradeKeysCount[baseKey] = currentPollCounts[baseKey]
			state.seenTradeKeys[fmt.Sprintf("%s#%d", baseKey, currentPollCounts[baseKey])] = now

			if trade.Size <= 0.01 && !strings.EqualFold(sizingMode, core.CopytradeSizingModeShares) && !strings.EqualFold(sizingMode, core.CopytradeSizingModeUSDC) {
				continue
			}

			fresh = append(fresh, trade)
		}
	}
	if !state.tradesSeeded {
		state.tradesSeeded = true
		if state.startedAt.IsZero() {
			return nil
		}
		startTs := realbotCopytradeBootstrapStartTimestamp(state.startedAt)
		bootstrap := make([]api.PublicTrade, 0, len(fresh))
		for _, trade := range fresh {
			if trade.Timestamp < startTs {
				continue
			}
			bootstrap = append(bootstrap, trade)
		}
		sort.Slice(bootstrap, func(i, j int) bool {
			if bootstrap[i].Timestamp == bootstrap[j].Timestamp {
				return realbotCopytradeTradeKey(bootstrap[i]) < realbotCopytradeTradeKey(bootstrap[j])
			}
			return bootstrap[i].Timestamp < bootstrap[j].Timestamp
		})
		return bootstrap
	}
	return fresh
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
	if bid-bestBid > 0.0005 {
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
	if !realbotCopytradeHasOnchainWatcher(poller) {
		return
	}

	pollEvery := time.Duration(liveCfg.CopytradePollIntervalMs) * time.Millisecond
	if pollEvery <= 0 {
		pollEvery = 2 * time.Second
	}
	polledTrades := make([]api.PublicTrade, 0)
	targetDeltas := make(map[string]float64)
	shouldPoll := state.lastTradeFetch.IsZero() || time.Since(state.lastTradeFetch) >= pollEvery
	if shouldPoll {
		since := state.lastTradeFetch
		state.lastTradeFetch = time.Now()
		combinedTrades := make([]api.PublicTrade, 0)
		minedTrades := poller.minedSignalsForCondition(market.ConditionID, since)
		if pendingTrades := poller.pendingSignalsForCondition(market.ConditionID, since); len(pendingTrades) > 0 {
			combinedTrades = append(append([]api.PublicTrade{}, pendingTrades...), minedTrades...)
		} else {
			combinedTrades = append(combinedTrades, minedTrades...)
		}
		if len(combinedTrades) > 0 {
			state.lastError = ""
			polledTrades = realbotCopytradeFreshTrades(state, combinedTrades, market.ConditionID, liveCfg.CopytradeSizingMode)
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
			feeRate := tokenFeeRates[outcome]
			if feeRate == 0 {
				feeRate = 1000
			}
			ask := 0.0
			// quoteSource := "WS"
			if localAsk, _, ok := realbotCanUseLocalTakerCloseQuote(time.Now(), outcome, tokenBids, tokenAsks, tokenFullAsks, quoteState, realbotTakerCloseLocalMaxAge); ok {
				ask = localAsk
			} else {
				// quoteSource = "REST"
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
			if entryGate != nil && !entryGate.TryAcquire() {
				realbotLogCopytradeSignalResult(tui, marketID, trade, "↩", "requeued: another market is executing a live entry")
				requeueTrade(trade)
				continue
			}

			submitPrice := core.CopytradeBuyLimitPrice(ask, liveCfg.CopytradeMaxSlippagePct)
			if submitPrice <= 0 || submitPrice >= 1.0 {
				realbotLogCopytradeSignalResult(tui, marketID, trade, "⛔", fmt.Sprintf("skipped: invalid slippage cap from ask $%.3f", ask))
				if entryGate != nil {
					entryGate.Release()
				}
				continue
			}

			budgetShares := core.CalculateCopytradeSharesForMode(tradeSize, submitPrice, liveCfg.CopytradeSizeUSDC, liveCfg.CopytradeSizeShares, liveCfg.CopytradeSizePercent, liveCfg.MaxTradeSize, liveCfg.CopytradeSizingMode)
			requestedQty := normalizeMarketBuyShares(budgetShares)
			if requestedQty < minOnChainActionShares {
				realbotLogCopytradeSignalResult(tui, marketID, trade, "⛔", fmt.Sprintf("skipped: actionable size %s is below %.2f share at cap $%.3f", formatShareQty(requestedQty), minOnChainActionShares, submitPrice))
				if entryGate != nil {
					entryGate.Release()
				}
				continue
			}

			initialPosition := trader.GetLivePositionSize(tokenID)
			// tui.LogEvent("[%s] 🪞 Copytrade BUY %s: target %s %s shares, submit %s @ cap $%.3f (%s ask $%.3f, slip %.0fc)",
			//	marketID, outcome, label, formatShareQty(tradeSize), formatShareQty(requestedQty), submitPrice, quoteSource, ask, liveCfg.CopytradeMaxSlippagePct)
			tradeCtx, tradeCancel := context.WithTimeout(ctx, 4*time.Second)
			exec := executeMarketOrderWithSignals(tradeCtx, trader, api.SideBuy, tokenID, outcome, submitPrice, requestedQty, feeRate, initialPosition, 2500*time.Millisecond)
			tradeCancel()
			logDirectExecutionAudit(tui, marketID, "Copytrade BUY", requestedQty, submitPrice, exec)
			if entryGate != nil {
				entryGate.Release()
			}
			if !exec.Success {
				if exec.Err != nil {
					realbotLogCopytradeSignalResult(tui, marketID, trade, "❌", fmt.Sprintf("failed: %v", exec.Err))
				} else if exec.Result != nil && exec.Result.Message != "" {
					realbotLogCopytradeSignalResult(tui, marketID, trade, "❌", fmt.Sprintf("failed: %s", exec.Result.Message))
				} else {
					realbotLogCopytradeSignalResult(tui, marketID, trade, "❌", "failed: execution did not succeed")
				}
				continue
			}

			execQty := attributedBuyFill(exec, requestedQty, 0, false)
			if !hasConfirmedExecutedQty(api.SideBuy, execQty) {
				realbotLogCopytradeSignalResult(tui, marketID, trade, "⛔", "skipped: lacked confirmed fill")
				continue
			}

			execPrice := venueExecutionEffectivePrice(exec)
			if execPrice <= 0 {
				execPrice = ask
			}
			execCost := reportedBuyCost(exec, execPrice, execQty, requestedQty)
			if _, buyErr := engine.BuyForMarket(marketID, outcome, execPrice, execQty); buyErr != nil {
				tui.LogEvent("[%s] ⚠️ Copytrade local buy sync failed for %s: %v", marketID, outcome, buyErr)
			}
			state.managed[outcome] = true
			tui.RecordOrderWithMode(marketID, outcome, "BUY", execQty, execPrice, execCost, 0.0, 0.0, "copytrade", "FILLED")
			realbotLogCopytradeSignalResult(tui, marketID, trade, "✅", fmt.Sprintf("bought %s at $%.3f", formatShareQty(execQty), execPrice))
			if refreshWalletTruth != nil {
				refreshWalletTruth(5 * time.Second)
			}
			continue
		}

		if tradeSide == "SELL" {
			if !managed || localQty <= 0.01 {
				realbotLogCopytradeSignalResult(tui, marketID, trade, "⛔", "skipped: no managed local position to sell")
				continue
			}
			feeRate := tokenFeeRates[outcome]
			if feeRate == 0 {
				feeRate = 1000
			}
			bid := 0.0
			// quoteSource := "WS"
			if localBid, _, ok := realbotCanUseLocalCopytradeSellQuote(time.Now(), outcome, tokenBids, tokenAsks, tokenFullBids, quoteState, realbotTakerCloseLocalMaxAge); ok {
				bid = localBid
			} else {
				// quoteSource = "REST"
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
			submitPrice := core.CopytradeSellFloorPrice(bid, liveCfg.CopytradeMaxSlippagePct)
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
			if positionSignal && requestedQty > tradeSize {
				requestedQty = normalizeMarketSellShares(tradeSize)
			}
			if requestedQty > localQty {
				requestedQty = normalizeMarketSellShares(localQty)
			}
			if requestedQty < minOnChainActionShares {
				realbotLogCopytradeSignalResult(tui, marketID, trade, "⛔", fmt.Sprintf("skipped: actionable size %s is below %.2f share", formatShareQty(requestedQty), minOnChainActionShares))
				continue
			}

			initialPosition := trader.GetLivePositionSize(tokenID)
			// tui.LogEvent("[%s] 🪞 Copytrade SELL %s: target %s %s shares, sell %s @ floor $%.3f (%s bid $%.3f, slip %.0fc)",
			//	marketID, outcome, label, formatShareQty(tradeSize), formatShareQty(requestedQty), submitPrice, quoteSource, bid, liveCfg.CopytradeMaxSlippagePct)
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
				continue
			}

			execQty := attributedSellFill(exec, requestedQty)
			if !hasConfirmedExecutedQty(api.SideSell, execQty) {
				realbotLogCopytradeSignalResult(tui, marketID, trade, "⛔", "skipped: lacked confirmed fill")
				continue
			}

			execPrice := venueExecutionEffectivePrice(exec)
			if execPrice <= 0 {
				execPrice = bid
			}
			if _, sellErr := engine.SellForMarket(marketID, outcome, execPrice, execQty); sellErr != nil {
				tui.LogEvent("[%s] ⚠️ Copytrade local sell sync failed for %s: %v", marketID, outcome, sellErr)
			}
			profit := (execPrice - avgPrice) * execQty
			tui.RecordOrderWithMode(marketID, outcome, "SELL", execQty, execPrice, execQty*execPrice, 0.0, profit, "copytrade", "FILLED")
			realbotLogCopytradeSignalResult(tui, marketID, trade, "✅", fmt.Sprintf("sold %s at $%.3f", formatShareQty(execQty), execPrice))
			if positionSignal {
				remainingSize := normalizeMarketSellShares(tradeSize - execQty)
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
