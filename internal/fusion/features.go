package fusion

import (
	"math"
	"time"

	"Market-bot/internal/core"
	"Market-bot/internal/paper"
)

type timedValue struct {
	At    time.Time
	Value float64
}

type ModelFeatures struct {
	CurrentProb           float64 `json:"current_prob"`
	FairUp                float64 `json:"fair_up"`
	Score                 float64 `json:"score"`
	SmoothedScore         float64 `json:"smoothed_score"`
	ExternalModelScore    float64 `json:"external_model_score"`
	ExternalModelWeight   float64 `json:"external_model_weight"`
	ExternalModelReason   string  `json:"external_model_reason"`
	Returns1m             float64 `json:"returns_1m"`
	Returns5m             float64 `json:"returns_5m"`
	Returns10m            float64 `json:"returns_10m"`
	OrderBookImbalanceL1  float64 `json:"order_book_imbalance_l1"`
	OrderBookImbalanceL5  float64 `json:"order_book_imbalance_l5"`
	TradeFlowImbalance    float64 `json:"trade_flow_imbalance"`
	CVDAcceleration       float64 `json:"cvd_acceleration"`
	SpreadPct             float64 `json:"spread_pct"`
	TradeIntensity        float64 `json:"trade_intensity"`
	LargeTradeFlag        float64 `json:"large_trade_flag"`
	Vol5m                 float64 `json:"vol_5m"`
	VolExpansion          float64 `json:"vol_expansion"`
	VolRegime             float64 `json:"vol_regime"`
	TrendRegime           float64 `json:"trend_regime"`
	ProbVelocity          float64 `json:"prob_velocity"`
	TimeRemainingFraction float64 `json:"time_remaining_fraction"`
	HasPosition           bool    `json:"has_position"`
	PositionSide          string  `json:"position_side"`
	PositionPnL           float64 `json:"position_pnl"`
	PrimaryReason         string  `json:"primary_reason"`
}

func recordMarketMicroLocked(market *trackedMarket, now time.Time) {
	if market == nil {
		return
	}
	market.EventTimes = append(market.EventTimes, now)
	market.EventTimes = pruneTimes(market.EventTimes, now.Add(-10*time.Minute))

	upMid := midpoint(market.Bids["Up"], market.Asks["Up"])
	if upMid <= 0 {
		return
	}
	if len(market.UpMidHistory) == 0 || now.Sub(market.UpMidHistory[len(market.UpMidHistory)-1].At) >= time.Second || math.Abs(market.UpMidHistory[len(market.UpMidHistory)-1].Value-upMid) >= 0.001 {
		market.UpMidHistory = append(market.UpMidHistory, timedValue{At: now, Value: upMid})
	}
	market.UpMidHistory = pruneTimedValues(market.UpMidHistory, now.Add(-15*time.Minute))
	market.ScoreHistory = pruneTimedValues(market.ScoreHistory, now.Add(-10*time.Minute))
}

