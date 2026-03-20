package paper

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// Trade represents a single executed trade
type Trade struct {
	ID        int
	Timestamp time.Time
	Side      string // "buy" or "sell"
	Outcome   string // "Yes", "No", "Up", "Down"
	Price     float64
	Quantity  float64
	Value     float64 // Price * Quantity
}

// Position represents current holdings for an outcome
type Position struct {
	Outcome   string
	MarketID  string // Which market this position belongs to (e.g., "BTC", "ETH")
	Quantity  float64
	AvgPrice  float64
	TotalCost float64
}

// Stats holds all trading statistics
type Stats struct {
	TotalTrades     int
	WinningTrades   int
	LosingTrades    int
	RealizedPnL     float64
	UnrealizedPnL   float64
	MaxDrawdown     float64
	PeakBalance     float64
	CurrentBalance  float64
	StartingBalance float64
}

// Engine is the paper trading engine
type Engine struct {
	mu sync.RWMutex

	// Configuration
	startingBalance float64
	pnlBaseline     float64
	currentBalance  float64

	// Compounding multiplier - increases with profitable rounds
	compoundMultiplier float64
	sizingBalance      float64
	roundsCompleted    int
	profitableRounds   int
	losingRounds       int

	// Positions: "marketID:outcome" -> position
	positions map[string]*Position

	// Split inventory reference (for equity calculation)
	// Split tokens are worth $1.00 per YES+NO pair
	splitInventories []*SplitInventory

	// Pending on-chain redemption payouts that are already economically locked in
	// but have not yet returned to wallet cash.
	pendingRedemptions map[string]float64

	// Trade history (capped to prevent memory growth)
	trades    []Trade
	maxTrades int

	// Stats tracking
	totalTrades   int
	realizedPnL   float64
	peakBalance   float64
	maxDrawdown   float64
	winningTrades int
	losingTrades  int

	// Current market prices for unrealized PnL (legacy - outcome only)
	currentPrices map[string]float64
	// Bid/Ask prices for realistic taker simulation (legacy - outcome only)
	currentBids map[string]float64 // Price you get when SELLING (taker)
	currentAsks map[string]float64 // Price you pay when BUYING (taker)

	// Per-market bid/ask prices: "marketID:outcome" -> price
	marketBids map[string]float64
	marketAsks map[string]float64

	feeRateBps int // Fee configuration
}

// NewEngine creates a new paper trading engine
func NewEngine(startingBalance float64) *Engine {
	return &Engine{
		startingBalance:    startingBalance,
		pnlBaseline:        startingBalance,
		currentBalance:     startingBalance,
		peakBalance:        startingBalance,
		compoundMultiplier: 1.0, // Start at 1x
		sizingBalance:      startingBalance,
		maxTrades:          1000, // Cap trade history to prevent memory growth
		positions:          make(map[string]*Position),
		pendingRedemptions: make(map[string]float64),
		trades:             make([]Trade, 0),
		currentPrices:      make(map[string]float64),
		currentBids:        make(map[string]float64),
		currentAsks:        make(map[string]float64),
		marketBids:         make(map[string]float64),
		marketAsks:         make(map[string]float64),
	}
}

// SetFeeRateBps sets the fee rate for paper trading simulations
func (e *Engine) SetFeeRateBps(rate int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.feeRateBps = rate
}

// UpdatePrice updates current market price for an outcome and recalculates drawdown
func (e *Engine) UpdatePrice(outcome string, price float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.currentPrices[outcome] = price
	e.recalculateDrawdown()
}

// UpdateMarketData performs a batch update of price and bid/ask data for an outcome
func (e *Engine) UpdateMarketData(marketID, outcome string, price, bid, ask float64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if price > 0 {
		e.currentPrices[outcome] = price
	}
	if bid > 0 {
		e.currentBids[outcome] = bid
	}
	if ask > 0 {
		e.currentAsks[outcome] = ask
	}

	if marketID != "" {
		key := marketID + ":" + outcome
		if bid > 0 {
			e.marketBids[key] = bid
		}
		if ask > 0 {
			e.marketAsks[key] = ask
		}
	}

	e.recalculateDrawdown()
}

// ClearMarketData clears cached market prices to prevent memory growth across market rounds
func (e *Engine) ClearMarketData() {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Simulate merging all remaining split inventories back to cash
	// to prevent artificial balance loss on rotation/restart.
	inventoryValue := e.getSplitInventoryValue()
	if inventoryValue > 0 {
		e.currentBalance += inventoryValue
	}

	e.currentPrices = make(map[string]float64)
	e.currentBids = make(map[string]float64)
	e.currentAsks = make(map[string]float64)
	e.marketBids = make(map[string]float64)
	e.marketAsks = make(map[string]float64)
	// Clear split inventory references since they are recreated per round
	e.splitInventories = nil
}

