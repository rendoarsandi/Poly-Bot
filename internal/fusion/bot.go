package fusion

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/signal"
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
)

const (
	defaultStartingBalance = 1000.0
	bookPollInterval       = 2 * time.Second
	marketRefreshInterval  = 10 * time.Second
	signalInterval         = 250 * time.Millisecond
	statusInterval         = 500 * time.Millisecond
	tuiRenderInterval      = 250 * time.Millisecond
)

type trackedMarket struct {
	Asset        string
	Market       *api.Market
	AnchorPrice  float64
	Bids         map[string]float64
	Asks         map[string]float64
	DepthBids    map[string][]paper.MarketLevel
	DepthAsks    map[string][]paper.MarketLevel
	LastUpdate   time.Time
	UpMidHistory []timedValue
	EventTimes   []time.Time
	ScoreHistory []timedValue
	LastFeatures ModelFeatures
}

type Bot struct {
	mu       sync.RWMutex
	cfg      *core.Config
	rest     *api.RestClient
	engine   *paper.Engine
	book     *paper.OrderBook
	tui      *paper.TUI
	binance  *BinanceStreamer
	pmFeed   *PolymarketFeed
	markets  map[string]*trackedMarket
	feedKey  string
	recorder *snapshotRecorder
	scorer   *externalScoreProvider
}

func Run() error {
	cfg, err := LoadFusionConfig()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return NewBot(cfg).Run(ctx, cancel)
}

func NewBot(cfg *core.Config) *Bot {
	startingBalance := cfg.BaseBalance
	if startingBalance <= 0 {
		startingBalance = defaultStartingBalance
	}
	engine := paper.NewEngine(startingBalance)
	engine.SetFeeRateBps(cfg.FeeRateBps)
	book := paper.NewOrderBook()
	tui := paper.NewTUI(engine, book)
	tui.InitSettings(settingsFromConfig(cfg), func(s paper.TUISettings) {
		cfg.MarketSlug, cfg.MaxMarkets, cfg.Timeframe = s.MarketSlug, s.MaxMarkets, s.Timeframe
		cfg.TradeScaleFactor, cfg.MinMarginPercent = s.TradeScaleFactor, s.MinMarginPercent
		cfg.MinAskPrice, cfg.MaxAskPrice = s.MinAskPrice, s.MaxAskPrice
		if err := SaveFusionSettings(s); err != nil {
			tui.LogEvent("fusion settings save failed: %v", err)
		} else {
			tui.LogEvent("fusion settings saved to %s", fusionSettingsFile)
		}
	})
	tui.SetMode("Fusionbot")
	tui.SetTradeFactor(cfg.TradeScaleFactor)
	tui.LogEvent("fusionbot isolated from realbot/paperbot")
	tui.LogEvent("using Binance WS + Polymarket WS/REST paper execution")
	tui.LogEvent("logic: estimate fair Up from Binance move + book skew, buy side with edge >= threshold, exit on edge decay/flip/near expiry")
	tui.LogEvent("ML note: external repo uses MLX PPO; Termux cannot run MLX here, so fusionbot is currently using the heuristic signal path")
	var scorer *externalScoreProvider
	if path := strings.TrimSpace(os.Getenv("FUSIONBOT_EXTERNAL_SCORES_PATH")); path != "" {
		scorer = newExternalScoreProvider(path)
		tui.LogEvent("external model score bridge enabled: %s", path)
	}
	return &Bot{cfg: cfg, rest: api.NewRestClient(""), engine: engine, book: book, tui: tui, binance: NewBinanceStreamer(), markets: map[string]*trackedMarket{}, scorer: scorer}
}

func (b *Bot) Run(ctx context.Context, cancel func()) error {
	if recordPath := strings.TrimSpace(os.Getenv("FUSIONBOT_RECORD_PATH")); recordPath != "" {
		recorder, err := newSnapshotRecorder(recordPath)
		if err != nil {
			b.tui.LogEvent("fusion recorder init failed: %v", err)
		} else {
			b.recorder = recorder
			defer b.recorder.Close()
			b.tui.LogEvent("recording fusion snapshots to %s", recordPath)
		}
	}
	b.tui.StartRenderLoop(tuiRenderInterval, cancel)
	go b.binance.Run(ctx, b.tui.LogEvent)
	if err := b.refreshMarkets(ctx); err != nil {
		b.tui.LogEvent("initial market refresh failed: %v", err)
	}
	if err := b.pollBooks(ctx); err != nil {
		b.tui.LogEvent("initial book seed failed: %v", err)
	}
	b.updateStatus()
	b.evaluateSignals()
	bookTicker, refreshTicker := time.NewTicker(bookPollInterval), time.NewTicker(marketRefreshInterval)
	signalTicker, statusTicker := time.NewTicker(signalInterval), time.NewTicker(statusInterval)
	defer bookTicker.Stop()
	defer refreshTicker.Stop()
	defer signalTicker.Stop()
	defer statusTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			b.closeFeed()
			b.tui.LogEvent("fusionbot shutting down")
			return nil
		case <-refreshTicker.C:
			if err := b.refreshMarkets(ctx); err != nil {
				b.tui.LogEvent("market refresh failed: %v", err)
			}
		case <-bookTicker.C:
			if err := b.pollBooks(ctx); err != nil {
				b.tui.LogEvent("book poll failed: %v", err)
			}
		case <-signalTicker.C:
			if b.tui.GetAndClearRestart() {
				b.tui.LogEvent("fusion settings changed; refreshing markets")
				if err := b.refreshMarkets(ctx); err != nil {
					b.tui.LogEvent("settings refresh failed: %v", err)
				}
			}
			b.evaluateSignals()
		case <-statusTicker.C:
			b.updateStatus()
		}
	}
}

