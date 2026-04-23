package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"Market-bot/internal/api"
	mkt "Market-bot/internal/markets"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

func realbotNormalizeTrackedShares(qty float64) float64 {
	if !hasActionableCleanupRemainder(qty) {
		return 0
	}
	return qty
}

func realbotRecordWalletTruthAdjustment(tui *paper.TUI, marketID, outcome string, deltaShares, localShares, onChainShares, splitShares, markPrice float64, action string) {
	/*
		if tui == nil || math.Abs(deltaShares) < realbotWalletTruthLogMinDelta {
			return
		}
		side := "ADJ"
		sign := ""
		if deltaShares > 0 {
			side = "ADJ+"
			sign = "+"
		} else if deltaShares < 0 {
			side = "ADJ-"
		}
		tui.RecordWalletSyncAdjustment(marketID, outcome, deltaShares, markPrice, side)
		if math.Abs(deltaShares) < realbotWalletTruthEventMinDelta {
			return
		}
		// Silenced per user request
		// tui.LogEvent("[%s] 🧾 Wallet sync %s %s %s%s (local %.4f, on-chain %.4f, split %.4f)",
		// 	marketID,
		// 	action,
		// 	outcome,
		// 	sign,
		// 	formatShareQty(math.Abs(deltaShares)),
		// 	localShares,
		// 	onChainShares,
		// 	splitShares,
		// )
	*/
}

func syncWalletTruthOutcomePosition(engine *paper.Engine, tui *paper.TUI, marketID, outcome string, localBoughtShares, onChainShares, splitShares float64) (float64, bool) {
	desiredBoughtShares := realbotNormalizeTrackedShares(math.Max(0, onChainShares-splitShares))
	deltaShares := desiredBoughtShares - localBoughtShares
	if math.Abs(deltaShares) <= 1e-6 {
		return desiredBoughtShares, false
	}

	markPrice := walletTruthSyncMarkPrice(engine, marketID, outcome)
	if !engine.SyncExternalPosition(marketID, outcome, desiredBoughtShares, markPrice) {
		return desiredBoughtShares, false
	}

	action := "restored"
	if deltaShares < 0 {
		action = "trimmed"
	}
	realbotRecordWalletTruthAdjustment(tui, marketID, outcome, deltaShares, localBoughtShares, onChainShares, splitShares, markPrice, action)
	return desiredBoughtShares, true
}

func syncWalletTruthPositions(ctx context.Context, marketID string, tokenToOutcome map[string]string, trader *trading.RealTrader, engine *paper.Engine, splitInventory *paper.SplitInventory, tui *paper.TUI) (bool, error) {
	enginePositions := engine.GetPositions()
	localByOutcome := make(map[string]float64)
	for _, pos := range enginePositions {
		if pos.MarketID != marketID {
			continue
		}
		localByOutcome[pos.Outcome] += pos.Quantity
	}

	positions := make([]paper.WalletTruthPosition, 0, len(tokenToOutcome))
	changed := false
	for tokenID, outcome := range tokenToOutcome {
		if tokenID == "" || outcome == "" {
			continue
		}
		onChainShares, err := trader.GetCTFBalanceFloat(ctx, tokenID)
		if err != nil {
			return changed, err
		}
		onChainShares = realbotNormalizeTrackedShares(onChainShares)
		localBoughtShares := localByOutcome[outcome]
		splitShares := 0.0
		if splitInventory != nil {
			splitShares = splitInventory.GetSplitShares(marketID, outcome)
		}
		var adjusted bool
		localBoughtShares, adjusted = syncWalletTruthOutcomePosition(engine, tui, marketID, outcome, localBoughtShares, onChainShares, splitShares+engine.GetSettledLoserShares(marketID, outcome))
		if adjusted {
			changed = true
		}
		localShares := realbotNormalizeTrackedShares(localBoughtShares + splitShares)
		positions = append(positions, paper.WalletTruthPosition{
			MarketID:      marketID,
			Outcome:       outcome,
			LocalShares:   localShares,
			OnChainShares: onChainShares,
			Drift:         onChainShares - localShares,
		})
	}
	sort.Slice(positions, func(i, j int) bool {
		if positions[i].MarketID == positions[j].MarketID {
			return positions[i].Outcome < positions[j].Outcome
		}
		return positions[i].MarketID < positions[j].MarketID
	})
	tui.SetWalletTruthPositions(marketID, positions)
	return changed, nil
}

