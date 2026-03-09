package paper

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"Market-bot/internal/core"
	tea "github.com/charmbracelet/bubbletea"
)

func TestTUI_RegisterSplitInventory(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)

	inv1 := NewSplitInventory()
	inv2 := NewSplitInventory()

	// Register inventories
	tui.RegisterSplitInventory(inv1)
	tui.RegisterSplitInventory(inv2)

	// Verify they were registered
	if len(tui.splitInventories) != 2 {
		t.Errorf("Expected 2 split inventories, got %d", len(tui.splitInventories))
	}
}

func TestTUI_SetWalletTruthPositions(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)

	tui.SetWalletTruthPositions("SOL", []WalletTruthPosition{{
		MarketID:      "SOL",
		Outcome:       "Up",
		LocalShares:   0.9311,
		OnChainShares: 0.9311,
		Drift:         0,
	}})

	positions := tui.getWalletTruthPositions()
	if len(positions) != 1 {
		t.Fatalf("expected 1 wallet truth position, got %d", len(positions))
	}
	if positions[0].MarketID != "SOL" || positions[0].Outcome != "Up" {
		t.Fatalf("unexpected wallet truth position: %+v", positions[0])
	}

	tui.ClearWalletTruthPositions("SOL")
	if got := tui.getWalletTruthPositions(); len(got) != 0 {
		t.Fatalf("expected wallet truth positions to clear, got %d", len(got))
	}
}

func TestTUI_SetWalletTruthPositionsClonesInput(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)

	positions := []WalletTruthPosition{{MarketID: "BTC", Outcome: "Yes", LocalShares: 1, OnChainShares: 1.25, Drift: 0.25}}
	tui.SetWalletTruthPositions("BTC", positions)
	positions[0].OnChainShares = 99

	got := tui.getWalletTruthPositions()
	if len(got) != 1 {
		t.Fatalf("expected 1 wallet truth position, got %d", len(got))
	}
	if got[0].OnChainShares != 1.25 {
		t.Fatalf("expected stored snapshot to be cloned, got %+v", got[0])
	}
}

func TestShouldPersistIssueEvent(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want bool
	}{
		{name: "critical reject", msg: "[12:00:00] [BTC] ❌ Side 1 MARKET Fail: order rejected", want: true},
		{name: "unbalanced cleanup", msg: "[12:00:00] [ETH] ⚠️ ARB UNBALANCED: YES still not filled (legging to auto-cleanup)", want: true},
		{name: "normal info", msg: "[12:00:00] [BTC] ✅ Side 1 MARKET: YES (Observed $0.42, Filled: 5.00/5.00)", want: false},
		{name: "discovery noise", msg: "[12:00:00] 🔍 Searching for active markets based on live settings...", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldPersistIssueEvent(tt.msg); got != tt.want {
				t.Fatalf("shouldPersistIssueEvent(%q) = %v, want %v", tt.msg, got, tt.want)
			}
		})
	}
}

func TestTUILogEvent_WritesOnlyCriticalEventsToIssueLog(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)

	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "realbot-issues.csv")
	logger, err := core.NewCSVLogger(logPath)
	if err != nil {
		t.Fatalf("NewCSVLogger() error = %v", err)
	}
	tui.SetIssueLogger(logger)

	tui.LogEvent("[%s] ✅ benign event", "BTC")
	tui.LogEvent("[%s] ❌ order rejected by exchange", "BTC")
	tui.CloseIssueLogger()

	file, err := os.Open(logPath)
	if err != nil {
		t.Fatalf("Open(%s) error = %v", logPath, err)
	}
	defer file.Close()

	records, err := csv.NewReader(file).ReadAll()
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected header + 1 critical record, got %d rows", len(records))
	}
	if got := records[1][2]; got != "BTC" {
		t.Fatalf("expected asset BTC, got %q", got)
	}
	if got := records[1][4]; got == "" || got == records[0][4] {
		t.Fatalf("expected critical message in details column, got %q", got)
	}
	if got := records[1][1]; got != "ERROR" {
		t.Fatalf("expected ERROR level, got %q", got)
	}
	_ = time.Now()
}

