package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"os/signal"
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
	"Market-bot/internal/strategy"
)

const (
	StartingBalance   = 100.0 // $100 paper trading balance
	UseLiveUI         = true  // Set to false for traditional logging
	paperArbModeTaker = "taker"
	paperArbModeMaker = "maker"

	// Split strategy constants
	MinSplitBuffer   = 50.0  // Minimum initial split buffer ($)
	MinSplitAmount   = 10.0  // Minimum split amount to execute ($)
	MaxSharesPerSell = 250.0 // Hard safety cap on shares per sell

	paperMakerQuoteStep           = 0.001
	paperMakerBaseOffset          = 0.008
	paperMakerInventorySkewStep   = 0.020
	paperMakerInventoryTargetMult = 2.5
	paperMakerInventoryCapMult    = 5.0
	paperMakerQuoteSizeSkewFactor = 0.75
	paperMakerRequoteInterval     = 1500 * time.Millisecond
	paperMakerMinQuoteValue      = 1.0
	paperMakerCashUsagePerOutcome = 0.35
)

var paperMakerStrategyParams = strategy.MakerParams{
	QuoteStep:           paperMakerQuoteStep,
	DefaultQuoteGap:     paperMakerBaseOffset,
	InventorySkewStep:   paperMakerInventorySkewStep,
	QuoteSizeSkewFactor: paperMakerQuoteSizeSkewFactor,
	CashUsagePerOutcome: paperMakerCashUsagePerOutcome,
	MinQuoteValue:      paperMakerMinQuoteValue,
}

// MarketTrader holds state for trading a single market
type MarketTrader struct {
	ID         string // "ETH" or "SOL"
	Market     *api.Market
	Engine     *paper.Engine
	OrderBook  *paper.OrderBook
	LadderMgr  *paper.LadderManager
	RiskMgr    *paper.RiskManager
	Monitor    *paper.MarketMonitor
	TokenMap   map[string]string // tokenID -> outcome
	Outcomes   []string
	EndTime    time.Time
	RestClient *api.RestClient
	WSMgr      *api.WSManager
	TUI        *paper.TUI      // Shared TUI
	CSVLogger  *core.CSVLogger // Optional CSV diagnostic logger
	Config     *core.Config    // Config for position sizing

	// Price tracking
	TokenBids     map[string]float64
	TokenAsks     map[string]float64
	TokenFullBids map[string][]paper.MarketLevel
	TokenFullAsks map[string][]paper.MarketLevel
	FloatPrices   map[string]float64

	// Last time ANY price update was received for this trader
	LastUpdate time.Time
	// Last time both sides had a valid, non-crossed local quote at once.
	LastPairUpdate time.Time

	// Last time we performed a REST fallback poll
	LastRestPoll time.Time

	// Split strategy simulation
	SplitInventory     *paper.SplitInventory
	ReplenishCtrl      *paper.ReplenishController
	SplitInitialized   bool
	InitialSplitAmount float64 // Track initial split for replenishment target
	LastSplitSell      time.Time
	MakerQuotes        map[string]*paper.LimitOrder
	LastMakerSync      time.Time

	// State
	LaddersPlaced bool
	MarketEnded   bool
	mu            sync.Mutex
}

type marketResult struct {
	realizedPnL float64
	trades      int
}

type paperExecutionLatency struct {
	detectedAt  time.Time
	startedAt   time.Time
	executedAt  time.Time
	settledAt   time.Time
	opportunity string
	marketID    string
	shares      float64
	marginPct   float64
	expectedPnL float64
}

func (l paperExecutionLatency) detectToStart() time.Duration {
	if l.detectedAt.IsZero() || l.startedAt.IsZero() {
		return 0
	}
	return l.startedAt.Sub(l.detectedAt)
}

func (l paperExecutionLatency) startToExecution() time.Duration {
	if l.startedAt.IsZero() || l.executedAt.IsZero() {
		return 0
	}
	return l.executedAt.Sub(l.startedAt)
}

func (l paperExecutionLatency) detectToExecution() time.Duration {
	if l.detectedAt.IsZero() || l.executedAt.IsZero() {
		return 0
	}
	return l.executedAt.Sub(l.detectedAt)
}

func (l paperExecutionLatency) detectToSettlement() time.Duration {
	if l.detectedAt.IsZero() || l.settledAt.IsZero() {
		return 0
	}
	return l.settledAt.Sub(l.detectedAt)
}

func logPaperExecutionLatency(t *MarketTrader, latency paperExecutionLatency) {
	settlement := latency.detectToSettlement()
	if settlement == 0 {
		settlement = latency.detectToExecution()
	}
	msg := fmt.Sprintf(
		"[%s] ⏱ %s latency | detect→start=%s | start→exec=%s | detect→exec=%s | detect→settle=%s | shares=%.2f | margin=%.2f%% | expected=$%.2f",
		latency.marketID,
		latency.opportunity,
		latency.detectToStart().Round(time.Microsecond),
		latency.startToExecution().Round(time.Microsecond),
		latency.detectToExecution().Round(time.Microsecond),
		settlement.Round(time.Microsecond),
		latency.shares,
		latency.marginPct,
		latency.expectedPnL,
	)
	t.TUI.LogEvent("%s", msg)
	if t.CSVLogger != nil {
		t.CSVLogger.Log("TRADE", t.ID, "LATENCY", msg, t.Engine.GetEquity())
	}
}

func normalizePaperArbMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case paperArbModeMaker:
		return paperArbModeMaker
	default:
		return paperArbModeTaker
	}
}

func roundDown(v float64) float64 {
	return math.Floor(v*1000) / 1000
}

