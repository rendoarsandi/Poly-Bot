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
	realbotUIRefreshInterval         = 100 * time.Millisecond
	realbotMainLoopInterval          = 10 * time.Millisecond
	realbotFillPollInterval          = 50 * time.Millisecond
	realbotTakerCloseQuoteRefresh    = 500 * time.Millisecond
	realbotTakerCloseLogInterval     = 5 * time.Second
	realbotTakerCloseLocalMaxAge     = 350 * time.Millisecond
	realbotWalletTruthLogMinDelta    = 0.25
	realbotMaxSaneOutcomeSpread      = 0.10
	realbotMaxSaneAskPairSum         = 1.10
	realbotMinSaneBidPairSum         = 0.90
	realbotExecutionGuardQuoteMaxAge = 1500 * time.Millisecond
	realbotBalanceSyncInterval       = 60 * time.Second
	realbotBalanceSyncTimeout        = 8 * time.Second

	realbotMakerQuoteStep           = 0.001
	realbotMakerBaseOffset          = 0.008
	realbotMakerInventorySkewStep   = 0.020
	realbotMakerInventoryTargetMult = 2.5
	realbotMakerInventoryCapMult    = 5.0
	realbotMakerQuoteSizeSkewFactor = 0.75
	realbotMakerRequoteInterval     = 500 * time.Millisecond
	realbotMakerMinQuoteValue       = 5.0
	realbotMakerCashUsagePerOutcome = 0.35
)

var realbotMakerStrategyParams = strategy.MakerParams{
	QuoteStep:           realbotMakerQuoteStep,
	DefaultQuoteGap:     realbotMakerBaseOffset,
	InventorySkewStep:   realbotMakerInventorySkewStep,
	QuoteSizeSkewFactor: realbotMakerQuoteSizeSkewFactor,
	CashUsagePerOutcome: realbotMakerCashUsagePerOutcome,
	MinQuoteValue:       realbotMakerMinQuoteValue,
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
	for _, outcome := range outcomes {
		bid := bids[outcome]
		ask := asks[outcome]
		if bid == 0 || ask == 0 || !realbotHasSaneTopOfBook(bid, ask) {
			return true
		}
	}
	return false
}

func realbotTakerCloseHoldMode(cfg paper.TUISettings) bool {
	return cfg.TakerCloseMarket
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
	if realbotTakerCloseHoldMode(cfg) {
		return false
	}
	return timeToExpiry > 0 && timeToExpiry <= mergeBuffer
}

func realbotTakerCloseBudget(cash, sizingBalance float64, liveCfg paper.TUISettings) float64 {
	if cash <= 0 {
		return 0
	}
	tradeFactor := liveCfg.TradeScaleFactor
	if tradeFactor <= 0 {
		tradeFactor = 0.01
	}
	if sizingBalance <= 0 {
		sizingBalance = cash
	}
	budget := sizingBalance * tradeFactor
	if budget > cash {
		budget = cash
	}
	if liveCfg.MaxTradeSize > 0 && budget > liveCfg.MaxTradeSize {
		budget = liveCfg.MaxTradeSize
	}
	if budget < 1.0 {
		budget = 1.0
	}
	return budget
}

func realbotSizingCapitalForTrade(engine *paper.Engine) float64 {
	if engine == nil {
		return 0
	}
	equity := engine.GetBookEquity()
	if equity < 0 {
		return 0
	}
	return equity
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
		return 0.60
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
	case paperArbModeMaker:
		return paperArbModeMaker
	default:
		return paperArbModeTaker
	}
}

func realbotTUISettingsFromConfig(cfg *core.Config) paper.TUISettings {
	return paper.TUISettings{
		Exchange:                       cfg.Exchange,
		MarketSlug:                     cfg.MarketSlug,
		MaxMarkets:                     cfg.MaxMarkets,
		Timeframe:                      cfg.Timeframe,
		TradeScaleFactor:               cfg.TradeScaleFactor,
		MinMarginPercent:               cfg.MinMarginPercent,
		PaperArbMode:                   normalizePaperArbMode(cfg.PaperArbMode),
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
	}
}

