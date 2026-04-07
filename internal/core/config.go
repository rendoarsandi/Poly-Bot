package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
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

	TradeSizingModePercent        = "percent"
	TradeSizingModeUSDC           = "usdc"
	CopytradeSizingModeUSDC       = "usdc"
	CopytradeSizingModeShares     = "shares"
	CopytradeSizingModePercent    = "percent"
	LadderedTakerSizingModeUSDC   = "usdc"
	LadderedTakerSizingModeShares = "shares"

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
	PaperBalance     float64 // Paperbot session balance / bankroll
	MinMarginPercent float64 // Minimum arbitrage margin to trade (1% default)
	TradeScaleFactor float64 // Fraction of balance to use per trade (0.05 = 5% default)
	TradeSizingMode  string  // "percent" or "usdc"
	TradeSizeUSDC    float64 // Fixed per-trade USDC amount when TradeSizingMode == "usdc"

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
	PaperArbMode string  // Paperbot arb execution mode: taker, laddered-taker, maker, copytrade, or binance-gap
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
	SplitStrategyEnabled              bool    // Enable split strategy (default: false)
	SplitMinMarginSell                float64 // Minimum margin to trigger sell (default: 3%)
	SplitTargetMarginReserve          float64 // Maintain inventory for this margin level (default: 6%)
	SplitReplenishThreshold           float64 // Trigger new split when shares fall below this (default: 50)
	SplitMergeBufferSeconds           int     // Seconds before expiry to merge unsold shares (default: 30)
	MakerMergeBufferSeconds           int     // Seconds before expiry to merge paired maker inventory (default: 30)
	MakerQuoteGap                     float64 // Distance from mid for maker quotes (default: 0.008)
	MakerInventoryTargetMult          float64 // Target multiplier for inventory skew (default: 3.0)
	MakerInventoryCapMult             float64 // Cap multiplier for inventory skew (default: 5.0)
	MakerMinQuoteValue                float64 // Minimum shares to quote (default: 10.0)
	SplitInitialCapPct                float64 // Initial Split Cap (default: 0.25)
	SplitReplenishCapPct              float64 // Replenishment Cap (default: 0.50)
	TradingHoursMode                  string  // "off", "weekdays trade only", "us open only"
	TakerCloseMarket                  bool    // Force GTC buy right before market closes
	TakerCloseMarketTime              int     // Seconds before close to trigger (default: 5)
	TakerCloseMarketSlippage          float64 // Limit price for taker close (default: 0.99)
	TakerCloseMarketMinPrice          float64 // Min price to trigger close buy (default: 0.60)
	CopytradeTarget                   string  // Wallet address, profile handle, or profile URL to follow
	CopytradePollIntervalMs           int     // Copytrade public-wallet poll interval in milliseconds
	CopytradeMaxSlippagePct           float64 // Legacy field name; interpreted as absolute copytrade slippage allowance in cents
	CopytradeSizingMode               string  // "usdc" or "shares" for copytrade entries
	CopytradeSizeUSDC                 float64 // Fixed per-trade USDC budget when copytrade mode uses USDC sizing
	CopytradeSizeShares               float64 // Fixed share cap per trade when copytrade mode uses share sizing
	CopytradeSizePercent              float64 // Percent of the target/master trade size when copytrade mode uses percent sizing
	LadderedTakerSizingMode           string  // "usdc" or "shares" for laddered paired taker entries
	LadderedTakerSizeUSDC             float64 // Fixed per-entry USDC budget when laddered taker uses USDC sizing
	LadderedTakerSizeShares           float64 // Fixed paired-share size per entry when laddered taker uses share sizing
	LadderedTakerReentryMoveCents     float64 // Minimum quote movement (in cents) required before the next laddered entry
	LadderedTakerMaxSlippagePct       float64 // Maximum slippage allowed for laddered taker orders (in cents)
	BinanceQuoteAsset                 string  // Futures quote asset suffix used to build symbols, e.g. USDT
	BinanceSignalThresholdPct         float64 // Percent move over the lookback window required to trigger entry
	PaperBinanceExecutionDelayMs      int     // Paper-only execution delay for Binance-gap entries/exits in milliseconds
	BinanceSignalLookbackMs           int     // Lookback window for Binance directional signal in milliseconds
	BinanceSignalCooldownMs           int     // Cooldown between Binance-triggered entries in milliseconds
	BinanceSignalMaxAgeMs             int     // Max allowed Binance signal staleness in milliseconds
	BinanceSignalPolyMaxMoveCents     float64 // Max Polymarket catch-up on the signaled side before entry is skipped
	BinanceSignalPolyAdverseMoveCents float64 // Max Polymarket wrong-way move allowed before entry is skipped
	BinanceSignalSpreadMaxCents       float64 // Max Polymarket target-side spread allowed for Binance gap entries
	StartupWizardSeen                 bool    // Whether the themed startup wizard has been completed

	settingsProfile string
	settingsPath    string
}

