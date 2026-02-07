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
	client := api.NewRestClient("")
	polygon := api.NewPolygonClient(cfg.PolygonRPCURL)
	address := trader.Address()

	fmt.Println("🚀 POLYARB UNIFIED MERGE & REDEEM TOOL")
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Printf("🔑 Wallet: %s\n", address)

	// 1. Scan for positions directly on-chain
	fmt.Println("🔍 Scanning blockchain for tokens...")
	markets, err := client.Get15mMarkets(ctx, nil)
	if err != nil {
		log.Fatalf("Failed to fetch markets: %v", err)
	}

	foundAny := false
	for _, m := range markets {
		// Fetch balances for both tokens in this market
		var balances []float64
		var outcomes []string
		var tokenIDs []*big.Int

		for _, t := range m.Tokens {
			tid := new(big.Int)
			tid.SetString(t.TokenID, 10)
			tokenIDs = append(tokenIDs, tid)
			
			bal, err := polygon.GetCTFBalance(ctx, address, tid)
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
		if balances[0] == 0 && balances[1] == 0 {
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
					// Update local balance for subsequent checks
					balances[0] -= minQty
					balances[1] -= minQty
				}
			}
		}

		// Logic 2: REDEEM (Resolved market with winning shares)
		if balances[0] > 0 || balances[1] > 0 {
			// Check if market is resolved
			info, err := trader.GetMarketInfo(ctx, m.ConditionID)
			if err == nil && info.Closed {
				winnerOutcome := ""
				for _, t := range info.Tokens {
					if t.Winner {
						winnerOutcome = t.Outcome
					}
				}

				if winnerOutcome != "" {
					fmt.Printf("   🏁 Market Resolved: %s Won\n", winnerOutcome)
					hasWinner := false
					if outcomes[0] == winnerOutcome && balances[0] > 0 { hasWinner = true }
					if outcomes[1] == winnerOutcome && balances[1] > 0 { hasWinner = true }

					if hasWinner {
						fmt.Printf("   👉 ACTION: Winning shares detected! Redeem for USDC.\n")
						fmt.Print("   Confirm On-Chain Redeem? (y/n): ")
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
						fmt.Printf("   💀 Market ended. Remaining shares are losers ($0.00).\n")
					}
				}
			} else if err == nil && !info.Closed {
				fmt.Printf("   ⏳ Market still active. Sell shares or wait for expiry.\n")
			}
		}
	}

	if !foundAny {
		fmt.Println("✅ No positions found on-chain.")
	}
	fmt.Println("\n═══════════════════════════════════════════════════════")
}