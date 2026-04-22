package markets

import (
	"sort"
	"strings"

	"Market-bot/internal/api"
)

// GetOutcomes returns the outcome labels for a market's tokens, sorted
// alphabetically for consistent ordering regardless of API response order.
func GetOutcomes(market *api.Market) []string {
	outcomes := make([]string, 0, len(market.Tokens))
	for _, token := range market.Tokens {
		outcomes = append(outcomes, token.Outcome)
	}
	sort.Strings(outcomes)
	return outcomes
}

// GetOrderedOutcomes returns the outcome labels for a market's tokens in their
// original API response order, which typically matches the on-chain index order.
func GetOrderedOutcomes(market *api.Market) []string {
	outcomes := make([]string, 0, len(market.Tokens))
	for _, token := range market.Tokens {
		outcomes = append(outcomes, token.Outcome)
	}
	return outcomes
}

// GetTokenIDForOutcome returns the on-chain token ID that corresponds to the
// given outcome label. Returns "" if the outcome is not found.
func GetTokenIDForOutcome(market *api.Market, outcome string) string {
	for _, t := range market.Tokens {
		if strings.EqualFold(t.Outcome, outcome) {
			return t.TokenID
		}
	}
	return ""
}