func applyRealbotTUISettings(cfg *core.Config, s paper.TUISettings) {
	cfg.Exchange = s.Exchange
	cfg.MarketSlug = s.MarketSlug
	cfg.MaxMarkets = s.MaxMarkets
	cfg.Timeframe = s.Timeframe
	cfg.TradeScaleFactor = s.TradeScaleFactor
	cfg.MinMarginPercent = s.MinMarginPercent
	cfg.PaperArbMode = normalizePaperArbMode(s.PaperArbMode)
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
	if cfg.Exchange == "kalshi" {
		cfg.SplitStrategyEnabled = false
		cfg.MakerMergeBufferSeconds = 0
	}
}

func roundDown(v float64) float64 {
	return math.Floor(v*1000) / 1000
}

func roundRealbotMakerPrice(v float64) float64 {
	return math.Round(v*1000) / 1000
}

func clampFloat64(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
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

func launchRealbotRedeemRetryLoop(marketID, conditionID, winner string, numOutcomes int, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI) {
	go func() {
		attempt := 0
		for {
			attempt++
			redeemCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			txHash, err := trader.RedeemOnChainForce(redeemCtx, conditionID, numOutcomes)
			cancel()

			refreshCtx, refreshCancel := context.WithTimeout(context.Background(), 20*time.Second)
			refreshErr := refreshWalletTruthForRedemption(refreshCtx, marketID, conditionID, trader, engine, tui)
			positions, positionsErr := realbotWalletTruthPositionsForRedemption(refreshCtx, marketID, conditionID, trader, engine)
			refreshCancel()

			if err == nil {
				if len(txHash) >= 10 {
					tui.LogEvent("[%s] ✅ REDEEMED! Tx: %s", marketID, txHash[:10]+"...")
				} else {
					tui.LogEvent("[%s] ✅ REDEEMED! Tx: %s", marketID, txHash)
				}
			} else {
				tui.LogEvent("[%s] ⚠️ Redeem attempt %d pending: %v", marketID, attempt, err)
			}

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

			time.Sleep(1 * time.Minute)
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
	return !realbotHasSanePairQuotes(outcomes, bids, asks) && !realbotLooksLikeTerminalBook(outcomes, bids, asks)
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

func (w *realbotCLOBWarmer) WarmOrderPath(ctx context.Context) error {
	var firstErr error
	var errMu sync.Mutex

	if w.client != nil {
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := w.client.Ping(ctx); err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
				}
			}()
		}
		wg.Wait()
	}
	// Occasional balance check to keep auth paths warm
	if w.trader != nil && time.Now().Unix()%15 == 0 {
		if _, err := w.trader.GetTradingAllowance(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func startRealbotOrderWarmLoop(ctx context.Context, warmer *realbotCLOBWarmer) func() {
	warmCtx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(900 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-warmCtx.Done():
				return
			case <-ticker.C:
				singleCtx, singleCancel := context.WithTimeout(warmCtx, 1200*time.Millisecond)
				_ = warmer.WarmOrderPath(singleCtx)
				singleCancel()
			}
		}
	}()
	return cancel
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
		tui.StartRenderLoop(realbotUIRefreshInterval, stop)
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
	currentBalance := balance // Seed with the pre-fetched balance

	for {
		// Check for shutdown signal before starting a new round
		select {
		case <-ctx.Done():
			goto shutdown
		default:
		}

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

		// Find markets
		tui.LogEvent("🔍 Scanning markets...")
		markets := mkt.FindMarkets(ctx, restClient, tui.GetSettings, func(format string, args ...interface{}) {
			tui.LogEvent(format, args...)
		})
		if len(markets) == 0 {
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

		// Create a context for this specific round of trading
		roundCtx, roundCancel := context.WithCancel(ctx)

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
			go func(id string, m *api.Market, end time.Time, r *paper.RiskManager, bal float64) {
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
				tradeMarket(ctx, tCtx, id, m, end, realTrader, engine, orderBook, r, tui, restClient, cfg, bal, globalSplitStatus, globalSplitInventories, globalInitialSplits, &splitMu, &splitTxMu, resolutionCache)
			}(marketID, market, endTime, marketRiskMgr, currentBalance)
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
	}

shutdown:
	tui.Stop()
	fmt.Println("\n👋 Bot stopped.")
	emergencyCleanup()
	return nil
}

func tradeMarket(globalCtx context.Context, ctx context.Context, id string, market *api.Market, endTime time.Time,
	trader *trading.RealTrader, engine *paper.Engine, orderBook *paper.OrderBook,
	riskMgr *paper.RiskManager, tui *paper.TUI, restClient *api.RestClient, cfg *core.Config, startingBalance float64,
	globalSplitStatus map[string]bool, globalSplitInventories map[string]*paper.SplitInventory, globalInitialSplits map[string]float64, splitMu *sync.Mutex, splitTxMu *sync.Mutex, resolutionCache *api.ResolutionCache) {

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
	lastPublishedQuoteAt := time.Time{}
	lastTrade := time.Time{}
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

	// Background ticker to keep balance and allowance fresh without blocking WS loop
	go func() {
		ticker := time.NewTicker(8 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				bgCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
				_ = trader.UpdateBalanceAllowance(bgCtx)
				// Polymarket's user WS does not expose USDC balance. Keep the
				// cached on-chain balance warm instead of forcing a refresh every tick.
				_, _ = trader.GetBalance(bgCtx)
				cancel()
			}
		}
	}()

	// Helper to get token ID from outcome
	getTokenID := func(outcome string) string {
		for tid, out := range tokenToOutcome {
			if out == outcome {
				return tid
			}
		}
		return ""
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
	var nextTakerCloseAttempt time.Time
	var lastTakerCloseLog time.Time
	var lastTakerCloseLogKey string
	var lastTakerCloseQuoteRefresh time.Time
	preserveWalletTruth := false
	defer func() {
		if !preserveWalletTruth {
			tui.ClearWalletTruthPositions(id)
		}
	}()
	replenishCtrl := paper.NewReplenishController() // Debounce replenish goroutines
	var nextNearCloseCleanup time.Time
	var nearExpiryNoticeSent bool

	refreshWalletTruth := func(timeout time.Duration) {
		truthCtx, truthCancel := context.WithTimeout(ctx, timeout)
		defer truthCancel()
		_, _ = syncWalletTruthPositions(truthCtx, id, tokenToOutcome, trader, engine, splitInventory, tui)
	}
	refreshWalletTruth(5 * time.Second)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refreshWalletTruth(5 * time.Second)
			}
		}
	}()

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
		mergeBuffer := time.Duration(cfg.SplitMergeBufferSeconds) * time.Second
		if realbotShouldRunNearExpiryCleanup(liveCfg, timeToExpiry, mergeBuffer) {
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
			recovered := handleRestFallbackWithDepth(ctx, id, pairQuoteAge, tokenMap, tokenBids, tokenAsks, displayBids, displayAsks, tokenFullBids, tokenFullAsks, quoteState, engine, restClient, tui, wasFallbackActive && !restRecoveryLogged)
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
		if liveCfg.TakerCloseMarket && timeToExpiry > 0 && timeToExpiry <= takerCloseTime {
			if !takerCloseAttempted && !time.Now().Before(nextTakerCloseAttempt) {
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
						nextTakerCloseAttempt = time.Now().Add(1 * time.Second)
						if realbotShouldLogTakerCloseState(&lastTakerCloseLog, &lastTakerCloseLogKey, "missing-token-id:"+bestOutcome, realbotTakerCloseLogInterval) {
							tui.LogEvent("[%s] ⚠️ Taker close skipped: missing token id for %s", id, bestOutcome)
						}
						continue
					}

					budget := realbotTakerCloseBudget(engine.GetBalance(), realbotSizingCapitalForTrade(engine), liveCfg)
					plan, planErr := buildRealbotTakerClosePlan(budget, confirmPrice, liveCfg)
					if planErr != nil {
						if realbotShouldLogTakerCloseState(&lastTakerCloseLog, &lastTakerCloseLogKey, "plan-rejected:"+strings.TrimSpace(planErr.Error()), realbotTakerCloseLogInterval) {
							tui.LogEvent("[%s] ⚠️ Taker close plan rejected: %v", id, planErr)
						}
						takerCloseAttempted = true
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
						continue
					}

					execQty := attributedBuyFill(exec, plan.RequestedQty, 0, false)
					if !hasConfirmedExecutedQty(api.SideBuy, execQty) {
						tui.LogEvent("[%s] ⚠️ Taker close execution below confirmation threshold: %s shares", id, formatShareQty(execQty))
						continue
					}

					execPrice := venueExecutionEffectivePrice(exec)
					if execPrice <= 0 {
						execPrice = plan.LimitPrice
					}
					preLocalQty, _ := localBoughtPositionAvg(engine, id, bestOutcome)
					execCost := execQty * execPrice
					if exec.AcknowledgedNotional > 0 {
						execCost = exec.AcknowledgedNotional
					}
					if _, buyErr := engine.BuyForMarket(id, bestOutcome, execPrice, execQty); buyErr != nil {
						tui.LogEvent("[%s] ⚠️ Taker close local inventory sync failed after confirmed fill: %v", id, buyErr)
					}
					postLocalQty, _ := localBoughtPositionAvg(engine, id, bestOutcome)
					tui.RecordOrder(id, bestOutcome, "BUY", execQty, execPrice, execCost, 0.0, 0.0, "FILLED")
					takerCloseExecutedAt = time.Now()
					if recoveredLateFill {
						tui.LogEvent("[%s] 🔄 Taker close recovered delayed fill: bought %s %s after post-timeout refresh", id, formatShareQty(execQty), bestOutcome)
					}
					if execPrice+1e-9 < plan.MinPrice {
						tui.LogEvent("[%s] ℹ️ Taker close filled below the trigger price ($%.3f < $%.3f); the min-price gate only decides when to enter, and the venue matched at a better price", id, execPrice, plan.MinPrice)
					}
					tui.LogEvent("[%s] ✅ Taker close confirmed: bought %s %s at $%.3f (cap $%.3f)",
						id, formatShareQty(execQty), bestOutcome, execPrice, plan.LimitPrice)
					tui.LogEvent("[%s] 🧾 Taker close position delta: %s %s | local position %.4f → %.4f | spend $%.4f",
						id, formatShareQty(execQty), bestOutcome, preLocalQty, postLocalQty, execCost)

					refreshWalletTruth(5 * time.Second)
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
			time.Sleep(100 * time.Millisecond)
			continue
		}

		liveCfg = tui.GetSettings()
		arbMode := normalizePaperArbMode(liveCfg.PaperArbMode)

		if !liveCfg.TakerCloseMarket && len(outcomes) == 2 && time.Since(lastTrade) > 5*time.Second && time.Now().After(nextLiveRecoveryAttempt) {
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
		if liveCfg.TakerCloseMarket {
			cancelMakerCtx, cancelMaker := context.WithTimeout(context.Background(), 5*time.Second)
			realbotCancelAllMakerQuotes(cancelMakerCtx, id, "taker close market enabled", trader, engine, tui, makerQuotes)
			cancelMaker()
			time.Sleep(100 * time.Millisecond)
			continue
		}

		if arbMode == paperArbModeMaker {
			makerCtx, makerCancel := context.WithTimeout(ctx, 5*time.Second)
			maintainRealbotMakerQuotes(makerCtx, id, endTime, outcomes, getTokenID, tokenBids, tokenAsks, tokenFeeRates, trader, engine, riskMgr, tui, liveCfg, cfg, makerQuotes, &lastMakerSync)
			makerCancel()
			time.Sleep(100 * time.Millisecond)
			continue
		}
		cancelMakerCtx, cancelMaker := context.WithTimeout(context.Background(), 5*time.Second)
		realbotCancelAllMakerQuotes(cancelMakerCtx, id, "maker mode disabled", trader, engine, tui, makerQuotes)
		cancelMaker()

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
				baseTradeSize := cfg.CalculateTradeSize(realbotSizingCapitalForTrade(engine))

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
				baseTradeSize := cfg.CalculateTradeSize(realbotSizingCapitalForTrade(engine))
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

					// Dynamic trade size uses the ratcheting session sizing balance.
					// It only moves up when a profitable round locks in a new high-water mark,
					// so drawdowns do not shrink the configured trade factor mid-session.
					tradeSize := cfg.CalculateTradeSize(realbotSizingCapitalForTrade(engine))

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

						lastTrade = time.Now()
					}
				}
			}
		}

		time.Sleep(realbotMainLoopInterval)
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
	if math.Abs(qty) >= 0.01 {
		return fmt.Sprintf("%.2f", qty)
	}
	return fmt.Sprintf("%.6f", qty)
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

