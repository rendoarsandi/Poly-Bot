package core

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	t.Setenv("MARKET_SLUG", "test-market")
	t.Setenv("TRADING_MODE", "real")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	if cfg.MarketSlug != "test-market" {
		t.Errorf("Expected MarketSlug 'test-market', got '%s'", cfg.MarketSlug)
	}
	if cfg.TradingMode != ModePaper {
		t.Fatalf("expected generic LoadConfig to ignore TRADING_MODE and default to paper, got %q", cfg.TradingMode)
	}
}

func TestValidateForRealTradingRequiresCredentialsWithoutModeFlag(t *testing.T) {
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	cfg.PK = ""
	cfg.APIKey = ""
	cfg.APISecret = ""
	cfg.APIPassphrase = ""
	cfg.PolygonRPCURL = ""

	if err := cfg.ValidateForRealTrading(); err == nil {
		t.Fatal("expected ValidateForRealTrading to require credentials regardless of TradingMode")
	}
}

func TestLoadBotConfigWithPathUsesJSONRuntimeSettings(t *testing.T) {
	t.Setenv("MARKET_SLUG", "env-market")
	t.Setenv("MIN_MARGIN_PERCENT", "9.9")
	t.Setenv("POLY_API_KEY", "env-api-key")

	settingsPath := filepath.Join(t.TempDir(), "realbot.settings.json")
	data := []byte(`{
		"marketSlug": "json-market",
		"minMarginPercent": 1.5,
		"tradeSizingMode": "usdc",
		"tradeSizeUsdc": 2.4,
		"enableRawApiLog": true,
		"executionLocalQuoteMaxAgeMs": 900,
		"restFallbackQuoteAgeMs": 2500,
		"restFallbackPollIntervalMs": 700,
		"copytradeTarget": "@json-profile",
		"copytradePollIntervalMs": 1500,
		"copytradeSizingMode": "shares",
		"copytradeSizeUsdc": 3.4,
		"copytradeSizeShares": 7.5,
		"copytradeSizePercent": 12.5,
		"binanceQuoteAsset": "USDT",
		"binanceSignalThresholdPct": 0.35,
		"binanceSignalLookbackMs": 1800,
		"binanceSignalCooldownMs": 3200,
		"binanceSignalMaxAgeMs": 4500,
		"binanceSignalPolyMaxMoveCents": 1.2,
		"binanceSignalPolyAdverseMoveCents": 0.4,
		"binanceSignalSpreadMaxCents": 3.5,
		"paperArbMode": "maker",
		"makerQuoteGap": 0.004
	}`)
	if err := os.WriteFile(settingsPath, data, 0o644); err != nil {
		t.Fatalf("failed to write temp settings: %v", err)
	}

	cfg, err := loadBotConfigWithPath("realbot", settingsPath)
	if err != nil {
		t.Fatalf("LoadBotConfig failed: %v", err)
	}
	if cfg.TradingMode != ModeReal {
		t.Fatalf("expected realbot profile to force real mode, got %q", cfg.TradingMode)
	}
	if cfg.MarketSlug != "json-market" {
		t.Fatalf("expected JSON MarketSlug to override env, got %q", cfg.MarketSlug)
	}
	if cfg.MinMarginPercent != 1.5 {
		t.Fatalf("expected JSON MinMarginPercent 1.5, got %.2f", cfg.MinMarginPercent)
	}
	if cfg.TradeSizingMode != TradeSizingModeUSDC {
		t.Fatalf("expected JSON TradeSizingMode usdc, got %q", cfg.TradeSizingMode)
	}
	if cfg.TradeSizeUSDC != 2.4 {
		t.Fatalf("expected JSON TradeSizeUSDC 2.4, got %.1f", cfg.TradeSizeUSDC)
	}
	if !cfg.EnableRawAPILog {
		t.Fatal("expected JSON EnableRawAPILog to override env/default")
	}
	if cfg.ExecutionLocalQuoteMaxAgeMs != 900 {
		t.Fatalf("expected JSON ExecutionLocalQuoteMaxAgeMs 900, got %d", cfg.ExecutionLocalQuoteMaxAgeMs)
	}
	if cfg.RestFallbackQuoteAgeMs != 2500 {
		t.Fatalf("expected JSON RestFallbackQuoteAgeMs 2500, got %d", cfg.RestFallbackQuoteAgeMs)
	}
	if cfg.RestFallbackPollIntervalMs != 700 {
		t.Fatalf("expected JSON RestFallbackPollIntervalMs 700, got %d", cfg.RestFallbackPollIntervalMs)
	}
	if cfg.CopytradeTarget != "@json-profile" {
		t.Fatalf("expected JSON CopytradeTarget @json-profile, got %q", cfg.CopytradeTarget)
	}
	if cfg.CopytradePollIntervalMs != 1500 {
		t.Fatalf("expected JSON CopytradePollIntervalMs 1500, got %d", cfg.CopytradePollIntervalMs)
	}
	if cfg.CopytradeSizingMode != CopytradeSizingModeShares {
		t.Fatalf("expected JSON CopytradeSizingMode shares, got %q", cfg.CopytradeSizingMode)
	}
	if cfg.CopytradeSizeUSDC != 3.4 {
		t.Fatalf("expected JSON CopytradeSizeUSDC 3.4, got %.1f", cfg.CopytradeSizeUSDC)
	}
	if cfg.CopytradeSizeShares != 7.5 {
		t.Fatalf("expected JSON CopytradeSizeShares 7.5, got %.1f", cfg.CopytradeSizeShares)
	}
	if cfg.CopytradeSizePercent != 12.5 {
		t.Fatalf("expected JSON CopytradeSizePercent 12.5, got %.1f", cfg.CopytradeSizePercent)
	}
	if cfg.BinanceQuoteAsset != "USDT" {
		t.Fatalf("expected JSON BinanceQuoteAsset USDT, got %q", cfg.BinanceQuoteAsset)
	}
	if cfg.BinanceSignalThresholdPct != 0.35 {
		t.Fatalf("expected JSON BinanceSignalThresholdPct 0.35, got %.2f", cfg.BinanceSignalThresholdPct)
	}
	if cfg.BinanceSignalLookbackMs != 1800 {
		t.Fatalf("expected JSON BinanceSignalLookbackMs 1800, got %d", cfg.BinanceSignalLookbackMs)
	}
	if cfg.BinanceSignalCooldownMs != 3200 {
		t.Fatalf("expected JSON BinanceSignalCooldownMs 3200, got %d", cfg.BinanceSignalCooldownMs)
	}
	if cfg.BinanceSignalMaxAgeMs != 4500 {
		t.Fatalf("expected JSON BinanceSignalMaxAgeMs 4500, got %d", cfg.BinanceSignalMaxAgeMs)
	}
	if cfg.BinanceSignalPolyMaxMoveCents != 1.2 {
		t.Fatalf("expected JSON BinanceSignalPolyMaxMoveCents 1.2, got %.2f", cfg.BinanceSignalPolyMaxMoveCents)
	}
	if cfg.BinanceSignalPolyAdverseMoveCents != 0.4 {
		t.Fatalf("expected JSON BinanceSignalPolyAdverseMoveCents 0.4, got %.2f", cfg.BinanceSignalPolyAdverseMoveCents)
	}
	if cfg.BinanceSignalSpreadMaxCents != 3.5 {
		t.Fatalf("expected JSON BinanceSignalSpreadMaxCents 3.5, got %.2f", cfg.BinanceSignalSpreadMaxCents)
	}
	if cfg.PaperArbMode != "maker" {
		t.Fatalf("expected JSON PaperArbMode maker, got %q", cfg.PaperArbMode)
	}
	if cfg.MakerQuoteGap != 0.004 {
		t.Fatalf("expected JSON MakerQuoteGap 0.004, got %.3f", cfg.MakerQuoteGap)
	}
	if cfg.TradingHoursMode != "weekdays trade only" {
		t.Fatal("expected missing JSON tradingHoursMode to keep default weekdays trade only")
	}
	if cfg.APIKey != "env-api-key" {
		t.Fatalf("expected env secret to remain loaded, got %q", cfg.APIKey)
	}
}

