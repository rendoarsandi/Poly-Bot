package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"Market-bot/internal/core"
)

// maxResponseBodySize caps how many bytes we'll read from any API response.
// This converts what would be an unrecoverable bytes.ErrTooLarge panic (in the
// HTTP/2 read-loop goroutine) into an ordinary JSON decode error.
const maxResponseBodySize = 2 * 1024 * 1024 // 2 MB

const restRequestAttempts = 3

// httpClient is the shared HTTP client for all REST calls.
//
// HTTP/2 is enabled for connection multiplexing — all concurrent requests to
// the same host share a single TCP+TLS connection, eliminating per-request
// handshake overhead.  Response bodies are still capped via io.LimitReader
// (maxResponseBodySize) at every call site to prevent unbounded reads.
var httpClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:          500,
		MaxIdleConnsPerHost:   500,
		MaxConnsPerHost:       0,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true,
		ForceAttemptHTTP2:     true,
		ExpectContinueTimeout: 1 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 15 * time.Second, // Aggressive TCP keep-alive pings to bypass NAT/LB timeouts
		}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
	},
}

func restRetryDelay(attempt int) time.Duration {
	return time.Duration(75*(1<<attempt)) * time.Millisecond
}

func isTransientRESTStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func isTransientRESTError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "goaway") || strings.Contains(msg, "enhance_your_calm") {
		return true
	}
	if strings.Contains(msg, "http2") && (strings.Contains(msg, "stream") || strings.Contains(msg, "connection")) {
		return true
	}
	return strings.Contains(msg, "connection reset") || strings.Contains(msg, "broken pipe") || strings.Contains(msg, "unexpected eof") || strings.Contains(msg, "server closed idle connection")
}

func waitRESTRetry(ctx context.Context, attempt int) error {
	timer := time.NewTimer(restRetryDelay(attempt))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func closeTransientResponseBody(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
}

func doGETWithRetry(ctx context.Context, url string) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < restRequestAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return nil, err
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			if !isTransientRESTError(err) || attempt == restRequestAttempts-1 {
				return nil, err
			}
			httpClient.CloseIdleConnections()
			if waitErr := waitRESTRetry(ctx, attempt); waitErr != nil {
				return nil, waitErr
			}
			continue
		}

		if isTransientRESTStatus(resp.StatusCode) && attempt < restRequestAttempts-1 {
			closeTransientResponseBody(resp)
			httpClient.CloseIdleConnections()
			if waitErr := waitRESTRetry(ctx, attempt); waitErr != nil {
				return nil, waitErr
			}
			continue
		}

		return resp, nil
	}
	return nil, lastErr
}

type Token struct {
	TokenID string `json:"token_id"`
	Outcome string `json:"outcome"`
	Winner  bool   `json:"winner"`
}

type Market struct {
	Active      bool      `json:"active"`
	Closed      bool      `json:"closed"`
	ConditionID string    `json:"condition_id"`
	Slug        string    `json:"slug"`
	MarketSlug  string    `json:"market_slug"` // Used in list response
	EndTime     time.Time `json:"-"`
	Tokens      []Token   `json:"tokens"`
}

type ListMarketsResponse struct {
	Data []Market `json:"data"`
}

type ClobMarketToken struct {
	TokenID string `json:"t"`
	Outcome string `json:"o"`
}

type PolymarketFloat float64

func (f *PolymarketFloat) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) || len(data) == 0 {
		*f = 0
		return nil
	}
	var n float64
	if err := json.Unmarshal(data, &n); err == nil {
		*f = PolymarketFloat(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return err
	}
	*f = PolymarketFloat(n)
	return nil
}

type ClobMarketFeeDetails struct {
	Rate      PolymarketFloat `json:"r"`
	Exponent  PolymarketFloat `json:"e"`
	TakerOnly bool            `json:"to,omitempty"`
}

func (d *ClobMarketFeeDetails) Curve() core.PolymarketFeeCurve {
	if d == nil {
		return core.PolymarketFeeCurve{}
	}
	return core.PolymarketFeeCurve{Rate: float64(d.Rate), Exponent: float64(d.Exponent)}
}

type ClobMarketInfo struct {
	ConditionID string                `json:"c"`
	Tokens      []ClobMarketToken     `json:"t"`
	MinTickSize float64               `json:"mts"`
	NegRisk     bool                  `json:"nr"`
	FeeDetails  *ClobMarketFeeDetails `json:"fd,omitempty"`
}

type RestClient struct {
	Exchange      string
	KalshiBaseURL string

	BaseURL  string
	GammaURL string
	// Rate limiting: strictly enforce max requests per second
	limiter <-chan time.Time

	// Real-time order book cache
	bookMu     sync.RWMutex
	bookCache  map[string]*OrderBookResponse
	wsActive   func() bool
	wsProvider func(tokenID string) *OrderBookResponse
}

func NewRestClient(exchange string) *RestClient {
	limiter := time.NewTicker(time.Second / 500)

	client := &RestClient{
		Exchange:      exchange,
		BaseURL:       "https://clob.polymarket.com",
		GammaURL:      "https://gamma-api.polymarket.com",
		KalshiBaseURL: KalshiBaseURL,
		limiter:       limiter.C,
		bookCache:     make(map[string]*OrderBookResponse),
	}

	return client
}

