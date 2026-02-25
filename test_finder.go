package main

import (
	"context"
	"fmt"
	"Market-bot/internal/api"
	"Market-bot/internal/markets"
	"Market-bot/internal/paper"
)

func main() {
	restClient := api.NewRestClient("")
	ctx := context.Background()

	getConfig := func() paper.TUISettings {
		return paper.TUISettings{
			MarketSlug: "", // Blank
			Timeframe:  "15m",
			MaxMarkets: 4,
		}
	}

	found := markets.FindMarkets(ctx, restClient, getConfig, func(f string, a ...interface{}) {
		fmt.Printf(f+"\n", a...)
	})

	for k, m := range found {
		fmt.Printf("Found: %s -> %s\n", k, m.Slug)
	}
}