func TestTUI_getSplitPositions(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)

	// Initially should be empty
	positions := tui.getSplitPositions()
	if len(positions) != 0 {
		t.Errorf("Expected 0 positions initially, got %d", len(positions))
	}

	// Create and register an inventory with positions
	inv := NewSplitInventory()
	inv.RecordSplit("BTC", "Up", "Down", 50.0)
	inv.RecordSplit("ETH", "Yes", "No", 30.0)

	tui.RegisterSplitInventory(inv)

	// Get positions
	positions = tui.getSplitPositions()

	// Should have 4 positions (2 markets x 2 outcomes)
	if len(positions) != 4 {
		t.Errorf("Expected 4 positions, got %d", len(positions))
	}

	// Verify positions contain expected data
	posMap := make(map[string]float64)
	for _, p := range positions {
		key := p.MarketID + ":" + p.Outcome
		posMap[key] = p.Shares
	}

	if shares, ok := posMap["BTC:Up"]; !ok || shares != 50.0 {
		t.Errorf("Expected BTC:Up = 50 shares, got %v", shares)
	}
	if shares, ok := posMap["BTC:Down"]; !ok || shares != 50.0 {
		t.Errorf("Expected BTC:Down = 50 shares, got %v", shares)
	}
	if shares, ok := posMap["ETH:Yes"]; !ok || shares != 30.0 {
		t.Errorf("Expected ETH:Yes = 30 shares, got %v", shares)
	}
	if shares, ok := posMap["ETH:No"]; !ok || shares != 30.0 {
		t.Errorf("Expected ETH:No = 30 shares, got %v", shares)
	}
}

func TestTUI_getSplitPositions_MultipleInventories(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)

	// Create multiple inventories
	inv1 := NewSplitInventory()
	inv1.RecordSplit("BTC", "Up", "Down", 50.0)

	inv2 := NewSplitInventory()
	inv2.RecordSplit("ETH", "Yes", "No", 30.0)

	tui.RegisterSplitInventory(inv1)
	tui.RegisterSplitInventory(inv2)

	// Get positions from all inventories
	positions := tui.getSplitPositions()

	// Should have 4 positions total
	if len(positions) != 4 {
		t.Errorf("Expected 4 positions from 2 inventories, got %d", len(positions))
	}

	// Verify all markets are represented
	marketCount := make(map[string]int)
	for _, p := range positions {
		marketCount[p.MarketID]++
	}

	if marketCount["BTC"] != 2 {
		t.Errorf("Expected 2 BTC positions (Up/Down), got %d", marketCount["BTC"])
	}
	if marketCount["ETH"] != 2 {
		t.Errorf("Expected 2 ETH positions (Yes/No), got %d", marketCount["ETH"])
	}
}

func TestTUI_getSplitPositions_ConcurrentAccess(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)

	inv := NewSplitInventory()
	inv.RecordSplit("BTC", "Up", "Down", 100.0)
	tui.RegisterSplitInventory(inv)

	// Test concurrent access - should not deadlock
	done := make(chan bool, 2)

	// Goroutine 1: repeatedly get positions
	go func() {
		for i := 0; i < 100; i++ {
			_ = tui.getSplitPositions()
		}
		done <- true
	}()

	// Goroutine 2: modify inventory
	go func() {
		for i := 0; i < 100; i++ {
			inv.RecordSell("BTC", "Up", 0.5, 0.55)
		}
		done <- true
	}()

	// Wait for both to complete
	<-done
	<-done

	// Final position check
	positions := tui.getSplitPositions()
	if len(positions) == 0 {
		t.Error("Expected positions after concurrent access")
	}
}

