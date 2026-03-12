package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"Market-bot/internal/core"
)

// maxResponseBodySize caps how many bytes we'll read from any API response.
// This converts what would be an unrecoverable bytes.ErrTooLarge panic (in the
// HTTP/2 read-loop goroutine) into an ordinary JSON decode error.
const maxResponseBodySize = 2 * 1024 * 1024 // 2 MB

// httpClient is the shared HTTP client for all REST calls.
//
// HTTP/2 is enabled for connection multiplexing — all concurrent requests to
// the same host share a single TCP+TLS connection, eliminating per-request
// handshake overhead.  Response bodies are still capped via io.LimitReader
// (maxResponseBodySize) at every call site to prevent unbounded reads.
var httpClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:          500,
		MaxIdleConnsPerHost:   500,
		MaxConnsPerHost:       0,
		IdleConnTimeout:       30 * time.Minute, // Extremely long idle timeout to prevent cold starts
		DisableCompression:    true,
		ForceAttemptHTTP2:     true,
		ExpectContinueTimeout: 1 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 15 * time.Second, // Aggressive TCP keep-alive pings to bypass NAT/LB timeouts
			Control: func(network, address string, c syscall.RawConn) error {
				var opErr error
				err := c.Control(func(fd uintptr) {
					opErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
				})
				if err != nil {
					return err
				}
				return opErr
			},
		}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
	},
}

type Token struct {
	TokenID string `json:"token_id"`
	Outcome string `json:"outcome"`
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

type RestClient struct {
	Exchange      string
	KalshiBaseURL string
	kalshiSigner  *KalshiSigner

	BaseURL  string
	GammaURL string
	// Rate limiting: strictly enforce max requests per second
	limiter <-chan time.Time
}

func NewRestClient(exchange, kalshiKey, kalshiPK string) *RestClient {
	limiter := time.NewTicker(time.Second / 500)
	
	client := &RestClient{
		Exchange:      exchange,
		BaseURL:       "https://clob.polymarket.com",
		GammaURL:      "https://gamma-api.polymarket.com",
		KalshiBaseURL: KalshiBaseURL,
		limiter:       limiter.C,
	}

	if exchange == "kalshi" && kalshiKey != "" && kalshiPK != "" {
		signer, err := NewKalshiSigner(kalshiKey, kalshiPK)
		if err == nil {
			client.kalshiSigner = signer
		}
	}

	return client
}

type GammaEvent struct {
	Slug    string        `json:"slug"`
	EndDate string        `json:"endDate"`
	Markets []GammaMarket `json:"markets"`
}

type GammaMarket struct {
	ConditionID  string `json:"conditionId"`
	ClobTokenIds string `json:"clobTokenIds"` // JSON-encoded string array
	Outcomes     string `json:"outcomes"`
	Active       bool   `json:"active"`
	Closed       bool   `json:"closed"`
}

func (c *RestClient) GetEventByTokenID(ctx context.Context, tokenID string) (*GammaEvent, error) {
	url := fmt.Sprintf("%s/events?clobTokenIds=%s", c.GammaURL, tokenID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get event by token id, status code: %d", resp.StatusCode)
	}

	var events []GammaEvent
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
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
		var tokenIDs []string
		if err := json.Unmarshal([]byte(gm.ClobTokenIds), &tokenIDs); err != nil || len(tokenIDs) < 2 {
			continue
		}

		var outcomes []string
		if err := json.Unmarshal([]byte(gm.Outcomes), &outcomes); err != nil || len(outcomes) < 2 {
			outcomes = []string{"Up", "Down"}
		}

		markets = append(markets, Market{
			ConditionID: gm.ConditionID,
			Slug:        eventSlug,
			Active:      gm.Active,
			Closed:      gm.Closed,
			EndTime:     eventEndTime,
			Tokens: []Token{
				{TokenID: tokenIDs[0], Outcome: core.SanitizeString(outcomes[0])},
				{TokenID: tokenIDs[1], Outcome: core.SanitizeString(outcomes[1])},
			},
		})
	}

	if len(markets) == 0 {
		return nil, fmt.Errorf("no markets found for slug: %s", fallbackSlug)
	}

	return markets, nil
}

