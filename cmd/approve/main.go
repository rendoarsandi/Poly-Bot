package main

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"github.com/joho/godotenv"
)

func main() {
	// Load environment variables
	godotenv.Load()

	cfg, err := core.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Polymarket Exchange contract address (The spender for trading)
	const ExchangeContract = "0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E"
	// CTF Contract (The spender for Splitting/Merging)
	// api.CTFContract is defined in internal/api/polygon.go

	fmt.Println("🚀 Initializing Enhanced Approval Script...")
	
	// Create signer
	signer, err := api.NewSigner(cfg.PK)
	if err != nil {
		log.Fatalf("Failed to create signer: %v", err)
	}
	fmt.Printf("🔑 Wallet Address: %s\n", signer.Address())

	// Create Polygon client
	polygon := api.NewPolygonClient(cfg.PolygonRPCURL)

	// Context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	// 1. Check current MATIC balance for gas
	matic, err := polygon.GetMATICBalance(ctx, signer.Address())
	if err != nil {
		log.Fatalf("Failed to check MATIC balance: %v", err)
	}
	fmt.Printf("⛽ MATIC Balance: %.4f\n", matic)

	// Max approval amount
	maxUint := new(big.Int).Exp(big.NewInt(2), big.NewInt(256), nil)
	maxUint.Sub(maxUint, big.NewInt(1))

	// 2. Approve Exchange for USDC (For Orderbook Trading)
	fmt.Printf("\n📝 [1/3] Approving Exchange for USDC...\n")
	tx1, err := polygon.ApproveUSDC(ctx, signer, ExchangeContract, maxUint)
	if err != nil {
		fmt.Printf("⚠️  Failed: %v\n", err)
	} else {
		fmt.Printf("✅ Sent! Hash: %s\n", tx1)
		fmt.Println("⏳ Waiting for confirmation...")
		polygon.WaitForTransaction(ctx, tx1)
	}

	time.Sleep(2 * time.Second)

	// 3. Approve CTF Contract for USDC (REQUIRED FOR SPLIT)
	fmt.Printf("\n📝 [2/3] Approving CTF Contract for USDC (for Splitting)...\n")
	tx2, err := polygon.ApproveUSDC(ctx, signer, api.CTFContract, maxUint)
	if err != nil {
		fmt.Printf("⚠️  Failed: %v\n", err)
	} else {
		fmt.Printf("✅ Sent! Hash: %s\n", tx2)
		fmt.Println("⏳ Waiting for confirmation...")
		polygon.WaitForTransaction(ctx, tx2)
	}

	time.Sleep(2 * time.Second)

	// 4. Approve Exchange for CTF Tokens (For Selling on Orderbook)
	fmt.Printf("\n📝 [3/3] Approving Exchange for CTF Tokens (for Selling)...\n")
	tx3, err := polygon.ApproveCTF(ctx, signer, ExchangeContract, true)
	if err != nil {
		fmt.Printf("⚠️  Failed: %v\n", err)
	} else {
		fmt.Printf("✅ Sent! Hash: %s\n", tx3)
		fmt.Println("⏳ Waiting for confirmation...")
		polygon.WaitForTransaction(ctx, tx3)
	}

	fmt.Println("\n🎉 ALL APPROVALS COMPLETE.")
	fmt.Println("📊 Run 'go run cmd/diagnose/main.go' to verify.")
}
