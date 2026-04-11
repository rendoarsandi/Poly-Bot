package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

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

func realbotMarketWindowDuration(marketID string) time.Duration {
	marketID = strings.TrimSpace(marketID)
	if marketID == "" {
		return 0
	}
	parts := strings.Split(marketID, "-")
	for _, part := range parts {
		if len(part) < 2 {
			continue
		}
		unit := part[len(part)-1]
		value := part[:len(part)-1]
		switch unit {
		case 'm', 'h', 'd':
			n := 0
			if _, err := fmt.Sscanf(value, "%d", &n); err != nil || n <= 0 {
				continue
			}
			switch unit {
			case 'm':
				return time.Duration(n) * time.Minute
			case 'h':
				return time.Duration(n) * time.Hour
			case 'd':
				return time.Duration(n) * 24 * time.Hour
			}
		}
	}
	return 0
}

func realbotMarketWindowStart(marketID string) (time.Time, bool) {
	endTime, err := paper.ParseEndTimeFromSlug(marketID)
	if err != nil || endTime.IsZero() {
		return time.Time{}, false
	}
	window := realbotMarketWindowDuration(marketID)
	if window <= 0 {
		return time.Time{}, false
	}
	return endTime.Add(-window), true
}

func realbotMarketSeriesKey(marketID string) string {
	marketID = strings.TrimSpace(marketID)
	if marketID == "" {
		return ""
	}
	idx := strings.LastIndex(marketID, "-")
	if idx <= 0 {
		return marketID
	}
	return marketID[:idx]
}

func realbotNewEntryBlockReason(currentMarketID string, engine *paper.Engine, splitInventory *paper.SplitInventory, liveCfg paper.TUISettings) (string, bool) {
	if !liveCfg.BlockNewEntriesOnPendingRedemption || engine == nil {
		return "", false
	}

	const eps = 1e-6
	_ = splitInventory

	pendingRedemptions := engine.GetPendingRedemptions()
	if len(pendingRedemptions) > 0 {
		marketIDs := make([]string, 0, len(pendingRedemptions))
		for marketID, payout := range pendingRedemptions {
			if marketID == "" || marketID == currentMarketID || payout <= eps {
				continue
			}
			marketIDs = append(marketIDs, marketID)
		}
		sort.Strings(marketIDs)
		if len(marketIDs) > 0 {
			marketID := marketIDs[0]
			return fmt.Sprintf("waiting for redemption payout from %s ($%.2f)", marketID, pendingRedemptions[marketID]), true
		}
	}

	positions := engine.GetPositions()
	if len(positions) == 0 {
		return "", false
	}
	sharesByMarket := make(map[string]float64)
	for _, pos := range positions {
		if pos.MarketID == "" || pos.MarketID == currentMarketID || pos.Quantity <= eps {
			continue
		}
		sharesByMarket[pos.MarketID] += pos.Quantity
	}
	if len(sharesByMarket) == 0 {
		return "", false
	}

	marketIDs := make([]string, 0, len(sharesByMarket))
	for marketID := range sharesByMarket {
		marketIDs = append(marketIDs, marketID)
	}
	sort.Strings(marketIDs)
	marketID := marketIDs[0]
	return fmt.Sprintf("waiting for prior inventory on %s (%s shares)", marketID, formatShareQty(sharesByMarket[marketID])), true
}

func realbotEntryBlockReason(currentMarketID string, engine *paper.Engine, splitInventory *paper.SplitInventory, liveCfg paper.TUISettings) (string, bool) {
	if reason, blocked := realbotNewEntryBlockReason(currentMarketID, engine, splitInventory, liveCfg); blocked {
		return reason, true
	}
	return realbotLateRedeemBlocksLadderEntry(currentMarketID, engine, liveCfg)
}

