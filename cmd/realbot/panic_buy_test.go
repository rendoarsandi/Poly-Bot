package main

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

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

func TestRealbotHandlePanicBuyStrategySyncsHiddenSharesBeforeInventoryCheck(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	tui.InitSettings(paper.TUISettings{
		PaperArbMode:                  paperArbModeLaddered,
		LadderedTakerReentryMoveCents: 5.0,
		LadderedTakerPnLGuardMode:     core.LadderedTakerPnLGuardMaxProfit,
		LadderedTakerMaxProfitPnL:     1.0,
		MinAskPrice:                   0.01,
		MaxAskPrice:                   0.99,
		LadderedTakerSizeUSDC:         2.0,
	}, nil)
	riskMgr := paper.NewRiskManager(paper.DefaultRiskConfig(), engine, paper.NewOrderBook(), []string{"Down", "Up"})

	engine.UpdateMarketBidAsk("BTC", "Up", 0.89, 0.90)
	if _, err := engine.BuyForMarket("BTC", "Up", 0.23, 1.0); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}
	trader := trading.NewEmbeddedPaperRealTrader(&core.Config{ExecutionBackend: core.ExecutionBackendPaper}, engine)
	trader.RegisterPaperToken("down-token", "BTC", "Down")
	trader.RegisterPaperToken("up-token", "BTC", "Up")

	// Wipe the local engine so only the trader's authoritative snapshot still knows about the share.
	engine.SyncExternalPosition("BTC", "Up", 0, 0.23)
	if engine.GetPositions()["BTC:Up"].Quantity != 0 {
		t.Fatalf("expected engine to be wiped to 0 shares")
	}

	entryExecutionInFlight := false
	panicBuyCooldown := time.Time{}
	lastPairUpdate := time.Now()
	lastTrade := time.Time{}
	lastDustRecoveryNotice := time.Time{}
	ladderedEntries := []realbotLadderedEntry{{seq: 1, ask0: 0.45, ask1: 0.46, side: 1, rung: 1}}
	nextLadderedEntrySeq := uint64(1)

	handled := realbotHandleLadderedStrategy(realbotPanicBuyStrategyArgs{
		ctx:            context.Background(),
		marketID:       "BTC",
		market:         &api.Market{Tokens: []api.Token{{TokenID: "down-token", Outcome: "Down"}, {TokenID: "up-token", Outcome: "Up"}}},
		outcomes:       []string{"Down", "Up"},
		tokenToOutcome: map[string]string{"down-token": "Down", "up-token": "Up"},
		tokenBids:      map[string]float64{"Down": 0.38, "Up": 0.69},
		tokenAsks:      map[string]float64{"Down": 0.39, "Up": 0.70},
		tui:            tui,
		engine:         engine,
		riskMgr:        riskMgr,
		trader:         trader,
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
		t.Fatal("expected strategy to handle the quotes")
	}
	if entryExecutionInFlight {
		t.Fatal("expected synced inventory to trip the max-profit guard before async execution starts")
	}
	if panicBuyCooldown.IsZero() {
		t.Fatal("expected blocked ladder re-entry to apply a cooldown")
	}
	if got := engine.GetPositions()["BTC:Up"].Quantity; math.Abs(got-1.0) > 0.000001 {
		t.Fatalf("expected pre-trade snapshot sync to discover and restore the hidden 1 share, got %.2f", got)
	}
	if got := engine.GetPositions()["BTC:Up"].AvgPrice; math.Abs(got-0.23) > 0.000001 {
		t.Fatalf("expected restored hidden share to keep its 0.23 cost basis instead of current mark, got %.4f", got)
	}
}
