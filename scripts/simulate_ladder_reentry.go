//go:build ignore

// simulate_ladder_reentry generates randomized binary-market price paths and
// compares laddered directional re-entry performance for:
//   - fixed 1 share per entry
//   - fixed 1 USDC per entry
//
// Presets sweep re-entry move thresholds from 1c to 5c. PnL is marked to the
// final simulated market price; there is no separate settlement model.
//
// Usage:
//
//	go run scripts/simulate_ladder_reentry.go
//	go run scripts/simulate_ladder_reentry.go -markets 50 -ticks 80 -seed 42
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"text/tabwriter"
	"time"

	"Market-bot/internal/api"
)

const (
	modeShares = "shares"
	modeUSDC   = "usdc"
)

type regime string

const (
	regimeTrendUp   regime = "trend-up"
	regimeTrendDown regime = "trend-down"
	regimeChop      regime = "chop"
	regimeAmbiguous regime = "ambiguous"
)

type marketCase struct {
	ID     int
	Regime regime
	Slug   string
	Prices []float64
}

type result struct {
	Cost        float64
	Value       float64
	PnL         float64
	Entries     int
	UpEntries   int
	DownEntries int
	UpShares    float64
	DownShares  float64
	StartPrice  float64
	FinalPrice  float64
	MaxExposure float64
	MaxDrawdown float64
}

type summary struct {
	Markets        int
	Profitable     int
	Entries        int
	Cost           float64
	Value          float64
	PnL            float64
	ExposureSum    float64
	MaxExposure    float64
	DrawdownSum    float64
	MaxDrawdown    float64
	BestMarketID   int
	BestMarketReg  regime
	BestMarketPnL  float64
	WorstMarketID  int
	WorstMarketReg regime
	WorstMarketPnL float64
	bestMarketSet  bool
	worstMarketSet bool
}

type presetChoice struct {
	Step int
	Mode string
	Sum  summary
}

type csvRecord struct {
	MarketID int
	Regime   regime
	Slug     string
	Step     int
	Mode     string
	Result   result
}

type priceHistoryResponse struct {
	History []priceHistoryPoint `json:"history"`
}

type priceHistoryPoint struct {
	T int64   `json:"t"`
	P float64 `json:"p"`
}

func (s *summary) add(marketID int, reg regime, r result) {
	s.Markets++
	if r.PnL > 0 {
		s.Profitable++
	}
	s.Entries += r.Entries
	s.Cost += r.Cost
	s.Value += r.Value
	s.PnL += r.PnL
	s.ExposureSum += r.MaxExposure
	s.DrawdownSum += r.MaxDrawdown
	if r.MaxExposure > s.MaxExposure {
		s.MaxExposure = r.MaxExposure
	}
	if r.MaxDrawdown > s.MaxDrawdown {
		s.MaxDrawdown = r.MaxDrawdown
	}
	if !s.bestMarketSet || r.PnL > s.BestMarketPnL {
		s.BestMarketID = marketID
		s.BestMarketReg = reg
		s.BestMarketPnL = r.PnL
		s.bestMarketSet = true
	}
	if !s.worstMarketSet || r.PnL < s.WorstMarketPnL {
		s.WorstMarketID = marketID
		s.WorstMarketReg = reg
		s.WorstMarketPnL = r.PnL
		s.worstMarketSet = true
	}
}

func classifyRegime(prices []float64) regime {
	if len(prices) < 2 {
		return regimeAmbiguous
	}
	start := prices[0]
	end := prices[len(prices)-1]
	net := end - start
	minPrice := start
	maxPrice := start
	signChanges := 0
	lastSign := 0
	totalAbsMove := 0.0

	for i := 1; i < len(prices); i++ {
		diff := prices[i] - prices[i-1]
		totalAbsMove += math.Abs(diff)
		sign := 0
		if diff > 1e-9 {
			sign = 1
		} else if diff < -1e-9 {
			sign = -1
		}
		if sign != 0 && lastSign != 0 && sign != lastSign {
			signChanges++
		}
		if sign != 0 {
			lastSign = sign
		}
		if prices[i] < minPrice {
			minPrice = prices[i]
		}
		if prices[i] > maxPrice {
			maxPrice = prices[i]
		}
	}

	pathRange := maxPrice - minPrice
	if net >= 0.15 && net >= pathRange*0.5 {
		return regimeTrendUp
	}
	if net <= -0.15 && -net >= pathRange*0.5 {
		return regimeTrendDown
	}
	if pathRange <= 0.12 || (math.Abs(net) <= pathRange*0.35 && signChanges >= 3 && totalAbsMove >= pathRange*1.5) {
		return regimeChop
	}
	return regimeAmbiguous
}

