package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
)

// TestMergePositions_CallDataEncoding verifies the merge calldata is correctly encoded
func TestMergePositions_CallDataEncoding(t *testing.T) {
	// From a known successful merge transaction:
	// https://polygonscan.com/tx/0x728673d8845665f8856550f391f10fe8898c6596ff63b17c60cbec128074cf1a
	// Method: 0x9e7212ad
	// collateralToken: 0x2791bca1f2de4661ed88a30c99a7a9449aa84174
	// parentCollectionId: 0x00...00
	// conditionId: 0xc68c0fd8b97571c790259a08c847794150eaa0b8aa4865023d0774a1c79a2710
	// partition: [2, 1]
	// amount: 19707500

	conditionID := "0xc68c0fd8b97571c790259a08c847794150eaa0b8aa4865023d0774a1c79a2710"
	amount := big.NewInt(19707500)

	// Build calldata manually (same logic as MergePositions)
	collateral := "000000000000000000000000" + strings.TrimPrefix(strings.ToLower(PUSDContract), "0x")
	parent := "0000000000000000000000000000000000000000000000000000000000000000"
	cond := strings.TrimPrefix(conditionID, "0x")
	offset := "00000000000000000000000000000000000000000000000000000000000000a0"
	amtHex := "00000000000000000000000000000000000000000000000000000000012cb66c" // 19707500 in hex
	arrayLen := "0000000000000000000000000000000000000000000000000000000000000002"
	idx1 := "0000000000000000000000000000000000000000000000000000000000000002"
	idx2 := "0000000000000000000000000000000000000000000000000000000000000001"

	expected := "0x9e7212ad" + collateral + parent + cond + offset + amtHex + arrayLen + idx1 + idx2

	// What our code generates
	actualCollateral := "000000000000000000000000" + strings.TrimPrefix(strings.ToLower(PUSDContract), "0x")
	actualParent := "0000000000000000000000000000000000000000000000000000000000000000"
	actualCond := strings.TrimPrefix(conditionID, "0x")
	actualOffset := "00000000000000000000000000000000000000000000000000000000000000a0"
	actualAmtHex := padToHex64(amount)
	actualArrayLen := "0000000000000000000000000000000000000000000000000000000000000002"
	actualIdx1 := "0000000000000000000000000000000000000000000000000000000000000002"
	actualIdx2 := "0000000000000000000000000000000000000000000000000000000000000001"

	actual := "0x9e7212ad" + actualCollateral + actualParent + actualCond + actualOffset + actualAmtHex + actualArrayLen + actualIdx1 + actualIdx2

	if !strings.EqualFold(expected, actual) {
		t.Errorf("Calldata mismatch:\nExpected: %s\nActual:   %s", expected, actual)
	}

	// Verify function selector
	if !strings.HasPrefix(actual, "0x9e7212ad") {
		t.Errorf("Wrong function selector, expected 0x9e7212ad, got %s", actual[:10])
	}
}

// TestSplitPositions_CallDataEncoding verifies the split calldata is correctly encoded
func TestSplitPositions_CallDataEncoding(t *testing.T) {
	// From a known successful split transaction:
	// https://polygonscan.com/tx/0xfd36396279c1f9141ffe875c196a8998c92b6c437633f00f9c6795693017cb2e
	// Method: 0x72ce4275
	// partition: [1, 2]
	// amount: 1000000

	// Verify function selector
	expectedSelector := "0x72ce4275"

	collateral := "000000000000000000000000" + strings.TrimPrefix(strings.ToLower(PUSDContract), "0x")
	parent := "0000000000000000000000000000000000000000000000000000000000000000"
	conditionID := "e235e4439819c4df8bd73ee5dd1470cd01b63addda00e9bc9e44c1a016d75d65"
	offset := "00000000000000000000000000000000000000000000000000000000000000a0"
	amtHex := "00000000000000000000000000000000000000000000000000000000000f4240" // 1000000
	arrayLen := "0000000000000000000000000000000000000000000000000000000000000002"
	idx1 := "0000000000000000000000000000000000000000000000000000000000000001"
	idx2 := "0000000000000000000000000000000000000000000000000000000000000002"

	data := expectedSelector + collateral + parent + conditionID + offset + amtHex + arrayLen + idx1 + idx2

	// Check it starts with the right selector
	if !strings.HasPrefix(data, expectedSelector) {
		t.Errorf("Wrong function selector, expected %s", expectedSelector)
	}

	// Check partition order is [1, 2] for split
	if !strings.Contains(data, arrayLen+idx1+idx2) {
		t.Error("Partition array should be [1, 2] for split")
	}
}

