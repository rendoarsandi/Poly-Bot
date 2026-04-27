package main

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

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
	ExactShares    bool
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

func directOrderNotional(price, size float64) float64 {
	if price <= 0 || size <= 0 {
		return 0
	}
	return price * size
}

func directSubmittedOrderValue(req directMarketOrderSignalRequest) float64 {
	orderReq := buildDirectMarketOrderRequest(req)
	amounts, err := api.ComputeOrderAmounts(orderReq)
	if err != nil {
		return 0
	}
	if req.Side == api.SideBuy {
		return float64(amounts.MakerMicro) / 1e6
	}
	return float64(amounts.TakerMicro) / 1e6
}

func hasActionableDirectOrderValue(price, size float64) bool {
	return directOrderNotional(price, size) >= realbotMinDirectOrderValue-1e-9
}

func hasActionableSubmittedDirectOrderValue(req directMarketOrderSignalRequest) bool {
	return directSubmittedOrderValue(req) >= realbotMinDirectOrderValue-1e-9
}

func directSubmittedOrderSummary(req directMarketOrderSignalRequest) string {
	orderReq := buildDirectMarketOrderRequest(req)
	amounts, err := api.ComputeOrderAmounts(orderReq)
	if err != nil {
		return fmt.Sprintf("%s shares @ $%.3f (encode err: %v)", formatShareQty(req.Size), req.Price, err)
	}
	submittedValue := float64(amounts.MakerMicro) / 1e6
	valueLabel := "buy"
	if req.Side == api.SideSell {
		submittedValue = float64(amounts.TakerMicro) / 1e6
		valueLabel = "sell"
	}
	return fmt.Sprintf("%s shares @ $%.3f => %sValue=$%.4f maker=%s taker=%s",
		formatShareQty(req.Size),
		req.Price,
		valueLabel,
		submittedValue,
		amounts.MakerAmount,
		amounts.TakerAmount,
	)
}

