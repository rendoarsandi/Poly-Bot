package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
)

func main() {
	var samples int
	flag.IntVar(&samples, "n", 10, "Number of ping samples")
	flag.Parse()

	// Disable default log output to reduce noise
	log.SetFlags(0)
	log.SetOutput(os.Stderr)

	cfg, err := core.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\\n", err)
		os.Exit(1)
	}

	clob, err := api.NewCLOBClient(cfg.PK, cfg.APIKey, cfg.APISecret, cfg.APIPassphrase)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize CLOB client: %v\\n", err)
		os.Exit(1)
	}

	// Run in test mode (offchain signing + auth ping only)
	clob.SetTestMode(true)

	// Suppress noisy prints from the internal packages by temporarily redirecting stdout
	oldStdout := os.Stdout
	null, _ := os.Open(os.DevNull)

	ctx := context.Background()
	tokenID := "21742633143463906290569050155826241533067272736897614950488156847949938836455"

	var totalLatency time.Duration
	var successful int

	fmt.Printf("🚀 Measuring CLOB Offchain + Ping Latency (%d samples)...\\n", samples)
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
			fmt.Printf("Attempt %d: ❌ Error: %v (Latency: %v)\\n", i, err, latency)
		} else {
			fmt.Printf("Attempt %d: ✅ Success | Latency: %v\\n", i, latency)
			totalLatency += latency
			successful++
		}

		// Wait briefly before next ping
		time.Sleep(200 * time.Millisecond)
	}

	fmt.Println("--------------------------------------------------")
	if successful > 0 {
		avg := totalLatency / time.Duration(successful)
		fmt.Printf("📊 Average Ping Latency: %v\\n", avg)
	} else {
		fmt.Println("📊 Average Ping Latency: N/A (all requests failed)")
	}
}
