package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"Market-bot/internal/api"
	mkt "Market-bot/internal/markets"
	"Market-bot/internal/paper"
)

var errNilRestOrderBook = errors.New("rest order book response was nil")

type realbotMarketQuoteArgs struct {
	ctx                    context.Context
	marketID               string
	wsMgr                  *api.WSManager
	wsMsgChan              <-chan []byte
	tokenMap               map[string]string
	tokenToOutcome         map[string]string
	outcomes               []string
	tokenBids              map[string]float64
	tokenAsks              map[string]float64
	tokenFullBids          map[string][]paper.MarketLevel
	tokenFullAsks          map[string][]paper.MarketLevel
	displayBids            map[string]float64
	displayAsks            map[string]float64
	publishedBids          map[string]float64
	publishedAsks          map[string]float64
	quoteState             map[string]realbotQuoteState
	polySignalTracker      *paper.DirectionalSignalTracker
	engine                 *paper.Engine
	restClient             *api.RestClient
	tui                    *paper.TUI
	restFallbackQuoteAge   time.Duration
	restFallbackPollPeriod time.Duration
}

type realbotMarketQuoteRuntime struct {
	lastPairUpdate       *time.Time
	lastPublishedQuoteAt *time.Time
	lastReconnectCount   *int32
	lastReconnectTime    *time.Time
	lastWsWarnTime       *time.Time
	lastForceReconnect   *time.Time
	lastRestFallbackPoll *time.Time
	lastTelemetryUpdate  *time.Time
	restFallbackActive   *bool
	restRecoveryLogged   *bool
	wsChannelClosed      *bool
	metrics              *realbotRuntimeMetrics
}

