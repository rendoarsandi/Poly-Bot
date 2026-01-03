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
