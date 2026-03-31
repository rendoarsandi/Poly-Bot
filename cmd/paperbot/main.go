package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	mkt "Market-bot/internal/markets"
	"Market-bot/internal/paper"
	"Market-bot/internal/setup"
	"Market-bot/internal/strategy"
)

const (
	StartingBalance        = 100.0 // $100 paper trading balance
	UseLiveUI              = true  // Set to false for traditional logging
	paperArbModeTaker      = "taker"
	paperArbModeBinanceGap = "binance-gap"
	paperArbModeCopytrade  = "copytrade"
	paperArbModeMaker      = "maker"
	terminalBidFloor       = 0.985
	terminalAskCeil        = 0.015
	// Price threshold used for post-expiry winner fallback when authoritative
	// resolution is still unavailable.
	terminalWinnerFloor = 0.99

	// Split strategy constants
	MinSplitBuffer   = 50.0  // Minimum initial split buffer ($)
	MinSplitAmount   = 10.0  // Minimum split amount to execute ($)
	MaxSharesPerSell = 250.0 // Hard safety cap on shares per sell

	paperMakerQuoteStep            = 0.001
	paperMakerBaseOffset           = 0.008
	paperMakerInventorySkewStep    = 0.020
	paperMakerInventoryTargetMult  = 2.5
	paperMakerInventoryCapMult     = 5.0
	paperMakerQuoteSizeSkewFactor  = 0.75
	paperMakerRequoteInterval      = 500 * time.Millisecond
	paperMakerMinQuoteValue        = 1.0
	paperMakerCashUsagePerOutcome  = 0.35
	paperExecutionGuardQuoteMaxAge = 1500 * time.Millisecond
	paperUIRefreshInterval         = 100 * time.Millisecond
	paperMainLoopInterval          = 10 * time.Millisecond
	paperCopytradeLoopIntervalMin  = 100 * time.Millisecond
	paperCopytradeLoopIntervalMax  = 250 * time.Millisecond
	paperCopytradeUIRefreshMin     = 250 * time.Millisecond
	paperCopytradeUIRefreshMax     = 500 * time.Millisecond
	paperCopytradeRetryQueueCap    = 256
	paperCopytradeRetryMaxAge      = 20 * time.Second
	paperFillPollInterval          = 50 * time.Millisecond
	paperResolutionRefreshInterval = 2 * time.Second
	paperHistoricalLookupInterval  = 2 * time.Second
	paperHistoricalNotFoundRetry   = 30 * time.Second
	paperResolutionErrorLogGap     = 5 * time.Second
	paperMaxSaneOutcomeSpread      = 0.10
	paperMaxSaneAskPairSum         = 1.10
	paperMinSaneBidPairSum         = 0.90
	paperbotMinActionShares        = 0.01
)

var paperMakerStrategyParams = strategy.MakerParams{
	QuoteStep:           paperMakerQuoteStep,
	DefaultQuoteGap:     paperMakerBaseOffset,
	InventorySkewStep:   paperMakerInventorySkewStep,
	QuoteSizeSkewFactor: paperMakerQuoteSizeSkewFactor,
	CashUsagePerOutcome: paperMakerCashUsagePerOutcome,
	MinQuoteValue:       paperMakerMinQuoteValue,
}

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
	// Last time both sides had a valid, non-crossed local quote at once.
	LastPairUpdate time.Time

	// Last time we performed a REST fallback poll
	LastRestPoll time.Time

	// Split strategy simulation
	SplitInventory     *paper.SplitInventory
	ReplenishCtrl      *paper.ReplenishController
	SplitInitialized   bool
	InitialSplitAmount float64 // Track initial split for replenishment target
	LastSplitSell      time.Time
	MakerQuotes        map[string]*paper.LimitOrder
	LastMakerSync      time.Time

	// On-chain resolution cache (shared across all traders)
	ResolutionCache *api.ResolutionCache

	BinanceFeed        *api.BinanceFuturesPriceFeed
	PolySignalTracker  *paper.DirectionalSignalTracker
	LastBinanceLog     *time.Time
	LastBinanceTrigger time.Time
	CopytradeWallet    string
	CopytradeLabel     string
	CopytradePoller    *paperbotCopytradePoller
	CopytradeState     *paperbotCopytradeState

	// State
	LaddersPlaced bool
	MarketEnded   bool
	// Guard repeated REST recovery logs until fallback pressure clears.
	RestFallbackActive bool
	RestRecoveryLogged bool
	mu                 sync.Mutex

	nextResolutionRefreshAt        time.Time
	lastResolutionErrorLogAt       time.Time
	lastResolutionError            string
	nextHistoricalLookupAt         time.Time
	lastHistoricalLookupErrorLogAt time.Time
	lastHistoricalLookupError      string
	lastCopytradeNoticeAt          time.Time
}

type marketResult struct {
	realizedPnL float64
	trades      int
}

type paperQuoteState struct {
	UpdatedAt time.Time
	Source    string
}

type paperExecutionLatency struct {
	detectedAt  time.Time
	startedAt   time.Time
	executedAt  time.Time
	settledAt   time.Time
	opportunity string
	marketID    string
	shares      float64
	marginPct   float64
	expectedPnL float64
}

type paperCopytradeLatency struct {
	pollStartedAt   time.Time
	apiReceivedAt   time.Time
	quoteReadyAt    time.Time
	executedAt      time.Time
	signalTimestamp int64
	marketID        string
	outcome         string
	side            string
	source          string
	txHash          string
}

type paperbotCopytradeTarget struct {
	Raw    string
	Wallet string
	Label  string
}

