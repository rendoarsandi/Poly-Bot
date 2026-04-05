package analysis

import (
	"testing"
	"time"

	"Market-bot/internal/api"
)

func TestAnalyzePublicWalletClassifiesHedgedDirectionalAccumulator(t *testing.T) {
	trades := []api.PublicTrade{
		testTrade("0xabc", "eth-updown-5m-1775331600", "BUY", "Up", 20, 0.72, 1775331601),
		testTrade("0xabc", "eth-updown-5m-1775331600", "BUY", "Up", 20, 0.63, 1775331610),
		testTrade("0xabc", "eth-updown-5m-1775331600", "BUY", "Down", 20, 0.18, 1775331620),
		testTrade("0xabc", "eth-updown-5m-1775331600", "BUY", "Down", 20, 0.24, 1775331630),
		testTrade("0xdef", "eth-updown-5m-1775331900", "BUY", "Up", 20, 0.20, 1775331901),
		testTrade("0xdef", "eth-updown-5m-1775331900", "BUY", "Up", 20, 0.26, 1775331910),
		testTrade("0xdef", "eth-updown-5m-1775331900", "BUY", "Down", 20, 0.78, 1775331920),
		testTrade("0xdef", "eth-updown-5m-1775331900", "BUY", "Down", 20, 0.86, 1775331930),
	}

	report := AnalyzePublicWallet("0xwallet", trades, nil)
	if report.Strategy != StrategyHedgedDirectionalAccumulator {
		t.Fatalf("expected %q, got %q", StrategyHedgedDirectionalAccumulator, report.Strategy)
	}
	if report.PrimaryFamily != "eth-updown-5m" {
		t.Fatalf("expected primary family eth-updown-5m, got %q", report.PrimaryFamily)
	}
	if report.BothOutcomeConditionPct != 1 {
		t.Fatalf("expected both-outcome ratio 1.0, got %.3f", report.BothOutcomeConditionPct)
	}
	if report.SellTradePct != 0 {
		t.Fatalf("expected zero sell ratio, got %.3f", report.SellTradePct)
	}
}

func TestAnalyzePublicWalletClassifiesTwoSidedMakerChurn(t *testing.T) {
	trades := []api.PublicTrade{
		testTrade("0xmaker", "btc-updown-5m-1775331600", "BUY", "Up", 10, 0.48, 1775331601),
		testTrade("0xmaker", "btc-updown-5m-1775331600", "SELL", "Up", 10, 0.53, 1775331609),
		testTrade("0xmaker", "btc-updown-5m-1775331600", "BUY", "Down", 10, 0.46, 1775331615),
		testTrade("0xmaker", "btc-updown-5m-1775331600", "SELL", "Down", 10, 0.51, 1775331622),
		testTrade("0xmaker", "btc-updown-5m-1775331900", "BUY", "Up", 10, 0.47, 1775331901),
		testTrade("0xmaker", "btc-updown-5m-1775331900", "SELL", "Up", 10, 0.52, 1775331908),
		testTrade("0xmaker", "btc-updown-5m-1775331900", "BUY", "Down", 10, 0.45, 1775331912),
		testTrade("0xmaker", "btc-updown-5m-1775331900", "SELL", "Down", 10, 0.50, 1775331918),
	}

	report := AnalyzePublicWallet("0xwallet", trades, nil)
	if report.Strategy != StrategyTwoSidedMakerChurn {
		t.Fatalf("expected %q, got %q", StrategyTwoSidedMakerChurn, report.Strategy)
	}
	if report.SellTradePct < 0.4 {
		t.Fatalf("expected sell ratio >= 0.4, got %.3f", report.SellTradePct)
	}
}

func TestSlugFamilyStripsTrailingTimestamp(t *testing.T) {
	if got := slugFamily("eth-updown-5m-1775331600"); got != "eth-updown-5m" {
		t.Fatalf("unexpected family %q", got)
	}
	if got := slugFamily("custom-market"); got != "custom-market" {
		t.Fatalf("unexpected family %q", got)
	}
}

func testTrade(conditionID, slug, side, outcome string, size, price float64, ts int64) api.PublicTrade {
	return api.PublicTrade{
		ConditionID: conditionID,
		Slug:        slug,
		Title:       slug,
		Side:        side,
		Outcome:     outcome,
		Size:        size,
		Price:       price,
		Timestamp:   ts,
		ObservedAt:  time.Unix(ts, 0).Unix(),
	}
}