func roundPaperMakerPrice(v float64) float64 {
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

func resolvePaperMakerQuoteGap(liveCfg paper.TUISettings, cfg *core.Config) float64 {
	if liveCfg.MakerQuoteGap > 0 {
		return liveCfg.MakerQuoteGap
	}
	if cfg != nil && cfg.MakerQuoteGap > 0 {
		return cfg.MakerQuoteGap
	}
	return paperMakerBaseOffset
}

func shouldPaperRestFallback(quoteAge, sinceLastRest, staleAfter, pollInterval time.Duration) bool {
	return quoteAge > staleAfter && sinceLastRest > pollInterval
}

func hasValidPaperPairQuotes(outcomes []string, bids, asks map[string]float64) bool {
	if len(outcomes) != 2 {
		return false
	}
	for _, outcome := range outcomes[:2] {
		bid := bids[outcome]
		ask := asks[outcome]
		if bid <= 0 || ask <= 0 || bid >= ask {
			return false
		}
	}
	return true
}

func paperPairQuoteAge(lastPairUpdate, now time.Time) time.Duration {
	if now.IsZero() {
		now = time.Now()
	}
	if lastPairUpdate.IsZero() {
		return time.Duration(1 << 62)
	}
	return now.Sub(lastPairUpdate)
}

func shouldUseLocalPaperPair(outcomes []string, bids, asks map[string]float64, lastPairUpdate time.Time, maxAge time.Duration, now time.Time) bool {
	return hasValidPaperPairQuotes(outcomes, bids, asks) && paperPairQuoteAge(lastPairUpdate, now) <= maxAge
}

func syncPaperPairUpdate(t *MarketTrader, now time.Time) {
	if hasValidPaperPairQuotes(t.Outcomes, t.TokenBids, t.TokenAsks) {
		t.LastPairUpdate = now
	}
}

func computePaperMakerArbPrices(bid1, ask1, bid2, ask2, maxSum float64) (float64, float64, bool) {
	minPrice1 := paperMakerQuoteStep
	if bid1 > 0 {
		minPrice1 = bid1 + paperMakerQuoteStep
	}
	minPrice2 := paperMakerQuoteStep
	if bid2 > 0 {
		minPrice2 = bid2 + paperMakerQuoteStep
	}
	maxPrice1 := ask1 - paperMakerQuoteStep
	maxPrice2 := ask2 - paperMakerQuoteStep
	if ask1 <= 0 || ask2 <= 0 || maxPrice1 < minPrice1 || maxPrice2 < minPrice2 {
		return 0, 0, false
	}

	price1 := roundDown((minPrice1 + maxPrice1) / 2)
	price2 := roundDown((minPrice2 + maxPrice2) / 2)
	if price1 < minPrice1 {
		price1 = minPrice1
	}
	if price2 < minPrice2 {
		price2 = minPrice2
	}
	if price1 > maxPrice1 {
		price1 = maxPrice1
	}
	if price2 > maxPrice2 {
		price2 = maxPrice2
	}

	for price1+price2 > maxSum+1e-9 {
		if price1-minPrice1 >= price2-minPrice2 && price1-paperMakerQuoteStep >= minPrice1 {
			price1 = roundDown(price1 - paperMakerQuoteStep)
			continue
		}
		if price2-paperMakerQuoteStep >= minPrice2 {
			price2 = roundDown(price2 - paperMakerQuoteStep)
			continue
		}
		return 0, 0, false
	}
	return price1, price2, true
}

func computePaperMakerSplitSellPrices(bid1, ask1, bid2, ask2, minSum float64) (float64, float64, bool) {
	minPrice1 := roundPaperMakerPrice(bid1 + paperMakerQuoteStep)
	minPrice2 := roundPaperMakerPrice(bid2 + paperMakerQuoteStep)
	maxPrice1 := roundPaperMakerPrice(ask1 - paperMakerQuoteStep)
	maxPrice2 := roundPaperMakerPrice(ask2 - paperMakerQuoteStep)
	if ask1 <= 0 || ask2 <= 0 || maxPrice1 < minPrice1 || maxPrice2 < minPrice2 {
		return 0, 0, false
	}

	price1 := minPrice1
	price2 := minPrice2
	for price1+price2 < minSum-1e-9 {
		room1 := maxPrice1 - price1
		room2 := maxPrice2 - price2
		if room1 < paperMakerQuoteStep-1e-9 && room2 < paperMakerQuoteStep-1e-9 {
			return 0, 0, false
		}
		if room1 >= room2 && room1 >= paperMakerQuoteStep-1e-9 {
			price1 = roundPaperMakerPrice(math.Min(maxPrice1, price1+paperMakerQuoteStep))
			continue
		}
		if room2 >= paperMakerQuoteStep-1e-9 {
			price2 = roundPaperMakerPrice(math.Min(maxPrice2, price2+paperMakerQuoteStep))
			continue
		}
		if room1 >= paperMakerQuoteStep-1e-9 {
			price1 = roundPaperMakerPrice(math.Min(maxPrice1, price1+paperMakerQuoteStep))
			continue
		}
		return 0, 0, false
	}
	return price1, price2, true
}

func computePaperSellFeeUsdc(shares, price float64, feeRateBps int) float64 {
	return strategy.ComputeMakerSellFeeUsdc(shares, price, feeRateBps)
}

func paperMakerQuoteKey(side, outcome string) string {
	return strings.ToLower(strings.TrimSpace(side)) + ":" + outcome
}

func isPaperOrderActive(order *paper.LimitOrder) bool {
	if order == nil {
		return false
	}
	return order.Status == paper.OrderStatusOpen || order.Status == paper.OrderStatusPartial
}

func getPaperMarketPosition(positions map[string]paper.Position, marketID, outcome string) (paper.Position, bool) {
	pos, ok := positions[marketID+":"+outcome]
	return pos, ok
}

func computePaperMakerInventorySkew(positionShares, peerShares, targetShares float64) float64 {
	return strategy.ComputeMakerInventorySkew(positionShares, peerShares, targetShares)
}

func computePaperMakerSkewedQuote(side string, bid, ask, skew, quoteGap float64, params strategy.MakerParams) (float64, bool) {
	return strategy.ComputeMakerSkewedQuote(strings.EqualFold(side, "buy"), bid, ask, skew, quoteGap, params)
}

func computePaperMakerBuyQty(baseShares, positionShares, skew, maxInventory, cash, price float64, params strategy.MakerParams) float64 {
	return strategy.ComputeMakerBuyQty(baseShares, positionShares, skew, maxInventory, cash, price, params, math.Floor)
}

func computePaperMakerSellQty(baseShares, positionShares, skew, price float64, params strategy.MakerParams) float64 {
	return strategy.ComputeMakerSellQty(baseShares, positionShares, skew, price, params, math.Floor)
}

func computePaperMakerProtectedSellQuote(bid, ask, avgCost, minEdge, skew, quoteGap float64, feeRateBps int, timeRemaining time.Duration, params strategy.MakerParams) (float64, bool) {
	return strategy.ComputeMakerProtectedSellQuote(bid, ask, avgCost, minEdge, skew, quoteGap, feeRateBps, timeRemaining, params)
}

func shouldPaperMakerBlockBuy(positionShares float64, sellOK bool, peerShares, peerAvgCost, price, minEdge float64) bool {
	return strategy.ShouldMakerBlockBuy(positionShares, sellOK, peerShares, peerAvgCost, price, minEdge)
}

func clearPaperMakerQuoteReference(t *MarketTrader, order *paper.LimitOrder) {
	if order == nil || len(t.MakerQuotes) == 0 {
		return
	}
	for key, existing := range t.MakerQuotes {
		if existing != nil && existing.ID == order.ID {
			delete(t.MakerQuotes, key)
		}
	}
}

func isPaperMakerQuote(t *MarketTrader, order *paper.LimitOrder) bool {
	if order == nil {
		return false
	}
	for _, existing := range t.MakerQuotes {
		if existing != nil && existing.ID == order.ID {
			return true
		}
	}
	return false
}

func cancelPaperMakerQuote(t *MarketTrader, side, outcome string) bool {
	key := paperMakerQuoteKey(side, outcome)
	existing := t.MakerQuotes[key]
	delete(t.MakerQuotes, key)
	if !isPaperOrderActive(existing) {
		return false
	}
	if err := t.OrderBook.CancelOrder(existing.ID); err != nil {
		return false
	}
	return true
}

func cancelAllPaperMakerQuotes(t *MarketTrader, reason string) {
	if len(t.MakerQuotes) == 0 {
		updatePaperPendingOrders(t)
		return
	}
	quotes := make(map[string]*paper.LimitOrder, len(t.MakerQuotes))
	for key, order := range t.MakerQuotes {
		quotes[key] = order
	}
	t.MakerQuotes = make(map[string]*paper.LimitOrder)
	for _, order := range quotes {
		if isPaperOrderActive(order) {
			_ = t.OrderBook.CancelOrder(order.ID)
		}
	}
	if reason != "" {
		t.TUI.LogEvent("[%s] 🧹 Maker quotes cancelled: %s", t.ID, reason)
	}
	updatePaperPendingOrders(t)
}

func upsertPaperMakerQuote(t *MarketTrader, side, outcome string, price, qty float64) bool {
	key := paperMakerQuoteKey(side, outcome)
	existing := t.MakerQuotes[key]
	
	// Check if the total dollar value meets the minimum requirement
	orderValue := qty * price
	if orderValue < t.Config.MakerMinQuoteValue || price <= 0 {
		return cancelPaperMakerQuote(t, side, outcome)
	}
	if isPaperOrderActive(existing) && math.Abs(existing.Price-price) < 1e-9 && math.Abs(existing.RemainingQty()-qty) < 1e-9 {
		return false
	}
	if isPaperOrderActive(existing) {
		_ = t.OrderBook.CancelOrder(existing.ID)
	}
	order := t.OrderBook.PlaceOrder(outcome, side, price, qty, 0)
	if t.MakerQuotes == nil {
		t.MakerQuotes = make(map[string]*paper.LimitOrder)
	}
	t.MakerQuotes[key] = order
	return true
}

func updatePaperPendingOrders(t *MarketTrader) {
	pending := make(map[string][]paper.PendingOrder)
	for _, order := range t.MakerQuotes {
		if !isPaperOrderActive(order) {
			continue
		}
		pending[order.Outcome] = append(pending[order.Outcome], paper.PendingOrder{
			MarketID: t.ID,
			Outcome:  order.Outcome,
			Price:    order.Price,
			Qty:      order.RemainingQty(),
			Side:     strings.ToUpper(order.Side),
		})
	}
	t.TUI.SetPendingOrders(t.ID, pending)
}

func ensurePaperMakerSplitInventory(t *MarketTrader, currentEquity, currentCash, sellMargin float64) {
	if len(t.Outcomes) != 2 {
		return
	}
	if !t.SplitInitialized {
		baseTradeSize := t.Config.CalculateTradeSize(currentEquity)
		initialBuffer := baseTradeSize * 2.0
		if initialBuffer < MinSplitBuffer {
			initialBuffer = MinSplitBuffer
		}
		maxInitial := currentEquity * t.Config.SplitInitialCapPct
		splitAmount := math.Min(initialBuffer, maxInitial)
		if splitAmount > currentCash {
			splitAmount = currentCash
		}
		if splitAmount >= MinSplitAmount {
			t.SplitInventory.RecordSplit(t.ID, t.Outcomes[0], t.Outcomes[1], splitAmount)
			t.Engine.DeductBalance(splitAmount)
			t.Engine.RecalculateDrawdown()
			t.SplitInitialized = true
			t.InitialSplitAmount = splitAmount
			t.TUI.LogEvent("[%s] 🔀 SPLIT (sim): Created %.0f shares ($%.2f)", t.ID, splitAmount, splitAmount)
			currentCash -= splitAmount
		}
	}

	baseTradeSize := t.Config.CalculateTradeSize(currentEquity)
	targetBuffer := baseTradeSize * t.Config.MaxAggressionMultiplier
	currentShares := t.SplitInventory.GetMinSplitShares(t.ID, t.Outcomes[0], t.Outcomes[1])
	replenishAmount := baseTradeSize * 2.0

	decision := t.ReplenishCtrl.CheckReplenish(paper.ReplenishParams{
		CurrentShares:      currentShares,
		TargetBuffer:       targetBuffer,
		InitialShares:      t.InitialSplitAmount,
		SellMargin:         sellMargin,
		MinMarginThreshold: t.Config.SplitMinMarginSell - 1.0,
		CurrentBalance:     currentCash,
		ReplenishAmount:    replenishAmount,
		MaxBalancePercent:  t.Config.SplitReplenishCapPct,
	})

	if decision.ShouldReplenish && t.ReplenishCtrl.MarkInProgress() {
		actualReplenish := decision.Amount
		if actualReplenish > currentCash {
			actualReplenish = currentCash
		}
		if actualReplenish >= MinSplitAmount {
			t.SplitInventory.RecordSplit(t.ID, t.Outcomes[0], t.Outcomes[1], actualReplenish)
			t.Engine.DeductBalance(actualReplenish)
			t.Engine.RecalculateDrawdown()
			t.TUI.LogEvent("[%s] 🔄 SPLIT (sim): Replenished +%.0f shares (now %.0f)", t.ID, actualReplenish, t.InitialSplitAmount)
		}
		t.ReplenishCtrl.MarkComplete()
	}
}

func maintainPaperMakerInventoryQuotes(t *MarketTrader, now time.Time) {
	if len(t.Outcomes) != 2 {
		cancelAllPaperMakerQuotes(t, "maker mode requires exactly 2 outcomes")
		return
	}
	localQuoteMaxAge := core.ResolveExecutionLocalQuoteMaxAge(t.Config)
	if !shouldUseLocalPaperPair(t.Outcomes, t.TokenBids, t.TokenAsks, t.LastPairUpdate, localQuoteMaxAge, now) {
		cancelAllPaperMakerQuotes(t, "waiting for fresh pair quotes")
		return
	}
	if !t.LastMakerSync.IsZero() && now.Sub(t.LastMakerSync) < paperMakerRequoteInterval {
		updatePaperPendingOrders(t)
		return
	}

	bid1, ask1 := t.TokenBids[t.Outcomes[0]], t.TokenAsks[t.Outcomes[0]]
	bid2, ask2 := t.TokenBids[t.Outcomes[1]], t.TokenAsks[t.Outcomes[1]]
	if bid1 <= 0 || ask1 <= 0 || bid2 <= 0 || ask2 <= 0 {
		cancelAllPaperMakerQuotes(t, "waiting for valid bid/ask on both outcomes")
		return
	}

	liveCfg := t.TUI.GetSettings()
	currentEquity := t.Engine.GetEquity()
	currentCash := t.Engine.GetBalance()
	positions := t.Engine.GetPositions()
	pos1, hasPos1 := getPaperMarketPosition(positions, t.ID, t.Outcomes[0])
	pos2, hasPos2 := getPaperMarketPosition(positions, t.ID, t.Outcomes[1])
	shares1, shares2 := 0.0, 0.0
	avgCost1, avgCost2 := 0.0, 0.0
	if hasPos1 {
		shares1 = pos1.Quantity
		avgCost1 = pos1.AvgPrice
	}
	if hasPos2 {
		shares2 = pos2.Quantity
		avgCost2 = pos2.AvgPrice
	}

	// Auto-merge delta-neutral inventory to free up capital and lock in spread profits
	if shares1 > 0 && shares2 > 0 {
		mergeQty := math.Min(shares1, shares2)
		if mergeQty >= 1.0 {
			result := t.Engine.MergeForMarket(t.ID, t.Outcomes[0], t.Outcomes[1], mergeQty)
			if t.SplitInventory != nil {
				t.SplitInventory.RecordMerge(t.ID, t.Outcomes[0], t.Outcomes[1], mergeQty)
			}
			if result != nil && result.PnL != 0 {
				t.TUI.LogEvent("[%s] 💰 Auto-merge realized PnL: $%.2f", t.ID, result.PnL)
			}

			// Re-fetch after merge
			positions = t.Engine.GetPositions()
			pos1, hasPos1 = getPaperMarketPosition(positions, t.ID, t.Outcomes[0])
			pos2, hasPos2 = getPaperMarketPosition(positions, t.ID, t.Outcomes[1])
			if hasPos1 {
				shares1 = pos1.Quantity
				avgCost1 = pos1.AvgPrice
			} else {
				shares1 = 0
				avgCost1 = 0
			}
			if hasPos2 {
				shares2 = pos2.Quantity
				avgCost2 = pos2.AvgPrice
			} else {
				shares2 = 0
				avgCost2 = 0
			}
		}
	}

	timeToEnd := time.Until(t.EndTime)
	mergeBuffer := 30 * time.Second
	if liveCfg.MakerMergeBufferSeconds > 0 {
		mergeBuffer = time.Duration(liveCfg.MakerMergeBufferSeconds) * time.Second
	} else if t.Config.MakerMergeBufferSeconds > 0 {
		mergeBuffer = time.Duration(t.Config.MakerMergeBufferSeconds) * time.Second
	}
	if timeToEnd < mergeBuffer && timeToEnd > 0 {
		cancelAllPaperMakerQuotes(t, "near expiry merge cleanup")
		mergeableShares := math.Floor(math.Min(shares1, shares2))
		if mergeableShares >= 1.0 {
			result := t.Engine.MergeForMarket(t.ID, t.Outcomes[0], t.Outcomes[1], mergeableShares)
			t.Engine.RecalculateDrawdown()
			t.TUI.LogEvent("[%s] 💰 MAKER MERGE (sim): Merged %.0f paired shares (pnl $%.2f)", t.ID, result.Shares, result.PnL)
		}
		updatePaperPendingOrders(t)
		return
	}

	minQuoteShares := t.Config.MakerMinQuoteValue
	if liveCfg.MakerMinQuoteValue > 0 {
		minQuoteShares = liveCfg.MakerMinQuoteValue
	}
	if minQuoteShares <= 0 {
		minQuoteShares = paperMakerMinQuoteValue
	}
	targetMult := t.Config.MakerInventoryTargetMult
	if liveCfg.MakerInventoryTargetMult > 0 {
		targetMult = liveCfg.MakerInventoryTargetMult
	}
	if targetMult <= 0 {
		targetMult = paperMakerInventoryTargetMult
	}
	capMult := t.Config.MakerInventoryCapMult
	if liveCfg.MakerInventoryCapMult > 0 {
		capMult = liveCfg.MakerInventoryCapMult
	}
	if capMult <= 0 {
		capMult = paperMakerInventoryCapMult
	}

	baseShares := t.Config.CalculateTradeSize(currentEquity)
	if baseShares < minQuoteShares {
		baseShares = minQuoteShares
	}
	targetShares := math.Max(minQuoteShares, baseShares*targetMult)
	maxInventory := math.Max(targetShares, baseShares*capMult)
	minSellEdge := t.Config.MinMarginPercent / 100.0
	quoteGap := resolvePaperMakerQuoteGap(liveCfg, t.Config)

	makerParams := paperMakerStrategyParams
	makerParams.MinQuoteValue = minQuoteShares

	skew1 := computePaperMakerInventorySkew(shares1, shares2, targetShares)
	skew2 := computePaperMakerInventorySkew(shares2, shares1, targetShares)

	buyPrice1, buyOK1 := computePaperMakerSkewedQuote("buy", bid1, ask1, skew1, quoteGap, makerParams)
	buyPrice2, buyOK2 := computePaperMakerSkewedQuote("buy", bid2, ask2, skew2, quoteGap, makerParams)
	maxMakerBuyPrice := liveCfg.MaxAskPrice
	if maxMakerBuyPrice <= 0 || maxMakerBuyPrice > 0.99 {
		maxMakerBuyPrice = 0.99
	}
	if !buyOK1 || buyPrice1 > maxMakerBuyPrice {
		buyOK1 = false
	}
	if !buyOK2 || buyPrice2 > maxMakerBuyPrice {
		buyOK2 = false
	}

	sellPrice1, sellOK1 := computePaperMakerProtectedSellQuote(bid1, ask1, avgCost1, minSellEdge, skew1, quoteGap, t.Config.FeeRateBps, timeToEnd, makerParams)
	sellPrice2, sellOK2 := computePaperMakerProtectedSellQuote(bid2, ask2, avgCost2, minSellEdge, skew2, quoteGap, t.Config.FeeRateBps, timeToEnd, makerParams)
	sellQty1 := math.Min(MaxSharesPerSell, computePaperMakerSellQty(baseShares, shares1, skew1, sellPrice1, makerParams))
	sellQty2 := math.Min(MaxSharesPerSell, computePaperMakerSellQty(baseShares, shares2, skew2, sellPrice2, makerParams))
	if !sellOK1 {
		sellQty1 = 0
	}
	if !sellOK2 {
		sellQty2 = 0
	}

	buyQty1 := 0.0
	buyQty2 := 0.0
	if buyOK1 && !shouldPaperMakerBlockBuy(shares1, sellOK1, shares2, avgCost2, buyPrice1, minSellEdge) {
		buyQty1 = math.Min(MaxSharesPerSell, computePaperMakerBuyQty(baseShares, shares1, skew1, maxInventory, currentCash, buyPrice1, makerParams))
	}
	if buyOK2 && !shouldPaperMakerBlockBuy(shares2, sellOK2, shares1, avgCost1, buyPrice2, minSellEdge) {
		buyQty2 = math.Min(MaxSharesPerSell, computePaperMakerBuyQty(baseShares, shares2, skew2, maxInventory, currentCash, buyPrice2, makerParams))
	}

	changed := false
	if upsertPaperMakerQuote(t, "buy", t.Outcomes[0], buyPrice1, buyQty1) {
		changed = true
	}
	if upsertPaperMakerQuote(t, "buy", t.Outcomes[1], buyPrice2, buyQty2) {
		changed = true
	}
	if upsertPaperMakerQuote(t, "sell", t.Outcomes[0], sellPrice1, sellQty1) {
		changed = true
	}
	if upsertPaperMakerQuote(t, "sell", t.Outcomes[1], sellPrice2, sellQty2) {
		changed = true
	}

	t.LastMakerSync = now
	if changed {
		t.TUI.LogEvent("[%s] 🧾 Maker quotes refreshed: %s buy@$%.3f/ sell@$%.3f | %s buy@$%.3f/ sell@$%.3f",
			t.ID,
			t.Outcomes[0], buyPrice1, sellPrice1,
			t.Outcomes[1], buyPrice2, sellPrice2,
		)
	}
	updatePaperPendingOrders(t)
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("Critical error: %v", err)
	}
}

