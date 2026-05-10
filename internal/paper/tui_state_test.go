package paper

import (
	"encoding/csv"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"Market-bot/internal/core"

	tea "github.com/charmbracelet/bubbletea"
)

func TestTUI_RegisterOrderBookAggregatesByMarket(t *testing.T) {
	engine := NewEngine(1000.0)
	tui := NewTUI(engine, nil)
	btcBook := NewOrderBookWithRealism(0, 0)
	ethBook := NewOrderBookWithRealism(0, 0)

	tui.RegisterOrderBook("BTC", btcBook)
	tui.RegisterOrderBook("ETH", ethBook)

	btcBook.PlaceOrder("Up", "buy", 0.45, 7, 0)
	ethBook.PlaceOrder("Down", "sell", 0.55, 9, 0)

	orders := tui.getOpenOrdersSnapshot()
	if len(orders) != 2 {
		t.Fatalf("expected 2 scoped open orders, got %d", len(orders))
	}
	seen := map[string]bool{}
	for _, order := range orders {
		seen[order.MarketID+":"+order.Order.Outcome] = true
	}
	if !seen["BTC:Up"] || !seen["ETH:Down"] {
		t.Fatalf("expected aggregated orders for BTC:Up and ETH:Down, got %+v", seen)
	}
}

func TestTUI_SetPendingOrdersKeepsMarketsSeparate(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), nil)
	tui.SetPendingOrders("BTC", map[string][]PendingOrder{
		"Up": {{Outcome: "Up", Qty: 5, Price: 0.44, Side: "BUY"}},
	})
	tui.SetPendingOrders("ETH", map[string][]PendingOrder{
		"Down": {{Outcome: "Down", Qty: 6, Price: 0.56, Side: "SELL"}},
	})

	if len(tui.pendingOrders) != 2 {
		t.Fatalf("expected pending orders for 2 markets, got %d", len(tui.pendingOrders))
	}
	if got := tui.pendingOrders["BTC"][0].MarketID; got != "BTC" {
		t.Fatalf("expected BTC pending order market id, got %q", got)
	}
	if got := tui.pendingOrders["ETH"][0].Outcome; got != "Down" {
		t.Fatalf("expected ETH pending order outcome Down, got %q", got)
	}
}

