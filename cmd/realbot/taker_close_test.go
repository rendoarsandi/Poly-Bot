package main

import (
	"context"
	"math"
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
)

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

func TestRealbotBestTakerCloseOutcomePriceUsesExecutableAsk(t *testing.T) {
	outcome, price := realbotBestTakerCloseOutcomePrice(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0.92, "Up": 0.89},
		map[string]float64{"Down": 0.93, "Up": 0.91},
	)
	if outcome != "Down" || math.Abs(price-0.93) > 0.000001 {
		t.Fatalf("expected taker-close trigger to use highest executable ask, got %s %.3f", outcome, price)
	}
}

func TestRealbotBestTakerCloseOutcomePriceIgnoresBidWithoutAsk(t *testing.T) {
	outcome, price := realbotBestTakerCloseOutcomePrice(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0.95, "Up": 0.89},
		map[string]float64{"Up": 0.91},
	)
	if outcome != "Up" || math.Abs(price-0.91) > 0.000001 {
		t.Fatalf("expected taker-close trigger to ignore non-executable bid-only side, got %s %.3f", outcome, price)
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

func TestBuildRealbotTakerClosePlanSizesUSDCByConfirmedPrice(t *testing.T) {
	plan, err := buildRealbotTakerClosePlan(50, 0.60, paper.TUISettings{
		BuyExecutionMarginFloorPercent: -1.0,
		TakerCloseMarketSlippage:       0.99,
		TakerCloseMarketMinPrice:       0.60,
	})
	if err != nil {
		t.Fatalf("expected plan, got %v", err)
	}
	if math.Abs(plan.SizingPrice-0.60) > 0.000001 {
		t.Fatalf("expected sizing price 0.60, got %.6f", plan.SizingPrice)
	}
	if math.Abs(plan.RequestedQty-83.3333) > 0.000001 {
		t.Fatalf("expected 83.3333 shares, got %.4f", plan.RequestedQty)
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

func TestNormalizedRealbotTakerCloseLimitPriceFloorsToMinPrice(t *testing.T) {
	minPrice := normalizedRealbotTakerCloseMinPrice(paper.TUISettings{TakerCloseMarketMinPrice: 0.90})
	got := normalizedRealbotTakerCloseLimitPrice(paper.TUISettings{
		TakerCloseMarketSlippage: 0.80,
	}, minPrice)
	if math.Abs(got-0.90) > 0.000001 {
		t.Fatalf("expected taker-close max cap to floor to min price 0.90, got %.3f", got)
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

func TestBuildRealbotTakerClosePlan_UsesConfirmedPriceForSizing(t *testing.T) {
	liveCfg := paper.TUISettings{
		TakerCloseMarketSlippage: 0.99,
		TakerCloseMarketMinPrice: 0.60,
	}

	plan, err := buildRealbotTakerClosePlan(5.0, 0.67, liveCfg)
	if err != nil {
		t.Fatalf("buildRealbotTakerClosePlan returned error: %v", err)
	}

	if math.Abs(plan.RequestedQty-7.4626) > 0.000001 {
		t.Fatalf("expected 7.4626 shares at $0.67 confirmed price for a $5 budget, got %.4f", plan.RequestedQty)
	}

	if got := plan.RequestedQty * plan.SizingPrice; math.Abs(got-5.0) > 0.0001 {
		t.Fatalf("expected confirmed-price notional to use the budget, got $%.4f", got)
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
	if math.Abs(plan.RequestedQty-1.4925) > 0.000001 {
		t.Fatalf("expected 1.4925 shares, got %.4f", plan.RequestedQty)
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
	if math.Abs(plan.RequestedQty-2.0543) > 0.000001 {
		t.Fatalf("expected 2.0543 shares, got %.4f", plan.RequestedQty)
	}
}

func TestBuildRealbotTakerClosePlan_UsesConfiguredSharesMode(t *testing.T) {
	liveCfg := paper.TUISettings{
		TakerCloseMarketSlippage: 0.99,
		TakerCloseMarketMinPrice: 0.60,
		TakerCloseSizingMode:     core.TakerCloseSizingModeShares,
		TakerCloseSizeShares:     12.34,
	}

	plan, err := buildRealbotTakerClosePlan(0, 0.72, liveCfg)
	if err != nil {
		t.Fatalf("expected share-sized close plan, got %v", err)
	}
	if math.Abs(plan.RequestedQty-12.34) > 0.000001 {
		t.Fatalf("expected configured 12.34 shares, got %.4f", plan.RequestedQty)
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
