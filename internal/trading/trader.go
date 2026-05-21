package trading

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
)

const (
	realTraderBalanceCacheTTL     = 30 * time.Second
	realTraderOnChainBalanceTTL   = 20 * time.Second
	realTraderOnChainRetryBackoff = 3 * time.Second
	realTraderCTFBalanceTTL       = 15 * time.Second
)

// Trader defines the interface for placing trades (paper or real)
type Trader interface {
	// Buy places a buy order
	Buy(ctx context.Context, tokenID, outcome string, price, size float64, orderType api.OrderType, tif api.TimeInForce, feeRateBps int) (*TradeResult, error)

	// Sell places a sell order
	Sell(ctx context.Context, tokenID, outcome string, price, size float64, orderType api.OrderType, tif api.TimeInForce, feeRateBps int) (*TradeResult, error)

	// CancelOrder cancels an existing order
	CancelOrder(ctx context.Context, orderID string) error

	// CancelAll cancels all open orders
	CancelAll(ctx context.Context) error

	// GetBalance returns the current available balance
	GetBalance(ctx context.Context) (float64, error)

	// GetPositions returns an authoritative external position snapshot.
	GetPositions(ctx context.Context) ([]PositionInfo, error)

	// IsPaperMode returns true if this is paper trading
	IsPaperMode() bool

	// IsTestMode returns true if in test mode (validating but not submitting orders)
	IsTestMode() bool

	// GetMarketInfo retrieves market info including resolution status
	GetMarketInfo(ctx context.Context, conditionID string) (*api.MarketInfo, error)

	// GetTradingAllowance returns the currently authorized trading allowance (pUSD collateral)
	GetTradingAllowance(ctx context.Context) (float64, error)
}

// TradeResult represents the result of a trade attempt
type TradeResult struct {
	OrderID              string
	Status               string
	Success              bool
	Message              string
	Price                float64
	Size                 float64
	Fee                  float64
	FeeRateBps           int
	Side                 string
	TokenID              string
	Outcome              string
	MakingAmount         string
	TakingAmount         string
	TransactionsHashes   []string
	TradeIDs             []string
	AcknowledgedQty      float64
	AcknowledgedNotional float64
	Timestamp            time.Time
}

// PositionInfo represents a held position
type PositionInfo struct {
	TokenID         string
	Outcome         string
	Size            float64
	AvgPrice        float64
	ConditionID     string
	Slug            string
	OppositeOutcome string
	OppositeAsset   string
}

func parseMicroAmount(raw string) float64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	val, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0
	}
	if strings.ContainsAny(raw, ".eE") {
		return val
	}
	// Polymarket responses are inconsistent here: some endpoints return integer
	// micros, others return already-decimal token/notional amounts. Large whole
	// numbers are treated as micros; small integers are treated as literal units.
	if math.Abs(val) >= 1000 {
		return val / 1e6
	}
	return val
}

func responseWasImmediatelyMatched(resp *api.OrderResponse) bool {
	if resp == nil {
		return false
	}
	status := strings.ToUpper(strings.TrimSpace(resp.Status))
	return status == "MATCHED" || status == "FILLED" || len(resp.TransactionsHashes) > 0 || len(resp.TradeIDs) > 0
}

func deriveAcknowledgedExecution(resp *api.OrderResponse, side api.Side) (qty float64, notional float64) {
	if !responseWasImmediatelyMatched(resp) {
		return 0, 0
	}
	making := parseMicroAmount(resp.MakingAmount)
	taking := parseMicroAmount(resp.TakingAmount)
	if side == api.SideBuy {
		return taking, making
	}
	return making, taking
}

func orderRequestIsMarketBuyAmount(req *api.OrderRequest) bool {
	if req == nil || req.Side != api.SideBuy || req.UseMarketBuyPrecision {
		return false
	}
	if req.OrderType == api.OrderTypeMarket {
		return true
	}
	return req.TimeInForce == api.TIFFillAndKill || req.TimeInForce == api.TIFFillOrKill
}

func orderRequestBuyCost(req *api.OrderRequest) float64 {
	if req == nil || req.Side != api.SideBuy {
		return 0
	}
	if orderRequestIsMarketBuyAmount(req) {
		return req.Size
	}
	return req.Price * req.Size
}

func orderRequestPaperSize(req *api.OrderRequest) float64 {
	if req == nil {
		return 0
	}
	if orderRequestIsMarketBuyAmount(req) && req.Price > 0 {
		return req.Size / req.Price
	}
	return req.Size
}

func paperOrderSafetyAmount(side api.Side, price, size float64, feeRateBps int) float64 {
	amount := price * size
	if side == api.SideSell && feeRateBps > 0 {
		amount -= core.PolymarketTakerFeeUSDC(size, price, feeRateBps)
		if amount < 0 {
			return 0
		}
	}
	return amount
}

func paperRequestBuySafetyAmount(req *api.OrderRequest, feeRateBps int) float64 {
	if req == nil || req.Side != api.SideBuy {
		return 0
	}
	if req.Price <= 0 {
		return orderRequestBuyCost(req)
	}
	return paperOrderSafetyAmount(api.SideBuy, req.Price, orderRequestPaperSize(req), feeRateBps)
}

func liveOrderRequest(req *api.OrderRequest) *api.OrderRequest {
	if req == nil {
		return nil
	}
	next := *req
	next.FeeRateBps = 0
	return &next
}

// NewTrader creates the appropriate trader based on config
func NewTrader(cfg *core.Config, engine *paper.Engine, orderBook *paper.OrderBook) (Trader, error) {
	if cfg.IsPaperMode() {
		return NewPaperTrader(engine, orderBook), nil
	}
	return NewRealTrader(cfg)
}
