package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

func TestPairBalancesFromPositions(t *testing.T) {
	positions := []trading.PositionInfo{
		{TokenID: "yes-token", Size: 12.5},
		{TokenID: "other-token", Size: 99},
		{TokenID: "no-token", Size: 7.25},
	}

	bal0, bal1 := pairBalancesFromPositions(positions, "yes-token", "no-token")
	if bal0 != 12.5 || bal1 != 7.25 {
		t.Fatalf("unexpected balances: got (%.2f, %.2f)", bal0, bal1)
	}
}

func TestPairBalancesFromPositionsMissingTokenDefaultsToZero(t *testing.T) {
	positions := []trading.PositionInfo{{TokenID: "yes-token", Size: 3}}

	bal0, bal1 := pairBalancesFromPositions(positions, "yes-token", "no-token")
	if bal0 != 3 || bal1 != 0 {
		t.Fatalf("unexpected balances with missing token: got (%.2f, %.2f)", bal0, bal1)
	}
}

func TestRealbotShouldMirrorExecutionIntoEngine(t *testing.T) {
	if !realbotShouldMirrorExecutionIntoEngine(nil) {
		t.Fatal("expected nil trader to require explicit engine sync")
	}
	if !realbotShouldMirrorExecutionIntoEngine(&trading.RealTrader{}) {
		t.Fatal("expected live trader to require explicit engine sync")
	}

	engine := paper.NewEngine(100)
	paperTrader := trading.NewEmbeddedPaperRealTrader(&core.Config{ExecutionBackend: core.ExecutionBackendPaper}, engine)
	if realbotShouldMirrorExecutionIntoEngine(paperTrader) {
		t.Fatal("expected embedded paper trader to skip duplicate engine sync")
	}
}

func TestLadderedTakerAskBoundsPreservesConfiguredRange(t *testing.T) {
	minAsk, maxAsk := ladderedTakerAskBounds(0.10, 0.90)
	if minAsk != 0.10 || maxAsk != 0.90 {
		t.Fatalf("unexpected laddered bounds %.2f-%.2f", minAsk, maxAsk)
	}
}

func TestLadderedTakerAskBoundsClampsToTradeableRange(t *testing.T) {
	minAsk, maxAsk := ladderedTakerAskBounds(0.001, 1.50)
	if minAsk != ladderedTakerMinAsk || maxAsk != ladderedTakerMaxAsk {
		t.Fatalf("unexpected clamped laddered bounds %.2f-%.2f", minAsk, maxAsk)
	}

	minAsk, maxAsk = ladderedTakerAskBounds(0.95, 0.40)
	if minAsk != 0.40 || maxAsk != 0.40 {
		t.Fatalf("expected inverted bounds to collapse at max ask, got %.2f-%.2f", minAsk, maxAsk)
	}
}

func TestLadderedTakerEntryEligibleRequiresSumAndSkew(t *testing.T) {
	if !ladderedTakerEntryEligible(0.72, 0.30) {
		t.Fatal("expected skewed pair inside ladder sum cap to be eligible")
	}
	if ladderedTakerEntryEligible(0.61, 0.60) {
		t.Fatal("expected pair above ladder sum cap to be ineligible")
	}
	if ladderedTakerEntryEligible(0.51, 0.50) {
		t.Fatal("expected low-skew pair to be ineligible")
	}
}

func TestRealbotBinanceGapBuyLimitPriceUsesFixedOneCentCap(t *testing.T) {
	if got := realbotBinanceGapBuyLimitPrice(0.54, 0.90); math.Abs(got-0.55) > 0.000001 {
		t.Fatalf("expected fixed 1c cap for binance-gap, got %.3f", got)
	}
}

func TestRealbotPriceWithinConfiguredRange(t *testing.T) {
	cfg := paper.TUISettings{MinAskPrice: 0.10, MaxAskPrice: 0.95}
	if !realbotPriceWithinConfiguredRange(0.95, cfg) {
		t.Fatal("expected upper bound to remain executable")
	}
	if realbotPriceWithinConfiguredRange(0.96, cfg) {
		t.Fatal("expected price above configured max to be rejected")
	}
	if realbotPriceWithinConfiguredRange(0.09, cfg) {
		t.Fatal("expected price below configured min to be rejected")
	}
}

func TestRealbotDirectionalBuyLimitPriceUsesAbsoluteSlippageCap(t *testing.T) {
	if got := realbotDirectionalBuyLimitPrice(0.95, 0.95, 99); math.Abs(got-0.99) > 0.000001 {
		t.Fatalf("expected buy cap to use 99c slippage limit instead of max ask filter, got %.3f", got)
	}
}

func TestRealbotDirectionalSellFloorPriceRespectsConfiguredMin(t *testing.T) {
	if got := realbotDirectionalSellFloorPrice(0.96, 0.95, 99); math.Abs(got-0.95) > 0.000001 {
		t.Fatalf("expected sell floor to stay at configured min 0.95, got %.3f", got)
	}
}

func TestRealbotEnsureTopAskLevelInjectsBBOWhenDepthMissing(t *testing.T) {
	levels := []paper.MarketLevel{{Price: 0.62, Size: 5}}
	updated := realbotEnsureTopAskLevel(levels, 0.60, 1.02)
	if len(updated) != 2 {
		t.Fatalf("expected injected top ask level, got %d levels", len(updated))
	}
	found := false
	for _, lvl := range updated {
		if math.Abs(lvl.Price-0.60) < 0.000001 {
			found = true
			if math.Abs(lvl.Size-1.02) > 0.000001 {
				t.Fatalf("expected injected top size 1.02, got %.4f", lvl.Size)
			}
		}
	}
	if !found {
		t.Fatalf("expected injected top ask 0.60, got %+v", updated)
	}
}

func TestRealbotEnsureTopAskLevelSkipsInjectionWhenTopAlreadyPresent(t *testing.T) {
	levels := []paper.MarketLevel{
		{Price: 0.58, Size: 2},
		{Price: 0.60, Size: 3},
	}
	updated := realbotEnsureTopAskLevel(levels, 0.60, 1.02)
	if len(updated) != len(levels) {
		t.Fatalf("expected no injected level when top already present, got %d levels", len(updated))
	}
}

func TestRealbotLadderedMoveThresholdMatchesPaperbotClamp(t *testing.T) {
	if got := realbotLadderedMoveThreshold(0.01); got != 0.01 {
		t.Fatalf("expected sub-1c threshold to clamp to 1c, got %.4f", got)
	}
	if got := realbotLadderedMoveThreshold(80); got != 0.25 {
		t.Fatalf("expected threshold to clamp to 25c, got %.4f", got)
	}
}

func TestRealbotLadderedDirectionalSideMatchesPaperbotCases(t *testing.T) {
	if side, _, ok := ladderedTakerDirectionalSide(nil, 0.62, 0.38, 1.0); !ok || side != 0 {
		t.Fatalf("expected initial higher-ask side 0, got side=%d ok=%v", side, ok)
	}
	if side, _, ok := ladderedTakerDirectionalSide([]realbotLadderedEntry{{ask0: 0.50, ask1: 0.40}}, 0.505, 0.401, 1.0); ok {
		t.Fatalf("expected move below threshold to block re-entry, got side=%d ok=%v", side, ok)
	}
	if side, _, ok := ladderedTakerDirectionalSide([]realbotLadderedEntry{{ask0: 0.50, ask1: 0.40}}, 0.512, 0.401, 1.0); !ok || side != 0 {
		t.Fatalf("expected side 0 re-entry, got side=%d ok=%v", side, ok)
	}
	if side, _, ok := ladderedTakerDirectionalSide([]realbotLadderedEntry{{ask0: 0.50, ask1: 0.40}}, 0.501, 0.412, 1.0); !ok || side != 1 {
		t.Fatalf("expected side 1 re-entry, got side=%d ok=%v", side, ok)
	}
}

func TestRealbotPendingLadderedEntryUsesCurrentQuoteOnLargeGaps(t *testing.T) {
	entries := []realbotLadderedEntry{{seq: 1, ask0: 0.50, ask1: 0.36}}

	pending := realbotPendingLadderedEntry(entries, 2, 0.62, 0.36, 2.0)
	if math.Abs(pending.ask0-0.62) > 1e-9 || math.Abs(pending.ask1-0.36) > 1e-9 {
		t.Fatalf("expected pending anchor to jump to the current quote, got %+v", pending)
	}

	entries = append(entries, pending)
	if side, mult, ok := ladderedTakerDirectionalSide(entries, 0.62, 0.36, 2.0); ok {
		t.Fatalf("expected unchanged quote to stop replaying missed rungs, got side=%d mult=%d ok=%v", side, mult, ok)
	}
}

func TestRealbotLargeGapRequiresFreshStepAfterAnchorReset(t *testing.T) {
	entries := []realbotLadderedEntry{{seq: 1, ask0: 0.50, ask1: 0.36}}
	entries = append(entries, realbotPendingLadderedEntry(entries, 2, 0.62, 0.36, 2.0))

	if side, mult, ok := ladderedTakerDirectionalSide(entries, 0.639, 0.36, 2.0); ok {
		t.Fatalf("expected move below the next full 2c step to stay blocked, got side=%d mult=%d ok=%v", side, mult, ok)
	}
	if side, mult, ok := ladderedTakerDirectionalSide(entries, 0.64, 0.36, 2.0); !ok || side != 0 || mult != 1 {
		t.Fatalf("expected the next fresh 2c move to allow exactly one new re-entry, got side=%d mult=%d ok=%v", side, mult, ok)
	}
}

func TestRealbotLadderedRequestedQtySharesModePreservesConfiguredSize(t *testing.T) {
	cfg := paper.TUISettings{
		LadderedTakerSizingMode: core.LadderedTakerSizingModeShares,
		LadderedTakerSizeShares: 3.5,
	}

	if got := realbotLadderedRequestedQty(0.84, cfg, 0.62, 0.64); math.Abs(got-3.5) > 1e-9 {
		t.Fatalf("expected share ladder sizing to keep configured 3.5 shares, got %.4f", got)
	}
}

func TestRealbotLadderedRequestedQtyUSDCSizesAgainstActiveSideLimit(t *testing.T) {
	cfg := paper.TUISettings{
		LadderedTakerSizingMode: core.LadderedTakerSizingModeUSDC,
		LadderedTakerSizeUSDC:   5.0,
	}

	if got := realbotLadderedRequestedQty(1.00, cfg, 0.80, 0.80); math.Abs(got-6.25) > 1e-9 {
		t.Fatalf("expected USDC ladder sizing to use the active side price, got %.4f", got)
	}
}

func TestRealbotLadderedRequestedQtyUSDCRespectsMaxTradeSize(t *testing.T) {
	cfg := paper.TUISettings{
		LadderedTakerSizingMode: core.LadderedTakerSizingModeUSDC,
		LadderedTakerSizeUSDC:   10.0,
		MaxTradeSize:            4.0,
	}

	want := normalizeMarketBuyShares(4.0 / 0.60)
	if got := realbotLadderedRequestedQty(0.90, cfg, 0.60, 0.61); math.Abs(got-want) > 1e-9 {
		t.Fatalf("expected max trade size to cap directional ladder sizing at %.4f, got %.4f", want, got)
	}
}

func TestRealbotResolveLadderedEntryDropsRejectedPendingAnchor(t *testing.T) {
	entries := []realbotLadderedEntry{
		{seq: 1, ask0: 0.50, ask1: 0.40},
		{seq: 2, ask0: 0.52, ask1: 0.40},
	}

	entries = realbotResolveLadderedEntry(entries, 2, false)

	if len(entries) != 1 {
		t.Fatalf("expected failed pending rung to be removed, got %d entries", len(entries))
	}
	if side, _, ok := ladderedTakerDirectionalSide(entries, 0.512, 0.401, 1.0); !ok || side != 0 {
		t.Fatalf("expected re-entry to fall back to the last confirmed ladder anchor, got side=%d ok=%v", side, ok)
	}
}

func TestRealbotShouldRetryLadderedBuyFailureTransientExecutionError(t *testing.T) {
	exec := directMarketExecution{
		Result: &trading.TradeResult{
			Message: "could not run the execution",
		},
	}

	if !realbotShouldRetryLadderedBuyFailure(exec) {
		t.Fatal("expected transient execution error to stay retryable for laddered buy flow")
	}
}

func TestRealbotShouldRetryLadderedBuyFailureIgnoresHardVenueRejects(t *testing.T) {
	exec := directMarketExecution{
		Result: &trading.TradeResult{
			Status:  "REJECTED",
			Message: "order rejected",
		},
	}

	if realbotShouldRetryLadderedBuyFailure(exec) {
		t.Fatal("expected hard venue reject not to arm laddered retries")
	}
}

func TestHasConfirmedExecutedQtyBuyUsesMinimumActionableThreshold(t *testing.T) {
	if !hasConfirmedExecutedQty(api.SideBuy, minOnChainActionShares) {
		t.Fatal("expected exact minimum actionable buy fill to count as confirmed")
	}
	if hasConfirmedExecutedQty(api.SideBuy, minOnChainActionShares-0.0001) {
		t.Fatal("expected sub-minimum buy fill to remain unconfirmed")
	}
}

func TestRealbotShouldAdvanceLadderedEntryTracksAnyActionableFill(t *testing.T) {
	if !realbotShouldAdvanceLadderedEntry(10.0, 0.01) {
		t.Fatal("expected minimum actionable partial fill to advance ladder anchor")
	}
	if !realbotShouldAdvanceLadderedEntry(10.0, 9.6) {
		t.Fatal("expected materially filled rung to advance ladder anchor")
	}
	if !realbotShouldAdvanceLadderedEntry(0.01, 0.01) {
		t.Fatal("expected minimum actionable rung to advance when fully filled")
	}
}

func TestRealbotLadderedRecoveredFillQtyUsesRequestedSideOnly(t *testing.T) {
	if got := realbotLadderedRecoveredFillQty(0, 1.02, 1.50, 9.0); math.Abs(got-1.02) > 1e-9 {
		t.Fatalf("expected side 0 recovery to clamp to requested qty, got %.4f", got)
	}
	if got := realbotLadderedRecoveredFillQty(1, 1.02, 9.0, 0.80); math.Abs(got-0.80) > 1e-9 {
		t.Fatalf("expected side 1 recovery to use the requested side only, got %.4f", got)
	}
	if got := realbotLadderedRecoveredFillQty(0, 1.02, 0, 0.80); got != 0 {
		t.Fatalf("expected opposite-side recovery to be ignored, got %.4f", got)
	}
}

func TestRealbotVerifiedLadderedBuyFillUsesRecoveredBalanceDelta(t *testing.T) {
	filledQty, confirmed, authoritative := realbotVerifiedLadderedBuyFill(1.02, 1.02, 0.61, nil)
	if math.Abs(filledQty-0.61) > 1e-9 {
		t.Fatalf("expected recovered ladder qty 0.61, got %.4f", filledQty)
	}
	if !confirmed {
		t.Fatal("expected recovered ladder fill to stay confirmed")
	}
	if !authoritative {
		t.Fatal("expected recovered ladder fill to be authoritative")
	}
}

func TestRealbotVerifiedLadderedBuyFillRejectsMissingVerifiedShares(t *testing.T) {
	filledQty, confirmed, authoritative := realbotVerifiedLadderedBuyFill(1.02, 1.02, 0, nil)
	if filledQty != 0 {
		t.Fatalf("expected missing verified ladder fill to zero out optimistic qty, got %.4f", filledQty)
	}
	if confirmed {
		t.Fatal("expected missing verified ladder fill to fail confirmation")
	}
	if !authoritative {
		t.Fatal("expected clean zero-qty verification to be authoritative")
	}
}

func TestRealbotVerifiedLadderedBuyFillFallsBackWhenVerificationUnavailable(t *testing.T) {
	filledQty, confirmed, authoritative := realbotVerifiedLadderedBuyFill(1.02, 0.98, 0, errors.New("refresh failed"))
	if math.Abs(filledQty-0.98) > 1e-9 {
		t.Fatalf("expected optimistic ladder qty fallback 0.98, got %.4f", filledQty)
	}
	if !confirmed {
		t.Fatal("expected optimistic ladder qty fallback to preserve confirmation")
	}
	if authoritative {
		t.Fatal("expected verification error fallback not to claim authoritative fill")
	}
}