func runLadder(prices []float64, stepCents int, mode string, shareSize, usdcSize float64) result {
	if len(prices) == 0 {
		return result{}
	}

	threshold := float64(stepCents) / 100.0
	anchor := prices[0]
	upShares := 0.0
	downShares := 0.0
	totalCost := 0.0
	out := result{StartPrice: prices[0]}
	peakEquity := 0.0

	for i := 0; i < len(prices); i++ {
		price := prices[i]
		if i > 0 {
			move := price - anchor

			switch {
			case move >= threshold:
				qty := shareSize
				if mode == modeUSDC {
					qty = usdcSize / price
				}
				upShares += qty
				cost := qty * price
				totalCost += cost
				anchor = price
				out.Entries++
				out.UpEntries++
			case move <= -threshold:
				downPrice := 1.0 - price
				qty := shareSize
				if mode == modeUSDC {
					qty = usdcSize / downPrice
				}
				downShares += qty
				cost := qty * downPrice
				totalCost += cost
				anchor = price
				out.Entries++
				out.DownEntries++
			}
		}

		markValue := upShares*price + downShares*(1.0-price)
		equity := markValue - totalCost
		if markValue > out.MaxExposure {
			out.MaxExposure = markValue
		}
		if equity > peakEquity {
			peakEquity = equity
		}
		drawdown := peakEquity - equity
		if drawdown > out.MaxDrawdown {
			out.MaxDrawdown = drawdown
		}
	}

	finalPrice := prices[len(prices)-1]
	finalValue := upShares*finalPrice + downShares*(1.0-finalPrice)
	out.Cost = totalCost
	out.Value = finalValue
	out.PnL = finalValue - totalCost
	out.UpShares = upShares
	out.DownShares = downShares
	out.FinalPrice = finalPrice
	return out
}

func pct(num, den float64) float64 {
	if den == 0 {
		return 0
	}
	return (num / den) * 100.0
}

func formatSummaryRow(w *tabwriter.Writer, label string, s summary) {
	avgPnL := 0.0
	avgEntries := 0.0
	avgCost := 0.0
	avgExposure := 0.0
	avgDrawdown := 0.0
	if s.Markets > 0 {
		avgPnL = s.PnL / float64(s.Markets)
		avgEntries = float64(s.Entries) / float64(s.Markets)
		avgCost = s.Cost / float64(s.Markets)
		avgExposure = s.ExposureSum / float64(s.Markets)
		avgDrawdown = s.DrawdownSum / float64(s.Markets)
	}
	fmt.Fprintf(w, "%s\t%.4f\t%.4f\t%.2f%%\t%.2f%%\t%.2f\t%.4f\t%.4f\t%.4f\t%.4f\t%.4f\tM%d %.4f\tM%d %.4f\t%d/%d\n",
		label,
		s.PnL,
		avgPnL,
		pct(s.PnL, s.Cost),
		pct(float64(s.Profitable), float64(s.Markets)),
		avgEntries,
		avgCost,
		avgExposure,
		s.MaxExposure,
		avgDrawdown,
		s.MaxDrawdown,
		s.BestMarketID,
		s.BestMarketPnL,
		s.WorstMarketID,
		s.WorstMarketPnL,
		s.Profitable, s.Markets,
	)
}

func winnerText(aName string, a summary, bName string, b summary) string {
	switch {
	case math.Abs(a.PnL-b.PnL) <= 1e-9:
		return "tie"
	case a.PnL > b.PnL:
		return aName
	default:
		return bName
	}
}

func bestByTotalPnL(overall map[int]map[string]summary) presetChoice {
	best := presetChoice{}
	bestSet := false
	for step := 1; step <= 5; step++ {
		for _, mode := range []string{modeShares, modeUSDC} {
			current := presetChoice{
				Step: step,
				Mode: mode,
				Sum:  overall[step][mode],
			}
			if !bestSet || current.Sum.PnL > best.Sum.PnL {
				best = current
				bestSet = true
			}
		}
	}
	return best
}

