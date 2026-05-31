package main

import (
	"context"
	"sort"
	"strings"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

func realbotWinningOnChainShares(positions []paper.WalletTruthPosition, winner string) float64 {
	if winner == "" {
		return 0
	}
	total := 0.0
	for _, pos := range positions {
		if strings.EqualFold(pos.Outcome, winner) && hasActionableCleanupRemainder(pos.OnChainShares) {
			total += pos.OnChainShares
		}
	}
	return total
}

func realbotWalletTruthPositionsForRedemption(ctx context.Context, marketID, conditionID string, trader *trading.RealTrader, engine *paper.Engine) ([]paper.WalletTruthPosition, error) {
	return realbotWalletTruthPositionsForRedemptionWithRefresh(ctx, marketID, conditionID, trader, engine, false)
}

func realbotWalletTruthPositionsForRedemptionFresh(ctx context.Context, marketID, conditionID string, trader *trading.RealTrader, engine *paper.Engine) ([]paper.WalletTruthPosition, error) {
	return realbotWalletTruthPositionsForRedemptionWithRefresh(ctx, marketID, conditionID, trader, engine, true)
}

func realbotWalletTruthPositionsForRedemptionWithRefresh(ctx context.Context, marketID, conditionID string, trader *trading.RealTrader, engine *paper.Engine, forceRefresh bool) ([]paper.WalletTruthPosition, error) {
	if trader == nil || engine == nil || marketID == "" || conditionID == "" {
		return nil, nil
	}

	info, err := trader.GetMarketInfo(ctx, conditionID)
	if err != nil {
		return nil, err
	}

	localByOutcome := make(map[string]float64)
	for _, pos := range engine.GetPositions() {
		if pos.MarketID != marketID || pos.Quantity <= 0 {
			continue
		}
		localByOutcome[realbotRedemptionOutcomeKey(pos.Outcome)] += pos.Quantity
	}

	positions := make([]paper.WalletTruthPosition, 0, len(info.Tokens))
	for _, token := range info.Tokens {
		if token.TokenID == "" || token.Outcome == "" {
			continue
		}
		var onChainShares float64
		var err error
		if forceRefresh {
			onChainShares, err = trader.ForceRefreshCTFBalanceFloat(ctx, token.TokenID)
		} else {
			onChainShares, err = trader.GetCTFBalanceFloat(ctx, token.TokenID)
		}
		if err != nil {
			return nil, err
		}
		onChainShares = realbotNormalizeTrackedShares(onChainShares)
		localShares := realbotNormalizeTrackedShares(localByOutcome[realbotRedemptionOutcomeKey(token.Outcome)])
		if localShares <= 0 && onChainShares <= 0 {
			continue
		}
		positions = append(positions, paper.WalletTruthPosition{
			MarketID:      marketID,
			Outcome:       token.Outcome,
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
	return positions, nil
}

func realbotRedemptionOutcomeKey(outcome string) string {
	return strings.ToLower(strings.TrimSpace(outcome))
}

func realbotSyncEngineToWalletTruthForResolution(engine *paper.Engine, marketID, winner string, positions []paper.WalletTruthPosition) (adjusted int, missingCostBasis []string) {
	if engine == nil || marketID == "" {
		return 0, nil
	}
	enginePositions := engine.GetPositions()
	winnerKey := realbotRedemptionOutcomeKey(winner)
	for _, wt := range positions {
		if wt.MarketID != marketID {
			continue
		}
		key := marketID + ":" + wt.Outcome
		pos, exists := enginePositions[key]
		outcomeKey := realbotRedemptionOutcomeKey(wt.Outcome)
		localShares := wt.LocalShares
		if exists && pos.Quantity > localShares {
			localShares = pos.Quantity
		}
		markPrice := 0.0
		if exists {
			markPrice = pos.AvgPrice
			if markPrice <= 0 && pos.Quantity > 0 {
				markPrice = pos.TotalCost / pos.Quantity
			}
		}
		if markPrice <= 0 {
			markPrice = 0.5
		}
		if !hasActionableCleanupRemainder(wt.OnChainShares) {
			if winnerKey != "" && outcomeKey == winnerKey {
				if hasActionableCleanupRemainder(localShares) {
					// A zero winning CTF balance at resolution can mean the redeem
					// transaction already cleared before this poll. Keep the local
					// winning lot so RedeemWithDetails books the payout instead of
					// turning the buy cost into a loss.
					if !exists || pos.Quantity <= 0 {
						missingCostBasis = append(missingCostBasis, wt.Outcome)
						if engine.SyncExternalPosition(marketID, wt.Outcome, localShares, 1.0) {
							adjusted++
						}
					}
					continue
				}
			}
			if winnerKey != "" && outcomeKey != winnerKey && hasActionableCleanupRemainder(localShares) {
				settledLoserShares := engine.GetSettledLoserShares(marketID, wt.Outcome)
				if settledLoserShares+1e-6 < localShares {
					// Losing ladder outcomes can already read as zero on-chain once the
					// market resolves. Keep the local lot so redemption books the loss
					// unless we already marked it settled earlier.
					continue
				}
			}
			if exists || hasActionableCleanupRemainder(wt.LocalShares) {
				if engine.SyncExternalPosition(marketID, wt.Outcome, 0, markPrice) {
					adjusted++
				}
			}
			continue
		}
		if !exists || pos.Quantity <= 0 {
			missingCostBasis = append(missingCostBasis, wt.Outcome)
			if winnerKey != "" && outcomeKey == winnerKey {
				// If the wallet holds winning shares that the local engine did not
				// cost, book them at payout value so redemption accounting recognizes
				// the receivable without inventing PnL.
				if engine.SyncExternalPosition(marketID, wt.Outcome, wt.OnChainShares, 1.0) {
					adjusted++
				}
			}
			continue
		}
		if engine.SyncExternalPosition(marketID, wt.Outcome, wt.OnChainShares, markPrice) {
			adjusted++
		}
	}
	sort.Strings(missingCostBasis)
	return adjusted, missingCostBasis
}

func refreshWalletTruthForRedemption(ctx context.Context, marketID, conditionID string, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI) error {
	positions, err := realbotWalletTruthPositionsForRedemption(ctx, marketID, conditionID, trader, engine)
	if err != nil {
		return err
	}
	tui.SetWalletTruthPositions(marketID, positions)
	return nil
}

func refreshWalletTruthForRedemptionFresh(ctx context.Context, marketID, conditionID string, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI) ([]paper.WalletTruthPosition, error) {
	positions, err := realbotWalletTruthPositionsForRedemptionFresh(ctx, marketID, conditionID, trader, engine)
	if err != nil {
		return nil, err
	}
	tui.SetWalletTruthPositions(marketID, positions)
	return positions, nil
}

func realbotShortTxHash(txHash string) string {
	txHash = strings.TrimSpace(txHash)
	if len(txHash) > 10 {
		return txHash[:10] + "..."
	}
	return txHash
}

func realbotShouldKeepPendingRedeemTx(txHash string, err error) bool {
	if strings.TrimSpace(txHash) == "" || err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "confirmation pending") || strings.Contains(errStr, "timeout waiting for transaction")
}

func realbotFinalizePendingRedemption(engine *paper.Engine, marketID string, payoutAlreadyInBalance bool) float64 {
	if engine == nil || strings.TrimSpace(marketID) == "" {
		return 0
	}

	payout := engine.GetPendingRedemptions()[marketID]
	if payout <= 0.000001 {
		engine.ClearPendingRedemption(marketID)
		return 0
	}

	// If wallet cash has already been synced upward to include the redemption,
	// only clear the receivable marker. Settling here would add the payout twice.
	if payoutAlreadyInBalance {
		engine.ClearPendingRedemption(marketID)
		return payout
	}

	return engine.SettlePendingRedemption(marketID)
}

func realbotApplyRedeemCashCorrection(engine *paper.Engine, tui *paper.TUI, marketID string, redeemStartBalance, expectedPayout, observedBalance float64, applied *float64) float64 {
	if engine == nil || strings.TrimSpace(marketID) == "" {
		return 0
	}
	if realbotHasActionableEnginePositionsForMarket(engine, marketID) {
		return 0
	}
	if expectedPayout < 0 {
		expectedPayout = 0
	}
	correction := observedBalance - (redeemStartBalance + expectedPayout)
	if applied != nil {
		correction -= *applied
	}
	if correction <= 0.005 {
		return 0
	}

	engine.AdjustRealizedPnL(correction)
	if tui != nil {
		tui.AmendMostRecentRoundForMarket(marketID, correction, nil)
		tui.LogEvent("[%s] 🧾 Live cash reconciliation: adjusted realized PnL by +$%.2f after wallet balance settled above expected redeem payout", marketID, correction)
	}
	if applied != nil {
		*applied += correction
	}
	return correction
}

func launchRealbotRedeemRetryLoop(ctx context.Context, marketID, conditionID, winner string, numOutcomes int, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI) {
	go func() {
		attempt := 0
		pendingTxHash := ""
		lastConfirmedAt := time.Time{}
		redeemStartBalance := engine.GetBalance()
		expectedRedeemPayout := engine.GetPendingRedemptions()[marketID]
		appliedCashCorrection := 0.0
		highestObservedBalance := redeemStartBalance
		winnerDepletedAt := time.Time{}
		for {
			if ctx.Err() != nil {
				return
			}
			attempt++
			skipSubmit := false

			if pendingTxHash != "" {
				probeCtx, probeCancel := context.WithTimeout(ctx, realbotRedeemProbeTimeout)
				txState, probeErr := trader.GetOnChainTxState(probeCtx, pendingTxHash)
				probeCancel()

				if probeErr != nil {
					tui.LogEvent("[%s] ⚠️ Redeem tx %s probe failed: %v", marketID, realbotShortTxHash(pendingTxHash), probeErr)
					skipSubmit = true
				} else {
					switch txState {
					case "success":
						tui.LogEvent("[%s] ✅ Redeem tx confirmed: %s", marketID, realbotShortTxHash(pendingTxHash))
						lastConfirmedAt = time.Now()
						pendingTxHash = ""
						skipSubmit = true
					case "reverted":
						tui.LogEvent("[%s] ⚠️ Redeem tx reverted on-chain: %s", marketID, realbotShortTxHash(pendingTxHash))
						pendingTxHash = ""
					case "dropped":
						tui.LogEvent("[%s] ⚠️ Redeem tx dropped from RPC: %s", marketID, realbotShortTxHash(pendingTxHash))
						pendingTxHash = ""
					default:
						tui.LogEvent("[%s] ⏳ Redeem tx still pending: %s", marketID, realbotShortTxHash(pendingTxHash))
						skipSubmit = true
					}
				}
			}
			if pendingTxHash == "" && !lastConfirmedAt.IsZero() && time.Since(lastConfirmedAt) < 15*time.Second {
				skipSubmit = true
			}

			if !skipSubmit && pendingTxHash == "" {
				redeemCtx, cancel := context.WithTimeout(ctx, realbotRedeemSubmitTimeout)
				gasMode := core.RedeemGasModeFast
				if tui != nil {
					gasMode = realbotNormalizeRedeemGasMode(tui.GetSettings().RedeemGasMode)
				}
				txHash, err := trader.SubmitRedeemOnChainForceWithGasMode(redeemCtx, conditionID, numOutcomes, gasMode)
				cancel()

				if err == nil {
					pendingTxHash = txHash
					tui.LogEvent("[%s] ⏳ Redeem attempt %d submitted (%s gas): %s", marketID, attempt, gasMode, realbotShortTxHash(txHash))
				} else if realbotShouldKeepPendingRedeemTx(txHash, err) {
					pendingTxHash = txHash
					tui.LogEvent("[%s] ⏳ Redeem attempt %d submitted (%s gas), waiting on-chain: %s", marketID, attempt, gasMode, realbotShortTxHash(txHash))
				} else {
					tui.LogEvent("[%s] ⚠️ Redeem attempt %d failed: %v", marketID, attempt, err)
				}
			}

			refreshCtx, refreshCancel := context.WithTimeout(ctx, 20*time.Second)
			positions, refreshErr := refreshWalletTruthForRedemptionFresh(refreshCtx, marketID, conditionID, trader, engine, tui)
			refreshCancel()

			if refreshErr != nil {
				tui.LogEvent("[%s] ⚠️ Post-redeem wallet-truth refresh failed: %v", marketID, refreshErr)
			} else {
				tui.UpdateWalletTruthResolution(marketID, true, winner)
			}

			balanceCtx, balanceCancel := context.WithTimeout(ctx, 10*time.Second)
			if newBal, balErr := trader.ForceRefreshBalance(balanceCtx); balErr != nil {
				tui.LogEvent("[%s] ⚠️ Post-redeem balance refresh failed: %v", marketID, balErr)
			} else {
				if newBal > highestObservedBalance {
					highestObservedBalance = newBal
				}
				realbotApplyRuntimeBalanceSync(engine, tui, newBal)
				realbotApplyRedeemCashCorrection(engine, tui, marketID, redeemStartBalance, expectedRedeemPayout, highestObservedBalance, &appliedCashCorrection)
				if walletCash, cashErr := trader.ForceRefreshOnChainUSDCBalance(balanceCtx); cashErr == nil {
					tui.SetWalletCash(walletCash)
				}
			}
			balanceCancel()

			if refreshErr == nil && realbotWinningOnChainShares(positions, winner) <= 0.000001 {
				if winnerDepletedAt.IsZero() {
					winnerDepletedAt = time.Now()
				}
				pendingPayout := engine.GetPendingRedemptions()[marketID]
				if pendingPayout <= 0.000001 {
					tui.LogEvent("[%s] ✅ Redeem settled; winning shares cleared on-chain", marketID)
					return
				}
				cashReflectsPayout := highestObservedBalance+0.01 >= redeemStartBalance+pendingPayout
				waitedLongEnough := time.Since(winnerDepletedAt) >= 45*time.Second
				if cashReflectsPayout || waitedLongEnough {
					if !cashReflectsPayout {
						tui.LogEvent("[%s] ⚠️ Clearing pending redemption after timeout without explicit balance confirmation (start=%.2f peak=%.2f expected +%.2f)",
							marketID, redeemStartBalance, highestObservedBalance, pendingPayout)
					}
					realbotFinalizePendingRedemption(engine, marketID, cashReflectsPayout)
					return
				}
				if time.Since(winnerDepletedAt) <= realbotRedeemRetryInterval {
					tui.LogEvent("[%s] ⏳ Winner shares depleted; waiting for wallet USDC to reflect redemption before clearing pending payout (+%.2f expected)", marketID, pendingPayout)
				}
			}

			if !watchRealbotSleep(ctx, realbotRedeemRetryInterval) {
				return
			}
		}
	}()
}

func checkRedemption(ctx context.Context, id, conditionID string, outcomes []string, marketEndTime time.Time, trader *trading.RealTrader, engine *paper.Engine, tui *paper.TUI, resCache *api.ResolutionCache) {
	if trader == nil {
		return
	}

	// Fast-path: exit immediately if the bot has no actionable positions and no pending payout.
	pendingPayout := 0.0
	if engine != nil {
		pendingPayout = engine.GetPendingRedemptions()[id]
	}
	if !realbotHasActionableEnginePositionsForMarket(engine, id) && pendingPayout <= 0.000001 {
		return
	}

	// Detect timeframe from the market ID slug (e.g. BTC-15m-1715878400)
	is5m := strings.Contains(strings.ToLower(id), "5m")
	is15m := strings.Contains(strings.ToLower(id), "15m")
	is1h := strings.Contains(strings.ToLower(id), "1h")
	_ = is5m
	_ = is15m

	// Set pollInterval according to the timeframe:
	// - 5m and 15m markets fallback to check every 1 minute instead of 5 minutes
	// - 1h markets check every 30 seconds
	// - default fallback is 1 minute
	pollInterval := 1 * time.Minute
	if is1h {
		pollInterval = 30 * time.Second
	}

	shouldSkipCheck := func() bool {
		if is1h && time.Since(marketEndTime) < 10*time.Minute {
			return true
		}
		return false
	}

	if trader.IsEmbeddedPaperMode() {
		pendingPayout := 0.0
		if engine != nil {
			pendingPayout = engine.GetPendingRedemptions()[id]
		}
		if !realbotHasActionableEnginePositionsForMarket(engine, id) && pendingPayout <= 0.000001 {
			return
		}
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		pendingLogged := false
		for {
			if resCache != nil && !shouldSkipCheck() {
				status := resCache.GetResolution(ctx, conditionID, outcomes, marketEndTime)
				if status.Error != nil {
					tui.LogEvent("[%s] ⚠️ Paper resolution check failed: %v", id, status.Error)
				} else if status.Winner != "" {
					var result *paper.RedemptionResult
					if realbotHasActionableEnginePositionsForMarket(engine, id) {
						result = engine.RedeemWithDetails(id, status.Winner)
					}
					settled := engine.SettlePendingRedemption(id)
					tui.UpdateWalletTruthResolution(id, true, status.Winner)
					if settled > 0 {
						tui.SetWalletCash(engine.GetBalance())
						tui.LogEvent("[%s] 💸 Paper redeem settled: +$%.2f", id, settled)
					}
					if result != nil && (result.WinningShares > 0 || result.LosingShares > 0 || result.TotalPayout > 0 || result.TotalPnL != 0) {
						tui.AmendMostRecentRoundForMarket(id, result.TotalPnL, []*paper.RedemptionResult{result})
						tui.LogEvent("[%s] 💰 PAPER RESOLVED: %s won | PnL: %+0.2f", id, status.Winner, result.TotalPnL)
					} else if settled > 0 {
						tui.LogEvent("[%s] 📭 PAPER RESOLVED: %s (settlement cleared with no local redemption delta)", id, status.Winner)
					} else {
						tui.LogEvent("[%s] 📭 PAPER RESOLVED: %s (no redeemable local inventory)", id, status.Winner)
					}
					return
				}
			}
			tui.UpdateWalletTruthResolution(id, false, "")
			if !pendingLogged {
				if is1h && time.Since(marketEndTime) < 10*time.Minute {
					tui.LogEvent("[%s] ⏳ Paper resolution pending (skipping check for the first 10m after close)...", id)
				} else {
					tui.LogEvent("[%s] ⏳ Paper resolution pending...", id)
				}
				pendingLogged = true
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}
	if err := refreshWalletTruthForRedemption(ctx, id, conditionID, trader, engine, tui); err != nil {
		if !realbotHasActionableEnginePositionsForMarket(engine, id) {
			return
		}
		tui.LogEvent("[%s] ⚠️ Initial redemption wallet-truth refresh failed: %v", id, err)
	} else {
		positions, refreshErr := realbotWalletTruthPositionsForRedemption(ctx, id, conditionID, trader, engine)
		if refreshErr == nil && len(positions) == 0 {
			return
		}
	}

	numOutcomes := len(outcomes)

	wsResCh := make(chan struct{}, 1)
	if globalResWatcher != nil {
		unregister := globalResWatcher.RegisterCallback(func(eventCondID string) {
			if strings.EqualFold(eventCondID, conditionID) {
				select {
				case wsResCh <- struct{}{}:
				default:
				}
			}
		})
		defer unregister()
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	checkRound := 0
	lastResolutionState := ""

	for {
		triggeredByWS := false
		if checkRound > 0 {
			select {
			case <-ctx.Done():
				return
			case <-wsResCh:
				tui.LogEvent("[%s] ⚡ WebSocket: ConditionResolution event detected on-chain!", id)
				triggeredByWS = true
			case <-ticker.C:
			}
		}
		checkRound++

		resolved := false
		winner := ""
		if resCache != nil && (triggeredByWS || !shouldSkipCheck()) {
			status := resCache.GetResolution(ctx, conditionID, outcomes, marketEndTime)
			if status.Error != nil {
				tui.LogEvent("[%s] ⚠️ Resolution check failed: %v", id, status.Error)
			}
			resolved = status.Resolved
			winner = status.Winner
		}

		if numOutcomes == 0 || winner == "" {
			if triggeredByWS || !shouldSkipCheck() {
				info, err := trader.GetMarketInfo(ctx, conditionID)
				if err != nil {
					if !resolved {
						tui.LogEvent("[%s] ⚠️ Resolution check failed: %v", id, err)
						continue
					}
				} else {
					if len(info.Tokens) > numOutcomes {
						numOutcomes = len(info.Tokens)
					}
					for _, token := range info.Tokens {
						if token.Winner {
							winner = token.Outcome
							break
						}
					}
				}
			}
		}

		if winner != "" {
			walletTruthWinningShares := 0.0
			missingCostBasis := []string(nil)
			if positions, positionsErr := realbotWalletTruthPositionsForRedemptionFresh(ctx, id, conditionID, trader, engine); positionsErr != nil {
				tui.LogEvent("[%s] ⚠️ Wallet-truth refresh before resolution settlement failed: %v", id, positionsErr)
			} else {
				walletTruthWinningShares = realbotWinningOnChainShares(positions, winner)
				tui.SetWalletTruthPositions(id, positions)
				if adjusted, missing := realbotSyncEngineToWalletTruthForResolution(engine, id, winner, positions); adjusted > 0 {
					tui.LogEvent("[%s] 🔄 Synced local resolution inventory to on-chain balances (%d outcomes adjusted)", id, adjusted)
				} else if len(missing) > 0 {
					missingCostBasis = append(missingCostBasis, missing...)
					tui.LogEvent("[%s] ⚠️ Resolution inventory drift detected with no local cost basis for: %s", id, strings.Join(missingCostBasis, ", "))
				}
			}
			result := engine.RedeemWithDetails(id, winner)
			if err := refreshWalletTruthForRedemption(ctx, id, conditionID, trader, engine, tui); err != nil {
				tui.LogEvent("[%s] ⚠️ Wallet-truth refresh after winner update failed: %v", id, err)
			}
			tui.UpdateWalletTruthResolution(id, true, winner)
			if result.WinningShares > 0 || result.LosingShares > 0 || result.TotalPayout > 0 || result.TotalPnL != 0 || walletTruthWinningShares > 0.000001 {
				pnlSign := "+"
				pnlEmoji := "💰"
				if result.TotalPnL < 0 {
					pnlSign = ""
					pnlEmoji = "💸"
				}
				if result.WinningShares > 0 || result.LosingShares > 0 || result.TotalPayout > 0 || result.TotalPnL != 0 {
					tui.AmendMostRecentRoundForMarket(id, result.TotalPnL, []*paper.RedemptionResult{result})
					tui.LogEvent("[%s] %s RESOLVED: %s won | PnL: %s$%.2f", id, pnlEmoji, winner, pnlSign, result.TotalPnL)
				} else {
					tui.LogEvent("[%s] ⏳ RESOLVED: %s won | wallet-truth redeemable %s shares (cost basis unavailable: %s)", id, winner, formatShareQty(walletTruthWinningShares), strings.Join(missingCostBasis, ", "))
				}

				if result.TotalPnL < 0 && trader != nil {
					trader.RecordLoss(-result.TotalPnL)
				}

				tui.LogEvent("[%s] ⏳ Starting forced on-chain redemption retry loop (every %s)...", id, realbotRedeemRetryInterval)
				launchRealbotRedeemRetryLoop(ctx, id, conditionID, winner, numOutcomes, trader, engine, tui)
			} else {
				tui.LogEvent("[%s] 📭 Market resolved: %s (no positions)", id, winner)
			}
			return
		}

		if resolved {
			if err := refreshWalletTruthForRedemption(ctx, id, conditionID, trader, engine, tui); err != nil {
				tui.LogEvent("[%s] ⚠️ Wallet-truth refresh during resolved-pending state failed: %v", id, err)
			}
			tui.UpdateWalletTruthResolution(id, true, "")
			if lastResolutionState != "resolved-pending-winner" {
				tui.LogEvent("[%s] ⏳ Market resolved on-chain, winner still pending...", id)
				lastResolutionState = "resolved-pending-winner"
			}
			continue
		}

		if err := refreshWalletTruthForRedemption(ctx, id, conditionID, trader, engine, tui); err != nil {
			tui.LogEvent("[%s] ⚠️ Wallet-truth refresh during pending resolution failed: %v", id, err)
		}
		tui.UpdateWalletTruthResolution(id, false, "")
		if lastResolutionState != "pending" {
			tui.LogEvent("[%s] ⏳ Resolution pending...", id)
			lastResolutionState = "pending"
		}
	}
}