// logEvent is a helper to log to both TUI and CSV logger safely
func logEvent(tui *paper.TUI, csv *core.CSVLogger, engine *paper.Engine, level, asset, event, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	if tui != nil {
		tui.LogEvent("%s", msg)
	}
	if csv != nil {
		equity := 0.0
		if engine != nil {
			equity = engine.GetEquity()
		}
		csv.Log(level, asset, event, msg, equity)
	}
}

func run() error {
	var engine *paper.Engine
	var tui *paper.TUI
	var csvLogger *core.CSVLogger

	// Setup signal handling with immediate terminal restore
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// emergencyCleanup ensures terminal is restored and positions are handled on crash/exit
	emergencyCleanup := func() {
		core.RestoreTerminal()
		if engine != nil {
			positions := engine.GetPositions()
			if len(positions) > 0 {
				fmt.Println("💰 Emergency: Liquidating all paper positions...")
				proceeds := engine.LiquidateAll()
				fmt.Printf("💵 Liquidation proceeds: $%.2f\n", proceeds)
			}
		}
		if tui != nil {
			tui.CancelAllOrders()
		}
	}

	// Global panic recovery - restore terminal on any panic
	defer func() {
		if r := recover(); r != nil {
			emergencyCleanup()
			stack := make([]byte, 4096)
			length := runtime.Stack(stack, false)
			fmt.Printf("\n🚨 PANIC RECOVERED: %v\n%s\n", r, stack[:length])
			if csvLogger != nil {
				equity := 0.0
				if engine != nil {
					equity = engine.GetEquity()
				}
				csvLogger.Log("CRITICAL", "SYSTEM", "PANIC", fmt.Sprintf("%v", r), equity)
			}
		}
	}()

	// Watchdog: Force exit after signal if graceful shutdown takes too long
	// This ensures we never get stuck even if goroutines are blocked
	go func() {
		<-ctx.Done()
		// Give graceful shutdown 10 seconds, then force exit
		time.Sleep(10 * time.Second)
		core.RestoreTerminal()
		fmt.Println("\n⚠️ Force exit: graceful shutdown timed out")
		os.Exit(1)
	}()

	// Disable terminal echo to prevent arrow keys from appearing
	// This is done via stty which works on most Unix systems including Termux
	disableEcho := exec.Command("stty", "-echo", "-icanon")
	disableEcho.Stdin = os.Stdin
	_ = disableEcho.Run() // Ignore errors if stty not available

	// Ensure terminal is restored on any exit
	defer core.RestoreTerminal()

	startTime := time.Now()

	// Clear screen at startup
	fmt.Print("\033[H\033[2J")
	fmt.Println("🎰 POLYARB-15M Starting (Multi-Asset: BTC, ETH, SOL, XRP)...")
	fmt.Printf("⏰ Started at: %s\n", startTime.Format("2006-01-02 15:04:05"))

	// Initialize persistent components (survive market rotation)
	engine = paper.NewEngine(StartingBalance)

	// Load paperbot settings + env-backed secrets
	cfg, err := core.LoadBotConfig("paperbot")
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	fmt.Println("✅ Config loaded successfully")

	// Apply fee settings to engine
	engine.SetFeeRateBps(cfg.FeeRateBps)

	if cfg.FeeRateBps > 0 {
		// Show effective fee at p=0.50 (worst case for arb)
		// Formula: fee_tokens = shares * base_rate * 2 * p * (1-p)
		// At p=0.50: curve = 0.5, so effective = base_rate * 0.5
		effectiveAt50 := float64(cfg.FeeRateBps) / 10000.0 * 0.5 * 100.0
		fmt.Printf("💰 Fee simulation enabled: %d bps base (~%.1f%% effective at p=0.50)\n", cfg.FeeRateBps, effectiveAt50)
	}

	restClient := api.NewRestClient("")

	// Create shared TUI (persistent across market rotations)
	tui = paper.NewTUI(engine, nil)

	// Seed settings panel from config (.env), so the live panel reflects initial values
	tui.InitSettings(paper.TUISettings{
		MarketSlug:              cfg.MarketSlug,
		MaxMarkets:              cfg.MaxMarkets,
		Timeframe:               cfg.Timeframe,
		TradeScaleFactor:        cfg.TradeScaleFactor,
		MinMarginPercent:        cfg.MinMarginPercent,
		PaperArbMode:            normalizePaperArbMode(cfg.PaperArbMode),
		SplitMinMarginSell:      cfg.SplitMinMarginSell,
		SplitStrategyEnabled:    cfg.SplitStrategyEnabled,
		SplitInitialCapPct:      cfg.SplitInitialCapPct,
		SplitReplenishCapPct:    cfg.SplitReplenishCapPct,
		MakerMergeBufferSeconds: cfg.MakerMergeBufferSeconds,
		MakerQuoteGap:           cfg.MakerQuoteGap,
		MinAskPrice:             cfg.MinAskPrice,
		MaxAskPrice:             cfg.MaxAskPrice,
		MaxTradeSize:            cfg.MaxTradeSize,
		MaxDailyLoss:            cfg.MaxDailyLoss,
	}, func(s paper.TUISettings) {
		cfg.MarketSlug = s.MarketSlug
		cfg.MaxMarkets = s.MaxMarkets
		cfg.Timeframe = s.Timeframe
		cfg.TradeScaleFactor = s.TradeScaleFactor
		cfg.MinMarginPercent = s.MinMarginPercent
		cfg.PaperArbMode = normalizePaperArbMode(s.PaperArbMode)
		cfg.SplitMinMarginSell = s.SplitMinMarginSell
		cfg.SplitStrategyEnabled = s.SplitStrategyEnabled
		cfg.SplitInitialCapPct = s.SplitInitialCapPct
		cfg.SplitReplenishCapPct = s.SplitReplenishCapPct
		cfg.MakerMergeBufferSeconds = s.MakerMergeBufferSeconds
		cfg.MakerQuoteGap = s.MakerQuoteGap
		cfg.MinAskPrice = s.MinAskPrice
		cfg.MaxAskPrice = s.MaxAskPrice
		cfg.MaxTradeSize = s.MaxTradeSize
		cfg.MaxDailyLoss = s.MaxDailyLoss
		_ = cfg.SaveSettings()
	})
	tui.SetTradeFactor(cfg.TradeScaleFactor)

	// Initialize CSV Logger if enabled in config
	if cfg.EnableCSVLogger {
		csvLogger, err = core.NewCSVLogger("bot_activity.csv")
		if err != nil {
			fmt.Printf("⚠️ Warning: Could not initialize CSV logger: %v\n", err)
		} else {
			defer csvLogger.Close()
		}
	}

	// Start TUI render loop — pass stop so a single Ctrl+C / [q] quits cleanly.
	if UseLiveUI {
		tui.StartRenderLoop(250*time.Millisecond, stop)
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

	logEvent(tui, csvLogger, engine, "INFO", "SYSTEM", "STARTUP", "Bot starting with multi-asset support")

	// Goroutine monitor and memory cleanup
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(60 * time.Second):
				count := runtime.NumGoroutine()
				// Only warn if goroutine count is extremely high (likely leak)
				if count > 200 {
					tui.LogEvent("⚠️ High goroutine count: %d", count)
					if csvLogger != nil {
						csvLogger.Log("WARN", "SYSTEM", "HIGH_GOROUTINES", fmt.Sprintf("Count: %d", count), engine.GetEquity())
					}
					runtime.GC()
				}

				// Periodic memory cleanup - remove old filled/cancelled orders
				tui.CleanupOrderBooks(5 * time.Minute)
			}
		}
	}()

	// Android background keepalive - prevents OS from throttling when alt-tabbed
	// Performs lightweight work every 500ms to maintain activity
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Lightweight activity to prevent Android throttling
				// Just reading the time is enough to keep the process active
				_ = time.Now().UnixNano()
			}
		}
	}()

	// Main loop - continuously trade markets and rotate to next when expired
	for {
		select {
		case <-ctx.Done():
			tui.Stop()
			fmt.Println("\n👋 Shutting down...")

			// Run emergency cleanup
			emergencyCleanup()

			stats := engine.GetStats()
			duration := time.Since(startTime).Round(time.Second)
			fmt.Printf("📊 Final Stats: Balance $%.2f | Realized PnL $%.2f | Trades %d | Duration %v\n",
				stats.CurrentBalance, stats.RealizedPnL, stats.TotalTrades, duration)
			return nil
		default:
		}

		// Find all available markets (BTC, ETH, SOL, XRP)
		logEvent(tui, csvLogger, engine, "INFO", "SYSTEM", "MARKET_SEARCH", "Searching for active markets based on live settings...")
		markets := mkt.FindMarkets(ctx, restClient, tui.GetSettings, func(format string, args ...interface{}) {
			tui.LogEvent(format, args...)
		})
		if len(markets) == 0 {
			logEvent(tui, csvLogger, engine, "WARN", "SYSTEM", "NO_MARKETS", "No active markets found, retrying...")
			select {
			case <-ctx.Done():
				tui.Stop()
				fmt.Println("\n👋 Shutting down...")
				stats := engine.GetStats()
				duration := time.Since(startTime).Round(time.Second)
				fmt.Printf("📊 Final Stats: Balance $%.2f | Realized PnL $%.2f | Trades %d | Duration %v\n",
					stats.CurrentBalance, stats.RealizedPnL, stats.TotalTrades, duration)
				return nil
			case <-time.After(2 * time.Second):
			}
			continue
		}

		// Track starting equity for compounding calculation
		startingEquity := engine.GetEquity()
		compoundMultiplier := engine.GetCompoundMultiplier()
		logEvent(tui, csvLogger, engine, "INFO", "SYSTEM", "ROUND_START", "Round starting with %d markets | Multiplier: %.2fx", len(markets), compoundMultiplier)

		// Create a context for this specific round of trading
		roundCtx, roundCancel := context.WithCancel(ctx)

		// Create traders for all found markets
		var wg sync.WaitGroup
		results := make(chan *marketResult, len(markets))
		errors := make(chan error, len(markets))

		tradersStarted := 0
		for assetID, market := range markets {
			// Parse end time and outcomes
			endTime, err := paper.ParseEndTimeFromSlug(market.Slug)
			if err != nil {
				endTime = time.Now().Add(15 * time.Minute)
			}
			outcomes := mkt.GetOutcomes(market)
			tui.AddMarket(assetID, market.Slug, outcomes, endTime)
			// Reduced logging: Only TUI for startup info
			tui.LogEvent("🚀 Trading %s: %s", assetID, market.Slug)

			trader := createTrader(assetID, market, engine, restClient, tui, outcomes, endTime, csvLogger, cfg)
			wg.Add(1)
			tradersStarted++
			go func(id string, t *MarketTrader) {
				defer wg.Done()
				// Create a sub-context for this specific trader to prevent goroutine leaks
				tCtx, tCancel := context.WithCancel(roundCtx)
				defer tCancel()

				// Panic recovery for trader goroutine
				defer func() {
					if r := recover(); r != nil {
						stack := make([]byte, 4096)
						length := runtime.Stack(stack, false)
						logEvent(t.TUI, t.CSVLogger, t.Engine, "CRITICAL", id, "PANIC", "Panic: %v\n%s", r, stack[:length])
						errors <- fmt.Errorf("%s: panic: %v", id, r)
					}
				}()
				result, err := runTrader(tCtx, t)
				if err != nil {
					logEvent(t.TUI, t.CSVLogger, t.Engine, "ERROR", id, "TRADER_ERROR", "Trader failed: %v", err)
					errors <- fmt.Errorf("%s: %w", id, err)
					return
				}
				results <- result
			}(assetID, trader)
		}

		logEvent(tui, csvLogger, engine, "INFO", "SYSTEM", "TRADERS_RUNNING", "Started %d concurrent market traders", tradersStarted)

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

		// Wait for all traders to complete with a context-aware mechanism
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		// Wait for either all traders to finish OR context cancellation
		select {
		case <-done:
			// All traders finished normally
			tui.LogEvent("✅ All %d traders completed", tradersStarted)
		case <-ctx.Done():
			// Context cancelled - give traders 2 seconds to clean up
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				tui.LogEvent("⚠️ Force stopping traders...")
			}
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

		// Close channels safely in a background goroutine AFTER wg.Wait()
		// This prevents deadlocks and panics if traders take a long time to exit
		go func() {
			wg.Wait()
			close(results)
			close(errors)
		}()

		// Collect results
		// Read exact number of expected results to avoid hanging if results channel isn't closed due to stuck trader
		totalPnL := 0.0
		totalTrades := 0
		for i := 0; i < tradersStarted; i++ {
			select {
			case result, ok := <-results:
				if !ok {
					// Channel was closed early (shouldn't happen but safe to check)
					i = tradersStarted
					continue
				}
				if result != nil {
					totalPnL += result.realizedPnL
					totalTrades += result.trades
				}
			case <-time.After(5 * time.Second):
				tui.LogEvent("⚠️ Timed out waiting for some traders to return results")
				// Force break out of the loop
				i = tradersStarted
			}
		}

		// Log market rotation with detailed stats
		stats := engine.GetStats()
		tui.LogEvent("📊 Round PnL: $%.2f | Total Balance: $%.2f | Rotating...", totalPnL, stats.CurrentBalance)

		// Update compounding multiplier based on round performance
		engine.UpdateCompoundMultiplier(totalPnL, startingEquity)
		newMultiplier := engine.GetCompoundMultiplier()
		if totalPnL > 0 {
			tui.LogEvent("📈 PROFIT! Multiplier: %.2fx → %.2fx (compounding)", compoundMultiplier, newMultiplier)
		} else if totalPnL < 0 {
			tui.LogEvent("📉 Loss. Multiplier: %.2fx → %.2fx", compoundMultiplier, newMultiplier)
		}

		// Check for context cancellation
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		// Clear old market data and release stale HTTP connections before
		// the next market-search phase.  Stale HTTP/1.1 keep-alive connections
		// left over from heavy per-trader REST polling can trigger unexpected
		// server responses on reuse; closing them here prevents that.
		tui.LogEvent("🔄 Market round complete, searching for new markets...")
		tui.ClearMarkets()
		tui.CancelAllOrders()
		engine.ClearMarketData()
		restClient.CloseIdleConnections()
	}
}

