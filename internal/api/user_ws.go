package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

type UserWSMakerOrder struct {
	OrderID       string `json:"order_id"`
	AssetID       string `json:"asset_id"`
	MatchedAmount string `json:"matched_amount"`
	Price         string `json:"price"`
	Outcome       string `json:"outcome"`
}

type OrderFillData struct {
	TradeID      string             `json:"id"`
	EventType    string             `json:"event_type"`
	Type         string             `json:"type"`
	OrderID      string             `json:"-"`
	TakerOrderID string             `json:"taker_order_id"`
	Price        string             `json:"price"`
	Size         string             `json:"size"`
	Side         string             `json:"side"`
	AssetID      string             `json:"asset_id"`
	Market       string             `json:"market"`
	Status       string             `json:"status"`
	Timestamp    string             `json:"timestamp"`
	MakerOrders  []UserWSMakerOrder `json:"maker_orders"`
}

type UserWSClient struct {
	manager *WSManager
	apiKey  string
	apiSec  string
	apiPass string

	// Callbacks
	onFill func(fill OrderFillData)

	mu                sync.Mutex
	listenStarted     bool
	authRecorded      bool
	authSent          bool
	deliveredTrades   map[string]struct{}
	subscribedMarkets map[string]struct{}
}

func NewUserWSClient(apiKey, apiSec, apiPass string) *UserWSClient {
	return &UserWSClient{
		manager:           NewWSManager("polymarket", "", "", "wss://ws-subscriptions-clob.polymarket.com/ws/user"),
		apiKey:            apiKey,
		apiSec:            apiSec,
		apiPass:           apiPass,
		deliveredTrades:   make(map[string]struct{}),
		subscribedMarkets: make(map[string]struct{}),
	}
}

func (c *UserWSClient) Connect(ctx context.Context) error {
	c.mu.Lock()
	markets := c.snapshotMarketsLocked()
	c.mu.Unlock()
	return c.ensureConnected(ctx, markets)
}

func (c *UserWSClient) SetOnFill(callback func(fill OrderFillData)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onFill = callback
}

func (c *UserWSClient) SubscribeMarkets(ctx context.Context, markets []string) error {
	normalized := normalizeMarketIDs(markets)
	if len(normalized) == 0 {
		return nil
	}

	c.mu.Lock()
	newMarkets := make([]string, 0, len(normalized))
	for _, marketID := range normalized {
		if _, exists := c.subscribedMarkets[marketID]; exists {
			continue
		}
		c.subscribedMarkets[marketID] = struct{}{}
		newMarkets = append(newMarkets, marketID)
	}
	allMarkets := c.snapshotMarketsLocked()
	connected := c.manager.connected.Load()
	authSent := c.authSent
	c.mu.Unlock()

	if !connected || !authSent {
		return c.ensureConnected(ctx, allMarkets)
	}
	if len(newMarkets) == 0 {
		return nil
	}

	err := c.manager.Subscribe(ctx, map[string]interface{}{
		"markets":   newMarkets,
		"operation": "subscribe",
	})
	if err == nil || !errors.Is(err, ErrWSConnectionUnhealthy) {
		return err
	}

	c.markAuthStale()
	return c.ensureConnected(ctx, allMarkets)
}

func (c *UserWSClient) listenLoop() {
	for {
		ctx := c.manager.getContext()
		if ctx == nil {
			return
		}

		msg, err := c.manager.ReadMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}

		c.processMessage(msg)
	}
}

func (c *UserWSClient) processMessage(msg []byte) {
	trimmed := bytes.TrimSpace(msg)
	if len(trimmed) == 0 || string(trimmed) == "PONG" {
		return
	}

	var base map[string]interface{}
	if err := json.Unmarshal(trimmed, &base); err == nil {
		c.handleEvent(base)
		return
	}

	var events []map[string]interface{}
	if err := json.Unmarshal(trimmed, &events); err == nil {
		for _, evt := range events {
			c.handleEvent(evt)
		}
	}
}

