package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"Market-bot/internal/api"
)

func main() {
	wsMgr := api.NewWSManager("")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := wsMgr.Connect(ctx); err != nil {
		log.Fatal(err)
	}
	defer wsMgr.Close()

	// Subscribe to a known active market token
	// Just fetch one from REST first
	rest := api.NewRestClient("")
	markets, err := rest.GetMarketsByTimeframe(ctx, nil, "15m")
	if err != nil || len(markets) == 0 {
		log.Fatal("No markets")
	}
	
	var tokenIDs []string
	for _, t := range markets[0].Tokens {
		tokenIDs = append(tokenIDs, t.TokenID)
	}

	wsMgr.Subscribe(ctx, map[string]interface{}{
		"assets_ids": tokenIDs,
		"type":       "market",
	})

	ch := wsMgr.StartStreaming(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			// Just print the raw message and exit if it's a price_change
			fmt.Println(string(msg))
			update, err := api.ParsePriceUpdate(msg)
			if err == nil && len(update.PriceChanges) > 0 {
				fmt.Println("Found price_change:", string(msg))
				return
			}
		}
	}
}
