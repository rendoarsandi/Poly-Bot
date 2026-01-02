package api

import (
	"encoding/json"
	"fmt"
)

type PriceLevel struct {
	Price string `json:"price"`
	Size  string `json:"size"`
}

type OrderBook struct {
	EventType string       `json:"event_type"`
	AssetID   string       `json:"asset_id"`
	Market    string       `json:"market"`
	Timestamp string       `json:"timestamp"`
	Hash      string       `json:"hash"`
	Buys      []PriceLevel `json:"buys"`
	Sells     []PriceLevel `json:"sells"`
}

func ParseOrderBook(data []byte) (*OrderBook, error) {
	var book OrderBook
	if err := json.Unmarshal(data, &book); err != nil {
		return nil, fmt.Errorf("failed to unmarshal order book: %w", err)
	}

	if book.EventType != "book" {
		return nil, fmt.Errorf("unexpected event type: %s", book.EventType)
	}

	return &book, nil
}