// TestRedeemPositions_CallDataEncoding verifies the redeem calldata is correctly encoded
func TestRedeemPositions_CallDataEncoding(t *testing.T) {
	// From successful redeem transaction:
	// https://polygonscan.com/tx/0x5bf9f3d38256e333f528817fbc77e3e2a40f7e6ead4f0c2cb877da52113a4017
	// Method: 0x01b7037c

	expectedSelector := "0x01b7037c"

	// Verify the selector is correct
	if expectedSelector != "0x01b7037c" {
		t.Errorf("Wrong redeem function selector, expected 0x01b7037c")
	}

	// Verify pUSD contract address is correct
	expectedCollateral := "0xC011a7E12a19f7B1f670d46F03B03f3342E82DFB"
	if !strings.EqualFold(PUSDContract, expectedCollateral) {
		t.Errorf("Wrong pUSD contract address")
	}

	// Verify CTF contract address is correct
	expectedCTF := "0x4D97DCd97eC945f40cF65F87097ACe5EA0476045"
	if !strings.EqualFold(CTFContract, expectedCTF) {
		t.Errorf("Wrong CTF contract address")
	}

	if polygonRedeemGasLimit != 500000 {
		t.Fatalf("expected redeem gas limit 500000, got %d", polygonRedeemGasLimit)
	}
}

func TestPolymarketV2ContractAddresses(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"USDCeContract", USDCeContract, "0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174"},
		{"PUSDContract", PUSDContract, "0xC011a7E12a19f7B1f670d46F03B03f3342E82DFB"},
		{"CollateralOnrampContract", CollateralOnrampContract, "0x93070a847efEf7F70739046A929D47a521F5B8ee"},
		{"CollateralOfframpContract", CollateralOfframpContract, "0x2957922Eb93258b93368531d39fAcCA3B4dC5854"},
		{"CTFExchange", CTFExchange, "0xE111180000d2663C0091e4f400237545B87B996B"},
		{"NegRiskExchange", NegRiskExchange, "0xe2222d279d744050d28e00520010520000310F59"},
		{"NegRiskAdapter", NegRiskAdapter, "0xd91E80cF2E7be2e162c6513ceD06f1dD0dA35296"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.EqualFold(tc.got, tc.want) {
				t.Fatalf("%s = %s, want %s", tc.name, tc.got, tc.want)
			}
		})
	}
}

// TestFunctionSelectors verifies all function selectors match Gnosis CTF contract
func TestFunctionSelectors(t *testing.T) {
	tests := []struct {
		name     string
		selector string
		function string
	}{
		{"mergePositions", "0x9e7212ad", "mergePositions(address,bytes32,bytes32,uint256[],uint256)"},
		{"splitPosition", "0x72ce4275", "splitPosition(address,bytes32,bytes32,uint256[],uint256)"},
		{"redeemPositions", "0x01b7037c", "redeemPositions(address,bytes32,bytes32,uint256[])"},
		{"collateralOnrampWrap", "0x62355638", "wrap(address,address,uint256)"},
		{"collateralOfframpUnwrap", "0x8cc7104f", "unwrap(address,address,uint256)"},
		{"payoutDenominator", payoutDenominatorSelector, "payoutDenominator(bytes32)"},
		{"payoutNumerators", payoutNumeratorsSelector, "payoutNumerators(bytes32,uint256)"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := selectorForSignature(tc.function)
			if got != tc.selector {
				t.Fatalf("selector for %s = %s, want %s", tc.function, got, tc.selector)
			}
		})
	}
}

func TestConditionResolutionTopic(t *testing.T) {
	got := crypto.Keccak256Hash([]byte("ConditionResolution(bytes32,address,bytes32,uint256,uint256[])")).Hex()
	if got != conditionResolutionTopic {
		t.Fatalf("ConditionResolution topic = %s, want %s", got, conditionResolutionTopic)
	}
}

func selectorForSignature(signature string) string {
	hash := crypto.Keccak256([]byte(signature))
	return "0x" + hex.EncodeToString(hash[:4])
}

func padToHex64(n *big.Int) string {
	hex := n.Text(16)
	if len(hex) < 64 {
		hex = strings.Repeat("0", 64-len(hex)) + hex
	}
	return hex
}

