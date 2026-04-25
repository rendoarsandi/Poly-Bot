package api

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type InventoryWatcher struct {
	wsURL   string
	address string

	mu             sync.Mutex
	callbacks      map[uint64]func()
	nextCallbackID uint64
	started        bool
}

func NewInventoryWatcher(wsURL string, walletAddress string) *InventoryWatcher {
	wsURL = ResolvePolygonWSURL("", wsURL)
	if wsURL == "" || walletAddress == "" {
		return nil
	}
	return &InventoryWatcher{
		wsURL:     wsURL,
		address:   strings.ToLower(walletAddress),
		callbacks: make(map[uint64]func()),
	}
}

func (w *InventoryWatcher) RegisterCallback(cb func()) func() {
	if w == nil || cb == nil {
		return func() {}
	}
	w.mu.Lock()
	if w.callbacks == nil {
		w.callbacks = make(map[uint64]func())
	}
	w.nextCallbackID++
	id := w.nextCallbackID
	w.callbacks[id] = cb
	defer w.mu.Unlock()
	return func() {
		w.mu.Lock()
		delete(w.callbacks, id)
		w.mu.Unlock()
	}
}

func (w *InventoryWatcher) Start(ctx context.Context, logf func(string, ...interface{})) {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.started {
		w.mu.Unlock()
		return
	}
	w.started = true
	w.mu.Unlock()

	go func() {
		backoff := 2 * time.Second
		for {
			if ctx.Err() != nil {
				return
			}
			connectedAt := time.Now()
			err := w.dialAndListen(ctx, logf)
			if err != nil && ctx.Err() == nil {
				if summary, benign := watcherDisconnectSummary(err); benign {
					logf("📡 [InventoryWatcher] Connection closed: %s. Reconnecting in %s...", summary, backoff)
				} else {
					logf("📡 [InventoryWatcher] Disconnected: %v. Reconnecting in %s...", err, backoff)
				}
			} else if ctx.Err() != nil {
				return
			}

			if !watcherSleep(ctx, backoff) {
				return
			}
			backoff = watcherNextBackoff(backoff, time.Since(connectedAt))
		}
	}()
}

func (w *InventoryWatcher) dialAndListen(ctx context.Context, logf func(string, ...interface{})) error {
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	c, _, err := websocket.Dial(dialCtx, w.wsURL, nil)
	cancel()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer c.Close(websocket.StatusInternalError, "watcher error")

	// ERC-1155 TransferSingle: 0xc3d58168c5ae7397731d063d5bbf3d657854427343f4c083240f7aacaa2d0f62
	// ERC-1155 TransferBatch:  0x4a39dc06d4c0dbc64b70af90fd698a233a518aa5d07e595d983b8c0526c8f7ce

	addressPadded := "0x000000000000000000000000" + strings.TrimPrefix(w.address, "0x")

	// Subscribe to incoming transfers
	subMsgIn := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_subscribe",
		"params": []interface{}{
			"logs",
			map[string]interface{}{
				"address": CTFContract,
				"topics": []interface{}{
					[]string{
						"0xc3d58168c5ae7397731d063d5bbf3d657854427343f4c083240f7aacaa2d0f62",
						"0x4a39dc06d4c0dbc64b70af90fd698a233a518aa5d07e595d983b8c0526c8f7ce",
					},
					nil,
					nil,
					addressPadded, // to
				},
			},
		},
	}

	// Subscribe to outgoing transfers
	subMsgOut := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "eth_subscribe",
		"params": []interface{}{
			"logs",
			map[string]interface{}{
				"address": CTFContract,
				"topics": []interface{}{
					[]string{
						"0xc3d58168c5ae7397731d063d5bbf3d657854427343f4c083240f7aacaa2d0f62",
						"0x4a39dc06d4c0dbc64b70af90fd698a233a518aa5d07e595d983b8c0526c8f7ce",
					},
					nil,
					addressPadded, // from
				},
			},
		},
	}

	subCtx, subCancel := context.WithTimeout(ctx, 10*time.Second)
	if err := wsjson.Write(subCtx, c, subMsgIn); err != nil {
		subCancel()
		return fmt.Errorf("subscribe write incoming: %w", err)
	}
	if err := wsjson.Write(subCtx, c, subMsgOut); err != nil {
		subCancel()
		return fmt.Errorf("subscribe write outgoing: %w", err)
	}
	subCancel()

	// Read both subscription responses
	for _, ack := range []struct {
		id    int
		label string
	}{
		{id: 1, label: "incoming inventory"},
		{id: 2, label: "outgoing inventory"},
	} {
		subReadCtx, subReadCancel := context.WithTimeout(ctx, 10*time.Second)
		if err := readWatcherSubscriptionAck(subReadCtx, c, ack.id, ack.label); err != nil {
			subReadCancel()
			return fmt.Errorf("subscribe read %d: %w", ack.id, err)
		}
		subReadCancel()
	}

	logf("📡 [InventoryWatcher] Subscribed to CTF Transfer events for %s", w.address)

	for {
		if ctx.Err() != nil {
			c.Close(websocket.StatusNormalClosure, "shutdown")
			return nil
		}

		var event struct {
			Method string `json:"method"`
		}

		if err := wsjson.Read(ctx, c, &event); err != nil {
			return fmt.Errorf("read: %w", err)
		}

		if event.Method == "eth_subscription" {
			w.mu.Lock()
			callbacks := make([]func(), 0, len(w.callbacks))
			for _, cb := range w.callbacks {
				callbacks = append(callbacks, cb)
			}
			w.mu.Unlock()
			for _, cb := range callbacks {
				go cb()
			}
		}
	}
}
