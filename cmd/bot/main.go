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
	UseLiveUI       = true   // Set to false for traditional logging
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

	ladderConfig := paper.LadderConfig{
		Levels:         3,
		SharesPerLevel: 25,
		PriceStep:      0.01,
		BasePrice:      0.48,
	}
	ladderMgr := paper.NewLadderManager(orderBook, ladderConfig)

	// TESTING: Kill switch disabled, risk limits 2x
	riskConfig := paper.RiskConfig{
		MaxExposure:        1000.0, // 2x (was 500)
		MaxUnmatchedRatio:  0.40,   // 2x (was 0.20)
		MaxUnmatchedShares: 150.0,  // 2x (was 75)
		SkewThreshold:      0.30,   // 2x (was 0.15)
		KillSwitchDrawdown: 999.0,  // Disabled (was 1.0)
	}
	riskMgr := paper.NewRiskManager(riskConfig, engine, orderBook, outcomeNames)

	marketMonitor := paper.NewMarketMonitor(engine, orderBook, ladderMgr, riskMgr)

	// Parse market end time
	endTime, err := paper.ParseEndTimeFromSlug(market.Slug)
	if err != nil {
		endTime = time.Now().Add(15 * time.Minute)
	}
	marketMonitor.SetMarket(market.Slug, market.ConditionID, outcomeNames, endTime)

	// Initialize TUI
	tui := paper.NewTUI(engine, orderBook)
	tui.SetMarket(market.Slug, outcomeNames, endTime)

	// Order fill callback
	orderBook.SetFillCallback(func(order *paper.LimitOrder, fillQty, fillPrice float64) {
		_, err := engine.Buy(order.Outcome, fillPrice, fillQty)
		if err != nil {
			tui.LogEvent("❌ Fill error: %v", err)
			return
		}
		saved := order.Price - fillPrice
		tui.LogEvent("✅ FILL %s %.0f @ $%.3f (saved $%.3f)", order.Outcome, fillQty, fillPrice, saved)
	})

	// Start TUI render loop
	if UseLiveUI {
		tui.StartRenderLoop(500 * time.Millisecond)
		defer tui.Stop()
	}

	// Track starting realized PnL to calculate this market's profit
	startingRealizedPnL := engine.GetStats().RealizedPnL
	tradesAtStart := engine.GetStats().TotalTrades

	// Data loop
	tokenPrices := make(map[string]string)
	tokenBids := make(map[string]float64)
	tokenAsks := make(map[string]float64)
	floatPrices := make(map[string]float64)
	lastLadderUpdate := time.Now()
	laddersPlaced := false
	marketEnded := false

	// Liquidity monitoring
	lastGoodLiquidity := time.Now()
	const liquidityTimeout = 45 * time.Second  // Exit if no liquidity for 45s
	const minSpread = 0.001                    // Minimum spread to consider "liquid"
	const maxSpread = 0.15                     // Max spread before considering "illiquid"

	for {
		select {
		case <-ctx.Done():
			tui.Stop()
			ladderMgr.CancelAllLadders()
			positions := engine.GetPositions()
			if len(positions) > 0 {
				tui.LogEvent("🔴 EMERGENCY EXIT: Liquidating...")
				engine.LiquidateAll()
			}
			return nil, ctx.Err()

		default:
			// Check kill switch
			if riskMgr.IsKillSwitchTriggered() {
				tui.SetKillSwitch("Risk limits exceeded")
				tui.Stop()
				ladderMgr.CancelAllLadders()
				positions := engine.GetPositions()
				if len(positions) > 0 {
					engine.LiquidateAll()
				}
				riskMgr.ExecuteKillSwitch()
				return nil, fmt.Errorf("kill switch triggered")
			}

			// Check market state
			marketState := marketMonitor.CheckState()

			// Handle market ending
			if marketState == paper.MarketStateEnding && !marketEnded {
				marketEnded = true
				tui.LogEvent("⏳ Market ended, resolving...")
				ladderMgr.CancelAllLadders()

				time.Sleep(5 * time.Second)

				winner := simulateResolution(outcomeNames, tokenPrices)
				tui.LogEvent("🏆 Winner: %s", winner)

				payout := engine.Redeem(winner)
				tui.LogEvent("💵 Redeemed: $%.2f", payout)

				tui.Stop()

				finalStats := engine.GetStats()
				result := &marketResult{
					realizedPnL: finalStats.RealizedPnL - startingRealizedPnL,
					trades:      finalStats.TotalTrades - tradesAtStart,
				}

				return result, nil
			}

			// Read WebSocket message
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
					floatPrices[outcome] = midPrice
				}
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
				// PriceUpdate contains individual order changes
				// We can use these to UPDATE bid/ask, but only if the price is reasonable
				// compared to what we already have (prevents wild swings from single orders)
				for _, pc := range update.PriceChanges {
					price, _ := strconv.ParseFloat(pc.Price, 64)
					outcome := tokenMap[pc.AssetID]
					if outcome == "" || price <= 0 {
						continue
					}

					tokenPrices[outcome] = pc.Price
					priceChanged = true

					// Update bid/ask only if we have existing data to compare
					// and the new price is within 20% of current mid-price
					currentMid := floatPrices[outcome]
					if currentMid > 0 {
						priceDiff := (price - currentMid) / currentMid
						if priceDiff < 0 {
							priceDiff = -priceDiff
						}
						// Only update if within 20% of current mid (reject outliers)
						if priceDiff <= 0.20 {
							if pc.Side == "buy" {
								tokenBids[outcome] = price
							} else {
								tokenAsks[outcome] = price
							}
							engine.UpdateBidAsk(outcome, tokenBids[outcome], tokenAsks[outcome])
						}
					}
				}
			}

			// Update TUI with prices
			tui.UpdatePrices(floatPrices, tokenBids, tokenAsks)

			// Process order fills
			for outcome := range tokenPrices {
				bid := tokenBids[outcome]
				ask := tokenAsks[outcome]
				if bid > 0 || ask > 0 {
					orderBook.ProcessPriceUpdate(outcome, bid, ask)
				}
			}

			// Check liquidity - if spread is reasonable, we have liquidity
			hasLiquidity := false
			if len(outcomeNames) == 2 {
				for _, outcome := range outcomeNames {
					bid := tokenBids[outcome]
					ask := tokenAsks[outcome]
					if bid > 0 && ask > 0 {
						spread := ask - bid
						if spread >= minSpread && spread <= maxSpread {
							hasLiquidity = true
							break
						}
					}
				}
			}

			if hasLiquidity {
				lastGoodLiquidity = time.Now()
			} else if time.Since(lastGoodLiquidity) > liquidityTimeout {
				// No liquidity for too long - exit to find new market
				tui.LogEvent("💨 Liquidity dried up, finding new market...")
				tui.Stop()
				ladderMgr.CancelAllLadders()

				// Liquidate if we have positions
				positions := engine.GetPositions()
				if len(positions) > 0 {
					tui.LogEvent("📤 Liquidating positions before exit...")
					engine.LiquidateAll()
				}

				finalStats := engine.GetStats()
				result := &marketResult{
					realizedPnL: finalStats.RealizedPnL - startingRealizedPnL,
					trades:      finalStats.TotalTrades - tradesAtStart,
				}
				return result, nil // Return normally to trigger new market search
			}

			// Check risk
			action, reason := riskMgr.Evaluate()
			switch action {
			case paper.RiskActionKillSwitch:
				tui.LogEvent("🚨 KILL: %s", reason)
				tui.SetKillSwitch(reason)
				ladderMgr.CancelAllLadders()
				positions := engine.GetPositions()
				if len(positions) > 0 {
					engine.LiquidateAll()
				}
				riskMgr.ExecuteKillSwitch()
				tui.Stop()
				return nil, fmt.Errorf("kill switch: %s", reason)

			case paper.RiskActionRebalance:
				if time.Since(lastLadderUpdate) > 5*time.Second {
					tui.LogEvent("⚖️ Rebalancing: %s", reason)
					lightSide, adjustment := riskMgr.GetSkewAdjustment()
					if lightSide != "" && adjustment > 0 {
						ladder := ladderMgr.GetOrCreateLadder(lightSide)
						ladder.Config.BasePrice += adjustment
						ladder.PlaceLadder()
					}
					lastLadderUpdate = time.Now()
				}

			case paper.RiskActionReduceSize:
				tui.LogEvent("📉 Reducing size: %s", reason)
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

					// Place ladders if opportunity
					if margin >= 2.0 && riskMgr.CanPlaceOrder(ladderConfig.SharesPerLevel*p1) {
						if !laddersPlaced || time.Since(lastLadderUpdate) > 30*time.Second {
							targetSum := 0.96
							fairPrice := targetSum / 2.0

							// Check inventory balance - pause overweight side
							positions := engine.GetPositions()
							pos1 := positions[outcomeNames[0]].Quantity
							pos2 := positions[outcomeNames[1]].Quantity
							imbalance := pos1 - pos2
							const maxImbalance = 50.0 // Pause when 50+ shares imbalanced

							if imbalance > maxImbalance {
								// Too much of outcome[0], only place for outcome[1]
								tui.LogEvent("⚖️ Pausing %s (%.0f ahead), placing %s only", outcomeNames[0], imbalance, outcomeNames[1])
								ladder := ladderMgr.GetOrCreateLadder(outcomeNames[1])
								ladder.UpdateLadder(fairPrice)
							} else if imbalance < -maxImbalance {
								// Too much of outcome[1], only place for outcome[0]
								tui.LogEvent("⚖️ Pausing %s (%.0f ahead), placing %s only", outcomeNames[1], -imbalance, outcomeNames[0])
								ladder := ladderMgr.GetOrCreateLadder(outcomeNames[0])
								ladder.UpdateLadder(fairPrice)
							} else {
								// Balanced - place both
								tui.LogEvent("📈 Placing ladders @ $%.3f", fairPrice)
								ladderMgr.PlaceAllLadders(outcomeNames, targetSum)
							}

							laddersPlaced = true
							lastLadderUpdate = time.Now()
						}
					}
				}
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

	if p1 > p2 {
		return outcomes[0]
	} else if p2 > p1 {
		return outcomes[1]
	}

	if rand.Float64() > 0.5 {
		return outcomes[0]
	}
	return outcomes[1]
}
