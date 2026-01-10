package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
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
)

const (
	UseLiveUI = true // Set to false for traditional logging
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("Error: %v", err)
	}
}

// restoreTerminal ensures terminal is in a usable state
func restoreTerminal() {
	restoreEcho := exec.Command("stty", "sane")
	restoreEcho.Stdin = os.Stdin
	_ = restoreEcho.Run()
	fmt.Print("\033[?25h")
	fmt.Print("\033[?1049l")
	fmt.Println()
}

func run() error {
	startTime := time.Now()
	fmt.Print("\033[H\033[2J") // Clear screen

	fmt.Println("╔═══════════════════════════════════════════════════════╗")
	fmt.Println("║     POLYMARKET REAL TRADING BOT                       ║")
	fmt.Println("║     ⚠️  WARNING: This uses REAL money! ⚠️              ║")
	fmt.Println("╚═══════════════════════════════════════════════════════╝")
	fmt.Printf("⏰ Started at: %s\n", startTime.Format("2006-01-02 15:04:05"))
	fmt.Println()

	// Load configuration
	cfg, err := core.LoadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Ensure we're in real mode
	if cfg.IsPaperMode() {
		fmt.Println("❌ TRADING_MODE is not set to 'real'")
		fmt.Println()
		fmt.Println("To use real trading:")
		fmt.Println("  1. Edit your .env file")
		fmt.Println("  2. Set TRADING_MODE=real")
		fmt.Println("  3. Add your credentials:")
		fmt.Println("     POLY_PK=your_private_key")
		fmt.Println("     POLY_API_KEY=your_api_key")
		fmt.Println("     POLY_API_SECRET=your_api_secret")
		fmt.Println("     POLY_PASSPHRASE=your_passphrase")
		fmt.Println()
		fmt.Println("For paper trading, use: go run cmd/bot/main.go")
		return nil
	}

	// Validate credentials
	if err := cfg.ValidateForRealTrading(); err != nil {
		return fmt.Errorf("credential validation failed: %w", err)
	}
	fmt.Println("✅ Credentials validated")

	// Create real trader
	realTrader, err := trading.NewRealTrader(cfg)
	if err != nil {
		return fmt.Errorf("failed to create trader: %w", err)
	}

	// Setup context
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

	// Display wallet info
	fmt.Println()
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Printf("🔑 Wallet: %s\n", realTrader.Address())

	// Get balance from CLOB API
	balance, err := realTrader.GetBalance(ctx)
	if err != nil {
		fmt.Printf("⚠️  Could not fetch balance: %v\n", err)
	} else {
		fmt.Printf("💵 Available Balance: $%.2f USDC\n", balance)
	}

	// Get positions
	positions, err := realTrader.GetPositions(ctx)
	if err != nil {
		fmt.Printf("⚠️  Could not fetch positions: %v\n", err)
	} else if len(positions) > 0 {
		fmt.Println()
		fmt.Println("📊 Current Positions:")
		for _, pos := range positions {
			outcomeDisplay := pos.Outcome
			if outcomeDisplay == "" {
				outcomeDisplay = pos.TokenID
			}
			outcomeDisplay = core.SanitizeString(outcomeDisplay)
			fmt.Printf("   • %s: %.2f shares @ $%.4f avg\n", outcomeDisplay, pos.Size, pos.AvgPrice)
		}
	} else {
		fmt.Println("📊 No open positions")
	}

	// Check MATIC for gas
	polygonClient := api.NewPolygonClient(cfg.PolygonRPCURL)
	maticBalance, err := polygonClient.GetMATICBalance(ctx, realTrader.Address())
	if err != nil {
		fmt.Printf("⚠️  Could not fetch MATIC balance: %v\n", err)
	} else {
		fmt.Printf("⛽ Gas Balance: %.4f MATIC\n", maticBalance)
		if maticBalance < 0.1 {
			fmt.Println("   ⚠️  Low MATIC - you may need more for gas")
		}
	}

	fmt.Println("═══════════════════════════════════════════════════════")
	cancel() // Done with initial queries

	// Display safety settings
	fmt.Println()
	fmt.Println("🛡️  Safety Settings:")
	fmt.Printf("   • Max trade size: $%.2f\n", cfg.MaxTradeSize)
	fmt.Printf("   • Max daily loss: $%.2f\n", cfg.MaxDailyLoss)
	fmt.Println()

	// Confirmation prompt
	if cfg.RequireConfirm {
		fmt.Println("╔═══════════════════════════════════════════════════════╗")
		fmt.Println("║  Type 'YES' to start real trading                     ║")
		fmt.Println("║  Type 'view' to just view markets without trading     ║")
		fmt.Println("╚═══════════════════════════════════════════════════════╝")
		fmt.Print("> ")

		reader := bufio.NewReader(os.Stdin)
		input, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("failed to read input: %w", err)
		}

		input = strings.TrimSpace(strings.ToLower(input))
		if input == "view" {
			return viewMarketsOnly(cfg, realTrader)
		}
		if input != "yes" {
			fmt.Println("❌ Cancelled")
			return nil
		}
		fmt.Println("✅ Starting real trading bot...")
	}

	// Setup signal handling
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Panic recovery
	defer func() {
		if r := recover(); r != nil {
			restoreTerminal()
			stack := make([]byte, 4096)
			length := runtime.Stack(stack, false)
			fmt.Printf("\n🚨 PANIC: %v\n%s\n", r, stack[:length])
		}
	}()

	// Watchdog for graceful shutdown
	go func() {
		<-ctx.Done()
		time.Sleep(5 * time.Second)
		restoreTerminal()
		fmt.Println("\n⚠️ Force exit")
		os.Exit(1)
	}()

	// Disable terminal echo
	disableEcho := exec.Command("stty", "-echo", "-icanon")
	disableEcho.Stdin = os.Stdin
	_ = disableEcho.Run()
	defer restoreTerminal()

	// Create paper engine for TUI display (tracks simulated P&L alongside real trades)
	engine := paper.NewEngine(balance)
	orderBook := paper.NewOrderBook()

	tui := paper.NewTUI(engine, orderBook)

	// Start TUI
	if UseLiveUI {
		tui.StartRenderLoop(250 * time.Millisecond)
		defer tui.Stop()
	}

	restClient := api.NewRestClient("")

	// Network health monitor
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				start := time.Now()
				// Use a lightweight check for latency
				pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
				_, err := restClient.Get15mMarkets(pingCtx, []string{"btc"})
				cancel()
				if err == nil {
					tui.UpdateLatency(time.Since(start))
				}
			}
		}
	}()

	// Main trading loop
	for {
		select {
		case <-ctx.Done():
			tui.Stop()
			fmt.Println("\n👋 Shutting down...")

			// Cancel all open orders
			cancelCtx, cancelFn := context.WithTimeout(context.Background(), 10*time.Second)
			if err := realTrader.CancelAll(cancelCtx); err != nil {
				fmt.Printf("⚠️  Failed to cancel orders: %v\n", err)
			} else {
				fmt.Println("✅ All orders cancelled")
			}
			cancelFn()

			// Show final balance
			balCtx, balFn := context.WithTimeout(context.Background(), 10*time.Second)
			finalBalance, err := realTrader.GetBalance(balCtx)
			balFn()
			duration := time.Since(startTime).Round(time.Second)
			if err == nil {
				fmt.Printf("💵 Final Balance: $%.2f | Duration: %v\n", finalBalance, duration)
			} else {
				fmt.Printf("⏱️  Total Duration: %v\n", duration)
			}
			return nil
		default:
		}

		// Get fresh balance at start of each round for compounding
		balCtx, balFn := context.WithTimeout(ctx, 10*time.Second)
		currentBalance, err := realTrader.GetBalance(balCtx)
		balFn()
		if err != nil {
			tui.LogEvent("⚠️ Could not refresh balance: %v", err)
			currentBalance = balance // Use last known balance
		} else {
			balance = currentBalance // Update stored balance
		}

		// Track starting equity for compounding calculation
		startingEquity := engine.GetEquity()
		compoundMultiplier := engine.GetCompoundMultiplier()
		tui.LogEvent("📊 Round starting | Balance: $%.2f | Multiplier: %.2fx", currentBalance, compoundMultiplier)

		// Find markets
		tui.LogEvent("🔍 Searching for active 15m markets...")
		markets := findMarkets(ctx, restClient, tui)
		if len(markets) == 0 {
			tui.LogEvent("⏳ No active markets, waiting...")
			select {
			case <-ctx.Done():
				continue
			case <-time.After(5 * time.Second):
			}
			continue
		}

		// Trade each market
		var wg sync.WaitGroup
		for assetID, market := range markets {
			endTime, _ := paper.ParseEndTimeFromSlug(market.Slug)
			outcomes := getOutcomes(market)
			tui.AddMarket(assetID, market.Slug, outcomes, endTime)
			tui.LogEvent("🚀 Trading %s: %s", assetID, market.Slug)

			// Create per-market Risk Manager
			riskConfig := paper.RiskConfig{
				MaxExposure:        500.0, // $500 max exposure
				MaxUnmatchedRatio:  0.20,  // 20% max unmatched
				MaxUnmatchedShares: 500.0, // 500 shares max on one side
				SkewThreshold:      0.10,  // 10% skew triggers rebalance
				KillSwitchDrawdown: 0.25, // 25% drawdown triggers kill switch (real money protection)
			}
			marketRiskMgr := paper.NewRiskManager(riskConfig, engine, orderBook, outcomes)

			wg.Add(1)
			go func(id string, m *api.Market, end time.Time, r *paper.RiskManager, bal float64) {
				defer wg.Done()
				tradeMarket(ctx, id, m, end, realTrader, engine, orderBook, r, tui, restClient, cfg, bal)
			}(assetID, market, endTime, marketRiskMgr, currentBalance)
		}

		// Wait for markets to complete
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
			// Calculate round PnL and update compounding multiplier
			roundPnL := engine.GetEquity() - startingEquity
			engine.UpdateCompoundMultiplier(roundPnL, startingEquity)
			newMultiplier := engine.GetCompoundMultiplier()

			if roundPnL > 0 {
				tui.LogEvent("📈 PROFIT! Round PnL: +$%.2f | Multiplier: %.2fx → %.2fx", roundPnL, compoundMultiplier, newMultiplier)
			} else if roundPnL < 0 {
				tui.LogEvent("📉 Loss. Round PnL: $%.2f | Multiplier: %.2fx → %.2fx", roundPnL, compoundMultiplier, newMultiplier)
			} else {
				tui.LogEvent("✅ Round complete, no change | Multiplier: %.2fx", newMultiplier)
			}
		case <-ctx.Done():
		}

		tui.ClearMarkets()
	}
}

