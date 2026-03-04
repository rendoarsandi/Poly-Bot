//go:build ignore

// measure_clob_ping measures the REAL latency the bot experiences during trading.
//
// It benchmarks each layer separately so you can see where time goes:
//   1. Raw network RTT        — GET /time (same httpClient as the bot)
//   2. Signing overhead        — EIP-712 sign + HMAC auth (CPU only, no network)
//   3. Authenticated GET       — GET /balance-allowance (auth header round-trip)
//   4. Real order POST         — POST /order at $0.01 price (will be rejected/killed, safe)
//   5. Parallel order POST     — 2x POST /order simultaneously (simulates panic buy)
//
// Usage: go run scripts/measure_clob_ping.go -n 10

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"sync"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"github.com/joho/godotenv"
)

type stats struct {
	samples []time.Duration
}

func (s *stats) add(d time.Duration) { s.samples = append(s.samples, d) }

func (s *stats) report() string {
	if len(s.samples) == 0 {
		return "N/A"
	}
	sort.Slice(s.samples, func(i, j int) bool { return s.samples[i] < s.samples[j] })
	var sum time.Duration
	for _, d := range s.samples {
		sum += d
	}
	avg := sum / time.Duration(len(s.samples))
	min := s.samples[0]
	max := s.samples[len(s.samples)-1]
	p50 := s.samples[len(s.samples)/2]
	p95idx := int(math.Ceil(float64(len(s.samples))*0.95)) - 1
	if p95idx < 0 {
		p95idx = 0
	}
	p95 := s.samples[p95idx]
	return fmt.Sprintf("avg=%v  min=%v  p50=%v  p95=%v  max=%v  (%d samples)",
		avg.Round(100*time.Microsecond), min.Round(100*time.Microsecond),
		p50.Round(100*time.Microsecond), p95.Round(100*time.Microsecond),
		max.Round(100*time.Microsecond), len(s.samples))
}

