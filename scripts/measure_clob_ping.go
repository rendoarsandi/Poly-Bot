//go:build ignore

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"github.com/joho/godotenv"
)

func main() {
	godotenv.Load("../.env")
	godotenv.Load(".env")

	var samples int
	flag.IntVar(&samples, "n", 10, "Number of ping samples")
	flag.Parse()

	// Disable default log output to reduce noise
	log.SetFlags(0)
	log.SetOutput(os.Stderr)

	fmt.Printf("🚀 Measuring Raw Network Ping (%d samples)...\n", samples)
	fmt.Println("--------------------------------------------------")
	
	client := &http.Client{Timeout: 5 * time.Second}
	var totalRaw time.Duration
	var rawSuccess int

	for i := 1; i <= samples; i++ {
		start := time.Now()
		resp, err := client.Get("https://clob.polymarket.com/time")
		latency := time.Since(start)

		if err != nil {
			fmt.Printf("Raw Ping %d: ❌ Error: %v\n", i, err)
		} else {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				fmt.Printf("Raw Ping %d: ✅ %v\n", i, latency)
				totalRaw += latency
				rawSuccess++
			} else {
				fmt.Printf("Raw Ping %d: ❌ Bad Status %d\n", i, resp.StatusCode)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	if rawSuccess > 0 {
		fmt.Printf("📊 Average Raw Ping: %v\n\n", totalRaw/time.Duration(rawSuccess))
	} else {
		fmt.Printf("📊 Average Raw Ping: N/A\n\n")
	}

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

	// Run in test mode (offchain signing + auth ping only)
	clob.SetTestMode(true)

	// Suppress noisy prints from the internal packages by temporarily redirecting stdout
	oldStdout := os.Stdout
	null, _ := os.Open(os.DevNull)

	ctx := context.Background()
	tokenID := "21742633143463906290569050155826241533067272736897614950488156847949938836455"

	var totalTrade time.Duration
	var tradeSuccess int

	fmt.Printf("🚀 Measuring Trade-Ready Ping (Signing + Auth) (%d samples)...\n", samples)
	fmt.Println("--------------------------------------------------")

	for i := 1; i <= samples; i++ {
		req := &api.OrderRequest{
			TokenID: tokenID,
			Price:   0.01,
			Size:    5.0,
			Side:    api.SideBuy, // Alternate to SideSell if you want, but Buy is fine for latency test
		}

		os.Stdout = null // Redirect stdout to /dev/null before PlaceOrder
		start := time.Now()
		_, err := clob.PlaceOrder(ctx, req)
		latency := time.Since(start)
		os.Stdout = oldStdout // Restore stdout

		if err != nil {
			fmt.Printf("Trade Ping %d: ❌ Error: %v (Latency: %v)\n", i, err, latency)
		} else {
			fmt.Printf("Trade Ping %d: ✅ %v\n", i, latency)
			totalTrade += latency
			tradeSuccess++
		}

		// Wait briefly before next ping
		time.Sleep(200 * time.Millisecond)
	}

	fmt.Println("--------------------------------------------------")
	if tradeSuccess > 0 {
		fmt.Printf("📊 Average Trade Ping: %v\n", totalTrade/time.Duration(tradeSuccess))
	} else {
		fmt.Println("📊 Average Trade Ping: N/A (all requests failed)")
	}
}