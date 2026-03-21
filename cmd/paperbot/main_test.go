package main

import (
	"testing"
	"time"

	"Market-bot/internal/core"
	"Market-bot/internal/paper"
)

func TestPaperExecutionLatencyDurations(t *testing.T) {
	base := time.Unix(1700000000, 0)
	latency := paperExecutionLatency{
		detectedAt: base,
		startedAt:  base.Add(2 * time.Millisecond),
		executedAt: base.Add(7 * time.Millisecond),
		settledAt:  base.Add(9 * time.Millisecond),
	}

	if got := latency.detectToStart(); got != 2*time.Millisecond {
		t.Fatalf("detectToStart = %v, want 2ms", got)
	}
	if got := latency.startToExecution(); got != 5*time.Millisecond {
		t.Fatalf("startToExecution = %v, want 5ms", got)
	}
	if got := latency.detectToExecution(); got != 7*time.Millisecond {
		t.Fatalf("detectToExecution = %v, want 7ms", got)
	}
	if got := latency.detectToSettlement(); got != 9*time.Millisecond {
		t.Fatalf("detectToSettlement = %v, want 9ms", got)
	}
}

func TestPaperExecutionLatencyHandlesMissingTimestamps(t *testing.T) {
	var latency paperExecutionLatency
	if got := latency.detectToStart(); got != 0 {
		t.Fatalf("detectToStart with zero timestamps = %v, want 0", got)
	}
	if got := latency.startToExecution(); got != 0 {
		t.Fatalf("startToExecution with zero timestamps = %v, want 0", got)
	}
	if got := latency.detectToExecution(); got != 0 {
		t.Fatalf("detectToExecution with zero timestamps = %v, want 0", got)
	}
	if got := latency.detectToSettlement(); got != 0 {
		t.Fatalf("detectToSettlement with zero timestamps = %v, want 0", got)
	}
}

func TestNormalizePaperArbModeDefaultsToTaker(t *testing.T) {
	if got := normalizePaperArbMode(""); got != paperArbModeTaker {
		t.Fatalf("normalizePaperArbMode(empty) = %q, want %q", got, paperArbModeTaker)
	}
	if got := normalizePaperArbMode("maker"); got != paperArbModeMaker {
		t.Fatalf("normalizePaperArbMode(maker) = %q, want %q", got, paperArbModeMaker)
	}
	if got := normalizePaperArbMode("weird"); got != paperArbModeTaker {
		t.Fatalf("normalizePaperArbMode(weird) = %q, want %q", got, paperArbModeTaker)
	}
}

func TestComputePaperMakerArbPricesStayInsideSpreadAndMarginCap(t *testing.T) {
	price1, price2, ok := computePaperMakerArbPrices(0.43, 0.46, 0.45, 0.49, 0.97)
	if !ok {
		t.Fatal("expected maker arb prices to be quotable")
	}
	if price1 <= 0.43 || price1 >= 0.46 {
		t.Fatalf("price1 = %.3f, want inside spread (0.43, 0.46)", price1)
	}
	if price2 <= 0.45 || price2 >= 0.49 {
		t.Fatalf("price2 = %.3f, want inside spread (0.45, 0.49)", price2)
	}
	if price1+price2 > 0.97+1e-9 {
		t.Fatalf("pair sum = %.3f, want <= 0.970", price1+price2)
	}
}

func TestComputePaperMakerArbPricesRejectWhenNoMakerRoom(t *testing.T) {
	if _, _, ok := computePaperMakerArbPrices(0.489, 0.490, 0.489, 0.490, 0.97); ok {
		t.Fatal("expected maker arb pricing to fail when there is no room inside the spread")
	}
	if _, _, ok := computePaperMakerArbPrices(0.44, 0.46, 0.44, 0.46, 0.85); ok {
		t.Fatal("expected maker arb pricing to fail when margin cap is too strict")
	}
}

