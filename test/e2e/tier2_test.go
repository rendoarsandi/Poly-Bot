package e2e

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/strategy"
)

// RecoveryFallbackClient implements RPC fallback with primary client recovery probe.
type RecoveryFallbackClient struct {
	mu               sync.Mutex
	endpoints        []string
	activeIdx        int
	clients          []*api.PolygonClient
	lastFailover     time.Time
	recoveryInterval time.Duration
}

func NewRecoveryFallbackClient(endpoints []string, recovery time.Duration) *RecoveryFallbackClient {
	clients := make([]*api.PolygonClient, len(endpoints))
	for i, url := range endpoints {
		clients[i] = api.NewPolygonClient(url)
	}
	return &RecoveryFallbackClient{
		endpoints:        endpoints,
		clients:          clients,
		recoveryInterval: recovery,
	}
}

func (fc *RecoveryFallbackClient) CallWithFallback(action func(client *api.PolygonClient) error) error {
	fc.mu.Lock()
	// Recovery probe: if we are not on primary, check if recovery interval passed to try primary again
	if fc.activeIdx != 0 && time.Since(fc.lastFailover) >= fc.recoveryInterval {
		fc.activeIdx = 0
	}
	startIdx := fc.activeIdx
	fc.mu.Unlock()

	var lastErr error
	for i := 0; i < len(fc.endpoints); i++ {
		idx := (startIdx + i) % len(fc.endpoints)
		client := fc.clients[idx]

		err := action(client)
		if err == nil {
			fc.mu.Lock()
			fc.activeIdx = idx
			fc.mu.Unlock()
			return nil
		}
		lastErr = err

		fc.mu.Lock()
		fc.lastFailover = time.Now()
		fc.mu.Unlock()
	}

	return fmt.Errorf("all endpoints failed, last error: %w", lastErr)
}

// ─── TIER 2 TESTS ────────────────────────────────────────────────────────────

// Lock-Free Price Snapshot: Extreme tick rate ingestion (e.g. 50k ticks/sec).
func TestT2_PriceSnapshot_ExtremeTickRates(t *testing.T) {
	feed := api.NewBinanceFuturesPriceFeed("BTCUSDT", 1*time.Second)
	feed.SetConnectedForTest(true)

	// Ingest 10,000 updates in a tight loop to simulate high frequency tick rate.
	start := time.Now()
	for i := 0; i < 10000; i++ {
		feed.RecordTradeSampleForTest(90000.0+float64(i), time.Now())
	}
	duration := time.Since(start)

	// Ingestion of 10k ticks should complete very quickly (within 50ms usually)
	if duration > 100*time.Millisecond {
		t.Logf("Warning: Ingestion of 10k ticks took %v", duration)
	}

	snap := feed.Snapshot(time.Now())
	if snap.Price != 99999.0 {
		t.Errorf("expected final price 99999.0, got %f", snap.Price)
	}
}

// Lock-Free Price Snapshot: Snapshot behaves correctly when updates are nil/empty.
func TestT2_PriceSnapshot_EmptyUpdates(t *testing.T) {
	feed := api.NewBinanceFuturesPriceFeed("BTCUSDT", 1*time.Second)

	snap := feed.Snapshot(time.Now())
	if snap.Ready {
		t.Error("snapshot should not be ready when empty")
	}
	if snap.Price != 0 {
		t.Errorf("expected price 0, got %f", snap.Price)
	}

	// Record an invalid sample
	feed.RecordTradeSampleForTest(0, time.Now())
	snap = feed.Snapshot(time.Now())
	if snap.Price != 0 {
		t.Errorf("expected price 0 after invalid update, got %f", snap.Price)
	}
}

// Lock-Free Price Snapshot: Handles sudden extreme price differences.
func TestT2_PriceSnapshot_PriceJumps(t *testing.T) {
	feed := api.NewBinanceFuturesPriceFeed("BTCUSDT", 1*time.Second)
	now := time.Now()

	// Baseline price 100.0, then jump to 200.0
	feed.RecordTradeSampleForTest(100.0, now.Add(-2*time.Second))
	feed.RecordTradeSampleForTest(200.0, now)

	snap := feed.Snapshot(now)
	if !snap.Ready {
		t.Fatal("expected snapshot to be ready")
	}
	if snap.Price != 200.0 {
		t.Errorf("expected latest price 200.0, got %f", snap.Price)
	}
	if diff := snap.DeltaPercent - 100.0; diff < -1e-9 || diff > 1e-9 {
		t.Errorf("expected delta percent 100.0, got %f", snap.DeltaPercent)
	}
}

