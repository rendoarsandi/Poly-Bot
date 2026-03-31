package main

import (
	"context"
	"errors"
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

func TestNormalizePaperArbModeSupportsBinanceGap(t *testing.T) {
	if got := normalizePaperArbMode("binance-gap"); got != paperArbModeBinanceGap {
		t.Fatalf("normalizePaperArbMode(binance-gap) = %q, want %q", got, paperArbModeBinanceGap)
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

func TestRealbotShouldRunNearExpiryCleanupSkipsTakerCloseMode(t *testing.T) {
	if realbotShouldRunNearExpiryCleanup(paper.TUISettings{TakerCloseMarket: true}, 5*time.Second, 30*time.Second) {
		t.Fatal("expected taker-close mode to bypass near-expiry cleanup")
	}
	if !realbotShouldRunNearExpiryCleanup(paper.TUISettings{}, 5*time.Second, 30*time.Second) {
		t.Fatal("expected normal mode to allow near-expiry cleanup inside merge buffer")
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

func TestRealbotCopytradeFreshTradesIgnoresPreStartHistoryThenDedupes(t *testing.T) {
	state := newRealbotCopytradeState()
	state.startedAt = time.Unix(1500, 0)
	initial := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 5.51, Timestamp: 1000, TransactionHash: "0x1"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "SELL", Size: 5.51, Timestamp: 2000, TransactionHash: "0x2"},
	}
	if got := realbotCopytradeFreshTrades(state, initial, "cond-1", ""); len(got) != 1 {
		t.Fatalf("expected initial snapshot to ignore pre-start history and keep one post-start signal, got %d", len(got))
	}
	if got := realbotCopytradeFreshTrades(state, initial, "cond-1", ""); len(got) != 0 {
		t.Fatalf("expected repeated trade snapshot to stay deduped, got %d", len(got))
	}

	next := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 5.51, Timestamp: 1000, TransactionHash: "0x1"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "SELL", Size: 5.51, Timestamp: 2000, TransactionHash: "0x2"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 1.25, Timestamp: 3000, TransactionHash: "0x3"},
	}
	got := realbotCopytradeFreshTrades(state, next, "cond-1", "")
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

	got := realbotCopytradeFreshTrades(state, trades, "cond-1", "")
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

func TestRealbotCopytradeFreshTradesKeepsDistinctMempoolSignalsSameTx(t *testing.T) {
	state := newRealbotCopytradeState()
	state.startedAt = time.Unix(1000, 0)
	trades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Timestamp: 1001, TransactionHash: "0xtx", Source: "mempool", SignalID: "0xtx:1"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Timestamp: 1001, TransactionHash: "0xtx", Source: "mempool", SignalID: "0xtx:2"},
	}

	got := realbotCopytradeFreshTrades(state, trades, "cond-1", "")
	if len(got) != 2 {
		t.Fatalf("expected two distinct mempool signals, got %d", len(got))
	}
}

func TestRealbotCopytradeFreshTradesKeepsDistinctPublicTradesSameTx(t *testing.T) {
	state := newRealbotCopytradeState()
	state.startedAt = time.Unix(1000, 0)
	trades := []api.PublicTrade{
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.44, Asset: "asset-a", Timestamp: 1001, TransactionHash: "0xtx"},
		{ConditionID: "cond-1", Outcome: "Up", Side: "BUY", Size: 2, Price: 0.45, Asset: "asset-b", Timestamp: 1001, TransactionHash: "0xtx"},
	}

	got := realbotCopytradeFreshTrades(state, trades, "cond-1", "")
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

	got := realbotCopytradeFreshTrades(state, trades, "cond-1", "")
	if len(got) != 2 {
		t.Fatalf("expected two identical fills from same tx to stay distinct, got %d", len(got))
	}
	if again := realbotCopytradeFreshTrades(state, trades, "cond-1", ""); len(again) != 0 {
		t.Fatalf("expected repeated identical snapshot to stay deduped, got %d", len(again))
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
	if got := realbotUIInterval(paper.TUISettings{PaperArbMode: "copytrade", CopytradePollIntervalMs: 250}); got != 500*time.Millisecond {
		t.Fatalf("expected copytrade UI interval 500ms, got %s", got)
	}
	if got := realbotUIInterval(paper.TUISettings{PaperArbMode: "maker"}); got != realbotUIRefreshInterval {
		t.Fatalf("expected default UI interval %s, got %s", realbotUIRefreshInterval, got)
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
	state := map[string]realbotQuoteState{
		"Down": {UpdatedAt: now.Add(-40 * time.Millisecond), Source: "ws"},
		"Up":   {UpdatedAt: now.Add(-70 * time.Millisecond), Source: "rest"},
	}

	fresh, age, reason := realbotCanUseLocalBuyQuote(now, outcomes, bids, asks, depth, state, 250*time.Millisecond)
	if !fresh || reason != "" {
		t.Fatalf("expected fresh local quote, got fresh=%v age=%v reason=%q", fresh, age, reason)
	}

	state["Up"] = realbotQuoteState{UpdatedAt: now.Add(-400 * time.Millisecond), Source: "ws"}
	fresh, _, reason = realbotCanUseLocalBuyQuote(now, outcomes, bids, asks, depth, state, 250*time.Millisecond)
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
	tokenBids := map[string]float64{"Down": 0.30, "Up": 0.60}
	tokenAsks := map[string]float64{"Down": 0.31, "Up": 0.61}
	tokenFullBids := map[string][]paper.MarketLevel{}
	tokenFullAsks := map[string][]paper.MarketLevel{}
	quoteState := map[string]realbotQuoteState{
		"Down": {UpdatedAt: time.Now().Add(-10 * time.Second), Source: "ws"},
		"Up":   {UpdatedAt: time.Now().Add(-10 * time.Second), Source: "ws"},
	}

	source, _, detail, err := realbotEnsureFreshBuyExecutionQuote(context.Background(), client, market, outcomes, tokenBids, tokenAsks, tokenFullBids, tokenFullAsks, quoteState, 250*time.Millisecond)
	if err != nil {
		t.Fatalf("expected REST refresh to succeed, got %v", err)
	}
	if source != "rest" {
		t.Fatalf("expected REST source, got %q", source)
	}
	if !strings.Contains(detail, "local ask depth") {
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
	}, bids, asks, map[string]float64{}, map[string]float64{}, fullBids, fullAsks, quoteState, nil, engine, client, tui, false)
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
	}, bids, asks, displayBids, displayAsks, fullBids, fullAsks, quoteState, nil, engine, client, tui, false)
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
	state := map[string]realbotQuoteState{
		"Down": {UpdatedAt: now.Add(-40 * time.Millisecond), Source: "ws"},
		"Up":   {UpdatedAt: now.Add(-70 * time.Millisecond), Source: "rest"},
	}

	fresh, age, reason := realbotCanUseLocalSellQuote(now, outcomes, bids, asks, depth, state, 250*time.Millisecond)
	if !fresh || reason != "" || age != 70*time.Millisecond {
		t.Fatalf("expected fresh local sell quote, got fresh=%v age=%v reason=%q", fresh, age, reason)
	}

	state["Up"] = realbotQuoteState{UpdatedAt: now.Add(-400 * time.Millisecond), Source: "ws"}
	fresh, _, reason = realbotCanUseLocalSellQuote(now, outcomes, bids, asks, depth, state, 250*time.Millisecond)
	if fresh || reason == "" {
		t.Fatalf("expected stale sell quote rejection, got fresh=%v reason=%q", fresh, reason)
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