func viewMarketsOnly(cfg *core.Config, trader *trading.RealTrader) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	restClient := api.NewRestClient("")

	fmt.Println()
	fmt.Println("🔍 Searching for active markets...")

	markets, err := restClient.Get15mMarkets(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to fetch markets: %w", err)
	}

	if len(markets) == 0 {
		fmt.Println("📭 No active markets found")
		return nil
	}

	fmt.Printf("\n📊 Found %d market(s):\n", len(markets))
	fmt.Println("═══════════════════════════════════════════════════════")

	for _, m := range markets {
		fmt.Printf("\n📈 %s\n", m.Slug)

		tokenMap := make(map[string]string)
		for _, t := range m.Tokens {
			tokenMap[t.TokenID] = t.Outcome
		}

		prices, err := restClient.GetCLOBBidAsk(ctx, tokenMap)
		if err != nil {
			fmt.Printf("   ⚠️  Error: %v\n", err)
			continue
		}

		sumAsks := 0.0
		for outcome, pa := range prices {
			spread := pa.Ask - pa.Bid
			fmt.Printf("   %s: Bid $%.3f | Ask $%.3f | Spread $%.3f\n",
				outcome, pa.Bid, pa.Ask, spread)
			sumAsks += pa.Ask
		}

		margin := (1.0 - sumAsks) * 100
		if margin > 0 {
			fmt.Printf("   💰 Arb margin: %.2f%% (sum=$%.3f)\n", margin, sumAsks)
		} else {
			fmt.Printf("   ❌ No arb (sum=$%.3f)\n", sumAsks)
		}
	}

	fmt.Println("\n═══════════════════════════════════════════════════════")
	return nil
}

