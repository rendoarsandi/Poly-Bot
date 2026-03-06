package marketlookup

import "testing"

func TestLooksLikeConditionID(t *testing.T) {
	if !LooksLikeConditionID("0xabd7a6a52fd2c53bba614104108c06403d14fd68bb2d667b0baf3af58548dd5e") {
		t.Fatal("expected valid 32-byte hex condition ID to be recognized")
	}
	if LooksLikeConditionID("btc-updown-15m-1772833500") {
		t.Fatal("did not expect slug to be treated as condition ID")
	}
	if LooksLikeConditionID("0xnothex") {
		t.Fatal("did not expect non-hex string to be treated as condition ID")
	}
}
