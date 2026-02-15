package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/big"
	"strings"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/trading"
	"github.com/joho/godotenv"
)

func main() {
	godotenv.Load()
	cfg, err := core.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	trader, err := trading.NewRealTrader(cfg)
	if err != nil {
		log.Fatalf("Failed to create trader: %v", err)
	}

	ctx := context.Background()
	rest := api.NewRestClient("")
	polygon := api.NewPolygonClient(cfg.PolygonRPCURL)
	address := trader.Address()

	fmt.Println("🚀 POLYARB SMART SCANNER (Merge & Redeem Only)")
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Printf("🔑 Wallet: %s\n", address)

	// 1. Smart Discovery: Find all recent 15m markets
	fmt.Println("🔍 Scanning for all recent 15m markets (including closed)...")
	markets, err := rest.Get15mMarkets(ctx, nil)
	if err != nil {
		log.Fatalf("Failed to fetch markets: %v", err)
	}

	foundAny := false
	processed := make(map[string]bool)

	for _, m := range markets {
		if processed[m.ConditionID] {
			continue
		}
		processed[m.ConditionID] = true

		// Fetch balances for both tokens in this market
		var balances []float64
		var outcomes []string
		
		for _, t := range m.Tokens {
			tokenBig := new(big.Int)
			tokenBig.SetString(t.TokenID, 10)
			
			bal, err := polygon.GetCTFBalance(ctx, address, tokenBig)
			if err != nil {
				balances = append(balances, 0)
			} else {
				shares := new(big.Float).SetInt(bal)
				shares = shares.Quo(shares, big.NewFloat(1e6))
				s, _ := shares.Float64()
				balances = append(balances, s)
			}
			outcomes = append(outcomes, t.Outcome)
		}

		// Skip if no tokens found in this market
		if balances[0] < 0.01 && balances[1] < 0.01 {
			continue
		}
		foundAny = true

		fmt.Printf("\n📈 Market: %s\n", m.Slug)
		fmt.Printf("   • %s: %.2f shares\n", outcomes[0], balances[0])
		fmt.Printf("   • %s: %.2f shares\n", outcomes[1], balances[1])

		// Logic 1: MERGE (Balanced pairs)
		if balances[0] >= 1.0 && balances[1] >= 1.0 {
			minQty := math.Min(balances[0], balances[1])
			fmt.Printf("   👉 ACTION: Can MERGE %.0f pairs into $%.2f USDC\n", minQty, minQty)
			fmt.Print("   Confirm Merge? (y/n): ")
			var confirm string
			fmt.Scanln(&confirm)
			if strings.ToLower(confirm) == "y" {
				tx, err := trader.MergeOnChain(ctx, m.ConditionID, minQty)
				if err != nil {
					fmt.Printf("   ❌ Merge failed: %v\n", err)
				} else {
					fmt.Printf("   ✅ Merge successful! Tx: %s\n", tx)
					balances[0] -= minQty
					balances[1] -= minQty
				}
			}
		}

		// Logic 2: REDEEM
		if balances[0] >= 0.01 || balances[1] >= 0.01 {
			info, err := trader.GetMarketInfo(ctx, m.ConditionID)
			if err != nil {
				fmt.Printf("   ⚠️ Resolution status pending or unavailable.\n")
				continue
			}

			if info.Closed {
				winnerOutcome := ""
				for _, t := range info.Tokens {
					if t.Winner {
						winnerOutcome = t.Outcome
					}
				}

				if winnerOutcome != "" {
					fmt.Printf("   🏁 Result: %s Won\n", winnerOutcome)
					hasWinner := false
					if outcomes[0] == winnerOutcome && balances[0] >= 0.01 { hasWinner = true }
					if outcomes[1] == winnerOutcome && balances[1] >= 0.01 { hasWinner = true }

					if hasWinner {
						fmt.Printf("   👉 ACTION: Redeem winning shares for USDC.\n")
						fmt.Print("   Confirm Redeem? (y/n): ")
						var confirm string
						fmt.Scanln(&confirm)
						if strings.ToLower(confirm) == "y" {
							tx, err := trader.RedeemOnChain(ctx, m.ConditionID)
							if err != nil {
								fmt.Printf("   ❌ Redeem failed: %v\n", err)
							} else {
								fmt.Printf("   ✅ Redeem successful! Tx: %s\n", tx)
							}
						}
					} else {
						fmt.Printf("   💀 Market ended. Shares are losers.\n")
					}
				} else {
					fmt.Printf("   ⏳ Market closed, resolution pending.\n")
				}
			} else {
				fmt.Printf("   ⏳ Market still active in API. If resolution is ready on-chain, you can force redeem.\n")
				fmt.Print("   Try Force Redeem? (y/n): ")
				var confirm string
				fmt.Scanln(&confirm)
				if strings.ToLower(confirm) == "y" {
					tx, err := trader.RedeemOnChain(ctx, m.ConditionID)
					if err != nil {
						fmt.Printf("   ❌ Force Redeem failed: %v\n", err)
					} else {
						fmt.Printf("   ✅ Force Redeem successful! Tx: %s\n", tx)
					}
				}
			}
		}
	}

	if !foundAny {
		fmt.Println("✅ No positions found in scanned markets.")
	}
	fmt.Println("\n═══════════════════════════════════════════════════════")
}
