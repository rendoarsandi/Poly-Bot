package paper

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"Market-bot/internal/core"
)

// ANSI escape codes for terminal control
const (
	ClearScreen  = "\033[2J"
	MoveCursor   = "\033[%d;%dH" // row, col
	ClearLine    = "\033[2K"
	ClearToEOL   = "\033[K"
	HideCursor   = "\033[?25l"
	ShowCursor   = "\033[?25h"
	AltScreenOn  = "\033[?1049h"
	AltScreenOff = "\033[?1049l"
	Bold         = "\033[1m"
	Reset        = "\033[0m"
	ColorRed     = "\033[31m"
	ColorGreen   = "\033[32m"
	ColorYellow  = "\033[33m"
	ColorBlue    = "\033[34m"
	ColorMagenta = "\033[35m"
	ColorCyan    = "\033[36m"
	ColorWhite   = "\033[37m"
	BgRed        = "\033[41m"
	BgGreen      = "\033[42m"
	BgYellow     = "\033[43m"
)

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

// TUI provides a live terminal user interface
type TUI struct {
	mu sync.Mutex

	// References
	engine    *Engine
	orderBook *OrderBook

	// State - now supports multiple markets
	markets    map[string]*MarketData // key = market identifier (e.g., "ETH", "SOL")
	marketSlug string                 // Legacy - primary market
	outcomes   []string               // Legacy - primary market outcomes
	endTime    time.Time              // Legacy - primary market end time
	lastPrices map[string]float64
	lastBids   map[string]float64
	lastAsks   map[string]float64
	eventLog   []string
	maxEvents  int
	running    bool
	stopped    atomic.Bool // Atomic flag for fast shutdown detection without lock
	killReason string
	isKilled   bool

	// Order history - persists across market rotations
	orderHistory    []OrderHistoryEntry
	maxOrderHistory int // Max entries to keep

	// Real market data (for comparison)
	realBids map[string]float64
	realAsks map[string]float64

	// Bot's intended orders (before placement)
	pendingOrders map[string][]PendingOrder

	// Order book depth per market
	orderBookDepth map[string]map[string][]MarketLevel // marketID -> outcome -> levels

	// Network Health - Real-time latency tracking
	latency        time.Duration
	latencySource  string
	restLatency    time.Duration   // Latest REST /book latency
	wsLatency      time.Duration   // Time since last WS message
	wsPingLatency  time.Duration   // WS ping round-trip time
	restLatencyAvg time.Duration   // Rolling average REST latency
	restSamples    []time.Duration // Recent REST latency samples for averaging

	// Display dimensions
	width int

	// Startup time
	startTime time.Time

	// Stop channel for clean shutdown
	stopCh   chan struct{}
	stopOnce sync.Once // Ensure Stop() only runs once

	// Non-blocking output channel
	frameCh chan string

	// Split inventory references for display
	splitInventories []*SplitInventory
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
		maxOrderHistory: 20, // Keep last 20 orders
		eventLog:        make([]string, 0),
		maxEvents:       10,
		width:           80,
		startTime:       time.Now(),
		running:         true,
		stopCh:          make(chan struct{}),
		frameCh:         make(chan string, 3), // Buffer 3 frames to prevent blocking
	}
}

// UpdateLatency updates the network health display (legacy - use UpdateRestLatency for real-time)
func (t *TUI) UpdateLatency(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.latency = d
}

// UpdateRestLatency updates REST API latency with rolling average
func (t *TUI) UpdateRestLatency(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.restLatency = d

	// Keep last 20 samples for rolling average
	t.restSamples = append(t.restSamples, d)
	if len(t.restSamples) > 20 {
		t.restSamples = t.restSamples[1:]
	}

	// Calculate rolling average
	var total time.Duration
	for _, s := range t.restSamples {
		total += s
	}
	t.restLatencyAvg = total / time.Duration(len(t.restSamples))

	// Also update main latency display
	t.latency = t.restLatencyAvg
	t.latencySource = "REST /book"
}

// UpdateWSLatency updates WebSocket staleness
func (t *TUI) UpdateWSLatency(timeSinceLastMsg time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.wsLatency = timeSinceLastMsg
}

// UpdateWSPingLatency updates WebSocket ping round-trip time
func (t *TUI) UpdateWSPingLatency(pingLatency time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.wsPingLatency = pingLatency
}

// AddMarket adds a market to the multi-market display
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

// ClearMarkets clears all market data for rotation to new markets
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

// UpdateMarketPrices updates prices for a specific market
func (t *TUI) UpdateMarketPrices(marketID string, bids, asks map[string]float64) {
	t.UpdateMarketPricesWithSource(marketID, bids, asks, "WS")
}

// UpdateMarketPricesWithSource updates prices and tracks the data source
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

// TouchMarket updates the LastUpdate timestamp without changing prices
// Use this when connection is healthy but no new data to report
func (t *TUI) TouchMarket(marketID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if m, ok := t.markets[marketID]; ok {
		m.LastUpdate = time.Now()
	}
}

