package paper

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"Market-bot/internal/core"
)

// ─── Design Tokens ────────────────────────────────────────────────────────────
// True-color palette — falls back gracefully to nearest ANSI in 256-color terms.
var (
	clrBrand   = lipgloss.Color("#7C3AED") // purple  – brand / title
	clrTeal    = lipgloss.Color("#06B6D4") // cyan    – ETH, section labels
	clrEmerald = lipgloss.Color("#10B981") // green   – positive / bid
	clrRose    = lipgloss.Color("#EF4444") // red     – negative / ask / kill
	clrAmber   = lipgloss.Color("#F59E0B") // amber   – BTC / warning
	clrOrange  = lipgloss.Color("#F97316") // orange  – SOL / mid-warning
	clrBlue    = lipgloss.Color("#60A5FA") // blue    – XRP
	clrSlate   = lipgloss.Color("#6B7280") // slate   – muted text
	clrWhite   = lipgloss.Color("#F3F4F6") // near-white – primary text
	clrDim     = lipgloss.Color("#9CA3AF") // dim     – labels / secondary

	// ── Plain text styles (names kept for package-internal compatibility) ──
	styleRed     = lipgloss.NewStyle().Foreground(clrRose)
	styleGreen   = lipgloss.NewStyle().Foreground(clrEmerald)
	styleYellow  = lipgloss.NewStyle().Foreground(clrAmber)
	styleMagenta = lipgloss.NewStyle().Foreground(clrOrange) // SOL
	styleCyan    = lipgloss.NewStyle().Foreground(clrTeal)
	styleWhite   = lipgloss.NewStyle().Foreground(clrWhite)
	styleMuted   = lipgloss.NewStyle().Foreground(clrSlate)
	styleDimmed  = lipgloss.NewStyle().Foreground(clrDim)
	styleBold    = lipgloss.NewStyle().Bold(true).Foreground(clrWhite)

	// Kill banner
	styleBgRedBold = lipgloss.NewStyle().
			Background(clrRose).
			Foreground(lipgloss.Color("#FFFFFF")).
			Bold(true).
			Padding(0, 2)

	styleBoldGreen = lipgloss.NewStyle().Bold(true).Foreground(clrBlue) // XRP

	// Per-asset border colors for market panels
	assetBorderColors = map[string]lipgloss.Color{
		"BTC": clrAmber,
		"ETH": clrTeal,
		"SOL": clrOrange,
		"XRP": clrBlue,
	}
)

const (
	defaultMaxOrderHistory  = 50
	defaultMaxRoundHistory  = 50
	defaultMaxEventHistory  = 250
	defaultTwoColRoundRows  = 4
	defaultOneColRoundRows  = 3
	defaultTwoColOrderRows  = 12
	defaultOneColOrderRows  = 10
	defaultTwoColEventRows  = 4
	defaultOneColEventRows  = 3
	recentQuoteDisplayGrace = 10 * time.Second
	terminalBidFloor        = 0.985
	terminalAskCeil         = 0.015
	inventoryDustCutoff     = 0.01
	showOnChainInventory    = true
	showWalletTruthPanels   = false
)

// ─── Asset style helpers ──────────────────────────────────────────────────────

func getAssetStyle(id string) lipgloss.Style {
	switch marketAssetID(id) {
	case "BTC":
		return styleYellow
	case "ETH":
		return styleCyan
	case "SOL":
		return styleMagenta
	case "XRP":
		return styleBoldGreen.UnsetBold()
	default:
		return styleWhite
	}
}

func marketAssetID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "UNKNOWN"
	}
	for _, sep := range []string{"#", ":", "/"} {
		if idx := strings.Index(id, sep); idx > 0 {
			return strings.ToUpper(strings.TrimSpace(id[:idx]))
		}
	}
	return strings.ToUpper(id)
}

func marketSortRank(id string) int {
	switch marketAssetID(id) {
	case "BTC":
		return 0
	case "ETH":
		return 1
	case "SOL":
		return 2
	case "XRP":
		return 3
	case "UNKNOWN":
		return 5
	default:
		return 4
	}
}

func outcomeSortRank(outcome string) int {
	switch strings.ToLower(strings.TrimSpace(outcome)) {
	case "up", "yes":
		return 0
	case "down", "no":
		return 1
	default:
		return 2
	}
}

func outcomeSortLess(a, b string) bool {
	rankA := outcomeSortRank(a)
	rankB := outcomeSortRank(b)
	if rankA != rankB {
		return rankA < rankB
	}
	return strings.TrimSpace(a) < strings.TrimSpace(b)
}

func orderedMarketIDs(ids []string) []string {
	sorted := append([]string(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool {
		ri := marketSortRank(sorted[i])
		rj := marketSortRank(sorted[j])
		if ri != rj {
			return ri < rj
		}
		return sorted[i] < sorted[j]
	})
	return sorted
}

func marketDisplayLabel(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "UNKNOWN"
	}
	return id
}

func truncateMarketLabel(id string, maxLen int) string {
	label := marketDisplayLabel(id)
	if maxLen > 0 && len(label) > maxLen {
		return label[:maxLen]
	}
	return label
}

func truncateText(s string, maxLen int) string {
	if maxLen <= 0 || len(s) <= maxLen {
		return s
	}
	if maxLen == 1 {
		return s[:1]
	}
	return s[:maxLen-1] + "…"
}

func orderHistoryMarketLabel(marketID, marketSlug string) string {
	if suffix := marketTimeSuffix(marketSlug); suffix != "" {
		return suffix
	}
	if suffix := marketTimeSuffix(marketID); suffix != "" {
		return suffix
	}
	return marketDisplayLabel(marketID)
}

func marketTimeSuffix(raw string) string {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return ""
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '-' || r == '#' || r == ':' || r == '/'
	})
	if len(parts) == 0 {
		return ""
	}
	suffix := strings.TrimSpace(parts[len(parts)-1])
	if suffix == "" {
		return ""
	}
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return suffix
}

func resolveOrderHistoryMarketSlug(entry OrderHistoryEntry, markets map[string]*MarketData) string {
	if slug := strings.TrimSpace(entry.MarketSlug); slug != "" {
		return slug
	}
	marketID := strings.TrimSpace(entry.MarketID)
	if marketID == "" || markets == nil {
		return ""
	}
	if market, ok := markets[marketID]; ok && market != nil {
		return strings.TrimSpace(market.Slug)
	}
	return ""
}

func marginStyle(pct float64) lipgloss.Style {
	switch {
	case pct >= 3:
		return styleGreen
	case pct >= 2:
		return styleYellow
	case pct < 1:
		return styleRed
	default:
		return styleWhite
	}
}

func recentDisplayQuote(current, lastGood float64, age time.Duration, cleared bool) float64 {
	if current > 0 {
		return current
	}
	if cleared {
		return current
	}
	if lastGood > 0 && age <= recentQuoteDisplayGrace {
		return lastGood
	}
	return current
}

func signedDollar(amount float64) string {
	sign := "+"
	if amount < 0 {
		sign = "-"
		amount = math.Abs(amount)
	}
	return fmt.Sprintf("%s$%.2f", sign, amount)
}

func formatDrawdownCash(amount float64) string {
	if math.Abs(amount) < 0.0001 {
		return "$0.00"
	}
	if amount < 0 {
		amount = math.Abs(amount)
	}
	return fmt.Sprintf("-$%.2f", amount)
}

func formatBinanceSignalPrice(symbol string, price float64) string {
	if price <= 0 {
		return "--"
	}

	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	switch {
	case strings.HasPrefix(symbol, "XRP"):
		return fmt.Sprintf("%.4f", price)
	case price >= 10:
		return fmt.Sprintf("%.2f", price)
	case price >= 1:
		return fmt.Sprintf("%.4f", price)
	case price >= 0.1:
		return fmt.Sprintf("%.4f", price)
	default:
		return fmt.Sprintf("%.5f", price)
	}
}

func renderTradingHoursStatus(mode string, now time.Time) string {
	usNow := core.USTime(now)
	usClock := usNow.Format("Mon 2006-01-02 15:04:05 MST")

	if mode == "off" {
		return styleDimmed.Render("US time " + usClock + "  ·  Trading Gate OFF (24/7)")
	}

	if mode == "weekdays trade only" {
		if core.IsUSWeekday(usNow) {
			return styleGreen.Render("US time " + usClock + "  ·  Weekday Gate OPEN (trading enabled)")
		}
		return styleRed.Render("US time " + usClock + "  ·  Weekday Gate CLOSED (weekend, trading blocked)")
	}

	if mode == "us open only" {
		if core.IsUSMarketOpen(now) {
			return styleGreen.Render("US time " + usClock + "  ·  US Market Gate OPEN (trading enabled)")
		}
		return styleRed.Render("US time " + usClock + "  ·  US Market Gate CLOSED (outside hours, trading blocked)")
	}

	return styleDimmed.Render("US time " + usClock + "  ·  Trading Gate OFF")
}

func walletTruthInventoryDisplayShares(wt WalletTruthPosition) (float64, bool) {
	switch {
	case displayableInventoryShares(wt.OnChainShares):
		if wt.ResolutionStatus == "resolved" && !wt.IsWinner && !wt.Redeemable {
			return 0, false
		}
		return wt.OnChainShares, true
	case displayableInventoryShares(wt.LocalShares) && wt.ResolutionStatus != "resolved":
		return wt.LocalShares, true
	default:
		return 0, false
	}
}

func displayableInventoryShares(qty float64) bool {
	return qty >= inventoryDustCutoff-1e-9
}

func walletTruthInventoryStatus(wt WalletTruthPosition) string {
	switch {
	case !displayableInventoryShares(wt.OnChainShares) && displayableInventoryShares(wt.LocalShares) && wt.ResolutionStatus != "resolved":
		return styleYellow.Render("[SYNCING]")
	case wt.Redeemable:
		return styleGreen.Render("[REDEEMABLE]")
	case wt.IsWinner:
		return styleGreen.Render("[WINNER]")
	case wt.ResolutionStatus == "resolved":
		return styleRed.Render("[LOSER]")
	default:
		return styleYellow.Render("[OPEN]")
	}
}

func looksTerminalBook(outcomes []string, bids, asks map[string]float64) bool {
	if len(outcomes) == 0 {
		return false
	}

	sawExtreme := false
	for _, outcome := range outcomes {
		bid := bids[outcome]
		ask := asks[outcome]

		if bid > 0 && bid < terminalBidFloor {
			return false
		}
		if ask > 0 && ask > terminalAskCeil {
			return false
		}
		if bid >= terminalBidFloor || (ask > 0 && ask <= terminalAskCeil) {
			sawExtreme = true
		}
	}

	return sawExtreme
}

// ─── Panel utilities ──────────────────────────────────────────────────────────

// makePanel wraps content in a rounded-border box.
// innerWidth is the CONTENT width; total rendered width = innerWidth + 4.
func makePanel(innerWidth int, borderColor lipgloss.Color, content string) string {
	if innerWidth < 4 {
		innerWidth = 4
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Padding(0, 1).
		Width(innerWidth).
		MaxWidth(innerWidth).
		Render(content)
}

// sectionHeader returns a colored bold label for use as the first line of a panel.
func sectionHeader(icon, label string, color lipgloss.Color) string {
	return lipgloss.NewStyle().Bold(true).Foreground(color).
		Render(icon + " " + label)
}

// renderBar draws [███░░░] of the given visual width at pct fill (0–1).
func renderBar(pct float64, width int) string {
	if width < 1 {
		return ""
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 1 {
		pct = 1
	}
	n := int(pct * float64(width))
	if n > width {
		n = width
	}
	return styleGreen.Render(strings.Repeat("█", n)) +
		styleMuted.Render(strings.Repeat("░", width-n))
}

// latencyDot returns a colored ● for a latency value.
func latencyDot(d time.Duration, warnMs, critMs int64) (string, lipgloss.Style) {
	ms := d.Milliseconds()
	st := styleGreen
	if d == 0 {
		st = styleMuted
	} else if ms >= critMs {
		st = styleRed
	} else if ms >= warnMs {
		st = styleYellow
	}
	return "●", st
}

// ─── Data Types ───────────────────────────────────────────────────────────────

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
	Timeframe                          string  // "5m", "15m", or "1h"
	TradeSizingMode                    string  // "percent" or "usdc"
	TradeScaleFactor                   float64 // e.g. 0.05 = 5% of equity per trade
	TradeSizeUSDC                      float64 // Fixed per-trade USDC amount when TradeSizingMode == "usdc"
	MinMarginPercent                   float64 // e.g. 2.0 = require 2% arb margin
	BinanceSignalThresholdPct          float64 // e.g. 0.02 = require 0.02% Binance move in binance-gap mode
	PaperBinanceExecutionDelayMs       int     // Paper-only execution delay after Binance-gap signal is detected
	PaperArbMode                       string  // taker, laddered-taker, binance-gap, copytrade, or maker
	CopytradeTarget                    string  // wallet address, profile handle, or profile URL
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
	BuyExecutionMarginFloorPercent     float64 // e.g. -1.0 = allow buy/sell execution to slip to -1% pair margin
	SplitMinMarginSell                 float64 // e.g. 3.0 = sell splits at 3% margin
	SplitStrategyEnabled               bool    // toggle split strategy on/off
	SplitInitialCapPct                 float64 // Initial Split Cap percentage
	SplitReplenishCapPct               float64 // Replenishment Cap percentage
	TradingHoursMode                   string  // "off", "weekdays trade only", "us open only"
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
	TakerCloseMarketTime               int     // e.g. 5 seconds
	TakerCloseMarketSlippage           float64 // e.g. 0.99 limit price
	TakerCloseMarketMinPrice           float64 // e.g. 0.60 min spike price
	PolygonRPC                         string  // Editable RPC URL
	PolygonPrivateKey                  string  // Editable Private Key
}

// Preset quick-select settings.
var (
	SettingsConservative = TUISettings{Exchange: "polymarket", ExecutionBackend: core.ExecutionBackendPaper, MarketSlug: "ALL", MaxMarkets: 2, Timeframe: "15m", TradeSizingMode: core.TradeSizingModePercent, TradeScaleFactor: 0.01, TradeSizeUSDC: 1.0, MinMarginPercent: 3.0, BinanceSignalThresholdPct: 0.12, PaperBinanceExecutionDelayMs: 250, PaperArbMode: "taker", CopytradePollIntervalMs: 2000, CopytradeSizingMode: core.CopytradeSizingModeUSDC, CopytradeSizeUSDC: 1.0, CopytradeSizeShares: 1.0, CopytradeSizePercent: 100.0, CopytradeMaxSlippagePct: 1.0, LadderedTakerSizingMode: core.LadderedTakerSizingModeUSDC, LadderedTakerSizeUSDC: 1.0, LadderedTakerSizeShares: 1.0, LadderedTakerReentryMoveCents: 1.0, LadderedTakerMaxSlippagePct: 1.0, BuyExecutionMarginFloorPercent: -0.01, SplitMinMarginSell: 5.0, MakerMergeBufferSeconds: 30, MakerQuoteGap: 0.008, MakerInventoryTargetMult: 3.0, MakerInventoryCapMult: 5.0, MakerMinQuoteValue: 5.0, MinAskPrice: 0.10, MaxAskPrice: 0.90, TradingHoursMode: "weekdays trade only", TakerCloseMarket: false, TakerCloseMarketTime: 5, TakerCloseMarketSlippage: 0.99, TakerCloseMarketMinPrice: 0.60}
	SettingsModerate     = TUISettings{Exchange: "polymarket", ExecutionBackend: core.ExecutionBackendPaper, MarketSlug: "ALL", MaxMarkets: 4, Timeframe: "15m", TradeSizingMode: core.TradeSizingModePercent, TradeScaleFactor: 0.05, TradeSizeUSDC: 5.0, MinMarginPercent: 2.0, BinanceSignalThresholdPct: 0.08, PaperBinanceExecutionDelayMs: 250, PaperArbMode: "taker", CopytradePollIntervalMs: 2000, CopytradeSizingMode: core.CopytradeSizingModeUSDC, CopytradeSizeUSDC: 5.0, CopytradeSizeShares: 5.0, CopytradeSizePercent: 100.0, CopytradeMaxSlippagePct: 1.0, LadderedTakerSizingMode: core.LadderedTakerSizingModeUSDC, LadderedTakerSizeUSDC: 5.0, LadderedTakerSizeShares: 5.0, LadderedTakerReentryMoveCents: 1.0, LadderedTakerMaxSlippagePct: 1.0, BuyExecutionMarginFloorPercent: -0.01, SplitMinMarginSell: 3.0, MakerMergeBufferSeconds: 30, MakerQuoteGap: 0.008, MakerInventoryTargetMult: 3.0, MakerInventoryCapMult: 5.0, MakerMinQuoteValue: 5.0, MinAskPrice: 0.10, MaxAskPrice: 0.90, TradingHoursMode: "weekdays trade only", TakerCloseMarket: false, TakerCloseMarketTime: 5, TakerCloseMarketSlippage: 0.99, TakerCloseMarketMinPrice: 0.60}
	SettingsAggressive   = TUISettings{Exchange: "polymarket", ExecutionBackend: core.ExecutionBackendPaper, MarketSlug: "ALL", MaxMarkets: 4, Timeframe: "15m", TradeSizingMode: core.TradeSizingModePercent, TradeScaleFactor: 0.10, TradeSizeUSDC: 10.0, MinMarginPercent: 1.0, BinanceSignalThresholdPct: 0.05, PaperBinanceExecutionDelayMs: 250, PaperArbMode: "taker", CopytradePollIntervalMs: 2000, CopytradeSizingMode: core.CopytradeSizingModeUSDC, CopytradeSizeUSDC: 10.0, CopytradeSizeShares: 10.0, CopytradeSizePercent: 100.0, CopytradeMaxSlippagePct: 1.0, LadderedTakerSizingMode: core.LadderedTakerSizingModeUSDC, LadderedTakerSizeUSDC: 10.0, LadderedTakerSizeShares: 10.0, LadderedTakerReentryMoveCents: 1.0, LadderedTakerMaxSlippagePct: 1.0, BuyExecutionMarginFloorPercent: -0.01, SplitMinMarginSell: 2.0, MakerMergeBufferSeconds: 30, MakerQuoteGap: 0.008, MakerInventoryTargetMult: 3.0, MakerInventoryCapMult: 5.0, MakerMinQuoteValue: 5.0, MinAskPrice: 0.10, MaxAskPrice: 0.90, TradingHoursMode: "weekdays trade only", TakerCloseMarket: false, TakerCloseMarketTime: 5, TakerCloseMarketSlippage: 0.99, TakerCloseMarketMinPrice: 0.60}
)

const (
	settingsRowMarket = iota
	settingsRowMaxMarkets
	settingsRowPaperBalance
	settingsRowTimeframe
	settingsRowTradeSizingMode
	settingsRowTradeSizingValue
	settingsRowLadderCooldown
	settingsRowLadderSlippage
	settingsRowMinMargin
	settingsRowBinanceExecutionDelay
	settingsRowPaperArbMode
	settingsRowCopytradeTarget
	settingsRowCopytradePoll
	settingsRowExecutionSlip
	settingsRowSplitMinMargin
	settingsRowSplitStrategy
	settingsRowSplitInitialCap
	settingsRowSplitReplenishCap
	settingsRowTakerCloseMarket
	settingsRowBlockPendingRedemption
	settingsRowRedeemEntryTiming
	settingsRowRedeemGasMode
	settingsRowMinAskPrice
	settingsRowMaxAskPrice
	settingsRowMakerMergeBuffer
	settingsRowMakerQuoteGap
	settingsRowMakerTargetMult
	settingsRowMakerCapMult
	settingsRowMakerMinQuoteValue
	settingsRowMaxTradeSize
	settingsRowMaxDailyLoss
	settingsRowExchange
	settingsRowExecutionBackend
	settingsRowTakerCloseTime
	settingsRowTakerCloseSlippage
	settingsRowTakerCloseMinPrice
	settingsRowTradingHoursMode
	settingsRowRPCEdit
	settingsRowPrivateKeyEdit
	settingsRowCount
)

func (m tuiModel) toggleExchange() (tea.Model, tea.Cmd) {
	if m.tui.settings.Exchange == "polymarket" {
		m.tui.settings.Exchange = "kalshi"
		// Kalshi websockets require an API key even for market data.
		if os.Getenv("KALSHI_API_KEY") == "" {
			m.tui.eventLog = append(m.tui.eventLog, "⚠️ Kalshi keys missing. Please restart the app to configure.")
			if len(m.tui.eventLog) > m.tui.maxEvents {
				m.tui.eventLog = m.tui.eventLog[len(m.tui.eventLog)-m.tui.maxEvents:]
			}
		}
	} else {
		m.tui.settings.Exchange = "polymarket"
	}
	return m, nil
}

func isMakerSettingsMode(cfg TUISettings) bool {
	return strings.EqualFold(cfg.PaperArbMode, "maker")
}

func isCopytradeSettingsMode(cfg TUISettings) bool {
	return strings.EqualFold(cfg.PaperArbMode, "copytrade")
}

func isLadderedTakerSettingsMode(cfg TUISettings) bool {
	return strings.EqualFold(cfg.PaperArbMode, "laddered-taker")
}

func isLadderedTakerShareSizingMode(cfg TUISettings) bool {
	return strings.EqualFold(strings.TrimSpace(cfg.LadderedTakerSizingMode), core.LadderedTakerSizingModeShares)
}

func isBinanceGapSettingsMode(cfg TUISettings) bool {
	return strings.EqualFold(cfg.PaperArbMode, "binance-gap")
}

func TakerCloseModeActive(cfg TUISettings) bool {
	return cfg.TakerCloseMarket && !isMakerSettingsMode(cfg) && !isCopytradeSettingsMode(cfg) && !isBinanceGapSettingsMode(cfg) && !isLadderedTakerSettingsMode(cfg)
}

func realbotPaperBackendDisablesMaker(cfg TUISettings, mode string) bool {
	return strings.EqualFold(mode, "Real") && strings.EqualFold(cfg.ExecutionBackend, core.ExecutionBackendPaper)
}

func realbotPaperBackendDisablesSplit(cfg TUISettings, mode string) bool {
	return strings.EqualFold(mode, "Real") && strings.EqualFold(cfg.ExecutionBackend, core.ExecutionBackendPaper)
}

func usesPaperExecutionSemantics(cfg TUISettings, mode string) bool {
	return strings.EqualFold(mode, "Paper") || realbotPaperBackendDisablesMaker(cfg, mode)
}

func settingsArbModes(cfg TUISettings, mode string) []string {
	modes := []string{"taker", "laddered-taker", "binance-gap", "copytrade"}
	if !realbotPaperBackendDisablesMaker(cfg, mode) {
		modes = append(modes, "maker")
	}
	return modes
}

func normalizeTUISettingsForContext(s TUISettings, mode string) TUISettings {
	s = normalizeTUISettings(s)
	if realbotPaperBackendDisablesMaker(s, mode) && strings.EqualFold(s.PaperArbMode, "maker") {
		s.PaperArbMode = "taker"
	}
	if realbotPaperBackendDisablesSplit(s, mode) {
		s.SplitStrategyEnabled = false
	}
	return s
}

func isRowVisible(cfg TUISettings, mode string, idx int) bool {
	maker := isMakerSettingsMode(cfg)
	copytrade := isCopytradeSettingsMode(cfg)
	laddered := isLadderedTakerSettingsMode(cfg)
	binanceGap := isBinanceGapSettingsMode(cfg)
	kalshi := cfg.Exchange == "kalshi"
	closeMarket := TakerCloseModeActive(cfg)
	paperMode := strings.EqualFold(mode, "Paper") || strings.EqualFold(cfg.ExecutionBackend, core.ExecutionBackendPaper)

	if idx == settingsRowPaperBalance {
		return paperMode
	}
	if idx == settingsRowExecutionBackend {
		return !strings.EqualFold(mode, "Paper")
	}
	if idx == settingsRowRedeemEntryTiming {
		return laddered && cfg.BlockNewEntriesOnPendingRedemption
	}
	if idx == settingsRowRedeemGasMode {
		return strings.EqualFold(mode, "Real") &&
			!strings.EqualFold(cfg.ExecutionBackend, core.ExecutionBackendPaper) &&
			!kalshi
	}

	if kalshi {
		// Kalshi uses its own scheduling and does not support split inventory.
		switch idx {
		case settingsRowTimeframe, settingsRowSplitMinMargin, settingsRowSplitStrategy, settingsRowSplitInitialCap, settingsRowSplitReplenishCap, settingsRowCopytradeTarget, settingsRowCopytradePoll:
			return false
		}
	}

	if copytrade {
		switch idx {
		case settingsRowMinMargin, settingsRowSplitMinMargin, settingsRowSplitStrategy, settingsRowSplitInitialCap, settingsRowSplitReplenishCap, settingsRowTakerCloseMarket, settingsRowMinAskPrice, settingsRowMaxAskPrice, settingsRowMakerMergeBuffer, settingsRowMakerQuoteGap, settingsRowMakerTargetMult, settingsRowMakerCapMult, settingsRowMakerMinQuoteValue, settingsRowTakerCloseTime, settingsRowTakerCloseSlippage, settingsRowTakerCloseMinPrice:
			return false
		}
	}

	if laddered {
		switch idx {
		case settingsRowMinMargin, settingsRowExecutionSlip, settingsRowSplitMinMargin, settingsRowSplitStrategy, settingsRowSplitInitialCap, settingsRowSplitReplenishCap, settingsRowTakerCloseMarket:
			return false
		}
	}

	if binanceGap {
		switch idx {
		case settingsRowExecutionSlip, settingsRowSplitMinMargin, settingsRowSplitStrategy, settingsRowSplitInitialCap, settingsRowSplitReplenishCap, settingsRowTakerCloseMarket, settingsRowTakerCloseTime, settingsRowTakerCloseSlippage, settingsRowTakerCloseMinPrice, settingsRowCopytradeTarget, settingsRowCopytradePoll:
			return false
		}
	}

	if closeMarket && !maker {
		// Taker-close mode bypasses the normal split/panic-buy paths, so hide
		// controls that do not affect the dedicated close-market execution flow.
		switch idx {
		case settingsRowMinMargin, settingsRowSplitMinMargin, settingsRowSplitStrategy, settingsRowSplitInitialCap, settingsRowSplitReplenishCap, settingsRowMinAskPrice, settingsRowMaxAskPrice:
			return false
		}
	}

	switch idx {
	case settingsRowLadderCooldown:
		return laddered
	case settingsRowCopytradeTarget, settingsRowCopytradePoll:
		return copytrade
	case settingsRowBinanceExecutionDelay:
		return binanceGap && paperMode
	case settingsRowExecutionSlip:
		return !maker && !binanceGap
	case settingsRowSplitMinMargin, settingsRowSplitStrategy, settingsRowSplitInitialCap, settingsRowSplitReplenishCap:
		if realbotPaperBackendDisablesSplit(cfg, mode) {
			return false
		}
		return !maker && !binanceGap && !copytrade && !laddered
	case settingsRowMakerMergeBuffer, settingsRowMakerQuoteGap, settingsRowMakerTargetMult, settingsRowMakerCapMult, settingsRowMakerMinQuoteValue:
		return maker
	case settingsRowTakerCloseTime, settingsRowTakerCloseSlippage, settingsRowTakerCloseMinPrice:
		return closeMarket && !copytrade && !binanceGap
	default:
		return true
	}
}

func isStructuralSetting(idx int) bool {
	switch idx {
	case settingsRowMarket,
		settingsRowMaxMarkets,
		settingsRowTimeframe,
		settingsRowPaperArbMode,
		settingsRowExchange,
		settingsRowExecutionBackend:
		return true
	default:
		return false
	}
}

func settingsRequireRestart(prev, next TUISettings) bool {
	return !strings.EqualFold(strings.TrimSpace(prev.MarketSlug), strings.TrimSpace(next.MarketSlug)) ||
		prev.MaxMarkets != next.MaxMarkets ||
		!strings.EqualFold(normalizeMarketTimeframe(prev.Timeframe), normalizeMarketTimeframe(next.Timeframe)) ||
		!strings.EqualFold(strings.TrimSpace(prev.PaperArbMode), strings.TrimSpace(next.PaperArbMode)) ||
		!strings.EqualFold(strings.TrimSpace(prev.Exchange), strings.TrimSpace(next.Exchange)) ||
		!strings.EqualFold(strings.TrimSpace(prev.ExecutionBackend), strings.TrimSpace(next.ExecutionBackend))
}

func settingsRowEditable(cfg TUISettings, mode string, idx int) bool {
	return isRowVisible(cfg, mode, idx)
}

func ensureVisibleSettingsCursor(cfg TUISettings, mode string, cursor int) int {
	if settingsRowCount <= 0 {
		return 0
	}
	if cursor < 0 {
		cursor = 0
	}
	cursor = cursor % settingsRowCount
	if isRowVisible(cfg, mode, cursor) {
		return cursor
	}
	for i := 1; i < settingsRowCount; i++ {
		idx := (cursor + i) % settingsRowCount
		if isRowVisible(cfg, mode, idx) {
			return idx
		}
	}
	return 0
}

func settingsRowLabel(cfg TUISettings, idx int) string {
	maker := isMakerSettingsMode(cfg)
	copytrade := isCopytradeSettingsMode(cfg)
	laddered := isLadderedTakerSettingsMode(cfg)
	binanceGap := isBinanceGapSettingsMode(cfg)
	switch idx {
	case settingsRowPaperBalance:
		return "Paper Balance"
	case settingsRowExecutionBackend:
		return "Execution Backend"
	case settingsRowTradeSizingMode:
		if copytrade {
			return "Copy Size Mode"
		}
		if laddered {
			return "Ladder Size Mode"
		}
		return "Trade Size Mode"
	case settingsRowTradeSizingValue:
		if copytrade {
			if strings.EqualFold(cfg.CopytradeSizingMode, core.CopytradeSizingModeShares) {
				return "Copy Size (Shares)"
			}
			if strings.EqualFold(cfg.CopytradeSizingMode, core.CopytradeSizingModePercent) {
				return "Copy Size (% Master)"
			}
			return "Copy Size (USDC)"
		}
		if laddered {
			if isLadderedTakerShareSizingMode(cfg) {
				return "Ladder Size (Shares)"
			}
			return "Ladder Size (USDC)"
		}
		if strings.EqualFold(cfg.TradeSizingMode, core.TradeSizingModeUSDC) {
			return "Trade Size (USDC)"
		}
		return "Trade Scale Factor"
	case settingsRowLadderCooldown:
		return "Ladder Re-entry Move"
	case settingsRowLadderSlippage:
		return "Ladder Max Slippage %"
	case settingsRowMinMargin:
		if maker {
			return "Maker Min Sell Edge %"
		}
		if copytrade {
			return "Copy Margin"
		}
		if laddered {
			return "Ladder Min Margin %"
		}
		if binanceGap {
			return "Profit Target %"
		}
		return "Buy Min Margin %"
	case settingsRowBinanceExecutionDelay:
		return "Paper Exec Delay"
	case settingsRowExecutionSlip:
		if copytrade {
			return "Copy Max Slip"
		}
		return "Max Exec Slip %"
	case settingsRowCopytradeTarget:
		return "Copytrade Target"
	case settingsRowCopytradePoll:
		return "Copytrade Poll"
	case settingsRowSplitMinMargin:
		return "Split Min Margin"
	case settingsRowSplitStrategy:
		return "Split Strategy"
	case settingsRowSplitInitialCap:
		return "Split Initial Cap"
	case settingsRowSplitReplenishCap:
		return "Split Replenish Cap"
	case settingsRowTakerCloseMarket:
		return "Taker Close Market"
	case settingsRowBlockPendingRedemption:
		return "Wait Redeem Before Entry"
	case settingsRowRedeemEntryTiming:
		return "Redeem Re-entry Timing"
	case settingsRowRedeemGasMode:
		return "Redeem Gas Speed"
	case settingsRowMinAskPrice:
		if maker {
			return "Maker Min Buy Price"
		}
		return "Min Ask Price"
	case settingsRowMaxAskPrice:
		if maker {
			return "Maker Max Buy Price"
		}
		return "Max Ask Price"
	case settingsRowMakerMergeBuffer:
		return "Maker Merge Buffer"
	case settingsRowMakerQuoteGap:
		return "Maker Quote Gap"
	case settingsRowMakerTargetMult:
		return "Maker Target Mult"
	case settingsRowMakerCapMult:
		return "Maker Cap Mult"
	case settingsRowMakerMinQuoteValue:
		return "Maker Min Quote ($)"
	case settingsRowMaxTradeSize:
		return "Max Trade Size"
	case settingsRowMaxDailyLoss:
		return "Max Daily Loss"
	case settingsRowExchange:
		return "Exchange"
	case settingsRowTakerCloseTime:
		return "Taker Close Time"
	case settingsRowTakerCloseSlippage:
		return "Taker Close Slippage"
	case settingsRowTakerCloseMinPrice:
		return "Taker Close Min Price"
	case settingsRowTradingHoursMode:
		return "Trading Hours Mode"
	case settingsRowRPCEdit:
		return "RPC URL (Press Enter to edit)"
	case settingsRowPrivateKeyEdit:
		return "Private Key (Press Enter to edit)"
	default:
		return ""
	}
}

func normalizeMarketSelection(slug string) string {
	slug = strings.TrimSpace(slug)
	if slug == "" || strings.EqualFold(slug, "ALL") {
		return "ALL"
	}
	parts := strings.Split(slug, ",")
	seen := make(map[string]bool, len(parts))
	normalized := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.ToUpper(strings.TrimSpace(part))
		if part == "" || part == "ALL" || seen[part] {
			continue
		}
		seen[part] = true
		normalized = append(normalized, part)
	}
	if len(normalized) == 0 {
		return "ALL"
	}
	return strings.Join(normalized, ",")
}

