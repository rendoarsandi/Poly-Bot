package paper

import (
	"time"
)

// MarketData holds live data for a single market.
type MarketData struct {
	Slug            string
	Outcomes        []string
	EndTime         time.Time
	InventoryStatus string
	Bids            map[string]float64
	Asks            map[string]float64
	ClearedBids     map[string]bool
	ClearedAsks     map[string]bool
	RealBids        map[string]float64
	RealAsks        map[string]float64
	LastUpdate      time.Time
	LastDepthUpdate time.Time
	DataSource      string // "WS" or "REST"
	BinanceSignal   MarketBinanceSignal
}

type MarketBinanceSignal struct {
	Enabled                bool
	Symbol                 string
	Price                  float64
	DeltaPercent           float64
	EffectiveGapPercent    float64
	TargetOutcome          string
	SignalLabel            string
	PolyFavorableMoveCents float64
	PolyAdverseMoveCents   float64
	TargetSpreadCents      float64
	TargetBookImbalance    float64
	OppositeBookImbalance  float64
	DirectionalBookScore   float64
	Ready                  bool
	Status                 string
	Reason                 string
	UpdatedAt              time.Time
}

// PendingOrder represents an order the bot intends to place.
type PendingOrder struct {
	MarketID string
	Outcome  string
	Price    float64
	Qty      float64
	Side     string // "BUY" or "SELL"
}

type ScopedLimitOrder struct {
	MarketID string
	Order    *LimitOrder
}

// OrderHistoryEntry represents a completed trade.
type OrderHistoryEntry struct {
	Timestamp     time.Time
	MarketID      string
	MarketSlug    string
	Outcome       string
	Side          string
	ExecutionMode string
	Shares        float64
	Price         float64
	Cost          float64
	Margin        float64
	Profit        float64
	Status        string // "FILLED", "PARTIAL", "FAILED"
}

type RoundHistoryEntry struct {
	Number         int
	Timestamp      time.Time
	StartingEquity float64
	EndingEquity   float64
	PnL            float64
	Trades         int
	ShareSummary   string

	positions   map[string]Position
	redemptions []*RedemptionResult
}

