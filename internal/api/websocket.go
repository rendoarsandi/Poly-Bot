package api

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

const (
	// Heartbeat interval - send ping every 30 seconds
	heartbeatInterval = 30 * time.Second
	// If no message received in this time, consider connection dead
	readTimeout = 45 * time.Second
	// Max reconnection attempts before giving up
	maxReconnectAttempts = 5
	// Delay between reconnection attempts
	reconnectDelay = 2 * time.Second
)

type WSManager struct {
	URL  string
	conn *websocket.Conn
	mu   sync.Mutex

	// Connection state
	connected     atomic.Bool
	lastMessage   atomic.Int64 // Unix timestamp of last message
	lastHeartbeat atomic.Int64

	// Subscription state for reconnection
	subscriptions []interface{}
	subMu         sync.Mutex

	// Reconnection
	reconnecting atomic.Bool
	ctx          context.Context
	cancel       context.CancelFunc

	// Stats
	reconnectCount atomic.Int32
	messageCount   atomic.Int64
}

func NewWSManager(url string) *WSManager {
	if url == "" {
		url = "wss://ws-subscriptions-clob.polymarket.com/ws/market"
	}
	return &WSManager{
		URL:           url,
		subscriptions: make([]interface{}, 0),
	}
}

func (m *WSManager) Connect(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Store context for reconnection
	m.ctx, m.cancel = context.WithCancel(ctx)

	if err := m.connectInternal(m.ctx); err != nil {
		return err
	}

	// Start heartbeat goroutine
	go m.heartbeatLoop()

	return nil
}

func (m *WSManager) connectInternal(ctx context.Context) error {
	opts := &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	}

	c, _, err := websocket.Dial(ctx, m.URL, opts)
	if err != nil {
		return fmt.Errorf("failed to dial: %w", err)
	}

	// Set read limit to handle large order books
	c.SetReadLimit(1024 * 1024) // 1MB

	m.conn = c
	m.connected.Store(true)
	m.lastMessage.Store(time.Now().Unix())
	m.lastHeartbeat.Store(time.Now().Unix())

	return nil
}

func (m *WSManager) heartbeatLoop() {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			if !m.connected.Load() {
				continue
			}

			// Check if we've received any message recently
			lastMsg := time.Unix(m.lastMessage.Load(), 0)
			if time.Since(lastMsg) > readTimeout {
				// Connection seems dead, trigger reconnect
				go m.tryReconnect()
				continue
			}

			// Send ping
			m.mu.Lock()
			if m.conn != nil {
				ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
				err := m.conn.Ping(ctx)
				cancel()
				if err != nil {
					m.mu.Unlock()
					go m.tryReconnect()
					continue
				}
				m.lastHeartbeat.Store(time.Now().Unix())
			}
			m.mu.Unlock()
		}
	}
}

func (m *WSManager) tryReconnect() {
	// Prevent multiple simultaneous reconnection attempts
	if !m.reconnecting.CompareAndSwap(false, true) {
		return
	}
	defer m.reconnecting.Store(false)

	m.connected.Store(false)

	for attempt := 1; attempt <= maxReconnectAttempts; attempt++ {
		select {
		case <-m.ctx.Done():
			return
		default:
		}

		// Close existing connection
		m.mu.Lock()
		if m.conn != nil {
			m.conn.Close(websocket.StatusGoingAway, "reconnecting")
			m.conn = nil
		}
		m.mu.Unlock()

		// Wait before reconnecting
		select {
		case <-m.ctx.Done():
			return
		case <-time.After(reconnectDelay * time.Duration(attempt)):
		}

		// Try to reconnect
		m.mu.Lock()
		err := m.connectInternal(m.ctx)
		m.mu.Unlock()

		if err != nil {
			continue
		}

		// Re-subscribe to all previous subscriptions
		m.subMu.Lock()
		for _, sub := range m.subscriptions {
			ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
			err := m.Subscribe(ctx, sub)
			cancel()
			if err != nil {
				m.subMu.Unlock()
				m.connected.Store(false)
				continue
			}
		}
		m.subMu.Unlock()

		m.reconnectCount.Add(1)
		return
	}
}

func (m *WSManager) Close() error {
	if m.cancel != nil {
		m.cancel()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.connected.Store(false)

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

	err := wsjson.Write(ctx, m.conn, payload)
	if err != nil {
		return err
	}

	// Store subscription for reconnection
	m.subMu.Lock()
	m.subscriptions = append(m.subscriptions, payload)
	m.subMu.Unlock()

	return nil
}

func (m *WSManager) ReadMessage(ctx context.Context) ([]byte, error) {
	if m.conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	_, p, err := m.conn.Read(ctx)
	if err != nil {
		// Trigger reconnection on read error
		go m.tryReconnect()
		return nil, err
	}

	m.lastMessage.Store(time.Now().Unix())
	m.messageCount.Add(1)
	return p, nil
}

// ReadMessageWithTimeout reads with a timeout, returning nil if timeout exceeded
func (m *WSManager) ReadMessageWithTimeout(ctx context.Context, timeout time.Duration) ([]byte, error) {
	// Check parent context first
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if !m.connected.Load() {
		// Try to trigger reconnection if not connected
		if !m.reconnecting.Load() {
			go m.tryReconnect()
		}
		return nil, nil
	}

	m.mu.Lock()
	conn := m.conn
	m.mu.Unlock()

	if conn == nil {
		return nil, nil
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	_, p, err := conn.Read(timeoutCtx)
	if err != nil {
		// Check if parent context was cancelled (Ctrl+C)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		// If it's just a timeout, return nil without error
		if timeoutCtx.Err() == context.DeadlineExceeded {
			return nil, nil
		}
		// On other errors, trigger reconnection
		go m.tryReconnect()
		return nil, err
	}

	m.lastMessage.Store(time.Now().Unix())
	m.messageCount.Add(1)
	return p, nil
}

// IsConnected returns current connection status
func (m *WSManager) IsConnected() bool {
	return m.connected.Load()
}

// GetStats returns connection statistics
func (m *WSManager) GetStats() (connected bool, lastMsg time.Time, reconnects int32, messages int64) {
	return m.connected.Load(),
		time.Unix(m.lastMessage.Load(), 0),
		m.reconnectCount.Load(),
		m.messageCount.Load()
}

// TimeSinceLastMessage returns duration since last message received
func (m *WSManager) TimeSinceLastMessage() time.Duration {
	return time.Since(time.Unix(m.lastMessage.Load(), 0))
}