// UpdateBidAsk updates bid/ask prices for realistic taker simulation
func (e *Engine) UpdateBidAsk(outcome string, bid, ask float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if bid > 0 {
		e.currentBids[outcome] = bid
	}
	if ask > 0 {
		e.currentAsks[outcome] = ask
	}
}

// UpdateMarketBidAsk updates bid/ask prices for a specific market
func (e *Engine) UpdateMarketBidAsk(marketID, outcome string, bid, ask float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	key := marketID + ":" + outcome
	if bid > 0 {
		e.marketBids[key] = bid
	}
	if ask > 0 {
		e.marketAsks[key] = ask
	}
}

// GetMarketBidAsk returns current bid/ask for a market position
func (e *Engine) GetMarketBidAsk(marketID, outcome string) (bid, ask float64) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	key := marketID + ":" + outcome
	return e.marketBids[key], e.marketAsks[key]
}

// Buy executes a simulated buy order
func (e *Engine) Buy(outcome string, price, quantity float64) (*Trade, error) {
	return e.BuyForMarket("", outcome, price, quantity)
}

// BuyForMarket executes a simulated buy order for a specific market
func (e *Engine) BuyForMarket(marketID, outcome string, price, quantity float64) (*Trade, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.executeBuy(marketID, outcome, price, quantity, false)
}

// MarketBuy executes a market order by consuming liquidity from provided levels
// This simulates a "taker" order that "chases" liquidity across multiple price levels.
func (e *Engine) MarketBuy(marketID, outcome string, quantity float64, levels []MarketLevel) (*Trade, float64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if len(levels) == 0 {
		return nil, 0, fmt.Errorf("no liquidity available for %s", outcome)
	}

	remaining := quantity
	totalCost := 0.0
	filledQty := 0.0

	// Levels should be sorted by price ascending for asks
	for _, lv := range levels {
		if lv.Size <= 0 {
			continue
		}

		take := math.Min(remaining, lv.Size)

		totalCost += take * lv.Price
		filledQty += take
		remaining -= take

		if remaining <= 0.0001 {
			break
		}
	}

	if filledQty <= 0 {
		return nil, 0, fmt.Errorf("insufficient liquidity to fill any amount for %s", outcome)
	}

	avgPrice := totalCost / filledQty
	trade, err := e.executeBuy(marketID, outcome, avgPrice, filledQty, false)
	return trade, avgPrice, err
}

// MarketBuyArb atomically executes a market buy for both sides of an arbitrage.
// It pre-checks the total cost against the current balance to prevent "legging"
// (where one side succeeds but the other fails due to insufficient funds).
func (e *Engine) MarketBuyArb(marketID, outcome1, outcome2 string, quantity float64, levels1, levels2 []MarketLevel) (*Trade, *Trade, float64, float64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// 1. Simulate the walk for Side 1
	if len(levels1) == 0 {
		return nil, nil, 0, 0, fmt.Errorf("no liquidity available for %s", outcome1)
	}
	rem1 := quantity
	cost1 := 0.0
	filled1 := 0.0
	for _, lv := range levels1 {
		if lv.Size <= 0 {
			continue
		}
		take := math.Min(rem1, lv.Size)
		cost1 += take * lv.Price
		filled1 += take
		rem1 -= take
		if rem1 <= 0.0001 {
			break
		}
	}
	if filled1 <= 0 {
		return nil, nil, 0, 0, fmt.Errorf("insufficient liquidity to fill any amount for %s", outcome1)
	}

	// 2. Simulate the walk for Side 2
	if len(levels2) == 0 {
		return nil, nil, 0, 0, fmt.Errorf("no liquidity available for %s", outcome2)
	}
	rem2 := quantity
	cost2 := 0.0
	filled2 := 0.0
	for _, lv := range levels2 {
		if lv.Size <= 0 {
			continue
		}
		take := math.Min(rem2, lv.Size)
		cost2 += take * lv.Price
		filled2 += take
		rem2 -= take
		if rem2 <= 0.0001 {
			break
		}
	}
	if filled2 <= 0 {
		return nil, nil, 0, 0, fmt.Errorf("insufficient liquidity to fill any amount for %s", outcome2)
	}

	// 3. Match quantities to ensure we don't buy unbalanced legs
	// The realbot limits quantity to matched liquidity beforehand, so this is just a safety
	minFilled := math.Min(filled1, filled2)

	// Recalculate costs for exactly minFilled
	cost1 = 0.0
	rem1 = minFilled
	for _, lv := range levels1 {
		if lv.Size <= 0 {
			continue
		}
		take := math.Min(rem1, lv.Size)
		cost1 += take * lv.Price
		rem1 -= take
		if rem1 <= 0.0001 {
			break
		}
	}

	cost2 = 0.0
	rem2 = minFilled
	for _, lv := range levels2 {
		if lv.Size <= 0 {
			continue
		}
		take := math.Min(rem2, lv.Size)
		cost2 += take * lv.Price
		rem2 -= take
		if rem2 <= 0.0001 {
			break
		}
	}

	avgPrice1 := cost1 / minFilled
	avgPrice2 := cost2 / minFilled

	// 4. ATOMIC BALANCE CHECK
	totalCost := cost1 + cost2
	if totalCost > e.currentBalance {
		return nil, nil, 0, 0, fmt.Errorf("insufficient balance for arb: need %.4f, have %.4f", totalCost, e.currentBalance)
	}

	// 5. Execute both buys (guaranteed to succeed since we hold the lock and checked balance)
	trade1, err1 := e.executeBuy(marketID, outcome1, avgPrice1, minFilled, false)
	if err1 != nil {
		return nil, nil, 0, 0, err1
	}

	trade2, err2 := e.executeBuy(marketID, outcome2, avgPrice2, minFilled, false)
	if err2 != nil {
		return nil, nil, 0, 0, err2
	}

	return trade1, trade2, avgPrice1, avgPrice2, nil
}

