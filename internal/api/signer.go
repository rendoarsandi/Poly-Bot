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

	"github.com/ethereum/go-ethereum/crypto"
	"golang.org/x/crypto/sha3"
)

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
	Side          string // "BUY" or "SELL"
	SignatureType int
}

// SignOrder signs an order using EIP-712
func (s *Signer) SignOrder(order *OrderData) (string, error) {
	// Polymarket CLOB uses EIP-712 typed data signing
	// Domain: { name: "Polymarket CTF Exchange", version: "1", chainId: 137 }

	domainSeparator := s.getDomainSeparator()
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
func (s *Signer) getDomainSeparator() [32]byte {
	// keccak256("EIP712Domain(string name,string version,uint256 chainId)")
	typeHash := keccak256([]byte("EIP712Domain(string name,string version,uint256 chainId)"))

	nameHash := keccak256([]byte("Polymarket CTF Exchange"))
	versionHash := keccak256([]byte("1"))
	chainId := big.NewInt(137) // Polygon mainnet

	// Encode: typeHash + nameHash + versionHash + chainId
	encoded := make([]byte, 128)
	copy(encoded[0:32], typeHash[:])
	copy(encoded[32:64], nameHash[:])
	copy(encoded[64:96], versionHash[:])
	chainIdBytes := chainId.Bytes()
	copy(encoded[128-len(chainIdBytes):128], chainIdBytes)

	return keccak256(encoded)
}

// getOrderStructHash returns the struct hash for an order
func (s *Signer) getOrderStructHash(order *OrderData) [32]byte {
	// Order struct type hash
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

	side := uint8(0) // BUY
	if order.Side == "SELL" {
		side = 1
	}
	signatureType := uint8(order.SignatureType)

	// Encode all fields (32 bytes each, padded)
	encoded := make([]byte, 12*32) // 12 fields
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
	encoded[352+31] = side
	encoded[384-1] = signatureType

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
	message := ts + method + path + body

	key, _ := base64.StdEncoding.DecodeString(a.APISecret)
	h := hmac.New(sha256.New, key)
	h.Write([]byte(message))
	sig := base64.StdEncoding.EncodeToString(h.Sum(nil))

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
