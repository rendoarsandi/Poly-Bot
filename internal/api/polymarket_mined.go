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

const (
	// Warn when reconnect catch-up spans more than a handful of blocks so
	// operators can distinguish backlog processing from live heads.
	polymarketMinedCatchupWarnBlocks = uint64(6)
	// Cap reconnect replay so a brief WS outage does not monopolize RPC quota.
	// Older missed fills are expected to be repaired by the copytrade position
	// reconciliation path instead of replaying an unbounded block backlog.
	polymarketMinedCatchupReplayCap = uint64(8)
	// Hard cap eth_getBlockByNumber pacing for mined watcher processing.
	polymarketMinedBlockFetchMinGap = 250 * time.Millisecond
	// Collapse split transfer logs from the same tx into a single signal before
	// copytrade consumes them. This avoids double-counting fee/rebate legs that
	// arrive as separate ERC1155 transfers for one master fill.
	polymarketMinedTransferAggregateWindow = 250 * time.Millisecond
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

	mu                    sync.Mutex
	recent                []MinedPolymarketSignal
	seen                  map[string]time.Time
	tokenCache            map[string]pendingResolvedToken
	lastDiscoveryFallback time.Time
	started               bool
	lastBlock             uint64
	lastBlockFetchAt      time.Time
	pendingTransfers      map[string]MinedPolymarketSignal
	logf                  func(string, ...interface{})
}

func NewPolymarketMinedWatcher(wsURL string, polygon *PolygonClient, rest *RestClient, targetWallet string) *PolymarketMinedWatcher {
	wsURL = ResolvePolygonWSURL("", wsURL)
	targetWallet = NormalizeWalletAddress(targetWallet)
	if wsURL == "" || polygon == nil || rest == nil || !IsWalletAddress(targetWallet) {
		return nil
	}
	return &PolymarketMinedWatcher{
		wsURL:            wsURL,
		polygon:          polygon,
		rest:             rest,
		targetWallet:     targetWallet,
		seen:             make(map[string]time.Time),
		tokenCache:       make(map[string]pendingResolvedToken),
		pendingTransfers: make(map[string]MinedPolymarketSignal),
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

func safeGetSelector(input string) string {
	input = strings.TrimPrefix(input, "0x")
	if len(input) >= 8 {
		return "0x" + input[:8]
	}
	return "0x"
}

func shortHexForLog(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) > 10 {
		return raw[:10] + "..."
	}
	return raw
}

func (w *PolymarketMinedWatcher) SignalsSince(conditionID string, since time.Time) []MinedPolymarketSignal {
	if w == nil {
		return nil
	}
	conditionID = strings.TrimSpace(conditionID)
	now := time.Now()
	w.flushReadyTransferSignals(now)

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
	w.logf = logf
	w.mu.Unlock()

	go w.run(ctx)
}

func (w *PolymarketMinedWatcher) run(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}

		sessionStart := time.Now()
		err := w.runSession(ctx)
		sessionDuration := time.Since(sessionStart)

		if err != nil && ctx.Err() == nil {
			delay := polygonWSRetryDelay(err, backoff)
			var dialErr *polygonWSDialError
			if errors.As(err, &dialErr) {
				if !dialErr.retryable() {
					if w.logf != nil {
						w.logf("⚠️ Copytrade mined watcher stopped: %v", err)
					}
					return
				}
			}

			if w.logf != nil {
				w.logf("⚠️ Copytrade mined watcher reconnecting in %s: %v", delay.Round(time.Second), err)
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
	conn.SetReadLimit(1024 * 1024)

	targetTopic := polygonLogTopicAddress(w.targetWallet)
	operatorTopics := polygonExchangeOperatorTopics()
	transferTopics := []string{
		"0xc3d58168c5ae7397731d063d5bbf3d657854427343f4c083240f7aacaa2d0f62",
		"0x4a39dc06d4c0dbc64b70af90fd698a233a518aa5d07e595d983b8c0526c8f7ce",
	}

	subReqIn := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_subscribe",
		"params": []interface{}{
			"logs",
			map[string]interface{}{
				"address": CTFContract,
				"topics": []interface{}{
					transferTopics,
					operatorTopics,
					nil,
					targetTopic,
				},
			},
		},
	}
	subReqOut := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "eth_subscribe",
		"params": []interface{}{
			"logs",
			map[string]interface{}{
				"address": CTFContract,
				"topics": []interface{}{
					transferTopics,
					operatorTopics,
					targetTopic,
				},
			},
		},
	}
	if err := wsjson.Write(ctx, conn, subReqIn); err != nil {
		return fmt.Errorf("mined incoming log subscribe failed: %w", err)
	}
	if err := wsjson.Write(ctx, conn, subReqOut); err != nil {
		return fmt.Errorf("mined outgoing log subscribe failed: %w", err)
	}

	for i := 0; i < 2; i++ {
		var subResp map[string]interface{}
		subReadCtx, subReadCancel := context.WithTimeout(ctx, 10*time.Second)
		if err := wsjson.Read(subReadCtx, conn, &subResp); err != nil {
			subReadCancel()
			return fmt.Errorf("mined subscribe ack failed: %w", err)
		}
		subReadCancel()
	}

	if w.logf != nil {
		w.logf("📡 Copytrade mined watcher subscribed to CTF transfer logs for %s", w.targetWallet)
	}

	return readPolygonWSJSONWithHeartbeat(ctx, conn, "mined", func(raw map[string]json.RawMessage) error {
		paramsRaw, ok := raw["params"]
		if !ok {
			return nil
		}
		var params struct {
			Result polymarketTransferLog `json:"result"`
		}
		if err := json.Unmarshal(paramsRaw, &params); err != nil {
			return nil
		}
		w.handleTransferLog(ctx, params.Result)
		return nil
	})
}