// MakerBuyForMarket executes a simulated maker buy order for a specific market (zero fees)
func (e *Engine) MakerBuyForMarket(marketID, outcome string, price, quantity float64) (*Trade, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.executeBuy(marketID, outcome, price, quantity, true)
}

// MakerSellForMarket executes a simulated maker sell order for a market-specific position (zero fees).
func (e *Engine) MakerSellForMarket(marketID, outcome string, price, quantity float64) (*Trade, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	posKey := outcome
	if marketID != "" {
		posKey = marketID + ":" + outcome
	}
	return e.executeSell(posKey, outcome, price, quantity, true)
}

// executeBuy is the internal implementation of a buy (must be called with lock)
func (e *Engine) executeBuy(marketID, outcome string, price, quantity float64, isMaker bool) (*Trade, error) {
	cost := price * quantity
	if cost > e.currentBalance+1e-8 {
		return nil, fmt.Errorf("insufficient balance: need %.4f, have %.4f", cost, e.currentBalance)
	}

	// Deduct from balance
	e.currentBalance -= cost
	if e.currentBalance < 0 {
		e.currentBalance = 0
	}

	// Calculate fee (collected in shares for BUY)
	feeTokens := 0.0
	if !isMaker && e.feeRateBps > 0 {
		// Calculate curve fee for crypto markets (feeRate=0.25, exponent=2.0)
		feeTokens = quantity * 0.25 * math.Pow(price*(1.0-price), 2.0)
	}
	netQuantity := quantity - feeTokens

	// Create position key that includes market ID
	posKey := outcome
	if marketID != "" {
		posKey = marketID + ":" + outcome
	}

	// Update position
	pos, exists := e.positions[posKey]
	if !exists {
		pos = &Position{Outcome: outcome, MarketID: marketID}
		e.positions[posKey] = pos
	}

	// Calculate new average price
	totalQty := pos.Quantity + netQuantity
	if totalQty > 0 {
		pos.AvgPrice = (pos.TotalCost + cost) / totalQty
	}
	pos.Quantity = totalQty
	pos.TotalCost += cost

	// Record trade
	e.totalTrades++
	trade := Trade{
		ID:        e.totalTrades,
		Timestamp: time.Now(),
		Side:      "buy",
		Outcome:   outcome,
		Price:     price,
		Quantity:  netQuantity,
		Value:     cost,
	}
	e.addTrade(trade)

	return &trade, nil
}

// Sell executes a simulated sell order
func (e *Engine) Sell(outcome string, price, quantity float64) (*Trade, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.executeSell(outcome, outcome, price, quantity, false)
}

// SellForMarket executes a simulated sell order for a market-specific position.
func (e *Engine) SellForMarket(marketID, outcome string, price, quantity float64) (*Trade, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	posKey := outcome
	if marketID != "" {
		posKey = marketID + ":" + outcome
	}
	return e.executeSell(posKey, outcome, price, quantity, false)
}