func realbotLateRedeemBlocksLadderEntry(currentMarketID string, engine *paper.Engine, liveCfg paper.TUISettings) (string, bool) {
	if !liveCfg.BlockNewEntriesOnPendingRedemption || engine == nil || normalizePaperArbMode(liveCfg.PaperArbMode) != paperArbModeLaddered {
		return "", false
	}
	currentStart, ok := realbotMarketWindowStart(currentMarketID)
	if !ok {
		return "", false
	}
	seriesKey := realbotMarketSeriesKey(currentMarketID)
	if seriesKey == "" {
		return "", false
	}

	settled := engine.GetSettledRedemptions()
	if len(settled) == 0 {
		return "", false
	}

	type lateRedeem struct {
		marketID  string
		settledAt time.Time
	}
	var latest lateRedeem
	found := false
	for marketID, settledAt := range settled {
		if marketID == "" || marketID == currentMarketID || settledAt.IsZero() {
			continue
		}
		if realbotMarketSeriesKey(marketID) != seriesKey {
			continue
		}
		if settledAt.Before(currentStart) {
			continue
		}
		if !found || settledAt.After(latest.settledAt) {
			latest = lateRedeem{marketID: marketID, settledAt: settledAt}
			found = true
		}
	}
	if !found {
		return "", false
	}
	return fmt.Sprintf("waiting for fresh next market after %s redeemed mid-window", latest.marketID), true
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

func normalizedRealbotExecutionPriceCap(liveCfg paper.TUISettings) float64 {
	limitPrice := liveCfg.TakerCloseMarketSlippage
	if limitPrice <= 0 || limitPrice >= 1.0 {
		return 0.99
	}
	return limitPrice
}

func realbotShouldMirrorExecutionIntoEngine(trader *trading.RealTrader) bool {
	return trader == nil || !trader.IsEmbeddedPaperMode()
}

func normalizePaperArbMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case paperArbModeLaddered:
		return paperArbModeLaddered
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

func realbotDecisionEvalInterval(settings paper.TUISettings, timeToExpiry time.Duration, entryExecutionInFlight bool) time.Duration {
	mode := normalizePaperArbMode(settings.PaperArbMode)
	if mode == paperArbModeCopytrade {
		return realbotTraderLoopInterval(settings)
	}
	if entryExecutionInFlight || mode == paperArbModeMaker || realbotTakerCloseHoldMode(settings) {
		return realbotMainLoopInterval
	}
	if timeToExpiry > 0 && timeToExpiry <= 30*time.Second {
		return realbotMainLoopInterval
	}
	if mode == paperArbModeBinanceGap {
		return 75 * time.Millisecond
	}
	return realbotDecisionLoopInterval
}

func realbotShouldRunDecisionLoop(now, lastEvalAt, lastEvalQuoteAt, latestQuoteAt time.Time, interval time.Duration) bool {
	if interval <= 0 {
		return true
	}
	if now.IsZero() {
		now = time.Now()
	}
	if lastEvalAt.IsZero() {
		return true
	}
	if now.Sub(lastEvalAt) >= interval {
		return true
	}
	return latestQuoteAt.After(lastEvalQuoteAt)
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

func realbotDirectionalBuyLimitPrice(ask, maxAskPrice, maxSlippagePct float64) float64 {
	if ask <= 0 {
		return 0
	}
	limit := ask + (maxSlippagePct / 100.0)
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

func realbotEnsureTopAskLevel(levels []paper.MarketLevel, topAsk, fallbackSize float64) []paper.MarketLevel {
	if topAsk <= 0 || topAsk >= 1.0 {
		return levels
	}
	hasTop := false
	for _, lvl := range levels {
		if lvl.Size <= 0 {
			continue
		}
		if lvl.Price <= topAsk+1e-6 {
			hasTop = true
			break
		}
	}
	if hasTop {
		return levels
	}
	injectSize := fallbackSize
	if injectSize < minOnChainActionShares {
		injectSize = minOnChainActionShares
	}
	return append(levels, paper.MarketLevel{Price: topAsk, Size: injectSize})
}

func realbotConfiguredPriceRange(liveCfg paper.TUISettings) (float64, float64) {
	minPrice := liveCfg.MinAskPrice
	maxPrice := liveCfg.MaxAskPrice
	if minPrice <= 0 {
		minPrice = 0.01
	}
	if maxPrice <= 0 || maxPrice > 0.99 {
		maxPrice = 0.99
	}
	if minPrice > maxPrice {
		minPrice = maxPrice
	}
	return minPrice, maxPrice
}

func realbotPriceWithinConfiguredRange(price float64, liveCfg paper.TUISettings) bool {
	minPrice, maxPrice := realbotConfiguredPriceRange(liveCfg)
	return price >= minPrice-1e-9 && price <= maxPrice+1e-9
}

func realbotConfiguredRangeReason(label string, price float64, liveCfg paper.TUISettings) string {
	minPrice, maxPrice := realbotConfiguredPriceRange(liveCfg)
	return fmt.Sprintf("%s $%.3f outside configured range %.3f-%.3f", label, price, minPrice, maxPrice)
}

func realbotDirectionalSellFloorPrice(bid, minPrice, maxSlippagePct float64) float64 {
	floor := core.CopytradeSellFloorPrice(bid, maxSlippagePct)
	if minPrice > floor {
		floor = minPrice
	}
	if floor > bid {
		floor = bid
	}
	return floor
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

func realbotCanUseLocalDirectionalBuyQuote(now time.Time, outcome string, tokenBids, tokenAsks map[string]float64, tokenFullAsks map[string][]paper.MarketLevel, lastPairUpdate time.Time, maxAge time.Duration) (bool, string) {
	_ = tokenFullAsks
	ask := tokenAsks[outcome]
	if ask <= 0 || ask >= 1.0 {
		return false, fmt.Sprintf("missing local ask for %s", outcome)
	}
	age := realbotPairQuoteAge(lastPairUpdate, now)
	if age > maxAge {
		return false, fmt.Sprintf("pair quote age %s > %s", age.Round(time.Millisecond), maxAge)
	}
	bid := tokenBids[outcome]
	if bid > 0 && !realbotHasSaneTopOfBook(bid, ask) {
		return false, fmt.Sprintf("crossed local quote for %s (bid %.3f >= ask %.3f)", outcome, bid, ask)
	}
	return true, ""
}

func realbotCanUseLocalDirectionalSellQuote(now time.Time, outcome string, tokenBids, tokenAsks map[string]float64, tokenFullBids map[string][]paper.MarketLevel, lastPairUpdate time.Time, maxAge time.Duration) (bool, string) {
	bid := tokenBids[outcome]
	if bid <= 0 || bid >= 1.0 {
		return false, fmt.Sprintf("missing local bid for %s", outcome)
	}
	if len(tokenFullBids[outcome]) == 0 {
		return false, fmt.Sprintf("missing local bid depth for %s", outcome)
	}
	age := realbotPairQuoteAge(lastPairUpdate, now)
	if age > maxAge {
		return false, fmt.Sprintf("pair quote age %s > %s", age.Round(time.Millisecond), maxAge)
	}
	ask := tokenAsks[outcome]
	if ask > 0 && !realbotHasSaneTopOfBook(bid, ask) {
		return false, fmt.Sprintf("crossed local quote for %s (bid %.3f >= ask %.3f)", outcome, bid, ask)
	}
	return true, ""
}

func realbotTUISettingsFromConfig(cfg *core.Config) paper.TUISettings {
	return paper.TUISettings{
		Exchange:                           cfg.Exchange,
		ExecutionBackend:                   cfg.ExecutionBackend,
		MarketSlug:                         cfg.MarketSlug,
		MaxMarkets:                         cfg.MaxMarkets,
		PaperBalance:                       cfg.PaperBalance,
		Timeframe:                          cfg.Timeframe,
		TradeSizingMode:                    cfg.TradeSizingMode,
		TradeScaleFactor:                   cfg.TradeScaleFactor,
		TradeSizeUSDC:                      cfg.TradeSizeUSDC,
		MinMarginPercent:                   cfg.MinMarginPercent,
		BinanceSignalThresholdPct:          cfg.BinanceSignalThresholdPct,
		PaperArbMode:                       normalizePaperArbMode(cfg.PaperArbMode),
		CopytradeTarget:                    cfg.CopytradeTarget,
		CopytradePollIntervalMs:            cfg.CopytradePollIntervalMs,
		CopytradeSizingMode:                cfg.CopytradeSizingMode,
		CopytradeSizeUSDC:                  cfg.CopytradeSizeUSDC,
		CopytradeSizeShares:                cfg.CopytradeSizeShares,
		CopytradeSizePercent:               cfg.CopytradeSizePercent,
		CopytradeMaxSlippagePct:            cfg.CopytradeMaxSlippagePct,
		LadderedTakerSizingMode:            cfg.LadderedTakerSizingMode,
		LadderedTakerSizeUSDC:              cfg.LadderedTakerSizeUSDC,
		LadderedTakerSizeShares:            cfg.LadderedTakerSizeShares,
		LadderedTakerReentryMoveCents:      cfg.LadderedTakerReentryMoveCents,
		LadderedTakerMaxSlippagePct:        cfg.LadderedTakerMaxSlippagePct,
		BuyExecutionMarginFloorPercent:     cfg.BuyExecutionMarginFloorPercent,
		SplitMinMarginSell:                 cfg.SplitMinMarginSell,
		SplitStrategyEnabled:               cfg.SplitStrategyEnabled,
		SplitInitialCapPct:                 cfg.SplitInitialCapPct,
		SplitReplenishCapPct:               cfg.SplitReplenishCapPct,
		MakerMergeBufferSeconds:            cfg.MakerMergeBufferSeconds,
		MakerQuoteGap:                      cfg.MakerQuoteGap,
		MakerInventoryTargetMult:           cfg.MakerInventoryTargetMult,
		MakerInventoryCapMult:              cfg.MakerInventoryCapMult,
		MakerMinQuoteValue:                 cfg.MakerMinQuoteValue,
		MinAskPrice:                        cfg.MinAskPrice,
		MaxAskPrice:                        cfg.MaxAskPrice,
		MaxTradeSize:                       cfg.MaxTradeSize,
		MaxDailyLoss:                       cfg.MaxDailyLoss,
		TakerCloseMarket:                   cfg.TakerCloseMarket,
		TakerCloseMarketTime:               cfg.TakerCloseMarketTime,
		TakerCloseMarketSlippage:           cfg.TakerCloseMarketSlippage,
		TakerCloseMarketMinPrice:           cfg.TakerCloseMarketMinPrice,
		TradingHoursMode:                   cfg.TradingHoursMode,
		PolygonRPC:                         cfg.PolygonRPCURL,
		PolygonPrivateKey:                  cfg.PK,
		BlockNewEntriesOnPendingRedemption: cfg.BlockNewEntriesOnPendingRedemption,
	}
}

func applyRealbotTUISettings(cfg *core.Config, s paper.TUISettings) {
	cfg.Exchange = s.Exchange
	cfg.ExecutionBackend = core.ExecutionBackendLive
	if strings.EqualFold(strings.TrimSpace(s.ExecutionBackend), core.ExecutionBackendPaper) {
		cfg.ExecutionBackend = core.ExecutionBackendPaper
	}
	cfg.MarketSlug = s.MarketSlug
	cfg.MaxMarkets = s.MaxMarkets
	cfg.PaperBalance = s.PaperBalance
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
	cfg.LadderedTakerSizingMode = s.LadderedTakerSizingMode
	cfg.LadderedTakerSizeUSDC = s.LadderedTakerSizeUSDC
	cfg.LadderedTakerSizeShares = s.LadderedTakerSizeShares
	cfg.LadderedTakerReentryMoveCents = s.LadderedTakerReentryMoveCents
	cfg.LadderedTakerMaxSlippagePct = s.LadderedTakerMaxSlippagePct
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
	cfg.BlockNewEntriesOnPendingRedemption = s.BlockNewEntriesOnPendingRedemption
	if cfg.ExecutionBackend == core.ExecutionBackendPaper {
		cfg.SplitStrategyEnabled = false
		if normalizePaperArbMode(cfg.PaperArbMode) == paperArbModeMaker {
			cfg.PaperArbMode = paperArbModeTaker
		}
	}
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
