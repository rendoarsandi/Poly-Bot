package api

import (
	"bytes"
	"compress/gzip"
	"context"

	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	"Market-bot/internal/core"
)

// Polygon USDC contract address
const USDCContract = "0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174"

// Polygon CTF (Conditional Tokens Framework) contract address
const CTFContract = "0x4D97DCd97eC945f40cF65F87097ACe5EA0476045"

// Polygon CTF Exchange (Legacy/Binary) contract address
const CTFExchange = "0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E"

// Polygon Negative Risk Exchange (Multi-outcome) contract address
const NegRiskExchange = "0xC5d563A36AE78145C45a50134d48A1215220f80a"

// Polymarket Exchange Router (used by web UI for matching)
const RouterExchange = "0xE3f18aCc55091E2C48d883fc8C8413319D4aB7b0"

const (
	polygonInitialReceiptPollInterval    = 2 * time.Second
	polygonMaxReceiptPollInterval        = 5 * time.Second
	polygonTimeoutStatusProbeTimeout     = 3 * time.Second
	polygonGasPriceBumpNumerator         = 15
	polygonGasPriceBumpDenominator       = 10
	polygonBaseFeeMultiplier             = 2
	polygonFastGasPriceBumpNumerator     = 17
	polygonFastGasPriceBumpDenominator   = 10
	polygonFastBaseFeeMultiplier         = 2
	polygonUrgentGasPriceBumpNumerator   = 20
	polygonUrgentGasPriceBumpDenominator = 10
	polygonUrgentBaseFeeMultiplier       = 3
	payoutDenominatorSelector            = "0xdd34de67"
	payoutNumeratorsSelector             = "0x0504c814"
)

