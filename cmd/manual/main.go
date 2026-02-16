package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"math/big"
	"os"
	"strconv"
	"strings"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/trading"
	"github.com/joho/godotenv"
)

// OnChainPosition represents a holding found on-chain
type OnChainPosition struct {
	TokenID     string
	Outcome     string
	Size        float64
	ConditionID string // Needed for split/merge
	Slug        string
}

func main() {
	godotenv.Load()
	cfg, err := core.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Always use real trader for this manual tool
	trader, err := trading.NewRealTrader(cfg)
	if err != nil {
		log.Fatalf("Failed to create trader: %v", err)
	}

	client := api.NewRestClient("")
	polygon := api.NewPolygonClient(cfg.PolygonRPCURL)
	ctx := context.Background()
	address := trader.Address()

	fmt.Println("╔═══════════════════════════════════════════════════════╗")
	fmt.Println("║           MANUAL POSITION MANAGER                     ║")
	fmt.Println("╚═══════════════════════════════════════════════════════╝")
	fmt.Printf("🔑 Wallet: %s\n", address)

	// 1. Scan for markets and On-Chain Positions
	fmt.Println("🔄 Scanning blockchain for positions (BTC, ETH, SOL, XRP)...")
	assets := []string{"btc", "eth", "sol", "xrp"}
	markets, err := client.Get15mMarkets(ctx, assets)
	if err != nil {
		log.Fatalf("Failed to fetch markets: %v", err)
	}

	var positions []OnChainPosition

	for _, m := range markets {
		for _, t := range m.Tokens {
			tid := new(big.Int)
			tid.SetString(t.TokenID, 10)

			// Query on-chain balance
			bal, err := polygon.GetCTFBalance(ctx, address, tid)
			if err != nil {
				continue
			}

			// Convert to float shares
			shares := new(big.Float).SetInt(bal)
			shares = shares.Quo(shares, big.NewFloat(1e6))
			s, _ := shares.Float64()

			if s > 0.0001 { // Filter dust
				positions = append(positions, OnChainPosition{
					TokenID:     t.TokenID,
					Outcome:     core.SanitizeString(t.Outcome),
					Size:        s,
					ConditionID: m.ConditionID,
					Slug:        m.Slug,
				})
			}
		}
	}

	if len(positions) == 0 {
		fmt.Println("✅ No on-chain positions found.")
		return
	}

	fmt.Printf("\nFound %d position(s):\n", len(positions))
	fmt.Println("   # | Market (Outcome)             | Size")
	fmt.Println("-----+------------------------------+--------")

	for i, pos := range positions {
		name := fmt.Sprintf("%s (%s)", pos.Slug, pos.Outcome)
		if len(name) > 28 {
			name = name[:25] + "..."
		}
		fmt.Printf("   %d | %-28s | %-6.1f\n", i+1, name, pos.Size)
	}
	fmt.Println("-----+------------------------------+--------")

	// 2. Select Position
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("\nSelect position # to manage (or '0' to exit): ")
	text, _ := reader.ReadString('\n')
	choiceStr := strings.TrimSpace(text)
	choice, err := strconv.Atoi(choiceStr)
	if err != nil || choice < 1 || choice > len(positions) {
		if choiceStr == "0" {
			return
		}
		log.Fatal("Invalid selection.")
	}

	selectedPos := positions[choice-1]
	fmt.Printf("\nSelected: %s - %s (Size: %.1f)\n", selectedPos.Slug, selectedPos.Outcome, selectedPos.Size)

	// 3. Choose Action
	fmt.Println("\nActions:")
	fmt.Println("  1. SELL current position (Dump stuck shares)")
	fmt.Println("  2. BUY more of current position")
	fmt.Println("  3. BUY OPPOSITE (Attempt to balance for merge)")
	fmt.Print("Choice: ")

	text, _ = reader.ReadString('\n')
	actionStr := strings.TrimSpace(text)
	actionChoice, _ := strconv.Atoi(actionStr)

	var targetTokenID string
	var targetOutcome string
	var executeSide string // "BUY" or "SELL" for execution

	switch actionChoice {
	case 1:
		targetTokenID = selectedPos.TokenID
		targetOutcome = selectedPos.Outcome
		executeSide = "SELL"
	case 2:
		targetTokenID = selectedPos.TokenID
		targetOutcome = selectedPos.Outcome
		executeSide = "BUY"
	case 3:
		fmt.Println("🔍 Finding opposite token...")

		// We already have the market info from the initial scan!
		// We just need to find the market in the `markets` list that matches selectedPos.ConditionID
		var foundMarket *api.Market
		for _, m := range markets {
			if m.ConditionID == selectedPos.ConditionID {
				mCopy := m
				foundMarket = &mCopy
				break
			}
		}

		if foundMarket == nil {
			log.Fatal("❌ Could not find market info. Cannot auto-balance.")
		}

		// Find the opposite token in this market
		for _, t := range foundMarket.Tokens {
			if t.TokenID != selectedPos.TokenID {
				targetTokenID = t.TokenID
				targetOutcome = t.Outcome
				break
			}
		}

		if targetTokenID == "" {
			log.Fatal("❌ Could not determine opposite token.")
		}

		targetOutcome = core.SanitizeString(targetOutcome)
		fmt.Printf("🎯 Found opposite: %s (%s)\n", targetOutcome, targetTokenID)
		executeSide = "BUY"

	default:
		log.Fatal("Invalid action.")
	}

	// 4. Specify Amount and Price
	defaultSize := selectedPos.Size
	fmt.Printf("\n%s %s (Token: %s)\n", executeSide, targetOutcome, targetTokenID)
	fmt.Printf("Enter shares (default %.1f): ", defaultSize)

	text, _ = reader.ReadString('\n')
	sizeStr := strings.TrimSpace(text)
	size := defaultSize
	if sizeStr != "" {
		if s, err := strconv.ParseFloat(sizeStr, 64); err == nil {
			size = s
		}
	}

	// Price logic
	var price float64
	if executeSide == "BUY" {
		fmt.Print("Enter limit price (default 0.99 for aggressive buy): ")
		text, _ = reader.ReadString('\n')
		priceStr := strings.TrimSpace(text)
		price = 0.99
		if priceStr != "" {
			if p, err := strconv.ParseFloat(priceStr, 64); err == nil {
				price = p
			}
		}
	} else {
		// SELL
		fmt.Print("Enter limit price (default 0.01 for aggressive dump): ")
		text, _ = reader.ReadString('\n')
		priceStr := strings.TrimSpace(text)
		price = 0.01
		if priceStr != "" {
			if p, err := strconv.ParseFloat(priceStr, 64); err == nil {
				price = p
			}
		}
	}

	// 5. Confirm and Execute
	totalVal := price * size
	fmt.Printf("\n🚨 READY TO EXECUTE: %s %.1f %s @ $%.2f (Total: $%.2f)\n", executeSide, size, targetOutcome, price, totalVal)
	fmt.Print("Type 'YES' to confirm: ")
	text, _ = reader.ReadString('\n')
	input := strings.TrimSpace(strings.ToUpper(text))
	if input != "YES" && input != "Y" {
		fmt.Println("Cancelled.")
		return
	}

	fmt.Println("🚀 Sending order...")

	// Fetch fee rate
	rate, _ := client.GetFeeRate(ctx, targetTokenID)
	if rate == 0 {
		rate = 1000 // Safe default for 15m
	}

	var res *trading.TradeResult
	if executeSide == "BUY" {
		res, err = trader.Buy(ctx, targetTokenID, targetOutcome, price, size, api.OrderTypeMarket, api.TIFFillOrKill, rate)
	} else {
		res, err = trader.Sell(ctx, targetTokenID, targetOutcome, price, size, api.OrderTypeMarket, api.TIFFillOrKill, rate)
	}

	if err != nil {
		fmt.Printf("❌ Execution Error: %v\n", err)
	} else if res != nil && !res.Success {
		fmt.Printf("❌ Order Failed: %s (Status: %s)\n", res.Message, res.Status)
	} else if res != nil {
		fmt.Printf("✅ Success! OrderID: %s\n", res.OrderID)
	} else {
		fmt.Println("❌ Unknown error (nil result)")
	}
}
