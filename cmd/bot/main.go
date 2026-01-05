package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
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
)

const (
	StartingBalance = 1000.0 // $1000 paper trading balance
	UseLiveUI       = true   // Set to false for traditional logging
)

// MarketTrader holds state for trading a single market
type MarketTrader struct {
	ID         string // "ETH" or "SOL"
	Market     *api.Market
	Engine     *paper.Engine
	OrderBook  *paper.OrderBook
	LadderMgr  *paper.LadderManager
	RiskMgr    *paper.RiskManager
	Monitor    *paper.MarketMonitor
	TokenMap   map[string]string // tokenID -> outcome
	Outcomes   []string
	EndTime    time.Time
	RestClient *api.RestClient
	WSMgr      *api.WSManager
	TUI        *paper.TUI      // Shared TUI
	CSVLogger  *core.CSVLogger // Optional CSV diagnostic logger
	Config     *core.Config    // Config for position sizing

	// Price tracking
	TokenBids     map[string]float64
	TokenAsks     map[string]float64
	TokenFullBids map[string][]paper.MarketLevel
	TokenFullAsks map[string][]paper.MarketLevel
	FloatPrices   map[string]float64

	// Last time ANY price update was received for this trader
	LastUpdate time.Time

	// Last time we performed a REST fallback poll
	LastRestPoll time.Time

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
	fmt.Print("\033[?25h")   // Show cursor
	fmt.Print("\033[?1049l") // Exit alternate screen buffer if active
	fmt.Println()
}

// logEvent is a helper to log to both TUI and CSV logger safely
func logEvent(tui *paper.TUI, csv *core.CSVLogger, engine *paper.Engine, level, asset, event, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	if tui != nil {
		tui.LogEvent("%s", msg)
	}
	if csv != nil {
		equity := 0.0
		if engine != nil {
			equity = engine.GetEquity()
		}
		csv.Log(level, asset, event, msg, equity)
	}
}

