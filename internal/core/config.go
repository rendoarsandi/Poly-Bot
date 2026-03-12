package core

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type TradingMode string

const (
	ModePaper TradingMode = "paper"
	ModeReal  TradingMode = "real"

	defaultExecutionLocalQuoteMaxAge = 5 * time.Second
	defaultRestFallbackQuoteAge      = 3 * time.Second
	defaultRestFallbackPollInterval  = 1 * time.Second
)

type Config struct {
	// Trading mode is inferred internally from the selected bot/profile.
	TradingMode TradingMode

	// Polymarket API credentials (required for real trading)
	PK            string // Private key for signing (hex, with or without 0x prefix)
	APIKey        string // CLOB API key
	APISecret     string // CLOB API secret (base64 encoded)
	APIPassphrase string // CLOB API passphrase

	// Kalshi API credentials
	Exchange     string // "polymarket" or "kalshi"
	KalshiAPIKey string // Kalshi API key
	KalshiPK     string // Kalshi Private Key (RSA)

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
	EnableRawAPILog bool // Whether to enable raw Polymarket request/response logging

	// Market data freshness / fallback settings
	ExecutionLocalQuoteMaxAgeMs int // Max age for a local quote before execution refreshes from REST
	RestFallbackQuoteAgeMs      int // Quote age required before REST fallback is allowed when WS is unhealthy
	RestFallbackPollIntervalMs  int // Minimum interval between REST fallback polls

	// Aggression settings
	EnableMarginAggression  bool    // Scale trade size by margin (e.g., 2% margin = 2x size)
	MaxAggressionMultiplier float64 // Maximum multiplier for margin-based aggression (default: 5.0)

	// API constraints
	MinOrderSize float64 // Minimum shares per order enforced by CLOB API (default: 5.0)

	// Price filters
	MinAskPrice  float64 // Minimum ask price to buy (default: 0.10)
	MaxAskPrice  float64 // Maximum ask price to buy (default: 0.90)
	PaperArbMode string  // Paperbot arb execution mode: taker or maker
	// Shared panic buy/sell execution tolerance: minimum acceptable combined pair
	// margin while walking deeper book liquidity during execution. Can be negative
	// to tolerate a small loss on the pair if that reduces legging risk.
	// Env name is kept as BUY_EXECUTION_MARGIN_FLOOR_PERCENT for backward compatibility.
	BuyExecutionMarginFloorPercent float64

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
	MakerMergeBufferSeconds  int     // Seconds before expiry to merge paired maker inventory (default: 30)
	MakerQuoteGap            float64 // Distance from mid for maker quotes (default: 0.008)
	MakerInventoryTargetMult float64 // Target multiplier for inventory skew (default: 3.0)
	MakerInventoryCapMult    float64 // Cap multiplier for inventory skew (default: 5.0)
	MakerMinQuoteValue      float64 // Minimum shares to quote (default: 10.0)
	SplitInitialCapPct       float64 // Initial Split Cap (default: 0.25)
	SplitReplenishCapPct     float64 // Replenishment Cap (default: 0.50)

	settingsProfile string
	settingsPath    string
}