func TestComputePaperMakerSplitSellPricesStayInsideSpreadAndMarginFloor(t *testing.T) {
	price1, price2, ok := computePaperMakerSplitSellPrices(0.49, 0.53, 0.50, 0.55, 1.03)
	if !ok {
		t.Fatal("expected split-backed maker sell prices to be quotable")
	}
	if price1 <= 0.49 || price1 >= 0.53 {
		t.Fatalf("price1 = %.3f, want inside spread (0.49, 0.53)", price1)
	}
	if price2 <= 0.50 || price2 >= 0.55 {
		t.Fatalf("price2 = %.3f, want inside spread (0.50, 0.55)", price2)
	}
	if price1+price2 < 1.03-1e-9 {
		t.Fatalf("pair quote sum = %.3f, want at least 1.030", price1+price2)
	}
	if price1+price2 > (0.53-paperMakerQuoteStep)+(0.55-paperMakerQuoteStep)+1e-9 {
		t.Fatalf("pair quote sum = %.3f exceeded available maker room", price1+price2)
	}
}

func TestComputePaperMakerSplitSellPricesRejectWhenNoProfitableMakerRoom(t *testing.T) {
	if _, _, ok := computePaperMakerSplitSellPrices(0.44, 0.46, 0.44, 0.46, 0.92); ok {
		t.Fatal("expected split-backed maker pricing to fail when there is no room inside the spread")
	}
	if _, _, ok := computePaperMakerSplitSellPrices(0.43, 0.46, 0.45, 0.49, 1.02); ok {
		t.Fatal("expected split-backed maker pricing to fail when margin floor is too high")
	}
}

func TestComputePaperMakerInventorySkewClampsToRange(t *testing.T) {
	if got := computePaperMakerInventorySkew(40, 0, 10); got != 1.0 {
		t.Fatalf("computePaperMakerInventorySkew long-heavy = %.2f, want 1.00", got)
	}
	if got := computePaperMakerInventorySkew(0, 40, 10); got != -1.0 {
		t.Fatalf("computePaperMakerInventorySkew short-heavy = %.2f, want -1.00", got)
	}
	if got := computePaperMakerInventorySkew(12, 8, 20); got != 0.2 {
		t.Fatalf("computePaperMakerInventorySkew balanced-ish = %.2f, want 0.20", got)
	}
}

func TestComputePaperMakerSkewedQuoteChangesAggressiveness(t *testing.T) {
	buyLong, ok := computePaperMakerSkewedQuote("buy", 0.47, 0.53, 1.0, 0.008, paperMakerStrategyParams)
	if !ok {
		t.Fatal("expected buy quote for long-heavy inventory")
	}
	buyShort, ok := computePaperMakerSkewedQuote("buy", 0.47, 0.53, -1.0, 0.008, paperMakerStrategyParams)
	if !ok {
		t.Fatal("expected buy quote for short-heavy inventory")
	}
	if buyLong >= buyShort {
		t.Fatalf("buy quote should be less aggressive when inventory is heavy: long=%.3f short=%.3f", buyLong, buyShort)
	}

	sellLong, ok := computePaperMakerSkewedQuote("sell", 0.47, 0.53, 1.0, 0.008, paperMakerStrategyParams)
	if !ok {
		t.Fatal("expected sell quote for long-heavy inventory")
	}
	sellShort, ok := computePaperMakerSkewedQuote("sell", 0.47, 0.53, -1.0, 0.008, paperMakerStrategyParams)
	if !ok {
		t.Fatal("expected sell quote for short-heavy inventory")
	}
	if sellLong >= sellShort {
		t.Fatalf("sell quote should be more aggressive when inventory is heavy: long=%.3f short=%.3f", sellLong, sellShort)
	}
}

func TestShouldPaperReconnectWSOnlyForInvalidStaleBooks(t *testing.T) {
	outcomes := []string{"Down", "Up"}
	validBids := map[string]float64{"Down": 0.48, "Up": 0.49}
	validAsks := map[string]float64{"Down": 0.50, "Up": 0.51}

	if shouldPaperReconnectWS(outcomes, validBids, validAsks, 30*time.Second, 15*time.Second, false) {
		t.Fatal("expected quiet but valid WS book to stay connected")
	}

	invalidAsks := map[string]float64{"Down": 0.50, "Up": 0}
	if !shouldPaperReconnectWS(outcomes, validBids, invalidAsks, 30*time.Second, 15*time.Second, false) {
		t.Fatal("expected missing side on a stale local book to trigger reconnect")
	}

	if shouldPaperReconnectWS(outcomes, validBids, invalidAsks, 30*time.Second, 15*time.Second, true) {
		t.Fatal("expected terminal-looking book to suppress reconnects")
	}
}

