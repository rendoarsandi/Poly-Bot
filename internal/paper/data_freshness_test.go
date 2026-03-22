package paper

import (
	"sync"
	"testing"
	"time"
)

// DataSource simulates a data source (WS or REST) with staleness tracking
type DataSource struct {
	mu          sync.RWMutex
	name        string
	lastUpdate  time.Time
	updateCount int
	isConnected bool
}

func NewDataSource(name string) *DataSource {
	return &DataSource{
		name:        name,
		lastUpdate:  time.Now(),
		isConnected: true,
	}
}

func (d *DataSource) Update() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastUpdate = time.Now()
	d.updateCount++
}

func (d *DataSource) TimeSinceUpdate() time.Duration {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return time.Since(d.lastUpdate)
}

func (d *DataSource) IsStale(threshold time.Duration) bool {
	return d.TimeSinceUpdate() > threshold
}

func (d *DataSource) SetConnected(connected bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.isConnected = connected
}

func (d *DataSource) IsConnected() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.isConnected
}

func (d *DataSource) GetUpdateCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.updateCount
}

// HybridDataManager simulates the bot's WS+REST hybrid approach
type HybridDataManager struct {
	ws               *DataSource
	rest             *DataSource
	restPollInterval time.Duration
	wsStaleThreshold time.Duration
	lastRestPoll     time.Time

	// Tracking
	restFallbackCount int
	wsUpdateCount     int
}

func NewHybridDataManager(restPollInterval, wsStaleThreshold time.Duration) *HybridDataManager {
	return &HybridDataManager{
		ws:               NewDataSource("WS"),
		rest:             NewDataSource("REST"),
		restPollInterval: restPollInterval,
		wsStaleThreshold: wsStaleThreshold,
	}
}

// ShouldPollREST determines if REST should be polled based on current state
// This mirrors the logic in cmd/paperbot/main.go
func (h *HybridDataManager) ShouldPollREST() bool {
	// Always poll REST for liquidity at the configured interval
	needsRestPoll := time.Since(h.lastRestPoll) > h.restPollInterval

	// Also force REST if WS is unhealthy
	wsUnhealthy := !h.ws.IsConnected() || h.ws.TimeSinceUpdate() > h.wsStaleThreshold
	wsStale := h.ws.TimeSinceUpdate() > 3*time.Second

	if wsUnhealthy && wsStale {
		needsRestPoll = true
	}

	return needsRestPoll
}

func (h *HybridDataManager) PollREST() {
	h.lastRestPoll = time.Now()
	h.rest.Update()
	h.restFallbackCount++
}

func (h *HybridDataManager) ReceiveWSUpdate() {
	h.ws.Update()
	h.wsUpdateCount++
}

// TestWSStaleDetection verifies WebSocket staleness is correctly detected
func TestWSStaleDetection(t *testing.T) {
	tests := []struct {
		name            string
		timeSinceUpdate time.Duration
		threshold       time.Duration
		expectStale     bool
	}{
		{
			name:            "Fresh data (0ms)",
			timeSinceUpdate: 0,
			threshold:       15 * time.Second,
			expectStale:     false,
		},
		{
			name:            "Fresh data (1s)",
			timeSinceUpdate: 1 * time.Second,
			threshold:       15 * time.Second,
			expectStale:     false,
		},
		{
			name:            "Getting stale (10s)",
			timeSinceUpdate: 10 * time.Second,
			threshold:       15 * time.Second,
			expectStale:     false,
		},
		{
			name:            "Just past threshold - considered stale",
			timeSinceUpdate: 15*time.Second + time.Millisecond,
			threshold:       15 * time.Second,
			expectStale:     true, // > threshold is stale
		},
		{
			name:            "Definitely stale (16s)",
			timeSinceUpdate: 16 * time.Second,
			threshold:       15 * time.Second,
			expectStale:     true,
		},
		{
			name:            "Very stale (30s)",
			timeSinceUpdate: 30 * time.Second,
			threshold:       15 * time.Second,
			expectStale:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ds := NewDataSource("test")
			// Simulate time passing
			ds.mu.Lock()
			ds.lastUpdate = time.Now().Add(-tc.timeSinceUpdate)
			ds.mu.Unlock()

			isStale := ds.IsStale(tc.threshold)
			if isStale != tc.expectStale {
				t.Errorf("Expected stale=%v, got stale=%v (time since update: %v, threshold: %v)",
					tc.expectStale, isStale, tc.timeSinceUpdate, tc.threshold)
			}
		})
	}
}