func TestSaveSettingsWritesBotJSON(t *testing.T) {
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	cfg.settingsProfile = "paperbot"
	cfg.settingsPath = filepath.Join(t.TempDir(), "paperbot.settings.json")
	cfg.MarketSlug = "paper-json"
	cfg.MinMarginPercent = 2.75
	cfg.TradeSizingMode = TradeSizingModeUSDC
	cfg.TradeSizeUSDC = 2.3
	cfg.EnableRawAPILog = true
	cfg.ExecutionLocalQuoteMaxAgeMs = 850
	cfg.RestFallbackQuoteAgeMs = 2800
	cfg.RestFallbackPollIntervalMs = 900
	cfg.CopytradeTarget = "0x1234567890abcdef1234567890abcdef12345678"
	cfg.CopytradePollIntervalMs = 1750
	cfg.CopytradeSizingMode = CopytradeSizingModeShares
	cfg.CopytradeSizeUSDC = 4.2
	cfg.CopytradeSizeShares = 6.5
	cfg.CopytradeSizePercent = 10.0
	cfg.BinanceQuoteAsset = "USDT"
	cfg.BinanceSignalThresholdPct = 0.45
	cfg.BinanceSignalLookbackMs = 2100
	cfg.BinanceSignalCooldownMs = 2800
	cfg.BinanceSignalMaxAgeMs = 4200
	cfg.BinanceSignalPolyMaxMoveCents = 1.4
	cfg.BinanceSignalPolyAdverseMoveCents = 0.5
	cfg.BinanceSignalSpreadMaxCents = 3.0
	cfg.PaperArbMode = "maker"
	cfg.MakerQuoteGap = 0.005
	cfg.TradingHoursMode = "off"

	if err := cfg.SaveSettings(); err != nil {
		t.Fatalf("SaveSettings failed: %v", err)
	}

	data, err := os.ReadFile(cfg.settingsPath)
	if err != nil {
		t.Fatalf("failed to read saved settings: %v", err)
	}
	var settings RuntimeSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("failed to decode saved settings JSON: %v", err)
	}
	if settings.MarketSlug != "paper-json" {
		t.Fatalf("expected saved MarketSlug paper-json, got %q", settings.MarketSlug)
	}
	if settings.MinMarginPercent != 2.75 {
		t.Fatalf("expected saved MinMarginPercent 2.75, got %.2f", settings.MinMarginPercent)
	}
	if settings.TradeSizingMode != TradeSizingModeUSDC {
		t.Fatalf("expected saved TradeSizingMode usdc, got %q", settings.TradeSizingMode)
	}
	if settings.TradeSizeUSDC != 2.3 {
		t.Fatalf("expected saved TradeSizeUSDC 2.3, got %.1f", settings.TradeSizeUSDC)
	}
	if !settings.EnableRawAPILog {
		t.Fatal("expected saved EnableRawAPILog true")
	}
	if settings.ExecutionLocalQuoteMaxAgeMs != 850 {
		t.Fatalf("expected saved ExecutionLocalQuoteMaxAgeMs 850, got %d", settings.ExecutionLocalQuoteMaxAgeMs)
	}
	if settings.RestFallbackQuoteAgeMs != 2800 {
		t.Fatalf("expected saved RestFallbackQuoteAgeMs 2800, got %d", settings.RestFallbackQuoteAgeMs)
	}
	if settings.RestFallbackPollIntervalMs != 900 {
		t.Fatalf("expected saved RestFallbackPollIntervalMs 900, got %d", settings.RestFallbackPollIntervalMs)
	}
	if settings.CopytradeTarget != "0x1234567890abcdef1234567890abcdef12345678" {
		t.Fatalf("expected saved CopytradeTarget to persist, got %q", settings.CopytradeTarget)
	}
	if settings.CopytradePollIntervalMs != 1750 {
		t.Fatalf("expected saved CopytradePollIntervalMs 1750, got %d", settings.CopytradePollIntervalMs)
	}
	if settings.CopytradeSizingMode != CopytradeSizingModeShares {
		t.Fatalf("expected saved CopytradeSizingMode shares, got %q", settings.CopytradeSizingMode)
	}
	if settings.CopytradeSizeUSDC != 4.2 {
		t.Fatalf("expected saved CopytradeSizeUSDC 4.2, got %.1f", settings.CopytradeSizeUSDC)
	}
	if settings.CopytradeSizeShares != 6.5 {
		t.Fatalf("expected saved CopytradeSizeShares 6.5, got %.1f", settings.CopytradeSizeShares)
	}
	if settings.CopytradeSizePercent != 10.0 {
		t.Fatalf("expected saved CopytradeSizePercent 10.0, got %.1f", settings.CopytradeSizePercent)
	}
	if settings.BinanceQuoteAsset != "USDT" {
		t.Fatalf("expected saved BinanceQuoteAsset USDT, got %q", settings.BinanceQuoteAsset)
	}
	if settings.BinanceSignalThresholdPct != 0.45 {
		t.Fatalf("expected saved BinanceSignalThresholdPct 0.45, got %.2f", settings.BinanceSignalThresholdPct)
	}
	if settings.BinanceSignalLookbackMs != 2100 {
		t.Fatalf("expected saved BinanceSignalLookbackMs 2100, got %d", settings.BinanceSignalLookbackMs)
	}
	if settings.BinanceSignalCooldownMs != 2800 {
		t.Fatalf("expected saved BinanceSignalCooldownMs 2800, got %d", settings.BinanceSignalCooldownMs)
	}
	if settings.BinanceSignalMaxAgeMs != 4200 {
		t.Fatalf("expected saved BinanceSignalMaxAgeMs 4200, got %d", settings.BinanceSignalMaxAgeMs)
	}
	if settings.BinanceSignalPolyMaxMoveCents != 1.4 {
		t.Fatalf("expected saved BinanceSignalPolyMaxMoveCents 1.4, got %.2f", settings.BinanceSignalPolyMaxMoveCents)
	}
	if settings.BinanceSignalPolyAdverseMoveCents != 0.5 {
		t.Fatalf("expected saved BinanceSignalPolyAdverseMoveCents 0.5, got %.2f", settings.BinanceSignalPolyAdverseMoveCents)
	}
	if settings.BinanceSignalSpreadMaxCents != 3.0 {
		t.Fatalf("expected saved BinanceSignalSpreadMaxCents 3.0, got %.2f", settings.BinanceSignalSpreadMaxCents)
	}
	if settings.PaperArbMode != "maker" {
		t.Fatalf("expected saved PaperArbMode maker, got %q", settings.PaperArbMode)
	}
	if settings.MakerQuoteGap != 0.005 {
		t.Fatalf("expected saved MakerQuoteGap 0.005, got %.3f", settings.MakerQuoteGap)
	}
	if settings.TradingHoursMode != "off" {
		t.Fatal("expected saved TradingHoursMode off")
	}
}

