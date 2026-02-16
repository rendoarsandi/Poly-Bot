package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Sync CLOB cached allowance with on-chain state
	fmt.Println("🔄 Syncing CLOB balance allowance...")
	if err := trader.UpdateBalanceAllowance(ctx); err != nil {
		log.Printf("⚠️ Failed to update balance allowance: %v", err)
	} else {
		fmt.Println("✅ CLOB balance allowance synced")
	}

	// 1. Find markets
	fmt.Println("🔍 Searching for active 15m markets...")
	markets := findMarkets(ctx, client)

	if len(markets) == 0 {
		log.Fatal("❌ No active 15m markets found.")
	}

	// 1.2 Asset selection
	var selectedAsset string
	var assetNames []string
	for k := range markets {
		assetNames = append(assetNames, k)
	}
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

	// 1.3 WebSocket Setup
	wsMgr := api.NewWSManager("")
	if err := wsMgr.Connect(ctx); err != nil {
		fmt.Printf("⚠️ WS failed: %v\n", err)
	}
	defer wsMgr.Close()

	tokenToOutcome := make(map[string]string)
	tokenMap := make(map[string]string)
	var outcomes []string
	var assetIDs []string
	for _, t := range market.Tokens {
		assetIDs = append(assetIDs, t.TokenID)
		tokenToOutcome[t.TokenID] = t.Outcome
		tokenMap[t.TokenID] = t.Outcome
		outcomes = append(outcomes, t.Outcome)
	}
	sort.Strings(outcomes)

	if wsMgr.IsConnected() {
		wsMgr.Subscribe(ctx, map[string]interface{}{"type": "market", "assets_ids": assetIDs})
	}
	wsMsgChan := wsMgr.StartStreaming(ctx)

	// Price and Liquidity state
	tokenBids := make(map[string]float64)
	tokenAsks := make(map[string]float64)
	tokenFullBids := make(map[string][]paper.MarketLevel)
	tokenFullAsks := make(map[string][]paper.MarketLevel)
	tokenFeeRates := make(map[string]int)
	for tid, out := range tokenMap {
		var rate int
		var err error
		for attempt := 1; attempt <= 3; attempt++ {
			rate, err = client.GetFeeRate(ctx, tid)
			if err == nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}

		if err == nil {
			tokenFeeRates[out] = rate
			// 15m markets require 1000 bps authorization even if endpoint returns 0
			if rate == 0 {
				tokenFeeRates[out] = 1000
				fmt.Printf("ℹ️  Fee rate for %s returned 0, forcing 1000 bps (required for 15m)\n", out)
			} else {
				fmt.Printf("ℹ️  Fee rate for %s: %d bps\n", out, rate)
			}
		} else {
			tokenFeeRates[out] = 1000 // Fallback to 1000 bps (10%) as required by API
			fmt.Printf("⚠️  Fee fetch failed for %s, using 1000 bps fallback\n", out)
		}
	}

	// Input handler
	inputChan := make(chan string)
	go func() {
		reader := bufio.NewReader(os.Stdin)
		text, _ := reader.ReadString('\n')
		inputChan <- text
	}()

	fmt.Print("\033[?25l") // Hide cursor
	ticker := time.NewTicker(200 * time.Millisecond)

	for {
		select {
		case msg := <-wsMsgChan:
			if books, err := api.ParseOrderBooks(msg); err == nil {
				for _, b := range books {
					out := tokenToOutcome[b.AssetID]
					if out == "" {
						continue
					}
					// Find best bid (highest) and best ask (lowest) like realbot
					bid, ask := 0.0, 1.0
					for _, order := range b.Bids {
						p, _ := strconv.ParseFloat(order.Price, 64)
						if p > bid {
							bid = p
						}
					}
					for _, order := range b.Asks {
						p, _ := strconv.ParseFloat(order.Price, 64)
						if p < ask && p > 0 {
							ask = p
						}
					}
					if ask >= 1.0 {
						ask = 0
					}
					if bid > 0 {
						tokenBids[out] = bid
					}
					if ask > 0 {
						tokenAsks[out] = ask
					}
					tokenFullBids[out] = toMarketLevels(b.Bids)
					tokenFullAsks[out] = toMarketLevels(b.Asks)
				}
			}
		case <-ticker.C:
			// REST Refresh: always poll for fresh prices and depth
			for tid, out := range tokenMap {
				if book, err := client.GetOrderBook(ctx, tid); err == nil {
					bid, ask := 0.0, 1.0
					for _, b := range book.Bids {
						p, _ := strconv.ParseFloat(b.Price, 64)
						if p > bid {
							bid = p
						}
					}
					for _, a := range book.Asks {
						p, _ := strconv.ParseFloat(a.Price, 64)
						if p < ask && p > 0 {
							ask = p
						}
					}
					if ask >= 1.0 {
						ask = 0
					}
					if bid > 0 {
						tokenBids[out] = bid
					}
					if ask > 0 {
						tokenAsks[out] = ask
					}
					tokenFullBids[out] = toMarketLevels(book.Bids)
					tokenFullAsks[out] = toMarketLevels(book.Asks)
				}
			}

			// Render
			fmt.Print("\033[H\033[2J")
			fmt.Printf("🚀 Market: %s\n", market.Slug)
			endTime, _ := paper.ParseEndTimeFromSlug(market.Slug)
			fmt.Printf("⏰ Time Left: %v\n\n", time.Until(endTime).Round(time.Second))
			fmt.Println("Outcome      | Bid (Size)       | Ask (Size)       | Spread")
			fmt.Println("-------------|------------------|------------------|-------")
			for _, out := range outcomes {
				b, a := tokenBids[out], tokenAsks[out]
				bs, as := 0.0, 0.0
				if len(tokenFullBids[out]) > 0 {
					bs = tokenFullBids[out][0].Size
				}
				if len(tokenFullAsks[out]) > 0 {
					as = tokenFullAsks[out][0].Size
				}
				fmt.Printf("%-12s | %5.3f (%-6.0f) | %5.3f (%-6.0f) | %5.3f\n", out, b, bs, a, as, a-b)
			}
			sum := tokenAsks[outcomes[0]] + tokenAsks[outcomes[1]]
			if sum < 1.0 && sum > 0.1 {
				fmt.Printf("\n💰 ARB: %.3f (%.1f%%)\n", sum, (1-sum)*100)
			}
			fmt.Println("\nPress ENTER to stop live view and take action...")
		case <-inputChan:
			goto takeAction
		case <-ctx.Done():
			fmt.Print("\033[?25h")
			return
		}
	}

