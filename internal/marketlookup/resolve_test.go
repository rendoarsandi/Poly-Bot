package marketlookup

import (
	"reflect"
	"strings"
	"testing"

	"Market-bot/internal/api"
)

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

func TestTokenIDsFromReceiptParsesTransferSingleAndBatch(t *testing.T) {
	receipt := &api.TransactionReceipt{Logs: []api.TransactionLog{
		{
			Address: api.CTFContract,
			Topics:  []string{erc1155TransferSingleTopic},
			Data:    "0x" + hexWord(12345) + hexWord(1000000),
		},
		{
			Address: api.CTFContract,
			Topics:  []string{erc1155TransferBatchTopic},
			Data: "0x" +
				hexWord(64) + // ids offset
				hexWord(192) + // values offset
				hexWord(2) +
				hexWord(777) +
				hexWord(888) +
				hexWord(2) +
				hexWord(10) +
				hexWord(20),
		},
	}}

	got := tokenIDsFromReceipt(receipt)
	want := []string{"12345", "777", "888"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected token IDs: got %v want %v", got, want)
	}
}

func hexWord(value int64) string {
	return leftPadHex(value)
}

func leftPadHex(value int64) string {
	const hexChars = "0123456789abcdef"
	if value == 0 {
		return strings.Repeat("0", 64)
	}
	buf := make([]byte, 0, 64)
	for value > 0 {
		buf = append([]byte{hexChars[value%16]}, buf...)
		value /= 16
	}
	return strings.Repeat("0", 64-len(buf)) + string(buf)
}
