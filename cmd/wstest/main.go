package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"Market-bot/internal/api"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Println("╔═══════════════════════════════════════════════════════════════╗")
	fmt.Println("║     WS vs REST LATENCY TEST                                   ║")
	fmt.Println("║     Detecting if WS is missing updates or market is quiet    ║")
	fmt.Println("╚═══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	restClient := api.NewRestClient("")

	// Find active market
	fmt.Println("🔍 Finding active 15m market...")
	markets, err := restClient.Get15mMarkets(ctx, nil)
	if err != nil || len(markets) == 0 {
		fmt.Printf("Error: %v\n", err)
		return
	}

	market := markets[0]
	fmt.Printf("📊 Market: %s\n\n", market.Slug)

	// Build token map
	tokenMap := make(map[string]string)
	tokenIDs := make([]string, 0)
	for _, t := range market.Tokens {
		tokenMap[t.TokenID] = t.Outcome
		tokenIDs = append(tokenIDs, t.TokenID)
	}

	// Connect WebSocket
	fmt.Println("📡 Connecting WebSocket...")
	wsMgr := api.NewWSManager("")
	if err := wsMgr.Connect(ctx); err != nil {
		fmt.Printf("WS connect failed: %v\n", err)
		return
	}
	defer wsMgr.Close()

	// Subscribe
	sub := map[string]interface{}{
		"type":       "market",
		"assets_ids": tokenIDs,
	}
	if err := wsMgr.Subscribe(ctx, sub); err != nil {
		fmt.Printf("Subscribe failed: %v\n", err)
		return
	}

	wsChan := wsMgr.StartStreaming(ctx)
	fmt.Println("✅ Connected")

	// Tracking state
	type PriceState struct {
		BestAsk float64
		BestBid float64
		AskLiq  float64 // Liquidity at best ask only
	}

	lastRESTState := make(map[string]*PriceState)
	lastWSState := make(map[string]*PriceState)

	restChanges := 0
	wsMessages := 0
	restPolls := 0
	wsLagEvents := 0 // Times when REST changed but no WS message

	// Track timing
	lastWSMessage := time.Now()
	lastRESTChange := time.Now()

	// Poll REST every 500ms
	restTicker := time.NewTicker(500 * time.Millisecond)
	defer restTicker.Stop()

	// Print summary every 5 seconds
	summaryTicker := time.NewTicker(5 * time.Second)
	defer summaryTicker.Stop()

	fmt.Println("Time       | Event      | Up Ask   | Up Liq | Down Ask | Down Liq | Notes")
	fmt.Println("-----------|------------|----------|--------|----------|----------|------------------")

	startTime := time.Now()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("\n\n═══════════════════════════════════════════════════════════════")
			fmt.Println("FINAL SUMMARY")
			fmt.Println("═══════════════════════════════════════════════════════════════")
			elapsed := time.Since(startTime)
			fmt.Printf("Duration: %v\n", elapsed.Round(time.Second))
			fmt.Printf("REST polls: %d (every 500ms)\n", restPolls)
			fmt.Printf("REST price changes detected: %d\n", restChanges)
			fmt.Printf("WS messages received: %d\n", wsMessages)
			fmt.Printf("WS lag events (REST changed, no WS): %d\n", wsLagEvents)
			if restChanges > 0 {
				missRate := float64(wsLagEvents) / float64(restChanges) * 100
				fmt.Printf("\n⚠️  WS MISS RATE: %.1f%%\n", missRate)
				if missRate > 50 {
					fmt.Println("   → WS is significantly lagging behind REST!")
					fmt.Println("   → Recommend using REST as primary data source")
				} else if missRate > 20 {
					fmt.Println("   → WS has occasional delays")
					fmt.Println("   → REST fallback is helpful")
				} else {
					fmt.Println("   → WS is keeping up reasonably well")
				}
			}
			return

		case msg, ok := <-wsChan:
			if !ok {
				continue
			}

			books, err := api.ParseOrderBooks(msg)
			if err != nil || len(books) == 0 {
				continue
			}

			wsMessages++
			now := time.Now()
			timeSinceLastWS := now.Sub(lastWSMessage)
			lastWSMessage = now

			for _, b := range books {
				outcome := tokenMap[b.AssetID]
				if outcome == "" {
					continue
				}

				bestAsk := 1.0
				askLiq := 0.0
				bestBid := 0.0

				for _, order := range b.Asks {
					p, _ := strconv.ParseFloat(order.Price, 64)
					s, _ := strconv.ParseFloat(order.Size, 64)
					if p < bestAsk && p > 0 {
						bestAsk = p
						askLiq = s // Liquidity at best ask only
					}
				}
				for _, order := range b.Bids {
					p, _ := strconv.ParseFloat(order.Price, 64)
					if p > bestBid {
						bestBid = p
					}
				}
				if bestAsk >= 1.0 {
					bestAsk = 0
					askLiq = 0
				}

				// Check if changed from last WS state
				changed := false
				if last, ok := lastWSState[outcome]; ok {
					if last.BestAsk != bestAsk || last.AskLiq != askLiq {
						changed = true
					}
				} else {
					changed = true
				}

				lastWSState[outcome] = &PriceState{BestAsk: bestAsk, BestBid: bestBid, AskLiq: askLiq}

				if changed {
					elapsed := time.Since(startTime)
					note := ""
					if timeSinceLastWS > 5*time.Second {
						note = fmt.Sprintf("(gap: %.1fs)", timeSinceLastWS.Seconds())
					}
					fmt.Printf("%10s | \033[32mWS\033[0m         | $%.3f   | %6.0f | ",
						elapsed.Round(time.Millisecond), bestAsk, askLiq)

					// Print other outcome if we have it
					for otherOutcome, otherState := range lastWSState {
						if otherOutcome != outcome {
							fmt.Printf("$%.3f   | %6.0f | %s\n", otherState.BestAsk, otherState.AskLiq, note)
							break
						}
					}
					if len(lastWSState) == 1 {
						fmt.Printf("   -     |    -   | %s\n", note)
					}
				}
			}

		case <-restTicker.C:
			restPolls++
			changed := false

			for tokenID, outcome := range tokenMap {
				book, err := restClient.GetOrderBook(ctx, tokenID)
				if err != nil {
					continue
				}

				bestAsk := 1.0
				askLiq := 0.0
				bestBid := 0.0

				// Find best ask and its liquidity
				for _, order := range book.Asks {
					p, _ := strconv.ParseFloat(order.Price, 64)
					s, _ := strconv.ParseFloat(order.Size, 64)
					if p < bestAsk && p > 0 {
						bestAsk = p
						askLiq = s
					}
				}
				for _, order := range book.Bids {
					p, _ := strconv.ParseFloat(order.Price, 64)
					if p > bestBid {
						bestBid = p
					}
				}
				if bestAsk >= 1.0 {
					bestAsk = 0
					askLiq = 0
				}

				// Check if changed
				if last, ok := lastRESTState[outcome]; ok {
					// Check for meaningful change (price or significant liq change)
					if last.BestAsk != bestAsk ||
					   (last.AskLiq > 0 && absFloat(last.AskLiq-askLiq)/last.AskLiq > 0.05) {
						changed = true

						// Check if WS had this update
						if ws, wsOk := lastWSState[outcome]; wsOk {
							if ws.BestAsk != bestAsk {
								wsLagEvents++
							}
						}
					}
				}

				lastRESTState[outcome] = &PriceState{BestAsk: bestAsk, BestBid: bestBid, AskLiq: askLiq}
			}

			if changed {
				restChanges++
				now := time.Now()
				timeSinceLastChange := now.Sub(lastRESTChange)
				lastRESTChange = now

				elapsed := time.Since(startTime)

				// Get current REST state
				upState := lastRESTState["Up"]
				downState := lastRESTState["Down"]

				if upState == nil {
					upState = &PriceState{}
				}
				if downState == nil {
					downState = &PriceState{}
				}

				note := ""
				if timeSinceLastChange < 1*time.Second {
					note = "FAST"
				}

				// Check if WS is behind
				wsBehind := false
				if ws, ok := lastWSState["Up"]; ok && upState.BestAsk > 0 {
					if ws.BestAsk != upState.BestAsk {
						wsBehind = true
					}
				}
				if ws, ok := lastWSState["Down"]; ok && downState.BestAsk > 0 {
					if ws.BestAsk != downState.BestAsk {
						wsBehind = true
					}
				}
				if wsBehind {
					note += " \033[31mWS BEHIND!\033[0m"
				}

				fmt.Printf("%10s | \033[33mREST\033[0m       | $%.3f   | %6.0f | $%.3f   | %6.0f | %s\n",
					elapsed.Round(time.Millisecond),
					upState.BestAsk, upState.AskLiq,
					downState.BestAsk, downState.AskLiq,
					note)
			}

		case <-summaryTicker.C:
			elapsed := time.Since(startTime)
			fmt.Printf("\n--- %.0fs SUMMARY: REST changes=%d, WS msgs=%d, WS lag=%d ---\n\n",
				elapsed.Seconds(), restChanges, wsMessages, wsLagEvents)
		}
	}
}

func absFloat(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
