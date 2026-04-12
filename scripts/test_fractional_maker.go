//go:build ignore

package main

import (
	"context"
	"fmt"
	"log"

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

	// Find an active market to test with
	markets, err := rest.GetMarketsByTimeframe(ctx, []string{"btc"}, "15m")
	if err != nil || len(markets) == 0 {
		log.Fatalf("Failed to get markets")
	}

	market := markets[0]
	tokenID := market.Tokens[0].TokenID

	fmt.Printf("🧪 Testing MAKER Fractional Share Execution\n")

	// Maker Buy 0.5 shares at $0.01 limit (Deep out of money)
	buyReq := &api.OrderRequest{
		TokenID:     tokenID,
		Price:       0.01,
		Size:        0.5,
		Side:        api.SideBuy,
		OrderType:   api.OrderTypeLimit,
		TimeInForce: api.TIFFillAndKill,
		FeeRateBps:  1000,
	}

	fmt.Printf("🛒 ACTION: Maker Buying 0.5 shares at $0.01 limit\n")
	buyResp, err := clob.PlaceOrder(ctx, buyReq)
	if err != nil {
		fmt.Printf("   ❌ ERROR: %v\n", err)
		return
	}
	
	if buyResp.Success {
		fmt.Printf("   ✅ SUCCESS! Buy Order ID: %s (Status: %s)\n", buyResp.OrderID, buyResp.Status)
	} else {
		fmt.Printf("   🚫 REJECTED: %s (Status: %s)\n", buyResp.ErrorMsg, buyResp.Status)
	}
}