func (e *Engine) executeSell(posKey, outcome string, price, quantity float64, isMaker bool) (*Trade, error) {

	pos, exists := e.positions[posKey]
	if !exists || pos.Quantity < quantity {
		available := 0.0
		if exists {
			available = pos.Quantity
		}
		return nil, fmt.Errorf("insufficient position: need %.4f, have %.4f", quantity, available)
	}

	// Calculate fee (collected in USDC for SELL)
	feeUsdc := 0.0
	if !isMaker && e.feeRateBps > 0 {
		feeTokens := quantity * 0.25 * math.Pow(price*(1.0-price), 2.0)
		feeUsdc = feeTokens * price
	}

	// Calculate PnL for this sale
	proceeds := (price * quantity) - feeUsdc
	costBasis := pos.AvgPrice * quantity
	pnl := proceeds - costBasis

	// Update realized PnL
	e.realizedPnL += pnl
	if pnl > 0 {
		e.winningTrades++
	} else if pnl < 0 {
		e.losingTrades++
	}

	// Add proceeds to balance
	e.currentBalance += proceeds

	// Update position
	pos.Quantity -= quantity
	pos.TotalCost -= costBasis
	if pos.Quantity <= 0.0001 {
		delete(e.positions, posKey)
	}

	// Update peak and drawdown
	e.updateDrawdown()

	// Record trade
	e.totalTrades++
	trade := Trade{
		ID:        e.totalTrades,
		Timestamp: time.Now(),
		Side:      "sell",
		Outcome:   outcome,
		Price:     price,
		Quantity:  quantity,
		Value:     proceeds,
	}
	e.addTrade(trade)

	return &trade, nil
}

// RedemptionResult holds detailed info about a market redemption
type RedemptionResult struct {
	WinningOutcome string
	WinningShares  float64
	WinningPayout  float64
	WinningCost    float64
	WinningPnL     float64
	LosingOutcome  string
	LosingShares   float64
	LosingCost     float64
	TotalPayout    float64
	TotalPnL       float64
}

// Redeem simulates market resolution payout
// winningOutcome is the outcome that won (pays $1 per share)
// Polymarket charges NO fees (0% on trading, deposits, withdrawals, and payouts)
func (e *Engine) Redeem(winningOutcome string) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()

	payout := 0.0

	for _, pos := range e.positions {
		if pos.Outcome == winningOutcome {
			// Winning shares pay $1 each (no fees!)
			proceeds := pos.Quantity * 1.0
			pnl := proceeds - pos.TotalCost
			e.realizedPnL += pnl
			e.currentBalance += proceeds
			payout += proceeds

			if pnl > 0 {
				e.winningTrades++
			} else {
				e.losingTrades++
			}
		} else {
			// Losing shares are worthless
			e.realizedPnL -= pos.TotalCost
			e.losingTrades++
		}
	}

	// Clear all positions
	e.positions = make(map[string]*Position)

	e.updateDrawdown()
	return payout
}

// RedeemWithDetails simulates market resolution and returns detailed results
func (e *Engine) RedeemWithDetails(marketID, winningOutcome string) *RedemptionResult {
	e.mu.Lock()
	defer e.mu.Unlock()

	result := &RedemptionResult{
		WinningOutcome: winningOutcome,
	}

	prefix := ""
	if marketID != "" {
		prefix = marketID + ":"
	}

	for key, pos := range e.positions {
		// Only process positions for this market
		if marketID != "" && !strings.HasPrefix(key, prefix) && pos.MarketID != marketID {
			continue
		}

		// Correctly match the outcome
		if pos.Outcome == winningOutcome {
			// Winning shares are economically locked in at $1 each, but on-chain cash
			// may not arrive until the redeem transaction confirms.
			proceeds := pos.Quantity * 1.0
			pnl := proceeds - pos.TotalCost
			e.realizedPnL += pnl

			result.WinningShares += pos.Quantity
			result.WinningPayout += proceeds
			result.WinningCost += pos.TotalCost
			result.WinningPnL += pnl
			result.TotalPayout += proceeds

			if pnl > 0 {
				e.winningTrades++
			} else {
				e.losingTrades++
			}
		} else {
			// Losing shares are worthless
			e.realizedPnL -= pos.TotalCost
			e.losingTrades++

			result.LosingOutcome = pos.Outcome
			result.LosingShares += pos.Quantity
			result.LosingCost += pos.TotalCost
		}

		// Remove processed position
		delete(e.positions, key)
	}

	// Now redeem split inventories
	for _, inv := range e.splitInventories {
		payout, pnl := inv.Redeem(marketID, winningOutcome)
		if payout > 0 || pnl != 0 {
			e.currentBalance += payout
			e.realizedPnL += pnl
			result.TotalPayout += payout
			result.WinningPayout += payout
			result.WinningPnL += pnl
		}
	}

	if marketID != "" && result.WinningPayout > 0 {
		e.pendingRedemptions[marketID] = result.WinningPayout
	}

	result.TotalPnL = result.WinningPnL - result.LosingCost

	e.updateDrawdown()
	return result
}

// MergeResult holds detailed info about a merge operation
type MergeResult struct {
	MarketID    string
	Outcome1    string
	Outcome2    string
	Shares      float64
	TotalCost   float64 // What we paid for both sides
	TotalPayout float64 // $1 * shares received back
	PnL         float64 // Profit from the merge
}

