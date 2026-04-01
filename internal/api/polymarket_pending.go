package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const polymarketMatchOrdersSelector = "0x2287e350"

type PendingTransaction struct {
	Hash  string `json:"hash"`
	From  string `json:"from"`
	To    string `json:"to"`
	Input string `json:"input"`
}

type PendingPolymarketSignal struct {
	ObservedAt  time.Time
	SignalID    string
	TxHash      string
	Wallet      string
	TokenID     string
	ConditionID string
	Slug        string
	Outcome     string
	Side        string
	Size        float64
}

type pendingResolvedToken struct {
	market  Market
	outcome string
}

type decodedPolymarketOrder struct {
	Maker        string
	Signer       string
	Taker        string
	TokenID      string
	MakerAmount  *big.Int
	TakerAmount  *big.Int
	Side         int
	FilledShares float64
}

type PolymarketPendingWatcher struct {
	wsURL        string
	rest         *RestClient
	targetWallet string
	polygon      *PolygonClient

	mu         sync.Mutex
	recent     []PendingPolymarketSignal
	seen       map[string]time.Time
	tokenCache map[string]pendingResolvedToken
	started    bool
}

func NewPolymarketPendingWatcher(wsURL string, rest *RestClient, polygon *PolygonClient, targetWallet string) *PolymarketPendingWatcher {
	wsURL = strings.TrimSpace(wsURL)
	targetWallet = NormalizeWalletAddress(targetWallet)
	if wsURL == "" || rest == nil || polygon == nil || !IsWalletAddress(targetWallet) {
		return nil
	}
	return &PolymarketPendingWatcher{
		wsURL:        wsURL,
		rest:         rest,
		polygon:      polygon,
		targetWallet: targetWallet,
		seen:         make(map[string]time.Time),
		tokenCache:   make(map[string]pendingResolvedToken),
	}
}

func ResolvePolymarketPendingWSURL(explicitWSURL, polygonRPCURL string) string {
	explicitWSURL = strings.TrimSpace(explicitWSURL)
	if explicitWSURL != "" {
		if normalized := normalizePendingWSURL(explicitWSURL); normalized != "" {
			return normalized
		}
		return explicitWSURL
	}
	return normalizePendingWSURL(polygonRPCURL)
}

func normalizePendingWSURL(rawURL string) string {
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
		return parsed.String()
	case "http":
		parsed.Scheme = "ws"
		return parsed.String()
	default:
		return ""
	}
}

func (w *PolymarketPendingWatcher) Enabled() bool {
	return w != nil && w.wsURL != "" && w.rest != nil && IsWalletAddress(w.targetWallet)
}

