package main

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	mkt "Market-bot/internal/markets"
	"Market-bot/internal/paper"
)

func realbotLooksLikeTerminalBook(outcomes []string, tokenBids, tokenAsks map[string]float64) bool {
	if len(outcomes) == 0 {
		return false
	}

	sawExtreme := false
	for _, outcome := range outcomes {
		bid := tokenBids[outcome]
		ask := tokenAsks[outcome]

		if bid > 0 && bid < terminalBidFloor {
			return false
		}
		if ask > 0 && ask > terminalAskCeil {
			return false
		}
		if bid >= terminalBidFloor || (ask > 0 && ask <= terminalAskCeil) {
			sawExtreme = true
		}
	}

	return sawExtreme
}

func realbotHasSaneTopOfBook(bid, ask float64) bool {
	if bid <= 0 || ask <= 0 || bid >= ask {
		return false
	}
	if bid >= terminalBidFloor || ask <= terminalAskCeil {
		return true
	}
	return (ask - bid) <= realbotMaxSaneOutcomeSpread
}

// realbotPairHasHighBid returns true if either outcome in the pair has a
// valid bid at or above the given threshold. This signals a high-price
// market regime where the complement naturally has sparse liquidity.
const realbotHighBidThreshold = 0.60

func realbotPairHasHighBid(outcomes []string, tokenBids map[string]float64) bool {
	for _, out := range outcomes {
		if tokenBids[out] >= realbotHighBidThreshold {
			return true
		}
	}
	return false
}

func realbotLocalQuoteSanityReason(outcomes []string, tokenBids, tokenAsks map[string]float64) string {
	highBidPresent := realbotPairHasHighBid(outcomes, tokenBids)

	for _, out := range outcomes {
		bid := tokenBids[out]
		ask := tokenAsks[out]
		if !realbotHasSaneTopOfBook(bid, ask) {
			if bid <= 0 || ask <= 0 {
				// In high-price regimes the complement side naturally has
				// sparse or missing asks. Tolerate a one-sided book when
				// the pair has a high bid so we don't keep clearing data.
				if highBidPresent {
					continue
				}
				return fmt.Sprintf("missing two-sided quote for %s", out)
			}
			if bid >= ask {
				return fmt.Sprintf("crossed local quote for %s (bid %.3f >= ask %.3f)", out, bid, ask)
			}
			return fmt.Sprintf("wide local spread for %s (bid %.3f ask %.3f spread %.3f > %.3f)", out, bid, ask, ask-bid, realbotMaxSaneOutcomeSpread)
		}
	}

	if len(outcomes) == 2 && !realbotLooksLikeTerminalBook(outcomes, tokenBids, tokenAsks) {
		askSum := tokenAsks[outcomes[0]] + tokenAsks[outcomes[1]]
		if !highBidPresent && askSum > realbotMaxSaneAskPairSum {
			return fmt.Sprintf("ask pair sum %.3f > %.3f", askSum, realbotMaxSaneAskPairSum)
		}
		bidSum := tokenBids[outcomes[0]] + tokenBids[outcomes[1]]
		if bidSum < realbotMinSaneBidPairSum {
			return fmt.Sprintf("bid pair sum %.3f < %.3f", bidSum, realbotMinSaneBidPairSum)
		}
	}

	return ""
}

func realbotHasSanePairQuotes(outcomes []string, tokenBids, tokenAsks map[string]float64) bool {
	return realbotLocalQuoteSanityReason(outcomes, tokenBids, tokenAsks) == ""
}

func realbotExecutionQuoteGuardAge(localQuoteMaxAge time.Duration) time.Duration {
	if localQuoteMaxAge <= 0 || localQuoteMaxAge > realbotExecutionGuardQuoteMaxAge {
		return realbotExecutionGuardQuoteMaxAge
	}
	return localQuoteMaxAge
}

func realbotBestAskFromLevels(levels []paper.MarketLevel) (float64, bool) {
	bestAsk := 1.0
	found := false
	for _, lvl := range levels {
		if lvl.Price > 0 && lvl.Price < bestAsk {
			bestAsk = lvl.Price
			found = true
		}
	}
	if !found {
		return 0, false
	}
	return bestAsk, true
}

func realbotBestBidFromLevels(levels []paper.MarketLevel) (float64, bool) {
	bestBid := 0.0
	found := false
	for _, lvl := range levels {
		if lvl.Price > bestBid {
			bestBid = lvl.Price
			found = true
		}
	}
	if !found {
		return 0, false
	}
	return bestBid, true
}