type RuntimeSettings struct {
	Exchange                          string  `json:"exchange"`
	MarketSlug                        string  `json:"marketSlug"`
	Timeframe                         string  `json:"timeframe"`
	MaxMarkets                        int     `json:"maxMarkets"`
	BaseBalance                       float64 `json:"baseBalance"`
	BaseTradeSize                     float64 `json:"baseTradeSize"`
	PaperBalance                      float64 `json:"paperBalance"`
	MinMarginPercent                  float64 `json:"minMarginPercent"`
	TradeScaleFactor                  float64 `json:"tradeScaleFactor"`
	TradeSizingMode                   string  `json:"tradeSizingMode"`
	TradeSizeUSDC                     float64 `json:"tradeSizeUsdc"`
	FeeRateBps                        int     `json:"feeRateBps"`
	MaxTradeSize                      float64 `json:"maxTradeSize"`
	MaxDailyLoss                      float64 `json:"maxDailyLoss"`
	RequireConfirm                    bool    `json:"requireConfirm"`
	EnableCSVLogger                   bool    `json:"enableCsvLogger"`
	EnableRawAPILog                   bool    `json:"enableRawApiLog"`
	ExecutionLocalQuoteMaxAgeMs       int     `json:"executionLocalQuoteMaxAgeMs"`
	RestFallbackQuoteAgeMs            int     `json:"restFallbackQuoteAgeMs"`
	RestFallbackPollIntervalMs        int     `json:"restFallbackPollIntervalMs"`
	EnableMarginAggression            bool    `json:"enableMarginAggression"`
	MaxAggressionMultiplier           float64 `json:"maxAggressionMultiplier"`
	MinAskPrice                       float64 `json:"minAskPrice"`
	MaxAskPrice                       float64 `json:"maxAskPrice"`
	PaperArbMode                      string  `json:"paperArbMode"`
	BuyExecutionMarginFloorPercent    float64 `json:"buyExecutionMarginFloorPercent"`
	SplitStrategyEnabled              bool    `json:"splitStrategyEnabled"`
	SplitMinMarginSell                float64 `json:"splitMinMarginSell"`
	SplitTargetMarginReserve          float64 `json:"splitTargetMarginReserve"`
	SplitReplenishThreshold           float64 `json:"splitReplenishThreshold"`
	SplitMergeBufferSeconds           int     `json:"splitMergeBufferSeconds"`
	MakerMergeBufferSeconds           int     `json:"makerMergeBufferSeconds"`
	MakerQuoteGap                     float64 `json:"makerQuoteGap"`
	MakerInventoryTargetMult          float64 `json:"makerInventoryTargetMult"`
	MakerInventoryCapMult             float64 `json:"makerInventoryCapMult"`
	MakerMinQuoteValue                float64 `json:"makerMinQuoteValue"`
	SplitInitialCapPct                float64 `json:"splitInitialCapPct"`
	SplitReplenishCapPct              float64 `json:"splitReplenishCapPct"`
	TradingHoursMode                  string  `json:"tradingHoursMode"`
	TakerCloseMarket                  bool    `json:"takerCloseMarket"`
	TakerCloseMarketTime              int     `json:"takerCloseMarketTime"`
	TakerCloseMarketSlippage          float64 `json:"takerCloseMarketSlippage"`
	TakerCloseMarketMinPrice          float64 `json:"takerCloseMarketMinPrice"`
	CopytradeTarget                   string  `json:"copytradeTarget"`
	CopytradePollIntervalMs           int     `json:"copytradePollIntervalMs"`
	CopytradeMaxSlippagePct           float64 `json:"copytradeMaxSlippagePct"`
	CopytradeSizingMode               string  `json:"copytradeSizingMode"`
	CopytradeSizeUSDC                 float64 `json:"copytradeSizeUsdc"`
	CopytradeSizeShares               float64 `json:"copytradeSizeShares"`
	CopytradeSizePercent              float64 `json:"copytradeSizePercent"`
	LadderedTakerSizingMode           string  `json:"ladderedTakerSizingMode"`
	LadderedTakerSizeUSDC             float64 `json:"ladderedTakerSizeUsdc"`
	LadderedTakerSizeShares           float64 `json:"ladderedTakerSizeShares"`
	LadderedTakerReentryMoveCents     float64 `json:"ladderedTakerReentryMoveCents"`
	LadderedTakerMaxSlippagePct       float64 `json:"ladderedTakerMaxSlippagePct"`
	BinanceQuoteAsset                 string  `json:"binanceQuoteAsset"`
	BinanceSignalThresholdPct         float64 `json:"binanceSignalThresholdPct"`
	PaperBinanceExecutionDelayMs      int     `json:"paperBinanceExecutionDelayMs"`
	BinanceSignalLookbackMs           int     `json:"binanceSignalLookbackMs"`
	BinanceSignalCooldownMs           int     `json:"binanceSignalCooldownMs"`
	BinanceSignalMaxAgeMs             int     `json:"binanceSignalMaxAgeMs"`
	BinanceSignalPolyMaxMoveCents     float64 `json:"binanceSignalPolyMaxMoveCents"`
	BinanceSignalPolyAdverseMoveCents float64 `json:"binanceSignalPolyAdverseMoveCents"`
	BinanceSignalSpreadMaxCents       float64 `json:"binanceSignalSpreadMaxCents"`
	StartupWizardSeen                 bool    `json:"startupWizardSeen"`
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
		PaperBalance:     normalizePaperBalance(parseEnvFloat("PAPER_BALANCE", 100.0)),
		MinMarginPercent: parseEnvFloat("MIN_MARGIN_PERCENT", 2.0),
		TradeScaleFactor: parseEnvFloat("TRADE_SCALE_FACTOR", 0.05), // 5% of balance
		TradeSizingMode:  normalizeTradeSizingMode(parseEnvString("TRADE_SIZING_MODE", TradeSizingModePercent)),
		TradeSizeUSDC:    normalizeFixedTradeSizeUSDC(parseEnvFloat("TRADE_SIZE_USDC", 1.0)),
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
		SplitStrategyEnabled:              os.Getenv("SPLIT_STRATEGY_ENABLED") == "true",
		SplitMinMarginSell:                parseEnvFloat("SPLIT_MIN_MARGIN_SELL", 3.0),
		SplitTargetMarginReserve:          parseEnvFloat("SPLIT_TARGET_MARGIN_RESERVE", 6.0),
		SplitReplenishThreshold:           parseEnvFloat("SPLIT_REPLENISH_THRESHOLD", 50.0),
		SplitMergeBufferSeconds:           parseEnvInt("SPLIT_MERGE_BUFFER_SECONDS", 30),
		MakerMergeBufferSeconds:           parseEnvInt("MAKER_MERGE_BUFFER_SECONDS", parseEnvInt("SPLIT_MERGE_BUFFER_SECONDS", 30)),
		MakerQuoteGap:                     parseEnvFloat("MAKER_QUOTE_GAP", 0.008),
		MakerInventoryTargetMult:          parseEnvFloat("MAKER_INVENTORY_TARGET_MULT", 3.0),
		MakerInventoryCapMult:             parseEnvFloat("MAKER_INVENTORY_CAP_MULT", 5.0),
		MakerMinQuoteValue:                parseEnvFloat("MAKER_MIN_QUOTE_SHARES", 10.0),
		SplitInitialCapPct:                parseEnvFloat("SPLIT_INITIAL_CAP_PCT", 0.25),
		SplitReplenishCapPct:              parseEnvFloat("SPLIT_REPLENISH_CAP_PCT", 0.50),
		TradingHoursMode:                  parseEnvString("TRADING_HOURS_MODE", "weekdays trade only"),
		CopytradeTarget:                   strings.TrimSpace(parseEnvString("COPYTRADE_TARGET", "")),
		CopytradePollIntervalMs:           normalizeCopytradePollIntervalMs(parseEnvInt("COPYTRADE_POLL_INTERVAL_MS", 500)),
		CopytradeMaxSlippagePct:           normalizeCopytradeMaxSlippagePct(parseEnvFloat("COPYTRADE_MAX_SLIPPAGE_PCT", 1.0)),
		CopytradeSizingMode:               normalizeCopytradeSizingMode(parseEnvString("COPYTRADE_SIZING_MODE", CopytradeSizingModeUSDC)),
		CopytradeSizeUSDC:                 normalizeCopytradeSizeUSDC(parseEnvFloat("COPYTRADE_SIZE_USDC", parseEnvFloat("TRADE_SIZE_USDC", 1.0))),
		CopytradeSizeShares:               normalizeCopytradeSizeShares(parseEnvFloat("COPYTRADE_SIZE_SHARES", 1.0)),
		CopytradeSizePercent:              normalizeCopytradeSizePercent(parseEnvFloat("COPYTRADE_SIZE_PERCENT", 100.0)),
		LadderedTakerSizingMode:           normalizeLadderedTakerSizingMode(parseEnvString("LADDERED_TAKER_SIZING_MODE", LadderedTakerSizingModeUSDC)),
		LadderedTakerSizeUSDC:             normalizeLadderedTakerSizeUSDC(parseEnvFloat("LADDERED_TAKER_SIZE_USDC", parseEnvFloat("TRADE_SIZE_USDC", 1.0))),
		LadderedTakerSizeShares:           normalizeLadderedTakerSizeShares(parseEnvFloat("LADDERED_TAKER_SIZE_SHARES", 1.0)),
		LadderedTakerReentryMoveCents:     normalizeLadderedTakerReentryMoveCents(parseEnvFloat("LADDERED_TAKER_REENTRY_MOVE_CENTS", 1.0)),
		LadderedTakerMaxSlippagePct:       normalizeLadderedTakerMaxSlippagePct(parseEnvFloat("LADDERED_TAKER_MAX_SLIPPAGE_PCT", 1.0)),
		BinanceQuoteAsset:                 normalizeBinanceQuoteAsset(parseEnvString("BINANCE_QUOTE_ASSET", "USDT")),
		BinanceSignalThresholdPct:         normalizeBinanceSignalThresholdPct(parseEnvFloat("BINANCE_SIGNAL_THRESHOLD_PCT", 0.02)),
		PaperBinanceExecutionDelayMs:      normalizePaperBinanceExecutionDelayMs(parseEnvInt("PAPER_BINANCE_EXECUTION_DELAY_MS", 250)),
		BinanceSignalLookbackMs:           normalizeBinanceSignalLookbackMs(parseEnvInt("BINANCE_SIGNAL_LOOKBACK_MS", 1500)),
		BinanceSignalCooldownMs:           normalizeBinanceSignalCooldownMs(parseEnvInt("BINANCE_SIGNAL_COOLDOWN_MS", 2500)),
		BinanceSignalMaxAgeMs:             normalizeBinanceSignalMaxAgeMs(parseEnvInt("BINANCE_SIGNAL_MAX_AGE_MS", 3000)),
		BinanceSignalPolyMaxMoveCents:     normalizeBinanceSignalPolyMaxMoveCents(parseEnvFloat("BINANCE_SIGNAL_POLY_MAX_MOVE_CENTS", 1.5)),
		BinanceSignalPolyAdverseMoveCents: normalizeBinanceSignalPolyAdverseMoveCents(parseEnvFloat("BINANCE_SIGNAL_POLY_ADVERSE_MOVE_CENTS", 0.75)),
		BinanceSignalSpreadMaxCents:       normalizeBinanceSignalSpreadMaxCents(parseEnvFloat("BINANCE_SIGNAL_SPREAD_MAX_CENTS", 4.0)),
	}

	return cfg, nil
}

func normalizeTradeSizingMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case TradeSizingModeUSDC:
		return TradeSizingModeUSDC
	default:
		return TradeSizingModePercent
	}
}

func normalizeCopytradeSizingMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case CopytradeSizingModeShares:
		return CopytradeSizingModeShares
	case CopytradeSizingModePercent:
		return CopytradeSizingModePercent
	default:
		return CopytradeSizingModeUSDC
	}
}

func normalizeLadderedTakerSizingMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case LadderedTakerSizingModeShares:
		return LadderedTakerSizingModeShares
	default:
		return LadderedTakerSizingModeUSDC
	}
}

func normalizeFixedTradeSizeUSDC(size float64) float64 {
	if size <= 0 {
		return 1.0
	}
	size = math.Round(size*10.0) / 10.0
	if size < 0.1 {
		return 0.1
	}
	return size
}

func normalizePaperBalance(balance float64) float64 {
	if balance <= 0 {
		return 100.0
	}
	return math.Round(balance*100.0) / 100.0
}

func normalizeCopytradeSizeUSDC(size float64) float64 {
	return normalizeFixedTradeSizeUSDC(size)
}

func normalizeLadderedTakerSizeUSDC(size float64) float64 {
	return normalizeFixedTradeSizeUSDC(size)
}

func normalizeCopytradeSizeShares(size float64) float64 {
	if size <= 0 {
		return 1.02
	}
	size = math.Round(size*100.0) / 100.0
	if size < 1.02 {
		return 1.02
	}
	return size
}

