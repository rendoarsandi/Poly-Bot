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
	Timeframe  string // 5m or 15m
	MaxMarkets int    // Maximum concurrent markets to trade

	// Position sizing settings
	// Default: $1000 balance → $50 per trade (5% of balance)
	BaseBalance      float64 // Reference balance for position sizing ($1000 default)
	BaseTradeSize    float64 // Trade size at base balance ($50 default)
	MinMarginPercent float64 // Minimum arbitrage margin to trade (1% default)
	TradeScaleFactor float64 // Fraction of balance to use per trade (0.05 = 5% default)

	// Fee settings (for paper trading simulation)
	// Polymarket fees use price-curve: fee_tokens = shares * base_rate * 2 * p * (1-p)
	// Default: 312 bps base rate calibrated to match ~1.6% effective at p=0.50
	FeeRateBps int // Base fee rate in basis points (312 = ~1.6% effective at p=0.50)

	// Safety settings for real trading
	MaxTradeSize   float64 // Maximum USDC per single trade (overrides scaling)
	MaxDailyLoss   float64 // Maximum daily loss before stopping
	RequireConfirm bool    // Require confirmation before each trade

	// Logging settings
	EnableCSVLogger bool // Whether to enable CSV logging of bot activity

	// Aggression settings
	EnableMarginAggression  bool    // Scale trade size by margin (e.g., 2% margin = 2x size)
	MaxAggressionMultiplier float64 // Maximum multiplier for margin-based aggression (default: 5.0)

	// API constraints
	MinOrderSize float64 // Minimum shares per order enforced by CLOB API (default: 5.0)

	// Price filters
	MinAskPrice float64 // Minimum ask price to buy (default: 0.10)
	MaxAskPrice float64 // Maximum ask price to buy (default: 0.90)

	// ═══════════════════════════════════════════════════════════════════════════
	// SPLIT STRATEGY SETTINGS (Panic Sell)
	// Strategy: SPLIT USDC → YES+NO shares, SELL when bid_sum > $1.03
	// This is the INVERSE of the panic buy strategy (buy when ask_sum < $0.98)
	// ═══════════════════════════════════════════════════════════════════════════
	SplitStrategyEnabled     bool    // Enable split strategy (default: false)
	SplitMinMarginSell       float64 // Minimum margin to trigger sell (default: 3%)
	SplitTargetMarginReserve float64 // Maintain inventory for this margin level (default: 6%)
	SplitReplenishThreshold  float64 // Trigger new split when shares fall below this (default: 50)
	SplitMergeBufferSeconds  int     // Seconds before expiry to merge unsold shares (default: 30)
	SplitInitialCapPct       float64 // Initial Split Cap (default: 0.25)
	SplitReplenishCapPct     float64 // Replenishment Cap (default: 0.50)
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
		PK:            getEnvWithFallback("POLY_PK", "PK"),
		APIKey:        getEnvWithFallback("POLY_API_KEY", "API_KEY"),
		APISecret:     getEnvWithFallback("POLY_API_SECRET", "API_SECRET"),
		APIPassphrase: getEnvWithFallback("POLY_PASSPHRASE", "API_PASSPHRASE"),
		PolygonRPCURL: os.Getenv("POLYGON_RPC_URL"),
		MarketSlug:    getEnvWithFallback("MARKET_SLUG", "ALL"),
		Timeframe:     getEnvWithFallback("TIMEFRAME", "15m"),
		MaxMarkets:    parseEnvInt("MAX_MARKETS", 4),
		// Position sizing defaults: $1000 balance → $50 per trade
		BaseBalance:      parseEnvFloat("BASE_BALANCE", 1000.0),
		BaseTradeSize:    parseEnvFloat("BASE_TRADE_SIZE", 50.0),
		MinMarginPercent: parseEnvFloat("MIN_MARGIN_PERCENT", 2.0),
		TradeScaleFactor: parseEnvFloat("TRADE_SCALE_FACTOR", 0.05), // 5% of balance
		// Fee settings (paper trading)
		FeeRateBps: parseEnvInt("FEE_RATE_BPS", 312), // Calibrated: ~1.6% effective at p=0.50
		// Safety settings
		MaxTradeSize:    parseEnvFloat("MAX_TRADE_SIZE", 0), // 0 = no hard cap, use scaling
		MaxDailyLoss:    parseEnvFloat("MAX_DAILY_LOSS", 0), // 0 = disabled (rely on kill switch drawdown instead)
		RequireConfirm:  os.Getenv("REQUIRE_CONFIRM") == "true",
		EnableCSVLogger: os.Getenv("ENABLE_CSV_LOGGER") == "true",
		// Aggression settings
		EnableMarginAggression:  os.Getenv("ENABLE_MARGIN_AGGRESSION") != "false", // Default true
		MaxAggressionMultiplier: parseEnvFloat("MAX_AGGRESSION_MULTIPLIER", 5.0),
		// Price filters
		MinAskPrice: parseEnvFloat("MIN_ASK_PRICE", 0.10),
		MaxAskPrice: parseEnvFloat("MAX_ASK_PRICE", 0.90),
		// Split strategy settings (panic sell)
		SplitStrategyEnabled:     os.Getenv("SPLIT_STRATEGY_ENABLED") == "true",
		SplitMinMarginSell:       parseEnvFloat("SPLIT_MIN_MARGIN_SELL", 3.0),
		SplitTargetMarginReserve: parseEnvFloat("SPLIT_TARGET_MARGIN_RESERVE", 6.0),
		SplitReplenishThreshold:  parseEnvFloat("SPLIT_REPLENISH_THRESHOLD", 50.0),
		SplitMergeBufferSeconds:  parseEnvInt("SPLIT_MERGE_BUFFER_SECONDS", 30),
		SplitInitialCapPct:       parseEnvFloat("SPLIT_INITIAL_CAP_PCT", 0.25),
		SplitReplenishCapPct:     parseEnvFloat("SPLIT_REPLENISH_CAP_PCT", 0.50),
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
		missing = append(missing, "POLY_PK")
	}
	if c.APIKey == "" {
		missing = append(missing, "POLY_API_KEY")
	}
	if c.APISecret == "" {
		missing = append(missing, "POLY_API_SECRET")
	}
	if c.APIPassphrase == "" {
		missing = append(missing, "POLY_PASSPHRASE")
	}

	if len(missing) > 0 {
		return errors.New("missing required credentials for real trading: " + strings.Join(missing, ", "))
	}

	// Validate private key format
	pk := strings.TrimPrefix(c.PK, "0x")
	if len(pk) != 64 {
		return errors.New("POLY_PK must be a 64-character hex string (with or without 0x prefix)")
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

func getEnvWithFallback(primary, fallback string) string {
	if val := os.Getenv(primary); val != "" {
		return val
	}
	return os.Getenv(fallback)
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

func parseEnvInt(key string, defaultVal int) int {
	val := os.Getenv(key)
	if val == "" {
		return defaultVal
	}
	i, err := strconv.Atoi(val)
	if err != nil {
		return defaultVal
	}
	return i
}

// SaveSettings writes the mutable runtime settings back to the .env file.
func (c *Config) SaveSettings() error {
	envMap, err := godotenv.Read(".env")
	if err != nil {
		// If .env doesn't exist, create an empty map
		envMap = make(map[string]string)
	}

	envMap["MARKET_SLUG"] = c.MarketSlug
	envMap["TIMEFRAME"] = c.Timeframe
	envMap["MAX_MARKETS"] = strconv.Itoa(c.MaxMarkets)
	envMap["MIN_MARGIN_PERCENT"] = strconv.FormatFloat(c.MinMarginPercent, 'f', -1, 64)
	envMap["TRADE_SCALE_FACTOR"] = strconv.FormatFloat(c.TradeScaleFactor, 'f', -1, 64)
	envMap["SPLIT_STRATEGY_ENABLED"] = strconv.FormatBool(c.SplitStrategyEnabled)
	envMap["SPLIT_MIN_MARGIN_SELL"] = strconv.FormatFloat(c.SplitMinMarginSell, 'f', -1, 64)
	envMap["SPLIT_INITIAL_CAP_PCT"] = strconv.FormatFloat(c.SplitInitialCapPct, 'f', -1, 64)
	envMap["SPLIT_REPLENISH_CAP_PCT"] = strconv.FormatFloat(c.SplitReplenishCapPct, 'f', -1, 64)

	return godotenv.Write(envMap, ".env")
}

