package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
)

type realbotCopytradeTarget struct {
	Raw    string
	Wallet string
	Label  string
}

type realbotCopytradeState struct {
	startedAt            time.Time
	lastError            string
	managed              map[string]bool
	targetShares         map[string]float64
	targetSeen           map[string]bool
	lastTargetPoll       map[string]time.Time
	pendingSellTarget    map[string]float64
	pendingSellPoll      map[string]time.Time
	lastTradeFetch       time.Time
	tradesSeeded         bool
	seenTradeKeys        map[string]time.Time
	seenTradeKeysCount   map[string]int
	retryTrades          []api.PublicTrade
	observedBuySizeSum   map[string]float64
	observedBuySizeCount map[string]int
	lastLogAt            map[string]time.Time
	lastLogMsg           map[string]string
}

type realbotCopytradePoller struct {
	wallet                 string
	conditionIDs           []string
	mu                     sync.Mutex
	lastPoll               time.Time
	lastPositionsRefreshAt time.Time
	fetching               bool
	waitCh                 chan struct{}
	lastPollStartedAt      time.Time
	lastSnapshot           api.PublicActivitySnapshot
	rateLimitUntil         time.Time
	rateLimitStreak        int
	pendingWatcher         *api.PolymarketPendingWatcher
	minedWatcher           *api.PolymarketMinedWatcher
}

type realbotCopytradeWatcherSet struct {
	wallet         string
	chainWSURL     string
	pendingWSURL   string
	cancel         context.CancelFunc
	pendingWatcher *api.PolymarketPendingWatcher
	minedWatcher   *api.PolymarketMinedWatcher
}

type realbotCopytradeMarketSnapshot struct {
	Trades            []api.PublicTrade
	Positions         []api.Position
	TradesErr         error
	PositionsErr      error
	PollStartedAt     time.Time
	PolledAt          time.Time
	PositionsPolledAt time.Time
}

func realbotCopytradeTradeFetchTimeout(pollEvery time.Duration) time.Duration {
	if pollEvery < 250*time.Millisecond {
		pollEvery = 250 * time.Millisecond
	}
	timeout := pollEvery * 4
	if timeout < 1500*time.Millisecond {
		timeout = 1500 * time.Millisecond
	}
	if timeout > 2500*time.Millisecond {
		timeout = 2500 * time.Millisecond
	}
	return timeout
}

func realbotCopytradePositionFetchTimeout(pollEvery time.Duration) time.Duration {
	timeout := realbotCopytradeTradeFetchTimeout(pollEvery) * 2
	if timeout < 4*time.Second {
		timeout = 4 * time.Second
	}
	if timeout > 8*time.Second {
		timeout = 8 * time.Second
	}
	return timeout
}

func realbotCopytradeCanReusePositions(lastRefresh time.Time, pollEvery time.Duration) bool {
	if lastRefresh.IsZero() {
		return false
	}
	maxAge := pollEvery * 3
	if maxAge < 5*time.Second {
		maxAge = 5 * time.Second
	}
	if maxAge > 15*time.Second {
		maxAge = 15 * time.Second
	}
	return time.Since(lastRefresh) <= maxAge
}

func newRealbotCopytradeState() *realbotCopytradeState {
	return &realbotCopytradeState{
		startedAt:            time.Now(),
		managed:              make(map[string]bool),
		targetShares:         make(map[string]float64),
		targetSeen:           make(map[string]bool),
		lastTargetPoll:       make(map[string]time.Time),
		pendingSellTarget:    make(map[string]float64),
		pendingSellPoll:      make(map[string]time.Time),
		seenTradeKeys:        make(map[string]time.Time),
		seenTradeKeysCount:   make(map[string]int),
		observedBuySizeSum:   make(map[string]float64),
		observedBuySizeCount: make(map[string]int),
		lastLogAt:            make(map[string]time.Time),
		lastLogMsg:           make(map[string]string),
	}
}

func newRealbotCopytradePoller(wallet string, conditionIDs []string) *realbotCopytradePoller {
	wallet = strings.TrimSpace(wallet)
	if wallet == "" {
		return nil
	}
	return &realbotCopytradePoller{
		wallet:       wallet,
		conditionIDs: normalizeRealbotCopytradeConditionIDs(conditionIDs),
	}
}

func (w *realbotCopytradeWatcherSet) stop() {
	if w == nil || w.cancel == nil {
		return
	}
	w.cancel()
	w.cancel = nil
}

func (w *realbotCopytradeWatcherSet) primeTrackedMarkets(markets []*api.Market) {
	if w == nil {
		return
	}
	if w.minedWatcher != nil {
		w.minedWatcher.PrimeTrackedMarkets(markets)
	}
	if w.pendingWatcher != nil {
		w.pendingWatcher.PrimeTrackedMarkets(markets)
	}
}

func (w *realbotCopytradeWatcherSet) attach(poller *realbotCopytradePoller) {
	if w == nil || poller == nil {
		return
	}
	poller.pendingWatcher = w.pendingWatcher
	poller.minedWatcher = w.minedWatcher
}