func normalizeMarketTimeframe(timeframe string) string {
	switch strings.ToLower(strings.TrimSpace(timeframe)) {
	case "5m":
		return "5m"
	case "1h":
		return "1h"
	default:
		return "15m"
	}
}

func cycleMarketTimeframe(current string, delta int) string {
	return cycleString([]string{"15m", "5m", "1h"}, normalizeMarketTimeframe(current), delta)
}

func normalizeTUISettings(s TUISettings) TUISettings {
	s.MarketSlug = normalizeMarketSelection(s.MarketSlug)
	s.Timeframe = normalizeMarketTimeframe(s.Timeframe)
	if strings.EqualFold(strings.TrimSpace(s.ExecutionBackend), core.ExecutionBackendLive) {
		s.ExecutionBackend = core.ExecutionBackendLive
	} else {
		s.ExecutionBackend = core.ExecutionBackendPaper
	}
	if s.PaperBalance <= 0 {
		s.PaperBalance = 100.0
	}
	s.PaperBalance = math.Round(s.PaperBalance*100.0) / 100.0
	switch strings.ToLower(strings.TrimSpace(s.PaperArbMode)) {
	case "maker":
		s.PaperArbMode = "maker"
	case "copytrade":
		s.PaperArbMode = "copytrade"
	case "laddered-taker":
		s.PaperArbMode = "laddered-taker"
	case "binance-gap":
		s.PaperArbMode = "binance-gap"
	default:
		s.PaperArbMode = "taker"
	}
	if strings.EqualFold(strings.TrimSpace(s.TradeSizingMode), core.TradeSizingModeUSDC) {
		s.TradeSizingMode = core.TradeSizingModeUSDC
	} else {
		s.TradeSizingMode = core.TradeSizingModePercent
	}
	if s.TradeSizeUSDC <= 0 {
		s.TradeSizeUSDC = math.Round(math.Max(s.TradeScaleFactor*100.0, 0.1)*10.0) / 10.0
	}
	s.TradeSizeUSDC = math.Round(s.TradeSizeUSDC*10.0) / 10.0
	if s.TradeSizeUSDC < 0.1 {
		s.TradeSizeUSDC = 0.1
	}
	if s.TradeScaleFactor <= 0 {
		s.TradeScaleFactor = 0.01
	}
	if s.TradeScaleFactor > 1.0 {
		s.TradeScaleFactor = 1.0
	}
	if s.MaxMarkets < 1 {
		s.MaxMarkets = 1
	}
	if s.MarketSlug != "ALL" {
		selected := len(strings.Split(s.MarketSlug, ","))
		if selected > 0 && s.MaxMarkets > selected {
			s.MaxMarkets = selected
		}
	}
	s.BuyExecutionMarginFloorPercent = normalizeExecutionFloorSetting(s.BuyExecutionMarginFloorPercent)
	if s.CopytradeMaxSlippagePct > 99.0 {
		s.CopytradeMaxSlippagePct = 99.0
	}
	if s.CopytradeMaxSlippagePct < 0 {
		s.CopytradeMaxSlippagePct = 0
	}
	s.CopytradeMaxSlippagePct = math.Round(s.CopytradeMaxSlippagePct)
	s.TakerCloseMarketSlippage = normalizeTakerClosePriceSetting(s.TakerCloseMarketSlippage, 0.99)
	s.TakerCloseMarketMinPrice = normalizeTakerClosePriceSetting(s.TakerCloseMarketMinPrice, 0.60)
	s.CopytradeTarget = strings.TrimSpace(s.CopytradeTarget)
	if s.CopytradePollIntervalMs <= 0 {
		s.CopytradePollIntervalMs = 2000
	}
	if s.CopytradePollIntervalMs < 100 {
		s.CopytradePollIntervalMs = 100
	}
	if s.CopytradePollIntervalMs > 30000 {
		s.CopytradePollIntervalMs = 30000
	}
	switch strings.ToLower(strings.TrimSpace(s.CopytradeSizingMode)) {
	case core.CopytradeSizingModeShares:
		s.CopytradeSizingMode = core.CopytradeSizingModeShares
	case core.CopytradeSizingModePercent:
		s.CopytradeSizingMode = core.CopytradeSizingModePercent
	default:
		s.CopytradeSizingMode = core.CopytradeSizingModeUSDC
	}
	if s.CopytradeSizeUSDC <= 0 {
		s.CopytradeSizeUSDC = math.Round(math.Max(s.TradeSizeUSDC, 0.1)*10.0) / 10.0
	}
	s.CopytradeSizeUSDC = math.Round(s.CopytradeSizeUSDC*10.0) / 10.0
	if s.CopytradeSizeUSDC < 0.1 {
		s.CopytradeSizeUSDC = 0.1
	}
	if s.CopytradeSizeShares <= 0 {
		s.CopytradeSizeShares = 1.0
	}
	s.CopytradeSizeShares = math.Round(s.CopytradeSizeShares*100.0) / 100.0
	if s.CopytradeSizeShares < 0.01 {
		s.CopytradeSizeShares = 0.01
	}
	if s.CopytradeSizePercent <= 0 {
		s.CopytradeSizePercent = 100.0
	}
	s.CopytradeSizePercent = math.Round(s.CopytradeSizePercent*10.0) / 10.0
	if s.CopytradeSizePercent < 0.1 {
		s.CopytradeSizePercent = 0.1
	}
	if s.CopytradeSizePercent > 100.0 {
		s.CopytradeSizePercent = 100.0
	}
	switch strings.ToLower(strings.TrimSpace(s.LadderedTakerSizingMode)) {
	case core.LadderedTakerSizingModeShares:
		s.LadderedTakerSizingMode = core.LadderedTakerSizingModeShares
	default:
		s.LadderedTakerSizingMode = core.LadderedTakerSizingModeUSDC
	}
	if s.LadderedTakerSizeUSDC <= 0 {
		s.LadderedTakerSizeUSDC = math.Round(math.Max(s.TradeSizeUSDC, 0.1)*10.0) / 10.0
	}
	s.LadderedTakerSizeUSDC = math.Round(s.LadderedTakerSizeUSDC*10.0) / 10.0
	if s.LadderedTakerSizeUSDC < 0.1 {
		s.LadderedTakerSizeUSDC = 0.1
	}
	if s.LadderedTakerSizeShares <= 0 {
		s.LadderedTakerSizeShares = 1.0
	}
	s.LadderedTakerSizeShares = math.Round(s.LadderedTakerSizeShares*100.0) / 100.0
	if s.LadderedTakerSizeShares < 0.01 {
		s.LadderedTakerSizeShares = 0.01
	}
	s.LadderedTakerReentryMoveCents = normalizeLadderedTakerReentryMoveCents(s.LadderedTakerReentryMoveCents)
	s.RedeemEntryTiming = normalizeRedeemEntryTiming(s.RedeemEntryTiming)
	s.RedeemGasMode = normalizeRedeemGasMode(s.RedeemGasMode)
	if s.LadderedTakerMaxSlippagePct < 0 {
		s.LadderedTakerMaxSlippagePct = 0
	} else if s.LadderedTakerMaxSlippagePct > 99.0 {
		s.LadderedTakerMaxSlippagePct = 99.0
	}
	s.LadderedTakerMaxSlippagePct = math.Round(s.LadderedTakerMaxSlippagePct)
	if s.BinanceSignalThresholdPct <= 0 {
		s.BinanceSignalThresholdPct = 0.02
	}
	s.BinanceSignalThresholdPct = math.Round(s.BinanceSignalThresholdPct*1000.0) / 1000.0
	if s.BinanceSignalThresholdPct < 0.005 {
		s.BinanceSignalThresholdPct = 0.005
	}
	if s.BinanceSignalThresholdPct > 5.0 {
		s.BinanceSignalThresholdPct = 5.0
	}
	s.PaperBinanceExecutionDelayMs = int(math.Round(float64(s.PaperBinanceExecutionDelayMs)/10.0) * 10.0)
	if s.PaperBinanceExecutionDelayMs < 0 {
		s.PaperBinanceExecutionDelayMs = 0
	}
	if s.PaperBinanceExecutionDelayMs > 5000 {
		s.PaperBinanceExecutionDelayMs = 5000
	}
	if s.TakerCloseMarketSlippage < s.TakerCloseMarketMinPrice {
		s.TakerCloseMarketSlippage = s.TakerCloseMarketMinPrice
	}
	return s
}

func cycleCopytradeSizingMode(mode string, delta int) string {
	modes := []string{
		core.CopytradeSizingModeUSDC,
		core.CopytradeSizingModeShares,
		core.CopytradeSizingModePercent,
	}
	current := normalizeTUISettings(TUISettings{CopytradeSizingMode: mode}).CopytradeSizingMode
	idx := 0
	for i, candidate := range modes {
		if current == candidate {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(modes)) % len(modes)
	return modes[idx]
}

func cycleLadderedTakerSizingMode(mode string, delta int) string {
	modes := []string{
		core.LadderedTakerSizingModeUSDC,
		core.LadderedTakerSizingModeShares,
	}
	current := normalizeTUISettings(TUISettings{LadderedTakerSizingMode: mode}).LadderedTakerSizingMode
	idx := 0
	for i, candidate := range modes {
		if current == candidate {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(modes)) % len(modes)
	return modes[idx]
}

func normalizeRedeemEntryTiming(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case core.RedeemEntryTimingImmediate:
		return core.RedeemEntryTimingImmediate
	default:
		return core.RedeemEntryTimingNextMarket
	}
}

func normalizeRedeemGasMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case core.RedeemGasModeNormal:
		return core.RedeemGasModeNormal
	case core.RedeemGasModeUrgent:
		return core.RedeemGasModeUrgent
	default:
		return core.RedeemGasModeFast
	}
}

