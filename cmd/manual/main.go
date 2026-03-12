package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/marketlookup"
	"Market-bot/internal/setup"
	"github.com/joho/godotenv"
)

const (
	manualbotEmergencySellFloor = 0.03
	manualbotQuoteTimeout       = 1500 * time.Millisecond
	manualbotQuoteMaxAge        = 2 * time.Second
)

func manualbotEmergencySellPrice(bestBid float64) float64 {
	price := core.CleanupSellLimitPrice(manualbotEmergencySellFloor)
	if bestBid > 0 && bestBid < price {
		price = bestBid
	}
	return math.Max(price, 0.01)
}

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
	rest := api.NewRestClient(cfg.Exchange, cfg.KalshiAPIKey, cfg.KalshiPK)
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

		// Check if market is closed
		info, err := trader.GetMarketInfo(ctx, m.ConditionID)
		if err == nil && info.Closed {
			continue
		}

		foundAny = true
		fmt.Printf("\n📈 Market: %s\n", m.Slug)

		for i, b := range balances {
			if b >= 0.0001 {
				fmt.Printf("   • %s: %.4f shares (Token: %s)\n", outcomes[i], b, tokenIDs[i][:10]+"...")
				fmt.Printf("   👉 ACTION: DUMP %.4f shares of %s at market?\n", b, outcomes[i])
				fmt.Print("   Confirm Sell? (y/n): ")
				var confirm string
				fmt.Scanln(&confirm)
				if strings.ToLower(confirm) == "y" {
					quoteCtx, cancelQuote := context.WithTimeout(ctx, manualbotQuoteTimeout)
					bestBid, latency, age, quoteErr := manualbotFetchFreshBestBid(quoteCtx, rest, tokenIDs[i], manualbotQuoteMaxAge)
					cancelQuote()
					if quoteErr != nil {
						fmt.Printf("   ⚠️ Fresh live bid unavailable for %s: %v\n", outcomes[i], quoteErr)
						fmt.Println("   ⏭️  Skipped to avoid dumping against a stale/empty book.")
						continue
					}
					sellPrice := manualbotEmergencySellPrice(bestBid)
					if bestBid+1e-9 < manualbotEmergencySellFloor {
						fmt.Printf("   📡 Live best bid %.3f is below dump floor %.3f (age %s, latency %s). Repricing to live bid.\n", bestBid, manualbotEmergencySellFloor, age.Round(time.Millisecond), latency.Round(time.Millisecond))
					} else {
						fmt.Printf("   📡 Live best bid: %.3f (age %s, latency %s)\n", bestBid, age.Round(time.Millisecond), latency.Round(time.Millisecond))
					}
					fmt.Printf("   ⏳ Selling %s with FAK floor %.3f...\n", outcomes[i], sellPrice)
					// Using trader.Sell with Market Order
					// Using trader.Sell with Market Order
					res, err := trader.Sell(ctx, tokenIDs[i], outcomes[i], sellPrice, b, api.OrderTypeMarket, api.TIFFillAndKill, 1000)
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
		fmt.Println("✅ No actionable open shares found to dump.")
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

func manualbotFetchFreshBestBid(ctx context.Context, restClient *api.RestClient, tokenID string, maxAge time.Duration) (bestBid float64, latency time.Duration, age time.Duration, err error) {
	start := time.Now()
	book, err := restClient.GetOrderBook(ctx, tokenID)
	latency = time.Since(start)
	if err != nil {
		return 0, latency, 0, err
	}
	age, err = api.OrderBookAgeAt(book, time.Now())
	if err != nil {
		return 0, latency, 0, err
	}
	if age > maxAge {
		return 0, latency, age, fmt.Errorf("stale order book age %s > %s", age.Round(time.Millisecond), maxAge)
	}
	for _, level := range book.Bids {
		price, parseErr := strconv.ParseFloat(level.Price, 64)
		if parseErr != nil {
			continue
		}
		if price > bestBid && price > 0 && price < 1.0 {
			bestBid = price
		}
	}
	if bestBid <= 0 {
		return 0, latency, age, fmt.Errorf("no live bid found in order book")
	}
	return bestBid, latency, age, nil
}
