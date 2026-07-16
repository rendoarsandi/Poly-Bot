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

// ─── TIER 3 TESTS ────────────────────────────────────────────────────────────

// TestT3_UI_With_ConcurrentSnapshot: UI renders smoothly during heavy, concurrent websocket price snapshot updates.
func TestT3_UI_With_ConcurrentSnapshot(t *testing.T) {
	feed := api.NewBinanceFuturesPriceFeed("BTCUSDT", 50*time.Millisecond)
	feed.SetConnectedForTest(true)
	feed.RecordTradeSampleForTest(90000.0, time.Now())

	r := NewMockWidgetRenderer(80, 10)

	// Rapid websocket updates in background
	stopCh := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		price := 90000.0
		for {
			select {
			case <-stopCh:
				return
			default:
				price += 0.5
				feed.RecordTradeSampleForTest(price, time.Now())
				time.Sleep(1 * time.Millisecond)
			}
		}
	}()

	// UI render simulation loop
	start := time.Now()
	renders := 0
	for i := 0; i < 200; i++ {
		snap := feed.Snapshot(time.Now())
		bids := map[string]float64{"Yes": snap.Price - 100.0, "No": snap.Price + 100.0}
		asks := map[string]float64{"Yes": snap.Price - 90.0, "No": snap.Price + 110.0}
		grid := r.RenderMarketGrid("BTC-15M", []string{"Yes", "No"}, bids, asks, "WS", "active", "BTCUSDT", snap.Price, snap.DeltaPercent, "Yes")
		if !strings.Contains(grid, "MARKET: BTC-15M") {
			t.Errorf("expected market grid in output, got %q", grid)
		}
		renders++
		time.Sleep(1 * time.Millisecond)
	}
	duration := time.Since(start)
	close(stopCh)
	wg.Wait()

	t.Logf("Completed %d renders in %v", renders, duration)
	if duration > 1000*time.Millisecond {
		t.Errorf("UI rendering took too long under concurrent snapshot updates: %v", duration)
	}
}