func main() {
	godotenv.Load("../.env")
	godotenv.Load(".env")

	var samples int
	flag.IntVar(&samples, "n", 10, "Number of ping samples per test")
	flag.Parse()

	log.SetFlags(0)
	log.SetOutput(os.Stderr)

	cfg, err := core.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	clob, err := api.NewCLOBClient(cfg.PK, cfg.APIKey, cfg.APISecret, cfg.APIPassphrase)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize CLOB client: %v\n", err)
		os.Exit(1)
	}

	rest := api.NewRestClient("")
	ctx := context.Background()

	// Use a real active token — price $0.01 ensures the order is harmless
	tokenID := "21742633143463906290569050155826241533067272736897614950488156847949938836455"

	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║        CLOB Latency Benchmark (Real Execution Path)     ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
	fmt.Println()

	// ── 1. Warm up connections (2 throwaway requests) ──
	fmt.Println("⏳ Warming up HTTP connection pool...")
	for i := 0; i < 2; i++ {
		_, _ = rest.GetOrderBook(ctx, tokenID)
	}
	fmt.Println()

	// ── 2. Raw Network RTT (uses same shared httpClient as the bot) ──
	fmt.Printf("── 1. Raw Network RTT: GET /time (%d samples) ──\n", samples)
	rawStats := &stats{}
	for i := 0; i < samples; i++ {
		start := time.Now()
		_, err := rest.GetOrderBook(ctx, tokenID)
		d := time.Since(start)
		if err != nil {
			fmt.Printf("   %2d: ❌ %v\n", i+1, err)
		} else {
			rawStats.add(d)
			fmt.Printf("   %2d: %v\n", i+1, d.Round(100*time.Microsecond))
		}
	}
	fmt.Printf("   📊 %s\n\n", rawStats.report())

	// ── 3. Signing overhead (CPU only — no network) ──
	fmt.Printf("── 2. EIP-712 Signing Overhead: CPU only (%d samples) ──\n", samples)
	signStats := &stats{}
	clob.SetTestMode(true)
	// Suppress stdout from internal packages
	oldStdout := os.Stdout
	null, _ := os.Open(os.DevNull)
	for i := 0; i < samples; i++ {
		req := &api.OrderRequest{
			TokenID: tokenID, Price: 0.50, Size: 10.0,
			Side: api.SideBuy, FeeRateBps: 100,
		}
		// In test mode PlaceOrder does: salt + amount calc + EIP-712 sign +
		// HMAC auth + JSON marshal + build HTTP request + GetBalanceAllowance.
		// We want ONLY the signing part, so we measure a tight loop of just
		// the signing by calling PlaceOrder with a disconnected context that
		// will fail on the network call, giving us just the CPU time.
		//
		// Actually, test mode returns before any network call if we give it
		// a context that already expired for the balance check. But that
		// changes the codepath. Instead, just measure the full test-mode
		// PlaceOrder (which includes one GET /balance-allowance) and subtract
		// the raw RTT later to isolate signing.
		os.Stdout = null
		start := time.Now()
		_, _ = clob.PlaceOrder(ctx, req)
		d := time.Since(start)
		os.Stdout = oldStdout
		signStats.add(d)
		fmt.Printf("   %2d: %v (sign + balance check)\n", i+1, d.Round(100*time.Microsecond))
	}
	fmt.Printf("   📊 %s\n", signStats.report())
	fmt.Println("   ℹ️  This includes 1x GET /balance-allowance. Subtract raw RTT for pure signing cost.")
	fmt.Println()

	// ── 4. Authenticated GET (what background balance refresh does) ──
	fmt.Printf("── 3. Authenticated GET: /balance-allowance (%d samples) ──\n", samples)
	authGetStats := &stats{}
	for i := 0; i < samples; i++ {
		start := time.Now()
		_, err := clob.GetBalanceAllowance(ctx)
		d := time.Since(start)
		if err != nil {
			fmt.Printf("   %2d: ❌ %v\n", i+1, err)
		} else {
			authGetStats.add(d)
			fmt.Printf("   %2d: %v\n", i+1, d.Round(100*time.Microsecond))
		}
	}
	fmt.Printf("   📊 %s\n\n", authGetStats.report())

	// ── 5. Real POST /order (what the bot does to place an order) ──
	fmt.Printf("── 4. Real POST /order: single order (%d samples) ──\n", samples)
	fmt.Println("   ℹ️  Uses price=$0.01 — orders will be KILLED/rejected (harmless)")
	clob.SetTestMode(false) // REAL mode
	orderStats := &stats{}
	for i := 0; i < samples; i++ {
		req := &api.OrderRequest{
			TokenID:     tokenID,
			Price:       0.01,
			Size:        5.0,
			Side:        api.SideBuy,
			OrderType:   api.OrderTypeMarket,
			TimeInForce: api.TIFImmediateOrCancel,
			FeeRateBps:  100,
		}
		os.Stdout = null
		start := time.Now()
		resp, err := clob.PlaceOrder(ctx, req)
		d := time.Since(start)
		os.Stdout = oldStdout

		status := "?"
		if err != nil {
			status = fmt.Sprintf("err: %v", err)
		} else if resp != nil {
			status = resp.Status
			if resp.ErrorMsg != "" {
				status += " (" + resp.ErrorMsg + ")"
			}
		}
		orderStats.add(d)
		fmt.Printf("   %2d: %v  [%s]\n", i+1, d.Round(100*time.Microsecond), status)
		time.Sleep(200 * time.Millisecond) // Avoid rate limit
	}
	fmt.Printf("   📊 %s\n\n", orderStats.report())

	// ── 6. Parallel POST /order (simulates panic buy — 2 orders at once) ──
	fmt.Printf("── 5. Parallel POST /order: 2 simultaneous orders (%d samples) ──\n", samples)
	fmt.Println("   ℹ️  This is what panic buy actually does — both legs at once")
	parallelStats := &stats{}
	leg1Stats := &stats{}
	leg2Stats := &stats{}
	for i := 0; i < samples; i++ {
		var d1, d2 time.Duration
		var wg sync.WaitGroup
		wg.Add(2)

		overallStart := time.Now()
		go func() {
			defer wg.Done()
			req := &api.OrderRequest{
				TokenID: tokenID, Price: 0.01, Size: 5.0,
				Side: api.SideBuy, OrderType: api.OrderTypeMarket,
				TimeInForce: api.TIFImmediateOrCancel, FeeRateBps: 100,
			}
			s := time.Now()
			os.Stdout = null
			_, _ = clob.PlaceOrder(ctx, req)
			os.Stdout = oldStdout
			d1 = time.Since(s)
		}()
		go func() {
			defer wg.Done()
			req := &api.OrderRequest{
				TokenID: tokenID, Price: 0.01, Size: 5.0,
				Side: api.SideBuy, OrderType: api.OrderTypeMarket,
				TimeInForce: api.TIFImmediateOrCancel, FeeRateBps: 100,
			}
			s := time.Now()
			os.Stdout = null
			_, _ = clob.PlaceOrder(ctx, req)
			os.Stdout = oldStdout
			d2 = time.Since(s)
		}()
		wg.Wait()
		total := time.Since(overallStart)

		parallelStats.add(total)
		leg1Stats.add(d1)
		leg2Stats.add(d2)
		fmt.Printf("   %2d: total=%v  leg1=%v  leg2=%v\n", i+1,
			total.Round(100*time.Microsecond),
			d1.Round(100*time.Microsecond),
			d2.Round(100*time.Microsecond))
		time.Sleep(300 * time.Millisecond)
	}
	fmt.Printf("   📊 Wall clock: %s\n", parallelStats.report())
	fmt.Printf("   📊 Leg 1:      %s\n", leg1Stats.report())
	fmt.Printf("   📊 Leg 2:      %s\n\n", leg2Stats.report())

	// ── Summary ──
	fmt.Println("══════════════════════════════════════════════════════════")
	fmt.Println("SUMMARY — What your bot actually experiences:")
	fmt.Println("──────────────────────────────────────────────────────────")
	fmt.Printf("  Raw network RTT:          %s\n", rawStats.report())
	fmt.Printf("  Auth GET (balance):       %s\n", authGetStats.report())
	fmt.Printf("  Single POST /order:       %s\n", orderStats.report())
	fmt.Printf("  Parallel 2x POST /order:  %s\n", parallelStats.report())
	fmt.Println("──────────────────────────────────────────────────────────")
	if len(orderStats.samples) > 0 && len(rawStats.samples) > 0 {
		sort.Slice(orderStats.samples, func(i, j int) bool { return orderStats.samples[i] < orderStats.samples[j] })
		sort.Slice(rawStats.samples, func(i, j int) bool { return rawStats.samples[i] < rawStats.samples[j] })
		overhead := orderStats.samples[len(orderStats.samples)/2] - rawStats.samples[len(rawStats.samples)/2]
		fmt.Printf("  Estimated signing + server processing overhead: ~%v\n", overhead.Round(100*time.Microsecond))
	}
	fmt.Println("══════════════════════════════════════════════════════════")
}