func (b *Bot) refreshMarkets(ctx context.Context) error {
	refreshCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	settings := b.tui.GetSettings()
	assets := settingsAssets(settings.MarketSlug)
	timeframe := defaultTimeframe(settings.Timeframe)
	markets, err := b.rest.GetMarketsByTimeframe(refreshCtx, assets, timeframe)
	if err != nil {
		return err
	}
	b.mu.RLock()
	currentMarkets := make(map[string]*trackedMarket, len(b.markets))
	for asset, market := range b.markets {
		currentMarkets[asset] = market
	}
	b.mu.RUnlock()
	now := time.Now().UTC()
	qualities := map[string]marketQuality{}
	if shouldProbeMarketQualities(currentMarkets) {
		qualities = b.fetchMarketQualities(refreshCtx, markets, currentMarkets, assets, timeframe, now)
	}
	selected, updated := chooseLatestMarkets(markets, currentMarkets, qualities, assets, timeframe, settings.MaxMarkets, now), map[string]*trackedMarket{}
	newFeedKey := feedKeyForMarkets(selected)
	b.mu.Lock()
	old := b.markets
	oldFeedKey := b.feedKey
	for asset, market := range selected {
		if existing := old[asset]; existing != nil && existing.Market.ConditionID == market.ConditionID {
			updated[asset] = existing
			continue
		}
		if old[asset] != nil {
			b.tui.LogEvent("[%s] rotating market %s -> %s", asset, old[asset].Market.Slug, market.Slug)
			go b.closeAssetPositions(asset, old[asset], "market rotated")
		} else {
			b.tui.LogEvent("[%s] selected market %s", asset, market.Slug)
		}
		updated[asset] = &trackedMarket{Asset: asset, Market: market, AnchorPrice: b.binance.Quote(asset).Price, Bids: map[string]float64{}, Asks: map[string]float64{}, DepthBids: map[string][]paper.MarketLevel{}, DepthAsks: map[string][]paper.MarketLevel{}}
	}
	for asset, market := range old {
		if updated[asset] == nil {
			go b.closeAssetPositions(asset, market, "market dropped")
		}
	}
	b.markets = updated
	b.feedKey = newFeedKey
	b.mu.Unlock()
	b.syncTUIMarkets()
	if newFeedKey != oldFeedKey {
		if err := b.replacePolymarketFeed(ctx, updated); err != nil {
			b.tui.LogEvent("polymarket ws setup failed: %v", err)
		}
	}
	return nil
}

type marketQuality struct {
	Available    bool
	Complete     bool
	UpBid        float64
	UpAsk        float64
	DownBid      float64
	DownAsk      float64
	UpBidDepth   float64
	UpAskDepth   float64
	DownBidDepth float64
	DownAskDepth float64
	PairBidSum   float64
	PairAskSum   float64
	MaxSpread    float64
}

func (b *Bot) fetchMarketQualities(ctx context.Context, markets []api.Market, current map[string]*trackedMarket, requestedAssets []string, timeframe string, now time.Time) map[string]marketQuality {
	_ = timeframe
	qualities := make(map[string]marketQuality)
	grouped := make(map[string][]api.Market)
	for _, market := range markets {
		asset, ok := fusionMarketAsset(market)
		if !ok || !isValidFusionMarket(market, now) {
			continue
		}
		grouped[asset] = append(grouped[asset], market)
	}
	for asset := range grouped {
		sort.Slice(grouped[asset], func(i, j int) bool {
			return grouped[asset][i].EndTime.Before(grouped[asset][j].EndTime)
		})
	}
	var wg sync.WaitGroup
	var qualitiesMu sync.Mutex
	for _, requested := range requestedAssets {
		asset := strings.ToUpper(strings.TrimSpace(requested))
		candidates := grouped[asset]
		if len(candidates) == 0 {
			continue
		}
		currentCondition := ""
		if existing := current[asset]; existing != nil && existing.Market != nil {
			currentCondition = existing.Market.ConditionID
			qualities[currentCondition] = marketQualityFromTracked(existing)
		}
		for _, candidate := range shortlistMarketQualityCandidates(candidates, currentCondition, 3) {
			if _, exists := qualities[candidate.ConditionID]; exists {
				continue
			}
			candidate := candidate
			wg.Add(1)
			go func() {
				defer wg.Done()
				quality, err := b.fetchMarketQuality(ctx, &candidate)
				if err != nil {
					return
				}
				qualitiesMu.Lock()
				qualities[candidate.ConditionID] = quality
				qualitiesMu.Unlock()
			}()
		}
	}
	wg.Wait()
	return qualities
}

func shouldProbeMarketQualities(current map[string]*trackedMarket) bool {
	for _, market := range current {
		if market != nil && market.Market != nil {
			return true
		}
	}
	return false
}

func shortlistMarketQualityCandidates(candidates []api.Market, currentCondition string, limit int) []api.Market {
	if limit <= 0 || len(candidates) <= limit {
		return append([]api.Market(nil), candidates...)
	}
	selected := append([]api.Market(nil), candidates[:limit]...)
	for _, candidate := range candidates[limit:] {
		if candidate.ConditionID == currentCondition {
			selected = append(selected, candidate)
			break
		}
	}
	return selected
}

func marketQualityFromTracked(market *trackedMarket) marketQuality {
	if market == nil {
		return marketQuality{}
	}
	return marketQuality{
		Available:    marketQuoteHealth(market.Bids, market.Asks).Complete,
		Complete:     marketQuoteHealth(market.Bids, market.Asks).Complete,
		UpBid:        market.Bids["Up"],
		UpAsk:        market.Asks["Up"],
		DownBid:      market.Bids["Down"],
		DownAsk:      market.Asks["Down"],
		UpBidDepth:   sumLevelSize(market.DepthBids["Up"], 3),
		UpAskDepth:   sumLevelSize(market.DepthAsks["Up"], 3),
		DownBidDepth: sumLevelSize(market.DepthBids["Down"], 3),
		DownAskDepth: sumLevelSize(market.DepthAsks["Down"], 3),
		PairBidSum:   market.Bids["Up"] + market.Bids["Down"],
		PairAskSum:   market.Asks["Up"] + market.Asks["Down"],
		MaxSpread:    math.Max(math.Max(0, market.Asks["Up"]-market.Bids["Up"]), math.Max(0, market.Asks["Down"]-market.Bids["Down"])),
	}
}

