package copytradeutil

import (
	"math"
	"strings"

	"Market-bot/internal/api"
)

type Signal struct {
	Outcome        string
	Side           string
	Size           float64
	PositionSignal bool
}

func ParseSignal(trade api.PublicTrade) Signal {
	return Signal{
		Outcome:        NormalizeOutcome(trade.Outcome),
		Side:           strings.ToUpper(strings.TrimSpace(trade.Side)),
		Size:           math.Max(0, trade.Size),
		PositionSignal: strings.HasPrefix(strings.ToLower(strings.TrimSpace(trade.Source)), "position"),
	}
}

func (s Signal) IsBuy() bool {
	return s.Side == "BUY"
}

func (s Signal) IsSell() bool {
	return s.Side == "SELL"
}

func (s Signal) SupportedSide() bool {
	return s.IsBuy() || s.IsSell()
}

func (s Signal) BelowMin(minSize float64, allowBelowMin bool) bool {
	if allowBelowMin {
		return false
	}
	return s.Size <= minSize
}

func BuildRetrySellTrade(trade api.PublicTrade, outcome string, remainingSize float64) api.PublicTrade {
	return api.PublicTrade{
		ConditionID: strings.TrimSpace(trade.ConditionID),
		Outcome:     outcome,
		Side:        "SELL",
		Size:        remainingSize,
		Timestamp:   trade.Timestamp,
		Source:      trade.Source,
	}
}
