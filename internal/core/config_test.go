package core

import (
	"encoding/json"
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
