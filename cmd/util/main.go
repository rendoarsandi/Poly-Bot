package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
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

	fmt.Print("Target Amount (USDC per side): ")
	var amtStr string
	fmt.Scanln(&amtStr)
	amt, err := strconv.ParseFloat(amtStr, 64)
	if err != nil || amt <= 0 {
		log.Fatal("Invalid amount.")
	}

	// Calculate target shares based on $ amount
	// For split, 1 USDC = 1 pair. For Buy/Sell, we'll estimate shares.
	targetShares := amt 
	if choice == 3 || choice == 4 { // Buy or Sell BOTH
		// Estimating 2 shares for $1.00 (avg price 0.50)
		// The executeBoth function will handle exact liquidity calculation.
		targetShares = amt / 0.50 
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
		fmt.Printf("Executing Robust Market Buy for BOTH sides (target $%.2f each)...\n", amt)
		executeBoth(ctx, trader, market, outcomes, "BUY", targetShares)
	case 4: // Sell BOTH
		fmt.Printf("Executing Robust Market Sell for BOTH sides (target $%.2f each)...\n", amt)
		executeBoth(ctx, trader, market, outcomes, "SELL", targetShares)
	default:
		fmt.Println("Invalid choice")
	}
}

func executeBoth(ctx context.Context, trader *trading.RealTrader, market api.Market, outcomes []string, side string, targetShares float64) {
	client := api.NewRestClient("")
	
	// 1. Fetch Order Books for Liquidity Check
	tokenMap := make(map[string]string)
	tokenToOutcome := make(map[string]string)
	for _, token := range market.Tokens {
		tokenMap[token.TokenID] = token.Outcome
		tokenToOutcome[token.TokenID] = token.Outcome
	}

	tokenFullBids := make(map[string][]paper.MarketLevel)
	tokenFullAsks := make(map[string][]paper.MarketLevel)
	tokenFeeRates := make(map[string]int)

	fmt.Println("Checking market liquidity and fees...")
	for tid, outcome := range tokenMap {
		book, err := client.GetOrderBook(ctx, tid)
		if err != nil {
			log.Fatalf("Failed to fetch order book for %s: %v", outcome, err)
		}
		tokenFullBids[outcome] = toMarketLevels(book.Bids)
		tokenFullAsks[outcome] = toMarketLevels(book.Asks)

		rate, err := client.GetFeeRate(ctx, tid)
		if err != nil || rate == 0 {
			rate = 1000 // Default for 15m
		}
		tokenFeeRates[outcome] = rate
	}

	// 2. Calculate Max Safe Shares
	shares := targetShares
	if side == "BUY" {
		// Aggregated Asks
		asks1 := tokenFullAsks[outcomes[0]]
		asks2 := tokenFullAsks[outcomes[1]]
		sort.Slice(asks1, func(i, j int) bool { return asks1[i].Price < asks1[j].Price })
		sort.Slice(asks2, func(i, j int) bool { return asks2[i].Price < asks2[j].Price })

		var totalMatchedLiquidity float64
		i, j := 0, 0
		for i < len(asks1) && j < len(asks2) {
			if asks1[i].Price + asks2[j].Price > 0.99 { // Keep it simple for util
				break
			}
			matched := math.Min(asks1[i].Size, asks2[j].Size)
			totalMatchedLiquidity += matched
			if asks1[i].Size <= asks2[j].Size {
				asks2[j].Size -= asks1[i].Size
				i++
			} else {
				asks1[i].Size -= asks2[j].Size
				j++
			}
		}
		if shares > totalMatchedLiquidity {
			fmt.Printf("⚠️ Capping shares at available liquidity: %.2f -> %.2f\n", shares, totalMatchedLiquidity)
			shares = totalMatchedLiquidity
		}
	} else {
		// Aggregated Bids
		bids1 := tokenFullBids[outcomes[0]]
		bids2 := tokenFullBids[outcomes[1]]
		sort.Slice(bids1, func(i, j int) bool { return bids1[i].Price > bids1[j].Price })
		sort.Slice(bids2, func(i, j int) bool { return bids2[i].Price > bids2[j].Price })

		var totalMatchedLiquidity float64
		i, j := 0, 0
		for i < len(bids1) && j < len(bids2) {
			if bids1[i].Price + bids2[j].Price < 1.01 {
				break
			}
			matched := math.Min(bids1[i].Size, bids2[j].Size)
			totalMatchedLiquidity += matched
			if bids1[i].Size <= bids2[j].Size {
				bids2[j].Size -= bids1[i].Size
				i++
			} else {
				bids1[i].Size -= bids2[j].Size
				j++
			}
		}
		if shares > totalMatchedLiquidity {
			fmt.Printf("⚠️ Capping shares at available liquidity: %.2f -> %.2f\n", shares, totalMatchedLiquidity)
			shares = totalMatchedLiquidity
		}
	}

	if shares < 1.0 {
		log.Fatalf("Insufficient liquidity for $1.00 minimum order.")
	}
	shares = math.Floor(shares) // Round to whole shares

	// 3. Balance Check
	balance, _ := trader.GetBalance(ctx)
	if side == "BUY" {
		cost := shares * 0.99 * 1.02 // Conservative cost estimate
		if balance < cost {
			log.Fatalf("Insufficient balance: Need ~$%.2f, have $%.2f", cost, balance)
		}
	}

	// 4. Parallel Execution
	var wg sync.WaitGroup
	wg.Add(len(outcomes))

	var results []*trading.TradeResult
	var errors []error
	var mu sync.Mutex

	for _, outcome := range outcomes {
		go func(o string) {
			defer wg.Done()
			tokenID := getTokenIDForOutcome(market, o)
			rate := tokenFeeRates[o]
			
			var res *trading.TradeResult
			var err error

			if side == "BUY" {
				res, err = trader.Buy(ctx, tokenID, o, 0.99, shares, api.OrderTypeMarket, api.TIFFillOrKill, rate)
			} else {
				res, err = trader.Sell(ctx, tokenID, o, 0.10, shares, api.OrderTypeMarket, api.TIFFillOrKill, rate)
			}
			
			mu.Lock()
			results = append(results, res)
			errors = append(errors, err)
			mu.Unlock()
			
			printTradeResult(side+" "+o, res, err)
		}(outcome)
	}
	wg.Wait()

	// 5. Recovery Logic
	side1Success := errors[0] == nil && results[0] != nil && results[0].Success
	side2Success := errors[1] == nil && results[1] != nil && results[1].Success

	if side1Success != side2Success {
		failedIdx := 0
		if side1Success {
			failedIdx = 1
		}
		failedOutcome := outcomes[failedIdx]
		failedToken := getTokenIDForOutcome(market, failedOutcome)
		fmt.Printf("\n⚠️ UNBALANCED FILL: %s failed. Retrying AGGRESSIVELY with GTC...\n", failedOutcome)

		for attempt := 1; attempt <= 3; attempt++ {
			fmt.Printf("Recovery Attempt #%d for %s...\n", attempt, failedOutcome)
			var res *trading.TradeResult
			var err error
			rate := tokenFeeRates[failedOutcome]

			if side == "BUY" {
				res, err = trader.Buy(ctx, failedToken, failedOutcome, 0.99, shares, api.OrderTypeMarket, api.TIFGoodTilCancelled, rate)
			} else {
				res, err = trader.Sell(ctx, failedToken, failedOutcome, 0.10, shares, api.OrderTypeMarket, api.TIFGoodTilCancelled, rate)
			}

			if err == nil && res != nil && res.Success {
				fmt.Printf("✅ RECOVERY SUCCESS for %s!\n", failedOutcome)
				break
			}
			fmt.Printf("❌ Recovery Attempt #%d failed: %v\n", attempt, err)
			time.Sleep(1 * time.Second)
		}
	}
}

func toMarketLevels(levels []api.PriceLevel) []paper.MarketLevel {
	result := make([]paper.MarketLevel, len(levels))
	for i, l := range levels {
		p, _ := strconv.ParseFloat(l.Price, 64)
		s, _ := strconv.ParseFloat(l.Size, 64)
		result[i] = paper.MarketLevel{Price: p, Size: s}
	}
	return result
}

func getTokenIDForOutcome(m api.Market, outcome string) string {
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