package main

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"os"
	"strings"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/marketlookup"
	"Market-bot/internal/setup"
	"github.com/joho/godotenv"
)

const minOnChainActionShares = 0.01
const redeemSkipOnlyLosingBalance = "only losing-side balance remains"

type marketInfoFetcher interface {
	GetMarketInfo(ctx context.Context, conditionID string) (*api.MarketInfo, error)
}

type marketResolutionReader interface {
	IsMarketResolved(ctx context.Context, conditionID string) (bool, error)
	GetWinningOutcome(ctx context.Context, conditionID string, outcomes []string) (string, error)
	GetPayoutNumerator(ctx context.Context, conditionID string, index int) (*big.Int, error)
}

type redeemDecision struct {
	winnerOutcome string
	shouldRedeem  bool
	resolved      bool
	source        string
	reason        string
}

func main() {
	_ = godotenv.Load()
	cfg, err := core.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Create context for setup
	setupCtx, cancelSetup := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelSetup()

	// Ensure credentials exist and allowances are ready for real trading
	trader, err := setup.EnsureRealTradingSetup(setupCtx, cfg)
	if err != nil {
		log.Fatalf("Failed to setup or create trader: %v", err)
	}

	ctx := context.Background()
	polygon := api.NewPolygonClient(cfg.PolygonRPCURL)
	address := trader.Address()

	fmt.Println("🚀 POLYARB SMART SCANNER (Merge & Redeem Only)")
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Printf("🔑 Wallet: %s\n", address)

	target := ""
	for _, arg := range os.Args[1:] {
		if target == "" && !strings.HasPrefix(arg, "-") {
			target = arg
		}
	}

	if target != "" {
		fmt.Printf("🔍 Resolving target market: %s\n", target)
	}

	markets, source, err := marketlookup.ResolveMarkets(ctx, trader, polygon, target)
	if err != nil {
		log.Fatalf("Failed to resolve markets: %v", err)
	}
	if len(markets) == 0 {
		if target != "" {
			fmt.Printf("✅ No markets found for target %s.\n", target)
		} else {
			fmt.Println("✅ No relevant markets found for your positions. Try `mergeredeem <slug-or-condition-id>` for a direct lookup.")
		}
		return
	}
	fmt.Printf("✅ Loaded %d market(s) via %s\n", len(markets), source)

	foundAny := false
	processed := make(map[string]bool)

	for _, m := range markets {
		if processed[m.ConditionID] {
			continue
		}
		processed[m.ConditionID] = true

		var balances []float64
		var outcomes []string

		for _, t := range m.Tokens {
			bal, err := trader.GetCTFBalanceFloat(ctx, t.TokenID)
			if err != nil {
				balances = append(balances, 0)
			} else {
				balances = append(balances, bal)
			}
			outcomes = append(outcomes, t.Outcome)
		}

		// Skip if no tokens found in this market or all are dust
		if len(balances) < 2 {
			continue
		}

		hasSignificantBalance := false
		for _, b := range balances {
			if b >= 0.0001 {
				hasSignificantBalance = true
				break
			}
		}

		if !hasSignificantBalance {
			continue
		}
		minQty := mergeablePairs(balances)
		remainingBalances := append([]float64(nil), balances...)

		marketLabelPrinted := false
		printMarketHeader := func() {
			if marketLabelPrinted {
				return
			}
			foundAny = true
			fmt.Printf("\n📈 Market: %s\n", m.Slug)
			for i, out := range outcomes {
				if i < len(balances) {
					fmt.Printf("   • %s: %.6f shares\n", out, balances[i])
				}
			}
			marketLabelPrinted = true
		}

		if minQty > 0 {
			printMarketHeader()
			fmt.Printf("   👉 ACTION: Can MERGE %.6f pairs (%.6f shares/side) into $%.2f USDC\n", minQty, minQty, minQty)
			fmt.Print("   Confirm Merge? (y/n): ")
			var confirm string
			_, _ = fmt.Scanln(&confirm)
			if strings.ToLower(confirm) == "y" {
				mergeCtx, cancelMerge := context.WithTimeout(ctx, 90*time.Second)
				tx, err := trader.MergeOnChain(mergeCtx, m.ConditionID, minQty, len(m.Tokens))
				cancelMerge()
				if err != nil {
					fmt.Printf("   ❌ Merge failed: %v\n", err)
				} else {
					fmt.Printf("   ✅ Merge successful! Tx: %s\n", tx)
					for i := range balances {
						balances[i] -= minQty
						remainingBalances[i] = balances[i]
					}
				}
			}
		}

		// Logic 2: REDEEM
		hasRemaining := false
		for _, bal := range remainingBalances {
			if bal >= 0.01 {
				hasRemaining = true
				break
			}
		}
		if hasRemaining {
			decision, err := resolveRedeemDecision(ctx, trader, polygon, m, remainingBalances)
			if err != nil {
				printMarketHeader()
				fmt.Printf("   ⚠️ Resolution status pending or unavailable.\n")
				continue
			}

			if !decision.shouldRedeem {
				if decision.reason != "" {
					if decision.reason == redeemSkipOnlyLosingBalance {
						continue
					}
					printMarketHeader()
					fmt.Printf("   ⏭️ Skip redeem: %s.\n", decision.reason)
				}
				continue
			}

			printMarketHeader()
			if decision.source == "on-chain" {
				fmt.Printf("   🏁 Result: %s Won (on-chain)\n", decision.winnerOutcome)
			} else {
				fmt.Printf("   🏁 Result: %s Won\n", decision.winnerOutcome)
			}
			fmt.Printf("   👉 ACTION: Auto redeem winning shares (forced).\n")

			redeemCtx, cancelRedeem := context.WithTimeout(ctx, 20*time.Second)
			tx, err := trader.SubmitRedeemOnChainForce(redeemCtx, m.ConditionID, len(m.Tokens))
			cancelRedeem()

			if err != nil {
				if !isSkippableRedeemError(err) {
					fmt.Printf("   ❌ Redeem failed: %v\n", err)
				}
			} else {
				fmt.Printf("   ⏳ Redeem submitted! Tx: %s\n", tx)
			}
		}
	}

	if !foundAny {
		fmt.Println("✅ No actionable merge/redeem positions found.")
	}
	fmt.Println("\n═══════════════════════════════════════════════════════")
}

