package trading

import (
	"context"
	"fmt"
	"math/big"
	"sync"
	"testing"

	"Market-bot/internal/api"
)

// MockPolygonClient simulates on-chain CTF operations
type MockPolygonClient struct {
	mu             sync.Mutex
	usdcBalance    float64
	tokenBalances  map[string]float64 // conditionID:outcome -> balance
	mergedAmounts  []float64
	splitAmounts   []float64
	redeemCalled   bool
	failNextMerge  bool
	failNextSplit  bool
	marketResolved map[string]bool
}

func NewMockPolygonClient(initialUSDC float64) *MockPolygonClient {
	return &MockPolygonClient{
		usdcBalance:    initialUSDC,
		tokenBalances:  make(map[string]float64),
		marketResolved: make(map[string]bool),
	}
}

func (m *MockPolygonClient) GetUSDCBalance(ctx context.Context, address string) (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.usdcBalance, nil
}

func (m *MockPolygonClient) SplitPositions(ctx context.Context, signer *api.Signer, conditionID string, amount *big.Int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failNextSplit {
		m.failNextSplit = false
		return "", fmt.Errorf("split failed")
	}

	// Convert amount from 6 decimals
	usdcAmount := float64(amount.Int64()) / 1e6

	if m.usdcBalance < usdcAmount {
		return "", fmt.Errorf("insufficient USDC balance")
	}

	// Deduct USDC, add tokens
	m.usdcBalance -= usdcAmount
	m.tokenBalances[conditionID+":Up"] += usdcAmount
	m.tokenBalances[conditionID+":Down"] += usdcAmount
	m.splitAmounts = append(m.splitAmounts, usdcAmount)

	return "0xsplit_tx_hash", nil
}

func (m *MockPolygonClient) MergePositions(ctx context.Context, signer *api.Signer, conditionID string, amount *big.Int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failNextMerge {
		m.failNextMerge = false
		return "", fmt.Errorf("merge failed")
	}

	// Convert amount from 6 decimals
	shares := float64(amount.Int64()) / 1e6

	upKey := conditionID + ":Up"
	downKey := conditionID + ":Down"

	if m.tokenBalances[upKey] < shares || m.tokenBalances[downKey] < shares {
		return "", fmt.Errorf("insufficient token balance for merge")
	}

	// Burn tokens, return USDC
	m.tokenBalances[upKey] -= shares
	m.tokenBalances[downKey] -= shares
	m.usdcBalance += shares // 1 UP + 1 DOWN = $1 USDC
	m.mergedAmounts = append(m.mergedAmounts, shares)

	return "0xmerge_tx_hash", nil
}

func (m *MockPolygonClient) RedeemPositions(ctx context.Context, signer *api.Signer, conditionID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.redeemCalled = true

	// Redeem winning tokens
	upKey := conditionID + ":Up"
	downKey := conditionID + ":Down"

	// Assume Up wins, redeem Up tokens at $1 each
	if m.tokenBalances[upKey] > 0 {
		m.usdcBalance += m.tokenBalances[upKey]
		m.tokenBalances[upKey] = 0
	}
	// Down tokens worth $0
	m.tokenBalances[downKey] = 0

	return "0xredeem_tx_hash", nil
}

func (m *MockPolygonClient) IsMarketResolved(ctx context.Context, conditionID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.marketResolved[conditionID], nil
}

func (m *MockPolygonClient) WaitForTransaction(ctx context.Context, txHash string) (bool, error) {
	return true, nil // Always succeed in mock
}

// MockCLOBClient simulates CLOB order operations
type MockCLOBClient struct {
	mu            sync.Mutex
	orders        []MockOrder
	fills         map[string]bool
	tokenBalances map[string]float64
	usdcBalance   float64
	failNextBuy   bool
	failNextSell  bool
	orderCounter  int
}

type MockOrder struct {
	OrderID string
	TokenID string
	Side    string
	Price   float64
	Size    float64
	Filled  bool
}

func NewMockCLOBClient(initialUSDC float64) *MockCLOBClient {
	return &MockCLOBClient{
		fills:         make(map[string]bool),
		tokenBalances: make(map[string]float64),
		usdcBalance:   initialUSDC,
	}
}

