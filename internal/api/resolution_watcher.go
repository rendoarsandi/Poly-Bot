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

type ResolutionWatcher struct {
	wsURL string

	mu             sync.Mutex
	callbacks      map[uint64]func(conditionID string)
	nextCallbackID uint64
	started        bool
}

const conditionResolutionTopic = "0xb44d84d3289691f71497564b85d4233648d9dbae8cbdbb4329f301c3a0185894"

func NewResolutionWatcher(wsURL string) *ResolutionWatcher {
	wsURL = ResolvePolygonWSURL("", wsURL)
	if wsURL == "" {
		return nil
	}
	return &ResolutionWatcher{
		wsURL:     wsURL,
		callbacks: make(map[uint64]func(conditionID string)),
	}
}

func (w *ResolutionWatcher) RegisterCallback(cb func(conditionID string)) func() {
	if w == nil || cb == nil {
		return func() {}
	}
	w.mu.Lock()
	if w.callbacks == nil {
		w.callbacks = make(map[uint64]func(conditionID string))
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

func (w *ResolutionWatcher) Start(ctx context.Context, logf func(string, ...interface{})) {
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
					logf("📡 [ResolutionWatcher] Connection closed: %s. Reconnecting in %s...", summary, backoff)
				} else {
					logf("📡 [ResolutionWatcher] Disconnected: %v. Reconnecting in %s...", err, backoff)
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

func (w *ResolutionWatcher) dialAndListen(ctx context.Context, logf func(string, ...interface{})) error {
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	c, _, err := websocket.Dial(dialCtx, w.wsURL, nil)
	cancel()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer c.Close(websocket.StatusInternalError, "watcher error")

	// Subscription message for CTF ConditionResolution events.
	subMsg := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_subscribe",
		"params": []interface{}{
			"logs",
			map[string]interface{}{
				"address": CTFContract,
				"topics": []string{
					conditionResolutionTopic,
				},
			},
		},
	}

	subCtx, subCancel := context.WithTimeout(ctx, 10*time.Second)
	err = wsjson.Write(subCtx, c, subMsg)
	subCancel()
	if err != nil {
		return fmt.Errorf("subscribe write: %w", err)
	}

	subReadCtx, subReadCancel := context.WithTimeout(ctx, 10*time.Second)
	err = readWatcherSubscriptionAck(subReadCtx, c, 1, "resolution")
	subReadCancel()
	if err != nil {
		return fmt.Errorf("subscribe read: %w", err)
	}

	logf("📡 [ResolutionWatcher] Subscribed to ConditionResolution events")

	for {
		if ctx.Err() != nil {
			c.Close(websocket.StatusNormalClosure, "shutdown")
			return nil
		}

		var event struct {
			Method string `json:"method"`
			Params struct {
				Result struct {
					Topics []string `json:"topics"`
				} `json:"result"`
			} `json:"params"`
		}

		if err := wsjson.Read(ctx, c, &event); err != nil {
			return fmt.Errorf("read: %w", err)
		}

		if event.Method == "eth_subscription" && len(event.Params.Result.Topics) >= 2 {
			// conditionId is the second topic (index 1) for ConditionResolution:
			// ConditionResolution(bytes32 indexed conditionId, address indexed oracle, bytes32 indexed questionId, uint256 outcomeSlotCount, uint256[] payoutNumerators)
			conditionID := strings.TrimPrefix(event.Params.Result.Topics[1], "0x")
			if len(conditionID) > 64 {
				conditionID = conditionID[len(conditionID)-64:]
			}
			conditionID = "0x" + conditionID

			w.mu.Lock()
			callbacks := make([]func(string), 0, len(w.callbacks))
			for _, cb := range w.callbacks {
				callbacks = append(callbacks, cb)
			}
			w.mu.Unlock()
			for _, cb := range callbacks {
				go cb(conditionID)
			}
		}
	}
}
