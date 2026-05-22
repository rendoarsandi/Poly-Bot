package paper

import (
	"testing"

	"Market-bot/internal/core"
)

func TestNormalizeStartupWizardSettingsAllowsRealbotPaperModes(t *testing.T) {
	got := normalizeStartupWizardSettings(TUISettings{
		ExecutionBackend:     core.ExecutionBackendPaper,
		PaperArbMode:         "maker",
		SplitStrategyEnabled: true,
	}, "Real")

	if got.PaperArbMode != "maker" {
		t.Fatalf("expected realbot paper startup settings to allow maker, got %q", got.PaperArbMode)
	}
	if got.SplitStrategyEnabled {
		t.Fatal("expected realbot paper startup settings to disable split strategy")
	}
}

func TestNormalizeStartupWizardSettingsPreservesOneHourTimeframe(t *testing.T) {
	got := normalizeStartupWizardSettings(TUISettings{Timeframe: "1h"}, "Paper")
	if got.Timeframe != "1h" {
		t.Fatalf("expected startup wizard to preserve 1h timeframe, got %q", got.Timeframe)
	}
}
