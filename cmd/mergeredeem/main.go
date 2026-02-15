package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/big"
	"strconv"
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
					// Update local balances
					balances[0] -= minQty
					balances[1] -= minQty
				}
			}
		}

		// Logic 2: SELL UNBALANCED (Leftovers after merge)
		for i, bal := range balances {
			if bal >= 1.0 {
				outcome := outcomes[i]
				tokenID := m.Tokens[i].TokenID

				// Fetch current bid price for liquidation
				fmt.Printf("   🔍 Fetching bid for %s (%s)...\n", outcome, tokenID)
				book, err := client.GetOrderBook(ctx, tokenID)
				bestBid := 0.0
				if err == nil {
					for _, b := range book.Bids {
						p, _ := strconv.ParseFloat(b.Price, 64)
						if p > bestBid {
							bestBid = p
						}
					}
				}

				if bestBid > 0 {
					fmt.Printf("   👉 ACTION: Sell %.0f shares of %s at ~$%.2f market bid?\n", bal, outcome, bestBid)
					fmt.Print("   Confirm Market Sell? (y/n): ")
					var confirm string
					fmt.Scanln(&confirm)
					if strings.ToLower(confirm) == "y" {
						// Use the actual best bid (minus a tiny 1% slippage) to stay above $1.00 minimum
						sellPrice := bestBid * 0.99
						
						rate, _ := client.GetFeeRate(ctx, tokenID)
						if rate == 0 { rate = 1000 }
						res, err := trader.Sell(ctx, tokenID, outcome, sellPrice, bal, api.OrderTypeMarket, api.TIFFillOrKill, rate)
						if err != nil {
							fmt.Printf("   ❌ Sell failed: %v\n", err)
						} else if res != nil && res.Success {
							fmt.Printf("   ✅ Sell successful! Sold %.0f %s at ~$%.2f\n", bal, outcome, bestBid)
							balances[i] = 0
						} else {
							msg := "Unknown error"
							if res != nil { msg = res.Message }
							fmt.Printf("   ❌ Sell rejected: %s\n", msg)
						}
					}
				} else {
					fmt.Printf("   ⚠️ No buy orders found for %s, cannot liquidate.\n", outcome)
				}
			}
		}

		// Logic 3: REDEEM (Resolved market with winning shares)
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