func TestCalculateCopytradeSharesForMode(t *testing.T) {
	if got := CalculateCopytradeSharesForMode(12, 0.4, 3.0, 8.0, 10.0, 0, CopytradeSizingModeUSDC); got != 7.5 {
		t.Fatalf("expected USDC copytrade sizing to buy 7.5 shares, got %.4f", got)
	}
	if got := CalculateCopytradeSharesForMode(12, 0.4, 3.0, 8.0, 10.0, 0, CopytradeSizingModeShares); got != 8.0 {
		t.Fatalf("expected share copytrade sizing to cap at 8 shares, got %.4f", got)
	}
	if got := CalculateCopytradeSharesForMode(100, 0.4, 10.0, 8.0, 10.0, 0, CopytradeSizingModePercent); got != 10.0 {
		t.Fatalf("expected percent copytrade sizing to follow 10%% of target shares, got %.4f", got)
	}
}

func TestNormalizeCopytradeMaxSlippagePctAllowsZeroAndRoundsToWholeCents(t *testing.T) {
	if got := normalizeCopytradeMaxSlippagePct(0); got != 0 {
		t.Fatalf("expected 0c slippage to remain 0, got %.2f", got)
	}
	if got := normalizeCopytradeMaxSlippagePct(1.4); got != 1 {
		t.Fatalf("expected 1.4c to round to 1c, got %.2f", got)
	}
	if got := normalizeCopytradeMaxSlippagePct(1.6); got != 2 {
		t.Fatalf("expected 1.6c to round to 2c, got %.2f", got)
	}
	if got := normalizeCopytradeMaxSlippagePct(120); got != 99 {
		t.Fatalf("expected slippage to clamp at 99c, got %.2f", got)
	}
}

