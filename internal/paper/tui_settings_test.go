package paper

import (
	"math"
	"strings"
	"testing"
	"time"

	"Market-bot/internal/core"

	tea "github.com/charmbracelet/bubbletea"
)

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

func TestNormalizeTUISettingsRoundsFixedTradeSizeUSDC(t *testing.T) {
	cfg := normalizeTUISettings(TUISettings{
		TradeSizingMode: core.TradeSizingModeUSDC,
		TradeSizeUSDC:   2.34,
	})
	if cfg.TradeSizingMode != core.TradeSizingModeUSDC {
		t.Fatalf("expected trade sizing mode usdc, got %q", cfg.TradeSizingMode)
	}
	if cfg.TradeSizeUSDC != 2.34 {
		t.Fatalf("expected trade size to keep cent precision, got %.2f", cfg.TradeSizeUSDC)
	}
}

func TestNormalizeTUISettingsClampsFixedTradeSizeUSDCToOneDollarMinimum(t *testing.T) {
	cfg := normalizeTUISettings(TUISettings{
		TradeSizingMode: core.TradeSizingModeUSDC,
		TradeSizeUSDC:   0.01,
	})
	if cfg.TradeSizeUSDC != 1.0 {
		t.Fatalf("expected trade size minimum 1.0, got %.1f", cfg.TradeSizeUSDC)
	}
}

func TestSettingsRowEditableDisablesSplitAndTakerOnlyRowsInMakerMode(t *testing.T) {
	cfg := TUISettings{PaperArbMode: "maker"}
	for _, idx := range []int{settingsRowSplitMinMargin, settingsRowSplitStrategy, settingsRowSplitInitialCap, settingsRowSplitReplenishCap, settingsRowTakerCloseMarket} {
		if settingsRowEditable(cfg, "Real", idx) {
			t.Fatalf("expected row %d to be disabled/invisible in maker mode", idx)
		}
	}
	for _, idx := range []int{settingsRowMinMargin, settingsRowMinAskPrice, settingsRowMaxAskPrice} {
		if !settingsRowEditable(cfg, "Paper", idx) {
			t.Fatalf("expected row %d to remain editable in maker mode", idx)
		}
	}
}

func TestSettingsArbModesShowMakerForRealbotPaperBackend(t *testing.T) {
	modes := settingsArbModes(TUISettings{ExecutionBackend: core.ExecutionBackendPaper}, "Real")
	found := false
	for _, mode := range modes {
		if mode == "maker" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected realbot paper backend arb modes to show maker, got %v", modes)
	}
}

func TestApplySettingsEditValueNormalizesTradingHoursMode(t *testing.T) {
	cfg := TUISettings{TradingHoursMode: core.TradingHoursModeOff}
	if !applySettingsEditValue(&cfg, settingsRowTradingHoursMode, "wib 8.00-17.30") {
		t.Fatal("expected Jakarta trading-hours edit to apply")
	}
	if cfg.TradingHoursMode != "08:00-17:30" {
		t.Fatalf("expected normalized Jakarta window, got %q", cfg.TradingHoursMode)
	}

	if applySettingsEditValue(&cfg, settingsRowTradingHoursMode, "bad-window") {
		t.Fatal("expected invalid Jakarta trading-hours edit to be rejected")
	}
	if cfg.TradingHoursMode != "08:00-17:30" {
		t.Fatalf("expected invalid edit to preserve prior value, got %q", cfg.TradingHoursMode)
	}
}