func TestPaperLooksLikeTerminalBookRecognizesRoundedPinnedEndState(t *testing.T) {
	terminal := paperLooksLikeTerminalBook(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0.989, "Up": 0},
		map[string]float64{"Down": 0, "Up": 0.011},
	)
	if !terminal {
		t.Fatal("expected rounded 0.99/0.01 terminal book to count as terminal-looking")
	}
}

func TestPaperLooksLikeTerminalBookRejectsOrdinaryOneSidedBook(t *testing.T) {
	terminal := paperLooksLikeTerminalBook(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0.64, "Up": 0},
		map[string]float64{"Down": 0, "Up": 0.36},
	)
	if terminal {
		t.Fatal("expected ordinary one-sided book to require freshness checks")
	}
}

func TestPaperPairQuoteAgeTreatsZeroAsStale(t *testing.T) {
	age := paperPairQuoteAge(time.Time{}, time.Now())
	if age <= 3*time.Second {
		t.Fatalf("expected zero pair update time to be treated as stale, got %v", age)
	}
}

func TestShouldUseLocalPaperPairRequiresFreshValidBothSides(t *testing.T) {
	now := time.Now()
	outcomes := []string{"Yes", "No"}
	bids := map[string]float64{"Yes": 0.41, "No": 0.57}
	asks := map[string]float64{"Yes": 0.43, "No": 0.59}

	if !shouldUseLocalPaperPair(outcomes, bids, asks, now.Add(-500*time.Millisecond), 750*time.Millisecond, now) {
		t.Fatal("expected recent valid pair quotes to be usable")
	}
	if shouldUseLocalPaperPair(outcomes, bids, asks, now.Add(-2*time.Second), 750*time.Millisecond, now) {
		t.Fatal("expected stale pair quotes to be rejected")
	}
	asks["No"] = 0
	if shouldUseLocalPaperPair(outcomes, bids, asks, now.Add(-100*time.Millisecond), 750*time.Millisecond, now) {
		t.Fatal("expected missing quote on one side to invalidate local pair")
	}
}

func TestSummarizePaperRoundUsesSharedEngineDelta(t *testing.T) {
	engine := paper.NewEngine(100.0)
	engine.AddBalance(4.65)
	engine.AddRealizedPnL(4.65)

	roundPnL, totalEquity, roundTrades, stats := summarizePaperRound(engine, 100.0, 0)
	if abs := roundPnL - 4.65; abs < -0.000001 || abs > 0.000001 {
		t.Fatalf("expected round PnL 4.65, got %.2f", roundPnL)
	}
	if abs := totalEquity - 104.65; abs < -0.000001 || abs > 0.000001 {
		t.Fatalf("expected total equity 104.65, got %.2f", totalEquity)
	}
	if roundTrades != stats.TotalTrades {
		t.Fatalf("expected round trades to mirror stats delta, got %d vs %d", roundTrades, stats.TotalTrades)
	}
}

func TestSummarizePaperRoundKeepsOpenInventoryNeutralAtRotation(t *testing.T) {
	engine := paper.NewEngine(100.0)
	if _, err := engine.BuyForMarket("m1", "Up", 0.60, 10.0); err != nil {
		t.Fatalf("buy failed: %v", err)
	}
	engine.UpdateMarketBidAsk("m1", "Up", 0.30, 0.31)

	roundPnL, totalEquity, _, _ := summarizePaperRound(engine, 100.0, 0)
	if roundPnL != 0 {
		t.Fatalf("expected unresolved inventory to stay neutral at rotation, got %.2f", roundPnL)
	}
	if totalEquity != 100.0 {
		t.Fatalf("expected round equity to use book equity 100.00, got %.2f", totalEquity)
	}
}