func (b *Bot) fetchMarketQuality(ctx context.Context, market *api.Market) (marketQuality, error) {
	quality := marketQuality{}
	if market == nil {
		return quality, fmt.Errorf("nil market")
	}
	bids := map[string]float64{}
	asks := map[string]float64{}
	bidDepth := map[string]float64{}
	askDepth := map[string]float64{}
	for _, token := range market.Tokens {
		bookCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		resp, err := b.rest.GetOrderBook(bookCtx, token.TokenID)
		cancel()
		if err != nil {
			return quality, err
		}
		levelsBid := mkt.LevelsToPriceDepth(resp.Bids, true)
		levelsAsk := mkt.LevelsToPriceDepth(resp.Asks, false)
		bids[token.Outcome] = bestPrice(levelsBid)
		asks[token.Outcome] = bestPrice(levelsAsk)
		bidDepth[token.Outcome] = sumLevelSize(levelsBid, 3)
		askDepth[token.Outcome] = sumLevelSize(levelsAsk, 3)
	}
	quality = marketQuality{
		Available:    bids["Up"] > 0 && asks["Up"] > 0 && bids["Down"] > 0 && asks["Down"] > 0,
		Complete:     bids["Up"] > 0 && asks["Up"] > 0 && bids["Down"] > 0 && asks["Down"] > 0,
		UpBid:        bids["Up"],
		UpAsk:        asks["Up"],
		DownBid:      bids["Down"],
		DownAsk:      asks["Down"],
		UpBidDepth:   bidDepth["Up"],
		UpAskDepth:   askDepth["Up"],
		DownBidDepth: bidDepth["Down"],
		DownAskDepth: askDepth["Down"],
		PairBidSum:   bids["Up"] + bids["Down"],
		PairAskSum:   asks["Up"] + asks["Down"],
		MaxSpread:    math.Max(math.Max(0, asks["Up"]-bids["Up"]), math.Max(0, asks["Down"]-bids["Down"])),
	}
	return quality, nil
}

func (b *Bot) replacePolymarketFeed(ctx context.Context, markets map[string]*trackedMarket) error {
	newFeed := NewPolymarketFeed()
	if err := newFeed.Reset(ctx, markets); err != nil {
		return err
	}
	b.mu.Lock()
	oldFeed := b.pmFeed
	b.pmFeed = newFeed
	b.mu.Unlock()
	if oldFeed != nil {
		oldFeed.Close()
	}
	if newFeed.Messages() != nil {
		go b.consumePolymarket(ctx, newFeed)
	}
	return nil
}

func (b *Bot) closeFeed() {
	b.mu.Lock()
	feed := b.pmFeed
	b.pmFeed = nil
	b.mu.Unlock()
	if feed != nil {
		feed.Close()
	}
}

func (b *Bot) consumePolymarket(ctx context.Context, feed *PolymarketFeed) {
	msgCh := feed.Messages()
	if msgCh == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-msgCh:
			if !ok {
				return
			}
			b.handlePolymarketMessage(feed, msg)
		}
	}
}

func (b *Bot) handlePolymarketMessage(feed *PolymarketFeed, msg []byte) {
	if books, err := api.ParseOrderBooks(msg); err == nil && len(books) > 0 && books[0].AssetID != "" {
		for i := range books {
			b.applyBookSnapshot(feed, &books[i])
		}
		return
	}
	if update, err := api.ParsePriceUpdate(msg); err == nil && len(update.PriceChanges) > 0 {
		b.applyPriceUpdate(feed, update)
		return
	}
	if book, err := api.ParseOrderBook(msg); err == nil && book.AssetID != "" {
		b.applyBookSnapshot(feed, book)
	}
}

func (b *Bot) applyBookSnapshot(feed *PolymarketFeed, book *api.OrderBook) {
	ref, ok := feed.Lookup(book.AssetID)
	if !ok {
		return
	}
	bids, asks := mkt.LevelsToPriceDepth(book.Bids, true), mkt.LevelsToPriceDepth(book.Asks, false)
	if len(bids) > 0 && len(asks) > 0 && bids[0].Price >= asks[0].Price {
		return
	}
	b.mu.Lock()
	market := b.markets[ref.Asset]
	if market == nil {
		b.mu.Unlock()
		return
	}
	now := time.Now()
	market.DepthBids[ref.Outcome] = mergeDepthLevels(market.DepthBids[ref.Outcome], bids)
	market.DepthAsks[ref.Outcome] = mergeDepthLevels(market.DepthAsks[ref.Outcome], asks)
	market.Bids[ref.Outcome] = preservePositiveQuote(market.Bids[ref.Outcome], bestPrice(market.DepthBids[ref.Outcome]))
	market.Asks[ref.Outcome] = preservePositiveQuote(market.Asks[ref.Outcome], bestPrice(market.DepthAsks[ref.Outcome]))
	market.LastUpdate = now
	recordMarketMicroLocked(market, now)
	bidsCopy, asksCopy, bidDepth, askDepth := copyFloatMap(market.Bids), copyFloatMap(market.Asks), copyDepthMap(market.DepthBids), copyDepthMap(market.DepthAsks)
	b.mu.Unlock()
	b.pushMarketToUI(ref.Asset, bidsCopy, asksCopy, bidDepth, askDepth, "WS")
}

