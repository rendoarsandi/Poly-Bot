package runtimecfg

import (
	"testing"

	"Market-bot/internal/botmode"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
)

func sampleConfig() *core.Config {
	return &core.Config{
		Exchange:                       "polymarket",
		MarketSlug:                     "BTC,ETH",
		MaxMarkets:                     3,
		PaperBalance:                   123.45,
		Timeframe:                      "15m",
		TradeSizingMode:                core.TradeSizingModeUSDC,
		TradeScaleFactor:               0.08,
		TradeSizeUSDC:                  12.5,
		MinMarginPercent:               1.7,
		BinanceSignalThresholdPct:      0.22,
		PaperBinanceExecutionDelayMs:   345,
		PaperArbMode:                   "maker",
		CopytradeTarget:                "  @target  ",
		CopytradePollIntervalMs:        1550,
		CopytradeSizingMode:            core.CopytradeSizingModePercent,
		CopytradeSizeUSDC:              3.2,
		CopytradeSizeShares:            7.5,
		CopytradeSizePercent:           62.0,
		CopytradeMaxSlippagePct:        2.0,
		LadderedTakerSizingMode:        core.LadderedTakerSizingModeShares,
		LadderedTakerSizeUSDC:          4.0,
		LadderedTakerSizeShares:        8.0,
		LadderedTakerReentryMoveCents:  2.5,
		BuyExecutionMarginFloorPercent: -0.02,
		SplitMinMarginSell:             4.5,
		SplitStrategyEnabled:           true,
		SplitInitialCapPct:             0.15,
		SplitReplenishCapPct:           0.25,
		TradingHoursMode:               "us open only",
		MakerMergeBufferSeconds:        42,
		MakerQuoteGap:                  0.006,
		MakerInventoryTargetMult:       2.8,
		MakerInventoryCapMult:          4.2,
		MakerMinQuoteValue:             11.0,
		MinAskPrice:                    0.11,
		MaxAskPrice:                    0.88,
		MaxTradeSize:                   30.0,
		MaxDailyLoss:                   12.0,
		TakerCloseMarket:               true,
		TakerCloseMarketTime:           7,
		TakerCloseMarketSlippage:       0.95,
		TakerCloseMarketMinPrice:       0.67,
	}
}

func TestTUISettingsFromConfigPaperIncludesPaperAndStrategyFields(t *testing.T) {
	cfg := sampleConfig()

	got := TUISettingsFromConfig(cfg, core.ModePaper)

	if got.PaperBalance != cfg.PaperBalance {
		t.Fatalf("expected PaperBalance %.2f, got %.2f", cfg.PaperBalance, got.PaperBalance)
	}
	if got.PaperBinanceExecutionDelayMs != cfg.PaperBinanceExecutionDelayMs {
		t.Fatalf("expected PaperBinanceExecutionDelayMs %d, got %d", cfg.PaperBinanceExecutionDelayMs, got.PaperBinanceExecutionDelayMs)
	}
	if got.PaperArbMode != botmode.ArbModeMaker {
		t.Fatalf("expected normalized maker arb mode, got %q", got.PaperArbMode)
	}
	if got.CopytradeTarget != "@target" {
		t.Fatalf("expected trimmed copytrade target, got %q", got.CopytradeTarget)
	}
	if got.MakerInventoryTargetMult != cfg.MakerInventoryTargetMult {
		t.Fatalf("expected MakerInventoryTargetMult %.2f, got %.2f", cfg.MakerInventoryTargetMult, got.MakerInventoryTargetMult)
	}
	if got.MakerInventoryCapMult != cfg.MakerInventoryCapMult {
		t.Fatalf("expected MakerInventoryCapMult %.2f, got %.2f", cfg.MakerInventoryCapMult, got.MakerInventoryCapMult)
	}
	if got.MakerMinQuoteValue != cfg.MakerMinQuoteValue {
		t.Fatalf("expected MakerMinQuoteValue %.2f, got %.2f", cfg.MakerMinQuoteValue, got.MakerMinQuoteValue)
	}
	if got.SplitStrategyEnabled {
		t.Fatal("expected maker mode to disable split strategy in paper settings")
	}
}

func TestApplyTUISettingsPaperRoundTripPreservesStrategyFields(t *testing.T) {
	original := sampleConfig()
	settings := TUISettingsFromConfig(original, core.ModePaper)
	cfg := &core.Config{}

	ApplyTUISettings(cfg, settings, core.ModePaper)

	if cfg.PaperBalance != original.PaperBalance {
		t.Fatalf("expected PaperBalance %.2f, got %.2f", original.PaperBalance, cfg.PaperBalance)
	}
	if cfg.PaperBinanceExecutionDelayMs != original.PaperBinanceExecutionDelayMs {
		t.Fatalf("expected PaperBinanceExecutionDelayMs %d, got %d", original.PaperBinanceExecutionDelayMs, cfg.PaperBinanceExecutionDelayMs)
	}
	if cfg.PaperArbMode != botmode.ArbModeMaker {
		t.Fatalf("expected normalized maker arb mode, got %q", cfg.PaperArbMode)
	}
	if cfg.CopytradeTarget != "@target" {
		t.Fatalf("expected trimmed copytrade target, got %q", cfg.CopytradeTarget)
	}
	if cfg.MakerInventoryTargetMult != original.MakerInventoryTargetMult {
		t.Fatalf("expected MakerInventoryTargetMult %.2f, got %.2f", original.MakerInventoryTargetMult, cfg.MakerInventoryTargetMult)
	}
	if cfg.MakerInventoryCapMult != original.MakerInventoryCapMult {
		t.Fatalf("expected MakerInventoryCapMult %.2f, got %.2f", original.MakerInventoryCapMult, cfg.MakerInventoryCapMult)
	}
	if cfg.MakerMinQuoteValue != original.MakerMinQuoteValue {
		t.Fatalf("expected MakerMinQuoteValue %.2f, got %.2f", original.MakerMinQuoteValue, cfg.MakerMinQuoteValue)
	}
	if cfg.SplitStrategyEnabled {
		t.Fatal("expected maker mode to disable split strategy on apply")
	}
	if cfg.TakerCloseMarket != original.TakerCloseMarket {
		t.Fatalf("expected TakerCloseMarket %v, got %v", original.TakerCloseMarket, cfg.TakerCloseMarket)
	}
}

