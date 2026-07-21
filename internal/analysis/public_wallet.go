package analysis

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"Market-bot/internal/api"
)

type StrategyKind string

const (
	StrategyUnclear                      StrategyKind = "unclear"
	StrategyDirectionalAccumulator       StrategyKind = "directional-accumulator"
	StrategyDirectionalTrader            StrategyKind = "directional-trader"
	StrategyPairedAccumulator            StrategyKind = "paired-accumulator"
	StrategyHedgedDirectionalAccumulator StrategyKind = "hedged-directional-accumulator"
	StrategyTwoSidedMakerChurn           StrategyKind = "two-sided-maker-churn"
)

type OutcomeSummary struct {
	Outcome            string
	TradeCount         int
	BuyCount           int
	SellCount          int
	TotalShares        float64
	BuyShares          float64
	SellShares         float64
	TotalNotional      float64
	BuyNotional        float64
	SellNotional       float64
	VWAP               float64
	PriceMin           float64
	PriceMax           float64
	DistinctPriceCount int
}

type MarketSummary struct {
	ConditionID          string
	Title                string
	Slug                 string
	MarketFamily         string
	TradeCount           int
	BuyCount             int
	SellCount            int
	FirstTrade           time.Time
	LastTrade            time.Time
	Span                 time.Duration
	Outcomes             []OutcomeSummary
	BothOutcomes         bool
	AvgDistinctPrices    float64
	OutcomeVWAPSum       float64
	NetBuyShareImbalance float64
}

type MarketFamilySummary struct {
	Family       string
	TradeCount   int
	ConditionIDs map[string]struct{}
}

type WalletStrategyReport struct {
	Wallet                    string
	TradeCount                int
	BuyCount                  int
	SellCount                 int
	PositionCount             int
	FirstTrade                time.Time
	LastTrade                 time.Time
	TradeSpan                 time.Duration
	ConditionCount            int
	BothOutcomeConditionCount int
	BothOutcomeConditionPct   float64
	SellTradePct              float64
	PrimaryFamily             string
	PrimaryFamilyTradePct     float64
	AvgDistinctPricesPerSide  float64
	AvgOutcomeVWAPSum         float64
	Strategy                  StrategyKind
	Confidence                float64
	Evidence                  []string
	Recommendations           []string
	Markets                   []MarketSummary
	MarketFamilies            []MarketFamilySummary
}

type outcomeAccumulator struct {
	name          string
	tradeCount    int
	buyCount      int
	sellCount     int
	totalShares   float64
	buyShares     float64
	sellShares    float64
	totalNotional float64
	buyNotional   float64
	sellNotional  float64
	priceMin      float64
	priceMax      float64
	priceLevels   map[string]struct{}
}

type marketAccumulator struct {
	conditionID string
	title       string
	slug        string
	family      string
	tradeCount  int
	buyCount    int
	sellCount   int
	firstTrade  time.Time
	lastTrade   time.Time
	outcomes    map[string]*outcomeAccumulator
}

