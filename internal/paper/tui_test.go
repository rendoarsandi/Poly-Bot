package paper

import (
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"Market-bot/internal/core"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
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

func TestTUI_SetWalletTruthPositionsMarksDirty(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	startVersion := tui.snapshotVersion

	tui.SetWalletTruthPositions("BTC", []WalletTruthPosition{{
		MarketID:      "BTC",
		Outcome:       "Down",
		LocalShares:   2.5,
		OnChainShares: 2.5,
	}})
	if tui.snapshotVersion <= startVersion {
		t.Fatalf("expected wallet-truth set to mark snapshot dirty, version %d -> %d", startVersion, tui.snapshotVersion)
	}

	midVersion := tui.snapshotVersion
	tui.ClearWalletTruthPositions("BTC")
	if tui.snapshotVersion <= midVersion {
		t.Fatalf("expected wallet-truth clear to mark snapshot dirty, version %d -> %d", midVersion, tui.snapshotVersion)
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

func TestRenderMarketPanelOmitsDuplicateSlugLine(t *testing.T) {
	model := tuiModel{}
	marketID := "btc-updown-15m-1776820500"
	mkt := &MarketData{
		Slug:       marketID,
		Outcomes:   []string{"Up", "Down"},
		EndTime:    time.Now().Add(10 * time.Minute),
		LastUpdate: time.Now(),
		Bids:       map[string]float64{"Up": 0.27, "Down": 0.71},
		Asks:       map[string]float64{"Up": 0.29, "Down": 0.73},
		RealBids:   map[string]float64{"Up": 0.27, "Down": 0.71},
		RealAsks:   map[string]float64{"Up": 0.29, "Down": 0.73},
		DataSource: "WS",
	}

	rendered, _ := model.renderMarketPanel(marketID, mkt, 80, nil)
	if count := strings.Count(rendered, marketID); count != 1 {
		t.Fatalf("expected market slug to render once, got %d in %q", count, rendered)
	}
}

func TestRenderMarketPanelKeepsQuotesVisibleAcrossShortQuietWindow(t *testing.T) {
	model := tuiModel{}
	mkt := &MarketData{
		Slug:       "btc-market",
		Outcomes:   []string{"Yes", "No"},
		EndTime:    time.Now().Add(10 * time.Minute),
		LastUpdate: time.Now().Add(-2500 * time.Millisecond),
		Bids:       map[string]float64{"Yes": 0.41, "No": 0.57},
		Asks:       map[string]float64{"Yes": 0.43, "No": 0.59},
		RealBids:   map[string]float64{"Yes": 0.41, "No": 0.57},
		RealAsks:   map[string]float64{"Yes": 0.43, "No": 0.59},
		DataSource: "WS",
	}

	rendered, _ := model.renderMarketPanel("BTC", mkt, 80, nil)
	if strings.Contains(rendered, "awaiting price data") {
		t.Fatalf("expected short quiet window to keep quotes visible, got %q", rendered)
	}
	if !strings.Contains(rendered, "Buy $") {
		t.Fatalf("expected spread line to remain visible, got %q", rendered)
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
	for _, want := range []string{"BIN: $84250.50", "0.642%", "Yes", "READY", "signal ready"} {
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

func TestRenderPositionsOrdersUpBeforeDown(t *testing.T) {
	engine := NewEngine(1000.0)
	engine.UpdateMarketBidAsk("BTC", "Up", 0.90, 0.91)
	engine.UpdateMarketBidAsk("BTC", "Down", 0.10, 0.11)
	_, _ = engine.BuyForMarket("BTC", "Down", 0.13, 2.87128)
	_, _ = engine.BuyForMarket("BTC", "Up", 0.88, 6.07744)

	tui := NewTUI(engine, nil)
	model := tuiModel{
		tui: tui,
		snap: tuiSnapshot{
			mode:      "Real",
			settings:  TUISettings{PaperArbMode: "laddered-taker"},
			positions: engine.GetPositionsWithPnL(),
		},
	}

	rendered := model.renderPositions(120, engine.GetPositionsWithPnL())
	upIdx := strings.Index(rendered, "Up: 6.07744")
	downIdx := strings.Index(rendered, "Down: 2.87128")
	if upIdx == -1 || downIdx == -1 {
		t.Fatalf("expected both Up and Down inventory rows, got %q", rendered)
	}
	if upIdx > downIdx {
		t.Fatalf("expected Up to render before Down, got %q", rendered)
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

func TestRenderPositionsHidesDustOnlyOnChainInventory(t *testing.T) {
	engine := NewEngine(1000.0)
	tui := NewTUI(engine, nil)
	tui.SetWalletTruthPositions("BTC", []WalletTruthPosition{
		{MarketID: "BTC", Outcome: "Up", OnChainShares: 0.00359, ResolutionStatus: "unresolved"},
	})

	model := tuiModel{tui: tui, snap: tuiSnapshot{walletTruth: tui.getWalletTruthPositions()}}
	rendered := model.renderPositions(120, nil)

	if strings.Contains(rendered, "ON-CHAIN INVENTORY") || strings.Contains(rendered, "0.00359") {
		t.Fatalf("expected dust-only on-chain inventory to stay hidden, got %q", rendered)
	}
	if !strings.Contains(rendered, "(none)") {
		t.Fatalf("expected positions panel to collapse for dust-only on-chain inventory, got %q", rendered)
	}
}

func TestRenderPositionsHidesDustOnlyInFlightInventory(t *testing.T) {
	engine := NewEngine(1000.0)
	engine.UpdateMarketBidAsk("BTC", "Up", 0.99, 1.00)
	_, _ = engine.BuyForMarket("BTC", "Up", 0.50, 0.00359)

	tui := NewTUI(engine, nil)
	model := tuiModel{
		tui: tui,
		snap: tuiSnapshot{
			positions: engine.GetPositionsWithPnL(),
		},
	}

	rendered := model.renderPositions(120, engine.GetPositionsWithPnL())
	if strings.Contains(rendered, "IN-FLIGHT") || strings.Contains(rendered, "0.00359") {
		t.Fatalf("expected dust-only in-flight inventory to stay hidden, got %q", rendered)
	}
	if !strings.Contains(rendered, "(none)") {
		t.Fatalf("expected positions panel to collapse for dust-only in-flight inventory, got %q", rendered)
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

func TestRenderPositionsShowsWaitingToSellForPendingInventoryExit(t *testing.T) {
	engine := NewEngine(1000.0)
	if _, err := engine.BuyForMarket("btc-updown-1h-1700000000", "Up", 0.60, 5); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	model := tuiModel{
		snap: tuiSnapshot{
			markets: map[string]*MarketData{
				"btc-updown-1h-1700000000": {
					Slug:            "btc-updown-1h-1700000000",
					EndTime:         time.Now().Add(-time.Minute),
					InventoryStatus: "WAITING TO SELL",
				},
			},
			positions: engine.GetPositionsWithPnL(),
		},
	}

	rendered := model.renderPositions(120, engine.GetPositionsWithPnL())
	if !strings.Contains(rendered, "WAITING TO SELL") {
		t.Fatalf("expected positions panel to show waiting-to-sell status, got %q", rendered)
	}
	if strings.Contains(rendered, "WAITING RESOLUTION") {
		t.Fatalf("expected waiting-to-sell status to replace waiting resolution, got %q", rendered)
	}
}

func TestRenderPositionsInfersSlugEndTimeForCurrentCarry(t *testing.T) {
	start := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Second)
	marketID := fmt.Sprintf("btc-updown-15m-%d", start.Unix())
	engine := NewEngine(1000.0)
	if _, err := engine.BuyForMarket(marketID, "Up", 0.60, 5); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	model := tuiModel{
		snap: tuiSnapshot{
			positions: engine.GetPositionsWithPnL(),
		},
	}

	rendered := model.renderPositions(120, engine.GetPositionsWithPnL())
	if strings.Contains(rendered, "WAITING RESOLUTION") {
		t.Fatalf("expected current carry not to be marked waiting resolution, got %q", rendered)
	}
}

func TestRenderPositionsUsesFullMarketResolutionForUnevenTwoSidedCarry(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), nil)
	tui.InitSettings(TUISettings{
		PaperArbMode: "copytrade",
	}, nil)

	positions := map[string]PositionPnL{
		"ETH-5M#98024767:Down": {
			Position: Position{
				MarketID:  "ETH-5M#98024767",
				Outcome:   "Down",
				Quantity:  26.46,
				AvgPrice:  0.75,
				TotalCost: 19.845,
			},
			CurrentBid: 0.99,
		},
		"ETH-5M#98024767:Up": {
			Position: Position{
				MarketID:  "ETH-5M#98024767",
				Outcome:   "Up",
				Quantity:  13.86,
				AvgPrice:  0.48,
				TotalCost: 6.6528,
			},
			CurrentBid: 0.01,
		},
	}

	model := tuiModel{tui: tui}
	rendered := model.renderPositions(140, positions)

	if !strings.Contains(rendered, "Now: $-0.16") {
		t.Fatalf("expected full market current pnl, got %q", rendered)
	}
	if !strings.Contains(rendered, "Resolve: $-0.04/$-12.64") {
		t.Fatalf("expected full market resolution range, got %q", rendered)
	}
	if strings.Contains(rendered, "Locked: -$3.12") {
		t.Fatalf("expected matched-pair locked pnl to stay hidden for uneven carry, got %q", rendered)
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

func TestSettingsArbModesHideMakerForRealbotPaperBackend(t *testing.T) {
	modes := settingsArbModes(TUISettings{ExecutionBackend: core.ExecutionBackendPaper}, "Real")
	for _, mode := range modes {
		if mode == "maker" {
			t.Fatalf("expected realbot paper backend arb modes to hide maker, got %v", modes)
		}
	}
}

func TestSetModeCoercesMakerForRealbotPaperBackend(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.InitSettings(TUISettings{
		ExecutionBackend: core.ExecutionBackendPaper,
		PaperArbMode:     "maker",
	}, nil)

	tui.SetMode("Real")

	if got := tui.GetSettings().PaperArbMode; got != "taker" {
		t.Fatalf("expected realbot paper backend to coerce maker mode to taker, got %q", got)
	}
}

func TestSetModeDisablesSplitForRealbotPaperBackend(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.InitSettings(TUISettings{
		ExecutionBackend:     core.ExecutionBackendPaper,
		PaperArbMode:         "taker",
		SplitStrategyEnabled: true,
	}, nil)

	tui.SetMode("Real")

	if tui.GetSettings().SplitStrategyEnabled {
		t.Fatal("expected realbot paper backend to disable split strategy")
	}
}

func TestExecutionBackendChangeDoesNotDeadlockWhenSettingsCallbackLogs(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.SetMode("Real")

	callbackDone := make(chan TUISettings, 1)
	tui.InitSettings(TUISettings{
		ExecutionBackend: core.ExecutionBackendPaper,
		PaperArbMode:     "taker",
	}, func(s TUISettings) {
		tui.LogEvent("backend changed to %s", s.ExecutionBackend)
		callbackDone <- s
	})

	model := tuiModel{
		tui:            tui,
		showSettings:   true,
		settingsCursor: settingsRowExecutionBackend,
	}

	updateDone := make(chan tuiModel, 1)
	go func() {
		next, _ := model.Update(tea.KeyMsg{Type: tea.KeyRight})
		updateDone <- next.(tuiModel)
	}()

	select {
	case updated := <-updateDone:
		if got := updated.tui.GetSettings().ExecutionBackend; got != core.ExecutionBackendLive {
			t.Fatalf("expected execution backend to change to live, got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("settings update deadlocked when callback logged to TUI")
	}

	select {
	case got := <-callbackDone:
		if got.ExecutionBackend != core.ExecutionBackendLive {
			t.Fatalf("expected callback to receive live backend, got %q", got.ExecutionBackend)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected settings callback to complete")
	}
}

func TestSettingsPanelAutoScrollsSelectedRowIntoViewOnSmallTerminal(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.SetMode("Real")
	tui.InitSettings(TUISettings{
		ExecutionBackend: core.ExecutionBackendPaper,
		PaperArbMode:     "taker",
	}, nil)

	model := tuiModel{
		tui:          tui,
		showSettings: true,
		scrollOffset: 0,
		snap: tuiSnapshot{
			height: 10,
		},
	}
	model.settingsCursor = settingsRowPrivateKeyEdit
	model.ensureSettingsCursorVisible(tui.GetSettings(), "Real")

	rendered := model.renderSettings(100)
	if !strings.Contains(rendered, "Private Key") {
		t.Fatalf("expected scrolled settings view to include selected row, got %q", rendered)
	}
	if strings.Contains(rendered, "Market") {
		t.Fatalf("expected small-terminal settings view to scroll past top rows, got %q", rendered)
	}
}

func TestSettingsPanelMouseWheelScrollsViewport(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.SetMode("Real")
	tui.InitSettings(TUISettings{
		ExecutionBackend: core.ExecutionBackendPaper,
		PaperArbMode:     "taker",
	}, nil)

	model := tuiModel{
		tui:          tui,
		showSettings: true,
		scrollOffset: 0,
		snap: tuiSnapshot{
			height: 10,
		},
	}

	next, _ := model.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	updated := next.(tuiModel)
	if updated.scrollOffset <= 0 {
		t.Fatalf("expected mouse wheel down to scroll settings viewport, got offset %d", updated.scrollOffset)
	}

	next, _ = updated.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	updated = next.(tuiModel)
	if updated.scrollOffset < 0 {
		t.Fatalf("expected mouse wheel up to clamp scroll offset, got %d", updated.scrollOffset)
	}
}

func TestMainPanelMouseWheelScrollsViewport(t *testing.T) {
	model := tuiModel{
		scrollOffset: 0,
		snap: tuiSnapshot{
			width:  120,
			height: 10,
			eventLog: []string{
				"one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten",
				"eleven", "twelve", "thirteen", "fourteen", "fifteen",
			},
		},
	}
	model.refreshScrollMetrics()

	next, _ := model.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	updated := next.(tuiModel)
	if updated.scrollOffset <= 0 {
		t.Fatalf("expected mouse wheel down to scroll main viewport, got offset %d", updated.scrollOffset)
	}

	next, _ = updated.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	updated = next.(tuiModel)
	if updated.scrollOffset < 0 {
		t.Fatalf("expected mouse wheel up to clamp main viewport offset, got %d", updated.scrollOffset)
	}
}

func TestRefreshScrollMetricsIfNeededSkipsStableLayout(t *testing.T) {
	model := tuiModel{
		contentLines:   123,
		layoutVersion:  7,
		layoutWidth:    120,
		layoutHeight:   30,
		layoutSettings: false,
		snap: tuiSnapshot{
			version: 7,
			width:   120,
			height:  30,
		},
	}

	model.refreshScrollMetricsIfNeeded()

	if model.contentLines != 123 {
		t.Fatalf("expected stable layout refresh to be skipped, got contentLines=%d", model.contentLines)
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

func TestIsRowVisibleKeepsCoreRowsVisibleWhenTakerCloseEnabled(t *testing.T) {
	cfg := TUISettings{PaperArbMode: "taker", TakerCloseMarket: true}
	for _, idx := range []int{settingsRowMarket, settingsRowMaxMarkets, settingsRowTimeframe, settingsRowTradeSizingMode, settingsRowTradeSizingValue, settingsRowPaperArbMode, settingsRowExecutionSlip, settingsRowTakerCloseMarket, settingsRowMaxTradeSize, settingsRowMaxDailyLoss, settingsRowExchange, settingsRowTakerCloseTime, settingsRowTakerCloseSlippage, settingsRowTakerCloseMinPrice, settingsRowTradingHoursMode} {
		if !isRowVisible(cfg, "Paper", idx) {
			t.Fatalf("expected row %d to remain visible with taker close enabled", idx)
		}
	}
}

func TestIsRowVisibleHidesSplitRowsForRealbotPaperBackend(t *testing.T) {
	cfg := TUISettings{
		ExecutionBackend:     core.ExecutionBackendPaper,
		PaperArbMode:         "taker",
		SplitStrategyEnabled: true,
	}
	for _, idx := range []int{settingsRowSplitMinMargin, settingsRowSplitStrategy, settingsRowSplitInitialCap, settingsRowSplitReplenishCap} {
		if isRowVisible(cfg, "Real", idx) {
			t.Fatalf("expected split row %d to be hidden for realbot paper backend", idx)
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

func TestIsRowVisibleShowsRedeemGasOnlyForLivePolygonMode(t *testing.T) {
	cfg := TUISettings{Exchange: "polymarket", ExecutionBackend: core.ExecutionBackendLive}
	if !isRowVisible(cfg, "Real", settingsRowRedeemGasMode) {
		t.Fatal("expected redeem gas row to be visible for real live Polymarket mode")
	}

	cfg.ExecutionBackend = core.ExecutionBackendPaper
	if isRowVisible(cfg, "Real", settingsRowRedeemGasMode) {
		t.Fatal("expected redeem gas row to be hidden for paper backend")
	}

	cfg.ExecutionBackend = core.ExecutionBackendLive
	cfg.Exchange = "kalshi"
	if isRowVisible(cfg, "Real", settingsRowRedeemGasMode) {
		t.Fatal("expected redeem gas row to be hidden for Kalshi")
	}

	cfg.Exchange = "polymarket"
	if isRowVisible(cfg, "Paper", settingsRowRedeemGasMode) {
		t.Fatal("expected redeem gas row to be hidden in paper mode")
	}
}

func TestIsRowVisibleHidesUnrelatedRowsInLadderedMode(t *testing.T) {
	cfg := TUISettings{PaperArbMode: "laddered-taker"}
	for _, idx := range []int{
		settingsRowMinMargin,
		settingsRowExecutionSlip,
		settingsRowTakerCloseMarket,
		settingsRowSplitMinMargin,
		settingsRowSplitStrategy,
		settingsRowSplitInitialCap,
		settingsRowSplitReplenishCap,
	} {
		if isRowVisible(cfg, "Paper", idx) {
			t.Fatalf("expected row %d to be hidden in laddered mode", idx)
		}
	}
	for _, idx := range []int{
		settingsRowLadderCooldown,
		settingsRowTradeSizingMode,
		settingsRowTradeSizingValue,
		settingsRowMinAskPrice,
		settingsRowMaxAskPrice,
		settingsRowPaperArbMode,
	} {
		if !isRowVisible(cfg, "Paper", idx) {
			t.Fatalf("expected row %d to remain visible in laddered mode", idx)
		}
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

func TestRenderSettingsShowsRedeemGasSpeedInLiveMode(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)
	tui.SetMode("Real")
	tui.InitSettings(TUISettings{
		Exchange:         "polymarket",
		ExecutionBackend: core.ExecutionBackendLive,
		RedeemGasMode:    core.RedeemGasModeFast,
	}, nil)

	view := (tuiModel{tui: tui}).renderSettings(120)
	if !strings.Contains(view, "Redeem Gas Speed") {
		t.Fatalf("renderSettings() missing Redeem Gas Speed\n%s", view)
	}
	if !strings.Contains(view, "fast") {
		t.Fatalf("renderSettings() missing fast gas value\n%s", view)
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

func TestRenderSettingsShowsLadderCooldownAndHidesUnrelatedRows(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)
	tui.InitSettings(TUISettings{
		PaperArbMode:                   "laddered-taker",
		LadderedTakerSizingMode:        core.LadderedTakerSizingModeShares,
		LadderedTakerSizeShares:        3.5,
		LadderedTakerReentryMoveCents:  1.8,
		MinMarginPercent:               2.0,
		TakerCloseMarket:               true,
		BuyExecutionMarginFloorPercent: -0.02,
	}, nil)

	view := (tuiModel{tui: tui}).renderSettings(120)
	for _, want := range []string{"Ladder Re-entry Move", "1.8c"} {
		if !strings.Contains(view, want) {
			t.Fatalf("renderSettings() missing %q\n%s", want, view)
		}
	}
	for _, hidden := range []string{"Ladder Min Margin %", "Taker Close Market", "Max Exec Slip %"} {
		if strings.Contains(view, hidden) {
			t.Fatalf("renderSettings() unexpectedly showed %q\n%s", hidden, view)
		}
	}
}

func TestRenderSettingsShowsLadderUSDCSizingText(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)
	tui.InitSettings(TUISettings{
		PaperArbMode:                   "laddered-taker",
		LadderedTakerSizingMode:        core.LadderedTakerSizingModeUSDC,
		LadderedTakerSizeUSDC:          2.7,
		LadderedTakerSizeShares:        9.5,
		LadderedTakerReentryMoveCents:  2.0,
		BuyExecutionMarginFloorPercent: -0.02,
	}, nil)

	view := (tuiModel{tui: tui}).renderSettings(120)
	for _, want := range []string{"Ladder Size Mode", "Ladder Size (USDC)", "USDC", "$2.70", "Laddered taker USDC sizing active"} {
		if !strings.Contains(view, want) {
			t.Fatalf("renderSettings() missing %q\n%s", want, view)
		}
	}
	for _, hidden := range []string{"Ladder Size (Shares)", "9.5 shares per entry", "share sizing active"} {
		if strings.Contains(view, hidden) {
			t.Fatalf("renderSettings() unexpectedly showed %q\n%s", hidden, view)
		}
	}
}

func TestSettingsTradeSizingValueSupportsDirectTypedEdit(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)
	tui.InitSettings(TUISettings{
		PaperArbMode:            "laddered-taker",
		LadderedTakerSizingMode: core.LadderedTakerSizingModeShares,
		LadderedTakerSizeShares: 3.5,
	}, nil)

	model := tuiModel{
		tui:            tui,
		showSettings:   true,
		settingsCursor: settingsRowTradeSizingValue,
	}

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'4'}})
	updated := next.(tuiModel)
	if !updated.settingsEdit {
		t.Fatal("expected typing on the size row to enter edit mode")
	}
	if updated.settingsInput != "4" {
		t.Fatalf("expected typed input to seed edit buffer, got %q", updated.settingsInput)
	}

	for _, r := range []rune{'.', '2', '5'} {
		next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		updated = next.(tuiModel)
	}

	next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyEnter})
	updated = next.(tuiModel)
	if updated.settingsEdit {
		t.Fatal("expected enter to commit the typed size edit")
	}
	if got := tui.GetSettings().LadderedTakerSizeShares; got != 4.25 {
		t.Fatalf("expected typed ladder share size 4.25, got %.2f", got)
	}
}

func TestSettingsTradeSizingValueSupportsDirectTypedEditInLadderedUSDCMode(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.InitSettings(TUISettings{
		PaperArbMode:            "laddered-taker",
		LadderedTakerSizingMode: core.LadderedTakerSizingModeUSDC,
		LadderedTakerSizeUSDC:   1.0,
		LadderedTakerSizeShares: 7.5,
	}, nil)

	model := tuiModel{
		tui:            tui,
		showSettings:   true,
		settingsCursor: settingsRowTradeSizingValue,
	}

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	updated := next.(tuiModel)
	if !updated.settingsEdit {
		t.Fatal("expected typing on the ladder usdc size row to enter edit mode")
	}
	if updated.settingsInput != "2" {
		t.Fatalf("expected typed input to seed edit buffer, got %q", updated.settingsInput)
	}

	for _, r := range []rune{'.', '7'} {
		next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		updated = next.(tuiModel)
	}

	next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyEnter})
	updated = next.(tuiModel)
	if updated.settingsEdit {
		t.Fatal("expected enter to commit the typed ladder usdc edit")
	}
	if got := tui.GetSettings().LadderedTakerSizeUSDC; got != 2.7 {
		t.Fatalf("expected typed ladder usdc size 2.7, got %.2f", got)
	}
	if got := tui.GetSettings().LadderedTakerSizeShares; got != 7.5 {
		t.Fatalf("expected ladder share size to remain unchanged, got %.2f", got)
	}
}

func TestSettingsTradeSizingValueEnterUsesLadderedUSDCValueAfterModeSwitch(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.InitSettings(TUISettings{
		PaperArbMode:            "laddered-taker",
		LadderedTakerSizingMode: core.LadderedTakerSizingModeShares,
		LadderedTakerSizeShares: 4.25,
		LadderedTakerSizeUSDC:   1.7,
	}, nil)

	model := tuiModel{
		tui:            tui,
		showSettings:   true,
		settingsCursor: settingsRowTradeSizingMode,
	}

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyLeft})
	updated := next.(tuiModel)
	if got := tui.GetSettings().LadderedTakerSizingMode; got != core.LadderedTakerSizingModeUSDC {
		t.Fatalf("expected ladder sizing mode to switch to usdc, got %q", got)
	}

	updated.settingsCursor = settingsRowTradeSizingValue
	next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyEnter})
	updated = next.(tuiModel)
	if !updated.settingsEdit {
		t.Fatal("expected enter on the ladder size row to start edit mode")
	}
	if updated.settingsInput != "1.70" {
		t.Fatalf("expected usdc edit buffer to seed from ladder usdc value, got %q", updated.settingsInput)
	}
}

func TestSettingsExecutionSlipSupportsDirectTypedEditInCopytradeMode(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.InitSettings(TUISettings{
		PaperArbMode:            "copytrade",
		CopytradeMaxSlippagePct: 5,
	}, nil)

	model := tuiModel{
		tui:            tui,
		showSettings:   true,
		settingsCursor: settingsRowExecutionSlip,
	}

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'9'}})
	updated := next.(tuiModel)
	if !updated.settingsEdit {
		t.Fatal("expected typing on the copytrade slippage row to enter edit mode")
	}

	for _, r := range []rune{'9'} {
		next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		updated = next.(tuiModel)
	}

	next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyEnter})
	updated = next.(tuiModel)
	if updated.settingsEdit {
		t.Fatal("expected enter to commit the typed slippage edit")
	}
	if got := tui.GetSettings().CopytradeMaxSlippagePct; got != 99 {
		t.Fatalf("expected typed copytrade slippage 99c, got %.2f", got)
	}
}

func TestSettingsLadderSlippageSupportsArrowAndTypedEdit(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.InitSettings(TUISettings{
		PaperArbMode:                "laddered-taker",
		LadderedTakerMaxSlippagePct: 99,
	}, nil)

	model := tuiModel{
		tui:            tui,
		showSettings:   true,
		settingsCursor: settingsRowLadderSlippage,
	}

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyLeft})
	updated := next.(tuiModel)
	if got := tui.GetSettings().LadderedTakerMaxSlippagePct; got != 98 {
		t.Fatalf("expected left arrow to reduce ladder slippage to 98c, got %.2f", got)
	}

	next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyRight})
	updated = next.(tuiModel)
	if got := tui.GetSettings().LadderedTakerMaxSlippagePct; got != 99 {
		t.Fatalf("expected right arrow to restore ladder slippage to 99c, got %.2f", got)
	}

	next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyEnter})
	updated = next.(tuiModel)
	if !updated.settingsEdit {
		t.Fatal("expected enter on ladder slippage row to start edit mode")
	}

	updated.settingsInput = ""
	for _, r := range []rune{'1', '7'} {
		next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		updated = next.(tuiModel)
	}
	next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyEnter})
	updated = next.(tuiModel)
	if updated.settingsEdit {
		t.Fatal("expected enter to commit the typed ladder slippage edit")
	}
	if got := tui.GetSettings().LadderedTakerMaxSlippagePct; got != 17 {
		t.Fatalf("expected typed ladder slippage 17c, got %.2f", got)
	}
}

