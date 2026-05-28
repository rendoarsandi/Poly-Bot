package main

import (
	"context"
	"math"
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
	for _, outcome := range tokenMap {
		tokenFeeRates[outcome] = 0
	}
	if market != nil && market.ConditionID != "" {
		info, err := restClient.GetClobMarketInfo(ctx, market.ConditionID)
		if err == nil && info != nil {
			if trader != nil {
				trader.SetConditionNegRisk(market.ConditionID, info.NegRisk)
			}
			if info.FeeDetails != nil {
				rateBps := int(math.Round(float64(info.FeeDetails.Rate) * 10000.0))
				for _, token := range info.Tokens {
					if outcome, ok := tokenMap[token.TokenID]; ok {
						tokenFeeRates[outcome] = rateBps
						if trader != nil && trader.IsEmbeddedPaperMode() {
							trader.RegisterPaperTokenFeeCurve(token.TokenID, info.FeeDetails.Curve())
						}
					}
				}
			}
		}
	}
	if tui != nil && trader != nil && trader.IsEmbeddedPaperMode() {
		maxFeeRate := 0
		for _, rate := range tokenFeeRates {
			if rate > maxFeeRate {
				maxFeeRate = rate
			}
		}
		tui.LogEvent("[%s] ℹ️ Paper backend using Polymarket fee curve: %.2f%% coefficient (%d bps display)", marketID, float64(maxFeeRate)/100.0, maxFeeRate)
	} else if tui != nil {
		tui.LogEvent("[%s] ℹ️ Live backend using V2 match-time fees; not submitting manual fee bps", marketID)
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

	if restClient != nil && wsMgr != nil {
		restClient.SetWSActiveCallback(func() bool {
			return wsMgr.IsConnected() && wsMgr.TimeSinceLastMessage() < 15*time.Second
		})
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
