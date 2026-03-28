package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	mkt "Market-bot/internal/markets"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

type realbotCopytradeTarget struct {
	Raw    string
	Wallet string
	Label  string
}

type realbotCopytradeState struct {
	lastPoll  time.Time
	lastError string
	mirrored  map[string]bool
}

func newRealbotCopytradeState() *realbotCopytradeState {
	return &realbotCopytradeState{
		mirrored: make(map[string]bool),
	}
}

func realbotResolveCopytradeTarget(ctx context.Context, restClient *api.RestClient, liveCfg paper.TUISettings) (realbotCopytradeTarget, error) {
	raw := strings.TrimSpace(liveCfg.CopytradeTarget)
	if raw == "" {
		return realbotCopytradeTarget{}, fmt.Errorf("copytrade target is empty")
	}
	wallet, profile, err := restClient.ResolvePublicProfileTarget(ctx, raw)
	if err != nil {
		return realbotCopytradeTarget{}, err
	}

	label := wallet
	if profile != nil {
		switch {
		case strings.TrimSpace(profile.Name) != "":
			label = profile.Name
		case strings.TrimSpace(profile.Pseudonym) != "":
			label = profile.Pseudonym
		case strings.TrimSpace(profile.Referral) != "":
			label = "@" + strings.TrimPrefix(profile.Referral, "@")
		}
	}
	return realbotCopytradeTarget{
		Raw:    raw,
		Wallet: wallet,
		Label:  label,
	}, nil
}

func realbotCopytradeLabelFromHint(slug, title string) string {
	if slug = core.SanitizeString(slug); slug != "" {
		parts := strings.Split(slug, "-")
		if len(parts) > 0 && strings.TrimSpace(parts[0]) != "" {
			return strings.ToUpper(strings.TrimSpace(parts[0]))
		}
		return strings.ToUpper(slug)
	}
	title = core.SanitizeString(title)
	if title == "" {
		return "COPY"
	}
	title = strings.ToUpper(title)
	if len(title) > 12 {
		title = title[:12]
	}
	return title
}

func parseCopytradeEndTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return parsed
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed
	}
	return time.Time{}
}

func buildCopytradeMarketFromPosition(pos api.Position) *api.Market {
	if pos.ConditionID == "" || pos.TokenID == "" || pos.Outcome == "" {
		return nil
	}
	market := &api.Market{
		ConditionID: pos.ConditionID,
		Slug:        core.SanitizeString(pos.Slug),
		EndTime:     parseCopytradeEndTime(pos.EndDate),
		Tokens: []api.Token{
			{TokenID: pos.TokenID, Outcome: core.SanitizeString(pos.Outcome)},
		},
	}
	if pos.OppositeAsset != "" && pos.OppositeOutcome != "" {
		market.Tokens = append(market.Tokens, api.Token{
			TokenID: pos.OppositeAsset,
			Outcome: core.SanitizeString(pos.OppositeOutcome),
		})
	}
	return market
}

func buildCopytradeMarketFromTrade(ctx context.Context, restClient *api.RestClient, trade api.PublicTrade) *api.Market {
	if restClient == nil || trade.ConditionID == "" {
		return nil
	}
	market, err := restClient.GetMarket(ctx, trade.ConditionID)
	if err == nil && market != nil {
		return market
	}
	return nil
}