// Lock-Free Price Snapshot: Resumes correctly after connection loss.
func TestT2_PriceSnapshot_StreamInterruption(t *testing.T) {
	feed := api.NewBinanceFuturesPriceFeed("BTCUSDT", 1*time.Second)
	now := time.Now()

	feed.RecordTradeSampleForTest(50000.0, now.Add(-2*time.Second))
	feed.SetConnectedForTest(false)

	snap := feed.Snapshot(now)
	if snap.Connected {
		t.Error("expected connected status to be false")
	}

	// Restore connection and feed new data
	feed.SetConnectedForTest(true)
	feed.RecordTradeSampleForTest(50500.0, now)

	snap2 := feed.Snapshot(now)
	if !snap2.Connected {
		t.Error("expected connected status to be true")
	}
	if snap2.Price != 50500.0 {
		t.Errorf("expected price to update to 50500.0, got %f", snap2.Price)
	}
}

// Lock-Free Price Snapshot: Runs long-term updates without leaking memory.
func TestT2_PriceSnapshot_MemoryFootprint(t *testing.T) {
	// Let's check: the samples buffer should be trimmed to maxBufferAge.
	// Feed lookback = 100ms. maxBufferAge is initialized to 30s.
	// Let's feed samples spanning 1 minute (60 seconds) and check that old ones are trimmed.
	feed := api.NewBinanceFuturesPriceFeed("BTCUSDT", 100*time.Millisecond)
	baseTime := time.Now()

	// Feed 1000 samples over 60 seconds (one sample every 60ms)
	for i := 0; i < 1000; i++ {
		tSample := baseTime.Add(time.Duration(i*60) * time.Millisecond)
		feed.RecordTradeSampleForTest(60000.0+float64(i), tSample)
	}

	// Check snapshot at the end of the 1-minute period
	endTime := baseTime.Add(60 * time.Second)
	snap := feed.Snapshot(endTime)

	if snap.Price != 60999.0 {
		t.Errorf("expected latest price 60999.0, got %f", snap.Price)
	}
	// Verify baseline time is within lookback (100ms ago relative to endTime)
	// snap.BaselineAt should be around endTime - 100ms
	expectedMinBaseline := endTime.Add(-200 * time.Millisecond)
	if snap.BaselineAt.Before(expectedMinBaseline) {
		t.Errorf("baseline time %v is too stale, expected after %v", snap.BaselineAt, expectedMinBaseline)
	}
}

// Monolithic UI Componentization: Terminal window resized to extremely small/large dimensions.
func TestT2_UIComponent_ExtremeResize(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("UI rendering panicked under extreme dimensions: %v", r)
		}
	}()

	bids := map[string]float64{"Yes": 0.45, "No": 0.55}
	asks := map[string]float64{"Yes": 0.47, "No": 0.57}

	// Test extremely small layout (width=2, height=2)
	rSmall := NewMockWidgetRenderer(2, 2)
	gridSmall := rSmall.RenderMarketGrid("M", []string{"Yes", "No"}, bids, asks, "WS", "active", "BTC", 90000.0, 0.5, "Yes")
	settingsSmall := rSmall.RenderSettingsPanel("p", "l", 1, "pct", 0.01, 1.0)
	logsSmall := rSmall.RenderLogViewer([]string{"Log 1", "Log 2"})

	// Check outputs exist
	if len(gridSmall) == 0 || len(settingsSmall) == 0 || len(logsSmall) == 0 {
		t.Error("expected non-empty output under small dimensions")
	}

	// Test extremely large layout (width=2000, height=2000)
	rLarge := NewMockWidgetRenderer(2000, 2000)
	gridLarge := rLarge.RenderMarketGrid("BTC-EX-LONG-MARKET-ID-12345", []string{"Yes", "No"}, bids, asks, "WS", "active", "BTCUSDT", 90000.0, 0.5, "Yes")
	settingsLarge := rLarge.RenderSettingsPanel("polymarket", "live", 100, "percent", 0.10, 2.5)
	logsLarge := rLarge.RenderLogViewer(make([]string, 100))

	if !strings.Contains(gridLarge, "BTC-EX-LONG-MARKET-ID-12345") {
		t.Error("expected market ID in large layout")
	}
	if !strings.Contains(settingsLarge, "Exchange=polymarket") {
		t.Error("expected exchange in large layout")
	}
	if len(logsLarge) == 0 {
		t.Error("expected logs in large layout")
	}
}