func bestByROI(overall map[int]map[string]summary) presetChoice {
	best := presetChoice{}
	bestSet := false
	for step := 1; step <= 5; step++ {
		for _, mode := range []string{modeShares, modeUSDC} {
			current := presetChoice{
				Step: step,
				Mode: mode,
				Sum:  overall[step][mode],
			}
			if !bestSet || pct(current.Sum.PnL, current.Sum.Cost) > pct(best.Sum.PnL, best.Sum.Cost) {
				best = current
				bestSet = true
			}
		}
	}
	return best
}

func writeDetailCSV(path string, records []csvRecord) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	header := []string{
		"market_id", "slug", "regime", "step_cents", "mode",
		"start_price", "final_price",
		"entries", "up_entries", "down_entries",
		"up_shares", "down_shares",
		"cost", "final_value", "pnl",
		"max_exposure", "max_drawdown",
	}
	if err := w.Write(header); err != nil {
		return err
	}

	for _, rec := range records {
		row := []string{
			strconv.Itoa(rec.MarketID),
			rec.Slug,
			string(rec.Regime),
			strconv.Itoa(rec.Step),
			rec.Mode,
			fmt.Sprintf("%.6f", rec.Result.StartPrice),
			fmt.Sprintf("%.6f", rec.Result.FinalPrice),
			strconv.Itoa(rec.Result.Entries),
			strconv.Itoa(rec.Result.UpEntries),
			strconv.Itoa(rec.Result.DownEntries),
			fmt.Sprintf("%.6f", rec.Result.UpShares),
			fmt.Sprintf("%.6f", rec.Result.DownShares),
			fmt.Sprintf("%.6f", rec.Result.Cost),
			fmt.Sprintf("%.6f", rec.Result.Value),
			fmt.Sprintf("%.6f", rec.Result.PnL),
			fmt.Sprintf("%.6f", rec.Result.MaxExposure),
			fmt.Sprintf("%.6f", rec.Result.MaxDrawdown),
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}

	return w.Error()
}

func writeSummaryCSV(path string, overall map[int]map[string]summary) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	header := []string{
		"step_cents", "mode",
		"markets", "profitable_markets",
		"total_pnl", "avg_pnl", "roi_pct", "win_rate_pct",
		"avg_entries", "avg_cost",
		"avg_max_exposure", "max_exposure",
		"avg_max_drawdown", "max_drawdown",
		"best_market_id", "best_market_pnl", "best_market_regime",
		"worst_market_id", "worst_market_pnl", "worst_market_regime",
	}
	if err := w.Write(header); err != nil {
		return err
	}

	for step := 1; step <= 5; step++ {
		for _, mode := range []string{modeShares, modeUSDC} {
			s := overall[step][mode]
			avgPnL := 0.0
			avgEntries := 0.0
			avgCost := 0.0
			avgExposure := 0.0
			avgDrawdown := 0.0
			if s.Markets > 0 {
				avgPnL = s.PnL / float64(s.Markets)
				avgEntries = float64(s.Entries) / float64(s.Markets)
				avgCost = s.Cost / float64(s.Markets)
				avgExposure = s.ExposureSum / float64(s.Markets)
				avgDrawdown = s.DrawdownSum / float64(s.Markets)
			}
			row := []string{
				strconv.Itoa(step),
				mode,
				strconv.Itoa(s.Markets),
				strconv.Itoa(s.Profitable),
				fmt.Sprintf("%.6f", s.PnL),
				fmt.Sprintf("%.6f", avgPnL),
				fmt.Sprintf("%.6f", pct(s.PnL, s.Cost)),
				fmt.Sprintf("%.6f", pct(float64(s.Profitable), float64(s.Markets))),
				fmt.Sprintf("%.6f", avgEntries),
				fmt.Sprintf("%.6f", avgCost),
				fmt.Sprintf("%.6f", avgExposure),
				fmt.Sprintf("%.6f", s.MaxExposure),
				fmt.Sprintf("%.6f", avgDrawdown),
				fmt.Sprintf("%.6f", s.MaxDrawdown),
				strconv.Itoa(s.BestMarketID),
				fmt.Sprintf("%.6f", s.BestMarketPnL),
				string(s.BestMarketReg),
				strconv.Itoa(s.WorstMarketID),
				fmt.Sprintf("%.6f", s.WorstMarketPnL),
				string(s.WorstMarketReg),
			}
			if err := w.Write(row); err != nil {
				return err
			}
		}
	}

	return w.Error()
}

