package main

import (
	"errors"
	"math/big"
	"testing"
)

func TestCheckPermissionStatus_AllGood(t *testing.T) {
	allowance := big.NewInt(1000)
	usdcIcon, allowanceStr, ctfIcon, ctfStr, isGood := checkPermissionStatus(
		allowance, nil, true, nil, "Exchange",
	)

	if !isGood {
		t.Fatalf("expected isGood to be true")
	}
	if usdcIcon != "✅" {
		t.Errorf("expected usdcIcon to be ✅, got %q", usdcIcon)
	}
	if allowanceStr != "1000" {
		t.Errorf("expected allowanceStr to be '1000', got %q", allowanceStr)
	}
	if ctfIcon != "✅" {
		t.Errorf("expected ctfIcon to be ✅, got %q", ctfIcon)
	}
	if ctfStr != "true" {
		t.Errorf("expected ctfStr to be 'true', got %q", ctfStr)
	}
}

func TestCheckPermissionStatus_AllowanceErr(t *testing.T) {
	errAllow := errors.New("rpc error")
	usdcIcon, allowanceStr, _, _, isGood := checkPermissionStatus(
		nil, errAllow, true, nil, "Exchange",
	)

	if isGood {
		t.Fatalf("expected isGood to be false")
	}
	if usdcIcon != "⚠️" {
		t.Errorf("expected usdcIcon to be ⚠️, got %q", usdcIcon)
	}
	if allowanceStr != "(rpc error)" {
		t.Errorf("expected allowanceStr to be '(rpc error)', got %q", allowanceStr)
	}
}

func TestCheckPermissionStatus_ZeroAllowance(t *testing.T) {
	allowance := big.NewInt(0)
	usdcIcon, allowanceStr, _, _, isGood := checkPermissionStatus(
		allowance, nil, true, nil, "Exchange",
	)

	if isGood {
		t.Fatalf("expected isGood to be false")
	}
	if usdcIcon != "❌" {
		t.Errorf("expected usdcIcon to be ❌, got %q", usdcIcon)
	}
	if allowanceStr != "0" {
		t.Errorf("expected allowanceStr to be '0', got %q", allowanceStr)
	}
}

func TestCheckPermissionStatus_CtfContractN_A(t *testing.T) {
	allowance := big.NewInt(1000)
	usdcIcon, allowanceStr, ctfIcon, ctfStr, isGood := checkPermissionStatus(
		allowance, nil, false, nil, "CTF Contract",
	)

	if !isGood {
		t.Fatalf("expected isGood to be true for CTF Contract with no operator status needed")
	}
	if usdcIcon != "✅" || allowanceStr != "1000" {
		t.Errorf("unexpected usdc result: %s, %s", usdcIcon, allowanceStr)
	}
	if ctfIcon != "⚪" {
		t.Errorf("expected ctfIcon to be ⚪ for CTF Contract, got %q", ctfIcon)
	}
	if ctfStr != "N/A" {
		t.Errorf("expected ctfStr to be 'N/A' for CTF Contract, got %q", ctfStr)
	}
}

func TestCheckPermissionStatus_CtfApproveErr(t *testing.T) {
	allowance := big.NewInt(1000)
	errApprove := errors.New("rpc error ctf")
	_, _, ctfIcon, ctfStr, isGood := checkPermissionStatus(
		allowance, nil, false, errApprove, "Exchange",
	)

	if isGood {
		t.Fatalf("expected isGood to be false due to CTF approval error")
	}
	if ctfIcon != "⚠️" {
		t.Errorf("expected ctfIcon to be ⚠️, got %q", ctfIcon)
	}
	if ctfStr != "(rpc error ctf)" {
		t.Errorf("expected ctfStr to be '(rpc error ctf)', got %q", ctfStr)
	}
}

func TestCheckPermissionStatus_CtfNotApproved(t *testing.T) {
	allowance := big.NewInt(1000)
	_, _, ctfIcon, ctfStr, isGood := checkPermissionStatus(
		allowance, nil, false, nil, "Exchange",
	)

	if isGood {
		t.Fatalf("expected isGood to be false due to CTF not approved")
	}
	if ctfIcon != "❌" {
		t.Errorf("expected ctfIcon to be ❌, got %q", ctfIcon)
	}
	if ctfStr != "false" {
		t.Errorf("expected ctfStr to be 'false', got %q", ctfStr)
	}
}
