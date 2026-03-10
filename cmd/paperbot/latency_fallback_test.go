package main

import (
	"testing"
	"time"
)

func TestShouldPaperRestFallbackRespectsPollInterval(t *testing.T) {
	if shouldPaperRestFallback(3500*time.Millisecond, 900*time.Millisecond, 3*time.Second, time.Second) {
		t.Fatal("expected REST fallback to stay blocked until poll interval elapses")
	}
	if !shouldPaperRestFallback(3500*time.Millisecond, 1100*time.Millisecond, 3*time.Second, time.Second) {
		t.Fatal("expected REST fallback once stale age and poll interval are both exceeded")
	}
}
