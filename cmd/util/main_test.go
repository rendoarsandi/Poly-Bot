package main

import (
	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
	"math"
	"testing"
	"time"
)

func TestIsUtilbotMarketInEntryWindow15m(t *testing.T) {
	now := time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		timeLeft time.Duration
		want     bool
	}{
		{name: "too_early", timeLeft: 15 * time.Minute, want: false},
		{name: "upper_bound", timeLeft: 14 * time.Minute, want: true},
		{name: "early_window", timeLeft: 11 * time.Minute, want: true},
		{name: "mid_window", timeLeft: 6 * time.Minute, want: true},
		{name: "inside_window", timeLeft: 3 * time.Minute, want: true},
		{name: "lower_bound", timeLeft: 3 * time.Minute, want: true},
		{name: "below_lower_bound", timeLeft: 2 * time.Minute, want: false},
		{name: "under_one_minute", timeLeft: 59 * time.Second, want: false},
		{name: "expired", timeLeft: -1 * time.Second, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			endTime := now.Add(tc.timeLeft)
			got := isUtilbotMarketInEntryWindow(now, endTime, "15m")
			if got != tc.want {
				t.Fatalf("timeLeft=%v got %v want %v", tc.timeLeft, got, tc.want)
			}
		})
	}
}

func TestIsUtilbotMarketInEntryWindowDefaultTimeframe(t *testing.T) {
	now := time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)

	if !isUtilbotMarketInEntryWindow(now, now.Add(6*time.Minute), "5m") {
		t.Fatal("expected non-15m timeframe to allow markets with more than 5 minutes left")
	}

	if isUtilbotMarketInEntryWindow(now, now.Add(30*time.Second), "5m") {
		t.Fatal("expected markets with less than 1 minute left to be rejected")
	}
}

func TestResolveUtilbotMarketEndTimePrefersExactMarketEndTime(t *testing.T) {
	exactEndTime := time.Date(2026, 3, 7, 12, 3, 0, 0, time.UTC)
	market := &api.Market{
		Slug:    "btc-updown-15m-not-a-timestamp",
		EndTime: exactEndTime,
	}

	got, err := resolveUtilbotMarketEndTime(market)
	if err != nil {
		t.Fatalf("resolveUtilbotMarketEndTime returned error: %v", err)
	}
	if !got.Equal(exactEndTime) {
		t.Fatalf("got %v want %v", got, exactEndTime)
	}
}

func TestPickUtilbotMarketsPrefersClosestExpiryPerAsset(t *testing.T) {
	now := time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)
	markets := []api.Market{
		{
			Active:  true,
			Slug:    "btc-updown-15m-farther",
			EndTime: now.Add(4 * time.Minute),
		},
		{
			Active:  true,
			Slug:    "btc-updown-15m-closer",
			EndTime: now.Add(3 * time.Minute),
		},
		{
			Active:  true,
			Slug:    "eth-updown-15m-valid",
			EndTime: now.Add(3 * time.Minute),
		},
		{
			Active:  true,
			Slug:    "btc-updown-15m-too-early",
			EndTime: now.Add(7 * time.Minute),
		},
	}

	found := pickUtilbotMarkets(now, markets, "15m", []string{"btc", "eth"})

	if len(found) != 2 {
		t.Fatalf("expected 2 markets, got %d", len(found))
	}
	if found["BTC"] == nil || found["BTC"].Slug != "btc-updown-15m-closer" {
		t.Fatalf("expected closest BTC market, got %+v", found["BTC"])
	}
	if found["ETH"] == nil || found["ETH"].Slug != "eth-updown-15m-valid" {
		t.Fatalf("expected ETH market, got %+v", found["ETH"])
	}
}

func TestUtilbotFinderPollInterval(t *testing.T) {
	if got := utilbotFinderPollInterval("15m"); got != 500*time.Millisecond {
		t.Fatalf("15m poll interval got %v want %v", got, 500*time.Millisecond)
	}
	if got := utilbotFinderPollInterval("5m"); got != 1*time.Second {
		t.Fatalf("default poll interval got %v want %v", got, 1*time.Second)
	}
}

func TestUtilbotBalancedAndExcessShares(t *testing.T) {
	balanced, excess0, excess1 := utilbotBalancedAndExcessShares(2.1579, 1.9541)

	if math.Abs(balanced-1.9541) > 0.000001 {
		t.Fatalf("balanced got %.4f want %.4f", balanced, 1.9541)
	}
	if math.Abs(excess0-0.2038) > 0.000001 {
		t.Fatalf("excess0 got %.4f want %.4f", excess0, 0.2038)
	}
	if math.Abs(excess1) > 0.000001 {
		t.Fatalf("excess1 got %.4f want 0", excess1)
	}
}

