package paper

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

// OrderStatus represents the status of an order
type OrderStatus string

const (
	OrderStatusOpen      OrderStatus = "open"
	OrderStatusFilled    OrderStatus = "filled"
	OrderStatusCancelled OrderStatus = "cancelled"
	OrderStatusPartial   OrderStatus = "partial"

	// cleanupRateLimit is the minimum interval between order cleanups
	cleanupRateLimit = time.Minute
)

// LimitOrder represents a limit order waiting to be filled
type LimitOrder struct {
	ID          int
	CreatedAt   time.Time
	Outcome     string  // "Up", "Down", "Yes", "No"
	Side        string  // "buy" or "sell"
	Price       float64 // Target price (limit)
	Quantity    float64 // Total quantity
	FilledQty   float64 // Amount filled
	FillPrice   float64 // Actual fill price (may be better than limit)
	Status      OrderStatus
	LadderLevel int // Which ladder level (0 = best price, 1 = next, etc.)
}

// RemainingQty returns unfilled quantity
func (o *LimitOrder) RemainingQty() float64 {
	return o.Quantity - o.FilledQty
}

// MarketLevel represents a price level in the market's order book
type MarketLevel struct {
	Price float64
	Size  float64
}

// OrderBook manages open limit orders and simulates fills
type OrderBook struct {
	mu          sync.RWMutex
	orders      map[int]*LimitOrder
	nextOrderID int

	// Realism settings
	queueBuffer float64       // Price buffer to simulate queue priority (e.g., 0.001 = must be 0.1 cent better)
	orderDelay  time.Duration // Delay when placing orders (simulates API latency)

	// Memory management
	lastCleanup time.Time

	// Callbacks
	onFill func(order *LimitOrder, fillQty float64, fillPrice float64)
}

// NewOrderBook creates a new order book
func NewOrderBook() *OrderBook {
	return &OrderBook{
		orders:      make(map[int]*LimitOrder),
		nextOrderID: 1,
		queueBuffer: 0.001,                  // Default: need price 0.1 cent better to fill
		orderDelay:  200 * time.Millisecond, // Default: 200ms API latency
		lastCleanup: time.Now(),
	}
}

// NewOrderBookWithRealism creates an order book with custom realism settings
func NewOrderBookWithRealism(queueBuffer float64, orderDelay time.Duration) *OrderBook {
	return &OrderBook{
		orders:      make(map[int]*LimitOrder),
		nextOrderID: 1,
		queueBuffer: queueBuffer,
		orderDelay:  orderDelay,
	}
}

// SetRealism configures realism settings
func (ob *OrderBook) SetRealism(queueBuffer float64, orderDelay time.Duration) {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	ob.queueBuffer = queueBuffer
	ob.orderDelay = orderDelay
}

// SetFillCallback sets the callback for when orders are filled
func (ob *OrderBook) SetFillCallback(cb func(order *LimitOrder, fillQty float64, fillPrice float64)) {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	ob.onFill = cb
}

// PlaceOrder places a new limit order (includes simulated API delay)
func (ob *OrderBook) PlaceOrder(outcome, side string, price, quantity float64, ladderLevel int) *LimitOrder {
	// Simulate API latency before placing order
	if ob.orderDelay > 0 {
		time.Sleep(ob.orderDelay)
	}

	ob.mu.Lock()
	defer ob.mu.Unlock()

	order := &LimitOrder{
		ID:          ob.nextOrderID,
		CreatedAt:   time.Now(),
		Outcome:     outcome,
		Side:        side,
		Price:       price,
		Quantity:    quantity,
		FilledQty:   0,
		Status:      OrderStatusOpen,
		LadderLevel: ladderLevel,
	}
	ob.orders[order.ID] = order
	ob.nextOrderID++

	return order
}

// CancelOrder cancels an open order
func (ob *OrderBook) CancelOrder(orderID int) error {
	ob.mu.Lock()
	defer ob.mu.Unlock()

	order, exists := ob.orders[orderID]
	if !exists {
		return fmt.Errorf("order %d not found", orderID)
	}
	if order.Status == OrderStatusFilled {
		return fmt.Errorf("order %d already filled", orderID)
	}
	order.Status = OrderStatusCancelled
	return nil
}

// CancelAllOrders cancels all open orders
func (ob *OrderBook) CancelAllOrders() int {
	ob.mu.Lock()
	defer ob.mu.Unlock()

	count := 0
	for _, order := range ob.orders {
		if order.Status == OrderStatusOpen || order.Status == OrderStatusPartial {
			order.Status = OrderStatusCancelled
			count++
		}
	}
	return count
}

// CancelOrdersForOutcome cancels all orders for a specific outcome
func (ob *OrderBook) CancelOrdersForOutcome(outcome string) int {
	ob.mu.Lock()
	defer ob.mu.Unlock()

	count := 0
	for _, order := range ob.orders {
		if order.Outcome == outcome && (order.Status == OrderStatusOpen || order.Status == OrderStatusPartial) {
			order.Status = OrderStatusCancelled
			count++
		}
	}
	return count
}