func TestSetModeDoesNotCoerceMakerForRealbotPaperBackend(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.InitSettings(TUISettings{
		ExecutionBackend: core.ExecutionBackendPaper,
		PaperArbMode:     "maker",
	}, nil)

	tui.SetMode("Real")

	if got := tui.GetSettings().PaperArbMode; got != "maker" {
		t.Fatalf("expected realbot paper backend to keep maker mode, got %q", got)
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

func TestSettingsTradeSizingValueIgnoresLettersDuringNumericEdit(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.InitSettings(TUISettings{
		PaperArbMode:            "laddered-taker",
		LadderedTakerSizingMode: core.LadderedTakerSizingModeUSDC,
		LadderedTakerSizeUSDC:   1.0,
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

	for _, r := range []rune{'a', '.', '7', 'b'} {
		next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		updated = next.(tuiModel)
	}

	if updated.settingsInput != "2.7" {
		t.Fatalf("expected numeric edit buffer to ignore letters, got %q", updated.settingsInput)
	}
}

func TestSettingsTradeSizingValueClampsTypedUSDCMinimumToOneDollar(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.InitSettings(TUISettings{
		PaperArbMode:    "taker",
		TradeSizingMode: core.TradeSizingModeUSDC,
		TradeSizeUSDC:   2.0,
	}, nil)

	model := tuiModel{
		tui:            tui,
		showSettings:   true,
		settingsCursor: settingsRowTradeSizingValue,
	}

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'0'}})
	updated := next.(tuiModel)
	if !updated.settingsEdit {
		t.Fatal("expected typing on the fixed usdc size row to enter edit mode")
	}

	for _, r := range []rune{'.', '0', '1'} {
		next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		updated = next.(tuiModel)
	}

	next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyEnter})
	updated = next.(tuiModel)
	if updated.settingsEdit {
		t.Fatal("expected enter to commit the typed usdc size edit")
	}
	if got := tui.GetSettings().TradeSizeUSDC; got != 1.0 {
		t.Fatalf("expected typed fixed usdc size to clamp to 1.0, got %.2f", got)
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

func TestSettingsLadderWorstPnLFloorSupportsArrowAndTypedEdit(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.InitSettings(TUISettings{
		PaperArbMode:               "laddered-taker",
		LadderedTakerPnLGuardMode:  core.LadderedTakerPnLGuardWorst,
		LadderedTakerWorstPnLFloor: 0,
	}, nil)

	model := tuiModel{
		tui:            tui,
		showSettings:   true,
		settingsCursor: settingsRowLadderWorstPnLFloor,
	}

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyLeft})
	updated := next.(tuiModel)
	if got := tui.GetSettings().LadderedTakerWorstPnLFloor; got != -0.25 {
		t.Fatalf("expected left arrow to reduce ladder worst PnL floor to -0.25, got %.2f", got)
	}

	next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyRight})
	updated = next.(tuiModel)
	if got := tui.GetSettings().LadderedTakerWorstPnLFloor; got != 0 {
		t.Fatalf("expected right arrow to restore ladder worst PnL floor to auto 0.00, got %.2f", got)
	}

	next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyEnter})
	updated = next.(tuiModel)
	if !updated.settingsEdit {
		t.Fatal("expected enter on ladder worst PnL floor row to start edit mode")
	}

	updated.settingsInput = ""
	for _, r := range []rune{'-', '1', '.', '5'} {
		next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		updated = next.(tuiModel)
	}
	next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyEnter})
	updated = next.(tuiModel)
	if updated.settingsEdit {
		t.Fatal("expected enter to commit the typed ladder worst PnL floor edit")
	}
	if got := tui.GetSettings().LadderedTakerWorstPnLFloor; got != -1.5 {
		t.Fatalf("expected typed ladder worst PnL floor -1.50, got %.2f", got)
	}
}