func run() error {
	var engine *paper.Engine
	var csvLogger *core.CSVLogger

	// Setup signal handling with immediate terminal restore
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Global panic recovery - restore terminal on any panic
	defer func() {
		if r := recover(); r != nil {
			restoreTerminal()
			stack := make([]byte, 4096)
			length := runtime.Stack(stack, false)
			fmt.Printf("\n🚨 PANIC RECOVERED: %v\n%s\n", r, stack[:length])
			if csvLogger != nil {
				equity := 0.0
				if engine != nil {
					equity = engine.GetEquity()
				}
				csvLogger.Log("CRITICAL", "SYSTEM", "PANIC", fmt.Sprintf("%v", r), equity)
			}
		}
	}()

	// Watchdog: Force exit after signal if graceful shutdown takes too long
	// This ensures we never get stuck even if goroutines are blocked
	go func() {
		<-ctx.Done()
		// Give graceful shutdown 5 seconds, then force exit
		time.Sleep(5 * time.Second)
		restoreTerminal()
		fmt.Println("\n⚠️ Force exit: graceful shutdown timed out")
		os.Exit(1)
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
	engine = paper.NewEngine(StartingBalance)

	// Load config
	cfg, err := core.LoadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	fmt.Println("✅ Config loaded successfully")
	_ = cfg // Reserved for future use

	restClient := api.NewRestClient("")

	// Create shared order book and TUI (persistent across market rotations)
	orderBook := paper.NewOrderBook()
	tui := paper.NewTUI(engine, orderBook)

	// Initialize CSV Logger for long-term diagnostics
	csvLogger, err = core.NewCSVLogger("bot_activity.csv")
	if err != nil {
		fmt.Printf("⚠️ Warning: Could not initialize CSV logger: %v\n", err)
	} else {
		defer csvLogger.Close()
	}
	logEvent(tui, csvLogger, engine, "INFO", "SYSTEM", "STARTUP", "Bot starting with multi-asset support")

	// Start TUI render loop
	if UseLiveUI {
		tui.StartRenderLoop(250 * time.Millisecond) // Fast refresh for live updates
		defer tui.Stop()
	}

	// Goroutine monitor and memory cleanup
	go func() {
		lastCount := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(60 * time.Second):
				count := runtime.NumGoroutine()
				// Only warn if goroutine count is extremely high (likely leak)
				if count > 200 {
					tui.LogEvent("⚠️ High goroutine count: %d", count)
					if csvLogger != nil {
						csvLogger.Log("WARN", "SYSTEM", "HIGH_GOROUTINES", fmt.Sprintf("Count: %d", count), engine.GetEquity())
					}
					runtime.GC()
				}
				lastCount = count
				_ = lastCount // Silence unused warning

				// Periodic memory cleanup - remove old filled/cancelled orders
				orderBook.CleanupOldOrders(5 * time.Minute)
			}
		}
	}()

	// Android background keepalive - prevents OS from throttling when alt-tabbed
	// Performs lightweight work every 500ms to maintain activity
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Lightweight activity to prevent Android throttling
				// Just reading the time is enough to keep the process active
				_ = time.Now().UnixNano()
			}
		}
	}()

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
				if csvLogger != nil {
					csvLogger.Log("INFO", "SYSTEM", "LIQUIDATION", fmt.Sprintf("Proceeds: %.2f", proceeds), engine.GetEquity())
				}
			}

			stats := engine.GetStats()
			fmt.Printf("📊 Final Stats: Balance $%.2f | Realized PnL $%.2f | Trades %d\n",
				stats.CurrentBalance, stats.RealizedPnL, stats.TotalTrades)
			return nil
		default:
		}

		// Find all available markets (BTC, ETH, SOL, XRP)
		logEvent(tui, csvLogger, engine, "INFO", "SYSTEM", "MARKET_SEARCH", "Searching for active 15m markets...")
		markets := findMarkets(ctx, restClient, tui)
		if len(markets) == 0 {
			logEvent(tui, csvLogger, engine, "WARN", "SYSTEM", "NO_MARKETS", "No active markets found, retrying...")
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
		logEvent(tui, csvLogger, engine, "INFO", "SYSTEM", "ROUND_START", "Round starting with %d markets | Multiplier: %.2fx", len(markets), compoundMultiplier)

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
			// Reduced logging: Only TUI for startup info
			tui.LogEvent("🚀 Trading %s: %s", assetID, market.Slug)

			trader := createTrader(assetID, market, engine, orderBook, restClient, tui, outcomes, endTime, csvLogger, cfg)
			wg.Add(1)
			tradersStarted++
			go func(id string, t *MarketTrader) {
				defer wg.Done()
				// Create a sub-context for this specific trader to prevent goroutine leaks
				tCtx, tCancel := context.WithCancel(ctx)
				defer tCancel()

				// Panic recovery for trader goroutine
				defer func() {
					if r := recover(); r != nil {
						stack := make([]byte, 4096)
						length := runtime.Stack(stack, false)
						logEvent(t.TUI, t.CSVLogger, t.Engine, "CRITICAL", id, "PANIC", "Panic: %v\n%s", r, stack[:length])
						errors <- fmt.Errorf("%s: panic: %v", id, r)
					}
				}()
				result, err := runTrader(tCtx, t)
				if err != nil {
					logEvent(t.TUI, t.CSVLogger, t.Engine, "ERROR", id, "TRADER_ERROR", "Trader failed: %v", err)
					errors <- fmt.Errorf("%s: %w", id, err)
					return
				}
				results <- result
			}(assetID, trader)
		}

		logEvent(tui, csvLogger, engine, "INFO", "SYSTEM", "TRADERS_RUNNING", "Started %d concurrent market traders", tradersStarted)

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

		// Clear old market data immediately and start searching
		tui.LogEvent("🔄 Market round complete, searching for new markets...")
		tui.ClearMarkets()
		orderBook.CancelAllOrders()
	}
}

