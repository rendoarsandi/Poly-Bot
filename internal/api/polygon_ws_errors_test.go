package api

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestParseRetryAfter(t *testing.T) {
	if got := parseRetryAfter("30"); got != 30*time.Second {
		t.Fatalf("unexpected numeric retry-after delay %v", got)
	}

	future := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(future); got <= 0 {
		t.Fatalf("expected positive delay from http-date, got %v", got)
	}
}

func TestPolygonWSDialErrorRetryable(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		retryable  bool
	}{
		{name: "unauthorized", statusCode: http.StatusUnauthorized, retryable: false},
		{name: "forbidden", statusCode: http.StatusForbidden, retryable: false},
		{name: "not found", statusCode: http.StatusNotFound, retryable: false},
		{name: "too many requests", statusCode: http.StatusTooManyRequests, retryable: true},
		{name: "server error", statusCode: http.StatusBadGateway, retryable: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := &polygonWSDialError{
				source:     "pending",
				statusCode: tc.statusCode,
				err:        errors.New("dial failed"),
			}
			if got := err.retryable(); got != tc.retryable {
				t.Fatalf("unexpected retryable=%v want %v", got, tc.retryable)
			}
		})
	}
}

func TestPolygonWSDialErrorRetryDelay(t *testing.T) {
	err := &polygonWSDialError{
		source:     "pending",
		statusCode: http.StatusTooManyRequests,
		err:        errors.New("dial failed"),
	}
	if got := err.retryDelay(time.Second); got != 30*time.Second {
		t.Fatalf("unexpected 429 retry delay %v", got)
	}

	err.retryAfter = 45 * time.Second
	if got := err.retryDelay(time.Second); got != 45*time.Second {
		t.Fatalf("unexpected retry-after delay %v", got)
	}
}

func TestPolygonWSRetryDelayClosePolicyViolation(t *testing.T) {
	err := fmt.Errorf("mined websocket read failed: %w", websocket.CloseError{
		Code:   websocket.StatusPolicyViolation,
		Reason: "Too Many Requests",
	})

	if got := polygonWSRetryDelay(err, time.Second); got != 30*time.Second {
		t.Fatalf("unexpected policy violation retry delay %v", got)
	}
}
