package paper

// LadderConfig configures ladder quoting parameters
type LadderConfig struct {
	Levels         int     // Number of price levels (e.g., 3)
	SharesPerLevel float64 // Shares at each level (e.g., 500)
	PriceStep      float64 // Price decrement per level (e.g., 0.01 = 1 cent)
	BasePrice      float64 // Starting price for ladder (e.g., 0.48)
}

// DefaultLadderConfig returns default ladder configuration
func DefaultLadderConfig() LadderConfig {
	return LadderConfig{
		Levels:         3,
		SharesPerLevel: 100,
		PriceStep:      0.01, // 1 cent steps
		BasePrice:      0.0,  // Must be set from real market data before placing orders
	}
}

// Ladder manages ladder quoting for an outcome
type Ladder struct {
	Outcome   string
	Config    LadderConfig
	OrderBook *OrderBook
	Orders    []*LimitOrder // Orders at each level
}

// NewLadder creates a new ladder for an outcome
func NewLadder(outcome string, config LadderConfig, orderBook *OrderBook) *Ladder {
	return &Ladder{
		Outcome:   outcome,
		Config:    config,
		OrderBook: orderBook,
		Orders:    make([]*LimitOrder, 0),
	}
}

// PlaceLadder places all ladder orders
// Returns nil if BasePrice is not set (must be initialized from real market data first)
func (l *Ladder) PlaceLadder() []*LimitOrder {
	// Don't place orders if BasePrice hasn't been set from real market data
	if l.Config.BasePrice <= 0.01 || l.Config.BasePrice >= 0.99 {
		return nil
	}

	l.CancelAll() // Cancel existing orders first
	l.Orders = make([]*LimitOrder, 0, l.Config.Levels)

	for i := 0; i < l.Config.Levels; i++ {
		price := l.Config.BasePrice - (float64(i) * l.Config.PriceStep)
		if price <= 0.01 { // Minimum viable price
			break
		}

		order := l.OrderBook.PlaceOrder(
			l.Outcome,
			"buy",
			price,
			l.Config.SharesPerLevel,
			i,
		)
		l.Orders = append(l.Orders, order)
	}

	return l.Orders
}

// UpdateLadder updates ladder based on new fair price from real market data
// Returns nil if fairPrice is out of valid range
func (l *Ladder) UpdateLadder(fairPrice float64) []*LimitOrder {
	// Validate fair price is within reasonable bounds
	if fairPrice <= 0.05 || fairPrice >= 0.95 {
		return nil
	}
	// Adjust base price to be below fair price
	l.Config.BasePrice = fairPrice - 0.02 // Bid 2 cents below fair value
	return l.PlaceLadder()
}

// CancelAll cancels all ladder orders
func (l *Ladder) CancelAll() int {
	count := 0
	for _, order := range l.Orders {
		if order != nil && (order.Status == OrderStatusOpen || order.Status == OrderStatusPartial) {
			_ = l.OrderBook.CancelOrder(order.ID)
			count++
		}
	}
	l.Orders = make([]*LimitOrder, 0)
	return count
}

// GetActiveOrders returns currently active orders
func (l *Ladder) GetActiveOrders() []*LimitOrder {
	var active []*LimitOrder
	for _, order := range l.Orders {
		if order.Status == OrderStatusOpen || order.Status == OrderStatusPartial {
			active = append(active, order)
		}
	}
	return active
}

// GetTotalOpenValue returns total value of open orders in this ladder
func (l *Ladder) GetTotalOpenValue() float64 {
	total := 0.0
	for _, order := range l.Orders {
		if order.Status == OrderStatusOpen || order.Status == OrderStatusPartial {
			total += order.RemainingQty() * order.Price
		}
	}
	return total
}

// LadderManager manages ladders for all outcomes
type LadderManager struct {
	Ladders   map[string]*Ladder
	OrderBook *OrderBook
	Config    LadderConfig
}

// NewLadderManager creates a new ladder manager
func NewLadderManager(orderBook *OrderBook, config LadderConfig) *LadderManager {
	return &LadderManager{
		Ladders:   make(map[string]*Ladder),
		OrderBook: orderBook,
		Config:    config,
	}
}

// GetOrCreateLadder gets or creates a ladder for an outcome
func (lm *LadderManager) GetOrCreateLadder(outcome string) *Ladder {
	if ladder, exists := lm.Ladders[outcome]; exists {
		return ladder
	}
	ladder := NewLadder(outcome, lm.Config, lm.OrderBook)
	lm.Ladders[outcome] = ladder
	return ladder
}

// PlaceAllLadders places ladders for all outcomes based on target sum
// DEPRECATED: Use PlaceAllLaddersWithPrices for real market data
func (lm *LadderManager) PlaceAllLadders(outcomes []string, targetSum float64) {
	// Calculate fair price per side (assuming 50/50)
	fairPricePerSide := targetSum / 2.0

	for _, outcome := range outcomes {
		ladder := lm.GetOrCreateLadder(outcome)
		ladder.UpdateLadder(fairPricePerSide)
	}
}

// PlaceAllLaddersWithPrices places ladders for all outcomes using real market prices
// prices maps outcome name to the actual market ask price from Polymarket
func (lm *LadderManager) PlaceAllLaddersWithPrices(outcomes []string, prices map[string]float64) {
	for _, outcome := range outcomes {
		price := prices[outcome]
		if price <= 0.05 || price >= 0.95 {
			continue
		}
		ladder := lm.GetOrCreateLadder(outcome)
		ladder.UpdateLadder(price)
	}
}

// CancelAllLadders cancels all orders in all ladders
func (lm *LadderManager) CancelAllLadders() int {
	count := 0
	for _, ladder := range lm.Ladders {
		count += ladder.CancelAll()
	}
	return count
}

// GetAllOpenOrders returns all open orders across all ladders
func (lm *LadderManager) GetAllOpenOrders() []*LimitOrder {
	var orders []*LimitOrder
	for _, ladder := range lm.Ladders {
		orders = append(orders, ladder.GetActiveOrders()...)
	}
	return orders
}

// PrintLadders prints current ladder status
func (lm *LadderManager) PrintLadders() {
}
