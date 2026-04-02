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

func TestTUI_SetWalletTruthPositionsPreservesResolutionStateAcrossRefresh(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)

	tui.SetWalletTruthPositions("BTC#1", []WalletTruthPosition{{
		MarketID:         "BTC#1",
		Outcome:          "Up",
		LocalShares:      3.25,
		OnChainShares:    3.25,
		IsWinner:         true,
		Redeemable:       true,
		ResolutionStatus: "redeemable",
	}})

	tui.SetWalletTruthPositions("BTC#1", []WalletTruthPosition{{
		MarketID:      "BTC#1",
		Outcome:       "Up",
		LocalShares:   3.25,
		OnChainShares: 3.25,
	}})

	got := tui.getWalletTruthPositions()
	if len(got) != 1 {
		t.Fatalf("expected 1 wallet truth position, got %d", len(got))
	}
	if !got[0].IsWinner || !got[0].Redeemable || got[0].ResolutionStatus != "redeemable" {
		t.Fatalf("expected refresh to preserve resolution state, got %+v", got[0])
	}
}

func TestTUI_UpdateWalletTruthResolutionMatchesTrimmedWinner(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)

	tui.SetWalletTruthPositions("BTC#2", []WalletTruthPosition{
		{MarketID: "BTC#2", Outcome: "Up", OnChainShares: 3.25},
		{MarketID: "BTC#2", Outcome: "Down", OnChainShares: 0},
	})

	tui.UpdateWalletTruthResolution("BTC#2", true, " up ")

	got := tui.getWalletTruthPositions()
	if len(got) != 2 {
		t.Fatalf("expected 2 wallet truth positions, got %d", len(got))
	}
	for _, pos := range got {
		switch pos.Outcome {
		case "Up":
			if !pos.IsWinner || !pos.Redeemable || pos.ResolutionStatus != "redeemable" {
				t.Fatalf("expected Up to be recognized as winner, got %+v", pos)
			}
		case "Down":
			if pos.IsWinner || pos.Redeemable || pos.ResolutionStatus != "resolved" {
				t.Fatalf("expected Down to be resolved loser, got %+v", pos)
			}
		}
	}
}

func TestDisplayedTradeBudgetsUsesEquityAndCompoundInPaperMode(t *testing.T) {
	base, effective := displayedTradeBudgets("Paper", 75, 100, 100, 100, 0.10, 0, 1.12)
	if base != 10 {
		t.Fatalf("expected paper base trade budget 10.00, got %.2f", base)
	}
	if diff := effective - 11.2; diff < -0.000001 || diff > 0.000001 {
		t.Fatalf("expected compounded effective budget 11.20, got %.2f", effective)
	}
}

func TestDisplayedTradeBudgetsUsesStartingBalanceFloorInRealMode(t *testing.T) {
	base, effective := displayedTradeBudgets("Real", 50, 100, 100, 100, 0.10, 0, 1.50)
	if base != 10 {
		t.Fatalf("expected real trade budget to keep session-start floor, got %.2f", base)
	}
	if effective != 10 {
		t.Fatalf("expected real effective budget to ignore paper compounding, got %.2f", effective)
	}
}

func TestDisplayedTradeBudgetsCompoundsUpInRealModeWhenCashGrows(t *testing.T) {
	base, effective := displayedTradeBudgets("Real", 125, 100, 100, 125, 0.10, 0, 1.50)
	if base != 12.5 {
		t.Fatalf("expected real trade budget to grow with cash, got %.2f", base)
	}
	if effective != 12.5 {
		t.Fatalf("expected real effective budget to match base in real mode, got %.2f", effective)
	}
}

