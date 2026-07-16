package e2e

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/strategy"
)

// GasCache implements caching of gas values with TTL, spike protection, and stampede prevention.
type GasCache struct {
	mu          sync.Mutex
	client      *api.PolygonClient
	ttl         time.Duration
	cachedGas   *big.Int
	cachedTime  time.Time
	lastError   error
	fetchCount  int
	fetching    bool
	waiters     []chan struct{}
	maxGasPrice *big.Int
}

func NewGasCache(client *api.PolygonClient, ttl time.Duration, maxGasPrice *big.Int) *GasCache {
	return &GasCache{
		client:      client,
		ttl:         ttl,
		maxGasPrice: maxGasPrice,
	}
}

func (c *GasCache) GetGasPrice(ctx context.Context) (*big.Int, error) {
	c.mu.Lock()
	now := time.Now()
	if c.cachedGas != nil && now.Sub(c.cachedTime) < c.ttl && c.lastError == nil {
		defer c.mu.Unlock()
		return new(big.Int).Set(c.cachedGas), nil
	}

	if c.fetching {
		ch := make(chan struct{})
		c.waiters = append(c.waiters, ch)
		c.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.lastError != nil {
			return nil, c.lastError
		}
		if c.cachedGas == nil {
			return nil, errors.New("failed to retrieve gas price")
		}
		return new(big.Int).Set(c.cachedGas), nil
	}

	c.fetching = true
	c.mu.Unlock()

	gas, err := c.client.GetGasPrice(ctx)

	c.mu.Lock()
	c.fetching = false
	c.fetchCount++

	if err != nil {
		c.lastError = err
	} else {
		if gas.Sign() <= 0 {
			c.lastError = errors.New("invalid gas price: 0 or negative")
		} else if c.maxGasPrice != nil && gas.Cmp(c.maxGasPrice) > 0 {
			c.lastError = fmt.Errorf("gas price spike detected: %s exceeds max %s", gas, c.maxGasPrice)
		} else {
			c.cachedGas = new(big.Int).Set(gas)
			c.cachedTime = time.Now()
			c.lastError = nil
		}
	}

	for _, ch := range c.waiters {
		close(ch)
	}
	c.waiters = nil

	if c.lastError != nil {
		c.mu.Unlock()
		return nil, c.lastError
	}
	c.mu.Unlock()
	return new(big.Int).Set(c.cachedGas), nil
}

// UI State Simulation helper for testing
type MockUIApp struct {
	Renderer     *MockWidgetRenderer
	ShowSettings bool
	SizingMode   string
	ScaleFactor  float64
	Paused       bool
	Logs         []string
}

func (app *MockUIApp) HandleKey(key string) {
	switch key {
	case "p", "P":
		app.Paused = !app.Paused
	case "s", "S":
		app.ShowSettings = !app.ShowSettings
	}
}

func (app *MockUIApp) Render() string {
	var sb strings.Builder
	if app.ShowSettings {
		sb.WriteString(app.Renderer.RenderSettingsPanel("polymarket", "paper", 5, app.SizingMode, app.ScaleFactor, 2.0))
	} else {
		bids := map[string]float64{"Yes": 0.41, "No": 0.57}
		asks := map[string]float64{"Yes": 0.43, "No": 0.59}
		sb.WriteString(app.Renderer.RenderMarketGrid("BTC-15M", []string{"Yes", "No"}, bids, asks, "WS", "active", "BTCUSDT", 84250.5, 0.64, "Yes"))
	}
	sb.WriteString("\n")
	sb.WriteString(app.Renderer.RenderLogViewer(app.Logs))
	return sb.String()
}

// ─── TIER 1 TESTS ────────────────────────────────────────────────────────────

