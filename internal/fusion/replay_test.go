package fusion

import (
	"testing"
	"time"

	"Market-bot/internal/core"
)

func TestEvaluateReplayCompletesProfitableRoundTrip(t *testing.T) {
	now := time.Date(2026, 3, 9, 7, 0, 0, 0, time.UTC)
	cfg := &core.Config{BaseBalance: 1000, BaseTradeSize: 50, TradeScaleFactor: 0.05, MinMarginPercent: 2, MinAskPrice: 0.1, MaxAskPrice: 0.9}
	report := EvaluateReplay(cfg, []ReplaySnapshot{
		{
			Timestamp:            now,
			Asset:                "BTC",
			MarketID:             "cond-1",
			UpBid:                0.59,
			UpAsk:                0.60,
			DownBid:              0.39,
			DownAsk:              0.40,
			TimeRemainingSec:     300,
			MarketDataAgeMillis:  250,
			BinanceDataAgeMillis: 250,
			UpAskDepth:           300,
			DownAskDepth:         300,
			Features:             ModelFeatures{FairUp: 0.66, Score: 0.08, Returns1m: 0.002, Returns5m: 0.003, TradeFlowImbalance: 0.3, OrderBookImbalanceL1: 0.2, ProbVelocity: 0.01, TrendRegime: 0.2},
		},
		{
			Timestamp:            now.Add(20 * time.Second),
			Asset:                "BTC",
			MarketID:             "cond-1",
			UpBid:                0.67,
			UpAsk:                0.68,
			DownBid:              0.31,
			DownAsk:              0.32,
			TimeRemainingSec:     20,
			MarketDataAgeMillis:  250,
			BinanceDataAgeMillis: 250,
			UpAskDepth:           300,
			DownAskDepth:         300,
			Features:             ModelFeatures{FairUp: 0.69, Score: 0.03},
		},
	})
	if report.CompletedTrades != 1 {
		t.Fatalf("expected 1 completed trade, got %+v", report)
	}
	if report.RealizedPnL <= 0 {
		t.Fatalf("expected positive realized pnl, got %+v", report)
	}
}

func TestEvaluateReplayBlocksStaleEntries(t *testing.T) {
	now := time.Date(2026, 3, 9, 7, 0, 0, 0, time.UTC)
	report := EvaluateReplay(defaultFusionConfig(), []ReplaySnapshot{{
		Timestamp:            now,
		Asset:                "ETH",
		MarketID:             "cond-2",
		UpBid:                0.58,
		UpAsk:                0.59,
		DownBid:              0.41,
		DownAsk:              0.42,
		TimeRemainingSec:     300,
		MarketDataAgeMillis:  5000,
		BinanceDataAgeMillis: 200,
		UpAskDepth:           300,
		DownAskDepth:         300,
		Features:             ModelFeatures{FairUp: 0.67, Score: 0.08, Returns1m: 0.003, Returns5m: 0.003, TradeFlowImbalance: 0.3, OrderBookImbalanceL1: 0.2, ProbVelocity: 0.01, TrendRegime: 0.2},
	}})
	if report.CompletedTrades != 0 || report.RealizedPnL != 0 {
		t.Fatalf("expected stale snapshots to avoid trading, got %+v", report)
	}
}
