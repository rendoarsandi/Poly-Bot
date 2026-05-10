package main

import (
	"context"
	"math"
	"strings"
	"testing"

	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

func TestRealbotNewEntryBlockReasonBlocksForPriorRoundInventory(t *testing.T) {
	engine := paper.NewEngine(100.0)
	if _, err := engine.BuyForMarket("BTC-older", "Up", 0.50, 5.0); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	reason, blocked := realbotNewEntryBlockReason(nil, "BTC-new", engine, nil, paper.TUISettings{
		BlockNewEntriesOnPendingRedemption: true,
	})
	if !blocked || !strings.Contains(reason, "BTC-older") {
		t.Fatalf("expected prior-round inventory block, got blocked=%v reason=%q", blocked, reason)
	}
	if reason, blocked = realbotNewEntryBlockReason(nil, "BTC-older", engine, nil, paper.TUISettings{
		BlockNewEntriesOnPendingRedemption: true,
	}); blocked || reason != "" {
		t.Fatalf("expected no block on current market, got blocked=%v reason=%q", blocked, reason)
	}
}

func TestRealbotNeutralRoundPnLExcludesWalletTruthReconciliationDelta(t *testing.T) {
	roundPnL := realbotNeutralRoundPnL(64.67, 74.13, 9.46)
	if math.Abs(roundPnL) > 0.000001 {
		t.Fatalf("expected reconciliation delta to stay neutral, got %.4f", roundPnL)
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

func TestRealbotRoundedLimitBuyCostMatchesVenueCentRounding(t *testing.T) {
	cost := realbotRoundedLimitBuyCost(0.501, 1)
	if math.Abs(cost-0.51) > 0.000001 {
		t.Fatalf("expected rounded venue cost 0.51, got %.4f", cost)
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

func TestRealbotRoundSnapshotPnLCountsObservedLiveFillCostAndProceeds(t *testing.T) {
	engine := paper.NewEngine(100)
	snapshot := realbotRoundSnapshot{startingEquity: engine.GetBookEquity()}

	if _, err := realbotMirrorLiveBuyIntoEngine(engine, "BTC", "Up", 10.50, 10); err != nil {
		t.Fatalf("live buy mirror failed: %v", err)
	}
	if _, err := realbotMirrorLiveSellIntoEngine(engine, "BTC", "Up", 7.25, 10); err != nil {
		t.Fatalf("live sell mirror failed: %v", err)
	}

	got := realbotRoundSnapshotPnL(&trading.RealTrader{}, engine, snapshot, engine.GetBookEquity(), 0)
	if math.Abs(got+3.25) > 0.000001 {
		t.Fatalf("expected round snapshot pnl to count observed tx loss -3.25, got %.6f", got)
	}
}
