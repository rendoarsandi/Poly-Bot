package setup

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"golang.org/x/crypto/sha3"
)

const (
	clobHost = "https://clob.polymarket.com"
	chainID  = 137
)

type APICredentials struct {
	APIKey     string `json:"apiKey"`
	Secret     string `json:"secret"`
	Passphrase string `json:"passphrase"`
}

func deriveOrBuildAPIKey(pk string) (*APICredentials, error) {
	if len(pk) >= 2 && pk[:2] == "0x" {
		pk = pk[2:]
	}

	privateKey, err := crypto.HexToECDSA(pk)
	if err != nil {
		return nil, fmt.Errorf("error parsing private key: %v", err)
	}

	address := crypto.PubkeyToAddress(privateKey.PublicKey).Hex()

	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "0"
	message := "This message attests that I control the given wallet"

	signature, err := signClobAuthMessage(privateKey, address, timestamp, nonce, message)
	if err != nil {
		return nil, fmt.Errorf("error signing message: %v", err)
	}

	creds, err := deriveAPIKey(address, timestamp, nonce, signature)
	if err != nil {
		creds, err = createAPIKey(address, timestamp, nonce, signature)
		if err != nil {
			return nil, fmt.Errorf("failed to create or derive api key: %v", err)
		}
	}
	return creds, nil
}

func signClobAuthMessage(privateKey *ecdsa.PrivateKey, address, timestamp, nonce, message string) (string, error) {
	domainTypeHash := keccak256([]byte("EIP712Domain(string name,string version,uint256 chainId)"))
	nameHash := keccak256([]byte("ClobAuthDomain"))
	versionHash := keccak256([]byte("1"))

	domainSeparator := keccak256(concat(
		domainTypeHash[:],
		nameHash[:],
		versionHash[:],
		padLeft(intToBytes(chainID), 32),
	))

	clobAuthTypeHash := keccak256([]byte("ClobAuth(address address,string timestamp,uint256 nonce,string message)"))

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

	finalMessage := concat([]byte{0x19, 0x01}, domainSeparator[:], structHash[:])
	messageDigest := keccak256(finalMessage)

	sig, err := crypto.Sign(messageDigest[:], privateKey)
	if err != nil {
		return "", err
	}
	sig[64] += 27

	return "0x" + hex.EncodeToString(sig), nil
}

func deriveAPIKey(address, timestamp, nonce, signature string) (*APICredentials, error) {
	req, err := http.NewRequest("GET", clobHost+"/auth/derive-api-key", nil)
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
		return nil, err
	}
	return &creds, nil
}

func createAPIKey(address, timestamp, nonce, signature string) (*APICredentials, error) {
	req, err := http.NewRequest("POST", clobHost+"/auth/api-key", bytes.NewReader([]byte("{}")))
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
		return nil, err
	}
	return &creds, nil
}

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