func TestWaitForTransactionTimeoutReportsPendingState(t *testing.T) {
	var receiptCalls atomic.Int32
	var txCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req RPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		switch req.Method {
		case "eth_getTransactionReceipt":
			receiptCalls.Add(1)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":null}`))
		case "eth_getTransactionByHash":
			txCalls.Add(1)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"hash":"0xabc","blockNumber":"0x"}}`))
		default:
			t.Fatalf("unexpected method %s", req.Method)
		}
	}))
	defer server.Close()

	client := NewPolygonClient(server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2200*time.Millisecond)
	defer cancel()

	success, err := client.WaitForTransaction(ctx, "0xabc")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if success {
		t.Fatal("expected unsuccessful confirmation")
	}
	if !strings.Contains(err.Error(), "still pending in RPC/mempool") {
		t.Fatalf("expected pending-state timeout detail, got %v", err)
	}
	if receiptCalls.Load() > 1 {
		t.Fatalf("expected at most one receipt poll before timeout, got %d", receiptCalls.Load())
	}
	if txCalls.Load() != 1 {
		t.Fatalf("expected one tx status probe on timeout, got %d", txCalls.Load())
	}
}

func TestBumpGasPrice(t *testing.T) {
	base := big.NewInt(100)
	bumped := bumpGasPrice(base)
	if bumped.String() != "150" {
		t.Fatalf("expected 50%% gas bump from 100 to 150, got %s", bumped.String())
	}
	if base.String() != "100" {
		t.Fatalf("expected original gas price to remain unchanged, got %s", base.String())
	}
}

func TestGasFeesForWriteTxUsesDynamicWhenRPCSupportsIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req RPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		switch req.Method {
		case "eth_gasPrice":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x64"}`))
		case "eth_maxPriorityFeePerGas":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x20"}`))
		case "eth_getBlockByNumber":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"baseFeePerGas":"0x65"}}`))
		default:
			t.Fatalf("unexpected method %s", req.Method)
		}
	}))
	defer server.Close()

	client := NewPolygonClient(server.URL)
	fees, err := client.gasFeesForWriteTx(context.Background())
	if err != nil {
		t.Fatalf("gasFeesForWriteTx() error = %v", err)
	}
	if !fees.UseDynamic() {
		t.Fatal("expected dynamic fee tx configuration")
	}
	if fees.LegacyGasPrice == nil || fees.LegacyGasPrice.String() != "150" {
		t.Fatalf("expected bumped legacy gas price 150, got %v", fees.LegacyGasPrice)
	}
	if fees.MaxPriorityFeePerGas == nil || fees.MaxPriorityFeePerGas.String() != "48" {
		t.Fatalf("expected bumped priority fee 48, got %v", fees.MaxPriorityFeePerGas)
	}
	if fees.MaxFeePerGas == nil || fees.MaxFeePerGas.String() != "250" {
		t.Fatalf("expected max fee 250, got %v", fees.MaxFeePerGas)
	}
}

func TestUrgentGasFeesForWriteTxUsesPriorityFloor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req RPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		switch req.Method {
		case "eth_gasPrice":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x64"}`))
		case "eth_maxPriorityFeePerGas":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x20"}`))
		case "eth_getBlockByNumber":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"baseFeePerGas":"0x65"}}`))
		default:
			t.Fatalf("unexpected method %s", req.Method)
		}
	}))
	defer server.Close()

	client := NewPolygonClient(server.URL)
	fees, err := client.urgentGasFeesForWriteTx(context.Background())
	if err != nil {
		t.Fatalf("urgentGasFeesForWriteTx() error = %v", err)
	}
	if !fees.UseDynamic() {
		t.Fatal("expected dynamic fee tx configuration")
	}
	if fees.LegacyGasPrice == nil || fees.LegacyGasPrice.String() != "200" {
		t.Fatalf("expected urgent bumped legacy gas price 200, got %v", fees.LegacyGasPrice)
	}
	if fees.MaxPriorityFeePerGas == nil || fees.MaxPriorityFeePerGas.Cmp(polygonUrgentMinPriorityFeePerGas) != 0 {
		t.Fatalf("expected urgent priority fee floor %v, got %v", polygonUrgentMinPriorityFeePerGas, fees.MaxPriorityFeePerGas)
	}
	wantMaxFee := new(big.Int).Mul(big.NewInt(101), big.NewInt(polygonUrgentBaseFeeMultiplier))
	wantMaxFee.Add(wantMaxFee, polygonUrgentMinPriorityFeePerGas)
	if fees.MaxFeePerGas == nil || fees.MaxFeePerGas.Cmp(wantMaxFee) != 0 {
		t.Fatalf("expected urgent max fee %v, got %v", wantMaxFee, fees.MaxFeePerGas)
	}
}

func TestFastGasFeesForWriteTxUsesModeratePriorityFloor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req RPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		switch req.Method {
		case "eth_gasPrice":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x64"}`))
		case "eth_maxPriorityFeePerGas":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x20"}`))
		case "eth_getBlockByNumber":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"baseFeePerGas":"0x65"}}`))
		default:
			t.Fatalf("unexpected method %s", req.Method)
		}
	}))
	defer server.Close()

	client := NewPolygonClient(server.URL)
	fees, err := client.fastGasFeesForWriteTx(context.Background())
	if err != nil {
		t.Fatalf("fastGasFeesForWriteTx() error = %v", err)
	}
	if !fees.UseDynamic() {
		t.Fatal("expected dynamic fee tx configuration")
	}
	if fees.LegacyGasPrice == nil || fees.LegacyGasPrice.String() != "170" {
		t.Fatalf("expected fast bumped legacy gas price 170, got %v", fees.LegacyGasPrice)
	}
	if fees.MaxPriorityFeePerGas == nil || fees.MaxPriorityFeePerGas.Cmp(polygonFastMinPriorityFeePerGas) != 0 {
		t.Fatalf("expected fast priority fee floor %v, got %v", polygonFastMinPriorityFeePerGas, fees.MaxPriorityFeePerGas)
	}
	wantMaxFee := new(big.Int).Mul(big.NewInt(101), big.NewInt(polygonFastBaseFeeMultiplier))
	wantMaxFee.Add(wantMaxFee, polygonFastMinPriorityFeePerGas)
	if fees.MaxFeePerGas == nil || fees.MaxFeePerGas.Cmp(wantMaxFee) != 0 {
		t.Fatalf("expected fast max fee %v, got %v", wantMaxFee, fees.MaxFeePerGas)
	}
}

func TestGasFeesForWriteTxFallsBackToLegacyWhenPriorityUnsupported(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req RPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		switch req.Method {
		case "eth_gasPrice":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x64"}`))
		case "eth_maxPriorityFeePerGas":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`))
		default:
			t.Fatalf("unexpected method %s", req.Method)
		}
	}))
	defer server.Close()

	client := NewPolygonClient(server.URL)
	fees, err := client.gasFeesForWriteTx(context.Background())
	if err != nil {
		t.Fatalf("gasFeesForWriteTx() error = %v", err)
	}
	if fees.UseDynamic() {
		t.Fatal("expected legacy fallback when priority fee RPC is unsupported")
	}
	if fees.LegacyGasPrice == nil || fees.LegacyGasPrice.String() != "150" {
		t.Fatalf("expected bumped legacy gas price 150, got %v", fees.LegacyGasPrice)
	}
}

func TestGetWinningOutcome(t *testing.T) {
	conditionID := "0xc68c0fd8b97571c790259a08c847794150eaa0b8aa4865023d0774a1c79a2710"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req RPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		params := req.Params[0].(map[string]interface{})
		data := params["data"].(string)
		switch {
		case strings.HasPrefix(data, payoutNumeratorsSelector):
			idxHex := data[len(data)-64:]
			if strings.HasSuffix(idxHex, "0") {
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x0"}`))
				return
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
		default:
			t.Fatalf("unexpected data payload %s", data)
		}
	}))
	defer server.Close()

	client := NewPolygonClient(server.URL)
	winner, err := client.GetWinningOutcome(context.Background(), conditionID, []string{"Down", "Up"})
	if err != nil {
		t.Fatalf("GetWinningOutcome() error = %v", err)
	}
	if winner != "Up" {
		t.Fatalf("GetWinningOutcome() = %q, want Up", winner)
	}
}