func TestSettingsMaxAskPriceSupportsEnterTypedEdit(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.InitSettings(TUISettings{
		PaperArbMode: "taker",
		MinAskPrice:  0.10,
		MaxAskPrice:  0.90,
	}, nil)

	model := tuiModel{
		tui:            tui,
		showSettings:   true,
		settingsCursor: settingsRowMaxAskPrice,
	}

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	updated := next.(tuiModel)
	if !updated.settingsEdit {
		t.Fatal("expected enter on max ask row to start edit mode")
	}

	updated.settingsInput = ""
	for _, r := range []rune{'0', '.', '9', '5'} {
		next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		updated = next.(tuiModel)
	}

	next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyEnter})
	updated = next.(tuiModel)
	if updated.settingsEdit {
		t.Fatal("expected enter to commit the typed max ask edit")
	}
	if got := tui.GetSettings().MaxAskPrice; math.Abs(got-0.95) > 0.000001 {
		t.Fatalf("expected typed max ask 0.95, got %.2f", got)
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
		tui:            tui,
		showSettings:   true,
		settingsBackup: tui.GetSettings(),
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

func TestStructuralSettingsRestartIsDeferredUntilOverlayClose(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.InitSettings(TUISettings{PaperArbMode: "taker", MaxMarkets: 2}, nil)

	model := tuiModel{
		tui:            tui,
		showSettings:   true,
		settingsCursor: settingsRowMaxMarkets,
		settingsBackup: tui.GetSettings(),
	}

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyRight})
	updated := next.(tuiModel)

	if got := tui.GetSettings().MaxMarkets; got != 3 {
		t.Fatalf("expected max markets to increase to 3, got %d", got)
	}
	if tui.GetAndClearRestart() {
		t.Fatal("expected structural edit to defer restart until settings close")
	}

	next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	updated = next.(tuiModel)
	if updated.showSettings {
		t.Fatal("expected s to close the settings overlay")
	}
	if !tui.GetAndClearRestart() {
		t.Fatal("expected committed structural change to request restart on close")
	}
}

