package api

import (
	"math"
	"testing"
	"time"
)

func TestBinanceFuturesPriceFeedSnapshotUsesLookbackBaseline(t *testing.T) {
	feed := NewBinanceFuturesPriceFeed("BTCUSDT", 1500*time.Millisecond)
	base := time.Unix(1700000000, 0)
	feed.recordSample(100, base, "ws")
	feed.recordSample(100.2, base.Add(800*time.Millisecond), "ws")
	feed.recordSample(100.6, base.Add(1700*time.Millisecond), "ws")

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
	feed.recordSample(200, base, "ws")
	feed.recordSample(201, base.Add(1200*time.Millisecond), "ws")

	snap := feed.Snapshot(base.Add(1200 * time.Millisecond))
	if snap.Ready {
		t.Fatal("expected snapshot to stay unready before the full lookback window elapses")
	}
}

func TestBinanceFuturesPriceFeedSnapshotResetsAfterGap(t *testing.T) {
	feed := NewBinanceFuturesPriceFeed("BTCUSDT", 1500*time.Millisecond)
	base := time.Unix(1700000000, 0)
	feed.recordSample(100, base, "ws")
	feed.recordSample(101, base.Add(4*time.Second), "ws")

	snap := feed.Snapshot(base.Add(4 * time.Second))
	if snap.Ready {
		t.Fatal("expected snapshot to stay unready after a stream gap until fresh lookback history rebuilds")
	}

	feed.recordSample(101.3, base.Add(4800*time.Millisecond), "ws")
	feed.recordSample(101.8, base.Add(5600*time.Millisecond), "ws")
	snap = feed.Snapshot(base.Add(5600 * time.Millisecond))
	if !snap.Ready {
		t.Fatal("expected snapshot to become ready again after a full fresh post-gap window")
	}
	if snap.BaselinePrice != 101 {
		t.Fatalf("expected post-gap baseline price 101, got %.4f", snap.BaselinePrice)
	}
}
