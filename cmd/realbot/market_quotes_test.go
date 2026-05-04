package main

import (
	"math"
	"testing"
	"time"

	"Market-bot/internal/paper"
)

func TestRealbotHandleMarketWSMessageProcessesPriceChangeEnvelope(t *testing.T) {
	outcomes := []string{"Yes", "No"}
	tokenBids := map[string]float64{"No": 0.25}
	tokenAsks := map[string]float64{"No": 0.26}
	tokenFullBids := map[string][]paper.MarketLevel{}
	tokenFullAsks := map[string][]paper.MarketLevel{}
	quoteState := map[string]realbotQuoteState{}
	lastPairUpdate := time.Time{}

	depthChanged := realbotHandleMarketWSMessage(realbotMarketQuoteArgs{
		marketID:       "BTC-5m",
		tokenToOutcome: map[string]string{"yes-token": "Yes"},
		outcomes:       outcomes,
		tokenBids:      tokenBids,
		tokenAsks:      tokenAsks,
		tokenFullBids:  tokenFullBids,
		tokenFullAsks:  tokenFullAsks,
		quoteState:     quoteState,
		polySignalTracker: paper.NewDirectionalSignalTracker(
			time.Second,
			outcomes,
		),
		engine: paper.NewEngine(100),
	}, []byte(`{
		"market": "test-market",
		"price_changes": [
			{
				"asset_id": "yes-token",
				"price": "0.73",
				"size": "12",
				"side": "BUY",
				"best_bid": "0.73",
				"best_ask": "0.74",
				"timestamp": "1766789469958"
			}
		]
	}`), &lastPairUpdate)

	if !depthChanged {
		t.Fatal("expected price-change message to mark order-book depth dirty")
	}
	if math.Abs(tokenBids["Yes"]-0.73) > 0.000001 {
		t.Fatalf("expected Yes bid 0.73, got %.4f", tokenBids["Yes"])
	}
	if math.Abs(tokenAsks["Yes"]-0.74) > 0.000001 {
		t.Fatalf("expected Yes ask 0.74, got %.4f", tokenAsks["Yes"])
	}
	if len(tokenFullBids["Yes"]) == 0 {
		t.Fatal("expected bid depth to be populated")
	}
	if quoteState["Yes"].Source != "ws" {
		t.Fatalf("expected quote source ws, got %q", quoteState["Yes"].Source)
	}
}

func TestRealbotHandleMarketWSBestBidAskClearsMissingSide(t *testing.T) {
	outcomes := []string{"Down", "Up"}
	tokenBids := map[string]float64{"Up": 0.52}
	tokenAsks := map[string]float64{"Up": 0.56}
	tokenFullBids := map[string][]paper.MarketLevel{}
	tokenFullAsks := map[string][]paper.MarketLevel{}
	quoteState := map[string]realbotQuoteState{}
	lastPairUpdate := time.Time{}

	realbotHandleMarketWSMessage(realbotMarketQuoteArgs{
		marketID:          "BTC-5m",
		tokenToOutcome:    map[string]string{"up-token": "Up"},
		outcomes:          outcomes,
		tokenBids:         tokenBids,
		tokenAsks:         tokenAsks,
		tokenFullBids:     tokenFullBids,
		tokenFullAsks:     tokenFullAsks,
		quoteState:        quoteState,
		polySignalTracker: paper.NewDirectionalSignalTracker(time.Second, outcomes),
		engine:            paper.NewEngine(100),
	}, []byte(`{
		"event_type": "best_bid_ask",
		"market": "test-market",
		"asset_id": "up-token",
		"best_bid": "",
		"best_ask": "0.02",
		"spread": "0.02",
		"timestamp": "1766789469958"
	}`), &lastPairUpdate)

	if tokenBids["Up"] != 0 {
		t.Fatalf("expected missing BBO bid to clear stale bid, got %.4f", tokenBids["Up"])
	}
	if math.Abs(tokenAsks["Up"]-0.02) > 0.000001 {
		t.Fatalf("expected BBO ask 0.02, got %.4f", tokenAsks["Up"])
	}
	if quoteState["Up"].Source != "ws-bbo" {
		t.Fatalf("expected quote source ws-bbo, got %q", quoteState["Up"].Source)
	}
}

