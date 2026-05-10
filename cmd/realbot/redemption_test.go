package main

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"Market-bot/internal/core"
	"Market-bot/internal/paper"
)

func TestRealbotNewEntryBlockReasonBlocksForPendingRedemptionPayout(t *testing.T) {
	engine := paper.NewEngine(100.0)
	engine.SetPendingRedemption("BTC-older", 12.0)

	reason, blocked := realbotNewEntryBlockReason(nil, "BTC-new", engine, nil, paper.TUISettings{
		BlockNewEntriesOnPendingRedemption: true,
	})
	if !blocked || !strings.Contains(reason, "BTC-older") {
		t.Fatalf("expected pending-redemption block, got blocked=%v reason=%q", blocked, reason)
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

	reason, blocked := realbotEntryBlockReason(nil, currentMarketID, engine, nil, paper.TUISettings{
		PaperArbMode:                       "laddered-taker",
		BlockNewEntriesOnPendingRedemption: true,
	})
	if !blocked || !strings.Contains(reason, "fresh next market") {
		t.Fatalf("expected late redemption to block current ladder market, got blocked=%v reason=%q", blocked, reason)
	}
}

func TestRealbotLateRedeemAllowsImmediateLadderReentryWhenConfigured(t *testing.T) {
	engine := paper.NewEngine(100.0)
	now := time.Now()
	previousMarketID := fmt.Sprintf("btc-updown-5m-%d", now.Add(-7*time.Minute).Unix())
	currentMarketID := fmt.Sprintf("btc-updown-5m-%d", now.Add(-2*time.Minute).Unix())
	engine.SetPendingRedemption(previousMarketID, 5.0)
	_ = engine.SettlePendingRedemption(previousMarketID)

	reason, blocked := realbotEntryBlockReason(nil, currentMarketID, engine, nil, paper.TUISettings{
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

	reason, blocked := realbotEntryBlockReason(nil, currentMarketID, engine, nil, paper.TUISettings{
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

func TestRealbotResolutionSyncPreservesLosingLocalSharesForRedemption(t *testing.T) {
	engine := paper.NewEngine(100)
	if _, err := engine.BuyForMarket("BTC", "Up", 0.30, 10); err != nil {
		t.Fatalf("seed up buy failed: %v", err)
	}
	if _, err := engine.BuyForMarket("BTC", "Down", 0.80, 10); err != nil {
		t.Fatalf("seed down buy failed: %v", err)
	}

	adjusted, missing := realbotSyncEngineToWalletTruthForResolution(engine, "BTC", "Up", []paper.WalletTruthPosition{
		{MarketID: "BTC", Outcome: "Up", LocalShares: 10, OnChainShares: 10},
		{MarketID: "BTC", Outcome: "Down", LocalShares: 10, OnChainShares: 0},
	})
	if adjusted != 0 {
		t.Fatalf("expected losing local lot to stay intact for redemption, got %d adjustments", adjusted)
	}
	if len(missing) != 0 {
		t.Fatalf("expected no missing cost basis, got %v", missing)
	}

	result := engine.RedeemWithDetails("BTC", "Up")
	if math.Abs(result.LosingCost-8.0) > 0.000001 {
		t.Fatalf("expected losing cost 8.00 to be realized at redemption, got %.2f", result.LosingCost)
	}
	if math.Abs(result.TotalPnL+1.0) > 0.000001 {
		t.Fatalf("expected total PnL -1.00 after realizing both sides, got %.2f", result.TotalPnL)
	}
}
