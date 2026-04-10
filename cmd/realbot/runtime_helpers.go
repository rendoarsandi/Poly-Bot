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
		return
	}
	if spendable, err := trader.GetBalance(cashCtx); err == nil {
		tui.SetWalletCash(spendable)
	}
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
	seq  uint64
	ask0 float64
	ask1 float64
}