func TestDisplayedTradeBudgetsUsesCurrentEquityInRealModeAfterDrawdown(t *testing.T) {
	base, effective := displayedTradeBudgets("Real", 100, 100, 100, 120, 0.10, 0, 1.0)
	if base != 10 {
		t.Fatalf("expected real trade budget to follow current equity after drawdown, got %.2f", base)
	}
	if effective != 10 {
		t.Fatalf("expected real effective budget to match current equity base, got %.2f", effective)
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

func TestDisplayedTradeBudgetsUsesEquityInsteadOfCashOnlyInRealMode(t *testing.T) {
	base, effective := displayedTradeBudgets("Real", 90, 100, 100, 90, 0.10, 0, 1.50)
	if base != 10 {
		t.Fatalf("expected real trade budget to use current equity, not cash-only sizing, got %.2f", base)
	}
	if effective != 10 {
		t.Fatalf("expected real effective budget to match base in real mode, got %.2f", effective)
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

func TestRenderMarketPanelUsesRecentLastGoodQuotesDuringBriefGap(t *testing.T) {
	model := tuiModel{}
	mkt := &MarketData{
		Slug:        "btc-market",
		Outcomes:    []string{"Yes", "No"},
		EndTime:     time.Now().Add(10 * time.Minute),
		LastUpdate:  time.Now(),
		Bids:        map[string]float64{"Yes": 0, "No": 0},
		Asks:        map[string]float64{"Yes": 0, "No": 0},
		ClearedBids: map[string]bool{"Yes": false, "No": false},
		ClearedAsks: map[string]bool{"Yes": false, "No": false},
		RealBids:    map[string]float64{"Yes": 0.41, "No": 0.57},
		RealAsks:    map[string]float64{"Yes": 0.43, "No": 0.59},
		DataSource:  "WS",
	}

	rendered, _ := model.renderMarketPanel("BTC", mkt, 80, nil)
	if strings.Contains(rendered, "awaiting price data") {
		t.Fatalf("expected recent last-good quotes to be displayed, got %q", rendered)
	}
	if !strings.Contains(rendered, "$0.43") || !strings.Contains(rendered, "$0.59") {
		t.Fatalf("expected last-good asks to be rendered, got %q", rendered)
	}
}

func TestRenderMarketPanelDoesNotReuseExplicitlyClearedQuotes(t *testing.T) {
	model := tuiModel{}
	mkt := &MarketData{
		Slug:        "btc-market",
		Outcomes:    []string{"Yes", "No"},
		EndTime:     time.Now().Add(10 * time.Minute),
		LastUpdate:  time.Now(),
		Bids:        map[string]float64{"Yes": 0, "No": 0},
		Asks:        map[string]float64{"Yes": 0, "No": 0},
		ClearedBids: map[string]bool{"Yes": true, "No": true},
		ClearedAsks: map[string]bool{"Yes": true, "No": true},
		RealBids:    map[string]float64{"Yes": 0.41, "No": 0.57},
		RealAsks:    map[string]float64{"Yes": 0.43, "No": 0.59},
		DataSource:  "WS",
	}

	rendered, _ := model.renderMarketPanel("BTC", mkt, 80, nil)
	if strings.Contains(rendered, "$0.43") || strings.Contains(rendered, "$0.59") {
		t.Fatalf("expected explicitly cleared quotes to stay hidden, got %q", rendered)
	}
	if !strings.Contains(rendered, "Waiting for liquidity") {
		t.Fatalf("expected explicitly cleared quotes to show no-liquidity state, got %q", rendered)
	}
}

func TestRenderMarketPanelShowsAwaitingWhenGapIsStale(t *testing.T) {
	model := tuiModel{}
	mkt := &MarketData{
		Slug:       "btc-market",
		Outcomes:   []string{"Yes", "No"},
		EndTime:    time.Now().Add(10 * time.Minute),
		LastUpdate: time.Now().Add(-(recentQuoteDisplayGrace + 250*time.Millisecond)),
		Bids:       map[string]float64{"Yes": 0, "No": 0},
		Asks:       map[string]float64{"Yes": 0, "No": 0},
		RealBids:   map[string]float64{"Yes": 0.41, "No": 0.57},
		RealAsks:   map[string]float64{"Yes": 0.43, "No": 0.59},
		DataSource: "WS",
	}

	rendered, _ := model.renderMarketPanel("BTC", mkt, 80, nil)
	if !strings.Contains(rendered, "awaiting price data") {
		t.Fatalf("expected stale gap to show awaiting state, got %q", rendered)
	}
}

func TestRenderMarketPanelShowsBinanceSignalStatus(t *testing.T) {
	model := tuiModel{}
	mkt := &MarketData{
		Slug:       "btc-market",
		Outcomes:   []string{"Yes", "No"},
		EndTime:    time.Now().Add(10 * time.Minute),
		LastUpdate: time.Now(),
		Bids:       map[string]float64{"Yes": 0.41, "No": 0.57},
		Asks:       map[string]float64{"Yes": 0.43, "No": 0.59},
		RealBids:   map[string]float64{"Yes": 0.41, "No": 0.57},
		RealAsks:   map[string]float64{"Yes": 0.43, "No": 0.59},
		DataSource: "WS",
		BinanceSignal: MarketBinanceSignal{
			Enabled:                true,
			Symbol:                 "BTCUSDT",
			Price:                  84250.5,
			DeltaPercent:           0.642,
			TargetOutcome:          "Yes",
			PolyFavorableMoveCents: 0.8,
			PolyAdverseMoveCents:   0.1,
			TargetSpreadCents:      0.4,
			Ready:                  true,
			Status:                 "ready",
			Reason:                 "signal ready",
		},
	}

	rendered, _ := model.renderMarketPanel("BTC", mkt, 80, nil)
	for _, want := range []string{"BTCUSDT", "0.642%", "Yes", "0.80c", "READY", "signal ready"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("expected Binance status %q in panel, got %q", want, rendered)
		}
	}
}

func TestRenderMarketPanelHidesGapWhenCurrentPairQuotesAreStale(t *testing.T) {
	model := tuiModel{}
	mkt := &MarketData{
		Slug:       "btc-market",
		Outcomes:   []string{"Yes", "No"},
		EndTime:    time.Now().Add(10 * time.Minute),
		LastUpdate: time.Now().Add(-(recentQuoteDisplayGrace + 250*time.Millisecond)),
		Bids:       map[string]float64{"Yes": 0.41, "No": 0.57},
		Asks:       map[string]float64{"Yes": 0.43, "No": 0.59},
		RealBids:   map[string]float64{"Yes": 0.41, "No": 0.57},
		RealAsks:   map[string]float64{"Yes": 0.43, "No": 0.59},
		DataSource: "WS",
	}

	rendered, _ := model.renderMarketPanel("BTC", mkt, 80, nil)
	if strings.Contains(rendered, "Buy $") {
		t.Fatalf("expected stale current pair quotes to suppress gap line, got %q", rendered)
	}
	if !strings.Contains(rendered, "awaiting price data") {
		t.Fatalf("expected stale current pair quotes to show awaiting state, got %q", rendered)
	}
}

func TestRenderMarketPanelTreatsRecentDepthChangeAsFresh(t *testing.T) {
	model := tuiModel{}
	mkt := &MarketData{
		Slug:            "btc-market",
		Outcomes:        []string{"Yes", "No"},
		EndTime:         time.Now().Add(10 * time.Minute),
		LastUpdate:      time.Now().Add(-(recentQuoteDisplayGrace + 250*time.Millisecond)),
		LastDepthUpdate: time.Now().Add(-250 * time.Millisecond),
		Bids:            map[string]float64{"Yes": 0.41, "No": 0.57},
		Asks:            map[string]float64{"Yes": 0.43, "No": 0.59},
		RealBids:        map[string]float64{"Yes": 0.41, "No": 0.57},
		RealAsks:        map[string]float64{"Yes": 0.43, "No": 0.59},
		DataSource:      "WS",
	}

	rendered, _ := model.renderMarketPanel("BTC", mkt, 80, nil)
	if strings.Contains(rendered, "awaiting price data") {
		t.Fatalf("expected recent depth activity to keep panel fresh, got %q", rendered)
	}
	if !strings.Contains(rendered, "Buy $") {
		t.Fatalf("expected spread line with recent depth activity, got %q", rendered)
	}
}

func TestRenderMarketPanelKeepsTerminalLastGoodQuotesVisible(t *testing.T) {
	model := tuiModel{}
	mkt := &MarketData{
		Slug:       "btc-market",
		Outcomes:   []string{"Down", "Up"},
		EndTime:    time.Now().Add(10 * time.Second),
		LastUpdate: time.Now().Add(-18 * time.Second),
		Bids:       map[string]float64{"Down": 0, "Up": 0},
		Asks:       map[string]float64{"Down": 0, "Up": 0},
		RealBids:   map[string]float64{"Down": 0.99, "Up": 0},
		RealAsks:   map[string]float64{"Down": 0, "Up": 0.01},
		DataSource: "WS",
	}

	rendered, _ := model.renderMarketPanel("BTC", mkt, 80, nil)
	if strings.Contains(rendered, "awaiting price data") {
		t.Fatalf("expected terminal last-good quotes to remain visible, got %q", rendered)
	}
	if !strings.Contains(rendered, "$0.99") || !strings.Contains(rendered, "$0.01") {
		t.Fatalf("expected terminal quotes to be rendered, got %q", rendered)
	}
}

func TestRenderPositionsHidesWalletTruthResolutionPanel(t *testing.T) {
	engine := NewEngine(1000.0)
	tui := NewTUI(engine, nil)
	tui.SetWalletTruthPositions("BTC", []WalletTruthPosition{
		{MarketID: "BTC", Outcome: "Up", LocalShares: 10, OnChainShares: 10, Drift: 0, ResolutionStatus: "unresolved"},
		{MarketID: "BTC", Outcome: "Down", LocalShares: 0, OnChainShares: 10, Drift: 10, ResolutionStatus: "redeemable", Redeemable: true, IsWinner: true},
	})

	model := tuiModel{tui: tui, snap: tuiSnapshot{walletTruth: tui.getWalletTruthPositions()}}
	rendered := model.renderPositions(120, nil)
	if strings.Contains(rendered, "WALLET TRUTH") {
		t.Fatalf("expected wallet-truth detail panel to stay hidden, got %q", rendered)
	}
	if !strings.Contains(rendered, "ON-CHAIN INVENTORY") {
		t.Fatalf("expected on-chain inventory panel to stay visible, got %q", rendered)
	}
	if !strings.Contains(rendered, "Up: 10 ") || !strings.Contains(rendered, "Down: 10 ") {
		t.Fatalf("expected on-chain inventory rows to be rendered, got %q", rendered)
	}
}

func TestRenderPositionsShowsOnChainInventoryFromWalletTruth(t *testing.T) {
	engine := NewEngine(1000.0)
	tui := NewTUI(engine, nil)
	tui.SetWalletTruthPositions("SOL", []WalletTruthPosition{
		{MarketID: "SOL", Outcome: "Up", OnChainShares: 3.5, ResolutionStatus: "unresolved"},
		{MarketID: "SOL", Outcome: "Down", OnChainShares: 1.25, ResolutionStatus: "redeemable", Redeemable: true, IsWinner: true},
	})

	model := tuiModel{tui: tui, snap: tuiSnapshot{walletTruth: tui.getWalletTruthPositions()}}
	rendered := model.renderPositions(120, nil)
	if !strings.Contains(rendered, "ON-CHAIN INVENTORY") {
		t.Fatalf("expected on-chain inventory section, got %q", rendered)
	}
	if strings.Contains(rendered, "WALLET TRUTH") {
		t.Fatalf("expected wallet-truth detail panel to stay hidden, got %q", rendered)
	}
	if !strings.Contains(rendered, "Up: 3.5") || !strings.Contains(rendered, "OPEN") {
		t.Fatalf("expected unresolved on-chain inventory row, got %q", rendered)
	}
	if !strings.Contains(rendered, "Down: 1.25") || !strings.Contains(rendered, "REDEEMABLE") {
		t.Fatalf("expected redeemable on-chain inventory row, got %q", rendered)
	}
}

func TestRenderPositionsShowsSyncingInventoryUntilChainCatchesUp(t *testing.T) {
	engine := NewEngine(1000.0)
	tui := NewTUI(engine, nil)
	tui.SetWalletTruthPositions("BTC", []WalletTruthPosition{
		{MarketID: "BTC", Outcome: "Up", LocalShares: 3.21, OnChainShares: 0, ResolutionStatus: "unresolved"},
	})

	model := tuiModel{tui: tui, snap: tuiSnapshot{walletTruth: tui.getWalletTruthPositions()}}
	rendered := model.renderPositions(120, nil)
	if !strings.Contains(rendered, "ON-CHAIN INVENTORY") {
		t.Fatalf("expected syncing inventory section, got %q", rendered)
	}
	if !strings.Contains(rendered, "Up: 3.21") || !strings.Contains(rendered, "SYNCING") {
		t.Fatalf("expected syncing inventory row, got %q", rendered)
	}
}

func TestRenderPositionsHidesResolvedLosersFromOnChainInventory(t *testing.T) {
	engine := NewEngine(1000.0)
	tui := NewTUI(engine, nil)
	tui.SetWalletTruthPositions("ETH#93fc5536", []WalletTruthPosition{
		{MarketID: "ETH#93fc5536", Outcome: "Up", OnChainShares: 4.0025, ResolutionStatus: "resolved"},
	})

	model := tuiModel{tui: tui, snap: tuiSnapshot{walletTruth: tui.getWalletTruthPositions()}}
	rendered := model.renderPositions(120, nil)

	if strings.Contains(rendered, "ON-CHAIN INVENTORY") || strings.Contains(rendered, "WALLET TRUTH") {
		t.Fatalf("expected resolved loser-only wallet-truth sections to stay hidden, got %q", rendered)
	}
	if !strings.Contains(rendered, "(none)") {
		t.Fatalf("expected positions panel to collapse for resolved loser-only inventory, got %q", rendered)
	}
}

func TestRenderPositionsHidesInFlightSectionWhenTakerCloseEnabled(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), nil)
	tui.InitSettings(TUISettings{
		PaperArbMode:         "taker",
		TakerCloseMarket:     true,
		TakerCloseMarketTime: 10,
	}, nil)

	positions := map[string]PositionPnL{
		"ETH#0c319cc1:Down": {
			Position: Position{
				MarketID:  "ETH#0c319cc1",
				Outcome:   "Down",
				Quantity:  3,
				AvgPrice:  0.99,
				TotalCost: 2.97,
			},
			CurrentBid: 0.56,
		},
	}

	model := tuiModel{tui: tui}
	rendered := model.renderPositions(120, positions)

	if strings.Contains(rendered, "IN-FLIGHT") || strings.Contains(rendered, "awaiting merge") {
		t.Fatalf("expected in-flight merge section to be hidden in taker-close mode, got %q", rendered)
	}
	if strings.Contains(rendered, "Down: 3@$0.99") {
		t.Fatalf("expected in-flight position rows to be hidden in taker-close mode, got %q", rendered)
	}
	if !strings.Contains(rendered, "(none)") {
		t.Fatalf("expected positions panel to collapse when only in-flight rows were suppressed, got %q", rendered)
	}
}

func TestRenderPositionsUsesNeutralHeaderInCopytradeMode(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), nil)
	tui.InitSettings(TUISettings{
		PaperArbMode: "copytrade",
	}, nil)

	positions := map[string]PositionPnL{
		"BTC#copy:Down": {
			Position: Position{
				MarketID:  "BTC#copy",
				Outcome:   "Down",
				Quantity:  6,
				AvgPrice:  0.93,
				TotalCost: 5.58,
			},
			CurrentBid: 0.95,
		},
	}

	model := tuiModel{tui: tui}
	rendered := model.renderPositions(120, positions)

	if strings.Contains(rendered, "IN-FLIGHT") || strings.Contains(rendered, "awaiting merge") {
		t.Fatalf("expected copytrade positions to avoid merge-oriented header, got %q", rendered)
	}
	if !strings.Contains(rendered, "POSITIONS") {
		t.Fatalf("expected copytrade positions header, got %q", rendered)
	}
}

