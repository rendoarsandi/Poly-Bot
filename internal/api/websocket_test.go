package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestWSManagerConnect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusInternalError, "the sky is falling")

		for {
			_, _, err := c.Read(r.Context())
			if err != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := strings.Replace(server.URL, "http", "ws", 1)
	mgr := NewWSManager(wsURL)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := mgr.Connect(ctx)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer mgr.Close()

	if mgr.conn == nil {
		t.Fatal("Expected connection to be established, got nil")
	}
}

func TestWSManagerSubscribeRead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusInternalError, "the sky is falling")

		var msg map[string]string
		_ = wsjson.Read(r.Context(), c, &msg)

		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	wsURL := strings.Replace(server.URL, "http", "ws", 1)
	mgr := NewWSManager(wsURL)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_ = mgr.Connect(ctx)
	defer mgr.Close()
	err := mgr.Subscribe(ctx, map[string]string{"type": "subscribe"})
	if err != nil {
		t.Fatalf("Failed to subscribe: %v", err)
	}

	msg, err := mgr.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("Failed to read message: %v", err)
	}

	if string(msg) != `{"status":"ok"}` {
		t.Errorf("Expected status ok, got %s", string(msg))
	}
}

func TestWSManagerDefault(t *testing.T) {
	mgr := NewWSManager("")
	if mgr.URL != "wss://ws-subscriptions-clob.polymarket.com/ws/market" {
		t.Errorf("Expected default URL, got %s", mgr.URL)
	}
}

func TestWSManagerTimeSinceLastDataMessage(t *testing.T) {
	mgr := NewWSManager("")
	mgr.lastDataMessage.Store(time.Now().Add(-2 * time.Second).UnixNano())

	got := mgr.TimeSinceLastDataMessage()
	if got < time.Second || got > 3*time.Second {
		t.Fatalf("expected last data age around 2s, got %v", got)
	}
}

func TestWSManagerTimeSinceLastDataMessageUnsetIsLarge(t *testing.T) {
	mgr := NewWSManager("")
	if got := mgr.TimeSinceLastDataMessage(); got < time.Hour {
		t.Fatalf("expected unset data age to appear stale, got %v", got)
	}
}
