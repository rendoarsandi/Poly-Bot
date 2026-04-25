package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const watcherBackoffResetAfter = 30 * time.Second

func watcherSleep(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func watcherNextBackoff(current, connectedFor time.Duration) time.Duration {
	if current <= 0 {
		current = 2 * time.Second
	}
	if connectedFor >= watcherBackoffResetAfter {
		return 2 * time.Second
	}
	next := current * 2
	if next > 30*time.Second {
		next = 30 * time.Second
	}
	return next
}

func watcherDisconnectSummary(err error) (string, bool) {
	if err == nil {
		return "", false
	}

	status := websocket.CloseStatus(err)
	switch status {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway, websocket.StatusNoStatusRcvd:
		return "peer closed the websocket", true
	}

	if errors.Is(err, io.EOF) {
		return "peer closed the websocket (EOF)", true
	}

	errText := strings.ToLower(strings.TrimSpace(err.Error()))
	switch {
	case strings.Contains(errText, "failed to read frame header: eof"),
		strings.Contains(errText, "unexpected eof"),
		strings.Contains(errText, "failed to get reader: eof"),
		strings.Contains(errText, "context canceled"),
		strings.Contains(errText, "use of closed network connection"):
		return "peer closed the websocket", true
	default:
		return "", false
	}
}

type watcherSubscriptionError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type watcherSubscriptionResponse struct {
	ID     int                       `json:"id"`
	Result string                    `json:"result"`
	Error  *watcherSubscriptionError `json:"error"`
}

func readWatcherSubscriptionAck(ctx context.Context, c *websocket.Conn, wantID int, label string) error {
	var resp watcherSubscriptionResponse
	if err := wsjson.Read(ctx, c, &resp); err != nil {
		return err
	}
	if resp.ID != wantID {
		return fmt.Errorf("%s subscription id mismatch: got %d, want %d", label, resp.ID, wantID)
	}
	if resp.Error != nil {
		return fmt.Errorf("%s subscription rejected: %d %s", label, resp.Error.Code, resp.Error.Message)
	}
	if strings.TrimSpace(resp.Result) == "" {
		return fmt.Errorf("%s subscription missing result", label)
	}
	return nil
}