func TestNormalizeTUISettingsNormalizesExecutionFloorToNonPositiveDecimal(t *testing.T) {
	cfg := normalizeTUISettings(TUISettings{BuyExecutionMarginFloorPercent: -1.0})
	if math.Abs(cfg.BuyExecutionMarginFloorPercent-(-0.01)) > 0.000001 {
		t.Fatalf("expected legacy -1.0 input to normalize to -0.01, got %.6f", cfg.BuyExecutionMarginFloorPercent)
	}

	cfg = normalizeTUISettings(TUISettings{BuyExecutionMarginFloorPercent: 0.03})
	if cfg.BuyExecutionMarginFloorPercent != 0 {
		t.Fatalf("expected positive execution floor to clamp to 0, got %.6f", cfg.BuyExecutionMarginFloorPercent)
	}
}

func TestNormalizeTUISettingsRoundsTakerClosePricesToDisplayPrecision(t *testing.T) {
	cfg := normalizeTUISettings(TUISettings{
		TakerCloseMarketSlippage: 0.9051,
		TakerCloseMarketMinPrice: 0.895,
	})
	if math.Abs(cfg.TakerCloseMarketMinPrice-0.90) > 0.000001 {
		t.Fatalf("expected taker-close min to round to 0.90, got %.6f", cfg.TakerCloseMarketMinPrice)
	}
	if math.Abs(cfg.TakerCloseMarketSlippage-0.91) > 0.000001 {
		t.Fatalf("expected taker-close slippage to round to 0.91, got %.6f", cfg.TakerCloseMarketSlippage)
	}

	cfg = normalizeTUISettings(TUISettings{
		TakerCloseMarketSlippage: 0.80,
		TakerCloseMarketMinPrice: 0.895,
	})
	if math.Abs(cfg.TakerCloseMarketMinPrice-0.90) > 0.000001 {
		t.Fatalf("expected taker-close min to round to 0.90, got %.6f", cfg.TakerCloseMarketMinPrice)
	}
	if math.Abs(cfg.TakerCloseMarketSlippage-0.90) > 0.000001 {
		t.Fatalf("expected slippage to clamp up to normalized min 0.90, got %.6f", cfg.TakerCloseMarketSlippage)
	}
}

