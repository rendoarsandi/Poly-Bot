package main

import (
	"context"
	"math"
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

func TestRealbotWaitForMarketWakeProcessesIncomingWSMessage(t *testing.T) {
	wsMsgChan := make(chan []byte, 1)
	wsMsgChan <- []byte(`{
		"event_type": "best_bid_ask",
		"market": "test-market",
		"asset_id": "yes-token",
		"best_bid": "0.73",
		"best_ask": "0.77",
		"spread": "0.04",
		"timestamp": "1766789469958"
	}`)

	lastPairUpdate := time.Time{}
	wsChannelClosed := true
	tokenBids := map[string]float64{"No": 0.21}
	tokenAsks := map[string]float64{"No": 0.22}
	quoteState := map[string]realbotQuoteState{
		"No": {UpdatedAt: time.Now(), Source: "ws"},
	}

	realbotWaitForMarketWake(realbotMarketQuoteArgs{
		ctx:               context.Background(),
		wsMsgChan:         wsMsgChan,
		tokenToOutcome:    map[string]string{"yes-token": "Yes"},
		outcomes:          []string{"No", "Yes"},
		tokenBids:         tokenBids,
		tokenAsks:         tokenAsks,
		tokenFullBids:     map[string][]paper.MarketLevel{},
		tokenFullAsks:     map[string][]paper.MarketLevel{},
		quoteState:        quoteState,
		polySignalTracker: paper.NewDirectionalSignalTracker(time.Second, []string{"No", "Yes"}),
		engine:            paper.NewEngine(100),
	}, time.Second, &lastPairUpdate, make(chan realbotAsyncEntryResult), &realbotAsyncEntryState{}, &wsChannelClosed)

	if math.Abs(tokenBids["Yes"]-0.73) > 0.000001 {
		t.Fatalf("expected Yes bid 0.73, got %.4f", tokenBids["Yes"])
	}
	if math.Abs(tokenAsks["Yes"]-0.77) > 0.000001 {
		t.Fatalf("expected Yes ask 0.77, got %.4f", tokenAsks["Yes"])
	}
	if quoteState["Yes"].Source != "ws-bbo" {
		t.Fatalf("expected Yes quote source ws-bbo, got %q", quoteState["Yes"].Source)
	}
	if lastPairUpdate.IsZero() {
		t.Fatal("expected incoming WS message to refresh pair timestamp")
	}
	if wsChannelClosed {
		t.Fatal("expected successful WS wake to clear closed-channel flag")
	}
}

func TestRealbotWaitForMarketWakeAppliesAsyncEntryResult(t *testing.T) {
	entryExecutionDone := make(chan realbotAsyncEntryResult, 1)
	now := time.Now()
	entryExecutionDone <- realbotAsyncEntryResult{
		lastTradeAt:            now,
		cooldownUntil:          now.Add(5 * time.Second),
		ladderedEntrySeq:       7,
		ladderedEntryConfirmed: false,
	}

	entryExecutionInFlight := true
	ladderedEntries := []realbotLadderedEntry{
		{seq: 7, ask0: 0.40, ask1: 0.41},
		{seq: 8, ask0: 0.42, ask1: 0.43},
	}
	lastTrade := time.Time{}
	panicBuyCooldown := time.Time{}

	realbotWaitForMarketWake(realbotMarketQuoteArgs{
		ctx:       context.Background(),
		wsMsgChan: make(chan []byte),
	}, time.Second, nil, entryExecutionDone, &realbotAsyncEntryState{
		entryExecutionInFlight: &entryExecutionInFlight,
		ladderedEntries:        &ladderedEntries,
		lastTrade:              &lastTrade,
		panicBuyCooldown:       &panicBuyCooldown,
	}, nil)

	if entryExecutionInFlight {
		t.Fatal("expected async wake to clear in-flight flag")
	}
	if len(ladderedEntries) != 1 || ladderedEntries[0].seq != 8 {
		t.Fatalf("expected confirmed laddered entry to be removed, got %+v", ladderedEntries)
	}
	if !lastTrade.Equal(now) {
		t.Fatalf("expected last trade timestamp %v, got %v", now, lastTrade)
	}
	if !panicBuyCooldown.Equal(now.Add(5 * time.Second)) {
		t.Fatalf("expected cooldown to update, got %v", panicBuyCooldown)
	}
}