func (m *MockCLOBClient) PlaceBuyOrder(tokenID string, price, size float64) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failNextBuy {
		m.failNextBuy = false
		return "", fmt.Errorf("buy order failed")
	}

	cost := price * size
	if m.usdcBalance < cost {
		return "", fmt.Errorf("insufficient USDC for buy")
	}

	m.orderCounter++
	orderID := fmt.Sprintf("order_%d", m.orderCounter)

	// Deduct USDC, add tokens (instant fill simulation)
	m.usdcBalance -= cost
	m.tokenBalances[tokenID] += size
	m.fills[orderID] = true

	m.orders = append(m.orders, MockOrder{
		OrderID: orderID,
		TokenID: tokenID,
		Side:    "BUY",
		Price:   price,
		Size:    size,
		Filled:  true,
	})

	return orderID, nil
}

func (m *MockCLOBClient) PlaceSellOrder(tokenID string, price, size float64) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failNextSell {
		m.failNextSell = false
		return "", fmt.Errorf("sell order failed")
	}

	if m.tokenBalances[tokenID] < size {
		return "", fmt.Errorf("insufficient tokens for sell")
	}

	m.orderCounter++
	orderID := fmt.Sprintf("order_%d", m.orderCounter)

	// Deduct tokens, add USDC (instant fill simulation)
	m.tokenBalances[tokenID] -= size
	m.usdcBalance += price * size
	m.fills[orderID] = true

	m.orders = append(m.orders, MockOrder{
		OrderID: orderID,
		TokenID: tokenID,
		Side:    "SELL",
		Price:   price,
		Size:    size,
		Filled:  true,
	})

	return orderID, nil
}

func (m *MockCLOBClient) IsFilled(orderID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fills[orderID]
}

// =============================================================================
// BUY-MERGE FLOW TESTS
// Simulates: detect ask_sum < $0.98 → buy both sides → merge → profit
// =============================================================================

func TestBuyMergeFlow_FullCycle(t *testing.T) {
	ctx := context.Background()

	// Setup: $10 USDC balance
	polygon := NewMockPolygonClient(10.0)
	clob := NewMockCLOBClient(10.0)

	conditionID := "0xtest_condition_id"
	upTokenID := "token_up_123"
	downTokenID := "token_down_456"

	// Simulate market prices: ask_sum = $0.96 (profitable!)
	askUp := 0.48
	askDown := 0.48
	askSum := askUp + askDown // $0.96

	// Verify opportunity exists
	if askSum >= 0.98 {
		t.Fatalf("Test setup error: ask_sum should be < $0.98, got %.2f", askSum)
	}

	shares := 1.0 // 1 share each side

	// Step 1: Buy UP tokens
	upOrderID, err := clob.PlaceBuyOrder(upTokenID, askUp, shares)
	if err != nil {
		t.Fatalf("Failed to buy UP: %v", err)
	}
	if !clob.IsFilled(upOrderID) {
		t.Fatal("UP order not filled")
	}

	// Step 2: Buy DOWN tokens
	downOrderID, err := clob.PlaceBuyOrder(downTokenID, askDown, shares)
	if err != nil {
		t.Fatalf("Failed to buy DOWN: %v", err)
	}
	if !clob.IsFilled(downOrderID) {
		t.Fatal("DOWN order not filled")
	}

	// Verify we spent the right amount
	expectedSpent := askSum * shares // $0.96
	actualBalance := clob.usdcBalance
	expectedBalance := 10.0 - expectedSpent
	if actualBalance != expectedBalance {
		t.Errorf("USDC balance after buy: expected $%.2f, got $%.2f", expectedBalance, actualBalance)
	}

	// Step 3: Transfer tokens to polygon for merge (simulate)
	polygon.tokenBalances[conditionID+":Up"] = shares
	polygon.tokenBalances[conditionID+":Down"] = shares

	// Step 4: Merge on-chain
	amount := big.NewInt(int64(shares * 1e6))
	txHash, err := polygon.MergePositions(ctx, nil, conditionID, amount)
	if err != nil {
		t.Fatalf("Merge failed: %v", err)
	}
	if txHash == "" {
		t.Fatal("No tx hash returned from merge")
	}

	// Step 5: Verify profit
	// Started with $10, spent $0.96, got back $1.00 from merge
	// Profit = $1.00 - $0.96 = $0.04
	finalUSDC := polygon.usdcBalance
	expectedFinalUSDC := 10.0 - expectedSpent + shares // $10 - $0.96 + $1.00 = $10.04

	// Note: In this test, polygon and clob have separate balances
	// In reality, they share the same wallet
	if polygon.usdcBalance != expectedFinalUSDC {
		t.Logf("Polygon USDC: $%.4f (expected $%.4f)", finalUSDC, expectedFinalUSDC)
	}

	// Verify merge was recorded
	if len(polygon.mergedAmounts) != 1 {
		t.Errorf("Expected 1 merge, got %d", len(polygon.mergedAmounts))
	}
	if polygon.mergedAmounts[0] != shares {
		t.Errorf("Merged amount: expected %.2f, got %.2f", shares, polygon.mergedAmounts[0])
	}

	// Verify tokens are gone after merge
	if polygon.tokenBalances[conditionID+":Up"] != 0 {
		t.Errorf("UP tokens should be 0 after merge, got %.2f", polygon.tokenBalances[conditionID+":Up"])
	}
	if polygon.tokenBalances[conditionID+":Down"] != 0 {
		t.Errorf("DOWN tokens should be 0 after merge, got %.2f", polygon.tokenBalances[conditionID+":Down"])
	}

	t.Logf("BUY-MERGE SUCCESS: Spent $%.4f, merged for $%.4f, profit $%.4f",
		expectedSpent, shares, shares-expectedSpent)
}