func AnalyzePublicWallet(wallet string, trades []api.PublicTrade, positions []api.Position) WalletStrategyReport {
	report := WalletStrategyReport{
		Wallet:        strings.TrimSpace(wallet),
		TradeCount:    len(trades),
		PositionCount: len(positions),
		Strategy:      StrategyUnclear,
	}
	if len(trades) == 0 {
		report.Evidence = append(report.Evidence, "no public trades returned for the requested wallet")
		if len(positions) > 0 {
			report.Evidence = append(report.Evidence, fmt.Sprintf("public positions still show %d open line(s)", len(positions)))
		}
		report.Recommendations = append(report.Recommendations,
			"Without public trades there is nothing reliable to reverse engineer. Re-run later or inspect the wallet live with the copytrade watchers.",
		)
		return report
	}

	markets := make(map[string]*marketAccumulator)
	families := make(map[string]*MarketFamilySummary)

	for _, trade := range trades {
		tradeTime := time.Unix(trade.Timestamp, 0).UTC()
		if report.FirstTrade.IsZero() || tradeTime.Before(report.FirstTrade) {
			report.FirstTrade = tradeTime
		}
		if report.LastTrade.IsZero() || tradeTime.After(report.LastTrade) {
			report.LastTrade = tradeTime
		}
		if strings.EqualFold(strings.TrimSpace(trade.Side), "SELL") {
			report.SellCount++
		} else {
			report.BuyCount++
		}

		conditionID := strings.TrimSpace(trade.ConditionID)
		if conditionID == "" {
			continue
		}
		family := slugFamily(trade.Slug)
		if family == "" {
			family = "(unknown)"
		}
		group := markets[conditionID]
		if group == nil {
			group = &marketAccumulator{
				conditionID: conditionID,
				title:       strings.TrimSpace(trade.Title),
				slug:        strings.TrimSpace(trade.Slug),
				family:      family,
				firstTrade:  tradeTime,
				lastTrade:   tradeTime,
				outcomes:    make(map[string]*outcomeAccumulator),
			}
			markets[conditionID] = group
		}
		group.tradeCount++
		if strings.EqualFold(strings.TrimSpace(trade.Side), "SELL") {
			group.sellCount++
		} else {
			group.buyCount++
		}
		if tradeTime.Before(group.firstTrade) {
			group.firstTrade = tradeTime
		}
		if tradeTime.After(group.lastTrade) {
			group.lastTrade = tradeTime
		}

		outcome := strings.TrimSpace(trade.Outcome)
		if outcome == "" {
			outcome = "(unknown)"
		}
		out := group.outcomes[outcome]
		if out == nil {
			out = &outcomeAccumulator{
				name:        outcome,
				priceMin:    math.MaxFloat64,
				priceMax:    -math.MaxFloat64,
				priceLevels: make(map[string]struct{}),
			}
			group.outcomes[outcome] = out
		}

		price := trade.Price
		shares := math.Abs(trade.Size)
		notional := shares * price
		out.tradeCount++
		out.totalShares += shares
		out.totalNotional += notional
		if price < out.priceMin {
			out.priceMin = price
		}
		if price > out.priceMax {
			out.priceMax = price
		}
		out.priceLevels[priceBucket(price)] = struct{}{}
		if strings.EqualFold(strings.TrimSpace(trade.Side), "SELL") {
			out.sellCount++
			out.sellShares += shares
			out.sellNotional += notional
		} else {
			out.buyCount++
			out.buyShares += shares
			out.buyNotional += notional
		}

		fam := families[family]
		if fam == nil {
			fam = &MarketFamilySummary{
				Family:       family,
				ConditionIDs: make(map[string]struct{}),
			}
			families[family] = fam
		}
		fam.TradeCount++
		fam.ConditionIDs[conditionID] = struct{}{}
	}

	report.TradeSpan = report.LastTrade.Sub(report.FirstTrade)
	report.ConditionCount = len(markets)
	if report.TradeCount > 0 {
		report.SellTradePct = float64(report.SellCount) / float64(report.TradeCount)
	}

	marketSummaries := make([]MarketSummary, 0, len(markets))
	familySummaries := make([]MarketFamilySummary, 0, len(families))
	for _, fam := range families {
		familySummaries = append(familySummaries, *fam)
	}
	sort.Slice(familySummaries, func(i, j int) bool {
		if familySummaries[i].TradeCount == familySummaries[j].TradeCount {
			return familySummaries[i].Family < familySummaries[j].Family
		}
		return familySummaries[i].TradeCount > familySummaries[j].TradeCount
	})
	report.MarketFamilies = familySummaries
	if len(familySummaries) > 0 {
		report.PrimaryFamily = familySummaries[0].Family
		report.PrimaryFamilyTradePct = float64(familySummaries[0].TradeCount) / float64(report.TradeCount)
	}
	totalBothOutcomeVWAPSum := 0.0
	totalVWAPSum := 0.0
	totalDistinctPerSide := 0.0
	totalSides := 0

	for _, group := range markets {
		outcomes := make([]OutcomeSummary, 0, len(group.outcomes))
		avgDistinct := 0.0
		vwapSum := 0.0

		for _, out := range group.outcomes {
			vwap := 0.0
			if out.totalShares > 0 {
				vwap = out.totalNotional / out.totalShares
			}
			distinct := len(out.priceLevels)
			outcomes = append(outcomes, OutcomeSummary{
				Outcome:            out.name,
				TradeCount:         out.tradeCount,
				BuyCount:           out.buyCount,
				SellCount:          out.sellCount,
				TotalShares:        out.totalShares,
				BuyShares:          out.buyShares,
				SellShares:         out.sellShares,
				TotalNotional:      out.totalNotional,
				BuyNotional:        out.buyNotional,
				SellNotional:       out.sellNotional,
				VWAP:               vwap,
				PriceMin:           out.priceMin,
				PriceMax:           out.priceMax,
				DistinctPriceCount: distinct,
			})
			avgDistinct += float64(distinct)
			vwapSum += vwap
			totalDistinctPerSide += float64(distinct)
			totalSides++
		}
		sort.Slice(outcomes, func(i, j int) bool {
			if outcomes[i].TradeCount == outcomes[j].TradeCount {
				return outcomes[i].Outcome < outcomes[j].Outcome
			}
			return outcomes[i].TradeCount > outcomes[j].TradeCount
		})
		bothOutcomes := len(outcomes) >= 2
		if bothOutcomes {
			report.BothOutcomeConditionCount++
			totalBothOutcomeVWAPSum += vwapSum
		}
		if len(outcomes) > 0 {
			avgDistinct /= float64(len(outcomes))
			totalVWAPSum += vwapSum
		}
		netImbalance := 0.0
		if len(outcomes) >= 2 {
			netImbalance = math.Abs(outcomes[0].BuyShares - outcomes[1].BuyShares)
		}
		marketSummaries = append(marketSummaries, MarketSummary{
			ConditionID:          group.conditionID,
			Title:                group.title,
			Slug:                 group.slug,
			MarketFamily:         group.family,
			TradeCount:           group.tradeCount,
			BuyCount:             group.buyCount,
			SellCount:            group.sellCount,
			FirstTrade:           group.firstTrade,
			LastTrade:            group.lastTrade,
			Span:                 group.lastTrade.Sub(group.firstTrade),
			Outcomes:             outcomes,
			BothOutcomes:         bothOutcomes,
			AvgDistinctPrices:    avgDistinct,
			OutcomeVWAPSum:       vwapSum,
			NetBuyShareImbalance: netImbalance,
		})
	}
	sort.Slice(marketSummaries, func(i, j int) bool {
		if marketSummaries[i].TradeCount == marketSummaries[j].TradeCount {
			return marketSummaries[i].ConditionID < marketSummaries[j].ConditionID
		}
		return marketSummaries[i].TradeCount > marketSummaries[j].TradeCount
	})
	report.Markets = marketSummaries
	if report.ConditionCount > 0 {
		report.BothOutcomeConditionPct = float64(report.BothOutcomeConditionCount) / float64(report.ConditionCount)
		if report.BothOutcomeConditionCount > 0 {
			report.AvgOutcomeVWAPSum = totalBothOutcomeVWAPSum / float64(report.BothOutcomeConditionCount)
		} else {
			report.AvgOutcomeVWAPSum = totalVWAPSum / float64(report.ConditionCount)
		}
	}
	if totalSides > 0 {
		report.AvgDistinctPricesPerSide = totalDistinctPerSide / float64(totalSides)
	}

	classifyWalletStrategy(&report)
	return report
}