func buildModelFeatures(cfg *core.Config, market *trackedMarket, quote BinanceQuote, bin BinanceSignals, pos *paper.Position) ModelFeatures {
	features := ModelFeatures{}
	if market == nil {
		return features
	}

	prob := midpoint(market.Bids["Up"], market.Asks["Up"])
	if prob <= 0 {
		prob = 0.5
	}
	features.CurrentProb = prob
	features.Returns1m = bin.Returns1m
	features.Returns5m = bin.Returns5m
	features.Returns10m = bin.Returns10m
	features.TradeFlowImbalance = bin.TradeFlowImbalance
	features.CVDAcceleration = clamp(bin.CVDAcceleration, -1, 1)
	features.TradeIntensity = bin.TradeIntensity
	features.LargeTradeFlag = bin.LargeTradeFlag
	features.OrderBookImbalanceL1 = computeBookSkew(market.DepthBids["Up"], market.DepthAsks["Up"])
	features.OrderBookImbalanceL5 = computeDepthImbalance(market.DepthBids["Up"], market.DepthAsks["Up"], 5)
	features.ProbVelocity = historyDelta(market.UpMidHistory, 30*time.Second)
	features.Vol5m = rollingStd(market.UpMidHistory, 5*time.Minute)
	baselineVol := rollingStdRange(market.UpMidHistory, 10*time.Minute, 5*time.Minute)
	if baselineVol > 0 {
		features.VolExpansion = clamp((features.Vol5m/baselineVol)-1, -1, 2)
	}
	features.VolRegime = clamp(features.VolExpansion, -1, 1)
	features.TrendRegime = clamp((features.Returns5m*30+features.Returns10m*20+features.ProbVelocity*8)/math.Max(0.05, features.Vol5m*150), -1, 1)
	features.TimeRemainingFraction = clamp(time.Until(market.Market.EndTime).Seconds()/(15*60), 0, 1)
	features.TradeIntensity += float64(countRecentTimes(market.EventTimes, 10*time.Second)) / 10.0

	if market.Bids["Up"] > 0 && market.Asks["Up"] > 0 && prob > 0 {
		features.SpreadPct = (market.Asks["Up"] - market.Bids["Up"]) / prob
	}

	if pos != nil {
		features.HasPosition = true
		features.PositionSide = pos.Outcome
		features.PositionPnL = unrealizedPnL(pos, market)
	}

	baseScore, reason := scoreFeatures(features)
	features.PrimaryReason = reason
	features.SmoothedScore = smoothScore(market.ScoreHistory, baseScore)
	features.Score = clamp(baseScore*0.7+features.SmoothedScore*0.3, -0.30, 0.30)
	features.FairUp = clamp(prob+features.Score, 0.02, 0.98)

	if cfg != nil && features.TimeRemainingFraction < 0.08 {
		features.Score *= 0.8
		features.FairUp = clamp(prob+features.Score, 0.02, 0.98)
	}

	return features
}

func applyExternalModelScore(features ModelFeatures, score externalModelScore) ModelFeatures {
	weight := clamp(score.Confidence, 0, 1) * 0.45
	if weight <= 0 || score.Score == 0 {
		return features
	}
	features.ExternalModelScore = clamp(score.Score, -0.30, 0.30)
	features.ExternalModelWeight = weight
	features.ExternalModelReason = score.Reason
	features.Score = clamp(features.Score*(1-weight)+features.ExternalModelScore*weight, -0.30, 0.30)
	features.FairUp = clamp(features.CurrentProb+features.Score, 0.02, 0.98)
	if weight >= 0.25 && math.Abs(features.ExternalModelScore) >= math.Abs(features.Score)*0.6 {
		features.PrimaryReason = "external_model"
	}
	return features
}

func scoreFeatures(f ModelFeatures) (float64, string) {
	type contribution struct {
		name  string
		value float64
	}
	bias := signOrZero(f.TradeFlowImbalance + f.Returns1m*30)
	if bias == 0 {
		bias = signOrZero(f.TrendRegime + f.ProbVelocity*8)
	}
	parts := []contribution{
		{"mom1", clamp(f.Returns1m*45, -0.12, 0.12)},
		{"mom5", clamp(f.Returns5m*28, -0.10, 0.10)},
		{"mom10", clamp(f.Returns10m*18, -0.07, 0.07)},
		{"obL1", 0.06 * clamp(f.OrderBookImbalanceL1, -1, 1)},
		{"obL5", 0.04 * clamp(f.OrderBookImbalanceL5, -1, 1)},
		{"flow", 0.08 * clamp(f.TradeFlowImbalance, -1, 1)},
		{"cvd", 0.05 * clamp(f.CVDAcceleration, -1, 1)},
		{"spread", -0.06 * clamp(f.SpreadPct*8, 0, 1)},
		{"intensity", 0.03 * clamp(f.TradeIntensity/6, 0, 1) * bias},
		{"large", 0.04 * f.LargeTradeFlag * bias},
		{"vol", 0.03 * clamp(f.VolRegime, -1, 1) * signOrZero(f.TrendRegime)},
		{"trend", 0.05 * clamp(f.TrendRegime, -1, 1)},
		{"prob", 0.04 * clamp(f.ProbVelocity*10, -1, 1)},
	}
	score := 0.0
	primary := "balanced"
	strongest := 0.0
	for _, part := range parts {
		score += part.value
		if abs := math.Abs(part.value); abs > strongest {
			strongest = abs
			primary = part.name
		}
	}
	return clamp(score, -0.30, 0.30), primary
}