func (c *RestClient) kalshiGetMarketsByTimeframe(ctx context.Context, assets []string, timeframe string) ([]Market, error) {
	if len(assets) == 0 {
		assets = []string{"btc", "eth"}
	}
	var markets []Market

	for _, asset := range assets {
		// Example kalshi series ticker: KXBTC
		seriesTicker := "KX" + strings.ToUpper(asset)
		url := fmt.Sprintf("%s/events?series_ticker=%s", c.KalshiBaseURL, seriesTicker)
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
				Ticker   string `json:"ticker"`
				MutuallyExclusive bool `json:"mutually_exclusive"`
			} `json:"events"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&kalshiResp); err != nil {
			resp.Body.Close()
			continue
		}
		resp.Body.Close()

		for _, event := range kalshiResp.Events {
			// Basic filtering, usually we match timeframe in ticker (e.g. KXBTC-15M)
			// For simplicity we just return the events as markets.
			
			markets = append(markets, Market{
				ConditionID: event.Ticker, // Kalshi ticker
				Slug:        event.Ticker,
				Active:      true,
				Closed:      false,
				EndTime:     time.Now().Add(1 * time.Hour), // stub
				Tokens: []Token{
					{TokenID: event.Ticker, Outcome: "Yes"},
					{TokenID: event.Ticker, Outcome: "No"},
				},
			})
		}
	}

	return markets, nil
}

func (c *RestClient) GetMarketsByTimeframe(ctx context.Context, assets []string, timeframe string) ([]Market, error) {
	if c.Exchange == "kalshi" {
		return c.kalshiGetMarketsByTimeframe(ctx, assets, timeframe)
	}
	if len(assets) == 0 {
		assets = []string{"btc", "eth"}
	}
	if timeframe == "" {
		timeframe = "15m"
	}

	var interval int64 = 900 // 15 minutes by default
	if timeframe == "5m" {
		interval = 300 // 5 minutes
	} else if timeframe == "1d" {
		interval = 86400 // 1 day
	}

	now := time.Now().UTC()
	currentTs := now.Unix()

	// Calculate the current window START
	currentWindowStart := (currentTs / interval) * interval

	var markets []Market

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

	for _, asset := range assets {
		for _, windowStart := range windowsToCheck {
			// Rate limit check
			select {
			case <-c.limiter:
			case <-ctx.Done():
				return nil, ctx.Err()
			}

			slug := fmt.Sprintf("%s-updown-%s-%d", asset, timeframe, windowStart)

			url := fmt.Sprintf("%s/events?slug=%s", c.GammaURL, slug)
			req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
			if err != nil {
				continue
			}

			resp, err := httpClient.Do(req)
			if err != nil || resp.StatusCode != http.StatusOK {
				if resp != nil {
					resp.Body.Close()
				}
				continue
			}

			var events []GammaEvent
			if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBodySize)).Decode(&events); err != nil || len(events) == 0 {
				resp.Body.Close()
				continue
			}
			resp.Body.Close()

			if len(events) == 0 || len(events[0].Markets) == 0 {
				continue
			}

			event := events[0]
			gm := event.Markets[0]
			eventEndTime := parseGammaEventEndTime(event.EndDate)

			// Parse clobTokenIds (it's a JSON-encoded string array)
			var tokenIds []string
			if err := json.Unmarshal([]byte(gm.ClobTokenIds), &tokenIds); err != nil || len(tokenIds) < 2 {
				continue
			}

			// Parse outcomes (also JSON-encoded string array)
			var outcomes []string
			if err := json.Unmarshal([]byte(gm.Outcomes), &outcomes); err != nil || len(outcomes) < 2 {
				// Fallback to default
				outcomes = []string{"Up", "Down"}
			}

			// Build Market from Gamma data
			market := &Market{
				ConditionID: gm.ConditionID,
				Slug:        core.SanitizeString(slug),
				Active:      gm.Active,
				Closed:      gm.Closed,
				EndTime:     eventEndTime,
				Tokens: []Token{
					{TokenID: tokenIds[0], Outcome: core.SanitizeString(outcomes[0])},
					{TokenID: tokenIds[1], Outcome: core.SanitizeString(outcomes[1])},
				},
			}

			markets = append(markets, *market)
		}
	}

	return markets, nil
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
	url := fmt.Sprintf("%s/markets/%s/orderbook?depth=100", c.KalshiBaseURL, ticker)
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
		Bids: make([]PriceLevel, 0),
		Asks: make([]PriceLevel, 0),
		Timestamp: fmt.Sprintf("%d", time.Now().UnixMilli()),
	}

	for _, yb := range kalshiResp.Orderbook.Yes {
		price := float64(yb.Price) / 100.0
		size := float64(yb.Quantity)
		book.Bids = append(book.Bids, PriceLevel{Price: fmt.Sprintf("%.3f", price), Size: fmt.Sprintf("%.2f", size)})
	}
	for _, nb := range kalshiResp.Orderbook.No {
		askPrice := float64(100 - nb.Price) / 100.0
		size := float64(nb.Quantity)
		book.Asks = append(book.Asks, PriceLevel{Price: fmt.Sprintf("%.3f", askPrice), Size: fmt.Sprintf("%.2f", size)})
	}

	return book, nil
}

// GetOrderBook fetches the current order book for a token from REST API
func (c *RestClient) GetOrderBook(ctx context.Context, tokenID string) (*OrderBookResponse, error) {
	if c.Exchange == "kalshi" {
		return c.kalshiGetOrderBook(ctx, tokenID)
	}
	// Rate limit check
	select {
	case <-c.limiter:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	url := fmt.Sprintf("%s/book?token_id=%s", c.BaseURL, tokenID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch order book: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch order book: status %d", resp.StatusCode)
	}

	var book OrderBookResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBodySize)).Decode(&book); err != nil {
		return nil, fmt.Errorf("failed to decode order book: %w", err)
	}

	return &book, nil
}

// FeeRateResponse represents the response from the fee-rate endpoint
type FeeRateResponse struct {
	FeeRateBps int `json:"fee_rate_bps"`
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

	return result.FeeRateBps, nil
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
