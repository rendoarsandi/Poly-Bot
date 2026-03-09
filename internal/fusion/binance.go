package fusion

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	binanceTickerURL = "https://api.binance.com/api/v3/ticker/price"
	binanceWSURL     = "wss://stream.binance.com:9443/stream?streams=btcusdt@trade/ethusdt@trade/solusdt@trade/xrpusdt@trade"
)

var binanceSymbols = map[string]string{
	"BTC": "BTCUSDT",
	"ETH": "ETHUSDT",
	"SOL": "SOLUSDT",
	"XRP": "XRPUSDT",
}

type BinanceQuote struct {
	Asset   string
	Price   float64
	Updated time.Time
}

type BinanceSignals struct {
	Price              float64
	Updated            time.Time
	Returns1m          float64
	Returns5m          float64
	Returns10m         float64
	TradeFlowImbalance float64
	CVDAcceleration    float64
	TradeIntensity     float64
	LargeTradeFlag     float64
}

type binanceTrade struct {
	At       time.Time
	Price    float64
	Qty      float64
	Notional float64
	Signed   float64
}

type binanceAssetState struct {
	Quote          BinanceQuote
	PriceHistory   []timedValue
	Trades         []binanceTrade
	CVDHistory     []timedValue
	LastLargeTrade time.Time
}

type BinanceStreamer struct {
	client *http.Client

	mu     sync.RWMutex
	states map[string]*binanceAssetState
}

func NewBinanceStreamer() *BinanceStreamer {
	return &BinanceStreamer{
		client: &http.Client{Timeout: 5 * time.Second},
		states: make(map[string]*binanceAssetState),
	}
}

func (s *BinanceStreamer) Run(ctx context.Context, logFn func(string, ...interface{})) {
	if err := s.bootstrap(ctx); err != nil && logFn != nil {
		logFn("binance bootstrap failed: %v", err)
	}

	for {
		if err := s.streamOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			if logFn != nil {
				logFn("binance ws reconnecting: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(1500 * time.Millisecond):
			}
			continue
		}
		return
	}
}

