package trading

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"

	"github.com/joho/godotenv"
)

// =============================================================================
// REAL ON-CHAIN INTEGRATION TESTS
// These tests call REAL Polygon RPC and spend REAL gas
// Run with: go test -v -run TestReal -tags=realchain
// =============================================================================

// Skip if not running real chain tests
func skipIfNotRealTest(t *testing.T) {
	if os.Getenv("RUN_REAL_CHAIN_TESTS") != "true" {
		t.Skip("Skipping real chain test. Set RUN_REAL_CHAIN_TESTS=true to run")
	}
}

// Setup real clients from .env
func setupRealClients(t *testing.T) (*api.PolygonClient, *api.Signer, *core.Config) {
	// Load .env from project root (tests run from package directory)
	envPaths := []string{
		".env",
		"../../.env",
		"../../../.env",
	}
	for _, p := range envPaths {
		if abs, err := filepath.Abs(p); err == nil {
			if _, err := os.Stat(abs); err == nil {
				godotenv.Load(abs)
				break
			}
		}
	}

	cfg, err := core.LoadConfig()
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	if cfg.PolygonRPCURL == "" {
		t.Fatal("POLYGON_RPC_URL not set in .env")
	}

	if cfg.PK == "" {
		t.Fatal("POLY_PK not set in .env")
	}

	polygon := api.NewPolygonClient(cfg.PolygonRPCURL)

	signer, err := api.NewSigner(cfg.PK, api.DefaultVerifyingContract)
	if err != nil {
		t.Fatalf("Failed to create signer: %v", err)
	}

	return polygon, signer, cfg
}

// Get a valid condition ID from an active market
func getActiveMarketConditionID(t *testing.T) string {
	ctx := context.Background()
	rest := api.NewRestClient("")

	// Find current and next 15-minute windows
	now := time.Now().UTC().Unix()
	windowStart := (now / 900) * 900

	// Try multiple windows (current, previous, next)
	windows := []int64{windowStart, windowStart - 900, windowStart + 900}
	assets := []string{"btc", "eth", "sol", "xrp"}

	for _, win := range windows {
		for _, asset := range assets {
			slug := fmt.Sprintf("%s-updown-15m-%d", asset, win)
			market, err := rest.GetMarket(ctx, slug)
			if err == nil && !market.Closed && market.ConditionID != "" {
				t.Logf("Found active market: %s (condition: %s)", slug, market.ConditionID)
				return market.ConditionID
			}
		}
	}

	t.Skip("No active market found. This is normal between market windows. Try again in a few minutes.")
	return ""
}

// =============================================================================
// REAL SPLIT TEST
// Actually splits $1 USDC into YES+NO tokens on Polygon
// =============================================================================

