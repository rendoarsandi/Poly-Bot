package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"Market-bot/internal/analysis"
	"Market-bot/internal/api"
)

func main() {
	var (
		tradeLimit     = flag.Int("trade-limit", 1000, "maximum public trades to fetch")
		positionLimit  = flag.Int("position-limit", 100, "maximum public positions to fetch")
		sizeThreshold  = flag.Float64("position-threshold", 0.01, "minimum public position size to include")
		topMarkets     = flag.Int("top", 6, "number of markets to print")
		requestTimeout = flag.Duration("timeout", 20*time.Second, "request timeout")
	)
	flag.Parse()

	if flag.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "usage: go run ./cmd/analyzewallet -- [flags] <wallet>\n")
		flag.PrintDefaults()
		os.Exit(2)
	}

	target := strings.TrimSpace(flag.Arg(0))
	client := api.NewRestClient("polymarket")

	ctx, cancel := context.WithTimeout(context.Background(), *requestTimeout)
	defer cancel()

	var wallet string
	var resolveErr error
	if api.IsWalletAddress(api.NormalizeWalletAddress(target)) {
		wallet = api.NormalizeWalletAddress(target)
	} else {
		resolvedWallet, _, err := client.ResolvePublicProfileTarget(ctx, target)
		if err != nil {
			resolveErr = err
		} else {
			wallet = resolvedWallet
		}
	}

	if wallet == "" {
		fmt.Fprintf(os.Stderr, "failed to resolve wallet address for target %q: %v\n", target, resolveErr)
		os.Exit(1)
	}

	snapshot := client.GetPublicActivitySnapshot(ctx, wallet, nil, *tradeLimit, *sizeThreshold, *positionLimit)
	if snapshot.TradesErr != nil {
		fmt.Fprintf(os.Stderr, "trade fetch failed: %v\n", snapshot.TradesErr)
		os.Exit(1)
	}
	if snapshot.PositionsErr != nil {
		fmt.Fprintf(os.Stderr, "warning: position fetch failed: %v (continuing with trade history analysis)\n", snapshot.PositionsErr)
	}

	report := analysis.AnalyzePublicWallet(wallet, snapshot.Trades, snapshot.Positions)
	printReport(report, *topMarkets)
}

func printReport(report analysis.WalletStrategyReport, topMarkets int) {
	fmt.Printf("Wallet:        %s\n", report.Wallet)
	fmt.Printf("Trades:        %d\n", report.TradeCount)
	fmt.Printf("Buys / Sells:  %d / %d\n", report.BuyCount, report.SellCount)
	fmt.Printf("Open Positions:%d\n", report.PositionCount)
	if !report.FirstTrade.IsZero() {
		fmt.Printf("Window:        %s -> %s (%s)\n",
			report.FirstTrade.Format(time.RFC3339),
			report.LastTrade.Format(time.RFC3339),
			report.TradeSpan.Round(time.Second),
		)
	}
	fmt.Printf("Markets:       %d condition(s)\n", report.ConditionCount)
	fmt.Printf("Primary Family:%s (%.0f%% of trades)\n", report.PrimaryFamily, report.PrimaryFamilyTradePct*100)
	fmt.Printf("Classification:%s (confidence %.0f%%)\n", report.Strategy, report.Confidence*100)
	fmt.Printf("Both Outcomes: %.0f%% of conditions\n", report.BothOutcomeConditionPct*100)
	fmt.Printf("Sell Rate:     %.0f%% of trades\n", report.SellTradePct*100)
	fmt.Printf("Ladder Depth:  %.1f distinct prices per outcome\n", report.AvgDistinctPricesPerSide)
	fmt.Printf("VWAP Sum:      %.3f average outcome-VWAP sum per condition\n", report.AvgOutcomeVWAPSum)

	if len(report.Evidence) > 0 {
		fmt.Printf("\nEvidence:\n")
		for _, line := range report.Evidence {
			fmt.Printf("- %s\n", line)
		}
	}

	if len(report.Recommendations) > 0 {
		fmt.Printf("\nBot Fit:\n")
		for _, line := range report.Recommendations {
			fmt.Printf("- %s\n", line)
		}
	}

	if topMarkets <= 0 || topMarkets > len(report.Markets) {
		topMarkets = len(report.Markets)
	}
	if topMarkets == 0 {
		return
	}

	fmt.Printf("\nTop Markets:\n")
	for i := 0; i < topMarkets; i++ {
		market := report.Markets[i]
		fmt.Printf("- %s | %d trades | span %s | bothOutcomes=%t\n", market.Slug, market.TradeCount, market.Span.Round(time.Second), market.BothOutcomes)
		for _, outcome := range market.Outcomes {
			fmt.Printf("  %s: trades=%d buys=%d sells=%d shares=%.2f vwap=%.3f range=%.3f-%.3f priceLevels=%d\n",
				outcome.Outcome,
				outcome.TradeCount,
				outcome.BuyCount,
				outcome.SellCount,
				outcome.TotalShares,
				outcome.VWAP,
				outcome.PriceMin,
				outcome.PriceMax,
				outcome.DistinctPriceCount,
			)
		}
	}
}