func mergeablePairs(balances []float64) float64 {
	if len(balances) < 2 {
		return 0
	}
	minQty := balances[0]
	for _, bal := range balances {
		if bal < minQty {
			minQty = bal
		}
	}
	if minQty < minOnChainActionShares {
		return 0
	}
	return minQty
}

func autoRedeemDecision(info *api.MarketInfo, outcomes []string, balances []float64) (winnerOutcome string, shouldRedeem bool) {
	if info == nil || !info.Closed {
		return "", false
	}
	for _, t := range info.Tokens {
		if t.Winner {
			winnerOutcome = t.Outcome
			break
		}
	}
	if winnerOutcome == "" {
		return "", false
	}
	for i, outcome := range outcomes {
		if strings.EqualFold(strings.TrimSpace(outcome), strings.TrimSpace(winnerOutcome)) && i < len(balances) && balances[i] >= minOnChainActionShares {
			return winnerOutcome, true
		}
	}
	return winnerOutcome, false
}

func resolveRedeemDecision(ctx context.Context, infoFetcher marketInfoFetcher, resolutionReader marketResolutionReader, market api.Market, balances []float64) (redeemDecision, error) {
	outcomes := marketOutcomes(market)

	info, err := infoFetcher.GetMarketInfo(ctx, market.ConditionID)
	if err == nil {
		winnerOutcome, shouldRedeem := autoRedeemDecision(info, outcomes, balances)
		if shouldRedeem {
			return redeemDecision{
				winnerOutcome: winnerOutcome,
				shouldRedeem:  true,
				resolved:      true,
				source:        "clob",
			}, nil
		}
		if info.Closed {
			if winnerOutcome == "" {
				return resolveRedeemDecisionOnChain(ctx, resolutionReader, market, outcomes, balances)
			}
			return redeemDecision{
				winnerOutcome: winnerOutcome,
				resolved:      true,
				source:        "clob",
				reason:        redeemSkipOnlyLosingBalance,
			}, nil
		}
	}

	if !market.EndTime.IsZero() && time.Now().Before(market.EndTime) {
		return redeemDecision{source: "clob", reason: "market not decided yet"}, nil
	}

	onChainDecision, onChainErr := resolveRedeemDecisionOnChain(ctx, resolutionReader, market, outcomes, balances)
	if onChainErr == nil {
		return onChainDecision, nil
	}
	if err != nil {
		return redeemDecision{}, fmt.Errorf("clob resolution lookup failed: %w", err)
	}
	return redeemDecision{}, onChainErr
}