func TestRealSplit_1Share(t *testing.T) {
	skipIfNotRealTest(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	polygon, signer, _ := setupRealClients(t)
	addr := signer.Address()

	// Get active market
	conditionID := getActiveMarketConditionID(t)

	// Check USDC balance before
	usdcBefore, err := polygon.GetUSDCBalance(ctx, addr)
	if err != nil {
		t.Fatalf("Failed to get USDC balance: %v", err)
	}
	t.Logf("USDC before split: $%.6f", usdcBefore)

	if usdcBefore < 1.0 {
		t.Fatalf("Insufficient USDC balance: need $1, have $%.2f", usdcBefore)
	}

	// Split 1 USDC ($1 = 1,000,000 in 6 decimals)
	amount := big.NewInt(1000000) // 1 USDC
	t.Logf("Splitting $1 USDC for condition: %s", conditionID)

	txHash, err := polygon.SplitPositions(ctx, signer, conditionID, amount)
	if err != nil {
		t.Fatalf("Split failed: %v", err)
	}
	t.Logf("Split TX: https://polygonscan.com/tx/%s", txHash)

	// Wait for confirmation
	success, err := polygon.WaitForTransaction(ctx, txHash)
	if err != nil {
		t.Fatalf("Wait for tx failed: %v", err)
	}
	if !success {
		t.Fatal("Split transaction reverted on-chain")
	}

	// Check USDC balance after
	usdcAfter, err := polygon.GetUSDCBalance(ctx, addr)
	if err != nil {
		t.Fatalf("Failed to get USDC balance after: %v", err)
	}
	t.Logf("USDC after split: $%.6f", usdcAfter)

	// Verify $1 was spent (minus some tolerance for timing)
	spent := usdcBefore - usdcAfter
	if spent < 0.99 || spent > 1.01 {
		t.Errorf("Expected to spend ~$1, actually spent $%.6f", spent)
	}

	t.Logf("✅ REAL SPLIT SUCCESS: Spent $%.6f", spent)
}

// =============================================================================
// REAL MERGE TEST
// Actually merges YES+NO tokens back to USDC on Polygon
// Requires tokens from a previous split
// =============================================================================

func TestRealMerge_1Share(t *testing.T) {
	skipIfNotRealTest(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	polygon, signer, _ := setupRealClients(t)
	addr := signer.Address()

	// Use condition ID from a market where you have tokens
	// You need to have previously split or bought tokens
	conditionID := os.Getenv("TEST_CONDITION_ID")
	if conditionID == "" {
		t.Skip("Set TEST_CONDITION_ID to a condition where you have tokens")
	}

	// Check USDC balance before
	usdcBefore, err := polygon.GetUSDCBalance(ctx, addr)
	if err != nil {
		t.Fatalf("Failed to get USDC balance: %v", err)
	}
	t.Logf("USDC before merge: $%.6f", usdcBefore)

	// Merge 1 token pair
	amount := big.NewInt(1000000) // 1 share
	t.Logf("Merging 1 share for condition: %s", conditionID)

	txHash, err := polygon.MergePositions(ctx, signer, conditionID, amount)
	if err != nil {
		t.Fatalf("Merge failed: %v", err)
	}
	t.Logf("Merge TX: https://polygonscan.com/tx/%s", txHash)

	// Wait for confirmation
	success, err := polygon.WaitForTransaction(ctx, txHash)
	if err != nil {
		t.Fatalf("Wait for tx failed: %v", err)
	}
	if !success {
		t.Fatal("Merge transaction reverted on-chain")
	}

	// Check USDC balance after
	usdcAfter, err := polygon.GetUSDCBalance(ctx, addr)
	if err != nil {
		t.Fatalf("Failed to get USDC balance after: %v", err)
	}
	t.Logf("USDC after merge: $%.6f", usdcAfter)

	// Verify $1 was received
	received := usdcAfter - usdcBefore
	if received < 0.99 || received > 1.01 {
		t.Errorf("Expected to receive ~$1, actually received $%.6f", received)
	}

	t.Logf("✅ REAL MERGE SUCCESS: Received $%.6f", received)
}

// =============================================================================
// REAL SPLIT+MERGE CYCLE TEST
// Full round-trip: Split $1 → Merge back → Verify only gas was spent
// =============================================================================

func TestRealSplitMergeCycle_1Share(t *testing.T) {
	skipIfNotRealTest(t)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	polygon, signer, _ := setupRealClients(t)
	addr := signer.Address()

	// Get active market
	conditionID := getActiveMarketConditionID(t)

	// Check balances before
	usdcBefore, _ := polygon.GetUSDCBalance(ctx, addr)
	maticBefore, _ := polygon.GetMATICBalance(ctx, addr)
	t.Logf("Before: USDC=$%.6f, POL=%.6f", usdcBefore, maticBefore)

	if usdcBefore < 1.0 {
		t.Fatalf("Need at least $1 USDC, have $%.2f", usdcBefore)
	}
	if maticBefore < 0.1 {
		t.Fatalf("Need at least 0.1 POL for gas, have %.4f", maticBefore)
	}

	// Step 1: Split
	amount := big.NewInt(1000000)
	t.Log("Step 1: Splitting $1 USDC...")
	splitTx, err := polygon.SplitPositions(ctx, signer, conditionID, amount)
	if err != nil {
		t.Fatalf("Split failed: %v", err)
	}
	t.Logf("Split TX: %s", splitTx)

	success, err := polygon.WaitForTransaction(ctx, splitTx)
	if err != nil || !success {
		t.Fatalf("Split tx failed: %v, success=%v", err, success)
	}
	t.Log("Split confirmed ✓")

	// Small delay between transactions
	time.Sleep(2 * time.Second)

	// Step 2: Merge
	t.Log("Step 2: Merging tokens back...")
	mergeTx, err := polygon.MergePositions(ctx, signer, conditionID, amount)
	if err != nil {
		t.Fatalf("Merge failed: %v", err)
	}
	t.Logf("Merge TX: %s", mergeTx)

	success, err = polygon.WaitForTransaction(ctx, mergeTx)
	if err != nil || !success {
		t.Fatalf("Merge tx failed: %v, success=%v", err, success)
	}
	t.Log("Merge confirmed ✓")

	// Check balances after
	usdcAfter, _ := polygon.GetUSDCBalance(ctx, addr)
	maticAfter, _ := polygon.GetMATICBalance(ctx, addr)
	t.Logf("After: USDC=$%.6f, POL=%.6f", usdcAfter, maticAfter)

	// USDC should be the same (split then merge = net zero)
	usdcDiff := usdcBefore - usdcAfter
	if usdcDiff > 0.01 || usdcDiff < -0.01 {
		t.Errorf("USDC should be unchanged, but diff is $%.6f", usdcDiff)
	}

	// POL should be lower (gas spent)
	gasSpent := maticBefore - maticAfter
	t.Logf("Gas spent: %.6f POL", gasSpent)

	t.Logf("✅ REAL SPLIT+MERGE CYCLE SUCCESS")
	t.Logf("   USDC change: $%.6f", -usdcDiff)
	t.Logf("   Gas cost: %.6f POL", gasSpent)
}

// =============================================================================
// REAL REDEEM TEST
// Redeems tokens from a resolved market
// =============================================================================

func TestRealRedeem_ResolvedMarket(t *testing.T) {
	skipIfNotRealTest(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	polygon, signer, _ := setupRealClients(t)
	addr := signer.Address()

	// Need a condition ID from a RESOLVED market where you have tokens
	conditionID := os.Getenv("TEST_RESOLVED_CONDITION_ID")
	if conditionID == "" {
		t.Skip("Set TEST_RESOLVED_CONDITION_ID to a resolved market where you have tokens")
	}

	// Check if market is actually resolved
	resolved, err := polygon.IsMarketResolved(ctx, conditionID)
	if err != nil {
		t.Fatalf("Failed to check resolution: %v", err)
	}
	if !resolved {
		t.Skip("Market not resolved yet")
	}

	// Check USDC before
	usdcBefore, _ := polygon.GetUSDCBalance(ctx, addr)
	t.Logf("USDC before redeem: $%.6f", usdcBefore)

	// Redeem
	t.Logf("Redeeming from condition: %s", conditionID)
	txHash, err := polygon.RedeemPositions(ctx, signer, conditionID)
	if err != nil {
		t.Fatalf("Redeem failed: %v", err)
	}
	t.Logf("Redeem TX: https://polygonscan.com/tx/%s", txHash)

	// Wait for confirmation
	success, err := polygon.WaitForTransaction(ctx, txHash)
	if err != nil {
		t.Fatalf("Wait failed: %v", err)
	}
	if !success {
		t.Fatal("Redeem transaction reverted")
	}

	// Check USDC after
	usdcAfter, _ := polygon.GetUSDCBalance(ctx, addr)
	t.Logf("USDC after redeem: $%.6f", usdcAfter)

	received := usdcAfter - usdcBefore
	t.Logf("✅ REAL REDEEM SUCCESS: Received $%.6f", received)
}

// =============================================================================
// BALANCE CHECK TEST
// Simple test to verify RPC connection works
// =============================================================================

func TestRealBalanceCheck(t *testing.T) {
	skipIfNotRealTest(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	polygon, signer, _ := setupRealClients(t)
	addr := signer.Address()

	t.Logf("Wallet: %s", addr)

	usdc, err := polygon.GetUSDCBalance(ctx, addr)
	if err != nil {
		t.Fatalf("Failed to get USDC balance: %v", err)
	}
	t.Logf("USDC Balance: $%.6f", usdc)

	matic, err := polygon.GetMATICBalance(ctx, addr)
	if err != nil {
		t.Fatalf("Failed to get POL balance: %v", err)
	}
	t.Logf("POL Balance: %.6f", matic)

	block, err := polygon.GetBlockNumber(ctx)
	if err != nil {
		t.Fatalf("Failed to get block number: %v", err)
	}
	t.Logf("Current Block: %d", block)

	t.Log("✅ RPC connection working")
}
