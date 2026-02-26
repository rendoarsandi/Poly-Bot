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
	Bids      []PriceLevel `json:"bids"`
	Asks      []PriceLevel `json:"asks"`
}

type PriceChange struct {
	AssetID string `json:"asset_id"`
	Price   string `json:"price"`
	Size    string `json:"size"`
	Side    string `json:"side"`
}

type PriceUpdate struct {
	Market       string        `json:"market"`
	PriceChanges []PriceChange `json:"price_changes"`
}

// WSMessage is a helper to detect message type
type WSMessage struct {
	EventType    string        `json:"event_type"`
	PriceChanges []PriceChange `json:"price_changes"`
}

func ParseOrderBook(data []byte) (*OrderBook, error) {
	var book OrderBook
	if err := json.Unmarshal(data, &book); err != nil {
		return nil, err
	}
	return &book, nil
}

// ParseOrderBooks parses WS order-book snapshots.
// Polymarket sends either a JSON array "[{...},{...}]" (multi-token batch)
// or a single JSON object "{...}" (single-token snapshot).  We handle both
// so no snapshot is silently dropped.
func ParseOrderBooks(data []byte) ([]OrderBook, error) {
	// Fast path: array form (most common for subscribed multi-asset streams).
	var books []OrderBook
	if err := json.Unmarshal(data, &books); err == nil && len(books) > 0 {
		return books, nil
	}
	// Fallback: single-object form.
	var single OrderBook
	if err := json.Unmarshal(data, &single); err != nil {
		return nil, err
	}
	// Only return it as a real book snapshot if it has an asset_id or bids/asks.
	if single.AssetID != "" || len(single.Bids) > 0 || len(single.Asks) > 0 {
		return []OrderBook{single}, nil
	}
	return nil, fmt.Errorf("no orderbook data in message")
}

func ParsePriceUpdate(data []byte) (*PriceUpdate, error) {
	var update PriceUpdate
	if err := json.Unmarshal(data, &update); err != nil {
		return nil, err
	}
	return &update, nil
}
