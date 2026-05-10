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