func fetchRecentBTC15mMarkets(ctx context.Context, count int) ([]marketCase, error) {
	if count <= 0 {
		return nil, errors.New("count must be > 0")
	}

	rest := api.NewRestClient("")
	now := time.Now().UTC().Unix()
	currentWindowStart := (now / 900) * 900
	cases := make([]marketCase, 0, count)

	for offset := int64(1); len(cases) < count && offset <= int64(count+10); offset++ {
		windowStart := currentWindowStart - offset*900
		slug := fmt.Sprintf("btc-updown-15m-%d", windowStart)
		markets, err := rest.GetMarketsByEventSlug(ctx, slug)
		if err != nil || len(markets) == 0 {
			continue
		}

		market := markets[0]
		tokenID := ""
		for _, token := range market.Tokens {
			if token.Outcome == "Up" {
				tokenID = token.TokenID
				break
			}
		}
		if tokenID == "" {
			tokenID = market.Tokens[0].TokenID
		}

		prices, err := fetchTokenPricePath(ctx, tokenID, windowStart, windowStart+900)
		if err != nil || len(prices) < 2 {
			continue
		}

		cases = append(cases, marketCase{
			ID:     len(cases) + 1,
			Slug:   market.Slug,
			Regime: classifyRegime(prices),
			Prices: prices,
		})
	}

	if len(cases) == 0 {
		return nil, fmt.Errorf("no BTC 15m markets with usable history found")
	}
	return cases, nil
}

func fetchTokenPricePath(ctx context.Context, tokenID string, startTS, endTS int64) ([]float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("https://clob.polymarket.com/prices-history?market=%s&interval=1m&fidelity=10", tokenID), nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prices-history status %d", resp.StatusCode)
	}

	var payload priceHistoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	if len(payload.History) == 0 {
		return nil, errors.New("empty price history")
	}

	prices := make([]float64, 0, len(payload.History))
	seeded := false
	for _, pt := range payload.History {
		if pt.T <= startTS {
			if len(prices) == 0 {
				prices = append(prices, pt.P)
			} else {
				prices[0] = pt.P
			}
			seeded = true
			continue
		}
		if pt.T > endTS {
			break
		}
		prices = append(prices, pt.P)
	}
	if !seeded && len(payload.History) > 0 {
		prices = append([]float64{payload.History[0].P}, prices...)
	}
	if len(prices) < 2 {
		last := prices
		for i := len(payload.History) - 1; i >= 0 && len(last) < 2; i-- {
			pt := payload.History[i]
			if pt.T <= endTS {
				last = append(last, pt.P)
			}
		}
		prices = last
	}
	return prices, nil
}

