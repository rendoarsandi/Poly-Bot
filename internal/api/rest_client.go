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
	Slug    string   `json:"slug"`
	EndDate string   `json:"endDate"`
	Markets []struct {
		ConditionID string `json:"conditionId"`
	} `json:"markets"`
}

func (c *RestClient) Get15mMarkets(assets []string) ([]Market, error) {
	if len(assets) == 0 {
		assets = []string{"btc", "eth", "sol", "xrp"}
	}

	now := time.Now().UTC()
	currentTs := now.Unix()

	// Calculate the current 15m window (the one that hasn't ended yet)
	// Current window: round down to nearest 900, that's the START of current window
	// The window END is start + 900
	currentWindowEnd := ((currentTs / 900) + 1) * 900

	var markets []Market

	// Check current window and next 2 windows
	// Start from current window (i=0), then next window (i=1), etc.
	for _, asset := range assets {
		for i := 0; i < 3; i++ {
			windowEnd := currentWindowEnd + (int64(i) * 900)

			slug := fmt.Sprintf("%s-updown-15m-%d", asset, windowEnd)

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

			// Get full details from CLOB
			market, err := c.GetMarket(event.Markets[0].ConditionID)
			if err != nil || !market.Active || market.Closed {
				continue
			}

			// Check if market has any liquidity (at least one side has orders)
			hasLiquidity := false
			for _, token := range market.Tokens {
				book, err := c.GetOrderBook(token.TokenID)
				if err == nil && (len(book.Bids) > 0 || len(book.Asks) > 0) {
					hasLiquidity = true
					break
				}
			}

			if !hasLiquidity {
				fmt.Printf("⚠️  %s has no liquidity, skipping...\n", slug)
				continue
			}

			market.Slug = slug // Use the 15m slug
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

// GammaMarketPrice represents the price from Gamma API
type GammaMarketPrice struct {
	ConditionID string  `json:"condition_id"`
	OutcomeYes  float64 `json:"outcomePrices"` // from outcomes array
	Tokens      []struct {
		TokenID string  `json:"token_id"`
		Outcome string  `json:"outcome"`
		Price   float64 `json:"price"`
	} `json:"tokens"`
}

// GetGammaPrice fetches the current price from Gamma API (used on Polymarket website)
func (c *RestClient) GetGammaPrice(conditionID string) (map[string]float64, error) {
	url := fmt.Sprintf("%s/markets/%s", c.GammaURL, conditionID)
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch gamma price: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch gamma price: status %d", resp.StatusCode)
	}

	var result struct {
		Tokens []struct {
			TokenID string `json:"token_id"`
			Outcome string `json:"outcome"`
			Price   string `json:"price"`
		} `json:"tokens"`
		OutcomePrices string `json:"outcomePrices"` // JSON array like "[0.02, 0.98]"
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode gamma price: %w", err)
	}

	prices := make(map[string]float64)
	for _, t := range result.Tokens {
		if p, err := parseFloat(t.Price); err == nil {
			prices[t.Outcome] = p
		}
	}

	return prices, nil
}

func parseFloat(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscanf(s, "%f", &f)
	return f, err
}