type polymarketTransferLog struct {
	Address         string   `json:"address"`
	Topics          []string `json:"topics"`
	Data            string   `json:"data"`
	BlockNumber     string   `json:"blockNumber"`
	TransactionHash string   `json:"transactionHash"`
	Removed         bool     `json:"removed"`
}

func polygonLogTopicAddress(address string) string {
	address = NormalizeWalletAddress(address)
	if address == "" {
		return ""
	}
	return "0x000000000000000000000000" + strings.TrimPrefix(address, "0x")
}

func polygonExchangeOperatorTopics() []string {
	return []string{
		polygonLogTopicAddress(CTFExchange),
		polygonLogTopicAddress(NegRiskExchange),
		polygonLogTopicAddress(RouterExchange),
	}
}

func decodeTransferSingleLog(data string) ([]string, []float64, error) {
	words, err := splitPendingHexWords(data)
	if err != nil {
		return nil, nil, err
	}
	if len(words) < 2 {
		return nil, nil, fmt.Errorf("transfer single log missing words")
	}
	tokenID := pendingHexWordToDecimal(words[0])
	value := pendingMicroToFloat(pendingHexWordToBigInt(words[1]))
	if tokenID == "" || value <= 0 {
		return nil, nil, fmt.Errorf("transfer single log missing token or value")
	}
	return []string{tokenID}, []float64{value}, nil
}