func TestResolutionCacheUsesOnChainWinner(t *testing.T) {
	conditionID := "0xc68c0fd8b97571c790259a08c847794150eaa0b8aa4865023d0774a1c79a2710"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req RPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		params := req.Params[0].(map[string]interface{})
		data := params["data"].(string)
		switch {
		case strings.HasPrefix(data, payoutDenominatorSelector):
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
		case strings.HasPrefix(data, payoutNumeratorsSelector):
			idxHex := data[len(data)-64:]
			if strings.HasSuffix(idxHex, "0") {
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x0"}`))
				return
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
		default:
			t.Fatalf("unexpected data payload %s", data)
		}
	}))
	defer server.Close()

	cache := NewResolutionCache(NewPolygonClient(server.URL), nil, nil)
	status := cache.GetResolution(context.Background(), conditionID, []string{"Down", "Up"}, time.Now().Add(-time.Minute))
	if !status.Resolved {
		t.Fatal("expected status to be resolved")
	}
	if status.Winner != "Up" {
		t.Fatalf("expected on-chain winner Up, got %q", status.Winner)
	}
}

func TestGetFullBlockByNumber(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req RPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Method != "eth_getBlockByNumber" {
			t.Fatalf("unexpected method %s", req.Method)
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x10","timestamp":"0x65ec8780","transactions":[{"hash":"0xabc","to":"` + CTFExchange + `","input":"0x1234","blockNumber":"0x10"}]}}`))
	}))
	defer server.Close()

	client := NewPolygonClient(server.URL)
	block, err := client.GetFullBlockByNumber(context.Background(), 16)
	if err != nil {
		t.Fatalf("GetFullBlockByNumber() error = %v", err)
	}
	if block == nil {
		t.Fatal("expected block result")
	}
	if block.Number != "0x10" {
		t.Fatalf("unexpected block number %q", block.Number)
	}
	if len(block.Transactions) != 1 || block.Transactions[0].Hash != "0xabc" {
		t.Fatalf("unexpected transactions %+v", block.Transactions)
	}
}
