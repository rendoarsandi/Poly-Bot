package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
)

func main() {
	cfg, err := core.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	if cfg.PK == "" {
		fmt.Println("❌ POLY_PK not set. Please set it to run latency tests.")
		os.Exit(1)
	}

	fmt.Println("╔═══════════════════════════════════════════════════════╗")
	fmt.Println("║     POLYMARKET LATENCY DIAGNOSTIC                     ║")
	fmt.Println("╚═══════════════════════════════════════════════════════╝")
	fmt.Println()

	// 1. Measure Signing Latency (Local CPU)
	fmt.Println("🧪 1. MEASURING EIP-712 SIGNING SPEED...")
	signer, err := api.NewSigner(cfg.PK)
	if err != nil {
		log.Fatalf("Failed to create signer: %v", err)
	}

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
		Side:          "BUY",
		SignatureType: 0,
	}

	var totalSignTime time.Duration
	iterations := 50
	for i := 0; i < iterations; i++ {
		start := time.Now()
		_, err := signer.SignOrder(testOrder)
		if err != nil {
			log.Fatalf("Sign error: %v", err)
		}
		totalSignTime += time.Since(start)
	}
	avgSignTime := totalSignTime / time.Duration(iterations)
	fmt.Printf("   ✅ Average Sign Time: %v (over %d runs)\n", avgSignTime, iterations)
	fmt.Println()

	// 2. Measure API Round-trip Latency
	fmt.Println("🧪 2. MEASURING API ROUND-TRIP LATENCY (GET)...")
	restClient := api.NewRestClient("")
	ctx := context.Background()

	var totalGetTime time.Duration
	getIterations := 10
	for i := 0; i < getIterations; i++ {
		start := time.Now()
		_, err := restClient.Get15mMarkets(ctx, []string{"btc"})
		if err != nil {
			fmt.Printf("   ⚠️  API Error: %v\n", err)
			continue
		}
		totalGetTime += time.Since(start)
		time.Sleep(200 * time.Millisecond) // Don't spam
	}
	avgGetTime := totalGetTime / time.Duration(getIterations)
	fmt.Printf("   ✅ Average GET Latency: %v\n", avgGetTime)
	fmt.Println()

	// 3. Measure Authenticated POST Latency (Mock Order path)
	fmt.Println("🧪 3. MEASURING AUTHENTICATED REQUEST LATENCY (L2 Signature)...")
	auth := &api.APIAuth{
		APIKey:     cfg.APIKey,
		APISecret:  cfg.APISecret,
		Passphrase: cfg.APIPassphrase,
	}
	
	var totalAuthTime time.Duration
	for i := 0; i < 10; i++ {
		start := time.Now()
		// Sign an L2 request (this is what submitOrder does)
		auth.SignL2Request("GET", "/orders", "")
		totalAuthTime += time.Since(start)
	}
	avgAuthTime := totalAuthTime / 10
	fmt.Printf("   ✅ Average L2 Auth Sign Time: %v\n", avgAuthTime)
	fmt.Println()

	fmt.Println("═══════════════════════════════════════════════════════")
	totalPathLatency := avgSignTime + avgAuthTime + avgGetTime
	fmt.Printf("🚀 ESTIMATED MINIMUM EXECUTION LATENCY: %v\n", totalPathLatency)
	fmt.Println("   (Sign + Auth + Network Roundtrip)")
	fmt.Println("═══════════════════════════════════════════════════════")
}
