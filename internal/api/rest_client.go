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
	// Start from next window (current might be ending)
	windowStart := ((currentTs / 900) + 1) * 900

	var markets []Market

	// Check next 3 windows (skip current which might be ending)
	for _, asset := range assets {
		for i := 0; i < 3; i++ {
			ts := windowStart + (int64(i) * 900)

			// Skip if window has already ended
			if ts < currentTs {
				continue
			}

			slug := fmt.Sprintf("%s-updown-15m-%d", asset, ts)

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

			market.Slug = slug // Use the 15m slug
			markets = append(markets, *market)
			break // Found next market for this asset
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