takeAction:
	fmt.Print("\033[?25h")
	fmt.Println("\nActions: 1:Panic Buy, 2:Panic Sell")
	fmt.Print("Choice: ")
	var choice int
	fmt.Scanln(&choice)
	fmt.Print("Shares per side (min 5): ")
	var shares float64
	fmt.Scanln(&shares)

	if choice == 1 {
		executeBoth(ctx, trader, market, outcomes, "BUY", shares, tokenFullBids, tokenFullAsks, tokenFeeRates)
	} else {
		executeBoth(ctx, trader, market, outcomes, "SELL", shares, tokenFullBids, tokenFullAsks, tokenFeeRates)
	}
}

func findMarkets(ctx context.Context, restClient *api.RestClient) map[string]*api.Market {
	found := make(map[string]*api.Market)
	assets := []string{"btc", "eth"}
	for {
		select {
		case <-ctx.Done():
			return found
		default:
			if ms, err := restClient.Get15mMarkets(ctx, nil); err == nil {
				for _, m := range ms {
					et, _ := paper.ParseEndTimeFromSlug(m.Slug)
					if time.Now().After(et) || time.Until(et) < 30*time.Second {
						continue
					}
					for _, a := range assets {
						if strings.Contains(strings.ToLower(m.Slug), a) {
							mCopy := m
							found[strings.ToUpper(a)] = &mCopy
						}
					}
				}
			}
			if len(found) > 0 {
				return found
			}
			fmt.Print(".")
			time.Sleep(2 * time.Second)
		}
	}
}

