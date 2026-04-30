package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/setup"
	"Market-bot/internal/trading"
)

type realbotBackendState struct {
	trader          *trading.RealTrader
	polygonClient   *api.PolygonClient
	startingBalance float64
	embeddedPaper   bool
}

type realbotBackendSetupFunc func(context.Context, *core.Config) (*realbotBackendState, error)

func realbotInitBackend(ctx context.Context, cfg *core.Config) (*realbotBackendState, error) {
	state := &realbotBackendState{
		startingBalance: cfg.PaperBalance,
		embeddedPaper:   strings.EqualFold(strings.TrimSpace(cfg.ExecutionBackend), core.ExecutionBackendPaper),
	}

	if strings.TrimSpace(cfg.PolygonRPCURL) != "" {
		state.polygonClient = api.NewPolygonClient(cfg.PolygonRPCURL)
	}

	if state.embeddedPaper {
		fmt.Println("🧪 Embedded paper backend enabled inside realbot")
		return state, nil
	}

	setupCtx, cancelSetup := context.WithTimeout(ctx, 2*time.Minute)
	defer cancelSetup()

	trader, err := setup.EnsureRealTradingSetup(setupCtx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to setup or create trader: %w", err)
	}
	state.trader = trader

	fmt.Println("🔄 Syncing CLOB balance allowance...")
	if err := trader.UpdateBalanceAllowance(ctx); err != nil {
		fmt.Printf("⚠️  Failed to update balance allowance: %v\n", err)
	} else {
		fmt.Println("✅ CLOB balance allowance synced")
	}

	fmt.Println("🔌 Preparing User WebSocket for real-time fills...")
	if err := trader.StartUserWS(ctx); err != nil {
		fmt.Printf("⚠️  Failed to connect User WS (fill confirmation will wait on WS timeout only): %v\n", err)
	} else {
		fmt.Println("✅ User WebSocket ready")
	}

	fmt.Println()
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Printf("🔑 Wallet: %s\n", trader.Address())

	initCtx, cancelInit := context.WithTimeout(ctx, 30*time.Second)
	defer cancelInit()

	balance, err := trader.ForceRefreshBalance(initCtx)
	if err != nil {
		fmt.Printf("⚠️  Could not fetch balance: %v\n", err)
	} else {
		state.startingBalance = balance
		fmt.Printf("💵 Available Balance: $%.2f USDC\n", balance)
	}

	fmt.Println("═══════════════════════════════════════════════════════")
	return state, nil
}

func realbotNewResolutionCache(polygonClient *api.PolygonClient, trader *trading.RealTrader, restClient *api.RestClient) *api.ResolutionCache {
	var resolutionExchange api.ExchangeClient
	if trader != nil {
		resolutionExchange = trader.Exchange()
	}
	return api.NewResolutionCache(polygonClient, resolutionExchange, restClient)
}

func realbotBindBackendTrader(cfg *core.Config, engine *paper.Engine, state *realbotBackendState) *trading.RealTrader {
	if state != nil && state.trader != nil {
		return state.trader
	}
	return trading.NewEmbeddedPaperRealTrader(cfg, engine)
}

func realbotDesiredExecutionBackend(cfg *core.Config) string {
	if cfg != nil && strings.EqualFold(strings.TrimSpace(cfg.ExecutionBackend), core.ExecutionBackendLive) {
		return core.ExecutionBackendLive
	}
	return core.ExecutionBackendPaper
}

func realbotExecutionBackendForTrader(trader *trading.RealTrader) string {
	if trader != nil && !trader.IsEmbeddedPaperMode() {
		return core.ExecutionBackendLive
	}
	return core.ExecutionBackendPaper
}

func realbotTraderMatchesExecutionBackend(cfg *core.Config, trader *trading.RealTrader) bool {
	return realbotDesiredExecutionBackend(cfg) == realbotExecutionBackendForTrader(trader)
}

func realbotBuildEmbeddedPaperBackend(cfg *core.Config, engine *paper.Engine) (*realbotBackendState, *trading.RealTrader) {
	state := &realbotBackendState{
		startingBalance: cfg.PaperBalance,
		embeddedPaper:   true,
	}
	if strings.TrimSpace(cfg.PolygonRPCURL) != "" {
		state.polygonClient = api.NewPolygonClient(cfg.PolygonRPCURL)
	}
	return state, trading.NewEmbeddedPaperRealTrader(cfg, engine)
}