func TestStructuralSettingsEscCancelsPendingRestart(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.InitSettings(TUISettings{PaperArbMode: "taker", MaxMarkets: 2}, nil)

	model := tuiModel{
		tui:            tui,
		showSettings:   true,
		settingsCursor: settingsRowMaxMarkets,
		settingsBackup: tui.GetSettings(),
	}

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyRight})
	updated := next.(tuiModel)
	if tui.GetAndClearRestart() {
		t.Fatal("expected structural edit to stay pending while settings remain open")
	}

	next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyEsc})
	updated = next.(tuiModel)
	if updated.showSettings {
		t.Fatal("expected esc to close the settings overlay")
	}
	if got := tui.GetSettings().MaxMarkets; got != 2 {
		t.Fatalf("expected esc to restore max markets to backup value 2, got %d", got)
	}
	if tui.GetAndClearRestart() {
		t.Fatal("expected esc to avoid requesting restart after reverting structural edits")
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

func TestPauseHotkeyDoesNotInterceptSettingsTextEdit(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	model := tuiModel{
		tui:           tui,
		showSettings:  true,
		settingsEdit:  true,
		settingsInput: "rpc-",
	}

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	updated := next.(tuiModel)
	if tui.IsTradingPaused() {
		t.Fatal("expected p typed during settings edit to avoid toggling pause")
	}
	if updated.settingsInput != "rpc-p" {
		t.Fatalf("expected settings input to keep typed p, got %q", updated.settingsInput)
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
	if !strings.Contains(rendered, "EVENTS  (2/3)") {
		t.Fatalf("expected render to show visible/retained counts, got %q", rendered)
	}
	if !strings.Contains(rendered, "two") || !strings.Contains(rendered, "three") {
		t.Fatalf("expected render to include newest events, got %q", rendered)
	}
	if strings.Contains(rendered, "one") {
		t.Fatalf("expected render to omit trimmed events, got %q", rendered)
	}
}

func TestRenderEventLogTruncatesLongLinesToSingleRow(t *testing.T) {
	model := tuiModel{snap: tuiSnapshot{eventLog: []string{"[10:00:00] this is a very long event log line that should be truncated instead of wrapping across many columns"}}}
	rendered := model.renderEventLog(50, 1)
	if !strings.Contains(rendered, "…") {
		t.Fatalf("expected long event line to be truncated, got %q", rendered)
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

func TestRenderOrderHistoryValueMatchesDisplayedPriceTimesShares(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.RecordOrderWithMode("BTC", "Up", "BUY", 1.02, 0.60, 0.97, 0.0, 0.0, "laddered-taker", "FILLED")

	model := tuiModel{snap: tuiSnapshot{orderHistory: tui.GetOrderHistory()}}
	rendered := model.renderOrderHistory(120, 5)

	if !strings.Contains(rendered, "$0.61") {
		t.Fatalf("expected order history to display value from shown shares*price, got %q", rendered)
	}
	if strings.Contains(rendered, "$0.97") {
		t.Fatalf("expected stale stored cost not to override displayed value, got %q", rendered)
	}
}

func TestRenderOrderHistoryShowsMarketRoundSuffixFromSlug(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.AddMarket("btc-updown", "btc-updown-5m-1775816700", []string{"Up", "Down"}, time.Now().Add(5*time.Minute))
	tui.RecordOrderWithMode("btc-updown", "Up", "BUY", 1.02, 0.60, 0.61, 0.0, 0.0, "laddered-taker", "FILLED")

	model := tuiModel{snap: tuiSnapshot{orderHistory: tui.GetOrderHistory()}}
	rendered := model.renderOrderHistory(140, 5)

	if !strings.Contains(rendered, "1775816700") {
		t.Fatalf("expected order history to include market round suffix from slug, got %q", rendered)
	}
	if strings.Contains(rendered, "btc-updown") {
		t.Fatalf("expected order history market label to collapse to suffix only, got %q", rendered)
	}
}

func TestRenderOrderHistoryBackfillsSlugForOrdersRecordedBeforeAddMarket(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.RecordOrderWithMode("btc-updown", "Up", "BUY", 1.02, 0.60, 0.61, 0.0, 0.0, "laddered-taker", "FILLED")
	tui.AddMarket("btc-updown", "btc-updown-5m-1775817900", []string{"Up", "Down"}, time.Now().Add(5*time.Minute))

	model := tuiModel{snap: tuiSnapshot{orderHistory: tui.GetOrderHistory(), markets: map[string]*MarketData{
		"btc-updown": {Slug: "btc-updown-5m-1775817900"},
	}}}
	rendered := model.renderOrderHistory(140, 5)

	if !strings.Contains(rendered, "1775817900") {
		t.Fatalf("expected order history to backfill market round suffix after AddMarket, got %q", rendered)
	}
	if strings.Contains(rendered, "btc-updown") {
		t.Fatalf("expected order history backfill label to use suffix only, got %q", rendered)
	}
}

func TestRenderPositionsHeaderShowsMarketsAndLegs(t *testing.T) {
	positions := map[string]PositionPnL{
		"m1:Up":   {Position: Position{MarketID: "m1", Outcome: "Up", Quantity: 1}},
		"m1:Down": {Position: Position{MarketID: "m1", Outcome: "Down", Quantity: 1}},
		"m2:Up":   {Position: Position{MarketID: "m2", Outcome: "Up", Quantity: 1}},
		"m2:Down": {Position: Position{MarketID: "m2", Outcome: "Down", Quantity: 1}},
	}
	model := tuiModel{snap: tuiSnapshot{settings: TUISettings{PaperArbMode: "laddered-taker"}}}

	rendered := model.renderPositions(140, positions)

	if !strings.Contains(rendered, "IN-FLIGHT  (2 markets / 4 legs)") {
		t.Fatalf("expected positions header to show market and leg counts, got %q", rendered)
	}
}

func TestRenderRoundHistoryShowsPnlAndWinLoss(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			roundHistory: []RoundHistoryEntry{
				{Number: 1, Timestamp: time.Unix(0, 0), StartingEquity: 100.0, EndingEquity: 104.65, PnL: 4.65, Trades: 3, ShareSummary: "m1: Up 118  |  Down 103"},
				{Number: 2, Timestamp: time.Unix(1, 0), StartingEquity: 104.65, EndingEquity: 101.15, PnL: -3.50, Trades: 2, ShareSummary: "m2: Up 90  |  Down 120"},
				{Number: 3, Timestamp: time.Unix(2, 0), StartingEquity: 101.15, EndingEquity: 101.15, PnL: 0.00, Trades: 0, ShareSummary: "m3: Up 0  |  Down 0"},
			},
		},
	}

	rendered := model.renderRoundHistory(120, 5)
	if !strings.Contains(rendered, "DELTA") || !strings.Contains(rendered, "WIN") || !strings.Contains(rendered, "LOSS") {
		t.Fatalf("expected pnl and result labels in round history, got %q", rendered)
	}
	if !strings.Contains(rendered, "FLAT") {
		t.Fatalf("expected flat round label in round history, got %q", rendered)
	}
	if strings.Contains(rendered, "UP") || strings.Contains(rendered, "DOWN") {
		t.Fatalf("expected round history not to imply market winner direction, got %q", rendered)
	}
	if !strings.Contains(rendered, "+$4.65") || !strings.Contains(rendered, "-$3.50") {
		t.Fatalf("expected signed round pnl values, got %q", rendered)
	}
	if !strings.Contains(rendered, "Up 118") || !strings.Contains(rendered, "Down 103") {
		t.Fatalf("expected per-round share summary, got %q", rendered)
	}
	if !strings.Contains(rendered, "m1: Up 118") || !strings.Contains(rendered, "m2: Up 90") {
		t.Fatalf("expected round history shares to be grouped by market, got %q", rendered)
	}
}

func TestRenderRoundHistoryMarksOpenInventoryAsCarry(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			roundHistory: []RoundHistoryEntry{
				{
					Number:         1,
					Timestamp:      time.Unix(3, 0),
					StartingEquity: 102.94,
					EndingEquity:   97.08,
					PnL:            -5.87,
					Trades:         58,
					ShareSummary:   "Up 26.62@$0.55  |  Down 30.79@$0.59",
					positions: map[string]Position{
						"btc:up":   {MarketID: "btc", Outcome: "Up", Quantity: 26.62, AvgPrice: 0.55},
						"btc:down": {MarketID: "btc", Outcome: "Down", Quantity: 30.79, AvgPrice: 0.59},
					},
				},
			},
		},
	}

	rendered := model.renderRoundHistory(120, 5)
	if !strings.Contains(rendered, "OPEN") {
		t.Fatalf("expected open-inventory carry label in round history, got %q", rendered)
	}
	if strings.Contains(rendered, "LOSS") {
		t.Fatalf("expected carry round not to be classified as loss, got %q", rendered)
	}
	if !strings.Contains(rendered, "W/L/F 0/0/1") {
		t.Fatalf("expected carry round to count toward flat bucket, got %q", rendered)
	}
	if !strings.Contains(rendered, "cash est") || !strings.Contains(rendered, "carry $") {
		t.Fatalf("expected carry round to explain book-equity split, got %q", rendered)
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

func TestRenderRoundHistorySuppressesUnchangedCarryFromLaterRows(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			roundHistory: []RoundHistoryEntry{
				{
					Number:         2,
					Timestamp:      time.Unix(1, 0),
					StartingEquity: 100.0,
					EndingEquity:   99.0,
					PnL:            -1.0,
					Trades:         6,
					positions: map[string]Position{
						"m2:Up": {MarketID: "m2", Outcome: "Up", Quantity: 10.0, TotalCost: 6.60},
					},
				},
				{
					Number:         3,
					Timestamp:      time.Unix(2, 0),
					StartingEquity: 99.0,
					EndingEquity:   98.0,
					PnL:            -1.0,
					Trades:         8,
					positions: map[string]Position{
						"m2:Up":   {MarketID: "m2", Outcome: "Up", Quantity: 10.0, TotalCost: 6.60},
						"m3:Up":   {MarketID: "m3", Outcome: "Up", Quantity: 14.0, TotalCost: 7.98},
						"m3:Down": {MarketID: "m3", Outcome: "Down", Quantity: 10.0, TotalCost: 6.10},
					},
				},
			},
		},
	}

	rendered := model.renderRoundHistory(140, 5)
	if count := strings.Count(rendered, "m2: Up 10"); count != 1 {
		t.Fatalf("expected unchanged carry market to render only on its original round, found %d copies in %q", count, rendered)
	}
	if !strings.Contains(rendered, "m3: Up 14@$0.57") || !strings.Contains(rendered, "Down 10@$0.61") {
		t.Fatalf("expected later round to keep only its new pair across wrapped lines, got %q", rendered)
	}
}

