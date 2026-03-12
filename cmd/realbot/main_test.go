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

func TestComputeRealbotMakerProtectedSellQuoteIgnoresCostFloor(t *testing.T) {
	price, ok := computeRealbotMakerProtectedSellQuote(0.54, 0.60, 0.56, 0.02, 0, 0.008, 1000, realbotMakerStrategyParams)
	if !ok {
		t.Fatal("expected protected sell quote to exist")
	}
	if price != 0.578 {
		t.Fatalf("expected sell quote to be based on mid and gap, got %.3f", price)
	}
	// Even in a narrow market, it should still place a quote (no longer rejects purely on cost)
	if _, ok := computeRealbotMakerProtectedSellQuote(0.54, 0.56, 0.56, 0.02, 0, 0.008, 1000, realbotMakerStrategyParams); !ok {
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
	asks := map[string]float64{"Down": 0.36, "Up": 0.64}
	depth := map[string][]paper.MarketLevel{
		"Down": {{Price: 0.36, Size: 10}},
		"Up":   {{Price: 0.64, Size: 8}},
	}
	state := map[string]realbotQuoteState{
		"Down": {UpdatedAt: now.Add(-40 * time.Millisecond), Source: "ws"},
		"Up":   {UpdatedAt: now.Add(-70 * time.Millisecond), Source: "rest"},
	}

	fresh, age, reason := realbotCanUseLocalBuyQuote(now, outcomes, asks, depth, state, 250*time.Millisecond)
	if !fresh || reason != "" {
		t.Fatalf("expected fresh local quote, got fresh=%v age=%v reason=%q", fresh, age, reason)
	}

	state["Up"] = realbotQuoteState{UpdatedAt: now.Add(-400 * time.Millisecond), Source: "ws"}
	fresh, _, reason = realbotCanUseLocalBuyQuote(now, outcomes, asks, depth, state, 250*time.Millisecond)
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
			http.Error(w, "unexpected token", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := api.NewRestClient(server.URL)
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

func TestRealbotCanUseLocalSellQuote(t *testing.T) {
	now := time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC)
	outcomes := []string{"Down", "Up"}
	bids := map[string]float64{"Down": 0.54, "Up": 0.49}
	depth := map[string][]paper.MarketLevel{
		"Down": {{Price: 0.54, Size: 8}},
		"Up":   {{Price: 0.49, Size: 10}},
	}
	state := map[string]realbotQuoteState{
		"Down": {UpdatedAt: now.Add(-40 * time.Millisecond), Source: "ws"},
		"Up":   {UpdatedAt: now.Add(-70 * time.Millisecond), Source: "rest"},
	}

	fresh, age, reason := realbotCanUseLocalSellQuote(now, outcomes, bids, depth, state, 250*time.Millisecond)
	if !fresh || reason != "" || age != 70*time.Millisecond {
		t.Fatalf("expected fresh local sell quote, got fresh=%v age=%v reason=%q", fresh, age, reason)
	}

	state["Up"] = realbotQuoteState{UpdatedAt: now.Add(-400 * time.Millisecond), Source: "ws"}
	fresh, _, reason = realbotCanUseLocalSellQuote(now, outcomes, bids, depth, state, 250*time.Millisecond)
	if fresh || reason == "" {
		t.Fatalf("expected stale sell quote rejection, got fresh=%v reason=%q", fresh, reason)
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
	if !strings.Contains(msg, "0.000432 YES shares") {
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