// Lock-Free Price Snapshot: Rapid websocket ticks without deadlocking.
func TestT1_PriceSnapshot_WebsocketUpdates(t *testing.T) {
	wsSrv := NewMockBinanceWSServer()
	defer wsSrv.Close()

	feed := api.NewBinanceFuturesPriceFeed("BTCUSDT", 100*time.Millisecond)
	feed.RecordTradeSampleForTest(84000.0, time.Now())
	feed.SetConnectedForTest(true)

	// Simulate rapid incoming ticks in a separate loop
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			price := 84000.0 + float64(i)
			feed.RecordTradeSampleForTest(price, time.Now())
			time.Sleep(1 * time.Millisecond)
		}
	}()

	// Query snapshots rapidly from the test goroutine
	for i := 0; i < 200; i++ {
		snap := feed.Snapshot(time.Now())
		if snap.Symbol != "BTCUSDT" {
			t.Errorf("unexpected symbol: %s", snap.Symbol)
		}
		time.Sleep(1 * time.Millisecond)
	}

	<-done
}

// Lock-Free Price Snapshot: Retrieve the latest snapshot correctly.
func TestT1_PriceSnapshot_ReadLatest(t *testing.T) {
	feed := api.NewBinanceFuturesPriceFeed("ETHUSDT", 500*time.Millisecond)
	now := time.Now()

	// Initial samples to build baseline and latest price
	feed.RecordTradeSampleForTest(3000.0, now.Add(-600*time.Millisecond))
	feed.RecordTradeSampleForTest(3030.0, now)

	snap := feed.Snapshot(now)
	if !snap.Ready {
		t.Fatal("expected snapshot to be ready")
	}
	if snap.Price != 3030.0 {
		t.Errorf("expected price 3030.0, got %f", snap.Price)
	}
	if snap.BaselinePrice != 3000.0 {
		t.Errorf("expected baseline price 3000.0, got %f", snap.BaselinePrice)
	}
	// (3030 - 3000) / 3000 * 100 = 1.0%
	if diff := snap.DeltaPercent - 1.0; diff < -1e-9 || diff > 1e-9 {
		t.Errorf("expected delta percent 1.0, got %f", snap.DeltaPercent)
	}
}

// Lock-Free Price Snapshot: Concurrent strategy decisions read price snapshot.
func TestT1_PriceSnapshot_ConcurrentStrategy(t *testing.T) {
	feed := api.NewBinanceFuturesPriceFeed("SOLUSDT", 200*time.Millisecond)
	feed.RecordTradeSampleForTest(150.0, time.Now().Add(-300*time.Millisecond))
	feed.RecordTradeSampleForTest(151.5, time.Now())

	var wg sync.WaitGroup
	// 10 concurrent strategy runners reading snapshot
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				snap := feed.Snapshot(time.Now())
				if snap.Price <= 0 {
					t.Errorf("invalid price in concurrent read: %f", snap.Price)
				}
			}
		}()
	}

	wg.Wait()
}

// Lock-Free Price Snapshot: Rendering reads price snapshot without blocking.
func TestT1_PriceSnapshot_UIRenderRead(t *testing.T) {
	feed := api.NewBinanceFuturesPriceFeed("BTCUSDT", 1*time.Second)
	feed.RecordTradeSampleForTest(90000.0, time.Now())

	start := time.Now()
	// Render loop simulation (reads snapshot)
	for i := 0; i < 100; i++ {
		snap := feed.Snapshot(time.Now())
		if snap.Price != 90000.0 {
			t.Errorf("unexpected snapshot price: %f", snap.Price)
		}
	}
	duration := time.Since(start)
	if duration > 100*time.Millisecond {
		t.Errorf("snapshot reads took too long: %v", duration)
	}
}

// Lock-Free Price Snapshot: Multiple readers read snapshot simultaneously.
func TestT1_PriceSnapshot_ConcurrentReaders(t *testing.T) {
	feed := api.NewBinanceFuturesPriceFeed("BTCUSDT", 1*time.Second)
	feed.RecordTradeSampleForTest(95000.0, time.Now())

	var wg sync.WaitGroup
	numReaders := 50
	for i := 0; i < numReaders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				snap := feed.Snapshot(time.Now())
				if snap.Price != 95000.0 {
					t.Errorf("expected 95000.0, got %f", snap.Price)
				}
			}
		}()
	}
	wg.Wait()
}