type GammaEvent struct {
	Slug          string             `json:"slug"`
	EndDate       string             `json:"endDate"`
	EventMetadata GammaEventMetadata `json:"eventMetadata"`
	Markets       []GammaMarket      `json:"markets"`
}

type GammaMarket struct {
	ConditionID         string `json:"conditionId"`
	Slug                string `json:"slug"`
	EndDate             string `json:"endDate"`
	ClobTokenIds        string `json:"clobTokenIds"` // JSON-encoded string array
	Outcomes            string `json:"outcomes"`
	OutcomePrices       string `json:"outcomePrices"`       // JSON-encoded string array
	UMAResolutionStatus string `json:"umaResolutionStatus"` // "resolved" when winner is authoritative
	CustomLiveness      int    `json:"customLiveness"`      // seconds; 1h crypto commonly uses 600
	Active              bool   `json:"active"`
	Closed              bool   `json:"closed"`
	Events              []struct {
		EventMetadata GammaEventMetadata `json:"eventMetadata"`
	} `json:"events"`
}

type GammaEventMetadata struct {
	FinalPrice  PolymarketFloat `json:"finalPrice"`
	PriceToBeat PolymarketFloat `json:"priceToBeat"`
}

func (c *RestClient) GetEventByTokenID(ctx context.Context, tokenID string) (*GammaEvent, error) {
	url := fmt.Sprintf("%s/events?clobTokenIds=%s", c.GammaURL, tokenID)
	select {
	case <-c.limiter:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get event by token id, status code: %d", resp.StatusCode)
	}

	var events []GammaEvent
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBodySize)).Decode(&events); err != nil {
		return nil, err
	}

	if len(events) == 0 {
		return nil, fmt.Errorf("no events found for token id: %s", tokenID)
	}

	return &events[0], nil
}

func (c *RestClient) GetMarketsByEventSlug(ctx context.Context, slug string) ([]Market, error) {
	select {
	case <-c.limiter:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	lookupURL := fmt.Sprintf("%s/events?slug=%s", c.GammaURL, url.QueryEscape(slug))
	req, err := http.NewRequestWithContext(ctx, "GET", lookupURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch event by slug: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch event by slug: status %d", resp.StatusCode)
	}

	var events []GammaEvent
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBodySize)).Decode(&events); err != nil {
		return nil, fmt.Errorf("failed to decode event by slug response: %w", err)
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("no event found for slug: %s", slug)
	}

	markets, err := marketsFromGammaEvent(events[0], slug)
	if err != nil {
		return nil, err
	}
	return markets, nil
}

func marketsFromGammaEvent(event GammaEvent, fallbackSlug string) ([]Market, error) {
	eventSlug := core.SanitizeString(event.Slug)
	if eventSlug == "" {
		eventSlug = core.SanitizeString(fallbackSlug)
	}
	eventEndTime := parseGammaEventEndTime(event.EndDate)

	markets := make([]Market, 0, len(event.Markets))
	for _, gm := range event.Markets {
		market, ok := marketFromGammaMarketWithEvent(gm, eventSlug, eventEndTime, event.EventMetadata)
		if !ok {
			continue
		}
		markets = append(markets, *market)
	}

	if len(markets) == 0 {
		return nil, fmt.Errorf("no markets found for slug: %s", fallbackSlug)
	}

	return markets, nil
}

func marketFromGammaMarket(gm GammaMarket, fallbackSlug string, fallbackEndTime time.Time) (*Market, bool) {
	return marketFromGammaMarketWithEvent(gm, fallbackSlug, fallbackEndTime, GammaEventMetadata{})
}

func marketFromGammaMarketWithEvent(gm GammaMarket, fallbackSlug string, fallbackEndTime time.Time, eventMetadata GammaEventMetadata) (*Market, bool) {
	var tokenIDs []string
	if err := json.Unmarshal([]byte(gm.ClobTokenIds), &tokenIDs); err != nil || len(tokenIDs) < 2 {
		return nil, false
	}

	var outcomes []string
	if err := json.Unmarshal([]byte(gm.Outcomes), &outcomes); err != nil || len(outcomes) < 2 {
		outcomes = []string{"Up", "Down"}
	}

	marketSlug := core.SanitizeString(gm.Slug)
	if marketSlug == "" {
		marketSlug = core.SanitizeString(fallbackSlug)
	}
	marketEndTime := parseGammaEventEndTime(gm.EndDate)
	if marketEndTime.IsZero() {
		marketEndTime = fallbackEndTime
	}
	if !gammaEventMetadataAvailable(eventMetadata) && len(gm.Events) > 0 {
		eventMetadata = gm.Events[0].EventMetadata
	}
	resolved := strings.EqualFold(strings.TrimSpace(gm.UMAResolutionStatus), "resolved")
	proposedFinal := gammaProposedFinalReady(gm, marketEndTime, eventMetadata, time.Now())
	tokenWinners := gammaWinnerFlags(gm, outcomes, marketEndTime, eventMetadata)
	resolved = resolved || proposedFinal && hasAnyWinnerFlag(tokenWinners)

	return &Market{
		ConditionID: gm.ConditionID,
		Slug:        marketSlug,
		Active:      gm.Active,
		Closed:      gm.Closed || resolved,
		EndTime:     marketEndTime,
		Tokens: []Token{
			{TokenID: tokenIDs[0], Outcome: core.SanitizeString(outcomes[0]), Winner: len(tokenWinners) > 0 && tokenWinners[0]},
			{TokenID: tokenIDs[1], Outcome: core.SanitizeString(outcomes[1]), Winner: len(tokenWinners) > 1 && tokenWinners[1]},
		},
	}, true
}

