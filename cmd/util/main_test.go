package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"Market-bot/internal/api"
)

func TestUtilbotRefreshRestQuotesFetchesBooksConcurrently(t *testing.T) {
	var inflight int32
	var maxInflight int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := atomic.AddInt32(&inflight, 1)
		for {
			observed := atomic.LoadInt32(&maxInflight)
			if current <= observed || atomic.CompareAndSwapInt32(&maxInflight, observed, current) {
				break
			}
		}
		defer atomic.AddInt32(&inflight, -1)
		time.Sleep(25 * time.Millisecond)

		tokenID := r.URL.Query().Get("token_id")
		w.Header().Set("Content-Type", "application/json")
		switch tokenID {
		case "yes-token":
			_, _ = fmt.Fprint(w, `{"asset_id":"yes-token","bids":[{"price":"0.41","size":"10"}],"asks":[{"price":"0.43","size":"12"}]}`)
		case "no-token":
			_, _ = fmt.Fprint(w, `{"asset_id":"no-token","bids":[{"price":"0.57","size":"8"}],"asks":[{"price":"0.59","size":"11"}]}`)
		default:
			http.Error(w, "unexpected token", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := api.NewRestClient(server.URL)
	store := newUtilbotQuoteStore()
	utilbotRefreshRestQuotes(context.Background(), client, map[string]string{
		"yes-token": "Yes",
		"no-token":  "No",
	}, store)

	snap := store.Snapshot([]string{"Yes", "No"})
	if atomic.LoadInt32(&maxInflight) < 2 {
		t.Fatalf("expected concurrent order book fetches, max inflight=%d", atomic.LoadInt32(&maxInflight))
	}
	if snap.TokenBids["Yes"] != 0.41 || snap.TokenAsks["Yes"] != 0.43 {
		t.Fatalf("unexpected Yes quote snapshot: %+v", snap)
	}
	if snap.TokenBids["No"] != 0.57 || snap.TokenAsks["No"] != 0.59 {
		t.Fatalf("unexpected No quote snapshot: %+v", snap)
	}
	if snap.QuoteState["Yes"].Source != "rest" || snap.QuoteState["No"].Source != "rest" {
		t.Fatalf("expected REST quote source markers, got %+v", snap.QuoteState)
	}
}
