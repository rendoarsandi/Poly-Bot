//go:build ignore

// measure_clob_ping benchmarks the bot's public CLOB latency path and a
// no-account end-to-end reaction path.
//
// Default mode is safe and public-only:
//   1. Raw network RTT              — GET /time
//   2. Market-data fetch RTT        — 2x GET /book for a live 2-token market
//   3. Profit detection latency     — local arb evaluation on the received books
//   4. Profit-seen → simulated exec — paper MarketBuyArb + MergeForMarket
//
// Optional authenticated probes can be enabled with -auth if real API
// credentials are configured.
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
	"sync/atomic"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/markets"
	"Market-bot/internal/paper"
	"github.com/joho/godotenv"
)

type stats struct {
	samples []time.Duration
}

func (s *stats) add(d time.Duration) { s.samples = append(s.samples, d) }

func formatDuration(d time.Duration) time.Duration {
	switch {
	case d < time.Millisecond:
		return d.Round(time.Microsecond)
	case d < time.Second:
		return d.Round(100 * time.Microsecond)
	default:
		return d.Round(time.Millisecond)
	}
}

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
		formatDuration(avg), formatDuration(min),
		formatDuration(p50), formatDuration(p95),
		formatDuration(max), len(s.samples))
}

// pingCLOB does GET /time on the CLOB using the bot's shared httpClient.
var benchRest *api.RestClient

func pingCLOB(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	err := benchRest.Ping(ctx)
	return time.Since(start), err
}

type simulatedOpportunity struct {
	levels1   []paper.MarketLevel
	levels2   []paper.MarketLevel
	shares    float64
	totalCost float64
	marginPct float64
	source    string
}

func walkCost(levels []paper.MarketLevel, shares float64) (float64, bool) {
	if shares <= 0 {
		return 0, false
	}
	remaining := shares
	total := 0.0
	for _, lv := range levels {
		if lv.Size <= 0 {
			continue
		}
		take := math.Min(remaining, lv.Size)
		total += take * lv.Price
		remaining -= take
		if remaining <= 0.000001 {
			return total, true
		}
	}
	return 0, false
}

func marginPercent(shares, totalCost float64) float64 {
	if shares <= 0 || totalCost <= 0 {
		return 0
	}
	return ((shares - totalCost) / totalCost) * 100
}

func buildSimulatedOpportunity(asks1, asks2 []paper.MarketLevel, shares, minMarginPct float64) (simulatedOpportunity, error) {
	if shares <= 0 {
		return simulatedOpportunity{}, fmt.Errorf("shares must be > 0")
	}

	if cost1, ok1 := walkCost(asks1, shares); ok1 {
		if cost2, ok2 := walkCost(asks2, shares); ok2 {
			totalCost := cost1 + cost2
			margin := marginPercent(shares, totalCost)
			if totalCost < shares && margin >= minMarginPct {
				return simulatedOpportunity{
					levels1:   asks1,
					levels2:   asks2,
					shares:    shares,
					totalCost: totalCost,
					marginPct: margin,
					source:    "live",
				}, nil
			}
		}
	}

	const syntheticPrice = 0.49
	return simulatedOpportunity{
		levels1:   []paper.MarketLevel{{Price: syntheticPrice, Size: shares}},
		levels2:   []paper.MarketLevel{{Price: syntheticPrice, Size: shares}},
		shares:    shares,
		totalCost: shares * syntheticPrice * 2,
		marginPct: marginPercent(shares, shares*syntheticPrice*2),
		source:    "synthetic",
	}, nil
}

func hasAuthCreds(cfg *core.Config) bool {
	return cfg != nil && cfg.PK != "" && cfg.APIKey != "" && cfg.APISecret != "" && cfg.APIPassphrase != ""
}

func marketLabel(m api.Market) string {
	if m.Slug != "" {
		return m.Slug
	}
	if m.MarketSlug != "" {
		return m.MarketSlug
	}
	return m.ConditionID
}