type paperbotCopytradeState struct {
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

type paperbotCopytradePoller struct {
	wallet            string
	conditionIDs      []string
	mu                sync.Mutex
	lastPoll          time.Time
	fetching          bool
	waitCh            chan struct{}
	lastPollStartedAt time.Time
	lastSnapshot      api.PublicActivitySnapshot
	rateLimitUntil    time.Time
	rateLimitStreak   int
	pendingWatcher    *api.PolymarketPendingWatcher
	minedWatcher      *api.PolymarketMinedWatcher
}

func paperbotCopytradeTradeFetchTimeout(pollEvery time.Duration) time.Duration {
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

func paperbotCopytradePositionFetchTimeout(pollEvery time.Duration, hasCachedPositions bool) time.Duration {
	if hasCachedPositions {
		timeout := pollEvery * 2
		if timeout < 350*time.Millisecond {
			timeout = 350 * time.Millisecond
		}
		if timeout > 900*time.Millisecond {
			timeout = 900 * time.Millisecond
		}
		return timeout
	}
	timeout := pollEvery * 4
	if timeout < 1200*time.Millisecond {
		timeout = 1200 * time.Millisecond
	}
	if timeout > 2500*time.Millisecond {
		timeout = 2500 * time.Millisecond
	}
	return timeout
}

func newPaperbotCopytradeState() *paperbotCopytradeState {
	return &paperbotCopytradeState{
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

func newPaperbotCopytradePoller(wallet string, conditionIDs []string) *paperbotCopytradePoller {
	wallet = strings.TrimSpace(wallet)
	if wallet == "" {
		return nil
	}
	return &paperbotCopytradePoller{
		wallet:       wallet,
		conditionIDs: normalizeCopytradeConditionIDs(conditionIDs),
	}
}

type paperbotCopytradeMarketSnapshot struct {
	Trades        []api.PublicTrade
	Positions     []api.Position
	TradesErr     error
	PositionsErr  error
	PollStartedAt time.Time
	PolledAt      time.Time
}

func paperbotCopytradeShouldLog(state *paperbotCopytradeState, key, msg string, interval time.Duration) bool {
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

func (l paperExecutionLatency) detectToStart() time.Duration {
	if l.detectedAt.IsZero() || l.startedAt.IsZero() {
		return 0
	}
	return l.startedAt.Sub(l.detectedAt)
}

func (l paperExecutionLatency) startToExecution() time.Duration {
	if l.startedAt.IsZero() || l.executedAt.IsZero() {
		return 0
	}
	return l.executedAt.Sub(l.startedAt)
}

func (l paperExecutionLatency) detectToExecution() time.Duration {
	if l.detectedAt.IsZero() || l.executedAt.IsZero() {
		return 0
	}
	return l.executedAt.Sub(l.detectedAt)
}

func (l paperExecutionLatency) detectToSettlement() time.Duration {
	if l.detectedAt.IsZero() || l.settledAt.IsZero() {
		return 0
	}
	return l.settledAt.Sub(l.detectedAt)
}

func logPaperExecutionLatency(t *MarketTrader, latency paperExecutionLatency) {
	settlement := latency.detectToSettlement()
	if settlement == 0 {
		settlement = latency.detectToExecution()
	}
	msg := fmt.Sprintf(
		"[%s] ⏱ %s latency | detect→start=%s | start→exec=%s | detect→exec=%s | detect→settle=%s | shares=%.2f | margin=%.2f%% | expected=$%.2f",
		latency.marketID,
		latency.opportunity,
		latency.detectToStart().Round(time.Microsecond),
		latency.startToExecution().Round(time.Microsecond),
		latency.detectToExecution().Round(time.Microsecond),
		settlement.Round(time.Microsecond),
		latency.shares,
		latency.marginPct,
		latency.expectedPnL,
	)
	t.TUI.LogEvent("%s", msg)
	if t.CSVLogger != nil {
		t.CSVLogger.Log("TRADE", t.ID, "LATENCY", msg, t.Engine.GetEquity())
	}
}

func (l paperCopytradeLatency) pollRoundTrip() time.Duration {
	if l.pollStartedAt.IsZero() || l.apiReceivedAt.IsZero() {
		return 0
	}
	return l.apiReceivedAt.Sub(l.pollStartedAt)
}

func (l paperCopytradeLatency) apiAgeAtDetect() time.Duration {
	if l.signalTimestamp == 0 || l.apiReceivedAt.IsZero() {
		return 0
	}
	return l.apiReceivedAt.Sub(time.Unix(l.signalTimestamp, 0))
}

func (l paperCopytradeLatency) detectToQuote() time.Duration {
	if l.apiReceivedAt.IsZero() || l.quoteReadyAt.IsZero() {
		return 0
	}
	return l.quoteReadyAt.Sub(l.apiReceivedAt)
}

func (l paperCopytradeLatency) quoteToExecution() time.Duration {
	if l.quoteReadyAt.IsZero() || l.executedAt.IsZero() {
		return 0
	}
	return l.executedAt.Sub(l.quoteReadyAt)
}

func (l paperCopytradeLatency) detectToExecution() time.Duration {
	if l.apiReceivedAt.IsZero() || l.executedAt.IsZero() {
		return 0
	}
	return l.executedAt.Sub(l.apiReceivedAt)
}

func logPaperCopytradeLatency(t *MarketTrader, latency paperCopytradeLatency) {
	apiAge := "n/a"
	if strings.EqualFold(strings.TrimSpace(latency.source), "trade") {
		if age := latency.apiAgeAtDetect(); age > 0 {
			apiAge = age.Round(time.Millisecond).String()
		}
	}
	txHash := strings.TrimSpace(latency.txHash)
	if txHash == "" {
		txHash = "n/a"
	}
	msg := fmt.Sprintf(
		"[%s] ⏱ Copytrade %s %s latency | source=%s | apiAge=%s | poll=%s | detect→quote=%s | quote→exec=%s | detect→exec=%s | tx=%s",
		latency.marketID,
		strings.ToUpper(strings.TrimSpace(latency.side)),
		latency.outcome,
		latency.source,
		apiAge,
		latency.pollRoundTrip().Round(time.Millisecond),
		latency.detectToQuote().Round(time.Millisecond),
		latency.quoteToExecution().Round(time.Millisecond),
		latency.detectToExecution().Round(time.Millisecond),
		txHash,
	)
	t.TUI.LogEvent("%s", msg)
	if t.CSVLogger != nil {
		t.CSVLogger.Log("TRADE", t.ID, "COPY_LATENCY", msg, t.Engine.GetEquity())
	}
}

func paperbotResolveCopytradeTarget(ctx context.Context, restClient *api.RestClient, liveCfg paper.TUISettings) (paperbotCopytradeTarget, error) {
	raw := strings.TrimSpace(liveCfg.CopytradeTarget)
	if raw == "" {
		return paperbotCopytradeTarget{}, fmt.Errorf("copytrade target is empty")
	}
	wallet, profile, err := restClient.ResolvePublicProfileTarget(ctx, raw)
	if err != nil {
		return paperbotCopytradeTarget{}, err
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
	return paperbotCopytradeTarget{
		Raw:    raw,
		Wallet: wallet,
		Label:  label,
	}, nil
}

func paperbotCopytradeLabelFromHint(slug, title string) string {
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

func parsePaperbotCopytradeEndTime(raw string) time.Time {
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

func buildPaperbotCopytradeMarketFromPosition(pos api.Position) *api.Market {
	if pos.ConditionID == "" || pos.TokenID == "" || pos.Outcome == "" {
		return nil
	}
	market := &api.Market{
		ConditionID: pos.ConditionID,
		Slug:        core.SanitizeString(pos.Slug),
		EndTime:     parsePaperbotCopytradeEndTime(pos.EndDate),
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

func buildPaperbotCopytradeMarketFromTrade(ctx context.Context, restClient *api.RestClient, trade api.PublicTrade) *api.Market {
	if restClient == nil || trade.ConditionID == "" {
		return nil
	}
	market, err := restClient.GetMarket(ctx, trade.ConditionID)
	if err == nil && market != nil {
		return market
	}
	return nil
}

func paperbotFindCopytradeMarkets(ctx context.Context, restClient *api.RestClient, wallet string, maxMarkets int) (map[string]*api.Market, error) {
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
		if !market.EndTime.IsZero() {
			if time.Now().After(market.EndTime) || time.Until(market.EndTime) < 30*time.Second {
				return false
			}
		}
		if label == "" {
			label = paperbotCopytradeLabelFromHint(market.Slug, "")
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
			if addMarket(paperbotCopytradeLabelFromHint(pos.Slug, pos.Title), buildPaperbotCopytradeMarketFromPosition(pos)) {
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
			if addMarket(paperbotCopytradeLabelFromHint(trade.Slug, trade.Title), buildPaperbotCopytradeMarketFromTrade(ctx, restClient, trade)) {
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

func paperbotCopytradeTargetShares(positions []api.Position) map[string]float64 {
	return paperbotCopytradeTargetSharesForCondition(positions, "")
}

func paperbotCopytradeSharesByCondition(positions []api.Position) map[string]map[string]float64 {
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

func paperbotCopytradeTargetSharesForCondition(positions []api.Position, conditionID string) map[string]float64 {
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

func paperbotCopytradeHoldsBothOutcomes(targetShares map[string]float64) bool {
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

func paperbotCopytradeHasAmbiguousPositionExit(positions []api.Position, conditionID string) bool {
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

func normalizeCopytradeConditionIDs(conditionIDs []string) []string {
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

func cloneCopytradeOutcomeShares(shares map[string]float64) map[string]float64 {
	if len(shares) == 0 {
		return map[string]float64{}
	}
	cloned := make(map[string]float64, len(shares))
	for outcome, qty := range shares {
		cloned[outcome] = qty
	}
	return cloned
}

func paperbotCopytradeIsRateLimited(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "status 429") || strings.Contains(msg, "error code: 1015")
}

func paperbotCopytradeRateLimitBackoff(streak int) time.Duration {
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

func filterCopytradeTradesByCondition(trades []api.PublicTrade, conditionID string) []api.PublicTrade {
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

func filterCopytradePositionsByCondition(positions []api.Position, conditionID string) []api.Position {
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

func (p *paperbotCopytradePoller) pendingSignalsForCondition(conditionID string, since time.Time) []api.PublicTrade {
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
			TransactionHash: sig.TxHash,
			Source:          "mempool",
			SignalID:        sig.SignalID,
			Slug:            sig.Slug,
		})
	}
	return trades
}

func (p *paperbotCopytradePoller) minedSignalsForCondition(conditionID string, since time.Time) []api.PublicTrade {
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
			TransactionHash: sig.TxHash,
			Source:          "onchain",
			SignalID:        sig.SignalID,
			Slug:            sig.Slug,
		})
	}
	return trades
}

func paperbotCopytradeHasOnchainWatcher(p *paperbotCopytradePoller) bool {
	return p != nil && ((p.pendingWatcher != nil && p.pendingWatcher.Enabled()) || (p.minedWatcher != nil && p.minedWatcher.Enabled()))
}

func (p *paperbotCopytradePoller) cachedSnapshotForCondition(conditionID string) paperbotCopytradeMarketSnapshot {
	if p == nil {
		return paperbotCopytradeMarketSnapshot{}
	}
	return paperbotCopytradeMarketSnapshot{
		Trades:        filterCopytradeTradesByCondition(p.lastSnapshot.Trades, conditionID),
		Positions:     filterCopytradePositionsByCondition(p.lastSnapshot.Positions, conditionID),
		TradesErr:     p.lastSnapshot.TradesErr,
		PositionsErr:  p.lastSnapshot.PositionsErr,
		PollStartedAt: p.lastPollStartedAt,
		PolledAt:      p.lastPoll,
	}
}

func (p *paperbotCopytradePoller) snapshotForCondition(ctx context.Context, restClient *api.RestClient, pollEvery time.Duration, conditionID string) (paperbotCopytradeMarketSnapshot, error) {
	if p == nil || restClient == nil {
		return paperbotCopytradeMarketSnapshot{}, fmt.Errorf("copytrade poller unavailable")
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
				return paperbotCopytradeMarketSnapshot{}, ctx.Err()
			case <-waitCh:
				continue
			}
		}

		p.fetching = true
		p.waitCh = make(chan struct{})
		wallet := p.wallet
		conditionIDs := append([]string(nil), p.conditionIDs...)
		cachedPositions := append([]api.Position(nil), p.lastSnapshot.Positions...)
		cachedPositionsValid := p.lastSnapshot.PositionsErr == nil
		pollStartedAt := time.Now()
		p.mu.Unlock()

		tradeLimit := len(conditionIDs) * 64
		if tradeLimit < 128 {
			tradeLimit = 128
		}
		if tradeLimit > 1000 {
			tradeLimit = 1000
		}
		positionLimit := len(conditionIDs) * 8
		if positionLimit < 32 {
			positionLimit = 32
		}
		if positionLimit > 500 {
			positionLimit = 500
		}
		snapshot := restClient.GetPublicActivitySnapshotWithFallback(
			ctx,
			wallet,
			conditionIDs,
			tradeLimit,
			0.01,
			positionLimit,
			cachedPositions,
			cachedPositionsValid,
			paperbotCopytradeTradeFetchTimeout(pollEvery),
			paperbotCopytradePositionFetchTimeout(pollEvery, cachedPositionsValid),
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
		} else {
			p.lastSnapshot.PositionsErr = snapshot.PositionsErr
		}
		if paperbotCopytradeIsRateLimited(snapshot.TradesErr) || (snapshot.TradesErr != nil && paperbotCopytradeIsRateLimited(snapshot.PositionsErr)) {
			p.rateLimitStreak++
			p.rateLimitUntil = now.Add(paperbotCopytradeRateLimitBackoff(p.rateLimitStreak))
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
		p.mu.Unlock()
		close(waitCh)

		return filtered, nil
	}
}

func paperbotClearPendingCopytradeSell(state *paperbotCopytradeState, outcome string) {
	if state == nil || outcome == "" {
		return
	}
	delete(state.pendingSellTarget, outcome)
	delete(state.pendingSellPoll, outcome)
}

func paperbotCopytradeTargetDelta(state *paperbotCopytradeState, outcome string, targetQty float64, pollTime time.Time) (float64, bool, bool) {
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
		paperbotClearPendingCopytradeSell(state, outcome)
		return 0, false, false
	}
	if lastPoll := state.lastTargetPoll[outcome]; !lastPoll.IsZero() && lastPoll.Equal(pollTime) {
		return 0, false, false
	}
	state.lastTargetPoll[outcome] = pollTime

	prev := state.targetShares[outcome]
	if targetQty > prev+0.01 {
		state.targetShares[outcome] = targetQty
		paperbotClearPendingCopytradeSell(state, outcome)
		return targetQty - prev, true, false
	}
	if targetQty >= prev-0.01 {
		state.targetShares[outcome] = targetQty
		paperbotClearPendingCopytradeSell(state, outcome)
		return 0, false, false
	}
	if _, waiting := state.pendingSellPoll[outcome]; waiting {
		state.targetShares[outcome] = targetQty
		paperbotClearPendingCopytradeSell(state, outcome)
		return targetQty - prev, true, false
	}
	state.pendingSellTarget[outcome] = targetQty
	state.pendingSellPoll[outcome] = pollTime
	return 0, false, true
}

func paperbotCopytradeTradeKey(trade api.PublicTrade) string {
	if signalID := strings.TrimSpace(trade.SignalID); signalID != "" {
		return "signal|" + signalID
	}
	txHash := strings.TrimSpace(trade.TransactionHash)
	if txHash != "" {
		return fmt.Sprintf("%s|%s|%s|%.6f|%.6f|%s|%d|%s", strings.TrimSpace(trade.ConditionID), core.SanitizeString(trade.Outcome), strings.ToUpper(strings.TrimSpace(trade.Side)), trade.Size, trade.Price, strings.TrimSpace(trade.Asset), trade.Timestamp, txHash)
	}
	return fmt.Sprintf("%s|%d|%s|%s|%.6f", strings.TrimSpace(trade.ConditionID), trade.Timestamp, core.SanitizeString(trade.Outcome), strings.ToUpper(strings.TrimSpace(trade.Side)), trade.Size)
}

func paperbotObserveCopytradeBuySignal(state *paperbotCopytradeState, trade api.PublicTrade) {
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

func paperbotEstimatedPositionBuySignals(state *paperbotCopytradeState, conditionID, outcome string, delta float64, mode string) []api.PublicTrade {
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

func paperbotCopytradeBootstrapStartTimestamp(startedAt time.Time) int64 {
	if startedAt.IsZero() {
		return 0
	}
	startTs := startedAt.Unix()
	if startedAt.Nanosecond() != 0 {
		startTs--
	}
	return startTs
}

func paperbotCopytradeRetrySignalFresh(now time.Time, trade api.PublicTrade) bool {
	if trade.Timestamp <= 0 {
		return true
	}
	tradeAt := time.Unix(trade.Timestamp, 0)
	if now.Before(tradeAt) {
		return true
	}
	return now.Sub(tradeAt) <= paperCopytradeRetryMaxAge
}

func paperbotCopytradeTakeRetryTrades(state *paperbotCopytradeState, now time.Time) []api.PublicTrade {
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
		if paperbotCopytradeRetrySignalFresh(now, trade) {
			fresh = append(fresh, trade)
		}
	}
	return fresh
}

func paperbotCopytradeQueueRetryTrades(state *paperbotCopytradeState, retries []api.PublicTrade) {
	if state == nil || len(retries) == 0 {
		return
	}
	if len(retries) > paperCopytradeRetryQueueCap {
		retries = retries[len(retries)-paperCopytradeRetryQueueCap:]
	}
	state.retryTrades = append(state.retryTrades, retries...)
	if len(state.retryTrades) > paperCopytradeRetryQueueCap {
		state.retryTrades = append([]api.PublicTrade(nil), state.retryTrades[len(state.retryTrades)-paperCopytradeRetryQueueCap:]...)
	}
}

func paperbotCopytradeFreshTrades(state *paperbotCopytradeState, trades []api.PublicTrade, conditionID string) []api.PublicTrade {
	if state == nil || len(trades) == 0 {
		return nil
	}
	conditionID = strings.TrimSpace(conditionID)
	now := time.Now()
	for key, seenAt := range state.seenTradeKeys {
		if now.Sub(seenAt) > 15*time.Minute {
			delete(state.seenTradeKeys, key)
		}
	}

	filtered := make([]api.PublicTrade, 0, len(trades))
	for _, trade := range trades {
		if conditionID != "" && !strings.EqualFold(strings.TrimSpace(trade.ConditionID), conditionID) {
			continue
		}
		if core.SanitizeString(trade.Outcome) == "" || trade.Size <= 0.01 {
			continue
		}
		filtered = append(filtered, trade)
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].Timestamp == filtered[j].Timestamp {
			return paperbotCopytradeTradeKey(filtered[i]) < paperbotCopytradeTradeKey(filtered[j])
		}
		return filtered[i].Timestamp < filtered[j].Timestamp
	})

	fresh := make([]api.PublicTrade, 0, len(filtered))
	currentPollCounts := make(map[string]int)
	for _, trade := range filtered {
		baseKey := paperbotCopytradeTradeKey(trade)
		currentPollCounts[baseKey]++
		
		totalSeen := state.seenTradeKeysCount[baseKey]
		if currentPollCounts[baseKey] > totalSeen {
			state.seenTradeKeysCount[baseKey] = currentPollCounts[baseKey]
			state.seenTradeKeys[fmt.Sprintf("%s#%d", baseKey, currentPollCounts[baseKey])] = now
			fresh = append(fresh, trade)
		}
	}
	if !state.tradesSeeded {
		state.tradesSeeded = true
		if state.startedAt.IsZero() {
			return nil
		}
		startTs := paperbotCopytradeBootstrapStartTimestamp(state.startedAt)
		bootstrap := make([]api.PublicTrade, 0, len(fresh))
		for _, trade := range fresh {
			if trade.Timestamp < startTs {
				continue
			}
			bootstrap = append(bootstrap, trade)
		}
		sort.Slice(bootstrap, func(i, j int) bool {
			if bootstrap[i].Timestamp == bootstrap[j].Timestamp {
				return paperbotCopytradeTradeKey(bootstrap[i]) < paperbotCopytradeTradeKey(bootstrap[j])
			}
			return bootstrap[i].Timestamp < bootstrap[j].Timestamp
		})
		return bootstrap
	}
	return fresh
}

func paperbotLocalPositionAvg(engine *paper.Engine, marketID, outcome string) (float64, float64) {
	if engine == nil {
		return 0, 0
	}
	pos, ok := getPaperMarketPosition(engine.GetPositions(), marketID, outcome)
	if !ok {
		return 0, 0
	}
	return pos.Quantity, pos.AvgPrice
}

func paperbotNormalizeMarketBuyShares(qty float64) float64 {
	if qty <= 0 {
		return 0
	}
	return math.Floor((qty*10000)+1e-9) / 10000
}

func paperbotNormalizeMarketSellShares(qty float64) float64 {
	if qty <= 0 {
		return 0
	}
	return math.Floor((qty*100)+1e-9) / 100
}

func paperbotFormatShareQty(qty float64) string {
	switch {
	case qty <= 0:
		return "0"
	case math.Abs(qty-math.Round(qty)) < 1e-9:
		return fmt.Sprintf("%.0f", qty)
	default:
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.5f", qty), "0"), ".")
	}
}

func paperbotCopytradePollEvery(settings paper.TUISettings) time.Duration {
	pollEvery := time.Duration(settings.CopytradePollIntervalMs) * time.Millisecond
	if pollEvery <= 0 {
		pollEvery = 2 * time.Second
	}
	return pollEvery
}

func paperbotTraderLoopInterval(settings paper.TUISettings) time.Duration {
	if normalizePaperArbMode(settings.PaperArbMode) == paperArbModeCopytrade {
		interval := paperbotCopytradePollEvery(settings) / 2
		if interval < paperCopytradeLoopIntervalMin {
			interval = paperCopytradeLoopIntervalMin
		}
		if interval > paperCopytradeLoopIntervalMax {
			interval = paperCopytradeLoopIntervalMax
		}
		return interval
	}
	return paperMainLoopInterval
}

func paperbotUIInterval(settings paper.TUISettings) time.Duration {
	if normalizePaperArbMode(settings.PaperArbMode) == paperArbModeCopytrade {
		interval := paperbotCopytradePollEvery(settings) / 2
		if interval < paperCopytradeUIRefreshMin {
			interval = paperCopytradeUIRefreshMin
		}
		if interval > paperCopytradeUIRefreshMax {
			interval = paperCopytradeUIRefreshMax
		}
		return interval
	}
	return paperUIRefreshInterval
}

func normalizePaperArbMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case paperArbModeBinanceGap:
		return paperArbModeBinanceGap
	case paperArbModeCopytrade:
		return paperArbModeCopytrade
	case paperArbModeMaker:
		return paperArbModeMaker
	default:
		return paperArbModeTaker
	}
}

func roundDown(v float64) float64 {
	return math.Floor(v*1000) / 1000
}

func roundPaperMakerPrice(v float64) float64 {
	return math.Round(v*1000) / 1000
}

func parseWSQuotedPrice(raw string) (float64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 || v > 1.0 {
		return 0, false
	}
	return v, true
}

func resolvePaperMakerQuoteGap(liveCfg paper.TUISettings, cfg *core.Config) float64 {
	if liveCfg.MakerQuoteGap > 0 {
		return liveCfg.MakerQuoteGap
	}
	if cfg != nil && cfg.MakerQuoteGap > 0 {
		return cfg.MakerQuoteGap
	}
	return paperMakerBaseOffset
}

func paperHasSaneTopOfBook(bid, ask float64) bool {
	if bid <= 0 || ask <= 0 || bid >= ask {
		return false
	}
	if bid >= terminalBidFloor || ask <= terminalAskCeil {
		return true
	}
	return (ask - bid) <= paperMaxSaneOutcomeSpread
}

const paperHighBidThreshold = 0.60

func paperPairHasHighBid(outcomes []string, tokenBids map[string]float64) bool {
	for _, out := range outcomes {
		if tokenBids[out] >= paperHighBidThreshold {
			return true
		}
	}
	return false
}

func paperLocalQuoteSanityReason(outcomes []string, bids, asks map[string]float64) string {
	highBidPresent := paperPairHasHighBid(outcomes, bids)

	for _, outcome := range outcomes {
		bid := bids[outcome]
		ask := asks[outcome]
		if !paperHasSaneTopOfBook(bid, ask) {
			if bid <= 0 || ask <= 0 {
				if highBidPresent {
					continue
				}
				return fmt.Sprintf("missing two-sided quote for %s", outcome)
			}
			if bid >= ask {
				return fmt.Sprintf("crossed local quote for %s (bid %.3f >= ask %.3f)", outcome, bid, ask)
			}
			return fmt.Sprintf("wide local spread for %s (bid %.3f ask %.3f spread %.3f > %.3f)", outcome, bid, ask, ask-bid, paperMaxSaneOutcomeSpread)
		}
	}

	if len(outcomes) == 2 && !paperLooksLikeTerminalBook(outcomes, bids, asks) {
		askSum := asks[outcomes[0]] + asks[outcomes[1]]
		if !highBidPresent && askSum > paperMaxSaneAskPairSum {
			return fmt.Sprintf("ask pair sum %.3f > %.3f", askSum, paperMaxSaneAskPairSum)
		}
		bidSum := bids[outcomes[0]] + bids[outcomes[1]]
		if bidSum < paperMinSaneBidPairSum {
			return fmt.Sprintf("bid pair sum %.3f < %.3f", bidSum, paperMinSaneBidPairSum)
		}
	}

	return ""
}

func hasValidPaperPairQuotes(outcomes []string, bids, asks map[string]float64) bool {
	return paperLocalQuoteSanityReason(outcomes, bids, asks) == ""
}

func shouldPaperReconnectWS(outcomes []string, bids, asks map[string]float64, pairQuoteAge, staleThreshold time.Duration, terminalBookState bool) bool {
	if staleThreshold <= 0 {
		staleThreshold = 15 * time.Second
	}
	if terminalBookState || pairQuoteAge <= staleThreshold {
		return false
	}
	return paperLocalQuoteSanityReason(outcomes, bids, asks) != ""
}

func paperLooksLikeTerminalBook(outcomes []string, bids, asks map[string]float64) bool {
	if len(outcomes) == 0 {
		return false
	}

	sawExtreme := false
	for _, outcome := range outcomes {
		bid := bids[outcome]
		ask := asks[outcome]

		if bid > 0 && bid < terminalBidFloor {
			return false
		}
		if ask > 0 && ask > terminalAskCeil {
			return false
		}
		if bid >= terminalBidFloor || (ask > 0 && ask <= terminalAskCeil) {
			sawExtreme = true
		}
	}

	return sawExtreme
}

func paperQuoteMapsEqual(outcomes []string, bidsA, asksA, bidsB, asksB map[string]float64) bool {
	for _, outcome := range outcomes {
		if math.Abs(bidsA[outcome]-bidsB[outcome]) > 1e-9 {
			return false
		}
		if math.Abs(asksA[outcome]-asksB[outcome]) > 1e-9 {
			return false
		}
	}
	return true
}

func paperShouldClearLocalPairQuotes(outcomes []string, bids, asks map[string]float64) bool {
	if hasValidPaperPairQuotes(outcomes, bids, asks) || paperLooksLikeTerminalBook(outcomes, bids, asks) {
		return false
	}
	if paperPairHasHighBid(outcomes, bids) {
		return false
	}
	return true
}

func paperStorePublishedQuotes(outcomes []string, srcBids, srcAsks, dstBids, dstAsks map[string]float64) {
	for _, outcome := range outcomes {
		dstBids[outcome] = srcBids[outcome]
		dstAsks[outcome] = srcAsks[outcome]
	}
}

func paperLatestQuoteUpdate(outcomes []string, quoteState map[string]paperQuoteState) (time.Time, string) {
	latest := time.Time{}
	latestSource := ""
	for _, outcome := range outcomes {
		state, ok := quoteState[outcome]
		if !ok || state.UpdatedAt.IsZero() {
			continue
		}
		if latest.IsZero() || state.UpdatedAt.After(latest) {
			latest = state.UpdatedAt
			latestSource = state.Source
		}
	}
	return latest, latestSource
}

func paperNormalizeDisplaySource(raw string) string {
	source := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case strings.HasPrefix(source, "rest"):
		return "REST"
	case strings.HasPrefix(source, "ws"):
		return "WS"
	default:
		return "WS"
	}
}

func paperDisplayHasUsableQuotes(outcomes []string, bids, asks map[string]float64) bool {
	return hasValidPaperPairQuotes(outcomes, bids, asks) || paperLooksLikeTerminalBook(outcomes, bids, asks)
}

func paperSyncDisplayQuotes(outcomes []string, liveBids, liveAsks, displayBids, displayAsks map[string]float64, authoritative bool) bool {
	nextBids := make(map[string]float64, len(outcomes))
	nextAsks := make(map[string]float64, len(outcomes))
	for _, outcome := range outcomes {
		nextBids[outcome] = displayBids[outcome]
		nextAsks[outcome] = displayAsks[outcome]
	}

	switch {
	case hasValidPaperPairQuotes(outcomes, liveBids, liveAsks):
		paperStorePublishedQuotes(outcomes, liveBids, liveAsks, nextBids, nextAsks)
	case paperLooksLikeTerminalBook(outcomes, liveBids, liveAsks):
		for _, outcome := range outcomes {
			if liveBids[outcome] > 0 {
				nextBids[outcome] = liveBids[outcome]
			}
			if liveAsks[outcome] > 0 {
				nextAsks[outcome] = liveAsks[outcome]
			}
		}
	case authoritative:
		paperStorePublishedQuotes(outcomes, liveBids, liveAsks, nextBids, nextAsks)
	default:
		return false
	}

	if paperQuoteMapsEqual(outcomes, nextBids, nextAsks, displayBids, displayAsks) {
		return false
	}
	paperStorePublishedQuotes(outcomes, nextBids, nextAsks, displayBids, displayAsks)
	return true
}

func summarizePaperRound(engine *paper.Engine, startingEquity float64, roundStartTrades int) (roundPnL, totalEquity float64, roundTrades int, stats paper.Stats) {
	stats = engine.GetStats()
	totalEquity = engine.GetBookEquity()
	roundPnL = totalEquity - startingEquity
	roundTrades = stats.TotalTrades - roundStartTrades
	if roundTrades < 0 {
		roundTrades = 0
	}
	return roundPnL, totalEquity, roundTrades, stats
}

func paperPairQuoteAge(lastPairUpdate, now time.Time) time.Duration {
	if now.IsZero() {
		now = time.Now()
	}
	if lastPairUpdate.IsZero() {
		return time.Duration(1 << 62)
	}
	return now.Sub(lastPairUpdate)
}

func shouldUseLocalPaperPair(outcomes []string, bids, asks map[string]float64, lastPairUpdate time.Time, maxAge time.Duration, now time.Time) bool {
	return hasValidPaperPairQuotes(outcomes, bids, asks) && paperPairQuoteAge(lastPairUpdate, now) <= maxAge
}

func paperExecutionQuoteGuardAge(localQuoteMaxAge time.Duration) time.Duration {
	if localQuoteMaxAge <= 0 || localQuoteMaxAge > paperExecutionGuardQuoteMaxAge {
		return paperExecutionGuardQuoteMaxAge
	}
	return localQuoteMaxAge
}

func syncPaperPairUpdate(t *MarketTrader, now time.Time) {
	if hasValidPaperPairQuotes(t.Outcomes, t.TokenBids, t.TokenAsks) {
		t.LastPairUpdate = now
	}
	if t.PolySignalTracker != nil && len(t.Outcomes) == 2 {
		for _, outcome := range t.Outcomes {
			bid := t.TokenBids[outcome]
			ask := t.TokenAsks[outcome]
			if bid > 0 && ask > 0 {
				t.PolySignalTracker.Record(outcome, bid, ask, now)
			}
		}
	}
}

func computePaperMakerArbPrices(bid1, ask1, bid2, ask2, maxSum float64) (float64, float64, bool) {
	minPrice1 := paperMakerQuoteStep
	if bid1 > 0 {
		minPrice1 = bid1 + paperMakerQuoteStep
	}
	minPrice2 := paperMakerQuoteStep
	if bid2 > 0 {
		minPrice2 = bid2 + paperMakerQuoteStep
	}
	maxPrice1 := ask1 - paperMakerQuoteStep
	maxPrice2 := ask2 - paperMakerQuoteStep
	if ask1 <= 0 || ask2 <= 0 || maxPrice1 < minPrice1 || maxPrice2 < minPrice2 {
		return 0, 0, false
	}

	price1 := roundDown((minPrice1 + maxPrice1) / 2)
	price2 := roundDown((minPrice2 + maxPrice2) / 2)
	if price1 < minPrice1 {
		price1 = minPrice1
	}
	if price2 < minPrice2 {
		price2 = minPrice2
	}
	if price1 > maxPrice1 {
		price1 = maxPrice1
	}
	if price2 > maxPrice2 {
		price2 = maxPrice2
	}

	for price1+price2 > maxSum+1e-9 {
		if price1-minPrice1 >= price2-minPrice2 && price1-paperMakerQuoteStep >= minPrice1 {
			price1 = roundDown(price1 - paperMakerQuoteStep)
			continue
		}
		if price2-paperMakerQuoteStep >= minPrice2 {
			price2 = roundDown(price2 - paperMakerQuoteStep)
			continue
		}
		return 0, 0, false
	}
	return price1, price2, true
}

func computePaperMakerSplitSellPrices(bid1, ask1, bid2, ask2, minSum float64) (float64, float64, bool) {
	minPrice1 := roundPaperMakerPrice(bid1 + paperMakerQuoteStep)
	minPrice2 := roundPaperMakerPrice(bid2 + paperMakerQuoteStep)
	maxPrice1 := roundPaperMakerPrice(ask1 - paperMakerQuoteStep)
	maxPrice2 := roundPaperMakerPrice(ask2 - paperMakerQuoteStep)
	if ask1 <= 0 || ask2 <= 0 || maxPrice1 < minPrice1 || maxPrice2 < minPrice2 {
		return 0, 0, false
	}

	price1 := minPrice1
	price2 := minPrice2
	for price1+price2 < minSum-1e-9 {
		room1 := maxPrice1 - price1
		room2 := maxPrice2 - price2
		if room1 < paperMakerQuoteStep-1e-9 && room2 < paperMakerQuoteStep-1e-9 {
			return 0, 0, false
		}
		if room1 >= room2 && room1 >= paperMakerQuoteStep-1e-9 {
			price1 = roundPaperMakerPrice(math.Min(maxPrice1, price1+paperMakerQuoteStep))
			continue
		}
		if room2 >= paperMakerQuoteStep-1e-9 {
			price2 = roundPaperMakerPrice(math.Min(maxPrice2, price2+paperMakerQuoteStep))
			continue
		}
		if room1 >= paperMakerQuoteStep-1e-9 {
			price1 = roundPaperMakerPrice(math.Min(maxPrice1, price1+paperMakerQuoteStep))
			continue
		}
		return 0, 0, false
	}
	return price1, price2, true
}

func paperMakerQuoteKey(side, outcome string) string {
	return strings.ToLower(strings.TrimSpace(side)) + ":" + outcome
}

func isPaperOrderActive(order *paper.LimitOrder) bool {
	if order == nil {
		return false
	}
	return order.Status == paper.OrderStatusOpen || order.Status == paper.OrderStatusPartial
}

func getPaperMarketPosition(positions map[string]paper.Position, marketID, outcome string) (paper.Position, bool) {
	pos, ok := positions[marketID+":"+outcome]
	return pos, ok
}

func estimatePaperWinner(outcomes []string, bids, asks, floatPrices map[string]float64) (string, float64) {
	if len(outcomes) == 0 {
		return "", 0
	}

	bestOutcome := outcomes[0]
	highestProb := 0.0

	for _, outcome := range outcomes {
		prob := bids[outcome]
		if prob <= 0 {
			ask := asks[outcome]
			if ask > 0 {
				prob = ask - 0.01
			}
		}
		if prob <= 0 {
			prob = floatPrices[outcome]
		}
		if prob > highestProb {
			highestProb = prob
			bestOutcome = outcome
		}
	}

	return bestOutcome, highestProb
}

func detectTerminalWinnerFromPrices(outcomes []string, bids, asks, floatPrices map[string]float64) (string, float64, string, bool) {
	if len(outcomes) == 0 {
		return "", 0, "", false
	}

	bestOutcome := ""
	bestPrice := 0.0
	bestSource := ""
	secondBest := 0.0

	for i, outcome := range outcomes {
		candidatePrice := 0.0
		candidateSource := ""

		if bid := bids[outcome]; bid > candidatePrice {
			candidatePrice = bid
			candidateSource = "bid"
		}
		if ask := asks[outcome]; ask >= terminalWinnerFloor && ask > candidatePrice {
			candidatePrice = ask
			candidateSource = "ask"
		}
		if mid := floatPrices[outcome]; mid > candidatePrice {
			candidatePrice = mid
			candidateSource = "mid"
		}

		// Binary fallback: if the peer ask is pinned near $0, this outcome is
		// effectively pinned near $1 even when one side disappears in sparse books.
		if len(outcomes) == 2 {
			peer := outcomes[1-i]
			if peerAsk := asks[peer]; peerAsk > 0 && peerAsk <= terminalAskCeil {
				if 1.0 > candidatePrice {
					candidatePrice = 1.0
					candidateSource = "peer_ask"
				}
			}
		}

		if candidatePrice > bestPrice+1e-9 {
			secondBest = bestPrice
			bestPrice = candidatePrice
			bestOutcome = outcome
			bestSource = candidateSource
		} else if candidatePrice > secondBest {
			secondBest = candidatePrice
		}
	}

	if bestOutcome == "" || bestPrice < terminalWinnerFloor {
		return "", 0, "", false
	}
	if math.Abs(bestPrice-secondBest) <= 1e-6 {
		return "", 0, "", false
	}

	return bestOutcome, bestPrice, bestSource, true
}

func computePaperMakerInventorySkew(positionShares, peerShares, targetShares float64) float64 {
	return strategy.ComputeMakerInventorySkew(positionShares, peerShares, targetShares)
}

func computePaperMakerSkewedQuote(side string, bid, ask, skew, quoteGap float64, params strategy.MakerParams) (float64, bool) {
	return strategy.ComputeMakerSkewedQuote(strings.EqualFold(side, "buy"), bid, ask, skew, quoteGap, params)
}

func computePaperMakerPairBuyPrices(bid1, ask1, bid2, ask2, maxPairCost, inventoryDelta float64, params strategy.MakerParams) (float64, float64, bool) {
	return strategy.ComputeMakerPairBuyPrices(bid1, ask1, bid2, ask2, maxPairCost, inventoryDelta, params)
}

func computePaperMakerPairQuoteQty(baseTradeValue, pairedShares, maxInventoryValue, cash, price1, price2 float64, params strategy.MakerParams) float64 {
	return strategy.ComputeMakerPairQuoteQty(baseTradeValue, pairedShares, maxInventoryValue, cash, price1, price2, params, math.Floor)
}

func computePaperMakerBuyQty(baseShares, positionShares, skew, maxInventory, cash, price float64, params strategy.MakerParams) float64 {
	return strategy.ComputeMakerBuyQty(baseShares, positionShares, skew, maxInventory, cash, price, params, math.Floor)
}

func computePaperMakerSellQty(baseShares, positionShares, skew, price float64, params strategy.MakerParams) float64 {
	return strategy.ComputeMakerSellQty(baseShares, positionShares, skew, price, params, math.Floor)
}

func computePaperMakerProtectedSellQuote(bid, ask, avgCost, minEdge, skew, quoteGap float64, feeRateBps int, timeRemaining time.Duration, params strategy.MakerParams) (float64, bool) {
	return strategy.ComputeMakerProtectedSellQuote(bid, ask, avgCost, minEdge, skew, quoteGap, feeRateBps, timeRemaining, params)
}

func shouldPaperMakerBlockBuy(positionShares float64, sellOK bool, peerShares, peerAvgCost, price, minEdge float64) bool {
	return strategy.ShouldMakerBlockBuy(positionShares, sellOK, peerShares, peerAvgCost, price, minEdge)
}

func clearPaperMakerQuoteReference(t *MarketTrader, order *paper.LimitOrder) {
	if order == nil || len(t.MakerQuotes) == 0 {
		return
	}
	for key, existing := range t.MakerQuotes {
		if existing != nil && existing.ID == order.ID {
			delete(t.MakerQuotes, key)
		}
	}
}

func cancelPaperMakerQuote(t *MarketTrader, side, outcome string) bool {
	key := paperMakerQuoteKey(side, outcome)
	existing := t.MakerQuotes[key]
	delete(t.MakerQuotes, key)
	if !isPaperOrderActive(existing) {
		return false
	}
	if err := t.OrderBook.CancelOrder(existing.ID); err != nil {
		return false
	}
	return true
}

func cancelAllPaperMakerQuotes(t *MarketTrader, reason string) {
	if len(t.MakerQuotes) == 0 {
		updatePaperPendingOrders(t)
		return
	}
	quotes := make(map[string]*paper.LimitOrder, len(t.MakerQuotes))
	for key, order := range t.MakerQuotes {
		quotes[key] = order
	}
	t.MakerQuotes = make(map[string]*paper.LimitOrder)
	for _, order := range quotes {
		if isPaperOrderActive(order) {
			_ = t.OrderBook.CancelOrder(order.ID)
		}
	}
	if reason != "" {
		t.TUI.LogEvent("[%s] 🧹 Maker quotes cancelled: %s", t.ID, reason)
	}
	updatePaperPendingOrders(t)
}

func upsertPaperMakerQuote(t *MarketTrader, side, outcome string, price, qty float64) bool {
	key := paperMakerQuoteKey(side, outcome)
	existing := t.MakerQuotes[key]

	// Check if the total dollar value meets the minimum requirement
	orderValue := qty * price
	if orderValue < t.Config.MakerMinQuoteValue || price <= 0 {
		return cancelPaperMakerQuote(t, side, outcome)
	}
	if isPaperOrderActive(existing) && math.Abs(existing.Price-price) < 1e-9 && math.Abs(existing.RemainingQty()-qty) < 1e-9 {
		return false
	}
	if isPaperOrderActive(existing) {
		_ = t.OrderBook.CancelOrder(existing.ID)
	}
	order := t.OrderBook.PlaceOrder(outcome, side, price, qty, 0)
	if t.MakerQuotes == nil {
		t.MakerQuotes = make(map[string]*paper.LimitOrder)
	}
	t.MakerQuotes[key] = order
	return true
}

func updatePaperPendingOrders(t *MarketTrader) {
	pending := make(map[string][]paper.PendingOrder)
	for _, order := range t.MakerQuotes {
		if !isPaperOrderActive(order) {
			continue
		}
		pending[order.Outcome] = append(pending[order.Outcome], paper.PendingOrder{
			MarketID: t.ID,
			Outcome:  order.Outcome,
			Price:    order.Price,
			Qty:      order.RemainingQty(),
			Side:     strings.ToUpper(order.Side),
		})
	}
	t.TUI.SetPendingOrders(t.ID, pending)
}

func maintainPaperMakerInventoryQuotes(t *MarketTrader, now time.Time) {
	if len(t.Outcomes) != 2 {
		cancelAllPaperMakerQuotes(t, "maker mode requires exactly 2 outcomes")
		return
	}
	localQuoteMaxAge := paperExecutionQuoteGuardAge(core.ResolveExecutionLocalQuoteMaxAge(t.Config))
	if !shouldUseLocalPaperPair(t.Outcomes, t.TokenBids, t.TokenAsks, t.LastPairUpdate, localQuoteMaxAge, now) {
		cancelAllPaperMakerQuotes(t, "waiting for fresh pair quotes")
		return
	}
	if !t.LastMakerSync.IsZero() && now.Sub(t.LastMakerSync) < paperMakerRequoteInterval {
		updatePaperPendingOrders(t)
		return
	}

	bid1, ask1 := t.TokenBids[t.Outcomes[0]], t.TokenAsks[t.Outcomes[0]]
	bid2, ask2 := t.TokenBids[t.Outcomes[1]], t.TokenAsks[t.Outcomes[1]]
	if bid1 <= 0 || ask1 <= 0 || bid2 <= 0 || ask2 <= 0 {
		cancelAllPaperMakerQuotes(t, "waiting for valid bid/ask on both outcomes")
		return
	}

	liveCfg := t.TUI.GetSettings()
	currentCash := t.Engine.GetBalance()
	positions := t.Engine.GetPositions()
	pos1, hasPos1 := getPaperMarketPosition(positions, t.ID, t.Outcomes[0])
	pos2, hasPos2 := getPaperMarketPosition(positions, t.ID, t.Outcomes[1])
	shares1, shares2 := 0.0, 0.0
	avgCost1, avgCost2 := 0.0, 0.0
	if hasPos1 {
		shares1 = pos1.Quantity
		avgCost1 = pos1.AvgPrice
	}
	if hasPos2 {
		shares2 = pos2.Quantity
		avgCost2 = pos2.AvgPrice
	}

	// Auto-merge delta-neutral inventory to free up capital and lock in spread profits
	if shares1 > 0 && shares2 > 0 {
		mergeQty := math.Min(shares1, shares2)
		if mergeQty >= 1.0 {
			result := t.Engine.MergeForMarket(t.ID, t.Outcomes[0], t.Outcomes[1], mergeQty)
			if t.SplitInventory != nil {
				t.SplitInventory.RecordMerge(t.ID, t.Outcomes[0], t.Outcomes[1], mergeQty)
			}
			if result != nil && result.PnL != 0 {
				t.TUI.LogEvent("[%s] 💰 Auto-merge realized PnL: $%.2f", t.ID, result.PnL)
			}

			// Re-fetch after merge
			positions = t.Engine.GetPositions()
			pos1, hasPos1 = getPaperMarketPosition(positions, t.ID, t.Outcomes[0])
			pos2, hasPos2 = getPaperMarketPosition(positions, t.ID, t.Outcomes[1])
			if hasPos1 {
				shares1 = pos1.Quantity
				avgCost1 = pos1.AvgPrice
			} else {
				shares1 = 0
				avgCost1 = 0
			}
			if hasPos2 {
				shares2 = pos2.Quantity
				avgCost2 = pos2.AvgPrice
			} else {
				shares2 = 0
				avgCost2 = 0
			}
		}
	}

	timeToEnd := time.Until(t.EndTime)
	mergeBuffer := 30 * time.Second
	if liveCfg.MakerMergeBufferSeconds > 0 {
		mergeBuffer = time.Duration(liveCfg.MakerMergeBufferSeconds) * time.Second
	} else if t.Config.MakerMergeBufferSeconds > 0 {
		mergeBuffer = time.Duration(t.Config.MakerMergeBufferSeconds) * time.Second
	}
	if timeToEnd < mergeBuffer && timeToEnd > 0 {
		cancelAllPaperMakerQuotes(t, "near expiry merge cleanup")
		mergeableShares := math.Floor(math.Min(shares1, shares2))
		if mergeableShares >= 1.0 {
			result := t.Engine.MergeForMarket(t.ID, t.Outcomes[0], t.Outcomes[1], mergeableShares)
			t.Engine.RecalculateDrawdown()
			t.TUI.LogEvent("[%s] 💰 MAKER MERGE (sim): Merged %.0f paired shares (pnl $%.2f)", t.ID, result.Shares, result.PnL)
		}
		updatePaperPendingOrders(t)
		return
	}

	minQuoteValue := t.Config.MakerMinQuoteValue
	if liveCfg.MakerMinQuoteValue > 0 {
		minQuoteValue = liveCfg.MakerMinQuoteValue
	}
	if minQuoteValue <= 0 {
		minQuoteValue = paperMakerMinQuoteValue
	}
	targetMult := t.Config.MakerInventoryTargetMult
	if liveCfg.MakerInventoryTargetMult > 0 {
		targetMult = liveCfg.MakerInventoryTargetMult
	}
	if targetMult <= 0 {
		targetMult = paperMakerInventoryTargetMult
	}
	capMult := t.Config.MakerInventoryCapMult
	if liveCfg.MakerInventoryCapMult > 0 {
		capMult = liveCfg.MakerInventoryCapMult
	}
	if capMult <= 0 {
		capMult = paperMakerInventoryCapMult
	}

	baseTradeValue := t.Config.CalculateTradeSize(t.Engine.GetSizingBalance())
	// We no longer clamp baseTradeValue up to minQuoteValue to avoid forcing users
	// to trade larger amounts than their configured TradeScaleFactor. If baseTradeValue
	// is too small, strategy.ComputeMakerBuyQty will return 0 and skip quoting.

	targetValue := math.Max(minQuoteValue, baseTradeValue*targetMult)
	maxInventoryValue := math.Max(targetValue, baseTradeValue*capMult)
	minPairEdge := t.Config.MinMarginPercent / 100.0
	maxPairCost := 1.0 - minPairEdge

	makerParams := paperMakerStrategyParams
	makerParams.MinQuoteValue = minQuoteValue

	pairedShares := math.Min(shares1, shares2)
	inventoryDelta := shares1 - shares2
	buyPrice1, buyPrice2, buyOK := computePaperMakerPairBuyPrices(bid1, ask1, bid2, ask2, maxPairCost, inventoryDelta, makerParams)
	maxMakerBuyPrice := liveCfg.MaxAskPrice
	if maxMakerBuyPrice <= 0 || maxMakerBuyPrice > 0.99 {
		maxMakerBuyPrice = 0.99
	}
	minMakerBuyPrice := liveCfg.MinAskPrice
	if !buyOK || buyPrice1 > maxMakerBuyPrice || buyPrice1 < minMakerBuyPrice || buyPrice2 > maxMakerBuyPrice || buyPrice2 < minMakerBuyPrice {
		buyOK = false
	}
	buyQty1 := 0.0
	buyQty2 := 0.0
	if buyOK {
		pairQuoteQty := math.Min(MaxSharesPerSell, computePaperMakerPairQuoteQty(baseTradeValue, pairedShares, maxInventoryValue, currentCash, buyPrice1, buyPrice2, makerParams))
		maxImbalanceShares := math.Max(1.0, math.Floor(baseTradeValue/math.Max(maxPairCost, paperMakerQuoteStep)))
		if pairQuoteQty > maxImbalanceShares {
			maxImbalanceShares = pairQuoteQty
		}
		buyQty1 = pairQuoteQty
		buyQty2 = pairQuoteQty
		if inventoryDelta > 0 {
			heavyScale := math.Min(1.0, inventoryDelta/math.Max(maxImbalanceShares, 1.0))
			buyQty1 = math.Floor(pairQuoteQty * (1.0 - heavyScale))
			if avgCost1 > 0 && buyPrice2+avgCost1 > maxPairCost+1e-9 {
				buyQty2 = 0
			}
		} else if inventoryDelta < 0 {
			heavyScale := math.Min(1.0, (-inventoryDelta)/math.Max(maxImbalanceShares, 1.0))
			buyQty2 = math.Floor(pairQuoteQty * (1.0 - heavyScale))
			if avgCost2 > 0 && buyPrice1+avgCost2 > maxPairCost+1e-9 {
				buyQty1 = 0
			}
		}
	}

	changed := false
	if upsertPaperMakerQuote(t, "buy", t.Outcomes[0], buyPrice1, buyQty1) {
		changed = true
	}
	if upsertPaperMakerQuote(t, "buy", t.Outcomes[1], buyPrice2, buyQty2) {
		changed = true
	}
	if upsertPaperMakerQuote(t, "sell", t.Outcomes[0], 0, 0) {
		changed = true
	}
	if upsertPaperMakerQuote(t, "sell", t.Outcomes[1], 0, 0) {
		changed = true
	}

	t.LastMakerSync = now
	if changed {
		t.TUI.LogEvent("[%s] 🧾 Maker pair bids refreshed: %s buy@$%.3f x %.0f | %s buy@$%.3f x %.0f | pair=$%.3f",
			t.ID,
			t.Outcomes[0], buyPrice1, buyQty1,
			t.Outcomes[1], buyPrice2, buyQty2,
			buyPrice1+buyPrice2,
		)
	}
	updatePaperPendingOrders(t)
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
	var tui *paper.TUI
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
		if tui != nil {
			tui.CancelAllOrders()
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

	// Ensure terminal is restored on any exit
	defer core.RestoreTerminal()

	startTime := time.Now()

	// Clear screen at startup
	fmt.Print("\033[H\033[2J")
	fmt.Println("🎰 POLYARB-15M Starting (Multi-Asset: BTC, ETH, SOL, XRP)...")
	fmt.Printf("⏰ Started at: %s\n", startTime.Format("2006-01-02 15:04:05"))

	// Initialize persistent components (survive market rotation)
	engine = paper.NewEngine(StartingBalance)

	// Load paperbot settings + env-backed secrets
	cfg, err := core.LoadBotConfig("paperbot")
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	fmt.Println("✅ Config loaded successfully")

	// If Kalshi is selected as the exchange at startup, ensure we have keys
	// since Kalshi websockets and endpoints require authentication even for market data.
	if cfg.Exchange == "kalshi" {
		if cfg.KalshiAPIKey == "" || cfg.KalshiPK == "" {
			if err := setup.EnsureKalshiCredentials(cfg); err != nil {
				if errors.Is(err, setup.ErrSkipToPolymarket) {
					cfg.Exchange = "polymarket"
					_ = cfg.SaveSettings()
				} else {
					return fmt.Errorf("failed to setup kalshi credentials: %w", err)
				}
			}
		}
	}

	// Disable terminal echo to prevent arrow keys from appearing
	// This is done via stty which works on most Unix systems including Termux
	disableEcho := exec.Command("stty", "-echo", "-icanon")
	disableEcho.Stdin = os.Stdin
	_ = disableEcho.Run() // Ignore errors if stty not available

	// Apply fee settings to engine
	engine.SetFeeRateBps(cfg.FeeRateBps)

	if cfg.FeeRateBps > 0 {
		// Show effective fee at p=0.50 (worst case for arb)
		// Formula: fee_tokens = shares * base_rate * 2 * p * (1-p)
		// At p=0.50: curve = 0.5, so effective = base_rate * 0.5
		effectiveAt50 := float64(cfg.FeeRateBps) / 10000.0 * 0.5 * 100.0
		fmt.Printf("💰 Fee simulation enabled: %d bps base (~%.1f%% effective at p=0.50)\n", cfg.FeeRateBps, effectiveAt50)
	}

	restClient := api.NewRestClient(cfg.Exchange)

	// Resolution cache for on-chain market resolution checking
	// Paperbot can use read-only Polygon RPC when available, plus REST fallback.
	var polygonClient *api.PolygonClient
	if strings.TrimSpace(cfg.PolygonRPCURL) != "" {
		polygonClient = api.NewPolygonClient(cfg.PolygonRPCURL)
	}
	resolutionCache := api.NewResolutionCache(polygonClient, nil, restClient)
	// Create shared TUI (persistent across market rotations)
	tui = paper.NewTUI(engine, nil)

	// Seed settings panel from config (.env), so the live panel reflects initial values
	tui.InitSettings(paper.TUISettings{
		Exchange:                     cfg.Exchange,
		MarketSlug:                   cfg.MarketSlug,
		MaxMarkets:                   cfg.MaxMarkets,
		Timeframe:                    cfg.Timeframe,
		TradeSizingMode:              cfg.TradeSizingMode,
		TradeScaleFactor:             cfg.TradeScaleFactor,
		TradeSizeUSDC:                cfg.TradeSizeUSDC,
		MinMarginPercent:             cfg.MinMarginPercent,
		BinanceSignalThresholdPct:    cfg.BinanceSignalThresholdPct,
		PaperBinanceExecutionDelayMs: cfg.PaperBinanceExecutionDelayMs,
		PaperArbMode:                 normalizePaperArbMode(cfg.PaperArbMode),
		CopytradeTarget:              cfg.CopytradeTarget,
		CopytradePollIntervalMs:      cfg.CopytradePollIntervalMs,
		CopytradeSizingMode:          cfg.CopytradeSizingMode,
		CopytradeSizeUSDC:            cfg.CopytradeSizeUSDC,
		CopytradeSizeShares:          cfg.CopytradeSizeShares,
		CopytradeSizePercent:         cfg.CopytradeSizePercent,
		CopytradeMaxSlippagePct:      cfg.CopytradeMaxSlippagePct,
		SplitMinMarginSell:           cfg.SplitMinMarginSell,
		SplitStrategyEnabled:         cfg.SplitStrategyEnabled,
		SplitInitialCapPct:           cfg.SplitInitialCapPct,
		SplitReplenishCapPct:         cfg.SplitReplenishCapPct,
		MakerMergeBufferSeconds:      cfg.MakerMergeBufferSeconds,
		MakerQuoteGap:                cfg.MakerQuoteGap,
		MinAskPrice:                  cfg.MinAskPrice,
		MaxAskPrice:                  cfg.MaxAskPrice,
		MaxTradeSize:                 cfg.MaxTradeSize,
		MaxDailyLoss:                 cfg.MaxDailyLoss,
		TakerCloseMarket:             cfg.TakerCloseMarket,
		TakerCloseMarketTime:         cfg.TakerCloseMarketTime,
		TakerCloseMarketSlippage:     cfg.TakerCloseMarketSlippage,
		TakerCloseMarketMinPrice:     cfg.TakerCloseMarketMinPrice,
		TradingHoursMode:             cfg.TradingHoursMode,
	}, func(s paper.TUISettings) {
		cfg.Exchange = s.Exchange
		cfg.MarketSlug = s.MarketSlug
		cfg.MaxMarkets = s.MaxMarkets
		cfg.Timeframe = s.Timeframe
		cfg.TradeSizingMode = s.TradeSizingMode
		cfg.TradeScaleFactor = s.TradeScaleFactor
		cfg.TradeSizeUSDC = s.TradeSizeUSDC
		cfg.MinMarginPercent = s.MinMarginPercent
		cfg.BinanceSignalThresholdPct = s.BinanceSignalThresholdPct
		cfg.PaperBinanceExecutionDelayMs = s.PaperBinanceExecutionDelayMs
		cfg.PaperArbMode = normalizePaperArbMode(s.PaperArbMode)
		cfg.CopytradeTarget = strings.TrimSpace(s.CopytradeTarget)
		cfg.CopytradePollIntervalMs = s.CopytradePollIntervalMs
		cfg.CopytradeSizingMode = s.CopytradeSizingMode
		cfg.CopytradeSizeUSDC = s.CopytradeSizeUSDC
		cfg.CopytradeSizeShares = s.CopytradeSizeShares
		cfg.CopytradeSizePercent = s.CopytradeSizePercent
		cfg.CopytradeMaxSlippagePct = s.CopytradeMaxSlippagePct
		cfg.SplitMinMarginSell = s.SplitMinMarginSell
		cfg.SplitStrategyEnabled = s.SplitStrategyEnabled
		cfg.SplitInitialCapPct = s.SplitInitialCapPct
		cfg.SplitReplenishCapPct = s.SplitReplenishCapPct
		cfg.MakerMergeBufferSeconds = s.MakerMergeBufferSeconds
		cfg.MakerQuoteGap = s.MakerQuoteGap
		cfg.MinAskPrice = s.MinAskPrice
		cfg.MaxAskPrice = s.MaxAskPrice
		cfg.MaxTradeSize = s.MaxTradeSize
		cfg.MaxDailyLoss = s.MaxDailyLoss
		cfg.TakerCloseMarket = s.TakerCloseMarket
		cfg.TakerCloseMarketTime = s.TakerCloseMarketTime
		cfg.TakerCloseMarketSlippage = s.TakerCloseMarketSlippage
		cfg.TakerCloseMarketMinPrice = s.TakerCloseMarketMinPrice
		cfg.TradingHoursMode = s.TradingHoursMode

		// Update the REST client exchange if it changed
		if restClient.Exchange != s.Exchange {
			restClient.Exchange = s.Exchange
		}

		_ = cfg.SaveSettings()
	})
	tui.SetTradeFactor(cfg.TradeScaleFactor)
	tui.SetMode("Paper")

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
		tui.StartRenderLoop(paperbotUIInterval(tui.GetSettings()), stop)
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
				tui.CleanupOrderBooks(5 * time.Minute)
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

		liveSettings := tui.GetSettings()
		arbMode := normalizePaperArbMode(liveSettings.PaperArbMode)
		copytradeTarget := paperbotCopytradeTarget{}

		// Find all available markets (BTC, ETH, SOL, XRP)
		logEvent(tui, csvLogger, engine, "INFO", "SYSTEM", "MARKET_SEARCH", "Searching for active markets based on live settings...")
		var markets map[string]*api.Market
		if arbMode == paperArbModeCopytrade {
			resolveCtx, resolveCancel := context.WithTimeout(ctx, 5*time.Second)
			target, targetErr := paperbotResolveCopytradeTarget(resolveCtx, restClient, liveSettings)
			resolveCancel()
			if targetErr != nil {
				logEvent(tui, csvLogger, engine, "WARN", "SYSTEM", "COPYTRADE_TARGET", "Copytrade target unavailable: %v", targetErr)
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(5 * time.Second):
				}
				continue
			}
			copytradeTarget = target
			tui.LogEvent("🪞 Copytrade target %s → %s", target.Raw, target.Wallet)
			markets = mkt.FindMarkets(ctx, restClient, tui.GetSettings, func(format string, args ...interface{}) {
				tui.LogEvent(format, args...)
			})
		} else {
			markets = mkt.FindMarkets(ctx, restClient, tui.GetSettings, func(format string, args ...interface{}) {
				tui.LogEvent(format, args...)
			})
		}
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
		copytradePoller := (*paperbotCopytradePoller)(nil)
		if arbMode == paperArbModeCopytrade {
			copytradePoller = newPaperbotCopytradePoller(copytradeTarget.Wallet, condIDs)
			if copytradePoller != nil {
				trackedMarkets := make([]*api.Market, 0, len(markets))
				for _, market := range markets {
					if market != nil {
						trackedMarkets = append(trackedMarkets, market)
					}
				}
				chainWSURL := api.ResolvePolygonWSURL(os.Getenv("POLYGON_WS_URL"), "")
				if watcher := api.NewPolymarketMinedWatcher(chainWSURL, polygonClient, restClient, copytradeTarget.Wallet); watcher != nil {
					watcher.PrimeTrackedMarkets(trackedMarkets)
					watcher.Start(ctx, func(format string, args ...interface{}) {
						tui.LogEvent(format, args...)
					})
					copytradePoller.minedWatcher = watcher
					tui.LogEvent("⛓️ Copytrade onchain watcher enabled for %s", copytradeTarget.Wallet)
				}
				pendingWSURL := api.ResolvePolymarketPendingWSURL(os.Getenv("COPYTRADE_PENDING_WS_URL"), "")
				if watcher := api.NewPolymarketPendingWatcher(pendingWSURL, restClient, polygonClient, copytradeTarget.Wallet); watcher != nil {
					watcher.PrimeTrackedMarkets(trackedMarkets)
					watcher.Start(ctx, func(format string, args ...interface{}) {
						tui.LogEvent(format, args...)
					})
					copytradePoller.pendingWatcher = watcher
					tui.LogEvent("🛰️ Copytrade mempool watcher enabled for %s", copytradeTarget.Wallet)
				}
				if !paperbotCopytradeHasOnchainWatcher(copytradePoller) {
					tui.LogEvent("⚠️ Copytrade watchers (mempool/onchain) disabled; using slower REST polling")
				}
			}
		}

		// Track starting equity for compounding calculation
		startingEquity := engine.GetBookEquity()
		roundStartTrades := engine.GetStats().TotalTrades
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
			marketID := mkt.ScopedMarketID(assetID, market)
			// Parse end time and outcomes
			endTime, err := paper.ParseEndTimeFromSlug(market.Slug)
			if err != nil {
				if !market.EndTime.IsZero() {
					endTime = market.EndTime
				} else {
					endTime = time.Now().Add(15 * time.Minute)
				}
			}
			outcomes := mkt.GetOutcomes(market)
			tui.AddMarket(marketID, market.Slug, outcomes, endTime)
			// Reduced logging: Only TUI for startup info
			tui.LogEvent("🚀 Trading %s: %s", marketID, market.Slug)

			trader := createTrader(marketID, market, engine, restClient, tui, outcomes, endTime, csvLogger, cfg, resolutionCache, copytradeTarget, copytradePoller)
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
			}(marketID, trader)
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
		// Read exact number of expected results to avoid hanging if results channel isn't closed due to stuck trader
		for i := 0; i < tradersStarted; i++ {
			select {
			case result, ok := <-results:
				if !ok {
					// Channel was closed early (shouldn't happen but safe to check)
					i = tradersStarted
					continue
				}
				_ = result
			case <-time.After(5 * time.Second):
				tui.LogEvent("⚠️ Timed out waiting for some traders to return results")
				// Force break out of the loop
				i = tradersStarted
			}
		}

		// Log market rotation from the shared engine state rather than summing
		// overlapping per-trader deltas from concurrent goroutines.
		roundPnL, roundEquity, roundTrades, _ := summarizePaperRound(engine, startingEquity, roundStartTrades)
		tui.LogEvent("📊 Round PnL: $%.2f | Total Equity: $%.2f | Trades: %d | Rotating...", roundPnL, roundEquity, roundTrades)

		// Update compounding multiplier based on round performance
		engine.UpdateCompoundMultiplier(roundPnL, startingEquity)
		newMultiplier := engine.GetCompoundMultiplier()
		if roundPnL > 0 {
			tui.LogEvent("📈 PROFIT! Multiplier: %.2fx → %.2fx (compounding)", compoundMultiplier, newMultiplier)
		} else if roundPnL < 0 {
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
		tui.CancelAllOrders()
		engine.ClearMarketData()
		restClient.CloseIdleConnections()
	}
}

func createTrader(id string, market *api.Market, engine *paper.Engine, restClient *api.RestClient, tui *paper.TUI, outcomes []string, endTime time.Time, csvLogger *core.CSVLogger, cfg *core.Config, resCache *api.ResolutionCache, copytradeTarget paperbotCopytradeTarget, copytradePoller *paperbotCopytradePoller) *MarketTrader {
	tokenMap := make(map[string]string)
	for _, token := range market.Tokens {
		tokenMap[token.TokenID] = token.Outcome
	}

	orderBook := paper.NewOrderBook()
	tui.RegisterOrderBook(id, orderBook)

	ladderConfig := paper.LadderConfig{
		Levels:         3,
		SharesPerLevel: 25,
		PriceStep:      0.01,
		BasePrice:      0.0,
	}
	ladderMgr := paper.NewLadderManager(orderBook, ladderConfig)

	riskConfig := paper.RiskConfig{
		DisableKillSwitch:  true,
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

	// Binance Gap tracking components
	polySignalTracker := paper.NewDirectionalSignalTracker(core.ResolveBinanceSignalLookback(cfg), outcomes)
	var binanceFeed *api.BinanceFuturesPriceFeed
	symbol := getPaperBinanceSymbol(id, cfg)
	if symbol != "" {
		binanceFeed = api.NewBinanceFuturesPriceFeed(symbol, core.ResolveBinanceSignalLookback(cfg))
	}

	return &MarketTrader{
		ID:                id,
		Market:            market,
		Engine:            engine,
		OrderBook:         orderBook,
		LadderMgr:         ladderMgr,
		RiskMgr:           riskMgr,
		BinanceFeed:       binanceFeed,
		PolySignalTracker: polySignalTracker,
		Monitor:           monitor,
		TokenMap:          tokenMap,
		Outcomes:          outcomes,
		EndTime:           endTime,
		RestClient:        restClient,
		TUI:               tui,
		CSVLogger:         csvLogger,
		Config:            cfg,
		TokenBids:         make(map[string]float64),
		TokenAsks:         make(map[string]float64),
		TokenFullBids:     make(map[string][]paper.MarketLevel),
		TokenFullAsks:     make(map[string][]paper.MarketLevel),
		FloatPrices:       make(map[string]float64),
		LastUpdate:        time.Now(),
		LastPairUpdate:    time.Now(),
		LastRestPoll:      time.Now(),
		SplitInventory:    splitInv,
		ReplenishCtrl:     paper.NewReplenishController(),
		SplitInitialized:  false,
		LastSplitSell:     time.Time{},
		MakerQuotes:       make(map[string]*paper.LimitOrder),
		ResolutionCache:   resCache,
		CopytradeWallet:   copytradeTarget.Wallet,
		CopytradeLabel:    copytradeTarget.Label,
		CopytradePoller:   copytradePoller,
		CopytradeState:    newPaperbotCopytradeState(),
	}
}

func runTrader(ctx context.Context, t *MarketTrader) (*marketResult, error) {
	// Setup WebSocket with retry
	wsMgr := api.NewWSManager(t.Config.Exchange, t.Config.KalshiAPIKey, t.Config.KalshiPK, "")
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
		"type":                   "market",
		"assets_ids":             assetIDs,
		"custom_feature_enabled": true,
	}
	if err := wsMgr.Subscribe(ctx, sub); err != nil {
		return nil, fmt.Errorf("subscribe failed: %w", err)
	}

	if t.BinanceFeed != nil {
		t.BinanceFeed.Start(ctx)

	}

	// Start WebSocket streaming in background

	wsMsgChan := wsMgr.StartStreaming(ctx)
	t.TUI.LogEvent("[%s] 📡 WebSocket streaming started", t.ID)

	// Order fill callback
	t.OrderBook.SetFillCallback(func(order *paper.LimitOrder, fillQty, fillPrice float64) {
		status := "PARTIAL"
		if order.Status == paper.OrderStatusFilled {
			status = "FILLED"
		}
		executionMode := strings.ToLower(strings.TrimSpace(order.ExecutionMode))
		if executionMode == "" {
			executionMode = "maker"
		}

		switch order.Side {
		case "buy":
			var trade *paper.Trade
			var err error
			if executionMode == "taker-close" {
				trade, err = t.Engine.BuyForMarket(t.ID, order.Outcome, fillPrice, fillQty)
			} else {
				trade, err = t.Engine.MakerBuyForMarket(t.ID, order.Outcome, fillPrice, fillQty)
			}
			if err != nil {
				t.TUI.LogEvent("[%s] ❌ BUY fill error: %v", t.ID, err)
				if t.CSVLogger != nil {
					t.CSVLogger.Log("ERROR", t.ID, "BUY_FILL_ERROR", err.Error(), t.Engine.GetEquity())
				}
				return
			}
			cost := fillQty * fillPrice
			if trade != nil {
				cost = trade.Value
			}
			saved := order.Price - fillPrice
			if executionMode == "taker-close" {
				t.TUI.LogEvent("[%s] ✅ TAKER CLOSE FILL %s %.0f @ $%.3f (saved $%.3f)", t.ID, order.Outcome, fillQty, fillPrice, saved)
			} else {
				t.TUI.LogEvent("[%s] ✅ BUY FILL %s %.0f @ $%.3f (saved $%.3f)", t.ID, order.Outcome, fillQty, fillPrice, saved)
			}
			t.TUI.RecordOrderWithMode(t.ID, order.Outcome, "BUY", fillQty, fillPrice, cost, 0.0, 0.0, executionMode, status)
			if t.CSVLogger != nil {
				t.CSVLogger.Log("TRADE", t.ID, "BUY_FILL", fmt.Sprintf("%s %.0f @ $%.3f", order.Outcome, fillQty, fillPrice), t.Engine.GetEquity())
			}
		case "sell":
			positions := t.Engine.GetPositions()
			pos, ok := getPaperMarketPosition(positions, t.ID, order.Outcome)
			avgCost := 0.0
			if ok {
				avgCost = pos.AvgPrice
			}
			trade, err := t.Engine.MakerSellForMarket(t.ID, order.Outcome, fillPrice, fillQty)
			if err != nil {
				t.TUI.LogEvent("[%s] ❌ SELL fill error: %v", t.ID, err)
				if t.CSVLogger != nil {
					t.CSVLogger.Log("ERROR", t.ID, "SELL_FILL_ERROR", err.Error(), t.Engine.GetEquity())
				}
				return
			}
			proceeds := fillQty * fillPrice
			if trade != nil {
				proceeds = trade.Value
			}
			profit := proceeds - (avgCost * fillQty)
			t.TUI.LogEvent("[%s] ✅ SELL FILL %s %.0f @ $%.3f (pnl $%.2f)", t.ID, order.Outcome, fillQty, fillPrice, profit)
			t.TUI.RecordOrderWithMode(t.ID, order.Outcome, "SELL", fillQty, fillPrice, proceeds, 0.0, profit, executionMode, status)
			if t.CSVLogger != nil {
				t.CSVLogger.Log("TRADE", t.ID, "SELL_FILL", fmt.Sprintf("%s %.0f @ $%.3f | pnl=%.2f", order.Outcome, fillQty, fillPrice, profit), t.Engine.GetEquity())
			}
		}

		if order.Status == paper.OrderStatusFilled || order.Status == paper.OrderStatusCancelled {
			clearPaperMakerQuoteReference(t, order)
		}
		updatePaperPendingOrders(t)
	})

	// Track starting realized PnL
	startingRealizedPnL := t.Engine.GetStats().RealizedPnL
	tradesAtStart := t.Engine.GetStats().TotalTrades

	tokenPrices := make(map[string]string)
	displayBids := make(map[string]float64)
	displayAsks := make(map[string]float64)
	publishedBids := make(map[string]float64)
	publishedAsks := make(map[string]float64)
	quoteState := make(map[string]paperQuoteState)
	lastPublishedQuoteAt := time.Time{}
	lastFillPoll := time.Time{}
	lastReconnectCount := int32(0)    // Track reconnections
	lastWsWarnTime := time.Time{}     // Rate-limit WS warnings
	lastForceReconnect := time.Time{} // Track forced reconnection attempts
	lastTrade := time.Time{}          // Prevent trade spam

	const wsWarnInterval = 10 * time.Second   // Only warn once per 10 seconds
	const wsForceReconnect = 10 * time.Second // Force reconnection after 10 seconds stale
	restFallbackQuoteAge := core.ResolveRestFallbackQuoteAge(t.Config)
	restFallbackPollInterval := core.ResolveRestFallbackPollInterval(t.Config)

	// Track WebSocket channel closure state (outside loop to persist across ticks)
	wsChannelClosed := false
	takerCloseAttempted := false
	var takerCloseTriggerOutcome string
	var takerCloseTriggerTime time.Time
	var lastTakerCloseLog time.Time
	lastResolutionPendingLog := time.Time{}
	usWeekdayGateClosedLogged := false

	mainLoopTicker := time.NewTicker(paperbotTraderLoopInterval(t.TUI.GetSettings()))
	defer mainLoopTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			cancelAllPaperMakerQuotes(t, "trader shutting down")
			t.OrderBook.CancelAllOrders()
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

							feeUsdc := 0.0
							if t.Config.FeeRateBps > 0 {
								feeUsdc = rem * 0.25 * math.Pow(bid*(1.0-bid), 2.0) * bid
							}

							profit := t.SplitInventory.RecordSell(t.ID, out, rem, bid)
							t.Engine.AddRealizedPnL(profit - feeUsdc)
							t.Engine.AddBalance((rem * bid) - feeUsdc)
							t.TUI.LogEvent("[%s] 📉 Sold %.0f split shares of %s at $%.3f", t.ID, rem, out, bid)
						}
					}
				}
			}
			return nil, ctx.Err()

		case <-mainLoopTicker.C:
		}

		// Check safety timeout - force exit if trader runs too long
		if time.Now().After(traderDeadline) {
			logEvent(t.TUI, t.CSVLogger, t.Engine, "WARN", t.ID, "TIMEOUT", "SAFETY TIMEOUT - Forcing market exit")
			cancelAllPaperMakerQuotes(t, "safety timeout")
			t.OrderBook.CancelAllOrders()
			t.LadderMgr.CancelAllLadders()

			winner := t.determineWinner()
			if winner != "" {
				logEvent(t.TUI, t.CSVLogger, t.Engine, "INFO", t.ID, "TIMEOUT_RESOLVE", "Timeout resolution: %s", winner)
				t.Engine.RedeemWithDetails(t.ID, winner)
				if settled := t.Engine.SettlePendingRedemption(t.ID); settled > 0 {
					t.TUI.LogEvent("[%s] 💸 Redeem settled: +$%.2f", t.ID, settled)
				}
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

		liveCfg := t.TUI.GetSettings()
		usNow := core.USTime(time.Now())

		weekdayTradingAllowed := true
		if liveCfg.TradingHoursMode == "weekdays trade only" {
			weekdayTradingAllowed = core.IsUSWeekday(usNow)
		} else if liveCfg.TradingHoursMode == "us open only" {
			weekdayTradingAllowed = core.IsUSMarketOpen(time.Now())
		}

		if !weekdayTradingAllowed {
			if !usWeekdayGateClosedLogged {
				t.TUI.LogEvent("[%s] 🗓️ Trading gate closed at %s - new trades paused", t.ID, usNow.Format("Mon 2006-01-02 15:04:05 MST"))
				usWeekdayGateClosedLogged = true
			}
		} else if usWeekdayGateClosedLogged {
			t.TUI.LogEvent("[%s] ✅ Trading gate open at %s - trading resumed", t.ID, usNow.Format("Mon 2006-01-02 15:04:05 MST"))
			usWeekdayGateClosedLogged = false
		}

		takerCloseMode := paper.TakerCloseModeActive(liveCfg)

		// --- TAKER CLOSE MARKET LOGIC ---
		takerCloseTime := time.Duration(liveCfg.TakerCloseMarketTime) * time.Second
		if weekdayTradingAllowed && takerCloseMode && timeToEnd > 0 && timeToEnd <= takerCloseTime {
			if !takerCloseAttempted {
				t.mu.Lock()
				bestOutcome := ""
				highestPrice := 0.0
				for _, outcome := range t.Outcomes {
					ask := t.TokenAsks[outcome]
					bid := t.TokenBids[outcome]
					price := ask
					if price <= 0 || price >= 1.0 {
						price = bid
					}
					if price > 0 && price <= 1.0 && price > highestPrice {
						highestPrice = price
						bestOutcome = outcome
					}
				}
				t.mu.Unlock()

				minPrice := liveCfg.TakerCloseMarketMinPrice
				if minPrice <= 0 {
					minPrice = 0.60
				}

				if bestOutcome == "" || highestPrice < minPrice {
					takerCloseTriggerOutcome = ""
					takerCloseTriggerTime = time.Time{}
					if time.Since(lastTakerCloseLog) > 5*time.Second {
						t.TUI.LogEvent("[%s] ⏳ Taker close waiting for valid quote (highest: $%.3f, needs >= $%.3f)", t.ID, highestPrice, minPrice)
						lastTakerCloseLog = time.Now()
					}
					continue
				}

				if bestOutcome != "" && highestPrice >= minPrice {
					if takerCloseTriggerOutcome != bestOutcome {
						takerCloseTriggerOutcome = bestOutcome
						takerCloseTriggerTime = time.Now()
					}
					if time.Since(takerCloseTriggerTime) < 1*time.Second {
						if time.Since(lastTakerCloseLog) > 5*time.Second {
							t.TUI.LogEvent("[%s] ⏳ Taker close stabilizing: %s at $%.3f (waiting 1s)", t.ID, bestOutcome, highestPrice)
							lastTakerCloseLog = time.Now()
						}
						continue
					}

					takerCloseAttempted = true
					t.TUI.LogEvent("[%s] ⚡ TAKER CLOSE TRIGGERED: Force buy %s (price: $%.2f)", t.ID, bestOutcome, highestPrice)

					budget := t.Config.CalculateTradeSize(t.Engine.GetSizingBalance())
					// Calculate expected execution price (price + absolute slippage allowance)
					// e.g. price 0.70 + (-0.03) = 0.73
					slippageDec := liveCfg.BuyExecutionMarginFloorPercent
					if slippageDec < 0 {
						slippageDec = -slippageDec // e.g. -0.03 becomes 0.03
					}
					sizingPrice := highestPrice + slippageDec
					if sizingPrice > 0.99 {
						sizingPrice = 0.99
					}
					// Execute base USDC based on the expected sizing price
					size := budget / sizingPrice
					if size < t.Config.MinOrderSize {
						size = t.Config.MinOrderSize
					}

					// But send the absolute max slippage (e.g. 0.99) as the limit price to ensure it fills
					limitPrice := liveCfg.TakerCloseMarketSlippage
					if limitPrice <= 0 || limitPrice >= 1.0 {
						limitPrice = 0.99
					}

					tokenID := ""
					for k, v := range t.TokenMap {
						if v == bestOutcome {
							tokenID = k
							break
						}
					}

					// Paper bot order placement
					tradeCtx, cancelTrade := context.WithTimeout(context.Background(), 5*time.Second)
					go func(tCtx context.Context, target string, sz float64, price float64, tid string) {
						defer cancelTrade()
						order := t.OrderBook.PlaceOrderWithMode(target, "buy", price, sz, 0, "taker-close")
						t.TUI.LogEvent("[%s] ✅ Taker close GTC buy placed for %.0f shares at $%.2f (paper ID: %d)", t.ID, sz, price, order.ID)
					}(tradeCtx, bestOutcome, size, limitPrice, tokenID)
				} else {
					if time.Since(lastTakerCloseLog) > 1*time.Second {
						t.TUI.LogEvent("[%s] ⏳ Taker close waiting: highest price is $%.2f (needs >= $%.2f)", t.ID, highestPrice, minPrice)
						lastTakerCloseLog = time.Now()
					}
				}
			}
		}
		// --------------------------------
		isExpired := timeToEnd <= 0

		if isExpired && !t.MarketEnded {
			winner := t.determineWinner()
			if winner == "" {
				if time.Since(lastResolutionPendingLog) > 1*time.Second {
					logEvent(t.TUI, t.CSVLogger, t.Engine, "INFO", t.ID, "EXPIRED_WAIT", "MARKET EXPIRED - waiting for winner confirmation")
					t.TUI.LogEvent("[%s] ⏳ Market expired - waiting for winner confirmation (on-chain or terminal >= $0.99)", t.ID)
					lastResolutionPendingLog = time.Now()
				}
			} else {
				t.MarketEnded = true
				logEvent(t.TUI, t.CSVLogger, t.Engine, "INFO", t.ID, "EXPIRED", "MARKET EXPIRED - resolving immediately")
				logEvent(t.TUI, t.CSVLogger, t.Engine, "INFO", t.ID, "WINNER", "WINNER: %s", winner)

				// Use detailed redemption
				result := t.Engine.RedeemWithDetails(t.ID, winner)
				if settled := t.Engine.SettlePendingRedemption(t.ID); settled > 0 {
					t.TUI.LogEvent("[%s] 💸 Redeem settled: +$%.2f", t.ID, settled)
				}

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

		// Process queued WebSocket messages, but bound per tick so heavy WS traffic
		// cannot starve timeout/expiry/reconciliation logic.
		messagesProcessed := 0
		wsDrainStartedAt := time.Now()
		const maxWSMessagesPerTick = 300
		const maxWSDrainPerTick = 5 * time.Millisecond
		for {
			if messagesProcessed >= maxWSMessagesPerTick || time.Since(wsDrainStartedAt) >= maxWSDrainPerTick {
				goto doneProcessingWS
			}
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
				// Polymarket CLOB WS sends:
				//   1. Book snapshots ("book") on subscribe/reconnect.
				//   2. Price-change deltas ("price_change") with changed levels and
				//      explicit best_bid / best_ask values.
				//   3. Best-bid-ask updates ("best_bid_ask") when subscribed with
				//      custom_feature_enabled.
				//
				// IMPORTANT: changed levels still update the local depth cache, but
				// explicit best_bid / best_ask fields now take priority for BBO so
				// one-sided book removals do not leave stale top-of-book behind.
				if books, err := api.ParseOrderBooks(msg); err == nil && len(books) > 0 && books[0].AssetID != "" {
					// ── Book snapshot (array) ──────────────────────────────
					foundForThisTrader := false
					for _, b := range books {
						bid, ask := 0.0, 0.0
						for _, order := range b.Bids {
							p, _ := strconv.ParseFloat(order.Price, 64)
							if p > 0 && p <= 1.0 && p > bid {
								bid = p
							}
						}
						for _, order := range b.Asks {
							p, _ := strconv.ParseFloat(order.Price, 64)
							if p > 0 && p <= 1.0 && (ask == 0 || p < ask) {
								ask = p
							}
						}

						outcome := t.TokenMap[b.AssetID]
						if outcome != "" {
							foundForThisTrader = true
							t.mu.Lock()

							// WS Snapshot is absolute state.
							if bid > 0 && ask > 0 && !paperHasSaneTopOfBook(bid, ask) {
								// Reject crossed snapshot and clear state
								t.TokenBids[outcome] = 0
								t.TokenAsks[outcome] = 0
								t.TokenFullBids[outcome] = nil
								t.TokenFullAsks[outcome] = nil
								t.mu.Unlock()
								continue
							}

							t.TokenBids[outcome] = bid
							t.TokenAsks[outcome] = ask

							if bid > 0 && ask > 0 {
								mid := (bid + ask) / 2
								t.FloatPrices[outcome] = mid
								tokenPrices[outcome] = fmt.Sprintf("%.3f", mid)
								t.Engine.UpdateMarketData(t.ID, outcome, mid, bid, ask)
							}
							// Always update full depth from snapshots.
							t.TokenFullBids[outcome] = mkt.LevelsToPriceDepth(b.Bids, true)
							t.TokenFullAsks[outcome] = mkt.LevelsToPriceDepth(b.Asks, false)
							quoteState[outcome] = paperQuoteState{UpdatedAt: time.Now(), Source: "ws"}
							t.mu.Unlock()
						}
					}
					if foundForThisTrader {
						now := time.Now()
						t.mu.Lock()
						t.LastUpdate = now
						syncPaperPairUpdate(t, now)
						t.mu.Unlock()
					}
				} else if update, err := api.ParsePriceUpdate(msg); err == nil && len(update.PriceChanges) > 0 {
					// ── Price-change delta ─────────────────────────────────
					foundForThisTrader := false
					touchedOutcomes := make(map[string]bool)
					type explicitTopOfBook struct {
						bid    float64
						ask    float64
						hasBid bool
						hasAsk bool
					}
					explicitTopByOutcome := make(map[string]explicitTopOfBook)

					t.mu.Lock()
					for _, pc := range update.PriceChanges {
						outcome := t.TokenMap[pc.AssetID]
						if outcome == "" {
							continue
						}
						foundForThisTrader = true
						touchedOutcomes[outcome] = true
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

						top := explicitTopByOutcome[outcome]
						if bestBid, ok := parseWSQuotedPrice(pc.BestBid); ok {
							top.bid = bestBid
							top.hasBid = true
						}
						if bestAsk, ok := parseWSQuotedPrice(pc.BestAsk); ok {
							top.ask = bestAsk
							top.hasAsk = true
						}
						if top.hasBid || top.hasAsk {
							explicitTopByOutcome[outcome] = top
						}
					}

					for _, outcome := range t.Outcomes {
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

						if top, ok := explicitTopByOutcome[outcome]; ok {
							if top.hasBid {
								t.TokenBids[outcome] = top.bid
							}
							if top.hasAsk {
								t.TokenAsks[outcome] = top.ask
							}
						}

						if t.TokenBids[outcome] > 0 && t.TokenAsks[outcome] > 0 {
							if !paperHasSaneTopOfBook(t.TokenBids[outcome], t.TokenAsks[outcome]) {
								t.LastUpdate = time.Now().Add(-20 * time.Second)
								t.TokenBids[outcome] = 0
								t.TokenAsks[outcome] = 0
								t.TokenFullBids[outcome] = nil
								t.TokenFullAsks[outcome] = nil
								continue
							}

							mid := (t.TokenBids[outcome] + t.TokenAsks[outcome]) / 2
							t.FloatPrices[outcome] = mid
							tokenPrices[outcome] = fmt.Sprintf("%.3f", mid)
							t.Engine.UpdateMarketData(t.ID, outcome, mid, t.TokenBids[outcome], t.TokenAsks[outcome])
						}
					}
					now := time.Now()
					if foundForThisTrader {
						t.LastUpdate = now
						syncPaperPairUpdate(t, now)
						for outcome := range touchedOutcomes {
							quoteState[outcome] = paperQuoteState{UpdatedAt: now, Source: "ws"}
						}
					}
					t.mu.Unlock()
				} else if bbo, err := api.ParseBestBidAsk(msg); err == nil && strings.EqualFold(strings.TrimSpace(bbo.EventType), "best_bid_ask") && bbo.AssetID != "" {
					outcome := t.TokenMap[bbo.AssetID]
					if outcome != "" {
						t.mu.Lock()
						now := time.Now()
						t.LastUpdate = now
						if bestBid, ok := parseWSQuotedPrice(bbo.BestBid); ok {
							t.TokenBids[outcome] = bestBid
						}
						if bestAsk, ok := parseWSQuotedPrice(bbo.BestAsk); ok {
							t.TokenAsks[outcome] = bestAsk
						}
						if t.TokenBids[outcome] > 0 && t.TokenAsks[outcome] > 0 {
							if !paperHasSaneTopOfBook(t.TokenBids[outcome], t.TokenAsks[outcome]) {
								t.TokenBids[outcome] = 0
								t.TokenAsks[outcome] = 0
							} else {
								mid := (t.TokenBids[outcome] + t.TokenAsks[outcome]) / 2
								t.FloatPrices[outcome] = mid
								tokenPrices[outcome] = fmt.Sprintf("%.3f", mid)
								t.Engine.UpdateMarketData(t.ID, outcome, mid, t.TokenBids[outcome], t.TokenAsks[outcome])
							}
						}
						quoteState[outcome] = paperQuoteState{UpdatedAt: now, Source: "ws-bbo"}
						syncPaperPairUpdate(t, now)
						t.mu.Unlock()
					}
				} else if book, err := api.ParseOrderBook(msg); err == nil && book.AssetID != "" {
					// ── Book snapshot (single object) ──────────────────────
					bid, ask := 0.0, 0.0
					for _, b := range book.Bids {
						p, _ := strconv.ParseFloat(b.Price, 64)
						if p > 0 && p <= 1.0 && p > bid {
							bid = p
						}
					}
					for _, order := range book.Asks {
						p, _ := strconv.ParseFloat(order.Price, 64)
						if p > 0 && p <= 1.0 && (ask == 0 || p < ask) {
							ask = p
						}
					}

					if bid > 0 && ask > 0 && !paperHasSaneTopOfBook(bid, ask) {
						continue // Reject crossed snapshot
					}

					outcome := t.TokenMap[book.AssetID]
					if outcome != "" {
						t.mu.Lock()
						now := time.Now()
						t.LastUpdate = now
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
						t.TokenFullBids[outcome] = mkt.LevelsToPriceDepth(book.Bids, true)
						t.TokenFullAsks[outcome] = mkt.LevelsToPriceDepth(book.Asks, false)
						quoteState[outcome] = paperQuoteState{UpdatedAt: now, Source: "ws"}
						syncPaperPairUpdate(t, now)
						t.mu.Unlock()
					}
				}
			default:
				// No more messages in channel, continue with rest of loop
				goto doneProcessingWS
			}
		}
	doneProcessingWS:

		// Final safety check: scrub invalid books that survived WS processing.
		t.mu.Lock()
		for outcome := range t.TokenMap {
			if t.TokenBids[outcome] > 0 && t.TokenAsks[outcome] > 0 && !paperHasSaneTopOfBook(t.TokenBids[outcome], t.TokenAsks[outcome]) {
				t.TokenBids[outcome] = 0
				t.TokenAsks[outcome] = 0
				t.TokenFullBids[outcome] = nil
				t.TokenFullAsks[outcome] = nil
				t.LastUpdate = time.Now().Add(-20 * time.Second)
			}
		}
		if paperShouldClearLocalPairQuotes(t.Outcomes, t.TokenBids, t.TokenAsks) {
			for _, outcome := range t.Outcomes {
				t.TokenBids[outcome] = 0
				t.TokenAsks[outcome] = 0
				t.TokenFullBids[outcome] = nil
				t.TokenFullAsks[outcome] = nil
			}
		}
		t.mu.Unlock()

		// Track feed age in the UI, but do not treat quiet prices as a broken socket.
		wsConnected := wsMgr.IsConnected()
		wsLastMsg := wsMgr.TimeSinceLastDataMessage()

		// Update WS staleness and ping latency in TUI
		t.TUI.UpdateWSLatency(wsLastMsg)
		t.TUI.UpdateWSPingLatency(wsMgr.PingLatency())

		now := time.Now()
		pairQuoteAge := paperPairQuoteAge(t.LastPairUpdate, now)
		localQuoteMaxAge := core.ResolveExecutionLocalQuoteMaxAge(t.Config)
		executionQuoteMaxAge := paperExecutionQuoteGuardAge(localQuoteMaxAge)
		localPairFresh := shouldUseLocalPaperPair(t.Outcomes, t.TokenBids, t.TokenAsks, t.LastPairUpdate, localQuoteMaxAge, now)
		executionPairFresh := shouldUseLocalPaperPair(t.Outcomes, t.TokenBids, t.TokenAsks, t.LastPairUpdate, executionQuoteMaxAge, now)

		terminalBookState := paperLooksLikeTerminalBook(t.Outcomes, t.TokenBids, t.TokenAsks)
		needsWSReconnect := shouldPaperReconnectWS(t.Outcomes, t.TokenBids, t.TokenAsks, pairQuoteAge, restFallbackQuoteAge, terminalBookState)
		shouldRestFallback := !terminalBookState &&
			!localPairFresh &&
			pairQuoteAge > restFallbackQuoteAge &&
			time.Since(t.LastRestPoll) >= restFallbackPollInterval

		if wsConnected && !wsChannelClosed && needsWSReconnect {
			if time.Since(lastForceReconnect) > wsForceReconnect {
				lastForceReconnect = time.Now()
				wsMgr.ForceReconnect()
				if time.Since(lastWsWarnTime) > wsWarnInterval {
					t.TUI.LogEvent("[%s] 🔄 WS local book invalid - reconnecting...", t.ID)
					lastWsWarnTime = time.Now()
				}
			}
		}

		if !wsMgr.IsConnected() && !wsChannelClosed {
			if time.Since(lastForceReconnect) > wsForceReconnect {
				lastForceReconnect = time.Now()
				wsMgr.ForceReconnect()
				if time.Since(lastWsWarnTime) > wsWarnInterval {
					t.TUI.LogEvent("[%s] 🔌 WS disconnected - reconnecting...", t.ID)
					lastWsWarnTime = time.Now()
				}
			}
		}

		if wsChannelClosed && time.Since(lastWsWarnTime) > wsWarnInterval {
			t.TUI.LogEvent("[%s] ⚠️ WebSocket closed - attempting reconnect", t.ID)
			lastWsWarnTime = time.Now()
			wsMgr.ForceReconnect()
		}

		restRecovered := false
		if shouldRestFallback {
			wasFallbackActive := t.RestFallbackActive
			t.RestFallbackActive = true
			recovered := t.handleRestFallback(ctx, tokenPrices, pairQuoteAge, quoteState, wasFallbackActive && !t.RestRecoveryLogged)
			restRecovered = recovered
			if recovered {
				t.RestFallbackActive = false
				t.RestRecoveryLogged = false
			} else if pairQuoteAge >= 10*time.Second {
				t.RestRecoveryLogged = true
			}
		} else {
			t.RestFallbackActive = false
			t.RestRecoveryLogged = false
		}

		displayUpdated := paperSyncDisplayQuotes(t.Outcomes, t.TokenBids, t.TokenAsks, displayBids, displayAsks, shouldRestFallback)
		quotesChanged := !paperQuoteMapsEqual(t.Outcomes, displayBids, displayAsks, publishedBids, publishedAsks)
		latestQuoteAt, latestQuoteSource := paperLatestQuoteUpdate(t.Outcomes, quoteState)
		displayUsable := paperDisplayHasUsableQuotes(t.Outcomes, displayBids, displayAsks)
		freshnessAdvanced := displayUsable && !latestQuoteAt.IsZero() && latestQuoteAt.After(lastPublishedQuoteAt)
		if displayUpdated || quotesChanged || freshnessAdvanced {
			t.TUI.UpdateMarketPricesWithSourceAt(t.ID, displayBids, displayAsks, paperNormalizeDisplaySource(latestQuoteSource), latestQuoteAt)
			paperStorePublishedQuotes(t.Outcomes, displayBids, displayAsks, publishedBids, publishedAsks)
			if freshnessAdvanced {
				lastPublishedQuoteAt = latestQuoteAt
			}
		}

		bookChanged := messagesProcessed > 0 || restRecovered
		if bookChanged {
			bidDepth := make(map[string][]paper.MarketLevel)
			askDepth := make(map[string][]paper.MarketLevel)
			t.mu.Lock()
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
		}

		shouldPollOrderFills := bookChanged || time.Since(lastFillPoll) >= paperFillPollInterval
		if shouldPollOrderFills {
			lastFillPoll = time.Now()
			if len(t.OrderBook.GetOpenOrders()) > 0 {
				t.mu.Lock()
				for _, outcome := range t.Outcomes {
					bids := t.TokenFullBids[outcome]
					asks := t.TokenFullAsks[outcome]
					if len(bids) == 0 && len(asks) == 0 {
						continue
					}
					bidsCopy := append([]paper.MarketLevel(nil), bids...)
					asksCopy := append([]paper.MarketLevel(nil), asks...)
					t.OrderBook.ProcessPriceUpdate(outcome, bidsCopy, asksCopy)
				}
				if len(t.Outcomes) == 2 {
					oppBids0 := append([]paper.MarketLevel(nil), t.TokenFullBids[t.Outcomes[1]]...)
					oppBids1 := append([]paper.MarketLevel(nil), t.TokenFullBids[t.Outcomes[0]]...)
					t.OrderBook.ProcessComplementaryBuyUpdate(t.Outcomes[0], oppBids0)
					t.OrderBook.ProcessComplementaryBuyUpdate(t.Outcomes[1], oppBids1)
				}
				t.mu.Unlock()
			}
		}

		// Check if market has ended (only exit condition that matters)
		// DON'T exit on "liquidity dried up" - volatile markets can have extreme prices
		// and that's normal market behavior, not a reason to exit

		// Trading logic - check every tick for arbitrage opportunities
		liveCfg = t.TUI.GetSettings()
		arbMode := normalizePaperArbMode(liveCfg.PaperArbMode)
		takerCloseMode = paper.TakerCloseModeActive(liveCfg)
		localQuoteMaxAge = core.ResolveExecutionLocalQuoteMaxAge(t.Config)
		executionQuoteMaxAge = paperExecutionQuoteGuardAge(localQuoteMaxAge)
		now = time.Now()
		localPairFresh = shouldUseLocalPaperPair(t.Outcomes, t.TokenBids, t.TokenAsks, t.LastPairUpdate, localQuoteMaxAge, now)
		executionPairFresh = shouldUseLocalPaperPair(t.Outcomes, t.TokenBids, t.TokenAsks, t.LastPairUpdate, executionQuoteMaxAge, now)
		if !weekdayTradingAllowed {
			cancelAllPaperMakerQuotes(t, "trading gate closed")
		} else if takerCloseMode {
			cancelAllPaperMakerQuotes(t, "taker close market enabled")
		} else if arbMode != paperArbModeMaker {
			cancelAllPaperMakerQuotes(t, "maker mode disabled")
		} else if marketState != paper.MarketStateActive || len(tokenPrices) != 2 || len(t.Outcomes) != 2 {
			cancelAllPaperMakerQuotes(t, "market not active for maker quoting")
		}
		if arbMode == paperArbModeCopytrade && marketState == paper.MarketStateActive && len(t.Outcomes) > 0 {
			if !weekdayTradingAllowed || takerCloseMode {
				continue
			}
			paperbotHandleCopytradeMarket(ctx, t, liveCfg)
			continue
		}
		if len(tokenPrices) == 2 && len(t.Outcomes) == 2 && marketState == paper.MarketStateActive {
			if !weekdayTradingAllowed {
				continue
			}

			// Skip normal trading completely if TakerCloseMarket is enabled
			if takerCloseMode {
				continue
			}

			if !executionPairFresh {
				if arbMode == paperArbModeMaker {
					cancelAllPaperMakerQuotes(t, "waiting for fresh pair quotes")
				}
				continue
			}
			if arbMode == paperArbModeBinanceGap {
				// Run Binance gap mode
				paperbotHandleBinanceGapMarket(ctx, t, liveCfg, t.Config)
				continue
			}
			if arbMode == paperArbModeMaker {
				if killSwitchActive {
					cancelAllPaperMakerQuotes(t, "risk pause active")
					continue
				}
				maintainPaperMakerInventoryQuotes(t, time.Now())
				continue
			}
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
				currentCash := t.Engine.GetBalance()
				tradeSize := t.Config.CalculateTradeSize(t.Engine.GetSizingBalance())
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

				if time.Since(lastTrade) <= 2*time.Second {
					// Cooldown - don't spam logs, just skip silently
					continue
				}

				if margin >= minMarginPercent-1e-4 && t.RiskMgr.CanPlaceOrder(baseSharesPerTrade*(ask1+ask2)) {
					baseShares := baseSharesPerTrade

					// AGGREGATED LIQUIDITY: Calculate total matched liquidity across ALL price levels
					// that maintain minimum margin. This allows "chasing" liquidity deeper into the book.
					maxSum := 1.0 - (minMarginPercent / 100.0) // e.g., 2% margin → max sum = 0.98

					// Copy and sort asks by price ascending for both outcomes
					asks1 := make([]paper.MarketLevel, len(t.TokenFullAsks[t.Outcomes[0]]))
					copy(asks1, t.TokenFullAsks[t.Outcomes[0]])
					// Inject BBO if missing due to orderbook lag
					hasAsk1 := false
					for _, a := range asks1 {
						if a.Price <= ask1+1e-6 {
							hasAsk1 = true
							break
						}
					}
					if !hasAsk1 {
						asks1 = append(asks1, paper.MarketLevel{Price: ask1, Size: baseShares})
					}
					sort.Slice(asks1, func(i, j int) bool { return asks1[i].Price < asks1[j].Price })

					asks2 := make([]paper.MarketLevel, len(t.TokenFullAsks[t.Outcomes[1]]))
					copy(asks2, t.TokenFullAsks[t.Outcomes[1]])
					hasAsk2 := false
					for _, a := range asks2 {
						if a.Price <= ask2+1e-6 {
							hasAsk2 = true
							break
						}
					}
					if !hasAsk2 {
						asks2 = append(asks2, paper.MarketLevel{Price: ask2, Size: baseShares})
					}
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
					shares = shares * compoundMult

					// Force at least 1 share if there's any matched liquidity and we have budget
					if shares < 1.0 && minLiquidity >= 1.0 {
						shares = 1.0
					}

					// FINAL LIQUIDITY CAP: Ensure shares never exceed available matched liquidity
					// This must be checked AFTER all scaling (margin scaling + compounding)
					if shares > maxSafeShares {
						shares = maxSafeShares
					}

					// --- PRE-CALCULATE ACTUAL COST BY WALKING THE BOOK ---
					// Instead of assuming all shares fill at the top-of-book price (ask1/ask2),
					// we simulate walking the orderbook to find the TRUE cost of this trade.
					trueCost := 0.0

					// Helper to calculate cost for one side
					calcSideCost := func(qty float64, asks []paper.MarketLevel) float64 {
						c := 0.0
						rem := qty
						for _, lv := range asks {
							if lv.Size <= 0 {
								continue
							}
							take := math.Min(rem, lv.Size)
							c += take * lv.Price
							rem -= take
							if rem <= 0.0001 {
								break
							}
						}
						return c
					}

					trueCost1 := calcSideCost(shares, asks1)
					trueCost2 := calcSideCost(shares, asks2)
					trueCost = trueCost1 + trueCost2

					// If the true cost exceeds our cash, scale DOWN the shares exactly
					// to what we can afford, ensuring we never hit the "Insufficient balance" error.
					if trueCost > currentCash {
						// If we can't afford it, scale the shares down proportionally
						scaleFactor := currentCash / trueCost
						shares = shares * scaleFactor

						// Recalculate true cost with the new, smaller share size
						trueCost1 = calcSideCost(shares, asks1)
						trueCost2 = calcSideCost(shares, asks2)
						trueCost = trueCost1 + trueCost2
					}

					// Calculate true net profit based on actual curve fees and instant merge logic
					feeRateBps := t.Config.FeeRateBps
					calcNetProfit := func(s, tc1, tc2, tc float64) float64 {
						if s <= 0 {
							return 0
						}
						avgP1 := tc1 / s
						avgP2 := tc2 / s
						feeT1, feeT2 := 0.0, 0.0
						if feeRateBps > 0 {
							feeT1 = s * 0.25 * math.Pow(avgP1*(1.0-avgP1), 2.0)
							feeT2 = s * 0.25 * math.Pow(avgP2*(1.0-avgP2), 2.0)
						}
						return math.Min(s-feeT1, s-feeT2) - tc
					}

					netProfit := calcNetProfit(shares, trueCost1, trueCost2, trueCost)
					cost := trueCost

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

						trueCost1 = calcSideCost(shares, asks1)
						trueCost2 = calcSideCost(shares, asks2)
						trueCost = trueCost1 + trueCost2
						cost = trueCost

						netProfit = calcNetProfit(shares, trueCost1, trueCost2, trueCost)

						// If still over risk limit or over cash, don't trade
						if !t.RiskMgr.CanPlaceOrder(cost) || cost > currentCash {
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
					latency := paperExecutionLatency{
						detectedAt:  time.Now(),
						opportunity: "paper buy-arb",
						marketID:    t.ID,
						shares:      shares,
						marginPct:   margin,
						expectedPnL: netProfit,
					}

					if t.CSVLogger != nil {
						t.CSVLogger.Log("TRADE", t.ID, "ARB_ENTRY", fmt.Sprintf("Sum: %.3f, Shares: %.0f, Margin: %.1f%%", sum, shares, margin), t.Engine.GetEquity())
					}

					// FORCE FILL: Use MarketBuy to "walk the book" and guarantee fills
					// This ensures both sides fill completely, avoiding legging risk
					// Use the previously generated asks1 and asks2 slices which ALREADY contain
					// the injected BBO if it was missing due to orderbook lag.
					freshAsks1 := make([]paper.MarketLevel, len(asks1))
					copy(freshAsks1, asks1)

					freshAsks2 := make([]paper.MarketLevel, len(asks2))
					copy(freshAsks2, asks2)

					// Execute market orders that consume liquidity across multiple levels
					// Force fill: walks the book atomically to guarantee execution without legging
					latency.startedAt = time.Now()
					trade1, trade2, avgPrice1, avgPrice2, err := t.Engine.MarketBuyArb(t.ID, t.Outcomes[0], t.Outcomes[1], shares, freshAsks1, freshAsks2)
					if err != nil {
						// Concurrency edge case: another market consumed the cash between our check and execution.
						// Fail gracefully without recording bogus $0 trades.
						t.TUI.LogEvent("[%s] ⚠️ Trade failed during execution (TOCTOU / Insufficient balance): %v", t.ID, err)
						continue
					}
					latency.executedAt = time.Now()
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
					t.TUI.RecordOrder(t.ID, t.Outcomes[0], "BUY", filled1, avgPrice1, actualCost1, margin, 0.0, "FILLED")
					t.TUI.RecordOrder(t.ID, t.Outcomes[1], "BUY", filled2, avgPrice2, actualCost2, margin, 0.0, "FILLED")

					// INSTANT MERGE: Immediately merge to realize profit
					// This matches realbot behavior and ensures round PnL is accurate
					minFilled := filled1
					if filled2 < minFilled {
						minFilled = filled2
					}
					if minFilled > 0 {
						result := t.Engine.MergeForMarket(t.ID, t.Outcomes[0], t.Outcomes[1], minFilled)
						latency.settledAt = time.Now()
						if result.PnL != 0 {
							t.TUI.LogEvent("[%s] 💰 MERGED! +$%.2f profit", t.ID, result.PnL)
						}
					}
					logPaperExecutionLatency(t, latency)

					lastTrade = time.Now()
					t.LaddersPlaced = true
				}
			}
		}

		// ═══════════════════════════════════════════════════════════════════════════
		// SPLIT STRATEGY SIMULATION: Sell when bid_sum > $1.00 + margin
		// This simulates the panic sell strategy without real blockchain calls
		// ═══════════════════════════════════════════════════════════════════════════
		if len(t.Outcomes) == 2 && marketState == paper.MarketStateActive && liveCfg.SplitStrategyEnabled && localPairFresh && weekdayTradingAllowed {
			bid1 := t.TokenBids[t.Outcomes[0]]
			bid2 := t.TokenBids[t.Outcomes[1]]
			currentBookEquity := t.Engine.GetBookEquity()

			// Initial split: create simulated inventory
			// Split is always safe - can merge back to USDC anytime at 1:1
			if !t.SplitInitialized {
				baseTradeSize := t.Config.CalculateTradeSize(t.Engine.GetSizingBalance())
				initialBuffer := baseTradeSize * 2.0
				if initialBuffer < MinSplitBuffer {
					initialBuffer = MinSplitBuffer
				}
				maxInitial := currentBookEquity * t.Config.SplitInitialCapPct
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
			if bid1 >= liveCfg.MinAskPrice && bid2 >= liveCfg.MinAskPrice && bid1 <= liveCfg.MaxAskPrice && bid2 <= liveCfg.MaxAskPrice {
				bidSum := bid1 + bid2
				sellMargin := (bidSum - 1.0) * 100

				// Background replenishment check
				baseTradeSize := t.Config.CalculateTradeSize(t.Engine.GetSizingBalance())
				targetBuffer := baseTradeSize * t.Config.MaxAggressionMultiplier
				currentShares := t.SplitInventory.GetMinSplitShares(t.ID, t.Outcomes[0], t.Outcomes[1])
				replenishAmount := baseTradeSize * 2.0

				decision := t.ReplenishCtrl.CheckReplenish(paper.ReplenishParams{
					CurrentShares:      currentShares,
					TargetBuffer:       targetBuffer,
					InitialShares:      t.InitialSplitAmount, // Replenish back to initial amount immediately
					SellMargin:         sellMargin,
					MinMarginThreshold: t.Config.SplitMinMarginSell - 1.0,
					CurrentBalance:     currentBookEquity,
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
				if sellMargin >= t.Config.SplitMinMarginSell-1e-4 && time.Since(t.LastSplitSell) > 2*time.Second {
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
						// Inject BBO if missing due to orderbook lag to prevent liq: 0/0
						hasBid1 := false
						for _, b := range sortedBids1 {
							if b.Price >= bid1-1e-6 {
								hasBid1 = true
								break
							}
						}
						if !hasBid1 {
							sortedBids1 = append(sortedBids1, paper.MarketLevel{Price: bid1, Size: sharesToSell})
						}
						sort.Slice(sortedBids1, func(a, b int) bool { return sortedBids1[a].Price > sortedBids1[b].Price })

						sortedBids2 := make([]paper.MarketLevel, len(bids2))
						copy(sortedBids2, bids2)
						hasBid2 := false
						for _, b := range sortedBids2 {
							if b.Price >= bid2-1e-6 {
								hasBid2 = true
								break
							}
						}
						if !hasBid2 {
							sortedBids2 = append(sortedBids2, paper.MarketLevel{Price: bid2, Size: sharesToSell})
						}
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

						// Calculate true proceeds by walking the bids
						calcSideProceeds := func(reqShares float64, bids []paper.MarketLevel) float64 {
							rem := reqShares
							p := 0.0
							for _, lvl := range bids {
								if rem <= 0 {
									break
								}
								fill := math.Min(rem, lvl.Size)
								p += fill * lvl.Price
								rem -= fill
							}
							if rem > 0 && len(bids) > 0 {
								p += rem * bids[0].Price
							}
							return p
						}

						trueProceeds1 := calcSideProceeds(sharesToSell, sortedBids1)
						trueProceeds2 := calcSideProceeds(sharesToSell, sortedBids2)

						avgBid1 := trueProceeds1 / sharesToSell
						avgBid2 := trueProceeds2 / sharesToSell

						// Calculate fees (collected in USDC for SELL)
						feeUsdc := 0.0
						if t.Config.FeeRateBps > 0 {
							fee1 := sharesToSell * 0.25 * math.Pow(avgBid1*(1.0-avgBid1), 2.0) * avgBid1
							fee2 := sharesToSell * 0.25 * math.Pow(avgBid2*(1.0-avgBid2), 2.0) * avgBid2
							feeUsdc = fee1 + fee2
						}

						// Ensure it's actually profitable after depth slippage and fees
						// Cost basis of a split share is exactly $1.00 for the pair (1 YES + 1 NO)
						expectedProfit := (trueProceeds1 + trueProceeds2) - feeUsdc - (sharesToSell * 1.0)
						if expectedProfit <= 0 {
							continue
						}
						latency := paperExecutionLatency{
							detectedAt:  time.Now(),
							opportunity: "paper split-sell",
							marketID:    t.ID,
							shares:      sharesToSell,
							marginPct:   sellMargin,
							expectedPnL: expectedProfit,
						}

						// Simulate sell: record profit using actual average fill prices
						latency.startedAt = time.Now()
						profit1 := t.SplitInventory.RecordSell(t.ID, t.Outcomes[0], sharesToSell, avgBid1)
						profit2 := t.SplitInventory.RecordSell(t.ID, t.Outcomes[1], sharesToSell, avgBid2)
						latency.executedAt = time.Now()
						totalProfit := profit1 + profit2 - feeUsdc

						// Divide the fee roughly proportionally to adjust the leg-level profit for display
						netProfit1 := profit1 - (feeUsdc / 2.0)
						netProfit2 := profit2 - (feeUsdc / 2.0)

						// Add proceeds back to balance
						proceeds := (trueProceeds1 + trueProceeds2) - feeUsdc
						t.Engine.AddBalance(proceeds)
						t.Engine.AddRealizedPnL(totalProfit)
						t.Engine.RecalculateDrawdown() // Safe to check drawdown now
						latency.settledAt = time.Now()

						// Enhanced log with liquidity and depth info (same format as ARB buy)
						t.TUI.LogEvent("[%s] 📈 SPLIT SELL! %s@$%.2f + %s@$%.2f = $%.3f (%.1f%%) | %.0f shares, profit $%.2f [liq: %.0f/%.0f, levels used: %d/%d (total depth: %d/%d)]",
							t.ID, t.Outcomes[0], bid1, t.Outcomes[1], bid2, bidSum, sellMargin, sharesToSell, totalProfit,
							rawLiq1, rawLiq2, maxValidI, maxValidJ, bookDepth1, bookDepth2)
						t.TUI.RecordOrder(t.ID, t.Outcomes[0], "SELL", sharesToSell, avgBid1, sharesToSell*avgBid1, sellMargin, netProfit1, "FILLED")
						t.TUI.RecordOrder(t.ID, t.Outcomes[1], "SELL", sharesToSell, avgBid2, sharesToSell*avgBid2, sellMargin, netProfit2, "FILLED")
						logPaperExecutionLatency(t, latency)
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
					t.Engine.AddBalance(merged)    // $1 per merged pair
					t.Engine.RecalculateDrawdown() // Safe to check drawdown now
					t.TUI.LogEvent("[%s] 💰 SPLIT MERGE (sim): Merged %.0f shares → $%.2f", t.ID, merged, merged)
				}
			}
		}
	}
}

// determineWinner uses authoritative resolution when available, otherwise
// waits for a terminal post-expiry price signal (>= $0.99) before settling.
func (t *MarketTrader) determineWinner() string {
	if len(t.Outcomes) == 0 {
		return ""
	}
	now := time.Now()

	resolveByCondition := func(conditionID string, outcomes []string, marketEndTime time.Time) (winner string, checked bool) {
		conditionID = strings.TrimSpace(conditionID)
		if t.ResolutionCache == nil || conditionID == "" {
			return "", false
		}

		if t.nextResolutionRefreshAt.IsZero() || !now.Before(t.nextResolutionRefreshAt) {
			t.ResolutionCache.ForceRefresh(conditionID)
			t.nextResolutionRefreshAt = now.Add(paperResolutionRefreshInterval)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		status := t.ResolutionCache.GetResolution(ctx, conditionID, outcomes, marketEndTime)
		if status.Error != nil {
			errMsg := status.Error.Error()
			if errMsg != t.lastResolutionError || t.lastResolutionErrorLogAt.IsZero() || now.Sub(t.lastResolutionErrorLogAt) >= paperResolutionErrorLogGap {
				t.TUI.LogEvent("[%s] ⚠️ Resolution check error: %v (falling back to price estimate)", t.ID, status.Error)
				t.lastResolutionError = errMsg
				t.lastResolutionErrorLogAt = now
			}
		}
		if status.Resolved && status.Winner != "" {
			return status.Winner, true
		}
		return "", true
	}

	selectHistoricalMarket := func(markets []api.Market) *api.Market {
		if len(markets) == 0 {
			return nil
		}

		if t.Market != nil {
			conditionID := strings.TrimSpace(t.Market.ConditionID)
			if conditionID != "" {
				for i := range markets {
					if strings.TrimSpace(markets[i].ConditionID) == conditionID {
						return &markets[i]
					}
				}
			}
		}

		if len(t.Outcomes) > 0 {
			expectedOutcomes := make(map[string]struct{}, len(t.Outcomes))
			for _, outcome := range t.Outcomes {
				expectedOutcomes[strings.ToLower(strings.TrimSpace(outcome))] = struct{}{}
			}
			for i := range markets {
				matched := 0
				for _, token := range markets[i].Tokens {
					if _, ok := expectedOutcomes[strings.ToLower(strings.TrimSpace(token.Outcome))]; ok {
						matched++
					}
				}
				if matched == len(expectedOutcomes) {
					return &markets[i]
				}
			}
		}

		return &markets[0]
	}

	lookupHistoricalMarket := func(ctx context.Context) (*api.Market, error) {
		if t.Market == nil {
			return nil, fmt.Errorf("missing market metadata")
		}

		slugs := make([]string, 0, 2)
		if marketSlug := strings.TrimSpace(t.Market.MarketSlug); marketSlug != "" {
			slugs = append(slugs, marketSlug)
		}
		if slug := strings.TrimSpace(t.Market.Slug); slug != "" {
			duplicate := false
			for _, s := range slugs {
				if strings.EqualFold(s, slug) {
					duplicate = true
					break
				}
			}
			if !duplicate {
				slugs = append(slugs, slug)
			}
		}

		if len(slugs) == 0 {
			return nil, fmt.Errorf("missing market slug")
		}

		var lastErr error
		for _, slug := range slugs {
			historyMarket, err := t.RestClient.GetMarket(ctx, slug)
			if err == nil {
				return historyMarket, nil
			}
			lastErr = err

			if !strings.Contains(err.Error(), "status 404") {
				continue
			}

			marketsByEvent, eventErr := t.RestClient.GetMarketsByEventSlug(ctx, slug)
			if eventErr != nil {
				lastErr = fmt.Errorf("%v; event lookup failed: %v", err, eventErr)
				continue
			}

			selected := selectHistoricalMarket(marketsByEvent)
			if selected != nil {
				return selected, nil
			}
		}

		if lastErr == nil {
			lastErr = fmt.Errorf("historical market lookup returned no matches")
		}
		return nil, lastErr
	}

	if t.Market != nil {
		if winner, checked := resolveByCondition(t.Market.ConditionID, t.Outcomes, t.EndTime); checked && winner != "" {
			t.TUI.LogEvent("[%s] 🔗 Resolution: %s", t.ID, winner)
			return winner
		}
	}

	if t.RestClient != nil && t.Market != nil && (t.nextHistoricalLookupAt.IsZero() || !now.Before(t.nextHistoricalLookupAt)) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		historyMarket, err := lookupHistoricalMarket(ctx)
		cancel()

		if err != nil {
			errMsg := err.Error()
			retryAfter := paperHistoricalLookupInterval
			if strings.Contains(errMsg, "status 404") || strings.Contains(errMsg, "no event found for slug") {
				retryAfter = paperHistoricalNotFoundRetry
			}
			t.nextHistoricalLookupAt = now.Add(retryAfter)
			if errMsg != t.lastHistoricalLookupError || t.lastHistoricalLookupErrorLogAt.IsZero() || now.Sub(t.lastHistoricalLookupErrorLogAt) >= paperResolutionErrorLogGap {
				t.TUI.LogEvent("[%s] ⚠️ Historical market lookup failed: %v (falling back to price estimate)", t.ID, err)
				t.lastHistoricalLookupError = errMsg
				t.lastHistoricalLookupErrorLogAt = now
			}
		} else {
			t.nextHistoricalLookupAt = now.Add(paperHistoricalLookupInterval)
			t.lastHistoricalLookupError = ""
			historyOutcomes := make([]string, 0, len(historyMarket.Tokens))
			for _, token := range historyMarket.Tokens {
				if token.Outcome != "" {
					historyOutcomes = append(historyOutcomes, token.Outcome)
				}
				if token.Winner {
					t.TUI.LogEvent("[%s] 🧾 Historical slug resolution: %s", t.ID, token.Outcome)
					return token.Outcome
				}
			}
			if len(historyOutcomes) == 0 {
				historyOutcomes = append(historyOutcomes, t.Outcomes...)
			}
			if winner, checked := resolveByCondition(historyMarket.ConditionID, historyOutcomes, t.EndTime); checked && winner != "" {
				t.TUI.LogEvent("[%s] 🧾 Historical condition resolution: %s", t.ID, winner)
				return winner
			}
		}
	}

	t.mu.Lock()
	bestOutcome, highestProb, signalSource, ok := detectTerminalWinnerFromPrices(t.Outcomes, t.TokenBids, t.TokenAsks, t.FloatPrices)
	t.mu.Unlock()

	if ok {
		t.TUI.LogEvent("[%s] 📊 Terminal price winner: %s ($%.3f via %s) [no on-chain winner yet]", t.ID, bestOutcome, highestProb, signalSource)
		return bestOutcome
	}
	return ""
}

// handleRestFallback polls REST API for fresh liquidity data.
// Returns true if any data was successfully retrieved.
func (t *MarketTrader) handleRestFallback(ctx context.Context, tokenPrices map[string]string, staleTime time.Duration, quoteState map[string]paperQuoteState, logRecovery bool) bool {
	t.LastRestPoll = time.Now()
	staleSeconds := int(staleTime.Seconds())

	restSuccess := 0
	restErrors := 0
	restEmpty := 0
	var lastErr error
	for tokenID, outcome := range t.TokenMap {
		restCtx, restCancel := context.WithTimeout(ctx, 3*time.Second)
		start := time.Now()
		book, err := t.RestClient.GetOrderBook(restCtx, tokenID)
		latency := time.Since(start)
		restCancel()

		t.TUI.UpdateRestLatency(latency)

		if err != nil {
			restErrors++
			lastErr = err
			break
		}

		updatedAt := time.Now()
		if len(book.Bids) == 0 && len(book.Asks) == 0 {
			restEmpty++
			t.mu.Lock()
			t.TokenBids[outcome] = 0
			t.TokenAsks[outcome] = 0
			t.TokenFullBids[outcome] = nil
			t.TokenFullAsks[outcome] = nil
			t.mu.Unlock()
			quoteState[outcome] = paperQuoteState{UpdatedAt: updatedAt, Source: "rest"}
			restSuccess++
			continue
		}

		bid, ask := 0.0, 0.0
		for _, b := range book.Bids {
			p, err := strconv.ParseFloat(b.Price, 64)
			if err != nil {
				t.TUI.LogEvent("[%s] Warning: failed to parse bid price %s: %v", t.ID, b.Price, err)
				continue
			}
			if p > 0 && p <= 1.0 && p > bid {
				bid = p
			}
		}
		for _, a := range book.Asks {
			p, err := strconv.ParseFloat(a.Price, 64)
			if err != nil {
				t.TUI.LogEvent("[%s] Warning: failed to parse ask price %s: %v", t.ID, a.Price, err)
				continue
			}
			if p > 0 && p <= 1.0 && (ask == 0 || p < ask) {
				ask = p
			}
		}

		if bid > 0 && ask > 0 && !paperHasSaneTopOfBook(bid, ask) {
			t.mu.Lock()
			t.TokenBids[outcome] = 0
			t.TokenAsks[outcome] = 0
			t.TokenFullBids[outcome] = nil
			t.TokenFullAsks[outcome] = nil
			t.mu.Unlock()
			quoteState[outcome] = paperQuoteState{UpdatedAt: updatedAt, Source: "rest"}
			restSuccess++
			continue
		}

		t.mu.Lock()
		t.TokenBids[outcome] = bid
		t.TokenAsks[outcome] = ask

		if bid > 0 && ask > 0 && ask < 1.0 {
			mid := (bid + ask) / 2
			t.FloatPrices[outcome] = mid
			tokenPrices[outcome] = fmt.Sprintf("%.3f", mid)
			t.Engine.UpdateMarketData(t.ID, outcome, mid, bid, ask)
		}
		t.TokenFullBids[outcome] = mkt.LevelsToPriceDepth(book.Bids, true)
		t.TokenFullAsks[outcome] = mkt.LevelsToPriceDepth(book.Asks, false)
		t.mu.Unlock()
		quoteState[outcome] = paperQuoteState{UpdatedAt: updatedAt, Source: "rest"}
		restSuccess++
	}

	if restSuccess > 0 {
		now := time.Now()
		t.LastUpdate = now
		syncPaperPairUpdate(t, now)
		if logRecovery && staleSeconds >= 10 {
			t.TUI.LogEvent("[%s] ✅ REST recovered after %ds", t.ID, staleSeconds)
		}
		return true
	}
	if restErrors > 0 {
		if staleSeconds%10 == 0 || staleSeconds == 10 {
			t.TUI.LogEvent("[%s] ❌ REST fail %ds: %v", t.ID, staleSeconds, lastErr)
		}
	} else if restEmpty == len(t.TokenMap) {
		if staleSeconds%10 == 0 {
			t.TUI.LogEvent("[%s] 📭 All books empty (%ds)", t.ID, staleSeconds)
		}
	}
	return false
}

func getPaperBinanceSymbol(marketID string, cfg *core.Config) string {
	if cfg == nil {
		return ""
	}
	asset := strings.TrimSpace(marketID)
	if idx := strings.Index(asset, "#"); idx >= 0 {
		asset = asset[:idx]
	}
	if idx := strings.Index(asset, "-"); idx >= 0 {
		asset = asset[:idx]
	}
	asset = strings.ToUpper(strings.TrimSpace(asset))
	if asset == "" || asset == "UNKNOWN" {
		return ""
	}
	return asset + strings.ToUpper(strings.TrimSpace(cfg.BinanceQuoteAsset))
}

func paperbotDirectionalProfitTargetPrice(avgPrice, profitTargetPct float64) float64 {
	if avgPrice <= 0 {
		return 0
	}
	if profitTargetPct <= 0 {
		return avgPrice
	}
	target := avgPrice * (1.0 + profitTargetPct/100.0)
	if target > 0.99 {
		return 0.99
	}
	return target
}

func paperbotAskLiquidityAtOrBelow(levels []paper.MarketLevel, maxPrice float64) float64 {
	total := 0.0
	for _, level := range levels {
		if level.Size <= 0 {
			continue
		}
		if maxPrice > 0 && level.Price > maxPrice+1e-9 {
			break
		}
		total += level.Size
	}
	return total
}

func paperbotBidLiquidityAtOrAbove(levels []paper.MarketLevel, minPrice float64) float64 {
	total := 0.0
	for _, level := range levels {
		if level.Size <= 0 {
			continue
		}
		if level.Price+1e-9 < minPrice {
			break
		}
		total += level.Size
	}
	return total
}

func paperbotNormalizedAsks(levels []paper.MarketLevel, bestAsk, minSize float64) []paper.MarketLevel {
	result := make([]paper.MarketLevel, len(levels))
	copy(result, levels)
	hasBest := false
	for _, level := range result {
		if math.Abs(level.Price-bestAsk) <= 1e-6 || level.Price < bestAsk+1e-6 {
			hasBest = true
			break
		}
	}
	if !hasBest && bestAsk > 0 {
		result = append(result, paper.MarketLevel{Price: bestAsk, Size: math.Max(minSize, 1)})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Price < result[j].Price })
	return result
}

func paperbotNormalizeBids(levels []paper.MarketLevel, bestBid, minSize float64) []paper.MarketLevel {
	result := make([]paper.MarketLevel, len(levels))
	copy(result, levels)
	hasBest := false
	for _, level := range result {
		if math.Abs(level.Price-bestBid) <= 1e-6 || level.Price > bestBid-1e-6 {
			hasBest = true
			break
		}
	}
	if !hasBest && bestBid > 0 {
		result = append(result, paper.MarketLevel{Price: bestBid, Size: math.Max(minSize, 1)})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Price > result[j].Price })
	return result
}

func paperbotFilterAsksAtOrBelow(levels []paper.MarketLevel, maxPrice float64) []paper.MarketLevel {
	if maxPrice <= 0 {
		return levels
	}
	filtered := make([]paper.MarketLevel, 0, len(levels))
	for _, level := range levels {
		if level.Size <= 0 {
			continue
		}
		if level.Price > maxPrice+1e-9 {
			break
		}
		filtered = append(filtered, level)
	}
	return filtered
}

func paperbotFilterBidsAtOrAbove(levels []paper.MarketLevel, minPrice float64) []paper.MarketLevel {
	if minPrice <= 0 {
		return levels
	}
	filtered := make([]paper.MarketLevel, 0, len(levels))
	for _, level := range levels {
		if level.Size <= 0 {
			continue
		}
		if level.Price+1e-9 < minPrice {
			break
		}
		filtered = append(filtered, level)
	}
	return filtered
}

func paperbotBuyCostForShares(qty float64, asks []paper.MarketLevel) float64 {
	remaining := qty
	cost := 0.0
	for _, level := range asks {
		if remaining <= 0 || level.Size <= 0 {
			continue
		}
		take := math.Min(remaining, level.Size)
		cost += take * level.Price
		remaining -= take
	}
	return cost
}

func paperbotCopytradeRequestedQty(targetDelta, price float64, liveCfg paper.TUISettings) float64 {
	return core.CalculateCopytradeSharesForMode(
		targetDelta,
		price,
		liveCfg.CopytradeSizeUSDC,
		liveCfg.CopytradeSizeShares,
		liveCfg.CopytradeSizePercent,
		liveCfg.MaxTradeSize,
		liveCfg.CopytradeSizingMode,
	)
}

func paperbotHandleCopytradeMarket(ctx context.Context, t *MarketTrader, liveCfg paper.TUISettings) {
	if t == nil || t.RestClient == nil || t.Engine == nil || t.Market == nil || t.CopytradeState == nil {
		return
	}
	if strings.TrimSpace(t.CopytradeWallet) == "" {
		if time.Since(t.lastCopytradeNoticeAt) > 10*time.Second {
			t.TUI.LogEvent("[%s] ⚠️ Copytrade target wallet is empty", t.ID)
			t.lastCopytradeNoticeAt = time.Now()
		}
		return
	}

	state := t.CopytradeState
	pollEvery := time.Duration(liveCfg.CopytradePollIntervalMs) * time.Millisecond
	if pollEvery <= 0 {
		pollEvery = 2 * time.Second
	}
	pollStartedAt := time.Now()
	apiReceivedAt := pollStartedAt
	polledTrades := make([]api.PublicTrade, 0)
	shouldPoll := state.lastTradeFetch.IsZero() || time.Since(state.lastTradeFetch) >= pollEvery
	if shouldPoll {
		since := state.lastTradeFetch
		state.lastTradeFetch = time.Now()
		if paperbotCopytradeHasOnchainWatcher(t.CopytradePoller) {
			combinedTrades := t.CopytradePoller.minedSignalsForCondition(t.Market.ConditionID, since)
			if pendingTrades := t.CopytradePoller.pendingSignalsForCondition(t.Market.ConditionID, since); len(pendingTrades) > 0 {
				combinedTrades = append(append([]api.PublicTrade{}, pendingTrades...), combinedTrades...)
			}
			polledTrades = paperbotCopytradeFreshTrades(state, combinedTrades, t.Market.ConditionID)
			state.lastError = ""
		} else {
			marketSnapshot := paperbotCopytradeMarketSnapshot{}
			if t.CopytradePoller != nil {
				pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				snapshot, err := t.CopytradePoller.snapshotForCondition(pollCtx, t.RestClient, pollEvery, t.Market.ConditionID)
				cancel()
				if err != nil {
					if err.Error() != state.lastError {
						t.TUI.LogEvent("[%s] ⚠️ Copytrade target poll failed: %v", t.ID, err)
						state.lastError = err.Error()
					}
					return
				}
				marketSnapshot = snapshot
				if !snapshot.PollStartedAt.IsZero() {
					pollStartedAt = snapshot.PollStartedAt
				}
			} else {
				pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				snapshot := t.RestClient.GetPublicActivitySnapshotWithFallback(
					pollCtx,
					t.CopytradeWallet,
					[]string{t.Market.ConditionID},
					128,
					0.01,
					8,
					nil,
					false,
					paperbotCopytradeTradeFetchTimeout(pollEvery),
					paperbotCopytradePositionFetchTimeout(pollEvery, false),
				)
				cancel()
				marketSnapshot = paperbotCopytradeMarketSnapshot{
					Trades:        snapshot.Trades,
					Positions:     snapshot.Positions,
					TradesErr:     snapshot.TradesErr,
					PositionsErr:  snapshot.PositionsErr,
					PollStartedAt: pollStartedAt,
					PolledAt:      time.Now(),
				}
			}
			apiReceivedAt = marketSnapshot.PolledAt
			if apiReceivedAt.IsZero() {
				apiReceivedAt = time.Now()
			}
			if marketSnapshot.TradesErr != nil && marketSnapshot.PositionsErr != nil {
				err := fmt.Errorf("trades: %v | positions: %v", marketSnapshot.TradesErr, marketSnapshot.PositionsErr)
				if err.Error() != state.lastError {
					t.TUI.LogEvent("[%s] ⚠️ Copytrade target poll failed: %v", t.ID, err)
					state.lastError = err.Error()
				}
				return
			}
			state.lastError = ""

			combinedTrades := marketSnapshot.Trades
			polledTrades = paperbotCopytradeFreshTrades(state, combinedTrades, t.Market.ConditionID)
			if len(polledTrades) == 0 && marketSnapshot.PositionsErr == nil {
				targetShares := paperbotCopytradeTargetSharesForCondition(marketSnapshot.Positions, t.Market.ConditionID)
				holdsBothOutcomes := paperbotCopytradeHoldsBothOutcomes(targetShares)
				hasAmbiguousExit := paperbotCopytradeHasAmbiguousPositionExit(marketSnapshot.Positions, t.Market.ConditionID)
				pollTime := time.Now()
				for _, outcome := range t.Outcomes {
					targetQty := targetShares[outcome]
					delta, changed, pending := paperbotCopytradeTargetDelta(state, outcome, targetQty, pollTime)
					if pending {
						msg := fmt.Sprintf("[%s] ⏳ Copytrade sell pending second snapshot for %s: target now %s shares", t.ID, outcome, paperbotFormatShareQty(targetQty))
						if paperbotCopytradeShouldLog(state, "sell-pending:"+outcome, msg, 10*time.Second) {
							t.TUI.LogEvent("%s", msg)
						}
						continue
					}
					if changed {
						if delta < -0.01 {
							reason := "position-only exits are disabled"
							switch {
							case hasAmbiguousExit:
								reason = "target inventory is mergeable/redeemable"
							case holdsBothOutcomes:
								reason = "target holds both outcomes"
							}
							msg := fmt.Sprintf("[%s] ℹ️ Copytrade ignored position-only sell for %s: %s", t.ID, outcome, reason)
							if paperbotCopytradeShouldLog(state, "sell-ambiguous:"+outcome, msg, 10*time.Second) {
								t.TUI.LogEvent("%s", msg)
							}
							continue
						}
						if delta <= 0.01 {
							continue
						}
						polledTrades = append(polledTrades, paperbotEstimatedPositionBuySignals(state, t.Market.ConditionID, outcome, delta, liveCfg.CopytradeSizingMode)...)
					}
				}
			}
		}
	}
	for _, signal := range polledTrades {
		paperbotObserveCopytradeBuySignal(state, signal)
	}

	freshTrades := make([]api.PublicTrade, 0, len(state.retryTrades)+len(polledTrades))
	if retries := paperbotCopytradeTakeRetryTrades(state, time.Now()); len(retries) > 0 {
		freshTrades = append(freshTrades, retries...)
	}
	if len(polledTrades) > 0 {
		freshTrades = append(freshTrades, polledTrades...)
	}
	if len(freshTrades) == 0 {
		return
	}

	retrySignals := make([]api.PublicTrade, 0)
	requeueSignal := func(sig api.PublicTrade) {
		retrySignals = append(retrySignals, sig)
	}

	for _, signal := range freshTrades {
		outcome := core.SanitizeString(signal.Outcome)
		if outcome == "" {
			continue
		}
		localQty, avgPrice := paperbotLocalPositionAvg(t.Engine, t.ID, outcome)
		if localQty > 0.01 {
			state.managed[outcome] = true
		}
		tokenID := mkt.GetTokenIDForOutcome(t.Market, outcome)
		if tokenID == "" {
			continue
		}
		tradeSide := strings.ToUpper(strings.TrimSpace(signal.Side))
		tradeSize := math.Max(0, signal.Size)
		if tradeSize <= 0.01 {
			continue
		}

		if tradeSide == "BUY" {
			ask := t.TokenAsks[outcome]
			quoteSource := "WS"
			asks := paperbotNormalizedAsks(t.TokenFullAsks[outcome], ask, math.Max(tradeSize, 1))
			if ask <= 0 || ask >= 1.0 {
				quoteSource = "REST"
				restCtx, restCancel := context.WithTimeout(ctx, 3*time.Second)
				_, restAsk, restErr := t.RestClient.GetBestBidAsk(restCtx, tokenID)
				restCancel()
				if restErr != nil {
					msg := fmt.Sprintf("[%s] ⚠️ Copytrade buy skipped for %s: quote refresh failed: %v", t.ID, outcome, restErr)
					if paperbotCopytradeShouldLog(state, "buy-quote:"+outcome, msg, 10*time.Second) {
						t.TUI.LogEvent("%s", msg)
					}
					requeueSignal(signal)
					continue
				}
				ask = restAsk
				asks = paperbotNormalizedAsks(nil, ask, math.Max(tradeSize, 1))
			}
			if ask <= 0 || ask >= 1.0 {
				msg := fmt.Sprintf("[%s] ⚠️ Copytrade buy skipped for %s: missing valid ask", t.ID, outcome)
				if paperbotCopytradeShouldLog(state, "buy-ask:"+outcome, msg, 10*time.Second) {
					t.TUI.LogEvent("%s", msg)
				}
				requeueSignal(signal)
				continue
			}
			submitPrice := core.CopytradeBuyLimitPrice(ask, liveCfg.CopytradeMaxSlippagePct)
			asks = paperbotFilterAsksAtOrBelow(asks, submitPrice)
			if len(asks) == 0 {
				msg := fmt.Sprintf("[%s] ⚠️ Copytrade buy skipped for %s: no ask liquidity within $%.3f cap", t.ID, outcome, submitPrice)
				if paperbotCopytradeShouldLog(state, "buy-cap:"+outcome, msg, 10*time.Second) {
					t.TUI.LogEvent("%s", msg)
				}
				continue
			}

			requestedQty := paperbotNormalizeMarketBuyShares(core.CalculateCopytradeSharesForMode(tradeSize, submitPrice, liveCfg.CopytradeSizeUSDC, liveCfg.CopytradeSizeShares, liveCfg.CopytradeSizePercent, liveCfg.MaxTradeSize, liveCfg.CopytradeSizingMode))
			liq := paperbotAskLiquidityAtOrBelow(asks, submitPrice)
			if liq > 0 && requestedQty > liq {
				requestedQty = paperbotNormalizeMarketBuyShares(liq)
			}
			if balance := t.Engine.GetBalance(); balance > 0 {
				for requestedQty >= paperbotMinActionShares && paperbotBuyCostForShares(requestedQty, asks) > balance+1e-9 {
					cost := paperbotBuyCostForShares(requestedQty, asks)
					if cost <= 0 {
						break
					}
					requestedQty = paperbotNormalizeMarketBuyShares(requestedQty * balance / cost)
				}
			}
			if requestedQty < paperbotMinActionShares {
				msg := fmt.Sprintf("[%s] ⚠️ Copytrade buy skipped for %s: actionable size below %.2f share", t.ID, outcome, paperbotMinActionShares)
				if paperbotCopytradeShouldLog(state, "buy-size:"+outcome, msg, 10*time.Second) {
					t.TUI.LogEvent("%s", msg)
				}
				continue
			}
			label := strings.TrimSpace(signal.Source)
			if label == "" {
				label = "trade"
				if signal.Timestamp == 0 {
					label = "position"
				}
			}
			latency := paperCopytradeLatency{
				pollStartedAt:   pollStartedAt,
				apiReceivedAt:   apiReceivedAt,
				signalTimestamp: signal.Timestamp,
				marketID:        t.ID,
				outcome:         outcome,
				side:            "BUY",
				source:          label,
				txHash:          strings.TrimSpace(signal.TransactionHash),
			}
			latency.quoteReadyAt = time.Now()
			t.TUI.LogEvent("[%s] 🪞 Copytrade BUY %s: target %s %s shares, submit %s @ cap $%.3f (%s ask $%.3f, slip %.0fc)",
				t.ID, outcome, label, paperbotFormatShareQty(tradeSize), paperbotFormatShareQty(requestedQty), submitPrice, quoteSource, ask, liveCfg.CopytradeMaxSlippagePct)
			trade, avgFill, buyErr := t.Engine.MarketBuy(t.ID, outcome, requestedQty, asks)
			if buyErr != nil {
				msg := fmt.Sprintf("[%s] ❌ Copytrade buy failed for %s: %v", t.ID, outcome, buyErr)
				if paperbotCopytradeShouldLog(state, "buy-fail:"+outcome, msg, 10*time.Second) {
					t.TUI.LogEvent("%s", msg)
				}
				continue
			}
			latency.executedAt = time.Now()
			state.managed[outcome] = true
			t.TUI.RecordOrderWithMode(t.ID, outcome, "BUY", trade.Quantity, avgFill, trade.Value, 0.0, 0.0, paperArbModeCopytrade, "FILLED")
			t.TUI.LogEvent("[%s] ✅ Copytrade bought %s %s at $%.3f", t.ID, paperbotFormatShareQty(trade.Quantity), outcome, avgFill)
			logPaperCopytradeLatency(t, latency)
			continue
		}

		if tradeSide == "SELL" && state.managed[outcome] && localQty > 0.01 {
			bid := t.TokenBids[outcome]
			quoteSource := "WS"
			bids := paperbotNormalizeBids(t.TokenFullBids[outcome], bid, math.Max(tradeSize, 1))
			if bid <= 0 || bid >= 1.0 {
				quoteSource = "REST"
				restCtx, restCancel := context.WithTimeout(ctx, 3*time.Second)
				restBid, _, restErr := t.RestClient.GetBestBidAsk(restCtx, tokenID)
				restCancel()
				if restErr != nil {
					msg := fmt.Sprintf("[%s] ⚠️ Copytrade sell skipped for %s: quote refresh failed: %v", t.ID, outcome, restErr)
					if paperbotCopytradeShouldLog(state, "sell-quote:"+outcome, msg, 10*time.Second) {
						t.TUI.LogEvent("%s", msg)
					}
					requeueSignal(signal)
					continue
				}
				bid = restBid
				bids = paperbotNormalizeBids(nil, bid, math.Max(tradeSize, 1))
			}
			if bid <= 0 || bid >= 1.0 {
				msg := fmt.Sprintf("[%s] ⚠️ Copytrade sell skipped for %s: missing valid bid", t.ID, outcome)
				if paperbotCopytradeShouldLog(state, "sell-bid:"+outcome, msg, 10*time.Second) {
					t.TUI.LogEvent("%s", msg)
				}
				requeueSignal(signal)
				continue
			}
			submitFloor := core.CopytradeSellFloorPrice(bid, liveCfg.CopytradeMaxSlippagePct)
			bids = paperbotFilterBidsAtOrAbove(bids, submitFloor)
			if len(bids) == 0 {
				msg := fmt.Sprintf("[%s] ⚠️ Copytrade sell skipped for %s: no bid liquidity above $%.3f floor", t.ID, outcome, submitFloor)
				if paperbotCopytradeShouldLog(state, "sell-floor:"+outcome, msg, 10*time.Second) {
					t.TUI.LogEvent("%s", msg)
				}
				continue
			}

			requestedQty := paperbotNormalizeMarketSellShares(core.CalculateCopytradeSharesForMode(tradeSize, submitFloor, liveCfg.CopytradeSizeUSDC, liveCfg.CopytradeSizeShares, liveCfg.CopytradeSizePercent, liveCfg.MaxTradeSize, liveCfg.CopytradeSizingMode))
			if requestedQty > localQty {
				requestedQty = paperbotNormalizeMarketSellShares(localQty)
			}
			liq := paperbotBidLiquidityAtOrAbove(bids, submitFloor)
			if liq > 0 && requestedQty > liq {
				requestedQty = paperbotNormalizeMarketSellShares(liq)
			}
			if requestedQty < paperbotMinActionShares {
				msg := fmt.Sprintf("[%s] ⚠️ Copytrade sell skipped for %s: actionable size below %.2f share", t.ID, outcome, paperbotMinActionShares)
				if paperbotCopytradeShouldLog(state, "sell-size:"+outcome, msg, 10*time.Second) {
					t.TUI.LogEvent("%s", msg)
				}
				continue
			}
			latency := paperCopytradeLatency{
				pollStartedAt:   pollStartedAt,
				apiReceivedAt:   apiReceivedAt,
				signalTimestamp: signal.Timestamp,
				marketID:        t.ID,
				outcome:         outcome,
				side:            "SELL",
				source:          "trade",
				txHash:          strings.TrimSpace(signal.TransactionHash),
			}
			if signal.Timestamp == 0 {
				latency.source = "position"
			}
			if signal.Source != "" {
				latency.source = signal.Source
			}
			latency.quoteReadyAt = time.Now()

			t.TUI.LogEvent("[%s] 🪞 Copytrade SELL %s: target %s %s shares, sell %s @ floor $%.3f (%s bid $%.3f, slip %.0fc)",
				t.ID, outcome, latency.source, paperbotFormatShareQty(tradeSize), paperbotFormatShareQty(requestedQty), submitFloor, quoteSource, bid, liveCfg.CopytradeMaxSlippagePct)
			trade, sellErr := t.Engine.SellForMarket(t.ID, outcome, bid, requestedQty)
			if sellErr != nil {
				msg := fmt.Sprintf("[%s] ❌ Copytrade sell failed for %s: %v", t.ID, outcome, sellErr)
				if paperbotCopytradeShouldLog(state, "sell-fail:"+outcome, msg, 10*time.Second) {
					t.TUI.LogEvent("%s", msg)
				}
				continue
			}
			latency.executedAt = time.Now()
			profit := trade.Value - (avgPrice * trade.Quantity)
			t.TUI.RecordOrderWithMode(t.ID, outcome, "SELL", trade.Quantity, bid, trade.Value, 0.0, profit, paperArbModeCopytrade, "FILLED")
			t.TUI.LogEvent("[%s] ✅ Copytrade sold %s %s at $%.3f", t.ID, paperbotFormatShareQty(trade.Quantity), outcome, bid)
			logPaperCopytradeLatency(t, latency)
			if remainingQty, _ := paperbotLocalPositionAvg(t.Engine, t.ID, outcome); remainingQty <= 0.01 {
				state.managed[outcome] = false
			}
		}
		paperbotCopytradeQueueRetryTrades(state, retrySignals)
	}
}

func paperbotHandleBinanceGapMarket(ctx context.Context, t *MarketTrader, liveCfg paper.TUISettings, cfg *core.Config) {
	_ = ctx
	now := time.Now()
	paperExecutionDelay := core.ResolvePaperBinanceExecutionDelay(cfg)
	logThrottled := func(format string, args ...interface{}) {
		if t.LastBinanceLog == nil {
			logAt := time.Now()
			t.LastBinanceLog = &logAt
			t.TUI.LogEvent(format, args...)
			return
		}
		if time.Since(*t.LastBinanceLog) >= 5*time.Second {
			t.TUI.LogEvent(format, args...)
			logAt := time.Now()
			t.LastBinanceLog = &logAt
		}
	}
	status := paper.MarketBinanceSignal{
		Enabled:   true,
		Status:    "waiting",
		Reason:    "awaiting Binance signal",
		UpdatedAt: now,
	}
	defer func() {
		if t.TUI != nil {
			status.UpdatedAt = time.Now()
			t.TUI.SetMarketBinanceSignal(t.ID, status)
		}
	}()

	mapping := paper.DirectionalOutcomes{}
	for _, outcome := range t.Outcomes {
		switch strings.ToLower(strings.TrimSpace(outcome)) {
		case "up", "yes":
			mapping.Up = outcome
		case "down", "no":
			mapping.Down = outcome
		}
	}
	if mapping.Up == "" || mapping.Down == "" {
		status.Status = "inactive"
		status.Reason = "outcomes are not Up/Down or Yes/No"
		logThrottled("[%s] ℹ️ Binance gap mode skipped: outcomes are not Up/Down or Yes/No", t.ID)
		return
	}
	if t.BinanceFeed == nil {
		status.Status = "inactive"
		status.Reason = "no Binance futures feed configured"
		logThrottled("[%s] ℹ️ Binance gap mode skipped: no Binance futures feed configured", t.ID)
		return
	}
	positions := t.Engine.GetPositions()
	upPos, hasUpPos := getPaperMarketPosition(positions, t.ID, mapping.Up)
	downPos, hasDownPos := getPaperMarketPosition(positions, t.ID, mapping.Down)
	if hasUpPos && upPos.Quantity > 0 && hasDownPos && downPos.Quantity > 0 {
		status.Status = "exit"
		status.Reason = "holding both outcomes; waiting for cleanup"
		return
	}
	heldOutcome := ""
	heldQty := 0.0
	heldAvg := 0.0
	if hasUpPos && upPos.Quantity > 0 {
		heldOutcome = mapping.Up
		heldQty = upPos.Quantity
		heldAvg = upPos.AvgPrice
	} else if hasDownPos && downPos.Quantity > 0 {
		heldOutcome = mapping.Down
		heldQty = downPos.Quantity
		heldAvg = downPos.AvgPrice
	}
	if heldOutcome != "" {
		status.TargetOutcome = heldOutcome
		status.Status = "exit"
		status.Reason = "managing existing position"
		bid := t.TokenBids[heldOutcome]
		targetBid := paperbotDirectionalProfitTargetPrice(heldAvg, liveCfg.MinMarginPercent)
		if bid <= 0 || bid+1e-9 < targetBid {
			return
		}
		liq := paperbotBidLiquidityAtOrAbove(t.TokenFullBids[heldOutcome], bid)
		sellQty := paperbotNormalizeMarketSellShares(math.Min(heldQty, liq))
		if sellQty < paperbotMinActionShares {
			status.Reason = fmt.Sprintf("waiting for bid liquidity on %s", heldOutcome)
			return
		}
		latency := paperExecutionLatency{
			detectedAt:  now,
			startedAt:   now,
			opportunity: "paper binance-gap exit",
			marketID:    t.ID,
			shares:      sellQty,
		}
		if paperExecutionDelay > 0 {
			time.Sleep(paperExecutionDelay)
		}
		trade, err := t.Engine.SellForMarket(t.ID, heldOutcome, bid, sellQty)
		if err != nil {
			status.Status = "blocked"
			status.Reason = err.Error()
			logThrottled("[%s] ⚠️ Binance paper exit failed for %s: %v", t.ID, heldOutcome, err)
			return
		}
		latency.executedAt = time.Now()
		profit := trade.Value - (heldAvg * sellQty)
		status.Status = "filled"
		status.Reason = fmt.Sprintf("sold %s %s @ $%.3f", paperbotFormatShareQty(sellQty), heldOutcome, bid)
		t.TUI.LogEvent("[%s] ✅ BINANCE PAPER EXIT %s %s @ $%.3f (target $%.3f, pnl $%.2f)", t.ID, heldOutcome, paperbotFormatShareQty(sellQty), bid, targetBid, profit)
		t.TUI.RecordOrderWithMode(t.ID, heldOutcome, "SELL", sellQty, bid, trade.Value, 0.0, profit, paperArbModeBinanceGap, "FILLED")
		logPaperExecutionLatency(t, latency)
		return
	}

	cooldown := core.ResolveBinanceSignalCooldown(cfg)
	if !t.LastBinanceTrigger.IsZero() && now.Sub(t.LastBinanceTrigger) < cooldown {
		status.Status = "cooldown"
		status.Reason = fmt.Sprintf("cooldown %s", cooldown.Round(time.Millisecond))
		return
	}

	snap := t.BinanceFeed.Snapshot(now)
	status.Symbol = snap.Symbol
	status.Price = snap.Price
	status.DeltaPercent = snap.DeltaPercent

	maxSignalAge := core.ResolveBinanceSignalMaxAge(cfg)
	if errMsg := strings.TrimSpace(snap.LastError); errMsg != "" {
		status.Status = "error"
		status.Reason = "Binance WS error"
		logThrottled("[%s] ⚠️ Binance gap feed error on %s: %s", t.ID, snap.Symbol, errMsg)
		return
	}
	if !snap.Connected && snap.UpdatedAt.IsZero() {
		status.Status = "connecting"
		status.Reason = fmt.Sprintf("connecting to Binance on %s", snap.Symbol)
		return
	}
	if !snap.Ready {
		status.Status = "warmup"
		status.Reason = fmt.Sprintf("building lookback window on %s", snap.Symbol)
		return
	}
	if snap.UpdatedAt.IsZero() || now.Sub(snap.UpdatedAt) > maxSignalAge {
		status.Status = "waiting"
		status.Reason = fmt.Sprintf("waiting for fresh WS signal on %s", snap.Symbol)
		return
	}
	signal, reason := paper.EvaluateBinanceGapSignal(now, mapping, t.TokenBids, t.TokenAsks, t.TokenFullBids, t.TokenFullAsks, snap, t.PolySignalTracker, maxSignalAge)
	status.TargetOutcome = signal.TargetOutcome
	status.SignalLabel = signal.SignalLabel
	status.EffectiveGapPercent = signal.EffectiveGapPercent
	status.PolyFavorableMoveCents = signal.PolyFavorableMoveCents
	status.PolyAdverseMoveCents = signal.PolyAdverseMoveCents
	status.TargetSpreadCents = signal.TargetSpreadCents
	status.TargetBookImbalance = signal.TargetBookImbalance
	status.OppositeBookImbalance = signal.OppositeBookImbalance
	status.DirectionalBookScore = signal.DirectionalBookScore

	if reason != "" {
		status.Status = "waiting"
		status.Reason = reason
		return
	}
	threshold := cfg.BinanceSignalThresholdPct
	if threshold <= 0 {
		threshold = 0.02
	}
	if signal.EffectiveGapPercent < threshold {
		status.Status = "idle"
		status.Reason = fmt.Sprintf("cross-market gap %.3f%% is below the %.3f%% trigger", signal.EffectiveGapPercent, threshold)
		return
	}
	if signal.DirectionalBookScore <= -0.35 {
		status.Status = "blocked"
		status.Reason = fmt.Sprintf("local book opposes %s signal (score %.2f)", signal.SignalLabel, signal.DirectionalBookScore)
		return
	}

	polyCatchupMax := cfg.BinanceSignalPolyMaxMoveCents
	if polyCatchupMax > 0 && signal.PolyFavorableMoveCents > polyCatchupMax {
		status.Status = "blocked"
		status.Reason = fmt.Sprintf("%s already caught up %.2fc > %.2fc", signal.TargetOutcome, signal.PolyFavorableMoveCents, polyCatchupMax)
		logThrottled("[%s] ⚠️ Binance entry skipped: %s already caught up %.2fc > %.2fc", t.ID, signal.TargetOutcome, signal.PolyFavorableMoveCents, polyCatchupMax)
		return
	}

	polyAdverseMax := cfg.BinanceSignalPolyAdverseMoveCents
	if polyAdverseMax > 0 && signal.PolyAdverseMoveCents > polyAdverseMax {
		status.Status = "blocked"
		status.Reason = fmt.Sprintf("Polymarket moved against %s by %.2fc > %.2fc", signal.SignalLabel, signal.PolyAdverseMoveCents, polyAdverseMax)
		logThrottled("[%s] ⚠️ Binance entry skipped: Polymarket moved against %s by %.2fc > %.2fc", t.ID, signal.SignalLabel, signal.PolyAdverseMoveCents, polyAdverseMax)
		return
	}

	spreadMax := cfg.BinanceSignalSpreadMaxCents
	if spreadMax <= 0 {
		spreadMax = paper.DefaultBinanceSignalSpreadMaxCents
	}
	if signal.TargetSpreadCents > spreadMax {
		status.Status = "blocked"
		status.Reason = fmt.Sprintf("%s spread %.2fc > %.2fc", signal.TargetOutcome, signal.TargetSpreadCents, spreadMax)
		logThrottled("[%s] ⚠️ Binance entry skipped: %s spread %.2fc > %.2fc", t.ID, signal.TargetOutcome, signal.TargetSpreadCents, spreadMax)
		return
	}

	targetOutcome := signal.TargetOutcome
	ask := t.TokenAsks[targetOutcome]
	if ask < liveCfg.MinAskPrice || ask > liveCfg.MaxAskPrice {
		status.Ready = false
		status.Status = "blocked"
		status.Reason = fmt.Sprintf("%s ask $%.3f outside %.3f-%.3f", targetOutcome, ask, liveCfg.MinAskPrice, liveCfg.MaxAskPrice)
		logThrottled("[%s] ⚠️ Binance entry skipped: %s ask $%.3f outside configured range %.3f-%.3f", t.ID, targetOutcome, ask, liveCfg.MinAskPrice, liveCfg.MaxAskPrice)
		return
	}

	status.Ready = true
	status.Status = "ready"
	status.Reason = "signal ready"
	tradeBudget := cfg.CalculateTradeSize(t.Engine.GetSizingBalance())
	asks := paperbotNormalizedAsks(t.TokenFullAsks[targetOutcome], ask, tradeBudget/ask)
	liq := paperbotAskLiquidityAtOrBelow(asks, ask)
	buyQty := paperbotNormalizeMarketBuyShares(math.Min(tradeBudget/ask, liq))
	if buyQty < paperbotMinActionShares {
		status.Ready = false
		status.Status = "blocked"
		status.Reason = fmt.Sprintf("actionable size below %.2f share for %s", paperbotMinActionShares, targetOutcome)
		logThrottled("[%s] ⚠️ Binance entry skipped: actionable size below %.2f share for %s", t.ID, paperbotMinActionShares, targetOutcome)
		return
	}
	latency := paperExecutionLatency{
		detectedAt:  now,
		startedAt:   now,
		opportunity: "paper binance-gap entry",
		marketID:    t.ID,
		shares:      buyQty,
		marginPct:   signal.EffectiveGapPercent,
		expectedPnL: tradeBudget - (buyQty * ask),
	}
	if paperExecutionDelay > 0 {
		time.Sleep(paperExecutionDelay)
	}
	trade, avgPrice, err := t.Engine.MarketBuy(t.ID, targetOutcome, buyQty, asks)
	if err != nil {
		status.Ready = false
		status.Status = "blocked"
		status.Reason = err.Error()
		logThrottled("[%s] ⚠️ Binance paper entry failed for %s: %v", t.ID, targetOutcome, err)
		return
	}
	latency.executedAt = time.Now()
	t.LastBinanceTrigger = now
	status.Status = "filled"
	status.Reason = fmt.Sprintf("bought %s %s @ $%.3f", paperbotFormatShareQty(trade.Quantity), targetOutcome, avgPrice)
	t.TUI.LogEvent("[%s] ✅ BINANCE PAPER ENTRY %s Move %.2f%%. Bought %s %s @ avg $%.3f (top $%.3f, budget $%.2f)", t.ID, signal.SignalLabel, snap.DeltaPercent, paperbotFormatShareQty(trade.Quantity), targetOutcome, avgPrice, ask, tradeBudget)
	t.TUI.RecordOrderWithMode(t.ID, targetOutcome, "BUY", trade.Quantity, avgPrice, trade.Value, snap.DeltaPercent, 0.0, paperArbModeBinanceGap, "FILLED")
	logPaperExecutionLatency(t, latency)
}