var (
	polygonFastMinPriorityFeePerGas   = big.NewInt(20_000_000_000) // 20 gwei
	polygonUrgentMinPriorityFeePerGas = big.NewInt(40_000_000_000) // 40 gwei
)

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
	// Function selector for payoutDenominator(bytes32): 0xdd34de67
	id := strings.TrimPrefix(conditionID, "0x")
	if len(id) != 64 {
		return false, fmt.Errorf("invalid condition ID length: %d", len(id))
	}
	data := payoutDenominatorSelector + id

	callParams := map[string]string{
		"to":   CTFContract,
		"data": data,
	}

	result, err := c.call(ctx, "eth_call", []interface{}{callParams, "latest"})
	if err != nil {
		// If it reverts, it likely means the market is not resolved yet or payouts aren't reported
		if strings.Contains(strings.ToLower(err.Error()), "revert") {
			return false, nil
		}
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

// GetWinningOutcome decodes the winning outcome from on-chain payout numerators.
// It returns an empty string when the market is resolved but the winner is ambiguous.
func (c *PolygonClient) GetWinningOutcome(ctx context.Context, conditionID string, outcomes []string) (string, error) {
	if len(outcomes) == 0 {
		return "", nil
	}
	id := strings.TrimPrefix(conditionID, "0x")
	if len(id) != 64 {
		return "", fmt.Errorf("invalid condition ID length: %d", len(id))
	}

	maxNumerator := big.NewInt(0)
	maxIndex := -1
	positiveCount := 0

	for i, outcome := range outcomes {
		numerator, err := c.getPayoutNumerator(ctx, id, i)
		if err != nil {
			return "", err
		}
		if numerator.Sign() <= 0 {
			continue
		}
		positiveCount++
		if numerator.Cmp(maxNumerator) > 0 {
			maxNumerator = numerator
			maxIndex = i
		} else if numerator.Cmp(maxNumerator) == 0 {
			maxIndex = -1
		}
		_ = outcome
	}

	if positiveCount == 1 && maxIndex >= 0 && maxIndex < len(outcomes) {
		return outcomes[maxIndex], nil
	}
	if maxIndex >= 0 && maxIndex < len(outcomes) {
		return outcomes[maxIndex], nil
	}
	return "", nil
}

func (c *PolygonClient) getPayoutNumerator(ctx context.Context, conditionIDNoPrefix string, index int) (*big.Int, error) {
	if index < 0 {
		return nil, fmt.Errorf("invalid payout numerator index: %d", index)
	}
	data := payoutNumeratorsSelector + conditionIDNoPrefix + fmt.Sprintf("%064x", index)
	callParams := map[string]string{
		"to":   CTFContract,
		"data": data,
	}
	result, err := c.call(ctx, "eth_call", []interface{}{callParams, "latest"})
	if err != nil {
		return nil, err
	}

	var hexResult string
	if err := json.Unmarshal(result, &hexResult); err != nil {
		return nil, err
	}
	return parseHexBigInt(hexResult)
}

// generateIndexSetsHex creates the ABI encoded dynamic array of index sets
// For N outcomes, it generates an array where the i-th element is 1 << i
func generateIndexSetsHex(numOutcomes int) string {
	arrayLenHex := fmt.Sprintf("%064x", numOutcomes)
	data := arrayLenHex
	for i := 0; i < numOutcomes; i++ {
		val := 1 << i
		data += fmt.Sprintf("%064x", val)
	}
	return data
}

// RedeemPositions sends the on-chain transaction to redeem winning tokens (PAID WRITE)
func (c *PolygonClient) RedeemPositions(ctx context.Context, signer *Signer, conditionID string, numOutcomes int) (string, error) {
	// Function selector for redeemPositions(address,bytes32,bytes32,uint256[]): 0x01b7037c
	// Parameters:
	// 1. collateralToken (USDC): 0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174
	// 2. parentCollectionId: 0x0000000000000000000000000000000000000000000000000000000000000000
	// 3. conditionId: (provided)
	// 4. indexSets: dynamic array of index sets (only winner pays out)

	collateral := "000000000000000000000000" + strings.TrimPrefix(strings.ToLower(USDCContract), "0x")
	parent := "0000000000000000000000000000000000000000000000000000000000000000"
	cond := strings.TrimPrefix(conditionID, "0x")

	// ABI encoding for indexSets (Dynamic array)
	// Offset to array (128 bytes = 4 * 32)
	offset := "0000000000000000000000000000000000000000000000000000000000000080"
	indexSetsData := generateIndexSetsHex(numOutcomes)

	data := "0x01b7037c" + collateral + parent + cond + offset + indexSetsData
	return c.signAndSendWriteTransaction(ctx, signer, CTFContract, big.NewInt(0), 350000, data)
}

// RedeemPositionsFast submits redeemPositions with a moderate gas profile.
func (c *PolygonClient) RedeemPositionsFast(ctx context.Context, signer *Signer, conditionID string, numOutcomes int) (string, error) {
	collateral := "000000000000000000000000" + strings.TrimPrefix(strings.ToLower(USDCContract), "0x")
	parent := "0000000000000000000000000000000000000000000000000000000000000000"
	cond := strings.TrimPrefix(conditionID, "0x")
	offset := "0000000000000000000000000000000000000000000000000000000000000080"
	indexSetsData := generateIndexSetsHex(numOutcomes)

	data := "0x01b7037c" + collateral + parent + cond + offset + indexSetsData
	return c.signAndSendFastWriteTransaction(ctx, signer, CTFContract, big.NewInt(0), 350000, data)
}

// RedeemPositionsUrgent submits the same redeem call with a more aggressive
// gas policy so resolved-market payouts clear faster.
func (c *PolygonClient) RedeemPositionsUrgent(ctx context.Context, signer *Signer, conditionID string, numOutcomes int) (string, error) {
	collateral := "000000000000000000000000" + strings.TrimPrefix(strings.ToLower(USDCContract), "0x")
	parent := "0000000000000000000000000000000000000000000000000000000000000000"
	cond := strings.TrimPrefix(conditionID, "0x")
	offset := "0000000000000000000000000000000000000000000000000000000000000080"
	indexSetsData := generateIndexSetsHex(numOutcomes)

	data := "0x01b7037c" + collateral + parent + cond + offset + indexSetsData
	return c.signAndSendUrgentWriteTransaction(ctx, signer, CTFContract, big.NewInt(0), 350000, data)
}

// RedeemPositionsWithGasMode submits redeemPositions using normal, fast, or
// urgent gas settings.
func (c *PolygonClient) RedeemPositionsWithGasMode(ctx context.Context, signer *Signer, conditionID string, numOutcomes int, gasMode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(gasMode)) {
	case core.RedeemGasModeNormal:
		return c.RedeemPositions(ctx, signer, conditionID, numOutcomes)
	case core.RedeemGasModeUrgent:
		return c.RedeemPositionsUrgent(ctx, signer, conditionID, numOutcomes)
	default:
		return c.RedeemPositionsFast(ctx, signer, conditionID, numOutcomes)
	}
}