func (w *PolymarketPendingWatcher) PrimeTrackedMarkets(markets []*Market) {
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

func (w *PolymarketPendingWatcher) Start(ctx context.Context, logf func(string, ...interface{})) {
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

func (w *PolymarketPendingWatcher) SignalsSince(conditionID string, since time.Time) []PendingPolymarketSignal {
	if w == nil {
		return nil
	}
	conditionID = strings.TrimSpace(conditionID)
	now := time.Now()

	w.mu.Lock()
	defer w.mu.Unlock()

	filtered := make([]PendingPolymarketSignal, 0, len(w.recent))
	kept := w.recent[:0]
	for _, sig := range w.recent {
		if now.Sub(sig.ObservedAt) > 2*time.Minute {
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
		if now.Sub(seenAt) > 5*time.Minute {
			delete(w.seen, key)
		}
	}
	return filtered
}

func (w *PolymarketPendingWatcher) run(ctx context.Context, logf func(string, ...interface{})) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		delay := backoff
		if err := w.runSession(ctx); err != nil && ctx.Err() == nil {
			var dialErr *polygonWSDialError
			if errors.As(err, &dialErr) {
				if !dialErr.retryable() {
					if logf != nil {
						logf("⚠️ Copytrade mempool watcher stopped: %v", err)
					}
					return
				}
				delay = dialErr.retryDelay(backoff)
			}
			if logf != nil {
				logf("⚠️ Copytrade mempool watcher reconnecting: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay > backoff {
			backoff = delay
		} else if backoff < 8*time.Second {
			backoff *= 2
		}
	}
}

func (w *PolymarketPendingWatcher) runSession(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conn, resp, err := websocket.Dial(dialCtx, w.wsURL, &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return newPolygonWSDialError("pending", resp, err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(1024 * 1024) // 1MB limit for full transaction payloads

	method := "newPendingTransactions"
	if strings.Contains(strings.ToLower(w.wsURL), "alchemy") {
		method = "alchemy_pendingTransactions"
	}

	params := []interface{}{method}
	if method == "alchemy_pendingTransactions" {
		params = append(params, map[string]interface{}{
			"toAddress":  []string{CTFExchange, NegRiskExchange, RouterExchange, w.targetWallet},
			"hashesOnly": false,
		})
	}

	subReq := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_subscribe",
		"params":  params,
	}
	if err := wsjson.Write(ctx, conn, subReq); err != nil {
		return fmt.Errorf("pending websocket subscribe failed: %w", err)
	}

	var wg sync.WaitGroup
	defer wg.Wait()

	txHashes := make(chan string, 10000)
	if method == "newPendingTransactions" {
		// Rate limit standard RPC queries to ~10 req/s to avoid spamming public nodes
		rateLimiter := time.NewTicker(100 * time.Millisecond)
		defer rateLimiter.Stop()

		for i := 0; i < 5; i++ { // Reduced worker count to avoid connection spikes
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-ctx.Done():
						return
					case txHash, ok := <-txHashes:
						if !ok {
							return
						}
						select {
						case <-ctx.Done():
							return
						case <-rateLimiter.C:
						}
						txCtx, txCancel := context.WithTimeout(ctx, 3*time.Second)
						tx, err := w.polygon.GetTransactionByHash(txCtx, txHash)
						txCancel()
						if err == nil && tx != nil {
							w.handlePendingTransaction(ctx, PendingTransaction{
								Hash:  tx.Hash,
								From:  tx.From,
								To:    tx.To,
								Input: tx.Input,
							})
						}
					}
				}
			}()
		}
	}

	for {
		var raw map[string]json.RawMessage
		if err := wsjson.Read(ctx, conn, &raw); err != nil {
			close(txHashes)
			return fmt.Errorf("pending websocket read failed: %w", err)
		}
		paramsRaw, ok := raw["params"]
		if !ok {
			continue
		}
		if method == "newPendingTransactions" {
			var params struct {
				Result string `json:"result"`
			}
			if err := json.Unmarshal(paramsRaw, &params); err != nil {
				continue
			}
			if params.Result != "" {
				select {
				case txHashes <- params.Result:
				default:
				}
			}
		} else {
			var params struct {
				Result PendingTransaction `json:"result"`
			}
			if err := json.Unmarshal(paramsRaw, &params); err != nil {
				continue
			}
			w.handlePendingTransaction(ctx, params.Result)
		}
	}
}

func (w *PolymarketPendingWatcher) handlePendingTransaction(parentCtx context.Context, tx PendingTransaction) {
	if w == nil {
		return
	}
	if !strings.Contains(tx.Input, polymarketMatchOrdersSelector[2:]) {
		return
	}
	orders, err := DecodePolymarketMatchOrdersInput(tx.Input)
	if err != nil || len(orders) == 0 {
		return
	}
	observedAt := time.Now()
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
		sig := PendingPolymarketSignal{
			ObservedAt:  observedAt,
			SignalID:    fmt.Sprintf("%s:%d", strings.TrimSpace(tx.Hash), idx),
			TxHash:      strings.TrimSpace(tx.Hash),
			Wallet:      w.targetWallet,
			TokenID:     order.TokenID,
			ConditionID: resolved.market.ConditionID,
			Slug:        resolved.market.Slug,
			Outcome:     resolved.outcome,
			Side:        side,
			Size:        size,
		}
		w.storeSignal(sig)
	}
}