func (s *BinanceStreamer) bootstrap(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, binanceTickerURL, nil)
	if err != nil {
		return err
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ticker bootstrap status %d", resp.StatusCode)
	}

	var tickers []struct {
		Symbol string `json:"symbol"`
		Price  string `json:"price"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tickers); err != nil {
		return err
	}

	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for asset, symbol := range binanceSymbols {
		for _, ticker := range tickers {
			if ticker.Symbol != symbol {
				continue
			}
			price, err := strconv.ParseFloat(ticker.Price, 64)
			if err != nil || price <= 0 {
				continue
			}
			s.upsertLocked(asset, price, now, 0, false)
			break
		}
	}
	return nil
}

func (s *BinanceStreamer) streamOnce(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(dialCtx, binanceWSURL, &websocket.DialOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return err
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(1024 * 1024)

	for {
		_, msg, err := conn.Read(ctx)
		if err != nil {
			return err
		}

		var payload struct {
			Data struct {
				Symbol       string `json:"s"`
				Price        string `json:"p"`
				Quantity     string `json:"q"`
				BuyerIsMaker bool   `json:"m"`
			} `json:"data"`
		}
		if err := json.Unmarshal(msg, &payload); err != nil {
			continue
		}

		asset := ""
		for candidate, symbol := range binanceSymbols {
			if payload.Data.Symbol == symbol {
				asset = candidate
				break
			}
		}
		if asset == "" {
			continue
		}

		price, err := strconv.ParseFloat(payload.Data.Price, 64)
		if err != nil || price <= 0 {
			continue
		}
		qty, _ := strconv.ParseFloat(payload.Data.Quantity, 64)

		s.mu.Lock()
		s.upsertLocked(asset, price, time.Now(), qty, !payload.Data.BuyerIsMaker)
		s.mu.Unlock()
	}
}

func (s *BinanceStreamer) Quote(asset string) BinanceQuote {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if state := s.states[asset]; state != nil {
		return state.Quote
	}
	return BinanceQuote{}
}

func (s *BinanceStreamer) Signals(asset string) BinanceSignals {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state := s.states[asset]
	if state == nil {
		return BinanceSignals{}
	}
	now := state.Quote.Updated
	if now.IsZero() {
		now = time.Now()
	}
	price := state.Quote.Price
	return BinanceSignals{
		Price:              price,
		Updated:            state.Quote.Updated,
		Returns1m:          priceReturn(state.PriceHistory, now, time.Minute),
		Returns5m:          priceReturn(state.PriceHistory, now, 5*time.Minute),
		Returns10m:         priceReturn(state.PriceHistory, now, 10*time.Minute),
		TradeFlowImbalance: tradeFlowImbalance(state.Trades, now, time.Minute),
		CVDAcceleration:    cvdAcceleration(state.CVDHistory, now),
		TradeIntensity:     float64(countTrades(state.Trades, now, 10*time.Second)) / 10.0,
		LargeTradeFlag:     largeTradeFlag(state, now),
	}
}

func (s *BinanceStreamer) upsertLocked(asset string, price float64, now time.Time, qty float64, buyAggressor bool) {
	state := s.states[asset]
	if state == nil {
		state = &binanceAssetState{}
		s.states[asset] = state
	}
	state.Quote = BinanceQuote{Asset: asset, Price: price, Updated: now}
	if len(state.PriceHistory) == 0 || now.Sub(state.PriceHistory[len(state.PriceHistory)-1].At) >= time.Second || math.Abs(state.PriceHistory[len(state.PriceHistory)-1].Value-price) >= price*0.00005 {
		state.PriceHistory = append(state.PriceHistory, timedValue{At: now, Value: price})
	}
	state.PriceHistory = pruneTimedValues(state.PriceHistory, now.Add(-15*time.Minute))
	if qty <= 0 {
		return
	}
	notional := qty * price
	signed := -notional
	if buyAggressor {
		signed = notional
	}
	state.Trades = append(state.Trades, binanceTrade{At: now, Price: price, Qty: qty, Notional: notional, Signed: signed})
	state.Trades = pruneTrades(state.Trades, now.Add(-15*time.Minute))
	cvd := signed
	if len(state.CVDHistory) > 0 {
		cvd += state.CVDHistory[len(state.CVDHistory)-1].Value
	}
	state.CVDHistory = append(state.CVDHistory, timedValue{At: now, Value: cvd})
	state.CVDHistory = pruneTimedValues(state.CVDHistory, now.Add(-15*time.Minute))
	if notional >= 15000 || notional >= rollingLargeThreshold(state.Trades, now) {
		state.LastLargeTrade = now
	}
}

func priceReturn(history []timedValue, now time.Time, window time.Duration) float64 {
	if len(history) < 2 {
		return 0
	}
	ref := history[0].Value
	cutoff := now.Add(-window)
	for _, item := range history {
		if !item.At.Before(cutoff) {
			ref = item.Value
			break
		}
		ref = item.Value
	}
	last := history[len(history)-1].Value
	if ref <= 0 {
		return 0
	}
	return (last - ref) / ref
}

func tradeFlowImbalance(trades []binanceTrade, now time.Time, window time.Duration) float64 {
	cutoff := now.Add(-window)
	buyVol := 0.0
	sellVol := 0.0
	for _, trade := range trades {
		if trade.At.Before(cutoff) {
			continue
		}
		if trade.Signed >= 0 {
			buyVol += trade.Notional
		} else {
			sellVol += trade.Notional
		}
	}
	if buyVol+sellVol == 0 {
		return 0
	}
	return clamp((buyVol-sellVol)/(buyVol+sellVol), -1, 1)
}

func cvdAcceleration(history []timedValue, now time.Time) float64 {
	if len(history) < 3 {
		return 0
	}
	recent := valueAt(history, now.Add(-30*time.Second))
	previous := valueAt(history, now.Add(-60*time.Second))
	current := history[len(history)-1].Value
	firstLeg := current - recent
	secondLeg := recent - previous
	denom := math.Max(1000, math.Abs(firstLeg)+math.Abs(secondLeg))
	return clamp((firstLeg-secondLeg)/denom*4, -1, 1)
}

func valueAt(history []timedValue, target time.Time) float64 {
	if len(history) == 0 {
		return 0
	}
	value := history[0].Value
	for _, item := range history {
		if item.At.After(target) {
			break
		}
		value = item.Value
	}
	return value
}

func countTrades(trades []binanceTrade, now time.Time, window time.Duration) int {
	cutoff := now.Add(-window)
	count := 0
	for _, trade := range trades {
		if !trade.At.Before(cutoff) {
			count++
		}
	}
	return count
}

func largeTradeFlag(state *binanceAssetState, now time.Time) float64 {
	if state == nil || state.LastLargeTrade.IsZero() {
		return 0
	}
	dt := now.Sub(state.LastLargeTrade)
	if dt < 0 || dt > 8*time.Second {
		return 0
	}
	return clamp(1-(dt.Seconds()/8.0), 0, 1)
}

func rollingLargeThreshold(trades []binanceTrade, now time.Time) float64 {
	cutoff := now.Add(-2 * time.Minute)
	total := 0.0
	count := 0.0
	for _, trade := range trades {
		if trade.At.Before(cutoff) {
			continue
		}
		total += trade.Notional
		count++
	}
	if count == 0 {
		return 15000
	}
	return math.Max(15000, (total/count)*3.5)
}

func pruneTrades(trades []binanceTrade, cutoff time.Time) []binanceTrade {
	idx := 0
	for idx < len(trades) && trades[idx].At.Before(cutoff) {
		idx++
	}
	if idx == 0 {
		return trades
	}
	return append([]binanceTrade(nil), trades[idx:]...)
}
