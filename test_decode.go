package main

import (
	"context"
	"fmt"
	"time"

	"Market-bot/internal/api"

	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load()
	rpcURL := "https://polygon-rpc.com"

	polygon := api.NewPolygonClient(rpcURL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// A tx from the logs: 0x08ba8be2... (or the one they mentioned: 0x7e4e40fc...)
	txHash := "0x7e4e40fcd900c410dbcf823a07b71383cd89e18b4097f48b11119b40097f6c38" 

	tx, err := polygon.GetTransactionByHash(ctx, txHash)
	if err != nil || tx == nil {
		// fallback to 0x08ba8be2
		txHash = "0x08ba8be297d2d38ff31f26a117b5e408ec209c158586d634db2dc052e69ca8ff"
		tx, _ = polygon.GetTransactionByHash(ctx, txHash)
		if tx == nil {
			fmt.Println("Error or tx nil")
			return
		}
	}

	orders, err := api.DecodePolymarketMatchOrdersInput(tx.Input)
	if err != nil {
		fmt.Println("Decode error:", err)
	} else {
		for i, order := range orders {
			fmt.Printf("Order %d: Side: %v, MakerAmt: %v, TakerAmt: %v, Filled: %v\n", i, order.Side, order.MakerAmount, order.TakerAmount, order.FilledShares)
		}
	}
}
