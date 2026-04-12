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

	// Find an active market to test with
	markets, err := rest.GetMarketsByTimeframe(ctx, []string{"btc"}, "15m")
	if err != nil || len(markets) == 0 {
		log.Fatalf("Failed to get markets")
	}

	market := markets[0]
	tokenID := market.Tokens[0].TokenID

	fmt.Printf("🧪 Testing Fractional Share Execution (99c Slippage Trick)\n")
	fmt.Printf("📈 Market: %s\n", market.Slug)
	fmt.Printf("🪙 Token ID: %s\n", tokenID)
	fmt.Println("--------------------------------------------------")

	// 1. Buy 1.02 shares at $0.99 limit (Value: ~$1.01)
	buyReq := &api.OrderRequest{
		TokenID:     tokenID,
		Price:       0.99,
		Size:        1.02,
		Side:        api.SideBuy,
		OrderType:   api.OrderTypeLimit,
		TimeInForce: api.TIFFillAndKill,
		FeeRateBps:  0, // some markets have fees, standard is to pass what the API gives, but let's try 0 or 1000
	}
	
	// Actually we should fetch fee rate just in case, but let's try 1000 to avoid rejection
	buyReq.FeeRateBps = 1000

	fmt.Printf("🛒 ACTION: Buying 0.5 shares at $0.99 limit (Value: $0.495)\n")
	buyResp, err := clob.PlaceOrder(ctx, buyReq)
	if err != nil {
		fmt.Printf("   ❌ ERROR: %v\n", err)
		return
	}
	
	if buyResp.Success {
		fmt.Printf("   ✅ SUCCESS! Buy Order ID: %s (Status: %s)\n", buyResp.OrderID, buyResp.Status)
	} else {
		fmt.Printf("   🚫 REJECTED: %s (Status: %s)\n", buyResp.ErrorMsg, buyResp.Status)
		return // Don't sell if buy failed
	}

	fmt.Println("⏳ Waiting 2 seconds before selling...")
	time.Sleep(2 * time.Second)

	// 2. Sell 0.5 shares at $0.01 limit
	sellReq := &api.OrderRequest{
		TokenID:     tokenID,
		Price:       0.01,
		Size:        0.5,
		Side:        api.SideSell,
		OrderType:   api.OrderTypeLimit,
		TimeInForce: api.TIFFillAndKill,
		FeeRateBps:  1000,
	}

	fmt.Printf("💸 ACTION: Selling 0.5 shares at $0.01 limit (Value: $0.005)\n")
	sellResp, err := clob.PlaceOrder(ctx, sellReq)
	if err != nil {
		fmt.Printf("   ❌ ERROR: %v\n", err)
	} else {
		if sellResp.Success {
			fmt.Printf("   ✅ SUCCESS! Sell Order ID: %s (Status: %s)\n", sellResp.OrderID, sellResp.Status)
		} else {
			fmt.Printf("   🚫 REJECTED: %s (Status: %s)\n", sellResp.ErrorMsg, sellResp.Status)
		}
	}
	fmt.Println("--------------------------------------------------")
	fmt.Println("🏁 Test complete.")
}