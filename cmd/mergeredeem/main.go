package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/marketlookup"
	"Market-bot/internal/setup"
	"github.com/joho/godotenv"
)

const minOnChainActionShares = 0.01

func main() {
	_ = godotenv.Load()
	cfg, err := core.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Create context for setup
	setupCtx, cancelSetup := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelSetup()

	// Ensure credentials exist and allowances are ready for real trading
	trader, err := setup.EnsureRealTradingSetup(setupCtx, cfg)
	if err != nil {
		log.Fatalf("Failed to setup or create trader: %v", err)
	}

	ctx := context.Background()
	polygon := api.NewPolygonClient(cfg.PolygonRPCURL)
	address := trader.Address()

	fmt.Println("🚀 POLYARB SMART SCANNER (Merge & Redeem Only)")
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Printf("🔑 Wallet: %s\n", address)

	forceRedeem := false
	target := ""
	for _, arg := range os.Args[1:] {
		if arg == "-force" {
			forceRedeem = true
			continue
		}
		if target == "" && !strings.HasPrefix(arg, "-") {
			target = arg
		}
	}

	if target != "" {
		fmt.Printf("🔍 Resolving target market: %s\n", target)
	}

	markets, source, err := marketlookup.ResolveMarkets(ctx, trader, polygon, target)
	if err != nil {
		log.Fatalf("Failed to resolve markets: %v", err)
	}
	if len(markets) == 0 {
		if target != "" {
			fmt.Printf("✅ No markets found for target %s.\n", target)
		} else {
			fmt.Println("✅ No relevant markets found for your positions. Try `mergeredeem <slug-or-condition-id>` for a direct lookup.")
		}
		return
	}
	fmt.Printf("✅ Loaded %d market(s) via %s\n", len(markets), source)

	foundAny := false
	processed := make(map[string]bool)

	for _, m := range markets {
		if processed[m.ConditionID] {
			continue
		}
		processed[m.ConditionID] = true

		var balances []float64
		var outcomes []string

		for _, t := range m.Tokens {
			bal, err := trader.GetCTFBalanceFloat(ctx, t.TokenID)
			if err != nil {
				balances = append(balances, 0)
			} else {
				balances = append(balances, bal)
			}
			outcomes = append(outcomes, t.Outcome)
		}

		// Skip if no tokens found in this market or all are dust
		if len(balances) < 2 {
			continue
		}

		hasSignificantBalance := false
		for _, b := range balances {
			if b >= 0.0001 {
				hasSignificantBalance = true
				break
			}
		}

		if !hasSignificantBalance {
			continue
		}
		minQty := mergeablePairs(balances)
		remainingBalances := append([]float64(nil), balances...)
		if minQty > 0 {
			remainingBalances[0] -= minQty
			remainingBalances[1] -= minQty
		}

		marketLabelPrinted := false
		printMarketHeader := func() {
			if marketLabelPrinted {
				return
			}
			foundAny = true
			fmt.Printf("\n📈 Market: %s\n", m.Slug)
			fmt.Printf("   • %s: %.6f shares\n", outcomes[0], balances[0])
			fmt.Printf("   • %s: %.6f shares\n", outcomes[1], balances[1])
			marketLabelPrinted = true
		}

		if minQty > 0 {
			printMarketHeader()
			fmt.Printf("   👉 ACTION: Can MERGE %.6f pairs (%.6f shares/side) into $%.2f USDC\n", minQty, minQty, minQty)
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
					remainingBalances[0] = balances[0]
					remainingBalances[1] = balances[1]
				}
			}
		}

		// Logic 2: REDEEM
		if remainingBalances[0] >= 0.01 || remainingBalances[1] >= 0.01 {
			info, err := trader.GetMarketInfo(ctx, m.ConditionID)
			if err != nil {
				printMarketHeader()
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
					hasWinner := false
					if outcomes[0] == winnerOutcome && remainingBalances[0] >= 0.01 {
						hasWinner = true
					}
					if outcomes[1] == winnerOutcome && remainingBalances[1] >= 0.01 {
						hasWinner = true
					}

					if hasWinner {
						printMarketHeader()
						fmt.Printf("   🏁 Result: %s Won\n", winnerOutcome)
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
								if !isSkippableRedeemError(err) {
									fmt.Printf("   ❌ Redeem failed: %v\n", err)
								}
							} else {
								fmt.Printf("   ✅ Redeem successful! Tx: %s\n", tx)
							}
						}
					}
				}
			} else {
				printMarketHeader()
				fmt.Printf("   ⏳ Market still active in API. If resolution is ready on-chain, you can force redeem.\n")
				fmt.Print("   Try Force Redeem? (y/n): ")
				var confirm string
				_, _ = fmt.Scanln(&confirm)
				if strings.ToLower(confirm) == "y" {
					tx, err := trader.RedeemOnChain(ctx, m.ConditionID, len(m.Tokens))
					if err != nil {
						if !isSkippableRedeemError(err) {
							fmt.Printf("   ❌ Force Redeem failed: %v\n", err)
						}
					} else {
						fmt.Printf("   ✅ Force Redeem successful! Tx: %s\n", tx)
					}
				}
			}
		}
	}

	if !foundAny {
		fmt.Println("✅ No actionable merge/redeem positions found.")
	}
	fmt.Println("\n═══════════════════════════════════════════════════════")
}

func mergeablePairs(balances []float64) float64 {
	if len(balances) < 2 {
		return 0
	}
	minQty := math.Min(balances[0], balances[1])
	if minQty < minOnChainActionShares {
		return 0
	}
	return minQty
}

func isSkippableRedeemError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "market not yet resolved on-chain") ||
		strings.Contains(msg, "payouts not reported") ||
		strings.Contains(msg, "reverted on-chain") ||
		strings.Contains(msg, "execution reverted")
}