func TestSummarizePaperRoundCountsLockedPairProfitAtRotation(t *testing.T) {
	engine := paper.NewEngine(100.0)
	if _, err := engine.BuyForMarket("m1", "Up", 0.48, 3.1); err != nil {
		t.Fatalf("buy up failed: %v", err)
	}
	if _, err := engine.BuyForMarket("m1", "Down", 0.49, 3.1); err != nil {
		t.Fatalf("buy down failed: %v", err)
	}

	roundPnL, totalEquity, _, _ := summarizePaperRound(engine, 100.0, 0)
	if abs := roundPnL - 0.093; abs < -0.000001 || abs > 0.000001 {
		t.Fatalf("expected locked round pnl 0.093, got %.6f", roundPnL)
	}
	if abs := totalEquity - 100.093; abs < -0.000001 || abs > 0.000001 {
		t.Fatalf("expected total equity 100.093, got %.6f", totalEquity)
	}
}

func TestPaperTradeSizeTracksResolutionOutcomeAfterCarrySettles(t *testing.T) {
	cfg := &core.Config{TradeScaleFactor: 0.05}

	t.Run("unresolved carry stays neutral", func(t *testing.T) {
		engine := paper.NewEngine(95.0)
		if !engine.SyncExternalPosition("m1", "Up", 10.0, 0.50) {
			t.Fatal("expected imported carry")
		}

		if got := cfg.CalculateTradeSize(engine.GetBookEquity()); got != 5.0 {
			t.Fatalf("expected 5%% trade size to stay $5.00 while unresolved, got %.2f", got)
		}
	})

	t.Run("matched unresolved pair uses locked profit immediately", func(t *testing.T) {
		engine := paper.NewEngine(100.0)
		if _, err := engine.BuyForMarket("m1", "Up", 0.48, 3.1); err != nil {
			t.Fatalf("buy up failed: %v", err)
		}
		if _, err := engine.BuyForMarket("m1", "Down", 0.49, 3.1); err != nil {
			t.Fatalf("buy down failed: %v", err)
		}

		if got := cfg.CalculateTradeSize(engine.GetBookEquity()); got < 5.004 || got > 5.005 {
			t.Fatalf("expected 5%% trade size to include locked pair profit, got %.6f", got)
		}
	})

	t.Run("winning resolution increases next trade size", func(t *testing.T) {
		engine := paper.NewEngine(95.0)
		if !engine.SyncExternalPosition("m1", "Up", 10.0, 0.50) {
			t.Fatal("expected imported carry")
		}

		res := engine.RedeemWithDetails("m1", "Up")
		if res.TotalPnL != 5.0 {
			t.Fatalf("expected total pnl 5.00, got %.2f", res.TotalPnL)
		}
		engine.SetBalance(105.0)
		engine.ClearPendingRedemption("m1")

		if got := cfg.CalculateTradeSize(engine.GetBookEquity()); got != 5.25 {
			t.Fatalf("expected 5%% trade size to become $5.25 after winning resolution, got %.2f", got)
		}
	})

	t.Run("losing resolution reduces next trade size", func(t *testing.T) {
		engine := paper.NewEngine(95.0)
		if !engine.SyncExternalPosition("m2", "Up", 10.0, 0.50) {
			t.Fatal("expected imported carry")
		}

		res := engine.RedeemWithDetails("m2", "Down")
		if res.TotalPnL != -5.0 {
			t.Fatalf("expected total pnl -5.00, got %.2f", res.TotalPnL)
		}

		if got := cfg.CalculateTradeSize(engine.GetBookEquity()); got != 4.75 {
			t.Fatalf("expected 5%% trade size to become $4.75 after losing resolution, got %.2f", got)
		}
	})
}

