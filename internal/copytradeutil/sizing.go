package copytradeutil

import (
	"strings"

	"Market-bot/internal/core"
)

type SizingConfig struct {
	SizeUSDC     float64
	SizeShares   float64
	SizePercent  float64
	MaxTradeSize float64
	SizingMode   string
}

type SellSizingPlan struct {
	RequestedQty   float64
	TargetQty      float64
	TargetDelta    float64
	PositionSignal bool
}

func BuyRequestedQty(targetShares, price float64, cfg SizingConfig) float64 {
	return core.CalculateCopytradeSharesForMode(
		targetShares,
		price,
		cfg.SizeUSDC,
		cfg.SizeShares,
		cfg.SizePercent,
		cfg.MaxTradeSize,
		cfg.SizingMode,
	)
}

func BuildSellSizingPlan(state *RuntimeState, outcome string, localQty, tradeSize, price float64, source string, targetDeltas map[string]float64, cfg SizingConfig) SellSizingPlan {
	plan := SellSizingPlan{
		TargetDelta:    -tradeSize,
		PositionSignal: strings.HasPrefix(strings.ToLower(strings.TrimSpace(source)), "position"),
	}

	if state != nil && state.TargetSeen[outcome] {
		plan.TargetQty = state.TargetShares[outcome]
		if !plan.PositionSignal {
			if delta, ok := targetDeltas[outcome]; ok && delta < -0.01 {
				plan.TargetDelta = delta
				delete(targetDeltas, outcome)
			}
		}
		plan.RequestedQty = core.CalculateCopytradeSellSharesForMode(
			localQty,
			plan.TargetQty,
			plan.TargetDelta,
			price,
			cfg.SizeUSDC,
			cfg.SizeShares,
			cfg.SizePercent,
			cfg.MaxTradeSize,
			cfg.SizingMode,
		)
	} else {
		plan.RequestedQty = core.CalculateCopytradeSharesForMode(
			tradeSize,
			price,
			cfg.SizeUSDC,
			cfg.SizeShares,
			cfg.SizePercent,
			cfg.MaxTradeSize,
			cfg.SizingMode,
		)
	}

	if plan.RequestedQty > localQty {
		plan.RequestedQty = localQty
	}
	return plan
}