func gammaWinnerFlags(market GammaMarket, outcomes []string, marketEndTime time.Time, eventMetadata GammaEventMetadata) []bool {
	if len(outcomes) == 0 {
		return nil
	}
	if gammaProposedFinalReady(market, marketEndTime, eventMetadata, time.Now()) {
		if flags := gammaWinnerFlagsFromEventMetadata(outcomes, eventMetadata); flags != nil {
			return flags
		}
	}
	if !strings.EqualFold(strings.TrimSpace(market.UMAResolutionStatus), "resolved") {
		return nil
	}
	return gammaWinnerFlagsFromOutcomePrices(market, outcomes)
}

func gammaWinnerFlagsFromOutcomePrices(market GammaMarket, outcomes []string) []bool {
	if len(outcomes) == 0 {
		return nil
	}

	var rawPrices []string
	if err := json.Unmarshal([]byte(market.OutcomePrices), &rawPrices); err != nil || len(rawPrices) != len(outcomes) {
		return nil
	}

	winnerIdx := -1
	winnerPrice := -1.0
	secondBest := -1.0
	for i, raw := range rawPrices {
		price, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return nil
		}
		if price > winnerPrice+1e-9 {
			secondBest = winnerPrice
			winnerPrice = price
			winnerIdx = i
			continue
		}
		if price > secondBest {
			secondBest = price
		}
	}

	if winnerIdx < 0 || winnerPrice < 0.5 || math.Abs(winnerPrice-secondBest) <= 1e-9 {
		return nil
	}

	flags := make([]bool, len(outcomes))
	flags[winnerIdx] = true
	return flags
}

func gammaWinnerFlagsFromEventMetadata(outcomes []string, metadata GammaEventMetadata) []bool {
	if len(outcomes) == 0 || !gammaEventMetadataAvailable(metadata) {
		return nil
	}
	winner := "Down"
	if float64(metadata.FinalPrice)+1e-9 >= float64(metadata.PriceToBeat) {
		winner = "Up"
	}
	flags := make([]bool, len(outcomes))
	for i, outcome := range outcomes {
		if strings.EqualFold(strings.TrimSpace(outcome), winner) {
			flags[i] = true
			return flags
		}
	}
	return nil
}

func gammaProposedFinalReady(market GammaMarket, marketEndTime time.Time, metadata GammaEventMetadata, now time.Time) bool {
	if !strings.EqualFold(strings.TrimSpace(market.UMAResolutionStatus), "proposed") {
		return false
	}
	if market.CustomLiveness <= 0 || marketEndTime.IsZero() || !gammaEventMetadataAvailable(metadata) {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	return !now.Before(marketEndTime.Add(time.Duration(market.CustomLiveness) * time.Second))
}

func gammaEventMetadataAvailable(metadata GammaEventMetadata) bool {
	return float64(metadata.FinalPrice) > 0 && float64(metadata.PriceToBeat) > 0
}

func hasAnyWinnerFlag(flags []bool) bool {
	for _, flag := range flags {
		if flag {
			return true
		}
	}
	return false
}

func (c *RestClient) fetchGammaMarkets(ctx context.Context, query url.Values) ([]GammaMarket, error) {
	select {
	case <-c.limiter:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	lookupURL := fmt.Sprintf("%s/markets?%s", c.GammaURL, query.Encode())
	req, err := http.NewRequestWithContext(ctx, "GET", lookupURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch gamma markets: status %d", resp.StatusCode)
	}

	var markets []GammaMarket
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBodySize)).Decode(&markets); err != nil {
		return nil, err
	}
	return markets, nil
}

func (c *RestClient) getGammaMarketByQuery(ctx context.Context, query url.Values, fallbackSlug string) (*Market, error) {
	markets, err := c.fetchGammaMarkets(ctx, query)
	if err != nil {
		return nil, err
	}
	for _, gm := range markets {
		market, ok := marketFromGammaMarket(gm, fallbackSlug, time.Time{})
		if ok {
			return market, nil
		}
	}
	return nil, fmt.Errorf("no gamma market found")
}

func (c *RestClient) GetGammaMarketBySlug(ctx context.Context, slug string) (*Market, error) {
	slug = core.SanitizeString(slug)
	if slug == "" {
		return nil, fmt.Errorf("missing gamma market slug")
	}
	query := url.Values{}
	query.Add("slug", slug)
	market, err := c.getGammaMarketByQuery(ctx, query, slug)
	if err == nil && market != nil {
		return market, nil
	}
	if eventMarket, eventErr := c.getGammaTimeframeMarket(ctx, slug); eventErr == nil && eventMarket != nil {
		return eventMarket, nil
	}
	return nil, err
}

