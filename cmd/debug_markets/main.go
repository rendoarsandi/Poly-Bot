package main

import (
	"context"
	"fmt"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"Market-bot/internal/api"
)

func main() {
	client := api.NewRestClient("")
	ctx := context.Background()

	assets := []string{"btc", "eth"}
	
	fmt.Println("🔍 SEARCHING FOR ANY LIQUID 15M/UPDOWN MARKETS...")
	fmt.Println("-------------------------------------------------------")

	for _, asset := range assets {
		// Search Gamma for the asset without specific timestamp
		url := fmt.Sprintf("https://gamma-api.polymarket.com/events?slug=%s-updown", asset)
		resp, err := http.Get(url)
		if err != nil {
			fmt.Printf("Error searching %s: %v\n", asset, err)
			continue
		}
		
		var events []struct {
			Slug    string `json:"slug"`
			Markets []struct {
				ConditionID  string `json:"conditionId"`
				ClobTokenIds string `json:"clobTokenIds"`
				Outcomes     string `json:"outcomes"`
				Active       bool   `json:"active"`
				Closed       bool   `json:"closed"`
			} `json:"markets"`
		}
		
		json.NewDecoder(resp.Body).Decode(&events)
		resp.Body.Close()

		for _, event := range events {
			if !strings.Contains(event.Slug, "15m") && !strings.Contains(event.Slug, "updown") {
				continue
			}

			for _, m := range event.Markets {
				if m.Closed { continue }
				
				var tokenIds []string
				json.Unmarshal([]byte(m.ClobTokenIds), &tokenIds)
				
				fmt.Printf("\n📈 Candidate: %s (Active: %v)\n", event.Slug, m.Active)
				
				for i, tid := range tokenIds {
					book, err := client.GetOrderBook(ctx, tid)
					if err != nil {
						fmt.Printf("      Token %d | Error: %v\n", i, err)
						continue
					}

					bid, ask := 0.0, 0.0
					if len(book.Bids) > 0 {
						bid, _ = strconv.ParseFloat(book.Bids[0].Price, 64)
					}
					if len(book.Asks) > 0 {
						ask, _ = strconv.ParseFloat(book.Asks[0].Price, 64)
					}
					
					// Use a simple outcome label since we don't have the outcomes array handy
					label := "Up"
					if i == 1 { label = "Down" }
					fmt.Printf("      %-5s | Bid: %.3f | Ask: %.3f | Spread: %.3f\n", label, bid, ask, ask-bid)
				}
			}
		}
	}
}