func TestRenderSettingsShowsExecutionFloorAsPercent(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)
	tui.InitSettings(TUISettings{
		PaperArbMode:                   "taker",
		BuyExecutionMarginFloorPercent: -0.01,
	}, nil)

	view := (tuiModel{tui: tui}).renderSettings(120)
	if !strings.Contains(view, "Max Exec Slip %") {
		t.Fatalf("expected updated execution floor label, got %q", view)
	}
	if !strings.Contains(view, "-1.0%") {
		t.Fatalf("expected execution floor to render as percent, got %q", view)
	}
}

func TestNormalizeTUISettingsRoundsFixedTradeSizeUSDC(t *testing.T) {
	cfg := normalizeTUISettings(TUISettings{
		TradeSizingMode: core.TradeSizingModeUSDC,
		TradeSizeUSDC:   2.34,
	})
	if cfg.TradeSizingMode != core.TradeSizingModeUSDC {
		t.Fatalf("expected trade sizing mode usdc, got %q", cfg.TradeSizingMode)
	}
	if cfg.TradeSizeUSDC != 2.3 {
		t.Fatalf("expected trade size to round to 2.3, got %.1f", cfg.TradeSizeUSDC)
	}
}

func TestRenderSettingsShowsFixedTradeSizeControls(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)
	tui.InitSettings(TUISettings{
		TradeSizingMode: core.TradeSizingModeUSDC,
		TradeSizeUSDC:   2.3,
	}, nil)

	view := (tuiModel{tui: tui}).renderSettings(120)
	for _, want := range []string{"Trade Size Mode", "Trade Size (USDC)", "USDC", "$2.30", "Fixed sizing active"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected fixed trade sizing controls to render %q, got %q", want, view)
		}
	}
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

