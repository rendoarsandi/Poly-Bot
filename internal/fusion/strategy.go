package fusion

import (
	"fmt"
	"math"
	"strings"
	"time"

	"Market-bot/internal/core"
	"Market-bot/internal/paper"
)

const flattenBeforeExpiry = 45 * time.Second

const (
	maxEntryMarketDataAge  = 3 * time.Second
	maxEntryBinanceDataAge = 3 * time.Second
	maxEntrySpreadPct      = 0.08
	minEntryScoreMagnitude = 0.02
	minEntryAskDepthShares = 60.0
)

type SignalSnapshot struct {
	Asset          string
	UpBid          float64
	UpAsk          float64
	DownBid        float64
	DownAsk        float64
	TimeRemaining  time.Duration
	Position       *paper.Position
	Features       ModelFeatures
	MarketDataAge  time.Duration
	BinanceDataAge time.Duration
	UpAskDepth     float64
	DownAskDepth   float64
}

type Decision struct {
	Action     string  `json:"action"`
	Outcome    string  `json:"outcome"`
	Price      float64 `json:"price"`
	Edge       float64 `json:"edge"`
	FairUp     float64 `json:"fair_up"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

func computeBookSkew(bids, asks []paper.MarketLevel) float64 {
	if len(bids) == 0 && len(asks) == 0 {
		return 0
	}
	bidSize := 0.0
	askSize := 0.0
	if len(bids) > 0 {
		bidSize = bids[0].Size
	}
	if len(asks) > 0 {
		askSize = asks[0].Size
	}
	if bidSize+askSize == 0 {
		return 0
	}
	return clamp((bidSize-askSize)/(bidSize+askSize), -1, 1)
}

func decideAction(cfg *core.Config, snap SignalSnapshot) Decision {
	fairUp := snap.Features.FairUp
	if fairUp <= 0 {
		fairUp = 0.5
	}
	fairDown := 1 - fairUp
	entryThreshold := dynamicEntryThreshold(cfg, snap.Features)
	exitThreshold := math.Max(0.005, entryThreshold/2)

	if snap.Position != nil {
		if snap.TimeRemaining <= flattenBeforeExpiry {
			return exitDecision(snap.Position.Outcome, bidForOutcome(snap, snap.Position.Outcome), fairUp, 0, "near expiry")
		}
		if snap.Position.Outcome == "Up" {
			if snap.UpBid > 0 && (fairUp <= snap.UpBid+exitThreshold || snap.Features.Score <= -entryThreshold*0.35) {
				return exitDecision("Up", snap.UpBid, fairUp, snap.UpBid-fairUp, "up edge decayed")
			}
			if snap.DownAsk > 0 && fairDown-snap.DownAsk >= entryThreshold*0.75 {
				return exitDecision("Up", snap.UpBid, fairUp, fairDown-snap.DownAsk, "signal flipped down")
			}
			return Decision{Action: "HOLD", FairUp: fairUp}
		}
		if snap.DownBid > 0 && (fairDown <= snap.DownBid+exitThreshold || snap.Features.Score >= entryThreshold*0.35) {
			return exitDecision("Down", snap.DownBid, fairUp, snap.DownBid-fairDown, "down edge decayed")
		}
		if snap.UpAsk > 0 && fairUp-snap.UpAsk >= entryThreshold*0.75 {
			return exitDecision("Down", snap.DownBid, fairUp, fairUp-snap.UpAsk, "signal flipped up")
		}
		return Decision{Action: "HOLD", FairUp: fairUp}
	}

	if snap.TimeRemaining <= 90*time.Second {
		return Decision{Action: "HOLD", FairUp: fairUp, Reason: "late market"}
	}

	upEdge := fairUp - snap.UpAsk
	downEdge := fairDown - snap.DownAsk
	confidence := decisionConfidence(snap.Features, upEdge, downEdge)
	if snap.UpAsk >= cfg.MinAskPrice && snap.UpAsk <= cfg.MaxAskPrice && upEdge >= downEdge && upEdge >= entryThreshold {
		if reason := entryBlockReason(snap, "Up"); reason != "" {
			return Decision{Action: "HOLD", FairUp: fairUp, Confidence: confidence, Reason: reason}
		}
		return Decision{Action: "BUY", Outcome: "Up", Price: snap.UpAsk, Edge: upEdge, FairUp: fairUp, Confidence: confidence, Reason: fmt.Sprintf("%s + bullish fusion", snap.Features.PrimaryReason)}
	}
	if snap.DownAsk >= cfg.MinAskPrice && snap.DownAsk <= cfg.MaxAskPrice && downEdge > upEdge && downEdge >= entryThreshold {
		if reason := entryBlockReason(snap, "Down"); reason != "" {
			return Decision{Action: "HOLD", FairUp: fairUp, Confidence: confidence, Reason: reason}
		}
		return Decision{Action: "BUY", Outcome: "Down", Price: snap.DownAsk, Edge: downEdge, FairUp: fairUp, Confidence: confidence, Reason: fmt.Sprintf("%s + bearish fusion", snap.Features.PrimaryReason)}
	}

	return Decision{Action: "HOLD", FairUp: fairUp, Reason: "no edge"}
}

func dynamicEntryThreshold(cfg *core.Config, features ModelFeatures) float64 {
	threshold := math.Max(0.01, cfg.MinMarginPercent/100.0)
	threshold += clamp(features.SpreadPct*0.20, 0, 0.02)
	if features.TimeRemainingFraction < 0.15 {
		threshold += 0.01
	}
	return threshold
}

func decisionConfidence(features ModelFeatures, upEdge, downEdge float64) float64 {
	edge := math.Max(math.Abs(upEdge), math.Abs(downEdge))
	strength := math.Max(math.Abs(features.Score), edge*2)
	return clamp(0.25+strength*3, 0.25, 1.0)
}

func entryBlockReason(snap SignalSnapshot, outcome string) string {
	if snap.MarketDataAge > maxEntryMarketDataAge {
		return "stale polymarket"
	}
	if snap.BinanceDataAge > maxEntryBinanceDataAge {
		return "stale binance"
	}
	if snap.Features.SpreadPct >= maxEntrySpreadPct {
		return "wide spread"
	}
	if math.Abs(snap.Features.Score) < minEntryScoreMagnitude {
		return "weak score"
	}
	if askDepthForOutcome(snap, outcome) < minEntryAskDepthShares {
		return "thin ask depth"
	}
	if !signalConsensus(snap.Features, outcome) {
		return "weak consensus"
	}
	return ""
}

func askDepthForOutcome(snap SignalSnapshot, outcome string) float64 {
	if strings.EqualFold(outcome, "Up") {
		return snap.UpAskDepth
	}
	return snap.DownAskDepth
}

func signalConsensus(features ModelFeatures, outcome string) bool {
	votes := 0
	if strings.EqualFold(outcome, "Up") {
		if features.Returns1m > 0 {
			votes++
		}
		if features.Returns5m > 0 {
			votes++
		}
		if features.TradeFlowImbalance > 0 {
			votes++
		}
		if features.OrderBookImbalanceL1 > 0 || features.OrderBookImbalanceL5 > 0 {
			votes++
		}
		if features.ProbVelocity > 0 {
			votes++
		}
		if features.TrendRegime > 0 {
			votes++
		}
	} else {
		if features.Returns1m < 0 {
			votes++
		}
		if features.Returns5m < 0 {
			votes++
		}
		if features.TradeFlowImbalance < 0 {
			votes++
		}
		if features.OrderBookImbalanceL1 < 0 || features.OrderBookImbalanceL5 < 0 {
			votes++
		}
		if features.ProbVelocity < 0 {
			votes++
		}
		if features.TrendRegime < 0 {
			votes++
		}
	}
	return votes >= 3
}

func exitDecision(outcome string, price, fairUp, edge float64, reason string) Decision {
	if price <= 0 {
		return Decision{Action: "HOLD", FairUp: fairUp}
	}
	return Decision{Action: "SELL", Outcome: outcome, Price: price, Edge: edge, FairUp: fairUp, Confidence: 1.0, Reason: reason}
}

func bidForOutcome(snap SignalSnapshot, outcome string) float64 {
	if outcome == "Up" {
		return snap.UpBid
	}
	return snap.DownBid
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
