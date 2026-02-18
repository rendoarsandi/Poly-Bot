package paper

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"Market-bot/internal/core"
)

// ─── Lipgloss styles (replace raw ANSI codes) ─────────────────────────────────
// Colors map to standard ANSI basic palette — compatible with all terminals.
var (
	styleRed     = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleGreen   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleYellow  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleMagenta = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
	styleCyan    = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	styleWhite   = lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
	styleBold    = lipgloss.NewStyle().Bold(true)

	// Background red + bold (kill banner)
	styleBgRedBold = lipgloss.NewStyle().
			Background(lipgloss.Color("1")).
			Foreground(lipgloss.Color("15")).
			Bold(true)

	// Bold + colored — used for section/asset headers
	styleBoldYellow  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
	styleBoldCyan    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	styleBoldMagenta = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("5"))
	styleBoldGreen   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2"))
)

// getAssetStyle returns the foreground style for a given asset ID.
func getAssetStyle(id string) lipgloss.Style {
	switch id {
	case "BTC":
		return styleYellow
	case "ETH":
		return styleCyan
	case "SOL":
		return styleMagenta
	case "XRP":
		return styleGreen
	default:
		return styleWhite
	}
}

// getBoldAssetStyle returns the bold+colored style for asset section headers.
func getBoldAssetStyle(id string) lipgloss.Style {
	switch id {
	case "BTC":
		return styleBoldYellow
	case "ETH":
		return styleBoldCyan
	case "SOL":
		return styleBoldMagenta
	case "XRP":
		return styleBoldGreen
	default:
		return styleBold
	}
}

// marginStyle returns a color style based on a percentage margin value.
func marginStyle(pct float64) lipgloss.Style {
	if pct >= 3 {
		return styleGreen
	} else if pct >= 2 {
		return styleYellow
	} else if pct < 1 {
		return styleRed
	}
	return styleWhite
}

// MarketData holds data for a single market in the TUI
type MarketData struct {
	Slug       string
	Outcomes   []string
	EndTime    time.Time
	Bids       map[string]float64
	Asks       map[string]float64
	RealBids   map[string]float64
	RealAsks   map[string]float64
	LastUpdate time.Time // When prices were last updated
	DataSource string    // "WS" or "REST" - which source provided latest data
}

// PendingOrder represents an order the bot intends to place
type PendingOrder struct {
	Outcome string
	Price   float64
	Qty     float64
	Side    string // "BUY" or "SELL"
}

// OrderHistoryEntry represents a completed trade for the order history
type OrderHistoryEntry struct {
	Timestamp time.Time
	MarketID  string // e.g., "BTC", "ETH"
	Outcome   string // e.g., "Up", "Down"
	Side      string // "BUY" or "SELL"
	Shares    float64
	Price     float64
	Cost      float64
	Margin    float64 // Arb margin at time of trade
	Status    string  // "FILLED", "PARTIAL", "FAILED"
}

// TUI provides a live terminal user interface.
// All public methods are identical to the original; bubbletea handles
// rendering, alt-screen, cursor management, and the event loop internally.
type TUI struct {
	mu sync.Mutex

	// References for engine data fetching on each tick
	engine    *Engine
	orderBook *OrderBook

	// Display state (all mutex-protected)
	markets         map[string]*MarketData
	marketSlug      string // Legacy single-market
	outcomes        []string
	endTime         time.Time
	lastPrices      map[string]float64
	lastBids        map[string]float64
	lastAsks        map[string]float64
	realBids        map[string]float64
	realAsks        map[string]float64
	pendingOrders   map[string][]PendingOrder
	orderBookDepth  map[string]map[string][]MarketLevel // marketID -> outcome+_bids/_asks -> levels
	eventLog        []string
	maxEvents       int
	orderHistory    []OrderHistoryEntry
	maxOrderHistory int
	isKilled        bool
	killReason      string
	tradeFactor     float64
	startTime       time.Time
	width           int

	// Network latency tracking
	restLatency    time.Duration
	restLatencyAvg time.Duration
	restSamples    []time.Duration
	wsLatency      time.Duration
	wsPingLatency  time.Duration
	latencySource  string

	// Split inventories — field accessed directly by package tests, so kept on TUI.
	splitInventories []*SplitInventory

	// Bubbletea program (created lazily in StartRenderLoop)
	program *tea.Program
}

// ─── Bubbletea internals ──────────────────────────────────────────────────────

// tuiSnapshot is a lock-free, point-in-time copy of all state needed to render
// one frame. Update() takes the snapshot under mu; View() renders it without locks.
type tuiSnapshot struct {
	// TUI display state
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
	restLatency    time.Duration
	restLatencyAvg time.Duration
	wsLatency      time.Duration
	wsPingLatency  time.Duration
	latencySource  string
	splitPositions []SplitPosition

	// Engine data fetched on each tick (engine has its own lock)
	stats           Stats
	exposure        float64
	equity          float64
	positions       map[string]PositionPnL
	orders          []*LimitOrder
	multiplier      float64
	rounds          int
	profitable      int
	enginePositions map[string]Position
}

