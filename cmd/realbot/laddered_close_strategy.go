package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"Market-bot/internal/api"
	mkt "Market-bot/internal/markets"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

const (
	realbotLadderedOneHourClosePrice        = 0.999
	realbotLadderedOneHourCloseTriggerPrice = 0.985
	realbotLadderedOneHourCloseMonitorTTL   = 5 * time.Hour
	realbotLadderedOneHourClosePollInterval = 2 * time.Second
)

type realbotPendingLadderCloseOrder struct {
	Outcome     string
	OrderID     string
	Price       float64
	SubmittedAt time.Time
}

type realbotLadderedOneHourCloseSelection struct {
	Outcome       string
	Qty           float64
	AvgPrice      float64
	ObservedPrice float64
}

type realbotLadderCloseState struct {
	mu     sync.Mutex
	orders map[string]realbotPendingLadderCloseOrder
}

func newRealbotLadderCloseState() *realbotLadderCloseState {
	return &realbotLadderCloseState{
		orders: make(map[string]realbotPendingLadderCloseOrder),
	}
}

func (s *realbotLadderCloseState) get(marketID string) (realbotPendingLadderCloseOrder, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[strings.TrimSpace(marketID)]
	return order, ok
}

func (s *realbotLadderCloseState) set(marketID string, order realbotPendingLadderCloseOrder) {
	marketID = strings.TrimSpace(marketID)
	if marketID == "" {
		return
	}
	s.mu.Lock()
	s.orders[marketID] = order
	s.mu.Unlock()
}

func (s *realbotLadderCloseState) clear(marketID string) {
	marketID = strings.TrimSpace(marketID)
	if marketID == "" {
		return
	}
	s.mu.Lock()
	delete(s.orders, marketID)
	s.mu.Unlock()
}

func (s *realbotLadderCloseState) reason(marketID string) (string, bool) {
	pending, ok := s.get(marketID)
	if !ok || pending.Price <= 0 {
		return "", false
	}
	outcome := strings.TrimSpace(pending.Outcome)
	if outcome == "" {
		outcome = "higher side"
	}
	return fmt.Sprintf("waiting to sell %s from %s at $%.3f (GTC)", outcome, marketID, pending.Price), true
}

func realbotShouldUseLadderedOneHourClose(marketID string, liveCfg paper.TUISettings) bool {
	return realbotLadderedHoldMode(liveCfg) && realbotMarketWindowDuration(marketID) == time.Hour
}

func realbotLadderedOneHourCloseWindow(liveCfg paper.TUISettings) time.Duration {
	seconds := liveCfg.TakerCloseMarketTime
	if seconds <= 0 {
		seconds = 10
	}
	return time.Duration(seconds) * time.Second
}

func realbotLadderedObservedOutcomePrice(engine *paper.Engine, marketID, outcome string, bids, asks map[string]float64) float64 {
	if price := asks[outcome]; price > 0 && price < 1.0 {
		return price
	}
	if price := bids[outcome]; price > 0 && price < 1.0 {
		return price
	}
	if engine == nil {
		return 0
	}
	bid, ask := engine.GetMarketBidAsk(marketID, outcome)
	if ask > 0 && ask < 1.0 {
		return ask
	}
	if bid > 0 && bid < 1.0 {
		return bid
	}
	return 0
}

func realbotLocalOutcomePosition(engine *paper.Engine, marketID, outcome string) (qty, avgPrice float64, ok bool) {
	if engine == nil {
		return 0, 0, false
	}
	positions := engine.GetPositions()
	pos, ok := positions[marketID+":"+outcome]
	if !ok || pos.Quantity <= 0 {
		return 0, 0, false
	}
	return pos.Quantity, pos.AvgPrice, true
}

