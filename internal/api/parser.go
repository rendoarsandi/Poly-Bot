package api

import (
	"encoding/json"
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

func ParseOrderBooks(data []byte) ([]OrderBook, error) {
	var books []OrderBook
	if err := json.Unmarshal(data, &books); err != nil {
		return nil, err
	}
	return books, nil
}

func ParsePriceUpdate(data []byte) (*PriceUpdate, error) {
	var update PriceUpdate
	if err := json.Unmarshal(data, &update); err != nil {
		return nil, err
	}
	return &update, nil
}