func ensureRealbotCopytradeWatcherSet(parentCtx context.Context, current *realbotCopytradeWatcherSet, wallet string, watcherMode string, chainWSURL, pendingWSURL string, polygonClient *api.PolygonClient, restClient *api.RestClient, trackedMarkets []*api.Market, logf func(string, ...interface{})) *realbotCopytradeWatcherSet {
	wallet = strings.TrimSpace(wallet)
	chainWSURL = strings.TrimSpace(chainWSURL)
	pendingWSURL = strings.TrimSpace(pendingWSURL)
	watcherMode = core.NormalizeCopytradeWatcherMode(watcherMode)
	useOnchain := watcherMode == "onchain"

	if wallet == "" {
		if current != nil {
			current.stop()
		}
		return nil
	}

	if current != nil &&
		strings.EqualFold(current.wallet, wallet) &&
		current.chainWSURL == chainWSURL &&
		current.pendingWSURL == pendingWSURL {
		current.primeTrackedMarkets(trackedMarkets)
		return current
	}

	if current != nil {
		current.stop()
	}

	watcherCtx, cancel := context.WithCancel(parentCtx)
	next := &realbotCopytradeWatcherSet{
		wallet:       wallet,
		chainWSURL:   chainWSURL,
		pendingWSURL: pendingWSURL,
		cancel:       cancel,
	}

	if useOnchain {
		if watcher := api.NewPolymarketMinedWatcher(chainWSURL, polygonClient, restClient, wallet); watcher != nil {
			watcher.PrimeTrackedMarkets(trackedMarkets)
			watcher.Start(watcherCtx, logf)
			next.minedWatcher = watcher
			logf("⛓️ Copytrade onchain watcher enabled for %s", wallet)
		}
	} else {
		logf("ℹ️ Copytrade onchain watcher disabled by copytradeWatcherMode setting")
	}

	if next.minedWatcher == nil && watcherMode != "public-api" {
		next.stop()
		return nil
	}
	return next
}

func realbotCopytradeShouldLog(state *realbotCopytradeState, key, msg string, interval time.Duration) bool {
	if state == nil {
		return true
	}
	if interval <= 0 {
		interval = 10 * time.Second
	}
	lastMsg := state.lastLogMsg[key]
	lastAt := state.lastLogAt[key]
	if msg == lastMsg && !lastAt.IsZero() && time.Since(lastAt) < interval {
		return false
	}
	state.lastLogMsg[key] = msg
	state.lastLogAt[key] = time.Now()
	return true
}

func realbotResolveCopytradeTarget(ctx context.Context, restClient *api.RestClient, liveCfg paper.TUISettings) (realbotCopytradeTarget, error) {
	raw := strings.TrimSpace(liveCfg.CopytradeTarget)
	if raw == "" {
		return realbotCopytradeTarget{}, fmt.Errorf("copytrade target is empty")
	}
	wallet, profile, err := restClient.ResolvePublicProfileTarget(ctx, raw)
	if err != nil {
		return realbotCopytradeTarget{}, err
	}

	label := wallet
	if profile != nil {
		switch {
		case strings.TrimSpace(profile.Name) != "":
			label = profile.Name
		case strings.TrimSpace(profile.Pseudonym) != "":
			label = profile.Pseudonym
		case strings.TrimSpace(profile.Referral) != "":
			label = "@" + strings.TrimPrefix(profile.Referral, "@")
		}
	}
	return realbotCopytradeTarget{
		Raw:    raw,
		Wallet: wallet,
		Label:  label,
	}, nil
}

func realbotCopytradeLabelFromHint(slug, title string) string {
	if slug = core.SanitizeString(slug); slug != "" {
		parts := strings.Split(slug, "-")
		if len(parts) > 0 && strings.TrimSpace(parts[0]) != "" {
			return strings.ToUpper(strings.TrimSpace(parts[0]))
		}
		return strings.ToUpper(slug)
	}
	title = core.SanitizeString(title)
	if title == "" {
		return "COPY"
	}
	title = strings.ToUpper(title)
	if len(title) > 12 {
		title = title[:12]
	}
	return title
}

func parseCopytradeEndTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return parsed
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed
	}
	return time.Time{}
}