func (w *PolymarketPendingWatcher) resolveToken(ctx context.Context, tokenID string) (pendingResolvedToken, error) {
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

func (w *PolymarketPendingWatcher) storeSignal(sig PendingPolymarketSignal) {
	key := strings.TrimSpace(sig.SignalID)
	if key == "" {
		key = fmt.Sprintf("%s|%s|%s|%s|%.6f", sig.TxHash, sig.TokenID, sig.Outcome, sig.Side, sig.Size)
	}
	now := time.Now()

	w.mu.Lock()
	defer w.mu.Unlock()

	if seenAt, exists := w.seen[key]; exists && now.Sub(seenAt) < 2*time.Minute {
		return
	}
	w.seen[key] = now
	w.recent = append(w.recent, sig)
	if len(w.recent) > 256 {
		w.recent = append([]PendingPolymarketSignal(nil), w.recent[len(w.recent)-256:]...)
	}
}

func DecodePolymarketMatchOrdersInput(input string) ([]decodedPolymarketOrder, error) {
	input = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(input)), "0x")
	
	idx := strings.Index(input, polymarketMatchOrdersSelector[2:])
	if idx == -1 {
		return nil, fmt.Errorf("unsupported function selector")
	}
	
	words, err := splitPendingHexWords(input[idx+8:])
	if err != nil {
		return nil, err
	}
	if len(words) < 7 {
		return nil, fmt.Errorf("matchOrders head too short")
	}

	takerOffset, err := pendingHexWordToInt(words[0])
	if err != nil {
		return nil, err
	}
	makerOrdersOffset, err := pendingHexWordToInt(words[1])
	if err != nil {
		return nil, err
	}

	takerFillAmount := pendingHexWordToBigInt(words[2])
	takerReceiveAmount := pendingHexWordToBigInt(words[3])
	makerFillAmountsOffset, err := pendingHexWordToInt(words[4])
	if err != nil {
		makerFillAmountsOffset = 0
	}
	makerFillAmounts := decodePendingUintArray(words, makerFillAmountsOffset/32)

	orders := make([]decodedPolymarketOrder, 0, 4)
	if takerOrder, err := decodePendingMatchOrder(words, takerOffset/32); err == nil {
		takerOrder.FilledShares = pendingOrderFilledShares(takerOrder, takerFillAmount, takerReceiveAmount)
		orders = append(orders, takerOrder)
	}

	makerArrayBase := makerOrdersOffset / 32
	if makerArrayBase < 0 || makerArrayBase >= len(words) {
		return orders, nil
	}
	count, err := pendingHexWordToInt(words[makerArrayBase])
	if err != nil {
		return orders, nil
	}
	for i := 0; i < count; i++ {
		offsetIndex := makerArrayBase + 1 + i
		if offsetIndex >= len(words) {
			break
		}
		orderOffset, err := pendingHexWordToInt(words[offsetIndex])
		if err != nil {
			continue
		}
		order, err := decodePendingMatchOrder(words, makerArrayBase+1+orderOffset/32)
		if err != nil {
			continue
		}
		if i < len(makerFillAmounts) {
			order.FilledShares = pendingOrderFilledShares(order, makerFillAmounts[i], nil)
		}
		orders = append(orders, order)
	}
	return orders, nil
}

func decodePendingMatchOrder(words []string, base int) (decodedPolymarketOrder, error) {
	if base < 0 || base+12 >= len(words) {
		return decodedPolymarketOrder{}, fmt.Errorf("order tuple out of bounds")
	}
	side, err := pendingHexWordToInt(words[base+10])
	if err != nil {
		return decodedPolymarketOrder{}, err
	}
	return decodedPolymarketOrder{
		Maker:       pendingHexWordToAddress(words[base+1]),
		Signer:      pendingHexWordToAddress(words[base+2]),
		Taker:       pendingHexWordToAddress(words[base+3]),
		TokenID:     pendingHexWordToDecimal(words[base+4]),
		MakerAmount: pendingHexWordToBigInt(words[base+5]),
		TakerAmount: pendingHexWordToBigInt(words[base+6]),
		Side:        side,
	}, nil
}

func (o decodedPolymarketOrder) sideString() string {
	switch o.Side {
	case 0:
		return string(SideBuy)
	case 1:
		return string(SideSell)
	default:
		return ""
	}
}

func (o decodedPolymarketOrder) fillSize() float64 {
	if o.FilledShares > 0 {
		return o.FilledShares
	}
	return o.shareSize()
}

func (o decodedPolymarketOrder) shareSize() float64 {
	if o.MakerAmount == nil || o.TakerAmount == nil {
		return 0
	}
	switch o.Side {
	case 0:
		return pendingMicroToFloat(o.TakerAmount)
	case 1:
		return pendingMicroToFloat(o.MakerAmount)
	default:
		return 0
	}
}

