package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
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
	Outcome       string
	OrderID       string
	Price         float64
	SubmittedAt   time.Time
	RequestedQty  float64
	MirroredQty   float64
	FeeRate       int
	MonitorActive bool
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

func (s *realbotLadderCloseState) setMirroredQty(marketID string, mirroredQty float64) bool {
	marketID = strings.TrimSpace(marketID)
	if marketID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[marketID]
	if !ok {
		return false
	}
	order.MirroredQty = mirroredQty
	s.orders[marketID] = order
	return true
}

func (s *realbotLadderCloseState) startMonitor(marketID string) (realbotPendingLadderCloseOrder, bool) {
	marketID = strings.TrimSpace(marketID)
	if marketID == "" {
		return realbotPendingLadderCloseOrder{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[marketID]
	if !ok || order.MonitorActive || strings.TrimSpace(order.OrderID) == "" || order.RequestedQty <= 0 {
		return realbotPendingLadderCloseOrder{}, false
	}
	order.MonitorActive = true
	s.orders[marketID] = order
	return order, true
}

func (s *realbotLadderCloseState) stopMonitor(marketID string) {
	marketID = strings.TrimSpace(marketID)
	if marketID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[marketID]
	if !ok {
		return
	}
	order.MonitorActive = false
	s.orders[marketID] = order
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

func realbotIsLadderedOneHourMarket(marketID string, liveCfg paper.TUISettings) bool {
	return realbotLadderedHoldMode(liveCfg) && realbotMarketWindowDuration(marketID) == time.Hour
}

func realbotLadderedOneHourExitMode(liveCfg paper.TUISettings) string {
	return strings.ToLower(strings.TrimSpace(liveCfg.OneHourCryptoExitMode))
}

func realbotShouldUseLadderedOneHourClose(marketID string, liveCfg paper.TUISettings) bool {
	if !realbotIsLadderedOneHourMarket(marketID, liveCfg) {
		return false
	}
	return realbotLadderedOneHourExitMode(liveCfg) != core.OneHourCryptoExitWaitResolve
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

func realbotLadderedSellCandidateObservedPrice(engine *paper.Engine, marketID, outcome string, bids, asks map[string]float64) float64 {
	if price := bids[outcome]; price > 0 && price < 1.0 {
		return price
	}
	bid, ask := 0.0, 0.0
	if engine != nil {
		bid, ask = engine.GetMarketBidAsk(marketID, outcome)
		if bid > 0 && bid < 1.0 {
			return bid
		}
	}
	if price := asks[outcome]; price > 0 && price < 1.0 {
		return price
	}
	if ask > 0 && ask < 1.0 {
		return ask
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

	for _, outcome := range outcomes {
		qty, avgPrice, ok := realbotLocalOutcomePosition(engine, marketID, outcome)
		if !ok || !hasActionableCleanupRemainder(qty) {
			continue
		}
		price := realbotLadderedSellCandidateObservedPrice(engine, marketID, outcome, bids, asks)
		if price <= 0 || price >= 1.0 {
			continue
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
	if best.ObservedPrice+1e-9 < realbotLadderedOneHourCloseTriggerPrice {
		return realbotLadderedOneHourCloseSelection{}, false
	}
	return best, true
}

func realbotSettleLadderedOneHourOppositeLosers(engine *paper.Engine, tui *paper.TUI, marketID, winningOutcome string) *paper.RedemptionResult {
	if engine == nil || strings.TrimSpace(marketID) == "" || strings.TrimSpace(winningOutcome) == "" {
		return nil
	}
	if qty, _, ok := realbotLocalOutcomePosition(engine, marketID, winningOutcome); ok && hasActionableCleanupRemainder(qty) {
		return nil
	}

	result := &paper.RedemptionResult{
		MarketID:       marketID,
		WinningOutcome: winningOutcome,
	}
	for _, pos := range engine.GetPositions() {
		if pos.MarketID != marketID || strings.EqualFold(strings.TrimSpace(pos.Outcome), strings.TrimSpace(winningOutcome)) || pos.Quantity <= 0 {
			continue
		}
		if !engine.RemovePositionForSettlement(marketID, pos.Outcome) {
			continue
		}
		if result.LosingOutcome == "" {
			result.LosingOutcome = pos.Outcome
		}
		result.LosingShares += pos.Quantity
		result.LosingCost += pos.TotalCost
	}
	if result.LosingCost <= 0 {
		return nil
	}

	result.TotalPnL = -result.LosingCost
	engine.AdjustRealizedPnL(result.TotalPnL)
	engine.RecordSettledLoser(marketID, result.LosingOutcome, result.LosingShares)
	if tui != nil {
		tui.AmendMostRecentRoundForMarket(marketID, result.TotalPnL, []*paper.RedemptionResult{result})
		tui.LogEvent("[%s] 🧹 1h ladder close settled opposite loser: %s %s removed (-$%.2f)", marketID, formatShareQty(result.LosingShares), result.LosingOutcome, result.LosingCost)
		if !realbotHasActionableEnginePositionsForMarket(engine, marketID) {
			tui.ClearMarketInventoryStatus(marketID)
		}
		tui.UpdateWalletTruthResolution(marketID, true, result.WinningOutcome)
	}
	return result
}

func realbotApplyLadderedOneHourCloseFill(engine *paper.Engine, tui *paper.TUI, marketID, outcome string, qty, price float64, feeRate int, settleOppositeLosers bool) float64 {
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
	}
	if settleOppositeLosers {
		realbotSettleLadderedOneHourOppositeLosers(engine, tui, marketID, outcome)
	}
	if tui != nil {
		if !realbotHasActionableEnginePositionsForMarket(engine, marketID) {
			tui.ClearMarketInventoryStatus(marketID)
		}
	}
	return qty
}

func realbotRecordLadderedOneHourPaperCloseFill(tui *paper.TUI, marketID, outcome string, qty, price, netProceeds, avgPrice float64) {
	if tui == nil || qty <= 0 {
		return
	}
	profit := netProceeds - (avgPrice * qty)
	tui.RecordOrderWithMode(marketID, outcome, "SELL", qty, price, netProceeds, 0.0, profit, paperArbModeLaddered, "FILLED")
	tui.LogEvent("[%s] ✅ 1h ladder close filled: sold %s %s at $%.3f", marketID, formatShareQty(qty), outcome, price)
}

func realbotStartLadderedOneHourCloseMonitor(ctx context.Context, ladderState *realbotLadderCloseState, marketID string, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI) {
	if trader == nil || engine == nil || ladderState == nil {
		return
	}
	pending, ok := ladderState.startMonitor(marketID)
	if !ok {
		return
	}

	go func(initial realbotPendingLadderCloseOrder) {
		defer ladderState.stopMonitor(marketID)
		deadline := initial.SubmittedAt.Add(realbotLadderedOneHourCloseMonitorTTL)
		if initial.SubmittedAt.IsZero() {
			deadline = time.Now().Add(realbotLadderedOneHourCloseMonitorTTL)
		}
		ticker := time.NewTicker(realbotLadderedOneHourClosePollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				current, ok := ladderState.get(marketID)
				if !ok {
					return
				}
				if current.MirroredQty >= current.RequestedQty-0.0001 || !realbotHasActionableEnginePositionsForMarket(engine, marketID) {
					if current.OrderID != "" {
						trader.ResetConfirmedFill(current.OrderID)
					}
					ladderState.clear(marketID)
					if tui != nil {
						tui.ClearMarketInventoryStatus(marketID)
					}
					return
				}
				if time.Now().After(deadline) {
					return
				}

				realbotSyncPendingLadderedOneHourCloseFill(ladderState, marketID, nil, trader, engine, tui)
			}
		}
	}(pending)
}

func realbotSyncPendingLadderedOneHourCloseFill(ladderState *realbotLadderCloseState, marketID string, bids map[string]float64, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI) bool {
	if ladderState == nil || engine == nil {
		return false
	}
	pending, ok := ladderState.get(marketID)
	if !ok || pending.Price <= 0 {
		return false
	}
	if !realbotHasActionableEnginePositionsForMarket(engine, marketID) {
		if pending.OrderID != "" {
			trader.ResetConfirmedFill(pending.OrderID)
		}
		ladderState.clear(marketID)
		if tui != nil {
			tui.ClearMarketInventoryStatus(marketID)
		}
		return true
	}
	if strings.TrimSpace(pending.OrderID) == "" {
		return realbotPollPaperLadderCloseFill(ladderState, marketID, bids, engine, tui)
	}
	if trader == nil {
		return false
	}

	confirmedQty := trader.GetConfirmedFillSize(pending.OrderID)
	if confirmedQty <= pending.MirroredQty+0.0001 {
		return false
	}
	delta := confirmedQty - pending.MirroredQty
	applied := realbotApplyLadderedOneHourCloseFill(engine, tui, marketID, pending.Outcome, delta, realbotLadderedOneHourClosePrice, pending.FeeRate, true)
	if applied <= 0 {
		return false
	}
	mirroredQty := pending.MirroredQty + applied
	ladderState.setMirroredQty(marketID, mirroredQty)
	if mirroredQty >= pending.RequestedQty-0.0001 || !realbotHasActionableEnginePositionsForMarket(engine, marketID) {
		if pending.OrderID != "" {
			trader.ResetConfirmedFill(pending.OrderID)
		}
		ladderState.clear(marketID)
		if tui != nil {
			tui.ClearMarketInventoryStatus(marketID)
		}
	}
	return true
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
		if tui != nil {
			tui.LogEventDedup("1h-close-no-candidate:"+marketID, 15*time.Second, "[%s] ⏳ 1h .999 close waiting: no held outcome has a sellable quote at or above $%.3f", marketID, realbotLadderedOneHourCloseTriggerPrice)
		}
		return false
	}
	tokenID := mkt.GetTokenIDForOutcome(market, candidate.Outcome)
	if tokenID == "" {
		tui.LogEvent("[%s] ⚠️ 1h ladder close skipped: missing token for %s", marketID, candidate.Outcome)
		return false
	}
	_, candidateAvgPrice, _ := realbotLocalOutcomePosition(engine, marketID, candidate.Outcome)

	feeRate := realbotResolveFeeRateBps(tokenFeeRates, candidate.Outcome, nil)
	realbotRecordOrderSubmissions(1)
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
		// In paper mode, a GTC sell that is not immediately marketable should
		// rest as a pending order so it can fill on a later tick when the
		// bid reaches the limit price.
		if trader.IsEmbeddedPaperMode() && strings.Contains(message, "not marketable") {
			ladderState.set(marketID, realbotPendingLadderCloseOrder{
				Outcome:      candidate.Outcome,
				Price:        realbotLadderedOneHourClosePrice,
				SubmittedAt:  time.Now(),
				RequestedQty: candidate.Qty,
				FeeRate:      feeRate,
			})
			tui.SetMarketInventoryStatus(marketID, "WAITING TO SELL")
			tui.LogEventDedup("1h-close-paper-resting:"+marketID, 15*time.Second, "[%s] 🪜 1h ladder close resting paper GTC sell %s %s at $%.3f (bid not yet at limit)", marketID, formatShareQty(candidate.Qty), candidate.Outcome, realbotLadderedOneHourClosePrice)
			return true
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
		Outcome:      candidate.Outcome,
		OrderID:      orderID,
		Price:        realbotLadderedOneHourClosePrice,
		SubmittedAt:  time.Now(),
		RequestedQty: candidate.Qty,
		FeeRate:      feeRate,
	})
	tui.SetMarketInventoryStatus(marketID, "WAITING TO SELL")
	tui.LogEvent("[%s] 🪜 1h ladder close working: GTC sell %s %s at $%.3f (signal $%.3f)", marketID, formatShareQty(candidate.Qty), candidate.Outcome, realbotLadderedOneHourClosePrice, candidate.ObservedPrice)

	mirroredQty := 0.0
	if result.AcknowledgedQty > 0 {
		fillPrice := realbotLadderedOneHourClosePrice
		if result.AcknowledgedQty > 0 && result.AcknowledgedNotional > 0 && !trader.IsEmbeddedPaperMode() {
			fillPrice = result.AcknowledgedNotional / result.AcknowledgedQty
		}
		if trader.IsEmbeddedPaperMode() {
			mirroredQty = result.AcknowledgedQty
			realbotRecordLadderedOneHourPaperCloseFill(tui, marketID, candidate.Outcome, result.AcknowledgedQty, fillPrice, result.AcknowledgedNotional, candidateAvgPrice)
			realbotSettleLadderedOneHourOppositeLosers(engine, tui, marketID, candidate.Outcome)
		} else {
			mirroredQty = realbotApplyLadderedOneHourCloseFill(engine, tui, marketID, candidate.Outcome, result.AcknowledgedQty, fillPrice, feeRate, true)
		}
	}
	ladderState.setMirroredQty(marketID, mirroredQty)
	if mirroredQty >= candidate.Qty-0.0001 || !realbotHasActionableEnginePositionsForMarket(engine, marketID) {
		ladderState.clear(marketID)
		tui.ClearMarketInventoryStatus(marketID)
		return true
	}
	if orderID != "" {
		realbotStartLadderedOneHourCloseMonitor(monitorCtx, ladderState, marketID, trader, engine, tui)
	}
	return true
}

func realbotPollPaperLadderCloseFill(ladderState *realbotLadderCloseState, marketID string, bids map[string]float64, engine *paper.Engine, tui *paper.TUI) bool {
	if ladderState == nil || engine == nil {
		return false
	}
	pending, ok := ladderState.get(marketID)
	if !ok || pending.Price <= 0 || strings.TrimSpace(pending.OrderID) != "" {
		return false
	}
	outcome := strings.TrimSpace(pending.Outcome)
	if outcome == "" {
		return false
	}

	bid := bids[outcome]
	if bid <= 0 {
		bid, _ = engine.GetMarketBidAsk(marketID, outcome)
	}
	if bid+1e-9 < pending.Price {
		// In paper mode, WS bid data may not capture the exact 0.999 tick
		// (e.g., bid jumps from 0.99 to 1.00). Use the trigger price as
		// a relaxed fill threshold — once the bid confirms the outcome is
		// clearly winning, the GTC sell would realistically fill.
		if bid+1e-9 < realbotLadderedOneHourCloseTriggerPrice {
			return false
		}
	}

	if _, _, posOK := realbotLocalOutcomePosition(engine, marketID, outcome); !posOK {
		if pending.OrderID != "" {
			trader.ResetConfirmedFill(pending.OrderID)
		}
		ladderState.clear(marketID)
		if tui != nil {
			tui.ClearMarketInventoryStatus(marketID)
		}
		return true
	}

	applied := realbotApplyLadderedOneHourCloseFill(engine, tui, marketID, outcome, pending.RequestedQty, pending.Price, pending.FeeRate, true)
	if applied > 0 {
		if pending.OrderID != "" {
			trader.ResetConfirmedFill(pending.OrderID)
		}
		ladderState.clear(marketID)
		if tui != nil && !realbotHasActionableEnginePositionsForMarket(engine, marketID) {
			tui.ClearMarketInventoryStatus(marketID)
		}
	}
	return applied > 0
}

func realbotHandleLadderedOneHourCloseWindow(ctx context.Context, ladderState *realbotLadderCloseState, marketID string, market *api.Market, outcomes []string, bids, asks map[string]float64, tokenFeeRates map[string]int, liveCfg paper.TUISettings, timeToExpiry time.Duration, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI) bool {
	if !realbotIsLadderedOneHourMarket(marketID, liveCfg) {
		return false
	}
	closeWindow := realbotLadderedOneHourCloseWindow(liveCfg)
	inCloseWindow := timeToExpiry > 0 && timeToExpiry <= closeWindow
	if !realbotShouldUseLadderedOneHourClose(marketID, liveCfg) {
		if inCloseWindow && realbotHasActionableEnginePositionsForMarket(engine, marketID) {
			if ladderState != nil {
				ladderState.clear(marketID)
			}
			if tui != nil {
				tui.SetMarketInventoryStatus(marketID, "WAITING RESOLUTION")
				tui.LogEventDedup("1h-wait-resolve:"+marketID, 30*time.Second, "[%s] ⏳ 1h ladder close set to wait for resolution; skipping .999 sell", marketID)
			}
			return true
		}
		return false
	}
	if pending, ok := ladderState.get(marketID); ok {
		if !realbotHasActionableEnginePositionsForMarket(engine, marketID) {
			ladderState.clear(marketID)
			if tui != nil {
				tui.ClearMarketInventoryStatus(marketID)
			}
			return true
		}
		if tui != nil {
			tui.SetMarketInventoryStatus(marketID, "WAITING TO SELL")
		}
		realbotSyncPendingLadderedOneHourCloseFill(ladderState, marketID, bids, trader, engine, tui)
		if strings.TrimSpace(pending.OrderID) == "" {
			return true
		}
		realbotStartLadderedOneHourCloseMonitor(ctx, ladderState, marketID, trader, engine, tui)
		return true
	}

	_, priceTriggered := realbotLadderedOneHourCloseCandidate(marketID, outcomes, engine, bids, asks)
	if !inCloseWindow && !priceTriggered {
		return false
	}
	if tui != nil && realbotHasActionableEnginePositionsForMarket(engine, marketID) {
		tui.SetMarketInventoryStatus(marketID, "WAITING TO SELL")
	}

	// Try to find a winning candidate we hold to sell
	submitCtx, cancel := context.WithTimeout(ctx, 1000*time.Millisecond)
	defer cancel()
	if realbotSubmitLadderedOneHourCloseOrder(submitCtx, ctx, ladderState, marketID, market, outcomes, bids, asks, tokenFeeRates, trader, engine, tui) {
		return true
	}
	if !inCloseWindow {
		return false
	}

	// If we couldn't find a winning side to sell, check if we hold clear losers.
	// If any opposite outcome is > 0.985, the current holdings are clear losers.
	for _, outcome := range outcomes {
		qty, _, ok := realbotLocalOutcomePosition(engine, marketID, outcome)
		if !ok || !hasActionableCleanupRemainder(qty) {
			continue
		}

		winningOutcome := ""
		for _, other := range outcomes {
			if other == outcome {
				continue
			}
			price := bids[other]
			if price <= 0 && engine != nil {
				price, _ = engine.GetMarketBidAsk(marketID, other)
			}
			if price >= realbotLadderedOneHourCloseTriggerPrice {
				winningOutcome = other
				break
			}
		}

		if winningOutcome != "" {
			realbotSettleLadderedOneHourOppositeLosers(engine, tui, marketID, winningOutcome)
		}
	}

	// If after settling losers we have no actionable positions, clear state.
	if !realbotHasActionableEnginePositionsForMarket(engine, marketID) {
		ladderState.clear(marketID)
		if tui != nil {
			tui.ClearMarketInventoryStatus(marketID)
		}
		return true
	}

	return false
}
