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
	mkt "Market-bot/internal/markets"
	"Market-bot/internal/paper"
	"Market-bot/internal/setup"
	"Market-bot/internal/strategy"
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

	// Create real trader and auto-setup credentials/allowances if missing
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute) // Increased timeout for setup
	realTrader, err := setup.EnsureRealTradingSetup(ctx, cfg)
	if err != nil {
		cancel()
		return fmt.Errorf("failed to setup or create trader: %w", err)
	}

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
		fmt.Println("║  Type 'on' to start with Split Strategy ENABLED       ║")
		fmt.Println("║  Type 'off' to start with Split Strategy DISABLED     ║")
		fmt.Println("╚═══════════════════════════════════════════════════════╝")
		fmt.Print("> ")

		reader := bufio.NewReader(os.Stdin)
		input, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("failed to read input: %w", err)
		}

		input = strings.TrimSpace(strings.ToLower(input))
		if input == "on" {
			cfg.SplitStrategyEnabled = true
			_ = cfg.SaveSettings()
			fmt.Println("✅ Starting real trading bot with Split Strategy ON...")
		} else if input == "off" {
			cfg.SplitStrategyEnabled = false
			_ = cfg.SaveSettings()
			fmt.Println("✅ Starting real trading bot with Split Strategy OFF...")
		} else {
			fmt.Println("❌ Cancelled (must type 'on' or 'off')")
			return nil
		}
	}

	// Setup signal handling
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	restClient := api.NewRestClient("")

	// emergencyCleanup ensures we don't leave hanging orders or unmerged positions
	emergencyCleanup := func() {
		// Give the overall cleanup up to 45 seconds, but each merge gets its own context
		overallCtx, cancelAll := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancelAll()

		fmt.Println("\n🧹 Running emergency cleanup...")

		// 1. Cancel all open orders
		if err := realTrader.CancelAll(overallCtx); err != nil {
			fmt.Printf("⚠️  Failed to cancel orders: %v\n", err)
		} else {
			fmt.Println("✅ All orders cancelled")
		}

		// 2. Identify and merge balanced positions
		positions, err := realTrader.GetPositions(overallCtx)
		if err != nil {
			fmt.Printf("⚠️  Could not fetch positions for merge: %v\n", err)
		} else if len(positions) > 0 {
			// Map positions to their markets to find ConditionIDs
			markets, err := restClient.GetMarketsByTimeframe(overallCtx, nil, "15m")
			if err == nil {
				// Group tokens by ConditionID
				condToTokens := make(map[string][]string)
				for _, m := range markets {
					for _, t := range m.Tokens {
						condToTokens[m.ConditionID] = append(condToTokens[m.ConditionID], t.TokenID)
					}
				}

				var wg sync.WaitGroup
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
						wg.Add(1)
						go func(cID string, tks []string, mq float64) {
							defer wg.Done()
							fmt.Printf("💰 Merging %.6f pairs for market %s...\n", mq, cID[:10])
							// Independent 30s timeout per merge
							mergeCtx, mergeCancel := context.WithTimeout(context.Background(), 30*time.Second)
							defer mergeCancel()

							_, err := realTrader.MergeOnChain(mergeCtx, cID, mq, len(tks))
							if err != nil {
								fmt.Printf("❌ Merge failed for %s: %v\n", cID[:10], err)
							} else {
								fmt.Printf("✅ Merge successful for %s\n", cID[:10])
							}
						}(condID, tokens, minQty)
					}
				}

				// Wait for all concurrent merges to finish or overall timeout
				done := make(chan struct{})
				go func() {
					wg.Wait()
					close(done)
				}()

				select {
				case <-done:
					fmt.Println("✅ All emergency merges completed")
				case <-overallCtx.Done():
					fmt.Println("⚠️ Emergency cleanup timed out waiting for some merges")
				}
			}
		}
	}

	// Panic recovery
	defer func() {
		if r := recover(); r != nil {
			core.RestoreTerminal()
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
		// If we receive a second interrupt during cleanup, force exit.
		// Use a separate signal channel since ctx is already cancelled.
		forceCh := make(chan os.Signal, 1)
		signal.Notify(forceCh, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-forceCh
			core.RestoreTerminal()
			fmt.Println("\n⚠️ Force exit requested")
			os.Exit(1)
		}()

		time.Sleep(10 * time.Second) // Give cleanup more time
		core.RestoreTerminal()
		fmt.Println("\n⚠️ Force exit: cleanup timed out")
		os.Exit(1)
	}()

	// Disable terminal echo
	disableEcho := exec.Command("stty", "-echo", "-icanon")
	disableEcho.Stdin = os.Stdin
	_ = disableEcho.Run()
	defer core.RestoreTerminal()

	engine := paper.NewEngine(balance)
	orderBook := paper.NewOrderBook()
	tui := paper.NewTUI(engine, orderBook)
	tui.SetMode("Real") // Show "Real Trading Mode" in footer (not "Paper Trading Mode")

	// Seed settings panel with values from config (.env)
	tui.InitSettings(paper.TUISettings{
		MarketSlug:           cfg.MarketSlug,
		MaxMarkets:           cfg.MaxMarkets,
		Timeframe:            cfg.Timeframe,
		TradeScaleFactor:     cfg.TradeScaleFactor,
		MinMarginPercent:     cfg.MinMarginPercent,
		SplitMinMarginSell:   cfg.SplitMinMarginSell,
		SplitStrategyEnabled: cfg.SplitStrategyEnabled,
		SplitInitialCapPct:   cfg.SplitInitialCapPct,
		SplitReplenishCapPct: cfg.SplitReplenishCapPct,
		MinAskPrice:          cfg.MinAskPrice,
		MaxAskPrice:          cfg.MaxAskPrice,
	}, func(s paper.TUISettings) {
		cfg.MarketSlug = s.MarketSlug
		cfg.MaxMarkets = s.MaxMarkets
		cfg.Timeframe = s.Timeframe
		cfg.TradeScaleFactor = s.TradeScaleFactor
		cfg.MinMarginPercent = s.MinMarginPercent
		cfg.SplitMinMarginSell = s.SplitMinMarginSell
		cfg.SplitStrategyEnabled = s.SplitStrategyEnabled
		cfg.SplitInitialCapPct = s.SplitInitialCapPct
		cfg.SplitReplenishCapPct = s.SplitReplenishCapPct
		cfg.MinAskPrice = s.MinAskPrice
		cfg.MaxAskPrice = s.MaxAskPrice
		_ = cfg.SaveSettings()
	})
	tui.SetTradeFactor(cfg.TradeScaleFactor)

	// Start TUI — pass stop so a single Ctrl+C / [q] quits cleanly.
	if UseLiveUI {
		tui.StartRenderLoop(250*time.Millisecond, stop)
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
				_, err := restClient.GetMarketsByTimeframe(pingCtx, []string{"btc"}, "15m")
				cancel()
				if err == nil {
					tui.UpdateLatency(time.Since(start))
				}
			}
		}
	}()

	// Main trading loop - Keep running: after each round of markets ends, search for new ones.
	globalSplitStatus := make(map[string]bool)
	globalSplitInventories := make(map[string]*paper.SplitInventory)
	globalInitialSplits := make(map[string]float64)
	var splitMu sync.Mutex
	var splitTxMu sync.Mutex
	currentBalance := balance // Seed with the pre-fetched balance

	for {
		// Check for shutdown signal before starting a new round
		select {
		case <-ctx.Done():
			goto shutdown
		default:
		}

		// Refresh balance at the start of each round for compounding
		{
			balCtx, balFn := context.WithTimeout(ctx, 10*time.Second)
			newBal, balErr := realTrader.GetBalance(balCtx)
			balFn()
			if balErr != nil {
				tui.LogEvent("⚠️ Could not refresh balance: %v", balErr)
				// keep currentBalance from last known value
			} else {
				currentBalance = newBal
				engine.SetBalance(currentBalance)
				engine.RecalculateDrawdown()
			}
		}

		// Track starting equity for this round's PnL calculation
		startingEquity := engine.GetEquity()
		compoundMultiplier := engine.GetCompoundMultiplier()
		tui.LogEvent("📊 Round starting | Balance: $%.2f | Multiplier: %.2fx", currentBalance, compoundMultiplier)

		// Find markets
		tui.LogEvent("🔍 Searching for active markets based on live settings...")
		markets := mkt.FindMarkets(ctx, restClient, tui.GetSettings, func(format string, args ...interface{}) {
			tui.LogEvent(format, args...)
		})
		if len(markets) == 0 {
			tui.LogEvent("⏳ No active markets found, waiting 30s before retry...")
			select {
			case <-time.After(30 * time.Second):
				continue // loop back and search again
			case <-ctx.Done():
				goto shutdown
			}
		}

		// Create a context for this specific round of trading
		roundCtx, roundCancel := context.WithCancel(ctx)

		// Trade each market in parallel
		var wg sync.WaitGroup
		for assetID, market := range markets {
			endTime, _ := paper.ParseEndTimeFromSlug(market.Slug)
			outcomes := mkt.GetOutcomes(market)
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
				// Create a sub-context for this specific trader to prevent goroutine leaks
				tCtx, tCancel := context.WithCancel(roundCtx)
				defer tCancel()

				defer func() {
					if r := recover(); r != nil {
						core.RestoreTerminal()
						stack := make([]byte, 4096)
						length := runtime.Stack(stack, false)
						fmt.Printf("\n🚨 TRADER PANIC [%s]: %v\n%s\n", id, r, stack[:length])
						emergencyCleanup()
					}
				}()
				tradeMarket(ctx, tCtx, id, m, end, realTrader, engine, orderBook, r, tui, restClient, cfg, bal, globalSplitStatus, globalSplitInventories, globalInitialSplits, &splitMu, &splitTxMu)
			}(assetID, market, endTime, marketRiskMgr, currentBalance)
		}

		// Goroutine to monitor for TUI restart requests
		go func() {
			ticker := time.NewTicker(1 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-roundCtx.Done():
					return
				case <-ticker.C:
					if tui.GetAndClearRestart() {
						tui.LogEvent("🔄 Settings saved. Restarting trading loop...")
						roundCancel() // This cancels the roundCtx, stopping all current traders
						return
					}
				}
			}
		}()

		// Wait for all markets in this round to finish
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
			tui.LogEvent("✅ All markets closed normally.")
		case <-ctx.Done():
			goto shutdown
		case <-roundCtx.Done():
			// Round cancelled (e.g. via settings restart)
			tui.LogEvent("⚠️ Traders stopped for restart...")
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
		}

		// Ensure round is cancelled even if it finished normally
		roundCancel()

		// Sync engine with on-chain balance before calculating round PnL
		{
			endBalCtx, endBalFn := context.WithTimeout(ctx, 10*time.Second)
			if endBal, endBalErr := realTrader.GetBalance(endBalCtx); endBalErr == nil {
				engine.SetBalance(endBal)
				engine.RecalculateDrawdown()
			}
			endBalFn()
		}

		// Calculate round PnL
		roundPnL := engine.GetEquity() - startingEquity
		if roundPnL > 0 {
			tui.LogEvent("📈 PROFIT! Round PnL: +$%.2f", roundPnL)
		} else if roundPnL < 0 {
			tui.LogEvent("📉 Loss. Round PnL: $%.2f", roundPnL)
		} else {
			tui.LogEvent("✅ Round complete, no change")
		}
		tui.LogEvent("🔄 Searching for next round...")

		// Release stale keep-alive connections before the next search phase.
		restClient.CloseIdleConnections()
		tui.ClearMarkets()
		orderBook.CancelAllOrders()
		engine.ClearMarketData()
	}

