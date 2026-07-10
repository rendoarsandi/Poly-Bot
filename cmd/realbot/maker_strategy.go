package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/strategy"
	"Market-bot/internal/trading"
)

func resolveRealbotMakerQuoteGap(liveCfg paper.TUISettings, cfg *core.Config) float64 {
	if liveCfg.MakerQuoteGap > 0 {
		return liveCfg.MakerQuoteGap
	}
	if cfg != nil && cfg.MakerQuoteGap > 0 {
		return cfg.MakerQuoteGap
	}
	return realbotMakerBaseOffset
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
			if quote != nil && quote.OrderID != "" {
				trader.ResetConfirmedFill(quote.OrderID)
			}
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
				trader.ResetConfirmedFill(quote.OrderID)
				delete(makerQuotes, key)
			}
			continue
		}
		quote.RemainingQty = normalizeMarketSellShares(math.Max(0, quote.RequestedQty-quote.AccountedFill))
		if quote.RemainingQty*quote.Price < 1.0 {
			trader.ResetConfirmedFill(quote.OrderID)
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
		trader.ResetConfirmedFill(quote.OrderID)
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
	realbotRecordOrderSubmissions(1)
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

func realbotUpsertPaperMakerQuote(marketID string, tui *paper.TUI, makerQuotes map[string]*realbotMakerQuote, side api.Side, outcome, tokenID string, price, qty float64) bool {
	key := realbotMakerQuoteKey(side, outcome)
	existing := makerQuotes[key]
	qty = normalizeMarketSellShares(qty)

	orderValue := qty * price
	if orderValue < 1.0 || price <= 0 || tokenID == "" {
		if existing != nil {
			delete(makerQuotes, key)
			return true
		}
		return false
	}

	if existing != nil {
		if math.Abs(existing.Price-price) < 1e-9 && math.Abs(existing.RemainingQty-qty) < 0.01 {
			return false
		}
		delete(makerQuotes, key)
	}

	makerQuotes[key] = &realbotMakerQuote{
		OrderID:       fmt.Sprintf("sim-%s-%s-%d", strings.ToLower(string(side)), outcome, time.Now().UnixNano()),
		TokenID:       tokenID,
		Outcome:       outcome,
		Side:          side,
		Price:         price,
		RequestedQty:  qty,
		RemainingQty:  qty,
		AccountedFill: 0,
		FeeRateBps:    0,
	}
	return true
}

func realbotSimulatePaperMakerFills(marketID string, engine *paper.Engine, tui *paper.TUI, makerQuotes map[string]*realbotMakerQuote, tokenBids, tokenAsks map[string]float64) {
	for key, quote := range makerQuotes {
		if quote == nil || quote.RemainingQty <= 0 {
			continue
		}

		bid := tokenBids[quote.Outcome]
		ask := tokenAsks[quote.Outcome]
		if bid <= 0 || ask <= 0 {
			continue
		}

		filled := false
		if quote.Side == api.SideBuy {
			if ask <= quote.Price+1e-9 {
				filled = true
			}
		} else {
			if bid >= quote.Price-1e-9 {
				filled = true
			}
		}

		if filled {
			qty := quote.RemainingQty
			if quote.Side == api.SideBuy {
				if _, err := engine.MakerBuyForMarket(marketID, quote.Outcome, quote.Price, qty); err != nil {
					tui.LogEvent("[%s] ⚠️ Paper Maker buy fill failed: %v", marketID, err)
				} else {
					tui.LogEvent("[%s] ✅ Paper Maker BUY fill: %s %.2f @ $%.3f", marketID, quote.Outcome, qty, quote.Price)
					tui.RecordOrderWithMode(marketID, quote.Outcome, "BUY", qty, quote.Price, qty*quote.Price, 0.0, 0.0, "maker", "FILLED")
				}
			} else {
				if _, err := engine.MakerSellForMarket(marketID, quote.Outcome, quote.Price, qty); err != nil {
					tui.LogEvent("[%s] ⚠️ Paper Maker sell fill failed: %v", marketID, err)
				} else {
					tui.LogEvent("[%s] ✅ Paper Maker SELL fill: %s %.2f @ $%.3f", marketID, quote.Outcome, qty, quote.Price)
					tui.RecordOrderWithMode(marketID, quote.Outcome, "SELL", qty, quote.Price, qty*quote.Price, 0.0, 0.0, "maker", "FILLED")
				}
			}
			delete(makerQuotes, key)
		}
	}
}

func maintainRealbotMakerQuotes(ctx context.Context, marketID string, endTime time.Time, outcomes []string, getTokenID func(string) string, tokenBids, tokenAsks map[string]float64, tokenFeeRates map[string]int, trader *trading.RealTrader, engine *paper.Engine, riskMgr *paper.RiskManager, tui *paper.TUI, liveCfg paper.TUISettings, cfg *core.Config, makerQuotes map[string]*realbotMakerQuote, lastMakerSync *time.Time, binanceFeed *api.BinanceFuturesPriceFeed) {
	if len(outcomes) != 2 {
		realbotCancelAllMakerQuotes(ctx, marketID, "maker mode requires exactly 2 outcomes", trader, engine, tui, makerQuotes)
		return
	}

	// Binance Volatility Protection
	if binanceFeed != nil {
		snap := binanceFeed.Snapshot(time.Now())
		if snap.Connected && snap.Ready && !snap.UpdatedAt.IsZero() && time.Since(snap.UpdatedAt) <= core.ResolveBinanceSignalMaxAge(cfg) {
			threshold := cfg.BinanceSignalThresholdPct
			if threshold <= 0 {
				threshold = 0.30
			}
			if math.Abs(snap.DeltaPercent) >= threshold {
				realbotCancelAllMakerQuotes(ctx, marketID, fmt.Sprintf("Binance volatility protection triggered: delta %.3f%% >= threshold %.3f%%", snap.DeltaPercent, threshold), trader, engine, tui, makerQuotes)
				if lastMakerSync != nil {
					*lastMakerSync = time.Now()
				}
				return
			}
		}
	}

	isPaper := trader != nil && trader.IsPaperMode()
	openByID := make(map[string]api.OpenOrder)

	if isPaper {
		realbotSimulatePaperMakerFills(marketID, engine, tui, makerQuotes, tokenBids, tokenAsks)
	} else {
		if len(makerQuotes) > 0 {
			openOrders, err := trader.GetOpenOrders(ctx)
			if err != nil {
				tui.LogEvent("[%s] ⚠️ Maker open-order refresh failed: %v — skipping maker quote maintenance", marketID, err)
				return
			}
			for _, order := range openOrders {
				openByID[order.OrderID] = order
			}
		}
		realbotSyncMakerQuoteFills(marketID, trader, engine, tui, makerQuotes, openByID)
	}

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

	baseTradeValue := realbotLiveTradeSize(realbotSizingCapitalForTrade(engine, liveCfg), liveCfg)
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

	// Dynamic Inventory-Skewed Bidirectional (Sell) Quoting
	mid0 := (bid0 + ask0) / 2.0
	mid1 := (bid1 + ask1) / 2.0
	if mid0 <= 0 {
		mid0 = 0.5
	}
	if mid1 <= 0 {
		mid1 = 0.5
	}
	targetShares0 := targetValue / mid0
	targetShares1 := targetValue / mid1

	skew0 := computeRealbotMakerInventorySkew(shares0, shares1, targetShares0)
	skew1 := computeRealbotMakerInventorySkew(shares1, shares0, targetShares1)

	quoteGap := resolveRealbotMakerQuoteGap(liveCfg, cfg)

	sellPrice0, sellOK0 := computeRealbotMakerProtectedSellQuote(bid0, ask0, avg0, minPairEdge, skew0, quoteGap, tokenFeeRates[outcomes[0]], timeToEnd, makerParams)
	sellPrice1, sellOK1 := computeRealbotMakerProtectedSellQuote(bid1, ask1, avg1, minPairEdge, skew1, quoteGap, tokenFeeRates[outcomes[1]], timeToEnd, makerParams)

	sellQty0 := 0.0
	if sellOK0 && shares0 > 0 {
		sellQty0 = computeRealbotMakerSellQty(baseTradeValue, shares0, skew0, sellPrice0, makerParams)
	}
	sellQty1 := 0.0
	if sellOK1 && shares1 > 0 {
		sellQty1 = computeRealbotMakerSellQty(baseTradeValue, shares1, skew1, sellPrice1, makerParams)
	}

	changed := false
	if isPaper {
		if realbotUpsertPaperMakerQuote(marketID, tui, makerQuotes, api.SideBuy, outcomes[0], getTokenID(outcomes[0]), buyPrice0, buyQty0) {
			changed = true
		}
		if realbotUpsertPaperMakerQuote(marketID, tui, makerQuotes, api.SideBuy, outcomes[1], getTokenID(outcomes[1]), buyPrice1, buyQty1) {
			changed = true
		}
		if realbotUpsertPaperMakerQuote(marketID, tui, makerQuotes, api.SideSell, outcomes[0], getTokenID(outcomes[0]), sellPrice0, sellQty0) {
			changed = true
		}
		if realbotUpsertPaperMakerQuote(marketID, tui, makerQuotes, api.SideSell, outcomes[1], getTokenID(outcomes[1]), sellPrice1, sellQty1) {
			changed = true
		}
	} else {
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
	}

	if lastMakerSync != nil {
		*lastMakerSync = time.Now()
	}
	if changed {
		tui.LogEvent("[%s] 🧾 Live maker quotes refreshed | Bids: %s buy@$%.3f x %.0f, %s buy@$%.3f x %.0f (pair=$%.3f) | Asks: %s sell@$%.3f x %.0f, %s sell@$%.3f x %.0f",
			marketID,
			outcomes[0], buyPrice0, buyQty0,
			outcomes[1], buyPrice1, buyQty1,
			buyPrice0+buyPrice1,
			outcomes[0], sellPrice0, sellQty0,
			outcomes[1], sellPrice1, sellQty1,
		)
	}
	realbotUpdateMakerPendingOrders(marketID, makerQuotes, tui)
}
