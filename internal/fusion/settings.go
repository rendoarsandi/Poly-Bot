package fusion

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"github.com/joho/godotenv"
)

const (
	fusionSettingsFile  = "fusionbot.settings.json"
	legacyFusionEnvFile = ".fusionbot.env"
)

type fusionSettings struct {
	MarketSlug                     string  `json:"market_slug"`
	Timeframe                      string  `json:"timeframe"`
	MaxMarkets                     int     `json:"max_markets"`
	TradeScaleFactor               float64 `json:"trade_scale_factor"`
	MinMarginPercent               float64 `json:"min_margin_percent"`
	BuyExecutionMarginFloorPercent float64 `json:"buy_execution_margin_floor_percent"`
	MinAskPrice                    float64 `json:"min_ask_price"`
	MaxAskPrice                    float64 `json:"max_ask_price"`
	FusionMinScorePercent          float64 `json:"fusion_min_score_percent"`
	FusionMaxSpreadPercent         float64 `json:"fusion_max_spread_percent"`
	FusionMinAskDepthShares        float64 `json:"fusion_min_ask_depth_shares"`
	FusionMaxMarketDataAgeSec      float64 `json:"fusion_max_market_data_age_sec"`
	FusionMaxBinanceDataAgeSec     float64 `json:"fusion_max_binance_data_age_sec"`
	FusionMinConsensusVotes        int     `json:"fusion_min_consensus_votes"`
}

func LoadFusionConfig() (*core.Config, error) {
	cfg := defaultFusionConfig()
	data, err := os.ReadFile(fusionSettingsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return loadLegacyFusionEnv(cfg)
		}
		return nil, fmt.Errorf("read %s: %w", fusionSettingsFile, err)
	}
	var settings fusionSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, fmt.Errorf("parse %s: %w", fusionSettingsFile, err)
	}
	applyFusionSettings(cfg, settings)
	return cfg, nil
}

func defaultFusionConfig() *core.Config {
	return &core.Config{
		TradingMode:                    core.ModePaper,
		MarketSlug:                     "ALL",
		Timeframe:                      "15m",
		MaxMarkets:                     4,
		BaseBalance:                    1000.0,
		BaseTradeSize:                  50.0,
		MinMarginPercent:               2.0,
		TradeScaleFactor:               0.05,
		FeeRateBps:                     312,
		MinAskPrice:                    0.10,
		MaxAskPrice:                    0.90,
		FusionMinScorePercent:          2.0,
		FusionMaxSpreadPercent:         8.0,
		FusionMinAskDepthShares:        60.0,
		FusionMaxMarketDataAgeSec:      3.0,
		FusionMaxBinanceDataAgeSec:     3.0,
		FusionMinConsensusVotes:        3,
		BuyExecutionMarginFloorPercent: -1.0,
		SplitMinMarginSell:             3.0,
		SplitInitialCapPct:             0.25,
		SplitReplenishCapPct:           0.50,
	}
}

func SaveFusionSettings(s paper.TUISettings) error {
	settings := fusionSettings{
		MarketSlug:                     s.MarketSlug,
		Timeframe:                      s.Timeframe,
		MaxMarkets:                     s.MaxMarkets,
		TradeScaleFactor:               s.TradeScaleFactor,
		MinMarginPercent:               s.MinMarginPercent,
		BuyExecutionMarginFloorPercent: s.BuyExecutionMarginFloorPercent,
		MinAskPrice:                    s.MinAskPrice,
		MaxAskPrice:                    s.MaxAskPrice,
		FusionMinScorePercent:          s.FusionMinScorePercent,
		FusionMaxSpreadPercent:         s.FusionMaxSpreadPercent,
		FusionMinAskDepthShares:        s.FusionMinAskDepthShares,
		FusionMaxMarketDataAgeSec:      s.FusionMaxMarketDataAgeSec,
		FusionMaxBinanceDataAgeSec:     s.FusionMaxBinanceDataAgeSec,
		FusionMinConsensusVotes:        s.FusionMinConsensusVotes,
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", fusionSettingsFile, err)
	}
	data = append(data, '\n')
	return os.WriteFile(fusionSettingsFile, data, 0644)
}

