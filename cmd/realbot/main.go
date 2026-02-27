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
			markets, err := restClient.GetMarketsByTimeframe(cleanupCtx, nil, "15m")
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
		// If we receive another interrupt during cleanup, force exit
		go func() {
			<-ctx.Done()
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
		MinAskPrice:          cfg.MinAskPrice,
		MaxAskPrice:          cfg.MaxAskPrice,
		SplitInitialCapPct:   cfg.SplitInitialCapPct,
		SplitReplenishCapPct: cfg.SplitReplenishCapPct,
	}, func(s paper.TUISettings) {
		cfg.MarketSlug = s.MarketSlug
		cfg.MaxMarkets = s.MaxMarkets
		cfg.Timeframe = s.Timeframe
		cfg.TradeScaleFactor = s.TradeScaleFactor
		cfg.MinMarginPercent = s.MinMarginPercent
		cfg.SplitMinMarginSell = s.SplitMinMarginSell
		cfg.SplitStrategyEnabled = s.SplitStrategyEnabled
		cfg.MinAskPrice = s.MinAskPrice
		cfg.MaxAskPrice = s.MaxAskPrice
		cfg.SplitInitialCapPct = s.SplitInitialCapPct
		cfg.SplitReplenishCapPct = s.SplitReplenishCapPct
		_ = cfg.SaveSettings()
	})
	tui.SetTradeFactor(cfg.TradeScaleFactor)

	// Start TUI — pass stop so a single Ctrl+C / [q] quits cleanly.
	if UseLiveUI {
		tui.StartRenderLoop(250*time.Millisecond, stop)
		defer tui.Stop()
	}

	// REST API connectivity is not measured separately — the header now shows
	// WS feed freshness instead. REST is only used for order execution, not pricing.

	// Main trading loop - Keep running: after each round of markets ends, search for new ones.
	globalSplitStatus := make(map[string]bool)
	var splitMu sync.Mutex
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
				balance = currentBalance
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
				defer func() {
					if r := recover(); r != nil {
						core.RestoreTerminal()
						stack := make([]byte, 4096)
						length := runtime.Stack(stack, false)
						fmt.Printf("\n🚨 TRADER PANIC [%s]: %v\n%s\n", id, r, stack[:length])
						emergencyCleanup()
					}
				}()
				tradeMarket(ctx, id, m, end, realTrader, engine, orderBook, r, tui, restClient, cfg, bal, globalSplitStatus, &splitMu)
			}(assetID, market, endTime, marketRiskMgr, currentBalance)
		}

		// Wait for all markets in this round to finish
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
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
			tui.LogEvent("🔄 All markets closed — searching for next round...")
			// Release stale keep-alive connections before the next search phase.
			restClient.CloseIdleConnections()
			tui.ClearMarkets()
			orderBook.CancelAllOrders()
			engine.ClearMarketData()
			// Loop back to search for new markets

		case <-ctx.Done():
			goto shutdown
		}
	}

shutdown:
	tui.Stop()
	fmt.Println("\n👋 Bot stopped.")
	emergencyCleanup()
	return nil
}

