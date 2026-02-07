package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// Polygon USDC contract address
const USDCContract = "0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174"

// Polygon CTF (Conditional Tokens Framework) contract address
const CTFContract = "0x4D97DCd97eC945f40cF65F87097ACe5EA0476045"

// PolygonClient handles Polygon RPC calls
type PolygonClient struct {
	RPCURL string
}

// NewPolygonClient creates a new Polygon client
func NewPolygonClient(rpcURL string) *PolygonClient {
	return &PolygonClient{
		RPCURL: rpcURL,
	}
}

// ... (existing code)

// IsMarketResolved checks if a market is resolved on-chain (FREE READ)
func (c *PolygonClient) IsMarketResolved(ctx context.Context, conditionID string) (bool, error) {
	// Function selector for payoutDenominator(bytes32): 0x1479831c
	id := strings.TrimPrefix(conditionID, "0x")
	data := "0x1479831c" + id

	callParams := map[string]string{
		"to":   CTFContract,
		"data": data,
	}

	result, err := c.call(ctx, "eth_call", []interface{}{callParams, "latest"})
	if err != nil {
		return false, err
	}

	var hexResult string
	if err := json.Unmarshal(result, &hexResult); err != nil {
		return false, err
	}

	denominator, err := parseHexBigInt(hexResult)
	if err != nil {
		return false, err
	}

	// If denominator > 0, the market has been resolved and payouts are reported
	return denominator.Cmp(big.NewInt(0)) > 0, nil
}

// RedeemPositions sends the on-chain transaction to redeem winning tokens (PAID WRITE)
func (c *PolygonClient) RedeemPositions(ctx context.Context, signer *Signer, conditionID string) (string, error) {
	// Function selector for redeemPositions(address,bytes32,bytes32,uint256[]): 0x01b7037c
	// Parameters:
	// 1. collateralToken (USDC): 0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174
	// 2. parentCollectionId: 0x0000000000000000000000000000000000000000000000000000000000000000
	// 3. conditionId: (provided)
	// 4. indexSets: [1, 2] for binary markets - redeems both outcomes (only winner pays out)

	collateral := "000000000000000000000000" + strings.TrimPrefix(strings.ToLower(USDCContract), "0x")
	parent := "0000000000000000000000000000000000000000000000000000000000000000"
	cond := strings.TrimPrefix(conditionID, "0x")

	// ABI encoding for indexSets [1, 2] (Dynamic array)
	// Offset to array (128 bytes = 4 * 32)
	offset := "0000000000000000000000000000000000000000000000000000000000000080"
	arrayLen := "0000000000000000000000000000000000000000000000000000000000000002"
	idx1 := "0000000000000000000000000000000000000000000000000000000000000001"
	idx2 := "0000000000000000000000000000000000000000000000000000000000000002"

	data := "0x01b7037c" + collateral + parent + cond + offset + arrayLen + idx1 + idx2

	// Get nonce and gas price
	nonce, err := c.GetNonce(ctx, signer.Address())
	if err != nil {
		return "", err
	}

	gasPrice, err := c.GetGasPrice(ctx)
	if err != nil {
		return "", err
	}

	// Sign transaction
	signedTx, err := signer.SignTransaction(nonce, CTFContract, big.NewInt(0), 200000, gasPrice, data)
	if err != nil {
		return "", err
	}

	// Send raw transaction
	return c.SendRawTransaction(ctx, signedTx)
}

