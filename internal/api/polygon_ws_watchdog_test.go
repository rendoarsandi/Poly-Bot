package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestReadPolygonWSJSONWithHeartbeatKeepsIdleConnectionAlive(t *testing.T) {
	oldInterval := polygonWSHeartbeatInterval
	oldTimeout := polygonWSHeartbeatTimeout
	polygonWSHeartbeatInterval = 20 * time.Millisecond
	polygonWSHeartbeatTimeout = 200 * time.Millisecond
	defer func() {
		polygonWSHeartbeatInterval = oldInterval
		polygonWSHeartbeatTimeout = oldTimeout
	}()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		go func() {
			for {
				if _, _, err := conn.Read(r.Context()); err != nil {
					return
				}
			}
		}()

		select {
		case <-time.After(60 * time.Millisecond):
		case <-r.Context().Done():
			return
		}

		_ = wsjson.Write(r.Context(), conn, map[string]any{
			"jsonrpc": "2.0",
			"params": map[string]any{
				"result": map[string]any{"number": "0x1"},
			},
		})

		select {
		case <-time.After(200 * time.Millisecond):
		case <-r.Context().Done():
		}
	}))
	defer server.Close()

	wsURL := strings.Replace(server.URL, "http", "ws", 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	messageSeen := make(chan struct{}, 1)
	err = readPolygonWSJSONWithHeartbeat(ctx, conn, "test", func(raw map[string]json.RawMessage) error {
		select {
		case messageSeen <- struct{}{}:
		default:
		}
		cancel()
		return nil
	})
	if err != nil && err != context.Canceled {
		t.Fatalf("unexpected watchdog error: %v", err)
	}

	select {
	case <-messageSeen:
	case <-time.After(time.Second):
		t.Fatal("expected message after idle heartbeat")
	}
}

func TestReadPolygonWSJSONWithHeartbeatHandlesReaderExitAndErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		// Immediately close the connection to force a read error
	}))
	defer server.Close()

	wsURL := strings.Replace(server.URL, "http", "ws", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// This should exit with a read error because the server closed connection immediately
	err = readPolygonWSJSONWithHeartbeat(ctx, conn, "test", func(raw map[string]json.RawMessage) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected read error, got nil")
	}
	if !strings.Contains(err.Error(), "websocket read failed") {
		t.Errorf("expected error message to contain 'websocket read failed', got: %v", err)
	}
}