func resolveRedeemDecisionOnChain(ctx context.Context, resolutionReader marketResolutionReader, market api.Market, outcomes []string, balances []float64) (redeemDecision, error) {
	if resolutionReader == nil {
		return redeemDecision{source: "on-chain", reason: "market not decided yet"}, nil
	}

	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	resolved, err := resolutionReader.IsMarketResolved(checkCtx, market.ConditionID)
	if err != nil {
		return redeemDecision{}, err
	}
	if !resolved {
		return redeemDecision{source: "on-chain", reason: "market not decided yet"}, nil
	}

	// Query payout numerators for all outcomes to handle splits, ties, and normal wins
	var winningOutcomes []string
	hasPayout := false
	for i, outcome := range outcomes {
		numerator, err := resolutionReader.GetPayoutNumerator(checkCtx, market.ConditionID, i)
		if err != nil {
			// If GetPayoutNumerator fails, fallback to GetWinningOutcome for backward compatibility
			winnerOutcome, winErr := resolutionReader.GetWinningOutcome(checkCtx, market.ConditionID, outcomes)
			if winErr != nil {
				return redeemDecision{}, winErr
			}
			if winnerOutcome == "" {
				return redeemDecision{
					resolved: true,
					source:   "on-chain",
					reason:   "winner not decided yet",
				}, nil
			}
			if hasWinningBalance(winnerOutcome, outcomes, balances) {
				return redeemDecision{
					winnerOutcome: winnerOutcome,
					shouldRedeem:  true,
					resolved:      true,
					source:        "on-chain",
				}, nil
			}
			return redeemDecision{
				winnerOutcome: winnerOutcome,
				resolved:      true,
				source:        "on-chain",
				reason:        redeemSkipOnlyLosingBalance,
			}, nil
		}

		if numerator.Sign() > 0 {
			winningOutcomes = append(winningOutcomes, outcome)
			if i < len(balances) && balances[i] >= minOnChainActionShares {
				hasPayout = true
			}
		}
	}

	if len(winningOutcomes) == 0 {
		return redeemDecision{
			resolved: true,
			source:   "on-chain",
			reason:   "winner not decided yet",
		}, nil
	}

	winnerLabel := strings.Join(winningOutcomes, "/")
	if hasPayout {
		return redeemDecision{
			winnerOutcome: winnerLabel,
			shouldRedeem:  true,
			resolved:      true,
			source:        "on-chain",
		}, nil
	}
	return redeemDecision{
		winnerOutcome: winnerLabel,
		resolved:      true,
		source:        "on-chain",
		reason:        redeemSkipOnlyLosingBalance,
	}, nil
}

func marketOutcomes(market api.Market) []string {
	outcomes := make([]string, 0, len(market.Tokens))
	for _, token := range market.Tokens {
		outcomes = append(outcomes, token.Outcome)
	}
	return outcomes
}

func hasWinningBalance(winnerOutcome string, outcomes []string, balances []float64) bool {
	for i, outcome := range outcomes {
		if strings.EqualFold(strings.TrimSpace(outcome), strings.TrimSpace(winnerOutcome)) && i < len(balances) && balances[i] >= minOnChainActionShares {
			return true
		}
	}
	return false
}

func isSkippableRedeemError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "market not yet resolved on-chain") ||
		strings.Contains(msg, "payouts not reported") ||
		strings.Contains(msg, "reverted on-chain") ||
		strings.Contains(msg, "execution reverted")
}