func TestSettingsRowEditableDisablesSplitAndTakerOnlyRowsInMakerMode(t *testing.T) {
	cfg := TUISettings{PaperArbMode: "maker"}
	for _, idx := range []int{settingsRowExecutionSlip, settingsRowSplitMinMargin, settingsRowSplitStrategy, settingsRowSplitInitialCap, settingsRowSplitReplenishCap} {
		if settingsRowEditable(cfg, "Paper", idx) {
			t.Fatalf("expected row %d to be read-only in maker mode", idx)
		}
	}
	for _, idx := range []int{settingsRowMinMargin, settingsRowTakerCloseMarket, settingsRowMinAskPrice, settingsRowMaxAskPrice} {
		if !settingsRowEditable(cfg, "Paper", idx) {
			t.Fatalf("expected row %d to remain editable in maker mode", idx)
		}
	}
}

func TestIsRowVisibleKeepsCoreRowsVisibleWhenTakerCloseEnabled(t *testing.T) {
	cfg := TUISettings{PaperArbMode: "taker", TakerCloseMarket: true}
	for _, idx := range []int{settingsRowMarket, settingsRowMaxMarkets, settingsRowTimeframe, settingsRowTradeSizingMode, settingsRowTradeSizingValue, settingsRowPaperArbMode, settingsRowExecutionSlip, settingsRowTakerCloseMarket, settingsRowMaxTradeSize, settingsRowMaxDailyLoss, settingsRowExchange, settingsRowTakerCloseTime, settingsRowTakerCloseSlippage, settingsRowTakerCloseMinPrice, settingsRowTradingHoursMode} {
		if !isRowVisible(cfg, "Paper", idx) {
			t.Fatalf("expected row %d to remain visible with taker close enabled", idx)
		}
	}
}

func TestIsRowVisibleHidesUnrelatedRowsWhenTakerCloseEnabled(t *testing.T) {
	cfg := TUISettings{PaperArbMode: "taker", TakerCloseMarket: true}
	for _, idx := range []int{settingsRowMinMargin, settingsRowSplitMinMargin, settingsRowSplitStrategy, settingsRowSplitInitialCap, settingsRowSplitReplenishCap, settingsRowMinAskPrice, settingsRowMaxAskPrice} {
		if isRowVisible(cfg, "Paper", idx) {
			t.Fatalf("expected row %d to be hidden with taker close enabled", idx)
		}
	}
}

func TestIsRowVisibleShowsMaxAskPriceInTakerMode(t *testing.T) {
	cfg := TUISettings{PaperArbMode: "taker"}
	if !isRowVisible(cfg, "Paper", settingsRowMaxAskPrice) {
		t.Fatalf("expected Max Ask Price row to remain visible in taker mode")
	}
}

func TestIsRowVisibleShowsCopytradeSlippageAndHidesPriceRows(t *testing.T) {
	cfg := TUISettings{PaperArbMode: "copytrade"}
	if !isRowVisible(cfg, "Paper", settingsRowExecutionSlip) {
		t.Fatalf("expected copytrade slippage row to remain visible in copytrade mode")
	}
	for _, idx := range []int{settingsRowMinAskPrice, settingsRowMaxAskPrice} {
		if isRowVisible(cfg, "Paper", idx) {
			t.Fatalf("expected price row %d to be hidden in copytrade mode", idx)
		}
	}
}

func TestIsRowVisibleShowsPaperBinanceDelayOnlyInPaperMode(t *testing.T) {
	cfg := TUISettings{PaperArbMode: "binance-gap"}
	if !isRowVisible(cfg, "Paper", settingsRowBinanceExecutionDelay) {
		t.Fatalf("expected paper Binance execution delay row to be visible in Paper mode")
	}
	if isRowVisible(cfg, "Real", settingsRowBinanceExecutionDelay) {
		t.Fatalf("expected paper Binance execution delay row to be hidden in Real mode")
	}
}

func TestFormatDisplayShareQtyKeepsFiveDecimalInventoryPrecision(t *testing.T) {
	if got := formatDisplayShareQty(1.234567); got != "1.23457" {
		t.Fatalf("expected 5-decimal share precision, got %q", got)
	}
	if got := formatSignedDisplayShareQty(-0.123456); got != "-0.12346" {
		t.Fatalf("expected signed 5-decimal share precision, got %q", got)
	}
}

func TestRenderSettingsShowsMakerSpecificLabels(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)
	tui.InitSettings(TUISettings{
		PaperArbMode:            "maker",
		TradeScaleFactor:        0.05,
		MinMarginPercent:        2.0,
		MakerQuoteGap:           0.006,
		MaxAskPrice:             0.90,
		MakerMergeBufferSeconds: 45,
	}, nil)

	view := (tuiModel{tui: tui}).renderSettings(120)
	for _, want := range []string{"Maker Min Sell Edge %", "Maker Merge Buffer", "Maker Max Buy Price", "Maker Quote Gap", "ignored live"} {
		if !strings.Contains(view, want) {
			t.Fatalf("renderSettings() missing %q\n%s", want, view)
		}
	}
}

func TestRenderSettingsHidesUnrelatedLabelsInTakerCloseMode(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)
	tui.InitSettings(TUISettings{
		PaperArbMode:             "taker",
		TakerCloseMarket:         true,
		MinMarginPercent:         2.0,
		SplitMinMarginSell:       3.0,
		SplitStrategyEnabled:     true,
		MinAskPrice:              0.10,
		MaxAskPrice:              0.90,
		TakerCloseMarketTime:     10,
		TakerCloseMarketSlippage: 0.99,
		TakerCloseMarketMinPrice: 0.60,
	}, nil)

	view := (tuiModel{tui: tui}).renderSettings(120)
	for _, hidden := range []string{"Buy Min Margin %", "Split Min Margin", "Split Strategy", "Min Ask Price", "Max Ask Price"} {
		if strings.Contains(view, hidden) {
			t.Fatalf("renderSettings() unexpectedly showed %q\n%s", hidden, view)
		}
	}
	for _, visible := range []string{"Taker Close Market", "Taker Close Time", "Taker Close Slippage", "Taker Close Min Price"} {
		if !strings.Contains(view, visible) {
			t.Fatalf("renderSettings() missing %q\n%s", visible, view)
		}
	}
}

