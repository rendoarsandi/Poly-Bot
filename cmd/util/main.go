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

	// 1. Find markets (EXACTLY LIKE REALBOT)
	fmt.Println("🔍 Searching for active 15m markets...")
	markets := findMarkets(ctx, client)
	
	if len(markets) == 0 {
		log.Fatal("❌ No active 15m markets found.")
	}

	// 1.2 Let user choose asset if multiple found
	var selectedAsset string
	var assetNames []string
	for k := range markets { assetNames = append(assetNames, k) }
	sort.Strings(assetNames)

	if len(markets) > 1 {
		fmt.Printf("Assets found: [%s]. Choose one: ", strings.Join(assetNames, ", "))
		fmt.Scanln(&selectedAsset)
		selectedAsset = strings.ToUpper(selectedAsset)
	} else {
		selectedAsset = assetNames[0]
	}

	market, ok := markets[selectedAsset]
	if !ok {
		log.Fatal("Invalid asset selected.")
	}

	endTime, _ := paper.ParseEndTimeFromSlug(market.Slug)
	timeLeft := time.Until(endTime)

	fmt.Printf("\n🚀 Selected: %s\n", market.Slug)
	fmt.Printf("⏰ Time Left: %v\n", timeLeft.Round(time.Second))

	tokenMap := make(map[string]string)
	var outcomes []string
	for _, t := range market.Tokens {
		tokenMap[t.TokenID] = t.Outcome
		outcomes = append(outcomes, t.Outcome)
	}
	sort.Strings(outcomes)

	// 1.5 Show Market Depth (IDENTICAL TO REALBOT DISPLAY)
	fmt.Println("\n📊 Current Market Depth:")
	fmt.Println("Outcome      | Bid (Size)       | Ask (Size)       | Spread")
	fmt.Println("-------------|------------------|------------------|-------")
	tokenFullBids := make(map[string][]paper.MarketLevel)
	tokenFullAsks := make(map[string][]paper.MarketLevel)
	tokenFeeRates := make(map[string]int)

	for tid, outcome := range tokenMap {
		book, err := client.GetOrderBook(ctx, tid)
		if err != nil {
			fmt.Printf("%-12s | Error: %v\n", outcome, err)
			continue
		}
		
		tokenFullBids[outcome] = toMarketLevels(book.Bids)
		tokenFullAsks[outcome] = toMarketLevels(book.Asks)
		
		rate, _ := client.GetFeeRate(ctx, tid)
		tokenFeeRates[outcome] = rate
		
		bestBid, bestBidSize := 0.0, 0.0
		if len(book.Bids) > 0 {
			bestBid, _ = strconv.ParseFloat(book.Bids[0].Price, 64)
			bestBidSize, _ = strconv.ParseFloat(book.Bids[0].Size, 64)
		}
		
		bestAsk, bestAskSize := 0.0, 0.0
		if len(book.Asks) > 0 {
			bestAsk, _ = strconv.ParseFloat(book.Asks[0].Price, 64)
			bestAskSize, _ = strconv.ParseFloat(book.Asks[0].Size, 64)
		}
		
		spread := bestAsk - bestBid
		fmt.Printf("%-12s | %5.3f (%-6.0f) | %5.3f (%-6.0f) | %5.3f\n", 
			outcome, bestBid, bestBidSize, bestAsk, bestAskSize, spread)
	}

	// 2. Simplified Menu
	fmt.Println("\nActions: 1:Panic Buy (Buy BOTH + Merge), 2:Panic Sell (Sell BOTH)")
	fmt.Print("Choose action (1-2): ")
	var choice int
	fmt.Scanln(&choice)

	if choice < 1 || choice > 2 {
		log.Fatal("Invalid choice.")
	}

	fmt.Print("Target Amount (USDC per side): ")
	var amtStr string
	fmt.Scanln(&amtStr)
	amt, err := strconv.ParseFloat(amtStr, 64)
	if err != nil || amt <= 0 {
		log.Fatal("Invalid amount.")
	}

	targetShares := amt / 0.50 

	// 3. Execute
	if choice == 1 {
		fmt.Printf("🎯 Executing Robust Panic Buy for target $%.2f each...\n", amt)
		executeBoth(ctx, trader, market, outcomes, "BUY", targetShares, tokenFullBids, tokenFullAsks, tokenFeeRates)
	} else {
		fmt.Printf("🎯 Executing Robust Panic Sell for target $%.2f each...\n", amt)
		executeBoth(ctx, trader, market, outcomes, "SELL", targetShares, tokenFullBids, tokenFullAsks, tokenFeeRates)
	}
}

// findMarkets uses the EXACT logic from realbot's findMarkets
func findMarkets(ctx context.Context, restClient *api.RestClient) map[string]*api.Market {
	found := make(map[string]*api.Market)
	assets := []string{"btc", "eth"}

	markets, err := restClient.Get15mMarkets(ctx, nil)
	if err != nil {
		return found
	}

	for _, m := range markets {
		endTime, err := paper.ParseEndTimeFromSlug(m.Slug)
		if err == nil && time.Now().After(endTime) {
			continue
		}
		if err == nil && time.Until(endTime) < 30*time.Second {
			continue
		}

		slug := strings.ToLower(m.Slug)
		is15m := strings.Contains(slug, "15m") || strings.Contains(slug, "updown")

		for _, asset := range assets {
			key := strings.ToUpper(asset)
			if _, exists := found[key]; !exists && strings.Contains(slug, asset) && is15m {
				mCopy := m
				found[key] = &mCopy
			}
		}
	}
	return found
}

