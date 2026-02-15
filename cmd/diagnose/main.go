package main

import (
	"context"
	"fmt"
	"log"
	"math/big"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/trading"
	"github.com/joho/godotenv"
)

func main() {
	godotenv.Load()
	cfg, err := core.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	trader, err := trading.NewRealTrader(cfg)
	if err != nil {
		log.Fatalf("Failed to create trader: %v", err)
	}

	ctx := context.Background()
	rest := api.NewRestClient("")
	polygon := api.NewPolygonClient(cfg.PolygonRPCURL)
	address := trader.Address()

	fmt.Println("🩺 POLYARB WALLET DIAGNOSTIC")
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Printf("🔑 Wallet:  %s\n", address)

	// 1. Check Native & Collateral Balances
	matic, _ := polygon.GetMATICBalance(ctx, address)
	usdc, _ := polygon.GetUSDCBalance(ctx, address)
	fmt.Printf("💰 MATIC:   %.4f\n", matic)
	fmt.Printf("💰 USDC:    $%.2f\n", usdc)

	// 2. Check Permissions
	allowance, _ := polygon.GetUSDCAllowance(ctx, address, api.CTFContract)
	ctfApproved, _ := polygon.IsCTFApproved(ctx, address, api.CTFContract)
	
	fmt.Printf("🔓 USDC Allowance: %s\n", allowance.String())
	fmt.Printf("🔓 CTF Approved:  %v\n", ctfApproved)

	// 3. Smart Scan for Tokens
	fmt.Println("\n🔍 Scanning for tokens in recent 15m markets...")
	markets, err := rest.Get15mMarkets(ctx, nil)
	if err != nil {
		fmt.Printf("⚠️ Could not scan markets: %v\n", err)
	}

	foundTokens := false
	processed := make(map[string]bool)

	for _, m := range markets {
		if processed[m.ConditionID] {
			continue
		}
		processed[m.ConditionID] = true

		for _, t := range m.Tokens {
			tokenBig := new(big.Int)
			tokenBig.SetString(t.TokenID, 10)
			
			bal, err := polygon.GetCTFBalance(ctx, address, tokenBig)
			if err == nil && bal.Cmp(big.NewInt(0)) > 0 {
				shares := new(big.Float).SetInt(bal)
				shares = shares.Quo(shares, big.NewFloat(1e6))
				s, _ := shares.Float64()
				
				if s >= 0.01 {
					if !foundTokens {
						fmt.Println("📦 Detected Token Balances:")
						foundTokens = true
					}
					fmt.Printf("   • %-10s: %.6f shares (%s)\n", m.Slug[:10]+"...", s, t.Outcome)
				}
			}
		}
	}

	if !foundTokens {
		fmt.Println("✅ No conditional tokens detected in recent markets.")
	}
	fmt.Println("\n═══════════════════════════════════════════════════════")
}
