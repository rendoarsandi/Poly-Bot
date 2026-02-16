package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"Market-bot/internal/core"
)

// Optimized HTTP client with connection pooling and timeouts for ultra-low latency
var httpClient = &http.Client{
	Timeout: 10 * time.Second, // Increased timeout for better stability on slower networks
	Transport: &http.Transport{
		MaxIdleConns:        200, // More idle connections
		MaxIdleConnsPerHost: 50,  // More per-host connections
		MaxConnsPerHost:     100, // Allow more concurrent connections
		IdleConnTimeout:     120 * time.Second,
		DisableCompression:  true, // Skip compression for speed
		DialContext: (&net.Dialer{
			Timeout:   2 * time.Second,
			KeepAlive: 60 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   2 * time.Second,
		ResponseHeaderTimeout: 3 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true, // Use HTTP/2 for multiplexing
	},
}

type Token struct {
	TokenID string `json:"token_id"`
	Outcome string `json:"outcome"`
}

type Market struct {
	Active      bool    `json:"active"`
	Closed      bool    `json:"closed"`
	ConditionID string  `json:"condition_id"`
	Slug        string  `json:"slug"`
	MarketSlug  string  `json:"market_slug"` // Used in list response
	Tokens      []Token `json:"tokens"`
}

type ListMarketsResponse struct {
	Data []Market `json:"data"`
}

type RestClient struct {
	BaseURL  string
	GammaURL string
	// Rate limiting: strictly enforce max requests per second
	limiter <-chan time.Time
}

func NewRestClient(baseURL string) *RestClient {
	if baseURL == "" {
		baseURL = "https://clob.polymarket.com"
	}
	// Rate limit to 500 RPS (matches Polymarket burst limit)
	limiter := time.NewTicker(time.Second / 500)
	return &RestClient{
		BaseURL:  baseURL,
		GammaURL: "https://gamma-api.polymarket.com",
		limiter:  limiter.C,
	}
}

type GammaEvent struct {
	Slug    string        `json:"slug"`
	EndDate string        `json:"endDate"`
	Markets []GammaMarket `json:"markets"`
}

type GammaMarket struct {
	ConditionID  string `json:"conditionId"`
	Slug         string `json:"slug"`
	ClobTokenIds string `json:"clobTokenIds"` // JSON-encoded string array
	Outcomes     string `json:"outcomes"`
	Active       bool   `json:"active"`
	Closed       bool   `json:"closed"`
}

func (c *RestClient) Get15mMarkets(ctx context.Context, assets []string) ([]Market, error) {
	var markets []Market

	// 1. Tag-based discovery (Discovery of all active/closed 15m markets)
	url := fmt.Sprintf("%s/events?limit=100&tag_id=102467", c.GammaURL)
	
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := httpClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		var events []GammaEvent
		if err := json.NewDecoder(resp.Body).Decode(&events); err == nil {
			for _, event := range events {
				for _, gm := range event.Markets {
					var tokenIds []string
					if err := json.Unmarshal([]byte(gm.ClobTokenIds), &tokenIds); err != nil || len(tokenIds) < 2 {
						continue
					}
					var outcomes []string
					if err := json.Unmarshal([]byte(gm.Outcomes), &outcomes); err != nil || len(outcomes) < 2 {
						outcomes = []string{"Up", "Down"}
					}
					markets = append(markets, Market{
						ConditionID: gm.ConditionID,
						Slug:        gm.Slug,
						Active:      gm.Active,
						Closed:      gm.Closed,
						Tokens: []Token{
							{TokenID: tokenIds[0], Outcome: outcomes[0]},
							{TokenID: tokenIds[1], Outcome: outcomes[1]},
						},
					})
				}
			}
		}
	}

	// 2. Window-based discovery (Fallback/Specific for provided assets)
	if len(assets) > 0 {
		now := time.Now().Unix()
		currentWindowStart := now - (now % 900)
		windowsToCheck := []int64{
			currentWindowStart,
			currentWindowStart + 900,
			currentWindowStart - 900,
			currentWindowStart - 1800,
		}

		for _, asset := range assets {
			for _, windowStart := range windowsToCheck {
				select {
				case <-c.limiter:
				case <-ctx.Done():
					return markets, ctx.Err()
				}

				slug := fmt.Sprintf("%s-updown-15m-%d", strings.ToLower(asset), windowStart)
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
				if err := json.NewDecoder(resp.Body).Decode(&events); err == nil && len(events) > 0 && len(events[0].Markets) > 0 {
					gm := events[0].Markets[0]
					var tokenIds []string
					if err := json.Unmarshal([]byte(gm.ClobTokenIds), &tokenIds); err == nil && len(tokenIds) >= 2 {
						var outcomes []string
						if err := json.Unmarshal([]byte(gm.Outcomes), &outcomes); err != nil || len(outcomes) < 2 {
							outcomes = []string{"Up", "Down"}
						}
						
						// Check if already found via tag
						exists := false
						for _, existing := range markets {
							if existing.ConditionID == gm.ConditionID {
								exists = true
								break
							}
						}
						
						if !exists {
							markets = append(markets, Market{
								ConditionID: gm.ConditionID,
								Slug:        gm.Slug,
								Active:      gm.Active,
								Closed:      gm.Closed,
								Tokens: []Token{
									{TokenID: tokenIds[0], Outcome: outcomes[0]},
									{TokenID: tokenIds[1], Outcome: outcomes[1]},
								},
							})
						}
					}
				}
				if resp != nil {
					resp.Body.Close()
				}
			}
		}
	}

	return markets, nil
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
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
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
	// Try CLOB API first
	url := fmt.Sprintf("%s/markets/%s", c.BaseURL, slug)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err == nil {
		resp, err := httpClient.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			defer resp.Body.Close()
			var market Market
			if err := json.NewDecoder(resp.Body).Decode(&market); err == nil {
				market.Slug = core.SanitizeString(market.Slug)
				market.MarketSlug = core.SanitizeString(market.MarketSlug)
				for i := range market.Tokens {
					market.Tokens[i].Outcome = core.SanitizeString(market.Tokens[i].Outcome)
				}
				return &market, nil
			}
		} else if resp != nil {
			resp.Body.Close()
		}
	}

	// Fallback to Gamma API for closed markets
	url = fmt.Sprintf("%s/events?slug=%s", c.GammaURL, slug)
	req, err = http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var events []GammaEvent
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil || len(events) == 0 {
		return nil, fmt.Errorf("market not found in CLOB or Gamma: %s", slug)
	}

	gm := events[0].Markets[0]
	var tokenIds []string
	json.Unmarshal([]byte(gm.ClobTokenIds), &tokenIds)
	var outcomes []string
	json.Unmarshal([]byte(gm.Outcomes), &outcomes)

	return &Market{
		ConditionID: gm.ConditionID,
		Slug:        slug,
		Active:      gm.Active,
		Closed:      gm.Closed,
		Tokens: []Token{
			{TokenID: tokenIds[0], Outcome: outcomes[0]},
			{TokenID: tokenIds[1], Outcome: outcomes[1]},
		},
	}, nil
}

