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
		"enableRawApiLog": true,
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
	if !cfg.EnableRawAPILog {
		t.Fatal("expected JSON EnableRawAPILog to override env/default")
	}
	if cfg.PaperArbMode != "maker" {
		t.Fatalf("expected JSON PaperArbMode maker, got %q", cfg.PaperArbMode)
	}
	if cfg.MakerQuoteGap != 0.004 {
		t.Fatalf("expected JSON MakerQuoteGap 0.004, got %.3f", cfg.MakerQuoteGap)
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
	cfg.EnableRawAPILog = true
	cfg.PaperArbMode = "maker"
	cfg.MakerQuoteGap = 0.005

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
	if !settings.EnableRawAPILog {
		t.Fatal("expected saved EnableRawAPILog true")
	}
	if settings.PaperArbMode != "maker" {
		t.Fatalf("expected saved PaperArbMode maker, got %q", settings.PaperArbMode)
	}
	if settings.MakerQuoteGap != 0.005 {
		t.Fatalf("expected saved MakerQuoteGap 0.005, got %.3f", settings.MakerQuoteGap)
	}
}