func TestRenderRoundHistoryWrapsSummaryWithinPanelWidth(t *testing.T) {
	const panelWidth = 50

	model := tuiModel{
		snap: tuiSnapshot{
			roundHistory: []RoundHistoryEntry{
				{
					Number:         3,
					Timestamp:      time.Unix(2, 0),
					StartingEquity: 501.41,
					EndingEquity:   505.13,
					PnL:            3.72,
					Trades:         11,
					ShareSummary: strings.Join([]string{
						"1776036300: Up 9.07207@$0.67 ✓  |  Down 2.01672@$0.31 ✗",
						"1776036600: Up 3.01786@$0.42 ✗  |  Down 8.06188@$0.67 ✓",
					}, "\n"),
				},
			},
		},
	}

	rendered := model.renderRoundHistory(panelWidth, 5)

	if !strings.Contains(rendered, "1776036300: Up 9.07207@$0.67") {
		t.Fatalf("expected first market summary to remain visible, got %q", rendered)
	}
	if !strings.Contains(rendered, "Down 2.01672@$0.31") {
		t.Fatalf("expected first market second outcome to wrap onto its own line, got %q", rendered)
	}
	if !strings.Contains(rendered, "1776036600: Up 3.01786@$0.42") {
		t.Fatalf("expected second market summary to remain on its own line, got %q", rendered)
	}
	if !strings.Contains(rendered, "Down 8.06188@$0.67") {
		t.Fatalf("expected second market second outcome to wrap onto its own line, got %q", rendered)
	}

	for _, line := range strings.Split(rendered, "\n") {
		if width := ansi.StringWidth(line); width > panelWidth {
			t.Fatalf("expected rendered round-history line width <= %d, got %d for %q", panelWidth, width, line)
		}
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

func TestRenderSettingsShowsPaperBalanceRow(t *testing.T) {
	tui := NewTUI(NewEngine(100.0), NewOrderBook())
	tui.InitSettings(TUISettings{
		PaperBalance:     250.25,
		MarketSlug:       "ALL",
		MaxMarkets:       4,
		Timeframe:        "15m",
		TradeScaleFactor: 0.05,
	}, nil)
	tui.SetMode("Paper")

	model := tuiModel{tui: tui}
	rendered := model.renderSettings(120)
	if !strings.Contains(rendered, "Paper Balance") {
		t.Fatalf("expected paper balance row in settings, got %q", rendered)
	}
	if !strings.Contains(rendered, "$250.25") {
		t.Fatalf("expected configured paper balance in settings, got %q", rendered)
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
	}, 0, 0, 120, 120, 1.0, 100, 0, 0, 0, nil)

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
	}, 6, 0, 97, 100, 1.0, 100, 0, 0, 0, map[string]Position{
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
	}, 0, 0, 100, 100, 1.3, 100, 0, 0, 0, nil)

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
	}, 0, 0, 120, 120, 1.0, 100, 0, 0, 0, nil)

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
	}, 0.0, 0, 66.35, 66.35, 1.0, 72.01, 2, 2, 0, nil)

	if !strings.Contains(rendered, "Realized +$1.08") {
		t.Fatalf("expected real-mode account status to show realized pnl explicitly, got %q", rendered)
	}
	if !strings.Contains(rendered, "Equity $66.35") {
		t.Fatalf("expected real-mode account status to show equity, got %q", rendered)
	}
	if !strings.Contains(rendered, "($3.60/trade)") {
		t.Fatalf("expected 5%% trade budget to keep the real-mode high-water floor, got %q", rendered)
	}
}

