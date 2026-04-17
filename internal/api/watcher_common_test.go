package api

import (
	"fmt"
	"testing"
	"time"
)

func TestWatcherDisconnectSummaryTreatsEOFAsBenign(t *testing.T) {
	msg, benign := watcherDisconnectSummary(fmt.Errorf("read: failed to read JSON message: failed to get reader: failed to read frame header: EOF"))
	if !benign {
		t.Fatal("expected EOF disconnect to be treated as benign")
	}
	if msg == "" {
		t.Fatal("expected benign disconnect summary")
	}
}

func TestWatcherNextBackoffResetsAfterStableSession(t *testing.T) {
	got := watcherNextBackoff(8*time.Second, watcherBackoffResetAfter+time.Second)
	if got != 2*time.Second {
		t.Fatalf("expected backoff reset to 2s after stable session, got %s", got)
	}
}

func TestWatcherNextBackoffCapsAtThirtySeconds(t *testing.T) {
	got := watcherNextBackoff(20*time.Second, 5*time.Second)
	if got != 30*time.Second {
		t.Fatalf("expected backoff cap at 30s, got %s", got)
	}
}