func TestRealbotResolveInitialPairSnapshotPrefersAuthoritativeBalanceForLadderedMode(t *testing.T) {
	bal0, bal1, source, err := realbotResolveInitialPairSnapshot(context.Background(), true, 0.0, 0.0, func(context.Context) (float64, float64, string, error) {
		return 0.99944, 1.94674, "live WS + on-chain backup", nil
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if math.Abs(bal0-0.99944) > 1e-9 || math.Abs(bal1-1.94674) > 1e-9 {
		t.Fatalf("expected authoritative ladder baseline 0.99944/1.94674, got %.5f/%.5f", bal0, bal1)
	}
	if source != "live WS + on-chain backup" {
		t.Fatalf("expected authoritative source, got %q", source)
	}
}

func TestRealbotResolveInitialPairSnapshotFallsBackToLiveWhenAuthoritativeSnapshotFails(t *testing.T) {
	bal0, bal1, source, err := realbotResolveInitialPairSnapshot(context.Background(), true, 0.25, 0.75, func(context.Context) (float64, float64, string, error) {
		return 0, 0, "", errors.New("snapshot failed")
	})
	if err == nil {
		t.Fatal("expected authoritative snapshot error")
	}
	if math.Abs(bal0-0.25) > 1e-9 || math.Abs(bal1-0.75) > 1e-9 {
		t.Fatalf("expected live fallback 0.25/0.75, got %.5f/%.5f", bal0, bal1)
	}
	if source != "live WS cache" {
		t.Fatalf("expected live fallback source, got %q", source)
	}
}

func TestRealbotResolveInitialPairSnapshotSkipsAuthoritativeLookupOutsideLadderedMode(t *testing.T) {
	called := false
	bal0, bal1, source, err := realbotResolveInitialPairSnapshot(context.Background(), false, 0.40, 0.60, func(context.Context) (float64, float64, string, error) {
		called = true
		return 9, 9, "unexpected", nil
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if called {
		t.Fatal("expected non-laddered mode not to call authoritative snapshot loader")
	}
	if math.Abs(bal0-0.40) > 1e-9 || math.Abs(bal1-0.60) > 1e-9 {
		t.Fatalf("expected live baseline 0.40/0.60, got %.5f/%.5f", bal0, bal1)
	}
	if source != "live WS cache" {
		t.Fatalf("expected live snapshot source, got %q", source)
	}
}

func TestRealbotShouldAutoMergeBalancedInventory(t *testing.T) {
	if realbotShouldAutoMergeBalancedInventory(paper.TUISettings{PaperArbMode: paperArbModeLaddered}) {
		t.Fatal("expected laddered mode to keep balanced inventory parked")
	}
	if realbotShouldAutoMergeBalancedInventory(paper.TUISettings{PaperArbMode: paperArbModeTaker}) {
		t.Fatal("expected taker mode to keep balanced inventory parked")
	}
	if realbotShouldAutoMergeBalancedInventory(paper.TUISettings{PaperArbMode: paperArbModeMaker}) {
		t.Fatal("expected maker mode to keep balanced inventory parked")
	}
}

func TestRealbotLadderedHoldMode(t *testing.T) {
	if !realbotLadderedHoldMode(paper.TUISettings{PaperArbMode: paperArbModeLaddered}) {
		t.Fatal("expected laddered mode to preserve inventory for redemption")
	}
	if realbotLadderedHoldMode(paper.TUISettings{PaperArbMode: paperArbModeTaker}) {
		t.Fatal("expected taker mode to avoid laddered hold behavior")
	}
}

func TestRealbotPrimaryExecutionMode(t *testing.T) {
	if got := realbotPrimaryExecutionMode(paper.TUISettings{PaperArbMode: paperArbModeMaker}); got != paperArbModeMaker {
		t.Fatalf("expected maker mode, got %q", got)
	}
	if got := realbotPrimaryExecutionMode(paper.TUISettings{PaperArbMode: paperArbModeBinanceGap}); got != paperArbModeBinanceGap {
		t.Fatalf("expected binance-gap mode, got %q", got)
	}
	if got := realbotPrimaryExecutionMode(paper.TUISettings{PaperArbMode: paperArbModeCopytrade}); got != paperArbModeCopytrade {
		t.Fatalf("expected copytrade mode, got %q", got)
	}
	if got := realbotPrimaryExecutionMode(paper.TUISettings{PaperArbMode: paperArbModeTaker, TakerCloseMarket: true}); got != realbotExecutionModeTakerClose {
		t.Fatalf("expected taker-close to override arb mode, got %q", got)
	}
	if got := realbotPrimaryExecutionMode(paper.TUISettings{PaperArbMode: paperArbModeMaker, TakerCloseMarket: true}); got != paperArbModeMaker {
		t.Fatalf("expected maker mode to stay exclusive even when taker-close is toggled, got %q", got)
	}
}

func TestRealbotTUISettingsRoundTripIncludesLadderedSlippage(t *testing.T) {
	cfg := &core.Config{
		ExecutionBackend:            core.ExecutionBackendPaper,
		PaperBalance:                42.5,
		PaperArbMode:                paperArbModeLaddered,
		LadderedTakerMaxSlippagePct: 7,
	}

	settings := realbotTUISettingsFromConfig(cfg)
	if settings.ExecutionBackend != core.ExecutionBackendPaper {
		t.Fatalf("expected TUI settings to include execution backend, got %q", settings.ExecutionBackend)
	}
	if settings.PaperBalance != 42.5 {
		t.Fatalf("expected TUI settings to include paper balance, got %.2f", settings.PaperBalance)
	}
	if settings.LadderedTakerMaxSlippagePct != 7 {
		t.Fatalf("expected TUI settings to include laddered slippage, got %.0f", settings.LadderedTakerMaxSlippagePct)
	}

	settings.ExecutionBackend = core.ExecutionBackendLive
	settings.PaperBalance = 77
	settings.LadderedTakerMaxSlippagePct = 13
	applyRealbotTUISettings(cfg, settings)
	if cfg.ExecutionBackend != core.ExecutionBackendLive {
		t.Fatalf("expected config to receive updated execution backend, got %q", cfg.ExecutionBackend)
	}
	if cfg.PaperBalance != 77 {
		t.Fatalf("expected config to receive updated paper balance, got %.2f", cfg.PaperBalance)
	}
	if cfg.LadderedTakerMaxSlippagePct != 13 {
		t.Fatalf("expected config to receive updated laddered slippage, got %.0f", cfg.LadderedTakerMaxSlippagePct)
	}
}

func TestRealbotInitBackendPaperMode(t *testing.T) {
	state, err := realbotInitBackend(context.Background(), &core.Config{
		ExecutionBackend: core.ExecutionBackendPaper,
		PaperBalance:     55.5,
		PolygonRPCURL:    "https://polygon-rpc.example",
	})
	if err != nil {
		t.Fatalf("expected embedded paper backend init to succeed, got %v", err)
	}
	if !state.embeddedPaper {
		t.Fatal("expected embedded paper backend flag to be true")
	}
	if state.trader != nil {
		t.Fatal("expected embedded paper init to defer trader creation until engine startup")
	}
	if math.Abs(state.startingBalance-55.5) > 0.000001 {
		t.Fatalf("expected embedded paper starting balance 55.5, got %.2f", state.startingBalance)
	}
	if state.polygonClient == nil {
		t.Fatal("expected polygon client to still be available for resolution checks")
	}
}

func TestApplyRealbotTUISettingsDisablesUnsupportedPaperBackendModes(t *testing.T) {
	cfg := &core.Config{}
	applyRealbotTUISettings(cfg, paper.TUISettings{
		ExecutionBackend:     core.ExecutionBackendPaper,
		PaperArbMode:         paperArbModeMaker,
		SplitStrategyEnabled: true,
	})

	if cfg.ExecutionBackend != core.ExecutionBackendPaper {
		t.Fatalf("expected paper execution backend, got %q", cfg.ExecutionBackend)
	}
	if cfg.PaperArbMode != paperArbModeTaker {
		t.Fatalf("expected unsupported maker mode to coerce to taker, got %q", cfg.PaperArbMode)
	}
	if cfg.SplitStrategyEnabled {
		t.Fatal("expected split strategy to be disabled on paper backend")
	}
}

func TestRealbotBindBackendTraderUsesLiveTraderWhenPresent(t *testing.T) {
	engine := paper.NewEngine(100)
	liveTrader := &trading.RealTrader{}

	got := realbotBindBackendTrader(&core.Config{ExecutionBackend: core.ExecutionBackendLive}, engine, &realbotBackendState{
		trader: liveTrader,
	})
	if got != liveTrader {
		t.Fatal("expected live backend binding to reuse the initialized trader")
	}
}

func TestRealbotBindBackendTraderCreatesEmbeddedPaperFallback(t *testing.T) {
	engine := paper.NewEngine(100)

	got := realbotBindBackendTrader(&core.Config{ExecutionBackend: core.ExecutionBackendPaper}, engine, &realbotBackendState{
		embeddedPaper: true,
	})
	if got == nil {
		t.Fatal("expected embedded paper backend binding to create a trader")
	}
	if !got.IsEmbeddedPaperMode() {
		t.Fatal("expected embedded paper backend binding to create an embedded paper trader")
	}
}

func TestNormalizePaperArbModeSupportsBinanceGap(t *testing.T) {
	if got := normalizePaperArbMode("binance-gap"); got != paperArbModeBinanceGap {
		t.Fatalf("normalizePaperArbMode(binance-gap) = %q, want %q", got, paperArbModeBinanceGap)
	}
}

func TestRealbotCopytradeShouldUsePublicActivityAPI(t *testing.T) {
	wallet := "0x0000000000000000000000000000000000000001"

	if !realbotCopytradeShouldUsePublicActivityAPI(nil) {
		t.Fatal("expected nil poller to allow public activity api")
	}

	minedOnly := &realbotCopytradePoller{
		minedWatcher: api.NewPolymarketMinedWatcher(
			"https://polygon-mainnet.infura.io/v3/test",
			&api.PolygonClient{},
			&api.RestClient{},
			wallet,
		),
	}
	if realbotCopytradeShouldUsePublicActivityAPI(minedOnly) {
		t.Fatal("expected mined-only watcher to disable public activity api")
	}

	pending := &realbotCopytradePoller{
		pendingWatcher: api.NewPolymarketPendingWatcher(
			"https://polygon-mainnet.g.alchemy.com/v2/test",
			&api.RestClient{},
			&api.PolygonClient{},
			wallet,
		),
	}
	if realbotCopytradeShouldUsePublicActivityAPI(pending) {
		t.Fatal("expected pending watcher to disable public activity api")
	}
}

func TestRealbotCopytradeSignalSummaryIncludesSourceAndTx(t *testing.T) {
	trade := api.PublicTrade{
		Outcome:         "Up",
		Side:            "buy",
		Size:            28.45,
		Source:          "onchain",
		TransactionHash: "0x1234567890abcdef",
	}
	if got := realbotCopytradeSignalSummary(trade); got != "BUY Up | master=28.45 | source=ONCHAIN | tx=0x12345678..." {
		t.Fatalf("unexpected summary %q", got)
	}
}

func TestRealbotCopytradeSignalSummaryDefaultsToPositionSource(t *testing.T) {
	trade := api.PublicTrade{
		Outcome: "Down",
		Side:    "sell",
		Size:    3,
	}
	if got := realbotCopytradeSignalSummary(trade); got != "SELL Down | master=3 | source=POSITION" {
		t.Fatalf("unexpected summary %q", got)
	}
}

func TestRealbotCopytradeMarketSelectableAllowsFinalSeconds(t *testing.T) {
	now := time.Unix(1700000000, 0)

	if !realbotCopytradeMarketSelectable(now, time.Time{}) {
		t.Fatal("expected zero end time to remain selectable")
	}
	if !realbotCopytradeMarketSelectable(now, now.Add(10*time.Second)) {
		t.Fatal("expected market with 10 seconds left to remain selectable")
	}
	if realbotCopytradeMarketSelectable(now, now.Add(-time.Second)) {
		t.Fatal("expected expired market to be rejected")
	}
}

func TestRealbotConditionIDsForMarketsDedupesAndSkipsBlank(t *testing.T) {
	condIDs := realbotConditionIDsForMarkets(map[string]*api.Market{
		"m1": {ConditionID: "cond-1"},
		"m2": {ConditionID: "cond-2"},
		"m3": {ConditionID: "cond-1"},
		"m4": {ConditionID: ""},
		"m5": nil,
	})

	if len(condIDs) != 2 {
		t.Fatalf("expected two unique condition ids, got %v", condIDs)
	}
	if condIDs[0] != "cond-1" || condIDs[1] != "cond-2" {
		t.Fatalf("unexpected condition ids %v", condIDs)
	}
}

func TestRealbotHandleLiveRecoverySkipsUnsupportedModes(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	currentBalance := 100.0
	nextAttempt := time.Now()
	panicBuyCooldown := time.Time{}
	lastDustNotice := time.Time{}

	handled := realbotHandleLiveRecovery(realbotLiveRecoveryArgs{
		ctx:         context.Background(),
		marketID:    "BTC",
		market:      &api.Market{Tokens: []api.Token{{TokenID: "down-token"}, {TokenID: "up-token"}}},
		outcomes:    []string{"Down", "Up"},
		primaryMode: paperArbModeLaddered,
		engine:      engine,
		tui:         tui,
		lastTrade:   time.Now().Add(-30 * time.Second),
	}, &realbotLiveRecoveryState{
		currentBalance:          &currentBalance,
		nextLiveRecoveryAttempt: &nextAttempt,
		panicBuyCooldown:        &panicBuyCooldown,
		lastDustRecoveryNotice:  &lastDustNotice,
	})
	if handled {
		t.Fatal("expected laddered mode to bypass live recovery handler")
	}
	if math.Abs(currentBalance-100.0) > 0.000001 {
		t.Fatalf("expected current balance unchanged, got %.2f", currentBalance)
	}
}

func TestRealbotHandleClosedMarketIgnoresActiveMarket(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	preserveWalletTruth := false

	handled := realbotHandleClosedMarket(realbotMarketClosureArgs{
		ladderCloseState: newRealbotLadderCloseState(),
		marketID: "BTC",
		market:   &api.Market{ConditionID: "cond-1"},
		endTime:  time.Now().Add(30 * time.Second),
		tui:      tui,
		engine:   engine,
	}, &realbotMarketClosureState{
		preserveWalletTruth: &preserveWalletTruth,
	})
	if handled {
		t.Fatal("expected active market to bypass closed-market handler")
	}
	if preserveWalletTruth {
		t.Fatal("expected active market to leave preserveWalletTruth unchanged")
	}
}

func TestRealbotHandleMarketShutdownPreservesTakerCloseInventory(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	tui.InitSettings(paper.TUISettings{TakerCloseMarket: true}, nil)
	if _, err := engine.BuyForMarket("BTC", "Up", 0.71, 3); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	preserveWalletTruth := false
	handled := realbotHandleMarketShutdown(realbotMarketShutdownArgs{
		globalCtx: context.Background(),
		marketID:  "BTC",
		endTime:   time.Now().Add(time.Minute),
		engine:    engine,
		tui:       tui,
	}, &realbotMarketClosureState{
		preserveWalletTruth: &preserveWalletTruth,
	})
	if !handled {
		t.Fatal("expected shutdown handler to take over")
	}
	if !preserveWalletTruth {
		t.Fatal("expected taker-close shutdown to preserve wallet-truth inventory")
	}
}

func TestRealbotHandleClosedMarketDropsDustInsteadOfWaitingForResolution(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	tui.InitSettings(paper.TUISettings{TakerCloseMarket: true}, nil)
	if _, err := engine.BuyForMarket("BTC", "Up", 0.71, 0.009); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	preserveWalletTruth := false
	handled := realbotHandleClosedMarket(realbotMarketClosureArgs{
		ladderCloseState: newRealbotLadderCloseState(),
		marketID: "BTC",
		market:   &api.Market{ConditionID: "cond-1"},
		endTime:  time.Now().Add(-time.Minute),
		outcomes: []string{"Down", "Up"},
		engine:   engine,
		tui:      tui,
	}, &realbotMarketClosureState{
		preserveWalletTruth: &preserveWalletTruth,
	})
	if !handled {
		t.Fatal("expected closed market handler to take over")
	}
	if preserveWalletTruth {
		t.Fatal("expected dust-only inventory not to be preserved for redemption")
	}
	if realbotHasEnginePositionsForMarket(engine, "BTC") {
		t.Fatal("expected dust-only closed-market inventory to be cleared")
	}
}

func TestRealbotHandleMarketShutdownDropsDustInsteadOfPreservingIt(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	tui.InitSettings(paper.TUISettings{TakerCloseMarket: true}, nil)
	if _, err := engine.BuyForMarket("BTC", "Up", 0.71, 0.009); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	preserveWalletTruth := false
	handled := realbotHandleMarketShutdown(realbotMarketShutdownArgs{
		globalCtx: context.Background(),
		marketID:  "BTC",
		endTime:   time.Now().Add(time.Minute),
		engine:    engine,
		tui:       tui,
	}, &realbotMarketClosureState{
		preserveWalletTruth: &preserveWalletTruth,
	})
	if !handled {
		t.Fatal("expected shutdown handler to take over")
	}
	if preserveWalletTruth {
		t.Fatal("expected dust-only shutdown inventory not to be preserved")
	}
	if realbotHasEnginePositionsForMarket(engine, "BTC") {
		t.Fatal("expected dust-only shutdown inventory to be cleared")
	}
}

func TestRealbotHandleMarketGuardsRespectsManualPause(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	tui.InitSettings(paper.TUISettings{}, nil)
	tui.SetTradingPaused(true)

	weekdayLogged := false
	manualLogged := false
	nextNearCloseCleanup := time.Time{}
	nearExpiryNoticeSent := false

	result := realbotHandleMarketGuards(realbotMarketGuardArgs{
		marketID: "BTC",
		endTime:  time.Now().Add(time.Minute),
		cfg:      &core.Config{},
		engine:   engine,
		tui:      tui,
	}, &realbotMarketGuardState{
		usWeekdayGateClosedLogged: &weekdayLogged,
		manualTradingPauseLogged:  &manualLogged,
		nextNearCloseCleanup:      &nextNearCloseCleanup,
		nearExpiryNoticeSent:      &nearExpiryNoticeSent,
	})
	if result.weekdayTradingAllowed != true {
		t.Fatal("expected default trading-hours gate to stay open")
	}
	if !result.manualTradingPaused || result.entryTradingAllowed {
		t.Fatalf("expected manual pause to block entries, got paused=%v entryAllowed=%v", result.manualTradingPaused, result.entryTradingAllowed)
	}
	if !manualLogged {
		t.Fatal("expected manual pause transition to be recorded")
	}
	if result.skip {
		t.Fatal("expected manual pause alone not to trigger near-expiry skip")
	}
}

func TestRealbotConsumeAsyncEntryResultUpdatesLoopState(t *testing.T) {
	entryExecutionInFlight := true
	lastTrade := time.Time{}
	panicBuyCooldown := time.Now()
	ladderedEntries := []realbotLadderedEntry{
		{seq: 1, ask0: 0.41, ask1: 0.59},
		{seq: 2, ask0: 0.42, ask1: 0.58},
	}
	done := make(chan realbotAsyncEntryResult, 1)
	expectedTrade := time.Now().Add(-2 * time.Second)
	expectedCooldown := time.Now().Add(10 * time.Second)
	done <- realbotAsyncEntryResult{
		lastTradeAt:            expectedTrade,
		cooldownUntil:          expectedCooldown,
		ladderedEntrySeq:       1,
		ladderedEntryConfirmed: false,
	}

	realbotConsumeAsyncEntryResult(done, &realbotAsyncEntryState{
		entryExecutionInFlight: &entryExecutionInFlight,
		ladderedEntries:        &ladderedEntries,
		lastTrade:              &lastTrade,
		panicBuyCooldown:       &panicBuyCooldown,
	})

	if entryExecutionInFlight {
		t.Fatal("expected async result to clear in-flight flag")
	}
	if !lastTrade.Equal(expectedTrade) {
		t.Fatalf("expected lastTrade %v, got %v", expectedTrade, lastTrade)
	}
	if !panicBuyCooldown.Equal(expectedCooldown) {
		t.Fatalf("expected cooldown %v, got %v", expectedCooldown, panicBuyCooldown)
	}
	if len(ladderedEntries) != 1 || ladderedEntries[0].seq != 2 {
		t.Fatalf("expected unresolved ladder entry to be pruned, got %+v", ladderedEntries)
	}
}

func TestRealbotHandleEntryBlockNoticeTracksTransitions(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	lastReason := ""

	realbotHandleEntryBlockNotice("BTC", true, "pending redemption", tui, &lastReason)
	if lastReason != "pending redemption" {
		t.Fatalf("expected block reason to persist, got %q", lastReason)
	}

	realbotHandleEntryBlockNotice("BTC", false, "", tui, &lastReason)
	if lastReason != "" {
		t.Fatalf("expected block reason to clear, got %q", lastReason)
	}
}

func TestRealbotBinanceSymbolForExactSlugMarket(t *testing.T) {
	cfg := &core.Config{BinanceQuoteAsset: "usdt"}
	got := realbotBinanceSymbolForMarket("btc-updown-15m-1767358800#deadbeef", cfg)
	if got != "BTCUSDT" {
		t.Fatalf("expected exact slug market to map to BTCUSDT, got %q", got)
	}
}

func TestRealbotResolveDirectionalOutcomesSupportsUpDownAndYesNo(t *testing.T) {
	if mapping, ok := realbotResolveDirectionalOutcomes([]string{"Up", "Down"}); !ok || mapping.Up != "Up" || mapping.Down != "Down" {
		t.Fatalf("unexpected Up/Down mapping: %#v ok=%v", mapping, ok)
	}
	if mapping, ok := realbotResolveDirectionalOutcomes([]string{"Yes", "No"}); !ok || mapping.Up != "Yes" || mapping.Down != "No" {
		t.Fatalf("unexpected Yes/No mapping: %#v ok=%v", mapping, ok)
	}
}

func TestCombineCleanupVerificationBalancesPrefersOnChainTruth(t *testing.T) {
	bal0, bal1, source, err := combineCleanupVerificationBalances(
		4.9183, 0,
		4.9183, 0,
		0.0083, 0,
		nil, nil,
	)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if source != "on-chain truth" {
		t.Fatalf("expected on-chain truth source, got %q", source)
	}
	if bal0 != 0.0083 || bal1 != 0 {
		t.Fatalf("expected on-chain balances, got (%.4f, %.4f)", bal0, bal1)
	}
}

func TestCombineCleanupVerificationBalancesFallsBackWithoutOnChain(t *testing.T) {
	bal0, bal1, source, err := combineCleanupVerificationBalances(
		4.0, 0,
		3.5, 0,
		0, 0,
		nil, errors.New("rpc unavailable"),
	)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if source != "live WS + external position snapshot" {
		t.Fatalf("unexpected source %q", source)
	}
	if bal0 != 4.0 || bal1 != 0 {
		t.Fatalf("expected fallback to strongest local snapshot, got (%.2f, %.2f)", bal0, bal1)
	}
}

func TestClampExecutionMarginFloor(t *testing.T) {
	if got := clampExecutionMarginFloor(2.0, -1.0); got != -1.0 {
		t.Fatalf("expected -1.0 floor, got %.2f", got)
	}
	if got := clampExecutionMarginFloor(2.0, 3.0); got != 2.0 {
		t.Fatalf("expected floor clamped to observed gate 2.0, got %.2f", got)
	}
}

func TestMaxExecutablePairSum(t *testing.T) {
	if got := maxExecutablePairSum(-1.0, 0.90); got != 1.01 {
		t.Fatalf("expected max executable sum 1.01, got %.2f", got)
	}
	if got := maxExecutablePairSum(-5.0, 0.45); got != 0.90 {
		t.Fatalf("expected max executable sum capped by two-sided max ask 0.90, got %.2f", got)
	}
}

func TestMinExecutablePairSum(t *testing.T) {
	if got := minExecutablePairSum(-1.0, 0.10); got != 0.99 {
		t.Fatalf("expected min executable sum 0.99, got %.2f", got)
	}
	if got := minExecutablePairSum(-20.0, 0.45); got != 0.90 {
		t.Fatalf("expected min executable sum floored by two-sided min ask 0.90, got %.2f", got)
	}
}

func TestRealbotExecutionQuoteGuardAgeCapsAtAwaitingWindow(t *testing.T) {
	if got := realbotExecutionQuoteGuardAge(3 * time.Second); got != realbotExecutionGuardQuoteMaxAge {
		t.Fatalf("expected execution guard to cap at %s, got %s", realbotExecutionGuardQuoteMaxAge, got)
	}
}

func TestRealbotExecutionQuoteGuardAgePreservesStricterConfig(t *testing.T) {
	if got := realbotExecutionQuoteGuardAge(500 * time.Millisecond); got != 500*time.Millisecond {
		t.Fatalf("expected stricter configured age to pass through, got %s", got)
	}
}

func TestRealbotTakerCloseBudgetUsesCurrentEquityWhenAboveCash(t *testing.T) {
	budget := realbotTakerCloseBudget(59.20, 65.80, paper.TUISettings{
		TradeScaleFactor: 0.05,
	})
	if math.Abs(budget-3.29) > 0.000001 {
		t.Fatalf("expected taker-close budget 3.29, got %.2f", budget)
	}
}

func TestRealbotTakerCloseBudgetGrowsWithCurrentCashWhenAboveEquity(t *testing.T) {
	budget := realbotTakerCloseBudget(72.00, 72.00, paper.TUISettings{
		TradeScaleFactor: 0.05,
	})
	if math.Abs(budget-3.60) > 0.000001 {
		t.Fatalf("expected taker-close budget 3.60, got %.2f", budget)
	}
}

func TestRealbotTakerCloseBudgetUsesCurrentEquityAfterDrawdown(t *testing.T) {
	budget := realbotTakerCloseBudget(59.20, 59.20, paper.TUISettings{
		TradeScaleFactor: 0.05,
	})
	if math.Abs(budget-2.96) > 0.000001 {
		t.Fatalf("expected taker-close budget to follow current equity 2.96, got %.2f", budget)
	}
}

func TestRealbotTakerCloseBudgetSupportsFixedUSDCMode(t *testing.T) {
	budget := realbotTakerCloseBudget(59.20, 65.80, paper.TUISettings{
		TradeSizingMode: core.TradeSizingModeUSDC,
		TradeSizeUSDC:   2.3,
	})
	if math.Abs(budget-2.3) > 0.000001 {
		t.Fatalf("expected fixed taker-close budget 2.3, got %.2f", budget)
	}
}

func TestRealbotSizingCapitalForTradeUsesHighWaterOutsideTakerClose(t *testing.T) {
	engine := paper.NewEngine(100.0)
	engine.UpdateCompoundMultiplier(20.0, 100.0)
	if _, err := engine.BuyForMarket("BTC", "Up", 0.50, 10.0); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}
	if got := realbotSizingCapitalForTrade(engine, paper.TUISettings{}); math.Abs(got-120.0) > 0.000001 {
		t.Fatalf("expected live sizing capital to keep high-water 120.00, got %.2f", got)
	}
}

func TestRealbotSizingCapitalForTradeUsesCurrentEquityInTakerClose(t *testing.T) {
	engine := paper.NewEngine(100.0)
	engine.UpdateCompoundMultiplier(20.0, 100.0)
	if _, err := engine.BuyForMarket("BTC", "Up", 0.50, 10.0); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}
	if got := realbotSizingCapitalForTrade(engine, paper.TUISettings{TakerCloseMarket: true}); math.Abs(got-100.0) > 0.000001 {
		t.Fatalf("expected taker-close sizing capital to use book equity 100.00, got %.2f", got)
	}
}

func TestRealbotNewEntryBlockReasonBlocksForPriorRoundInventory(t *testing.T) {
	engine := paper.NewEngine(100.0)
	if _, err := engine.BuyForMarket("BTC-older", "Up", 0.50, 5.0); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	reason, blocked := realbotNewEntryBlockReason(nil,"BTC-new", engine, nil, paper.TUISettings{
		BlockNewEntriesOnPendingRedemption: true,
	})
	if !blocked || !strings.Contains(reason, "BTC-older") {
		t.Fatalf("expected prior-round inventory block, got blocked=%v reason=%q", blocked, reason)
	}
	if reason, blocked = realbotNewEntryBlockReason(nil,"BTC-older", engine, nil, paper.TUISettings{
		BlockNewEntriesOnPendingRedemption: true,
	}); blocked || reason != "" {
		t.Fatalf("expected no block on current market, got blocked=%v reason=%q", blocked, reason)
	}
}

func TestRealbotNewEntryBlockReasonBlocksForPendingRedemptionPayout(t *testing.T) {
	engine := paper.NewEngine(100.0)
	engine.SetPendingRedemption("BTC-older", 12.0)

	reason, blocked := realbotNewEntryBlockReason(nil,"BTC-new", engine, nil, paper.TUISettings{
		BlockNewEntriesOnPendingRedemption: true,
	})
	if !blocked || !strings.Contains(reason, "BTC-older") {
		t.Fatalf("expected pending-redemption block, got blocked=%v reason=%q", blocked, reason)
	}
}

func TestRealbotNewEntryBlockReasonBlocksForGroupedInventory(t *testing.T) {
	engine := paper.NewEngine(100.0)
	if _, err := engine.BuyForMarket("BTC-older", "Down", 0.18, 22.1457); err != nil {
		t.Fatalf("seed down buy failed: %v", err)
	}
	if _, err := engine.BuyForMarket("BTC-older", "Up", 0.89, 2.2405); err != nil {
		t.Fatalf("seed up buy failed: %v", err)
	}

	reason, blocked := realbotNewEntryBlockReason(nil,"BTC-new", engine, nil, paper.TUISettings{
		BlockNewEntriesOnPendingRedemption: true,
	})
	if !blocked || !strings.Contains(reason, "BTC-older") {
		t.Fatalf("expected grouped-inventory block, got blocked=%v reason=%q", blocked, reason)
	}
}

func TestRealbotNewEntryBlockReasonDisabledSettingAllowsEntries(t *testing.T) {
	engine := paper.NewEngine(100.0)
	if _, err := engine.BuyForMarket("BTC-older", "Up", 0.50, 5.0); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}
	engine.SetPendingRedemption("BTC-older", 12.0)

	reason, blocked := realbotNewEntryBlockReason(nil,"BTC-new", engine, nil, paper.TUISettings{
		BlockNewEntriesOnPendingRedemption: false,
	})
	if blocked || reason != "" {
		t.Fatalf("expected setting OFF to allow entries, got blocked=%v reason=%q", blocked, reason)
	}
}

func TestRealbotLateRedeemBlocksLadderEntryUntilNextWindow(t *testing.T) {
	engine := paper.NewEngine(100.0)
	now := time.Now()
	currentStart := now.Add(-2 * time.Minute).Unix()
	previousStart := now.Add(-7 * time.Minute).Unix()
	previousMarketID := fmt.Sprintf("btc-updown-5m-%d", previousStart)
	currentMarketID := fmt.Sprintf("btc-updown-5m-%d", currentStart)

	engine.SetPendingRedemption(previousMarketID, 7.5)
	if got := engine.SettlePendingRedemption(previousMarketID); math.Abs(got-7.5) > 0.000001 {
		t.Fatalf("expected pending redemption settle 7.5, got %.2f", got)
	}

	settled := engine.GetSettledRedemptions()
	settledAt := settled[previousMarketID]
	if settledAt.IsZero() {
		t.Fatal("expected settled redemption timestamp to be recorded")
	}

	marketStart, ok := realbotMarketWindowStart(currentMarketID)
	if !ok {
		t.Fatal("expected to parse current market window start")
	}
	if settledAt.Before(marketStart) {
		t.Fatal("expected settlement timestamp to occur after current market start in this test")
	}

	reason, blocked := realbotEntryBlockReason(nil,currentMarketID, engine, nil, paper.TUISettings{
		PaperArbMode:                       "laddered-taker",
		BlockNewEntriesOnPendingRedemption: true,
	})
	if !blocked || !strings.Contains(reason, "fresh next market") {
		t.Fatalf("expected late redemption to block current ladder market, got blocked=%v reason=%q", blocked, reason)
	}
}

func TestRealbotMarketWindowHelpersSupportHumanReadableHourlySlug(t *testing.T) {
	marketID := "bitcoin-up-or-down-april-19-2026-2am-et"

	if got := realbotMarketWindowDuration(marketID); got != time.Hour {
		t.Fatalf("expected hourly window duration, got %v", got)
	}
	start, ok := realbotMarketWindowStart(marketID)
	if !ok {
		t.Fatal("expected to parse hourly human-readable market window start")
	}
	wantStart := time.Date(2026, time.April, 19, 6, 0, 0, 0, time.UTC)
	if !start.Equal(wantStart) {
		t.Fatalf("expected hourly window start %s, got %s", wantStart, start)
	}
	if got := realbotMarketSeriesKey(marketID); got != "bitcoin-up-or-down-1h" {
		t.Fatalf("expected hourly series key, got %q", got)
	}
}

func TestRealbotLateRedeemAllowsImmediateLadderReentryWhenConfigured(t *testing.T) {
	engine := paper.NewEngine(100.0)
	now := time.Now()
	previousMarketID := fmt.Sprintf("btc-updown-5m-%d", now.Add(-7*time.Minute).Unix())
	currentMarketID := fmt.Sprintf("btc-updown-5m-%d", now.Add(-2*time.Minute).Unix())
	engine.SetPendingRedemption(previousMarketID, 5.0)
	_ = engine.SettlePendingRedemption(previousMarketID)

	reason, blocked := realbotEntryBlockReason(nil,currentMarketID, engine, nil, paper.TUISettings{
		PaperArbMode:                       "laddered-taker",
		BlockNewEntriesOnPendingRedemption: true,
		RedeemEntryTiming:                  core.RedeemEntryTimingImmediate,
	})
	if blocked || reason != "" {
		t.Fatalf("expected immediate mode to skip next-window wait, got blocked=%v reason=%q", blocked, reason)
	}
}

func TestRealbotLateRedeemDoesNotBlockNonLadderModes(t *testing.T) {
	engine := paper.NewEngine(100.0)
	now := time.Now()
	previousMarketID := fmt.Sprintf("btc-updown-5m-%d", now.Add(-7*time.Minute).Unix())
	currentMarketID := fmt.Sprintf("btc-updown-5m-%d", now.Add(-2*time.Minute).Unix())
	engine.SetPendingRedemption(previousMarketID, 5.0)
	_ = engine.SettlePendingRedemption(previousMarketID)

	reason, blocked := realbotEntryBlockReason(nil,currentMarketID, engine, nil, paper.TUISettings{
		PaperArbMode:                       "taker",
		BlockNewEntriesOnPendingRedemption: true,
	})
	if blocked || reason != "" {
		t.Fatalf("expected non-ladder mode to ignore late redemption window block, got blocked=%v reason=%q", blocked, reason)
	}
}

func TestRealbotShouldKeepPendingRedeemTxTracksConfirmationTimeouts(t *testing.T) {
	err := errors.New("redeem tx 0xabc confirmation pending: timeout waiting for transaction")
	if !realbotShouldKeepPendingRedeemTx("0xabc", err) {
		t.Fatal("expected pending redeem tx to stay in-flight")
	}
	if realbotShouldKeepPendingRedeemTx("", err) {
		t.Fatal("expected empty tx hash not to be tracked")
	}
}

func TestRealbotShouldKeepPendingRedeemTxIgnoresHardFailures(t *testing.T) {
	err := errors.New("transaction reverted")
	if realbotShouldKeepPendingRedeemTx("0xabc", err) {
		t.Fatal("expected hard failure not to be tracked as pending")
	}
}

func TestDirectExecutionTxSummaryIncludesAllReturnedHashes(t *testing.T) {
	exec := directMarketExecution{
		Result: &trading.TradeResult{
			TransactionsHashes: []string{
				"0x9071f607f4c2ae4b7b4a4849ca1052b7798011540fcb3759536368225a1a186c",
				"0x1056093066fcc6225983d769b6951bbf0c72f15a7af21ffa5f8c893395722474",
			},
		},
	}

	summary := directExecutionTxSummary(exec)
	if !strings.Contains(summary, "2 txs [") {
		t.Fatalf("expected multi-tx count in summary, got %q", summary)
	}
	if !strings.Contains(summary, "0x9071f607f4...") {
		t.Fatalf("expected first tx hash in summary, got %q", summary)
	}
	if !strings.Contains(summary, "0x1056093066...") {
		t.Fatalf("expected second tx hash in summary, got %q", summary)
	}
}

func TestAttributedSellFillFallsBackToAcknowledgedQty(t *testing.T) {
	exec := directMarketExecution{
		ExecutedQty:     0,
		AcknowledgedQty: 0.51,
	}

	got := attributedSellFill(exec, 0.75)
	if math.Abs(got-0.51) > 0.000001 {
		t.Fatalf("expected acknowledged sell qty fallback 0.51, got %.4f", got)
	}
}

func TestRealbotEntryGateAllowsOnlyOneConcurrentAcquire(t *testing.T) {
	gate := newRealbotEntryGate()
	if !gate.TryAcquire() {
		t.Fatal("expected first acquire to succeed")
	}
	if gate.TryAcquire() {
		t.Fatal("expected second acquire to be blocked while gate is held")
	}
	gate.Release()
	if !gate.TryAcquire() {
		t.Fatal("expected acquire to succeed again after release")
	}
	gate.Release()
}

func TestRealbotNeutralRoundPnLExcludesWalletTruthReconciliationDelta(t *testing.T) {
	roundPnL := realbotNeutralRoundPnL(64.67, 74.13, 9.46)
	if math.Abs(roundPnL) > 0.000001 {
		t.Fatalf("expected reconciliation delta to stay neutral, got %.4f", roundPnL)
	}
}

func TestRealbotRoundSnapshotPnLUsesNeutralizedBookDeltaForLiveBackend(t *testing.T) {
	engine := paper.NewEngine(100.0)
	engine.AddRealizedPnL(0.29)

	snapshot := realbotRoundSnapshot{
		startingEquity: 33.38,
		startRealized:  0.00,
	}

	got := realbotRoundSnapshotPnL(&trading.RealTrader{}, engine, snapshot, 33.66, 0.02)
	if math.Abs(got-0.26) > 0.000001 {
		t.Fatalf("expected live round snapshot pnl to follow neutralized book delta 0.26, got %.4f", got)
	}
}

func TestRealbotRoundSnapshotPnLUsesNeutralizedBookDeltaForPaperMode(t *testing.T) {
	engine := paper.NewEngine(100.0)
	engine.AddRealizedPnL(4.50)
	snapshot := realbotRoundSnapshot{
		startingEquity: 64.67,
		startRealized:  1.23,
	}

	got := realbotRoundSnapshotPnL(nil, engine, snapshot, 74.13, 9.46)
	if math.Abs(got) > 0.000001 {
		t.Fatalf("expected paper-mode snapshot pnl to follow neutralized book delta 0.00, got %.4f", got)
	}
}

func TestRealbotBeginRoundUsesBookEquityForSnapshotStart(t *testing.T) {
	engine := paper.NewEngine(36.60)
	engine.AddRealizedPnL(29.24)
	tui := paper.NewTUI(engine, paper.NewOrderBook())

	snapshot, currentBalance := realbotBeginRound(context.Background(), nil, engine, tui, 36.60)

	if math.Abs(snapshot.startingEquity-36.60) > 0.000001 {
		t.Fatalf("expected round snapshot to start from book equity 36.60, got %.4f", snapshot.startingEquity)
	}
	if math.Abs(currentBalance-36.60) > 0.000001 {
		t.Fatalf("expected current balance to remain 36.60 when sync fails, got %.4f", currentBalance)
	}
}

func TestNormalizeExecutionToleranceFractionSupportsLegacyPercentAndDecimalForms(t *testing.T) {
	if got := normalizeExecutionToleranceFraction(-1.0); math.Abs(got-0.01) > 0.000001 {
		t.Fatalf("expected -1.0 to normalize to 1%%, got %.6f", got)
	}
	if got := normalizeExecutionToleranceFraction(-0.01); math.Abs(got-0.01) > 0.000001 {
		t.Fatalf("expected -0.01 to normalize to 1%%, got %.6f", got)
	}
}

func TestBuildRealbotTakerClosePlanRespectsBuyExecSizing(t *testing.T) {
	plan, err := buildRealbotTakerClosePlan(50, 0.60, paper.TUISettings{
		BuyExecutionMarginFloorPercent: -1.0,
		TakerCloseMarketSlippage:       0.99,
		TakerCloseMarketMinPrice:       0.60,
	})
	if err != nil {
		t.Fatalf("expected plan, got %v", err)
	}
	if math.Abs(plan.SizingPrice-0.99) > 0.000001 {
		t.Fatalf("expected sizing price 0.99, got %.6f", plan.SizingPrice)
	}
	if math.Abs(plan.RequestedQty-50.5050) > 0.000001 {
		t.Fatalf("expected 50.5050 shares, got %.4f", plan.RequestedQty)
	}
}

func TestBuildRealbotTakerClosePlanFloorsLimitToMinPrice(t *testing.T) {
	plan, err := buildRealbotTakerClosePlan(20, 0.60, paper.TUISettings{
		BuyExecutionMarginFloorPercent: -1.0,
		TakerCloseMarketSlippage:       0.55,
		TakerCloseMarketMinPrice:       0.60,
	})
	if err != nil {
		t.Fatalf("expected plan, got %v", err)
	}
	if math.Abs(plan.LimitPrice-0.60) > 0.000001 {
		t.Fatalf("expected limit floor at min price 0.60, got %.3f", plan.LimitPrice)
	}
	if math.Abs(plan.SizingPrice-0.60) > 0.000001 {
		t.Fatalf("expected sizing price capped by limit 0.60, got %.3f", plan.SizingPrice)
	}
}

func TestRealbotRoundedLimitBuyCostMatchesVenueCentRounding(t *testing.T) {
	cost := realbotRoundedLimitBuyCost(0.501, 1)
	if math.Abs(cost-0.51) > 0.000001 {
		t.Fatalf("expected rounded venue cost 0.51, got %.4f", cost)
	}
}

func TestRealbotClampBuySharesToBudgetUsesCapPriceNotObservedQuote(t *testing.T) {
	shares := realbotClampBuySharesToBudget(2, 2.0, 0.50, 0.51)
	if math.Abs(shares-1.98) > 0.000001 {
		t.Fatalf("expected clamp to 1.9800 shares, got %.4f", shares)
	}
}

func TestRealbotClampBuySharesToBudgetKeepsMarketLikeFractionalShares(t *testing.T) {
	shares := realbotClampBuySharesToBudget(3.3131, 3.28, 0.50, 0.49)
	if math.Abs(shares-3.3061) > 0.000001 {
		t.Fatalf("expected clamp to 3.3061 shares, got %.4f", shares)
	}
}

func TestNormalizeMarketBuySharesUsesFourDecimals(t *testing.T) {
	if got := normalizeMarketBuyShares(3.31319); got != 3.3131 {
		t.Fatalf("expected 3.3131, got %.4f", got)
	}
	if got := normalizeMarketBuyShares(0.00009); got != 0 {
		t.Fatalf("expected dust to round down to 0, got %.5f", got)
	}
}

func TestRealbotShouldSkipStaleQuoteUpdateOnlyWhenCurrentQuoteIsAlreadySane(t *testing.T) {
	now := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	quoteState := map[string]realbotQuoteState{
		"Up": {UpdatedAt: now, Source: "ws"},
	}

	if !realbotShouldSkipStaleQuoteUpdate(quoteState, "Up", now.Add(-250*time.Millisecond), 0.45, 0.46) {
		t.Fatal("expected stale update to be ignored when current quote is already sane")
	}
	if realbotShouldSkipStaleQuoteUpdate(quoteState, "Up", now.Add(-250*time.Millisecond), 0, 0) {
		t.Fatal("expected stale update to be allowed when current quote is unusable")
	}
}

func TestRealbotShouldClearLocalPairQuotesKeepsTerminalOneSidedBook(t *testing.T) {
	outcomes := []string{"Down", "Up"}
	bids := map[string]float64{"Down": 0.99, "Up": 0}
	asks := map[string]float64{"Down": 0, "Up": 0.01}

	if realbotShouldClearLocalPairQuotes(outcomes, bids, asks) {
		t.Fatal("expected terminal-looking one-sided book to remain available locally")
	}
}

func TestRealbotSyncDisplayQuotesIgnoresTransientNonTerminalWSGap(t *testing.T) {
	outcomes := []string{"Down", "Up"}
	displayBids := map[string]float64{"Down": 0.44, "Up": 0.54}
	displayAsks := map[string]float64{"Down": 0.45, "Up": 0.55}
	liveBids := map[string]float64{"Down": 0, "Up": 0.54}
	liveAsks := map[string]float64{"Down": 0.45, "Up": 0}

	if realbotSyncDisplayQuotes(outcomes, liveBids, liveAsks, displayBids, displayAsks, false) {
		t.Fatal("expected transient WS gap to keep the existing display quotes")
	}
	if displayBids["Down"] != 0.44 || displayAsks["Up"] != 0.55 {
		t.Fatalf("expected prior display quotes to remain untouched, got bids=%v asks=%v", displayBids, displayAsks)
	}
}

func TestRealbotSyncDisplayQuotesPreservesTerminalDisplaySides(t *testing.T) {
	outcomes := []string{"Down", "Up"}
	displayBids := map[string]float64{"Down": 0.97, "Up": 0.02}
	displayAsks := map[string]float64{"Down": 0.98, "Up": 0.03}
	liveBids := map[string]float64{"Down": 0.99, "Up": 0}
	liveAsks := map[string]float64{"Down": 0, "Up": 0.01}

	if !realbotSyncDisplayQuotes(outcomes, liveBids, liveAsks, displayBids, displayAsks, false) {
		t.Fatal("expected terminal-looking update to refresh the display")
	}
	// With high-bid tolerance (Down bid 0.99 ≥ 0.60), the pair is now treated
	// as sane and all live values are published verbatim, including zero sides.
	// The terminal-book display path preserved prior quotes; the high-bid path
	// publishes the live snapshot directly.
	if displayBids["Down"] != 0.99 || displayAsks["Up"] != 0.01 {
		t.Fatalf("expected live terminal sides to be published, got bids=%v asks=%v", displayBids, displayAsks)
	}
}

func TestRealbotSyncDisplayQuotesLetsRESTClearBrokenPair(t *testing.T) {
	outcomes := []string{"Down", "Up"}
	displayBids := map[string]float64{"Down": 0.44, "Up": 0.54}
	displayAsks := map[string]float64{"Down": 0.45, "Up": 0.55}
	liveBids := map[string]float64{"Down": 0, "Up": 0}
	liveAsks := map[string]float64{"Down": 0, "Up": 0}

	if !realbotSyncDisplayQuotes(outcomes, liveBids, liveAsks, displayBids, displayAsks, true) {
		t.Fatal("expected authoritative REST update to clear the display")
	}
	if displayBids["Down"] != 0 || displayAsks["Up"] != 0 {
		t.Fatalf("expected display quotes to clear after REST confirmation, got bids=%v asks=%v", displayBids, displayAsks)
	}
}

func TestRealbotShouldLogTakerCloseStateLogsOnChangeAndThenThrottles(t *testing.T) {
	lastAt := time.Now()
	lastKey := "waiting"

	if !realbotShouldLogTakerCloseState(&lastAt, &lastKey, "submitted", time.Hour) {
		t.Fatal("expected changed taker-close state to log immediately")
	}

	prevAt := lastAt
	if realbotShouldLogTakerCloseState(&lastAt, &lastKey, "submitted", time.Hour) {
		t.Fatal("expected repeated taker-close state to be throttled")
	}
	if !lastAt.Equal(prevAt) {
		t.Fatal("expected throttled state to preserve last log timestamp")
	}
}

func TestRealbotShouldLogTakerCloseStateStaysSilentForSameStateAfterInterval(t *testing.T) {
	lastAt := time.Now().Add(-10 * time.Second)
	lastKey := "waiting"

	if realbotShouldLogTakerCloseState(&lastAt, &lastKey, "waiting", 5*time.Second) {
		t.Fatal("expected repeated taker-close state to remain silent even after interval")
	}
}

func TestRealbotWinningOnChainSharesOnlyCountsWinner(t *testing.T) {
	positions := []paper.WalletTruthPosition{
		{Outcome: "Up", OnChainShares: 3.1},
		{Outcome: "Down", OnChainShares: 2.0},
		{Outcome: "Up", OnChainShares: 0.4},
		{Outcome: "Up", OnChainShares: 0.00359},
	}
	if got := realbotWinningOnChainShares(positions, "Up"); math.Abs(got-3.5) > 0.000001 {
		t.Fatalf("expected winning on-chain shares 3.5, got %.4f", got)
	}
	if got := realbotWinningOnChainShares(positions, "Down"); math.Abs(got-2.0) > 0.000001 {
		t.Fatalf("expected winning on-chain shares 2.0, got %.4f", got)
	}
	if got := realbotWinningOnChainShares(positions, ""); got != 0 {
		t.Fatalf("expected empty winner to count as zero, got %.4f", got)
	}
}

func TestRealbotSyncEngineToWalletTruthForResolutionScalesLocalCostBasisToOnChainQty(t *testing.T) {
	engine := paper.NewEngine(100)
	if _, err := engine.BuyForMarket("BTC", "Up", 0.60, 3.0); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	adjusted, missing := realbotSyncEngineToWalletTruthForResolution(engine, "BTC", []paper.WalletTruthPosition{
		{MarketID: "BTC", Outcome: "Up", LocalShares: 3.0, OnChainShares: 3.1},
	})
	if adjusted != 1 {
		t.Fatalf("expected one adjusted outcome, got %d", adjusted)
	}
	if len(missing) != 0 {
		t.Fatalf("expected no missing-cost-basis outcomes, got %v", missing)
	}

	res := engine.RedeemWithDetails("BTC", "Up")
	if math.Abs(res.WinningShares-3.1) > 0.000001 {
		t.Fatalf("expected winning shares 3.1 after wallet-truth sync, got %.4f", res.WinningShares)
	}
	if math.Abs(res.WinningCost-1.86) > 0.000001 {
		t.Fatalf("expected winning cost 1.86 after proportional sync, got %.4f", res.WinningCost)
	}
}

func TestRealbotLooksLikeTerminalBookRecognizesPinnedEndState(t *testing.T) {
	terminal := realbotLooksLikeTerminalBook(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0.99, "Up": 0},
		map[string]float64{"Down": 0, "Up": 0.01},
	)
	if !terminal {
		t.Fatal("expected pinned 0.99/0.01 book to count as terminal-looking")
	}
}