func realbotSwitchExecutionBackend(ctx context.Context, cfg *core.Config, engine *paper.Engine, currentTrader *trading.RealTrader, setupLive realbotBackendSetupFunc) (*realbotBackendState, *trading.RealTrader, error) {
	if realbotTraderMatchesExecutionBackend(cfg, currentTrader) {
		return nil, currentTrader, nil
	}

	switch realbotDesiredExecutionBackend(cfg) {
	case core.ExecutionBackendLive:
		if setupLive == nil {
			setupLive = realbotInitBackend
		}
		state, err := setupLive(ctx, cfg)
		if err != nil {
			return nil, currentTrader, err
		}
		if state == nil || state.trader == nil {
			return nil, currentTrader, fmt.Errorf("live backend setup returned no trader")
		}
		if engine != nil {
			engine.ForceResetPaperSession(state.startingBalance)
		}
		return state, state.trader, nil
	default:
		state, trader := realbotBuildEmbeddedPaperBackend(cfg, engine)
		if engine != nil {
			if engine.CanResetPaperSession() {
				if err := engine.ResetPaperSession(state.startingBalance); err != nil {
					return nil, currentTrader, err
				}
			} else {
				engine.RebaseBalance(state.startingBalance)
			}
		}
		return state, trader, nil
	}
}

func realbotStartBackendRuntime(ctx context.Context, cfg *core.Config, trader *trading.RealTrader, logf func(string, ...interface{})) func() {
	realbotStartLiveRuntimeWatchers(ctx, cfg, trader, logf)
	return realbotStartRawAPILog(cfg, trader)
}

func realbotStartSessionBackendRuntime(ctx context.Context, cfg *core.Config, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI, restClient *api.RestClient, resolutionCache *api.ResolutionCache) func() {
	runtimeCtx, cancel := context.WithCancel(ctx)
	closeRawLog := realbotStartBackendRuntime(runtimeCtx, cfg, trader, tui.LogEvent)
	realbotStartBalanceSyncLoop(runtimeCtx, trader, engine, tui)
	realbotStartEmbeddedPaperResolutionSweep(runtimeCtx, trader, engine, tui, restClient, resolutionCache)
	return func() {
		cancel()
		if closeRawLog != nil {
			closeRawLog()
		}
		_ = trader.CloseUserWS()
	}
}

func realbotInitSettingsRuntime(tui *paper.TUI, cfg *core.Config, restClient *api.RestClient) {
	tui.InitSettings(realbotTUISettingsFromConfig(cfg), func(s paper.TUISettings) {
		previousExecutionBackend := cfg.ExecutionBackend
		applyRealbotTUISettings(cfg, s)
		if restClient != nil && restClient.Exchange != s.Exchange {
			restClient.Exchange = s.Exchange
		}

		if err := cfg.SaveSettings(); err != nil {
			tui.LogEvent("⚠️ Failed to save settings: %v", err)
		}
		if cfg.ExecutionBackend != previousExecutionBackend {
			tui.LogEvent("🔁 Execution backend set to %s. Apply or R restarts the trading loop to switch runtime.", cfg.ExecutionBackend)
		}

		if s.PolygonRPC != cfg.PolygonRPCURL || s.PolygonPrivateKey != cfg.PK {
			if err := setup.UpdatePolymarketCredentials(s.PolygonRPC, s.PolygonPrivateKey); err != nil {
				tui.LogEvent("⚠️ Failed to update credentials: %v", err)
			} else {
				tui.LogEvent("✅ Credentials updated in .env! Restarting bot...")
				time.Sleep(1 * time.Second)
				os.Exit(0)
			}
		}
	})
	tui.SetTradeFactor(cfg.TradeScaleFactor)
	tui.SetMode("Real")
	tui.SetTradingPaused(true)
}

func realbotSyncRuntimeBalance(ctx context.Context, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI, timeout time.Duration) (float64, float64, error) {
	if trader == nil || engine == nil {
		return 0, 0, fmt.Errorf("missing runtime balance dependencies")
	}

	balanceCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	newBal, err := trader.ForceRefreshBalance(balanceCtx)
	if err != nil {
		return 0, 0, err
	}

	syncResult := realbotApplyRuntimeBalanceSync(engine, tui, newBal)
	realbotRefreshWalletCashDisplay(ctx, trader, tui, timeout)
	return newBal, syncResult.NeutralizedDelta, nil
}

func realbotApplyRuntimeBalanceSync(engine *paper.Engine, tui *paper.TUI, balance float64) paper.BalanceSyncResult {
	if engine == nil {
		return paper.BalanceSyncResult{}
	}
	syncResult := engine.SyncBalanceNeutralDetailed(balance)
	engine.RecalculateDrawdown()
	realbotLogBalanceSyncResult(tui, syncResult)
	return syncResult
}

func realbotLogBalanceSyncResult(tui *paper.TUI, syncResult paper.BalanceSyncResult) {
	if tui == nil || math.Abs(syncResult.Delta) < 0.005 {
		return
	}
	switch {
	case math.Abs(syncResult.NeutralizedDelta) >= 0.005:
		tui.LogEventDedup("balance-sync:neutralized", 15*time.Second,
			"🧮 Balance sync neutralized cash Δ%+.2f (raw Δ%+.2f, book $%.2f, realized %+.2f)",
			syncResult.NeutralizedDelta, syncResult.Delta, syncResult.BookEquity, syncResult.RealizedPnL)
	case math.Abs(syncResult.RealizedDelta) >= 0.005:
		tui.LogEventDedup("balance-sync:realized", 15*time.Second,
			"🧾 Balance sync booked external cash drift Δ%+.2f as realized PnL (book $%.2f, realized %+.2f)",
			syncResult.RealizedDelta, syncResult.BookEquity, syncResult.RealizedPnL)
	}
}

