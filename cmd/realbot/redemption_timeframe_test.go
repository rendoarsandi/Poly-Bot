package main

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

func TestCheckRedemptionTimeframeSkipRule(t *testing.T) {
	// Mock REST server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"condition_id":"cond-1","closed":true,"tokens":[{"token_id":"down-token","outcome":"Down","winner":false},{"token_id":"up-token","outcome":"Up","winner":true}]}`))
	}))
	defer server.Close()

	restClient := api.NewRestClient("polymarket")
	restClient.BaseURL = server.URL
	resCache := api.NewResolutionCache(nil, nil, restClient)

	t.Run("1h market skips check in first 10m", func(t *testing.T) {
		engine := paper.NewEngine(100)
		if _, err := engine.BuyForMarket("BTC-1h-1715878400", "Up", 0.60, 5); err != nil {
			t.Fatalf("seed buy failed: %v", err)
		}

		tui := paper.NewTUI(engine, paper.NewOrderBook())
		tui.RecordRound(100, 97, -3, 1, engine.GetPositions(), nil)

		trader := trading.NewEmbeddedPaperRealTrader(&core.Config{ExecutionBackend: core.ExecutionBackendPaper}, engine)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()

		// marketEndTime is only 1 minute ago (within the first 10 minutes skip window)
		marketEndTime := time.Now().Add(-1 * time.Minute)

		checkRedemption(ctx, "BTC-1h-1715878400", "cond-1", []string{"Down", "Up"}, marketEndTime, trader, engine, tui, resCache)

		// Market should NOT have been resolved/redeemed because the check was skipped
		if len(engine.GetPositions()) == 0 {
			t.Fatal("expected positions to still exist because check was skipped")
		}
		if got := engine.GetBalance(); got != 97.0 {
			t.Fatalf("expected balance to remain 97.0 (post-buy remaining cash), got %.2f", got)
		}
	})

	t.Run("1h market checks after 10m", func(t *testing.T) {
		engine := paper.NewEngine(100)
		if _, err := engine.BuyForMarket("BTC-1h-1715878400", "Up", 0.60, 5); err != nil {
			t.Fatalf("seed buy failed: %v", err)
		}

		tui := paper.NewTUI(engine, paper.NewOrderBook())
		tui.RecordRound(100, 97, -3, 1, engine.GetPositions(), nil)

		trader := trading.NewEmbeddedPaperRealTrader(&core.Config{ExecutionBackend: core.ExecutionBackendPaper}, engine)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()

		// marketEndTime is 11 minutes ago (past the 10m skip window)
		marketEndTime := time.Now().Add(-11 * time.Minute)

		checkRedemption(ctx, "BTC-1h-1715878400", "cond-1", []string{"Down", "Up"}, marketEndTime, trader, engine, tui, resCache)

		// Market SHOULD have been resolved and settled
		if got := engine.GetBalance(); math.Abs(got-102.0) > 0.000001 {
			t.Fatalf("expected embedded paper balance 102.00 after settlement, got %.2f", got)
		}
	})

	t.Run("5m and 15m markets check immediately", func(t *testing.T) {
		for _, tf := range []string{"5m", "15m"} {
			t.Run(tf, func(t *testing.T) {
				engine := paper.NewEngine(100)
				marketID := "BTC-" + tf + "-1715878400"
				if _, err := engine.BuyForMarket(marketID, "Up", 0.60, 5); err != nil {
					t.Fatalf("seed buy failed: %v", err)
				}

				tui := paper.NewTUI(engine, paper.NewOrderBook())
				tui.RecordRound(100, 97, -3, 1, engine.GetPositions(), nil)

				trader := trading.NewEmbeddedPaperRealTrader(&core.Config{ExecutionBackend: core.ExecutionBackendPaper}, engine)

				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				defer cancel()

				// marketEndTime is only 1 minute ago (within 10m, but 5m/15m are not skipped)
				marketEndTime := time.Now().Add(-1 * time.Minute)

				checkRedemption(ctx, marketID, "cond-1", []string{"Down", "Up"}, marketEndTime, trader, engine, tui, resCache)

				// Market SHOULD have been resolved and settled
				if got := engine.GetBalance(); math.Abs(got-102.0) > 0.000001 {
					t.Fatalf("expected embedded paper balance 102.00 after settlement, got %.2f", got)
				}
			})
		}
	})
}