func realbotHandleMarketWSMessage(args realbotMarketQuoteArgs, msg []byte, lastPairUpdate *time.Time) bool {
	// Parse and process WebSocket message immediately.
	//
	// Polymarket CLOB WS sends:
	// 1. Book snapshots ("book") on subscribe/reconnect.
	// 2. Price-change deltas ("price_change") with changed levels and explicit BBO values.
	// 3. Best-bid-ask updates ("best_bid_ask") when subscribed with custom_feature_enabled.
	parsed, err := api.ParseMarketWSMessage(msg)
	if err != nil || parsed == nil {
		return false
	}

	switch parsed.Kind {
	case api.MarketWSMessageOrderBooks:
		depthChanged := false
		for _, book := range parsed.OrderBooks {
			outcome := args.tokenToOutcome[book.AssetID]
			if outcome == "" {
				continue
			}
			updatedAt := realbotQuoteTimestampOrNow(book.Timestamp)
			if realbotShouldSkipStaleQuoteUpdate(args.quoteState, outcome, updatedAt, args.tokenBids[outcome], args.tokenAsks[outcome]) {
				continue
			}

			bid, ask := 0.0, 0.0
			for _, order := range book.Bids {
				price, err := strconv.ParseFloat(order.Price, 64)
				if err != nil {
					continue
				}
				if price > 0 && price <= 1.0 && price > bid {
					bid = price
				}
			}
			for _, order := range book.Asks {
				price, err := strconv.ParseFloat(order.Price, 64)
				if err != nil {
					continue
				}
				if price > 0 && price <= 1.0 && (ask == 0 || price < ask) {
					ask = price
				}
			}

			if bid > 0 && ask > 0 && !realbotHasSaneTopOfBook(bid, ask) {
				args.tokenBids[outcome] = 0
				args.tokenAsks[outcome] = 0
				args.tokenFullBids[outcome] = nil
				args.tokenFullAsks[outcome] = nil
				depthChanged = true
				continue
			}

			args.tokenBids[outcome] = bid
			args.tokenAsks[outcome] = ask
			args.tokenFullBids[outcome] = mkt.LevelsToPriceDepth(book.Bids, true)
			args.tokenFullAsks[outcome] = mkt.LevelsToPriceDepth(book.Asks, false)
			args.quoteState[outcome] = realbotQuoteState{UpdatedAt: updatedAt, Source: "ws"}
			realbotSyncPairUpdate(args.outcomes, args.tokenBids, args.tokenAsks, lastPairUpdate, updatedAt)
			depthChanged = true

			if bid > 0 && ask > 0 {
				mid := (bid + ask) / 2
				args.engine.UpdateMarketData(args.marketID, outcome, mid, bid, ask)
				args.polySignalTracker.Record(outcome, bid, ask, updatedAt)
			}
		}
		return depthChanged

	case api.MarketWSMessagePriceUpdate:
		update := parsed.PriceUpdate
		if update == nil || len(update.PriceChanges) == 0 {
			return false
		}
		foundForThisMarket := false
		depthChanged := false
		touchedOutcomes := make(map[string]bool)
		type explicitTopOfBook struct {
			bid    float64
			ask    float64
			hasBid bool
			hasAsk bool
		}
		explicitTopByOutcome := make(map[string]explicitTopOfBook)

		for _, pc := range update.PriceChanges {
			outcome := args.tokenToOutcome[pc.AssetID]
			if outcome == "" {
				continue
			}
			foundForThisMarket = true
			touchedOutcomes[outcome] = true
			price, errP := strconv.ParseFloat(pc.Price, 64)
			size, errS := strconv.ParseFloat(pc.Size, 64)
			if errP != nil || errS != nil || price <= 0 {
				continue
			}

			switch pc.Side {
			case "BUY":
				args.tokenFullBids[outcome] = mkt.ApplyDelta(args.tokenFullBids[outcome], price, size, true)
				depthChanged = true
			case "SELL":
				args.tokenFullAsks[outcome] = mkt.ApplyDelta(args.tokenFullAsks[outcome], price, size, false)
				depthChanged = true
			}

			top := explicitTopByOutcome[outcome]
			if strings.TrimSpace(pc.BestBid) == "" {
				top.bid = 0
				top.hasBid = true
			} else if bestBid, ok := parseWSQuotedPrice(pc.BestBid); ok {
				top.bid = bestBid
				top.hasBid = true
			}
			if strings.TrimSpace(pc.BestAsk) == "" {
				top.ask = 0
				top.hasAsk = true
			} else if bestAsk, ok := parseWSQuotedPrice(pc.BestAsk); ok {
				top.ask = bestAsk
				top.hasAsk = true
			}
			if top.hasBid || top.hasAsk {
				explicitTopByOutcome[outcome] = top
			}
		}

		mkt.RefreshTopOfBookFromDepth(args.outcomes, args.tokenFullBids, args.tokenFullAsks, args.tokenBids, args.tokenAsks)
		for _, outcome := range args.outcomes {
			if top, ok := explicitTopByOutcome[outcome]; ok {
				if top.hasBid {
					args.tokenBids[outcome] = top.bid
				}
				if top.hasAsk {
					args.tokenAsks[outcome] = top.ask
				}
			}

			if args.tokenBids[outcome] > 0 && args.tokenAsks[outcome] > 0 {
				if !realbotHasSaneTopOfBook(args.tokenBids[outcome], args.tokenAsks[outcome]) {
					args.tokenBids[outcome] = 0
					args.tokenAsks[outcome] = 0
					args.tokenFullBids[outcome] = nil
					args.tokenFullAsks[outcome] = nil
					continue
				}

				mid := (args.tokenBids[outcome] + args.tokenAsks[outcome]) / 2
				args.engine.UpdateMarketData(args.marketID, outcome, mid, args.tokenBids[outcome], args.tokenAsks[outcome])
				args.polySignalTracker.Record(outcome, args.tokenBids[outcome], args.tokenAsks[outcome], time.Now())
			}
		}

		if foundForThisMarket {
			now := time.Now()
			for outcome := range touchedOutcomes {
				args.quoteState[outcome] = realbotQuoteState{UpdatedAt: now, Source: "ws"}
			}
			realbotSyncPairUpdate(args.outcomes, args.tokenBids, args.tokenAsks, lastPairUpdate, now)
		}
		return depthChanged

	case api.MarketWSMessageBestBidAsk:
		bbo := parsed.BestBidAsk
		if bbo == nil || !strings.EqualFold(strings.TrimSpace(bbo.EventType), "best_bid_ask") || bbo.AssetID == "" {
			return false
		}
		outcome := args.tokenToOutcome[bbo.AssetID]
		if outcome == "" {
			return false
		}
		updatedAt := realbotQuoteTimestampOrNow(bbo.Timestamp)
		if realbotShouldSkipStaleQuoteUpdate(args.quoteState, outcome, updatedAt, args.tokenBids[outcome], args.tokenAsks[outcome]) {
			return false
		}
		if bestBid, ok := parseWSQuotedPrice(bbo.BestBid); ok {
			args.tokenBids[outcome] = bestBid
		} else {
			args.tokenBids[outcome] = 0
		}
		if bestAsk, ok := parseWSQuotedPrice(bbo.BestAsk); ok {
			args.tokenAsks[outcome] = bestAsk
		} else {
			args.tokenAsks[outcome] = 0
		}
		if args.tokenBids[outcome] > 0 && args.tokenAsks[outcome] > 0 && !realbotHasSaneTopOfBook(args.tokenBids[outcome], args.tokenAsks[outcome]) {
			args.tokenBids[outcome] = 0
			args.tokenAsks[outcome] = 0
		}
		if args.tokenBids[outcome] > 0 && args.tokenAsks[outcome] > 0 {
			mid := (args.tokenBids[outcome] + args.tokenAsks[outcome]) / 2
			args.engine.UpdateMarketData(args.marketID, outcome, mid, args.tokenBids[outcome], args.tokenAsks[outcome])
			args.polySignalTracker.Record(outcome, args.tokenBids[outcome], args.tokenAsks[outcome], updatedAt)
		}
		args.quoteState[outcome] = realbotQuoteState{UpdatedAt: updatedAt, Source: "ws-bbo"}
		realbotSyncPairUpdate(args.outcomes, args.tokenBids, args.tokenAsks, lastPairUpdate, updatedAt)
		return false

	case api.MarketWSMessageOrderBook:
		book := parsed.OrderBook
		if book == nil || book.AssetID == "" {
			return false
		}
		bid, ask := 0.0, 0.0
		for _, bidLevel := range book.Bids {
			price, _ := strconv.ParseFloat(bidLevel.Price, 64)
			if price > 0 && price <= 1.0 && price > bid {
				bid = price
			}
		}
		for _, askLevel := range book.Asks {
			price, _ := strconv.ParseFloat(askLevel.Price, 64)
			if price > 0 && price <= 1.0 && (ask == 0 || price < ask) {
				ask = price
			}
		}

		if bid > 0 && ask > 0 && !realbotHasSaneTopOfBook(bid, ask) {
			return false
		}

		outcome := args.tokenToOutcome[book.AssetID]
		if outcome == "" {
			return false
		}
		updatedAt := realbotQuoteTimestampOrNow(book.Timestamp)
		if realbotShouldSkipStaleQuoteUpdate(args.quoteState, outcome, updatedAt, args.tokenBids[outcome], args.tokenAsks[outcome]) {
			return false
		}
		args.tokenBids[outcome] = bid
		args.tokenAsks[outcome] = ask
		if bid > 0 && ask > 0 {
			mid := (bid + ask) / 2
			args.engine.UpdateMarketData(args.marketID, outcome, mid, bid, ask)
			args.polySignalTracker.Record(outcome, bid, ask, updatedAt)
		}
		args.tokenFullBids[outcome] = mkt.LevelsToPriceDepth(book.Bids, true)
		args.tokenFullAsks[outcome] = mkt.LevelsToPriceDepth(book.Asks, false)
		args.quoteState[outcome] = realbotQuoteState{UpdatedAt: updatedAt, Source: "ws"}
		realbotSyncPairUpdate(args.outcomes, args.tokenBids, args.tokenAsks, lastPairUpdate, updatedAt)
		return true
	}

	return false
}

