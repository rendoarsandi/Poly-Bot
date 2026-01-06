package core

import (
	"errors"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type TradingMode string

const (
	ModePaper TradingMode = "paper"
	ModeReal  TradingMode = "real"
)

type Config struct {
	// Trading mode: "paper" (default) or "real"
	TradingMode TradingMode

	// Polymarket API credentials (required for real trading)
	PK            string // Private key for signing (hex, with or without 0x prefix)
	APIKey        string // CLOB API key
	APISecret     string // CLOB API secret (base64 encoded)
	APIPassphrase string // CLOB API passphrase

	// Polygon network
	PolygonRPCURL string

	// Market settings
	MarketSlug string

	// Position sizing settings
	// Default: $1000 balance → $50 per trade (5% of balance)
	BaseBalance      float64 // Reference balance for position sizing ($1000 default)
	BaseTradeSize    float64 // Trade size at base balance ($50 default)
	MinMarginPercent float64 // Minimum arbitrage margin to trade (1% default)
	TradeScaleFactor float64 // Fraction of balance to use per trade (0.05 = 5% default)

	// Safety settings for real trading
	MaxTradeSize   float64 // Maximum USDC per single trade (overrides scaling)
	MaxDailyLoss   float64 // Maximum daily loss before stopping
	RequireConfirm bool    // Require confirmation before each trade
	DryRunFirst    bool    // Run in dry-run mode first (simulate real API calls)
}

func LoadConfig() (*Config, error) {
	// Load .env file if it exists, but don't fail if it doesn't (env vars might be set already)
	_ = godotenv.Load()

	mode := TradingMode(strings.ToLower(os.Getenv("TRADING_MODE")))
	if mode == "" {
		mode = ModePaper
	}

	cfg := &Config{
		TradingMode:   mode,
		PK:            os.Getenv("PK"),
		APIKey:        os.Getenv("API_KEY"),
		APISecret:     os.Getenv("API_SECRET"),
		APIPassphrase: os.Getenv("API_PASSPHRASE"),
		PolygonRPCURL: os.Getenv("POLYGON_RPC_URL"),
		MarketSlug:    os.Getenv("MARKET_SLUG"),
		// Position sizing defaults: $1000 balance → $50 per trade
		BaseBalance:      parseEnvFloat("BASE_BALANCE", 1000.0),
		BaseTradeSize:    parseEnvFloat("BASE_TRADE_SIZE", 50.0),
		MinMarginPercent: parseEnvFloat("MIN_MARGIN_PERCENT", 1.0),
		TradeScaleFactor: parseEnvFloat("TRADE_SCALE_FACTOR", 0.05), // 5% of balance
		// Safety settings
		MaxTradeSize:   parseEnvFloat("MAX_TRADE_SIZE", 0),    // 0 = no hard cap, use scaling
		MaxDailyLoss:   parseEnvFloat("MAX_DAILY_LOSS", 50.0), // Default $50 max daily loss
		RequireConfirm: os.Getenv("REQUIRE_CONFIRM") == "true",
		DryRunFirst:    os.Getenv("DRY_RUN_FIRST") != "false", // Default true for safety
	}

	return cfg, nil
}

// ValidateForRealTrading checks that all required credentials are present for real trading
func (c *Config) ValidateForRealTrading() error {
	if c.TradingMode != ModeReal {
		return nil // No validation needed for paper trading
	}

	var missing []string

	if c.PK == "" {
		missing = append(missing, "PK (private key)")
	}
	if c.APIKey == "" {
		missing = append(missing, "API_KEY")
	}
	if c.APISecret == "" {
		missing = append(missing, "API_SECRET")
	}
	if c.APIPassphrase == "" {
		missing = append(missing, "API_PASSPHRASE")
	}

	if len(missing) > 0 {
		return errors.New("missing required credentials for real trading: " + strings.Join(missing, ", "))
	}

	// Validate private key format
	pk := strings.TrimPrefix(c.PK, "0x")
	if len(pk) != 64 {
		return errors.New("PK must be a 64-character hex string (with or without 0x prefix)")
	}

	return nil
}

// IsPaperMode returns true if running in paper trading mode
func (c *Config) IsPaperMode() bool {
	return c.TradingMode != ModeReal
}

// CalculateTradeSize returns the trade size (in $) based on current balance and margin
// Formula: tradeSize = balance * scaleFactor
// Example: $1000 balance * 0.10 = $100 trade size
// For $100 balance: $100 * 0.10 = $10 trade size
func (c *Config) CalculateTradeSize(currentBalance float64) float64 {
	// Calculate scaled trade size based on balance
	tradeSize := currentBalance * c.TradeScaleFactor

	// Apply max trade size cap if configured
	if c.MaxTradeSize > 0 && tradeSize > c.MaxTradeSize {
		tradeSize = c.MaxTradeSize
	}

	// Minimum trade size of $1 to avoid dust orders
	if tradeSize < 1.0 {
		tradeSize = 1.0
	}

	return tradeSize
}

// CalculateShares returns the number of shares to buy based on balance, margin, and price sum
// Example: $1000 balance, 2% margin, sum=$0.98 → shares = $100 / $0.98 ≈ 102 shares
func (c *Config) CalculateShares(currentBalance, priceSum float64) float64 {
	if priceSum <= 0 || priceSum >= 1.0 {
		return 0
	}

	tradeSize := c.CalculateTradeSize(currentBalance)
	shares := tradeSize / priceSum

	return shares
}

// String returns a redacted string representation of the config
func (c *Config) String() string {
	return "Config{TradingMode: " + string(c.TradingMode) +
		", MarketSlug: " + c.MarketSlug +
		", PK: [REDACTED], APIKey: [REDACTED], APISecret: [REDACTED], APIPassphrase: [REDACTED]}"
}

func parseEnvFloat(key string, defaultVal float64) float64 {
	val := os.Getenv(key)
	if val == "" {
		return defaultVal
	}
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return defaultVal
	}
	return f
}

// SanitizeString removes control characters from a string to prevent terminal manipulation.
func SanitizeString(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 {
			return -1
		}
		return r
	}, s)
}