func TestComputePaperMakerQuoteSizesRespectSkewAndCaps(t *testing.T) {
	buyHeavy := computePaperMakerBuyQty(10, 18, 1.0, 20, 100, 0.49, paperMakerStrategyParams)
	buyLight := computePaperMakerBuyQty(10, 2, -1.0, 20, 100, 0.49, paperMakerStrategyParams)
	if buyHeavy >= buyLight {
		t.Fatalf("expected long-heavy inventory to quote smaller buys: heavy=%.0f light=%.0f", buyHeavy, buyLight)
	}

	sellHeavy := computePaperMakerSellQty(10, 30, 1.0, 0.50, paperMakerStrategyParams)
	sellBalanced := computePaperMakerSellQty(10, 30, 0.0, 0.50, paperMakerStrategyParams)
	if sellHeavy <= sellBalanced {
		t.Fatalf("expected long-heavy inventory to quote larger sells: heavy=%.0f balanced=%.0f", sellHeavy, sellBalanced)
	}
	if capped := computePaperMakerSellQty(10, 4, 1.0, 0.50, paperMakerStrategyParams); capped != 4 {
		t.Fatalf("expected sell qty to cap at available inventory, got %.0f want 4", capped)
	}
}

func TestComputePaperMakerSkewedQuoteRespectsConfiguredGap(t *testing.T) {
	tight, ok := computePaperMakerSkewedQuote("buy", 0.47, 0.53, 0.0, 0.003, paperMakerStrategyParams)
	if !ok {
		t.Fatal("expected tight maker quote")
	}
	wide, ok := computePaperMakerSkewedQuote("buy", 0.47, 0.53, 0.0, 0.012, paperMakerStrategyParams)
	if !ok {
		t.Fatal("expected wide maker quote")
	}
	if tight <= wide {
		t.Fatalf("expected tighter gap to place buy closer to ask: tight=%.3f wide=%.3f", tight, wide)
	}

	tightSell, ok := computePaperMakerSkewedQuote("sell", 0.47, 0.53, 0.0, 0.003, paperMakerStrategyParams)
	if !ok {
		t.Fatal("expected tight maker sell quote")
	}
	wideSell, ok := computePaperMakerSkewedQuote("sell", 0.47, 0.53, 0.0, 0.012, paperMakerStrategyParams)
	if !ok {
		t.Fatal("expected wide maker sell quote")
	}
	if tightSell >= wideSell {
		t.Fatalf("expected tighter gap to place sell closer to bid: tight=%.3f wide=%.3f", tightSell, wideSell)
	}
}

func TestComputePaperMakerProtectedSellQuoteIgnoresCostFloor(t *testing.T) {
	price, ok := computePaperMakerProtectedSellQuote(0.47, 0.60, 0.52, 0.02, 0.0, 0.008, 0, time.Hour, paperMakerStrategyParams)
	if !ok {
		t.Fatal("expected protected sell quote to be available")
	}
	if price < 0.54 {
		t.Fatalf("sell quote = %.3f, want at least 0.540 to clear cost floor", price)
	}
	if price >= 0.60 {
		t.Fatalf("sell quote = %.3f, want inside spread", price)
	}
}

func TestComputePaperMakerProtectedSellQuoteSucceedsEvenWhenNoProfitableRoom(t *testing.T) {
	if _, ok := computePaperMakerProtectedSellQuote(0.47, 0.54, 0.53, 0.02, 0.0, 0.008, 0, time.Hour, paperMakerStrategyParams); !ok {
		t.Fatal("expected protected sell quote to succeed even when spread cannot clear cost floor")
	}
}

func TestShouldPaperMakerBlockBuyRejectsBadPairCompletion(t *testing.T) {
	if !shouldPaperMakerBlockBuy(0, true, 24, 0.77, 0.46, 0.02) {
		t.Fatal("expected buy to be blocked when completing peer inventory would lock a bad pair")
	}
	if shouldPaperMakerBlockBuy(0, true, 24, 0.77, 0.20, 0.02) {
		t.Fatal("expected cheap enough completion buy to remain allowed")
	}
}

func TestShouldPaperMakerBlockBuyRejectsNoExitAddOn(t *testing.T) {
	if !shouldPaperMakerBlockBuy(20, false, 8, 0.35, 0.69, 0.02) {
		t.Fatal("expected add-on buy to be blocked when the current side is already heavy with no profitable sell")
	}
	if shouldPaperMakerBlockBuy(8, false, 20, 0.35, 0.20, 0.02) {
		t.Fatal("expected underweight side to remain buyable when completion price is safe")
	}
}
