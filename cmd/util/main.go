package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
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

	client := api.NewRestClient("")
	ctx := context.Background()

	// 1. Find the active 15m market
	fmt.Println("Finding active market...")
	markets, err := client.Get15mMarkets(ctx, []string{"btc", "eth"})
	if err != nil || len(markets) == 0 {
		log.Fatal("No active 15m markets found.")
	}

	market := markets[0]
	endTime, _ := paper.ParseEndTimeFromSlug(market.Slug)
	timeLeft := time.Until(endTime)

	fmt.Printf("Market: %s\nTime Left: %v\n", market.Slug, timeLeft.Round(time.Second))

	fmt.Print("Outcomes found: ")
	var outcomes []string
	for _, t := range market.Tokens {
		fmt.Printf("[%s] ", t.Outcome)
		outcomes = append(outcomes, t.Outcome)
	}
	fmt.Println()

	if timeLeft < 2*time.Minute {
		log.Fatal("Too close to expiry (< 2 mins). Operation cancelled for safety.")
	}

	// 2. Simple Menu
	fmt.Println("\nActions: 1:Split, 2:Merge, 3:Buy BOTH, 4:Sell BOTH")
	fmt.Print("Choose action (1-4): ")
	var choice int
	fmt.Scanln(&choice)

	fmt.Print("Amount (per side): ")
	var amtStr string
	fmt.Scanln(&amtStr)
	amt, err := strconv.ParseFloat(amtStr, 64)
	if err != nil || amt <= 0 {
		log.Fatal("Invalid amount.")
	}

	// 3. Execute
	switch choice {
	case 1: // Split
		fmt.Println("Executing Split...")
		tx, err := trader.SplitOnChain(ctx, market.ConditionID, amt)
		printResult("Split", tx, err)
	case 2: // Merge
		fmt.Println("Executing Merge...")
		tx, err := trader.MergeOnChain(ctx, market.ConditionID, amt)
		printResult("Merge", tx, err)
	case 3: // Buy BOTH
		fmt.Printf("Executing Market Buy for BOTH sides (%.2f each)...\n", amt)
		executeBoth(ctx, trader, market, outcomes, "BUY", amt)
	case 4: // Sell BOTH
		fmt.Printf("Executing Market Sell for BOTH sides (%.2f each)...\n", amt)
		executeBoth(ctx, trader, market, outcomes, "SELL", amt)
	default:
		fmt.Println("Invalid choice")
	}
}

func executeBoth(ctx context.Context, trader *trading.RealTrader, market api.Market, outcomes []string, side string, amt float64) {
	var wg sync.WaitGroup
	wg.Add(len(outcomes))

	client := api.NewRestClient("")

	for _, outcome := range outcomes {
		go func(o string) {
			defer wg.Done()
			tokenID := getTokenID(market, o)
			if tokenID == "" {
				printTradeResult(side+" "+o, nil, fmt.Errorf("token id not found for outcome %q", o))
				return
			}

			// Fetch current fee rate
			feeRate, err := client.GetFeeRate(ctx, tokenID)
			if err != nil {
				fmt.Printf("Warning: could not fetch fee rate for %s: %v. Using 1000.\n", o, err)
				feeRate = 1000
			}
			if feeRate == 0 {
				feeRate = 1000
			}

			var res *trading.TradeResult

			if side == "BUY" {
				// Use true MARKET order with protective price cap, same style as realbot.
				res, err = trader.Buy(ctx, tokenID, o, 0.99, amt, api.OrderTypeMarket, api.TIFFillOrKill, feeRate)
			} else {
				// Use true MARKET order with protective price floor, same style as realbot.
				res, err = trader.Sell(ctx, tokenID, o, 0.60, amt, api.OrderTypeMarket, api.TIFFillOrKill, feeRate)
			}
			printTradeResult(side+" "+o, res, err)
		}(outcome)
	}
	wg.Wait()
}

func getTokenID(m api.Market, outcome string) string {
	for _, t := range m.Tokens {
		if strings.EqualFold(t.Outcome, outcome) {
			return t.TokenID
		}
	}
	return ""
}

func printResult(act, tx string, err error) {
	if err != nil {
		fmt.Printf("FAILED: %s - error: %v\n", act, err)
	} else {
		fmt.Printf("SUCCESS: %s - Tx: %s\n", act, tx)
	}
}

func printTradeResult(act string, res *trading.TradeResult, err error) {
	if err != nil || (res != nil && !res.Success) {
		msg := "Unknown error"
		if err != nil {
			msg = err.Error()
		} else if res != nil {
			msg = res.Message
		}
		fmt.Printf("FAILED: %s - error: %s\n", act, msg)
	} else {
		fmt.Printf("SUCCESS: %s - OrderID: %s\n", act, res.OrderID)
	}
}
