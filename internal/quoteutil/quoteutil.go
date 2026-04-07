package quoteutil

import (
	"math"
	"strings"
	"time"
)

type State struct {
	UpdatedAt time.Time
	Source    string
}

type PairCheckFunc func(outcomes []string, bids, asks map[string]float64) bool
type PairReasonFunc func(outcomes []string, bids, asks map[string]float64) string

func MapsEqual(outcomes []string, bidsA, asksA, bidsB, asksB map[string]float64) bool {
	for _, outcome := range outcomes {
		if math.Abs(bidsA[outcome]-bidsB[outcome]) > 1e-9 {
			return false
		}
		if math.Abs(asksA[outcome]-asksB[outcome]) > 1e-9 {
			return false
		}
	}
	return true
}

func StorePublished(outcomes []string, srcBids, srcAsks, dstBids, dstAsks map[string]float64) {
	for _, outcome := range outcomes {
		dstBids[outcome] = srcBids[outcome]
		dstAsks[outcome] = srcAsks[outcome]
	}
}

func LatestUpdate(outcomes []string, quoteState map[string]State) (time.Time, string) {
	latest := time.Time{}
	latestSource := ""
	for _, outcome := range outcomes {
		state, ok := quoteState[outcome]
		if !ok || state.UpdatedAt.IsZero() {
			continue
		}
		if latest.IsZero() || state.UpdatedAt.After(latest) {
			latest = state.UpdatedAt
			latestSource = state.Source
		}
	}
	return latest, latestSource
}

func NormalizeDisplaySource(raw string) string {
	source := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case strings.HasPrefix(source, "rest"):
		return "REST"
	case strings.HasPrefix(source, "ws"):
		return "WS"
	default:
		return "WS"
	}
}

func HasUsableQuotes(outcomes []string, bids, asks map[string]float64, validFn, terminalFn PairCheckFunc) bool {
	return validFn(outcomes, bids, asks) || terminalFn(outcomes, bids, asks)
}

func SyncDisplayQuotes(outcomes []string, liveBids, liveAsks, displayBids, displayAsks map[string]float64, authoritative bool, validFn, terminalFn PairCheckFunc) bool {
	nextBids := make(map[string]float64, len(outcomes))
	nextAsks := make(map[string]float64, len(outcomes))
	for _, outcome := range outcomes {
		nextBids[outcome] = displayBids[outcome]
		nextAsks[outcome] = displayAsks[outcome]
	}

	switch {
	case validFn(outcomes, liveBids, liveAsks):
		StorePublished(outcomes, liveBids, liveAsks, nextBids, nextAsks)
	case terminalFn(outcomes, liveBids, liveAsks):
		for _, outcome := range outcomes {
			if liveBids[outcome] > 0 {
				nextBids[outcome] = liveBids[outcome]
			}
			if liveAsks[outcome] > 0 {
				nextAsks[outcome] = liveAsks[outcome]
			}
		}
	case authoritative:
		StorePublished(outcomes, liveBids, liveAsks, nextBids, nextAsks)
	default:
		return false
	}

	if MapsEqual(outcomes, nextBids, nextAsks, displayBids, displayAsks) {
		return false
	}
	StorePublished(outcomes, nextBids, nextAsks, displayBids, displayAsks)
	return true
}

func ShouldClearLocalPairQuotes(outcomes []string, bids, asks map[string]float64, validFn, terminalFn, highBidFn PairCheckFunc) bool {
	if validFn(outcomes, bids, asks) || terminalFn(outcomes, bids, asks) {
		return false
	}
	if highBidFn(outcomes, bids, asks) {
		return false
	}
	return true
}

func ShouldReconnectWS(outcomes []string, bids, asks map[string]float64, pairQuoteAge, staleThreshold time.Duration, terminalBookState bool, reasonFn PairReasonFunc) bool {
	if staleThreshold <= 0 {
		staleThreshold = 15 * time.Second
	}
	if terminalBookState || pairQuoteAge <= staleThreshold {
		return false
	}
	return reasonFn(outcomes, bids, asks) != ""
}

func ClampGuardAge(localQuoteMaxAge, hardMax time.Duration) time.Duration {
	if localQuoteMaxAge <= 0 || localQuoteMaxAge > hardMax {
		return hardMax
	}
	return localQuoteMaxAge
}

func PairQuoteAge(now time.Time, outcomes []string, quoteState map[string]State) time.Duration {
	maxAge := time.Duration(0)
	sawMissing := false
	for _, outcome := range outcomes {
		updatedAt := quoteState[outcome].UpdatedAt
		if updatedAt.IsZero() {
			sawMissing = true
			continue
		}
		age := now.Sub(updatedAt)
		if age > maxAge {
			maxAge = age
		}
	}
	if sawMissing {
		return 24 * time.Hour
	}
	return maxAge
}