func decodeTransferBatchLog(data string) ([]string, []float64, error) {
	words, err := splitPendingHexWords(data)
	if err != nil {
		return nil, nil, err
	}
	if len(words) < 2 {
		return nil, nil, fmt.Errorf("transfer batch log missing offsets")
	}
	idsBase, err := polygonLogOffsetWordToBase(words[0])
	if err != nil {
		return nil, nil, fmt.Errorf("transfer batch ids offset: %w", err)
	}
	valuesBase, err := polygonLogOffsetWordToBase(words[1])
	if err != nil {
		return nil, nil, fmt.Errorf("transfer batch values offset: %w", err)
	}
	idsRaw := decodePendingUintArray(words, idsBase)
	valuesRaw := decodePendingUintArray(words, valuesBase)
	if len(idsRaw) == 0 || len(valuesRaw) == 0 || len(idsRaw) != len(valuesRaw) {
		return nil, nil, fmt.Errorf("transfer batch arrays mismatch")
	}
	tokenIDs := make([]string, 0, len(idsRaw))
	sizes := make([]float64, 0, len(valuesRaw))
	for i := range idsRaw {
		if idsRaw[i] == nil || valuesRaw[i] == nil {
			continue
		}
		value := pendingMicroToFloat(valuesRaw[i])
		if value <= 0 {
			continue
		}
		tokenIDs = append(tokenIDs, idsRaw[i].String())
		sizes = append(sizes, value)
	}
	if len(tokenIDs) == 0 {
		return nil, nil, fmt.Errorf("transfer batch contained no positive values")
	}
	return tokenIDs, sizes, nil
}

func polygonLogOffsetWordToBase(word string) (int, error) {
	offset := pendingHexWordToBigInt(word)
	if offset == nil || !offset.IsInt64() {
		return 0, fmt.Errorf("invalid offset")
	}
	offsetInt := int(offset.Int64())
	if offsetInt < 0 || offsetInt%32 != 0 {
		return 0, fmt.Errorf("invalid offset %d", offsetInt)
	}
	return offsetInt / 32, nil
}

func (w *PolymarketMinedWatcher) handleTransferLog(parentCtx context.Context, entry polymarketTransferLog) {
	if w == nil || entry.Removed || len(entry.Topics) < 4 {
		return
	}

	from := pendingHexWordToAddress(strings.TrimPrefix(entry.Topics[2], "0x"))
	to := pendingHexWordToAddress(strings.TrimPrefix(entry.Topics[3], "0x"))
	side := ""
	switch {
	case strings.EqualFold(to, w.targetWallet) && !strings.EqualFold(from, w.targetWallet):
		side = "BUY"
	case strings.EqualFold(from, w.targetWallet) && !strings.EqualFold(to, w.targetWallet):
		side = "SELL"
	default:
		return
	}

	var (
		tokenIDs []string
		sizes    []float64
		err      error
	)
	switch strings.ToLower(strings.TrimSpace(entry.Topics[0])) {
	case "0xc3d58168c5ae7397731d063d5bbf3d657854427343f4c083240f7aacaa2d0f62":
		tokenIDs, sizes, err = decodeTransferSingleLog(entry.Data)
	case "0x4a39dc06d4c0dbc64b70af90fd698a233a518aa5d07e595d983b8c0526c8f7ce":
		tokenIDs, sizes, err = decodeTransferBatchLog(entry.Data)
	default:
		return
	}
	if err != nil {
		if w.logf != nil {
			w.logf("⚠️ Copytrade mined watcher skipped undecodable transfer log %s: %v", shortHexForLog(entry.TransactionHash), err)
		}
		return
	}

	blockNumber, _ := parseHexUint64(entry.BlockNumber)
	observedAt := time.Now()

	for i, tokenID := range tokenIDs {
		if i >= len(sizes) || sizes[i] <= 0.01 {
			continue
		}
		resolveCtx, cancel := context.WithTimeout(parentCtx, 4*time.Second)
		resolved, err := w.resolveToken(resolveCtx, tokenID)
		cancel()
		if err != nil {
			if w.logf != nil {
				w.logf("⚠️ Skip master trade: could not resolve transferred token %s", tokenID)
			}
			continue
		}

		sig := MinedPolymarketSignal{
			ObservedAt:     observedAt,
			SignalID:       fmt.Sprintf("%s:%s:%s", strings.TrimSpace(entry.TransactionHash), tokenID, side),
			TxHash:         strings.TrimSpace(entry.TransactionHash),
			Wallet:         w.targetWallet,
			TokenID:        tokenID,
			ConditionID:    resolved.market.ConditionID,
			Slug:           resolved.market.Slug,
			Outcome:        resolved.outcome,
			Side:           side,
			Size:           sizes[i],
			BlockNumber:    blockNumber,
			BlockTimestamp: observedAt.Unix(),
		}
		w.queueTransferSignal(sig)
	}
}