func findBenchmarkMarket(ctx context.Context, rest *api.RestClient, timeframe string) (api.Market, error) {
	if timeframe != "" {
		if tfMarkets, err := rest.GetMarketsByTimeframe(ctx, []string{"btc", "eth"}, timeframe); err == nil {
			for _, m := range tfMarkets {
				if len(m.Tokens) < 2 {
					continue
				}
				book1, err1 := rest.GetOrderBook(ctx, m.Tokens[0].TokenID)
				if err1 != nil || book1 == nil {
					continue
				}
				book2, err2 := rest.GetOrderBook(ctx, m.Tokens[1].TokenID)
				if err2 != nil || book2 == nil {
					continue
				}
				return m, nil
			}
			if len(tfMarkets) > 0 {
				return tfMarkets[0], nil
			}
		}
	}

	activeMarkets, err := rest.ListMarkets(ctx)
	if err != nil {
		return api.Market{}, err
	}
	var fallback api.Market
	checked := 0
	for _, m := range activeMarkets {
		if !m.Active || len(m.Tokens) < 2 {
			continue
		}
		if fallback.ConditionID == "" {
			fallback = m
		}
		checked++
		if checked > 100 {
			break
		}
		book1, err1 := rest.GetOrderBook(ctx, m.Tokens[0].TokenID)
		if err1 != nil || book1 == nil {
			continue
		}
		book2, err2 := rest.GetOrderBook(ctx, m.Tokens[1].TokenID)
		if err2 != nil || book2 == nil {
			continue
		}
		return m, nil
	}
	if fallback.ConditionID != "" {
		return fallback, nil
	}
	return api.Market{}, fmt.Errorf("no active 2-token market found")
}