// findMarkets searches for BTC, ETH, SOL, XRP 15m markets
func findMarkets(ctx context.Context, restClient *api.RestClient, tui *paper.TUI) map[string]*api.Market {
	found := make(map[string]*api.Market)
	assets := []string{"btc", "eth", "sol", "xrp"}

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
			// Parse end time and skip if market already expired
			endTime, err := paper.ParseEndTimeFromSlug(m.Slug)
			if err == nil && time.Now().After(endTime) {
				// Market already expired, skip it
				continue
			}

			// Also skip markets that are about to expire (less than 30 seconds)
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

		// Return if we found at least one market
		if len(found) > 0 {
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
	return outcomes
}

func createTrader(id string, market *api.Market, engine *paper.Engine, orderBook *paper.OrderBook, restClient *api.RestClient, tui *paper.TUI, outcomes []string, endTime time.Time, csvLogger *core.CSVLogger, cfg *core.Config) *MarketTrader {
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
		MaxExposure:        2000.0,
		MaxUnmatchedRatio:  0.40,
		MaxUnmatchedShares: 300.0,
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
		CSVLogger:     csvLogger,
		Config:        cfg,
		TokenBids:     make(map[string]float64),
		TokenAsks:     make(map[string]float64),
		TokenFullBids: make(map[string][]paper.MarketLevel),
		TokenFullAsks: make(map[string][]paper.MarketLevel),
		FloatPrices:   make(map[string]float64),
		LastUpdate:    time.Now(),
		LastRestPoll:  time.Now(),
	}
}