func (b *Bot) applyPriceUpdate(feed *PolymarketFeed, update *api.PriceUpdate) {
	touched := map[string]struct{}{}
	b.mu.Lock()
	for _, pc := range update.PriceChanges {
		ref, ok := feed.Lookup(pc.AssetID)
		if !ok {
			continue
		}
		price, errP := strconv.ParseFloat(pc.Price, 64)
		size, errS := strconv.ParseFloat(pc.Size, 64)
		if errP != nil || errS != nil || price <= 0 {
			continue
		}
		market := b.markets[ref.Asset]
		if market == nil {
			continue
		}
		switch pc.Side {
		case "BUY":
			market.DepthBids[ref.Outcome] = mkt.ApplyDelta(market.DepthBids[ref.Outcome], price, size, true)
		case "SELL":
			market.DepthAsks[ref.Outcome] = mkt.ApplyDelta(market.DepthAsks[ref.Outcome], price, size, false)
		default:
			continue
		}
		now := time.Now()
		market.Bids[ref.Outcome] = preservePositiveQuote(market.Bids[ref.Outcome], bestPrice(market.DepthBids[ref.Outcome]))
		market.Asks[ref.Outcome] = preservePositiveQuote(market.Asks[ref.Outcome], bestPrice(market.DepthAsks[ref.Outcome]))
		market.LastUpdate = now
		recordMarketMicroLocked(market, now)
		touched[ref.Asset] = struct{}{}
	}
	updates := map[string]struct {
		bids, asks         map[string]float64
		bidDepth, askDepth map[string][]paper.MarketLevel
	}{}
	for asset := range touched {
		market := b.markets[asset]
		if market == nil {
			continue
		}
		updates[asset] = struct {
			bids, asks         map[string]float64
			bidDepth, askDepth map[string][]paper.MarketLevel
		}{copyFloatMap(market.Bids), copyFloatMap(market.Asks), copyDepthMap(market.DepthBids), copyDepthMap(market.DepthAsks)}
	}
	b.mu.Unlock()
	for asset, snapshot := range updates {
		b.pushMarketToUI(asset, snapshot.bids, snapshot.asks, snapshot.bidDepth, snapshot.askDepth, "WS")
	}
}

func (b *Bot) pushMarketToUI(asset string, bids, asks map[string]float64, bidDepth, askDepth map[string][]paper.MarketLevel, source string) {
	for outcome, bid := range bids {
		ask := asks[outcome]
		if bid > 0 || ask > 0 {
			b.engine.UpdateMarketData(asset, outcome, midpoint(bid, ask), bid, ask)
		}
	}
	b.tui.UpdateMarketPricesWithSource(asset, bids, asks, source)
	b.tui.UpdateOrderBookDepth(asset, bidDepth, askDepth)
	b.tui.TouchMarket(asset)
}

func (b *Bot) syncTUIMarkets() {
	b.mu.RLock()
	desired := make(map[string]*trackedMarket, len(b.markets))
	for asset, market := range b.markets {
		desired[asset] = market
	}
	b.mu.RUnlock()
	for _, asset := range b.marketIDs() {
		b.mu.RLock()
		market := b.markets[asset]
		b.mu.RUnlock()
		if market == nil {
			continue
		}
		b.tui.AddMarket(asset, market.Market.Slug, marketOutcomes(market.Market), market.Market.EndTime)
	}
	for _, asset := range []string{"BTC", "ETH", "SOL", "XRP"} {
		if desired[asset] == nil {
			b.tui.RemoveMarket(asset)
		}
	}
}

func (b *Bot) marketIDs() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	assets := make([]string, 0, len(b.markets))
	for asset := range b.markets {
		assets = append(assets, asset)
	}
	sort.Strings(assets)
	return assets
}

func (b *Bot) pollBooks(ctx context.Context) error {
	b.mu.RLock()
	feed := b.pmFeed
	repairAssets := make([]string, 0)
	for asset, market := range b.markets {
		if marketNeedsBookRepair(market) {
			repairAssets = append(repairAssets, asset)
		}
	}
	b.mu.RUnlock()
	if feed != nil && feed.IsConnected() && feed.TimeSinceLastDataMessage() < 3*time.Second && len(repairAssets) == 0 {
		return nil
	}
	started := time.Now()
	assets := repairAssets
	if len(assets) == 0 {
		assets = b.marketIDs()
	}
	sort.Strings(assets)
	for _, asset := range assets {
		if err := b.pollMarketBooks(ctx, asset); err != nil {
			b.tui.LogEvent("[%s] rest book poll failed: %v", asset, err)
		}
	}
	b.tui.UpdateRestLatency(time.Since(started))
	return nil
}

func (b *Bot) pollMarketBooks(ctx context.Context, asset string) error {
	b.mu.RLock()
	market := b.markets[asset]
	b.mu.RUnlock()
	if market == nil {
		return nil
	}
	bids, asks, bidDepth, askDepth := map[string]float64{}, map[string]float64{}, map[string][]paper.MarketLevel{}, map[string][]paper.MarketLevel{}
	conditionID := market.Market.ConditionID
	for _, token := range market.Market.Tokens {
		bookCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		resp, err := b.rest.GetOrderBook(bookCtx, token.TokenID)
		cancel()
		if err != nil {
			return err
		}
		bidDepth[token.Outcome], askDepth[token.Outcome] = mkt.LevelsToPriceDepth(resp.Bids, true), mkt.LevelsToPriceDepth(resp.Asks, false)
		bids[token.Outcome], asks[token.Outcome] = bestPrice(bidDepth[token.Outcome]), bestPrice(askDepth[token.Outcome])
	}
	b.mu.Lock()
	current := b.markets[asset]
	if current == nil || current.Market.ConditionID != conditionID {
		b.mu.Unlock()
		return nil
	}
	now := time.Now()
	for outcome, levels := range bidDepth {
		bidDepth[outcome] = mergeDepthLevels(current.DepthBids[outcome], levels)
		bids[outcome] = preservePositiveQuote(current.Bids[outcome], bestPrice(bidDepth[outcome]))
	}
	for outcome, levels := range askDepth {
		askDepth[outcome] = mergeDepthLevels(current.DepthAsks[outcome], levels)
		asks[outcome] = preservePositiveQuote(current.Asks[outcome], bestPrice(askDepth[outcome]))
	}
	current.Bids, current.Asks, current.DepthBids, current.DepthAsks, current.LastUpdate = bids, asks, bidDepth, askDepth, now
	recordMarketMicroLocked(current, now)
	b.mu.Unlock()
	b.pushMarketToUI(asset, bids, asks, bidDepth, askDepth, "REST")
	return nil
}