// tuiModel implements tea.Model. It holds a reference to the TUI adapter and
// a cached snapshot of all state for lock-free rendering in View().
type tuiModel struct {
	tui      *TUI
	interval time.Duration
	snap     tuiSnapshot
}

type tickMsg time.Time

func tickCmd(interval time.Duration) tea.Cmd {
	return tea.Tick(interval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// Init kicks off the first tick.
func (m tuiModel) Init() tea.Cmd {
	return tickCmd(m.interval)
}

// Update handles messages. On each tickMsg it snapshots all state for rendering.
func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		_ = msg

		// Fetch engine/orderbook data OUTSIDE tui.mu — they have their own locks.
		stats := m.tui.engine.GetStats()
		exposure, _ := m.tui.engine.GetExposure()
		equity := m.tui.engine.GetEquity()
		positions := m.tui.engine.GetPositionsWithPnL()
		orders := m.tui.orderBook.GetOpenOrders()
		multiplier, rounds, profitable := m.tui.engine.GetCompoundStats()
		enginePositions := m.tui.engine.GetPositions()
		// getSplitPositions must be called WITHOUT tui.mu
		// (split inventories have their own locks — avoid lock ordering deadlock).
		splitPositions := m.tui.getSplitPositions()

		// Snapshot TUI display state under its lock.
		m.tui.mu.Lock()
		m.snap = tuiSnapshot{
			markets:         m.tui.markets,
			marketSlug:      m.tui.marketSlug,
			outcomes:        m.tui.outcomes,
			endTime:         m.tui.endTime,
			lastPrices:      m.tui.lastPrices,
			lastBids:        m.tui.lastBids,
			lastAsks:        m.tui.lastAsks,
			realBids:        m.tui.realBids,
			realAsks:        m.tui.realAsks,
			pendingOrders:   m.tui.pendingOrders,
			orderBookDepth:  m.tui.orderBookDepth,
			eventLog:        append([]string(nil), m.tui.eventLog...),
			orderHistory:    append([]OrderHistoryEntry(nil), m.tui.orderHistory...),
			isKilled:        m.tui.isKilled,
			killReason:      m.tui.killReason,
			tradeFactor:     m.tui.tradeFactor,
			startTime:       m.tui.startTime,
			width:           m.tui.width,
			restLatency:     m.tui.restLatency,
			restLatencyAvg:  m.tui.restLatencyAvg,
			wsLatency:       m.tui.wsLatency,
			wsPingLatency:   m.tui.wsPingLatency,
			latencySource:   m.tui.latencySource,
			splitPositions:  splitPositions,
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

		return m, tickCmd(m.interval)

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "Q", "ctrl+c":
			return m, tea.Quit
		}
	}
	return m, nil
}

// View renders the current snapshot. Pure and lock-free — safe to call any time.
func (m tuiModel) View() string {
	s := m.snap
	var sb strings.Builder

	sb.WriteString(m.renderHeader())
	sb.WriteString("\n")
	sb.WriteString(m.renderMarketInfo())
	sb.WriteString("\n")
	sb.WriteString(m.renderAccountStatus(s.stats, s.exposure, s.equity, s.multiplier, s.rounds, s.profitable, s.enginePositions))
	sb.WriteString("\n")
	sb.WriteString(m.renderPositions(s.positions))
	sb.WriteString("\n")
	sb.WriteString(m.renderOrders(s.orders))
	sb.WriteString("\n")
	sb.WriteString(m.renderOrderHistory())
	sb.WriteString("\n")
	sb.WriteString(m.renderEventLog())

	if s.isKilled {
		sb.WriteString(m.renderKillBanner())
	}

	return sb.String()
}

// ─── TUI public API (all signatures unchanged) ────────────────────────────────

// NewTUI creates a new terminal UI
func NewTUI(engine *Engine, orderBook *OrderBook) *TUI {
	return &TUI{
		engine:          engine,
		orderBook:       orderBook,
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
		maxOrderHistory: 20,
		eventLog:        make([]string, 0),
		maxEvents:       10,
		width:           80,
		startTime:       time.Now(),
	}
}

// StartRenderLoop creates the bubbletea program and begins rendering.
// Replaces the previous manual goroutine + frameCh + frameWriter approach.
func (t *TUI) StartRenderLoop(interval time.Duration) {
	model := tuiModel{tui: t, interval: interval}
	t.program = tea.NewProgram(model, tea.WithAltScreen())
	go func() {
		if _, err := t.program.Run(); err != nil {
			// Program exited (normal or error) — nothing to do.
			_ = err
		}
	}()
}

// Stop quits the bubbletea program and restores the terminal.
// Safe to call multiple times (bubbletea's Quit is idempotent).
func (t *TUI) Stop() {
	if t.program != nil {
		t.program.Quit()
	}
}

// UpdateLatency updates the network health display (legacy entry point).
func (t *TUI) UpdateLatency(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.restLatency = d
}

// UpdateRestLatency updates REST API latency with a rolling average.
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

// UpdateWSLatency updates WebSocket staleness.
func (t *TUI) UpdateWSLatency(timeSinceLastMsg time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.wsLatency = timeSinceLastMsg
}

