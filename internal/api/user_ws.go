package api

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

type OrderFillData struct {
	OrderID   string  `json:"order_id"`
	Maker     string  `json:"maker"`
	Taker     string  `json:"taker"`
	Price     string  `json:"price"` // Polymarket uses string numbers for Fills
	Size      string  `json:"size"`
	Side      string  `json:"side"`
	AssetID   string  `json:"asset_id"` // TokenID
	Timestamp string  `json:"timestamp"`
}

type UserWSClient struct {
	manager  *WSManager
	apiKey   string
	apiSec   string
	apiPass  string

	// Callbacks
	onFill func(fill OrderFillData)

	mu sync.Mutex
}

func NewUserWSClient(apiKey, apiSec, apiPass string) *UserWSClient {
	return &UserWSClient{
		manager: NewWSManager("wss://ws-subscriptions-clob.polymarket.com/ws/user"),
		apiKey:  apiKey,
		apiSec:  apiSec,
		apiPass: apiPass,
	}
}

func (c *UserWSClient) Connect(ctx context.Context) error {
	if err := c.manager.Connect(ctx); err != nil {
		return err
	}

	authMsg := map[string]interface{}{
		"type": "user",
		"auth": map[string]string{
			"apiKey":     c.apiKey,
			"secret":     c.apiSec,
			"passphrase": c.apiPass,
		},
	}
	
	if err := c.manager.Subscribe(ctx, authMsg); err != nil {
		return fmt.Errorf("failed to send user WS auth message: %w", err)
	}

	go c.listenLoop()
	return nil
}

func (c *UserWSClient) SetOnFill(callback func(fill OrderFillData)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onFill = callback
}

func (c *UserWSClient) listenLoop() {
	for {
		ctx := c.manager.getContext()
		if ctx == nil {
			return
		}

		msg, err := c.manager.ReadMessage(ctx)
		if err != nil {
			// Reconnection is handled by WSManager, we just back off slightly
			time.Sleep(1 * time.Second)
			continue
		}

		// Look at what's in the message
		var base map[string]interface{}
		if err := json.Unmarshal(msg, &base); err != nil {
			// Could be an array of events
			var events []map[string]interface{}
			if err := json.Unmarshal(msg, &events); err == nil {
				for _, evt := range events {
					c.handleEvent(evt)
				}
			}
			continue
		}
		
		c.handleEvent(base)
	}
}

func (c *UserWSClient) handleEvent(evt map[string]interface{}) {
	evtType, ok := evt["event"].(string)
	if !ok {
		return
	}

	if evtType == "order_fill" {
		c.mu.Lock()
		cb := c.onFill
		c.mu.Unlock()

		if cb != nil {
			var fill OrderFillData
			// re-encode data to map it to struct if possible. Or we just map directly
			data, err := json.Marshal(evt)
			if err == nil {
				if err2 := json.Unmarshal(data, &fill); err2 == nil {
					cb(fill)
				}
			}
		}
	}
}

func (c *UserWSClient) Close() error {
	return c.manager.Close()
}
