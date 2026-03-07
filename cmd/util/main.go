package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
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
	"Market-bot/internal/setup"
	"Market-bot/internal/trading"
	"github.com/charmbracelet/lipgloss"
	"github.com/joho/godotenv"
)

const (
	utilbotLocalQuoteMaxAge  = 250 * time.Millisecond
	utilbotJITRequoteTimeout = 750 * time.Millisecond
)

type utilbotQuoteState struct {
	UpdatedAt time.Time
	Source    string
}

type utilbotQuoteSnapshot struct {
	TokenBids     map[string]float64
	TokenAsks     map[string]float64
	TokenFullBids map[string][]paper.MarketLevel
	TokenFullAsks map[string][]paper.MarketLevel
	QuoteState    map[string]utilbotQuoteState
}

type utilbotQuoteStore struct {
	mu            sync.RWMutex
	tokenBids     map[string]float64
	tokenAsks     map[string]float64
	tokenFullBids map[string][]paper.MarketLevel
	tokenFullAsks map[string][]paper.MarketLevel
	quoteState    map[string]utilbotQuoteState
}

func newUtilbotQuoteStore() *utilbotQuoteStore {
	return &utilbotQuoteStore{
		tokenBids:     make(map[string]float64),
		tokenAsks:     make(map[string]float64),
		tokenFullBids: make(map[string][]paper.MarketLevel),
		tokenFullAsks: make(map[string][]paper.MarketLevel),
		quoteState:    make(map[string]utilbotQuoteState),
	}
}

func (s *utilbotQuoteStore) Update(out string, bid, ask float64, fullBids, fullAsks []paper.MarketLevel, source string, updatedAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bid > 0 {
		s.tokenBids[out] = bid
	}
	if ask > 0 {
		s.tokenAsks[out] = ask
	}
	s.tokenFullBids[out] = append([]paper.MarketLevel(nil), fullBids...)
	s.tokenFullAsks[out] = append([]paper.MarketLevel(nil), fullAsks...)
	s.quoteState[out] = utilbotQuoteState{UpdatedAt: updatedAt, Source: source}
}

func (s *utilbotQuoteStore) Snapshot(outcomes []string) utilbotQuoteSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap := utilbotQuoteSnapshot{
		TokenBids:     make(map[string]float64, len(outcomes)),
		TokenAsks:     make(map[string]float64, len(outcomes)),
		TokenFullBids: make(map[string][]paper.MarketLevel, len(outcomes)),
		TokenFullAsks: make(map[string][]paper.MarketLevel, len(outcomes)),
		QuoteState:    make(map[string]utilbotQuoteState, len(outcomes)),
	}
	for _, out := range outcomes {
		snap.TokenBids[out] = s.tokenBids[out]
		snap.TokenAsks[out] = s.tokenAsks[out]
		snap.TokenFullBids[out] = append([]paper.MarketLevel(nil), s.tokenFullBids[out]...)
		snap.TokenFullAsks[out] = append([]paper.MarketLevel(nil), s.tokenFullAsks[out]...)
		if state, ok := s.quoteState[out]; ok {
			snap.QuoteState[out] = state
		}
	}
	return snap
}