// UpdateWSPingLatency updates WebSocket ping round-trip time.
func (t *TUI) UpdateWSPingLatency(pingLatency time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.wsPingLatency = pingLatency
}

// SetTradeFactor updates the trade factor for display.
func (t *TUI) SetTradeFactor(factor float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tradeFactor = factor
}

// AddMarket adds a market to the multi-market display.
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

// ClearMarkets clears all market data for rotation to new markets.
func (t *TUI) ClearMarkets() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.markets = make(map[string]*MarketData)
	t.lastPrices = make(map[string]float64)
	t.lastBids = make(map[string]float64)
	t.lastAsks = make(map[string]float64)
	t.orderBookDepth = make(map[string]map[string][]MarketLevel)
	t.pendingOrders = make(map[string][]PendingOrder)
}

// UpdateMarketPrices updates prices for a specific market.
func (t *TUI) UpdateMarketPrices(marketID string, bids, asks map[string]float64) {
	t.UpdateMarketPricesWithSource(marketID, bids, asks, "WS")
}

// UpdateMarketPricesWithSource updates prices and tracks the data source.
func (t *TUI) UpdateMarketPricesWithSource(marketID string, bids, asks map[string]float64, source string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if m, ok := t.markets[marketID]; ok {
		for k, v := range bids {
			m.Bids[k] = v
			m.RealBids[k] = v
		}
		for k, v := range asks {
			m.Asks[k] = v
			m.RealAsks[k] = v
		}
		m.LastUpdate = time.Now()
		m.DataSource = source
	}
}

// TouchMarket updates the LastUpdate timestamp without changing prices.
func (t *TUI) TouchMarket(marketID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if m, ok := t.markets[marketID]; ok {
		m.LastUpdate = time.Now()
	}
}

// UpdateOrderBookDepth updates the full order book depth for a market.
func (t *TUI) UpdateOrderBookDepth(marketID string, bids, asks map[string][]MarketLevel) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.orderBookDepth[marketID] == nil {
		t.orderBookDepth[marketID] = make(map[string][]MarketLevel)
	}

	for outcome, levels := range bids {
		copied := make([]MarketLevel, 0, 5)
		for i := 0; i < len(levels) && i < 5; i++ {
			copied = append(copied, levels[i])
		}
		t.orderBookDepth[marketID][outcome+"_bids"] = copied
	}

	for outcome, levels := range asks {
		copied := make([]MarketLevel, 0, 5)
		for i := 0; i < len(levels) && i < 5; i++ {
			copied = append(copied, levels[i])
		}
		t.orderBookDepth[marketID][outcome+"_asks"] = copied
	}
}

// SetMarket sets the current market info (legacy single-market API).
func (t *TUI) SetMarket(slug string, outcomes []string, endTime time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.marketSlug = slug
	t.outcomes = outcomes
	t.endTime = endTime
}

// UpdatePrices updates the current prices (legacy single-market API).
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

// UpdateRealMarket updates the real market prices (from external verification).
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

// SetPendingOrders sets the orders the bot intends to place.
func (t *TUI) SetPendingOrders(orders map[string][]PendingOrder) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pendingOrders = orders
}

// LogEvent adds an event to the log.
func (t *TUI) LogEvent(format string, args ...interface{}) {
	t.mu.Lock()
	defer t.mu.Unlock()

	timestamp := time.Now().Format("15:04:05")
	msg := fmt.Sprintf("[%s] %s", timestamp, fmt.Sprintf(format, args...))
	msg = core.SanitizeString(msg)

	t.eventLog = append(t.eventLog, msg)
	if len(t.eventLog) > t.maxEvents {
		t.eventLog = t.eventLog[1:]
	}
}

// SetKillSwitch marks the UI as killed.
func (t *TUI) SetKillSwitch(reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.isKilled = true
	t.killReason = reason
}

// RecordOrder adds a trade to the order history.
func (t *TUI) RecordOrder(marketID, outcome, side string, shares, price, cost, margin float64, status string) {
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
		Status:    status,
	}
	t.orderHistory = append(t.orderHistory, entry)
	if len(t.orderHistory) > t.maxOrderHistory {
		t.orderHistory = t.orderHistory[len(t.orderHistory)-t.maxOrderHistory:]
	}
}

// GetOrderHistory returns a copy of the order history.
func (t *TUI) GetOrderHistory() []OrderHistoryEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	result := make([]OrderHistoryEntry, len(t.orderHistory))
	copy(result, t.orderHistory)
	return result
}

// RegisterSplitInventory adds a split inventory for display in the positions section.
func (t *TUI) RegisterSplitInventory(inv *SplitInventory) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.splitInventories = append(t.splitInventories, inv)
}

// getSplitPositions collects all split positions from registered inventories.
// Must be called WITHOUT holding t.mu (split inventories have their own locks).
func (t *TUI) getSplitPositions() []SplitPosition {
	var all []SplitPosition
	for _, inv := range t.splitInventories {
		all = append(all, inv.GetAllPositions()...)
	}
	return all
}

// ─── Render methods (on tuiModel, all read from m.snap) ──────────────────────

