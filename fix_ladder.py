import sys

with open("cmd/realbot/main.go", "r") as f:
    content = f.read()

# 1. Replace requestSize initialization
old_request_size = """						requestSize1 := shares
						requestSize2 := shares
						if ladderedMode {
							requestSize1, requestSize2 = ladderedTakerBiasedBuyShares(shares, ask1, ask2)
						}
						if requestSize1 < minEntryShares || requestSize2 < minEntryShares {
							tui.LogEvent("[%s] ⚠️ Actionable laddered legs below %.2f share minimum: %s/%s", id, minEntryShares, formatShareQty(requestSize1), formatShareQty(requestSize2))
							continue
						}"""

new_request_size = """						ladderedDirection := -1
						if ladderedMode {
							var directionalReady bool
							ladderedDirection, directionalReady = ladderedTakerDirectionalSide(ladderedEntries, ask1, ask2, realbotCfg.LadderedTakerReentryMoveCents)
							if !directionalReady {
								continue
							}
						}

						requestSize1 := shares
						requestSize2 := shares
						if ladderedMode {
							requestSize1, requestSize2 = 0, 0
							if ladderedDirection == 1 {
								requestSize2 = normalizeMarketBuyShares(shares)
							} else {
								requestSize1 = normalizeMarketBuyShares(shares)
							}
							activeSize := requestSize1
							if ladderedDirection == 1 {
								activeSize = requestSize2
							}
							if activeSize < minEntryShares {
								tui.LogEvent("[%s] ⚠️ Actionable laddered leg below %.2f share minimum: %s", id, minEntryShares, formatShareQty(activeSize))
								continue
							}
						} else {
							if requestSize1 < minEntryShares || requestSize2 < minEntryShares {
								tui.LogEvent("[%s] ⚠️ Actionable arb legs below %.2f share minimum: %s/%s", id, minEntryShares, formatShareQty(requestSize1), formatShareQty(requestSize2))
								continue
							}
						}"""

content = content.replace(old_request_size, new_request_size)

# 2. Replace budget check for laddered
old_budget_check = """						if ladderedMode {
							estimatedCost := (requestSize1 * limitPrice1) + (requestSize2 * limitPrice2)
							if estimatedCost > currentBalance && estimatedCost > 0 {
								scale := currentBalance / estimatedCost
								requestSize1 = normalizeMarketBuyShares(requestSize1 * scale)
								requestSize2 = normalizeMarketBuyShares(requestSize2 * scale)
							}
							if requestSize1 < minEntryShares || requestSize2 < minEntryShares {
								tui.LogEvent("[%s] ⚠️ Skipping buy: laddered legs no longer fit balance (%s/%s)", id, formatShareQty(requestSize1), formatShareQty(requestSize2))
								continue
							}"""

new_budget_check = """						if ladderedMode {
							estimatedCost := requestSize1 * limitPrice1
							if ladderedDirection == 1 {
								estimatedCost = requestSize2 * limitPrice2
							}
							if estimatedCost > currentBalance && estimatedCost > 0 {
								scale := currentBalance / estimatedCost
								if ladderedDirection == 1 {
									requestSize2 = normalizeMarketBuyShares(requestSize2 * scale)
								} else {
									requestSize1 = normalizeMarketBuyShares(requestSize1 * scale)
								}
							}
							activeSize := requestSize1
							if ladderedDirection == 1 {
								activeSize = requestSize2
							}
							if activeSize < minEntryShares {
								tui.LogEvent("[%s] ⚠️ Skipping buy: laddered leg no longer fits balance (%s)", id, formatShareQty(activeSize))
								continue
							}"""

content = content.replace(old_budget_check, new_budget_check)

# 3. Replace batchExecs submission
old_batch = """						batchExecs := executeMarketOrderBatchWithSignals(ctx, trader, []directMarketOrderSignalRequest{
							{
								Side:           api.SideBuy,
								TokenID:        token0,
								Outcome:        outcomes[0],
								Price:          limitPrice1,
								Size:           requestSize1,
								FeeRateBps:     rate1,
								InitialBalance: initialBal0,
							},
							{
								Side:           api.SideBuy,
								TokenID:        token1,
								Outcome:        outcomes[1],
								Price:          limitPrice2,
								Size:           requestSize2,
								FeeRateBps:     rate2,
								InitialBalance: initialBal1,
							},
						}, 2*time.Second)
						exec1, exec2 := batchExecs[0], batchExecs[1]"""

new_batch = """						var requests []directMarketOrderSignalRequest
						if !ladderedMode || ladderedDirection == 0 {
							requests = append(requests, directMarketOrderSignalRequest{
								Side:           api.SideBuy,
								TokenID:        token0,
								Outcome:        outcomes[0],
								Price:          limitPrice1,
								Size:           requestSize1,
								FeeRateBps:     rate1,
								InitialBalance: initialBal0,
							})
						}
						if !ladderedMode || ladderedDirection == 1 {
							requests = append(requests, directMarketOrderSignalRequest{
								Side:           api.SideBuy,
								TokenID:        token1,
								Outcome:        outcomes[1],
								Price:          limitPrice2,
								Size:           requestSize2,
								FeeRateBps:     rate2,
								InitialBalance: initialBal1,
							})
						}

						batchExecs := executeMarketOrderBatchWithSignals(ctx, trader, requests, 2*time.Second)
						var exec1, exec2 directMarketExecution
						if ladderedMode {
							if ladderedDirection == 0 {
								exec1 = batchExecs[0]
								exec2 = directMarketExecution{Success: false}
							} else {
								exec1 = directMarketExecution{Success: false}
								exec2 = batchExecs[0]
							}
						} else {
							exec1, exec2 = batchExecs[0], batchExecs[1]
						}"""