func viewMarketsOnly(cfg *core.Config, trader *trading.RealTrader) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	restClient := api.NewRestClient("")

	fmt.Println()
	fmt.Println("🔍 Searching for active markets...")

	markets, err := restClient.GetMarketsByTimeframe(ctx, nil, "15m")
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

	// feeRateCache stores the most recently fetched fee rate per tokenID.
	// It is refreshed before every trade so we always use the live market rate.
	// The API returns the required taker fee in basis points (e.g. 1000 = 10%).
	// A return value of 0 from the API means the market is fee-free — that is
	// a valid rate and must NOT be replaced with a non-zero fallback.
	// The hard fallback of 1000 bps is only used when the API call itself fails.
	type feeEntry struct {
		rate      int
		fetchedAt time.Time
	}
	feeCache := make(map[string]*feeEntry) // tokenID → entry
	feeCacheTTL := 30 * time.Second

	// getFeeRate returns the live taker fee for tokenID, refreshing the cache
	// if the entry is absent or older than feeCacheTTL.
	getFeeRate := func(tokenID string) int {
		if e, ok := feeCache[tokenID]; ok && time.Since(e.fetchedAt) < feeCacheTTL {
			return e.rate
		}
		rateCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		rate, err := restClient.GetFeeRate(rateCtx, tokenID)
		cancel()
		if err != nil {
			// API unreachable — use 1000 bps (10%) which is the standard taker
			// fee for 15m markets per the Polymarket CLOB documentation.
			if e, ok := feeCache[tokenID]; ok {
				return e.rate // return last-known good value if available
			}
			return 1000
		}
		feeCache[tokenID] = &feeEntry{rate: rate, fetchedAt: time.Now()}
		tui.LogEvent("[%s] 💸 Fee rate %s: %d bps (%.2f%%)", id, tokenID[:8]+"…", rate, float64(rate)/100.0)
		return rate
	}

	// Build tokenID lookup: outcome → tokenID (reverse of tokenMap)
	outcomeToTokenID := make(map[string]string, len(tokenMap))
	for tid, outcome := range tokenMap {
		outcomeToTokenID[outcome] = tid
	}

	// Pre-warm the cache for all tokens so the first trade has the correct rate.
	for tid, outcome := range tokenMap {
		rate := getFeeRate(tid)
		tui.LogEvent("[%s] ℹ️ Fee rate for %s: %d bps (%.2f%%)", id, outcome, rate, float64(rate)/100.0)
	}

	tokenBids := make(map[string]float64)
	tokenAsks := make(map[string]float64)
	tokenFullBids := make(map[string][]paper.MarketLevel)
	tokenFullAsks := make(map[string][]paper.MarketLevel)
	lastUpdateTs := make(map[string]int64) // outcome -> unix nano timestamp
	lastUpdate := time.Now()
	lastTrade := time.Time{}
	lastSplitSell := time.Time{}    // Track last split sell to avoid rapid-fire
	nextSplitAttempt := time.Time{} // Cooldown for retrying failed splits
	leggedPanicBuy := false         // Set true if a legged position couldn't be recovered — blocks further buys
	var panicBuyCooldown time.Time  // Cooldown for panic buys after successful auto-cleanup

	// Initial balance tracking
	currentBalance := startingBalance
	// currentCash := startingBalance // Unused after removing balance checks

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

					// Guard: only persist valid (0,1) prices.
					if bid > 0 && bid < 1.0 {
						tokenBids[outcome] = bid
					}
					if ask > 0 && ask < 1.0 {
						tokenAsks[outcome] = ask
					}

						// Always update full depth from snapshots
						tokenFullBids[outcome] = mkt.LevelsToPriceDepth(b.Bids)
						tokenFullAsks[outcome] = mkt.LevelsToPriceDepth(b.Asks)

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

				// Update best bids/asks based on the new full depth.
				// tokenToOutcome is keyed by tokenID; we range over it to get
				// the canonical outcome name for each token, then look up depth
				// by that outcome name (which is how tokenFullBids/Asks are keyed).
				//
				// IMPORTANT: only overwrite the stored best price when the depth has
				// a valid level available. If a price_change delta removed the last
				// level on one side, leave the previous best price in place rather
				// than writing 0 — a momentarily empty side is a transient book
				// state, not a real price of $0.
				for _, outcome := range tokenToOutcome {
					bids := tokenFullBids[outcome]
					if len(bids) > 0 && bids[0].Price > 0 {
						tokenBids[outcome] = bids[0].Price
					}
					// else: keep previous best bid — do NOT zero it out

					asks := tokenFullAsks[outcome]
					if len(asks) > 0 && asks[0].Price > 0 && asks[0].Price < 1.0 {
						tokenAsks[outcome] = asks[0].Price
					}
					// else: keep previous best ask — do NOT zero it out

					if tokenBids[outcome] > 0 && tokenAsks[outcome] > 0 {
						mid := (tokenBids[outcome] + tokenAsks[outcome]) / 2
						engine.UpdateMarketData(id, outcome, mid, tokenBids[outcome], tokenAsks[outcome])
					}
				}

					if foundForThisMarket {
						lastUpdate = time.Now()
					}
				}
			default:
				goto doneWS
			}
		}
	doneWS:

		if messagesProcessed > 0 {
			tui.UpdateMarketPricesWithSource(id, tokenBids, tokenAsks, "WS")
			// Push full depth to TUI so the order-book panel and depth-based
			// margin calculations reflect live WS liquidity, not just best prices.
			if len(tokenFullBids) > 0 || len(tokenFullAsks) > 0 {
				bidDepth := make(map[string][]paper.MarketLevel, len(tokenFullBids))
				askDepth := make(map[string][]paper.MarketLevel, len(tokenFullAsks))
				for outcome, levels := range tokenFullBids {
					cp := make([]paper.MarketLevel, len(levels))
					copy(cp, levels)
					bidDepth[outcome] = cp
				}
				for outcome, levels := range tokenFullAsks {
					cp := make([]paper.MarketLevel, len(levels))
					copy(cp, levels)
					askDepth[outcome] = cp
				}
				tui.UpdateOrderBookDepth(id, bidDepth, askDepth)
			}
		}

		// ============ REST FALLBACK ============
		// WS is primary for liquidity data via full depth updates and deltas.
		// Only poll REST if WS is unhealthy or stale.
		staleTime := time.Since(lastUpdate)

		// Update WS staleness and ping latency in TUI
		wsTimeSinceMsg := wsMgr.TimeSinceLastMessage()
		tui.UpdateWSLatency(wsTimeSinceMsg)
		tui.UpdateWSPingLatency(wsMgr.PingLatency())

		wsUnhealthy := !wsMgr.IsConnected() || wsTimeSinceMsg > 10*time.Second
		if wsUnhealthy && staleTime > 3*time.Second {
			// Note: REST fallback updated to also capture full depth
			if handleRestFallbackWithDepth(ctx, id, tokenMap, tokenBids, tokenAsks, tokenFullBids, tokenFullAsks, lastUpdateTs, engine, restClient, tui) {
				lastUpdate = time.Now()
				// Push REST depth to TUI after fallback refresh
				if len(tokenFullBids) > 0 || len(tokenFullAsks) > 0 {
					bidDepth := make(map[string][]paper.MarketLevel, len(tokenFullBids))
					askDepth := make(map[string][]paper.MarketLevel, len(tokenFullAsks))
					for outcome, levels := range tokenFullBids {
						cp := make([]paper.MarketLevel, len(levels))
						copy(cp, levels)
						bidDepth[outcome] = cp
					}
					for outcome, levels := range tokenFullAsks {
						cp := make([]paper.MarketLevel, len(levels))
						copy(cp, levels)
						askDepth[outcome] = cp
					}
					tui.UpdateOrderBookDepth(id, bidDepth, askDepth)
				}
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

				// Scale initial buffer based on balance: 2x trade size, but at least $2 and at most SplitInitialCapPct of balance
				initialBuffer := baseTradeSize * 2.0
				if initialBuffer < 2.0 {
					initialBuffer = 2.0
				}

				initialCapPct := liveCfg.SplitInitialCapPct
				if initialCapPct <= 0 {
					initialCapPct = 0.25 // fallback default
				}
				maxInitial := currentBalance * initialCapPct
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

			replenishCapPct := liveCfg.SplitReplenishCapPct
			if replenishCapPct <= 0 {
				replenishCapPct = 0.50 // fallback default
			}
			decision := replenishCtrl.CheckReplenish(paper.ReplenishParams{
				CurrentShares:      currentShares,
				TargetBuffer:       targetBuffer,
				InitialShares:      initialSplitAmount, // Replenish back to initial amount
				SellMargin:         sellMargin,
				MinMarginThreshold: cfg.SplitMinMarginSell - 1.0,
				CurrentBalance:     currentBalance,
				ReplenishAmount:    replenishAmount,
				MaxBalancePercent:  replenishCapPct,
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
						sort.Slice(sortedBids1, func(a, b int) bool { return sortedBids1[a].Price > sortedBids1[b].Price })

						sortedBids2 := make([]paper.MarketLevel, len(bids2))
						copy(sortedBids2, bids2)
						sort.Slice(sortedBids2, func(a, b int) bool { return sortedBids2[a].Price > sortedBids2[b].Price })

						var rawLiq1, rawLiq2, matchedBidLiq float64
						var maxValidI, maxValidJ int

						for bi, bj := 0, 0; bi < len(sortedBids1) && bj < len(sortedBids2); {
							if sortedBids1[bi].Price+sortedBids2[bj].Price < minSum {
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

							// Sync CLOB allowance with on-chain state right before trading.
							// This is the root cause of "insufficient balance/allowance" errors:
							// the CLOB loses sync with on-chain state between startup and trade time.
							// utilbot does this at startup right before execution — we do it here
							// for every trade attempt so the CLOB is always current.
							if syncErr := trader.UpdateBalanceAllowance(ctx); syncErr != nil {
								tui.LogEvent("[%s] ⚠️ SPLIT: Allowance sync failed: %v (continuing)", id, syncErr)
							}

							var wg sync.WaitGroup
							wg.Add(2)

							var res1, res2 *trading.TradeResult
							var err1, err2 error

					// Use market orders for split selling — fetch live fee rate per token.
					rate0 := getFeeRate(token0)
					rate1 := getFeeRate(token1)
					go func() {
						defer wg.Done()
						res1, err1 = trader.Sell(ctx, token0, outcomes[0], 0.01, sharesToSell, api.OrderTypeMarket, api.TIFFillOrKill, rate0)
					}()

					go func() {
						defer wg.Done()
						res2, err2 = trader.Sell(ctx, token1, outcomes[1], 0.01, sharesToSell, api.OrderTypeMarket, api.TIFFillOrKill, rate1)
					}()

							wg.Wait()

							side1Success := err1 == nil && res1 != nil && res1.Success
							side2Success := err2 == nil && res2 != nil && res2.Success

							// ═══════════════════════════════════════════════════════════════
							// ONE-SHOT: No recovery - if unbalanced, log and exit
							// ═══════════════════════════════════════════════════════════════
							if side1Success != side2Success {
								// One succeeded, one failed - just log and move on
								failedOutcome := outcomes[0]
								if side1Success {
									failedOutcome = outcomes[1]
								}
								tui.LogEvent("[%s] ⚠️ SPLIT UNBALANCED: %s sold, %s failed. Skipping recovery (one-shot mode).", id,
									map[bool]string{true: outcomes[0], false: outcomes[1]}[side1Success], failedOutcome)
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

							// ONE-SHOT: Exit after successful sell
							tui.LogEvent("[%s] ✅ One-shot execution complete after successful split sell.", id)
							return
						} else {
							// Partial success - record to keep inventory accurate
							if side1Success {
								splitInventory.RecordSell(id, outcomes[0], sharesToSell, bid1)
								tui.LogEvent("[%s] ⚠️ SPLIT: Only %s sold (one-shot)", id, outcomes[0])
							}
							if side2Success {
								splitInventory.RecordSell(id, outcomes[1], sharesToSell, bid2)
								tui.LogEvent("[%s] ⚠️ SPLIT: Only %s sold (one-shot)", id, outcomes[1])
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
		// If we have an irrecoverable legged position, stop buying to prevent
		// accumulating more exposure on the already-filled side.
		if leggedPanicBuy {
			time.Sleep(100 * time.Millisecond)
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
						// currentCash = latestBalance // Unused
						currentBalance = latestBalance
					}

					// For real bot, equity = cash + market value of positions
					// Simplification: use cash balance as proxy for sizing, or fetch equity
					currentEquity := currentBalance // In realbot we use cash as conservative equity
					tradeSize := cfg.CalculateTradeSize(currentEquity)

				// Get max fee rate for conservative margin calculation.
				// Fetch live rates — cache hit is instant, miss re-fetches from API.
				feeR0 := getFeeRate(outcomeToTokenID[outcomes[0]])
				feeR1 := getFeeRate(outcomeToTokenID[outcomes[1]])
				maxFeeRateBps := feeR0
				if feeR1 > maxFeeRateBps {
					maxFeeRateBps = feeR1
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

						// TRUE MARKET EXECUTION: Set limit price to the absolute exchange
						// maximum (0.99) for BUY orders so the order behaves as a "true market"
						// order with maximum slippage tolerance. This guarantees X shares are
						// filled regardless of price movement, prioritizing execution volume
						// over price protection.
						limitPrice1 := 0.99
						limitPrice2 := 0.99

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
						// utilbot syncs right before execution; we mirror that here.
						if syncErr := trader.UpdateBalanceAllowance(ctx); syncErr != nil {
							tui.LogEvent("[%s] ⚠️ ARB: Allowance sync failed: %v (continuing)", id, syncErr)
						}

						var wg sync.WaitGroup
						wg.Add(2)

						var res1, res2 *trading.TradeResult
						var err1, err2 error

				// Fetch live fee rates immediately before order submission.
				buyRate0 := getFeeRate(token0)
				buyRate1 := getFeeRate(token1)
				go func() {
					defer wg.Done()
					res1, err1 = trader.Buy(ctx, token0, outcomes[0], limitPrice1, shares, api.OrderTypeMarket, api.TIFFillOrKill, buyRate0)
				}()

				go func() {
					defer wg.Done()
					res2, err2 = trader.Buy(ctx, token1, outcomes[1], limitPrice2, shares, api.OrderTypeMarket, api.TIFFillOrKill, buyRate1)
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
							// If we have the shares, we consider the side "filled" regardless of API error
							side1Success = bal0 >= shares
							side2Success = bal1 >= shares
						} else {
							tui.LogEvent("[%s] ⚠️ Failed to verify positions: %v (relying on API response)", id, verifyErr)
							side1Success = err1 == nil && res1 != nil && res1.Success
							side2Success = err2 == nil && res2 != nil && res2.Success
						}

						// Calculate costs using the original target price for reporting (actual will be better)
						cost1 := shares * price1
						cost2 := shares * price2

						// Log results based on VERIFIED state
						if side1Success {
							tui.LogEvent("[%s] ✅ Side 1 MARKET: %s (Target $%.3f)", id, outcomes[0], price1)
							tui.RecordOrder(id, outcomes[0], "BUY", shares, price1, cost1, margin, "FILLED")
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
							tui.RecordOrder(id, outcomes[0], "BUY", shares, price1, cost1, margin, "FAILED")
						}

						if side2Success {
							tui.LogEvent("[%s] ✅ Side 2 MARKET: %s (Target $%.3f)", id, outcomes[1], price2)
							tui.RecordOrder(id, outcomes[1], "BUY", shares, price2, cost2, margin, "FILLED")
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
							tui.RecordOrder(id, outcomes[1], "BUY", shares, price2, cost2, margin, "FAILED")
						}

						// ═══════════════════════════════════════════════════════════════
						// LEGGED SHARE RECOVERY: If one side filled and the other didn't,
						// wait 2 seconds for late settlement, re-verify positions, then
						// retry the missing side once to prevent a legged position.
						// ═══════════════════════════════════════════════════════════════
						if side1Success != side2Success {
							tui.LogEvent("[%s] ⚠️ ARB LEGGED: %s=%v %s=%v — waiting 2s then retrying missing side...",
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
								side1Success = rbal0 >= shares
								side2Success = rbal1 >= shares
								tui.LogEvent("[%s] 🔍 Re-verify after delay: %s=%.4f (%v→%v), %s=%.4f (%v→%v)",
									id, outcomes[0], rbal0, prevSide1, side1Success,
									outcomes[1], rbal1, prevSide2, side2Success)
							}

					// If still unbalanced, retry the order for the missing side
					if !side1Success {
						tui.LogEvent("[%s] 🔄 Retrying buy for missing side: %s...", id, outcomes[0])
						retryRes1, retryErr1 := trader.Buy(ctx, token0, outcomes[0], limitPrice1, shares, api.OrderTypeMarket, api.TIFFillOrKill, getFeeRate(token0))
							if retryErr1 == nil && retryRes1 != nil && retryRes1.Success {
								side1Success = true
								tui.LogEvent("[%s] ✅ Retry %s succeeded", id, outcomes[0])
							} else {
								// Final position check after retry
								if retryPos, retryErr := trader.GetPositions(ctx); retryErr == nil {
									for _, pos := range retryPos {
										if pos.TokenID == token0 && pos.Size >= shares {
											side1Success = true
										}
									}
								}
								if !side1Success {
									tui.LogEvent("[%s] ❌ Retry %s failed: %v", id, outcomes[0], retryErr1)
								}
							}
						}
					if !side2Success {
						tui.LogEvent("[%s] 🔄 Retrying buy for missing side: %s...", id, outcomes[1])
						retryRes2, retryErr2 := trader.Buy(ctx, token1, outcomes[1], limitPrice2, shares, api.OrderTypeMarket, api.TIFFillOrKill, getFeeRate(token1))
							if retryErr2 == nil && retryRes2 != nil && retryRes2.Success {
								side2Success = true
								tui.LogEvent("[%s] ✅ Retry %s succeeded", id, outcomes[1])
							} else {
								// Final position check after retry
								if retryPos, retryErr := trader.GetPositions(ctx); retryErr == nil {
									for _, pos := range retryPos {
										if pos.TokenID == token1 && pos.Size >= shares {
											side2Success = true
										}
									}
								}
								if !side2Success {
									tui.LogEvent("[%s] ❌ Retry %s failed: %v", id, outcomes[1], retryErr2)
								}
							}
						}

							// Final status after recovery
							if side1Success != side2Success {
								failedSide := outcomes[1]
								if !side1Success {
									failedSide = outcomes[0]
								}
								tui.LogEvent("[%s] ⚠️ ARB UNBALANCED after retry: %s still not filled (legged position recorded)", id, failedSide)
							} else if side1Success && side2Success {
								tui.LogEvent("[%s] ✅ Legged position recovered — both sides now filled", id)
							}
						}

						// NOW record to engine - only record positions that actually succeeded
						// This ensures engine state matches reality for accurate drawdown calculation
						if side1Success && side2Success {
							// Both sides filled (either initially or via recovery) - record both
							engine.BuyForMarket(id, outcomes[0], price1, shares)
							engine.BuyForMarket(id, outcomes[1], price2, shares)

							// ONE-SHOT: Execute merge and then EXIT
							tui.LogEvent("[%s] ⏳ Waiting 5s for position sync before merge...", id)
							time.Sleep(5 * time.Second)

							mergeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
							defer cancel()

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
								return // Exit tradeMarket
							}

							txHash, err := trader.MergeOnChain(mergeCtx, market.ConditionID, mergeQty)
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
									tui.LogEvent("[%s] 🧹 Auto-cleanup: Market selling %.2f excess %s shares", id, excess0, outcomes[0])
									_, sellErr := trader.Sell(mergeCtx, token0, outcomes[0], 0.10, excess0, api.OrderTypeMarket, api.TIFFillOrKill, cfg.FeeRateBps)
									if sellErr != nil {
										tui.LogEvent("[%s] ⚠️ Auto-cleanup sell failed for %s: %v", id, outcomes[0], sellErr)
									}
								}
								if excess1 >= 0.01 {
									tui.LogEvent("[%s] 🧹 Auto-cleanup: Market selling %.2f excess %s shares", id, excess1, outcomes[1])
									_, sellErr := trader.Sell(mergeCtx, token1, outcomes[1], 0.10, excess1, api.OrderTypeMarket, api.TIFFillOrKill, cfg.FeeRateBps)
									if sellErr != nil {
										tui.LogEvent("[%s] ⚠️ Auto-cleanup sell failed for %s: %v", id, outcomes[1], sellErr)
									}
								}
							}

							tui.LogEvent("[%s] ✅ One-shot execution complete after successful buy and merge.", id)
							return // Exit tradeMarket
						} else if side1Success || side2Success {
							// Only one side filled after retry — record the unbalanced position and
							// permanently block further panic buys to prevent exposure accumulation.
							if side1Success {
								engine.BuyForMarket(id, outcomes[0], price1, shares)
								tui.LogEvent("[%s] ⚠️ Engine: Recording unbalanced position (only %s)", id, outcomes[0])
							}
							if side2Success {
								engine.BuyForMarket(id, outcomes[1], price2, shares)
								tui.LogEvent("[%s] ⚠️ Engine: Recording unbalanced position (only %s)", id, outcomes[1])
							}

							cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 30*time.Second)
							defer cancelCleanup()

							bal0, bal1, balErr0, balErr1 := trader.QueryBalancedCTFBalances(cleanupCtx, token0, token1, shares)
							if balErr0 != nil || balErr1 != nil {
								tui.LogEvent("[%s] ⚠️ On-chain balance query failed (err0=%v, err1=%v), cannot safely cleanup", id, balErr0, balErr1)
								leggedPanicBuy = true
								tui.LogEvent("[%s] 🚫 Panic buy disabled for this market (legged position — holding until expiry)", id)
							} else {
								tui.LogEvent("[%s] 🧹 Legged trade detected! Balances: %s=%.6f, %s=%.6f", id, outcomes[0], bal0, outcomes[1], bal1)

								var sell0Err, sell1Err error
								if bal0 >= 0.01 {
									tui.LogEvent("[%s] 🧹 Auto-cleanup: Market selling %.2f %s shares", id, bal0, outcomes[0])
									_, sell0Err = trader.Sell(cleanupCtx, token0, outcomes[0], 0.10, bal0, api.OrderTypeMarket, api.TIFFillOrKill, cfg.FeeRateBps)
								}
								if bal1 >= 0.01 {
									tui.LogEvent("[%s] 🧹 Auto-cleanup: Market selling %.2f %s shares", id, bal1, outcomes[1])
									_, sell1Err = trader.Sell(cleanupCtx, token1, outcomes[1], 0.10, bal1, api.OrderTypeMarket, api.TIFFillOrKill, cfg.FeeRateBps)
								}

								if (bal0 < 0.01 || sell0Err == nil) && (bal1 < 0.01 || sell1Err == nil) {
									tui.LogEvent("[%s] ✅ Auto-cleanup successful! Applying 30s cooldown before unblocking.", id)
									panicBuyCooldown = time.Now().Add(30 * time.Second)
									leggedPanicBuy = false
								} else {
									leggedPanicBuy = true
									tui.LogEvent("[%s] 🚫 Auto-cleanup failed! Panic buy disabled for this market.", id)
								}
							}
						}
						// If both failed, nothing to record

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

		// Update prices only if newer AND value is valid (0,1).
		if isNewer {
			if bid > 0 && bid < 1.0 {
				bids[outcome] = bid
				success = true
			}
			if ask > 0 && ask < 1.0 {
				asks[outcome] = ask
				success = true
			}
			if bid > 0 && ask > 0 {
				mid := (bid + ask) / 2
				engine.UpdateMarketData(id, outcome, mid, bid, ask)
			}
		}

		// ALWAYS update full depth (liquidity), as REST is our primary source for this
		// and stale liquidity is better than no liquidity for safety checks
		fullBids[outcome] = mkt.LevelsToPriceDepth(book.Bids)
		fullAsks[outcome] = mkt.LevelsToPriceDepth(book.Asks)
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