func executeBoth(ctx context.Context, trader *trading.RealTrader, market *api.Market, outcomes []string, side string, targetShares float64, tokenFullBids, tokenFullAsks map[string][]paper.MarketLevel, tokenFeeRates map[string]int) {
	// Determine execution pricing
	prices := make(map[string]float64, len(outcomes))
	for _, out := range outcomes {
		var price float64
		if side == "BUY" {
			price = 0.99
			bestAsk := 1.0
			found := false
			for _, lvl := range tokenFullAsks[out] {
				if lvl.Price > 0 && lvl.Price < bestAsk {
					bestAsk = lvl.Price
					found = true
				}
			}
			if found {
				price = bestAsk
			}
		} else {
			price = 0.01
			bestBid := 0.0
			found := false
			for _, lvl := range tokenFullBids[out] {
				if lvl.Price > bestBid {
					bestBid = lvl.Price
					found = true
				}
			}
			if found {
				price = bestBid
			}
			if price >= 1.0 {
				price = 0.99
			} else if price < 0.01 {
				price = 0.01
			}
		}
		prices[out] = price
	}

	shares := targetShares

	if side == "BUY" {
		totalLiq := estimateMatchedLiquidity(
			append([]paper.MarketLevel(nil), tokenFullAsks[outcomes[0]]...),
			append([]paper.MarketLevel(nil), tokenFullAsks[outcomes[1]]...),
			func(i, j int, levels []paper.MarketLevel) bool { return levels[i].Price < levels[j].Price },
			func(p1, p2 float64) bool { return p1+p2 <= 1.10 },
		)
		if shares > totalLiq {
			fmt.Printf("⚠️  Capping shares by liquidity: %.0f -> %.0f\n", shares, totalLiq)
			shares = math.Floor(totalLiq)
		}
	} else {
		totalLiq := estimateMatchedLiquidity(
			append([]paper.MarketLevel(nil), tokenFullBids[outcomes[0]]...),
			append([]paper.MarketLevel(nil), tokenFullBids[outcomes[1]]...),
			func(i, j int, levels []paper.MarketLevel) bool { return levels[i].Price > levels[j].Price },
			func(p1, p2 float64) bool { return p1+p2 >= 0.90 },
		)
		if shares > totalLiq {
			fmt.Printf("⚠️  Capping shares by liquidity: %.0f -> %.0f\n", shares, totalLiq)
			shares = math.Floor(totalLiq)
		}
	}

	totalValue := shares * (prices[outcomes[0]] + prices[outcomes[1]])
	fmt.Printf("🚀 Executing: %s %.0f shares (Est. total value: $%.2f USDC). Confirm? (y/n): ", side, shares, totalValue)
	var confirm string
	fmt.Scanln(&confirm)
	if strings.ToLower(strings.TrimSpace(confirm)) != "y" {
		log.Fatal("Cancelled.")
	}

	// For SELL: Split USDC into YES+NO tokens first
	if side == "SELL" {
		splitAmount := shares // 1 USDC per split → 1 YES + 1 NO
		fmt.Printf("🔄 Splitting $%.0f USDC into token pairs...\n", splitAmount)
		splitCtx, cancelSplit := context.WithTimeout(ctx, 90*time.Second)
		defer cancelSplit()
		tx, err := trader.SplitOnChain(splitCtx, market.ConditionID, splitAmount)
		if err != nil {
			log.Fatalf("❌ Split failed: %v", err)
		}
		fmt.Printf("✅ Split successful! Tx: %s\n", tx)
		fmt.Println("⏳ Waiting for on-chain settlement...")
		time.Sleep(3 * time.Second)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	results, errs := make([]*trading.TradeResult, 2), make([]error, 2)

	// Execute both sides in parallel
	for idx, out := range outcomes {
		go func(o string, i int) {
			defer wg.Done()
			tid := getTokenIDForOutcome(market, o)
			execShares := shares
			rate := tokenFeeRates[o]
			if rate == -1 {
				rate = 0 // Default to 0 (fee-free) if fetch failed
			}

			if side == "BUY" {
				price := prices[o]
				// Use FOK for atomic execution
				results[i], errs[i] = trader.Buy(ctx, tid, o, price, execShares, api.OrderTypeMarket, api.TIFFillOrKill, rate)
			} else {
				price := prices[o]
				results[i], errs[i] = trader.Sell(ctx, tid, o, price, execShares, api.OrderTypeMarket, api.TIFFillOrKill, rate)
			}
			printTradeResult(side+" "+o, results[i], errs[i])
		}(out, idx)
	}
	wg.Wait()

	// Check for unbalanced fill
	side1Success := errs[0] == nil && results[0].Success
	side2Success := errs[1] == nil && results[1].Success

	if side1Success != side2Success {
		fmt.Println("⚠️  UNBALANCED FILL! Attempting aggressive recovery...")
		
		// Identify failed side
		failedIdx := 0
		if side1Success {
			failedIdx = 1
		}
		failedOutcome := outcomes[failedIdx]
		tid := getTokenIDForOutcome(market, failedOutcome)

		rate := tokenFeeRates[failedOutcome]
		if rate == 0 {
			rate = 1000
		}

		// Robust recovery loop: 40 attempts, 50ms interval (matches realbot)
		retryCount := 0
		const maxRetries = 40
		
		for retryCount < maxRetries {
			retryCount++
			fmt.Printf("🔄 Recovery attempt #%d for %s...\n", retryCount, failedOutcome)

			var retryRes *trading.TradeResult
			var retryErr error

			// Short delay to allow API/Nonce to settle
			time.Sleep(50 * time.Millisecond)

			if side == "BUY" {
				// Use $0.99 cap for buy recovery to guarantee fill
				retryRes, retryErr = trader.Buy(ctx, tid, failedOutcome, 0.99, shares, api.OrderTypeMarket, api.TIFFillOrKill, rate)
			} else {
				// Use latest bid-driven price for sell recovery
				retryPrice := prices[failedOutcome]
				if retryPrice <= 0 || retryPrice >= 1 {
					retryPrice = 0.5
				}
				retryRes, retryErr = trader.Sell(ctx, tid, failedOutcome, retryPrice, shares, api.OrderTypeMarket, api.TIFFillOrKill, rate)
			}

			if retryErr == nil && retryRes != nil && retryRes.Success {
				fmt.Printf("✅ Recovery SUCCESS for %s after %d retries!\n", failedOutcome, retryCount)
				results[failedIdx] = retryRes
				errs[failedIdx] = nil
				side1Success = true // Mark as success to proceed to merge
				side2Success = true
				break
			}

			msg := "Unknown error"
			if retryErr != nil {
				msg = retryErr.Error()
			} else if retryRes != nil {
				msg = retryRes.Message
			}
			fmt.Printf("❌ Recovery attempt #%d failed: %s\n", retryCount, msg)
		}

		if (errs[0] == nil && results[0].Success) != (errs[1] == nil && results[1].Success) {
			log.Fatal("🚨 CRITICAL: Failed to balance positions after recovery attempts. Manual intervention required!")
		}
	}

	if (errs[0] == nil && results[0].Success) && (errs[1] == nil && results[1].Success) {
		if side == "BUY" {
			fmt.Println("💰 Buy success! Querying on-chain balances for merge...")
			time.Sleep(3 * time.Second) // Initial wait for on-chain state to settle

			// Query actual on-chain CTF balances to merge the correct amount
			token0 := getTokenIDForOutcome(market, outcomes[0])
			token1 := getTokenIDForOutcome(market, outcomes[1])
			fmt.Printf("🔍 Querying balances for tokens: %s (%s), %s (%s)\n", outcomes[0], token0, outcomes[1], token1)

			bal0, bal1, err0, err1 := queryBalancedCTFBalances(context.Background(), trader, token0, token1, shares)
			if err0 != nil || err1 != nil {
				fmt.Printf("⚠️ Failed to query balances (err0=%v, err1=%v), falling back to requested shares\n", err0, err1)
				bal0, bal1 = shares, shares
			}
			fmt.Printf("📊 On-chain balances: %s=%.2f, %s=%.2f\n", outcomes[0], bal0, outcomes[1], bal1)

			// Don't floor the merge quantity! CTF supports 6 decimals.
			// Use the minimum available balance directly to merge everything.
			minQty := math.Min(math.Min(bal0, bal1), shares)

			// Only filter out tiny dust (< 0.000001)
			if minQty >= 0.000001 {
				fmt.Printf("🔄 Merging %.6f pairs...\n", minQty)
				mergeCtx, cancelMerge := context.WithTimeout(context.Background(), 90*time.Second)
				tx, mergeErr := trader.MergeOnChain(mergeCtx, market.ConditionID, minQty)
				cancelMerge()
				if mergeErr != nil {
					fmt.Printf("❌ Merge failed: %v\n", mergeErr)
				} else {
					fmt.Printf("✅ Merge successful! Tx: %s\n", tx)
				}
			} else {
				fmt.Printf("⚠️ Not enough balanced pairs to merge (min=%.6f)\n", minQty)
			}
		}
	}
}

func queryBalancedCTFBalances(ctx context.Context, trader *trading.RealTrader, token0, token1 string, expectedShares float64) (float64, float64, error, error) {
	const maxAttempts = 8
	const settleDelay = 500 * time.Millisecond

	var bal0, bal1 float64
	var err0, err1 error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		bal0, err0 = trader.GetCTFBalanceFloat(ctx, token0)
		bal1, err1 = trader.GetCTFBalanceFloat(ctx, token1)

		if err0 == nil && err1 == nil {
			minBal := math.Min(bal0, bal1)
			if minBal >= 0.000001 {
				if math.Abs(bal0-bal1) <= 0.000001 || minBal >= expectedShares-0.05 {
					return bal0, bal1, nil, nil
				}
			}
		}

		if attempt < maxAttempts {
			time.Sleep(settleDelay)
		}
	}

	return bal0, bal1, err0, err1
}