func realbotReconcileTrackedRoundWalletTruth(ctx context.Context, markets map[string]*api.Market, trader *trading.RealTrader, engine *paper.Engine, splitInventories map[string]*paper.SplitInventory, splitMu *sync.Mutex, tui *paper.TUI) (int, error) {
	if trader == nil || engine == nil || len(markets) == 0 {
		return 0, nil
	}

	changedMarkets := 0
	var firstErr error

	for assetID, market := range markets {
		if market == nil {
			continue
		}

		tokenToOutcome := make(map[string]string)
		for _, token := range market.Tokens {
			if token.TokenID == "" || token.Outcome == "" {
				continue
			}
			tokenToOutcome[token.TokenID] = token.Outcome
		}
		if len(tokenToOutcome) == 0 {
			continue
		}

		marketID := mkt.ScopedMarketID(assetID, market)
		var splitInventory *paper.SplitInventory
		if splitMu != nil {
			splitMu.Lock()
			splitInventory = splitInventories[market.ConditionID]
			splitMu.Unlock()
		} else {
			splitInventory = splitInventories[market.ConditionID]
		}

		marketCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		changed, err := syncWalletTruthPositions(marketCtx, marketID, tokenToOutcome, trader, engine, splitInventory, tui)
		cancel()
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", marketID, err)
			}
			continue
		}
		if changed {
			changedMarkets++
		}
	}

	return changedMarkets, firstErr
}

func localBoughtPairBalances(engine *paper.Engine, marketID, outcome0, outcome1 string) (bal0, bal1 float64) {
	positions := engine.GetPositions()
	for _, pos := range positions {
		if pos.MarketID != marketID || pos.Quantity <= 0 {
			continue
		}
		switch pos.Outcome {
		case outcome0:
			bal0 += pos.Quantity
		case outcome1:
			bal1 += pos.Quantity
		}
	}
	return bal0, bal1
}

func pendingPairRecoveryBalances(ctx context.Context, marketID, token0, token1 string, outcomes []string, trader *trading.RealTrader, engine *paper.Engine, splitInventory *paper.SplitInventory) (bal0, bal1 float64, source string, err error) {
	if len(outcomes) != 2 {
		return 0, 0, "", nil
	}
	local0, local1 := localBoughtPairBalances(engine, marketID, outcomes[0], outcomes[1])
	if hasActionableCleanupRemainder(local0) || hasActionableCleanupRemainder(local1) {
		return local0, local1, "local engine", nil
	}
	onChain0, onChain1, err := loadPairOnChainBalances(ctx, trader, token0, token1)
	if err != nil {
		return 0, 0, "", err
	}
	split0, split1 := 0.0, 0.0
	if splitInventory != nil {
		split0 = splitInventory.GetSplitShares(marketID, outcomes[0])
		split1 = splitInventory.GetSplitShares(marketID, outcomes[1])
	}
	return math.Max(0, onChain0-split0), math.Max(0, onChain1-split1), "on-chain truth", nil
}

func walletTruthSyncMarkPrice(engine *paper.Engine, marketID, outcome string) float64 {
	bid, ask := engine.GetMarketBidAsk(marketID, outcome)
	if bid >= 0.01 {
		return bid
	}
	if ask >= 0.01 {
		return ask
	}
	return 0.50
}