func runTrader(ctx context.Context, t *MarketTrader) (*marketResult, error) {
	// Setup WebSocket with retry
	wsMgr := api.NewWSManager("")
	var wsErr error
	for attempt := 1; attempt <= 3; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		wsErr = wsMgr.Connect(ctx)
		if wsErr == nil {
			break
		}
		logEvent(t.TUI, t.CSVLogger, t.Engine, "WARN", t.ID, "WS_CONNECT_FAIL", "WS connect attempt %d failed: %v", attempt, wsErr)
		if attempt < 3 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
	}
	if wsErr != nil {
		return nil, fmt.Errorf("websocket connect failed after 3 attempts: %w", wsErr)
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
	t.TUI.LogEvent("[%s] 📡 WebSocket streaming started", t.ID)

	// Order fill callback
	t.OrderBook.SetFillCallback(func(order *paper.LimitOrder, fillQty, fillPrice float64) {
		_, err := t.Engine.Buy(order.Outcome, fillPrice, fillQty)
		if err != nil {
			t.TUI.LogEvent("[%s] ❌ Fill error: %v", t.ID, err)
			if t.CSVLogger != nil {
				t.CSVLogger.Log("ERROR", t.ID, "FILL_ERROR", err.Error(), t.Engine.GetEquity())
			}
			return
		}
		saved := order.Price - fillPrice
		t.TUI.LogEvent("[%s] ✅ FILL %s %.0f @ $%.3f (saved $%.3f)", t.ID, order.Outcome, fillQty, fillPrice, saved)
		if t.CSVLogger != nil {
			t.CSVLogger.Log("TRADE", t.ID, "FILL", fmt.Sprintf("%s %.0f @ $%.3f", order.Outcome, fillQty, fillPrice), t.Engine.GetEquity())
		}
	})

	// Track starting realized PnL
	startingRealizedPnL := t.Engine.GetStats().RealizedPnL
	tradesAtStart := t.Engine.GetStats().TotalTrades

	tokenPrices := make(map[string]string)
	lastReconnectCount := int32(0)    // Track reconnections
	lastWsWarnTime := time.Time{}     // Rate-limit WS warnings
	lastForceReconnect := time.Time{} // Track forced reconnection attempts

	const wsWarnInterval = 15 * time.Second   // Only warn once per 15 seconds
	const wsForceReconnect = 15 * time.Second // Force reconnection after 15 seconds stale

	// Track WebSocket channel closure state (outside loop to persist across ticks)
	wsChannelClosed := false

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
				logEvent(t.TUI, t.CSVLogger, t.Engine, "WARN", t.ID, "TIMEOUT", "SAFETY TIMEOUT - Forcing market exit")
				t.LadderMgr.CancelAllLadders()

				// Use more robust resolution simulation
				winner := t.determineWinner()
				if winner != "" {
					logEvent(t.TUI, t.CSVLogger, t.Engine, "INFO", t.ID, "TIMEOUT_RESOLVE", "Timeout resolution: %s", winner)
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
				logEvent(t.TUI, t.CSVLogger, t.Engine, "INFO", t.ID, "EXPIRED", "MARKET EXPIRED - resolving immediately")

				// Use more robust resolution simulation
				winner := t.determineWinner()
				logEvent(t.TUI, t.CSVLogger, t.Engine, "INFO", t.ID, "WINNER", "WINNER: %s", winner)

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

				if t.CSVLogger != nil {
					t.CSVLogger.Log("INFO", t.ID, "REDEEM", fmt.Sprintf("Winner: %s, PnL: %.2f", winner, result.TotalPnL), t.Engine.GetEquity())
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

			// ============ WEBSOCKET-ONLY PRICE UPDATES (FASTEST) ============
			// Check for WebSocket reconnection and log it
			_, _, reconnects, _ := wsMgr.GetStats()
			if reconnects > lastReconnectCount {
				t.TUI.LogEvent("[%s] 🔄 WebSocket reconnected (attempt #%d)", t.ID, reconnects)
				if t.CSVLogger != nil {
					t.CSVLogger.Log("INFO", t.ID, "WS_RECONNECT", fmt.Sprintf("Attempt #%d", reconnects), t.Engine.GetEquity())
				}
				lastReconnectCount = reconnects
			}

			// Process ALL available WebSocket messages (real-time, non-blocking)
			// This drains the channel to get the latest data
			messagesProcessed := 0
			for {
				select {
				case msg, ok := <-wsMsgChan:
					if !ok {
						// Channel closed, WebSocket done
						wsChannelClosed = true
						goto doneProcessingWS
					}
					messagesProcessed++

					// Parse and process WebSocket message immediately
					if books, err := api.ParseOrderBooks(msg); err == nil && len(books) > 0 && books[0].AssetID != "" {
						foundForThisTrader := false
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
								foundForThisTrader = true
								t.TokenBids[outcome] = bid
								t.TokenAsks[outcome] = ask
								if bid > 0 && ask > 0 && ask < 1.0 {
									mid := (bid + ask) / 2
									t.FloatPrices[outcome] = mid
									tokenPrices[outcome] = fmt.Sprintf("%.3f", mid)
									// Use batch update for better performance
									t.Engine.UpdateMarketData(t.ID, outcome, mid, bid, ask)
								}
								t.TokenFullBids[outcome] = toMarketLevels(t.TUI, t.ID, b.Bids)
								t.TokenFullAsks[outcome] = toMarketLevels(t.TUI, t.ID, b.Asks)
							}
						}
						if foundForThisTrader {
							t.LastUpdate = time.Now()
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
							t.LastUpdate = time.Now()
							t.TokenBids[outcome] = bid
							t.TokenAsks[outcome] = ask
							if bid > 0 && ask > 0 && ask < 1.0 {
								mid := (bid + ask) / 2
								t.FloatPrices[outcome] = mid
								tokenPrices[outcome] = fmt.Sprintf("%.3f", mid)
								// Use batch update for better performance
								t.Engine.UpdateMarketData(t.ID, outcome, mid, bid, ask)
							}
							t.TokenFullBids[outcome] = toMarketLevels(t.TUI, t.ID, book.Bids)
							t.TokenFullAsks[outcome] = toMarketLevels(t.TUI, t.ID, book.Asks)
						}
					}
				default:
					// No more messages in channel, continue with rest of loop
					goto doneProcessingWS
				}
			}
		doneProcessingWS:

			// Update TUI after processing WS messages
			if messagesProcessed > 0 {
				t.TUI.UpdateMarketPrices(t.ID, t.TokenBids, t.TokenAsks)
			} else if wsMgr.IsConnected() && wsMgr.TimeSinceLastMessage() < 5*time.Second {
				// If WebSocket is healthy but no message this tick, "touch" to prevent stale warnings
				t.TUI.TouchMarket(t.ID)
			}

			// Check WebSocket connection health for REST fallback decision
			wsConnected := wsMgr.IsConnected()
			wsLastMsg := wsMgr.TimeSinceLastMessage()

			// Individual trader staleness watchdog
			// Only poll REST if WS is unhealthy (disconnected or no messages for 15s)
			// Note: t.LastUpdate tracks actual PRICE updates, not just connection health
			staleTime := time.Since(t.LastUpdate)
			restPollInterval := 2 * time.Second
			needsRestFallback := (!wsConnected || wsLastMsg > 15*time.Second) && staleTime > 3*time.Second

			if needsRestFallback && time.Since(t.LastRestPoll) > restPollInterval {
				t.handleRestFallback(ctx, tokenPrices, staleTime)
			}

			// FORCE RECONNECT: If stale for 15s (reduced from 30s for faster recovery)
			if time.Since(t.LastUpdate) > 15*time.Second {
				if time.Since(lastForceReconnect) > wsForceReconnect {
					t.TUI.LogEvent("[%s] ⚠️ STALE (15s) - forcing WS reconnect", t.ID)
					lastForceReconnect = time.Now()
					wsMgr.ForceReconnect()
				}
			}

			// Handle WebSocket issues - only reconnect if actually disconnected
			// (Inactive markets with no trades won't send data, but connection is still alive)
			if !wsMgr.IsConnected() && !wsChannelClosed {
				// WebSocket disconnected, force reconnection (rate-limited)
				if time.Since(lastForceReconnect) > wsForceReconnect {
					lastForceReconnect = time.Now()
					wsMgr.ForceReconnect()
					if time.Since(lastWsWarnTime) > wsWarnInterval {
						t.TUI.LogEvent("[%s] 🔌 WS disconnected - reconnecting...", t.ID)
						lastWsWarnTime = time.Now()
					}
				}
			}

			// If WebSocket channel closed, log once and try reconnect
			if wsChannelClosed && time.Since(lastWsWarnTime) > wsWarnInterval {
				t.TUI.LogEvent("[%s] ⚠️ WebSocket closed - attempting reconnect", t.ID)
				lastWsWarnTime = time.Now()
				wsMgr.ForceReconnect()
			}

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

			// Check if market has ended (only exit condition that matters)
			// DON'T exit on "liquidity dried up" - volatile markets can have extreme prices
			// and that's normal market behavior, not a reason to exit

			// Trading logic - check every tick for arbitrage opportunities
			if len(tokenPrices) == 2 && len(t.Outcomes) == 2 && marketState == paper.MarketStateActive {
				ask1 := t.TokenAsks[t.Outcomes[0]]
				ask2 := t.TokenAsks[t.Outcomes[1]]

				if ask1 >= 0.10 && ask1 <= 0.90 && ask2 >= 0.10 && ask2 <= 0.90 {
					sum := ask1 + ask2
					margin := (1.0 - sum) * 100

					// Use config for minimum margin (default 2%)
					minMarginPercent := t.Config.MinMarginPercent

					// Calculate dynamic trade size based on current balance
					// $1000 balance * 10% = $100 trade size
					// $100 balance * 10% = $10 trade size
					currentBalance := t.Engine.GetBalance()
					tradeSize := t.Config.CalculateTradeSize(currentBalance)
					baseSharesPerTrade := tradeSize / sum // Shares = $ / price per share pair

					// Evaluate portfolio risk before trading
					riskAction, riskReason := t.RiskMgr.Evaluate()
					if riskAction == paper.RiskActionKillSwitch {
						t.TUI.LogEvent("[%s] 🛑 RISK: Kill switch - %s", t.ID, riskReason)
						continue
					}
					if riskAction == paper.RiskActionReduceSize {
						t.TUI.LogEvent("[%s] ⚠️ RISK: Reducing size - %s", t.ID, riskReason)
						// Will use baseShares only (no scaling)
					}

					if margin >= minMarginPercent && t.RiskMgr.CanPlaceOrder(baseSharesPerTrade*(ask1+ask2)) {
						baseShares := baseSharesPerTrade
						
						// LIQUIDITY CHECK: Ensure we don't exceed top-of-book size
						// This prevents partial fills that break the arbitrage
						maxLiquidity := 1e9
						for _, outcome := range t.Outcomes {
							asks := t.TokenFullAsks[outcome]
							if len(asks) > 0 && asks[0].Size < maxLiquidity {
								maxLiquidity = asks[0].Size
							}
						}
						
						// Cap shares at 90% of available top-of-book liquidity for safety
						if baseShares > maxLiquidity*0.9 {
							baseShares = maxLiquidity * 0.9
						}

						// Only scale if risk allows
						shares := baseShares
						if riskAction != paper.RiskActionReduceSize {
							// Scale shares based on margin - adjusted for 1% baseline
							if margin >= 4.0 {
								shares = baseShares * 4
							} else if margin >= 3.0 {
								shares = baseShares * 3
							} else if margin >= 2.0 {
								shares = baseShares * 2
							}
						}

						// Apply compounding multiplier from profitable rounds
						compoundMult := t.Engine.GetCompoundMultiplier()
						shares = float64(int(float64(shares) * compoundMult))

						cost := shares * (ask1 + ask2)
						if !t.RiskMgr.CanPlaceOrder(cost) || cost > currentBalance {
							// Scale back to base if over risk limit or balance
							shares = baseShares
							cost = shares * (ask1 + ask2)

							// If even base is too much, don't trade
							if !t.RiskMgr.CanPlaceOrder(cost) || cost > currentBalance {
								continue
							}
						}

						profit := shares * (1.0 - sum)
						if compoundMult > 1.0 {
							t.TUI.LogEvent("[%s] 🎯 ARB! %s@$%.2f + %s@$%.2f = $%.2f | %.0f shares (%.1fx), profit $%.2f (%.1f%%)",
								t.ID, t.Outcomes[0], ask1, t.Outcomes[1], ask2, sum, shares, compoundMult, profit, margin)
						} else {
							t.TUI.LogEvent("[%s] 🎯 ARB! %s@$%.2f + %s@$%.2f = $%.2f | %.0f shares ($%.0f), profit $%.2f (%.1f%%)",
								t.ID, t.Outcomes[0], ask1, t.Outcomes[1], ask2, sum, shares, cost, profit, margin)
						}

						if t.CSVLogger != nil {
							t.CSVLogger.Log("TRADE", t.ID, "ARB_ENTRY", fmt.Sprintf("Sum: %.3f, Shares: %.0f, Margin: %.1f%%", sum, shares, margin), t.Engine.GetEquity())
						}

						t.Engine.BuyForMarket(t.ID, t.Outcomes[0], ask1, shares)
						t.Engine.BuyForMarket(t.ID, t.Outcomes[1], ask2, shares)

						t.LaddersPlaced = true
					}
				}
			}

			// Minimal sleep for ultra-low latency trading - context-aware
			// 10ms = 100 ticks/sec - balance between responsiveness and CPU
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
}