func realbotCanUseLocalBuyQuote(now time.Time, outcomes []string, tokenBids, tokenAsks map[string]float64, tokenFullAsks map[string][]paper.MarketLevel, lastPairUpdate time.Time, maxAge time.Duration) (bool, time.Duration, string) {
	for _, out := range outcomes {
		if tokenAsks[out] <= 0 {
			return false, 0, fmt.Sprintf("missing local ask for %s", out)
		}
	}
	if reason := realbotLocalQuoteSanityReason(outcomes, tokenBids, tokenAsks); reason != "" {
		return false, 0, reason
	}
	age := realbotPairQuoteAge(lastPairUpdate, now)
	if age > maxAge {
		return false, age, fmt.Sprintf("pair quote age %s > %s", age.Round(time.Millisecond), maxAge)
	}
	return true, age, ""
}

func realbotCanUseLocalSellQuote(now time.Time, outcomes []string, tokenBids, tokenAsks map[string]float64, tokenFullBids map[string][]paper.MarketLevel, lastPairUpdate time.Time, maxAge time.Duration) (bool, time.Duration, string) {
	for _, out := range outcomes {
		if tokenBids[out] <= 0 {
			return false, 0, fmt.Sprintf("missing local bid for %s", out)
		}
		if len(tokenFullBids[out]) == 0 {
			return false, 0, fmt.Sprintf("missing local bid depth for %s", out)
		}
	}
	if reason := realbotLocalQuoteSanityReason(outcomes, tokenBids, tokenAsks); reason != "" {
		return false, 0, reason
	}
	age := realbotPairQuoteAge(lastPairUpdate, now)
	if age > maxAge {
		return false, age, fmt.Sprintf("pair quote age %s > %s", age.Round(time.Millisecond), maxAge)
	}
	return true, age, ""
}