// Monolithic UI Componentization: Market grid widget displays correct details.
func TestT1_UIComponent_MarketGrid(t *testing.T) {
	r := NewMockWidgetRenderer(80, 10)
	bids := map[string]float64{"Yes": 0.45, "No": 0.53}
	asks := map[string]float64{"Yes": 0.47, "No": 0.55}

	grid := r.RenderMarketGrid("ETH-WEEKLY", []string{"Yes", "No"}, bids, asks, "WS", "active", "ETHUSDT", 3050.0, -1.25, "No")

	if !strings.Contains(grid, "MARKET: ETH-WEEKLY") {
		t.Errorf("expected market ID, got %q", grid)
	}
	if !strings.Contains(grid, "Yes ($0.45 / $0.47)") {
		t.Errorf("expected Yes prices, got %q", grid)
	}
	if !strings.Contains(grid, "No ($0.53 / $0.55)") {
		t.Errorf("expected No prices, got %q", grid)
	}
	if !strings.Contains(grid, "Binance: ETHUSDT $3050.00") {
		t.Errorf("expected Binance details, got %q", grid)
	}
}

// Monolithic UI Componentization: Settings panel displays active profile configuration.
func TestT1_UIComponent_SettingsPanel(t *testing.T) {
	r := NewMockWidgetRenderer(80, 10)
	settings := r.RenderSettingsPanel("polymarket", "live", 10, "percent", 0.08, 1.5)

	if !strings.Contains(settings, "Exchange=polymarket") {
		t.Errorf("expected exchange, got %q", settings)
	}
	if !strings.Contains(settings, "Backend=live") {
		t.Errorf("expected backend, got %q", settings)
	}
	if !strings.Contains(settings, "Max Markets: 10") {
		t.Errorf("expected max markets, got %q", settings)
	}
	if !strings.Contains(settings, "Sizing: percent (8.00%)") {
		t.Errorf("expected sizing mode and scale, got %q", settings)
	}
}

// Monolithic UI Componentization: Log viewer widget displays logs in real-time.
func TestT1_UIComponent_LogViewer(t *testing.T) {
	r := NewMockWidgetRenderer(80, 10)
	logs := []string{
		"System start",
		"Connected to websocket",
		"Order placed: Buy Yes 100 shares @ 0.45",
	}

	view := r.RenderLogViewer(logs)
	if !strings.Contains(view, "EVENT LOGS:") {
		t.Errorf("expected header, got %q", view)
	}
	for _, l := range logs {
		if !strings.Contains(view, l) {
			t.Errorf("expected log line %q, got %q", l, view)
		}
	}
}

// Monolithic UI Componentization: Widget render loop performs modular UI updates.
func TestT1_UIComponent_ModularLayout(t *testing.T) {
	app := MockUIApp{
		Renderer:     NewMockWidgetRenderer(80, 15),
		ShowSettings: false,
		SizingMode:   "percent",
		ScaleFactor:  0.05,
		Paused:       false,
		Logs:         []string{"Start", "Tick 1"},
	}

	layout1 := app.Render()
	if !strings.Contains(layout1, "MARKET: BTC-15M") {
		t.Errorf("expected market grid in main view, got %s", layout1)
	}
	if strings.Contains(layout1, "SETTINGS:") {
		t.Errorf("did not expect settings in main view, got %s", layout1)
	}

	app.ShowSettings = true
	layout2 := app.Render()
	if !strings.Contains(layout2, "SETTINGS: Exchange=polymarket") {
		t.Errorf("expected settings panel in settings view, got %s", layout2)
	}
	if strings.Contains(layout2, "MARKET: BTC-15M") {
		t.Errorf("did not expect market grid in settings view, got %s", layout2)
	}
}