// determineWinner picks the winning outcome based on last known prices
func (t *MarketTrader) determineWinner() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.Outcomes) == 0 {
		return ""
	}

	// For 2-outcome markets (Up/Down)
	if len(t.Outcomes) == 2 {
		p1 := t.FloatPrices[t.Outcomes[0]]
		p2 := t.FloatPrices[t.Outcomes[1]]

		if p1 > p2 {
			return t.Outcomes[0]
		} else if p2 > p1 {
			return t.Outcomes[1]
		}
	}

	// Fallback: Pick first outcome if no data
	return t.Outcomes[0]
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

func toMarketLevels(tui *paper.TUI, id string, levels []api.PriceLevel) []paper.MarketLevel {
	result := make([]paper.MarketLevel, len(levels))
	for i, l := range levels {
		p, err := strconv.ParseFloat(l.Price, 64)
		if err != nil {
			tui.LogEvent("[%s] Warning: failed to parse price '%s': %v", id, l.Price, err)
			continue
		}
		s, err := strconv.ParseFloat(l.Size, 64)
		if err != nil {
			tui.LogEvent("[%s] Warning: failed to parse size '%s': %v", id, l.Size, err)
			continue
		}
		result[i] = paper.MarketLevel{Price: p, Size: s}
	}
	return result
}