shutdown:
	tui.Stop()
	fmt.Println("\n👋 Bot stopped.")
	emergencyCleanup()
	return nil
}

func tradeMarket(globalCtx context.Context, ctx context.Context, id string, market *api.Market, endTime time.Time,
	trader *trading.RealTrader, engine *paper.Engine, orderBook *paper.OrderBook,
	riskMgr *paper.RiskManager, tui *paper.TUI, restClient *api.RestClient, cfg *core.Config, startingBalance float64,
	globalSplitStatus map[string]bool, globalSplitInventories map[string]*paper.SplitInventory, globalInitialSplits map[string]float64, splitMu *sync.Mutex, splitTxMu *sync.Mutex) {

	tokenMap := make(map[string]string)
	tokenToOutcome := make(map[string]string)
	for _, token := range market.Tokens {
		tokenMap[token.TokenID] = token.Outcome
		tokenToOutcome[token.TokenID] = token.Outcome
	}

	outcomes := mkt.GetOutcomes(market)

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
	lastUpdate := time.Now()
	lastTrade := time.Time{}
	lastSplitSell := time.Time{}    // Track last split sell to avoid rapid-fire
	nextSplitAttempt := time.Time{} // Cooldown for retrying failed splits
	var panicBuyCooldown time.Time  // Cooldown for panic buys after successful auto-cleanup

	// Initial balance tracking
	currentBalance := startingBalance
	// currentCash := startingBalance // Unused after removing balance checks

	// Background ticker to keep balance and allowance fresh without blocking WS loop
	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				bgCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
				_ = trader.UpdateBalanceAllowance(bgCtx)
				_, _ = trader.ForceRefreshBalance(bgCtx)
				cancel()
			}
		}
	}()

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
	splitMu.Lock()
	splitInventory, exists := globalSplitInventories[market.ConditionID]
	if !exists {
		splitInventory = paper.NewSplitInventory()
		globalSplitInventories[market.ConditionID] = splitInventory
	}
	initialSplitAmount := globalInitialSplits[market.ConditionID]
	splitMu.Unlock()

	engine.RegisterSplitInventory(splitInventory)   // Register for equity calculation
	tui.RegisterSplitInventory(splitInventory)      // Register for TUI display
	replenishCtrl := paper.NewReplenishController() // Debounce replenish goroutines

	for {
		select {
		case <-ctx.Done():
			isShutdown := globalCtx.Err() != nil
			timeToExpiry := time.Until(endTime)

			// TUI Restart logic: Preserve inventory if active
			if !isShutdown && timeToExpiry > 30*time.Second {
				tui.LogEvent("[%s] ⚠️ TUI Restart: Preserving split inventory for next round", id)
				return
			}

			// Shutdown or expiration: Execute full merge and cleanup using actual on-chain balances
			tui.LogEvent("[%s] 🔀 EMERGENCY EXIT: Querying on-chain balances for full merge...", id)

			cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancelCleanup()

			// Query all token balances
			balances := make([]float64, len(outcomes))
			tokens := make([]string, len(outcomes))

			for i, out := range outcomes {
				tokens[i] = getTokenID(out)
			}

			// Retry loop for balances
			var minShares float64
			var fetchErr error
			for attempt := 1; attempt <= 20; attempt++ {
				minShares = math.MaxFloat64
				fetchErr = nil
				for i, t := range tokens {
					if t == "" {
						fetchErr = fmt.Errorf("empty token ID for outcome %s", outcomes[i])
						break
					}
					bal, err := trader.GetCTFBalanceFloat(cleanupCtx, t)
					if err != nil {
						fetchErr = err
						break
					}
					balances[i] = bal
					if bal < minShares {
						minShares = bal
					}
				}
				if fetchErr == nil {
					break
				}
				time.Sleep(1000 * time.Millisecond)
			}

			if fetchErr == nil {
				if minShares >= 0.000001 {
					txHash, err := trader.MergeOnChain(cleanupCtx, market.ConditionID, minShares, len(market.Tokens))
					if err == nil {
						// Only record paper engine stats if it's a binary market (engine limitation)
						if len(outcomes) == 2 {
							engine.MergeForMarket(id, outcomes[0], outcomes[1], minShares)
							splitInventory.RecordMerge(id, outcomes[0], outcomes[1], minShares)
						}

						if txHash != "" && len(txHash) >= 10 {
							tui.LogEvent("[%s] 🔀 Merged %.0f sets | Tx: %s...", id, minShares, txHash[:10])
						} else {
							tui.LogEvent("[%s] 🔀 Merged %.0f sets", id, minShares)
						}
						for i := range balances {
							balances[i] -= minShares
						}
					} else {
						tui.LogEvent("[%s] ⚠️ Failed to merge %.0f sets on emergency exit: %v", id, minShares, err)
					}
				}

				// Sell remaining unbalanced shares
				for i, out := range outcomes {
					if balances[i] >= 0.01 {
						rate := tokenFeeRates[out]
						if rate == 0 {
							rate = 1000
						}
						_, sellErr := trader.Sell(cleanupCtx, tokens[i], out, 0.01, balances[i], api.OrderTypeMarket, api.TIFFillAndKill, rate)
						if sellErr == nil {
							tui.LogEvent("[%s] 📉 Sold %.0f unbalanced shares of %s", id, balances[i], out)
						}
					}
				}
			} else {
				tui.LogEvent("[%s] ⚠️ Could not query on-chain balances for emergency merge: %v", id, fetchErr)
			}
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

				// Parse and process WebSocket message immediately.
				//
				// Polymarket CLOB WS sends two message shapes:
				//   1. Book snapshot  – array of objects with "bids"/"asks" fields
				//      (event_type:"book").  Sent on subscribe and after reconnect.
				//   2. Price-change delta – single object with "price_changes" array
				//      (event_type:"price_change").  Contains only changed levels
				//      but NO size information, so we cannot update the full book
				//      from them.  We apply the best-bid/ask update from the delta
				//      and rely on the 4 ms REST poll for full-depth accuracy.
				//
				// IMPORTANT: only write to tokenBids/tokenAsks when the parsed
				// value is strictly positive — a zero value means "no orders on
				// that side in this message" and must NOT overwrite a previously
				// valid price from REST or an earlier WS snapshot.
				if books, err := api.ParseOrderBooks(msg); err == nil && len(books) > 0 {
					for _, b := range books {
						outcome := tokenToOutcome[b.AssetID]
						if outcome == "" {
							continue
						}

						bid, ask := 0.0, 0.0
						for _, order := range b.Bids {
							p, err := strconv.ParseFloat(order.Price, 64)
							if err != nil {
								continue
							}
							if p > 0 && p < 1.0 && p > bid {
								bid = p
							}
						}
						for _, order := range b.Asks {
							p, err := strconv.ParseFloat(order.Price, 64)
							if err != nil {
								continue
							}
							if p > 0 && p < 1.0 && (ask == 0 || p < ask) {
								ask = p
							}
						}

						// WS Snapshot is absolute state.
						if bid > 0 && ask > 0 && bid >= ask {
							// Reject crossed snapshot and clear state
							tokenBids[outcome] = 0
							tokenAsks[outcome] = 0
							tokenFullBids[outcome] = nil
							tokenFullAsks[outcome] = nil
							continue
						}

						tokenBids[outcome] = bid
						tokenAsks[outcome] = ask

						// Always update full depth from snapshots
						tokenFullBids[outcome] = mkt.LevelsToPriceDepth(b.Bids, true)
						tokenFullAsks[outcome] = mkt.LevelsToPriceDepth(b.Asks, false)

						if bid > 0 && ask > 0 {
							mid := (bid + ask) / 2
							engine.UpdateMarketData(id, outcome, mid, bid, ask)
						}
					}
					lastUpdate = time.Now()
				} else if update, err := api.ParsePriceUpdate(msg); err == nil && len(update.PriceChanges) > 0 {
					// ── Price-change delta ─────────────────────────────────
					foundForThisMarket := false

					for _, pc := range update.PriceChanges {
						outcome := tokenToOutcome[pc.AssetID]
						if outcome == "" {
							continue
						}
						foundForThisMarket = true
						p, errP := strconv.ParseFloat(pc.Price, 64)
						s, errS := strconv.ParseFloat(pc.Size, 64)
						if errP != nil || errS != nil || p <= 0 {
							continue
						}

						switch pc.Side {
						case "BUY":
							tokenFullBids[outcome] = mkt.ApplyDelta(tokenFullBids[outcome], p, s, true)
						case "SELL":
							tokenFullAsks[outcome] = mkt.ApplyDelta(tokenFullAsks[outcome], p, s, false)
						}
					}

					// Update best bids/asks based on the new full depth
					for outcome := range tokenToOutcome {
						bids := tokenFullBids[outcome]
						if len(bids) > 0 {
							tokenBids[outcome] = bids[0].Price
						} else {
							tokenBids[outcome] = 0
						}

						asks := tokenFullAsks[outcome]
						if len(asks) > 0 {
							tokenAsks[outcome] = asks[0].Price
						} else {
							tokenAsks[outcome] = 0
						}

						if tokenBids[outcome] > 0 && tokenAsks[outcome] > 0 {
							// Check for crossed book (WS state corruption or missing delete delta)
							if tokenBids[outcome] >= tokenAsks[outcome] {
								// Force a REST poll immediately by making WS look stale
								lastUpdate = time.Now().Add(-20 * time.Second)
								
								// Clear corrupted data
								tokenBids[outcome] = 0
								tokenAsks[outcome] = 0
								tokenFullBids[outcome] = nil
								tokenFullAsks[outcome] = nil
								continue
							}

							mid := (tokenBids[outcome] + tokenAsks[outcome]) / 2
							engine.UpdateMarketData(id, outcome, mid, tokenBids[outcome], tokenAsks[outcome])
						}
					}

					if foundForThisMarket {
						lastUpdate = time.Now()
					}
				} else if book, err := api.ParseOrderBook(msg); err == nil && book.AssetID != "" {
					// ── Book snapshot (single object) ──────────────────────
					bid, ask := 0.0, 0.0
					for _, b := range book.Bids {
						p, _ := strconv.ParseFloat(b.Price, 64)
						if p > 0 && p < 1.0 && p > bid {
							bid = p
						}
					}
					for _, order := range book.Asks {
						p, _ := strconv.ParseFloat(order.Price, 64)
						if p > 0 && p < 1.0 && (ask == 0 || p < ask) {
							ask = p
						}
					}

					if bid > 0 && ask > 0 && bid >= ask {
						continue // Reject crossed snapshot
					}

					outcome := tokenToOutcome[book.AssetID]
					if outcome != "" {
						lastUpdate = time.Now()
						// Guard: only persist valid (non-zero) prices.
						if bid > 0 {
							tokenBids[outcome] = bid
						}
						if ask > 0 {
							tokenAsks[outcome] = ask
						}
						if bid > 0 && ask > 0 {
							mid := (bid + ask) / 2
							engine.UpdateMarketData(id, outcome, mid, bid, ask)
						}
						tokenFullBids[outcome] = mkt.LevelsToPriceDepth(book.Bids, true)
						tokenFullAsks[outcome] = mkt.LevelsToPriceDepth(book.Asks, false)
					}
				}
			default:
				goto doneWS
			}
		}
	doneWS:

		// Final safety check: scrub any crossed books that survived the WS processing loop
		for _, outcome := range outcomes {
			if tokenBids[outcome] > 0 && tokenAsks[outcome] > 0 && tokenBids[outcome] >= tokenAsks[outcome] {
				tokenBids[outcome] = 0
				tokenAsks[outcome] = 0
				tokenFullBids[outcome] = nil
				tokenFullAsks[outcome] = nil
				lastUpdate = time.Now().Add(-20 * time.Second) // Force REST poll
			}
		}

		if messagesProcessed > 0 {
			tui.UpdateMarketPricesWithSource(id, tokenBids, tokenAsks, "WS")
		}

		// Also update order book depth for live display
		bidDepth := make(map[string][]paper.MarketLevel)
		askDepth := make(map[string][]paper.MarketLevel)

		for _, outcome := range outcomes {
			if bids, ok := tokenFullBids[outcome]; ok {
				bidDepth[outcome] = append([]paper.MarketLevel(nil), bids...)
			}
			if asks, ok := tokenFullAsks[outcome]; ok {
				askDepth[outcome] = append([]paper.MarketLevel(nil), asks...)
			}
		}
		tui.UpdateOrderBookDepth(id, bidDepth, askDepth)

		// ============ REST FALLBACK ============
		// WS is primary for liquidity data via full depth updates and deltas.
		// Only poll REST if WS is unhealthy or stale.
		staleTime := time.Since(lastUpdate)

		// Update WS staleness and ping latency in TUI
		wsTimeSinceMsg := wsMgr.TimeSinceLastMessage()
		tui.UpdateWSLatency(wsTimeSinceMsg)
		tui.UpdateWSPingLatency(wsMgr.PingLatency())

		// Force REST fallback if a book was just cleared or if it is currently crossed
		forceRestFallback := false
		for _, outcome := range outcomes {
			if tokenBids[outcome] == 0 || tokenAsks[outcome] == 0 || tokenBids[outcome] >= tokenAsks[outcome] {
				// Only force if we haven't updated in a few seconds to avoid spamming
				if staleTime > 3*time.Second {
					forceRestFallback = true
					break
				}
			}
		}

		wsUnhealthy := !wsMgr.IsConnected() || wsTimeSinceMsg > 10*time.Second
		if forceRestFallback || (wsUnhealthy && staleTime > 3*time.Second) {
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

		liveCfg := tui.GetSettings()

		// ═══════════════════════════════════════════════════════════════════════════
		// SPLIT STRATEGY: Sell to panic buyers when bid_sum > $1.03
		// This is SEPARATE from the panic buy strategy (buy when ask_sum < $0.98)
		// Split shares are ONLY for selling, bought shares are ONLY for merging
		// ═══════════════════════════════════════════════════════════════════════════
		skipPanicBuy := false // Flag to skip panic buy when nearing expiry

		if liveCfg.SplitStrategyEnabled && len(tokenBids) >= 2 && len(outcomes) == 2 {
			bid1 := tokenBids[outcomes[0]]
			bid2 := tokenBids[outcomes[1]]

			// Check if we need to merge before expiry
			timeToExpiry := time.Until(endTime)
			mergeBuffer := time.Duration(cfg.SplitMergeBufferSeconds) * time.Second

			if timeToExpiry <= mergeBuffer && timeToExpiry > 0 {
				// Merging before expiry can take a long time and block the bot.
				// We disable the auto-merge here and just let the market expire
				// to be redeemed in the background later.
				skipPanicBuy = true
			}

			// Initial split: create inventory if not done yet
			// Move to BACKGROUND to prevent blocking the main trading loop
			splitMu.Lock()
			isSplit := globalSplitStatus[market.ConditionID]

			// We check the cooldown safely using the global initial split value map (if 0, we can try)
			// But for cooldown, we'll just move nextSplitAttempt access inside the lock if we use a global cooldown map.
			// Or better: keep nextSplitAttempt but protect it with splitMu.
			shouldSplit := !isSplit && time.Now().After(nextSplitAttempt)
			if shouldSplit {
				// Optimistically mark as split to prevent concurrent duplicate attempts
				globalSplitStatus[market.ConditionID] = true
			}
			splitMu.Unlock()

			if shouldSplit && replenishCtrl.MarkInProgress() {
				baseTradeSize := cfg.CalculateTradeSize(currentBalance)

				// Scale initial buffer based on balance: 2x trade size, but at least $2 and at most 25% of balance
				initialBuffer := baseTradeSize * 2.0
				if initialBuffer < 2.0 {
					initialBuffer = 2.0
				}

				maxInitial := currentBalance * cfg.SplitInitialCapPct
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

						splitTxMu.Lock()
						txHash, err := trader.SplitOnChain(splitCtx, condID, amt, len(outcomes))
						splitTxMu.Unlock()

						splitMu.Lock() // Re-acquire lock to update shared state
						if err != nil {
							tui.LogEvent("[%s] ⚠️ SPLIT: Background initial split failed: %v (will retry in 15s)", mID, err)
							// Set cooldown on failure to prevent RPC spam and nonce issues
							nextSplitAttempt = time.Now().Add(15 * time.Second)

							// Revert optimistic split status so it can be retried
							globalSplitStatus[condID] = false
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
							globalSplitStatus[condID] = true
							globalInitialSplits[condID] = amt
							initialSplitAmount = amt
						}
						splitMu.Unlock()
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
			if bid1 >= liveCfg.MinAskPrice && bid2 >= liveCfg.MinAskPrice && bid1 <= liveCfg.MaxAskPrice && bid2 <= liveCfg.MaxAskPrice {
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
					MaxBalancePercent:  cfg.SplitReplenishCapPct,
				})

				if decision.ShouldReplenish && replenishCtrl.MarkInProgress() {
					tui.LogEvent("[%s] 🔄 SPLIT: Low inventory (%.0f/%.0f), replenishing +%.0f shares...", id, currentShares, initialSplitAmount, decision.Amount)
					go func(mID, condID, out0, out1 string, amt float64) {
						defer replenishCtrl.MarkComplete()
						// Use derived context for proper shutdown propagation
						bgCtx, bgCancel := context.WithTimeout(ctx, 60*time.Second)
						defer bgCancel()

						splitTxMu.Lock()
						_, bgErr := trader.SplitOnChain(bgCtx, condID, amt, len(outcomes))
						splitTxMu.Unlock()

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

				if sellMargin >= cfg.SplitMinMarginSell-1e-4 && time.Since(lastSplitSell) > 2*time.Second {
					// DETERMINISTIC AGGRESSION
					// Use SplitInitialCapPct to determine the number of shares to sell
					requestedShares := currentBalance * cfg.SplitInitialCapPct

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
						// Hard safety cap
						if sharesToSell > 250 {
							sharesToSell = 250
						}

						// ═══════════════════════════════════════════════════════════════
						// MATCHED BID LIQUIDITY: Walk bid levels (price descending) and
						// only count pairs where bid1+bid2 >= minSum (the profitability
						// threshold). This mirrors utilbot's estimateMatchedLiquidity and
						// ensures we never order more than what can actually be filled at
						// a profitable price. Used for BOTH sizing and display.
						// ═══════════════════════════════════════════════════════════════
						bids1 := tokenFullBids[outcomes[0]]
						bids2 := tokenFullBids[outcomes[1]]
						bookDepth1, bookDepth2 := len(bids1), len(bids2)
						minSum := 1.0 + (cfg.SplitMinMarginSell / 100.0)

						sortedBids1 := make([]paper.MarketLevel, len(bids1))
						copy(sortedBids1, bids1)
						// Inject BBO if missing due to orderbook lag to prevent liq: 0/0
						hasBid1 := false
						for _, b := range sortedBids1 {
							if b.Price >= bid1-1e-6 {
								hasBid1 = true
								break
							}
						}
						if !hasBid1 {
							sortedBids1 = append(sortedBids1, paper.MarketLevel{Price: bid1, Size: sharesToSell})
						}
						sort.Slice(sortedBids1, func(a, b int) bool { return sortedBids1[a].Price > sortedBids1[b].Price })

						sortedBids2 := make([]paper.MarketLevel, len(bids2))
						copy(sortedBids2, bids2)
						hasBid2 := false
						for _, b := range sortedBids2 {
							if b.Price >= bid2-1e-6 {
								hasBid2 = true
								break
							}
						}
						if !hasBid2 {
							sortedBids2 = append(sortedBids2, paper.MarketLevel{Price: bid2, Size: sharesToSell})
						}
						sort.Slice(sortedBids2, func(a, b int) bool { return sortedBids2[a].Price > sortedBids2[b].Price })
						var rawLiq1, rawLiq2, matchedBidLiq float64
						var maxValidI, maxValidJ int

						for bi, bj := 0, 0; bi < len(sortedBids1) && bj < len(sortedBids2); {
							if sortedBids1[bi].Price+sortedBids2[bj].Price < minSum-1e-6 {
								break // below profitability threshold — stop
							}
							if bi+1 > maxValidI {
								maxValidI = bi + 1
								rawLiq1 += sortedBids1[bi].Size
							}
							if bj+1 > maxValidJ {
								maxValidJ = bj + 1
								rawLiq2 += sortedBids2[bj].Size
							}
							matched := sortedBids1[bi].Size
							if sortedBids2[bj].Size < matched {
								matched = sortedBids2[bj].Size
							}
							matchedBidLiq += matched
							if sortedBids1[bi].Size <= sortedBids2[bj].Size {
								sortedBids2[bj].Size -= sortedBids1[bi].Size
								bi++
							} else {
								sortedBids1[bi].Size -= sortedBids2[bj].Size
								bj++
							}
						}

						// Cap to matched bid liquidity (follows utilbot's approach exactly)
						if sharesToSell > matchedBidLiq {
							sharesToSell = matchedBidLiq
						}

						// Ensure min order size 1 share
						if sharesToSell < 1.0 {
							sharesToSell = 1.0
						}

						sharesToSell = math.Floor(sharesToSell)

						if sharesToSell >= 1.0 && sharesToSell <= availableShares {
							// Enhanced log with liquidity and depth info (same format as paper bot)
							tui.LogEvent("[%s] 📈 SPLIT SELL! %s@$%.2f + %s@$%.2f = $%.3f (%.1f%%) | %.0f shares [liq: %.0f/%.0f, levels used: %d/%d (total depth: %d/%d)]",
								id, outcomes[0], bid1, outcomes[1], bid2, bidSum, sellMargin, sharesToSell,
								rawLiq1, rawLiq2, maxValidI, maxValidJ, bookDepth1, bookDepth2)

							// Sell both sides in parallel
							token0 := getTokenID(outcomes[0])
							token1 := getTokenID(outcomes[1])

							// Validate token IDs before trading
							if token0 == "" || token1 == "" {
								tui.LogEvent("[%s] ⚠️ SPLIT: Token ID not found for %s/%s", id, outcomes[0], outcomes[1])
								continue
							}

							// Sync CLOB allowance with on-chain state right before trading.
							// This is the root cause of "insufficient balance/allowance" errors:
							// the CLOB loses sync with on-chain state between startup and trade time.
							// Background ticker keeps allowance synced.

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
								res1, err1 = trader.Sell(ctx, token0, outcomes[0], 0.01, sharesToSell, api.OrderTypeMarket, api.TIFFillAndKill, rate)
							}()

							go func() {
								defer wg.Done()
								rate := tokenFeeRates[outcomes[1]]
								if rate == 0 {
									rate = 1000
								}
								res2, err2 = trader.Sell(ctx, token1, outcomes[1], 0.01, sharesToSell, api.OrderTypeMarket, api.TIFFillAndKill, rate)
							}()
							wg.Wait()

							// ROBUSTNESS: Wait for CLOB sync and verify if balance dropped
							time.Sleep(1500 * time.Millisecond)

							var side1Success, side2Success bool
							var sold1, sold2 float64
							verifyPositions, verifyErr := trader.GetPositions(ctx)
							if verifyErr == nil {
								var bal0, bal1 float64
								for _, pos := range verifyPositions {
									if pos.TokenID == token0 { bal0 = pos.Size }
									if pos.TokenID == token1 { bal1 = pos.Size }
								}

								// We started with at least `availableShares` before this sell block
								// Any drop in balance relative to our known inventory indicates a successful sell
								sold1 = availableShares - bal0
								sold2 = availableShares - bal1
								
								if sold1 < 0 { sold1 = 0 }
								if sold2 < 0 { sold2 = 0 }

								side1Success = (err1 == nil && res1 != nil && res1.Success) || sold1 > 0.01
								side2Success = (err2 == nil && res2 != nil && res2.Success) || sold2 > 0.01

								// Optimistic fallback if API returned true but positions haven't synced
								if side1Success && sold1 == 0 { sold1 = sharesToSell }
								if side2Success && sold2 == 0 { sold2 = sharesToSell }
							} else {
								tui.LogEvent("[%s] ⚠️ Failed to verify split sell positions: %v", id, verifyErr)
								side1Success = err1 == nil && res1 != nil && res1.Success
								side2Success = err2 == nil && res2 != nil && res2.Success
								if side1Success { sold1 = sharesToSell }
								if side2Success { sold2 = sharesToSell }
							}

							// ═══════════════════════════════════════════════════════════════
							// LEGGED SPLIT SELL RECOVERY: If one side sold and the other
							// didn't, retry the failed side once to avoid an unbalanced
							// split inventory (which causes permanent exposure).
							// ═══════════════════════════════════════════════════════════════
							if side1Success != side2Success {
								failedOutcome := outcomes[0]
								failedToken := token0
								failedRate := tokenFeeRates[outcomes[0]]
								retryShares := sold2 // If 0 failed, retry exactly what 1 sold
								
								if side1Success {
									failedOutcome = outcomes[1]
									failedToken = token1
									failedRate = tokenFeeRates[outcomes[1]]
									retryShares = sold1 // If 1 failed, retry exactly what 0 sold
								}
								if failedRate == 0 {
									failedRate = 1000
								}
								tui.LogEvent("[%s] ⚠️ SPLIT LEGGED: %s sold, %s failed — retrying %.2f shares...", id,
									map[bool]string{true: outcomes[0], false: outcomes[1]}[side1Success], failedOutcome, retryShares)
								
								time.Sleep(1 * time.Second)
								retryRes, retryErr := trader.Sell(ctx, failedToken, failedOutcome, 0.01, retryShares, api.OrderTypeMarket, api.TIFFillAndKill, failedRate)
								
								// Re-verify after retry
								if retryPos, retryVerErr := trader.GetPositions(ctx); retryVerErr == nil {
									for _, pos := range retryPos {
										if pos.TokenID == failedToken {
											// Sold amount is the difference between available and current
											retrySold := availableShares - pos.Size
											if retrySold > 0.01 {
												if !side1Success {
													side1Success = true
													sold1 = retrySold
												} else {
													side2Success = true
													sold2 = retrySold
												}
											}
										}
									}
								} else if retryErr == nil && retryRes != nil && retryRes.Success {
									// Optimistic retry success
									if !side1Success {
										side1Success = true
										sold1 = retryShares
									} else {
										side2Success = true
										sold2 = retryShares
									}
								}
								
								if side1Success && side2Success {
									tui.LogEvent("[%s] ✅ SPLIT: Retry %s succeeded", id, failedOutcome)
								} else {
									tui.LogEvent("[%s] ❌ SPLIT: Retry %s failed (legged position remains)", id, failedOutcome)
								}
							}

							if side1Success && side2Success {
								// Both sides sold - record in split inventory using actual sold amounts
								profit1 := splitInventory.RecordSell(id, outcomes[0], sold1, bid1)
								profit2 := splitInventory.RecordSell(id, outcomes[1], sold2, bid2)
								totalProfit := profit1 + profit2
								engine.AddRealizedPnL(totalProfit)
								tui.LogEvent("[%s] ✅ SPLIT SOLD! %s: %.2f, %s: %.2f | Profit: +$%.2f", id, outcomes[0], sold1, outcomes[1], sold2, totalProfit)
								tui.RecordOrder(id, outcomes[0], "SELL", sold1, bid1, sold1*bid1, sellMargin, profit1, "FILLED")
								tui.RecordOrder(id, outcomes[1], "SELL", sold2, bid2, sold2*bid2, sellMargin, profit2, "FILLED")
								
								// Refresh balance after successful sell (cash increased)
								_, _ = trader.ForceRefreshBalance(ctx)

								tui.LogEvent("[%s] ✅ Execution complete after successful split sell.", id)
							} else {
								// Partial success - record to keep inventory accurate
								if side1Success {
									splitInventory.RecordSell(id, outcomes[0], sold1, bid1)
									tui.LogEvent("[%s] ⚠️ SPLIT: Only %s sold %.2f (one-shot)", id, outcomes[0], sold1)
								}
								if side2Success {
									splitInventory.RecordSell(id, outcomes[1], sold2, bid2)
									tui.LogEvent("[%s] ⚠️ SPLIT: Only %s sold %.2f (one-shot)", id, outcomes[1], sold2)
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
		if time.Now().Before(panicBuyCooldown) {
			continue
		}
		if len(tokenAsks) >= 2 && len(outcomes) == 2 {
			ask1 := tokenAsks[outcomes[0]]
			ask2 := tokenAsks[outcomes[1]]

			// Read live price-range filter from settings panel (adjustable at runtime)
			realbotCfg := tui.GetSettings()
			rMinAsk := realbotCfg.MinAskPrice
			rMaxAsk := realbotCfg.MaxAskPrice

			if ask1 >= rMinAsk && ask1 <= rMaxAsk && ask2 >= rMinAsk && ask2 <= rMaxAsk {
				// Slippage buffer: set to 0 to execute exactly at configured MinMarginPercent
				const slippageBuffer = 0.0
				sum := ask1 + ask2
				bufferedSum := (ask1 + slippageBuffer) + (ask2 + slippageBuffer)
				margin := (1.0 - bufferedSum) * 100 // Use buffered sum for margin check

				if margin >= cfg.MinMarginPercent-1e-4 {
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
						// currentCash = latestBalance // Unused
						currentBalance = latestBalance
					}

					// For real bot, equity = cash + market value of positions
					// Simplification: use cash balance as proxy for sizing, or fetch equity
					currentEquity := currentBalance // In realbot we use cash as conservative equity
					tradeSize := cfg.CalculateTradeSize(currentEquity)

					// Get max fee rate for conservative margin calculation
					maxFeeRateBps := 0
					if rate1, ok := tokenFeeRates[outcomes[0]]; ok && rate1 > maxFeeRateBps {
						maxFeeRateBps = rate1
					}
					if rate2, ok := tokenFeeRates[outcomes[1]]; ok && rate2 > maxFeeRateBps {
						maxFeeRateBps = rate2
					}

					// Scale shares based on margin (User requested NO fee buffer deduction)
					shares := tradeSize / sum
					shares = math.Floor(shares) // Round down to integer shares for cleaner execution matching utilbot

					// Fee estimation and balance check logging removed per user request

					// AGGREGATED LIQUIDITY: Calculate total matched liquidity across ALL price levels
					// that maintain minimum margin. This allows "chasing" liquidity deeper into the book.
					maxSum := 1.0 - (cfg.MinMarginPercent / 100.0) // e.g., 2% margin → max sum = 0.98

					// Copy and sort asks by price ascending for both outcomes
					asks1 := make([]paper.MarketLevel, len(tokenFullAsks[outcomes[0]]))
					copy(asks1, tokenFullAsks[outcomes[0]])
					// Inject BBO if missing due to orderbook lag
					hasAsk1 := false
					for _, a := range asks1 {
						if a.Price <= ask1+1e-6 {
							hasAsk1 = true
							break
						}
					}
					if !hasAsk1 {
						asks1 = append(asks1, paper.MarketLevel{Price: ask1, Size: shares})
					}
					sort.Slice(asks1, func(i, j int) bool { return asks1[i].Price < asks1[j].Price })

					asks2 := make([]paper.MarketLevel, len(tokenFullAsks[outcomes[1]]))
					copy(asks2, tokenFullAsks[outcomes[1]])
					hasAsk2 := false
					for _, a := range asks2 {
						if a.Price <= ask2+1e-6 {
							hasAsk2 = true
							break
						}
					}
					if !hasAsk2 {
						asks2 = append(asks2, paper.MarketLevel{Price: ask2, Size: shares})
					}
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
						if p1+p2 > maxSum+1e-6 {
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
					cost := strategy.CalculateTradeMetricsFlat(shares, sum, maxFeeRateBps).Cost

					// REFRESH BALANCE RIGHT BEFORE TRADING to prevent unbalanced fills
					// This is the "Zero Excuse" check to ensure we have enough for BOTH sides
					latestBalance, balErr := trader.GetBalance(ctx)
					if balErr == nil {
						currentBalance = latestBalance
						// currentCash = latestBalance // Unused
					}

					// Check risk limits only (Balance check disabled per user request to match utilbot behavior)
					if !riskMgr.CanPlaceOrder(cost) {
						tui.LogEvent("[%s] ⚠️ Risk limit exceeded for cost $%.2f", id, cost)
						continue
					}

					// Skipping conservative balance checks (costWithBuffer > currentCash) to allow max execution.
					// If balance is insufficient, the API call will fail naturally.

					// Check why we might skip trading
					if shares < 1.0 {
						tui.LogEvent("[%s] ⚠️ Actionable matched liquidity below 1.0 share minimum: %.2f", id, shares)
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

						tui.LogEvent("[%s] 🎯 ARB! %s@$%.3f + %s@$%.3f = $%.3f (%.1f%% margin, %.1f%% after slippage) [liq: %.0f/%.0f, levels used: %d/%d (total depth: %d/%d)]",
							id, outcomes[0], ask1, outcomes[1], ask2, sum, (1.0-sum)*100, margin, liq1, liq2, maxValidI, maxValidJ, bookDepth1, bookDepth2)

						// Map tokens
						token0, token1 := "", ""
						for tid, out := range tokenToOutcome {
							if out == outcomes[0] {
								token0 = tid
							} else if out == outcomes[1] {
								token1 = tid
							}
						}

						// MARKET EXECUTION: Use a small +$0.02 buffer above the ask to ensure
						// fill while keeping the maker amount as low as possible.
						// utilbot uses ask price directly; +$0.02 mirrors that closely while
						// still providing a slippage cushion for fast-moving markets.
						// Keeping this small also reduces the chance of hitting the CLOB $1/side
						// minimum on cheap outcome tokens (e.g. $0.24 ask → $0.26 limit instead
						// of the old $0.29, requiring fewer shares to clear the minimum).
						limitPrice1 := math.Min(0.99, ask1+0.02)
						limitPrice2 := math.Min(0.99, ask2+0.02)

						// ═══════════════════════════════════════════════════════════════
						// CLOB MINIMUM ORDER VALUE: Each side must be >= $1.
						// When one outcome has a very low price (e.g. $0.24), a small
						// share count can produce a sub-$1 maker amount the CLOB rejects
						// with "invalid amount for a marketable BUY order, min size: $1".
						// Solution: compute the minimum shares required so that
						//   shares × limitPriceN >= $1 for BOTH sides, then either bump
						//   up to that floor or skip if balance can't cover it.
						// ═══════════════════════════════════════════════════════════════
						const minOrderUSD = 1.0
						minSharesCLOB := math.Ceil(math.Max(minOrderUSD/limitPrice1, minOrderUSD/limitPrice2))
						if shares < minSharesCLOB {
							totalMinCost := minSharesCLOB * (limitPrice1 + limitPrice2)
							if totalMinCost > currentBalance {
								tui.LogEvent("[%s] ⚠️ Skipping: min order %.0f shares ($%.2f) exceeds balance $%.2f (lim1=$%.2f lim2=$%.2f)",
									id, minSharesCLOB, totalMinCost, currentBalance, limitPrice1, limitPrice2)
								lastTrade = time.Now() // apply cooldown to avoid log spam
								continue
							}
							tui.LogEvent("[%s] 📏 %.0f→%.0f shares to meet CLOB $1/side min (lim1=$%.2f lim2=$%.2f cost=$%.2f)",
								id, shares, minSharesCLOB, limitPrice1, limitPrice2, totalMinCost)
							shares = minSharesCLOB
						}

						// Sync CLOB allowance with on-chain state right before trading.
						// Root cause of "insufficient balance/allowance" errors in realbot:
						// allowance synced once at startup can go stale by the time an arb opportunity arrives.
						// Background ticker keeps allowance synced.
						var wg sync.WaitGroup
						wg.Add(2)

						var res1, res2 *trading.TradeResult
						var err1, err2 error

						go func() {
							defer wg.Done()
							rate := tokenFeeRates[outcomes[0]]
							if rate == 0 {
								rate = 1000
							}
							res1, err1 = trader.Buy(ctx, token0, outcomes[0], limitPrice1, shares, api.OrderTypeMarket, api.TIFFillAndKill, rate)
						}()

						go func() {
							defer wg.Done()
							rate := tokenFeeRates[outcomes[1]]
							if rate == 0 {
								rate = 1000
							}
							res2, err2 = trader.Buy(ctx, token1, outcomes[1], limitPrice2, shares, api.OrderTypeMarket, api.TIFFillAndKill, rate)
						}()

						wg.Wait()

						// Wait for CLOB to sync before verifying positions.
						// The CLOB can return a 400/error response while the order actually
						// went through (race between execution and response) — "fake error".
						// Without this delay both positions show 0.0000 immediately, the bot
						// thinks both sides failed, and we end up with a legged position
						// (e.g. 2 Up filled silently, 0 Down). 1.5s is enough for CLOB sync.
						time.Sleep(1500 * time.Millisecond)

						// ROBUSTNESS: Verify actual positions from CLOB after sync delay.
						// This catches "Fake Errors" (timeout/400 but actually filled) and
						// prevents double-buys on retry.
						var side1Success, side2Success bool
						var filled1, filled2 float64
						verifyPositions, verifyErr := trader.GetPositions(ctx)
						if verifyErr == nil {
							var bal0, bal1 float64
							for _, pos := range verifyPositions {
								if pos.TokenID == token0 {
									bal0 = pos.Size
								} else if pos.TokenID == token1 {
									bal1 = pos.Size
								}
							}

							tui.LogEvent("[%s] 🔍 Verify Positions: %s=%.4f, %s=%.4f (Target: %.0f)", id, outcomes[0], bal0, outcomes[1], bal1, shares)

							// Override success flags based on actual inventory
							// We consider the side "filled" if we got at least 0.01 shares (partial fill)
							// OR if the API reported success. 
							filled1 = bal0
							filled2 = bal1
							side1Success = (err1 == nil && res1 != nil && res1.Success) || bal0 > 0.01
							side2Success = (err2 == nil && res2 != nil && res2.Success) || bal1 > 0.01
							
							// If API said success but CLOB is lagging, assume full fill for log metrics initially
							if side1Success && filled1 == 0 {
							    filled1 = shares
							}
							if side2Success && filled2 == 0 {
							    filled2 = shares
							}
						} else {
							tui.LogEvent("[%s] ⚠️ Failed to verify positions: %v (relying on API response)", id, verifyErr)
							side1Success = err1 == nil && res1 != nil && res1.Success
							side2Success = err2 == nil && res2 != nil && res2.Success
							if side1Success { filled1 = shares }
							if side2Success { filled2 = shares }
						}

						// Calculate costs using the actual filled size for reporting
						cost1 := filled1 * price1
						cost2 := filled2 * price2

						// Log results based on VERIFIED state
						if side1Success {
							tui.LogEvent("[%s] ✅ Side 1 MARKET: %s (Target $%.3f, Filled: %.2f/%.2f)", id, outcomes[0], price1, filled1, shares)
							tui.RecordOrder(id, outcomes[0], "BUY", filled1, price1, cost1, margin, 0.0, "FILLED")
						} else {
							// Log the actual failure reason (err or res.Message)
							if err1 != nil {
								tui.LogEvent("[%s] ❌ Side 1 MARKET Fail: %v", id, err1)
							} else if res1 != nil && res1.Message != "" {
								tui.LogEvent("[%s] ❌ Side 1 MARKET Fail: %s", id, res1.Message)
							} else if res1 == nil {
								tui.LogEvent("[%s] ❌ Side 1 MARKET Fail: nil response", id)
							} else {
								tui.LogEvent("[%s] ❌ Side 1 MARKET Fail: unknown error (res=%v)", id, res1)
							}
							tui.RecordOrder(id, outcomes[0], "BUY", shares, price1, cost1, margin, 0.0, "FAILED")
						}

						if side2Success {
							tui.LogEvent("[%s] ✅ Side 2 MARKET: %s (Target $%.3f, Filled: %.2f/%.2f)", id, outcomes[1], price2, filled2, shares)
							tui.RecordOrder(id, outcomes[1], "BUY", filled2, price2, cost2, margin, 0.0, "FILLED")
						} else {
							// Log the actual failure reason (err or res.Message)
							if err2 != nil {
								tui.LogEvent("[%s] ❌ Side 2 MARKET Fail: %v", id, err2)
							} else if res2 != nil && res2.Message != "" {
								tui.LogEvent("[%s] ❌ Side 2 MARKET Fail: %s", id, res2.Message)
							} else if res2 == nil {
								tui.LogEvent("[%s] ❌ Side 2 MARKET Fail: nil response", id)
							} else {
								tui.LogEvent("[%s] ❌ Side 2 MARKET Fail: unknown error (res=%v)", id, res2)
							}
							tui.RecordOrder(id, outcomes[1], "BUY", shares, price2, cost2, margin, 0.0, "FAILED")
						}

						// ═══════════════════════════════════════════════════════════════
						// LEGGED SHARE VERIFICATION: If one side filled and the other didn't,
						// wait 2 seconds for late settlement, re-verify positions.
						// We no longer retry buys here to avoid $1 minimum errors and 
						// double-spending. We let the auto-cleanup routine handle it.
						// ═══════════════════════════════════════════════════════════════
						if side1Success != side2Success {
							tui.LogEvent("[%s] ⚠️ ARB LEGGED: %s=%v %s=%v — waiting 2s then re-verifying...",
								id, outcomes[0], side1Success, outcomes[1], side2Success)
							time.Sleep(2 * time.Second)

							// Re-verify: the "failed" order may have settled during the delay
							if retryPos, retryErr := trader.GetPositions(ctx); retryErr == nil {
								var rbal0, rbal1 float64
								for _, pos := range retryPos {
									if pos.TokenID == token0 {
										rbal0 = pos.Size
									} else if pos.TokenID == token1 {
										rbal1 = pos.Size
									}
								}
								prevSide1, prevSide2 := side1Success, side2Success
								side1Success = rbal0 > 0.01
								side2Success = rbal1 > 0.01
								if side1Success { filled1 = rbal0 }
								if side2Success { filled2 = rbal1 }
								tui.LogEvent("[%s] 🔍 Re-verify after delay: %s=%.4f (%v→%v), %s=%.4f (%v→%v)",
									id, outcomes[0], rbal0, prevSide1, side1Success,
									outcomes[1], rbal1, prevSide2, side2Success)
							}

							// Final status after verification
							if side1Success != side2Success {
								failedSide := outcomes[1]
								if !side1Success {
									failedSide = outcomes[0]
								}
								tui.LogEvent("[%s] ⚠️ ARB UNBALANCED: %s still not filled (legging to auto-cleanup)", id, failedSide)
							} else if side1Success && side2Success {
								tui.LogEvent("[%s] ✅ Legged position recovered via delayed settlement — both sides now filled (%.2f vs %.2f)", id, filled1, filled2)
							}
						}

						// NOW record to engine - only record positions that actually succeeded
						// This ensures engine state matches reality for accurate drawdown calculation
						if side1Success && side2Success {
						        // Both sides filled (either initially or via recovery) - record both
						        _, _ = engine.BuyForMarket(id, outcomes[0], price1, filled1)
						        _, _ = engine.BuyForMarket(id, outcomes[1], price2, filled2)

						        // ONE-SHOT: Execute merge and then EXIT
							tui.LogEvent("[%s] ⏳ Waiting 5s for position sync before merge...", id)
							time.Sleep(5 * time.Second)

							mergeCtx, mergeCancel := context.WithTimeout(context.Background(), 60*time.Second)

							// Query on-chain CTF balances with retries (mirrors utilbot's queryBalancedCTFBalances).
							// CLOB positions are off-chain order records; for MergeOnChain the tokens must be
							// physically present in the CTF contract on-chain. Using on-chain balance with
							// settle-delay retries is the only way to guarantee the merge quantity is correct.
							mergeQty := 0.0
							bal0, bal1, balErr0, balErr1 := trader.QueryBalancedCTFBalances(mergeCtx, token0, token1, shares)
							if balErr0 != nil || balErr1 != nil {
								tui.LogEvent("[%s] ⚠️ On-chain balance query failed (err0=%v, err1=%v), falling back to %.0f shares", id, balErr0, balErr1, shares)
								bal0, bal1 = shares, shares
							}
							tui.LogEvent("[%s] 📊 On-chain Balances: %s=%.6f, %s=%.6f", id, outcomes[0], bal0, outcomes[1], bal1)
							// Don't floor — CTF supports 6 decimals; merge exactly what's settled on-chain
							actualMin := math.Min(math.Min(bal0, bal1), shares)
							if actualMin >= 0.000001 {
								mergeQty = actualMin
							} else {
								tui.LogEvent("[%s] ⚠️ No balanced on-chain positions to merge", id)
								continue // Keep monitoring instead of exiting
							}

							txHash, err := trader.MergeOnChain(mergeCtx, market.ConditionID, mergeQty, len(market.Tokens))
							if err != nil {
								tui.LogEvent("[%s] ⚠️ Merge failed: %v (will redeem at expiry)", id, err)
							} else {
								// Update engine to reflect closed position
								result := engine.MergeForMarket(id, outcomes[0], outcomes[1], mergeQty)
								if txHash != "" && len(txHash) >= 10 {
									tui.LogEvent("[%s] 💰 MERGED! +$%.2f profit | Tx: %s...", id, result.PnL, txHash[:10])
								} else {
									tui.LogEvent("[%s] 💰 MERGED! +$%.2f profit", id, result.PnL)
								}

								// Phase 3: Auto-cleanup of unbalanced excess shares using Market Sell
								excess0 := bal0 - actualMin
								excess1 := bal1 - actualMin
								if excess0 >= 0.01 {
									// Check against Polymarket's ~$1.00 minimum order value for sells
									// We estimate this by checking if shares * price >= $1.00
									// Since it's a market order, we conservatively estimate if shares < 2.0 (assuming max 0.50 price)
									// If it fails, we catch the error and log it clearly.
									tui.LogEvent("[%s] 🧹 Auto-cleanup: Market selling %.2f excess %s shares", id, excess0, outcomes[0])
									_, sellErr := trader.Sell(mergeCtx, token0, outcomes[0], 0.01, excess0, api.OrderTypeMarket, api.TIFFillAndKill, cfg.FeeRateBps)
									if sellErr != nil {
										if strings.Contains(sellErr.Error(), "min size") {
											tui.LogEvent("[%s] ⚠️ Kept %.2f %s shares as dust (Value under $1.00 minimum limit)", id, excess0, outcomes[0])
										} else {
											tui.LogEvent("[%s] ⚠️ Auto-cleanup sell failed for %s: %v", id, outcomes[0], sellErr)
										}
									}
								}
								if excess1 >= 0.01 {
									tui.LogEvent("[%s] 🧹 Auto-cleanup: Market selling %.2f excess %s shares", id, excess1, outcomes[1])
									_, sellErr := trader.Sell(mergeCtx, token1, outcomes[1], 0.01, excess1, api.OrderTypeMarket, api.TIFFillAndKill, cfg.FeeRateBps)
									if sellErr != nil {
										if strings.Contains(sellErr.Error(), "min size") {
											tui.LogEvent("[%s] ⚠️ Kept %.2f %s shares as dust (Value under $1.00 minimum limit)", id, excess1, outcomes[1])
										} else {
											tui.LogEvent("[%s] ⚠️ Auto-cleanup sell failed for %s: %v", id, outcomes[1], sellErr)
										}
									}
								}
							}

							mergeCancel() // Release merge context resources
							tui.LogEvent("[%s] ✅ Execution complete after successful buy and merge. Applying 5s cooldown...", id)

							// Refresh balance for next trade
							if newBal, err := trader.ForceRefreshBalance(ctx); err == nil {
								currentBalance = newBal
							}
							time.Sleep(5 * time.Second)
						} else if side1Success || side2Success {
							// Only one side filled after retry — record the unbalanced position and
							// temporarily block further panic buys to prevent exposure accumulation.
							if side1Success {
							        _, _ = engine.BuyForMarket(id, outcomes[0], price1, shares)
							        tui.LogEvent("[%s] ⚠️ Engine: Recording unbalanced position (only %s)", id, outcomes[0])
							}
							if side2Success {
								_, _ = engine.BuyForMarket(id, outcomes[1], price2, shares)
								tui.LogEvent("[%s] ⚠️ Engine: Recording unbalanced position (only %s)", id, outcomes[1])
							}

							cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 60*time.Second)

							tui.LogEvent("[%s] ⚠️ Legged trade detected! Waiting up to 10s for delayed on-chain balances to settle...", id)

							var bal0, bal1 float64
							var balErr0, balErr1 error
							settled := false

							// Ping pong loop to check balances for up to 10 seconds before acting
							for probe := 0; probe < 10; probe++ {
								bal0, bal1, balErr0, balErr1 = trader.QueryBalancedCTFBalances(cleanupCtx, token0, token1, shares)

								if balErr0 == nil && balErr1 == nil {
									// If both show up eventually, we are safe to merge
									if bal0 >= 0.01 && bal1 >= 0.01 {
										tui.LogEvent("[%s] 🟢 Delayed balances arrived: %s=%.2f, %s=%.2f. Attempting Merge!", id, outcomes[0], bal0, outcomes[1], bal1)
										settled = true
										break
									}
								}
								time.Sleep(1 * time.Second)
							}

							if balErr0 != nil || balErr1 != nil {
								tui.LogEvent("[%s] ⚠️ On-chain balance query failed (err0=%v, err1=%v), applying 1m cooldown", id, balErr0, balErr1)
								// panicBuyCooldown is assigned at the end of the block
							} else if settled && bal0 >= 0.01 && bal1 >= 0.01 {
								// Both balances arrived, try to merge them safely instead of dumping them to market
								actualMin := math.Min(bal0, bal1)
								_, err := trader.MergeOnChain(cleanupCtx, market.ConditionID, actualMin, len(market.Tokens))
								if err != nil {
									tui.LogEvent("[%s] ⚠️ Delayed Merge failed: %v", id, err)
									// Fallback to sell below
								} else {
									tui.LogEvent("[%s] ✅ Delayed Merge successful! Applying 30s cooldown.", id)
									// panicBuyCooldown is assigned at the end of the block
									// Clean up any remaining dust below
									bal0 -= actualMin
									bal1 -= actualMin
								}
							}

							// If not settled via merge, or if dust remains, clean it up via Market Sell
							tui.LogEvent("[%s] 🧹 Auto-cleanup: Checking for unbalanced shares to sell... Balances: %s=%.2f, %s=%.2f", id, outcomes[0], bal0, outcomes[1], bal1)

							var sell0Err, sell1Err error
							if bal0 >= 0.01 {
								tui.LogEvent("[%s] 🧹 Auto-cleanup: Market selling %.2f %s shares", id, bal0, outcomes[0])
								_, sell0Err = trader.Sell(cleanupCtx, token0, outcomes[0], 0.01, bal0, api.OrderTypeMarket, api.TIFFillAndKill, cfg.FeeRateBps)
								if sell0Err != nil && strings.Contains(sell0Err.Error(), "min size") {
									tui.LogEvent("[%s] ⚠️ Kept %.2f %s shares as dust (Value under $1.00 minimum limit)", id, bal0, outcomes[0])
									sell0Err = nil // Treat dust as 'successfully handled' so we don't spam retries
								}
							}
							if bal1 >= 0.01 {
								tui.LogEvent("[%s] 🧹 Auto-cleanup: Market selling %.2f %s shares", id, bal1, outcomes[1])
								_, sell1Err = trader.Sell(cleanupCtx, token1, outcomes[1], 0.01, bal1, api.OrderTypeMarket, api.TIFFillAndKill, cfg.FeeRateBps)
								if sell1Err != nil && strings.Contains(sell1Err.Error(), "min size") {
									tui.LogEvent("[%s] ⚠️ Kept %.2f %s shares as dust (Value under $1.00 minimum limit)", id, bal1, outcomes[1])
									sell1Err = nil // Treat dust as 'successfully handled' so we don't spam retries
								}
							}

							if (bal0 < 0.01 || sell0Err == nil) && (bal1 < 0.01 || sell1Err == nil) {
								// Record estimated loss from cleanup sells (sold at market floor $0.01)
								cleanupLoss := 0.0
								if bal0 >= 0.01 && sell0Err == nil {
									cleanupLoss += bal0 * (price1 - 0.01) // Lost difference vs buy price
								}
								if bal1 >= 0.01 && sell1Err == nil {
									cleanupLoss += bal1 * (price2 - 0.01)
								}
								if cleanupLoss > 0 {
									trader.RecordLoss(cleanupLoss)
									tui.LogEvent("[%s] 📉 Cleanup loss recorded: $%.2f", id, cleanupLoss)
								}
								tui.LogEvent("[%s] ✅ Auto-cleanup routine finished! Applying 30s cooldown before unblocking.", id)
								panicBuyCooldown = time.Now().Add(30 * time.Second)
							} else {
								tui.LogEvent("[%s] 🚫 Auto-cleanup failed! Applying 2m cooldown to prevent immediate retry loops.", id)
								panicBuyCooldown = time.Now().Add(120 * time.Second)
							}
							cancelCleanup() // Release cleanup context resources
						} // If both failed, nothing to record

						// Force refresh balance after trade to ensure accurate tracking
						if newBal, err := trader.ForceRefreshBalance(ctx); err == nil {
							currentBalance = newBal
							// currentCash = newBal // Unused
						}

						lastTrade = time.Now()
					}
				}
			}
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func handleRestFallbackWithDepth(ctx context.Context, id string, tokenMap map[string]string, bids, asks map[string]float64, fullBids, fullAsks map[string][]paper.MarketLevel, engine *paper.Engine, restClient *api.RestClient, tui *paper.TUI) bool {
	success := false
	for tokenID, outcome := range tokenMap {
		start := time.Now()
		// Use a short 2s timeout for fallback to prevent freezing the main loop when internet is down
		reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		book, err := restClient.GetOrderBook(reqCtx, tokenID)
		latency := time.Since(start)
		cancel()

		// Update TUI with real REST latency
		tui.UpdateRestLatency(latency)

		if err != nil {
			// If one request fails (likely due to no internet), break immediately to prevent further blocking
			break
		}

		// Parse timestamp for freshness check (removed logic)

		bid, ask := 0.0, 0.0
		for _, b := range book.Bids {
			p, _ := strconv.ParseFloat(b.Price, 64)
			if p > 0 && p < 1.0 && p > bid {
				bid = p
			}
		}
		for _, a := range book.Asks {
			p, _ := strconv.ParseFloat(a.Price, 64)
			if p > 0 && p < 1.0 && (ask == 0 || p < ask) {
				ask = p
			}
		}

		// Reject crossed books from REST
		if bid > 0 && ask > 0 && bid >= ask {
			// Clear crossed book state
			bids[outcome] = 0
			asks[outcome] = 0
			fullBids[outcome] = nil
			fullAsks[outcome] = nil
			success = true // Important: ensure UI updates to 0 (--.-)
			continue
		}
		
		// REST is absolute state. If it's missing a side, that side is 0.
		bids[outcome] = bid
		asks[outcome] = ask
		success = true
		
		if bid > 0 && ask > 0 {
			mid := (bid + ask) / 2
			engine.UpdateMarketData(id, outcome, mid, bid, ask)
		}
		// ALWAYS update full depth (liquidity) if newer, as REST is our primary source
		// for recovering from stale or dropped WS states.
		fullBids[outcome] = mkt.LevelsToPriceDepth(book.Bids, true)
		fullAsks[outcome] = mkt.LevelsToPriceDepth(book.Asks, false)
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
			result := engine.RedeemWithDetails(id, winner)
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
				// This is disabled as requested since it takes a long time.
				// The user can use the manual tools to redeem later.
				tui.LogEvent("[%s] ℹ️ Skipping auto-redeem. Use manual tool to claim.", id)
				/*
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
				*/
			} else {
				tui.LogEvent("[%s] 📭 Market resolved: %s (no positions)", id, winner)
			}
			return
		}

		tui.LogEvent("[%s] ⏳ Resolution pending... (attempt %d/%d)", id, attempt+1, len(retryDelays))
	}

	tui.LogEvent("[%s] ⚠️ Could not get resolution after %d attempts", id, len(retryDelays))
}
