package paper

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"Market-bot/internal/api"
)

const (
	DefaultBinanceSignalPolyMaxMoveCents     = 1.5
	DefaultBinanceSignalPolyAdverseMoveCents = 0.75
	DefaultBinanceSignalSpreadMaxCents       = 4.0
	binanceGapBookScoreLevels                = 3
)

type DirectionalOutcomes struct {
	Up   string
	Down string
}

type MidSample struct {
	Mid float64
	At  time.Time
}

type DirectionalSignalSnapshot struct {
	Outcome     string
	Mid         float64
	BaselineMid float64
	DeltaCents  float64
	UpdatedAt   time.Time
	BaselineAt  time.Time
	Ready       bool
}

type DirectionalSignalTracker struct {
	lookback     time.Duration
	maxBufferAge time.Duration

	mu      sync.RWMutex
	samples map[string][]MidSample
}

type BinanceGapSignal struct {
	TargetOutcome          string
	OppositeOutcome        string
	SignalLabel            string
	BinanceDeltaPercent    float64
	EffectiveGapPercent    float64
	PolyTargetMoveCents    float64
	PolyOppositeMoveCents  float64
	PolyFavorableMoveCents float64
	PolyAdverseMoveCents   float64
	TargetSpreadCents      float64
	TargetBookImbalance    float64
	OppositeBookImbalance  float64
	DirectionalBookScore   float64
}

func directionalBookImbalance(bids, asks []MarketLevel, levels int) float64 {
	if levels <= 0 {
		levels = binanceGapBookScoreLevels
	}
	bidVol := 0.0
	for i := 0; i < len(bids) && i < levels; i++ {
		if bids[i].Size > 0 {
			bidVol += bids[i].Size
		}
	}
	askVol := 0.0
	for i := 0; i < len(asks) && i < levels; i++ {
		if asks[i].Size > 0 {
			askVol += asks[i].Size
		}
	}
	total := bidVol + askVol
	if total <= 0 {
		return 0
	}
	return (bidVol - askVol) / total
}

func NewDirectionalSignalTracker(lookback time.Duration, outcomes []string) *DirectionalSignalTracker {
	if lookback <= 0 {
		lookback = 1500 * time.Millisecond
	}
	maxBufferAge := 30 * time.Second
	if scaled := lookback * 6; scaled > maxBufferAge {
		maxBufferAge = scaled
	}
	samples := make(map[string][]MidSample, len(outcomes))
	for _, outcome := range outcomes {
		key := strings.TrimSpace(outcome)
		if key == "" {
			continue
		}
		samples[key] = nil
	}
	return &DirectionalSignalTracker{
		lookback:     lookback,
		maxBufferAge: maxBufferAge,
		samples:      samples,
	}
}

