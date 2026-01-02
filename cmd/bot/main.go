package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/strategy"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("Critical error: %v", err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 1. Load Configuration
	cfg, err := core.LoadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	fmt.Printf("Starting Market Bot for slug: %s\n", cfg.MarketSlug)

	// 2. Fetch Market Details
	restClient := api.NewRestClient("")
	market, err := restClient.GetMarket(cfg.MarketSlug)
	if err != nil {
		return fmt.Errorf("failed to fetch market details: %w", err)
	}
	fmt.Printf("Market Found: %s (Condition ID: %s)\n", market.Slug, market.ConditionID)

	// 3. Setup WebSocket
	wsMgr := api.NewWSManager("")
	if err := wsMgr.Connect(ctx); err != nil {
		return fmt.Errorf("failed to connect to websocket: %w", err)
	}
	defer wsMgr.Close()

	// 4. Subscribe to Order Books
	var assetIDs []string
	for _, token := range market.Tokens {
		assetIDs = append(assetIDs, token.TokenID)
	}

	sub := map[string]interface{}{
		"type":       "market",
		"assets_ids": assetIDs,
	}
	if err := wsMgr.Subscribe(ctx, sub); err != nil {
		return fmt.Errorf("failed to subscribe to market: %w", err)
	}
	fmt.Println("Subscribed to order book updates. Listening...")

	// 5. Data Loop
	prices := make(map[string]string) // asset_id -> best_bid

	for {
		select {
		case <-ctx.Done():
			fmt.Println("Shutting down...")
			return nil
		default:
			msg, err := wsMgr.ReadMessage(ctx)
			if err != nil {
				log.Printf("Read error: %v", err)
				continue
			}

			book, err := api.ParseOrderBook(msg)
			if err != nil {
				// Ignore non-book events or parse errors for now
				continue
			}

			if len(book.Buys) > 0 {
				prices[book.AssetID] = book.Buys[0].Price
			}

			// If we have prices for both Yes and No, calculate sum
			if len(prices) >= 2 {
				// Note: In a real bot, we'd map assetIDs back to Yes/No outcomes.
				// For this monitor, we just sum whatever two assets we have.
				var pList []string
				for _, p := range prices {
					pList = append(pList, p)
				}
				
				sum, err := strategy.CalculateDiscountSum(pList[0], pList[1])
				if err == nil {
					fmt.Printf("[%s] Sum: %.4f | Prices: %s + %s\n", 
						book.Timestamp, sum, pList[0], pList[1])
					
					if sum < 1.00 {
						fmt.Printf("🔥 PROFITABLE OPPORTUNITY DETECTED: %.2f%% margin\n", (1.0-sum)*100)
					}
				}
			}
		}
	}
}