func TestRealbotLooksLikeTerminalBookRecognizesRoundedPinnedEndState(t *testing.T) {
	terminal := realbotLooksLikeTerminalBook(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0.989, "Up": 0},
		map[string]float64{"Down": 0, "Up": 0.011},
	)
	if !terminal {
		t.Fatal("expected rounded terminal-looking book to bypass stale WS recovery")
	}
}

func TestRealbotLooksLikeTerminalBookRejectsNormalOneSidedBook(t *testing.T) {
	terminal := realbotLooksLikeTerminalBook(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0.64, "Up": 0},
		map[string]float64{"Down": 0, "Up": 0.36},
	)
	if terminal {
		t.Fatal("expected ordinary one-sided book to require normal WS freshness checks")
	}
}

func TestRealbotShouldRunNearExpiryCleanupIsDisabled(t *testing.T) {
	if realbotShouldRunNearExpiryCleanup(paper.TUISettings{TakerCloseMarket: true}, 5*time.Second, 30*time.Second) {
		t.Fatal("expected taker-close mode to bypass near-expiry cleanup")
	}
	if realbotShouldRunNearExpiryCleanup(paper.TUISettings{PaperArbMode: paperArbModeLaddered}, 5*time.Second, 30*time.Second) {
		t.Fatal("expected laddered mode to bypass near-expiry cleanup")
	}
	if realbotShouldRunNearExpiryCleanup(paper.TUISettings{}, 5*time.Second, 30*time.Second) {
		t.Fatal("expected near-expiry cleanup to be disabled in normal mode too")
	}
}

