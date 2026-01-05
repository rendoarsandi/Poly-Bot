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
)

// Polygon USDC contract address
const USDCContract = "0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174"

// PolygonClient handles Polygon RPC calls
type PolygonClient struct {
	RPCURL string
}

// NewPolygonClient creates a new Polygon RPC client
func NewPolygonClient(rpcURL string) *PolygonClient {
	if rpcURL == "" {
		rpcURL = "https://polygon-rpc.com"
	}
	return &PolygonClient{RPCURL: rpcURL}
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
