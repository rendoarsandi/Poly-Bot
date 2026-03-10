package marketlookup

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/trading"
)

const (
	minRelevantPositionShares  = 0.0001
	erc1155TransferSingleTopic = "0xc3d58168c5ae7397731d063d5bbf3d657854427343f4c083240f7aacaa2d0f62"
	erc1155TransferBatchTopic  = "0x4a39dc06d4c0dbc64b70af90fd698a233a518aa5d07e595d983b8c0526c8f7fb"
)

var recentScanAssets = []string{"btc", "eth", "sol", "xrp"}
var recentScanTimeframes = []string{"15m", "5m"}

func collectMarketsByTimeframesConcurrently(ctx context.Context, timeframes []string, fetch func(context.Context, string) ([]api.Market, error)) (map[string]api.Market, error) {
	type timeframeFetchResult struct {
		markets []api.Market
		err     error
	}

	results := make(chan timeframeFetchResult, len(timeframes))
	var wg sync.WaitGroup
	for _, timeframe := range timeframes {
		timeframe := timeframe
		wg.Add(1)
		go func() {
			defer wg.Done()
			markets, err := fetch(ctx, timeframe)
			results <- timeframeFetchResult{markets: markets, err: err}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	candidates := make(map[string]api.Market)
	var lastErr error
	for result := range results {
		if result.err != nil {
			lastErr = result.err
			continue
		}
		for _, market := range result.markets {
			if market.ConditionID == "" {
				continue
			}
			candidates[market.ConditionID] = market
		}
	}
	return candidates, lastErr
}

func ResolveMarkets(ctx context.Context, trader *trading.RealTrader, polygon *api.PolygonClient, target string) ([]api.Market, string, error) {
	target = core.SanitizeString(strings.TrimSpace(target))
	rest := api.NewRestClient("")

	if target != "" {
		return resolveExplicitTarget(ctx, trader, polygon, rest, target)
	}

	byCondition := make(map[string]api.Market)
	sources := make([]string, 0, 2)

	if err := collectMarketsFromPositions(ctx, trader, rest, byCondition); err == nil {
		if len(byCondition) > 0 {
			sources = append(sources, "API positions")
		}
	} else if len(byCondition) == 0 {
		// Continue to recent wallet scan fallback below.
	}

	if added, _ := collectMarketsFromRecentWalletScan(ctx, trader, rest, byCondition); added > 0 {
		sources = append(sources, "recent wallet scan")
	}

	markets := marketsFromConditionMap(byCondition)
	if len(markets) == 0 {
		return nil, "", nil
	}

	if len(sources) == 0 {
		sources = append(sources, "recent wallet scan")
	}

	return markets, strings.Join(sources, " + "), nil
}

func resolveExplicitTarget(ctx context.Context, trader *trading.RealTrader, polygon *api.PolygonClient, rest *api.RestClient, target string) ([]api.Market, string, error) {
	if LooksLikeHex32(target) {
		txErr := fmt.Errorf("polygon client unavailable")
		if polygon != nil {
			if markets, err := marketsFromTransactionHash(ctx, polygon, rest, target); err == nil && len(markets) > 0 {
				return markets, "transaction hash", nil
			} else if err != nil {
				txErr = err
			}
		}

		market, condErr := marketFromConditionID(ctx, trader, target)
		if condErr == nil {
			return []api.Market{market}, "condition ID", nil
		}

		return nil, "", fmt.Errorf("failed to resolve hex target %q (tx lookup: %v; condition lookup: %v)", target, txErr, condErr)
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

func collectMarketsFromPositions(ctx context.Context, trader *trading.RealTrader, rest *api.RestClient, dest map[string]api.Market) error {
	positions, err := trader.ForceRefreshPositions(ctx)
	if err != nil {
		return err
	}

	for _, pos := range positions {
		if pos.Size < minRelevantPositionShares {
			continue
		}

		if pos.TokenID != "" {
			if event, err := rest.GetEventByTokenID(ctx, pos.TokenID); err == nil {
				if market, ok := marketFromEventTokenID(pos.Slug, event, pos.TokenID); ok {
					addOrMergeMarket(dest, market)
					continue
				}
			}
		}

		if market, ok := marketFromPosition(pos); ok {
			addOrMergeMarket(dest, market)
			continue
		}

		if pos.ConditionID != "" {
			market, err := marketFromConditionID(ctx, trader, pos.ConditionID)
			if err != nil {
				continue
			}
			if pos.Slug != "" {
				market.Slug = core.SanitizeString(pos.Slug)
			}
			addOrMergeMarket(dest, market)
		}
	}

	return nil
}

func collectMarketsFromRecentWalletScan(ctx context.Context, trader *trading.RealTrader, rest *api.RestClient, dest map[string]api.Market) (int, error) {
	candidates, lastErr := collectMarketsByTimeframesConcurrently(ctx, recentScanTimeframes, func(ctx context.Context, timeframe string) ([]api.Market, error) {
		return rest.GetMarketsByTimeframe(ctx, recentScanAssets, timeframe)
	})

	added := 0
	for _, market := range candidates {
		if _, exists := dest[market.ConditionID]; exists {
			continue
		}
		hasBalance, err := marketHasWalletBalance(ctx, trader, market)
		if err != nil {
			lastErr = err
			continue
		}
		if !hasBalance {
			continue
		}
		addOrMergeMarket(dest, market)
		added++
	}

	if len(candidates) == 0 && lastErr != nil {
		return added, lastErr
	}
	return added, nil
}

func marketsFromTransactionHash(ctx context.Context, polygon *api.PolygonClient, rest *api.RestClient, txHash string) ([]api.Market, error) {
	receipt, err := polygon.GetTransactionReceipt(ctx, txHash)
	if err != nil {
		return nil, err
	}
	if receipt == nil {
		return nil, fmt.Errorf("transaction %s is pending or not found", txHash)
	}

	tokenIDs := tokenIDsFromReceipt(receipt)
	if len(tokenIDs) == 0 {
		return nil, fmt.Errorf("transaction %s did not emit any CTF token transfer logs", txHash)
	}

	byCondition := make(map[string]api.Market)
	for _, tokenID := range tokenIDs {
		event, err := rest.GetEventByTokenID(ctx, tokenID)
		if err != nil {
			continue
		}
		if market, ok := marketFromEventTokenID("", event, tokenID); ok {
			addOrMergeMarket(byCondition, market)
		}
	}

	markets := marketsFromConditionMap(byCondition)
	if len(markets) == 0 {
		return nil, fmt.Errorf("transaction %s could not be mapped to a market", txHash)
	}
	return markets, nil
}

func tokenIDsFromReceipt(receipt *api.TransactionReceipt) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0)
	for _, log := range receipt.Logs {
		if !strings.EqualFold(log.Address, api.CTFContract) || len(log.Topics) == 0 {
			continue
		}

		switch strings.ToLower(log.Topics[0]) {
		case erc1155TransferSingleTopic:
			tokenID, err := tokenIDFromTransferSingleData(log.Data)
			if err == nil && tokenID != "" {
				if _, exists := seen[tokenID]; !exists {
					seen[tokenID] = struct{}{}
					result = append(result, tokenID)
				}
			}
		case erc1155TransferBatchTopic:
			ids, err := tokenIDsFromTransferBatchData(log.Data)
			if err != nil {
				continue
			}
			for _, tokenID := range ids {
				if tokenID == "" {
					continue
				}
				if _, exists := seen[tokenID]; exists {
					continue
				}
				seen[tokenID] = struct{}{}
				result = append(result, tokenID)
			}
		}
	}
	return result
}

func tokenIDFromTransferSingleData(data string) (string, error) {
	words, err := splitHexWords(data)
	if err != nil {
		return "", err
	}
	if len(words) < 1 {
		return "", fmt.Errorf("transfer single data missing token ID")
	}
	return hexWordToDecimalString(words[0])
}

func tokenIDsFromTransferBatchData(data string) ([]string, error) {
	words, err := splitHexWords(data)
	if err != nil {
		return nil, err
	}
	if len(words) < 3 {
		return nil, fmt.Errorf("transfer batch data too short")
	}

	idsOffset, err := hexWordToInt(words[0])
	if err != nil {
		return nil, err
	}
	idsIndex := idsOffset / 32
	if idsIndex < 0 || idsIndex >= len(words) {
		return nil, fmt.Errorf("invalid ids offset")
	}

	idsLen, err := hexWordToInt(words[idsIndex])
	if err != nil {
		return nil, err
	}
	start := idsIndex + 1
	end := start + idsLen
	if start < 0 || end > len(words) {
		return nil, fmt.Errorf("invalid ids length")
	}

	result := make([]string, 0, idsLen)
	for _, word := range words[start:end] {
		tokenID, err := hexWordToDecimalString(word)
		if err != nil {
			return nil, err
		}
		result = append(result, tokenID)
	}
	return result, nil
}

func splitHexWords(data string) ([]string, error) {
	data = strings.TrimPrefix(strings.TrimSpace(data), "0x")
	if data == "" {
		return nil, fmt.Errorf("empty hex data")
	}
	if len(data)%64 != 0 {
		return nil, fmt.Errorf("hex data is not word-aligned")
	}

	words := make([]string, 0, len(data)/64)
	for i := 0; i < len(data); i += 64 {
		words = append(words, data[i:i+64])
	}
	return words, nil
}

func hexWordToDecimalString(word string) (string, error) {
	n := new(big.Int)
	if _, ok := n.SetString(word, 16); !ok {
		return "", fmt.Errorf("invalid hex word")
	}
	return n.String(), nil
}

func hexWordToInt(word string) (int, error) {
	n := new(big.Int)
	if _, ok := n.SetString(word, 16); !ok {
		return 0, fmt.Errorf("invalid hex word")
	}
	if !n.IsInt64() {
		return 0, fmt.Errorf("hex word out of int64 range")
	}
	return int(n.Int64()), nil
}

func marketHasWalletBalance(ctx context.Context, trader *trading.RealTrader, market api.Market) (bool, error) {
	for _, token := range market.Tokens {
		if token.TokenID == "" {
			continue
		}
		balance, err := trader.GetCTFBalanceFloat(ctx, token.TokenID)
		if err != nil {
			return false, err
		}
		if balance >= minRelevantPositionShares {
			return true, nil
		}
	}
	return false, nil
}

func marketsFromConditionMap(byCondition map[string]api.Market) []api.Market {
	markets := make([]api.Market, 0, len(byCondition))
	for _, market := range byCondition {
		markets = append(markets, market)
	}
	sort.Slice(markets, func(i, j int) bool {
		return markets[i].Slug < markets[j].Slug
	})

	return markets
}

func LooksLikeHex32(target string) bool {
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

func LooksLikeConditionID(target string) bool {
	return LooksLikeHex32(target)
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

func marketFromEventTokenID(fallbackSlug string, event *api.GammaEvent, tokenID string) (api.Market, bool) {
	if event == nil {
		return api.Market{}, false
	}

	for _, gm := range event.Markets {
		var tokenIDs []string
		if err := json.Unmarshal([]byte(gm.ClobTokenIds), &tokenIDs); err != nil || len(tokenIDs) < 2 {
			continue
		}
		if tokenIDs[0] != tokenID && tokenIDs[1] != tokenID {
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

func addOrMergeMarket(dest map[string]api.Market, market api.Market) {
	if market.ConditionID == "" {
		return
	}
	current, exists := dest[market.ConditionID]
	if !exists {
		dest[market.ConditionID] = market
		return
	}
	if current.Slug == "" && market.Slug != "" {
		current.Slug = market.Slug
	}
	if len(current.Tokens) < len(market.Tokens) {
		current.Tokens = market.Tokens
	}
	current.Active = current.Active || market.Active
	current.Closed = current.Closed || market.Closed
	if current.Slug == "" {
		current.Slug = market.ConditionID
	}
	dest[market.ConditionID] = current
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