func realbotCopytradeMarketAllowed(slug string, cfg paper.TUISettings) bool {
	lSlug := strings.ToLower(slug)
	marketSlug := strings.TrimSpace(cfg.MarketSlug)
	if marketSlug != "" && !strings.EqualFold(marketSlug, "ALL") {
		allowed := false
		for _, s := range strings.Split(marketSlug, ",") {
			s = strings.TrimSpace(strings.ToLower(s))
			if s != "" && strings.Contains(lSlug, s) {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}

	timeframe := strings.TrimSpace(cfg.Timeframe)
	if timeframe != "" && !strings.EqualFold(timeframe, "ALL") {
		allowed := false
		for _, f := range strings.Split(timeframe, ",") {
			f = strings.TrimSpace(strings.ToLower(f))
			if f != "" && (strings.Contains(lSlug, "-"+f+"-") || strings.HasSuffix(lSlug, "-"+f)) {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	return true
}

func realbotCopytradeMarketSelectable(now, endTime time.Time) bool {
	if endTime.IsZero() {
		return true
	}
	return !now.After(endTime)
}

func buildCopytradeMarketFromPosition(pos api.Position) *api.Market {
	if pos.ConditionID == "" || pos.TokenID == "" || pos.Outcome == "" {
		return nil
	}
	market := &api.Market{
		ConditionID: pos.ConditionID,
		Slug:        core.SanitizeString(pos.Slug),
		EndTime:     parseCopytradeEndTime(pos.EndDate),
		Tokens: []api.Token{
			{TokenID: pos.TokenID, Outcome: core.SanitizeString(pos.Outcome)},
		},
	}
	if pos.OppositeAsset != "" && pos.OppositeOutcome != "" {
		market.Tokens = append(market.Tokens, api.Token{
			TokenID: pos.OppositeAsset,
			Outcome: core.SanitizeString(pos.OppositeOutcome),
		})
	}
	return market
}

func buildCopytradeMarketFromTrade(ctx context.Context, restClient *api.RestClient, trade api.PublicTrade) *api.Market {
	if restClient == nil || trade.ConditionID == "" {
		return nil
	}
	market, err := restClient.GetMarket(ctx, trade.ConditionID)
	if err == nil && market != nil {
		return market
	}
	return nil
}

func realbotFindCopytradeMarkets(ctx context.Context, restClient *api.RestClient, wallet string, maxMarkets int, liveCfg paper.TUISettings) (map[string]*api.Market, error) {
	if restClient == nil {
		return nil, fmt.Errorf("rest client is nil")
	}
	if maxMarkets <= 0 {
		maxMarkets = 4
	}

	found := make(map[string]*api.Market)
	seen := make(map[string]struct{})

	addMarket := func(label string, market *api.Market) bool {
		if market == nil || market.ConditionID == "" {
			return false
		}
		if _, ok := seen[market.ConditionID]; ok {
			return false
		}
		if !realbotCopytradeMarketSelectable(time.Now(), market.EndTime) {
			return false
		}
		if !realbotCopytradeMarketAllowed(market.Slug, liveCfg) {
			return false
		}
		if label == "" {
			label = realbotCopytradeLabelFromHint(market.Slug, "")
		}
		if _, exists := found[label]; exists {
			fingerprint := strings.TrimPrefix(strings.TrimPrefix(market.ConditionID, "0x"), "0X")
			if len(fingerprint) > 6 {
				fingerprint = fingerprint[:6]
			}
			if fingerprint == "" {
				fingerprint = "mkt"
			}
			label = label + "-" + strings.ToUpper(fingerprint)
		}
		seen[market.ConditionID] = struct{}{}
		found[label] = market
		return len(found) >= maxMarkets
	}

	positions, posErr := restClient.GetPublicPositions(ctx, wallet, nil, 0.01, maxMarkets*8)
	if posErr == nil {
		for _, pos := range positions {
			if pos.Size <= 0.01 {
				continue
			}
			if addMarket(realbotCopytradeLabelFromHint(pos.Slug, pos.Title), buildCopytradeMarketFromPosition(pos)) {
				return found, nil
			}
		}
	}

	trades, tradeErr := restClient.GetPublicTrades(ctx, wallet, nil, maxMarkets*8)
	if tradeErr == nil {
		sort.Slice(trades, func(i, j int) bool {
			return trades[i].Timestamp > trades[j].Timestamp
		})
		for _, trade := range trades {
			if addMarket(realbotCopytradeLabelFromHint(trade.Slug, trade.Title), buildCopytradeMarketFromTrade(ctx, restClient, trade)) {
				return found, nil
			}
		}
	}

	if len(found) == 0 {
		switch {
		case posErr != nil && tradeErr != nil:
			return nil, fmt.Errorf("positions: %v | trades: %v", posErr, tradeErr)
		case posErr != nil:
			return nil, posErr
		case tradeErr != nil:
			return nil, tradeErr
		}
	}
	return found, nil
}

func realbotCopytradeHeldOutcomes(positions []api.Position) map[string]api.Position {
	held := make(map[string]api.Position, len(positions))
	for _, pos := range positions {
		outcome := core.SanitizeString(pos.Outcome)
		if outcome == "" || pos.Size <= 0.01 {
			continue
		}
		held[outcome] = pos
	}
	return held
}

func realbotCopytradeTargetShares(positions []api.Position) map[string]float64 {
	return realbotCopytradeTargetSharesForCondition(positions, "")
}

func realbotCopytradeSharesByCondition(positions []api.Position) map[string]map[string]float64 {
	sharesByCondition := make(map[string]map[string]float64)
	for _, pos := range positions {
		conditionID := strings.TrimSpace(pos.ConditionID)
		outcome := core.SanitizeString(pos.Outcome)
		if conditionID == "" || outcome == "" || pos.Size <= 0.01 {
			continue
		}
		outcomeShares := sharesByCondition[conditionID]
		if outcomeShares == nil {
			outcomeShares = make(map[string]float64)
			sharesByCondition[conditionID] = outcomeShares
		}
		outcomeShares[outcome] += pos.Size
	}
	return sharesByCondition
}

func realbotCopytradeTargetSharesForCondition(positions []api.Position, conditionID string) map[string]float64 {
	shares := make(map[string]float64, len(positions))
	conditionID = strings.TrimSpace(conditionID)
	for _, pos := range positions {
		if conditionID != "" && !strings.EqualFold(strings.TrimSpace(pos.ConditionID), conditionID) {
			continue
		}
		outcome := core.SanitizeString(pos.Outcome)
		if outcome == "" || pos.Size <= 0.01 {
			continue
		}
		shares[outcome] += pos.Size
	}
	return shares
}

func realbotCopytradeHoldsBothOutcomes(targetShares map[string]float64) bool {
	held := 0
	for _, qty := range targetShares {
		if qty > 0.01 {
			held++
			if held >= 2 {
				return true
			}
		}
	}
	return false
}

func realbotCopytradeHasAmbiguousPositionExit(positions []api.Position, conditionID string) bool {
	conditionID = strings.TrimSpace(conditionID)
	for _, pos := range positions {
		if conditionID != "" && !strings.EqualFold(strings.TrimSpace(pos.ConditionID), conditionID) {
			continue
		}
		if pos.Size <= 0.01 {
			continue
		}
		if pos.Mergeable || pos.Redeemable {
			return true
		}
	}
	return false
}

func normalizeRealbotCopytradeConditionIDs(conditionIDs []string) []string {
	seen := make(map[string]struct{}, len(conditionIDs))
	normalized := make([]string, 0, len(conditionIDs))
	for _, conditionID := range conditionIDs {
		conditionID = strings.TrimSpace(conditionID)
		if conditionID == "" {
			continue
		}
		if _, exists := seen[conditionID]; exists {
			continue
		}
		seen[conditionID] = struct{}{}
		normalized = append(normalized, conditionID)
	}
	return normalized
}

func realbotCopytradeIsRateLimited(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "status 429") || strings.Contains(msg, "error code: 1015")
}

func realbotCopytradeRateLimitBackoff(streak int) time.Duration {
	if streak < 1 {
		return 0
	}
	backoff := time.Second
	for i := 1; i < streak; i++ {
		backoff *= 2
		if backoff >= 8*time.Second {
			return 8 * time.Second
		}
	}
	return backoff
}

func filterRealbotCopytradeTradesByCondition(trades []api.PublicTrade, conditionID string) []api.PublicTrade {
	if strings.TrimSpace(conditionID) == "" || len(trades) == 0 {
		return trades
	}
	filtered := make([]api.PublicTrade, 0, len(trades))
	for _, trade := range trades {
		if strings.EqualFold(strings.TrimSpace(trade.ConditionID), conditionID) {
			filtered = append(filtered, trade)
		}
	}
	return filtered
}

func filterRealbotCopytradePositionsByCondition(positions []api.Position, conditionID string) []api.Position {
	if strings.TrimSpace(conditionID) == "" || len(positions) == 0 {
		return positions
	}
	filtered := make([]api.Position, 0, len(positions))
	for _, pos := range positions {
		if strings.EqualFold(strings.TrimSpace(pos.ConditionID), conditionID) {
			filtered = append(filtered, pos)
		}
	}
	return filtered
}

func (p *realbotCopytradePoller) pendingSignalsForCondition(conditionID string, since time.Time) []api.PublicTrade {
	if p == nil || p.pendingWatcher == nil {
		return nil
	}
	signals := p.pendingWatcher.SignalsSince(conditionID, since)
	if len(signals) == 0 {
		return nil
	}
	trades := make([]api.PublicTrade, 0, len(signals))
	for _, sig := range signals {
		trades = append(trades, api.PublicTrade{
			ConditionID:     sig.ConditionID,
			Outcome:         sig.Outcome,
			Side:            sig.Side,
			Size:            sig.Size,
			Timestamp:       sig.ObservedAt.Unix(),
			ObservedAt:      sig.ObservedAt.Unix(),
			TransactionHash: sig.TxHash,
			Source:          "mempool",
			SignalID:        sig.SignalID,
			Slug:            sig.Slug,
		})
	}
	return trades
}

func (p *realbotCopytradePoller) minedSignalsForCondition(conditionID string, since time.Time) []api.PublicTrade {
	if p == nil || p.minedWatcher == nil {
		return nil
	}
	signals := p.minedWatcher.SignalsSince(conditionID, since)
	if len(signals) == 0 {
		return nil
	}
	trades := make([]api.PublicTrade, 0, len(signals))
	for _, sig := range signals {
		trades = append(trades, api.PublicTrade{
			ConditionID:     sig.ConditionID,
			Outcome:         sig.Outcome,
			Side:            sig.Side,
			Size:            sig.Size,
			Timestamp:       sig.BlockTimestamp,
			ObservedAt:      sig.ObservedAt.Unix(),
			TransactionHash: sig.TxHash,
			Source:          "onchain",
			SignalID:        sig.SignalID,
			Slug:            sig.Slug,
		})
	}
	return trades
}

func realbotCopytradeHasWatcher(p *realbotCopytradePoller) bool {
	return p != nil && ((p.pendingWatcher != nil && p.pendingWatcher.Enabled()) || (p.minedWatcher != nil && p.minedWatcher.Enabled()))
}

func realbotCopytradeHasOnchainWatcher(p *realbotCopytradePoller) bool {
	return p != nil && p.minedWatcher != nil && p.minedWatcher.Enabled()
}

func realbotCopytradeHasPendingWatcher(p *realbotCopytradePoller) bool {
	return p != nil && p.pendingWatcher != nil && p.pendingWatcher.Enabled()
}

func realbotCopytradeShouldUsePublicActivityAPI(p *realbotCopytradePoller, watcherMode string) bool {
	return core.NormalizeCopytradeWatcherMode(watcherMode) == "public-api" || !realbotCopytradeHasWatcher(p)
}

func (p *realbotCopytradePoller) cachedSnapshotForCondition(conditionID string) realbotCopytradeMarketSnapshot {
	if p == nil {
		return realbotCopytradeMarketSnapshot{}
	}
	return realbotCopytradeMarketSnapshot{
		Trades:            filterRealbotCopytradeTradesByCondition(p.lastSnapshot.Trades, conditionID),
		Positions:         filterRealbotCopytradePositionsByCondition(p.lastSnapshot.Positions, conditionID),
		TradesErr:         p.lastSnapshot.TradesErr,
		PositionsErr:      p.lastSnapshot.PositionsErr,
		PollStartedAt:     p.lastPollStartedAt,
		PolledAt:          p.lastPoll,
		PositionsPolledAt: p.lastPositionsRefreshAt,
	}
}

func (p *realbotCopytradePoller) snapshotForCondition(ctx context.Context, restClient *api.RestClient, pollEvery time.Duration, conditionID string) (realbotCopytradeMarketSnapshot, error) {
	if p == nil || restClient == nil {
		return realbotCopytradeMarketSnapshot{}, fmt.Errorf("copytrade poller unavailable")
	}
	if pollEvery <= 0 {
		pollEvery = 2 * time.Second
	}
	conditionID = strings.TrimSpace(conditionID)

	for {
		p.mu.Lock()
		if !p.lastPoll.IsZero() && time.Since(p.lastPoll) < pollEvery {
			snapshot := p.cachedSnapshotForCondition(conditionID)
			p.mu.Unlock()
			return snapshot, nil
		}
		if !p.rateLimitUntil.IsZero() && time.Now().Before(p.rateLimitUntil) && !p.lastPoll.IsZero() {
			snapshot := p.cachedSnapshotForCondition(conditionID)
			p.mu.Unlock()
			return snapshot, nil
		}
		if p.fetching {
			waitCh := p.waitCh
			p.mu.Unlock()
			select {
			case <-ctx.Done():
				return realbotCopytradeMarketSnapshot{}, ctx.Err()
			case <-waitCh:
				continue
			}
		}

		p.fetching = true
		p.waitCh = make(chan struct{})
		wallet := p.wallet
		conditionIDs := append([]string(nil), p.conditionIDs...)
		pollStartedAt := time.Now()
		cachedPositions := append([]api.Position(nil), p.lastSnapshot.Positions...)
		cachedPositionsValid := p.lastSnapshot.PositionsErr == nil && realbotCopytradeCanReusePositions(p.lastPositionsRefreshAt, pollEvery)
		p.mu.Unlock()

		done := false
		defer func() {
			if !done {
				p.mu.Lock()
				waitCh := p.waitCh
				p.fetching = false
				p.waitCh = nil
				if waitCh != nil {
					close(waitCh)
				}
				p.mu.Unlock()
			}
		}()

		tradeLimit := len(conditionIDs) * 64
		if tradeLimit < 128 {
			tradeLimit = 128
		}
		if tradeLimit > 1000 {
			tradeLimit = 1000
		}
		positionLimit := len(conditionIDs) * 8
		if positionLimit < 16 {
			positionLimit = 16
		}
		if positionLimit > 500 {
			positionLimit = 500
		}
		tradeTimeout := realbotCopytradeTradeFetchTimeout(pollEvery)
		positionTimeout := realbotCopytradePositionFetchTimeout(pollEvery)
		snapshot := restClient.GetPublicActivitySnapshotWithFallback(
			ctx,
			wallet,
			conditionIDs,
			tradeLimit,
			0.01,
			positionLimit,
			cachedPositions,
			cachedPositionsValid,
			tradeTimeout,
			positionTimeout,
		)
		now := time.Now()

		p.mu.Lock()
		if snapshot.TradesErr == nil {
			p.lastSnapshot.Trades = snapshot.Trades
			p.lastSnapshot.TradesErr = nil
		} else {
			p.lastSnapshot.TradesErr = snapshot.TradesErr
		}
		if snapshot.PositionsErr == nil {
			p.lastSnapshot.Positions = snapshot.Positions
			p.lastSnapshot.PositionsErr = nil
			if !snapshot.PositionsCached {
				p.lastPositionsRefreshAt = now
			}
		} else {
			p.lastSnapshot.PositionsErr = snapshot.PositionsErr
		}
		if realbotCopytradeIsRateLimited(snapshot.TradesErr) {
			p.rateLimitStreak++
			p.rateLimitUntil = now.Add(realbotCopytradeRateLimitBackoff(p.rateLimitStreak))
		} else {
			p.rateLimitStreak = 0
			p.rateLimitUntil = time.Time{}
		}
		p.lastPollStartedAt = pollStartedAt
		p.lastPoll = now
		waitCh := p.waitCh
		p.fetching = false
		p.waitCh = nil
		filtered := p.cachedSnapshotForCondition(conditionID)
		done = true
		p.mu.Unlock()
		if waitCh != nil {
			close(waitCh)
		}

		return filtered, nil
	}
}

func realbotCopytradePositionSyncTrades(state *realbotCopytradeState, conditionID string, outcomes []string, positions []api.Position, pollTime time.Time, freshTrades []api.PublicTrade, sizingMode string) ([]api.PublicTrade, map[string]float64) {
	if state == nil || pollTime.IsZero() {
		return nil, nil
	}
	if !strings.EqualFold(sizingMode, core.CopytradeSizingModePercent) {
		return nil, nil
	}

	targetShares := realbotCopytradeTargetSharesForCondition(positions, conditionID)
	holdsBoth := realbotCopytradeHoldsBothOutcomes(targetShares)
	ambiguousExit := realbotCopytradeHasAmbiguousPositionExit(positions, conditionID)

	freshBuySize := make(map[string]float64)
	freshSell := make(map[string]bool)
	for _, trade := range freshTrades {
		outcome := core.SanitizeString(trade.Outcome)
		if outcome == "" {
			continue
		}
		switch strings.ToUpper(strings.TrimSpace(trade.Side)) {
		case "BUY":
			freshBuySize[outcome] += math.Max(0, trade.Size)
		case "SELL":
			freshSell[outcome] = true
		}
	}

	relevantOutcomes := make(map[string]struct{})
	for _, outcome := range outcomes {
		outcome = core.SanitizeString(outcome)
		if outcome != "" {
			relevantOutcomes[outcome] = struct{}{}
		}
	}
	for outcome := range targetShares {
		relevantOutcomes[outcome] = struct{}{}
	}
	for outcome := range state.targetSeen {
		if outcome != "" {
			relevantOutcomes[outcome] = struct{}{}
		}
	}
	if len(relevantOutcomes) == 0 {
		return nil, nil
	}

	targetDeltas := make(map[string]float64)
	syncTrades := make([]api.PublicTrade, 0)
	for outcome := range relevantOutcomes {
		targetQty := targetShares[outcome]
		delta, ready, pending := realbotCopytradeTargetDelta(state, outcome, targetQty, pollTime)
		if !ready || pending || math.Abs(delta) <= 0.01 {
			continue
		}
		targetDeltas[outcome] = delta
		switch {
		case delta > 0:
			if remaining := delta - freshBuySize[outcome]; remaining > 0.01 {
				syncTrades = append(syncTrades, realbotEstimatedPositionBuySignals(state, strings.TrimSpace(conditionID), outcome, remaining, sizingMode)...)
			}
		case delta < 0 && !freshSell[outcome] && !holdsBoth && !ambiguousExit:
			syncTrades = append(syncTrades, api.PublicTrade{
				ConditionID: strings.TrimSpace(conditionID),
				Outcome:     outcome,
				Side:        "SELL",
				Size:        -delta,
				Timestamp:   pollTime.Unix(),
				Source:      "position",
			})
		}
	}

	sort.Slice(syncTrades, func(i, j int) bool {
		if syncTrades[i].Outcome == syncTrades[j].Outcome {
			return syncTrades[i].Side < syncTrades[j].Side
		}
		return syncTrades[i].Outcome < syncTrades[j].Outcome
	})

	return syncTrades, targetDeltas
}

func realbotClearPendingCopytradeSell(state *realbotCopytradeState, outcome string) {
	if state == nil || outcome == "" {
		return
	}
	delete(state.pendingSellTarget, outcome)
	delete(state.pendingSellPoll, outcome)
}

func realbotCopytradeTargetDelta(state *realbotCopytradeState, outcome string, targetQty float64, pollTime time.Time) (float64, bool, bool) {
	if state == nil {
		return 0, false, false
	}
	outcome = core.SanitizeString(outcome)
	if outcome == "" {
		return 0, false, false
	}
	if !state.targetSeen[outcome] {
		state.targetSeen[outcome] = true
		state.targetShares[outcome] = targetQty
		state.lastTargetPoll[outcome] = pollTime
		realbotClearPendingCopytradeSell(state, outcome)
		if state.tradesSeeded {
			return targetQty, true, false
		}
		return 0, false, false
	}
	if lastPoll := state.lastTargetPoll[outcome]; !lastPoll.IsZero() && lastPoll.Equal(pollTime) {
		return 0, false, false
	}
	state.lastTargetPoll[outcome] = pollTime

	prev := state.targetShares[outcome]
	if targetQty > prev+0.01 {
		state.targetShares[outcome] = targetQty
		realbotClearPendingCopytradeSell(state, outcome)
		return targetQty - prev, true, false
	}
	if targetQty >= prev-0.01 {
		state.targetShares[outcome] = targetQty
		realbotClearPendingCopytradeSell(state, outcome)
		return 0, false, false
	}
	if _, waiting := state.pendingSellPoll[outcome]; waiting {
		state.targetShares[outcome] = targetQty
		realbotClearPendingCopytradeSell(state, outcome)
		return targetQty - prev, true, false
	}
	state.pendingSellTarget[outcome] = targetQty
	state.pendingSellPoll[outcome] = pollTime
	return 0, false, true
}

func realbotCopytradeTradeKey(trade api.PublicTrade) string {
	if signalID := strings.TrimSpace(trade.SignalID); signalID != "" {
		return "signal|" + signalID
	}
	txHash := strings.TrimSpace(trade.TransactionHash)
	if txHash != "" {
		return fmt.Sprintf("%s|%s|%s|%.6f|%.6f|%s|%d|%s", strings.TrimSpace(trade.ConditionID), core.SanitizeString(trade.Outcome), strings.ToUpper(strings.TrimSpace(trade.Side)), trade.Size, trade.Price, strings.TrimSpace(trade.Asset), trade.Timestamp, txHash)
	}
	return fmt.Sprintf("%s|%d|%s|%s|%.6f", strings.TrimSpace(trade.ConditionID), trade.Timestamp, core.SanitizeString(trade.Outcome), strings.ToUpper(strings.TrimSpace(trade.Side)), trade.Size)
}

func realbotCopytradeEffectiveTimestamp(trade api.PublicTrade) int64 {
	if trade.ObservedAt > 0 {
		return trade.ObservedAt
	}
	return trade.Timestamp
}

func realbotCopytradeSignalSource(trade api.PublicTrade) string {
	label := strings.TrimSpace(trade.Source)
	if label != "" {
		return label
	}
	if trade.Timestamp == 0 {
		return "position"
	}
	return "trade"
}

func realbotCopytradeSignalSourceLabel(trade api.PublicTrade) string {
	switch strings.ToLower(strings.TrimSpace(realbotCopytradeSignalSource(trade))) {
	case "mempool":
		return "MEMPOOL"
	case "onchain":
		return "ONCHAIN"
	case "position", "position-estimate":
		return "POSITION"
	case "public":
		return "PUBLIC"
	default:
		return strings.ToUpper(strings.TrimSpace(realbotCopytradeSignalSource(trade)))
	}
}

func realbotCopytradeSignalSummary(trade api.PublicTrade) string {
	side := strings.ToUpper(strings.TrimSpace(trade.Side))
	if side == "" {
		side = "?"
	}
	outcome := core.SanitizeString(trade.Outcome)
	if outcome == "" {
		outcome = "?"
	}
	parts := []string{
		fmt.Sprintf("%s %s", side, outcome),
		fmt.Sprintf("master=%s", formatShareQty(math.Max(0, trade.Size))),
		fmt.Sprintf("source=%s", realbotCopytradeSignalSourceLabel(trade)),
	}
	if txHash := realbotShortTxHash(trade.TransactionHash); txHash != "" {
		parts = append(parts, "tx="+txHash)
	}
	return strings.Join(parts, " | ")
}

func realbotLogCopytradeSignalResult(tui *paper.TUI, marketID string, trade api.PublicTrade, status, result string) {
	if tui == nil {
		return
	}
	tui.LogEvent("[%s] %s [%s] Copytrade signal %s -> %s", marketID, status, realbotCopytradeSignalSourceLabel(trade), realbotCopytradeSignalSummary(trade), result)
}

func realbotNormalizeCopytradeSignalID(trade api.PublicTrade) string {
	if signalID := strings.TrimSpace(trade.SignalID); signalID != "" {
		return signalID
	}
	txHash := strings.TrimSpace(trade.TransactionHash)
	asset := strings.TrimSpace(trade.Asset)
	side := strings.ToUpper(strings.TrimSpace(trade.Side))
	if txHash == "" || asset == "" || side == "" {
		return ""
	}
	return fmt.Sprintf("%s:%s:%s", txHash, asset, side)
}

func realbotPrepareCopytradeTrades(trades []api.PublicTrade, source string, liveCfg paper.TUISettings) []api.PublicTrade {
	if len(trades) == 0 {
		return nil
	}
	prepared := make([]api.PublicTrade, 0, len(trades))
	for _, trade := range trades {
		normalized := trade
		if strings.TrimSpace(normalized.Source) == "" && strings.TrimSpace(source) != "" {
			normalized.Source = source
		}
		normalized.SignalID = realbotNormalizeCopytradeSignalID(normalized)
		prepared = append(prepared, normalized)
	}
	return prepared
}

func realbotMergeCopytradeTrades(groups ...[]api.PublicTrade) []api.PublicTrade {
	total := 0
	for _, group := range groups {
		total += len(group)
	}
	if total == 0 {
		return nil
	}

	merged := make([]api.PublicTrade, 0, total)
	seenSignals := make(map[string]string, total)
	for _, group := range groups {
		for _, trade := range group {
			key := realbotNormalizeCopytradeSignalID(trade)
			if key != "" {
				source := strings.TrimSpace(trade.Source)
				if seenSource, exists := seenSignals[key]; exists {
					if seenSource != source {
						continue
					}
				} else {
					seenSignals[key] = source
				}
			}
			merged = append(merged, trade)
		}
	}
	return merged
}

func realbotObserveCopytradeBuySignal(state *realbotCopytradeState, trade api.PublicTrade) {
	if state == nil {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(trade.Side), "BUY") {
		return
	}
	if trade.Size <= 0.01 {
		return
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(trade.Source)), "position") {
		return
	}
	outcome := core.SanitizeString(trade.Outcome)
	if outcome == "" {
		return
	}
	state.observedBuySizeSum[outcome] += trade.Size
	state.observedBuySizeCount[outcome]++
}

func realbotEstimatedPositionBuySignals(state *realbotCopytradeState, conditionID, outcome string, delta float64, mode string) []api.PublicTrade {
	outcome = core.SanitizeString(outcome)
	if outcome == "" || delta <= 0.01 {
		return nil
	}
	if strings.EqualFold(mode, core.CopytradeSizingModePercent) {
		return []api.PublicTrade{{
			ConditionID: strings.TrimSpace(conditionID),
			Outcome:     outcome,
			Side:        "BUY",
			Size:        delta,
			Source:      "position",
		}}
	}

	estimatedTrades := 1
	if state != nil {
		if count := state.observedBuySizeCount[outcome]; count > 0 {
			avg := state.observedBuySizeSum[outcome] / float64(count)
			if avg > 0.01 {
				estimatedTrades = int(math.Ceil(delta / avg))
			}
		}
	}
	if estimatedTrades < 1 {
		estimatedTrades = 1
	}
	if estimatedTrades > 16 {
		estimatedTrades = 16
	}

	signals := make([]api.PublicTrade, 0, estimatedTrades)
	remaining := delta
	for i := 0; i < estimatedTrades; i++ {
		chunk := remaining / float64(estimatedTrades-i)
		if chunk <= 0.01 {
			continue
		}
		signals = append(signals, api.PublicTrade{
			ConditionID: strings.TrimSpace(conditionID),
			Outcome:     outcome,
			Side:        "BUY",
			Size:        chunk,
			Source:      "position-estimate",
		})
		remaining -= chunk
	}
	if len(signals) == 0 {
		return nil
	}
	return signals
}

func realbotCopytradeBootstrapStartTimestamp(startedAt time.Time) int64 {
	if startedAt.IsZero() {
		return 0
	}
	startTs := startedAt.Unix()
	if startedAt.Nanosecond() != 0 {
		startTs--
	}
	return startTs
}

func realbotCopytradeBootstrapAcceptsTrade(state *realbotCopytradeState, trade api.PublicTrade) bool {
	if state == nil || state.startedAt.IsZero() {
		return false
	}
	startTs := realbotCopytradeBootstrapStartTimestamp(state.startedAt)
	effectiveTS := realbotCopytradeEffectiveTimestamp(trade)
	if effectiveTS >= startTs {
		return true
	}

	source := strings.ToLower(strings.TrimSpace(realbotCopytradeSignalSource(trade)))
	if source != "onchain" && source != "mempool" {
		return false
	}

	tradeAt := time.Unix(effectiveTS, 0)
	return !tradeAt.Before(state.startedAt.Add(-realbotCopytradeRetryMaxAge))
}

func realbotCopytradeRetrySignalFresh(now time.Time, trade api.PublicTrade) bool {
	effectiveTS := realbotCopytradeEffectiveTimestamp(trade)
	if effectiveTS <= 0 {
		return true
	}
	tradeAt := time.Unix(effectiveTS, 0)
	if now.Before(tradeAt) {
		return true
	}
	return now.Sub(tradeAt) <= realbotCopytradeRetryMaxAge
}

func realbotCopytradeTakeRetryTrades(state *realbotCopytradeState, now time.Time) []api.PublicTrade {
	if state == nil || len(state.retryTrades) == 0 {
		return nil
	}
	retries := state.retryTrades
	state.retryTrades = nil
	if now.IsZero() {
		now = time.Now()
	}
	fresh := make([]api.PublicTrade, 0, len(retries))
	for _, trade := range retries {
		if realbotCopytradeRetrySignalFresh(now, trade) {
			fresh = append(fresh, trade)
		}
	}
	return fresh
}

func realbotCopytradeQueueRetryTrades(state *realbotCopytradeState, retries []api.PublicTrade) {
	if state == nil || len(retries) == 0 {
		return
	}
	if len(retries) > realbotCopytradeRetryQueueCap {
		retries = retries[len(retries)-realbotCopytradeRetryQueueCap:]
	}
	state.retryTrades = append(state.retryTrades, retries...)
	if len(state.retryTrades) > realbotCopytradeRetryQueueCap {
		state.retryTrades = append([]api.PublicTrade(nil), state.retryTrades[len(state.retryTrades)-realbotCopytradeRetryQueueCap:]...)
	}
}

func realbotCopytradeFreshTrades(state *realbotCopytradeState, trades []api.PublicTrade, conditionID string, sizingMode string) []api.PublicTrade {
	if state == nil {
		return nil
	}
	conditionID = strings.TrimSpace(conditionID)
	now := time.Now()
	for key, seenAt := range state.seenTradeKeys {
		if now.Sub(seenAt) > 15*time.Minute {
			delete(state.seenTradeKeys, key)
			if idx := strings.LastIndex(key, "#"); idx != -1 {
				baseKey := key[:idx]
				if count, exists := state.seenTradeKeysCount[baseKey]; exists {
					if count <= 1 {
						delete(state.seenTradeKeysCount, baseKey)
					} else {
						state.seenTradeKeysCount[baseKey]--
					}
				}
			}
		}
	}
	if len(trades) == 0 {
		return nil
	}

	filtered := make([]api.PublicTrade, 0, len(trades))
	for _, trade := range trades {
		if conditionID != "" && !strings.EqualFold(strings.TrimSpace(trade.ConditionID), conditionID) {
			continue
		}
		if core.SanitizeString(trade.Outcome) == "" {
			continue
		}
		filtered = append(filtered, trade)
	}
	sort.Slice(filtered, func(i, j int) bool {
		leftTS := realbotCopytradeEffectiveTimestamp(filtered[i])
		rightTS := realbotCopytradeEffectiveTimestamp(filtered[j])
		if leftTS == rightTS {
			return realbotCopytradeTradeKey(filtered[i]) < realbotCopytradeTradeKey(filtered[j])
		}
		return leftTS < rightTS
	})

	fresh := make([]api.PublicTrade, 0, len(filtered))
	currentPollCounts := make(map[string]int)
	for _, trade := range filtered {
		baseKey := realbotCopytradeTradeKey(trade)
		currentPollCounts[baseKey]++

		totalSeen := state.seenTradeKeysCount[baseKey]
		if currentPollCounts[baseKey] > totalSeen {
			state.seenTradeKeysCount[baseKey] = currentPollCounts[baseKey]
			state.seenTradeKeys[fmt.Sprintf("%s#%d", baseKey, currentPollCounts[baseKey])] = now

			if trade.Size <= 0.01 && !strings.EqualFold(sizingMode, core.CopytradeSizingModeShares) && !strings.EqualFold(sizingMode, core.CopytradeSizingModeUSDC) {
				continue
			}

			effectiveTS := realbotCopytradeEffectiveTimestamp(trade)
			if effectiveTS > 0 && !state.startedAt.IsZero() {
				tradeAt := time.Unix(effectiveTS, 0)
				startTs := realbotCopytradeBootstrapStartTimestamp(state.startedAt)
				startAt := time.Unix(startTs, 0)
				if !now.Before(tradeAt) && tradeAt.Before(startAt.Add(-realbotCopytradeRetryMaxAge)) {
					continue
				}
			}

			fresh = append(fresh, trade)
		}
	}
	if !state.tradesSeeded {
		state.tradesSeeded = true
		if state.startedAt.IsZero() {
			return nil
		}
		bootstrap := make([]api.PublicTrade, 0, len(fresh))
		for _, trade := range fresh {
			if !realbotCopytradeBootstrapAcceptsTrade(state, trade) {
				continue
			}
			bootstrap = append(bootstrap, trade)
		}
		sort.Slice(bootstrap, func(i, j int) bool {
			leftTS := realbotCopytradeEffectiveTimestamp(bootstrap[i])
			rightTS := realbotCopytradeEffectiveTimestamp(bootstrap[j])
			if leftTS == rightTS {
				return realbotCopytradeTradeKey(bootstrap[i]) < realbotCopytradeTradeKey(bootstrap[j])
			}
			return leftTS < rightTS
		})
		return bootstrap
	}
	return fresh
}