func reconcileLocalBoughtPositionsToWalletTruth(ctx context.Context, marketID, token0, token1 string, outcomes []string, trader *trading.RealTrader, engine *paper.Engine, splitInventory *paper.SplitInventory, tui *paper.TUI) (bool, error) {
	if len(outcomes) != 2 {
		return false, nil
	}
	onChain0, onChain1, err := loadPairOnChainBalances(ctx, trader, token0, token1)
	if err != nil {
		return false, err
	}
	local0, local1 := localBoughtPairBalances(engine, marketID, outcomes[0], outcomes[1])
	split0, split1 := 0.0, 0.0
	if splitInventory != nil {
		split0 = splitInventory.GetSplitShares(marketID, outcomes[0])
		split1 = splitInventory.GetSplitShares(marketID, outcomes[1])
	}
	desired0 := math.Max(0, onChain0-split0)
	desired1 := math.Max(0, onChain1-split1)
	changed := false
	if local0 > desired0+1e-6 {
		trimQty := local0 - desired0
		markPrice := walletTruthSyncMarkPrice(engine, marketID, outcomes[0])
		if engine.SyncExternalPosition(marketID, outcomes[0], desired0, markPrice) {
			realbotRecordWalletTruthAdjustment(tui, marketID, outcomes[0], -trimQty, local0, onChain0, split0, markPrice, "trimmed")
			changed = true
		}
	} else if desired0 > local0+1e-6 {
		addQty := desired0 - local0
		markPrice := walletTruthSyncMarkPrice(engine, marketID, outcomes[0])
		if engine.SyncExternalPosition(marketID, outcomes[0], desired0, markPrice) {
			realbotRecordWalletTruthAdjustment(tui, marketID, outcomes[0], addQty, local0, onChain0, split0, markPrice, "restored")
			changed = true
		}
	}
	if local1 > desired1+1e-6 {
		trimQty := local1 - desired1
		markPrice := walletTruthSyncMarkPrice(engine, marketID, outcomes[1])
		if engine.SyncExternalPosition(marketID, outcomes[1], desired1, markPrice) {
			realbotRecordWalletTruthAdjustment(tui, marketID, outcomes[1], -trimQty, local1, onChain1, split1, markPrice, "trimmed")
			changed = true
		}
	} else if desired1 > local1+1e-6 {
		addQty := desired1 - local1
		markPrice := walletTruthSyncMarkPrice(engine, marketID, outcomes[1])
		if engine.SyncExternalPosition(marketID, outcomes[1], desired1, markPrice) {
			realbotRecordWalletTruthAdjustment(tui, marketID, outcomes[1], addQty, local1, onChain1, split1, markPrice, "restored")
			changed = true
		}
	}
	return changed, nil
}

func mergeBalancedPositionWSFirst(ctx context.Context, trader *trading.RealTrader, conditionID, token0, token1 string, requestedQty float64, numOutcomes int) (mergeQty, settled0, settled1 float64, txHash string, err error) {
	if requestedQty < minOnChainActionShares {
		return 0, 0, 0, "", fmt.Errorf("merge skipped: %.6f shares is below %.2f minimum", requestedQty, minOnChainActionShares)
	}

	settled0, settled1, err0, err1 := trader.QueryBalancedCTFBalances(ctx, token0, token1, requestedQty)
	if err0 != nil || err1 != nil {
		return 0, settled0, settled1, "", fmt.Errorf("on-chain settlement check failed (err0=%v err1=%v)", err0, err1)
	}

	mergeQty = math.Min(math.Min(settled0, settled1), requestedQty)
	if mergeQty < minOnChainActionShares {
		return 0, settled0, settled1, "", fmt.Errorf("merge skipped: settled balanced size %.6f is below %.2f minimum", mergeQty, minOnChainActionShares)
	}

	txHash, err = trader.MergeOnChain(ctx, conditionID, mergeQty, numOutcomes)
	if err != nil {
		return 0, settled0, settled1, txHash, err
	}
	return mergeQty, settled0, settled1, txHash, nil
}