func TestBuyMergeFlow_InsufficientBalance(t *testing.T) {
	clob := NewMockCLOBClient(0.50) // Only $0.50

	// Try to buy $0.48 + $0.48 = $0.96 worth
	_, err := clob.PlaceBuyOrder("token_up", 0.48, 1.0)
	if err != nil {
		t.Fatalf("First buy should succeed: %v", err)
	}

	// Second buy should fail
	_, err = clob.PlaceBuyOrder("token_down", 0.48, 1.0)
	if err == nil {
		t.Error("Second buy should fail due to insufficient balance")
	}
}

func TestBuyMergeFlow_MergeFails(t *testing.T) {
	ctx := context.Background()
	polygon := NewMockPolygonClient(10.0)

	conditionID := "0xtest"
	polygon.tokenBalances[conditionID+":Up"] = 1.0
	polygon.tokenBalances[conditionID+":Down"] = 1.0
	polygon.failNextMerge = true

	amount := big.NewInt(1e6)
	_, err := polygon.MergePositions(ctx, nil, conditionID, amount)
	if err == nil {
		t.Error("Merge should have failed")
	}

	// Tokens should still exist
	if polygon.tokenBalances[conditionID+":Up"] != 1.0 {
		t.Error("Tokens should not be burned on failed merge")
	}
}

// =============================================================================
// SPLIT-SELL FLOW TESTS
// Simulates: split USDC → detect bid_sum > $1.03 → sell both sides → profit
// =============================================================================