type RuntimeSettings struct {
	Exchange                       string  `json:"exchange"`
	MarketSlug                     string  `json:"marketSlug"`
	Timeframe                      string  `json:"timeframe"`
	MaxMarkets                     int     `json:"maxMarkets"`
	BaseBalance                    float64 `json:"baseBalance"`
	BaseTradeSize                  float64 `json:"baseTradeSize"`
	MinMarginPercent               float64 `json:"minMarginPercent"`
	TradeScaleFactor               float64 `json:"tradeScaleFactor"`
	FeeRateBps                     int     `json:"feeRateBps"`
	MaxTradeSize                   float64 `json:"maxTradeSize"`
	MaxDailyLoss                   float64 `json:"maxDailyLoss"`
	RequireConfirm                 bool    `json:"requireConfirm"`
	EnableCSVLogger                bool    `json:"enableCsvLogger"`
	EnableRawAPILog                bool    `json:"enableRawApiLog"`
	ExecutionLocalQuoteMaxAgeMs    int     `json:"executionLocalQuoteMaxAgeMs"`
	RestFallbackQuoteAgeMs         int     `json:"restFallbackQuoteAgeMs"`
	RestFallbackPollIntervalMs     int     `json:"restFallbackPollIntervalMs"`
	EnableMarginAggression         bool    `json:"enableMarginAggression"`
	MaxAggressionMultiplier        float64 `json:"maxAggressionMultiplier"`
	MinAskPrice                    float64 `json:"minAskPrice"`
	MaxAskPrice                    float64 `json:"maxAskPrice"`
	PaperArbMode                   string  `json:"paperArbMode"`
	BuyExecutionMarginFloorPercent float64 `json:"buyExecutionMarginFloorPercent"`
	SplitStrategyEnabled           bool    `json:"splitStrategyEnabled"`
	SplitMinMarginSell             float64 `json:"splitMinMarginSell"`
	SplitTargetMarginReserve       float64 `json:"splitTargetMarginReserve"`
	SplitReplenishThreshold        float64 `json:"splitReplenishThreshold"`
	SplitMergeBufferSeconds        int     `json:"splitMergeBufferSeconds"`
	MakerMergeBufferSeconds        int     `json:"makerMergeBufferSeconds"`
	MakerQuoteGap                  float64 `json:"makerQuoteGap"`
	MakerInventoryTargetMult       float64 `json:"makerInventoryTargetMult"`
	MakerInventoryCapMult          float64 `json:"makerInventoryCapMult"`
	MakerMinQuoteValue            float64 `json:"makerMinQuoteValue"`
	SplitInitialCapPct             float64 `json:"splitInitialCapPct"`
	SplitReplenishCapPct           float64 `json:"splitReplenishCapPct"`
}

func LoadConfig() (*Config, error) {
	// Load .env file if it exists, but don't fail if it doesn't (env vars might be set already)
	_ = godotenv.Load()

	cfg := &Config{
		TradingMode:   ModePaper,
		Exchange:      getEnvWithFallback("EXCHANGE", "polymarket"),
		KalshiAPIKey:  getEnvWithFallback("KALSHI_API_KEY", ""),
		KalshiPK:      getEnvWithFallback("KALSHI_PK", ""),
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
		MaxTradeSize:                parseEnvFloat("MAX_TRADE_SIZE", 0), // 0 = no hard cap, use scaling
		MaxDailyLoss:                parseEnvFloat("MAX_DAILY_LOSS", 0), // 0 = disabled (rely on kill switch drawdown instead)
		RequireConfirm:              os.Getenv("REQUIRE_CONFIRM") == "true",
		EnableCSVLogger:             os.Getenv("ENABLE_CSV_LOGGER") == "true",
		EnableRawAPILog:             os.Getenv("ENABLE_RAW_API_LOG") == "true",
		ExecutionLocalQuoteMaxAgeMs: parseEnvInt("EXECUTION_LOCAL_QUOTE_MAX_AGE_MS", int(defaultExecutionLocalQuoteMaxAge/time.Millisecond)),
		RestFallbackQuoteAgeMs:      parseEnvInt("REST_FALLBACK_QUOTE_AGE_MS", int(defaultRestFallbackQuoteAge/time.Millisecond)),
		RestFallbackPollIntervalMs:  parseEnvInt("REST_FALLBACK_POLL_INTERVAL_MS", int(defaultRestFallbackPollInterval/time.Millisecond)),
		// Aggression settings
		EnableMarginAggression:  os.Getenv("ENABLE_MARGIN_AGGRESSION") != "false", // Default true
		MaxAggressionMultiplier: parseEnvFloat("MAX_AGGRESSION_MULTIPLIER", 5.0),
		// Price filters
		MinAskPrice: parseEnvFloat("MIN_ASK_PRICE", 0.10),
		MaxAskPrice: parseEnvFloat("MAX_ASK_PRICE", 0.90),
		PaperArbMode: func() string {
			mode := strings.ToLower(strings.TrimSpace(os.Getenv("PAPER_ARB_MODE")))
			if mode == "" {
				return "taker"
			}
			return mode
		}(),
		BuyExecutionMarginFloorPercent: parseEnvFloat("BUY_EXECUTION_MARGIN_FLOOR_PERCENT", -1.0),
		// Split strategy settings (panic sell)
		SplitStrategyEnabled:     os.Getenv("SPLIT_STRATEGY_ENABLED") == "true",
		SplitMinMarginSell:       parseEnvFloat("SPLIT_MIN_MARGIN_SELL", 3.0),
		SplitTargetMarginReserve: parseEnvFloat("SPLIT_TARGET_MARGIN_RESERVE", 6.0),
		SplitReplenishThreshold:  parseEnvFloat("SPLIT_REPLENISH_THRESHOLD", 50.0),
		SplitMergeBufferSeconds:  parseEnvInt("SPLIT_MERGE_BUFFER_SECONDS", 30),
		MakerMergeBufferSeconds:  parseEnvInt("MAKER_MERGE_BUFFER_SECONDS", parseEnvInt("SPLIT_MERGE_BUFFER_SECONDS", 30)),
		MakerQuoteGap:            parseEnvFloat("MAKER_QUOTE_GAP", 0.008),
		MakerInventoryTargetMult: parseEnvFloat("MAKER_INVENTORY_TARGET_MULT", 3.0),
		MakerInventoryCapMult:    parseEnvFloat("MAKER_INVENTORY_CAP_MULT", 5.0),
		MakerMinQuoteValue:      parseEnvFloat("MAKER_MIN_QUOTE_SHARES", 10.0),
		SplitInitialCapPct:       parseEnvFloat("SPLIT_INITIAL_CAP_PCT", 0.25),
		SplitReplenishCapPct:     parseEnvFloat("SPLIT_REPLENISH_CAP_PCT", 0.50),
	}

	return cfg, nil
}