// SplitPositions converts USDC into YES+NO tokens via CTF contract (PAID WRITE)
// This is the inverse of MergePositions - use to create inventory for panic selling.
// 1 USDC → 1 YES token + 1 NO token
// Use this to build inventory, then sell when bid_sum > $1.03 for profit.
func (c *PolygonClient) SplitPositions(ctx context.Context, signer *Signer, conditionID string, amount *big.Int, numOutcomes int) (string, error) {
	// Function selector for splitPosition(address,bytes32,bytes32,uint256[],uint256): 0x72ce4275
	// Parameters:
	// 1. collateralToken (USDC): 0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174
	// 2. parentCollectionId: 0x00...00 (null for Polymarket)
	// 3. conditionId: (provided)
	// 4. partition: dynamic array of index sets
	// 5. amount: USDC amount to split (returns this many token pairs)

	collateral := "000000000000000000000000" + strings.TrimPrefix(strings.ToLower(USDCContract), "0x")
	parent := "0000000000000000000000000000000000000000000000000000000000000000"
	cond := strings.TrimPrefix(conditionID, "0x")

	// ABI encoding for partition (Dynamic array)
	// Offset to array data (160 bytes = 5 * 32, since amount is 5th param)
	offset := "00000000000000000000000000000000000000000000000000000000000000a0"
	// Amount (5th param) - pad to 32 bytes
	amtHex := fmt.Sprintf("%064x", amount)

	indexSetsData := generateIndexSetsHex(numOutcomes)

	data := "0x72ce4275" + collateral + parent + cond + offset + amtHex + indexSetsData
	return c.signAndSendWriteTransaction(ctx, signer, CTFContract, big.NewInt(0), 200000, data)
}

// MergePositions burns equal YES+NO tokens to get USDC back instantly (PAID WRITE)
// Unlike RedeemPositions, this works ANYTIME - no need to wait for market resolution.
// Use this immediately after buying both sides to capture arbitrage profit instantly.
func (c *PolygonClient) MergePositions(ctx context.Context, signer *Signer, conditionID string, amount *big.Int, numOutcomes int) (string, error) {
	// Function selector for mergePositions(address,bytes32,bytes32,uint256[],uint256): 0x9e7212ad
	// Parameters:
	// 1. collateralToken (USDC): 0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174
	// 2. parentCollectionId: 0x00...00 (null for Polymarket)
	// 3. conditionId: (provided)
	// 4. partition: dynamic array of index sets
	// 5. amount: number of full sets to merge (returns this much USDC)

	collateral := "000000000000000000000000" + strings.TrimPrefix(strings.ToLower(USDCContract), "0x")
	parent := "0000000000000000000000000000000000000000000000000000000000000000"
	cond := strings.TrimPrefix(conditionID, "0x")

	// ABI encoding for partition (Dynamic array)
	// Offset to array data (160 bytes = 5 * 32, pointing past the 5 fixed params)
	offset := "00000000000000000000000000000000000000000000000000000000000000a0"
	// Amount (5th param) - pad to 32 bytes
	amtHex := fmt.Sprintf("%064x", amount)

	indexSetsData := generateIndexSetsHex(numOutcomes)

	data := "0x9e7212ad" + collateral + parent + cond + offset + amtHex + indexSetsData
	return c.signAndSendWriteTransaction(ctx, signer, CTFContract, big.NewInt(0), 200000, data)
}