func findMarkets(ctx context.Context, restClient *api.RestClient, tui *paper.TUI) map[string]*api.Market {
	found := make(map[string]*api.Market)
	assets := []string{"btc", "eth", "sol"}

	for attempts := 0; attempts < 30; attempts++ {
		select {
		case <-ctx.Done():
			return found
		default:
		}

		markets, err := restClient.Get15mMarkets(ctx, nil)
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
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

		if len(found) > 0 {
			return found
		}

		time.Sleep(500 * time.Millisecond)
	}

	return found
}

func getOutcomes(market *api.Market) []string {
	outcomes := make([]string, 0, len(market.Tokens))
	for _, token := range market.Tokens {
		outcomes = append(outcomes, token.Outcome)
	}
	return outcomes
}

func tradeMarket(ctx context.Context, id string, market *api.Market, endTime time.Time, trader *trading.RealTrader, engine *paper.Engine, orderBook *paper.OrderBook, riskMgr *paper.RiskManager, tui *paper.TUI, restClient *api.RestClient, cfg *core.Config, currentBalance float64) {
	tokenMap := make(map[string]string)
	tokenToOutcome := make(map[string]string)
	for _, token := range market.Tokens {
		tokenMap[token.TokenID] = token.Outcome
		tokenToOutcome[token.TokenID] = token.Outcome
	}

	outcomes := getOutcomes(market)

	// Setup WebSocket
	wsMgr := api.NewWSManager("")
	if err := wsMgr.Connect(ctx); err != nil {
		tui.LogEvent("[%s] ❌ WS connect failed: %v", id, err)
		return
	}
	defer wsMgr.Close()

	// Subscribe to order books
	var assetIDs []string
	for _, token := range market.Tokens {
		assetIDs = append(assetIDs, token.TokenID)
	}
	sub := map[string]interface{}{
		"type":       "market",
		"assets_ids": assetIDs,
	}
	if err := wsMgr.Subscribe(ctx, sub); err != nil {
		tui.LogEvent("[%s] ❌ Subscribe failed: %v", id, err)
		return
	}

	wsMsgChan := wsMgr.StartStreaming(ctx)
	tui.LogEvent("[%s] 📡 Connected, trading until %v", id, endTime.Format("15:04:05"))

	// Fetch fee rates for the tokens
	tokenFeeRates := make(map[string]int)
	for tid, outcome := range tokenMap {
		rate, err := restClient.GetFeeRate(ctx, tid)
		if err == nil {
			tokenFeeRates[outcome] = rate
			if rate > 0 {
				tui.LogEvent("[%s] ℹ️ Fee enabled for %s: %.2f%% (%d bps)", id, outcome, float64(rate)/100.0, rate)
			}
		}
	}

	tokenBids := make(map[string]float64)
	tokenAsks := make(map[string]float64)
	tokenFullBids := make(map[string][]paper.MarketLevel)
	tokenFullAsks := make(map[string][]paper.MarketLevel)
	lastUpdate := time.Now()
	lastRestPoll := time.Now()
	lastTrade := time.Time{}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Check if market ended
		if time.Now().After(endTime.Add(30 * time.Second)) {
			tui.LogEvent("[%s] ⏰ Market ended", id)
			checkRedemption(ctx, id, market.ConditionID, trader, engine, tui)
			return
		}

		// Check kill switch - DON'T EXIT, just pause trading
		// Exiting would leave positions unmatched; better to hold until expiration
		killSwitchActive := riskMgr.IsKillSwitchTriggered()

		// ============ FAST WEBSOCKET PROCESSING ============
		messagesProcessed := 0
		for {
			select {
			case msg, ok := <-wsMsgChan:
				if !ok {
					tui.LogEvent("[%s] ⚠️ WS closed", id)
					return
				}
				messagesProcessed++

				if books, err := api.ParseOrderBooks(msg); err == nil && len(books) > 0 {
					for _, b := range books {
						outcome := tokenToOutcome[b.AssetID]
						if outcome == "" {
							continue
						}

						bid, ask := 0.0, 1.0
						for _, order := range b.Bids {
							p, err := strconv.ParseFloat(order.Price, 64)
							if err != nil {
								continue
							}
							if p > bid {
								bid = p
							}
						}
						for _, order := range b.Asks {
							p, err := strconv.ParseFloat(order.Price, 64)
							if err != nil {
								continue
							}
							if p < ask && p > 0 {
								ask = p
							}
						}
						if ask >= 1.0 {
							ask = 0
						}

						tokenBids[outcome] = bid
						tokenAsks[outcome] = ask
						
						// Track full depth for liquidity checks
						tokenFullBids[outcome] = toMarketLevels(tui, id, b.Bids)
						tokenFullAsks[outcome] = toMarketLevels(tui, id, b.Asks)

						if bid > 0 && ask > 0 && ask < 1.0 {
							mid := (bid + ask) / 2
							engine.UpdateMarketData(id, outcome, mid, bid, ask)
						}
					}
					lastUpdate = time.Now()
				}
			default:
				goto doneWS
			}
		}
	doneWS:

		if messagesProcessed > 0 {
			tui.UpdateMarketPricesWithSource(id, tokenBids, tokenAsks, "WS")
		}

						// ============ REST PRIMARY FOR LIQUIDITY ============
						// REST is now PRIMARY for liquidity data (WS doesn't send liquidity updates)
						// Poll REST every 20ms for high-frequency liquidity updates (50 RPS per trader)
						// Global rate limiter in RestClient caps total at 148 RPS across all traders
						staleTime := time.Since(lastUpdate)
						restPollInterval := 20 * time.Millisecond
		needsRestPoll := time.Since(lastRestPoll) > restPollInterval

		// Update WS staleness in TUI
		wsTimeSinceMsg := wsMgr.TimeSinceLastMessage()
		tui.UpdateWSLatency(wsTimeSinceMsg)

		// Also force REST if WS is unhealthy
		wsUnhealthy := !wsMgr.IsConnected() || wsTimeSinceMsg > 10*time.Second
		if wsUnhealthy && staleTime > 3*time.Second {
			needsRestPoll = true
		}

		if needsRestPoll {
			lastRestPoll = time.Now()
			// Note: REST fallback updated to also capture full depth
			if handleRestFallbackWithDepth(ctx, id, tokenMap, tokenBids, tokenAsks, tokenFullBids, tokenFullAsks, engine, restClient, tui) {
				lastUpdate = time.Now()
			}
		}

		// ============ TRADING LOGIC ============
		// Skip new trades if kill switch active, but keep monitoring (don't exit)
		if killSwitchActive {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		if len(tokenAsks) >= 2 && len(outcomes) == 2 {
			ask1 := tokenAsks[outcomes[0]]
			ask2 := tokenAsks[outcomes[1]]

			if ask1 >= 0.10 && ask1 <= 0.90 && ask2 >= 0.10 && ask2 <= 0.90 {
				sum := ask1 + ask2
				margin := (1.0 - sum) * 100

				if margin >= cfg.MinMarginPercent {
					// Evaluate risk
					riskAction, riskReason := riskMgr.Evaluate()
					if riskAction == paper.RiskActionKillSwitch {
						tui.LogEvent("[%s] 🛑 RISK: Kill switch - %s (pausing, not exiting)", id, riskReason)
						continue
					}

					// Dynamic trade size based on EQUITY (not just cash)
					// This ensures consistent sizing regardless of how much is in positions
					currentEquity := engine.GetEquity()
					currentCash := currentBalance
					latestBalance, _ := trader.GetBalance(ctx)
					if latestBalance > 0 {
						currentCash = latestBalance
						currentBalance = latestBalance
					}

					// Use equity for sizing calculation
					// Equity naturally grows with profits, providing automatic compounding
					tradeSize := cfg.CalculateTradeSize(currentEquity)

					// Scale shares based on margin
					shares := tradeSize / sum

					// Apply aggression scaling based on margin (2%, 3%, 4%+)
					// Higher margin = better opportunity = more aggressive sizing
					if riskAction != paper.RiskActionReduceSize {
						if margin >= 4.0 {
							shares *= 4
						} else if margin >= 3.0 {
							shares *= 3
						} else if margin >= 2.0 {
							shares *= 2
						}
						// 1% margin = 1x (no scaling, baseline)
					}

					// Round to whole shares
					shares = float64(int(shares))

					// Get max fee rate for conservative margin calculation
					maxFeeRateBps := 0
					if rate1, ok := tokenFeeRates[outcomes[0]]; ok && rate1 > maxFeeRateBps {
						maxFeeRateBps = rate1
					}
					if rate2, ok := tokenFeeRates[outcomes[1]]; ok && rate2 > maxFeeRateBps {
						maxFeeRateBps = rate2
					}

					// AGGREGATED LIQUIDITY: Calculate total matched liquidity across ALL price levels
					// that maintain minimum margin. This allows "chasing" liquidity deeper into the book.
					maxSum := 1.0 - (cfg.MinMarginPercent / 100.0) // e.g., 2% margin → max sum = 0.98

					// Copy and sort asks by price ascending for both outcomes
					asks1 := make([]paper.MarketLevel, len(tokenFullAsks[outcomes[0]]))
					copy(asks1, tokenFullAsks[outcomes[0]])
					sort.Slice(asks1, func(i, j int) bool { return asks1[i].Price < asks1[j].Price })

					asks2 := make([]paper.MarketLevel, len(tokenFullAsks[outcomes[1]]))
					copy(asks2, tokenFullAsks[outcomes[1]])
					sort.Slice(asks2, func(i, j int) bool { return asks2[i].Price < asks2[j].Price })

					// Calculate aggregated matched liquidity across valid price levels
					var totalMatchedLiquidity float64
					var rawLiq1, rawLiq2 float64
					var maxValidI, maxValidJ int

					i, j := 0, 0
					for i < len(asks1) && j < len(asks2) {
						p1 := asks1[i].Price
						p2 := asks2[j].Price

						// Check if this combination maintains minimum margin
						if p1+p2 > maxSum {
							break // Can't go deeper, would exceed margin threshold
						}


						// Get liquidity at current levels
						levelLiq1 := asks1[i].Size
						levelLiq2 := asks2[j].Size

						// Matched liquidity = min of both sides (arbitrage requires equal shares)
						matchedAtLevel := levelLiq1
						if levelLiq2 < matchedAtLevel {
							matchedAtLevel = levelLiq2
						}

						
						if i+1 > maxValidI { maxValidI = i+1; rawLiq1 += asks1[i].Size }
						if j+1 > maxValidJ { maxValidJ = j+1; rawLiq2 += asks2[j].Size }
						totalMatchedLiquidity += matchedAtLevel

						// Move pointer on the side with less remaining liquidity
						remaining1 := levelLiq1 - matchedAtLevel
						remaining2 := levelLiq2 - matchedAtLevel

						if remaining1 <= 0 {
							i++
						} else {
							asks1[i].Size = remaining1
						}
						if remaining2 <= 0 {
							j++
						} else {
							asks2[j].Size = remaining2
						}
					}

					// Use aggregated liquidity for display
					liq1 := rawLiq1
					liq2 := rawLiq2
					minLiquidity := totalMatchedLiquidity
				bookDepth1 := len(tokenFullAsks[outcomes[0]])
					bookDepth2 := len(tokenFullAsks[outcomes[1]])

					// Use 100% of matched liquidity - MarketBuy walks the book atomically for guaranteed fills
					// No legging risk since we execute both sides simultaneously, not single-level limit orders
					maxSafeShares := minLiquidity * 1.00
					if shares > maxSafeShares {
						shares = maxSafeShares
					}
					
					// Force at least 1 share if there's any matched liquidity and we have budget
					if shares < 1.0 && minLiquidity >= 1.0 {
						shares = 1.0
					}

					// Calculate metrics for reporting
					cost, _, _, _ := calculateTradeMetrics(shares, sum, maxFeeRateBps)

					// REFRESH BALANCE RIGHT BEFORE TRADING to prevent unbalanced fills
					// This is the "Zero Excuse" check to ensure we have enough for BOTH sides
					latestBalance, balErr := trader.GetBalance(ctx)
					if balErr == nil {
						currentBalance = latestBalance
						currentCash = latestBalance
					}

					// Scale down if cost exceeds cash
					if !riskMgr.CanPlaceOrder(cost) || cost < (cost * 1.02) { // 2% buffer for price movements
						if cost > currentCash {
							maxAffordableShares := (currentCash * 0.98) / sum // Use 98% of cash for safety
							if maxAffordableShares < 1.0 {
								continue // Truly not enough cash for 1 share
							}
							shares = math.Floor(maxAffordableShares)
							cost, _, _, _ = calculateTradeMetrics(shares, sum, maxFeeRateBps)
						}
					}

					// FINAL SAFETY CHECK: If we still don't have enough for both sides, SKIP
					if cost > currentCash {
						tui.LogEvent("[%s] ⚠️ Insufficient balance for both sides: Need $%.2f, Have $%.2f", id, cost, currentCash)
						continue
					}

					if time.Since(lastTrade) > 2*time.Second && shares >= 1.0 {
						// Add slippage buffer: willing to pay up to 1.5% more for guaranteed fill
						const slippageBuffer = 0.015
						price1 := ask1 + slippageBuffer
						price2 := ask2 + slippageBuffer

						// Recalculate with buffered prices including order cost overhead
						bufferedSum := price1 + price2
						_, _, _, _ = calculateTradeMetrics(shares, bufferedSum, maxFeeRateBps)
						bufferedMargin := (1.0 - bufferedSum) * 100

						tui.LogEvent("[%s] 🎯 ARB! %s@$%.3f + %s@$%.3f = $%.3f (%.1f%% margin) [liq: %.0f/%.0f, depth: %d/%d→%d/%d]",
							id, outcomes[0], price1, outcomes[1], price2, bufferedSum, bufferedMargin, liq1, liq2, bookDepth1, bookDepth2, maxValidI, maxValidJ)

						// Map tokens
						token0, token1 := "", ""
						for tid, out := range tokenToOutcome {
							if out == outcomes[0] {
								token0 = tid
							} else if out == outcomes[1] {
								token1 = tid
							}
						}

						// MARKET EXECUTION: Force fill both sides with GTC to avoid FOK cancels
						const worstCasePrice = 0.95
						
						var wg sync.WaitGroup
						wg.Add(2)

						var res1, res2 *trading.TradeResult
						var err1, err2 error

						go func() {
							defer wg.Done()
							rate := tokenFeeRates[outcomes[0]]
							res1, err1 = trader.Buy(ctx, token0, outcomes[0], worstCasePrice, shares, api.OrderTypeMarket, api.TIFGoodTilCancelled, rate)
						}()

						go func() {
							defer wg.Done()
							rate := tokenFeeRates[outcomes[1]]
							res2, err2 = trader.Buy(ctx, token1, outcomes[1], worstCasePrice, shares, api.OrderTypeMarket, api.TIFGoodTilCancelled, rate)
						}()

						wg.Wait()

						// Calculate costs using the original target price for reporting (actual will be better)
						cost1 := shares * price1
						cost2 := shares * price2

						// Process results
						if err1 == nil && res1 != nil && res1.Success {
							tui.LogEvent("[%s] ✅ Side 1 MARKET: %s (Target $%.3f)", id, outcomes[0], price1)
							engine.BuyForMarket(id, outcomes[0], price1, shares)
							tui.RecordOrder(id, outcomes[0], "BUY", shares, price1, cost1, bufferedMargin, "FILLED")
						} else {
							tui.LogEvent("[%s] ❌ Side 1 MARKET Fail: %v", id, err1)
							tui.RecordOrder(id, outcomes[0], "BUY", shares, price1, cost1, bufferedMargin, "FAILED")
						}

						if err2 == nil && res2 != nil && res2.Success {
							tui.LogEvent("[%s] ✅ Side 2 MARKET: %s (Target $%.3f)", id, outcomes[1], price2)
							engine.BuyForMarket(id, outcomes[1], price2, shares)
							tui.RecordOrder(id, outcomes[1], "BUY", shares, price2, cost2, bufferedMargin, "FILLED")
						} else {
							tui.LogEvent("[%s] ❌ Side 2 MARKET Fail: %v", id, err2)
							tui.RecordOrder(id, outcomes[1], "BUY", shares, price2, cost2, bufferedMargin, "FAILED")
						}

						// ═══════════════════════════════════════════════════════════════
						// UNBALANCED FILL RECOVERY: If one side succeeded and other failed,
						// retry the failed side up to 3 times to prevent unbalanced position
						// ═══════════════════════════════════════════════════════════════
						side1Success := err1 == nil && res1 != nil && res1.Success
						side2Success := err2 == nil && res2 != nil && res2.Success

						if side1Success != side2Success {
							// One succeeded, one failed - need to recover
							failedSide := 1
							failedToken := token0
							failedOutcome := outcomes[0]
							failedPrice := price1
							if side1Success {
								failedSide = 2
								failedToken = token1
								failedOutcome = outcomes[1]
								failedPrice = price2
							}

							tui.LogEvent("[%s] ⚠️ UNBALANCED: Side %d failed, attempting recovery...", id, failedSide)

							// Retry failed side up to 3 times with increasing aggression
							for retry := 1; retry <= 3; retry++ {
								time.Sleep(100 * time.Millisecond) // Brief pause between retries

								// Increase price aggressiveness each retry
								retryPrice := failedPrice + (float64(retry) * 0.01) // +1%, +2%, +3%
								if retryPrice > 0.95 {
									retryPrice = 0.95 // Cap at 95 cents
								}

								tui.LogEvent("[%s] 🔄 Recovery attempt %d/3 for %s @ $%.3f", id, retry, failedOutcome, retryPrice)

								rate := tokenFeeRates[failedOutcome]
								retryRes, retryErr := trader.Buy(ctx, failedToken, failedOutcome, retryPrice, shares, api.OrderTypeMarket, api.TIFGoodTilCancelled, rate)

								if retryErr == nil && retryRes != nil && retryRes.Success {
									tui.LogEvent("[%s] ✅ Recovery SUCCESS for %s!", id, failedOutcome)
									engine.BuyForMarket(id, failedOutcome, retryPrice, shares)
									retryCost := shares * retryPrice
									tui.RecordOrder(id, failedOutcome, "BUY", shares, retryPrice, retryCost, bufferedMargin, "FILLED")
									break
								}

								if retry == 3 {
									tui.LogEvent("[%s] 🚨 Recovery FAILED after 3 attempts - position unbalanced!", id)
									// Position is now unbalanced - will be handled by risk manager
								}
							}
						}

						// Force refresh balance after trade to ensure accurate tracking
						if newBal, err := trader.ForceRefreshBalance(ctx); err == nil {
							currentBalance = newBal
							currentCash = newBal
						}

						lastTrade = time.Now()
					}
				}
			}
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// Helper to match bot's toMarketLevels
func calculateTradeMetrics(shares, sum float64, feeRateBps int) (cost, overhead, gross, net float64) {
	cost = shares * sum

	// Polymarket 15m markets now have taker fees
	// feeRateBps is in basis points (1000 = 10%)
	// Fee is deducted from proceeds (bought tokens or sold USDC)
	overhead = 0
	if feeRateBps > 0 {
		// Effective fee on the total arbitrage cost
		overhead = cost * (float64(feeRateBps) / 10000.0)
	}

	gross = shares * (1.0 - sum)
	net = gross - overhead
	return
}

func toMarketLevels(tui *paper.TUI, id string, levels []api.PriceLevel) []paper.MarketLevel {
	result := make([]paper.MarketLevel, len(levels))
	for i, l := range levels {
		p, _ := strconv.ParseFloat(l.Price, 64)
		s, _ := strconv.ParseFloat(l.Size, 64)
		result[i] = paper.MarketLevel{Price: p, Size: s}
	}
	return result
}

func handleRestFallbackWithDepth(ctx context.Context, id string, tokenMap map[string]string, bids, asks map[string]float64, fullBids, fullAsks map[string][]paper.MarketLevel, engine *paper.Engine, restClient *api.RestClient, tui *paper.TUI) bool {
	success := false
	for tokenID, outcome := range tokenMap {
		start := time.Now()
		book, err := restClient.GetOrderBook(ctx, tokenID)
		latency := time.Since(start)

		// Update TUI with real REST latency
		tui.UpdateRestLatency(latency)

		if err != nil {
			continue
		}

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

		if bid > 0 || (ask > 0 && ask < 1.0) {
			bids[outcome] = bid
			asks[outcome] = ask
			fullBids[outcome] = toMarketLevels(tui, id, book.Bids)
			fullAsks[outcome] = toMarketLevels(tui, id, book.Asks)

			if bid > 0 && ask > 0 && ask < 1.0 {
				mid := (bid + ask) / 2
				engine.UpdateMarketData(id, outcome, mid, bid, ask)
			}
			success = true
		}
	}
	if success {
		tui.UpdateMarketPricesWithSource(id, bids, asks, "REST")
	}
	return success
}


func handleRestFallback(ctx context.Context, id string, tokenMap map[string]string, bids, asks map[string]float64, engine *paper.Engine, restClient *api.RestClient, tui *paper.TUI) bool {
	success := false
	for tokenID, outcome := range tokenMap {
		book, err := restClient.GetOrderBook(ctx, tokenID)
		if err != nil {
			continue
		}

		bid, ask := 0.0, 1.0
		for _, b := range book.Bids {
			p, err := strconv.ParseFloat(b.Price, 64)
			if err != nil {
				continue
			}
			if p > bid {
				bid = p
			}
		}
		for _, a := range book.Asks {
			p, err := strconv.ParseFloat(a.Price, 64)
			if err != nil {
				continue
			}
			if p < ask && p > 0 {
				ask = p
			}
		}
		if ask >= 1.0 {
			ask = 0
		}

		if bid > 0 && ask > 0 {
			bids[outcome] = bid
			asks[outcome] = ask
			mid := (bid + ask) / 2
			engine.UpdateMarketData(id, outcome, mid, bid, ask)
			success = true
		}
	}
	if success {
		tui.UpdateMarketPricesWithSource(id, bids, asks, "REST")
	}
	return success
}

func checkRedemption(ctx context.Context, id, conditionID string, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI) {
	// Retry resolution check with exponential backoff
	// 15-min markets may take a few seconds to resolve
	retryDelays := []time.Duration{5 * time.Second, 10 * time.Second, 30 * time.Second, 60 * time.Second}

	for attempt, delay := range retryDelays {
		time.Sleep(delay)

		info, err := trader.GetMarketInfo(ctx, conditionID)
		if err != nil {
			tui.LogEvent("[%s] ⚠️ Resolution check %d failed: %v", id, attempt+1, err)
			continue
		}

		winner := ""
		for _, token := range info.Tokens {
			if token.Winner {
				winner = token.Outcome
				break
			}
		}

		if winner != "" {
			result := engine.RedeemWithDetails(winner)
			if result.TotalPnL != 0 {
				pnlSign := "+"
				pnlEmoji := "💰"
				if result.TotalPnL < 0 {
					pnlSign = ""
					pnlEmoji = "💸"
				}
				tui.LogEvent("[%s] %s RESOLVED: %s won | PnL: %s$%.2f", id, pnlEmoji, winner, pnlSign, result.TotalPnL)

				// Record loss for safety limits
				if result.TotalPnL < 0 && trader != nil {
					trader.RecordLoss(-result.TotalPnL)
				}

				// AUTOMATIC ON-CHAIN REDEMPTION
				// This converts winning tokens back into spendable USDC
				go func() {
					tui.LogEvent("[%s] ⏳ Starting on-chain redemption...", id)
					// Wait a bit for on-chain state to sync
					time.Sleep(30 * time.Second)
					txHash, err := trader.RedeemOnChain(ctx, conditionID)
					if err != nil {
						tui.LogEvent("[%s] ⚠️ On-chain redeem pending: %v", id, err)
					} else {
						tui.LogEvent("[%s] ✅ REDEEMED! Tx: %s", id, txHash[:10]+"...")
					}
				}()
			} else {
				tui.LogEvent("[%s] 📭 Market resolved: %s (no positions)", id, winner)
			}
			return
		}

		tui.LogEvent("[%s] ⏳ Resolution pending... (attempt %d/%d)", id, attempt+1, len(retryDelays))
	}

	tui.LogEvent("[%s] ⚠️ Could not get resolution after %d attempts", id, len(retryDelays))
}