// SplitPositions converts USDC into YES+NO tokens via CTF contract (PAID WRITE)
// This is the inverse of MergePositions - use to create inventory for panic selling.
// 1 USDC → 1 YES token + 1 NO token
// Use this to build inventory, then sell when bid_sum > $1.03 for profit.
func (c *PolygonClient) SplitPositions(ctx context.Context, signer *Signer, conditionID string, amount *big.Int) (string, error) {
	// Function selector for splitPosition(address,bytes32,bytes32,uint256[],uint256): 0x72ce4275
	// Parameters:
	// 1. collateralToken (USDC): 0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174
	// 2. parentCollectionId: 0x00...00 (null for Polymarket)
	// 3. conditionId: (provided)
	// 4. partition: [1, 2] for binary markets (YES/NO or Up/Down)
	// 5. amount: USDC amount to split (returns this many token pairs)

	collateral := "000000000000000000000000" + strings.TrimPrefix(strings.ToLower(USDCContract), "0x")
	parent := "0000000000000000000000000000000000000000000000000000000000000000"
	cond := strings.TrimPrefix(conditionID, "0x")

	// ABI encoding for partition [1, 2] (Dynamic array)
	// Offset to array data (160 bytes = 5 * 32, since amount is 5th param)
	offset := "00000000000000000000000000000000000000000000000000000000000000a0"
	// Amount (5th param) - pad to 32 bytes
	amtHex := fmt.Sprintf("%064x", amount)
	// Array: length=2, values=[1,2]
	arrayLen := "0000000000000000000000000000000000000000000000000000000000000002"
	idx1 := "0000000000000000000000000000000000000000000000000000000000000001"
	idx2 := "0000000000000000000000000000000000000000000000000000000000000002"

	data := "0x72ce4275" + collateral + parent + cond + offset + amtHex + arrayLen + idx1 + idx2

	// Get nonce and gas price
	nonce, err := c.GetNonce(ctx, signer.Address())
	if err != nil {
		return "", err
	}

	gasPrice, err := c.GetGasPrice(ctx)
	if err != nil {
		return "", err
	}

	// Sign transaction (200k gas limit should be plenty for split)
	signedTx, err := signer.SignTransaction(nonce, CTFContract, big.NewInt(0), 200000, gasPrice, data)
	if err != nil {
		return "", err
	}

	// Send raw transaction
	return c.SendRawTransaction(ctx, signedTx)
}

// MergePositions burns equal YES+NO tokens to get USDC back instantly (PAID WRITE)
// Unlike RedeemPositions, this works ANYTIME - no need to wait for market resolution.
// Use this immediately after buying both sides to capture arbitrage profit instantly.
func (c *PolygonClient) MergePositions(ctx context.Context, signer *Signer, conditionID string, amount *big.Int) (string, error) {
	// Function selector for mergePositions(address,bytes32,bytes32,uint256[],uint256): 0x9e7212ad
	// Parameters:
	// 1. collateralToken (USDC): 0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174
	// 2. parentCollectionId: 0x00...00 (null for Polymarket)
	// 3. conditionId: (provided)
	// 4. partition: [2, 1] for binary markets (index sets for NO, YES)
	// 5. amount: number of full sets to merge (returns this much USDC)

	collateral := "000000000000000000000000" + strings.TrimPrefix(strings.ToLower(USDCContract), "0x")
	parent := "0000000000000000000000000000000000000000000000000000000000000000"
	cond := strings.TrimPrefix(conditionID, "0x")

	// ABI encoding for partition [2, 1] (Dynamic array)
	// Offset to array data (160 bytes = 5 * 32, pointing past the 5 fixed params)
	offset := "00000000000000000000000000000000000000000000000000000000000000a0"
	// Amount (5th param) - pad to 32 bytes
	amtHex := fmt.Sprintf("%064x", amount)
	// Array: length=2, values=[2, 1] (NO index set first, YES index set second)
	arrayLen := "0000000000000000000000000000000000000000000000000000000000000002"
	idx1 := "0000000000000000000000000000000000000000000000000000000000000002"
	idx2 := "0000000000000000000000000000000000000000000000000000000000000001"

	data := "0x9e7212ad" + collateral + parent + cond + offset + amtHex + arrayLen + idx1 + idx2

	// Get nonce and gas price
	nonce, err := c.GetNonce(ctx, signer.Address())
	if err != nil {
		return "", err
	}

	gasPrice, err := c.GetGasPrice(ctx)
	if err != nil {
		return "", err
	}

	// Sign transaction (200k gas limit should be plenty for merge)
	signedTx, err := signer.SignTransaction(nonce, CTFContract, big.NewInt(0), 200000, gasPrice, data)
	if err != nil {
		return "", err
	}

	// Send raw transaction
	return c.SendRawTransaction(ctx, signedTx)
}