// TestT3_Decimal_With_RPCFallback: RPC gas price queries feed into fixed-point Decimal maker strategy calculations.
func TestT3_Decimal_With_RPCFallback(t *testing.T) {
	s1 := NewMockRPCServer()
	defer s1.Close()
	s2 := NewMockRPCServer()
	defer s2.Close()

	s1.SetHealthy(false) // s1 is unhealthy to trigger fallback to s2
	s2.GasPrice = big.NewInt(60_000_000_000) // 60 Gwei

	fc := NewFallbackClient([]string{s1.URL(), s2.URL()})

	// Fetch gas price using fallback client
	var gasPrice *big.Int
	err := fc.CallWithFallback(func(client *api.PolygonClient) error {
		var callErr error
		gasPrice, callErr = client.GetGasPrice(context.Background())
		return callErr
	})
	if err != nil {
		t.Fatalf("failed to fetch gas price via fallback client: %v", err)
	}

	// Dynamic Gas Cost Calculation:
	// Let's assume gas limit of 500 (polygonRedeemGasLimit scale) and ETH price of 3000 USDC
	gasLimit := 500.0
	ethPrice := 3000.0

	// Convert gasPrice (Wei) to ETH, then to USDC
	gasPriceWei := new(big.Float).SetInt(gasPrice)
	gasPriceEth := new(big.Float).Quo(gasPriceWei, big.NewFloat(1e18))
	gasCostEth := new(big.Float).Mul(gasPriceEth, big.NewFloat(gasLimit))
	gasCostUsdc, _ := new(big.Float).Mul(gasCostEth, big.NewFloat(ethPrice)).Float64()

	// Feed this gasCostUsdc as dynamic min edge into maker parameters
	params := strategy.MakerParams{
		QuoteStep:           0.01,
		DefaultQuoteGap:     0.02,
		InventorySkewStep:   0.05,
		QuoteSizeSkewFactor: 0.1,
		CashUsagePerOutcome: 0.5,
		MinQuoteValue:       gasCostUsdc + 0.10, // Must cover gas cost + 0.10 margin
	}

	// Calculate skewed quote under normal gas (0.09 USDC overhead)
	// Mid price = (0.50 + 0.52) / 2 = 0.51. QuoteGap = 0.02.
	// Buy price base = 0.51 - 0.02 - 0 = 0.49.
	price, ok := strategy.ComputeMakerSkewedQuote(true, 0.50, 0.52, 0.0, 0.02, params)
	if !ok {
		t.Fatal("expected quote computation to succeed")
	}
	if price != 0.49 {
		t.Errorf("expected buy price 0.49, got %f", price)
	}

	// Let's verify sizing based on cash:
	// cash = 10.0. MinQuoteValue = 0.09 + 0.10 = 0.19.
	// BuyQty should be non-zero since CashUsagePerOutcome * cash / price = 5.0 / 0.49 = 10.2 shares, which is worth ~5.0 USDC > 0.19 USDC.
	qty := strategy.ComputeMakerBuyQty(5.0, 0.0, 0.0, 100.0, 10.0, price, params, nil)
	if qty <= 0 {
		t.Errorf("expected positive buy quantity, got %f", qty)
	}

	// Now simulate a gas price spike on s2
	s2.mu.Lock()
	s2.GasPrice = big.NewInt(600_000_000_000) // 600 Gwei (approx 0.90 USDC gas cost)
	s2.mu.Unlock()

	err = fc.CallWithFallback(func(client *api.PolygonClient) error {
		var callErr error
		gasPrice, callErr = client.GetGasPrice(context.Background())
		return callErr
	})
	if err != nil {
		t.Fatalf("failed to fetch gas price after spike: %v", err)
	}

	gasPriceWei = new(big.Float).SetInt(gasPrice)
	gasPriceEth = new(big.Float).Quo(gasPriceWei, big.NewFloat(1e18))
	gasCostEth = new(big.Float).Mul(gasPriceEth, big.NewFloat(gasLimit))
	gasCostUsdcSpiked, _ := new(big.Float).Mul(gasCostEth, big.NewFloat(ethPrice)).Float64()

	// Update params with spiked gas cost
	params.MinQuoteValue = gasCostUsdcSpiked + 0.10 // 0.90 + 0.10 = 1.00

	// If cash is low, let's see if the dynamic MinQuoteValue blocks buying.
	// For cash = 1.50 USDC, CashUsagePerOutcome = 0.5, affordable cash = 0.75 USDC.
	// Affordable shares = 0.75 / 0.49 = 1.53 shares.
	// Sized value in USDC = 1.53 * 0.49 = 0.75 USDC.
	// Since 0.75 USDC < MinQuoteValue (1.00 USDC), it should return 0.
	qtySpiked := strategy.ComputeMakerBuyQty(5.0, 0.0, 0.0, 100.0, 1.50, price, params, nil)
	if qtySpiked != 0 {
		t.Errorf("expected buy quantity to be blocked (0) due to spiked gas cost, got %f", qtySpiked)
	}
}

