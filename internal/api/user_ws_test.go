package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

func TestUserWSClientProcessTradeEvent_EmitsOnlyConfirmedTrade(t *testing.T) {
	client := NewUserWSClient("key", "secret", "pass")
	callCount := 0
	var got OrderFillData
	client.SetOnFill(func(fill OrderFillData) {
		callCount++
		got = fill
	})

	client.processMessage([]byte(`{"event_type":"trade","id":"trade-1","type":"TRADE","status":"MATCHED","taker_order_id":"order-1","asset_id":"asset-1","side":"BUY","size":"4","price":"0.45","market":"cond-1"}`))
	client.processMessage([]byte(`{"event_type":"trade","id":"trade-1","type":"TRADE","status":"MINED","taker_order_id":"order-1","asset_id":"asset-1","side":"BUY","size":"4","price":"0.45","market":"cond-1"}`))
	client.processMessage([]byte(`{"event_type":"trade","id":"trade-1","type":"TRADE","status":"CONFIRMED","taker_order_id":"order-1","asset_id":"asset-1","side":"BUY","size":"4","price":"0.45","market":"cond-1"}`))

	if callCount != 1 {
		t.Fatalf("expected one callback for a trade lifecycle, got %d", callCount)
	}
	if got.Status != "CONFIRMED" {
		t.Fatalf("expected only terminal CONFIRMED status to be emitted, got %q", got.Status)
	}
}

func TestUserWSClientIgnoresRetryingAndFailedTrades(t *testing.T) {
	client := NewUserWSClient("key", "secret", "pass")
	called := false
	client.SetOnFill(func(fill OrderFillData) {
		called = true
	})

	client.processMessage([]byte(`{"event_type":"trade","id":"trade-1","type":"TRADE","status":"RETRYING","taker_order_id":"order-1","asset_id":"asset-1","side":"BUY","size":"4","price":"0.45","market":"cond-1"}`))
	client.processMessage([]byte(`{"event_type":"trade","id":"trade-2","type":"TRADE","status":"FAILED","taker_order_id":"order-2","asset_id":"asset-1","side":"BUY","size":"4","price":"0.45","market":"cond-1"}`))

	if called {
		t.Fatal("expected no callback for RETRYING/FAILED trade events")
	}
}

func TestUserWSClientIgnoresDuplicateConfirmedTradeEvents(t *testing.T) {
	client := NewUserWSClient("key", "secret", "pass")
	callCount := 0
	client.SetOnFill(func(fill OrderFillData) {
		callCount++
	})

	msg := []byte(`{"event_type":"trade","id":"trade-1","type":"TRADE","status":"CONFIRMED","taker_order_id":"order-1","asset_id":"asset-1","side":"BUY","size":"4","price":"0.45","market":"cond-1"}`)
	client.processMessage(msg)
	client.processMessage(msg)

	if callCount != 1 {
		t.Fatalf("expected duplicate CONFIRMED trade to be delivered once, got %d callbacks", callCount)
	}
}

func TestUserWSClientProcessMessage_BatchedTradeArrayEmitsUniqueConfirmedOnly(t *testing.T) {
	client := NewUserWSClient("key", "secret", "pass")
	var got []OrderFillData
	client.SetOnFill(func(fill OrderFillData) {
		got = append(got, fill)
	})

	client.processMessage([]byte(`[
		{"event_type":"trade","id":"trade-1","type":"TRADE","status":"MATCHED","taker_order_id":"order-1","asset_id":"asset-1","side":"BUY","size":"4","price":"0.45","market":"cond-1"},
		{"event_type":"trade","id":"trade-1","type":"TRADE","status":"CONFIRMED","taker_order_id":"order-1","asset_id":"asset-1","side":"BUY","size":"4","price":"0.45","market":"cond-1"},
		{"event_type":"trade","id":"trade-1","type":"TRADE","status":"CONFIRMED","taker_order_id":"order-1","asset_id":"asset-1","side":"BUY","size":"4","price":"0.45","market":"cond-1"},
		{"event_type":"trade","id":"trade-2","type":"TRADE","status":"CONFIRMED","taker_order_id":"order-2","asset_id":"asset-2","side":"SELL","size":"2","price":"0.55","market":"cond-2"}
	]`))

	if len(got) != 2 {
		t.Fatalf("expected two unique confirmed fills from batch, got %d", len(got))
	}
	if got[0].TradeID != "trade-1" || got[1].TradeID != "trade-2" {
		t.Fatalf("unexpected confirmed trade sequence: %+v", got)
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
	client.manager = NewWSManager("polymarket", "", "", "ws"+strings.TrimPrefix(server.URL, "http"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.SubscribeMarkets(ctx, []string{"cond-1"}); err != nil {
		t.Fatalf("initial subscribe failed: %v", err)
	}
	first := <-messages
	if first["type"] != "market" {
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

func TestUserWSClientSubscribeMarketsReconnectsAfterClosedConn(t *testing.T) {
	messages := make(chan map[string]any, 2)
	var connCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept failed: %v", err)
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")

		idx := connCount.Add(1)
		var msg map[string]any
		if err := wsjson.Read(r.Context(), conn, &msg); err != nil {
			t.Errorf("read failed: %v", err)
			return
		}
		messages <- msg

		if idx == 1 {
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	client := NewUserWSClient("key", "secret", "pass")
	client.manager = NewWSManager("polymarket", "", "", "ws"+strings.TrimPrefix(server.URL, "http"))
	client.listenStarted = true

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.SubscribeMarkets(ctx, []string{"cond-1"}); err != nil {
		t.Fatalf("initial subscribe failed: %v", err)
	}
	first := <-messages
	if first["type"] != "market" {
		t.Fatalf("expected initial auth payload, got %+v", first)
	}

	if client.manager.conn == nil {
		t.Fatal("expected manager connection after initial subscribe")
	}
	_ = client.manager.conn.Close(websocket.StatusGoingAway, "simulate dropped conn")

	if err := client.SubscribeMarkets(ctx, []string{"cond-1", "cond-2"}); err != nil {
		t.Fatalf("expected closed connection to recover, got error: %v", err)
	}
	second := <-messages
	if second["type"] != "market" {
		t.Fatalf("expected reconnect auth payload, got %+v", second)
	}
	markets, ok := second["markets"].([]any)
	if !ok || len(markets) != 2 || markets[0] != "cond-1" || markets[1] != "cond-2" {
		t.Fatalf("unexpected reconnect markets payload: %+v", second["markets"])
	}

	_ = client.Close()
}
