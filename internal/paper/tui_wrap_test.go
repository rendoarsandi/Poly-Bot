package paper

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestTUIWrapKeyOpensConfirmAndYConfirms verifies that pressing W with a non-zero
// USDC.e balance opens the confirmation overlay and pressing Y dispatches the
// wrap callback with the full balance.
func TestTUIWrapKeyOpensConfirmAndYConfirms(t *testing.T) {
	engine := NewEngine(1000.0)
	tui := NewTUI(engine, NewOrderBook())

	tui.SetWalletUSDCe(42.50)

	var called atomic.Int32
	gotAmount := make(chan float64, 1)
	tui.SetCollateralWrapHandlers(func(amount float64) {
		called.Add(1)
		gotAmount <- amount
	}, nil)

	m := tuiModel{tui: tui}

	out, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	m = out.(tuiModel)
	if m.wrapConfirmAction != "wrap" {
		t.Fatalf("expected wrapConfirmAction=wrap, got %q", m.wrapConfirmAction)
	}
	if m.wrapConfirmAmount != 42.50 {
		t.Fatalf("expected wrapConfirmAmount=42.50, got %v", m.wrapConfirmAmount)
	}

	out, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	m = out.(tuiModel)
	if m.wrapConfirmAction != "" {
		t.Fatalf("expected overlay closed after Y, got %q", m.wrapConfirmAction)
	}

	select {
	case amt := <-gotAmount:
		if amt != 42.50 {
			t.Fatalf("expected wrap callback to receive 42.50, got %v", amt)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("wrap callback was not invoked within timeout")
	}
	if called.Load() != 1 {
		t.Fatalf("expected wrap callback called exactly once, got %d", called.Load())
	}
}

// TestTUIWrapKeyCancelsOnN ensures pressing N closes the overlay without dispatching.
func TestTUIWrapKeyCancelsOnN(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.SetWalletUSDCe(10.0)

	var called atomic.Int32
	tui.SetCollateralWrapHandlers(func(amount float64) {
		called.Add(1)
	}, nil)

	m := tuiModel{tui: tui}
	out, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	m = out.(tuiModel)
	if m.wrapConfirmAction == "" {
		t.Fatalf("expected confirm overlay to open on W")
	}
	out, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m = out.(tuiModel)
	if m.wrapConfirmAction != "" {
		t.Fatalf("expected overlay cleared on N, got %q", m.wrapConfirmAction)
	}
	if called.Load() != 0 {
		t.Fatalf("expected wrap callback NOT to fire on cancel, got %d invocations", called.Load())
	}
}

// TestTUIWrapKeyNoBalance verifies that W is a no-op (no overlay) when there is
// no USDC.e to wrap.
func TestTUIWrapKeyNoBalance(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.SetCollateralWrapHandlers(func(amount float64) {}, nil)

	m := tuiModel{tui: tui}
	out, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	m = out.(tuiModel)
	if m.wrapConfirmAction != "" {
		t.Fatalf("expected no overlay when USDC.e balance is zero, got %q", m.wrapConfirmAction)
	}
}

// TestTUIUnwrapKeyOpensConfirmAndYConfirms mirrors the wrap test for U → unwrap.
func TestTUIUnwrapKeyOpensConfirmAndYConfirms(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.SetWalletCash(7.25)

	gotAmount := make(chan float64, 1)
	tui.SetCollateralWrapHandlers(nil, func(amount float64) {
		gotAmount <- amount
	})

	m := tuiModel{tui: tui}
	out, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("u")})
	m = out.(tuiModel)
	if m.wrapConfirmAction != "unwrap" {
		t.Fatalf("expected wrapConfirmAction=unwrap, got %q", m.wrapConfirmAction)
	}
	if m.wrapConfirmAmount != 7.25 {
		t.Fatalf("expected wrapConfirmAmount=7.25, got %v", m.wrapConfirmAmount)
	}
	out, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	m = out.(tuiModel)
	if m.wrapConfirmAction != "" {
		t.Fatalf("expected overlay cleared after Y, got %q", m.wrapConfirmAction)
	}
	select {
	case amt := <-gotAmount:
		if amt != 7.25 {
			t.Fatalf("expected unwrap callback to receive 7.25, got %v", amt)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("unwrap callback was not invoked within timeout")
	}
}

// TestTUIWrapCancelOnNDoesNotFireGoroutine waits briefly to confirm cancel does
// not race with a delayed goroutine dispatch.
func TestTUIWrapCancelOnNDoesNotFireGoroutine(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.SetWalletUSDCe(10.0)

	var called atomic.Int32
	tui.SetCollateralWrapHandlers(func(amount float64) { called.Add(1) }, nil)

	m := tuiModel{tui: tui}
	out, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	m = out.(tuiModel)
	out, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m = out.(tuiModel)
	time.Sleep(50 * time.Millisecond)
	if called.Load() != 0 {
		t.Fatalf("expected wrap callback NOT to fire on cancel, got %d", called.Load())
	}
}

// TestRenderWrapConfirmOverlay verifies the overlay renders the action, amount,
// and the [Y]/[N] prompt.
func TestRenderWrapConfirmOverlay(t *testing.T) {
	m := tuiModel{wrapConfirmAction: "wrap", wrapConfirmAmount: 12.34}
	out := m.renderWrapConfirmOverlay(80)
	for _, want := range []string{"Confirm Wrap", "12.34 USDC.e", "[Y]", "[N]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected overlay to contain %q, got:\n%s", want, out)
		}
	}

	m = tuiModel{wrapConfirmAction: "unwrap", wrapConfirmAmount: 99.99}
	out = m.renderWrapConfirmOverlay(80)
	for _, want := range []string{"Confirm Unwrap", "99.99 pUSD", "[Y]", "[N]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected overlay to contain %q, got:\n%s", want, out)
		}
	}
}

// TestRenderAccountStatusRealModeShowsUSDCeAndPOL verifies the wallet line surfaces
// USDC.e + POL balances in real (non-paper) mode.
func TestRenderAccountStatusRealModeShowsUSDCeAndPOL(t *testing.T) {
	model := tuiModel{
		snap: tuiSnapshot{
			mode:           "Real",
			tradeFactor:    0.05,
			walletCash:     50.0,
			hasWalletCash:  true,
			walletUSDCe:    25.55,
			hasWalletUSDCe: true,
			walletPOL:      0.1234,
			hasWalletPOL:   true,
		},
	}
	rendered := model.renderAccountStatus(140, Stats{
		CurrentBalance:  50.0,
		StartingBalance: 50.0,
	}, 0.0, 0, 50.0, 50.0, 1.0, 50.0, 0, 0, 0, nil)

	for _, want := range []string{"USDC.e", "$25.55", "POL", "0.1234", "[W]", "[U]"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("expected account panel to contain %q, got:\n%s", want, rendered)
		}
	}
}
