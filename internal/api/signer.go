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

const (
	defaultExchangeContract = "0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E"
	negRiskExchangeContract = "0xC5d563A36AE78145C45A50134D48A1215220ccf1"
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

// Signer handles EIP-712 signing for Polymarket CLOB API
type Signer struct {
	privateKey *ecdsa.PrivateKey
	address    string
}

// NewSigner creates a new signer from a hex-encoded private key
func NewSigner(privateKeyHex string) (*Signer, error) {
	pk := strings.TrimPrefix(privateKeyHex, "0x")
	privateKey, err := crypto.HexToECDSA(pk)
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %w", err)
	}

	address := crypto.PubkeyToAddress(privateKey.PublicKey).Hex()

	return &Signer{
		privateKey: privateKey,
		address:    address,
	}, nil
}

// Address returns the Ethereum address for this signer
func (s *Signer) Address() string {
	return s.address
}

// OrderData represents the data needed to sign an order
type OrderData struct {
	Salt          string
	Maker         string
	Signer        string
	Taker         string
	TokenID       string
	MakerAmount   string
	TakerAmount   string
	Expiration    string
	Nonce         string
	FeeRateBps    string
	Side          int // 0 for BUY, 1 for SELL
	SignatureType int
}

// SignOrder signs an order using EIP-712
func (s *Signer) SignOrder(order *OrderData) (string, error) {
	return s.SignOrderWithContract(order, defaultExchangeContract)
}

// SignOrderWithContract signs an order using EIP-712 for a specific verifying contract.
func (s *Signer) SignOrderWithContract(order *OrderData, verifyingContract string) (string, error) {
	// Polymarket CLOB uses EIP-712 typed data signing
	// Domain: { name: "Polymarket CTF Exchange", version: "1", chainId: 137 }

	domainSeparator := s.getDomainSeparator(verifyingContract)
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

// getDomainSeparator returns the EIP-712 domain separator for Polymarket CTF Exchange
func (s *Signer) getDomainSeparator(verifyingContract string) [32]byte {
	// keccak256("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)")
	typeHash := keccak256([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"))

	nameHash := keccak256([]byte("Polymarket CTF Exchange"))
	versionHash := keccak256([]byte("1"))
	chainId := big.NewInt(137) // Polygon mainnet
	contractAddress := parseAddress(verifyingContract)

	// Encode: typeHash + nameHash + versionHash + chainId + verifyingContract
	encoded := make([]byte, 32*5)
	copy(encoded[0:32], typeHash[:])
	copy(encoded[32:64], nameHash[:])
	copy(encoded[64:96], versionHash[:])

	// ChainId as uint256 (32 bytes)
	chainIdBytes := padLeft(chainId.Bytes(), 32)
	copy(encoded[96:128], chainIdBytes)

	// address as uint256 (32 bytes, padded left)
	copy(encoded[128:160], padLeft(contractAddress, 32))

	return keccak256(encoded)
}

// getOrderStructHash returns the struct hash for an order
func (s *Signer) getOrderStructHash(order *OrderData) [32]byte {
	// Order struct type hash (EXACT field order and casing required by Polymarket)
	typeHash := keccak256([]byte(
		"Order(uint256 salt,address maker,address signer,address taker,uint256 tokenId,uint256 makerAmount,uint256 takerAmount,uint256 expiration,uint256 nonce,uint256 feeRateBps,uint8 side,uint8 signatureType)",
	))

	// Parse values
	salt := parseBigInt(order.Salt)
	maker := parseAddress(order.Maker)
	signer := parseAddress(order.Signer)
	taker := parseAddress(order.Taker)
	tokenID := parseBigInt(order.TokenID)
	makerAmount := parseBigInt(order.MakerAmount)
	takerAmount := parseBigInt(order.TakerAmount)
	expiration := parseBigInt(order.Expiration)
	nonce := parseBigInt(order.Nonce)
	feeRateBps := parseBigInt(order.FeeRateBps)

	side := uint8(order.Side)
	signatureType := uint8(order.SignatureType)

	// Encode all fields (32 bytes each, padded)
	// Sequence: salt, maker, signer, taker, tokenId, makerAmount, takerAmount, expiration, nonce, feeRateBps, side, signatureType
	encoded := make([]byte, 13*32) // typeHash + 12 fields
	copy(encoded[0:32], typeHash[:])
	copy(encoded[32:64], padLeft(salt.Bytes(), 32))
	copy(encoded[64:96], padLeft(maker, 32))
	copy(encoded[96:128], padLeft(signer, 32))
	copy(encoded[128:160], padLeft(taker, 32))
	copy(encoded[160:192], padLeft(tokenID.Bytes(), 32))
	copy(encoded[192:224], padLeft(makerAmount.Bytes(), 32))
	copy(encoded[224:256], padLeft(takerAmount.Bytes(), 32))
	copy(encoded[256:288], padLeft(expiration.Bytes(), 32))
	copy(encoded[288:320], padLeft(nonce.Bytes(), 32))
	copy(encoded[320:352], padLeft(feeRateBps.Bytes(), 32))

	// uint8 fields are at the end of their 32-byte slots (padded left)
	encoded[352+31] = side
	encoded[384+31] = signatureType

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
	key, err := base64.URLEncoding.DecodeString(a.APISecret)
	if err != nil {
		// Fallback to standard encoding for decoding
		key, _ = base64.StdEncoding.DecodeString(a.APISecret)
	}
	h := hmac.New(sha256.New, key)
	h.Write([]byte(message))
	// Use URL-safe base64 for output signature (matches Python/TS clients)
	sig := base64.URLEncoding.EncodeToString(h.Sum(nil))

	return ts, sig
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

func padLeft(data []byte, size int) []byte {
	if len(data) >= size {
		return data[len(data)-size:]
	}
	padded := make([]byte, size)
	copy(padded[size-len(data):], data)
	return padded
}