func TestRenderAccountStatusRealbotPaperBackendUsesMarkToMarketEquityChange(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			mode:        "Real",
			tradeFactor: 0.05,
			settings: TUISettings{
				ExecutionBackend: core.ExecutionBackendPaper,
			},
		},
	}

	rendered := model.renderAccountStatus(120, Stats{
		CurrentBalance:  55.57,
		StartingBalance: 97.77,
		RealizedPnL:     4.09,
	}, 48.52, 109.63, 101.86, 101.86, 1.0, 97.77, 2, 1, 1, map[string]Position{
		"btc-updown-5m-1:Up":   {MarketID: "btc-updown-5m-1", Outcome: "Up", Quantity: 53.04, AvgPrice: 0.48, TotalCost: 25.46},
		"btc-updown-5m-1:Down": {MarketID: "btc-updown-5m-1", Outcome: "Down", Quantity: 53.04, AvgPrice: 0.58, TotalCost: 30.76},
	})

	if !strings.Contains(rendered, "Equity $101.86") {
		t.Fatalf("expected paper-backend realbot account status to keep mtm equity, got %q", rendered)
	}
	if !strings.Contains(rendered, "(+$4.09)") {
		t.Fatalf("expected net change to follow mtm equity on paper backend, got %q", rendered)
	}
	if !strings.Contains(rendered, "Realized +$4.09") {
		t.Fatalf("expected realized pnl line to remain explicit on paper backend, got %q", rendered)
	}
}

