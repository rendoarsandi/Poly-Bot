package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type MinedPolymarketSignal struct {
	ObservedAt     time.Time
	SignalID       string
	TxHash         string
	Wallet         string
	TokenID        string
	ConditionID    string
	Slug           string
	Outcome        string
	Side           string
	Size           float64
	BlockNumber    uint64
	BlockTimestamp int64
}

type PolymarketMinedWatcher struct {
	wsURL        string
	polygon      *PolygonClient
	rest         *RestClient
	targetWallet string

	mu         sync.Mutex
	recent     []MinedPolymarketSignal
	seen       map[string]time.Time
	tokenCache map[string]pendingResolvedToken
	started    bool
	lastBlock  uint64
}

func NewPolymarketMinedWatcher(wsURL string, polygon *PolygonClient, rest *RestClient, targetWallet string) *PolymarketMinedWatcher {
	wsURL = ResolvePolygonWSURL("", wsURL)
	targetWallet = NormalizeWalletAddress(targetWallet)
	if wsURL == "" || polygon == nil || rest == nil || !IsWalletAddress(targetWallet) {
		return nil
	}
	return &PolymarketMinedWatcher{
		wsURL:        wsURL,
		polygon:      polygon,
		rest:         rest,
		targetWallet: targetWallet,
		seen:         make(map[string]time.Time),
		tokenCache:   make(map[string]pendingResolvedToken),
	}
}

func ResolvePolygonWSURL(explicitWSURL, polygonRPCURL string) string {
	explicitWSURL = strings.TrimSpace(explicitWSURL)
	if explicitWSURL != "" {
		if normalized := normalizePolygonWSURL(explicitWSURL); normalized != "" {
			return normalized
		}
		return explicitWSURL
	}
	return normalizePolygonWSURL(polygonRPCURL)
}

func normalizePolygonWSURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	switch strings.ToLower(parsed.Scheme) {
	case "wss", "ws":
		return parsed.String()
	case "https":
		parsed.Scheme = "wss"
		// Infura requires /ws/v3/ for WebSockets, but the HTTP endpoint is just /v3/
		if strings.Contains(strings.ToLower(parsed.Host), "infura.io") && !strings.Contains(strings.ToLower(parsed.Path), "/ws/") {
			parsed.Path = "/ws" + parsed.Path
		}
		return parsed.String()
	case "http":
		parsed.Scheme = "ws"
		if strings.Contains(strings.ToLower(parsed.Host), "infura.io") && !strings.Contains(strings.ToLower(parsed.Path), "/ws/") {
			parsed.Path = "/ws" + parsed.Path
		}
		return parsed.String()
	default:
		return ""
	}
}

func (w *PolymarketMinedWatcher) Enabled() bool {
	return w != nil && w.wsURL != "" && w.polygon != nil && w.rest != nil && IsWalletAddress(w.targetWallet)
}