func realbotRefreshExecutionBooks(ctx context.Context, restClient *api.RestClient, market *api.Market, outcomes []string, tokenBids, tokenAsks map[string]float64, tokenFullBids, tokenFullAsks map[string][]paper.MarketLevel, quoteState map[string]realbotQuoteState, lastPairUpdate *time.Time) (time.Duration, error) {
	type quoteResult struct {
		outcome string
		bids    []paper.MarketLevel
		asks    []paper.MarketLevel
		latency time.Duration
		err     error
	}

	results := make(chan quoteResult, len(outcomes))
	var wg sync.WaitGroup
	for _, out := range outcomes {
		tokenID := mkt.GetTokenIDForOutcome(market, out)
		if tokenID == "" {
			return 0, fmt.Errorf("missing token id for outcome %s", out)
		}
		wg.Add(1)
		go func(outcome, token string) {
			defer wg.Done()
			start := time.Now()
			book, err := restClient.GetOrderBook(ctx, token)
			latency := time.Since(start)
			if err != nil {
				results <- quoteResult{outcome: outcome, latency: latency, err: err}
				return
			}
			results <- quoteResult{
				outcome: outcome,
				bids:    mkt.LevelsToPriceDepth(book.Bids, true),
				asks:    mkt.LevelsToPriceDepth(book.Asks, false),
				latency: latency,
			}
		}(out, tokenID)
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	var maxLatency time.Duration
	for res := range results {
		if res.latency > maxLatency {
			maxLatency = res.latency
		}
		if res.err != nil {
			return maxLatency, fmt.Errorf("fetching fresh order book for %s failed: %w", res.outcome, res.err)
		}
		tokenFullBids[res.outcome] = res.bids
		tokenFullAsks[res.outcome] = res.asks
		bestBid, hasBid := realbotBestBidFromLevels(res.bids)
		bestAsk, hasAsk := realbotBestAskFromLevels(res.asks)
		if !hasBid || !hasAsk || !realbotHasSaneTopOfBook(bestBid, bestAsk) {
			return maxLatency, fmt.Errorf("invalid refreshed book for %s", res.outcome)
		}
		tokenBids[res.outcome] = bestBid
		tokenAsks[res.outcome] = bestAsk
		quoteState[res.outcome] = realbotQuoteState{UpdatedAt: time.Now(), Source: "rest-exec"}
	}
	if reason := realbotLocalQuoteSanityReason(outcomes, tokenBids, tokenAsks); reason != "" {
		return maxLatency, fmt.Errorf("invalid refreshed pair quote: %s", reason)
	}
	realbotSyncPairUpdate(outcomes, tokenBids, tokenAsks, lastPairUpdate, time.Now())
	return maxLatency, nil
}

func realbotEnsureFreshBuyExecutionQuote(ctx context.Context, restClient *api.RestClient, market *api.Market, outcomes []string, tokenBids, tokenAsks map[string]float64, tokenFullBids, tokenFullAsks map[string][]paper.MarketLevel, quoteState map[string]realbotQuoteState, lastPairUpdate time.Time, localQuoteMaxAge time.Duration, pairUpdateTarget *time.Time) (source string, metric time.Duration, detail string, err error) {
	now := time.Now()
	fresh, age, reason := realbotCanUseLocalBuyQuote(now, outcomes, tokenBids, tokenAsks, tokenFullAsks, lastPairUpdate, localQuoteMaxAge)
	if fresh {
		return "local", age, "", nil
	}
	latency, refreshErr := realbotRefreshExecutionBooks(ctx, restClient, market, outcomes, tokenBids, tokenAsks, tokenFullBids, tokenFullAsks, quoteState, pairUpdateTarget)
	if refreshErr != nil {
		return "rest", latency, reason, fmt.Errorf("local quote unavailable (%s): %w", reason, refreshErr)
	}
	return "rest", latency, reason, nil
}

func realbotEnsureFreshSellExecutionQuote(ctx context.Context, restClient *api.RestClient, market *api.Market, outcomes []string, tokenBids, tokenAsks map[string]float64, tokenFullBids, tokenFullAsks map[string][]paper.MarketLevel, quoteState map[string]realbotQuoteState, lastPairUpdate time.Time, localQuoteMaxAge time.Duration, pairUpdateTarget *time.Time) (source string, metric time.Duration, detail string, err error) {
	now := time.Now()
	fresh, age, reason := realbotCanUseLocalSellQuote(now, outcomes, tokenBids, tokenAsks, tokenFullBids, lastPairUpdate, localQuoteMaxAge)
	if fresh {
		return "local", age, "", nil
	}
	latency, refreshErr := realbotRefreshExecutionBooks(ctx, restClient, market, outcomes, tokenBids, tokenAsks, tokenFullBids, tokenFullAsks, quoteState, pairUpdateTarget)
	if refreshErr != nil {
		return "rest", latency, reason, fmt.Errorf("local quote unavailable (%s): %w", reason, refreshErr)
	}
	return "rest", latency, reason, nil
}

type realbotCleanupSellQuote struct {
	SubmitPrice       float64
	BestBid           float64
	ExecutableQty     float64
	BookAge           time.Duration
	FetchLatency      time.Duration
	TotalBidLiquidity float64
}

func realbotBuildCleanupSellQuote(ctx context.Context, restClient *api.RestClient, tokenID string, requestedQty, configuredFloor float64) (realbotCleanupSellQuote, error) {
	start := time.Now()
	book, err := restClient.GetOrderBook(ctx, tokenID)
	latency := time.Since(start)
	if err != nil {
		return realbotCleanupSellQuote{}, err
	}
	age := time.Duration(0)
	if parsedAge, ageErr := api.OrderBookAgeAt(book, time.Now()); ageErr == nil {
		age = parsedAge
	}
	bids := mkt.LevelsToPriceDepth(book.Bids, true)
	bestBid, hasBid := realbotBestBidFromLevels(bids)
	if !hasBid || bestBid <= 0 {
		return realbotCleanupSellQuote{}, fmt.Errorf("no live bid found")
	}
	submitPrice := core.CleanupSellLimitPrice(configuredFloor)
	if bestBid < submitPrice {
		submitPrice = bestBid
	}
	totalBidLiquidity := 0.0
	for _, lvl := range bids {
		if lvl.Price+1e-9 >= submitPrice {
			totalBidLiquidity += lvl.Size
		}
	}
	executableQty := normalizeMarketSellShares(math.Min(requestedQty, totalBidLiquidity))
	if executableQty < minOnChainActionShares {
		return realbotCleanupSellQuote{}, fmt.Errorf("live bid liquidity %.4f below %.2f shares at $%.3f", totalBidLiquidity, minOnChainActionShares, submitPrice)
	}
	return realbotCleanupSellQuote{
		SubmitPrice:       submitPrice,
		BestBid:           bestBid,
		ExecutableQty:     executableQty,
		BookAge:           age,
		FetchLatency:      latency,
		TotalBidLiquidity: totalBidLiquidity,
	}, nil
}

func realbotMatchedAskLiquidity(asks0, asks1 []paper.MarketLevel, maxExecutionSum float64) float64 {
	return mkt.EstimateMatchedLiquidity(
		append([]paper.MarketLevel(nil), asks0...),
		append([]paper.MarketLevel(nil), asks1...),
		func(i, j int, levels []paper.MarketLevel) bool { return levels[i].Price < levels[j].Price },
		func(p1, p2 float64) bool { return p1+p2 <= maxExecutionSum },
	)
}

func realbotMatchedBidLiquidity(bids0, bids1 []paper.MarketLevel, minExecutionSum float64) float64 {
	return mkt.EstimateMatchedLiquidity(
		append([]paper.MarketLevel(nil), bids0...),
		append([]paper.MarketLevel(nil), bids1...),
		func(i, j int, levels []paper.MarketLevel) bool { return levels[i].Price > levels[j].Price },
		func(p1, p2 float64) bool { return p1+p2 >= minExecutionSum },
	)
}
