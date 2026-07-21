package main

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/paper"
)

func TestRealbotExecutionQuoteGuardAgeCapsAtAwaitingWindow(t *testing.T) {
	if got := realbotExecutionQuoteGuardAge(3 * time.Second); got != realbotExecutionGuardQuoteMaxAge {
		t.Fatalf("expected execution guard to cap at %s, got %s", realbotExecutionGuardQuoteMaxAge, got)
	}
}

func TestRealbotExecutionQuoteGuardAgePreservesStricterConfig(t *testing.T) {
	if got := realbotExecutionQuoteGuardAge(500 * time.Millisecond); got != 500*time.Millisecond {
		t.Fatalf("expected stricter configured age to pass through, got %s", got)
	}
}

func TestRealbotClampBuySharesToBudgetUsesCapPriceNotObservedQuote(t *testing.T) {
	shares := realbotClampBuySharesToBudget(2, 2.0, 0.50, 0.51)
	if math.Abs(shares-1.98) > 0.000001 {
		t.Fatalf("expected clamp to 1.9800 shares, got %.4f", shares)
	}
}

func TestRealbotShouldSkipStaleQuoteUpdateOnlyWhenCurrentQuoteIsAlreadySane(t *testing.T) {
	now := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	quoteState := map[string]realbotQuoteState{
		"Up": {UpdatedAt: now, Source: "ws"},
	}

	if !realbotShouldSkipStaleQuoteUpdate(quoteState, "Up", now.Add(-250*time.Millisecond), 0.45, 0.46) {
		t.Fatal("expected stale update to be ignored when current quote is already sane")
	}
	if realbotShouldSkipStaleQuoteUpdate(quoteState, "Up", now.Add(-250*time.Millisecond), 0, 0) {
		t.Fatal("expected stale update to be allowed when current quote is unusable")
	}
}

func TestRealbotShouldClearLocalPairQuotesKeepsTerminalOneSidedBook(t *testing.T) {
	outcomes := []string{"Down", "Up"}
	bids := map[string]float64{"Down": 0.99, "Up": 0}
	asks := map[string]float64{"Down": 0, "Up": 0.01}

	if realbotShouldClearLocalPairQuotes(outcomes, bids, asks) {
		t.Fatal("expected terminal-looking one-sided book to remain available locally")
	}
}

func TestRealbotSyncDisplayQuotesIgnoresTransientNonTerminalWSGap(t *testing.T) {
	outcomes := []string{"Down", "Up"}
	displayBids := map[string]float64{"Down": 0.44, "Up": 0.54}
	displayAsks := map[string]float64{"Down": 0.45, "Up": 0.55}
	liveBids := map[string]float64{"Down": 0, "Up": 0.54}
	liveAsks := map[string]float64{"Down": 0.45, "Up": 0}

	if realbotSyncDisplayQuotes(outcomes, liveBids, liveAsks, displayBids, displayAsks, false) {
		t.Fatal("expected transient WS gap to keep the existing display quotes")
	}
	if displayBids["Down"] != 0.44 || displayAsks["Up"] != 0.55 {
		t.Fatalf("expected prior display quotes to remain untouched, got bids=%v asks=%v", displayBids, displayAsks)
	}
}

func TestRealbotSyncDisplayQuotesPreservesTerminalDisplaySides(t *testing.T) {
	outcomes := []string{"Down", "Up"}
	displayBids := map[string]float64{"Down": 0.97, "Up": 0.02}
	displayAsks := map[string]float64{"Down": 0.98, "Up": 0.03}
	liveBids := map[string]float64{"Down": 0.99, "Up": 0}
	liveAsks := map[string]float64{"Down": 0, "Up": 0.01}

	if !realbotSyncDisplayQuotes(outcomes, liveBids, liveAsks, displayBids, displayAsks, false) {
		t.Fatal("expected terminal-looking update to refresh the display")
	}
	// With high-bid tolerance (Down bid 0.99 ≥ 0.60), the pair is now treated
	// as sane and all live values are published verbatim, including zero sides.
	// The terminal-book display path preserved prior quotes; the high-bid path
	// publishes the live snapshot directly.
	if displayBids["Down"] != 0.99 || displayAsks["Up"] != 0.01 {
		t.Fatalf("expected live terminal sides to be published, got bids=%v asks=%v", displayBids, displayAsks)
	}
}

func TestRealbotSyncDisplayQuotesLetsRESTClearBrokenPair(t *testing.T) {
	outcomes := []string{"Down", "Up"}
	displayBids := map[string]float64{"Down": 0.44, "Up": 0.54}
	displayAsks := map[string]float64{"Down": 0.45, "Up": 0.55}
	liveBids := map[string]float64{"Down": 0, "Up": 0}
	liveAsks := map[string]float64{"Down": 0, "Up": 0}

	if !realbotSyncDisplayQuotes(outcomes, liveBids, liveAsks, displayBids, displayAsks, true) {
		t.Fatal("expected authoritative REST update to clear the display")
	}
	if displayBids["Down"] != 0 || displayAsks["Up"] != 0 {
		t.Fatalf("expected display quotes to clear after REST confirmation, got bids=%v asks=%v", displayBids, displayAsks)
	}
}