func TestCopytradePriceBoundsUseAbsoluteCents(t *testing.T) {
	if got := CopytradeBuyLimitPrice(0.54, 1); math.Abs(got-0.55) > 0.000001 {
		t.Fatalf("expected buy limit 0.55 from 1c slippage, got %.4f", got)
	}
	if got := CopytradeBuyLimitPrice(0.54, 0); math.Abs(got-0.54) > 0.000001 {
		t.Fatalf("expected buy limit unchanged at 0c slippage, got %.4f", got)
	}
	if got := CopytradeSellFloorPrice(0.54, 1); math.Abs(got-0.53) > 0.000001 {
		t.Fatalf("expected sell floor 0.53 from 1c slippage, got %.4f", got)
	}
	if got := CopytradeSellFloorPrice(0.54, 0); math.Abs(got-0.54) > 0.000001 {
		t.Fatalf("expected sell floor unchanged at 0c slippage, got %.4f", got)
	}
}

func TestCalculateCopytradeSellSharesForModeLiquidatesFullFlatTarget(t *testing.T) {
	got := CalculateCopytradeSellSharesForMode(5.51, 0, -5.51, 0.23, 1.0, 1.0, 100.0, 0, CopytradeSizingModeUSDC)
	if got != 5.51 {
		t.Fatalf("expected flat target to liquidate the full 5.51-share remainder, got %.4f", got)
	}
}

