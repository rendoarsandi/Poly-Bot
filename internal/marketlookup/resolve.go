package marketlookup

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/trading"
)

const minRelevantPositionShares = 0.0001

func ResolveMarkets(ctx context.Context, trader *trading.RealTrader, target string) ([]api.Market, string, error) {
	target = core.SanitizeString(strings.TrimSpace(target))
	rest := api.NewRestClient("")

	if target != "" {
		if LooksLikeConditionID(target) {
			market, err := marketFromConditionID(ctx, trader, target)
			if err != nil {
				return nil, "", fmt.Errorf("failed to resolve condition ID %s: %w", target, err)
			}
			return []api.Market{market}, "condition ID", nil
		}

		markets, slugErr := rest.GetMarketsByEventSlug(ctx, target)
		if slugErr == nil && len(markets) > 0 {
			return markets, "slug", nil
		}

		market, marketErr := rest.GetMarket(ctx, target)
		if marketErr == nil && market != nil {
			return []api.Market{*market}, "slug", nil
		}

		return nil, "", fmt.Errorf("failed to resolve target %q (event lookup: %v; market lookup: %v)", target, slugErr, marketErr)
	}

	positions, err := trader.ForceRefreshPositions(ctx)
	if err != nil {
		return nil, "", err
	}

	byCondition := make(map[string]api.Market)
	for _, pos := range positions {
		if pos.Size < minRelevantPositionShares || pos.ConditionID == "" {
			continue
		}

		if market, ok := marketFromPosition(pos); ok {
			if current, exists := byCondition[pos.ConditionID]; !exists || len(current.Tokens) < len(market.Tokens) {
				byCondition[pos.ConditionID] = market
			}
			continue
		}

		if pos.TokenID != "" {
			if event, err := rest.GetEventByTokenID(ctx, pos.TokenID); err == nil {
				if market, ok := marketFromEvent(pos.ConditionID, pos.Slug, event); ok {
					byCondition[pos.ConditionID] = market
					continue
				}
			}
		}

		if current, exists := byCondition[pos.ConditionID]; exists && len(current.Tokens) >= 2 {
			continue
		}

		market, err := marketFromConditionID(ctx, trader, pos.ConditionID)
		if err != nil {
			continue
		}
		if pos.Slug != "" {
			market.Slug = core.SanitizeString(pos.Slug)
		}
		byCondition[pos.ConditionID] = market
	}

	markets := make([]api.Market, 0, len(byCondition))
	for _, market := range byCondition {
		markets = append(markets, market)
	}
	sort.Slice(markets, func(i, j int) bool {
		return markets[i].Slug < markets[j].Slug
	})

	return markets, "API positions", nil
}

func LooksLikeConditionID(target string) bool {
	target = strings.TrimSpace(strings.ToLower(target))
	if !strings.HasPrefix(target, "0x") || len(target) != 66 {
		return false
	}
	for _, ch := range target[2:] {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

func marketFromConditionID(ctx context.Context, trader *trading.RealTrader, conditionID string) (api.Market, error) {
	info, err := trader.GetMarketInfo(ctx, conditionID)
	if err != nil {
		return api.Market{}, err
	}

	market := api.Market{
		ConditionID: info.ConditionID,
		Slug:        info.ConditionID,
		Active:      info.Active,
		Closed:      info.Closed,
		Tokens:      make([]api.Token, 0, len(info.Tokens)),
	}
	for _, token := range info.Tokens {
		market.Tokens = append(market.Tokens, api.Token{TokenID: token.TokenID, Outcome: core.SanitizeString(token.Outcome)})
	}
	if len(market.Tokens) == 0 {
		return api.Market{}, fmt.Errorf("market %s did not return any tokens", conditionID)
	}
	return market, nil
}

func marketFromPosition(pos trading.PositionInfo) (api.Market, bool) {
	if pos.TokenID == "" || pos.OppositeAsset == "" {
		return api.Market{}, false
	}

	market := api.Market{
		ConditionID: pos.ConditionID,
		Slug:        firstNonEmpty(core.SanitizeString(pos.Slug), pos.ConditionID),
		Active:      true,
		Tokens: []api.Token{
			{TokenID: pos.TokenID, Outcome: firstNonEmpty(core.SanitizeString(pos.Outcome), "Outcome A")},
			{TokenID: pos.OppositeAsset, Outcome: firstNonEmpty(core.SanitizeString(pos.OppositeOutcome), "Outcome B")},
		},
	}
	return market, true
}

func marketFromEvent(conditionID, fallbackSlug string, event *api.GammaEvent) (api.Market, bool) {
	if event == nil {
		return api.Market{}, false
	}

	for _, gm := range event.Markets {
		if conditionID != "" && gm.ConditionID != conditionID {
			continue
		}

		var tokenIDs []string
		if err := json.Unmarshal([]byte(gm.ClobTokenIds), &tokenIDs); err != nil || len(tokenIDs) < 2 {
			continue
		}

		var outcomes []string
		if err := json.Unmarshal([]byte(gm.Outcomes), &outcomes); err != nil || len(outcomes) < 2 {
			outcomes = []string{"Up", "Down"}
		}

		market := api.Market{
			ConditionID: gm.ConditionID,
			Slug:        firstNonEmpty(core.SanitizeString(event.Slug), core.SanitizeString(fallbackSlug), gm.ConditionID),
			Active:      gm.Active,
			Closed:      gm.Closed,
			Tokens: []api.Token{
				{TokenID: tokenIDs[0], Outcome: core.SanitizeString(outcomes[0])},
				{TokenID: tokenIDs[1], Outcome: core.SanitizeString(outcomes[1])},
			},
		}
		return market, true
	}

	return api.Market{}, false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
