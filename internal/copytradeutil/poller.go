package copytradeutil

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"Market-bot/internal/api"
)

type MarketSnapshot struct {
	Trades            []api.PublicTrade
	Positions         []api.Position
	TradesErr         error
	PositionsErr      error
	PollStartedAt     time.Time
	PolledAt          time.Time
	PositionsPolledAt time.Time
}

type Poller struct {
	State          *PollerState
	PendingWatcher *api.PolymarketPendingWatcher
	MinedWatcher   *api.PolymarketMinedWatcher
}

type PollerState struct {
	Wallet                 string
	ConditionIDs           []string
	Mu                     sync.Mutex
	LastPoll               time.Time
	LastPositionsRefreshAt time.Time
	Fetching               bool
	WaitCh                 chan struct{}
	LastPollStartedAt      time.Time
	LastSnapshot           api.PublicActivitySnapshot
	RateLimitUntil         time.Time
	RateLimitStreak        int
}

type ActivitySnapshotFetcher interface {
	GetPublicActivitySnapshotWithFallback(ctx context.Context, user string, markets []string, tradeLimit int, positionSizeThreshold float64, positionLimit int, cachedPositions []api.Position, cachedPositionsValid bool, tradeTimeout, positionTimeout time.Duration) api.PublicActivitySnapshot
}

func NewPoller(wallet string, conditionIDs []string) *Poller {
	state := NewPollerState(wallet, conditionIDs)
	if state == nil {
		return nil
	}
	return &Poller{State: state}
}

func NewPollerState(wallet string, conditionIDs []string) *PollerState {
	wallet = strings.TrimSpace(wallet)
	if wallet == "" {
		return nil
	}
	return &PollerState{
		Wallet:       wallet,
		ConditionIDs: NormalizeConditionIDs(conditionIDs),
	}
}

func PendingSignalsToTrades(signals []api.PendingPolymarketSignal) []api.PublicTrade {
	if len(signals) == 0 {
		return nil
	}
	trades := make([]api.PublicTrade, 0, len(signals))
	for _, sig := range signals {
		trades = append(trades, api.PublicTrade{
			ConditionID:     sig.ConditionID,
			Outcome:         sig.Outcome,
			Side:            sig.Side,
			Size:            sig.Size,
			Timestamp:       sig.ObservedAt.Unix(),
			ObservedAt:      sig.ObservedAt.Unix(),
			TransactionHash: sig.TxHash,
			Source:          "mempool",
			SignalID:        sig.SignalID,
			Slug:            sig.Slug,
		})
	}
	return trades
}

func MinedSignalsToTrades(signals []api.MinedPolymarketSignal) []api.PublicTrade {
	if len(signals) == 0 {
		return nil
	}
	trades := make([]api.PublicTrade, 0, len(signals))
	for _, sig := range signals {
		trades = append(trades, api.PublicTrade{
			ConditionID:     sig.ConditionID,
			Outcome:         sig.Outcome,
			Side:            sig.Side,
			Size:            sig.Size,
			Timestamp:       sig.BlockTimestamp,
			ObservedAt:      sig.ObservedAt.Unix(),
			TransactionHash: sig.TxHash,
			Source:          "onchain",
			SignalID:        sig.SignalID,
			Slug:            sig.Slug,
		})
	}
	return trades
}

func (p *Poller) PendingSignalsForCondition(conditionID string, since time.Time) []api.PublicTrade {
	if p == nil || p.PendingWatcher == nil {
		return nil
	}
	return PendingSignalsToTrades(p.PendingWatcher.SignalsSince(conditionID, since))
}

func (p *Poller) MinedSignalsForCondition(conditionID string, since time.Time) []api.PublicTrade {
	if p == nil || p.MinedWatcher == nil {
		return nil
	}
	return MinedSignalsToTrades(p.MinedWatcher.SignalsSince(conditionID, since))
}

func HasOnchainWatcher(p *Poller) bool {
	return p != nil && ((p.PendingWatcher != nil && p.PendingWatcher.Enabled()) || (p.MinedWatcher != nil && p.MinedWatcher.Enabled()))
}

func HasPendingWatcher(p *Poller) bool {
	return p != nil && p.PendingWatcher != nil && p.PendingWatcher.Enabled()
}

func ShouldUsePublicActivityAPI(p *Poller) bool {
	return !HasOnchainWatcher(p)
}

func (p *Poller) CachedSnapshotForCondition(conditionID string) MarketSnapshot {
	if p == nil {
		return MarketSnapshot{}
	}
	return CachedSnapshot(p.State, conditionID)
}