// MergeForMarket simulates merging equal YES+NO tokens back into USDC
// This is used after buying both sides of an arb - instantly captures profit
// without waiting for market resolution.
// Returns the profit from the merge.
func (e *Engine) MergeForMarket(marketID, outcome1, outcome2 string, shares float64) *MergeResult {
	e.mu.Lock()
	defer e.mu.Unlock()

	result := &MergeResult{
		MarketID: marketID,
		Outcome1: outcome1,
		Outcome2: outcome2,
		Shares:   shares,
	}

	// Build position keys
	key1 := outcome1
	key2 := outcome2
	if marketID != "" {
		key1 = marketID + ":" + outcome1
		key2 = marketID + ":" + outcome2
	}

	// Get positions
	pos1, exists1 := e.positions[key1]
	pos2, exists2 := e.positions[key2]

	if !exists1 || !exists2 {
		return result // No positions to merge
	}

	// Merge the minimum of both positions (must have equal shares to merge)
	mergeQty := shares
	if pos1.Quantity < mergeQty {
		mergeQty = pos1.Quantity
	}
	if pos2.Quantity < mergeQty {
		mergeQty = pos2.Quantity
	}

	if mergeQty <= 0 {
		return result
	}

	// Calculate cost basis for merged shares
	costBasis1 := (pos1.TotalCost / pos1.Quantity) * mergeQty
	costBasis2 := (pos2.TotalCost / pos2.Quantity) * mergeQty
	totalCost := costBasis1 + costBasis2

	// Merge returns $1 per share (full set = 1 YES + 1 NO = $1 USDC)
	payout := mergeQty * 1.0
	pnl := payout - totalCost

	result.TotalCost = totalCost
	result.TotalPayout = payout
	result.PnL = pnl

	// Update balance
	e.currentBalance += payout
	e.realizedPnL += pnl

	// Update positions
	pos1.Quantity -= mergeQty
	pos1.TotalCost -= costBasis1
	pos2.Quantity -= mergeQty
	pos2.TotalCost -= costBasis2

	// Clean up empty positions
	if pos1.Quantity <= 0.0001 {
		delete(e.positions, key1)
	}
	if pos2.Quantity <= 0.0001 {
		delete(e.positions, key2)
	}

	// Track as winning trade if profitable
	if pnl > 0 {
		e.winningTrades++
	} else if pnl < 0 {
		e.losingTrades++
	}

	e.updateDrawdown()
	return result
}

// LiquidateAll sells all positions at current BID prices (taker - chasing liquidity)
// This simulates realistic emergency exit where you SELL at the BID (worse price)
func (e *Engine) LiquidateAll() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()

	totalProceeds := 0.0

	for outcome, pos := range e.positions {
		if pos.Quantity <= 0 {
			continue
		}

		// TAKER SELL: Use BID price (the price buyers are willing to pay)
		// This is worse than mid-price, simulating realistic slippage
		price := pos.AvgPrice // Fallback to cost basis

		// Price sanity bounds - disabled to defer to config settings
		const minSanePrice = 0.00
		const maxSanePrice = 1.00

		if bid, ok := e.currentBids[outcome]; ok && bid >= minSanePrice && bid <= maxSanePrice {
			price = bid // Use BID for taker sells
		} else if p, ok := e.currentPrices[outcome]; ok && p >= minSanePrice && p <= maxSanePrice {
			// Fallback to mid-price with simulated slippage (2% worse)
			price = p * 0.98
		}

		// Calculate fee (collected in USDC for SELL)
		feeUsdc := 0.0
		if e.feeRateBps > 0 {
			feeTokens := pos.Quantity * 0.25 * math.Pow(price*(1.0-price), 2.0)
			feeUsdc = feeTokens * price
		}

		proceeds := (pos.Quantity * price) - feeUsdc
		pnl := proceeds - pos.TotalCost
		e.realizedPnL += pnl
		e.currentBalance += proceeds
		totalProceeds += proceeds

		if pnl > 0 {
			e.winningTrades++
		} else if pnl < 0 {
			e.losingTrades++
		}

		e.totalTrades++
		trade := Trade{
			ID:        e.totalTrades,
			Timestamp: time.Now(),
			Side:      "sell",
			Outcome:   outcome,
			Price:     price,
			Quantity:  pos.Quantity,
			Value:     proceeds,
		}
		e.addTrade(trade)
	}

	// Clear all positions
	e.positions = make(map[string]*Position)

	// Liquidate split inventory (merge back to cash)
	inventoryValue := e.getSplitInventoryValue()
	if inventoryValue > 0 {
		e.currentBalance += inventoryValue
		totalProceeds += inventoryValue
		// No realized PnL because split shares hold their cash value exactly
	}
	e.splitInventories = nil

	e.recalculateDrawdown()

	return totalProceeds
}

func (e *Engine) updateDrawdown() {
	e.recalculateDrawdown()
}