func main() {
	// Styled startup banner
	titleSt := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7C3AED"))
	dimSt := lipgloss.NewStyle().Foreground(lipgloss.Color("#6B7280"))
	warnSt := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#EF4444"))
	fmt.Println(lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#7C3AED")).
		Padding(0, 2).
		Render(titleSt.Render("🛠  UTILBOT  —  Panic Buy / Sell") + "\n" +
			warnSt.Render("⚠  Executes REAL trades with on-chain merge") + "\n" +
			dimSt.Render("Live order book  ·  Liquidity-capped execution")))

	_ = godotenv.Load()
	cfg, err := core.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Create context for setup
	setupCtx, cancelSetup := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelSetup()

	// Ensure trading mode is set and credentials exist
	cfg.TradingMode = core.ModeReal
	trader, err := setup.EnsureRealTradingSetup(setupCtx, cfg)
	if err != nil {
		log.Fatalf("Failed to setup or create trader: %v", err)
	}

	client := api.NewRestClient("")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Sync CLOB cached allowance with on-chain state
	fmt.Println("🔄 Syncing CLOB balance allowance...")
	if err := trader.UpdateBalanceAllowance(ctx); err != nil {
		log.Printf("⚠️ Failed to update balance allowance: %v", err)
	} else {
		fmt.Println("✅ CLOB balance allowance synced")
	}

	fmt.Println("🔌 Preparing User WebSocket for real-time fills...")
	if err := trader.StartUserWS(ctx); err != nil {
		fmt.Printf("⚠️ Failed to connect User WS (falling back to REST): %v\n", err)
	} else {
		fmt.Println("✅ User WebSocket ready")
	}

	// 1. Find markets
	fmt.Println("Select timeframe:")
	fmt.Println("1. 5m")
	fmt.Println("2. 15m")
	fmt.Print("Choice [default 2]: ")
	var tfChoice string
	_, _ = fmt.Scanln(&tfChoice)
	tfChoice = strings.TrimSpace(tfChoice)

	timeframe := "15m"
	if tfChoice == "1" {
		timeframe = "5m"
	} else if tfChoice == "2" || tfChoice == "" {
		timeframe = "15m"
	} else {
		log.Fatalf("❌ Invalid choice.")
	}

	fmt.Printf("🔍 Searching for active %s markets...\n", timeframe)
	markets := findMarkets(ctx, client, timeframe)

	if len(markets) == 0 {
		log.Fatalf("❌ No active %s markets found.", timeframe)
	}

	// 1.2 Asset selection
	var selectedAsset string
	var assetNames []string
	for k := range markets {
		assetNames = append(assetNames, k)
	}
	sort.Strings(assetNames)

	if len(markets) > 1 {
		fmt.Printf("Assets found: [%s]. Choose one: ", strings.Join(assetNames, ", "))
		_, _ = fmt.Scanln(&selectedAsset)
		selectedAsset = strings.ToUpper(selectedAsset)
	} else {
		selectedAsset = assetNames[0]
	}

	market, ok := markets[selectedAsset]
	if !ok {
		log.Fatal("Invalid asset selected.")
	}
	if err := trader.SubscribeUserWSMarkets(ctx, market.ConditionID); err != nil {
		fmt.Printf("⚠️ Failed to subscribe User WS for %s: %v\n", market.Slug, err)
	} else {
		fmt.Println("✅ User WebSocket subscribed for selected market")
	}

	// 1.3 WebSocket Setup
	wsMgr := api.NewWSManager("")
	if err := wsMgr.Connect(ctx); err != nil {
		fmt.Printf("⚠️ WS failed: %v\n", err)
	}
	defer wsMgr.Close()

	tokenToOutcome := make(map[string]string)
	tokenMap := make(map[string]string)
	var outcomes []string
	var assetIDs []string
	for _, t := range market.Tokens {
		assetIDs = append(assetIDs, t.TokenID)
		tokenToOutcome[t.TokenID] = t.Outcome
		tokenMap[t.TokenID] = t.Outcome
		outcomes = append(outcomes, t.Outcome)
	}
	sort.Strings(outcomes)

	if wsMgr.IsConnected() {
		_ = wsMgr.Subscribe(ctx, map[string]interface{}{"type": "market", "assets_ids": assetIDs})
	}
	wsMsgChan := wsMgr.StartStreaming(ctx)

	quoteStore := newUtilbotQuoteStore()
	tokenFeeRates := make(map[string]int)
	for tid, out := range tokenMap {
		var rate int
		var err error
		for attempt := 1; attempt <= 3; attempt++ {
			rate, err = client.GetFeeRate(ctx, tid)
			if err == nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}

		if err == nil {
			tokenFeeRates[out] = rate
			// markets might require 1000 bps authorization even if endpoint returns 0
			if rate == 0 {
				tokenFeeRates[out] = 1000
				fmt.Printf("ℹ️  Fee rate for %s returned 0, forcing 1000 bps\n", out)
			} else {
				fmt.Printf("ℹ️  Fee rate for %s: %d bps\n", out, rate)
			}
		} else {
			tokenFeeRates[out] = 1000 // Fallback to 1000 bps (10%) as required by API
			fmt.Printf("⚠️  Fee fetch failed for %s, using 1000 bps fallback\n", out)
		}
	}

	// Input handler
	inputChan := make(chan string)
	go func() {
		reader := bufio.NewReader(os.Stdin)
		text, _ := reader.ReadString('\n')
		inputChan <- text
	}()

	fmt.Print("\033[?25l") // Hide cursor
	go utilbotRunQuotePump(ctx, client, tokenMap, tokenToOutcome, wsMsgChan, quoteStore)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Render
			snap := quoteStore.Snapshot(outcomes)
			fmt.Print("\033[H\033[2J")
			fmt.Printf("🚀 Market: %s\n", market.Slug)
			endTime, err := resolveUtilbotMarketEndTime(market)
			if err != nil {
				fmt.Printf("⏰ Time Left: unknown (%v)\n\n", err)
			} else {
				fmt.Printf("⏰ Time Left: %v\n\n", time.Until(endTime).Round(time.Second))
			}
			fmt.Println("Outcome      | Bid (Size)       | Ask (Size)       | Spread")
			fmt.Println("-------------|------------------|------------------|-------")
			for _, out := range outcomes {
				b, a := snap.TokenBids[out], snap.TokenAsks[out]
				bs, as := 0.0, 0.0
				if len(snap.TokenFullBids[out]) > 0 {
					bs = snap.TokenFullBids[out][0].Size
				}
				if len(snap.TokenFullAsks[out]) > 0 {
					as = snap.TokenFullAsks[out][0].Size
				}
				fmt.Printf("%-12s | %5.3f (%-6.0f) | %5.3f (%-6.0f) | %5.3f\n", out, b, bs, a, as, a-b)
			}
			sum := snap.TokenAsks[outcomes[0]] + snap.TokenAsks[outcomes[1]]
			if sum < 1.0 && sum > 0.1 {
				fmt.Printf("\n💰 ARB: %.3f (%.1f%%)\n", sum, (1-sum)*100)
			}
			fmt.Println("\nPress ENTER to stop live view and take action...")
		case <-inputChan:
			goto takeAction
		case <-ctx.Done():
			fmt.Print("\033[?25h")
			return
		}
	}

takeAction:
	fmt.Print("\033[?25h")
	fmt.Println("\nActions: 1:Panic Buy, 2:Panic Sell")
	fmt.Print("Choice: ")
	var choice int
	_, _ = fmt.Scanln(&choice)

	var inputAmount float64
	if choice == 1 {
		fmt.Print("Pairs to Panic Buy (shares per side): ")
		_, _ = fmt.Scanln(&inputAmount)
		executeBoth(ctx, trader, client, cfg, market, outcomes, "BUY", inputAmount, quoteStore, tokenFeeRates)
	} else if choice == 2 {
		fmt.Print("Pairs to Panic Sell (shares per side): ")
		_, _ = fmt.Scanln(&inputAmount)
		executeBoth(ctx, trader, client, cfg, market, outcomes, "SELL", inputAmount, quoteStore, tokenFeeRates)
	} else {
		log.Fatal("Invalid choice.")
	}
}