func applyFusionSettings(cfg *core.Config, s fusionSettings) {
	if s.MarketSlug != "" {
		cfg.MarketSlug = s.MarketSlug
	}
	if s.Timeframe != "" {
		cfg.Timeframe = s.Timeframe
	}
	if s.MaxMarkets > 0 {
		cfg.MaxMarkets = s.MaxMarkets
	}
	if s.TradeScaleFactor > 0 {
		cfg.TradeScaleFactor = s.TradeScaleFactor
	}
	if s.MinMarginPercent > 0 {
		cfg.MinMarginPercent = s.MinMarginPercent
	}
	cfg.BuyExecutionMarginFloorPercent = s.BuyExecutionMarginFloorPercent
	if s.MinAskPrice > 0 {
		cfg.MinAskPrice = s.MinAskPrice
	}
	if s.MaxAskPrice > 0 {
		cfg.MaxAskPrice = s.MaxAskPrice
	}
	if s.FusionMinScorePercent > 0 {
		cfg.FusionMinScorePercent = s.FusionMinScorePercent
	}
	if s.FusionMaxSpreadPercent > 0 {
		cfg.FusionMaxSpreadPercent = s.FusionMaxSpreadPercent
	}
	if s.FusionMinAskDepthShares > 0 {
		cfg.FusionMinAskDepthShares = s.FusionMinAskDepthShares
	}
	if s.FusionMaxMarketDataAgeSec > 0 {
		cfg.FusionMaxMarketDataAgeSec = s.FusionMaxMarketDataAgeSec
	}
	if s.FusionMaxBinanceDataAgeSec > 0 {
		cfg.FusionMaxBinanceDataAgeSec = s.FusionMaxBinanceDataAgeSec
	}
	if s.FusionMinConsensusVotes > 0 {
		cfg.FusionMinConsensusVotes = s.FusionMinConsensusVotes
	}
}

func loadLegacyFusionEnv(cfg *core.Config) (*core.Config, error) {
	values, err := godotenv.Read(legacyFusionEnvFile)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read %s: %w", legacyFusionEnvFile, err)
	}
	settings := fusionSettings{}
	if v := values["MARKET_SLUG"]; v != "" {
		settings.MarketSlug = v
	}
	if v := values["TIMEFRAME"]; v != "" {
		settings.Timeframe = v
	}
	if v := values["MAX_MARKETS"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			settings.MaxMarkets = n
		}
	}
	if v := values["TRADE_SCALE_FACTOR"]; v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			settings.TradeScaleFactor = n
		}
	}
	if v := values["MIN_MARGIN_PERCENT"]; v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			settings.MinMarginPercent = n
		}
	}
	if v := values["BUY_EXECUTION_MARGIN_FLOOR_PERCENT"]; v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			settings.BuyExecutionMarginFloorPercent = n
		}
	}
	if v := values["MIN_ASK_PRICE"]; v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			settings.MinAskPrice = n
		}
	}
	if v := values["MAX_ASK_PRICE"]; v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			settings.MaxAskPrice = n
		}
	}
	if v := values["FUSION_MIN_SCORE_PERCENT"]; v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			settings.FusionMinScorePercent = n
		}
	}
	if v := values["FUSION_MAX_SPREAD_PERCENT"]; v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			settings.FusionMaxSpreadPercent = n
		}
	}
	if v := values["FUSION_MIN_ASK_DEPTH_SHARES"]; v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			settings.FusionMinAskDepthShares = n
		}
	}
	if v := values["FUSION_MAX_MARKET_DATA_AGE_SEC"]; v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			settings.FusionMaxMarketDataAgeSec = n
		}
	}
	if v := values["FUSION_MAX_BINANCE_DATA_AGE_SEC"]; v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			settings.FusionMaxBinanceDataAgeSec = n
		}
	}
	if v := values["FUSION_MIN_CONSENSUS_VOTES"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			settings.FusionMinConsensusVotes = n
		}
	}
	applyFusionSettings(cfg, settings)
	return cfg, nil
}