func (c *RestClient) GetGammaMarketByConditionID(ctx context.Context, conditionID string) (*Market, error) {
	conditionID = strings.TrimSpace(conditionID)
	if conditionID == "" {
		return nil, fmt.Errorf("missing gamma condition id")
	}
	query := url.Values{}
	query.Add("condition_ids", conditionID)
	return c.getGammaMarketByQuery(ctx, query, "")
}

func (c *RestClient) kalshiGetMarketsByTimeframe(ctx context.Context, assets []string, timeframe string) ([]Market, error) {
	if len(assets) == 0 {
		assets = []string{"btc", "eth"}
	}
	var markets []Market

	// Default to 15m if not specified, since Kalshi uses explicit timeframe strings
	tfSuffix := "15M"
	if timeframe != "" {
		tfSuffix = strings.ToUpper(timeframe)
	}

	for _, asset := range assets {
		// Example kalshi series ticker for 15m: KXBTC15M
		seriesTicker := "KX" + strings.ToUpper(asset) + tfSuffix
		url := fmt.Sprintf("%s/events?series_ticker=%s&with_nested_markets=true&limit=100", c.KalshiBaseURL, seriesTicker)
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return nil, err
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			continue
		}

		var kalshiResp struct {
			Events []struct {
				EventTicker string `json:"event_ticker"`
				Markets     []struct {
					Ticker    string `json:"ticker"`
					Status    string `json:"status"`
					CloseTime string `json:"close_time"`
				} `json:"markets"`
			} `json:"events"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&kalshiResp); err != nil {
			resp.Body.Close()
			continue
		}
		resp.Body.Close()

		foundForAsset := 0
		for _, event := range kalshiResp.Events {
			for _, mkt := range event.Markets {
				if mkt.Status != "active" {
					continue
				}

				t, _ := time.Parse(time.RFC3339, mkt.CloseTime)
				if t.IsZero() {
					t = time.Now().Add(1 * time.Hour)
				}

				markets = append(markets, Market{
					ConditionID: mkt.Ticker, // Kalshi ticker
					Slug:        mkt.Ticker,
					Active:      true,
					Closed:      false,
					EndTime:     t,
					Tokens: []Token{
						{TokenID: mkt.Ticker + "-YES", Outcome: "Yes"},
						{TokenID: mkt.Ticker + "-NO", Outcome: "No"},
					},
				})

				foundForAsset++
				// Just grab up to 3 active markets for this asset
				if foundForAsset >= 3 {
					break
				}
			}
			if foundForAsset >= 3 {
				break
			}
		}
	}

	return markets, nil
}

func (c *RestClient) getGammaTimeframeMarket(ctx context.Context, slug string) (*Market, error) {
	select {
	case <-c.limiter:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	lookupURL := fmt.Sprintf("%s/events?slug=%s", c.GammaURL, url.QueryEscape(slug))
	req, err := http.NewRequestWithContext(ctx, "GET", lookupURL, nil)
	if err != nil {
		return nil, err
	}

	var resp *http.Response
	for attempt := 0; attempt < 3; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		resp, err = httpClient.Do(req)
		if err == nil {
			if resp.StatusCode == 429 {
				resp.Body.Close()
				time.Sleep(time.Duration(150*(attempt+1)) * time.Millisecond)
				continue
			}
			break
		}

		time.Sleep(time.Duration(150*(attempt+1)) * time.Millisecond)
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch market by slug %q: status %d", slug, resp.StatusCode)
	}

	var events []GammaEvent
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBodySize)).Decode(&events); err != nil {
		return nil, err
	}
	if len(events) == 0 || len(events[0].Markets) == 0 {
		return nil, nil
	}

	event := events[0]
	eventEndTime := parseGammaEventEndTime(event.EndDate)
	for _, gm := range event.Markets {
		market, ok := marketFromGammaMarket(gm, slug, eventEndTime)
		if ok {
			return market, nil
		}
	}
	return nil, nil
}

func (c *RestClient) GetMarketsByTimeframe(ctx context.Context, assets []string, timeframe string) ([]Market, error) {
	if c.Exchange == "kalshi" {
		return c.kalshiGetMarketsByTimeframe(ctx, assets, timeframe)
	}
	if len(assets) == 0 {
		assets = []string{"btc", "eth"}
	}
	timeframe = strings.ToLower(strings.TrimSpace(timeframe))
	if timeframe == "" {
		timeframe = "15m"
	}

	var interval int64 = 900 // 15 minutes by default
	switch timeframe {
	case "5m":
		interval = 300 // 5 minutes
	case "1h":
		interval = 3600 // 1 hour
	case "4h":
		interval = 14400 // 4 hours
	case "1d":
		interval = 86400 // 1 day
	}

	now := time.Now().UTC()
	currentTs := now.Unix()

	// Calculate the current window START
	currentWindowStart := (currentTs / interval) * interval

	// Check multiple windows to handle edge cases:
	// - Current window (most likely)
	// - Next window (might be pre-created near end of current window)
	// - Window after next (for early creation)
	// - Previous 4 windows (to support redemption of recently closed markets)
	windowsToCheck := []int64{
		currentWindowStart,              // Current window
		currentWindowStart + interval,   // Next window (might be pre-created)
		currentWindowStart + 2*interval, // Window after next (early creation)
		currentWindowStart - interval,   // Previous window
		currentWindowStart - 2*interval, // 2 windows ago
		currentWindowStart - 3*interval, // 3 windows ago
		currentWindowStart - 4*interval, // 4 windows ago
	}

	type timeframeTask struct {
		index int
		slugs []string
	}
	type timeframeResult struct {
		index  int
		market *Market
		err    error
	}

	tasks := make([]timeframeTask, 0, len(assets)*len(windowsToCheck))
	for _, asset := range assets {
		for _, windowStart := range windowsToCheck {
			tasks = append(tasks, timeframeTask{
				index: len(tasks),
				slugs: gammaTimeframeWindowSlugCandidates(asset, timeframe, time.Unix(windowStart, 0).UTC()),
			})
		}
	}

	results := make(chan timeframeResult, len(tasks))
	var wg sync.WaitGroup
	taskCh := make(chan timeframeTask)
	workerCount := 12
	if len(tasks) < workerCount {
		workerCount = len(tasks)
	}
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range taskCh {
				var market *Market
				var err error
				for _, slug := range task.slugs {
					market, err = c.getGammaTimeframeMarket(ctx, slug)
					if market != nil {
						break
					}
				}
				results <- timeframeResult{index: task.index, market: market, err: err}
			}
		}()
	}
	go func() {
		defer close(taskCh)
		for _, task := range tasks {
			taskCh <- task
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	ordered := make([]*Market, len(tasks))
	var lastErr error
	for result := range results {
		if result.err != nil {
			lastErr = result.err
			continue
		}
		ordered[result.index] = result.market
	}

	markets := make([]Market, 0, len(tasks))
	for _, market := range ordered {
		if market != nil {
			markets = append(markets, *market)
		}
	}
	if len(markets) == 0 && lastErr != nil {
		return nil, lastErr
	}

	return markets, nil
}

func gammaTimeframeWindowSlugCandidates(asset, timeframe string, windowStart time.Time) []string {
	timeframe = strings.ToLower(strings.TrimSpace(timeframe))
	asset = strings.ToLower(strings.TrimSpace(asset))

	var slugAssets []string
	switch asset {
	case "btc", "bitcoin":
		slugAssets = []string{"bitcoin", "btc"}
	case "eth", "ethereum":
		slugAssets = []string{"ethereum", "eth"}
	case "sol", "solana":
		slugAssets = []string{"solana", "sol"}
	case "xrp", "ripple":
		slugAssets = []string{"ripple", "xrp"}
	default:
		slugAssets = []string{asset}
	}

	var candidates []string
	for _, sa := range slugAssets {
		legacy := fmt.Sprintf("%s-updown-%s-%d", sa, timeframe, windowStart.Unix())
		if timeframe == "1h" {
			candidates = append(candidates, core.PolymarketHourlyEventSlug(sa, windowStart))
		}
		candidates = append(candidates, legacy)
		if timeframe == "1d" {
			candidates = append(candidates, core.PolymarketDailyEventSlug(sa, windowStart))
			candidates = append(candidates, fmt.Sprintf("%s-updown-1D-%d", sa, windowStart.Unix()))
			candidates = append(candidates, fmt.Sprintf("%s-updown-24h-%d", sa, windowStart.Unix()))
		}
	}

	return candidates
}

func parseGammaEventEndTime(endDate string) time.Time {
	endDate = strings.TrimSpace(endDate)
	if endDate == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, endDate); err == nil {
		return t.UTC()
	}
	if t, err := time.Parse(time.RFC3339, endDate); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

func (c *RestClient) ListMarkets(ctx context.Context) ([]Market, error) {
	url := fmt.Sprintf("%s/markets?active=true&closed=false", c.BaseURL)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to list markets: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to list markets: status %d", resp.StatusCode)
	}

	var result ListMarketsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBodySize)).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode markets list: %w", err)
	}

	for i := range result.Data {
		result.Data[i].Slug = core.SanitizeString(result.Data[i].Slug)
		result.Data[i].MarketSlug = core.SanitizeString(result.Data[i].MarketSlug)
		for j := range result.Data[i].Tokens {
			result.Data[i].Tokens[j].Outcome = core.SanitizeString(result.Data[i].Tokens[j].Outcome)
		}
	}

	return result.Data, nil
}

func (c *RestClient) GetMarket(ctx context.Context, slug string) (*Market, error) {
	url := fmt.Sprintf("%s/markets/%s", c.BaseURL, slug)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch market: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch market: status %d", resp.StatusCode)
	}

	var market Market
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBodySize)).Decode(&market); err != nil {
		return nil, fmt.Errorf("failed to decode market response: %w", err)
	}

	market.Slug = core.SanitizeString(market.Slug)
	market.MarketSlug = core.SanitizeString(market.MarketSlug)
	for i := range market.Tokens {
		market.Tokens[i].Outcome = core.SanitizeString(market.Tokens[i].Outcome)
	}

	return &market, nil
}

func (c *RestClient) GetClobMarketInfo(ctx context.Context, conditionID string) (*ClobMarketInfo, error) {
	select {
	case <-c.limiter:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/clob-markets/%s", c.BaseURL, conditionID), nil)
	if err != nil {
		return nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch clob market info: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
		return nil, fmt.Errorf("failed to fetch clob market info: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var info ClobMarketInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBodySize)).Decode(&info); err != nil {
		return nil, fmt.Errorf("failed to decode clob market info: %w", err)
	}
	return &info, nil
}

// OrderBookResponse represents the CLOB order book
type OrderBookResponse struct {
	Market    string       `json:"market"`
	AssetID   string       `json:"asset_id"`
	Timestamp string       `json:"timestamp"`
	Bids      []PriceLevel `json:"bids"`
	Asks      []PriceLevel `json:"asks"`
}

func ParseOrderBookTimestamp(raw string) (time.Time, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}, fmt.Errorf("missing order book timestamp")
	}
	if unix, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		switch {
		case unix >= 1_000_000_000_000:
			return time.UnixMilli(unix), nil
		case unix >= 1_000_000_000:
			return time.Unix(unix, 0), nil
		default:
			return time.Time{}, fmt.Errorf("unsupported numeric order book timestamp %q", raw)
		}
	}
	if ts, err := time.Parse(time.RFC3339Nano, trimmed); err == nil {
		return ts, nil
	}
	if ts, err := time.Parse(time.RFC3339, trimmed); err == nil {
		return ts, nil
	}
	return time.Time{}, fmt.Errorf("unsupported order book timestamp %q", raw)
}

func OrderBookAgeAt(book *OrderBookResponse, now time.Time) (time.Duration, error) {
	if book == nil {
		return 0, fmt.Errorf("nil order book")
	}
	ts, err := ParseOrderBookTimestamp(book.Timestamp)
	if err != nil {
		return 0, err
	}
	if now.Before(ts) {
		return 0, nil
	}
	return now.Sub(ts), nil
}

func (c *RestClient) kalshiGetOrderBook(ctx context.Context, ticker string) (*OrderBookResponse, error) {
	isNoSide := strings.HasSuffix(ticker, "-NO")
	baseTicker := ticker
	if strings.HasSuffix(ticker, "-YES") {
		baseTicker = strings.TrimSuffix(ticker, "-YES")
	} else if isNoSide {
		baseTicker = strings.TrimSuffix(ticker, "-NO")
	}

	url := fmt.Sprintf("%s/markets/%s/orderbook?depth=100", c.KalshiBaseURL, baseTicker)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("kalshi orderbook error: %d %s", resp.StatusCode, string(b))
	}

	var kalshiResp struct {
		Orderbook struct {
			Yes []struct {
				Price    int `json:"price"`
				Quantity int `json:"quantity"`
			} `json:"yes"`
			No []struct {
				Price    int `json:"price"`
				Quantity int `json:"quantity"`
			} `json:"no"`
		} `json:"orderbook"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&kalshiResp); err != nil {
		return nil, err
	}

	// Convert Kalshi Bids to standard Bids/Asks
	// Yes Bids = Bids for Yes
	// No Bids = Asks for Yes (Ask Price = 100 - No Bid Price)

	book := &OrderBookResponse{
		Bids:      make([]PriceLevel, 0),
		Asks:      make([]PriceLevel, 0),
		Timestamp: fmt.Sprintf("%d", time.Now().UnixMilli()),
	}

	if isNoSide {
		for _, nb := range kalshiResp.Orderbook.No {
			price := float64(nb.Price) / 100.0
			size := float64(nb.Quantity)
			book.Bids = append(book.Bids, PriceLevel{Price: fmt.Sprintf("%.3f", price), Size: fmt.Sprintf("%.2f", size)})
		}
		for _, yb := range kalshiResp.Orderbook.Yes {
			askPrice := float64(100-yb.Price) / 100.0
			size := float64(yb.Quantity)
			book.Asks = append(book.Asks, PriceLevel{Price: fmt.Sprintf("%.3f", askPrice), Size: fmt.Sprintf("%.2f", size)})
		}
	} else {
		for _, yb := range kalshiResp.Orderbook.Yes {
			price := float64(yb.Price) / 100.0
			size := float64(yb.Quantity)
			book.Bids = append(book.Bids, PriceLevel{Price: fmt.Sprintf("%.3f", price), Size: fmt.Sprintf("%.2f", size)})
		}
		for _, nb := range kalshiResp.Orderbook.No {
			askPrice := float64(100-nb.Price) / 100.0
			size := float64(nb.Quantity)
			book.Asks = append(book.Asks, PriceLevel{Price: fmt.Sprintf("%.3f", askPrice), Size: fmt.Sprintf("%.2f", size)})
		}
	}

	return book, nil
}

