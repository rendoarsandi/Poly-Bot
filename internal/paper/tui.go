package paper

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
	defaultMaxOrderHistory  = 20
	defaultMaxEventHistory  = 250
	defaultTwoColOrderRows  = 8
	defaultOneColOrderRows  = 6
	defaultTwoColEventRows  = 18
	defaultOneColEventRows  = 14
	recentQuoteDisplayGrace = 1500 * time.Millisecond
)

// ─── Asset style helpers ──────────────────────────────────────────────────────

func getAssetStyle(id string) lipgloss.Style {
	switch id {
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

func recentDisplayQuote(current, lastGood float64, age time.Duration) float64 {
	if current > 0 {
		return current
	}
	if lastGood > 0 && age <= recentQuoteDisplayGrace {
		return lastGood
	}
	return current
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
	Slug       string
	Outcomes   []string
	EndTime    time.Time
	Bids       map[string]float64
	Asks       map[string]float64
	RealBids   map[string]float64
	RealAsks   map[string]float64
	LastUpdate time.Time
	DataSource string // "WS" or "REST"
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
	Timestamp time.Time
	MarketID  string
	Outcome   string
	Side      string
	Shares    float64
	Price     float64
	Cost      float64
	Margin    float64
	Profit    float64
	Status    string // "FILLED", "PARTIAL", "FAILED"
}

// TUISettings holds runtime-adjustable trading parameters.
// These can be changed live from the settings panel (press 's').
type TUISettings struct {
	MarketSlug                     string  // Current selected market slug or ALL or BTC,ETH
	MaxMarkets                     int     // Max concurrent markets to trade
	Timeframe                      string  // "5m" or "15m"
	TradeScaleFactor               float64 // e.g. 0.05 = 5% of equity per trade
	MinMarginPercent               float64 // e.g. 2.0 = require 2% arb margin
	PaperArbMode                   string  // taker or maker
	BuyExecutionMarginFloorPercent float64 // e.g. -1.0 = allow buy/sell execution to slip to -1% pair margin
	SplitMinMarginSell             float64 // e.g. 3.0 = sell splits at 3% margin
	SplitStrategyEnabled           bool    // toggle split strategy on/off
	SplitInitialCapPct             float64 // Initial Split Cap percentage
	SplitReplenishCapPct           float64 // Replenishment Cap percentage
	MakerMergeBufferSeconds        int     // seconds before expiry to merge paired maker inventory
	MakerQuoteGap                  float64 // distance from mid for maker quotes
	MakerInventoryTargetMult       float64
	MakerInventoryCapMult          float64
	MakerMinQuoteShares            float64
	MinAskPrice                    float64 // e.g. 0.10 = minimum ask price filter
	MaxAskPrice                    float64 // e.g. 0.90 = maximum ask price filter
	MaxTradeSize                   float64 // e.g. 50.00 = max trade size $50
	MaxDailyLoss                   float64 // e.g. 0.0 = disabled, else max drawdown limit
}

// Preset quick-select settings.
var (
	SettingsConservative = TUISettings{MarketSlug: "ALL", MaxMarkets: 2, Timeframe: "15m", TradeScaleFactor: 0.01, MinMarginPercent: 3.0, PaperArbMode: "taker", BuyExecutionMarginFloorPercent: -1.0, SplitMinMarginSell: 5.0, MakerMergeBufferSeconds: 30, MakerQuoteGap: 0.008, MakerInventoryTargetMult: 3.0, MakerInventoryCapMult: 5.0, MakerMinQuoteShares: 10.0, MinAskPrice: 0.10, MaxAskPrice: 0.90}
	SettingsModerate     = TUISettings{MarketSlug: "ALL", MaxMarkets: 4, Timeframe: "15m", TradeScaleFactor: 0.05, MinMarginPercent: 2.0, PaperArbMode: "taker", BuyExecutionMarginFloorPercent: -1.0, SplitMinMarginSell: 3.0, MakerMergeBufferSeconds: 30, MakerQuoteGap: 0.008, MakerInventoryTargetMult: 3.0, MakerInventoryCapMult: 5.0, MakerMinQuoteShares: 10.0, MinAskPrice: 0.10, MaxAskPrice: 0.90}
	SettingsAggressive   = TUISettings{MarketSlug: "ALL", MaxMarkets: 4, Timeframe: "15m", TradeScaleFactor: 0.10, MinMarginPercent: 1.0, PaperArbMode: "taker", BuyExecutionMarginFloorPercent: -1.0, SplitMinMarginSell: 2.0, MakerMergeBufferSeconds: 30, MakerQuoteGap: 0.008, MakerInventoryTargetMult: 3.0, MakerInventoryCapMult: 5.0, MakerMinQuoteShares: 10.0, MinAskPrice: 0.10, MaxAskPrice: 0.90}
)

func isMakerSettingsMode(cfg TUISettings) bool {
	return strings.EqualFold(cfg.PaperArbMode, "maker")
}

func isRowVisible(cfg TUISettings, idx int) bool {
	maker := isMakerSettingsMode(cfg)
	switch idx {
	case 6, 7, 8, 9, 10: // Taker specific
		return !maker
	case 13, 14, 15, 16, 17: // Maker specific
		return maker
	default:
		return true
	}
}

func settingsRowEditable(cfg TUISettings, idx int) bool {
	if !isRowVisible(cfg, idx) {
		return false
	}
	return true
}

func settingsRowLabel(cfg TUISettings, idx int) string {
	maker := isMakerSettingsMode(cfg)
	switch idx {
	case 4:
		if maker {
			return "Maker Min Sell Edge %"
		}
		return "Buy Min Margin %"
	case 6:
		return "Buy/Sell Exec Floor %"
	case 7:
		return "Split Min Margin"
	case 8:
		return "Split Strategy"
	case 9:
		return "Split Initial Cap"
	case 10:
		return "Split Replenish Cap"
	case 11:
		return "Min Ask Price"
	case 12:
		if maker {
			return "Maker Max Buy Price"
		}
		return "Max Ask Price"
	case 13:
		return "Maker Merge Buffer"
	case 14:
		return "Maker Quote Gap"
	case 15:
		return "Maker Target Mult"
	case 16:
		return "Maker Cap Mult"
	case 17:
		return "Maker Min Quote"
	case 18:
		return "Max Trade Size"
	case 19:
		return "Max Daily Loss"
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

func normalizeTUISettings(s TUISettings) TUISettings {
	s.MarketSlug = normalizeMarketSelection(s.MarketSlug)
	if s.MaxMarkets < 1 {
		s.MaxMarkets = 1
	}
	if s.MarketSlug != "ALL" {
		selected := len(strings.Split(s.MarketSlug, ","))
		if selected > 0 && s.MaxMarkets > selected {
			s.MaxMarkets = selected
		}
	}
	return s
}

// ─── TUI struct ───────────────────────────────────────────────────────────────

// TUI provides a live bubbletea-driven terminal user interface.
type TUI struct {
	mu sync.Mutex

	engine     *Engine
	orderBook  *OrderBook
	orderBooks map[string]*OrderBook

	markets         map[string]*MarketData
	marketSlug      string
	outcomes        []string
	endTime         time.Time
	lastPrices      map[string]float64
	lastBids        map[string]float64
	lastAsks        map[string]float64
	realBids        map[string]float64
	realAsks        map[string]float64
	pendingOrders   map[string][]PendingOrder
	orderBookDepth  map[string]map[string][]MarketLevel
	eventLog        []string
	maxEvents       int
	orderHistory    []OrderHistoryEntry
	maxOrderHistory int
	isKilled        bool
	killReason      string
	tradeFactor     float64
	startTime       time.Time
	width           int
	height          int
	mode            string // "Paper" or "Real" - for footer display

	restLatency    time.Duration
	restLatencyAvg time.Duration
	restSamples    []time.Duration
	wsLatency      time.Duration
	wsPingLatency  time.Duration
	latencySource  string

	splitInventories []*SplitInventory
	walletTruth      map[string][]WalletTruthPosition

	// Runtime-adjustable settings (readable by the trading loop via GetSettings)
	settings         TUISettings
	onSettingsChange func(TUISettings)

	restartReq bool

	program     *tea.Program
	issueLogger *core.CSVLogger
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
	markets        map[string]*MarketData
	marketSlug     string
	outcomes       []string
	endTime        time.Time
	lastPrices     map[string]float64
	lastBids       map[string]float64
	lastAsks       map[string]float64
	realBids       map[string]float64
	realAsks       map[string]float64
	pendingOrders  map[string][]PendingOrder
	orderBookDepth map[string]map[string][]MarketLevel
	eventLog       []string
	orderHistory   []OrderHistoryEntry
	isKilled       bool
	killReason     string
	tradeFactor    float64
	startTime      time.Time
	width          int
	height         int
	restLatency    time.Duration
	restLatencyAvg time.Duration
	wsLatency      time.Duration
	wsPingLatency  time.Duration
	latencySource  string
	splitPositions []SplitPosition
	walletTruth    []WalletTruthPosition

	stats           Stats
	exposure        float64
	equity          float64
	positions       map[string]PositionPnL
	orders          []ScopedLimitOrder
	multiplier      float64
	rounds          int
	profitable      int
	enginePositions map[string]Position
}

type tuiModel struct {
	tui      *TUI
	interval time.Duration
	snap     tuiSnapshot
	// Quit callback — called before tea.Quit so parent context is cancelled first
	onQuit func()
	// Settings overlay state (immediate, not snapshotted)
	showSettings   bool
	settingsCursor int // 0=TradeScale, 1=MinMargin, 2=SplitMargin, 3=SplitEnabled
	scrollOffset   int
	contentLines   int
}

type WalletTruthPosition struct {
	MarketID      string
	Outcome       string
	LocalShares   float64
	OnChainShares float64
	Drift         float64
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

func (m *tuiModel) clampScrollOffset() {
	if m.scrollOffset < 0 {
		m.scrollOffset = 0
	}
	maxOffset := m.maxScrollOffset()
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

func (m tuiModel) renderMainContent(w int) string {
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
		leftRows = append(leftRows, m.renderAccountStatus(leftW, s.stats, s.exposure, s.equity, s.multiplier, s.rounds, s.profitable, s.enginePositions))
		leftRows = append(leftRows, m.renderPositions(leftW, s.positions))
		if ord := m.renderOrders(leftW, s.orders); ord != "" {
			leftRows = append(leftRows, ord)
		}

		var rightRows []string
		rightRows = append(rightRows, m.renderOrderHistory(rightW, m.orderHistoryRows(true)))
		rightRows = append(rightRows, m.renderEventLog(rightW, m.eventLogRows(true)))

		leftCol := strings.Join(leftRows, "\n\n")
		rightCol := strings.Join(rightRows, "\n\n")

		content := lipgloss.JoinHorizontal(lipgloss.Top, leftCol, "  ", rightCol)
		rows = append(rows, content)
	} else {
		rows = append(rows, m.renderMarketInfo(w))
		rows = append(rows, m.renderAccountStatus(w, s.stats, s.exposure, s.equity,
			s.multiplier, s.rounds, s.profitable, s.enginePositions))
		rows = append(rows, "")
		rows = append(rows, m.renderPositions(w, s.positions))

		if ord := m.renderOrders(w, s.orders); ord != "" {
			rows = append(rows, ord)
		}

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

func (m *tuiModel) refreshScrollMetrics() {
	if m.showSettings {
		m.contentLines = 0
		m.scrollOffset = 0
		return
	}
	w := normalizeTUIWidth(m.snap.width)
	content := m.renderMainContent(w)
	m.contentLines = lipgloss.Height(content)
	m.clampScrollOffset()
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
		positions := m.tui.engine.GetPositionsWithPnL()
		orders := m.tui.getOpenOrdersSnapshot()
		multiplier, rounds, profitable := m.tui.engine.GetCompoundStats()
		enginePositions := m.tui.engine.GetPositions()
		splitPositions := m.tui.getSplitPositions()
		walletTruth := m.tui.getWalletTruthPositions()

		m.tui.mu.Lock()

		// Deep copy maps to prevent data races with background goroutines
		snapMarkets := make(map[string]*MarketData)
		for k, v := range m.tui.markets {
			md := &MarketData{
				Slug:       v.Slug,
				Outcomes:   append([]string(nil), v.Outcomes...),
				EndTime:    v.EndTime,
				Bids:       make(map[string]float64),
				Asks:       make(map[string]float64),
				RealBids:   make(map[string]float64),
				RealAsks:   make(map[string]float64),
				LastUpdate: v.LastUpdate,
				DataSource: v.DataSource,
			}
			for outcome, price := range v.Bids {
				md.Bids[outcome] = price
			}
			for outcome, price := range v.Asks {
				md.Asks[outcome] = price
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

		m.snap = tuiSnapshot{
			markets:         snapMarkets,
			marketSlug:      m.tui.marketSlug,
			outcomes:        append([]string(nil), m.tui.outcomes...),
			endTime:         m.tui.endTime,
			lastPrices:      snapLastPrices,
			lastBids:        snapLastBids,
			lastAsks:        snapLastAsks,
			realBids:        snapRealBids,
			realAsks:        snapRealAsks,
			pendingOrders:   snapPendingOrders,
			orderBookDepth:  snapOrderBookDepth,
			eventLog:        append([]string(nil), m.tui.eventLog...),
			orderHistory:    append([]OrderHistoryEntry(nil), m.tui.orderHistory...),
			isKilled:        m.tui.isKilled,
			killReason:      m.tui.killReason,
			tradeFactor:     m.tui.tradeFactor,
			startTime:       m.tui.startTime,
			width:           m.tui.width,
			height:          m.tui.height,
			restLatency:     m.tui.restLatency,
			restLatencyAvg:  m.tui.restLatencyAvg,
			wsLatency:       m.tui.wsLatency,
			wsPingLatency:   m.tui.wsPingLatency,
			latencySource:   m.tui.latencySource,
			splitPositions:  splitPositions,
			walletTruth:     walletTruth,
			stats:           stats,
			exposure:        exposure,
			equity:          equity,
			positions:       positions,
			orders:          orders,
			multiplier:      multiplier,
			rounds:          rounds,
			profitable:      profitable,
			enginePositions: enginePositions,
		}
		m.tui.mu.Unlock()
		m.refreshScrollMetrics()

		return m, tickCmd(m.interval)

	case tea.KeyMsg:
		key := msg.String()

		// ── Settings overlay key handling ────────────────────────────────────
		if m.showSettings {
			switch key {
			case "s", "S", "enter":
				m.tui.mu.Lock()
				m.tui.restartReq = true
				m.tui.mu.Unlock()
				m.showSettings = false
				m.refreshScrollMetrics()
				return m, nil
			case "esc":
				m.showSettings = false
				m.refreshScrollMetrics()
				return m, nil
						case "up", "k":
				for {
					m.settingsCursor--
					if m.settingsCursor < 0 {
						m.settingsCursor = 19
					}
					if isRowVisible(m.tui.settings, m.settingsCursor) {
						break
					}
				}
				return m, nil
			case "down", "j":
				for {
					m.settingsCursor = (m.settingsCursor + 1) % 20
					if isRowVisible(m.tui.settings, m.settingsCursor) {
						break
					}
				}
				return m, nil
			case "left", "-", "h":
				m.tui.mu.Lock()
				changed := false
				if !settingsRowEditable(m.tui.settings, m.settingsCursor) {
					m.tui.mu.Unlock()
					return m, nil
				}
				switch m.settingsCursor {
				case 0: // Market
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
				case 1: // MaxMarkets
					m.tui.settings.MaxMarkets--
					if m.tui.settings.MaxMarkets < 1 {
						m.tui.settings.MaxMarkets = 1
					}
					changed = true
				case 2: // Timeframe
					if m.tui.settings.Timeframe == "15m" {
						m.tui.settings.Timeframe = "5m"
					} else {
						m.tui.settings.Timeframe = "15m"
					}
					changed = true
				case 3:
					m.tui.settings.TradeScaleFactor -= 0.01
					if m.tui.settings.TradeScaleFactor < 0.01 {
						m.tui.settings.TradeScaleFactor = 0.01
					}
					changed = true
				case 4:
					m.tui.settings.MinMarginPercent -= 0.5
					if m.tui.settings.MinMarginPercent < 0.5 {
						m.tui.settings.MinMarginPercent = 0.5
					}
					changed = true
				case 5:
					m.tui.settings.PaperArbMode = "taker"
					changed = true
				case 6:
					m.tui.settings.BuyExecutionMarginFloorPercent -= 0.5
					if m.tui.settings.BuyExecutionMarginFloorPercent < -10.0 {
						m.tui.settings.BuyExecutionMarginFloorPercent = -10.0
					}
					changed = true
				case 7:
					m.tui.settings.SplitMinMarginSell -= 0.5
					if m.tui.settings.SplitMinMarginSell < 1.0 {
						m.tui.settings.SplitMinMarginSell = 1.0
					}
					changed = true
				case 8:
					m.tui.settings.SplitStrategyEnabled = false
					changed = true
				case 9:
					m.tui.settings.SplitInitialCapPct -= 0.05
					if m.tui.settings.SplitInitialCapPct < 0.05 {
						m.tui.settings.SplitInitialCapPct = 0.05
					}
					changed = true
				case 10:
					m.tui.settings.SplitReplenishCapPct -= 0.05
					if m.tui.settings.SplitReplenishCapPct < 0.05 {
						m.tui.settings.SplitReplenishCapPct = 0.05
					}
					changed = true
				case 11:
					m.tui.settings.MinAskPrice -= 0.01
					if m.tui.settings.MinAskPrice < 0.01 {
						m.tui.settings.MinAskPrice = 0.01
					}
					changed = true
				case 12:
					m.tui.settings.MaxAskPrice -= 0.01
					if m.tui.settings.MaxAskPrice < 0.01 {
						m.tui.settings.MaxAskPrice = 0.01
					}
					changed = true
				case 13:
					m.tui.settings.MakerMergeBufferSeconds -= 5
					if m.tui.settings.MakerMergeBufferSeconds < 5 {
						m.tui.settings.MakerMergeBufferSeconds = 5
					}
					changed = true
				case 14:
					m.tui.settings.MakerQuoteGap -= 0.001
					if m.tui.settings.MakerQuoteGap < 0.001 {
						m.tui.settings.MakerQuoteGap = 0.001
					}
					changed = true
				case 15:
					m.tui.settings.MakerInventoryTargetMult -= 0.5
					if m.tui.settings.MakerInventoryTargetMult < 1.0 {
						m.tui.settings.MakerInventoryTargetMult = 1.0
					}
					changed = true
				case 16:
					m.tui.settings.MakerInventoryCapMult -= 0.5
					if m.tui.settings.MakerInventoryCapMult < 1.0 {
						m.tui.settings.MakerInventoryCapMult = 1.0
					}
					changed = true
				case 17:
					m.tui.settings.MakerMinQuoteShares -= 1.0
					if m.tui.settings.MakerMinQuoteShares < 1.0 {
						m.tui.settings.MakerMinQuoteShares = 1.0
					}
					changed = true
				case 18:
					m.tui.settings.MaxTradeSize -= 5.0
					if m.tui.settings.MaxTradeSize < 0.0 {
						m.tui.settings.MaxTradeSize = 0.0
					}
					changed = true
				case 19:
					m.tui.settings.MaxDailyLoss -= 5.0
					if m.tui.settings.MaxDailyLoss < 0.0 {
						m.tui.settings.MaxDailyLoss = 0.0
					}
					changed = true
				}
				m.tui.tradeFactor = m.tui.settings.TradeScaleFactor
				if changed && m.tui.onSettingsChange != nil {
					m.tui.onSettingsChange(m.tui.settings)
				}
				m.tui.mu.Unlock()
				return m, nil
			case "right", "+", "l":
				m.tui.mu.Lock()
				changed := false
				if !settingsRowEditable(m.tui.settings, m.settingsCursor) {
					m.tui.mu.Unlock()
					return m, nil
				}
				switch m.settingsCursor {
				case 0: // Market
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
				case 1: // MaxMarkets
					m.tui.settings.MaxMarkets++
					if m.tui.settings.MaxMarkets > 20 {
						m.tui.settings.MaxMarkets = 20
					}
					changed = true
				case 2: // Timeframe
					if m.tui.settings.Timeframe == "15m" {
						m.tui.settings.Timeframe = "5m"
					} else {
						m.tui.settings.Timeframe = "15m"
					}
					changed = true
				case 3:
					m.tui.settings.TradeScaleFactor += 0.01
					if m.tui.settings.TradeScaleFactor > 1.0 {
						m.tui.settings.TradeScaleFactor = 1.0
					}
					changed = true
				case 4:
					m.tui.settings.MinMarginPercent += 0.5
					if m.tui.settings.MinMarginPercent > 20.0 {
						m.tui.settings.MinMarginPercent = 20.0
					}
					changed = true
				case 5:
					m.tui.settings.PaperArbMode = "maker"
					changed = true
				case 6:
					m.tui.settings.BuyExecutionMarginFloorPercent += 0.5
					if m.tui.settings.BuyExecutionMarginFloorPercent > 10.0 {
						m.tui.settings.BuyExecutionMarginFloorPercent = 10.0
					}
					changed = true
				case 7:
					m.tui.settings.SplitMinMarginSell += 0.5
					if m.tui.settings.SplitMinMarginSell > 20.0 {
						m.tui.settings.SplitMinMarginSell = 20.0
					}
					changed = true
				case 8:
					m.tui.settings.SplitStrategyEnabled = true
					changed = true
				case 9:
					m.tui.settings.SplitInitialCapPct += 0.05
					if m.tui.settings.SplitInitialCapPct > 1.0 {
						m.tui.settings.SplitInitialCapPct = 1.0
					}
					changed = true
				case 10:
					m.tui.settings.SplitReplenishCapPct += 0.05
					if m.tui.settings.SplitReplenishCapPct > 1.0 {
						m.tui.settings.SplitReplenishCapPct = 1.0
					}
					changed = true
				case 11:
					m.tui.settings.MinAskPrice += 0.01
					if m.tui.settings.MinAskPrice > 0.99 {
						m.tui.settings.MinAskPrice = 0.99
					}
					changed = true
				case 12:
					m.tui.settings.MaxAskPrice += 0.01
					if m.tui.settings.MaxAskPrice > 0.99 {
						m.tui.settings.MaxAskPrice = 0.99
					}
					changed = true
				case 13:
					m.tui.settings.MakerMergeBufferSeconds += 5
					if m.tui.settings.MakerMergeBufferSeconds > 300 {
						m.tui.settings.MakerMergeBufferSeconds = 300
					}
					changed = true
				case 14:
					m.tui.settings.MakerQuoteGap += 0.001
					if m.tui.settings.MakerQuoteGap > 0.100 {
						m.tui.settings.MakerQuoteGap = 0.100
					}
					changed = true
				case 15:
					m.tui.settings.MakerInventoryTargetMult += 0.5
					if m.tui.settings.MakerInventoryTargetMult > 20.0 {
						m.tui.settings.MakerInventoryTargetMult = 20.0
					}
					changed = true
				case 16:
					m.tui.settings.MakerInventoryCapMult += 0.5
					if m.tui.settings.MakerInventoryCapMult > 50.0 {
						m.tui.settings.MakerInventoryCapMult = 50.0
					}
					changed = true
				case 17:
					m.tui.settings.MakerMinQuoteShares += 1.0
					if m.tui.settings.MakerMinQuoteShares > 500.0 {
						m.tui.settings.MakerMinQuoteShares = 500.0
					}
					changed = true
				case 18:
					m.tui.settings.MaxTradeSize += 5.0
					changed = true
				case 19:
					m.tui.settings.MaxDailyLoss += 5.0
					changed = true
				}
				m.tui.tradeFactor = m.tui.settings.TradeScaleFactor
				if changed && m.tui.onSettingsChange != nil {
					m.tui.onSettingsChange(m.tui.settings)
				}
				m.tui.mu.Unlock()
				return m, nil
			// Quick presets
			case "1":
				m.tui.mu.Lock()
				m.tui.settings = SettingsConservative
				m.tui.tradeFactor = SettingsConservative.TradeScaleFactor
				m.tui.mu.Unlock()
				return m, nil
			case "2":
				m.tui.mu.Lock()
				m.tui.settings = SettingsModerate
				m.tui.tradeFactor = SettingsModerate.TradeScaleFactor
				m.tui.mu.Unlock()
				return m, nil
			case "3":
				m.tui.mu.Lock()
				m.tui.settings = SettingsAggressive
				m.tui.tradeFactor = SettingsAggressive.TradeScaleFactor
				m.tui.mu.Unlock()
				return m, nil
			}
			return m, nil
		}

		// ── Normal key handling ──────────────────────────────────────────────
		switch key {
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
			m.refreshScrollMetrics()
			return m, nil
		case "c", "C":
			m.tui.mu.Lock()
			m.tui.eventLog = []string{}
			m.tui.mu.Unlock()
			m.refreshScrollMetrics()
			return m, nil
		case "q", "Q", "ctrl+c":
			// Call the parent cancel func FIRST so the trading loop shuts down
			// immediately (fixes double Ctrl+C requirement).
			if m.onQuit != nil {
				m.onQuit()
			}
			return m, tea.Quit
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
		eventLog:        make([]string, 0),
		maxEvents:       defaultMaxEventHistory,
		walletTruth:     make(map[string][]WalletTruthPosition),
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
	t.program = tea.NewProgram(model, tea.WithAltScreen())
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
}

func (t *TUI) SetMode(mode string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.mode = mode
}

func (t *TUI) AddMarket(id string, slug string, outcomes []string, endTime time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.markets[id] = &MarketData{
		Slug:     slug,
		Outcomes: outcomes,
		EndTime:  endTime,
		Bids:     make(map[string]float64),
		Asks:     make(map[string]float64),
		RealBids: make(map[string]float64),
		RealAsks: make(map[string]float64),
	}
}

func (t *TUI) ClearMarkets() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.markets = make(map[string]*MarketData)
	t.lastPrices = make(map[string]float64)
	t.lastBids = make(map[string]float64)
	t.lastAsks = make(map[string]float64)
	t.orderBookDepth = make(map[string]map[string][]MarketLevel)
	t.pendingOrders = make(map[string][]PendingOrder)
	t.orderBooks = make(map[string]*OrderBook)
	t.splitInventories = nil
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
		for k, v := range bids {
			m.Bids[k] = v
			if v > 0 {
				m.RealBids[k] = v
			}
		}
		for k, v := range asks {
			m.Asks[k] = v
			if v > 0 {
				m.RealAsks[k] = v
			}
		}
		if updatedAt.IsZero() {
			updatedAt = time.Now()
		}
		m.LastUpdate = updatedAt
		m.DataSource = source
	}
}

func (t *TUI) TouchMarket(marketID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if m, ok := t.markets[marketID]; ok {
		m.LastUpdate = time.Now()
	}
}

func (t *TUI) UpdateOrderBookDepth(marketID string, bids, asks map[string][]MarketLevel) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.orderBookDepth[marketID] == nil {
		t.orderBookDepth[marketID] = make(map[string][]MarketLevel)
	}
	for outcome, levels := range bids {
		if len(levels) > 0 {
			copied := make([]MarketLevel, 0, 5)
			for i := 0; i < len(levels) && i < 5; i++ {
				copied = append(copied, levels[i])
			}
			t.orderBookDepth[marketID][outcome+"_bids"] = copied
		} else {
			delete(t.orderBookDepth[marketID], outcome+"_bids")
		}
	}
	for outcome, levels := range asks {
		if len(levels) > 0 {
			copied := make([]MarketLevel, 0, 5)
			for i := 0; i < len(levels) && i < 5; i++ {
				copied = append(copied, levels[i])
			}
			t.orderBookDepth[marketID][outcome+"_asks"] = copied
		} else {
			delete(t.orderBookDepth[marketID], outcome+"_asks")
		}
	}
}

func (t *TUI) SetMarket(slug string, outcomes []string, endTime time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.marketSlug = slug
	t.outcomes = outcomes
	t.endTime = endTime
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
		delete(t.pendingOrders, marketID)
		return
	}
	t.pendingOrders[marketID] = flattened
}

func (t *TUI) LogEvent(format string, args ...interface{}) {
	timestamp := time.Now().Format("15:04:05")
	msg := fmt.Sprintf("[%s] %s", timestamp, fmt.Sprintf(format, args...))
	msg = core.SanitizeString(msg)

	var issueLogger *core.CSVLogger
	equity := 0.0

	t.mu.Lock()
	t.eventLog = append(t.eventLog, msg)
	if len(t.eventLog) > t.maxEvents {
		t.eventLog = t.eventLog[1:]
	}
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
}

func (t *TUI) RecordOrder(marketID, outcome, side string, shares, price, cost, margin, profit float64, status string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := OrderHistoryEntry{
		Timestamp: time.Now(),
		MarketID:  marketID,
		Outcome:   outcome,
		Side:      side,
		Shares:    shares,
		Price:     price,
		Cost:      cost,
		Margin:    margin,
		Profit:    profit,
		Status:    status,
	}
	t.orderHistory = append(t.orderHistory, entry)
	if len(t.orderHistory) > t.maxOrderHistory {
		t.orderHistory = t.orderHistory[len(t.orderHistory)-t.maxOrderHistory:]
	}
}

func (t *TUI) GetOrderHistory() []OrderHistoryEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	result := make([]OrderHistoryEntry, len(t.orderHistory))
	copy(result, t.orderHistory)
	return result
}

func (t *TUI) RegisterSplitInventory(inv *SplitInventory) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.splitInventories = append(t.splitInventories, inv)
}

func (t *TUI) SetWalletTruthPositions(marketID string, positions []WalletTruthPosition) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(positions) == 0 {
		delete(t.walletTruth, marketID)
		return
	}
	t.walletTruth[marketID] = append([]WalletTruthPosition(nil), positions...)
}

func (t *TUI) ClearWalletTruthPositions(marketID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.walletTruth, marketID)
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
	assetOrder := []string{"BTC", "ETH", "SOL", "XRP"}

	var active []string
	for _, id := range assetOrder {
		if _, ok := s.markets[id]; ok {
			active = append(active, id)
		}
	}
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
	emojis := map[string]string{"BTC": "₿", "ETH": "Ξ", "SOL": "◎", "XRP": "✕"}
	emoji := emojis[id]
	if emoji == "" {
		emoji = "•"
	}

	borderColor := assetBorderColors[id]
	if borderColor == "" {
		borderColor = clrSlate
	}

	// ── Header line: bold asset symbol
	header := lipgloss.NewStyle().Bold(true).Foreground(borderColor).
		Render(fmt.Sprintf("%s  %s", emoji, id))

	// ── Slug (truncate to fit)
	slug := core.SanitizeString(mkt.Slug)
	if len(slug) > innerW {
		slug = slug[:innerW-1] + "…"
	}
	slugLine := styleDimmed.Render(slug)

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
	ageSt := styleGreen
	ageWarn := ""
	if age > 10*time.Second {
		ageSt = styleRed
		ageWarn = " ⚠"
	} else if age > 5*time.Second {
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
		bid1 := mkt.Bids[mkt.Outcomes[0]]
		ask1 := mkt.Asks[mkt.Outcomes[0]]
		bid2 := mkt.Bids[mkt.Outcomes[1]]
		ask2 := mkt.Asks[mkt.Outcomes[1]]

		bid1 = recentDisplayQuote(bid1, mkt.RealBids[mkt.Outcomes[0]], age)
		ask1 = recentDisplayQuote(ask1, mkt.RealAsks[mkt.Outcomes[0]], age)
		bid2 = recentDisplayQuote(bid2, mkt.RealBids[mkt.Outcomes[1]], age)
		ask2 = recentDisplayQuote(ask2, mkt.RealAsks[mkt.Outcomes[1]], age)

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

		o1 := core.SanitizeString(mkt.Outcomes[0])
		o2 := core.SanitizeString(mkt.Outcomes[1])
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
		if pairFreshForDisplay && bid1 > 0 && ask1 > 0 && bid2 > 0 && ask2 > 0 {
			askSum := ask1 + ask2
			buyMargin = (1.0 - askSum) * 100
			bidSum := bid1 + bid2
			sellMargin := (bidSum - 1.0) * 100
			priceLinesB.WriteString(fmt.Sprintf("  Buy $%.3f %s  Sell $%.3f %s",
				askSum, marginStyle(buyMargin).Render(fmt.Sprintf("%+.1f%%", buyMargin)),
				bidSum, marginStyle(sellMargin).Render(fmt.Sprintf("%+.1f%%", sellMargin)),
			))
		} else {
			priceLinesB.WriteString(styleDimmed.Render("  ↻ awaiting price data…"))
		}

	}

	content := header + "\n" +
		slugLine + "\n" +
		timeLine + "\n" +
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
		return clamp(defaultTwoColEventRows+extra/2, defaultTwoColEventRows, 40)
	}
	return clamp(defaultOneColEventRows+extra/2, defaultOneColEventRows, 32)
}

func (m tuiModel) renderSingleMarketPrices(outcomes []string, bids, asks, realBids, realAsks map[string]float64, innerW int) string {
	s := m.snap
	var sb strings.Builder

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
//	│  Compound 1.02×  ·  5 rounds (3 profitable)  ·  ⏱ 1h23m         │
//	╰──────────────────────────────────────────────────────────────────╯
func (m tuiModel) renderAccountStatus(w int, stats Stats, totalExposure, equity, multiplier float64, rounds, profitable int, positions map[string]Position) string {
	s := m.snap
	inner := w - 4

	netChange := equity - stats.StartingBalance
	changeSt := styleGreen
	changeSign := "+"
	if netChange < 0 {
		changeSt = styleRed
		changeSign = ""
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
	arbSign := "+"
	if guaranteedProfit < 0 {
		arbSt = styleRed
		arbSign = ""
	}

	// Deployment bar
	deployedPct := 0.0
	if equity > 0 {
		deployedPct = totalExposure / equity
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
	if s.tradeFactor > 0 {
		tradeCost := equity * s.tradeFactor
		if tradeCost < 1.0 {
			tradeCost = 1.0
		}
		tradeLine = fmt.Sprintf("  Trade %.1f%%  ($%.2f/trade)  ·  ", s.tradeFactor*100, tradeCost)
	} else {
		tradeLine = "  "
	}
	tradeLine += fmt.Sprintf("Realized %s  ·  Arb %s",
		changeSt.Render(fmt.Sprintf("%s$%.2f", changeSign, stats.RealizedPnL)),
		arbSt.Render(fmt.Sprintf("%s$%.2f", arbSign, guaranteedProfit)),
	)

	uptime := time.Since(s.startTime).Round(time.Second)

	header := sectionHeader("💼", "ACCOUNT STATUS", clrTeal)
	row1 := fmt.Sprintf("  Cash %s  ·  Exposure %s  ·  Equity %s  (%s)",
		styleBold.Render(fmt.Sprintf("$%.2f", stats.CurrentBalance)),
		styleWhite.Render(fmt.Sprintf("$%.2f", totalExposure)),
		styleBold.Render(fmt.Sprintf("$%.2f", equity)),
		changeSt.Render(fmt.Sprintf("%s$%.2f", changeSign, netChange)),
	)
	row3 := tradeLine
	row4 := fmt.Sprintf("  Compound %s  ·  %d rounds (%d profitable)  ·  ⏱ %s",
		multSt.Render(fmt.Sprintf("%.2f×", multiplier)),
		rounds, profitable,
		styleDimmed.Render(uptime.String()),
	)

	content := header + "\n" + row1 + "\n" + barLine + "\n" + row3 + "\n" + row4
	return makePanel(inner, clrTeal, content)
}

// renderPositions: bordered panel for in-flight and split inventory positions.
func (m tuiModel) renderPositions(w int, positionsWithPnL map[string]PositionPnL) string {
	s := m.snap
	inner := w - 4

	splitPositions := s.splitPositions
	walletTruthPositions := s.walletTruth
	hasPositions := len(positionsWithPnL) > 0
	hasSplitInventory := len(splitPositions) > 0
	hasWalletTruth := len(walletTruthPositions) > 0

	if !hasPositions && !hasSplitInventory && !hasWalletTruth {
		return makePanel(inner, clrSlate,
			sectionHeader("📦", "POSITIONS", clrSlate)+"\n"+
				styleDimmed.Render("  (none)"))
	}

	var sb strings.Builder

	// ── In-flight positions ──
	if hasPositions {
		sb.WriteString(sectionHeader("📦", fmt.Sprintf("IN-FLIGHT  (%d) %s",
			len(positionsWithPnL), styleYellow.Render("⏳ awaiting merge")), clrTeal) + "\n")
	} else {
		sb.WriteString(sectionHeader("📦", "POSITIONS", clrTeal) + "\n")
	}

	byMarket := make(map[string][]PositionPnL)
	for _, pos := range positionsWithPnL {
		mid := pos.MarketID
		if mid == "" {
			mid = "UNKNOWN"
		}
		byMarket[mid] = append(byMarket[mid], pos)
	}

	assetOrder := []string{"BTC", "ETH", "SOL", "XRP", "UNKNOWN"}
	totalMarketPnL, totalLockedPnL := 0.0, 0.0
	hasMarketPrices := false

	for _, marketID := range assetOrder {
		mps, ok := byMarket[marketID]
		if !ok || len(mps) == 0 {
			continue
		}

		aStyle := getAssetStyle(marketID)
		sb.WriteString("  " + aStyle.Render("["+marketID+"]") + "  ")

		sort.Slice(mps, func(i, j int) bool { return mps[i].Outcome < mps[j].Outcome })

		strs := make([]string, 0, len(mps))
		for _, pos := range mps {
			ps := fmt.Sprintf("%s: %.0f@$%.2f", core.SanitizeString(pos.Outcome), pos.Quantity, pos.AvgPrice)
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

		if len(mps) == 2 {
			matched := mps[0].Quantity
			if mps[1].Quantity < matched {
				matched = mps[1].Quantity
			}
			if matched > 0 {
				cost := (mps[0].AvgPrice + mps[1].AvgPrice) * matched
				locked := matched - cost
				totalLockedPnL += locked

				signOf := func(v float64) (string, lipgloss.Style) {
					if v < 0 {
						return "", styleRed
					}
					return "+", styleGreen
				}

				if mps[0].CurrentBid > 0 && mps[1].CurrentBid > 0 {
					mktVal := (mps[0].CurrentBid + mps[1].CurrentBid) * matched
					mktProfit := mktVal - cost
					totalMarketPnL += mktProfit
					hasMarketPrices = true
					sg, pSt := signOf(mktProfit)
					sb.WriteString("  →  " + pSt.Render(fmt.Sprintf("%s$%.2f", sg, mktProfit)))
				} else {
					sg, pSt := signOf(locked)
					sb.WriteString("  →  🔒" + pSt.Render(fmt.Sprintf("%s$%.2f", sg, locked)))
				}
			}
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
		lckSg, lckSt := func() (string, lipgloss.Style) {
			if totalLockedPnL < 0 {
				return "", styleRed
			}
			return "+", styleGreen
		}()
		sb.WriteString(styleBold.Render(fmt.Sprintf("  📊 Now: %s  ·  🔒 Locked: %s",
			mktSt.Render(fmt.Sprintf("%s$%.2f", mktSg, totalMarketPnL)),
			lckSt.Render(fmt.Sprintf("%s$%.2f", lckSg, totalLockedPnL)))) + "\n")
	} else if totalLockedPnL != 0 {
		sg, pSt := func() (string, lipgloss.Style) {
			if totalLockedPnL < 0 {
				return "", styleRed
			}
			return "+", styleGreen
		}()
		sb.WriteString(styleBold.Render("  🔒 Locked: "+pSt.Render(fmt.Sprintf("%s$%.2f", sg, totalLockedPnL))) + "\n")
	}

	// ── Split inventory ──
	if hasSplitInventory {
		sb.WriteString("\n" + sectionHeader("🔀", "SPLIT INVENTORY  (panic sell)", clrTeal) + "\n")
		splitByMarket := make(map[string][]SplitPosition)
		for _, sp := range splitPositions {
			splitByMarket[sp.MarketID] = append(splitByMarket[sp.MarketID], sp)
		}

		for _, marketID := range assetOrder {
			sps, ok := splitByMarket[marketID]
			if !ok || len(sps) == 0 {
				continue
			}
			aStyle := getAssetStyle(marketID)
			sb.WriteString("  " + aStyle.Render("["+marketID+"]") + "  ")

			sort.Slice(sps, func(i, j int) bool { return sps[i].Outcome < sps[j].Outcome })
			strs := make([]string, 0, len(sps))
			for _, sp := range sps {
				strs = append(strs, fmt.Sprintf("%s: %.2f@$%.4f",
					core.SanitizeString(sp.Outcome), sp.Shares, sp.CostBasis))
			}
			sb.WriteString(strings.Join(strs, "  │  "))

			if len(sps) >= 2 {
				minSh := sps[0].Shares
				for _, sp := range sps[1:] {
					if sp.Shares < minSh {
						minSh = sp.Shares
					}
				}
				sb.WriteString("  →  " + styleGreen.Render(fmt.Sprintf("%.2f pairs sellable", minSh)))
			}
			sb.WriteString("\n")
		}
	}

	if hasWalletTruth {
		sb.WriteString("\n" + sectionHeader("🧾", "WALLET TRUTH  (local vs on-chain)", clrTeal) + "\n")
		truthByMarket := make(map[string][]WalletTruthPosition)
		marketSet := make(map[string]struct{})
		for _, wt := range walletTruthPositions {
			truthByMarket[wt.MarketID] = append(truthByMarket[wt.MarketID], wt)
			marketSet[wt.MarketID] = struct{}{}
		}

		orderedMarkets := make([]string, 0, len(marketSet))
		for _, marketID := range assetOrder {
			if _, ok := marketSet[marketID]; ok {
				orderedMarkets = append(orderedMarkets, marketID)
				delete(marketSet, marketID)
			}
		}
		extraMarkets := make([]string, 0, len(marketSet))
		for marketID := range marketSet {
			extraMarkets = append(extraMarkets, marketID)
		}
		sort.Strings(extraMarkets)
		orderedMarkets = append(orderedMarkets, extraMarkets...)

		for _, marketID := range orderedMarkets {
			positions := truthByMarket[marketID]
			if len(positions) == 0 {
				continue
			}
			aStyle := getAssetStyle(marketID)
			sort.Slice(positions, func(i, j int) bool { return positions[i].Outcome < positions[j].Outcome })
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
				parts = append(parts, fmt.Sprintf("%s %s L:%.4f C:%.4f Δ:%s",
					marker,
					core.SanitizeString(wt.Outcome),
					wt.LocalShares,
					wt.OnChainShares,
					driftStyle.Render(fmt.Sprintf("%+.4f", wt.Drift)),
				))
			}
			prefix := "  " + aStyle.Render("["+marketID+"]") + "  "
			if marketWarning {
				prefix = "  " + styleYellow.Render("⚠") + " " + aStyle.Render("["+marketID+"]") + "  "
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
	sort.Strings(outcomes)

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

	sb.WriteString(sectionHeader("📋", fmt.Sprintf("ORDER HISTORY  (last %d)", len(s.orderHistory)), clrSlate) + "\n")

	// Table header
	sb.WriteString(styleDimmed.Render(fmt.Sprintf("  %-8s  %-5s  %-6s  %-5s  %-9s  %-8s  %-8s  %s",
		"TIME", "MKT", "OUTC", "SIDE", "SHARES", "PRICE", "VALUE", "PROFIT/MARGIN")) + "\n")
	sb.WriteString(styleMuted.Render("  "+strings.Repeat("─", min(inner-2, 75))) + "\n")

	displayCount := len(s.orderHistory)
	if displayCount > maxItems {
		displayCount = maxItems
	}

	for i := len(s.orderHistory) - 1; i >= len(s.orderHistory)-displayCount && i >= 0; i-- {
		o := s.orderHistory[i]

		statusIcon := "✅"
		marginSt := styleGreen
		switch o.Status {
		case "FAILED":
			statusIcon = "❌"
			marginSt = styleRed
		case "PARTIAL":
			statusIcon = "⚠️"
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
				marginText = fmt.Sprintf("%s$%.2f (maker)", sg, math.Abs(o.Profit))
			} else {
				marginText = fmt.Sprintf("%s$%.2f (%.1f%%)", sg, math.Abs(o.Profit), o.Margin)
			}
		} else {
			if o.Margin == 0.0 {
				marginText = "maker"
				marginSt = styleDimmed
			} else {
				marginText = fmt.Sprintf("%.2f%%", o.Margin)
			}
		}

		aStyle := getAssetStyle(o.MarketID)
		sb.WriteString(fmt.Sprintf("  %s  %s  %-6s  %-5s  %7.2f  $%-7.4f  $%-7.2f  %s  %s\n",
			styleDimmed.Render(o.Timestamp.Format("15:04:05")),
			aStyle.Render(fmt.Sprintf("%-3s", o.MarketID)),
			core.SanitizeString(o.Outcome),
			o.Side,
			o.Shares,
			o.Price,
			o.Cost,
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
	label := "EVENT LOG"
	if len(s.eventLog) > 0 {
		label = fmt.Sprintf("EVENT LOG  (showing %d/%d)", visible, len(s.eventLog))
	}
	sb.WriteString(sectionHeader("📜", label, clrSlate) + "\n")
	if len(s.eventLog) == 0 {
		sb.WriteString(styleDimmed.Render("  (waiting for events…)"))
	} else {
		startIdx := 0
		if len(s.eventLog) > maxItems {
			startIdx = len(s.eventLog) - maxItems
		}
		for i := startIdx; i < len(s.eventLog); i++ {
			sb.WriteString("  " + s.eventLog[i] + "\n")
		}
	}
	return makePanel(inner, clrSlate, sb.String())
}

// renderFooter: slim status bar.
func (m tuiModel) renderFooter(w int, scrollOffset, maxOffset int) string {
	m.tui.mu.Lock()
	mode := m.tui.mode
	m.tui.mu.Unlock()

	if mode == "" {
		mode = "Paper"
	}

	modeText := mode + " Trading Mode"
	scrollText := "Top"
	if maxOffset > 0 {
		scrollText = fmt.Sprintf("Scroll %d/%d", scrollOffset, maxOffset)
	}
	leftText := "  Polyarb-15m  ·  " + modeText + "  ·  " + scrollText
	rightText := "[↑↓/jk] scroll  [PgUp/PgDn] page  [g/G] top/btm  [q] quit  "
	if w < 120 {
		rightText = "[↑↓/jk] scroll  [PgUp/PgDn] page  [q] quit  "
	}
	if w < 92 {
		rightText = "[↑↓] scroll  [q] quit  "
	}
	left := styleMuted.Render(leftText)
	right := styleMuted.Render(rightText)
	leftLen := len(leftText)
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
	m.tui.mu.Unlock()

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(clrBrand)
	title := titleStyle.Render("⚙  LIVE SETTINGS")

	keysLine := styleDimmed.Render("  [↑↓/jk] Navigate  [←→/+-] Adjust  [1/2/3] Presets  [s/Esc] Close")

	divider := styleMuted.Render("  " + strings.Repeat("─", min(inner-2, 60)))

	type row struct {
		label    string
		value    string
		bar      string
		disabled bool
	}

	fmtPct := func(v float64) string { return fmt.Sprintf("%5.1f%%", v*100) }

	makerMode := isMakerSettingsMode(cfg)
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
			label: "Timeframe",
			value: fmt.Sprintf(" %s ", cfg.Timeframe),
			bar:   "",
		},
		{
			label: "Trade Scale Factor",
			value: fmtPct(cfg.TradeScaleFactor),
			bar:   renderBar(cfg.TradeScaleFactor/1.0, 20),
		},
		{
			label: settingsRowLabel(cfg, 4),
			value: fmt.Sprintf("%5.1f%%", cfg.MinMarginPercent),
			bar:   renderBar(cfg.MinMarginPercent/20.0, 20),
		},
		{
			label: "Paper Arb Mode",
			value: func() string {
				if strings.EqualFold(cfg.PaperArbMode, "maker") {
					return styleGreen.Render(" maker ")
				}
				return styleYellow.Render(" taker ")
			}(),
			bar: "",
		},
		{
			label: settingsRowLabel(cfg, 6),
			value: fmt.Sprintf("%5.1f%%", cfg.BuyExecutionMarginFloorPercent),
			bar:   renderBar((cfg.BuyExecutionMarginFloorPercent+10.0)/15.0, 20),
		},
		{
			label: settingsRowLabel(cfg, 7),
			value: fmt.Sprintf("%5.1f%%", cfg.SplitMinMarginSell),
			bar:   renderBar(cfg.SplitMinMarginSell/20.0, 20),
		},
		{
			label: settingsRowLabel(cfg, 8),
			value: func() string {
				if cfg.SplitStrategyEnabled {
					return styleGreen.Render("  ON ")
				}
				return styleMuted.Render(" OFF ")
			}(),
			bar:   "",
		},
		{
			label: settingsRowLabel(cfg, 9),
			value: fmtPct(cfg.SplitInitialCapPct),
			bar:   renderBar(cfg.SplitInitialCapPct, 20),
		},
		{
			label: settingsRowLabel(cfg, 10),
			value: fmtPct(cfg.SplitReplenishCapPct),
			bar:   renderBar(cfg.SplitReplenishCapPct, 20),
		},
		{
			label: settingsRowLabel(cfg, 11),
			value: fmt.Sprintf(" $%.2f ", cfg.MinAskPrice),
			bar:   renderBar(cfg.MinAskPrice, 20),
		},
		{
			label: settingsRowLabel(cfg, 12),
			value: fmt.Sprintf(" $%.2f ", cfg.MaxAskPrice),
			bar:   renderBar(cfg.MaxAskPrice, 20),
		},
		{
			label: settingsRowLabel(cfg, 13),
			value: fmt.Sprintf(" %3ds ", cfg.MakerMergeBufferSeconds),
			bar:   renderBar(float64(cfg.MakerMergeBufferSeconds)/120.0, 20),
		},
		{
			label: settingsRowLabel(cfg, 14),
			value: fmt.Sprintf(" $%.3f ", cfg.MakerQuoteGap),
			bar:   renderBar(cfg.MakerQuoteGap/0.05, 20),
		},
		{
			label: settingsRowLabel(cfg, 15),
			value: fmt.Sprintf(" %.1fx ", cfg.MakerInventoryTargetMult),
			bar:   renderBar(cfg.MakerInventoryTargetMult/10.0, 20),
		},
		{
			label: settingsRowLabel(cfg, 16),
			value: fmt.Sprintf(" %.1fx ", cfg.MakerInventoryCapMult),
			bar:   renderBar(cfg.MakerInventoryCapMult/20.0, 20),
		},
		{
			label: settingsRowLabel(cfg, 17),
			value: fmt.Sprintf(" %.1f sh ", cfg.MakerMinQuoteShares),
			bar:   renderBar(cfg.MakerMinQuoteShares/50.0, 20),
		},
		{
			label: settingsRowLabel(cfg, 18),
			value: func() string {
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
			label: settingsRowLabel(cfg, 19),
			value: func() string {
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
	}

	cursorStyle := lipgloss.NewStyle().Bold(true).Foreground(clrBrand)
	labelStyle := lipgloss.NewStyle().Foreground(clrDim)
	valueStyle := lipgloss.NewStyle().Bold(true).Foreground(clrWhite)

		var rowLines []string
	for i, r := range rows {
		if !isRowVisible(cfg, i) {
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
	modeNote := ""
	if makerMode {
		modeNote = styleDimmed.Render("  Maker mode: split/taker-only rows are shown for reference and ignored live") + "\n"
	}

	content := title + "\n" +
		keysLine + "\n\n" +
		strings.Join(rowLines, "\n") + "\n\n" +
		presetDivider + "\n" +
		presetTitle + "\n" +
		p1 + "\n" +
		p2 + "\n" +
		p3 + "\n\n" +
		divider + "\n" +
		modeNote +
		balanceNote

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
	s = normalizeTUISettings(s)
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