func cycleRedeemEntryTiming(mode string, delta int) string {
	modes := []string{
		core.RedeemEntryTimingNextMarket,
		core.RedeemEntryTimingImmediate,
	}
	current := normalizeRedeemEntryTiming(mode)
	idx := 0
	for i, candidate := range modes {
		if current == candidate {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(modes)) % len(modes)
	return modes[idx]
}

func cycleRedeemGasMode(mode string, delta int) string {
	modes := []string{
		core.RedeemGasModeNormal,
		core.RedeemGasModeFast,
		core.RedeemGasModeUrgent,
	}
	current := normalizeRedeemGasMode(mode)
	idx := 0
	for i, candidate := range modes {
		if current == candidate {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(modes)) % len(modes)
	return modes[idx]
}

func normalizeTakerClosePriceSetting(v, fallback float64) float64 {
	if v <= 0 || v >= 1.0 {
		v = fallback
	}
	v = math.Round(v*100.0) / 100.0
	if v < 0.01 {
		return 0.01
	}
	if v > 0.99 {
		return 0.99
	}
	return v
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

func normalizeExecutionFloorSetting(v float64) float64 {
	// Support both legacy percent form (-1.0 == -1%) and decimal form
	// (-0.01 == -1%), but keep the runtime/UI value in decimal slippage form.
	if math.Abs(v) > 0.10 {
		v = v / 100.0
	}
	if v > 0 {
		v = 0
	}
	if v < -0.10 {
		v = -0.10
	}
	return v
}

func executionFloorDisplayPercent(v float64) float64 {
	return normalizeExecutionFloorSetting(v) * 100.0
}

func normalizeCopytradeTargetInput(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	return raw
}

func renderCopytradeTargetValue(raw string, editing bool, buffer string) string {
	target := normalizeCopytradeTargetInput(raw)
	if editing {
		value := normalizeCopytradeTargetInput(buffer)
		if value == "" {
			value = "paste wallet / @handle / profile URL"
		}
		return styleCyan.Render(" " + value + " _ ")
	}
	if target == "" {
		return styleMuted.Render(" paste target ")
	}
	if len(target) > 28 {
		target = target[:25] + "..."
	}
	return styleCyan.Render(" " + target + " ")
}

func renderStringValue(raw string, editing bool, buffer string, placeholder string) string {
	target := strings.TrimSpace(raw)
	if editing {
		value := strings.TrimSpace(buffer)
		if value == "" {
			value = placeholder
		}
		return styleCyan.Render(" " + value + " _ ")
	}
	if target == "" {
		return styleMuted.Render(" " + placeholder + " ")
	}
	if len(target) > 28 {
		target = target[:25] + "..."
	}
	return styleCyan.Render(" " + target + " ")
}

func fmtFloatTrim(v float64, decimals int) string {
	s := strconv.FormatFloat(v, 'f', decimals, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" {
		return "0"
	}
	return s
}

func formatDisplayShareQty(v float64) string {
	if math.Abs(v-math.Round(v)) < 1e-9 {
		return fmt.Sprintf("%.0f", v)
	}
	return fmtFloatTrim(v, 5)
}

func formatSignedDisplayShareQty(v float64) string {
	switch {
	case v > 0:
		return "+" + formatDisplayShareQty(v)
	case v < 0:
		return "-" + formatDisplayShareQty(math.Abs(v))
	default:
		return "0"
	}
}

func displayedTradeBudgetsWithMode(mode string, cash, equity, startingBalance, sizingBalance, tradeFactor, tradeSizeUSDC, maxTradeSize, multiplier float64, tradeSizingMode string) (base, effective float64) {
	sizingCapital := equity
	if strings.EqualFold(mode, "Real") || strings.EqualFold(mode, "Live") {
		sizingCapital = equity
		if sizingCapital <= 0 {
			sizingCapital = math.Max(cash, startingBalance)
		}
		if cash > sizingCapital {
			sizingCapital = cash
		}
	}

	base = core.CalculateTradeSizeForMode(sizingCapital, tradeFactor, tradeSizeUSDC, maxTradeSize, tradeSizingMode)
	if base <= 0 {
		return 0, 0
	}

	effective = base
	if strings.EqualFold(mode, "Paper") && multiplier > 1.0 && !strings.EqualFold(tradeSizingMode, core.TradeSizingModeUSDC) {
		effective = base * multiplier
	}
	return base, effective
}

// ─── TUI struct ───────────────────────────────────────────────────────────────

// TUI provides a live bubbletea-driven terminal user interface.
type TUI struct {
	mu sync.Mutex

	engine     *Engine
	orderBook  *OrderBook
	orderBooks map[string]*OrderBook

	markets            map[string]*MarketData
	marketSlug         string
	outcomes           []string
	endTime            time.Time
	lastPrices         map[string]float64
	lastBids           map[string]float64
	lastAsks           map[string]float64
	realBids           map[string]float64
	realAsks           map[string]float64
	pendingOrders      map[string][]PendingOrder
	orderBookDepth     map[string]map[string][]MarketLevel
	eventLog           []string
	maxEvents          int
	orderHistory       []OrderHistoryEntry
	maxOrderHistory    int
	roundHistory       []RoundHistoryEntry
	maxRoundHistory    int
	isKilled           bool
	killReason         string
	tradeFactor        float64
	startTime          time.Time
	width              int
	height             int
	mode               string // "Paper" or "Real" - for footer display
	manualTradingPause bool

	restLatency    time.Duration
	restLatencyAvg time.Duration
	restSamples    []time.Duration
	wsLatency      time.Duration
	wsPingLatency  time.Duration
	latencySource  string

	splitInventories []*SplitInventory
	walletTruth      map[string][]WalletTruthPosition
	walletCash       float64
	hasWalletCash    bool

	// Runtime-adjustable settings (readable by the trading loop via GetSettings)
	settings         TUISettings
	onSettingsChange func(TUISettings)
	restartReq       bool

	amendedPnLForNextRound float64

	program     *tea.Program
	issueLogger *core.CSVLogger

	lastDedupLogAt      map[string]time.Time
	lastDedupLogMsg     map[string]string
	lastDisplayDirtyAt  time.Time
	displayDirtyPending bool
	lastEventBody       string
	lastEventAt         time.Time

	snapshotVersion uint64
}

// GetAndClearRestart returns true if a restart was requested via settings and clears the flag
func (t *TUI) GetAndClearRestart() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	req := t.restartReq
	t.restartReq = false
	return req
}

// ─── Bubbletea internals ──────────────────────────────────────────────────────

type tuiSnapshot struct {
	version            uint64
	markets            map[string]*MarketData
	marketSlug         string
	outcomes           []string
	endTime            time.Time
	lastPrices         map[string]float64
	lastBids           map[string]float64
	lastAsks           map[string]float64
	realBids           map[string]float64
	realAsks           map[string]float64
	pendingOrders      map[string][]PendingOrder
	orderBookDepth     map[string]map[string][]MarketLevel
	eventLog           []string
	orderHistory       []OrderHistoryEntry
	roundHistory       []RoundHistoryEntry
	isKilled           bool
	killReason         string
	tradeFactor        float64
	maxTradeSize       float64
	settings           TUISettings
	startTime          time.Time
	width              int
	height             int
	mode               string
	manualTradingPause bool
	restLatency        time.Duration
	restLatencyAvg     time.Duration
	wsLatency          time.Duration
	wsPingLatency      time.Duration
	latencySource      string
	splitPositions     []SplitPosition
	walletTruth        []WalletTruthPosition
	walletCash         float64
	hasWalletCash      bool

	stats           Stats
	exposure        float64
	maxExposure     float64
	equity          float64
	bookEquity      float64
	positions       map[string]PositionPnL
	orders          []ScopedLimitOrder
	multiplier      float64
	sizingBalance   float64
	rounds          int
	profitable      int
	losingRounds    int
	enginePositions map[string]Position
}

func (t *TUI) markDirtyLocked() {
	t.snapshotVersion++
}

func (t *TUI) markDisplayDirtyLocked(now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	if t.lastDisplayDirtyAt.IsZero() || now.Sub(t.lastDisplayDirtyAt) >= tuiDisplayDirtyMinInterval {
		t.snapshotVersion++
		t.lastDisplayDirtyAt = now
		t.displayDirtyPending = false
		return
	}
	t.displayDirtyPending = true
}

func (t *TUI) SetWalletCash(balance float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if balance < 0 {
		balance = 0
	}
	if t.hasWalletCash && math.Abs(t.walletCash-balance) < 0.000001 {
		return
	}
	t.walletCash = balance
	t.hasWalletCash = true
	t.markDirtyLocked()
}

func (t *TUI) ClearWalletCash() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.hasWalletCash && math.Abs(t.walletCash) < 0.000001 {
		return
	}
	t.walletCash = 0
	t.hasWalletCash = false
	t.markDirtyLocked()
}

type tuiModel struct {
	tui      *TUI
	interval time.Duration
	snap     tuiSnapshot
	// Quit callback — called before tea.Quit so parent context is cancelled first
	onQuit func()
	// Settings overlay state (immediate, not snapshotted)
	showSettings        bool
	settingsCursor      int // 0=TradeScale, 1=MinMargin, 2=SplitMargin, 3=SplitEnabled
	settingsEdit        bool
	settingsInput       string
	settingsBackup      TUISettings
	scrollOffset        int
	contentLines        int
	layoutVersion       uint64
	layoutWidth         int
	layoutHeight        int
	layoutSettings      bool
	mainContentVersion  uint64
	mainContentWidth    int
	mainContentSettings bool
	mainContentCached   string
}

const (
	tuiDisplayDirtyMinInterval   = 250 * time.Millisecond
	tuiConsecutiveLogDedupWindow = 750 * time.Millisecond
	tuiQuoteDisplayMinInterval   = 150 * time.Millisecond
	tuiDepthDisplayMinInterval   = 500 * time.Millisecond
	tuiQuoteDisplayMinMove       = 0.01
)

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

func settingsEditValue(cfg TUISettings, row int) string {
	switch row {
	case settingsRowCopytradeTarget:
		return cfg.CopytradeTarget
	case settingsRowPaperBalance:
		return fmt.Sprintf("%.2f", cfg.PaperBalance)
	case settingsRowTradeSizingValue:
		if isCopytradeSettingsMode(cfg) {
			if strings.EqualFold(cfg.CopytradeSizingMode, core.CopytradeSizingModeShares) {
				return fmt.Sprintf("%.2f", cfg.CopytradeSizeShares)
			}
			if strings.EqualFold(cfg.CopytradeSizingMode, core.CopytradeSizingModePercent) {
				return fmt.Sprintf("%.2f", cfg.CopytradeSizePercent)
			}
			return fmt.Sprintf("%.2f", cfg.CopytradeSizeUSDC)
		}
		if isLadderedTakerSettingsMode(cfg) {
			if isLadderedTakerShareSizingMode(cfg) {
				return fmt.Sprintf("%.2f", cfg.LadderedTakerSizeShares)
			}
			return fmt.Sprintf("%.2f", cfg.LadderedTakerSizeUSDC)
		}
		if strings.EqualFold(cfg.TradeSizingMode, core.TradeSizingModeUSDC) {
			return fmt.Sprintf("%.2f", cfg.TradeSizeUSDC)
		}
		return fmt.Sprintf("%.3f", cfg.TradeScaleFactor)
	case settingsRowExecutionSlip:
		if isCopytradeSettingsMode(cfg) {
			return fmt.Sprintf("%.0f", cfg.CopytradeMaxSlippagePct)
		}
		return fmt.Sprintf("%.1f", executionFloorDisplayPercent(cfg.BuyExecutionMarginFloorPercent))
	case settingsRowLadderSlippage:
		return fmt.Sprintf("%.0f", cfg.LadderedTakerMaxSlippagePct)
	case settingsRowMinAskPrice:
		return fmt.Sprintf("%.2f", cfg.MinAskPrice)
	case settingsRowMaxAskPrice:
		return fmt.Sprintf("%.2f", cfg.MaxAskPrice)
	case settingsRowTakerCloseSlippage:
		return fmt.Sprintf("%.2f", cfg.TakerCloseMarketSlippage)
	case settingsRowTakerCloseMinPrice:
		return fmt.Sprintf("%.2f", cfg.TakerCloseMarketMinPrice)
	case settingsRowRPCEdit:
		return cfg.PolygonRPC
	case settingsRowPrivateKeyEdit:
		return cfg.PolygonPrivateKey
	default:
		return ""
	}
}

func settingsRowSupportsTypedEdit(cfg TUISettings, mode string, row int) bool {
	if !settingsRowEditable(cfg, mode, row) {
		return false
	}
	switch row {
	case settingsRowCopytradeTarget,
		settingsRowPaperBalance,
		settingsRowTradeSizingValue,
		settingsRowExecutionSlip,
		settingsRowLadderSlippage,
		settingsRowMinAskPrice,
		settingsRowMaxAskPrice,
		settingsRowTakerCloseSlippage,
		settingsRowTakerCloseMinPrice,
		settingsRowRPCEdit,
		settingsRowPrivateKeyEdit,
		settingsRowLadderCooldown,
		settingsRowMinMargin,
		settingsRowBinanceExecutionDelay,
		settingsRowCopytradePoll,
		settingsRowSplitMinMargin,
		settingsRowSplitInitialCap,
		settingsRowSplitReplenishCap,
		settingsRowMakerMergeBuffer,
		settingsRowMakerQuoteGap,
		settingsRowMakerTargetMult,
		settingsRowMakerCapMult,
		settingsRowMakerMinQuoteValue,
		settingsRowMaxTradeSize,
		settingsRowMaxDailyLoss:
		return true
	default:
		return false
	}
}

func settingsKeyStartsTypedEdit(msg tea.KeyMsg) bool {
	if len(msg.Runes) != 1 {
		return false
	}
	r := msg.Runes[0]
	return (r >= '0' && r <= '9') || r == '.' || r == '-'
}

func applySettingsEditValue(cfg *TUISettings, row int, input string) bool {
	if cfg == nil {
		return false
	}
	input = strings.TrimSpace(input)
	switch row {
	case settingsRowCopytradeTarget:
		value := normalizeCopytradeTargetInput(input)
		if normalizeCopytradeTargetInput(cfg.CopytradeTarget) == value {
			return false
		}
		cfg.CopytradeTarget = value
		return true
	case settingsRowRPCEdit:
		if cfg.PolygonRPC == input {
			return false
		}
		cfg.PolygonRPC = input
		return true
	case settingsRowPrivateKeyEdit:
		if cfg.PolygonPrivateKey == input {
			return false
		}
		cfg.PolygonPrivateKey = input
		return true
	}

	value, err := strconv.ParseFloat(input, 64)
	if err != nil {
		return false
	}

	switch row {
	case settingsRowPaperBalance:
		if value <= 0 || cfg.PaperBalance == value {
			return false
		}
		cfg.PaperBalance = value
		return true
	case settingsRowTradeSizingValue:
		if value <= 0 {
			return false
		}
		if isCopytradeSettingsMode(*cfg) {
			if strings.EqualFold(cfg.CopytradeSizingMode, core.CopytradeSizingModeShares) {
				if cfg.CopytradeSizeShares == value {
					return false
				}
				cfg.CopytradeSizeShares = value
				return true
			}
			if strings.EqualFold(cfg.CopytradeSizingMode, core.CopytradeSizingModePercent) {
				if cfg.CopytradeSizePercent == value {
					return false
				}
				cfg.CopytradeSizePercent = value
				return true
			}
			if cfg.CopytradeSizeUSDC == value {
				return false
			}
			cfg.CopytradeSizeUSDC = value
			return true
		}
		if isLadderedTakerSettingsMode(*cfg) {
			if isLadderedTakerShareSizingMode(*cfg) {
				if cfg.LadderedTakerSizeShares == value {
					return false
				}
				cfg.LadderedTakerSizeShares = value
				return true
			}
			if cfg.LadderedTakerSizeUSDC == value {
				return false
			}
			cfg.LadderedTakerSizeUSDC = value
			return true
		}
		if strings.EqualFold(cfg.TradeSizingMode, core.TradeSizingModeUSDC) {
			if cfg.TradeSizeUSDC == value {
				return false
			}
			cfg.TradeSizeUSDC = value
			return true
		}
		if cfg.TradeScaleFactor == value {
			return false
		}
		cfg.TradeScaleFactor = value
		return true
	case settingsRowLadderCooldown:
		if cfg.LadderedTakerReentryMoveCents == value {
			return false
		}
		cfg.LadderedTakerReentryMoveCents = value
		return true
	case settingsRowMinMargin:
		if cfg.MinMarginPercent == value {
			return false
		}
		cfg.MinMarginPercent = value
		return true
	case settingsRowBinanceExecutionDelay:
		if float64(cfg.PaperBinanceExecutionDelayMs) == value {
			return false
		}
		cfg.PaperBinanceExecutionDelayMs = int(value)
		return true
	case settingsRowCopytradePoll:
		if float64(cfg.CopytradePollIntervalMs) == value {
			return false
		}
		cfg.CopytradePollIntervalMs = int(value)
		return true
	case settingsRowSplitMinMargin:
		if cfg.SplitMinMarginSell == value {
			return false
		}
		cfg.SplitMinMarginSell = value
		return true
	case settingsRowSplitInitialCap:
		if cfg.SplitInitialCapPct == value {
			return false
		}
		cfg.SplitInitialCapPct = value
		return true
	case settingsRowSplitReplenishCap:
		if cfg.SplitReplenishCapPct == value {
			return false
		}
		cfg.SplitReplenishCapPct = value
		return true
	case settingsRowMakerMergeBuffer:
		if float64(cfg.MakerMergeBufferSeconds) == value {
			return false
		}
		cfg.MakerMergeBufferSeconds = int(value)
		return true
	case settingsRowMakerQuoteGap:
		if cfg.MakerQuoteGap == value {
			return false
		}
		cfg.MakerQuoteGap = value
		return true
	case settingsRowMakerTargetMult:
		if cfg.MakerInventoryTargetMult == value {
			return false
		}
		cfg.MakerInventoryTargetMult = value
		return true
	case settingsRowMakerCapMult:
		if cfg.MakerInventoryCapMult == value {
			return false
		}
		cfg.MakerInventoryCapMult = value
		return true
	case settingsRowMakerMinQuoteValue:
		if cfg.MakerMinQuoteValue == value {
			return false
		}
		cfg.MakerMinQuoteValue = value
		return true
	case settingsRowMaxTradeSize:
		if cfg.MaxTradeSize == value {
			return false
		}
		cfg.MaxTradeSize = value
		return true
	case settingsRowMaxDailyLoss:
		if cfg.MaxDailyLoss == value {
			return false
		}
		cfg.MaxDailyLoss = value
		return true
	case settingsRowExecutionSlip:
		if isCopytradeSettingsMode(*cfg) {
			if cfg.CopytradeMaxSlippagePct == value {
				return false
			}
			cfg.CopytradeMaxSlippagePct = value
			return true
		}
		value = value / 100.0
		if cfg.BuyExecutionMarginFloorPercent == value {
			return false
		}
		cfg.BuyExecutionMarginFloorPercent = value
		return true
	case settingsRowLadderSlippage:
		if cfg.LadderedTakerMaxSlippagePct == value {
			return false
		}
		cfg.LadderedTakerMaxSlippagePct = value
		return true
	case settingsRowMinAskPrice:
		if value <= 0 || cfg.MinAskPrice == value {
			return false
		}
		cfg.MinAskPrice = value
		return true
	case settingsRowMaxAskPrice:
		if value <= 0 || cfg.MaxAskPrice == value {
			return false
		}
		cfg.MaxAskPrice = value
		return true
	case settingsRowTakerCloseSlippage:
		if value <= 0 || cfg.TakerCloseMarketSlippage == value {
			return false
		}
		cfg.TakerCloseMarketSlippage = value
		return true
	case settingsRowTakerCloseMinPrice:
		if value <= 0 || cfg.TakerCloseMarketMinPrice == value {
			return false
		}
		cfg.TakerCloseMarketMinPrice = value
		return true
	default:
		return false
	}
}

func walletTruthOutcomeKey(outcome string) string {
	return strings.ToLower(strings.TrimSpace(outcome))
}

type tickMsg time.Time

func tickCmd(interval time.Duration) tea.Cmd {
	return tea.Tick(interval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m tuiModel) Init() tea.Cmd {
	return tickCmd(m.interval)
}

func normalizeTUIWidth(w int) int {
	if w < 60 {
		return 80
	}
	return w
}

func (m tuiModel) bodyViewportHeight() int {
	h := m.snap.height
	if h <= 1 {
		return 20
	}
	return h - 1
}

func (m *tuiModel) maxScrollOffset() int {
	maxOffset := m.contentLines - m.bodyViewportHeight()
	if maxOffset < 0 {
		return 0
	}
	return maxOffset
}

func settingsContentLineCount(cfg TUISettings, mode string) int {
	rows := 0
	for i := 0; i < settingsRowCount; i++ {
		if isRowVisible(cfg, mode, i) {
			rows++
		}
	}
	lines := 3 + rows + 8 + 2
	if isMakerSettingsMode(cfg) || isCopytradeSettingsMode(cfg) || isLadderedTakerSettingsMode(cfg) {
		lines++
	}
	if strings.EqualFold(mode, "Paper") || strings.EqualFold(cfg.ExecutionBackend, core.ExecutionBackendPaper) {
		lines++
	}
	return lines
}

func (m *tuiModel) settingsMaxScrollOffset() int {
	m.tui.mu.Lock()
	cfg := m.tui.settings
	mode := m.tui.mode
	m.tui.mu.Unlock()
	maxOffset := settingsContentLineCount(cfg, mode) - m.settingsViewportHeight()
	if maxOffset < 0 {
		return 0
	}
	return maxOffset
}

func (m *tuiModel) clampScrollOffset() {
	if m.scrollOffset < 0 {
		m.scrollOffset = 0
	}
	maxOffset := m.maxScrollOffset()
	if m.showSettings {
		maxOffset = m.settingsMaxScrollOffset()
	}
	if m.scrollOffset > maxOffset {
		m.scrollOffset = maxOffset
	}
}

func (m *tuiModel) scrollBy(delta int) {
	m.scrollOffset += delta
	m.clampScrollOffset()
}

func (m *tuiModel) scrollTo(offset int) {
	m.scrollOffset = offset
	m.clampScrollOffset()
}

func viewportLines(lines []string, offset, height int) ([]string, int, int) {
	if height < 1 {
		height = 1
	}
	maxOffset := len(lines) - height
	if maxOffset < 0 {
		maxOffset = 0
	}
	if offset < 0 {
		offset = 0
	}
	if offset > maxOffset {
		offset = maxOffset
	}
	end := offset + height
	if end > len(lines) {
		end = len(lines)
	}
	visible := append([]string(nil), lines[offset:end]...)
	for len(visible) < height {
		visible = append(visible, "")
	}
	return visible, offset, maxOffset
}

func visibleSettingsRowOrdinal(cfg TUISettings, mode string, cursor int) int {
	ordinal := 0
	for i := 0; i < settingsRowCount; i++ {
		if !isRowVisible(cfg, mode, i) {
			continue
		}
		if i == cursor {
			return ordinal
		}
		ordinal++
	}
	return 0
}

func (m *tuiModel) settingsViewportHeight() int {
	if m.snap.height <= 1 {
		return 1000
	}
	height := m.bodyViewportHeight() - 2
	if height < 6 {
		height = 6
	}
	return height
}

func (m *tuiModel) ensureSettingsCursorVisible(cfg TUISettings, mode string) {
	cursorLine := 3 + visibleSettingsRowOrdinal(cfg, mode, m.settingsCursor)
	height := m.settingsViewportHeight()
	if cursorLine < m.scrollOffset {
		m.scrollOffset = cursorLine
	}
	if cursorLine >= m.scrollOffset+height {
		m.scrollOffset = cursorLine - height + 1
	}
	if m.scrollOffset < 0 {
		m.scrollOffset = 0
	}
}

func (m tuiModel) buildMainContent(w int) string {
	if m.showSettings {
		return m.renderSettings(w)
	}

	s := m.snap
	var rows []string

	rows = append(rows, m.renderHeader(w))
	rows = append(rows, "")

	if w > 100 {
		leftW := (w - 2) / 2
		rightW := w - leftW - 2

		var leftRows []string
		leftRows = append(leftRows, m.renderMarketInfo(leftW))
		leftRows = append(leftRows, m.renderAccountStatus(leftW, s.stats, s.exposure, s.maxExposure, s.equity, s.bookEquity, s.multiplier, s.sizingBalance, s.rounds, s.profitable, s.losingRounds, s.enginePositions))
		leftRows = append(leftRows, m.renderPositions(leftW, s.positions))
		if ord := m.renderOrders(leftW, s.orders); ord != "" {
			leftRows = append(leftRows, ord)
		}

		var rightRows []string
		rightRows = append(rightRows, m.renderRoundHistory(rightW, m.roundHistoryRows(true)))
		rightRows = append(rightRows, m.renderOrderHistory(rightW, m.orderHistoryRows(true)))
		rightRows = append(rightRows, m.renderEventLog(rightW, m.eventLogRows(true)))

		leftCol := strings.Join(leftRows, "\n\n")
		rightCol := strings.Join(rightRows, "\n\n")

		content := lipgloss.JoinHorizontal(lipgloss.Top, leftCol, "  ", rightCol)
		rows = append(rows, content)
	} else {
		rows = append(rows, m.renderMarketInfo(w))
		rows = append(rows, m.renderAccountStatus(w, s.stats, s.exposure, s.maxExposure, s.equity, s.bookEquity,
			s.multiplier, s.sizingBalance, s.rounds, s.profitable, s.losingRounds, s.enginePositions))
		rows = append(rows, "")
		rows = append(rows, m.renderPositions(w, s.positions))

		if ord := m.renderOrders(w, s.orders); ord != "" {
			rows = append(rows, ord)
		}

		rows = append(rows, m.renderRoundHistory(w, m.roundHistoryRows(false)))
		rows = append(rows, m.renderOrderHistory(w, m.orderHistoryRows(false)))
		rows = append(rows, "")
		rows = append(rows, m.renderEventLog(w, m.eventLogRows(false)))
	}

	if s.isKilled {
		rows = append(rows, "")
		rows = append(rows, m.renderKillBanner(w))
	}

	return strings.Join(rows, "\n")
}

func (m *tuiModel) renderMainContent(w int) string {
	if m.mainContentCached != "" &&
		m.mainContentVersion == m.snap.version &&
		m.mainContentWidth == w &&
		m.mainContentSettings == m.showSettings {
		return m.mainContentCached
	}

	content := m.buildMainContent(w)
	m.mainContentCached = content
	m.mainContentVersion = m.snap.version
	m.mainContentWidth = w
	m.mainContentSettings = m.showSettings
	return content
}

func (m *tuiModel) refreshScrollMetrics() {
	if m.showSettings {
		m.contentLines = 0
		m.scrollOffset = 0
		m.layoutVersion = m.snap.version
		m.layoutWidth = m.snap.width
		m.layoutHeight = m.snap.height
		m.layoutSettings = true
		return
	}
	w := normalizeTUIWidth(m.snap.width)
	content := m.renderMainContent(w)
	m.contentLines = lipgloss.Height(content)
	m.clampScrollOffset()
	m.layoutVersion = m.snap.version
	m.layoutWidth = m.snap.width
	m.layoutHeight = m.snap.height
	m.layoutSettings = false
}

func (m *tuiModel) refreshScrollMetricsIfNeeded() {
	if m.layoutVersion != m.snap.version || m.layoutWidth != m.snap.width || m.layoutHeight != m.snap.height || m.layoutSettings != m.showSettings {
		m.refreshScrollMetrics()
	}
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.tui.mu.Lock()
		m.tui.width = msg.Width
		m.tui.height = msg.Height
		m.tui.mu.Unlock()

		// Update the snapshot immediately so the next View() call is perfectly sized
		m.snap.width = msg.Width
		m.snap.height = msg.Height
		m.refreshScrollMetrics()

		// Clear screen on resize to prevent rendering artifacts
		return m, tea.ClearScreen

	case tickMsg:
		_ = msg

		stats := m.tui.engine.GetStats()
		exposure, _ := m.tui.engine.GetExposure()
		equity := m.tui.engine.GetEquity()
		bookEquity := m.tui.engine.GetBookEquity()
		positions := m.tui.engine.GetPositionsWithPnL()
		orders := m.tui.getOpenOrdersSnapshot()
		multiplier, rounds, profitable, losingRounds, sizingBalance := m.tui.engine.GetCompoundStats()
		enginePositions := m.tui.engine.GetPositions()
		splitPositions := m.tui.getSplitPositions()
		walletTruth := m.tui.getWalletTruthPositions()

		m.tui.mu.Lock()
		if m.tui.displayDirtyPending {
			m.tui.snapshotVersion++
			m.tui.lastDisplayDirtyAt = time.Now()
			m.tui.displayDirtyPending = false
		}

		if m.snap.markets == nil || m.snap.version != m.tui.snapshotVersion {
			// Rebuild the expensive collections only when the underlying TUI data changed.
			snapMarkets := make(map[string]*MarketData)
			for k, v := range m.tui.markets {
				md := &MarketData{
					Slug:            v.Slug,
					Outcomes:        append([]string(nil), v.Outcomes...),
					EndTime:         v.EndTime,
					InventoryStatus: v.InventoryStatus,
					Bids:            make(map[string]float64),
					Asks:            make(map[string]float64),
					ClearedBids:     make(map[string]bool),
					ClearedAsks:     make(map[string]bool),
					RealBids:        make(map[string]float64),
					RealAsks:        make(map[string]float64),
					LastUpdate:      v.LastUpdate,
					LastDepthUpdate: v.LastDepthUpdate,
					DataSource:      v.DataSource,
					BinanceSignal:   v.BinanceSignal,
				}
				for outcome, price := range v.Bids {
					md.Bids[outcome] = price
				}
				for outcome, price := range v.Asks {
					md.Asks[outcome] = price
				}
				for outcome, cleared := range v.ClearedBids {
					md.ClearedBids[outcome] = cleared
				}
				for outcome, cleared := range v.ClearedAsks {
					md.ClearedAsks[outcome] = cleared
				}
				for outcome, price := range v.RealBids {
					md.RealBids[outcome] = price
				}
				for outcome, price := range v.RealAsks {
					md.RealAsks[outcome] = price
				}
				snapMarkets[k] = md
			}

			snapLastPrices := make(map[string]float64)
			for k, v := range m.tui.lastPrices {
				snapLastPrices[k] = v
			}
			snapLastBids := make(map[string]float64)
			for k, v := range m.tui.lastBids {
				snapLastBids[k] = v
			}
			snapLastAsks := make(map[string]float64)
			for k, v := range m.tui.lastAsks {
				snapLastAsks[k] = v
			}
			snapRealBids := make(map[string]float64)
			for k, v := range m.tui.realBids {
				snapRealBids[k] = v
			}
			snapRealAsks := make(map[string]float64)
			for k, v := range m.tui.realAsks {
				snapRealAsks[k] = v
			}

			snapPendingOrders := make(map[string][]PendingOrder)
			for k, v := range m.tui.pendingOrders {
				snapPendingOrders[k] = append([]PendingOrder(nil), v...)
			}

			snapOrderBookDepth := make(map[string]map[string][]MarketLevel)
			for mk, mv := range m.tui.orderBookDepth {
				inner := make(map[string][]MarketLevel)
				for ok, ov := range mv {
					inner[ok] = append([]MarketLevel(nil), ov...)
				}
				snapOrderBookDepth[mk] = inner
			}

			m.snap.version = m.tui.snapshotVersion
			m.snap.markets = snapMarkets
			m.snap.marketSlug = m.tui.marketSlug
			m.snap.outcomes = append([]string(nil), m.tui.outcomes...)
			m.snap.endTime = m.tui.endTime
			m.snap.lastPrices = snapLastPrices
			m.snap.lastBids = snapLastBids
			m.snap.lastAsks = snapLastAsks
			m.snap.realBids = snapRealBids
			m.snap.realAsks = snapRealAsks
			m.snap.pendingOrders = snapPendingOrders
			m.snap.orderBookDepth = snapOrderBookDepth
			m.snap.eventLog = append([]string(nil), m.tui.eventLog...)
			m.snap.orderHistory = append([]OrderHistoryEntry(nil), m.tui.orderHistory...)
			m.snap.roundHistory = append([]RoundHistoryEntry(nil), m.tui.roundHistory...)
		}

		m.snap.isKilled = m.tui.isKilled
		m.snap.killReason = m.tui.killReason
		m.snap.tradeFactor = m.tui.tradeFactor
		m.snap.maxTradeSize = m.tui.settings.MaxTradeSize
		m.snap.settings = m.tui.settings
		m.snap.startTime = m.tui.startTime
		m.snap.width = m.tui.width
		m.snap.height = m.tui.height
		m.snap.mode = m.tui.mode
		m.snap.manualTradingPause = m.tui.manualTradingPause
		m.snap.restLatency = m.tui.restLatency
		m.snap.restLatencyAvg = m.tui.restLatencyAvg
		m.snap.wsLatency = m.tui.wsLatency
		m.snap.wsPingLatency = m.tui.wsPingLatency
		m.snap.latencySource = m.tui.latencySource
		m.snap.splitPositions = splitPositions
		m.snap.walletTruth = walletTruth
		m.snap.walletCash = m.tui.walletCash
		m.snap.hasWalletCash = m.tui.hasWalletCash
		m.snap.stats = stats
		m.snap.exposure = exposure
		m.snap.maxExposure = stats.PeakExposure
		m.snap.equity = equity
		m.snap.bookEquity = bookEquity
		m.snap.positions = positions
		m.snap.orders = orders
		m.snap.multiplier = multiplier
		m.snap.sizingBalance = sizingBalance
		m.snap.rounds = rounds
		m.snap.profitable = profitable
		m.snap.losingRounds = losingRounds
		m.snap.enginePositions = enginePositions
		m.tui.mu.Unlock()
		m.refreshScrollMetricsIfNeeded()

		return m, tickCmd(m.interval)

	case tea.KeyMsg:
		key := msg.String()

		// ── Settings overlay key handling ────────────────────────────────────
		if m.showSettings {
			if m.settingsEdit {
				switch key {
				case "esc":
					m.settingsEdit = false
					m.settingsInput = ""
					return m, nil
				case "enter":
					m.tui.mu.Lock()
					changed := false
					var notifySettingsChange func()
					prevPaperBalance := m.tui.settings.PaperBalance
					changed = applySettingsEditValue(&m.tui.settings, m.settingsCursor, m.settingsInput)
					if changed {
						m.tui.settings = normalizeTUISettingsForContext(m.tui.settings, m.tui.mode)
						if math.Abs(m.tui.settings.PaperBalance-prevPaperBalance) >= 0.005 {
							if err := m.tui.applyPaperBalanceLocked(m.tui.settings.PaperBalance); err != nil {
								m.tui.settings.PaperBalance = prevPaperBalance
								m.tui.appendEventLocked(fmt.Sprintf("⚠️ Paper balance change requires a flat book: %v", err))
							} else {
								m.tui.appendEventLocked(fmt.Sprintf("💼 Paper balance reset to $%.2f", m.tui.settings.PaperBalance))
							}
						}
						notifySettingsChange = m.tui.settingsChangeHookLocked()
					}
					m.tui.mu.Unlock()
					if notifySettingsChange != nil {
						notifySettingsChange()
					}
					m.settingsEdit = false
					m.settingsInput = ""
					return m, nil
				case "backspace", "ctrl+h":
					runes := []rune(m.settingsInput)
					if len(runes) > 0 {
						m.settingsInput = string(runes[:len(runes)-1])
					}
					return m, nil
				case "ctrl+u":
					m.settingsInput = ""
					return m, nil
				}
				if len(msg.Runes) > 0 {
					m.settingsInput += string(msg.Runes)
				}
				return m, nil
			}

			if settingsRowSupportsTypedEdit(m.tui.settings, m.tui.mode, m.settingsCursor) && settingsKeyStartsTypedEdit(msg) {
				m.settingsEdit = true
				m.settingsInput = string(msg.Runes)
				return m, nil
			}

			switch key {
			case "p", "P":
				m.snap.manualTradingPause = m.tui.ToggleTradingPause()
				return m, nil
			case "s", "S", "r", "R":
				m.showSettings = false
				m.refreshScrollMetrics()
				m.tui.mu.Lock()
				if settingsRequireRestart(m.settingsBackup, m.tui.settings) {
					m.tui.restartReq = true
				}
				m.tui.mu.Unlock()
				return m, nil
			case "enter":
				if settingsRowSupportsTypedEdit(m.tui.settings, m.tui.mode, m.settingsCursor) {
					m.tui.mu.Lock()
					m.settingsInput = settingsEditValue(m.tui.settings, m.settingsCursor)
					m.tui.mu.Unlock()
					m.settingsEdit = true
				}
				return m, nil
			case "esc":
				m.showSettings = false
				m.refreshScrollMetrics()
				m.tui.mu.Lock()
				m.tui.settings = m.settingsBackup
				m.tui.tradeFactor = m.tui.settings.TradeScaleFactor
				notify := m.tui.settingsChangeHookLocked()
				m.tui.mu.Unlock()
				if notify != nil {
					notify()
				}
				return m, nil
			case "up", "k":
				for {
					m.settingsCursor--
					if m.settingsCursor < 0 {
						m.settingsCursor = settingsRowCount - 1
					}
					if isRowVisible(m.tui.settings, m.tui.mode, m.settingsCursor) {
						break
					}
				}
				m.ensureSettingsCursorVisible(m.tui.settings, m.tui.mode)
				return m, nil
			case "down", "j":
				for {
					m.settingsCursor = (m.settingsCursor + 1) % settingsRowCount
					if isRowVisible(m.tui.settings, m.tui.mode, m.settingsCursor) {
						break
					}
				}
				m.ensureSettingsCursorVisible(m.tui.settings, m.tui.mode)
				return m, nil
			case "pgup", "ctrl+b":
				m.scrollBy(-(m.settingsViewportHeight() - 2))
				return m, nil
			case "pgdown", "ctrl+f":
				m.scrollBy(m.settingsViewportHeight() - 2)
				return m, nil
			case "left", "-", "h":
				m.tui.mu.Lock()
				changed := false
				var notifySettingsChange func()
				prevPaperBalance := m.tui.settings.PaperBalance
				if !settingsRowEditable(m.tui.settings, m.tui.mode, m.settingsCursor) {
					m.tui.mu.Unlock()
					return m, nil
				}
				switch m.settingsCursor {
				case settingsRowMarket:
					markets := []string{"ALL", "BTC", "ETH", "SOL", "XRP", "BTC,ETH", "SOL,XRP", "BTC,ETH,SOL"}
					idx := 0
					for i, mkt := range markets {
						if strings.EqualFold(m.tui.settings.MarketSlug, mkt) {
							idx = i
							break
						}
					}
					idx--
					if idx < 0 {
						idx = len(markets) - 1
					}
					m.tui.settings.MarketSlug = markets[idx]
					changed = true
				case settingsRowMaxMarkets:
					m.tui.settings.MaxMarkets--
					if m.tui.settings.MaxMarkets < 1 {
						m.tui.settings.MaxMarkets = 1
					}
					changed = true
				case settingsRowPaperBalance:
					m.tui.settings.PaperBalance -= 10.0
					if m.tui.settings.PaperBalance < 10.0 {
						m.tui.settings.PaperBalance = 10.0
					}
					changed = true
				case settingsRowTimeframe:
					m.tui.settings.Timeframe = cycleMarketTimeframe(m.tui.settings.Timeframe, -1)
					changed = true
				case settingsRowTradeSizingMode:
					if isCopytradeSettingsMode(m.tui.settings) {
						m.tui.settings.CopytradeSizingMode = cycleCopytradeSizingMode(m.tui.settings.CopytradeSizingMode, -1)
					} else if isLadderedTakerSettingsMode(m.tui.settings) {
						m.tui.settings.LadderedTakerSizingMode = cycleLadderedTakerSizingMode(m.tui.settings.LadderedTakerSizingMode, -1)
					} else {
						if strings.EqualFold(m.tui.settings.TradeSizingMode, core.TradeSizingModeUSDC) {
							m.tui.settings.TradeSizingMode = core.TradeSizingModePercent
						} else {
							m.tui.settings.TradeSizingMode = core.TradeSizingModeUSDC
						}
					}
					changed = true
				case settingsRowTradeSizingValue:
					if isCopytradeSettingsMode(m.tui.settings) {
						if strings.EqualFold(m.tui.settings.CopytradeSizingMode, core.CopytradeSizingModeShares) {
							m.tui.settings.CopytradeSizeShares -= 0.25
							if m.tui.settings.CopytradeSizeShares < 0.01 {
								m.tui.settings.CopytradeSizeShares = 0.01
							}
						} else if strings.EqualFold(m.tui.settings.CopytradeSizingMode, core.CopytradeSizingModePercent) {
							m.tui.settings.CopytradeSizePercent -= 1.0
							if m.tui.settings.CopytradeSizePercent < 0.1 {
								m.tui.settings.CopytradeSizePercent = 0.1
							}
						} else {
							m.tui.settings.CopytradeSizeUSDC -= 0.1
							if m.tui.settings.CopytradeSizeUSDC < 0.1 {
								m.tui.settings.CopytradeSizeUSDC = 0.1
							}
						}
					} else if isLadderedTakerSettingsMode(m.tui.settings) {
						if isLadderedTakerShareSizingMode(m.tui.settings) {
							m.tui.settings.LadderedTakerSizeShares -= 0.25
							if m.tui.settings.LadderedTakerSizeShares < 0.01 {
								m.tui.settings.LadderedTakerSizeShares = 0.01
							}
						} else {
							m.tui.settings.LadderedTakerSizeUSDC -= 0.1
							if m.tui.settings.LadderedTakerSizeUSDC < 0.1 {
								m.tui.settings.LadderedTakerSizeUSDC = 0.1
							}
						}
					} else {
						if strings.EqualFold(m.tui.settings.TradeSizingMode, core.TradeSizingModeUSDC) {
							m.tui.settings.TradeSizeUSDC -= 0.1
							if m.tui.settings.TradeSizeUSDC < 0.1 {
								m.tui.settings.TradeSizeUSDC = 0.1
							}
						} else {
							m.tui.settings.TradeScaleFactor -= 0.01
							if m.tui.settings.TradeScaleFactor < 0.01 {
								m.tui.settings.TradeScaleFactor = 0.01
							}
						}
					}
					changed = true
				case settingsRowLadderCooldown:
					m.tui.settings.LadderedTakerReentryMoveCents -= 1.0
					if m.tui.settings.LadderedTakerReentryMoveCents < 1.0 {
						m.tui.settings.LadderedTakerReentryMoveCents = 1.0
					}
					changed = true
				case settingsRowLadderSlippage:
					m.tui.settings.LadderedTakerMaxSlippagePct -= 1.0
					if m.tui.settings.LadderedTakerMaxSlippagePct < 0 {
						m.tui.settings.LadderedTakerMaxSlippagePct = 0
					}
					changed = true
				case settingsRowMinMargin:
					m.tui.settings.MinMarginPercent -= 0.5
					if m.tui.settings.MinMarginPercent < 0.5 {
						m.tui.settings.MinMarginPercent = 0.5
					}
					changed = true
				case settingsRowBinanceExecutionDelay:
					m.tui.settings.PaperBinanceExecutionDelayMs -= 10
					if m.tui.settings.PaperBinanceExecutionDelayMs < 0 {
						m.tui.settings.PaperBinanceExecutionDelayMs = 0
					}
					changed = true
				case settingsRowPaperArbMode:
					m.tui.settings.PaperArbMode = cycleString(settingsArbModes(m.tui.settings, m.tui.mode), m.tui.settings.PaperArbMode, -1)
					changed = true
				case settingsRowCopytradeTarget:
					// Use Enter to edit this free-form field.
				case settingsRowCopytradePoll:
					m.tui.settings.CopytradePollIntervalMs -= 100
					if m.tui.settings.CopytradePollIntervalMs < 100 {
						m.tui.settings.CopytradePollIntervalMs = 100
					}
					changed = true
				case settingsRowExecutionSlip:
					if isCopytradeSettingsMode(m.tui.settings) {
						m.tui.settings.CopytradeMaxSlippagePct -= 1.0
						if m.tui.settings.CopytradeMaxSlippagePct < 0 {
							m.tui.settings.CopytradeMaxSlippagePct = 0
						}
					} else {
						m.tui.settings.BuyExecutionMarginFloorPercent -= 0.01
						if m.tui.settings.BuyExecutionMarginFloorPercent < -0.10 {
							m.tui.settings.BuyExecutionMarginFloorPercent = -0.10
						}
					}
					changed = true
				case settingsRowSplitMinMargin:
					m.tui.settings.SplitMinMarginSell -= 0.5
					if m.tui.settings.SplitMinMarginSell < 1.0 {
						m.tui.settings.SplitMinMarginSell = 1.0
					}
					changed = true
				case settingsRowSplitStrategy:
					m.tui.settings.SplitStrategyEnabled = false
					changed = true
				case settingsRowSplitInitialCap:
					m.tui.settings.SplitInitialCapPct -= 0.05
					if m.tui.settings.SplitInitialCapPct < 0.05 {
						m.tui.settings.SplitInitialCapPct = 0.05
					}
					changed = true
				case settingsRowSplitReplenishCap:
					m.tui.settings.SplitReplenishCapPct -= 0.05
					if m.tui.settings.SplitReplenishCapPct < 0.05 {
						m.tui.settings.SplitReplenishCapPct = 0.05
					}
					changed = true
				case settingsRowTakerCloseMarket:
					m.tui.settings.TakerCloseMarket = !m.tui.settings.TakerCloseMarket
					changed = true
				case settingsRowBlockPendingRedemption:
					m.tui.settings.BlockNewEntriesOnPendingRedemption = !m.tui.settings.BlockNewEntriesOnPendingRedemption
					changed = true
				case settingsRowRedeemEntryTiming:
					m.tui.settings.RedeemEntryTiming = cycleRedeemEntryTiming(m.tui.settings.RedeemEntryTiming, -1)
					changed = true
				case settingsRowRedeemGasMode:
					m.tui.settings.RedeemGasMode = cycleRedeemGasMode(m.tui.settings.RedeemGasMode, -1)
					changed = true
				case settingsRowMinAskPrice:
					m.tui.settings.MinAskPrice -= 0.01
					if m.tui.settings.MinAskPrice < 0.01 {
						m.tui.settings.MinAskPrice = 0.01
					}
					changed = true
				case settingsRowMaxAskPrice:
					m.tui.settings.MaxAskPrice -= 0.01
					if m.tui.settings.MaxAskPrice < 0.01 {
						m.tui.settings.MaxAskPrice = 0.01
					}
					changed = true
				case settingsRowMakerMergeBuffer:
					m.tui.settings.MakerMergeBufferSeconds -= 5
					if m.tui.settings.MakerMergeBufferSeconds < 5 {
						m.tui.settings.MakerMergeBufferSeconds = 5
					}
					changed = true
				case settingsRowMakerQuoteGap:
					m.tui.settings.MakerQuoteGap -= 0.001
					if m.tui.settings.MakerQuoteGap < 0.001 {
						m.tui.settings.MakerQuoteGap = 0.001
					}
					changed = true
				case settingsRowMakerTargetMult:
					m.tui.settings.MakerInventoryTargetMult -= 0.5
					if m.tui.settings.MakerInventoryTargetMult < 1.0 {
						m.tui.settings.MakerInventoryTargetMult = 1.0
					}
					changed = true
				case settingsRowMakerCapMult:
					m.tui.settings.MakerInventoryCapMult -= 0.5
					if m.tui.settings.MakerInventoryCapMult < 1.0 {
						m.tui.settings.MakerInventoryCapMult = 1.0
					}
					changed = true
				case settingsRowMakerMinQuoteValue:
					m.tui.settings.MakerMinQuoteValue -= 1.0
					if m.tui.settings.MakerMinQuoteValue < 1.0 {
						m.tui.settings.MakerMinQuoteValue = 1.0
					}
					changed = true
				case settingsRowMaxTradeSize:
					m.tui.settings.MaxTradeSize -= 5.0
					if m.tui.settings.MaxTradeSize < 0.0 {
						m.tui.settings.MaxTradeSize = 0.0
					}
					changed = true
				case settingsRowMaxDailyLoss:
					m.tui.settings.MaxDailyLoss -= 5.0
					if m.tui.settings.MaxDailyLoss < 0.0 {
						m.tui.settings.MaxDailyLoss = 0.0
					}
					changed = true
				case settingsRowExchange:
					newM, cmd := m.toggleExchange()
					if cmd != nil {
						m.tui.mu.Unlock()
						return newM, cmd
					}
					changed = true
				case settingsRowExecutionBackend:
					m.tui.settings.ExecutionBackend = cycleString([]string{core.ExecutionBackendPaper, core.ExecutionBackendLive}, m.tui.settings.ExecutionBackend, -1)
					changed = true
				case settingsRowTakerCloseTime:
					m.tui.settings.TakerCloseMarketTime -= 1
					if m.tui.settings.TakerCloseMarketTime < 1 {
						m.tui.settings.TakerCloseMarketTime = 1
					}
					changed = true
				case settingsRowTakerCloseSlippage:
					m.tui.settings.TakerCloseMarketSlippage -= 0.01
					if m.tui.settings.TakerCloseMarketSlippage < 0.50 {
						m.tui.settings.TakerCloseMarketSlippage = 0.50
					}
					changed = true
				case settingsRowTakerCloseMinPrice:
					m.tui.settings.TakerCloseMarketMinPrice -= 0.01
					if m.tui.settings.TakerCloseMarketMinPrice < 0.01 {
						m.tui.settings.TakerCloseMarketMinPrice = 0.01
					}
					changed = true
				case settingsRowTradingHoursMode:
					if m.tui.settings.TradingHoursMode == "off" {
						m.tui.settings.TradingHoursMode = "weekdays trade only"
					} else if m.tui.settings.TradingHoursMode == "weekdays trade only" {
						m.tui.settings.TradingHoursMode = "us open only"
					} else {
						m.tui.settings.TradingHoursMode = "off"
					}
					changed = true
				}
				if changed {
					m.tui.settings = normalizeTUISettingsForContext(m.tui.settings, m.tui.mode)
					if math.Abs(m.tui.settings.PaperBalance-prevPaperBalance) >= 0.005 {
						if err := m.tui.applyPaperBalanceLocked(m.tui.settings.PaperBalance); err != nil {
							m.tui.settings.PaperBalance = prevPaperBalance
							m.tui.appendEventLocked(fmt.Sprintf("⚠️ Paper balance change requires a flat book: %v", err))
						} else {
							m.tui.appendEventLocked(fmt.Sprintf("💼 Paper balance reset to $%.2f", m.tui.settings.PaperBalance))
						}
					}
				}
				m.settingsCursor = ensureVisibleSettingsCursor(m.tui.settings, m.tui.mode, m.settingsCursor)
				m.ensureSettingsCursorVisible(m.tui.settings, m.tui.mode)
				m.tui.tradeFactor = m.tui.settings.TradeScaleFactor
				if changed {
					notifySettingsChange = m.tui.settingsChangeHookLocked()
				}
				m.tui.mu.Unlock()
				if notifySettingsChange != nil {
					notifySettingsChange()
				}
				return m, nil
			case "right", "+", "l":
				m.tui.mu.Lock()
				changed := false
				var notifySettingsChange func()
				prevPaperBalance := m.tui.settings.PaperBalance
				if !settingsRowEditable(m.tui.settings, m.tui.mode, m.settingsCursor) {
					m.tui.mu.Unlock()
					return m, nil
				}
				switch m.settingsCursor {
				case settingsRowMarket:
					markets := []string{"ALL", "BTC", "ETH", "SOL", "XRP", "BTC,ETH", "SOL,XRP", "BTC,ETH,SOL"}
					idx := 0
					for i, mkt := range markets {
						if strings.EqualFold(m.tui.settings.MarketSlug, mkt) {
							idx = i
							break
						}
					}
					idx = (idx + 1) % len(markets)
					m.tui.settings.MarketSlug = markets[idx]
					changed = true
				case settingsRowMaxMarkets:
					m.tui.settings.MaxMarkets++
					if m.tui.settings.MaxMarkets > 20 {
						m.tui.settings.MaxMarkets = 20
					}
					changed = true
				case settingsRowPaperBalance:
					m.tui.settings.PaperBalance += 10.0
					changed = true
				case settingsRowTimeframe:
					m.tui.settings.Timeframe = cycleMarketTimeframe(m.tui.settings.Timeframe, 1)
					changed = true
				case settingsRowTradeSizingMode:
					if isCopytradeSettingsMode(m.tui.settings) {
						m.tui.settings.CopytradeSizingMode = cycleCopytradeSizingMode(m.tui.settings.CopytradeSizingMode, 1)
					} else if isLadderedTakerSettingsMode(m.tui.settings) {
						m.tui.settings.LadderedTakerSizingMode = cycleLadderedTakerSizingMode(m.tui.settings.LadderedTakerSizingMode, 1)
					} else {
						if strings.EqualFold(m.tui.settings.TradeSizingMode, core.TradeSizingModeUSDC) {
							m.tui.settings.TradeSizingMode = core.TradeSizingModePercent
						} else {
							m.tui.settings.TradeSizingMode = core.TradeSizingModeUSDC
						}
					}
					changed = true
				case settingsRowTradeSizingValue:
					if isCopytradeSettingsMode(m.tui.settings) {
						if strings.EqualFold(m.tui.settings.CopytradeSizingMode, core.CopytradeSizingModeShares) {
							m.tui.settings.CopytradeSizeShares += 0.25
						} else if strings.EqualFold(m.tui.settings.CopytradeSizingMode, core.CopytradeSizingModePercent) {
							m.tui.settings.CopytradeSizePercent += 1.0
							if m.tui.settings.CopytradeSizePercent > 100.0 {
								m.tui.settings.CopytradeSizePercent = 100.0
							}
						} else {
							m.tui.settings.CopytradeSizeUSDC += 0.1
						}
					} else if isLadderedTakerSettingsMode(m.tui.settings) {
						if isLadderedTakerShareSizingMode(m.tui.settings) {
							m.tui.settings.LadderedTakerSizeShares += 0.25
						} else {
							m.tui.settings.LadderedTakerSizeUSDC += 0.1
						}
					} else {
						if strings.EqualFold(m.tui.settings.TradeSizingMode, core.TradeSizingModeUSDC) {
							m.tui.settings.TradeSizeUSDC += 0.1
						} else {
							m.tui.settings.TradeScaleFactor += 0.01
							if m.tui.settings.TradeScaleFactor > 1.0 {
								m.tui.settings.TradeScaleFactor = 1.0
							}
						}
					}
					changed = true
				case settingsRowLadderCooldown:
					m.tui.settings.LadderedTakerReentryMoveCents += 1.0
					if m.tui.settings.LadderedTakerReentryMoveCents > 25.0 {
						m.tui.settings.LadderedTakerReentryMoveCents = 25.0
					}
					changed = true
				case settingsRowLadderSlippage:
					m.tui.settings.LadderedTakerMaxSlippagePct += 1.0
					if m.tui.settings.LadderedTakerMaxSlippagePct > 99.0 {
						m.tui.settings.LadderedTakerMaxSlippagePct = 99.0
					}
					changed = true
				case settingsRowMinMargin:
					m.tui.settings.MinMarginPercent += 0.5
					if m.tui.settings.MinMarginPercent > 20.0 {
						m.tui.settings.MinMarginPercent = 20.0
					}
					changed = true
				case settingsRowBinanceExecutionDelay:
					m.tui.settings.PaperBinanceExecutionDelayMs += 10
					if m.tui.settings.PaperBinanceExecutionDelayMs > 5000 {
						m.tui.settings.PaperBinanceExecutionDelayMs = 5000
					}
					changed = true
				case settingsRowPaperArbMode:
					m.tui.settings.PaperArbMode = cycleString(settingsArbModes(m.tui.settings, m.tui.mode), m.tui.settings.PaperArbMode, 1)
					changed = true
				case settingsRowCopytradeTarget:
					// Use Enter to edit this free-form field.
				case settingsRowCopytradePoll:
					m.tui.settings.CopytradePollIntervalMs += 100
					if m.tui.settings.CopytradePollIntervalMs > 30000 {
						m.tui.settings.CopytradePollIntervalMs = 30000
					}
					changed = true
				case settingsRowExecutionSlip:
					if isCopytradeSettingsMode(m.tui.settings) {
						m.tui.settings.CopytradeMaxSlippagePct += 1.0
						if m.tui.settings.CopytradeMaxSlippagePct > 99.0 {
							m.tui.settings.CopytradeMaxSlippagePct = 99.0
						}
					} else {
						m.tui.settings.BuyExecutionMarginFloorPercent += 0.01
						if m.tui.settings.BuyExecutionMarginFloorPercent > 0.0 {
							m.tui.settings.BuyExecutionMarginFloorPercent = 0.0
						}
					}
					changed = true
				case settingsRowSplitMinMargin:
					m.tui.settings.SplitMinMarginSell += 0.5
					if m.tui.settings.SplitMinMarginSell > 20.0 {
						m.tui.settings.SplitMinMarginSell = 20.0
					}
					changed = true
				case settingsRowSplitStrategy:
					m.tui.settings.SplitStrategyEnabled = true
					changed = true
				case settingsRowSplitInitialCap:
					m.tui.settings.SplitInitialCapPct += 0.05
					if m.tui.settings.SplitInitialCapPct > 1.0 {
						m.tui.settings.SplitInitialCapPct = 1.0
					}
					changed = true
				case settingsRowSplitReplenishCap:
					m.tui.settings.SplitReplenishCapPct += 0.05
					if m.tui.settings.SplitReplenishCapPct > 1.0 {
						m.tui.settings.SplitReplenishCapPct = 1.0
					}
					changed = true
				case settingsRowTakerCloseMarket:
					m.tui.settings.TakerCloseMarket = !m.tui.settings.TakerCloseMarket
					changed = true
				case settingsRowBlockPendingRedemption:
					m.tui.settings.BlockNewEntriesOnPendingRedemption = !m.tui.settings.BlockNewEntriesOnPendingRedemption
					changed = true
				case settingsRowRedeemEntryTiming:
					m.tui.settings.RedeemEntryTiming = cycleRedeemEntryTiming(m.tui.settings.RedeemEntryTiming, 1)
					changed = true
				case settingsRowRedeemGasMode:
					m.tui.settings.RedeemGasMode = cycleRedeemGasMode(m.tui.settings.RedeemGasMode, 1)
					changed = true
				case settingsRowMinAskPrice:
					m.tui.settings.MinAskPrice += 0.01
					if m.tui.settings.MinAskPrice > 0.99 {
						m.tui.settings.MinAskPrice = 0.99
					}
					changed = true
				case settingsRowMaxAskPrice:
					m.tui.settings.MaxAskPrice += 0.01
					if m.tui.settings.MaxAskPrice > 0.99 {
						m.tui.settings.MaxAskPrice = 0.99
					}
					changed = true
				case settingsRowMakerMergeBuffer:
					m.tui.settings.MakerMergeBufferSeconds += 5
					if m.tui.settings.MakerMergeBufferSeconds > 300 {
						m.tui.settings.MakerMergeBufferSeconds = 300
					}
					changed = true
				case settingsRowMakerQuoteGap:
					m.tui.settings.MakerQuoteGap += 0.001
					if m.tui.settings.MakerQuoteGap > 0.100 {
						m.tui.settings.MakerQuoteGap = 0.100
					}
					changed = true
				case settingsRowMakerTargetMult:
					m.tui.settings.MakerInventoryTargetMult += 0.5
					if m.tui.settings.MakerInventoryTargetMult > 20.0 {
						m.tui.settings.MakerInventoryTargetMult = 20.0
					}
					changed = true
				case settingsRowMakerCapMult:
					m.tui.settings.MakerInventoryCapMult += 0.5
					if m.tui.settings.MakerInventoryCapMult > 50.0 {
						m.tui.settings.MakerInventoryCapMult = 50.0
					}
					changed = true
				case settingsRowMakerMinQuoteValue:
					m.tui.settings.MakerMinQuoteValue += 1.0
					if m.tui.settings.MakerMinQuoteValue > 500.0 {
						m.tui.settings.MakerMinQuoteValue = 500.0
					}
					changed = true
				case settingsRowMaxTradeSize:
					m.tui.settings.MaxTradeSize += 5.0
					changed = true
				case settingsRowMaxDailyLoss:
					m.tui.settings.MaxDailyLoss += 5.0
					changed = true
				case settingsRowExchange:
					newM, cmd := m.toggleExchange()
					if cmd != nil {
						m.tui.mu.Unlock()
						return newM, cmd
					}
					changed = true
				case settingsRowExecutionBackend:
					m.tui.settings.ExecutionBackend = cycleString([]string{core.ExecutionBackendPaper, core.ExecutionBackendLive}, m.tui.settings.ExecutionBackend, 1)
					changed = true
				case settingsRowTakerCloseTime:
					m.tui.settings.TakerCloseMarketTime += 1
					if m.tui.settings.TakerCloseMarketTime > 60 {
						m.tui.settings.TakerCloseMarketTime = 60
					}
					changed = true
				case settingsRowTakerCloseSlippage:
					m.tui.settings.TakerCloseMarketSlippage += 0.01
					if m.tui.settings.TakerCloseMarketSlippage > 0.99 {
						m.tui.settings.TakerCloseMarketSlippage = 0.99
					}
					changed = true
				case settingsRowTakerCloseMinPrice:
					m.tui.settings.TakerCloseMarketMinPrice += 0.01
					if m.tui.settings.TakerCloseMarketMinPrice > 0.99 {
						m.tui.settings.TakerCloseMarketMinPrice = 0.99
					}
					changed = true
				case settingsRowTradingHoursMode:
					if m.tui.settings.TradingHoursMode == "off" {
						m.tui.settings.TradingHoursMode = "weekdays trade only"
					} else if m.tui.settings.TradingHoursMode == "weekdays trade only" {
						m.tui.settings.TradingHoursMode = "us open only"
					} else {
						m.tui.settings.TradingHoursMode = "off"
					}
					changed = true
				}
				if changed {
					m.tui.settings = normalizeTUISettingsForContext(m.tui.settings, m.tui.mode)
					if math.Abs(m.tui.settings.PaperBalance-prevPaperBalance) >= 0.005 {
						if err := m.tui.applyPaperBalanceLocked(m.tui.settings.PaperBalance); err != nil {
							m.tui.settings.PaperBalance = prevPaperBalance
							m.tui.appendEventLocked(fmt.Sprintf("⚠️ Paper balance change requires a flat book: %v", err))
						} else {
							m.tui.appendEventLocked(fmt.Sprintf("💼 Paper balance reset to $%.2f", m.tui.settings.PaperBalance))
						}
					}
				}
				m.settingsCursor = ensureVisibleSettingsCursor(m.tui.settings, m.tui.mode, m.settingsCursor)
				m.ensureSettingsCursorVisible(m.tui.settings, m.tui.mode)
				m.tui.tradeFactor = m.tui.settings.TradeScaleFactor
				if changed {
					notifySettingsChange = m.tui.settingsChangeHookLocked()
				}
				m.tui.mu.Unlock()
				if notifySettingsChange != nil {
					notifySettingsChange()
				}
				return m, nil
			// Quick presets
			case "1":
				m.tui.mu.Lock()
				preset := SettingsConservative
				preset.PaperBalance = m.tui.settings.PaperBalance
				m.tui.settings = normalizeTUISettingsForContext(preset, m.tui.mode)
				m.tui.tradeFactor = m.tui.settings.TradeScaleFactor
				notifySettingsChange := m.tui.settingsChangeHookLocked()
				m.tui.mu.Unlock()
				if notifySettingsChange != nil {
					notifySettingsChange()
				}
				return m, nil
			case "2":
				m.tui.mu.Lock()
				preset := SettingsModerate
				preset.PaperBalance = m.tui.settings.PaperBalance
				m.tui.settings = normalizeTUISettingsForContext(preset, m.tui.mode)
				m.tui.tradeFactor = m.tui.settings.TradeScaleFactor
				notifySettingsChange := m.tui.settingsChangeHookLocked()
				m.tui.mu.Unlock()
				if notifySettingsChange != nil {
					notifySettingsChange()
				}
				return m, nil
			case "3":
				m.tui.mu.Lock()
				preset := SettingsAggressive
				preset.PaperBalance = m.tui.settings.PaperBalance
				m.tui.settings = normalizeTUISettingsForContext(preset, m.tui.mode)
				m.tui.tradeFactor = m.tui.settings.TradeScaleFactor
				notifySettingsChange := m.tui.settingsChangeHookLocked()
				m.tui.mu.Unlock()
				if notifySettingsChange != nil {
					notifySettingsChange()
				}
				return m, nil
			}
			return m, nil
		}

		// ── Normal key handling ──────────────────────────────────────────────
		switch key {
		case "p", "P":
			m.snap.manualTradingPause = m.tui.ToggleTradingPause()
			return m, nil
		case "up", "k":
			m.scrollBy(-1)
			return m, nil
		case "down", "j":
			m.scrollBy(1)
			return m, nil
		case "pgup", "b":
			m.scrollBy(-(m.bodyViewportHeight() - 2))
			return m, nil
		case "pgdown", "f", " ":
			m.scrollBy(m.bodyViewportHeight() - 2)
			return m, nil
		case "g", "home":
			m.scrollTo(0)
			return m, nil
		case "G", "end":
			m.scrollTo(m.maxScrollOffset())
			return m, nil
		case "s", "S":
			m.showSettings = true
			m.settingsEdit = false
			m.settingsInput = ""
			m.scrollOffset = 0
			m.tui.mu.Lock()
			m.settingsBackup = m.tui.settings
			m.tui.mu.Unlock()
			if !isRowVisible(m.tui.settings, m.tui.mode, m.settingsCursor) {
				m.settingsCursor = 0
				for m.settingsCursor < settingsRowCount-1 && !isRowVisible(m.tui.settings, m.tui.mode, m.settingsCursor) {
					m.settingsCursor++
				}
			}
			m.ensureSettingsCursorVisible(m.tui.settings, m.tui.mode)
			m.refreshScrollMetrics()
			return m, nil
		case "c", "C":
			m.tui.mu.Lock()
			m.tui.eventLog = []string{}
			m.tui.mu.Unlock()
			m.refreshScrollMetrics()
			return m, nil
		case "r", "R":
			m.tui.mu.Lock()
			m.tui.restartReq = true
			m.tui.mu.Unlock()
			return m, nil
		case "q", "Q", "ctrl+c":
			// Call the parent cancel func FIRST so the trading loop shuts down
			// immediately (fixes double Ctrl+C requirement).
			if m.onQuit != nil {
				m.onQuit()
			}
			return m, tea.Quit
		}

	case tea.MouseMsg:
		if tea.MouseEvent(msg).IsWheel() {
			switch msg.Button {
			case tea.MouseButtonWheelUp:
				m.scrollBy(-3)
				return m, nil
			case tea.MouseButtonWheelDown:
				m.scrollBy(3)
				return m, nil
			}
		}
	}
	return m, nil
}

// View composes all panels into the final frame. Pure and lock-free.
func (m tuiModel) View() string {
	s := m.snap
	w := normalizeTUIWidth(s.width)

	// Settings overlay: replace entire view while open.
	if m.showSettings {
		return m.renderSettings(w)
	}
	body := m.renderMainContent(w)
	lines := strings.Split(body, "\n")
	visibleHeight := m.bodyViewportHeight()
	visibleLines, effectiveOffset, maxOffset := viewportLines(lines, m.scrollOffset, visibleHeight)
	footer := m.renderFooter(w, effectiveOffset, maxOffset)
	return strings.Join(append(visibleLines, footer), "\n")
}

// ─── TUI Public API ───────────────────────────────────────────────────────────

func NewTUI(engine *Engine, orderBook *OrderBook) *TUI {
	return &TUI{
		engine:          engine,
		orderBook:       orderBook,
		orderBooks:      make(map[string]*OrderBook),
		markets:         make(map[string]*MarketData),
		lastPrices:      make(map[string]float64),
		lastBids:        make(map[string]float64),
		lastAsks:        make(map[string]float64),
		realBids:        make(map[string]float64),
		realAsks:        make(map[string]float64),
		pendingOrders:   make(map[string][]PendingOrder),
		orderBookDepth:  make(map[string]map[string][]MarketLevel),
		latencySource:   "London API",
		orderHistory:    make([]OrderHistoryEntry, 0),
		maxOrderHistory: defaultMaxOrderHistory,
		roundHistory:    make([]RoundHistoryEntry, 0),
		maxRoundHistory: defaultMaxRoundHistory,
		eventLog:        make([]string, 0),
		maxEvents:       defaultMaxEventHistory,
		walletTruth:     make(map[string][]WalletTruthPosition),
		lastDedupLogAt:  make(map[string]time.Time),
		lastDedupLogMsg: make(map[string]string),
		width:           80,
		height:          24,
		startTime:       time.Now(),
	}
}

// StartRenderLoop creates the bubbletea program and begins rendering.
// cancelFunc (optional) is called when the user presses q/Q/Ctrl+C so the
// parent context is cancelled immediately — fixing the double Ctrl+C requirement.
func (t *TUI) StartRenderLoop(interval time.Duration, cancelFuncs ...func()) {
	onQuit := func() {}
	if len(cancelFuncs) > 0 && cancelFuncs[0] != nil {
		onQuit = cancelFuncs[0]
	}
	model := tuiModel{tui: t, interval: interval, onQuit: onQuit}
	t.program = tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion())
	go func() {
		if _, err := t.program.Run(); err != nil {
			_ = err
		}
	}()
}