// handleRestFallback polls REST API when WebSocket data is stale
// Returns true if any data was successfully retrieved
func (t *MarketTrader) handleRestFallback(ctx context.Context, tokenPrices map[string]string, staleTime time.Duration) bool {
	t.LastRestPoll = time.Now()
	staleSeconds := int(staleTime.Seconds())

	// Poll REST synchronously for reliability
	restSuccess := 0
	restErrors := 0
	restEmpty := 0
	var lastErr error
	for tokenID, outcome := range t.TokenMap {
		// Use short timeout
		restCtx, restCancel := context.WithTimeout(ctx, 3*time.Second)
		book, err := t.RestClient.GetOrderBook(restCtx, tokenID)
		restCancel()

		if err != nil {
			restErrors++
			lastErr = err
			continue
		}

		// Check if book is empty
		if len(book.Bids) == 0 && len(book.Asks) == 0 {
			restEmpty++
			continue
		}

		bid, ask := 0.0, 0.0
		for _, b := range book.Bids {
			p, err := strconv.ParseFloat(b.Price, 64)
			if err != nil {
				t.TUI.LogEvent("[%s] Warning: failed to parse bid price '%s': %v", t.ID, b.Price, err)
				continue
			}
			if p > bid {
				bid = p
			}
		}
		for _, a := range book.Asks {
			p, err := strconv.ParseFloat(a.Price, 64)
			if err != nil {
				t.TUI.LogEvent("[%s] Warning: failed to parse ask price '%s': %v", t.ID, a.Price, err)
				continue
			}
			if p > 0 && (ask == 0 || p < ask) {
				ask = p
			}
		}

		// Always update with whatever data we got (even partial)
		t.mu.Lock()
		if bid > 0 {
			t.TokenBids[outcome] = bid
		}
		if ask > 0 && ask < 1.0 {
			t.TokenAsks[outcome] = ask
		}
		if bid > 0 && ask > 0 && ask < 1.0 {
			mid := (bid + ask) / 2
			t.FloatPrices[outcome] = mid
			tokenPrices[outcome] = fmt.Sprintf("%.3f", mid)
			t.Engine.UpdateMarketData(t.ID, outcome, mid, bid, ask)
		}
		t.TokenFullBids[outcome] = toMarketLevels(t.TUI, t.ID, book.Bids)
		t.TokenFullAsks[outcome] = toMarketLevels(t.TUI, t.ID, book.Asks)
		t.mu.Unlock()

		// Count as success if we got any valid data
		if bid > 0 || (ask > 0 && ask < 1.0) || len(book.Bids) > 0 || len(book.Asks) > 0 {
			restSuccess++
		}
	}

	// Log result - minimal spam, maximum info
	if restSuccess > 0 {
		t.LastUpdate = time.Now()
		t.TUI.UpdateMarketPrices(t.ID, t.TokenBids, t.TokenAsks)
		// Only log recovery after significant staleness
		if staleSeconds >= 5 {
			t.TUI.LogEvent("[%s] ✅ REST recovered after %ds", t.ID, staleSeconds)
		}
		return true
	} else if restErrors > 0 {
		// Log errors every 10 seconds to avoid spam
		if staleSeconds%10 == 0 || staleSeconds == 5 {
			t.TUI.LogEvent("[%s] ❌ REST fail %ds: %v", t.ID, staleSeconds, lastErr)
		}
	} else if restEmpty == len(t.TokenMap) {
		// All books empty - likely market ended
		if staleSeconds%10 == 0 {
			t.TUI.LogEvent("[%s] 📭 All books empty (%ds)", t.ID, staleSeconds)
		}
	}
	return false
}