func (c *PolygonClient) GetNonce(ctx context.Context, address string) (uint64, error) {
	result, err := c.call(ctx, "eth_getTransactionCount", []interface{}{address, "pending"})
	if err != nil {
		return 0, err
	}
	var hexResult string
	_ = json.Unmarshal(result, &hexResult)
	n, _ := parseHexBigInt(hexResult)
	return n.Uint64(), nil
}

func (c *PolygonClient) GetGasPrice(ctx context.Context) (*big.Int, error) {
	result, err := c.call(ctx, "eth_gasPrice", []interface{}{})
	if err != nil {
		return nil, err
	}
	var hexResult string
	_ = json.Unmarshal(result, &hexResult)
	return parseHexBigInt(hexResult)
}

func (c *PolygonClient) GetMaxPriorityFeePerGas(ctx context.Context) (*big.Int, error) {
	result, err := c.call(ctx, "eth_maxPriorityFeePerGas", []interface{}{})
	if err != nil {
		return nil, err
	}
	var hexResult string
	_ = json.Unmarshal(result, &hexResult)
	return parseHexBigInt(hexResult)
}

type writeTxFees struct {
	LegacyGasPrice       *big.Int
	MaxFeePerGas         *big.Int
	MaxPriorityFeePerGas *big.Int
}

func (f writeTxFees) UseDynamic() bool {
	return f.MaxFeePerGas != nil && f.MaxPriorityFeePerGas != nil && f.MaxFeePerGas.Sign() > 0 && f.MaxPriorityFeePerGas.Sign() > 0
}

func (c *PolygonClient) gasFeesForWriteTx(ctx context.Context) (writeTxFees, error) {
	return c.gasFeesForWriteTxMode(ctx, core.RedeemGasModeNormal)
}

func (c *PolygonClient) fastGasFeesForWriteTx(ctx context.Context) (writeTxFees, error) {
	return c.gasFeesForWriteTxMode(ctx, core.RedeemGasModeFast)
}

func (c *PolygonClient) urgentGasFeesForWriteTx(ctx context.Context) (writeTxFees, error) {
	return c.gasFeesForWriteTxMode(ctx, core.RedeemGasModeUrgent)
}

func (c *PolygonClient) gasFeesForWriteTxMode(ctx context.Context, gasMode string) (writeTxFees, error) {
	gasPrice, err := c.GetGasPrice(ctx)
	if err != nil {
		return writeTxFees{}, err
	}
	legacyBumpNum := int64(polygonGasPriceBumpNumerator)
	legacyBumpDen := int64(polygonGasPriceBumpDenominator)
	baseFeeMultiplier := int64(polygonBaseFeeMultiplier)
	minPriorityFee := (*big.Int)(nil)
	switch strings.ToLower(strings.TrimSpace(gasMode)) {
	case core.RedeemGasModeFast:
		legacyBumpNum = polygonFastGasPriceBumpNumerator
		legacyBumpDen = polygonFastGasPriceBumpDenominator
		baseFeeMultiplier = polygonFastBaseFeeMultiplier
		minPriorityFee = polygonFastMinPriorityFeePerGas
	case core.RedeemGasModeUrgent:
		legacyBumpNum = polygonUrgentGasPriceBumpNumerator
		legacyBumpDen = polygonUrgentGasPriceBumpDenominator
		baseFeeMultiplier = polygonUrgentBaseFeeMultiplier
		minPriorityFee = polygonUrgentMinPriorityFeePerGas
	}

	fees := writeTxFees{
		LegacyGasPrice: bumpGasPriceWithRatio(gasPrice, legacyBumpNum, legacyBumpDen),
	}

	priorityFee, err := c.GetMaxPriorityFeePerGas(ctx)
	if err != nil || priorityFee == nil || priorityFee.Sign() <= 0 {
		return fees, nil
	}

	baseFee, err := c.GetBlockBaseFee(ctx)
	if err != nil || baseFee == nil || baseFee.Sign() <= 0 {
		return fees, nil
	}

	bumpedPriority := bumpGasPriceWithRatio(priorityFee, legacyBumpNum, legacyBumpDen)
	if minPriorityFee != nil && bumpedPriority.Cmp(minPriorityFee) < 0 {
		bumpedPriority = new(big.Int).Set(minPriorityFee)
	}
	maxFee := new(big.Int).Mul(baseFee, big.NewInt(baseFeeMultiplier))
	maxFee.Add(maxFee, bumpedPriority)
	if fees.LegacyGasPrice != nil && maxFee.Cmp(fees.LegacyGasPrice) < 0 {
		maxFee = new(big.Int).Set(fees.LegacyGasPrice)
	}

	fees.MaxPriorityFeePerGas = bumpedPriority
	fees.MaxFeePerGas = maxFee
	return fees, nil
}

