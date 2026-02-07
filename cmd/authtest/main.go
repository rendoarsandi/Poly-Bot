package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/joho/godotenv"
)

func main() {
	godotenv.Load()

	apiKey := os.Getenv("POLY_API_KEY")
	apiSecret := os.Getenv("POLY_API_SECRET")
	passphrase := os.Getenv("POLY_PASSPHRASE")
	pk := os.Getenv("POLY_PK")

	// Derive address from private key
	pkClean := strings.TrimPrefix(pk, "0x")
	privateKey, err := crypto.HexToECDSA(pkClean)
	if err != nil {
		fmt.Printf("Error parsing PK: %v\n", err)
		return
	}
	address := crypto.PubkeyToAddress(privateKey.PublicKey).Hex()

	fmt.Println("=== Polymarket API Auth Test ===")
	fmt.Printf("API Key: %s\n", apiKey)
	fmt.Printf("Address: %s\n", address)

	// Decode secret
	key, _ := base64.URLEncoding.DecodeString(apiSecret)

	// Test with COLLATERAL asset type
	fullURL := "https://clob.polymarket.com/balance-allowance?asset_type=COLLATERAL"
	basePath := "/balance-allowance"  // Sign without query params
	method := "GET"
	body := ""

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	message := ts + method + basePath + body

	h := hmac.New(sha256.New, key)
	h.Write([]byte(message))
	sig := base64.URLEncoding.EncodeToString(h.Sum(nil))

	fmt.Printf("\nSigning: '%s'\n", message)
	fmt.Printf("Signature: %s\n", sig)

	// Make authenticated request
	req, _ := http.NewRequest("GET", fullURL, nil)
	req.Header.Set("POLY_API_KEY", apiKey)
	req.Header.Set("POLY_ADDRESS", address)
	req.Header.Set("POLY_PASSPHRASE", passphrase)
	req.Header.Set("POLY_TIMESTAMP", ts)
	req.Header.Set("POLY_SIGNATURE", sig)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("❌ Request failed: %v\n", err)
		return
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	fmt.Printf("\nStatus: HTTP %d\n", resp.StatusCode)
	fmt.Printf("Response: %s\n", string(bodyBytes))

	if resp.StatusCode == 200 {
		fmt.Println("\n✅ SUCCESS! Authentication is working!")
	} else if resp.StatusCode == 401 {
		fmt.Println("\n❌ Authentication failed")
	} else {
		fmt.Println("\n⚠️ Request completed but with error")
	}
}