func (t *TUI) Stop() {
	if t.program != nil {
		t.program.Quit()
	}
}

// StopAndWait sends a quit signal and waits for the program to fully terminate
// before returning, ensuring terminal state is cleanly restored.
func (t *TUI) StopAndWait() {
	if t.program != nil {
		t.program.Quit()
		t.program.Wait()
		t.program = nil
	}
}

func (t *TUI) SetIssueLogger(logger *core.CSVLogger) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.issueLogger = logger
}

func (t *TUI) CloseIssueLogger() {
	t.mu.Lock()
	logger := t.issueLogger
	t.issueLogger = nil
	t.mu.Unlock()
	if logger != nil {
		logger.Close()
	}
}

func (t *TUI) UpdateLatency(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.restLatency = d
}

func (t *TUI) UpdateRestLatency(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.restLatency = d
	t.restSamples = append(t.restSamples, d)
	if len(t.restSamples) > 20 {
		t.restSamples = t.restSamples[1:]
	}
	var total time.Duration
	for _, s := range t.restSamples {
		total += s
	}
	t.restLatencyAvg = total / time.Duration(len(t.restSamples))
	t.latencySource = "REST /book"
}

func (t *TUI) UpdateWSLatency(timeSinceLastMsg time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.wsLatency = timeSinceLastMsg
}

func (t *TUI) UpdateWSPingLatency(pingLatency time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.wsPingLatency = pingLatency
}

func (t *TUI) SetTradeFactor(factor float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tradeFactor = factor
	t.markDirtyLocked()
}

func (t *TUI) SetMode(mode string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.mode = mode
	t.settings = normalizeTUISettingsForContext(t.settings, t.mode)
	t.markDirtyLocked()
}

func (t *TUI) SetExecutionBackend(backend string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.settings.ExecutionBackend = backend
	t.settings = normalizeTUISettingsForContext(t.settings, t.mode)
	t.markDirtyLocked()
}

func (t *TUI) ToggleTradingPause() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.manualTradingPause = !t.manualTradingPause
	if t.manualTradingPause {
		t.appendEventLocked("⏸️ Manual trading pause enabled")
	} else {
		t.appendEventLocked("▶️ Manual trading pause disabled")
	}
	return t.manualTradingPause
}

func (t *TUI) SetTradingPaused(paused bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.manualTradingPause == paused {
		return
	}
	t.manualTradingPause = paused
	t.markDirtyLocked()
}

func (t *TUI) IsTradingPaused() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.manualTradingPause
}

func (t *TUI) AddMarket(id string, slug string, outcomes []string, endTime time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.markets[id] = &MarketData{
		Slug:        slug,
		Outcomes:    outcomes,
		EndTime:     endTime,
		Bids:        make(map[string]float64),
		Asks:        make(map[string]float64),
		ClearedBids: make(map[string]bool),
		ClearedAsks: make(map[string]bool),
		RealBids:    make(map[string]float64),
		RealAsks:    make(map[string]float64),
	}
	for i := range t.orderHistory {
		if t.orderHistory[i].MarketID == id && strings.TrimSpace(t.orderHistory[i].MarketSlug) == "" {
			t.orderHistory[i].MarketSlug = slug
		}
	}
	t.markDirtyLocked()
}

func (t *TUI) SetMarketBinanceSignal(marketID string, signal MarketBinanceSignal) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if market, ok := t.markets[marketID]; ok {
		current := market.BinanceSignal
		if current.Enabled == signal.Enabled &&
			current.Symbol == signal.Symbol &&
			current.Price == signal.Price &&
			current.DeltaPercent == signal.DeltaPercent &&
			current.EffectiveGapPercent == signal.EffectiveGapPercent &&
			current.TargetOutcome == signal.TargetOutcome &&
			current.SignalLabel == signal.SignalLabel &&
			current.PolyFavorableMoveCents == signal.PolyFavorableMoveCents &&
			current.PolyAdverseMoveCents == signal.PolyAdverseMoveCents &&
			current.TargetSpreadCents == signal.TargetSpreadCents &&
			current.TargetBookImbalance == signal.TargetBookImbalance &&
			current.OppositeBookImbalance == signal.OppositeBookImbalance &&
			current.DirectionalBookScore == signal.DirectionalBookScore &&
			current.Ready == signal.Ready &&
			current.Status == signal.Status &&
			current.Reason == signal.Reason {
			return
		}
		market.BinanceSignal = signal
		t.markDisplayDirtyLocked(time.Now())
	}
}

func (t *TUI) SetMarketInventoryStatus(marketID, status string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	market, ok := t.markets[marketID]
	if !ok || market == nil {
		return
	}
	status = strings.TrimSpace(status)
	if market.InventoryStatus == status {
		return
	}
	market.InventoryStatus = status
	t.markDisplayDirtyLocked(time.Now())
}

func (t *TUI) ClearMarketInventoryStatus(marketID string) {
	t.SetMarketInventoryStatus(marketID, "")
}

func (t *TUI) ClearMarkets() {
	t.mu.Lock()
	defer t.mu.Unlock()

	const eps = 1e-6
	preservedMarkets := make(map[string]*MarketData)
	if t.engine != nil {
		for _, pos := range t.engine.GetPositions() {
			marketID := strings.TrimSpace(pos.MarketID)
			if marketID == "" || pos.Quantity <= eps {
				continue
			}
			if existing, ok := t.markets[marketID]; ok && existing != nil {
				preserved := *existing
				preserved.Bids = make(map[string]float64)
				preserved.Asks = make(map[string]float64)
				preserved.ClearedBids = make(map[string]bool)
				preserved.ClearedAsks = make(map[string]bool)
				preserved.RealBids = make(map[string]float64)
				preserved.RealAsks = make(map[string]float64)
				preserved.LastUpdate = time.Time{}
				preserved.LastDepthUpdate = time.Time{}
				preserved.DataSource = ""
				preserved.BinanceSignal = MarketBinanceSignal{}
				preservedMarkets[marketID] = &preserved
			}
		}
	}
	for marketID, positions := range t.walletTruth {
		marketID = strings.TrimSpace(marketID)
		if marketID == "" {
			continue
		}
		if _, exists := preservedMarkets[marketID]; exists {
			continue
		}
		for _, wt := range positions {
			if wt.LocalShares <= eps && wt.OnChainShares <= eps {
				continue
			}
			if existing, ok := t.markets[marketID]; ok && existing != nil {
				preserved := *existing
				preserved.Bids = make(map[string]float64)
				preserved.Asks = make(map[string]float64)
				preserved.ClearedBids = make(map[string]bool)
				preserved.ClearedAsks = make(map[string]bool)
				preserved.RealBids = make(map[string]float64)
				preserved.RealAsks = make(map[string]float64)
				preserved.LastUpdate = time.Time{}
				preserved.LastDepthUpdate = time.Time{}
				preserved.DataSource = ""
				preserved.BinanceSignal = MarketBinanceSignal{}
				preservedMarkets[marketID] = &preserved
			}
			break
		}
	}

	t.markets = preservedMarkets
	t.lastPrices = make(map[string]float64)
	t.lastBids = make(map[string]float64)
	t.lastAsks = make(map[string]float64)
	t.orderBookDepth = make(map[string]map[string][]MarketLevel)
	t.pendingOrders = make(map[string][]PendingOrder)
	t.orderBooks = make(map[string]*OrderBook)
	t.splitInventories = nil
	t.markDirtyLocked()
}

func (t *TUI) RegisterOrderBook(marketID string, orderBook *OrderBook) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.orderBooks == nil {
		t.orderBooks = make(map[string]*OrderBook)
	}
	t.orderBooks[marketID] = orderBook
}

func (t *TUI) getOpenOrdersSnapshot() []ScopedLimitOrder {
	t.mu.Lock()
	books := make(map[string]*OrderBook, len(t.orderBooks))
	for marketID, orderBook := range t.orderBooks {
		if orderBook != nil {
			books[marketID] = orderBook
		}
	}
	fallback := t.orderBook
	t.mu.Unlock()

	if len(books) == 0 {
		if fallback == nil {
			return nil
		}
		open := fallback.GetOpenOrders()
		scoped := make([]ScopedLimitOrder, 0, len(open))
		for _, order := range open {
			scoped = append(scoped, ScopedLimitOrder{Order: order})
		}
		return scoped
	}

	orders := make([]ScopedLimitOrder, 0)
	for marketID, orderBook := range books {
		for _, order := range orderBook.GetOpenOrders() {
			orders = append(orders, ScopedLimitOrder{MarketID: marketID, Order: order})
		}
	}
	return orders
}

func (t *TUI) CancelAllOrders() {
	t.mu.Lock()
	books := make([]*OrderBook, 0, len(t.orderBooks))
	for _, orderBook := range t.orderBooks {
		if orderBook != nil {
			books = append(books, orderBook)
		}
	}
	fallback := t.orderBook
	t.mu.Unlock()

	if len(books) == 0 {
		if fallback != nil {
			fallback.CancelAllOrders()
		}
		return
	}
	for _, orderBook := range books {
		orderBook.CancelAllOrders()
	}
}

func (t *TUI) CleanupOrderBooks(maxAge time.Duration) {
	t.mu.Lock()
	books := make([]*OrderBook, 0, len(t.orderBooks))
	for _, orderBook := range t.orderBooks {
		if orderBook != nil {
			books = append(books, orderBook)
		}
	}
	fallback := t.orderBook
	t.mu.Unlock()

	if len(books) == 0 {
		if fallback != nil {
			fallback.CleanupOldOrders(maxAge)
		}
		return
	}
	for _, orderBook := range books {
		orderBook.CleanupOldOrders(maxAge)
	}
}

func (t *TUI) UpdateMarketPrices(marketID string, bids, asks map[string]float64) {
	t.UpdateMarketPricesWithSource(marketID, bids, asks, "WS")
}

func (t *TUI) UpdateMarketPricesWithSource(marketID string, bids, asks map[string]float64, source string) {
	t.UpdateMarketPricesWithSourceAt(marketID, bids, asks, source, time.Now())
}

func (t *TUI) UpdateMarketPricesWithSourceAt(marketID string, bids, asks map[string]float64, source string, updatedAt time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if m, ok := t.markets[marketID]; ok {
		if updatedAt.IsZero() {
			updatedAt = time.Now()
		}
		if !m.LastUpdate.IsZero() && updatedAt.Sub(m.LastUpdate) < tuiQuoteDisplayMinInterval &&
			!tuiQuoteMapMoved(m.Bids, bids, tuiQuoteDisplayMinMove) &&
			!tuiQuoteMapMoved(m.Asks, asks, tuiQuoteDisplayMinMove) {
			return
		}
		for k, v := range bids {
			m.Bids[k] = v
			if v > 0 {
				m.RealBids[k] = v
				m.ClearedBids[k] = false
			} else {
				m.ClearedBids[k] = true
			}
		}
		for k, v := range asks {
			m.Asks[k] = v
			if v > 0 {
				m.RealAsks[k] = v
				m.ClearedAsks[k] = false
			} else {
				m.ClearedAsks[k] = true
			}
		}
		m.LastUpdate = updatedAt
		m.DataSource = source
		t.markDisplayDirtyLocked(updatedAt)
	}
}

func (t *TUI) TouchMarket(marketID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if m, ok := t.markets[marketID]; ok {
		m.LastUpdate = time.Now()
		t.markDisplayDirtyLocked(m.LastUpdate)
	}
}

func marketDepthLevelsEqual(a, b []MarketLevel) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Abs(a[i].Price-b[i].Price) > 1e-9 || math.Abs(a[i].Size-b[i].Size) > 1e-9 {
			return false
		}
	}
	return true
}

func (t *TUI) UpdateOrderBookDepth(marketID string, bids, asks map[string][]MarketLevel) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if market, ok := t.markets[marketID]; ok && !market.LastDepthUpdate.IsZero() && time.Since(market.LastDepthUpdate) < tuiDepthDisplayMinInterval {
		return
	}
	if t.orderBookDepth[marketID] == nil {
		t.orderBookDepth[marketID] = make(map[string][]MarketLevel)
	}
	marketDepth := t.orderBookDepth[marketID]
	depthChanged := false

	for outcome, levels := range bids {
		key := outcome + "_bids"
		if len(levels) > 0 {
			copied := make([]MarketLevel, 0, 5)
			for i := 0; i < len(levels) && i < 5; i++ {
				copied = append(copied, levels[i])
			}
			if existing, ok := marketDepth[key]; !ok || !marketDepthLevelsEqual(existing, copied) {
				depthChanged = true
			}
			marketDepth[key] = copied
		} else {
			if _, ok := marketDepth[key]; ok {
				depthChanged = true
			}
			delete(marketDepth, key)
		}
	}
	for outcome, levels := range asks {
		key := outcome + "_asks"
		if len(levels) > 0 {
			copied := make([]MarketLevel, 0, 5)
			for i := 0; i < len(levels) && i < 5; i++ {
				copied = append(copied, levels[i])
			}
			if existing, ok := marketDepth[key]; !ok || !marketDepthLevelsEqual(existing, copied) {
				depthChanged = true
			}
			marketDepth[key] = copied
		} else {
			if _, ok := marketDepth[key]; ok {
				depthChanged = true
			}
			delete(marketDepth, key)
		}
	}

	if depthChanged {
		if market, ok := t.markets[marketID]; ok {
			market.LastDepthUpdate = time.Now()
		}
		t.markDisplayDirtyLocked(time.Now())
	}
}

func (t *TUI) SetMarket(slug string, outcomes []string, endTime time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.marketSlug = slug
	t.outcomes = outcomes
	t.endTime = endTime
	t.markDirtyLocked()
}

func (t *TUI) UpdatePrices(prices map[string]float64, bids, asks map[string]float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, v := range prices {
		t.lastPrices[k] = v
	}
	for k, v := range bids {
		t.lastBids[k] = v
	}
	for k, v := range asks {
		t.lastAsks[k] = v
	}
	t.markDisplayDirtyLocked(time.Now())
}

func (t *TUI) UpdateRealMarket(bids, asks map[string]float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, v := range bids {
		t.realBids[k] = v
	}
	for k, v := range asks {
		t.realAsks[k] = v
	}
	t.markDisplayDirtyLocked(time.Now())
}

func pendingOrdersEqual(a, b []PendingOrder) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].MarketID != b[i].MarketID || a[i].Outcome != b[i].Outcome || a[i].Side != b[i].Side {
			return false
		}
		if math.Abs(a[i].Price-b[i].Price) > 1e-9 || math.Abs(a[i].Qty-b[i].Qty) > 1e-9 {
			return false
		}
	}
	return true
}

func tuiQuoteMapMoved(existing, incoming map[string]float64, minMove float64) bool {
	if len(incoming) == 0 {
		return false
	}
	for k, v := range incoming {
		if math.Abs(existing[k]-v) >= minMove {
			return true
		}
	}
	return false
}

func (t *TUI) appendEventLocked(msg string) {
	timestamp := time.Now().Format("15:04:05")
	t.eventLog = append(t.eventLog, fmt.Sprintf("[%s] %s", timestamp, core.SanitizeString(msg)))
	if len(t.eventLog) > t.maxEvents {
		t.eventLog = t.eventLog[len(t.eventLog)-t.maxEvents:]
	}
	t.markDirtyLocked()
}

func (t *TUI) resetSessionDisplayLocked() {
	t.orderHistory = nil
	t.roundHistory = nil
	t.pendingOrders = make(map[string][]PendingOrder)
	t.splitInventories = nil
	t.walletTruth = make(map[string][]WalletTruthPosition)
	t.walletCash = 0
	t.hasWalletCash = false
	t.isKilled = false
	t.killReason = ""
	t.amendedPnLForNextRound = 0
	t.lastDedupLogAt = make(map[string]time.Time)
	t.lastDedupLogMsg = make(map[string]string)
	t.startTime = time.Now()
}

func (t *TUI) ResetSessionDisplay() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resetSessionDisplayLocked()
	t.markDirtyLocked()
}

func (t *TUI) applyPaperBalanceLocked(balance float64) error {
	if t.engine == nil {
		return nil
	}
	if t.engine.CanResetPaperSession() {
		if err := t.engine.ResetPaperSession(balance); err != nil {
			return err
		}
		t.resetSessionDisplayLocked()
	} else {
		t.engine.RebaseBalance(balance)
	}
	t.markDirtyLocked()
	return nil
}

func (t *TUI) settingsChangeHookLocked() func() {
	if t.onSettingsChange == nil {
		return nil
	}
	settings := t.settings
	onChange := t.onSettingsChange
	return func() {
		onChange(settings)
	}
}

func (t *TUI) SetPendingOrders(marketID string, orders map[string][]PendingOrder) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pendingOrders == nil {
		t.pendingOrders = make(map[string][]PendingOrder)
	}
	flattened := make([]PendingOrder, 0)
	for outcome, batch := range orders {
		for _, order := range batch {
			if order.Outcome == "" {
				order.Outcome = outcome
			}
			order.MarketID = marketID
			flattened = append(flattened, order)
		}
	}
	if len(flattened) == 0 {
		if _, exists := t.pendingOrders[marketID]; exists {
			delete(t.pendingOrders, marketID)
			t.markDisplayDirtyLocked(time.Now())
		}
		return
	}
	if existing, ok := t.pendingOrders[marketID]; ok && pendingOrdersEqual(existing, flattened) {
		return
	}
	t.pendingOrders[marketID] = flattened
	t.markDisplayDirtyLocked(time.Now())
}

// StartRound marks the beginning of a new round, clearing any pending amendment deltas.
// Call this immediately after sampling the round's starting equity.
func (t *TUI) StartRound() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.amendedPnLForNextRound = 0
}

func (t *TUI) LogEvent(format string, args ...interface{}) {
	now := time.Now()
	body := core.SanitizeString(fmt.Sprintf(format, args...))

	var issueLogger *core.CSVLogger
	equity := 0.0

	t.mu.Lock()
	if body == t.lastEventBody && !t.lastEventAt.IsZero() && now.Sub(t.lastEventAt) < tuiConsecutiveLogDedupWindow {
		t.mu.Unlock()
		return
	}
	t.lastEventBody = body
	t.lastEventAt = now
	msg := fmt.Sprintf("[%s] %s", now.Format("15:04:05"), body)
	t.eventLog = append(t.eventLog, msg)
	if len(t.eventLog) > t.maxEvents {
		t.eventLog = t.eventLog[1:]
	}
	t.markDirtyLocked()
	if shouldPersistIssueEvent(msg) {
		issueLogger = t.issueLogger
		if t.engine != nil {
			equity = t.engine.GetEquity()
		}
	}
	t.mu.Unlock()

	if issueLogger != nil {
		issueLogger.Log(issueLogLevel(msg), extractIssueAsset(msg), "REALBOT_ISSUE", msg, equity)
	}
}

func (t *TUI) LogEventDedup(key string, interval time.Duration, format string, args ...interface{}) bool {
	if strings.TrimSpace(key) == "" {
		t.LogEvent(format, args...)
		return true
	}
	if interval <= 0 {
		interval = 10 * time.Second
	}

	timestamp := time.Now().Format("15:04:05")
	msg := fmt.Sprintf("[%s] %s", timestamp, fmt.Sprintf(format, args...))
	msg = core.SanitizeString(msg)

	var issueLogger *core.CSVLogger
	equity := 0.0

	t.mu.Lock()
	lastMsg := t.lastDedupLogMsg[key]
	lastAt := t.lastDedupLogAt[key]
	if msg == lastMsg && !lastAt.IsZero() && time.Since(lastAt) < interval {
		t.mu.Unlock()
		return false
	}
	t.lastDedupLogMsg[key] = msg
	t.lastDedupLogAt[key] = time.Now()
	t.eventLog = append(t.eventLog, msg)
	if len(t.eventLog) > t.maxEvents {
		t.eventLog = t.eventLog[1:]
	}
	t.markDirtyLocked()
	if shouldPersistIssueEvent(msg) {
		issueLogger = t.issueLogger
		if t.engine != nil {
			equity = t.engine.GetEquity()
		}
	}
	t.mu.Unlock()

	if issueLogger != nil {
		issueLogger.Log(issueLogLevel(msg), extractIssueAsset(msg), "REALBOT_ISSUE", msg, equity)
	}
	return true
}

