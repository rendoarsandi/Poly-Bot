package main

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"Market-bot/internal/api"
)

const (
	defaultBinanceSignalPolyMaxMoveCents     = 1.5
	defaultBinanceSignalPolyAdverseMoveCents = 0.75
	defaultBinanceSignalSpreadMaxCents       = 4.0
)

type realbotMidSample struct {
	Mid float64
	At  time.Time
}

type realbotDirectionalSignalSnapshot struct {
	Outcome     string
	Mid         float64
	BaselineMid float64
	DeltaCents  float64
	UpdatedAt   time.Time
	BaselineAt  time.Time
	Ready       bool
}

type realbotDirectionalSignalTracker struct {
	lookback     time.Duration
	maxBufferAge time.Duration

	mu      sync.RWMutex
	samples map[string][]realbotMidSample
}

type realbotBinanceGapSignal struct {
	TargetOutcome          string
	OppositeOutcome        string
	SignalLabel            string
	BinanceDeltaPercent    float64
	PolyTargetMoveCents    float64
	PolyOppositeMoveCents  float64
	PolyFavorableMoveCents float64
	PolyAdverseMoveCents   float64
	TargetSpreadCents      float64
}

func newRealbotDirectionalSignalTracker(lookback time.Duration, outcomes []string) *realbotDirectionalSignalTracker {
	if lookback <= 0 {
		lookback = 1500 * time.Millisecond
	}
	maxBufferAge := 30 * time.Second
	if scaled := lookback * 6; scaled > maxBufferAge {
		maxBufferAge = scaled
	}
	samples := make(map[string][]realbotMidSample, len(outcomes))
	for _, outcome := range outcomes {
		key := strings.TrimSpace(outcome)
		if key == "" {
			continue
		}
		samples[key] = nil
	}
	return &realbotDirectionalSignalTracker{
		lookback:     lookback,
		maxBufferAge: maxBufferAge,
		samples:      samples,
	}
}

func (t *realbotDirectionalSignalTracker) Record(outcome string, bid, ask float64, at time.Time) {
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
	series = append(series, realbotMidSample{Mid: mid, At: at})
	cutoff := at.Add(-t.maxBufferAge)
	trim := 0
	for trim < len(series)-1 && series[trim].At.Before(cutoff) {
		trim++
	}
	if trim > 0 {
		series = append([]realbotMidSample(nil), series[trim:]...)
	}
	t.samples[key] = series
}

func (t *realbotDirectionalSignalTracker) Snapshot(outcome string, now time.Time) realbotDirectionalSignalSnapshot {
	snap := realbotDirectionalSignalSnapshot{Outcome: strings.TrimSpace(outcome)}
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

func realbotEvaluateBinanceGapSignal(now time.Time, mapping realbotDirectionalOutcomes, tokenBids, tokenAsks map[string]float64, binanceSnap api.BinanceFuturesSignalSnapshot, polyTracker *realbotDirectionalSignalTracker, maxAge time.Duration) (realbotBinanceGapSignal, string) {
	signal := realbotBinanceGapSignal{BinanceDeltaPercent: binanceSnap.DeltaPercent}
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
	return signal, ""
}