func (b *Bot) updateStatus() {
	b.mu.RLock()
	feed := b.pmFeed
	b.mu.RUnlock()
	if feed != nil {
		b.tui.UpdateWSLatency(feed.TimeSinceLastDataMessage())
		b.tui.UpdateWSPingLatency(feed.PingLatency())
	}
	for _, asset := range b.marketIDs() {
		quote := b.binance.Quote(asset)
		b.mu.RLock()
		market := b.markets[asset]
		b.mu.RUnlock()
		if market != nil {
			b.tui.SetMarketDetails(asset, b.buildMarketDetails(market, quote, b.currentPosition(asset)))
		}
	}
}

func (b *Bot) buildMarketDetails(market *trackedMarket, quote BinanceQuote, pos *paper.Position) []string {
	if market == nil {
		return nil
	}
	if quote.Price <= 0 {
		return []string{"waiting for Binance WS price…"}
	}
	features := market.LastFeatures
	if features.FairUp <= 0 {
		features = b.modelFeatures(market.Asset, market, quote, pos)
	}
	upEdge, downEdge := edgeFromAsk(features.FairUp, market.Asks["Up"]), edgeFromAsk(1-features.FairUp, market.Asks["Down"])
	positionText := "flat"
	if pos != nil {
		positionText = fmt.Sprintf("pos %s %.2f @ %.3f", pos.Outcome, pos.Quantity, pos.AvgPrice)
	}
	details := []string{
		fmt.Sprintf("Binance $%.4f  R1 %+.2f%% R5 %+.2f%% R10 %+.2f%%", quote.Price, features.Returns1m*100, features.Returns5m*100, features.Returns10m*100),
		fmt.Sprintf("Prob %.3f Fair %.3f Score %+.1f%% Smooth %+.1f%%", features.CurrentProb, features.FairUp, features.Score*100, features.SmoothedScore*100),
		fmt.Sprintf("Edge U %+.1f%% D %+.1f%% | OB %.2f/%.2f Flow %.2f CVD %.2f %s", upEdge*100, downEdge*100, features.OrderBookImbalanceL1, features.OrderBookImbalanceL5, features.TradeFlowImbalance, features.CVDAcceleration, positionText),
	}
	if features.ExternalModelWeight > 0 {
		ageText := ""
		if features.ExternalModelAgeSec > 0 {
			ageText = fmt.Sprintf(" • age %.0fs", features.ExternalModelAgeSec)
		}
		details = append(details, fmt.Sprintf("ML %+.1f%% • Weight %.0f%%%s • %s", features.ExternalModelScore*100, features.ExternalModelWeight*100, ageText, strings.TrimSpace(features.ExternalModelReason)))
	}
	if note := marketBookStatusLine(market); note != "" {
		details = append(details, note)
	}
	return details
}

func (b *Bot) modelFeatures(asset string, market *trackedMarket, quote BinanceQuote, pos *paper.Position) ModelFeatures {
	features := buildModelFeatures(b.cfg, market, quote, b.binance.Signals(asset), pos)
	if b.scorer == nil {
		return features
	}
	score, ok := b.scorer.LookupFresh(asset, maxExternalModelScoreAge, time.Now())
	if !ok {
		return features
	}
	return applyExternalModelScore(features, score)
}

func (b *Bot) evaluateSignals() {
	pending := map[string][]paper.PendingOrder{}
	for _, asset := range b.marketIDs() {
		quote := b.binance.Quote(asset)
		if quote.Price <= 0 {
			continue
		}
		b.mu.Lock()
		market := b.markets[asset]
		if market == nil {
			b.mu.Unlock()
			continue
		}
		if market.AnchorPrice <= 0 {
			market.AnchorPrice = quote.Price
			b.mu.Unlock()
			continue
		}
		marketCopy := *market
		marketCopy.Bids, marketCopy.Asks = copyFloatMap(market.Bids), copyFloatMap(market.Asks)
		marketCopy.DepthBids, marketCopy.DepthAsks = copyDepthMap(market.DepthBids), copyDepthMap(market.DepthAsks)
		marketCopy.UpMidHistory = append([]timedValue(nil), market.UpMidHistory...)
		marketCopy.EventTimes = append([]time.Time(nil), market.EventTimes...)
		marketCopy.ScoreHistory = append([]timedValue(nil), market.ScoreHistory...)
		marketCopy.LastFeatures = market.LastFeatures
		b.mu.Unlock()
		pos := b.currentPosition(asset)
		features := b.modelFeatures(asset, &marketCopy, quote, pos)
		decision := decideAction(b.cfg, SignalSnapshot{
			Asset:          asset,
			UpBid:          marketCopy.Bids["Up"],
			UpAsk:          marketCopy.Asks["Up"],
			DownBid:        marketCopy.Bids["Down"],
			DownAsk:        marketCopy.Asks["Down"],
			TimeRemaining:  time.Until(marketCopy.Market.EndTime),
			Position:       pos,
			Features:       features,
			MarketDataAge:  time.Since(marketCopy.LastUpdate),
			BinanceDataAge: time.Since(quote.Updated),
			UpAskDepth:     sumLevelSize(marketCopy.DepthAsks["Up"], 3),
			DownAskDepth:   sumLevelSize(marketCopy.DepthAsks["Down"], 3),
		})
		b.mu.Lock()
		if live := b.markets[asset]; live != nil && live.Market.ConditionID == marketCopy.Market.ConditionID {
			live.LastFeatures = features
			live.ScoreHistory = append(live.ScoreHistory, timedValue{At: time.Now(), Value: features.Score})
			live.ScoreHistory = pruneTimedValues(live.ScoreHistory, time.Now().Add(-10*time.Minute))
		}
		b.mu.Unlock()
		b.recordSnapshot(ReplaySnapshot{
			Timestamp:            time.Now().UTC(),
			Asset:                asset,
			MarketID:             marketCopy.Market.ConditionID,
			MarketSlug:           marketCopy.Market.Slug,
			UpBid:                marketCopy.Bids["Up"],
			UpAsk:                marketCopy.Asks["Up"],
			DownBid:              marketCopy.Bids["Down"],
			DownAsk:              marketCopy.Asks["Down"],
			TimeRemainingSec:     time.Until(marketCopy.Market.EndTime).Seconds(),
			MarketDataAgeMillis:  time.Since(marketCopy.LastUpdate).Milliseconds(),
			BinanceDataAgeMillis: time.Since(quote.Updated).Milliseconds(),
			UpAskDepth:           sumLevelSize(marketCopy.DepthAsks["Up"], 3),
			DownAskDepth:         sumLevelSize(marketCopy.DepthAsks["Down"], 3),
			Features:             features,
			Decision:             decision,
		})
		if decision.Action == "BUY" && decision.Price > 0 {
			qty := decisionQuantity(b.cfg, b.engine.GetEquity(), decision, sumLevelSize(marketCopy.DepthAsks[decision.Outcome], 3))
			if qty >= 1 {
				pending[asset] = []paper.PendingOrder{{Outcome: decision.Outcome, Side: decision.Action, Price: decision.Price, Qty: qty}}
			}
		}
		b.applyDecision(asset, decision)
		marketCopy.LastFeatures = features
		b.tui.SetMarketDetails(asset, b.buildMarketDetails(&marketCopy, quote, b.currentPosition(asset)))
	}
	b.tui.SetPendingOrders(pending)
}

