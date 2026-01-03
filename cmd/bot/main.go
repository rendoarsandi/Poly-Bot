package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
)

const (
	StartingBalance = 1000.0 // $1000 paper trading balance
	StatsInterval   = 15     // Print stats every 15 seconds
)

func contains(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("Critical error: %v", err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Print startup header
	paper.PrintHeader(StartingBalance)

	// Initialize persistent components (survive market rotation)
	engine := paper.NewEngine(StartingBalance)

	// Load config
	cfg, err := core.LoadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	restClient := api.NewRestClient("")

	// Main market rotation loop
	for {
		select {
		case <-ctx.Done():
			fmt.Println("\n🛑 Shutting down...")
			return nil
		default:
		}

		// Find next market
		market, err := findNextMarket(restClient, cfg.MarketSlug)
		if err != nil {
			fmt.Printf("⚠️  No market found: %v. Retrying in 30s...\n", err)
			time.Sleep(30 * time.Second)
			continue
		}

		// Trade this market
		result, err := tradeMarket(ctx, market, engine, restClient)
		if err != nil {
			if strings.Contains(err.Error(), "context canceled") {
				return nil
			}
			fmt.Printf("⚠️  Market error: %v\n", err)
		}

		// Show result
		if result != nil {
			fmt.Printf("\n💰 Market %s completed: Realized PnL: $%.2f\n", market.Slug, result.realizedPnL)
		}

		// Brief pause before next market
		fmt.Println("\n🔄 Looking for next market in 10 seconds...")
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(10 * time.Second):
		}
	}
}

type marketResult struct {
	realizedPnL float64
	trades      int
}

func findNextMarket(restClient *api.RestClient, preferredSlug string) (*api.Market, error) {
	var market *api.Market
	var err error

	// Try preferred slug first
	if preferredSlug != "" {
		market, err = restClient.GetMarket(preferredSlug)
		if err == nil && market.Active && !market.Closed {
			return market, nil
		}
	}

	// Scan for 15m markets
	fmt.Println("🔎 Scanning for active 15m markets...")
	markets, err := restClient.Get15mMarkets(nil)
	if err == nil && len(markets) > 0 {
		fmt.Printf("✅ Found: %s\n", markets[0].Slug)
		return &markets[0], nil
	}

	// Fallback to general scanner
	fmt.Println("🔎 Falling back to general scanner...")
	allMarkets, err := restClient.ListMarkets()
	if err == nil {
		for _, m := range allMarkets {
			slug := m.MarketSlug
			if slug == "" {
				slug = m.Slug
			}
			if (contains(slug, "bitcoin") || contains(slug, "btc") || contains(slug, "eth")) && contains(slug, "price") {
				market, err = restClient.GetMarket(slug)
				if err == nil {
					return market, nil
				}
			}
		}
	}

	return nil, fmt.Errorf("no suitable markets found")
}

