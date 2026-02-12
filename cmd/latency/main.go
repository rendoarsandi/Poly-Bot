package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
)

type LatencyStats struct {
	Min    time.Duration
	Max    time.Duration
	Avg    time.Duration
	P50    time.Duration
	P95    time.Duration
	P99    time.Duration
	Count  int
	Errors int
}

func calculateStats(samples []time.Duration) LatencyStats {
	if len(samples) == 0 {
		return LatencyStats{}
	}

	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var total time.Duration
	for _, s := range sorted {
		total += s
	}

	return LatencyStats{
		Min:   sorted[0],
		Max:   sorted[len(sorted)-1],
		Avg:   total / time.Duration(len(sorted)),
		P50:   sorted[len(sorted)*50/100],
		P95:   sorted[len(sorted)*95/100],
		P99:   sorted[len(sorted)*99/100],
		Count: len(sorted),
	}
}

func printStats(name string, stats LatencyStats) {
	fmt.Printf("   ├─ Min: %v\n", stats.Min)
	fmt.Printf("   ├─ Max: %v\n", stats.Max)
	fmt.Printf("   ├─ Avg: %v\n", stats.Avg)
	fmt.Printf("   ├─ P50: %v\n", stats.P50)
	fmt.Printf("   ├─ P95: %v\n", stats.P95)
	fmt.Printf("   ├─ P99: %v\n", stats.P99)
	fmt.Printf("   └─ Samples: %d (errors: %d)\n", stats.Count, stats.Errors)
}