func TestRenderSettingsShowsCopytradeSlippageAndHidesPriceRows(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)
	tui.InitSettings(TUISettings{
		PaperArbMode:            "copytrade",
		CopytradeTarget:         "0xabc",
		CopytradePollIntervalMs: 250,
		CopytradeSizingMode:     core.CopytradeSizingModeShares,
		CopytradeSizeShares:     5.5,
		CopytradeMaxSlippagePct: 1,
		MinAskPrice:             0.10,
		MaxAskPrice:             0.90,
	}, nil)

	view := (tuiModel{tui: tui}).renderSettings(120)
	if !strings.Contains(view, "Copy Max Slip") {
		t.Fatalf("expected copytrade slippage row, got %q", view)
	}
	if !strings.Contains(view, "1c") {
		t.Fatalf("expected copytrade slippage to render in cents, got %q", view)
	}
	if strings.Contains(view, "Min Ask Price") || strings.Contains(view, "Max Ask Price") {
		t.Fatalf("expected copytrade settings to hide generic price rows, got %q", view)
	}
}

func TestSettingsEnterDoesNotRequestRestart(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.InitSettings(TUISettings{PaperArbMode: "taker"}, nil)

	model := tuiModel{
		tui:            tui,
		showSettings:   true,
		settingsCursor: settingsRowMinMargin,
	}

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	updated := next.(tuiModel)

	if !updated.showSettings {
		t.Fatalf("expected settings overlay to stay open on enter")
	}
	if tui.GetAndClearRestart() {
		t.Fatalf("expected enter to avoid requesting a restart")
	}
}

func TestSettingsCloseKeyDoesNotRequestRestart(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.InitSettings(TUISettings{PaperArbMode: "taker"}, nil)

	model := tuiModel{
		tui:          tui,
		showSettings: true,
	}

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	updated := next.(tuiModel)

	if updated.showSettings {
		t.Fatalf("expected s to close the settings overlay")
	}
	if tui.GetAndClearRestart() {
		t.Fatalf("expected s to close settings without requesting a restart")
	}
}

func TestSettingsRestartKeyRequestsRestart(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.InitSettings(TUISettings{PaperArbMode: "taker"}, nil)

	model := tuiModel{
		tui:          tui,
		showSettings: true,
	}

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	updated := next.(tuiModel)

	if updated.showSettings {
		t.Fatalf("expected r to close the settings overlay")
	}
	if !tui.GetAndClearRestart() {
		t.Fatalf("expected r to request a restart")
	}
}

func TestInitSettingsKeepsLowCopytradePollInterval(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)
	tui.InitSettings(TUISettings{
		PaperArbMode:            "copytrade",
		CopytradePollIntervalMs: 100,
	}, nil)

	if got := tui.GetSettings().CopytradePollIntervalMs; got != 100 {
		t.Fatalf("expected 100ms copytrade poll interval, got %d", got)
	}
}

func TestInitSettingsAllowsZeroCopytradeSlippage(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)
	tui.InitSettings(TUISettings{
		PaperArbMode:            "copytrade",
		CopytradeMaxSlippagePct: 0,
	}, nil)

	if got := tui.GetSettings().CopytradeMaxSlippagePct; got != 0 {
		t.Fatalf("expected 0c copytrade slippage, got %.2f", got)
	}
}

func TestInitSettingsAllowsNinetyNineCentCopytradeSlippage(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)
	tui.InitSettings(TUISettings{
		PaperArbMode:            "copytrade",
		CopytradeMaxSlippagePct: 120,
	}, nil)

	if got := tui.GetSettings().CopytradeMaxSlippagePct; got != 99 {
		t.Fatalf("expected copytrade slippage to clamp at 99c, got %.2f", got)
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

func TestRenderOrderHistoryShowsExplicitCloseModeInsteadOfMaker(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.RecordOrderWithMode("ETH", "Up", "BUY", 12.85, 0.70, 8.99, 0.0, 0.0, "taker-close", "FILLED")

	model := tuiModel{snap: tuiSnapshot{orderHistory: tui.GetOrderHistory()}}
	rendered := model.renderOrderHistory(120, 5)

	if !strings.Contains(rendered, "close") {
		t.Fatalf("expected close-mode tag in order history, got %q", rendered)
	}
	if strings.Contains(rendered, "maker") {
		t.Fatalf("expected taker-close entry not to be labeled maker, got %q", rendered)
	}
}

func TestRenderAccountStatusFormatsRealizedFromRealizedPnL(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			mode:        "Paper",
			tradeFactor: 0.05,
		},
	}

	rendered := model.renderAccountStatus(120, Stats{
		CurrentBalance:  100,
		StartingBalance: 100,
		RealizedPnL:     -3,
	}, 0, 120, 120, 1.0, 100, 0, 0, 0, nil)

	if strings.Contains(rendered, "+$-3.00") {
		t.Fatalf("expected realized pnl to format from its own sign, got %q", rendered)
	}
	if !strings.Contains(rendered, "-$3.00") {
		t.Fatalf("expected realized pnl to render as -$3.00, got %q", rendered)
	}
}

func TestRenderAccountStatusUsesBookEquityForPaperTradeBudget(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			mode:        "Paper",
			tradeFactor: 0.05,
		},
	}

	rendered := model.renderAccountStatus(120, Stats{
		CurrentBalance:  94,
		StartingBalance: 100,
	}, 6, 97, 100, 1.0, 100, 0, 0, 0, map[string]Position{
		"m1:Up": {MarketID: "m1", Outcome: "Up", Quantity: 10, AvgPrice: 0.60, TotalCost: 6},
	})

	if !strings.Contains(rendered, ".00/trade") {
		t.Fatalf("expected paper trade budget to use neutral book equity, got %q", rendered)
	}
}

