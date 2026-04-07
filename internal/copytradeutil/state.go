package copytradeutil

import (
	"time"

	"Market-bot/internal/api"
)

const copytradeSignalReplayLookback = 10 * time.Second

type RuntimeState struct {
	StartedAt            time.Time
	LastError            string
	Managed              map[string]bool
	TargetShares         map[string]float64
	TargetSeen           map[string]bool
	LastTargetPoll       map[string]time.Time
	PendingSellTarget    map[string]float64
	PendingSellPoll      map[string]time.Time
	LastTradeFetch       time.Time
	TradesSeeded         bool
	SeenTradeKeys        map[string]time.Time
	SeenTradeKeysCount   map[string]int
	RetryTrades          []api.PublicTrade
	ObservedBuySizeSum   map[string]float64
	ObservedBuySizeCount map[string]int
	LastLogAt            map[string]time.Time
	LastLogMsg           map[string]string
}

func NewRuntimeState() *RuntimeState {
	return &RuntimeState{
		StartedAt:            time.Now(),
		Managed:              make(map[string]bool),
		TargetShares:         make(map[string]float64),
		TargetSeen:           make(map[string]bool),
		LastTargetPoll:       make(map[string]time.Time),
		PendingSellTarget:    make(map[string]float64),
		PendingSellPoll:      make(map[string]time.Time),
		SeenTradeKeys:        make(map[string]time.Time),
		SeenTradeKeysCount:   make(map[string]int),
		ObservedBuySizeSum:   make(map[string]float64),
		ObservedBuySizeCount: make(map[string]int),
		LastLogAt:            make(map[string]time.Time),
		LastLogMsg:           make(map[string]string),
	}
}

func ShouldLog(state *RuntimeState, key, msg string, interval time.Duration) bool {
	if state == nil {
		return true
	}
	if interval <= 0 {
		interval = 10 * time.Second
	}
	lastMsg := state.LastLogMsg[key]
	lastAt := state.LastLogAt[key]
	if msg == lastMsg && !lastAt.IsZero() && time.Since(lastAt) < interval {
		return false
	}
	state.LastLogMsg[key] = msg
	state.LastLogAt[key] = time.Now()
	return true
}

func (s *RuntimeState) MarkPoll(now time.Time, pollEvery time.Duration) (time.Time, bool) {
	if s == nil {
		return time.Time{}, false
	}
	if pollEvery <= 0 {
		pollEvery = 2 * time.Second
	}
	if !s.LastTradeFetch.IsZero() && now.Sub(s.LastTradeFetch) < pollEvery {
		return time.Time{}, false
	}
	since := s.LastTradeFetch
	s.LastTradeFetch = now
	if !since.IsZero() {
		since = since.Add(-copytradeSignalReplayLookback)
	}
	return since, true
}

func (s *RuntimeState) ObserveBuySignal(trade api.PublicTrade) {
	if s == nil {
		return
	}
	positionState := s.PositionStateSnapshot()
	ObserveBuySignal(&positionState, trade)
	s.ApplyPositionState(positionState)
}

func (s *RuntimeState) ObserveBuySignals(trades []api.PublicTrade) {
	for _, trade := range trades {
		s.ObserveBuySignal(trade)
	}
}

func (s *RuntimeState) PositionSyncTrades(conditionID string, outcomes []string, positions []api.Position, pollTime time.Time, freshTrades []api.PublicTrade, sizingMode string) ([]api.PublicTrade, map[string]float64) {
	if s == nil {
		return nil, nil
	}
	positionState := s.PositionStateSnapshot()
	trades, deltas := PositionSyncTrades(&positionState, conditionID, outcomes, positions, pollTime, freshTrades, sizingMode)
	s.ApplyPositionState(positionState)
	return trades, deltas
}

func (s *RuntimeState) ClearPendingSell(outcome string) {
	if s == nil {
		return
	}
	positionState := s.PositionStateSnapshot()
	ClearPendingSell(&positionState, outcome)
	s.ApplyPositionState(positionState)
}