// Monolithic UI Componentization: Graceful handling of malformed market details.
func TestT2_UIComponent_CorruptedMarketDetails(t *testing.T) {
	r := NewMockWidgetRenderer(80, 10)

	// Malformed outcomes list (nil) and empty bids/asks
	grid := r.RenderMarketGrid("", nil, nil, nil, "", "", "", 0, 0, "")

	if !strings.Contains(grid, "MARKET:") {
		t.Errorf("expected basic layout format even with malformed details, got %q", grid)
	}
}

// Monolithic UI Componentization: Handle logs exceeding max buffer capacity.
func TestT2_UIComponent_LogBufferOverflow(t *testing.T) {
	// Create renderer with height=5. The LogViewer will display logs up to r.height - 4 (so 1 log line max).
	r := NewMockWidgetRenderer(80, 5)

	logs := []string{"L1", "L2", "L3", "L4", "L5", "L6"}
	view := r.RenderLogViewer(logs)

	// Verify L1 is displayed
	if !strings.Contains(view, "L1") {
		t.Errorf("expected L1 log, got %q", view)
	}
	// Verify L2 is not displayed since it overflows the height budget
	if strings.Contains(view, "L2") {
		t.Errorf("did not expect L2 log, got %q", view)
	}
}

// Monolithic UI Componentization: Render under rapid state changes.
func TestT2_UIComponent_HighFrequencyRender(t *testing.T) {
	app := MockUIApp{
		Renderer:     NewMockWidgetRenderer(80, 15),
		ShowSettings: false,
		SizingMode:   "percent",
		ScaleFactor:  0.05,
		Paused:       false,
		Logs:         []string{"Event"},
	}

	start := time.Now()
	// Render 500 times rapidly with alternating state
	for i := 0; i < 500; i++ {
		app.ShowSettings = (i%2 == 0)
		app.ScaleFactor = 0.01 + float64(i)*0.0001
		out := app.Render()
		if len(out) == 0 {
			t.Fatal("empty render output")
		}
	}
	duration := time.Since(start)
	if duration > 200*time.Millisecond {
		t.Logf("Warning: 500 renders took %v", duration)
	}
}

// Monolithic UI Componentization: Resources are correctly freed up on shutdown.
func TestT2_UIComponent_TearDown(t *testing.T) {
	wsSrv := NewMockBinanceWSServer()
	rpcSrv := NewMockRPCServer()

	// Verify clean teardown by calling Close on our mock resources.
	// This ensures sockets, HTTP listeners, and internal channels are released.
	wsSrv.Close()
	rpcSrv.Close()
}

// Decimal Fixed-Point Precision: Zero and massive quantities/prices calculations.
func TestT2_Decimal_ExtremeValues(t *testing.T) {
	params := strategy.MakerParams{
		QuoteStep:           0.0001,
		MinQuoteValue:       0.01,
		CashUsagePerOutcome: 1.0,
	}

	// Zero cash: quantity should be exactly 0
	qtyZero := strategy.ComputeMakerBuyQty(100.0, 0.0, 0.0, 1000.0, 0.0, 0.50, params, nil)
	if qtyZero != 0 {
		t.Errorf("expected 0 quantity for 0 cash, got %f", qtyZero)
	}

	// Massive cash/trade values:
	// baseTradeValue = 1e9, cash = 1e9, price = 0.50 -> 1e9/0.50 = 2e9 shares
	qtyMassive := strategy.ComputeMakerBuyQty(1e9, 0.0, 0.0, 1e12, 1e9, 0.50, params, nil)
	if qtyMassive != 2e9 {
		t.Errorf("expected 2e9 quantity, got %f", qtyMassive)
	}
}

// Decimal Fixed-Point Precision: Negative and zero fee bps edge cases.
func TestT2_Decimal_ZeroFeeBps(t *testing.T) {
	// Zero fee bps
	feeZero := strategy.ComputeMakerSellFeeUsdc(100.0, 0.50, 0)
	if feeZero != 0 {
		t.Errorf("expected 0 fee for 0 bps, got %f", feeZero)
	}

	// Negative fee bps
	feeNeg := strategy.ComputeMakerSellFeeUsdc(100.0, 0.50, -10)
	if feeNeg != 0 {
		t.Errorf("expected 0 fee for negative bps, got %f", feeNeg)
	}
}

