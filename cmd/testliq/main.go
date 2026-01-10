package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/paper"
)

// MarketLevel for liquidity tracking
type MarketLevel struct {
	Price float64
	Size  float64
}

func main() {
	fmt.Println("╔═══════════════════════════════════════════════════════╗")
	fmt.Println("║     LIQUIDITY TEST - Real-time Order Book Analysis    ║")
	fmt.Println("╚═══════════════════════════════════════════════════════╝")
	fmt.Println()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	restClient := api.NewRestClient("")

	// Find active markets
	fmt.Println("🔍 Searching for active 15m markets...")
	markets, err := restClient.Get15mMarkets(ctx, nil)
	if err != nil {
		fmt.Printf("❌ Failed to fetch markets: %v\n", err)
		return
	}

	if len(markets) == 0 {
		fmt.Println("📭 No active markets found")
		return
	}

	// Pick first valid market
	var market *api.Market
	for _, m := range markets {
		slug := strings.ToLower(m.Slug)
		if strings.Contains(slug, "15m") || strings.Contains(slug, "updown") {
			endTime, err := paper.ParseEndTimeFromSlug(m.Slug)
			if err == nil && time.Until(endTime) > 30*time.Second {
				mCopy := m
				market = &mCopy
				break
			}
		}
	}

	if market == nil {
		fmt.Println("📭 No valid 15m markets found")
		return
	}

	fmt.Printf("📊 Testing market: %s\n", market.Slug)
	fmt.Println()

	// Get token info
	tokenMap := make(map[string]string)
	var outcomes []string
	for _, token := range market.Tokens {
		tokenMap[token.TokenID] = token.Outcome
		outcomes = append(outcomes, token.Outcome)
	}

	if len(outcomes) != 2 {
		fmt.Println("❌ Expected 2 outcomes for binary market")
		return
	}

	// Setup WebSocket
	wsMgr := api.NewWSManager("")
	if err := wsMgr.Connect(ctx); err != nil {
		fmt.Printf("❌ WebSocket connect failed: %v\n", err)
		return
	}
	defer wsMgr.Close()

	// Subscribe to order books
	var assetIDs []string
	for _, token := range market.Tokens {
		assetIDs = append(assetIDs, token.TokenID)
	}

	sub := map[string]interface{}{
		"type":       "market",
		"assets_ids": assetIDs,
	}
	if err := wsMgr.Subscribe(ctx, sub); err != nil {
		fmt.Printf("❌ Subscribe failed: %v\n", err)
		return
	}

	wsMsgChan := wsMgr.StartStreaming(ctx)
	fmt.Println("📡 Connected to WebSocket, streaming order book updates...")
	fmt.Println("Press Ctrl+C to stop")
	fmt.Println()

	// Track order books
	tokenToOutcome := tokenMap
	tokenFullAsks := make(map[string][]MarketLevel)
	tokenAsks := make(map[string]float64)
	lastLog := time.Now()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("\n👋 Stopping...")
			return
		case msg, ok := <-wsMsgChan:
			if !ok {
				fmt.Println("⚠️ WebSocket closed")
				return
			}

			// Parse order book
			books, err := api.ParseOrderBooks(msg)
			if err != nil || len(books) == 0 {
				continue
			}

			for _, b := range books {
				outcome := tokenToOutcome[b.AssetID]
				if outcome == "" {
					continue
				}

				// Parse asks
				var asks []MarketLevel
				bestAsk := 1.0
				for _, order := range b.Asks {
					p, _ := strconv.ParseFloat(order.Price, 64)
					s, _ := strconv.ParseFloat(order.Size, 64)
					if p > 0 && p < 1.0 && s > 0 {
						asks = append(asks, MarketLevel{Price: p, Size: s})
						if p < bestAsk {
							bestAsk = p
						}
					}
				}

				sort.Slice(asks, func(i, j int) bool { return asks[i].Price < asks[j].Price })
				tokenFullAsks[outcome] = asks
				if bestAsk < 1.0 {
					tokenAsks[outcome] = bestAsk
				}
			}

			// Log every 3 seconds
			if time.Since(lastLog) > 3*time.Second && len(tokenAsks) >= 2 {
				lastLog = time.Now()

				ask1 := tokenAsks[outcomes[0]]
				ask2 := tokenAsks[outcomes[1]]

				if ask1 > 0 && ask1 < 1 && ask2 > 0 && ask2 < 1 {
					sum := ask1 + ask2
					margin := (1.0 - sum) * 100

					fmt.Println()
					fmt.Printf("⏰ %s\n", time.Now().Format("15:04:05.000"))
					fmt.Printf("═══════════════════════════════════════════════════════\n")
					fmt.Printf("📈 BEST ASKS: %s=$%.3f + %s=$%.3f = $%.3f (margin: %.2f%%)\n",
						outcomes[0], ask1, outcomes[1], ask2, sum, margin)
					fmt.Println()

					// Log individual levels
					asks1 := tokenFullAsks[outcomes[0]]
					asks2 := tokenFullAsks[outcomes[1]]

					fmt.Printf("📊 %s ASK LEVELS (%d total):\n", outcomes[0], len(asks1))
					var cumLiq1 float64
					for i, lvl := range asks1 {
						if i >= 10 {
							break
						}
						cumLiq1 += lvl.Size
						fmt.Printf("   L%d: $%.3f x %6.0f shares (cumulative: %6.0f)\n",
							i+1, lvl.Price, lvl.Size, cumLiq1)
					}

					fmt.Println()
					fmt.Printf("📊 %s ASK LEVELS (%d total):\n", outcomes[1], len(asks2))
					var cumLiq2 float64
					for i, lvl := range asks2 {
						if i >= 10 {
							break
						}
						cumLiq2 += lvl.Size
						fmt.Printf("   L%d: $%.3f x %6.0f shares (cumulative: %6.0f)\n",
							i+1, lvl.Price, lvl.Size, cumLiq2)
					}

					// Calculate matched liquidity at different thresholds
					fmt.Println()
					fmt.Println("📈 MATCHED LIQUIDITY BY MARGIN THRESHOLD:")
					fmt.Println("   (matched = min shares tradeable on BOTH sides)")
					fmt.Println()

					thresholds := []float64{-2.0, -1.0, 0.0, 1.0, 2.0, 3.0, 4.0, 5.0, 6.0}
					for _, threshold := range thresholds {
						maxSum := 1.0 - (threshold / 100.0)

						// Copy asks for calculation
						a1 := make([]MarketLevel, len(asks1))
						copy(a1, asks1)
						a2 := make([]MarketLevel, len(asks2))
						copy(a2, asks2)

						var totalMatched, raw1, raw2 float64
						var levels1, levels2 int

						i, j := 0, 0
						for i < len(a1) && j < len(a2) {
							p1, p2 := a1[i].Price, a2[j].Price
							if p1+p2 > maxSum {
								break
							}

							liq1, liq2 := a1[i].Size, a2[j].Size
							matched := liq1
							if liq2 < matched {
								matched = liq2
							}

							if i+1 > levels1 {
								levels1 = i + 1
								raw1 += a1[i].Size
							}
							if j+1 > levels2 {
								levels2 = j + 1
								raw2 += a2[j].Size
							}
							totalMatched += matched

							rem1, rem2 := liq1-matched, liq2-matched
							if rem1 <= 0 {
								i++
							} else {
								a1[i].Size = rem1
							}
							if rem2 <= 0 {
								j++
							} else {
								a2[j].Size = rem2
							}
						}

						marginSign := ""
						if threshold >= 0 {
							marginSign = "+"
						}

						indicator := "  "
						if threshold == 2.0 {
							indicator = "→ " // Default min margin
						}

						fmt.Printf("   %s%s%.0f%%: matched=%6.0f | %s(L%d)=%6.0f | %s(L%d)=%6.0f\n",
							indicator, marginSign, threshold, totalMatched,
							outcomes[0], levels1, raw1,
							outcomes[1], levels2, raw2)
					}

					fmt.Println("═══════════════════════════════════════════════════════")
				}
			}
		}
	}
}