// Monolithic UI Componentization: Terminal keypresses propagate to respective widgets.
func TestT1_UIComponent_InputPropagation(t *testing.T) {
	app := MockUIApp{
		Renderer:     NewMockWidgetRenderer(80, 15),
		ShowSettings: false,
		SizingMode:   "percent",
		ScaleFactor:  0.05,
		Paused:       false,
		Logs:         []string{"Event"},
	}

	app.HandleKey("s")
	if !app.ShowSettings {
		t.Fatal("expected 's' to toggle settings overlay")
	}

	app.HandleKey("p")
	if !app.Paused {
		t.Fatal("expected 'p' to toggle pause status")
	}

	app.HandleKey("S")
	if app.ShowSettings {
		t.Fatal("expected 'S' to toggle settings overlay back to false")
	}
}

// Decimal Fixed-Point Precision: Rounding conforms to CLOB tick size.
func TestT1_RoundingTickSize(t *testing.T) {
	params := strategy.MakerParams{
		QuoteStep: 0.05,
	}

	// Mid price = (0.41 + 0.59) / 2 = 0.50. Quote gap = 0.02. Skew = 0.
	// Expected buy price rounded to QuoteStep (0.05):
	// mid - quoteGap - skew*skewStep = 0.50 - 0.02 - 0 = 0.48. Clamped/rounded to QuoteStep (0.05) -> 0.50.
	price, ok := strategy.ComputeMakerSkewedQuote(true, 0.41, 0.59, 0.0, 0.02, params)
	if !ok {
		t.Fatal("expected successful quote computation")
	}
	if price != 0.50 {
		t.Errorf("expected rounded price 0.50, got %f", price)
	}

	// Change quoteGap to 0.06:
	// mid - quoteGap = 0.50 - 0.06 = 0.44. Rounded to nearest 0.05 -> 0.45.
	price, ok = strategy.ComputeMakerSkewedQuote(true, 0.41, 0.59, 0.0, 0.06, params)
	if !ok {
		t.Fatal("expected successful quote computation")
	}
	if price != 0.45 {
		t.Errorf("expected rounded price 0.45, got %f", price)
	}
}

// Decimal Fixed-Point Precision: Precise sell fee calculation.
func TestT1_Decimal_ComputeFee(t *testing.T) {
	// fee = C × feeRate × p × (1 - p)
	// where C = 1000 shares, feeRate = 100 bps (0.01), p = 0.40
	// fee = 1000 * 0.01 * 0.40 * 0.60 = 2.40 USDC
	fee := strategy.ComputeMakerSellFeeUsdc(1000.0, 0.40, 100)
	if fee != 2.40 {
		t.Errorf("expected fee 2.40, got %f", fee)
	}

	// Test 5 decimal rounding:
	// shares = 123.456, price = 0.35, feeRate = 50 bps (0.005)
	// fee = 123.456 * 0.005 * 0.35 * 0.65 = 0.1404312 -> rounded to 5 decimals: 0.14043
	feeRound := strategy.ComputeMakerSellFeeUsdc(123.456, 0.35, 50)
	if feeRound != 0.14043 {
		t.Errorf("expected rounded fee 0.14043, got %f", feeRound)
	}
}

