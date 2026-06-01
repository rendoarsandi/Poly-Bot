package main

import (
	"math"
	"strings"
	"testing"

	"Market-bot/internal/api"
	"Market-bot/internal/trading"
)

func TestDirectExecutionTxSummaryIncludesAllReturnedHashes(t *testing.T) {
	exec := directMarketExecution{
		Result: &trading.TradeResult{
			TransactionsHashes: []string{
				"0x9071f607f4c2ae4b7b4a4849ca1052b7798011540fcb3759536368225a1a186c",
				"0x1056093066fcc6225983d769b6951bbf0c72f15a7af21ffa5f8c893395722474",
			},
		},
	}

	summary := directExecutionTxSummary(exec)
	if !strings.Contains(summary, "2 txs [") {
		t.Fatalf("expected multi-tx count in summary, got %q", summary)
	}
	if !strings.Contains(summary, "0x9071f607f4...") {
		t.Fatalf("expected first tx hash in summary, got %q", summary)
	}
	if !strings.Contains(summary, "0x1056093066...") {
		t.Fatalf("expected second tx hash in summary, got %q", summary)
	}
}

func TestBuildDirectMarketOrderRequestUsesFAKLimitShape(t *testing.T) {
	req := buildDirectMarketOrderRequest(directMarketOrderSignalRequest{
		Side:       api.SideBuy,
		TokenID:    "token-up",
		Outcome:    "Up",
		Price:      0.47,
		Size:       12.5,
		FeeRateBps: 85,
	})

	if req.TokenID != "token-up" || req.Price != 0.47 || req.Size != 12.5 {
		t.Fatalf("unexpected request payload: %+v", req)
	}
	if req.Side != api.SideBuy {
		t.Fatalf("expected buy side, got %s", req.Side)
	}
	if req.OrderType != api.OrderTypeLimit {
		t.Fatalf("expected limit order type, got %s", req.OrderType)
	}
	if req.TimeInForce != api.TIFFillAndKill {
		t.Fatalf("expected FAK time-in-force, got %s", req.TimeInForce)
	}
	if req.FeeRateBps != 85 {
		t.Fatalf("expected fee rate 85, got %d", req.FeeRateBps)
	}
}

func TestHydrateDirectMarketTradeResultBackfillsBatchMetadata(t *testing.T) {
	result := hydrateDirectMarketTradeResult(directMarketOrderSignalRequest{
		Side:       api.SideSell,
		TokenID:    "token-down",
		Outcome:    "Down",
		Price:      0.58,
		Size:       9,
		FeeRateBps: 100,
	}, &trading.TradeResult{OrderID: "ord-123", Status: "matched"})

	if result.OrderID != "ord-123" || result.Status != "matched" {
		t.Fatalf("expected existing venue fields to survive, got %+v", result)
	}
	if result.Side != string(api.SideSell) || result.TokenID != "token-down" || result.Outcome != "Down" {
		t.Fatalf("expected metadata to be backfilled, got %+v", result)
	}
	if result.Price != 0.58 || result.Size != 9 {
		t.Fatalf("expected request price/size on hydrated result, got %+v", result)
	}
	if result.FeeRateBps != 100 {
		t.Fatalf("expected fee rate to be preserved, got %+v", result)
	}
	if result.Timestamp.IsZero() {
		t.Fatal("expected hydrateDirectMarketTradeResult to stamp timestamp")
	}
}

func TestDirectExecutionHasSizingDriftFlagsVenueOverfill(t *testing.T) {
	exec := directMarketExecution{AcknowledgedQty: 7.885631, AcknowledgedNotional: 2.15}
	if !directExecutionHasSizingDrift(exec, 5.0) {
		t.Fatal("expected acknowledged overfill to be flagged as sizing drift")
	}
	if got := venueExecutionEffectivePrice(exec); math.Abs(got-0.272648) > 0.00001 {
		t.Fatalf("unexpected effective price %.6f", got)
	}
}