func tradeMarket(ctx context.Context, market *api.Market, engine *paper.Engine, restClient *api.RestClient) (*marketResult, error) {
	fmt.Printf("\n📊 Trading Market: %s\n", market.Slug)
	fmt.Printf("   Condition ID: %s\n", market.ConditionID)
	for _, t := range market.Tokens {
		fmt.Printf("   • %s: %s...\n", t.Outcome, t.TokenID[:20])
	}

	// Setup WebSocket
	fmt.Println("\n🔌 Connecting to WebSocket...")
	wsMgr := api.NewWSManager("")
	if err := wsMgr.Connect(ctx); err != nil {
		return nil, fmt.Errorf("websocket connect failed: %w", err)
	}
	defer wsMgr.Close()

	// Subscribe to Order Books
	var assetIDs []string
	var outcomeNames []string
	tokenMap := make(map[string]string)
	for _, token := range market.Tokens {
		assetIDs = append(assetIDs, token.TokenID)
		tokenMap[token.TokenID] = token.Outcome
		outcomeNames = append(outcomeNames, token.Outcome)
	}

	sub := map[string]interface{}{
		"type":       "market",
		"assets_ids": assetIDs,
	}
	if err := wsMgr.Subscribe(ctx, sub); err != nil {
		return nil, fmt.Errorf("subscribe failed: %w", err)
	}
	fmt.Println("✅ Subscribed to order book updates")

	// Initialize per-market components
	orderBook := paper.NewOrderBook()
	display := paper.NewDisplay(engine, time.Duration(StatsInterval)*time.Second)

	ladderConfig := paper.LadderConfig{
		Levels:         3,
		SharesPerLevel: 25,    // Reduced from 50 to 25
		PriceStep:      0.01,
		BasePrice:      0.48,
	}
	ladderMgr := paper.NewLadderManager(orderBook, ladderConfig)

	riskConfig := paper.RiskConfig{
		MaxExposure:        500.0,
		MaxUnmatchedRatio:  0.20,
		MaxUnmatchedShares: 75.0,   // Reduced from 150 to 75 (3 levels × 25)
		SkewThreshold:      0.15,
		KillSwitchDrawdown: 1.0,    // Disabled for testing (100% = never triggers)
	}
	riskMgr := paper.NewRiskManager(riskConfig, engine, orderBook, outcomeNames)

	marketMonitor := paper.NewMarketMonitor(engine, orderBook, ladderMgr, riskMgr)

	// Parse market end time
	endTime, err := paper.ParseEndTimeFromSlug(market.Slug)
	if err != nil {
		endTime = time.Now().Add(15 * time.Minute)
	}
	fmt.Printf("⏰ Market ends at: %s (%v remaining)\n",
		endTime.Format("15:04:05"),
		endTime.Sub(time.Now()).Round(time.Second))
	marketMonitor.SetMarket(market.Slug, market.ConditionID, outcomeNames, endTime)

	// Order fill callback
	orderBook.SetFillCallback(func(order *paper.LimitOrder, fillQty, fillPrice float64) {
		trade, err := engine.Buy(order.Outcome, fillPrice, fillQty)
		if err != nil {
			log.Printf("Fill error: %v", err)
			return
		}
		display.PrintTrade(trade)
	})

	fmt.Println("\n🚀 Starting trading loop...\n")
	printStrategyConfig(ladderConfig, riskConfig)

	// Track starting realized PnL to calculate this market's profit
	startingRealizedPnL := engine.GetStats().RealizedPnL
	tradesAtStart := engine.GetStats().TotalTrades

	// Data loop
	tokenPrices := make(map[string]string)
	tokenBids := make(map[string]float64)
	tokenAsks := make(map[string]float64)
	lastOutput := time.Now()
	lastStats := time.Now()
	lastLadderUpdate := time.Now()
	laddersPlaced := false
	marketEnded := false

	for {
		select {
		case <-ctx.Done():
			ladderMgr.CancelAllLadders()
			// Auto-close all positions on shutdown
			positions := engine.GetPositions()
			if len(positions) > 0 {
				fmt.Println("\n🔴 EMERGENCY EXIT: Liquidating all positions...")
				proceeds := engine.LiquidateAll()
				fmt.Printf("💵 Sold all positions for $%.2f\n", proceeds)
			}
			display.PrintStats()
			return nil, ctx.Err()

		default:
			// Check kill switch
			if riskMgr.IsKillSwitchTriggered() {
				ladderMgr.CancelAllLadders()
				// Auto-close all positions on kill switch
				positions := engine.GetPositions()
				if len(positions) > 0 {
					fmt.Println("\n🚨 KILL SWITCH: Liquidating all positions...")
					proceeds := engine.LiquidateAll()
					fmt.Printf("💵 Sold all positions for $%.2f\n", proceeds)
				}
				riskMgr.ExecuteKillSwitch()
				display.PrintStats()
				return nil, fmt.Errorf("kill switch triggered")
			}

			// Check market state
			marketState := marketMonitor.CheckState()

			// Handle market ending
			if marketState == paper.MarketStateEnding && !marketEnded {
				marketEnded = true
				ladderMgr.CancelAllLadders()

				// Wait a bit for resolution (in real trading, we'd poll the API)
				fmt.Println("\n⏳ Market ended. Waiting for resolution...")

				// Simulate resolution based on final prices (paper trading)
				// In real trading, you'd poll the API for actual resolution
				time.Sleep(5 * time.Second)

				// Determine winner based on final prices (simulate)
				winner := simulateResolution(outcomeNames, tokenPrices)
				fmt.Printf("🏆 Market resolved: %s wins!\n", winner)

				// Redeem positions
				payout := engine.Redeem(winner)
				fmt.Printf("💵 Redeemed positions for $%.2f\n", payout)

				// Show final stats for this market
				display.PrintStats()

				// Calculate this market's result
				finalStats := engine.GetStats()
				result := &marketResult{
					realizedPnL: finalStats.RealizedPnL - startingRealizedPnL,
					trades:      finalStats.TotalTrades - tradesAtStart,
				}

				return result, nil
			}

			// Read WebSocket message with timeout
			msg, err := wsMgr.ReadMessage(ctx)
			if err != nil {
				if strings.Contains(err.Error(), "closed") || strings.Contains(err.Error(), "canceled") {
					return nil, err
				}
				continue
			}

			priceChanged := false

			updatePrice := func(assetID, priceStr string, bid, ask float64) {
				outcome := tokenMap[assetID]
				if outcome == "" {
					return
				}
				if tokenPrices[outcome] != priceStr {
					tokenPrices[outcome] = priceStr
					priceChanged = true
				}
				if bid > 0 {
					tokenBids[outcome] = bid
				}
				if ask > 0 {
					tokenAsks[outcome] = ask
				}

				// Use MID PRICE for position valuation (more accurate than just bid)
				midPrice := 0.0
				if bid > 0 && ask > 0 {
					midPrice = (bid + ask) / 2.0
				} else if bid > 0 {
					midPrice = bid
				} else if ask > 0 {
					midPrice = ask
				} else if price, err := strconv.ParseFloat(priceStr, 64); err == nil {
					midPrice = price
				}

				if midPrice > 0 {
					engine.UpdatePrice(outcome, midPrice)
				}
				// Also update bid/ask for realistic taker simulation
				if bid > 0 || ask > 0 {
					engine.UpdateBidAsk(outcome, bid, ask)
				}
			}

			// Parse messages
			if books, err := api.ParseOrderBooks(msg); err == nil && len(books) > 0 && books[0].AssetID != "" {
				for _, b := range books {
					bid, ask := 0.0, 0.0
					if len(b.Bids) > 0 {
						bid, _ = strconv.ParseFloat(b.Bids[0].Price, 64)
					}
					if len(b.Asks) > 0 {
						ask, _ = strconv.ParseFloat(b.Asks[0].Price, 64)
					}
					priceStr := ""
					if len(b.Bids) > 0 {
						priceStr = b.Bids[0].Price
					}
					updatePrice(b.AssetID, priceStr, bid, ask)
				}
			} else if book, err := api.ParseOrderBook(msg); err == nil && book.AssetID != "" {
				bid, ask := 0.0, 0.0
				if len(book.Bids) > 0 {
					bid, _ = strconv.ParseFloat(book.Bids[0].Price, 64)
				}
				if len(book.Asks) > 0 {
					ask, _ = strconv.ParseFloat(book.Asks[0].Price, 64)
				}
				priceStr := ""
				if len(book.Bids) > 0 {
					priceStr = book.Bids[0].Price
				}
				updatePrice(book.AssetID, priceStr, bid, ask)
			} else if update, err := api.ParsePriceUpdate(msg); err == nil && len(update.PriceChanges) > 0 {
				for _, pc := range update.PriceChanges {
					price, _ := strconv.ParseFloat(pc.Price, 64)
					bid, ask := 0.0, 0.0
					if pc.Side == "buy" {
						bid = price
					} else {
						ask = price
					}
					updatePrice(pc.AssetID, pc.Price, bid, ask)
				}
			}

			// Process order fills
			for outcome := range tokenPrices {
				bid := tokenBids[outcome]
				ask := tokenAsks[outcome]
				if bid > 0 || ask > 0 {
					filledOrders := orderBook.ProcessPriceUpdate(outcome, bid, ask)
					for _, order := range filledOrders {
						fmt.Printf("✅ Filled: %s %s %.0f @ $%.3f (limit was $%.3f, saved $%.3f)\n",
							order.Side, order.Outcome, order.Quantity,
							order.FillPrice, order.Price, order.Price-order.FillPrice)
					}
				}
			}

			// Check risk
			action, reason := riskMgr.Evaluate()
			switch action {
			case paper.RiskActionKillSwitch:
				fmt.Printf("🚨 RISK: %s\n", reason)
				ladderMgr.CancelAllLadders()
				// Auto-close all positions
				positions := engine.GetPositions()
				if len(positions) > 0 {
					fmt.Println("🔴 Liquidating all positions...")
					proceeds := engine.LiquidateAll()
					fmt.Printf("💵 Sold all positions for $%.2f\n", proceeds)
				}
				riskMgr.ExecuteKillSwitch()
				display.PrintStats()
				return nil, fmt.Errorf("kill switch: %s", reason)

			case paper.RiskActionRebalance:
				if time.Since(lastLadderUpdate) > 5*time.Second {
					fmt.Printf("⚖️  REBALANCING: %s\n", reason)
					lightSide, adjustment := riskMgr.GetSkewAdjustment()
					if lightSide != "" && adjustment > 0 {
						ladder := ladderMgr.GetOrCreateLadder(lightSide)
						ladder.Config.BasePrice += adjustment
						ladder.PlaceLadder()
					}
					lastLadderUpdate = time.Now()
				}

			case paper.RiskActionReduceSize:
				fmt.Printf("📉 REDUCING SIZE: %s\n", reason)
				for _, ladder := range ladderMgr.Ladders {
					ladder.Config.SharesPerLevel *= 0.5
					ladder.PlaceLadder()
				}
			}

			// Trading logic (only when market is active)
			if priceChanged && len(tokenPrices) == 2 && len(outcomeNames) == 2 && marketState == paper.MarketStateActive {
				p1Str := tokenPrices[outcomeNames[0]]
				p2Str := tokenPrices[outcomeNames[1]]

				if p1Str != "" && p2Str != "" {
					p1, _ := strconv.ParseFloat(p1Str, 64)
					p2, _ := strconv.ParseFloat(p2Str, 64)
					sum := p1 + p2
					margin := (1.0 - sum) * 100

					// Price output
					if time.Since(lastOutput) > 3*time.Second {
						symbol := "📊"
						if margin > 2 {
							symbol = "🔥"
						}
						remaining := marketMonitor.GetTimeToEnd()
						fmt.Printf("%s [%s] Sum: %.4f (%.2f%%) | %s: %.3f, %s: %.3f | ⏱️ %v\n",
							symbol, time.Now().Format("15:04:05"),
							sum, margin,
							outcomeNames[0], p1, outcomeNames[1], p2,
							remaining.Round(time.Second))
						lastOutput = time.Now()
					}

					// Place ladders if opportunity
					if margin >= 2.0 && riskMgr.CanPlaceOrder(ladderConfig.SharesPerLevel*p1) {
						if !laddersPlaced || time.Since(lastLadderUpdate) > 30*time.Second {
							targetSum := 0.96
							fairPrice := targetSum / 2.0
							fmt.Printf("📈 Placing ladders @ $%.3f\n", fairPrice)
							ladderMgr.PlaceAllLadders(outcomeNames, targetSum)
							laddersPlaced = true
							lastLadderUpdate = time.Now()
						}
					}
				}
			}

			// Periodic stats
			if time.Since(lastStats) > time.Duration(StatsInterval)*time.Second {
				display.PrintStats()
				riskMgr.PrintStatus()
				marketMonitor.PrintStatus()
				lastStats = time.Now()
			}
		}
	}
}