func TestRenderAccountStatusRealModeShowsWalletCashSeparatelyFromSpendableBalance(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			mode:          "Real",
			tradeFactor:   0.05,
			walletCash:    18.00,
			hasWalletCash: true,
		},
	}

	rendered := model.renderAccountStatus(120, Stats{
		CurrentBalance:  9.93,
		StartingBalance: 18.00,
		RealizedPnL:     -4.08,
	}, 0.0, 0, 9.93, 9.93, 1.0, 18.00, 3, 1, 1, nil)

	if !strings.Contains(rendered, "Spendable $18.00") {
		t.Fatalf("expected spendable to show wallet cash in real-mode account status, got %q", rendered)
	}
	if !strings.Contains(rendered, "Equity ") || !strings.Contains(rendered, "$9.93") {
		t.Fatalf("expected real-mode equity labels to reflect local book equity, got %q", rendered)
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
	}, 0.0, 0, 61.53, 61.53, 1.0, 203.20, 4, 4, 0, nil)

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
	}, 0, 0, 96.9, 96.9, 1.0, 100, 1, 0, 1, nil)

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
	}, 0, 0, 100, 100, 1.0, 100, 0, 0, 0, nil)

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
	}, 0, 0, 100, 100, 1.0, 120, 0, 0, 0, nil)

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
	}, 0, 0, 100, 100, 1.2, 120, 8, 3, 2, nil)

	if !strings.Contains(rendered, "Win 60%") {
		t.Fatalf("expected round win rate fallback in account status, got %q", rendered)
	}
	if !strings.Contains(rendered, "W/L/F 3/2/3") {
		t.Fatalf("expected round win/loss/flat fallback in account status, got %q", rendered)
	}
	if strings.Contains(rendered, "profitable") {
		t.Fatalf("expected profitable-round text to be removed, got %q", rendered)
	}
	if !strings.Contains(rendered, "$6.00/trade") {
		t.Fatalf("expected real trade budget to follow high-water sizing, got %q", rendered)
	}
}

