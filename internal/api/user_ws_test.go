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

func TestUserWSClientProcessTradeEvent(t *testing.T) {
	client := NewUserWSClient("key", "secret", "pass")
	var got OrderFillData
	called := false
	client.SetOnFill(func(fill OrderFillData) {
		called = true
		got = fill
	})

	client.processMessage([]byte(`{"event_type":"trade","id":"trade-1","type":"TRADE","status":"CONFIRMED","taker_order_id":"order-1","asset_id":"asset-1","side":"BUY","size":"4","price":"0.45","market":"cond-1"}`))

	if !called {
		t.Fatal("expected trade callback to fire for CONFIRMED trade event")
	}
	if got.OrderID != "order-1" {
		t.Fatalf("expected OrderID order-1, got %q", got.OrderID)
	}
	if got.AssetID != "asset-1" || got.Side != "BUY" || got.Size != "4" {
		t.Fatalf("unexpected fill payload: %+v", got)
	}
}

func TestUserWSClientSubscribeMarketsSendsAuthAndDynamicSubscribe(t *testing.T) {
	messages := make(chan map[string]any, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept failed: %v", err)
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")

		for i := 0; i < 2; i++ {
			var msg map[string]any
			if err := wsjson.Read(r.Context(), conn, &msg); err != nil {
				t.Errorf("read failed: %v", err)
				return
			}
			messages <- msg
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	client := NewUserWSClient("key", "secret", "pass")
	client.manager = NewWSManager("ws" + strings.TrimPrefix(server.URL, "http"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.SubscribeMarkets(ctx, []string{"cond-1"}); err != nil {
		t.Fatalf("initial subscribe failed: %v", err)
	}
	first := <-messages
	if first["type"] != "user" {
		t.Fatalf("expected initial auth subscription, got %+v", first)
	}
	markets, ok := first["markets"].([]any)
	if !ok || len(markets) != 1 || markets[0] != "cond-1" {
		t.Fatalf("unexpected auth markets payload: %+v", first["markets"])
	}

	if err := client.SubscribeMarkets(ctx, []string{"cond-1", "cond-2"}); err != nil {
		t.Fatalf("dynamic subscribe failed: %v", err)
	}
	second := <-messages
	if second["operation"] != "subscribe" {
		t.Fatalf("expected dynamic subscribe payload, got %+v", second)
	}
	moreMarkets, ok := second["markets"].([]any)
	if !ok || len(moreMarkets) != 1 || moreMarkets[0] != "cond-2" {
		t.Fatalf("unexpected dynamic markets payload: %+v", second["markets"])
	}

	_ = client.Close()
}
