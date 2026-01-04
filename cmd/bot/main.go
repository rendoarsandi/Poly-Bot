package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
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

// MarketTrader holds state for trading a single market
type MarketTrader struct {
	ID          string // "ETH" or "SOL"
	Market      *api.Market
	Engine      *paper.Engine
	OrderBook   *paper.OrderBook
	LadderMgr   *paper.LadderManager
	RiskMgr     *paper.RiskManager
	Monitor     *paper.MarketMonitor
	TokenMap    map[string]string // tokenID -> outcome
	Outcomes    []string
	EndTime     time.Time
	RestClient  *api.RestClient
	WSMgr       *api.WSManager
	TUI         *paper.TUI // Shared TUI

	// Price tracking
	TokenBids     map[string]float64
	TokenAsks     map[string]float64
	TokenFullBids map[string][]paper.MarketLevel
	TokenFullAsks map[string][]paper.MarketLevel
	FloatPrices   map[string]float64

	// State
	LaddersPlaced bool
	MarketEnded   bool
	mu            sync.Mutex
}

type marketResult struct {
	realizedPnL float64
	trades      int
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("Critical error: %v", err)
	}
}

// restoreTerminal ensures terminal is in a usable state
func restoreTerminal() {
	restoreEcho := exec.Command("stty", "sane") // sane resets to safe defaults
	restoreEcho.Stdin = os.Stdin
	_ = restoreEcho.Run()
	fmt.Print("\033[?25h") // Show cursor
	fmt.Print("\033[?1049l") // Exit alternate screen buffer if active
	fmt.Println()
}