func TestRenderAccountStatusUsesRoundHistorySummaryWhenAvailable(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			mode:        "Real",
			tradeFactor: 0.05,
			orderHistory: []OrderHistoryEntry{
				{MarketID: "BTC#m1", Outcome: "Up", Side: "SELL", Profit: 0.7, Status: "FILLED"},
				{MarketID: "ETH#m2", Outcome: "Down", Side: "SELL", Profit: -0.2, Status: "FILLED"},
			},
			roundHistory: []RoundHistoryEntry{
				{Number: 1, Timestamp: time.Unix(1, 0), PnL: 2.0},
				{Number: 2, Timestamp: time.Unix(2, 0), PnL: -1.0},
				{Number: 3, Timestamp: time.Unix(3, 0), PnL: 0.0, positions: map[string]Position{"m1:Up": {MarketID: "m1", Outcome: "Up", Quantity: 1}}},
			},
		},
	}

	rendered := model.renderAccountStatus(120, Stats{
		CurrentBalance:  100,
		StartingBalance: 100,
		WinningTrades:   9,
		LosingTrades:    1,
	}, 0, 0, 100, 100, 1.0, 120, 3, 1, 1, nil)

	if !strings.Contains(rendered, "W/L/F 1/1/1") {
		t.Fatalf("expected account status to match round history outcomes when round history is available, got %q", rendered)
	}
	if strings.Contains(rendered, "W/L 9/1") {
		t.Fatalf("expected round summary to override trade-history win/loss display, got %q", rendered)
	}
}

func TestRenderAccountStatusUsesRoundHistoryWhenEngineRoundCountLags(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			mode:        "Real",
			tradeFactor: 0.05,
			roundHistory: []RoundHistoryEntry{
				{Number: 1, Timestamp: time.Unix(1, 0), PnL: 0},
				{Number: 2, Timestamp: time.Unix(2, 0), PnL: -3.20},
			},
		},
	}

	rendered := model.renderAccountStatus(120, Stats{
		CurrentBalance:  20.73,
		StartingBalance: 20.86,
		WinningTrades:   9,
		LosingTrades:    1,
		RealizedPnL:     -3.20,
	}, 0, 0, 20.73, 20.73, 1.0, 20.86, 1, 1, 0, nil)

	if !strings.Contains(rendered, "2 rounds") {
		t.Fatalf("expected account status to show round-history count, got %q", rendered)
	}
	if !strings.Contains(rendered, "W/L/F 0/1/1") {
		t.Fatalf("expected account status to match amended round history outcomes, got %q", rendered)
	}
	if strings.Contains(rendered, "W/L/F 1/0/0") {
		t.Fatalf("expected stale engine round counters to be ignored, got %q", rendered)
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
	}, 3.1, 0, 100, 100, 1.0, 100, 0, 0, 0, map[string]Position{
		"m1:Up": {MarketID: "m1", Outcome: "Up", Quantity: 3.5, AvgPrice: 3.1 / 3.5, TotalCost: 3.1},
	})

	if !strings.Contains(rendered, "Resolve +$0.40/-$3.10") {
		t.Fatalf("expected account status to show resolution estimate, got %q", rendered)
	}
}

func TestRenderAccountStatusUsesMatchedLabelInLadderedMode(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			mode: "Real",
			settings: TUISettings{
				PaperArbMode:                "laddered-taker",
				LadderedTakerSizingMode:     core.LadderedTakerSizingModeUSDC,
				LadderedTakerSizeUSDC:       1.0,
				LadderedTakerMaxSlippagePct: 5.0,
			},
		},
	}

	rendered := model.renderAccountStatus(120, Stats{
		CurrentBalance:  65.0,
		StartingBalance: 100.0,
	}, 35.0, 0, 92.25, 92.25, 1.0, 100.0, 0, 0, 0, map[string]Position{
		"m1:Down": {MarketID: "m1", Outcome: "Down", Quantity: 27.1186, AvgPrice: 0.74, TotalCost: 20.067764},
		"m1:Up":   {MarketID: "m1", Outcome: "Up", Quantity: 40.5598, AvgPrice: 0.37, TotalCost: 15.007126},
	})

	if !strings.Contains(rendered, "Matched ") {
		t.Fatalf("expected laddered account status to label matched-pair pnl clearly, got %q", rendered)
	}
	if !strings.Contains(rendered, "Equity ") {
		t.Fatalf("expected real account status to show equity labels, got %q", rendered)
	}
}

func TestRenderAccountStatusShowsLadderUSDCCapWhenConfigured(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			mode: "Real",
			settings: TUISettings{
				PaperArbMode:                "laddered-taker",
				LadderedTakerSizingMode:     core.LadderedTakerSizingModeUSDC,
				LadderedTakerSizeUSDC:       2.7,
				LadderedTakerSizeShares:     9.5,
				LadderedTakerMaxSlippagePct: 5.0,
			},
		},
	}

	rendered := model.renderAccountStatus(120, Stats{
		CurrentBalance:  65.0,
		StartingBalance: 100.0,
	}, 35.0, 0, 92.25, 92.25, 1.0, 100.0, 0, 0, 0, map[string]Position{
		"m1:Down": {MarketID: "m1", Outcome: "Down", Quantity: 27.1186, AvgPrice: 0.74, TotalCost: 20.067764},
		"m1:Up":   {MarketID: "m1", Outcome: "Up", Quantity: 40.5598, AvgPrice: 0.37, TotalCost: 15.007126},
	})

	if !strings.Contains(rendered, "Ladder $2.70 cap") {
		t.Fatalf("expected ladder usdc account status to show usdc cap, got %q", rendered)
	}
	if strings.Contains(rendered, "9.5 shares") {
		t.Fatalf("expected ladder usdc account status to avoid share sizing text, got %q", rendered)
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
	}, 6.08, 0, 99.70, 99.70, 1.0, 100.0, 0, 0, 0, map[string]Position{
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
	}, 0, 0, 100, 100, 1.0, 100.0, 0, 0, 0, nil)

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

