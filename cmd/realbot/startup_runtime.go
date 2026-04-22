package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

type realbotCLOBWarmer struct {
	client *api.RestClient
	trader *trading.RealTrader
}

func init() {
	runEntrypoint = run
}

func realbotLoadStartupPositionsAsync(ctx context.Context, realTrader *trading.RealTrader, restClient *api.RestClient, engine *paper.Engine, tui *paper.TUI, resolutionCache *api.ResolutionCache) {
	if realTrader == nil || realTrader.IsEmbeddedPaperMode() || restClient == nil || engine == nil || tui == nil {
		return
	}

	go func() {
		tui.LogEvent("⏳ Loading existing positions in background...")

		positions, err := realbotSyncRuntimePositions(ctx, realTrader, 10*time.Second)
		if err != nil {
			tui.LogEvent("⚠️ Failed to load existing positions: %v", err)
			return
		}

		recoveredPositions := 0
		if recovered, recoveredCount, recoverErr := realbotRecoverStartupCarryPositions(ctx, realTrader, restClient, positions); recoverErr != nil {
			tui.LogEvent("⚠️ Startup carry recovery incomplete: %v", recoverErr)
			positions = recovered
			recoveredPositions = recoveredCount
		} else {
			positions = recovered
			recoveredPositions = recoveredCount
		}
		if len(positions) == 0 {
			return
		}

		positions, skippedPositions, skippedShares := realbotFilterStartupCarryPositions(ctx, realTrader, positions)
		positions, skippedDustPositions, skippedDustShares := realbotFilterDustStartupCarryPositions(positions)
		startupCarryMarkets := realbotStartupCarryMarkets(ctx, realTrader, positions)
		for _, carry := range startupCarryMarkets {
			tui.AddMarket(carry.MarketID, carry.Slug, carry.Outcomes, carry.EndTime)
		}
		for _, pos := range positions {
			engine.SyncExternalPosition(realbotStartupCarryMarketID(pos), pos.Outcome, pos.Size, pos.AvgPrice)
		}
		tui.LogEvent("✅ Loaded %d startup carry position(s)", len(positions))
		if recoveredPositions > 0 {
			tui.LogEvent("🧾 Recovered %d startup carry position(s) from wallet scan", recoveredPositions)
		}
		if skippedPositions > 0 {
			tui.LogEvent("⏭️ Ignored %d resolved losing startup position(s) (%.2f shares)", skippedPositions, skippedShares)
		}
		if skippedDustPositions > 0 {
			tui.LogEvent("⏭️ Ignored %d dust startup carry position(s) (%.4f shares below %.2f-share redemption minimum)", skippedDustPositions, skippedDustShares, minOnChainActionShares)
		}
		for _, carry := range startupCarryMarkets {
			if carry.ConditionID == "" || len(carry.Outcomes) == 0 || carry.EndTime.IsZero() || carry.EndTime.After(time.Now()) {
				continue
			}
			realbotLaunchRedemptionCheck(carry.MarketID, carry.ConditionID, carry.Outcomes, carry.EndTime, realTrader, engine, tui, resolutionCache)
		}
	}()
}

