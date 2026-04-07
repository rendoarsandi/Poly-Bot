import sys

# For realbot
with open("cmd/realbot/main.go", "r") as f:
    content = f.read()

# 1. Remove old cost check
old_cost = """					cost := strategy.CalculateTradeMetricsFlat(shares, maxExecutionSum, maxFeeRateBps).Cost
					if ladderedMode {
						est0, est1 := ladderedTakerBiasedBuyShares(shares, ask1, ask2)
						cost = (est0 * ask1) + (est1 * ask2)
					}"""

new_cost = """					cost := strategy.CalculateTradeMetricsFlat(shares, maxExecutionSum, maxFeeRateBps).Cost
					if ladderedMode {
						if ask1 > ask2 {
							cost = shares * ask1
						} else {
							cost = shares * ask2
						}
					}"""
content = content.replace(old_cost, new_cost)

# 2. Remove redundant move threshold check
old_redundant = """					if ladderedMode && !ladderedTakerEntryMovedEnough(ladderedEntries, ask1, ask2, realbotCfg.LadderedTakerReentryMoveCents) {
						continue
					}"""
content = content.replace(old_redundant, "")

with open("cmd/realbot/main.go", "w") as f:
    f.write(content)

# For paperbot
with open("cmd/paperbot/main.go", "r") as f:
    content = f.read()

old_cost = """					cost := strategy.CalculateTradeMetricsFlat(shares, maxExecutionSum, maxFeeRateBps).Cost
					if arbMode == paperArbModeLaddered {
						est0, est1 := ladderedTakerBiasedBuyShares(shares, ask1, ask2)
						cost = (est0 * ask1) + (est1 * ask2)
					}"""
new_cost = """					cost := strategy.CalculateTradeMetricsFlat(shares, maxExecutionSum, maxFeeRateBps).Cost
					if arbMode == paperArbModeLaddered {
						if ask1 > ask2 {
							cost = shares * ask1
						} else {
							cost = shares * ask2
						}
					}"""
content = content.replace(old_cost, new_cost)

with open("cmd/paperbot/main.go", "w") as f:
    f.write(content)
