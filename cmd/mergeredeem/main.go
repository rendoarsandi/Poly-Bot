package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

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

	forceRedeem := false
	for _, arg := range os.Args {
		if arg == "-force" {
			forceRedeem = true
			break
		}
	}

	var markets []api.Market
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		specificSlug := os.Args[1]
		fmt.Printf("🔍 Looking for specific slug: %s\n", specificSlug)

		// Attempt to get market by slug from Gamma
		lookupURL := fmt.Sprintf("https://gamma-api.polymarket.com/events?slug=%s", url.QueryEscape(specificSlug))
		req, reqErr := http.NewRequestWithContext(ctx, "GET", lookupURL, nil)
		if reqErr != nil {
			fmt.Printf("   ❌ Error creating slug lookup request: %v\n", reqErr)
		} else {
			resp, err := http.DefaultClient.Do(req)
			if err == nil && resp.StatusCode == http.StatusOK {
				var events []api.GammaEvent
				if err := json.NewDecoder(resp.Body).Decode(&events); err == nil && len(events) > 0 {
					event := events[0]
					if len(event.Markets) > 0 {
						gm := event.Markets[0]
						var tokenIds []string
						if err := json.Unmarshal([]byte(gm.ClobTokenIds), &tokenIds); err == nil && len(tokenIds) >= 2 {
							var outcomes []string
							if err := json.Unmarshal([]byte(gm.Outcomes), &outcomes); err != nil || len(outcomes) < 2 {
								outcomes = []string{"Up", "Down"}
							}

							markets = append(markets, api.Market{
								ConditionID: gm.ConditionID,
								Slug:        core.SanitizeString(specificSlug),
								Active:      gm.Active,
								Closed:      gm.Closed,
								Tokens: []api.Token{
									{TokenID: tokenIds[0], Outcome: core.SanitizeString(outcomes[0])},
									{TokenID: tokenIds[1], Outcome: core.SanitizeString(outcomes[1])},
								},
							})
							fmt.Printf("   ✅ Found market: %s\n", gm.ConditionID)
						}
					}
				}
				resp.Body.Close()
			} else {
				if err != nil {
					fmt.Printf("   ❌ Error looking up slug: %v\n", err)
				} else {
					fmt.Printf("   ❌ Slug not found (status %d)\n", resp.StatusCode)
					resp.Body.Close()
				}
			}
		}
	}

	if len(markets) == 0 {
		// 1. Scan for positions directly on-chain
		fmt.Println("🔍 Scanning blockchain for tokens (BTC, ETH, SOL, XRP)...")
		assets := []string{"btc", "eth", "sol", "xrp"}
		foundMarkets, err := client.Get15mMarkets(ctx, assets)
		if err != nil {
			log.Fatalf("Failed to fetch markets: %v", err)
		}
		markets = foundMarkets
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
				mergeCtx, cancelMerge := context.WithTimeout(ctx, 90*time.Second)
				tx, err := trader.MergeOnChain(mergeCtx, m.ConditionID, minQty)
				cancelMerge()
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
					if outcomes[0] == winnerOutcome && balances[0] > 0 {
						hasWinner = true
					}
					if outcomes[1] == winnerOutcome && balances[1] > 0 {
						hasWinner = true
					}

					if hasWinner {
						fmt.Printf("   👉 ACTION: Winning shares detected! Redeem for USDC.\n")

						// Automatic wait for resolution
						fmt.Printf("   ⏳ Checking on-chain resolution status...")
						resolved := false
						for i := 0; i < 18; i++ { // Wait up to 3 minutes (18 * 10s)
							isRes, err := polygon.IsMarketResolved(ctx, m.ConditionID)
							if err == nil && isRes {
								resolved = true
								fmt.Println(" ✅ READY")
								break
							}
							if i == 0 {
								fmt.Print("\n   ⏳ Market not yet settled on-chain. Waiting for Polygon to sync...")
							} else {
								fmt.Print(".")
							}
							time.Sleep(10 * time.Second)
						}

						if !resolved && !forceRedeem {
							fmt.Println("\n   ⚠️  Market still not settled on-chain after 3 minutes.")
							fmt.Print("   Do you want to FORCE the redemption attempt anyway? (y/n): ")
						} else {
							fmt.Print("   Confirm On-Chain Redeem? (y/n): ")
						}

						var confirm string
						fmt.Scanln(&confirm)
						if strings.ToLower(confirm) == "y" {
							var tx string
							var err error
							// If we're resolved OR user forced it, we call RedeemPositions
							// Note: trader.RedeemOnChain has its own check, so we call polygon directly if we want to bypass it
							redeemCtx, cancelRedeem := context.WithTimeout(ctx, 90*time.Second)
							if forceRedeem || !resolved {
								tx, err = polygon.RedeemPositions(redeemCtx, trader.GetSigner(), m.ConditionID)
							} else {
								tx, err = trader.RedeemOnChain(redeemCtx, m.ConditionID)
							}
							cancelRedeem()

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