func (c *PolygonClient) signAndSendWriteTransaction(ctx context.Context, signer *Signer, to string, value *big.Int, gasLimit uint64, data string) (string, error) {
	nonce, err := c.GetNonce(ctx, signer.Address())
	if err != nil {
		return "", err
	}

	fees, err := c.gasFeesForWriteTx(ctx)
	if err != nil {
		return "", err
	}
	return c.signAndSendWriteTransactionWithFees(ctx, signer, nonce, to, value, gasLimit, data, fees)
}

func (c *PolygonClient) signAndSendFastWriteTransaction(ctx context.Context, signer *Signer, to string, value *big.Int, gasLimit uint64, data string) (string, error) {
	nonce, err := c.GetNonce(ctx, signer.Address())
	if err != nil {
		return "", err
	}

	fees, err := c.fastGasFeesForWriteTx(ctx)
	if err != nil {
		return "", err
	}
	return c.signAndSendWriteTransactionWithFees(ctx, signer, nonce, to, value, gasLimit, data, fees)
}

func (c *PolygonClient) signAndSendUrgentWriteTransaction(ctx context.Context, signer *Signer, to string, value *big.Int, gasLimit uint64, data string) (string, error) {
	nonce, err := c.GetNonce(ctx, signer.Address())
	if err != nil {
		return "", err
	}

	fees, err := c.urgentGasFeesForWriteTx(ctx)
	if err != nil {
		return "", err
	}
	return c.signAndSendWriteTransactionWithFees(ctx, signer, nonce, to, value, gasLimit, data, fees)
}

func (c *PolygonClient) signAndSendWriteTransactionWithFees(ctx context.Context, signer *Signer, nonce uint64, to string, value *big.Int, gasLimit uint64, data string, fees writeTxFees) (string, error) {
	var signedTx string
	var err error
	if fees.UseDynamic() {
		signedTx, err = signer.SignDynamicFeeTransaction(nonce, to, value, gasLimit, fees.MaxFeePerGas, fees.MaxPriorityFeePerGas, data)
	} else {
		signedTx, err = signer.SignTransaction(nonce, to, value, gasLimit, fees.LegacyGasPrice, data)
	}
	if err != nil {
		return "", err
	}

	return c.SendRawTransaction(ctx, signedTx)
}

func (c *PolygonClient) SendRawTransaction(ctx context.Context, signedTx string) (string, error) {
	result, err := c.call(ctx, "eth_sendRawTransaction", []interface{}{signedTx})
	if err != nil {
		return "", err
	}
	var txHash string
	_ = json.Unmarshal(result, &txHash)
	return txHash, nil
}

// TransactionReceipt represents the result of a mined transaction
type TransactionLog struct {
	Address string   `json:"address"`
	Topics  []string `json:"topics"`
	Data    string   `json:"data"`
}

type TransactionReceipt struct {
	Status      string           `json:"status"`      // "0x1" = success, "0x0" = reverted
	BlockNumber string           `json:"blockNumber"` // Block where tx was mined
	GasUsed     string           `json:"gasUsed"`     // Actual gas consumed
	TxHash      string           `json:"transactionHash"`
	Logs        []TransactionLog `json:"logs"`
}

