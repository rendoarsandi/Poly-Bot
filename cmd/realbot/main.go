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
		fmt.Println("For paper trading, use: go run cmd/paperbot/main.go")
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

	// Sync CLOB cached allowance with on-chain state
	fmt.Println("🔄 Syncing CLOB balance allowance...")
	if err := realTrader.UpdateBalanceAllowance(ctx); err != nil {
		fmt.Printf("⚠️  Failed to update balance allowance: %v\n", err)
	} else {
		fmt.Println("✅ CLOB balance allowance synced")
	}

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
	if cfg.MaxDailyLoss > 0 {
		fmt.Printf("   • Max daily loss: $%.2f\n", cfg.MaxDailyLoss)
	} else {
		fmt.Println("   • Max daily loss: disabled (using 10% drawdown kill switch)")
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

	restClient := api.NewRestClient("")

	// emergencyCleanup ensures we don't leave hanging orders or unmerged positions
	emergencyCleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		fmt.Println("\n🧹 Running emergency cleanup...")

		// 1. Cancel all open orders
		if err := realTrader.CancelAll(cleanupCtx); err != nil {
			fmt.Printf("⚠️  Failed to cancel orders: %v\n", err)
		} else {
			fmt.Println("✅ All orders cancelled")
		}

		// 2. Identify and merge balanced positions
		positions, err := realTrader.GetPositions(cleanupCtx)
		if err != nil {
			fmt.Printf("⚠️  Could not fetch positions for merge: %v\n", err)
		} else if len(positions) > 0 {
			// Map positions to their markets to find ConditionIDs
			// We'll need a fresh list of markets for this
			markets, err := restClient.Get15mMarkets(cleanupCtx, nil)
			if err == nil {
				// Group tokens by ConditionID
				condToTokens := make(map[string][]string)
				for _, m := range markets {
					for _, t := range m.Tokens {
						condToTokens[m.ConditionID] = append(condToTokens[m.ConditionID], t.TokenID)
					}
				}

				// Find balanced pairs for each market
				for condID, tokens := range condToTokens {
					if len(tokens) != 2 {
						continue
					}

					var qty1, qty2 float64
					for _, pos := range positions {
						if pos.TokenID == tokens[0] {
							qty1 = pos.Size
						} else if pos.TokenID == tokens[1] {
							qty2 = pos.Size
						}
					}

					minQty := qty1
					if qty2 < minQty {
						minQty = qty2
					}

					if minQty >= 0.000001 {
						fmt.Printf("💰 Merging %.6f pairs for market %s...\n", minQty, condID[:10])
						_, err := realTrader.MergeOnChain(cleanupCtx, condID, minQty)
						if err != nil {
							fmt.Printf("❌ Merge failed: %v\n", err)
						} else {
							fmt.Println("✅ Merge successful")
						}
					}
				}
			}
		}
	}

	// Panic recovery
	defer func() {
		if r := recover(); r != nil {
			restoreTerminal()
			stack := make([]byte, 4096)
			length := runtime.Stack(stack, false)
			fmt.Printf("\n🚨 PANIC: %v\n%s\n", r, stack[:length])

			// Run emergency cleanup on panic
			emergencyCleanup()
		}
	}()

	// Watchdog for graceful shutdown
	go func() {
		<-ctx.Done()
		// If we receive another interrupt during cleanup, force exit
		go func() {
			<-ctx.Done()
			restoreTerminal()
			fmt.Println("\n⚠️ Force exit requested")
			os.Exit(1)
		}()

		time.Sleep(10 * time.Second) // Give cleanup more time
		restoreTerminal()
		fmt.Println("\n⚠️ Force exit: cleanup timed out")
		os.Exit(1)
	}()

	// Disable terminal echo
	disableEcho := exec.Command("stty", "-echo", "-icanon")
	disableEcho.Stdin = os.Stdin
	_ = disableEcho.Run()
	defer restoreTerminal()

	engine := paper.NewEngine(balance)
	orderBook := paper.NewOrderBook()
	tui := paper.NewTUI(engine, orderBook)
	tui.SetTradeFactor(cfg.TradeScaleFactor)

	// Start TUI
	if UseLiveUI {
		tui.StartRenderLoop(250 * time.Millisecond)
		defer tui.Stop()
	}

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
				_, err := restClient.Get15mMarkets(pingCtx, []string{"btc", "eth"})
				cancel()
				if err == nil {
					tui.UpdateLatency(time.Since(start))
				}
			}
		}
	}()

	// Main trading loop
	globalSplitStatus := make(map[string]bool)
	var splitMu sync.Mutex

	for {
		select {
		case <-ctx.Done():
			tui.Stop()
			fmt.Println("\n👋 Shutting down...")

			// Run emergency cleanup on graceful shutdown
			emergencyCleanup()

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
			balance = currentBalance          // Update stored balance
			engine.SetBalance(currentBalance) // Sync engine with on-chain balance
			engine.RecalculateDrawdown()      // Check drawdown after sync
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
				MaxExposure:        math.MaxFloat64, // Unlimited exposure (rely on kill switch for safety)
				MaxUnmatchedRatio:  0.20,            // 20% max unmatched
				MaxUnmatchedShares: 500.0,           // 500 shares max on one side
				SkewThreshold:      0.10,            // 10% skew triggers rebalance
				KillSwitchDrawdown: 0.10,            // 10% drawdown triggers kill switch (real money protection)
			}
			marketRiskMgr := paper.NewRiskManager(riskConfig, engine, orderBook, outcomes)

			wg.Add(1)
			go func(id string, m *api.Market, end time.Time, r *paper.RiskManager, bal float64) {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						restoreTerminal()
						stack := make([]byte, 4096)
						length := runtime.Stack(stack, false)
						fmt.Printf("\n🚨 TRADER PANIC [%s]: %v\n%s\n", id, r, stack[:length])
						emergencyCleanup()
					}
				}()
				tradeMarket(ctx, id, m, end, realTrader, engine, orderBook, r, tui, restClient, cfg, bal, globalSplitStatus, &splitMu)
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
			// Sync engine with on-chain balance before calculating round PnL
			// This ensures merges that happened in background are reflected
			endBalCtx, endBalFn := context.WithTimeout(ctx, 10*time.Second)
			if endBal, err := realTrader.GetBalance(endBalCtx); err == nil {
				engine.SetBalance(endBal)
				engine.RecalculateDrawdown()
			}
			endBalFn()

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
	assets := []string{"btc", "eth"}

	// Fast polling for new markets - check every 500ms for first 30 seconds
	// Then slow down to every 2 seconds
	maxFastAttempts := 60 // 30 seconds of fast polling
	maxSlowAttempts := 60 // 2 more minutes of slow polling
	lastLogTime := time.Now()

	for attempts := 0; attempts < maxFastAttempts+maxSlowAttempts; attempts++ {
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
			// Short sleep on error
			select {
			case <-ctx.Done():
				return found
			case <-time.After(500 * time.Millisecond):
			}
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

		// Return early only when all target assets are found.
		if len(found) == len(assets) {
			return found
		}

		// Log progress every 5 seconds
		if time.Since(lastLogTime) >= 5*time.Second {
			tui.LogEvent("🔍 Waiting for new markets... (%ds)", attempts/2)
			lastLogTime = time.Now()
		}

		// Fast polling for first 30 seconds, then slow down
		sleepDuration := 500 * time.Millisecond
		if attempts >= maxFastAttempts {
			sleepDuration = 2 * time.Second
		}

		select {
		case <-ctx.Done():
			return found
		case <-time.After(sleepDuration):
		}
	}

	tui.LogEvent("⚠️ No 15m markets found after polling")
	return found
}