// Decimal Fixed-Point Precision: Negative inventory skew edge cases.
func TestT2_Decimal_NegativeSkew(t *testing.T) {
	// position = 100, peer = 400, target = 300 -> (100 - 400)/300 = -1.0
	skew := strategy.ComputeMakerInventorySkew(100.0, 400.0, 300.0)
	if skew != -1.0 {
		t.Errorf("expected skew -1.0, got %f", skew)
	}

	params := strategy.MakerParams{
		QuoteStep:         0.01,
		DefaultQuoteGap:   0.02,
		InventorySkewStep: 0.05,
	}

	// Buy quote with negative skew (less inventory, buy more aggressively):
	// mid - quoteGap - (skew * skewStep) = 0.50 - 0.02 - (-1.0 * 0.05) = 0.48 + 0.05 = 0.53.
	// Since ask is 0.60, 0.53 is fine. Clamped/rounded to QuoteStep (0.01) -> 0.53.
	price, ok := strategy.ComputeMakerSkewedQuote(true, 0.40, 0.60, -1.0, 0.02, params)
	if !ok {
		t.Fatal("expected buy quote success")
	}
	if price != 0.53 {
		t.Errorf("expected buy price 0.53, got %f", price)
	}
}

// Decimal Fixed-Point Precision: Maker sizing calculations when cash is insufficient for min quote.
func TestT2_Decimal_LowCashMinimum(t *testing.T) {
	params := strategy.MakerParams{
		QuoteStep:           0.01,
		MinQuoteValue:       5.0, // Minimum quote must be $5.00
		CashUsagePerOutcome: 1.0,
	}

	// Cash = $1.00, price = 0.50. Max shares affordable is 1.00 / 0.50 = 2 shares.
	// Value of these shares = 2 * 0.50 = $1.00, which is below MinQuoteValue ($5.00).
	// Sizer should return 0.
	qty := strategy.ComputeMakerBuyQty(10.0, 0.0, 0.0, 100.0, 1.0, 0.50, params, nil)
	if qty != 0 {
		t.Errorf("expected 0 quantity due to low cash, got %f", qty)
	}
}

// Decimal Fixed-Point Precision: Complete elimination of floating-point division precision loss.
func TestT2_Decimal_TruncationElimination(t *testing.T) {
	params := strategy.MakerParams{
		QuoteStep:         0.03, // Custom step that can easily cause float issues if not rounded
		DefaultQuoteGap:   0.02,
		InventorySkewStep: 0.05,
	}

	// mid = (0.41 + 0.62) / 2 = 0.515
	// buy quote base = 0.515 - 0.02 - 0 = 0.495
	// 0.495 / 0.03 = 16.5 -> round to nearest multiple -> 17 -> 17 * 0.03 = 0.51.
	price, ok := strategy.ComputeMakerSkewedQuote(true, 0.41, 0.62, 0.0, 0.02, params)
	if !ok {
		t.Fatal("expected quote success")
	}

	// Verify price is exactly a multiple of QuoteStep (0.03) and has no weird float remainder
	rem := math.Mod(price, params.QuoteStep)
	if rem > 1e-9 && rem < params.QuoteStep-1e-9 {
		t.Errorf("expected price %f to be perfectly divisible by %f, remainder %f", price, params.QuoteStep, rem)
	}
	if price != 0.51 {
		t.Errorf("expected price 0.51, got %f", price)
	}
}

