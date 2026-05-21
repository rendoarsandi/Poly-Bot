package main

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"Market-bot/internal/api"
	mkt "Market-bot/internal/markets"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

const realbotStartupPositionResolutionTimeout = 1500 * time.Millisecond

const (
	realbotStartupCarryMinShares     = 0.0001
	realbotStartupCarryTradePageSize = 500
	realbotStartupCarryTradeMaxPages = 4
)

var realbotStartupCarryScanAssets = []string{"btc", "eth", "sol", "xrp"}
var realbotStartupCarryScanTimeframes = []string{"5m", "15m", "1h", "4h", "1d"}

type realbotStartupMarketInfoReader interface {
	GetMarketInfo(ctx context.Context, conditionID string) (*api.MarketInfo, error)
}

type realbotStartupCarryBalanceReader interface {
	Address() string
	GetCTFBalanceFloat(ctx context.Context, tokenID string) (float64, error)
}

type realbotStartupCarryRecoverySource interface {
	GetMarketsByTimeframe(ctx context.Context, assets []string, timeframe string) ([]api.Market, error)
	GetPublicTradesPage(ctx context.Context, user string, markets []string, limit int, offset int) ([]api.PublicTrade, error)
}

type realbotStartupCarryMarket struct {
	MarketID    string
	Slug        string
	ConditionID string
	Outcomes    []string
	EndTime     time.Time
}

type realbotStartupRecoveredCarry struct {
	market          api.Market
	tokenID         string
	outcome         string
	oppositeOutcome string
	oppositeAsset   string
	shares          float64
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
	slugEndTime := time.Time{}
	if endTime, err := paper.ParseEndTimeFromSlug(strings.TrimSpace(pos.Slug)); err == nil && !endTime.IsZero() {
		slugEndTime = endTime
	}
	if info != nil {
		if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(info.EndDateISO)); err == nil && !parsed.IsZero() {
			if parsed.After(time.Now()) || info.Closed || slugEndTime.IsZero() {
				return parsed
			}
		}
		if info.Closed {
			return time.Now().Add(-time.Second)
		}
	}
	return slugEndTime
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

func realbotStartupCarryScanMarkets(ctx context.Context, source realbotStartupCarryRecoverySource) ([]api.Market, error) {
	if source == nil {
		return nil, nil
	}

	byCondition := make(map[string]api.Market)
	var lastErr error

	for _, timeframe := range realbotStartupCarryScanTimeframes {
		markets, err := source.GetMarketsByTimeframe(ctx, realbotStartupCarryScanAssets, timeframe)
		if err != nil {
			lastErr = err
			continue
		}
		for _, market := range markets {
			conditionID := strings.TrimSpace(market.ConditionID)
			if conditionID == "" {
				continue
			}
			current, exists := byCondition[conditionID]
			if !exists {
				byCondition[conditionID] = market
				continue
			}
			if strings.TrimSpace(current.Slug) == "" && strings.TrimSpace(market.Slug) != "" {
				current.Slug = market.Slug
			}
			if current.EndTime.IsZero() && !market.EndTime.IsZero() {
				current.EndTime = market.EndTime
			}
			if len(current.Tokens) == 0 && len(market.Tokens) > 0 {
				current.Tokens = append([]api.Token(nil), market.Tokens...)
			}
			byCondition[conditionID] = current
		}
	}

	if len(byCondition) == 0 {
		return nil, lastErr
	}

	conditionIDs := make([]string, 0, len(byCondition))
	for conditionID := range byCondition {
		conditionIDs = append(conditionIDs, conditionID)
	}
	sort.Strings(conditionIDs)

	markets := make([]api.Market, 0, len(conditionIDs))
	for _, conditionID := range conditionIDs {
		markets = append(markets, byCondition[conditionID])
	}
	return markets, lastErr
}

func realbotStartupCarryCounterparty(tokens []api.Token, tokenID string) (oppositeOutcome, oppositeAsset string) {
	for _, token := range tokens {
		if strings.TrimSpace(token.TokenID) == strings.TrimSpace(tokenID) {
			continue
		}
		oppositeOutcome = strings.TrimSpace(token.Outcome)
		oppositeAsset = strings.TrimSpace(token.TokenID)
		if oppositeOutcome != "" || oppositeAsset != "" {
			return oppositeOutcome, oppositeAsset
		}
	}
	return "", ""
}