func executeBoth(ctx context.Context, trader *trading.RealTrader, market *api.Market, outcomes []string, side string, targetShares float64, tokenFullBids, tokenFullAsks map[string][]paper.MarketLevel, tokenFeeRates map[string]int) {
	shares := targetShares
	maxPriceSum := 1.10
	minPriceSum := 0.90

	if side == "BUY" {
		asks1 := tokenFullAsks[outcomes[0]]
		asks2 := tokenFullAsks[outcomes[1]]
		sort.Slice(asks1, func(i, j int) bool { return asks1[i].Price < asks1[j].Price })
		sort.Slice(asks2, func(i, j int) bool { return asks2[i].Price < asks2[j].Price })

		var totalMatchedLiquidity float64
		i, j := 0, 0
		for i < len(asks1) && j < len(asks2) {
			if asks1[i].Price + asks2[j].Price > maxPriceSum {
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
			fmt.Printf("⚠️ Capping shares at available matched liquidity: %.2f -> %.2f\n", shares, totalMatchedLiquidity)
			shares = totalMatchedLiquidity
		}
	} else {
		bids1 := tokenFullBids[outcomes[0]]
		bids2 := tokenFullBids[outcomes[1]]
		sort.Slice(bids1, func(i, j int) bool { return bids1[i].Price > bids1[j].Price })
		sort.Slice(bids2, func(i, j int) bool { return bids2[i].Price > bids2[j].Price })

		var totalMatchedLiquidity float64
		i, j := 0, 0
		for i < len(bids1) && j < len(bids2) {
			if bids1[i].Price + bids2[j].Price < minPriceSum {
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
			fmt.Printf("⚠️ Capping shares at available matched liquidity: %.2f -> %.2f\n", shares, totalMatchedLiquidity)
			shares = totalMatchedLiquidity
		}
	}

	if shares < 1.0 {
		log.Fatalf("Insufficient matched liquidity for minimum order.")
	}
	shares = math.Floor(shares)

	fmt.Printf("🚀 Executing: %s %.0f shares...\n", side, shares)
	fmt.Print("Confirm? (y/n): ")
	var confirm string
	fmt.Scanln(&confirm)
	if strings.ToLower(confirm) != "y" {
		log.Fatal("Cancelled.")
	}

	var wg sync.WaitGroup
	wg.Add(len(outcomes))

	results := make([]*trading.TradeResult, len(outcomes))
	errs := make([]error, len(outcomes))

	for idx, outcome := range outcomes {
		go func(o string, i int) {
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
			
			results[i] = res
			errs[i] = err
			printTradeResult(side+" "+o, res, err)
		}(outcome, idx)
	}
	wg.Wait()

	side1Success := errs[0] == nil && results[0] != nil && results[0].Success
	side2Success := errs[1] == nil && results[1] != nil && results[1].Success

	if side1Success != side2Success {
		failedIdx := 0
		if side1Success { failedIdx = 1 }
		failedOutcome := outcomes[failedIdx]
		failedToken := getTokenIDForOutcome(market, failedOutcome)
		fmt.Printf("\n⚠️ UNBALANCED FILL: %s failed. Retrying AGGRESSIVELY...\n", failedOutcome)

		for attempt := 1; attempt <= 10; attempt++ {
			time.Sleep(100 * time.Millisecond)
			var res *trading.TradeResult
			var err error
			rate := tokenFeeRates[failedOutcome]

			if side == "BUY" {
				res, err = trader.Buy(ctx, failedToken, failedOutcome, 0.99, shares, api.OrderTypeMarket, api.TIFGoodTilCancelled, rate)
			} else {
				res, err = trader.Sell(ctx, failedToken, failedOutcome, 0.10, shares, api.OrderTypeMarket, api.TIFGoodTilCancelled, rate)
			}

			if err == nil && res != nil && res.Success {
				fmt.Printf("✅ RECOVERY SUCCESS for %s after %d attempts!\n", failedOutcome, attempt)
				if failedIdx == 0 { side1Success = true } else { side2Success = true }
				break
			}
		}
	}

	if side1Success && side2Success {
		if newBal, err := trader.ForceRefreshBalance(ctx); err == nil {
			fmt.Printf("💵 Updated Balance: $%.2f\n", newBal)
		}

		if side == "BUY" {
			fmt.Printf("💰 Starting instant on-chain merge for %.0f pairs...\n", shares)
			go func(cid string, qty float64) {
				time.Sleep(2 * time.Second)
				mergeCtx, _ := context.WithTimeout(context.Background(), 60*time.Second)
				txHash, err := trader.MergeOnChain(mergeCtx, cid, qty)
				if err != nil {
					fmt.Printf("\n⚠️ Merge failed: %v\n", err)
				} else {
					fmt.Printf("\n✅ INSTANT MERGE SUCCESS! Tx: %s\n", txHash)
				}
			}(market.ConditionID, shares)
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

func getTokenIDForOutcome(m *api.Market, outcome string) string {
	for _, t := range m.Tokens {
		if strings.EqualFold(t.Outcome, outcome) {
			return t.TokenID
		}
	}
	return ""
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