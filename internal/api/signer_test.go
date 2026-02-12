package api

import "testing"

const testPrivateKey = "0x4c0883a69102937d6231471b5dbb6204fe512961708279f0b8f359bd2d96f9f4"

func TestNewSignerRejectsInvalidVerifyingContract(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		_, err := NewSigner(testPrivateKey, "")
		if err == nil {
			t.Fatal("expected error for empty verifying contract")
		}
	})

	t.Run("malformed", func(t *testing.T) {
		_, err := NewSigner(testPrivateKey, "not-an-address")
		if err == nil {
			t.Fatal("expected error for malformed verifying contract")
		}
	})
}

func TestNewSignerUsesDefaultVerifyingContractWhenNotProvided(t *testing.T) {
	signerDefault, err := NewSigner(testPrivateKey)
	if err != nil {
		t.Fatalf("failed to create default signer: %v", err)
	}

	signerExplicit, err := NewSigner(testPrivateKey, DefaultVerifyingContract)
	if err != nil {
		t.Fatalf("failed to create explicit signer: %v", err)
	}

	if signerDefault.getDomainSeparator() != signerExplicit.getDomainSeparator() {
		t.Fatal("default verifying contract should match explicit default contract")
	}
}

func TestDomainSeparatorChangesWithVerifyingContract(t *testing.T) {
	signerA, err := NewSigner(testPrivateKey, "0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E")
	if err != nil {
		t.Fatalf("failed to create signer A: %v", err)
	}
	signerB, err := NewSigner(testPrivateKey, "0x1111111111111111111111111111111111111111")
	if err != nil {
		t.Fatalf("failed to create signer B: %v", err)
	}

	if signerA.getDomainSeparator() == signerB.getDomainSeparator() {
		t.Fatal("expected domain separators to differ by verifying contract")
	}
}
