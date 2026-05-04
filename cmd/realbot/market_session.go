package main

import (
	"context"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	mkt "Market-bot/internal/markets"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

type realbotMarketSession struct {
	tokenMap       map[string]string
	tokenToOutcome map[string]string
	outcomeToToken map[string]string
	outcomes       []string
	tokenFeeRates  map[string]int
	wsMgr          *api.WSManager
	wsMsgChan      <-chan []byte
}

func realbotCanonicalizeMarketSession(ctx context.Context, marketID string, market *api.Market, trader *trading.RealTrader, tui *paper.TUI) {
	if market == nil || market.ConditionID == "" || trader == nil || trader.IsPaperMode() {
		return
	}
	infoCtx, infoCancel := context.WithTimeout(ctx, 3*time.Second)
	info, err := trader.GetMarketInfo(infoCtx, market.ConditionID)
	infoCancel()
	if err == nil {
		if changed, matched := realbotCanonicalizeMarketTokens(market, info); changed {
			tui.LogEvent("[%s] ℹ️ Canonicalized token mapping from CLOB market info (%d/%d tokens matched)", marketID, matched, len(market.Tokens))
		}
	}
}

func realbotBuildMarketTokenMaps(marketID string, market *api.Market, trader *trading.RealTrader) (map[string]string, map[string]string, map[string]string) {
	tokenMap := make(map[string]string)
	tokenToOutcome := make(map[string]string)
	outcomeToToken := make(map[string]string)
	if market == nil {
		return tokenMap, tokenToOutcome, outcomeToToken
	}
	for _, token := range market.Tokens {
		tokenMap[token.TokenID] = token.Outcome
		tokenToOutcome[token.TokenID] = token.Outcome
		if _, exists := outcomeToToken[token.Outcome]; !exists {
			outcomeToToken[token.Outcome] = token.TokenID
		}
		if trader != nil && trader.IsEmbeddedPaperMode() {
			trader.RegisterPaperToken(token.TokenID, marketID, token.Outcome)
		}
	}
	return tokenMap, tokenToOutcome, outcomeToToken
}

func realbotMarketAssetIDs(market *api.Market) []string {
	if market == nil {
		return nil
	}
	assetIDs := make([]string, 0, len(market.Tokens))
	for _, token := range market.Tokens {
		assetIDs = append(assetIDs, token.TokenID)
	}
	return assetIDs
}

func realbotSubscribeMarketBooks(ctx context.Context, marketID string, market *api.Market, cfg *apiConfigBridge, tui *paper.TUI) (*api.WSManager, <-chan []byte, error) {
	wsMgr := api.NewWSManager(cfg.exchange, cfg.kalshiAPIKey, cfg.kalshiPK, "")
	if err := wsMgr.Connect(ctx); err != nil {
		tui.LogEvent("[%s] ❌ WS connect failed: %v", marketID, err)
		return nil, nil, err
	}

	sub := map[string]interface{}{
		"type":                   "market",
		"assets_ids":             realbotMarketAssetIDs(market),
		"custom_feature_enabled": true,
	}
	if err := wsMgr.Subscribe(ctx, sub); err != nil {
		tui.LogEvent("[%s] ❌ Subscribe failed: %v", marketID, err)
		_ = wsMgr.Close()
		return nil, nil, err
	}
	return wsMgr, wsMgr.StartStreaming(ctx), nil
}

func realbotLoadMarketFeeRates(ctx context.Context, marketID string, market *api.Market, restClient *api.RestClient, tokenMap map[string]string, cfg *core.Config, trader *trading.RealTrader, tui *paper.TUI) map[string]int {
	tokenFeeRates := make(map[string]int, len(tokenMap))
	fallbackFeeRate := realbotConfigFeeRateBps(cfg)
	if trader != nil && trader.IsEmbeddedPaperMode() {
		for _, outcome := range tokenMap {
			tokenFeeRates[outcome] = fallbackFeeRate
		}
		tui.LogEvent("[%s] ℹ️ Paper backend using configured simulated fee: %.2f%% (%d bps)", marketID, float64(fallbackFeeRate)/100.0, fallbackFeeRate)
		return tokenFeeRates
	}
	if market != nil && market.ConditionID != "" {
		info, err := restClient.GetClobMarketInfo(ctx, market.ConditionID)
		if err == nil && info != nil {
			if trader != nil {
				trader.SetConditionNegRisk(market.ConditionID, info.NegRisk)
			}
		}
		if err == nil && info != nil && info.FeeDetails != nil {
			rate := realbotNormalizeFeeRateBps(info.FeeDetails.Rate)
			for _, outcome := range tokenMap {
				tokenFeeRates[outcome] = rate
			}
			tui.LogEvent("[%s] ℹ️ Fee rate from clob-market info: %.2f%% (%d bps)", marketID, float64(rate)/100.0, rate)
			return tokenFeeRates
		}
	}
	for tid, outcome := range tokenMap {
		var rate int
		var err error
		for attempt := 1; attempt <= 3; attempt++ {
			rate, err = restClient.GetFeeRate(ctx, tid)
			if err == nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}

		if err == nil {
			rate = realbotNormalizeFeeRateBps(rate)
			tokenFeeRates[outcome] = rate
			tui.LogEvent("[%s] ℹ️ Fee rate for %s: %.2f%% (%d bps)", marketID, outcome, float64(rate)/100.0, rate)
		} else {
			tokenFeeRates[outcome] = fallbackFeeRate
			tui.LogEvent("[%s] ⚠️ Fee fetch failed, using configured fallback %d bps", marketID, fallbackFeeRate)
		}
	}
	return tokenFeeRates
}

type apiConfigBridge struct {
	exchange     string
	kalshiAPIKey string
	kalshiPK     string
}

func realbotInitMarketSession(ctx context.Context, marketID string, market *api.Market, trader *trading.RealTrader, restClient *api.RestClient, cfg *core.Config, tui *paper.TUI) (*realbotMarketSession, error) {
	realbotCanonicalizeMarketSession(ctx, marketID, market, trader, tui)
	tokenMap, tokenToOutcome, outcomeToToken := realbotBuildMarketTokenMaps(marketID, market, trader)

	wsMgr, wsMsgChan, err := realbotSubscribeMarketBooks(ctx, marketID, market, &apiConfigBridge{
		exchange:     cfg.Exchange,
		kalshiAPIKey: cfg.KalshiAPIKey,
		kalshiPK:     cfg.KalshiPK,
	}, tui)
	if err != nil {
		return nil, err
	}

	return &realbotMarketSession{
		tokenMap:       tokenMap,
		tokenToOutcome: tokenToOutcome,
		outcomeToToken: outcomeToToken,
		outcomes:       mkt.GetOutcomes(market),
		tokenFeeRates:  realbotLoadMarketFeeRates(ctx, marketID, market, restClient, tokenMap, cfg, trader, tui),
		wsMgr:          wsMgr,
		wsMsgChan:      wsMsgChan,
	}, nil
}
