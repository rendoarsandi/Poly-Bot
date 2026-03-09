package markets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/paper"
)

func TestFindMarkets_CaseSensitivity(t *testing.T) {
	// Set up mock server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Mock Gamma response
		var resp []api.GammaEvent

		// Create a market with an uppercase asset in slug to test case insensitivity
		// Let's use an asset 'BTC' but the target user config might say 'btc'
		// Or the slug has 'btc' and the user config says 'BTC'
		// The issue was matching slug "btc-updown-..." with asset "BTC" using strings.Contains

		market := api.GammaMarket{
			ConditionID:  "0x123",
			ClobTokenIds: `["111","222"]`,
			Outcomes:     `["Yes","No"]`,
			Active:       true,
			Closed:       false,
		}

		// Target slug format is handled in ParseEndTimeFromSlug
		// Let's give it an end time 10 minutes from now
		now := time.Now().Add(10 * time.Minute)
		endDate := now.Format(time.RFC3339)

		event := api.GammaEvent{
			Slug:    "btc-updown-15m",
			EndDate: endDate,
			Markets: []api.GammaMarket{market},
		}
		resp = append(resp, event)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	restClient := api.NewRestClient("")
	restClient.GammaURL = server.URL

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	getConfig := func() paper.TUISettings {
		return paper.TUISettings{
			MarketSlug: "BTC", // User requested uppercase 'BTC'
			Timeframe:  "15m",
			MaxMarkets: 4,
		}
	}

	markets := FindMarkets(ctx, restClient, getConfig, nil)

	if len(markets) == 0 {
		t.Errorf("Failed to find market. Case-sensitivity bug still present.")
	}

	if _, ok := markets["BTC"]; !ok {
		t.Errorf("Expected market 'BTC' to be found, got %v", markets)
	}
}

func TestDiscoveryTimeframes(t *testing.T) {
	tests := []struct {
		name      string
		requested string
		want      []string
	}{
		{name: "default", requested: "", want: []string{"15m", "5m"}},
		{name: "explicit 15m", requested: "15m", want: []string{"15m"}},
		{name: "explicit all", requested: "all", want: []string{"15m", "5m"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := discoveryTimeframes(tt.requested)
			if len(got) != len(tt.want) {
				t.Fatalf("expected %v, got %v", tt.want, got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("expected %v, got %v", tt.want, got)
				}
			}
		})
	}
}