func getOutcomes(market *api.Market) []string {
	outcomes := make([]string, 0, len(market.Tokens))
	for _, token := range market.Tokens {
		outcomes = append(outcomes, token.Outcome)
	}
	// Sort outcomes for consistent ordering across API calls
	// This prevents token-to-price mapping bugs if API returns tokens in different order
	sort.Strings(outcomes)
	return outcomes
}

func tradeMarket(ctx context.Context, id string, market *api.Market, endTime time.Time,
	trader *trading.RealTrader, engine *paper.Engine, orderBook *paper.OrderBook,
	riskMgr *paper.RiskManager, tui *paper.TUI, restClient *api.RestClient, cfg *core.Config, startingBalance float64,
	globalSplitStatus map[string]bool, splitMu *sync.Mutex) {

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
		// Retry fee fetch a few times at startup
		var rate int
		var err error
		for attempt := 1; attempt <= 3; attempt++ {
			rate, err = restClient.GetFeeRate(ctx, tid)
			if err == nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}

		if err == nil {
			tokenFeeRates[outcome] = rate
			// 15m markets require 1000 bps authorization even if endpoint returns 0
			if rate == 0 {
				tokenFeeRates[outcome] = 1000
			} else {
				tui.LogEvent("[%s] ℹ️ Fee rate for %s: %.2f%% (%d bps)", id, outcome, float64(rate)/100.0, rate)
			}
		} else {
			// If API fails, use 1000 bps (10%) which is the standard taker fee for 15m markets
			tokenFeeRates[outcome] = 1000
			tui.LogEvent("[%s] ⚠️ Fee fetch failed, using default 1000 bps", id)
		}
	}

	tokenBids := make(map[string]float64)
	tokenAsks := make(map[string]float64)
	tokenFullBids := make(map[string][]paper.MarketLevel)
	tokenFullAsks := make(map[string][]paper.MarketLevel)
	lastUpdateTs := make(map[string]int64) // outcome -> unix nano timestamp
	lastUpdate := time.Now()
	lastRestPoll := time.Now()
	lastTrade := time.Time{}
	lastSplitSell := time.Time{}    // Track last split sell to avoid rapid-fire
	nextSplitAttempt := time.Time{} // Cooldown for retrying failed splits

	// Initial balance tracking
	currentBalance := startingBalance
	currentCash := startingBalance

	// Helper to get token ID from outcome
	getTokenID := func(outcome string) string {
		for tid, out := range tokenToOutcome {
			if out == outcome {
				return tid
			}
		}
		return ""
	}

	// ═══════════════════════════════════════════════════════════════════════════
	// SPLIT STRATEGY INITIALIZATION
	// Create split inventory tracker (separate from bought shares)
	// ═══════════════════════════════════════════════════════════════════════════
	splitInventory := paper.NewSplitInventory()
	engine.RegisterSplitInventory(splitInventory)   // Register for equity calculation
	tui.RegisterSplitInventory(splitInventory)      // Register for TUI display
	replenishCtrl := paper.NewReplenishController() // Debounce replenish goroutines
	var initialSplitAmount float64                  // Track initial split for replenishment target

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Check if market ended
		if time.Now().After(endTime.Add(5 * time.Second)) {
			tui.LogEvent("[%s] ⏰ Market ended", id)
			// Run redemption check in background so we can find next market immediately
			go checkRedemption(ctx, id, market.ConditionID, trader, engine, tui)
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
					// Channel closed - this only happens when context is cancelled
					// Check if we should exit or if it's a reconnection scenario
					select {
					case <-ctx.Done():
						tui.LogEvent("[%s] ⚠️ WS closed (context cancelled)", id)
						return
					default:
						// Context still active but channel closed unexpectedly
						// This shouldn't happen with the current WS manager, but handle it
						tui.LogEvent("[%s] ⚠️ WS channel closed unexpectedly, continuing with REST only", id)
						goto doneWS
					}
				}
				messagesProcessed++

				if books, err := api.ParseOrderBooks(msg); err == nil && len(books) > 0 {
					for _, b := range books {
						outcome := tokenToOutcome[b.AssetID]
						if outcome == "" {
							continue
						}

						// Parse timestamp for freshness check
						var msgTs int64
						if b.Timestamp != "" {
							// Try parsing as unix nano or micro string first
							if ts, err := strconv.ParseInt(b.Timestamp, 10, 64); err == nil {
								msgTs = ts
							} else if t, err := time.Parse(time.RFC3339Nano, b.Timestamp); err == nil {
								msgTs = t.UnixNano()
							}
						}

						// Only update if data is newer or same age
						if msgTs > 0 && msgTs < lastUpdateTs[outcome] {
							continue
						}
						if msgTs > 0 {
							lastUpdateTs[outcome] = msgTs
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
		// Poll REST every 4ms for high-frequency liquidity updates (~250 RPS per trader)
		// Global rate limiter in RestClient caps total at 500 RPS across all traders
		staleTime := time.Since(lastUpdate)
		restPollInterval := 4 * time.Millisecond
		needsRestPoll := time.Since(lastRestPoll) > restPollInterval

		// Update WS staleness and ping latency in TUI
		wsTimeSinceMsg := wsMgr.TimeSinceLastMessage()
		tui.UpdateWSLatency(wsTimeSinceMsg)
		tui.UpdateWSPingLatency(wsMgr.PingLatency())

		// Also force REST if WS is unhealthy
		wsUnhealthy := !wsMgr.IsConnected() || wsTimeSinceMsg > 10*time.Second
		if wsUnhealthy && staleTime > 3*time.Second {
			needsRestPoll = true
		}

		if needsRestPoll {
			lastRestPoll = time.Now()
			// Note: REST fallback updated to also capture full depth
			if handleRestFallbackWithDepth(ctx, id, tokenMap, tokenBids, tokenAsks, tokenFullBids, tokenFullAsks, lastUpdateTs, engine, restClient, tui) {
				lastUpdate = time.Now()
			}
		}

		// ============ TRADING LOGIC ============
		// Skip new trades if kill switch active, but keep monitoring (don't exit)
		if killSwitchActive {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		// ═══════════════════════════════════════════════════════════════════════════
		// SPLIT STRATEGY: Sell to panic buyers when bid_sum > $1.03
		// This is SEPARATE from the panic buy strategy (buy when ask_sum < $0.98)
		// Split shares are ONLY for selling, bought shares are ONLY for merging
		// ═══════════════════════════════════════════════════════════════════════════
		skipPanicBuy := false // Flag to skip panic buy when nearing expiry

		if cfg.SplitStrategyEnabled && len(tokenBids) >= 2 && len(outcomes) == 2 {
			bid1 := tokenBids[outcomes[0]]
			bid2 := tokenBids[outcomes[1]]

			// Check if we need to merge before expiry
			timeToExpiry := time.Until(endTime)
			mergeBuffer := time.Duration(cfg.SplitMergeBufferSeconds) * time.Second

			if timeToExpiry <= mergeBuffer && timeToExpiry > 0 {
				// MERGE ALL UNSOLD SPLIT SHARES before market expires
				availableShares := splitInventory.GetMinSplitShares(id, outcomes[0], outcomes[1])
				if availableShares >= 1.0 {
					tui.LogEvent("[%s] ⏰ SPLIT: Merging %.0f unsold shares before expiry", id, availableShares)

					mergeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
					txHash, err := trader.MergeOnChain(mergeCtx, market.ConditionID, availableShares)
					cancel()

					if err != nil {
						tui.LogEvent("[%s] ⚠️ SPLIT: Pre-expiry merge failed: %v", id, err)
					} else {
						merged := splitInventory.RecordMerge(id, outcomes[0], outcomes[1], availableShares)
						// Refresh balance after merge (tokens converted back to USDC)
						if newBal, err := trader.ForceRefreshBalance(ctx); err == nil {
							currentBalance = newBal
						}
						if txHash != "" && len(txHash) >= 10 {
							tui.LogEvent("[%s] 💰 SPLIT: Merged %.0f shares | Tx: %s...", id, merged, txHash[:10])
						} else {
							tui.LogEvent("[%s] 💰 SPLIT: Merged %.0f shares", id, merged)
						}
					}
				}
				// Don't do any more trading, let market expire
				skipPanicBuy = true
			}

			// Initial split: create inventory if not done yet
			// Move to BACKGROUND to prevent blocking the main trading loop
			splitMu.Lock()
			isSplit := globalSplitStatus[market.ConditionID]
			splitMu.Unlock()

			if !isSplit && time.Now().After(nextSplitAttempt) && replenishCtrl.MarkInProgress() {
				baseTradeSize := cfg.CalculateTradeSize(currentBalance)

				// Scale initial buffer based on balance: 2x trade size, but at least $2 and at most 25% of balance
				initialBuffer := baseTradeSize * 2.0
				if initialBuffer < 2.0 {
					initialBuffer = 2.0
				}

				maxInitial := currentBalance * 0.25
				splitAmount := initialBuffer
				if splitAmount > maxInitial {
					splitAmount = maxInitial
				}

				// Lower threshold to $1.0 to support testing with small balances (like $5)
				if splitAmount >= 1.0 {
					tui.LogEvent("[%s] 🔀 SPLIT: Creating inventory ($%.2f) in background...", id, splitAmount)

					go func(mID, condID, out0, out1 string, amt float64) {
						defer replenishCtrl.MarkComplete()
						// Increase timeout to 120s to be more resilient to Polygon congestion
						splitCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
						defer cancel()

						txHash, err := trader.SplitOnChain(splitCtx, condID, amt)
						if err != nil {
							tui.LogEvent("[%s] ⚠️ SPLIT: Background initial split failed: %v (will retry in 60s)", mID, err)
							// Set cooldown on failure to prevent RPC spam and nonce issues
							nextSplitAttempt = time.Now().Add(60 * time.Second)
						} else {
							// Update engine simulation immediately
							splitInventory.RecordSplit(mID, out0, out1, amt)
							engine.DeductBalance(amt)
							engine.RecalculateDrawdown()

							if txHash != "" && len(txHash) >= 10 {
								tui.LogEvent("[%s] ✅ SPLIT: Created %.0f shares | Tx: %s...", mID, amt, txHash[:10])
							} else {
								tui.LogEvent("[%s] ✅ SPLIT: Created %.0f shares", mID, amt)
							}

							// Only mark as initialized on SUCCESS (globally)
							splitMu.Lock()
							globalSplitStatus[condID] = true
							splitMu.Unlock()
							initialSplitAmount = amt
						}
					}(id, market.ConditionID, outcomes[0], outcomes[1], splitAmount)
				} else {
					// Not enough balance to split even $1
					replenishCtrl.MarkComplete()
					splitMu.Lock()
					if !globalSplitStatus[market.ConditionID] {
						tui.LogEvent("[%s] ⚠️ SPLIT: Balance too low for split ($%.2f < $1.00)", id, splitAmount)
						globalSplitStatus[market.ConditionID] = true // Mark true to stop spamming, even if skipped
					}
					splitMu.Unlock()
				}
			}

			// Check for panic sell opportunity: bid_sum > $1.00 + minMargin
			if bid1 > 0.10 && bid2 > 0.10 && bid1 < 0.90 && bid2 < 0.90 {
				bidSum := bid1 + bid2
				sellMargin := (bidSum - 1.0) * 100 // Profit margin from selling

				// BACKGROUND REPLENISHMENT
				baseTradeSize := cfg.CalculateTradeSize(currentBalance)
				targetBuffer := baseTradeSize * cfg.MaxAggressionMultiplier
				currentShares := splitInventory.GetMinSplitShares(id, outcomes[0], outcomes[1])
				replenishAmount := baseTradeSize * 2.0

				decision := replenishCtrl.CheckReplenish(paper.ReplenishParams{
					CurrentShares:      currentShares,
					TargetBuffer:       targetBuffer,
					InitialShares:      initialSplitAmount, // Replenish back to initial amount
					SellMargin:         sellMargin,
					MinMarginThreshold: cfg.SplitMinMarginSell - 1.0,
					CurrentBalance:     currentBalance,
					ReplenishAmount:    replenishAmount,
					MaxBalancePercent:  0.50,
				})

				if decision.ShouldReplenish && replenishCtrl.MarkInProgress() {
					tui.LogEvent("[%s] 🔄 SPLIT: Low inventory (%.0f/%.0f), replenishing +%.0f shares...", id, currentShares, initialSplitAmount, decision.Amount)
					go func(mID, condID, out0, out1 string, amt float64) {
						defer replenishCtrl.MarkComplete()
						// Use derived context for proper shutdown propagation
						bgCtx, bgCancel := context.WithTimeout(ctx, 60*time.Second)
						defer bgCancel()
						_, bgErr := trader.SplitOnChain(bgCtx, condID, amt)
						if bgErr == nil {
							// Update engine simulation immediately
							splitInventory.RecordSplit(mID, out0, out1, amt)
							engine.DeductBalance(amt)
							engine.RecalculateDrawdown()
							tui.LogEvent("[%s] ✅ SPLIT: Replenished to %.0f shares (+%.0f)", mID, initialSplitAmount, amt)
						} else {
							tui.LogEvent("[%s] ⚠️ SPLIT: Background replenish failed: %v", mID, bgErr)
						}
					}(id, market.ConditionID, outcomes[0], outcomes[1], decision.Amount)
				}

				if sellMargin >= cfg.SplitMinMarginSell && time.Since(lastSplitSell) > 2*time.Second {
					// DETERMINISTIC AGGRESSION
					requestedShares := baseTradeSize
					if cfg.EnableMarginAggression {
						multiplier := sellMargin / 2.0
						if multiplier > cfg.MaxAggressionMultiplier {
							multiplier = cfg.MaxAggressionMultiplier
						}
						if multiplier < 1.0 {
							multiplier = 1.0
						}
						requestedShares = baseTradeSize * multiplier
					}

					// GRACEFUL SELL: Sell what we have
					availableShares := splitInventory.GetMinSplitShares(id, outcomes[0], outcomes[1])
					sharesToSell := requestedShares
					if sharesToSell > availableShares {
						if availableShares >= 1.0 {
							tui.LogEvent("[%s] ⚠️ SPLIT: Capped sell at available inventory (%.0f/%.0f)", id, availableShares, requestedShares)
							sharesToSell = availableShares
						} else {
							sharesToSell = 0
						}
					}

					if sharesToSell >= 1.0 {
						// Risk limits and liquidity checks
						if sharesToSell > 250 {
							sharesToSell = 250 // Hard safety cap
						}

						// Check bid depth liquidity
						bidLiq1 := 0.0
						bidLiq2 := 0.0
						for _, lvl := range tokenFullBids[outcomes[0]] {
							bidLiq1 += lvl.Size
						}
						for _, lvl := range tokenFullBids[outcomes[1]] {
							bidLiq2 += lvl.Size
						}
						minBidLiq := bidLiq1
						if bidLiq2 < minBidLiq {
							minBidLiq = bidLiq2
						}

						// Only sell up to available liquidity
						if sharesToSell > minBidLiq*0.85 {
							sharesToSell = minBidLiq * 0.85
						}

						// Ensure min order size 1 share
						if sharesToSell < 1.0 {
							sharesToSell = 1.0
						}

						sharesToSell = math.Floor(sharesToSell)

						if sharesToSell >= 1.0 && sharesToSell <= availableShares {
							// Calculate liquidity depth for display (same as paper bot)
							bids1 := tokenFullBids[outcomes[0]]
							bids2 := tokenFullBids[outcomes[1]]
							bookDepth1, bookDepth2 := len(bids1), len(bids2)

							// Calculate matched liquidity across valid bid levels
							minSum := 1.0 + (cfg.SplitMinMarginSell / 100.0)
							var rawLiq1, rawLiq2 float64
							var maxValidI, maxValidJ int

							// Sort bids by price descending (best bids first)
							sortedBids1 := make([]paper.MarketLevel, len(bids1))
							copy(sortedBids1, bids1)
							sort.Slice(sortedBids1, func(a, b int) bool { return sortedBids1[a].Price > sortedBids1[b].Price })

							sortedBids2 := make([]paper.MarketLevel, len(bids2))
							copy(sortedBids2, bids2)
							sort.Slice(sortedBids2, func(a, b int) bool { return sortedBids2[a].Price > sortedBids2[b].Price })

							// Walk bid levels to find matched liquidity
							for bi, bj := 0, 0; bi < len(sortedBids1) && bj < len(sortedBids2); {
								if sortedBids1[bi].Price+sortedBids2[bj].Price < minSum {
									break
								}
								if bi+1 > maxValidI {
									maxValidI = bi + 1
									rawLiq1 += sortedBids1[bi].Size
								}
								if bj+1 > maxValidJ {
									maxValidJ = bj + 1
									rawLiq2 += sortedBids2[bj].Size
								}
								if sortedBids1[bi].Size <= sortedBids2[bj].Size {
									sortedBids2[bj].Size -= sortedBids1[bi].Size
									bi++
								} else {
									sortedBids1[bi].Size -= sortedBids2[bj].Size
									bj++
								}
							}

							// Enhanced log with liquidity and depth info (same format as paper bot)
							tui.LogEvent("[%s] 📈 SPLIT SELL! %s@$%.2f + %s@$%.2f = $%.3f (%.1f%%) | %.0f shares [liq: %.0f/%.0f, depth: %d/%d→%d/%d]",
								id, outcomes[0], bid1, outcomes[1], bid2, bidSum, sellMargin, sharesToSell,
								rawLiq1, rawLiq2, bookDepth1, bookDepth2, maxValidI, maxValidJ)

							// Sell both sides in parallel
							token0 := getTokenID(outcomes[0])
							token1 := getTokenID(outcomes[1])

							// Validate token IDs before trading
							if token0 == "" || token1 == "" {
								tui.LogEvent("[%s] ⚠️ SPLIT: Token ID not found for %s/%s", id, outcomes[0], outcomes[1])
								continue
							}

							var wg sync.WaitGroup
							wg.Add(2)

							var res1, res2 *trading.TradeResult
							var err1, err2 error

							// Use market orders for split selling with aggressive $0.10 floor
							// This ensures immediate fill against any available liquidity
							go func() {
								defer wg.Done()
								rate := tokenFeeRates[outcomes[0]]
								if rate == 0 {
									rate = 1000
								}
								res1, err1 = trader.Sell(ctx, token0, outcomes[0], 0.10, sharesToSell, api.OrderTypeMarket, api.TIFFillOrKill, rate)
							}()

							go func() {
								defer wg.Done()
								rate := tokenFeeRates[outcomes[1]]
								if rate == 0 {
									rate = 1000
								}
								res2, err2 = trader.Sell(ctx, token1, outcomes[1], 0.10, sharesToSell, api.OrderTypeMarket, api.TIFFillOrKill, rate)
							}()

							wg.Wait()

							side1Success := err1 == nil && res1 != nil && res1.Success
							side2Success := err2 == nil && res2 != nil && res2.Success

							// ═══════════════════════════════════════════════════════════════
							// UNBALANCED SELL RECOVERY: If one side succeeded and other failed,
							// retry the failed side up to 3 times to prevent unbalanced inventory
							// ═══════════════════════════════════════════════════════════════
							if side1Success != side2Success {
								failedSide := 1
								failedToken := token0
								failedOutcome := outcomes[0]
								if side1Success {
									failedSide = 2
									failedToken = token1
									failedOutcome = outcomes[1]
								}

								tui.LogEvent("[%s] ⚠️ SPLIT UNBALANCED: Side %d failed, starting bounded recovery...", id, failedSide)

								// Retry failed side with a bounded loop to avoid infinite single-side spam.
								retryCount := 0
								const maxSplitRecoveryAttempts = 40
								for retryCount < maxSplitRecoveryAttempts {
									retryCount++

									// Check context cancellation to allow graceful shutdown
									select {
									case <-ctx.Done():
										tui.LogEvent("[%s] 🛑 SPLIT Recovery interrupted by shutdown after %d attempts", id, retryCount)
										goto splitRecoveryDone
									default:
									}

									// Fast constant delay for balancing: 50ms
									time.Sleep(50 * time.Millisecond)

									// Force fill with floor price ($0.10) for split selling
									// Since it's a MARKET order, it will fill at the BEST available bid price
									// but providing $0.10 as the "price" gives enough room for any liquidity
									retryPrice := 0.10

									tui.LogEvent("[%s] 🔄 SPLIT Recovery #%d for %s (MARKET @ $0.10 floor)", id, retryCount, failedOutcome)

									rate := tokenFeeRates[failedOutcome]
									if rate == 0 {
										rate = 1000
									}
									retryRes, retryErr := trader.Sell(ctx, failedToken, failedOutcome, retryPrice, sharesToSell, api.OrderTypeMarket, api.TIFFillOrKill, rate)

									if retryErr == nil && retryRes != nil && retryRes.Success {
										tui.LogEvent("[%s] ✅ SPLIT Recovery SUCCESS for %s after %d attempts!", id, failedOutcome, retryCount)
										if failedSide == 1 {
											side1Success = true
											bid1 = retryPrice
										} else {
											side2Success = true
											bid2 = retryPrice
										}
										break
									}

									// Log failure with error details
									if retryErr != nil {
										tui.LogEvent("[%s] ⚠️ SPLIT Recovery #%d failed: %v", id, retryCount, retryErr)
									} else if retryRes != nil && !retryRes.Success {
										tui.LogEvent("[%s] ⚠️ SPLIT Recovery #%d failed: %s", id, retryCount, retryRes.Message)
										if strings.Contains(strings.ToLower(retryRes.Message), "invalid expiration value") {
											tui.LogEvent("[%s] 🛑 SPLIT Recovery halted early due to CLOB expiration validation error", id)
											break
										}
									}
								}

								if side1Success != side2Success {
									tui.LogEvent("[%s] 🛑 SPLIT Recovery exhausted (%d attempts), stopping retries to avoid spam", id, retryCount)
								}
							splitRecoveryDone:
							}

							if side1Success && side2Success {
								// Both sides sold - record in split inventory
								profit1 := splitInventory.RecordSell(id, outcomes[0], sharesToSell, bid1)
								profit2 := splitInventory.RecordSell(id, outcomes[1], sharesToSell, bid2)
								totalProfit := profit1 + profit2
								tui.LogEvent("[%s] ✅ SPLIT SOLD! Profit: +$%.2f", id, totalProfit)
								tui.RecordOrder(id, "SPLIT_SELL", "SELL", sharesToSell*2, (bid1+bid2)/2, sharesToSell*bidSum, sellMargin, "FILLED")
								// Refresh balance after successful sell (cash increased)
								if newBal, err := trader.ForceRefreshBalance(ctx); err == nil {
									currentBalance = newBal
								}
							} else {
								// Recovery failed - record partial success to keep inventory accurate
								if side1Success {
									splitInventory.RecordSell(id, outcomes[0], sharesToSell, bid1)
									tui.LogEvent("[%s] ⚠️ SPLIT: Only %s sold after recovery attempts", id, outcomes[0])
								}
								if side2Success {
									splitInventory.RecordSell(id, outcomes[1], sharesToSell, bid2)
									tui.LogEvent("[%s] ⚠️ SPLIT: Only %s sold after recovery attempts", id, outcomes[1])
								}
							}

							lastSplitSell = time.Now()
						}
					}
				}
			}
		}
		// ═══════════════════════════════════════════════════════════════════════════
		// PANIC BUY STRATEGY: Buy when ask_sum < $0.98, then merge for instant profit
		// These shares are SEPARATE from split shares - they go straight to merge
		// ═══════════════════════════════════════════════════════════════════════════
		if skipPanicBuy {
			continue
		}
		if len(tokenAsks) >= 2 && len(outcomes) == 2 {
			ask1 := tokenAsks[outcomes[0]]
			ask2 := tokenAsks[outcomes[1]]

			if ask1 >= 0.10 && ask1 <= 0.90 && ask2 >= 0.10 && ask2 <= 0.90 {
				// Slippage buffer: set to 0 to execute exactly at configured MinMarginPercent
				const slippageBuffer = 0.0
				sum := ask1 + ask2
				bufferedSum := (ask1 + slippageBuffer) + (ask2 + slippageBuffer)
				margin := (1.0 - bufferedSum) * 100 // Use buffered sum for margin check

				if margin >= cfg.MinMarginPercent {
					// Evaluate risk
					riskAction, riskReason := riskMgr.Evaluate()
					if riskAction == paper.RiskActionKillSwitch {
						tui.LogEvent("[%s] 🛑 RISK: Kill switch - %s (pausing, not exiting)", id, riskReason)
						continue
					}

					// Dynamic trade size based on EQUITY (not just cash)
					// This ensures consistent sizing regardless of how much is in positions
					latestBalance, _ := trader.GetBalance(ctx)
					if latestBalance > 0 {
						currentCash = latestBalance
						currentBalance = latestBalance
					}

					// For real bot, equity = cash + market value of positions
					// Simplification: use cash balance as proxy for sizing, or fetch equity
					currentEquity := currentBalance // In realbot we use cash as conservative equity
					tradeSize := cfg.CalculateTradeSize(currentEquity)
					// Scale shares based on margin
					shares := tradeSize / sum

					// Apply aggression scaling based on margin (e.g., 2% = 2x, 3% = 3x)
					// Higher margin = better opportunity = more aggressive sizing
					if cfg.EnableMarginAggression && riskAction != paper.RiskActionReduceSize {
						multiplier := math.Floor(margin)
						if multiplier > cfg.MaxAggressionMultiplier {
							multiplier = cfg.MaxAggressionMultiplier
						}
						if multiplier < 1 {
							multiplier = 1
						}
						shares *= multiplier
					}

					// Round to whole shares
					shares = math.Floor(shares)

					// Ensure min order size $1.00 at worstCasePrice 0.99
					if shares*0.99 < 1.0 {
						shares = math.Ceil(1.0 / 0.99)
					}

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

						if i+1 > maxValidI {
							maxValidI = i + 1
							rawLiq1 += asks1[i].Size
						}
						if j+1 > maxValidJ {
							maxValidJ = j + 1
							rawLiq2 += asks2[j].Size
						}
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

					// Calculate metrics for reporting
					cost, _, _, _ := calculateTradeMetrics(shares, sum, maxFeeRateBps)

					// REFRESH BALANCE RIGHT BEFORE TRADING to prevent unbalanced fills
					// This is the "Zero Excuse" check to ensure we have enough for BOTH sides
					latestBalance, balErr := trader.GetBalance(ctx)
					if balErr == nil {
						currentBalance = latestBalance
						currentCash = latestBalance
					}

					// Scale down if cost exceeds cash (add 2% buffer for price movements)
					costWithBuffer := cost * 1.02
					if !riskMgr.CanPlaceOrder(cost) || costWithBuffer > currentCash {
						if cost > currentCash {
							maxAffordableShares := (currentCash * 0.98) / sum // Use 98% of cash for safety
							if maxAffordableShares < 1.0 {
								tui.LogEvent("[%s] ⚠️ Insufficient funds: Need $%.2f for 1 share, have $%.2f", id, sum, currentCash)
								continue
							}
							shares = math.Floor(maxAffordableShares)
							cost, _, _, _ = calculateTradeMetrics(shares, sum, maxFeeRateBps)
						}
					}

					// MARKET orders are submitted with a $0.99 cap per side.
					// Budget check must use worst-case spend across BOTH sides to avoid recovery failures
					// like "not enough balance / allowance" after one side fills first.
					const worstCaseExecutionPrice = 0.99
					worstCaseDualCost := shares * worstCaseExecutionPrice * 2.0
					if worstCaseDualCost > currentCash {
						maxAffordableWorstCase := math.Floor((currentCash * 0.98) / (worstCaseExecutionPrice * 2.0))
						if maxAffordableWorstCase < 1.0 {
							tui.LogEvent("[%s] ⚠️ Insufficient worst-case balance for dual market buy: Need $%.2f for 1x1 share, Have $%.2f", id, worstCaseExecutionPrice*2.0, currentCash)
							continue
						}
						shares = maxAffordableWorstCase
						cost, _, _, _ = calculateTradeMetrics(shares, sum, maxFeeRateBps)
					}

					// FINAL SAFETY CHECK: If we still don't have enough for both sides, SKIP
					if cost > currentCash || (shares*worstCaseExecutionPrice*2.0) > currentCash {
						tui.LogEvent("[%s] ⚠️ Insufficient balance for both sides: Need est $%.2f (worst-case $%.2f), Have $%.2f", id, cost, shares*worstCaseExecutionPrice*2.0, currentCash)
						continue
					}

					// Check why we might skip trading
					if shares < 1.0 {
						tui.LogEvent("[%s] ⚠️ Shares below 1.0 minimum: %.2f", id, shares)
						continue
					}
					if time.Since(lastTrade) <= 2*time.Second {
						// Cooldown - don't spam logs, just skip silently
						continue
					}

					if true { // Always execute if we got here
						// Use buffered prices (slippageBuffer already defined above)
						price1 := ask1 + slippageBuffer
						price2 := ask2 + slippageBuffer

						// Calculate trade metrics with buffered prices
						_, _, _, _ = calculateTradeMetrics(shares, bufferedSum, maxFeeRateBps)

						tui.LogEvent("[%s] 🎯 ARB! %s@$%.3f + %s@$%.3f = $%.3f (%.1f%% margin, %.1f%% after slippage) [liq: %.0f/%.0f, depth: %d/%d→%d/%d]",
							id, outcomes[0], ask1, outcomes[1], ask2, sum, (1.0-sum)*100, margin, liq1, liq2, bookDepth1, bookDepth2, maxValidI, maxValidJ)

						// Map tokens
						token0, token1 := "", ""
						for tid, out := range tokenToOutcome {
							if out == outcomes[0] {
								token0 = tid
							} else if out == outcomes[1] {
								token1 = tid
							}
						}

						// If we're already unbalanced in this market, prioritize closing the gap.
						pos := engine.GetPositions()
						pos1Qty := 0.0
						pos2Qty := 0.0
						if p, ok := pos[id+":"+outcomes[0]]; ok {
							pos1Qty = p.Quantity
						}
						if p, ok := pos[id+":"+outcomes[1]]; ok {
							pos2Qty = p.Quantity
						}
						const rebalanceEpsilon = 0.000001
						onlyRebalance := math.Abs(pos1Qty-pos2Qty) > rebalanceEpsilon
						onlyOutcome := ""
						if onlyRebalance {
							if pos1Qty < pos2Qty {
								onlyOutcome = outcomes[0]
							} else {
								onlyOutcome = outcomes[1]
							}
							tui.LogEvent("[%s] ⚖️ Existing imbalance detected (%s=%.0f, %s=%.0f) → rebalancing only %s", id, outcomes[0], pos1Qty, outcomes[1], pos2Qty, onlyOutcome)
						}

						tradeShares := shares
						if onlyRebalance {
							gap := math.Abs(pos1Qty - pos2Qty)
							if gap > rebalanceEpsilon && tradeShares > gap {
								tradeShares = gap
							}
						}

						if tradeShares < 1.0 {
							tui.LogEvent("[%s] ℹ️ Rebalance gap %.4f too small for minimum order size, waiting", id, tradeShares)
							continue
						}

						// MARKET EXECUTION: Force fill with cap price so CLOB can cross the book.
						const worstCasePrice = 0.99

						var wg sync.WaitGroup
						wg.Add(2)

						var res1, res2 *trading.TradeResult
						var err1, err2 error

						go func() {
							defer wg.Done()
							if onlyRebalance && onlyOutcome != outcomes[0] {
								err1 = fmt.Errorf("rebalance mode: skipped heavy side %s", outcomes[0])
								return
							}
							rate := tokenFeeRates[outcomes[0]]
							if rate == 0 {
								rate = 1000
							}
							res1, err1 = trader.Buy(ctx, token0, outcomes[0], worstCasePrice, tradeShares, api.OrderTypeMarket, api.TIFFillOrKill, rate)
						}()

						go func() {
							defer wg.Done()
							if onlyRebalance && onlyOutcome != outcomes[1] {
								err2 = fmt.Errorf("rebalance mode: skipped heavy side %s", outcomes[1])
								return
							}
							rate := tokenFeeRates[outcomes[1]]
							if rate == 0 {
								rate = 1000
							}
							res2, err2 = trader.Buy(ctx, token1, outcomes[1], worstCasePrice, tradeShares, api.OrderTypeMarket, api.TIFFillOrKill, rate)
						}()

						wg.Wait()

						// Calculate costs using the original target price for reporting (actual will be better)
						cost1 := tradeShares * price1
						cost2 := tradeShares * price2

						// Track success state - defer engine recording until final state is known
						side1Success := err1 == nil && res1 != nil && res1.Success
						side2Success := err2 == nil && res2 != nil && res2.Success

						// Log results (but don't record to engine yet)
						if side1Success {
							tui.LogEvent("[%s] ✅ Side 1 MARKET: %s (Target $%.3f)", id, outcomes[0], price1)
							tui.RecordOrder(id, outcomes[0], "BUY", tradeShares, price1, cost1, margin, "FILLED")
						} else {
							if err1 != nil {
								tui.LogEvent("[%s] ❌ Side 1 MARKET Fail: %v", id, err1)
							} else if res1 != nil && res1.Message != "" {
								tui.LogEvent("[%s] ❌ Side 1 MARKET Fail: %s", id, res1.Message)
							} else if res1 == nil {
								tui.LogEvent("[%s] ❌ Side 1 MARKET Fail: nil response", id)
							} else {
								tui.LogEvent("[%s] ❌ Side 1 MARKET Fail: unknown error (res=%v)", id, res1)
							}
							tui.RecordOrder(id, outcomes[0], "BUY", tradeShares, price1, cost1, margin, "FAILED")
						}

						if side2Success {
							tui.LogEvent("[%s] ✅ Side 2 MARKET: %s (Target $%.3f)", id, outcomes[1], price2)
							tui.RecordOrder(id, outcomes[1], "BUY", tradeShares, price2, cost2, margin, "FILLED")
						} else {
							if err2 != nil {
								tui.LogEvent("[%s] ❌ Side 2 MARKET Fail: %v", id, err2)
							} else if res2 != nil && res2.Message != "" {
								tui.LogEvent("[%s] ❌ Side 2 MARKET Fail: %s", id, res2.Message)
							} else if res2 == nil {
								tui.LogEvent("[%s] ❌ Side 2 MARKET Fail: nil response", id)
							} else {
								tui.LogEvent("[%s] ❌ Side 2 MARKET Fail: unknown error (res=%v)", id, res2)
							}
							tui.RecordOrder(id, outcomes[1], "BUY", tradeShares, price2, cost2, margin, "FAILED")
						}

						recoverySuccess := false
						if !onlyRebalance && side1Success != side2Success {
							failedSide := 1
							failedToken := token0
							failedOutcome := outcomes[0]
							if side1Success {
								failedSide = 2
								failedToken = token1
								failedOutcome = outcomes[1]
							}

							tui.LogEvent("[%s] ⚠️ ARB UNBALANCED: Side %d failed, starting bounded recovery...", id, failedSide)

							retryCount := 0
							const maxARBRecoveryAttempts = 40
							for retryCount < maxARBRecoveryAttempts {
								retryCount++

								select {
								case <-ctx.Done():
									tui.LogEvent("[%s] 🛑 ARB Recovery interrupted by shutdown after %d attempts", id, retryCount)
									goto arbRecoveryDone
								default:
								}

								time.Sleep(50 * time.Millisecond)
								retryPrice := 0.99
								tui.LogEvent("[%s] 🔄 ARB Recovery #%d for %s (MARKET @ $0.99 cap)", id, retryCount, failedOutcome)

								rate := tokenFeeRates[failedOutcome]
								if rate == 0 {
									rate = 1000
								}

								retryShares := tradeShares
								if onlyRebalance {
									gap := math.Abs(pos1Qty - pos2Qty)
									if gap > rebalanceEpsilon && retryShares > gap {
										retryShares = gap
									}
								}

								// Cap retry size by currently available cash to avoid allowance/balance spam.
								if latestBal, balErr := trader.GetBalance(ctx); balErr == nil {
									maxRetryShares := math.Floor((latestBal * 0.98) / retryPrice)
									if maxRetryShares < 1.0 {
										tui.LogEvent("[%s] 🛑 ARB Recovery halted: insufficient balance for retry (bal=$%.2f)", id, latestBal)
										break
									}
									if retryShares > maxRetryShares {
										retryShares = maxRetryShares
									}
								}

								retryRes, retryErr := trader.Buy(ctx, failedToken, failedOutcome, retryPrice, retryShares, api.OrderTypeMarket, api.TIFFillOrKill, rate)

								if retryErr == nil && retryRes != nil && retryRes.Success {
									tui.LogEvent("[%s] ✅ ARB Recovery SUCCESS for %s after %d attempts!", id, failedOutcome, retryCount)
									retryCost := retryShares * retryPrice
									tui.RecordOrder(id, failedOutcome, "BUY", retryShares, retryPrice, retryCost, margin, "FILLED")
									recoverySuccess = true
									if failedSide == 1 {
										side1Success = true
										price1 = retryPrice
									} else {
										side2Success = true
										price2 = retryPrice
									}
									tradeShares = retryShares
									break
								}

								if retryErr != nil {
									tui.LogEvent("[%s] ⚠️ ARB Recovery #%d failed: %v", id, retryCount, retryErr)
								} else if retryRes != nil && !retryRes.Success {
									tui.LogEvent("[%s] ⚠️ ARB Recovery #%d failed: %s", id, retryCount, retryRes.Message)
									if strings.Contains(strings.ToLower(retryRes.Message), "invalid expiration value") {
										tui.LogEvent("[%s] 🛑 ARB Recovery halted early due to CLOB expiration validation error", id)
										break
									}
								}
							}

							if !recoverySuccess {
								tui.LogEvent("[%s] 🛑 ARB Recovery exhausted (%d attempts), stopping retries to avoid single-side spam", id, retryCount)
							}
						arbRecoveryDone:
						}

						// NOW record to engine - only record positions that actually succeeded
						// This ensures engine state matches reality for accurate drawdown calculation
						if side1Success && side2Success {
							// Both sides filled (either initially or via recovery) - record both
							engine.BuyForMarket(id, outcomes[0], price1, tradeShares)
							engine.BuyForMarket(id, outcomes[1], price2, tradeShares)

							// INSTANT MERGE: Immediately merge tokens to capture arb profit
							// This converts YES+NO tokens back to USDC without waiting for expiry
							go func(cid string, qty float64, o1, o2, tid0, tid1 string) {
								// Small delay to let on-chain state settle after fills
								time.Sleep(3 * time.Second)

								mergeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
								defer cancel()

								// Query actual on-chain balances to merge the correct amount
								mergeQty := qty
								bal0, err0 := trader.GetCTFBalanceFloat(mergeCtx, tid0)
								bal1, err1 := trader.GetCTFBalanceFloat(mergeCtx, tid1)
								if err0 == nil && err1 == nil {
									// Don't floor! Merge exact fractional amount available
									actualMin := math.Min(bal0, bal1)
									// Filter dust
									if actualMin >= 0.000001 {
										mergeQty = actualMin
										tui.LogEvent("[%s] 📊 On-chain balances: %s=%.2f, %s=%.2f → merging %.6f", id, o1, bal0, o2, bal1, mergeQty)
									}
								}

								txHash, err := trader.MergeOnChain(mergeCtx, cid, mergeQty)
								if err != nil {
									tui.LogEvent("[%s] ⚠️ Merge failed: %v (will redeem at expiry)", id, err)
									// Fallback: positions remain, will be redeemed at expiry via checkRedemption
								} else {
									// Update engine to reflect closed position
									result := engine.MergeForMarket(id, o1, o2, mergeQty)
									// Note: Balance will be refreshed on next trade cycle via GetBalance
									// We can't update currentBalance here as it's not in scope
									if txHash != "" && len(txHash) >= 10 {
										tui.LogEvent("[%s] 💰 MERGED! +$%.2f profit | Tx: %s...", id, result.PnL, txHash[:10])
									} else {
										tui.LogEvent("[%s] 💰 MERGED! +$%.2f profit", id, result.PnL)
									}
								}
							}(market.ConditionID, tradeShares, outcomes[0], outcomes[1], token0, token1)
						} else if side1Success || side2Success {
							// Only one side filled and recovery failed - record the unbalanced position
							// This is important so the risk manager can see the exposure
							if side1Success {
								engine.BuyForMarket(id, outcomes[0], price1, tradeShares)
								tui.LogEvent("[%s] ⚠️ Engine: Recording unbalanced position (only %s)", id, outcomes[0])
							}
							if side2Success {
								engine.BuyForMarket(id, outcomes[1], price2, tradeShares)
								tui.LogEvent("[%s] ⚠️ Engine: Recording unbalanced position (only %s)", id, outcomes[1])
							}
						}
						// If both failed, nothing to record
						_ = recoverySuccess // Used above to update success flags

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

func handleRestFallbackWithDepth(ctx context.Context, id string, tokenMap map[string]string, bids, asks map[string]float64, fullBids, fullAsks map[string][]paper.MarketLevel, lastUpdateTs map[string]int64, engine *paper.Engine, restClient *api.RestClient, tui *paper.TUI) bool {
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

		// Parse timestamp for freshness check
		var msgTs int64
		if book.Timestamp != "" {
			if ts, err := strconv.ParseInt(book.Timestamp, 10, 64); err == nil {
				msgTs = ts
			} else if t, err := time.Parse(time.RFC3339Nano, book.Timestamp); err == nil {
				msgTs = t.UnixNano()
			}
		}

		// FRESHNESS CHECK: Only update prices if this REST data is newer than what we have
		isNewer := msgTs >= lastUpdateTs[outcome]
		if msgTs > 0 && isNewer {
			lastUpdateTs[outcome] = msgTs
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

		// Update prices only if newer
		if isNewer && (bid > 0 || (ask > 0 && ask < 1.0)) {
			bids[outcome] = bid
			asks[outcome] = ask
			if bid > 0 && ask > 0 && ask < 1.0 {
				mid := (bid + ask) / 2
				engine.UpdateMarketData(id, outcome, mid, bid, ask)
			}
			success = true
		}

		// ALWAYS update full depth (liquidity), as REST is our primary source for this
		// and stale liquidity is better than no liquidity for safety checks
		fullBids[outcome] = toMarketLevels(tui, id, book.Bids)
		fullAsks[outcome] = toMarketLevels(tui, id, book.Asks)
	}
	if success {
		tui.UpdateMarketPricesWithSource(id, bids, asks, "REST")
	}
	return success
}

func checkRedemption(ctx context.Context, id, conditionID string, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI) {
	// Check if we have any positions to redeem first
	// If merge already happened, we have no positions and can skip redemption entirely
	positions := engine.GetPositions()
	hasPositions := false
	for _, pos := range positions {
		if pos.Quantity > 0 {
			hasPositions = true
			break
		}
	}

	if !hasPositions {
		tui.LogEvent("[%s] ✅ No positions to redeem (already merged)", id)
		return
	}

	// Retry resolution check with exponential backoff
	// 5-min markets may take a few seconds to resolve
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
				go func(cid string) {
					tui.LogEvent("[%s] ⏳ Starting on-chain redemption...", id)
					// Wait a bit for on-chain state to sync
					time.Sleep(30 * time.Second)
					// Use fresh context since parent ctx may be cancelled during shutdown
					redeemCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
					defer cancel()
					txHash, err := trader.RedeemOnChain(redeemCtx, cid)
					if err != nil {
						tui.LogEvent("[%s] ⚠️ On-chain redeem pending: %v", id, err)
					} else if len(txHash) >= 10 {
						tui.LogEvent("[%s] ✅ REDEEMED! Tx: %s", id, txHash[:10]+"...")
					} else {
						tui.LogEvent("[%s] ✅ REDEEMED! Tx: %s", id, txHash)
					}
				}(conditionID)
			} else {
				tui.LogEvent("[%s] 📭 Market resolved: %s (no positions)", id, winner)
			}
			return
		}

		tui.LogEvent("[%s] ⏳ Resolution pending... (attempt %d/%d)", id, attempt+1, len(retryDelays))
	}

	tui.LogEvent("[%s] ⚠️ Could not get resolution after %d attempts", id, len(retryDelays))
}