func TestRealbotHandleMarketWSPriceChangeClearsBlankExplicitBestBid(t *testing.T) {
	outcomes := []string{"Down", "Up"}
	tokenBids := map[string]float64{"Up": 0.52}
	tokenAsks := map[string]float64{"Up": 0.56}
	tokenFullBids := map[string][]paper.MarketLevel{"Up": {{Price: 0.52, Size: 10}}}
	tokenFullAsks := map[string][]paper.MarketLevel{"Up": {{Price: 0.56, Size: 10}}}
	quoteState := map[string]realbotQuoteState{}
	lastPairUpdate := time.Time{}

	realbotHandleMarketWSMessage(realbotMarketQuoteArgs{
		marketID:          "BTC-5m",
		tokenToOutcome:    map[string]string{"up-token": "Up"},
		outcomes:          outcomes,
		tokenBids:         tokenBids,
		tokenAsks:         tokenAsks,
		tokenFullBids:     tokenFullBids,
		tokenFullAsks:     tokenFullAsks,
		quoteState:        quoteState,
		polySignalTracker: paper.NewDirectionalSignalTracker(time.Second, outcomes),
		engine:            paper.NewEngine(100),
	}, []byte(`{
		"market":"test-market",
		"price_changes":[
			{
				"asset_id":"up-token",
				"price":"0.52",
				"size":"0",
				"side":"BUY",
				"best_bid":"",
				"best_ask":"0.02",
				"timestamp":"1766789469958"
			}
		]
	}`), &lastPairUpdate)

	if tokenBids["Up"] != 0 {
		t.Fatalf("expected blank explicit best_bid to clear stale bid, got %.4f", tokenBids["Up"])
	}
	if math.Abs(tokenAsks["Up"]-0.02) > 0.000001 {
		t.Fatalf("expected explicit best_ask 0.02, got %.4f", tokenAsks["Up"])
	}
}

func TestRealbotHandleMarketWSOrderBookClearsEmptySide(t *testing.T) {
	outcomes := []string{"Down", "Up"}
	tokenBids := map[string]float64{"Up": 0.52}
	tokenAsks := map[string]float64{"Up": 0.56}
	tokenFullBids := map[string][]paper.MarketLevel{}
	tokenFullAsks := map[string][]paper.MarketLevel{}
	quoteState := map[string]realbotQuoteState{}
	lastPairUpdate := time.Time{}

	realbotHandleMarketWSMessage(realbotMarketQuoteArgs{
		marketID:          "BTC-5m",
		tokenToOutcome:    map[string]string{"up-token": "Up"},
		outcomes:          outcomes,
		tokenBids:         tokenBids,
		tokenAsks:         tokenAsks,
		tokenFullBids:     tokenFullBids,
		tokenFullAsks:     tokenFullAsks,
		quoteState:        quoteState,
		polySignalTracker: paper.NewDirectionalSignalTracker(time.Second, outcomes),
		engine:            paper.NewEngine(100),
	}, []byte(`{
		"event_type": "book",
		"asset_id": "up-token",
		"timestamp": "1766789469958",
		"bids": [],
		"asks": [{"price":"0.01","size":"10"}]
	}`), &lastPairUpdate)

	if tokenBids["Up"] != 0 {
		t.Fatalf("expected empty book bid side to clear stale bid, got %.4f", tokenBids["Up"])
	}
	if math.Abs(tokenAsks["Up"]-0.01) > 0.000001 {
		t.Fatalf("expected order book ask 0.01, got %.4f", tokenAsks["Up"])
	}
	if len(tokenFullBids["Up"]) != 0 || len(tokenFullAsks["Up"]) != 1 {
		t.Fatalf("expected depth to mirror one-sided book, got bids=%v asks=%v", tokenFullBids["Up"], tokenFullAsks["Up"])
	}
}
