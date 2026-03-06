package api

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	// Heartbeat interval - ping every 10 seconds for aggressive keepalive
	// Prevents connection from going stale on mobile/constrained environments
	heartbeatInterval = 10 * time.Second
	// Max reconnection attempts before giving up (effectively infinite)
	maxReconnectAttempts = 1000000
	// Delay between reconnection attempts (starts at 1s, doubles each attempt)
	reconnectDelay = 1 * time.Second
	// Ping failures before triggering reconnect
	maxPingFailures = 2
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
	pingLatencyNs  atomic.Int64 // Last ping round-trip time in nanoseconds
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
	if m.conn != nil {
		m.mu.Unlock()
		return nil
	}

	// Cancel old context if any
	if m.cancel != nil {
		m.cancel()
	}

	// Store context for reconnection
	m.ctx, m.cancel = context.WithCancel(ctx)
	m.mu.Unlock()

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

	m.mu.Lock()
	m.conn = c
	m.mu.Unlock()

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

func (m *WSManager) getContext() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ctx
}

func (m *WSManager) heartbeatLoop() {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	consecutivePingFailures := 0

	for {
		ctx := m.getContext()
		if ctx == nil {
			return
		}

		select {
		case <-ctx.Done():
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
				// 5s timeout balances Android throttle concerns vs stale-data detection speed
				pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
				pingStart := time.Now()
				
				// Polymarket requires a text frame with exactly "PING" (not a WebSocket control frame)
				err := conn.Write(pingCtx, websocket.MessageText, []byte("PING"))
				
				pingLatency := time.Since(pingStart)
				pingCancel()
				if err != nil {
					consecutivePingFailures++
					m.pingLatencyNs.Store(0)

					// Only reconnect after multiple failures (handles temporary throttling)
					if consecutivePingFailures >= maxPingFailures {
						consecutivePingFailures = 0
						go m.tryReconnect()
					}
					continue
				}
				// Ping succeeded - connection is alive, record latency
				consecutivePingFailures = 0
				m.lastHeartbeat.Store(time.Now().Unix())
				m.pingLatencyNs.Store(pingLatency.Nanoseconds())
				// NOTE: Do NOT update lastMessage here - only actual data messages
				// should update lastMessage. This allows the bot to detect when
				// the connection is alive but no market data is flowing, triggering
				// the REST fallback for fresh prices.
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
	m.pingLatencyNs.Store(0)

	m.mu.Lock()
	ctx := m.ctx
	m.mu.Unlock()

	if ctx == nil {
		return
	}

attemptLoop:
	for attempt := 1; attempt <= maxReconnectAttempts; attempt++ {
		select {
		case <-ctx.Done():
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
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		// Try to reconnect
		err := m.connectInternal(ctx)

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
			subCtx, subCancel := context.WithTimeout(ctx, 5*time.Second)
			err := m.subscribeInternal(subCtx, sub, false)
			subCancel()
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
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	m.connected.Store(false)

	if m.conn != nil {
		return m.conn.Close(websocket.StatusNormalClosure, "")
	}
	return nil
}

func (m *WSManager) Subscribe(ctx context.Context, payload interface{}) error {
	return m.subscribeInternal(ctx, payload, true)
}

func (m *WSManager) subscribeInternal(ctx context.Context, payload interface{}, record bool) error {
	conn := m.getConn()
	if conn == nil {
		return fmt.Errorf("not connected")
	}

	err := wsjson.Write(ctx, conn, payload)
	if err != nil {
		return err
	}

	if record {
		// Store subscription for reconnection
		m.subMu.Lock()
		m.subscriptions = append(m.subscriptions, payload)
		m.subMu.Unlock()
	}

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

// PingLatency returns the last measured ping round-trip time
func (m *WSManager) PingLatency() time.Duration {
	return time.Duration(m.pingLatencyNs.Load())
}

// StartStreaming starts a goroutine that continuously reads messages and sends to channel
// Returns a channel that receives messages in real-time
func (m *WSManager) StartStreaming(ctx context.Context) <-chan []byte {
	msgChan := make(chan []byte, 1000) // Increased buffer for bursts

	go func() {
		defer close(msgChan)

		// Panic recovery for streaming goroutine
		defer func() {
			_ = recover() // Catch panic but do nothing, let channel close naturally
		}()

		consecutiveErrors := 0
		const maxConsecutiveErrors = 10
		lastReconnectAttempt := time.Time{}
		const minReconnectInterval = 5 * time.Second

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			if !m.connected.Load() {
				// Try reconnection if not already attempting and enough time has passed
				if !m.reconnecting.Load() && time.Since(lastReconnectAttempt) > minReconnectInterval {
					lastReconnectAttempt = time.Now()
					go m.tryReconnect()
				}
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

			// Read with shorter timeout for faster stale detection
			// 10 seconds is enough - if no data in 10s, we should check REST
			readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
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
					if time.Since(lastReconnectAttempt) > minReconnectInterval {
						lastReconnectAttempt = time.Now()
						go m.tryReconnect()
					}
				}
				continue
			}

			// Reset error counter on success
			consecutiveErrors = 0
			m.lastMessage.Store(time.Now().Unix())
			m.messageCount.Add(1)

			// Polymarket responds to PING with literal "PONG" strings
			// Filter these out so JSON parsers downstream don't crash
			if string(p) == "PONG" {
				continue
			}

			// Send to channel - prioritize newest message
			// Uses labeled loop for clarity (avoids goto)
		sendLoop:
			for {
				select {
				case msgChan <- p:
					// Successfully sent
					break sendLoop
				default:
					// Channel full - drain oldest message to make room
					select {
					case <-msgChan:
						// Drained, loop will try to send again
					default:
						// Channel was drained by consumer, loop will try to send again
					}
				}
			}
		}
	}()

	return msgChan
}