// Decimal Fixed-Point Precision: Precise inventory skew ratio.
func TestT1_Decimal_ComputeSkew(t *testing.T) {
	// skew = (positionShares - peerShares) / targetShares
	// position = 300, peer = 100, target = 500 -> (300-100)/500 = 0.40
	skew := strategy.ComputeMakerInventorySkew(300.0, 100.0, 500.0)
	if skew != 0.40 {
		t.Errorf("expected skew 0.40, got %f", skew)
	}

	// Test clamping hi:
	// position = 1000, peer = 100, target = 500 -> (1000-100)/500 = 1.8 -> clamped to 1.0
	skewClampHi := strategy.ComputeMakerInventorySkew(1000.0, 100.0, 500.0)
	if skewClampHi != 1.0 {
		t.Errorf("expected clamped skew 1.0, got %f", skewClampHi)
	}

	// Test clamping lo:
	// position = 100, peer = 900, target = 500 -> (100-900)/500 = -1.6 -> clamped to -1.0
	skewClampLo := strategy.ComputeMakerInventorySkew(100.0, 900.0, 500.0)
	if skewClampLo != -1.0 {
		t.Errorf("expected clamped skew -1.0, got %f", skewClampLo)
	}
}

// Decimal Fixed-Point Precision: Precise skewed buy/sell quote calculation.
func TestT1_Decimal_ComputeSkewedQuote(t *testing.T) {
	params := strategy.MakerParams{
		QuoteStep:         0.01,
		DefaultQuoteGap:   0.02,
		InventorySkewStep: 0.05,
	}

	// Mid price = (0.40 + 0.60) / 2 = 0.50
	// Buy quote with positive skew (more inventory, buy cheaper):
	// mid - quoteGap - (skew * skewStep) = 0.50 - 0.02 - (2.0 * 0.05) = 0.50 - 0.02 - 0.10 = 0.38
	price, ok := strategy.ComputeMakerSkewedQuote(true, 0.40, 0.60, 2.0, 0.02, params)
	if !ok {
		t.Fatal("expected quote success")
	}
	if price != 0.38 {
		t.Errorf("expected skewed buy price 0.38, got %f", price)
	}

	// Sell quote with positive skew (sell cheaper to reduce inventory):
	// mid + quoteGap - (skew * skewStep) = 0.50 + 0.02 - (2.0 * 0.05) = 0.52 - 0.10 = 0.42
	price, ok = strategy.ComputeMakerSkewedQuote(false, 0.40, 0.60, 2.0, 0.02, params)
	if !ok {
		t.Fatal("expected quote success")
	}
	if price != 0.42 {
		t.Errorf("expected skewed sell price 0.42, got %f", price)
	}
}

// Decimal Fixed-Point Precision: Precise pair pricing calculation.
func TestT1_Decimal_ComputePairPrices(t *testing.T) {
	params := strategy.MakerParams{
		QuoteStep: 0.01,
	}
	// ask1=0.45, ask2=0.65, maxPairCost=0.98. inventoryDelta=0 (neutral).
	// Expect prices: price1 = ask1 - step = 0.44, price2 = ask2 - step = 0.64
	// Total = 1.08 > 0.98, so it loops to reduce.
	// Since inventoryDelta=0, it reduces the leg that is further from its min price first.
	price1, price2, ok := strategy.ComputeMakerPairBuyPrices(0.35, 0.45, 0.55, 0.65, 0.98, 0.0, params)
	if !ok {
		t.Fatal("expected pair buy price success")
	}
	if price1+price2 > 0.98+1e-9 {
		t.Errorf("expected total pair price <= 0.98, got %f + %f = %f", price1, price2, price1+price2)
	}
}

// RPC Fallback & Gas Cache: Successfully fetch block/gas on primary endpoint.
func TestT1_RPC_PrimarySuccess(t *testing.T) {
	s := NewMockRPCServer()
	defer s.Close()

	client := api.NewPolygonClient(s.URL())
	gas, err := client.GetGasPrice(context.Background())
	if err != nil {
		t.Fatalf("failed to fetch gas price: %v", err)
	}
	if gas.Cmp(big.NewInt(50_000_000_000)) != 0 {
		t.Errorf("unexpected gas price: %v", gas)
	}

	baseFee, err := client.GetBlockBaseFee(context.Background())
	if err != nil {
		t.Fatalf("failed to fetch base fee: %v", err)
	}
	if baseFee.Cmp(big.NewInt(40_000_000_000)) != 0 {
		t.Errorf("unexpected base fee: %v", baseFee)
	}
}

