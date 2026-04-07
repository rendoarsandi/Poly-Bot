package copytradeutil

import (
	"testing"

	"Market-bot/internal/api"
)

func TestShortTxHash(t *testing.T) {
	if got := ShortTxHash(" 0x1234567890abcdef "); got != "0x12345678..." {
		t.Fatalf("short tx hash = %q", got)
	}
	if got := ShortTxHash("0x1234"); got != "0x1234" {
		t.Fatalf("short tx hash short input = %q", got)
	}
}

func TestSignalSummary(t *testing.T) {
	trade := api.PublicTrade{
		Outcome:         "Up",
		Side:            "buy",
		Size:            12.5,
		Source:          "onchain",
		TransactionHash: "0x1234567890abcdef",
	}
	got := SignalSummary(trade, func(qty float64) string {
		if qty == 12.5 {
			return "12.5"
		}
		return "bad"
	})
	want := "BUY Up | master=12.5 | source=ONCHAIN | tx=0x12345678..."
	if got != want {
		t.Fatalf("signal summary = %q, want %q", got, want)
	}
}