func createTrader(id string, market *api.Market, engine *paper.Engine, restClient *api.RestClient, tui *paper.TUI, outcomes []string, endTime time.Time, csvLogger *core.CSVLogger, cfg *core.Config) *MarketTrader {
	tokenMap := make(map[string]string)
	for _, token := range market.Tokens {
		tokenMap[token.TokenID] = token.Outcome
	}

	orderBook := paper.NewOrderBook()
	tui.RegisterOrderBook(id, orderBook)

	ladderConfig := paper.LadderConfig{
		Levels:         3,
		SharesPerLevel: 25,
		PriceStep:      0.01,
		BasePrice:      0.0,
	}
	ladderMgr := paper.NewLadderManager(orderBook, ladderConfig)

	riskConfig := paper.RiskConfig{
		DisableKillSwitch:  true,
		MaxExposure:        2000.0,
		MaxUnmatchedRatio:  0.40,
		MaxUnmatchedShares: 300.0,
		SkewThreshold:      0.30,
		KillSwitchDrawdown: 0.10,
	}
	riskMgr := paper.NewRiskManager(riskConfig, engine, orderBook, outcomes)

	monitor := paper.NewMarketMonitor(engine, orderBook, ladderMgr, riskMgr)
	monitor.SetMarket(market.Slug, market.ConditionID, outcomes, endTime)

	splitInv := paper.NewSplitInventory()
	engine.RegisterSplitInventory(splitInv) // Register for equity calculation
	tui.RegisterSplitInventory(splitInv)    // Register for TUI display

	return &MarketTrader{
		ID:               id,
		Market:           market,
		Engine:           engine,
		OrderBook:        orderBook,
		LadderMgr:        ladderMgr,
		RiskMgr:          riskMgr,
		Monitor:          monitor,
		TokenMap:         tokenMap,
		Outcomes:         outcomes,
		EndTime:          endTime,
		RestClient:       restClient,
		TUI:              tui,
		CSVLogger:        csvLogger,
		Config:           cfg,
		TokenBids:        make(map[string]float64),
		TokenAsks:        make(map[string]float64),
		TokenFullBids:    make(map[string][]paper.MarketLevel),
		TokenFullAsks:    make(map[string][]paper.MarketLevel),
		FloatPrices:      make(map[string]float64),
		LastUpdate:       time.Now(),
		LastPairUpdate:   time.Now(),
		LastRestPoll:     time.Now(),
		SplitInventory:   splitInv,
		ReplenishCtrl:    paper.NewReplenishController(),
		SplitInitialized: false,
		LastSplitSell:    time.Time{},
		MakerQuotes:      make(map[string]*paper.LimitOrder),
	}
}

