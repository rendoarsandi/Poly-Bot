package main

import (
	"runtime"
	"sync/atomic"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/paper"
)

var realbotSubmittedOrderCount atomic.Int64

func realbotRecordOrderSubmissions(count int) {
	if count <= 0 {
		return
	}
	realbotSubmittedOrderCount.Add(int64(count))
}

func realbotSubmittedOrdersTotal() int64 {
	return realbotSubmittedOrderCount.Load()
}

type realbotRuntimeMetrics struct {
	windowStart    time.Time
	lastLog        time.Time
	lastOrderTotal int64

	wsMessages   int
	quoteUpdates int
	decisions    int
	restFallback int
	maxQueue     int
}

func newRealbotRuntimeMetrics(now time.Time) *realbotRuntimeMetrics {
	if now.IsZero() {
		now = time.Now()
	}
	return &realbotRuntimeMetrics{
		windowStart:    now,
		lastLog:        now,
		lastOrderTotal: realbotSubmittedOrdersTotal(),
	}
}

func (m *realbotRuntimeMetrics) observeWSMessages(count, queueDepth int) {
	if m == nil {
		return
	}
	m.wsMessages += count
	if queueDepth > m.maxQueue {
		m.maxQueue = queueDepth
	}
}

func (m *realbotRuntimeMetrics) observeQuoteUpdate() {
	if m == nil {
		return
	}
	m.quoteUpdates++
}

func (m *realbotRuntimeMetrics) observeDecision() {
	if m == nil {
		return
	}
	m.decisions++
}

func (m *realbotRuntimeMetrics) observeRestFallback() {
	if m == nil {
		return
	}
	m.restFallback++
}

func (m *realbotRuntimeMetrics) maybeLog(tui *paper.TUI, marketID string, wsMgr *api.WSManager, queueDepth int, now time.Time) {
	if m == nil || tui == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	if now.Sub(m.lastLog) < realbotRuntimeMetricsLogInterval {
		return
	}
	elapsed := now.Sub(m.windowStart).Seconds()
	if elapsed <= 0 {
		elapsed = 1
	}
	if queueDepth > m.maxQueue {
		m.maxQueue = queueDepth
	}

	connected := false
	reconnects := int32(0)
	wsTotal := int64(0)
	if wsMgr != nil {
		connected, _, reconnects, wsTotal = wsMgr.GetStats()
	}
	orderTotal := realbotSubmittedOrdersTotal()
	orderDelta := orderTotal - m.lastOrderTotal
	if orderDelta < 0 {
		orderDelta = 0
	}

	status := "down"
	if connected {
		status = "up"
	}
	tui.LogEvent("[%s] 📟 Runtime %.0fs: ws %.1f/s, quotes %.1f/s, decisions %.1f/s, rest %.1f/s, orders %.1f/s, q %d, goroutines %d, reconnects %d, wsTotal %d, %s",
		marketID,
		elapsed,
		float64(m.wsMessages)/elapsed,
		float64(m.quoteUpdates)/elapsed,
		float64(m.decisions)/elapsed,
		float64(m.restFallback)/elapsed,
		float64(orderDelta)/elapsed,
		m.maxQueue,
		runtime.NumGoroutine(),
		reconnects,
		wsTotal,
		status,
	)

	m.windowStart = now
	m.lastLog = now
	m.lastOrderTotal = orderTotal
	m.wsMessages = 0
	m.quoteUpdates = 0
	m.decisions = 0
	m.restFallback = 0
	m.maxQueue = queueDepth
}
