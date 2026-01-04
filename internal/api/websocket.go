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
	// Heartbeat interval - check connection every 30 seconds
	// Longer interval to handle Android background throttling
	heartbeatInterval = 30 * time.Second
	// If no message received in this time, consider connection dead
	// (Only used as fallback - ping is primary health check)
	readTimeout = 60 * time.Second
	// Max reconnection attempts before giving up
	maxReconnectAttempts = 10
	// Delay between reconnection attempts (starts at 1s, doubles each attempt)
	reconnectDelay = 1 * time.Second
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
	// Add connection timeout
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	opts := &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	}

	c, _, err := websocket.Dial(dialCtx, m.URL, opts)
	if err != nil {
		return fmt.Errorf("failed to dial %s: %w", m.URL, err)
	}

	// Set read limit to handle large order books
	c.SetReadLimit(1024 * 1024) // 1MB

	m.conn = c
	
	m.connected.Store(true)
	m.lastMessage.Store(time.Now().Unix())
	m.lastHeartbeat.Store(time.Now().Unix())

	return nil
}

func (m *WSManager) getConn() *websocket.Conn {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conn
}

func (m *WSManager) heartbeatLoop() {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	consecutivePingFailures := 0
	const maxPingFailures = 3 // Allow a few failures before reconnecting

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			if !m.connected.Load() {
				consecutivePingFailures = 0
				continue
			}

			// Get connection safely with minimal lock time
			m.mu.Lock()
			conn := m.conn
			m.mu.Unlock()

			if conn != nil {
				// Send ping outside of lock to prevent blocking
				// Use longer timeout - Android may throttle when backgrounded
				ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
				err := conn.Ping(ctx)
				cancel()
				if err != nil {
					consecutivePingFailures++

					// Only reconnect after multiple failures (handles temporary throttling)
					if consecutivePingFailures >= maxPingFailures {
						consecutivePingFailures = 0
						go m.tryReconnect()
					}
					continue
				}
				// Ping succeeded - connection is alive
				consecutivePingFailures = 0
				m.lastHeartbeat.Store(time.Now().Unix())
				// Also update lastMessage so inactive markets don't show as stale
				// (connection is alive, just no trading activity)
				m.lastMessage.Store(time.Now().Unix())
			}
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

attemptLoop:
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

		// Exponential backoff with cap at 10 seconds
		delay := reconnectDelay * time.Duration(1<<uint(attempt-1))
		if delay > 10*time.Second {
			delay = 10 * time.Second
		}
		select {
		case <-m.ctx.Done():
			return
		case <-time.After(delay):
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
		subscriptions := make([]interface{}, len(m.subscriptions))
		copy(subscriptions, m.subscriptions)
		m.subMu.Unlock()

		allSubscribed := true
		for _, sub := range subscriptions {
			ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
			err := m.Subscribe(ctx, sub)
			cancel()
			if err != nil {
				m.connected.Store(false)
				allSubscribed = false
				break
			}
		}
		if !allSubscribed {
			continue attemptLoop
		}

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
	conn := m.getConn()
	if conn == nil {
		return fmt.Errorf("not connected")
	}

	err := wsjson.Write(ctx, conn, payload)
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
	conn := m.getConn()
	if conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	_, p, err := conn.Read(ctx)
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

	conn := m.getConn()
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

// ForceReconnect triggers a reconnection attempt from external code
func (m *WSManager) ForceReconnect() {
	// Close existing connection to force a fresh dial
	m.mu.Lock()
	if m.conn != nil {
		m.conn.Close(websocket.StatusGoingAway, "force reconnect")
		m.conn = nil
	}
	m.mu.Unlock()

	// Mark as disconnected to trigger tryReconnect's logic
	m.connected.Store(false)
	// Trigger reconnection in background
	go m.tryReconnect()
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

// StartStreaming starts a goroutine that continuously reads messages and sends to channel
// Returns a channel that receives messages in real-time
func (m *WSManager) StartStreaming(ctx context.Context) <-chan []byte {
	msgChan := make(chan []byte, 1000) // Increased buffer for bursts

	go func() {
		defer close(msgChan)

		// Panic recovery for streaming goroutine
		defer func() {
			if r := recover(); r != nil {
				// Log but don't crash - just close channel
			}
		}()

		consecutiveErrors := 0
		const maxConsecutiveErrors = 10

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			if !m.connected.Load() {
				// Wait a bit and retry
				select {
				case <-ctx.Done():
					return
				case <-time.After(100 * time.Millisecond):
				}
				continue
			}

			conn := m.getConn()
			if conn == nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(100 * time.Millisecond):
				}
				continue
			}

			// Read with a longer timeout - inactive markets may not send data for a while
			// This is NOT an error condition, just means no trading activity
			readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_, p, err := conn.Read(readCtx)
			cancel()

			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
				}

				// Check if it's just a timeout (normal for inactive markets)
				if readCtx.Err() == context.DeadlineExceeded {
					// Timeout is normal - connection is still alive
					// The heartbeat ping will verify actual connection health
					continue
				}

				// Real error (not timeout) - might need reconnection
				consecutiveErrors++
				if consecutiveErrors >= maxConsecutiveErrors {
					// Too many real errors, force reconnection
					m.connected.Store(false)
					consecutiveErrors = 0
				}

				// Trigger reconnection on real error
				if !m.reconnecting.Load() {
					go m.tryReconnect()
				}
				continue
			}

			// Reset error counter on success
			consecutiveErrors = 0
			m.lastMessage.Store(time.Now().Unix())
			m.messageCount.Add(1)

			// Send to channel (non-blocking)
			select {
			case msgChan <- p:
			default:
				// Channel full, skip (shouldn't happen with buffer)
			}
		}
	}()

	return msgChan
}