func (c *PolygonClient) GetNonce(ctx context.Context, address string) (uint64, error) {
	result, err := c.call(ctx, "eth_getTransactionCount", []interface{}{address, "latest"})
	if err != nil {
		return 0, err
	}
	var hexResult string
	json.Unmarshal(result, &hexResult)
	n, _ := parseHexBigInt(hexResult)
	return n.Uint64(), nil
}

func (c *PolygonClient) GetGasPrice(ctx context.Context) (*big.Int, error) {
	result, err := c.call(ctx, "eth_gasPrice", []interface{}{})
	if err != nil {
		return nil, err
	}
	var hexResult string
	json.Unmarshal(result, &hexResult)
	return parseHexBigInt(hexResult)
}

func (c *PolygonClient) SendRawTransaction(ctx context.Context, signedTx string) (string, error) {
	result, err := c.call(ctx, "eth_sendRawTransaction", []interface{}{signedTx})
	if err != nil {
		return "", err
	}
	var txHash string
	json.Unmarshal(result, &txHash)
	return txHash, nil
}

// TransactionReceipt represents the result of a mined transaction
type TransactionReceipt struct {
	Status      string `json:"status"`      // "0x1" = success, "0x0" = reverted
	BlockNumber string `json:"blockNumber"` // Block where tx was mined
	GasUsed     string `json:"gasUsed"`     // Actual gas consumed
	TxHash      string `json:"transactionHash"`
}

// GetTransactionReceipt fetches the receipt for a mined transaction
// Returns nil if transaction is still pending (not yet mined)
func (c *PolygonClient) GetTransactionReceipt(ctx context.Context, txHash string) (*TransactionReceipt, error) {
	result, err := c.call(ctx, "eth_getTransactionReceipt", []interface{}{txHash})
	if err != nil {
		return nil, err
	}

	// null result means transaction is still pending
	if string(result) == "null" {
		return nil, nil
	}

	var receipt TransactionReceipt
	if err := json.Unmarshal(result, &receipt); err != nil {
		return nil, fmt.Errorf("failed to parse receipt: %w", err)
	}

	return &receipt, nil
}

// WaitForTransaction polls for transaction confirmation until mined or timeout
// Returns (success, error) where success indicates if the tx executed successfully on-chain
func (c *PolygonClient) WaitForTransaction(ctx context.Context, txHash string) (bool, error) {
	// Poll every 1 second for up to context deadline
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	rpcErrors := 0
	const maxRPCErrors = 10

	for {
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("timeout waiting for transaction %s", txHash)
		case <-ticker.C:
			receipt, err := c.GetTransactionReceipt(ctx, txHash)
			if err != nil {
				// RPC error - keep trying up to a limit
				rpcErrors++
				if rpcErrors > maxRPCErrors {
					return false, fmt.Errorf("too many RPC errors (%d) waiting for tx %s: %w", rpcErrors, txHash, err)
				}
				continue
			}

			// Reset error counter on successful RPC call
			rpcErrors = 0

			if receipt == nil {
				// Still pending, keep waiting
				continue
			}

			// Transaction mined - check status
			// status: "0x1" = success, "0x0" = reverted
			if receipt.Status == "0x1" {
				return true, nil
			}

			return false, fmt.Errorf("transaction %s reverted on-chain", txHash)
		}
	}
}

// RPCRequest represents a JSON-RPC request
type RPCRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
	ID      int           `json:"id"`
}