// simulateResolution determines winner based on final prices (for paper trading)
func simulateResolution(outcomes []string, prices map[string]string) string {
	if len(outcomes) != 2 {
		return outcomes[0]
	}

	p1, _ := strconv.ParseFloat(prices[outcomes[0]], 64)
	p2, _ := strconv.ParseFloat(prices[outcomes[1]], 64)

	// Higher probability wins (simulate)
	// In reality, this would come from the API
	if p1 > p2 {
		return outcomes[0]
	} else if p2 > p1 {
		return outcomes[1]
	}

	// Random if equal
	if rand.Float64() > 0.5 {
		return outcomes[0]
	}
	return outcomes[1]
}

func printStrategyConfig(ladder paper.LadderConfig, risk paper.RiskConfig) {
	fmt.Println("┌─────────────────────────────────────────────────┐")
	fmt.Println("│           GABAGOOL STRATEGY CONFIG              │")
	fmt.Println("├─────────────────────────────────────────────────┤")
	fmt.Printf("│ Ladder: %d levels × %.0f shares @ $%.2f step     │\n",
		ladder.Levels, ladder.SharesPerLevel, ladder.PriceStep)
	fmt.Printf("│ Max Exposure: $%.0f | Kill DD: %.0f%%              │\n",
		risk.MaxExposure, risk.KillSwitchDrawdown*100)
	fmt.Println("└─────────────────────────────────────────────────┘")
}
