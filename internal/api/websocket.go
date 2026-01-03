package api

import (
	"context"
	"fmt"
	"sync"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

type WSManager struct {
	URL  string
	conn *websocket.Conn
	mu   sync.Mutex
}

func NewWSManager(url string) *WSManager {
	if url == "" {
		url = "wss://ws-subscriptions-clob.polymarket.com/ws/market"
	}
	return &WSManager{URL: url}
}

func (m *WSManager) Connect(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	c, _, err := websocket.Dial(ctx, m.URL, nil)
	if err != nil {
		return fmt.Errorf("failed to dial: %w", err)
	}
	m.conn = c
	return nil
}

func (m *WSManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.conn != nil {
		return m.conn.Close(websocket.StatusNormalClosure, "")
	}
	return nil
}

func (m *WSManager) Subscribe(ctx context.Context, payload interface{}) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.conn == nil {
		return fmt.Errorf("not connected")
	}

	return wsjson.Write(ctx, m.conn, payload)
}

func (m *WSManager) ReadMessage(ctx context.Context) ([]byte, error) {
	if m.conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	_, p, err := m.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// ReadMessageWithTimeout reads with a timeout, returning nil if timeout exceeded
func (m *WSManager) ReadMessageWithTimeout(ctx context.Context, timeout time.Duration) ([]byte, error) {
	if m.conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	_, p, err := m.conn.Read(timeoutCtx)
	if err != nil {
		// If it's just a timeout, return nil without error
		if timeoutCtx.Err() == context.DeadlineExceeded {
			return nil, nil
		}
		return nil, err
	}
	return p, nil
}