func realbotSyncRuntimePositions(ctx context.Context, trader *trading.RealTrader, timeout time.Duration) ([]trading.PositionInfo, error) {
	if trader == nil || trader.IsEmbeddedPaperMode() {
		return nil, nil
	}

	positionCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return trader.ForceRefreshPositions(positionCtx)
}

func realbotStartBalanceSyncLoop(ctx context.Context, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI) {
	go func() {
		ticker := time.NewTicker(realbotBalanceSyncInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _, err := realbotSyncRuntimeBalance(ctx, trader, engine, tui, realbotBalanceSyncTimeout)
				if err != nil {
					continue
				}
			}
		}
	}()
}

func realbotStartPositionSyncLoop(ctx context.Context, trader *trading.RealTrader, tui *paper.TUI) {
	if trader == nil || trader.IsEmbeddedPaperMode() {
		return
	}

	go func() {
		ticker := time.NewTicker(realbotPositionSyncInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := realbotSyncRuntimePositions(ctx, trader, realbotPositionSyncTimeout); err != nil && tui != nil {
					tui.LogEventDedup("position-sync:"+strings.TrimSpace(err.Error()), 15*time.Second,
						"⚠️ Position sync failed: %v", err)
				}
			}
		}
	}()
}

func realbotStartLiveRuntimeWatchers(ctx context.Context, cfg *core.Config, trader *trading.RealTrader, logf func(string, ...interface{})) {
	globalResWatcher = nil
	if trader == nil || trader.IsEmbeddedPaperMode() {
		return
	}

	globalResWatcher = api.NewResolutionWatcher(cfg.PolygonRPCURL)
	if globalResWatcher != nil {
		globalResWatcher.Start(ctx, func(format string, args ...interface{}) {
			logf(format, args...)
		})
	}

	invWatcher := api.NewInventoryWatcher(cfg.PolygonRPCURL, trader.Address())
	if invWatcher != nil {
		invWatcher.Start(ctx, func(format string, args ...interface{}) {
			logf(format, args...)
		})
		invWatcher.RegisterCallback(func() {
			trader.InvalidateCTFBalanceCache()
		})
	}
}

func realbotStartRawAPILog(cfg *core.Config, trader *trading.RealTrader) func() {
	if cfg == nil || trader == nil || trader.IsEmbeddedPaperMode() || !cfg.EnableRawAPILog {
		fmt.Println("⚡ Raw Polymarket API debug log disabled for lower latency")
		return func() {}
	}

	rawAPILogPath := filepath.Join("logs", "realbot-polymarket-raw.jsonl")
	if err := trader.EnableRawAPILog(rawAPILogPath); err != nil {
		fmt.Printf("⚠️  Could not start raw Polymarket API log: %v\n", err)
		return func() {}
	}
	fmt.Printf("🧾 Raw Polymarket debug log: %s\n", rawAPILogPath)
	return func() { _ = trader.CloseRawAPILog() }
}

func realbotEmergencyCleanup(trader *trading.RealTrader) {
	if trader == nil || trader.IsEmbeddedPaperMode() {
		fmt.Println("\n🧹 Skipping live emergency cleanup on paper backend")
		return
	}

	overallCtx, cancelAll := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancelAll()

	fmt.Println("\n🧹 Running emergency cleanup...")

	if err := trader.CancelAll(overallCtx); err != nil {
		fmt.Printf("⚠️  Failed to cancel orders: %v\n", err)
	} else {
		fmt.Println("✅ All orders cancelled")
	}

	positions, err := trader.GetPositions(overallCtx)
	if err != nil {
		fmt.Printf("⚠️  Could not fetch positions for merge: %v\n", err)
		return
	}
	if len(positions) == 0 {
		return
	}

	condToPos := make(map[string][]trading.PositionInfo)
	for _, pos := range positions {
		if pos.ConditionID != "" {
			condToPos[pos.ConditionID] = append(condToPos[pos.ConditionID], pos)
		}
	}

	var wg sync.WaitGroup
	for condID, poses := range condToPos {
		if len(poses) < 2 {
			continue
		}

		minQty := poses[0].Size
		for _, p := range poses {
			if p.Size < minQty {
				minQty = p.Size
			}
		}

		if minQty < minOnChainActionShares {
			continue
		}

		mInfo, err := trader.GetMarketInfo(overallCtx, condID)
		if err != nil {
			fmt.Printf("⚠️  Could not fetch market info for %s: %v\n", condID[:10], err)
			continue
		}
		if len(poses) < len(mInfo.Tokens) {
			continue
		}

		wg.Add(1)
		go func(cID string, mq float64, numOutcomes int) {
			defer wg.Done()
			_ = numOutcomes
			fmt.Printf("ℹ️ Auto-merge disabled; leaving %.6f balanced pairs parked for market %s\n", mq, cID[:10])
		}(condID, minQty, len(mInfo.Tokens))
	}

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
