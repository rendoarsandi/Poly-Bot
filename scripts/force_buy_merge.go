package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/trading"
	"github.com/joho/godotenv"
)

func main() {
	runForceBuyMerge()
}

func runForceBuyMerge() {
	if len(os.Args) < 2 {
		log.Fatal("Usage: go run scripts/force_buy_merge.go <market_slug>")
	}
	targetSlug := os.Args[1]

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

	// 1. Fetch the specific market
	fmt.Printf("🔍 Fetching market: %s...\n", targetSlug)
	market, err := client.GetMarket(ctx, targetSlug)
	if err != nil {
		// Fallback: try list and find
		markets, _ := client.GetMarketsByTimeframe(ctx, nil, "15m")
		for _, m := range markets {
			if m.Slug == targetSlug {
				market = &m
				break
			}
		}
		if market == nil {
			log.Fatalf("❌ Could not find market %s: %v", targetSlug, err)
		}
	}
	fmt.Printf("✅ Found ConditionID: %s\n", market.ConditionID)

	// 2. Buy 2 shares of each side
	shares := 2.0
	tokenMap := make(map[string]string)
	
	for _, t := range market.Tokens {
		tokenMap[t.TokenID] = t.Outcome
	}

	// Fetch fees first
	tokenFeeRates := make(map[string]int)
	for tid, out := range tokenMap {
		rate, err := client.GetFeeRate(ctx, tid)
		if err != nil || rate == 0 {
			tokenFeeRates[out] = 1000 // Force 1000 bps fallback
		} else {
			tokenFeeRates[out] = rate
		}
		fmt.Printf("ℹ️  Fee for %s: %d bps\n", out, tokenFeeRates[out])
	}

	fmt.Printf("🚀 Buying %.0f shares of both sides to sweep dust...\n", shares)
	var wg sync.WaitGroup
	wg.Add(2)
	
	for _, t := range market.Tokens {
		go func(token api.Token) {
			defer wg.Done()
			rate := tokenFeeRates[token.Outcome]
			// Use aggressive pricing for guaranteed fill
			price := 0.99
			if strings.EqualFold(token.Outcome, "No") || strings.EqualFold(token.Outcome, "Down") {
				// For binary markets, just cap at 0.99 is fine for market orders
			}
			
			res, err := trader.Buy(ctx, token.TokenID, token.Outcome, price, shares, api.OrderTypeMarket, api.TIFFillOrKill, rate)
			if err != nil {
				fmt.Printf("❌ Buy %s failed: %v\n", token.Outcome, err)
			} else if !res.Success {
				fmt.Printf("❌ Buy %s failed: %s\n", token.Outcome, res.Message)
			} else {
				fmt.Printf("✅ Buy %s success! OrderID: %s\n", token.Outcome, res.OrderID)
			}
		}(t)
	}
	wg.Wait()

	fmt.Println("⏳ Waiting 5 seconds for on-chain settlement...")
	time.Sleep(5 * time.Second)

	// 3. Query TOTAL balance (new + dust)
	fmt.Println("🔍 Querying total on-chain balances...")
	balances := make(map[string]float64)
	
	for _, t := range market.Tokens {
		bal, err := trader.GetCTFBalanceFloat(ctx, t.TokenID)
		if err != nil {
			fmt.Printf("⚠️  Failed to get balance for %s: %v\n", t.Outcome, err)
			bal = 0
		}
		balances[t.Outcome] = bal
		fmt.Printf("   • %s: %.6f\n", t.Outcome, bal)
	}

	// 4. Merge
	// Find minimum common balance
	minBal := 1000000.0
	for _, b := range balances {
		if b < minBal {
			minBal = b
		}
	}

	if minBal < 0.000001 {
		fmt.Println("⚠️  No common balance to merge.")
		return
	}

	fmt.Printf("🔄 Merging %.6f pairs (cleaning up dust)...\n", minBal)
	tx, err := trader.MergeOnChain(ctx, market.ConditionID, minBal)
	if err != nil {
		log.Fatalf("❌ Merge failed: %v", err)
	}
	fmt.Printf("✅ Merge successful! Tx: %s\n", tx)
}
