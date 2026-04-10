package main

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"Market-bot/internal/api"
	mkt "Market-bot/internal/markets"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

const (
	realbotEmbeddedPaperRedemptionLead         = 3 * time.Second
	realbotEmbeddedPaperRedemptionPollInterval = 3 * time.Second
)

var (
	realbotRedemptionChecksMu sync.Mutex
	realbotRedemptionChecks   = make(map[string]struct{})
)

func realbotTryReserveRedemptionCheck(marketID string) bool {
	marketID = strings.TrimSpace(marketID)
	if marketID == "" {
		return false
	}
	realbotRedemptionChecksMu.Lock()
	defer realbotRedemptionChecksMu.Unlock()
	if _, exists := realbotRedemptionChecks[marketID]; exists {
		return false
	}
	realbotRedemptionChecks[marketID] = struct{}{}
	return true
}

func realbotReleaseRedemptionCheck(marketID string) {
	marketID = strings.TrimSpace(marketID)
	if marketID == "" {
		return
	}
	realbotRedemptionChecksMu.Lock()
	delete(realbotRedemptionChecks, marketID)
	realbotRedemptionChecksMu.Unlock()
}

func realbotClearEngineMarketQuotes(engine *paper.Engine, marketID string, outcomes []string) {
	if engine == nil || marketID == "" {
		return
	}
	for _, outcome := range outcomes {
		outcome = strings.TrimSpace(outcome)
		if outcome == "" {
			continue
		}
		engine.UpdateMarketBidAsk(marketID, outcome, 0, 0)
	}
}

func realbotTrackedEmbeddedPaperMarkets(engine *paper.Engine) []string {
	if engine == nil {
		return nil
	}

	marketSet := make(map[string]struct{})
	for _, pos := range engine.GetPositions() {
		marketID := strings.TrimSpace(pos.MarketID)
		if marketID == "" || pos.Quantity <= 0.000001 {
			continue
		}
		marketSet[marketID] = struct{}{}
	}
	for marketID, payout := range engine.GetPendingRedemptions() {
		marketID = strings.TrimSpace(marketID)
		if marketID == "" || payout <= 0.000001 {
			continue
		}
		marketSet[marketID] = struct{}{}
	}

	if len(marketSet) == 0 {
		return nil
	}

	marketIDs := make([]string, 0, len(marketSet))
	for marketID := range marketSet {
		marketIDs = append(marketIDs, marketID)
	}
	sort.Strings(marketIDs)
	return marketIDs
}

func realbotEmbeddedPaperMarketReady(marketID string, now time.Time) bool {
	if strings.TrimSpace(marketID) == "" {
		return false
	}
	endTime, err := paper.ParseEndTimeFromSlug(marketID)
	if err != nil || endTime.IsZero() {
		return true
	}
	return !now.Add(realbotEmbeddedPaperRedemptionLead).Before(endTime)
}

func realbotStartEmbeddedPaperResolutionSweep(ctx context.Context, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI, restClient *api.RestClient, resCache *api.ResolutionCache) {
	if trader == nil || !trader.IsEmbeddedPaperMode() || engine == nil || tui == nil || restClient == nil {
		return
	}

	scan := func() {
		now := time.Now()
		for _, marketID := range realbotTrackedEmbeddedPaperMarkets(engine) {
			if !realbotEmbeddedPaperMarketReady(marketID, now) {
				continue
			}

			queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			market, err := restClient.GetMarket(queryCtx, marketID)
			cancel()
			if err != nil || market == nil {
				continue
			}

			endTime := market.EndTime
			if endTime.IsZero() {
				endTime, _ = paper.ParseEndTimeFromSlug(market.Slug)
			}
			if endTime.IsZero() {
				endTime, _ = paper.ParseEndTimeFromSlug(marketID)
			}

			outcomes := mkt.GetOutcomes(market)
			if len(outcomes) == 0 {
				for _, token := range market.Tokens {
					outcome := strings.TrimSpace(token.Outcome)
					if outcome == "" {
						continue
					}
					outcomes = append(outcomes, outcome)
				}
			}

			if !endTime.IsZero() && now.After(endTime) {
				realbotClearEngineMarketQuotes(engine, marketID, outcomes)
			}

			winner := ""
			if market.Closed {
				for _, token := range market.Tokens {
					if token.Winner {
						winner = strings.TrimSpace(token.Outcome)
						break
					}
				}
			}
			if winner != "" {
				var result *paper.RedemptionResult
				if realbotHasEnginePositionsForMarket(engine, marketID) {
					result = engine.RedeemWithDetails(marketID, winner)
				}
				settled := engine.SettlePendingRedemption(marketID)
				tui.UpdateWalletTruthResolution(marketID, true, winner)
				if settled > 0 {
					tui.SetWalletCash(engine.GetBalance())
				}
				if result != nil && (result.WinningShares > 0 || result.LosingShares > 0 || result.TotalPayout > 0 || result.TotalPnL != 0) {
					tui.AmendMostRecentRoundForMarket(marketID, result.TotalPnL, []*paper.RedemptionResult{result})
				}
				continue
			}

			if market.ConditionID == "" || len(outcomes) == 0 {
				continue
			}

			realbotLaunchRedemptionCheck(marketID, market.ConditionID, outcomes, endTime, trader, engine, tui, resCache)
		}
	}

	go func() {
		scan()
		ticker := time.NewTicker(realbotEmbeddedPaperRedemptionPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				scan()
			}
		}
	}()
}
