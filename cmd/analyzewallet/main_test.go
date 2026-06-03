package main

import (
	"bytes"
	"io"
	"os"
	"testing"
	"time"

	"Market-bot/internal/analysis"
)

func TestPrintReport(t *testing.T) {
	// Redirect stdout to capture output
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	report := analysis.WalletStrategyReport{
		Wallet:                     "0x1234567890abcdef1234567890abcdef12345678",
		TradeCount:                 10,
		BuyCount:                   6,
		SellCount:                  4,
		PositionCount:              2,
		FirstTrade:                 time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		LastTrade:                  time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC),
		TradeSpan:                  24 * time.Hour,
		ConditionCount:             3,
		PrimaryFamily:              "Test Family",
		PrimaryFamilyTradePct:      0.6,
		Strategy:                   "Test Strategy",
		Confidence:                 0.95,
		BothOutcomeConditionPct:    0.33,
		SellTradePct:               0.4,
		AvgDistinctPricesPerSide:   2.5,
		AvgOutcomeVWAPSum:          0.98,
		Evidence:                   []string{"Evidence 1", "Evidence 2"},
		Recommendations:            []string{"Recommendation 1"},
		Markets: []analysis.MarketSummary{
			{
				Slug:         "test-market",
				TradeCount:   5,
				Span:         12 * time.Hour,
				BothOutcomes: true,
				Outcomes: []analysis.OutcomeSummary{
					{
						Outcome:            "Yes",
						TradeCount:         3,
						BuyCount:           2,
						SellCount:          1,
						TotalShares:        100.0,
						VWAP:               0.55,
						PriceMin:           0.50,
						PriceMax:           0.60,
						DistinctPriceCount: 2,
					},
				},
			},
		},
	}

	printReport(report, 5)

	// Close writer and restore stdout
	w.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	output := buf.String()

	// Verify key details are present in stdout
	expectedStrings := []string{
		"Wallet:        0x1234567890abcdef1234567890abcdef12345678",
		"Trades:        10",
		"Buys / Sells:  6 / 4",
		"Open Positions:2",
		"Window:        2026-01-01T12:00:00Z -> 2026-01-02T12:00:00Z (24h0m0s)",
		"Primary Family:Test Family (60% of trades)",
		"Classification:Test Strategy (confidence 95%)",
		"Evidence 1",
		"Recommendation 1",
		"test-market",
		"Yes: trades=3 buys=2 sells=1 shares=100.00 vwap=0.550 range=0.500-0.600 priceLevels=2",
	}

	for _, expected := range expectedStrings {
		if !bytes.Contains(buf.Bytes(), []byte(expected)) {
			t.Errorf("Expected output to contain %q, but it did not.\nFull output:\n%s", expected, output)
		}
	}
}