func realbotProcessMarketQuotes(args realbotMarketQuoteArgs, runtime realbotMarketQuoteRuntime) bool {
	_, _, reconnects, _ := args.wsMgr.GetStats()
	if reconnects > *runtime.lastReconnectCount {
		args.tui.LogEventDedup(fmt.Sprintf("ws-reconnected:%d", reconnects), 3*time.Second,
			"🔄 WebSocket reconnected (attempt #%d)", reconnects)
		*runtime.lastReconnectCount = reconnects
		*runtime.lastReconnectTime = time.Now()
		*runtime.wsChannelClosed = false

		for _, outcome := range args.outcomes {
			args.tokenBids[outcome] = 0
			args.tokenAsks[outcome] = 0
			args.tokenFullBids[outcome] = nil
			args.tokenFullAsks[outcome] = nil
			delete(args.quoteState, outcome)
		}
		realbotClearEngineMarketQuotes(args.engine, args.marketID, args.outcomes)
	}

	drained := false
	depthDirty := false
	quotesSanitized := false
	telemetryNow := time.Time{}
	wsMessagesProcessed := 0
	for !drained {
		select {
		case msg, ok := <-args.wsMsgChan:
			if !ok {
				select {
				case <-args.ctx.Done():
					args.tui.LogEvent("[%s] ⚠️ WS closed (context cancelled)", args.marketID)
					return true
				default:
					*runtime.wsChannelClosed = true
					drained = true
					continue
				}
			}
			wsMessagesProcessed++
			*runtime.wsChannelClosed = false
			if telemetryNow.IsZero() {
				telemetryNow = time.Now()
			}
			if realbotHandleMarketWSMessage(args, msg, runtime.lastPairUpdate) {
				depthDirty = true
			}
		default:
			drained = true
		}
	}
	if runtime.metrics != nil {
		runtime.metrics.observeWSMessages(wsMessagesProcessed, len(args.wsMsgChan))
	}

	for _, outcome := range args.outcomes {
		if args.tokenBids[outcome] > 0 && args.tokenAsks[outcome] > 0 && !realbotHasSaneTopOfBook(args.tokenBids[outcome], args.tokenAsks[outcome]) {
			args.tokenBids[outcome] = 0
			args.tokenAsks[outcome] = 0
			if len(args.tokenFullBids[outcome]) > 0 || len(args.tokenFullAsks[outcome]) > 0 {
				depthDirty = true
			}
			args.tokenFullBids[outcome] = nil
			args.tokenFullAsks[outcome] = nil
			quotesSanitized = true
		}
	}
	if realbotShouldClearLocalPairQuotes(args.outcomes, args.tokenBids, args.tokenAsks) {
		for _, outcome := range args.outcomes {
			if args.tokenBids[outcome] != 0 || args.tokenAsks[outcome] != 0 || len(args.tokenFullBids[outcome]) > 0 || len(args.tokenFullAsks[outcome]) > 0 {
				depthDirty = true
				quotesSanitized = true
			}
			args.tokenBids[outcome] = 0
			args.tokenAsks[outcome] = 0
			args.tokenFullBids[outcome] = nil
			args.tokenFullAsks[outcome] = nil
		}
	}
	realbotSyncDisplayQuotes(args.outcomes, args.tokenBids, args.tokenAsks, args.displayBids, args.displayAsks, false)

	wsTimeSinceMsg := args.wsMgr.TimeSinceLastDataMessage()
	now := time.Now()
	if runtime.lastTelemetryUpdate != nil {
		shouldUpdateTelemetry := runtime.lastTelemetryUpdate.IsZero() || now.Sub(*runtime.lastTelemetryUpdate) >= 250*time.Millisecond
		if shouldUpdateTelemetry {
			args.tui.UpdateWSLatency(wsTimeSinceMsg)
			args.tui.UpdateWSPingLatency(args.wsMgr.PingLatency())
			*runtime.lastTelemetryUpdate = now
		}
	}
	terminalBookState := realbotLooksLikeTerminalBook(args.outcomes, args.tokenBids, args.tokenAsks)
	pairQuoteAge := realbotPairQuoteAge(*runtime.lastPairUpdate, now)
	needsWSReconnect := realbotShouldReconnectWS(args.outcomes, args.tokenBids, args.tokenAsks, pairQuoteAge, args.restFallbackQuoteAge, terminalBookState)
	shouldRestFallback := realbotShouldPollRestFallback(*runtime.lastPairUpdate, *runtime.lastRestFallbackPoll, now, args.restFallbackQuoteAge, args.restFallbackPollPeriod, terminalBookState)

	if shouldRestFallback {
		if runtime.metrics != nil {
			runtime.metrics.observeRestFallback()
		}
		wasFallbackActive := *runtime.restFallbackActive
		*runtime.restFallbackActive = true
		recovered, fallbackDepthDirty := handleRestFallbackWithDepth(args.ctx, args.marketID, pairQuoteAge, args.tokenMap, args.tokenBids, args.tokenAsks, args.displayBids, args.displayAsks, args.tokenFullBids, args.tokenFullAsks, args.quoteState, runtime.lastPairUpdate, args.polySignalTracker, args.engine, args.restClient, args.tui, wasFallbackActive && !*runtime.restRecoveryLogged)
		if fallbackDepthDirty {
			depthDirty = true
		}
		*runtime.lastRestFallbackPoll = time.Now()
		if recovered {
			*runtime.restFallbackActive = false
			*runtime.restRecoveryLogged = false
		} else if pairQuoteAge >= 10*time.Second {
			*runtime.restRecoveryLogged = true
		}
	} else {
		*runtime.restFallbackActive = false
		*runtime.restRecoveryLogged = false
	}

	if depthDirty || quotesSanitized {
		bidDepth := make(map[string][]paper.MarketLevel)
		askDepth := make(map[string][]paper.MarketLevel)
		for _, outcome := range args.outcomes {
			if bids, ok := args.tokenFullBids[outcome]; ok {
				bidDepth[outcome] = append([]paper.MarketLevel(nil), bids...)
			}
			if asks, ok := args.tokenFullAsks[outcome]; ok {
				askDepth[outcome] = append([]paper.MarketLevel(nil), asks...)
			}
		}
		args.tui.UpdateOrderBookDepth(args.marketID, bidDepth, askDepth)
	}

	quotesChanged := !realbotQuoteMapsEqual(args.outcomes, args.displayBids, args.displayAsks, args.publishedBids, args.publishedAsks)
	latestQuoteAt, latestQuoteSource := realbotLatestQuoteUpdate(args.outcomes, args.quoteState)
	displayUsable := realbotDisplayHasUsableQuotes(args.outcomes, args.displayBids, args.displayAsks)
	freshnessAdvanced := displayUsable && !latestQuoteAt.IsZero() && latestQuoteAt.After(*runtime.lastPublishedQuoteAt)
	if quotesChanged || freshnessAdvanced {
		args.tui.UpdateMarketPricesWithSourceAt(args.marketID, args.displayBids, args.displayAsks, realbotNormalizeDisplaySource(latestQuoteSource), latestQuoteAt)
		realbotStorePublishedQuotes(args.outcomes, args.displayBids, args.displayAsks, args.publishedBids, args.publishedAsks)
		if runtime.metrics != nil {
			runtime.metrics.observeQuoteUpdate()
		}
		if freshnessAdvanced {
			*runtime.lastPublishedQuoteAt = latestQuoteAt
		}
	}

	if needsWSReconnect && args.wsMgr.IsConnected() && !*runtime.wsChannelClosed && time.Since(*runtime.lastForceReconnect) > realbotWSForceReconnect {
		*runtime.lastForceReconnect = time.Now()
		args.wsMgr.ForceReconnect()
		if time.Since(*runtime.lastWsWarnTime) > realbotWSWarnInterval {
			args.tui.LogEvent("[%s] 🔄 WS local book invalid - reconnecting...", args.marketID)
			*runtime.lastWsWarnTime = time.Now()
		}
	}

	if !args.wsMgr.IsConnected() && !*runtime.wsChannelClosed && time.Since(*runtime.lastForceReconnect) > realbotWSForceReconnect {
		*runtime.lastForceReconnect = time.Now()
		args.wsMgr.ForceReconnect()
		if time.Since(*runtime.lastWsWarnTime) > realbotWSWarnInterval {
			args.tui.LogEvent("[%s] 🔌 WS disconnected - reconnecting...", args.marketID)
			*runtime.lastWsWarnTime = time.Now()
		}
	}

	if *runtime.wsChannelClosed && time.Since(*runtime.lastWsWarnTime) > realbotWSWarnInterval {
		args.tui.LogEvent("[%s] ⚠️ WebSocket closed - attempting reconnect", args.marketID)
		*runtime.lastWsWarnTime = time.Now()
		*runtime.lastForceReconnect = time.Now()
		args.wsMgr.ForceReconnect()
	}

	return false
}

