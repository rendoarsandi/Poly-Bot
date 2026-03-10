package marketlookup

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
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

func TestCollectMarketsByTimeframesConcurrentlyMergesResults(t *testing.T) {
	timeframes := []string{"15m", "5m", "1h"}
	var inflight int32
	var maxInflight int32
	var started int32
	release := make(chan struct{})

	fetch := func(ctx context.Context, timeframe string) ([]api.Market, error) {
		current := atomic.AddInt32(&inflight, 1)
		for {
			observed := atomic.LoadInt32(&maxInflight)
			if current <= observed || atomic.CompareAndSwapInt32(&maxInflight, observed, current) {
				break
			}
		}
		if atomic.AddInt32(&started, 1) == int32(len(timeframes)) {
			close(release)
		}
		<-release
		defer atomic.AddInt32(&inflight, -1)

		if timeframe == "5m" {
			return nil, fmt.Errorf("boom")
		}
		return []api.Market{{ConditionID: timeframe + "-cond", Slug: timeframe + "-slug"}}, nil
	}

	candidates, err := collectMarketsByTimeframesConcurrently(context.Background(), timeframes, fetch)
	if err == nil || err.Error() != "boom" {
		t.Fatalf("expected fetch error to be retained, got %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("expected 2 successful candidate markets, got %d", len(candidates))
	}
	if _, ok := candidates["15m-cond"]; !ok {
		t.Fatalf("expected 15m result in candidates, got %+v", candidates)
	}
	if _, ok := candidates["1h-cond"]; !ok {
		t.Fatalf("expected 1h result in candidates, got %+v", candidates)
	}
	if atomic.LoadInt32(&maxInflight) < 2 {
		t.Fatalf("expected concurrent timeframe fetches, max inflight=%d", atomic.LoadInt32(&maxInflight))
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