func TestRealbotProcessMarketQuotesPublishesDisplayAndFreshness(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	tui.AddMarket("BTC", "btc", []string{"Down", "Up"}, time.Now().Add(time.Minute))

	now := time.Now()
	displayBids := make(map[string]float64)
	displayAsks := make(map[string]float64)
	publishedBids := make(map[string]float64)
	publishedAsks := make(map[string]float64)
	tokenBids := map[string]float64{"Down": 0.48, "Up": 0.51}
	tokenAsks := map[string]float64{"Down": 0.49, "Up": 0.52}
	tokenFullBids := map[string][]paper.MarketLevel{
		"Down": {{Price: 0.48, Size: 10}},
		"Up":   {{Price: 0.51, Size: 10}},
	}
	tokenFullAsks := map[string][]paper.MarketLevel{
		"Down": {{Price: 0.49, Size: 10}},
		"Up":   {{Price: 0.52, Size: 10}},
	}
	quoteState := map[string]realbotQuoteState{
		"Down": {UpdatedAt: now, Source: "ws"},
		"Up":   {UpdatedAt: now, Source: "ws"},
	}

	lastPairUpdate := now
	lastPublishedQuoteAt := time.Time{}
	lastReconnectCount := int32(0)
	lastWsWarnTime := now
	lastForceReconnect := now
	lastRestFallbackPoll := now
	restFallbackActive := false
	restRecoveryLogged := false
	wsChannelClosed := false

	if realbotProcessMarketQuotes(realbotMarketQuoteArgs{
		ctx:                    context.Background(),
		marketID:               "BTC",
		wsMgr:                  &api.WSManager{},
		wsMsgChan:              make(chan []byte, 1),
		tokenMap:               map[string]string{"down-token": "Down", "up-token": "Up"},
		tokenToOutcome:         map[string]string{"down-token": "Down", "up-token": "Up"},
		outcomes:               []string{"Down", "Up"},
		tokenBids:              tokenBids,
		tokenAsks:              tokenAsks,
		tokenFullBids:          tokenFullBids,
		tokenFullAsks:          tokenFullAsks,
		displayBids:            displayBids,
		displayAsks:            displayAsks,
		publishedBids:          publishedBids,
		publishedAsks:          publishedAsks,
		quoteState:             quoteState,
		polySignalTracker:      paper.NewDirectionalSignalTracker(time.Second, []string{"Down", "Up"}),
		engine:                 engine,
		restClient:             api.NewRestClient("polymarket"),
		tui:                    tui,
		restFallbackQuoteAge:   time.Minute,
		restFallbackPollPeriod: time.Minute,
	}, realbotMarketQuoteRuntime{
		lastPairUpdate:       &lastPairUpdate,
		lastPublishedQuoteAt: &lastPublishedQuoteAt,
		lastReconnectCount:   &lastReconnectCount,
		lastWsWarnTime:       &lastWsWarnTime,
		lastForceReconnect:   &lastForceReconnect,
		lastRestFallbackPoll: &lastRestFallbackPoll,
		restFallbackActive:   &restFallbackActive,
		restRecoveryLogged:   &restRecoveryLogged,
		wsChannelClosed:      &wsChannelClosed,
	}) {
		t.Fatal("expected active quote loop to continue running")
	}

	if math.Abs(displayBids["Down"]-0.48) > 0.000001 || math.Abs(displayAsks["Up"]-0.52) > 0.000001 {
		t.Fatalf("expected display quotes to publish sane pair, got bids=%v asks=%v", displayBids, displayAsks)
	}
	if math.Abs(publishedBids["Up"]-0.51) > 0.000001 || math.Abs(publishedAsks["Down"]-0.49) > 0.000001 {
		t.Fatalf("expected published quotes to track display, got bids=%v asks=%v", publishedBids, publishedAsks)
	}
	if lastPublishedQuoteAt.IsZero() {
		t.Fatal("expected fresh quote publication to advance lastPublishedQuoteAt")
	}
}

