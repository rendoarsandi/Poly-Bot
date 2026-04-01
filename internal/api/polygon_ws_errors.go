package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const (
	polygonWSRateLimitMinDelay       = 30 * time.Second
	polygonWSPolicyViolationMinDelay = 10 * time.Second
)

type polygonWSDialError struct {
	source     string
	statusCode int
	retryAfter time.Duration
	err        error
}

func (e *polygonWSDialError) Error() string {
	if e == nil {
		return ""
	}
	if e.statusCode > 0 {
		return fmt.Sprintf("%s websocket dial failed: handshake status %d: %v", e.source, e.statusCode, e.err)
	}
	return fmt.Sprintf("%s websocket dial failed: %v", e.source, e.err)
}

func (e *polygonWSDialError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func (e *polygonWSDialError) retryable() bool {
	if e == nil {
		return true
	}
	switch e.statusCode {
	case http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusMethodNotAllowed,
		http.StatusUpgradeRequired:
		return false
	default:
		return true
	}
}

func (e *polygonWSDialError) retryDelay(defaultDelay time.Duration) time.Duration {
	if e == nil {
		return defaultDelay
	}
	if e.retryAfter > 0 {
		return e.retryAfter
	}
	if e.statusCode == http.StatusTooManyRequests && defaultDelay < polygonWSRateLimitMinDelay {
		return polygonWSRateLimitMinDelay
	}
	return defaultDelay
}

func polygonWSRetryDelay(err error, defaultDelay time.Duration) time.Duration {
	if defaultDelay <= 0 {
		defaultDelay = time.Second
	}

	var dialErr *polygonWSDialError
	if errors.As(err, &dialErr) {
		return dialErr.retryDelay(defaultDelay)
	}

	if delay, ok := polygonWSCloseRetryDelay(err, defaultDelay); ok {
		return delay
	}
	return defaultDelay
}

func polygonWSCloseRetryDelay(err error, defaultDelay time.Duration) (time.Duration, bool) {
	if err == nil {
		return defaultDelay, false
	}

	var closeErr websocket.CloseError
	if !errors.As(err, &closeErr) {
		return defaultDelay, false
	}

	reason := strings.ToLower(strings.TrimSpace(closeErr.Reason))
	switch closeErr.Code {
	case websocket.StatusPolicyViolation:
		if strings.Contains(reason, "too many requests") || strings.Contains(reason, "rate limit") {
			if defaultDelay < polygonWSRateLimitMinDelay {
				return polygonWSRateLimitMinDelay, true
			}
			return defaultDelay, true
		}
		if defaultDelay < polygonWSPolicyViolationMinDelay {
			return polygonWSPolicyViolationMinDelay, true
		}
		return defaultDelay, true
	case websocket.StatusTryAgainLater:
		if defaultDelay < polygonWSRateLimitMinDelay {
			return polygonWSRateLimitMinDelay, true
		}
		return defaultDelay, true
	default:
		return defaultDelay, false
	}
}

func newPolygonWSDialError(source string, resp *http.Response, err error) error {
	if err == nil {
		return nil
	}
	wsErr := &polygonWSDialError{
		source: strings.TrimSpace(source),
		err:    err,
	}
	if resp != nil {
		wsErr.statusCode = resp.StatusCode
		wsErr.retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
	}
	return wsErr
}

func parseRetryAfter(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(raw)
	if err != nil {
		return 0
	}
	delay := time.Until(when)
	if delay < 0 {
		return 0
	}
	return delay
}