func findMarkets(ctx context.Context, restClient *api.RestClient, timeframe string) map[string]*api.Market {
	assets := []string{"btc", "eth"}

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			if ms, err := restClient.GetMarketsByTimeframe(ctx, nil, timeframe); err == nil {
				found := pickUtilbotMarkets(time.Now(), ms, timeframe, assets)
				if len(found) > 0 {
					return found
				}
			}
			fmt.Print(".")
			time.Sleep(utilbotFinderPollInterval(timeframe))
		}
	}
}

func pickUtilbotMarkets(now time.Time, markets []api.Market, timeframe string, assets []string) map[string]*api.Market {
	type candidate struct {
		market   *api.Market
		timeLeft time.Duration
	}

	best := make(map[string]candidate)
	for _, market := range markets {
		if !market.Active || market.Closed {
			continue
		}

		endTime, err := resolveUtilbotMarketEndTime(&market)
		if err != nil || !isUtilbotMarketInEntryWindow(now, endTime, timeframe) {
			continue
		}

		timeLeft := endTime.Sub(now)
		slug := strings.ToLower(market.Slug)
		for _, asset := range assets {
			if !strings.Contains(slug, strings.ToLower(asset)) {
				continue
			}

			key := strings.ToUpper(asset)
			existing, ok := best[key]
			if ok && existing.timeLeft <= timeLeft {
				continue
			}

			marketCopy := market
			marketCopy.EndTime = endTime
			best[key] = candidate{market: &marketCopy, timeLeft: timeLeft}
		}
	}

	found := make(map[string]*api.Market, len(best))
	for key, candidate := range best {
		found[key] = candidate.market
	}
	return found
}

func resolveUtilbotMarketEndTime(market *api.Market) (time.Time, error) {
	if market == nil {
		return time.Time{}, fmt.Errorf("market is nil")
	}
	if !market.EndTime.IsZero() {
		return market.EndTime, nil
	}
	return paper.ParseEndTimeFromSlug(market.Slug)
}

func isUtilbotMarketInEntryWindow(now, endTime time.Time, timeframe string) bool {
	if !now.Before(endTime) {
		return false
	}

	timeLeft := endTime.Sub(now)
	minTimeLeft, maxTimeLeft, hasUpperBound := utilbotEntryWindow(timeframe)
	if timeLeft < minTimeLeft {
		return false
	}
	if hasUpperBound && timeLeft > maxTimeLeft {
		return false
	}

	return true
}

func utilbotEntryWindow(timeframe string) (minTimeLeft, maxTimeLeft time.Duration, hasUpperBound bool) {
	switch strings.ToLower(strings.TrimSpace(timeframe)) {
	case "15m":
		// Utility bot should prefer 15m markets after the opening noise but before
		// the final rush, focusing on the 3m-14m pre-expiry range.
		return 3 * time.Minute, 14 * time.Minute, true
	default:
		return 1 * time.Minute, 0, false
	}
}

func utilbotFinderPollInterval(timeframe string) time.Duration {
	switch strings.ToLower(strings.TrimSpace(timeframe)) {
	case "15m":
		return 500 * time.Millisecond
	default:
		return 1 * time.Second
	}
}