func (b *Bot) applyDecision(asset string, decision Decision) {
	if decision.Action == "HOLD" || decision.Price <= 0 {
		return
	}
	if decision.Action == "SELL" {
		pos := b.currentPosition(asset)
		if pos == nil || !strings.EqualFold(pos.Outcome, decision.Outcome) {
			return
		}
		profit := (decision.Price - pos.AvgPrice) * pos.Quantity
		if _, err := b.engine.SellForMarket(asset, decision.Outcome, decision.Price, pos.Quantity); err != nil {
			b.tui.LogEvent("[%s] sell %s failed: %v", asset, decision.Outcome, err)
			return
		}
		b.tui.RecordOrder(asset, decision.Outcome, "SELL", pos.Quantity, decision.Price, pos.Quantity*decision.Price, decision.Edge*100, profit, "FILLED")
		b.tui.LogEvent("[%s] SELL %s %.2f @ %.3f (%s)", asset, decision.Outcome, pos.Quantity, decision.Price, decision.Reason)
		return
	}
	if pos := b.currentPosition(asset); pos != nil && !strings.EqualFold(pos.Outcome, decision.Outcome) {
		return
	}
	b.mu.RLock()
	market := b.markets[asset]
	availableDepth := 0.0
	if market != nil {
		availableDepth = sumLevelSize(market.DepthAsks[decision.Outcome], 3)
	}
	b.mu.RUnlock()
	qty := decisionQuantity(b.cfg, b.engine.GetEquity(), decision, availableDepth)
	if qty < 1 {
		return
	}
	trade, err := b.engine.BuyForMarket(asset, decision.Outcome, decision.Price, qty)
	if err != nil {
		b.tui.LogEvent("[%s] buy %s failed: %v", asset, decision.Outcome, err)
		return
	}
	b.tui.RecordOrder(asset, decision.Outcome, "BUY", trade.Quantity, decision.Price, trade.Value, decision.Edge*100, 0, "FILLED")
	b.tui.LogEvent("[%s] BUY %s %.2f @ %.3f | edge %.2f%% | fairUp %.3f (%s)", asset, decision.Outcome, trade.Quantity, decision.Price, decision.Edge*100, decision.FairUp, decision.Reason)
}

func (b *Bot) recordSnapshot(snapshot ReplaySnapshot) {
	if b.recorder == nil {
		return
	}
	if err := b.recorder.Record(snapshot); err != nil {
		b.tui.LogEvent("fusion snapshot record failed: %v", err)
		_ = b.recorder.Close()
		b.recorder = nil
	}
}

func (b *Bot) currentPosition(asset string) *paper.Position {
	for _, pos := range b.engine.GetPositions() {
		if pos.MarketID == asset {
			cp := pos
			return &cp
		}
	}
	return nil
}

func (b *Bot) closeAssetPositions(asset string, market *trackedMarket, reason string) {
	for _, pos := range b.engine.GetPositions() {
		if pos.MarketID != asset {
			continue
		}
		bid := 0.0
		if market != nil {
			bid = market.Bids[pos.Outcome]
		}
		if bid <= 0 {
			continue
		}
		profit := (bid - pos.AvgPrice) * pos.Quantity
		if _, err := b.engine.SellForMarket(asset, pos.Outcome, bid, pos.Quantity); err != nil {
			b.tui.LogEvent("[%s] forced close failed: %v", asset, err)
			continue
		}
		b.tui.RecordOrder(asset, pos.Outcome, "SELL", pos.Quantity, bid, pos.Quantity*bid, 0, profit, "FILLED")
		b.tui.LogEvent("[%s] forced close %s @ %.3f (%s)", asset, pos.Outcome, bid, reason)
	}
}