func (w *PolymarketMinedWatcher) processBlocks(ctx context.Context, headNum uint64) error {
	w.mu.Lock()
	lastBlock := w.lastBlock
	w.mu.Unlock()

	start, end, truncated, ok := minedWatcherSelectBlockRange(lastBlock, headNum)
	if !ok {
		return nil
	}
	if lastBlock > 0 && w.logf != nil {
		backlog := end - start + 1
		if backlog > polymarketMinedCatchupWarnBlocks {
			w.logf(
				"⚠️ Copytrade mined watcher catching up %d blocks after reconnect (last=%d head=%d)",
				backlog,
				lastBlock,
				headNum,
			)
		}
		if truncated > 0 {
			w.logf(
				"ℹ️ Copytrade mined watcher skipped %d older block(s) after reconnect to reduce RPC load (replaying %d newest block(s), head=%d)",
				truncated,
				backlog,
				headNum,
			)
		}
	}

	for blockNum := start; blockNum <= end; blockNum++ {
		if err := w.waitBlockFetchSlot(ctx); err != nil {
			return err
		}
		var block *FullBlock
		var err error

		// Retry block fetch up to 3 times if it returns null (common on Infura/Alchemy indexing lag)
		for attempt := 0; attempt < 3; attempt++ {
			blockCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			block, err = w.polygon.GetFullBlockByNumber(blockCtx, blockNum)
			cancel()

			if err != nil {
				return fmt.Errorf("get block %d failed: %w", blockNum, err)
			}
			if block != nil {
				break
			}
			// Block is null, wait a bit and retry
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
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

func minedWatcherSelectBlockRange(lastBlock, headNum uint64) (start, end, truncated uint64, ok bool) {
	if headNum == 0 {
		return 0, 0, 0, false
	}
	if lastBlock > 0 && headNum <= lastBlock {
		return 0, 0, 0, false
	}

	if lastBlock == 0 {
		return headNum, headNum, 0, true
	}

	start = lastBlock + 1
	end = headNum
	if replay := end - start + 1; replay > polymarketMinedCatchupReplayCap {
		truncated = replay - polymarketMinedCatchupReplayCap
		start = end - polymarketMinedCatchupReplayCap + 1
	}
	return start, end, truncated, true
}

func (w *PolymarketMinedWatcher) waitBlockFetchSlot(ctx context.Context) error {
	if w == nil {
		return nil
	}
	for {
		w.mu.Lock()
		now := time.Now()
		if w.lastBlockFetchAt.IsZero() || now.Sub(w.lastBlockFetchAt) >= polymarketMinedBlockFetchMinGap {
			w.lastBlockFetchAt = now
			w.mu.Unlock()
			return nil
		}
		waitFor := polymarketMinedBlockFetchMinGap - now.Sub(w.lastBlockFetchAt)
		w.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitFor):
		}
	}
}

func (w *PolymarketMinedWatcher) handleBlock(parentCtx context.Context, block *FullBlock) {
	if w == nil || block == nil {
		return
	}
	blockNumber, _ := parseHexUint64(block.Number)
	// For copytrading, we MUST use the current arrival time.
	// If we use blockTimestamp, the trader might skip it because it looks "old"
	// (mined blocks are always in the past).
	observedAt := time.Now()

	targetAddrHex := strings.TrimPrefix(strings.ToLower(w.targetWallet), "0x")

	for _, tx := range block.Transactions {
		inputLower := strings.ToLower(tx.Input)
		isFromTarget := strings.EqualFold(tx.From, w.targetWallet) || strings.EqualFold(tx.To, w.targetWallet)
		containsTarget := strings.Contains(inputLower, targetAddrHex)

		if (isFromTarget || containsTarget) && w.logf != nil {
			// Silenced diagnostic logs for cleaner console
			// w.logf("🔍 Spotted target wallet in tx %s (from: %s, to: %s, contains_addr: %v)", tx.Hash, tx.From, tx.To, containsTarget)
		}

		if !strings.Contains(tx.Input, polymarketMatchOrdersSelector[2:]) {
			// Silenced diagnostic logs
			// if containsTarget && w.logf != nil {
			// 	w.logf("💡 Target wallet found in tx %s but it doesn't use the matchOrders selector. Method: %s", tx.Hash, safeGetSelector(tx.Input))
			// }
			continue
		}

		orders, err := DecodePolymarketMatchOrdersInput(tx.Input)
		if err != nil || len(orders) == 0 {
			if isFromTarget && w.logf != nil {
				w.logf("⚠️ Failed to decode orders from target wallet tx: %v", err)
			}
			continue
		}

		type aggKey struct {
			tokenID string
			side    string
		}
		aggregatedSizes := make(map[aggKey]float64)

		for _, order := range orders {
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
			aggregatedSizes[aggKey{tokenID: order.TokenID, side: side}] += size
		}

		for key, totalSize := range aggregatedSizes {
			signalID := fmt.Sprintf("%s:%s:%s", strings.TrimSpace(tx.Hash), key.tokenID, key.side)

			w.mu.Lock()
			seenAt, exists := w.seen[signalID]
			w.mu.Unlock()

			if exists && time.Since(seenAt) < 5*time.Minute {
				continue
			}

			resolveCtx, cancel := context.WithTimeout(parentCtx, 4*time.Second)
			resolved, err := w.resolveToken(resolveCtx, key.tokenID)
			cancel()
			if err != nil {
				if w.logf != nil {
					w.logf("⚠️ Skip master trade: could not resolve token %s (5m/15m markets might not be indexed yet)", key.tokenID)
				}
				continue
			}
			sig := MinedPolymarketSignal{
				ObservedAt:     observedAt,
				SignalID:       signalID,
				TxHash:         strings.TrimSpace(tx.Hash),
				Wallet:         w.targetWallet,
				TokenID:        key.tokenID,
				ConditionID:    resolved.market.ConditionID,
				Slug:           resolved.market.Slug,
				Outcome:        resolved.outcome,
				Side:           key.side,
				Size:           totalSize,
				BlockNumber:    blockNumber,
				BlockTimestamp: observedAt.Unix(),
			}
			if !w.storeSignal(sig) {
				continue
			}
			if w.logf != nil {
				marketLabel := strings.TrimSpace(resolved.market.Slug)
				if marketLabel == "" {
					marketLabel = strings.TrimSpace(resolved.market.MarketSlug)
				}
				if marketLabel == "" {
					marketLabel = strings.TrimSpace(resolved.market.ConditionID)
				}
				w.logf("🎯 Master %s %.2f %s on %s (%s)", key.side, totalSize, resolved.outcome, marketLabel, shortHexForLog(tx.Hash))
			}
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

	// Try direct event lookup first
	event, err := w.rest.GetEventByTokenID(ctx, tokenID)
	if err == nil {
		market, outcome, ok := marketFromGammaEventTokenID(event, tokenID)
		if ok {
			resolved := pendingResolvedToken{market: market, outcome: outcome}
			w.mu.Lock()
			w.tokenCache[tokenID] = resolved
			w.mu.Unlock()
			return resolved, nil
		}
	}

	w.mu.Lock()
	canFallback := time.Since(w.lastDiscoveryFallback) > 5*time.Second
	w.mu.Unlock()

	if !canFallback {
		return pendingResolvedToken{}, fmt.Errorf("token %s not found and discovery fallback is cooling down", tokenID)
	}

	w.mu.Lock()
	w.lastDiscoveryFallback = time.Now()
	w.mu.Unlock()

	// FALLBACK: Proactively discover 5m/15m markets for BTC/ETH/SOL/XRP
	// Polymarket high-frequency markets (BTC 5m) often aren't indexed by token ID in time.
	for _, timeframe := range []string{"5m", "15m"} {
		markets, err := w.rest.GetMarketsByTimeframe(ctx, []string{"btc", "eth", "sol", "xrp"}, timeframe)
		if err == nil {
			for _, mkt := range markets {
				for _, tkn := range mkt.Tokens {
					res := pendingResolvedToken{market: mkt, outcome: tkn.Outcome}
					w.mu.Lock()
					w.tokenCache[tkn.TokenID] = res
					w.mu.Unlock()
					if tkn.TokenID == tokenID {
						return res, nil
					}
				}
			}
		}
	}

	return pendingResolvedToken{}, fmt.Errorf("token %s could not be resolved via Gamma or Timeframe discovery", tokenID)
}

func (w *PolymarketMinedWatcher) storeSignal(sig MinedPolymarketSignal) bool {
	key := strings.TrimSpace(sig.SignalID)
	if key == "" {
		key = fmt.Sprintf("%s|%s|%s|%s|%.6f", sig.TxHash, sig.TokenID, sig.Outcome, sig.Side, sig.Size)
	}
	now := time.Now()

	w.mu.Lock()
	defer w.mu.Unlock()

	if seenAt, exists := w.seen[key]; exists && now.Sub(seenAt) < 5*time.Minute {
		return false
	}
	w.seen[key] = now
	w.recent = append(w.recent, sig)
	if len(w.recent) > 512 {
		w.recent = append([]MinedPolymarketSignal(nil), w.recent[len(w.recent)-512:]...)
	}
	return true
}

func (w *PolymarketMinedWatcher) queueTransferSignal(sig MinedPolymarketSignal) {
	if w == nil {
		return
	}
	now := time.Now()

	w.mu.Lock()
	if w.pendingTransfers == nil {
		w.pendingTransfers = make(map[string]MinedPolymarketSignal)
	}
	if pending, exists := w.pendingTransfers[sig.SignalID]; exists {
		pending.Size += sig.Size
		if pending.ObservedAt.IsZero() || sig.ObservedAt.Before(pending.ObservedAt) {
			pending.ObservedAt = sig.ObservedAt
		}
		w.pendingTransfers[sig.SignalID] = pending
	} else {
		w.pendingTransfers[sig.SignalID] = sig
	}
	w.mu.Unlock()

	w.flushReadyTransferSignals(now)
}

func (w *PolymarketMinedWatcher) flushReadyTransferSignals(now time.Time) {
	if w == nil {
		return
	}
	ready := w.takeReadyTransferSignals(now)
	for _, sig := range ready {
		if !w.storeSignal(sig) {
			continue
		}
		if w.logf != nil {
			marketLabel := strings.TrimSpace(sig.Slug)
			if marketLabel == "" {
				marketLabel = strings.TrimSpace(sig.ConditionID)
			}
			w.logf("🎯 Master %s %.2f %s on %s (%s)", sig.Side, sig.Size, sig.Outcome, marketLabel, shortHexForLog(sig.TxHash))
		}
	}
}

func (w *PolymarketMinedWatcher) takeReadyTransferSignals(now time.Time) []MinedPolymarketSignal {
	if w == nil {
		return nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.pendingTransfers) == 0 {
		return nil
	}

	ready := make([]MinedPolymarketSignal, 0, len(w.pendingTransfers))
	for key, sig := range w.pendingTransfers {
		if !sig.ObservedAt.IsZero() && now.Sub(sig.ObservedAt) < polymarketMinedTransferAggregateWindow {
			continue
		}
		ready = append(ready, sig)
		delete(w.pendingTransfers, key)
	}
	return ready
}