// GetOrderBook fetches the current order book for a token from REST API
func (c *RestClient) GetOrderBook(ctx context.Context, tokenID string) (*OrderBookResponse, error) {
	if c.Exchange == "kalshi" {
		return c.kalshiGetOrderBook(ctx, tokenID)
	}

	// 1. Try to serve from live WS-backed provider or cache if WebSocket is active/healthy
	c.bookMu.RLock()
	wsIsActive := c.wsActive != nil && c.wsActive()
	provider := c.wsProvider
	cached, found := c.bookCache[tokenID]
	c.bookMu.RUnlock()

	if wsIsActive {
		if provider != nil {
			if book := provider(tokenID); book != nil {
				return book, nil
			}
		}
		if found && cached != nil {
			return cached, nil
		}
	} else if found && cached != nil {
		// If WS is not active, fallback to returning cache if it is extremely fresh (< 5s old)
		if parsedTime, err := ParseOrderBookTimestamp(cached.Timestamp); err == nil {
			if time.Since(parsedTime) < 5*time.Second {
				return cached, nil
			}
		}
	}

	// Rate limit check
	select {
	case <-c.limiter:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	url := fmt.Sprintf("%s/book?token_id=%s", c.BaseURL, tokenID)
	resp, err := doGETWithRetry(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch order book: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusBadRequest {
			// If market is closed or token not found, return empty book rather than error
			return &OrderBookResponse{
				Bids:      make([]PriceLevel, 0),
				Asks:      make([]PriceLevel, 0),
				Timestamp: fmt.Sprintf("%d", time.Now().UnixMilli()),
			}, nil
		}
		return nil, fmt.Errorf("failed to fetch order book: status %d", resp.StatusCode)
	}

	var book OrderBookResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBodySize)).Decode(&book); err != nil {
		return nil, fmt.Errorf("failed to decode order book: %w", err)
	}

	return &book, nil
}