func (p *Poller) SnapshotForCondition(ctx context.Context, fetcher ActivitySnapshotFetcher, pollEvery time.Duration, conditionID string) (MarketSnapshot, error) {
	if p == nil {
		return MarketSnapshot{}, fmt.Errorf("copytrade poller unavailable")
	}
	return SnapshotForCondition(ctx, p.State, fetcher, pollEvery, conditionID)
}

func CachedSnapshot(state *PollerState, conditionID string) MarketSnapshot {
	if state == nil {
		return MarketSnapshot{}
	}
	return MarketSnapshot{
		Trades:            FilterTradesByCondition(state.LastSnapshot.Trades, conditionID),
		Positions:         FilterPositionsByCondition(state.LastSnapshot.Positions, conditionID),
		TradesErr:         state.LastSnapshot.TradesErr,
		PositionsErr:      state.LastSnapshot.PositionsErr,
		PollStartedAt:     state.LastPollStartedAt,
		PolledAt:          state.LastPoll,
		PositionsPolledAt: state.LastPositionsRefreshAt,
	}
}

func SnapshotForCondition(ctx context.Context, state *PollerState, fetcher ActivitySnapshotFetcher, pollEvery time.Duration, conditionID string) (MarketSnapshot, error) {
	if state == nil || fetcher == nil {
		return MarketSnapshot{}, fmt.Errorf("copytrade poller unavailable")
	}
	if pollEvery <= 0 {
		pollEvery = 2 * time.Second
	}
	conditionID = strings.TrimSpace(conditionID)

	for {
		state.Mu.Lock()
		if !state.LastPoll.IsZero() && time.Since(state.LastPoll) < pollEvery {
			snapshot := CachedSnapshot(state, conditionID)
			state.Mu.Unlock()
			return snapshot, nil
		}
		if !state.RateLimitUntil.IsZero() && time.Now().Before(state.RateLimitUntil) && !state.LastPoll.IsZero() {
			snapshot := CachedSnapshot(state, conditionID)
			state.Mu.Unlock()
			return snapshot, nil
		}
		if state.Fetching {
			waitCh := state.WaitCh
			state.Mu.Unlock()
			select {
			case <-ctx.Done():
				return MarketSnapshot{}, ctx.Err()
			case <-waitCh:
				continue
			}
		}

		state.Fetching = true
		state.WaitCh = make(chan struct{})
		wallet := state.Wallet
		conditionIDs := append([]string(nil), state.ConditionIDs...)
		pollStartedAt := time.Now()
		state.Mu.Unlock()

		tradeLimit := len(conditionIDs) * 64
		if tradeLimit < 128 {
			tradeLimit = 128
		}
		if tradeLimit > 1000 {
			tradeLimit = 1000
		}
		positionLimit := len(conditionIDs) * 8
		if positionLimit < 16 {
			positionLimit = 16
		}
		if positionLimit > 500 {
			positionLimit = 500
		}
		tradeTimeout := TradeFetchTimeout(pollEvery)
		positionTimeout := PositionFetchTimeout(pollEvery)

		state.Mu.Lock()
		cachedPositions := append([]api.Position(nil), state.LastSnapshot.Positions...)
		cachedPositionsValid := state.LastSnapshot.PositionsErr == nil && CanReusePositions(time.Now(), state.LastPositionsRefreshAt, pollEvery)
		state.Mu.Unlock()

		snapshot := fetcher.GetPublicActivitySnapshotWithFallback(
			ctx,
			wallet,
			conditionIDs,
			tradeLimit,
			minTrackedShares,
			positionLimit,
			cachedPositions,
			cachedPositionsValid,
			tradeTimeout,
			positionTimeout,
		)
		now := time.Now()

		state.Mu.Lock()
		if snapshot.TradesErr == nil {
			state.LastSnapshot.Trades = snapshot.Trades
			state.LastSnapshot.TradesErr = nil
		} else {
			state.LastSnapshot.TradesErr = snapshot.TradesErr
		}
		if snapshot.PositionsErr == nil {
			state.LastSnapshot.Positions = snapshot.Positions
			state.LastSnapshot.PositionsErr = nil
			if !snapshot.PositionsCached {
				state.LastPositionsRefreshAt = now
			}
		} else {
			state.LastSnapshot.PositionsErr = snapshot.PositionsErr
		}
		if IsRateLimited(snapshot.TradesErr) {
			state.RateLimitStreak++
			state.RateLimitUntil = now.Add(RateLimitBackoff(state.RateLimitStreak))
		} else {
			state.RateLimitStreak = 0
			state.RateLimitUntil = time.Time{}
		}
		state.LastPollStartedAt = pollStartedAt
		state.LastPoll = now
		waitCh := state.WaitCh
		state.Fetching = false
		state.WaitCh = nil
		filtered := CachedSnapshot(state, conditionID)
		state.Mu.Unlock()
		close(waitCh)

		return filtered, nil
	}
}