func executeBoth(ctx context.Context, trader *trading.RealTrader, restClient *api.RestClient, cfg *core.Config, market *api.Market, outcomes []string, side string, targetShares float64, quoteStore *utilbotQuoteStore, tokenFeeRates map[string]int) {
	snapshot := quoteStore.Snapshot(outcomes)
	tokenFullBids := snapshot.TokenFullBids
	tokenFullAsks := snapshot.TokenFullAsks
	quoteState := snapshot.QuoteState
	// Determine execution pricing
	prices := make(map[string]float64, len(outcomes))
	buyCaps := make(map[string]float64, len(outcomes))
	token0 := mkt.GetTokenIDForOutcome(market, outcomes[0])
	token1 := mkt.GetTokenIDForOutcome(market, outcomes[1])
	var initialBal0, initialBal1 float64
	haveInitialSnapshot := false
	if side == "BUY" {
		b0, err0 := trader.GetCTFBalanceFloat(ctx, token0)
		b1, err1 := trader.GetCTFBalanceFloat(ctx, token1)
		if err0 == nil && err1 == nil {
			initialBal0 = b0
			initialBal1 = b1
			haveInitialSnapshot = true
		} else {
			fmt.Printf("⚠️ Could not snapshot pre-buy CTF balances (err0=%v, err1=%v). Merge will use live balances only.\n", err0, err1)
		}
	}
	for _, out := range outcomes {
		var price float64
		if side == "BUY" {
			price = 0.99
			if bestAsk, found := utilbotBestAskFromLevels(tokenFullAsks[out]); found {
				price = bestAsk
			}
		} else {
			price = 0.01
			if bestBid, found := utilbotBestBidFromLevels(tokenFullBids[out]); found {
				price = bestBid
			}
			if price >= 1.0 {
				price = 0.99
			} else if price < 0.01 {
				price = 0.01
			}
		}
		prices[out] = price
	}
	refreshBuyCaps := func() error {
		if side != "BUY" {
			return nil
		}
		cap0, cap1, err := core.BuyExecutionLimitPrices(
			prices[outcomes[0]], prices[outcomes[1]],
			cfg.MinAskPrice, cfg.MaxAskPrice, cfg.BuyExecutionMarginFloorPercent,
		)
		if err != nil {
			return err
		}
		buyCaps[outcomes[0]] = cap0
		buyCaps[outcomes[1]] = cap1
		return nil
	}
	if err := refreshBuyCaps(); err != nil {
		fmt.Printf("❌ Buy pricing rejected by config: %v\n", err)
		return
	}

	shares := targetShares

	if side == "BUY" {
		buyMaxSum := core.MaxExecutablePairSum(cfg.BuyExecutionMarginFloorPercent, cfg.MaxAskPrice)
		totalLiq := mkt.EstimateMatchedLiquidity(
			append([]paper.MarketLevel(nil), tokenFullAsks[outcomes[0]]...),
			append([]paper.MarketLevel(nil), tokenFullAsks[outcomes[1]]...),
			func(i, j int, levels []paper.MarketLevel) bool { return levels[i].Price < levels[j].Price },
			func(p1, p2 float64) bool { return p1+p2 <= buyMaxSum },
		)
		if shares > totalLiq {
			fmt.Printf("⚠️  Capping by liquidity: %.6f -> %.6f pairs\n", shares, totalLiq)
			shares = totalLiq
		}

		adjustedShares, bumped := normalizePanicBuySharesPerSide(shares, prices[outcomes[0]], prices[outcomes[1]])
		if bumped {
			if totalLiq > 0 && adjustedShares > totalLiq {
				fmt.Printf("⚠️ Need %.6f shares/side to satisfy Polymarket's ~$1 minimum per leg, but only %.6f pair liquidity is currently available. Keeping liquidity cap.\n", adjustedShares, totalLiq)
			} else {
				fmt.Printf("ℹ️ Raised buy target from %.6f to %.6f pairs (shares per side) to satisfy Polymarket's ~$1 minimum per leg. Very cheap asks are floored at $0.10 for sizing so utilbot doesn't overshoot into huge imbalances.\n", shares, adjustedShares)
				shares = adjustedShares
			}
		}
	} else {
		sellExecutionFloor := core.ClampExecutionMarginFloor(cfg.SplitMinMarginSell, cfg.BuyExecutionMarginFloorPercent)
		sellMinSum := core.MinExecutablePairSum(sellExecutionFloor, cfg.MinAskPrice)
		totalLiq := mkt.EstimateMatchedLiquidity(
			append([]paper.MarketLevel(nil), tokenFullBids[outcomes[0]]...),
			append([]paper.MarketLevel(nil), tokenFullBids[outcomes[1]]...),
			func(i, j int, levels []paper.MarketLevel) bool { return levels[i].Price > levels[j].Price },
			func(p1, p2 float64) bool { return p1+p2 >= sellMinSum },
		)
		if shares > totalLiq {
			fmt.Printf("⚠️  Capping by liquidity: %.6f -> %.6f pairs\n", shares, totalLiq)
			shares = totalLiq
		}
	}

	totalValue := shares * (prices[outcomes[0]] + prices[outcomes[1]])

	expectedFee0 := shares * (float64(tokenFeeRates[outcomes[0]]) / 10000.0)
	expectedFee1 := shares * (float64(tokenFeeRates[outcomes[1]]) / 10000.0)

	if side == "BUY" {
		fmt.Printf("💸 Expected fee deduction: %s=%.4f shares, %s=%.4f shares\n", outcomes[0], expectedFee0, outcomes[1], expectedFee1)
	} else {
		expectedUsdcFee0 := expectedFee0 * prices[outcomes[0]]
		expectedUsdcFee1 := expectedFee1 * prices[outcomes[1]]
		fmt.Printf("💸 Expected fee deduction: %s=$%.4f, %s=$%.4f\n", outcomes[0], expectedUsdcFee0, outcomes[1], expectedUsdcFee1)
	}

	fmt.Printf("🚀 Executing: %s %.6f pairs (%.6f shares/side, Est. total value: $%.2f USDC). Confirm? (y/n): ", side, shares, shares, totalValue)
	var confirm string
	_, _ = fmt.Scanln(&confirm)
	if strings.ToLower(strings.TrimSpace(confirm)) != "y" {
		log.Fatal("Cancelled.")
	}
	if side == "BUY" {
		latestSnapshot := quoteStore.Snapshot(outcomes)
		tokenFullBids = latestSnapshot.TokenFullBids
		tokenFullAsks = latestSnapshot.TokenFullAsks
		quoteState = latestSnapshot.QuoteState
		for _, out := range outcomes {
			if bestAsk, found := utilbotBestAskFromLevels(tokenFullAsks[out]); found {
				prices[out] = bestAsk
			}
		}
		if fresh, maxAge, reason := utilbotCanUseLocalBuyQuote(time.Now(), outcomes, tokenFullAsks, quoteState, utilbotLocalQuoteMaxAge); fresh {
			if err := refreshBuyCaps(); err != nil {
				fmt.Printf("❌ Local quote rejected by config: %v. Aborting before submission.\n", err)
				return
			}
			cap0 := buyCaps[outcomes[0]]
			cap1 := buyCaps[outcomes[1]]
			updatedTotalValue := shares * (prices[outcomes[0]] + prices[outcomes[1]])
			fmt.Printf("⚡ Using fresh local quote (max age %s): %s=%.3f (cap %.3f), %s=%.3f (cap %.3f) | Est. total: $%.2f\n", maxAge.Round(time.Millisecond), outcomes[0], prices[outcomes[0]], cap0, outcomes[1], prices[outcomes[1]], cap1, updatedTotalValue)
		} else {
			fmt.Printf("🔄 Local quote not fresh enough (%s). Re-quoting both legs just before submission...\n", reason)
			requoteCtx, cancelRequote := context.WithTimeout(ctx, utilbotJITRequoteTimeout)
			latency, err := utilbotRefreshExecutionBooks(requoteCtx, restClient, market, outcomes, side, tokenFullBids, tokenFullAsks, prices)
			cancelRequote()
			if err != nil {
				fmt.Printf("❌ Re-quote failed: %v. Aborting before submission.\n", err)
				return
			}
			if err := refreshBuyCaps(); err != nil {
				fmt.Printf("❌ Fresh quote rejected by config: %v. Aborting before submission.\n", err)
				return
			}
			cap0 := buyCaps[outcomes[0]]
			cap1 := buyCaps[outcomes[1]]
			updatedTotalValue := shares * (prices[outcomes[0]] + prices[outcomes[1]])
			fmt.Printf("✅ Fresh asks: %s=%.3f (cap %.3f), %s=%.3f (cap %.3f) | Updated est. total: $%.2f | rest=%s\n", outcomes[0], prices[outcomes[0]], cap0, outcomes[1], prices[outcomes[1]], cap1, updatedTotalValue, latency.Round(time.Millisecond))
		}
	}

	var initialUSDC float64
	if side == "SELL" {
		initialUSDC, _ = trader.ForceRefreshBalance(ctx)

		splitAmount := shares // 1 USDC per split → 1 YES + 1 NO
		fmt.Printf("🔄 Splitting $%.6f USDC into %.6f token pairs...\n", splitAmount, splitAmount)
		splitCtx, cancelSplit := context.WithTimeout(ctx, 90*time.Second)
		defer cancelSplit()
		tx, err := trader.SplitOnChain(splitCtx, market.ConditionID, splitAmount, len(market.Tokens))
		if err != nil {
			log.Fatalf("❌ Split failed: %v", err)
		}
		fmt.Printf("✅ Split successful! Tx: %s\n", tx)
		fmt.Println("⏳ Waiting for on-chain settlement...")
		time.Sleep(3 * time.Second)
	}
	results, errs := make([]*trading.TradeResult, 2), make([]error, 2)
	batchReqs := make([]*api.OrderRequest, 0, len(outcomes))
	batchMeta := make([]struct {
		outcome string
		shares  float64
		rate    int
	}, 0, len(outcomes))

	for _, out := range outcomes {
		tid := mkt.GetTokenIDForOutcome(market, out)
		rate := tokenFeeRates[out]
		if rate == -1 {
			rate = 0 // Default to 0 (fee-free) if fetch failed, safer than 1000
			log.Printf("⚠️ Fee rate fetch failed for %s, using 0 bps", out)
		}

		price := 0.01
		if side == "BUY" {
			price = buyCaps[out]
			if price <= 0 {
				idx := len(batchMeta)
				errs[idx] = fmt.Errorf("missing configured buy cap for %s", out)
				batchMeta = append(batchMeta, struct {
					outcome string
					shares  float64
					rate    int
				}{outcome: out, shares: shares, rate: rate})
				continue
			}
		}

		batchReqs = append(batchReqs, &api.OrderRequest{
			TokenID:     tid,
			Price:       price,
			Size:        shares,
			Side:        map[string]api.Side{"BUY": api.SideBuy, "SELL": api.SideSell}[side],
			OrderType:   api.OrderTypeLimit,
			TimeInForce: api.TIFFillAndKill,
			FeeRateBps:  rate,
		})
		batchMeta = append(batchMeta, struct {
			outcome string
			shares  float64
			rate    int
		}{outcome: out, shares: shares, rate: rate})
	}

	startReq := time.Now()
	if len(batchReqs) > 0 {
		batchResults, batchErr := trader.ExecuteBatch(ctx, batchReqs)
		latency := time.Since(startReq)
		resultIdx := 0
		for i := range batchMeta {
			if errs[i] != nil {
				printTradeResult(side+" "+batchMeta[i].outcome, results[i], errs[i], batchMeta[i].rate, batchMeta[i].shares, latency)
				continue
			}
			if batchErr != nil {
				errs[i] = batchErr
			} else if resultIdx < len(batchResults) {
				results[i] = batchResults[resultIdx]
			} else {
				errs[i] = fmt.Errorf("missing batch result for %s", batchMeta[i].outcome)
			}
			printTradeResult(side+" "+batchMeta[i].outcome, results[i], errs[i], batchMeta[i].rate, batchMeta[i].shares, latency)
			resultIdx++
		}
	}

	if side == "BUY" && utilbotAnyTradeSucceeded(results, errs) {
		if tradeSucceeded(results[0], errs[0]) && tradeSucceeded(results[1], errs[1]) {
			fmt.Println("💰 Buy success! Querying on-chain balances for merge/cleanup...")
		} else {
			fmt.Println("⚠️ Partial/unbalanced buy detected. Using live balances to clean up actual filled shares...")
		}
		finalizeUtilbotBuy(ctx, trader, cfg, market, outcomes, [2]string{token0, token1}, tokenFeeRates, shares, haveInitialSnapshot, initialBal0, initialBal1)
	} else if side == "SELL" {
		if tradeSucceeded(results[0], errs[0]) && tradeSucceeded(results[1], errs[1]) {
			fmt.Println("💰 Sell success! Waiting for on-chain balances to update...")
			time.Sleep(3 * time.Second)

			finalUSDC, err := trader.ForceRefreshBalance(ctx)
			if err != nil {
				fmt.Printf("⚠️ Failed to fetch final USDC balance: %v\n", err)
			} else {
				// We started with initialUSDC, split 'shares' amount, then sold.
				// actual received from the sale = finalUSDC - (initialUSDC - shares)
				expectedRemaining := initialUSDC - shares
				actualReceived := finalUSDC - expectedRemaining

				// Expected to receive totalValue, but paid fee and slippage
				expectedReceived := totalValue
				totalActualFee := expectedReceived - actualReceived
				if totalActualFee < 0 {
					totalActualFee = 0
				}

				fmt.Printf("📊 On-chain USDC: Initial=$%.2f, Final=$%.2f\n", initialUSDC, finalUSDC)
				fmt.Printf("💵 Actual Received: $%.4f USDC (Expected ~$%.2f)\n", actualReceived, expectedReceived)
				fmt.Printf("💸 ACTUAL DEDUCTED FEE & SLIPPAGE: $%.4f USDC\n", totalActualFee)
			}
		}
	}
}

