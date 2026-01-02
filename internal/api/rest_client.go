package api

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type Token struct {
	TokenID string `json:"token_id"`
	Outcome string `json:"outcome"`
}

type Market struct {
	Active      bool    `json:"active"`
	ConditionID string  `json:"condition_id"`
	Slug        string  `json:"slug"`
	Tokens      []Token `json:"tokens"`
}

type RestClient struct {
	BaseURL string
}

func NewRestClient(baseURL string) *RestClient {
	if baseURL == "" {
		baseURL = "https://clob.polymarket.com"
	}
	return &RestClient{BaseURL: baseURL}
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