// TUISettings holds runtime-adjustable trading parameters.
// These can be changed live from the settings panel (press 's').
type TUISettings struct {
	Exchange                           string  // "polymarket" or "kalshi"
	ExecutionBackend                   string  // "live" or "paper"
	MarketSlug                         string  // Current selected market slug or ALL or BTC,ETH
	MaxMarkets                         int     // Max concurrent markets to trade
	PaperBalance                       float64 // Paper-only bankroll / session reset amount
	Timeframe                          string  // "5m", "15m", "1h", "4h", or "1d"
	TradeSizingMode                    string  // "percent" or "usdc"
	TradeScaleFactor                   float64 // e.g. 0.05 = 5% of equity per trade
	TradeSizeUSDC                      float64 // Fixed per-trade USDC amount when TradeSizingMode == "usdc"
	MinMarginPercent                   float64 // e.g. 2.0 = require 2% arb margin
	BinanceSignalThresholdPct          float64 // e.g. 0.02 = require 0.02% Binance move in binance-gap mode
	PaperBinanceExecutionDelayMs       int     // Paper-only execution delay after Binance-gap signal is detected
	PaperArbMode                       string  // taker, laddered-taker, binance-gap, copytrade, or maker
	CopytradeTarget                    string  // wallet address, profile handle, or profile URL
	CopytradeUseMempool                bool    // whether to watch mempool via Alchemy
	CopytradePollIntervalMs            int     // public-wallet poll interval for copytrade mode
	CopytradeSizingMode                string  // "usdc" or "shares" when PaperArbMode == copytrade
	CopytradeSizeUSDC                  float64 // fixed per-trade copytrade budget when sizing by USDC
	CopytradeSizeShares                float64 // fixed per-trade copytrade share cap when sizing by shares
	CopytradeSizePercent               float64 // percent of the master/target trade size when sizing by percent
	CopytradeMaxSlippagePct            float64 // legacy field name; interpreted as absolute copytrade slippage allowance in cents
	LadderedTakerSizingMode            string  // "usdc" or "shares" when PaperArbMode == laddered-taker
	LadderedTakerSizeUSDC              float64 // fixed per-entry paired budget when laddered taker uses USDC sizing
	LadderedTakerSizeShares            float64 // fixed paired share cap when laddered taker uses share sizing
	LadderedTakerReentryMoveCents      float64 // minimum quote movement in cents required before the next laddered entry
	LadderedTakerMaxSlippagePct        float64 // maximum slippage in cents for laddered taker
	LadderedTakerPnLGuardMode          string  // "worst-pnl" or "max-profit-pnl" for laddered taker entry blocking
	LadderedTakerWorstPnLFloor         float64 // 0 = no safety guard (DISABLED), otherwise block entries below this projected worst-case resolve PnL
	LadderedTakerMaxProfitPnL          float64 // 0 = no safety guard (DISABLED), otherwise require projected winning-side resolve PnL cap
	LadderedTakerHedgeBypass           bool    // true = bypass PnL floor guard for risk-reducing or risk-neutral trades
	BuyExecutionMarginFloorPercent     float64 // e.g. -1.0 = allow buy/sell execution to slip to -1% pair margin
	SplitMinMarginSell                 float64 // e.g. 3.0 = sell splits at 3% margin
	SplitStrategyEnabled               bool    // toggle split strategy on/off
	SplitInitialCapPct                 float64 // Initial Split Cap percentage
	SplitReplenishCapPct               float64 // Replenishment Cap percentage
	TradingHoursMode                   string  // "off", "weekdays trade only", "us open only", or Jakarta "HH:MM-HH:MM"
	MakerMergeBufferSeconds            int     // seconds before expiry to merge paired maker inventory
	MakerQuoteGap                      float64 // distance from mid for maker quotes
	MakerInventoryTargetMult           float64
	MakerInventoryCapMult              float64
	MakerMinQuoteValue                 float64
	MinAskPrice                        float64 // e.g. 0.10 = minimum ask price filter
	MaxAskPrice                        float64 // e.g. 0.90 = maximum ask price filter
	MaxTradeSize                       float64 // e.g. 50.00 = max trade size $50
	MaxDailyLoss                       float64 // e.g. 0.0 = disabled, else max drawdown limit
	TakerCloseMarket                   bool    // e.g. force buy higher side close to end
	BlockNewEntriesOnPendingRedemption bool    // block fresh entries while prior-round inventory is still awaiting redemption
	RedeemEntryTiming                  string  // when wait-redeem is ON: "immediate" or "next-market" re-entry behavior
	RedeemGasMode                      string  // "normal", "fast", or "urgent" gas profile for live redeems
	OneHourCryptoExitMode              string  // "sell-999" or "wait-resolve" for 1h laddered crypto exits
	TakerCloseMarketTime               int     // e.g. 5 seconds
	TakerCloseMarketSlippage           float64 // e.g. 0.99 limit price
	TakerCloseMarketMinPrice           float64 // e.g. 0.60 min spike price
	TakerCloseSizingMode               string  // "percent", "usdc", or "shares" when taker close is enabled
	TakerCloseSizeUSDC                 float64 // fixed close-market budget when sizing by USDC
	TakerCloseSizeShares               float64 // fixed close-market share cap when sizing by shares
	PolygonRPC                         string  // Editable RPC URL
	PolygonPrivateKey                  string  // Editable Private Key
}

type WalletTruthPosition struct {
	MarketID         string
	Outcome          string
	LocalShares      float64
	OnChainShares    float64
	Drift            float64
	Redeemable       bool
	IsWinner         bool   // This outcome is the winning side (from on-chain/API resolution)
	ResolutionStatus string // "unresolved", "resolved", "redeemable"
}
