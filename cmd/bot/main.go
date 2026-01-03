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

	// Track last traded market to avoid re-entering
	var lastMarketSlug string

	// Main market rotation loop
	for {
		select {
		case <-ctx.Done():
			fmt.Println("\n🛑 Shutting down...")
			return nil
		default:
		}

		// Find next market (skip the one we just traded)
		market, err := findNextMarket(restClient, cfg.MarketSlug, lastMarketSlug)
		if err != nil {
			fmt.Printf("⚠️  No market found: %v. Retrying in 30s...\n", err)
			time.Sleep(30 * time.Second)
			continue
		}

		// Trade this market
		lastMarketSlug = market.Slug
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

func findNextMarket(restClient *api.RestClient, preferredSlug string, skipSlug string) (*api.Market, error) {
	var market *api.Market
	var err error

	// Try preferred slug first (unless it's the one we're skipping)
	if preferredSlug != "" && preferredSlug != skipSlug {
		market, err = restClient.GetMarket(preferredSlug)
		if err == nil && market.Active && !market.Closed {
			return market, nil
		}
	}

	// Scan for 15m markets
	fmt.Println("🔎 Scanning for active 15m markets...")
	markets, err := restClient.Get15mMarkets(nil)
	if err == nil && len(markets) > 0 {
		for _, m := range markets {
			if m.Slug != skipSlug && m.Active && !m.Closed {
				fmt.Printf("✅ Found: %s\n", m.Slug)
				return &m, nil
			}
		}
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
			if slug == skipSlug {
				continue // Skip the market we just traded
			}
			if (contains(slug, "bitcoin") || contains(slug, "btc") || contains(slug, "eth")) && contains(slug, "price") {
				market, err = restClient.GetMarket(slug)
				if err == nil && market.Active && !market.Closed {
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
	const liquidityTimeout = 45 * time.Second // Exit if no liquidity for 45s

	// REST API polling for accurate prices
	lastRESTFetch := time.Time{}
	const restFetchInterval = 2 * time.Second

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
				tui.Stop() // Stop TUI first so we can print details

				fmt.Println("\n" + strings.Repeat("═", 50))
				fmt.Println("⏳ MARKET ENDING - RESOLUTION")
				fmt.Println(strings.Repeat("═", 50))

				// Show positions before resolution
				positions := engine.GetPositions()
				if len(positions) > 0 {
					fmt.Println("\n📦 Positions to resolve:")
					for outcome, pos := range positions {
						price := floatPrices[outcome]
						fmt.Printf("   • %s: %.0f shares @ $%.3f avg (current: $%.3f)\n",
							outcome, pos.Quantity, pos.AvgPrice, price)
					}
				} else {
					fmt.Println("\n📦 No positions to resolve")
				}

				// Open orders just expire - no need to cancel, they're void now
				openOrders := orderBook.GetOpenOrders()
				if len(openOrders) > 0 {
					fmt.Printf("📝 %d unfilled orders expired (void)\n", len(openOrders))
				}

				fmt.Println("\n⏳ Waiting 5s for final price settlement...")
				time.Sleep(5 * time.Second)

				winner := simulateResolution(outcomeNames, tokenPrices)
				fmt.Printf("\n🏆 WINNER: %s\n", winner)

				payout := engine.Redeem(winner)
				fmt.Printf("💵 Total Payout: $%.2f\n", payout)

				finalStats := engine.GetStats()
				marketPnL := finalStats.RealizedPnL - startingRealizedPnL
				fmt.Printf("\n📊 Market Summary:\n")
				fmt.Printf("   • Trades: %d\n", finalStats.TotalTrades-tradesAtStart)
				fmt.Printf("   • Market PnL: $%.2f\n", marketPnL)
				fmt.Printf("   • Total Balance: $%.2f\n", finalStats.CurrentBalance)
				fmt.Println(strings.Repeat("═", 50))

				result := &marketResult{
					realizedPnL: marketPnL,
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

			// Poll REST API for accurate order book data (every 2 seconds)
			if time.Since(lastRESTFetch) >= restFetchInterval {
				for _, token := range market.Tokens {
					book, err := restClient.GetOrderBook(token.TokenID)
					if err != nil {
						tui.LogEvent("❌ REST error %s: %v", token.Outcome, err)
						continue
					}
					outcome := token.Outcome

					// Find best bid (highest) and best ask (lowest)
					bestBid := 0.0
					bestAsk := 1.0
					for _, b := range book.Bids {
						p, _ := strconv.ParseFloat(b.Price, 64)
						if p > bestBid {
							bestBid = p
						}
					}
					for _, a := range book.Asks {
						p, _ := strconv.ParseFloat(a.Price, 64)
						if p < bestAsk && p > 0 {
							bestAsk = p
						}
					}

					// Debug: log REST API response
					tui.LogEvent("📡 REST %s: bestBid=$%.3f bestAsk=$%.3f",
						outcome, bestBid, bestAsk)

					if bestBid > 0 {
						tokenBids[outcome] = bestBid
					}
					if bestAsk < 1.0 {
						tokenAsks[outcome] = bestAsk
					}
					if bestBid > 0 && bestAsk < 1.0 {
						midPrice := (bestBid + bestAsk) / 2.0
						floatPrices[outcome] = midPrice
						engine.UpdatePrice(outcome, midPrice)
						engine.UpdateBidAsk(outcome, bestBid, bestAsk)
					}
				}
				lastRESTFetch = time.Now()
			}

			priceChanged := false
			msgCount := 0 // Debug counter

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

			// Parse messages - log first few to debug
			if books, err := api.ParseOrderBooks(msg); err == nil && len(books) > 0 && books[0].AssetID != "" {
				for _, b := range books {
					bid, ask := 0.0, 0.0
					if len(b.Bids) > 0 {
						bid, _ = strconv.ParseFloat(b.Bids[0].Price, 64)
					}
					if len(b.Asks) > 0 {
						ask, _ = strconv.ParseFloat(b.Asks[0].Price, 64)
					}
					// Debug: log first order book update
					if msgCount < 2 {
						outcome := tokenMap[b.AssetID]
						tui.LogEvent("📥 OrderBook %s: bid=$%.3f ask=$%.3f", outcome, bid, ask)
						msgCount++
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
				// Debug: log first order book update
				if msgCount < 2 {
					outcome := tokenMap[book.AssetID]
					tui.LogEvent("📥 OrderBook %s: bid=$%.3f ask=$%.3f", outcome, bid, ask)
					msgCount++
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
			} else {
				// Debug: log unparsed messages (first few only)
				if msgCount < 3 {
					// Try to get a preview of the message
					preview := string(msg)
					if len(preview) > 100 {
						preview = preview[:100] + "..."
					}
					tui.LogEvent("📭 Unknown msg: %s", preview)
					msgCount++
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

			// Check liquidity - if we have reasonable bid/ask prices, we have liquidity
			hasLiquidity := false
			if len(outcomeNames) == 2 {
				for _, outcome := range outcomeNames {
					bid := tokenBids[outcome]
					ask := tokenAsks[outcome]
					// Consider liquid if we have any valid bid or ask in reasonable range
					if (bid >= 0.15 && bid <= 0.85) || (ask >= 0.15 && ask <= 0.85) {
						hasLiquidity = true
						break
					}
				}
			}

			if hasLiquidity {
				lastGoodLiquidity = time.Now()
			} else if time.Since(lastGoodLiquidity) > liquidityTimeout {
				// No liquidity for too long - find another market but keep positions
				tui.Stop()

				positions := engine.GetPositions()
				if len(positions) > 0 {
					fmt.Println("\n💨 Liquidity dried up - keeping positions for expiration")
					fmt.Println("📦 Positions held:")
					for outcome, pos := range positions {
						fmt.Printf("   • %s: %.0f shares @ $%.3f avg\n", outcome, pos.Quantity, pos.AvgPrice)
					}
					fmt.Println("⏳ These will resolve when market expires naturally")
					fmt.Println("🔄 Finding another market to trade...\n")
				} else {
					fmt.Println("\n💨 Liquidity dried up - finding another market...\n")
				}

				// Don't liquidate - just cancel open orders and move on
				ladderMgr.CancelAllLadders()

				finalStats := engine.GetStats()
				result := &marketResult{
					realizedPnL: finalStats.RealizedPnL - startingRealizedPnL,
					trades:      finalStats.TotalTrades - tradesAtStart,
				}
				return result, nil // Return to find new market, positions remain
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
				// Get actual market asks (what we'd pay to buy)
				ask1 := tokenAsks[outcomeNames[0]]
				ask2 := tokenAsks[outcomeNames[1]]

				// Log market state periodically
				if !laddersPlaced && time.Since(lastLadderUpdate) > 5*time.Second {
					if ask1 == 0 || ask2 == 0 {
						tui.LogEvent("⏳ Waiting for asks: %s=$%.2f, %s=$%.2f", outcomeNames[0], ask1, outcomeNames[1], ask2)
					} else if ask1 < 0.10 || ask1 > 0.90 || ask2 < 0.10 || ask2 > 0.90 {
						tui.LogEvent("⚠️ Asks out of range: %s=$%.2f, %s=$%.2f", outcomeNames[0], ask1, outcomeNames[1], ask2)
					} else {
						sum := ask1 + ask2
						margin := (1.0 - sum) * 100
						if margin < 2.0 {
							tui.LogEvent("📊 No arb: %s=$%.2f + %s=$%.2f = $%.2f (margin %.1f%%)", outcomeNames[0], ask1, outcomeNames[1], ask2, sum, margin)
						}
					}
					lastLadderUpdate = time.Now()
				}

				// Only trade if we have real ask prices in sane range
				if ask1 >= 0.10 && ask1 <= 0.90 && ask2 >= 0.10 && ask2 <= 0.90 {
					sum := ask1 + ask2
					margin := (1.0 - sum) * 100

					// Place ladders if opportunity exists (sum < 1.0 means arbitrage)
					if margin >= 2.0 && riskMgr.CanPlaceOrder(ladderConfig.SharesPerLevel*ask1) {
						if !laddersPlaced || time.Since(lastLadderUpdate) > 30*time.Second {
							// Place bids slightly below actual asks
							bidOffset := 0.02 // Bid 2 cents below ask

							// Check inventory balance - pause overweight side
							positions := engine.GetPositions()
							pos1 := positions[outcomeNames[0]].Quantity
							pos2 := positions[outcomeNames[1]].Quantity
							imbalance := pos1 - pos2
							const maxImbalance = 50.0

							if imbalance > maxImbalance {
								// Too much of outcome[0], only place for outcome[1]
								tui.LogEvent("⚖️ Pausing %s (%.0f ahead), placing %s @ $%.3f", outcomeNames[0], imbalance, outcomeNames[1], ask2-bidOffset)
								ladder := ladderMgr.GetOrCreateLadder(outcomeNames[1])
								ladder.Config.BasePrice = ask2 - bidOffset
								ladder.PlaceLadder()
							} else if imbalance < -maxImbalance {
								// Too much of outcome[1], only place for outcome[0]
								tui.LogEvent("⚖️ Pausing %s (%.0f ahead), placing %s @ $%.3f", outcomeNames[1], -imbalance, outcomeNames[0], ask1-bidOffset)
								ladder := ladderMgr.GetOrCreateLadder(outcomeNames[0])
								ladder.Config.BasePrice = ask1 - bidOffset
								ladder.PlaceLadder()
							} else {
								// Balanced - place both at real prices
								tui.LogEvent("📈 Placing ladders: %s@$%.3f, %s@$%.3f (margin %.1f%%)",
									outcomeNames[0], ask1-bidOffset, outcomeNames[1], ask2-bidOffset, margin)

								ladder1 := ladderMgr.GetOrCreateLadder(outcomeNames[0])
								ladder1.Config.BasePrice = ask1 - bidOffset
								ladder1.PlaceLadder()

								ladder2 := ladderMgr.GetOrCreateLadder(outcomeNames[1])
								ladder2.Config.BasePrice = ask2 - bidOffset
								ladder2.PlaceLadder()
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
