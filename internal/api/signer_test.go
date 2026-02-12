package api

import "testing"

const signerTestPrivateKey = "0x4c0883a69102937d6231471b5dbb6204fe512961708279f0b8f359bd2d96f9f4"

func TestNewSignerRejectsInvalidVerifyingContract(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		_, err := NewSigner(signerTestPrivateKey, "")
		if err == nil {
			t.Fatal("expected error for empty verifying contract")
		}
	})

	t.Run("malformed", func(t *testing.T) {
		_, err := NewSigner(signerTestPrivateKey, "not-an-address")
		if err == nil {
			t.Fatal("expected error for malformed verifying contract")
		}
	})
}

func TestNewSignerUsesDefaultVerifyingContractWhenNotProvided(t *testing.T) {
	signerDefault, err := NewSigner(signerTestPrivateKey)
	if err != nil {
		t.Fatalf("failed to create default signer: %v", err)
	}

	signerExplicit, err := NewSigner(signerTestPrivateKey, DefaultVerifyingContract)
	if err != nil {
		t.Fatalf("failed to create explicit signer: %v", err)
	}

	if signerDefault.getDomainSeparator("0xregular-token") != signerExplicit.getDomainSeparator("0xregular-token") {
		t.Fatal("default verifying contract should match explicit default contract")
	}
}

func TestDomainSeparatorChangesWithVerifyingContract(t *testing.T) {
	signer, err := NewSigner(signerTestPrivateKey)
	if err != nil {
		t.Fatalf("failed to create signer: %v", err)
	}
	signer.SetNegRiskTokenIDs([]string{"0xneg-risk-token"})

	if signer.getDomainSeparator("0xregular-token") == signer.getDomainSeparator("0xneg-risk-token") {
		t.Fatal("expected domain separators to differ by token risk domain")
	}
}