func pendingOrderFilledShares(order decodedPolymarketOrder, fillAmount *big.Int, receiveAmount *big.Int) float64 {
	if order.MakerAmount == nil || order.TakerAmount == nil {
		return 0
	}
	switch order.Side {
	case 0:
		if receiveAmount != nil && receiveAmount.Sign() > 0 {
			return pendingMicroToFloat(receiveAmount)
		}
		if fillAmount == nil || fillAmount.Sign() <= 0 || order.MakerAmount.Sign() <= 0 {
			return 0
		}
		shares := new(big.Rat).SetInt(order.TakerAmount)
		shares.Mul(shares, new(big.Rat).SetInt(fillAmount))
		shares.Quo(shares, new(big.Rat).SetInt(order.MakerAmount))
		shares.Quo(shares, big.NewRat(1_000_000, 1))
		f, _ := shares.Float64()
		return f
	case 1:
		if fillAmount == nil || fillAmount.Sign() <= 0 {
			return 0
		}
		return pendingMicroToFloat(fillAmount)
	default:
		return 0
	}
}

func splitPendingHexWords(data string) ([]string, error) {
	data = strings.TrimPrefix(strings.TrimSpace(data), "0x")
	if data == "" {
		return nil, fmt.Errorf("empty hex data")
	}
	remainder := len(data) % 64
	if remainder != 0 {
		data += strings.Repeat("0", 64-remainder)
	}
	words := make([]string, 0, len(data)/64)
	for i := 0; i < len(data); i += 64 {
		words = append(words, data[i:i+64])
	}
	return words, nil
}

func pendingHexWordToAddress(word string) string {
	word = strings.TrimSpace(word)
	if len(word) < 40 {
		return ""
	}
	return NormalizeWalletAddress("0x" + word[len(word)-40:])
}

func pendingHexWordToDecimal(word string) string {
	n := pendingHexWordToBigInt(word)
	if n == nil {
		return ""
	}
	return n.String()
}

func pendingHexWordToBigInt(word string) *big.Int {
	n := new(big.Int)
	if _, ok := n.SetString(strings.TrimSpace(word), 16); !ok {
		return nil
	}
	return n
}

func pendingHexWordToInt(word string) (int, error) {
	n := pendingHexWordToBigInt(word)
	if n == nil || !n.IsInt64() {
		return 0, fmt.Errorf("invalid integer word")
	}
	return int(n.Int64()), nil
}

func decodePendingUintArray(words []string, base int) []*big.Int {
	if base < 0 || base >= len(words) {
		return nil
	}
	count, err := pendingHexWordToInt(words[base])
	if err != nil || count <= 0 {
		return nil
	}
	result := make([]*big.Int, 0, count)
	for i := 0; i < count; i++ {
		idx := base + 1 + i
		if idx >= len(words) {
			break
		}
		result = append(result, pendingHexWordToBigInt(words[idx]))
	}
	return result
}

func pendingMicroToFloat(n *big.Int) float64 {
	if n == nil {
		return 0
	}
	r := new(big.Rat).SetInt(n)
	r.Quo(r, big.NewRat(1_000_000, 1))
	f, _ := r.Float64()
	return f
}

func marketFromGammaEventTokenID(event *GammaEvent, tokenID string) (Market, string, bool) {
	if event == nil {
		return Market{}, "", false
	}
	for _, gm := range event.Markets {
		var tokenIDs []string
		if err := json.Unmarshal([]byte(gm.ClobTokenIds), &tokenIDs); err != nil || len(tokenIDs) < 2 {
			continue
		}
		if tokenIDs[0] != tokenID && tokenIDs[1] != tokenID {
			continue
		}
		var outcomes []string
		if err := json.Unmarshal([]byte(gm.Outcomes), &outcomes); err != nil || len(outcomes) < 2 {
			outcomes = []string{"Up", "Down"}
		}
		market := Market{
			ConditionID: gm.ConditionID,
			Slug:        coreSanitizeFallback(event.Slug, gm.ConditionID),
			Active:      gm.Active,
			Closed:      gm.Closed,
			Tokens: []Token{
				{TokenID: tokenIDs[0], Outcome: strings.TrimSpace(outcomes[0])},
				{TokenID: tokenIDs[1], Outcome: strings.TrimSpace(outcomes[1])},
			},
		}
		outcome := market.Tokens[0].Outcome
		if tokenIDs[1] == tokenID {
			outcome = market.Tokens[1].Outcome
		}
		return market, outcome, true
	}
	return Market{}, "", false
}

func coreSanitizeFallback(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}