func normalizePanicBuySharesPerSide(requested float64, bookAsks ...float64) (float64, bool) {
	minShares := requested
	for _, ask := range bookAsks {
		effectiveAsk := ask
		if effectiveAsk < 0.10 {
			effectiveAsk = 0.10
		}
		if effectiveAsk <= 0 || effectiveAsk >= 1 {
			continue
		}
		requiredShares := math.Ceil((1.0/effectiveAsk)*10000) / 10000
		if requiredShares > minShares {
			minShares = requiredShares
		}
	}
	if minShares > requested+0.0000001 {
		return minShares, true
	}
	return requested, false
}

func utilbotBestAskFromLevels(levels []paper.MarketLevel) (float64, bool) {
	bestAsk := 1.0
	found := false
	for _, lvl := range levels {
		if lvl.Price > 0 && lvl.Price < bestAsk {
			bestAsk = lvl.Price
			found = true
		}
	}
	if !found {
		return 0, false
	}
	return bestAsk, true
}

func utilbotBestBidFromLevels(levels []paper.MarketLevel) (float64, bool) {
	bestBid := 0.0
	found := false
	for _, lvl := range levels {
		if lvl.Price > bestBid {
			bestBid = lvl.Price
			found = true
		}
	}
	if !found {
		return 0, false
	}
	return bestBid, true
}

