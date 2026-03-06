package main

import "testing"

func TestFirstTargetArg(t *testing.T) {
	if got := firstTargetArg([]string{"-force", "btc-updown-15m-1772833500"}); got != "btc-updown-15m-1772833500" {
		t.Fatalf("expected slug target, got %q", got)
	}
	if got := firstTargetArg([]string{"0xabd7a6a52fd2c53bba614104108c06403d14fd68bb2d667b0baf3af58548dd5e"}); got == "" {
		t.Fatal("expected condition ID target to be returned")
	}
	if got := firstTargetArg([]string{"-force"}); got != "" {
		t.Fatalf("expected empty target, got %q", got)
	}
}
