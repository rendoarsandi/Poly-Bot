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
	"io"
	"log"
	"math"
	"os"
	"sort"
	"sync"
	"sync/atomic"
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

// pingCLOB does GET /time on the CLOB using the bot's shared httpClient
// via RestClient.PingTime(). Lightest possible authenticated round-trip.
var benchRest *api.RestClient

func pingCLOB(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	err := benchRest.Ping(ctx)
	return time.Since(start), err
}

func main() {
	godotenv.Load("../.env")
	godotenv.Load(".env")

	var samples int
	flag.IntVar(&samples, "n", 10, "Number of ping samples per test")
	flag.Parse()

	// Silence all log output (including [CLOB] API error lines from clob_client)
	log.SetFlags(0)
	log.SetOutput(io.Discard)

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

	benchRest = api.NewRestClient("")
	ctx := context.Background()

	// Fetch a live token ID from an active market
	fmt.Println("⏳ Finding an active market token...")
	tokenID := ""
	markets, err := benchRest.ListMarkets(ctx)
	if err == nil {
		for _, m := range markets {
			if m.Active && !m.Closed && len(m.Tokens) >= 2 {
				tokenID = m.Tokens[0].TokenID
				fmt.Printf("   Using token from market: %s\n", m.Slug)
				break
			}
		}
	}
	if tokenID == "" {
		fmt.Println("   ⚠️  Could not find active market, using fallback token (orders will 400 but latency is still valid)")
		tokenID = "21742633143463906290569050155826241533067272736897614950488156847949938836455"
	}

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║        CLOB Latency Benchmark (Real Execution Path)     ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
	fmt.Println()

	// ── 1. Warm up connections (3 throwaway requests) ──
	fmt.Println("⏳ Warming up HTTP connection pool...")
	var warmupErrs int
	for i := 0; i < 3; i++ {
		if _, err := pingCLOB(ctx); err != nil {
			warmupErrs++
		}
	}
	if warmupErrs > 0 {
		fmt.Fprintf(os.Stderr, "   ⚠️  %d/%d warm-up pings failed — check network/auth\n", warmupErrs, 3)
	}
	fmt.Println()

	// ── 2. Raw Network RTT ──
	fmt.Printf("── 1. Raw Network RTT: GET /time (%d samples) ──\n", samples)
	rawStats := &stats{}
	for i := 0; i < samples; i++ {
		d, err := pingCLOB(ctx)
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
	for i := 0; i < samples; i++ {
		req := &api.OrderRequest{
			TokenID: tokenID, Price: 0.50, Size: 10.0,
			Side: api.SideBuy, FeeRateBps: 100,
		}
		start := time.Now()
		_, err := clob.PlaceOrder(ctx, req)
		d := time.Since(start)
		if err != nil {
			fmt.Printf("   %2d: ❌ %v\n", i+1, err)
		} else {
			signStats.add(d)
			fmt.Printf("   %2d: %v (sign + balance check)\n", i+1, d.Round(100*time.Microsecond))
		}
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
		start := time.Now()
		resp, err := clob.PlaceOrder(ctx, req)
		d := time.Since(start)

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
	var errCount int64
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
			_, err := clob.PlaceOrder(ctx, req)
			d1 = time.Since(s)
			if err != nil {
				atomic.AddInt64(&errCount, 1)
			}
		}()
		go func() {
			defer wg.Done()
			req := &api.OrderRequest{
				TokenID: tokenID, Price: 0.01, Size: 5.0,
				Side: api.SideBuy, OrderType: api.OrderTypeMarket,
				TimeInForce: api.TIFImmediateOrCancel, FeeRateBps: 100,
			}
			s := time.Now()
			_, err := clob.PlaceOrder(ctx, req)
			d2 = time.Since(s)
			if err != nil {
				atomic.AddInt64(&errCount, 1)
			}
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
	if errCount > 0 {
		fmt.Printf("   ⚠️  %d errors encountered — results may not reflect real latency\n", errCount)
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