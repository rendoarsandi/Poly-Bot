package main

import (
	"errors"
	"math"
	"testing"

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

func TestRealbotRedeemCashCorrectionReconcilesRecoveredLiveBalance(t *testing.T) {
	engine := paper.NewEngine(20.86)
	if _, err := engine.BuyForMarket("BTC", "Up", 0.85, 5.03834); err != nil {
		t.Fatalf("seed up buy failed: %v", err)
	}
	if _, err := engine.BuyForMarket("BTC", "Down", 0.48, 1.95923); err != nil {
		t.Fatalf("seed down buy failed: %v", err)
	}

	tui := paper.NewTUI(engine, paper.NewOrderBook())
	tui.RecordRound(20.86, 20.86, 0, 7, engine.GetPositions(), nil)

	result := engine.RedeemWithDetails("BTC", "Down")
	tui.AmendMostRecentRoundForMarket("BTC", result.TotalPnL, []*paper.RedemptionResult{result})
	if result.TotalPnL >= -3.20 {
		t.Fatalf("expected local redemption to initially overstate the loss, got %.4f", result.TotalPnL)
	}
	if got := engine.GetStats().MaxDrawdownCash; math.Abs(got-3.2638) > 0.0001 {
		t.Fatalf("expected pending redemption to stamp economic drawdown, got %.4f", got)
	}

	redeemStartBalance := engine.GetBalance()
	expectedPayout := engine.GetPendingRedemptions()["BTC"]
	applied := 0.0
	correction := realbotApplyRedeemCashCorrection(engine, tui, "BTC", redeemStartBalance, expectedPayout, 20.73, &applied)
	if correction <= 3.0 {
		t.Fatalf("expected live cash correction above 3.00, got %.4f", correction)
	}
	if duplicate := realbotApplyRedeemCashCorrection(engine, tui, "BTC", redeemStartBalance, expectedPayout, 20.73, &applied); duplicate != 0 {
		t.Fatalf("expected repeated same-balance correction to be ignored, got %.4f", duplicate)
	}

	expectedPnL := 20.73 - 20.86
	if got := engine.GetStats().RealizedPnL; math.Abs(got-expectedPnL) > 0.01 {
		t.Fatalf("expected realized pnl to reconcile near wallet net %.4f, got %.4f", expectedPnL, got)
	}
	if got := engine.GetStats().MaxDrawdownCash; math.Abs(got-3.2638) > 0.0001 {
		t.Fatalf("expected max drawdown to preserve observed economic drawdown, got %.4f", got)
	}
	history := tui.GetRoundHistory()
	if len(history) != 1 {
		t.Fatalf("expected one round history entry, got %d", len(history))
	}
	if got := history[0].PnL; math.Abs(got-expectedPnL) > 0.01 {
		t.Fatalf("expected round pnl to reconcile near wallet net %.4f, got %.4f", expectedPnL, got)
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