func LoadBotConfig(profile string) (*Config, error) {
	return loadBotConfigWithPath(profile, settingsPathForProfile(profile))
}

func loadBotConfigWithPath(profile, path string) (*Config, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, err
	}
	cfg.settingsProfile = strings.ToLower(strings.TrimSpace(profile))
	cfg.settingsPath = path
	switch cfg.settingsProfile {
	case "paperbot":
		cfg.TradingMode = ModePaper
	case "realbot":
		cfg.TradingMode = ModeReal
	}
	if cfg.settingsPath == "" {
		return cfg, nil
	}
	runtime, err := readRuntimeSettings(cfg.settingsPath, cfg.runtimeSettings())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return nil, err
	}
	cfg.applyRuntimeSettings(runtime)
	return cfg, nil
}

func ResolveExecutionLocalQuoteMaxAge(cfg *Config) time.Duration {
	if cfg != nil && cfg.ExecutionLocalQuoteMaxAgeMs > 0 {
		return time.Duration(cfg.ExecutionLocalQuoteMaxAgeMs) * time.Millisecond
	}
	return defaultExecutionLocalQuoteMaxAge
}

func ResolveRestFallbackQuoteAge(cfg *Config) time.Duration {
	if cfg != nil && cfg.RestFallbackQuoteAgeMs > 0 {
		return time.Duration(cfg.RestFallbackQuoteAgeMs) * time.Millisecond
	}
	return defaultRestFallbackQuoteAge
}

func ResolveRestFallbackPollInterval(cfg *Config) time.Duration {
	if cfg != nil && cfg.RestFallbackPollIntervalMs > 0 {
		return time.Duration(cfg.RestFallbackPollIntervalMs) * time.Millisecond
	}
	return defaultRestFallbackPollInterval
}

// UseRealTrading marks the config as intended for real trading. Bot entrypoints infer
// this automatically; explicit real-only utilities can call it directly.
func (c *Config) UseRealTrading() {
	c.TradingMode = ModeReal
}

// ReloadSecretsFromEnv refreshes env-backed credentials without touching bot JSON settings.
func (c *Config) ReloadSecretsFromEnv() {
	c.Exchange = getEnvWithFallback("EXCHANGE", "polymarket")
	c.KalshiAPIKey = getEnvWithFallback("KALSHI_API_KEY", "")
	c.KalshiPK = getEnvWithFallback("KALSHI_PK", "")
	c.PK = getEnvWithFallback("POLY_PK", "PK")
	c.APIKey = getEnvWithFallback("POLY_API_KEY", "API_KEY")
	c.APISecret = getEnvWithFallback("POLY_API_SECRET", "API_SECRET")
	c.APIPassphrase = getEnvWithFallback("POLY_PASSPHRASE", "API_PASSPHRASE")
	c.PolygonRPCURL = os.Getenv("POLYGON_RPC_URL")
}

