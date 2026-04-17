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

	mu        sync.Mutex
	callbacks []func(conditionID string)
	started   bool
}

func NewResolutionWatcher(wsURL string) *ResolutionWatcher {
	wsURL = ResolvePolygonWSURL("", wsURL)
	if wsURL == "" {
		return nil
	}
	return &ResolutionWatcher{
		wsURL: wsURL,
	}
}

func (w *ResolutionWatcher) RegisterCallback(cb func(conditionID string)) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.callbacks = append(w.callbacks, cb)
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

	// Subscription message for ConditionResolved events
	subMsg := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_subscribe",
		"params": []interface{}{
			"logs",
			map[string]interface{}{
				"address": CTFContract,
				"topics": []string{
					"0x8b39414ea5475e7a828e833f4439c29a50bb913ab028eb0dcfcc9265fdbddfba",
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

	var subResp map[string]interface{}
	subReadCtx, subReadCancel := context.WithTimeout(ctx, 10*time.Second)
	err = wsjson.Read(subReadCtx, c, &subResp)
	subReadCancel()
	if err != nil {
		return fmt.Errorf("subscribe read: %w", err)
	}

	logf("📡 [ResolutionWatcher] Subscribed to ConditionResolved events")

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
			// Typically, conditionId is the second topic (index 1) for ConditionResolved
			// ConditionResolved(bytes32 indexed conditionId, address indexed oracle, bytes32 indexed questionId, uint256 outcomeSlotCount, uint256[] payoutNumerators)
			conditionID := strings.TrimPrefix(event.Params.Result.Topics[1], "0x")
			if len(conditionID) > 64 {
				conditionID = conditionID[len(conditionID)-64:]
			}
			conditionID = "0x" + conditionID

			w.mu.Lock()
			callbacks := append([]func(string){}, w.callbacks...)
			w.mu.Unlock()
			for _, cb := range callbacks {
				go cb(conditionID)
			}
		}
	}
}