func estimateMatchedLiquidity(levels1, levels2 []paper.MarketLevel, less func(i, j int, levels []paper.MarketLevel) bool, priceCheck func(p1, p2 float64) bool) float64 {
	sort.Slice(levels1, func(i, j int) bool { return less(i, j, levels1) })
	sort.Slice(levels2, func(i, j int) bool { return less(i, j, levels2) })

	totalLiq := 0.0
	i, j := 0, 0
	for i < len(levels1) && j < len(levels2) {
		if !priceCheck(levels1[i].Price, levels2[j].Price) {
			break
		}

		matched := math.Min(levels1[i].Size, levels2[j].Size)
		totalLiq += matched
		if levels1[i].Size <= levels2[j].Size {
			levels2[j].Size -= levels1[i].Size
			i++
		} else {
			levels1[i].Size -= levels2[j].Size
			j++
		}
	}

	return totalLiq
}

func toMarketLevels(levels []api.PriceLevel) []paper.MarketLevel {
	res := make([]paper.MarketLevel, len(levels))
	for i, l := range levels {
		p, _ := strconv.ParseFloat(l.Price, 64)
		s, _ := strconv.ParseFloat(l.Size, 64)
		res[i] = paper.MarketLevel{Price: p, Size: s}
	}
	return res
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
		msg := "Error"
		if err != nil {
			msg = err.Error()
		} else if res != nil {
			msg = res.Message
		}
		fmt.Printf("FAILED: %s - %s\n", act, msg)
	} else {
		fmt.Printf("SUCCESS: %s - OrderID: %s\n", act, res.OrderID)
	}
}
