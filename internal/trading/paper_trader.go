package trading

import (
	"context"
	"fmt"
	"math"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
)

// PaperTrader implements Trader for paper trading
type PaperTrader struct {
	engine    *paper.Engine
	orderBook *paper.OrderBook
}

// NewPaperTrader creates a new paper trader
func NewPaperTrader(engine *paper.Engine, orderBook *paper.OrderBook) *PaperTrader {
	return &PaperTrader{
		engine:    engine,
		orderBook: orderBook,
	}
}

func (t *PaperTrader) Buy(ctx context.Context, tokenID, outcome string, price, size float64, orderType api.OrderType, tif api.TimeInForce, feeRateBps int) (*TradeResult, error) {
	cost := price * size
	fee := core.PolymarketTakerFeeUSDC(size, price, feeRateBps)

	trade, err := t.engine.BuyWithFeeRate(outcome, price, size, feeRateBps)
	if err != nil {
		return &TradeResult{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	return &TradeResult{
		OrderID:              fmt.Sprintf("paper-%d", time.Now().UnixNano()),
		Status:               "FILLED",
		Success:              true,
		Price:                price,
		Size:                 trade.Quantity,
		Fee:                  fee,
		FeeRateBps:           feeRateBps,
		Side:                 "BUY",
		TokenID:              tokenID,
		Outcome:              outcome,
		AcknowledgedQty:      trade.Quantity,
		AcknowledgedNotional: trade.Value,
		Timestamp:            time.Now(),
		Message:              fmt.Sprintf("Bought %.5f %s @ $%.4f (cost: $%.2f, fee: $%.5f)", trade.Quantity, outcome, price, cost, fee),
	}, nil
}

func (t *PaperTrader) Sell(ctx context.Context, tokenID, outcome string, price, size float64, orderType api.OrderType, tif api.TimeInForce, feeRateBps int) (*TradeResult, error) {
	fee := core.PolymarketTakerFeeUSDC(size, price, feeRateBps)

	_, err := t.engine.SellWithFeeRate(outcome, price, size, feeRateBps)
	if err != nil {
		return &TradeResult{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	return &TradeResult{
		OrderID:              fmt.Sprintf("paper-%d", time.Now().UnixNano()),
		Status:               "FILLED",
		Success:              true,
		Price:                price,
		Size:                 size,
		Fee:                  fee,
		FeeRateBps:           feeRateBps,
		Side:                 "SELL",
		TokenID:              tokenID,
		Outcome:              outcome,
		AcknowledgedQty:      size,
		AcknowledgedNotional: (price * size) - fee,
		Timestamp:            time.Now(),
		Message:              fmt.Sprintf("Sold %.5f %s @ $%.4f (fee: $%.5f)", size, outcome, price, fee),
	}, nil
}

func (t *PaperTrader) CancelOrder(ctx context.Context, orderID string) error {
	// Paper trading doesn't track individual orders in the same way
	return nil
}

func (t *PaperTrader) CancelAll(ctx context.Context) error {
	return nil
}

func (t *PaperTrader) GetBalance(ctx context.Context) (float64, error) {
	return t.engine.GetBalance(), nil
}

func (t *PaperTrader) GetPositions(ctx context.Context) ([]PositionInfo, error) {
	enginePositions := t.engine.GetPositions()

	positions := make([]PositionInfo, 0, len(enginePositions))
	for _, pos := range enginePositions {
		positions = append(positions, PositionInfo{
			Outcome:  pos.Outcome,
			Size:     pos.Quantity,
			AvgPrice: pos.AvgPrice,
		})
	}
	return positions, nil
}

func (t *PaperTrader) IsPaperMode() bool {
	return true
}

func (t *PaperTrader) IsTestMode() bool {
	return false
}

func (t *PaperTrader) GetMarketInfo(ctx context.Context, conditionID string) (*api.MarketInfo, error) {
	// Paper trader doesn't have real market info access
	return nil, fmt.Errorf("not implemented for paper trader")
}

func (t *PaperTrader) GetTradingAllowance(ctx context.Context) (float64, error) {
	return math.MaxFloat64, nil
}