// ValidateForRealTrading checks that all required credentials are present for real trading.
func (c *Config) ValidateForRealTrading() error {
	var missing []string

	if c.Exchange == "kalshi" {
		if c.KalshiAPIKey == "" {
			missing = append(missing, "KALSHI_API_KEY")
		}
		if c.KalshiPK == "" {
			missing = append(missing, "KALSHI_PK")
		}
		if len(missing) > 0 {
			return errors.New("missing required credentials for kalshi real trading: " + strings.Join(missing, ", "))
		}
		return nil
	}

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

func settingsPathForProfile(profile string) string {
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "paperbot":
		return filepath.Join("config", "paperbot.settings.json")
	case "realbot":
		return filepath.Join("config", "realbot.settings.json")
	default:
		return ""
	}
}

func readRuntimeSettings(path string, base RuntimeSettings) (RuntimeSettings, error) {
	settings := base
	data, err := os.ReadFile(path)
	if err != nil {
		return settings, err
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return settings, err
	}
	return settings, nil
}

func writeRuntimeSettings(path string, settings RuntimeSettings) error {
	if path == "" {
		return errors.New("settings path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func (c *Config) runtimeSettings() RuntimeSettings {
	return RuntimeSettings{
		Exchange:                       c.Exchange,
		MarketSlug:                     c.MarketSlug,
		Timeframe:                      c.Timeframe,
		MaxMarkets:                     c.MaxMarkets,
		BaseBalance:                    c.BaseBalance,
		BaseTradeSize:                  c.BaseTradeSize,
		MinMarginPercent:               c.MinMarginPercent,
		TradeScaleFactor:               c.TradeScaleFactor,
		FeeRateBps:                     c.FeeRateBps,
		MaxTradeSize:                   c.MaxTradeSize,
		MaxDailyLoss:                   c.MaxDailyLoss,
		RequireConfirm:                 c.RequireConfirm,
		EnableCSVLogger:                c.EnableCSVLogger,
		EnableRawAPILog:                c.EnableRawAPILog,
		ExecutionLocalQuoteMaxAgeMs:    c.ExecutionLocalQuoteMaxAgeMs,
		RestFallbackQuoteAgeMs:         c.RestFallbackQuoteAgeMs,
		RestFallbackPollIntervalMs:     c.RestFallbackPollIntervalMs,
		EnableMarginAggression:         c.EnableMarginAggression,
		MaxAggressionMultiplier:        c.MaxAggressionMultiplier,
		MinAskPrice:                    c.MinAskPrice,
		MaxAskPrice:                    c.MaxAskPrice,
		PaperArbMode:                   c.PaperArbMode,
		BuyExecutionMarginFloorPercent: c.BuyExecutionMarginFloorPercent,
		SplitStrategyEnabled:           c.SplitStrategyEnabled,
		SplitMinMarginSell:             c.SplitMinMarginSell,
		SplitTargetMarginReserve:       c.SplitTargetMarginReserve,
		SplitReplenishThreshold:        c.SplitReplenishThreshold,
		SplitMergeBufferSeconds:        c.SplitMergeBufferSeconds,
		MakerMergeBufferSeconds:        c.MakerMergeBufferSeconds,
		MakerQuoteGap:                  c.MakerQuoteGap,
		MakerInventoryTargetMult:       c.MakerInventoryTargetMult,
		MakerInventoryCapMult:          c.MakerInventoryCapMult,
		MakerMinQuoteValue:            c.MakerMinQuoteValue,
		SplitInitialCapPct:             c.SplitInitialCapPct,
		SplitReplenishCapPct:           c.SplitReplenishCapPct,
	}
}

func (c *Config) applyRuntimeSettings(s RuntimeSettings) {
	if s.Exchange != "" {
		c.Exchange = s.Exchange
	}
	c.MarketSlug = s.MarketSlug
	c.Timeframe = s.Timeframe
	c.MaxMarkets = s.MaxMarkets
	c.BaseBalance = s.BaseBalance
	c.BaseTradeSize = s.BaseTradeSize
	c.MinMarginPercent = s.MinMarginPercent
	c.TradeScaleFactor = s.TradeScaleFactor
	c.FeeRateBps = s.FeeRateBps
	c.MaxTradeSize = s.MaxTradeSize
	c.MaxDailyLoss = s.MaxDailyLoss
	c.RequireConfirm = s.RequireConfirm
	c.EnableCSVLogger = s.EnableCSVLogger
	c.EnableRawAPILog = s.EnableRawAPILog
	c.ExecutionLocalQuoteMaxAgeMs = s.ExecutionLocalQuoteMaxAgeMs
	c.RestFallbackQuoteAgeMs = s.RestFallbackQuoteAgeMs
	c.RestFallbackPollIntervalMs = s.RestFallbackPollIntervalMs
	c.EnableMarginAggression = s.EnableMarginAggression
	c.MaxAggressionMultiplier = s.MaxAggressionMultiplier
	c.MinAskPrice = s.MinAskPrice
	c.MaxAskPrice = s.MaxAskPrice
	c.PaperArbMode = s.PaperArbMode
	c.BuyExecutionMarginFloorPercent = s.BuyExecutionMarginFloorPercent
	c.SplitStrategyEnabled = s.SplitStrategyEnabled
	c.SplitMinMarginSell = s.SplitMinMarginSell
	c.SplitTargetMarginReserve = s.SplitTargetMarginReserve
	c.SplitReplenishThreshold = s.SplitReplenishThreshold
	c.SplitMergeBufferSeconds = s.SplitMergeBufferSeconds
	c.MakerMergeBufferSeconds = s.MakerMergeBufferSeconds
	c.MakerQuoteGap = s.MakerQuoteGap
	c.MakerInventoryTargetMult = s.MakerInventoryTargetMult
	c.MakerInventoryCapMult = s.MakerInventoryCapMult
	c.MakerMinQuoteValue = s.MakerMinQuoteValue
	c.SplitInitialCapPct = s.SplitInitialCapPct
	c.SplitReplenishCapPct = s.SplitReplenishCapPct

	// Force disable split/merge for Kalshi
	if c.Exchange == "kalshi" {
		c.SplitStrategyEnabled = false
		c.MakerMergeBufferSeconds = 0
	}
}

// SaveSettings writes mutable runtime settings to the bot-specific JSON file when
// the config was loaded through LoadBotConfig. Generic configs keep the legacy
// .env fallback for non-bot tools.
func (c *Config) SaveSettings() error {
	if c.settingsPath != "" {
		return writeRuntimeSettings(c.settingsPath, c.runtimeSettings())
	}
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
	envMap["PAPER_ARB_MODE"] = c.PaperArbMode
	envMap["MIN_ASK_PRICE"] = strconv.FormatFloat(c.MinAskPrice, 'f', -1, 64)
	envMap["MAX_ASK_PRICE"] = strconv.FormatFloat(c.MaxAskPrice, 'f', -1, 64)
	envMap["PAPER_ARB_MODE"] = c.PaperArbMode
	envMap["BUY_EXECUTION_MARGIN_FLOOR_PERCENT"] = strconv.FormatFloat(c.BuyExecutionMarginFloorPercent, 'f', -1, 64)
	envMap["SPLIT_STRATEGY_ENABLED"] = strconv.FormatBool(c.SplitStrategyEnabled)
	envMap["SPLIT_MIN_MARGIN_SELL"] = strconv.FormatFloat(c.SplitMinMarginSell, 'f', -1, 64)
	envMap["MAKER_MERGE_BUFFER_SECONDS"] = strconv.Itoa(c.MakerMergeBufferSeconds)
	envMap["MAKER_QUOTE_GAP"] = strconv.FormatFloat(c.MakerQuoteGap, 'f', -1, 64)
	envMap["SPLIT_INITIAL_CAP_PCT"] = strconv.FormatFloat(c.SplitInitialCapPct, 'f', -1, 64)
	envMap["SPLIT_REPLENISH_CAP_PCT"] = strconv.FormatFloat(c.SplitReplenishCapPct, 'f', -1, 64)

	return godotenv.Write(envMap, ".env")
}