func (c *UserWSClient) handleEvent(evt map[string]interface{}) {
	evtType, _ := evt["event_type"].(string)
	rawType, _ := evt["type"].(string)
	if !strings.EqualFold(evtType, "trade") && !strings.EqualFold(rawType, "TRADE") {
		return
	}

	var fill OrderFillData
	data, err := json.Marshal(evt)
	if err != nil {
		return
	}
	if err := json.Unmarshal(data, &fill); err != nil {
		return
	}
	if fill.TakerOrderID != "" {
		fill.OrderID = fill.TakerOrderID
	} else if len(fill.MakerOrders) > 0 {
		fill.OrderID = fill.MakerOrders[0].OrderID
	}
	if !shouldEmitTradeFill(fill.Status) {
		return
	}

	c.mu.Lock()
	cb := c.onFill
	if cb == nil {
		c.mu.Unlock()
		return
	}
	if fill.TradeID != "" {
		if _, exists := c.deliveredTrades[fill.TradeID]; exists {
			c.mu.Unlock()
			return
		}
		if len(c.deliveredTrades) >= 4096 {
			c.deliveredTrades = make(map[string]struct{})
		}
		c.deliveredTrades[fill.TradeID] = struct{}{}
	}
	c.mu.Unlock()

	cb(fill)
}

func shouldEmitTradeFill(status string) bool {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "CONFIRMED":
		return true
	default:
		return false
	}
}

func (c *UserWSClient) ensureConnected(ctx context.Context, markets []string) error {
	err := c.ensureConnectedOnce(ctx, markets)
	if err == nil || !errors.Is(err, ErrWSConnectionUnhealthy) {
		return err
	}

	c.markAuthStale()
	return c.ensureConnectedOnce(ctx, markets)
}

func (c *UserWSClient) ensureConnectedOnce(ctx context.Context, markets []string) error {
	if len(markets) == 0 {
		return nil
	}

	if !c.manager.connected.Load() {
		if err := c.manager.Connect(ctx); err != nil {
			return err
		}
		c.mu.Lock()
		c.authSent = false
		c.mu.Unlock()
	}

	c.mu.Lock()
	needAuth := !c.authSent
	recordAuth := !c.authRecorded
	startListener := !c.listenStarted
	c.mu.Unlock()

	if needAuth {
		authMsg := map[string]interface{}{
			"type":    "user",
			"markets": markets,
			"auth": map[string]string{
				"apiKey":     c.apiKey,
				"secret":     c.apiSec,
				"passphrase": c.apiPass,
			},
		}
		if err := c.manager.subscribeInternal(ctx, authMsg, recordAuth); err != nil {
			return fmt.Errorf("failed to send user WS auth message: %w", err)
		}
		c.mu.Lock()
		c.authSent = true
		if recordAuth {
			c.authRecorded = true
		}
		c.mu.Unlock()
	}

	if startListener {
		c.mu.Lock()
		shouldStart := !c.listenStarted
		if shouldStart {
			c.listenStarted = true
		}
		c.mu.Unlock()
		if shouldStart {
			go c.listenLoop()
		}
	}
	return nil
}

func (c *UserWSClient) markAuthStale() {
	c.mu.Lock()
	c.authSent = false
	c.mu.Unlock()
}

func (c *UserWSClient) snapshotMarketsLocked() []string {
	markets := make([]string, 0, len(c.subscribedMarkets))
	for marketID := range c.subscribedMarkets {
		markets = append(markets, marketID)
	}
	sort.Strings(markets)
	return markets
}

func normalizeMarketIDs(markets []string) []string {
	seen := make(map[string]struct{}, len(markets))
	result := make([]string, 0, len(markets))
	for _, marketID := range markets {
		marketID = strings.TrimSpace(marketID)
		if marketID == "" {
			continue
		}
		if _, exists := seen[marketID]; exists {
			continue
		}
		seen[marketID] = struct{}{}
		result = append(result, marketID)
	}
	return result
}

func (c *UserWSClient) Close() error {
	c.mu.Lock()
	c.authSent = false
	c.listenStarted = false
	c.deliveredTrades = make(map[string]struct{})
	c.mu.Unlock()
	return c.manager.Close()
}