func TestTakerCloseModeActiveOnlyInTakerMode(t *testing.T) {
	if !TakerCloseModeActive(TUISettings{PaperArbMode: "taker", TakerCloseMarket: true}) {
		t.Fatal("expected taker-close to be active in taker mode")
	}

	for _, mode := range []string{"maker", "copytrade", "binance-gap"} {
		if TakerCloseModeActive(TUISettings{PaperArbMode: mode, TakerCloseMarket: true}) {
			t.Fatalf("expected taker-close to be inactive in %s mode", mode)
		}
	}

	if TakerCloseModeActive(TUISettings{PaperArbMode: "taker", TakerCloseMarket: false}) {
		t.Fatal("expected taker-close to stay inactive when disabled")
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

func TestTUIUpdateMarketPricesWithSourceRetainsLastNonZeroQuotes(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.AddMarket("BTC", "btc-market", []string{"Yes", "No"}, time.Now().Add(10*time.Minute))

	tui.UpdateMarketPricesWithSource("BTC",
		map[string]float64{"Yes": 0.41, "No": 0.57},
		map[string]float64{"Yes": 0.43, "No": 0.59},
		"WS",
	)
	tui.UpdateMarketPricesWithSource("BTC",
		map[string]float64{"Yes": 0, "No": 0},
		map[string]float64{"Yes": 0, "No": 0},
		"WS",
	)

	mkt := tui.markets["BTC"]
	if got := mkt.Bids["Yes"]; got != 0 {
		t.Fatalf("expected live bid to clear to 0, got %.2f", got)
	}
	if !mkt.ClearedBids["Yes"] || !mkt.ClearedAsks["No"] {
		t.Fatalf("expected explicit clear flags to be set, got bids=%v asks=%v", mkt.ClearedBids, mkt.ClearedAsks)
	}
	if got := mkt.RealBids["Yes"]; got != 0.41 {
		t.Fatalf("expected last good bid to remain 0.41, got %.2f", got)
	}
	if got := mkt.RealAsks["No"]; got != 0.59 {
		t.Fatalf("expected last good ask to remain 0.59, got %.2f", got)
	}
}

func TestDisplayDirtyThrottleMarksPendingDuringQuoteBurst(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.AddMarket("BTC", "btc-updown-5m-1", []string{"Up", "Down"}, time.Now().Add(time.Minute))

	base := time.Unix(100, 0)
	initialVersion := tui.snapshotVersion
	tui.UpdateMarketPricesWithSourceAt("BTC", map[string]float64{"Up": 0.45}, map[string]float64{"Up": 0.55}, "WS", base)
	if tui.snapshotVersion != initialVersion+1 {
		t.Fatalf("expected first display update to bump snapshot version, got %d", tui.snapshotVersion)
	}

	tui.UpdateMarketPricesWithSourceAt("BTC", map[string]float64{"Up": 0.46}, map[string]float64{"Up": 0.56}, "WS", base.Add(100*time.Millisecond))
	if tui.snapshotVersion != initialVersion+1 {
		t.Fatalf("expected burst display update to stay throttled at version %d, got %d", initialVersion+1, tui.snapshotVersion)
	}
	if !tui.displayDirtyPending {
		t.Fatal("expected throttled display update to leave a pending dirty flag")
	}

	tui.UpdateMarketPricesWithSourceAt("BTC", map[string]float64{"Up": 0.47}, map[string]float64{"Up": 0.57}, "WS", base.Add(tuiDisplayDirtyMinInterval))
	if tui.snapshotVersion != initialVersion+2 {
		t.Fatalf("expected later display update to bump snapshot version again, got %d", tui.snapshotVersion)
	}
	if tui.displayDirtyPending {
		t.Fatal("expected pending dirty flag to clear after throttled flush")
	}
}

func TestTickFlushesPendingDisplayDirty(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.AddMarket("BTC", "btc-updown-5m-1", []string{"Up", "Down"}, time.Now().Add(time.Minute))

	base := time.Unix(100, 0)
	tui.UpdateMarketPricesWithSourceAt("BTC", map[string]float64{"Up": 0.45}, map[string]float64{"Up": 0.55}, "WS", base)
	tui.UpdateMarketPricesWithSourceAt("BTC", map[string]float64{"Up": 0.46}, map[string]float64{"Up": 0.56}, "WS", base.Add(100*time.Millisecond))
	if !tui.displayDirtyPending {
		t.Fatal("expected pending dirty flag before tick flush")
	}

	model := tuiModel{tui: tui, interval: time.Second}
	next, _ := model.Update(tickMsg(base.Add(time.Second)))
	updated := next.(tuiModel)

	if tui.displayDirtyPending {
		t.Fatal("expected tick to flush pending display dirty flag")
	}
	if updated.snap.version != tui.snapshotVersion {
		t.Fatalf("expected tick snapshot version %d to match tui version %d", updated.snap.version, tui.snapshotVersion)
	}
}

func TestClearMarketsPreservesMetadataForHeldCarry(t *testing.T) {
	engine := NewEngine(100.0)
	tui := NewTUI(engine, NewOrderBook())
	endTime := time.Now().Add(-time.Minute)
	tui.AddMarket("btc-updown-5m-1776383100", "btc-updown-5m-1776383100", []string{"Up", "Down"}, endTime)

	if !engine.SyncExternalPosition("btc-updown-5m-1776383100", "Up", 1.0163, 0.95) {
		t.Fatal("expected carry position sync to change engine state")
	}

	tui.ClearMarkets()

	market, ok := tui.markets["btc-updown-5m-1776383100"]
	if !ok || market == nil {
		t.Fatalf("expected ClearMarkets to preserve metadata for held carry, got %+v", tui.markets)
	}
	if !market.EndTime.Equal(endTime) {
		t.Fatalf("expected preserved carry market end time %s, got %s", endTime, market.EndTime)
	}
}

func TestLogEventSuppressesImmediateConsecutiveDuplicates(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())

	tui.LogEvent("repeat me")
	tui.LogEvent("repeat me")

	if got := len(tui.eventLog); got != 1 {
		t.Fatalf("expected consecutive duplicate logs to collapse to 1 entry, got %d", got)
	}
}

func TestUpdateMarketPricesWithSourceAtThrottlesTinyBurstMoves(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.AddMarket("BTC", "btc-updown-5m-1", []string{"Up", "Down"}, time.Now().Add(time.Minute))

	base := time.Unix(100, 0)
	tui.UpdateMarketPricesWithSourceAt("BTC", map[string]float64{"Up": 0.45}, map[string]float64{"Up": 0.55}, "WS", base)
	firstVersion := tui.snapshotVersion

	tui.UpdateMarketPricesWithSourceAt("BTC", map[string]float64{"Up": 0.451}, map[string]float64{"Up": 0.551}, "WS", base.Add(50*time.Millisecond))

	if tui.snapshotVersion != firstVersion {
		t.Fatalf("expected tiny burst move to stay throttled at version %d, got %d", firstVersion, tui.snapshotVersion)
	}
	if got := tui.markets["BTC"].Bids["Up"]; math.Abs(got-0.45) > 1e-9 {
		t.Fatalf("expected throttled tiny burst to keep displayed bid at 0.45, got %.4f", got)
	}
}

func TestUpdateOrderBookDepthThrottlesRapidRefresh(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.AddMarket("BTC", "btc-updown-5m-1", []string{"Up", "Down"}, time.Now().Add(time.Minute))

	tui.UpdateOrderBookDepth("BTC", map[string][]MarketLevel{"Up": {{Price: 0.45, Size: 10}}}, map[string][]MarketLevel{"Up": {{Price: 0.55, Size: 10}}})
	firstVersion := tui.snapshotVersion
	firstDepthTime := tui.markets["BTC"].LastDepthUpdate

	tui.UpdateOrderBookDepth("BTC", map[string][]MarketLevel{"Up": {{Price: 0.46, Size: 11}}}, map[string][]MarketLevel{"Up": {{Price: 0.56, Size: 11}}})

	if tui.snapshotVersion != firstVersion {
		t.Fatalf("expected rapid depth refresh to stay throttled at version %d, got %d", firstVersion, tui.snapshotVersion)
	}
	if !tui.markets["BTC"].LastDepthUpdate.Equal(firstDepthTime) {
		t.Fatalf("expected throttled rapid depth refresh to preserve last depth update time")
	}
}

func TestTUIToggleTradingPause(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	if tui.IsTradingPaused() {
		t.Fatal("expected manual trading pause to start disabled")
	}

	if paused := tui.ToggleTradingPause(); !paused {
		t.Fatal("expected toggle to enable manual trading pause")
	}
	if !tui.IsTradingPaused() {
		t.Fatal("expected manual trading pause to remain enabled")
	}
	if len(tui.eventLog) == 0 || !strings.Contains(tui.eventLog[len(tui.eventLog)-1], "Manual trading pause enabled") {
		t.Fatalf("expected pause-enable event log, got %#v", tui.eventLog)
	}

	if paused := tui.ToggleTradingPause(); paused {
		t.Fatal("expected second toggle to disable manual trading pause")
	}
	if tui.IsTradingPaused() {
		t.Fatal("expected manual trading pause to be disabled")
	}
	if len(tui.eventLog) == 0 || !strings.Contains(tui.eventLog[len(tui.eventLog)-1], "Manual trading pause disabled") {
		t.Fatalf("expected pause-disable event log, got %#v", tui.eventLog)
	}
}

func TestTUISetTradingPaused(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.SetTradingPaused(true)
	if !tui.IsTradingPaused() {
		t.Fatal("expected SetTradingPaused(true) to pause trading")
	}
	tui.SetTradingPaused(false)
	if tui.IsTradingPaused() {
		t.Fatal("expected SetTradingPaused(false) to resume trading")
	}
}

func TestPauseHotkeyTogglesTradingPause(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	model := tuiModel{tui: tui}

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	updated := next.(tuiModel)
	if !tui.IsTradingPaused() {
		t.Fatal("expected p hotkey to enable manual trading pause")
	}
	if !updated.snap.manualTradingPause {
		t.Fatal("expected snapshot pause state to update immediately")
	}

	next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'P'}})
	updated = next.(tuiModel)
	if tui.IsTradingPaused() {
		t.Fatal("expected P hotkey to disable manual trading pause")
	}
	if updated.snap.manualTradingPause {
		t.Fatal("expected snapshot pause state to clear immediately")
	}
}