func TestRealbotHasEnginePositionsForMarket(t *testing.T) {
	engine := paper.NewEngine(100)
	if _, err := engine.BuyForMarket("btc-1", "Up", 0.70, 10); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}
	if !realbotHasEnginePositionsForMarket(engine, "btc-1") {
		t.Fatal("expected engine positions for btc-1")
	}
	if realbotHasEnginePositionsForMarket(engine, "eth-1") {
		t.Fatal("did not expect engine positions for eth-1")
	}
}

func TestRealbotDropDustOnlyEnginePositionsForMarket(t *testing.T) {
	engine := paper.NewEngine(100)
	if _, err := engine.BuyForMarket("btc-1", "Up", 0.70, 0.009); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	dropped, shares := realbotDropDustOnlyEnginePositionsForMarket(engine, "btc-1")
	if dropped != 1 {
		t.Fatalf("expected 1 dropped dust position, got %d", dropped)
	}
	if math.Abs(shares-0.009) > 0.000001 {
		t.Fatalf("expected 0.009 dropped shares, got %.6f", shares)
	}
	if realbotHasEnginePositionsForMarket(engine, "btc-1") {
		t.Fatal("expected dust-only market inventory to be cleared")
	}
}

func TestRealbotDropDustOnlyEnginePositionsForMarketKeepsActionableInventory(t *testing.T) {
	engine := paper.NewEngine(100)
	if _, err := engine.BuyForMarket("btc-1", "Up", 0.70, 0.02); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	dropped, shares := realbotDropDustOnlyEnginePositionsForMarket(engine, "btc-1")
	if dropped != 0 || shares != 0 {
		t.Fatalf("expected actionable inventory to remain, got dropped=%d shares=%.6f", dropped, shares)
	}
	if !realbotHasActionableEnginePositionsForMarket(engine, "btc-1") {
		t.Fatal("expected actionable market inventory to remain")
	}
}

func TestCheckRedemptionEmbeddedPaperSettlesResolvedPayout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/markets/cond-1" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"condition_id":"cond-1","closed":true,"tokens":[{"token_id":"down-token","outcome":"Down","winner":false},{"token_id":"up-token","outcome":"Up","winner":true}]}`))
	}))
	defer server.Close()

	engine := paper.NewEngine(100)
	if _, err := engine.BuyForMarket("BTC", "Up", 0.60, 5); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	tui := paper.NewTUI(engine, paper.NewOrderBook())
	tui.RecordRound(100, 97, -3, 1, engine.GetPositions(), nil)

	restClient := api.NewRestClient("polymarket")
	restClient.BaseURL = server.URL
	resCache := api.NewResolutionCache(nil, nil, restClient)
	trader := trading.NewEmbeddedPaperRealTrader(&core.Config{ExecutionBackend: core.ExecutionBackendPaper}, engine)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	checkRedemption(ctx, "BTC", "cond-1", []string{"Down", "Up"}, time.Now().Add(-time.Minute), trader, engine, tui, resCache)

	if got := engine.GetPendingRedemptions()["BTC"]; got != 0 {
		t.Fatalf("expected no pending redemption after embedded paper settle, got %.2f", got)
	}
	if got := engine.GetBalance(); math.Abs(got-102.0) > 0.000001 {
		t.Fatalf("expected embedded paper balance 102.00 after settlement, got %.2f", got)
	}

	history := tui.GetRoundHistory()
	if len(history) != 1 {
		t.Fatalf("expected one round history entry, got %d", len(history))
	}
	if got := history[0].EndingEquity; math.Abs(got-99.0) > 0.000001 {
		t.Fatalf("expected round ending equity 99.00 after redemption delta, got %.2f", got)
	}
	if got := history[0].PnL; math.Abs(got-(-1.0)) > 0.000001 {
		t.Fatalf("expected round pnl -1.00 after redemption delta, got %.2f", got)
	}
}

func TestRealbotFinalizePendingRedemptionClearsAlreadyReflectedBalance(t *testing.T) {
	engine := paper.NewEngine(92.0)
	engine.SetPendingRedemption("BTC", 10.0)

	neutralized := engine.SyncBalanceNeutral(102.0)
	if math.Abs(neutralized-10.0) > 0.000001 {
		t.Fatalf("expected neutralized reflected payout 10.00, got %.2f", neutralized)
	}
	if got := engine.GetPendingRedemptions()["BTC"]; got != 0 {
		t.Fatalf("expected reflected payout sync to clear pending redemption, got %.2f", got)
	}

	cleared := realbotFinalizePendingRedemption(engine, "BTC", true)
	if math.Abs(cleared) > 0.000001 {
		t.Fatalf("expected finalize to no-op after payout already reflected, got %.2f", cleared)
	}
	if got := engine.GetBalance(); math.Abs(got-102.0) > 0.000001 {
		t.Fatalf("expected reflected balance to stay 102.00 without double credit, got %.2f", got)
	}
	if got := engine.GetBookEquity(); math.Abs(got-102.0) > 0.000001 {
		t.Fatalf("expected book equity to stay 102.00 after clearing reflected payout, got %.2f", got)
	}
	if got := engine.GetPendingRedemptions()["BTC"]; got != 0 {
		t.Fatalf("expected pending redemption cleared after reflected payout, got %.2f", got)
	}
}

func TestRealbotFinalizePendingRedemptionSettlesWhenBalanceNotYetReflected(t *testing.T) {
	engine := paper.NewEngine(92.0)
	engine.SetPendingRedemption("BTC", 10.0)

	cleared := realbotFinalizePendingRedemption(engine, "BTC", false)
	if math.Abs(cleared-10.0) > 0.000001 {
		t.Fatalf("expected settled payout 10.00, got %.2f", cleared)
	}
	if got := engine.GetBalance(); math.Abs(got-102.0) > 0.000001 {
		t.Fatalf("expected settle path to credit balance to 102.00, got %.2f", got)
	}
	if got := engine.GetBookEquity(); math.Abs(got-102.0) > 0.000001 {
		t.Fatalf("expected book equity 102.00 after settle path, got %.2f", got)
	}
	if got := engine.GetPendingRedemptions()["BTC"]; got != 0 {
		t.Fatalf("expected no pending redemption after settle path, got %.2f", got)
	}
}

func TestEmbeddedPaperResolutionSweepSettlesHeldExpiredMarketBySlug(t *testing.T) {
	marketID := "btc-updown-5m-1700000000"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/markets/"+marketID {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"condition_id":"cond-btc-1","slug":"` + marketID + `","closed":true,"tokens":[{"token_id":"down-token","outcome":"Down","winner":false},{"token_id":"up-token","outcome":"Up","winner":true}]}`))
	}))
	defer server.Close()

	engine := paper.NewEngine(100)
	if _, err := engine.BuyForMarket(marketID, "Up", 0.60, 5); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}
	engine.UpdateMarketBidAsk(marketID, "Up", 0.99, 1.00)
	engine.UpdateMarketBidAsk(marketID, "Down", 0.01, 0.02)

	tui := paper.NewTUI(engine, paper.NewOrderBook())
	restClient := api.NewRestClient("polymarket")
	restClient.BaseURL = server.URL
	resCache := api.NewResolutionCache(nil, nil, restClient)
	trader := trading.NewEmbeddedPaperRealTrader(&core.Config{ExecutionBackend: core.ExecutionBackendPaper}, engine)

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	realbotStartEmbeddedPaperResolutionSweep(ctx, trader, engine, tui, restClient, resCache)

	deadline := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := engine.GetBalance(); math.Abs(got-102.0) <= 0.000001 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	if got := engine.GetBalance(); math.Abs(got-102.0) > 0.000001 {
		t.Fatalf("expected background embedded-paper sweep to settle to 102.00, got %.2f", got)
	}
	if got := engine.GetPendingRedemptions()[marketID]; got != 0 {
		t.Fatalf("expected no pending redemption after background settle, got %.2f", got)
	}
	if realbotHasEnginePositionsForMarket(engine, marketID) {
		t.Fatal("expected held expired market inventory to be redeemed and cleared")
	}
}

func TestRealbotPanicBuyCompletionGuardBlocksUnprofitableCompletion(t *testing.T) {
	engine := paper.NewEngine(100)
	if _, err := engine.BuyForMarket("mkt-1", "Yes", 0.62, 10); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	block, reason := realbotPanicBuyCompletionGuard(engine, "mkt-1", "Yes", "No", 0.45, 0.40, 2.0)
	if !block {
		t.Fatal("expected completion guard to block unprofitable pair completion")
	}
	if !strings.Contains(reason, "Yes") || !strings.Contains(reason, "No") {
		t.Fatalf("expected reason to mention affected outcomes, got %q", reason)
	}
}

func TestRealbotPanicBuyCompletionGuardAllowsProfitableCompletion(t *testing.T) {
	engine := paper.NewEngine(100)
	if _, err := engine.BuyForMarket("mkt-1", "Yes", 0.44, 10); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	block, reason := realbotPanicBuyCompletionGuard(engine, "mkt-1", "Yes", "No", 0.45, 0.40, 2.0)
	if block {
		t.Fatalf("expected profitable completion to pass, got reason %q", reason)
	}
}

func TestRealbotPanicBuyCompletionGuardIgnoresBalancedInventory(t *testing.T) {
	engine := paper.NewEngine(100)
	if _, err := engine.BuyForMarket("mkt-1", "Yes", 0.62, 5); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}
	if _, err := engine.BuyForMarket("mkt-1", "No", 0.30, 5); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	block, reason := realbotPanicBuyCompletionGuard(engine, "mkt-1", "Yes", "No", 0.45, 0.40, 2.0)
	if block {
		t.Fatalf("expected balanced inventory to bypass completion guard, got reason %q", reason)
	}
}

func TestRealbotHandlePanicBuyStrategySkipsCrossedNonLadderedQuotes(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)

	entryExecutionInFlight := false
	panicBuyCooldown := time.Time{}
	lastPairUpdate := time.Now()
	lastTrade := time.Time{}
	lastDustRecoveryNotice := time.Time{}
	ladderedEntries := []realbotLadderedEntry{{seq: 1, ask0: 0.45, ask1: 0.46}}
	nextLadderedEntrySeq := uint64(1)

	handled := realbotHandlePanicBuyStrategy(realbotPanicBuyStrategyArgs{
		ctx:            context.Background(),
		marketID:       "BTC",
		outcomes:       []string{"Down", "Up"},
		tokenToOutcome: map[string]string{"down-token": "Down", "up-token": "Up"},
		tokenBids:      map[string]float64{"Down": 0.52, "Up": 0.48},
		tokenAsks:      map[string]float64{"Down": 0.51, "Up": 0.49},
		tui:            tui,
		engine:         engine,
		arbMode:        paperArbModeTaker,
	}, &realbotPanicBuyStrategyState{
		lastPairUpdate:         &lastPairUpdate,
		ladderedEntries:        &ladderedEntries,
		nextLadderedEntrySeq:   &nextLadderedEntrySeq,
		panicBuyCooldown:       &panicBuyCooldown,
		lastTrade:              &lastTrade,
		lastDustRecoveryNotice: &lastDustRecoveryNotice,
		entryExecutionInFlight: &entryExecutionInFlight,
	})
	if !handled {
		t.Fatal("expected crossed non-laddered quotes to short-circuit the strategy")
	}
	if entryExecutionInFlight {
		t.Fatal("expected crossed quotes to avoid launching aggressive entry execution")
	}
	if nextLadderedEntrySeq != 1 || len(ladderedEntries) != 1 {
		t.Fatal("expected crossed quotes to leave ladder state unchanged")
	}
}

func TestRealbotHandlePanicBuyStrategySkipsCrossedLadderedQuotes(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)

	entryExecutionInFlight := false
	panicBuyCooldown := time.Time{}
	lastPairUpdate := time.Now()
	lastTrade := time.Time{}
	lastDustRecoveryNotice := time.Time{}
	ladderedEntries := []realbotLadderedEntry{{seq: 1, ask0: 0.45, ask1: 0.46}}
	nextLadderedEntrySeq := uint64(1)

	handled := realbotHandlePanicBuyStrategy(realbotPanicBuyStrategyArgs{
		ctx:            context.Background(),
		marketID:       "BTC",
		outcomes:       []string{"Down", "Up"},
		tokenToOutcome: map[string]string{"down-token": "Down", "up-token": "Up"},
		tokenBids:      map[string]float64{"Down": 0.40, "Up": 0.48},
		tokenAsks:      map[string]float64{"Down": 0.39, "Up": 0.70},
		tui:            tui,
		engine:         engine,
		arbMode:        paperArbModeLaddered,
	}, &realbotPanicBuyStrategyState{
		lastPairUpdate:         &lastPairUpdate,
		ladderedEntries:        &ladderedEntries,
		nextLadderedEntrySeq:   &nextLadderedEntrySeq,
		panicBuyCooldown:       &panicBuyCooldown,
		lastTrade:              &lastTrade,
		lastDustRecoveryNotice: &lastDustRecoveryNotice,
		entryExecutionInFlight: &entryExecutionInFlight,
	})
	if !handled {
		t.Fatal("expected crossed laddered quotes to short-circuit the strategy")
	}
	if entryExecutionInFlight {
		t.Fatal("expected crossed laddered quotes to avoid launching aggressive entry execution")
	}
	if nextLadderedEntrySeq != 1 || len(ladderedEntries) != 1 {
		t.Fatal("expected crossed laddered quotes to leave ladder state unchanged")
	}
}

func TestRealbotHandlePanicBuyStrategySkipsLadderedEntryWhileInFlight(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)

	entryExecutionInFlight := true
	panicBuyCooldown := time.Time{}
	lastPairUpdate := time.Now()
	lastTrade := time.Time{}
	lastDustRecoveryNotice := time.Time{}
	ladderedEntries := []realbotLadderedEntry{{seq: 1, ask0: 0.45, ask1: 0.46}}
	nextLadderedEntrySeq := uint64(1)

	handled := realbotHandlePanicBuyStrategy(realbotPanicBuyStrategyArgs{
		ctx:            context.Background(),
		marketID:       "BTC",
		outcomes:       []string{"Down", "Up"},
		tokenToOutcome: map[string]string{"down-token": "Down", "up-token": "Up"},
		tokenBids:      map[string]float64{"Down": 0.40, "Up": 0.48},
		tokenAsks:      map[string]float64{"Down": 0.39, "Up": 0.70},
		tui:            tui,
		engine:         engine,
		arbMode:        paperArbModeLaddered,
	}, &realbotPanicBuyStrategyState{
		lastPairUpdate:         &lastPairUpdate,
		ladderedEntries:        &ladderedEntries,
		nextLadderedEntrySeq:   &nextLadderedEntrySeq,
		panicBuyCooldown:       &panicBuyCooldown,
		lastTrade:              &lastTrade,
		lastDustRecoveryNotice: &lastDustRecoveryNotice,
		entryExecutionInFlight: &entryExecutionInFlight,
	})
	if !handled {
		t.Fatal("expected laddered in-flight execution to short-circuit the strategy")
	}
	if nextLadderedEntrySeq != 1 || len(ladderedEntries) != 1 {
		t.Fatal("expected ladder state to stay unchanged while execution is still in flight")
	}
}

func TestRealbotHandlePanicBuyStrategySkipsLadderedEntryDuringCooldown(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)

	entryExecutionInFlight := false
	panicBuyCooldown := time.Now().Add(2 * time.Second)
	lastPairUpdate := time.Now()
	lastTrade := time.Time{}
	lastDustRecoveryNotice := time.Time{}
	ladderedEntries := []realbotLadderedEntry{{seq: 1, ask0: 0.45, ask1: 0.46}}
	nextLadderedEntrySeq := uint64(1)

	handled := realbotHandlePanicBuyStrategy(realbotPanicBuyStrategyArgs{
		ctx:            context.Background(),
		marketID:       "BTC",
		outcomes:       []string{"Down", "Up"},
		tokenToOutcome: map[string]string{"down-token": "Down", "up-token": "Up"},
		tokenBids:      map[string]float64{"Down": 0.40, "Up": 0.48},
		tokenAsks:      map[string]float64{"Down": 0.39, "Up": 0.70},
		tui:            tui,
		engine:         engine,
		arbMode:        paperArbModeLaddered,
	}, &realbotPanicBuyStrategyState{
		lastPairUpdate:         &lastPairUpdate,
		ladderedEntries:        &ladderedEntries,
		nextLadderedEntrySeq:   &nextLadderedEntrySeq,
		panicBuyCooldown:       &panicBuyCooldown,
		lastTrade:              &lastTrade,
		lastDustRecoveryNotice: &lastDustRecoveryNotice,
		entryExecutionInFlight: &entryExecutionInFlight,
	})
	if !handled {
		t.Fatal("expected laddered cooldown to short-circuit the strategy")
	}
	if nextLadderedEntrySeq != 1 || len(ladderedEntries) != 1 {
		t.Fatal("expected ladder state to stay unchanged while cooldown is active")
	}
}

func TestNormalizePaperArbModeDefaultsToTaker(t *testing.T) {
	if got := normalizePaperArbMode(""); got != paperArbModeTaker {
		t.Fatalf("expected empty mode to normalize to taker, got %q", got)
	}
	if got := normalizePaperArbMode("maker"); got != paperArbModeMaker {
		t.Fatalf("expected maker to remain maker, got %q", got)
	}
	if got := normalizePaperArbMode("copytrade"); got != paperArbModeCopytrade {
		t.Fatalf("expected copytrade to remain copytrade, got %q", got)
	}
}

func TestRealbotCopytradeTargetSharesAggregatesByOutcome(t *testing.T) {
	shares := realbotCopytradeTargetShares([]api.Position{
		{Outcome: "Up", Size: 3.5},
		{Outcome: "Up", Size: 1.25},
		{Outcome: "Down", Size: 2.0},
		{Outcome: "Down", Size: 0.009},
	})
	if math.Abs(shares["Up"]-4.75) > 0.000001 {
		t.Fatalf("expected Up shares 4.75, got %.4f", shares["Up"])
	}
	if math.Abs(shares["Down"]-2.0) > 0.000001 {
		t.Fatalf("expected Down shares 2.0, got %.4f", shares["Down"])
	}
}

func TestShouldRealbotMakerBlockBuyBlocksHeavyLegWithoutProtectedSell(t *testing.T) {
	if !shouldRealbotMakerBlockBuy(12, false, 8, 0.44, 0.43, 0.02) {
		t.Fatal("expected heavy leg without protected sell to block maker buy")
	}
	if shouldRealbotMakerBlockBuy(12, true, 8, 0.44, 0.43, 0.02) {
		t.Fatal("expected protected sell path to allow maker buy evaluation")
	}
}

func TestShouldRealbotMakerBlockBuyBlocksExpensivePairCompletion(t *testing.T) {
	if !shouldRealbotMakerBlockBuy(3, true, 10, 0.62, 0.39, 0.02) {
		t.Fatal("expected expensive completion path to block maker buy")
	}
	if shouldRealbotMakerBlockBuy(3, true, 10, 0.50, 0.39, 0.02) {
		t.Fatal("expected affordable completion path to pass")
	}
}

func TestRealbotShouldReconnectWSOnlyForInvalidStaleBook(t *testing.T) {
	outcomes := []string{"Down", "Up"}
	validBids := map[string]float64{"Down": 0.46, "Up": 0.51}
	validAsks := map[string]float64{"Down": 0.48, "Up": 0.53}

	if realbotShouldReconnectWS(outcomes, validBids, validAsks, 25*time.Second, 15*time.Second, false) {
		t.Fatal("expected quiet but valid local quotes to remain on WS")
	}

	invalidAsks := map[string]float64{"Down": 0.48, "Up": 0}
	if !realbotShouldReconnectWS(outcomes, validBids, invalidAsks, 25*time.Second, 15*time.Second, false) {
		t.Fatal("expected reconnect when the stale local book loses one side")
	}

	if realbotShouldReconnectWS(outcomes, validBids, invalidAsks, 25*time.Second, 15*time.Second, true) {
		t.Fatal("expected terminal-looking book to suppress reconnects")
	}
}

func TestRealbotShouldPollRestFallbackOnStaleSaneBook(t *testing.T) {
	now := time.Now()
	if !realbotShouldPollRestFallback(
		now.Add(-5*time.Second),
		now.Add(-2*time.Second),
		now,
		3*time.Second,
		time.Second,
		false,
	) {
		t.Fatal("expected stale sane book to trigger REST fallback polling")
	}
}

func TestRealbotHandleMarketWSMessageUpdatesSnapshotState(t *testing.T) {
	engine := paper.NewEngine(100)
	tracker := paper.NewDirectionalSignalTracker(time.Second, []string{"Down", "Up"})
	lastPairUpdate := time.Time{}
	quoteState := map[string]realbotQuoteState{
		"Up": {UpdatedAt: time.Now(), Source: "ws"},
	}
	tokenBids := map[string]float64{"Up": 0.51}
	tokenAsks := map[string]float64{"Up": 0.52}
	tokenFullBids := make(map[string][]paper.MarketLevel)
	tokenFullAsks := make(map[string][]paper.MarketLevel)

	args := realbotMarketQuoteArgs{
		marketID:          "BTC",
		tokenToOutcome:    map[string]string{"down-token": "Down"},
		outcomes:          []string{"Down", "Up"},
		tokenBids:         tokenBids,
		tokenAsks:         tokenAsks,
		tokenFullBids:     tokenFullBids,
		tokenFullAsks:     tokenFullAsks,
		quoteState:        quoteState,
		polySignalTracker: tracker,
		engine:            engine,
	}

	msg := []byte(`[
		{
			"event_type":"book",
			"asset_id":"down-token",
			"timestamp":"1766789469958",
			"bids":[{"price":"0.48","size":"100"}],
			"asks":[{"price":"0.49","size":"150"}]
		}
	]`)

	realbotHandleMarketWSMessage(args, msg, &lastPairUpdate)

	if got := tokenBids["Down"]; math.Abs(got-0.48) > 0.000001 {
		t.Fatalf("expected Down bid 0.48, got %.4f", got)
	}
	if got := tokenAsks["Down"]; math.Abs(got-0.49) > 0.000001 {
		t.Fatalf("expected Down ask 0.49, got %.4f", got)
	}
	if len(tokenFullBids["Down"]) != 1 || len(tokenFullAsks["Down"]) != 1 {
		t.Fatalf("expected full depth to be populated, got bids=%v asks=%v", tokenFullBids["Down"], tokenFullAsks["Down"])
	}
	if quoteState["Down"].Source != "ws" {
		t.Fatalf("expected Down quote source ws, got %q", quoteState["Down"].Source)
	}
	if lastPairUpdate.IsZero() {
		t.Fatal("expected sane pair snapshot to update lastPairUpdate")
	}
}