func realbotStartupKnownCarryKeys(positions []trading.PositionInfo) (map[string]struct{}, map[string]struct{}) {
	knownTokens := make(map[string]struct{})
	knownOutcomes := make(map[string]struct{})
	for _, pos := range positions {
		if tokenID := strings.TrimSpace(pos.TokenID); tokenID != "" {
			knownTokens[tokenID] = struct{}{}
		}
		conditionID := strings.TrimSpace(pos.ConditionID)
		outcome := strings.ToLower(strings.TrimSpace(pos.Outcome))
		if conditionID != "" && outcome != "" {
			knownOutcomes[conditionID+"|"+outcome] = struct{}{}
		}
	}
	return knownTokens, knownOutcomes
}

func realbotRecoverStartupCarryCandidates(ctx context.Context, trader realbotStartupCarryBalanceReader, source realbotStartupCarryRecoverySource, positions []trading.PositionInfo) ([]realbotStartupRecoveredCarry, error) {
	if trader == nil || source == nil {
		return nil, nil
	}

	markets, err := realbotStartupCarryScanMarkets(ctx, source)
	if len(markets) == 0 {
		return nil, err
	}

	knownTokens, knownOutcomes := realbotStartupKnownCarryKeys(positions)
	candidates := make([]realbotStartupRecoveredCarry, 0)
	var lastErr error

	for _, market := range markets {
		for _, token := range market.Tokens {
			tokenID := strings.TrimSpace(token.TokenID)
			outcome := strings.TrimSpace(token.Outcome)
			if tokenID == "" || outcome == "" {
				continue
			}
			if _, exists := knownTokens[tokenID]; exists {
				continue
			}
			conditionID := strings.TrimSpace(market.ConditionID)
			outcomeKey := strings.ToLower(outcome)
			if conditionID != "" {
				if _, exists := knownOutcomes[conditionID+"|"+outcomeKey]; exists {
					continue
				}
			}

			shares, balErr := trader.GetCTFBalanceFloat(ctx, tokenID)
			if balErr != nil {
				lastErr = balErr
				continue
			}
			if shares < realbotStartupCarryMinShares {
				continue
			}

			oppositeOutcome, oppositeAsset := realbotStartupCarryCounterparty(market.Tokens, tokenID)
			candidates = append(candidates, realbotStartupRecoveredCarry{
				market:          market,
				tokenID:         tokenID,
				outcome:         outcome,
				oppositeOutcome: oppositeOutcome,
				oppositeAsset:   oppositeAsset,
				shares:          shares,
			})
			knownTokens[tokenID] = struct{}{}
			if conditionID != "" {
				knownOutcomes[conditionID+"|"+outcomeKey] = struct{}{}
			}
		}
	}

	if len(candidates) == 0 {
		return nil, lastErr
	}
	return candidates, lastErr
}

func realbotFetchStartupCarryTrades(ctx context.Context, source realbotStartupCarryRecoverySource, user string) ([]api.PublicTrade, error) {
	user = strings.TrimSpace(user)
	if source == nil || user == "" {
		return nil, nil
	}

	trades := make([]api.PublicTrade, 0, realbotStartupCarryTradePageSize)
	offset := 0
	var lastErr error
	for page := 0; page < realbotStartupCarryTradeMaxPages; page++ {
		pageTrades, err := source.GetPublicTradesPage(ctx, user, nil, realbotStartupCarryTradePageSize, offset)
		if err != nil {
			lastErr = err
			break
		}
		if len(pageTrades) == 0 {
			break
		}
		trades = append(trades, pageTrades...)
		offset += len(pageTrades)
		if len(pageTrades) < realbotStartupCarryTradePageSize {
			break
		}
	}
	return trades, lastErr
}