func TestTUIGetOpenOrdersSnapshotAggregatesRegisteredBooks(t *testing.T) {
	engine := NewEngine(1000.0)
	tui := NewTUI(engine, nil)

	book1 := NewOrderBook()
	book2 := NewOrderBook()
	tui.RegisterOrderBook("BTC", book1)
	tui.RegisterOrderBook("ETH", book2)

	book1.PlaceOrder("Up", "buy", 0.48, 5, 0)
	book2.PlaceOrder("Down", "sell", 0.52, 7, 0)

	orders := tui.getOpenOrdersSnapshot()
	if len(orders) != 2 {
		t.Fatalf("expected 2 open orders across registered books, got %d", len(orders))
	}
}

func TestTUICancelAllOrdersCancelsRegisteredBooks(t *testing.T) {
	engine := NewEngine(1000.0)
	tui := NewTUI(engine, nil)

	book1 := NewOrderBook()
	book2 := NewOrderBook()
	tui.RegisterOrderBook("BTC", book1)
	tui.RegisterOrderBook("ETH", book2)

	book1.PlaceOrder("Up", "buy", 0.48, 5, 0)
	book2.PlaceOrder("Down", "sell", 0.52, 7, 0)

	tui.CancelAllOrders()

	if got := len(book1.GetOpenOrders()); got != 0 {
		t.Fatalf("expected BTC book to be fully cancelled, got %d open orders", got)
	}
	if got := len(book2.GetOpenOrders()); got != 0 {
		t.Fatalf("expected ETH book to be fully cancelled, got %d open orders", got)
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

func TestApplyPaperBalanceLockedAllowsOpenInventory(t *testing.T) {
	engine := NewEngine(1000.0)
	if _, err := engine.BuyForMarket("BTC#m1", "Up", 0.5, 10); err != nil {
		t.Fatalf("buy failed: %v", err)
	}
	tui := NewTUI(engine, NewOrderBook())

	if err := tui.applyPaperBalanceLocked(250.0); err != nil {
		t.Fatalf("expected paper balance to adjust with open inventory, got %v", err)
	}

	stats := engine.GetStats()
	if stats.CurrentBalance != 250.0 {
		t.Fatalf("expected current balance 250.0 after cash sync, got %.2f", stats.CurrentBalance)
	}
	if stats.StartingBalance <= 0 {
		t.Fatalf("expected baseline to remain neutralized after cash sync, got %.2f", stats.StartingBalance)
	}
}

func TestResetSessionDisplayClearsLiveSessionState(t *testing.T) {
	tui := NewTUI(NewEngine(100.0), NewOrderBook())
	tui.LogEvent("keep audit log")
	tui.RecordOrder("BTC", "Up", "BUY", 1, 0.5, 0.5, 0, 0, "FILLED")
	tui.RecordRound(100, 101, 1, 1, map[string]Position{
		"BTC:Up": {MarketID: "BTC", Outcome: "Up", Quantity: 1, TotalCost: 0.5},
	}, nil)
	tui.SetWalletCash(42.5)
	tui.SetWalletTruthPositions("BTC", []WalletTruthPosition{{
		MarketID:      "BTC",
		Outcome:       "Up",
		OnChainShares: 1,
	}})
	tui.SetPendingOrders("BTC", map[string][]PendingOrder{
		"Up": {{Price: 0.51, Qty: 1, Side: "BUY"}},
	})
	tui.RegisterSplitInventory(NewSplitInventory())
	tui.SetKillSwitch("live issue")
	tui.amendedPnLForNextRound = 1.23

	tui.ResetSessionDisplay()

	if got := len(tui.GetOrderHistory()); got != 0 {
		t.Fatalf("expected order history cleared, got %d entries", got)
	}
	if got := len(tui.GetRoundHistory()); got != 0 {
		t.Fatalf("expected round history cleared, got %d entries", got)
	}
	if got := len(tui.getWalletTruthPositions()); got != 0 {
		t.Fatalf("expected wallet truth cleared, got %d entries", got)
	}
	if got := len(tui.getSplitPositions()); got != 0 {
		t.Fatalf("expected split positions cleared, got %d entries", got)
	}

	tui.mu.Lock()
	defer tui.mu.Unlock()
	if len(tui.pendingOrders) != 0 {
		t.Fatalf("expected pending orders cleared, got %+v", tui.pendingOrders)
	}
	if tui.hasWalletCash || tui.walletCash != 0 {
		t.Fatalf("expected wallet cash cleared, has=%v cash=%.2f", tui.hasWalletCash, tui.walletCash)
	}
	if tui.isKilled || tui.killReason != "" {
		t.Fatalf("expected kill state cleared, killed=%v reason=%q", tui.isKilled, tui.killReason)
	}
	if tui.amendedPnLForNextRound != 0 {
		t.Fatalf("expected amended pnl reset, got %.2f", tui.amendedPnLForNextRound)
	}
	if len(tui.eventLog) == 0 {
		t.Fatal("expected audit event log to be preserved")
	}
}

func TestRecordRoundCapturesOutcomeShares(t *testing.T) {
	tui := NewTUI(NewEngine(100.0), NewOrderBook())
	tui.RecordRound(100.0, 92.5, -7.5, 2, map[string]Position{
		"m1:Up":   {MarketID: "m1", Outcome: "Up", Quantity: 118},
		"m1:Down": {MarketID: "m1", Outcome: "Down", Quantity: 103},
	}, nil)

	history := tui.GetRoundHistory()
	if len(history) != 1 {
		t.Fatalf("expected 1 round history entry, got %d", len(history))
	}
	if !strings.Contains(history[0].ShareSummary, "Up 118") || !strings.Contains(history[0].ShareSummary, "Down 103") {
		t.Fatalf("expected up/down shares in round summary, got %q", history[0].ShareSummary)
	}
	if !strings.Contains(history[0].ShareSummary, "m1:") {
		t.Fatalf("expected round summary to be grouped by market, got %q", history[0].ShareSummary)
	}
}

func TestRecordRoundPreservesEarlierCarrySnapshots(t *testing.T) {
	tui := NewTUI(NewEngine(100.0), NewOrderBook())
	tui.RecordRound(100.0, 99.0, -1.0, 6, map[string]Position{
		"m2:Up": {MarketID: "m2", Outcome: "Up", Quantity: 10.0, TotalCost: 6.60},
	}, nil)
	tui.RecordRound(99.0, 98.0, -1.0, 8, map[string]Position{
		"m2:Up":   {MarketID: "m2", Outcome: "Up", Quantity: 10.0, TotalCost: 6.60},
		"m3:Up":   {MarketID: "m3", Outcome: "Up", Quantity: 14.0, TotalCost: 7.98},
		"m3:Down": {MarketID: "m3", Outcome: "Down", Quantity: 10.0, TotalCost: 6.10},
	}, nil)

	history := tui.GetRoundHistory()
	if len(history) != 2 {
		t.Fatalf("expected 2 round history entries, got %d", len(history))
	}
	if !strings.Contains(history[0].ShareSummary, "m2: Up 10") {
		t.Fatalf("expected earlier round to keep its carry snapshot, got %q", history[0].ShareSummary)
	}
	if strings.Contains(history[0].ShareSummary, "m3:") {
		t.Fatalf("expected earlier round not to absorb later market carry, got %q", history[0].ShareSummary)
	}
}

func TestRoundHistoryShareSummaryLeavesOpenCarryUnmarkedWhenOtherMarketRedeemed(t *testing.T) {
	summary := roundHistoryShareSummary(map[string]Position{
		"m2:Up":   {MarketID: "m2", Outcome: "Up", Quantity: 9.04519, TotalCost: 9.04519 * 0.47},
		"m2:Down": {MarketID: "m2", Outcome: "Down", Quantity: 12.10775, TotalCost: 12.10775 * 0.70},
	}, []*RedemptionResult{
		{
			MarketID:       "m1",
			WinningOutcome: "Down",
			WinningShares:  12.13305,
			WinningCost:    12.13305 * 0.75,
			LosingOutcome:  "Up",
			LosingShares:   6.04569,
			LosingCost:     6.04569 * 0.36,
		},
	})

	if !strings.Contains(summary, "m2: Up 9.04519@$0.47  |  Down 12.10775@$0.70") {
		t.Fatalf("expected unresolved carry market summary to remain visible, got %q", summary)
	}
	if strings.Contains(summary, "m2: Up 9.04519@$0.47 ✗") || strings.Contains(summary, "m2: Down 12.10775@$0.70 ✗") {
		t.Fatalf("expected unresolved carry market not to be marked as settled loss, got %q", summary)
	}
}

func TestRoundHistoryDisplaySummarySuppressesResolvedMarketsWhenOtherCarryRemains(t *testing.T) {
	history := []RoundHistoryEntry{
		{
			Number: 1,
		},
		{
			Number: 2,
			positions: map[string]Position{
				"m2:Up":   {MarketID: "m2", Outcome: "Up", Quantity: 9.04519, TotalCost: 9.04519 * 0.47},
				"m2:Down": {MarketID: "m2", Outcome: "Down", Quantity: 12.10775, TotalCost: 12.10775 * 0.70},
			},
			redemptions: []*RedemptionResult{
				{
					MarketID:       "m1",
					WinningOutcome: "Down",
					WinningShares:  12.13305,
					WinningCost:    12.13305 * 0.75,
					LosingOutcome:  "Up",
					LosingShares:   6.04569,
					LosingCost:     6.04569 * 0.36,
				},
			},
		},
	}

	summary := roundHistoryDisplaySummary(history, 1)
	if !strings.Contains(summary, "m2: Up 9.04519@$0.47  |  Down 12.10775@$0.70") {
		t.Fatalf("expected unresolved carry market to remain in display summary, got %q", summary)
	}
	if strings.Contains(summary, "m1:") {
		t.Fatalf("expected resolved market to be suppressed while other carry remains, got %q", summary)
	}
}

func TestRoundHistoryDisplaySummaryKeepsResolvedMarketsWhenNoCarryRemains(t *testing.T) {
	history := []RoundHistoryEntry{
		{
			Number: 1,
			positions: map[string]Position{
				"m1:Up":   {MarketID: "m1", Outcome: "Up", Quantity: 6.0, TotalCost: 2.40},
				"m1:Down": {MarketID: "m1", Outcome: "Down", Quantity: 4.0, TotalCost: 2.00},
			},
		},
		{
			Number: 2,
			redemptions: []*RedemptionResult{
				{
					MarketID:       "m1",
					WinningOutcome: "Up",
					WinningShares:  6.0,
					WinningCost:    2.40,
					LosingOutcome:  "Down",
					LosingShares:   4.0,
					LosingCost:     2.00,
				},
			},
		},
	}

	summary := roundHistoryDisplaySummary(history, 1)
	if !strings.Contains(summary, "m1: Up 6@$0.40") || !strings.Contains(summary, "Down 4@$0.50") {
		t.Fatalf("expected fully resolved round to keep redemption detail, got %q", summary)
	}
}

func TestAmendMostRecentRoundForMarketTargetsMatchingRound(t *testing.T) {
	tui := NewTUI(NewEngine(100.0), NewOrderBook())
	tui.RecordRound(100.0, 100.0, 0.0, 10, map[string]Position{
		"m1:Up":   {MarketID: "m1", Outcome: "Up", Quantity: 6.0, TotalCost: 2.40},
		"m1:Down": {MarketID: "m1", Outcome: "Down", Quantity: 4.0, TotalCost: 2.00},
	}, nil)
	tui.RecordRound(100.0, 101.0, 1.0, 12, map[string]Position{
		"m2:Up": {MarketID: "m2", Outcome: "Up", Quantity: 1.0, TotalCost: 0.50},
	}, nil)

	tui.AmendMostRecentRoundForMarket("m1", 1.60, []*RedemptionResult{{
		MarketID:       "m1",
		WinningOutcome: "Up",
		WinningShares:  6.0,
		WinningPayout:  6.0,
		WinningCost:    2.40,
		WinningPnL:     3.60,
		LosingOutcome:  "Down",
		LosingShares:   4.0,
		LosingCost:     2.00,
		TotalPayout:    6.0,
		TotalPnL:       1.60,
	}})

	history := tui.GetRoundHistory()
	if len(history) != 2 {
		t.Fatalf("expected 2 round history entries, got %d", len(history))
	}
	if got := history[0].EndingEquity; math.Abs(got-101.60) > 0.0001 {
		t.Fatalf("expected matching round ending equity to include redemption delta, got %.2f", got)
	}
	if got := history[0].PnL; math.Abs(got-1.60) > 0.0001 {
		t.Fatalf("expected matching round pnl to include redemption delta, got %.2f", got)
	}
	if got := history[1].StartingEquity; math.Abs(got-101.60) > 0.0001 {
		t.Fatalf("expected later round starting equity to rebase after redemption delta, got %.2f", got)
	}
	if got := history[1].EndingEquity; math.Abs(got-102.60) > 0.0001 {
		t.Fatalf("expected later round ending equity to rebase after redemption delta, got %.2f", got)
	}
	if got := history[1].PnL; math.Abs(got-1.0) > 0.0001 {
		t.Fatalf("expected later round pnl to remain unchanged, got %.2f", got)
	}
	if !strings.Contains(history[0].ShareSummary, "m1:") || !strings.Contains(history[0].ShareSummary, "Up 6@$0.40") || !strings.Contains(history[0].ShareSummary, "Down 4@$0.50") {
		t.Fatalf("expected amended round summary to keep redemption outcomes, got %q", history[0].ShareSummary)
	}
}

func TestAmendMostRecentRoundForMarketAttributesCarryResolutionToOriginRound(t *testing.T) {
	tui := NewTUI(NewEngine(100.0), NewOrderBook())
	tui.RecordRound(100.0, 98.0, -2.0, 10, map[string]Position{
		"m1:Up":   {MarketID: "m1", Outcome: "Up", Quantity: 6.0, TotalCost: 2.40},
		"m1:Down": {MarketID: "m1", Outcome: "Down", Quantity: 4.0, TotalCost: 2.00},
	}, nil)
	tui.RecordRound(98.0, 99.0, 1.0, 8, map[string]Position{
		"m1:Up":   {MarketID: "m1", Outcome: "Up", Quantity: 6.0, TotalCost: 2.40},
		"m1:Down": {MarketID: "m1", Outcome: "Down", Quantity: 4.0, TotalCost: 2.00},
		"m2:Up":   {MarketID: "m2", Outcome: "Up", Quantity: 3.0, TotalCost: 1.50},
		"m2:Down": {MarketID: "m2", Outcome: "Down", Quantity: 2.0, TotalCost: 1.20},
	}, nil)

	tui.AmendMostRecentRoundForMarket("m1", 1.60, []*RedemptionResult{{
		MarketID:       "m1",
		WinningOutcome: "Up",
		WinningShares:  6.0,
		WinningPayout:  6.0,
		WinningCost:    2.40,
		WinningPnL:     3.60,
		LosingOutcome:  "Down",
		LosingShares:   4.0,
		LosingCost:     2.00,
		TotalPayout:    6.0,
		TotalPnL:       1.60,
	}})

	history := tui.GetRoundHistory()
	if len(history) != 2 {
		t.Fatalf("expected 2 round history entries, got %d", len(history))
	}
	if got := history[0].EndingEquity; math.Abs(got-99.60) > 0.0001 {
		t.Fatalf("expected carry-origin round ending equity to include redemption delta, got %.2f", got)
	}
	if got := history[0].PnL; math.Abs(got-(-0.40)) > 0.0001 {
		t.Fatalf("expected carry-origin round pnl to include redemption delta, got %.2f", got)
	}
	if !strings.Contains(history[0].ShareSummary, "m1:") || !strings.Contains(history[0].ShareSummary, "Up 6@$0.40") {
		t.Fatalf("expected carry-origin round to retain resolved m1 detail, got %q", history[0].ShareSummary)
	}
	if got := history[1].StartingEquity; math.Abs(got-99.60) > 0.0001 {
		t.Fatalf("expected later round starting equity to rebase after redemption delta, got %.2f", got)
	}
	if got := history[1].EndingEquity; math.Abs(got-100.60) > 0.0001 {
		t.Fatalf("expected later round ending equity to rebase after redemption delta, got %.2f", got)
	}
	if got := history[1].PnL; math.Abs(got-1.0) > 0.0001 {
		t.Fatalf("expected later round pnl to remain unchanged, got %.2f", got)
	}
	if _, ok := history[1].positions["m1:Up"]; ok {
		t.Fatalf("expected later carry-forward row to drop duplicate m1 up leg after resolution, got %+v", history[1].positions["m1:Up"])
	}
	if _, ok := history[1].positions["m1:Down"]; ok {
		t.Fatalf("expected later carry-forward row to drop duplicate m1 down leg after resolution, got %+v", history[1].positions["m1:Down"])
	}
	if got, ok := history[1].positions["m2:Up"]; !ok || math.Abs(got.Quantity-3.0) > 0.0001 || math.Abs(got.TotalCost-1.50) > 0.0001 {
		t.Fatalf("expected unrelated m2 up leg preserved in later row, got %+v exists=%v", got, ok)
	}
	if got, ok := history[1].positions["m2:Down"]; !ok || math.Abs(got.Quantity-2.0) > 0.0001 || math.Abs(got.TotalCost-1.20) > 0.0001 {
		t.Fatalf("expected unrelated m2 down leg preserved in later row, got %+v exists=%v", got, ok)
	}
	if strings.Contains(history[1].ShareSummary, "m1:") {
		t.Fatalf("expected later carry-forward row to drop duplicate resolved m1 summary, got %q", history[1].ShareSummary)
	}
	if !strings.Contains(history[1].ShareSummary, "m2:") {
		t.Fatalf("expected later row to keep unrelated market summary, got %q", history[1].ShareSummary)
	}
}

func TestAmendMostRecentRoundForMarketOnlyTouchesTargetMarket(t *testing.T) {
	for i := 0; i < 100; i++ {
		tui := NewTUI(NewEngine(100.0), NewOrderBook())
		tui.RecordRound(100.0, 100.0, 0.0, 10, map[string]Position{
			"m1:Up":   {MarketID: "m1", Outcome: "Up", Quantity: 6.0, TotalCost: 2.40},
			"m1:Down": {MarketID: "m1", Outcome: "Down", Quantity: 4.0, TotalCost: 2.00},
			"m2:Up":   {MarketID: "m2", Outcome: "Up", Quantity: 3.0, TotalCost: 1.50},
			"m2:Down": {MarketID: "m2", Outcome: "Down", Quantity: 2.0, TotalCost: 1.20},
		}, nil)

		tui.AmendMostRecentRoundForMarket("m1", 1.60, []*RedemptionResult{{
			MarketID:       "m1",
			WinningOutcome: "Up",
			WinningShares:  6.0,
			WinningPayout:  6.0,
			WinningCost:    2.40,
			WinningPnL:     3.60,
			LosingOutcome:  "Down",
			LosingShares:   4.0,
			LosingCost:     2.00,
			TotalPayout:    6.0,
			TotalPnL:       1.60,
		}})

		history := tui.GetRoundHistory()
		if len(history) != 1 {
			t.Fatalf("expected 1 round history entry, got %d", len(history))
		}

		entry := history[0]
		if _, ok := entry.positions["m1:Up"]; ok {
			t.Fatalf("expected redeemed m1 up leg removed from snapshot on iteration %d", i)
		}
		if _, ok := entry.positions["m1:Down"]; ok {
			t.Fatalf("expected redeemed m1 down leg removed from snapshot on iteration %d", i)
		}

		if got, ok := entry.positions["m2:Up"]; !ok || math.Abs(got.Quantity-3.0) > 0.0001 || math.Abs(got.TotalCost-1.50) > 0.0001 {
			t.Fatalf("expected unrelated m2 up leg untouched on iteration %d, got %+v exists=%v", i, got, ok)
		}
		if got, ok := entry.positions["m2:Down"]; !ok || math.Abs(got.Quantity-2.0) > 0.0001 || math.Abs(got.TotalCost-1.20) > 0.0001 {
			t.Fatalf("expected unrelated m2 down leg untouched on iteration %d, got %+v exists=%v", i, got, ok)
		}
		if got := entry.PnL; math.Abs(got-1.60) > 0.0001 {
			t.Fatalf("expected target round pnl to reflect redeemed m1 delta on iteration %d, got %.2f", i, got)
		}
		if got := entry.EndingEquity; math.Abs(got-101.60) > 0.0001 {
			t.Fatalf("expected target round ending equity to reflect redeemed m1 delta on iteration %d, got %.2f", i, got)
		}
	}
}

func TestRoundHistoryHasOpenInventoryIgnoresResolvedMarketResiduals(t *testing.T) {
	entry := RoundHistoryEntry{
		positions: map[string]Position{
			"m1:Up": {MarketID: "m1", Outcome: "Up", Quantity: 0.5},
		},
		redemptions: []*RedemptionResult{
			{MarketID: "m1", WinningOutcome: "Up", WinningShares: 10},
		},
	}
	if roundHistoryHasOpenInventory(entry) {
		t.Fatal("expected redeemed market residual snapshot not to count as open inventory")
	}
}

func TestRoundHistoryHasOpenInventoryKeepsOtherMarketsOpen(t *testing.T) {
	entry := RoundHistoryEntry{
		positions: map[string]Position{
			"m1:Up": {MarketID: "m1", Outcome: "Up", Quantity: 0.5},
			"m2:Up": {MarketID: "m2", Outcome: "Up", Quantity: 1.0},
		},
		redemptions: []*RedemptionResult{
			{MarketID: "m1", WinningOutcome: "Up", WinningShares: 10},
		},
	}
	if !roundHistoryHasOpenInventory(entry) {
		t.Fatal("expected unresolved other-market inventory to keep row open")
	}
}

func TestRecordOrderDefaultsToTakerMode(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.RecordOrder("BTC", "Down", "BUY", 10, 0.84, 8.4, 0.0, 0.0, "FILLED")

	history := tui.GetOrderHistory()
	if len(history) != 1 {
		t.Fatalf("expected 1 order history entry, got %d", len(history))
	}
	if history[0].ExecutionMode != "taker" {
		t.Fatalf("expected default execution mode taker, got %q", history[0].ExecutionMode)
	}
}

func TestLogEventDedupSuppressesRepeatedMessages(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())

	if !tui.LogEventDedup("wallet-sync:BTC", time.Minute, "[%s] sync failed", "BTC") {
		t.Fatal("expected first deduped log to be recorded")
	}
	if tui.LogEventDedup("wallet-sync:BTC", time.Minute, "[%s] sync failed", "BTC") {
		t.Fatal("expected duplicate deduped log to be suppressed")
	}
	if got := len(tui.eventLog); got != 1 {
		t.Fatalf("expected 1 event log entry after suppression, got %d", got)
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
