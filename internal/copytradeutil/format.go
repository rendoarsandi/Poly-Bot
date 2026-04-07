package copytradeutil

import (
	"fmt"
	"math"
	"strings"

	"Market-bot/internal/api"
)

func ShortTxHash(txHash string) string {
	txHash = strings.TrimSpace(txHash)
	if len(txHash) > 10 {
		return txHash[:10] + "..."
	}
	return txHash
}

func SignalSummary(trade api.PublicTrade, formatQty func(float64) string) string {
	side := strings.ToUpper(strings.TrimSpace(trade.Side))
	if side == "" {
		side = "?"
	}
	outcome := NormalizeOutcome(trade.Outcome)
	if outcome == "" {
		outcome = "?"
	}
	if formatQty == nil {
		formatQty = func(qty float64) string {
			return fmt.Sprintf("%.5f", qty)
		}
	}
	parts := []string{
		fmt.Sprintf("%s %s", side, outcome),
		fmt.Sprintf("master=%s", formatQty(math.Max(0, trade.Size))),
		fmt.Sprintf("source=%s", SignalSourceLabel(trade)),
	}
	if txHash := ShortTxHash(trade.TransactionHash); txHash != "" {
		parts = append(parts, "tx="+txHash)
	}
	return strings.Join(parts, " | ")
}