func main() {
	cfg, err := core.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	fmt.Println("╔════════════════════════════════════════════════════════════════╗")
	fmt.Println("║          POLYMARKET FULL LATENCY BENCHMARK                     ║")
	fmt.Println("║     Real network latency - no money spent                      ║")
	fmt.Println("╚════════════════════════════════════════════════════════════════╝")
	fmt.Println()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	restClient := api.NewRestClient("")

	// First, find a real market to test with
	fmt.Println("🔍 Finding active 15m market for testing...")
	markets, err := restClient.Get15mMarkets(ctx, []string{"btc", "eth", "sol"})
	if err != nil || len(markets) == 0 {
		fmt.Println("⚠️  No active markets found, using placeholder token ID")
		// Use a known token ID format for dry testing
	}

	var testTokenID string
	var testMarketName string
	if len(markets) > 0 && len(markets[0].Tokens) > 0 {
		testTokenID = markets[0].Tokens[0].TokenID
		testMarketName = markets[0].Slug
		fmt.Printf("   ✅ Using market: %s\n", testMarketName)
		fmt.Printf("   ✅ Token ID: %s...\n", testTokenID[:20])
	}
	fmt.Println()

	// ═══════════════════════════════════════════════════════════════════
	// 1. REST /book Endpoint Latency (what you poll every 20ms)
	// ═══════════════════════════════════════════════════════════════════
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("🧪 1. REST /book LATENCY (polled every 20ms)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	var restSamples []time.Duration
	restErrors := 0
	iterations := 100

	if testTokenID != "" {
		for i := 0; i < iterations; i++ {
			start := time.Now()
			_, err := restClient.GetOrderBook(ctx, testTokenID)
			elapsed := time.Since(start)
			if err != nil {
				restErrors++
			} else {
				restSamples = append(restSamples, elapsed)
			}
			// Small delay to avoid rate limit during test
			time.Sleep(10 * time.Millisecond)
		}
	}

	restStats := calculateStats(restSamples)
	restStats.Errors = restErrors
	printStats("REST /book", restStats)
	fmt.Println()

	// ═══════════════════════════════════════════════════════════════════
	// 2. WebSocket Message Latency
	// ═══════════════════════════════════════════════════════════════════
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("🧪 2. WEBSOCKET MESSAGE LATENCY")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	wsMgr := api.NewWSManager("")
	wsCtx, wsCancel := context.WithTimeout(ctx, 30*time.Second)
	defer wsCancel()

	var wsSamples []time.Duration
	var wsMessageCount atomic.Int32
	wsConnectStart := time.Now()

	if err := wsMgr.Connect(wsCtx); err != nil {
		fmt.Printf("   ❌ WS Connect failed: %v\n", err)
	} else {
		wsConnectTime := time.Since(wsConnectStart)
		fmt.Printf("   ✅ WS Connect time: %v\n", wsConnectTime)

		// Subscribe to market if we have a token ID
		if testTokenID != "" {
			subPayload := map[string]interface{}{
				"type":      "Market",
				"assets_id": testTokenID,
			}
			if err := wsMgr.Subscribe(wsCtx, subPayload); err != nil {
				fmt.Printf("   ⚠️  Subscribe failed: %v\n", err)
			} else {
				fmt.Printf("   ✅ Subscribed to token\n")
			}
		}

		// Measure message arrival times for 10 seconds
		fmt.Println("   ⏳ Collecting WS messages for 10 seconds...")
		msgChan := wsMgr.StartStreaming(wsCtx)

		collectStart := time.Now()
		lastMsgTime := time.Now()

		var wsMu sync.Mutex
		done := make(chan struct{})

		go func() {
			for {
				select {
				case msg, ok := <-msgChan:
					if !ok {
						return
					}
					now := time.Now()
					if wsMessageCount.Load() > 0 {
						wsMu.Lock()
						wsSamples = append(wsSamples, now.Sub(lastMsgTime))
						wsMu.Unlock()
					}
					lastMsgTime = now
					wsMessageCount.Add(1)
					_ = msg // consume
				case <-done:
					return
				}
			}
		}()

		time.Sleep(10 * time.Second)
		close(done)

		elapsed := time.Since(collectStart)
		fmt.Printf("   ✅ Received %d messages in %v\n", wsMessageCount.Load(), elapsed.Round(time.Millisecond))

		if len(wsSamples) > 1 {
			wsStats := calculateStats(wsSamples)
			fmt.Println("   📊 Inter-message intervals:")
			printStats("WS Inter-msg", wsStats)
		} else {
			fmt.Println("   ⚠️  Not enough messages to calculate inter-message latency")
			fmt.Println("      (Market may be inactive - this is normal)")
		}

		wsMgr.Close()
	}
	fmt.Println()

	// ═══════════════════════════════════════════════════════════════════
	// 3. EIP-712 Signing Speed (Local CPU)
	// ═══════════════════════════════════════════════════════════════════
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("🧪 3. EIP-712 SIGNING SPEED (local CPU)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	if cfg.PK == "" {
		fmt.Println("   ⚠️  POLY_PK not set - skipping signing benchmark")
	} else {
		signer, err := api.NewSigner(cfg.PK, api.DefaultVerifyingContract)
		if err != nil {
			fmt.Printf("   ❌ Failed to create signer: %v\n", err)
		} else {
			testOrder := &api.OrderData{
				Salt:          "123456789",
				Maker:         signer.Address(),
				Signer:        signer.Address(),
				Taker:         "0x0000000000000000000000000000000000000000",
				TokenID:       "123456789",
				MakerAmount:   "1000000",
				TakerAmount:   "1000000",
				Expiration:    "123456789",
				Nonce:         "0",
				FeeRateBps:    "0",
				Side:          0,
				SignatureType: 0,
			}

			var signSamples []time.Duration
			signIterations := 100
			for i := 0; i < signIterations; i++ {
				start := time.Now()
				_, err := signer.SignOrder(testOrder)
				if err == nil {
					signSamples = append(signSamples, time.Since(start))
				}
			}

			signStats := calculateStats(signSamples)
			printStats("EIP-712 Sign", signStats)
		}
	}
	fmt.Println()

	// ═══════════════════════════════════════════════════════════════════
	// 4. L2 Auth Header Signing Speed
	// ═══════════════════════════════════════════════════════════════════
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("🧪 4. L2 AUTH HEADER SIGNING (local CPU)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	if cfg.APIKey == "" || cfg.APISecret == "" {
		fmt.Println("   ⚠️  API credentials not set - skipping L2 auth benchmark")
	} else {
		auth := &api.APIAuth{
			APIKey:     cfg.APIKey,
			APISecret:  cfg.APISecret,
			Passphrase: cfg.APIPassphrase,
		}

		var authSamples []time.Duration
		for i := 0; i < 100; i++ {
			start := time.Now()
			auth.SignL2Request("POST", "/order", `{"side":"BUY"}`)
			authSamples = append(authSamples, time.Since(start))
		}

		authStats := calculateStats(authSamples)
		printStats("L2 Auth Sign", authStats)
	}
	fmt.Println()

	// ═══════════════════════════════════════════════════════════════════
	// 5. Full Order Submission (DRY RUN - no money spent)
	// ═══════════════════════════════════════════════════════════════════
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("🧪 5. ORDER SUBMISSION LATENCY (dry-run, invalid order)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("   ℹ️  Sends intentionally invalid order to measure full round-trip")
	fmt.Println("   ℹ️  Server will reject it - we measure the response time")

	if cfg.PK == "" || cfg.APIKey == "" {
		fmt.Println("   ⚠️  Credentials not set - skipping order submission benchmark")
	} else {
		clobClient, err := api.NewCLOBClient(cfg.PK, cfg.APIKey, cfg.APISecret, cfg.APIPassphrase)
		if err != nil {
			fmt.Printf("   ❌ Failed to create CLOB client: %v\n", err)
		} else {
			var orderSamples []time.Duration
			orderIterations := 10

			for i := 0; i < orderIterations; i++ {
				start := time.Now()
				// Send order with $0.001 price and 1 share - will be rejected but measures latency
				_, err := clobClient.PlaceOrder(ctx, &api.OrderRequest{
					TokenID: "invalid_token_for_latency_test",
					Side:    api.SideBuy,
					Price:   0.001,
					Size:    1,
				})
				elapsed := time.Since(start)
				// We expect an error (invalid token) - that's fine, we're measuring latency
				orderSamples = append(orderSamples, elapsed)
				_ = err
				time.Sleep(100 * time.Millisecond) // Don't spam
			}

			orderStats := calculateStats(orderSamples)
			printStats("Order Submit", orderStats)
		}
	}
	fmt.Println()

	// ═══════════════════════════════════════════════════════════════════
	// 6. REST Throughput Test (max RPS)
	// ═══════════════════════════════════════════════════════════════════
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("🧪 6. REST THROUGHPUT TEST (5 seconds, no delay)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	if testTokenID != "" {
		// Create fresh client to reset rate limiter state
		throughputClient := api.NewRestClient("")
		throughputCtx, throughputCancel := context.WithTimeout(ctx, 5*time.Second)

		var throughputSamples []time.Duration
		var throughputErrors int
		throughputStart := time.Now()

		for {
			select {
			case <-throughputCtx.Done():
				goto done
			default:
				start := time.Now()
				_, err := throughputClient.GetOrderBook(throughputCtx, testTokenID)
				if err != nil {
					throughputErrors++
				} else {
					throughputSamples = append(throughputSamples, time.Since(start))
				}
			}
		}
	done:
		throughputCancel()
		elapsed := time.Since(throughputStart)
		rps := float64(len(throughputSamples)) / elapsed.Seconds()

		fmt.Printf("   ✅ Completed %d requests in %v\n", len(throughputSamples), elapsed.Round(time.Millisecond))
		fmt.Printf("   ✅ Throughput: %.1f RPS\n", rps)
		fmt.Printf("   ✅ Errors: %d\n", throughputErrors)

		if len(throughputSamples) > 0 {
			throughputStats := calculateStats(throughputSamples)
			fmt.Println("   📊 Per-request latency:")
			printStats("Throughput", throughputStats)
		}
	} else {
		fmt.Println("   ⚠️  No token ID available for throughput test")
	}
	fmt.Println()

	// ═══════════════════════════════════════════════════════════════════
	// Summary
	// ═══════════════════════════════════════════════════════════════════
	fmt.Println("╔════════════════════════════════════════════════════════════════╗")
	fmt.Println("║                         SUMMARY                                ║")
	fmt.Println("╚════════════════════════════════════════════════════════════════╝")

	if len(restSamples) > 0 {
		fmt.Printf("   REST /book avg latency:     %v\n", restStats.Avg)
	}
	if wsMessageCount.Load() > 0 {
		fmt.Printf("   WS messages received:       %d\n", wsMessageCount.Load())
	}

	// Estimate full trade execution path
	fmt.Println()
	fmt.Println("   📈 ESTIMATED TRADE EXECUTION PATH:")
	fmt.Println("   ┌─────────────────────────────────────────────────────┐")
	fmt.Printf("   │ 1. See price (REST poll)      ~%v\n", restStats.Avg)
	fmt.Println("   │ 2. Sign order (local CPU)    ~1-5ms")
	fmt.Println("   │ 3. Submit order (network)    ~50-200ms")
	fmt.Println("   │ ─────────────────────────────────────────────────── │")
	totalEstimate := restStats.Avg + 5*time.Millisecond + 100*time.Millisecond
	fmt.Printf("   │ TOTAL ESTIMATE:               ~%v\n", totalEstimate)
	fmt.Println("   └─────────────────────────────────────────────────────┘")
}

// Helper for JSON debug output
func prettyJSON(v interface{}) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}