func TestTUI_RegisterSplitInventory_ThreadSafe(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)

	// Concurrent registration
	done := make(chan bool, 3)

	for i := 0; i < 3; i++ {
		go func() {
			inv := NewSplitInventory()
			inv.RecordSplit("BTC", "Up", "Down", 10.0)
			tui.RegisterSplitInventory(inv)
			done <- true
		}()
	}

	// Wait for all registrations
	<-done
	<-done
	<-done

	// Verify all inventories were registered
	if len(tui.splitInventories) != 3 {
		t.Errorf("Expected 3 split inventories, got %d", len(tui.splitInventories))
	}
}

func TestNewTUI_UsesExpandedEventHistory(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	if tui.maxEvents < 100 {
		t.Fatalf("expected expanded event retention, got %d", tui.maxEvents)
	}
}

func TestTUILogEventRetainsNewestEntriesUpToMax(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.maxEvents = 3

	for i := 1; i <= 5; i++ {
		tui.LogEvent("event %d", i)
	}

	if len(tui.eventLog) != 3 {
		t.Fatalf("expected 3 retained events, got %d", len(tui.eventLog))
	}
	if !strings.Contains(tui.eventLog[0], "event 3") {
		t.Fatalf("expected oldest retained event to be event 3, got %q", tui.eventLog[0])
	}
	if !strings.Contains(tui.eventLog[2], "event 5") {
		t.Fatalf("expected newest retained event to be event 5, got %q", tui.eventLog[2])
	}
}

func TestTUIEventLogRowsPrioritizeLargerVisibleHistory(t *testing.T) {
	model := tuiModel{snap: tuiSnapshot{height: 40}}
	if got := model.eventLogRows(true); got < defaultTwoColEventRows {
		t.Fatalf("expected two-column event rows >= %d, got %d", defaultTwoColEventRows, got)
	}
	if got := model.eventLogRows(false); got < defaultOneColEventRows {
		t.Fatalf("expected one-column event rows >= %d, got %d", defaultOneColEventRows, got)
	}
	if got := model.orderHistoryRows(true); got > 12 {
		t.Fatalf("expected capped two-column order rows, got %d", got)
	}
}

func TestRenderEventLogShowsVisibleVsRetainedCount(t *testing.T) {
	model := tuiModel{snap: tuiSnapshot{eventLog: []string{"one", "two", "three"}}}
	rendered := model.renderEventLog(120, 2)
	if !strings.Contains(rendered, "showing 2/3") {
		t.Fatalf("expected render to show visible/retained counts, got %q", rendered)
	}
	if !strings.Contains(rendered, "two") || !strings.Contains(rendered, "three") {
		t.Fatalf("expected render to include newest events, got %q", rendered)
	}
	if strings.Contains(rendered, "one") {
		t.Fatalf("expected render to omit trimmed events, got %q", rendered)
	}
}

func TestViewportLinesClampsOffsetAndPads(t *testing.T) {
	visible, offset, maxOffset := viewportLines([]string{"a", "b", "c", "d", "e"}, 99, 3)
	if offset != 2 || maxOffset != 2 {
		t.Fatalf("expected offset/maxOffset 2/2, got %d/%d", offset, maxOffset)
	}
	joined := strings.Join(visible, ",")
	if joined != "c,d,e" {
		t.Fatalf("unexpected visible lines %q", joined)
	}

	visible, offset, maxOffset = viewportLines([]string{"only"}, 0, 3)
	if offset != 0 || maxOffset != 0 {
		t.Fatalf("expected offset/maxOffset 0/0, got %d/%d", offset, maxOffset)
	}
	if len(visible) != 3 || visible[0] != "only" || visible[1] != "" || visible[2] != "" {
		t.Fatalf("expected padded viewport, got %#v", visible)
	}
}