func utilbotRunQuotePump(ctx context.Context, client *api.RestClient, tokenMap, tokenToOutcome map[string]string, wsMsgChan <-chan []byte, store *utilbotQuoteStore) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-wsMsgChan:
			if !ok {
				wsMsgChan = nil
				continue
			}
			books, err := api.ParseOrderBooks(msg)
			if err != nil {
				continue
			}
			updatedAt := time.Now()
			for _, b := range books {
				out := tokenToOutcome[b.AssetID]
				if out == "" {
					continue
				}
				bid, ask := 0.0, 1.0
				for _, order := range b.Bids {
					p, _ := strconv.ParseFloat(order.Price, 64)
					if p > bid {
						bid = p
					}
				}
				for _, order := range b.Asks {
					p, _ := strconv.ParseFloat(order.Price, 64)
					if p < ask && p > 0 {
						ask = p
					}
				}
				if ask >= 1.0 {
					ask = 0
				}
				store.Update(out, bid, ask, mkt.LevelsToPriceDepth(b.Bids, true), mkt.LevelsToPriceDepth(b.Asks, false), "ws", updatedAt)
			}
		case <-ticker.C:
			for tid, out := range tokenMap {
				book, err := client.GetOrderBook(ctx, tid)
				if err != nil {
					continue
				}
				updatedAt := time.Now()
				bid, ask := 0.0, 1.0
				for _, b := range book.Bids {
					p, _ := strconv.ParseFloat(b.Price, 64)
					if p > bid {
						bid = p
					}
				}
				for _, a := range book.Asks {
					p, _ := strconv.ParseFloat(a.Price, 64)
					if p < ask && p > 0 {
						ask = p
					}
				}
				if ask >= 1.0 {
					ask = 0
				}
				store.Update(out, bid, ask, mkt.LevelsToPriceDepth(book.Bids, true), mkt.LevelsToPriceDepth(book.Asks, false), "rest", updatedAt)
			}
		}
	}
}

func utilbotCanUseLocalBuyQuote(now time.Time, outcomes []string, tokenFullAsks map[string][]paper.MarketLevel, quoteState map[string]utilbotQuoteState, maxAge time.Duration) (bool, time.Duration, string) {
	maxObservedAge := time.Duration(0)
	for _, out := range outcomes {
		if len(tokenFullAsks[out]) == 0 {
			return false, maxObservedAge, fmt.Sprintf("missing local ask depth for %s", out)
		}
		state, ok := quoteState[out]
		if !ok || state.UpdatedAt.IsZero() {
			return false, maxObservedAge, fmt.Sprintf("missing quote timestamp for %s", out)
		}
		age := now.Sub(state.UpdatedAt)
		if age > maxObservedAge {
			maxObservedAge = age
		}
		if age > maxAge {
			return false, maxObservedAge, fmt.Sprintf("%s quote age %s > %s", out, age.Round(time.Millisecond), maxAge)
		}
	}
	return true, maxObservedAge, ""
}

