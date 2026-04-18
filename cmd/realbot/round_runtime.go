package main

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"sync"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	mkt "Market-bot/internal/markets"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

type realbotRoundSnapshot struct {
	startedAt      time.Time
	startingEquity float64
	startRealized  float64
	startTrades    int
}

func realbotBeginRound(ctx context.Context, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI, currentBalance float64) (realbotRoundSnapshot, float64) {
	snapshot := realbotRoundSnapshot{
		startedAt: time.Now(),
	}

	newBal, _, err := realbotSyncRuntimeBalance(ctx, trader, engine, tui, 10*time.Second)
	if err != nil {
		tui.LogEvent("⚠️ Could not refresh balance: %v", err)
	} else {
		currentBalance = newBal
	}

	snapshot.startingEquity = engine.GetBookEquity()
	snapshot.startRealized = engine.GetStats().RealizedPnL
	tui.StartRound()
	snapshot.startTrades = engine.GetStats().TotalTrades
	tui.LogEvent("📊 Balance $%.2f | %.2fx", currentBalance, engine.GetCompoundMultiplier())
	return snapshot, currentBalance
}

func realbotRoundSnapshotPnL(trader *trading.RealTrader, engine *paper.Engine, snapshot realbotRoundSnapshot, endingBookEquity, excludedDelta float64) float64 {
	return realbotNeutralRoundPnL(snapshot.startingEquity, endingBookEquity, excludedDelta)
}

func realbotWaitForRound(ctx, roundCtx context.Context, roundCancel context.CancelFunc, wg *sync.WaitGroup, tui *paper.TUI) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		tui.LogEvent("✅ Markets closed")
	case <-ctx.Done():
		return false
	case <-roundCtx.Done():
		tui.LogEvent("⚠️ Traders stopped for restart...")
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}

	roundCancel()
	return true
}

func realbotFinalizeRound(ctx context.Context, markets map[string]*api.Market, trader *trading.RealTrader, engine *paper.Engine, globalSplitInventories map[string]*paper.SplitInventory, splitMu *sync.Mutex, tui *paper.TUI, restClient *api.RestClient, orderBook *paper.OrderBook, snapshot realbotRoundSnapshot) {
	balanceSyncDelta := 0.0
	if _, delta, err := realbotSyncRuntimeBalance(ctx, trader, engine, tui, 10*time.Second); err == nil {
		balanceSyncDelta = delta
	} else {
		tui.LogEvent("⚠️ Round-end balance sync failed: %v", err)
	}

	reconciliationDelta := 0.0
	preReconcileBookEquity := engine.GetBookEquity()
	if !trader.IsPaperMode() {
		reconcileCtx, reconcileCancel := context.WithTimeout(ctx, 20*time.Second)
		if changed, reconcileErr := realbotReconcileTrackedRoundWalletTruth(reconcileCtx, markets, trader, engine, globalSplitInventories, splitMu, tui); reconcileErr != nil {
			tui.LogEvent("⚠️ Round-end wallet-truth reconciliation incomplete: %v", reconcileErr)
		} else if changed > 0 {
			tui.LogEvent("🧾 Round-end wallet-truth reconciliation restored %d tracked market(s)", changed)
		}
		reconcileCancel()
	}
	reconciliationDelta = engine.GetBookEquity() - preReconcileBookEquity
	if math.Abs(reconciliationDelta) >= 0.005 {
		tui.LogEvent("🧮 Excluding wallet-truth sync delta %+0.2f from round PnL", reconciliationDelta)
	}

	endingBookEquity := engine.GetBookEquity()
	roundPnL := realbotRoundSnapshotPnL(trader, engine, snapshot, endingBookEquity, reconciliationDelta+balanceSyncDelta)
	roundTrades := engine.GetStats().TotalTrades - snapshot.startTrades
	if roundTrades < 0 {
		roundTrades = 0
	}
	tui.RecordRound(snapshot.startingEquity, snapshot.startingEquity+roundPnL, roundPnL, roundTrades, engine.GetPositions(), nil)
	engine.UpdateCompoundMultiplier(roundPnL, snapshot.startingEquity)
	if roundPnL > 0 {
		tui.LogEvent("📈 PROFIT! Round PnL: +$%.2f", roundPnL)
	} else if roundPnL < 0 {
		tui.LogEvent("📉 Loss. Round PnL: $%.2f", roundPnL)
	} else {
		tui.LogEvent("✅ No change")
	}
	tui.LogEvent("🔄 Next round")

	restClient.CloseIdleConnections()
	tui.ClearMarkets()
	orderBook.CancelAllOrders()
	engine.ClearMarketData()
}