func TestSettingsLadderMaxProfitPnLSupportsArrowAndTypedEdit(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.InitSettings(TUISettings{
		PaperArbMode:              "laddered-taker",
		LadderedTakerPnLGuardMode: core.LadderedTakerPnLGuardMaxProfit,
		LadderedTakerMaxProfitPnL: 0,
	}, nil)

	model := tuiModel{
		tui:            tui,
		showSettings:   true,
		settingsCursor: settingsRowLadderWorstPnLFloor,
	}

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyLeft})
	updated := next.(tuiModel)
	if got := tui.GetSettings().LadderedTakerMaxProfitPnL; got != 0 {
		t.Fatalf("expected left arrow to clamp ladder min profit pnl at auto 0.00, got %.2f", got)
	}

	next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyRight})
	updated = next.(tuiModel)
	if got := tui.GetSettings().LadderedTakerMaxProfitPnL; got != 0.25 {
		t.Fatalf("expected right arrow to increase ladder min profit pnl to 0.25, got %.2f", got)
	}

	next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyEnter})
	updated = next.(tuiModel)
	if !updated.settingsEdit {
		t.Fatal("expected enter on ladder min profit pnl row to start edit mode")
	}

	updated.settingsInput = ""
	for _, r := range []rune{'0'} {
		next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		updated = next.(tuiModel)
	}
	next, _ = updated.Update(tea.KeyMsg{Type: tea.KeyEnter})
	updated = next.(tuiModel)
	if updated.settingsEdit {
		t.Fatal("expected enter to commit the typed ladder min profit pnl edit")
	}
	if got := tui.GetSettings().LadderedTakerMaxProfitPnL; got != 0 {
		t.Fatalf("expected typed ladder min profit pnl 0 to become auto 0.00, got %.2f", got)
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

func TestPauseHotkeyDoesNotInterceptSettingsTextEdit(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	model := tuiModel{
		tui:            tui,
		showSettings:   true,
		settingsCursor: settingsRowRPCEdit,
		settingsEdit:   true,
		settingsInput:  "rpc-",
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
	for _, tc := range []struct {
		input string
		want  string
	}{
		{"1h", "1h"},
		{"4h", "4h"},
		{"1d", "1d"},
		{"1D", "1d"},
		{"invalid", "15m"},
	} {
		got := normalizeTUISettings(TUISettings{Timeframe: tc.input})
		if got.Timeframe != tc.want {
			t.Errorf("normalizeTUISettings(Timeframe: %q) = %q, want %q", tc.input, got.Timeframe, tc.want)
		}
	}
}

func TestCycleMarketTimeframe(t *testing.T) {
	tests := []struct {
		current string
		delta   int
		want    string
	}{
		{"15m", 1, "5m"},
		{"5m", 1, "1h"},
		{"1h", 1, "4h"},
		{"4h", 1, "1d"},
		{"1d", 1, "15m"},
		{"15m", -1, "1d"},
	}
	for _, tc := range tests {
		got := cycleMarketTimeframe(tc.current, tc.delta)
		if got != tc.want {
			t.Errorf("cycleMarketTimeframe(%q, %d) = %q, want %q", tc.current, tc.delta, got, tc.want)
		}
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

func TestNormalizeTUISettingsClampsLadderedTakerWorstPnLFloor(t *testing.T) {
	got := normalizeTUISettings(TUISettings{LadderedTakerWorstPnLFloor: math.NaN()})
	if got.LadderedTakerWorstPnLFloor != 0 {
		t.Fatalf("expected NaN ladder worst PnL floor to normalize to 0, got %.2f", got.LadderedTakerWorstPnLFloor)
	}
	got = normalizeTUISettings(TUISettings{LadderedTakerWorstPnLFloor: -1005})
	if got.LadderedTakerWorstPnLFloor != -1000.0 {
		t.Fatalf("expected ladder worst PnL floor to clamp to -1000.0, got %.2f", got.LadderedTakerWorstPnLFloor)
	}
	got = normalizeTUISettings(TUISettings{LadderedTakerWorstPnLFloor: 1005})
	if got.LadderedTakerWorstPnLFloor != -1000.0 {
		t.Fatalf("expected positive ladder worst PnL floor to be inverted and clamped to -1000.0, got %.2f", got.LadderedTakerWorstPnLFloor)
	}
}

func TestNormalizeTUISettingsClampsLadderedTakerMaxProfitPnL(t *testing.T) {
	got := normalizeTUISettings(TUISettings{LadderedTakerMaxProfitPnL: math.NaN()})
	if got.LadderedTakerMaxProfitPnL != 0 {
		t.Fatalf("expected NaN ladder min profit pnl to normalize to 0, got %.2f", got.LadderedTakerMaxProfitPnL)
	}
	got = normalizeTUISettings(TUISettings{LadderedTakerMaxProfitPnL: -5})
	if got.LadderedTakerMaxProfitPnL != 0 {
		t.Fatalf("expected negative ladder min profit pnl to clamp to 0, got %.2f", got.LadderedTakerMaxProfitPnL)
	}
	got = normalizeTUISettings(TUISettings{LadderedTakerMaxProfitPnL: 1005})
	if got.LadderedTakerMaxProfitPnL != 1000.0 {
		t.Fatalf("expected ladder min profit pnl to clamp to 1000.0, got %.2f", got.LadderedTakerMaxProfitPnL)
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

func TestNormalizeTUISettingsDefaultsOneHourCryptoExitToSell999(t *testing.T) {
	got := normalizeTUISettings(TUISettings{})
	if got.OneHourCryptoExitMode != core.OneHourCryptoExitSell999 {
		t.Fatalf("expected default 1h crypto exit mode %q, got %q", core.OneHourCryptoExitSell999, got.OneHourCryptoExitMode)
	}

	got = normalizeTUISettings(TUISettings{OneHourCryptoExitMode: core.OneHourCryptoExitWaitResolve})
	if got.OneHourCryptoExitMode != core.OneHourCryptoExitWaitResolve {
		t.Fatalf("expected 1h crypto exit mode %q, got %q", core.OneHourCryptoExitWaitResolve, got.OneHourCryptoExitMode)
	}
}

func TestTradingHoursDigitsOnlyInput(t *testing.T) {
	// Test keepOnlyDigits
	if got := keepOnlyDigits("08:00-17:00"); got != "08001700" {
		t.Fatalf("expected keepOnlyDigits to return '08001700', got %q", got)
	}
	if got := keepOnlyDigits("wib 8.00-17.30"); got != "8001730" {
		t.Fatalf("expected keepOnlyDigits to return '8001730', got %q", got)
	}

	// Test formatTradingHoursFromDigits
	testCases := []struct {
		digits   string
		expected string
	}{
		{"", ""},
		{"0", "0"},
		{"08", "08"},
		{"080", "08:0"},
		{"0800", "08:00"},
		{"08001", "08:00-1"},
		{"080017", "08:00-17"},
		{"0800170", "08:00-17:0"},
		{"08001700", "08:00-17:00"},
	}
	for _, tc := range testCases {
		if got := formatTradingHoursFromDigits(tc.digits); got != tc.expected {
			t.Fatalf("formatTradingHoursFromDigits(%q) = %q; expected %q", tc.digits, got, tc.expected)
		}
	}

	// Test appendSettingsTypedInput for settingsRowTradingHoursMode
	cfg := TUISettings{}
	// Empty input, typing a digit
	got := appendSettingsTypedInput(cfg, settingsRowTradingHoursMode, "", []rune{'9'})
	if got != "9" {
		t.Fatalf("expected '9', got %q", got)
	}

	// Input "08:00", typing '1'
	got = appendSettingsTypedInput(cfg, settingsRowTradingHoursMode, "08:00", []rune{'1'})
	if got != "08:00-1" {
		t.Fatalf("expected '08:00-1', got %q", got)
	}

	// Input "08:00-17:00" (max digits), typing '9' (should be ignored)
	got = appendSettingsTypedInput(cfg, settingsRowTradingHoursMode, "08:00-17:00", []rune{'9'})
	if got != "08:00-17:00" {
		t.Fatalf("expected '08:00-17:00', got %q", got)
	}

	// Typed input with multiple runes (some non-digit)
	got = appendSettingsTypedInput(cfg, settingsRowTradingHoursMode, "08:0", []rune{'0', '-', '1', '7', ':', '0', '0'})
	if got != "08:00-17:00" {
		t.Fatalf("expected '08:00-17:00', got %q", got)
	}
}
