package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

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
}

func NewRestClient(baseURL string) *RestClient {
	if baseURL == "" {
		baseURL = "https://clob.polymarket.com"
	}
	return &RestClient{
		BaseURL:  baseURL,
		GammaURL: "https://gamma-api.polymarket.com",
	}
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

func (c *RestClient) Get15mMarkets(assets []string) ([]Market, error) {
	if len(assets) == 0 {
		assets = []string{"btc", "eth", "sol", "xrp"}
	}

	now := time.Now().UTC()
	currentTs := now.Unix()

	// Calculate the current 15m window START
	currentWindowStart := (currentTs / 900) * 900

	// Time remaining in current window
	currentWindowEnd := currentWindowStart + 900
	timeRemaining := currentWindowEnd - currentTs

	var markets []Market

	for _, asset := range assets {
		// Strategy: If >2 minutes left in current window, use current
		// Otherwise look at next window (more time to trade)
		var windowsToCheck []int64

		if timeRemaining > 120 { // More than 2 minutes left
			windowsToCheck = []int64{currentWindowStart}
		} else {
			// Current window ending soon, prefer next window but check current too
			windowsToCheck = []int64{currentWindowStart + 900, currentWindowStart}
		}

		for _, windowStart := range windowsToCheck {
			slug := fmt.Sprintf("%s-updown-15m-%d", asset, windowStart)

			url := fmt.Sprintf("%s/events?slug=%s", c.GammaURL, slug)
			resp, err := http.Get(url)
			if err != nil || resp.StatusCode != http.StatusOK {
				continue
			}

			var events []GammaEvent
			if err := json.NewDecoder(resp.Body).Decode(&events); err != nil || len(events) == 0 {
				resp.Body.Close()
				continue
			}
			resp.Body.Close()

			event := events[0]
			if len(event.Markets) == 0 {
				continue
			}

			gm := event.Markets[0]

			// Skip if market is closed
			if gm.Closed || !gm.Active {
				continue
			}

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

			// Build Market from Gamma data with correct outcome mapping
			market := &Market{
				ConditionID: gm.ConditionID,
				Slug:        slug,
				Active:      gm.Active,
				Closed:      gm.Closed,
				Tokens: []Token{
					{TokenID: tokenIds[0], Outcome: outcomes[0]},
					{TokenID: tokenIds[1], Outcome: outcomes[1]},
				},
			}

			// Check if market has any liquidity
			hasLiquidity := false
			for _, token := range market.Tokens {
				book, err := c.GetOrderBook(token.TokenID)
				if err == nil && (len(book.Bids) > 0 || len(book.Asks) > 0) {
					hasLiquidity = true
					break
				}
			}

			if !hasLiquidity {
				continue // Silent skip
			}

			markets = append(markets, *market)
			break // Found market for this asset
		}
	}

	return markets, nil
}

func (c *RestClient) ListMarkets() ([]Market, error) {
	resp, err := http.Get(fmt.Sprintf("%s/markets?active=true&closed=false", c.BaseURL))
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

	return result.Data, nil
}

func (c *RestClient) GetMarket(slug string) (*Market, error) {
	resp, err := http.Get(fmt.Sprintf("%s/markets/%s", c.BaseURL, slug))
	if err != nil {
		return nil, fmt.Errorf("failed to fetch market: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch market: status %d", resp.StatusCode)
	}

	var market Market
	if err := json.NewDecoder(resp.Body).Decode(&market); err != nil {
		return nil, fmt.Errorf("failed to decode market response: %w", err)
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

// GetOrderBook fetches the current order book for a token from REST API
func (c *RestClient) GetOrderBook(tokenID string) (*OrderBookResponse, error) {
	url := fmt.Sprintf("%s/book?token_id=%s", c.BaseURL, tokenID)
	resp, err := http.Get(url)
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

// GammaPriceResult contains bid/ask prices for an outcome
type GammaPriceResult struct {
	Bid float64
	Ask float64
}

// GetGammaPriceBySlug fetches the current price from Gamma API using slug
func (c *RestClient) GetGammaPriceBySlug(slug string) (map[string]float64, error) {
	result, err := c.GetGammaBidAskBySlug(slug)
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
func (c *RestClient) GetGammaBidAskBySlug(slug string) (map[string]GammaPriceResult, error) {
	url := fmt.Sprintf("%s/markets?slug=%s", c.GammaURL, slug)
	resp, err := http.Get(url)
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

func parseFloat(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscanf(s, "%f", &f)
	return f, err
}