func realbotLadderedOneHourCloseCandidate(marketID string, outcomes []string, engine *paper.Engine, bids, asks map[string]float64) (realbotLadderedOneHourCloseSelection, bool) {
	best := realbotLadderedOneHourCloseSelection{}
	isClosedFallback := bids == nil && asks == nil

	for _, outcome := range outcomes {
		qty, avgPrice, ok := realbotLocalOutcomePosition(engine, marketID, outcome)
		if !ok || !hasActionableCleanupRemainder(qty) {
			continue
		}
		price := realbotLadderedObservedOutcomePrice(engine, marketID, outcome, bids, asks)
		if price <= 0 || price >= 1.0 {
			if isClosedFallback {
				price = avgPrice
			} else {
				continue
			}
		}
		if price > best.ObservedPrice {
			best = realbotLadderedOneHourCloseSelection{
				Outcome:       outcome,
				Qty:           qty,
				AvgPrice:      avgPrice,
				ObservedPrice: price,
			}
		}
	}
	if best.Outcome == "" {
		return realbotLadderedOneHourCloseSelection{}, false
	}
	if !isClosedFallback && best.ObservedPrice+1e-9 < realbotLadderedOneHourCloseTriggerPrice {
		return realbotLadderedOneHourCloseSelection{}, false
	}
	return best, true
}

func realbotApplyLadderedOneHourCloseFill(engine *paper.Engine, tui *paper.TUI, marketID, outcome string, qty, price float64, feeRate int) float64 {
	if engine == nil || qty <= 0 {
		return 0
	}
	posQty, avgPrice, ok := realbotLocalOutcomePosition(engine, marketID, outcome)
	if !ok {
		return 0
	}
	if qty > posQty {
		qty = posQty
	}
	qty = normalizeMarketSellShares(qty)
	if qty <= 0 {
		return 0
	}

	trade, err := engine.SellForMarketWithFeeRate(marketID, outcome, price, qty, feeRate)
	if err != nil {
		if tui != nil {
			tui.LogEvent("[%s] ⚠️ 1h ladder close local sync failed for %s: %v", marketID, outcome, err)
		}
		return 0
	}
	profit := trade.Value - (avgPrice * qty)
	if tui != nil {
		tui.RecordOrderWithMode(marketID, outcome, "SELL", qty, price, trade.Value, 0.0, profit, paperArbModeLaddered, "FILLED")
		tui.LogEvent("[%s] ✅ 1h ladder close filled: sold %s %s at $%.3f", marketID, formatShareQty(qty), outcome, price)
		if !realbotHasActionableEnginePositionsForMarket(engine, marketID) {
			tui.ClearMarketInventoryStatus(marketID)
		}
	}
	return qty
}

func realbotStartLadderedOneHourCloseMonitor(ctx context.Context, ladderState *realbotLadderCloseState, marketID, outcome, orderID string, requestedQty, mirroredQty float64, feeRate int, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI) {
	if trader == nil || engine == nil || strings.TrimSpace(orderID) == "" || requestedQty <= 0 {
		return
	}

	go func() {
		deadline := time.Now().Add(realbotLadderedOneHourCloseMonitorTTL)
		ticker := time.NewTicker(realbotLadderedOneHourClosePollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if mirroredQty >= requestedQty-0.0001 || !realbotHasActionableEnginePositionsForMarket(engine, marketID) {
					ladderState.clear(marketID)
					if tui != nil {
						tui.ClearMarketInventoryStatus(marketID)
					}
					return
				}
				if time.Now().After(deadline) {
					return
				}

				confirmedQty := trader.GetConfirmedFillSize(orderID)
				if confirmedQty > mirroredQty+0.0001 {
					delta := confirmedQty - mirroredQty
					mirroredQty += realbotApplyLadderedOneHourCloseFill(engine, tui, marketID, outcome, delta, realbotLadderedOneHourClosePrice, feeRate)
				}
			}
		}
	}()
}