func main() {
	godotenv.Load("../.env")
	godotenv.Load(".env")

	var samples int
	var shares float64
	var authBench bool
	flag.IntVar(&samples, "n", 10, "Number of ping samples per test")
	flag.Float64Var(&shares, "shares", 5.0, "Shares to use for public end-to-end simulation")
	flag.BoolVar(&authBench, "auth", false, "Also run authenticated CLOB order probes (requires real credentials)")
	flag.Parse()

	// Silence all log output (including [CLOB] API error lines from clob_client)
	log.SetFlags(0)
	log.SetOutput(io.Discard)

	cfg, err := core.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	benchRest = api.NewRestClient("")
	ctx := context.Background()

	fmt.Println("⏳ Finding an active 2-token market...")
	market, err := findBenchmarkMarket(ctx, benchRest, cfg.Timeframe)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to find benchmark market: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("   Using market: %s (%s / %s)\n", marketLabel(market), market.Tokens[0].Outcome, market.Tokens[1].Outcome)

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║     CLOB Latency Benchmark (Public + No-Account E2E)   ║")
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

	// ── 3. Public E2E benchmark (no account required) ──
	fmt.Printf("── 2. Public market-data + profit-seen → simulated execution (%d samples, %.2f shares) ──\n", samples, shares)
	bookFetchStats := &stats{}
	bookAgeStats := &stats{}
	detectStats := &stats{}
	signalToExecStats := &stats{}
	signalToSettleStats := &stats{}
	for i := 0; i < samples; i++ {
		fetchStart := time.Now()
		book1, err := benchRest.GetOrderBook(ctx, market.Tokens[0].TokenID)
		book1Err := err
		book2, err := benchRest.GetOrderBook(ctx, market.Tokens[1].TokenID)
		book2Err := err
		fetchDur := time.Since(fetchStart)

		bookFetchLabel := "unavailable"
		now := time.Now()
		bookAgeText := "n/a"
		asks1 := []paper.MarketLevel(nil)
		asks2 := []paper.MarketLevel(nil)
		if book1Err == nil && book2Err == nil && book1 != nil && book2 != nil {
			bookFetchStats.add(fetchDur)
			bookFetchLabel = fetchDur.Round(100 * time.Microsecond).String()
			if age1, err1 := api.OrderBookAgeAt(book1, now); err1 == nil {
				if age2, err2 := api.OrderBookAgeAt(book2, now); err2 == nil {
					oldestAge := age1
					if age2 > oldestAge {
						oldestAge = age2
					}
					bookAgeStats.add(oldestAge)
					bookAgeText = oldestAge.Round(time.Millisecond).String()
				}
			}
			asks1 = markets.LevelsToPriceDepth(book1.Asks, false)
			asks2 = markets.LevelsToPriceDepth(book2.Asks, false)
		}

		detectStart := time.Now()
		opp, err := buildSimulatedOpportunity(asks1, asks2, shares, cfg.MinMarginPercent)
		detectDur := time.Since(detectStart)
		if err != nil {
			fmt.Printf("   %2d: ❌ detect opportunity: %v\n", i+1, err)
			continue
		}
		detectStats.add(detectDur)

		engine := paper.NewEngine(1000)
		engine.SetFeeRateBps(cfg.FeeRateBps)

		signalSeenAt := time.Now()
		_, _, _, _, err = engine.MarketBuyArb(
			market.ConditionID,
			market.Tokens[0].Outcome,
			market.Tokens[1].Outcome,
			opp.shares,
			opp.levels1,
			opp.levels2,
		)
		execDoneAt := time.Now()
		if err != nil {
			fmt.Printf("   %2d: ❌ simulate execution: %v\n", i+1, err)
			continue
		}
		signalToExec := execDoneAt.Sub(signalSeenAt)
		signalToExecStats.add(signalToExec)

		_ = engine.MergeForMarket(market.ConditionID, market.Tokens[0].Outcome, market.Tokens[1].Outcome, opp.shares)
		signalToSettle := time.Since(signalSeenAt)
		signalToSettleStats.add(signalToSettle)

		fmt.Printf(
			"   %2d: books=%v age=%s detect=%v profit→exec=%v profit→settle=%v [%s, margin=%.2f%%]\n",
			i+1,
			bookFetchLabel,
			bookAgeText,
			detectDur.Round(time.Microsecond),
			signalToExec.Round(time.Microsecond),
			signalToSettle.Round(time.Microsecond),
			opp.source,
			opp.marginPct,
		)
		time.Sleep(150 * time.Millisecond)
	}
	fmt.Printf("   📊 Book fetch RTT:          %s\n", bookFetchStats.report())
	fmt.Printf("   📊 Order-book age:          %s\n", bookAgeStats.report())
	fmt.Printf("   📊 Profit detection:        %s\n", detectStats.report())
	fmt.Printf("   📊 Profit-seen → execute:   %s\n", signalToExecStats.report())
	fmt.Printf("   📊 Profit-seen → settle:    %s\n", signalToSettleStats.report())
	fmt.Println()

	var authGetStats, orderStats, batchStats *stats
	if authBench {
		if !hasAuthCreds(cfg) {
			fmt.Println("── 3. Authenticated probes skipped: missing API credentials ──")
			fmt.Println()
		} else {
			clob, err := api.NewCLOBClient(cfg.PK, cfg.APIKey, cfg.APISecret, cfg.APIPassphrase)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Failed to initialize authenticated CLOB client: %v\n", err)
				os.Exit(1)
			}

			fmt.Printf("── 3. Authenticated GET: /balance-allowance (%d samples) ──\n", samples)
			authGetStats = &stats{}
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

			fmt.Printf("── 4. Real POST /order: single order (%d samples) ──\n", samples)
			fmt.Println("   ℹ️  Uses price=$0.01 — orders will be KILLED/rejected (harmless)")
			clob.SetTestMode(false)
			orderStats = &stats{}
			for i := 0; i < samples; i++ {
				req := &api.OrderRequest{
					TokenID:     market.Tokens[0].TokenID,
					Price:       0.01,
					Size:        5.0,
					Side:        api.SideBuy,
					OrderType:   api.OrderTypeMarket,
					TimeInForce: api.TIFFillAndKill,
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
				time.Sleep(200 * time.Millisecond)
			}
			fmt.Printf("   📊 %s\n\n", orderStats.report())

			fmt.Printf("── 5. Batch POST /orders: Atomic execution of 2 legs (%d samples) ──\n", samples)
			fmt.Println("   ℹ️  This is what the bot uses for synchronized panic buys & split sells")
			batchStats = &stats{}
			var errCount int64
			for i := 0; i < samples; i++ {
				reqs := []*api.OrderRequest{
					{
						TokenID: market.Tokens[0].TokenID, Price: 0.01, Size: 5.0,
						Side: api.SideBuy, OrderType: api.OrderTypeMarket,
						TimeInForce: api.TIFFillAndKill, FeeRateBps: 100,
					},
					{
						TokenID: market.Tokens[1].TokenID, Price: 0.01, Size: 5.0,
						Side: api.SideBuy, OrderType: api.OrderTypeMarket,
						TimeInForce: api.TIFFillAndKill, FeeRateBps: 100,
					},
				}

				s := time.Now()
				_, err := clob.PlaceOrders(ctx, reqs)
				total := time.Since(s)

				if err != nil {
					atomic.AddInt64(&errCount, 1)
				}

				batchStats.add(total)
				fmt.Printf("   %2d: total=%v\n", i+1, total.Round(100*time.Microsecond))
				time.Sleep(300 * time.Millisecond)
			}
			if errCount > 0 {
				fmt.Printf("   ⚠️  %d errors encountered — results may not reflect real latency\n", errCount)
			}
			fmt.Printf("   📊 Batch RTT: %s\n\n", batchStats.report())
		}
	} else {
		fmt.Println("── 3. Authenticated probes skipped (use -auth to enable) ──")
		fmt.Println()
	}

	// ── Summary ──
	fmt.Println("══════════════════════════════════════════════════════════")
	fmt.Println("SUMMARY — What your bot actually experiences:")
	fmt.Println("──────────────────────────────────────────────────────────")
	fmt.Printf("  Raw network RTT:          %s\n", rawStats.report())
	fmt.Printf("  Public book fetch RTT:    %s\n", bookFetchStats.report())
	fmt.Printf("  Order-book age:           %s\n", bookAgeStats.report())
	fmt.Printf("  Profit detection:         %s\n", detectStats.report())
	fmt.Printf("  Profit-seen → execute:    %s\n", signalToExecStats.report())
	fmt.Printf("  Profit-seen → settle:     %s\n", signalToSettleStats.report())
	if authGetStats != nil {
		fmt.Printf("  Auth GET (balance):       %s\n", authGetStats.report())
	}
	if orderStats != nil {
		fmt.Printf("  Single POST /order:       %s\n", orderStats.report())
	}
	if batchStats != nil {
		fmt.Printf("  Batch POST /orders (2x):  %s\n", batchStats.report())
	}
	fmt.Println("──────────────────────────────────────────────────────────")
	if orderStats != nil && len(orderStats.samples) > 0 && len(rawStats.samples) > 0 {
		sort.Slice(orderStats.samples, func(i, j int) bool { return orderStats.samples[i] < orderStats.samples[j] })
		sort.Slice(rawStats.samples, func(i, j int) bool { return rawStats.samples[i] < rawStats.samples[j] })
		overhead := orderStats.samples[len(orderStats.samples)/2] - rawStats.samples[len(rawStats.samples)/2]
		fmt.Printf("  Estimated signing + server processing overhead: ~%v\n", overhead.Round(100*time.Microsecond))
	}
	fmt.Println("══════════════════════════════════════════════════════════")
}