func realbotFindCopytradeMarkets(ctx context.Context, restClient *api.RestClient, wallet string, maxMarkets int) (map[string]*api.Market, error) {
	if restClient == nil {
		return nil, fmt.Errorf("rest client is nil")
	}
	if maxMarkets <= 0 {
		maxMarkets = 4
	}

	found := make(map[string]*api.Market)
	seen := make(map[string]struct{})

	addMarket := func(label string, market *api.Market) bool {
		if market == nil || market.ConditionID == "" {
			return false
		}
		if _, ok := seen[market.ConditionID]; ok {
			return false
		}
		if !market.EndTime.IsZero() {
			if time.Now().After(market.EndTime) || time.Until(market.EndTime) < 30*time.Second {
				return false
			}
		}
		if label == "" {
			label = realbotCopytradeLabelFromHint(market.Slug, "")
		}
		if _, exists := found[label]; exists {
			fingerprint := strings.TrimPrefix(strings.TrimPrefix(market.ConditionID, "0x"), "0X")
			if len(fingerprint) > 6 {
				fingerprint = fingerprint[:6]
			}
			if fingerprint == "" {
				fingerprint = "mkt"
			}
			label = label + "-" + strings.ToUpper(fingerprint)
		}
		seen[market.ConditionID] = struct{}{}
		found[label] = market
		return len(found) >= maxMarkets
	}

	positions, posErr := restClient.GetPublicPositions(ctx, wallet, nil, 0.01, maxMarkets*8)
	if posErr == nil {
		for _, pos := range positions {
			if pos.Size <= 0.01 {
				continue
			}
			if addMarket(realbotCopytradeLabelFromHint(pos.Slug, pos.Title), buildCopytradeMarketFromPosition(pos)) {
				return found, nil
			}
		}
	}

	trades, tradeErr := restClient.GetPublicTrades(ctx, wallet, nil, maxMarkets*8)
	if tradeErr == nil {
		sort.Slice(trades, func(i, j int) bool {
			return trades[i].Timestamp > trades[j].Timestamp
		})
		for _, trade := range trades {
			if addMarket(realbotCopytradeLabelFromHint(trade.Slug, trade.Title), buildCopytradeMarketFromTrade(ctx, restClient, trade)) {
				return found, nil
			}
		}
	}

	if len(found) == 0 {
		switch {
		case posErr != nil && tradeErr != nil:
			return nil, fmt.Errorf("positions: %v | trades: %v", posErr, tradeErr)
		case posErr != nil:
			return nil, posErr
		case tradeErr != nil:
			return nil, tradeErr
		}
	}
	return found, nil
}

func realbotCopytradeHeldOutcomes(positions []api.Position) map[string]api.Position {
	held := make(map[string]api.Position, len(positions))
	for _, pos := range positions {
		outcome := core.SanitizeString(pos.Outcome)
		if outcome == "" || pos.Size <= 0.01 {
			continue
		}
		held[outcome] = pos
	}
	return held
}

func realbotCanUseLocalCopytradeSellQuote(now time.Time, outcome string, tokenBids, tokenAsks map[string]float64, tokenFullBids map[string][]paper.MarketLevel, quoteState map[string]realbotQuoteState, maxAge time.Duration) (float64, string, bool) {
	bid := tokenBids[outcome]
	if bid <= 0 || bid >= 1.0 {
		return 0, fmt.Sprintf("missing local bid for %s", outcome), false
	}
	depth := tokenFullBids[outcome]
	if len(depth) == 0 {
		return 0, fmt.Sprintf("missing local bid depth for %s", outcome), false
	}
	bestBid, ok := realbotBestBidFromLevels(depth)
	if !ok || bestBid <= 0 || bestBid >= 1.0 {
		return 0, fmt.Sprintf("invalid local bid depth for %s", outcome), false
	}
	if bid-bestBid > 0.0005 {
		return 0, fmt.Sprintf("local bid %.3f mismatches depth %.3f for %s", bid, bestBid, outcome), false
	}
	state, ok := quoteState[outcome]
	if !ok || state.UpdatedAt.IsZero() {
		return 0, fmt.Sprintf("missing quote timestamp for %s", outcome), false
	}
	if age := now.Sub(state.UpdatedAt); age > maxAge {
		return 0, fmt.Sprintf("%s quote age %s > %s", outcome, age.Round(time.Millisecond), maxAge), false
	}
	ask := tokenAsks[outcome]
	if ask > 0 && !realbotHasSaneTopOfBook(bid, ask) {
		return 0, fmt.Sprintf("crossed local quote for %s (bid %.3f >= ask %.3f)", outcome, bid, ask), false
	}
	return bid, "", true
}

