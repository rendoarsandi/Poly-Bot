package copytradeutil

import (
	"time"

	"Market-bot/internal/api"
)

type TradeSignalFetcher interface {
	PendingSignalsForCondition(conditionID string, since time.Time) []api.PublicTrade
	MinedSignalsForCondition(conditionID string, since time.Time) []api.PublicTrade
}

type CycleOptions struct {
	PollEvery         time.Duration
	RetryMaxAge       time.Duration
	FreshTradeOptions FreshTradeOptions
	Now               func() time.Time
}

type CycleResult struct {
	PollStartedAt time.Time
	APIReceivedAt time.Time
	PolledTrades  []api.PublicTrade
	FreshTrades   []api.PublicTrade
}

func (s *RuntimeState) PollCycle(fetcher TradeSignalFetcher, conditionID string, opts CycleOptions) CycleResult {
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	pollStartedAt := nowFn()
	apiReceivedAt := pollStartedAt
	result := CycleResult{
		PollStartedAt: pollStartedAt,
		APIReceivedAt: apiReceivedAt,
	}
	if s == nil {
		return result
	}

	if fetcher != nil {
		if since, ok := s.MarkPoll(pollStartedAt, opts.PollEvery); ok {
			minedTrades := fetcher.MinedSignalsForCondition(conditionID, since)
			pendingTrades := fetcher.PendingSignalsForCondition(conditionID, since)
			combinedTrades := MergeTrades(pendingTrades, minedTrades)
			if len(combinedTrades) > 0 {
				result.APIReceivedAt = nowFn()
				s.LastError = ""
				freshOpts := opts.FreshTradeOptions
				freshOpts.Now = result.APIReceivedAt
				freshOpts.ConditionID = conditionID
				result.PolledTrades = s.FreshTrades(combinedTrades, freshOpts)
				s.ObserveBuySignals(result.PolledTrades)
			}
		}
	}

	if retries := s.TakeRetryTrades(nowFn(), opts.RetryMaxAge); len(retries) > 0 {
		result.FreshTrades = append(result.FreshTrades, retries...)
	}
	if len(result.PolledTrades) > 0 {
		result.FreshTrades = append(result.FreshTrades, result.PolledTrades...)
	}
	return result
}