func run() error {
	// Setup signal handling with immediate terminal restore
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Global panic recovery - restore terminal on any panic
	defer func() {
		if r := recover(); r != nil {
			restoreTerminal()
			fmt.Printf("\n🚨 PANIC RECOVERED: %v\n", r)
		}
	}()

	// Disable terminal echo to prevent arrow keys from appearing
	// This is done via stty which works on most Unix systems including Termux
	disableEcho := exec.Command("stty", "-echo", "-icanon")
	disableEcho.Stdin = os.Stdin
	_ = disableEcho.Run() // Ignore errors if stty not available

	// Ensure terminal is restored on any exit
	defer restoreTerminal()

	// Clear screen at startup
	fmt.Print("\033[H\033[2J")
	fmt.Println("🎰 POLYARB-15M Starting (Multi-Asset: BTC, ETH, SOL, XRP)...")

	// Initialize persistent components (survive market rotation)
	engine := paper.NewEngine(StartingBalance)

	// Load config
	_, err := core.LoadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	restClient := api.NewRestClient("")

	// Create shared order book and TUI (persistent across market rotations)
	orderBook := paper.NewOrderBook()
	tui := paper.NewTUI(engine, orderBook)

	// Start TUI render loop
	if UseLiveUI {
		tui.StartRenderLoop(250 * time.Millisecond) // Fast refresh for live updates
		defer tui.Stop()
	}

	// Main loop - continuously trade markets and rotate to next when expired
	for {
		select {
		case <-ctx.Done():
			tui.Stop()
			fmt.Println("\n👋 Shutting down - liquidating positions...")

			// Liquidate all positions before exit
			positions := engine.GetPositions()
			if len(positions) > 0 {
				fmt.Println("💰 Cashing out positions at current market prices...")
				proceeds := engine.LiquidateAll()
				fmt.Printf("💵 Liquidation proceeds: $%.2f\n", proceeds)
			}

			stats := engine.GetStats()
			fmt.Printf("📊 Final Stats: Balance $%.2f | Realized PnL $%.2f | Trades %d\n",
				stats.CurrentBalance, stats.RealizedPnL, stats.TotalTrades)
			return nil
		default:
		}

		// Find all available markets (BTC, ETH, SOL, XRP)
		markets := findMarkets(ctx, restClient, tui)
		if len(markets) == 0 {
			tui.LogEvent("⏳ No markets found, retrying in 2s...")
			select {
			case <-ctx.Done():
				tui.Stop()
				fmt.Println("\n👋 Shutting down...")
				stats := engine.GetStats()
				fmt.Printf("📊 Final Stats: Balance $%.2f | Realized PnL $%.2f | Trades %d\n",
					stats.CurrentBalance, stats.RealizedPnL, stats.TotalTrades)
				return nil
			case <-time.After(2 * time.Second):
			}
			continue
		}

		// Track starting equity for compounding calculation
		startingEquity := engine.GetEquity()
		compoundMultiplier := engine.GetCompoundMultiplier()
		tui.LogEvent("💹 Round starting | Equity: $%.2f | Multiplier: %.2fx", startingEquity, compoundMultiplier)

		// Create traders for all found markets
		var wg sync.WaitGroup
		results := make(chan *marketResult, len(markets))
		errors := make(chan error, len(markets))

		tradersStarted := 0
		for assetID, market := range markets {
			// Parse end time and outcomes
			endTime, err := paper.ParseEndTimeFromSlug(market.Slug)
			if err != nil {
				endTime = time.Now().Add(15 * time.Minute)
			}
			outcomes := getOutcomes(market)
			tui.AddMarket(assetID, market.Slug, outcomes, endTime)
			tui.LogEvent("🚀 Trading %s: %s", assetID, market.Slug)

			trader := createTrader(assetID, market, engine, orderBook, restClient, tui, outcomes, endTime)
			wg.Add(1)
			tradersStarted++
			go func(id string, t *MarketTrader) {
				defer wg.Done()
				// Panic recovery for trader goroutine
				defer func() {
					if r := recover(); r != nil {
						tui.LogEvent("[%s] 🚨 PANIC: %v - restarting...", id, r)
						errors <- fmt.Errorf("%s: panic: %v", id, r)
					}
				}()
				result, err := runTrader(ctx, t)
				if err != nil {
					errors <- fmt.Errorf("%s: %w", id, err)
					return
				}
				results <- result
			}(assetID, trader)
		}

		tui.LogEvent("📈 Started %d concurrent market traders", tradersStarted)

		// Wait for all traders to complete with a context-aware mechanism
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		// Wait for either all traders to finish OR context cancellation
		select {
		case <-done:
			// All traders finished normally
			tui.LogEvent("✅ All %d traders completed", tradersStarted)
		case <-ctx.Done():
			// Context cancelled - give traders 2 seconds to clean up
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				tui.LogEvent("⚠️ Force stopping traders...")
			}
		}

		close(results)
		close(errors)

		// Collect results
		totalPnL := 0.0
		totalTrades := 0
		for result := range results {
			if result != nil {
				totalPnL += result.realizedPnL
				totalTrades += result.trades
			}
		}

		// Log market rotation with detailed stats
		stats := engine.GetStats()
		tui.LogEvent("📊 Round PnL: $%.2f | Total Balance: $%.2f | Rotating...", totalPnL, stats.CurrentBalance)

		// Update compounding multiplier based on round performance
		engine.UpdateCompoundMultiplier(totalPnL, startingEquity)
		newMultiplier := engine.GetCompoundMultiplier()
		if totalPnL > 0 {
			tui.LogEvent("📈 PROFIT! Multiplier: %.2fx → %.2fx (compounding)", compoundMultiplier, newMultiplier)
		} else if totalPnL < 0 {
			tui.LogEvent("📉 Loss. Multiplier: %.2fx → %.2fx", compoundMultiplier, newMultiplier)
		}

		// Check for context cancellation
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		// Brief pause before finding next markets (context-aware)
		tui.LogEvent("🔄 Market round complete, searching for new markets...")
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(1 * time.Second):
		}

		// Clear old market data from TUI for fresh display
		tui.ClearMarkets()
		orderBook.CancelAllOrders()
	}
}