func realbotSubmitLadderedOneHourCloseOrder(submitCtx, monitorCtx context.Context, ladderState *realbotLadderCloseState, marketID string, market *api.Market, outcomes []string, bids, asks map[string]float64, tokenFeeRates map[string]int, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI) bool {
	if trader == nil || engine == nil || market == nil || tui == nil {
		return false
	}
	if pending, ok := ladderState.get(marketID); ok && pending.Price > 0 {
		return true
	}

	candidate, ok := realbotLadderedOneHourCloseCandidate(marketID, outcomes, engine, bids, asks)
	if !ok {
		return false
	}
	tokenID := mkt.GetTokenIDForOutcome(market, candidate.Outcome)
	if tokenID == "" {
		tui.LogEvent("[%s] ⚠️ 1h ladder close skipped: missing token for %s", marketID, candidate.Outcome)
		return false
	}

	feeRate := tokenFeeRates[candidate.Outcome]
	if feeRate <= 0 {
		feeRate = 1000
	}
	result, err := trader.Sell(submitCtx, tokenID, candidate.Outcome, realbotLadderedOneHourClosePrice, candidate.Qty, api.OrderTypeLimit, api.TIFGoodTilCancelled, feeRate)
	if err != nil {
		tui.LogEvent("[%s] ⚠️ 1h ladder close submit failed for %s: %v", marketID, candidate.Outcome, err)
		return false
	}
	if result == nil || !result.Success {
		message := ""
		if result != nil {
			message = strings.TrimSpace(result.Message)
		}
		if message == "" {
			message = "execution did not succeed"
		}
		tui.LogEvent("[%s] ⚠️ 1h ladder close rejected for %s: %s", marketID, candidate.Outcome, message)
		return false
	}

	orderID := strings.TrimSpace(result.OrderID)
	if orderID == "" && !trader.IsPaperMode() {
		tui.LogEvent("[%s] ⚠️ 1h ladder close rejected for %s: missing order id", marketID, candidate.Outcome)
		return false
	}

	ladderState.set(marketID, realbotPendingLadderCloseOrder{
		Outcome:     candidate.Outcome,
		OrderID:     orderID,
		Price:       realbotLadderedOneHourClosePrice,
		SubmittedAt: time.Now(),
	})
	tui.SetMarketInventoryStatus(marketID, "WAITING TO SELL")
	tui.LogEvent("[%s] 🪜 1h ladder close working: GTC sell %s %s at $%.3f (signal $%.3f)", marketID, formatShareQty(candidate.Qty), candidate.Outcome, realbotLadderedOneHourClosePrice, candidate.ObservedPrice)

	mirroredQty := 0.0
	if result.AcknowledgedQty > 0 {
		fillPrice := realbotLadderedOneHourClosePrice
		if result.AcknowledgedQty > 0 && result.AcknowledgedNotional > 0 {
			fillPrice = result.AcknowledgedNotional / result.AcknowledgedQty
		}
		mirroredQty = realbotApplyLadderedOneHourCloseFill(engine, tui, marketID, candidate.Outcome, result.AcknowledgedQty, fillPrice, feeRate)
	}
	if mirroredQty >= candidate.Qty-0.0001 || !realbotHasActionableEnginePositionsForMarket(engine, marketID) {
		ladderState.clear(marketID)
		tui.ClearMarketInventoryStatus(marketID)
		return true
	}
	if orderID != "" {
		realbotStartLadderedOneHourCloseMonitor(monitorCtx, ladderState, marketID, candidate.Outcome, orderID, candidate.Qty, mirroredQty, feeRate, trader, engine, tui)
	}
	return true
}

func realbotHandleLadderedOneHourCloseWindow(ctx context.Context, ladderState *realbotLadderCloseState, marketID string, market *api.Market, outcomes []string, bids, asks map[string]float64, tokenFeeRates map[string]int, liveCfg paper.TUISettings, timeToExpiry time.Duration, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI) bool {
	if !realbotShouldUseLadderedOneHourClose(marketID, liveCfg) {
		return false
	}
	closeWindow := realbotLadderedOneHourCloseWindow(liveCfg)
	if timeToExpiry <= 0 || timeToExpiry > closeWindow {
		return false
	}
	if _, ok := ladderState.get(marketID); ok {
		if tui != nil {
			tui.SetMarketInventoryStatus(marketID, "WAITING TO SELL")
		}
		return true
	}
	submitCtx, cancel := context.WithTimeout(ctx, 1000*time.Millisecond)
	defer cancel()
	return realbotSubmitLadderedOneHourCloseOrder(submitCtx, ctx, ladderState, marketID, market, outcomes, bids, asks, tokenFeeRates, trader, engine, tui)
}


