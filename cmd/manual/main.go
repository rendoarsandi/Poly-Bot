package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/marketlookup"
	"Market-bot/internal/setup"
	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load()
	cfg, err := core.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	setupCtx, cancelSetup := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelSetup()

	cfg.TradingMode = core.ModeReal
	trader, err := setup.EnsureRealTradingSetup(setupCtx, cfg)
	if err != nil {
		log.Fatalf("Failed to setup trader: %v", err)
	}

	ctx := context.Background()
	polygon := api.NewPolygonClient(cfg.PolygonRPCURL)
	address := trader.Address()
	target := firstTargetArg(os.Args[1:])

	fmt.Println("🚀 MANUAL DUMP SCRIPT (Sell/Dump Only)")
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Printf("🔑 Wallet: %s\n", address)

	if target != "" {
		fmt.Printf("🔍 Resolving target market: %s\n", target)
	} else {
		fmt.Printf("🔍 Fetching positions from API...\n")
	}

	markets, source, err := marketlookup.ResolveMarkets(ctx, trader, polygon, target)
	if err != nil {
		log.Fatalf("Failed to resolve markets: %v", err)
	}
	if len(markets) == 0 {
		if target != "" {
			fmt.Printf("✅ No markets found for target %s.\n", target)
		} else {
			fmt.Println("✅ No positions found. Try `manual <slug-or-condition-id>` for a direct lookup.")
		}
		return
	}
	fmt.Printf("✅ Loaded %d market(s) via %s\n", len(markets), source)

	fmt.Println("🔌 Preparing User WebSocket for real-time order updates...")
	if err := trader.StartUserWS(ctx); err != nil {
		fmt.Printf("⚠️ Failed to prepare User WS: %v\n", err)
	} else {
		var condIDs []string
		for _, m := range markets {
			if m.ConditionID != "" {
				condIDs = append(condIDs, m.ConditionID)
			}
		}
		if err := trader.SubscribeUserWSMarkets(ctx, condIDs...); err != nil {
			fmt.Printf("⚠️ Failed to subscribe User WS for current positions: %v\n", err)
		} else {
			fmt.Println("✅ User WebSocket ready for current positions")
		}
	}

	foundAny := false
	for _, m := range markets {
		var balances []float64
		var outcomes []string
		var tokenIDs []string

		for _, t := range m.Tokens {
			bal, err := trader.GetCTFBalanceFloat(ctx, t.TokenID)
			if err != nil {
				balances = append(balances, 0)
			} else {
				balances = append(balances, bal)
			}
			outcomes = append(outcomes, t.Outcome)
			tokenIDs = append(tokenIDs, t.TokenID)
		}

		hasBal := false
		for _, b := range balances {
			if b >= 0.0001 {
				hasBal = true
				break
			}
		}

		if !hasBal {
			continue
		}
		foundAny = true

		fmt.Printf("\n📈 Market: %s\n", m.Slug)

		// Check if market is closed
		info, err := trader.GetMarketInfo(ctx, m.ConditionID)
		if err == nil && info.Closed {
			fmt.Println("   ⚠️ MARKET IS RESOLVED/CLOSED. Cannot sell.")
			continue
		}

		for i, b := range balances {
			if b >= 0.0001 {
				fmt.Printf("   • %s: %.4f shares (Token: %s)\n", outcomes[i], b, tokenIDs[i][:10]+"...")
				fmt.Printf("   👉 ACTION: DUMP %.4f shares of %s at market?\n", b, outcomes[i])
				fmt.Print("   Confirm Sell? (y/n): ")
				var confirm string
				fmt.Scanln(&confirm)
				if strings.ToLower(confirm) == "y" {
					fmt.Printf("   ⏳ Selling %s...\n", outcomes[i])
					// Using trader.Sell with Market Order
					// Using trader.Sell with Market Order
					res, err := trader.Sell(ctx, tokenIDs[i], outcomes[i], 0.01, b, api.OrderTypeMarket, api.TIFFillAndKill, 1000)
					if err != nil {
						fmt.Printf("   ❌ Sell error: %v\n", err)
					} else {
						fmt.Printf("   ✅ Sell API Result: %v (Message: %s)\n", res.Success, res.Message)
					}
				} else {
					fmt.Println("   ⏭️  Skipped.")
				}
			}
		}
	}

	if !foundAny {
		fmt.Println("✅ No on-chain shares found to dump.")
	}
}

func firstTargetArg(args []string) string {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return arg
	}
	return ""
}