func (m tuiModel) renderHeader() string {
	s := m.snap
	line := strings.Repeat("═", s.width)
	title := " 🎰 POLYARB-15M MULTI-ASSET TRADING "
	padding := (s.width - len(title)) / 2
	if padding < 0 {
		padding = 0
	}

	uptime := time.Since(s.startTime).Round(time.Second)

	restStyle := styleGreen
	if s.restLatency > 200*time.Millisecond {
		restStyle = styleRed
	} else if s.restLatency > 100*time.Millisecond {
		restStyle = styleYellow
	}
	restStr := "..."
	if s.restLatency > 0 {
		restStr = s.restLatency.Round(time.Millisecond).String()
	}

	wsStyle := styleGreen
	wsStatus := "✓"
	if s.wsPingLatency == 0 {
		wsStyle = styleYellow
		wsStatus = "?"
	} else if s.wsPingLatency > 500*time.Millisecond {
		wsStyle = styleRed
		wsStatus = "⚠"
	} else if s.wsPingLatency > 200*time.Millisecond {
		wsStyle = styleYellow
	}

	freshStyle := styleGreen
	if s.wsLatency > 10*time.Second {
		freshStyle = styleRed
		wsStatus = "✗"
	} else if s.wsLatency > 5*time.Second {
		freshStyle = styleYellow
	}

	wsStr := "..."
	if s.wsPingLatency > 0 {
		wsStr = s.wsPingLatency.Round(time.Millisecond).String()
	}
	freshStr := "..."
	if s.wsLatency > 0 {
		freshStr = fmt.Sprintf("%.1fs", s.wsLatency.Seconds())
	}

	healthLine := fmt.Sprintf("  ⏱️  Uptime: %v | 📡 REST: %s | 🔌 WS: %s (%s %s)",
		uptime,
		restStyle.Render(restStr),
		wsStyle.Render(wsStr),
		freshStyle.Render(freshStr),
		wsStatus)

	healthDisplayWidth := len(uptime.String()) + len(restStr) + len(wsStr) + len(freshStr) + 50
	healthPadding := s.width - healthDisplayWidth
	if healthPadding < 0 {
		healthPadding = 0
	}

	boldLine := styleBold.Render(line)
	boldTitle := styleBold.Render(strings.Repeat(" ", padding) + title)
	return boldLine + "\n" + boldTitle + "\n" +
		healthLine + strings.Repeat(" ", healthPadding) + "\n" +
		line
}

func (m tuiModel) renderMarketInfo() string {
	s := m.snap
	var sb strings.Builder

	if len(s.markets) > 0 {
		return m.renderMultiMarketInfo()
	}

	// Legacy single-market rendering
	remaining := time.Until(s.endTime)
	if remaining < 0 {
		remaining = 0
	}
	timeStyle := styleGreen
	if remaining < 2*time.Minute {
		timeStyle = styleRed
	} else if remaining < 5*time.Minute {
		timeStyle = styleYellow
	}

	sb.WriteString(styleBold.Render("📊 MARKET:") + " " + s.marketSlug + "\n")
	sb.WriteString("   ⏱️  Time: " + timeStyle.Render(remaining.Round(time.Second).String()) + " remaining\n")

	if len(s.outcomes) == 2 {
		sb.WriteString("\n")
		sb.WriteString(m.renderSingleMarketPrices(s.outcomes, s.lastBids, s.lastAsks, s.realBids, s.realAsks))
	}

	return sb.String()
}

