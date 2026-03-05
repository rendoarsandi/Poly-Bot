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
	"Market-bot/internal/setup"
	"github.com/joho/godotenv"
)

func main() {
        _ = godotenv.Load()
        cfg, err := core.LoadConfig()
        if err != nil {		log.Fatalf("Failed to load config: %v", err)
	}

	// Create context for setup
	setupCtx, cancelSetup := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelSetup()

	// Ensure trading mode is set and credentials exist
	cfg.TradingMode = core.ModeReal
	trader, err := setup.EnsureRealTradingSetup(setupCtx, cfg)
	if err != nil {
		log.Fatalf("Failed to setup or create trader: %v", err)
	}

	ctx := context.Background()
	rest := api.NewRestClient("")
	polygon := api.NewPolygonClient(cfg.PolygonRPCURL)
	address := trader.Address()

	fmt.Println("🚀 POLYARB SMART SCANNER (Merge & Redeem Only)")
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
					if resp != nil {
						resp.Body.Close()
					}
				} else {
					fmt.Printf("   ❌ Slug not found (status %d)\n", resp.StatusCode)
					resp.Body.Close()
				}
			}
		}
	}

	if len(markets) == 0 {
		// Smart Discovery: Scan for active/closed markets via Gamma tag
		fmt.Printf("🔍 Scanning for all recent %s markets (including closed)...\n", cfg.Timeframe)
		foundMarkets, err := rest.GetMarketsByTimeframe(ctx, nil, cfg.Timeframe)
		if err != nil {
			log.Fatalf("Failed to fetch markets: %v", err)
		}
		markets = foundMarkets
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
		if len(balances) < 2 || (balances[0] < 0.01 && balances[1] < 0.01) {
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
			_, _ = fmt.Scanln(&confirm)
			if strings.ToLower(confirm) == "y" {
				mergeCtx, cancelMerge := context.WithTimeout(ctx, 90*time.Second)
				tx, err := trader.MergeOnChain(mergeCtx, m.ConditionID, minQty, len(m.Tokens))
				cancelMerge()
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
					if outcomes[0] == winnerOutcome && balances[0] >= 0.01 {
						hasWinner = true
					}
					if outcomes[1] == winnerOutcome && balances[1] >= 0.01 {
						hasWinner = true
					}

					if hasWinner {
						fmt.Printf("   👉 ACTION: Winning shares detected! Redeem for USDC.\n")

						// Automatic wait for resolution
						fmt.Printf("   ⏳ Checking on-chain resolution status...")
						isRes, err := polygon.IsMarketResolved(ctx, m.ConditionID)
						resolved := err == nil && isRes

						if resolved {
							fmt.Println(" ✅ READY")
							fmt.Print("   Confirm On-Chain Redeem? (y/n): ")
						} else {
							fmt.Println(" ⚠️ NOT YET SETTLED")
							fmt.Print("   Market not settled on-chain. Force redemption attempt anyway? (y/n): ")
						}

						var confirm string
						_, _ = fmt.Scanln(&confirm)
						if strings.ToLower(confirm) == "y" {
							var tx string
							var err error
							redeemCtx, cancelRedeem := context.WithTimeout(ctx, 90*time.Second)
							if forceRedeem || !resolved {
								tx, err = polygon.RedeemPositions(redeemCtx, trader.GetSigner(), m.ConditionID, len(m.Tokens))
							} else {
								tx, err = trader.RedeemOnChain(redeemCtx, m.ConditionID, len(m.Tokens))
							}
							cancelRedeem()

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
				_, _ = fmt.Scanln(&confirm)
				if strings.ToLower(confirm) == "y" {
					tx, err := trader.RedeemOnChain(ctx, m.ConditionID, len(m.Tokens))
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
