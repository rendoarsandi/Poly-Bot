package main

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"os"
	"os/signal"
	"strings"
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

	// 2b. Check API Authentication
	fmt.Print("🔑 Checking API Authentication... ")
	authSuccess := false
	allowance, err := clob.GetBalanceAllowance(ctx)
	if err != nil {
		fmt.Printf("❌ FAILED: %v\n", err)
	} else {
		fmt.Printf("✅ SUCCESS (API Key & Signer verified)\n")
		authSuccess = true
		_ = allowance // Use to avoid unused warning if needed, but it's fine
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
	if !authSuccess {
		fmt.Println("❌ AUTH FAILURE: Your API Key, Secret, or Passphrase might be incorrect.")
		ready = false
	}
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
		if !authSuccess {
			fmt.Println("🛠️  ACTION REQUIRED: Fix your .env credentials before continuing.")
		} else if maticBal < 0.01 {
			fmt.Println("🛠️  ACTION REQUIRED: Send MATIC to your wallet for gas fees.")
		} else {
			fmt.Println("\n🛠️  ACTION SUGGESTED: Would you like to fix these permissions now? (y/n)")
			fmt.Print("> ")
			var input string
			fmt.Scanln(&input)
			if strings.ToLower(strings.TrimSpace(input)) == "y" {
				fixPermissions(ctx, polygon, address, cfg.PK)
			}
		}
	}

	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Println("Press Ctrl+C or wait 5s to exit.")

	select {
	case <-ctx.Done():
		fmt.Println("\nExiting diagnostic...")
	case <-time.After(5 * time.Second):
		fmt.Println("\nDiagnostic complete.")
	}
}

func fixPermissions(ctx context.Context, polygon *api.PolygonClient, address string, pk string) {
	fmt.Println("\n🚀 Starting Permission Repair...")
	signer, err := api.NewSigner(pk)
	if err != nil {
		fmt.Printf("❌ Failed to create signer: %v\n", err)
		return
	}

	maxUint := new(big.Int).Exp(big.NewInt(2), big.NewInt(256), nil)
	maxUint.Sub(maxUint, big.NewInt(1))

	const EXCHANGE_ADDR = "0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E"

	// 1. Approve Exchange for USDC
	fmt.Printf("📝 [1/3] Approving Exchange for USDC...\n")
	tx1, err := polygon.ApproveUSDC(ctx, signer, EXCHANGE_ADDR, maxUint)
	if err != nil {
		fmt.Printf("⚠️  Failed: %v\n", err)
	} else {
		fmt.Printf("✅ Sent! Hash: %s\n", tx1)
		polygon.WaitForTransaction(ctx, tx1)
	}

	// 2. Approve CTF Contract for USDC
	fmt.Printf("📝 [2/3] Approving CTF Contract for USDC (for Splitting)...\n")
	tx2, err := polygon.ApproveUSDC(ctx, signer, api.CTFContract, maxUint)
	if err != nil {
		fmt.Printf("⚠️  Failed: %v\n", err)
	} else {
		fmt.Printf("✅ Sent! Hash: %s\n", tx2)
		polygon.WaitForTransaction(ctx, tx2)
	}

	// 3. Approve Exchange for CTF Tokens
	fmt.Printf("📝 [3/3] Approving Exchange for CTF Tokens (for Selling)...\n")
	tx3, err := polygon.ApproveCTF(ctx, signer, EXCHANGE_ADDR, true)
	if err != nil {
		fmt.Printf("⚠️  Failed: %v\n", err)
	} else {
		fmt.Printf("✅ Sent! Hash: %s\n", tx3)
		polygon.WaitForTransaction(ctx, tx3)
	}

	fmt.Println("\n🎉 ALL APPROVALS COMPLETE. Please re-run diagnostics to verify.")
}