func (m tuiModel) renderMultiMarketInfo() string {
	s := m.snap
	var sb strings.Builder

	totalMargin := 0.0
	marketCount := 0

	assetOrder := []string{"BTC", "ETH", "SOL", "XRP"}
	assetEmojis := map[string]string{
		"BTC": "₿",
		"ETH": "Ξ",
		"SOL": "◎",
		"XRP": "✕",
	}

	for _, id := range assetOrder {
		mkt, ok := s.markets[id]
		if !ok {
			continue
		}

		remaining := time.Until(mkt.EndTime)
		if remaining < 0 {
			remaining = 0
		}
		timeStyle := styleGreen
		if remaining < 2*time.Minute {
			timeStyle = styleRed
		} else if remaining < 5*time.Minute {
			timeStyle = styleYellow
		}

		hdrStyle := getBoldAssetStyle(id)
		emoji := assetEmojis[id]
		if emoji == "" {
			emoji = "•"
		}

		sb.WriteString(hdrStyle.Render(fmt.Sprintf("═══ %s %s ══════════════════════════════════════════════", emoji, id)) + "\n")
		sb.WriteString(fmt.Sprintf("   📊 %s\n", core.SanitizeString(mkt.Slug)))

		updateAge := time.Since(mkt.LastUpdate)
		updateStyle := styleGreen
		updateWarning := ""
		if updateAge > 10*time.Second {
			updateStyle = styleRed
			updateWarning = " ⚠️ STALE!"
		} else if updateAge > 5*time.Second {
			updateStyle = styleYellow
			updateWarning = " (slow)"
		} else if updateAge > 2*time.Second {
			updateStyle = styleYellow
		}

		sourceStyle := styleGreen
		sourceStr := mkt.DataSource
		if sourceStr == "" {
			sourceStr = "?"
			sourceStyle = styleYellow
		} else if sourceStr == "REST" {
			sourceStyle = styleCyan
		}

		sb.WriteString(fmt.Sprintf("   ⏱️  Time: %s | %s [%s]%s\n",
			timeStyle.Render(remaining.Round(time.Second).String()),
			updateStyle.Render(fmt.Sprintf("%.1fs ago", updateAge.Seconds())),
			sourceStyle.Render(sourceStr),
			updateWarning))

		if len(mkt.Outcomes) == 2 {
			bid1 := mkt.Bids[mkt.Outcomes[0]]
			ask1 := mkt.Asks[mkt.Outcomes[0]]
			bid2 := mkt.Bids[mkt.Outcomes[1]]
			ask2 := mkt.Asks[mkt.Outcomes[1]]

			// For binary markets, infer missing prices from complement
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

			sb.WriteString(m.renderOrderBookForMarket(id, mkt.Outcomes[0], bid1, ask1))
			sb.WriteString(m.renderOrderBookForMarket(id, mkt.Outcomes[1], bid2, ask2))

			if ask1 > 0 && ask2 > 0 && bid1 > 0 && bid2 > 0 {
				askSum := ask1 + ask2
				buyMargin := (1.0 - askSum) * 100
				bidSum := bid1 + bid2
				sellMargin := (bidSum - 1.0) * 100

				sb.WriteString(fmt.Sprintf("   📉 BUY: $%.2f | %s  📈 SELL: $%.2f | %s\n",
					askSum, marginStyle(buyMargin).Render(fmt.Sprintf("%+.1f%%", buyMargin)),
					bidSum, marginStyle(sellMargin).Render(fmt.Sprintf("%+.1f%%", sellMargin))))
				totalMargin += buyMargin
				marketCount++
			} else {
				sb.WriteString("   📈 " + styleYellow.Render("(waiting for price data...)") + "\n")
			}
		}
		sb.WriteString("\n")
	}

	if marketCount > 0 {
		avgMargin := totalMargin / float64(marketCount)
		sb.WriteString(styleBold.Render(fmt.Sprintf("📊 COMBINED: %d markets | Avg Margin: %s",
			marketCount, marginStyle(avgMargin).Render(fmt.Sprintf("%.1f%%", avgMargin)))) + "\n")
	}

	return sb.String()
}

func (m tuiModel) renderOrderBookForMarket(marketID, outcome string, bestBid, bestAsk float64) string {
	s := m.snap
	var sb strings.Builder

	depth := s.orderBookDepth[marketID]
	bids := depth[outcome+"_bids"]
	asks := depth[outcome+"_asks"]

	if len(bids) > 0 && bids[0].Price > bestBid {
		bestBid = bids[0].Price
	}
	if len(asks) > 0 && asks[0].Price > 0 && (bestAsk == 0 || asks[0].Price < bestAsk) {
		bestAsk = asks[0].Price
	}

	displayOutcome := core.SanitizeString(outcome)
	if len(displayOutcome) > 6 {
		displayOutcome = displayOutcome[:6]
	}

	sb.WriteString(fmt.Sprintf("   %-6s  ", displayOutcome))

	if bestBid > 0 {
		sb.WriteString(styleGreen.Render(fmt.Sprintf("Bid: $%.2f", bestBid)))
	} else {
		sb.WriteString(styleGreen.Render("Bid: --.---"))
	}
	sb.WriteString("  │  ")
	if bestAsk > 0 {
		sb.WriteString(styleRed.Render(fmt.Sprintf("Ask: $%.2f", bestAsk)))
	} else {
		sb.WriteString(styleRed.Render("Ask: --.---"))
	}
	sb.WriteString("\n")
	return sb.String()
}

