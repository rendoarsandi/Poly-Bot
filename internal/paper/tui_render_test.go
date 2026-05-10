package paper

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"Market-bot/internal/core"

	"github.com/charmbracelet/x/ansi"
)

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

func TestDisplayedTradeBudgetsUsesEquityInsteadOfCashOnlyInRealMode(t *testing.T) {
	base, effective := displayedTradeBudgets("Real", 90, 100, 100, 90, 0.10, 0, 1.50)
	if base != 10 {
		t.Fatalf("expected real trade budget to use current equity, not cash-only sizing, got %.2f", base)
	}
	if effective != 10 {
		t.Fatalf("expected real effective budget to match base in real mode, got %.2f", effective)
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

func TestRenderSettingsShowsTradingHoursEditInput(t *testing.T) {
	tui := NewTUI(NewEngine(100), nil)
	tui.InitSettings(TUISettings{TradingHoursMode: "08:00-17:00"}, nil)
	model := tuiModel{
		tui:            tui,
		showSettings:   true,
		settingsCursor: settingsRowTradingHoursMode,
		settingsEdit:   true,
		settingsInput:  "09:00-15:00",
	}

	rendered := model.renderSettings(120)
	if !strings.Contains(rendered, "Trading Hours (WIB)") {
		t.Fatalf("expected settings view to include Jakarta trading-hours row, got %q", rendered)
	}
	if !strings.Contains(rendered, "09:00-15:00") {
		t.Fatalf("expected settings view to show active trading-hours input, got %q", rendered)
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
		LadderedTakerPnLGuardMode:      core.LadderedTakerPnLGuardWorst,
		LadderedTakerWorstPnLFloor:     -2.5,
		MinMarginPercent:               2.0,
		TakerCloseMarket:               true,
		BuyExecutionMarginFloorPercent: -0.02,
	}, nil)

	view := (tuiModel{tui: tui}).renderSettings(120)
	for _, want := range []string{"Ladder Re-entry Move", "1.8c", "Ladder PnL Guard", "worst-pnl", "Ladder Worst PnL Floor", "-$2.50"} {
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

func TestRenderEventLogWrapsLongLines(t *testing.T) {
	model := tuiModel{snap: tuiSnapshot{eventLog: []string{"[10:00:00] this is a very long event log line that should be truncated instead of wrapping across many columns"}}}
	rendered := model.renderEventLog(50, 1)
	if strings.Contains(rendered, "…") {
		t.Fatalf("expected long event line to wrap instead of truncating, got %q", rendered)
	}
	if !strings.Contains(rendered, "very long event log") || !strings.Contains(rendered, "wrapping across many columns") {
		t.Fatalf("expected wrapped event line content to remain visible, got %q", rendered)
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

func TestRenderOrderHistoryValueUsesRecordedNotional(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.RecordOrderWithMode("BTC", "Up", "BUY", 1.02, 0.60, 0.97, 0.0, 0.0, "laddered-taker", "FILLED")

	model := tuiModel{snap: tuiSnapshot{orderHistory: tui.GetOrderHistory()}}
	rendered := model.renderOrderHistory(120, 5)

	if !strings.Contains(rendered, "$0.97") {
		t.Fatalf("expected order history to display recorded notional, got %q", rendered)
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

	if !strings.Contains(rendered, "pUSD $18.00") {
		t.Fatalf("expected pUSD label to show wallet cash in real-mode account status, got %q", rendered)
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

func TestRenderAccountStatusLiveModeShowsBookEquityNetChange(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			mode:        "Real",
			tradeFactor: 0.05,
		},
	}

	rendered := model.renderAccountStatus(120, Stats{
		CurrentBalance:  96.75,
		StartingBalance: 100,
		RealizedPnL:     0,
	}, 0, 0, 96.75, 96.75, 1.0, 100, 0, 0, 0, nil)

	if !strings.Contains(rendered, "Equity") || !strings.Contains(rendered, "-$3.25") {
		t.Fatalf("expected live account equity to show tx-derived net loss, got %q", rendered)
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

func TestRenderAccountStatusShowsJakartaWeekdayGateStatus(t *testing.T) {
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

	if !strings.Contains(rendered, "Jakarta time") {
		t.Fatalf("expected account status to include Jakarta clock, got %q", rendered)
	}
	if !strings.Contains(rendered, "Weekday Gate") {
		t.Fatalf("expected account status to include weekday gate status, got %q", rendered)
	}
}

func TestRenderAccountStatusShowsJakartaCustomGateStatus(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			mode:        "Paper",
			tradeFactor: 0.05,
			settings: TUISettings{
				TradingHoursMode: "08:00-17:00",
			},
		},
	}

	rendered := model.renderAccountStatus(120, Stats{
		CurrentBalance:  100,
		StartingBalance: 100,
	}, 0, 0, 100, 100, 1.0, 100, 0, 0, 0, nil)

	if !strings.Contains(rendered, "Jakarta time") {
		t.Fatalf("expected account status to include Jakarta clock for custom gate, got %q", rendered)
	}
	if !strings.Contains(rendered, "Jakarta Gate") {
		t.Fatalf("expected account status to include Jakarta gate status, got %q", rendered)
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
	if !strings.Contains(rendered, "Loss") || !strings.Contains(rendered, "Streak $0.00") {
		t.Fatalf("expected account status to label realized loss streak explicitly, got %q", rendered)
	}
}