// RPCResponse represents a JSON-RPC response
type RPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result"`
	Error   *RPCError       `json:"error,omitempty"`
	ID      int             `json:"id"`
}

// RPCError represents a JSON-RPC error
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// GetUSDCBalance returns the USDC balance for an address in human-readable format (6 decimals)
func (c *PolygonClient) GetUSDCBalance(ctx context.Context, address string) (float64, error) {
	// ERC20 balanceOf function selector: 0x70a08231
	// Followed by address padded to 32 bytes
	addr := strings.TrimPrefix(address, "0x")
	if len(addr) < 40 {
		addr = strings.Repeat("0", 40-len(addr)) + addr
	}
	data := "0x70a08231000000000000000000000000" + addr

	// Make eth_call
	callParams := map[string]string{
		"to":   USDCContract,
		"data": data,
	}

	result, err := c.call(ctx, "eth_call", []interface{}{callParams, "latest"})
	if err != nil {
		return 0, fmt.Errorf("failed to get USDC balance: %w", err)
	}

	// Parse hex result
	var hexResult string
	if err := json.Unmarshal(result, &hexResult); err != nil {
		return 0, fmt.Errorf("failed to parse balance result: %w", err)
	}

	balance, err := parseHexBigInt(hexResult)
	if err != nil {
		return 0, fmt.Errorf("failed to parse balance hex: %w", err)
	}

	// Convert from 6 decimal places to float
	balanceFloat := new(big.Float).SetInt(balance)
	divisor := new(big.Float).SetInt64(1e6)
	balanceFloat.Quo(balanceFloat, divisor)

	result64, _ := balanceFloat.Float64()
	return result64, nil
}

// ApproveUSDC grants allowance to the Polymarket Exchange contract to spend USDC (PAID WRITE)
func (c *PolygonClient) ApproveUSDC(ctx context.Context, signer *Signer, spender string, amount *big.Int) (string, error) {
	// Function selector for approve(address,uint256): 0x095ea7b3
	// Parameters:
	// 1. spender: (provided)
	// 2. amount: (provided)

	spenderAddr := "000000000000000000000000" + strings.TrimPrefix(strings.ToLower(spender), "0x")
	amtHex := fmt.Sprintf("%064x", amount)

	data := "0x095ea7b3" + spenderAddr + amtHex

	// Get nonce and gas price
	nonce, err := c.GetNonce(ctx, signer.Address())
	if err != nil {
		return "", err
	}

	gasPrice, err := c.GetGasPrice(ctx)
	if err != nil {
		return "", err
	}

	// Sign transaction
	signedTx, err := signer.SignTransaction(nonce, USDCContract, big.NewInt(0), 100000, gasPrice, data)
	if err != nil {
		return "", err
	}

	// Send raw transaction
	return c.SendRawTransaction(ctx, signedTx)
}

// ApproveCTF grants allowance for Conditional Tokens (ERC1155) (PAID WRITE)
func (c *PolygonClient) ApproveCTF(ctx context.Context, signer *Signer, spender string, approved bool) (string, error) {
	// Function selector for setApprovalForAll(address,bool): 0xa22cb465
	// Parameters:
	// 1. operator: (spender)
	// 2. approved: (true/false)

	operator := "000000000000000000000000" + strings.TrimPrefix(strings.ToLower(spender), "0x")
	
	val := "0000000000000000000000000000000000000000000000000000000000000001"
	if !approved {
		val = "0000000000000000000000000000000000000000000000000000000000000000"
	}

	data := "0x22cb465" // Note: the selector is shorter if we don't pad correctly, but we'll use standard 8 chars
	// Correct selector for setApprovalForAll is 0xa22cb465
	data = "0xa22cb465" + operator + val

	// Get nonce and gas price
	nonce, err := c.GetNonce(ctx, signer.Address())
	if err != nil {
		return "", err
	}

	gasPrice, err := c.GetGasPrice(ctx)
	if err != nil {
		return "", err
	}

	// Sign transaction
	signedTx, err := signer.SignTransaction(nonce, CTFContract, big.NewInt(0), 100000, gasPrice, data)
	if err != nil {
		return "", err
	}

	// Send raw transaction
	return c.SendRawTransaction(ctx, signedTx)
}