func utilbotRefreshExecutionBooks(ctx context.Context, restClient *api.RestClient, market *api.Market, outcomes []string, side string, tokenFullBids, tokenFullAsks map[string][]paper.MarketLevel, prices map[string]float64) (time.Duration, error) {
	type quoteResult struct {
		outcome string
		bids    []paper.MarketLevel
		asks    []paper.MarketLevel
		latency time.Duration
		err     error
	}

	results := make(chan quoteResult, len(outcomes))
	for _, out := range outcomes {
		tokenID := mkt.GetTokenIDForOutcome(market, out)
		if tokenID == "" {
			return 0, fmt.Errorf("missing token id for outcome %s", out)
		}
		go func(outcome, token string) {
			start := time.Now()
			book, err := restClient.GetOrderBook(ctx, token)
			latency := time.Since(start)
			if err != nil {
				results <- quoteResult{outcome: outcome, latency: latency, err: err}
				return
			}
			results <- quoteResult{
				outcome: outcome,
				bids:    mkt.LevelsToPriceDepth(book.Bids, true),
				asks:    mkt.LevelsToPriceDepth(book.Asks, false),
				latency: latency,
			}
		}(out, tokenID)
	}

	maxLatency := time.Duration(0)
	for i := 0; i < len(outcomes); i++ {
		res := <-results
		if res.latency > maxLatency {
			maxLatency = res.latency
		}
		if res.err != nil {
			return maxLatency, fmt.Errorf("fetching fresh order book for %s failed: %w", res.outcome, res.err)
		}
		tokenFullBids[res.outcome] = res.bids
		tokenFullAsks[res.outcome] = res.asks

		if side == "BUY" {
			bestAsk, found := utilbotBestAskFromLevels(tokenFullAsks[res.outcome])
			if !found {
				return maxLatency, fmt.Errorf("no live ask found for %s", res.outcome)
			}
			prices[res.outcome] = bestAsk
			continue
		}

		bestBid, found := utilbotBestBidFromLevels(tokenFullBids[res.outcome])
		if !found {
			return maxLatency, fmt.Errorf("no live bid found for %s", res.outcome)
		}
		prices[res.outcome] = bestBid
	}
	return maxLatency, nil
}

func utilbotBuyLimitPrice(bookAsk, pairedAsk float64, cfg *core.Config) (float64, error) {
	price, _, err := core.BuyExecutionLimitPrices(bookAsk, pairedAsk, cfg.MinAskPrice, cfg.MaxAskPrice, cfg.BuyExecutionMarginFloorPercent)
	if err != nil {
		return 0, err
	}
	return price, nil
}

func tradeSucceeded(res *trading.TradeResult, err error) bool {
	if err != nil || res == nil || !res.Success {
		return false
	}
	return strings.TrimSpace(res.OrderID) != "" || strings.TrimSpace(res.Status) != ""
}

func utilbotAnyTradeSucceeded(results []*trading.TradeResult, errs []error) bool {
	for i, res := range results {
		var err error
		if i < len(errs) {
			err = errs[i]
		}
		if tradeSucceeded(res, err) {
			return true
		}
	}
	return false
}

func incrementalBalance(initial, current float64) float64 {
	return math.Max(0, current-initial)
}

func incrementalBalancedPairs(initial0, initial1, current0, current1 float64) float64 {
	return math.Min(incrementalBalance(initial0, current0), incrementalBalance(initial1, current1))
}

func utilbotBalancedAndExcessShares(acquired0, acquired1 float64) (balanced, excess0, excess1 float64) {
	balanced = math.Min(acquired0, acquired1)
	excess0 = math.Max(0, acquired0-balanced)
	excess1 = math.Max(0, acquired1-balanced)
	return balanced, excess0, excess1
}

func utilbotAcquiredShares(bal0, bal1, initial0, initial1 float64, haveInitialSnapshot bool) (float64, float64) {
	if haveInitialSnapshot {
		return incrementalBalance(initial0, bal0), incrementalBalance(initial1, bal1)
	}
	return math.Max(0, bal0), math.Max(0, bal1)
}

func utilbotQueryBuyBalanceDelta(ctx context.Context, trader *trading.RealTrader, tokenIDs [2]string, initialBal0, initialBal1 float64, haveInitialSnapshot bool) (acquired0, acquired1, bal0, bal1 float64, err0, err1 error) {
	const maxAttempts = 5
	const settleDelay = 750 * time.Millisecond

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		bal0, err0 = trader.GetCTFBalanceFloat(ctx, tokenIDs[0])
		bal1, err1 = trader.GetCTFBalanceFloat(ctx, tokenIDs[1])
		if err0 == nil && err1 == nil {
			acquired0, acquired1 = utilbotAcquiredShares(bal0, bal1, initialBal0, initialBal1, haveInitialSnapshot)
			if acquired0 >= 0.000001 || acquired1 >= 0.000001 || attempt == maxAttempts {
				return acquired0, acquired1, bal0, bal1, nil, nil
			}
		}
		if attempt < maxAttempts {
			select {
			case <-ctx.Done():
				return acquired0, acquired1, bal0, bal1, ctx.Err(), ctx.Err()
			case <-time.After(settleDelay):
			}
		}
	}

	return acquired0, acquired1, bal0, bal1, err0, err1
}