// TestT3_Concurrency_With_Decimal: Multiple strategy worker routines run decimal pricing calculations using atomic snapshot values simultaneously.
func TestT3_Concurrency_With_Decimal(t *testing.T) {
	feed := api.NewBinanceFuturesPriceFeed("ETHUSDT", 100*time.Millisecond)
	feed.SetConnectedForTest(true)

	// Start with some price baseline
	feed.RecordTradeSampleForTest(3000.0, time.Now())

	params := strategy.MakerParams{
		QuoteStep:           0.01,
		DefaultQuoteGap:     0.02,
		InventorySkewStep:   0.05,
		QuoteSizeSkewFactor: 0.1,
		CashUsagePerOutcome: 0.5,
		MinQuoteValue:       1.0,
	}

	// Goroutine writing price updates rapidly
	stopCh := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		price := 3000.0
		for {
			select {
			case <-stopCh:
				return
			default:
				price += 0.1
				feed.RecordTradeSampleForTest(price, time.Now())
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()

	// 10 concurrent strategy workers running calculations
	var workersWg sync.WaitGroup
	numWorkers := 10
	iterations := 100
	for i := 0; i < numWorkers; i++ {
		workersWg.Add(1)
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			defer workersWg.Done()
			for j := 0; j < iterations; j++ {
				snap := feed.Snapshot(time.Now())
				if !snap.Ready {
					time.Sleep(1 * time.Millisecond)
					continue
				}

				// Generate synthetic bids/asks centered around snap.Price normalized to cents
				// e.g. mid = snap.Price mod 1 (cents range)
				mid := math.Mod(snap.Price, 1.0)
				if mid <= 0.1 || mid >= 0.9 {
					mid = 0.5
				}
				bid := mid - 0.05
				ask := mid + 0.05

				buyPrice, buyOk := strategy.ComputeMakerSkewedQuote(true, bid, ask, 0.2, 0.02, params)
				sellPrice, sellOk := strategy.ComputeMakerSkewedQuote(false, bid, ask, 0.2, 0.02, params)

				if buyOk && buyPrice <= 0 {
					t.Errorf("worker %d: invalid buy price: %f", workerID, buyPrice)
				}
				if sellOk && sellPrice <= 0 {
					t.Errorf("worker %d: invalid sell price: %f", workerID, sellPrice)
				}

				// Sizing calculations
				buyQty := strategy.ComputeMakerBuyQty(10.0, 5.0, 0.2, 100.0, 50.0, buyPrice, params, nil)
				sellQty := strategy.ComputeMakerSellQty(10.0, 5.0, 0.2, sellPrice, params, nil)

				_ = buyQty
				_ = sellQty
			}
		}(i)
	}

	workersWg.Wait()
	close(stopCh)
	wg.Wait()
}

// TestT3_UI_With_DecimalFormatting: Componentized UI widgets format and display fixed-point decimal prices/quantities correctly.
func TestT3_UI_With_DecimalFormatting(t *testing.T) {
	r := NewMockWidgetRenderer(80, 12)

	params := strategy.MakerParams{
		QuoteStep:           0.0001, // High precision
		DefaultQuoteGap:     0.0050,
		InventorySkewStep:   0.0010,
		QuoteSizeSkewFactor: 0.05,
		CashUsagePerOutcome: 0.25,
		MinQuoteValue:       0.05,
	}

	// Compute skewed buy/sell quotes with higher precision
	bid1, ask1 := 0.4520, 0.4580
	bid2, ask2 := 0.5420, 0.5480

	buyPrice1, ok1 := strategy.ComputeMakerSkewedQuote(true, bid1, ask1, 0.1, 0.0020, params)
	buyPrice2, ok2 := strategy.ComputeMakerSkewedQuote(true, bid2, ask2, -0.1, 0.0020, params)

	if !ok1 || !ok2 {
		t.Fatal("expected quote computations to succeed")
	}

	// Verify price formatting in UI
	bids := map[string]float64{"Yes": buyPrice1, "No": buyPrice2}
	asks := map[string]float64{"Yes": buyPrice1 + 0.005, "No": buyPrice2 + 0.005}

	gridOutput := r.RenderMarketGrid("ETH-HIGH-PREC", []string{"Yes", "No"}, bids, asks, "WS", "active", "ETHUSDT", 3050.45, 0.25, "Yes")

	// Assert the prices are formatted to two decimals since RenderMarketGrid formats to $%.2f
	expectedYesStr := fmt.Sprintf("$%.2f", buyPrice1)
	expectedNoStr := fmt.Sprintf("$%.2f", buyPrice2)

	if !strings.Contains(gridOutput, expectedYesStr) {
		t.Errorf("expected grid to contain Yes price %s, got %s", expectedYesStr, gridOutput)
	}
	if !strings.Contains(gridOutput, expectedNoStr) {
		t.Errorf("expected grid to contain No price %s, got %s", expectedNoStr, gridOutput)
	}

	// Verify settings panel formatting
	scaleFactor := 0.0525
	settingsOutput := r.RenderSettingsPanel("polymarket", "paper", 10, "percent", scaleFactor, 2.35)

	if !strings.Contains(settingsOutput, "Sizing: percent (5.25%)") {
		t.Errorf("expected formatted scale factor in settings, got %q", settingsOutput)
	}
	if !strings.Contains(settingsOutput, "Min Margin: 2.35%") {
		t.Errorf("expected formatted minimum margin in settings, got %q", settingsOutput)
	}

	// Test Log formatting of decimal quantities
	qty1 := strategy.ComputeMakerBuyQty(10.0, 0.0, 0.0, 100.0, 100.0, buyPrice1, params, nil)
	qty2 := strategy.ComputeMakerSellQty(10.0, 50.0, 0.0, buyPrice2, params, nil)

	log1 := fmt.Sprintf("Order Buy Yes: Qty=%.4f Price=%.4f", qty1, buyPrice1)
	log2 := fmt.Sprintf("Order Sell No: Qty=%.4f Price=%.4f", qty2, buyPrice2)

	logs := []string{log1, log2}
	logOutput := r.RenderLogViewer(logs)

	if !strings.Contains(logOutput, log1) {
		t.Errorf("expected log view to contain %q, got %q", log1, logOutput)
	}
	if !strings.Contains(logOutput, log2) {
		t.Errorf("expected log view to contain %q, got %q", log2, logOutput)
	}
}