func realbotStartupTradesByToken(trades []api.PublicTrade) map[string][]api.PublicTrade {
	if len(trades) == 0 {
		return nil
	}
	byToken := make(map[string][]api.PublicTrade)
	for _, trade := range trades {
		tokenID := strings.TrimSpace(trade.Asset)
		if tokenID == "" {
			continue
		}
		byToken[tokenID] = append(byToken[tokenID], trade)
	}
	for tokenID := range byToken {
		sort.SliceStable(byToken[tokenID], func(i, j int) bool {
			return byToken[tokenID][i].Timestamp < byToken[tokenID][j].Timestamp
		})
	}
	return byToken
}

func realbotStartupCarryAvgPriceFromTrades(trades []api.PublicTrade, currentShares float64) (float64, bool) {
	const eps = 1e-6
	if currentShares <= eps || len(trades) == 0 {
		return 0, false
	}

	ordered := append([]api.PublicTrade(nil), trades...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Timestamp < ordered[j].Timestamp
	})

	shares := 0.0
	totalCost := 0.0
	for _, trade := range ordered {
		size := math.Abs(trade.Size)
		price := trade.Price
		if size <= eps || price <= 0 {
			continue
		}
		switch strings.ToUpper(strings.TrimSpace(trade.Side)) {
		case "BUY":
			shares += size
			totalCost += size * price
		case "SELL":
			if shares <= eps {
				continue
			}
			sold := math.Min(size, shares)
			avgPrice := totalCost / shares
			totalCost -= avgPrice * sold
			shares -= sold
			if shares <= eps {
				shares = 0
				totalCost = 0
			}
		}
	}

	if shares <= eps {
		return 0, false
	}
	avgPrice := totalCost / shares
	if avgPrice <= 0 {
		return 0, false
	}
	return avgPrice, shares+eps >= currentShares
}

func realbotStartupCarryFallbackAvgPrice(trades []api.PublicTrade) float64 {
	if avgPrice, ok := realbotStartupCarryAvgPriceFromTrades(trades, 0.0001); ok && avgPrice > 0 {
		return avgPrice
	}
	for i := len(trades) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(trades[i].Side), "BUY") && trades[i].Price > 0 {
			return trades[i].Price
		}
	}
	return 0.5
}

func realbotRecoverStartupCarryPositions(ctx context.Context, trader realbotStartupCarryBalanceReader, source realbotStartupCarryRecoverySource, positions []trading.PositionInfo) ([]trading.PositionInfo, int, error) {
	recovered, recoverErr := realbotRecoverStartupCarryCandidates(ctx, trader, source, positions)
	if len(recovered) == 0 {
		if len(positions) == 0 {
			return nil, 0, recoverErr
		}
		return append([]trading.PositionInfo(nil), positions...), 0, recoverErr
	}

	trades, tradesErr := realbotFetchStartupCarryTrades(ctx, source, trader.Address())
	if recoverErr == nil {
		recoverErr = tradesErr
	}
	tradesByToken := realbotStartupTradesByToken(trades)

	result := append([]trading.PositionInfo(nil), positions...)
	for _, carry := range recovered {
		avgPrice, ok := realbotStartupCarryAvgPriceFromTrades(tradesByToken[carry.tokenID], carry.shares)
		if !ok {
			avgPrice = realbotStartupCarryFallbackAvgPrice(tradesByToken[carry.tokenID])
		}
		result = append(result, trading.PositionInfo{
			TokenID:         carry.tokenID,
			Outcome:         carry.outcome,
			Size:            carry.shares,
			AvgPrice:        avgPrice,
			ConditionID:     strings.TrimSpace(carry.market.ConditionID),
			Slug:            strings.TrimSpace(carry.market.Slug),
			OppositeOutcome: carry.oppositeOutcome,
			OppositeAsset:   carry.oppositeAsset,
		})
	}
	return result, len(recovered), recoverErr
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

func realbotFilterDustStartupCarryPositions(positions []trading.PositionInfo) ([]trading.PositionInfo, int, float64) {
	if len(positions) == 0 {
		return nil, 0, 0
	}

	filtered := make([]trading.PositionInfo, 0, len(positions))
	skippedPositions := 0
	skippedShares := 0.0

	for _, pos := range positions {
		if isDustCleanupRemainder(pos.Size) {
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