func realbotHandleCopytradeMarket(ctx context.Context, marketID string, market *api.Market, outcomes []string, tokenBids, tokenAsks map[string]float64, tokenFullBids, tokenFullAsks map[string][]paper.MarketLevel, quoteState map[string]realbotQuoteState, tokenFeeRates map[string]int, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI, restClient *api.RestClient, liveCfg paper.TUISettings, copytradeWallet string, state *realbotCopytradeState, entryGate *realbotEntryGate, refreshWalletTruth func(time.Duration)) {
	if restClient == nil || trader == nil || engine == nil || market == nil || state == nil {
		return
	}

	pollEvery := time.Duration(liveCfg.CopytradePollIntervalMs) * time.Millisecond
	if pollEvery <= 0 {
		pollEvery = 2 * time.Second
	}
	if !state.lastPoll.IsZero() && time.Since(state.lastPoll) < pollEvery {
		return
	}
	state.lastPoll = time.Now()

	pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	positions, err := restClient.GetPublicPositions(pollCtx, copytradeWallet, []string{market.ConditionID}, 0.01, len(outcomes)+4)
	cancel()
	if err != nil {
		if err.Error() != state.lastError {
			tui.LogEvent("[%s] ⚠️ Copytrade target poll failed: %v", marketID, err)
			state.lastError = err.Error()
		}
		return
	}
	state.lastError = ""

	held := realbotCopytradeHeldOutcomes(positions)
	if len(state.mirrored) == 0 {
		for _, outcome := range outcomes {
			if _, ok := held[outcome]; ok {
				localQty, _ := localBoughtPositionAvg(engine, marketID, outcome)
				if localQty > 0.01 {
					state.mirrored[outcome] = true
				}
			}
		}
	}

	for _, outcome := range outcomes {
		targetPos, targetHeld := held[outcome]
		localQty, avgPrice := localBoughtPositionAvg(engine, marketID, outcome)
		mirrored := state.mirrored[outcome]
		tokenID := mkt.GetTokenIDForOutcome(market, outcome)
		if tokenID == "" {
			continue
		}

		if targetHeld && localQty <= 0.01 {
			feeRate := tokenFeeRates[outcome]
			if feeRate == 0 {
				feeRate = 1000
			}
			ask := 0.0
			quoteSource := "WS"
			if localAsk, _, ok := realbotCanUseLocalTakerCloseQuote(time.Now(), outcome, tokenBids, tokenAsks, tokenFullAsks, quoteState, realbotTakerCloseLocalMaxAge); ok {
				ask = localAsk
			} else {
				quoteSource = "REST"
				restCtx, restCancel := context.WithTimeout(ctx, realbotTakerCloseRESTTimeout)
				_, restAsk, restErr := restClient.GetBestBidAsk(restCtx, tokenID)
				restCancel()
				if restErr != nil {
					tui.LogEvent("[%s] ⚠️ Copytrade buy skipped for %s: quote refresh failed: %v", marketID, outcome, restErr)
					continue
				}
				ask = restAsk
			}
			if ask <= 0 || ask >= 1.0 {
				tui.LogEvent("[%s] ⚠️ Copytrade buy skipped for %s: missing valid ask", marketID, outcome)
				continue
			}
			if ask < liveCfg.MinAskPrice || ask > liveCfg.MaxAskPrice {
				tui.LogEvent("[%s] ⚠️ Copytrade buy skipped for %s: ask $%.3f outside %.3f-%.3f", marketID, outcome, ask, liveCfg.MinAskPrice, liveCfg.MaxAskPrice)
				continue
			}
			if entryGate != nil && !entryGate.TryAcquire() {
				tui.LogEvent("[%s] ⏳ Copytrade buy paused for %s: another market is executing a live entry", marketID, outcome)
				continue
			}

			submitPrice := liveCfg.MaxAskPrice
			if submitPrice <= 0 || submitPrice >= 1.0 {
				submitPrice = ask
			}
			if submitPrice < ask {
				submitPrice = ask
			}

			budget := core.CalculateTradeSizeForMode(realbotSizingCapitalForTrade(engine, liveCfg), liveCfg.TradeScaleFactor, liveCfg.TradeSizeUSDC, liveCfg.MaxTradeSize, liveCfg.TradeSizingMode)
			requestedQty := normalizeMarketBuyShares(budget / submitPrice)
			requestedQty = realbotClampBuySharesToBudget(requestedQty, budget, submitPrice)
			if requestedQty < 1 {
				tui.LogEvent("[%s] ⚠️ Copytrade buy skipped for %s: trade budget $%.2f is too small at cap $%.3f", marketID, outcome, budget, submitPrice)
				if entryGate != nil {
					entryGate.Release()
				}
				continue
			}

			initialPosition := trader.GetLivePositionSize(tokenID)
			tui.LogEvent("[%s] 🪞 Copytrade BUY %s: target holds %.2f shares, submit %s @ cap $%.3f (%s ask $%.3f)",
				marketID, outcome, targetPos.Size, formatShareQty(requestedQty), submitPrice, quoteSource, ask)
			tradeCtx, tradeCancel := context.WithTimeout(ctx, 4*time.Second)
			exec := executeMarketOrderWithSignals(tradeCtx, trader, api.SideBuy, tokenID, outcome, submitPrice, requestedQty, feeRate, initialPosition, 2500*time.Millisecond)
			tradeCancel()
			logDirectExecutionAudit(tui, marketID, "Copytrade BUY", requestedQty, submitPrice, exec)
			if entryGate != nil {
				entryGate.Release()
			}
			if !exec.Success {
				if exec.Err != nil {
					tui.LogEvent("[%s] ❌ Copytrade buy failed for %s: %v", marketID, outcome, exec.Err)
				} else if exec.Result != nil && exec.Result.Message != "" {
					tui.LogEvent("[%s] ❌ Copytrade buy failed for %s: %s", marketID, outcome, exec.Result.Message)
				}
				continue
			}

			execQty := attributedBuyFill(exec, requestedQty, 0, false)
			if !hasConfirmedExecutedQty(api.SideBuy, execQty) {
				tui.LogEvent("[%s] ⚠️ Copytrade buy for %s lacked confirmed fill", marketID, outcome)
				continue
			}

			execPrice := venueExecutionEffectivePrice(exec)
			if execPrice <= 0 {
				execPrice = ask
			}
			execCost := reportedBuyCost(exec, execPrice, execQty, requestedQty)
			if _, buyErr := engine.BuyForMarket(marketID, outcome, execPrice, execQty); buyErr != nil {
				tui.LogEvent("[%s] ⚠️ Copytrade local buy sync failed for %s: %v", marketID, outcome, buyErr)
			}
			state.mirrored[outcome] = true
			tui.RecordOrderWithMode(marketID, outcome, "BUY", execQty, execPrice, execCost, 0.0, 0.0, "copytrade", "FILLED")
			tui.LogEvent("[%s] ✅ Copytrade bought %s %s at $%.3f", marketID, formatShareQty(execQty), outcome, execPrice)
			if refreshWalletTruth != nil {
				refreshWalletTruth(5 * time.Second)
			}
			continue
		}

		if !targetHeld && mirrored && localQty > 0.01 {
			feeRate := tokenFeeRates[outcome]
			if feeRate == 0 {
				feeRate = 1000
			}
			bid := 0.0
			quoteSource := "WS"
			if localBid, _, ok := realbotCanUseLocalCopytradeSellQuote(time.Now(), outcome, tokenBids, tokenAsks, tokenFullBids, quoteState, realbotTakerCloseLocalMaxAge); ok {
				bid = localBid
			} else {
				quoteSource = "REST"
				restCtx, restCancel := context.WithTimeout(ctx, realbotTakerCloseRESTTimeout)
				restBid, _, restErr := restClient.GetBestBidAsk(restCtx, tokenID)
				restCancel()
				if restErr != nil {
					tui.LogEvent("[%s] ⚠️ Copytrade sell skipped for %s: quote refresh failed: %v", marketID, outcome, restErr)
					continue
				}
				bid = restBid
			}
			if bid <= 0 || bid >= 1.0 {
				tui.LogEvent("[%s] ⚠️ Copytrade sell skipped for %s: missing valid bid", marketID, outcome)
				continue
			}
			if bid < liveCfg.MinAskPrice {
				tui.LogEvent("[%s] ⚠️ Copytrade sell skipped for %s: bid $%.3f below configured floor $%.3f", marketID, outcome, bid, liveCfg.MinAskPrice)
				continue
			}

			submitPrice := liveCfg.MinAskPrice
			if submitPrice <= 0 || submitPrice >= 1.0 {
				submitPrice = bid
			}
			if submitPrice > bid {
				submitPrice = bid
			}

			requestedQty := normalizeMarketSellShares(localQty)
			if requestedQty < minOnChainActionShares {
				state.mirrored[outcome] = false
				continue
			}

			initialPosition := trader.GetLivePositionSize(tokenID)
			tui.LogEvent("[%s] 🪞 Copytrade SELL %s: target exited, sell %s @ floor $%.3f (%s bid $%.3f)",
				marketID, outcome, formatShareQty(requestedQty), submitPrice, quoteSource, bid)
			tradeCtx, tradeCancel := context.WithTimeout(ctx, 4*time.Second)
			exec := executeMarketOrderWithSignals(tradeCtx, trader, api.SideSell, tokenID, outcome, submitPrice, requestedQty, feeRate, initialPosition, 2500*time.Millisecond)
			tradeCancel()
			logDirectExecutionAudit(tui, marketID, "Copytrade SELL", requestedQty, submitPrice, exec)
			if !exec.Success {
				if exec.Err != nil {
					tui.LogEvent("[%s] ❌ Copytrade sell failed for %s: %v", marketID, outcome, exec.Err)
				} else if exec.Result != nil && exec.Result.Message != "" {
					tui.LogEvent("[%s] ❌ Copytrade sell failed for %s: %s", marketID, outcome, exec.Result.Message)
				}
				continue
			}

			execQty := clampRequestedExecutionQty(exec.ExecutedQty, requestedQty)
			if !hasConfirmedExecutedQty(api.SideSell, execQty) {
				tui.LogEvent("[%s] ⚠️ Copytrade sell for %s lacked confirmed fill", marketID, outcome)
				continue
			}

			execPrice := venueExecutionEffectivePrice(exec)
			if execPrice <= 0 {
				execPrice = bid
			}
			if _, sellErr := engine.SellForMarket(marketID, outcome, execPrice, execQty); sellErr != nil {
				tui.LogEvent("[%s] ⚠️ Copytrade local sell sync failed for %s: %v", marketID, outcome, sellErr)
			}
			profit := (execPrice - avgPrice) * execQty
			tui.RecordOrderWithMode(marketID, outcome, "SELL", execQty, execPrice, execQty*execPrice, 0.0, profit, "copytrade", "FILLED")
			tui.LogEvent("[%s] ✅ Copytrade sold %s %s at $%.3f", marketID, formatShareQty(execQty), outcome, execPrice)
			if remainingQty, _ := localBoughtPositionAvg(engine, marketID, outcome); remainingQty <= 0.01 {
				state.mirrored[outcome] = false
			}
			if refreshWalletTruth != nil {
				refreshWalletTruth(5 * time.Second)
			}
		}
	}
}