func realbotQuoteTimestampOrNow(raw string) time.Time {
	ts, err := api.ParseOrderBookTimestamp(raw)
	if err != nil || ts.IsZero() {
		return time.Now()
	}
	return ts
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

func realbotShouldSkipStaleQuoteUpdate(quoteState map[string]realbotQuoteState, outcome string, updatedAt time.Time, currentBid, currentAsk float64) bool {
	if updatedAt.IsZero() {
		return false
	}
	state, ok := quoteState[outcome]
	if !ok || state.UpdatedAt.IsZero() {
		return false
	}
	if !updatedAt.Before(state.UpdatedAt) {
		return false
	}
	return realbotHasSaneTopOfBook(currentBid, currentAsk)
}

func realbotQuoteMapsEqual(outcomes []string, bidsA, asksA, bidsB, asksB map[string]float64) bool {
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

func realbotShouldClearLocalPairQuotes(outcomes []string, bids, asks map[string]float64) bool {
	if realbotHasSanePairQuotes(outcomes, bids, asks) || realbotLooksLikeTerminalBook(outcomes, bids, asks) {
		return false
	}
	if realbotPairHasHighBid(outcomes, bids) {
		return false
	}
	return true
}

func realbotStorePublishedQuotes(outcomes []string, srcBids, srcAsks, dstBids, dstAsks map[string]float64) {
	for _, outcome := range outcomes {
		dstBids[outcome] = srcBids[outcome]
		dstAsks[outcome] = srcAsks[outcome]
	}
}

func realbotLatestQuoteUpdate(outcomes []string, quoteState map[string]realbotQuoteState) (time.Time, string) {
	var latest time.Time
	source := ""
	for _, outcome := range outcomes {
		state := quoteState[outcome]
		if state.UpdatedAt.After(latest) {
			latest = state.UpdatedAt
			source = state.Source
		}
	}
	return latest, source
}

func realbotNormalizeDisplaySource(raw string) string {
	source := strings.ToUpper(strings.TrimSpace(raw))
	switch {
	case strings.HasPrefix(source, "REST"):
		return "REST"
	case strings.HasPrefix(source, "WS"):
		return "WS"
	default:
		return "WS"
	}
}

func realbotDisplayHasUsableQuotes(outcomes []string, bids, asks map[string]float64) bool {
	return realbotHasSanePairQuotes(outcomes, bids, asks) || realbotLooksLikeTerminalBook(outcomes, bids, asks)
}

func realbotSyncDisplayQuotes(outcomes []string, liveBids, liveAsks, displayBids, displayAsks map[string]float64, authoritative bool) bool {
	nextBids := make(map[string]float64, len(outcomes))
	nextAsks := make(map[string]float64, len(outcomes))
	for _, outcome := range outcomes {
		nextBids[outcome] = displayBids[outcome]
		nextAsks[outcome] = displayAsks[outcome]
	}

	switch {
	case realbotHasSanePairQuotes(outcomes, liveBids, liveAsks):
		realbotStorePublishedQuotes(outcomes, liveBids, liveAsks, nextBids, nextAsks)
	case realbotLooksLikeTerminalBook(outcomes, liveBids, liveAsks):
		for _, outcome := range outcomes {
			if liveBids[outcome] > 0 {
				nextBids[outcome] = liveBids[outcome]
			}
			if liveAsks[outcome] > 0 {
				nextAsks[outcome] = liveAsks[outcome]
			}
		}
	case authoritative:
		realbotStorePublishedQuotes(outcomes, liveBids, liveAsks, nextBids, nextAsks)
	default:
		return false
	}

	if realbotQuoteMapsEqual(outcomes, nextBids, nextAsks, displayBids, displayAsks) {
		return false
	}
	realbotStorePublishedQuotes(outcomes, nextBids, nextAsks, displayBids, displayAsks)
	return true
}

func realbotPairQuoteAge(lastPairUpdate, now time.Time) time.Duration {
	if now.IsZero() {
		now = time.Now()
	}
	if lastPairUpdate.IsZero() {
		return time.Duration(1 << 62)
	}
	return now.Sub(lastPairUpdate)
}

func realbotShouldUseLocalPair(outcomes []string, tokenBids, tokenAsks map[string]float64, lastPairUpdate time.Time, maxAge time.Duration, now time.Time) bool {
	return realbotHasSanePairQuotes(outcomes, tokenBids, tokenAsks) && realbotPairQuoteAge(lastPairUpdate, now) <= maxAge
}

func realbotShouldPollRestFallback(lastPairUpdate, lastRestPoll, now time.Time, restFallbackQuoteAge, restFallbackPollInterval time.Duration, terminalBookState bool) bool {
	if terminalBookState {
		return false
	}
	if realbotPairQuoteAge(lastPairUpdate, now) < restFallbackQuoteAge {
		return false
	}
	return lastRestPoll.IsZero() || now.Sub(lastRestPoll) >= restFallbackPollInterval
}

func realbotSyncPairUpdate(outcomes []string, tokenBids, tokenAsks map[string]float64, lastPairUpdate *time.Time, now time.Time) {
	if lastPairUpdate == nil {
		return
	}
	if realbotHasSanePairQuotes(outcomes, tokenBids, tokenAsks) {
		*lastPairUpdate = now
	}
}

func handleRestFallbackWithDepth(ctx context.Context, id string, staleTime time.Duration, tokenMap map[string]string, bids, asks, displayBids, displayAsks map[string]float64, fullBids, fullAsks map[string][]paper.MarketLevel, quoteState map[string]realbotQuoteState, lastPairUpdate *time.Time, polyTracker *paper.DirectionalSignalTracker, engine *paper.Engine, restClient *api.RestClient, tui *paper.TUI, logRecovery bool) (bool, bool) {
	success := false
	depthChanged := false
	staleSeconds := int(staleTime.Seconds())
	restErrors := 0
	restEmpty := 0
	var lastErr error
	outcomes := make([]string, 0, len(tokenMap))
	for _, outcome := range tokenMap {
		outcomes = append(outcomes, outcome)
	}
	type restBookFetchResult struct {
		outcome string
		book    *api.OrderBookResponse
		latency time.Duration
		err     error
	}
	results := make(chan restBookFetchResult, len(tokenMap))
	var wg sync.WaitGroup
	for tokenID, outcome := range tokenMap {
		wg.Add(1)
		go func(tokenID, outcome string) {
			defer wg.Done()
			start := time.Now()
			reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			book, err := restClient.GetOrderBook(reqCtx, tokenID)
			cancel()
			results <- restBookFetchResult{
				outcome: outcome,
				book:    book,
				latency: time.Since(start),
				err:     err,
			}
		}(tokenID, outcome)
	}
	wg.Wait()
	close(results)

	for result := range results {
		outcome := result.outcome
		book := result.book
		err := result.err
		tui.UpdateRestLatency(result.latency)
		if err != nil {
			restErrors++
			lastErr = err
			continue
		}
		if book == nil {
			restErrors++
			lastErr = errNilRestOrderBook
			continue
		}

		updatedAt := realbotQuoteTimestampOrNow(book.Timestamp)
		now := time.Now()
		if realbotShouldSkipStaleQuoteUpdate(quoteState, outcome, updatedAt, bids[outcome], asks[outcome]) {
			success = true
			if state, ok := quoteState[outcome]; ok {
				state.UpdatedAt = now
				quoteState[outcome] = state
			}
			continue
		}
		updatedAt = now
		if len(book.Bids) == 0 && len(book.Asks) == 0 {
			restEmpty++
			bids[outcome] = 0
			asks[outcome] = 0
			fullBids[outcome] = nil
			fullAsks[outcome] = nil
			quoteState[outcome] = realbotQuoteState{UpdatedAt: updatedAt, Source: "rest"}
			success = true
			depthChanged = true
			continue
		}

		bid, ask := 0.0, 0.0
		for _, b := range book.Bids {
			p, _ := strconv.ParseFloat(b.Price, 64)
			if p > 0 && p <= 1.0 && p > bid {
				bid = p
			}
		}
		for _, a := range book.Asks {
			p, _ := strconv.ParseFloat(a.Price, 64)
			if p > 0 && p <= 1.0 && (ask == 0 || p < ask) {
				ask = p
			}
		}

		if bid > 0 && ask > 0 && !realbotHasSaneTopOfBook(bid, ask) {
			bids[outcome] = 0
			asks[outcome] = 0
			fullBids[outcome] = nil
			fullAsks[outcome] = nil
			quoteState[outcome] = realbotQuoteState{UpdatedAt: updatedAt, Source: "rest"}
			success = true
			depthChanged = true
			continue
		}

		bids[outcome] = bid
		asks[outcome] = ask
		success = true

		if bid > 0 && ask > 0 {
			mid := (bid + ask) / 2
			engine.UpdateMarketData(id, outcome, mid, bid, ask)
			polyTracker.Record(outcome, bid, ask, updatedAt)
		}
		fullBids[outcome] = mkt.LevelsToPriceDepth(book.Bids, true)
		fullAsks[outcome] = mkt.LevelsToPriceDepth(book.Asks, false)
		quoteState[outcome] = realbotQuoteState{UpdatedAt: updatedAt, Source: "rest"}
		depthChanged = true
	}
	realbotSyncDisplayQuotes(outcomes, bids, asks, displayBids, displayAsks, true)
	if success && realbotShouldClearLocalPairQuotes(outcomes, bids, asks) {
		for _, outcome := range tokenMap {
			bids[outcome] = 0
			asks[outcome] = 0
			fullBids[outcome] = nil
			fullAsks[outcome] = nil
			quoteState[outcome] = realbotQuoteState{UpdatedAt: time.Now(), Source: "rest"}
		}
		depthChanged = true
	}
	if success {
		realbotSyncPairUpdate(outcomes, bids, asks, lastPairUpdate, time.Now())
	}
	if success {
		if logRecovery && staleSeconds >= 10 {
			tui.LogEvent("[%s] ✅ REST recovered after %ds", id, staleSeconds)
		}
	} else if restErrors > 0 {
		if staleSeconds%10 == 0 || staleSeconds == 10 {
			tui.LogEvent("[%s] ❌ REST fallback failed after %ds: %v", id, staleSeconds, lastErr)
		}
	} else if restEmpty == len(tokenMap) && len(tokenMap) > 0 {
		if staleSeconds%10 == 0 || staleSeconds == 10 {
			tui.LogEvent("[%s] 📭 REST returned empty books after %ds", id, staleSeconds)
		}
	}
	return success, depthChanged
}