// GetCTFBalance returns the balance of a specific Conditional Token (ERC1155)
func (c *PolygonClient) GetCTFBalance(ctx context.Context, address string, tokenID *big.Int) (*big.Int, error) {
	// ERC1155 balanceOf(address,uint256) function selector: 0x00fdd58e
	// Parameters:
	// 1. account: address (padded to 32 bytes)
	// 2. id: tokenID (padded to 32 bytes)

	account := "000000000000000000000000" + strings.TrimPrefix(strings.ToLower(address), "0x")
	idHex := fmt.Sprintf("%064x", tokenID)

	data := "0x00fdd58e" + account + idHex

	callParams := map[string]string{
		"to":   CTFContract,
		"data": data,
	}

	result, err := c.call(ctx, "eth_call", []interface{}{callParams, "latest"})
	if err != nil {
		return nil, fmt.Errorf("failed to get CTF balance: %w", err)
	}

	var hexResult string
	if err := json.Unmarshal(result, &hexResult); err != nil {
		return nil, fmt.Errorf("failed to parse balance result: %w", err)
	}

	return parseHexBigInt(hexResult)
}

// GetMATICBalance returns the native MATIC balance for an address
func (c *PolygonClient) GetMATICBalance(ctx context.Context, address string) (float64, error) {
	result, err := c.call(ctx, "eth_getBalance", []interface{}{address, "latest"})
	if err != nil {
		return 0, fmt.Errorf("failed to get MATIC balance: %w", err)
	}

	var hexResult string
	if err := json.Unmarshal(result, &hexResult); err != nil {
		return 0, fmt.Errorf("failed to parse balance result: %w", err)
	}

	balance, err := parseHexBigInt(hexResult)
	if err != nil {
		return 0, fmt.Errorf("failed to parse balance hex: %w", err)
	}

	// Convert from 18 decimal places to float
	balanceFloat := new(big.Float).SetInt(balance)
	divisor := new(big.Float).SetInt64(1e18)
	balanceFloat.Quo(balanceFloat, divisor)

	result64, _ := balanceFloat.Float64()
	return result64, nil
}

// GetBlockNumber returns the current block number
func (c *PolygonClient) GetBlockNumber(ctx context.Context) (uint64, error) {
	result, err := c.call(ctx, "eth_blockNumber", []interface{}{})
	if err != nil {
		return 0, fmt.Errorf("failed to get block number: %w", err)
	}

	var hexResult string
	if err := json.Unmarshal(result, &hexResult); err != nil {
		return 0, fmt.Errorf("failed to parse block number: %w", err)
	}

	blockNum, err := parseHexBigInt(hexResult)
	if err != nil {
		return 0, err
	}

	return blockNum.Uint64(), nil
}

// call makes a JSON-RPC call
func (c *PolygonClient) call(ctx context.Context, method string, params []interface{}) (json.RawMessage, error) {
	reqBody := RPCRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
		ID:      1,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.RPCURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var rpcResp RPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return nil, fmt.Errorf("failed to decode RPC response: %w", err)
	}

	if rpcResp.Error != nil {
		return nil, fmt.Errorf("RPC error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	return rpcResp.Result, nil
}

// parseHexBigInt parses a hex string to big.Int
func parseHexBigInt(s string) (*big.Int, error) {
	s = strings.TrimPrefix(s, "0x")
	if s == "" {
		s = "0"
	}

	b, err := hex.DecodeString(s)
	if err != nil {
		// Try parsing directly as hex string
		n := new(big.Int)
		n.SetString(s, 16)
		return n, nil
	}

	return new(big.Int).SetBytes(b), nil
}
