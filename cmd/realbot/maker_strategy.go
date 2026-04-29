package main

import (
	"context"
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
	if trader != nil && trader.IsPaperMode() {
		realbotCancelAllMakerQuotes(ctx, marketID, "maker mode unavailable on embedded paper backend", trader, engine, tui, makerQuotes)
		return
	}
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
