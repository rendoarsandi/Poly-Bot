package main

import (
	"context"
	"sort"
	"strings"
	"time"

	"Market-bot/internal/api"
	mkt "Market-bot/internal/markets"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

const realbotStartupPositionResolutionTimeout = 1500 * time.Millisecond

type realbotStartupMarketInfoReader interface {
	GetMarketInfo(ctx context.Context, conditionID string) (*api.MarketInfo, error)
}

type realbotStartupCarryMarket struct {
	MarketID    string
	Slug        string
	ConditionID string
	Outcomes    []string
	EndTime     time.Time
}

func realbotWinningOutcomeFromMarketInfo(info *api.MarketInfo) string {
	if info == nil || !info.Closed {
		return ""
	}
	for _, token := range info.Tokens {
		if token.Winner {
			return strings.TrimSpace(token.Outcome)
		}
	}
	return ""
}

func realbotStartupCarryMarketID(pos trading.PositionInfo) string {
	market := &api.Market{
		Slug:        strings.TrimSpace(pos.Slug),
		ConditionID: strings.TrimSpace(pos.ConditionID),
	}
	return mkt.ScopedMarketID("", market)
}

func realbotStartupCarryMarketSlug(pos trading.PositionInfo) string {
	if slug := strings.TrimSpace(pos.Slug); slug != "" {
		return slug
	}
	if conditionID := strings.TrimSpace(pos.ConditionID); conditionID != "" {
		return conditionID
	}
	return "UNKNOWN"
}

func realbotStartupCarryEndTime(pos trading.PositionInfo, info *api.MarketInfo) time.Time {
	if info != nil {
		if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(info.EndDateISO)); err == nil && !parsed.IsZero() {
			return parsed
		}
		if info.Closed {
			return time.Now().Add(-time.Second)
		}
	}
	if endTime, err := paper.ParseEndTimeFromSlug(strings.TrimSpace(pos.Slug)); err == nil && !endTime.IsZero() {
		return endTime
	}
	return time.Time{}
}

func realbotStartupCarryOutcomes(pos trading.PositionInfo, info *api.MarketInfo) []string {
	seen := make(map[string]struct{})
	outcomes := make([]string, 0, 2)
	appendOutcome := func(outcome string) {
		outcome = strings.TrimSpace(outcome)
		if outcome == "" {
			return
		}
		key := strings.ToLower(outcome)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		outcomes = append(outcomes, outcome)
	}

	appendOutcome(pos.Outcome)
	appendOutcome(pos.OppositeOutcome)
	if info != nil {
		for _, token := range info.Tokens {
			appendOutcome(token.Outcome)
		}
	}
	sort.Slice(outcomes, func(i, j int) bool { return outcomes[i] < outcomes[j] })
	return outcomes
}

func realbotLookupStartupCarryMarketInfo(ctx context.Context, reader realbotStartupMarketInfoReader, conditionID string) *api.MarketInfo {
	if reader == nil || strings.TrimSpace(conditionID) == "" {
		return nil
	}
	marketCtx, cancel := context.WithTimeout(ctx, realbotStartupPositionResolutionTimeout)
	defer cancel()
	info, err := reader.GetMarketInfo(marketCtx, conditionID)
	if err != nil {
		return nil
	}
	return info
}

func realbotFilterStartupCarryPositions(ctx context.Context, reader realbotStartupMarketInfoReader, positions []trading.PositionInfo) ([]trading.PositionInfo, int, float64) {
	if len(positions) == 0 {
		return nil, 0, 0
	}

	filtered := make([]trading.PositionInfo, 0, len(positions))
	if reader == nil {
		return append(filtered, positions...), 0, 0
	}

	winningByCondition := make(map[string]string)
	skippedPositions := 0
	skippedShares := 0.0

	for _, pos := range positions {
		if pos.Size <= 0.000001 {
			continue
		}

		conditionID := strings.TrimSpace(pos.ConditionID)
		outcome := strings.TrimSpace(pos.Outcome)
		if conditionID == "" || outcome == "" {
			filtered = append(filtered, pos)
			continue
		}

		winnerOutcome, lookedUp := winningByCondition[conditionID]
		if !lookedUp {
			winnerOutcome = realbotWinningOutcomeFromMarketInfo(realbotLookupStartupCarryMarketInfo(ctx, reader, conditionID))
			winningByCondition[conditionID] = winnerOutcome
		}

		if winnerOutcome != "" && !strings.EqualFold(outcome, winnerOutcome) {
			skippedPositions++
			skippedShares += pos.Size
			continue
		}

		filtered = append(filtered, pos)
	}

	return filtered, skippedPositions, skippedShares
}

func realbotStartupCarryMarkets(ctx context.Context, reader realbotStartupMarketInfoReader, positions []trading.PositionInfo) map[string]realbotStartupCarryMarket {
	if len(positions) == 0 {
		return nil
	}

	infoByCondition := make(map[string]*api.MarketInfo)
	markets := make(map[string]realbotStartupCarryMarket)

	for _, pos := range positions {
		if pos.Size <= 0.000001 {
			continue
		}

		conditionID := strings.TrimSpace(pos.ConditionID)
		if conditionID != "" {
			if _, exists := infoByCondition[conditionID]; !exists {
				infoByCondition[conditionID] = realbotLookupStartupCarryMarketInfo(ctx, reader, conditionID)
			}
		}

		info := infoByCondition[conditionID]
		marketID := realbotStartupCarryMarketID(pos)
		if strings.TrimSpace(marketID) == "" {
			continue
		}

		carry := markets[marketID]
		carry.MarketID = marketID
		if carry.Slug == "" {
			carry.Slug = realbotStartupCarryMarketSlug(pos)
		}
		if carry.ConditionID == "" {
			carry.ConditionID = conditionID
		}
		if carry.EndTime.IsZero() {
			carry.EndTime = realbotStartupCarryEndTime(pos, info)
		}

		existingOutcomes := make(map[string]struct{}, len(carry.Outcomes))
		for _, outcome := range carry.Outcomes {
			existingOutcomes[strings.ToLower(strings.TrimSpace(outcome))] = struct{}{}
		}
		for _, outcome := range realbotStartupCarryOutcomes(pos, info) {
			key := strings.ToLower(strings.TrimSpace(outcome))
			if _, exists := existingOutcomes[key]; exists {
				continue
			}
			existingOutcomes[key] = struct{}{}
			carry.Outcomes = append(carry.Outcomes, outcome)
		}
		sort.Slice(carry.Outcomes, func(i, j int) bool { return carry.Outcomes[i] < carry.Outcomes[j] })

		markets[marketID] = carry
	}

	if len(markets) == 0 {
		return nil
	}
	return markets
}
