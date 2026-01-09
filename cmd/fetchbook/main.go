package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/paper"
)

// Level represents a single price level in the order book
type Level struct {
	Price float64
	Size  float64
}

// LiquidityReport shows liquidity available at different profit margins
type LiquidityReport struct {
	Margin          float64 // Profit margin percentage (e.g., 2.0, 4.0, 6.0)
	MaxSum          float64 // Maximum price sum for this margin (1.0 - margin/100)
	MatchedLiq      float64 // Matched liquidity (min of both sides after pairing)
	RawLiq1         float64 // Raw liquidity on side 1 at valid levels
	RawLiq2         float64 // Raw liquidity on side 2 at valid levels
	ValidLevels1    int     // Number of valid price levels on side 1
	ValidLevels2    int     // Number of valid price levels on side 2
	AvgPrice1       float64 // Weighted average price on side 1
	AvgPrice2       float64 // Weighted average price on side 2
	ExpectedProfit  float64 // Expected profit from matched liquidity
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client := api.NewRestClient("")

	fmt.Println("=" + strings.Repeat("=", 79))
	fmt.Println(" FETCHBOOK - Liquidity Depth Analyzer")
	fmt.Println(" Verifies bot sees correct liquidity at each profit margin level")
	fmt.Println("=" + strings.Repeat("=", 79))
	fmt.Println()

	// Get active 15-minute markets
	fmt.Println("Fetching active 15-minute markets...")
	assets := []string{"BTC", "ETH", "SOL"}
	markets, err := client.Get15mMarkets(ctx, assets)
	if err != nil {
		fmt.Printf("Error fetching 15m markets: %v\n", err)
	}

	fmt.Printf("Found %d active 15-minute markets\n\n", len(markets))

	if len(markets) == 0 {
		fmt.Println("No 15m markets available. Looking for crypto markets...")
		allMarkets, err := client.ListMarkets(ctx)
		if err != nil {
			fmt.Printf("Error listing markets: %v\n", err)
			return
		}
		// Look for any crypto binary market with active order books
		cryptoKeywords := []string{"btc", "eth", "sol", "bitcoin", "ethereum", "crypto"}
		for i := range allMarkets {
			if len(allMarkets[i].Tokens) >= 2 && allMarkets[i].Active {
				slug := strings.ToLower(allMarkets[i].Slug)
				isCrypto := false
				for _, kw := range cryptoKeywords {
					if strings.Contains(slug, kw) {
						isCrypto = true
						break
					}
				}
				if isCrypto {
					markets = append(markets, allMarkets[i])
					if len(markets) >= 3 {
						break
					}
				}
			}
		}
		// If still no crypto markets, take any binary market
		if len(markets) == 0 {
			for i := range allMarkets {
				if len(allMarkets[i].Tokens) == 2 && allMarkets[i].Active {
					markets = append(markets, allMarkets[i])
					if len(markets) >= 1 {
						break
					}
				}
			}
		}
	}

	if len(markets) == 0 {
		fmt.Println("No markets with tokens found")
		return
	}

	// Analyze each market
	for _, market := range markets {
		analyzeMarket(ctx, client, &market)
	}
}

