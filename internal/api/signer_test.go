package api

import (
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
)

func TestSignDynamicFeeTransactionProducesType2Tx(t *testing.T) {
	signer, err := NewSigner(strings.Repeat("1", 64))
	if err != nil {
		t.Fatalf("NewSigner() error = %v", err)
	}

	rawTx, err := signer.SignDynamicFeeTransaction(
		7,
		CTFContract,
		big.NewInt(0),
		210000,
		big.NewInt(300),
		big.NewInt(100),
		"0x01b7037c",
	)
	if err != nil {
		t.Fatalf("SignDynamicFeeTransaction() error = %v", err)
	}

	payload, err := hex.DecodeString(strings.TrimPrefix(rawTx, "0x"))
	if err != nil {
		t.Fatalf("decode raw tx: %v", err)
	}

	var tx types.Transaction
	if err := tx.UnmarshalBinary(payload); err != nil {
		t.Fatalf("UnmarshalBinary() error = %v", err)
	}
	if tx.Type() != types.DynamicFeeTxType {
		t.Fatalf("expected type-2 dynamic fee tx, got type %d", tx.Type())
	}
	if tx.GasFeeCap().Cmp(big.NewInt(300)) != 0 {
		t.Fatalf("expected max fee cap 300, got %s", tx.GasFeeCap())
	}
	if tx.GasTipCap().Cmp(big.NewInt(100)) != 0 {
		t.Fatalf("expected max priority fee 100, got %s", tx.GasTipCap())
	}
}