func TestRealbotProcessMarketQuotesDrainsQueuedWebSocketBurst(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	tui.AddMarket("BTC", "btc", []string{"Down", "Up"}, time.Now().Add(time.Minute))

	ch := make(chan []byte, 741)
	for i := 0; i < cap(ch); i++ {
		ch <- []byte(`{}`)
	}

	now := time.Now()
	lastPairUpdate := now
	lastPublishedQuoteAt := time.Time{}
	lastReconnectCount := int32(0)
	lastWsWarnTime := now
	lastForceReconnect := now
	lastRestFallbackPoll := now
	restFallbackActive := false
	restRecoveryLogged := false
	wsChannelClosed := false

	if realbotProcessMarketQuotes(realbotMarketQuoteArgs{
		ctx:            context.Background(),
		marketID:       "BTC",
		wsMgr:          &api.WSManager{},
		wsMsgChan:      ch,
		tokenMap:       map[string]string{"down-token": "Down", "up-token": "Up"},
		tokenToOutcome: map[string]string{"down-token": "Down", "up-token": "Up"},
		outcomes:       []string{"Down", "Up"},
		tokenBids:      map[string]float64{"Down": 0.48, "Up": 0.51},
		tokenAsks:      map[string]float64{"Down": 0.49, "Up": 0.52},
		tokenFullBids:  map[string][]paper.MarketLevel{},
		tokenFullAsks:  map[string][]paper.MarketLevel{},
		displayBids:    map[string]float64{},
		displayAsks:    map[string]float64{},
		publishedBids:  map[string]float64{},
		publishedAsks:  map[string]float64{},
		quoteState: map[string]realbotQuoteState{
			"Down": {UpdatedAt: now, Source: "ws"},
			"Up":   {UpdatedAt: now, Source: "ws"},
		},
		polySignalTracker:      paper.NewDirectionalSignalTracker(time.Second, []string{"Down", "Up"}),
		engine:                 engine,
		restClient:             api.NewRestClient("polymarket"),
		tui:                    tui,
		restFallbackQuoteAge:   time.Minute,
		restFallbackPollPeriod: time.Minute,
	}, realbotMarketQuoteRuntime{
		lastPairUpdate:       &lastPairUpdate,
		lastPublishedQuoteAt: &lastPublishedQuoteAt,
		lastReconnectCount:   &lastReconnectCount,
		lastWsWarnTime:       &lastWsWarnTime,
		lastForceReconnect:   &lastForceReconnect,
		lastRestFallbackPoll: &lastRestFallbackPoll,
		restFallbackActive:   &restFallbackActive,
		restRecoveryLogged:   &restRecoveryLogged,
		wsChannelClosed:      &wsChannelClosed,
	}) {
		t.Fatal("expected active quote loop to continue running")
	}

	if got := len(ch); got != 0 {
		t.Fatalf("expected one drain pass to consume the queued WS burst, got %d remaining", got)
	}
}

func TestRealbotProcessMarketQuotesClosedChannelSchedulesReconnect(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	tui.AddMarket("BTC", "btc", []string{"Down", "Up"}, time.Now().Add(time.Minute))

	ch := make(chan []byte)
	close(ch)

	lastPairUpdate := time.Now()
	lastPublishedQuoteAt := time.Time{}
	lastReconnectCount := int32(0)
	lastWsWarnTime := time.Now().Add(-2 * realbotWSWarnInterval)
	lastForceReconnect := time.Time{}
	lastRestFallbackPoll := time.Now()
	restFallbackActive := false
	restRecoveryLogged := false
	wsChannelClosed := false

	if realbotProcessMarketQuotes(realbotMarketQuoteArgs{
		ctx:                    context.Background(),
		marketID:               "BTC",
		wsMgr:                  &api.WSManager{},
		wsMsgChan:              ch,
		tokenMap:               map[string]string{"down-token": "Down", "up-token": "Up"},
		tokenToOutcome:         map[string]string{"down-token": "Down", "up-token": "Up"},
		outcomes:               []string{"Down", "Up"},
		tokenBids:              map[string]float64{"Down": 0.48, "Up": 0.51},
		tokenAsks:              map[string]float64{"Down": 0.49, "Up": 0.52},
		tokenFullBids:          map[string][]paper.MarketLevel{},
		tokenFullAsks:          map[string][]paper.MarketLevel{},
		displayBids:            map[string]float64{},
		displayAsks:            map[string]float64{},
		publishedBids:          map[string]float64{},
		publishedAsks:          map[string]float64{},
		quoteState:             map[string]realbotQuoteState{},
		polySignalTracker:      paper.NewDirectionalSignalTracker(time.Second, []string{"Down", "Up"}),
		engine:                 engine,
		restClient:             api.NewRestClient("polymarket"),
		tui:                    tui,
		restFallbackQuoteAge:   time.Minute,
		restFallbackPollPeriod: time.Minute,
	}, realbotMarketQuoteRuntime{
		lastPairUpdate:       &lastPairUpdate,
		lastPublishedQuoteAt: &lastPublishedQuoteAt,
		lastReconnectCount:   &lastReconnectCount,
		lastWsWarnTime:       &lastWsWarnTime,
		lastForceReconnect:   &lastForceReconnect,
		lastRestFallbackPoll: &lastRestFallbackPoll,
		restFallbackActive:   &restFallbackActive,
		restRecoveryLogged:   &restRecoveryLogged,
		wsChannelClosed:      &wsChannelClosed,
	}) {
		t.Fatal("expected closed-but-active channel to stay in retry loop rather than exit")
	}

	if !wsChannelClosed {
		t.Fatal("expected closed channel to mark wsChannelClosed")
	}
	if lastForceReconnect.IsZero() {
		t.Fatal("expected reconnect path to update lastForceReconnect")
	}
	if time.Since(lastWsWarnTime) > time.Second {
		t.Fatal("expected reconnect warning timestamp to refresh")
	}
}