func TestCalculateCopytradeSellSharesForModePercentUsesTargetDelta(t *testing.T) {
	// Local shares: 50
	// Target owns 1000 shares, target sells 500 shares (delta -500). Target remaining shares: 500.
	// Percent: 10%
	// Expected sell size: 10% of 500 = 50.
	got := CalculateCopytradeSellSharesForMode(50, 500, -500, 0.5, 0, 0, 10.0, 0, CopytradeSizingModePercent)
	if got != 50.0 {
		t.Fatalf("expected to sell 50 shares (10%% of target delta 500), got %.4f", got)
	}
}

func TestCalculateTradeSizeForModeUsesFixedUSDC(t *testing.T) {
	got := CalculateTradeSizeForMode(1000, 0.05, 2.3, 0, TradeSizingModeUSDC)
	if got != 2.3 {
		t.Fatalf("expected fixed trade size 2.3, got %.1f", got)
	}
}

func TestCalculateTradeSizeForModeRoundsAndClampsFixedUSDC(t *testing.T) {
	got := CalculateTradeSizeForMode(1000, 0.05, 0.04, 0, TradeSizingModeUSDC)
	if got != 0.1 {
		t.Fatalf("expected fixed trade size to clamp to 0.1, got %.1f", got)
	}
}

func TestCalculateTradeSizeForModeHonorsMaxTradeCap(t *testing.T) {
	got := CalculateTradeSizeForMode(1000, 0.05, 7.5, 5.0, TradeSizingModeUSDC)
	if got != 5.0 {
		t.Fatalf("expected max trade cap 5.0, got %.1f", got)
	}
}

func TestNormalizeBinanceSignalPolyMaxMoveCentsAllowsDisable(t *testing.T) {
	if got := normalizeBinanceSignalPolyMaxMoveCents(0); got != 0 {
		t.Fatalf("expected zero catch-up limit to stay disabled, got %.2f", got)
	}
	if got := normalizeBinanceSignalPolyMaxMoveCents(-1); got != 0 {
		t.Fatalf("expected negative catch-up limit to normalize to disabled, got %.2f", got)
	}
}

func TestNormalizeBinanceSignalPolyAdverseMoveCentsAllowsDisable(t *testing.T) {
	if got := normalizeBinanceSignalPolyAdverseMoveCents(0); got != 0 {
		t.Fatalf("expected zero adverse-move limit to stay disabled, got %.2f", got)
	}
	if got := normalizeBinanceSignalPolyAdverseMoveCents(-1); got != 0 {
		t.Fatalf("expected negative adverse-move limit to normalize to disabled, got %.2f", got)
	}
}