// min returns the smaller of two integers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (m tuiModel) renderSingleMarketPrices(outcomes []string, bids, asks, realBids, realAsks map[string]float64) string {
	s := m.snap
	var sb strings.Builder

	sb.WriteString(styleCyan.Render("┌─ 🌐 REAL MARKET (Polymarket Website) ─────────────────────┐") + "\n")
	realBid1 := realBids[outcomes[0]]
	realAsk1 := realAsks[outcomes[0]]
	realBid2 := realBids[outcomes[1]]
	realAsk2 := realAsks[outcomes[1]]

	if realAsk1 > 0 || realAsk2 > 0 {
		sb.WriteString(fmt.Sprintf("│  %s: bid $%.2f / ask $%.2f\n", core.SanitizeString(outcomes[0]), realBid1, realAsk1))
		sb.WriteString(fmt.Sprintf("│  %s: bid $%.2f / ask $%.2f\n", core.SanitizeString(outcomes[1]), realBid2, realAsk2))
	} else {
		sb.WriteString("│  (waiting for real market data...)\n")
	}
	sb.WriteString(styleCyan.Render("└────────────────────────────────────────────────────────────┘") + "\n")

	sb.WriteString(styleYellow.Render("┌─ 🤖 BOT READING (REST API Response) ──────────────────────┐") + "\n")
	bid1 := bids[outcomes[0]]
	ask1 := asks[outcomes[0]]
	bid2 := bids[outcomes[1]]
	ask2 := asks[outcomes[1]]

	mismatch1 := realAsk1 > 0 && (abs(ask1-realAsk1) > 0.05 || abs(bid1-realBid1) > 0.05)
	mismatch2 := realAsk2 > 0 && (abs(ask2-realAsk2) > 0.05 || abs(bid2-realBid2) > 0.05)

	line1 := fmt.Sprintf("│  %s: bid $%.2f / ask $%.2f", core.SanitizeString(outcomes[0]), bid1, ask1)
	if mismatch1 {
		line1 = styleRed.Render(line1) + " " + styleRed.Render("⚠️ MISMATCH!")
	}
	sb.WriteString(line1 + "\n")

	line2 := fmt.Sprintf("│  %s: bid $%.2f / ask $%.2f", core.SanitizeString(outcomes[1]), bid2, ask2)
	if mismatch2 {
		line2 = styleRed.Render(line2) + " " + styleRed.Render("⚠️ MISMATCH!")
	}
	sb.WriteString(line2 + "\n")

	askSum := ask1 + ask2
	buyMargin := (1.0 - askSum) * 100
	bidSum := bid1 + bid2
	sellMargin := (bidSum - 1.0) * 100

	sb.WriteString(fmt.Sprintf("│  📉 BUY:  ask_sum=$%.2f | %s\n", askSum,
		marginStyle(buyMargin).Render(fmt.Sprintf("Margin: %+.1f%%", buyMargin))))
	sb.WriteString(fmt.Sprintf("│  📈 SELL: bid_sum=$%.2f | %s\n", bidSum,
		marginStyle(sellMargin).Render(fmt.Sprintf("Margin: %+.1f%%", sellMargin))))
	sb.WriteString(styleYellow.Render("└────────────────────────────────────────────────────────────┘") + "\n")

	sb.WriteString(styleGreen.Render("┌─ 📋 BOT PLANNED ORDERS ───────────────────────────────────┐") + "\n")
	if len(s.pendingOrders) > 0 {
		for outcome, orders := range s.pendingOrders {
			for _, o := range orders {
				sb.WriteString(fmt.Sprintf("│  %s %s: %.0f shares @ $%.2f\n", o.Side, core.SanitizeString(outcome), o.Qty, o.Price))
			}
		}
	} else {
		sb.WriteString("│  (no pending orders)\n")
	}
	sb.WriteString(styleGreen.Render("└────────────────────────────────────────────────────────────┘") + "\n")

	return sb.String()
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func (m tuiModel) renderAccountStatus(stats Stats, totalExposure, equity, multiplier float64, rounds, profitable int, positions map[string]Position) string {
	s := m.snap
	var sb strings.Builder

	netChange := equity - stats.StartingBalance
	changeSign := "+"
	changeStyle := styleGreen
	if netChange < 0 {
		changeStyle = styleRed
		changeSign = ""
	}

	multStyle := styleWhite
	if multiplier >= 1.5 {
		multStyle = styleGreen
	} else if multiplier > 1.0 {
		multStyle = styleYellow
	}

	byMarket := make(map[string][]Position)
	for _, pos := range positions {
		marketID := pos.MarketID
		if marketID == "" {
			marketID = "UNKNOWN"
		}
		byMarket[marketID] = append(byMarket[marketID], pos)
	}

	guaranteedProfit := 0.0
	for _, marketPositions := range byMarket {
		if len(marketPositions) == 2 {
			pos1 := marketPositions[0]
			pos2 := marketPositions[1]
			matchedQty := pos1.Quantity
			if pos2.Quantity < matchedQty {
				matchedQty = pos2.Quantity
			}
			matchedCost := (pos1.AvgPrice + pos2.AvgPrice) * matchedQty
			guaranteedProfit += (matchedQty * 1.0) - matchedCost
		}
	}

	arbSign := "+"
	arbStyle := styleGreen
	if guaranteedProfit < 0 {
		arbStyle = styleRed
		arbSign = ""
	}

	sb.WriteString(styleBold.Render("💼 ACCOUNT") + "\n")
	sb.WriteString(fmt.Sprintf("   💵 Cash:     $%.2f\n", stats.CurrentBalance))
	sb.WriteString(fmt.Sprintf("   📦 Exposure: $%.2f\n", totalExposure))
	sb.WriteString(fmt.Sprintf("   💰 Equity:   $%.2f (%s)\n",
		equity, changeStyle.Render(fmt.Sprintf("%s$%.2f", changeSign, netChange))))

	if s.tradeFactor > 0 {
		tradeCost := equity * s.tradeFactor
		if tradeCost < 1.0 {
			tradeCost = 1.0
		}
		sb.WriteString(fmt.Sprintf("   🎯 Trade:    %.1f%% ($%.2f/trade)\n", s.tradeFactor*100, tradeCost))
	}

	sb.WriteString(fmt.Sprintf("   📊 Realized: $%.2f | 🎯 Arb Profit: %s\n",
		stats.RealizedPnL, arbStyle.Render(fmt.Sprintf("%s$%.2f", arbSign, guaranteedProfit))))
	sb.WriteString(fmt.Sprintf("   📈 Compound: %s | Rounds: %d (%d profitable)\n",
		multStyle.Render(fmt.Sprintf("%.2fx", multiplier)), rounds, profitable))

	uptime := time.Since(s.startTime).Round(time.Second)
	sb.WriteString(fmt.Sprintf("   ⏱️  Uptime:   %v\n", uptime))

	return sb.String()
}

