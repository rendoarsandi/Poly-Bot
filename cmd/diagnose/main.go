package main

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"os"
	"os/signal"
	"syscall"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
)

func main() {
	// Setup signal handling for graceful exit
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := core.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	polygon := api.NewPolygonClient(cfg.PolygonRPCURL)
	clob, _ := api.NewCLOBClient(cfg.PK, cfg.APIKey, cfg.APISecret, cfg.APIPassphrase)
	address := clob.Address()

	fmt.Println("🔍 POLYARB SYSTEM DIAGNOSTICS")
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Printf("🔑 Wallet: %s\n", address)

	// Constants for Polymarket
	const (
		EXCHANGE_ADDR = "0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E" // CLOB Exchange
	)

	// 1. Check USDC Balance
	fmt.Print("💵 Checking USDC Balance... ")
	usdcBal, err := polygon.GetUSDCBalance(ctx, address)
	if err != nil {
		fmt.Printf("❌ Error: %v\n", err)
	} else {
		fmt.Printf("✅ $%.2f USDC\n", usdcBal)
	}

	// 2. Check MATIC Balance (Gas)
	fmt.Print("⛽ Checking MATIC Balance... ")
	maticBal, err := polygon.GetMATICBalance(ctx, address)
	if err != nil {
		fmt.Printf("❌ Error: %v\n", err)
	} else {
		fmt.Printf("✅ %.4f MATIC\n", maticBal)
	}

	fmt.Println("\n🛡️  ALLOWANCE CHECK (Permissions)")
	fmt.Println("───────────────────────────────────────────────────────")

	// 3. Check Allowance for EXCHANGE (Buying/Selling)
	fmt.Print("🛒 Exchange (USDC): ")
	allowanceExchange, err := polygon.GetUSDCAllowance(ctx, address, EXCHANGE_ADDR)
	if err != nil {
		fmt.Printf("❌ Error: %v\n", err)
	} else {
		if allowanceExchange.Cmp(big.NewInt(1000000000)) > 0 {
			fmt.Println("✅ UNLIMITED (Ready for trading)")
		} else {
			val := new(big.Float).SetInt(allowanceExchange)
			val = val.Quo(val, big.NewFloat(1e6))
			fmt.Printf("⚠️  Limited ($%.2f)\n", val)
		}
	}

	// 4. Check Allowance for CTF (Splitting USDC)
	fmt.Print("🔀 CTF Split (USDC): ")
	allowanceCTFUSDC, err := polygon.GetUSDCAllowance(ctx, address, api.CTFContract)
	if err != nil {
		fmt.Printf("❌ Error: %v\n", err)
	} else {
		if allowanceCTFUSDC.Cmp(big.NewInt(1000000000)) > 0 {
			fmt.Println("✅ UNLIMITED (Ready for Split)")
		} else {
			val := new(big.Float).SetInt(allowanceCTFUSDC)
			val = val.Quo(val, big.NewFloat(1e6))
			if val.Cmp(big.NewFloat(0)) == 0 {
				fmt.Printf("❌ NOT APPROVED ($0.00) - THIS CAUSED YOUR ERROR\n")
			} else {
				fmt.Printf("⚠️  Limited ($%.2f)\n", val)
			}
		}
	}

	// 5. Check Approval for CTF Tokens (Merging/Selling)
	fmt.Print("🔄 CTF Tokens Approval: ")
	approvedCTF, err := polygon.IsCTFApproved(ctx, address, EXCHANGE_ADDR)
	if err != nil {
		fmt.Printf("❌ Error: %v\n", err)
	} else {
		if approvedCTF {
			fmt.Println("✅ APPROVED (Ready for Merge/Sell)")
		} else {
			fmt.Println("❌ NOT APPROVED - Selling might fail")
		}
	}

	fmt.Println("\n📋 CONCLUSION:")
	ready := true
	if maticBal < 0.05 {
		fmt.Println("⚠️  Warning: Extremely low MATIC. Transactions WILL fail.")
		ready = false
	}
	if allowanceCTFUSDC.Cmp(big.NewInt(1000000)) < 0 {
		fmt.Println("🚫 ACTION REQUIRED: You must approve the CTF contract to SPLIT.")
		ready = false
	}
	
	if ready {
		fmt.Println("🚀 All systems ready. No permission issues detected.")
	} else {
		fmt.Println("🛠️  Run: go run cmd/approve/main.go to fix permissions.")
	}

	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Println("Press Ctrl+C or wait 10s to exit.")

	select {
	case <-ctx.Done():
		fmt.Println("\nExiting diagnostic...")
	case <-time.After(10 * time.Second):
		fmt.Println("\nDiagnostic complete.")
	}
}