func TestDirectExecutionHasSizingDriftIgnoresTinyRoundingNoise(t *testing.T) {
	exec := directMarketExecution{AcknowledgedQty: 5.0099, AcknowledgedNotional: 2.15}
	if directExecutionHasSizingDrift(exec, 5.0) {
		t.Fatal("expected tiny acknowledgment noise to be ignored")
	}
}

func TestDirectExecutionHasSizingDriftIgnoresPaperFeeNetInventory(t *testing.T) {
	exec := directMarketExecution{AcknowledgedQty: 1.02, AcknowledgedNotional: 0.5049}
	if directExecutionHasSizingDrift(exec, 1.02) {
		t.Fatal("expected gross paper acknowledgement to avoid sizing drift")
	}
}

func TestBuildDirectMarketOrderRequestBuyExactSharesUsesGTC(t *testing.T) {
	req := buildDirectMarketOrderRequest(directMarketOrderSignalRequest{
		Side:        api.SideBuy,
		TokenID:     "token-1",
		Price:       0.44,
		Size:        1.02,
		FeeRateBps:  1000,
		ExactShares: true,
	})

	if req.TimeInForce != api.TIFGoodTilCancelled {
		t.Fatalf("expected GTC for exact-share buy, got %q", req.TimeInForce)
	}
	if req.Size != 1.02 {
		t.Fatalf("expected buy size to remain shares, got %.4f", req.Size)
	}
}

func TestDirectOrderNotional(t *testing.T) {
	if got := directOrderNotional(0.95, 1.02); math.Abs(got-0.969) > 0.000001 {
		t.Fatalf("expected direct order notional 0.969, got %.6f", got)
	}
}

func TestHasActionableDirectOrderValueRequiresOneDollarMinimum(t *testing.T) {
	if hasActionableDirectOrderValue(0.95, 1.02) {
		t.Fatal("expected sub-$1 direct order value to be rejected")
	}
	if !hasActionableDirectOrderValue(0.99, 1.02) {
		t.Fatal("expected >=$1 direct order value to pass")
	}
}

func TestHasActionableSubmittedDirectOrderValueRejectsShareSizedBuyBelowOneDollar(t *testing.T) {
	req := directMarketOrderSignalRequest{
		Side:        api.SideBuy,
		Price:       0.90,
		Size:        1.10,
		ExactShares: true,
	}
	if got := directSubmittedOrderValue(req); math.Abs(got-0.99) > 0.000001 {
		t.Fatalf("expected encoded buy amount 0.99, got %.4f", got)
	}
	if hasActionableSubmittedDirectOrderValue(req) {
		t.Fatal("expected encoded buy amount below $1 to be rejected")
	}
}

func TestBuildDirectMarketOrderRequestSellKeepsFAK(t *testing.T) {
	req := buildDirectMarketOrderRequest(directMarketOrderSignalRequest{
		Side:       api.SideSell,
		TokenID:    "token-1",
		Price:      0.44,
		Size:       1.02,
		FeeRateBps: 1000,
	})

	if req.TimeInForce != api.TIFFillAndKill {
		t.Fatalf("expected sell to keep FAK, got %q", req.TimeInForce)
	}
}

func TestShouldCancelResidualBuyOrderRespectsNoCancel(t *testing.T) {
	req := directMarketOrderSignalRequest{
		Side:        api.SideBuy,
		TokenID:     "token-1",
		Price:       0.44,
		Size:        1.00,
		ExactShares: true,
		NoCancel:    true,
	}

	if shouldCancelResidualBuyOrder(req, 0.50) {
		t.Fatal("expected shouldCancelResidualBuyOrder to return false when NoCancel is true")
	}

	req.NoCancel = false
	if !shouldCancelResidualBuyOrder(req, 0.50) {
		t.Fatal("expected shouldCancelResidualBuyOrder to return true when NoCancel is false and executedQty is less than requested size")
	}
}