// addTrade records a trade and trims history if needed (must be called with lock held)
func (e *Engine) addTrade(trade Trade) {
	e.trades = append(e.trades, trade)
	// Trim trade history if it exceeds max to prevent memory growth
	if len(e.trades) > e.maxTrades {
		e.trades = e.trades[len(e.trades)-e.maxTrades:]
	}
}

// recalculateDrawdown updates max drawdown based on current equity
func (e *Engine) recalculateDrawdown() {
	totalEquity := e.getBookEquity()
	if totalEquity > e.peakBalance {
		e.peakBalance = totalEquity
	}
	if e.peakBalance > 0 {
		drawdown := (e.peakBalance - totalEquity) / e.peakBalance
		if drawdown > e.maxDrawdown {
			e.maxDrawdown = drawdown
		}
	}
}

// GetEquity returns total equity (balance + unrealized value)
func (e *Engine) GetEquity() float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.currentBalance + e.getUnrealizedValue() + e.getSplitInventoryValue()
}

// GetBookEquity returns cash plus cost basis for open positions and split inventory.
// This keeps unresolved inventory neutral until it is actually sold, merged, or redeemed.
func (e *Engine) GetBookEquity() float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.getBookEquity()
}

// GetPeakEquity returns the highest equity seen so far
func (e *Engine) GetPeakEquity() float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.peakBalance
}

// getSplitInventoryValue returns the value of all split inventories
// Split tokens are worth $1.00 per YES+NO pair (can merge anytime)
func (e *Engine) getSplitInventoryValue() float64 {
	value := 0.0
	for _, inv := range e.splitInventories {
		_, _, unrealizedValue := inv.GetStats()
		// unrealizedValue is shares * $0.50 cost basis per share
		// But a YES+NO pair is worth $1.00, so we need to count pairs
		// The unrealizedValue already accounts for this correctly
		value += unrealizedValue
	}
	return value
}

func (e *Engine) getPositionBookValue() float64 {
	byMarket := make(map[string][]*Position)
	for _, pos := range e.positions {
		marketID := pos.MarketID
		if marketID == "" {
			marketID = "__default__"
		}
		byMarket[marketID] = append(byMarket[marketID], pos)
	}

	value := 0.0
	for _, positions := range byMarket {
		if len(positions) < 2 {
			for _, pos := range positions {
				value += pos.TotalCost
			}
			continue
		}

		matchedQty := positions[0].Quantity
		for _, pos := range positions[1:] {
			if pos.Quantity < matchedQty {
				matchedQty = pos.Quantity
			}
		}
		if matchedQty < 0 {
			matchedQty = 0
		}

		value += matchedQty
		for _, pos := range positions {
			if pos.Quantity <= 0 {
				continue
			}
			unmatchedQty := pos.Quantity - matchedQty
			if unmatchedQty <= 0 {
				continue
			}
			value += (pos.TotalCost / pos.Quantity) * unmatchedQty
		}
	}
	return value
}

func (e *Engine) getPendingRedemptionValue() float64 {
	value := 0.0
	for _, payout := range e.pendingRedemptions {
		value += payout
	}
	return value
}

func (e *Engine) getBookEquity() float64 {
	return e.currentBalance + e.getPositionBookValue() + e.getSplitInventoryValue() + e.getPendingRedemptionValue()
}

func (e *Engine) getUnrealizedValue() float64 {
	value := 0.0
	for key, pos := range e.positions {
		// Use BID price for valuation (what we could sell for)
		// This is more conservative and realistic
		if bid, ok := e.marketBids[key]; ok && bid > 0 {
			value += pos.Quantity * bid
		} else if bid, ok := e.currentBids[pos.Outcome]; ok && bid > 0 {
			value += pos.Quantity * bid
		} else if price, ok := e.currentPrices[pos.Outcome]; ok {
			value += pos.Quantity * price
		} else {
			// Use cost basis if no current price
			value += pos.TotalCost
		}
	}
	return value
}

// GetUnrealizedPnL returns unrealized profit/loss
func (e *Engine) GetUnrealizedPnL() float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.getUnrealizedPnL()
}

func (e *Engine) getUnrealizedPnL() float64 {
	unrealized := 0.0
	for key, pos := range e.positions {
		// Use BID price for valuation (what we could sell for)
		if bid, ok := e.marketBids[key]; ok && bid > 0 {
			currentValue := pos.Quantity * bid
			unrealized += currentValue - pos.TotalCost
		} else if bid, ok := e.currentBids[pos.Outcome]; ok && bid > 0 {
			currentValue := pos.Quantity * bid
			unrealized += currentValue - pos.TotalCost
		} else if price, ok := e.currentPrices[pos.Outcome]; ok {
			currentValue := pos.Quantity * price
			unrealized += currentValue - pos.TotalCost
		}
	}
	return unrealized
}