func (t *DirectionalSignalTracker) Record(outcome string, bid, ask float64, at time.Time) {
	if t == nil || outcome == "" || bid <= 0 || ask <= 0 || ask < bid {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}
	mid := (bid + ask) / 2
	if mid <= 0 {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	key := strings.TrimSpace(outcome)
	series := t.samples[key]
	if n := len(series); n > 0 && !at.Before(series[n-1].At) && at.Sub(series[n-1].At) >= t.lookback {
		// Quote droughts should restart the lag window so stale pre-gap mids cannot drive a fresh signal.
		series = nil
	}
	series = append(series, MidSample{Mid: mid, At: at})
	cutoff := at.Add(-t.maxBufferAge)
	trim := 0
	for trim < len(series)-1 && series[trim].At.Before(cutoff) {
		trim++
	}
	if trim > 0 {
		series = append([]MidSample(nil), series[trim:]...)
	}
	t.samples[key] = series
}

func (t *DirectionalSignalTracker) Snapshot(outcome string, now time.Time) DirectionalSignalSnapshot {
	snap := DirectionalSignalSnapshot{Outcome: strings.TrimSpace(outcome)}
	if t == nil || snap.Outcome == "" {
		return snap
	}
	if now.IsZero() {
		now = time.Now()
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	series := t.samples[snap.Outcome]
	if len(series) == 0 {
		return snap
	}
	latest := series[len(series)-1]
	snap.Mid = latest.Mid
	snap.UpdatedAt = latest.At

	// When evaluating Polymarket's move, we must anchor the lookback strictly to 'now'
	// to properly capture true latency and delays. If we anchor to 'latest.At', a stale
	// or highly delayed Polymarket feed could result in false positives or evaluate
	// moves over arbitrary historical windows rather than the live lookback period.
	target := now.Add(-t.lookback)
	baseline := series[0]
	for i := len(series) - 1; i >= 0; i-- {
		if !series[i].At.After(target) {
			baseline = series[i]
			break
		}
	}
	snap.BaselineMid = baseline.Mid
	snap.BaselineAt = baseline.At
	snap.DeltaCents = (latest.Mid - baseline.Mid) * 100.0

	// Consider the tracker ready if our baseline quote is at least as old as the lookback window.
	// Since we anchor target to 'now', the readiness check should measure from 'now'.
	snap.Ready = !baseline.At.IsZero() && now.Sub(baseline.At) >= t.lookback
	return snap
}

func EvaluateBinanceGapSignal(now time.Time, mapping DirectionalOutcomes, tokenBids, tokenAsks map[string]float64, tokenFullBids, tokenFullAsks map[string][]MarketLevel, binanceSnap api.BinanceFuturesSignalSnapshot, polyTracker *DirectionalSignalTracker, maxAge time.Duration) (BinanceGapSignal, string) {
	signal := BinanceGapSignal{BinanceDeltaPercent: binanceSnap.DeltaPercent}
	if now.IsZero() {
		now = time.Now()
	}
	if polyTracker == nil {
		return signal, "no Polymarket signal tracker"
	}
	if !binanceSnap.Ready {
		return signal, fmt.Sprintf("waiting for Binance lookback window on %s", binanceSnap.Symbol)
	}
	if binanceSnap.UpdatedAt.IsZero() {
		return signal, fmt.Sprintf("waiting for fresh Binance signal on %s", binanceSnap.Symbol)
	}
	if maxAge > 0 && now.Sub(binanceSnap.UpdatedAt) > maxAge {
		return signal, fmt.Sprintf("waiting for fresh Binance signal on %s", binanceSnap.Symbol)
	}

	signal.TargetOutcome = mapping.Up
	signal.OppositeOutcome = mapping.Down
	signal.SignalLabel = "UP"
	if binanceSnap.DeltaPercent < 0 {
		signal.TargetOutcome = mapping.Down
		signal.OppositeOutcome = mapping.Up
		signal.SignalLabel = "DOWN"
	}

	bid := tokenBids[signal.TargetOutcome]
	ask := tokenAsks[signal.TargetOutcome]
	if bid <= 0 || ask <= 0 || ask < bid {
		return signal, fmt.Sprintf("waiting for live %s top of book", signal.TargetOutcome)
	}
	signal.TargetSpreadCents = (ask - bid) * 100.0
	signal.TargetBookImbalance = directionalBookImbalance(tokenFullBids[signal.TargetOutcome], tokenFullAsks[signal.TargetOutcome], binanceGapBookScoreLevels)
	signal.OppositeBookImbalance = directionalBookImbalance(tokenFullBids[signal.OppositeOutcome], tokenFullAsks[signal.OppositeOutcome], binanceGapBookScoreLevels)
	signal.DirectionalBookScore = (signal.TargetBookImbalance - signal.OppositeBookImbalance) / 2.0

	targetSnap := polyTracker.Snapshot(signal.TargetOutcome, now)
	if !targetSnap.Ready {
		return signal, fmt.Sprintf("waiting for Polymarket lookback window on %s", signal.TargetOutcome)
	}
	if targetSnap.UpdatedAt.IsZero() || (maxAge > 0 && now.Sub(targetSnap.UpdatedAt) > maxAge) {
		return signal, fmt.Sprintf("waiting for fresh %s quote history", signal.TargetOutcome)
	}

	oppositeSnap := polyTracker.Snapshot(signal.OppositeOutcome, now)
	if !oppositeSnap.Ready {
		return signal, fmt.Sprintf("waiting for Polymarket lookback window on %s", signal.OppositeOutcome)
	}
	if oppositeSnap.UpdatedAt.IsZero() || (maxAge > 0 && now.Sub(oppositeSnap.UpdatedAt) > maxAge) {
		return signal, fmt.Sprintf("waiting for fresh %s quote history", signal.OppositeOutcome)
	}

	signal.PolyTargetMoveCents = targetSnap.DeltaCents
	signal.PolyOppositeMoveCents = -oppositeSnap.DeltaCents
	signal.PolyFavorableMoveCents = math.Max(math.Max(signal.PolyTargetMoveCents, 0), math.Max(signal.PolyOppositeMoveCents, 0))
	signal.PolyAdverseMoveCents = math.Max(-signal.PolyTargetMoveCents, 0) + math.Max(-signal.PolyOppositeMoveCents, 0)

	rawGap := math.Max(math.Abs(signal.BinanceDeltaPercent)-signal.PolyFavorableMoveCents, 0)

	// Cross-Market State Fusion insight: Extremeness multiplier
	// Binary markets offer higher edge at extreme probabilities (near 0 or 1) due to
	// asymmetric share-based payoffs. We boost the effective gap to prioritize these entries.
	prob := ask
	if prob > 1.0 {
		prob = 1.0
	}
	extremeness := math.Abs(prob-0.5) * 2.0            // Ranges from 0.0 (at 0.5) to 1.0 (at 0 or 1)
	extremenessMultiplier := 1.0 + (1.0 * extremeness) // Scales gap up to 2x at extremes

	signal.EffectiveGapPercent = rawGap * extremenessMultiplier
	return signal, ""
}
