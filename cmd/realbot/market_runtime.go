package main

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

type realbotMarketRuntime struct {
	embeddedPaperMode      bool
	binanceFeed            *api.BinanceFuturesPriceFeed
	splitInventory         *paper.SplitInventory
	mergeCoordinator       *realbotMergeCoordinator
	replenishCtrl          *paper.ReplenishController
	copytradeState         *realbotCopytradeState
	entryExecutionDone     chan realbotAsyncEntryResult
	refreshWalletTruth     func(time.Duration)
	restFallbackQuoteAge   time.Duration
	restFallbackPollPeriod time.Duration
}

func realbotGetOrCreateSplitInventory(conditionID string, globalSplitInventories map[string]*paper.SplitInventory, splitMu *sync.Mutex) *paper.SplitInventory {
	splitMu.Lock()
	defer splitMu.Unlock()

	if splitInventory, exists := globalSplitInventories[conditionID]; exists {
		return splitInventory
	}

	splitInventory := paper.NewSplitInventory()
	globalSplitInventories[conditionID] = splitInventory
	return splitInventory
}

func realbotInitMarketBinanceFeed(ctx context.Context, marketID string, cfg *core.Config, arbMode string) *api.BinanceFuturesPriceFeed {
	if arbMode != paperArbModeBinanceGap {
		return nil
	}
	symbol := realbotBinanceSymbolForMarket(marketID, cfg)
	if symbol == "" {
		return nil
	}

	binanceFeed := api.NewBinanceFuturesPriceFeed(symbol, core.ResolveBinanceSignalLookback(cfg))
	binanceFeed.Start(ctx)
	return binanceFeed
}

func realbotWalletTruthTokenIDs(tokenToOutcome map[string]string) []string {
	tokenIDs := make([]string, 0, len(tokenToOutcome))
	for tokenID := range tokenToOutcome {
		tokenIDs = append(tokenIDs, tokenID)
	}
	sort.Strings(tokenIDs)
	return tokenIDs
}

func realbotNewWalletTruthRefresher(ctx context.Context, marketID string, tokenToOutcome map[string]string, trader *trading.RealTrader, engine *paper.Engine, splitInventory *paper.SplitInventory, tui *paper.TUI) func(time.Duration) {
	walletTruthTokenIDs := realbotWalletTruthTokenIDs(tokenToOutcome)
	return func(timeout time.Duration) {
		if trader == nil || trader.IsPaperMode() {
			return
		}
		if len(walletTruthTokenIDs) > 0 {
			trader.InvalidateCTFBalanceCache(walletTruthTokenIDs...)
		}
		truthCtx, truthCancel := context.WithTimeout(ctx, timeout)
		defer truthCancel()
		if _, err := syncWalletTruthPositions(truthCtx, marketID, tokenToOutcome, trader, engine, splitInventory, tui); err != nil {
			tui.LogEventDedup("wallet-truth-refresh:"+marketID+":"+strings.TrimSpace(err.Error()), 15*time.Second,
				"[%s] ⚠️ Wallet-truth refresh failed: %v", marketID, err)
		}
	}
}

func realbotInitMarketRuntime(ctx context.Context, marketID, conditionID string, tokenToOutcome map[string]string, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI, cfg *core.Config, globalSplitInventories map[string]*paper.SplitInventory, splitMu *sync.Mutex) *realbotMarketRuntime {
	splitInventory := realbotGetOrCreateSplitInventory(conditionID, globalSplitInventories, splitMu)
	engine.RegisterSplitInventory(splitInventory)
	tui.RegisterSplitInventory(splitInventory)

	refreshWalletTruth := realbotNewWalletTruthRefresher(ctx, marketID, tokenToOutcome, trader, engine, splitInventory, tui)
	refreshWalletTruth(5 * time.Second)

	return &realbotMarketRuntime{
		embeddedPaperMode:      trader != nil && trader.IsEmbeddedPaperMode(),
		binanceFeed:            realbotInitMarketBinanceFeed(ctx, marketID, cfg, tui.GetSettings().PaperArbMode),
		splitInventory:         splitInventory,
		mergeCoordinator:       newRealbotMergeCoordinator(),
		replenishCtrl:          paper.NewReplenishController(),
		copytradeState:         newRealbotCopytradeState(),
		entryExecutionDone:     make(chan realbotAsyncEntryResult, 50),
		refreshWalletTruth:     refreshWalletTruth,
		restFallbackQuoteAge:   core.ResolveRestFallbackQuoteAge(cfg),
		restFallbackPollPeriod: core.ResolveRestFallbackPollInterval(cfg),
	}
}

func realbotClearWalletTruthOnExit(tui *paper.TUI, marketID string, preserve bool) {
	if preserve {
		return
	}
	tui.ClearWalletTruthPositions(marketID)
}
