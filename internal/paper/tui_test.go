package paper

import (
	"testing"

	"Market-bot/internal/core"

	tea "github.com/charmbracelet/bubbletea"
)

func TestExecutionBackendChangeRequestsImmediateRestart(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.SetMode("Real")
	tui.InitSettings(TUISettings{
		ExecutionBackend: core.ExecutionBackendPaper,
		PaperArbMode:     "taker",
	}, nil)

	model := tuiModel{
		tui:            tui,
		showSettings:   true,
		settingsCursor: settingsRowExecutionBackend,
		settingsBackup: tui.GetSettings(),
	}

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyRight})
	updated := next.(tuiModel)

	if got := updated.tui.GetSettings().ExecutionBackend; got != core.ExecutionBackendLive {
		t.Fatalf("expected execution backend to change to live, got %q", got)
	}
	if !tui.GetAndClearRestart() {
		t.Fatal("expected execution backend change to request immediate restart")
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

func TestFormatMarketWithSuffix(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"btc-updown-15m-1776820500", "BTC-15M 1776820500"},
		{"ETH-5M#98024767", "ETH-5M 98024767"},
		{"eth-5m#98024767", "ETH-5M 98024767"},
		{"BTC", "BTC"},
		{"1779324000", "1779324000"},
		{"btc-updown-5m-1", "BTC-5M 1"},
		{"", "UNKNOWN"},
		{"  ", "UNKNOWN"},
	}

	for _, tt := range tests {
		got := formatMarketWithSuffix(tt.input)
		if got != tt.expected {
			t.Errorf("formatMarketWithSuffix(%q) = %q, expected %q", tt.input, got, tt.expected)
		}
	}
}

func TestOrderHistoryMarketLabel(t *testing.T) {
	tests := []struct {
		marketID   string
		marketSlug string
		expected   string
	}{
		{"BTC", "btc-updown-15m-1776820500", "BTC-15M 1776820500"},
		{"btc-updown-5m-1", "", "BTC-5M 1"},
		{"BTC", "", "BTC"},
	}

	for _, tt := range tests {
		got := orderHistoryMarketLabel(tt.marketID, tt.marketSlug)
		if got != tt.expected {
			t.Errorf("orderHistoryMarketLabel(%q, %q) = %q, expected %q", tt.marketID, tt.marketSlug, got, tt.expected)
		}
	}
}
