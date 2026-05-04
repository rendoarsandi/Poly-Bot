package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"math/big"
	"os"
	"strings"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/trading"
	"github.com/charmbracelet/lipgloss"
	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load()
	cfg, err := core.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	trader, err := trading.NewRealTrader(cfg)
	if err != nil {
		log.Fatalf("Failed to create trader: %v", err)
	}

	ctx := context.Background()
	rest := api.NewRestClient(cfg.Exchange)
	polygon := api.NewPolygonClient(cfg.PolygonRPCURL)
	address := trader.Address()

	titleSt := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7C3AED"))
	dimSt := lipgloss.NewStyle().Foreground(lipgloss.Color("#6B7280"))
	fmt.Println(lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#7C3AED")).
		Padding(0, 2).
		Render(titleSt.Render("🩺  POLYARB WALLET DIAGNOSTIC") + "\n" +
			dimSt.Render("Checks balances, permissions, and on-chain token holdings")))
	fmt.Println(dimSt.Render("  🔑 Wallet:  " + address))

	// 1. Check Native & Collateral Balances
	matic, _ := polygon.GetMATICBalance(ctx, address)
	usdc, _ := polygon.GetUSDCBalance(ctx, address)
	fmt.Printf("💰 MATIC:   %.4f\n", matic)
	fmt.Printf("💰 pUSD:    $%.2f\n", usdc)

	// 2. Check Permissions
	fmt.Println("\n🔐 Permission Analysis:")

	allGood := true
	printPerms := func(name, contract string) {
		allowance, errAllow := polygon.GetUSDCAllowance(ctx, address, contract)
		ctfApproved, errApprove := polygon.IsCTFApproved(ctx, address, contract)

		usdcIcon := "✅"
		var allowanceStr string

		// Check pUSD allowance
		if errAllow != nil {
			usdcIcon = "⚠️"
			allowanceStr = fmt.Sprintf("(%v)", errAllow)
			allGood = false
		} else if allowance.Cmp(big.NewInt(0)) == 0 {
			usdcIcon = "❌"
			allowanceStr = allowance.String()
			allGood = false
		} else {
			allowanceStr = allowance.String()
		}

		// Check CTF Operator (Skip for CTF Contract)
		ctfIcon := "✅"
		ctfStr := fmt.Sprintf("%v", ctfApproved)

		if name == "CTF Contract" {
			ctfIcon = "⚪"
			ctfStr = "N/A"
		} else {
			if errApprove != nil {
				ctfIcon = "⚠️"
				ctfStr = fmt.Sprintf("(%v)", errApprove)
				allGood = false
			} else if !ctfApproved {
				ctfIcon = "❌"
				allGood = false
			}
		}

		fmt.Printf("   • %-20s\n", name)
		fmt.Printf("     pUSD Allowance: %s %s\n", usdcIcon, allowanceStr)
		if name != "CTF Contract" {
			fmt.Printf("     CTF Operator:   %s %s\n", ctfIcon, ctfStr)
		}
	}

	printPerms("CTF Contract", api.CTFContract)
	printPerms("Exchange", api.CTFExchange)
	printPerms("NegRisk Exchange", api.NegRiskExchange)
	printPerms("CTF Adapter", api.CtfCollateralAdapter)
	printPerms("NegRisk CTF Adapter", api.NegRiskCtfCollateralAdapter)

	// 3. Smart Scan for Tokens
	fmt.Println("\n🔍 Scanning for tokens in recent 15m markets...")
	markets, err := rest.GetMarketsByTimeframe(ctx, nil, "15m")
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

	// 4. Auto-Approval Prompt
	if allGood {
		fmt.Println("\n✅ All system permissions are correct. No action needed.")
	} else {
		fmt.Println("\n🛠️  PERMISSION REPAIR")
		fmt.Print("Would you like to attempt auto-fixing missing permissions? (y/N): ")
		reader := bufio.NewReader(os.Stdin)
		text, _ := reader.ReadString('\n')
		if strings.TrimSpace(strings.ToLower(text)) == "y" {
			fmt.Println("🚀 Starting approval process... (this may take a minute)")
			if _, err := trader.ApproveTrading(ctx); err != nil {
				fmt.Printf("❌ Approval failed: %v\n", err)
			} else {
				fmt.Println("✅ All permissions verified/updated.")
			}
		} else {
			fmt.Println("No changes made.")
		}
	}
}