// UpdateOrderBookDepth updates the full order book depth for a market
func (t *TUI) UpdateOrderBookDepth(marketID string, bids, asks map[string][]MarketLevel) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.orderBookDepth[marketID] == nil {
		t.orderBookDepth[marketID] = make(map[string][]MarketLevel)
	}

	// Store bids (sorted by price descending - highest first)
	for outcome, levels := range bids {
		// Make a copy and keep top 5 levels
		copied := make([]MarketLevel, 0, 5)
		for i := 0; i < len(levels) && i < 5; i++ {
			copied = append(copied, levels[i])
		}
		t.orderBookDepth[marketID][outcome+"_bids"] = copied
	}

	// Store asks (sorted by price ascending - lowest first)
	for outcome, levels := range asks {
		copied := make([]MarketLevel, 0, 5)
		for i := 0; i < len(levels) && i < 5; i++ {
			copied = append(copied, levels[i])
		}
		t.orderBookDepth[marketID][outcome+"_asks"] = copied
	}
}

// SetMarket sets the current market info
func (t *TUI) SetMarket(slug string, outcomes []string, endTime time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.marketSlug = slug
	t.outcomes = outcomes
	t.endTime = endTime
}

// UpdatePrices updates the current prices (bot's reading from API)
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

// UpdateRealMarket updates the real market prices (from external verification)
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

// SetPendingOrders sets the orders the bot intends to place
func (t *TUI) SetPendingOrders(orders map[string][]PendingOrder) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pendingOrders = orders
}

// LogEvent adds an event to the log
func (t *TUI) LogEvent(format string, args ...interface{}) {
	t.mu.Lock()
	defer t.mu.Unlock()

	timestamp := time.Now().Format("15:04:05")
	msg := fmt.Sprintf("[%s] %s", timestamp, fmt.Sprintf(format, args...))

	// Sanitize to prevent terminal injection
	msg = core.SanitizeString(msg)

	t.eventLog = append(t.eventLog, msg)
	if len(t.eventLog) > t.maxEvents {
		t.eventLog = t.eventLog[1:]
	}
}

// SetKillSwitch marks the UI as killed
func (t *TUI) SetKillSwitch(reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.isKilled = true
	t.killReason = reason
}

// RecordOrder adds a trade to the order history
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

	// Keep only the last maxOrderHistory entries
	if len(t.orderHistory) > t.maxOrderHistory {
		t.orderHistory = t.orderHistory[len(t.orderHistory)-t.maxOrderHistory:]
	}
}

// GetOrderHistory returns a copy of the order history
func (t *TUI) GetOrderHistory() []OrderHistoryEntry {
	t.mu.Lock()
	defer t.mu.Unlock()

	result := make([]OrderHistoryEntry, len(t.orderHistory))
	copy(result, t.orderHistory)
	return result
}

// RegisterSplitInventory adds a split inventory for display in the positions section
func (t *TUI) RegisterSplitInventory(inv *SplitInventory) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.splitInventories = append(t.splitInventories, inv)
}

// getSplitPositions collects all split positions from registered inventories
// Must be called WITHOUT holding t.mu lock (inventories have their own locks)
func (t *TUI) getSplitPositions() []SplitPosition {
	var all []SplitPosition
	for _, inv := range t.splitInventories {
		all = append(all, inv.GetAllPositions()...)
	}
	return all
}

// Stop stops the UI - safe to call multiple times
func (t *TUI) Stop() {
	// Use sync.Once to ensure we only stop once
	t.stopOnce.Do(func() {
		// Set atomic flag first for instant detection by render loop
		t.stopped.Store(true)

		// Then update the mutex-protected state
		t.mu.Lock()
		t.running = false
		t.mu.Unlock()

		// Close the stop channel to signal the render loop
		close(t.stopCh)

		// Restore cursor - this is safe to do outside the lock
		fmt.Print(ShowCursor)
		fmt.Println() // Move to new line for clean exit
	})
}

// Render draws the entire UI
func (t *TUI) Render() {
	// Fast path: check atomic flag without lock
	if t.stopped.Load() {
		return
	}

	// 1. Fetch data from other components BEFORE taking TUI lock
	// This breaks the circular dependency: TUI.mu -> Engine.mu/OrderBook.mu
	stats := t.engine.GetStats()
	exposure, _ := t.engine.GetExposure()
	equity := t.engine.GetEquity()
	positions := t.engine.GetPositionsWithPnL()
	orders := t.orderBook.GetOpenOrders()
	multiplier, rounds, profitable := t.engine.GetCompoundStats()
	enginePositions := t.engine.GetPositions()

	// 2. Build the frame while holding the TUI lock
	t.mu.Lock()

	if !t.running {
		t.mu.Unlock()
		return
	}

	var sb strings.Builder

	// Move cursor to top-left (alternate buffer handles the rest)
	sb.WriteString(fmt.Sprintf(MoveCursor, 1, 1))

	// Helper to write lines with clear-to-end-of-line
	writeLine := func(s string) {
		lines := strings.Split(s, "\n")
		for i, line := range lines {
			sb.WriteString(line)
			sb.WriteString(ClearToEOL)
			if i < len(lines)-1 {
				sb.WriteString("\n")
			}
		}
	}

	// Header
	writeLine(t.renderHeader())
	sb.WriteString("\n")

	// Market Info
	writeLine(t.renderMarketInfo())
	sb.WriteString("\n")

	// Account Status
	writeLine(t.renderAccountStatus(stats, exposure, equity, multiplier, rounds, profitable, enginePositions))
	sb.WriteString("\n")

	// Positions
	writeLine(t.renderPositions(positions))
	sb.WriteString("\n")

	// Open Orders
	writeLine(t.renderOrders(orders))
	sb.WriteString("\n")

	// Order History (persists across market rotations)
	writeLine(t.renderOrderHistory())
	sb.WriteString("\n")

	// Event Log
	writeLine(t.renderEventLog())

	// Kill switch banner if triggered
	if t.isKilled {
		writeLine(t.renderKillBanner())
	}

	// Clear from cursor to end of screen
	sb.WriteString("\033[J")

	// Get the complete frame as a string
	frame := sb.String()

	// Release the lock BEFORE doing I/O
	t.mu.Unlock()

	// Non-blocking send to frame channel
	select {
	case t.frameCh <- frame:
		// Frame queued
	default:
		// Channel full, skip frame
	}
}