type Transaction struct {
	Hash        string `json:"hash"`
	BlockNumber string `json:"blockNumber"`
	To          string `json:"to"`
	From        string `json:"from"`
	Input       string `json:"input"`
}

type FullBlockTransaction struct {
	Hash        string `json:"hash"`
	From        string `json:"from"`
	To          string `json:"to"`
	Input       string `json:"input"`
	BlockNumber string `json:"blockNumber"`
}

type Block struct {
	BaseFeePerGas string `json:"baseFeePerGas"`
}

type FullBlock struct {
	Number       string                 `json:"number"`
	Timestamp    string                 `json:"timestamp"`
	Transactions []FullBlockTransaction `json:"transactions"`
}

func (c *PolygonClient) GetBlockBaseFee(ctx context.Context) (*big.Int, error) {
	for _, blockTag := range []string{"pending", "latest"} {
		result, err := c.call(ctx, "eth_getBlockByNumber", []interface{}{blockTag, false})
		if err != nil {
			if blockTag == "latest" {
				return nil, err
			}
			continue
		}
		if string(result) == "null" {
			continue
		}

		var block Block
		if err := json.Unmarshal(result, &block); err != nil {
			return nil, fmt.Errorf("failed to parse block: %w", err)
		}
		if strings.TrimSpace(block.BaseFeePerGas) == "" || block.BaseFeePerGas == "0x" {
			continue
		}
		return parseHexBigInt(block.BaseFeePerGas)
	}
	return nil, fmt.Errorf("base fee unavailable from RPC")
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

// GetTransactionByHash fetches the transaction if it is still known by the node.
// Returns nil if the tx is unknown/dropped.
func (c *PolygonClient) GetTransactionByHash(ctx context.Context, txHash string) (*Transaction, error) {
	result, err := c.call(ctx, "eth_getTransactionByHash", []interface{}{txHash})
	if err != nil {
		return nil, err
	}
	if string(result) == "null" {
		return nil, nil
	}
	var tx Transaction
	if err := json.Unmarshal(result, &tx); err != nil {
		return nil, fmt.Errorf("failed to parse transaction: %w", err)
	}
	return &tx, nil
}

// WaitForTransaction polls for transaction confirmation until mined or timeout
// Returns (success, error) where success indicates if the tx executed successfully on-chain
func (c *PolygonClient) WaitForTransaction(ctx context.Context, txHash string) (bool, error) {
	rpcErrors := 0
	const maxRPCErrors = 10
	pollInterval := polygonInitialReceiptPollInterval
	timer := time.NewTimer(pollInterval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			status := c.describePendingTransaction(txHash)
			return false, fmt.Errorf("timeout waiting for transaction %s (%s)", txHash, status)
		case <-timer.C:
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
				pollInterval = nextReceiptPollInterval(pollInterval)
				resetTimer(timer, pollInterval)
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
	Error   json.RawMessage `json:"error,omitempty"`
	ID      int             `json:"id"`
}

// RPCError represents a JSON-RPC error
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// rpcError extracts an RPCError from the raw error field, handling both
// object format {"code":...,"message":...} and plain string format.
func rpcError(raw json.RawMessage) *RPCError {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	// Try object first
	var obj RPCError
	if err := json.Unmarshal(raw, &obj); err == nil && (obj.Code != 0 || obj.Message != "") {
		return &obj
	}
	// Fall back to plain string
	var msg string
	if err := json.Unmarshal(raw, &msg); err == nil && msg != "" {
		return &RPCError{Code: -1, Message: msg}
	}
	// Last resort: treat raw bytes as message
	return &RPCError{Code: -1, Message: string(raw)}
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

// GetUSDCAllowance returns the current allowance for a spender to use an owner's USDC
func (c *PolygonClient) GetUSDCAllowance(ctx context.Context, owner, spender string) (*big.Int, error) {
	// ERC20 allowance(address,address) function selector: 0xdd62ed3e
	ownerAddr := "000000000000000000000000" + strings.TrimPrefix(strings.ToLower(owner), "0x")
	spenderAddr := "000000000000000000000000" + strings.TrimPrefix(strings.ToLower(spender), "0x")

	data := "0xdd62ed3e" + ownerAddr + spenderAddr

	callParams := map[string]string{
		"to":   USDCContract,
		"data": data,
	}

	result, err := c.call(ctx, "eth_call", []interface{}{callParams, "latest"})
	if err != nil {
		return nil, fmt.Errorf("failed to get USDC allowance: %w", err)
	}

	var hexResult string
	if err := json.Unmarshal(result, &hexResult); err != nil {
		return nil, fmt.Errorf("failed to parse allowance result: %w", err)
	}

	return parseHexBigInt(hexResult)
}

// IsCTFApproved checks if a spender is approved for all of an owner's CTF tokens
func (c *PolygonClient) IsCTFApproved(ctx context.Context, owner, operator string) (bool, error) {
	// ERC1155 isApprovedForAll(address,address) function selector: 0xe985e9c5
	ownerAddr := "000000000000000000000000" + strings.TrimPrefix(strings.ToLower(owner), "0x")
	operatorAddr := "000000000000000000000000" + strings.TrimPrefix(strings.ToLower(operator), "0x")

	data := "0xe985e9c5" + ownerAddr + operatorAddr

	callParams := map[string]string{
		"to":   CTFContract,
		"data": data,
	}

	result, err := c.call(ctx, "eth_call", []interface{}{callParams, "latest"})
	if err != nil {
		return false, fmt.Errorf("failed to check CTF approval: %w", err)
	}

	var hexResult string
	if err := json.Unmarshal(result, &hexResult); err != nil {
		return false, fmt.Errorf("failed to parse approval result: %w", err)
	}

	val, err := parseHexBigInt(hexResult)
	if err != nil {
		return false, err
	}

	return val.Cmp(big.NewInt(0)) > 0, nil
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
	return c.signAndSendWriteTransaction(ctx, signer, USDCContract, big.NewInt(0), 100000, data)
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

	// Correct selector for setApprovalForAll is 0xa22cb465
	data := "0xa22cb465" + operator + val
	return c.signAndSendWriteTransaction(ctx, signer, CTFContract, big.NewInt(0), 100000, data)
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

func (c *PolygonClient) GetFullBlockByNumber(ctx context.Context, blockNumber uint64) (*FullBlock, error) {
	blockTag := fmt.Sprintf("0x%x", blockNumber)

	// Create a specialized struct to unmarshal the whole response in one go
	var response struct {
		JSONRPC string     `json:"jsonrpc"`
		Result  *FullBlock `json:"result"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}

	// Use a new internal method that returns raw body for better debugging
	body, err := c.callRaw(ctx, "eth_getBlockByNumber", []interface{}{blockTag, true})
	if err != nil {
		return nil, fmt.Errorf("failed to get full block %d: %w", blockNumber, err)
	}

	if err := json.Unmarshal(body, &response); err != nil {
		// Dump failed block to disk for debugging
		dumpPath := fmt.Sprintf("failed_block_%d.json", blockNumber)
		_ = os.WriteFile(dumpPath, body, 0644)
		return nil, fmt.Errorf("failed to parse full block (dumped to %s): %w", dumpPath, err)
	}

	if response.Error.Code != 0 || response.Error.Message != "" {
		return nil, fmt.Errorf("RPC error %d: %s", response.Error.Code, response.Error.Message)
	}

	return response.Result, nil
}

// callRaw is like call but returns the raw response body
func (c *PolygonClient) callRaw(ctx context.Context, method string, params []interface{}) ([]byte, error) {
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
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var reader io.ReadCloser = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		var err error
		reader, err = gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to create gzip reader: %w", err)
		}
		defer reader.Close()
	}

	bodyBytes, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read RPC response body (status %d): %w", resp.StatusCode, err)
	}

	if len(bodyBytes) == 0 {
		return nil, fmt.Errorf("empty RPC response body (status %d)", resp.StatusCode)
	}

	return bodyBytes, nil
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
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var reader io.ReadCloser = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		var err error
		reader, err = gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to create gzip reader: %w", err)
		}
		defer reader.Close()
	}

	bodyBytes, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read RPC response body (status %d): %w", resp.StatusCode, err)
	}

	if len(bodyBytes) == 0 {
		return nil, fmt.Errorf("empty RPC response body (status %d)", resp.StatusCode)
	}

	var rpcResp RPCResponse
	if err := json.Unmarshal(bodyBytes, &rpcResp); err != nil {
		snippet := string(bodyBytes)
		if len(snippet) > 100 {
			snippet = snippet[len(snippet)-100:]
		}
		return nil, fmt.Errorf("failed to unmarshal RPC response (status %d, len %d): %w | tail: %s", resp.StatusCode, len(bodyBytes), err, snippet)
	}

	if rpcErr := rpcError(rpcResp.Error); rpcErr != nil {
		return nil, fmt.Errorf("RPC error %d: %s", rpcErr.Code, rpcErr.Message)
	}

	// If the result field is entirely missing (nil), it's an invalid response.
	// But if it's present as "null", json.RawMessage will be []byte("null") or nil depending on decoder.
	// We want to allow valid "null" results for callers to handle (e.g. block not found).
	if rpcResp.Result == nil && rpcResp.Error == nil {
		// Only error if we have no result AND no error object at all
		// This handles the case where the JSON-RPC response is totally malformed.
	}

	return rpcResp.Result, nil
}

// parseHexBigInt parses a hex string to big.Int
func parseHexBigInt(s string) (*big.Int, error) {
	s = strings.TrimPrefix(s, "0x")
	if s == "" {
		return big.NewInt(0), nil
	}

	n := new(big.Int)
	if _, ok := n.SetString(s, 16); !ok {
		return nil, fmt.Errorf("failed to parse hex string: %s", s)
	}
	return n, nil
}

func parseHexUint64(s string) (uint64, error) {
	n, err := parseHexBigInt(s)
	if err != nil {
		return 0, err
	}
	return n.Uint64(), nil
}

func parseHexInt64(s string) (int64, error) {
	n, err := parseHexBigInt(s)
	if err != nil {
		return 0, err
	}
	return n.Int64(), nil
}

func bumpGasPrice(base *big.Int) *big.Int {
	return bumpGasPriceWithRatio(base, polygonGasPriceBumpNumerator, polygonGasPriceBumpDenominator)
}

func bumpGasPriceWithRatio(base *big.Int, numerator, denominator int64) *big.Int {
	if base == nil {
		return nil
	}
	if denominator <= 0 {
		return new(big.Int).Set(base)
	}
	bumped := new(big.Int).Mul(base, big.NewInt(numerator))
	bumped.Div(bumped, big.NewInt(denominator))
	if bumped.Cmp(base) < 0 {
		return new(big.Int).Set(base)
	}
	return bumped
}

func nextReceiptPollInterval(current time.Duration) time.Duration {
	if current >= polygonMaxReceiptPollInterval {
		return polygonMaxReceiptPollInterval
	}
	next := current + time.Second
	if next > polygonMaxReceiptPollInterval {
		return polygonMaxReceiptPollInterval
	}
	return next
}

func resetTimer(timer *time.Timer, d time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(d)
}

func (c *PolygonClient) describePendingTransaction(txHash string) string {
	statusCtx, cancel := context.WithTimeout(context.Background(), polygonTimeoutStatusProbeTimeout)
	defer cancel()

	tx, err := c.GetTransactionByHash(statusCtx, txHash)
	if err != nil {
		return fmt.Sprintf("timed out; unable to probe tx status: %v", err)
	}
	if tx == nil {
		return "timed out; transaction not found on RPC (dropped or not propagated)"
	}
	if tx.BlockNumber == "" || tx.BlockNumber == "0x" || tx.BlockNumber == "0x0" {
		return "timed out; transaction still pending in RPC/mempool"
	}
	return fmt.Sprintf("timed out; transaction seen in block %s but receipt unavailable", tx.BlockNumber)
}
