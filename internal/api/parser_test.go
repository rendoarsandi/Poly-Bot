package api

import (
	"testing"
)

func TestParseOrderBook(t *testing.T) {
	rawJSON := `{
		"event_type": "book",
		"asset_id": "yes-token",
		"market": "test-condition",
		"timestamp": "123456789",
		"hash": "somehash",
		"bids": [
			{"price": "0.48", "size": "100"}
		],
		"asks": [
			{"price": "0.50", "size": "200"}
		]
	}`

	book, err := ParseOrderBook([]byte(rawJSON))
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	if book.AssetID != "yes-token" {
		t.Errorf("Expected AssetID 'yes-token', got %s", book.AssetID)
	}

	if len(book.Bids) != 1 || book.Bids[0].Price != "0.48" {
		t.Errorf("Expected Bid Price '0.48', got %+v", book.Bids)
	}

	if len(book.Asks) != 1 || book.Asks[0].Price != "0.50" {
		t.Errorf("Expected Ask Price '0.50', got %+v", book.Asks)
	}
}

func TestParseOrderBookEmptyBidsAsks(t *testing.T) {
	rawJSON := `{"event_type": "book", "asset_id": "test", "bids": [], "asks": []}`
	book, err := ParseOrderBook([]byte(rawJSON))
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	if len(book.Bids) != 0 || len(book.Asks) != 0 {
		t.Errorf("Expected empty bids and asks, got bids=%d asks=%d", len(book.Bids), len(book.Asks))
	}
}

func TestParsePriceUpdatePreservesExplicitBestBidAsk(t *testing.T) {
	rawJSON := `{
		"market": "test-market",
		"price_changes": [
			{
				"asset_id": "yes-token",
				"price": "0.74",
				"size": "10",
				"side": "SELL",
				"best_bid": "0.73",
				"best_ask": "0.74",
				"timestamp": "1766789469958"
			}
		]
	}`

	update, err := ParsePriceUpdate([]byte(rawJSON))
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	if len(update.PriceChanges) != 1 {
		t.Fatalf("expected 1 price change, got %d", len(update.PriceChanges))
	}
	change := update.PriceChanges[0]
	if change.BestBid != "0.73" || change.BestAsk != "0.74" {
		t.Fatalf("expected explicit best bid/ask, got %+v", change)
	}
	if change.Timestamp != "1766789469958" {
		t.Fatalf("expected timestamp to round-trip, got %q", change.Timestamp)
	}
}

func TestParseBestBidAsk(t *testing.T) {
	rawJSON := `{
		"event_type": "best_bid_ask",
		"market": "test-market",
		"asset_id": "yes-token",
		"best_bid": "0.73",
		"best_ask": "0.77",
		"spread": "0.04",
		"timestamp": "1766789469958"
	}`

	update, err := ParseBestBidAsk([]byte(rawJSON))
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	if update.EventType != "best_bid_ask" {
		t.Fatalf("expected best_bid_ask event, got %q", update.EventType)
	}
	if update.AssetID != "yes-token" || update.BestBid != "0.73" || update.BestAsk != "0.77" {
		t.Fatalf("unexpected best bid/ask payload: %+v", update)
	}
}
