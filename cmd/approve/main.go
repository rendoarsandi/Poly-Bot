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

	// Polymarket Exchange contract address (The spender)
	const ExchangeContract = "0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E"

	fmt.Println("🚀 Initializing Full Approval Script (USDC + CTF)...")
	
	// Create signer
	signer, err := api.NewSigner(cfg.PK)
	if err != nil {
		log.Fatalf("Failed to create signer: %v", err)
	}
	fmt.Printf("🔑 Wallet Address: %s\n", signer.Address())

	// Create Polygon client
	polygon := api.NewPolygonClient(cfg.PolygonRPCURL)

	// Context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// 1. Check current MATIC balance for gas
	matic, err := polygon.GetMATICBalance(ctx, signer.Address())
	if err != nil {
		log.Fatalf("Failed to check MATIC balance: %v", err)
	}
	fmt.Printf("⛽ MATIC Balance: %.4f\n", matic)

	// 2. Send USDC Approval
	maxUint := new(big.Int).Exp(big.NewInt(2), big.NewInt(256), nil)
	maxUint.Sub(maxUint, big.NewInt(1))

	fmt.Printf("📝 Sending USDC Approval...\n")
	txHash, err := polygon.ApproveUSDC(ctx, signer, ExchangeContract, maxUint)
	if err != nil {
		fmt.Printf("⚠️ USDC Approval failed or pending: %v\n", err)
	} else {
		fmt.Printf("✅ USDC Sent! Hash: %s\n", txHash)
		fmt.Println("⏳ Waiting for USDC confirmation...")
		polygon.WaitForTransaction(ctx, txHash)
	}

	// Wait 2 seconds between transactions to avoid nonce issues
	time.Sleep(2 * time.Second)

	// 3. Send CTF Approval
	fmt.Printf("📝 Sending CTF Approval...\n")
	ctfTxHash, err := polygon.ApproveCTF(ctx, signer, ExchangeContract, true)
	if err != nil {
		fmt.Printf("⚠️ CTF Approval failed: %v\n", err)
	} else {
		fmt.Printf("✅ CTF Sent! Hash: %s\n", ctfTxHash)
		fmt.Println("⏳ Waiting for CTF confirmation...")
		polygon.WaitForTransaction(ctx, ctfTxHash)
	}

	fmt.Println("\n🎉 SCRIPT FINISHED.")
	fmt.Println("📊 Run 'go run cmd/authtest/main.go' to see updated allowances.")
}