func normalizeLadderedTakerSizeShares(size float64) float64 {
	return normalizeCopytradeSizeShares(size)
}

func normalizeCopytradeSizePercent(size float64) float64 {
	if size <= 0 {
		return 100.0
	}
	size = math.Round(size*10.0) / 10.0
	if size < 0.1 {
		return 0.1
	}
	if size > 100.0 {
		return 100.0
	}
	return size
}

func normalizeCopytradePollIntervalMs(v int) int {
	switch {
	case v <= 0:
		return 500
	case v < 100:
		return 100
	case v > 30000:
		return 30000
	default:
		return v
	}
}

func normalizeLadderedTakerReentryMoveCents(v float64) float64 {
	if v <= 0 {
		return 1.0
	}
	v = math.Round(v*10.0) / 10.0
	if v < 1.0 {
		return 1.0
	}
	if v > 25.0 {
		return 25.0
	}
	return v
}

func normalizeCopytradeMaxSlippagePct(v float64) float64 {
	switch {
	case v < 0:
		v = 0
	case v > 99.0:
		v = 99.0
	}
	return math.Round(v)
}

func normalizeLadderedTakerMaxSlippagePct(v float64) float64 {
	switch {
	case v < 0:
		v = 0
	case v > 99.0:
		v = 99.0
	}
	return math.Round(v)
}

func CopytradeBuyLimitPrice(observedAsk, maxSlippagePct float64) float64 {
	if observedAsk <= 0 || observedAsk >= 1.0 {
		return 0
	}
	limit := observedAsk + normalizeCopytradeMaxSlippagePct(maxSlippagePct)/100.0
	if limit > 0.99 {
		limit = 0.99
	}
	if limit < observedAsk {
		limit = observedAsk
	}
	return limit
}

func CopytradeSellFloorPrice(observedBid, maxSlippagePct float64) float64 {
	if observedBid <= 0 || observedBid >= 1.0 {
		return 0
	}
	floor := observedBid - normalizeCopytradeMaxSlippagePct(maxSlippagePct)/100.0
	if floor < 0.01 {
		floor = 0.01
	}
	if floor > observedBid {
		floor = observedBid
	}
	return floor
}

func normalizeBinanceQuoteAsset(raw string) string {
	raw = strings.ToUpper(strings.TrimSpace(raw))
	if raw == "" {
		return "USDT"
	}
	return raw
}

func normalizeBinanceSignalThresholdPct(v float64) float64 {
	if v <= 0 {
		return 0.02
	}
	if v > 10 {
		return 10
	}
	return v
}

func normalizePaperBinanceExecutionDelayMs(v int) int {
	switch {
	case v < 0:
		return 0
	case v > 5000:
		v = 5000
	}
	return int(math.Round(float64(v)/10.0) * 10.0)
}

func normalizeBinanceSignalLookbackMs(v int) int {
	switch {
	case v <= 0:
		return 1500
	case v < 250:
		return 250
	case v > 15000:
		return 15000
	default:
		return v
	}
}

func normalizeBinanceSignalCooldownMs(v int) int {
	switch {
	case v <= 0:
		return 2500
	case v < 250:
		return 250
	case v > 60000:
		return 60000
	default:
		return v
	}
}