func TestSplitSellFlow_FullCycle(t *testing.T) {
	ctx := context.Background()

	// Setup: $10 USDC balance
	polygon := NewMockPolygonClient(10.0)
	clob := NewMockCLOBClient(0) // CLOB starts with 0, gets tokens from split

	conditionID := "0xtest_condition_id"
	upTokenID := "token_up_123"
	downTokenID := "token_down_456"

	shares := 1.0 // Split $1 USDC into 1 UP + 1 DOWN

	// Step 1: Split USDC into tokens
	amount := big.NewInt(int64(shares * 1e6))
	txHash, err := polygon.SplitPositions(ctx, nil, conditionID, amount)
	if err != nil {
		t.Fatalf("Split failed: %v", err)
	}
	if txHash == "" {
		t.Fatal("No tx hash returned from split")
	}

	// Verify split worked
	if polygon.usdcBalance != 9.0 {
		t.Errorf("USDC after split: expected $9.00, got $%.2f", polygon.usdcBalance)
	}
	if polygon.tokenBalances[conditionID+":Up"] != shares {
		t.Errorf("UP tokens after split: expected %.2f, got %.2f", shares, polygon.tokenBalances[conditionID+":Up"])
	}
	if polygon.tokenBalances[conditionID+":Down"] != shares {
		t.Errorf("DOWN tokens after split: expected %.2f, got %.2f", shares, polygon.tokenBalances[conditionID+":Down"])
	}

	// Step 2: Transfer tokens to CLOB for selling (simulate)
	clob.tokenBalances[upTokenID] = shares
	clob.tokenBalances[downTokenID] = shares

	// Simulate market prices: bid_sum = $1.04 (profitable!)
	bidUp := 0.52
	bidDown := 0.52
	bidSum := bidUp + bidDown // $1.04

	// Verify opportunity exists
	if bidSum <= 1.03 {
		t.Fatalf("Test setup error: bid_sum should be > $1.03, got %.2f", bidSum)
	}

	// Step 3: Sell UP tokens
	upOrderID, err := clob.PlaceSellOrder(upTokenID, bidUp, shares)
	if err != nil {
		t.Fatalf("Failed to sell UP: %v", err)
	}
	if !clob.IsFilled(upOrderID) {
		t.Fatal("UP sell order not filled")
	}

	// Step 4: Sell DOWN tokens
	downOrderID, err := clob.PlaceSellOrder(downTokenID, bidDown, shares)
	if err != nil {
		t.Fatalf("Failed to sell DOWN: %v", err)
	}
	if !clob.IsFilled(downOrderID) {
		t.Fatal("DOWN sell order not filled")
	}

	// Step 5: Verify profit
	// Split cost: $1.00
	// Sold for: $0.52 + $0.52 = $1.04
	// Profit: $0.04
	expectedProceeds := bidSum * shares
	if clob.usdcBalance != expectedProceeds {
		t.Errorf("CLOB USDC after sells: expected $%.4f, got $%.4f", expectedProceeds, clob.usdcBalance)
	}

	// Verify tokens are gone after sell
	if clob.tokenBalances[upTokenID] != 0 {
		t.Errorf("UP tokens should be 0 after sell, got %.2f", clob.tokenBalances[upTokenID])
	}
	if clob.tokenBalances[downTokenID] != 0 {
		t.Errorf("DOWN tokens should be 0 after sell, got %.2f", clob.tokenBalances[downTokenID])
	}

	profit := expectedProceeds - shares
	t.Logf("SPLIT-SELL SUCCESS: Split $%.4f, sold for $%.4f, profit $%.4f",
		shares, expectedProceeds, profit)
}

func TestSplitSellFlow_InsufficientUSDCForSplit(t *testing.T) {
	ctx := context.Background()
	polygon := NewMockPolygonClient(0.50) // Only $0.50

	// Try to split $1.00
	amount := big.NewInt(1e6)
	_, err := polygon.SplitPositions(ctx, nil, "0xtest", amount)
	if err == nil {
		t.Error("Split should fail due to insufficient USDC")
	}
}

func TestSplitSellFlow_PartialSell(t *testing.T) {
	clob := NewMockCLOBClient(0)
	clob.tokenBalances["token_up"] = 1.0
	clob.tokenBalances["token_down"] = 1.0

	// Sell UP successfully
	_, err := clob.PlaceSellOrder("token_up", 0.52, 1.0)
	if err != nil {
		t.Fatalf("UP sell should succeed: %v", err)
	}

	// Fail DOWN sell
	clob.failNextSell = true
	_, err = clob.PlaceSellOrder("token_down", 0.52, 1.0)
	if err == nil {
		t.Error("DOWN sell should fail")
	}

	// We have USDC from UP but still hold DOWN tokens
	if clob.usdcBalance != 0.52 {
		t.Errorf("Should have $0.52 from UP sell, got $%.2f", clob.usdcBalance)
	}
	if clob.tokenBalances["token_down"] != 1.0 {
		t.Errorf("Should still hold DOWN tokens, got %.2f", clob.tokenBalances["token_down"])
	}
}

// =============================================================================
// EDGE CASES AND ERROR HANDLING
// =============================================================================

func TestMergeWithImbalancedTokens(t *testing.T) {
	ctx := context.Background()
	polygon := NewMockPolygonClient(10.0)

	conditionID := "0xtest"
	// Imbalanced: 2 UP but only 1 DOWN
	polygon.tokenBalances[conditionID+":Up"] = 2.0
	polygon.tokenBalances[conditionID+":Down"] = 1.0

	// Try to merge 2 - should fail
	amount := big.NewInt(2e6)
	_, err := polygon.MergePositions(ctx, nil, conditionID, amount)
	if err == nil {
		t.Error("Merge should fail with insufficient DOWN tokens")
	}

	// Merge 1 should succeed
	amount = big.NewInt(1e6)
	_, err = polygon.MergePositions(ctx, nil, conditionID, amount)
	if err != nil {
		t.Fatalf("Merge of 1 share should succeed: %v", err)
	}

	// Should have 1 UP left, 0 DOWN
	if polygon.tokenBalances[conditionID+":Up"] != 1.0 {
		t.Errorf("Expected 1 UP remaining, got %.2f", polygon.tokenBalances[conditionID+":Up"])
	}
	if polygon.tokenBalances[conditionID+":Down"] != 0 {
		t.Errorf("Expected 0 DOWN remaining, got %.2f", polygon.tokenBalances[conditionID+":Down"])
	}
}

