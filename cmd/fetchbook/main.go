package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"Market-bot/internal/api"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := api.NewRestClient("")

	// Try to find active 15-minute markets first
	fmt.Println("Fetching active 15-minute markets...")
	assets := []string{"BTC", "ETH", "SOL"}
	markets, err := client.Get15mMarkets(ctx, assets)
	if err != nil {
		fmt.Printf("Error fetching 15m markets: %v\n", err)
	}

	fmt.Printf("Found %d 15-minute markets\n", len(markets))

	// If no 15m markets, use a known active market slug
	if len(markets) == 0 {
		fmt.Println("\nNo 15m markets active. Trying a known crypto market...")
		// Try to get any market with crypto
		allMarkets, err := client.ListMarkets(ctx)
		if err != nil {
			fmt.Printf("Error listing markets: %v\n", err)
			return
		}
		fmt.Printf("Found %d total markets\n", len(allMarkets))

		// Find first market with tokens
		for i := range allMarkets {
			if len(allMarkets[i].Tokens) >= 2 && allMarkets[i].Active {
				markets = append(markets, allMarkets[i])
				if len(markets) >= 1 {
					break
				}
			}
		}
	}

	if len(markets) == 0 {
		fmt.Println("No markets with tokens found")
		return
	}

	targetMarket := &markets[0]
	fmt.Printf("\nMarket: %s\n", targetMarket.Slug)
	fmt.Printf("Tokens: %d\n", len(targetMarket.Tokens))

	// Fetch order book for both tokens
	type Level struct {
		Price float64
		Size  float64
	}
	var allAsks [2][]Level
	var outcomes [2]string

	for idx, token := range targetMarket.Tokens {
		if idx >= 2 {
			break
		}
		outcomes[idx] = token.Outcome
		fmt.Printf("\n=== %s ===\n", token.Outcome)

		book, err := client.GetOrderBook(ctx, token.TokenID)
		if err != nil {
			fmt.Printf("Error fetching book: %v\n", err)
			continue
		}

		fmt.Printf("Asks (sell orders) - %d levels:\n", len(book.Asks))

		for _, a := range book.Asks {
			p, _ := strconv.ParseFloat(a.Price, 64)
			s, _ := strconv.ParseFloat(a.Size, 64)
			allAsks[idx] = append(allAsks[idx], Level{Price: p, Size: s})
		}
		sort.Slice(allAsks[idx], func(i, j int) bool { return allAsks[idx][i].Price < allAsks[idx][j].Price })

		// Show first 15 levels
		for i, a := range allAsks[idx] {
			if i >= 15 {
				fmt.Printf("  ... and %d more levels\n", len(allAsks[idx])-15)
				break
			}
			fmt.Printf("  Level %2d: $%.3f x %.0f shares\n", i+1, a.Price, a.Size)
		}
	}

	// Now simulate the depth aggregation
	fmt.Printf("\n\n=== DEPTH AGGREGATION SIMULATION ===\n")
	fmt.Printf("maxSum = 0.98 (2%% min margin)\n\n")

	asks1 := allAsks[0]
	asks2 := allAsks[1]
	maxSum := 0.98

	if len(asks1) == 0 || len(asks2) == 0 {
		fmt.Println("No asks available")
		return
	}

	// Show top of book
	fmt.Printf("Top of book: %s@$%.3f + %s@$%.3f = $%.3f (%.1f%% margin)\n\n",
		outcomes[0], asks1[0].Price, outcomes[1], asks2[0].Price,
		asks1[0].Price+asks2[0].Price, (1-(asks1[0].Price+asks2[0].Price))*100)

	var totalMatched float64
	var rawLiq1, rawLiq2 float64
	var maxValidI, maxValidJ int

	i, j := 0, 0
	iteration := 0
	for i < len(asks1) && j < len(asks2) {
		p1 := asks1[i].Price
		p2 := asks2[j].Price
		sum := p1 + p2

		fmt.Printf("Iter %d: i=%d j=%d | %.3f + %.3f = %.3f (%.1f%%)",
			iteration, i, j, p1, p2, sum, (1-sum)*100)

		if sum > maxSum {
			fmt.Printf(" → BREAK (exceeds %.2f)\n", maxSum)
			break
		}
		fmt.Println(" ✓")

		if i+1 > maxValidI {
			maxValidI = i + 1
			rawLiq1 += asks1[i].Size
		}
		if j+1 > maxValidJ {
			maxValidJ = j + 1
			rawLiq2 += asks2[j].Size
		}

		liq1 := asks1[i].Size
		liq2 := asks2[j].Size
		matched := liq1
		if liq2 < matched {
			matched = liq2
		}
		totalMatched += matched

		rem1 := liq1 - matched
		rem2 := liq2 - matched
		if rem1 <= 0 {
			i++
		} else {
			asks1[i].Size = rem1
		}
		if rem2 <= 0 {
			j++
		} else {
			asks2[j].Size = rem2
		}
		iteration++

		if iteration > 20 {
			fmt.Println("... (truncated)")
			break
		}
	}

	fmt.Printf("\n=== RESULTS ===\n")
	fmt.Printf("rawLiq: %.0f / %.0f\n", rawLiq1, rawLiq2)
	fmt.Printf("totalMatched: %.0f\n", totalMatched)
	fmt.Printf("validLevels: %d / %d\n", maxValidI, maxValidJ)
	fmt.Printf("bookDepth: %d / %d\n", len(allAsks[0]), len(allAsks[1]))
}
