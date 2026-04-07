package copytradeutil

import (
	"context"
	"strings"

	"Market-bot/internal/api"
)

type RuntimeSetup struct {
	ConditionIDs   []string
	TrackedMarkets []*api.Market
	Poller         *Poller
	Watchers       *WatcherSet
}

type RuntimeSetupOptions struct {
	ParentCtx        context.Context
	CurrentWatchers  *WatcherSet
	Wallet           string
	Markets          map[string]*api.Market
	ChainWSURL       string
	PendingWSURL     string
	PolygonClient    *api.PolygonClient
	RestClient       *api.RestClient
	MinedWatcherMode string
	Logf             func(string, ...interface{})
}

func ConditionIDsForMarkets(markets map[string]*api.Market) []string {
	conditionIDs := make([]string, 0, len(markets))
	for _, market := range markets {
		if market == nil {
			continue
		}
		conditionIDs = append(conditionIDs, strings.TrimSpace(market.ConditionID))
	}
	return NormalizeConditionIDs(conditionIDs)
}

func TrackedMarkets(markets map[string]*api.Market) []*api.Market {
	tracked := make([]*api.Market, 0, len(markets))
	for _, market := range markets {
		if market != nil {
			tracked = append(tracked, market)
		}
	}
	return tracked
}

func SetupRuntime(opts RuntimeSetupOptions) RuntimeSetup {
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...interface{}) {}
	}

	setup := RuntimeSetup{
		ConditionIDs:   ConditionIDsForMarkets(opts.Markets),
		TrackedMarkets: TrackedMarkets(opts.Markets),
	}
	setup.Poller = NewPoller(opts.Wallet, setup.ConditionIDs)
	if setup.Poller == nil {
		if opts.CurrentWatchers != nil {
			opts.CurrentWatchers.Stop()
		}
		return setup
	}

	setup.Watchers = EnsureWatcherSet(
		opts.ParentCtx,
		opts.CurrentWatchers,
		opts.Wallet,
		opts.ChainWSURL,
		opts.PendingWSURL,
		opts.PolygonClient,
		opts.RestClient,
		setup.TrackedMarkets,
		opts.MinedWatcherMode,
		logf,
	)
	if setup.Watchers != nil {
		setup.Watchers.Attach(setup.Poller)
	}

	return setup
}
