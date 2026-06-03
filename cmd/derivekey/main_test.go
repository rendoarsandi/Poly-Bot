package main

import (
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

func TestIntToBytes(t *testing.T) {
	tests := []struct {
		val  int
		want []byte
	}{
		{0, []byte{0}},
		{1, []byte{1}},
		{256, []byte{1, 0}},
		{65535, []byte{255, 255}},
	}

	for _, tt := range tests {
		got := intToBytes(tt.val)
		if len(got) != len(tt.want) {
			t.Fatalf("intToBytes(%d) length: got %d, want %d", tt.val, len(got), len(tt.want))
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Fatalf("intToBytes(%d) mismatch at index %d: got %d, want %d", tt.val, i, got[i], tt.want[i])
			}
		}
	}
}

func TestPadLeft(t *testing.T) {
	tests := []struct {
		data []byte
		size int
		want []byte
	}{
		{[]byte{1, 2}, 4, []byte{0, 0, 1, 2}},
		{[]byte{1, 2, 3, 4}, 2, []byte{3, 4}},
		{[]byte{}, 3, []byte{0, 0, 0}},
	}

	for _, tt := range tests {
		got := padLeft(tt.data, tt.size)
		if len(got) != tt.size {
			t.Fatalf("padLeft length: got %d, want %d", len(got), tt.size)
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Fatalf("padLeft mismatch at index %d: got %v, want %v", i, got, tt.want)
			}
		}
	}
}

func TestConcat(t *testing.T) {
	got := concat([]byte{1, 2}, []byte{3}, []byte{4, 5})
	want := []byte{1, 2, 3, 4, 5}
	if len(got) != len(want) {
		t.Fatalf("concat length: got %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("concat mismatch at index %d", i)
		}
	}
}

func TestKeccak256(t *testing.T) {
	data := []byte("hello")
	got := keccak256(data)
	// Expected keccak256 of "hello"
	expectedHex := "1c8aff950685c2ed4bc3174f3472287b56d9517b9c948127319a09a7a36deac8"
	gotHex := hex.EncodeToString(got[:])
	if gotHex != expectedHex {
		t.Fatalf("keccak256 mismatch: got %s, want %s", gotHex, expectedHex)
	}
}

func TestSignClobAuthMessage(t *testing.T) {
	// Generate dummy key
	privKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	address := crypto.PubkeyToAddress(privKey.PublicKey).Hex()
	timestamp := "1700000000"
	nonce := "0"
	message := "This message attests that I control the given wallet"

	sig, err := signClobAuthMessage(privKey, address, timestamp, nonce, message)
	if err != nil {
		t.Fatalf("signClobAuthMessage failed: %v", err)
	}

	if len(sig) != 132 || sig[:2] != "0x" {
		t.Fatalf("invalid signature format: %q", sig)
	}

	// Verify signature
	sigBytes, err := hex.DecodeString(sig[2:])
	if err != nil {
		t.Fatalf("failed to decode signature: %v", err)
	}

	// Adjust v value back for standard ECDSA recovery (subtract 27)
	sigBytes[64] -= 27

	// Recreate message hash
	domainTypeHash := keccak256([]byte("EIP712Domain(string name,string version,uint256 chainId)"))
	nameHash := keccak256([]byte("ClobAuthDomain"))
	versionHash := keccak256([]byte("1"))
	domainSeparator := keccak256(concat(
		domainTypeHash[:],
		nameHash[:],
		versionHash[:],
		padLeft(intToBytes(CHAIN_ID), 32),
	))

	clobAuthTypeHash := keccak256([]byte("ClobAuth(address address,string timestamp,uint256 nonce,string message)"))
	addressBytes, _ := hex.DecodeString(address[2:])
	timestampHash := keccak256([]byte(timestamp))
	messageHash := keccak256([]byte(message))

	structHash := keccak256(concat(
		clobAuthTypeHash[:],
		padLeft(addressBytes, 32),
		timestampHash[:],
		padLeft(intToBytes(0), 32),
		messageHash[:],
	))

	finalMessage := concat(
		[]byte{0x19, 0x01},
		domainSeparator[:],
		structHash[:],
	)
	messageDigest := keccak256(finalMessage)

	pubKeyBytes, err := crypto.Ecrecover(messageDigest[:], sigBytes)
	if err != nil {
		t.Fatalf("failed to recover public key: %v", err)
	}

	recoveredPubKey, err := crypto.UnmarshalPubkey(pubKeyBytes)
	if err != nil {
		t.Fatalf("failed to unmarshal recovered public key: %v", err)
	}

	recoveredAddress := crypto.PubkeyToAddress(*recoveredPubKey).Hex()
	if recoveredAddress != address {
		t.Fatalf("recovered address mismatch: got %s, want %s", recoveredAddress, address)
	}
}

// Test parsing of private keys using ethereum helper
func TestParsePrivateKey(t *testing.T) {
	// A valid 32-byte hex string (dummy)
	dummyKeyHex := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	privKey, err := crypto.HexToECDSA(dummyKeyHex)
	if err != nil {
		t.Fatalf("HexToECDSA failed: %v", err)
	}
	if privKey.D.Cmp(big.NewInt(0)) == 0 {
		t.Fatalf("invalid private key value")
	}
}