func (m tuiModel) renderPositions(positionsWithPnL map[string]PositionPnL) string {
	s := m.snap
	var sb strings.Builder

	splitPositions := s.splitPositions
	hasPositions := len(positionsWithPnL) > 0
	hasSplitInventory := len(splitPositions) > 0

	if !hasPositions && !hasSplitInventory {
		sb.WriteString(styleBold.Render("📦 POSITIONS") + " (none)\n")
		return sb.String()
	}

	if hasPositions {
		sb.WriteString(styleBold.Render("📦 IN-FLIGHT"))
		sb.WriteString(fmt.Sprintf(" (%d) %s\n", len(positionsWithPnL),
			styleYellow.Render("⏳ awaiting merge")))
	} else if hasSplitInventory {
		sb.WriteString(styleBold.Render("📦 POSITIONS") + "\n")
	}

	byMarket := make(map[string][]PositionPnL)
	for _, pos := range positionsWithPnL {
		marketID := pos.MarketID
		if marketID == "" {
			marketID = "UNKNOWN"
		}
		byMarket[marketID] = append(byMarket[marketID], pos)
	}

	assetOrder := []string{"BTC", "ETH", "SOL", "XRP", "UNKNOWN"}

	totalMarketPnL := 0.0
	totalLockedPnL := 0.0
	hasMarketPrices := false

	for _, marketID := range assetOrder {
		marketPositions, ok := byMarket[marketID]
		if !ok || len(marketPositions) == 0 {
			continue
		}

		aStyle := getAssetStyle(marketID)
		sb.WriteString("   " + aStyle.Render("["+marketID+"]") + " ")

		sort.Slice(marketPositions, func(i, j int) bool {
			return marketPositions[i].Outcome < marketPositions[j].Outcome
		})

		positionStrs := make([]string, 0, len(marketPositions))
		for _, pos := range marketPositions {
			posStr := fmt.Sprintf("%s: %.0f@$%.2f", core.SanitizeString(pos.Outcome), pos.Quantity, pos.AvgPrice)
			if pos.CurrentBid > 0 {
				bidStyle := styleGreen
				if pos.CurrentBid < pos.AvgPrice {
					bidStyle = styleRed
				}
				posStr += " (" + bidStyle.Render(fmt.Sprintf("now:$%.2f", pos.CurrentBid)) + ")"
			}
			positionStrs = append(positionStrs, posStr)
		}
		sb.WriteString(strings.Join(positionStrs, " | "))

		if len(marketPositions) == 2 {
			pos1 := marketPositions[0]
			pos2 := marketPositions[1]
			matchedQty := pos1.Quantity
			if pos2.Quantity < matchedQty {
				matchedQty = pos2.Quantity
			}
			if matchedQty > 0 {
				matchedCost := (pos1.AvgPrice + pos2.AvgPrice) * matchedQty
				lockedProfit := (matchedQty * 1.0) - matchedCost
				totalLockedPnL += lockedProfit

				pnlSign := func(v float64) (string, lipgloss.Style) {
					if v < 0 {
						return "", styleRed
					}
					return "+", styleGreen
				}

				if pos1.CurrentBid > 0 && pos2.CurrentBid > 0 {
					marketValue := (pos1.CurrentBid + pos2.CurrentBid) * matchedQty
					marketProfit := marketValue - matchedCost
					totalMarketPnL += marketProfit
					hasMarketPrices = true
					sign, pStyle := pnlSign(marketProfit)
					sb.WriteString(" → " + pStyle.Render(fmt.Sprintf("%s$%.2f", sign, marketProfit)))
				} else {
					sign, pStyle := pnlSign(lockedProfit)
					sb.WriteString(" → 🔒" + pStyle.Render(fmt.Sprintf("%s$%.2f", sign, lockedProfit)))
				}
			}
		}
		sb.WriteString("\n")
	}

	if hasMarketPrices {
		mktSign, mktStyle := func() (string, lipgloss.Style) {
			if totalMarketPnL < 0 {
				return "", styleRed
			}
			return "+", styleGreen
		}()
		lckSign, lckStyle := func() (string, lipgloss.Style) {
			if totalLockedPnL < 0 {
				return "", styleRed
			}
			return "+", styleGreen
		}()
		sb.WriteString("   " + styleBold.Render(fmt.Sprintf("📊 Now: %s | 🔒 Locked: %s",
			mktStyle.Render(fmt.Sprintf("%s$%.2f", mktSign, totalMarketPnL)),
			lckStyle.Render(fmt.Sprintf("%s$%.2f", lckSign, totalLockedPnL)))) + "\n")
	} else if totalLockedPnL != 0 {
		sign, pStyle := func() (string, lipgloss.Style) {
			if totalLockedPnL < 0 {
				return "", styleRed
			}
			return "+", styleGreen
		}()
		sb.WriteString("   " + styleBold.Render("🔒 Locked Profit: "+
			pStyle.Render(fmt.Sprintf("%s$%.2f", sign, totalLockedPnL))) + "\n")
	}

	if hasSplitInventory {
		sb.WriteString("\n" + styleBold.Render("🔀 SPLIT INVENTORY") + " (panic sell)\n")

		splitByMarket := make(map[string][]SplitPosition)
		for _, sp := range splitPositions {
			splitByMarket[sp.MarketID] = append(splitByMarket[sp.MarketID], sp)
		}

		for _, marketID := range assetOrder {
			positions, ok := splitByMarket[marketID]
			if !ok || len(positions) == 0 {
				continue
			}

			aStyle := getAssetStyle(marketID)
			sb.WriteString("   " + aStyle.Render("["+marketID+"]") + " ")

			sort.Slice(positions, func(i, j int) bool {
				return positions[i].Outcome < positions[j].Outcome
			})

			posStrs := make([]string, 0, len(positions))
			for _, sp := range positions {
				posStrs = append(posStrs, fmt.Sprintf("%s: %.0f@$%.2f",
					core.SanitizeString(sp.Outcome), sp.Shares, sp.CostBasis))
			}
			sb.WriteString(strings.Join(posStrs, " | "))

			if len(positions) >= 2 {
				minShares := positions[0].Shares
				for _, p := range positions[1:] {
					if p.Shares < minShares {
						minShares = p.Shares
					}
				}
				sb.WriteString(" → " + styleGreen.Render(fmt.Sprintf("%.0f pairs sellable", minShares)))
			}
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

func (m tuiModel) renderOrders(orders []*LimitOrder) string {
	if len(orders) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString(styleBold.Render("📝 LIMIT ORDERS"))
	sb.WriteString(fmt.Sprintf(" (%d)\n", len(orders)))

	byOutcome := make(map[string][]*LimitOrder)
	for _, o := range orders {
		byOutcome[o.Outcome] = append(byOutcome[o.Outcome], o)
	}

	for outcome, ords := range byOutcome {
		totalQty := 0.0
		totalVal := 0.0
		for _, o := range ords {
			totalQty += o.RemainingQty()
			totalVal += o.RemainingQty() * o.Price
		}
		sb.WriteString(fmt.Sprintf("   • %s: %d orders, %.0f shares, $%.2f value\n",
			core.SanitizeString(outcome), len(ords), totalQty, totalVal))
	}

	return sb.String()
}

func (m tuiModel) renderEventLog() string {
	s := m.snap
	var sb strings.Builder

	sb.WriteString(styleBold.Render("📜 EVENTS") + "\n")

	if len(s.eventLog) == 0 {
		sb.WriteString("   (waiting for events...)\n")
		return sb.String()
	}

	for _, event := range s.eventLog {
		sb.WriteString("   " + event + "\n")
	}

	return sb.String()
}

func (m tuiModel) renderOrderHistory() string {
	s := m.snap
	var sb strings.Builder

	sb.WriteString(styleBold.Render("📋 ORDER HISTORY"))

	if len(s.orderHistory) == 0 {
		sb.WriteString(" (no trades yet)\n")
		return sb.String()
	}

	sb.WriteString(fmt.Sprintf(" (last %d)\n", len(s.orderHistory)))

	displayCount := len(s.orderHistory)
	if displayCount > 8 {
		displayCount = 8
	}

	for i := len(s.orderHistory) - 1; i >= len(s.orderHistory)-displayCount && i >= 0; i-- {
		o := s.orderHistory[i]

		statusStyle := styleGreen
		statusIcon := "✅"
		if o.Status == "FAILED" {
			statusStyle = styleRed
			statusIcon = "❌"
		} else if o.Status == "PARTIAL" {
			statusStyle = styleYellow
			statusIcon = "⚠️"
		}

		aStyle := getAssetStyle(o.MarketID)
		timeStr := o.Timestamp.Format("15:04:05")

		sb.WriteString(fmt.Sprintf("   %s %s %s %-6s %.0f @ $%.2f ($%.1f) %s\n",
			timeStr,
			aStyle.Render("["+o.MarketID+"]"),
			statusIcon,
			core.SanitizeString(o.Outcome),
			o.Shares,
			o.Price,
			o.Cost,
			statusStyle.Render(fmt.Sprintf("%.1f%%", o.Margin))))
	}

	return sb.String()
}

func (m tuiModel) renderKillBanner() string {
	s := m.snap
	var sb strings.Builder

	pad := func(n int) string {
		if n < 0 {
			n = 0
		}
		return strings.Repeat(" ", n)
	}

	sb.WriteString("\n")
	line1 := styleBgRedBold.Render(pad(s.width))
	line2 := styleBgRedBold.Render("   🚨 KILL SWITCH ACTIVATED 🚨" + pad(s.width-31))
	reasonPad := s.width - 12 - len(s.killReason)
	line3 := styleBgRedBold.Render(fmt.Sprintf("   Reason: %s%s", s.killReason, pad(reasonPad)))
	line4 := styleBgRedBold.Render(pad(s.width))

	sb.WriteString(line1 + "\n" + line2 + "\n" + line3 + "\n" + line4 + "\n")

	return sb.String()
}
