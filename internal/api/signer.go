package api

import (
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"golang.org/x/crypto/sha3"
)

// SignTransaction signs a legacy Ethereum transaction for the Polygon network
func (s *Signer) SignTransaction(nonce uint64, to string, value *big.Int, gasLimit uint64, gasPrice *big.Int, data string) (string, error) {
	toAddr := common.HexToAddress(to)
	dataBytes, err := hex.DecodeString(strings.TrimPrefix(data, "0x"))
	if err != nil {
		return "", fmt.Errorf("invalid data hex: %w", err)
	}

	// Create legacy transaction (Polygon supports EIP-155)
	tx := types.NewTransaction(nonce, toAddr, value, gasLimit, gasPrice, dataBytes)

	// Sign with ChainID 137 (Polygon)
	chainID := big.NewInt(137)
	signer := types.NewEIP155Signer(chainID)

	signedTx, err := types.SignTx(tx, signer, s.privateKey)
	if err != nil {
		return "", fmt.Errorf("failed to sign transaction: %w", err)
	}

	rawTxBytes, err := signedTx.MarshalBinary()
	if err != nil {
		return "", fmt.Errorf("failed to marshal signed transaction: %w", err)
	}
	return "0x" + hex.EncodeToString(rawTxBytes), nil
}

// SignDynamicFeeTransaction signs an EIP-1559 dynamic-fee transaction for the Polygon network.
func (s *Signer) SignDynamicFeeTransaction(nonce uint64, to string, value *big.Int, gasLimit uint64, maxFeePerGas, maxPriorityFeePerGas *big.Int, data string) (string, error) {
	toAddr := common.HexToAddress(to)
	dataBytes, err := hex.DecodeString(strings.TrimPrefix(data, "0x"))
	if err != nil {
		return "", fmt.Errorf("invalid data hex: %w", err)
	}
	if maxFeePerGas == nil || maxPriorityFeePerGas == nil {
		return "", fmt.Errorf("dynamic fee parameters are required")
	}

	chainID := big.NewInt(137)
	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		GasTipCap: maxPriorityFeePerGas,
		GasFeeCap: maxFeePerGas,
		Gas:       gasLimit,
		To:        &toAddr,
		Value:     value,
		Data:      dataBytes,
	})

	signer := types.NewLondonSigner(chainID)
	signedTx, err := types.SignTx(tx, signer, s.privateKey)
	if err != nil {
		return "", fmt.Errorf("failed to sign dynamic-fee transaction: %w", err)
	}

	rawTxBytes, err := signedTx.MarshalBinary()
	if err != nil {
		return "", fmt.Errorf("failed to marshal signed transaction: %w", err)
	}
	return "0x" + hex.EncodeToString(rawTxBytes), nil
}

// Signer handles EIP-712 signing for Polymarket CLOB API
type Signer struct {
	privateKey *ecdsa.PrivateKey
	address    string
	orderType  [32]byte
}

// NewSigner creates a new signer from a hex-encoded private key
func NewSigner(privateKeyHex string) (*Signer, error) {
	pk := strings.TrimPrefix(privateKeyHex, "0x")
	privateKey, err := crypto.HexToECDSA(pk)
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %w", err)
	}

	address := crypto.PubkeyToAddress(privateKey.PublicKey).Hex()
	orderType := buildOrderTypeHash()

	return &Signer{
		privateKey: privateKey,
		address:    address,
		orderType:  orderType,
	}, nil
}

// Address returns the Ethereum address for this signer
func (s *Signer) Address() string {
	return s.address
}

// OrderData represents the data needed to sign an order
type OrderData struct {
	Salt              string
	Maker             string
	Signer            string
	Taker             string
	TokenID           string
	MakerAmount       string
	TakerAmount       string
	Expiration        string
	Nonce             string
	FeeRateBps        string
	Timestamp         string
	Metadata          string
	Builder           string
	VerifyingContract string
	Side              int // 0 for BUY, 1 for SELL
	SignatureType     int
}

// SignOrder signs an order using EIP-712
func (s *Signer) SignOrder(order *OrderData) (string, error) {
	domainSeparator := buildDomainSeparator(order.VerifyingContract)
	structHash := s.getOrderStructHash(order)

	// EIP-712: keccak256("\x19\x01" + domainSeparator + structHash)
	message := make([]byte, 2+32+32)
	message[0] = 0x19
	message[1] = 0x01
	copy(message[2:34], domainSeparator[:])
	copy(message[34:66], structHash[:])

	messageHash := keccak256(message)

	// Sign the message hash
	sig, err := s.signHash(messageHash)
	if err != nil {
		return "", fmt.Errorf("failed to sign order: %w", err)
	}

	return "0x" + hex.EncodeToString(sig), nil
}