func settingsFromConfig(cfg *core.Config) paper.TUISettings {
	marketSlug, timeframe := cfg.MarketSlug, defaultTimeframe(cfg.Timeframe)
	if strings.TrimSpace(marketSlug) == "" {
		marketSlug = "ALL"
	}
	if cfg.MaxMarkets <= 0 {
		cfg.MaxMarkets = 4
	}
	if cfg.TradeScaleFactor <= 0 {
		cfg.TradeScaleFactor = 0.05
	}
	if cfg.MinMarginPercent <= 0 {
		cfg.MinMarginPercent = 2.0
	}
	if cfg.MinAskPrice <= 0 {
		cfg.MinAskPrice = 0.10
	}
	if cfg.MaxAskPrice <= 0 {
		cfg.MaxAskPrice = 0.90
	}
	return paper.TUISettings{MarketSlug: marketSlug, MaxMarkets: cfg.MaxMarkets, Timeframe: timeframe, TradeScaleFactor: cfg.TradeScaleFactor, MinMarginPercent: cfg.MinMarginPercent, BuyExecutionMarginFloorPercent: cfg.BuyExecutionMarginFloorPercent, SplitMinMarginSell: cfg.SplitMinMarginSell, SplitStrategyEnabled: false, SplitInitialCapPct: cfg.SplitInitialCapPct, SplitReplenishCapPct: cfg.SplitReplenishCapPct, MinAskPrice: cfg.MinAskPrice, MaxAskPrice: cfg.MaxAskPrice}
}

func settingsAssets(marketSlug string) []string {
	marketSlug = strings.TrimSpace(strings.ToLower(marketSlug))
	if marketSlug == "" || marketSlug == "all" {
		return []string{"btc", "eth", "sol", "xrp"}
	}
	parts, assets := strings.Split(marketSlug, ","), make([]string, 0)
	for _, part := range parts {
		if asset := strings.TrimSpace(strings.ToLower(part)); asset != "" {
			assets = append(assets, asset)
		}
	}
	if len(assets) == 0 {
		return []string{"btc", "eth", "sol", "xrp"}
	}
	return assets
}

func chooseLatestMarkets(markets []api.Market, current map[string]*trackedMarket, qualities map[string]marketQuality, requestedAssets []string, timeframe string, maxMarkets int, now time.Time) map[string]*api.Market {
	grouped := make(map[string][]api.Market)
	for _, market := range markets {
		asset, ok := fusionMarketAsset(market)
		if !ok || !isValidFusionMarket(market, now) {
			continue
		}
		grouped[asset] = append(grouped[asset], market)
	}
	for asset := range grouped {
		sort.Slice(grouped[asset], func(i, j int) bool {
			return grouped[asset][i].EndTime.Before(grouped[asset][j].EndTime)
		})
	}
	selected := map[string]*api.Market{}
	for _, requested := range requestedAssets {
		asset := strings.ToUpper(strings.TrimSpace(requested))
		candidates := grouped[asset]
		if len(candidates) == 0 {
			continue
		}
		currentCondition := ""
		if existing := current[asset]; existing != nil && existing.Market != nil {
			currentCondition = existing.Market.ConditionID
		}
		if chosen := chooseBestMarketCandidate(candidates, qualities, currentCondition, timeframeDuration(timeframe), now); chosen != nil {
			selected[asset] = chosen
		}
		if maxMarkets > 0 && len(selected) >= maxMarkets {
			break
		}
	}
	return selected
}

func chooseBestMarketCandidate(candidates []api.Market, qualities map[string]marketQuality, currentCondition string, tf time.Duration, now time.Time) *api.Market {
	if len(candidates) == 0 {
		return nil
	}
	rotateLead := marketRotateLead(tf)
	fallbackLead := marketFallbackLead(tf)
	var currentCandidate *api.Market
	for i := range candidates {
		candidate := &candidates[i]
		if candidate.ConditionID == currentCondition {
			currentCandidate = candidate
			if candidate.EndTime.Sub(now) > rotateLead {
				return candidate
			}
			break
		}
	}
	if chosen := bestQualityCandidate(candidates, qualities, currentCondition, rotateLead, now); chosen != nil {
		return chosen
	}
	if currentCandidate != nil && currentCandidate.EndTime.After(now.Add(15*time.Second)) {
		return currentCandidate
	}
	if chosen := bestQualityCandidate(candidates, qualities, currentCondition, fallbackLead, now); chosen != nil {
		return chosen
	}
	bestScore := math.Inf(-1)
	var best *api.Market
	for i := range candidates {
		candidate := &candidates[i]
		if candidate.EndTime.After(now) {
			score := marketSelectionScore(*candidate, qualities[candidate.ConditionID], currentCondition, now)
			if best == nil || score > bestScore {
				best, bestScore = candidate, score
			}
		}
	}
	return best
}

func bestQualityCandidate(candidates []api.Market, qualities map[string]marketQuality, currentCondition string, minLead time.Duration, now time.Time) *api.Market {
	bestScore := math.Inf(-1)
	var best *api.Market
	for i := range candidates {
		candidate := &candidates[i]
		if candidate.EndTime.Sub(now) <= minLead {
			continue
		}
		score := marketSelectionScore(*candidate, qualities[candidate.ConditionID], currentCondition, now)
		if best == nil || score > bestScore {
			best, bestScore = candidate, score
		}
	}
	return best
}

func marketSelectionScore(candidate api.Market, quality marketQuality, currentCondition string, now time.Time) float64 {
	timeToEnd := candidate.EndTime.Sub(now)
	if timeToEnd <= 0 {
		return math.Inf(-1)
	}
	score := -timeToEnd.Seconds() / (15 * 60)
	if candidate.ConditionID == currentCondition {
		score += 0.35
	}
	coherencePenalty := clamp((math.Abs(quality.PairAskSum-1.0)+math.Abs(quality.PairBidSum-1.0))/0.08, 0, 1) * 1.15
	spreadPenalty := clamp((math.Max(0, quality.UpAsk-quality.UpBid)+math.Max(0, quality.DownAsk-quality.DownBid)+quality.MaxSpread)/0.12, 0, 1) * 1.10
	if !quality.Available || !quality.Complete {
		return score - 0.35 - coherencePenalty
	}
	depthScore := clamp(math.Min(quality.UpAskDepth, quality.DownAskDepth)/200, 0, 1)
	bidSupport := clamp((quality.UpBidDepth+quality.DownBidDepth)/300, 0, 1)
	quoteIntegrity := 0.0
	if quality.UpBid > 0 && quality.UpAsk > 0 && quality.DownBid > 0 && quality.DownAsk > 0 {
		quoteIntegrity = 0.25
	}
	return score + depthScore*1.25 + bidSupport*0.35 + quoteIntegrity - spreadPenalty - coherencePenalty
}

