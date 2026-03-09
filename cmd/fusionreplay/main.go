package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"

	"Market-bot/internal/fusion"
)

func main() {
	input := flag.String("input", "", "path to fusionbot JSONL snapshot recording")
	jsonOut := flag.Bool("json", false, "print full JSON report")
	flag.Parse()
	if *input == "" {
		log.Fatal("-input is required")
	}
	snapshots, err := fusion.LoadReplaySnapshots(*input)
	if err != nil {
		log.Fatalf("load replay snapshots: %v", err)
	}
	report := fusion.EvaluateReplay(nil, snapshots)
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			log.Fatalf("encode report: %v", err)
		}
		return
	}
	fmt.Printf("snapshots=%d completed_trades=%d realized_pnl=%.2f final_equity=%.2f max_drawdown=%.2f%% win_rate=%.1f%%\n",
		report.Snapshots,
		report.CompletedTrades,
		report.RealizedPnL,
		report.FinalEquity,
		report.MaxDrawdownPct,
		report.WinRate*100,
	)
	assets := make([]string, 0, len(report.ByAsset))
	for asset := range report.ByAsset {
		assets = append(assets, asset)
	}
	sort.Strings(assets)
	for _, asset := range assets {
		item := report.ByAsset[asset]
		fmt.Printf("  %s trades=%d pnl=%.2f win_rate=%.1f%%\n", asset, item.Trades, item.RealizedPnL, item.WinRate*100)
	}
}