// GetBestBidAsk fetches the best bid and ask price for a single token via REST.
// This is a convenience wrapper around GetOrderBook for quick price confirmation.
func (c *RestClient) GetBestBidAsk(ctx context.Context, tokenID string) (bestBid, bestAsk float64, err error) {
	book, err := c.GetOrderBook(ctx, tokenID)
	if err != nil {
		return 0, 0, err
	}
	for _, b := range book.Bids {
		p, _ := parseFloat(b.Price)
		if p > bestBid {
			bestBid = p
		}
	}
	for _, a := range book.Asks {
		p, _ := parseFloat(a.Price)
		if p > 0 && (bestAsk == 0 || p < bestAsk) {
			bestAsk = p
		}
	}
	return bestBid, bestAsk, nil
}

// FeeRateResponse represents the response from the fee-rate endpoint
type FeeRateResponse struct {
	BaseFee       *int `json:"base_fee"`
	FeeRateBps    *int `json:"fee_rate_bps"`
	FeeRateBpsAlt *int `json:"feeRateBps"`
}

func (r FeeRateResponse) rateBps() (int, bool) {
	switch {
	case r.BaseFee != nil:
		return *r.BaseFee, true
	case r.FeeRateBps != nil:
		return *r.FeeRateBps, true
	case r.FeeRateBpsAlt != nil:
		return *r.FeeRateBpsAlt, true
	default:
		return 0, false
	}
}

