package fusion

import (
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
)

func TestBuildModelFeaturesUsesTemporalSmoothing(t *testing.T) {
	now := time.Now()
	market := &trackedMarket{
		Bids:         map[string]float64{"Up": 0.54, "Down": 0.44},
		Asks:         map[string]float64{"Up": 0.56, "Down": 0.46},
		DepthBids:    map[string][]paper.MarketLevel{"Up": []paper.MarketLevel{{Price: 0.54, Size: 100}, {Price: 0.53, Size: 80}}},
		DepthAsks:    map[string][]paper.MarketLevel{"Up": []paper.MarketLevel{{Price: 0.56, Size: 40}, {Price: 0.57, Size: 50}}},
		UpMidHistory: []timedValue{{At: now.Add(-6 * time.Minute), Value: 0.49}, {At: now.Add(-4 * time.Minute), Value: 0.51}, {At: now.Add(-2 * time.Minute), Value: 0.53}, {At: now, Value: 0.55}},
		EventTimes:   []time.Time{now.Add(-4 * time.Second), now.Add(-2 * time.Second), now},
		ScoreHistory: []timedValue{{At: now.Add(-10 * time.Second), Value: 0.04}, {At: now.Add(-5 * time.Second), Value: 0.06}},
		Market:       (&paperlessMarket{EndTime: now.Add(5 * time.Minute)}).toAPIMarket(),
	}
	features := buildModelFeatures(&core.Config{MinMarginPercent: 2}, market, BinanceQuote{Price: 100}, BinanceSignals{Returns1m: 0.002, Returns5m: 0.004, Returns10m: 0.006, TradeFlowImbalance: 0.35, CVDAcceleration: 0.2, TradeIntensity: 1.2, LargeTradeFlag: 1}, nil)
	if features.FairUp <= features.CurrentProb {
		t.Fatalf("expected bullish features to lift fair value: %+v", features)
	}
	if features.SmoothedScore <= 0 {
		t.Fatalf("expected temporal smoothing to stay positive: %+v", features)
	}
	if features.PrimaryReason == "" || features.PrimaryReason == "balanced" {
		t.Fatalf("expected a primary feature reason, got %+v", features)
	}
}

func TestDecideActionBuyUp(t *testing.T) {
	cfg := &core.Config{MinMarginPercent: 2, MinAskPrice: 0.1, MaxAskPrice: 0.9}
	decision := decideAction(cfg, SignalSnapshot{
		UpAsk:         0.62,
		DownAsk:       0.40,
		TimeRemaining: 5 * time.Minute,
		UpAskDepth:    200,
		DownAskDepth:  200,
		Features: ModelFeatures{
			FairUp:               0.67,
			Score:                0.08,
			PrimaryReason:        "flow",
			Returns1m:            0.002,
			Returns5m:            0.003,
			TradeFlowImbalance:   0.3,
			OrderBookImbalanceL1: 0.2,
			ProbVelocity:         0.01,
			TrendRegime:          0.2,
		},
	})
	if decision.Action != "BUY" || decision.Outcome != "Up" {
		t.Fatalf("expected BUY Up, got %+v", decision)
	}
}

func TestDecideActionBuyDown(t *testing.T) {
	cfg := &core.Config{MinMarginPercent: 2, MinAskPrice: 0.1, MaxAskPrice: 0.9}
	decision := decideAction(cfg, SignalSnapshot{
		UpAsk:         0.63,
		DownAsk:       0.34,
		TimeRemaining: 5 * time.Minute,
		UpAskDepth:    200,
		DownAskDepth:  200,
		Features: ModelFeatures{
			FairUp:               0.31,
			Score:                -0.09,
			PrimaryReason:        "mom1",
			Returns1m:            -0.002,
			Returns5m:            -0.003,
			TradeFlowImbalance:   -0.25,
			OrderBookImbalanceL1: -0.2,
			ProbVelocity:         -0.01,
			TrendRegime:          -0.2,
		},
	})
	if decision.Action != "BUY" || decision.Outcome != "Down" {
		t.Fatalf("expected BUY Down, got %+v", decision)
	}
}

func TestDecideActionSellNearExpiry(t *testing.T) {
	cfg := &core.Config{MinMarginPercent: 2, MinAskPrice: 0.1, MaxAskPrice: 0.9}
	decision := decideAction(cfg, SignalSnapshot{
		UpBid:         0.70,
		UpAsk:         0.72,
		DownBid:       0.28,
		DownAsk:       0.30,
		TimeRemaining: 20 * time.Second,
		Position:      &paper.Position{Outcome: "Up", MarketID: "BTC", Quantity: 10, AvgPrice: 0.61},
		Features:      ModelFeatures{FairUp: 0.74, Score: 0.03, PrimaryReason: "mom1"},
	})
	if decision.Action != "SELL" || decision.Outcome != "Up" {
		t.Fatalf("expected SELL Up near expiry, got %+v", decision)
	}
}

func TestDecideActionBlocksStaleEntry(t *testing.T) {
	cfg := &core.Config{MinMarginPercent: 2, MinAskPrice: 0.1, MaxAskPrice: 0.9}
	decision := decideAction(cfg, SignalSnapshot{
		UpAsk:          0.60,
		DownAsk:        0.39,
		TimeRemaining:  5 * time.Minute,
		MarketDataAge:  4 * time.Second,
		BinanceDataAge: 500 * time.Millisecond,
		UpAskDepth:     200,
		DownAskDepth:   200,
		Features: ModelFeatures{
			FairUp:               0.68,
			Score:                0.08,
			Returns1m:            0.002,
			Returns5m:            0.003,
			TradeFlowImbalance:   0.3,
			OrderBookImbalanceL1: 0.2,
			ProbVelocity:         0.01,
			TrendRegime:          0.2,
		},
	})
	if decision.Action != "HOLD" || decision.Reason != "stale polymarket" {
		t.Fatalf("expected stale-data HOLD, got %+v", decision)
	}
}

func TestDecideActionUsesConfigurableFusionThresholds(t *testing.T) {
	cfg := &core.Config{
		MinMarginPercent:           2,
		MinAskPrice:                0.1,
		MaxAskPrice:                0.9,
		FusionMinAskDepthShares:    150,
		FusionMaxSpreadPercent:     4.0,
		FusionMinScorePercent:      5.0,
		FusionMaxMarketDataAgeSec:  1.0,
		FusionMaxBinanceDataAgeSec: 1.0,
		FusionMinConsensusVotes:    4,
	}
	decision := decideAction(cfg, SignalSnapshot{
		UpAsk:          0.60,
		DownAsk:        0.39,
		TimeRemaining:  5 * time.Minute,
		MarketDataAge:  500 * time.Millisecond,
		BinanceDataAge: 500 * time.Millisecond,
		UpAskDepth:     120,
		DownAskDepth:   120,
		Features: ModelFeatures{
			FairUp:               0.68,
			Score:                0.045,
			SpreadPct:            0.03,
			Returns1m:            0.002,
			Returns5m:            0.003,
			TradeFlowImbalance:   0.3,
			OrderBookImbalanceL1: 0.2,
			ProbVelocity:         0.01,
			TrendRegime:          0.2,
		},
	})
	if decision.Action != "HOLD" || decision.Reason != "weak score" {
		t.Fatalf("expected configurable weak-score HOLD, got %+v", decision)
	}
}

type paperlessMarket struct{ EndTime time.Time }

func (p *paperlessMarket) toAPIMarket() *api.Market { return &api.Market{EndTime: p.EndTime} }
