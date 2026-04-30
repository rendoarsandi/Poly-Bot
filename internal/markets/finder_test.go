package markets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/paper"
)

func TestFindMarkets_CaseSensitivity(t *testing.T) {
	// Set up mock server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("slug"); !strings.HasPrefix(got, "btc-updown-15m-") {
			_ = json.NewEncoder(w).Encode([]api.GammaEvent{})
			return
		}

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
	restClient.BaseURL = server.URL
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

	if _, ok := markets["BTC-15m"]; !ok {
		t.Errorf("Expected market 'BTC-15m' to be found, got %v", markets)
	}
}

func TestFindMarketsSupportsOneHourMarkets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("slug"); !strings.HasPrefix(got, "bitcoin-up-or-down-") {
			_ = json.NewEncoder(w).Encode([]api.GammaEvent{})
			return
		}

		endDate := time.Now().Add(45 * time.Minute).Format(time.RFC3339)
		event := api.GammaEvent{
			Slug:    "bitcoin-up-or-down-april-19-2026-2am-et",
			EndDate: endDate,
			Markets: []api.GammaMarket{{
				ConditionID:  "0x1h",
				Slug:         "bitcoin-up-or-down-april-19-2026-2am-et",
				ClobTokenIds: `["111","222"]`,
				Outcomes:     `["Yes","No"]`,
				Active:       true,
				Closed:       false,
			}},
		}
		_ = json.NewEncoder(w).Encode([]api.GammaEvent{event})
	}))
	defer server.Close()

	restClient := api.NewRestClient("")
	restClient.BaseURL = server.URL
	restClient.GammaURL = server.URL

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	getConfig := func() paper.TUISettings {
		return paper.TUISettings{
			MarketSlug: "BTC",
			Timeframe:  "1h",
			MaxMarkets: 4,
		}
	}

	markets := FindMarkets(ctx, restClient, getConfig, nil)
	if len(markets) == 0 {
		t.Fatalf("expected one-hour market to be discovered")
	}
	if _, ok := markets["BTC-1h"]; !ok {
		t.Fatalf("expected market 'BTC-1h' to be found, got %v", markets)
	}
}

func TestFindMarketsSkipsSecondaryTimeframeOutsideCopytrade(t *testing.T) {
	var (
		mu             sync.Mutex
		requestedSlugs []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slug := r.URL.Query().Get("slug")
		mu.Lock()
		requestedSlugs = append(requestedSlugs, slug)
		mu.Unlock()
		if !strings.Contains(slug, "-5m-") {
			_ = json.NewEncoder(w).Encode([]api.GammaEvent{})
			return
		}

		endDate := time.Now().Add(10 * time.Minute).Format(time.RFC3339)
		event := api.GammaEvent{
			Slug:    "btc-updown-5m",
			EndDate: endDate,
			Markets: []api.GammaMarket{{
				ConditionID:  "0x5m",
				Slug:         slug,
				ClobTokenIds: `["111","222"]`,
				Outcomes:     `["Yes","No"]`,
				Active:       true,
				Closed:       false,
			}},
		}
		_ = json.NewEncoder(w).Encode([]api.GammaEvent{event})
	}))
	defer server.Close()

	restClient := api.NewRestClient("")
	restClient.BaseURL = server.URL
	restClient.GammaURL = server.URL

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	markets := FindMarkets(ctx, restClient, func() paper.TUISettings {
		return paper.TUISettings{
			MarketSlug:   "BTC",
			Timeframe:    "5m",
			MaxMarkets:   1,
			PaperArbMode: "taker",
		}
	}, nil)

	if len(markets) == 0 {
		t.Fatalf("expected 5m market to be discovered")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, slug := range requestedSlugs {
		if strings.Contains(slug, "-15m-") {
			t.Fatalf("did not expect secondary 15m lookup outside copytrade, saw %q", slug)
		}
	}
}

func TestFindMarketsKeepsSecondaryTimeframeForCopytrade(t *testing.T) {
	var (
		mu             sync.Mutex
		requestedSlugs []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slug := r.URL.Query().Get("slug")
		mu.Lock()
		requestedSlugs = append(requestedSlugs, slug)
		mu.Unlock()
		if !strings.Contains(slug, "-5m-") {
			_ = json.NewEncoder(w).Encode([]api.GammaEvent{})
			return
		}

		endDate := time.Now().Add(10 * time.Minute).Format(time.RFC3339)
		event := api.GammaEvent{
			Slug:    "btc-updown-5m",
			EndDate: endDate,
			Markets: []api.GammaMarket{{
				ConditionID:  "0x5m-copytrade",
				Slug:         slug,
				ClobTokenIds: `["111","222"]`,
				Outcomes:     `["Yes","No"]`,
				Active:       true,
				Closed:       false,
			}},
		}
		_ = json.NewEncoder(w).Encode([]api.GammaEvent{event})
	}))
	defer server.Close()

	restClient := api.NewRestClient("")
	restClient.BaseURL = server.URL
	restClient.GammaURL = server.URL

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	markets := FindMarkets(ctx, restClient, func() paper.TUISettings {
		return paper.TUISettings{
			MarketSlug:   "BTC",
			Timeframe:    "5m",
			MaxMarkets:   1,
			PaperArbMode: "copytrade",
		}
	}, nil)

	if len(markets) == 0 {
		t.Fatalf("expected 5m market to be discovered")
	}

	mu.Lock()
	defer mu.Unlock()
	sawSecondary := false
	for _, slug := range requestedSlugs {
		if strings.Contains(slug, "-15m-") {
			sawSecondary = true
			break
		}
	}
	if !sawSecondary {
		t.Fatal("expected copytrade discovery to keep secondary 15m lookup")
	}
}