// findMarkets searches for BTC, ETH, SOL, XRP 15m markets
func findMarkets(ctx context.Context, restClient *api.RestClient, tui *paper.TUI) map[string]*api.Market {
	found := make(map[string]*api.Market)
	assets := []string{"btc", "eth", "sol", "xrp"}

	// Keep trying for up to 3 minutes (markets might take time to appear after previous expires)
	maxAttempts := 90
	lastLogTime := time.Now()

	for attempts := 0; attempts < maxAttempts; attempts++ {
		select {
		case <-ctx.Done():
			return found
		default:
		}

		markets, err := restClient.Get15mMarkets(ctx, nil)
		if err != nil {
			if attempts == 0 {
				tui.LogEvent("⚠️ Market fetch error: %v, retrying...", err)
			}
			select {
			case <-ctx.Done():
				return found
			case <-time.After(2 * time.Second):
			}
			continue
		}

		for _, m := range markets {
			// Parse end time and skip if market already expired
			endTime, err := paper.ParseEndTimeFromSlug(m.Slug)
			if err == nil && time.Now().After(endTime) {
				// Market already expired, skip it
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

		// Return if we found at least one market
		if len(found) > 0 {
			return found
		}

		// Log progress every 10 seconds
		if time.Since(lastLogTime) >= 10*time.Second {
			tui.LogEvent("🔍 Waiting for new markets... (%ds)", attempts*2)
			lastLogTime = time.Now()
		}

		select {
		case <-ctx.Done():
			return found
		case <-time.After(2 * time.Second):
		}
	}

	tui.LogEvent("⚠️ No 15m markets found after %d attempts", maxAttempts)
	return found
}

func getOutcomes(market *api.Market) []string {
	outcomes := make([]string, 0, len(market.Tokens))
	for _, token := range market.Tokens {
		outcomes = append(outcomes, token.Outcome)
	}
	return outcomes
}

func createTrader(id string, market *api.Market, engine *paper.Engine, orderBook *paper.OrderBook, restClient *api.RestClient, tui *paper.TUI, outcomes []string, endTime time.Time) *MarketTrader {
	tokenMap := make(map[string]string)
	for _, token := range market.Tokens {
		tokenMap[token.TokenID] = token.Outcome
	}

	ladderConfig := paper.LadderConfig{
		Levels:         3,
		SharesPerLevel: 25,
		PriceStep:      0.01,
		BasePrice:      0.0,
	}
	ladderMgr := paper.NewLadderManager(orderBook, ladderConfig)

	riskConfig := paper.RiskConfig{
		MaxExposure:        1000.0,
		MaxUnmatchedRatio:  0.40,
		MaxUnmatchedShares: 150.0,
		SkewThreshold:      0.30,
		KillSwitchDrawdown: 999.0,
	}
	riskMgr := paper.NewRiskManager(riskConfig, engine, orderBook, outcomes)

	monitor := paper.NewMarketMonitor(engine, orderBook, ladderMgr, riskMgr)
	monitor.SetMarket(market.Slug, market.ConditionID, outcomes, endTime)

	return &MarketTrader{
		ID:            id,
		Market:        market,
		Engine:        engine,
		OrderBook:     orderBook,
		LadderMgr:     ladderMgr,
		RiskMgr:       riskMgr,
		Monitor:       monitor,
		TokenMap:      tokenMap,
		Outcomes:      outcomes,
		EndTime:       endTime,
		RestClient:    restClient,
		TUI:           tui,
		TokenBids:     make(map[string]float64),
		TokenAsks:     make(map[string]float64),
		TokenFullBids: make(map[string][]paper.MarketLevel),
		TokenFullAsks: make(map[string][]paper.MarketLevel),
		FloatPrices:   make(map[string]float64),
	}
}

func runTrader(ctx context.Context, t *MarketTrader) (*marketResult, error) {
	// Setup WebSocket
	wsMgr := api.NewWSManager("")
	if err := wsMgr.Connect(ctx); err != nil {
		return nil, fmt.Errorf("websocket connect failed: %w", err)
	}
	defer wsMgr.Close()
	t.WSMgr = wsMgr

	// Safety timeout: based on market end time + 1 minute buffer for resolution
	// This ensures we exit shortly after the market should have resolved
	safetyBuffer := 1 * time.Minute
	traderDeadline := t.EndTime.Add(safetyBuffer)
	timeUntilDeadline := time.Until(traderDeadline)
	t.TUI.LogEvent("[%s] ⏰ Timeout: %v (expires + 1m)", t.ID, timeUntilDeadline.Round(time.Second))

	// Subscribe to Order Books
	var assetIDs []string
	for _, token := range t.Market.Tokens {
		assetIDs = append(assetIDs, token.TokenID)
	}

	sub := map[string]interface{}{
		"type":       "market",
		"assets_ids": assetIDs,
	}
	if err := wsMgr.Subscribe(ctx, sub); err != nil {
		return nil, fmt.Errorf("subscribe failed: %w", err)
	}

	// Start WebSocket streaming in background - REAL-TIME updates via channel
	wsMsgChan := wsMgr.StartStreaming(ctx)
	t.TUI.LogEvent("[%s] 📡 WebSocket streaming started (real-time)", t.ID)

	// Order fill callback
	t.OrderBook.SetFillCallback(func(order *paper.LimitOrder, fillQty, fillPrice float64) {
		_, err := t.Engine.Buy(order.Outcome, fillPrice, fillQty)
		if err != nil {
			t.TUI.LogEvent("[%s] ❌ Fill error: %v", t.ID, err)
			return
		}
		saved := order.Price - fillPrice
		t.TUI.LogEvent("[%s] ✅ FILL %s %.0f @ $%.3f (saved $%.3f)", t.ID, order.Outcome, fillQty, fillPrice, saved)
	})

	// Track starting realized PnL
	startingRealizedPnL := t.Engine.GetStats().RealizedPnL
	tradesAtStart := t.Engine.GetStats().TotalTrades

	tokenPrices := make(map[string]string)
	lastLadderUpdate := time.Now()
	lastGoodLiquidity := time.Now()
	lastRESTFetch := time.Time{}
	lastGammaFetch := time.Time{}
	lastReconnectCount := int32(0) // Track reconnections

	const liquidityTimeout = 45 * time.Second
	const restFetchInterval = 100 * time.Millisecond // Fast REST polling (100ms = 10/sec)
	const gammaFetchInterval = 100 * time.Millisecond // Fast CLOB polling (100ms)

	ladderConfig := paper.LadderConfig{
		Levels:         3,
		SharesPerLevel: 25,
		PriceStep:      0.01,
		BasePrice:      0.0,
	}

	for {
		select {
		case <-ctx.Done():
			t.LadderMgr.CancelAllLadders()
			positions := t.Engine.GetPositions()
			if len(positions) > 0 {
				t.TUI.LogEvent("[%s] 🔴 EMERGENCY EXIT: Liquidating...", t.ID)
				t.Engine.LiquidateAll()
			}
			return nil, ctx.Err()

		default:
			// Check safety timeout - force exit if trader runs too long
			if time.Now().After(traderDeadline) {
				t.TUI.LogEvent("[%s] ⚠️ SAFETY TIMEOUT - Forcing market exit", t.ID)
				t.LadderMgr.CancelAllLadders()

				// Simulate resolution based on last known prices
				winner := simulateResolution(t.Outcomes, tokenPrices)
				if winner != "" {
					t.TUI.LogEvent("[%s] 🏆 Timeout resolution: %s", t.ID, winner)
					t.Engine.RedeemWithDetails(winner)
				}

				finalStats := t.Engine.GetStats()
				return &marketResult{
					realizedPnL: finalStats.RealizedPnL - startingRealizedPnL,
					trades:      finalStats.TotalTrades - tradesAtStart,
				}, nil
			}

			// Check kill switch
			if t.RiskMgr.IsKillSwitchTriggered() {
				t.TUI.SetKillSwitch("Risk limits exceeded")
				t.LadderMgr.CancelAllLadders()
				positions := t.Engine.GetPositions()
				if len(positions) > 0 {
					t.Engine.LiquidateAll()
				}
				t.RiskMgr.ExecuteKillSwitch()
				return nil, fmt.Errorf("kill switch triggered")
			}

			// Check market state
			marketState := t.Monitor.CheckState()

			// Handle market ending
			timeToEnd := time.Until(t.EndTime)
			isExpired := timeToEnd <= 0

			if isExpired && !t.MarketEnded {
				t.MarketEnded = true
				t.TUI.LogEvent("[%s] ⏳ MARKET EXPIRED - AWAITING RESOLUTION", t.ID)

				// Context-aware sleep
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(2 * time.Second):
				}

				winner := simulateResolution(t.Outcomes, tokenPrices)
				t.TUI.LogEvent("[%s] 🏆 WINNER: %s", t.ID, winner)

				// Use detailed redemption
				result := t.Engine.RedeemWithDetails(winner)
				if result.WinningShares > 0 || result.LosingShares > 0 {
					t.TUI.LogEvent("[%s] 💰 WIN: %.0f shares → $%.2f (profit: $%.2f)",
						t.ID, result.WinningShares, result.WinningPayout, result.WinningPnL)
					if result.LosingShares > 0 {
						t.TUI.LogEvent("[%s] 💀 LOSS: %.0f shares → $0 (lost: $%.2f)",
							t.ID, result.LosingShares, result.LosingCost)
					}
					pnlSign := "+"
					pnlColor := "🟢"
					if result.TotalPnL < 0 {
						pnlSign = ""
						pnlColor = "🔴"
					}
					t.TUI.LogEvent("[%s] %s NET PnL: %s$%.2f", t.ID, pnlColor, pnlSign, result.TotalPnL)
				} else {
					t.TUI.LogEvent("[%s] 📭 No positions to redeem", t.ID)
				}

				finalStats := t.Engine.GetStats()
				marketPnL := finalStats.RealizedPnL - startingRealizedPnL

				return &marketResult{
					realizedPnL: marketPnL,
					trades:      finalStats.TotalTrades - tradesAtStart,
				}, nil
			}

			if marketState == paper.MarketStateEnding && !t.MarketEnded {
				if !t.LaddersPlaced {
					t.TUI.LogEvent("[%s] ⏳ Market ending in %v...", t.ID, timeToEnd.Round(time.Second))
					t.LaddersPlaced = true
				}
			}

			// ============ PRICE UPDATES - DO THESE FIRST ============
			// Poll REST API for order book data BEFORE WebSocket
			// This ensures prices update even if WS is slow
			restInterval := restFetchInterval
			if !wsMgr.IsConnected() {
				restInterval = 100 * time.Millisecond // Super fast when WS is down
			}
			if time.Since(lastRESTFetch) >= restInterval {
				for _, token := range t.Market.Tokens {
					book, err := t.RestClient.GetOrderBook(ctx, token.TokenID)
					if err != nil {
						continue
					}
					outcome := token.Outcome
					t.TokenFullBids[outcome] = toMarketLevels(book.Bids)
					t.TokenFullAsks[outcome] = toMarketLevels(book.Asks)

					// Extract best bid/ask from order book
					bestBid := 0.0
					bestAsk := 0.0
					for _, level := range book.Bids {
						p, _ := strconv.ParseFloat(level.Price, 64)
						if p > bestBid {
							bestBid = p
						}
					}
					for _, level := range book.Asks {
						p, _ := strconv.ParseFloat(level.Price, 64)
						if p > 0 && (bestAsk == 0 || p < bestAsk) {
							bestAsk = p
						}
					}

					// Update prices from REST data
					if bestBid > 0 {
						t.TokenBids[outcome] = bestBid
					}
					if bestAsk > 0 && bestAsk < 1.0 {
						t.TokenAsks[outcome] = bestAsk
					}
					if bestBid > 0 && bestAsk > 0 && bestAsk < 1.0 {
						mid := (bestBid + bestAsk) / 2
						t.FloatPrices[outcome] = mid
						tokenPrices[outcome] = fmt.Sprintf("%.3f", mid)
						t.Engine.UpdatePrice(outcome, mid)
						t.Engine.UpdateBidAsk(outcome, bestBid, bestAsk)
						// Also update per-market bid/ask for P&L calculation
						t.Engine.UpdateMarketBidAsk(t.ID, outcome, bestBid, bestAsk)
					}
				}
				// Update TUI after REST fetch
				t.TUI.UpdateMarketPrices(t.ID, t.TokenBids, t.TokenAsks)
				lastRESTFetch = time.Now()
			}

			// Fetch CLOB prices (alternative source)
			if time.Since(lastGammaFetch) >= gammaFetchInterval {
				reverseTokenMap := make(map[string]string)
				for tokenID, outcome := range t.TokenMap {
					reverseTokenMap[tokenID] = outcome
				}

				realPricesBA, err := t.RestClient.GetCLOBBidAsk(ctx, reverseTokenMap)
				if err == nil && len(realPricesBA) > 0 {
					for outcome, pa := range realPricesBA {
						if pa.Bid > 0 || pa.Ask > 0 {
							t.TokenBids[outcome] = pa.Bid
							t.TokenAsks[outcome] = pa.Ask
							t.FloatPrices[outcome] = (pa.Bid + pa.Ask) / 2
							tokenPrices[outcome] = fmt.Sprintf("%.3f", t.FloatPrices[outcome])
							t.Engine.UpdatePrice(outcome, t.FloatPrices[outcome])
							t.Engine.UpdateBidAsk(outcome, pa.Bid, pa.Ask)
							// Also update per-market bid/ask for P&L calculation
							t.Engine.UpdateMarketBidAsk(t.ID, outcome, pa.Bid, pa.Ask)
						}
					}
					// Update TUI after CLOB fetch
					t.TUI.UpdateMarketPrices(t.ID, t.TokenBids, t.TokenAsks)
				}
				lastGammaFetch = time.Now()
			}

			// Check for WebSocket reconnection and log it
			_, _, reconnects, _ := wsMgr.GetStats()
			if reconnects > lastReconnectCount {
				t.TUI.LogEvent("[%s] 🔄 WebSocket reconnected (attempt #%d)", t.ID, reconnects)
				lastReconnectCount = reconnects
			}

			// Process ALL available WebSocket messages (real-time, non-blocking)
			// This drains the channel to get the latest data
			wsChannelClosed := false
			for {
				select {
				case msg, ok := <-wsMsgChan:
					if !ok {
						// Channel closed, WebSocket done - rely on REST API
						wsChannelClosed = true
						goto doneProcessingWS
					}
					// Parse and process WebSocket message immediately
					if books, err := api.ParseOrderBooks(msg); err == nil && len(books) > 0 && books[0].AssetID != "" {
						for _, b := range books {
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
								ask = 0.0
							}
							outcome := t.TokenMap[b.AssetID]
							if outcome != "" {
								t.TokenBids[outcome] = bid
								t.TokenAsks[outcome] = ask
								if bid > 0 && ask > 0 && ask < 1.0 {
									mid := (bid + ask) / 2
									t.FloatPrices[outcome] = mid
									tokenPrices[outcome] = fmt.Sprintf("%.3f", mid)
									t.Engine.UpdatePrice(outcome, mid)
									t.Engine.UpdateBidAsk(outcome, bid, ask)
									t.Engine.UpdateMarketBidAsk(t.ID, outcome, bid, ask)
								}
								t.TokenFullBids[outcome] = toMarketLevels(b.Bids)
								t.TokenFullAsks[outcome] = toMarketLevels(b.Asks)
								// Update TUI immediately on WS data
								t.TUI.UpdateMarketPrices(t.ID, t.TokenBids, t.TokenAsks)
							}
						}
					} else if book, err := api.ParseOrderBook(msg); err == nil && book.AssetID != "" {
						bid, ask := 0.0, 1.0
						for _, order := range book.Bids {
							p, _ := strconv.ParseFloat(order.Price, 64)
							if p > bid {
								bid = p
							}
						}
						for _, order := range book.Asks {
							p, _ := strconv.ParseFloat(order.Price, 64)
							if p < ask && p > 0 {
								ask = p
							}
						}
						if ask >= 1.0 {
							ask = 0.0
						}
						outcome := t.TokenMap[book.AssetID]
						if outcome != "" {
							t.TokenBids[outcome] = bid
							t.TokenAsks[outcome] = ask
							if bid > 0 && ask > 0 && ask < 1.0 {
								mid := (bid + ask) / 2
								t.FloatPrices[outcome] = mid
								tokenPrices[outcome] = fmt.Sprintf("%.3f", mid)
								t.Engine.UpdatePrice(outcome, mid)
								t.Engine.UpdateBidAsk(outcome, bid, ask)
								t.Engine.UpdateMarketBidAsk(t.ID, outcome, bid, ask)
							}
							t.TokenFullBids[outcome] = toMarketLevels(book.Bids)
							t.TokenFullAsks[outcome] = toMarketLevels(book.Asks)
							// Update TUI immediately on WS data
							t.TUI.UpdateMarketPrices(t.ID, t.TokenBids, t.TokenAsks)
						}
					}
				default:
					// No more messages in channel, continue with rest of loop
					goto doneProcessingWS
				}
			}
		doneProcessingWS:

			// If WebSocket channel closed, log once and rely on REST
			if wsChannelClosed && !t.LaddersPlaced {
				t.TUI.LogEvent("[%s] ⚠️ WebSocket closed, using REST API only", t.ID)
			}

			// Update TUI with prices EVERY tick (not just when changed)
			// This ensures the display always reflects current data
			t.TUI.UpdateMarketPrices(t.ID, t.TokenBids, t.TokenAsks)

			// Also update order book depth for live display
			bidDepth := make(map[string][]paper.MarketLevel)
			askDepth := make(map[string][]paper.MarketLevel)

			// Map current trader's depth data for TUI
			for _, outcome := range t.Outcomes {
				if bids, ok := t.TokenFullBids[outcome]; ok {
					bidDepth[outcome] = bids
				}
				if asks, ok := t.TokenFullAsks[outcome]; ok {
					askDepth[outcome] = asks
				}
			}
			t.TUI.UpdateOrderBookDepth(t.ID, bidDepth, askDepth)

			// Process order fills
			for outcome := range tokenPrices {
				bids := t.TokenFullBids[outcome]
				asks := t.TokenFullAsks[outcome]
				if len(bids) > 0 || len(asks) > 0 {
					t.OrderBook.ProcessPriceUpdate(outcome, bids, asks)
				}
			}

			// Check liquidity
			hasLiquidity := false
			if len(t.Outcomes) == 2 {
				for _, outcome := range t.Outcomes {
					bid := t.TokenBids[outcome]
					ask := t.TokenAsks[outcome]
					if (bid >= 0.15 && bid <= 0.85) || (ask >= 0.15 && ask <= 0.85) {
						hasLiquidity = true
						break
					}
				}
			}

			if hasLiquidity {
				lastGoodLiquidity = time.Now()
			} else if time.Since(lastGoodLiquidity) > liquidityTimeout {
				positions := t.Engine.GetPositions()
				if len(positions) > 0 {
					// Check if market should have expired by now
					if time.Now().After(t.EndTime) {
						t.TUI.LogEvent("[%s] ⏰ Market expired, forcing resolution", t.ID)
						winner := simulateResolution(t.Outcomes, tokenPrices)
						t.TUI.LogEvent("[%s] 🏆 WINNER: %s", t.ID, winner)
						result := t.Engine.RedeemWithDetails(winner)
						if result.WinningShares > 0 || result.LosingShares > 0 {
							t.TUI.LogEvent("[%s] 💰 Redemption: $%.2f", t.ID, result.TotalPnL)
						}
						finalStats := t.Engine.GetStats()
						return &marketResult{
							realizedPnL: finalStats.RealizedPnL - startingRealizedPnL,
							trades:      finalStats.TotalTrades - tradesAtStart,
						}, nil
					}

					if !t.LaddersPlaced {
						t.TUI.LogEvent("[%s] ⏳ Waiting for resolution with positions...", t.ID)
						t.LaddersPlaced = true
					}
					t.LadderMgr.CancelAllLadders()
					// Don't reset lastGoodLiquidity - let the safety timeout catch infinite loops
					continue
				}

				t.TUI.LogEvent("[%s] 💨 Liquidity dried up", t.ID)
				t.LadderMgr.CancelAllLadders()

				finalStats := t.Engine.GetStats()
				return &marketResult{
					realizedPnL: finalStats.RealizedPnL - startingRealizedPnL,
					trades:      finalStats.TotalTrades - tradesAtStart,
				}, nil
			}

			// Trading logic - check every tick for arbitrage opportunities
			if len(tokenPrices) == 2 && len(t.Outcomes) == 2 && marketState == paper.MarketStateActive {
				ask1 := t.TokenAsks[t.Outcomes[0]]
				ask2 := t.TokenAsks[t.Outcomes[1]]

				if ask1 >= 0.10 && ask1 <= 0.90 && ask2 >= 0.10 && ask2 <= 0.90 {
					sum := ask1 + ask2
					margin := (1.0 - sum) * 100

					const minMarginPercent = 1.5

					if margin >= minMarginPercent && t.RiskMgr.CanPlaceOrder(ladderConfig.SharesPerLevel*(ask1+ask2)) {
						baseShares := ladderConfig.SharesPerLevel
						shares := baseShares

						// Scale shares based on margin
						if margin >= 5.0 {
							shares = baseShares * 4
						} else if margin >= 4.0 {
							shares = baseShares * 3
						} else if margin >= 3.0 {
							shares = baseShares * 2
						}

						// Apply compounding multiplier from profitable rounds
						compoundMult := t.Engine.GetCompoundMultiplier()
						shares = float64(int(float64(shares) * compoundMult))

						cost := shares * (ask1 + ask2)
						if !t.RiskMgr.CanPlaceOrder(cost) {
							// Scale back if over risk limit
							shares = baseShares
						}

						profit := shares * (1.0 - sum)
						if compoundMult > 1.0 {
							t.TUI.LogEvent("[%s] 🎯 ARB! %s@$%.2f + %s@$%.2f = $%.2f | %.0f shares (%.1fx), profit $%.2f (%.1f%%)",
								t.ID, t.Outcomes[0], ask1, t.Outcomes[1], ask2, sum, shares, compoundMult, profit, margin)
						} else {
							t.TUI.LogEvent("[%s] 🎯 ARB! %s@$%.2f + %s@$%.2f = $%.2f | %.0f shares, profit $%.2f (%.1f%%)",
								t.ID, t.Outcomes[0], ask1, t.Outcomes[1], ask2, sum, shares, profit, margin)
						}

						t.Engine.BuyForMarket(t.ID, t.Outcomes[0], ask1, shares)
						t.Engine.BuyForMarket(t.ID, t.Outcomes[1], ask2, shares)

						lastLadderUpdate = time.Now()
						t.LaddersPlaced = true
					}
				}
			}

			// Suppress unused variable warnings
			_ = lastLadderUpdate

			// Small sleep to prevent CPU spinning
			// Use 20ms for low latency trading (50 ticks/sec)
			time.Sleep(20 * time.Millisecond)
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

func toMarketLevels(levels []api.PriceLevel) []paper.MarketLevel {
	result := make([]paper.MarketLevel, len(levels))
	for i, l := range levels {
		p, _ := strconv.ParseFloat(l.Price, 64)
		s, _ := strconv.ParseFloat(l.Size, 64)
		result[i] = paper.MarketLevel{Price: p, Size: s}
	}
	return result
}