// RPC Fallback & Gas Cache: Fall back to secondary RPC on primary timeout/failure.
func TestT1_RPC_FallbackSecondary(t *testing.T) {
	s1 := NewMockRPCServer()
	defer s1.Close()
	s2 := NewMockRPCServer()
	defer s2.Close()

	s1.SetHealthy(false) // primary fails

	fc := NewFallbackClient([]string{s1.URL(), s2.URL()})

	var gas *big.Int
	err := fc.CallWithFallback(func(client *api.PolygonClient) error {
		var callErr error
		gas, callErr = client.GetGasPrice(context.Background())
		return callErr
	})

	if err != nil {
		t.Fatalf("fallback failed: %v", err)
	}
	if gas.Cmp(big.NewInt(50_000_000_000)) != 0 {
		t.Errorf("unexpected gas price: %v", gas)
	}
}

// RPC Fallback & Gas Cache: Fall back to tertiary RPC when primary & secondary fail.
func TestT1_RPC_FallbackTertiary(t *testing.T) {
	s1 := NewMockRPCServer()
	defer s1.Close()
	s2 := NewMockRPCServer()
	defer s2.Close()
	s3 := NewMockRPCServer()
	defer s3.Close()

	s1.SetHealthy(false)
	s2.SetHealthy(false)

	s3.GasPrice = big.NewInt(60_000_000_000)

	fc := NewFallbackClient([]string{s1.URL(), s2.URL(), s3.URL()})

	var gas *big.Int
	err := fc.CallWithFallback(func(client *api.PolygonClient) error {
		var callErr error
		gas, callErr = client.GetGasPrice(context.Background())
		return callErr
	})

	if err != nil {
		t.Fatalf("tertiary fallback failed: %v", err)
	}
	if gas.Cmp(big.NewInt(60_000_000_000)) != 0 {
		t.Errorf("expected tertiary gas price 60 Gwei, got %v", gas)
	}
}

// RPC Fallback & Gas Cache: Verify cached gas values within TTL.
func TestT1_RPC_GasCacheTTL(t *testing.T) {
	s := NewMockRPCServer()
	defer s.Close()

	client := api.NewPolygonClient(s.URL())
	cache := NewGasCache(client, 1*time.Second, nil)

	// Call 1
	g1, err := cache.GetGasPrice(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Call 2 (within TTL)
	g2, err := cache.GetGasPrice(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if g1.Cmp(g2) != 0 {
		t.Errorf("expected same gas price, got %v and %v", g1, g2)
	}

	s.mu.Lock()
	calls := s.MethodCallCount["eth_gasPrice"]
	s.mu.Unlock()

	if calls != 1 {
		t.Errorf("expected exactly 1 RPC call, got %d", calls)
	}
}

// RPC Fallback & Gas Cache: Refresh gas values when TTL expires.
func TestT1_RPC_GasCacheExpiry(t *testing.T) {
	s := NewMockRPCServer()
	defer s.Close()

	client := api.NewPolygonClient(s.URL())
	cache := NewGasCache(client, 5*time.Millisecond, nil)

	_, err := cache.GetGasPrice(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(10 * time.Millisecond)

	// Change mock value
	s.mu.Lock()
	s.GasPrice = big.NewInt(55_000_000_000)
	s.mu.Unlock()

	g2, err := cache.GetGasPrice(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if g2.Cmp(big.NewInt(55_000_000_000)) != 0 {
		t.Errorf("expected refreshed gas price 55 Gwei, got %v", g2)
	}

	s.mu.Lock()
	calls := s.MethodCallCount["eth_gasPrice"]
	s.mu.Unlock()

	if calls != 2 {
		t.Errorf("expected exactly 2 RPC calls, got %d", calls)
	}
}