func shouldPersistIssueEvent(msg string) bool {
	lower := strings.ToLower(msg)
	criticalPhrases := []string{
		"❌",
		"rejected",
		" failed",
		"failed:",
		"unbalanced",
		"legged",
		"kill switch",
		"cleanup failed",
		"merge failed",
		"merge skipped",
		"no confirmed fill",
		"snapshot unavailable",
		"still pending",
		"could not get resolution",
	}
	for _, phrase := range criticalPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

func issueLogLevel(msg string) string {
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "❌") || strings.Contains(lower, "rejected") {
		return "ERROR"
	}
	return "WARN"
}

func extractIssueAsset(msg string) string {
	closeTimeIdx := strings.Index(msg, "]")
	if closeTimeIdx == -1 || closeTimeIdx+1 >= len(msg) {
		return ""
	}
	remainder := strings.TrimSpace(msg[closeTimeIdx+1:])
	if !strings.HasPrefix(remainder, "[") {
		return ""
	}
	closeAssetIdx := strings.Index(remainder, "]")
	if closeAssetIdx <= 1 {
		return ""
	}
	return remainder[1:closeAssetIdx]
}

func (t *TUI) SetKillSwitch(reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.isKilled = true
	t.killReason = reason
	t.markDirtyLocked()
}

func (t *TUI) RecordOrder(marketID, outcome, side string, shares, price, cost, margin, profit float64, status string) {
	t.RecordOrderWithMode(marketID, outcome, side, shares, price, cost, margin, profit, "taker", status)
}

func (t *TUI) RecordOrderWithMode(marketID, outcome, side string, shares, price, cost, margin, profit float64, executionMode, status string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	marketSlug := ""
	if market, ok := t.markets[marketID]; ok && market != nil {
		marketSlug = market.Slug
	}
	entry := OrderHistoryEntry{
		Timestamp:     time.Now(),
		MarketID:      marketID,
		MarketSlug:    marketSlug,
		Outcome:       outcome,
		Side:          side,
		ExecutionMode: strings.ToLower(strings.TrimSpace(executionMode)),
		Shares:        shares,
		Price:         price,
		Cost:          cost,
		Margin:        margin,
		Profit:        profit,
		Status:        status,
	}
	t.orderHistory = append(t.orderHistory, entry)
	if len(t.orderHistory) > t.maxOrderHistory {
		t.orderHistory = t.orderHistory[len(t.orderHistory)-t.maxOrderHistory:]
	}
	t.markDirtyLocked()
}

func (t *TUI) RecordWalletSyncAdjustment(marketID, outcome string, deltaShares, markPrice float64, direction string) {
	// Silenced per user request to not show sync adjustments
	/*
		if math.Abs(deltaShares) < 1e-9 {
			return
		}
		side := "ADJ"
		switch {
		case deltaShares > 0:
			side = "ADJ+"
		case deltaShares < 0:
			side = "ADJ-"
		}
		if trimmed := strings.TrimSpace(direction); trimmed != "" {
			side = trimmed
		}
		t.RecordOrderWithMode(marketID, outcome, side, math.Abs(deltaShares), markPrice, 0.0, 0.0, 0.0, "wallet-sync", "SYNCED")
	*/
}

func (t *TUI) GetOrderHistory() []OrderHistoryEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	result := make([]OrderHistoryEntry, len(t.orderHistory))
	copy(result, t.orderHistory)
	return result
}

func roundHistoryShareSummary(positions map[string]Position, redemptions []*RedemptionResult) string {
	if len(positions) == 0 && len(redemptions) == 0 {
		return ""
	}

	type outcomeTotal struct {
		shares  float64
		cost    float64
		settled bool
		isWin   bool
	}
	type marketTotals struct {
		marketID string
		outcomes map[string]*outcomeTotal
	}
	byMarket := make(map[string]*marketTotals)

	getMarket := func(marketID string) *marketTotals {
		marketID = strings.TrimSpace(marketID)
		if marketID == "" {
			marketID = "UNKNOWN"
		}
		if _, ok := byMarket[marketID]; !ok {
			byMarket[marketID] = &marketTotals{
				marketID: marketID,
				outcomes: make(map[string]*outcomeTotal),
			}
		}
		return byMarket[marketID]
	}

	getOt := func(marketID, outcome string) *outcomeTotal {
		outcome = strings.TrimSpace(outcome)
		if outcome == "" {
			outcome = "Unknown"
		}
		market := getMarket(marketID)
		if _, ok := market.outcomes[outcome]; !ok {
			market.outcomes[outcome] = &outcomeTotal{}
		}
		return market.outcomes[outcome]
	}

	for _, pos := range positions {
		ot := getOt(pos.MarketID, pos.Outcome)
		ot.shares += pos.Quantity
		ot.cost += pos.TotalCost
	}

	for _, req := range redemptions {
		if req == nil {
			continue
		}
		if req.WinningShares > 0 && req.WinningOutcome != "" {
			ot := getOt(req.MarketID, req.WinningOutcome)
			ot.shares += req.WinningShares
			ot.cost += req.WinningCost
			ot.settled = true
			ot.isWin = true
		}
		if req.LosingShares > 0 && req.LosingOutcome != "" {
			ot := getOt(req.MarketID, req.LosingOutcome)
			ot.shares += req.LosingShares
			ot.cost += req.LosingCost
			ot.settled = true
		}
	}

	if len(byMarket) == 0 {
		return ""
	}

	marketIDs := make([]string, 0, len(byMarket))
	for marketID := range byMarket {
		marketIDs = append(marketIDs, marketID)
	}
	marketIDs = orderedMarketIDs(marketIDs)

	marketLines := make([]string, 0, len(marketIDs))
	for _, marketID := range marketIDs {
		market := byMarket[marketID]
		if market == nil || len(market.outcomes) == 0 {
			continue
		}

		type orderedItem struct {
			outcome string
			total   *outcomeTotal
		}
		ordered := make([]orderedItem, 0, len(market.outcomes))
		for outcome, total := range market.outcomes {
			ordered = append(ordered, orderedItem{outcome: outcome, total: total})
		}
		sort.Slice(ordered, func(i, j int) bool {
			return outcomeLess(ordered[i].outcome, ordered[j].outcome)
		})

		parts := make([]string, 0, len(ordered))
		for _, item := range ordered {
			avgPrice := 0.0
			if item.total.shares > 0 {
				avgPrice = item.total.cost / item.total.shares
			}
			text := fmt.Sprintf("%s %s@$%.2f", core.SanitizeString(item.outcome), formatDisplayShareQty(item.total.shares), avgPrice)
			if item.total.settled && item.total.isWin {
				text += styleGreen.Render(" ✓")
			} else if item.total.settled {
				text += styleRed.Render(" ✗")
			}
			parts = append(parts, text)
		}

		prefix := marketTimeSuffix(market.marketID)
		if prefix == "" {
			prefix = marketDisplayLabel(market.marketID)
		}
		marketLines = append(marketLines, prefix+": "+strings.Join(parts, "  |  "))
	}
	return strings.Join(marketLines, "\n")
}

func roundHistoryDisplayRedemptions(entry RoundHistoryEntry, redemptions []*RedemptionResult) []*RedemptionResult {
	if len(redemptions) == 0 {
		return nil
	}

	openMarkets := make(map[string]struct{})
	for _, pos := range entry.positions {
		if pos.Quantity <= 0.000001 {
			continue
		}
		marketID := strings.ToLower(strings.TrimSpace(pos.MarketID))
		if marketID == "" {
			continue
		}
		openMarkets[marketID] = struct{}{}
	}
	if len(openMarkets) == 0 {
		return redemptions
	}

	filtered := make([]*RedemptionResult, 0, len(redemptions))
	for _, redemption := range redemptions {
		if redemption == nil {
			continue
		}
		marketID := strings.ToLower(strings.TrimSpace(redemption.MarketID))
		if _, ok := openMarkets[marketID]; !ok {
			continue
		}
		filtered = append(filtered, redemption)
	}
	return filtered
}

func (t *TUI) RecordRound(startingEquity, endingEquity, pnl float64, trades int, positions map[string]Position, redemptions []*RedemptionResult) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if math.Abs(t.amendedPnLForNextRound) > 0.000001 {
		startingEquity += t.amendedPnLForNextRound
		pnl -= t.amendedPnLForNextRound
		t.amendedPnLForNextRound = 0
	}

	// Clone positions map
	posClone := make(map[string]Position, len(positions))
	for k, v := range positions {
		posClone[k] = v
	}

	// Clone redemptions slice
	redClone := make([]*RedemptionResult, len(redemptions))
	copy(redClone, redemptions)

	entry := RoundHistoryEntry{
		Number:         len(t.roundHistory) + 1,
		Timestamp:      time.Now(),
		StartingEquity: startingEquity,
		EndingEquity:   endingEquity,
		PnL:            pnl,
		Trades:         trades,
		ShareSummary:   roundHistoryShareSummary(positions, redemptions),
		positions:      posClone,
		redemptions:    redClone,
	}
	t.roundHistory = append(t.roundHistory, entry)
	if len(t.roundHistory) > t.maxRoundHistory {
		t.roundHistory = t.roundHistory[len(t.roundHistory)-t.maxRoundHistory:]
		for i := range t.roundHistory {
			t.roundHistory[i].Number = i + 1
		}
	}
	t.markDirtyLocked()
}

func roundHistoryPositionBookValueForMarket(positions map[string]Position, marketID string) (float64, bool) {
	marketID = strings.TrimSpace(marketID)
	if marketID == "" {
		return 0, false
	}

	marketPositions := make([]Position, 0, len(positions))
	for _, pos := range positions {
		if strings.EqualFold(strings.TrimSpace(pos.MarketID), marketID) {
			marketPositions = append(marketPositions, pos)
		}
	}
	if len(marketPositions) == 0 {
		return 0, false
	}

	if len(marketPositions) < 2 {
		value := 0.0
		for _, pos := range marketPositions {
			value += pos.TotalCost
		}
		return value, true
	}

	matchedQty := marketPositions[0].Quantity
	for _, pos := range marketPositions[1:] {
		if pos.Quantity < matchedQty {
			matchedQty = pos.Quantity
		}
	}
	if matchedQty < 0 {
		matchedQty = 0
	}

	value := matchedQty
	for _, pos := range marketPositions {
		if pos.Quantity <= 0 {
			continue
		}
		unmatchedQty := pos.Quantity - matchedQty
		if unmatchedQty <= 0 {
			continue
		}
		value += (pos.TotalCost / pos.Quantity) * unmatchedQty
	}
	return value, true
}

func roundHistoryRedemptionDelta(entry RoundHistoryEntry, pnlDelta float64, newRedemptions []*RedemptionResult) float64 {
	if len(newRedemptions) == 0 {
		return pnlDelta
	}

	delta := 0.0
	for _, req := range newRedemptions {
		if req == nil {
			continue
		}
		delta += req.TotalPnL
	}
	return delta
}

func amendRoundHistoryEntry(entry *RoundHistoryEntry, pnlDelta float64, newRedemptions []*RedemptionResult) {
	if entry == nil {
		return
	}
	if delta := roundHistoryRedemptionDelta(*entry, pnlDelta, newRedemptions); math.Abs(delta) >= 0.000001 {
		entry.PnL += delta
		entry.EndingEquity += delta
	}

	// Append new redemptions
	entry.redemptions = append(entry.redemptions, newRedemptions...)

	// Deduct the newly redeemed shares from the snapshot so they aren't double counted
	for _, req := range newRedemptions {
		reqMarketID := strings.TrimSpace(req.MarketID)
		winShares := req.WinningShares
		winCost := req.WinningCost
		loseShares := req.LosingShares
		loseCost := req.LosingCost

		for k, pos := range entry.positions {
			if reqMarketID != "" && !strings.EqualFold(strings.TrimSpace(pos.MarketID), reqMarketID) {
				continue
			}

			if winShares > 0 && strings.EqualFold(strings.TrimSpace(pos.Outcome), strings.TrimSpace(req.WinningOutcome)) {
				deductShares := winShares
				if deductShares > pos.Quantity {
					deductShares = pos.Quantity
				}
				// Pro-rate cost based on shares deducted, or use actual?
				// Since all shares in a position typically have the same average cost,
				// we just deduct whatever req says, capped at pos.TotalCost.
				deductCost := winCost
				if deductCost > pos.TotalCost {
					deductCost = pos.TotalCost
				}

				pos.Quantity -= deductShares
				pos.TotalCost -= deductCost
				winShares -= deductShares
				winCost -= deductCost

				if pos.Quantity <= 0.0001 {
					delete(entry.positions, k)
				} else {
					entry.positions[k] = pos
				}
			}

			if loseShares > 0 && strings.EqualFold(strings.TrimSpace(pos.Outcome), strings.TrimSpace(req.LosingOutcome)) {
				deductShares := loseShares
				if deductShares > pos.Quantity {
					deductShares = pos.Quantity
				}
				deductCost := loseCost
				if deductCost > pos.TotalCost {
					deductCost = pos.TotalCost
				}

				pos.Quantity -= deductShares
				pos.TotalCost -= deductCost
				loseShares -= deductShares
				loseCost -= deductCost

				if pos.Quantity <= 0.0001 {
					delete(entry.positions, k)
				} else {
					entry.positions[k] = pos
				}
			}
		}
	}

	// Update share summary with combined view
	entry.ShareSummary = roundHistoryShareSummary(entry.positions, entry.redemptions)
}

func rebaseSubsequentRoundHistoryEntries(history []RoundHistoryEntry, startIdx int, delta float64) {
	if math.Abs(delta) < 0.000001 {
		return
	}
	if startIdx < 0 {
		startIdx = 0
	}
	for i := startIdx; i < len(history); i++ {
		history[i].StartingEquity += delta
		history[i].EndingEquity += delta
	}
}

func roundHistoryEntryHasMarket(entry RoundHistoryEntry, marketID string) bool {
	marketID = strings.TrimSpace(marketID)
	if marketID == "" {
		return false
	}
	for _, pos := range entry.positions {
		if strings.EqualFold(strings.TrimSpace(pos.MarketID), marketID) {
			return true
		}
	}
	for _, redemption := range entry.redemptions {
		if redemption != nil && strings.EqualFold(strings.TrimSpace(redemption.MarketID), marketID) {
			return true
		}
	}
	return false
}

func roundHistoryEntryHasRedemptionForMarket(entry RoundHistoryEntry, marketID string) bool {
	marketID = strings.TrimSpace(marketID)
	if marketID == "" {
		return false
	}
	for _, redemption := range entry.redemptions {
		if redemption != nil && strings.EqualFold(strings.TrimSpace(redemption.MarketID), marketID) {
			return true
		}
	}
	return false
}

func roundHistoryRedemptionsForMarket(redemptions []*RedemptionResult, marketID string) []*RedemptionResult {
	marketID = strings.TrimSpace(marketID)
	if marketID == "" || len(redemptions) == 0 {
		return nil
	}
	filtered := make([]*RedemptionResult, 0, len(redemptions))
	for _, redemption := range redemptions {
		if redemption == nil || !strings.EqualFold(strings.TrimSpace(redemption.MarketID), marketID) {
			continue
		}
		filtered = append(filtered, redemption)
	}
	return filtered
}

func roundHistoryRemoveMarketPositions(entry *RoundHistoryEntry, marketID string) {
	if entry == nil {
		return
	}
	marketID = strings.TrimSpace(marketID)
	if marketID == "" || len(entry.positions) == 0 {
		return
	}
	for key, pos := range entry.positions {
		if strings.EqualFold(strings.TrimSpace(pos.MarketID), marketID) {
			delete(entry.positions, key)
		}
	}
}

func roundHistoryMarketPositions(entry RoundHistoryEntry, marketID string) map[string]Position {
	marketID = strings.TrimSpace(marketID)
	if marketID == "" || len(entry.positions) == 0 {
		return nil
	}

	filtered := make(map[string]Position)
	for _, pos := range entry.positions {
		if !strings.EqualFold(strings.TrimSpace(pos.MarketID), marketID) {
			continue
		}
		outcomeKey := strings.ToLower(strings.TrimSpace(pos.Outcome))
		if outcomeKey == "" {
			outcomeKey = strings.ToLower(strings.TrimSpace(pos.MarketID))
		}
		filtered[outcomeKey] = pos
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

func roundHistoryMarketStateChanged(prev, cur RoundHistoryEntry, marketID string) bool {
	prevPositions := roundHistoryMarketPositions(prev, marketID)
	curPositions := roundHistoryMarketPositions(cur, marketID)
	if len(prevPositions) != len(curPositions) {
		return true
	}
	for key, curPos := range curPositions {
		prevPos, ok := prevPositions[key]
		if !ok || roundHistoryPositionChanged(prevPos, curPos) {
			return true
		}
	}

	prevRedemptions := roundHistoryRedemptionsForMarket(prev.redemptions, marketID)
	curRedemptions := roundHistoryRedemptionsForMarket(cur.redemptions, marketID)
	if len(prevRedemptions) != len(curRedemptions) {
		return true
	}
	prevKeys := make(map[string]struct{}, len(prevRedemptions))
	for _, redemption := range prevRedemptions {
		if key := roundHistoryRedemptionKey(redemption); key != "" {
			prevKeys[key] = struct{}{}
		}
	}
	for _, redemption := range curRedemptions {
		key := roundHistoryRedemptionKey(redemption)
		if key == "" {
			continue
		}
		if _, ok := prevKeys[key]; !ok {
			return true
		}
	}
	return false
}

// AmendLatestRound updates the most recent round history entry with late background redemptions
func (t *TUI) AmendLatestRound(pnlDelta float64, newRedemptions []*RedemptionResult) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.roundHistory) == 0 {
		return
	}

	lastIdx := len(t.roundHistory) - 1
	beforeEnding := t.roundHistory[lastIdx].EndingEquity
	amendRoundHistoryEntry(&t.roundHistory[lastIdx], pnlDelta, newRedemptions)
	delta := t.roundHistory[lastIdx].EndingEquity - beforeEnding
	rebaseSubsequentRoundHistoryEntries(t.roundHistory, lastIdx+1, delta)
	t.amendedPnLForNextRound += delta

	t.markDirtyLocked()
}

// AmendMostRecentRoundForMarket updates the round-history entry where the
// market's carried state most recently changed, then rebases later snapshots
// that were only carrying that same unresolved inventory forward.
func (t *TUI) AmendMostRecentRoundForMarket(marketID string, pnlDelta float64, newRedemptions []*RedemptionResult) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.roundHistory) == 0 {
		return
	}

	targetIdx := -1
	for i := len(t.roundHistory) - 1; i >= 0; i-- {
		if roundHistoryEntryHasMarket(t.roundHistory[i], marketID) {
			targetIdx = i
			break
		}
	}
	if targetIdx < 0 {
		return
	}
	for targetIdx > 0 &&
		roundHistoryEntryHasMarket(t.roundHistory[targetIdx-1], marketID) &&
		!roundHistoryMarketStateChanged(t.roundHistory[targetIdx-1], t.roundHistory[targetIdx], marketID) {
		targetIdx--
	}

	beforeEnding := t.roundHistory[targetIdx].EndingEquity
	amendRoundHistoryEntry(&t.roundHistory[targetIdx], pnlDelta, newRedemptions)
	delta := t.roundHistory[targetIdx].EndingEquity - beforeEnding
	rebaseSubsequentRoundHistoryEntries(t.roundHistory, targetIdx+1, delta)
	t.amendedPnLForNextRound += delta

	for i := range t.roundHistory {
		if i == targetIdx || !roundHistoryEntryHasMarket(t.roundHistory[i], marketID) {
			continue
		}
		roundHistoryRemoveMarketPositions(&t.roundHistory[i], marketID)
		t.roundHistory[i].ShareSummary = roundHistoryShareSummary(t.roundHistory[i].positions, t.roundHistory[i].redemptions)
	}

	t.markDirtyLocked()
}

func (t *TUI) GetRoundHistory() []RoundHistoryEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	result := make([]RoundHistoryEntry, len(t.roundHistory))
	copy(result, t.roundHistory)
	return result
}

func (t *TUI) RegisterSplitInventory(inv *SplitInventory) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, existing := range t.splitInventories {
		if existing == inv {
			return
		}
	}
	t.splitInventories = append(t.splitInventories, inv)
}

func (t *TUI) SetWalletTruthPositions(marketID string, positions []WalletTruthPosition) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(positions) == 0 {
		delete(t.walletTruth, marketID)
		t.markDirtyLocked()
		return
	}
	if existing, ok := t.walletTruth[marketID]; ok && len(existing) > 0 {
		existingByOutcome := make(map[string]WalletTruthPosition, len(existing))
		for _, pos := range existing {
			existingByOutcome[walletTruthOutcomeKey(pos.Outcome)] = pos
		}
		for i := range positions {
			prev, ok := existingByOutcome[walletTruthOutcomeKey(positions[i].Outcome)]
			if !ok {
				continue
			}
			// Refreshes usually only carry local/on-chain balances. Preserve known
			// resolution metadata until a later resolution update overrides it.
			if positions[i].ResolutionStatus == "" {
				positions[i].ResolutionStatus = prev.ResolutionStatus
			}
			if !positions[i].IsWinner {
				positions[i].IsWinner = prev.IsWinner
			}
			if !positions[i].Redeemable {
				positions[i].Redeemable = prev.Redeemable
			}
		}
	}
	t.walletTruth[marketID] = append([]WalletTruthPosition(nil), positions...)
	t.markDirtyLocked()
}

func (t *TUI) UpdateWalletTruthRedeemable(marketID string, winningOutcome string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	positions, ok := t.walletTruth[marketID]
	if !ok {
		return
	}
	winningKey := walletTruthOutcomeKey(winningOutcome)
	for i := range positions {
		positions[i].ResolutionStatus = "resolved"
		positions[i].Redeemable = false
		if walletTruthOutcomeKey(positions[i].Outcome) == winningKey {
			positions[i].IsWinner = true
			if positions[i].OnChainShares > 0 {
				positions[i].Redeemable = true
				positions[i].ResolutionStatus = "redeemable"
			}
		} else {
			positions[i].IsWinner = false
		}
	}
	t.walletTruth[marketID] = positions
	t.markDirtyLocked()
}

// UpdateWalletTruthResolution updates resolution status for all positions in a market.
// winningOutcome is the outcome that won. If empty, just marks as "resolved" without a winner.
func (t *TUI) UpdateWalletTruthResolution(marketID string, resolved bool, winningOutcome string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	positions, ok := t.walletTruth[marketID]
	if !ok {
		return
	}
	winningKey := walletTruthOutcomeKey(winningOutcome)
	for i := range positions {
		if resolved {
			positions[i].ResolutionStatus = "resolved"
			if winningOutcome != "" {
				positions[i].IsWinner = walletTruthOutcomeKey(positions[i].Outcome) == winningKey
				positions[i].Redeemable = false
				if positions[i].IsWinner && positions[i].OnChainShares > 0 {
					positions[i].Redeemable = true
					positions[i].ResolutionStatus = "redeemable"
				}
			}
		} else {
			positions[i].ResolutionStatus = "unresolved"
			positions[i].IsWinner = false
			positions[i].Redeemable = false
		}
	}
	t.walletTruth[marketID] = positions
	t.markDirtyLocked()
}

func (t *TUI) ClearWalletTruthPositions(marketID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.walletTruth, marketID)
	t.markDirtyLocked()
}

func (t *TUI) getSplitPositions() []SplitPosition {
	t.mu.Lock()
	defer t.mu.Unlock()
	var all []SplitPosition
	for _, inv := range t.splitInventories {
		all = append(all, inv.GetAllPositions()...)
	}
	return all
}

func (t *TUI) getWalletTruthPositions() []WalletTruthPosition {
	t.mu.Lock()
	defer t.mu.Unlock()
	var all []WalletTruthPosition
	for _, positions := range t.walletTruth {
		all = append(all, positions...)
	}
	return all
}

// ─── Render Methods ───────────────────────────────────────────────────────────

// renderHeader: branded title bar + network health row.
//
//	╭─ ◆ POLYARB-15M TRADING TERMINAL ◆ ──────────────────────────────╮
//	│          ● REST 45ms  ● WS 12ms (2.1s)  ⏱ 1h23m  ·  [q] quit   │
//	╰───────────────────────────────────────────────────────────────────╯
func (m tuiModel) renderHeader(w int) string {
	s := m.snap
	inner := w - 4

	title := lipgloss.NewStyle().
		Bold(true).
		Foreground(clrBrand).
		Width(inner).
		Align(lipgloss.Center).
		Render("◆  POLYARB-15M TRADING TERMINAL  ◆")

	uptime := time.Since(s.startTime).Round(time.Second)
	uptimePart := styleDimmed.Render("⏱ " + uptime.String())
	quitPart := styleMuted.Render("[q] quit")
	settingsPart := lipgloss.NewStyle().Foreground(clrBrand).Render("[s] settings")
	clearPart := styleMuted.Render("[c] clear")

	sep := styleMuted.Render("  ·  ")
	info := "  " + uptimePart + sep + settingsPart + sep + clearPart + sep + quitPart

	content := title + "\n" + info
	return makePanel(inner, clrBrand, content)
}

// renderMarketInfo dispatches to multi- or single-market layout.
func (m tuiModel) renderMarketInfo(w int) string {
	s := m.snap
	if len(s.markets) > 0 {
		return m.renderMultiMarketGrid(w)
	}
	return m.renderSingleMarket(w)
}

// renderMultiMarketGrid renders all active markets in a responsive grid.
// ≥2 markets → 2-column rows; 1 market → full-width.
func (m tuiModel) renderMultiMarketGrid(w int) string {
	s := m.snap
	active := make([]string, 0, len(s.markets))
	for id, mkt := range s.markets {
		skip := false
		if mkt != nil && !mkt.EndTime.IsZero() && time.Now().After(mkt.EndTime) {
			skip = true
		}
		if !skip {
			for _, wt := range s.walletTruth {
				if wt.MarketID == id && wt.ResolutionStatus != "" && wt.ResolutionStatus != "unresolved" {
					skip = true
					break
				}
			}
		}
		if skip {
			continue // Skip resolving or resolved markets
		}
		active = append(active, id)
	}
	active = orderedMarketIDs(active)
	if len(active) == 0 {
		return ""
	}

	// Column geometry
	// 2-col: each panel = (w-1)/2, inner = (w-1)/2 - 4
	// 1-col: panel = w,           inner = w - 4
	twoCol := len(active) > 1
	colW := w
	innerW := w - 4
	if twoCol {
		colW = (w - 1) / 2
		innerW = colW - 4
	}
	if innerW < 8 {
		innerW = 8
	}
	_ = colW

	var sb strings.Builder

	totalMargin := 0.0
	marketCount := 0

	for i := 0; i < len(active); i += 2 {
		id1 := active[i]
		p1, margin1 := m.renderMarketPanel(id1, s.markets[id1], innerW, s.orderBookDepth)
		if margin1 != 0 {
			totalMargin += margin1
			marketCount++
		}

		if twoCol && i+1 < len(active) {
			id2 := active[i+1]
			p2, margin2 := m.renderMarketPanel(id2, s.markets[id2], innerW, s.orderBookDepth)
			if margin2 != 0 {
				totalMargin += margin2
				marketCount++
			}
			row := lipgloss.JoinHorizontal(lipgloss.Top, p1, " ", p2)
			sb.WriteString(row + "\n")
		} else {
			sb.WriteString(p1 + "\n")
		}
	}

	if marketCount > 0 {
		avgMargin := totalMargin / float64(marketCount)
		summary := fmt.Sprintf("  %d markets active  ·  Avg margin: %s",
			marketCount,
			marginStyle(avgMargin).Render(fmt.Sprintf("%.1f%%", avgMargin)))
		sb.WriteString(styleDimmed.Render(summary) + "\n")
	}

	return sb.String()
}