// getOrderStructHash returns the struct hash for an order
func (s *Signer) getOrderStructHash(order *OrderData) [32]byte {
	salt := parseBigInt(order.Salt)
	maker := parseAddress(order.Maker)
	signer := parseAddress(order.Signer)
	tokenID := parseBigInt(order.TokenID)
	makerAmount := parseBigInt(order.MakerAmount)
	takerAmount := parseBigInt(order.TakerAmount)
	timestamp := parseBigInt(order.Timestamp)
	metadata := parseBytes32(order.Metadata)
	builder := parseBytes32(order.Builder)

	side := uint8(order.Side)
	signatureType := uint8(order.SignatureType)

	encoded := make([]byte, 12*32) // typeHash + 11 fields
	copy(encoded[0:32], s.orderType[:])
	copy(encoded[32:64], padLeft(salt.Bytes(), 32))
	copy(encoded[64:96], padLeft(maker, 32))
	copy(encoded[96:128], padLeft(signer, 32))
	copy(encoded[128:160], padLeft(tokenID.Bytes(), 32))
	copy(encoded[160:192], padLeft(makerAmount.Bytes(), 32))
	copy(encoded[192:224], padLeft(takerAmount.Bytes(), 32))

	encoded[224+31] = side
	encoded[256+31] = signatureType
	copy(encoded[288:320], padLeft(timestamp.Bytes(), 32))
	copy(encoded[320:352], metadata[:])
	copy(encoded[352:384], builder[:])

	return keccak256(encoded)
}

// signHash signs a 32-byte hash with the private key using go-ethereum/crypto
func (s *Signer) signHash(hash [32]byte) ([]byte, error) {
	// Use go-ethereum's secure signing implementation
	// This handles RFC 6979 deterministic nonces correctly
	sig, err := crypto.Sign(hash[:], s.privateKey)
	if err != nil {
		return nil, err
	}

	// Adjust recovery ID (v) for Ethereum: add 27
	// go-ethereum returns v as 0 or 1, Ethereum expects 27 or 28
	sig[64] += 27

	return sig, nil
}

// APIAuth holds L2 API authentication data
type APIAuth struct {
	APIKey     string
	APISecret  string
	Passphrase string
	signingKey []byte
}

func NewAPIAuth(apiKey, apiSecret, passphrase string) *APIAuth {
	return &APIAuth{
		APIKey:     apiKey,
		APISecret:  apiSecret,
		Passphrase: passphrase,
		signingKey: decodeAPISecret(apiSecret),
	}
}

// SignL2Request signs an L2 API request for authentication
func (a *APIAuth) SignL2Request(method, path string, body string) (timestamp, signature string) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	// Strip query params from path for signing (Polymarket signs base path only)
	signPath := path
	if idx := strings.Index(path, "?"); idx != -1 {
		signPath = path[:idx]
	}

	message := ts + method + signPath + body

	// Polymarket uses URL-safe base64 for both decoding secret AND encoding signature
	h := hmac.New(sha256.New, a.signingKey)
	h.Write([]byte(message))
	// Use URL-safe base64 for output signature (matches Python/TS clients)
	sig := base64.URLEncoding.EncodeToString(h.Sum(nil))

	return ts, sig
}

func decodeAPISecret(secret string) []byte {
	key, err := base64.URLEncoding.DecodeString(secret)
	if err == nil {
		return key
	}
	key, _ = base64.StdEncoding.DecodeString(secret)
	return key
}

func buildDomainSeparator(verifyingContract string) [32]byte {
	// keccak256("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)")
	typeHash := keccak256([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"))
	nameHash := keccak256([]byte("Polymarket CTF Exchange"))
	versionHash := keccak256([]byte("2"))
	chainID := big.NewInt(137)
	if strings.TrimSpace(verifyingContract) == "" {
		verifyingContract = CTFExchange
	}
	verifyingContractAddr := parseAddress(verifyingContract)

	encoded := make([]byte, 32*5)
	copy(encoded[0:32], typeHash[:])
	copy(encoded[32:64], nameHash[:])
	copy(encoded[64:96], versionHash[:])
	copy(encoded[96:128], padLeft(chainID.Bytes(), 32))
	copy(encoded[128:160], padLeft(verifyingContractAddr, 32))

	return keccak256(encoded)
}

func buildOrderTypeHash() [32]byte {
	return keccak256([]byte(
		"Order(uint256 salt,address maker,address signer,uint256 tokenId,uint256 makerAmount,uint256 takerAmount,uint8 side,uint8 signatureType,uint256 timestamp,bytes32 metadata,bytes32 builder)",
	))
}

// Helper functions

func keccak256(data []byte) [32]byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(data)
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}

func parseBigInt(s string) *big.Int {
	n := new(big.Int)
	s = strings.TrimPrefix(s, "0x")
	n.SetString(s, 0)
	return n
}

func parseAddress(s string) []byte {
	s = strings.TrimPrefix(s, "0x")
	addr, _ := hex.DecodeString(s)
	if len(addr) < 20 {
		padded := make([]byte, 20)
		copy(padded[20-len(addr):], addr)
		return padded
	}
	return addr[:20]
}

func parseBytes32(s string) [32]byte {
	s = strings.TrimPrefix(strings.TrimSpace(s), "0x")
	decoded, _ := hex.DecodeString(s)
	var out [32]byte
	if len(decoded) > len(out) {
		decoded = decoded[len(decoded)-len(out):]
	}
	copy(out[32-len(decoded):], decoded)
	return out
}

func padLeft(data []byte, size int) []byte {
	if len(data) >= size {
		return data[len(data)-size:]
	}
	padded := make([]byte, size)
	copy(padded[size-len(data):], data)
	return padded
}
