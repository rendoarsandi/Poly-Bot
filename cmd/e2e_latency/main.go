package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
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
	fmt.Println("║     BOT INTERNAL E2E LATENCY BENCHMARK                ║")
	fmt.Println("║     (From Price Detection to Signed Orders)           ║")
	fmt.Println("╚═══════════════════════════════════════════════════════╝")
	fmt.Println()

	// Setup components
	signer, _ := api.NewSigner(cfg.PK)
	engine := paper.NewEngine(1000.0)
	orderBook := paper.NewOrderBook()
	outcomes := []string{"Up", "Down"}
	
	riskConfig := paper.RiskConfig{
		MaxExposure:        2000.0,
		MaxUnmatchedRatio:  0.40,
		MaxUnmatchedShares: 300.0,
		SkewThreshold:      0.30,
		KillSwitchDrawdown: 999.0,
	}
	riskMgr := paper.NewRiskManager(riskConfig, engine, orderBook, outcomes)

	// Mock Price Update
	// Outcome1 Ask: 0.48, Outcome2 Ask: 0.48 (Sum: 0.96 = 4% Arb)
	price1 := 0.48
	price2 := 0.48

	fmt.Println("🏃 Running 1000 simulated arb detections...")
	
	var totalDuration time.Duration
	iterations := 1000

	for i := 0; i < iterations; i++ {
		start := time.Now()

		// 1. STRATEGY: Detection Logic
		sum := price1 + price2
		margin := (1.0 - sum) * 100
		
		if margin >= cfg.MinMarginPercent {
			// 2. RISK: Evaluate Risk
			riskAction, _ := riskMgr.Evaluate()
			
			if riskAction != paper.RiskActionKillSwitch {
				// 3. SIZING: Calculate Shares
				currentEquity := engine.GetEquity()
				tradeSize := cfg.CalculateTradeSize(currentEquity)
				_ = tradeSize / sum // shares

				// 4. SIGNING: Generate BOTH signed orders (Side 1 and Side 2)
				// Order 1
				order1 := &api.OrderData{
					Salt: "1", Maker: signer.Address(), Signer: signer.Address(),
					Taker: "0x0000000000000000000000000000000000000000",
					TokenID: "token1_id", MakerAmount: "480000", TakerAmount: "1000000",
					Expiration: "1767882600", Nonce: "0", FeeRateBps: "0", Side: "BUY",
				}
				_, _ = signer.SignOrder(order1)

				// Order 2
				order2 := &api.OrderData{
					Salt: "2", Maker: signer.Address(), Signer: signer.Address(),
					Taker: "0x0000000000000000000000000000000000000000",
					TokenID: "token2_id", MakerAmount: "480000", TakerAmount: "1000000",
					Expiration: "1767882600", Nonce: "0", FeeRateBps: "0", Side: "BUY",
				}
				_, _ = signer.SignOrder(order2)
			}
		}
		
		totalDuration += time.Since(start)
	}

	avgDuration := totalDuration / time.Duration(iterations)

	fmt.Println()
	fmt.Println("📊 INTERNAL PERFORMANCE RESULTS:")
	fmt.Println("───────────────────────────────────────────────────────")
	fmt.Printf("   Average Cycle Time:    %v\n", avgDuration)
	fmt.Printf("   Cycles per Second:     %.0f ops/sec\n", 1.0/avgDuration.Seconds())
	fmt.Println("───────────────────────────────────────────────────────")
	fmt.Println()
	fmt.Println("💡 This is the time it takes the bot to \"think\" and prepare")
	fmt.Println("   the crypto signatures before hitting the network cable.")
	fmt.Println("═══════════════════════════════════════════════════════")
}