// GetFeeRate fetches the current fee rate for a token
func (c *RestClient) GetFeeRate(ctx context.Context, tokenID string) (int, error) {
	// Rate limit check
	select {
	case <-c.limiter:
	case <-ctx.Done():
		return 0, ctx.Err()
	}

	url := fmt.Sprintf("%s/fee-rate?token_id=%s", c.BaseURL, tokenID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to fetch fee rate: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("failed to fetch fee rate: status %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
	if err != nil {
		return 0, fmt.Errorf("failed to read fee rate response: %w", err)
	}

	// Try plain number first (API may return e.g. "1000" or 1000)
	trimmed := strings.TrimSpace(string(bodyBytes))
	trimmed = strings.Trim(trimmed, "\"")
	if v, err := strconv.Atoi(trimmed); err == nil {
		return v, nil
	}

	// Fall back to JSON object parsing
	var result FeeRateResponse
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return 0, fmt.Errorf("failed to decode fee rate (body=%q): %w", trimmed, err)
	}

	rate, ok := result.rateBps()
	if !ok {
		return 0, fmt.Errorf("failed to decode fee rate: response missing base_fee or fee_rate_bps (body=%q)", trimmed)
	}
	return rate, nil
}

// GammaPriceResult contains bid/ask prices for an outcome
type GammaPriceResult struct {
	Bid float64
	Ask float64
}

// GetGammaPriceBySlug fetches the current price from Gamma API using slug
func (c *RestClient) GetGammaPriceBySlug(ctx context.Context, slug string) (map[string]float64, error) {
	result, err := c.GetGammaBidAskBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	// Return mid prices for backward compatibility
	prices := make(map[string]float64)
	for outcome, pa := range result {
		prices[outcome] = (pa.Bid + pa.Ask) / 2
	}
	return prices, nil
}

// GetGammaBidAskBySlug fetches bid/ask from Gamma API using slug
func (c *RestClient) GetGammaBidAskBySlug(ctx context.Context, slug string) (map[string]GammaPriceResult, error) {
	url := fmt.Sprintf("%s/markets?slug=%s", c.GammaURL, slug)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch gamma price: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch gamma price: status %d", resp.StatusCode)
	}

	var results []struct {
		BestBid float64 `json:"bestBid"`
		BestAsk float64 `json:"bestAsk"`
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBodySize)).Decode(&results); err != nil {
		return nil, fmt.Errorf("failed to decode gamma price: %w", err)
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no market found for slug: %s", slug)
	}

	// For binary markets, the results contain bestBid/bestAsk for "Up" outcome
	// "Down" is 1 - price
	prices := make(map[string]GammaPriceResult)
	prices["Up"] = GammaPriceResult{
		Bid: results[0].BestBid,
		Ask: results[0].BestAsk,
	}
	prices["Down"] = GammaPriceResult{
		Bid: 1 - results[0].BestAsk, // Down bid = 1 - Up ask
		Ask: 1 - results[0].BestBid, // Down ask = 1 - Up bid
	}

	return prices, nil
}