content = content.replace(old_batch, new_batch)

# 4. Replace Legged check
old_legged = """						if side1Success != side2Success {
							if haveInitialSnapshot {
								tui.LogEvent("[%s] 🧾 Pre-trade share snapshot (%s): %s=%.4f, %s=%.4f", id, initialSnapshotSource, outcomes[0], initialSnapshot0, outcomes[1], initialSnapshot1)
							}
							tui.LogEvent("[%s] ⚠️ ARB LEGGED: %s=%v %s=%v — waiting 2s then re-verifying...",
								id, outcomes[0], side1Success, outcomes[1], side2Success)"""

new_legged = """						if !ladderedMode && side1Success != side2Success {
							if haveInitialSnapshot {
								tui.LogEvent("[%s] 🧾 Pre-trade share snapshot (%s): %s=%.4f, %s=%.4f", id, initialSnapshotSource, outcomes[0], initialSnapshot0, outcomes[1], initialSnapshot1)
							}
							tui.LogEvent("[%s] ⚠️ ARB LEGGED: %s=%v %s=%v — waiting 2s then re-verifying...",
								id, outcomes[0], side1Success, outcomes[1], side2Success)"""
content = content.replace(old_legged, new_legged)

# 5. Replace Recording to Engine
old_recording = """						if side1Success && side2Success {
							// Both sides filled (either initially or via recovery) - record both
							_, _ = engine.BuyForMarket(id, outcomes[0], ask1, filled1)
							_, _ = engine.BuyForMarket(id, outcomes[1], ask2, filled2)

							if arbMode == paperArbModeLaddered {
								tui.LogEvent("[%s] 🪜 Laddered taker inventory added: %s=%s, %s=%s", id, outcomes[0], formatShareQty(filled1), outcomes[1], formatShareQty(filled2))
								ladderedEntries = append(ladderedEntries, struct{ ask0, ask1 float64 }{ask1, ask2})
								if newBal, err := trader.ForceRefreshBalance(ctx); err == nil {
									currentBalance = newBal
									engine.SyncBalanceNeutral(currentBalance)
									engine.RecalculateDrawdown()
								}
								refreshWalletTruth(5 * time.Second)
							} else {"""

new_recording = """						if ladderedMode {
							if ladderedDirection == 0 && side1Success {
								_, _ = engine.BuyForMarket(id, outcomes[0], ask1, filled1)
								tui.LogEvent("[%s] 🪜 Laddered taker inventory added: %s=%s", id, outcomes[0], formatShareQty(filled1))
								ladderedEntries = append(ladderedEntries, struct{ ask0, ask1 float64 }{ask1, ask2})
								if newBal, err := trader.ForceRefreshBalance(ctx); err == nil {
									currentBalance = newBal
									engine.SyncBalanceNeutral(currentBalance)
									engine.RecalculateDrawdown()
								}
								refreshWalletTruth(5 * time.Second)
							} else if ladderedDirection == 1 && side2Success {
								_, _ = engine.BuyForMarket(id, outcomes[1], ask2, filled2)
								tui.LogEvent("[%s] 🪜 Laddered taker inventory added: %s=%s", id, outcomes[1], formatShareQty(filled2))
								ladderedEntries = append(ladderedEntries, struct{ ask0, ask1 float64 }{ask1, ask2})
								if newBal, err := trader.ForceRefreshBalance(ctx); err == nil {
									currentBalance = newBal
									engine.SyncBalanceNeutral(currentBalance)
									engine.RecalculateDrawdown()
								}
								refreshWalletTruth(5 * time.Second)
							}
						} else if side1Success && side2Success {
							// Both sides filled (either initially or via recovery) - record both
							_, _ = engine.BuyForMarket(id, outcomes[0], ask1, filled1)
							_, _ = engine.BuyForMarket(id, outcomes[1], ask2, filled2)

							if false { // replaced block
							} else {"""
content = content.replace(old_recording, new_recording)

# 6. Add ladderedTakerDirectionalSide to the bottom of the file
ladder_dir = """
func ladderedTakerDirectionalSide(entries []struct{ ask0, ask1 float64 }, ask0, ask1, moveCents float64) (int, bool) {
	if len(entries) == 0 {
		switch {
		case ask0 > ask1+1e-9:
			return 0, true
		case ask1 > ask0+1e-9:
			return 1, true
		default:
			return -1, false
		}
	}
	lastAsk0 := entries[len(entries)-1].ask0
	lastAsk1 := entries[len(entries)-1].ask1
	threshold := moveCents
	switch {
	case threshold <= 0:
		threshold = 1.0
	case threshold < 0.1:
		threshold = 0.1
	case threshold > 25.0:
		threshold = 25.0
	}
	threshold = threshold / 100.0

	move0 := ask0 - lastAsk0
	move1 := ask1 - lastAsk1
	switch {
	case move0 >= threshold-1e-9 && move0 > move1+1e-9:
		return 0, true
	case move1 >= threshold-1e-9 && move1 > move0+1e-9:
		return 1, true
	case move0 >= threshold-1e-9 && move1 >= threshold-1e-9:
		if ask0 > ask1+1e-9 {
			return 0, true
		}
		if ask1 > ask0+1e-9 {
			return 1, true
		}
	}
	return -1, false
}
"""
if "func ladderedTakerDirectionalSide" not in content:
    content += ladder_dir

with open("cmd/realbot/main.go", "w") as f:
    f.write(content)