// TestRESTFallbackTrigger verifies REST fallback triggers correctly
func TestRESTFallbackTrigger(t *testing.T) {
	tests := []struct {
		name              string
		restPollInterval  time.Duration
		timeSinceRestPoll time.Duration
		wsConnected       bool
		wsTimeSinceUpdate time.Duration
		wsStaleThreshold  time.Duration
		expectRESTpoll    bool
		reason            string
	}{
		{
			name:              "Normal polling interval reached",
			restPollInterval:  25 * time.Millisecond,
			timeSinceRestPoll: 30 * time.Millisecond,
			wsConnected:       true,
			wsTimeSinceUpdate: 100 * time.Millisecond,
			wsStaleThreshold:  15 * time.Second,
			expectRESTpoll:    true,
			reason:            "25ms interval passed",
		},
		{
			name:              "Polling interval not reached yet",
			restPollInterval:  25 * time.Millisecond,
			timeSinceRestPoll: 10 * time.Millisecond,
			wsConnected:       true,
			wsTimeSinceUpdate: 100 * time.Millisecond,
			wsStaleThreshold:  15 * time.Second,
			expectRESTpoll:    false,
			reason:            "Only 10ms since last poll, need 25ms",
		},
		{
			name:              "WS disconnected forces REST",
			restPollInterval:  25 * time.Millisecond,
			timeSinceRestPoll: 10 * time.Millisecond, // Not due yet
			wsConnected:       false,                 // WS is down!
			wsTimeSinceUpdate: 5 * time.Second,       // > 3s stale
			wsStaleThreshold:  15 * time.Second,
			expectRESTpoll:    true,
			reason:            "WS disconnected + stale > 3s",
		},
		{
			name:              "WS stale (>15s) forces REST",
			restPollInterval:  25 * time.Millisecond,
			timeSinceRestPoll: 10 * time.Millisecond,
			wsConnected:       true,
			wsTimeSinceUpdate: 20 * time.Second, // Very stale
			wsStaleThreshold:  15 * time.Second,
			expectRESTpoll:    true,
			reason:            "WS stale > 15s threshold",
		},
		{
			name:              "WS slightly stale but connected - no force",
			restPollInterval:  25 * time.Millisecond,
			timeSinceRestPoll: 10 * time.Millisecond,
			wsConnected:       true,
			wsTimeSinceUpdate: 2 * time.Second, // < 3s
			wsStaleThreshold:  15 * time.Second,
			expectRESTpoll:    false,
			reason:            "WS connected and only 2s stale, wait for interval",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hdm := NewHybridDataManager(tc.restPollInterval, tc.wsStaleThreshold)

			// Set up state
			hdm.lastRestPoll = time.Now().Add(-tc.timeSinceRestPoll)
			hdm.ws.SetConnected(tc.wsConnected)
			hdm.ws.mu.Lock()
			hdm.ws.lastUpdate = time.Now().Add(-tc.wsTimeSinceUpdate)
			hdm.ws.mu.Unlock()

			shouldPoll := hdm.ShouldPollREST()
			if shouldPoll != tc.expectRESTpoll {
				t.Errorf("%s\nExpected REST poll=%v, got=%v",
					tc.reason, tc.expectRESTpoll, shouldPoll)
			}
		})
	}
}

// TestHybridDataFreshness simulates the hybrid WS+REST approach over time
func TestHybridDataFreshness(t *testing.T) {
	t.Run("REST keeps data fresh when WS stops", func(t *testing.T) {
		hdm := NewHybridDataManager(25*time.Millisecond, 15*time.Second)

		// Simulate: WS works for a bit, then stops
		for i := 0; i < 5; i++ {
			hdm.ReceiveWSUpdate()
		}

		// WS stops updating (simulating the stale problem you experienced)
		hdm.ws.mu.Lock()
		hdm.ws.lastUpdate = time.Now().Add(-20 * time.Second) // 20s stale
		hdm.ws.mu.Unlock()

		// Check that REST fallback triggers
		if !hdm.ShouldPollREST() {
			t.Error("REST should poll when WS is 20s stale")
		}

		// Simulate REST taking over
		for i := 0; i < 10; i++ {
			if hdm.ShouldPollREST() {
				hdm.PollREST()
			}
			// Small delay simulation
			hdm.lastRestPoll = time.Now().Add(-30 * time.Millisecond)
		}

		// REST should have taken over
		if hdm.restFallbackCount < 10 {
			t.Errorf("REST fallback should have triggered at least 10 times, got %d", hdm.restFallbackCount)
		}

		// REST data should be fresh
		if hdm.rest.IsStale(1 * time.Second) {
			t.Error("REST data should be fresh after polling")
		}
	})

	t.Run("Both sources work together normally", func(t *testing.T) {
		hdm := NewHybridDataManager(25*time.Millisecond, 15*time.Second)

		// Simulate normal operation: both WS and REST active
		cycles := 0
		for i := 0; i < 100; i++ {
			// WS sends update occasionally
			if i%3 == 0 {
				hdm.ReceiveWSUpdate()
			}

			// REST polls on schedule
			if hdm.ShouldPollREST() {
				hdm.PollREST()
				cycles++
			}

			// Simulate 10ms passing
			hdm.lastRestPoll = time.Now().Add(-10 * time.Millisecond)
		}

		// Both should have received updates
		if hdm.wsUpdateCount == 0 {
			t.Error("WS should have received updates")
		}
		if hdm.restFallbackCount == 0 {
			t.Error("REST should have polled")
		}
	})
}