// GetCLOBBidAsk fetches real-time bid/ask from CLOB order books for given token IDs
// tokenMap maps token ID to outcome name (e.g., "Up" or "Down")
func (c *RestClient) GetCLOBBidAsk(ctx context.Context, tokenMap map[string]string) (map[string]GammaPriceResult, error) {
	prices := make(map[string]GammaPriceResult)
	type clobBidAskResult struct {
		outcome string
		price   GammaPriceResult
		err     error
	}

	results := make(chan clobBidAskResult, len(tokenMap))
	var wg sync.WaitGroup
	for tokenID, outcome := range tokenMap {
		tokenID, outcome := tokenID, outcome
		wg.Add(1)
		go func() {
			defer wg.Done()
			book, err := c.GetOrderBook(ctx, tokenID)
			if err != nil {
				results <- clobBidAskResult{err: err}
				return
			}

			var bestBid, bestAsk float64
			for _, b := range book.Bids {
				p, _ := parseFloat(b.Price)
				if p > bestBid {
					bestBid = p
				}
			}
			for _, a := range book.Asks {
				p, _ := parseFloat(a.Price)
				if p > 0 && (bestAsk == 0 || p < bestAsk) {
					bestAsk = p
				}
			}

			results <- clobBidAskResult{
				outcome: outcome,
				price: GammaPriceResult{
					Bid: bestBid,
					Ask: bestAsk,
				},
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	for result := range results {
		if result.err != nil {
			continue
		}
		prices[result.outcome] = result.price
	}

	// For binary markets with Up/Down, infer missing prices from complement
	// Up ask ≈ 1 - Down bid, Up bid ≈ 1 - Down ask
	upPrices, hasUp := prices["Up"]
	downPrices, hasDown := prices["Down"]

	if hasUp && hasDown {
		// Infer Up prices from Down if missing
		if upPrices.Bid == 0 && downPrices.Ask > 0 {
			upPrices.Bid = 1.0 - downPrices.Ask
		}
		if upPrices.Ask == 0 && downPrices.Bid > 0 {
			upPrices.Ask = 1.0 - downPrices.Bid
		}

		// Infer Down prices from Up if missing
		if downPrices.Bid == 0 && upPrices.Ask > 0 {
			downPrices.Bid = 1.0 - upPrices.Ask
		}
		if downPrices.Ask == 0 && upPrices.Bid > 0 {
			downPrices.Ask = 1.0 - upPrices.Bid
		}

		prices["Up"] = upPrices
		prices["Down"] = downPrices
	}

	return prices, nil
}

func parseFloat(s string) (float64, error) {
	return strconv.ParseFloat(s, 64)
}

// CloseIdleConnections closes any idle HTTP connections held in the pool.
// Call this between market rounds to flush stale connections and free memory,
// which reduces the risk of the transport reusing a connection that is in a
// bad state after heavy polling.
func (c *RestClient) CloseIdleConnections() {
	httpClient.CloseIdleConnections()
}

// Ping does a lightweight GET /time to measure raw network RTT through the
// shared httpClient (same transport, connection pool, and HTTP/2 as the bot).
func (c *RestClient) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/time", nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

// SetWSActiveCallback sets a callback to check if the WebSocket feed is active and healthy
func (c *RestClient) SetWSActiveCallback(cb func() bool) {
	if c == nil {
		return
	}
	c.bookMu.Lock()
	defer c.bookMu.Unlock()
	c.wsActive = cb
}

// UpdateOrderBookCache writes a fresh real-time order book snapshot into the in-memory cache
func (c *RestClient) UpdateOrderBookCache(tokenID string, book *OrderBookResponse) {
	if c == nil || tokenID == "" || book == nil {
		return
	}
	c.bookMu.Lock()
	defer c.bookMu.Unlock()
	if c.bookCache == nil {
		c.bookCache = make(map[string]*OrderBookResponse)
	}
	c.bookCache[tokenID] = book
}

// SetWSOrderBookProvider sets a callback to query the live in-memory WebSocket feed on demand
func (c *RestClient) SetWSOrderBookProvider(cb func(tokenID string) *OrderBookResponse) {
	if c == nil {
		return
	}
	c.bookMu.Lock()
	defer c.bookMu.Unlock()
	c.wsProvider = cb
}