func TestRealbotProcessMarketQuotesReturnsOnCancelledClosedChannel(t *testing.T) {
	ch := make(chan []byte)
	close(ch)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	lastPairUpdate := time.Now()
	lastPublishedQuoteAt := time.Time{}
	lastReconnectCount := int32(0)
	lastWsWarnTime := time.Now()
	lastForceReconnect := time.Now()
	lastRestFallbackPoll := time.Now()
	restFallbackActive := false
	restRecoveryLogged := false
	wsChannelClosed := false

	if !realbotProcessMarketQuotes(realbotMarketQuoteArgs{
		ctx:                    ctx,
		marketID:               "BTC",
		wsMgr:                  &api.WSManager{},
		wsMsgChan:              ch,
		tokenMap:               map[string]string{},
		tokenToOutcome:         map[string]string{},
		outcomes:               []string{"Down", "Up"},
		tokenBids:              map[string]float64{},
		tokenAsks:              map[string]float64{},
		tokenFullBids:          map[string][]paper.MarketLevel{},
		tokenFullAsks:          map[string][]paper.MarketLevel{},
		displayBids:            map[string]float64{},
		displayAsks:            map[string]float64{},
		publishedBids:          map[string]float64{},
		publishedAsks:          map[string]float64{},
		quoteState:             map[string]realbotQuoteState{},
		polySignalTracker:      paper.NewDirectionalSignalTracker(time.Second, []string{"Down", "Up"}),
		engine:                 paper.NewEngine(100),
		restClient:             api.NewRestClient("polymarket"),
		tui:                    paper.NewTUI(paper.NewEngine(100), nil),
		restFallbackQuoteAge:   time.Minute,
		restFallbackPollPeriod: time.Minute,
	}, realbotMarketQuoteRuntime{
		lastPairUpdate:       &lastPairUpdate,
		lastPublishedQuoteAt: &lastPublishedQuoteAt,
		lastReconnectCount:   &lastReconnectCount,
		lastWsWarnTime:       &lastWsWarnTime,
		lastForceReconnect:   &lastForceReconnect,
		lastRestFallbackPoll: &lastRestFallbackPoll,
		restFallbackActive:   &restFallbackActive,
		restRecoveryLogged:   &restRecoveryLogged,
		wsChannelClosed:      &wsChannelClosed,
	}) {
		t.Fatal("expected cancelled context with closed channel to exit quote loop")
	}
}

func TestComputeRealbotMakerProtectedSellQuoteIgnoresCostFloor(t *testing.T) {
	price, ok := computeRealbotMakerProtectedSellQuote(0.54, 0.60, 0.50, 0.02, 0, 0.008, 1000, time.Hour, realbotMakerStrategyParams)
	if !ok {
		t.Fatal("expected protected sell quote to exist when profitable")
	}
	if price < 0.52 {
		t.Fatalf("expected sell quote to respect cost floor, got %.3f", price)
	}
	// When at a loss and not overloaded (skew < 0.75), it should protect cost basis and return false
	if _, ok := computeRealbotMakerProtectedSellQuote(0.54, 0.56, 0.56, 0.02, 0, 0.008, 1000, time.Hour, realbotMakerStrategyParams); ok {
		t.Fatal("expected un-profitable sell quote without toxic skew to be rejected by cost floor")
	}
	// When severely overloaded (skew >= 0.75), panic dump mode bypasses cost floor
	if _, ok := computeRealbotMakerProtectedSellQuote(0.54, 0.56, 0.56, 0.02, 0.8, 0.008, 1000, time.Hour, realbotMakerStrategyParams); !ok {
		t.Fatal("expected overloaded bag (skew >= 0.75) to bypass cost floor and place dump sell quote")
	}
}

func TestComputeRealbotMakerSkewedQuoteRespectsConfiguredGap(t *testing.T) {
	tight, ok := computeRealbotMakerSkewedQuote(api.SideBuy, 0.47, 0.53, 0.0, 0.003, realbotMakerStrategyParams)
	if !ok {
		t.Fatal("expected tight maker buy quote")
	}
	wide, ok := computeRealbotMakerSkewedQuote(api.SideBuy, 0.47, 0.53, 0.0, 0.012, realbotMakerStrategyParams)
	if !ok {
		t.Fatal("expected wide maker buy quote")
	}
	if tight <= wide {
		t.Fatalf("expected tighter gap to quote closer to ask: tight=%.3f wide=%.3f", tight, wide)
	}
}

