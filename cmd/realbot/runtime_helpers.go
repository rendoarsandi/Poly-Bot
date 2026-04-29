package main

import (
	"context"
	"strings"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

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

func realbotRefreshWalletCashDisplay(ctx context.Context, trader *trading.RealTrader, tui *paper.TUI, timeout time.Duration) {
	if trader == nil || tui == nil {
		return
	}
	cashCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if walletCash, err := trader.ForceRefreshOnChainUSDCBalance(cashCtx); err == nil {
		tui.SetWalletCash(walletCash)
	} else if spendable, err := trader.GetBalance(cashCtx); err == nil {
		tui.SetWalletCash(spendable)
	}
	// Also surface legacy USDC.e (wrap candidate) and POL (gas) balances for the
	// account panel so the operator can see what's available before pressing W/U.
	if usdce, err := trader.GetUSDCeBalance(cashCtx); err == nil {
		tui.SetWalletUSDCe(usdce)
	}
	if pol, err := trader.GetPOLBalance(cashCtx); err == nil {
		tui.SetWalletPOL(pol)
	}
}

func watchRealbotSleep(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// realbotBindCollateralWrapHandlers wires the TUI W/U keys to the trader's
// USDC.e <-> pUSD wrap/unwrap helpers. Only call this in real (non-paper) mode.
func realbotBindCollateralWrapHandlers(ctx context.Context, trader *trading.RealTrader, tui *paper.TUI) {
	if trader == nil || tui == nil || trader.IsEmbeddedPaperMode() {
		return
	}
	wrap := func(amount float64) {
		if amount <= 0 {
			tui.LogEvent("⚠️ Wrap aborted: amount must be > 0")
			return
		}
		txCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		tx, err := trader.WrapUSDCeToPUSD(txCtx, amount)
		if err != nil {
			tui.LogEvent("❌ Wrap failed: %v", err)
			return
		}
		tui.LogEvent("✅ Wrapped %.2f USDC.e → pUSD (tx %s)", amount, tx)
		realbotRefreshWalletCashDisplay(ctx, trader, tui, 8*time.Second)
	}
	unwrap := func(amount float64) {
		if amount <= 0 {
			tui.LogEvent("⚠️ Unwrap aborted: amount must be > 0")
			return
		}
		txCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		tx, err := trader.UnwrapPUSDToUSDCe(txCtx, amount)
		if err != nil {
			tui.LogEvent("❌ Unwrap failed: %v", err)
			return
		}
		tui.LogEvent("✅ Unwrapped %.2f pUSD → USDC.e (tx %s)", amount, tx)
		realbotRefreshWalletCashDisplay(ctx, trader, tui, 8*time.Second)
	}
	tui.SetCollateralWrapHandlers(wrap, unwrap)
}

func realbotLogGasBalance(ctx context.Context, polygonClient *api.PolygonClient, trader *trading.RealTrader, tui *paper.TUI, timeout time.Duration) {
	if polygonClient == nil || trader == nil || tui == nil || trader.IsEmbeddedPaperMode() {
		return
	}
	go func() {
		gasCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		maticBalance, err := polygonClient.GetMATICBalance(gasCtx, trader.Address())
		if err != nil {
			tui.LogEvent("⚠️ Could not fetch MATIC balance: %v", err)
			return
		}
		tui.LogEvent("⛽ Gas Balance: %.4f MATIC", maticBalance)
		if maticBalance < 0.1 {
			tui.LogEvent("⚠️ Low MATIC balance; live transactions may fail for gas")
		}
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

func realbotLadderedHoldMode(cfg paper.TUISettings) bool {
	return strings.EqualFold(normalizePaperArbMode(cfg.PaperArbMode), paperArbModeLaddered)
}

func realbotPrimaryExecutionMode(cfg paper.TUISettings) string {
	if realbotTakerCloseHoldMode(cfg) {
		return realbotExecutionModeTakerClose
	}
	return normalizePaperArbMode(cfg.PaperArbMode)
}

func realbotShouldAutoMergeBalancedInventory(cfg paper.TUISettings) bool {
	_ = cfg
	return false
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

func realbotHasActionableEnginePositionsForMarket(engine *paper.Engine, marketID string) bool {
	if engine == nil || marketID == "" {
		return false
	}
	for _, pos := range engine.GetPositions() {
		if pos.MarketID == marketID && hasActionableCleanupRemainder(pos.Quantity) {
			return true
		}
	}
	return false
}

func realbotDropDustOnlyEnginePositionsForMarket(engine *paper.Engine, marketID string) (int, float64) {
	if engine == nil || marketID == "" {
		return 0, 0
	}

	type dustPosition struct {
		outcome string
		qty     float64
	}

	dust := make([]dustPosition, 0, 2)
	for _, pos := range engine.GetPositions() {
		if pos.MarketID != marketID || !shouldAttemptCleanupSell(pos.Quantity) {
			continue
		}
		if hasActionableCleanupRemainder(pos.Quantity) {
			return 0, 0
		}
		dust = append(dust, dustPosition{outcome: pos.Outcome, qty: pos.Quantity})
	}
	if len(dust) == 0 {
		return 0, 0
	}

	dropped := 0
	totalShares := 0.0
	for _, pos := range dust {
		if engine.SyncExternalPosition(marketID, pos.outcome, 0, 0) {
			dropped++
			totalShares += pos.qty
		}
	}
	return dropped, totalShares
}

func realbotShouldRunNearExpiryCleanup(cfg paper.TUISettings, timeToExpiry, mergeBuffer time.Duration) bool {
	_ = cfg
	_ = timeToExpiry
	_ = mergeBuffer
	return false
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

type realbotQuoteState struct {
	UpdatedAt time.Time
	Source    string
}

type realbotAsyncEntryResult struct {
	lastTradeAt            time.Time
	cooldownUntil          time.Time
	ladderedEntrySeq       uint64
	ladderedEntryConfirmed bool
}

type realbotLadderedEntry struct {
	seq   uint64
	ask0  float64
	ask1  float64
	side  int
	rung  int
	armed bool
}