func TestRealbotHandleMarketWSMessagePriceUpdateAppliesExplicitBestBidAsk(t *testing.T) {
	engine := paper.NewEngine(100)
	tracker := paper.NewDirectionalSignalTracker(time.Second, []string{"Down", "Up"})
	lastPairUpdate := time.Time{}
	quoteState := map[string]realbotQuoteState{
		"Up": {UpdatedAt: time.Now(), Source: "ws"},
	}
	tokenBids := map[string]float64{"Up": 0.26, "Down": 0.40}
	tokenAsks := map[string]float64{"Up": 0.27, "Down": 0.60}
	tokenFullBids := map[string][]paper.MarketLevel{
		"Down": {{Price: 0.40, Size: 10}},
		"Up":   {{Price: 0.26, Size: 10}},
	}
	tokenFullAsks := map[string][]paper.MarketLevel{
		"Down": {{Price: 0.60, Size: 10}},
		"Up":   {{Price: 0.27, Size: 10}},
	}

	args := realbotMarketQuoteArgs{
		marketID:          "BTC",
		tokenToOutcome:    map[string]string{"down-token": "Down"},
		outcomes:          []string{"Down", "Up"},
		tokenBids:         tokenBids,
		tokenAsks:         tokenAsks,
		tokenFullBids:     tokenFullBids,
		tokenFullAsks:     tokenFullAsks,
		quoteState:        quoteState,
		polySignalTracker: tracker,
		engine:            engine,
	}

	msg := []byte(`{
		"market":"btc-market",
		"price_changes":[
			{
				"asset_id":"down-token",
				"price":"0.74",
				"size":"12",
				"side":"SELL",
				"best_bid":"0.73",
				"best_ask":"0.74",
				"timestamp":"1766789469958"
			}
		]
	}`)

	realbotHandleMarketWSMessage(args, msg, &lastPairUpdate)

	if got := tokenBids["Down"]; math.Abs(got-0.73) > 0.000001 {
		t.Fatalf("expected explicit Down best bid 0.73, got %.4f", got)
	}
	if got := tokenAsks["Down"]; math.Abs(got-0.74) > 0.000001 {
		t.Fatalf("expected explicit Down best ask 0.74, got %.4f", got)
	}
	if quoteState["Down"].Source != "ws" {
		t.Fatalf("expected Down quote source ws after price update, got %q", quoteState["Down"].Source)
	}
	if lastPairUpdate.IsZero() {
		t.Fatal("expected sane explicit BBO update to refresh lastPairUpdate")
	}
}

func TestRealbotProcessMarketQuotesPublishesDisplayAndFreshness(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	tui.AddMarket("BTC", "btc", []string{"Down", "Up"}, time.Now().Add(time.Minute))

	now := time.Now()
	displayBids := make(map[string]float64)
	displayAsks := make(map[string]float64)
	publishedBids := make(map[string]float64)
	publishedAsks := make(map[string]float64)
	tokenBids := map[string]float64{"Down": 0.48, "Up": 0.51}
	tokenAsks := map[string]float64{"Down": 0.49, "Up": 0.52}
	tokenFullBids := map[string][]paper.MarketLevel{
		"Down": {{Price: 0.48, Size: 10}},
		"Up":   {{Price: 0.51, Size: 10}},
	}
	tokenFullAsks := map[string][]paper.MarketLevel{
		"Down": {{Price: 0.49, Size: 10}},
		"Up":   {{Price: 0.52, Size: 10}},
	}
	quoteState := map[string]realbotQuoteState{
		"Down": {UpdatedAt: now, Source: "ws"},
		"Up":   {UpdatedAt: now, Source: "ws"},
	}

	lastPairUpdate := now
	lastPublishedQuoteAt := time.Time{}
	lastReconnectCount := int32(0)
	lastWsWarnTime := now
	lastForceReconnect := now
	lastRestFallbackPoll := now
	restFallbackActive := false
	restRecoveryLogged := false
	wsChannelClosed := false

	if realbotProcessMarketQuotes(realbotMarketQuoteArgs{
		ctx:                    context.Background(),
		marketID:               "BTC",
		wsMgr:                  &api.WSManager{},
		wsMsgChan:              make(chan []byte, 1),
		tokenMap:               map[string]string{"down-token": "Down", "up-token": "Up"},
		tokenToOutcome:         map[string]string{"down-token": "Down", "up-token": "Up"},
		outcomes:               []string{"Down", "Up"},
		tokenBids:              tokenBids,
		tokenAsks:              tokenAsks,
		tokenFullBids:          tokenFullBids,
		tokenFullAsks:          tokenFullAsks,
		displayBids:            displayBids,
		displayAsks:            displayAsks,
		publishedBids:          publishedBids,
		publishedAsks:          publishedAsks,
		quoteState:             quoteState,
		polySignalTracker:      paper.NewDirectionalSignalTracker(time.Second, []string{"Down", "Up"}),
		engine:                 engine,
		restClient:             api.NewRestClient("polymarket"),
		tui:                    tui,
		restFallbackQuoteAge:   time.Minute,
		restFallbackPollPeriod: time.Minute,
	}, realbotMarketQuoteRuntime{
		lastPairUpdate:       &lastPairUpdate,
		lastPublishedQuoteAt: &lastPublishedQuoteAt,
		lastReconnectCount:   &lastReconnectCount,
		lastWsWarnTime:       &lastWsWarnTime,
		lastForceReconnect:   &lastForceReconnect,
		lastRestFallbackPoll: &lastRestFallbackPoll,
		restFallbackActive:   &restFallbackActive,
		restRecoveryLogged:   &restRecoveryLogged,
		wsChannelClosed:      &wsChannelClosed,
	}) {
		t.Fatal("expected active quote loop to continue running")
	}

	if math.Abs(displayBids["Down"]-0.48) > 0.000001 || math.Abs(displayAsks["Up"]-0.52) > 0.000001 {
		t.Fatalf("expected display quotes to publish sane pair, got bids=%v asks=%v", displayBids, displayAsks)
	}
	if math.Abs(publishedBids["Up"]-0.51) > 0.000001 || math.Abs(publishedAsks["Down"]-0.49) > 0.000001 {
		t.Fatalf("expected published quotes to track display, got bids=%v asks=%v", publishedBids, publishedAsks)
	}
	if lastPublishedQuoteAt.IsZero() {
		t.Fatal("expected fresh quote publication to advance lastPublishedQuoteAt")
	}
}

func TestRealbotProcessMarketQuotesClosedChannelSchedulesReconnect(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	tui.AddMarket("BTC", "btc", []string{"Down", "Up"}, time.Now().Add(time.Minute))

	ch := make(chan []byte)
	close(ch)

	lastPairUpdate := time.Now()
	lastPublishedQuoteAt := time.Time{}
	lastReconnectCount := int32(0)
	lastWsWarnTime := time.Now().Add(-2 * realbotWSWarnInterval)
	lastForceReconnect := time.Time{}
	lastRestFallbackPoll := time.Now()
	restFallbackActive := false
	restRecoveryLogged := false
	wsChannelClosed := false

	if realbotProcessMarketQuotes(realbotMarketQuoteArgs{
		ctx:                    context.Background(),
		marketID:               "BTC",
		wsMgr:                  &api.WSManager{},
		wsMsgChan:              ch,
		tokenMap:               map[string]string{"down-token": "Down", "up-token": "Up"},
		tokenToOutcome:         map[string]string{"down-token": "Down", "up-token": "Up"},
		outcomes:               []string{"Down", "Up"},
		tokenBids:              map[string]float64{"Down": 0.48, "Up": 0.51},
		tokenAsks:              map[string]float64{"Down": 0.49, "Up": 0.52},
		tokenFullBids:          map[string][]paper.MarketLevel{},
		tokenFullAsks:          map[string][]paper.MarketLevel{},
		displayBids:            map[string]float64{},
		displayAsks:            map[string]float64{},
		publishedBids:          map[string]float64{},
		publishedAsks:          map[string]float64{},
		quoteState:             map[string]realbotQuoteState{},
		polySignalTracker:      paper.NewDirectionalSignalTracker(time.Second, []string{"Down", "Up"}),
		engine:                 engine,
		restClient:             api.NewRestClient("polymarket"),
		tui:                    tui,
		restFallbackQuoteAge:   time.Minute,
		restFallbackPollPeriod: time.Minute,
	}, realbotMarketQuoteRuntime{
		lastPairUpdate:       &lastPairUpdate,
		lastPublishedQuoteAt: &lastPublishedQuoteAt,
		lastReconnectCount:   &lastReconnectCount,
		lastWsWarnTime:       &lastWsWarnTime,
		lastForceReconnect:   &lastForceReconnect,
		lastRestFallbackPoll: &lastRestFallbackPoll,
		restFallbackActive:   &restFallbackActive,
		restRecoveryLogged:   &restRecoveryLogged,
		wsChannelClosed:      &wsChannelClosed,
	}) {
		t.Fatal("expected closed-but-active channel to stay in retry loop rather than exit")
	}

	if !wsChannelClosed {
		t.Fatal("expected closed channel to mark wsChannelClosed")
	}
	if lastForceReconnect.IsZero() {
		t.Fatal("expected reconnect path to update lastForceReconnect")
	}
	if time.Since(lastWsWarnTime) > time.Second {
		t.Fatal("expected reconnect warning timestamp to refresh")
	}
}

func TestRealbotProcessMarketQuotesReturnsOnCancelledClosedChannel(t *testing.T) {
	ch := make(chan []byte)
	close(ch)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	lastPairUpdate := time.Now()
	lastPublishedQuoteAt := time.Time{}
	lastReconnectCount := int32(0)
	lastWsWarnTime := time.Now()
	lastForceReconnect := time.Now()
	lastRestFallbackPoll := time.Now()
	restFallbackActive := false
	restRecoveryLogged := false
	wsChannelClosed := false

	if !realbotProcessMarketQuotes(realbotMarketQuoteArgs{
		ctx:                    ctx,
		marketID:               "BTC",
		wsMgr:                  &api.WSManager{},
		wsMsgChan:              ch,
		tokenMap:               map[string]string{},
		tokenToOutcome:         map[string]string{},
		outcomes:               []string{"Down", "Up"},
		tokenBids:              map[string]float64{},
		tokenAsks:              map[string]float64{},
		tokenFullBids:          map[string][]paper.MarketLevel{},
		tokenFullAsks:          map[string][]paper.MarketLevel{},
		displayBids:            map[string]float64{},
		displayAsks:            map[string]float64{},
		publishedBids:          map[string]float64{},
		publishedAsks:          map[string]float64{},
		quoteState:             map[string]realbotQuoteState{},
		polySignalTracker:      paper.NewDirectionalSignalTracker(time.Second, []string{"Down", "Up"}),
		engine:                 paper.NewEngine(100),
		restClient:             api.NewRestClient("polymarket"),
		tui:                    paper.NewTUI(paper.NewEngine(100), nil),
		restFallbackQuoteAge:   time.Minute,
		restFallbackPollPeriod: time.Minute,
	}, realbotMarketQuoteRuntime{
		lastPairUpdate:       &lastPairUpdate,
		lastPublishedQuoteAt: &lastPublishedQuoteAt,
		lastReconnectCount:   &lastReconnectCount,
		lastWsWarnTime:       &lastWsWarnTime,
		lastForceReconnect:   &lastForceReconnect,
		lastRestFallbackPoll: &lastRestFallbackPoll,
		restFallbackActive:   &restFallbackActive,
		restRecoveryLogged:   &restRecoveryLogged,
		wsChannelClosed:      &wsChannelClosed,
	}) {
		t.Fatal("expected cancelled context with closed channel to exit quote loop")
	}
}

func TestComputeRealbotMakerProtectedSellQuoteIgnoresCostFloor(t *testing.T) {
	price, ok := computeRealbotMakerProtectedSellQuote(0.54, 0.60, 0.56, 0.02, 0, 0.008, 1000, time.Hour, realbotMakerStrategyParams)
	if !ok {
		t.Fatal("expected protected sell quote to exist")
	}
	if price != 0.580 {
		t.Fatalf("expected sell quote to be based on mid and gap, got %.3f", price)
	}
	// Even in a narrow market, it should still place a quote (no longer rejects purely on cost)
	if _, ok := computeRealbotMakerProtectedSellQuote(0.54, 0.56, 0.56, 0.02, 0, 0.008, 1000, time.Hour, realbotMakerStrategyParams); !ok {
		t.Fatal("expected narrow market to still place a sell quote")
	}
}

func TestComputeRealbotMakerSkewedQuoteRespectsConfiguredGap(t *testing.T) {
	tight, ok := computeRealbotMakerSkewedQuote(api.SideBuy, 0.47, 0.53, 0.0, 0.003, realbotMakerStrategyParams)
	if !ok {
		t.Fatal("expected tight maker buy quote")
	}
	wide, ok := computeRealbotMakerSkewedQuote(api.SideBuy, 0.47, 0.53, 0.0, 0.012, realbotMakerStrategyParams)
	if !ok {
		t.Fatal("expected wide maker buy quote")
	}
	if tight <= wide {
		t.Fatalf("expected tighter gap to quote closer to ask: tight=%.3f wide=%.3f", tight, wide)
	}
}

func TestExecutionDeltaFromPositionsBuy(t *testing.T) {
	positions := []trading.PositionInfo{{TokenID: "yes-token", Size: 7.5}}

	if got := executionDeltaFromPositions(positions, "yes-token", 2.0, "BUY"); got != 5.5 {
		t.Fatalf("expected buy delta 5.5, got %.2f", got)
	}
}

func TestExecutionDeltaFromPositionsSellClampsAtZero(t *testing.T) {
	positions := []trading.PositionInfo{{TokenID: "no-token", Size: 8.0}}

	if got := executionDeltaFromPositions(positions, "no-token", 5.0, "SELL"); got != 0 {
		t.Fatalf("expected sell delta clamp to zero, got %.2f", got)
	}
}

func TestRealbotBestAskFromLevels(t *testing.T) {
	asks := []paper.MarketLevel{{Price: 0.42}, {Price: 0.36}, {Price: 0.39}}
	bestAsk, ok := realbotBestAskFromLevels(asks)
	if !ok || math.Abs(bestAsk-0.36) > 0.000001 {
		t.Fatalf("best ask got %.3f ok=%v want 0.36,true", bestAsk, ok)
	}
	if _, ok := realbotBestAskFromLevels(nil); ok {
		t.Fatal("expected empty asks to return false")
	}
}

func TestRealbotBestBidFromLevels(t *testing.T) {
	bids := []paper.MarketLevel{{Price: 0.42}, {Price: 0.36}, {Price: 0.39}}
	bestBid, ok := realbotBestBidFromLevels(bids)
	if !ok || math.Abs(bestBid-0.42) > 0.000001 {
		t.Fatalf("best bid got %.3f ok=%v want 0.42,true", bestBid, ok)
	}
	if _, ok := realbotBestBidFromLevels(nil); ok {
		t.Fatal("expected empty bids to return false")
	}
}

func TestRealbotCopytradeTargetSharesForConditionFiltersOtherMarkets(t *testing.T) {
	shares := realbotCopytradeTargetSharesForCondition([]api.Position{
		{ConditionID: "cond-1", Outcome: "Up", Size: 2.25},
		{ConditionID: "cond-2", Outcome: "Up", Size: 9.0},
		{ConditionID: "cond-1", Outcome: "Down", Size: 4.0},
	}, "cond-1")
	if shares["Up"] != 2.25 {
		t.Fatalf("expected cond-1 Up shares 2.25, got %.4f", shares["Up"])
	}
	if shares["Down"] != 4.0 {
		t.Fatalf("expected cond-1 Down shares 4.0, got %.4f", shares["Down"])
	}
}

func TestRealbotCopytradeSharesByConditionAggregatesPerMarket(t *testing.T) {
	sharesByCondition := realbotCopytradeSharesByCondition([]api.Position{
		{ConditionID: "cond-1", Outcome: "Up", Size: 2.25},
		{ConditionID: "cond-1", Outcome: "Up", Size: 0.75},
		{ConditionID: "cond-1", Outcome: "Down", Size: 4.0},
		{ConditionID: "cond-2", Outcome: "Up", Size: 1.5},
		{ConditionID: "cond-2", Outcome: "Down", Size: 0.009},
	})
	if sharesByCondition["cond-1"]["Up"] != 3.0 {
		t.Fatalf("expected cond-1 Up shares 3.0, got %.4f", sharesByCondition["cond-1"]["Up"])
	}
	if sharesByCondition["cond-1"]["Down"] != 4.0 {
		t.Fatalf("expected cond-1 Down shares 4.0, got %.4f", sharesByCondition["cond-1"]["Down"])
	}
	if sharesByCondition["cond-2"]["Up"] != 1.5 {
		t.Fatalf("expected cond-2 Up shares 1.5, got %.4f", sharesByCondition["cond-2"]["Up"])
	}
	if sharesByCondition["cond-2"]["Down"] != 0 {
		t.Fatalf("expected cond-2 Down shares to ignore dust, got %.4f", sharesByCondition["cond-2"]["Down"])
	}
}

func TestRealbotCopytradeTargetDeltaSkipsInitialSnapshotThenTracksNetChange(t *testing.T) {
	state := newRealbotCopytradeState()
	t0 := time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)

	if delta, ready, pending := realbotCopytradeTargetDelta(state, "Up", 25, t0); ready || pending || delta != 0 {
		t.Fatalf("initial snapshot should seed only, got delta=%.4f ready=%v pending=%v", delta, ready, pending)
	}
	if delta, ready, pending := realbotCopytradeTargetDelta(state, "Up", 28.5, t0.Add(2*time.Second)); !ready || pending || delta != 3.5 {
		t.Fatalf("expected +3.5 delta after increase, got delta=%.4f ready=%v pending=%v", delta, ready, pending)
	}
	if delta, ready, pending := realbotCopytradeTargetDelta(state, "Up", 26.0, t0.Add(4*time.Second)); ready || !pending || delta != 0 {
		t.Fatalf("expected first lower snapshot to wait for confirmation, got delta=%.4f ready=%v pending=%v", delta, ready, pending)
	}
	if delta, ready, pending := realbotCopytradeTargetDelta(state, "Up", 26.0, t0.Add(6*time.Second)); !ready || pending || delta != -2.5 {
		t.Fatalf("expected -2.5 delta after second lower snapshot, got delta=%.4f ready=%v pending=%v", delta, ready, pending)
	}
}

func TestRealbotCopytradeTargetDeltaSeedsVisiblePositionAfterTradesSeeded(t *testing.T) {
	state := newRealbotCopytradeState()
	state.tradesSeeded = true
	t0 := time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)

	if delta, ready, pending := realbotCopytradeTargetDelta(state, "Up", 7.25, t0); !ready || pending || math.Abs(delta-7.25) > 0.000001 {
		t.Fatalf("expected seeded trades to surface +7.25 delta, got delta=%.4f ready=%v pending=%v", delta, ready, pending)
	}
}

func TestRealbotCopytradePositionSyncTradesCreatesFallbackSellWhenTargetFlats(t *testing.T) {
	state := newRealbotCopytradeState()
	t0 := time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)

	if trades, deltas := realbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		[]api.Position{{ConditionID: "cond-1", Outcome: "Up", Size: 5.51}},
		t0,
		nil,
		core.CopytradeSizingModePercent,
	); len(trades) != 0 || len(deltas) != 0 {
		t.Fatalf("initial seed should not emit sync trades, got trades=%d deltas=%d", len(trades), len(deltas))
	}

	trades, deltas := realbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		nil,
		t0.Add(2*time.Second),
		nil,
		core.CopytradeSizingModePercent,
	)
	if len(trades) != 0 {
		t.Fatalf("first lower snapshot should wait for confirmation, got %+v", trades)
	}
	if len(deltas) != 0 {
		t.Fatalf("first lower snapshot should not surface a delta yet, got %+v", deltas)
	}

	trades, deltas = realbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		nil,
		t0.Add(4*time.Second),
		nil,
		core.CopytradeSizingModePercent,
	)
	if len(trades) != 1 {
		t.Fatalf("expected one fallback sell trade, got %d", len(trades))
	}
	if trades[0].Side != "SELL" || trades[0].Outcome != "Up" || trades[0].Source != "position" {
		t.Fatalf("unexpected fallback sell trade: %+v", trades[0])
	}
	if math.Abs(trades[0].Size-5.51) > 0.000001 {
		t.Fatalf("expected fallback sell size 5.51, got %.4f", trades[0].Size)
	}
	if got := deltas["Up"]; math.Abs(got+5.51) > 0.000001 {
		t.Fatalf("expected target delta -5.51, got %.4f", got)
	}
}

func TestRealbotCopytradePositionSyncTradesSkipsFallbackSellWhenFreshSellExists(t *testing.T) {
	state := newRealbotCopytradeState()
	t0 := time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)

	realbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		[]api.Position{{ConditionID: "cond-1", Outcome: "Up", Size: 5.51}},
		t0,
		nil,
		core.CopytradeSizingModePercent,
	)

	trades, deltas := realbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		nil,
		t0.Add(2*time.Second),
		[]api.PublicTrade{{ConditionID: "cond-1", Outcome: "Up", Side: "SELL", Size: 5.51, Timestamp: t0.Add(2 * time.Second).Unix()}},
		core.CopytradeSizingModePercent,
	)
	if len(trades) != 0 {
		t.Fatalf("expected fresh sell to suppress fallback sell, got %+v", trades)
	}
	if len(deltas) != 0 {
		t.Fatalf("first lower snapshot should still be pending, got %+v", deltas)
	}
}

func TestRealbotCopytradePositionSyncTradesCreatesEstimatedBuyWithoutFreshTrade(t *testing.T) {
	state := newRealbotCopytradeState()
	t0 := time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)

	realbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		nil,
		t0,
		nil,
		core.CopytradeSizingModePercent,
	)

	trades, deltas := realbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		[]api.Position{{ConditionID: "cond-1", Outcome: "Up", Size: 7.25}},
		t0.Add(2*time.Second),
		nil,
		core.CopytradeSizingModePercent,
	)
	if len(trades) != 1 {
		t.Fatalf("expected one estimated buy trade, got %d", len(trades))
	}
	if trades[0].Side != "BUY" || trades[0].Outcome != "Up" {
		t.Fatalf("unexpected estimated buy trade: %+v", trades[0])
	}
	if trades[0].Source != "position" {
		t.Fatalf("expected position source, got %q", trades[0].Source)
	}
	if math.Abs(trades[0].Size-7.25) > 0.000001 {
		t.Fatalf("expected estimated buy size 7.25, got %.4f", trades[0].Size)
	}
	if got := deltas["Up"]; math.Abs(got-7.25) > 0.000001 {
		t.Fatalf("expected target delta 7.25, got %.4f", got)
	}
}