func run() error {
	startTime := time.Now()
	fmt.Print("\033[H\033[2J")

	fmt.Println("╔═══════════════════════════════════════════════════════╗")
	fmt.Println("║     POLYMARKET REAL TRADING BOT                       ║")
	fmt.Println("║     ⚠️  WARNING: This uses REAL money! ⚠️              ║")
	fmt.Println("╚═══════════════════════════════════════════════════════╝")
	fmt.Printf("⏰ Started at: %s\n", startTime.Format("2006-01-02 15:04:05"))
	fmt.Println()

	cfg, err := core.LoadBotConfig("realbot")
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	applyRealbotTUISettings(cfg, realbotTUISettingsFromConfig(cfg))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.RequireConfirm || !cfg.StartupWizardSeen {
		startupSettings, confirmed, err := paper.RunStartupWizard(paper.StartupWizardOptions{
			Title:          "REALBOT STARTUP",
			ProfileLabel:   "single entrypoint, configurable backend",
			Mode:           "Real",
			Settings:       realbotTUISettingsFromConfig(cfg),
			FirstRun:       !cfg.StartupWizardSeen,
			RequireConfirm: cfg.RequireConfirm,
		})
		if err != nil {
			return fmt.Errorf("startup wizard failed: %w", err)
		}
		if !confirmed {
			fmt.Println("Startup cancelled.")
			return nil
		}
		applyRealbotTUISettings(cfg, startupSettings)
		cfg.StartupWizardSeen = true
		if err := cfg.SaveSettings(); err != nil {
			return fmt.Errorf("failed to save startup settings: %w", err)
		}
	}

	backendState, err := realbotInitBackend(ctx, cfg)
	if err != nil {
		return err
	}
	realTrader := backendState.trader
	polygonClient := backendState.polygonClient
	balance := backendState.startingBalance

	fmt.Println()
	fmt.Println("🛡️  Safety Settings:")
	fmt.Printf("   • Max trade size: $%.2f\n", cfg.MaxTradeSize)
	if cfg.MaxDailyLoss > 0 {
		fmt.Printf("   • Max daily loss: $%.2f\n", cfg.MaxDailyLoss)
	} else {
		fmt.Println("   • Max daily loss: disabled (using 10% drawdown kill switch)")
	}
	fmt.Printf("   • Buy/sell execution margin floor: %.1f%%\n", cfg.BuyExecutionMarginFloorPercent)
	fmt.Println()

	restClient := api.NewRestClient(cfg.Exchange)
	resolutionCache := realbotNewResolutionCache(polygonClient, realTrader, restClient)

	defer func() {
		if r := recover(); r != nil {
			core.RestoreTerminal()
			stack := make([]byte, 4096)
			length := runtime.Stack(stack, false)
			fmt.Printf("\n🚨 PANIC: %v\n%s\n", r, stack[:length])
			realbotEmergencyCleanup(realTrader)
		}
	}()

	go func() {
		<-ctx.Done()
		forceCh := make(chan os.Signal, 1)
		signal.Notify(forceCh, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-forceCh
			core.RestoreTerminal()
			fmt.Println("\n⚠️ Force exit requested")
			os.Exit(1)
		}()

		time.Sleep(10 * time.Second)
		core.RestoreTerminal()
		fmt.Println("\n⚠️ Force exit: cleanup timed out")
		os.Exit(1)
	}()

	disableEcho := exec.Command("stty", "-echo", "-icanon")
	disableEcho.Stdin = os.Stdin
	_ = disableEcho.Run()
	defer core.RestoreTerminal()

	engine := paper.NewEngine(balance)
	orderBook := paper.NewOrderBook()
	realTrader = realbotBindBackendTrader(cfg, engine, backendState)
	resolutionCache = realbotNewResolutionCache(polygonClient, realTrader, restClient)
	tui := paper.NewTUI(engine, orderBook)
	tui.SetMode("Real")

	if err := os.MkdirAll("logs", 0o755); err != nil {
		fmt.Printf("⚠️  Could not create logs directory: %v\n", err)
	} else {
		issueLogPath := filepath.Join("logs", "realbot-issues.csv")
		issueLogger, logErr := core.NewCSVLogger(issueLogPath)
		if logErr != nil {
			fmt.Printf("⚠️  Could not start critical issue logger: %v\n", logErr)
		} else {
			tui.SetIssueLogger(issueLogger)
			defer tui.CloseIssueLogger()
			fmt.Printf("📝 Critical issue log: %s\n", issueLogPath)
		}
	}
	backendRuntimeStop := realbotStartSessionBackendRuntime(ctx, cfg, realTrader, engine, tui, restClient, resolutionCache)
	defer func() {
		if backendRuntimeStop != nil {
			backendRuntimeStop()
		}
	}()

	realbotInitSettingsRuntime(tui, cfg, restClient)

	if UseLiveUI {
		tui.StartRenderLoop(realbotUIInterval(tui.GetSettings()), stop)
		defer tui.Stop()
	}

	go realbotRefreshWalletCashDisplay(ctx, realTrader, tui, 8*time.Second)
	realbotLogGasBalance(ctx, polygonClient, realTrader, tui, 8*time.Second)
	realbotLoadStartupPositionsAsync(ctx, realTrader, restClient, engine, tui, resolutionCache)

	go func() {
		ticker := time.NewTicker(realbotHealthProbeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				start := time.Now()
				pingCtx, cancel := context.WithTimeout(ctx, realbotHealthProbeTimeout)
				_, err := restClient.GetMarketsByTimeframe(pingCtx, []string{"btc"}, "15m")
				cancel()
				if err == nil {
					tui.UpdateLatency(time.Since(start))
				}
			}
		}
	}()

	// realbotStartPositionSyncLoop(ctx, realTrader, tui) // Disabled: REST polling overwrites real-time WS fills
	globalSplitStatus := make(map[string]bool)
	globalSplitInventories := make(map[string]*paper.SplitInventory)
	globalInitialSplits := make(map[string]float64)
	var splitMu sync.Mutex
	var splitTxMu sync.Mutex
	entryGate := newRealbotEntryGate()
	ladderCloseState := newRealbotLadderCloseState()
	currentBalance := balance
	var copytradeWatchers *realbotCopytradeWatcherSet
	defer func() {
		if copytradeWatchers != nil {
			copytradeWatchers.stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			goto shutdown
		default:
		}

		if tui.GetAndClearRestart() {
			tui.LogEvent("🔄 Settings saved. Applying before next round...")
		}

		if !realbotTraderMatchesExecutionBackend(cfg, realTrader) {
			oldBackend := realbotExecutionBackendForTrader(realTrader)
			desiredBackend := realbotDesiredExecutionBackend(cfg)
			tui.LogEvent("🔁 Switching execution backend to %s...", desiredBackend)

			switchCtx, cancelSwitch := context.WithTimeout(ctx, 2*time.Minute)
			nextBackendState, nextTrader, switchErr := realbotSwitchExecutionBackend(switchCtx, cfg, engine, realTrader, realbotInitBackend)
			cancelSwitch()
			if switchErr != nil {
				tui.SetExecutionBackend(oldBackend)
				applyRealbotTUISettings(cfg, tui.GetSettings())
				if saveErr := cfg.SaveSettings(); saveErr != nil {
					tui.LogEvent("⚠️ Failed to restore execution backend setting: %v", saveErr)
				}
				tui.LogEvent("⚠️ Execution backend switch failed: %v. Staying on %s.", switchErr, oldBackend)
			} else if nextBackendState != nil {
				if backendRuntimeStop != nil {
					backendRuntimeStop()
				}
				backendState = nextBackendState
				realTrader = nextTrader
				polygonClient = backendState.polygonClient
				currentBalance = backendState.startingBalance
				resolutionCache = realbotNewResolutionCache(polygonClient, realTrader, restClient)
				backendRuntimeStop = realbotStartSessionBackendRuntime(ctx, cfg, realTrader, engine, tui, restClient, resolutionCache)
				tui.LogEvent("✅ Execution backend active: %s", desiredBackend)
				go realbotRefreshWalletCashDisplay(ctx, realTrader, tui, 8*time.Second)
				realbotLogGasBalance(ctx, polygonClient, realTrader, tui, 8*time.Second)
				realbotLoadStartupPositionsAsync(ctx, realTrader, restClient, engine, tui, resolutionCache)
			}
		}

		roundSnapshot, updatedBalance := realbotBeginRound(ctx, realTrader, engine, tui, currentBalance)
		currentBalance = updatedBalance

		liveSettings := tui.GetSettings()
		arbMode := normalizePaperArbMode(liveSettings.PaperArbMode)
		discovery, retryDelay, discoverErr := realbotDiscoverRound(ctx, arbMode, restClient, tui, liveSettings, &copytradeWatchers)
		if discoverErr != nil {
			tui.LogEvent("⚠️ Copytrade target unavailable: %v", discoverErr)
			select {
			case <-time.After(retryDelay):
				continue
			case <-ctx.Done():
				goto shutdown
			}
		}
		if discovery == nil {
			tui.LogEvent("⏳ No active markets found, waiting 30s before retry...")
			select {
			case <-time.After(retryDelay):
				continue
			case <-ctx.Done():
				goto shutdown
			}
		}
		markets := discovery.markets
		condIDs := discovery.conditionIDs

		if err := realTrader.SubscribeUserWSMarkets(ctx, condIDs...); err != nil {
			tui.LogEvent("⚠️ User WS subscription update failed: %v", err)
		}

		roundCtx, roundCancel := context.WithCancel(ctx)

		copytradePoller := (*realbotCopytradePoller)(nil)
		if arbMode == paperArbModeCopytrade {
			copytradePoller = realbotPrepareCopytradeRound(ctx, cfg, polygonClient, restClient, tui, discovery, &copytradeWatchers)
		}

		wg := realbotLaunchRoundMarkets(ctx, roundCtx, markets, realTrader, engine, orderBook, tui, restClient, cfg, currentBalance, copytradePoller, globalSplitStatus, globalSplitInventories, globalInitialSplits, &splitMu, &splitTxMu, entryGate, ladderCloseState, resolutionCache)
		realbotStartRoundRestartMonitor(roundCtx, roundCancel, tui)

		if !realbotWaitForRound(ctx, roundCtx, roundCancel, wg, tui) {
			goto shutdown
		}

		realbotFinalizeRound(ctx, markets, realTrader, engine, globalSplitInventories, &splitMu, tui, restClient, orderBook, roundSnapshot)

		if elapsed := time.Since(roundSnapshot.startedAt); elapsed < 10*time.Second {
			select {
			case <-time.After(10*time.Second - elapsed):
			case <-ctx.Done():
				goto shutdown
			}
		}
	}

shutdown:
	tui.Stop()
	fmt.Println("\n👋 Bot stopped.")
	// realbotEmergencyCleanup(realTrader) // Disabled terminal cleanup
	return nil
}
