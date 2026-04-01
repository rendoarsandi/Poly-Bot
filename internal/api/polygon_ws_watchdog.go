package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

var (
	polygonWSHeartbeatInterval = 20 * time.Second
	polygonWSHeartbeatTimeout  = 5 * time.Second
)

func readPolygonWSJSONWithHeartbeat(ctx context.Context, conn *websocket.Conn, source string, handle func(map[string]json.RawMessage) error) error {
	if conn == nil {
		return fmt.Errorf("%s websocket connection is nil", source)
	}

	msgCh := make(chan map[string]json.RawMessage, 1)
	errCh := make(chan error, 1)

	go func() {
		defer close(msgCh)
		for {
			var raw map[string]json.RawMessage
			if err := wsjson.Read(ctx, conn, &raw); err != nil {
				select {
				case errCh <- fmt.Errorf("%s websocket read failed: %w", source, err):
				default:
				}
				return
			}

			select {
			case msgCh <- raw:
			case <-ctx.Done():
				return
			}
		}
	}()

	lastHealthyAt := time.Now()
	ticker := time.NewTicker(polygonWSHeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			if err != nil {
				return err
			}
		case raw, ok := <-msgCh:
			if !ok {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("%s websocket read loop ended", source)
			}
			lastHealthyAt = time.Now()
			if err := handle(raw); err != nil {
				return err
			}
		case <-ticker.C:
			if ctx.Err() != nil {
				return ctx.Err()
			}

			idleFor := time.Since(lastHealthyAt)
			if idleFor < polygonWSHeartbeatInterval {
				continue
			}

			pingCtx, cancel := context.WithTimeout(ctx, polygonWSHeartbeatTimeout)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("%s websocket ping failed after %s idle: %w", source, idleFor.Round(time.Second), err)
			}
			lastHealthyAt = time.Now()
		}
	}
}
