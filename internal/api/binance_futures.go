package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	defaultBinanceSignalLookback = 1500 * time.Millisecond
	binanceFuturesWSBaseURL      = "wss://fstream.binance.com/stream?streams="
)

type binanceFuturesSample struct {
	Price float64
	At    time.Time
}

type binanceFlexibleInt64 int64

func (v *binanceFlexibleInt64) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*v = 0
		return nil
	}

	var asInt int64
	if err := json.Unmarshal(data, &asInt); err == nil {
		*v = binanceFlexibleInt64(asInt)
		return nil
	}

	var asString string
	if err := json.Unmarshal(data, &asString); err == nil {
		asString = strings.TrimSpace(asString)
		if asString == "" {
			*v = 0
			return nil
		}
		parsed, err := strconv.ParseInt(asString, 10, 64)
		if err != nil {
			return fmt.Errorf("parse quoted int64 %q: %w", asString, err)
		}
		*v = binanceFlexibleInt64(parsed)
		return nil
	}

	return fmt.Errorf("unsupported int64 JSON payload: %s", string(data))
}

type BinanceFuturesSignalSnapshot struct {
	Symbol        string
	Price         float64
	BaselinePrice float64
	DeltaPercent  float64
	UpdatedAt     time.Time
	BaselineAt    time.Time
	Source        string
	LastError     string
	Connected     bool
	Ready         bool
}

type BinanceFuturesPriceFeed struct {
	symbol       string
	lookback     time.Duration
	maxBufferAge time.Duration
	gapResetAge  time.Duration
	wsURL        string

	mu             sync.RWMutex
	samples        []binanceFuturesSample
	lastTradePrice float64
	lastTradeAt    time.Time
	lastMarkPrice  float64
	lastMarkAt     time.Time
	lastSource     string
	lastError      string
	connected      bool
}

func NewBinanceFuturesPriceFeed(symbol string, lookback time.Duration) *BinanceFuturesPriceFeed {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if lookback <= 0 {
		lookback = defaultBinanceSignalLookback
	}
	maxBufferAge := 30 * time.Second
	if scaled := lookback * 4; scaled > maxBufferAge {
		maxBufferAge = scaled
	}
	gapResetAge := 5 * time.Second
	if scaled := lookback * 4; scaled > gapResetAge {
		gapResetAge = scaled
	}
	streams := strings.ToLower(symbol) + "@aggTrade/" + strings.ToLower(symbol) + "@markPrice@1s"

	return &BinanceFuturesPriceFeed{
		symbol:       symbol,
		lookback:     lookback,
		maxBufferAge: maxBufferAge,
		gapResetAge:  gapResetAge,
		wsURL:        binanceFuturesWSBaseURL + streams,
	}
}

func (f *BinanceFuturesPriceFeed) Start(ctx context.Context) {
	if f == nil || f.symbol == "" {
		return
	}
	go f.run(ctx)
}

func (f *BinanceFuturesPriceFeed) Snapshot(now time.Time) BinanceFuturesSignalSnapshot {
	if f == nil {
		return BinanceFuturesSignalSnapshot{}
	}
	if now.IsZero() {
		now = time.Now()
	}

	f.mu.RLock()
	defer f.mu.RUnlock()

	snap := BinanceFuturesSignalSnapshot{
		Symbol:    f.symbol,
		UpdatedAt: f.lastTradeAt,
		Source:    f.lastSource,
		LastError: f.lastError,
		Connected: f.connected,
	}
	switch {
	case f.lastTradePrice > 0 && (!f.lastMarkAt.After(f.lastTradeAt) || now.Sub(f.lastTradeAt) <= 2*time.Second):
		snap.Price = f.lastTradePrice
		snap.Source = "ws-trade"
	case f.lastMarkPrice > 0:
		snap.Price = f.lastMarkPrice
		snap.Source = "ws-mark"
	default:
		snap.Price = f.lastTradePrice
	}
	if len(f.samples) == 0 || f.lastTradePrice <= 0 || f.lastTradeAt.IsZero() {
		return snap
	}

	// Anchor the target strictly to 'now' to ensure we measure a real-time window,
	// preventing stale signals if the market has been quiet.
	target := now.Add(-f.lookback)
	baseline := f.samples[0]
	for i := len(f.samples) - 1; i >= 0; i-- {
		if !f.samples[i].At.After(target) {
			baseline = f.samples[i]
			break
		}
	}

	snap.BaselinePrice = baseline.Price
	snap.BaselineAt = baseline.At
	if baseline.Price > 0 {
		snap.DeltaPercent = ((f.lastTradePrice / baseline.Price) - 1.0) * 100.0
	}
	// The feed is ready if the oldest sample is at least lookback time old relative to NOW.
	// This naturally avoids needing complex stale-check logic, as true stream gaps
	// are already handled by recordTradeSample wiping the buffer after gapResetAge.
	snap.Ready = !baseline.At.IsZero() && now.Sub(f.samples[0].At) >= f.lookback
	return snap
}