func ackNotionalMatchesAttributedBuy(exec directMarketExecution, attributedQty float64) bool {
	if exec.AcknowledgedQty <= 0 || exec.AcknowledgedNotional <= 0 || attributedQty <= 0 {
		return false
	}
	diff := math.Abs(exec.AcknowledgedQty - attributedQty)
	return diff <= math.Max(0.05, attributedQty*0.05)
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
	tx := exec.Result.TransactionsHashes[0]
	if len(tx) > 12 {
		return tx[:12] + "..."
	}
	return tx
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

func finalizeDirectMarketExecutionWithSignals(ctx context.Context, trader *trading.RealTrader, req directMarketOrderSignalRequest, confirmTimeout time.Duration, result *trading.TradeResult, err error) directMarketExecution {
	result = hydrateDirectMarketTradeResult(req, result)
	orderID := result.OrderID
	acknowledgedQty := result.AcknowledgedQty
	acknowledgedNotional := result.AcknowledgedNotional
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

func computeRealbotMakerSellFeeUsdc(shares, price float64, feeRateBps int) float64 {
	return strategy.ComputeMakerSellFeeUsdc(shares, price, feeRateBps)
}

func computeRealbotMakerInventorySkew(positionShares, peerShares, targetShares float64) float64 {
	return strategy.ComputeMakerInventorySkew(positionShares, peerShares, targetShares)
}

func computeRealbotMakerSkewedQuote(side api.Side, bid, ask, skew, quoteGap float64, params strategy.MakerParams) (float64, bool) {
	return strategy.ComputeMakerSkewedQuote(side == api.SideBuy, bid, ask, skew, quoteGap, params)
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
				if _, err := engine.BuyForMarket(marketID, quote.Outcome, quote.Price, delta); err != nil {
					tui.LogEvent("[%s] ⚠️ Maker buy fill sync failed for %s %.4f @ $%.3f: %v", marketID, quote.Outcome, delta, quote.Price, err)
				} else {
					tui.LogEvent("[%s] ✅ Maker BUY fill: %s %.2f @ $%.3f", marketID, quote.Outcome, delta, quote.Price)
					tui.RecordOrderWithMode(marketID, quote.Outcome, "BUY", delta, quote.Price, delta*quote.Price, 0.0, 0.0, "maker", "FILLED")
				}
			} else {
				if _, err := engine.SellForMarket(marketID, quote.Outcome, quote.Price, delta); err != nil {
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

	baseTradeValue := cfg.CalculateTradeSize(realbotSizingCapitalForTrade(engine))
	// We no longer clamp baseTradeValue up to minQuoteValue to avoid forcing users
	// to trade larger amounts than their configured TradeScaleFactor. If baseTradeValue
	// is too small, strategy.ComputeMakerBuyQty will return 0 and skip quoting.

	targetValue := math.Max(minQuoteValue, baseTradeValue*targetMult)
	maxInventoryValue := math.Max(targetValue, baseTradeValue*capMult)
	minSellEdge := liveCfg.MinMarginPercent / 100.0
	quoteGap := resolveRealbotMakerQuoteGap(liveCfg, cfg)

	makerParams := realbotMakerStrategyParams
	makerParams.MinQuoteValue = minQuoteValue

	targetShares0 := 0.0
	if bid0 > 0 {
		targetShares0 = targetValue / bid0
	}
	targetShares1 := 0.0
	if bid1 > 0 {
		targetShares1 = targetValue / bid1
	}

	skew0 := computeRealbotMakerInventorySkew(shares0, shares1, targetShares0)
	skew1 := computeRealbotMakerInventorySkew(shares1, shares0, targetShares1)

	buyPrice0, buyOK0 := computeRealbotMakerSkewedQuote(api.SideBuy, bid0, ask0, skew0, quoteGap, makerParams)
	buyPrice1, buyOK1 := computeRealbotMakerSkewedQuote(api.SideBuy, bid1, ask1, skew1, quoteGap, makerParams)
	maxMakerBuyPrice := liveCfg.MaxAskPrice
	if maxMakerBuyPrice <= 0 || maxMakerBuyPrice > 0.99 {
		maxMakerBuyPrice = 0.99
	}
	minMakerBuyPrice := liveCfg.MinAskPrice
	if !buyOK0 || buyPrice0 > maxMakerBuyPrice || buyPrice0 < minMakerBuyPrice {
		buyOK0 = false
	}
	if !buyOK1 || buyPrice1 > maxMakerBuyPrice || buyPrice1 < minMakerBuyPrice {
		buyOK1 = false
	}

	sellFee0 := tokenFeeRates[outcomes[0]]
	sellFee1 := tokenFeeRates[outcomes[1]]
	sellPrice0, sellOK0 := computeRealbotMakerProtectedSellQuote(bid0, ask0, avg0, minSellEdge, skew0, quoteGap, sellFee0, timeToEnd, makerParams)
	sellPrice1, sellOK1 := computeRealbotMakerProtectedSellQuote(bid1, ask1, avg1, minSellEdge, skew1, quoteGap, sellFee1, timeToEnd, makerParams)
	sellQty0 := computeRealbotMakerSellQty(baseTradeValue, shares0, skew0, sellPrice0, makerParams)
	sellQty1 := computeRealbotMakerSellQty(baseTradeValue, shares1, skew1, sellPrice1, makerParams)
	if !sellOK0 {
		sellQty0 = 0
	}
	if !sellOK1 {
		sellQty1 = 0
	}

	buyQty0 := 0.0
	buyQty1 := 0.0
	if buyOK0 && !shouldRealbotMakerBlockBuy(shares0, sellOK0, shares1, avg1, buyPrice0, minSellEdge) {
		buyQty0 = computeRealbotMakerBuyQty(baseTradeValue, shares0, skew0, maxInventoryValue, quoteCash, buyPrice0, makerParams)
	}
	if buyOK1 && !shouldRealbotMakerBlockBuy(shares1, sellOK1, shares0, avg0, buyPrice1, minSellEdge) {
		buyQty1 = computeRealbotMakerBuyQty(baseTradeValue, shares1, skew1, maxInventoryValue, quoteCash, buyPrice1, makerParams)
	}

	changed := false
	if realbotUpsertMakerQuote(ctx, marketID, trader, riskMgr, tui, makerQuotes, openByID, api.SideBuy, outcomes[0], getTokenID(outcomes[0]), buyPrice0, buyQty0, tokenFeeRates[outcomes[0]]) {
		changed = true
	}
	if realbotUpsertMakerQuote(ctx, marketID, trader, riskMgr, tui, makerQuotes, openByID, api.SideBuy, outcomes[1], getTokenID(outcomes[1]), buyPrice1, buyQty1, tokenFeeRates[outcomes[1]]) {
		changed = true
	}
	if realbotUpsertMakerQuote(ctx, marketID, trader, riskMgr, tui, makerQuotes, openByID, api.SideSell, outcomes[0], getTokenID(outcomes[0]), sellPrice0, sellQty0, tokenFeeRates[outcomes[0]]) {
		changed = true
	}
	if realbotUpsertMakerQuote(ctx, marketID, trader, riskMgr, tui, makerQuotes, openByID, api.SideSell, outcomes[1], getTokenID(outcomes[1]), sellPrice1, sellQty1, tokenFeeRates[outcomes[1]]) {
		changed = true
	}

	if lastMakerSync != nil {
		*lastMakerSync = time.Now()
	}
	if changed {
		tui.LogEvent("[%s] 🧾 Live maker quotes refreshed: %s buy@$%.3f/ sell@$%.3f | %s buy@$%.3f/ sell@$%.3f",
			marketID,
			outcomes[0], buyPrice0, sellPrice0,
			outcomes[1], buyPrice1, sellPrice1,
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

func realbotLocalQuoteSanityReason(outcomes []string, tokenBids, tokenAsks map[string]float64) string {
	for _, out := range outcomes {
		bid := tokenBids[out]
		ask := tokenAsks[out]
		if !realbotHasSaneTopOfBook(bid, ask) {
			if bid <= 0 || ask <= 0 {
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
		if askSum > realbotMaxSaneAskPairSum {
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

func realbotRefreshBuyExecutionBooks(ctx context.Context, restClient *api.RestClient, market *api.Market, outcomes []string, tokenFullBids, tokenFullAsks map[string][]paper.MarketLevel) (map[string]float64, time.Duration, error) {
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
			return nil, 0, fmt.Errorf("missing token id for outcome %s", out)
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

	prices := make(map[string]float64, len(outcomes))
	var maxLatency time.Duration
	for res := range results {
		if res.latency > maxLatency {
			maxLatency = res.latency
		}
		if res.err != nil {
			return nil, maxLatency, fmt.Errorf("fetching fresh order book for %s failed: %w", res.outcome, res.err)
		}
		tokenFullBids[res.outcome] = res.bids
		tokenFullAsks[res.outcome] = res.asks
		bestAsk, found := realbotBestAskFromLevels(res.asks)
		if !found {
			return nil, maxLatency, fmt.Errorf("no live ask found for %s", res.outcome)
		}
		prices[res.outcome] = bestAsk
	}
	return prices, maxLatency, nil
}

func realbotRefreshSellExecutionBooks(ctx context.Context, restClient *api.RestClient, market *api.Market, outcomes []string, tokenFullBids, tokenFullAsks map[string][]paper.MarketLevel) (map[string]float64, time.Duration, error) {
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
			return nil, 0, fmt.Errorf("missing token id for outcome %s", out)
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

	prices := make(map[string]float64, len(outcomes))
	var maxLatency time.Duration
	for res := range results {
		if res.latency > maxLatency {
			maxLatency = res.latency
		}
		if res.err != nil {
			return nil, maxLatency, fmt.Errorf("fetching fresh order book for %s failed: %w", res.outcome, res.err)
		}
		tokenFullBids[res.outcome] = res.bids
		tokenFullAsks[res.outcome] = res.asks
		bestBid, found := realbotBestBidFromLevels(res.bids)
		if !found {
			return nil, maxLatency, fmt.Errorf("no live bid found for %s", res.outcome)
		}
		prices[res.outcome] = bestBid
	}
	return prices, maxLatency, nil
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

func captureInitialPairSnapshot(ctx context.Context, trader *trading.RealTrader, token0, token1 string) (bal0, bal1 float64, source string, ok bool) {
	if onChain0, onChain1, err := loadPairOnChainBalances(ctx, trader, token0, token1); err == nil {
		return onChain0, onChain1, "on-chain", true
	}
	if pos0, pos1, err := loadPairPositionBalances(ctx, trader, token0, token1); err == nil {
		return pos0, pos1, "external position snapshot", true
	}
	return 0, 0, "", false
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

func handleRestFallbackWithDepth(ctx context.Context, id string, staleTime time.Duration, tokenMap map[string]string, bids, asks, displayBids, displayAsks map[string]float64, fullBids, fullAsks map[string][]paper.MarketLevel, quoteState map[string]realbotQuoteState, engine *paper.Engine, restClient *api.RestClient, tui *paper.TUI, logRecovery bool) bool {
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
		if realbotShouldSkipStaleQuoteUpdate(quoteState, outcome, updatedAt, bids[outcome], asks[outcome]) {
			success = true
			continue
		}
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

	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	checkRound := 0

	for {
		if checkRound > 0 {
			select {
			case <-ctx.Done():
				return
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

				tui.LogEvent("[%s] ⏳ Starting forced on-chain redemption retry loop (every 1m)...", id)
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