func TestTUIUpdateScrollKeys(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	model := tuiModel{
		tui:          tui,
		snap:         tuiSnapshot{height: 10, width: 120},
		contentLines: 40,
	}

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyDown})
	updated := next.(tuiModel)
	if updated.scrollOffset != 1 {
		t.Fatalf("expected down key to scroll to 1, got %d", updated.scrollOffset)
	}

	next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	updated = next.(tuiModel)
	if updated.scrollOffset <= 1 {
		t.Fatalf("expected pgdown to advance scroll, got %d", updated.scrollOffset)
	}

	next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	updated = next.(tuiModel)
	if updated.scrollOffset != 0 {
		t.Fatalf("expected g to jump to top, got %d", updated.scrollOffset)
	}

	next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
	updated = next.(tuiModel)
	if updated.scrollOffset != updated.maxScrollOffset() {
		t.Fatalf("expected G to jump to bottom, got %d want %d", updated.scrollOffset, updated.maxScrollOffset())
	}
}

func TestRenderFooterShowsScrollStatus(t *testing.T) {
	model := tuiModel{tui: NewTUI(NewEngine(1000.0), NewOrderBook())}
	rendered := model.renderFooter(140, 12, 50)
	if !strings.Contains(rendered, "Scroll 12/50") {
		t.Fatalf("expected footer scroll status, got %q", rendered)
	}
	if !strings.Contains(rendered, "[↑↓/jk] scroll") {
		t.Fatalf("expected footer controls, got %q", rendered)
	}
}

func TestTUIAddMarketPreservesExistingDataForSameMarket(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)
	end := time.Now().Add(10 * time.Minute).Round(time.Second)

	tui.AddMarket("BTC", "btc-updown-15m-1", []string{"Up", "Down"}, end)
	tui.UpdateMarketPricesWithSource("BTC", map[string]float64{"Up": 0.51}, map[string]float64{"Up": 0.53}, "WS")
	tui.SetMarketDetails("BTC", []string{"detail"})
	tui.AddMarket("BTC", "btc-updown-15m-1", []string{"Up", "Down"}, end)

	tui.mu.Lock()
	defer tui.mu.Unlock()
	market := tui.markets["BTC"]
	if market == nil {
		t.Fatal("expected BTC market to exist")
	}
	if market.Bids["Up"] != 0.51 || market.Asks["Up"] != 0.53 {
		t.Fatalf("expected prices to be preserved, got bids=%v asks=%v", market.Bids, market.Asks)
	}
	if len(market.Details) != 1 || market.Details[0] != "detail" {
		t.Fatalf("expected details to be preserved, got %+v", market.Details)
	}
}

func TestTUIUpdateMarketPricesWithSourceIgnoresZeroQuotes(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)

	tui.AddMarket("BTC", "btc-updown-15m-1", []string{"Up", "Down"}, time.Now().Add(10*time.Minute))
	tui.UpdateMarketPricesWithSource("BTC", map[string]float64{"Up": 0.51}, map[string]float64{"Up": 0.53}, "WS")
	tui.UpdateMarketPricesWithSource("BTC", map[string]float64{"Up": 0}, map[string]float64{"Up": 0}, "WS")

	tui.mu.Lock()
	defer tui.mu.Unlock()
	market := tui.markets["BTC"]
	if market.Bids["Up"] != 0.51 || market.Asks["Up"] != 0.53 {
		t.Fatalf("expected zero quotes to be ignored, got bids=%v asks=%v", market.Bids, market.Asks)
	}
}

func TestRenderAccountStatusFusionUsesFusionMetrics(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.mu.Lock()
	tui.mode = "Fusion"
	tui.mu.Unlock()
	model := tuiModel{tui: tui, snap: tuiSnapshot{tradeFactor: 0.05, startTime: time.Now().Add(-8 * time.Minute)}}

	rendered := model.renderAccountStatus(140, Stats{
		StartingBalance: 1000,
		CurrentBalance:  850,
		RealizedPnL:     -43.45,
		UnrealizedPnL:   -6.94,
		TotalTrades:     10,
		WinningTrades:   4,
		MaxDrawdown:     7.2,
	}, 108.03, 950.39, 1.0, 0, 0, map[string]Position{"BTC:Up": {}, "ETH:Down": {}, "SOL:Down": {}})

	if !strings.Contains(rendered, "FUSION STATUS") || !strings.Contains(rendered, "Mark") || !strings.Contains(rendered, "Unrealized") {
		t.Fatalf("expected fusion-specific metrics, got %q", rendered)
	}
	if strings.Contains(rendered, "Exposure") || strings.Contains(rendered, "Compound") || strings.Contains(rendered, "rounds") {
		t.Fatalf("expected arb wording to be absent in fusion mode, got %q", rendered)
	}
}

