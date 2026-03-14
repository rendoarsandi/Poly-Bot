package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
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

var ErrWSConnectionUnhealthy = errors.New("websocket connection unhealthy")

type WSManager struct {
	URL          string
	Exchange     string
	kalshiSigner *KalshiSigner
	conn         *websocket.Conn
	mu           sync.Mutex

	// Connection state
	connected       atomic.Bool
	lastMessage     atomic.Int64 // Unix timestamp of last message, including PONG heartbeats
	lastDataMessage atomic.Int64 // Unix timestamp (ns) of last non-heartbeat data message
	lastHeartbeat   atomic.Int64

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
	pingLatencyNs  atomic.Int64 // Last PING -> PONG round-trip time in nanoseconds
	lastPingSentNs atomic.Int64
}

func NewWSManager(exchange, kalshiKey, kalshiPK, customURL string) *WSManager {
	url := "wss://ws-subscriptions-clob.polymarket.com/ws/market"
	var kalshiSigner *KalshiSigner
	if exchange == "kalshi" {
		url = "wss://api.elections.kalshi.com/trade-api/ws/v2"
		if kalshiKey != "" && kalshiPK != "" {
			kalshiSigner, _ = NewKalshiSigner(kalshiKey, kalshiPK)
		}
	}
	
	if customURL != "" {
		url = customURL
	}

	return &WSManager{
		URL:           url,
		Exchange:      exchange,
		kalshiSigner:  kalshiSigner,
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

	if m.Exchange == "kalshi" && m.kalshiSigner != nil {
		timestamp, signature, err := m.kalshiSigner.SignRequest("GET", "/trade-api/ws/v2")
		if err != nil {
			return fmt.Errorf("failed to sign kalshi ws request: %w", err)
		}
		opts.HTTPHeader = make(http.Header)
		opts.HTTPHeader.Set("KALSHI-ACCESS-KEY", m.kalshiSigner.AccessKey)
		opts.HTTPHeader.Set("KALSHI-ACCESS-SIGNATURE", signature)
		opts.HTTPHeader.Set("KALSHI-ACCESS-TIMESTAMP", timestamp)
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
	m.lastPingSentNs.Store(0)

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
				// Check for stale connection BEFORE sending the next ping
				// 3 * heartbeatInterval gives it ~30 seconds to receive any data or a PONG
				lastMsg := time.Unix(m.lastMessage.Load(), 0)
				if time.Since(lastMsg) > 3*heartbeatInterval {
					m.handleConnectionFailure(conn, websocket.StatusGoingAway, "stale connection (no messages or PONG)")
					go m.tryReconnect()
					continue
				}

				// Send ping outside of lock to prevent blocking
				// 5s timeout balances Android throttle concerns vs stale-data detection speed
				pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
				pingStart := time.Now()

				// Polymarket requires a text frame with exactly "PING" (not a WebSocket control frame)
				err := conn.Write(pingCtx, websocket.MessageText, []byte("PING"))
				pingCancel()
				if err != nil {
					consecutivePingFailures++
					m.pingLatencyNs.Store(0)
					m.lastPingSentNs.Store(0)

					// Only reconnect after multiple failures (handles temporary throttling)
					if consecutivePingFailures >= maxPingFailures {
						consecutivePingFailures = 0
						go m.tryReconnect()
					}
					continue
				}
				// Ping succeeded - connection is alive, record latency
				consecutivePingFailures = 0
				m.lastPingSentNs.Store(pingStart.UnixNano())
				// NOTE: Do NOT update lastMessage here - only actual data messages
				// or PONG heartbeats should update lastMessage. This allows the bot to detect when
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

func (m *WSManager) handleConnectionFailure(failedConn *websocket.Conn, code websocket.StatusCode, reason string) {
	m.connected.Store(false)
	m.pingLatencyNs.Store(0)
	m.lastPingSentNs.Store(0)

	var connToClose *websocket.Conn
	m.mu.Lock()
	switch {
	case failedConn == nil:
		connToClose = m.conn
		m.conn = nil
	case m.conn == failedConn:
		connToClose = m.conn
		m.conn = nil
	}
	m.mu.Unlock()

	if connToClose != nil {
		_ = connToClose.Close(code, reason)
	}
}

func (m *WSManager) Close() error {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()

	m.connected.Store(false)
	m.pingLatencyNs.Store(0)
	m.lastPingSentNs.Store(0)

	var connToClose *websocket.Conn
	m.mu.Lock()
	connToClose = m.conn
	m.conn = nil
	m.mu.Unlock()

	if connToClose == nil {
		return nil
	}
	return connToClose.Close(websocket.StatusNormalClosure, "")
}

func (m *WSManager) Subscribe(ctx context.Context, payload interface{}) error {
	return m.subscribeInternal(ctx, payload, true)
}

func (m *WSManager) subscribeInternal(ctx context.Context, payload interface{}, record bool) error {
	conn := m.getConn()
	if conn == nil {
		return fmt.Errorf("%w: not connected", ErrWSConnectionUnhealthy)
	}

	err := wsjson.Write(ctx, conn, payload)
	if err != nil {
		m.handleConnectionFailure(conn, websocket.StatusGoingAway, "write failed")
		return fmt.Errorf("%w: %v", ErrWSConnectionUnhealthy, err)
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
		return nil, fmt.Errorf("%w: not connected", ErrWSConnectionUnhealthy)
	}

	for {
		_, p, err := conn.Read(ctx)
		if err != nil {
			m.handleConnectionFailure(conn, websocket.StatusGoingAway, "read failed")
			// Trigger reconnection on read error
			go m.tryReconnect()
			return nil, err
		}

		msg, heartbeat := m.processInboundMessage(p)
		if heartbeat {
			continue
		}
		return msg, nil
	}
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

	for {
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
			m.handleConnectionFailure(conn, websocket.StatusGoingAway, "read timeout loop failed")
			// On other errors, trigger reconnection
			go m.tryReconnect()
			return nil, err
		}

		msg, heartbeat := m.processInboundMessage(p)
		if heartbeat {
			continue
		}
		return msg, nil
	}
}

// IsConnected returns current connection status
func (m *WSManager) IsConnected() bool {
	return m.connected.Load()
}

// ForceReconnect triggers a reconnection attempt from external code
func (m *WSManager) ForceReconnect() {
	m.handleConnectionFailure(nil, websocket.StatusGoingAway, "force reconnect")
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

// TimeSinceLastDataMessage returns duration since the last non-heartbeat data message.
func (m *WSManager) TimeSinceLastDataMessage() time.Duration {
	last := m.lastDataMessage.Load()
	if last == 0 {
		return time.Duration(1<<63 - 1)
	}
	return time.Since(time.Unix(0, last))
}

// PingLatency returns the last measured PING -> PONG heartbeat round-trip latency.
func (m *WSManager) PingLatency() time.Duration {
	return time.Duration(m.pingLatencyNs.Load())
}

func (m *WSManager) processInboundMessage(p []byte) ([]byte, bool) {
	now := time.Now()
	m.lastMessage.Store(now.Unix())
	m.messageCount.Add(1)

	if bytes.Equal(bytes.TrimSpace(p), []byte("PONG")) {
		m.lastHeartbeat.Store(now.Unix())
		if pingSentNs := m.lastPingSentNs.Swap(0); pingSentNs > 0 {
			m.pingLatencyNs.Store(now.Sub(time.Unix(0, pingSentNs)).Nanoseconds())
		}
		return nil, true
	}

	m.lastDataMessage.Store(now.UnixNano())
	return p, false
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

			// Polymarket responds to PING with literal "PONG" strings.
			// Consume them here so downstream JSON parsers only see data events.
			msg, heartbeat := m.processInboundMessage(p)
			if heartbeat {
				continue
			}

			// Send to channel - prioritize newest message
			// Uses labeled loop for clarity (avoids goto)
		sendLoop:
			for {
				select {
				case msgChan <- msg:
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