func TestRealbotShouldRunDecisionLoopPrioritizesNewQuotes(t *testing.T) {
	base := time.Unix(1000, 0)
	lastEval := base
	lastQuote := base
	latestQuote := base.Add(10 * time.Millisecond)

	if realbotShouldRunDecisionLoop(base.Add(25*time.Millisecond), lastEval, lastQuote, latestQuote, 100*time.Millisecond) {
		t.Fatal("expected new quotes to remain throttled before half interval")
	}
	if !realbotShouldRunDecisionLoop(base.Add(50*time.Millisecond), lastEval, lastQuote, latestQuote, 100*time.Millisecond) {
		t.Fatal("expected new quote at half interval to trigger loop")
	}
	if realbotShouldRunDecisionLoop(base.Add(50*time.Millisecond), lastEval, latestQuote, latestQuote, 100*time.Millisecond) {
		t.Fatal("expected no new quote inside interval to be throttled")
	}
	if !realbotShouldRunDecisionLoop(base.Add(100*time.Millisecond), lastEval, latestQuote, latestQuote, 100*time.Millisecond) {
		t.Fatal("expected decision loop to run once interval elapses even without new quotes")
	}
}

func TestRealbotCanUseLocalBuyQuote(t *testing.T) {
	now := time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC)
	outcomes := []string{"Down", "Up"}
	bids := map[string]float64{"Down": 0.35, "Up": 0.63}
	asks := map[string]float64{"Down": 0.36, "Up": 0.64}
	depth := map[string][]paper.MarketLevel{
		"Down": {{Price: 0.36, Size: 10}},
		"Up":   {{Price: 0.64, Size: 8}},
	}
	lastPairUpdate := now.Add(-70 * time.Millisecond)

	fresh, age, reason := realbotCanUseLocalBuyQuote(now, outcomes, bids, asks, depth, lastPairUpdate, 250*time.Millisecond)
	if !fresh || reason != "" {
		t.Fatalf("expected fresh local quote, got fresh=%v age=%v reason=%q", fresh, age, reason)
	}

	lastPairUpdate = now.Add(-400 * time.Millisecond)
	fresh, _, reason = realbotCanUseLocalBuyQuote(now, outcomes, bids, asks, depth, lastPairUpdate, 250*time.Millisecond)
	if fresh || reason == "" {
		t.Fatalf("expected stale quote rejection, got fresh=%v reason=%q", fresh, reason)
	}
}