func TestRenderAccountStatusShowsFixedUSDCTradeBudget(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			mode:        "Paper",
			tradeFactor: 0.05,
			settings: TUISettings{
				TradeSizingMode: core.TradeSizingModeUSDC,
				TradeSizeUSDC:   2.3,
			},
		},
	}

	rendered := model.renderAccountStatus(120, Stats{
		CurrentBalance:  100,
		StartingBalance: 100,
	}, 0, 100, 100, 1.3, 100, 0, 0, 0, nil)

	if !strings.Contains(rendered, "Trade $2.30 fixed") {
		t.Fatalf("expected account status to show fixed trade sizing, got %q", rendered)
	}
	if strings.Contains(rendered, "effective") {
		t.Fatalf("expected fixed trade sizing to ignore paper compounding preview, got %q", rendered)
	}
}

func TestRenderAccountStatusFallsBackToNetChangeWhenFlat(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			mode:        "Paper",
			tradeFactor: 0.05,
		},
	}

	rendered := model.renderAccountStatus(120, Stats{
		CurrentBalance:  120,
		StartingBalance: 100,
		RealizedPnL:     0,
	}, 0, 120, 120, 1.0, 100, 0, 0, 0, nil)

	if !strings.Contains(rendered, "Realized +$20.00") {
		t.Fatalf("expected flat realized line to fall back to settled net change, got %q", rendered)
	}
}

func TestRenderAccountStatusRealModeUsesRealizedForEquityChangeDisplay(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			mode:        "Real",
			tradeFactor: 0.05,
		},
	}

	rendered := model.renderAccountStatus(120, Stats{
		CurrentBalance:  66.35,
		StartingBalance: 72.01,
		RealizedPnL:     1.08,
	}, 0.0, 66.35, 66.35, 1.0, 72.01, 2, 2, 0, nil)

	if !strings.Contains(rendered, "(+$1.08)") {
		t.Fatalf("expected real-mode account change to follow realized pnl, got %q", rendered)
	}
	if strings.Contains(rendered, "(-$5.66)") {
		t.Fatalf("expected baseline drift to be hidden from real-mode account change, got %q", rendered)
	}
	if !strings.Contains(rendered, "($3.60/trade)") {
		t.Fatalf("expected 5%% trade budget to keep the real-mode high-water floor, got %q", rendered)
	}
}

func TestRenderAccountStatusRealModeTakerCloseUsesCurrentEquityBudget(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			mode:        "Real",
			tradeFactor: 0.05,
			settings: TUISettings{
				TakerCloseMarket: true,
			},
		},
	}

	rendered := model.renderAccountStatus(120, Stats{
		CurrentBalance:  61.53,
		StartingBalance: 56.00,
		RealizedPnL:     5.59,
	}, 0.0, 61.53, 61.53, 1.0, 203.20, 4, 4, 0, nil)

	if !strings.Contains(rendered, "($3.08/trade)") {
		t.Fatalf("expected taker-close trade budget to follow current live equity, got %q", rendered)
	}
	if strings.Contains(rendered, "($10.16/trade)") {
		t.Fatalf("expected taker-close mode to ignore stale high-water sizing, got %q", rendered)
	}
}

func TestRenderAccountStatusDoesNotFallbackToNetChangeWhileWalletTruthInventoryOpen(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			mode:        "Real",
			tradeFactor: 0.05,
			walletTruth: []WalletTruthPosition{
				{
					MarketID:         "BTC#latefill",
					Outcome:          "Up",
					OnChainShares:    3.5,
					ResolutionStatus: "unresolved",
				},
			},
		},
	}

	rendered := model.renderAccountStatus(120, Stats{
		CurrentBalance:  96.9,
		StartingBalance: 100,
		RealizedPnL:     0,
	}, 0, 96.9, 96.9, 1.0, 100, 1, 0, 1, nil)

	if strings.Contains(rendered, "Realized -$3.10") {
		t.Fatalf("expected open wallet-truth inventory to suppress net-change realized fallback, got %q", rendered)
	}
	if strings.Contains(rendered, "W/L 0/1") {
		t.Fatalf("expected unresolved wallet-truth inventory to suppress round loss fallback, got %q", rendered)
	}
	if !strings.Contains(rendered, "Realized +$0.00") {
		t.Fatalf("expected realized pnl to stay neutral while inventory is unresolved, got %q", rendered)
	}
}

func TestRenderAccountStatusShowsWinRateAndWinLossCounts(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			mode:        "Paper",
			tradeFactor: 0.05,
		},
	}

	rendered := model.renderAccountStatus(120, Stats{
		CurrentBalance:  100,
		StartingBalance: 100,
		WinningTrades:   7,
		LosingTrades:    3,
	}, 0, 100, 100, 1.0, 100, 5, 3, 0, nil)

	if !strings.Contains(rendered, "Win 70%") {
		t.Fatalf("expected win rate in account status, got %q", rendered)
	}
	if !strings.Contains(rendered, "W/L 7/3") {
		t.Fatalf("expected win/loss counts in account status, got %q", rendered)
	}
}

func TestRenderAccountStatusUsesPositionWinLossFromOrderHistory(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			mode:        "Real",
			tradeFactor: 0.05,
			orderHistory: []OrderHistoryEntry{
				{MarketID: "BTC#m1", Outcome: "Up", Side: "SELL", Profit: 0.7, Status: "FILLED"},
				{MarketID: "ETH#m2", Outcome: "Down", Side: "SELL", Profit: -0.2, Status: "FILLED"},
				{MarketID: "BTC#m1", Outcome: "Up", Side: "SELL", Profit: 0.1, Status: "PARTIAL"},
			},
		},
	}

	rendered := model.renderAccountStatus(120, Stats{
		CurrentBalance:  100,
		StartingBalance: 100,
		WinningTrades:   9,
		LosingTrades:    1,
	}, 0, 100, 100, 1.0, 120, 4, 4, 0, nil)

	if !strings.Contains(rendered, "W/L 1/1") {
		t.Fatalf("expected W/L to be based on per-position realized result from order history, got %q", rendered)
	}
	if !strings.Contains(rendered, "Win 50%") {
		t.Fatalf("expected win rate to follow per-position W/L, got %q", rendered)
	}
}

