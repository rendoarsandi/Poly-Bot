package copytradeutil

import (
	"context"
	"strings"

	"Market-bot/internal/api"
)

type WatcherSet struct {
	Wallet         string
	ChainWSURL     string
	PendingWSURL   string
	cancel         context.CancelFunc
	PendingWatcher *api.PolymarketPendingWatcher
	MinedWatcher   *api.PolymarketMinedWatcher
}

func (w *WatcherSet) Stop() {
	if w == nil || w.cancel == nil {
		return
	}
	w.cancel()
	w.cancel = nil
}

func (w *WatcherSet) PrimeTrackedMarkets(markets []*api.Market) {
	if w == nil {
		return
	}
	if w.MinedWatcher != nil {
		w.MinedWatcher.PrimeTrackedMarkets(markets)
	}
	if w.PendingWatcher != nil {
		w.PendingWatcher.PrimeTrackedMarkets(markets)
	}
}

func (w *WatcherSet) Attach(poller *Poller) {
	if w == nil || poller == nil {
		return
	}
	poller.PendingWatcher = w.PendingWatcher
	poller.MinedWatcher = w.MinedWatcher
}

func EnsureWatcherSet(parentCtx context.Context, current *WatcherSet, wallet, chainWSURL, pendingWSURL string, polygonClient *api.PolygonClient, restClient *api.RestClient, trackedMarkets []*api.Market, minedWatcherMode string, logf func(string, ...interface{})) *WatcherSet {
	wallet = strings.TrimSpace(wallet)
	chainWSURL = strings.TrimSpace(chainWSURL)
	pendingWSURL = strings.TrimSpace(pendingWSURL)
	minedWatcherMode = api.NormalizeCopytradeMinedWatcherMode(minedWatcherMode)
	if wallet == "" {
		if current != nil {
			current.Stop()
		}
		return nil
	}

	if current != nil &&
		strings.EqualFold(current.Wallet, wallet) &&
		current.ChainWSURL == chainWSURL &&
		current.PendingWSURL == pendingWSURL {
		current.PrimeTrackedMarkets(trackedMarkets)
		return current
	}

	if current != nil {
		current.Stop()
	}

	watcherCtx, cancel := context.WithCancel(parentCtx)
	next := &WatcherSet{
		Wallet:       wallet,
		ChainWSURL:   chainWSURL,
		PendingWSURL: pendingWSURL,
		cancel:       cancel,
	}

	pendingSupported := api.SupportsPolymarketPendingWSURL(pendingWSURL)
	if api.ShouldEnableCopytradeMinedWatcher(minedWatcherMode, pendingWSURL) {
		if watcher := api.NewPolymarketMinedWatcher(chainWSURL, polygonClient, restClient, wallet); watcher != nil {
			watcher.PrimeTrackedMarkets(trackedMarkets)
			watcher.Start(watcherCtx, logf)
			next.MinedWatcher = watcher
			logf("⛓️ Copytrade onchain watcher enabled for %s", wallet)
		}
	} else {
		switch {
		case minedWatcherMode == api.CopytradeMinedWatcherModeOff:
			logf("ℹ️ Copytrade onchain watcher disabled by COPYTRADE_MINED_WATCHER_MODE=off")
		case pendingSupported:
			logf("ℹ️ Copytrade onchain watcher skipped: pending watcher available, reducing Polygon RPC usage")
		default:
			logf("ℹ️ Copytrade onchain watcher skipped")
		}
	}
	if pendingSupported {
		if watcher := api.NewPolymarketPendingWatcher(pendingWSURL, restClient, polygonClient, wallet); watcher != nil {
			watcher.PrimeTrackedMarkets(trackedMarkets)
			watcher.Start(watcherCtx, logf)
			next.PendingWatcher = watcher
			logf("🛰️ Copytrade mempool watcher enabled for %s", wallet)
		}
	} else if pendingWSURL != "" {
		logf("ℹ️ Copytrade mempool watcher skipped: pending filtering requires Alchemy; using standard Polygon WS for onchain watcher only")
	}

	if next.PendingWatcher == nil && next.MinedWatcher == nil {
		next.Stop()
		return nil
	}
	return next
}
