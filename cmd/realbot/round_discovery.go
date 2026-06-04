package main

import (
	"context"
	"os"
	"sort"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	mkt "Market-bot/internal/markets"
	"Market-bot/internal/paper"
)

type realbotRoundDiscovery struct {
	markets         map[string]*api.Market
	conditionIDs    []string
	copytradeTarget realbotCopytradeTarget
}

func realbotStopCopytradeWatchers(current **realbotCopytradeWatcherSet) {
	if current == nil || *current == nil {
		return
	}
	(*current).stop()
	*current = nil
}

func realbotConditionIDsForMarkets(markets map[string]*api.Market) []string {
	condSet := make(map[string]struct{}, len(markets))
	condIDs := make([]string, 0, len(markets))
	for _, market := range markets {
		if market == nil || market.ConditionID == "" {
			continue
		}
		if _, exists := condSet[market.ConditionID]; exists {
			continue
		}
		condSet[market.ConditionID] = struct{}{}
		condIDs = append(condIDs, market.ConditionID)
	}
	sort.Strings(condIDs)
	return condIDs
}

func realbotDiscoverRound(ctx context.Context, arbMode string, restClient *api.RestClient, tui *paper.TUI, liveSettings paper.TUISettings, copytradeWatchers **realbotCopytradeWatcherSet) (*realbotRoundDiscovery, time.Duration, error) {
	if arbMode != paperArbModeCopytrade {
		realbotStopCopytradeWatchers(copytradeWatchers)
	}

	tui.LogEvent("🔍 Scanning markets...")

	discovery := &realbotRoundDiscovery{}
	if arbMode == paperArbModeCopytrade {
		resolveCtx, resolveCancel := context.WithTimeout(ctx, 5*time.Second)
		target, targetErr := realbotResolveCopytradeTarget(resolveCtx, restClient, liveSettings)
		resolveCancel()
		if targetErr != nil {
			realbotStopCopytradeWatchers(copytradeWatchers)
			return nil, 10 * time.Second, targetErr
		}
		discovery.copytradeTarget = target
		tui.LogEvent("🪞 Copytrade target %s → %s", target.Raw, target.Wallet)
	}

	discovery.markets = mkt.FindMarkets(ctx, restClient, tui.GetSettings, func(format string, args ...interface{}) {
		tui.LogEvent(format, args...)
	})
	if len(discovery.markets) == 0 {
		realbotStopCopytradeWatchers(copytradeWatchers)
		return nil, 30 * time.Second, nil
	}

	discovery.conditionIDs = realbotConditionIDsForMarkets(discovery.markets)
	return discovery, 0, nil
}

func realbotPrepareCopytradeRound(ctx context.Context, cfg *core.Config, polygonClient *api.PolygonClient, restClient *api.RestClient, tui *paper.TUI, discovery *realbotRoundDiscovery, copytradeWatchers **realbotCopytradeWatcherSet) *realbotCopytradePoller {
	if discovery == nil || discovery.copytradeTarget.Wallet == "" {
		realbotStopCopytradeWatchers(copytradeWatchers)
		return nil
	}

	copytradePoller := newRealbotCopytradePoller(discovery.copytradeTarget.Wallet, discovery.conditionIDs)
	if copytradePoller == nil {
		realbotStopCopytradeWatchers(copytradeWatchers)
		return nil
	}

	trackedMarkets := make([]*api.Market, 0, len(discovery.markets))
	for _, market := range discovery.markets {
		if market != nil {
			trackedMarkets = append(trackedMarkets, market)
		}
	}
	chainWSURL := api.ResolvePolygonWSURL(os.Getenv("POLYGON_WS_URL"), cfg.PolygonRPCURL)
	pendingWSURL := ""
	if cfg.CopytradeUseMempool {
		pendingWSURL = api.ResolvePolymarketPendingWSURL(os.Getenv("COPYTRADE_PENDING_WS_URL"), cfg.PolygonRPCURL)
	}
	*copytradeWatchers = ensureRealbotCopytradeWatcherSet(
		ctx,
		*copytradeWatchers,
		discovery.copytradeTarget.Wallet,
		chainWSURL,
		pendingWSURL,
		polygonClient,
		restClient,
		trackedMarkets,
		func(format string, args ...interface{}) {
			tui.LogEvent(format, args...)
		},
	)
	if *copytradeWatchers != nil {
		(*copytradeWatchers).attach(copytradePoller)
	}
	if !realbotCopytradeHasOnchainWatcher(copytradePoller) {
		tui.LogEvent("⚠️ Copytrade disabled: Polygon WS RPC watcher is required; public trades/positions API fallback is off")
	} else {
		tui.LogEvent("ℹ️ Copytrade WS-only mode active; public trades/positions API disabled")
		if !realbotCopytradeHasPendingWatcher(copytradePoller) {
			tui.LogEvent("ℹ️ Copytrade running in mined/onchain mode only; pending filtering requires Alchemy, so fills can trail the master")
		}
	}
	return copytradePoller
}
