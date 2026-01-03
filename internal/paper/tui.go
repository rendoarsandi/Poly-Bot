package paper

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// ANSI escape codes for terminal control
const (
	ClearScreen    = "\033[2J"
	MoveCursor     = "\033[%d;%dH" // row, col
	ClearLine      = "\033[2K"
	HideCursor     = "\033[?25l"
	ShowCursor     = "\033[?25h"
	Bold           = "\033[1m"
	Reset          = "\033[0m"
	ColorRed       = "\033[31m"
	ColorGreen     = "\033[32m"
	ColorYellow    = "\033[33m"
	ColorBlue      = "\033[34m"
	ColorMagenta   = "\033[35m"
	ColorCyan      = "\033[36m"
	ColorWhite     = "\033[37m"
	BgRed          = "\033[41m"
	BgGreen        = "\033[42m"
	BgYellow       = "\033[43m"
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
}

// TUI provides a live terminal user interface
type TUI struct {
	mu sync.Mutex

	// References
	engine    *Engine
	orderBook *OrderBook

	// State - now supports multiple markets
	markets      map[string]*MarketData // key = market identifier (e.g., "ETH", "SOL")
	marketSlug   string                 // Legacy - primary market
	outcomes     []string               // Legacy - primary market outcomes
	endTime      time.Time              // Legacy - primary market end time
	lastPrices   map[string]float64
	lastBids     map[string]float64
	lastAsks     map[string]float64
	eventLog     []string
	maxEvents    int
	running      bool
	killReason   string
	isKilled     bool

	// Real market data (for comparison)
	realBids map[string]float64
	realAsks map[string]float64

	// Bot's intended orders (before placement)
	pendingOrders map[string][]PendingOrder

	// Order book depth per market
	orderBookDepth map[string]map[string][]MarketLevel // marketID -> outcome -> levels

	// Display dimensions
	width int

	// Stop channel for clean shutdown
	stopCh chan struct{}
}

// PendingOrder represents an order the bot intends to place
type PendingOrder struct {
	Outcome string
	Price   float64
	Qty     float64
	Side    string // "BUY" or "SELL"
}

// NewTUI creates a new terminal UI
func NewTUI(engine *Engine, orderBook *OrderBook) *TUI {
	return &TUI{
		engine:         engine,
		orderBook:      orderBook,
		markets:        make(map[string]*MarketData),
		lastPrices:     make(map[string]float64),
		lastBids:       make(map[string]float64),
		lastAsks:       make(map[string]float64),
		realBids:       make(map[string]float64),
		realAsks:       make(map[string]float64),
		pendingOrders:  make(map[string][]PendingOrder),
		orderBookDepth: make(map[string]map[string][]MarketLevel),
		eventLog:       make([]string, 0),
		maxEvents:      10,
		width:          80,
		running:        true,
		stopCh:         make(chan struct{}),
	}
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
}

// UpdateMarketPrices updates prices for a specific market
func (t *TUI) UpdateMarketPrices(marketID string, bids, asks map[string]float64) {
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

// Stop stops the UI
func (t *TUI) Stop() {
	t.mu.Lock()
	wasRunning := t.running
	t.running = false
	t.mu.Unlock()

	// Signal stop channel only once
	if wasRunning {
		close(t.stopCh)
		// Restore cursor and clear screen position
		fmt.Print(ShowCursor)
		fmt.Println() // Move to new line for clean exit
	}
}

// Render draws the entire UI
func (t *TUI) Render() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.running {
		return
	}

	var sb strings.Builder

	// Clear screen and move to top
	sb.WriteString(ClearScreen)
	sb.WriteString(fmt.Sprintf(MoveCursor, 1, 1))

	// Header
	sb.WriteString(t.renderHeader())
	sb.WriteString("\n")

	// Market Info
	sb.WriteString(t.renderMarketInfo())
	sb.WriteString("\n")

	// Account Status
	sb.WriteString(t.renderAccountStatus())
	sb.WriteString("\n")

	// Positions
	sb.WriteString(t.renderPositions())
	sb.WriteString("\n")

	// Open Orders
	sb.WriteString(t.renderOrders())
	sb.WriteString("\n")

	// Event Log
	sb.WriteString(t.renderEventLog())

	// Kill switch banner if triggered
	if t.isKilled {
		sb.WriteString(t.renderKillBanner())
	}

	fmt.Print(sb.String())
}