func runTrader(ctx context.Context, t *MarketTrader) (*marketResult, error) {
	// Setup WebSocket with retry
	wsMgr := api.NewWSManager("")
	var wsErr error
	for attempt := 1; attempt <= 3; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		wsErr = wsMgr.Connect(ctx)
		if wsErr == nil {
			break
		}
		logEvent(t.TUI, t.CSVLogger, t.Engine, "WARN", t.ID, "WS_CONNECT_FAIL", "WS connect attempt %d failed: %v", attempt, wsErr)
		if attempt < 3 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
	}
	if wsErr != nil {
		return nil, fmt.Errorf("websocket connect failed after 3 attempts: %w", wsErr)
	}
	defer wsMgr.Close()
	t.WSMgr = wsMgr

	// Safety timeout: based on market end time + 1 minute buffer for resolution
	// This ensures we exit shortly after the market should have resolved
	safetyBuffer := 1 * time.Minute
	traderDeadline := t.EndTime.Add(safetyBuffer)
	timeUntilDeadline := time.Until(traderDeadline)
	t.TUI.LogEvent("[%s] ⏰ Timeout: %v (expires + 1m)", t.ID, timeUntilDeadline.Round(time.Second))

	// Subscribe to Order Books
	var assetIDs []string
	for _, token := range t.Market.Tokens {
		assetIDs = append(assetIDs, token.TokenID)
	}

	sub := map[string]interface{}{
		"type":       "market",
		"assets_ids": assetIDs,
	}
	if err := wsMgr.Subscribe(ctx, sub); err != nil {
		return nil, fmt.Errorf("subscribe failed: %w", err)
	}

	// Start WebSocket streaming in background - REAL-TIME updates via channel
	wsMsgChan := wsMgr.StartStreaming(ctx)
	t.TUI.LogEvent("[%s] 📡 WebSocket streaming started", t.ID)

	// Order fill callback
	t.OrderBook.SetFillCallback(func(order *paper.LimitOrder, fillQty, fillPrice float64) {
		status := "PARTIAL"
		if order.Status == paper.OrderStatusFilled {
			status = "FILLED"
		}

		switch order.Side {
		case "buy":
			trade, err := t.Engine.MakerBuyForMarket(t.ID, order.Outcome, fillPrice, fillQty)
			if err != nil {
				t.TUI.LogEvent("[%s] ❌ BUY fill error: %v", t.ID, err)
				if t.CSVLogger != nil {
					t.CSVLogger.Log("ERROR", t.ID, "BUY_FILL_ERROR", err.Error(), t.Engine.GetEquity())
				}
				return
			}
			cost := fillQty * fillPrice
			if trade != nil {
				cost = trade.Value
			}
			saved := order.Price - fillPrice
			t.TUI.LogEvent("[%s] ✅ BUY FILL %s %.0f @ $%.3f (saved $%.3f)", t.ID, order.Outcome, fillQty, fillPrice, saved)
			t.TUI.RecordOrder(t.ID, order.Outcome, "BUY", fillQty, fillPrice, cost, 0.0, 0.0, status)
			if t.CSVLogger != nil {
				t.CSVLogger.Log("TRADE", t.ID, "BUY_FILL", fmt.Sprintf("%s %.0f @ $%.3f", order.Outcome, fillQty, fillPrice), t.Engine.GetEquity())
			}
		case "sell":
			positions := t.Engine.GetPositions()
			pos, ok := getPaperMarketPosition(positions, t.ID, order.Outcome)
			avgCost := 0.0
			if ok {
				avgCost = pos.AvgPrice
			}
			trade, err := t.Engine.MakerSellForMarket(t.ID, order.Outcome, fillPrice, fillQty)
			if err != nil {
				t.TUI.LogEvent("[%s] ❌ SELL fill error: %v", t.ID, err)
				if t.CSVLogger != nil {
					t.CSVLogger.Log("ERROR", t.ID, "SELL_FILL_ERROR", err.Error(), t.Engine.GetEquity())
				}
				return
			}
			proceeds := fillQty * fillPrice
			if trade != nil {
				proceeds = trade.Value
			}
			profit := proceeds - (avgCost * fillQty)
			t.TUI.LogEvent("[%s] ✅ SELL FILL %s %.0f @ $%.3f (pnl $%.2f)", t.ID, order.Outcome, fillQty, fillPrice, profit)
			t.TUI.RecordOrder(t.ID, order.Outcome, "SELL", fillQty, fillPrice, proceeds, 0.0, profit, status)
			if t.CSVLogger != nil {
				t.CSVLogger.Log("TRADE", t.ID, "SELL_FILL", fmt.Sprintf("%s %.0f @ $%.3f | pnl=%.2f", order.Outcome, fillQty, fillPrice, profit), t.Engine.GetEquity())
			}
		}

		if order.Status == paper.OrderStatusFilled || order.Status == paper.OrderStatusCancelled {
			clearPaperMakerQuoteReference(t, order)
		}
		updatePaperPendingOrders(t)
	})

	// Track starting realized PnL
	startingRealizedPnL := t.Engine.GetStats().RealizedPnL
	tradesAtStart := t.Engine.GetStats().TotalTrades

	tokenPrices := make(map[string]string)
	lastReconnectCount := int32(0)    // Track reconnections
	lastWsWarnTime := time.Time{}     // Rate-limit WS warnings
	lastForceReconnect := time.Time{} // Track forced reconnection attempts
	lastTrade := time.Time{}          // Prevent trade spam

	const wsWarnInterval = 10 * time.Second   // Only warn once per 10 seconds
	const wsForceReconnect = 10 * time.Second // Force reconnection after 10 seconds stale

	// Track WebSocket channel closure state (outside loop to persist across ticks)
	wsChannelClosed := false

	for {
		select {
		case <-ctx.Done():
			cancelAllPaperMakerQuotes(t, "trader shutting down")
			t.OrderBook.CancelAllOrders()
			t.LadderMgr.CancelAllLadders()
			positions := t.Engine.GetPositions()
			if len(positions) > 0 {
				t.TUI.LogEvent("[%s] 🔴 EMERGENCY EXIT: Liquidating positions...", t.ID)
				t.Engine.LiquidateAll()
			}
			// Liquidate split inventory
			splitPositions := t.SplitInventory.GetAllPositions()
			if len(splitPositions) > 0 {
				t.TUI.LogEvent("[%s] 🔀 EMERGENCY EXIT: Merging & Liquidating Split Inventory...", t.ID)
				// For a 2-outcome market, we just merge min shares, then sell the rest at bid.
				if len(t.Outcomes) == 2 {
					minShares := t.SplitInventory.GetMinSplitShares(t.ID, t.Outcomes[0], t.Outcomes[1])
					if minShares > 0 {
						t.Engine.MergeForMarket(t.ID, t.Outcomes[0], t.Outcomes[1], minShares)
						t.SplitInventory.RecordMerge(t.ID, t.Outcomes[0], t.Outcomes[1], minShares)
						t.TUI.LogEvent("[%s] 🔀 Merged %.0f pairs", t.ID, minShares)
					}
					// Sell remaining unbalanced shares at current bid
					for _, out := range t.Outcomes {
						rem := t.SplitInventory.GetSplitShares(t.ID, out)
						if rem > 0 {
							bid, _ := t.Engine.GetMarketBidAsk(t.ID, out)
							if bid <= 0 { // Fallback
								bid = 0.50
							}

							feeUsdc := 0.0
							if t.Config.FeeRateBps > 0 {
								feeUsdc = rem * 0.25 * math.Pow(bid*(1.0-bid), 2.0) * bid
							}

							profit := t.SplitInventory.RecordSell(t.ID, out, rem, bid)
							t.Engine.AddRealizedPnL(profit - feeUsdc)
							t.Engine.AddBalance((rem * bid) - feeUsdc)
							t.TUI.LogEvent("[%s] 📉 Sold %.0f split shares of %s at $%.3f", t.ID, rem, out, bid)
						}
					}
				}
			}
			return nil, ctx.Err()

		default:
			// Check safety timeout - force exit if trader runs too long
			if time.Now().After(traderDeadline) {
				logEvent(t.TUI, t.CSVLogger, t.Engine, "WARN", t.ID, "TIMEOUT", "SAFETY TIMEOUT - Forcing market exit")
				cancelAllPaperMakerQuotes(t, "safety timeout")
				t.OrderBook.CancelAllOrders()
				t.LadderMgr.CancelAllLadders()

				// Use more robust resolution simulation
				winner := t.determineWinner()
				if winner != "" {
					logEvent(t.TUI, t.CSVLogger, t.Engine, "INFO", t.ID, "TIMEOUT_RESOLVE", "Timeout resolution: %s", winner)
					t.Engine.RedeemWithDetails(t.ID, winner)
				}

				finalStats := t.Engine.GetStats()
				return &marketResult{
					realizedPnL: finalStats.RealizedPnL - startingRealizedPnL,
					trades:      finalStats.TotalTrades - tradesAtStart,
				}, nil
			}

			// Check kill switch - DON'T EXIT, just pause trading
			// Exiting would leave positions unmatched; better to hold until expiration
			killSwitchActive := t.RiskMgr.IsKillSwitchTriggered()
			if killSwitchActive {
				// Log once per state change, then just skip trading
				t.TUI.SetKillSwitch("Risk limits exceeded - pausing trades")
			}

			// Check market state
			marketState := t.Monitor.CheckState()

			// Handle market ending
			timeToEnd := time.Until(t.EndTime)
			isExpired := timeToEnd <= 0

			if isExpired && !t.MarketEnded {
				t.MarketEnded = true
				logEvent(t.TUI, t.CSVLogger, t.Engine, "INFO", t.ID, "EXPIRED", "MARKET EXPIRED - resolving immediately")

				// Use more robust resolution simulation
				winner := t.determineWinner()
				logEvent(t.TUI, t.CSVLogger, t.Engine, "INFO", t.ID, "WINNER", "WINNER: %s", winner)

				// Use detailed redemption
				result := t.Engine.RedeemWithDetails(t.ID, winner)
				if result.WinningShares > 0 || result.LosingShares > 0 {
					t.TUI.LogEvent("[%s] 💰 WIN: %.0f shares → $%.2f (profit: $%.2f)",
						t.ID, result.WinningShares, result.WinningPayout, result.WinningPnL)
					if result.LosingShares > 0 {
						t.TUI.LogEvent("[%s] 💀 LOSS: %.0f shares → $0 (lost: $%.2f)",
							t.ID, result.LosingShares, result.LosingCost)
					}
					pnlSign := "+"
					pnlColor := "🟢"
					if result.TotalPnL < 0 {
						pnlSign = ""
						pnlColor = "🔴"
					}
					t.TUI.LogEvent("[%s] %s NET PnL: %s$%.2f", t.ID, pnlColor, pnlSign, result.TotalPnL)
				} else {
					t.TUI.LogEvent("[%s] 📭 No positions to redeem", t.ID)
				}

				if t.CSVLogger != nil {
					t.CSVLogger.Log("INFO", t.ID, "REDEEM", fmt.Sprintf("Winner: %s, PnL: %.2f", winner, result.TotalPnL), t.Engine.GetEquity())
				}

				finalStats := t.Engine.GetStats()
				marketPnL := finalStats.RealizedPnL - startingRealizedPnL

				return &marketResult{
					realizedPnL: marketPnL,
					trades:      finalStats.TotalTrades - tradesAtStart,
				}, nil
			}

			if marketState == paper.MarketStateEnding && !t.MarketEnded {
				if !t.LaddersPlaced {
					t.TUI.LogEvent("[%s] ⏳ Market ending in %v...", t.ID, timeToEnd.Round(time.Second))
					t.LaddersPlaced = true
				}
			}

			// ============ WEBSOCKET-ONLY PRICE UPDATES (FASTEST) ============
			// Check for WebSocket reconnection and log it
			_, _, reconnects, _ := wsMgr.GetStats()
			if reconnects > lastReconnectCount {
				t.TUI.LogEvent("[%s] 🔄 WebSocket reconnected (attempt #%d)", t.ID, reconnects)
				if t.CSVLogger != nil {
					t.CSVLogger.Log("INFO", t.ID, "WS_RECONNECT", fmt.Sprintf("Attempt #%d", reconnects), t.Engine.GetEquity())
				}
				lastReconnectCount = reconnects
			}

			// Process ALL available WebSocket messages (real-time, non-blocking)
			// This drains the channel to get the latest data
			messagesProcessed := 0
			for {
				select {
				case msg, ok := <-wsMsgChan:
					if !ok {
						// Channel closed - check if context cancelled or unexpected
						select {
						case <-ctx.Done():
							// Context cancelled, normal shutdown
							return nil, ctx.Err()
						default:
							// Unexpected close, mark for reconnect attempt
							wsChannelClosed = true
							goto doneProcessingWS
						}
					}
					messagesProcessed++

					// Parse and process WebSocket message immediately.
					//
					// Polymarket CLOB WS sends two message shapes:
					//   1. Book snapshot  – array of objects with "bids"/"asks" fields
					//      (event_type:"book").  Sent on subscribe and after reconnect.
					//   2. Price-change delta – single object with "price_changes" array
					//      (event_type:"price_change").  Contains only changed levels
					//      but NO size information, so we cannot update the full book
					//      from them.  We apply the best-bid/ask update from the delta
					//      and rely on the 4 ms REST poll for full-depth accuracy.
					//
					// IMPORTANT: only write to TokenBids/TokenAsks when the parsed
					// value is strictly positive — a zero value means "no orders on
					// that side in this message" and must NOT overwrite a previously
					// valid price from REST or an earlier WS snapshot.
					if books, err := api.ParseOrderBooks(msg); err == nil && len(books) > 0 && books[0].AssetID != "" {
						// ── Book snapshot (array) ──────────────────────────────
						foundForThisTrader := false
						for _, b := range books {
							bid, ask := 0.0, 0.0
							for _, order := range b.Bids {
								p, _ := strconv.ParseFloat(order.Price, 64)
								if p > 0 && p < 1.0 && p > bid {
									bid = p
								}
							}
							for _, order := range b.Asks {
								p, _ := strconv.ParseFloat(order.Price, 64)
								if p > 0 && p < 1.0 && (ask == 0 || p < ask) {
									ask = p
								}
							}

							outcome := t.TokenMap[b.AssetID]
							if outcome != "" {
								foundForThisTrader = true
								t.mu.Lock()

								// WS Snapshot is absolute state.
								if bid > 0 && ask > 0 && bid >= ask {
									// Reject crossed snapshot and clear state
									t.TokenBids[outcome] = 0
									t.TokenAsks[outcome] = 0
									t.TokenFullBids[outcome] = nil
									t.TokenFullAsks[outcome] = nil
									t.mu.Unlock()
									continue
								}

								t.TokenBids[outcome] = bid
								t.TokenAsks[outcome] = ask

								if bid > 0 && ask > 0 {
									mid := (bid + ask) / 2
									t.FloatPrices[outcome] = mid
									tokenPrices[outcome] = fmt.Sprintf("%.3f", mid)
									t.Engine.UpdateMarketData(t.ID, outcome, mid, bid, ask)
								}
								// Always update full depth from snapshots — REST will
								// keep this fresh on the 4 ms poll as well.
								t.TokenFullBids[outcome] = mkt.LevelsToPriceDepth(b.Bids, true)
								t.TokenFullAsks[outcome] = mkt.LevelsToPriceDepth(b.Asks, false)
								t.mu.Unlock()
							}
						}
						if foundForThisTrader {
							now := time.Now()
							t.mu.Lock()
							t.LastUpdate = now
							syncPaperPairUpdate(t, now)
							t.mu.Unlock()
						}
					} else if update, err := api.ParsePriceUpdate(msg); err == nil && len(update.PriceChanges) > 0 {
						// ── Price-change delta ─────────────────────────────────
						foundForThisTrader := false

						t.mu.Lock()
						for _, pc := range update.PriceChanges {
							outcome := t.TokenMap[pc.AssetID]
							if outcome == "" {
								continue
							}
							foundForThisTrader = true
							p, errP := strconv.ParseFloat(pc.Price, 64)
							s, errS := strconv.ParseFloat(pc.Size, 64)
							if errP != nil || errS != nil || p <= 0 {
								continue
							}

							switch pc.Side {
							case "BUY":
								t.TokenFullBids[outcome] = mkt.ApplyDelta(t.TokenFullBids[outcome], p, s, true)
							case "SELL":
								t.TokenFullAsks[outcome] = mkt.ApplyDelta(t.TokenFullAsks[outcome], p, s, false)
							}
						}

						for outcome := range t.TokenMap {
							bids := t.TokenFullBids[outcome]
							if len(bids) > 0 {
								t.TokenBids[outcome] = bids[0].Price
							} else {
								t.TokenBids[outcome] = 0
							}

							asks := t.TokenFullAsks[outcome]
							if len(asks) > 0 {
								t.TokenAsks[outcome] = asks[0].Price
							} else {
								t.TokenAsks[outcome] = 0
							}

							if t.TokenBids[outcome] > 0 && t.TokenAsks[outcome] > 0 {
								// Check for crossed book
								if t.TokenBids[outcome] >= t.TokenAsks[outcome] {
									t.LastUpdate = time.Now().Add(-20 * time.Second)
									t.TokenBids[outcome] = 0
									t.TokenAsks[outcome] = 0
									t.TokenFullBids[outcome] = nil
									t.TokenFullAsks[outcome] = nil
									continue
								}

								mid := (t.TokenBids[outcome] + t.TokenAsks[outcome]) / 2
								t.FloatPrices[outcome] = mid
								tokenPrices[outcome] = fmt.Sprintf("%.3f", mid)
								t.Engine.UpdateMarketData(t.ID, outcome, mid, t.TokenBids[outcome], t.TokenAsks[outcome])
							}
						}
						now := time.Now()
						if foundForThisTrader {
							t.LastUpdate = now
							syncPaperPairUpdate(t, now)
						}
						t.mu.Unlock()
					} else if book, err := api.ParseOrderBook(msg); err == nil && book.AssetID != "" {
						// ── Book snapshot (single object) ──────────────────────
						bid, ask := 0.0, 0.0
						for _, b := range book.Bids {
							p, _ := strconv.ParseFloat(b.Price, 64)
							if p > 0 && p < 1.0 && p > bid {
								bid = p
							}
						}
						for _, order := range book.Asks {
							p, _ := strconv.ParseFloat(order.Price, 64)
							if p > 0 && p < 1.0 && (ask == 0 || p < ask) {
								ask = p
							}
						}

						if bid > 0 && ask > 0 && bid >= ask {
							continue // Reject crossed snapshot
						}

						outcome := t.TokenMap[book.AssetID]
						if outcome != "" {
							t.mu.Lock()
							now := time.Now()
							t.LastUpdate = now
							// Guard: only persist valid (non-zero) prices.
							if bid > 0 {
								t.TokenBids[outcome] = bid
							}
							if ask > 0 {
								t.TokenAsks[outcome] = ask
							}
							if bid > 0 && ask > 0 {
								mid := (bid + ask) / 2
								t.FloatPrices[outcome] = mid
								tokenPrices[outcome] = fmt.Sprintf("%.3f", mid)
								t.Engine.UpdateMarketData(t.ID, outcome, mid, bid, ask)
							}
							t.TokenFullBids[outcome] = mkt.LevelsToPriceDepth(book.Bids, true)
							t.TokenFullAsks[outcome] = mkt.LevelsToPriceDepth(book.Asks, false)
							syncPaperPairUpdate(t, now)
							t.mu.Unlock()
						}
					}
				default:
					// No more messages in channel, continue with rest of loop
					goto doneProcessingWS
				}
			}
		doneProcessingWS:

			// Final safety check: scrub any crossed books that survived the WS processing loop
			t.mu.Lock()
			for outcome := range t.TokenMap {
				if t.TokenBids[outcome] > 0 && t.TokenAsks[outcome] > 0 && t.TokenBids[outcome] >= t.TokenAsks[outcome] {
					t.TokenBids[outcome] = 0
					t.TokenAsks[outcome] = 0
					t.TokenFullBids[outcome] = nil
					t.TokenFullAsks[outcome] = nil
					t.LastUpdate = time.Now().Add(-20 * time.Second) // Force REST poll
				}
			}
			t.mu.Unlock()

			// Update TUI after processing WS messages
			if messagesProcessed > 0 {
				t.TUI.UpdateMarketPricesWithSourceAt(t.ID, t.TokenBids, t.TokenAsks, "WS", t.LastPairUpdate)
			}
			// NOTE: Removed TouchMarket call - WS connection being "alive" doesn't mean
			// data is fresh. WS often doesn't send liquidity updates, so we should only
			// update LastUpdate when we actually receive new price/liquidity data.
			// This ensures the UI accurately shows data staleness.

			// Check WebSocket connection health for REST fallback decision
			wsConnected := wsMgr.IsConnected()
			wsLastMsg := wsMgr.TimeSinceLastMessage()

			// Update WS staleness and ping latency in TUI
			t.TUI.UpdateWSLatency(wsLastMsg)
			t.TUI.UpdateWSPingLatency(wsMgr.PingLatency())

			// ============ REST FALLBACK ============
			// WS is primary for liquidity data via full depth updates and deltas.
			// Only poll REST if WS is unhealthy or stale.
			now := time.Now()
			pairQuoteAge := paperPairQuoteAge(t.LastPairUpdate, now)

			// Update WS staleness and ping latency in TUI
			t.TUI.UpdateWSLatency(wsLastMsg)
			t.TUI.UpdateWSPingLatency(wsMgr.PingLatency())
			localQuoteMaxAge := core.ResolveExecutionLocalQuoteMaxAge(t.Config)
			localPairFresh := shouldUseLocalPaperPair(t.Outcomes, t.TokenBids, t.TokenAsks, t.LastPairUpdate, localQuoteMaxAge, now)
			restFallbackQuoteAge := core.ResolveRestFallbackQuoteAge(t.Config)
			restFallbackPollInterval := core.ResolveRestFallbackPollInterval(t.Config)

			forceRestFallback := !localPairFresh && pairQuoteAge > restFallbackQuoteAge

			wsUnhealthy := !wsConnected || wsLastMsg > 10*time.Second
			if forceRestFallback || wsUnhealthy || shouldPaperRestFallback(pairQuoteAge, time.Since(t.LastRestPoll), restFallbackQuoteAge, restFallbackPollInterval) {
				t.handleRestFallback(ctx, tokenPrices, pairQuoteAge)
			}

			// FORCE RECONNECT: If stale for 10s for faster recovery
			if pairQuoteAge > 10*time.Second {
				if time.Since(lastForceReconnect) > wsForceReconnect {
					t.TUI.LogEvent("[%s] ⚠️ STALE (10s) - forcing WS reconnect", t.ID)
					lastForceReconnect = time.Now()
					wsMgr.ForceReconnect()
				}
			}

			// Handle WebSocket issues - only reconnect if actually disconnected
			// (Inactive markets with no trades won't send data, but connection is still alive)
			if !wsMgr.IsConnected() && !wsChannelClosed {
				// WebSocket disconnected, force reconnection (rate-limited)
				if time.Since(lastForceReconnect) > wsForceReconnect {
					lastForceReconnect = time.Now()
					wsMgr.ForceReconnect()
					if time.Since(lastWsWarnTime) > wsWarnInterval {
						t.TUI.LogEvent("[%s] 🔌 WS disconnected - reconnecting...", t.ID)
						lastWsWarnTime = time.Now()
					}
				}
			}

			// If WebSocket channel closed, log once and try reconnect
			if wsChannelClosed && time.Since(lastWsWarnTime) > wsWarnInterval {
				t.TUI.LogEvent("[%s] ⚠️ WebSocket closed - attempting reconnect", t.ID)
				lastWsWarnTime = time.Now()
				wsMgr.ForceReconnect()
			}

			// Also update order book depth for live display
			bidDepth := make(map[string][]paper.MarketLevel)
			askDepth := make(map[string][]paper.MarketLevel)

			t.mu.Lock()
			// Map current trader's depth data for TUI
			for _, outcome := range t.Outcomes {
				if bids, ok := t.TokenFullBids[outcome]; ok {
					bidDepth[outcome] = append([]paper.MarketLevel(nil), bids...)
				}
				if asks, ok := t.TokenFullAsks[outcome]; ok {
					askDepth[outcome] = append([]paper.MarketLevel(nil), asks...)
				}
			}
			t.mu.Unlock()

			t.TUI.UpdateOrderBookDepth(t.ID, bidDepth, askDepth)

			// Process order fills
			t.mu.Lock()
			for outcome := range tokenPrices {
				bids := t.TokenFullBids[outcome]
				asks := t.TokenFullAsks[outcome]
				if len(bids) > 0 || len(asks) > 0 {
					bidsCopy := append([]paper.MarketLevel(nil), bids...)
					asksCopy := append([]paper.MarketLevel(nil), asks...)
					t.OrderBook.ProcessPriceUpdate(outcome, bidsCopy, asksCopy)
				}
			}
			t.mu.Unlock()

			// Check if market has ended (only exit condition that matters)
			// DON'T exit on "liquidity dried up" - volatile markets can have extreme prices
			// and that's normal market behavior, not a reason to exit

			// Trading logic - check every tick for arbitrage opportunities
			liveCfg := t.TUI.GetSettings()
			arbMode := normalizePaperArbMode(liveCfg.PaperArbMode)
			localQuoteMaxAge = core.ResolveExecutionLocalQuoteMaxAge(t.Config)
			localPairFresh = shouldUseLocalPaperPair(t.Outcomes, t.TokenBids, t.TokenAsks, t.LastPairUpdate, localQuoteMaxAge, time.Now())
			if arbMode != paperArbModeMaker {
				cancelAllPaperMakerQuotes(t, "maker mode disabled")
			} else if marketState != paper.MarketStateActive || len(tokenPrices) != 2 || len(t.Outcomes) != 2 {
				cancelAllPaperMakerQuotes(t, "market not active for maker quoting")
			}
			if len(tokenPrices) == 2 && len(t.Outcomes) == 2 && marketState == paper.MarketStateActive {
				if !localPairFresh {
					if arbMode == paperArbModeMaker {
						cancelAllPaperMakerQuotes(t, "waiting for fresh pair quotes")
					}
					continue
				}
				if arbMode == paperArbModeMaker {
					if killSwitchActive {
						cancelAllPaperMakerQuotes(t, "risk pause active")
						continue
					}
					maintainPaperMakerInventoryQuotes(t, time.Now())
					continue
				}
				ask1 := t.TokenAsks[t.Outcomes[0]]
				ask2 := t.TokenAsks[t.Outcomes[1]]

				// Read live price-range filter from settings panel (adjustable at runtime)
				minAsk := liveCfg.MinAskPrice
				maxAsk := liveCfg.MaxAskPrice

				if ask1 >= minAsk && ask1 <= maxAsk && ask2 >= minAsk && ask2 <= maxAsk {
					sum := ask1 + ask2
					margin := (1.0 - sum) * 100

					// Skip trading if kill switch is active (but don't exit - wait for expiration)
					if killSwitchActive {
						continue
					}

					// Use config for minimum margin (default 2%)
					minMarginPercent := t.Config.MinMarginPercent

					// Calculate dynamic trade size based on EQUITY (not just cash)
					// This ensures consistent sizing regardless of how much is in positions
					// $100 equity * 5% = $5 trade size (even if only $10 is cash)
					currentEquity := t.Engine.GetEquity()
					currentCash := t.Engine.GetBalance()
					tradeSize := t.Config.CalculateTradeSize(currentEquity)
					baseSharesPerTrade := tradeSize / sum // Shares = $ / price per share pair

					// Evaluate portfolio risk before trading
					riskAction, riskReason := t.RiskMgr.Evaluate()
					if riskAction == paper.RiskActionKillSwitch {
						t.TUI.LogEvent("[%s] 🛑 RISK: Kill switch - %s (pausing, not exiting)", t.ID, riskReason)
						continue
					}
					if riskAction == paper.RiskActionReduceSize {
						t.TUI.LogEvent("[%s] ⚠️ RISK: Reducing size - %s", t.ID, riskReason)
						// Will use baseShares only (no scaling)
					}

					if time.Since(lastTrade) <= 2*time.Second {
						// Cooldown - don't spam logs, just skip silently
						continue
					}

					if margin >= minMarginPercent-1e-4 && t.RiskMgr.CanPlaceOrder(baseSharesPerTrade*(ask1+ask2)) {
						baseShares := baseSharesPerTrade

						// AGGREGATED LIQUIDITY: Calculate total matched liquidity across ALL price levels
						// that maintain minimum margin. This allows "chasing" liquidity deeper into the book.
						maxSum := 1.0 - (minMarginPercent / 100.0) // e.g., 2% margin → max sum = 0.98

						// Copy and sort asks by price ascending for both outcomes
						asks1 := make([]paper.MarketLevel, len(t.TokenFullAsks[t.Outcomes[0]]))
						copy(asks1, t.TokenFullAsks[t.Outcomes[0]])
						// Inject BBO if missing due to orderbook lag
						hasAsk1 := false
						for _, a := range asks1 {
							if a.Price <= ask1+1e-6 {
								hasAsk1 = true
								break
							}
						}
						if !hasAsk1 {
							asks1 = append(asks1, paper.MarketLevel{Price: ask1, Size: baseShares})
						}
						sort.Slice(asks1, func(i, j int) bool { return asks1[i].Price < asks1[j].Price })

						asks2 := make([]paper.MarketLevel, len(t.TokenFullAsks[t.Outcomes[1]]))
						copy(asks2, t.TokenFullAsks[t.Outcomes[1]])
						hasAsk2 := false
						for _, a := range asks2 {
							if a.Price <= ask2+1e-6 {
								hasAsk2 = true
								break
							}
						}
						if !hasAsk2 {
							asks2 = append(asks2, paper.MarketLevel{Price: ask2, Size: baseShares})
						}
						sort.Slice(asks2, func(i, j int) bool { return asks2[i].Price < asks2[j].Price })

						// Calculate aggregated matched liquidity across valid price levels
						var totalMatchedLiquidity float64
						var rawLiq1, rawLiq2 float64 // Track actual liquidity on each side for display
						var maxValidI, maxValidJ int // Track deepest valid level on each side price level combinations were valid

						i, j := 0, 0
						for i < len(asks1) && j < len(asks2) {
							// Current prices at each pointer
							p1 := asks1[i].Price
							p2 := asks2[j].Price

							// Check if this combination maintains minimum margin
							if p1+p2 > maxSum+1e-6 {
								break // Can't go deeper, would exceed margin threshold
							}

							// Get liquidity at current levels
							levelLiq1 := asks1[i].Size
							levelLiq2 := asks2[j].Size

							// Track deepest valid level on each side (only count once per level)
							if i+1 > maxValidI {
								maxValidI = i + 1
								rawLiq1 += asks1[i].Size
							}
							if j+1 > maxValidJ {
								maxValidJ = j + 1
								rawLiq2 += asks2[j].Size
							}

							// Get liquidity at current levels (may be partial after matching)

							// Matched liquidity = min of both sides (arbitrage requires equal shares)
							matchedAtLevel := levelLiq1
							if levelLiq2 < matchedAtLevel {
								matchedAtLevel = levelLiq2
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
							// If both exhausted at same time, both pointers already incremented
						}

						// Use RAW liquidity for display (shows actual available on each side)
						liq1 := rawLiq1
						liq2 := rawLiq2
						minLiquidity := totalMatchedLiquidity
						bookDepth1 := len(t.TokenFullAsks[t.Outcomes[0]])
						bookDepth2 := len(t.TokenFullAsks[t.Outcomes[1]])

						// Use 100% of matched liquidity - force MarketBuy guarantees atomic fills on both sides
						// No legging risk since we walk the book simultaneously, not single-level limit orders
						maxSafeShares := minLiquidity * 1.00

						// Only scale if risk allows
						shares := baseShares
						if t.Config.EnableMarginAggression && riskAction != paper.RiskActionReduceSize {
							multiplier := math.Floor(margin)
							if multiplier > t.Config.MaxAggressionMultiplier {
								multiplier = t.Config.MaxAggressionMultiplier
							}
							if multiplier < 1 {
								multiplier = 1
							}
							shares = baseShares * multiplier
						}

						// Apply compounding multiplier from profitable rounds
						compoundMult := t.Engine.GetCompoundMultiplier()
						shares = shares * compoundMult

						// Force at least 1 share if there's any matched liquidity and we have budget
						if shares < 1.0 && minLiquidity >= 1.0 {
							shares = 1.0
						}

						// FINAL LIQUIDITY CAP: Ensure shares never exceed available matched liquidity
						// This must be checked AFTER all scaling (margin scaling + compounding)
						if shares > maxSafeShares {
							shares = maxSafeShares
						}

						// --- PRE-CALCULATE ACTUAL COST BY WALKING THE BOOK ---
						// Instead of assuming all shares fill at the top-of-book price (ask1/ask2),
						// we simulate walking the orderbook to find the TRUE cost of this trade.
						trueCost := 0.0

						// Helper to calculate cost for one side
						calcSideCost := func(qty float64, asks []paper.MarketLevel) float64 {
							c := 0.0
							rem := qty
							for _, lv := range asks {
								if lv.Size <= 0 {
									continue
								}
								take := math.Min(rem, lv.Size)
								c += take * lv.Price
								rem -= take
								if rem <= 0.0001 {
									break
								}
							}
							return c
						}

						trueCost1 := calcSideCost(shares, asks1)
						trueCost2 := calcSideCost(shares, asks2)
						trueCost = trueCost1 + trueCost2

						// If the true cost exceeds our cash, scale DOWN the shares exactly
						// to what we can afford, ensuring we never hit the "Insufficient balance" error.
						if trueCost > currentCash {
							// If we can't afford it, scale the shares down proportionally
							scaleFactor := currentCash / trueCost
							shares = shares * scaleFactor

							// Recalculate true cost with the new, smaller share size
							trueCost1 = calcSideCost(shares, asks1)
							trueCost2 = calcSideCost(shares, asks2)
							trueCost = trueCost1 + trueCost2
						}

						// Calculate true net profit based on actual curve fees and instant merge logic
						feeRateBps := t.Config.FeeRateBps
						calcNetProfit := func(s, tc1, tc2, tc float64) float64 {
							if s <= 0 {
								return 0
							}
							avgP1 := tc1 / s
							avgP2 := tc2 / s
							feeT1, feeT2 := 0.0, 0.0
							if feeRateBps > 0 {
								feeT1 = s * 0.25 * math.Pow(avgP1*(1.0-avgP1), 2.0)
								feeT2 = s * 0.25 * math.Pow(avgP2*(1.0-avgP2), 2.0)
							}
							return math.Min(s-feeT1, s-feeT2) - tc
						}

						netProfit := calcNetProfit(shares, trueCost1, trueCost2, trueCost)
						cost := trueCost

						if !t.RiskMgr.CanPlaceOrder(cost) || cost > currentCash {
							// Scale back to what cash allows, but still respect liquidity cap
							maxAffordableShares := currentCash / sum

							// Apply the stricter of: cash limit OR liquidity limit
							if maxAffordableShares > maxSafeShares {
								maxAffordableShares = maxSafeShares
							}

							if maxAffordableShares < 1 {
								continue // Not enough cash/liquidity for even 1 share
							}
							shares = maxAffordableShares

							trueCost1 = calcSideCost(shares, asks1)
							trueCost2 = calcSideCost(shares, asks2)
							trueCost = trueCost1 + trueCost2
							cost = trueCost

							netProfit = calcNetProfit(shares, trueCost1, trueCost2, trueCost)

							// If still over risk limit or over cash, don't trade
							if !t.RiskMgr.CanPlaceOrder(cost) || cost > currentCash {
								continue
							}
						}
						if compoundMult > 1.0 {
							t.TUI.LogEvent("[%s] 🎯 ARB! %s@$%.2f + %s@$%.2f = $%.2f | %.0f shares (%.1fx), profit $%.2f (%.1f%%) [liq: %.0f/%.0f, levels used: %d/%d (total depth: %d/%d)]",
								t.ID, t.Outcomes[0], ask1, t.Outcomes[1], ask2, sum, shares, compoundMult, netProfit, margin, liq1, liq2, maxValidI, maxValidJ, bookDepth1, bookDepth2)
						} else {
							t.TUI.LogEvent("[%s] 🎯 ARB! %s@$%.2f + %s@$%.2f = $%.2f | %.0f shares ($%.0f), profit $%.2f (%.1f%%) [liq: %.0f/%.0f, levels used: %d/%d (total depth: %d/%d)]",
								t.ID, t.Outcomes[0], ask1, t.Outcomes[1], ask2, sum, shares, cost, netProfit, margin, liq1, liq2, maxValidI, maxValidJ, bookDepth1, bookDepth2)
						}
						latency := paperExecutionLatency{
							detectedAt:  time.Now(),
							opportunity: "paper buy-arb",
							marketID:    t.ID,
							shares:      shares,
							marginPct:   margin,
							expectedPnL: netProfit,
						}

						if t.CSVLogger != nil {
							t.CSVLogger.Log("TRADE", t.ID, "ARB_ENTRY", fmt.Sprintf("Sum: %.3f, Shares: %.0f, Margin: %.1f%%", sum, shares, margin), t.Engine.GetEquity())
						}

						// FORCE FILL: Use MarketBuy to "walk the book" and guarantee fills
						// This ensures both sides fill completely, avoiding legging risk
						// Use the previously generated asks1 and asks2 slices which ALREADY contain
						// the injected BBO if it was missing due to orderbook lag.
						freshAsks1 := make([]paper.MarketLevel, len(asks1))
						copy(freshAsks1, asks1)

						freshAsks2 := make([]paper.MarketLevel, len(asks2))
						copy(freshAsks2, asks2)

						// Execute market orders that consume liquidity across multiple levels
						// Force fill: walks the book atomically to guarantee execution without legging
						latency.startedAt = time.Now()
						trade1, trade2, avgPrice1, avgPrice2, err := t.Engine.MarketBuyArb(t.ID, t.Outcomes[0], t.Outcomes[1], shares, freshAsks1, freshAsks2)
						if err != nil {
							// Concurrency edge case: another market consumed the cash between our check and execution.
							// Fail gracefully without recording bogus $0 trades.
							t.TUI.LogEvent("[%s] ⚠️ Trade failed during execution (TOCTOU / Insufficient balance): %v", t.ID, err)
							continue
						}
						latency.executedAt = time.Now()
						// Get actual fill quantities
						filled1, filled2 := shares, shares
						actualCost1, actualCost2 := shares*avgPrice1, shares*avgPrice2
						if trade1 != nil {
							filled1 = trade1.Quantity
							actualCost1 = trade1.Value
						}
						if trade2 != nil {
							filled2 = trade2.Quantity
							actualCost2 = trade2.Value
						}

						// Log if we walked deeper into the book
						if avgPrice1 != ask1 || avgPrice2 != ask2 {
							t.TUI.LogEvent("[%s] 📊 Walked book: %s@$%.3f, %s@$%.3f",
								t.ID, t.Outcomes[0], avgPrice1, t.Outcomes[1], avgPrice2)
						}

						// Record both sides - force market orders always fill
						t.TUI.RecordOrder(t.ID, t.Outcomes[0], "BUY", filled1, avgPrice1, actualCost1, margin, 0.0, "FILLED")
						t.TUI.RecordOrder(t.ID, t.Outcomes[1], "BUY", filled2, avgPrice2, actualCost2, margin, 0.0, "FILLED")

						// INSTANT MERGE: Immediately merge to realize profit
						// This matches realbot behavior and ensures round PnL is accurate
						minFilled := filled1
						if filled2 < minFilled {
							minFilled = filled2
						}
						if minFilled > 0 {
							result := t.Engine.MergeForMarket(t.ID, t.Outcomes[0], t.Outcomes[1], minFilled)
							latency.settledAt = time.Now()
							if result.PnL != 0 {
								t.TUI.LogEvent("[%s] 💰 MERGED! +$%.2f profit", t.ID, result.PnL)
							}
						}
						logPaperExecutionLatency(t, latency)

						lastTrade = time.Now()
						t.LaddersPlaced = true
					}
				}
			}

			// ═══════════════════════════════════════════════════════════════════════════
			// SPLIT STRATEGY SIMULATION: Sell when bid_sum > $1.00 + margin
			// This simulates the panic sell strategy without real blockchain calls
			// ═══════════════════════════════════════════════════════════════════════════
			if len(t.Outcomes) == 2 && marketState == paper.MarketStateActive && liveCfg.SplitStrategyEnabled && localPairFresh {
				bid1 := t.TokenBids[t.Outcomes[0]]
				bid2 := t.TokenBids[t.Outcomes[1]]
				currentEquity := t.Engine.GetEquity()

				// Initial split: create simulated inventory
				// Split is always safe - can merge back to USDC anytime at 1:1
				if !t.SplitInitialized {
					baseTradeSize := t.Config.CalculateTradeSize(currentEquity)
					initialBuffer := baseTradeSize * 2.0
					if initialBuffer < MinSplitBuffer {
						initialBuffer = MinSplitBuffer
					}
					maxInitial := currentEquity * t.Config.SplitInitialCapPct
					splitAmount := initialBuffer
					if splitAmount > maxInitial {
						splitAmount = maxInitial
					}
					if splitAmount >= MinSplitAmount {
						t.SplitInventory.RecordSplit(t.ID, t.Outcomes[0], t.Outcomes[1], splitAmount)
						t.Engine.DeductBalance(splitAmount)
						t.Engine.RecalculateDrawdown() // Safe to check drawdown now
						t.SplitInitialized = true
						t.InitialSplitAmount = splitAmount // Store for replenishment target
						t.TUI.LogEvent("[%s] 🔀 SPLIT (sim): Created %.0f shares ($%.2f)", t.ID, splitAmount, splitAmount)
					}
				}

				// Check for panic sell opportunity
				if bid1 >= liveCfg.MinAskPrice && bid2 >= liveCfg.MinAskPrice && bid1 <= liveCfg.MaxAskPrice && bid2 <= liveCfg.MaxAskPrice {
					bidSum := bid1 + bid2
					sellMargin := (bidSum - 1.0) * 100

					// Background replenishment check
					baseTradeSize := t.Config.CalculateTradeSize(currentEquity)
					targetBuffer := baseTradeSize * t.Config.MaxAggressionMultiplier
					currentShares := t.SplitInventory.GetMinSplitShares(t.ID, t.Outcomes[0], t.Outcomes[1])
					replenishAmount := baseTradeSize * 2.0

					decision := t.ReplenishCtrl.CheckReplenish(paper.ReplenishParams{
						CurrentShares:      currentShares,
						TargetBuffer:       targetBuffer,
						InitialShares:      t.InitialSplitAmount, // Replenish back to initial amount immediately
						SellMargin:         sellMargin,
						MinMarginThreshold: t.Config.SplitMinMarginSell - 1.0,
						CurrentBalance:     currentEquity,
						ReplenishAmount:    replenishAmount,
						MaxBalancePercent:  t.Config.SplitReplenishCapPct,
					})

					if decision.ShouldReplenish && t.ReplenishCtrl.MarkInProgress() {
						// Simulate replenishment - use exact amount needed to reach initial
						actualReplenish := decision.Amount
						t.SplitInventory.RecordSplit(t.ID, t.Outcomes[0], t.Outcomes[1], actualReplenish)
						t.Engine.DeductBalance(actualReplenish)
						t.Engine.RecalculateDrawdown() // Safe to check drawdown now
						t.TUI.LogEvent("[%s] 🔄 SPLIT (sim): Replenished +%.0f shares (now %.0f)", t.ID, actualReplenish, t.InitialSplitAmount)
						t.ReplenishCtrl.MarkComplete()
					}

					// Panic sell logic
					if sellMargin >= t.Config.SplitMinMarginSell-1e-4 && time.Since(t.LastSplitSell) > 2*time.Second {
						requestedShares := baseTradeSize
						if t.Config.EnableMarginAggression {
							multiplier := sellMargin / 2.0
							if multiplier > t.Config.MaxAggressionMultiplier {
								multiplier = t.Config.MaxAggressionMultiplier
							}
							if multiplier < 1.0 {
								multiplier = 1.0
							}
							requestedShares = baseTradeSize * multiplier
						}

						availableShares := t.SplitInventory.GetMinSplitShares(t.ID, t.Outcomes[0], t.Outcomes[1])
						sharesToSell := requestedShares
						if sharesToSell > availableShares {
							if availableShares >= 1.0 {
								sharesToSell = availableShares
							} else {
								sharesToSell = 0
							}
						}

						if sharesToSell >= 1.0 {
							if sharesToSell > MaxSharesPerSell {
								sharesToSell = MaxSharesPerSell
							}

							// Calculate liquidity depth for display (similar to ARB buy)
							bids1 := t.TokenFullBids[t.Outcomes[0]]
							bids2 := t.TokenFullBids[t.Outcomes[1]]
							bookDepth1, bookDepth2 := len(bids1), len(bids2)

							// Calculate matched liquidity across valid bid levels
							minSum := 1.0 + (t.Config.SplitMinMarginSell / 100.0)
							var rawLiq1, rawLiq2 float64
							var maxValidI, maxValidJ int

							// Sort bids by price descending (best bids first)
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

							// Walk bid levels to find matched liquidity
							for bi, bj := 0, 0; bi < len(sortedBids1) && bj < len(sortedBids2); {
								if sortedBids1[bi].Price+sortedBids2[bj].Price < minSum-1e-6 {
									break
								}
								if bi+1 > maxValidI {
									maxValidI = bi + 1
									rawLiq1 += sortedBids1[bi].Size
								}
								if bj+1 > maxValidJ {
									maxValidJ = bj + 1
									rawLiq2 += sortedBids2[bj].Size
								}
								if sortedBids1[bi].Size <= sortedBids2[bj].Size {
									sortedBids2[bj].Size -= sortedBids1[bi].Size
									bi++
								} else {
									sortedBids1[bi].Size -= sortedBids2[bj].Size
									bj++
								}
							}

							// Calculate true proceeds by walking the bids
							calcSideProceeds := func(reqShares float64, bids []paper.MarketLevel) float64 {
								rem := reqShares
								p := 0.0
								for _, lvl := range bids {
									if rem <= 0 {
										break
									}
									fill := math.Min(rem, lvl.Size)
									p += fill * lvl.Price
									rem -= fill
								}
								if rem > 0 && len(bids) > 0 {
									p += rem * bids[0].Price
								}
								return p
							}

							trueProceeds1 := calcSideProceeds(sharesToSell, sortedBids1)
							trueProceeds2 := calcSideProceeds(sharesToSell, sortedBids2)

							avgBid1 := trueProceeds1 / sharesToSell
							avgBid2 := trueProceeds2 / sharesToSell

							// Calculate fees (collected in USDC for SELL)
							feeUsdc := 0.0
							if t.Config.FeeRateBps > 0 {
								fee1 := sharesToSell * 0.25 * math.Pow(avgBid1*(1.0-avgBid1), 2.0) * avgBid1
								fee2 := sharesToSell * 0.25 * math.Pow(avgBid2*(1.0-avgBid2), 2.0) * avgBid2
								feeUsdc = fee1 + fee2
							}

							// Ensure it's actually profitable after depth slippage and fees
							// Cost basis of a split share is exactly $1.00 for the pair (1 YES + 1 NO)
							expectedProfit := (trueProceeds1 + trueProceeds2) - feeUsdc - (sharesToSell * 1.0)
							if expectedProfit <= 0 {
								continue
							}
							latency := paperExecutionLatency{
								detectedAt:  time.Now(),
								opportunity: "paper split-sell",
								marketID:    t.ID,
								shares:      sharesToSell,
								marginPct:   sellMargin,
								expectedPnL: expectedProfit,
							}

							// Simulate sell: record profit using actual average fill prices
							latency.startedAt = time.Now()
							profit1 := t.SplitInventory.RecordSell(t.ID, t.Outcomes[0], sharesToSell, avgBid1)
							profit2 := t.SplitInventory.RecordSell(t.ID, t.Outcomes[1], sharesToSell, avgBid2)
							latency.executedAt = time.Now()
							totalProfit := profit1 + profit2 - feeUsdc

							// Divide the fee roughly proportionally to adjust the leg-level profit for display
							netProfit1 := profit1 - (feeUsdc / 2.0)
							netProfit2 := profit2 - (feeUsdc / 2.0)

							// Add proceeds back to balance
							proceeds := (trueProceeds1 + trueProceeds2) - feeUsdc
							t.Engine.AddBalance(proceeds)
							t.Engine.AddRealizedPnL(totalProfit)
							t.Engine.RecalculateDrawdown() // Safe to check drawdown now
							latency.settledAt = time.Now()

							// Enhanced log with liquidity and depth info (same format as ARB buy)
							t.TUI.LogEvent("[%s] 📈 SPLIT SELL! %s@$%.2f + %s@$%.2f = $%.3f (%.1f%%) | %.0f shares, profit $%.2f [liq: %.0f/%.0f, levels used: %d/%d (total depth: %d/%d)]",
								t.ID, t.Outcomes[0], bid1, t.Outcomes[1], bid2, bidSum, sellMargin, sharesToSell, totalProfit,
								rawLiq1, rawLiq2, maxValidI, maxValidJ, bookDepth1, bookDepth2)
							t.TUI.RecordOrder(t.ID, t.Outcomes[0], "SELL", sharesToSell, avgBid1, sharesToSell*avgBid1, sellMargin, netProfit1, "FILLED")
							t.TUI.RecordOrder(t.ID, t.Outcomes[1], "SELL", sharesToSell, avgBid2, sharesToSell*avgBid2, sellMargin, netProfit2, "FILLED")
							logPaperExecutionLatency(t, latency)
							t.LastSplitSell = time.Now()
						}
					}
				}

				// End-of-market merge: merge remaining split shares before expiry
				timeToEnd := time.Until(t.EndTime)
				if timeToEnd < 30*time.Second && timeToEnd > 0 {
					remainingShares := t.SplitInventory.GetMinSplitShares(t.ID, t.Outcomes[0], t.Outcomes[1])
					if remainingShares >= 1.0 {
						merged := t.SplitInventory.RecordMerge(t.ID, t.Outcomes[0], t.Outcomes[1], remainingShares)
						t.Engine.AddBalance(merged)    // $1 per merged pair
						t.Engine.RecalculateDrawdown() // Safe to check drawdown now
						t.TUI.LogEvent("[%s] 💰 SPLIT MERGE (sim): Merged %.0f shares → $%.2f", t.ID, merged, merged)
					}
				}
			}

			// Minimal sleep for ultra-low latency trading - context-aware
			// 10ms = 100 ticks/sec - balance between responsiveness and CPU
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
}

// determineWinner picks the winning outcome based on last known prices
func (t *MarketTrader) determineWinner() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.Outcomes) == 0 {
		return ""
	}

	// For 2-outcome markets (Up/Down)
	if len(t.Outcomes) == 2 {
		p1 := t.FloatPrices[t.Outcomes[0]]
		p2 := t.FloatPrices[t.Outcomes[1]]

		if p1 > p2 {
			return t.Outcomes[0]
		} else if p2 > p1 {
			return t.Outcomes[1]
		}
	}

	// Fallback: Pick first outcome if no data
	return t.Outcomes[0]
}