func (f *BinanceFuturesPriceFeed) run(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}

		err := f.runWebsocket(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			f.setError(err)
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < 15*time.Second {
			backoff *= 2
			if backoff > 15*time.Second {
				backoff = 15 * time.Second
			}
		}
	}
}

func (f *BinanceFuturesPriceFeed) runWebsocket(ctx context.Context) error {
	if f.symbol == "" {
		return fmt.Errorf("binance symbol is empty")
	}

	sessionCtx, endSession := context.WithTimeout(ctx, 23*time.Hour)
	defer endSession()

	dialCtx, cancel := context.WithTimeout(sessionCtx, 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(dialCtx, f.wsURL, &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		f.setConnected(false)
		return fmt.Errorf("binance websocket dial failed: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(1 << 20)
	f.setConnected(true)
	defer f.setConnected(false)

	type eventEnvelope struct {
		Stream string          `json:"stream"`
		Data   json.RawMessage `json:"data"`
	}
	type eventHeader struct {
		EventType string               `json:"e"`
		EventTime binanceFlexibleInt64 `json:"E"`
	}
	type aggTradeMessage struct {
		EventType string               `json:"e"`
		Price     string               `json:"p"`
		EventTime binanceFlexibleInt64 `json:"E"`
		TradeTime binanceFlexibleInt64 `json:"T"`
	}
	type markPriceMessage struct {
		EventType string               `json:"e"`
		Price     string               `json:"p"`
		EventTime binanceFlexibleInt64 `json:"E"`
	}

	for {
		var raw json.RawMessage
		if err := wsjson.Read(sessionCtx, conn, &raw); err != nil {
			return fmt.Errorf("binance websocket read failed: %w", err)
		}

		payload := raw
		var envelope eventEnvelope
		if err := json.Unmarshal(raw, &envelope); err == nil && len(envelope.Data) > 0 {
			payload = envelope.Data
		}

		var header eventHeader
		if err := json.Unmarshal(payload, &header); err != nil {
			continue
		}

		switch header.EventType {
		case "aggTrade":
			var msg aggTradeMessage
			if err := json.Unmarshal(payload, &msg); err != nil {
				continue
			}
			price, err := strconv.ParseFloat(msg.Price, 64)
			if err != nil || price <= 0 {
				continue
			}
			ts := int64(msg.TradeTime)
			if ts <= 0 {
				ts = int64(msg.EventTime)
			}
			at := time.Now()
			if ts > 0 {
				at = time.UnixMilli(ts)
			}
			f.recordTradeSample(price, at)
		case "markPriceUpdate":
			var msg markPriceMessage
			if err := json.Unmarshal(payload, &msg); err != nil {
				continue
			}
			price, err := strconv.ParseFloat(msg.Price, 64)
			if err != nil || price <= 0 {
				continue
			}
			at := time.Now()
			if ts := int64(msg.EventTime); ts > 0 {
				at = time.UnixMilli(ts)
			}
			f.recordMarkPrice(price, at)
		}
	}
}

func (f *BinanceFuturesPriceFeed) recordTradeSample(price float64, at time.Time) {
	if f == nil || price <= 0 {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.lastTradePrice = price
	f.lastTradeAt = at
	f.lastSource = "ws-trade"
	f.lastError = ""
	series := f.samples
	if n := len(series); n > 0 && !at.Before(series[n-1].At) && at.Sub(series[n-1].At) >= f.gapResetAge {
		// Only reset after a real quote drought; short exchange quiet periods should not constantly re-warm the feed.
		series = nil
	}
	series = append(series, binanceFuturesSample{Price: price, At: at})

	cutoff := at.Add(-f.maxBufferAge)
	trim := 0
	for trim < len(series)-1 && series[trim].At.Before(cutoff) {
		trim++
	}
	if trim > 0 {
		series = append([]binanceFuturesSample(nil), series[trim:]...)
	}
	f.samples = series
}

func (f *BinanceFuturesPriceFeed) recordMarkPrice(price float64, at time.Time) {
	if f == nil || price <= 0 {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.lastMarkPrice = price
	f.lastMarkAt = at
	if f.lastTradePrice <= 0 {
		f.lastSource = "ws-mark"
	}
	f.lastError = ""
}

func (f *BinanceFuturesPriceFeed) setConnected(connected bool) {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.connected = connected
	f.mu.Unlock()
}

func (f *BinanceFuturesPriceFeed) setError(err error) {
	if f == nil {
		return
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	f.mu.Lock()
	f.lastError = msg
	f.mu.Unlock()
}