func TestNormalizePanicBuySharesPerSideBumpsNearMinimum(t *testing.T) {
	got, bumped := normalizePanicBuySharesPerSide(1.0, 0.99, 0.02)
	if !bumped {
		t.Fatal("expected utilbot buy shares to bump toward the per-leg minimum")
	}
	if math.Abs(got-10.0) > 0.000001 {
		t.Fatalf("got %.4f want 10.0", got)
	}
}

func TestNormalizePanicBuySharesPerSideKeepsReasonableHighAskBump(t *testing.T) {
	got, bumped := normalizePanicBuySharesPerSide(1.0, 0.99, 0.99)
	if !bumped {
		t.Fatal("expected 0.99 ask leg to get a small bump above 1 share")
	}
	if math.Abs(got-1.0102) > 0.0001 {
		t.Fatalf("got %.4f want about 1.0102", got)
	}
}

func TestUtilbotBuyLimitPrice(t *testing.T) {
	cfg := &core.Config{MinAskPrice: 0.10, MaxAskPrice: 0.90, BuyExecutionMarginFloorPercent: -3.0}

	price, err := utilbotBuyLimitPrice(0.15, 0.70, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if math.Abs(price-0.33) > 0.000001 {
		t.Fatalf("price got %.4f want 0.33", price)
	}

	price, err = utilbotBuyLimitPrice(0.88, 0.10, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if math.Abs(price-0.90) > 0.000001 {
		t.Fatalf("capped price got %.4f want 0.90", price)
	}

	if _, err := utilbotBuyLimitPrice(0, 0.40, cfg); err == nil {
		t.Fatal("expected invalid ask price error")
	}
}

func TestUtilbotBestPriceFromLevels(t *testing.T) {
	asks := []paper.MarketLevel{{Price: 0.32}, {Price: 0.41}, {Price: 0.29}}
	bestAsk, ok := utilbotBestAskFromLevels(asks)
	if !ok || math.Abs(bestAsk-0.29) > 0.000001 {
		t.Fatalf("best ask got %.4f ok=%v want 0.29,true", bestAsk, ok)
	}

	bids := []paper.MarketLevel{{Price: 0.62}, {Price: 0.55}, {Price: 0.68}}
	bestBid, ok := utilbotBestBidFromLevels(bids)
	if !ok || math.Abs(bestBid-0.68) > 0.000001 {
		t.Fatalf("best bid got %.4f ok=%v want 0.68,true", bestBid, ok)
	}

	if _, ok := utilbotBestAskFromLevels(nil); ok {
		t.Fatal("expected empty ask levels to return false")
	}
	if _, ok := utilbotBestBidFromLevels(nil); ok {
		t.Fatal("expected empty bid levels to return false")
	}
}

func TestTradeSucceeded(t *testing.T) {
	if tradeSucceeded(nil, nil) {
		t.Fatal("nil result should not count as success")
	}
	if tradeSucceeded(&trading.TradeResult{Success: true}, nil) {
		t.Fatal("blank success response should not count as success")
	}
	if !tradeSucceeded(&trading.TradeResult{Success: true, Status: "MATCHED"}, nil) {
		t.Fatal("status-backed result should count as success")
	}
}

func TestUtilbotAnyTradeSucceeded(t *testing.T) {
	results := []*trading.TradeResult{
		{Success: true},
		{Success: true, Status: "matched"},
	}
	errs := []error{nil, nil}
	if !utilbotAnyTradeSucceeded(results, errs) {
		t.Fatal("expected at least one successful trade to be detected")
	}

	results[1] = nil
	if utilbotAnyTradeSucceeded(results, errs) {
		t.Fatal("expected no successful trades when all responses are blank or nil")
	}
}

func TestUtilbotAcquiredShares(t *testing.T) {
	acquired0, acquired1 := utilbotAcquiredShares(4.2, 3.7, 1.2, 1.5, true)
	if math.Abs(acquired0-3.0) > 0.000001 || math.Abs(acquired1-2.2) > 0.000001 {
		t.Fatalf("snapshot acquired shares got %.4f/%.4f want 3.0/2.2", acquired0, acquired1)
	}

	acquired0, acquired1 = utilbotAcquiredShares(0.9, 1.1, 0, 0, false)
	if math.Abs(acquired0-0.9) > 0.000001 || math.Abs(acquired1-1.1) > 0.000001 {
		t.Fatalf("live acquired shares got %.4f/%.4f want 0.9/1.1", acquired0, acquired1)
	}
}