// handleRestFallback polls REST API for fresh liquidity data
// REST is now the PRIMARY source for liquidity (WS only sends price changes)
// Returns true if any data was successfully retrieved
func (t *MarketTrader) handleRestFallback(ctx context.Context, tokenPrices map[string]string, staleTime time.Duration) bool {
	t.LastRestPoll = time.Now()
	staleSeconds := int(staleTime.Seconds())

	// Poll REST synchronously for reliability
	restSuccess := 0
	restErrors := 0
	restEmpty := 0
	var lastErr error
	for tokenID, outcome := range t.TokenMap {
		// Use short timeout
		restCtx, restCancel := context.WithTimeout(ctx, 3*time.Second)
		start := time.Now()
		book, err := t.RestClient.GetOrderBook(restCtx, tokenID)
		latency := time.Since(start)
		restCancel()

		// Update TUI with real REST latency
		t.TUI.UpdateRestLatency(latency)

		if err != nil {
			restErrors++
			lastErr = err
			// If one request fails (likely due to no internet), break immediately to prevent further blocking
			break
		}

		// Check if book is empty
		if len(book.Bids) == 0 && len(book.Asks) == 0 {
			restEmpty++
			t.mu.Lock()
			t.TokenBids[outcome] = 0
			t.TokenAsks[outcome] = 0
			t.TokenFullBids[outcome] = nil
			t.TokenFullAsks[outcome] = nil
			t.mu.Unlock()
			restSuccess++
			continue
		}

		bid, ask := 0.0, 0.0
		for _, b := range book.Bids {
			p, err := strconv.ParseFloat(b.Price, 64)
			if err != nil {
				t.TUI.LogEvent("[%s] Warning: failed to parse bid price '%s': %v", t.ID, b.Price, err)
				continue
			}
			if p > 0 && p < 1.0 && p > bid {
				bid = p
			}
		}
		for _, a := range book.Asks {
			p, err := strconv.ParseFloat(a.Price, 64)
			if err != nil {
				t.TUI.LogEvent("[%s] Warning: failed to parse ask price '%s': %v", t.ID, a.Price, err)
				continue
			}
			if p > 0 && p < 1.0 && (ask == 0 || p < ask) {
				ask = p
			}
		}

		if bid > 0 && ask > 0 && bid >= ask {
			t.mu.Lock()
			t.TokenBids[outcome] = 0
			t.TokenAsks[outcome] = 0
			t.TokenFullBids[outcome] = nil
			t.TokenFullAsks[outcome] = nil
			t.mu.Unlock()
			restSuccess++ // Ensure UI gets updated to 0
			continue      // Reject crossed book
		}

		// Always update with whatever data we got (even partial)
		t.mu.Lock()
		t.TokenBids[outcome] = bid
		t.TokenAsks[outcome] = ask

		if bid > 0 && ask > 0 && ask < 1.0 {
			mid := (bid + ask) / 2
			t.FloatPrices[outcome] = mid
			tokenPrices[outcome] = fmt.Sprintf("%.3f", mid)
			t.Engine.UpdateMarketData(t.ID, outcome, mid, bid, ask)
		}
		t.TokenFullBids[outcome] = mkt.LevelsToPriceDepth(book.Bids, true)
		t.TokenFullAsks[outcome] = mkt.LevelsToPriceDepth(book.Asks, false)
		t.mu.Unlock()

		// Count as success to ensure UI gets updated
		restSuccess++
	}

	// Log result - minimal spam, maximum info
	if restSuccess > 0 {
		now := time.Now()
		t.LastUpdate = now
		syncPaperPairUpdate(t, now)
		t.TUI.UpdateMarketPricesWithSourceAt(t.ID, t.TokenBids, t.TokenAsks, "REST", t.LastPairUpdate)
		// Only log recovery if WS was significantly stale (not normal polling)
		if staleSeconds >= 10 {
			t.TUI.LogEvent("[%s] ✅ REST recovered after %ds", t.ID, staleSeconds)
		}
		return true
	} else if restErrors > 0 {
		// Log errors every 10 seconds to avoid spam
		if staleSeconds%10 == 0 || staleSeconds == 10 {
			t.TUI.LogEvent("[%s] ❌ REST fail %ds: %v", t.ID, staleSeconds, lastErr)
		}
	} else if restEmpty == len(t.TokenMap) {
		// All books empty - likely market ended
		if staleSeconds%10 == 0 {
			t.TUI.LogEvent("[%s] 📭 All books empty (%ds)", t.ID, staleSeconds)
		}
	}
	return false
}