func normalizeBinanceSignalMaxAgeMs(v int) int {
	switch {
	case v <= 0:
		return 3000
	case v < 250:
		return 250
	case v > 60000:
		return 60000
	default:
		return v
	}
}

func normalizeBinanceSignalPolyMaxMoveCents(v float64) float64 {
	if v <= 0 {
		return 0
	}
	if v > 25 {
		return 25
	}
	return v
}

func normalizeBinanceSignalPolyAdverseMoveCents(v float64) float64 {
	if v <= 0 {
		return 0
	}
	if v > 25 {
		return 25
	}
	return v
}

func normalizeBinanceSignalSpreadMaxCents(v float64) float64 {
	if v <= 0 {
		return 4.0
	}
	if v > 50 {
		return 50
	}
	return v
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

func parseEnvBool(key string, defaultVal bool) bool {
	val := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if val == "" {
		return defaultVal
	}
	switch val {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		return defaultVal
	}
}

func parseEnvString(key string, defaultVal string) string {
	val := os.Getenv(key)
	if val == "" {
		return defaultVal
	}
	return val
}

func (c *Config) runtimeSettings() RuntimeSettings {
	return RuntimeSettings{
		Exchange:                          c.Exchange,
		MarketSlug:                        c.MarketSlug,
		Timeframe:                         c.Timeframe,
		MaxMarkets:                        c.MaxMarkets,
		BaseBalance:                       c.BaseBalance,
		BaseTradeSize:                     c.BaseTradeSize,
		PaperBalance:                      normalizePaperBalance(c.PaperBalance),
		MinMarginPercent:                  c.MinMarginPercent,
		TradeScaleFactor:                  c.TradeScaleFactor,
		TradeSizingMode:                   normalizeTradeSizingMode(c.TradeSizingMode),
		TradeSizeUSDC:                     normalizeFixedTradeSizeUSDC(c.TradeSizeUSDC),
		FeeRateBps:                        c.FeeRateBps,
		MaxTradeSize:                      c.MaxTradeSize,
		MaxDailyLoss:                      c.MaxDailyLoss,
		RequireConfirm:                    c.RequireConfirm,
		EnableCSVLogger:                   c.EnableCSVLogger,
		EnableRawAPILog:                   c.EnableRawAPILog,
		ExecutionLocalQuoteMaxAgeMs:       c.ExecutionLocalQuoteMaxAgeMs,
		RestFallbackQuoteAgeMs:            c.RestFallbackQuoteAgeMs,
		RestFallbackPollIntervalMs:        c.RestFallbackPollIntervalMs,
		EnableMarginAggression:            c.EnableMarginAggression,
		MaxAggressionMultiplier:           c.MaxAggressionMultiplier,
		MinAskPrice:                       c.MinAskPrice,
		MaxAskPrice:                       c.MaxAskPrice,
		PaperArbMode:                      c.PaperArbMode,
		BuyExecutionMarginFloorPercent:    c.BuyExecutionMarginFloorPercent,
		SplitStrategyEnabled:              c.SplitStrategyEnabled,
		SplitMinMarginSell:                c.SplitMinMarginSell,
		SplitTargetMarginReserve:          c.SplitTargetMarginReserve,
		SplitReplenishThreshold:           c.SplitReplenishThreshold,
		SplitMergeBufferSeconds:           c.SplitMergeBufferSeconds,
		MakerMergeBufferSeconds:           c.MakerMergeBufferSeconds,
		MakerQuoteGap:                     c.MakerQuoteGap,
		MakerInventoryTargetMult:          c.MakerInventoryTargetMult,
		MakerInventoryCapMult:             c.MakerInventoryCapMult,
		MakerMinQuoteValue:                c.MakerMinQuoteValue,
		SplitInitialCapPct:                c.SplitInitialCapPct,
		SplitReplenishCapPct:              c.SplitReplenishCapPct,
		TradingHoursMode:                  c.TradingHoursMode,
		TakerCloseMarket:                  c.TakerCloseMarket,
		TakerCloseMarketTime:              c.TakerCloseMarketTime,
		TakerCloseMarketSlippage:          c.TakerCloseMarketSlippage,
		TakerCloseMarketMinPrice:          c.TakerCloseMarketMinPrice,
		CopytradeTarget:                   strings.TrimSpace(c.CopytradeTarget),
		CopytradePollIntervalMs:           normalizeCopytradePollIntervalMs(c.CopytradePollIntervalMs),
		CopytradeMaxSlippagePct:           normalizeCopytradeMaxSlippagePct(c.CopytradeMaxSlippagePct),
		CopytradeSizingMode:               normalizeCopytradeSizingMode(c.CopytradeSizingMode),
		CopytradeSizeUSDC:                 normalizeCopytradeSizeUSDC(c.CopytradeSizeUSDC),
		CopytradeSizeShares:               normalizeCopytradeSizeShares(c.CopytradeSizeShares),
		CopytradeSizePercent:              normalizeCopytradeSizePercent(c.CopytradeSizePercent),
		LadderedTakerSizingMode:           normalizeLadderedTakerSizingMode(c.LadderedTakerSizingMode),
		LadderedTakerSizeUSDC:             normalizeLadderedTakerSizeUSDC(c.LadderedTakerSizeUSDC),
		LadderedTakerSizeShares:           normalizeLadderedTakerSizeShares(c.LadderedTakerSizeShares),
		LadderedTakerReentryMoveCents:     normalizeLadderedTakerReentryMoveCents(c.LadderedTakerReentryMoveCents),
		LadderedTakerMaxSlippagePct:       normalizeLadderedTakerMaxSlippagePct(c.LadderedTakerMaxSlippagePct),
		BinanceQuoteAsset:                 normalizeBinanceQuoteAsset(c.BinanceQuoteAsset),
		BinanceSignalThresholdPct:         normalizeBinanceSignalThresholdPct(c.BinanceSignalThresholdPct),
		PaperBinanceExecutionDelayMs:      normalizePaperBinanceExecutionDelayMs(c.PaperBinanceExecutionDelayMs),
		BinanceSignalLookbackMs:           normalizeBinanceSignalLookbackMs(c.BinanceSignalLookbackMs),
		BinanceSignalCooldownMs:           normalizeBinanceSignalCooldownMs(c.BinanceSignalCooldownMs),
		BinanceSignalMaxAgeMs:             normalizeBinanceSignalMaxAgeMs(c.BinanceSignalMaxAgeMs),
		BinanceSignalPolyMaxMoveCents:     normalizeBinanceSignalPolyMaxMoveCents(c.BinanceSignalPolyMaxMoveCents),
		BinanceSignalPolyAdverseMoveCents: normalizeBinanceSignalPolyAdverseMoveCents(c.BinanceSignalPolyAdverseMoveCents),
		BinanceSignalSpreadMaxCents:       normalizeBinanceSignalSpreadMaxCents(c.BinanceSignalSpreadMaxCents),
		StartupWizardSeen:                 c.StartupWizardSeen,
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
	c.PaperBalance = normalizePaperBalance(s.PaperBalance)
	c.MinMarginPercent = s.MinMarginPercent
	c.TradeScaleFactor = s.TradeScaleFactor
	c.TradeSizingMode = normalizeTradeSizingMode(s.TradeSizingMode)
	c.TradeSizeUSDC = normalizeFixedTradeSizeUSDC(s.TradeSizeUSDC)
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
	c.TradingHoursMode = s.TradingHoursMode
	c.TakerCloseMarket = s.TakerCloseMarket
	c.TakerCloseMarketTime = s.TakerCloseMarketTime
	c.TakerCloseMarketSlippage = s.TakerCloseMarketSlippage
	c.TakerCloseMarketMinPrice = s.TakerCloseMarketMinPrice
	c.CopytradeTarget = strings.TrimSpace(s.CopytradeTarget)
	c.CopytradePollIntervalMs = normalizeCopytradePollIntervalMs(s.CopytradePollIntervalMs)
	c.CopytradeMaxSlippagePct = normalizeCopytradeMaxSlippagePct(s.CopytradeMaxSlippagePct)
	c.CopytradeSizingMode = normalizeCopytradeSizingMode(s.CopytradeSizingMode)
	c.CopytradeSizeUSDC = normalizeCopytradeSizeUSDC(s.CopytradeSizeUSDC)
	c.CopytradeSizeShares = normalizeCopytradeSizeShares(s.CopytradeSizeShares)
	c.CopytradeSizePercent = normalizeCopytradeSizePercent(s.CopytradeSizePercent)
	c.LadderedTakerSizingMode = normalizeLadderedTakerSizingMode(s.LadderedTakerSizingMode)
	c.LadderedTakerSizeUSDC = normalizeLadderedTakerSizeUSDC(s.LadderedTakerSizeUSDC)
	c.LadderedTakerSizeShares = normalizeLadderedTakerSizeShares(s.LadderedTakerSizeShares)
	c.LadderedTakerReentryMoveCents = normalizeLadderedTakerReentryMoveCents(s.LadderedTakerReentryMoveCents)
	c.LadderedTakerMaxSlippagePct = normalizeLadderedTakerMaxSlippagePct(s.LadderedTakerMaxSlippagePct)
	c.BinanceQuoteAsset = normalizeBinanceQuoteAsset(s.BinanceQuoteAsset)
	c.BinanceSignalThresholdPct = normalizeBinanceSignalThresholdPct(s.BinanceSignalThresholdPct)
	c.PaperBinanceExecutionDelayMs = normalizePaperBinanceExecutionDelayMs(s.PaperBinanceExecutionDelayMs)
	c.BinanceSignalLookbackMs = normalizeBinanceSignalLookbackMs(s.BinanceSignalLookbackMs)
	c.BinanceSignalCooldownMs = normalizeBinanceSignalCooldownMs(s.BinanceSignalCooldownMs)
	c.BinanceSignalMaxAgeMs = normalizeBinanceSignalMaxAgeMs(s.BinanceSignalMaxAgeMs)
	c.BinanceSignalPolyMaxMoveCents = normalizeBinanceSignalPolyMaxMoveCents(s.BinanceSignalPolyMaxMoveCents)
	c.BinanceSignalPolyAdverseMoveCents = normalizeBinanceSignalPolyAdverseMoveCents(s.BinanceSignalPolyAdverseMoveCents)
	c.BinanceSignalSpreadMaxCents = normalizeBinanceSignalSpreadMaxCents(s.BinanceSignalSpreadMaxCents)
	c.StartupWizardSeen = s.StartupWizardSeen

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
	envMap["PAPER_BALANCE"] = strconv.FormatFloat(normalizePaperBalance(c.PaperBalance), 'f', -1, 64)
	envMap["TRADE_SCALE_FACTOR"] = strconv.FormatFloat(c.TradeScaleFactor, 'f', -1, 64)
	envMap["TRADE_SIZING_MODE"] = normalizeTradeSizingMode(c.TradeSizingMode)
	envMap["TRADE_SIZE_USDC"] = strconv.FormatFloat(normalizeFixedTradeSizeUSDC(c.TradeSizeUSDC), 'f', -1, 64)
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
	envMap["TRADING_HOURS_MODE"] = c.TradingHoursMode
	envMap["COPYTRADE_TARGET"] = strings.TrimSpace(c.CopytradeTarget)
	envMap["COPYTRADE_POLL_INTERVAL_MS"] = strconv.Itoa(normalizeCopytradePollIntervalMs(c.CopytradePollIntervalMs))
	envMap["COPYTRADE_MAX_SLIPPAGE_PCT"] = strconv.FormatFloat(normalizeCopytradeMaxSlippagePct(c.CopytradeMaxSlippagePct), 'f', -1, 64)
	envMap["COPYTRADE_SIZING_MODE"] = normalizeCopytradeSizingMode(c.CopytradeSizingMode)
	envMap["COPYTRADE_SIZE_USDC"] = strconv.FormatFloat(normalizeCopytradeSizeUSDC(c.CopytradeSizeUSDC), 'f', -1, 64)
	envMap["COPYTRADE_SIZE_SHARES"] = strconv.FormatFloat(normalizeCopytradeSizeShares(c.CopytradeSizeShares), 'f', -1, 64)
	envMap["COPYTRADE_SIZE_PERCENT"] = strconv.FormatFloat(normalizeCopytradeSizePercent(c.CopytradeSizePercent), 'f', -1, 64)
	envMap["LADDERED_TAKER_SIZING_MODE"] = normalizeLadderedTakerSizingMode(c.LadderedTakerSizingMode)
	envMap["LADDERED_TAKER_SIZE_USDC"] = strconv.FormatFloat(normalizeLadderedTakerSizeUSDC(c.LadderedTakerSizeUSDC), 'f', -1, 64)
	envMap["LADDERED_TAKER_SIZE_SHARES"] = strconv.FormatFloat(normalizeLadderedTakerSizeShares(c.LadderedTakerSizeShares), 'f', -1, 64)
	envMap["LADDERED_TAKER_REENTRY_MOVE_CENTS"] = strconv.FormatFloat(normalizeLadderedTakerReentryMoveCents(c.LadderedTakerReentryMoveCents), 'f', -1, 64)
	envMap["LADDERED_TAKER_MAX_SLIPPAGE_PCT"] = strconv.FormatFloat(normalizeLadderedTakerMaxSlippagePct(c.LadderedTakerMaxSlippagePct), 'f', -1, 64)
	envMap["BINANCE_QUOTE_ASSET"] = normalizeBinanceQuoteAsset(c.BinanceQuoteAsset)
	envMap["BINANCE_SIGNAL_THRESHOLD_PCT"] = strconv.FormatFloat(normalizeBinanceSignalThresholdPct(c.BinanceSignalThresholdPct), 'f', -1, 64)
	envMap["PAPER_BINANCE_EXECUTION_DELAY_MS"] = strconv.Itoa(normalizePaperBinanceExecutionDelayMs(c.PaperBinanceExecutionDelayMs))
	envMap["BINANCE_SIGNAL_LOOKBACK_MS"] = strconv.Itoa(normalizeBinanceSignalLookbackMs(c.BinanceSignalLookbackMs))
	envMap["BINANCE_SIGNAL_COOLDOWN_MS"] = strconv.Itoa(normalizeBinanceSignalCooldownMs(c.BinanceSignalCooldownMs))
	envMap["BINANCE_SIGNAL_MAX_AGE_MS"] = strconv.Itoa(normalizeBinanceSignalMaxAgeMs(c.BinanceSignalMaxAgeMs))
	envMap["BINANCE_SIGNAL_POLY_MAX_MOVE_CENTS"] = strconv.FormatFloat(normalizeBinanceSignalPolyMaxMoveCents(c.BinanceSignalPolyMaxMoveCents), 'f', -1, 64)
	envMap["BINANCE_SIGNAL_POLY_ADVERSE_MOVE_CENTS"] = strconv.FormatFloat(normalizeBinanceSignalPolyAdverseMoveCents(c.BinanceSignalPolyAdverseMoveCents), 'f', -1, 64)
	envMap["BINANCE_SIGNAL_SPREAD_MAX_CENTS"] = strconv.FormatFloat(normalizeBinanceSignalSpreadMaxCents(c.BinanceSignalSpreadMaxCents), 'f', -1, 64)

	return godotenv.Write(envMap, ".env")
}

func (c *Config) CalculateTradeSize(currentBalance float64) float64 {
	if c == nil {
		return CalculateTradeSizeForMode(currentBalance, 0.05, 1.0, 0, TradeSizingModePercent)
	}
	return CalculateTradeSizeForMode(currentBalance, c.TradeScaleFactor, c.TradeSizeUSDC, c.MaxTradeSize, c.TradeSizingMode)
}

func CalculateTradeSizeForMode(currentBalance, tradeScaleFactor, tradeSizeUSDC, maxTradeSize float64, tradeSizingMode string) float64 {
	var tradeSize float64
	switch normalizeTradeSizingMode(tradeSizingMode) {
	case TradeSizingModeUSDC:
		tradeSize = normalizeFixedTradeSizeUSDC(tradeSizeUSDC)
	default:
		if tradeScaleFactor <= 0 {
			tradeScaleFactor = 0.01
		}
		tradeSize = currentBalance * tradeScaleFactor
	}
	if maxTradeSize > 0 && tradeSize > maxTradeSize {
		tradeSize = maxTradeSize
	}
	if tradeSize < 0.1 {
		tradeSize = 0.1
	}
	return tradeSize
}

func CalculateCopytradeSharesForMode(targetShares, price, sizeUSDC, sizeShares, sizePercent, maxTradeSize float64, mode string) float64 {
	if price <= 0 {
		return 0
	}
	switch normalizeCopytradeSizingMode(mode) {
	case CopytradeSizingModeShares:
		return normalizeCopytradeSizeShares(sizeShares)
	case CopytradeSizingModePercent:
		if targetShares <= 0 {
			return 0
		}
		percent := normalizeCopytradeSizePercent(sizePercent)
		return targetShares * (percent / 100.0)
	default:
		budget := normalizeCopytradeSizeUSDC(sizeUSDC)
		if maxTradeSize > 0 && budget > maxTradeSize {
			budget = maxTradeSize
		}
		return budget / price
	}
}

func CalculateLadderedTakerSharesForMode(pairPrice, sizeUSDC, sizeShares, maxTradeSize float64, mode string) float64 {
	if pairPrice <= 0 {
		return 0
	}
	switch normalizeLadderedTakerSizingMode(mode) {
	case LadderedTakerSizingModeShares:
		return normalizeLadderedTakerSizeShares(sizeShares)
	default:
		budget := normalizeLadderedTakerSizeUSDC(sizeUSDC)
		if maxTradeSize > 0 && budget > maxTradeSize {
			budget = maxTradeSize
		}
		return budget / pairPrice
	}
}

func CalculateCopytradeSellSharesForMode(localShares, targetShares, targetDelta, price, sizeUSDC, sizeShares, sizePercent, maxTradeSize float64, mode string) float64 {
	if localShares <= 0 || price <= 0 {
		return 0
	}

	// When the target is fully flat, keep unwinding until our managed inventory
	// is fully flat too. Otherwise a capped first sell can strand a remainder
	// once the target delta goes back to zero on the next poll.
	if targetShares <= 0.01 {
		return localShares
	}

	// If using fixed sizing (shares or usdc), we must exit proportionally to the master's exit.
	// For example: if master drops from 300 to 240, they sold 60 shares (20% of their bag).
	// We must sell 20% of our localShares, otherwise we dump our fixed config size prematurely.
	if normalizeCopytradeSizingMode(mode) != CopytradeSizingModePercent {
		soldShares := math.Max(0, -targetDelta)
		previousMasterShares := targetShares + soldShares
		if previousMasterShares > 0.01 {
			proportion := soldShares / previousMasterShares
			calculated := localShares * proportion
			return math.Min(localShares, calculated)
		}
	}

	calculated := CalculateCopytradeSharesForMode(math.Max(0, -targetDelta), price, sizeUSDC, sizeShares, sizePercent, maxTradeSize, mode)
	return math.Min(localShares, calculated)
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
	default:
		return nil, fmt.Errorf("unknown bot profile: %s", profile)
	}
	if path == "" {
		return cfg, nil
	}
	settings, err := readRuntimeSettings(path, cfg.runtimeSettings())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return cfg, nil
	}
	cfg.applyRuntimeSettings(settings)
	return cfg, nil
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

func ResolveBinanceSignalLookback(cfg *Config) time.Duration {
	if cfg != nil {
		return time.Duration(normalizeBinanceSignalLookbackMs(cfg.BinanceSignalLookbackMs)) * time.Millisecond
	}
	return time.Duration(normalizeBinanceSignalLookbackMs(0)) * time.Millisecond
}

func ResolveBinanceSignalCooldown(cfg *Config) time.Duration {
	if cfg != nil {
		return time.Duration(normalizeBinanceSignalCooldownMs(cfg.BinanceSignalCooldownMs)) * time.Millisecond
	}
	return time.Duration(normalizeBinanceSignalCooldownMs(0)) * time.Millisecond
}

func ResolveBinanceSignalMaxAge(cfg *Config) time.Duration {
	if cfg != nil {
		return time.Duration(normalizeBinanceSignalMaxAgeMs(cfg.BinanceSignalMaxAgeMs)) * time.Millisecond
	}
	return time.Duration(normalizeBinanceSignalMaxAgeMs(0)) * time.Millisecond
}

func ResolvePaperBinanceExecutionDelay(cfg *Config) time.Duration {
	if cfg != nil {
		return time.Duration(normalizePaperBinanceExecutionDelayMs(cfg.PaperBinanceExecutionDelayMs)) * time.Millisecond
	}
	return time.Duration(normalizePaperBinanceExecutionDelayMs(250)) * time.Millisecond
}