func TestRealbotCopytradePositionSyncTradesCreatesResidualBuyWhenFreshBuyIsPartial(t *testing.T) {
	state := newRealbotCopytradeState()
	t0 := time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)

	realbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		nil,
		t0,
		nil,
		core.CopytradeSizingModePercent,
	)

	trades, deltas := realbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		[]api.Position{{ConditionID: "cond-1", Outcome: "Up", Size: 10}},
		t0.Add(2*time.Second),
		[]api.PublicTrade{{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 4, Timestamp: t0.Add(2 * time.Second).Unix()}},
		core.CopytradeSizingModePercent,
	)
	if len(trades) != 1 {
		t.Fatalf("expected one residual estimated buy, got %d", len(trades))
	}
	if trades[0].Side != "BUY" || trades[0].Source != "position" {
		t.Fatalf("unexpected residual buy trade: %+v", trades[0])
	}
	if math.Abs(trades[0].Size-6) > 0.000001 {
		t.Fatalf("expected residual buy size 6, got %.4f", trades[0].Size)
	}
	if got := deltas["Up"]; math.Abs(got-10) > 0.000001 {
		t.Fatalf("expected full target delta 10, got %.4f", got)
	}
}

func TestRealbotCopytradePositionSyncTradesCreatesBuyCatchupWhileHoldingBothOutcomes(t *testing.T) {
	state := newRealbotCopytradeState()
	t0 := time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)

	realbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		[]api.Position{
			{ConditionID: "cond-1", Outcome: "Up", Size: 100},
			{ConditionID: "cond-1", Outcome: "Down", Size: 100},
		},
		t0,
		nil,
		core.CopytradeSizingModePercent,
	)

	trades, deltas := realbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		[]api.Position{
			{ConditionID: "cond-1", Outcome: "Up", Size: 129},
			{ConditionID: "cond-1", Outcome: "Down", Size: 100},
		},
		t0.Add(2*time.Second),
		nil,
		core.CopytradeSizingModePercent,
	)
	if len(trades) != 1 {
		t.Fatalf("expected one buy catch-up trade while holding both outcomes, got %d", len(trades))
	}
	if trades[0].Outcome != "Up" || trades[0].Side != "BUY" || trades[0].Source != "position" {
		t.Fatalf("unexpected buy catch-up trade: %+v", trades[0])
	}
	if math.Abs(trades[0].Size-29) > 0.000001 {
		t.Fatalf("expected 29-share catch-up buy, got %.4f", trades[0].Size)
	}
	if got := deltas["Up"]; math.Abs(got-29) > 0.000001 {
		t.Fatalf("expected up delta 29, got %.4f", got)
	}
}

func TestRealbotCopytradePositionSyncTradesIgnoresFixedSizeModes(t *testing.T) {
	state := newRealbotCopytradeState()
	t0 := time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)

	trades, deltas := realbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		[]api.Position{{ConditionID: "cond-1", Outcome: "Up", Size: 7.25}},
		t0,
		nil,
		core.CopytradeSizingModeShares,
	)
	if len(trades) != 0 || len(deltas) != 0 {
		t.Fatalf("shares mode should ignore position sync, got trades=%d deltas=%d", len(trades), len(deltas))
	}

	trades, deltas = realbotCopytradePositionSyncTrades(
		state,
		"cond-1",
		[]string{"Up", "Down"},
		[]api.Position{{ConditionID: "cond-1", Outcome: "Up", Size: 7.25}},
		t0,
		nil,
		core.CopytradeSizingModeUSDC,
	)
	if len(trades) != 0 || len(deltas) != 0 {
		t.Fatalf("usdc mode should ignore position sync, got trades=%d deltas=%d", len(trades), len(deltas))
	}
}

func TestRealbotCopytradeFreshTradesIgnoresPreStartHistoryThenDedupes(t *testing.T) {
	state := newRealbotCopytradeState()
	state.startedAt = time.Unix(1500, 0)
	initial := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 5.51, Timestamp: 1000, TransactionHash: "0x1"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "SELL", Size: 5.51, Timestamp: 2000, TransactionHash: "0x2"},
	}
	if got := realbotCopytradeFreshTrades(state, initial, "cond-1", "shares"); len(got) != 1 {
		t.Fatalf("expected initial snapshot to ignore pre-start history and keep one post-start signal, got %d", len(got))
	}
	if got := realbotCopytradeFreshTrades(state, initial, "cond-1", "shares"); len(got) != 0 {
		t.Fatalf("expected repeated trade snapshot to stay deduped, got %d", len(got))
	}

	next := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 5.51, Timestamp: 1000, TransactionHash: "0x1"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "SELL", Size: 5.51, Timestamp: 2000, TransactionHash: "0x2"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 1.25, Timestamp: 3000, TransactionHash: "0x3"},
	}
	got := realbotCopytradeFreshTrades(state, next, "cond-1", "shares")
	if len(got) != 1 {
		t.Fatalf("expected exactly one fresh trade, got %d", len(got))
	}
	if got[0].Side != "BUY" || got[0].Timestamp != 3000 {
		t.Fatalf("unexpected fresh trade: %+v", got[0])
	}
}

func TestRealbotCopytradeBootstrapStartTimestamp(t *testing.T) {
	if got := realbotCopytradeBootstrapStartTimestamp(time.Unix(1500, 0)); got != 1500 {
		t.Fatalf("exact-second bootstrap timestamp = %d, want 1500", got)
	}
	if got := realbotCopytradeBootstrapStartTimestamp(time.Unix(1500, 250_000_000)); got != 1499 {
		t.Fatalf("sub-second bootstrap timestamp = %d, want 1499", got)
	}
}

func TestRealbotCopytradeFreshTradesSortsUnorderedHistoryChronologically(t *testing.T) {
	state := newRealbotCopytradeState()
	state.startedAt = time.Unix(1000, 500_000_000)
	trades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 1, Timestamp: 1001, TransactionHash: "0xb"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 1, Timestamp: 1000, TransactionHash: "0xc"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 1, Timestamp: 1001, TransactionHash: "0xa"},
	}

	got := realbotCopytradeFreshTrades(state, trades, "cond-1", "shares")
	if len(got) != 3 {
		t.Fatalf("expected three bootstrap trades, got %d", len(got))
	}
	if got[0].Timestamp != 1000 || got[0].TransactionHash != "0xc" {
		t.Fatalf("first trade = %+v, want timestamp 1000 tx 0xc", got[0])
	}
	if got[1].Timestamp != 1001 || got[1].TransactionHash != "0xa" {
		t.Fatalf("second trade = %+v, want timestamp 1001 tx 0xa", got[1])
	}
	if got[2].Timestamp != 1001 || got[2].TransactionHash != "0xb" {
		t.Fatalf("third trade = %+v, want timestamp 1001 tx 0xb", got[2])
	}
}

func TestRealbotCopytradeFreshTradesBootstrapUsesObservedAtForOnchainSignals(t *testing.T) {
	state := newRealbotCopytradeState()
	state.startedAt = time.Unix(1000, 0)
	trades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Timestamp: 999, ObservedAt: 1001, TransactionHash: "0xtx", Source: "onchain"},
	}

	got := realbotCopytradeFreshTrades(state, trades, "cond-1", "shares")
	if len(got) != 1 {
		t.Fatalf("expected onchain signal observed after start to survive bootstrap, got %d", len(got))
	}
}

func TestRealbotCopytradeFreshTradesBootstrapKeepsRecentWatcherSignalsBeforeStart(t *testing.T) {
	state := newRealbotCopytradeState()
	state.startedAt = time.Unix(1000, 500_000_000)
	trades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Down", Side: "BUY", Size: 2, Timestamp: 981, TransactionHash: "0xon", Source: "onchain"},
		{ConditionID: "cond-1", Outcome: "Down", Side: "BUY", Size: 2, Timestamp: 981, TransactionHash: "0xmem", Source: "mempool", SignalID: "0xmem:1"},
	}

	got := realbotCopytradeFreshTrades(state, trades, "cond-1", "shares")
	if len(got) != 2 {
		t.Fatalf("expected recent watcher signals just before start to survive bootstrap, got %d", len(got))
	}
}

func TestSyncWalletTruthOutcomePositionTrimsExcessLocalInventory(t *testing.T) {
	engine := paper.NewEngine(100.0)
	tui := paper.NewTUI(engine, paper.NewOrderBook())
	engine.UpdateMarketBidAsk("m1", "Down", 0.09, 0.10)
	if !engine.SyncExternalPosition("m1", "Down", 17.0297, 0.42) {
		t.Fatal("expected seed position to sync into engine")
	}

	desired, changed := syncWalletTruthOutcomePosition(engine, tui, "m1", "Down", 17.0297, 16.60494, 0)
	if !changed {
		t.Fatal("expected wallet-truth trim to change local inventory")
	}
	if math.Abs(desired-16.60494) > 0.000001 {
		t.Fatalf("unexpected desired shares %.5f", desired)
	}

	positions := engine.GetPositions()
	pos, ok := positions["m1:Down"]
	if !ok {
		t.Fatal("expected trimmed position to remain in engine")
	}
	if math.Abs(pos.Quantity-16.60494) > 0.000001 {
		t.Fatalf("expected engine quantity 16.60494, got %.5f", pos.Quantity)
	}

	history := tui.GetOrderHistory()
	if len(history) != 0 {
		t.Fatalf("expected zero wallet-sync history entry because it was silenced, got %d", len(history))
	}
}

func TestSyncWalletTruthOutcomePositionDropsDustInventory(t *testing.T) {
	engine := paper.NewEngine(100.0)
	tui := paper.NewTUI(engine, paper.NewOrderBook())
	engine.UpdateMarketBidAsk("m1", "Up", 0.99, 1.00)
	if !engine.SyncExternalPosition("m1", "Up", 0.00359, 0.50) {
		t.Fatal("expected seed dust position to sync into engine")
	}

	desired, changed := syncWalletTruthOutcomePosition(engine, tui, "m1", "Up", 0.00359, 0.00359, 0)
	if !changed {
		t.Fatal("expected dust-only wallet-truth sync to clear local inventory")
	}
	if desired != 0 {
		t.Fatalf("expected dust desired shares to normalize to zero, got %.5f", desired)
	}
	if _, ok := engine.GetPositions()["m1:Up"]; ok {
		t.Fatal("expected dust-only engine position to be cleared")
	}
}

func TestRealbotCopytradeFreshTradesBootstrapDropsRecentPublicSignalsBeforeStart(t *testing.T) {
	state := newRealbotCopytradeState()
	state.startedAt = time.Unix(1000, 500_000_000)
	trades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Down", Side: "BUY", Size: 2, Timestamp: 981, TransactionHash: "0xpub", Source: "public"},
	}

	got := realbotCopytradeFreshTrades(state, trades, "cond-1", "shares")
	if len(got) != 0 {
		t.Fatalf("expected pre-start public signal to be dropped during bootstrap, got %d", len(got))
	}
}

func TestRealbotCopytradeFreshTradesKeepsDistinctMempoolSignalsSameTx(t *testing.T) {
	state := newRealbotCopytradeState()
	state.startedAt = time.Unix(1000, 0)
	trades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Timestamp: 1001, TransactionHash: "0xtx", Source: "mempool", SignalID: "0xtx:1"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Timestamp: 1001, TransactionHash: "0xtx", Source: "mempool", SignalID: "0xtx:2"},
	}

	got := realbotCopytradeFreshTrades(state, trades, "cond-1", "shares")
	if len(got) != 2 {
		t.Fatalf("expected two distinct mempool signals, got %d", len(got))
	}
}

func TestRealbotMergeCopytradeTradesDedupesWatcherAndPublicSameTx(t *testing.T) {
	watcherTrades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx", Source: "mempool", SignalID: "0xtx:asset-a:BUY"},
	}
	publicTrades := realbotPrepareCopytradeTrades([]api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Asset: "asset-a", Timestamp: 1002, TransactionHash: "0xtx"},
	}, "public", paper.TUISettings{})

	got := realbotMergeCopytradeTrades(watcherTrades, publicTrades)
	if len(got) != 1 {
		t.Fatalf("expected watcher/public duplicate tx to merge into one trade, got %d", len(got))
	}
	if got[0].Source != "mempool" {
		t.Fatalf("expected watcher trade to win merge precedence, got source %q", got[0].Source)
	}
}

func TestRealbotCopytradeFreshTradesDetectsAdditionalPublicFillSameSignalAcrossPolls(t *testing.T) {
	state := newRealbotCopytradeState()
	state.startedAt = time.Unix(1000, 0)

	poll1 := realbotMergeCopytradeTrades(realbotPrepareCopytradeTrades([]api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
	}, "public", paper.TUISettings{}))
	if got := realbotCopytradeFreshTrades(state, poll1, "cond-1", "shares"); len(got) != 1 {
		t.Fatalf("Poll 1: expected 1 fresh trade, got %d", len(got))
	}

	poll2 := realbotMergeCopytradeTrades(realbotPrepareCopytradeTrades([]api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 3, Price: 0.46, Asset: "asset-a", Timestamp: 1002, TransactionHash: "0xtx"},
	}, "public", paper.TUISettings{}))
	got := realbotCopytradeFreshTrades(state, poll2, "cond-1", "shares")
	if len(got) != 1 {
		t.Fatalf("Poll 2: expected 1 newly backfilled trade, got %d", len(got))
	}
	if got[0].Size != 3 || got[0].Timestamp != 1002 {
		t.Fatalf("unexpected backfilled trade: %+v", got[0])
	}
}

func TestRealbotCopytradeFreshTradesKeepsDistinctPublicTradesSameTx(t *testing.T) {
	state := newRealbotCopytradeState()
	state.startedAt = time.Unix(1000, 0)
	trades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.45, Asset: "asset-b", Timestamp: 1001, TransactionHash: "0xtx"},
	}

	got := realbotCopytradeFreshTrades(state, trades, "cond-1", "shares")
	if len(got) != 2 {
		t.Fatalf("expected two distinct public trades from same tx, got %d", len(got))
	}
}

func TestRealbotCopytradeFreshTradesKeepsIdenticalPublicTradesSameTx(t *testing.T) {
	state := newRealbotCopytradeState()
	state.startedAt = time.Unix(1000, 0)
	trades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
	}

	got := realbotCopytradeFreshTrades(state, trades, "cond-1", "shares")
	if len(got) != 2 {
		t.Fatalf("expected two identical fills from same tx to stay distinct, got %d", len(got))
	}
	if again := realbotCopytradeFreshTrades(state, trades, "cond-1", "shares"); len(again) != 0 {
		t.Fatalf("expected repeated identical snapshot to stay deduped, got %d", len(again))
	}
}

func TestRealbotCopytradeFreshTradesKeepsIdenticalPublicTradesAcrossPolls(t *testing.T) {
	state := newRealbotCopytradeState()
	state.startedAt = time.Unix(1000, 0)

	// Poll 1: sees 3 identical trades
	trades1 := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
	}

	got1 := realbotCopytradeFreshTrades(state, trades1, "cond-1", "shares")
	if len(got1) != 3 {
		t.Fatalf("Poll 1: expected 3 fresh trades, got %d", len(got1))
	}

	// Poll 2: sees 5 identical trades (2 new ones added to the batch)
	trades2 := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
	}

	got2 := realbotCopytradeFreshTrades(state, trades2, "cond-1", "shares")
	if len(got2) != 2 {
		t.Fatalf("Poll 2: expected 2 fresh trades (3 already seen, 5 total in poll), got %d", len(got2))
	}
}

func TestRealbotCopytradeTakeRetryTradesDropsStaleTimestampedSignals(t *testing.T) {
	state := newRealbotCopytradeState()
	now := time.Unix(5000, 0)
	state.retryTrades = []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 1, Timestamp: now.Add(-25 * time.Second).Unix()},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 1, Timestamp: now.Add(-5 * time.Second).Unix()},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 1, Timestamp: 0},
	}

	got := realbotCopytradeTakeRetryTrades(state, now)
	if len(got) != 2 {
		t.Fatalf("expected stale retries to be filtered, got %d", len(got))
	}
	if len(state.retryTrades) != 0 {
		t.Fatalf("expected retry queue to be drained after take, got %d", len(state.retryTrades))
	}
}

func TestRealbotCopytradeQueueRetryTradesCapsQueueLength(t *testing.T) {
	state := newRealbotCopytradeState()
	retries := make([]api.PublicTrade, realbotCopytradeRetryQueueCap+8)
	for i := range retries {
		retries[i] = api.PublicTrade{
			ConditionID: "cond-1",
			Outcome:     "Up",
			Side:        "BUY",
			Size:        1,
			Timestamp:   int64(1000 + i),
		}
	}

	realbotCopytradeQueueRetryTrades(state, retries)
	if len(state.retryTrades) != realbotCopytradeRetryQueueCap {
		t.Fatalf("expected retry queue cap %d, got %d", realbotCopytradeRetryQueueCap, len(state.retryTrades))
	}
	wantFirst := retries[len(retries)-realbotCopytradeRetryQueueCap].Timestamp
	if state.retryTrades[0].Timestamp != wantFirst {
		t.Fatalf("expected queue to keep newest retries starting at %d, got %d", wantFirst, state.retryTrades[0].Timestamp)
	}
}

func TestRealbotCopytradeTradeKeyPrefersSignalID(t *testing.T) {
	trade := api.PublicTrade{
		ConditionID:     "cond-1",
		Outcome:         "Up",
		Side:            "BUY",
		Size:            2,
		TransactionHash: "0xtx",
		Source:          "onchain",
		SignalID:        "0xtx:1",
	}
	if got := realbotCopytradeTradeKey(trade); got != "signal|0xtx:1" {
		t.Fatalf("unexpected trade key %q", got)
	}
}

func TestRealbotEstimatedPositionBuySignalsSplitsUsingObservedTradeSize(t *testing.T) {
	state := newRealbotCopytradeState()
	realbotObserveCopytradeBuySignal(state, api.PublicTrade{Outcome: "Up", Side: "BUY", Size: 10, Timestamp: 1001})
	realbotObserveCopytradeBuySignal(state, api.PublicTrade{Outcome: "Up", Side: "BUY", Size: 10, Timestamp: 1002})

	got := realbotEstimatedPositionBuySignals(state, "cond-1", "Up", 50, core.CopytradeSizingModeUSDC)
	if len(got) != 5 {
		t.Fatalf("expected 5 estimated position buys, got %d", len(got))
	}
	total := 0.0
	for _, sig := range got {
		total += sig.Size
		if sig.Source != "position-estimate" {
			t.Fatalf("expected position-estimate source, got %q", sig.Source)
		}
	}
	if total < 49.99 || total > 50.01 {
		t.Fatalf("unexpected total estimated delta %.6f", total)
	}
}

func TestRealbotEstimatedPositionBuySignalsKeepsSinglePercentSignal(t *testing.T) {
	state := newRealbotCopytradeState()
	got := realbotEstimatedPositionBuySignals(state, "cond-1", "Up", 50, core.CopytradeSizingModePercent)
	if len(got) != 1 {
		t.Fatalf("expected 1 percent-mode signal, got %d", len(got))
	}
	if got[0].Source != "position" {
		t.Fatalf("expected position source, got %q", got[0].Source)
	}
}

func TestRealbotCopytradeIsRateLimited(t *testing.T) {
	if !realbotCopytradeIsRateLimited(errors.New("get public trades failed with status 429: error code: 1015")) {
		t.Fatal("expected 429/1015 error to be treated as rate limit")
	}
	if realbotCopytradeIsRateLimited(errors.New("context deadline exceeded")) {
		t.Fatal("expected non-429 timeout error not to be treated as rate limit")
	}
}

func TestRealbotCopytradeRateLimitBackoffCaps(t *testing.T) {
	if got := realbotCopytradeRateLimitBackoff(1); got != time.Second {
		t.Fatalf("first backoff = %v, want 1s", got)
	}
	if got := realbotCopytradeRateLimitBackoff(2); got != 2*time.Second {
		t.Fatalf("second backoff = %v, want 2s", got)
	}
	if got := realbotCopytradeRateLimitBackoff(5); got != 8*time.Second {
		t.Fatalf("capped backoff = %v, want 8s", got)
	}
}

func TestRealbotCopytradeHoldsBothOutcomes(t *testing.T) {
	if !realbotCopytradeHoldsBothOutcomes(map[string]float64{"Up": 10, "Down": 5}) {
		t.Fatal("expected both-sided target inventory to be detected")
	}
	if realbotCopytradeHoldsBothOutcomes(map[string]float64{"Up": 10, "Down": 0.009}) {
		t.Fatal("expected dust on second side not to count as both-sided inventory")
	}
}

func TestRealbotCopytradeHasAmbiguousPositionExit(t *testing.T) {
	positions := []api.Position{
		{ConditionID: "cond-1", Outcome: "Up", Size: 10, Mergeable: true},
		{ConditionID: "cond-2", Outcome: "Down", Size: 10},
	}
	if !realbotCopytradeHasAmbiguousPositionExit(positions, "cond-1") {
		t.Fatal("expected mergeable target inventory to block position-only sell fallback")
	}
	if realbotCopytradeHasAmbiguousPositionExit(positions, "cond-2") {
		t.Fatal("expected unrelated non-mergeable market not to be blocked")
	}
}

func TestRealbotTraderLoopIntervalUsesSlowerCadenceForCopytrade(t *testing.T) {
	if got := realbotTraderLoopInterval(paper.TUISettings{PaperArbMode: "copytrade", CopytradePollIntervalMs: 250}); got != 125*time.Millisecond {
		t.Fatalf("expected copytrade loop interval 125ms, got %s", got)
	}
	if got := realbotTraderLoopInterval(paper.TUISettings{PaperArbMode: "maker"}); got != realbotMainLoopInterval {
		t.Fatalf("expected default loop interval %s, got %s", realbotMainLoopInterval, got)
	}
}

func TestRealbotUIIntervalUsesSlowerCadenceForCopytrade(t *testing.T) {
	if got := realbotUIInterval(paper.TUISettings{PaperArbMode: "copytrade", CopytradePollIntervalMs: 250}); got != 1000*time.Millisecond {
		t.Fatalf("expected copytrade UI interval 1000ms, got %s", got)
	}
	if got := realbotUIInterval(paper.TUISettings{PaperArbMode: "maker"}); got != realbotUIRefreshInterval {
		t.Fatalf("expected default UI interval %s, got %s", realbotUIRefreshInterval, got)
	}
}

func TestRealbotDecisionEvalIntervalKeepsUrgentModesFast(t *testing.T) {
	if got := realbotDecisionEvalInterval(paper.TUISettings{PaperArbMode: "maker"}, time.Minute, false); got != realbotMainLoopInterval {
		t.Fatalf("expected maker decision interval %s, got %s", realbotMainLoopInterval, got)
	}
	if got := realbotDecisionEvalInterval(paper.TUISettings{PaperArbMode: "taker", TakerCloseMarket: true}, time.Minute, false); got != realbotMainLoopInterval {
		t.Fatalf("expected taker-close decision interval %s, got %s", realbotMainLoopInterval, got)
	}
	if got := realbotDecisionEvalInterval(paper.TUISettings{PaperArbMode: "taker"}, 20*time.Second, false); got != realbotMainLoopInterval {
		t.Fatalf("expected near-expiry decision interval %s, got %s", realbotMainLoopInterval, got)
	}
	if got := realbotDecisionEvalInterval(paper.TUISettings{PaperArbMode: "taker"}, time.Minute, true); got != realbotMainLoopInterval {
		t.Fatalf("expected in-flight decision interval %s, got %s", realbotMainLoopInterval, got)
	}
}