func analyzeMarket(ctx context.Context, client *api.RestClient, market *api.Market) {
	fmt.Printf("\n%s\n", strings.Repeat("-", 80))
	fmt.Printf("MARKET: %s\n", market.Slug)

	// Parse end time
	if endTime, err := paper.ParseEndTimeFromSlug(market.Slug); err == nil {
		remaining := time.Until(endTime)
		if remaining < 0 {
			fmt.Printf("STATUS: EXPIRED (%v ago)\n", -remaining.Round(time.Second))
		} else {
			fmt.Printf("STATUS: ACTIVE (expires in %v)\n", remaining.Round(time.Second))
		}
	}
	fmt.Println(strings.Repeat("-", 80))

	if len(market.Tokens) < 2 {
		fmt.Println("ERROR: Market has less than 2 tokens")
		return
	}

	// Fetch order books for both outcomes
	var allAsks [2][]Level
	var outcomes [2]string
	var bookDepth [2]int

	for idx := 0; idx < 2; idx++ {
		token := market.Tokens[idx]
		outcomes[idx] = token.Outcome

		book, err := client.GetOrderBook(ctx, token.TokenID)
		if err != nil {
			fmt.Printf("  ERROR fetching %s: %v\n", token.Outcome, err)
			continue
		}

		bookDepth[idx] = len(book.Asks)

		for _, a := range book.Asks {
			p, _ := strconv.ParseFloat(a.Price, 64)
			s, _ := strconv.ParseFloat(a.Size, 64)
			allAsks[idx] = append(allAsks[idx], Level{Price: p, Size: s})
		}
		sort.Slice(allAsks[idx], func(i, j int) bool { return allAsks[idx][i].Price < allAsks[idx][j].Price })
	}

	if len(allAsks[0]) == 0 || len(allAsks[1]) == 0 {
		fmt.Println("ERROR: One or both books are empty")
		return
	}

	// Show top of book
	fmt.Printf("\n  %s: Best Ask $%.3f (%d levels total)\n", outcomes[0], allAsks[0][0].Price, bookDepth[0])
	fmt.Printf("  %s: Best Ask $%.3f (%d levels total)\n", outcomes[1], allAsks[1][0].Price, bookDepth[1])

	topSum := allAsks[0][0].Price + allAsks[1][0].Price
	topMargin := (1.0 - topSum) * 100
	fmt.Printf("\n  TOP OF BOOK: $%.3f + $%.3f = $%.3f (%.1f%% margin)\n",
		allAsks[0][0].Price, allAsks[1][0].Price, topSum, topMargin)

	// Show first 5 levels of each side to see price structure
	fmt.Println("\n  ORDER BOOK STRUCTURE (first 5 levels):")
	fmt.Println("  " + strings.Repeat("-", 60))
	maxLevels := 5
	if len(allAsks[0]) < maxLevels {
		maxLevels = len(allAsks[0])
	}
	if len(allAsks[1]) < maxLevels {
		maxLevels = len(allAsks[1])
	}
	fmt.Printf("  %-6s | %-20s | %-20s\n", "Level", outcomes[0], outcomes[1])
	fmt.Println("  " + strings.Repeat("-", 60))
	for lvl := 0; lvl < maxLevels; lvl++ {
		p1, s1 := allAsks[0][lvl].Price, allAsks[0][lvl].Size
		p2, s2 := allAsks[1][lvl].Price, allAsks[1][lvl].Size
		sum := p1 + p2
		margin := (1.0 - sum) * 100
		marginStr := fmt.Sprintf("%.1f%%", margin)
		if margin < 2.0 {
			marginStr = fmt.Sprintf("%.1f%% ✗", margin)
		}
		fmt.Printf("  [%d]    | $%.3f x %-6.0f     | $%.3f x %-6.0f   → sum $%.3f (%s)\n",
			lvl, p1, s1, p2, s2, sum, marginStr)
	}

	// Calculate liquidity at different margin thresholds (1% steps up to 6%, then 8%, 10%)
	marginThresholds := []float64{1.0, 2.0, 3.0, 4.0, 5.0, 6.0, 7.0, 8.0, 10.0}

	fmt.Println()
	fmt.Println("  " + strings.Repeat("=", 76))
	fmt.Printf("  %-8s | %-10s | %-15s | %-12s | %-20s\n",
		"MARGIN", "MAX SUM", "MATCHED LIQ", "RAW LIQ", "DEPTH (total→valid)")
	fmt.Println("  " + strings.Repeat("-", 76))

	for _, marginPct := range marginThresholds {
		report := calculateLiquidity(allAsks[0], allAsks[1], marginPct)

		// Format the output
		if report.MatchedLiq > 0 {
			fmt.Printf("  %5.1f%%  | $%.2f      | %8.0f shares | %4.0f / %-4.0f | %d/%d → %d/%d\n",
				marginPct, report.MaxSum, report.MatchedLiq,
				report.RawLiq1, report.RawLiq2,
				bookDepth[0], bookDepth[1], report.ValidLevels1, report.ValidLevels2)
		} else {
			fmt.Printf("  %5.1f%%  | $%.2f      | %8s       | %4s / %-4s | %d/%d → %d/%d\n",
				marginPct, report.MaxSum, "-", "-", "-",
				bookDepth[0], bookDepth[1], 0, 0)
		}
	}
	fmt.Println("  " + strings.Repeat("=", 76))

	// Show detailed breakdown for the 2% margin (bot's default)
	fmt.Println("\n  DETAILED BREAKDOWN (2% margin - bot default):")
	fmt.Println("  " + strings.Repeat("-", 60))
	showDetailedMatching(allAsks[0], allAsks[1], outcomes, 2.0)

	// If there's good liquidity at 6%+, highlight it
	report6 := calculateLiquidity(allAsks[0], allAsks[1], 6.0)
	if report6.MatchedLiq > 0 {
		fmt.Println("\n  DETAILED BREAKDOWN (6% margin - high profit):")
		fmt.Println("  " + strings.Repeat("-", 60))
		showDetailedMatching(allAsks[0], allAsks[1], outcomes, 6.0)

		expectedProfit := report6.MatchedLiq * 0.06 // 6% on matched liquidity
		fmt.Printf("\n  PROFIT OPPORTUNITY: %.0f shares at 6%% = $%.2f potential profit\n",
			report6.MatchedLiq, expectedProfit)
	}
}