func realbotMarketRiskConfig() paper.RiskConfig {
	return paper.RiskConfig{
		DisableKillSwitch:  true,
		MaxExposure:        math.MaxFloat64,
		MaxUnmatchedRatio:  0.20,
		MaxUnmatchedShares: 500.0,
		SkewThreshold:      0.10,
		KillSwitchDrawdown: 0.10,
	}
}

func realbotResolveMarketEndTime(ctx context.Context, trader *trading.RealTrader, market *api.Market) time.Time {
	endTime, _ := paper.ParseEndTimeFromSlug(market.Slug)
	if market == nil || trader == nil {
		return endTime
	}
	if mInfo, err := trader.GetMarketInfo(ctx, market.ConditionID); err == nil && mInfo.EndDateISO != "" {
		if parsed, err := time.Parse(time.RFC3339, mInfo.EndDateISO); err == nil {
			if parsed.After(time.Now()) || mInfo.Closed {
				endTime = parsed
			}
		}
	}
	return endTime
}

func realbotStartMarketWorker(globalCtx, roundCtx context.Context, marketID string, market *api.Market, endTime time.Time, realTrader *trading.RealTrader, engine *paper.Engine, orderBook *paper.OrderBook, marketRiskMgr *paper.RiskManager, tui *paper.TUI, restClient *api.RestClient, cfg *core.Config, currentBalance float64, copytradePoller *realbotCopytradePoller, globalSplitStatus map[string]bool, globalSplitInventories map[string]*paper.SplitInventory, globalInitialSplits map[string]float64, splitMu *sync.Mutex, splitTxMu *sync.Mutex, entryGate *realbotEntryGate, resolutionCache *api.ResolutionCache, wg *sync.WaitGroup) {
	wg.Add(1)
	go func(id string, m *api.Market, end time.Time, r *paper.RiskManager, bal float64, poller *realbotCopytradePoller) {
		defer wg.Done()
		tCtx, tCancel := context.WithCancel(roundCtx)
		defer tCancel()

		defer func() {
			if recovered := recover(); recovered != nil {
				core.RestoreTerminal()
				stack := make([]byte, 4096)
				length := runtime.Stack(stack, false)
				fmt.Printf("\n🚨 TRADER PANIC [%s]: %v\n%s\n", id, recovered, stack[:length])
				realbotEmergencyCleanup(realTrader)
			}
		}()
		tradeMarket(globalCtx, tCtx, id, m, end, realTrader, engine, orderBook, r, tui, restClient, cfg, bal, poller, globalSplitStatus, globalSplitInventories, globalInitialSplits, splitMu, splitTxMu, entryGate, resolutionCache)
	}(marketID, market, endTime, marketRiskMgr, currentBalance, copytradePoller)
}

func realbotLaunchRoundMarkets(globalCtx, roundCtx context.Context, markets map[string]*api.Market, realTrader *trading.RealTrader, engine *paper.Engine, orderBook *paper.OrderBook, tui *paper.TUI, restClient *api.RestClient, cfg *core.Config, currentBalance float64, copytradePoller *realbotCopytradePoller, globalSplitStatus map[string]bool, globalSplitInventories map[string]*paper.SplitInventory, globalInitialSplits map[string]float64, splitMu *sync.Mutex, splitTxMu *sync.Mutex, entryGate *realbotEntryGate, resolutionCache *api.ResolutionCache) *sync.WaitGroup {
	var wg sync.WaitGroup
	for assetID, market := range markets {
		marketID := mkt.ScopedMarketID(assetID, market)
		endTime := realbotResolveMarketEndTime(globalCtx, realTrader, market)
		outcomes := mkt.GetOutcomes(market)
		tui.AddMarket(marketID, market.Slug, outcomes, endTime)
		tui.LogEvent("🚀 %s → %s", marketID, endTime.Format("15:04"))

		marketRiskMgr := paper.NewRiskManager(realbotMarketRiskConfig(), engine, orderBook, outcomes)
		realbotStartMarketWorker(globalCtx, roundCtx, marketID, market, endTime, realTrader, engine, orderBook, marketRiskMgr, tui, restClient, cfg, currentBalance, copytradePoller, globalSplitStatus, globalSplitInventories, globalInitialSplits, splitMu, splitTxMu, entryGate, resolutionCache, &wg)
	}
	return &wg
}

func realbotStartRoundRestartMonitor(roundCtx context.Context, roundCancel context.CancelFunc, tui *paper.TUI) {
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
					roundCancel()
					return
				}
			}
		}
	}()
}