func TestRealbotDecisionEvalIntervalSlowsSteadyStateModes(t *testing.T) {
	if got := realbotDecisionEvalInterval(paper.TUISettings{PaperArbMode: "taker"}, time.Minute, false); got != realbotDecisionLoopInterval {
		t.Fatalf("expected steady-state taker decision interval %s, got %s", realbotDecisionLoopInterval, got)
	}
	if got := realbotDecisionEvalInterval(paper.TUISettings{PaperArbMode: "binance-gap"}, time.Minute, false); got != 75*time.Millisecond {
		t.Fatalf("expected binance-gap decision interval 75ms, got %s", got)
	}
}

func TestRealbotShouldRunDecisionLoopPrioritizesNewQuotes(t *testing.T) {
	base := time.Unix(1000, 0)
	lastEval := base
	lastQuote := base
	latestQuote := base.Add(10 * time.Millisecond)

	if !realbotShouldRunDecisionLoop(base.Add(50*time.Millisecond), lastEval, lastQuote, latestQuote, 100*time.Millisecond) {
		t.Fatal("expected new quote inside interval to trigger loop immediately")
	}
	if realbotShouldRunDecisionLoop(base.Add(50*time.Millisecond), lastEval, latestQuote, latestQuote, 100*time.Millisecond) {
		t.Fatal("expected no new quote inside interval to be throttled")
	}
	if !realbotShouldRunDecisionLoop(base.Add(100*time.Millisecond), lastEval, latestQuote, latestQuote, 100*time.Millisecond) {
		t.Fatal("expected decision loop to run once interval elapses even without new quotes")
	}
}

func TestFormatShareQtyKeepsFiveDecimalInventoryPrecision(t *testing.T) {
	if got := formatShareQty(1.234567); got != "1.23457" {
		t.Fatalf("expected 5-decimal share precision, got %q", got)
	}
}

func TestRealbotCanonicalizeMarketTokensPrefersCLOBMetadataByTokenID(t *testing.T) {
	market := &api.Market{
		ConditionID: "cond-1",
		Tokens: []api.Token{
			{TokenID: "up-token", Outcome: "Down"},
			{TokenID: "down-token", Outcome: "Up"},
		},
	}
	info := &api.MarketInfo{
		ConditionID: "cond-1",
		Tokens: []struct {
			TokenID string      `json:"token_id"`
			Outcome string      `json:"outcome"`
			Winner  bool        `json:"winner"`
			Price   interface{} `json:"price"`
		}{
			{TokenID: "up-token", Outcome: "Up"},
			{TokenID: "down-token", Outcome: "Down"},
		},
	}

	changed, matched := realbotCanonicalizeMarketTokens(market, info)
	if !changed {
		t.Fatal("expected canonicalization to fix flipped token labels")
	}
	if matched != 2 {
		t.Fatalf("expected 2 matched tokens, got %d", matched)
	}
	if market.Tokens[0].Outcome != "Up" || market.Tokens[1].Outcome != "Down" {
		t.Fatalf("unexpected canonicalized outcomes: %+v", market.Tokens)
	}
}

func TestRealbotMatchedAskLiquidityHonorsExecutionSum(t *testing.T) {
	asks0 := []paper.MarketLevel{{Price: 0.36, Size: 3}, {Price: 0.38, Size: 2}}
	asks1 := []paper.MarketLevel{{Price: 0.62, Size: 2}, {Price: 0.66, Size: 4}}

	liq := realbotMatchedAskLiquidity(asks0, asks1, 1.01)
	if math.Abs(liq-2.0) > 0.000001 {
		t.Fatalf("matched liquidity got %.2f want 2.00", liq)
	}
}

func TestRealbotCanUseLocalBuyQuote(t *testing.T) {
	now := time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC)
	outcomes := []string{"Down", "Up"}
	bids := map[string]float64{"Down": 0.35, "Up": 0.63}
	asks := map[string]float64{"Down": 0.36, "Up": 0.64}
	depth := map[string][]paper.MarketLevel{
		"Down": {{Price: 0.36, Size: 10}},
		"Up":   {{Price: 0.64, Size: 8}},
	}
	lastPairUpdate := now.Add(-70 * time.Millisecond)

	fresh, age, reason := realbotCanUseLocalBuyQuote(now, outcomes, bids, asks, depth, lastPairUpdate, 250*time.Millisecond)
	if !fresh || reason != "" {
		t.Fatalf("expected fresh local quote, got fresh=%v age=%v reason=%q", fresh, age, reason)
	}

	lastPairUpdate = now.Add(-400 * time.Millisecond)
	fresh, _, reason = realbotCanUseLocalBuyQuote(now, outcomes, bids, asks, depth, lastPairUpdate, 250*time.Millisecond)
	if fresh || reason == "" {
		t.Fatalf("expected stale quote rejection, got fresh=%v reason=%q", fresh, reason)
	}
}

func TestRealbotEnsureFreshBuyExecutionQuoteFallsBackToREST(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/book" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		ts := time.Now().UTC().Format(time.RFC3339Nano)
		switch r.URL.Query().Get("token_id") {
		case "down-token":
			_, _ = w.Write([]byte("{\"asset_id\":\"down-token\",\"timestamp\":\"" + ts + "\",\"bids\":[{\"price\":\"0.34\",\"size\":\"7\"}],\"asks\":[{\"price\":\"0.35\",\"size\":\"9\"}]}"))
		case "up-token":
			_, _ = w.Write([]byte("{\"asset_id\":\"up-token\",\"timestamp\":\"" + ts + "\",\"bids\":[{\"price\":\"0.61\",\"size\":\"8\"}],\"asks\":[{\"price\":\"0.62\",\"size\":\"10\"}]}"))
		default:
			http.Error(w, "unexpected token: "+r.URL.String(), http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := api.NewRestClient("polymarket")
	client.BaseURL = server.URL
	market := &api.Market{Tokens: []api.Token{{TokenID: "down-token", Outcome: "Down"}, {TokenID: "up-token", Outcome: "Up"}}}
	outcomes := []string{"Down", "Up"}
	tokenBids := map[string]float64{"Down": 0.20, "Up": 0.60}
	tokenAsks := map[string]float64{"Down": 0.31, "Up": 0.61}
	tokenFullBids := map[string][]paper.MarketLevel{}
	tokenFullAsks := map[string][]paper.MarketLevel{}
	quoteState := map[string]realbotQuoteState{
		"Down": {UpdatedAt: time.Now().Add(-10 * time.Second), Source: "ws"},
		"Up":   {UpdatedAt: time.Now().Add(-10 * time.Second), Source: "ws"},
	}
	lastPairUpdate := time.Time{}

	source, _, detail, err := realbotEnsureFreshBuyExecutionQuote(context.Background(), client, market, outcomes, tokenBids, tokenAsks, tokenFullBids, tokenFullAsks, quoteState, lastPairUpdate, 250*time.Millisecond, &lastPairUpdate)
	if err != nil {
		t.Fatalf("expected REST refresh to succeed, got %v", err)
	}
	if source != "rest" {
		t.Fatalf("expected REST source, got %q", source)
	}
	if strings.TrimSpace(detail) == "" {
		t.Fatalf("expected stale-local detail, got %q", detail)
	}
	if math.Abs(tokenAsks["Down"]-0.35) > 0.000001 || math.Abs(tokenAsks["Up"]-0.62) > 0.000001 {
		t.Fatalf("expected refreshed asks, got Down=%.3f Up=%.3f", tokenAsks["Down"], tokenAsks["Up"])
	}
	if len(tokenFullAsks["Down"]) == 0 || len(tokenFullAsks["Up"]) == 0 {
		t.Fatalf("expected refreshed ask depth, got Down=%d Up=%d", len(tokenFullAsks["Down"]), len(tokenFullAsks["Up"]))
	}
	if quoteState["Down"].Source != "rest-exec" || quoteState["Up"].Source != "rest-exec" {
		t.Fatalf("expected refreshed quote source, got Down=%q Up=%q", quoteState["Down"].Source, quoteState["Up"].Source)
	}
}

func TestRealbotCanUseLocalTakerCloseQuoteAcceptsFreshWSAsk(t *testing.T) {
	now := time.Date(2026, 3, 21, 10, 0, 0, 0, time.UTC)
	bids := map[string]float64{"Up": 0.82}
	asks := map[string]float64{"Up": 0.83}
	depth := map[string][]paper.MarketLevel{
		"Up": {{Price: 0.83, Size: 12}},
	}
	state := map[string]realbotQuoteState{
		"Up": {UpdatedAt: now.Add(-120 * time.Millisecond), Source: "ws"},
	}

	price, reason, ok := realbotCanUseLocalTakerCloseQuote(now, "Up", bids, asks, depth, state, 350*time.Millisecond)
	if !ok {
		t.Fatalf("expected fresh WS taker-close quote, got reason=%q", reason)
	}
	if math.Abs(price-0.83) > 0.000001 {
		t.Fatalf("expected local confirm price 0.83, got %.3f", price)
	}
}

func TestRealbotCanUseLocalTakerCloseQuoteRejectsNonWSOrStaleQuote(t *testing.T) {
	now := time.Date(2026, 3, 21, 10, 0, 0, 0, time.UTC)
	bids := map[string]float64{"Up": 0.82}
	asks := map[string]float64{"Up": 0.83}
	depth := map[string][]paper.MarketLevel{
		"Up": {{Price: 0.83, Size: 12}},
	}

	price, reason, ok := realbotCanUseLocalTakerCloseQuote(now, "Up", bids, asks, depth, map[string]realbotQuoteState{
		"Up": {UpdatedAt: now.Add(-100 * time.Millisecond), Source: "rest-exec"},
	}, 350*time.Millisecond)
	if ok || price != 0 || !strings.Contains(reason, "not aggressive-safe") {
		t.Fatalf("expected rest-exec quote rejection, got ok=%v price=%.3f reason=%q", ok, price, reason)
	}

	price, reason, ok = realbotCanUseLocalTakerCloseQuote(now, "Up", bids, asks, depth, map[string]realbotQuoteState{
		"Up": {UpdatedAt: now.Add(-500 * time.Millisecond), Source: "ws"},
	}, 350*time.Millisecond)
	if ok || price != 0 || !strings.Contains(reason, "quote age") {
		t.Fatalf("expected stale quote rejection, got ok=%v price=%.3f reason=%q", ok, price, reason)
	}
}

func TestRealbotHandleTakerCloseWindowStopsAtBusyEntryGate(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	gate := newRealbotEntryGate()
	if !gate.TryAcquire() {
		t.Fatal("expected fresh entry gate to be acquirable for test setup")
	}
	defer gate.Release()

	liveCfg := paper.TUISettings{
		TakerCloseMarket:         true,
		TakerCloseMarketTime:     60,
		TakerCloseMarketMinPrice: 0.60,
		TakerCloseMarketSlippage: 0.99,
		TradeSizingMode:          "fixed",
		TradeSizeUSDC:            5,
	}
	takerCloseAttempted := false
	takerCloseExecutedAt := time.Time{}
	lastLogAt := time.Time{}
	lastLogKey := ""
	lastQuoteRefresh := time.Time{}
	lastForceReconnect := time.Time{}

	handled := realbotHandleTakerCloseWindow(realbotTakerCloseStrategyArgs{
		ctx:                 context.Background(),
		marketID:            "BTC",
		market:              &api.Market{Tokens: []api.Token{{TokenID: "up-token", Outcome: "Up"}}},
		outcomes:            []string{"Down", "Up"},
		tokenMap:            map[string]string{"up-token": "Up"},
		tokenToOutcome:      map[string]string{"up-token": "Up"},
		tokenBids:           map[string]float64{"Down": 0.17, "Up": 0.82},
		tokenAsks:           map[string]float64{"Down": 0.18, "Up": 0.83},
		tokenFullAsks:       map[string][]paper.MarketLevel{"Up": {{Price: 0.83, Size: 20}}},
		quoteState:          map[string]realbotQuoteState{"Up": {UpdatedAt: time.Now(), Source: "ws"}},
		tokenFeeRates:       map[string]int{"Up": 1000},
		liveCfg:             liveCfg,
		timeToExpiry:        30 * time.Second,
		entryTradingAllowed: true,
		wsMgr:               &api.WSManager{},
		engine:              engine,
		tui:                 tui,
		entryGate:           gate,
	}, &realbotTakerCloseStrategyState{
		takerCloseAttempted:        &takerCloseAttempted,
		takerCloseExecutedAt:       &takerCloseExecutedAt,
		lastTakerCloseLog:          &lastLogAt,
		lastTakerCloseLogKey:       &lastLogKey,
		lastTakerCloseQuoteRefresh: &lastQuoteRefresh,
		lastForceReconnect:         &lastForceReconnect,
	})
	if !handled {
		t.Fatal("expected busy entry gate to short-circuit taker-close handling")
	}
	if takerCloseAttempted {
		t.Fatal("expected busy entry gate to avoid marking taker-close attempted")
	}
	if !takerCloseExecutedAt.IsZero() {
		t.Fatal("expected no taker-close execution timestamp when gate is busy")
	}
}

func TestHandleRestFallbackWithDepthSkipsOlderBooksWhenCurrentQuoteIsFresh(t *testing.T) {
	staleTS := time.Now().Add(-2 * time.Second).UTC().Format(time.RFC3339Nano)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("token_id") {
		case "down-token":
			_, _ = w.Write([]byte("{\"asset_id\":\"down-token\",\"timestamp\":\"" + staleTS + "\",\"bids\":[{\"price\":\"0.34\",\"size\":\"7\"}],\"asks\":[{\"price\":\"0.35\",\"size\":\"9\"}]}"))
		case "up-token":
			_, _ = w.Write([]byte("{\"asset_id\":\"up-token\",\"timestamp\":\"" + staleTS + "\",\"bids\":[{\"price\":\"0.61\",\"size\":\"8\"}],\"asks\":[{\"price\":\"0.62\",\"size\":\"10\"}]}"))
		default:
			http.Error(w, "unexpected token", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := api.NewRestClient("polymarket")
	client.BaseURL = server.URL
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	tui.AddMarket("SOL", "sol", []string{"Down", "Up"}, time.Now().Add(time.Minute))

	bids := map[string]float64{"Down": 0.40, "Up": 0.58}
	asks := map[string]float64{"Down": 0.41, "Up": 0.59}
	fullBids := map[string][]paper.MarketLevel{}
	fullAsks := map[string][]paper.MarketLevel{}
	quoteState := map[string]realbotQuoteState{
		"Down": {UpdatedAt: time.Now(), Source: "ws"},
		"Up":   {UpdatedAt: time.Now(), Source: "ws"},
	}

	ok := handleRestFallbackWithDepth(context.Background(), "SOL", 12*time.Second, map[string]string{
		"down-token": "Down",
		"up-token":   "Up",
	}, bids, asks, map[string]float64{}, map[string]float64{}, fullBids, fullAsks, quoteState, nil, nil, engine, client, tui, false)
	if !ok {
		t.Fatal("expected fallback call to complete")
	}
	if math.Abs(bids["Down"]-0.40) > 0.000001 || math.Abs(asks["Up"]-0.59) > 0.000001 {
		t.Fatalf("expected stale REST data to be ignored, got bids=%v asks=%v", bids, asks)
	}
}

func TestHandleRestFallbackWithDepthPreservesDisplayForOneSidedBooks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("token_id") {
		case "down-token":
			_, _ = w.Write([]byte(`{"asset_id":"down-token","timestamp":"2026-03-20T00:00:00Z","bids":[{"price":"0.99","size":"12"}],"asks":[]}`))
		case "up-token":
			_, _ = w.Write([]byte(`{"asset_id":"up-token","timestamp":"2026-03-20T00:00:00Z","bids":[],"asks":[{"price":"0.02","size":"8"}]}`))
		default:
			http.Error(w, "unexpected token", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := api.NewRestClient("polymarket")
	client.BaseURL = server.URL
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	tui.AddMarket("BTC", "btc", []string{"Down", "Up"}, time.Now().Add(time.Minute))

	bids := map[string]float64{}
	asks := map[string]float64{}
	displayBids := map[string]float64{}
	displayAsks := map[string]float64{}
	fullBids := map[string][]paper.MarketLevel{}
	fullAsks := map[string][]paper.MarketLevel{}
	quoteState := map[string]realbotQuoteState{}

	ok := handleRestFallbackWithDepth(context.Background(), "BTC", 12*time.Second, map[string]string{
		"down-token": "Down",
		"up-token":   "Up",
	}, bids, asks, displayBids, displayAsks, fullBids, fullAsks, quoteState, nil, nil, engine, client, tui, false)
	if !ok {
		t.Fatal("expected fallback call to complete")
	}
	// With high-bid tolerance (Down bid 0.99 ≥ 0.60), one-sided books are
	// preserved rather than pair-cleared. This matches real market behavior
	// at extreme prices where the complement side has sparse liquidity.
	if bids["Down"] != 0.99 {
		t.Fatalf("expected high-bid side to be preserved, got bids=%v", bids)
	}
	if math.Abs(displayBids["Down"]-0.99) > 0.000001 {
		t.Fatalf("expected display bid to preserve one-sided quote, got %.3f", displayBids["Down"])
	}
	if math.Abs(displayAsks["Up"]-0.02) > 0.000001 {
		t.Fatalf("expected display ask to preserve one-sided quote, got %.3f", displayAsks["Up"])
	}
}

func TestRealbotCanUseLocalSellQuote(t *testing.T) {
	now := time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC)
	outcomes := []string{"Down", "Up"}
	bids := map[string]float64{"Down": 0.54, "Up": 0.49}
	asks := map[string]float64{"Down": 0.55, "Up": 0.50}
	depth := map[string][]paper.MarketLevel{
		"Down": {{Price: 0.54, Size: 8}},
		"Up":   {{Price: 0.49, Size: 10}},
	}
	lastPairUpdate := now.Add(-70 * time.Millisecond)

	fresh, age, reason := realbotCanUseLocalSellQuote(now, outcomes, bids, asks, depth, lastPairUpdate, 250*time.Millisecond)
	if !fresh || reason != "" || age != 70*time.Millisecond {
		t.Fatalf("expected fresh local sell quote, got fresh=%v age=%v reason=%q", fresh, age, reason)
	}

	lastPairUpdate = now.Add(-400 * time.Millisecond)
	fresh, _, reason = realbotCanUseLocalSellQuote(now, outcomes, bids, asks, depth, lastPairUpdate, 250*time.Millisecond)
	if fresh || reason == "" {
		t.Fatalf("expected stale sell quote rejection, got fresh=%v reason=%q", fresh, reason)
	}
}

func TestRealbotBuildCleanupSellQuoteKeepsConfiguredFloor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/book" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		ts := time.Now().UTC().Format(time.RFC3339Nano)
		_, _ = w.Write([]byte("{\"asset_id\":\"token-up\",\"timestamp\":\"" + ts + "\",\"bids\":[{\"price\":\"0.54\",\"size\":\"7\"}],\"asks\":[{\"price\":\"0.55\",\"size\":\"4\"}]}"))
	}))
	defer server.Close()

	client := api.NewRestClient("polymarket")
	client.BaseURL = server.URL

	_, err := realbotBuildCleanupSellQuote(context.Background(), client, "token-up", 2, 0.60)
	if err == nil {
		t.Fatal("expected cleanup quote to reject bids below the configured floor")
	}
	if !strings.Contains(err.Error(), "below") {
		t.Fatalf("expected floor-liquidity rejection, got %v", err)
	}
}

func TestRealbotBuildCleanupSellQuoteUsesLiquidityAtConfiguredFloor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/book" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		ts := time.Now().UTC().Format(time.RFC3339Nano)
		_, _ = w.Write([]byte("{\"asset_id\":\"token-up\",\"timestamp\":\"" + ts + "\",\"bids\":[{\"price\":\"0.60\",\"size\":\"1.25\"},{\"price\":\"0.59\",\"size\":\"8\"}],\"asks\":[{\"price\":\"0.61\",\"size\":\"4\"}]}"))
	}))
	defer server.Close()

	client := api.NewRestClient("polymarket")
	client.BaseURL = server.URL

	quote, err := realbotBuildCleanupSellQuote(context.Background(), client, "token-up", 2, 0.60)
	if err != nil {
		t.Fatalf("expected cleanup quote at configured floor to succeed, got %v", err)
	}
	if math.Abs(quote.SubmitPrice-0.60) > 0.000001 {
		t.Fatalf("expected cleanup submit price 0.60, got %.3f", quote.SubmitPrice)
	}
	if math.Abs(quote.TotalBidLiquidity-1.25) > 0.000001 {
		t.Fatalf("expected only floor-respecting liquidity to count, got %.2f", quote.TotalBidLiquidity)
	}
	if math.Abs(quote.ExecutableQty-1.25) > 0.000001 {
		t.Fatalf("expected executable qty to cap at floor liquidity 1.25, got %.2f", quote.ExecutableQty)
	}
}

func TestRealbotLocalQuoteSanityReasonRejectsWideOutcomeSpread(t *testing.T) {
	reason := realbotLocalQuoteSanityReason(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0.40, "Up": 0.56},
		map[string]float64{"Down": 0.57, "Up": 0.60},
	)
	if reason == "" || !strings.Contains(reason, "wide local spread") {
		t.Fatalf("expected wide-spread rejection, got %q", reason)
	}
}

func TestRealbotLocalQuoteSanityReasonRejectsImpossiblePairSum(t *testing.T) {
	reason := realbotLocalQuoteSanityReason(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0.46, "Up": 0.47},
		map[string]float64{"Down": 0.55, "Up": 0.56},
	)
	if reason == "" || !strings.Contains(reason, "ask pair sum") {
		t.Fatalf("expected pair-sum rejection, got %q", reason)
	}
}

func TestRealbotMatchedBidLiquidityHonorsExecutionSum(t *testing.T) {
	bids0 := []paper.MarketLevel{{Price: 0.54, Size: 3}, {Price: 0.52, Size: 2}}
	bids1 := []paper.MarketLevel{{Price: 0.49, Size: 2}, {Price: 0.46, Size: 4}}

	liq := realbotMatchedBidLiquidity(bids0, bids1, 1.01)
	if math.Abs(liq-2.0) > 0.000001 {
		t.Fatalf("matched bid liquidity got %.2f want 2.00", liq)
	}
}