func (t *TUI) renderHeader() string {
	line := strings.Repeat("═", t.width)
	title := " 🎰 POLYARB-15M MULTI-ASSET TRADING "
	padding := (t.width - len(title)) / 2
	if padding < 0 {
		padding = 0
	}

	uptime := time.Since(t.startTime).Round(time.Second)

	// REST latency health (actual network round-trip)
	restColor := ColorGreen
	if t.restLatency > 200*time.Millisecond {
		restColor = ColorRed
	} else if t.restLatency > 100*time.Millisecond {
		restColor = ColorYellow
	}

	restStr := "..."
	if t.restLatency > 0 {
		restStr = fmt.Sprintf("%v", t.restLatency.Round(time.Millisecond))
	}

	// WS ping latency health (actual network round-trip)
	wsColor := ColorGreen
	wsStatus := "✓"
	if t.wsPingLatency == 0 {
		wsColor = ColorYellow
		wsStatus = "?"
	} else if t.wsPingLatency > 500*time.Millisecond {
		wsColor = ColorRed
		wsStatus = "⚠"
	} else if t.wsPingLatency > 200*time.Millisecond {
		wsColor = ColorYellow
	}

	// WS data freshness indicator
	freshColor := ColorGreen
	if t.wsLatency > 10*time.Second {
		freshColor = ColorRed
		wsStatus = "✗"
	} else if t.wsLatency > 5*time.Second {
		freshColor = ColorYellow
	}

	wsStr := "..."
	if t.wsPingLatency > 0 {
		wsStr = fmt.Sprintf("%v", t.wsPingLatency.Round(time.Millisecond))
	}

	freshStr := "..."
	if t.wsLatency > 0 {
		freshStr = fmt.Sprintf("%.1fs", t.wsLatency.Seconds())
	}

	// Health line showing uptime, REST and WS latency
	healthLine := fmt.Sprintf("  ⏱️  Uptime: %v | 📡 REST: %s%s%s | 🔌 WS: %s%s%s (%s%s%s %s)",
		uptime, restColor, restStr, Reset, wsColor, wsStr, Reset, freshColor, freshStr, Reset, wsStatus)

	// Calculate display width (excluding ANSI codes and accounting for emoji width)
	healthDisplayWidth := len(uptime.String()) + len(restStr) + len(wsStr) + len(freshStr) + 50 // fixed text + emojis
	healthPadding := t.width - healthDisplayWidth
	if healthPadding < 0 {
		healthPadding = 0
	}

	return fmt.Sprintf("%s%s\n%s%s%s\n%s%s\n%s",
		Bold, line,
		strings.Repeat(" ", padding), title, Reset,
		healthLine, strings.Repeat(" ", healthPadding),
		line)
}

func (t *TUI) renderMarketInfo() string {
	var sb strings.Builder

	// If we have multiple markets, render them all
	if len(t.markets) > 0 {
		return t.renderMultiMarketInfo()
	}

	// Legacy single market rendering
	remaining := time.Until(t.endTime)
	if remaining < 0 {
		remaining = 0
	}

	// Color based on time remaining
	timeColor := ColorGreen
	if remaining < 2*time.Minute {
		timeColor = ColorRed
	} else if remaining < 5*time.Minute {
		timeColor = ColorYellow
	}

	sb.WriteString(fmt.Sprintf("%s📊 MARKET:%s %s\n", Bold, Reset, t.marketSlug))
	sb.WriteString(fmt.Sprintf("   ⏱️  Time: %s%v%s remaining\n", timeColor, remaining.Round(time.Second), Reset))

	if len(t.outcomes) == 2 {
		sb.WriteString("\n")
		sb.WriteString(t.renderSingleMarketPrices(t.outcomes, t.lastBids, t.lastAsks, t.realBids, t.realAsks))
	}

	return sb.String()
}