// renderMarketPanel builds one bordered market card.
// Returns the rendered string and the best available buy margin.
func (m tuiModel) renderMarketPanel(id string, mkt *MarketData, innerW int, depth map[string]map[string][]MarketLevel) (string, float64) {
	assetID := marketAssetID(id)
	emojis := map[string]string{"BTC": "₿", "ETH": "Ξ", "SOL": "◎", "XRP": "✕"}
	emoji := emojis[assetID]
	if emoji == "" {
		emoji = "•"
	}

	borderColor := assetBorderColors[assetID]
	if borderColor == "" {
		borderColor = clrSlate
	}

	// ── Header line: bold asset symbol
	header := lipgloss.NewStyle().Bold(true).Foreground(borderColor).
		Render(fmt.Sprintf("%s  %s", emoji, marketDisplayLabel(id)))

	// ── Slug (truncate to fit) when it adds information beyond the header.
	slug := core.SanitizeString(mkt.Slug)
	headerLabel := core.SanitizeString(marketDisplayLabel(id))
	slugLine := ""
	if slug != "" && !strings.EqualFold(slug, headerLabel) {
		slugLine = styleDimmed.Render(truncateText(slug, innerW))
	}

	// ── Time remaining
	remaining := time.Until(mkt.EndTime)
	if remaining < 0 {
		remaining = 0
	}
	timeSt := styleGreen
	if remaining < 2*time.Minute {
		timeSt = styleRed
	} else if remaining < 5*time.Minute {
		timeSt = styleYellow
	}

	// ── Staleness
	age := time.Since(mkt.LastUpdate)
	if !mkt.LastDepthUpdate.IsZero() {
		if depthAge := time.Since(mkt.LastDepthUpdate); depthAge < age {
			age = depthAge
		}
	}
	ageSt := styleGreen
	ageWarn := ""

	// Only show warning if prices are extremely old (> 60s) or the connection is actively failing
	wsLatency := time.Duration(0)
	if m.tui != nil {
		wsLatency = m.tui.wsLatency
	}

	isResolved := looksTerminalBook(mkt.Outcomes, mkt.Bids, mkt.Asks) || looksTerminalBook(mkt.Outcomes, mkt.RealBids, mkt.RealAsks)

	isUnhealthyWS := mkt.DataSource == "WS" && wsLatency > 15*time.Second && !isResolved
	if (age > 60*time.Second && !isResolved) || isUnhealthyWS {
		ageSt = styleRed
		ageWarn = " ⚠"
	} else if age > 10*time.Second && !isResolved {
		ageSt = styleYellow
	}

	srcSt := styleCyan
	src := mkt.DataSource
	if src == "" {
		src = "?"
		srcSt = styleMuted
	} else if src == "REST" {
		srcSt = styleYellow
	}

	timeLine := fmt.Sprintf("⏱ %s  ·  %s [%s]%s",
		timeSt.Render(remaining.Round(time.Second).String()),
		ageSt.Render(fmt.Sprintf("%dms", age.Milliseconds())),
		srcSt.Render(src),
		ageWarn,
	)

	var priceLinesB strings.Builder
	buyMargin := 0.0

	if len(mkt.Outcomes) == 2 {
		outStr0 := mkt.Outcomes[0]
		outStr1 := mkt.Outcomes[1]
		if !outcomeLess(outStr0, outStr1) {
			outStr0, outStr1 = outStr1, outStr0
		}

		bid1 := mkt.Bids[outStr0]
		ask1 := mkt.Asks[outStr0]
		bid2 := mkt.Bids[outStr1]
		ask2 := mkt.Asks[outStr1]

		bid1 = recentDisplayQuote(bid1, mkt.RealBids[outStr0], age, mkt.ClearedBids[outStr0])
		ask1 = recentDisplayQuote(ask1, mkt.RealAsks[outStr0], age, mkt.ClearedAsks[outStr0])
		bid2 = recentDisplayQuote(bid2, mkt.RealBids[outStr1], age, mkt.ClearedBids[outStr1])
		ask2 = recentDisplayQuote(ask2, mkt.RealAsks[outStr1], age, mkt.ClearedAsks[outStr1])
		if looksTerminalBook(mkt.Outcomes, mkt.RealBids, mkt.RealAsks) {
			// Preserve the last terminal-looking quotes even when the live WS feed
			// goes sparse near expiry, so the panel does not regress to "--.-".
			if bid1 == 0 {
				bid1 = mkt.RealBids[outStr0]
			}
			if ask1 == 0 {
				ask1 = mkt.RealAsks[outStr0]
			}
			if bid2 == 0 {
				bid2 = mkt.RealBids[outStr1]
			}
			if ask2 == 0 {
				ask2 = mkt.RealAsks[outStr1]
			}
		}

		// Supplement from order-book depth logic removed to prevent UI from pulling ghost prices from stale depth maps
		/*
			if d := depth[id]; d != nil {
				if bids := d[mkt.Outcomes[0]+"_bids"]; len(bids) > 0 && bids[0].Price > bid1 {
					bid1 = bids[0].Price
				}
				if asks := d[mkt.Outcomes[0]+"_asks"]; len(asks) > 0 && (ask1 == 0 || asks[0].Price < ask1) {
					ask1 = asks[0].Price
				}
				if bids := d[mkt.Outcomes[1]+"_bids"]; len(bids) > 0 && bids[0].Price > bid2 {
					bid2 = bids[0].Price
				}
				if asks := d[mkt.Outcomes[1]+"_asks"]; len(asks) > 0 && (ask2 == 0 || asks[0].Price < ask2) {
					ask2 = asks[0].Price
				}
			}
		*/

		// Infer from complement logic removed to prevent display artifacts when bot clears orderbook
		/*
			if bid1 == 0 && ask2 > 0 {
				bid1 = 1.0 - ask2
			}
			if ask1 == 0 && bid2 > 0 {
				ask1 = 1.0 - bid2
			}
			if bid2 == 0 && ask1 > 0 {
				bid2 = 1.0 - ask1
			}
			if ask2 == 0 && bid1 > 0 {
				ask2 = 1.0 - bid1
			}
		*/

		fmtP := func(v float64) string {
			if v <= 0 {
				return "--.-"
			}
			return fmt.Sprintf("%.2f", v)
		}

		o1 := core.SanitizeString(outStr0)
		o2 := core.SanitizeString(outStr1)
		maxLbl := 4
		if len(o1) > maxLbl {
			o1 = o1[:maxLbl]
		}
		if len(o2) > maxLbl {
			o2 = o2[:maxLbl]
		}

		// formatDepth is unused when depth display is disabled
		/*
			formatDepth := func(lvls []MarketLevel, idx int, c lipgloss.Style) string {
				if idx < len(lvls) {
					s := lvls[idx].Size
					sStr := fmt.Sprintf("%.0f", s)
					if s >= 1000 {
						sStr = fmt.Sprintf("%.1fk", s/1000)
					}
					if len(sStr) > 4 {
						sStr = sStr[:4]
					}
					str := fmt.Sprintf("%s@.%02.0f", sStr, lvls[idx].Price*100)
					return c.Render(fmt.Sprintf("%-8s", str))
				}
				return "        " // 8 spaces
			}
		*/

		// --- Outcome 1 ---
		priceLinesB.WriteString(fmt.Sprintf("  %-4s  %s %-5s  %s %-5s  %s\n",
			o1,
			styleGreen.Render("B:"), styleGreen.Render("$"+fmtP(bid1)),
			styleRed.Render("A:"), styleRed.Render("$"+fmtP(ask1)),
			styleDimmed.Render(fmt.Sprintf("↕%.2f", math.Max(0, ask1-bid1))),
		))

		// Depth display disabled
		/*
			if d := depth[id]; d != nil {
				o1Bids := d[mkt.Outcomes[0]+"_bids"]
				o1Asks := d[mkt.Outcomes[0]+"_asks"]
				for i := 1; i <= 2; i++ {
					bStr := formatDepth(o1Bids, i, styleGreen)
					aStr := formatDepth(o1Asks, i, styleRed)
					priceLinesB.WriteString(fmt.Sprintf("           %s  %s\n", bStr, aStr))
				}
			} else {
				priceLinesB.WriteString("\n\n")
			}
		*/

		// --- Outcome 2 ---
		priceLinesB.WriteString(fmt.Sprintf("  %-4s  %s %-5s  %s %-5s  %s\n",
			o2,
			styleGreen.Render("B:"), styleGreen.Render("$"+fmtP(bid2)),
			styleRed.Render("A:"), styleRed.Render("$"+fmtP(ask2)),
			styleDimmed.Render(fmt.Sprintf("↕%.2f", math.Max(0, ask2-bid2))),
		))

		// Depth display disabled
		/*
			if d := depth[id]; d != nil {
				o2Bids := d[mkt.Outcomes[1]+"_bids"]
				o2Asks := d[mkt.Outcomes[1]+"_asks"]
				for i := 1; i <= 2; i++ {
					bStr := formatDepth(o2Bids, i, styleGreen)
					aStr := formatDepth(o2Asks, i, styleRed)
					priceLinesB.WriteString(fmt.Sprintf("           %s  %s\n", bStr, aStr))
				}
			} else {
				priceLinesB.WriteString("\n\n")
			}
		*/

		pairFreshForDisplay := age <= recentQuoteDisplayGrace

		effBid1, effAsk1 := bid1, ask1
		effBid2, effAsk2 := bid2, ask2

		if effBid1 == 0 && effAsk2 > 0 {
			effBid1 = 1.0 - effAsk2
		}
		if effAsk1 == 0 && effBid2 > 0 {
			effAsk1 = 1.0 - effBid2
		}
		if effBid2 == 0 && effAsk1 > 0 {
			effBid2 = 1.0 - effAsk1
		}
		if effAsk2 == 0 && effBid1 > 0 {
			effAsk2 = 1.0 - effBid1
		}

		// Extreme/Peak pricing inference
		if effBid1 >= 0.95 || (effAsk2 > 0 && effAsk2 <= 0.05) {
			if effAsk1 == 0 {
				effAsk1 = 1.0
			}
		}
		if effBid2 >= 0.95 || (effAsk1 > 0 && effAsk1 <= 0.05) {
			if effAsk2 == 0 {
				effAsk2 = 1.0
			}
		}

		isExtreme := (effBid1 >= 0.95) || (effBid2 >= 0.95)
		pairFreshForDisplay = (age <= recentQuoteDisplayGrace) || isExtreme

		// Allow effBid to be 0 in extreme markets so the gap line still renders
		validLiquidity := effAsk1 > 0 && effAsk2 > 0 && (effBid1 > 0 || effBid2 > 0)
		if !isExtreme {
			validLiquidity = effBid1 > 0 && effAsk1 > 0 && effBid2 > 0 && effAsk2 > 0
		}

		if pairFreshForDisplay && validLiquidity {
			askSum := effAsk1 + effAsk2
			buyMargin = (1.0 - askSum) * 100
			bidSum := effBid1 + effBid2
			sellMargin := (bidSum - 1.0) * 100
			priceLinesB.WriteString(fmt.Sprintf("  Buy $%.3f %s  Sell $%.3f %s",
				askSum, marginStyle(buyMargin).Render(fmt.Sprintf("%+.1f%%", buyMargin)),
				bidSum, marginStyle(sellMargin).Render(fmt.Sprintf("%+.1f%%", sellMargin)),
			))
		} else if !pairFreshForDisplay {
			priceLinesB.WriteString(styleDimmed.Render("  ↻ awaiting price data…"))
		} else {
			priceLinesB.WriteString(styleDimmed.Render("  Waiting for liquidity…"))
		}

	}

	if mkt.BinanceSignal.Enabled {
		sig := mkt.BinanceSignal
		priceText := formatBinanceSignalPrice(sig.Symbol, sig.Price)

		statusLabel := strings.ToUpper(strings.TrimSpace(sig.Status))
		if statusLabel == "" {
			if sig.Ready {
				statusLabel = "READY"
			} else {
				statusLabel = "WAIT"
			}
		}
		statusStyle := styleYellow
		switch strings.ToLower(strings.TrimSpace(sig.Status)) {
		case "ready":
			statusStyle = styleGreen
		case "triggered":
			statusStyle = styleCyan
		case "blocked", "inactive":
			statusStyle = styleRed
		}

		target := core.SanitizeString(sig.TargetOutcome)

		// Line 1: Binance Price & Directional Signal
		binLine := fmt.Sprintf("  BIN: $%s (%+.3f%%)", priceText, sig.DeltaPercent)
		if target != "" {
			binLine += " 🎯 " + target
		} else if label := strings.TrimSpace(sig.SignalLabel); label != "" {
			binLine += " " + strings.ToUpper(label)
		}
		priceLinesB.WriteString("\n" + truncateText(binLine, innerW))

		// Line 2: The Actionable Gap & Metrics
		if sig.TargetOutcome != "" || sig.PolyFavorableMoveCents != 0 || sig.PolyAdverseMoveCents != 0 || sig.TargetSpreadCents != 0 || sig.DirectionalBookScore != 0 || sig.Ready {
			detailLine := fmt.Sprintf("  GAP: %.2f%% | SPRD: %.1fc | BOOK: %+.2f",
				sig.EffectiveGapPercent,
				sig.TargetSpreadCents,
				sig.DirectionalBookScore,
			)
			priceLinesB.WriteString("\n" + truncateText(detailLine, innerW-len(statusLabel)-2) + "  " + statusStyle.Render(statusLabel))
		} else {
			priceLinesB.WriteString("\n" + truncateText("  Status", innerW-len(statusLabel)-2) + "  " + statusStyle.Render(statusLabel))
		}

		if reason := strings.TrimSpace(sig.Reason); reason != "" {
			priceLinesB.WriteString("\n" + styleDimmed.Render(truncateText("  "+reason, innerW)))
		}
	}

	content := header + "\n"
	if slugLine != "" {
		content += slugLine + "\n"
	}
	content += timeLine + "\n" +
		"\n" +
		priceLinesB.String()

	return makePanel(innerW, borderColor, content), buyMargin
}

// renderSingleMarket handles the legacy single-market display.
func (m tuiModel) renderSingleMarket(w int) string {
	s := m.snap
	if s.marketSlug == "" {
		return ""
	}

	inner := w - 4
	remaining := time.Until(s.endTime)
	if remaining < 0 {
		remaining = 0
	}
	timeSt := styleGreen
	if remaining < 2*time.Minute {
		timeSt = styleRed
	} else if remaining < 5*time.Minute {
		timeSt = styleYellow
	}

	var sb strings.Builder
	sb.WriteString(sectionHeader("📊", "MARKET", clrTeal) + "\n")
	sb.WriteString(styleDimmed.Render("  "+s.marketSlug) + "\n")
	sb.WriteString(fmt.Sprintf("  ⏱ %s remaining\n", timeSt.Render(remaining.Round(time.Second).String())))

	if len(s.outcomes) == 2 {
		sb.WriteString("\n")
		sb.WriteString(m.renderSingleMarketPrices(s.outcomes, s.lastBids, s.lastAsks, s.realBids, s.realAsks, inner))
	}

	return makePanel(inner, clrTeal, sb.String())
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func clamp(n, low, high int) int {
	if n < low {
		return low
	}
	if n > high {
		return high
	}
	return n
}

func (m tuiModel) orderHistoryRows(twoColumn bool) int {
	h := m.snap.height
	if h <= 0 {
		if twoColumn {
			return defaultTwoColOrderRows
		}
		return defaultOneColOrderRows
	}
	extra := max(0, h-24)
	if twoColumn {
		return clamp(defaultTwoColOrderRows+extra/10, defaultTwoColOrderRows, 12)
	}
	return clamp(defaultOneColOrderRows+extra/12, defaultOneColOrderRows, 10)
}

func (m tuiModel) roundHistoryRows(twoColumn bool) int {
	h := m.snap.height
	if h <= 0 {
		if twoColumn {
			return defaultTwoColRoundRows
		}
		return defaultOneColRoundRows
	}
	extra := max(0, h-24)
	if twoColumn {
		return clamp(defaultTwoColRoundRows+extra/12, defaultTwoColRoundRows, 10)
	}
	return clamp(defaultOneColRoundRows+extra/14, defaultOneColRoundRows, 8)
}

func (m tuiModel) eventLogRows(twoColumn bool) int {
	h := m.snap.height
	if h <= 0 {
		if twoColumn {
			return defaultTwoColEventRows
		}
		return defaultOneColEventRows
	}
	extra := max(0, h-24)
	if twoColumn {
		return clamp(defaultTwoColEventRows+extra/4, defaultTwoColEventRows, 16)
	}
	return clamp(defaultOneColEventRows+extra/5, defaultOneColEventRows, 10)
}

func (m tuiModel) renderSingleMarketPrices(outcomes []string, bids, asks, realBids, realAsks map[string]float64, innerW int) string {
	s := m.snap
	var sb strings.Builder

	if len(outcomes) >= 2 {
		outStr0 := outcomes[0]
		outStr1 := outcomes[1]
		if !outcomeLess(outStr0, outStr1) {
			outcomes = []string{outStr1, outStr0}
		}
	}

	// ── Real market box ──
	sb.WriteString(styleCyan.Render("  ┌─ 🌐 Real Market") + "\n")
	realBid1 := realBids[outcomes[0]]
	realAsk1 := realAsks[outcomes[0]]
	realBid2 := realBids[outcomes[1]]
	realAsk2 := realAsks[outcomes[1]]

	if realAsk1 > 0 || realAsk2 > 0 {
		sb.WriteString(fmt.Sprintf("  │  %-6s  B: %s  A: %s\n",
			core.SanitizeString(outcomes[0]),
			styleGreen.Render(fmt.Sprintf("$%.2f", realBid1)),
			styleRed.Render(fmt.Sprintf("$%.2f", realAsk1)),
		))
		sb.WriteString(fmt.Sprintf("  │  %-6s  B: %s  A: %s\n",
			core.SanitizeString(outcomes[1]),
			styleGreen.Render(fmt.Sprintf("$%.2f", realBid2)),
			styleRed.Render(fmt.Sprintf("$%.2f", realAsk2)),
		))
	} else {
		sb.WriteString(styleDimmed.Render("  │  (waiting for real market data…)") + "\n")
	}

	// ── Bot REST reading ──
	sb.WriteString(styleYellow.Render("  ├─ 🤖 Bot Reading") + "\n")
	bid1, ask1 := bids[outcomes[0]], asks[outcomes[0]]
	bid2, ask2 := bids[outcomes[1]], asks[outcomes[1]]

	mismatch1 := realAsk1 > 0 && (abs(ask1-realAsk1) > 0.05 || abs(bid1-realBid1) > 0.05)
	mismatch2 := realAsk2 > 0 && (abs(ask2-realAsk2) > 0.05 || abs(bid2-realBid2) > 0.05)

	line1 := fmt.Sprintf("  │  %-6s  B: $%.2f  A: $%.2f", core.SanitizeString(outcomes[0]), bid1, ask1)
	if mismatch1 {
		line1 = styleRed.Render(line1) + "  " + styleRed.Render("⚠ MISMATCH")
	}
	sb.WriteString(line1 + "\n")

	line2 := fmt.Sprintf("  │  %-6s  B: $%.2f  A: $%.2f", core.SanitizeString(outcomes[1]), bid2, ask2)
	if mismatch2 {
		line2 = styleRed.Render(line2) + "  " + styleRed.Render("⚠ MISMATCH")
	}
	sb.WriteString(line2 + "\n")

	askSum := ask1 + ask2
	buyMargin := (1.0 - askSum) * 100
	bidSum := bid1 + bid2
	sellMargin := (bidSum - 1.0) * 100

	sb.WriteString(fmt.Sprintf("  │  Buy:  $%.3f  %s    Sell: $%.3f  %s\n",
		askSum, marginStyle(buyMargin).Render(fmt.Sprintf("%+.1f%%", buyMargin)),
		bidSum, marginStyle(sellMargin).Render(fmt.Sprintf("%+.1f%%", sellMargin)),
	))

	// ── Pending orders ──
	sb.WriteString(styleGreen.Render("  └─ 📋 Planned Orders") + "\n")
	if orders := s.pendingOrders[s.marketSlug]; len(orders) > 0 {
		for _, o := range orders {
			sb.WriteString(fmt.Sprintf("       %s %-6s  %.0f @ $%.2f\n",
				o.Side, core.SanitizeString(o.Outcome), o.Qty, o.Price))
		}
	} else {
		sb.WriteString(styleDimmed.Render("       (no pending orders)") + "\n")
	}

	return sb.String()
}

// renderAccountStatus: bordered panel showing balance, equity, and a progress bar.
//
//	╭─ 💼 ACCOUNT STATUS ──────────────────────────────────────────────╮
//	│  Cash $982.50  ·  Exposure $17.50  ·  Equity $1,002.30 (+$2.30) │
//	│  [████████░░░░░░░░░░] 20% deployed                               │
//	│  Trade 2.5% ($25/trade)  ·  Realized +$2.30  ·  Arb +$0.45      │
//	│  Compound 1.02×  ·  5 rounds  ·  Win 60%  ·  W/L 3/2  ·  ⏱ 1h23m │
//	╰──────────────────────────────────────────────────────────────────╯
func (m tuiModel) renderAccountStatus(w int, stats Stats, totalExposure, maxExposure, equity, bookEquity, multiplier, sizingBalance float64, rounds, profitable, losingRounds int, positions map[string]Position) string {
	s := m.snap
	inner := w - 4
	settings := s.settings
	if m.tui != nil && strings.TrimSpace(settings.PaperArbMode) == "" {
		settings = m.tui.settings
	}
	copytradeMode := isCopytradeSettingsMode(settings)
	ladderedMode := isLadderedTakerSettingsMode(settings)
	isRealMode := strings.EqualFold(s.mode, "Real") || strings.EqualFold(s.mode, "Live")
	paperExecutionMode := usesPaperExecutionSemantics(settings, s.mode)
	displayMode := s.mode
	if paperExecutionMode {
		displayMode = "Paper"
	}

	displayEquity := equity
	if isRealMode && !paperExecutionMode {
		displayEquity = bookEquity
	}
	displayCash := stats.CurrentBalance
	if isRealMode && !paperExecutionMode && s.hasWalletCash {
		displayCash = s.walletCash
	}
	sizingEquity := bookEquity

	netChange := displayEquity - stats.StartingBalance

	hasWalletTruthInventory := false
	for _, wt := range s.walletTruth {
		if wt.OnChainShares <= 0.000001 {
			continue
		}
		if wt.ResolutionStatus == "resolved" && !wt.IsWinner && !wt.Redeemable {
			continue
		}
		hasWalletTruthInventory = true
		break
	}

	displayRealized := stats.RealizedPnL
	if math.Abs(displayRealized) < 0.0001 && totalExposure <= 0.0001 && len(positions) == 0 && !hasWalletTruthInventory && math.Abs(netChange) >= 0.005 {
		displayRealized = netChange
	}
	displayNetChange := netChange
	if isRealMode && !paperExecutionMode {
		displayNetChange = displayRealized
	} else if totalExposure <= 0.0001 && len(positions) == 0 && !hasWalletTruthInventory && math.Abs(displayRealized-netChange) >= 0.005 {
		displayNetChange = displayRealized
	}
	changeSt := styleGreen
	if displayNetChange < 0 {
		changeSt = styleRed
	}

	realizedSt := styleGreen
	if displayRealized < 0 {
		realizedSt = styleRed
	}

	multSt := styleWhite
	if multiplier >= 1.5 {
		multSt = styleGreen
	} else if multiplier > 1.0 {
		multSt = styleYellow
	}

	// Guaranteed arb profit from matched pairs
	byMarket := make(map[string][]Position)
	for _, pos := range positions {
		mid := pos.MarketID
		if mid == "" {
			mid = "UNKNOWN"
		}
		byMarket[mid] = append(byMarket[mid], pos)
	}
	guaranteedProfit := 0.0
	for _, mps := range byMarket {
		if len(mps) == 2 {
			matched := mps[0].Quantity
			if mps[1].Quantity < matched {
				matched = mps[1].Quantity
			}
			cost := (mps[0].AvgPrice + mps[1].AvgPrice) * matched
			guaranteedProfit += matched - cost
		}
	}
	arbSt := styleGreen
	if guaranteedProfit < 0 {
		arbSt = styleRed
	}
	bestResolvePnL, worstResolvePnL := resolutionPnLRangeFromPositions(positions)

	// Deployment bar
	deployedPct := 0.0
	if displayEquity > 0 {
		deployedPct = totalExposure / displayEquity
	}
	barW := inner - 25 // leave room for label
	if barW < 8 {
		barW = 8
	}
	bar := renderBar(deployedPct, barW)
	barLine := fmt.Sprintf("  %s %s",
		bar,
		styleDimmed.Render(fmt.Sprintf("%.0f%% deployed", deployedPct*100)),
	)

	// Trade size
	tradeLine := ""
	tradeBudgetEquity := sizingEquity
	effectiveSizingBalance := sizingBalance
	if isRealMode {
		useCurrentEquityBudget := TakerCloseModeActive(settings)
		if useCurrentEquityBudget {
			if tradeBudgetEquity <= 0 {
				tradeBudgetEquity = math.Max(stats.StartingBalance, stats.CurrentBalance)
			}
		} else {
			if effectiveSizingBalance <= 0 {
				effectiveSizingBalance = math.Max(stats.StartingBalance, stats.CurrentBalance)
			}
			tradeBudgetEquity = effectiveSizingBalance
		}
		if tradeBudgetEquity < 0 {
			tradeBudgetEquity = 0
		}
		if effectiveSizingBalance < 0 {
			effectiveSizingBalance = 0
		}
	}
	if copytradeMode {
		if strings.EqualFold(settings.CopytradeSizingMode, core.CopytradeSizingModeShares) {
			tradeLine = fmt.Sprintf("  Copy %.5g shares  ·  ", settings.CopytradeSizeShares)
		} else if strings.EqualFold(settings.CopytradeSizingMode, core.CopytradeSizingModePercent) {
			tradeLine = fmt.Sprintf("  Copy %.1f%% master  ·  ", settings.CopytradeSizePercent)
		} else {
			tradeLine = fmt.Sprintf("  Copy $%.2f cap  ·  ", settings.CopytradeSizeUSDC)
		}
	} else if ladderedMode {
		if isLadderedTakerShareSizingMode(settings) {
			tradeLine = fmt.Sprintf("  Ladder %.5g shares  ·  ", settings.LadderedTakerSizeShares)
		} else {
			tradeLine = fmt.Sprintf("  Ladder $%.2f cap  ·  ", settings.LadderedTakerSizeUSDC)
		}
	} else if baseTradeCost, effectiveTradeCost := displayedTradeBudgetsWithMode(displayMode, stats.CurrentBalance, tradeBudgetEquity, stats.StartingBalance, effectiveSizingBalance, s.tradeFactor, settings.TradeSizeUSDC, s.maxTradeSize, multiplier, settings.TradeSizingMode); baseTradeCost > 0 {
		if strings.EqualFold(settings.TradeSizingMode, core.TradeSizingModeUSDC) {
			tradeLine = fmt.Sprintf("  Trade $%.2f fixed  ·  ", baseTradeCost)
		} else if strings.EqualFold(displayMode, "Paper") && math.Abs(effectiveTradeCost-baseTradeCost) > 0.005 {
			tradeLine = fmt.Sprintf("  Trade %.1f%%  ($%.2f base / $%.2f effective)  ·  ", s.tradeFactor*100, baseTradeCost, effectiveTradeCost)
		} else {
			tradeLine = fmt.Sprintf("  Trade %.1f%%  ($%.2f/trade)  ·  ", s.tradeFactor*100, baseTradeCost)
		}
	} else {
		tradeLine = "  "
	}
	tradeLine += fmt.Sprintf("Realized %s", realizedSt.Render(signedDollar(displayRealized)))
	if !copytradeMode {
		arbLabel := "Arb"
		if ladderedMode {
			arbLabel = "Matched"
		}
		tradeLine += fmt.Sprintf("  ·  %s %s", arbLabel, arbSt.Render(signedDollar(guaranteedProfit)))
	}
	if !copytradeMode && len(positions) > 0 && (math.Abs(bestResolvePnL) >= 0.005 || math.Abs(worstResolvePnL) >= 0.005) {
		resolveBestSt := styleGreen
		if bestResolvePnL < 0 {
			resolveBestSt = styleRed
		}
		resolveWorstSt := styleGreen
		if worstResolvePnL < 0 {
			resolveWorstSt = styleRed
		}
		resolveLabel := "Resolve"
		if ladderedMode {
			resolveLabel = "Resolve(if held)"
		}
		tradeLine += fmt.Sprintf("  ·  %s %s/%s",
			resolveLabel,
			resolveBestSt.Render(signedDollar(bestResolvePnL)),
			resolveWorstSt.Render(signedDollar(worstResolvePnL)),
		)
	}

	uptime := time.Since(s.startTime).Round(time.Second)
	roundWins, roundLosses, roundFlats := roundOutcomeCounts(s.roundHistory)
	winCount, lossCount, flatCount := 0, 0, 0
	displayRounds := rounds
	useRoundSummary := false
	switch {
	case len(s.roundHistory) > 0 && (rounds == 0 || len(s.roundHistory) >= rounds):
		useRoundSummary = true
		displayRounds = len(s.roundHistory)
		winCount = roundWins
		lossCount = roundLosses
		flatCount = roundFlats
	case rounds > 0:
		useRoundSummary = true
		if len(s.roundHistory) > 0 && len(s.roundHistory) == rounds {
			winCount = roundWins
			lossCount = roundLosses
			flatCount = roundFlats
		} else {
			winCount = profitable
			lossCount = losingRounds
			flatCount = rounds - profitable - losingRounds
			if flatCount < 0 {
				flatCount = 0
			}
		}
	default:
		winCount, lossCount = positionWinLossFromOrderHistory(s.orderHistory, strings.EqualFold(s.settings.PaperArbMode, "copytrade"))
		if winCount+lossCount == 0 {
			winCount = stats.WinningTrades
			lossCount = stats.LosingTrades
		}
	}
	totalDecisions := winCount + lossCount
	winRate := 0.0
	if totalDecisions > 0 {
		winRate = (float64(winCount) / float64(totalDecisions)) * 100
	}

	drawdownSt := styleWhite
	if stats.MaxDrawdown > 5.0 {
		drawdownSt = styleYellow
	}
	if stats.MaxDrawdown > 10.0 {
		drawdownSt = styleRed
	}

	header := sectionHeader("💼", "ACCOUNT STATUS", clrTeal)
	cashLabel := "Cash"
	cashText := fmt.Sprintf("$%.2f", displayCash)
	if isRealMode {
		cashLabel = "Spendable"
	}
	row1 := fmt.Sprintf("  %s %s  ·  Exposure %s  ·  Max Exp %s  ·  Equity %s  (%s)  ·  Max DD %s",
		cashLabel,
		styleBold.Render(cashText),
		styleWhite.Render(fmt.Sprintf("$%.2f", totalExposure)),
		styleWhite.Render(fmt.Sprintf("$%.2f", maxExposure)),
		styleBold.Render(fmt.Sprintf("$%.2f", displayEquity)),
		changeSt.Render(signedDollar(displayNetChange)),
		drawdownSt.Render(formatDrawdownCash(stats.MaxDrawdownCash)),
	)
	row3 := tradeLine
	row4 := ""
	if useRoundSummary {
		row4 = fmt.Sprintf("  Compound %s  ·  %d rounds  ·  Win %.0f%%  ·  W/L/F %d/%d/%d  ·  ⏱ %s",
			multSt.Render(fmt.Sprintf("%.2f×", multiplier)),
			displayRounds,
			winRate,
			winCount, lossCount, flatCount,
			styleDimmed.Render(uptime.String()),
		)
	} else {
		row4 = fmt.Sprintf("  Compound %s  ·  %d rounds  ·  Win %.0f%%  ·  W/L %d/%d  ·  ⏱ %s",
			multSt.Render(fmt.Sprintf("%.2f×", multiplier)),
			rounds,
			winRate,
			winCount, lossCount,
			styleDimmed.Render(uptime.String()),
		)
	}
	row5 := "  " + renderTradingHoursStatus(s.settings.TradingHoursMode, time.Now())

	content := header + "\n" + row1 + "\n" + barLine + "\n" + row3 + "\n" + row4 + "\n" + row5
	return makePanel(inner, clrTeal, content)
}

func positionWinLossFromOrderHistory(orderHistory []OrderHistoryEntry, isCopytrade bool) (wins, losses int) {
	positionPnL := make(map[string]float64)
	for _, entry := range orderHistory {
		if !strings.EqualFold(strings.TrimSpace(entry.Side), "SELL") {
			continue
		}
		status := strings.ToUpper(strings.TrimSpace(entry.Status))
		if status == "FAILED" {
			continue
		}
		marketID := strings.TrimSpace(entry.MarketID)
		outcome := strings.TrimSpace(entry.Outcome)
		if marketID == "" || outcome == "" {
			continue
		}
		var key string
		if isCopytrade {
			key = marketID
		} else {
			key = marketID + ":" + strings.ToLower(outcome)
		}
		positionPnL[key] += entry.Profit
	}
	for _, pnl := range positionPnL {
		if pnl > 0.0001 {
			wins++
		} else if pnl < -0.0001 {
			losses++
		}
	}
	return wins, losses
}

func resolutionPnLRangeFromPositions(positions map[string]Position) (best float64, worst float64) {
	type marketResolution struct {
		totalCost      float64
		outcomePayouts map[string]float64
	}

	byMarket := make(map[string]*marketResolution)
	for posKey, pos := range positions {
		marketID := strings.TrimSpace(pos.MarketID)
		if marketID == "" {
			marketID = posKey
		}
		entry := byMarket[marketID]
		if entry == nil {
			entry = &marketResolution{
				outcomePayouts: make(map[string]float64),
			}
			byMarket[marketID] = entry
		}
		entry.totalCost += pos.TotalCost
		entry.outcomePayouts[pos.Outcome] += pos.Quantity
	}

	for _, market := range byMarket {
		if len(market.outcomePayouts) == 0 {
			continue
		}

		marketBest := 0.0
		marketWorst := 0.0
		first := true
		for _, payout := range market.outcomePayouts {
			pnl := payout - market.totalCost
			if first {
				marketBest = pnl
				marketWorst = pnl
				first = false
				continue
			}
			if pnl > marketBest {
				marketBest = pnl
			}
			if pnl < marketWorst {
				marketWorst = pnl
			}
		}

		// Only one held side implies opposite-outcome full-loss scenario.
		if len(market.outcomePayouts) == 1 {
			fullLoss := -market.totalCost
			if fullLoss < marketWorst {
				marketWorst = fullLoss
			}
			if fullLoss > marketBest {
				marketBest = fullLoss
			}
		}

		best += marketBest
		worst += marketWorst
	}

	return best, worst
}

type marketPnLSummary struct {
	currentPnL      float64
	hasCurrentPnL   bool
	bestResolvePnL  float64
	worstResolvePnL float64
	bestOutcome     string
	worstOutcome    string
}

func summarizeMarketPnL(positions []PositionPnL) marketPnLSummary {
	summary := marketPnLSummary{}
	if len(positions) == 0 {
		return summary
	}

	totalCost := 0.0
	currentValue := 0.0
	allHaveCurrent := true
	outcomePayouts := make(map[string]float64)

	for _, pos := range positions {
		totalCost += pos.TotalCost
		outcomePayouts[pos.Outcome] += pos.Quantity
		if pos.CurrentBid > 0 {
			currentValue += pos.Quantity * pos.CurrentBid
		} else {
			allHaveCurrent = false
		}
	}

	if allHaveCurrent {
		summary.hasCurrentPnL = true
		summary.currentPnL = currentValue - totalCost
	}

	first := true
	for outcome, payout := range outcomePayouts {
		pnl := payout - totalCost
		if first {
			summary.bestResolvePnL = pnl
			summary.worstResolvePnL = pnl
			summary.bestOutcome = outcome
			summary.worstOutcome = outcome
			first = false
			continue
		}
		if pnl > summary.bestResolvePnL {
			summary.bestResolvePnL = pnl
			summary.bestOutcome = outcome
		}
		if pnl < summary.worstResolvePnL {
			summary.worstResolvePnL = pnl
			summary.worstOutcome = outcome
		}
	}

	if len(outcomePayouts) == 1 {
		fullLoss := -totalCost
		if fullLoss < summary.worstResolvePnL {
			summary.worstResolvePnL = fullLoss
			summary.worstOutcome = "Other"
		}
		if fullLoss > summary.bestResolvePnL {
			summary.bestResolvePnL = fullLoss
			summary.bestOutcome = "Other"
		}
	}

	return summary
}

func displayedTradeBudgets(mode string, cash, equity, startingBalance, sizingBalance, tradeFactor, maxTradeSize, multiplier float64) (base, effective float64) {
	return displayedTradeBudgetsWithMode(mode, cash, equity, startingBalance, sizingBalance, tradeFactor, 0, maxTradeSize, multiplier, core.TradeSizingModePercent)
}

// renderPositions: bordered panel for in-flight and split inventory positions.
func (m tuiModel) renderPositions(w int, positionsWithPnL map[string]PositionPnL) string {
	s := m.snap
	inner := w - 4
	settings := s.settings
	if m.tui != nil && strings.TrimSpace(settings.PaperArbMode) == "" {
		settings = m.tui.settings
	}
	ladderedMode := isLadderedTakerSettingsMode(settings)

	splitPositions := s.splitPositions
	walletTruthPositions := s.walletTruth
	inflightLegCount := 0
	for _, pos := range positionsWithPnL {
		if displayableInventoryShares(pos.Quantity) {
			inflightLegCount++
		}
	}
	showInFlightPositions := inflightLegCount > 0
	if TakerCloseModeActive(settings) {
		showInFlightPositions = false
	}
	hasSplitInventory := len(splitPositions) > 0
	hasWalletTruth := len(walletTruthPositions) > 0
	hasOnChainInventory := false
	if showOnChainInventory {
		for _, wt := range walletTruthPositions {
			if _, ok := walletTruthInventoryDisplayShares(wt); !ok {
				continue
			}
			hasOnChainInventory = true
			break
		}
	}
	hasWalletTruthDisplay := hasWalletTruth && showWalletTruthPanels

	if !showInFlightPositions && !hasSplitInventory && !hasOnChainInventory && !hasWalletTruthDisplay {
		return makePanel(inner, clrSlate,
			sectionHeader("📦", "POSITIONS", clrSlate)+"\n"+
				styleDimmed.Render("  (none)"))
	}

	var sb strings.Builder

	byMarket := make(map[string][]PositionPnL)
	if showInFlightPositions {
		for _, pos := range positionsWithPnL {
			if !displayableInventoryShares(pos.Quantity) {
				continue
			}
			mid := pos.MarketID
			if mid == "" {
				mid = "UNKNOWN"
			}
			byMarket[mid] = append(byMarket[mid], pos)
		}
	}

	// ── In-flight positions ──
	if showInFlightPositions && !isCopytradeSettingsMode(settings) {
		inflightStatus := styleYellow.Render("⏳ awaiting merge")
		if ladderedMode {
			inflightStatus = styleYellow.Render("⏳ open inventory")
		}
		headerCount := fmt.Sprintf("%d legs", inflightLegCount)
		if len(byMarket) > 0 {
			headerCount = fmt.Sprintf("%d markets / %d legs", len(byMarket), inflightLegCount)
		}
		sb.WriteString(sectionHeader("📦", fmt.Sprintf("IN-FLIGHT  (%s) %s",
			headerCount, inflightStatus), clrTeal) + "\n")
	} else {
		sb.WriteString(sectionHeader("📦", "POSITIONS", clrTeal) + "\n")
	}

	totalMarketPnL := 0.0
	hasMarketPrices := false
	totalBestResolvePnL, totalWorstResolvePnL := 0.0, 0.0

	marketIDs := make([]string, 0, len(byMarket))
	for marketID := range byMarket {
		marketIDs = append(marketIDs, marketID)
	}
	for _, marketID := range orderedMarketIDs(marketIDs) {
		mps, ok := byMarket[marketID]
		if !ok || len(mps) == 0 {
			continue
		}

		aStyle := getAssetStyle(marketID)
		marketStatus := styleYellow.Render(" [TRADING]")
		hasResolvedStatus := false
		isResolving := false

		if mkt, ok := s.markets[marketID]; ok && mkt != nil {
			if !mkt.EndTime.IsZero() && time.Now().After(mkt.EndTime) {
				isResolving = true
			}
		} else {
			parts := strings.Split(marketID, "-")
			if len(parts) > 1 {
				lastPart := parts[len(parts)-1]
				if ts, err := strconv.ParseInt(lastPart, 10, 64); err == nil && ts > 1000000000 {
					if time.Now().After(time.Unix(ts, 0)) {
						isResolving = true
					}
				}
			}
		}

		for _, wt := range walletTruthPositions {
			if wt.MarketID == marketID && wt.ResolutionStatus != "" && wt.ResolutionStatus != "unresolved" {
				marketStatus = styleDimmed.Render(" [" + strings.ToUpper(wt.ResolutionStatus) + "]")
				hasResolvedStatus = true
				isResolving = false
				break
			}
		}
		if !hasResolvedStatus {
			if market, ok := s.markets[marketID]; ok && market != nil {
				if status := strings.TrimSpace(market.InventoryStatus); status != "" {
					marketStatus = styleYellow.Render(" [" + strings.ToUpper(status) + "]")
					isResolving = false
				}
			}
		}

		if isResolving {
			marketStatus = styleDimmed.Render(" [WAITING RESOLUTION]")
		}

		sb.WriteString("  " + aStyle.Render("["+marketDisplayLabel(marketID)+"]") + marketStatus + "  ")

		sort.Slice(mps, func(i, j int) bool { return outcomeSortLess(mps[i].Outcome, mps[j].Outcome) })

		strs := make([]string, 0, len(mps))
		for _, pos := range mps {
			ps := fmt.Sprintf("%s: %s@$%.2f", core.SanitizeString(pos.Outcome), formatDisplayShareQty(pos.Quantity), pos.AvgPrice)
			if pos.CurrentBid > 0 {
				bidSt := styleGreen
				if pos.CurrentBid < pos.AvgPrice {
					bidSt = styleRed
				}
				ps += " (" + bidSt.Render(fmt.Sprintf("now:$%.2f", pos.CurrentBid)) + ")"
			}
			strs = append(strs, ps)
		}
		sb.WriteString(strings.Join(strs, "  │  "))

		summary := summarizeMarketPnL(mps)
		totalBestResolvePnL += summary.bestResolvePnL
		totalWorstResolvePnL += summary.worstResolvePnL

		signOf := func(v float64) (string, lipgloss.Style) {
			if v < 0 {
				return "", styleRed
			}
			return "+", styleGreen
		}

		if summary.hasCurrentPnL {
			totalMarketPnL += summary.currentPnL
			hasMarketPrices = true
			sg, pSt := signOf(summary.currentPnL)
			sb.WriteString("  →  " + pSt.Render(fmt.Sprintf("%s$%.2f", sg, summary.currentPnL)))

			if math.Abs(summary.bestResolvePnL) >= 0.005 || math.Abs(summary.worstResolvePnL) >= 0.005 {
				bestSign, bestStyle := signOf(summary.bestResolvePnL)
				worstSign, worstStyle := signOf(summary.worstResolvePnL)
				bestOut := core.SanitizeString(summary.bestOutcome)
				worstOut := core.SanitizeString(summary.worstOutcome)
				sb.WriteString("  │  🏁 " + bestStyle.Render(fmt.Sprintf("If %s wins: %s$%.2f", bestOut, bestSign, summary.bestResolvePnL)) + " / " +
					worstStyle.Render(fmt.Sprintf("If %s wins: %s$%.2f", worstOut, worstSign, summary.worstResolvePnL)))
			}
		} else if math.Abs(summary.bestResolvePnL-summary.worstResolvePnL) < 0.005 {
			sg, pSt := signOf(summary.bestResolvePnL)
			sb.WriteString("  →  🔒" + pSt.Render(fmt.Sprintf("%s$%.2f", sg, summary.bestResolvePnL)))
		} else if math.Abs(summary.bestResolvePnL) >= 0.005 || math.Abs(summary.worstResolvePnL) >= 0.005 {
			bestSign, bestStyle := signOf(summary.bestResolvePnL)
			worstSign, worstStyle := signOf(summary.worstResolvePnL)
			bestOut := core.SanitizeString(summary.bestOutcome)
			worstOut := core.SanitizeString(summary.worstOutcome)
			sb.WriteString("  →  🏁 " + bestStyle.Render(fmt.Sprintf("If %s wins: %s$%.2f", bestOut, bestSign, summary.bestResolvePnL)) + " / " +
				worstStyle.Render(fmt.Sprintf("If %s wins: %s$%.2f", worstOut, worstSign, summary.worstResolvePnL)))
		}
		sb.WriteString("\n")
	}

	// Total PnL summary
	if hasMarketPrices {
		mktSg, mktSt := func() (string, lipgloss.Style) {
			if totalMarketPnL < 0 {
				return "", styleRed
			}
			return "+", styleGreen
		}()
		marketLabel := "Now"
		resolveLabel := "Resolve"
		if ladderedMode {
			marketLabel = "Unrealized"
			resolveLabel = "Resolve(if held)"
		}
		summaryLine := styleBold.Render(fmt.Sprintf("  📊 %s: %s",
			marketLabel,
			mktSt.Render(fmt.Sprintf("%s$%.2f", mktSg, totalMarketPnL))))
		if math.Abs(totalBestResolvePnL-totalWorstResolvePnL) < 0.005 {
			lckSg, lckSt := func() (string, lipgloss.Style) {
				if totalBestResolvePnL < 0 {
					return "", styleRed
				}
				return "+", styleGreen
			}()
			summaryLine += styleBold.Render(fmt.Sprintf("  ·  🔒 Locked: %s",
				lckSt.Render(fmt.Sprintf("%s$%.2f", lckSg, totalBestResolvePnL))))
		} else if math.Abs(totalBestResolvePnL) >= 0.005 || math.Abs(totalWorstResolvePnL) >= 0.005 {
			bestSg, bestSt := func() (string, lipgloss.Style) {
				if totalBestResolvePnL < 0 {
					return "", styleRed
				}
				return "+", styleGreen
			}()
			worstSg, worstSt := func() (string, lipgloss.Style) {
				if totalWorstResolvePnL < 0 {
					return "", styleRed
				}
				return "+", styleGreen
			}()
			summaryLine += styleBold.Render(fmt.Sprintf("  ·  🏁 %s: %s/%s",
				resolveLabel,
				bestSt.Render(fmt.Sprintf("%s$%.2f", bestSg, totalBestResolvePnL)),
				worstSt.Render(fmt.Sprintf("%s$%.2f", worstSg, totalWorstResolvePnL))))
		}
		sb.WriteString(summaryLine + "\n")
	} else if math.Abs(totalBestResolvePnL-totalWorstResolvePnL) < 0.005 && math.Abs(totalBestResolvePnL) >= 0.005 {
		sg, pSt := func() (string, lipgloss.Style) {
			if totalBestResolvePnL < 0 {
				return "", styleRed
			}
			return "+", styleGreen
		}()
		sb.WriteString(styleBold.Render("  🔒 Locked: "+pSt.Render(fmt.Sprintf("%s$%.2f", sg, totalBestResolvePnL))) + "\n")
	} else if math.Abs(totalBestResolvePnL) >= 0.005 || math.Abs(totalWorstResolvePnL) >= 0.005 {
		bestSg, bestSt := func() (string, lipgloss.Style) {
			if totalBestResolvePnL < 0 {
				return "", styleRed
			}
			return "+", styleGreen
		}()
		worstSg, worstSt := func() (string, lipgloss.Style) {
			if totalWorstResolvePnL < 0 {
				return "", styleRed
			}
			return "+", styleGreen
		}()
		resolveLabel := "Resolve"
		if ladderedMode {
			resolveLabel = "Resolve(if held)"
		}
		sb.WriteString(styleBold.Render(fmt.Sprintf("  🏁 %s: %s/%s",
			resolveLabel,
			bestSt.Render(fmt.Sprintf("%s$%.2f", bestSg, totalBestResolvePnL)),
			worstSt.Render(fmt.Sprintf("%s$%.2f", worstSg, totalWorstResolvePnL)))) + "\n")
	}

	// ── Split inventory ──
	if hasSplitInventory {
		sb.WriteString("\n" + sectionHeader("🔀", "SPLIT INVENTORY  (panic sell)", clrTeal) + "\n")
		splitByMarket := make(map[string][]SplitPosition)
		for _, sp := range splitPositions {
			splitByMarket[sp.MarketID] = append(splitByMarket[sp.MarketID], sp)
		}

		splitMarketIDs := make([]string, 0, len(splitByMarket))
		for marketID := range splitByMarket {
			splitMarketIDs = append(splitMarketIDs, marketID)
		}
		for _, marketID := range orderedMarketIDs(splitMarketIDs) {
			sps, ok := splitByMarket[marketID]
			if !ok || len(sps) == 0 {
				continue
			}
			aStyle := getAssetStyle(marketID)
			sb.WriteString("  " + aStyle.Render("["+marketDisplayLabel(marketID)+"]") + "  ")

			sort.Slice(sps, func(i, j int) bool { return outcomeSortLess(sps[i].Outcome, sps[j].Outcome) })
			strs := make([]string, 0, len(sps))
			for _, sp := range sps {
				strs = append(strs, fmt.Sprintf("%s: %s@$%.4f",
					core.SanitizeString(sp.Outcome), formatDisplayShareQty(sp.Shares), sp.CostBasis))
			}
			sb.WriteString(strings.Join(strs, "  │  "))

			if len(sps) >= 2 {
				minSh := sps[0].Shares
				for _, sp := range sps[1:] {
					if sp.Shares < minSh {
						minSh = sp.Shares
					}
				}
				sb.WriteString("  →  " + styleGreen.Render(fmt.Sprintf("%s pairs sellable", formatDisplayShareQty(minSh))))
			}
			sb.WriteString("\n")
		}
	}

	if hasOnChainInventory {
		onChainInventoryByMarket := make(map[string][]WalletTruthPosition)
		onChainInventoryCount := 0
		for _, wt := range walletTruthPositions {
			if _, ok := walletTruthInventoryDisplayShares(wt); !ok {
				continue
			}
			onChainInventoryByMarket[wt.MarketID] = append(onChainInventoryByMarket[wt.MarketID], wt)
			onChainInventoryCount++
		}
		if onChainInventoryCount > 0 {
			sb.WriteString("\n" + sectionHeader("🏦", "ON-CHAIN INVENTORY", clrTeal) + "\n")
			onChainMarketIDs := make([]string, 0, len(onChainInventoryByMarket))
			for marketID := range onChainInventoryByMarket {
				onChainMarketIDs = append(onChainMarketIDs, marketID)
			}
			for _, marketID := range orderedMarketIDs(onChainMarketIDs) {
				positions := onChainInventoryByMarket[marketID]
				if len(positions) == 0 {
					continue
				}
				aStyle := getAssetStyle(marketID)
				sort.Slice(positions, func(i, j int) bool { return outcomeSortLess(positions[i].Outcome, positions[j].Outcome) })
				parts := make([]string, 0, len(positions))
				for _, wt := range positions {
					displayShares, _ := walletTruthInventoryDisplayShares(wt)
					parts = append(parts, fmt.Sprintf("%s: %s %s",
						core.SanitizeString(wt.Outcome),
						formatDisplayShareQty(displayShares),
						walletTruthInventoryStatus(wt),
					))
				}
				sb.WriteString("  " + aStyle.Render("["+marketDisplayLabel(marketID)+"]") + "  " + strings.Join(parts, "  │  ") + "\n")
			}
		}

	}

	if hasWalletTruthDisplay {
		sb.WriteString("\n" + sectionHeader("🧾", "WALLET TRUTH  (local vs on-chain)", clrTeal) + "\n")
		truthByMarket := make(map[string][]WalletTruthPosition)
		marketSet := make(map[string]struct{})
		for _, wt := range walletTruthPositions {
			if !displayableInventoryShares(wt.LocalShares) && !displayableInventoryShares(wt.OnChainShares) {
				continue
			}
			truthByMarket[wt.MarketID] = append(truthByMarket[wt.MarketID], wt)
			marketSet[wt.MarketID] = struct{}{}
		}

		orderedMarkets := make([]string, 0, len(marketSet))
		for marketID := range marketSet {
			orderedMarkets = append(orderedMarkets, marketID)
		}
		orderedMarkets = orderedMarketIDs(orderedMarkets)

		for _, marketID := range orderedMarkets {
			positions := truthByMarket[marketID]
			if len(positions) == 0 {
				continue
			}
			aStyle := getAssetStyle(marketID)
			sort.Slice(positions, func(i, j int) bool { return outcomeSortLess(positions[i].Outcome, positions[j].Outcome) })
			parts := make([]string, 0, len(positions))
			marketWarning := false
			for _, wt := range positions {
				driftStyle := styleGreen
				marker := "✅"
				if math.Abs(wt.Drift) >= 0.01 {
					driftStyle = styleRed
					marker = "⚠"
					marketWarning = true
				}

				redeemTag := ""
				if wt.Redeemable {
					redeemTag = styleGreen.Render(" [REDEEMABLE 💰]")
				} else if wt.IsWinner {
					redeemTag = styleGreen.Render(" [WINNER ✓]")
				} else if wt.ResolutionStatus == "resolved" && !wt.IsWinner {
					redeemTag = styleRed.Render(" [LOSER ✗]")
				} else if wt.ResolutionStatus == "unresolved" && (displayableInventoryShares(wt.LocalShares) || displayableInventoryShares(wt.OnChainShares)) {
					redeemTag = styleYellow.Render(" [RESOLVING ⏳]")
				}

				parts = append(parts, fmt.Sprintf("%s %s L:%s C:%s Δ:%s%s",
					marker,
					core.SanitizeString(wt.Outcome),
					formatDisplayShareQty(wt.LocalShares),
					formatDisplayShareQty(wt.OnChainShares),
					driftStyle.Render(formatSignedDisplayShareQty(wt.Drift)),
					redeemTag,
				))
			}
			prefix := "  " + aStyle.Render("["+marketDisplayLabel(marketID)+"]") + "  "
			if marketWarning {
				prefix = "  " + styleYellow.Render("⚠") + " " + aStyle.Render("["+marketDisplayLabel(marketID)+"]") + "  "
			}
			sb.WriteString(prefix + strings.Join(parts, "  │  ") + "\n")
		}
	}

	return makePanel(inner, clrTeal, sb.String())
}

// renderOrders: open limit orders panel.
func (m tuiModel) renderOrders(w int, orders []ScopedLimitOrder) string {
	if len(orders) == 0 {
		return ""
	}
	inner := w - 4
	var sb strings.Builder
	sb.WriteString(sectionHeader("📝", fmt.Sprintf("LIMIT ORDERS  (%d)", len(orders)), clrSlate) + "\n")

	byOutcome := make(map[string][]ScopedLimitOrder)
	for _, o := range orders {
		if o.Order == nil {
			continue
		}
		key := o.Order.Outcome
		if o.MarketID != "" {
			key = o.MarketID + ":" + key
		}
		byOutcome[key] = append(byOutcome[key], o)
	}
	// Sorted outcomes for stable render
	outcomes := make([]string, 0, len(byOutcome))
	for oc := range byOutcome {
		outcomes = append(outcomes, oc)
	}
	sort.Slice(outcomes, func(i, j int) bool { return outcomeLess(outcomes[i], outcomes[j]) })

	for _, oc := range outcomes {
		ords := byOutcome[oc]
		totalQty, totalVal := 0.0, 0.0
		for _, o := range ords {
			totalQty += o.Order.RemainingQty()
			totalVal += o.Order.RemainingQty() * o.Order.Price
		}
		sb.WriteString(fmt.Sprintf("  %-8s  %s orders  ·  %.0f shares  ·  $%.2f value\n",
			core.SanitizeString(oc), styleDimmed.Render(fmt.Sprintf("%d", len(ords))),
			totalQty, totalVal))
	}

	return makePanel(inner, clrSlate, sb.String())
}

func roundHistoryOpenInventoryCounts(entry RoundHistoryEntry) (markets, legs int) {
	if len(entry.positions) == 0 {
		return 0, 0
	}
	resolvedMarkets := make(map[string]struct{}, len(entry.redemptions))
	for _, redemption := range entry.redemptions {
		if redemption == nil {
			continue
		}
		marketID := strings.TrimSpace(redemption.MarketID)
		if marketID == "" {
			continue
		}
		resolvedMarkets[strings.ToLower(marketID)] = struct{}{}
	}
	openMarkets := make(map[string]struct{})
	for _, pos := range entry.positions {
		marketID := strings.TrimSpace(pos.MarketID)
		if marketID == "" {
			marketID = "UNKNOWN"
		}
		marketKey := strings.ToLower(marketID)
		if _, ok := resolvedMarkets[marketKey]; ok {
			continue
		}
		if !displayableInventoryShares(pos.Quantity) {
			continue
		}
		openMarkets[marketKey] = struct{}{}
		legs++
	}
	return len(openMarkets), legs
}

func roundHistoryHasOpenInventory(entry RoundHistoryEntry) bool {
	_, legs := roundHistoryOpenInventoryCounts(entry)
	return legs > 0
}

func roundHistoryOpenCarryCost(entry RoundHistoryEntry) float64 {
	if len(entry.positions) == 0 {
		return 0
	}
	resolvedMarkets := make(map[string]struct{}, len(entry.redemptions))
	for _, redemption := range entry.redemptions {
		if redemption == nil {
			continue
		}
		marketID := strings.TrimSpace(redemption.MarketID)
		if marketID == "" {
			continue
		}
		resolvedMarkets[strings.ToLower(marketID)] = struct{}{}
	}

	total := 0.0
	for _, pos := range entry.positions {
		marketID := strings.ToLower(strings.TrimSpace(pos.MarketID))
		if _, ok := resolvedMarkets[marketID]; ok {
			continue
		}
		if !displayableInventoryShares(pos.Quantity) {
			continue
		}
		cost := pos.TotalCost
		if cost <= 0 && pos.Quantity > 0 && pos.AvgPrice > 0 {
			cost = pos.Quantity * pos.AvgPrice
		}
		total += cost
	}
	return total
}

func roundHistoryPositionChanged(prev Position, cur Position) bool {
	return !strings.EqualFold(strings.TrimSpace(prev.MarketID), strings.TrimSpace(cur.MarketID)) ||
		!strings.EqualFold(strings.TrimSpace(prev.Outcome), strings.TrimSpace(cur.Outcome)) ||
		math.Abs(prev.Quantity-cur.Quantity) > 0.000001 ||
		math.Abs(prev.TotalCost-cur.TotalCost) > 0.000001
}

func roundHistoryRedemptionKey(redemption *RedemptionResult) string {
	if redemption == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(redemption.MarketID)) + "|" +
		strings.ToLower(strings.TrimSpace(redemption.WinningOutcome)) + "|" +
		strings.ToLower(strings.TrimSpace(redemption.LosingOutcome)) + "|" +
		fmt.Sprintf("%.6f|%.6f|%.6f", redemption.WinningShares, redemption.LosingShares, redemption.TotalPayout)
}