func TestBuildDirectMarketOrderRequestUsesFAKLimitShape(t *testing.T) {
	req := buildDirectMarketOrderRequest(directMarketOrderSignalRequest{
		Side:       api.SideBuy,
		TokenID:    "token-up",
		Outcome:    "Up",
		Price:      0.47,
		Size:       12.5,
		FeeRateBps: 85,
	})

	if req.TokenID != "token-up" || req.Price != 0.47 || req.Size != 12.5 {
		t.Fatalf("unexpected request payload: %+v", req)
	}
	if req.Side != api.SideBuy {
		t.Fatalf("expected buy side, got %s", req.Side)
	}
	if req.OrderType != api.OrderTypeLimit {
		t.Fatalf("expected limit order type, got %s", req.OrderType)
	}
	if req.TimeInForce != api.TIFFillAndKill {
		t.Fatalf("expected FAK time-in-force, got %s", req.TimeInForce)
	}
	if req.FeeRateBps != 85 {
		t.Fatalf("expected fee rate 85, got %d", req.FeeRateBps)
	}
}

func TestHydrateDirectMarketTradeResultBackfillsBatchMetadata(t *testing.T) {
	result := hydrateDirectMarketTradeResult(directMarketOrderSignalRequest{
		Side:       api.SideSell,
		TokenID:    "token-down",
		Outcome:    "Down",
		Price:      0.58,
		Size:       9,
		FeeRateBps: 100,
	}, &trading.TradeResult{OrderID: "ord-123", Status: "matched"})

	if result.OrderID != "ord-123" || result.Status != "matched" {
		t.Fatalf("expected existing venue fields to survive, got %+v", result)
	}
	if result.Side != string(api.SideSell) || result.TokenID != "token-down" || result.Outcome != "Down" {
		t.Fatalf("expected metadata to be backfilled, got %+v", result)
	}
	if result.Price != 0.58 || result.Size != 9 {
		t.Fatalf("expected request price/size on hydrated result, got %+v", result)
	}
	if result.FeeRateBps != 100 {
		t.Fatalf("expected fee rate to be preserved, got %+v", result)
	}
	if result.Timestamp.IsZero() {
		t.Fatal("expected hydrateDirectMarketTradeResult to stamp timestamp")
	}
}

func TestClampRequestedExecutionQtyCapsAttributedOverfill(t *testing.T) {
	if got := clampRequestedExecutionQty(4.2, 4.0); got != 4.0 {
		t.Fatalf("expected attributed execution to cap at request, got %.2f", got)
	}
}

func TestClampRequestedExecutionQtyPreservesPartialFill(t *testing.T) {
	if got := clampRequestedExecutionQty(3.7, 4.0); got != 3.7 {
		t.Fatalf("expected partial fill to remain unchanged, got %.2f", got)
	}
}

func TestClampRequestedExecutionQtyAllowsRawQtyWithoutRequestCap(t *testing.T) {
	if got := clampRequestedExecutionQty(4.2, 0); got != 4.2 {
		t.Fatalf("expected uncapped qty when request size is unavailable, got %.2f", got)
	}
}

func TestSubtractMergedPairBalancesUsesActualMergeQty(t *testing.T) {
	bal0, bal1 := subtractMergedPairBalances(5.0, 4.0, 1.5)
	if math.Abs(bal0-3.5) > 0.000001 || math.Abs(bal1-2.5) > 0.000001 {
		t.Fatalf("got balances %.2f/%.2f want 3.50/2.50", bal0, bal1)
	}
}

func TestSubtractMergedPairBalancesIgnoresFailedMergeQty(t *testing.T) {
	bal0, bal1 := subtractMergedPairBalances(5.0, 4.0, 0)
	if math.Abs(bal0-5.0) > 0.000001 || math.Abs(bal1-4.0) > 0.000001 {
		t.Fatalf("got balances %.2f/%.2f want 5.00/4.00", bal0, bal1)
	}
}

func TestPreferLivePairBalancesKeepsLiveRemainderWhenBackupIsStale(t *testing.T) {
	bal0, bal1 := preferLivePairBalances(0.1748, 0, 0, 0)
	if math.Abs(bal0-0.1748) > 0.000001 || bal1 != 0 {
		t.Fatalf("got balances %.4f/%.4f want 0.1748/0.0000", bal0, bal1)
	}
}

func TestPreferLivePairBalancesAllowsBackupToFillMissingSide(t *testing.T) {
	bal0, bal1 := preferLivePairBalances(0.1748, 0, 0.1500, 0.2250)
	if math.Abs(bal0-0.1748) > 0.000001 || math.Abs(bal1-0.2250) > 0.000001 {
		t.Fatalf("got balances %.4f/%.4f want 0.1748/0.2250", bal0, bal1)
	}
}

func TestCombinePairBalanceSnapshotsKeepsLiveWhenBackupErrors(t *testing.T) {
	bal0, bal1, source, err := combinePairBalanceSnapshots(0.1748, 0, 0, 0, errors.New("backup failed"))
	if err != nil {
		t.Fatalf("expected live snapshot to survive backup failure, got err %v", err)
	}
	if math.Abs(bal0-0.1748) > 0.000001 || bal1 != 0 || source != "live WS" {
		t.Fatalf("got %.4f/%.4f source=%q", bal0, bal1, source)
	}
}

func TestCombinePairBalanceSnapshotsUsesBackupWhenLiveMissing(t *testing.T) {
	bal0, bal1, source, err := combinePairBalanceSnapshots(0, 0, 0.1500, 0.2250, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if math.Abs(bal0-0.1500) > 0.000001 || math.Abs(bal1-0.2250) > 0.000001 || source != "on-chain backup" {
		t.Fatalf("got %.4f/%.4f source=%q", bal0, bal1, source)
	}
}

func TestCombinePairBalanceSnapshotsLetsOnChainCorrectUnderreportedWS(t *testing.T) {
	bal0, bal1, source, err := combinePairBalanceSnapshots(5.0, 5.0, 5.2164, 5.931082, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if math.Abs(bal0-5.2164) > 0.000001 || math.Abs(bal1-5.931082) > 0.000001 {
		t.Fatalf("got %.6f/%.6f want 5.216400/5.931082", bal0, bal1)
	}
	if source != "live WS + on-chain backup" {
		t.Fatalf("unexpected source %q", source)
	}
}

func TestSubtractMergedPairBalancesLeavesPostMergeCleanupRemainder(t *testing.T) {
	bal0, bal1 := subtractMergedPairBalances(5.2164, 5.931082, 5.0)
	if math.Abs(bal0-0.2164) > 0.000001 || math.Abs(bal1-0.931082) > 0.000001 {
		t.Fatalf("got balances %.6f/%.6f want 0.216400/0.931082", bal0, bal1)
	}
}

func TestIsMinSizeRejectionMessage(t *testing.T) {
	if !isMinSizeRejectionMessage("invalid amount for a marketable SELL order, min size: $1") {
		t.Fatal("expected min-size rejection to be detected")
	}
	if !isMinSizeRejectionMessage("MIN SIZE violation") {
		t.Fatal("expected detection to be case-insensitive")
	}
	if isMinSizeRejectionMessage("order was killed with no fill") {
		t.Fatal("unexpected min-size detection for unrelated message")
	}
}

func TestCleanupRejectionMessageAvoidsHardcodedDollarMinimum(t *testing.T) {
	msg := cleanupRejectionMessage(0.23, "YES", "invalid amount for a marketable SELL order, min size: $1")
	if !strings.Contains(msg, "Cleanup attempt rejected") {
		t.Fatalf("expected cleanup rejection wording, got %q", msg)
	}
	if !strings.Contains(msg, "0.23 YES shares") {
		t.Fatalf("expected quantity/outcome in message, got %q", msg)
	}
	if strings.Contains(msg, "$1.00 minimum") {
		t.Fatalf("message should not claim a hard-coded $1 minimum, got %q", msg)
	}
}

func TestCleanupRejectionMessagePreservesDustPrecision(t *testing.T) {
	msg := cleanupRejectionMessage(0.000432, "YES", "venue said no")
	if !strings.Contains(msg, "0.00043 YES shares") {
		t.Fatalf("expected dust precision in message, got %q", msg)
	}
}

func TestShouldAttemptCleanupSellAllowsDust(t *testing.T) {
	if !shouldAttemptCleanupSell(0.000432) {
		t.Fatal("expected dust sell quantity to be attempted")
	}
	if shouldAttemptCleanupSell(0.0000001) {
		t.Fatal("expected near-zero quantity to be ignored")
	}
}

func TestNormalizeMarketSellShares(t *testing.T) {
	if got := normalizeMarketSellShares(0.2363); got != 0.23 {
		t.Fatalf("expected 0.23, got %.4f", got)
	}
	if got := normalizeMarketSellShares(0.0878); got != 0.08 {
		t.Fatalf("expected 0.08, got %.4f", got)
	}
	if got := normalizeMarketSellShares(0.0081); got != 0.00 {
		t.Fatalf("expected 0.00, got %.4f", got)
	}
}

func TestHasConfirmedExecutedQtyTreatsSellDustAsConfirmed(t *testing.T) {
	if !hasConfirmedExecutedQty("SELL", 0.000432) {
		t.Fatal("expected dust sell execution to count as confirmed")
	}
	if hasConfirmedExecutedQty("BUY", 0.000432) {
		t.Fatal("expected tiny buy execution to remain below confirmation threshold")
	}
}

func TestDirectExecutionHasSizingDriftFlagsVenueOverfill(t *testing.T) {
	exec := directMarketExecution{AcknowledgedQty: 7.885631, AcknowledgedNotional: 2.15}
	if !directExecutionHasSizingDrift(exec, 5.0) {
		t.Fatal("expected acknowledged overfill to be flagged as sizing drift")
	}
	if got := venueExecutionEffectivePrice(exec); math.Abs(got-0.272648) > 0.00001 {
		t.Fatalf("unexpected effective price %.6f", got)
	}
}

func TestDirectExecutionHasSizingDriftIgnoresTinyRoundingNoise(t *testing.T) {
	exec := directMarketExecution{AcknowledgedQty: 5.0099, AcknowledgedNotional: 2.15}
	if directExecutionHasSizingDrift(exec, 5.0) {
		t.Fatal("expected tiny acknowledgment noise to be ignored")
	}
}

func TestReportedBuyCostUsesAcknowledgedNotionalWhenAttributedSizeMatches(t *testing.T) {
	exec := directMarketExecution{AcknowledgedQty: 3.14, AcknowledgedNotional: 3.1086}
	got := reportedBuyCost(exec, 0.99, 3.12, 3.14)
	if math.Abs(got-3.1086) > 0.000001 {
		t.Fatalf("expected acknowledged notional 3.1086, got %.6f", got)
	}
}

func TestReportedBuyCostUsesAttributedSizeWhenAcknowledgedSizeDrifts(t *testing.T) {
	exec := directMarketExecution{AcknowledgedQty: 3.14, AcknowledgedNotional: 3.1086}
	got := reportedBuyCost(exec, 0.99, 3.00, 3.14)
	expected := 2.97
	if math.Abs(got-expected) > 0.000001 {
		t.Fatalf("expected attributed notional %.6f, got %.6f", expected, got)
	}
}

func TestReportedSellProceedsUsesAcknowledgedNotionalWhenAttributedSizeMatches(t *testing.T) {
	exec := directMarketExecution{AcknowledgedQty: 3.14, AcknowledgedNotional: 1.884}
	got := reportedSellProceeds(exec, 0.58, 3.12, 3.14)
	if math.Abs(got-1.884) > 0.000001 {
		t.Fatalf("expected acknowledged proceeds 1.8840, got %.6f", got)
	}
}

func TestReportedSellProceedsUsesAttributedSizeWhenAcknowledgedSizeDrifts(t *testing.T) {
	exec := directMarketExecution{AcknowledgedQty: 3.14, AcknowledgedNotional: 1.884}
	got := reportedSellProceeds(exec, 0.58, 3.00, 3.14)
	expected := 1.74
	if math.Abs(got-expected) > 0.000001 {
		t.Fatalf("expected attributed proceeds %.6f, got %.6f", expected, got)
	}
}

func TestRealbotApplySplitSellAccountingCreditsBalanceAndRealizedPnL(t *testing.T) {
	engine := paper.NewEngine(10)
	engine.DeductBalance(2)
	inv := paper.NewSplitInventory()
	inv.RecordSplit("BTC", "Up", "Down", 2)

	profit := realbotApplySplitSellAccounting(engine, inv, "BTC", "Up", 1.5, 0.57, 0.855, false)
	if math.Abs(profit-0.105) > 0.000001 {
		t.Fatalf("expected split sell profit 0.1050, got %.6f", profit)
	}
	if got := engine.GetBalance(); math.Abs(got-8.855) > 0.000001 {
		t.Fatalf("expected engine balance 8.8550 after sell proceeds, got %.6f", got)
	}
	if got := engine.GetStats().RealizedPnL; math.Abs(got-0.105) > 0.000001 {
		t.Fatalf("expected realized pnl 0.1050 after split sell, got %.6f", got)
	}
}

func TestStartupPositionsSummarySuppressesDetailedDump(t *testing.T) {
	summary := startupPositionsSummary([]trading.PositionInfo{
		{TokenID: "token-a", Outcome: "YES", Size: 2.89, AvgPrice: 0},
		{TokenID: "token-b", Outcome: "NO", Size: 34.47, AvgPrice: 0},
	})
	if summary != "📊 Open positions: 2 token(s), 37.36 total shares" {
		t.Fatalf("unexpected summary: %q", summary)
	}
	if strings.Contains(summary, "token-a") || strings.Contains(summary, "YES") {
		t.Fatalf("summary should suppress per-position detail, got %q", summary)
	}
}

type stubRealbotOrderWarmer struct {
	calls chan struct{}
}

func (s *stubRealbotOrderWarmer) GetTradingAllowance(context.Context) (float64, error) {
	select {
	case s.calls <- struct{}{}:
	default:
	}
	return 1, nil
}

func TestPrimeRealbotOrderPathStartsAsyncWarmup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	warmer := &stubRealbotOrderWarmer{calls: make(chan struct{}, 1)}
	primeRealbotOrderPath(ctx, warmer)

	select {
	case <-warmer.calls:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected realbot order-path primer to issue async warmup call")
	}
}

func TestBuildRealbotTakerClosePlan_UsesLimitPriceForSizing(t *testing.T) {
	liveCfg := paper.TUISettings{
		TakerCloseMarketSlippage: 0.99,
		TakerCloseMarketMinPrice: 0.60,
	}

	plan, err := buildRealbotTakerClosePlan(5.0, 0.67, liveCfg)
	if err != nil {
		t.Fatalf("buildRealbotTakerClosePlan returned error: %v", err)
	}

	if math.Abs(plan.RequestedQty-5.0505) > 0.000001 {
		t.Fatalf("expected 5.0505 shares at $0.99 cap for a $5 budget, got %.4f", plan.RequestedQty)
	}

	if got := plan.RequestedQty * plan.LimitPrice; got > 5.0+1e-9 {
		t.Fatalf("expected cap-sized notional to stay within budget, got $%.4f", got)
	}
}

func TestBuildRealbotTakerClosePlan_AllowsSingleShareBudgetNearDollarCap(t *testing.T) {
	liveCfg := paper.TUISettings{
		TakerCloseMarketSlippage: 0.99,
		TakerCloseMarketMinPrice: 0.60,
	}

	plan, err := buildRealbotTakerClosePlan(1.0, 0.67, liveCfg)
	if err != nil {
		t.Fatalf("expected close plan to allow a $1 budget at a $0.99 cap, got %v", err)
	}
	if math.Abs(plan.RequestedQty-1.0101) > 0.000001 {
		t.Fatalf("expected 1.0101 shares, got %.4f", plan.RequestedQty)
	}
}

func TestBuildRealbotTakerClosePlan_UsesBudgetFloorWithoutArtificialTwoShareMinimum(t *testing.T) {
	liveCfg := paper.TUISettings{
		TakerCloseMarketSlippage: 0.99,
		TakerCloseMarketMinPrice: 0.60,
	}

	plan, err := buildRealbotTakerClosePlan(1.89, 0.92, liveCfg)
	if err != nil {
		t.Fatalf("expected one-share budget floor to pass, got %v", err)
	}
	if math.Abs(plan.RequestedQty-1.9090) > 0.000001 {
		t.Fatalf("expected 1.9090 shares, got %.4f", plan.RequestedQty)
	}
}

func TestBuildRealbotTakerClosePlan_RejectsConfirmedPriceBelowMinPrice(t *testing.T) {
	liveCfg := paper.TUISettings{
		TakerCloseMarketSlippage: 0.99,
		TakerCloseMarketMinPrice: 0.80,
	}

	if _, err := buildRealbotTakerClosePlan(5.0, 0.54, liveCfg); err == nil {
		t.Fatal("expected close plan to reject confirmed price below configured min price")
	}
}

func TestNormalizedRealbotTakerCloseMinPriceRoundsToTwoDecimals(t *testing.T) {
	if got := normalizedRealbotTakerCloseMinPrice(paper.TUISettings{TakerCloseMarketMinPrice: 0.895}); math.Abs(got-0.90) > 0.000001 {
		t.Fatalf("expected min price 0.895 to normalize to 0.90, got %.6f", got)
	}
	if got := normalizedRealbotTakerCloseMinPrice(paper.TUISettings{TakerCloseMarketMinPrice: 0.9}); math.Abs(got-0.90) > 0.000001 {
		t.Fatalf("expected min price 0.9 to stay 0.90, got %.6f", got)
	}
}

func TestRealbotLatestQuoteUpdateReturnsFreshestState(t *testing.T) {
	outcomes := []string{"Down", "Up"}
	base := time.Date(2026, 3, 21, 10, 0, 0, 0, time.UTC)
	state := map[string]realbotQuoteState{
		"Down": {UpdatedAt: base.Add(150 * time.Millisecond), Source: "ws"},
		"Up":   {UpdatedAt: base.Add(350 * time.Millisecond), Source: "rest"},
	}

	updatedAt, source := realbotLatestQuoteUpdate(outcomes, state)
	if !updatedAt.Equal(base.Add(350 * time.Millisecond)) {
		t.Fatalf("expected freshest timestamp %s, got %s", base.Add(350*time.Millisecond), updatedAt)
	}
	if source != "rest" {
		t.Fatalf("expected freshest source rest, got %q", source)
	}
}

func TestRealbotNormalizeDisplaySource(t *testing.T) {
	if got := realbotNormalizeDisplaySource("ws-bbo"); got != "WS" {
		t.Fatalf("expected WS for ws-bbo, got %q", got)
	}
	if got := realbotNormalizeDisplaySource("rest-exec"); got != "REST" {
		t.Fatalf("expected REST for rest-exec, got %q", got)
	}
	if got := realbotNormalizeDisplaySource("unknown"); got != "WS" {
		t.Fatalf("expected WS default for unknown, got %q", got)
	}
}

func TestRealbotDisplayHasUsableQuotes(t *testing.T) {
	outcomes := []string{"Down", "Up"}

	if realbotDisplayHasUsableQuotes(outcomes,
		map[string]float64{"Down": 0, "Up": 0},
		map[string]float64{"Down": 0, "Up": 0},
	) {
		t.Fatal("expected empty display quotes to be unusable")
	}

	if !realbotDisplayHasUsableQuotes(outcomes,
		map[string]float64{"Down": 0.44, "Up": 0.54},
		map[string]float64{"Down": 0.45, "Up": 0.55},
	) {
		t.Fatal("expected sane two-sided display quotes to be usable")
	}

	if !realbotDisplayHasUsableQuotes(outcomes,
		map[string]float64{"Down": 0.99, "Up": 0},
		map[string]float64{"Down": 0, "Up": 0.01},
	) {
		t.Fatal("expected terminal one-sided display quotes to be usable")
	}
}

func TestShouldSkipImmediateExecutionConfirmationForFAKNoMatch(t *testing.T) {
	result := &trading.TradeResult{
		Success: false,
		Status:  "KILLED",
		Message: "no orders found to match with FAK order. FAK orders are partially filled or killed if no match is found.",
	}

	if !shouldSkipImmediateExecutionConfirmation(result, nil) {
		t.Fatal("expected immediate confirmation skip for explicit FAK no-match")
	}
}

func TestShouldSkipImmediateExecutionConfirmationKeepsVerificationWhenFillSignalsExist(t *testing.T) {
	result := &trading.TradeResult{
		Success:         false,
		Status:          "KILLED",
		Message:         "order was killed",
		AcknowledgedQty: 1.25,
	}

	if shouldSkipImmediateExecutionConfirmation(result, nil) {
		t.Fatal("expected verification to continue when venue acknowledged quantity exists")
	}
}

func TestBuildDirectMarketOrderRequestBuyExactSharesUsesGTC(t *testing.T) {
	req := buildDirectMarketOrderRequest(directMarketOrderSignalRequest{
		Side:        api.SideBuy,
		TokenID:     "token-1",
		Price:       0.44,
		Size:        1.02,
		FeeRateBps:  1000,
		ExactShares: true,
	})

	if req.TimeInForce != api.TIFGoodTilCancelled {
		t.Fatalf("expected GTC for exact-share buy, got %q", req.TimeInForce)
	}
	if req.Size != 1.02 {
		t.Fatalf("expected buy size to remain shares, got %.4f", req.Size)
	}
}

func TestDirectOrderNotional(t *testing.T) {
	if got := directOrderNotional(0.95, 1.02); math.Abs(got-0.969) > 0.000001 {
		t.Fatalf("expected direct order notional 0.969, got %.6f", got)
	}
}

func TestHasActionableDirectOrderValueRequiresOneDollarMinimum(t *testing.T) {
	if hasActionableDirectOrderValue(0.95, 1.02) {
		t.Fatal("expected sub-$1 direct order value to be rejected")
	}
	if !hasActionableDirectOrderValue(0.99, 1.02) {
		t.Fatal("expected >=$1 direct order value to pass")
	}
}

func TestBuildDirectMarketOrderRequestSellKeepsFAK(t *testing.T) {
	req := buildDirectMarketOrderRequest(directMarketOrderSignalRequest{
		Side:       api.SideSell,
		TokenID:    "token-1",
		Price:      0.44,
		Size:       1.02,
		FeeRateBps: 1000,
	})

	if req.TimeInForce != api.TIFFillAndKill {
		t.Fatalf("expected sell to keep FAK, got %q", req.TimeInForce)
	}
}

func TestShouldCancelResidualBuyOrderOnlyForExactShareBuys(t *testing.T) {
	if !shouldCancelResidualBuyOrder(directMarketOrderSignalRequest{
		Side:        api.SideBuy,
		Size:        1.02,
		ExactShares: true,
	}, 0.75) {
		t.Fatal("expected residual exact-share buy to be cancelled")
	}
	if shouldCancelResidualBuyOrder(directMarketOrderSignalRequest{
		Side: api.SideBuy,
		Size: 1.02,
	}, 0.75) {
		t.Fatal("expected non-exact-share buy to skip residual cancellation")
	}
	if shouldCancelResidualBuyOrder(directMarketOrderSignalRequest{
		Side:        api.SideSell,
		Size:        1.02,
		ExactShares: true,
	}, 0.75) {
		t.Fatal("expected sells to skip residual cancellation")
	}
}
