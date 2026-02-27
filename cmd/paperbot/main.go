package main

import (
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
	"sync"
	"syscall"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	mkt "Market-bot/internal/markets"
	"Market-bot/internal/paper"
	"Market-bot/internal/strategy"
)

const (
	StartingBalance = 100.0 // $100 paper trading balance
	UseLiveUI       = true  // Set to false for traditional logging

	// Split strategy constants
	MinSplitBuffer   = 50.0  // Minimum initial split buffer ($)
	MinSplitAmount   = 10.0  // Minimum split amount to execute ($)
	MaxSharesPerSell = 250.0 // Hard safety cap on shares per sell
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

	// Split strategy simulation
	SplitInventory     *paper.SplitInventory
	ReplenishCtrl      *paper.ReplenishController
	SplitInitialized   bool
	InitialSplitAmount float64 // Track initial split for replenishment target
	LastSplitSell      time.Time

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
	var orderBook *paper.OrderBook
	var csvLogger *core.CSVLogger

	// Setup signal handling with immediate terminal restore
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// emergencyCleanup ensures terminal is restored and positions are handled on crash/exit
	emergencyCleanup := func() {
		core.RestoreTerminal()
		if engine != nil {
			positions := engine.GetPositions()
			if len(positions) > 0 {
				fmt.Println("💰 Emergency: Liquidating all paper positions...")
				proceeds := engine.LiquidateAll()
				fmt.Printf("💵 Liquidation proceeds: $%.2f\n", proceeds)
			}
		}
		if orderBook != nil {
			orderBook.CancelAllOrders()
		}
	}

	// Global panic recovery - restore terminal on any panic
	defer func() {
		if r := recover(); r != nil {
			emergencyCleanup()
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
		// Give graceful shutdown 10 seconds, then force exit
		time.Sleep(10 * time.Second)
		core.RestoreTerminal()
		fmt.Println("\n⚠️ Force exit: graceful shutdown timed out")
		os.Exit(1)
	}()

	// Disable terminal echo to prevent arrow keys from appearing
	// This is done via stty which works on most Unix systems including Termux
	disableEcho := exec.Command("stty", "-echo", "-icanon")
	disableEcho.Stdin = os.Stdin
	_ = disableEcho.Run() // Ignore errors if stty not available

	// Ensure terminal is restored on any exit
	defer core.RestoreTerminal()

	startTime := time.Now()

	// Clear screen at startup
	fmt.Print("\033[H\033[2J")
	fmt.Println("🎰 POLYARB-15M Starting (Multi-Asset: BTC, ETH, SOL, XRP)...")
	fmt.Printf("⏰ Started at: %s\n", startTime.Format("2006-01-02 15:04:05"))

	// Initialize persistent components (survive market rotation)
	engine = paper.NewEngine(StartingBalance)

	// Load config
	cfg, err := core.LoadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	fmt.Println("✅ Config loaded successfully")
	if cfg.FeeRateBps > 0 {
		// Show effective fee at p=0.50 (worst case for arb)
		// Formula: fee_tokens = shares * base_rate * 2 * p * (1-p)
		// At p=0.50: curve = 0.5, so effective = base_rate * 0.5
		effectiveAt50 := float64(cfg.FeeRateBps) / 10000.0 * 0.5 * 100.0
		fmt.Printf("💰 Fee simulation enabled: %d bps base (~%.1f%% effective at p=0.50)\n", cfg.FeeRateBps, effectiveAt50)
	}

	restClient := api.NewRestClient("")

	// Create shared order book and TUI (persistent across market rotations)
	orderBook = paper.NewOrderBook()
	tui := paper.NewTUI(engine, orderBook)

	// Seed settings panel from config (.env), so the live panel reflects initial values
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

	// Initialize CSV Logger if enabled in config
	if cfg.EnableCSVLogger {
		csvLogger, err = core.NewCSVLogger("bot_activity.csv")
		if err != nil {
			fmt.Printf("⚠️ Warning: Could not initialize CSV logger: %v\n", err)
		} else {
			defer csvLogger.Close()
		}
	}

	// Start TUI render loop — pass stop so a single Ctrl+C / [q] quits cleanly.
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

	logEvent(tui, csvLogger, engine, "INFO", "SYSTEM", "STARTUP", "Bot starting with multi-asset support")

	// Goroutine monitor and memory cleanup
	go func() {
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
			fmt.Println("\n👋 Shutting down...")

			// Run emergency cleanup
			emergencyCleanup()

			stats := engine.GetStats()
			duration := time.Since(startTime).Round(time.Second)
			fmt.Printf("📊 Final Stats: Balance $%.2f | Realized PnL $%.2f | Trades %d | Duration %v\n",
				stats.CurrentBalance, stats.RealizedPnL, stats.TotalTrades, duration)
			return nil
		default:
		}

		// Find all available markets (BTC, ETH, SOL, XRP)
		logEvent(tui, csvLogger, engine, "INFO", "SYSTEM", "MARKET_SEARCH", "Searching for active markets based on live settings...")
		markets := mkt.FindMarkets(ctx, restClient, tui.GetSettings, func(format string, args ...interface{}) {
			tui.LogEvent(format, args...)
		})
		if len(markets) == 0 {
			logEvent(tui, csvLogger, engine, "WARN", "SYSTEM", "NO_MARKETS", "No active markets found, retrying...")
			select {
			case <-ctx.Done():
				tui.Stop()
				fmt.Println("\n👋 Shutting down...")
				stats := engine.GetStats()
				duration := time.Since(startTime).Round(time.Second)
				fmt.Printf("📊 Final Stats: Balance $%.2f | Realized PnL $%.2f | Trades %d | Duration %v\n",
					stats.CurrentBalance, stats.RealizedPnL, stats.TotalTrades, duration)
				return nil
			case <-time.After(2 * time.Second):
			}
			continue
		}

		// Track starting equity for compounding calculation
		startingEquity := engine.GetEquity()
		compoundMultiplier := engine.GetCompoundMultiplier()
		logEvent(tui, csvLogger, engine, "INFO", "SYSTEM", "ROUND_START", "Round starting with %d markets | Multiplier: %.2fx", len(markets), compoundMultiplier)

		// Create a context for this specific round of trading
		roundCtx, roundCancel := context.WithCancel(ctx)

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
			outcomes := mkt.GetOutcomes(market)
			tui.AddMarket(assetID, market.Slug, outcomes, endTime)
			// Reduced logging: Only TUI for startup info
			tui.LogEvent("🚀 Trading %s: %s", assetID, market.Slug)

			trader := createTrader(assetID, market, engine, orderBook, restClient, tui, outcomes, endTime, csvLogger, cfg)
			wg.Add(1)
			tradersStarted++
			go func(id string, t *MarketTrader) {
				defer wg.Done()
				// Create a sub-context for this specific trader to prevent goroutine leaks
				tCtx, tCancel := context.WithCancel(roundCtx)
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

		// Close channels safely in a background goroutine AFTER wg.Wait()
		// This prevents deadlocks and panics if traders take a long time to exit
		go func() {
			wg.Wait()
			close(results)
			close(errors)
		}()

		// Collect results
		// range over the channel safely processes only successful returns and cleanly exits
		// once the background goroutine closes the channel after wg.Wait() finishes.
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

		// Clear old market data and release stale HTTP connections before
		// the next market-search phase.  Stale HTTP/1.1 keep-alive connections
		// left over from heavy per-trader REST polling can trigger unexpected
		// server responses on reuse; closing them here prevents that.
		tui.LogEvent("🔄 Market round complete, searching for new markets...")
		tui.ClearMarkets()
		orderBook.CancelAllOrders()
		engine.ClearMarketData()
		restClient.CloseIdleConnections()
	}
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
		KillSwitchDrawdown: 0.10,
	}
	riskMgr := paper.NewRiskManager(riskConfig, engine, orderBook, outcomes)

	monitor := paper.NewMarketMonitor(engine, orderBook, ladderMgr, riskMgr)
	monitor.SetMarket(market.Slug, market.ConditionID, outcomes, endTime)

	splitInv := paper.NewSplitInventory()
	engine.RegisterSplitInventory(splitInv) // Register for equity calculation
	tui.RegisterSplitInventory(splitInv)    // Register for TUI display

	return &MarketTrader{
		ID:               id,
		Market:           market,
		Engine:           engine,
		OrderBook:        orderBook,
		LadderMgr:        ladderMgr,
		RiskMgr:          riskMgr,
		Monitor:          monitor,
		TokenMap:         tokenMap,
		Outcomes:         outcomes,
		EndTime:          endTime,
		RestClient:       restClient,
		TUI:              tui,
		CSVLogger:        csvLogger,
		Config:           cfg,
		TokenBids:        make(map[string]float64),
		TokenAsks:        make(map[string]float64),
		TokenFullBids:    make(map[string][]paper.MarketLevel),
		TokenFullAsks:    make(map[string][]paper.MarketLevel),
		FloatPrices:      make(map[string]float64),
		LastUpdate:       time.Now(),
		LastRestPoll:     time.Now(),
		SplitInventory:   splitInv,
		ReplenishCtrl:    paper.NewReplenishController(),
		SplitInitialized: false,
		LastSplitSell:    time.Time{},
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
		_, err := t.Engine.BuyForMarket(t.ID, order.Outcome, fillPrice, fillQty)
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

	const wsWarnInterval = 10 * time.Second   // Only warn once per 10 seconds
	const wsForceReconnect = 10 * time.Second // Force reconnection after 10 seconds stale

	// Track WebSocket channel closure state (outside loop to persist across ticks)
	wsChannelClosed := false

	for {
		select {
		case <-ctx.Done():
			t.LadderMgr.CancelAllLadders()
			positions := t.Engine.GetPositions()
			if len(positions) > 0 {
				t.TUI.LogEvent("[%s] 🔴 EMERGENCY EXIT: Liquidating positions...", t.ID)
				t.Engine.LiquidateAll()
			}
			// Liquidate split inventory
			splitPositions := t.SplitInventory.GetAllPositions()
			if len(splitPositions) > 0 {
				t.TUI.LogEvent("[%s] 🔀 EMERGENCY EXIT: Merging & Liquidating Split Inventory...", t.ID)
				// For a 2-outcome market, we just merge min shares, then sell the rest at bid.
				if len(t.Outcomes) == 2 {
					minShares := t.SplitInventory.GetMinSplitShares(t.ID, t.Outcomes[0], t.Outcomes[1])
					if minShares > 0 {
						t.Engine.MergeForMarket(t.ID, t.Outcomes[0], t.Outcomes[1], minShares)
						t.SplitInventory.RecordMerge(t.ID, t.Outcomes[0], t.Outcomes[1], minShares)
						t.TUI.LogEvent("[%s] 🔀 Merged %.0f pairs", t.ID, minShares)
					}
					// Sell remaining unbalanced shares at current bid
					for _, out := range t.Outcomes {
						rem := t.SplitInventory.GetSplitShares(t.ID, out)
						if rem > 0 {
							bid, _ := t.Engine.GetMarketBidAsk(t.ID, out)
							if bid <= 0 { // Fallback
								bid = 0.50
							}
							t.SplitInventory.RecordSell(t.ID, out, rem, bid)
							t.Engine.AddBalance(rem * bid)
							t.TUI.LogEvent("[%s] 📉 Sold %.0f split shares of %s at $%.3f", t.ID, rem, out, bid)
						}
					}
				}
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
					t.Engine.RedeemWithDetails(t.ID, winner)
				}

				finalStats := t.Engine.GetStats()
				return &marketResult{
					realizedPnL: finalStats.RealizedPnL - startingRealizedPnL,
					trades:      finalStats.TotalTrades - tradesAtStart,
				}, nil
			}

			// Check kill switch - DON'T EXIT, just pause trading
			// Exiting would leave positions unmatched; better to hold until expiration
			killSwitchActive := t.RiskMgr.IsKillSwitchTriggered()
			if killSwitchActive {
				// Log once per state change, then just skip trading
				t.TUI.SetKillSwitch("Risk limits exceeded - pausing trades")
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
				result := t.Engine.RedeemWithDetails(t.ID, winner)
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
						// Channel closed - check if context cancelled or unexpected
						select {
						case <-ctx.Done():
							// Context cancelled, normal shutdown
							return nil, ctx.Err()
						default:
							// Unexpected close, mark for reconnect attempt
							wsChannelClosed = true
							goto doneProcessingWS
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
				// IMPORTANT: only write to TokenBids/TokenAsks when the parsed
				// value is strictly positive — a zero value means "no orders on
				// that side in this message" and must NOT overwrite a previously
				// valid price from REST or an earlier WS snapshot.
				if books, err := api.ParseOrderBooks(msg); err == nil && len(books) > 0 && books[0].AssetID != "" {
					// ── Book snapshot (array) ──────────────────────────────
					foundForThisTrader := false
					for _, b := range books {
						bid, ask := 0.0, 0.0
						for _, order := range b.Bids {
							p, _ := strconv.ParseFloat(order.Price, 64)
							if p > bid {
								bid = p
							}
						}
						for _, order := range b.Asks {
							p, _ := strconv.ParseFloat(order.Price, 64)
							if p > 0 && p < 1.0 && (ask == 0 || p < ask) {
								ask = p
							}
						}
						outcome := t.TokenMap[b.AssetID]
						if outcome != "" {
							foundForThisTrader = true
							t.mu.Lock()
							// Guard: only persist valid (non-zero) prices so we
							// never overwrite a good REST value with a zero.
							if bid > 0 {
								t.TokenBids[outcome] = bid
							}
							if ask > 0 {
								t.TokenAsks[outcome] = ask
							}
							if bid > 0 && ask > 0 {
								mid := (bid + ask) / 2
								t.FloatPrices[outcome] = mid
								tokenPrices[outcome] = fmt.Sprintf("%.3f", mid)
								t.Engine.UpdateMarketData(t.ID, outcome, mid, bid, ask)
							}
							// Always update full depth from snapshots — REST will
							// keep this fresh on the 4 ms poll as well.
							t.TokenFullBids[outcome] = mkt.LevelsToPriceDepth(b.Bids)
							t.TokenFullAsks[outcome] = mkt.LevelsToPriceDepth(b.Asks)
							t.mu.Unlock()
						}
					}
					if foundForThisTrader {
						t.mu.Lock()
						t.LastUpdate = time.Now()
						t.mu.Unlock()
					}
				} else if update, err := api.ParsePriceUpdate(msg); err == nil && len(update.PriceChanges) > 0 {
					// ── Price-change delta ─────────────────────────────────
					foundForThisTrader := false
					
					t.mu.Lock()
					for _, pc := range update.PriceChanges {
						outcome := t.TokenMap[pc.AssetID]
						if outcome == "" {
							continue
						}
						foundForThisTrader = true
						p, errP := strconv.ParseFloat(pc.Price, 64)
						s, errS := strconv.ParseFloat(pc.Size, 64)
						if errP != nil || errS != nil || p <= 0 {
							continue
						}

						switch pc.Side {
						case "BUY":
							t.TokenFullBids[outcome] = mkt.ApplyDelta(t.TokenFullBids[outcome], p, s, true)
						case "SELL":
							t.TokenFullAsks[outcome] = mkt.ApplyDelta(t.TokenFullAsks[outcome], p, s, false)
						}
					}
					
					for outcome := range t.TokenMap {
						bids := t.TokenFullBids[outcome]
						if len(bids) > 0 {
							t.TokenBids[outcome] = bids[0].Price
						} else {
							t.TokenBids[outcome] = 0
						}

						asks := t.TokenFullAsks[outcome]
						if len(asks) > 0 {
							t.TokenAsks[outcome] = asks[0].Price
						} else {
							t.TokenAsks[outcome] = 0
						}

						if t.TokenBids[outcome] > 0 && t.TokenAsks[outcome] > 0 {
							mid := (t.TokenBids[outcome] + t.TokenAsks[outcome]) / 2
							t.FloatPrices[outcome] = mid
							tokenPrices[outcome] = fmt.Sprintf("%.3f", mid)
							t.Engine.UpdateMarketData(t.ID, outcome, mid, t.TokenBids[outcome], t.TokenAsks[outcome])
						}
					}
					t.mu.Unlock()

					if foundForThisTrader {
						t.mu.Lock()
						t.LastUpdate = time.Now()
						t.mu.Unlock()
					}
				} else if book, err := api.ParseOrderBook(msg); err == nil && book.AssetID != "" {
					// ── Book snapshot (single object) ──────────────────────
					bid, ask := 0.0, 0.0
					for _, order := range book.Bids {
						p, _ := strconv.ParseFloat(order.Price, 64)
						if p > bid {
							bid = p
						}
					}
					for _, order := range book.Asks {
						p, _ := strconv.ParseFloat(order.Price, 64)
						if p > 0 && p < 1.0 && (ask == 0 || p < ask) {
							ask = p
						}
					}
					outcome := t.TokenMap[book.AssetID]
					if outcome != "" {
						t.mu.Lock()
						t.LastUpdate = time.Now()
						// Guard: only persist valid (non-zero) prices.
						if bid > 0 {
							t.TokenBids[outcome] = bid
						}
						if ask > 0 {
							t.TokenAsks[outcome] = ask
						}
						if bid > 0 && ask > 0 {
							mid := (bid + ask) / 2
							t.FloatPrices[outcome] = mid
							tokenPrices[outcome] = fmt.Sprintf("%.3f", mid)
							t.Engine.UpdateMarketData(t.ID, outcome, mid, bid, ask)
						}
						t.TokenFullBids[outcome] = mkt.LevelsToPriceDepth(book.Bids)
						t.TokenFullAsks[outcome] = mkt.LevelsToPriceDepth(book.Asks)
						t.mu.Unlock()
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
				t.TUI.UpdateMarketPricesWithSource(t.ID, t.TokenBids, t.TokenAsks, "WS")
			}
			// NOTE: Removed TouchMarket call - WS connection being "alive" doesn't mean
			// data is fresh. WS often doesn't send liquidity updates, so we should only
			// update LastUpdate when we actually receive new price/liquidity data.
			// This ensures the UI accurately shows data staleness.

			// Check WebSocket connection health for REST fallback decision
			wsConnected := wsMgr.IsConnected()
			wsLastMsg := wsMgr.TimeSinceLastMessage()

			// Update WS staleness and ping latency in TUI
			t.TUI.UpdateWSLatency(wsLastMsg)
			t.TUI.UpdateWSPingLatency(wsMgr.PingLatency())

			// ============ REST FALLBACK ============
			// WS is primary for liquidity data via full depth updates and deltas.
			// Only poll REST if WS is unhealthy or stale.
			staleTime := time.Since(t.LastUpdate)
			
			// Update WS staleness and ping latency in TUI
			t.TUI.UpdateWSLatency(wsLastMsg)
			t.TUI.UpdateWSPingLatency(wsMgr.PingLatency())

			wsUnhealthy := !wsConnected || wsLastMsg > 10*time.Second
			if wsUnhealthy && staleTime > 3*time.Second {
				t.handleRestFallback(ctx, tokenPrices, staleTime)
			}

			// FORCE RECONNECT: If stale for 10s for faster recovery
			if time.Since(t.LastUpdate) > 10*time.Second {
				if time.Since(lastForceReconnect) > wsForceReconnect {
					t.TUI.LogEvent("[%s] ⚠️ STALE (10s) - forcing WS reconnect", t.ID)
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

			t.mu.Lock()
			// Map current trader's depth data for TUI
			for _, outcome := range t.Outcomes {
				if bids, ok := t.TokenFullBids[outcome]; ok {
					bidDepth[outcome] = append([]paper.MarketLevel(nil), bids...)
				}
				if asks, ok := t.TokenFullAsks[outcome]; ok {
					askDepth[outcome] = append([]paper.MarketLevel(nil), asks...)
				}
			}
			t.mu.Unlock()

			t.TUI.UpdateOrderBookDepth(t.ID, bidDepth, askDepth)

			// Process order fills
			t.mu.Lock()
			for outcome := range tokenPrices {
				bids := t.TokenFullBids[outcome]
				asks := t.TokenFullAsks[outcome]
				if len(bids) > 0 || len(asks) > 0 {
					bidsCopy := append([]paper.MarketLevel(nil), bids...)
					asksCopy := append([]paper.MarketLevel(nil), asks...)
					t.OrderBook.ProcessPriceUpdate(outcome, bidsCopy, asksCopy)
				}
			}
			t.mu.Unlock()

			// Check if market has ended (only exit condition that matters)
			// DON'T exit on "liquidity dried up" - volatile markets can have extreme prices
			// and that's normal market behavior, not a reason to exit

			// Trading logic - check every tick for arbitrage opportunities
			liveCfg := t.TUI.GetSettings()
			if len(tokenPrices) == 2 && len(t.Outcomes) == 2 && marketState == paper.MarketStateActive {
				ask1 := t.TokenAsks[t.Outcomes[0]]
				ask2 := t.TokenAsks[t.Outcomes[1]]

			// Read live price-range filter from settings panel (adjustable at runtime)
			minAsk := liveCfg.MinAskPrice
			maxAsk := liveCfg.MaxAskPrice

			if ask1 >= minAsk && ask1 <= maxAsk && ask2 >= minAsk && ask2 <= maxAsk {
				sum := ask1 + ask2
				margin := (1.0 - sum) * 100

				// Skip trading if kill switch is active (but don't exit - wait for expiration)
				if killSwitchActive {
					continue
				}

				// Use config for minimum margin (default 2%)
				minMarginPercent := t.Config.MinMarginPercent

					// Calculate dynamic trade size based on EQUITY (not just cash)
					// This ensures consistent sizing regardless of how much is in positions
					// $100 equity * 5% = $5 trade size (even if only $10 is cash)
					currentEquity := t.Engine.GetEquity()
					currentCash := t.Engine.GetBalance()
					tradeSize := t.Config.CalculateTradeSize(currentEquity)
					baseSharesPerTrade := tradeSize / sum // Shares = $ / price per share pair

					// Evaluate portfolio risk before trading
					riskAction, riskReason := t.RiskMgr.Evaluate()
					if riskAction == paper.RiskActionKillSwitch {
						t.TUI.LogEvent("[%s] 🛑 RISK: Kill switch - %s (pausing, not exiting)", t.ID, riskReason)
						continue
					}
					if riskAction == paper.RiskActionReduceSize {
						t.TUI.LogEvent("[%s] ⚠️ RISK: Reducing size - %s", t.ID, riskReason)
						// Will use baseShares only (no scaling)
					}

					if margin >= minMarginPercent && t.RiskMgr.CanPlaceOrder(baseSharesPerTrade*(ask1+ask2)) {
						baseShares := baseSharesPerTrade

						// AGGREGATED LIQUIDITY: Calculate total matched liquidity across ALL price levels
						// that maintain minimum margin. This allows "chasing" liquidity deeper into the book.
						maxSum := 1.0 - (minMarginPercent / 100.0) // e.g., 2% margin → max sum = 0.98

						// Adjust maxSum to account for fees if enabled
						// Polymarket fees: fee_tokens = shares * base_rate * 2 * p * (1-p)
						// For arb near sum=1.0, prices are ~0.50 each, so curve = 0.5
						// Conservative: assume worst case fee (both sides at p=0.50)
						feeRateBps := t.Config.FeeRateBps
						if feeRateBps > 0 {
							baseRate := float64(feeRateBps) / 10000.0
							// Worst case: both sides at p=0.50, curve = 2*0.5*0.5 = 0.5
							worstCaseFeePerSide := baseRate * 0.5
							// Total fee for both sides (in tokens, which equals $ at settlement)
							totalFeeFraction := worstCaseFeePerSide * 2
							maxSum -= totalFeeFraction
						}

						// Copy and sort asks by price ascending for both outcomes
						asks1 := make([]paper.MarketLevel, len(t.TokenFullAsks[t.Outcomes[0]]))
						copy(asks1, t.TokenFullAsks[t.Outcomes[0]])
						sort.Slice(asks1, func(i, j int) bool { return asks1[i].Price < asks1[j].Price })

						asks2 := make([]paper.MarketLevel, len(t.TokenFullAsks[t.Outcomes[1]]))
						copy(asks2, t.TokenFullAsks[t.Outcomes[1]])
						sort.Slice(asks2, func(i, j int) bool { return asks2[i].Price < asks2[j].Price })

						// Calculate aggregated matched liquidity across valid price levels
						var totalMatchedLiquidity float64
						var rawLiq1, rawLiq2 float64 // Track actual liquidity on each side for display
						var maxValidI, maxValidJ int // Track deepest valid level on each side price level combinations were valid

						i, j := 0, 0
						for i < len(asks1) && j < len(asks2) {
							// Current prices at each pointer
							p1 := asks1[i].Price
							p2 := asks2[j].Price

							// Check if this combination maintains minimum margin
							if p1+p2 > maxSum+1e-6 {
								break // Can't go deeper, would exceed margin threshold
							}

							// Get liquidity at current levels
							levelLiq1 := asks1[i].Size
							levelLiq2 := asks2[j].Size

							// Track deepest valid level on each side (only count once per level)
							if i+1 > maxValidI {
								maxValidI = i + 1
								rawLiq1 += asks1[i].Size
							}
							if j+1 > maxValidJ {
								maxValidJ = j + 1
								rawLiq2 += asks2[j].Size
							}

							// Get liquidity at current levels (may be partial after matching)

							// Matched liquidity = min of both sides (arbitrage requires equal shares)
							matchedAtLevel := levelLiq1
							if levelLiq2 < matchedAtLevel {
								matchedAtLevel = levelLiq2
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
							// If both exhausted at same time, both pointers already incremented
						}

						// Use RAW liquidity for display (shows actual available on each side)
						liq1 := rawLiq1
						liq2 := rawLiq2
						minLiquidity := totalMatchedLiquidity
						bookDepth1 := len(t.TokenFullAsks[t.Outcomes[0]])
						bookDepth2 := len(t.TokenFullAsks[t.Outcomes[1]])

						// Use 100% of matched liquidity - force MarketBuy guarantees atomic fills on both sides
						// No legging risk since we walk the book simultaneously, not single-level limit orders
						maxSafeShares := minLiquidity * 1.00

						// Only scale if risk allows
						shares := baseShares
						if t.Config.EnableMarginAggression && riskAction != paper.RiskActionReduceSize {
							multiplier := math.Floor(margin)
							if multiplier > t.Config.MaxAggressionMultiplier {
								multiplier = t.Config.MaxAggressionMultiplier
							}
							if multiplier < 1 {
								multiplier = 1
							}
							shares = baseShares * multiplier
						}

						// Apply compounding multiplier from profitable rounds
						compoundMult := t.Engine.GetCompoundMultiplier()
						shares = float64(int(float64(shares) * compoundMult))

						// Force at least 1 share if there's any matched liquidity and we have budget
						if shares < 1.0 && minLiquidity >= 1.0 {
							shares = 1.0
						}

						// FINAL LIQUIDITY CAP: Ensure shares never exceed available matched liquidity
						// This must be checked AFTER all scaling (margin scaling + compounding)
						if shares > maxSafeShares {
							shares = maxSafeShares
						}
						tm := strategy.CalculateTradeMetricsCurve(shares, ask1, ask2, feeRateBps)
						cost, netProfit := tm.Cost, tm.Net

						// Skip if net profit is not positive after order cost
						if netProfit <= 0 {
							continue
						}

						if !t.RiskMgr.CanPlaceOrder(cost) || cost > currentCash {
							// Scale back to what cash allows, but still respect liquidity cap
							maxAffordableShares := currentCash / sum

							// Apply the stricter of: cash limit OR liquidity limit
							if maxAffordableShares > maxSafeShares {
								maxAffordableShares = maxSafeShares
							}

							if maxAffordableShares < 1 {
								continue // Not enough cash/liquidity for even 1 share
							}
							shares = maxAffordableShares
							tm = strategy.CalculateTradeMetricsCurve(shares, ask1, ask2, feeRateBps)
							cost, netProfit = tm.Cost, tm.Net

							// If still over risk limit or not profitable after cost, don't trade
							if !t.RiskMgr.CanPlaceOrder(cost) || cost > currentCash || netProfit <= 0 {
								continue
							}
						}
						if compoundMult > 1.0 {
							t.TUI.LogEvent("[%s] 🎯 ARB! %s@$%.2f + %s@$%.2f = $%.2f | %.0f shares (%.1fx), profit $%.2f (%.1f%%) [liq: %.0f/%.0f, levels used: %d/%d (total depth: %d/%d)]",
								t.ID, t.Outcomes[0], ask1, t.Outcomes[1], ask2, sum, shares, compoundMult, netProfit, margin, liq1, liq2, maxValidI, maxValidJ, bookDepth1, bookDepth2)
						} else {
							t.TUI.LogEvent("[%s] 🎯 ARB! %s@$%.2f + %s@$%.2f = $%.2f | %.0f shares ($%.0f), profit $%.2f (%.1f%%) [liq: %.0f/%.0f, levels used: %d/%d (total depth: %d/%d)]",
								t.ID, t.Outcomes[0], ask1, t.Outcomes[1], ask2, sum, shares, cost, netProfit, margin, liq1, liq2, maxValidI, maxValidJ, bookDepth1, bookDepth2)
						}

						if t.CSVLogger != nil {
							t.CSVLogger.Log("TRADE", t.ID, "ARB_ENTRY", fmt.Sprintf("Sum: %.3f, Shares: %.0f, Margin: %.1f%%", sum, shares, margin), t.Engine.GetEquity())
						}

						// FORCE FILL: Use MarketBuy to "walk the book" and guarantee fills
						// This ensures both sides fill completely, avoiding legging risk
						// Get fresh copies of ask levels (sorted by price ascending)
						freshAsks1 := make([]paper.MarketLevel, len(t.TokenFullAsks[t.Outcomes[0]]))
						copy(freshAsks1, t.TokenFullAsks[t.Outcomes[0]])
						sort.Slice(freshAsks1, func(i, j int) bool { return freshAsks1[i].Price < freshAsks1[j].Price })

						freshAsks2 := make([]paper.MarketLevel, len(t.TokenFullAsks[t.Outcomes[1]]))
						copy(freshAsks2, t.TokenFullAsks[t.Outcomes[1]])
						sort.Slice(freshAsks2, func(i, j int) bool { return freshAsks2[i].Price < freshAsks2[j].Price })

						// Execute market orders that consume liquidity across multiple levels
						// Force fill: walks the book to guarantee execution
						trade1, avgPrice1, err1 := t.Engine.MarketBuy(t.ID, t.Outcomes[0], shares, freshAsks1)
						trade2, avgPrice2, err2 := t.Engine.MarketBuy(t.ID, t.Outcomes[1], shares, freshAsks2)

						if err1 != nil || err2 != nil {
							// Concurrency edge case: another market consumed the cash between our check and execution.
							// Fail gracefully without recording bogus $0 trades.
							t.TUI.LogEvent("[%s] ⚠️ Trade failed during execution (TOCTOU / Insufficient balance). err1: %v, err2: %v", t.ID, err1, err2)
							
							// If one legged (rare in paperbot since it's synchronous per market, but possible if cash ran out mid-way),
							// we could theoretically unwind, but paperbot engine doesn't track legged paper trades strictly.
							continue
						}

						// Get actual fill quantities
						filled1, filled2 := shares, shares
						actualCost1, actualCost2 := shares*avgPrice1, shares*avgPrice2
						if trade1 != nil {
							filled1 = trade1.Quantity
							actualCost1 = trade1.Value
						}
						if trade2 != nil {
							filled2 = trade2.Quantity
							actualCost2 = trade2.Value
						}

						// Log if we walked deeper into the book
						if avgPrice1 != ask1 || avgPrice2 != ask2 {
							t.TUI.LogEvent("[%s] 📊 Walked book: %s@$%.3f, %s@$%.3f",
								t.ID, t.Outcomes[0], avgPrice1, t.Outcomes[1], avgPrice2)
						}

						// Record both sides - force market orders always fill
						t.TUI.RecordOrder(t.ID, t.Outcomes[0], "BUY", filled1, avgPrice1, actualCost1, margin, "FILLED")
						t.TUI.RecordOrder(t.ID, t.Outcomes[1], "BUY", filled2, avgPrice2, actualCost2, margin, "FILLED")

						// INSTANT MERGE: Immediately merge to realize profit
						// This matches realbot behavior and ensures round PnL is accurate
						minFilled := filled1
						if filled2 < minFilled {
							minFilled = filled2
						}
						if minFilled > 0 {
							result := t.Engine.MergeForMarket(t.ID, t.Outcomes[0], t.Outcomes[1], minFilled)
							if result.PnL != 0 {
								t.TUI.LogEvent("[%s] 💰 MERGED! +$%.2f profit", t.ID, result.PnL)
							}
						}

						t.LaddersPlaced = true
					}
				}
			}

			// ═══════════════════════════════════════════════════════════════════════════
			// SPLIT STRATEGY SIMULATION: Sell when bid_sum > $1.00 + margin
			// This simulates the panic sell strategy without real blockchain calls
			// ═══════════════════════════════════════════════════════════════════════════
			if len(t.Outcomes) == 2 && marketState == paper.MarketStateActive && liveCfg.SplitStrategyEnabled {
				bid1 := t.TokenBids[t.Outcomes[0]]
				bid2 := t.TokenBids[t.Outcomes[1]]
				currentEquity := t.Engine.GetEquity()

				// Initial split: create simulated inventory
				// Split is always safe - can merge back to USDC anytime at 1:1
				if !t.SplitInitialized {
					baseTradeSize := t.Config.CalculateTradeSize(currentEquity)
					initialBuffer := baseTradeSize * 2.0
					if initialBuffer < MinSplitBuffer {
						initialBuffer = MinSplitBuffer
					}
					maxInitial := currentEquity * t.Config.SplitInitialCapPct
					splitAmount := initialBuffer
					if splitAmount > maxInitial {
						splitAmount = maxInitial
					}
					if splitAmount >= MinSplitAmount {
						t.SplitInventory.RecordSplit(t.ID, t.Outcomes[0], t.Outcomes[1], splitAmount)
						t.Engine.DeductBalance(splitAmount)
						t.Engine.RecalculateDrawdown() // Safe to check drawdown now
						t.SplitInitialized = true
						t.InitialSplitAmount = splitAmount // Store for replenishment target
						t.TUI.LogEvent("[%s] 🔀 SPLIT (sim): Created %.0f shares ($%.2f)", t.ID, splitAmount, splitAmount)
					}
				}

				// Check for panic sell opportunity
				if bid1 > 0.10 && bid2 > 0.10 && bid1 < 0.90 && bid2 < 0.90 {
					bidSum := bid1 + bid2
					sellMargin := (bidSum - 1.0) * 100

					// Background replenishment check
					baseTradeSize := t.Config.CalculateTradeSize(currentEquity)
					targetBuffer := baseTradeSize * t.Config.MaxAggressionMultiplier
					currentShares := t.SplitInventory.GetMinSplitShares(t.ID, t.Outcomes[0], t.Outcomes[1])
					replenishAmount := baseTradeSize * 2.0

					decision := t.ReplenishCtrl.CheckReplenish(paper.ReplenishParams{
						CurrentShares:      currentShares,
						TargetBuffer:       targetBuffer,
						InitialShares:      t.InitialSplitAmount, // Replenish back to initial amount immediately
						SellMargin:         sellMargin,
						MinMarginThreshold: t.Config.SplitMinMarginSell - 1.0,
						CurrentBalance:     currentEquity,
						ReplenishAmount:    replenishAmount,
						MaxBalancePercent:  t.Config.SplitReplenishCapPct,
					})

					if decision.ShouldReplenish && t.ReplenishCtrl.MarkInProgress() {
						// Simulate replenishment - use exact amount needed to reach initial
						actualReplenish := decision.Amount
						t.SplitInventory.RecordSplit(t.ID, t.Outcomes[0], t.Outcomes[1], actualReplenish)
						t.Engine.DeductBalance(actualReplenish)
						t.Engine.RecalculateDrawdown() // Safe to check drawdown now
						t.TUI.LogEvent("[%s] 🔄 SPLIT (sim): Replenished +%.0f shares (now %.0f)", t.ID, actualReplenish, t.InitialSplitAmount)
						t.ReplenishCtrl.MarkComplete()
					}

					// Panic sell logic
					if sellMargin >= t.Config.SplitMinMarginSell && time.Since(t.LastSplitSell) > 2*time.Second {
						requestedShares := baseTradeSize
						if t.Config.EnableMarginAggression {
							multiplier := sellMargin / 2.0
							if multiplier > t.Config.MaxAggressionMultiplier {
								multiplier = t.Config.MaxAggressionMultiplier
							}
							if multiplier < 1.0 {
								multiplier = 1.0
							}
							requestedShares = baseTradeSize * multiplier
						}

						availableShares := t.SplitInventory.GetMinSplitShares(t.ID, t.Outcomes[0], t.Outcomes[1])
						sharesToSell := requestedShares
						if sharesToSell > availableShares {
							if availableShares >= 1.0 {
								sharesToSell = availableShares
							} else {
								sharesToSell = 0
							}
						}

						if sharesToSell >= 1.0 {
							if sharesToSell > MaxSharesPerSell {
								sharesToSell = MaxSharesPerSell
							}

							// Calculate liquidity depth for display (similar to ARB buy)
							bids1 := t.TokenFullBids[t.Outcomes[0]]
							bids2 := t.TokenFullBids[t.Outcomes[1]]
							bookDepth1, bookDepth2 := len(bids1), len(bids2)

							// Calculate matched liquidity across valid bid levels
							minSum := 1.0 + (t.Config.SplitMinMarginSell / 100.0)
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
								if sortedBids1[bi].Price+sortedBids2[bj].Price < minSum-1e-6 {
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

							// Simulate sell: record profit
							profit1 := t.SplitInventory.RecordSell(t.ID, t.Outcomes[0], sharesToSell, bid1)
							profit2 := t.SplitInventory.RecordSell(t.ID, t.Outcomes[1], sharesToSell, bid2)
							totalProfit := profit1 + profit2

							// Add proceeds back to balance
							proceeds := sharesToSell * bidSum
							t.Engine.AddBalance(proceeds)
							t.Engine.AddRealizedPnL(totalProfit)
							t.Engine.RecalculateDrawdown() // Safe to check drawdown now

							// Enhanced log with liquidity and depth info (same format as ARB buy)
							t.TUI.LogEvent("[%s] 📈 SPLIT SELL! %s@$%.2f + %s@$%.2f = $%.3f (%.1f%%) | %.0f shares, profit $%.2f [liq: %.0f/%.0f, levels used: %d/%d (total depth: %d/%d)]",
								t.ID, t.Outcomes[0], bid1, t.Outcomes[1], bid2, bidSum, sellMargin, sharesToSell, totalProfit,
								rawLiq1, rawLiq2, maxValidI, maxValidJ, bookDepth1, bookDepth2)
							t.TUI.RecordOrder(t.ID, "SPLIT_SELL", "SELL", sharesToSell*2, bidSum/2, proceeds, sellMargin, "FILLED")
							t.LastSplitSell = time.Now()
						}
					}
				}

				// End-of-market merge: merge remaining split shares before expiry
				timeToEnd := time.Until(t.EndTime)
				if timeToEnd < 30*time.Second && timeToEnd > 0 {
					remainingShares := t.SplitInventory.GetMinSplitShares(t.ID, t.Outcomes[0], t.Outcomes[1])
					if remainingShares >= 1.0 {
						merged := t.SplitInventory.RecordMerge(t.ID, t.Outcomes[0], t.Outcomes[1], remainingShares)
						t.Engine.AddBalance(merged) // $1 per merged pair
						t.Engine.RecalculateDrawdown() // Safe to check drawdown now
						t.TUI.LogEvent("[%s] 💰 SPLIT MERGE (sim): Merged %.0f shares → $%.2f", t.ID, merged, merged)
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

// handleRestFallback polls REST API for fresh liquidity data
// REST is now the PRIMARY source for liquidity (WS only sends price changes)
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
		start := time.Now()
		book, err := t.RestClient.GetOrderBook(restCtx, tokenID)
		latency := time.Since(start)
		restCancel()

		// Update TUI with real REST latency
		t.TUI.UpdateRestLatency(latency)

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
		t.TokenFullBids[outcome] = mkt.LevelsToPriceDepth(book.Bids)
		t.TokenFullAsks[outcome] = mkt.LevelsToPriceDepth(book.Asks)
		t.mu.Unlock()

		// Count as success if we got any valid data
		if bid > 0 || (ask > 0 && ask < 1.0) || len(book.Bids) > 0 || len(book.Asks) > 0 {
			restSuccess++
		}
	}

	// Log result - minimal spam, maximum info
	if restSuccess > 0 {
		t.LastUpdate = time.Now()
		t.TUI.UpdateMarketPricesWithSource(t.ID, t.TokenBids, t.TokenAsks, "REST")
		// Only log recovery if WS was significantly stale (not normal polling)
		if staleSeconds >= 10 {
			t.TUI.LogEvent("[%s] ✅ REST recovered after %ds", t.ID, staleSeconds)
		}
		return true
	} else if restErrors > 0 {
		// Log errors every 10 seconds to avoid spam
		if staleSeconds%10 == 0 || staleSeconds == 10 {
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