func TestRecordWalletSyncAdjustmentAddsSyncHistoryEntry(t *testing.T) {
	t.Skip("Silenced ADJ logs per user request")
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.RecordWalletSyncAdjustment("BTC", "Down", 3.001719, 0.28, "ADJ+")

	history := tui.GetOrderHistory()
	if len(history) != 1 {
		t.Fatalf("expected 1 sync history entry, got %d", len(history))
	}
	if history[0].ExecutionMode != "wallet-sync" {
		t.Fatalf("expected wallet-sync execution mode, got %q", history[0].ExecutionMode)
	}
	if history[0].Status != "SYNCED" {
		t.Fatalf("expected SYNCED status, got %q", history[0].Status)
	}
	if history[0].Side != "ADJ+" {
		t.Fatalf("expected ADJ+ side, got %q", history[0].Side)
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
	if !strings.Contains(rendered, "[P] pause") {
		t.Fatalf("expected footer pause hotkey, got %q", rendered)
	}
	if !strings.Contains(rendered, "[↑↓/jk] scroll") {
		t.Fatalf("expected footer controls, got %q", rendered)
	}
}

func TestRenderFooterShowsPausedStatus(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	model := tuiModel{
		tui:  tui,
		snap: tuiSnapshot{mode: "Real", manualTradingPause: true},
	}
	rendered := model.renderFooter(140, 0, 0)
	if !strings.Contains(rendered, "PAUSED") {
		t.Fatalf("expected footer pause badge, got %q", rendered)
	}
	if !strings.Contains(rendered, "[P] resume") {
		t.Fatalf("expected footer resume hotkey, got %q", rendered)
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

func TestNormalizeTUISettingsNormalizesTimeframe(t *testing.T) {
	got := normalizeTUISettings(TUISettings{Timeframe: "1h"})
	if got.Timeframe != "1h" {
		t.Fatalf("expected 1h timeframe to be preserved, got %q", got.Timeframe)
	}

	got = normalizeTUISettings(TUISettings{Timeframe: "invalid"})
	if got.Timeframe != "15m" {
		t.Fatalf("expected invalid timeframe to fall back to 15m, got %q", got.Timeframe)
	}
}

func TestNormalizeTUISettingsClampsLadderedTakerReentryMove(t *testing.T) {
	got := normalizeTUISettings(TUISettings{LadderedTakerReentryMoveCents: 0})
	if got.LadderedTakerReentryMoveCents != 1.0 {
		t.Fatalf("expected default ladder reentry move 1.0c, got %.1f", got.LadderedTakerReentryMoveCents)
	}
	got = normalizeTUISettings(TUISettings{LadderedTakerReentryMoveCents: 0.01})
	if got.LadderedTakerReentryMoveCents != 1.0 {
		t.Fatalf("expected ladder reentry move to clamp to 1.0c, got %.1f", got.LadderedTakerReentryMoveCents)
	}
	got = normalizeTUISettings(TUISettings{LadderedTakerReentryMoveCents: 70})
	if got.LadderedTakerReentryMoveCents != 25.0 {
		t.Fatalf("expected ladder reentry move to clamp to 25.0c, got %.1f", got.LadderedTakerReentryMoveCents)
	}
}

func TestNormalizeTUISettingsClampsLadderedTakerMaxSlippagePct(t *testing.T) {
	got := normalizeTUISettings(TUISettings{LadderedTakerMaxSlippagePct: 0})
	if got.LadderedTakerMaxSlippagePct != 0 {
		t.Fatalf("expected ladder max slip to clamp to 0, got %.1f", got.LadderedTakerMaxSlippagePct)
	}
	got = normalizeTUISettings(TUISettings{LadderedTakerMaxSlippagePct: -5})
	if got.LadderedTakerMaxSlippagePct != 0 {
		t.Fatalf("expected negative ladder max slip to clamp to 0, got %.1f", got.LadderedTakerMaxSlippagePct)
	}
	got = normalizeTUISettings(TUISettings{LadderedTakerMaxSlippagePct: 150})
	if got.LadderedTakerMaxSlippagePct != 99.0 {
		t.Fatalf("expected ladder max slip to clamp to 99.0, got %.1f", got.LadderedTakerMaxSlippagePct)
	}
}

func TestNormalizeTUISettingsDefaultsRedeemEntryTimingToNextMarket(t *testing.T) {
	got := normalizeTUISettings(TUISettings{})
	if got.RedeemEntryTiming != core.RedeemEntryTimingNextMarket {
		t.Fatalf("expected default redeem entry timing %q, got %q", core.RedeemEntryTimingNextMarket, got.RedeemEntryTiming)
	}

	got = normalizeTUISettings(TUISettings{RedeemEntryTiming: "immediate"})
	if got.RedeemEntryTiming != core.RedeemEntryTimingImmediate {
		t.Fatalf("expected redeem entry timing %q, got %q", core.RedeemEntryTimingImmediate, got.RedeemEntryTiming)
	}
}

func TestNormalizeTUISettingsDefaultsRedeemGasModeToFast(t *testing.T) {
	got := normalizeTUISettings(TUISettings{})
	if got.RedeemGasMode != core.RedeemGasModeFast {
		t.Fatalf("expected default redeem gas mode %q, got %q", core.RedeemGasModeFast, got.RedeemGasMode)
	}

	got = normalizeTUISettings(TUISettings{RedeemGasMode: "urgent"})
	if got.RedeemGasMode != core.RedeemGasModeUrgent {
		t.Fatalf("expected redeem gas mode %q, got %q", core.RedeemGasModeUrgent, got.RedeemGasMode)
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
	}, 0, 0, 100, 100, 1.0, 100, 0, 0, 0, nil)

	if !strings.Contains(rendered, "US time") {
		t.Fatalf("expected account status to include US clock, got %q", rendered)
	}
	if !strings.Contains(rendered, "Weekday Gate") {
		t.Fatalf("expected account status to include weekday gate status, got %q", rendered)
	}
}

func TestRenderAccountStatusShowsCurrentAndMaxExposureAndDollarDrawdown(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			mode:        "Paper",
			tradeFactor: 0.05,
		},
	}

	rendered := model.renderAccountStatus(120, Stats{
		CurrentBalance:  95.0,
		StartingBalance: 100.0,
		PeakExposure:    25.0,
		MaxDrawdown:     5.0,
		MaxDrawdownCash: 5.0,
	}, 12.5, 25.0, 100.0, 100.0, 1.0, 100.0, 0, 0, 0, nil)

	if !strings.Contains(rendered, "Exposure $12.50") {
		t.Fatalf("expected account status to show current exposure, got %q", rendered)
	}
	if !strings.Contains(rendered, "Max Exp $25.00") {
		t.Fatalf("expected account status to show max exposure as a UI stat, got %q", rendered)
	}
	if !strings.Contains(rendered, "Max DD -$5.00") {
		t.Fatalf("expected account status to show max dollar drawdown explicitly, got %q", rendered)
	}
}
