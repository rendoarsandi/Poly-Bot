package main

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/joho/godotenv"
	"golang.org/x/crypto/sha3"
)

const (
	CLOB_HOST = "https://clob.polymarket.com"
	CHAIN_ID  = 137
)

// APICredentials represents the API key response
type APICredentials struct {
	APIKey     string `json:"apiKey"`
	Secret     string `json:"secret"`
	Passphrase string `json:"passphrase"`
}

func main() {
	godotenv.Load()

	pk := os.Getenv("PK")
	if pk == "" {
		fmt.Println("Error: PK environment variable not set")
		os.Exit(1)
	}

	// Remove 0x prefix if present
	if len(pk) >= 2 && pk[:2] == "0x" {
		pk = pk[2:]
	}

	privateKey, err := crypto.HexToECDSA(pk)
	if err != nil {
		fmt.Printf("Error parsing private key: %v\n", err)
		os.Exit(1)
	}

	address := crypto.PubkeyToAddress(privateKey.PublicKey).Hex()
	fmt.Printf("Wallet Address: %s\n", address)

	// Generate timestamp and nonce
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "0"

	// Create EIP-712 message
	message := "This message attests that I control the given wallet"

	// Sign the ClobAuth message using EIP-712
	signature, err := signClobAuthMessage(privateKey, address, timestamp, nonce, message)
	if err != nil {
		fmt.Printf("Error signing message: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Timestamp: %s\n", timestamp)
	fmt.Printf("Signature: %s\n", signature)

	// First try to derive existing API key
	fmt.Println("\n--- Attempting to derive existing API key ---")
	creds, err := deriveAPIKey(address, timestamp, nonce, signature)
	if err != nil {
		fmt.Printf("Derive failed: %v\n", err)
		fmt.Println("\n--- Attempting to create new API key ---")
		creds, err = createAPIKey(address, timestamp, nonce, signature)
		if err != nil {
			fmt.Printf("Create failed: %v\n", err)
			os.Exit(1)
		}
	}

	fmt.Println("\n=== SUCCESS! Add these to your .env file ===")
	fmt.Printf("POLY_API_KEY=%s\n", creds.APIKey)
	fmt.Printf("POLY_API_SECRET=%s\n", creds.Secret)
	fmt.Printf("POLY_PASSPHRASE=%s\n", creds.Passphrase)
}

func signClobAuthMessage(privateKey *crypto.PrivateKey, address, timestamp, nonce, message string) (string, error) {
	// EIP-712 Domain
	domainTypeHash := keccak256([]byte("EIP712Domain(string name,string version,uint256 chainId)"))
	nameHash := keccak256([]byte("ClobAuthDomain"))
	versionHash := keccak256([]byte("1"))

	// Encode domain separator
	domainSeparator := keccak256(concat(
		domainTypeHash[:],
		nameHash[:],
		versionHash[:],
		padLeft(intToBytes(CHAIN_ID), 32),
	))

	// ClobAuth type hash
	clobAuthTypeHash := keccak256([]byte("ClobAuth(address address,string timestamp,uint256 nonce,string message)"))

	// Encode message struct
	addressBytes, _ := hex.DecodeString(address[2:])
	timestampHash := keccak256([]byte(timestamp))
	nonceInt, _ := strconv.Atoi(nonce)
	messageHash := keccak256([]byte(message))

	structHash := keccak256(concat(
		clobAuthTypeHash[:],
		padLeft(addressBytes, 32),
		timestampHash[:],
		padLeft(intToBytes(nonceInt), 32),
		messageHash[:],
	))

	// Final message hash: keccak256("\x19\x01" + domainSeparator + structHash)
	finalMessage := concat(
		[]byte{0x19, 0x01},
		domainSeparator[:],
		structHash[:],
	)
	messageDigest := keccak256(finalMessage)

	// Sign with private key
	sig, err := crypto.Sign(messageDigest[:], privateKey)
	if err != nil {
		return "", err
	}

	// Adjust v value for Ethereum (add 27)
	sig[64] += 27

	return "0x" + hex.EncodeToString(sig), nil
}

func deriveAPIKey(address, timestamp, nonce, signature string) (*APICredentials, error) {
	url := CLOB_HOST + "/auth/derive-api-key"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("POLY_ADDRESS", address)
	req.Header.Set("POLY_SIGNATURE", signature)
	req.Header.Set("POLY_TIMESTAMP", timestamp)
	req.Header.Set("POLY_NONCE", nonce)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var creds APICredentials
	if err := json.Unmarshal(body, &creds); err != nil {
		return nil, fmt.Errorf("failed to parse response: %v, body: %s", err, string(body))
	}

	return &creds, nil
}

func createAPIKey(address, timestamp, nonce, signature string) (*APICredentials, error) {
	url := CLOB_HOST + "/auth/api-key"

	req, err := http.NewRequest("POST", url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("POLY_ADDRESS", address)
	req.Header.Set("POLY_SIGNATURE", signature)
	req.Header.Set("POLY_TIMESTAMP", timestamp)
	req.Header.Set("POLY_NONCE", nonce)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var creds APICredentials
	if err := json.Unmarshal(body, &creds); err != nil {
		return nil, fmt.Errorf("failed to parse response: %v, body: %s", err, string(body))
	}

	return &creds, nil
}

// Helper functions
func keccak256(data []byte) [32]byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(data)
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}

func padLeft(data []byte, size int) []byte {
	if len(data) >= size {
		return data[len(data)-size:]
	}
	padded := make([]byte, size)
	copy(padded[size-len(data):], data)
	return padded
}

func intToBytes(n int) []byte {
	if n == 0 {
		return []byte{0}
	}
	var result []byte
	for n > 0 {
		result = append([]byte{byte(n & 0xff)}, result...)
		n >>= 8
	}
	return result
}

func concat(slices ...[]byte) []byte {
	var result []byte
	for _, s := range slices {
		result = append(result, s...)
	}
	return result
}