func finalizeUtilbotBuy(ctx context.Context, trader *trading.RealTrader, cfg *core.Config, market *api.Market, outcomes []string, tokenIDs [2]string, tokenFeeRates map[string]int, requestedShares float64, haveInitialSnapshot bool, initialBal0, initialBal1 float64) {
	time.Sleep(3 * time.Second)
	fmt.Printf("🔍 Querying balances for tokens: %s (%s), %s (%s)\n", outcomes[0], tokenIDs[0], outcomes[1], tokenIDs[1])

	queryCtx, cancelQuery := context.WithTimeout(ctx, 10*time.Second)
	defer cancelQuery()
	acquired0, acquired1, bal0, bal1, err0, err1 := utilbotQueryBuyBalanceDelta(queryCtx, trader, tokenIDs, initialBal0, initialBal1, haveInitialSnapshot)
	if err0 != nil || err1 != nil {
		fmt.Printf("⚠️ Failed to verify buy balances (err0=%v, err1=%v). Skipping merge/cleanup.\n", err0, err1)
		return
	}

	shortfall0 := math.Max(0, requestedShares-acquired0)
	shortfall1 := math.Max(0, requestedShares-acquired1)
	fmt.Printf("📊 On-chain balances: %s=%.4f, %s=%.4f\n", outcomes[0], bal0, outcomes[1], bal1)
	if haveInitialSnapshot {
		fmt.Printf("🆕 Newly acquired since pre-buy snapshot: %s=%.4f, %s=%.4f\n", outcomes[0], acquired0, outcomes[1], acquired1)
		preExistingPairs := math.Min(initialBal0, initialBal1)
		if preExistingPairs >= 0.000001 {
			fmt.Printf("ℹ️ Pre-existing balanced inventory left untouched: %.6f pairs\n", preExistingPairs)
		}
	}
	fmt.Printf("📉 Shortfall vs requested size: %s=%.4f shares, %s=%.4f shares\n", outcomes[0], shortfall0, outcomes[1], shortfall1)

	minQty, excess0, excess1 := utilbotBalancedAndExcessShares(acquired0, acquired1)
	if excess0 >= 0.000001 || excess1 >= 0.000001 {
		fmt.Printf("⚖️ Share imbalance detected: %s excess=%.4f, %s excess=%.4f\n", outcomes[0], excess0, outcomes[1], excess1)
	}

	if minQty >= 0.000001 {
		fmt.Printf("🔄 Merging %.6f balanced pairs...\n", minQty)
		mergeCtx, cancelMerge := context.WithTimeout(ctx, 90*time.Second)
		tx, mergeErr := trader.MergeOnChain(mergeCtx, market.ConditionID, minQty, len(market.Tokens))
		cancelMerge()
		if mergeErr != nil {
			fmt.Printf("❌ Merge failed: %v\n", mergeErr)
		} else {
			fmt.Printf("✅ Merge successful! Tx: %s\n", tx)
		}
	} else {
		fmt.Printf("ℹ️ No balanced pairs available to merge (min=%.6f).\n", minQty)
	}

	cleanupCtx, cancelCleanup := context.WithTimeout(ctx, 30*time.Second)
	defer cancelCleanup()
	cleanupUtilbotExcessShares(cleanupCtx, trader, cfg, outcomes, tokenIDs, [2]float64{excess0, excess1}, tokenFeeRates)
}

func cleanupUtilbotExcessShares(ctx context.Context, trader *trading.RealTrader, cfg *core.Config, outcomes []string, tokenIDs [2]string, excesses [2]float64, tokenFeeRates map[string]int) {
	sellPrice := core.CleanupSellLimitPrice(cfg.MinAskPrice)
	for i, excess := range excesses {
		if excess < 0.01 {
			continue
		}

		outcome := outcomes[i]
		rate := tokenFeeRates[outcome]
		if rate == -1 {
			rate = 0
		}

		fmt.Printf("🧹 Auto-cleanup: selling %.4f excess %s shares...\n", excess, outcome)
		res, err := trader.Sell(ctx, tokenIDs[i], outcome, sellPrice, excess, api.OrderTypeLimit, api.TIFFillAndKill, rate)
		if err != nil {
			fmt.Printf("⚠️ Auto-cleanup sell failed for %s: %v\n", outcome, err)
			continue
		}
		if res == nil || !res.Success {
			msg := "unknown error"
			if res != nil && strings.TrimSpace(res.Message) != "" {
				msg = res.Message
			}
			fmt.Printf("⚠️ Auto-cleanup sell not filled for %s: %s\n", outcome, msg)
			continue
		}

		fmt.Printf("✅ Auto-cleanup sold excess %s shares. OrderID: %s\n", outcome, res.OrderID)
	}
}

func printTradeResult(act string, res *trading.TradeResult, err error, rate int, shares float64, latency time.Duration) {
	if !tradeSucceeded(res, err) {
		msg := "Error"
		if err != nil {
			msg = err.Error()
		} else if res != nil {
			msg = strings.TrimSpace(res.Message)
			if msg == "" && strings.TrimSpace(res.Status) != "" {
				msg = fmt.Sprintf("status=%s", res.Status)
			}
		}
		if msg == "" {
			msg = "empty order response"
		}
		fmt.Printf("FAILED: %s - %s (Latency: %v)\n", act, msg, latency)
	} else {
		actualFeeRate := float64(rate) / 10000.0
		feePercentage := float64(rate) / 100.0 // bps to percentage
		orderLabel := strings.TrimSpace(res.OrderID)
		if orderLabel == "" {
			orderLabel = fmt.Sprintf("status=%s", res.Status)
		}
		if strings.HasPrefix(act, "BUY") {
			// For BUY, fee is deducted from the shares you receive.
			feeShares := shares * actualFeeRate
			fmt.Printf("SUCCESS: %s - OrderID: %s | Estimated Fee (%.2f%%): %.4f shares (Latency: %v)\n", act, orderLabel, feePercentage, feeShares, latency)
		} else {
			feeUSDC := shares * actualFeeRate
			fmt.Printf("SUCCESS: %s - OrderID: %s | Estimated Fee (%.2f%%): $%.4f USDC (Latency: %v)\n", act, orderLabel, feePercentage, feeUSDC, latency)
		}
	}
}
