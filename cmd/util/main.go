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
	mkt "Market-bot/internal/markets"
	"Market-bot/internal/paper"
	"Market-bot/internal/setup"
	"Market-bot/internal/trading"
	"github.com/charmbracelet/lipgloss"
	"github.com/joho/godotenv"
)

func main() {
	// Styled startup banner
	titleSt := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7C3AED"))
	dimSt := lipgloss.NewStyle().Foreground(lipgloss.Color("#6B7280"))
	warnSt := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#EF4444"))
	fmt.Println(lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#7C3AED")).
		Padding(0, 2).
		Render(titleSt.Render("🛠  UTILBOT  —  Panic Buy / Sell") + "\n" +
			warnSt.Render("⚠  Executes REAL trades with on-chain merge") + "\n" +
			dimSt.Render("Live order book  ·  Liquidity-capped execution")))

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
	fmt.Println("Select timeframe:")
	fmt.Println("1. 5m")
	fmt.Println("2. 15m")
	fmt.Print("Choice [default 2]: ")
	var tfChoice string
	_, _ = fmt.Scanln(&tfChoice)
	tfChoice = strings.TrimSpace(tfChoice)
	
	timeframe := "15m"
	if tfChoice == "1" {
		timeframe = "5m"
	} else if tfChoice == "2" || tfChoice == "" {
		timeframe = "15m"
	} else {
		log.Fatalf("❌ Invalid choice.")
	}

	fmt.Printf("🔍 Searching for active %s markets...\n", timeframe)
	markets := findMarkets(ctx, client, timeframe)

	if len(markets) == 0 {
		log.Fatalf("❌ No active %s markets found.", timeframe)
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
	        _, _ = fmt.Scanln(&selectedAsset)
	        selectedAsset = strings.ToUpper(selectedAsset)
	} else {		selectedAsset = assetNames[0]
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
		_ = wsMgr.Subscribe(ctx, map[string]interface{}{"type": "market", "assets_ids": assetIDs})
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
			// markets might require 1000 bps authorization even if endpoint returns 0
			if rate == 0 {
				tokenFeeRates[out] = 1000
				fmt.Printf("ℹ️  Fee rate for %s returned 0, forcing 1000 bps\n", out)
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
					tokenFullBids[out] = mkt.LevelsToPriceDepth(b.Bids, true)
					tokenFullAsks[out] = mkt.LevelsToPriceDepth(b.Asks, false)
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
					tokenFullBids[out] = mkt.LevelsToPriceDepth(book.Bids, true)
					tokenFullAsks[out] = mkt.LevelsToPriceDepth(book.Asks, false)
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
	_, _ = fmt.Scanln(&choice)
	
	var inputAmount float64
	if choice == 1 {
		fmt.Print("USDC to Panic Buy (min 5): ")
		_, _ = fmt.Scanln(&inputAmount)
		executeBoth(ctx, trader, market, outcomes, "BUY", inputAmount, tokenFullBids, tokenFullAsks, tokenFeeRates)
	} else if choice == 2 {
		fmt.Print("USDC to Panic Sell (min 5): ")
		_, _ = fmt.Scanln(&inputAmount)
		executeBoth(ctx, trader, market, outcomes, "SELL", inputAmount, tokenFullBids, tokenFullAsks, tokenFeeRates)
	} else {
		log.Fatal("Invalid choice.")
	}
}

func findMarkets(ctx context.Context, restClient *api.RestClient, timeframe string) map[string]*api.Market {
	found := make(map[string]*api.Market)
	assets := []string{"btc", "eth"}
	
	// Set dynamic spare time based on timeframe to prevent volatility
	spareTime := 31 * time.Minute
	if timeframe == "5m" {
		spareTime = 11 * time.Minute // 5m markets are created closer to expiration, use 11m
	}

	for {
		select {
		case <-ctx.Done():
			return found
		default:
			if ms, err := restClient.GetMarketsByTimeframe(ctx, nil, timeframe); err == nil {
				for _, m := range ms {
					et, _ := paper.ParseEndTimeFromSlug(m.Slug)
					if time.Now().After(et) || time.Until(et) < spareTime {
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
		combinedPrice := prices[outcomes[0]] + prices[outcomes[1]]
		if combinedPrice > 0 {
			shares = targetShares / combinedPrice
		}
		
		// To avoid "min size: $1" errors on Market buys due to price being slightly below $1.00 or floating point issues:
		// Let's ensure the *MakerAmount* will be at least 1.05 USDC if they try to buy a very small amount.
		// For a market buy, price used is 0.99, so makerAmount = shares * 0.99.
		// We want shares * 0.99 >= 1.05  => shares >= 1.05 / 0.99
		minShares := 1.05 / 0.99
		if shares < minShares {
			shares = minShares
		}
		
		totalLiq := mkt.EstimateMatchedLiquidity(
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
		totalLiq := mkt.EstimateMatchedLiquidity(
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

	expectedFee0 := shares * (float64(tokenFeeRates[outcomes[0]]) / 10000.0)
	expectedFee1 := shares * (float64(tokenFeeRates[outcomes[1]]) / 10000.0)
	
	if side == "BUY" {
		fmt.Printf("💸 Expected fee deduction: %s=%.4f shares, %s=%.4f shares\n", outcomes[0], expectedFee0, outcomes[1], expectedFee1)
	} else {
		expectedUsdcFee0 := expectedFee0 * prices[outcomes[0]]
		expectedUsdcFee1 := expectedFee1 * prices[outcomes[1]]
		fmt.Printf("💸 Expected fee deduction: %s=$%.4f, %s=$%.4f\n", outcomes[0], expectedUsdcFee0, outcomes[1], expectedUsdcFee1)
	}

	unitName := "shares"
	if side == "SELL" {
		unitName = "USDC"
	}
	fmt.Printf("🚀 Executing: %s %.0f %s (Est. total value: $%.2f USDC). Confirm? (y/n): ", side, shares, unitName, totalValue)
	var confirm string
	_, _ = fmt.Scanln(&confirm)
	if strings.ToLower(strings.TrimSpace(confirm)) != "y" {		log.Fatal("Cancelled.")
	}

	var initialUSDC float64
	if side == "SELL" {
		initialUSDC, _ = trader.ForceRefreshBalance(ctx)

		splitAmount := shares // 1 USDC per split → 1 YES + 1 NO
		fmt.Printf("🔄 Splitting $%.0f USDC into token pairs...\n", splitAmount)
		splitCtx, cancelSplit := context.WithTimeout(ctx, 90*time.Second)
		defer cancelSplit()
		tx, err := trader.SplitOnChain(splitCtx, market.ConditionID, splitAmount, len(market.Tokens))
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
	for idx, out := range outcomes {
		go func(o string, i int) {
			defer wg.Done()
			tid := mkt.GetTokenIDForOutcome(market, o)
			execShares := shares
			rate := tokenFeeRates[o]
			if rate == -1 {
				rate = 0 // Default to 0 (fee-free) if fetch failed, safer than 1000
				log.Printf("⚠️ Fee rate fetch failed for %s, using 0 bps", o)
			}
			
			startReq := time.Now()
			
			if side == "BUY" {
				// Use 0.99 for Market Buys to ensure fill and bypass limits if needed
				price := 0.99
				results[i], errs[i] = trader.Buy(ctx, tid, o, price, execShares, api.OrderTypeMarket, api.TIFFillAndKill, rate)
			} else {
				// Use 0.01 for Market Sells to ensure fill and bypass 5-share limit
				price := 0.01
				// Use FOK for Panic Sell to match realbot behavior and avoid GTC price validation issues
				results[i], errs[i] = trader.Sell(ctx, tid, o, price, execShares, api.OrderTypeMarket, api.TIFFillAndKill, rate)
			}
			latency := time.Since(startReq)
			printTradeResult(side+" "+o, results[i], errs[i], rate, execShares, latency)
		}(out, idx)
	}
	wg.Wait()

	if (errs[0] == nil && results[0].Success) != (errs[1] == nil && results[1].Success) {
		fmt.Println("⚠️  UNBALANCED FILL! Attempting aggressive recovery...")
		failedIdx := 0
		if errs[0] == nil && results[0].Success {
			failedIdx = 1
		}
		failedOutcome := outcomes[failedIdx]
		tid := mkt.GetTokenIDForOutcome(market, failedOutcome)

		rate := tokenFeeRates[failedOutcome]
		if rate == 0 {
			rate = 1000
		}

		retryCount := 0
		for retryCount < 10 { // Max 10 retries
			retryCount++
			fmt.Printf("🔄 Recovery attempt #%d for %s...\n", retryCount, failedOutcome)

			var retryRes *trading.TradeResult
			var retryErr error

			startReq := time.Now()

			if side == "BUY" {
				// Use $0.99 cap for buy recovery to guarantee fill
				retryRes, retryErr = trader.Buy(ctx, tid, failedOutcome, 0.99, shares, api.OrderTypeMarket, api.TIFFillAndKill, rate)
			} else {
				// Use 0.01 for market sell recovery to guarantee fill and bypass 5-share limit
				retryPrice := 0.01
				retryRes, retryErr = trader.Sell(ctx, tid, failedOutcome, retryPrice, shares, api.OrderTypeMarket, api.TIFFillAndKill, rate)
			}

			latency := time.Since(startReq)

			if retryErr == nil && retryRes != nil && retryRes.Success {
				fmt.Printf("✅ Recovery SUCCESS for %s after %d retries! (Latency: %v)\n", failedOutcome, retryCount, latency)
				results[failedIdx] = retryRes
				errs[failedIdx] = nil
				break
			}

			msg := "Unknown error"
			if retryErr != nil {
				msg = retryErr.Error()
			} else if retryRes != nil {
				msg = retryRes.Message
			}
			fmt.Printf("❌ Recovery attempt #%d failed: %s (Latency: %v)\n", retryCount, msg, latency)
			time.Sleep(500 * time.Millisecond)
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
			token0 := mkt.GetTokenIDForOutcome(market, outcomes[0])
			token1 := mkt.GetTokenIDForOutcome(market, outcomes[1])
			fmt.Printf("🔍 Querying balances for tokens: %s (%s), %s (%s)\n", outcomes[0], token0, outcomes[1], token1)

			bal0, bal1, err0, err1 := trader.QueryBalancedCTFBalances(context.Background(), token0, token1, shares)
			if err0 != nil || err1 != nil {
				feeRate0 := float64(tokenFeeRates[outcomes[0]]) / 10000.0
				feeRate1 := float64(tokenFeeRates[outcomes[1]]) / 10000.0
				bal0 = shares * (1.0 - feeRate0)
				bal1 = shares * (1.0 - feeRate1)
				fmt.Printf("⚠️ Failed to query balances (err0=%v, err1=%v), falling back to fee-deducted shares (%.6f, %.6f)\n", err0, err1, bal0, bal1)
			}
			
			actualFee0 := shares - bal0
			actualFee1 := shares - bal1
			if actualFee0 < 0 { actualFee0 = 0 }
			if actualFee1 < 0 { actualFee1 = 0 }
			
			fmt.Printf("📊 On-chain balances: %s=%.4f, %s=%.4f\n", outcomes[0], bal0, outcomes[1], bal1)
			fmt.Printf("💸 ACTUAL DEDUCTED FEE: %s=%.4f shares, %s=%.4f shares\n", outcomes[0], actualFee0, outcomes[1], actualFee1)

			// Don't floor the merge quantity! CTF supports 6 decimals.
			// Use the minimum available balance directly to merge everything.
			minQty := math.Min(math.Min(bal0, bal1), shares)

			// Only filter out tiny dust (< 0.000001)
			if minQty >= 0.000001 {
				fmt.Printf("🔄 Merging %.6f pairs...\n", minQty)
				mergeCtx, cancelMerge := context.WithTimeout(context.Background(), 90*time.Second)
				tx, mergeErr := trader.MergeOnChain(mergeCtx, market.ConditionID, minQty, len(market.Tokens))
				cancelMerge()
				if mergeErr != nil {
					fmt.Printf("❌ Merge failed: %v\n", mergeErr)
				} else {
					fmt.Printf("✅ Merge successful! Tx: %s\n", tx)
				}
			} else {
				fmt.Printf("⚠️ Not enough balanced pairs to merge (min=%.6f)\n", minQty)
			}
		} else if side == "SELL" {
			fmt.Println("💰 Sell success! Waiting for on-chain balances to update...")
			time.Sleep(3 * time.Second)

			finalUSDC, err := trader.ForceRefreshBalance(ctx)
			if err != nil {
				fmt.Printf("⚠️ Failed to fetch final USDC balance: %v\n", err)
			} else {
				// We started with initialUSDC, split 'shares' amount, then sold.
				// actual received from the sale = finalUSDC - (initialUSDC - shares)
				expectedRemaining := initialUSDC - shares
				actualReceived := finalUSDC - expectedRemaining

				// Expected to receive totalValue, but paid fee and slippage
				expectedReceived := totalValue
				totalActualFee := expectedReceived - actualReceived
				if totalActualFee < 0 { totalActualFee = 0 }

				fmt.Printf("📊 On-chain USDC: Initial=$%.2f, Final=$%.2f\n", initialUSDC, finalUSDC)
				fmt.Printf("💵 Actual Received: $%.4f USDC (Expected ~$%.2f)\n", actualReceived, expectedReceived)
				fmt.Printf("💸 ACTUAL DEDUCTED FEE & SLIPPAGE: $%.4f USDC\n", totalActualFee)
			}
		}
	}
}

func printTradeResult(act string, res *trading.TradeResult, err error, rate int, shares float64, latency time.Duration) {
	if err != nil || (res != nil && !res.Success) {
		msg := "Error"
		if err != nil {
			msg = err.Error()
		} else if res != nil {
			msg = res.Message
		}
		fmt.Printf("FAILED: %s - %s (Latency: %v)\n", act, msg, latency)
	} else {
		actualFeeRate := float64(rate) / 10000.0
		feePercentage := float64(rate) / 100.0 // bps to percentage
		if strings.HasPrefix(act, "BUY") {
			// For BUY, fee is deducted from the shares you receive.
			// However, since we executed at price 0.99 for Market Buy, the actual shares matched might differ.
			// The API charges fee on the matched shares. For simplicity and estimation parity, we show the expected fee based on the requested shares.
			feeShares := shares * actualFeeRate
			fmt.Printf("SUCCESS: %s - OrderID: %s | Estimated Fee (%.2f%%): %.4f shares (Latency: %v)\n", act, res.OrderID, feePercentage, feeShares, latency)
		} else {
			// For SELL, fee is deducted from the USDC you receive.
			// Since we executed at price 0.01 for Market Sell, the actual USDC matched might differ.
			// For simplicity and estimation parity, we show the expected fee based on the requested shares.
			feeUSDC := shares * actualFeeRate
			fmt.Printf("SUCCESS: %s - OrderID: %s | Estimated Fee (%.2f%%): $%.4f USDC (Latency: %v)\n", act, res.OrderID, feePercentage, feeUSDC, latency)
		}
	}
}