func TestApplyTUISettingsPaperPreservesSplitForPlainTakerMode(t *testing.T) {
	cfg := &core.Config{}
	settings := paper.TUISettings{
		Exchange:             "polymarket",
		PaperArbMode:         botmode.ArbModeTaker,
		SplitStrategyEnabled: true,
	}

	ApplyTUISettings(cfg, settings, core.ModePaper)

	if !cfg.SplitStrategyEnabled {
		t.Fatal("expected plain taker mode to preserve split strategy")
	}
}

func TestApplyTUISettingsPaperDisablesSplitOutsidePlainTakerMode(t *testing.T) {
	tests := []paper.TUISettings{
		{Exchange: "polymarket", PaperArbMode: botmode.ArbModeCopytrade, SplitStrategyEnabled: true},
		{Exchange: "polymarket", PaperArbMode: botmode.ArbModeLadderedTaker, SplitStrategyEnabled: true},
		{Exchange: "polymarket", PaperArbMode: botmode.ArbModeTaker, TakerCloseMarket: true, SplitStrategyEnabled: true},
		{Exchange: "kalshi", PaperArbMode: botmode.ArbModeTaker, SplitStrategyEnabled: true},
	}

	for _, settings := range tests {
		cfg := &core.Config{}
		ApplyTUISettings(cfg, settings, core.ModePaper)
		if cfg.SplitStrategyEnabled {
			t.Fatalf("expected split strategy disabled for settings %+v", settings)
		}
	}
}

func TestTUISettingsFromConfigRealOmitsPaperOnlyFields(t *testing.T) {
	cfg := sampleConfig()

	got := TUISettingsFromConfig(cfg, core.ModeReal)

	if got.PaperBalance != 0 {
		t.Fatalf("expected real settings to omit PaperBalance, got %.2f", got.PaperBalance)
	}
	if got.PaperBinanceExecutionDelayMs != 0 {
		t.Fatalf("expected real settings to omit PaperBinanceExecutionDelayMs, got %d", got.PaperBinanceExecutionDelayMs)
	}
}

func TestApplyTUISettingsRealPreservesPaperOnlyFieldsAndEnforcesKalshiGuards(t *testing.T) {
	cfg := &core.Config{
		PaperBalance:                 777,
		PaperBinanceExecutionDelayMs: 999,
		SplitStrategyEnabled:         true,
		MakerMergeBufferSeconds:      42,
	}

	settings := paper.TUISettings{
		Exchange:                 "kalshi",
		MarketSlug:               "BTC",
		MaxMarkets:               1,
		Timeframe:                "15m",
		TradeSizingMode:          core.TradeSizingModePercent,
		TradeScaleFactor:         0.05,
		TradeSizeUSDC:            5,
		MinMarginPercent:         1.5,
		PaperArbMode:             "copytrade",
		CopytradeTarget:          " trader ",
		SplitStrategyEnabled:     true,
		MakerMergeBufferSeconds:  33,
		MakerInventoryTargetMult: 2.4,
		MakerInventoryCapMult:    4.6,
		MakerMinQuoteValue:       9.0,
	}

	ApplyTUISettings(cfg, settings, core.ModeReal)

	if cfg.PaperBalance != 777 {
		t.Fatalf("expected real apply to preserve PaperBalance, got %.2f", cfg.PaperBalance)
	}
	if cfg.PaperBinanceExecutionDelayMs != 999 {
		t.Fatalf("expected real apply to preserve PaperBinanceExecutionDelayMs, got %d", cfg.PaperBinanceExecutionDelayMs)
	}
	if cfg.PaperArbMode != botmode.ArbModeCopytrade {
		t.Fatalf("expected normalized copytrade arb mode, got %q", cfg.PaperArbMode)
	}
	if cfg.CopytradeTarget != "trader" {
		t.Fatalf("expected trimmed copytrade target, got %q", cfg.CopytradeTarget)
	}
	if cfg.SplitStrategyEnabled {
		t.Fatal("expected real kalshi apply to disable split strategy")
	}
	if cfg.MakerMergeBufferSeconds != 0 {
		t.Fatalf("expected real kalshi apply to zero MakerMergeBufferSeconds, got %d", cfg.MakerMergeBufferSeconds)
	}
	if cfg.MakerInventoryTargetMult != settings.MakerInventoryTargetMult {
		t.Fatalf("expected MakerInventoryTargetMult %.2f, got %.2f", settings.MakerInventoryTargetMult, cfg.MakerInventoryTargetMult)
	}
	if cfg.MakerInventoryCapMult != settings.MakerInventoryCapMult {
		t.Fatalf("expected MakerInventoryCapMult %.2f, got %.2f", settings.MakerInventoryCapMult, cfg.MakerInventoryCapMult)
	}
	if cfg.MakerMinQuoteValue != settings.MakerMinQuoteValue {
		t.Fatalf("expected MakerMinQuoteValue %.2f, got %.2f", settings.MakerMinQuoteValue, cfg.MakerMinQuoteValue)
	}
}
