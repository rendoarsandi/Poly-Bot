package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
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
		fmt.Println("     PK=your_private_key")
		fmt.Println("     API_KEY=your_api_key")
		fmt.Println("     API_SECRET=your_api_secret")
		fmt.Println("     API_PASSPHRASE=your_passphrase")
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
	if cfg.DryRunFirst {
		fmt.Println("   • Mode: DRY-RUN (orders simulated)")
		fmt.Println("     Set DRY_RUN_FIRST=false to place real orders")
	} else {
		fmt.Println("   • Mode: LIVE (real orders will be placed!)")
	}
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
				KillSwitchDrawdown: 999.0, // High limit as we're using real money
			}
			marketRiskMgr := paper.NewRiskManager(riskConfig, engine, orderBook, outcomes)

			wg.Add(1)
			go func(id string, m *api.Market, end time.Time, r *paper.RiskManager, bal float64, mult float64) {
				defer wg.Done()
				tradeMarket(ctx, id, m, end, realTrader, engine, orderBook, r, tui, restClient, cfg, bal, mult)
			}(assetID, market, endTime, marketRiskMgr, currentBalance, compoundMultiplier)
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
	assets := []string{"btc", "eth", "sol", "xrp"}

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

func tradeMarket(ctx context.Context, id string, market *api.Market, endTime time.Time, trader *trading.RealTrader, engine *paper.Engine, orderBook *paper.OrderBook, riskMgr *paper.RiskManager, tui *paper.TUI, restClient *api.RestClient, cfg *core.Config, currentBalance float64, compoundMultiplier float64) {
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

		// Check kill switch
		if riskMgr.IsKillSwitchTriggered() {
			tui.LogEvent("[%s] 🛑 RISK: Kill switch active", id)
			return
		}

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
			tui.UpdateMarketPrices(id, tokenBids, tokenAsks)
		}

		// ============ REST FALLBACK ============
		staleTime := time.Since(lastUpdate)
		if (!wsMgr.IsConnected() || wsMgr.TimeSinceLastMessage() > 15*time.Second) && staleTime > 3*time.Second {
			if time.Since(lastRestPoll) > 2*time.Second {
				lastRestPoll = time.Now()
				// Note: REST fallback updated to also capture full depth
				if handleRestFallbackWithDepth(ctx, id, tokenMap, tokenBids, tokenAsks, tokenFullBids, tokenFullAsks, engine, restClient, tui) {
					lastUpdate = time.Now()
				}
			}
		}

		// ============ TRADING LOGIC ============
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
						tui.LogEvent("[%s] 🛑 RISK: Kill switch - %s", id, riskReason)
						continue
					}

					// Dynamic trade size with compounding and margin scaling
					latestBalance, _ := trader.GetBalance(ctx)
					if latestBalance > 0 {
						currentBalance = latestBalance
					}

					baseTradeSize := cfg.CalculateTradeSize(currentBalance)
					tradeSize := baseTradeSize * compoundMultiplier

					// Scale shares based on margin
					shares := tradeSize / sum
					
					// Apply aggression scaling based on margin
					if riskAction != paper.RiskActionReduceSize {
						if margin >= 4.0 {
							shares *= 4
						} else if margin >= 3.0 {
							shares *= 3
						} else if margin >= 2.0 {
							shares *= 2
						}
					}

					// Apply compounding multiplier from profitable rounds
					shares = float64(int(shares * compoundMultiplier))

					// LIQUIDITY CHECK: Cap size based on available volume
					maxLiquidity := 1e9
					for _, out := range outcomes {
						asks := tokenFullAsks[out]
						if len(asks) > 0 && asks[0].Size < maxLiquidity {
							maxLiquidity = asks[0].Size
						}
					}
					
					// Use 80% of top-of-book for real trading to avoid slippage
					if shares > maxLiquidity*0.8 {
						shares = maxLiquidity * 0.8
					}

					// Ensure we don't spam and risk allows
					cost := shares * sum
					if time.Since(lastTrade) > 2*time.Second && shares >= 1.0 && riskMgr.CanPlaceOrder(cost) && cost <= currentBalance {
						tui.LogEvent("[%s] 🎯 ARB! %s@$%.3f + %s@$%.3f = $%.3f (%.1f%% margin)",
							id, outcomes[0], ask1, outcomes[1], ask2, sum, margin)

						// Map tokens
						token0, token1 := "", ""
						for tid, out := range tokenToOutcome {
							if out == outcomes[0] {
								token0 = tid
							} else if out == outcomes[1] {
								token1 = tid
							}
						}

						// ATOMIC EXECUTION: Fire both orders in parallel to minimize legging risk
						var wg sync.WaitGroup
						wg.Add(2)
						
						var res1, res2 *trading.TradeResult
						var err1, err2 error
						
						go func() {
							defer wg.Done()
							res1, err1 = trader.Buy(ctx, token0, outcomes[0], ask1, shares, api.TIFGoodTilCancelled)
						}()
						
						go func() {
							defer wg.Done()
							res2, err2 = trader.Buy(ctx, token1, outcomes[1], ask2, shares, api.TIFGoodTilCancelled)
						}()
						
						wg.Wait()

						// Process results
						if err1 == nil && res1.Success {
							tui.LogEvent("[%s] ✅ Side 1 Fill: %s @ $%.3f", id, outcomes[0], ask1)
							engine.BuyForMarket(id, outcomes[0], ask1, shares)
						} else {
							tui.LogEvent("[%s] ❌ Side 1 Fail: %v", id, err1)
						}

						if err2 == nil && res2.Success {
							tui.LogEvent("[%s] ✅ Side 2 Fill: %s @ $%.3f", id, outcomes[1], ask2)
							engine.BuyForMarket(id, outcomes[1], ask2, shares)
						} else {
							tui.LogEvent("[%s] ❌ Side 2 Fail: %v", id, err2)
						}
						
						if (res1 != nil && !res1.Success) || (res2 != nil && !res2.Success) {
							tui.LogEvent("[%s] ⚠️ PARTIAL FILL DETECTED - Position skewed!", id)
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
		book, err := restClient.GetOrderBook(ctx, tokenID)
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
		tui.UpdateMarketPrices(id, bids, asks)
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
		tui.UpdateMarketPrices(id, bids, asks)
	}
	return success
}

func checkRedemption(ctx context.Context, id, conditionID string, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI) {
	// Wait a bit for resolution
	time.Sleep(5 * time.Second)

	info, err := trader.GetMarketInfo(ctx, conditionID)
	if err != nil {
		tui.LogEvent("[%s] ⚠️ Could not fetch resolution: %v", id, err)
		return
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
			tui.LogEvent("[%s] 💰 RESOLVED: %s | PnL: $%.2f", id, winner, result.TotalPnL)
		}
	} else {
		tui.LogEvent("[%s] ⏳ Market pending resolution...", id)
	}
}
