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
		"buys": [
			{"price": "0.48", "size": "100"}
		],
		"sells": [
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

	if len(book.Buys) != 1 || book.Buys[0].Price != "0.48" {
		t.Errorf("Expected Buy Price '0.48', got %+v", book.Buys)
	}

	if len(book.Sells) != 1 || book.Sells[0].Price != "0.50" {
		t.Errorf("Expected Sell Price '0.50', got %+v", book.Sells)
	}
}

func TestParseOrderBookWrongEvent(t *testing.T) {
	rawJSON := `{"event_type": "not-a-book"}`
	_, err := ParseOrderBook([]byte(rawJSON))
	if err == nil {
		t.Fatal("Expected error for wrong event type, got nil")
	}
}