func TestRealbotEnsureFreshBuyExecutionQuoteFallsBackToREST(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/book" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		ts := time.Now().UTC().Format(time.RFC3339Nano)
		switch r.URL.Query().Get("token_id") {
		case "down-token":
			_, _ = w.Write([]byte("{\"asset_id\":\"down-token\",\"timestamp\":\"" + ts + "\",\"bids\":[{\"price\":\"0.34\",\"size\":\"7\"}],\"asks\":[{\"price\":\"0.35\",\"size\":\"9\"}]}"))
		case "up-token":
			_, _ = w.Write([]byte("{\"asset_id\":\"up-token\",\"timestamp\":\"" + ts + "\",\"bids\":[{\"price\":\"0.61\",\"size\":\"8\"}],\"asks\":[{\"price\":\"0.62\",\"size\":\"10\"}]}"))
		default:
			http.Error(w, "unexpected token: "+r.URL.String(), http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := api.NewRestClient("polymarket")
	client.BaseURL = server.URL
	market := &api.Market{Tokens: []api.Token{{TokenID: "down-token", Outcome: "Down"}, {TokenID: "up-token", Outcome: "Up"}}}
	outcomes := []string{"Down", "Up"}
	tokenBids := map[string]float64{"Down": 0.20, "Up": 0.60}
	tokenAsks := map[string]float64{"Down": 0.31, "Up": 0.61}
	tokenFullBids := map[string][]paper.MarketLevel{}
	tokenFullAsks := map[string][]paper.MarketLevel{}
	quoteState := map[string]realbotQuoteState{
		"Down": {UpdatedAt: time.Now().Add(-10 * time.Second), Source: "ws"},
		"Up":   {UpdatedAt: time.Now().Add(-10 * time.Second), Source: "ws"},
	}
	lastPairUpdate := time.Time{}

	source, _, detail, err := realbotEnsureFreshBuyExecutionQuote(context.Background(), client, market, outcomes, tokenBids, tokenAsks, tokenFullBids, tokenFullAsks, quoteState, lastPairUpdate, 250*time.Millisecond, &lastPairUpdate)
	if err != nil {
		t.Fatalf("expected REST refresh to succeed, got %v", err)
	}
	if source != "rest" {
		t.Fatalf("expected REST source, got %q", source)
	}
	if strings.TrimSpace(detail) == "" {
		t.Fatalf("expected stale-local detail, got %q", detail)
	}
	if math.Abs(tokenAsks["Down"]-0.35) > 0.000001 || math.Abs(tokenAsks["Up"]-0.62) > 0.000001 {
		t.Fatalf("expected refreshed asks, got Down=%.3f Up=%.3f", tokenAsks["Down"], tokenAsks["Up"])
	}
	if len(tokenFullAsks["Down"]) == 0 || len(tokenFullAsks["Up"]) == 0 {
		t.Fatalf("expected refreshed ask depth, got Down=%d Up=%d", len(tokenFullAsks["Down"]), len(tokenFullAsks["Up"]))
	}
	if quoteState["Down"].Source != "rest-exec" || quoteState["Up"].Source != "rest-exec" {
		t.Fatalf("expected refreshed quote source, got Down=%q Up=%q", quoteState["Down"].Source, quoteState["Up"].Source)
	}
}

func TestRealbotCanUseLocalTakerCloseQuoteAcceptsFreshWSAsk(t *testing.T) {
	now := time.Date(2026, 3, 21, 10, 0, 0, 0, time.UTC)
	bids := map[string]float64{"Up": 0.82}
	asks := map[string]float64{"Up": 0.83}
	depth := map[string][]paper.MarketLevel{
		"Up": {{Price: 0.83, Size: 12}},
	}
	state := map[string]realbotQuoteState{
		"Up": {UpdatedAt: now.Add(-120 * time.Millisecond), Source: "ws"},
	}

	price, reason, ok := realbotCanUseLocalTakerCloseQuote(now, "Up", bids, asks, depth, state, 350*time.Millisecond)
	if !ok {
		t.Fatalf("expected fresh WS taker-close quote, got reason=%q", reason)
	}
	if math.Abs(price-0.83) > 0.000001 {
		t.Fatalf("expected local confirm price 0.83, got %.3f", price)
	}
}

func TestRealbotCanUseLocalTakerCloseQuoteRejectsNonWSOrStaleQuote(t *testing.T) {
	now := time.Date(2026, 3, 21, 10, 0, 0, 0, time.UTC)
	bids := map[string]float64{"Up": 0.82}
	asks := map[string]float64{"Up": 0.83}
	depth := map[string][]paper.MarketLevel{
		"Up": {{Price: 0.83, Size: 12}},
	}

	price, reason, ok := realbotCanUseLocalTakerCloseQuote(now, "Up", bids, asks, depth, map[string]realbotQuoteState{
		"Up": {UpdatedAt: now.Add(-100 * time.Millisecond), Source: "rest-exec"},
	}, 350*time.Millisecond)
	if ok || price != 0 || !strings.Contains(reason, "not aggressive-safe") {
		t.Fatalf("expected rest-exec quote rejection, got ok=%v price=%.3f reason=%q", ok, price, reason)
	}

	price, reason, ok = realbotCanUseLocalTakerCloseQuote(now, "Up", bids, asks, depth, map[string]realbotQuoteState{
		"Up": {UpdatedAt: now.Add(-500 * time.Millisecond), Source: "ws"},
	}, 350*time.Millisecond)
	if ok || price != 0 || !strings.Contains(reason, "quote age") {
		t.Fatalf("expected stale quote rejection, got ok=%v price=%.3f reason=%q", ok, price, reason)
	}
}

func TestHandleRestFallbackWithDepthSkipsOlderBooksWhenCurrentQuoteIsFresh(t *testing.T) {
	staleTS := time.Now().Add(-2 * time.Second).UTC().Format(time.RFC3339Nano)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("token_id") {
		case "down-token":
			_, _ = w.Write([]byte("{\"asset_id\":\"down-token\",\"timestamp\":\"" + staleTS + "\",\"bids\":[{\"price\":\"0.34\",\"size\":\"7\"}],\"asks\":[{\"price\":\"0.35\",\"size\":\"9\"}]}"))
		case "up-token":
			_, _ = w.Write([]byte("{\"asset_id\":\"up-token\",\"timestamp\":\"" + staleTS + "\",\"bids\":[{\"price\":\"0.61\",\"size\":\"8\"}],\"asks\":[{\"price\":\"0.62\",\"size\":\"10\"}]}"))
		default:
			http.Error(w, "unexpected token", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := api.NewRestClient("polymarket")
	client.BaseURL = server.URL
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	tui.AddMarket("SOL", "sol", []string{"Down", "Up"}, time.Now().Add(time.Minute))

	bids := map[string]float64{"Down": 0.40, "Up": 0.58}
	asks := map[string]float64{"Down": 0.41, "Up": 0.59}
	fullBids := map[string][]paper.MarketLevel{}
	fullAsks := map[string][]paper.MarketLevel{}
	quoteState := map[string]realbotQuoteState{
		"Down": {UpdatedAt: time.Now(), Source: "ws"},
		"Up":   {UpdatedAt: time.Now(), Source: "ws"},
	}

	ok, _ := handleRestFallbackWithDepth(context.Background(), "SOL", 12*time.Second, map[string]string{
		"down-token": "Down",
		"up-token":   "Up",
	}, bids, asks, map[string]float64{}, map[string]float64{}, fullBids, fullAsks, quoteState, nil, nil, engine, client, tui, false)
	if !ok {
		t.Fatal("expected fallback call to complete")
	}
	if math.Abs(bids["Down"]-0.40) > 0.000001 || math.Abs(asks["Up"]-0.59) > 0.000001 {
		t.Fatalf("expected stale REST data to be ignored, got bids=%v asks=%v", bids, asks)
	}
}

func TestHandleRestFallbackWithDepthPreservesDisplayForOneSidedBooks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("token_id") {
		case "down-token":
			_, _ = w.Write([]byte(`{"asset_id":"down-token","timestamp":"2026-03-20T00:00:00Z","bids":[{"price":"0.99","size":"12"}],"asks":[]}`))
		case "up-token":
			_, _ = w.Write([]byte(`{"asset_id":"up-token","timestamp":"2026-03-20T00:00:00Z","bids":[],"asks":[{"price":"0.02","size":"8"}]}`))
		default:
			http.Error(w, "unexpected token", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := api.NewRestClient("polymarket")
	client.BaseURL = server.URL
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	tui.AddMarket("BTC", "btc", []string{"Down", "Up"}, time.Now().Add(time.Minute))

	bids := map[string]float64{}
	asks := map[string]float64{}
	displayBids := map[string]float64{}
	displayAsks := map[string]float64{}
	fullBids := map[string][]paper.MarketLevel{}
	fullAsks := map[string][]paper.MarketLevel{}
	quoteState := map[string]realbotQuoteState{}

	ok, _ := handleRestFallbackWithDepth(context.Background(), "BTC", 12*time.Second, map[string]string{
		"down-token": "Down",
		"up-token":   "Up",
	}, bids, asks, displayBids, displayAsks, fullBids, fullAsks, quoteState, nil, nil, engine, client, tui, false)
	if !ok {
		t.Fatal("expected fallback call to complete")
	}
	// With high-bid tolerance (Down bid 0.99 ≥ 0.60), one-sided books are
	// preserved rather than pair-cleared. This matches real market behavior
	// at extreme prices where the complement side has sparse liquidity.
	if bids["Down"] != 0.99 {
		t.Fatalf("expected high-bid side to be preserved, got bids=%v", bids)
	}
	if math.Abs(displayBids["Down"]-0.99) > 0.000001 {
		t.Fatalf("expected display bid to preserve one-sided quote, got %.3f", displayBids["Down"])
	}
	if math.Abs(displayAsks["Up"]-0.02) > 0.000001 {
		t.Fatalf("expected display ask to preserve one-sided quote, got %.3f", displayAsks["Up"])
	}
}

func TestRealbotCanUseLocalSellQuote(t *testing.T) {
	now := time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC)
	outcomes := []string{"Down", "Up"}
	bids := map[string]float64{"Down": 0.54, "Up": 0.49}
	asks := map[string]float64{"Down": 0.55, "Up": 0.50}
	depth := map[string][]paper.MarketLevel{
		"Down": {{Price: 0.54, Size: 8}},
		"Up":   {{Price: 0.49, Size: 10}},
	}
	lastPairUpdate := now.Add(-70 * time.Millisecond)

	fresh, age, reason := realbotCanUseLocalSellQuote(now, outcomes, bids, asks, depth, lastPairUpdate, 250*time.Millisecond)
	if !fresh || reason != "" || age != 70*time.Millisecond {
		t.Fatalf("expected fresh local sell quote, got fresh=%v age=%v reason=%q", fresh, age, reason)
	}

	lastPairUpdate = now.Add(-400 * time.Millisecond)
	fresh, _, reason = realbotCanUseLocalSellQuote(now, outcomes, bids, asks, depth, lastPairUpdate, 250*time.Millisecond)
	if fresh || reason == "" {
		t.Fatalf("expected stale sell quote rejection, got fresh=%v reason=%q", fresh, reason)
	}
}

func TestRealbotBuildCleanupSellQuoteKeepsConfiguredFloor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/book" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		ts := time.Now().UTC().Format(time.RFC3339Nano)
		_, _ = w.Write([]byte("{\"asset_id\":\"token-up\",\"timestamp\":\"" + ts + "\",\"bids\":[{\"price\":\"0.54\",\"size\":\"7\"}],\"asks\":[{\"price\":\"0.55\",\"size\":\"4\"}]}"))
	}))
	defer server.Close()

	client := api.NewRestClient("polymarket")
	client.BaseURL = server.URL

	_, err := realbotBuildCleanupSellQuote(context.Background(), client, "token-up", 2, 0.60)
	if err == nil {
		t.Fatal("expected cleanup quote to reject bids below the configured floor")
	}
	if !strings.Contains(err.Error(), "below") {
		t.Fatalf("expected floor-liquidity rejection, got %v", err)
	}
}

