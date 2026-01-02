package paper

import (
	"fmt"
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
)

// LimitOrder represents a limit order waiting to be filled
type LimitOrder struct {
	ID           int
	CreatedAt    time.Time
	Outcome      string      // "Up", "Down", "Yes", "No"
	Side         string      // "buy" or "sell"
	Price        float64     // Target price
	Quantity     float64     // Total quantity
	FilledQty    float64     // Amount filled
	Status       OrderStatus
	LadderLevel  int         // Which ladder level (0 = best price, 1 = next, etc.)
}

// RemainingQty returns unfilled quantity
func (o *LimitOrder) RemainingQty() float64 {
	return o.Quantity - o.FilledQty
}

// OrderBook manages open limit orders and simulates fills
type OrderBook struct {
	mu          sync.RWMutex
	orders      map[int]*LimitOrder
	nextOrderID int

	// Callbacks
	onFill func(order *LimitOrder, fillQty float64, fillPrice float64)
}

// NewOrderBook creates a new order book
func NewOrderBook() *OrderBook {
	return &OrderBook{
		orders:      make(map[int]*LimitOrder),
		nextOrderID: 1,
	}
}

// SetFillCallback sets the callback for when orders are filled
func (ob *OrderBook) SetFillCallback(cb func(order *LimitOrder, fillQty float64, fillPrice float64)) {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	ob.onFill = cb
}

// PlaceOrder places a new limit order
func (ob *OrderBook) PlaceOrder(outcome, side string, price, quantity float64, ladderLevel int) *LimitOrder {
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

// ProcessPriceUpdate checks if any orders should be filled based on new market prices
// For BUY orders: fill if market price <= order price (someone willing to sell at our bid)
// For SELL orders: fill if market price >= order price (someone willing to buy at our ask)
func (ob *OrderBook) ProcessPriceUpdate(outcome string, marketBid, marketAsk float64) []*LimitOrder {
	ob.mu.Lock()
	defer ob.mu.Unlock()

	var filledOrders []*LimitOrder

	for _, order := range ob.orders {
		if order.Outcome != outcome {
			continue
		}
		if order.Status != OrderStatusOpen && order.Status != OrderStatusPartial {
			continue
		}

		shouldFill := false
		fillPrice := 0.0

		if order.Side == "buy" {
			// Buy limit order fills when market ask <= our bid price
			// (someone is willing to sell at or below our price)
			if marketAsk > 0 && marketAsk <= order.Price {
				shouldFill = true
				fillPrice = marketAsk // We get filled at the better price
			}
		} else if order.Side == "sell" {
			// Sell limit order fills when market bid >= our ask price
			// (someone is willing to buy at or above our price)
			if marketBid > 0 && marketBid >= order.Price {
				shouldFill = true
				fillPrice = marketBid
			}
		}

		if shouldFill {
			fillQty := order.RemainingQty()
			order.FilledQty = order.Quantity
			order.Status = OrderStatusFilled
			filledOrders = append(filledOrders, order)

			if ob.onFill != nil {
				ob.onFill(order, fillQty, fillPrice)
			}
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
