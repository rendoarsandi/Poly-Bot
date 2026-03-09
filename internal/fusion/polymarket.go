package fusion

import (
	"context"
	"fmt"
	"sync"
	"time"

	"Market-bot/internal/api"
)

type tokenRef struct {
	Asset   string
	Outcome string
}

type PolymarketFeed struct {
	mu      sync.RWMutex
	ws      *api.WSManager
	msgCh   <-chan []byte
	lookup  map[string]tokenRef
	assets  []string
	started time.Time
}

func NewPolymarketFeed() *PolymarketFeed {
	return &PolymarketFeed{lookup: make(map[string]tokenRef)}
}

func (f *PolymarketFeed) Reset(ctx context.Context, markets map[string]*trackedMarket) error {
	f.Close()
	if len(markets) == 0 {
		return nil
	}

	assetIDs := make([]string, 0, len(markets)*2)
	lookup := make(map[string]tokenRef, len(markets)*2)
	assets := make([]string, 0, len(markets))
	for asset, market := range markets {
		assets = append(assets, asset)
		for _, token := range market.Market.Tokens {
			assetIDs = append(assetIDs, token.TokenID)
			lookup[token.TokenID] = tokenRef{Asset: asset, Outcome: token.Outcome}
		}
	}

	ws := api.NewWSManager("")
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		err = ws.Connect(ctx)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * time.Second):
		}
	}
	if err != nil {
		return fmt.Errorf("polymarket ws connect failed: %w", err)
	}

	sub := map[string]interface{}{"type": "market", "assets_ids": assetIDs}
	if err := ws.Subscribe(ctx, sub); err != nil {
		ws.Close()
		return fmt.Errorf("polymarket ws subscribe failed: %w", err)
	}

	f.mu.Lock()
	f.ws = ws
	f.msgCh = ws.StartStreaming(ctx)
	f.lookup = lookup
	f.assets = assets
	f.started = time.Now()
	f.mu.Unlock()
	return nil
}

func (f *PolymarketFeed) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ws != nil {
		_ = f.ws.Close()
	}
	f.ws = nil
	f.msgCh = nil
	f.lookup = make(map[string]tokenRef)
	f.assets = nil
	f.started = time.Time{}
}

func (f *PolymarketFeed) Messages() <-chan []byte {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.msgCh
}

func (f *PolymarketFeed) Lookup(assetID string) (tokenRef, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	ref, ok := f.lookup[assetID]
	return ref, ok
}

func (f *PolymarketFeed) IsConnected() bool {
	f.mu.RLock()
	ws := f.ws
	f.mu.RUnlock()
	return ws != nil && ws.IsConnected()
}

func (f *PolymarketFeed) TimeSinceLastDataMessage() time.Duration {
	f.mu.RLock()
	ws := f.ws
	f.mu.RUnlock()
	if ws == nil {
		return time.Duration(1<<63 - 1)
	}
	return ws.TimeSinceLastDataMessage()
}

func (f *PolymarketFeed) PingLatency() time.Duration {
	f.mu.RLock()
	ws := f.ws
	f.mu.RUnlock()
	if ws == nil {
		return 0
	}
	return ws.PingLatency()
}