// renderMultiMarketInfo renders info for multiple markets
func (t *TUI) renderMultiMarketInfo() string {
	var sb strings.Builder

	totalMargin := 0.0
	marketCount := 0

	// Define asset order and colors for consistent display
	assetOrder := []string{"BTC", "ETH", "SOL", "XRP"}
	assetColors := map[string]string{
		"BTC": ColorYellow,  // Bitcoin - gold/yellow
		"ETH": ColorCyan,    // Ethereum - cyan/blue
		"SOL": ColorMagenta, // Solana - purple
		"XRP": ColorGreen,   // XRP - green
	}
	assetEmojis := map[string]string{
		"BTC": "₿",
		"ETH": "Ξ",
		"SOL": "◎",
		"XRP": "✕",
	}

	for _, id := range assetOrder {
		m, ok := t.markets[id]
		if !ok {
			continue
		}

		remaining := time.Until(m.EndTime)
		if remaining < 0 {
			remaining = 0
		}

		// Color based on time remaining
		timeColor := ColorGreen
		if remaining < 2*time.Minute {
			timeColor = ColorRed
		} else if remaining < 5*time.Minute {
			timeColor = ColorYellow
		}

		// Get asset-specific color
		headerColor := assetColors[id]
		if headerColor == "" {
			headerColor = ColorWhite
		}
		emoji := assetEmojis[id]
		if emoji == "" {
			emoji = "•"
		}

		sb.WriteString(fmt.Sprintf("%s%s═══ %s %s ══════════════════════════════════════════════%s\n", Bold, headerColor, emoji, id, Reset))
		sb.WriteString(fmt.Sprintf("   📊 %s\n", core.SanitizeString(m.Slug)))

		// Show time remaining and last price update
		updateAge := time.Since(m.LastUpdate)
		updateColor := ColorGreen
		updateWarning := ""
		if updateAge > 10*time.Second {
			updateColor = ColorRed
			updateWarning = " ⚠️ STALE!"
		} else if updateAge > 5*time.Second {
			updateColor = ColorYellow
			updateWarning = " (slow)"
		} else if updateAge > 2*time.Second {
			updateColor = ColorYellow
		}

		// Show data source (WS or REST)
		sourceColor := ColorGreen
		sourceStr := m.DataSource
		if sourceStr == "" {
			sourceStr = "?"
			sourceColor = ColorYellow
		} else if sourceStr == "REST" {
			sourceColor = ColorCyan
		}

		sb.WriteString(fmt.Sprintf("   ⏱️  Time: %s%v%s | %s%.1fs ago%s [%s%s%s]%s\n",
			timeColor, remaining.Round(time.Second), Reset,
			updateColor, updateAge.Seconds(), Reset,
			sourceColor, sourceStr, Reset, updateWarning))

		if len(m.Outcomes) == 2 {
			bid1 := m.Bids[m.Outcomes[0]]
			ask1 := m.Asks[m.Outcomes[0]]
			bid2 := m.Bids[m.Outcomes[1]]
			ask2 := m.Asks[m.Outcomes[1]]

			// For binary markets, infer missing prices from complement
			// Up bid ≈ 1 - Down ask, Up ask ≈ 1 - Down bid
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

			// Display order book depth for each outcome
			sb.WriteString(t.renderOrderBookForMarket(id, m.Outcomes[0], bid1, ask1))
			sb.WriteString(t.renderOrderBookForMarket(id, m.Outcomes[1], bid2, ask2))

			// Calculate margin - only show valid data
			if ask1 > 0 && ask2 > 0 && bid1 > 0 && bid2 > 0 {
				askSum := ask1 + ask2
				buyMargin := (1.0 - askSum) * 100
				buyMarginColor := ColorWhite
				if buyMargin >= 3 {
					buyMarginColor = ColorGreen
				} else if buyMargin >= 2 {
					buyMarginColor = ColorYellow
				} else if buyMargin < 1 {
					buyMarginColor = ColorRed
				}

				bidSum := bid1 + bid2
				sellMargin := (bidSum - 1.0) * 100
				sellMarginColor := ColorWhite
				if sellMargin >= 3 {
					sellMarginColor = ColorGreen
				} else if sellMargin >= 2 {
					sellMarginColor = ColorYellow
				} else if sellMargin < 1 {
					sellMarginColor = ColorRed
				}

				sb.WriteString(fmt.Sprintf("   📉 BUY: $%.2f | %s%+.1f%%%s  📈 SELL: $%.2f | %s%+.1f%%%s\n",
					askSum, buyMarginColor, buyMargin, Reset,
					bidSum, sellMarginColor, sellMargin, Reset))
				totalMargin += buyMargin
				marketCount++
			} else {
				sb.WriteString(fmt.Sprintf("   📈 %s(waiting for price data...)%s\n", ColorYellow, Reset))
			}
		}
		sb.WriteString("\n")
	}

	// Summary line
	if marketCount > 0 {
		avgMargin := totalMargin / float64(marketCount)
		avgColor := ColorWhite
		if avgMargin >= 2 {
			avgColor = ColorGreen
		} else if avgMargin < 1 {
			avgColor = ColorRed
		}
		sb.WriteString(fmt.Sprintf("%s📊 COMBINED: %d markets | Avg Margin: %s%.1f%%%s%s\n", Bold, marketCount, avgColor, avgMargin, Reset, Reset))
	}

	return sb.String()
}