func main() {
	var (
		marketCount = flag.Int("markets", 50, "number of random markets to simulate")
		csvPrefix   = flag.String("csv-prefix", "", "output prefix for detail/summary CSV files")
		shareSize   = flag.Float64("share-size", 1.0, "fixed shares per entry when mode=shares")
		usdcSize    = flag.Float64("usdc-size", 1.0, "fixed USDC budget per entry when mode=usdc")
	)
	flag.Parse()

	if *marketCount <= 0 || *shareSize <= 0 || *usdcSize <= 0 {
		fmt.Fprintln(os.Stderr, "markets must be > 0, and share/usdc sizes must be > 0")
		os.Exit(1)
	}

	var markets []marketCase
	regimeCounts := map[regime]int{}
	
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var err error
	markets, err = fetchRecentBTC15mMarkets(ctx, *marketCount)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to fetch BTC 15m history: %v\n", err)
		os.Exit(1)
	}
	for _, m := range markets {
		regimeCounts[m.Regime]++
	}

	overall := make(map[int]map[string]summary)
	byRegime := make(map[regime]map[int]map[string]summary)
	records := make([]csvRecord, 0, *marketCount*10)
	for _, reg := range []regime{regimeTrendUp, regimeTrendDown, regimeChop, regimeAmbiguous} {
		byRegime[reg] = make(map[int]map[string]summary)
	}

	for step := 1; step <= 5; step++ {
		overall[step] = map[string]summary{
			modeShares: {},
			modeUSDC:   {},
		}
		for _, reg := range []regime{regimeTrendUp, regimeTrendDown, regimeChop, regimeAmbiguous} {
			byRegime[reg][step] = map[string]summary{
				modeShares: {},
				modeUSDC:   {},
			}
		}
	}

	for _, m := range markets {
		for step := 1; step <= 5; step++ {
			for _, mode := range []string{modeShares, modeUSDC} {
				res := runLadder(m.Prices, step, mode, *shareSize, *usdcSize)
				records = append(records, csvRecord{
					MarketID: m.ID,
					Regime:   m.Regime,
					Slug:     m.Slug,
					Step:     step,
					Mode:     mode,
					Result:   res,
				})
				s := overall[step][mode]
				s.add(m.ID, m.Regime, res)
				overall[step][mode] = s

				rs := byRegime[m.Regime][step][mode]
				rs.add(m.ID, m.Regime, res)
				byRegime[m.Regime][step][mode] = rs
			}
		}
	}

	prefix := *csvPrefix
	if prefix == "" {
		prefix = filepath.Join("logs", fmt.Sprintf("ladder_reentry_btc15m_%dmarkets", len(markets)))
	}
	if err := os.MkdirAll(filepath.Dir(prefix), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create csv output dir: %v\n", err)
		os.Exit(1)
	}
	detailCSV := prefix + "_detail.csv"
	summaryCSV := prefix + "_summary.csv"
	if err := writeDetailCSV(detailCSV, records); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write detail csv: %v\n", err)
		os.Exit(1)
	}
	if err := writeSummaryCSV(summaryCSV, overall); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write summary csv: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Ladder re-entry simulation\n")
	fmt.Printf("source=btc15m markets=%d\n", len(markets))
	fmt.Printf("re-entry presets=1c..5c | entry sizing compares fixed %.2f share vs fixed %.2f USDC\n", *shareSize, *usdcSize)
	fmt.Printf("PnL is mark-to-final-price, not settlement payout\n\n")
	fmt.Printf("CSV detail: %s\n", detailCSV)
	fmt.Printf("CSV summary: %s\n\n", summaryCSV)

	type regimeCount struct {
		name  regime
		count int
	}
	counts := []regimeCount{
		{name: regimeTrendUp, count: regimeCounts[regimeTrendUp]},
		{name: regimeTrendDown, count: regimeCounts[regimeTrendDown]},
		{name: regimeChop, count: regimeCounts[regimeChop]},
		{name: regimeAmbiguous, count: regimeCounts[regimeAmbiguous]},
	}
	fmt.Printf("Generated regimes:\n")
	for _, item := range counts {
		fmt.Printf("  %-10s %d\n", item.name, item.count)
	}
	fmt.Println()

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "OVERALL\tTOTAL_PNL\tAVG_PNL\tROI\tWIN_RATE\tAVG_ENTRIES\tAVG_COST\tAVG_MAX_EXPOSURE\tMAX_EXPOSURE\tAVG_MAX_DRAWDOWN\tMAX_DRAWDOWN\tBEST_PNL\tWORST_PNL\tPROFITABLE")
	for step := 1; step <= 5; step++ {
		formatSummaryRow(w, fmt.Sprintf("%dc %s", step, modeShares), overall[step][modeShares])
		formatSummaryRow(w, fmt.Sprintf("%dc %s", step, modeUSDC), overall[step][modeUSDC])
	}
	_ = w.Flush()
	fmt.Println()

	fmt.Println("Preset winners by total PnL:")
	for step := 1; step <= 5; step++ {
		shareSummary := overall[step][modeShares]
		usdcSummary := overall[step][modeUSDC]
		fmt.Printf("  %dc: %s\n", step, winnerText(modeShares, shareSummary, modeUSDC, usdcSummary))
	}
	fmt.Println()

	bestPnL := bestByTotalPnL(overall)
	bestROI := bestByROI(overall)
	fmt.Printf("Best preset by total PnL: %dc %s (total PnL %.4f, ROI %.2f%%)\n",
		bestPnL.Step, bestPnL.Mode, bestPnL.Sum.PnL, pct(bestPnL.Sum.PnL, bestPnL.Sum.Cost))
	fmt.Printf("Best preset by ROI: %dc %s (total PnL %.4f, ROI %.2f%%)\n\n",
		bestROI.Step, bestROI.Mode, bestROI.Sum.PnL, pct(bestROI.Sum.PnL, bestROI.Sum.Cost))

	regimes := []regime{regimeTrendUp, regimeTrendDown, regimeChop, regimeAmbiguous}
	sort.Slice(regimes, func(i, j int) bool { return regimes[i] < regimes[j] })
	for _, reg := range regimes {
		fmt.Printf("%s winners by total PnL:\n", reg)
		for step := 1; step <= 5; step++ {
			shareSummary := byRegime[reg][step][modeShares]
			usdcSummary := byRegime[reg][step][modeUSDC]
			fmt.Printf("  %dc: %s\n", step, winnerText(modeShares, shareSummary, modeUSDC, usdcSummary))
		}
		fmt.Println()
	}
}