func TestRenderPositionsFusionRemovesAwaitingMergeLabel(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.mu.Lock()
	tui.mode = "Fusion"
	tui.mu.Unlock()
	model := tuiModel{tui: tui}

	rendered := model.renderPositions(140, map[string]PositionPnL{
		"BTC:Up":   {Position: Position{MarketID: "BTC", Outcome: "Up", Quantity: 72, AvgPrice: 0.39}, CurrentBid: 0.29, MarketValue: 20.88, UnrealizedPnL: -7.20},
		"ETH:Down": {Position: Position{MarketID: "ETH", Outcome: "Down", Quantity: 40, AvgPrice: 0.81}, CurrentBid: 0.83, MarketValue: 33.20, UnrealizedPnL: 0.80},
	})

	if !strings.Contains(rendered, "OPEN SIGNAL POSITIONS") || !strings.Contains(rendered, "Unrealized") {
		t.Fatalf("expected fusion positions panel, got %q", rendered)
	}
	if strings.Contains(rendered, "awaiting merge") || strings.Contains(rendered, "Locked") {
		t.Fatalf("expected merge wording to be absent, got %q", rendered)
	}
}

func TestRenderOrderHistoryFusionUsesEdgeAndPnLLabels(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.mu.Lock()
	tui.mode = "Fusion"
	tui.orderHistory = []OrderHistoryEntry{
		{Timestamp: time.Now(), MarketID: "BTC", Outcome: "Down", Side: "BUY", Shares: 66, Price: 0.75, Cost: 50, Margin: 13.28, Profit: 0, Status: "FILLED"},
		{Timestamp: time.Now(), MarketID: "SOL", Outcome: "Down", Side: "SELL", Shares: 56, Price: 0.53, Cost: 29.68, Margin: 2.4, Profit: 2.10, Status: "FILLED"},
	}
	tui.mu.Unlock()
	model := tuiModel{tui: tui, snap: tuiSnapshot{orderHistory: tui.GetOrderHistory()}}

	rendered := model.renderOrderHistory(140, 10)
	if !strings.Contains(rendered, "EDGE / PNL") || !strings.Contains(rendered, "edge +13.28%") || !strings.Contains(rendered, "+$2.10") {
		t.Fatalf("expected fusion order-history semantics, got %q", rendered)
	}
	if strings.Contains(rendered, "PROFIT/MARGIN") {
		t.Fatalf("expected generic arb label to be absent, got %q", rendered)
	}
}

func TestRenderSettingsFusionShowsHeuristicControls(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.mu.Lock()
	tui.mode = "Fusion"
	tui.settings = TUISettings{MarketSlug: "SOL", MaxMarkets: 4, Timeframe: "15m", TradeScaleFactor: 0.05, MinMarginPercent: 2.0, BuyExecutionMarginFloorPercent: -1.0, MinAskPrice: 0.10, MaxAskPrice: 0.90, FusionMinAskDepthShares: 60, FusionMaxSpreadPercent: 8, FusionMinScorePercent: 2, FusionMaxMarketDataAgeSec: 3, FusionMaxBinanceDataAgeSec: 3, FusionMinConsensusVotes: 3}
	tui.mu.Unlock()
	model := tuiModel{tui: tui, showSettings: true}

	rendered := model.renderSettings(140)
	for _, label := range []string{"Min Ask Depth", "Max Spread %", "Min Score %", "Consensus Votes"} {
		if !strings.Contains(rendered, label) {
			t.Fatalf("expected fusion settings label %q, got %q", label, rendered)
		}
	}
}