func TestRenderAccountStatusFallsBackToRoundWinLossCounts(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			mode:        "Real",
			tradeFactor: 0.05,
		},
	}

	rendered := model.renderAccountStatus(120, Stats{
		CurrentBalance:  100,
		StartingBalance: 100,
	}, 0, 100, 100, 1.2, 120, 8, 3, 2, nil)

	if !strings.Contains(rendered, "Win 60%") {
		t.Fatalf("expected round win rate fallback in account status, got %q", rendered)
	}
	if !strings.Contains(rendered, "W/L 3/2") {
		t.Fatalf("expected round win/loss fallback in account status, got %q", rendered)
	}
	if strings.Contains(rendered, "profitable") {
		t.Fatalf("expected profitable-round text to be removed, got %q", rendered)
	}
	if !strings.Contains(rendered, "$6.00/trade") {
		t.Fatalf("expected real trade budget to follow high-water sizing, got %q", rendered)
	}
}

func TestRenderAccountStatusShowsResolutionEstimateForUnresolvedInventory(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			mode:        "Real",
			tradeFactor: 0.05,
		},
	}

	rendered := model.renderAccountStatus(120, Stats{
		CurrentBalance:  96.9,
		StartingBalance: 100,
		RealizedPnL:     0,
	}, 3.1, 100, 100, 1.0, 100, 0, 0, 0, map[string]Position{
		"m1:Up": {MarketID: "m1", Outcome: "Up", Quantity: 3.5, AvgPrice: 3.1 / 3.5, TotalCost: 3.1},
	})

	if !strings.Contains(rendered, "Resolve +$0.40/-$3.10") {
		t.Fatalf("expected account status to show resolution estimate, got %q", rendered)
	}
}

func TestRenderAccountStatusHidesArbAndResolveInCopytradeMode(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			mode: "Real",
			settings: TUISettings{
				PaperArbMode:            "copytrade",
				CopytradeSizingMode:     core.CopytradeSizingModeShares,
				CopytradeSizeShares:     5.51,
				CopytradeMaxSlippagePct: 5.0,
			},
		},
	}

	rendered := model.renderAccountStatus(120, Stats{
		CurrentBalance:  93.92,
		StartingBalance: 100.00,
		RealizedPnL:     0,
	}, 6.08, 99.70, 99.70, 1.0, 100.0, 0, 0, 0, map[string]Position{
		"m1:Up":   {MarketID: "m1", Outcome: "Up", Quantity: 2, AvgPrice: 0.25, TotalCost: 0.50},
		"m1:Down": {MarketID: "m1", Outcome: "Down", Quantity: 6, AvgPrice: 0.93, TotalCost: 5.58},
	})

	if !strings.Contains(rendered, "Copy 5.51 shares") {
		t.Fatalf("expected copytrade sizing label, got %q", rendered)
	}
	if strings.Contains(rendered, "Arb ") || strings.Contains(rendered, "Resolve ") {
		t.Fatalf("expected copytrade account status to hide arb/resolve metrics, got %q", rendered)
	}
}

func TestRenderAccountStatusShowsPercentCopytradeSizing(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			mode: "Real",
			settings: TUISettings{
				PaperArbMode:            "copytrade",
				CopytradeSizingMode:     core.CopytradeSizingModePercent,
				CopytradeSizePercent:    10.0,
				CopytradeMaxSlippagePct: 5.0,
			},
		},
	}

	rendered := model.renderAccountStatus(120, Stats{
		CurrentBalance:  100.0,
		StartingBalance: 100.0,
		RealizedPnL:     0,
	}, 0, 100, 100, 1.0, 100.0, 0, 0, 0, nil)

	if !strings.Contains(rendered, "Copy 10.0% master") {
		t.Fatalf("expected percent copytrade sizing label, got %q", rendered)
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

func TestNormalizeTUISettingsClampsMaxMarketsToSelectedAssets(t *testing.T) {
	got := normalizeTUISettings(TUISettings{MarketSlug: "btc", MaxMarkets: 4, Timeframe: "15m"})
	if got.MarketSlug != "BTC" {
		t.Fatalf("expected normalized market slug BTC, got %q", got.MarketSlug)
	}
	if got.MaxMarkets != 1 {
		t.Fatalf("expected single-market selection to clamp MaxMarkets to 1, got %d", got.MaxMarkets)
	}

	got = normalizeTUISettings(TUISettings{MarketSlug: "BTC,eth", MaxMarkets: 4, Timeframe: "15m"})
	if got.MarketSlug != "BTC,ETH" {
		t.Fatalf("expected normalized multi-market slug BTC,ETH, got %q", got.MarketSlug)
	}
	if got.MaxMarkets != 2 {
		t.Fatalf("expected two-market selection to clamp MaxMarkets to 2, got %d", got.MaxMarkets)
	}
}

func TestRenderAccountStatusShowsUSWeekdayGateStatus(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			mode:        "Paper",
			tradeFactor: 0.05,
			settings: TUISettings{
				TradingHoursMode: "weekdays trade only",
			},
		},
	}

	rendered := model.renderAccountStatus(120, Stats{
		CurrentBalance:  100,
		StartingBalance: 100,
	}, 0, 100, 100, 1.0, 100, 0, 0, 0, nil)

	if !strings.Contains(rendered, "US time") {
		t.Fatalf("expected account status to include US clock, got %q", rendered)
	}
	if !strings.Contains(rendered, "Weekday Gate") {
		t.Fatalf("expected account status to include weekday gate status, got %q", rendered)
	}
}
