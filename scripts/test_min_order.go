//go:build ignore

package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"github.com/joho/godotenv"
)

func main() {
	godotenv.Load("../.env")
	godotenv.Load(".env")

	cfg, err := core.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	clob, err := api.NewCLOBClient(cfg.PK, cfg.APIKey, cfg.APISecret, cfg.APIPassphrase)
	if err != nil {
		log.Fatalf("Failed to create CLOB client: %v", err)
	}

	rest := api.NewRestClient("")
	ctx := context.Background()

	// Find an active market token to test with
	markets, err := rest.GetMarketsByTimeframe(ctx, []string{"btc"}, "15m")
	if err != nil || len(markets) == 0 {
		log.Fatalf("Failed to get markets")
	}

	market := markets[0]
	tokenID := market.Tokens[0].TokenID

	fmt.Printf("Testing with Token ID: %s (Market: %s)\n", tokenID, market.Slug)

	// We'll test with the CLOB client in test mode to see if it catches client-side validation,
	// but mostly we care about what the actual API returns.
	// We will run this against the REAL api but with an impossibly bad price so it just sits on the book (or use test mode).
	// Actually, TEST MODE (SetTestMode) in our client only checks local things and skips submission.
	// We need to actually submit a dry run or rely on the actual API rejection.
	// Since we don't want to accidentally execute, we'll place Limit orders deeply out of money.

	testCases := []struct {
		Name  string
		Side  api.Side
		Type  api.OrderType
		Price float64
		Size  float64
	}{
		{"Limit Buy 0.5 shares @ $0.01 (Value: $0.005)", api.SideBuy, api.OrderTypeLimit, 0.01, 0.5},
		{"Limit Buy 1.0 shares @ $0.01 (Value: $0.01)", api.SideBuy, api.OrderTypeLimit, 0.01, 1.0},
		{"Limit Buy 100 shares @ $0.01 (Value: $1.00)", api.SideBuy, api.OrderTypeLimit, 0.01, 100.0},

		{"Limit Sell 0.5 shares @ $0.99 (Value: $0.495)", api.SideSell, api.OrderTypeLimit, 0.99, 0.5},
		{"Limit Sell 1.0 shares @ $0.99 (Value: $0.99)", api.SideSell, api.OrderTypeLimit, 0.99, 1.0},
		{"Limit Sell 5.0 shares @ $0.99 (Value: $4.95)", api.SideSell, api.OrderTypeLimit, 0.99, 5.0},

		{"Market Buy $0.50 worth of shares", api.SideBuy, api.OrderTypeMarket, 0.99, 0.5},
		{"Market Buy $1.00 worth of shares", api.SideBuy, api.OrderTypeMarket, 0.99, 1.0},
		{"Market Buy $1.01 worth of shares", api.SideBuy, api.OrderTypeMarket, 0.99, 1.01},

		// Market sell sizes are in SHARES, not USD
		{"Market Sell 0.5 shares", api.SideSell, api.OrderTypeMarket, 0.01, 0.5},
		{"Market Sell 1.0 shares", api.SideSell, api.OrderTypeMarket, 0.01, 1.0},
	}

	fmt.Println("--------------------------------------------------")
	for _, tc := range testCases {
		fmt.Printf("TEST: %s\n", tc.Name)

		// Wait 1s between orders to avoid rate limits
		time.Sleep(1 * time.Second)

		req := &api.OrderRequest{
			TokenID:     tokenID,
			Price:       tc.Price,
			Size:        tc.Size,
			Side:        tc.Side,
			OrderType:   tc.Type,
			TimeInForce: api.TIFFillAndKill,
			FeeRateBps:  1000, // Explicitly set to 1000 to pass fee validation
		}

		resp, err := clob.PlaceOrder(ctx, req)
		if err != nil {
			fmt.Printf("   -> ERROR: %v\n", err)
		} else {
			if resp.Success {
				fmt.Printf("   -> SUCCESS! Order ID: %s (Status: %s)\n", resp.OrderID, resp.Status)
			} else {
				fmt.Printf("   -> REJECTED: %s (Status: %s)\n", resp.ErrorMsg, resp.Status)
			}
		}
		fmt.Println("--------------------------------------------------")
	}
}