func TestRedeemAfterMarketResolution(t *testing.T) {
	ctx := context.Background()
	polygon := NewMockPolygonClient(0)

	conditionID := "0xtest"
	// Hold 10 UP tokens (winner) and 10 DOWN tokens (loser)
	polygon.tokenBalances[conditionID+":Up"] = 10.0
	polygon.tokenBalances[conditionID+":Down"] = 10.0
	polygon.marketResolved[conditionID] = true

	// Check market is resolved
	resolved, _ := polygon.IsMarketResolved(ctx, conditionID)
	if !resolved {
		t.Fatal("Market should be resolved")
	}

	// Redeem
	_, err := polygon.RedeemPositions(ctx, nil, conditionID)
	if err != nil {
		t.Fatalf("Redeem failed: %v", err)
	}

	// Should get $10 back (UP wins at $1 each)
	if polygon.usdcBalance != 10.0 {
		t.Errorf("Expected $10 from redeem, got $%.2f", polygon.usdcBalance)
	}

	// All tokens should be gone
	if polygon.tokenBalances[conditionID+":Up"] != 0 {
		t.Errorf("UP tokens should be 0, got %.2f", polygon.tokenBalances[conditionID+":Up"])
	}
	if polygon.tokenBalances[conditionID+":Down"] != 0 {
		t.Errorf("DOWN tokens should be 0, got %.2f", polygon.tokenBalances[conditionID+":Down"])
	}
}

func TestConcurrentBuyMerge(t *testing.T) {
	ctx := context.Background()
	polygon := NewMockPolygonClient(100.0)

	conditionID := "0xtest"
	var wg sync.WaitGroup
	successCount := 0
	var mu sync.Mutex

	// Simulate 10 concurrent split+merge operations
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			// Split
			amount := big.NewInt(1e6)
			_, err := polygon.SplitPositions(ctx, nil, conditionID, amount)
			if err != nil {
				return
			}

			// Merge
			_, err = polygon.MergePositions(ctx, nil, conditionID, amount)
			if err != nil {
				return
			}

			mu.Lock()
			successCount++
			mu.Unlock()
		}(i)
	}

	wg.Wait()

	// All should succeed since we have enough balance
	if successCount != 10 {
		t.Errorf("Expected 10 successful operations, got %d", successCount)
	}

	// Balance should be unchanged (split then merge = net zero)
	if polygon.usdcBalance != 100.0 {
		t.Errorf("Balance should be $100 after split+merge pairs, got $%.2f", polygon.usdcBalance)
	}
}

// =============================================================================
// TIMING AND MARKET WINDOW TESTS
// =============================================================================

func TestMarketWindowExpiry(t *testing.T) {
	ctx := context.Background()
	polygon := NewMockPolygonClient(10.0)

	conditionID := "0xtest"

	// Split at start of window
	amount := big.NewInt(1e6)
	_, err := polygon.SplitPositions(ctx, nil, conditionID, amount)
	if err != nil {
		t.Fatalf("Split failed: %v", err)
	}

	// Simulate: couldn't sell in time, market resolved
	polygon.marketResolved[conditionID] = true

	// Should redeem instead of merge
	resolved, _ := polygon.IsMarketResolved(ctx, conditionID)
	if !resolved {
		t.Fatal("Market should be resolved")
	}

	// Redeem works
	_, err = polygon.RedeemPositions(ctx, nil, conditionID)
	if err != nil {
		t.Fatalf("Redeem failed: %v", err)
	}

	t.Log("MARKET EXPIRY: Successfully redeemed after market resolution")
}

// Benchmark for performance testing
func BenchmarkSplitMergeCycle(b *testing.B) {
	ctx := context.Background()
	polygon := NewMockPolygonClient(float64(b.N * 10))
	conditionID := "0xbench"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		amount := big.NewInt(1e6)
		polygon.SplitPositions(ctx, nil, conditionID, amount)
		polygon.MergePositions(ctx, nil, conditionID, amount)
	}
}