func calculateLiquidity(asks1, asks2 []Level, marginPct float64) LiquidityReport {
	maxSum := 1.0 - (marginPct / 100.0)

	// Make copies since we modify sizes
	a1 := make([]Level, len(asks1))
	a2 := make([]Level, len(asks2))
	copy(a1, asks1)
	copy(a2, asks2)

	var totalMatched, rawLiq1, rawLiq2 float64
	var maxValidI, maxValidJ int

	i, j := 0, 0
	for i < len(a1) && j < len(a2) {
		p1 := a1[i].Price
		p2 := a2[j].Price

		if p1+p2 > maxSum {
			break
		}

		// Track deepest valid level on each side
		if i+1 > maxValidI {
			maxValidI = i + 1
			rawLiq1 += asks1[i].Size // Use original size for raw liquidity
		}
		if j+1 > maxValidJ {
			maxValidJ = j + 1
			rawLiq2 += asks2[j].Size // Use original size for raw liquidity
		}

		// Match liquidity
		liq1 := a1[i].Size
		liq2 := a2[j].Size
		matched := liq1
		if liq2 < matched {
			matched = liq2
		}
		totalMatched += matched

		// Move pointers
		rem1 := liq1 - matched
		rem2 := liq2 - matched
		if rem1 <= 0 {
			i++
		} else {
			a1[i].Size = rem1
		}
		if rem2 <= 0 {
			j++
		} else {
			a2[j].Size = rem2
		}
	}

	return LiquidityReport{
		Margin:       marginPct,
		MaxSum:       maxSum,
		MatchedLiq:   totalMatched,
		RawLiq1:      rawLiq1,
		RawLiq2:      rawLiq2,
		ValidLevels1: maxValidI,
		ValidLevels2: maxValidJ,
	}
}