func (s *RuntimeState) TargetDelta(outcome string, targetQty float64, pollTime time.Time) (float64, bool, bool) {
	if s == nil {
		return 0, false, false
	}
	positionState := s.PositionStateSnapshot()
	delta, ready, pending := TargetDelta(&positionState, outcome, targetQty, pollTime)
	s.ApplyPositionState(positionState)
	return delta, ready, pending
}

func (s *RuntimeState) EstimatedPositionBuySignals(conditionID, outcome string, delta float64, mode string) []api.PublicTrade {
	if s == nil {
		return EstimatedPositionBuySignals(nil, conditionID, outcome, delta, mode)
	}
	positionState := s.PositionStateSnapshot()
	return EstimatedPositionBuySignals(&positionState, conditionID, outcome, delta, mode)
}

func (s *RuntimeState) BootstrapAcceptsTrade(maxAge time.Duration, trade api.PublicTrade) bool {
	if s == nil || s.StartedAt.IsZero() {
		return false
	}
	return BootstrapAcceptsTrade(s.StartedAt, maxAge, trade)
}

func (s *RuntimeState) TakeRetryTrades(now time.Time, maxAge time.Duration) []api.PublicTrade {
	if s == nil || len(s.RetryTrades) == 0 {
		return nil
	}
	retries := s.RetryTrades
	s.RetryTrades = nil
	return TakeRetryTrades(retries, now, maxAge)
}

func (s *RuntimeState) QueueRetryTrades(retries []api.PublicTrade, queueCap int) {
	if s == nil || len(retries) == 0 {
		return
	}
	s.RetryTrades = QueueRetryTrades(s.RetryTrades, retries, queueCap)
}

func (s *RuntimeState) FreshTrades(trades []api.PublicTrade, opts FreshTradeOptions) []api.PublicTrade {
	if s == nil || len(trades) == 0 {
		return nil
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	tracker := s.FreshTradeStateSnapshot()
	fresh := FreshTrades(&tracker, trades, opts)
	s.ApplyFreshTradeState(tracker)
	return fresh
}

func (s *RuntimeState) PositionStateSnapshot() PositionState {
	if s == nil {
		return PositionState{}
	}
	return PositionState{
		TradesSeeded:         s.TradesSeeded,
		TargetShares:         s.TargetShares,
		TargetSeen:           s.TargetSeen,
		LastTargetPoll:       s.LastTargetPoll,
		PendingSellTarget:    s.PendingSellTarget,
		PendingSellPoll:      s.PendingSellPoll,
		ObservedBuySizeSum:   s.ObservedBuySizeSum,
		ObservedBuySizeCount: s.ObservedBuySizeCount,
	}
}

func (s *RuntimeState) ApplyPositionState(state PositionState) {
	if s == nil {
		return
	}
	s.TargetShares = state.TargetShares
	s.TargetSeen = state.TargetSeen
	s.LastTargetPoll = state.LastTargetPoll
	s.PendingSellTarget = state.PendingSellTarget
	s.PendingSellPoll = state.PendingSellPoll
	s.ObservedBuySizeSum = state.ObservedBuySizeSum
	s.ObservedBuySizeCount = state.ObservedBuySizeCount
	s.TradesSeeded = state.TradesSeeded
}

func (s *RuntimeState) FreshTradeStateSnapshot() FreshTradeState {
	if s == nil {
		return FreshTradeState{}
	}
	return FreshTradeState{
		StartedAt:          s.StartedAt,
		TradesSeeded:       s.TradesSeeded,
		SeenTradeKeys:      s.SeenTradeKeys,
		SeenTradeKeysCount: s.SeenTradeKeysCount,
	}
}

func (s *RuntimeState) ApplyFreshTradeState(state FreshTradeState) {
	if s == nil {
		return
	}
	s.TradesSeeded = state.TradesSeeded
	s.SeenTradeKeys = state.SeenTradeKeys
	s.SeenTradeKeysCount = state.SeenTradeKeysCount
}
