package main

import (
	"context"
	"strings"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/trading"
)

const realbotStartupPositionResolutionTimeout = 1500 * time.Millisecond

type realbotStartupMarketInfoReader interface {
	GetMarketInfo(ctx context.Context, conditionID string) (*api.MarketInfo, error)
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
			winnerOutcome = ""
			marketCtx, cancel := context.WithTimeout(ctx, realbotStartupPositionResolutionTimeout)
			info, err := reader.GetMarketInfo(marketCtx, conditionID)
			cancel()
			if err == nil {
				winnerOutcome = realbotWinningOutcomeFromMarketInfo(info)
			}
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