func slugFamily(slug string) string {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return ""
	}
	parts := strings.Split(slug, "-")
	if len(parts) == 0 {
		return slug
	}
	last := parts[len(parts)-1]
	if val, err := strconv.ParseInt(last, 10, 64); err == nil {
		if val > 1000000000 || (val >= 2000 && val <= 2100) {
			return strings.Join(parts[:len(parts)-1], "-")
		}
	}
	return slug
}

func priceBucket(price float64) string {
	return fmt.Sprintf("%.3f", price)
}

func classifyWalletStrategy(report *WalletStrategyReport) {
	if report == nil {
		return
	}
	var evidence []string
	evidence = append(evidence,
		fmt.Sprintf("%d public trades across %d condition(s) from %s to %s", report.TradeCount, report.ConditionCount, report.FirstTrade.Format(time.RFC3339), report.LastTrade.Format(time.RFC3339)),
		fmt.Sprintf("%.0f%% of sampled trades are in %s", report.PrimaryFamilyTradePct*100, report.PrimaryFamily),
		fmt.Sprintf("%.0f%% of sampled conditions trade both outcomes", report.BothOutcomeConditionPct*100),
		fmt.Sprintf("%.0f%% of sampled trades are SELL fills", report.SellTradePct*100),
		fmt.Sprintf("average ladder depth is %.1f distinct price levels per outcome", report.AvgDistinctPricesPerSide),
	)
	if report.PositionCount == 0 {
		evidence = append(evidence, "public positions currently show no open inventory")
	}
	report.Evidence = evidence

	confidence := 0.35
	if report.PrimaryFamilyTradePct >= 0.9 {
		confidence += 0.20
	}
	if report.BothOutcomeConditionPct >= 0.8 {
		confidence += 0.20
	}
	if report.SellTradePct <= 0.05 {
		confidence += 0.10
	}
	if report.AvgDistinctPricesPerSide >= 3 {
		confidence += 0.10
	}
	if report.PositionCount == 0 {
		confidence += 0.05
	}

	switch {
	case report.BothOutcomeConditionPct >= 0.8 && report.SellTradePct >= 0.15:
		report.Strategy = StrategyTwoSidedMakerChurn
		report.Recommendations = []string{
			"This looks like true two-sided inventory recycling. Existing copytrade mode will lag it badly.",
			"If you want to mimic it, use maker infrastructure plus pending/onchain watchers, not single-leg copytrading.",
		}
	case report.BothOutcomeConditionPct >= 0.8 && report.SellTradePct <= 0.05:
		if report.AvgOutcomeVWAPSum >= 0.97 && report.AvgOutcomeVWAPSum <= 1.03 {
			report.Strategy = StrategyPairedAccumulator
			report.Recommendations = []string{
				"This is close to a paired accumulation flow. Existing panic-buy / pair logic is the closest base, but it needs repeated laddered entries instead of one-shot pair fills.",
				"Do not use current copytrade mode as the main replication path because the target is not behaving like a clean single-leg enter/exit trader.",
			}
		} else {
			report.Strategy = StrategyHedgedDirectionalAccumulator
			report.Recommendations = []string{
				"This is not clean under-$1 pair arb. The wallet buys both outcomes, but the price sums and share imbalances imply a hedged directional taker rather than a pure merge bot.",
				"The closest fit is a new taker mode for 5m ETH markets that can ladder into the favored side while optionally buying a cheap hedge on the opposite side.",
				"Current copytrade mode is a poor fit because it assumes a single-leg position state and exit detection from public holdings.",
			}
		}
	case report.BothOutcomeConditionPct < 0.3 && report.SellTradePct <= 0.10:
		report.Strategy = StrategyDirectionalAccumulator
		report.Recommendations = []string{
			"This mostly behaves like one-sided accumulation. Existing copytrade mode can mirror the direction if the wallet keeps positions visible long enough.",
		}
	default:
		report.Strategy = StrategyDirectionalTrader
		report.Recommendations = []string{
			"This looks closer to directional trading with active entry and exit. Existing copytrade mode may work, but only if public trades/positions stay fresh enough for your latency budget.",
		}
	}

	if confidence > 0.98 {
		confidence = 0.98
	}
	report.Confidence = confidence
}