// OrderBookResponse represents the CLOB order book
type OrderBookResponse struct {
	Market    string       `json:"market"`
	AssetID   string       `json:"asset_id"`
	Timestamp string       `json:"timestamp"`
	Bids      []PriceLevel `json:"bids"`
	Asks      []PriceLevel `json:"asks"`
}

// GetOrderBook fetches the current order book for a token from REST API
func (c *RestClient) GetOrderBook(ctx context.Context, tokenID string) (*OrderBookResponse, error) {
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
	if err := json.NewDecoder(resp.Body).Decode(&book); err != nil {
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

	bodyBytes, err := io.ReadAll(resp.Body)
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

	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
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

	for tokenID, outcome := range tokenMap {
		book, err := c.GetOrderBook(ctx, tokenID)
		if err != nil {
			continue
		}

		var bestBid, bestAsk float64 = 0, 0

		// Find best bid (highest)
		for _, b := range book.Bids {
			p, _ := parseFloat(b.Price)
			if p > bestBid {
				bestBid = p
			}
		}

		// Find best ask (lowest)
		for _, a := range book.Asks {
			p, _ := parseFloat(a.Price)
			if p > 0 && (bestAsk == 0 || p < bestAsk) {
				bestAsk = p
			}
		}

		prices[outcome] = GammaPriceResult{
			Bid: bestBid,
			Ask: bestAsk,
		}
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