type bookQuoteHealth struct {
	Complete  bool
	Missing   []string
	PairBid   float64
	PairAsk   float64
	MaxSpread float64
}

func marketQuoteHealth(bids, asks map[string]float64) bookQuoteHealth {
	health := bookQuoteHealth{Missing: make([]string, 0, 4)}
	for _, outcome := range []string{"Up", "Down"} {
		bid := bids[outcome]
		ask := asks[outcome]
		if bid <= 0 {
			health.Missing = append(health.Missing, outcome+" bid")
		}
		if ask <= 0 {
			health.Missing = append(health.Missing, outcome+" ask")
		}
		health.PairBid += bid
		health.PairAsk += ask
		health.MaxSpread = math.Max(health.MaxSpread, math.Max(0, ask-bid))
	}
	health.Complete = len(health.Missing) == 0
	return health
}

func marketNeedsBookRepair(market *trackedMarket) bool {
	if market == nil {
		return false
	}
	return !marketQuoteHealth(market.Bids, market.Asks).Complete
}

func marketBookStatusLine(market *trackedMarket) string {
	if market == nil {
		return ""
	}
	health := marketQuoteHealth(market.Bids, market.Asks)
	if !health.Complete {
		return fmt.Sprintf("Book degraded: missing %s • REST repair active", strings.Join(health.Missing, ", "))
	}
	if health.MaxSpread >= 0.03 || math.Abs(health.PairAsk-1.0) >= 0.04 || math.Abs(health.PairBid-1.0) >= 0.04 {
		return fmt.Sprintf("Book wide: buy %.3f sell %.3f max spread %.1f%%", health.PairAsk, health.PairBid, health.MaxSpread*100)
	}
	return ""
}

func fusionMarketAsset(market api.Market) (string, bool) {
	asset := strings.ToUpper(strings.TrimSpace(strings.SplitN(market.Slug, "-", 2)[0]))
	if asset == "" {
		return "", false
	}
	return asset, true
}

func isValidFusionMarket(market api.Market, now time.Time) bool {
	if !market.Active || market.Closed || market.ConditionID == "" || market.EndTime.IsZero() || !market.EndTime.After(now) {
		return false
	}
	if len(market.Tokens) != 2 {
		return false
	}
	seen := map[string]struct{}{}
	outcomes := map[string]struct{}{}
	for _, token := range market.Tokens {
		if token.TokenID == "" {
			return false
		}
		if _, exists := seen[token.TokenID]; exists {
			return false
		}
		seen[token.TokenID] = struct{}{}
		outcomes[strings.ToUpper(strings.TrimSpace(token.Outcome))] = struct{}{}
	}
	_, hasUp := outcomes["UP"]
	_, hasDown := outcomes["DOWN"]
	return hasUp && hasDown
}

func timeframeDuration(tf string) time.Duration {
	switch strings.TrimSpace(strings.ToLower(tf)) {
	case "5m":
		return 5 * time.Minute
	case "1d":
		return 24 * time.Hour
	default:
		return 15 * time.Minute
	}
}

func marketRotateLead(tf time.Duration) time.Duration {
	if tf <= 0 {
		return 2 * time.Minute
	}
	lead := tf / 8
	if lead < 60*time.Second {
		lead = 60 * time.Second
	}
	if lead > 2*time.Minute {
		lead = 2 * time.Minute
	}
	return lead
}

func marketFallbackLead(tf time.Duration) time.Duration {
	lead := marketRotateLead(tf) / 2
	if lead < 20*time.Second {
		lead = 20 * time.Second
	}
	return lead
}

func marketOutcomes(market *api.Market) []string {
	outcomes := make([]string, 0, len(market.Tokens))
	for _, token := range market.Tokens {
		outcomes = append(outcomes, token.Outcome)
	}
	return outcomes
}
func defaultTimeframe(tf string) string {
	if strings.TrimSpace(tf) == "" {
		return "15m"
	}
	return tf
}
func midpoint(bid, ask float64) float64 {
	if bid > 0 && ask > 0 {
		return (bid + ask) / 2
	}
	if bid > 0 {
		return bid
	}
	return ask
}
func bestPrice(levels []paper.MarketLevel) float64 {
	if len(levels) == 0 {
		return 0
	}
	return levels[0].Price
}

func mergeDepthLevels(existing, incoming []paper.MarketLevel) []paper.MarketLevel {
	if len(incoming) > 0 {
		return append([]paper.MarketLevel(nil), incoming...)
	}
	if len(existing) > 0 {
		return append([]paper.MarketLevel(nil), existing...)
	}
	return nil
}

func preservePositiveQuote(previous, next float64) float64 {
	if next > 0 {
		return next
	}
	return previous
}

func edgeFromAsk(fair, ask float64) float64 {
	if ask <= 0 {
		return 0
	}
	return fair - ask
}

func feedKeyForMarkets(markets map[string]*api.Market) string {
	parts := make([]string, 0, len(markets))
	for asset, market := range markets {
		tokens := make([]string, 0, len(market.Tokens))
		for _, token := range market.Tokens {
			tokens = append(tokens, token.TokenID)
		}
		sort.Strings(tokens)
		parts = append(parts, asset+":"+market.ConditionID+":"+strings.Join(tokens, ","))
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

func roundedShares(v float64) float64 { return math.Floor(v*10000) / 10000 }
func copyFloatMap(src map[string]float64) map[string]float64 {
	dst := make(map[string]float64, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
func copyDepthMap(src map[string][]paper.MarketLevel) map[string][]paper.MarketLevel {
	dst := make(map[string][]paper.MarketLevel, len(src))
	for k, levels := range src {
		copied := make([]paper.MarketLevel, len(levels))
		copy(copied, levels)
		dst[k] = copied
	}
	return dst
}