func (w *PolymarketMinedWatcher) PrimeTrackedMarkets(markets []*Market) {
	if w == nil {
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	for _, market := range markets {
		if market == nil {
			continue
		}
		conditionID := strings.TrimSpace(market.ConditionID)
		if conditionID == "" {
			continue
		}
		slug := strings.TrimSpace(market.Slug)
		if slug == "" {
			slug = strings.TrimSpace(market.MarketSlug)
		}
		for _, token := range market.Tokens {
			tokenID := strings.TrimSpace(token.TokenID)
			outcome := strings.TrimSpace(token.Outcome)
			if tokenID == "" || outcome == "" {
				continue
			}
			w.tokenCache[tokenID] = pendingResolvedToken{
				market: Market{
					ConditionID: conditionID,
					Slug:        slug,
					MarketSlug:  strings.TrimSpace(market.MarketSlug),
				},
				outcome: outcome,
			}
		}
	}
}

func (w *PolymarketMinedWatcher) SignalsSince(conditionID string, since time.Time) []MinedPolymarketSignal {
	if w == nil {
		return nil
	}
	conditionID = strings.TrimSpace(conditionID)
	now := time.Now()

	w.mu.Lock()
	defer w.mu.Unlock()

	filtered := make([]MinedPolymarketSignal, 0, len(w.recent))
	kept := w.recent[:0]
	for _, sig := range w.recent {
		if now.Sub(sig.ObservedAt) > 5*time.Minute {
			continue
		}
		kept = append(kept, sig)
		if !since.IsZero() && !sig.ObservedAt.After(since) {
			continue
		}
		if conditionID != "" && !strings.EqualFold(sig.ConditionID, conditionID) {
			continue
		}
		filtered = append(filtered, sig)
	}
	w.recent = kept

	for key, seenAt := range w.seen {
		if now.Sub(seenAt) > 10*time.Minute {
			delete(w.seen, key)
		}
	}
	return filtered
}

func (w *PolymarketMinedWatcher) Start(ctx context.Context, logf func(string, ...interface{})) {
	if !w.Enabled() {
		return
	}

	w.mu.Lock()
	if w.started {
		w.mu.Unlock()
		return
	}
	w.started = true
	w.mu.Unlock()

	go w.run(ctx, logf)
}

func (w *PolymarketMinedWatcher) run(ctx context.Context, logf func(string, ...interface{})) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		
		sessionStart := time.Now()
		err := w.runSession(ctx)
		sessionDuration := time.Since(sessionStart)

		if err != nil && ctx.Err() == nil {
			delay := backoff
			var dialErr *polygonWSDialError
			if errors.As(err, &dialErr) {
				if !dialErr.retryable() {
					if logf != nil {
						logf("⚠️ Copytrade mined watcher stopped: %v", err)
					}
					return
				}
				delay = dialErr.retryDelay(backoff)
			}
			
			if logf != nil {
				logf("⚠️ Copytrade mined watcher reconnecting: %v", err)
			}

			// Add jitter to avoid multiple watchers syncing reconnects
			jitter := time.Duration(100+((sessionStart.UnixNano()%500)*1000000)) * time.Nanosecond
			
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay + jitter):
			}

			// Update backoff for next time
			if delay > backoff {
				backoff = delay
			} else if backoff < 60*time.Second {
				backoff *= 2
				if backoff > 60*time.Second {
					backoff = 60 * time.Second
				}
			}
		} else if err == nil {
			// Successful session ended without error (likely context done)
			// Reset backoff ONLY if the session was long enough to be considered stable
			if sessionDuration > 30*time.Second {
				backoff = time.Second
			}
		}
	}
}