// renderOrderBookForMarket renders a simple bid/ask display for a single outcome
func (t *TUI) renderOrderBookForMarket(marketID, outcome string, bestBid, bestAsk float64) string {
	var sb strings.Builder

	// Get best bid/ask from depth if available
	depth := t.orderBookDepth[marketID]
	bids := depth[outcome+"_bids"]
	asks := depth[outcome+"_asks"]

	// Use depth data for best prices if available
	if len(bids) > 0 && bids[0].Price > bestBid {
		bestBid = bids[0].Price
	}
	if len(asks) > 0 && asks[0].Price > 0 && (bestAsk == 0 || asks[0].Price < bestAsk) {
		bestAsk = asks[0].Price
	}

	// Format outcome name (truncate if too long)
	displayOutcome := core.SanitizeString(outcome)
	if len(displayOutcome) > 6 {
		displayOutcome = displayOutcome[:6]
	}

	// Simple format: Outcome  Bid | Ask
	sb.WriteString(fmt.Sprintf("   %-6s  ", displayOutcome))

	// Show bid (green)
	if bestBid > 0 {
		sb.WriteString(fmt.Sprintf("%sBid: $%.2f%s", ColorGreen, bestBid, Reset))
	} else {
		sb.WriteString(fmt.Sprintf("%sBid: --.---%s", ColorGreen, Reset))
	}

	sb.WriteString("  │  ")

	// Show ask (red)
	if bestAsk > 0 {
		sb.WriteString(fmt.Sprintf("%sAsk: $%.2f%s", ColorRed, bestAsk, Reset))
	} else {
		sb.WriteString(fmt.Sprintf("%sAsk: --.---%s", ColorRed, Reset))
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

// renderSingleMarketPrices renders price panels for a single market (legacy)
func (t *TUI) renderSingleMarketPrices(outcomes []string, bids, asks, realBids, realAsks map[string]float64) string {
	var sb strings.Builder

	// ══════════════════════════════════════════════════════════════
	// PANEL 1: REAL MARKET (what we see on Polymarket website)
	// ══════════════════════════════════════════════════════════════
	sb.WriteString(fmt.Sprintf("%s┌─ 🌐 REAL MARKET (Polymarket Website) ─────────────────────┐%s\n", ColorCyan, Reset))
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
	sb.WriteString(fmt.Sprintf("%s└────────────────────────────────────────────────────────────┘%s\n", ColorCyan, Reset))

	// ══════════════════════════════════════════════════════════════
	// PANEL 2: BOT READING (what our bot receives from API)
	// ══════════════════════════════════════════════════════════════
	sb.WriteString(fmt.Sprintf("%s┌─ 🤖 BOT READING (REST API Response) ──────────────────────┐%s\n", ColorYellow, Reset))
	bid1 := bids[outcomes[0]]
	ask1 := asks[outcomes[0]]
	bid2 := bids[outcomes[1]]
	ask2 := asks[outcomes[1]]

	// Check for mismatch with real market
	mismatch1 := false
	mismatch2 := false
	if realAsk1 > 0 && (abs(ask1-realAsk1) > 0.05 || abs(bid1-realBid1) > 0.05) {
		mismatch1 = true
	}
	if realAsk2 > 0 && (abs(ask2-realAsk2) > 0.05 || abs(bid2-realBid2) > 0.05) {
		mismatch2 = true
	}

	color1 := ""
	color2 := ""
	if mismatch1 {
		color1 = ColorRed
	}
	if mismatch2 {
		color2 = ColorRed
	}

	sb.WriteString(fmt.Sprintf("│  %s%s: bid $%.2f / ask $%.2f%s", color1, core.SanitizeString(outcomes[0]), bid1, ask1, Reset))
	if mismatch1 {
		sb.WriteString(fmt.Sprintf(" %s⚠️ MISMATCH!%s", ColorRed, Reset))
	}
	sb.WriteString("\n")

	sb.WriteString(fmt.Sprintf("│  %s%s: bid $%.2f / ask $%.2f%s", color2, core.SanitizeString(outcomes[1]), bid2, ask2, Reset))
	if mismatch2 {
		sb.WriteString(fmt.Sprintf(" %s⚠️ MISMATCH!%s", ColorRed, Reset))
	}
	sb.WriteString("\n")

	// Calculate BUY margin (panic buy: when ask_sum < $0.98)
	askSum := ask1 + ask2
	buyMargin := (1.0 - askSum) * 100
	buyMarginColor := ColorWhite
	if buyMargin >= 3 {
		buyMarginColor = ColorGreen
	} else if buyMargin >= 2 {
		buyMarginColor = ColorYellow
	} else if buyMargin < 1 {
		buyMarginColor = ColorRed
	}

	// Calculate SELL margin (panic sell: when bid_sum > $1.03)
	bidSum := bid1 + bid2
	sellMargin := (bidSum - 1.0) * 100
	sellMarginColor := ColorWhite
	if sellMargin >= 3 {
		sellMarginColor = ColorGreen
	} else if sellMargin >= 2 {
		sellMarginColor = ColorYellow
	} else if sellMargin < 1 {
		sellMarginColor = ColorRed
	}

	sb.WriteString(fmt.Sprintf("│  📉 BUY:  ask_sum=$%.2f | %sMargin: %+.1f%%%s\n", askSum, buyMarginColor, buyMargin, Reset))
	sb.WriteString(fmt.Sprintf("│  📈 SELL: bid_sum=$%.2f | %sMargin: %+.1f%%%s\n", bidSum, sellMarginColor, sellMargin, Reset))
	sb.WriteString(fmt.Sprintf("%s└────────────────────────────────────────────────────────────┘%s\n", ColorYellow, Reset))

	// ══════════════════════════════════════════════════════════════
	// PANEL 3: BOT ORDERS (what orders the bot will place)
	// ══════════════════════════════════════════════════════════════
	sb.WriteString(fmt.Sprintf("%s┌─ 📋 BOT PLANNED ORDERS ───────────────────────────────────┐%s\n", ColorGreen, Reset))
	if len(t.pendingOrders) > 0 {
		for outcome, orders := range t.pendingOrders {
			for _, o := range orders {
				sb.WriteString(fmt.Sprintf("│  %s %s: %.0f shares @ $%.2f\n", o.Side, core.SanitizeString(outcome), o.Qty, o.Price))
			}
		}
	} else {
		sb.WriteString("│  (no pending orders)\n")
	}
	sb.WriteString(fmt.Sprintf("%s└────────────────────────────────────────────────────────────┘%s\n", ColorGreen, Reset))

	return sb.String()
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func (t *TUI) renderAccountStatus(stats Stats, totalExposure, equity, multiplier float64, rounds, profitable int, positions map[string]Position) string {
	var sb strings.Builder

	netChange := equity - stats.StartingBalance
	changeColor := ColorGreen
	changeSign := "+"
	if netChange < 0 {
		changeColor = ColorRed
		changeSign = ""
	}

	multColor := ColorWhite
	if multiplier >= 1.5 {
		multColor = ColorGreen
	} else if multiplier > 1.0 {
		multColor = ColorYellow
	}

	// Calculate guaranteed arbitrage profit across all markets
	// Group positions by market
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

	// Format guaranteed profit
	arbColor := ColorGreen
	arbSign := "+"
	if guaranteedProfit < 0 {
		arbColor = ColorRed
		arbSign = ""
	}

	sb.WriteString(fmt.Sprintf("%s💼 ACCOUNT%s\n", Bold, Reset))
	sb.WriteString(fmt.Sprintf("   💵 Cash:     $%.2f\n", stats.CurrentBalance))
	sb.WriteString(fmt.Sprintf("   📦 Exposure: $%.2f\n", totalExposure))
	sb.WriteString(fmt.Sprintf("   💰 Equity:   $%.2f (%s%s$%.2f%s)\n",
		equity, changeColor, changeSign, netChange, Reset))
	sb.WriteString(fmt.Sprintf("   📊 Realized: $%.2f | 🎯 Arb Profit: %s%s$%.2f%s\n",
		stats.RealizedPnL, arbColor, arbSign, guaranteedProfit, Reset))
	sb.WriteString(fmt.Sprintf("   📈 Compound: %s%.2fx%s | Rounds: %d (%d profitable)\n",
		multColor, multiplier, Reset, rounds, profitable))

	uptime := time.Since(t.startTime).Round(time.Second)
	sb.WriteString(fmt.Sprintf("   ⏱️  Uptime:   %v\n", uptime))

	return sb.String()
}

func (t *TUI) renderPositions(positionsWithPnL map[string]PositionPnL) string {
	var sb strings.Builder

	// Check if we have any positions or split inventory to show
	splitPositions := t.getSplitPositions()
	hasPositions := len(positionsWithPnL) > 0
	hasSplitInventory := len(splitPositions) > 0

	// If no positions and no split inventory, show minimal output
	if !hasPositions && !hasSplitInventory {
		sb.WriteString(fmt.Sprintf("%s📦 POSITIONS%s (none)\n", Bold, Reset))
		return sb.String()
	}

	// Show in-flight positions (awaiting merge)
	if hasPositions {
		sb.WriteString(fmt.Sprintf("%s📦 IN-FLIGHT%s", Bold, Reset))
		sb.WriteString(fmt.Sprintf(" (%d) %s⏳ awaiting merge%s\n", len(positionsWithPnL), ColorYellow, Reset))
	} else if hasSplitInventory {
		// Show header even when only split inventory exists
		sb.WriteString(fmt.Sprintf("%s📦 POSITIONS%s\n", Bold, Reset))
	}

	// Group positions by market
	byMarket := make(map[string][]PositionPnL)
	for _, pos := range positionsWithPnL {
		marketID := pos.MarketID
		if marketID == "" {
			marketID = "UNKNOWN"
		}
		byMarket[marketID] = append(byMarket[marketID], pos)
	}

	// Define asset order and colors
	assetOrder := []string{"BTC", "ETH", "SOL", "XRP", "UNKNOWN"}
	assetColors := map[string]string{
		"BTC":     ColorYellow,
		"ETH":     ColorCyan,
		"SOL":     ColorMagenta,
		"XRP":     ColorGreen,
		"UNKNOWN": ColorWhite,
	}

	totalMarketPnL := 0.0
	totalLockedPnL := 0.0
	hasMarketPrices := false

	for _, marketID := range assetOrder {
		marketPositions, ok := byMarket[marketID]
		if !ok || len(marketPositions) == 0 {
			continue
		}

		// Get color for this market
		color := assetColors[marketID]
		if color == "" {
			color = ColorWhite
		}

		sb.WriteString(fmt.Sprintf("   %s[%s]%s ", color, marketID, Reset))

		// Sort positions: "Down" before "Up" for consistent display
		sort.Slice(marketPositions, func(i, j int) bool {
			return marketPositions[i].Outcome < marketPositions[j].Outcome
		})

		// Display each position for this market
		positionStrs := make([]string, 0, len(marketPositions))
		for _, pos := range marketPositions {
			posStr := fmt.Sprintf("%s: %.0f@$%.2f", core.SanitizeString(pos.Outcome), pos.Quantity, pos.AvgPrice)
			// Show current bid if available
			if pos.CurrentBid > 0 {
				bidColor := ColorGreen
				if pos.CurrentBid < pos.AvgPrice {
					bidColor = ColorRed
				}
				posStr += fmt.Sprintf(" (%snow:$%.2f%s)", bidColor, pos.CurrentBid, Reset)
			}
			positionStrs = append(positionStrs, posStr)
		}
		sb.WriteString(strings.Join(positionStrs, " | "))

		// Calculate P&L for this market's matched pairs
		if len(marketPositions) == 2 {
			pos1 := marketPositions[0]
			pos2 := marketPositions[1]
			matchedQty := pos1.Quantity
			if pos2.Quantity < matchedQty {
				matchedQty = pos2.Quantity
			}
			if matchedQty > 0 {
				// Locked P&L: guaranteed $1 payout at resolution
				matchedCost := (pos1.AvgPrice + pos2.AvgPrice) * matchedQty
				lockedProfit := (matchedQty * 1.0) - matchedCost
				totalLockedPnL += lockedProfit

				// Market P&L: what we'd get if we sold NOW at current bids
				marketProfit := 0.0
				if pos1.CurrentBid > 0 && pos2.CurrentBid > 0 {
					marketValue := (pos1.CurrentBid + pos2.CurrentBid) * matchedQty
					marketProfit = marketValue - matchedCost
					totalMarketPnL += marketProfit
					hasMarketPrices = true

					// Show market P&L (real-time)
					mktColor := ColorGreen
					mktSign := "+"
					if marketProfit < 0 {
						mktColor = ColorRed
						mktSign = ""
					}
					sb.WriteString(fmt.Sprintf(" → %s%s$%.2f%s", mktColor, mktSign, marketProfit, Reset))
				} else {
					// Fallback to locked P&L
					lckColor := ColorGreen
					lckSign := "+"
					if lockedProfit < 0 {
						lckColor = ColorRed
						lckSign = ""
					}
					sb.WriteString(fmt.Sprintf(" → 🔒%s%s$%.2f%s", lckColor, lckSign, lockedProfit, Reset))
				}
			}
		}
		sb.WriteString("\n")
	}

	// Show total P&L
	if hasMarketPrices {
		// Show both market and locked P&L
		mktColor := ColorGreen
		mktSign := "+"
		if totalMarketPnL < 0 {
			mktColor = ColorRed
			mktSign = ""
		}
		lckColor := ColorGreen
		lckSign := "+"
		if totalLockedPnL < 0 {
			lckColor = ColorRed
			lckSign = ""
		}
		sb.WriteString(fmt.Sprintf("   %s📊 Now: %s%s$%.2f%s | 🔒 Locked: %s%s$%.2f%s%s\n",
			Bold, mktColor, mktSign, totalMarketPnL, Reset,
			lckColor, lckSign, totalLockedPnL, Reset, Reset))
	} else if totalLockedPnL != 0 {
		lckColor := ColorGreen
		lckSign := "+"
		if totalLockedPnL < 0 {
			lckColor = ColorRed
			lckSign = ""
		}
		sb.WriteString(fmt.Sprintf("   %s🔒 Locked Profit: %s%s$%.2f%s%s\n",
			Bold, lckColor, lckSign, totalLockedPnL, Reset, Reset))
	}

	// Show split inventory positions (for panic sell strategy)
	if hasSplitInventory {
		sb.WriteString(fmt.Sprintf("\n%s🔀 SPLIT INVENTORY%s (panic sell)\n", Bold, Reset))

		// Group split positions by market
		splitByMarket := make(map[string][]SplitPosition)
		for _, sp := range splitPositions {
			splitByMarket[sp.MarketID] = append(splitByMarket[sp.MarketID], sp)
		}

		for _, marketID := range assetOrder {
			positions, ok := splitByMarket[marketID]
			if !ok || len(positions) == 0 {
				continue
			}

			color := assetColors[marketID]
			if color == "" {
				color = ColorWhite
			}

			sb.WriteString(fmt.Sprintf("   %s[%s]%s ", color, marketID, Reset))

			// Sort positions: "Up" before "Down" for consistent display
			sort.Slice(positions, func(i, j int) bool {
				return positions[i].Outcome < positions[j].Outcome
			})

			posStrs := make([]string, 0, len(positions))
			for _, sp := range positions {
				posStrs = append(posStrs, fmt.Sprintf("%s: %.0f@$%.2f", core.SanitizeString(sp.Outcome), sp.Shares, sp.CostBasis))
			}
			sb.WriteString(strings.Join(posStrs, " | "))

			// Show min matched (sellable pairs)
			if len(positions) >= 2 {
				minShares := positions[0].Shares
				for _, p := range positions[1:] {
					if p.Shares < minShares {
						minShares = p.Shares
					}
				}
				sb.WriteString(fmt.Sprintf(" → %s%.0f pairs sellable%s", ColorGreen, minShares, Reset))
			}
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

func (t *TUI) renderOrders(orders []*LimitOrder) string {
	// Only show this section if there are actually open orders
	// The current strategy uses market orders, not limit orders,
	// so this section is typically empty
	if len(orders) == 0 {
		return "" // Don't show empty section
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s📝 LIMIT ORDERS%s", Bold, Reset))
	sb.WriteString(fmt.Sprintf(" (%d)\n", len(orders)))

	// Group by outcome
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

func (t *TUI) renderEventLog() string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("%s📜 EVENTS%s\n", Bold, Reset))

	if len(t.eventLog) == 0 {
		sb.WriteString("   (waiting for events...)\n")
		return sb.String()
	}

	for _, event := range t.eventLog {
		sb.WriteString(fmt.Sprintf("   %s\n", event))
	}

	return sb.String()
}

func (t *TUI) renderOrderHistory() string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("%s📋 ORDER HISTORY%s", Bold, Reset))

	if len(t.orderHistory) == 0 {
		sb.WriteString(" (no trades yet)\n")
		return sb.String()
	}

	sb.WriteString(fmt.Sprintf(" (last %d)\n", len(t.orderHistory)))

	// Show most recent orders first (reversed)
	displayCount := len(t.orderHistory)
	if displayCount > 8 {
		displayCount = 8 // Show max 8 in UI
	}

	for i := len(t.orderHistory) - 1; i >= len(t.orderHistory)-displayCount && i >= 0; i-- {
		o := t.orderHistory[i]

		// Color based on status
		statusColor := ColorGreen
		statusIcon := "✅"
		if o.Status == "FAILED" {
			statusColor = ColorRed
			statusIcon = "❌"
		} else if o.Status == "PARTIAL" {
			statusColor = ColorYellow
			statusIcon = "⚠️"
		}

		// Asset color
		assetColor := ColorWhite
		switch o.MarketID {
		case "BTC":
			assetColor = ColorYellow
		case "ETH":
			assetColor = ColorCyan
		case "SOL":
			assetColor = ColorMagenta
		case "XRP":
			assetColor = ColorGreen
		}

		// Format timestamp (just time, not date)
		timeStr := o.Timestamp.Format("15:04:05")

		// Format the entry
		sb.WriteString(fmt.Sprintf("   %s %s[%s]%s %s %-6s %.0f @ $%.2f ($%.1f) %s%.1f%%%s\n",
			timeStr,
			assetColor, o.MarketID, Reset,
			statusIcon,
			core.SanitizeString(o.Outcome),
			o.Shares,
			o.Price,
			o.Cost,
			statusColor, o.Margin, Reset))
	}

	return sb.String()
}

func (t *TUI) renderKillBanner() string {
	var sb strings.Builder

	// Helper to clamp padding to non-negative
	pad := func(n int) string {
		if n < 0 {
			n = 0
		}
		return strings.Repeat(" ", n)
	}

	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf("%s%s", BgRed, Bold))
	sb.WriteString(pad(t.width) + "\n")
	// "   🚨 KILL SWITCH ACTIVATED 🚨" displays as ~31 chars (emojis = 2 each)
	sb.WriteString("   🚨 KILL SWITCH ACTIVATED 🚨" + pad(t.width-31) + "\n")
	// "   Reason: " = 12 chars display width
	reasonPad := t.width - 12 - len(t.killReason)
	sb.WriteString(fmt.Sprintf("   Reason: %s%s\n", t.killReason, pad(reasonPad)))
	sb.WriteString(pad(t.width) + "\n")
	sb.WriteString(Reset)

	return sb.String()
}

// StartRenderLoop starts a goroutine that renders the UI periodically
func (t *TUI) StartRenderLoop(interval time.Duration) {
	// Start the dedicated frame writer goroutine
	// This handles terminal output in a separate goroutine so blocking I/O
	// doesn't affect the main application
	go t.frameWriter()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		// Hide cursor and clear screen once at start
		fmt.Print(HideCursor)
		fmt.Print(ClearScreen)
		fmt.Print(fmt.Sprintf(MoveCursor, 1, 1))

		for {
			select {
			case <-t.stopCh:
				fmt.Print(ShowCursor)
				fmt.Print(ClearScreen)
				fmt.Print(fmt.Sprintf(MoveCursor, 1, 1))
				return
			case <-ticker.C:
				t.mu.Lock()
				running := t.running
				t.mu.Unlock()

				if !running {
					fmt.Print(ShowCursor)
					return
				}
				t.Render()
			}
		}
	}()
}

// frameWriter is a dedicated goroutine that writes frames to the terminal
// This isolates blocking terminal I/O from the rest of the application
func (t *TUI) frameWriter() {
	for {
		select {
		case <-t.stopCh:
			// Drain any remaining frames quickly
			for {
				select {
				case <-t.frameCh:
				default:
					return
				}
			}
		case frame, ok := <-t.frameCh:
			if !ok {
				return
			}
			// Check if we should stop before writing
			if t.stopped.Load() {
				return
			}
			// Write frame to terminal - this may block if terminal is slow
			// but only this goroutine is affected
			fmt.Print(frame)
		}
	}
}
