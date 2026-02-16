package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"github.com/joho/godotenv"
)

func main() {
	// Load .env
	_ = godotenv.Load()

	cfg, err := core.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	if cfg.TradingMode != core.ModeReal {
		log.Fatal("This script requires TRADING_MODE=real in .env to invoke authenticated endpoints.")
	}

	clob, err := api.NewCLOBClient(cfg.PK, cfg.APIKey, cfg.APISecret, cfg.APIPassphrase)
	if err != nil {
		log.Fatalf("Failed to create CLOB client: %v", err)
	}

	fmt.Println("🚀 Testing '/positions' endpoint latency...")
	fmt.Println("--------------------------------------------------")

	ctx := context.Background()
	samples := 10
	var total time.Duration

	for i := 1; i <= samples; i++ {
		start := time.Now()
		positions, err := clob.GetPositions(ctx)
		duration := time.Since(start)

		if err != nil {
			fmt.Printf("Attempt %d: ❌ Error: %v (Latency: %v)\n", i, err, duration)
		} else {
			count := len(positions)
			fmt.Printf("Attempt %d: ✅ Success (%d positions) | Latency: %v\n", i, count, duration)
			total += duration
		}

		time.Sleep(500 * time.Millisecond)
	}

	avg := total / time.Duration(samples)
	fmt.Println("--------------------------------------------------")
	fmt.Printf("📊 Average Latency: %v\n", avg)
}