func (w *PolymarketMinedWatcher) runSession(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conn, resp, err := websocket.Dial(dialCtx, w.wsURL, &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return newPolygonWSDialError("mined", resp, err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(5 * 1024 * 1024) // 5MB limit for full block payloads

	subReq := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_subscribe",
		"params":  []interface{}{"newHeads"},
	}
	if err := wsjson.Write(ctx, conn, subReq); err != nil {
		return fmt.Errorf("mined websocket subscribe failed: %w", err)
	}

	for {
		var raw map[string]json.RawMessage
		if err := wsjson.Read(ctx, conn, &raw); err != nil {
			return fmt.Errorf("mined websocket read failed: %w", err)
		}
		paramsRaw, ok := raw["params"]
		if !ok {
			continue
		}
		var params struct {
			Result struct {
				Number string `json:"number"`
			} `json:"result"`
		}
		if err := json.Unmarshal(paramsRaw, &params); err != nil {
			continue
		}
		headNum, err := parseHexUint64(params.Result.Number)
		if err != nil || headNum == 0 {
			continue
		}
		if err := w.processBlocks(ctx, headNum); err != nil {
			return err
		}
	}
}

func (w *PolymarketMinedWatcher) processBlocks(ctx context.Context, headNum uint64) error {
	w.mu.Lock()
	start := headNum
	if w.lastBlock > 0 && headNum > w.lastBlock {
		start = w.lastBlock + 1
	}
	if w.lastBlock > 0 && headNum <= w.lastBlock {
		w.mu.Unlock()
		return nil
	}
	w.mu.Unlock()

	for blockNum := start; blockNum <= headNum; blockNum++ {
		blockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		block, err := w.polygon.GetFullBlockByNumber(blockCtx, blockNum)
		cancel()
		if err != nil {
			return fmt.Errorf("get block %d failed: %w", blockNum, err)
		}
		if block == nil {
			continue
		}
		w.handleBlock(ctx, block)
		w.mu.Lock()
		if blockNum > w.lastBlock {
			w.lastBlock = blockNum
		}
		w.mu.Unlock()
	}
	return nil
}

func (w *PolymarketMinedWatcher) handleBlock(parentCtx context.Context, block *FullBlock) {
	if w == nil || block == nil {
		return
	}
	blockNumber, _ := parseHexUint64(block.Number)
	blockTimestamp, _ := parseHexInt64(block.Timestamp)
	observedAt := time.Now()
	if blockTimestamp > 0 {
		observedAt = time.Unix(blockTimestamp, 0)
	}

	for _, tx := range block.Transactions {
		if !strings.Contains(tx.Input, polymarketMatchOrdersSelector[2:]) {
			continue
		}
		orders, err := DecodePolymarketMatchOrdersInput(tx.Input)
		if err != nil || len(orders) == 0 {
			continue
		}
		for idx, order := range orders {
			if !strings.EqualFold(order.Maker, w.targetWallet) && !strings.EqualFold(order.Signer, w.targetWallet) && !strings.EqualFold(order.Taker, w.targetWallet) {
				continue
			}
			side := order.sideString()
			if side == "" {
				continue
			}
			size := order.fillSize()
			if size <= 0.01 {
				continue
			}
			resolveCtx, cancel := context.WithTimeout(parentCtx, 3*time.Second)
			resolved, err := w.resolveToken(resolveCtx, order.TokenID)
			cancel()
			if err != nil {
				continue
			}
			sig := MinedPolymarketSignal{
				ObservedAt:     observedAt,
				SignalID:       fmt.Sprintf("%s:%d", strings.TrimSpace(tx.Hash), idx),
				TxHash:         strings.TrimSpace(tx.Hash),
				Wallet:         w.targetWallet,
				TokenID:        order.TokenID,
				ConditionID:    resolved.market.ConditionID,
				Slug:           resolved.market.Slug,
				Outcome:        resolved.outcome,
				Side:           side,
				Size:           size,
				BlockNumber:    blockNumber,
				BlockTimestamp: blockTimestamp,
			}
			w.storeSignal(sig)
		}
	}
}

func (w *PolymarketMinedWatcher) resolveToken(ctx context.Context, tokenID string) (pendingResolvedToken, error) {
	tokenID = strings.TrimSpace(tokenID)
	if tokenID == "" {
		return pendingResolvedToken{}, fmt.Errorf("token id is empty")
	}

	w.mu.Lock()
	if cached, ok := w.tokenCache[tokenID]; ok {
		w.mu.Unlock()
		return cached, nil
	}
	w.mu.Unlock()

	event, err := w.rest.GetEventByTokenID(ctx, tokenID)
	if err != nil {
		return pendingResolvedToken{}, err
	}
	market, outcome, ok := marketFromGammaEventTokenID(event, tokenID)
	if !ok {
		return pendingResolvedToken{}, fmt.Errorf("token %s could not be mapped to a market", tokenID)
	}
	resolved := pendingResolvedToken{market: market, outcome: outcome}

	w.mu.Lock()
	w.tokenCache[tokenID] = resolved
	w.mu.Unlock()
	return resolved, nil
}

func (w *PolymarketMinedWatcher) storeSignal(sig MinedPolymarketSignal) {
	key := strings.TrimSpace(sig.SignalID)
	if key == "" {
		key = fmt.Sprintf("%s|%s|%s|%s|%.6f", sig.TxHash, sig.TokenID, sig.Outcome, sig.Side, sig.Size)
	}
	now := time.Now()

	w.mu.Lock()
	defer w.mu.Unlock()

	if seenAt, exists := w.seen[key]; exists && now.Sub(seenAt) < 5*time.Minute {
		return
	}
	w.seen[key] = now
	w.recent = append(w.recent, sig)
	if len(w.recent) > 512 {
		w.recent = append([]MinedPolymarketSignal(nil), w.recent[len(w.recent)-512:]...)
	}
}