// ProcessPriceUpdate checks if any orders should be filled based on full market depth
// For BUY orders: match against market asks <= order price - queueBuffer
// For SELL orders: match against market bids >= order price + queueBuffer
func (ob *OrderBook) ProcessPriceUpdate(outcome string, bids, asks []MarketLevel) []*LimitOrder {
	ob.mu.Lock()

	var filledOrders []*LimitOrder

	type fillRecord struct {
		order     *LimitOrder
		fillQty   float64
		fillPrice float64
	}
	var fills []fillRecord

	// Collect active orders for this outcome
	var activeOrders []*LimitOrder
	for _, order := range ob.orders {
		if order.Outcome == outcome && (order.Status == OrderStatusOpen || order.Status == OrderStatusPartial) {
			activeOrders = append(activeOrders, order)
		}
	}

	// Sort orders by CreatedAt to simulate FIFO queue priority
	sort.Slice(activeOrders, func(i, j int) bool {
		return activeOrders[i].CreatedAt.Before(activeOrders[j].CreatedAt)
	})

	// We iterate through our orders and match them against the available market liquidity
	for _, order := range activeOrders {
		remaining := order.RemainingQty()
		if remaining <= 0 {
			continue
		}

		// Price sanity bounds for binary markets
		const minSanePrice = 0.20
		const maxSanePrice = 0.80
		const maxPriceImprovement = 0.30

		if order.Side == "buy" {
			// BUY order matches against market ASKS
			fillThreshold := order.Price - ob.queueBuffer

			for i := range asks {
				ask := &asks[i]
				if ask.Size <= 0 || ask.Price > fillThreshold {
					continue
				}

				// Sanity checks
				if ask.Price < minSanePrice || ask.Price > maxSanePrice {
					continue
				}
				priceImprovement := (order.Price - ask.Price) / order.Price
				if priceImprovement > maxPriceImprovement {
					continue
				}

				// Calculate fill amount
				fillQty := math.Min(remaining, ask.Size)
				if fillQty > 0 {
					order.FilledQty += fillQty
					order.FillPrice = ask.Price
					ask.Size -= fillQty
					remaining -= fillQty

					if order.FilledQty >= order.Quantity-0.0001 {
						order.Status = OrderStatusFilled
					} else {
						order.Status = OrderStatusPartial
					}

					filledOrders = append(filledOrders, order)
					fills = append(fills, fillRecord{order, fillQty, ask.Price})

					if order.Status == OrderStatusFilled {
						break
					}
				}
			}
		} else if order.Side == "sell" {
			// SELL order matches against market BIDS
			fillThreshold := order.Price + ob.queueBuffer

			for i := range bids {
				bid := &bids[i]
				if bid.Size <= 0 || bid.Price < fillThreshold {
					continue
				}

				// Sanity checks
				if bid.Price < minSanePrice || bid.Price > maxSanePrice {
					continue
				}
				priceImprovement := (bid.Price - order.Price) / order.Price
				if priceImprovement > maxPriceImprovement {
					continue
				}

				// Calculate fill amount
				fillQty := math.Min(remaining, bid.Size)
				if fillQty > 0 {
					order.FilledQty += fillQty
					order.FillPrice = bid.Price
					bid.Size -= fillQty
					remaining -= fillQty

					if order.FilledQty >= order.Quantity-0.0001 {
						order.Status = OrderStatusFilled
					} else {
						order.Status = OrderStatusPartial
					}

					filledOrders = append(filledOrders, order)
					fills = append(fills, fillRecord{order, fillQty, bid.Price})

					if order.Status == OrderStatusFilled {
						break
					}
				}
			}
		}
	}

	// Release lock BEFORE calling external callbacks
	onFill := ob.onFill
	ob.mu.Unlock()

	// Execute callbacks outside of lock to prevent deadlocks
	if onFill != nil {
		for _, f := range fills {
			onFill(f.order, f.fillQty, f.fillPrice)
		}
	}

	return filledOrders
}

// GetOpenOrders returns all open orders
func (ob *OrderBook) GetOpenOrders() []*LimitOrder {
	ob.mu.RLock()
	defer ob.mu.RUnlock()

	var open []*LimitOrder
	for _, order := range ob.orders {
		if order.Status == OrderStatusOpen || order.Status == OrderStatusPartial {
			open = append(open, order)
		}
	}
	return open
}

// GetOpenOrdersForOutcome returns open orders for a specific outcome
func (ob *OrderBook) GetOpenOrdersForOutcome(outcome string) []*LimitOrder {
	ob.mu.RLock()
	defer ob.mu.RUnlock()

	var open []*LimitOrder
	for _, order := range ob.orders {
		if order.Outcome == outcome && (order.Status == OrderStatusOpen || order.Status == OrderStatusPartial) {
			open = append(open, order)
		}
	}
	return open
}

// GetOpenOrderValue returns total value of open orders
func (ob *OrderBook) GetOpenOrderValue() float64 {
	ob.mu.RLock()
	defer ob.mu.RUnlock()

	total := 0.0
	for _, order := range ob.orders {
		if order.Status == OrderStatusOpen || order.Status == OrderStatusPartial {
			total += order.RemainingQty() * order.Price
		}
	}
	return total
}

// GetOpenOrdersByOutcome returns map of outcome -> open order quantity
func (ob *OrderBook) GetOpenOrdersByOutcome() map[string]float64 {
	ob.mu.RLock()
	defer ob.mu.RUnlock()

	result := make(map[string]float64)
	for _, order := range ob.orders {
		if order.Status == OrderStatusOpen || order.Status == OrderStatusPartial {
			result[order.Outcome] += order.RemainingQty()
		}
	}
	return result
}

// CleanupOldOrders removes filled/cancelled orders older than maxAge to prevent memory growth
// This should be called periodically (e.g., every minute)
func (ob *OrderBook) CleanupOldOrders(maxAge time.Duration) int {
	ob.mu.Lock()
	defer ob.mu.Unlock()

	// Don't cleanup too frequently
	if time.Since(ob.lastCleanup) < cleanupRateLimit {
		return 0
	}
	ob.lastCleanup = time.Now()

	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for id, order := range ob.orders {
		if (order.Status == OrderStatusFilled || order.Status == OrderStatusCancelled) &&
			order.CreatedAt.Before(cutoff) {
			delete(ob.orders, id)
			removed++
		}
	}
	return removed
}