func smoothScore(history []timedValue, current float64) float64 {
	values := make([]float64, 0, 5)
	start := 0
	if len(history) > 4 {
		start = len(history) - 4
	}
	for _, item := range history[start:] {
		values = append(values, item.Value)
	}
	values = append(values, current)
	if len(values) == 0 {
		return 0
	}
	weightSum := 0.0
	total := 0.0
	for i, value := range values {
		weight := float64(i + 1)
		total += value * weight
		weightSum += weight
	}
	if weightSum == 0 {
		return 0
	}
	return clamp(total/weightSum, -0.30, 0.30)
}

func computeDepthImbalance(bids, asks []paper.MarketLevel, depth int) float64 {
	bidSize := sumLevelSize(bids, depth)
	askSize := sumLevelSize(asks, depth)
	if bidSize+askSize == 0 {
		return 0
	}
	return clamp((bidSize-askSize)/(bidSize+askSize), -1, 1)
}

func sumLevelSize(levels []paper.MarketLevel, depth int) float64 {
	if depth <= 0 || len(levels) == 0 {
		return 0
	}
	if depth > len(levels) {
		depth = len(levels)
	}
	total := 0.0
	for _, level := range levels[:depth] {
		total += level.Size
	}
	return total
}

func historyDelta(history []timedValue, window time.Duration) float64 {
	if len(history) < 2 {
		return 0
	}
	now := history[len(history)-1].At
	threshold := now.Add(-window)
	reference := history[0].Value
	for _, item := range history {
		if !item.At.Before(threshold) {
			reference = item.Value
			break
		}
		reference = item.Value
	}
	if reference == 0 {
		return 0
	}
	return history[len(history)-1].Value - reference
}

func rollingStd(history []timedValue, window time.Duration) float64 {
	now := time.Time{}
	if len(history) == 0 {
		return 0
	}
	now = history[len(history)-1].At
	cutoff := now.Add(-window)
	vals := make([]float64, 0, len(history))
	for _, item := range history {
		if item.At.Before(cutoff) {
			continue
		}
		vals = append(vals, item.Value)
	}
	return stddev(vals)
}

func rollingStdRange(history []timedValue, older, recent time.Duration) float64 {
	if len(history) == 0 || older <= recent {
		return 0
	}
	now := history[len(history)-1].At
	start := now.Add(-older)
	end := now.Add(-recent)
	vals := make([]float64, 0, len(history))
	for _, item := range history {
		if item.At.Before(start) || item.At.After(end) {
			continue
		}
		vals = append(vals, item.Value)
	}
	return stddev(vals)
}

func stddev(values []float64) float64 {
	if len(values) < 2 {
		return 0
	}
	mean := 0.0
	for _, value := range values {
		mean += value
	}
	mean /= float64(len(values))
	variance := 0.0
	for _, value := range values {
		diff := value - mean
		variance += diff * diff
	}
	variance /= float64(len(values))
	return math.Sqrt(variance)
}

func countRecentTimes(times []time.Time, window time.Duration) int {
	if len(times) == 0 {
		return 0
	}
	cutoff := times[len(times)-1].Add(-window)
	count := 0
	for _, item := range times {
		if !item.Before(cutoff) {
			count++
		}
	}
	return count
}

func pruneTimedValues(items []timedValue, cutoff time.Time) []timedValue {
	idx := 0
	for idx < len(items) && items[idx].At.Before(cutoff) {
		idx++
	}
	if idx == 0 {
		return items
	}
	return append([]timedValue(nil), items[idx:]...)
}

func pruneTimes(items []time.Time, cutoff time.Time) []time.Time {
	idx := 0
	for idx < len(items) && items[idx].Before(cutoff) {
		idx++
	}
	if idx == 0 {
		return items
	}
	return append([]time.Time(nil), items[idx:]...)
}

func unrealizedPnL(pos *paper.Position, market *trackedMarket) float64 {
	if pos == nil || market == nil {
		return 0
	}
	bid := market.Bids[pos.Outcome]
	if bid <= 0 {
		return 0
	}
	return (bid - pos.AvgPrice) * pos.Quantity
}

func signOrZero(v float64) float64 {
	if v > 0 {
		return 1
	}
	if v < 0 {
		return -1
	}
	return 0
}
