package runtimecfg

import (
	"strings"

	"Market-bot/internal/botmode"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
)

func TUISettingsFromConfig(cfg *core.Config, mode core.TradingMode) paper.TUISettings {
	if cfg == nil {
		return paper.TUISettings{}
	}

	settings := paper.TUISettings{
		Exchange:                       cfg.Exchange,
		MarketSlug:                     cfg.MarketSlug,
		MaxMarkets:                     cfg.MaxMarkets,
		Timeframe:                      cfg.Timeframe,
		TradeSizingMode:                cfg.TradeSizingMode,
		TradeScaleFactor:               cfg.TradeScaleFactor,
		TradeSizeUSDC:                  cfg.TradeSizeUSDC,
		MinMarginPercent:               cfg.MinMarginPercent,
		BinanceSignalThresholdPct:      cfg.BinanceSignalThresholdPct,
		PaperArbMode:                   botmode.NormalizeArbMode(cfg.PaperArbMode),
		CopytradeTarget:                strings.TrimSpace(cfg.CopytradeTarget),
		CopytradePollIntervalMs:        cfg.CopytradePollIntervalMs,
		CopytradeSizingMode:            cfg.CopytradeSizingMode,
		CopytradeSizeUSDC:              cfg.CopytradeSizeUSDC,
		CopytradeSizeShares:            cfg.CopytradeSizeShares,
		CopytradeSizePercent:           cfg.CopytradeSizePercent,
		CopytradeMaxSlippagePct:        cfg.CopytradeMaxSlippagePct,
		LadderedTakerSizingMode:        cfg.LadderedTakerSizingMode,
		LadderedTakerSizeUSDC:          cfg.LadderedTakerSizeUSDC,
		LadderedTakerSizeShares:        cfg.LadderedTakerSizeShares,
		LadderedTakerReentryMoveCents:  cfg.LadderedTakerReentryMoveCents,
		BuyExecutionMarginFloorPercent: cfg.BuyExecutionMarginFloorPercent,
		SplitMinMarginSell:             cfg.SplitMinMarginSell,
		SplitStrategyEnabled:           botmode.SplitStrategyActive(cfg.PaperArbMode, cfg.TakerCloseMarket, cfg.Exchange, cfg.SplitStrategyEnabled),
		SplitInitialCapPct:             cfg.SplitInitialCapPct,
		SplitReplenishCapPct:           cfg.SplitReplenishCapPct,
		TradingHoursMode:               cfg.TradingHoursMode,
		MakerMergeBufferSeconds:        cfg.MakerMergeBufferSeconds,
		MakerQuoteGap:                  cfg.MakerQuoteGap,
		MakerInventoryTargetMult:       cfg.MakerInventoryTargetMult,
		MakerInventoryCapMult:          cfg.MakerInventoryCapMult,
		MakerMinQuoteValue:             cfg.MakerMinQuoteValue,
		MinAskPrice:                    cfg.MinAskPrice,
		MaxAskPrice:                    cfg.MaxAskPrice,
		MaxTradeSize:                   cfg.MaxTradeSize,
		MaxDailyLoss:                   cfg.MaxDailyLoss,
		TakerCloseMarket:               cfg.TakerCloseMarket,
		TakerCloseMarketTime:           cfg.TakerCloseMarketTime,
		TakerCloseMarketSlippage:       cfg.TakerCloseMarketSlippage,
		TakerCloseMarketMinPrice:       cfg.TakerCloseMarketMinPrice,
	}

	if mode != core.ModeReal {
		settings.PaperBalance = cfg.PaperBalance
		settings.PaperBinanceExecutionDelayMs = cfg.PaperBinanceExecutionDelayMs
	}

	return settings
}

func ApplyTUISettings(cfg *core.Config, settings paper.TUISettings, mode core.TradingMode) {
	if cfg == nil {
		return
	}

	cfg.Exchange = settings.Exchange
	cfg.MarketSlug = settings.MarketSlug
	cfg.MaxMarkets = settings.MaxMarkets
	cfg.Timeframe = settings.Timeframe
	cfg.TradeSizingMode = settings.TradeSizingMode
	cfg.TradeScaleFactor = settings.TradeScaleFactor
	cfg.TradeSizeUSDC = settings.TradeSizeUSDC
	cfg.MinMarginPercent = settings.MinMarginPercent
	cfg.BinanceSignalThresholdPct = settings.BinanceSignalThresholdPct
	cfg.PaperArbMode = botmode.NormalizeArbMode(settings.PaperArbMode)
	cfg.CopytradeTarget = strings.TrimSpace(settings.CopytradeTarget)
	cfg.CopytradePollIntervalMs = settings.CopytradePollIntervalMs
	cfg.CopytradeSizingMode = settings.CopytradeSizingMode
	cfg.CopytradeSizeUSDC = settings.CopytradeSizeUSDC
	cfg.CopytradeSizeShares = settings.CopytradeSizeShares
	cfg.CopytradeSizePercent = settings.CopytradeSizePercent
	cfg.CopytradeMaxSlippagePct = settings.CopytradeMaxSlippagePct
	cfg.LadderedTakerSizingMode = settings.LadderedTakerSizingMode
	cfg.LadderedTakerSizeUSDC = settings.LadderedTakerSizeUSDC
	cfg.LadderedTakerSizeShares = settings.LadderedTakerSizeShares
	cfg.LadderedTakerReentryMoveCents = settings.LadderedTakerReentryMoveCents
	cfg.BuyExecutionMarginFloorPercent = settings.BuyExecutionMarginFloorPercent
	cfg.SplitMinMarginSell = settings.SplitMinMarginSell
	cfg.SplitStrategyEnabled = botmode.SplitStrategyActive(settings.PaperArbMode, settings.TakerCloseMarket, settings.Exchange, settings.SplitStrategyEnabled)
	cfg.SplitInitialCapPct = settings.SplitInitialCapPct
	cfg.SplitReplenishCapPct = settings.SplitReplenishCapPct
	cfg.TradingHoursMode = settings.TradingHoursMode
	cfg.MakerMergeBufferSeconds = settings.MakerMergeBufferSeconds
	cfg.MakerQuoteGap = settings.MakerQuoteGap
	cfg.MakerInventoryTargetMult = settings.MakerInventoryTargetMult
	cfg.MakerInventoryCapMult = settings.MakerInventoryCapMult
	cfg.MakerMinQuoteValue = settings.MakerMinQuoteValue
	cfg.MinAskPrice = settings.MinAskPrice
	cfg.MaxAskPrice = settings.MaxAskPrice
	cfg.MaxTradeSize = settings.MaxTradeSize
	cfg.MaxDailyLoss = settings.MaxDailyLoss
	cfg.TakerCloseMarket = settings.TakerCloseMarket
	cfg.TakerCloseMarketTime = settings.TakerCloseMarketTime
	cfg.TakerCloseMarketSlippage = settings.TakerCloseMarketSlippage
	cfg.TakerCloseMarketMinPrice = settings.TakerCloseMarketMinPrice

	if mode != core.ModeReal {
		cfg.PaperBalance = settings.PaperBalance
		cfg.PaperBinanceExecutionDelayMs = settings.PaperBinanceExecutionDelayMs
	}

	if mode == core.ModeReal && cfg.Exchange == "kalshi" {
		cfg.MakerMergeBufferSeconds = 0
	}
}