// GetStats returns current trading statistics
func (e *Engine) GetStats() Stats {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return Stats{
		TotalTrades:     e.totalTrades,
		WinningTrades:   e.winningTrades,
		LosingTrades:    e.losingTrades,
		RealizedPnL:     e.realizedPnL,
		UnrealizedPnL:   e.getUnrealizedPnL(),
		MaxDrawdown:     e.maxDrawdown * 100, // as percentage
		PeakBalance:     e.peakBalance,
		CurrentBalance:  e.currentBalance,
		StartingBalance: e.pnlBaseline,
	}
}

// GetPositions returns current positions
func (e *Engine) GetPositions() map[string]Position {
	e.mu.RLock()
	defer e.mu.RUnlock()

	result := make(map[string]Position)
	for k, v := range e.positions {
		result[k] = *v
	}
	return result
}

// PositionPnL contains real-time P&L info for a position
type PositionPnL struct {
	Position
	CurrentBid    float64 // Current bid price (what we can sell for)
	CurrentAsk    float64 // Current ask price
	MarketValue   float64 // Current value if sold at bid
	UnrealizedPnL float64 // Current P&L if sold now
	LockedPnL     float64 // Guaranteed P&L if held to resolution ($1 payout)
}

// GetPositionsWithPnL returns positions with real-time P&L calculations
func (e *Engine) GetPositionsWithPnL() map[string]PositionPnL {
	e.mu.RLock()
	defer e.mu.RUnlock()

	result := make(map[string]PositionPnL)
	for k, pos := range e.positions {
		pnl := PositionPnL{
			Position: *pos,
		}

		// Get current bid/ask for this position
		key := k // Already includes marketID:outcome
		if bid, ok := e.marketBids[key]; ok && bid > 0 {
			pnl.CurrentBid = bid
		} else if bid, ok := e.currentBids[pos.Outcome]; ok && bid > 0 {
			pnl.CurrentBid = bid
		}

		if ask, ok := e.marketAsks[key]; ok && ask > 0 {
			pnl.CurrentAsk = ask
		} else if ask, ok := e.currentAsks[pos.Outcome]; ok && ask > 0 {
			pnl.CurrentAsk = ask
		}

		// Calculate market value (what we could sell for now)
		if pnl.CurrentBid > 0 {
			pnl.MarketValue = pos.Quantity * pnl.CurrentBid
			pnl.UnrealizedPnL = pnl.MarketValue - pos.TotalCost
		}

		// Locked P&L assumes $1 payout at resolution
		// This is only meaningful for matched pairs
		pnl.LockedPnL = (pos.Quantity * 1.0) - pos.TotalCost

		result[k] = pnl
	}
	return result
}

// GetRecentTrades returns the last n trades
func (e *Engine) GetRecentTrades(n int) []Trade {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if n > len(e.trades) {
		n = len(e.trades)
	}
	start := len(e.trades) - n
	result := make([]Trade, n)
	copy(result, e.trades[start:])
	return result
}

// GetBalance returns current cash balance
func (e *Engine) GetBalance() float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.currentBalance
}

// GetStartingBalance returns the PnL baseline used for account-level display and sizing.
func (e *Engine) GetStartingBalance() float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.pnlBaseline
}

func (e *Engine) SetPendingRedemption(marketID string, payout float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if marketID == "" {
		return
	}
	if payout <= 0 {
		delete(e.pendingRedemptions, marketID)
	} else {
		e.pendingRedemptions[marketID] = payout
	}
	e.recalculateDrawdown()
}

func (e *Engine) ClearPendingRedemption(marketID string) {
	e.SetPendingRedemption(marketID, 0)
}

// DeductBalance removes amount from balance (for split operations)
func (e *Engine) DeductBalance(amount float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if amount > e.currentBalance {
		amount = e.currentBalance
	}
	e.currentBalance -= amount
}

// AddBalance adds amount to balance (for split sell/merge proceeds)
func (e *Engine) AddBalance(amount float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.currentBalance += amount
}

// AddRealizedPnL adds realized PnL to the engine stats
func (e *Engine) AddRealizedPnL(pnl float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.realizedPnL += pnl
	if pnl > 0 {
		e.winningTrades++
	} else if pnl < 0 {
		e.losingTrades++
	}
}

// RegisterSplitInventory registers a split inventory for equity calculation
// Split tokens are worth $1.00 per YES+NO pair (can merge anytime)
func (e *Engine) RegisterSplitInventory(inv *SplitInventory) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, existing := range e.splitInventories {
		if existing == inv {
			return
		}
	}
	e.splitInventories = append(e.splitInventories, inv)
}

// SetBalance sets the current balance (for syncing with on-chain balance)
func (e *Engine) SetBalance(balance float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.currentBalance = balance
}

