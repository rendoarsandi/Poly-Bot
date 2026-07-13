package api

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
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
	AssetID   string `json:"asset_id"`
	Price     string `json:"price"`
	Size      string `json:"size"`
	Side      string `json:"side"`
	BestBid   string `json:"best_bid"`
	BestAsk   string `json:"best_ask"`
	Hash      string `json:"hash"`
	Timestamp string `json:"timestamp"`
}

type PriceUpdate struct {
	Market       string        `json:"market"`
	PriceChanges []PriceChange `json:"price_changes"`
}

type BestBidAskUpdate struct {
	EventType string `json:"event_type"`
	Market    string `json:"market"`
	AssetID   string `json:"asset_id"`
	BestBid   string `json:"best_bid"`
	BestAsk   string `json:"best_ask"`
	Spread    string `json:"spread"`
	Timestamp string `json:"timestamp"`
}

// WSMessage is a helper to detect message type
type WSMessage struct {
	EventType    string        `json:"event_type"`
	PriceChanges []PriceChange `json:"price_changes"`
}

type MarketWSMessageKind int

const (
	MarketWSMessageUnknown MarketWSMessageKind = iota
	MarketWSMessageOrderBooks
	MarketWSMessageOrderBook
	MarketWSMessagePriceUpdate
	MarketWSMessageBestBidAsk
)

type MarketWSMessage struct {
	Kind        MarketWSMessageKind
	OrderBooks  []OrderBook
	OrderBook   *OrderBook
	PriceUpdate *PriceUpdate
	BestBidAsk  *BestBidAskUpdate
}

type marketWSMessageEnvelope struct {
	EventType    string        `json:"event_type"`
	AssetID      string        `json:"asset_id"`
	Market       string        `json:"market"`
	Timestamp    string        `json:"timestamp"`
	Bids         []PriceLevel  `json:"bids"`
	Asks         []PriceLevel  `json:"asks"`
	PriceChanges []PriceChange `json:"price_changes"`
	BestBid      string        `json:"best_bid"`
	BestAsk      string        `json:"best_ask"`
	Spread       string        `json:"spread"`
}

func ParseMarketWSMessage(data []byte) (*MarketWSMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return &MarketWSMessage{Kind: MarketWSMessageUnknown}, nil
	}
	if trimmed[0] == '[' {
		var books []OrderBook
		if err := json.Unmarshal(trimmed, &books); err != nil {
			return nil, err
		}
		return &MarketWSMessage{
			Kind:       MarketWSMessageOrderBooks,
			OrderBooks: books,
		}, nil
	}

	var envelope marketWSMessageEnvelope
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return nil, err
	}

	if len(envelope.PriceChanges) > 0 {
		return &MarketWSMessage{
			Kind: MarketWSMessagePriceUpdate,
			PriceUpdate: &PriceUpdate{
				Market:       envelope.Market,
				PriceChanges: envelope.PriceChanges,
			},
		}, nil
	}

	if strings.EqualFold(strings.TrimSpace(envelope.EventType), "best_bid_ask") && envelope.AssetID != "" {
		return &MarketWSMessage{
			Kind: MarketWSMessageBestBidAsk,
			BestBidAsk: &BestBidAskUpdate{
				EventType: envelope.EventType,
				Market:    envelope.Market,
				AssetID:   envelope.AssetID,
				BestBid:   envelope.BestBid,
				BestAsk:   envelope.BestAsk,
				Spread:    envelope.Spread,
				Timestamp: envelope.Timestamp,
			},
		}, nil
	}

	if envelope.AssetID != "" || len(envelope.Bids) > 0 || len(envelope.Asks) > 0 || strings.EqualFold(strings.TrimSpace(envelope.EventType), "book") {
		return &MarketWSMessage{
			Kind: MarketWSMessageOrderBook,
			OrderBook: &OrderBook{
				EventType: envelope.EventType,
				AssetID:   envelope.AssetID,
				Market:    envelope.Market,
				Timestamp: envelope.Timestamp,
				Bids:      envelope.Bids,
				Asks:      envelope.Asks,
			},
		}, nil
	}

	return &MarketWSMessage{Kind: MarketWSMessageUnknown}, nil
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

func ParseBestBidAsk(data []byte) (*BestBidAskUpdate, error) {
	var update BestBidAskUpdate
	if err := json.Unmarshal(data, &update); err != nil {
		return nil, err
	}
	return &update, nil
}

// BestBidAskFromPriceLevels returns the best bid and best ask from raw PriceLevel slices.
func BestBidAskFromPriceLevels(bids, asks []PriceLevel) (float64, float64) {
	bestBid, bestAsk := 0.0, 0.0
	for _, b := range bids {
		p, _ := strconv.ParseFloat(b.Price, 64)
		if p > bestBid {
			bestBid = p
		}
	}
	for _, a := range asks {
		p, _ := strconv.ParseFloat(a.Price, 64)
		if p > 0 && (bestAsk == 0 || p < bestAsk) {
			bestAsk = p
		}
	}
	return bestBid, bestAsk
}
