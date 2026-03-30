package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
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
	if e.statusCode == http.StatusTooManyRequests && defaultDelay < 30*time.Second {
		return 30 * time.Second
	}
	return defaultDelay
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
