package setup

import (
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

func TestSignClobAuthMessage(t *testing.T) {
	// Use a known private key for deterministic testing
	privateKeyHex := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	privateKey, err := crypto.HexToECDSA(privateKeyHex)
	if err != nil {
		t.Fatalf("Failed to load private key: %v", err)
	}

	address := crypto.PubkeyToAddress(privateKey.PublicKey).Hex()
	timestamp := "1672531200" // Example static timestamp
	nonce := "0"
	message := "This message attests that I control the given wallet"

	sig, err := signClobAuthMessage(privateKey, address, timestamp, nonce, message)
	if err != nil {
		t.Fatalf("Failed to sign message: %v", err)
	}

	if !strings.HasPrefix(sig, "0x") {
		t.Errorf("Signature should start with 0x, got: %s", sig)
	}
	
	// A typical Ethereum signature is 65 bytes (130 hex chars) + '0x' prefix = 132 chars
	if len(sig) != 132 {
		t.Errorf("Expected signature length 132, got %d", len(sig))
	}
}

func TestDeriveOrBuildAPIKey_InvalidPK(t *testing.T) {
	_, err := deriveOrBuildAPIKey("invalid-pk-string")
	if err == nil {
		t.Error("Expected error for invalid private key, got nil")
	}
	
	if !strings.Contains(err.Error(), "error parsing private key") {
		t.Errorf("Expected parse error, got: %v", err)
	}
}

// We do not test deriveAPIKey or createAPIKey directly against the live API 
// in standard unit tests to avoid network dependencies and rate limits.
// Mocks would be required for deeper testing of those HTTP requests.