func (t *TUI) renderHeader() string {
	line := strings.Repeat("═", t.width)
	title := " 🎰 POLYARB-15M MULTI-ASSET PAPER TRADING "
	padding := (t.width - len(title)) / 2
	if padding < 0 {
		padding = 0
	}

	return fmt.Sprintf("%s%s\n%s%s%s\n%s",
		Bold, line,
		strings.Repeat(" ", padding), title, Reset,
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
		sb.WriteString(fmt.Sprintf("   📊 %s\n", m.Slug))
		sb.WriteString(fmt.Sprintf("   ⏱️  Time: %s%v%s remaining\n", timeColor, remaining.Round(time.Second), Reset))

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
			if ask1 > 0 && ask2 > 0 {
				sum := ask1 + ask2
				margin := (1.0 - sum) * 100
				marginColor := ColorWhite
				if margin >= 3 {
					marginColor = ColorGreen
				} else if margin >= 2 {
					marginColor = ColorYellow
				} else if margin < 1 {
					marginColor = ColorRed
				}
				sb.WriteString(fmt.Sprintf("   📈 Sum: $%.3f | %sMargin: %.1f%%%s\n", sum, marginColor, margin, Reset))
				totalMargin += margin
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

// renderOrderBookForMarket renders a compact order book display for a single outcome
func (t *TUI) renderOrderBookForMarket(marketID, outcome string, bestBid, bestAsk float64) string {
	var sb strings.Builder

	// Get order book depth if available
	depth := t.orderBookDepth[marketID]
	bids := depth[outcome+"_bids"]
	asks := depth[outcome+"_asks"]

	// Format outcome name (truncate if too long)
	displayOutcome := outcome
	if len(displayOutcome) > 6 {
		displayOutcome = displayOutcome[:6]
	}

	// Show outcome with best bid/ask and depth
	sb.WriteString(fmt.Sprintf("   %s%-6s%s ", Bold, displayOutcome, Reset))

	// Show bids (green, right-aligned) - up to 3 levels
	sb.WriteString(fmt.Sprintf("%s", ColorGreen))
	if len(bids) > 0 {
		// Show levels from worst to best (so best is closest to spread)
		for i := min(2, len(bids)-1); i >= 0; i-- {
			sb.WriteString(fmt.Sprintf("%.0f@%.2f ", bids[i].Size, bids[i].Price))
		}
	} else if bestBid > 0 {
		sb.WriteString(fmt.Sprintf("bid:%.3f ", bestBid))
	}
	sb.WriteString(Reset)

	// Spread indicator
	sb.WriteString(fmt.Sprintf("%s│%s ", ColorWhite, Reset))

	// Show asks (red) - up to 3 levels
	sb.WriteString(fmt.Sprintf("%s", ColorRed))
	if len(asks) > 0 {
		for i := 0; i < min(3, len(asks)); i++ {
			sb.WriteString(fmt.Sprintf("%.2f@%.0f ", asks[i].Price, asks[i].Size))
		}
	} else if bestAsk > 0 {
		sb.WriteString(fmt.Sprintf("ask:%.3f ", bestAsk))
	}
	sb.WriteString(Reset)

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
		sb.WriteString(fmt.Sprintf("│  %s: bid $%.3f / ask $%.3f\n", outcomes[0], realBid1, realAsk1))
		sb.WriteString(fmt.Sprintf("│  %s: bid $%.3f / ask $%.3f\n", outcomes[1], realBid2, realAsk2))
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

	sb.WriteString(fmt.Sprintf("│  %s%s: bid $%.3f / ask $%.3f%s", color1, outcomes[0], bid1, ask1, Reset))
	if mismatch1 {
		sb.WriteString(fmt.Sprintf(" %s⚠️ MISMATCH!%s", ColorRed, Reset))
	}
	sb.WriteString("\n")

	sb.WriteString(fmt.Sprintf("│  %s%s: bid $%.3f / ask $%.3f%s", color2, outcomes[1], bid2, ask2, Reset))
	if mismatch2 {
		sb.WriteString(fmt.Sprintf(" %s⚠️ MISMATCH!%s", ColorRed, Reset))
	}
	sb.WriteString("\n")

	// Calculate margin
	sum := ask1 + ask2
	margin := (1.0 - sum) * 100
	marginColor := ColorWhite
	if margin >= 3 {
		marginColor = ColorGreen
	} else if margin >= 2 {
		marginColor = ColorYellow
	} else if margin < 1 {
		marginColor = ColorRed
	}
	sb.WriteString(fmt.Sprintf("│  📈 Ask Sum: %.3f | %sMargin: %.1f%%%s\n", sum, marginColor, margin, Reset))
	sb.WriteString(fmt.Sprintf("%s└────────────────────────────────────────────────────────────┘%s\n", ColorYellow, Reset))

	// ══════════════════════════════════════════════════════════════
	// PANEL 3: BOT ORDERS (what orders the bot will place)
	// ══════════════════════════════════════════════════════════════
	sb.WriteString(fmt.Sprintf("%s┌─ 📋 BOT PLANNED ORDERS ───────────────────────────────────┐%s\n", ColorGreen, Reset))
	if len(t.pendingOrders) > 0 {
		for outcome, orders := range t.pendingOrders {
			for _, o := range orders {
				sb.WriteString(fmt.Sprintf("│  %s %s: %.0f shares @ $%.3f\n", o.Side, outcome, o.Qty, o.Price))
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

func (t *TUI) renderAccountStatus() string {
	var sb strings.Builder

	stats := t.engine.GetStats()
	totalExposure, _ := t.engine.GetExposure()
	equity := t.engine.GetEquity()

	netChange := equity - stats.StartingBalance
	changeColor := ColorGreen
	changeSign := "+"
	if netChange < 0 {
		changeColor = ColorRed
		changeSign = ""
	}

	// Get compounding stats
	multiplier, rounds, profitable := t.engine.GetCompoundStats()
	multColor := ColorWhite
	if multiplier >= 1.5 {
		multColor = ColorGreen
	} else if multiplier > 1.0 {
		multColor = ColorYellow
	}

	sb.WriteString(fmt.Sprintf("%s💼 ACCOUNT%s\n", Bold, Reset))
	sb.WriteString(fmt.Sprintf("   💵 Cash:     $%.2f\n", stats.CurrentBalance))
	sb.WriteString(fmt.Sprintf("   📦 Exposure: $%.2f\n", totalExposure))
	sb.WriteString(fmt.Sprintf("   💰 Equity:   $%.2f (%s%s$%.2f%s)\n",
		equity, changeColor, changeSign, netChange, Reset))
	sb.WriteString(fmt.Sprintf("   📊 PnL:      Realized: $%.2f | Unrealized: $%.2f\n",
		stats.RealizedPnL, stats.UnrealizedPnL))
	sb.WriteString(fmt.Sprintf("   📈 Compound: %s%.2fx%s | Rounds: %d (%d profitable)\n",
		multColor, multiplier, Reset, rounds, profitable))

	return sb.String()
}

func (t *TUI) renderPositions() string {
	var sb strings.Builder

	positions := t.engine.GetPositions()

	sb.WriteString(fmt.Sprintf("%s📦 POSITIONS%s", Bold, Reset))

	if len(positions) == 0 {
		sb.WriteString(" (none)\n")
		return sb.String()
	}
	sb.WriteString("\n")

	// Calculate matched pairs for arbitrage display
	var matchedQty float64
	var matchedCost float64
	var outcomes []string
	for outcome := range positions {
		outcomes = append(outcomes, outcome)
	}

	// Find matched quantity (minimum of all positions)
	if len(positions) == 2 && len(outcomes) == 2 {
		pos1 := positions[outcomes[0]]
		pos2 := positions[outcomes[1]]
		matchedQty = pos1.Quantity
		if pos2.Quantity < matchedQty {
			matchedQty = pos2.Quantity
		}
		// Cost of matched pairs
		matchedCost = (pos1.AvgPrice + pos2.AvgPrice) * matchedQty
	}

	for outcome, pos := range positions {
		sb.WriteString(fmt.Sprintf("   • %s: %.0f @ $%.3f avg (cost $%.2f)\n",
			outcome, pos.Quantity, pos.AvgPrice, pos.TotalCost))
	}

	// Show arbitrage profit for matched pairs
	if matchedQty > 0 {
		// Matched pairs pay $1 per share at resolution
		guaranteedPayout := matchedQty * 1.0
		guaranteedProfit := guaranteedPayout - matchedCost

		profitColor := ColorGreen
		profitSign := "+"
		if guaranteedProfit < 0 {
			profitColor = ColorRed
			profitSign = ""
		}

		sb.WriteString(fmt.Sprintf("   🎯 Matched: %.0f pairs | Payout: $%.2f | %sProfit: %s$%.2f%s\n",
			matchedQty, guaranteedPayout, profitColor, profitSign, guaranteedProfit, Reset))

		// Show unmatched shares (risky exposure)
		for outcome, pos := range positions {
			unmatched := pos.Quantity - matchedQty
			if unmatched > 0 {
				sb.WriteString(fmt.Sprintf("   ⚠️  Unmatched %s: %.0f shares (risky)\n", outcome, unmatched))
			}
		}
	}

	return sb.String()
}

func (t *TUI) renderOrders() string {
	var sb strings.Builder

	orders := t.orderBook.GetOpenOrders()

	sb.WriteString(fmt.Sprintf("%s📝 OPEN ORDERS%s", Bold, Reset))

	if len(orders) == 0 {
		sb.WriteString(" (none)\n")
		return sb.String()
	}
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
			outcome, len(ords), totalQty, totalVal))
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

func (t *TUI) renderKillBanner() string {
	var sb strings.Builder

	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf("%s%s", BgRed, Bold))
	sb.WriteString(strings.Repeat(" ", t.width) + "\n")
	sb.WriteString("   🚨 KILL SWITCH ACTIVATED 🚨" + strings.Repeat(" ", t.width-31) + "\n")
	sb.WriteString(fmt.Sprintf("   Reason: %s%s\n", t.killReason, strings.Repeat(" ", t.width-12-len(t.killReason))))
	sb.WriteString(strings.Repeat(" ", t.width) + "\n")
	sb.WriteString(Reset)

	return sb.String()
}

// StartRenderLoop starts a goroutine that renders the UI periodically
func (t *TUI) StartRenderLoop(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		// Hide cursor
		fmt.Print(HideCursor)

		for {
			select {
			case <-t.stopCh:
				fmt.Print(ShowCursor)
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
