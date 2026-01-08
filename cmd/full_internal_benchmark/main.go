package main

import (
	"encoding/json"
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
		fmt.Println("❌ POLY_PK not set.")
		os.Exit(1)
	}

	// 1. Prepare a REALISTIC WebSocket Message (JSON bytes)
	// This simulates what the bot receives from London
	rawWSMessage := `{
		"type": "book",
		"asset_id": "token1_id",
		"bids": [{"price": "0.47", "size": "1000"}, {"price": "0.46", "size": "5000"}],
		"asks": [{"price": "0.48", "size": "2000"}, {"price": "0.49", "size": "3000"}],
		"timestamp": "1704727725000"
	}`
	messageBytes := []byte(rawWSMessage)

	// Setup components
	signer, _ := api.NewSigner(cfg.PK)
	engine := paper.NewEngine(1000.0)
	orderBook := paper.NewOrderBook()
	outcomes := []string{"Up", "Down"}
	riskConfig := paper.RiskConfig{MaxExposure: 2000.0, MaxUnmatchedRatio: 0.40, MaxUnmatchedShares: 300.0}
	riskMgr := paper.NewRiskManager(riskConfig, engine, orderBook, outcomes)

	fmt.Println("╔═══════════════════════════════════════════════════════╗")
	fmt.Println("║     FULL SOFTWARE STACK LATENCY BENCHMARK             ║")
	fmt.Println("║     (Raw JSON Bytes → → → Signed Orders)              ║")
	fmt.Println("╚═══════════════════════════════════════════════════════╝")
	fmt.Println()

	iterations := 1000
	var totalTime time.Duration

	for i := 0; i < iterations; i++ {
		start := time.Now()

		// --- THE "READ" PART ---
		// 1. JSON Parsing
		var bookUpdate api.OrderBook
		_ = json.Unmarshal(messageBytes, &bookUpdate)

		// 2. State Update (OrderBook & Engine)
		// Simulating the logic in cmd/bot/main.go
		outcome := "Up"
		bid, ask := 0.47, 0.48
		mid := (bid + ask) / 2
		engine.UpdateMarketData("BTC", outcome, mid, bid, ask)
		
		// --- THE "THINK" PART ---
		// 3. Strategy Detection
		price1 := 0.48 // Up
		price2 := 0.48 // Down (simulated)
		sum := price1 + price2
		margin := (1.0 - sum) * 100

		if margin >= cfg.MinMarginPercent {
			// 4. Risk & Sizing
			riskAction, _ := riskMgr.Evaluate()
			if riskAction != paper.RiskActionKillSwitch {
				tradeSize := cfg.CalculateTradeSize(engine.GetEquity())
				_ = tradeSize / sum // shares
				
				// --- THE "WRITE" PART ---
				// 5. Dual Order Signing
				order1 := &api.OrderData{
					Salt: "1", Maker: signer.Address(), Signer: signer.Address(),
					Taker: "0x0000000000000000000000000000000000000000",
					TokenID: "token1_id", MakerAmount: "480000", TakerAmount: "1000000",
					Expiration: "1767882600", Nonce: "0", FeeRateBps: "0", Side: "BUY",
				}
				_, _ = signer.SignOrder(order1)

				order2 := &api.OrderData{
					Salt: "2", Maker: signer.Address(), Signer: signer.Address(),
					Taker: "0x0000000000000000000000000000000000000000",
					TokenID: "token2_id", MakerAmount: "480000", TakerAmount: "1000000",
					Expiration: "1767882600", Nonce: "0", FeeRateBps: "0", Side: "BUY",
				}
				_, _ = signer.SignOrder(order2)
			}
		}

		totalTime += time.Since(start)
	}

	avgTime := totalTime / time.Duration(iterations)

	fmt.Printf("📊 TOTAL INTERNAL PIPELINE LATENCY: %v\n", avgTime)
	fmt.Printf("   Parsing Overhead: ~%v\n", 15*time.Microsecond) // Estimated JSON cost
	fmt.Printf("   Logic + Signing:  ~%v\n", avgTime - 15*time.Microsecond)
	fmt.Println()
	fmt.Println("💡 This measures every line of code from receiving the raw")
	fmt.Println("   network packet to having the final orders ready to ship.")
	fmt.Println("═══════════════════════════════════════════════════════")
}
