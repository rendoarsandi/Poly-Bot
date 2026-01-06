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
)

func main() {
	if err := run(); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}

// LevelCount tracks how many price levels we receive
type LevelCount struct {
	Source       string
	BidLevels    int
	AskLevels    int
	TotalBidSize float64
	TotalAskSize float64
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Println("╔═══════════════════════════════════════════════════════════════╗")
	fmt.Println("║     POLYMARKET LIVE LIQUIDITY DIAGNOSTIC                      ║")
	fmt.Println("║     Compare REST vs WebSocket vs Real Market                  ║")
	fmt.Println("╚═══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	restClient := api.NewRestClient("")

	// Find active 15m markets
	fmt.Println("🔍 Searching for active 15m markets...")
	markets, err := restClient.Get15mMarkets(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to fetch markets: %w", err)
	}

	if len(markets) == 0 {
		fmt.Println("❌ No active 15m markets found")
		return nil
	}

	// Pick first market
	market := markets[0]
	fmt.Printf("\n📊 Found market: %s\n", market.Slug)
	fmt.Println(strings.Repeat("═", 70))

	// Build token map
	tokenMap := make(map[string]string)
	tokenIDs := make([]string, 0)
	for _, t := range market.Tokens {
		tokenMap[t.TokenID] = t.Outcome
		tokenIDs = append(tokenIDs, t.TokenID)
		fmt.Printf("   Token: %s → %s\n", t.TokenID[:16]+"...", t.Outcome)
	}
	fmt.Println()

	// Connect WebSocket
	fmt.Println("📡 Connecting WebSocket...")
	wsMgr := api.NewWSManager("")
	if err := wsMgr.Connect(ctx); err != nil {
		return fmt.Errorf("WS connect failed: %w", err)
	}
	defer wsMgr.Close()

	// Subscribe
	sub := map[string]interface{}{
		"type":       "market",
		"assets_ids": tokenIDs,
	}
	if err := wsMgr.Subscribe(ctx, sub); err != nil {
		return fmt.Errorf("subscribe failed: %w", err)
	}

	wsChan := wsMgr.StartStreaming(ctx)
	fmt.Println("✅ WebSocket connected and subscribed")
	fmt.Println()

	// Track data
	type BookData struct {
		Source    string
		Timestamp time.Time
		BestBid   float64
		BestAsk   float64
		BidDepth  []api.PriceLevel // Full depth
		AskDepth  []api.PriceLevel
		TotalBidLiq float64
		TotalAskLiq float64
	}

	wsData := make(map[string]*BookData)
	restData := make(map[string]*BookData)
	lastRestPoll := time.Time{}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	displayTicker := time.NewTicker(1 * time.Second)
	defer displayTicker.Stop()

	fmt.Println("📈 Live data (Ctrl+C to exit):")
	fmt.Println(strings.Repeat("─", 70))

	for {
		select {
		case <-ctx.Done():
			fmt.Println("\n👋 Exiting...")
			return nil

		case msg, ok := <-wsChan:
			if !ok {
				fmt.Println("⚠️ WebSocket closed")
				continue
			}

			// Parse order books
			if books, err := api.ParseOrderBooks(msg); err == nil && len(books) > 0 {
				for _, b := range books {
					outcome := tokenMap[b.AssetID]
					if outcome == "" {
						continue
					}

					bestBid, bestAsk := 0.0, 1.0
					totalBidLiq, totalAskLiq := 0.0, 0.0

					for _, order := range b.Bids {
						p, _ := strconv.ParseFloat(order.Price, 64)
						s, _ := strconv.ParseFloat(order.Size, 64)
						if p > bestBid {
							bestBid = p
						}
						totalBidLiq += s
					}
					for _, order := range b.Asks {
						p, _ := strconv.ParseFloat(order.Price, 64)
						s, _ := strconv.ParseFloat(order.Size, 64)
						if p < bestAsk && p > 0 {
							bestAsk = p
						}
						totalAskLiq += s
					}
					if bestAsk >= 1.0 {
						bestAsk = 0
					}

					wsData[outcome] = &BookData{
						Source:      "WS",
						Timestamp:   time.Now(),
						BestBid:     bestBid,
						BestAsk:     bestAsk,
						BidDepth:    b.Bids,
						AskDepth:    b.Asks,
						TotalBidLiq: totalBidLiq,
						TotalAskLiq: totalAskLiq,
					}
				}
			}

		case <-ticker.C:
			// Poll REST every 2 seconds
			if time.Since(lastRestPoll) >= 2*time.Second {
				lastRestPoll = time.Now()
				for tokenID, outcome := range tokenMap {
					book, err := restClient.GetOrderBook(ctx, tokenID)
					if err != nil {
						continue
					}

					bestBid, bestAsk := 0.0, 1.0
					totalBidLiq, totalAskLiq := 0.0, 0.0

					for _, order := range book.Bids {
						p, _ := strconv.ParseFloat(order.Price, 64)
						s, _ := strconv.ParseFloat(order.Size, 64)
						if p > bestBid {
							bestBid = p
						}
						totalBidLiq += s
					}
					for _, order := range book.Asks {
						p, _ := strconv.ParseFloat(order.Price, 64)
						s, _ := strconv.ParseFloat(order.Size, 64)
						if p < bestAsk && p > 0 {
							bestAsk = p
						}
						totalAskLiq += s
					}
					if bestAsk >= 1.0 {
						bestAsk = 0
					}

					restData[outcome] = &BookData{
						Source:      "REST",
						Timestamp:   time.Now(),
						BestBid:     bestBid,
						BestAsk:     bestAsk,
						BidDepth:    book.Bids,
						AskDepth:    book.Asks,
						TotalBidLiq: totalBidLiq,
						TotalAskLiq: totalAskLiq,
					}
				}
			}

		case <-displayTicker.C:
			// Clear and redraw
			fmt.Print("\033[H\033[2J") // Clear screen
			fmt.Println("╔═══════════════════════════════════════════════════════════════╗")
			fmt.Println("║     POLYMARKET LIVE LIQUIDITY DIAGNOSTIC                      ║")
			fmt.Println("╚═══════════════════════════════════════════════════════════════╝")
			fmt.Printf("\n📊 Market: %s\n", market.Slug)
			fmt.Printf("⏰ Time: %s\n\n", time.Now().Format("15:04:05.000"))

			outcomes := []string{}
			for _, t := range market.Tokens {
				outcomes = append(outcomes, t.Outcome)
			}

			// Show comparison table
			fmt.Println("┌────────────┬────────┬──────────┬──────────┬──────────┬──────────┬─────────┐")
			fmt.Println("│  Outcome   │ Source │  BidPrice│  AskPrice│  BidLiq  │  AskLiq  │  Age    │")
			fmt.Println("├────────────┼────────┼──────────┼──────────┼──────────┼──────────┼─────────┤")

			for _, outcome := range outcomes {
				// WebSocket data
				if ws, ok := wsData[outcome]; ok {
					age := time.Since(ws.Timestamp)
					ageStr := fmt.Sprintf("%.1fs", age.Seconds())
					if age > 5*time.Second {
						ageStr = fmt.Sprintf("\033[31m%.1fs\033[0m", age.Seconds()) // Red if stale
					}
					fmt.Printf("│ %-10s │ \033[32mWS\033[0m     │ $%-7.3f │ $%-7.3f │ %8.1f │ %8.1f │ %7s │\n",
						truncate(outcome, 10), ws.BestBid, ws.BestAsk, ws.TotalBidLiq, ws.TotalAskLiq, ageStr)
				} else {
					fmt.Printf("│ %-10s │ \033[32mWS\033[0m     │ \033[33m(no data)\033[0m│          │          │          │         │\n", truncate(outcome, 10))
				}

				// REST data
				if rest, ok := restData[outcome]; ok {
					age := time.Since(rest.Timestamp)
					ageStr := fmt.Sprintf("%.1fs", age.Seconds())
					fmt.Printf("│ %-10s │ \033[33mREST\033[0m   │ $%-7.3f │ $%-7.3f │ %8.1f │ %8.1f │ %7s │\n",
						"", rest.BestBid, rest.BestAsk, rest.TotalBidLiq, rest.TotalAskLiq, ageStr)
				}

				// Check for mismatch
				if ws, ok1 := wsData[outcome]; ok1 {
					if rest, ok2 := restData[outcome]; ok2 {
						bidDiff := abs(ws.BestBid - rest.BestBid)
						askDiff := abs(ws.BestAsk - rest.BestAsk)
						if bidDiff > 0.01 || askDiff > 0.01 {
							fmt.Printf("│            │ \033[31m⚠️ MISMATCH: bid diff=$%.3f, ask diff=$%.3f\033[0m          │\n", bidDiff, askDiff)
						}
					}
				}
			}
			fmt.Println("└────────────┴────────┴──────────┴──────────┴──────────┴──────────┴─────────┘")

			// NEW: Show level counts to verify full depth
			fmt.Println()
			fmt.Println("═══════════════════════════════════════════════════════════════════")
			fmt.Println("📊 LIQUIDITY DEPTH VERIFICATION (are we seeing full book?)")
			fmt.Println("═══════════════════════════════════════════════════════════════════")
			for _, outcome := range outcomes {
				fmt.Printf("\n   %s:\n", outcome)
				if ws, ok := wsData[outcome]; ok {
					fmt.Printf("      WS:   %d bid levels (%.0f total) | %d ask levels (%.0f total)\n",
						len(ws.BidDepth), ws.TotalBidLiq, len(ws.AskDepth), ws.TotalAskLiq)
				} else {
					fmt.Printf("      WS:   (no data)\n")
				}
				if rest, ok := restData[outcome]; ok {
					fmt.Printf("      REST: %d bid levels (%.0f total) | %d ask levels (%.0f total)\n",
						len(rest.BidDepth), rest.TotalBidLiq, len(rest.AskDepth), rest.TotalAskLiq)
				} else {
					fmt.Printf("      REST: (no data)\n")
				}
				// Compare
				if ws, ok1 := wsData[outcome]; ok1 {
					if rest, ok2 := restData[outcome]; ok2 {
						bidLevelDiff := len(rest.BidDepth) - len(ws.BidDepth)
						askLevelDiff := len(rest.AskDepth) - len(ws.AskDepth)
						bidSizeDiff := rest.TotalBidLiq - ws.TotalBidLiq
						askSizeDiff := rest.TotalAskLiq - ws.TotalAskLiq
						if bidLevelDiff != 0 || askLevelDiff != 0 {
							fmt.Printf("      \033[33m⚠️  Level diff: REST has %+d bid, %+d ask levels vs WS\033[0m\n", bidLevelDiff, askLevelDiff)
						}
						if abs(bidSizeDiff) > 1 || abs(askSizeDiff) > 1 {
							fmt.Printf("      \033[33m⚠️  Size diff: REST has %+.0f bid, %+.0f ask shares vs WS\033[0m\n", bidSizeDiff, askSizeDiff)
						}
						if bidLevelDiff == 0 && askLevelDiff == 0 && abs(bidSizeDiff) <= 1 && abs(askSizeDiff) <= 1 {
							fmt.Printf("      \033[32m✓ WS and REST match!\033[0m\n")
						}
					}
				}
			}

			// Calculate arb opportunity
			if len(outcomes) == 2 {
				fmt.Println()
				fmt.Println("═══════════════════════════════════════════════════════════════════")
				fmt.Println("📈 ARB ANALYSIS")
				fmt.Println("═══════════════════════════════════════════════════════════════════")

				// Using WS data
				if ws0, ok0 := wsData[outcomes[0]]; ok0 {
					if ws1, ok1 := wsData[outcomes[1]]; ok1 {
						sum := ws0.BestAsk + ws1.BestAsk
						margin := (1.0 - sum) * 100
						marginColor := "\033[31m" // Red
						if margin >= 2.0 {
							marginColor = "\033[32m" // Green
						} else if margin >= 1.0 {
							marginColor = "\033[33m" // Yellow
						}

						// Calculate matched liquidity
						minLiq := ws0.TotalAskLiq
						if ws1.TotalAskLiq < minLiq {
							minLiq = ws1.TotalAskLiq
						}
						safeShares := minLiq * 0.80

						fmt.Printf("   WebSocket:  Sum=$%.3f | %sMargin=%.2f%%%s | Matched Liq=%.0f | Safe Shares=%.0f\n",
							sum, marginColor, margin, "\033[0m", minLiq, safeShares)

						// Show what bot would trade
						if margin >= 2.0 && safeShares >= 1 {
							tradeSize := 50.0 // Base trade size
							baseShares := tradeSize / sum
							scaledShares := baseShares * 2 // 2x at 2% margin
							if scaledShares > safeShares {
								scaledShares = safeShares
							}
							cost := scaledShares * sum
							profit := scaledShares * (1.0 - sum)
							orderCost := cost * 0.01
							netProfit := profit - orderCost

							fmt.Printf("   Bot would: %.0f shares @ $%.2f cost = $%.2f profit (net: $%.2f after 1%% cost)\n",
								scaledShares, cost, profit, netProfit)
						}

						// Show depth analysis
						fmt.Println()
						fmt.Println("   📊 ASK DEPTH (liquidity available to buy):")
						for i, outcome := range outcomes {
							ws := wsData[outcome]
							if ws == nil {
								continue
							}

							// Sort asks by price ascending
							asks := make([]api.PriceLevel, len(ws.AskDepth))
							copy(asks, ws.AskDepth)
							sort.Slice(asks, func(a, b int) bool {
								pa, _ := strconv.ParseFloat(asks[a].Price, 64)
								pb, _ := strconv.ParseFloat(asks[b].Price, 64)
								return pa < pb
							})

							fmt.Printf("   %s:\n", outcome)
							cumLiq := 0.0
							for j, lvl := range asks {
								if j >= 5 {
									break
								}
								p, _ := strconv.ParseFloat(lvl.Price, 64)
								s, _ := strconv.ParseFloat(lvl.Size, 64)
								cumLiq += s
								fmt.Printf("      $%.3f × %.0f (cum: %.0f)\n", p, s, cumLiq)
							}
							if i == 0 {
								fmt.Println()
							}
						}
					}
				}
			}

			fmt.Println()
			fmt.Println("Press Ctrl+C to exit...")
		}
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