func (e *Engine) refreshCompoundStateLocked(candidateSizingBalance float64) {
	if candidateSizingBalance > e.sizingBalance {
		e.sizingBalance = candidateSizingBalance
	}
	if e.pnlBaseline > e.sizingBalance {
		e.sizingBalance = e.pnlBaseline
	}

	base := e.pnlBaseline
	if base <= 0 {
		e.compoundMultiplier = 1.0
		return
	}

	multiplier := e.sizingBalance / base
	if multiplier < 1.0 {
		multiplier = 1.0
	}
	if multiplier > 3.0 {
		multiplier = 3.0
		e.sizingBalance = base * multiplier
	}
	e.compoundMultiplier = multiplier
}

// SyncExternalPosition aligns the shadow position inventory to authoritative
// external holdings without changing cash balance or trade statistics.
func (e *Engine) SyncExternalPosition(marketID, outcome string, quantity, markPrice float64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	posKey := outcome
	if marketID != "" {
		posKey = marketID + ":" + outcome
	}

	const eps = 1e-6

	if quantity <= eps {
		if _, exists := e.positions[posKey]; exists {
			delete(e.positions, posKey)
			e.recalculateDrawdown()
			return true
		}
		return false
	}

	if markPrice <= 0 {
		markPrice = 0.5
	}

	pos, exists := e.positions[posKey]
	if !exists {
		totalCost := quantity * markPrice
		e.positions[posKey] = &Position{
			Outcome:   outcome,
			MarketID:  marketID,
			Quantity:  quantity,
			AvgPrice:  markPrice,
			TotalCost: totalCost,
		}
		// External carry should start neutral in the session PnL view until it is
		// actually sold, merged, or redeemed on-chain.
		e.pnlBaseline += totalCost
		e.refreshCompoundStateLocked(e.pnlBaseline)
		e.recalculateDrawdown()
		return true
	}

	changed := false
	switch {
	case quantity > pos.Quantity+eps:
		addQty := quantity - pos.Quantity
		pos.TotalCost += addQty * markPrice
		e.pnlBaseline += addQty * markPrice
		e.refreshCompoundStateLocked(e.pnlBaseline)
		pos.Quantity = quantity
		changed = true
	case quantity < pos.Quantity-eps:
		if pos.Quantity > eps {
			pos.TotalCost *= quantity / pos.Quantity
		} else {
			pos.TotalCost = quantity * markPrice
		}
		pos.Quantity = quantity
		changed = true
	}

	if pos.Quantity > eps {
		newAvgPrice := pos.TotalCost / pos.Quantity
		if math.Abs(pos.AvgPrice-newAvgPrice) > eps {
			pos.AvgPrice = newAvgPrice
			changed = true
		}
	} else {
		delete(e.positions, posKey)
		changed = true
	}

	if changed {
		e.recalculateDrawdown()
	}
	return changed
}

// RecalculateDrawdown manually triggers a drawdown recalculation.
// Use this after performing a multi-step operation (like a split) to ensure
// drawdown is checked only when the state is consistent.
func (e *Engine) RecalculateDrawdown() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.recalculateDrawdown()
}

// GetExposure returns exposure metrics
func (e *Engine) GetExposure() (totalExposure float64, maxSingleExposure float64) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	for _, pos := range e.positions {
		totalExposure += pos.TotalCost
		if pos.TotalCost > maxSingleExposure {
			maxSingleExposure = pos.TotalCost
		}
	}
	return
}

// Round helper for display
func Round(val float64, precision int) float64 {
	ratio := math.Pow(10, float64(precision))
	return math.Round(val*ratio) / ratio
}

// GetCompoundMultiplier returns the current compounding multiplier
func (e *Engine) GetCompoundMultiplier() float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.compoundMultiplier
}

// GetSizingBalance returns the ratcheting balance used for trade sizing.
// It starts at the session baseline and only moves up when profitable rounds
// lock in a new high-water mark.
func (e *Engine) GetSizingBalance() float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.sizingBalance
}

// UpdateCompoundMultiplier updates the multiplier based on round profit/loss
// Profitable rounds ratchet the sizing base upward to the new high-water mark.
// Losses do not shrink sizing; they only affect round win/loss reporting.
func (e *Engine) UpdateCompoundMultiplier(roundPnL float64, startingEquity float64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.roundsCompleted++

	if roundPnL > 0 {
		e.profitableRounds++
		endingEquity := startingEquity + roundPnL
		if endingEquity > 0 {
			e.refreshCompoundStateLocked(endingEquity)
			return
		}
	} else if roundPnL < 0 {
		e.losingRounds++
	}

	e.refreshCompoundStateLocked(0)
}

// GetCompoundStats returns compounding statistics
func (e *Engine) GetCompoundStats() (multiplier float64, rounds int, profitable int, losing int, sizingBalance float64) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.compoundMultiplier, e.roundsCompleted, e.profitableRounds, e.losingRounds, e.sizingBalance
}