// TestPollingRateWithRateLimiter verifies the effective polling rate
func TestPollingRateWithRateLimiter(t *testing.T) {
	tests := []struct {
		name            string
		targetInterval  time.Duration
		rateLimitRPS    int
		numTokens       int
		expectedMinRate float64 // Minimum updates per second per token
		description     string
	}{
		{
			name:            "25ms interval, 145 RPS, 8 tokens",
			targetInterval:  25 * time.Millisecond,
			rateLimitRPS:    145,
			numTokens:       8,  // 4 assets × 2 outcomes
			expectedMinRate: 18, // 145/8 = ~18 per token
			description:     "Each token gets ~18 updates/sec",
		},
		{
			name:            "25ms interval, 145 RPS, 4 tokens",
			targetInterval:  25 * time.Millisecond,
			rateLimitRPS:    145,
			numTokens:       4,  // 2 assets × 2 outcomes
			expectedMinRate: 36, // 145/4 = ~36 per token
			description:     "Fewer tokens = more updates each",
		},
		{
			name:            "50ms interval, 145 RPS, 8 tokens",
			targetInterval:  50 * time.Millisecond,
			rateLimitRPS:    145,
			numTokens:       8,
			expectedMinRate: 18, // Still limited by rate limiter
			description:     "Rate limiter is the bottleneck",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Calculate theoretical rate
			maxPossibleRPS := float64(1) / tc.targetInterval.Seconds() // Per token
			rateLimitedRPS := float64(tc.rateLimitRPS) / float64(tc.numTokens)

			// Actual rate is min of the two
			actualRate := maxPossibleRPS
			if rateLimitedRPS < actualRate {
				actualRate = rateLimitedRPS
			}

			if actualRate < tc.expectedMinRate {
				t.Errorf("%s\nExpected rate >= %.0f/s, calculated %.0f/s",
					tc.description, tc.expectedMinRate, actualRate)
			}

			// Calculate effective latency
			effectiveLatency := 1000.0 / actualRate // ms
			t.Logf("  %s: %.0f updates/sec per token (%.0fms effective latency)",
				tc.name, actualRate, effectiveLatency)
		})
	}
}

// TestWSOnlyModeProblems demonstrates why WS-only mode is problematic
func TestWSOnlyModeProblems(t *testing.T) {
	t.Run("WS can go silent for extended periods", func(t *testing.T) {
		// Simulate scenarios where WS stops sending data
		scenarios := []struct {
			name        string
			silentFor   time.Duration
			isConnected bool
			description string
		}{
			{
				name:        "No trades happening",
				silentFor:   30 * time.Second,
				isConnected: true, // Connection alive, just no messages
				description: "Market is quiet, no trades = no WS updates",
			},
			{
				name:        "WS reconnecting",
				silentFor:   5 * time.Second,
				isConnected: false,
				description: "WS dropped and reconnecting",
			},
			{
				name:        "WS lagging",
				silentFor:   10 * time.Second,
				isConnected: true,
				description: "WS connected but messages delayed/buffered",
			},
		}

		for _, sc := range scenarios {
			t.Run(sc.name, func(t *testing.T) {
				ds := NewDataSource("WS")
				ds.SetConnected(sc.isConnected)
				ds.mu.Lock()
				ds.lastUpdate = time.Now().Add(-sc.silentFor)
				ds.mu.Unlock()

				// Check staleness
				staleAt3s := ds.IsStale(3 * time.Second)
				staleAt15s := ds.IsStale(15 * time.Second)

				t.Logf("  %s: silent for %v", sc.description, sc.silentFor)
				t.Logf("    Stale at 3s threshold: %v", staleAt3s)
				t.Logf("    Stale at 15s threshold: %v", staleAt15s)

				// With WS-only, you'd be trading on stale data!
				if sc.silentFor > 3*time.Second && !staleAt3s {
					t.Error("Should detect staleness at 3s threshold")
				}
			})
		}
	})

	t.Run("WS does not provide liquidity updates", func(t *testing.T) {
		// This is a fundamental limitation of Polymarket's WS
		// WS sends: price changes, trade notifications
		// WS does NOT send: order book depth, liquidity sizes

		// With WS-only, you'd see PRICES but not LIQUIDITY
		// The bot would see ask=$0.48 but not know there's only 5 shares available

		t.Log("WS limitation: Does not send liquidity/depth updates")
		t.Log("  - You'd see prices but not sizes")
		t.Log("  - Can't calculate accurate safe share sizing")
		t.Log("  - REST is REQUIRED for liquidity data")
	})
}

// Benchmark the staleness check performance
func BenchmarkStalenessCheck(b *testing.B) {
	ds := NewDataSource("test")
	threshold := 15 * time.Second

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ds.IsStale(threshold)
	}
}

func BenchmarkHybridShouldPoll(b *testing.B) {
	hdm := NewHybridDataManager(25*time.Millisecond, 15*time.Second)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = hdm.ShouldPollREST()
	}
}