func showDetailedMatching(asks1, asks2 []Level, outcomes [2]string, marginPct float64) {
	maxSum := 1.0 - (marginPct / 100.0)

	// Make copies
	a1 := make([]Level, len(asks1))
	a2 := make([]Level, len(asks2))
	copy(a1, asks1)
	copy(a2, asks2)

	i, j := 0, 0
	iteration := 0
	var totalMatched float64

	for i < len(a1) && j < len(a2) {
		p1 := a1[i].Price
		p2 := a2[j].Price
		sum := p1 + p2
		actualMargin := (1.0 - sum) * 100

		if sum > maxSum {
			fmt.Printf("    [STOP] $%.3f + $%.3f = $%.3f (%.1f%%) > max $%.2f\n",
				p1, p2, sum, actualMargin, maxSum)
			break
		}

		liq1 := a1[i].Size
		liq2 := a2[j].Size
		matched := liq1
		if liq2 < matched {
			matched = liq2
		}
		totalMatched += matched

		fmt.Printf("    [%d] %s[%d] $%.3f + %s[%d] $%.3f = $%.3f (%.1f%%) | match %.0f of %.0f/%.0f\n",
			iteration, outcomes[0], i, p1, outcomes[1], j, p2, sum, actualMargin, matched, liq1, liq2)

		// Move pointers
		rem1 := liq1 - matched
		rem2 := liq2 - matched
		if rem1 <= 0 {
			i++
		} else {
			a1[i].Size = rem1
		}
		if rem2 <= 0 {
			j++
		} else {
			a2[j].Size = rem2
		}
		iteration++

		if iteration >= 10 {
			fmt.Printf("    ... (truncated, %d more iterations)\n", countRemainingIterations(a1[i:], a2[j:], maxSum))
			break
		}
	}

	if totalMatched > 0 {
		fmt.Printf("    TOTAL MATCHED: %.0f shares\n", totalMatched)
	}
}

func countRemainingIterations(asks1, asks2 []Level, maxSum float64) int {
	if len(asks1) == 0 || len(asks2) == 0 {
		return 0
	}

	a1 := make([]Level, len(asks1))
	a2 := make([]Level, len(asks2))
	copy(a1, asks1)
	copy(a2, asks2)

	count := 0
	i, j := 0, 0
	for i < len(a1) && j < len(a2) {
		if a1[i].Price+a2[j].Price > maxSum {
			break
		}
		matched := a1[i].Size
		if a2[j].Size < matched {
			matched = a2[j].Size
		}
		rem1 := a1[i].Size - matched
		rem2 := a2[j].Size - matched
		if rem1 <= 0 {
			i++
		} else {
			a1[i].Size = rem1
		}
		if rem2 <= 0 {
			j++
		} else {
			a2[j].Size = rem2
		}
		count++
	}
	return count
}

func init() {
	// Show usage if help requested
	for _, arg := range os.Args[1:] {
		if arg == "-h" || arg == "--help" {
			fmt.Println("Usage: go run ./cmd/fetchbook")
			fmt.Println()
			fmt.Println("This tool analyzes Polymarket 15-minute markets to verify")
			fmt.Println("the bot correctly sees liquidity depth at different profit margins.")
			fmt.Println()
			fmt.Println("Output explanation:")
			fmt.Println("  MARGIN     - Profit margin threshold (e.g., 2% means max sum is $0.98)")
			fmt.Println("  MAX SUM    - Maximum price sum for this margin (1.0 - margin/100)")
			fmt.Println("  MATCHED    - Matched liquidity (shares you can trade on BOTH sides)")
			fmt.Println("  RAW LIQ    - Raw liquidity on each side at valid price levels")
			fmt.Println("  DEPTH      - Book depth: total levels → valid levels within margin")
			fmt.Println()
			fmt.Println("Bot log format:")
			fmt.Println("  ARB! Up@$0.62 + Down@$0.32 = $0.94 | 21 shares, profit $0.65 (6.0%) [liq: 270/26, depth: 38/67→1/1]")
			fmt.Println()
			fmt.Println("  What this means:")
			fmt.Println("  - $0.62 + $0.32 = $0.94 → Top-of-book prices show 6% margin opportunity")
			fmt.Println("  - (6.0%) = actual margin at current prices (what you earn per share)")
			fmt.Println("  - liq: 270/26 = raw shares at valid levels (Up has 270, Down has 26)")
			fmt.Println("  - depth: 38/67→1/1 = total book levels (38 Up, 67 Down) → valid at 2% threshold (1,1)")
			fmt.Println()
			fmt.Println("  IMPORTANT: The liq/depth uses your CONFIG threshold (default 2%), NOT the 6% observed!")
			fmt.Println("  This means: 'How much liquidity is available at ≥2% margin?'")
			fmt.Println("  If only 1/1 levels valid at 2%, it means deeper levels have <2% margin.")
			os.Exit(0)
		}
	}
}