func roundHistoryDisplaySummary(history []RoundHistoryEntry, idx int) string {
	if idx < 0 || idx >= len(history) {
		return ""
	}
	entry := history[idx]
	if len(entry.positions) == 0 && len(entry.redemptions) == 0 {
		return entry.ShareSummary
	}
	if idx == 0 {
		return roundHistoryShareSummary(entry.positions, roundHistoryDisplayRedemptions(entry, entry.redemptions))
	}

	prev := history[idx-1]
	filteredPositions := make(map[string]Position)
	for key, pos := range entry.positions {
		prevPos, ok := prev.positions[key]
		if ok && !roundHistoryPositionChanged(prevPos, pos) {
			continue
		}
		filteredPositions[key] = pos
	}

	prevRedemptions := make(map[string]struct{}, len(prev.redemptions))
	for _, redemption := range prev.redemptions {
		if key := roundHistoryRedemptionKey(redemption); key != "" {
			prevRedemptions[key] = struct{}{}
		}
	}
	filteredRedemptions := make([]*RedemptionResult, 0, len(entry.redemptions))
	for _, redemption := range entry.redemptions {
		key := roundHistoryRedemptionKey(redemption)
		if key == "" {
			continue
		}
		if _, seen := prevRedemptions[key]; seen {
			continue
		}
		filteredRedemptions = append(filteredRedemptions, redemption)
	}

	filteredRedemptions = roundHistoryDisplayRedemptions(entry, filteredRedemptions)

	return roundHistoryShareSummary(filteredPositions, filteredRedemptions)
}

func wrapRoundHistoryDisplaySummary(summary string, width int) []string {
	if strings.TrimSpace(summary) == "" {
		return nil
	}
	if width < 12 {
		width = 12
	}

	lines := make([]string, 0, strings.Count(summary, "\n")+1)
	for _, rawLine := range strings.Split(summary, "\n") {
		rawLine = strings.TrimRight(rawLine, " ")
		if rawLine == "" {
			continue
		}
		for _, expandedLine := range expandRoundHistoryOutcomeLine(rawLine) {
			expandedLine = strings.TrimRight(expandedLine, " ")
			if expandedLine == "" {
				continue
			}
			for _, wrappedLine := range strings.Split(ansi.Wordwrap(expandedLine, width, " "), "\n") {
				wrappedLine = strings.TrimRight(wrappedLine, " ")
				if wrappedLine == "" {
					continue
				}
				lines = append(lines, wrappedLine)
			}
		}
	}
	return lines
}

func expandRoundHistoryOutcomeLine(line string) []string {
	idx := strings.Index(line, ": ")
	if idx < 0 {
		return []string{line}
	}

	prefix := line[:idx+2]
	remainder := line[idx+2:]
	parts := strings.Split(remainder, "  |  ")
	if len(parts) <= 1 {
		return []string{line}
	}

	lines := make([]string, 0, len(parts))
	continuationPrefix := strings.Repeat(" ", lipgloss.Width(prefix))
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if i == 0 {
			lines = append(lines, prefix+part)
		} else {
			lines = append(lines, continuationPrefix+part)
		}
	}
	if len(lines) == 0 {
		return []string{line}
	}
	return lines
}

func positionsWithPnLOpenInventoryCounts(positions map[string]PositionPnL) (markets, legs int) {
	if len(positions) == 0 {
		return 0, 0
	}
	openMarkets := make(map[string]struct{})
	for _, pos := range positions {
		if !displayableInventoryShares(pos.Quantity) {
			continue
		}
		marketID := strings.TrimSpace(pos.MarketID)
		if marketID == "" {
			marketID = "UNKNOWN"
		}
		openMarkets[strings.ToLower(marketID)] = struct{}{}
		legs++
	}
	return len(openMarkets), legs
}

func roundOutcomeCounts(history []RoundHistoryEntry) (wins, losses, flats int) {
	for _, entry := range history {
		if roundHistoryHasOpenInventory(entry) {
			flats++
			continue
		}
		switch {
		case entry.PnL > 0.0001:
			wins++
		case entry.PnL < -0.0001:
			losses++
		default:
			flats++
		}
	}
	return wins, losses, flats
}

func (m tuiModel) renderRoundHistory(w int, maxItems int) string {
	s := m.snap
	inner := w - 4
	var sb strings.Builder

	if len(s.roundHistory) == 0 {
		sb.WriteString(sectionHeader("🧮", "ROUND SNAPSHOTS", clrSlate) + "\n")
		sb.WriteString(styleDimmed.Render("  (no completed rounds yet)"))
		return makePanel(inner, clrSlate, sb.String())
	}

	wins, losses, flats := roundOutcomeCounts(s.roundHistory)

	sb.WriteString(sectionHeader("🧮", fmt.Sprintf("ROUND SNAPSHOTS  (W/L/F %d/%d/%d)", wins, losses, flats), clrSlate) + "\n")
	sb.WriteString(styleDimmed.Render("  historical round-close snapshots; current live round is not included until it closes") + "\n")
	carryMarkets, carryLegs := 0, 0
	for i := len(s.roundHistory) - 1; i >= 0; i-- {
		markets, legs := roundHistoryOpenInventoryCounts(s.roundHistory[i])
		if legs <= 0 {
			continue
		}
		carryMarkets, carryLegs = markets, legs
		break
	}
	liveMarkets, liveLegs := positionsWithPnLOpenInventoryCounts(s.positions)
	if carryLegs > 0 || liveLegs > 0 {
		sb.WriteString(styleDimmed.Render(
			fmt.Sprintf("  open carry (latest close): %d markets / %d legs  ·  live in-flight now: %d markets / %d legs",
				carryMarkets, carryLegs, liveMarkets, liveLegs),
		) + "\n")
		sb.WriteString(styleDimmed.Render("  OPEN rows show conservative book equity: spendable cash plus unresolved carry at cost basis") + "\n")
	}
	sb.WriteString(styleDimmed.Render(fmt.Sprintf("  %-4s  %-8s  %-10s  %-10s  %-11s  %-5s  %s",
		"#", "END", "START", "CLOSE EQ", "EQ DELTA", "TRDS", "RESULT")) + "\n")
	sb.WriteString(styleMuted.Render("  "+strings.Repeat("─", min(inner-2, 86))) + "\n")

	displayCount := len(s.roundHistory)
	if displayCount > maxItems {
		displayCount = maxItems
	}

	for i := len(s.roundHistory) - 1; i >= len(s.roundHistory)-displayCount && i >= 0; i-- {
		entry := s.roundHistory[i]
		openInventory := roundHistoryHasOpenInventory(entry)
		pnlStyle := styleDimmed
		resultLabel := "FLAT"
		resultStyle := styleDimmed
		pnlText := signedDollar(entry.PnL)
		if openInventory {
			pnlStyle = styleYellow
			resultLabel = "OPEN"
			resultStyle = styleYellow
		} else {
			switch {
			case entry.PnL > 0.0001:
				pnlStyle = styleGreen
				resultLabel = "WIN"
				resultStyle = styleGreen
			case entry.PnL < -0.0001:
				pnlStyle = styleRed
				resultLabel = "LOSS"
				resultStyle = styleRed
			}
		}

		sb.WriteString(fmt.Sprintf("  %-4d  %s  $%-9.2f  $%-9.2f  %s  %-5d  %s\n",
			entry.Number,
			styleDimmed.Render(entry.Timestamp.Format("15:04:05")),
			entry.StartingEquity,
			entry.EndingEquity,
			pnlStyle.Render(fmt.Sprintf("%11s", pnlText)),
			entry.Trades,
			resultStyle.Render(resultLabel),
		))
		if openInventory {
			carryCost := roundHistoryOpenCarryCost(entry)
			if carryCost > 0.000001 {
				estCash := entry.EndingEquity - carryCost
				sb.WriteString("        " + styleDimmed.Render(
					fmt.Sprintf("cash est $%.2f  ·  carry $%.2f", estCash, carryCost),
				) + "\n")
			}
		}
		displaySummary := roundHistoryDisplaySummary(s.roundHistory, i)
		if displaySummary != "" {
			summaryLines := wrapRoundHistoryDisplaySummary(displaySummary, max(12, inner-10))
			for idx, line := range summaryLines {
				if idx == 0 {
					sb.WriteString("        " + styleDimmed.Render("↳ ") + styleWhite.Render(line) + "\n")
				} else {
					sb.WriteString("          " + styleWhite.Render(line) + "\n")
				}
			}
		}
	}

	return makePanel(inner, clrSlate, sb.String())
}

// renderOrderHistory: recent trade log panel.
func (m tuiModel) renderOrderHistory(w int, maxItems int) string {
	s := m.snap
	inner := w - 4
	var sb strings.Builder

	if len(s.orderHistory) == 0 {
		sb.WriteString(sectionHeader("📋", "ORDER HISTORY", clrSlate) + "\n")
		sb.WriteString(styleDimmed.Render("  (no trades yet)"))
		return makePanel(inner, clrSlate, sb.String())
	}

	sb.WriteString(sectionHeader("📋", fmt.Sprintf("GLOBAL ORDER HISTORY  (last %d)", len(s.orderHistory)), clrSlate) + "\n")

	// Table header
	sb.WriteString(styleDimmed.Render(fmt.Sprintf("  %-8s  %-10s  %-6s  %-5s  %-9s  %-8s  %-8s  %s",
		"TIME", "MKT", "OUTC", "SIDE", "SHARES", "PRICE", "VALUE", "PROFIT/MARGIN")) + "\n")
	sb.WriteString(styleMuted.Render("  "+strings.Repeat("─", min(inner-2, 75))) + "\n")

	displayCount := len(s.orderHistory)
	if displayCount > maxItems {
		displayCount = maxItems
	}

	for i := len(s.orderHistory) - 1; i >= len(s.orderHistory)-displayCount && i >= 0; i-- {
		o := s.orderHistory[i]
		mode := strings.ToLower(strings.TrimSpace(o.ExecutionMode))
		modeLabel := ""
		switch mode {
		case "maker":
			modeLabel = "maker"
		case "copytrade":
			modeLabel = "copy"
		case "laddered-taker":
			modeLabel = "ladder"
		case "taker-close":
			modeLabel = "close"
		case "taker":
			modeLabel = "taker"
		case "wallet-sync":
			modeLabel = "sync"
		}

		statusIcon := "✅"
		marginSt := styleGreen
		switch o.Status {
		case "FAILED":
			statusIcon = "❌"
			marginSt = styleRed
		case "PARTIAL":
			statusIcon = "⚠️"
			marginSt = styleYellow
		case "SYNCED":
			statusIcon = "🧾"
			marginSt = styleYellow
		}

		var marginText string
		if o.Side == "SELL" {
			sg := ""
			if o.Profit > 0 {
				sg = "+"
			} else if o.Profit < 0 {
				sg = "-"
				marginSt = styleRed
			}
			if o.Margin == 0.0 {
				if modeLabel != "" {
					marginText = fmt.Sprintf("%s$%.2f (%s)", sg, math.Abs(o.Profit), modeLabel)
				} else {
					marginText = fmt.Sprintf("%s$%.2f", sg, math.Abs(o.Profit))
				}
			} else {
				marginText = fmt.Sprintf("%s$%.2f (%.1f%%)", sg, math.Abs(o.Profit), o.Margin)
			}
		} else {
			if modeLabel != "" {
				marginText = modeLabel
				marginSt = styleDimmed
			} else {
				marginText = "BUY"
				marginSt = styleDimmed
			}
		}

		displayValue := o.Cost
		if o.Shares > 0 && o.Price > 0 && (o.Side == "BUY" || o.Side == "SELL") {
			displayValue = o.Shares * o.Price
		}

		aStyle := getAssetStyle(o.MarketID)
		marketSlug := resolveOrderHistoryMarketSlug(o, s.markets)
		sb.WriteString(fmt.Sprintf("  %s  %s  %-6s  %-5s  %7.2f  $%-7.4f  $%-7.2f  %s  %s\n",
			styleDimmed.Render(o.Timestamp.Format("15:04:05")),
			aStyle.Render(fmt.Sprintf("%-21s", truncateText(orderHistoryMarketLabel(o.MarketID, marketSlug), 21))),
			core.SanitizeString(o.Outcome),
			o.Side,
			o.Shares,
			o.Price,
			displayValue,
			statusIcon,
			marginSt.Render(marginText),
		))
	}

	return makePanel(inner, clrSlate, sb.String())
}