func TestRealbotBuildCleanupSellQuoteUsesLiquidityAtConfiguredFloor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/book" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		ts := time.Now().UTC().Format(time.RFC3339Nano)
		_, _ = w.Write([]byte("{\"asset_id\":\"token-up\",\"timestamp\":\"" + ts + "\",\"bids\":[{\"price\":\"0.60\",\"size\":\"1.25\"},{\"price\":\"0.59\",\"size\":\"8\"}],\"asks\":[{\"price\":\"0.61\",\"size\":\"4\"}]}"))
	}))
	defer server.Close()

	client := api.NewRestClient("polymarket")
	client.BaseURL = server.URL

	quote, err := realbotBuildCleanupSellQuote(context.Background(), client, "token-up", 2, 0.60)
	if err != nil {
		t.Fatalf("expected cleanup quote at configured floor to succeed, got %v", err)
	}
	if math.Abs(quote.SubmitPrice-0.60) > 0.000001 {
		t.Fatalf("expected cleanup submit price 0.60, got %.3f", quote.SubmitPrice)
	}
	if math.Abs(quote.TotalBidLiquidity-1.25) > 0.000001 {
		t.Fatalf("expected only floor-respecting liquidity to count, got %.2f", quote.TotalBidLiquidity)
	}
	if math.Abs(quote.ExecutableQty-1.25) > 0.000001 {
		t.Fatalf("expected executable qty to cap at floor liquidity 1.25, got %.2f", quote.ExecutableQty)
	}
}