func directRejectedMinSizeTradeResult(req directMarketOrderSignalRequest) *trading.TradeResult {
	submittedValue := directSubmittedOrderValue(req)
	message := fmt.Sprintf("submitted marketable %s amount $%.4f is below Polymarket $1 minimum (%s)",
		strings.ToLower(string(req.Side)),
		submittedValue,
		directSubmittedOrderSummary(req),
	)
	return hydrateDirectMarketTradeResult(req, &trading.TradeResult{
		Success: false,
		Status:  "REJECTED",
		Message: message,
	})
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

func hasConfirmedExecutedQty(side api.Side, qty float64) bool {
	if side == api.SideSell {
		return qty > 0.000001
	}
	return qty >= minOnChainActionShares-1e-9
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

func ackNotionalMatchesAttributedSell(exec directMarketExecution, attributedQty float64) bool {
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

func reportedSellProceeds(exec directMarketExecution, observedPrice, attributedQty, requestedQty float64) float64 {
	qty := clampRequestedExecutionQty(attributedQty, requestedQty)
	if ackNotionalMatchesAttributedSell(exec, qty) {
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

func directExecutionNeedsAuditLog(exec directMarketExecution, requestedQty float64) bool {
	if exec.Result == nil {
		return false
	}
	if exec.VerifyErr != nil {
		return true
	}
	if directExecutionHasSizingDrift(exec, requestedQty) {
		return true
	}
	if len(exec.Result.TransactionsHashes) > 1 {
		return true
	}
	if !exec.Success {
		return exec.AcknowledgedQty > 0 || exec.AcknowledgedNotional > 0 || len(exec.Result.TransactionsHashes) > 0
	}
	return false
}

func logDirectExecutionAudit(tui *paper.TUI, id, label string, requestedQty, limitPrice float64, exec directMarketExecution) {
	if tui == nil || exec.Result == nil {
		return
	}
	if !directExecutionNeedsAuditLog(exec, requestedQty) {
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
	timeInForce := api.TIFFillAndKill
	if req.Side == api.SideBuy && req.ExactShares {
		// Polymarket treats FAK/FOK BUY size as notional dollars, not shares.
		// Use a marketable limit order so `Size` remains share-quantity, then
		// cancel any unfilled remainder after the immediate match window.
		timeInForce = api.TIFGoodTilCancelled
	}
	return &api.OrderRequest{
		TokenID:     req.TokenID,
		Price:       req.Price,
		Size:        req.Size,
		Side:        req.Side,
		OrderType:   api.OrderTypeLimit,
		TimeInForce: timeInForce,
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

func shouldCancelResidualBuyOrder(req directMarketOrderSignalRequest, executedQty float64) bool {
	if req.Side != api.SideBuy || !req.ExactShares || req.Size <= 0 {
		return false
	}
	return executedQty < req.Size-0.0001
}

func isIgnorableCancelOrderError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(msg, "status 404") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, "already canceled") ||
		strings.Contains(msg, "already cancelled") ||
		strings.Contains(msg, "already filled")
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

	executedQty, wsConfirmed, orderConfirmed, verifyErr := confirmMarketOrderExecution(ctx, trader, req, orderID, confirmTimeout)
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
	for _, req := range reqs {
		if req.Side == api.SideBuy && !hasActionableSubmittedDirectOrderValue(req) {
			execs := make([]directMarketExecution, len(reqs))
			for i := range reqs {
				execs[i] = directMarketExecution{
					Result:  directRejectedMinSizeTradeResult(req),
					Success: false,
				}
			}
			return execs
		}
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
		ExactShares:    side == api.SideBuy,
	}
	if req.Side == api.SideBuy && !hasActionableSubmittedDirectOrderValue(req) {
		return directMarketExecution{
			Result:  directRejectedMinSizeTradeResult(req),
			Success: false,
		}
	}
	result, err := submitDirectMarketOrder(ctx, trader, side, tokenID, outcome, price, size, feeRateBps)
	return finalizeDirectMarketExecutionWithSignals(ctx, trader, req, confirmTimeout, result, err)
}

func submitDirectMarketOrder(ctx context.Context, trader *trading.RealTrader, side api.Side, tokenID, outcome string, price, size float64, feeRateBps int) (*trading.TradeResult, error) {
	primeRealbotOrderPath(ctx, trader)

	if side == api.SideSell {
		return trader.Sell(ctx, tokenID, outcome, price, size, api.OrderTypeLimit, api.TIFFillAndKill, feeRateBps)
	}
	return trader.Buy(ctx, tokenID, outcome, price, size, api.OrderTypeLimit, api.TIFGoodTilCancelled, feeRateBps)
}

func confirmMarketOrderExecution(ctx context.Context, trader *trading.RealTrader, req directMarketOrderSignalRequest, orderID string, timeout time.Duration) (executedQty float64, wsConfirmed bool, orderConfirmed bool, verifyErr error) {
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
				wsConfirmed = hasConfirmedExecutedQty(req.Side, wsQty)
			}
		}

		liveBalance := trader.GetLivePositionSize(req.TokenID)
		if delta := executionDeltaFromLiveBalance(liveBalance, req.InitialBalance, req.Side); delta > executedQty {
			executedQty = delta
		}

		if hasConfirmedExecutedQty(req.Side, executedQty) || time.Now().After(deadline) {
			break
		}
		time.Sleep(realbotFillPollInterval)
	}

	if shouldCancelResidualBuyOrder(req, executedQty) && orderID != "" {
		cancelCtx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		cancelErr := trader.CancelOrderByID(cancelCtx, orderID)
		cancel()
		if cancelErr != nil && verifyErr == nil && !isIgnorableCancelOrderError(cancelErr) {
			verifyErr = fmt.Errorf("cancel residual buy failed: %w", cancelErr)
		}
		time.Sleep(100 * time.Millisecond)
	}

	refreshCtx, refreshCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer refreshCancel()
	if positions, err := trader.ForceRefreshPositions(refreshCtx); err == nil {
		if delta := executionDeltaFromPositions(positions, req.TokenID, req.InitialBalance, req.Side); delta > executedQty {
			executedQty = delta
		}
		verifyErr = nil
	}
	if orderID != "" {
		if wsQty := trader.GetConfirmedFillSize(orderID); wsQty > executedQty {
			executedQty = wsQty
			wsConfirmed = hasConfirmedExecutedQty(req.Side, wsQty)
		}
	}
	if hasConfirmedExecutedQty(req.Side, executedQty) {
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
