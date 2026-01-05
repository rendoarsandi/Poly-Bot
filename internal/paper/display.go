package paper

import (
	"time"
)

// Display formats and prints trading stats to console
type Display struct {
	engine    *Engine
	lastPrint time.Time
	interval  time.Duration
}

// NewDisplay creates a new display formatter
func NewDisplay(engine *Engine, interval time.Duration) *Display {
	return &Display{
		engine:   engine,
		interval: interval,
	}
}

// ShouldPrint returns true if enough time has passed since last print
func (d *Display) ShouldPrint() bool {
	return time.Since(d.lastPrint) >= d.interval
}

// PrintStats prints formatted trading statistics
func (d *Display) PrintStats() {
	/*
		stats := d.engine.GetStats()
		positions := d.engine.GetPositions()
		totalExposure, _ := d.engine.GetExposure()
		totalEquity := d.engine.GetEquity()

		d.lastPrint = time.Now()

		// Header
		fmt.Println()
		fmt.Println(strings.Repeat("═", 50))
		fmt.Printf("  📊 PAPER TRADING STATS [%s]\n", time.Now().Format("15:04:05"))
		fmt.Println(strings.Repeat("─", 50))

		// Account Overview - Clear breakdown
		fmt.Println("  💼 ACCOUNT OVERVIEW:")
		fmt.Printf("     ├─ 💵 Cash:        $%.2f\n", stats.CurrentBalance)
		fmt.Printf("     ├─ 📦 In Positions: $%.2f\n", totalExposure)
		fmt.Printf("     └─ 💰 Total Equity: $%.2f\n", totalEquity)

		// Show net change from starting
		netChange := totalEquity - stats.StartingBalance
		changeSymbol := "📈"
		changeSign := "+"
		if netChange < 0 {
			changeSymbol = "📉"
			changeSign = ""
		}
		fmt.Printf("  %s Net Change:   %s$%.2f (from $%.2f start)\n",
			changeSymbol, changeSign, netChange, stats.StartingBalance)

		fmt.Println(strings.Repeat("─", 50))

		// PnL Breakdown
		totalPnL := stats.RealizedPnL + stats.UnrealizedPnL
		pnlSign := "+"
		if totalPnL < 0 {
			pnlSign = ""
		}
		fmt.Println("  📊 PROFIT/LOSS:")
		fmt.Printf("     ├─ Realized:   $%.2f (closed trades)\n", stats.RealizedPnL)
		fmt.Printf("     ├─ Unrealized: $%.2f (open positions)\n", stats.UnrealizedPnL)
		fmt.Printf("     └─ Total PnL:  %s$%.2f\n", pnlSign, totalPnL)

		fmt.Println(strings.Repeat("─", 50))

		// Trade Stats
		winRate := 0.0
		if stats.WinningTrades+stats.LosingTrades > 0 {
			winRate = float64(stats.WinningTrades) / float64(stats.WinningTrades+stats.LosingTrades) * 100
		}

		fmt.Printf("  📈 Total Trades: %d\n", stats.TotalTrades)
		fmt.Printf("     ├─ Wins:  %d\n", stats.WinningTrades)
		fmt.Printf("     └─ Losses: %d\n", stats.LosingTrades)
		fmt.Printf("  🎯 Win Rate:     %.1f%%\n", winRate)

		fmt.Println(strings.Repeat("─", 50))

		// Risk Metrics
		fmt.Printf("  ⚠️  Max Drawdown: %.2f%%\n", stats.MaxDrawdown)
		fmt.Printf("  📊 Exposure:      $%.2f\n", totalExposure)

		// Positions
		if len(positions) > 0 {
			fmt.Println(strings.Repeat("─", 50))
			fmt.Println("  📦 Open Positions:")
			for outcome, pos := range positions {
				unrealizedPnL := 0.0
				if price, ok := d.engine.currentPrices[outcome]; ok {
					unrealizedPnL = (price * pos.Quantity) - pos.TotalCost
				}
				pnlStr := fmt.Sprintf("$%.2f", unrealizedPnL)
				if unrealizedPnL >= 0 {
					pnlStr = "+" + pnlStr
				}
				fmt.Printf("     • %s: %.0f shares @ $%.3f avg (PnL: %s)\n",
					outcome, pos.Quantity, pos.AvgPrice, pnlStr)
			}
		}

		fmt.Println(strings.Repeat("═", 50))
		fmt.Println()
	*/
}

// PrintTrade prints a single trade execution
func (d *Display) PrintTrade(trade *Trade) {
	/*
		symbol := "🟢"
		action := "BUY"
		if trade.Side == "sell" {
			symbol = "🔴"
			action = "SELL"
		}

		fmt.Printf("%s [%s] %s %s: %.0f shares @ $%.3f = $%.2f\n",
			symbol,
			trade.Timestamp.Format("15:04:05"),
			action,
			trade.Outcome,
			trade.Quantity,
			trade.Price,
			trade.Value)
	*/
}

// PrintOpportunity prints when an arbitrage opportunity is detected
func (d *Display) PrintOpportunity(sum float64, prices map[string]string) {
	/*
		margin := (1.0 - sum) * 100
		fmt.Printf("🔥 OPPORTUNITY: Sum=%.4f (%.2f%% margin) | Prices: %v\n", sum, margin, prices)
	*/
}

// PrintHeader prints startup header
func PrintHeader(startingBalance float64) {
	/*
		fmt.Println()
		fmt.Println(strings.Repeat("═", 50))
		fmt.Println("  🎰 POLYARB-15M PAPER TRADING BOT")
		fmt.Println(strings.Repeat("─", 50))
		fmt.Printf("  💵 Starting Balance: $%.2f\n", startingBalance)
		fmt.Println("  📝 Mode: PAPER TRADING (no real money)")
		fmt.Println(strings.Repeat("═", 50))
		fmt.Println()
	*/
}
