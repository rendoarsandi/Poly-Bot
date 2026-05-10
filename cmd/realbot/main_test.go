package main

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

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

func TestRealbotResolveFeeRateBpsPreservesZeroRate(t *testing.T) {
	cfg := &core.Config{}
	rates := map[string]int{"Up": 0}

	if got := realbotResolveFeeRateBps(rates, "Up", cfg); got != 0 {
		t.Fatalf("expected explicit zero fee rate to be preserved, got %d", got)
	}
}

func TestRealbotResolveFeeRateBpsFallsBackToDefaultWhenMissing(t *testing.T) {
	cfg := &core.Config{}
	rates := map[string]int{"Down": 0}

	if got := realbotResolveFeeRateBps(rates, "Up", cfg); got != 3 {
		t.Fatalf("expected missing fee rate to fall back to default, got %d", got)
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

func TestRealbotLargeGapRequiresFreshStepAfterAnchorReset(t *testing.T) {
	// Configured at a $0.50 base with a 2c step: an ask of 0.85 sits at rung 17.
	entries := []realbotLadderedEntry{{seq: 1, ask0: 0.50, ask1: 0.85}}
	entries = append(entries, realbotPendingLadderedEntry(entries, 2, 0.55, 0.85, 0.50, 2.0))

	if side, mult, ok := ladderedTakerDirectionalSide(entries, 0.55, 0.859, 0.50, 2.0); ok {
		t.Fatalf("expected move below the next full 2c step to stay blocked, got side=%d mult=%d ok=%v", side, mult, ok)
	}
	if side, mult, ok := ladderedTakerDirectionalSide(entries, 0.55, 0.86, 0.50, 2.0); !ok || side != 1 || mult != 1 {
		t.Fatalf("expected the next fresh 2c move to allow exactly one new re-entry, got side=%d mult=%d ok=%v", side, mult, ok)
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
		tradingGateClosedLogged:  &weekdayLogged,
		manualTradingPauseLogged: &manualLogged,
		nextNearCloseCleanup:     &nextNearCloseCleanup,
		nearExpiryNoticeSent:     &nearExpiryNoticeSent,
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

func TestRealbotNewEntryBlockReasonBlocksForGroupedInventory(t *testing.T) {
	engine := paper.NewEngine(100.0)
	if _, err := engine.BuyForMarket("BTC-older", "Down", 0.18, 22.1457); err != nil {
		t.Fatalf("seed down buy failed: %v", err)
	}
	if _, err := engine.BuyForMarket("BTC-older", "Up", 0.89, 2.2405); err != nil {
		t.Fatalf("seed up buy failed: %v", err)
	}

	reason, blocked := realbotNewEntryBlockReason(nil, "BTC-new", engine, nil, paper.TUISettings{
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

	reason, blocked := realbotNewEntryBlockReason(nil, "BTC-new", engine, nil, paper.TUISettings{
		BlockNewEntriesOnPendingRedemption: false,
	})
	if blocked || reason != "" {
		t.Fatalf("expected setting OFF to allow entries, got blocked=%v reason=%q", blocked, reason)
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

func TestNormalizeExecutionToleranceFractionSupportsLegacyPercentAndDecimalForms(t *testing.T) {
	if got := normalizeExecutionToleranceFraction(-1.0); math.Abs(got-0.01) > 0.000001 {
		t.Fatalf("expected -1.0 to normalize to 1%%, got %.6f", got)
	}
	if got := normalizeExecutionToleranceFraction(-0.01); math.Abs(got-0.01) > 0.000001 {
		t.Fatalf("expected -0.01 to normalize to 1%%, got %.6f", got)
	}
}

func TestRealbotClampBuySharesToBudgetKeepsMarketLikeFractionalShares(t *testing.T) {
	shares := realbotClampBuySharesToBudget(3.3131, 3.28, 0.50, 0.49)
	if math.Abs(shares-3.3061) > 0.000001 {
		t.Fatalf("expected clamp to 3.3061 shares, got %.4f", shares)
	}
}

func TestRealbotClampSingleBuySharesToBudgetUsesVenueCompatibleShareStep(t *testing.T) {
	// Strict venue-compatible step case (budget $2.10 -> 2.0 shares = $1.98 cost, >= $1.00)
	shares := realbotClampSingleBuySharesToBudget(3.1153, 2.10, 0.99)
	if math.Abs(shares-2.0) > 0.000001 {
		t.Fatalf("expected 0.99-capped buy with $2.10 budget to floor to 2 whole shares, got %.4f", shares)
	}

	// Fallback case (budget $1.10 -> strict step 1.0 shares = $0.99 cost < $1.00 minimum, so it falls back to 1.1111 shares = $1.10 cost)
	fallbackShares := realbotClampSingleBuySharesToBudget(2.1153, 1.10, 0.99)
	if math.Abs(fallbackShares-1.1111) > 0.000001 {
		t.Fatalf("expected 0.99-capped buy with $1.10 budget to fallback to 1.1111 shares to satisfy $1.00 minimum, got %.4f", fallbackShares)
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

	adjusted, missing := realbotSyncEngineToWalletTruthForResolution(engine, "BTC", "Up", []paper.WalletTruthPosition{
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

func TestRealbotLooksLikeTerminalBookRecognizesOneSidedHighAskEndState(t *testing.T) {
	terminal := realbotLooksLikeTerminalBook(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0, "Up": 0},
		map[string]float64{"Down": 1.00, "Up": 0.01},
	)
	if !terminal {
		t.Fatal("expected high-ask/low-ask terminal book to bypass stale WS recovery")
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

func TestRealbotResolutionSyncClearsAlreadySettledLosingLocalShares(t *testing.T) {
	engine := paper.NewEngine(100)
	if _, err := engine.BuyForMarket("BTC", "Down", 0.80, 10); err != nil {
		t.Fatalf("seed down buy failed: %v", err)
	}
	engine.RecordSettledLoser("BTC", "Down", 10)

	adjusted, missing := realbotSyncEngineToWalletTruthForResolution(engine, "BTC", "Up", []paper.WalletTruthPosition{
		{MarketID: "BTC", Outcome: "Down", LocalShares: 10, OnChainShares: 0},
	})
	if adjusted != 1 {
		t.Fatalf("expected settled losing lot to be cleared locally, got %d adjustments", adjusted)
	}
	if len(missing) != 0 {
		t.Fatalf("expected no missing cost basis, got %v", missing)
	}
	if pos := engine.GetPositions()["BTC:Down"]; pos.Quantity != 0 {
		t.Fatalf("expected settled losing local position cleared, got %+v", pos)
	}
}

func TestRealbotResolutionSyncKeepsZeroOnChainWinningLocalShares(t *testing.T) {
	engine := paper.NewEngine(100)
	if _, err := engine.BuyForMarket("BTC", "Up", 0.81, 1.00458); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	adjusted, missing := realbotSyncEngineToWalletTruthForResolution(engine, "BTC", "Up", []paper.WalletTruthPosition{
		{MarketID: "BTC", Outcome: "Up", LocalShares: 1.00458, OnChainShares: 0},
	})
	if adjusted != 0 {
		t.Fatalf("expected winning local shares to be preserved without wallet adjustment, got %d adjustments", adjusted)
	}
	if len(missing) != 0 {
		t.Fatalf("expected no missing cost basis, got %v", missing)
	}
	if got := engine.GetPositions()["BTC:Up"].Quantity; math.Abs(got-1.00458) > 0.000001 {
		t.Fatalf("expected local winning shares preserved before redemption, got %.5f", got)
	}

	result := engine.RedeemWithDetails("BTC", "Up")
	expectedCost := 0.81 * 1.00458
	expectedPnL := 1.00458 - expectedCost
	if math.Abs(result.WinningShares-1.00458) > 0.000001 {
		t.Fatalf("expected winning shares 1.00458, got %.5f", result.WinningShares)
	}
	if math.Abs(result.WinningCost-expectedCost) > 0.000001 {
		t.Fatalf("expected winning cost %.5f, got %.5f", expectedCost, result.WinningCost)
	}
	if math.Abs(result.TotalPnL-expectedPnL) > 0.000001 {
		t.Fatalf("expected winning PnL %.5f, got %.5f", expectedPnL, result.TotalPnL)
	}
	if got := engine.GetStats().RealizedPnL; math.Abs(got-expectedPnL) > 0.000001 {
		t.Fatalf("expected realized PnL %.5f, got %.5f", expectedPnL, got)
	}
	if got := engine.GetPendingRedemptions()["BTC"]; math.Abs(got-1.00458) > 0.000001 {
		t.Fatalf("expected pending payout 1.00458, got %.5f", got)
	}
}

func TestRealbotResolutionSyncRecognizesUncostedWinningSharesNeutrally(t *testing.T) {
	engine := paper.NewEngine(100)

	adjusted, missing := realbotSyncEngineToWalletTruthForResolution(engine, "BTC", "Up", []paper.WalletTruthPosition{
		{MarketID: "BTC", Outcome: "Up", LocalShares: 0, OnChainShares: 5},
	})
	if adjusted == 0 {
		t.Fatal("expected uncosted winning shares to be synced as redeemable")
	}
	if len(missing) != 1 || missing[0] != "Up" {
		t.Fatalf("expected missing Up cost basis marker, got %v", missing)
	}

	result := engine.RedeemWithDetails("BTC", "Up")
	if math.Abs(result.WinningPayout-5.0) > 0.000001 {
		t.Fatalf("expected winning payout 5.00, got %.2f", result.WinningPayout)
	}
	if math.Abs(result.TotalPnL) > 0.000001 {
		t.Fatalf("expected uncosted winning shares to be PnL neutral, got %.2f", result.TotalPnL)
	}
	if got := engine.GetPendingRedemptions()["BTC"]; math.Abs(got-5.0) > 0.000001 {
		t.Fatalf("expected pending redemption 5.00, got %.2f", got)
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

func TestRealbotMatchedBidLiquidityHonorsExecutionSum(t *testing.T) {
	bids0 := []paper.MarketLevel{{Price: 0.54, Size: 3}, {Price: 0.52, Size: 2}}
	bids1 := []paper.MarketLevel{{Price: 0.49, Size: 2}, {Price: 0.46, Size: 4}}

	liq := realbotMatchedBidLiquidity(bids0, bids1, 1.01)
	if math.Abs(liq-2.0) > 0.000001 {
		t.Fatalf("matched bid liquidity got %.2f want 2.00", liq)
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

func TestReportedSellProceedsUsesDirectNotionalWhenAcknowledgedSizeDrifts(t *testing.T) {
	exec := directMarketExecution{AcknowledgedQty: 3.14, AcknowledgedNotional: 1.884}
	got := reportedSellProceeds(exec, 0.58, 3.00, 3.14)
	expected := 1.8
	if math.Abs(got-expected) > 0.000001 {
		t.Fatalf("expected attributed proceeds %.6f, got %.6f", expected, got)
	}
}

func TestRealbotMirrorLiveFillUsesObservedCostAndProceeds(t *testing.T) {
	engine := paper.NewEngine(100)
	if _, err := realbotMirrorLiveBuyIntoEngine(engine, "BTC", "Up", 10.50, 10); err != nil {
		t.Fatalf("live buy mirror failed: %v", err)
	}
	if _, err := realbotMirrorLiveSellIntoEngine(engine, "BTC", "Up", 7.25, 10); err != nil {
		t.Fatalf("live sell mirror failed: %v", err)
	}
	if got := engine.GetStats().RealizedPnL; math.Abs(got+3.25) > 0.000001 {
		t.Fatalf("expected realized PnL to include observed fee/cost delta -3.25, got %.6f", got)
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

func TestDirectSubmittedOrderValueUsesEncodedBuyAmount(t *testing.T) {
	req := directMarketOrderSignalRequest{
		Side:        api.SideBuy,
		Price:       0.60,
		Size:        1.8333,
		ExactShares: true,
	}
	if got := directSubmittedOrderValue(req); math.Abs(got-1.09998) > 0.000001 {
		t.Fatalf("expected encoded buy amount 1.09998, got %.5f", got)
	}
	if !hasActionableSubmittedDirectOrderValue(req) {
		t.Fatal("expected encoded buy amount above $1 to be actionable")
	}
}

func TestVenueRequiredFeeRateBpsParsesMismatchMessage(t *testing.T) {
	got, ok := venueRequiredFeeRateBps("invalid fee rate (312), current market's taker fee: 1000")
	if !ok {
		t.Fatal("expected fee mismatch parser to succeed")
	}
	if got != 1000 {
		t.Fatalf("expected required fee rate 1000, got %d", got)
	}
}

func TestVenueRequiredFeeRateBpsRejectsOtherMessages(t *testing.T) {
	if _, ok := venueRequiredFeeRateBps("invalid amount for a marketable BUY order ($0.88), min size: $1"); ok {
		t.Fatal("expected non-fee rejection to be ignored")
	}
}

func TestExecuteMarketOrderWithSignalsRejectsBuyBelowVenueMinimumBeforeSubmission(t *testing.T) {
	exec := executeMarketOrderWithSignals(context.Background(), nil, api.SideBuy, "token-up", "Up", 0.90, 1.10, 0, 0, time.Second)
	if exec.Success {
		t.Fatal("expected sub-$1 encoded buy to fail locally")
	}
	if exec.Result == nil || !strings.Contains(exec.Result.Message, "below Polymarket $1 minimum") {
		t.Fatalf("expected local min-size failure message, got %+v", exec.Result)
	}
}

func TestExecuteMarketOrderBatchWithSignalsRejectsBuyBelowVenueMinimumBeforeSubmission(t *testing.T) {
	execs := executeMarketOrderBatchWithSignals(context.Background(), nil, []directMarketOrderSignalRequest{{
		Side:        api.SideBuy,
		TokenID:     "token-up",
		Outcome:     "Up",
		Price:       0.90,
		Size:        1.10,
		ExactShares: true,
	}}, time.Second)
	if len(execs) != 1 {
		t.Fatalf("expected 1 execution result, got %d", len(execs))
	}
	if execs[0].Success {
		t.Fatal("expected batch sub-$1 encoded buy to fail locally")
	}
	if execs[0].Result == nil || !strings.Contains(execs[0].Result.Message, "below Polymarket $1 minimum") {
		t.Fatalf("expected local min-size failure message, got %+v", execs[0].Result)
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
