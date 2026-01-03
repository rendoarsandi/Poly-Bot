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

// TUI provides a live terminal user interface
type TUI struct {
	mu sync.Mutex

	// References
	engine    *Engine
	orderBook *OrderBook

	// State
	marketSlug   string
	outcomes     []string
	endTime      time.Time
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

	// Display dimensions
	width int
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
		engine:        engine,
		orderBook:     orderBook,
		lastPrices:    make(map[string]float64),
		lastBids:      make(map[string]float64),
		lastAsks:      make(map[string]float64),
		realBids:      make(map[string]float64),
		realAsks:      make(map[string]float64),
		pendingOrders: make(map[string][]PendingOrder),
		eventLog:      make([]string, 0),
		maxEvents:     8,
		width:         80,
		running:       true,
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
	defer t.mu.Unlock()
	t.running = false
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
	title := " 🎰 POLYARB-15M PAPER TRADING BOT "
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

		// ══════════════════════════════════════════════════════════════
		// PANEL 1: REAL MARKET (what we see on Polymarket website)
		// ══════════════════════════════════════════════════════════════
		sb.WriteString(fmt.Sprintf("%s┌─ 🌐 REAL MARKET (Polymarket Website) ─────────────────────┐%s\n", ColorCyan, Reset))
		realBid1 := t.realBids[t.outcomes[0]]
		realAsk1 := t.realAsks[t.outcomes[0]]
		realBid2 := t.realBids[t.outcomes[1]]
		realAsk2 := t.realAsks[t.outcomes[1]]

		if realAsk1 > 0 || realAsk2 > 0 {
			sb.WriteString(fmt.Sprintf("│  %s: bid $%.3f / ask $%.3f\n", t.outcomes[0], realBid1, realAsk1))
			sb.WriteString(fmt.Sprintf("│  %s: bid $%.3f / ask $%.3f\n", t.outcomes[1], realBid2, realAsk2))
		} else {
			sb.WriteString("│  (waiting for real market data...)\n")
		}
		sb.WriteString(fmt.Sprintf("%s└────────────────────────────────────────────────────────────┘%s\n", ColorCyan, Reset))

		// ══════════════════════════════════════════════════════════════
		// PANEL 2: BOT READING (what our bot receives from API)
		// ══════════════════════════════════════════════════════════════
		sb.WriteString(fmt.Sprintf("%s┌─ 🤖 BOT READING (REST API Response) ──────────────────────┐%s\n", ColorYellow, Reset))
		bid1 := t.lastBids[t.outcomes[0]]
		ask1 := t.lastAsks[t.outcomes[0]]
		bid2 := t.lastBids[t.outcomes[1]]
		ask2 := t.lastAsks[t.outcomes[1]]

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

		sb.WriteString(fmt.Sprintf("│  %s%s: bid $%.3f / ask $%.3f%s", color1, t.outcomes[0], bid1, ask1, Reset))
		if mismatch1 {
			sb.WriteString(fmt.Sprintf(" %s⚠️ MISMATCH!%s", ColorRed, Reset))
		}
		sb.WriteString("\n")

		sb.WriteString(fmt.Sprintf("│  %s%s: bid $%.3f / ask $%.3f%s", color2, t.outcomes[1], bid2, ask2, Reset))
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
	}

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

	sb.WriteString(fmt.Sprintf("%s💼 ACCOUNT%s\n", Bold, Reset))
	sb.WriteString(fmt.Sprintf("   💵 Cash:     $%.2f\n", stats.CurrentBalance))
	sb.WriteString(fmt.Sprintf("   📦 Exposure: $%.2f\n", totalExposure))
	sb.WriteString(fmt.Sprintf("   💰 Equity:   $%.2f (%s%s$%.2f%s)\n",
		equity, changeColor, changeSign, netChange, Reset))
	sb.WriteString(fmt.Sprintf("   📊 PnL:      Realized: $%.2f | Unrealized: $%.2f\n",
		stats.RealizedPnL, stats.UnrealizedPnL))

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

	for outcome, pos := range positions {
		price := t.lastPrices[outcome]
		unrealized := (price * pos.Quantity) - pos.TotalCost

		pnlColor := ColorGreen
		pnlSign := "+"
		if unrealized < 0 {
			pnlColor = ColorRed
			pnlSign = ""
		}

		sb.WriteString(fmt.Sprintf("   • %s: %.0f @ $%.3f avg | %s%s$%.2f%s\n",
			outcome, pos.Quantity, pos.AvgPrice,
			pnlColor, pnlSign, unrealized, Reset))
	}

	// Show balance
	if len(t.outcomes) == 2 {
		pos1 := positions[t.outcomes[0]]
		pos2 := positions[t.outcomes[1]]
		unmatched := pos1.Quantity - pos2.Quantity
		if unmatched < 0 {
			unmatched = -unmatched
		}

		balanceColor := ColorGreen
		if unmatched > 50 {
			balanceColor = ColorYellow
		}
		if unmatched > 75 {
			balanceColor = ColorRed
		}

		sb.WriteString(fmt.Sprintf("   ⚖️  Unmatched: %s%.0f shares%s\n",
			balanceColor, unmatched, Reset))
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
		defer fmt.Print(ShowCursor)

		for range ticker.C {
			t.mu.Lock()
			running := t.running
			t.mu.Unlock()

			if !running {
				break
			}
			t.Render()
		}
	}()
}