func TestRealbotLocalQuoteSanityReasonRejectsWideOutcomeSpread(t *testing.T) {
	reason := realbotLocalQuoteSanityReason(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0.40, "Up": 0.56},
		map[string]float64{"Down": 0.57, "Up": 0.60},
	)
	if reason == "" || !strings.Contains(reason, "wide local spread") {
		t.Fatalf("expected wide-spread rejection, got %q", reason)
	}
}

func TestRealbotLocalQuoteSanityReasonRejectsImpossiblePairSum(t *testing.T) {
	reason := realbotLocalQuoteSanityReason(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0.46, "Up": 0.47},
		map[string]float64{"Down": 0.55, "Up": 0.56},
	)
	if reason == "" || !strings.Contains(reason, "ask pair sum") {
		t.Fatalf("expected pair-sum rejection, got %q", reason)
	}
}

func TestRealbotLatestQuoteUpdateReturnsFreshestState(t *testing.T) {
	outcomes := []string{"Down", "Up"}
	base := time.Date(2026, 3, 21, 10, 0, 0, 0, time.UTC)
	state := map[string]realbotQuoteState{
		"Down": {UpdatedAt: base.Add(150 * time.Millisecond), Source: "ws"},
		"Up":   {UpdatedAt: base.Add(350 * time.Millisecond), Source: "rest"},
	}

	updatedAt, source := realbotLatestQuoteUpdate(outcomes, state)
	if !updatedAt.Equal(base.Add(350 * time.Millisecond)) {
		t.Fatalf("expected freshest timestamp %s, got %s", base.Add(350*time.Millisecond), updatedAt)
	}
	if source != "rest" {
		t.Fatalf("expected freshest source rest, got %q", source)
	}
}

func TestRealbotNormalizeDisplaySource(t *testing.T) {
	if got := realbotNormalizeDisplaySource("ws-bbo"); got != "WS" {
		t.Fatalf("expected WS for ws-bbo, got %q", got)
	}
	if got := realbotNormalizeDisplaySource("rest-exec"); got != "REST" {
		t.Fatalf("expected REST for rest-exec, got %q", got)
	}
	if got := realbotNormalizeDisplaySource("unknown"); got != "WS" {
		t.Fatalf("expected WS default for unknown, got %q", got)
	}
}

func TestRealbotDisplayHasUsableQuotes(t *testing.T) {
	outcomes := []string{"Down", "Up"}

	if realbotDisplayHasUsableQuotes(outcomes,
		map[string]float64{"Down": 0, "Up": 0},
		map[string]float64{"Down": 0, "Up": 0},
	) {
		t.Fatal("expected empty display quotes to be unusable")
	}

	if !realbotDisplayHasUsableQuotes(outcomes,
		map[string]float64{"Down": 0.44, "Up": 0.54},
		map[string]float64{"Down": 0.45, "Up": 0.55},
	) {
		t.Fatal("expected sane two-sided display quotes to be usable")
	}

	if !realbotDisplayHasUsableQuotes(outcomes,
		map[string]float64{"Down": 0.99, "Up": 0},
		map[string]float64{"Down": 0, "Up": 0.01},
	) {
		t.Fatal("expected terminal one-sided display quotes to be usable")
	}
}
