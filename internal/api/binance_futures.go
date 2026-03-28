package api

import (
	"context"
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
	binanceFuturesWSBaseURL      = "wss://fstream.binance.com/ws"
)

type binanceFuturesSample struct {
	Price float64
	At    time.Time
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
	wsURL        string

	mu         sync.RWMutex
	samples    []binanceFuturesSample
	lastPrice  float64
	lastUpdate time.Time
	lastSource string
	lastError  string
	connected  bool
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

	return &BinanceFuturesPriceFeed{
		symbol:       symbol,
		lookback:     lookback,
		maxBufferAge: maxBufferAge,
		wsURL:        binanceFuturesWSBaseURL + "/" + strings.ToLower(symbol) + "@aggTrade",
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
		Price:     f.lastPrice,
		UpdatedAt: f.lastUpdate,
		Source:    f.lastSource,
		LastError: f.lastError,
		Connected: f.connected,
	}
	if len(f.samples) == 0 || f.lastPrice <= 0 || f.lastUpdate.IsZero() {
		return snap
	}

	target := f.lastUpdate.Add(-f.lookback)
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
		snap.DeltaPercent = ((f.lastPrice / baseline.Price) - 1.0) * 100.0
	}
	snap.Ready = !baseline.At.IsZero() && f.lastUpdate.Sub(baseline.At) >= f.lookback
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

	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
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

	type aggTradeMessage struct {
		Price     string `json:"p"`
		EventTime int64  `json:"E"`
		TradeTime int64  `json:"T"`
	}

	for {
		var msg aggTradeMessage
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return fmt.Errorf("binance websocket read failed: %w", err)
		}
		price, err := strconv.ParseFloat(msg.Price, 64)
		if err != nil || price <= 0 {
			continue
		}
		ts := msg.TradeTime
		if ts <= 0 {
			ts = msg.EventTime
		}
		at := time.Now()
		if ts > 0 {
			at = time.UnixMilli(ts)
		}
		f.recordSample(price, at, "ws")
	}
}

func (f *BinanceFuturesPriceFeed) recordSample(price float64, at time.Time, source string) {
	if f == nil || price <= 0 {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.lastPrice = price
	f.lastUpdate = at
	f.lastSource = source
	f.lastError = ""
	f.samples = append(f.samples, binanceFuturesSample{Price: price, At: at})

	cutoff := at.Add(-f.maxBufferAge)
	trim := 0
	for trim < len(f.samples)-1 && f.samples[trim].At.Before(cutoff) {
		trim++
	}
	if trim > 0 {
		f.samples = append([]binanceFuturesSample(nil), f.samples[trim:]...)
	}
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
