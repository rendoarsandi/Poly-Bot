package api

import (
	"context"
)

// ExchangeClient is an abstraction for order management and exchange interactions
type ExchangeClient interface {
	PlaceOrder(ctx context.Context, req *OrderRequest) (*OrderResponse, error)
	CancelOrder(ctx context.Context, orderID string) error
	CancelAllOrders(ctx context.Context) error
	GetPositions(ctx context.Context) ([]Position, error)
	GetOrder(ctx context.Context, orderID string) (*OpenOrder, error)
	GetOpenOrders(ctx context.Context) ([]OpenOrder, error)

	GetBalanceAllowance(ctx context.Context) (*BalanceAllowance, error)
	UpdateBalanceAllowance(ctx context.Context) error
	GetMarketInfo(ctx context.Context, conditionID string) (*MarketInfo, error)

	SetTestMode(enabled bool)
	IsTestMode() bool

	GetSigner() *Signer
	Address() string // Used primarily for Polygon balances (Polymarket only)

	EnableRawAPILog(path string) error
	CloseRawAPILog() error
}

// Ensure CLOBClient implements ExchangeClient
var _ ExchangeClient = (*CLOBClient)(nil)