// renderEventLog: scrolling event log panel.
func (m tuiModel) renderEventLog(w int, maxItems int) string {
	s := m.snap
	inner := w - 4
	var sb strings.Builder

	visible := min(len(s.eventLog), maxItems)
	label := "EVENTS"
	if len(s.eventLog) > 0 {
		label = fmt.Sprintf("EVENTS  (%d/%d)", visible, len(s.eventLog))
	}
	sb.WriteString(sectionHeader("📜", label, clrSlate) + "\n")
	if len(s.eventLog) == 0 {
		sb.WriteString(styleDimmed.Render("  (waiting for events…)"))
	} else {
		startIdx := 0
		if len(s.eventLog) > maxItems {
			startIdx = len(s.eventLog) - maxItems
		}
		maxLineWidth := inner - 2
		if maxLineWidth < 8 {
			maxLineWidth = 8
		}

		for i := startIdx; i < len(s.eventLog); i++ {
			sb.WriteString("  " + truncateText(s.eventLog[i], maxLineWidth) + "\n")
		}
	}
	return makePanel(inner, clrSlate, sb.String())
}

// renderFooter: slim status bar.
func (m tuiModel) renderFooter(w int, scrollOffset, maxOffset int) string {
	mode := m.snap.mode
	if mode == "" {
		mode = "Paper"
	}

	modeText := mode + " Trading Mode"
	scrollText := "Top"
	if maxOffset > 0 {
		scrollText = fmt.Sprintf("Scroll %d/%d", scrollOffset, maxOffset)
	}
	timeframe := normalizeMarketTimeframe(m.snap.settings.Timeframe)
	leftText := "  Polyarb-" + timeframe + "  ·  " + modeText + "  ·  " + scrollText
	pauseLabel := " LIVE "
	pauseStyle := lipgloss.NewStyle().Foreground(clrEmerald).Bold(true)
	pauseAction := "pause"
	if m.snap.manualTradingPause {
		pauseLabel = " PAUSED "
		pauseStyle = styleBgRedBold
		pauseAction = "resume"
	}
	rightText := fmt.Sprintf("[P] %s  [↑↓/jk] scroll  [PgUp/PgDn] page  [g/G] top/btm  [q] quit  ", pauseAction)
	if w < 120 {
		rightText = fmt.Sprintf("[P] %s  [↑↓/jk] scroll  [PgUp/PgDn] page  [q] quit  ", pauseAction)
	}
	if w < 92 {
		rightText = fmt.Sprintf("[P] %s  [↑↓] scroll  [q] quit  ", pauseAction)
	}
	left := styleMuted.Render(leftText) + "  " + pauseStyle.Render(pauseLabel)
	right := styleMuted.Render(rightText)
	leftLen := len(leftText) + 2 + len(pauseLabel)
	rightLen := len(rightText)
	gap := w - 2 - leftLen - rightLen
	if gap < 1 {
		gap = 1
	}
	line := left + strings.Repeat(" ", gap) + right
	return lipgloss.NewStyle().
		Border(lipgloss.NormalBorder(), false, false, false, false).
		Foreground(clrSlate).
		Render(line)
}

// renderKillBanner: full-width red alert overlay.
func (m tuiModel) renderKillBanner(w int) string {
	s := m.snap
	pad := func(n int) string {
		if n < 0 {
			n = 0
		}
		return strings.Repeat(" ", n)
	}
	bannerInner := w - 4
	if bannerInner < 10 {
		bannerInner = 10
	}

	l1 := styleBgRedBold.Width(bannerInner).Render(pad(bannerInner))
	l2 := styleBgRedBold.Width(bannerInner).Render("🚨  KILL SWITCH ACTIVATED  🚨")
	reason := "Reason: " + s.killReason
	if len(reason) > bannerInner-4 {
		reason = reason[:bannerInner-4]
	}
	l3 := styleBgRedBold.Width(bannerInner).Render(reason)
	l4 := styleBgRedBold.Width(bannerInner).Render(pad(bannerInner))

	return makePanel(bannerInner, clrRose, l1+"\n"+l2+"\n"+l3+"\n"+l4)
}

// renderSettings: full-screen settings overlay.
//
//	╭─ ⚙  SETTINGS ──────────────────────────────────────────────────╮
//	│  [↑↓/jk] Navigate  [←→/+-] Adjust  [1/2/3] Presets  [s] Close │
//	│                                                                  │
//	│  > Trade Scale Factor  [ 5.0%]  ████████░░░░░░░░░░░░  5%       │
//	│    Min Margin %        [ 2.0%]                                  │
//	│    Split Min Margin    [ 3.0%]                                  │
//	│    Split Strategy      [  ON ]                                  │
//	│                                                                  │
//	│  ─── Presets ─────────────────────────────────────────────────  │
//	│  [1] Conservative  scale=1%  margin=3%  (low risk, $1/trade)   │
//	│  [2] Moderate      scale=5%  margin=2%  (balanced)             │
//	│  [3] Aggressive    scale=10% margin=1%  (max frequency)        │
//	╰──────────────────────────────────────────────────────────────────╯
func (m tuiModel) renderSettings(w int) string {
	inner := w - 4
	if inner < 40 {
		inner = 40
	}

	// Read current settings (under lock for safety)
	m.tui.mu.Lock()
	cfg := m.tui.settings
	mode := m.tui.mode
	m.tui.mu.Unlock()

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(clrBrand)
	title := titleStyle.Render("⚙  LIVE SETTINGS")

	keysLine := styleDimmed.Render("  [↑↓/jk] Navigate  [←→/+-] Adjust  [PgUp/PgDn] Scroll  [1/2/3] Presets  [r/s] Apply  [Esc] Dismiss")
	if m.settingsEdit {
		keysLine = styleDimmed.Render("  Type value  [Enter] Save  [Esc] Cancel  [Ctrl+U] Clear")
	} else if isCopytradeSettingsMode(cfg) {
		keysLine = styleDimmed.Render("  [↑↓/jk] Navigate  [←→/+-] Adjust  [PgUp/PgDn] Scroll  [Enter] Paste Target  [1/2/3] Presets  [r/s] Apply  [Esc] Dismiss")
	}

	divider := styleMuted.Render("  " + strings.Repeat("─", min(inner-2, 60)))

	type row struct {
		idx      int
		label    string
		value    string
		bar      string
		disabled bool
	}

	fmtPct := func(v float64) string { return fmt.Sprintf("%5.1f%%", v*100) }

	makerMode := isMakerSettingsMode(cfg)
	copytradeMode := isCopytradeSettingsMode(cfg)
	ladderedMode := isLadderedTakerSettingsMode(cfg)
	tradeSizeBarMax := 50.0
	if cfg.MaxTradeSize > 0 {
		tradeSizeBarMax = cfg.MaxTradeSize
	}
	if tradeSizeBarMax < 0.1 {
		tradeSizeBarMax = 0.1
	}
	rows := []row{
		{
			label: "Market",
			value: fmt.Sprintf(" %s ", cfg.MarketSlug),
			bar:   "",
		},
		{
			label: "Max Concurrent",
			value: fmt.Sprintf(" %d ", cfg.MaxMarkets),
			bar:   renderBar(float64(cfg.MaxMarkets)/4.0, 20),
		},
		{
			label: settingsRowLabel(cfg, settingsRowPaperBalance),
			value: func() string {
				if m.settingsEdit && m.settingsCursor == settingsRowPaperBalance {
					return styleCyan.Render(fmt.Sprintf(" %s _ ", m.settingsInput))
				}
				return fmt.Sprintf(" $%.2f ", cfg.PaperBalance)
			}(),
			bar: renderBar(cfg.PaperBalance/1000.0, 20),
		},
		{
			label: "Timeframe",
			value: fmt.Sprintf(" %s ", cfg.Timeframe),
			bar:   "",
		},
		{
			label: settingsRowLabel(cfg, settingsRowTradeSizingMode),
			value: func() string {
				if copytradeMode {
					if strings.EqualFold(cfg.CopytradeSizingMode, core.CopytradeSizingModeShares) {
						return styleCyan.Render(" SHARES ")
					}
					if strings.EqualFold(cfg.CopytradeSizingMode, core.CopytradeSizingModePercent) {
						return styleYellow.Render(" MASTER% ")
					}
					return styleGreen.Render(" USDC ")
				}
				if ladderedMode {
					if isLadderedTakerShareSizingMode(cfg) {
						return styleCyan.Render(" SHARES ")
					}
					return styleGreen.Render(" USDC ")
				}
				if strings.EqualFold(cfg.TradeSizingMode, core.TradeSizingModeUSDC) {
					return styleGreen.Render(" USDC ")
				}
				return styleYellow.Render("   %  ")
			}(),
			bar: "",
		},
		{
			label: settingsRowLabel(cfg, settingsRowTradeSizingValue),
			value: func() string {
				if m.settingsEdit && m.settingsCursor == settingsRowTradeSizingValue {
					return styleCyan.Render(fmt.Sprintf(" %s _ ", m.settingsInput))
				}
				if copytradeMode {
					if strings.EqualFold(cfg.CopytradeSizingMode, core.CopytradeSizingModeShares) {
						return fmt.Sprintf(" %s sh ", fmtFloatTrim(cfg.CopytradeSizeShares, 2))
					}
					if strings.EqualFold(cfg.CopytradeSizingMode, core.CopytradeSizingModePercent) {
						return fmt.Sprintf(" %s%% ", fmtFloatTrim(cfg.CopytradeSizePercent, 1))
					}
					return fmt.Sprintf(" $%.2f ", cfg.CopytradeSizeUSDC)
				}
				if ladderedMode {
					if isLadderedTakerShareSizingMode(cfg) {
						return fmt.Sprintf(" %s sh ", fmtFloatTrim(cfg.LadderedTakerSizeShares, 2))
					}
					return fmt.Sprintf(" $%.2f ", cfg.LadderedTakerSizeUSDC)
				}
				if strings.EqualFold(cfg.TradeSizingMode, core.TradeSizingModeUSDC) {
					return fmt.Sprintf(" $%.2f ", cfg.TradeSizeUSDC)
				}
				return fmtPct(cfg.TradeScaleFactor)
			}(),
			bar: func() string {
				if copytradeMode {
					if strings.EqualFold(cfg.CopytradeSizingMode, core.CopytradeSizingModeShares) {
						return renderBar(cfg.CopytradeSizeShares/25.0, 20)
					}
					if strings.EqualFold(cfg.CopytradeSizingMode, core.CopytradeSizingModePercent) {
						return renderBar(cfg.CopytradeSizePercent/100.0, 20)
					}
					return renderBar(cfg.CopytradeSizeUSDC/tradeSizeBarMax, 20)
				}
				if ladderedMode {
					if isLadderedTakerShareSizingMode(cfg) {
						return renderBar(cfg.LadderedTakerSizeShares/25.0, 20)
					}
					return renderBar(cfg.LadderedTakerSizeUSDC/tradeSizeBarMax, 20)
				}
				if strings.EqualFold(cfg.TradeSizingMode, core.TradeSizingModeUSDC) {
					return renderBar(cfg.TradeSizeUSDC/tradeSizeBarMax, 20)
				}
				return renderBar(cfg.TradeScaleFactor, 20)
			}(),
		},
		{
			label: settingsRowLabel(cfg, settingsRowLadderCooldown),
			value: func() string {
				if m.settingsEdit && m.settingsCursor == settingsRowLadderCooldown {
					return styleCyan.Render(fmt.Sprintf(" %s _ ", m.settingsInput))
				}
				return fmt.Sprintf(" %4.1fc ", cfg.LadderedTakerReentryMoveCents)
			}(),
			bar: renderBar(cfg.LadderedTakerReentryMoveCents/5.0, 20),
		},
		{
			label: settingsRowLabel(cfg, settingsRowLadderSlippage),
			value: fmt.Sprintf(" %4.0fc ", cfg.LadderedTakerMaxSlippagePct),
			bar:   renderBar(cfg.LadderedTakerMaxSlippagePct/99.0, 20),
		},
		{
			label: settingsRowLabel(cfg, settingsRowMinMargin),
			value: func() string {
				if m.settingsEdit && m.settingsCursor == settingsRowMinMargin {
					return styleCyan.Render(fmt.Sprintf(" %s _ ", m.settingsInput))
				}
				return fmt.Sprintf("%5.1f%%", cfg.MinMarginPercent)
			}(),
			bar: renderBar(cfg.MinMarginPercent/20.0, 20),
		},
		{
			label: settingsRowLabel(cfg, settingsRowBinanceExecutionDelay), value: func() string {
				if m.settingsEdit && m.settingsCursor == settingsRowBinanceExecutionDelay {
					return styleCyan.Render(fmt.Sprintf(" %s _ ", m.settingsInput))
				}
				return fmt.Sprintf(" %dms ", cfg.PaperBinanceExecutionDelayMs)
			}(),
			bar: renderBar(float64(cfg.PaperBinanceExecutionDelayMs)/1000.0, 20),
		},
		{
			label: "Paper Arb Mode",
			value: func() string {
				if strings.EqualFold(cfg.PaperArbMode, "maker") {
					return styleGreen.Render(" maker ")
				}
				if strings.EqualFold(cfg.PaperArbMode, "copytrade") {
					return styleCyan.Render(" copytrade ")
				}
				if strings.EqualFold(cfg.PaperArbMode, "laddered-taker") {
					return styleCyan.Render(" ladder ")
				}
				if strings.EqualFold(cfg.PaperArbMode, "binance-gap") {
					return styleCyan.Render(" binance ")
				}
				return styleYellow.Render(" taker ")
			}(),
			bar: "",
		},
		{
			label: settingsRowLabel(cfg, settingsRowCopytradeTarget),
			value: renderCopytradeTargetValue(
				cfg.CopytradeTarget,
				m.settingsEdit && m.settingsCursor == settingsRowCopytradeTarget,
				m.settingsInput,
			),
			bar: "",
		},
		{
			label: settingsRowLabel(cfg, settingsRowCopytradePoll),
			value: func() string {
				if m.settingsEdit && m.settingsCursor == settingsRowCopytradePoll {
					return styleCyan.Render(fmt.Sprintf(" %s _ ", m.settingsInput))
				}
				return fmt.Sprintf(" %4dms ", cfg.CopytradePollIntervalMs)
			}(),
			bar: renderBar(float64(cfg.CopytradePollIntervalMs)/5000.0, 20),
		},
		{
			label: settingsRowLabel(cfg, settingsRowExecutionSlip),
			value: func() string {
				if isCopytradeSettingsMode(cfg) {
					return fmt.Sprintf(" %4.0fc ", cfg.CopytradeMaxSlippagePct)
				}
				return fmt.Sprintf(" %5.1f%% ", executionFloorDisplayPercent(cfg.BuyExecutionMarginFloorPercent))
			}(),
			bar: func() string {
				if isCopytradeSettingsMode(cfg) {
					return renderBar(cfg.CopytradeMaxSlippagePct/99.0, 20)
				}
				return renderBar(math.Abs(normalizeExecutionFloorSetting(cfg.BuyExecutionMarginFloorPercent))/0.10, 20)
			}(),
		},
		{
			label: settingsRowLabel(cfg, settingsRowSplitMinMargin),
			value: func() string {
				if m.settingsEdit && m.settingsCursor == settingsRowSplitMinMargin {
					return styleCyan.Render(fmt.Sprintf(" %s _ ", m.settingsInput))
				}
				return fmt.Sprintf("%5.1f%%", cfg.SplitMinMarginSell)
			}(),
			bar: renderBar(cfg.SplitMinMarginSell/20.0, 20),
		},
		{
			label: settingsRowLabel(cfg, settingsRowSplitStrategy),
			value: func() string {
				if cfg.SplitStrategyEnabled {
					return styleGreen.Render("  ON ")
				}
				return styleMuted.Render(" OFF ")
			}(),
			bar: "",
		},
		{
			label: settingsRowLabel(cfg, settingsRowSplitInitialCap),
			value: func() string {
				if m.settingsEdit && m.settingsCursor == settingsRowSplitInitialCap {
					return styleCyan.Render(fmt.Sprintf(" %s _ ", m.settingsInput))
				}
				return fmtPct(cfg.SplitInitialCapPct)
			}(),
			bar: renderBar(cfg.SplitInitialCapPct, 20),
		},
		{
			label: settingsRowLabel(cfg, settingsRowSplitReplenishCap),
			value: func() string {
				if m.settingsEdit && m.settingsCursor == settingsRowSplitReplenishCap {
					return styleCyan.Render(fmt.Sprintf(" %s _ ", m.settingsInput))
				}
				return fmtPct(cfg.SplitReplenishCapPct)
			}(),
			bar: renderBar(cfg.SplitReplenishCapPct, 20),
		},
		{
			label: settingsRowLabel(cfg, settingsRowTakerCloseMarket),
			value: func() string {
				if cfg.TakerCloseMarket {
					return styleGreen.Render("  ON ")
				}
				return styleMuted.Render(" OFF ")
			}(),
			bar: "",
		},
		{
			label: settingsRowLabel(cfg, settingsRowBlockPendingRedemption),
			value: func() string {
				if cfg.BlockNewEntriesOnPendingRedemption {
					return styleGreen.Render("  ON ")
				}
				return styleMuted.Render(" OFF ")
			}(),
			bar: "",
		},
		{
			label: settingsRowLabel(cfg, settingsRowRedeemEntryTiming),
			value: func() string {
				if normalizeRedeemEntryTiming(cfg.RedeemEntryTiming) == core.RedeemEntryTimingImmediate {
					return styleGreen.Render(" IMMEDIATE ")
				}
				return styleYellow.Render(" NEXT FULL ")
			}(),
			bar: "",
		},
		{
			label: settingsRowLabel(cfg, settingsRowRedeemGasMode),
			value: func() string {
				switch normalizeRedeemGasMode(cfg.RedeemGasMode) {
				case core.RedeemGasModeNormal:
					return styleGreen.Render(" normal ")
				case core.RedeemGasModeUrgent:
					return styleRed.Render(" urgent ")
				default:
					return styleYellow.Render(" fast ")
				}
			}(),
			bar: "",
		},
		{
			label: settingsRowLabel(cfg, settingsRowMinAskPrice),
			value: fmt.Sprintf(" $%.2f ", cfg.MinAskPrice),
			bar:   renderBar(cfg.MinAskPrice, 20),
		},
		{
			label: settingsRowLabel(cfg, settingsRowMaxAskPrice),
			value: fmt.Sprintf(" $%.2f ", cfg.MaxAskPrice),
			bar:   renderBar(cfg.MaxAskPrice, 20),
		},
		{
			label: settingsRowLabel(cfg, settingsRowMakerMergeBuffer),
			value: func() string {
				if m.settingsEdit && m.settingsCursor == settingsRowMakerMergeBuffer {
					return styleCyan.Render(fmt.Sprintf(" %s _ ", m.settingsInput))
				}
				return fmt.Sprintf(" %3ds ", cfg.MakerMergeBufferSeconds)
			}(),
			bar: renderBar(float64(cfg.MakerMergeBufferSeconds)/120.0, 20),
		},
		{
			label: settingsRowLabel(cfg, settingsRowMakerQuoteGap),
			value: func() string {
				if m.settingsEdit && m.settingsCursor == settingsRowMakerQuoteGap {
					return styleCyan.Render(fmt.Sprintf(" %s _ ", m.settingsInput))
				}
				return fmt.Sprintf(" $%.3f ", cfg.MakerQuoteGap)
			}(),
			bar: renderBar(cfg.MakerQuoteGap/0.05, 20),
		},
		{
			label: settingsRowLabel(cfg, settingsRowMakerTargetMult),
			value: func() string {
				if m.settingsEdit && m.settingsCursor == settingsRowMakerTargetMult {
					return styleCyan.Render(fmt.Sprintf(" %s _ ", m.settingsInput))
				}
				return fmt.Sprintf(" %.1fx ", cfg.MakerInventoryTargetMult)
			}(),
			bar: renderBar(cfg.MakerInventoryTargetMult/10.0, 20),
		},
		{
			label: settingsRowLabel(cfg, settingsRowMakerCapMult),
			value: func() string {
				if m.settingsEdit && m.settingsCursor == settingsRowMakerCapMult {
					return styleCyan.Render(fmt.Sprintf(" %s _ ", m.settingsInput))
				}
				return fmt.Sprintf(" %.1fx ", cfg.MakerInventoryCapMult)
			}(),
			bar: renderBar(cfg.MakerInventoryCapMult/20.0, 20),
		},
		{
			label: settingsRowLabel(cfg, settingsRowMakerMinQuoteValue),
			value: func() string {
				if m.settingsEdit && m.settingsCursor == settingsRowMakerMinQuoteValue {
					return styleCyan.Render(fmt.Sprintf(" %s _ ", m.settingsInput))
				}
				return fmt.Sprintf(" $%.1f ", cfg.MakerMinQuoteValue)
			}(),
			bar: renderBar(cfg.MakerMinQuoteValue/50.0, 20),
		},
		{
			label: settingsRowLabel(cfg, settingsRowMaxTradeSize),
			value: func() string {
				if m.settingsEdit && m.settingsCursor == settingsRowMaxTradeSize {
					return styleCyan.Render(fmt.Sprintf(" %s _ ", m.settingsInput))
				}
				if cfg.MaxTradeSize <= 0 {
					return styleMuted.Render(" disabled ")
				}
				return fmt.Sprintf(" $%.2f ", cfg.MaxTradeSize)
			}(),
			bar: func() string {
				if cfg.MaxTradeSize <= 0 {
					return ""
				}
				return renderBar(cfg.MaxTradeSize/1000.0, 20)
			}(),
		},
		{
			label: settingsRowLabel(cfg, settingsRowMaxDailyLoss),
			value: func() string {
				if m.settingsEdit && m.settingsCursor == settingsRowMaxDailyLoss {
					return styleCyan.Render(fmt.Sprintf(" %s _ ", m.settingsInput))
				}
				if cfg.MaxDailyLoss <= 0 {
					return styleMuted.Render(" disabled ")
				}
				return fmt.Sprintf(" $%.2f ", cfg.MaxDailyLoss)
			}(),
			bar: func() string {
				if cfg.MaxDailyLoss <= 0 {
					return ""
				}
				return renderBar(cfg.MaxDailyLoss/5000.0, 20)
			}(),
		},
		{
			label: settingsRowLabel(cfg, settingsRowExchange),
			value: func() string {
				if cfg.Exchange == "kalshi" {
					return styleGreen.Render(" kalshi ")
				}
				return styleYellow.Render(" polymarket ")
			}(),
			bar: "",
		},
		{
			label: settingsRowLabel(cfg, settingsRowExecutionBackend),
			value: func() string {
				if strings.EqualFold(cfg.ExecutionBackend, core.ExecutionBackendLive) {
					return styleRed.Render(" live ")
				}
				return styleGreen.Render(" paper ")
			}(),
			bar: "",
		},
		{
			label: settingsRowLabel(cfg, settingsRowTakerCloseTime),
			value: fmt.Sprintf(" %ds ", cfg.TakerCloseMarketTime),
			bar:   renderBar(float64(cfg.TakerCloseMarketTime)/60.0, 20),
		},
		{
			label: settingsRowLabel(cfg, settingsRowTakerCloseSlippage),
			value: fmt.Sprintf(" $%.2f ", cfg.TakerCloseMarketSlippage),
			bar:   renderBar(cfg.TakerCloseMarketSlippage, 20),
		},
		{
			label: settingsRowLabel(cfg, settingsRowTakerCloseMinPrice),
			value: fmt.Sprintf(" $%.2f ", cfg.TakerCloseMarketMinPrice),
			bar:   renderBar(cfg.TakerCloseMarketMinPrice, 20),
		},
		{
			label: settingsRowLabel(cfg, settingsRowTradingHoursMode),
			value: func() string {
				if cfg.TradingHoursMode == "weekdays trade only" {
					return styleGreen.Render(" WEEKDAYS ")
				} else if cfg.TradingHoursMode == "us open only" {
					return styleYellow.Render(" US OPEN ")
				}
				return styleMuted.Render(" OFF ")
			}(),
			bar: "",
		},
		{
			idx:   settingsRowRPCEdit,
			label: settingsRowLabel(cfg, settingsRowRPCEdit),
			value: renderStringValue(
				cfg.PolygonRPC,
				m.settingsEdit && m.settingsCursor == settingsRowRPCEdit,
				m.settingsInput,
				"paste RPC URL",
			),
			bar: "",
		},
		{
			idx:   settingsRowPrivateKeyEdit,
			label: settingsRowLabel(cfg, settingsRowPrivateKeyEdit),
			value: func() string {
				if m.settingsEdit && m.settingsCursor == settingsRowPrivateKeyEdit {
					value := strings.TrimSpace(m.settingsInput)
					if value == "" {
						value = "paste Private Key"
					} else {
						// Mask during editing if it's long enough, else show typing
						if len(value) > 4 {
							value = "..." + value[len(value)-4:]
						}
					}
					return styleCyan.Render(" " + value + " _ ")
				}
				if cfg.PolygonPrivateKey == "" {
					return styleMuted.Render(" paste Private Key ")
				}
				return styleCyan.Render(" ******************************** ")
			}(),
			bar: "",
		},
	}

	cursorStyle := lipgloss.NewStyle().Bold(true).Foreground(clrBrand)
	labelStyle := lipgloss.NewStyle().Foreground(clrDim)
	valueStyle := lipgloss.NewStyle().Bold(true).Foreground(clrWhite)

	var rowLines []string
	for i, r := range rows {
		if !isRowVisible(cfg, mode, i) {
			continue
		}
		cursor := "  "

		lSt := labelStyle
		vSt := valueStyle
		if i == m.settingsCursor {
			cursor = cursorStyle.Render("> ")
			if r.disabled {
				lSt = lipgloss.NewStyle().Bold(true).Foreground(clrDim)
				vSt = lipgloss.NewStyle().Bold(true).Foreground(clrDim)
			} else {
				lSt = lipgloss.NewStyle().Bold(true).Foreground(clrBrand)
				vSt = lipgloss.NewStyle().Bold(true).Foreground(clrWhite)
			}
		} else if r.disabled {
			lSt = lipgloss.NewStyle().Foreground(clrDim)
			vSt = lipgloss.NewStyle().Foreground(clrDim)
		}

		// Use fixed-width padding to perfectly align the bars
		valStr := r.value
		// Strip ANSI codes if any for length calculation (basic approach, though lipgloss can help)
		visibleLen := lipgloss.Width(valStr)
		padLen := 10 - visibleLen
		if padLen < 0 {
			padLen = 0
		}

		valPad := valStr + strings.Repeat(" ", padLen)
		val := "[" + vSt.Render(valPad) + "]"

		// Properly pad the label ignoring ANSI codes, and increase width to 25 to avoid being too close
		labelLen := lipgloss.Width(r.label)
		labelPadLen := 25 - labelLen
		if labelPadLen < 0 {
			labelPadLen = 0
		}
		renderedLabel := lSt.Render(r.label) + strings.Repeat(" ", labelPadLen)

		line := fmt.Sprintf("%s%s  %s  %s",
			cursor,
			renderedLabel,
			val,
			r.bar,
		)
		rowLines = append(rowLines, line)
	}

	// Preset descriptions
	presetDivider := styleMuted.Render("  " + strings.Repeat("─", min(inner-2, 60)))
	presetTitle := styleDimmed.Render("  Quick Presets:")
	p1 := fmt.Sprintf("  %s Conservative  scale=1%%   margin=3%%  (%s)",
		lipgloss.NewStyle().Foreground(clrAmber).Render("[1]"),
		styleDimmed.Render("$1/trade on $100 balance"))
	p2 := fmt.Sprintf("  %s Moderate      scale=5%%   margin=2%%  (%s)",
		lipgloss.NewStyle().Foreground(clrTeal).Render("[2]"),
		styleDimmed.Render("$5/trade on $100 balance"))
	p3 := fmt.Sprintf("  %s Aggressive    scale=10%%  margin=1%%  (%s)",
		lipgloss.NewStyle().Foreground(clrEmerald).Render("[3]"),
		styleDimmed.Render("$10/trade on $100 balance"))

	// Trade size preview
	balanceNote := styleDimmed.Render(fmt.Sprintf(
		"  At $100 balance → $%.0f/trade  ·  At $500 → $%.0f/trade",
		100*cfg.TradeScaleFactor,
		500*cfg.TradeScaleFactor,
	))
	if strings.EqualFold(cfg.TradeSizingMode, core.TradeSizingModeUSDC) {
		balanceNote = styleDimmed.Render(fmt.Sprintf(
			"  Fixed sizing active → $%.2f/trade  ·  balance changes do not rescale entries",
			cfg.TradeSizeUSDC,
		))
	}
	modeNote := ""
	if makerMode {
		modeNote = styleDimmed.Render("  Maker mode: split/taker-only rows are shown for reference and ignored live") + "\n"
	} else if copytradeMode {
		if strings.EqualFold(cfg.CopytradeSizingMode, core.CopytradeSizingModeShares) {
			balanceNote = styleDimmed.Render(fmt.Sprintf(
				"  Copytrade share sizing active → max %s shares per entry",
				fmtFloatTrim(cfg.CopytradeSizeShares, 2),
			))
		} else if strings.EqualFold(cfg.CopytradeSizingMode, core.CopytradeSizingModePercent) {
			balanceNote = styleDimmed.Render(fmt.Sprintf(
				"  Copytrade percent sizing active → follow %s%% of each master trade",
				fmtFloatTrim(cfg.CopytradeSizePercent, 1),
			))
		} else {
			balanceNote = styleDimmed.Render(fmt.Sprintf(
				"  Copytrade USDC sizing active → max $%.2f per entry",
				cfg.CopytradeSizeUSDC,
			))
		}
		modeNote = styleDimmed.Render("  Copytrade mode: buy when the target wallet/profile holds an outcome, sell when it exits. Enter a wallet, @handle, or profile URL on the target row.") + "\n"
	} else if ladderedMode {
		if isLadderedTakerShareSizingMode(cfg) {
			balanceNote = styleDimmed.Render(fmt.Sprintf(
				"  Laddered taker share sizing active → buy paired slices of %s shares per entry",
				fmtFloatTrim(cfg.LadderedTakerSizeShares, 2),
			))
		} else {
			balanceNote = styleDimmed.Render(fmt.Sprintf(
				"  Laddered taker USDC sizing active → buy paired slices up to $%.2f per entry",
				cfg.LadderedTakerSizeUSDC,
			))
		}
		modeNote = styleDimmed.Render("  Laddered taker mode accumulates paired taker inventory in small slices and leaves it for later cleanup/merge instead of instant merge.") + "\n"
	}
	balanceResetNote := ""
	if strings.EqualFold(mode, "Paper") || strings.EqualFold(cfg.ExecutionBackend, core.ExecutionBackendPaper) {
		balanceResetNote = styleDimmed.Render("  Paper Balance updates available paper USDC. When flat it resets the session bankroll; with open inventory it applies as a neutral cash sync.") + "\n"
	}

	contentLines := []string{
		title,
		keysLine,
		"",
	}
	contentLines = append(contentLines, rowLines...)
	contentLines = append(contentLines,
		"",
		presetDivider,
		presetTitle,
		p1,
		p2,
		p3,
		"",
		divider,
	)
	if modeNote != "" {
		contentLines = append(contentLines, strings.TrimRight(modeNote, "\n"))
	}
	if balanceResetNote != "" {
		contentLines = append(contentLines, strings.TrimRight(balanceResetNote, "\n"))
	}
	contentLines = append(contentLines, balanceNote)

	visibleLines, _, _ := viewportLines(contentLines, m.scrollOffset, m.settingsViewportHeight())
	content := strings.Join(visibleLines, "\n")

	return makePanel(inner, clrBrand, content)
}

// ─── Settings Public API ──────────────────────────────────────────────────────

// GetSettings returns a snapshot of the current runtime settings.
// The trading loop should call this every iteration to pick up live changes.
func (t *TUI) GetSettings() TUISettings {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.settings
}

// InitSettings seeds the settings panel with values from config (e.g., from .env).
// Call this once after NewTUI and before StartRenderLoop.
func (t *TUI) InitSettings(s TUISettings, onChange func(TUISettings)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s = normalizeTUISettingsForContext(s, t.mode)
	if s.MakerQuoteGap <= 0 {
		s.MakerQuoteGap = 0.008
	}
	t.settings = s
	t.onSettingsChange = onChange
	// Keep tradeFactor in sync so the account panel shows the right value.
	if s.TradeScaleFactor > 0 {
		t.tradeFactor = s.TradeScaleFactor
	}
}

func outcomeLess(a, b string) bool {
	aStr := strings.ToLower(strings.TrimSpace(a))
	bStr := strings.ToLower(strings.TrimSpace(b))
	rank := func(name string) int {
		switch name {
		case "up":
			return 0
		case "down":
			return 1
		case "yes":
			return 2
		case "no":
			return 3
		default:
			return 4
		}
	}
	if rank(aStr) != rank(bStr) {
		return rank(aStr) < rank(bStr)
	}
	return aStr < bStr
}
