package main

import (
	"context"
	"testing"
	"time"

	"Market-bot/internal/core"
	"Market-bot/internal/paper"
)

func TestRealbotEnsureFreshSellExecutionQuoteUsesLocalQuoteWithinConfiguredAge(t *testing.T) {
	outcomes := []string{"YES", "NO"}
	tokenBids := map[string]float64{"YES": 0.53, "NO": 0.52}
	tokenAsks := map[string]float64{"YES": 0.55, "NO": 0.54}
	tokenFullBids := map[string][]paper.MarketLevel{
		"YES": {{Price: 0.53, Size: 25}},
		"NO":  {{Price: 0.52, Size: 25}},
	}
	tokenFullAsks := map[string][]paper.MarketLevel{
		"YES": {{Price: 0.55, Size: 25}},
		"NO":  {{Price: 0.54, Size: 25}},
	}
	quoteState := map[string]realbotQuoteState{
		"YES": {UpdatedAt: time.Now().Add(-500 * time.Millisecond), Source: "ws"},
		"NO":  {UpdatedAt: time.Now().Add(-450 * time.Millisecond), Source: "ws"},
	}

	source, _, _, err := realbotEnsureFreshSellExecutionQuote(
		context.Background(),
		nil,
		nil,
		outcomes,
		tokenBids,
		tokenAsks,
		tokenFullBids,
		tokenFullAsks,
		quoteState,
		time.Now().Add(-500*time.Millisecond),
		core.ResolveExecutionLocalQuoteMaxAge(&core.Config{ExecutionLocalQuoteMaxAgeMs: 750}),
		nil,
	)
	if err != nil {
		t.Fatalf("expected local quote to be accepted without REST refresh: %v", err)
	}
	if source != "local" {
		t.Fatalf("expected local quote source, got %q", source)
	}
}

func TestRealbotEnsureFreshBuyExecutionQuoteUsesLocalQuoteWithinConfiguredAge(t *testing.T) {
	outcomes := []string{"YES", "NO"}
	tokenBids := map[string]float64{"YES": 0.53, "NO": 0.52}
	tokenAsks := map[string]float64{"YES": 0.55, "NO": 0.54}
	tokenFullBids := map[string][]paper.MarketLevel{
		"YES": {{Price: 0.53, Size: 25}},
		"NO":  {{Price: 0.52, Size: 25}},
	}
	tokenFullAsks := map[string][]paper.MarketLevel{
		"YES": {{Price: 0.55, Size: 25}},
		"NO":  {{Price: 0.54, Size: 25}},
	}
	quoteState := map[string]realbotQuoteState{
		"YES": {UpdatedAt: time.Now().Add(-500 * time.Millisecond), Source: "ws"},
		"NO":  {UpdatedAt: time.Now().Add(-450 * time.Millisecond), Source: "ws"},
	}

	source, _, _, err := realbotEnsureFreshBuyExecutionQuote(
		context.Background(),
		nil,
		nil,
		outcomes,
		tokenBids,
		tokenAsks,
		tokenFullBids,
		tokenFullAsks,
		quoteState,
		time.Now().Add(-500*time.Millisecond),
		core.ResolveExecutionLocalQuoteMaxAge(&core.Config{ExecutionLocalQuoteMaxAgeMs: 750}),
		nil,
	)
	if err != nil {
		t.Fatalf("expected local quote to be accepted without REST refresh: %v", err)
	}
	if source != "local" {
		t.Fatalf("expected local quote source, got %q", source)
	}
}
