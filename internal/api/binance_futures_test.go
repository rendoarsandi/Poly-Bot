package api

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestBinanceFlexibleInt64SupportsQuotedAndNumericPayloads(t *testing.T) {
	var quoted binanceFlexibleInt64
	if err := json.Unmarshal([]byte(`"1743168154701"`), &quoted); err != nil {
		t.Fatalf("quoted timestamp unmarshal failed: %v", err)
	}
	if got := int64(quoted); got != 1743168154701 {
		t.Fatalf("quoted timestamp = %d, want 1743168154701", got)
	}

	var numeric binanceFlexibleInt64
	if err := json.Unmarshal([]byte(`1743168154701`), &numeric); err != nil {
		t.Fatalf("numeric timestamp unmarshal failed: %v", err)
	}
	if got := int64(numeric); got != 1743168154701 {
		t.Fatalf("numeric timestamp = %d, want 1743168154701", got)
	}
}

func TestBinanceCombinedStreamHeaderParsesEventType(t *testing.T) {
	type eventEnvelope struct {
		Stream string          `json:"stream"`
		Data   json.RawMessage `json:"data"`
	}
	type eventHeader struct {
		EventType string               `json:"e"`
		EventTime binanceFlexibleInt64 `json:"E"`
	}

	raw := []byte(`{"stream":"btcusdt@aggTrade","data":{"e":"aggTrade","E":1774703713098,"T":1774703712944,"p":"66438.20"}}`)
	var env eventEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("combined stream envelope unmarshal failed: %v", err)
	}

	var header eventHeader
	if err := json.Unmarshal(env.Data, &header); err != nil {
		t.Fatalf("combined stream header unmarshal failed: %v", err)
	}
	if header.EventType != "aggTrade" {
		t.Fatalf("expected event type aggTrade, got %q", header.EventType)
	}
	if int64(header.EventTime) != 1774703713098 {
		t.Fatalf("expected event time 1774703713098, got %d", int64(header.EventTime))
	}
}

func TestBinanceFuturesPriceFeedSnapshotUsesLookbackBaseline(t *testing.T) {
	feed := NewBinanceFuturesPriceFeed("BTCUSDT", 1500*time.Millisecond)
	base := time.Unix(1700000000, 0)
	feed.recordTradeSample(100, base)
	feed.recordTradeSample(100.2, base.Add(800*time.Millisecond))
	feed.recordTradeSample(100.6, base.Add(1700*time.Millisecond))

	snap := feed.Snapshot(base.Add(1700 * time.Millisecond))
	if !snap.Ready {
		t.Fatal("expected snapshot to be ready once lookback window is populated")
	}
	if snap.BaselinePrice != 100 {
		t.Fatalf("expected baseline price 100, got %.4f", snap.BaselinePrice)
	}
	if math.Abs(snap.DeltaPercent-0.6) > 0.000001 {
		t.Fatalf("expected delta 0.6%%, got %.6f%%", snap.DeltaPercent)
	}
}

func TestBinanceFuturesPriceFeedSnapshotWaitsForLookbackWindow(t *testing.T) {
	feed := NewBinanceFuturesPriceFeed("ETHUSDT", 2*time.Second)
	base := time.Unix(1700000000, 0)
	feed.recordTradeSample(200, base)
	feed.recordTradeSample(201, base.Add(1200*time.Millisecond))

	snap := feed.Snapshot(base.Add(1200 * time.Millisecond))
	if snap.Ready {
		t.Fatal("expected snapshot to stay unready before the full lookback window elapses")
	}
}

func TestBinanceFuturesPriceFeedSnapshotResetsAfterGap(t *testing.T) {
	feed := NewBinanceFuturesPriceFeed("BTCUSDT", 1500*time.Millisecond)
	base := time.Unix(1700000000, 0)
	feed.recordTradeSample(100, base)
	feed.recordTradeSample(101, base.Add(4*time.Second))

	snap := feed.Snapshot(base.Add(4 * time.Second))
	if snap.Ready {
		t.Fatal("expected snapshot to stay unready when the lookback baseline is too stale after a stream gap")
	}

	feed.recordTradeSample(101.3, base.Add(4800*time.Millisecond))
	feed.recordTradeSample(101.8, base.Add(5600*time.Millisecond))
	snap = feed.Snapshot(base.Add(5600 * time.Millisecond))
	if !snap.Ready {
		t.Fatal("expected snapshot to become ready again after a full fresh post-gap window")
	}
	if snap.BaselinePrice != 101 {
		t.Fatalf("expected post-gap baseline price 101, got %.4f", snap.BaselinePrice)
	}
}

func TestBinanceFuturesPriceFeedSnapshotUsesMarkPriceOnlyAsDisplayFallback(t *testing.T) {
	feed := NewBinanceFuturesPriceFeed("BTCUSDT", 1500*time.Millisecond)
	base := time.Unix(1700000000, 0)
	feed.recordTradeSample(100.0, base)
	feed.recordMarkPrice(100.3, base.Add(3*time.Second))

	snap := feed.Snapshot(base.Add(3 * time.Second))
	if snap.Ready {
		t.Fatal("expected mark-price heartbeat alone not to make the trade-driven signal ready")
	}
	if snap.Source != "ws-mark" {
		t.Fatalf("expected last source ws-mark, got %q", snap.Source)
	}
	if snap.Price != 100.3 {
		t.Fatalf("expected display price to fall back to mark price 100.3, got %.4f", snap.Price)
	}
	if snap.UpdatedAt != base {
		t.Fatalf("expected signal updated time to remain on last trade sample, got %v", snap.UpdatedAt)
	}
}