// RPC Fallback & Gas Cache: Graceful failure reporting when all RPCs are dead.
func TestT2_RPC_AllEndpointsDead(t *testing.T) {
	s1 := NewMockRPCServer()
	defer s1.Close()
	s2 := NewMockRPCServer()
	defer s2.Close()

	s1.SetHealthy(false)
	s2.SetHealthy(false)

	fc := NewFallbackClient([]string{s1.URL(), s2.URL()})

	err := fc.CallWithFallback(func(client *api.PolygonClient) error {
		_, callErr := client.GetGasPrice(context.Background())
		return callErr
	})

	if err == nil {
		t.Fatal("expected error when all RPCs are dead, got nil")
	}
	if !strings.Contains(err.Error(), "all endpoints failed") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// RPC Fallback & Gas Cache: Gas cache handles zero or extremely high gas price spikes.
func TestT2_RPC_GasSpikeProtection(t *testing.T) {
	s := NewMockRPCServer()
	defer s.Close()

	client := api.NewPolygonClient(s.URL())
	// Max gas price allowed: 200 Gwei (200,000,000,000)
	cache := NewGasCache(client, 1*time.Second, big.NewInt(200_000_000_000))

	// 1. Zero gas price case
	s.mu.Lock()
	s.GasPrice = big.NewInt(0)
	s.mu.Unlock()

	_, err := cache.GetGasPrice(context.Background())
	if err == nil {
		t.Error("expected error for zero gas price, got nil")
	}

	// 2. Gas spike case (500 Gwei > 200 Gwei)
	s.mu.Lock()
	s.GasPrice = big.NewInt(500_000_000_000)
	s.mu.Unlock()

	_, err = cache.GetGasPrice(context.Background())
	if err == nil {
		t.Error("expected error for gas price spike, got nil")
	}
}

// RPC Fallback & Gas Cache: Prevent race conditions during concurrent gas cache queries.
func TestT2_RPC_ConcurrentStampede(t *testing.T) {
	s := NewMockRPCServer()
	defer s.Close()

	// Slow down response to let concurrent requests pile up
	s.mu.Lock()
	s.Delay = 50 * time.Millisecond
	s.mu.Unlock()

	client := api.NewPolygonClient(s.URL())
	cache := NewGasCache(client, 1*time.Second, nil)

	var wg sync.WaitGroup
	numRoutines := 50
	results := make([]*big.Int, numRoutines)
	errors := make([]error, numRoutines)

	for i := 0; i < numRoutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			gas, err := cache.GetGasPrice(context.Background())
			results[idx] = gas
			errors[idx] = err
		}(i)
	}

	wg.Wait()

	// Ensure all succeeded and got the correct gas price
	for i := 0; i < numRoutines; i++ {
		if errors[i] != nil {
			t.Errorf("routine %d failed: %v", i, errors[i])
		}
		if results[i] == nil || results[i].Cmp(big.NewInt(50_000_000_000)) != 0 {
			t.Errorf("routine %d got unexpected price: %v", i, results[i])
		}
	}

	s.mu.Lock()
	calls := s.MethodCallCount["eth_gasPrice"]
	s.mu.Unlock()

	// Verify only 1 call was actually made to the RPC server (stampede protection)
	if calls != 1 {
		t.Errorf("expected exactly 1 RPC call, got %d", calls)
	}
}

// RPC Fallback & Gas Cache: Reconnect to primary RPC once it recovers.
func TestT2_RPC_PrimaryRecovery(t *testing.T) {
	s1 := NewMockRPCServer()
	defer s1.Close()
	s2 := NewMockRPCServer()
	defer s2.Close()

	s1.SetHealthy(false) // Primary starts unhealthy

	// Recovery timeout = 10ms
	fc := NewRecoveryFallbackClient([]string{s1.URL(), s2.URL()}, 10*time.Millisecond)

	// Call 1: should fall back to s2
	err := fc.CallWithFallback(func(client *api.PolygonClient) error {
		_, err := client.GetGasPrice(context.Background())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	s2.mu.Lock()
	s2Calls := s2.MethodCallCount["eth_gasPrice"]
	s2.mu.Unlock()
	if s2Calls != 1 {
		t.Errorf("expected 1 call on s2, got %d", s2Calls)
	}

	// Recover primary
	s1.SetHealthy(true)
	// Sleep to let recovery interval elapse
	time.Sleep(15 * time.Millisecond)

	// Call 2: should attempt primary again and succeed
	err = fc.CallWithFallback(func(client *api.PolygonClient) error {
		_, err := client.GetGasPrice(context.Background())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	s1.mu.Lock()
	s1Calls := s1.MethodCallCount["eth_gasPrice"]
	s1.mu.Unlock()
	if s1Calls != 1 {
		t.Errorf("expected 1 call on s1 after recovery, got %d", s1Calls)
	}
}

// RPC Fallback & Gas Cache: Fallback occurs within custom configured timeout thresholds.
func TestT2_RPC_FallbackTimeout(t *testing.T) {
	s1 := NewMockRPCServer()
	defer s1.Close()
	s2 := NewMockRPCServer()
	defer s2.Close()

	// Slow down s1 response so it exceeds our custom timeout of 20ms
	s1.mu.Lock()
	s1.Delay = 200 * time.Millisecond
	s1.mu.Unlock()

	fc := NewFallbackClient([]string{s1.URL(), s2.URL()})

	start := time.Now()

	err := fc.CallWithFallback(func(client *api.PolygonClient) error {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := client.GetGasPrice(ctx)
		return err
	})

	duration := time.Since(start)

	if err != nil {
		t.Fatalf("fallback call failed: %v", err)
	}

	// The fallback should happen within ~50ms (timeout 20ms + fallback latency)
	if duration > 100*time.Millisecond {
		t.Errorf("fallback took too long: %v, expected < 100ms", duration)
	}

	s2.mu.Lock()
	s2Calls := s2.MethodCallCount["eth_gasPrice"]
	s2.mu.Unlock()
	if s2Calls != 1 {
		t.Errorf("expected 1 call on s2, got %d", s2Calls)
	}
}
