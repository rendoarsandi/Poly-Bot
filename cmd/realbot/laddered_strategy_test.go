package main

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

func TestLadderedTakerAskBoundsPreservesConfiguredRange(t *testing.T) {
	minAsk, maxAsk := ladderedTakerAskBounds(0.10, 0.90)
	if minAsk != 0.10 || maxAsk != 0.90 {
		t.Fatalf("unexpected laddered bounds %.2f-%.2f", minAsk, maxAsk)
	}
}

func TestLadderedTakerAskBoundsClampsToTradeableRange(t *testing.T) {
	minAsk, maxAsk := ladderedTakerAskBounds(0.001, 1.50)
	if minAsk != ladderedTakerMinAsk || maxAsk != ladderedTakerMaxAsk {
		t.Fatalf("unexpected clamped laddered bounds %.2f-%.2f", minAsk, maxAsk)
	}

	minAsk, maxAsk = ladderedTakerAskBounds(0.95, 0.40)
	if minAsk != 0.40 || maxAsk != 0.40 {
		t.Fatalf("expected inverted bounds to collapse at max ask, got %.2f-%.2f", minAsk, maxAsk)
	}
}

func TestLadderedTakerEntryEligibleRequiresSumAndSkew(t *testing.T) {
	if !ladderedTakerEntryEligible(0.72, 0.30) {
		t.Fatal("expected skewed pair inside ladder sum cap to be eligible")
	}
	if ladderedTakerEntryEligible(0.61, 0.60) {
		t.Fatal("expected pair above ladder sum cap to be ineligible")
	}
	if ladderedTakerEntryEligible(0.51, 0.50) {
		t.Fatal("expected low-skew pair to be ineligible")
	}
}

func TestRealbotLadderedTerminalEntryBlockReasonBlocksHybridTerminalBook(t *testing.T) {
	reason := realbotLadderedTerminalEntryBlockReason(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0.99, "Up": 0.34},
		map[string]float64{"Down": 0.90, "Up": 0.56},
	)
	if reason == "" || !strings.Contains(reason, "Down bid") {
		t.Fatalf("expected high terminal bid to block ladder entry, got %q", reason)
	}

	reason = realbotLadderedTerminalEntryBlockReason(
		[]string{"Down", "Up"},
		map[string]float64{"Down": 0.46, "Up": 0.53},
		map[string]float64{"Down": 0.47, "Up": 0.54},
	)
	if reason != "" {
		t.Fatalf("expected ordinary book to remain tradable, got %q", reason)
	}
}

func TestRealbotLadderedMoveThresholdMatchesPaperbotClamp(t *testing.T) {
	if got := realbotLadderedMoveThreshold(0.01); got != 0.01 {
		t.Fatalf("expected sub-1c threshold to clamp to 1c, got %.4f", got)
	}
	if got := realbotLadderedMoveThreshold(80); got != 0.25 {
		t.Fatalf("expected threshold to clamp to 25c, got %.4f", got)
	}
}

func TestRealbotLadderedRungIndexHonorsBasePrice(t *testing.T) {
	if got := realbotLadderedRungIndex(0.50, 0.50, 5.0); got != 0 {
		t.Fatalf("expected 50c to sit on the configured base rung, got %d", got)
	}
	if got := realbotLadderedRungIndex(0.55, 0.50, 5.0); got != 1 {
		t.Fatalf("expected 55c to enter rung 1 above the configured base, got %d", got)
	}
	if got := realbotLadderedRungIndex(0.60, 0.50, 5.0); got != 2 {
		t.Fatalf("expected 60c to enter rung 2 above the configured base, got %d", got)
	}
	if got := realbotLadderedRungIndex(0.45, 0.50, 5.0); got != 0 {
		t.Fatalf("expected prices below the configured base to clamp to rung 0, got %d", got)
	}
	if got := realbotLadderedRungIndex(0.55, 0.30, 5.0); got != 5 {
		t.Fatalf("expected the configured basePrice to be honored, got %d", got)
	}
}

func TestRealbotLadderedDirectionalSideAllowsSubFiftyWithLowMinAsk(t *testing.T) {
	entries := []realbotLadderedEntry{
		{seq: 0, ask0: 0.02, ask1: 0.01, side: 0, rung: 0, armed: true},
		{seq: 0, ask0: 0.02, ask1: 0.01, side: 1, rung: 0, armed: true},
	}

	if side, _, ok := ladderedTakerDirectionalSide(entries, 0.49, 0.30, 0.01, 5.0); !ok || side != 0 {
		t.Fatalf("expected low min ask base to allow sub-50c leader entry, got side=%d ok=%v", side, ok)
	}
}

func TestRealbotLadderedDirectionalSideUsesAnchoredRungs(t *testing.T) {
	if side, _, ok := ladderedTakerDirectionalSide(nil, 0.55, 0.45, 0.50, 5.0); !ok || side != 0 {
		t.Fatalf("expected initial higher-ask side 0, got side=%d ok=%v", side, ok)
	}

	entries := []realbotLadderedEntry{
		{seq: 1, ask0: 0.55, ask1: 0.45, side: 0, rung: 1},
	}
	if side, _, ok := ladderedTakerDirectionalSide(entries, 0.59, 0.41, 0.50, 5.0); ok {
		t.Fatalf("expected same anchored 5c rung to block re-entry, got side=%d ok=%v", side, ok)
	}
	if side, _, ok := ladderedTakerDirectionalSide(entries, 0.60, 0.40, 0.50, 5.0); !ok || side != 0 {
		t.Fatalf("expected side 0 re-entry after crossing the next anchored rung, got side=%d ok=%v", side, ok)
	}
	if side, _, ok := ladderedTakerDirectionalSide(entries, 0.59, 0.54, 0.50, 5.0); ok {
		t.Fatalf("expected lower side 1 move to stay blocked while side 0 is still higher, got side=%d ok=%v", side, ok)
	}
}

func TestRealbotLadderedDirectionalSideOnlyBuysCurrentHigherSide(t *testing.T) {
	entries := []realbotLadderedEntry{{seq: 1, ask0: 0.55, ask1: 0.45, side: 0, rung: 1}}

	if side, _, ok := ladderedTakerDirectionalSide(entries, 0.60, 0.55, 0.50, 5.0); !ok || side != 0 {
		t.Fatalf("expected higher side 0 to re-enter after crossing the next anchored 5c rung, got side=%d ok=%v", side, ok)
	}

	entries = append(entries, realbotLadderedEntry{seq: 2, ask0: 0.60, ask1: 0.55, side: 0, rung: 2})
	if side, _, ok := ladderedTakerDirectionalSide(entries, 0.60, 0.55, 0.50, 5.0); ok {
		t.Fatalf("expected lower side 1 at the first rung to stay blocked while side 0 is still higher, got side=%d ok=%v", side, ok)
	}
	if side, _, ok := ladderedTakerDirectionalSide(entries, 0.50, 0.55, 0.50, 5.0); !ok || side != 1 {
		t.Fatalf("expected side 1 to re-enter only after becoming the higher side, got side=%d ok=%v", side, ok)
	}
}

func TestRealbotArmInitialLadderedEntriesStartsFromBaseRung(t *testing.T) {
	entries := realbotArmInitialLadderedEntries(nil, 0.80, 0.20, 0.01, 5.0)
	if len(entries) != 2 {
		t.Fatalf("expected armed markers for both sides, got %d", len(entries))
	}
	if !entries[0].armed || entries[0].side != 0 || entries[0].rung != 0 {
		t.Fatalf("expected side 0 to arm at the base rung, got %+v", entries[0])
	}
	if !entries[1].armed || entries[1].side != 1 || entries[1].rung != 0 {
		t.Fatalf("expected side 1 to arm at the base rung, got %+v", entries[1])
	}
	if side, _, ok := ladderedTakerDirectionalSide(entries, 0.80, 0.20, 0.01, 5.0); !ok || side != 0 {
		t.Fatalf("expected startup arm to allow the current live rung after base reset, got side=%d ok=%v", side, ok)
	}
}

func TestRealbotLadderedStartupStabilityReadyAlwaysAllowsImmediateEntry(t *testing.T) {
	// The startup stability gate is permanently disabled; rungs can fire
	// immediately. The state pointers should still be updated so downstream
	// inspection sees the most recently observed side/rung.
	stableAt := time.Time{}
	side := -1
	rung := -1
	state := &realbotPanicBuyStrategyState{
		ladderedStartupStableAt: &stableAt,
		ladderedStartupSide:     &side,
		ladderedStartupRung:     &rung,
	}
	now := time.Now()

	if !realbotLadderedStartupStabilityReady(state, 0, 1, now) {
		t.Fatal("expected stability gate to be disabled and allow the first rung immediately")
	}
	if side != 0 {
		t.Fatalf("expected startup side recorded as 0, got %d", side)
	}
	if rung != 1 {
		t.Fatalf("expected startup rung recorded as 1, got %d", rung)
	}
	if stableAt.IsZero() {
		t.Fatal("expected startup stability timer to be initialized on first observation")
	}
	if !realbotLadderedStartupStabilityReady(state, 1, 3, now.Add(time.Millisecond)) {
		t.Fatal("expected rung changes to remain unblocked without any waiting period")
	}
	if rung != 3 {
		t.Fatalf("expected startup rung updated to 3, got %d", rung)
	}
	if side != 1 {
		t.Fatalf("expected startup side updated to 1, got %d", side)
	}
}

func TestRealbotResetLadderedStartupStabilityClearsState(t *testing.T) {
	stableAt := time.Now()
	side := 1
	rung := 3
	state := &realbotPanicBuyStrategyState{
		ladderedStartupStableAt: &stableAt,
		ladderedStartupSide:     &side,
		ladderedStartupRung:     &rung,
	}

	realbotResetLadderedStartupStability(state)

	if !stableAt.IsZero() {
		t.Fatal("expected startup stability time to clear")
	}
	if side != -1 {
		t.Fatalf("expected startup side reset to -1, got %d", side)
	}
	if rung != -1 {
		t.Fatalf("expected startup rung reset to -1, got %d", rung)
	}
}

func TestRealbotLadderedDirectionalSideRearmsAfterReturningToBaseRung(t *testing.T) {
	// Configured at a $0.50 base with a 5c step: rungs 0..3 sit at 0.50/0.55/0.60/0.65.
	entries := []realbotLadderedEntry{
		{seq: 0, ask0: 0.50, ask1: 0.45, side: 0, rung: 0, armed: true},
		{seq: 1, ask0: 0.55, ask1: 0.45, side: 0, rung: 1},
		{seq: 2, ask0: 0.60, ask1: 0.45, side: 0, rung: 2},
		{seq: 3, ask0: 0.65, ask1: 0.45, side: 0, rung: 3},
	}

	if side, _, ok := ladderedTakerDirectionalSide(entries, 0.65, 0.40, 0.50, 5.0); ok {
		t.Fatalf("expected side 0 to stay blocked until it rearms, got side=%d ok=%v", side, ok)
	}

	// Side 0 drops back to the $0.50 base; refresh should arm a new rung 0
	// marker so a subsequent leadership flip lets side 0 re-enter.
	entries = realbotRefreshLadderedEntries(entries, 0.50, 0.65, 0.50, 5.0)
	if side, _, ok := ladderedTakerDirectionalSide(entries, 0.55, 0.45, 0.50, 5.0); !ok || side != 0 {
		t.Fatalf("expected side 0 to re-arm after returning to the base rung, got side=%d ok=%v", side, ok)
	}
}

func TestRealbotPendingLadderedEntryUsesCurrentQuoteOnLargeGaps(t *testing.T) {
	entries := []realbotLadderedEntry{{seq: 1, ask0: 0.50, ask1: 0.85}}

	// With the base at $0.50 and a 5c step, an ask of 0.85 = (0.85-0.50)/0.05 = rung 7.
	pending := realbotPendingLadderedEntry(entries, 2, 0.55, 0.85, 0.50, 5.0)
	if math.Abs(pending.ask0-0.55) > 1e-9 || math.Abs(pending.ask1-0.85) > 1e-9 {
		t.Fatalf("expected pending anchor to jump to the current quote, got %+v", pending)
	}
	if pending.side != 1 || pending.rung != 7 {
		t.Fatalf("expected pending entry to remember side 1 rung 7 from the configured base, got %+v", pending)
	}

	entries = append(entries, pending)
	if side, mult, ok := ladderedTakerDirectionalSide(entries, 0.55, 0.85, 0.50, 5.0); ok {
		t.Fatalf("expected unchanged quote to stop replaying missed rungs, got side=%d mult=%d ok=%v", side, mult, ok)
	}
}

func TestRealbotLadderedRequestedQtySharesModePreservesConfiguredSize(t *testing.T) {
	cfg := paper.TUISettings{
		LadderedTakerSizingMode: core.LadderedTakerSizingModeShares,
		LadderedTakerSizeShares: 3.5,
	}

	if got := realbotLadderedRequestedQty(0.84, cfg, 0.62, 0.64); math.Abs(got-3.5) > 1e-9 {
		t.Fatalf("expected share ladder sizing to keep configured 3.5 shares, got %.4f", got)
	}
}

func TestRealbotLadderedRequestedQtyUSDCSizesAgainstActiveSideLimit(t *testing.T) {
	cfg := paper.TUISettings{
		LadderedTakerSizingMode: core.LadderedTakerSizingModeUSDC,
		LadderedTakerSizeUSDC:   5.0,
	}

	if got := realbotLadderedRequestedQty(1.00, cfg, 0.80, 0.80); math.Abs(got-6.25) > 1e-9 {
		t.Fatalf("expected USDC ladder sizing to use the active side price, got %.4f", got)
	}
}

func TestRealbotLadderedRequestedQtyUSDCRespectsMaxTradeSize(t *testing.T) {
	cfg := paper.TUISettings{
		LadderedTakerSizingMode: core.LadderedTakerSizingModeUSDC,
		LadderedTakerSizeUSDC:   10.0,
		MaxTradeSize:            4.0,
	}

	want := 6.0
	if got := realbotLadderedRequestedQty(0.90, cfg, 0.60, 0.61); math.Abs(got-want) > 1e-9 {
		t.Fatalf("expected max trade size to cap directional ladder sizing at %.4f, got %.4f", want, got)
	}
}

func TestRealbotLadderedRequestedQtyUSDCRespectsSubmitCapPrecision(t *testing.T) {
	cfg := paper.TUISettings{
		LadderedTakerSizingMode: core.LadderedTakerSizingModeUSDC,
		LadderedTakerSizeUSDC:   2.10,
	}

	if got := realbotLadderedRequestedQty(1.00, cfg, 0.52, 0.99); math.Abs(got-2.0) > 1e-9 {
		t.Fatalf("expected 2.10 budget at 0.99 cap to floor to 2.0 shares, got %.4f", got)
	}
}

func TestRealbotLadderedRequestedQtyUSDCClampsSubDollarBudgetToVenueMinimum(t *testing.T) {
	cfg := paper.TUISettings{
		LadderedTakerSizingMode: core.LadderedTakerSizingModeUSDC,
		LadderedTakerSizeUSDC:   0.01,
	}

	want := normalizeMarketBuyShares(1.0 / 0.80)
	if got := realbotLadderedRequestedQty(1.00, cfg, 0.80, 0.80); math.Abs(got-want) > 1e-9 {
		t.Fatalf("expected sub-dollar ladder budget to clamp to the $1 venue minimum, got %.4f want %.4f", got, want)
	}
}

func TestRealbotLadderedRequestedQtyUSDCFloorsSubDollarMaxTradeCapToVenueMinimum(t *testing.T) {
	cfg := paper.TUISettings{
		LadderedTakerSizingMode: core.LadderedTakerSizingModeUSDC,
		LadderedTakerSizeUSDC:   1.10,
		MaxTradeSize:            0.10,
	}

	want := normalizeMarketBuyShares(1.0 / 0.80)
	if got := realbotLadderedRequestedQty(1.00, cfg, 0.80, 0.80); math.Abs(got-want) > 1e-9 {
		t.Fatalf("expected sub-dollar ladder max trade cap to floor at the $1 venue minimum, got %.4f want %.4f", got, want)
	}
}

func TestRealbotLadderedInventoryCapBlocksHeavyLeader(t *testing.T) {
	engine := paper.NewEngine(100)
	if _, err := engine.BuyForMarket("BTC", "Up", 0.60, 3.1); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	blocked, reason := realbotLadderedInventoryCapReached(engine, "BTC", []string{"Down", "Up"}, 1, 1.0, 0.60, core.LadderedTakerPnLGuardWorst, -1.20, 0)
	if !blocked {
		t.Fatal("expected active side inventory cap to block an already-heavy leader")
	}
	if !strings.Contains(reason, "worst-case resolve PnL") {
		t.Fatalf("expected reason to explain the projected resolve PnL hole, got %q", reason)
	}
}

func TestRealbotLadderedInventoryCapAllowsLeaderToBalanceOtherSide(t *testing.T) {
	engine := paper.NewEngine(100)
	if _, err := engine.BuyForMarket("BTC", "Down", 0.40, 3.0); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}
	if _, err := engine.BuyForMarket("BTC", "Up", 0.60, 1.0); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	blocked, reason := realbotLadderedInventoryCapReached(engine, "BTC", []string{"Down", "Up"}, 1, 1.0, 0.60, core.LadderedTakerPnLGuardWorst, -1.20, 0)
	if blocked {
		t.Fatalf("expected cap to allow buying the leader while it is balancing the other side, got %q", reason)
	}
}

func TestRealbotLadderedInventoryCapAllowsBootstrapExposureForFirstTwoChunks(t *testing.T) {
	engine := paper.NewEngine(100)

	blocked, reason := realbotLadderedInventoryCapReached(engine, "BTC", []string{"Down", "Up"}, 1, 1.0, 0.60, core.LadderedTakerPnLGuardWorst, -1.20, 0)
	if blocked {
		t.Fatalf("expected first ladder chunk to be allowed, got %q", reason)
	}
	if _, err := engine.BuyForMarket("BTC", "Up", 0.60, 1.0); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	blocked, reason = realbotLadderedInventoryCapReached(engine, "BTC", []string{"Down", "Up"}, 1, 1.0, 0.60, core.LadderedTakerPnLGuardWorst, -1.20, 0)
	if blocked {
		t.Fatalf("expected second ladder chunk to stay allowed, got %q", reason)
	}
	if _, err := engine.BuyForMarket("BTC", "Up", 0.60, 1.0); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}

	blocked, reason = realbotLadderedInventoryCapReached(engine, "BTC", []string{"Down", "Up"}, 1, 1.0, 0.60, core.LadderedTakerPnLGuardWorst, -1.20, 0)
	if !blocked {
		t.Fatal("expected third same-side chunk to be blocked once worst-case resolve loss gets too deep")
	}
}

func TestRealbotLadderedInventoryCapZeroFloorDisablesWorstPnLGuard(t *testing.T) {
	// configuredWorstPnLFloor == 0 means the operator turned the safety guard
	// OFF. Even an arbitrarily heavy one-sided position should not block.
	engine := paper.NewEngine(100)
	if _, err := engine.BuyForMarket("BTC", "Up", 0.60, 50.0); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}
	blocked, reason := realbotLadderedInventoryCapReached(engine, "BTC", []string{"Down", "Up"}, 1, 10.0, 0.60, core.LadderedTakerPnLGuardWorst, 0, 0)
	if blocked {
		t.Fatalf("expected zero worst-PnL floor to disable the safety guard, got %q", reason)
	}
}

func TestRealbotLadderedInventoryCapZeroCapDisablesMaxProfitGuard(t *testing.T) {
	// configuredMaxProfitPnL == 0 means the operator turned the safety guard
	// OFF. Even a deeply profitable winning-side projection should not block.
	engine := paper.NewEngine(100)
	if _, err := engine.BuyForMarket("BTC", "Up", 0.30, 100.0); err != nil {
		t.Fatalf("seed buy failed: %v", err)
	}
	blocked, reason := realbotLadderedInventoryCapReached(engine, "BTC", []string{"Down", "Up"}, 1, 10.0, 0.30, core.LadderedTakerPnLGuardMaxProfit, 0, 0)
	if blocked {
		t.Fatalf("expected zero max-profit cap to disable the safety guard, got %q", reason)
	}
}

func TestRealbotLadderedInventoryCapHonorsConfiguredWorstPnLFloor(t *testing.T) {
	engine := paper.NewEngine(100)

	blocked, reason := realbotLadderedInventoryCapReached(engine, "BTC", []string{"Down", "Up"}, 1, 1.0, 0.60, core.LadderedTakerPnLGuardWorst, -0.50, 0)
	if !blocked {
		t.Fatal("expected tighter configured worst-PnL floor to block the first chunk")
	}
	if !strings.Contains(reason, "floor -$0.50") {
		t.Fatalf("expected block reason to show the configured floor, got %q", reason)
	}

	blocked, reason = realbotLadderedInventoryCapReached(engine, "BTC", []string{"Down", "Up"}, 1, 1.0, 0.60, core.LadderedTakerPnLGuardWorst, -1.00, 0)
	if blocked {
		t.Fatalf("expected looser configured worst-PnL floor to allow the first chunk, got %q", reason)
	}
}

func TestRealbotLadderedInventoryCapHonorsConfiguredMaxProfitPnL(t *testing.T) {
	engine := paper.NewEngine(100)

	// Buy 10 shares @ 0.60. Cost: 6.00. PnL if "Up" wins: 10.00 - 6.00 = +$4.00.
	// If cap is 5.00, this should be allowed.
	blocked, reason := realbotLadderedInventoryCapReached(engine, "BTC", []string{"Down", "Up"}, 1, 10.0, 0.60, core.LadderedTakerPnLGuardMaxProfit, 0, 5.00)
	if blocked {
		t.Fatalf("expected trade under cap to be allowed, got %q", reason)
	}

	engine.BuyForMarket("BTC", "Up", 0.60, 10.0)

	// Attempt to buy 4 more shares @ 0.50. Additional Cost: 2.00. Total Cost: 8.00.
	// New PnL if "Up" wins: 14.00 - 8.00 = +$6.00.
	// If cap is 5.00, this should be blocked.
	blocked, reason = realbotLadderedInventoryCapReached(engine, "BTC", []string{"Down", "Up"}, 1, 4.0, 0.50, core.LadderedTakerPnLGuardMaxProfit, 0, 5.00)
	if !blocked {
		t.Fatal("expected trade exceeding cap to be blocked")
	}
	if !strings.Contains(reason, "active-side resolve PnL") {
		t.Fatalf("expected block reason to mention active-side resolve PnL, got %q", reason)
	}

	// Attempt to buy 4 more shares on "Down" @ 0.50. Cost: 2.00. Total Cost: 8.00.
	// New PnL if "Down" wins: 4.00 - 8.00 = -$4.00.
	// This is well below the cap of 5.00, so it should be allowed (hedging).
	blocked, reason = realbotLadderedInventoryCapReached(engine, "BTC", []string{"Down", "Up"}, 0, 4.0, 0.50, core.LadderedTakerPnLGuardMaxProfit, 0, 5.00)
	if blocked {
		t.Fatalf("expected hedging trade under cap to be allowed, got %q", reason)
	}
}

func TestRealbotResolveLadderedEntryDropsRejectedPendingAnchor(t *testing.T) {
	entries := []realbotLadderedEntry{
		{seq: 1, ask0: 0.50, ask1: 0.40},
		{seq: 2, ask0: 0.52, ask1: 0.40},
	}

	entries = realbotResolveLadderedEntry(entries, 2, false)

	if len(entries) != 1 {
		t.Fatalf("expected failed pending rung to be removed, got %d entries", len(entries))
	}
	if side, _, ok := ladderedTakerDirectionalSide(entries, 0.512, 0.401, 0.01, 1.0); !ok || side != 0 {
		t.Fatalf("expected re-entry to fall back to the last confirmed ladder anchor, got side=%d ok=%v", side, ok)
	}
}

func TestRealbotShouldRetryLadderedBuyFailureTransientExecutionError(t *testing.T) {
	exec := directMarketExecution{
		Result: &trading.TradeResult{
			Message: "could not run the execution",
		},
	}

	if !realbotShouldRetryLadderedBuyFailure(exec) {
		t.Fatal("expected transient execution error to stay retryable for laddered buy flow")
	}
}

func TestRealbotShouldRetryLadderedBuyFailureIgnoresHardVenueRejects(t *testing.T) {
	exec := directMarketExecution{
		Result: &trading.TradeResult{
			Status:  "REJECTED",
			Message: "order rejected",
		},
	}

	if realbotShouldRetryLadderedBuyFailure(exec) {
		t.Fatal("expected hard venue reject not to arm laddered retries")
	}
}

func TestRealbotShouldAdvanceLadderedEntryTracksAnyActionableFill(t *testing.T) {
	if !realbotShouldAdvanceLadderedEntry(10.0, 0.01) {
		t.Fatal("expected minimum actionable partial fill to advance ladder anchor")
	}
	if !realbotShouldAdvanceLadderedEntry(10.0, 9.6) {
		t.Fatal("expected materially filled rung to advance ladder anchor")
	}
	if !realbotShouldAdvanceLadderedEntry(0.01, 0.01) {
		t.Fatal("expected minimum actionable rung to advance when fully filled")
	}
}

func TestRealbotLadderedRecoveredFillQtyUsesRequestedSideOnly(t *testing.T) {
	if got := realbotLadderedRecoveredFillQty(0, 1.02, 1.50, 9.0); math.Abs(got-1.02) > 1e-9 {
		t.Fatalf("expected side 0 recovery to clamp to requested qty, got %.4f", got)
	}
	if got := realbotLadderedRecoveredFillQty(1, 1.02, 9.0, 0.80); math.Abs(got-0.80) > 1e-9 {
		t.Fatalf("expected side 1 recovery to use the requested side only, got %.4f", got)
	}
	if got := realbotLadderedRecoveredFillQty(0, 1.02, 0, 0.80); got != 0 {
		t.Fatalf("expected opposite-side recovery to be ignored, got %.4f", got)
	}
}

func TestRealbotVerifiedLadderedBuyFillUsesRecoveredBalanceDelta(t *testing.T) {
	filledQty, confirmed, authoritative := realbotVerifiedLadderedBuyFill(1.02, 1.02, 0.61, nil)
	if math.Abs(filledQty-0.61) > 1e-9 {
		t.Fatalf("expected recovered ladder qty 0.61, got %.4f", filledQty)
	}
	if !confirmed {
		t.Fatal("expected recovered ladder fill to stay confirmed")
	}
	if !authoritative {
		t.Fatal("expected recovered ladder fill to be authoritative")
	}
}

func TestRealbotVerifiedLadderedBuyFillRejectsMissingVerifiedShares(t *testing.T) {
	filledQty, confirmed, authoritative := realbotVerifiedLadderedBuyFill(1.02, 1.02, 0, nil)
	if filledQty != 0 {
		t.Fatalf("expected missing verified ladder fill to zero out optimistic qty, got %.4f", filledQty)
	}
	if confirmed {
		t.Fatal("expected missing verified ladder fill to fail confirmation")
	}
	if !authoritative {
		t.Fatal("expected clean zero-qty verification to be authoritative")
	}
}

func TestRealbotVerifiedLadderedBuyFillFallsBackWhenVerificationUnavailable(t *testing.T) {
	filledQty, confirmed, authoritative := realbotVerifiedLadderedBuyFill(1.02, 0.98, 0, errors.New("refresh failed"))
	if math.Abs(filledQty-0.98) > 1e-9 {
		t.Fatalf("expected optimistic ladder qty fallback 0.98, got %.4f", filledQty)
	}
	if !confirmed {
		t.Fatal("expected optimistic ladder qty fallback to preserve confirmation")
	}
	if authoritative {
		t.Fatal("expected verification error fallback not to claim authoritative fill")
	}
}

func TestRealbotResolveInitialPairSnapshotPrefersAuthoritativeBalanceForLadderedMode(t *testing.T) {
	bal0, bal1, source, err := realbotResolveInitialPairSnapshot(context.Background(), true, 0.0, 0.0, func(context.Context) (float64, float64, string, error) {
		return 0.99944, 1.94674, "live WS + on-chain backup", nil
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if math.Abs(bal0-0.99944) > 1e-9 || math.Abs(bal1-1.94674) > 1e-9 {
		t.Fatalf("expected authoritative ladder baseline 0.99944/1.94674, got %.5f/%.5f", bal0, bal1)
	}
	if source != "live WS + on-chain backup" {
		t.Fatalf("expected authoritative source, got %q", source)
	}
}

func TestRealbotResolveInitialPairSnapshotSkipsAuthoritativeLookupOutsideLadderedMode(t *testing.T) {
	called := false
	bal0, bal1, source, err := realbotResolveInitialPairSnapshot(context.Background(), false, 0.40, 0.60, func(context.Context) (float64, float64, string, error) {
		called = true
		return 9, 9, "unexpected", nil
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if called {
		t.Fatal("expected non-laddered mode not to call authoritative snapshot loader")
	}
	if math.Abs(bal0-0.40) > 1e-9 || math.Abs(bal1-0.60) > 1e-9 {
		t.Fatalf("expected live baseline 0.40/0.60, got %.5f/%.5f", bal0, bal1)
	}
	if source != "live WS cache" {
		t.Fatalf("expected live snapshot source, got %q", source)
	}
}

func TestRealbotLadderedHoldMode(t *testing.T) {
	if !realbotLadderedHoldMode(paper.TUISettings{PaperArbMode: paperArbModeLaddered}) {
		t.Fatal("expected laddered mode to preserve inventory for redemption")
	}
	if realbotLadderedHoldMode(paper.TUISettings{PaperArbMode: paperArbModeTaker}) {
		t.Fatal("expected taker mode to avoid laddered hold behavior")
	}
}

func TestRealbotTUISettingsRoundTripIncludesLadderedSlippage(t *testing.T) {
	cfg := &core.Config{
		ExecutionBackend:            core.ExecutionBackendPaper,
		PaperBalance:                42.5,
		PaperArbMode:                paperArbModeLaddered,
		LadderedTakerMaxSlippagePct: 7,
		LadderedTakerPnLGuardMode:   core.LadderedTakerPnLGuardMaxProfit,
		LadderedTakerWorstPnLFloor:  -2.25,
		LadderedTakerMaxProfitPnL:   0.8,
		RedeemGasMode:               core.RedeemGasModeUrgent,
	}

	settings := realbotTUISettingsFromConfig(cfg)
	if settings.ExecutionBackend != core.ExecutionBackendPaper {
		t.Fatalf("expected TUI settings to include execution backend, got %q", settings.ExecutionBackend)
	}
	if settings.PaperBalance != 42.5 {
		t.Fatalf("expected TUI settings to include paper balance, got %.2f", settings.PaperBalance)
	}
	if settings.LadderedTakerMaxSlippagePct != 7 {
		t.Fatalf("expected TUI settings to include laddered slippage, got %.0f", settings.LadderedTakerMaxSlippagePct)
	}
	if settings.LadderedTakerWorstPnLFloor != -2.25 {
		t.Fatalf("expected TUI settings to include laddered worst PnL floor, got %.2f", settings.LadderedTakerWorstPnLFloor)
	}
	if settings.LadderedTakerPnLGuardMode != core.LadderedTakerPnLGuardMaxProfit {
		t.Fatalf("expected TUI settings to include laddered pnl guard mode, got %q", settings.LadderedTakerPnLGuardMode)
	}
	if settings.LadderedTakerMaxProfitPnL != 0.8 {
		t.Fatalf("expected TUI settings to include laddered min profit pnl, got %.2f", settings.LadderedTakerMaxProfitPnL)
	}
	if settings.RedeemGasMode != core.RedeemGasModeUrgent {
		t.Fatalf("expected TUI settings to include redeem gas mode, got %q", settings.RedeemGasMode)
	}

	settings.ExecutionBackend = core.ExecutionBackendLive
	settings.PaperBalance = 77
	settings.LadderedTakerMaxSlippagePct = 13
	settings.LadderedTakerPnLGuardMode = core.LadderedTakerPnLGuardWorst
	settings.LadderedTakerWorstPnLFloor = -1.5
	settings.LadderedTakerMaxProfitPnL = 1.1
	settings.RedeemGasMode = core.RedeemGasModeNormal
	applyRealbotTUISettings(cfg, settings)
	if cfg.ExecutionBackend != core.ExecutionBackendLive {
		t.Fatalf("expected config to receive updated execution backend, got %q", cfg.ExecutionBackend)
	}
	if cfg.PaperBalance != 77 {
		t.Fatalf("expected config to receive updated paper balance, got %.2f", cfg.PaperBalance)
	}
	if cfg.LadderedTakerMaxSlippagePct != 13 {
		t.Fatalf("expected config to receive updated laddered slippage, got %.0f", cfg.LadderedTakerMaxSlippagePct)
	}
	if cfg.LadderedTakerWorstPnLFloor != -1.5 {
		t.Fatalf("expected config to receive updated laddered worst PnL floor, got %.2f", cfg.LadderedTakerWorstPnLFloor)
	}
	if cfg.LadderedTakerPnLGuardMode != core.LadderedTakerPnLGuardWorst {
		t.Fatalf("expected config to receive updated laddered pnl guard mode, got %q", cfg.LadderedTakerPnLGuardMode)
	}
	if cfg.LadderedTakerMaxProfitPnL != 1.1 {
		t.Fatalf("expected config to receive updated laddered min profit pnl, got %.2f", cfg.LadderedTakerMaxProfitPnL)
	}
	if cfg.RedeemGasMode != core.RedeemGasModeNormal {
		t.Fatalf("expected config to receive updated redeem gas mode, got %q", cfg.RedeemGasMode)
	}
}

func TestRealbotHandlePanicBuyStrategySkipsCrossedNonLadderedQuotes(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)

	entryExecutionInFlight := false
	panicBuyCooldown := time.Time{}
	lastPairUpdate := time.Now()
	lastTrade := time.Time{}
	lastDustRecoveryNotice := time.Time{}
	ladderedEntries := []realbotLadderedEntry{{seq: 1, ask0: 0.45, ask1: 0.46}}
	nextLadderedEntrySeq := uint64(1)

	handled := realbotHandlePanicBuyStrategy(realbotPanicBuyStrategyArgs{
		ctx:            context.Background(),
		marketID:       "BTC",
		outcomes:       []string{"Down", "Up"},
		tokenToOutcome: map[string]string{"down-token": "Down", "up-token": "Up"},
		tokenBids:      map[string]float64{"Down": 0.52, "Up": 0.48},
		tokenAsks:      map[string]float64{"Down": 0.51, "Up": 0.49},
		tui:            tui,
		engine:         engine,
		arbMode:        paperArbModeTaker,
	}, &realbotPanicBuyStrategyState{
		lastPairUpdate:         &lastPairUpdate,
		ladderedEntries:        &ladderedEntries,
		nextLadderedEntrySeq:   &nextLadderedEntrySeq,
		panicBuyCooldown:       &panicBuyCooldown,
		lastTrade:              &lastTrade,
		lastDustRecoveryNotice: &lastDustRecoveryNotice,
		entryExecutionInFlight: &entryExecutionInFlight,
	})
	if !handled {
		t.Fatal("expected crossed non-laddered quotes to short-circuit the strategy")
	}
	if entryExecutionInFlight {
		t.Fatal("expected crossed quotes to avoid launching aggressive entry execution")
	}
	if nextLadderedEntrySeq != 1 || len(ladderedEntries) != 1 {
		t.Fatal("expected crossed quotes to leave ladder state unchanged")
	}
}

func TestRealbotHandlePanicBuyStrategySkipsCrossedLadderedQuotes(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)

	entryExecutionInFlight := false
	panicBuyCooldown := time.Time{}
	lastPairUpdate := time.Now()
	lastTrade := time.Time{}
	lastDustRecoveryNotice := time.Time{}
	ladderedEntries := []realbotLadderedEntry{{seq: 1, ask0: 0.45, ask1: 0.46}}
	nextLadderedEntrySeq := uint64(1)

	handled := realbotHandleLadderedStrategy(realbotPanicBuyStrategyArgs{
		ctx:            context.Background(),
		marketID:       "BTC",
		outcomes:       []string{"Down", "Up"},
		tokenToOutcome: map[string]string{"down-token": "Down", "up-token": "Up"},
		tokenBids:      map[string]float64{"Down": 0.40, "Up": 0.48},
		tokenAsks:      map[string]float64{"Down": 0.39, "Up": 0.70},
		tui:            tui,
		engine:         engine,
		arbMode:        paperArbModeLaddered,
	}, &realbotPanicBuyStrategyState{
		lastPairUpdate:         &lastPairUpdate,
		ladderedEntries:        &ladderedEntries,
		nextLadderedEntrySeq:   &nextLadderedEntrySeq,
		panicBuyCooldown:       &panicBuyCooldown,
		lastTrade:              &lastTrade,
		lastDustRecoveryNotice: &lastDustRecoveryNotice,
		entryExecutionInFlight: &entryExecutionInFlight,
	})
	if !handled {
		t.Fatal("expected crossed laddered quotes to short-circuit the strategy")
	}
	if entryExecutionInFlight {
		t.Fatal("expected crossed laddered quotes to avoid launching aggressive entry execution")
	}
	if nextLadderedEntrySeq != 1 || len(ladderedEntries) != 1 {
		t.Fatal("expected crossed laddered quotes to leave ladder state unchanged")
	}
}

func TestRealbotHandlePanicBuyStrategyArmsInitialLadderedRungWithoutBuying(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	tui.InitSettings(paper.TUISettings{
		PaperArbMode:                  paperArbModeLaddered,
		LadderedTakerReentryMoveCents: 5.0,
		MinAskPrice:                   0.01,
		MaxAskPrice:                   0.99,
	}, nil)

	entryExecutionInFlight := false
	panicBuyCooldown := time.Time{}
	lastPairUpdate := time.Now()
	lastTrade := time.Time{}
	lastDustRecoveryNotice := time.Time{}
	ladderedEntries := []realbotLadderedEntry{}
	nextLadderedEntrySeq := uint64(4)

	handled := realbotHandleLadderedStrategy(realbotPanicBuyStrategyArgs{
		ctx:            context.Background(),
		marketID:       "BTC",
		outcomes:       []string{"Down", "Up"},
		tokenToOutcome: map[string]string{"down-token": "Down", "up-token": "Up"},
		tokenBids:      map[string]float64{"Down": 0.79, "Up": 0.19},
		tokenAsks:      map[string]float64{"Down": 0.80, "Up": 0.20},
		tui:            tui,
		engine:         engine,
		arbMode:        paperArbModeLaddered,
	}, &realbotPanicBuyStrategyState{
		lastPairUpdate:         &lastPairUpdate,
		ladderedEntries:        &ladderedEntries,
		nextLadderedEntrySeq:   &nextLadderedEntrySeq,
		panicBuyCooldown:       &panicBuyCooldown,
		lastTrade:              &lastTrade,
		lastDustRecoveryNotice: &lastDustRecoveryNotice,
		entryExecutionInFlight: &entryExecutionInFlight,
	})
	if !handled {
		t.Fatal("expected initial laddered signal to be handled by arming the rung")
	}
	if entryExecutionInFlight {
		t.Fatal("expected arming to avoid launching entry execution")
	}
	if nextLadderedEntrySeq != 4 {
		t.Fatalf("expected arming to leave the execution sequence unchanged, got %d", nextLadderedEntrySeq)
	}
	if !panicBuyCooldown.IsZero() {
		t.Fatalf("expected arming not to apply a blind startup cooldown, got %v", panicBuyCooldown)
	}
	if len(ladderedEntries) != 2 {
		t.Fatalf("expected initial arming markers for both sides, got %d", len(ladderedEntries))
	}
	if !ladderedEntries[0].armed || ladderedEntries[0].side != 0 || ladderedEntries[0].rung != 0 {
		t.Fatalf("expected side 0 to arm at the base rung, got %+v", ladderedEntries[0])
	}
	if !ladderedEntries[1].armed || ladderedEntries[1].side != 1 || ladderedEntries[1].rung != 0 {
		t.Fatalf("expected side 1 to arm at the base rung, got %+v", ladderedEntries[1])
	}
}

func TestRealbotHandlePanicBuyStrategySkipsLadderedEntryWhileInFlight(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)

	entryExecutionInFlight := true
	panicBuyCooldown := time.Time{}
	lastPairUpdate := time.Now()
	lastTrade := time.Time{}
	lastDustRecoveryNotice := time.Time{}
	ladderedEntries := []realbotLadderedEntry{{seq: 1, ask0: 0.45, ask1: 0.46}}
	nextLadderedEntrySeq := uint64(1)

	handled := realbotHandleLadderedStrategy(realbotPanicBuyStrategyArgs{
		ctx:            context.Background(),
		marketID:       "BTC",
		outcomes:       []string{"Down", "Up"},
		tokenToOutcome: map[string]string{"down-token": "Down", "up-token": "Up"},
		tokenBids:      map[string]float64{"Down": 0.40, "Up": 0.48},
		tokenAsks:      map[string]float64{"Down": 0.39, "Up": 0.70},
		tui:            tui,
		engine:         engine,
		arbMode:        paperArbModeLaddered,
	}, &realbotPanicBuyStrategyState{
		lastPairUpdate:         &lastPairUpdate,
		ladderedEntries:        &ladderedEntries,
		nextLadderedEntrySeq:   &nextLadderedEntrySeq,
		panicBuyCooldown:       &panicBuyCooldown,
		lastTrade:              &lastTrade,
		lastDustRecoveryNotice: &lastDustRecoveryNotice,
		entryExecutionInFlight: &entryExecutionInFlight,
	})
	if !handled {
		t.Fatal("expected laddered in-flight execution to short-circuit the strategy")
	}
	if nextLadderedEntrySeq != 1 || len(ladderedEntries) != 1 {
		t.Fatal("expected ladder state to stay unchanged while execution is still in flight")
	}
}

func TestRealbotHandlePanicBuyStrategySkipsLadderedEntryDuringCooldown(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)

	entryExecutionInFlight := false
	panicBuyCooldown := time.Now().Add(2 * time.Second)
	lastPairUpdate := time.Now()
	lastTrade := time.Time{}
	lastDustRecoveryNotice := time.Time{}
	ladderedEntries := []realbotLadderedEntry{{seq: 1, ask0: 0.45, ask1: 0.46}}
	nextLadderedEntrySeq := uint64(1)

	handled := realbotHandleLadderedStrategy(realbotPanicBuyStrategyArgs{
		ctx:            context.Background(),
		marketID:       "BTC",
		outcomes:       []string{"Down", "Up"},
		tokenToOutcome: map[string]string{"down-token": "Down", "up-token": "Up"},
		tokenBids:      map[string]float64{"Down": 0.40, "Up": 0.48},
		tokenAsks:      map[string]float64{"Down": 0.39, "Up": 0.70},
		tui:            tui,
		engine:         engine,
		arbMode:        paperArbModeLaddered,
	}, &realbotPanicBuyStrategyState{
		lastPairUpdate:         &lastPairUpdate,
		ladderedEntries:        &ladderedEntries,
		nextLadderedEntrySeq:   &nextLadderedEntrySeq,
		panicBuyCooldown:       &panicBuyCooldown,
		lastTrade:              &lastTrade,
		lastDustRecoveryNotice: &lastDustRecoveryNotice,
		entryExecutionInFlight: &entryExecutionInFlight,
	})
	if !handled {
		t.Fatal("expected laddered cooldown to short-circuit the strategy")
	}
	if nextLadderedEntrySeq != 1 || len(ladderedEntries) != 1 {
		t.Fatal("expected ladder state to stay unchanged while cooldown is active")
	}
}

func TestRealbotHandleLadderedStrategyBlocksTerminalHighBidEntry(t *testing.T) {
	engine := paper.NewEngine(100)
	tui := paper.NewTUI(engine, nil)
	tui.InitSettings(paper.TUISettings{
		PaperArbMode:                  paperArbModeLaddered,
		LadderedTakerReentryMoveCents: 5.0,
		MinAskPrice:                   0.01,
		MaxAskPrice:                   0.99,
	}, nil)

	entryExecutionInFlight := false
	panicBuyCooldown := time.Time{}
	lastPairUpdate := time.Now()
	lastTrade := time.Time{}
	lastDustRecoveryNotice := time.Time{}
	ladderedEntries := []realbotLadderedEntry{{seq: 1, ask0: 0.54, ask1: 0.56, side: 1, rung: 1}}
	nextLadderedEntrySeq := uint64(1)

	handled := realbotHandleLadderedStrategy(realbotPanicBuyStrategyArgs{
		ctx:            context.Background(),
		marketID:       "BTC",
		outcomes:       []string{"Down", "Up"},
		tokenToOutcome: map[string]string{"down-token": "Down", "up-token": "Up"},
		tokenBids:      map[string]float64{"Down": 0.99, "Up": 0.34},
		tokenAsks:      map[string]float64{"Down": 0.90, "Up": 0.56},
		tui:            tui,
		engine:         engine,
		arbMode:        paperArbModeLaddered,
	}, &realbotPanicBuyStrategyState{
		lastPairUpdate:         &lastPairUpdate,
		ladderedEntries:        &ladderedEntries,
		nextLadderedEntrySeq:   &nextLadderedEntrySeq,
		panicBuyCooldown:       &panicBuyCooldown,
		lastTrade:              &lastTrade,
		lastDustRecoveryNotice: &lastDustRecoveryNotice,
		entryExecutionInFlight: &entryExecutionInFlight,
	})
	if !handled {
		t.Fatal("expected terminal ladder quote to be handled")
	}
	if entryExecutionInFlight {
		t.Fatal("expected terminal ladder quote to avoid launching entry execution")
	}
	if nextLadderedEntrySeq != 1 || len(ladderedEntries) != 1 {
		t.Fatal("expected terminal ladder quote to leave ladder state unchanged")
	}
	if panicBuyCooldown.IsZero() {
		t.Fatal("expected terminal ladder block to apply a short cooldown")
	}
}

func TestRealbotRefreshLadderedPreTradeQuoteOverridesSwappedWSQuotes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/book" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		ts := time.Now().UTC().Format(time.RFC3339Nano)
		switch r.URL.Query().Get("token_id") {
		case "down-token":
			_, _ = w.Write([]byte("{\"asset_id\":\"down-token\",\"timestamp\":\"" + ts + "\",\"bids\":[{\"price\":\"0.33\",\"size\":\"7\"}],\"asks\":[{\"price\":\"0.34\",\"size\":\"9\"}]}"))
		case "up-token":
			_, _ = w.Write([]byte("{\"asset_id\":\"up-token\",\"timestamp\":\"" + ts + "\",\"bids\":[{\"price\":\"0.64\",\"size\":\"8\"}],\"asks\":[{\"price\":\"0.65\",\"size\":\"10\"}]}"))
		default:
			http.Error(w, "unexpected token: "+r.URL.String(), http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := api.NewRestClient("polymarket")
	client.BaseURL = server.URL
	lastPairUpdate := time.Now()
	cooldown := time.Time{}
	tokenBids := map[string]float64{"Down": 0.64, "Up": 0.33}
	tokenAsks := map[string]float64{"Down": 0.65, "Up": 0.34}
	tokenFullBids := map[string][]paper.MarketLevel{
		"Down": {{Price: 0.64, Size: 7}},
		"Up":   {{Price: 0.33, Size: 8}},
	}
	tokenFullAsks := map[string][]paper.MarketLevel{
		"Down": {{Price: 0.65, Size: 9}},
		"Up":   {{Price: 0.34, Size: 10}},
	}
	quoteState := map[string]realbotQuoteState{
		"Down": {UpdatedAt: time.Now(), Source: "ws"},
		"Up":   {UpdatedAt: time.Now(), Source: "ws"},
	}

	ok := realbotRefreshLadderedPreTradeQuote(realbotPanicBuyStrategyArgs{
		ctx:           context.Background(),
		marketID:      "BTC",
		market:        &api.Market{Tokens: []api.Token{{TokenID: "down-token", Outcome: "Down"}, {TokenID: "up-token", Outcome: "Up"}}},
		outcomes:      []string{"Down", "Up"},
		tokenBids:     tokenBids,
		tokenAsks:     tokenAsks,
		tokenFullBids: tokenFullBids,
		tokenFullAsks: tokenFullAsks,
		quoteState:    quoteState,
		restClient:    client,
	}, &realbotPanicBuyStrategyState{
		lastPairUpdate:    &lastPairUpdate,
		lastReconnectTime: &lastPairUpdate,
	}, func(d time.Duration) {
		cooldown = time.Now().Add(d)
	})

	if !ok {
		t.Fatal("expected REST ladder quote confirmation to succeed")
	}
	if !cooldown.IsZero() {
		t.Fatalf("expected successful confirmation not to set cooldown, got %v", cooldown)
	}
	if math.Abs(tokenAsks["Down"]-0.34) > 0.000001 || math.Abs(tokenAsks["Up"]-0.65) > 0.000001 {
		t.Fatalf("expected REST asks to replace swapped WS asks, got Down=%.3f Up=%.3f", tokenAsks["Down"], tokenAsks["Up"])
	}
	if quoteState["Down"].Source != "rest-exec" || quoteState["Up"].Source != "rest-exec" {
		t.Fatalf("expected REST quote state, got Down=%q Up=%q", quoteState["Down"].Source, quoteState["Up"].Source)
	}
}

func TestRealbotRefreshLadderedPreTradeQuoteUsesFreshLocalQuoteWithoutRest(t *testing.T) {
	restCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		restCalls++
		http.Error(w, "unexpected REST call", http.StatusInternalServerError)
	}))
	defer server.Close()

	client := api.NewRestClient("polymarket")
	client.BaseURL = server.URL
	lastPairUpdate := time.Now()
	cooldown := time.Time{}
	tokenBids := map[string]float64{"Down": 0.51, "Up": 0.46}
	tokenAsks := map[string]float64{"Down": 0.52, "Up": 0.47}
	tokenFullBids := map[string][]paper.MarketLevel{
		"Down": {{Price: 0.51, Size: 7}},
		"Up":   {{Price: 0.46, Size: 8}},
	}
	tokenFullAsks := map[string][]paper.MarketLevel{
		"Down": {{Price: 0.52, Size: 9}},
		"Up":   {{Price: 0.47, Size: 10}},
	}

	ok := realbotRefreshLadderedPreTradeQuote(realbotPanicBuyStrategyArgs{
		ctx:           context.Background(),
		marketID:      "BTC",
		market:        &api.Market{Tokens: []api.Token{{TokenID: "down-token", Outcome: "Down"}, {TokenID: "up-token", Outcome: "Up"}}},
		outcomes:      []string{"Down", "Up"},
		tokenBids:     tokenBids,
		tokenAsks:     tokenAsks,
		tokenFullBids: tokenFullBids,
		tokenFullAsks: tokenFullAsks,
		restClient:    client,
	}, &realbotPanicBuyStrategyState{
		lastPairUpdate: &lastPairUpdate,
	}, func(d time.Duration) {
		cooldown = time.Now().Add(d)
	})

	if !ok {
		t.Fatal("expected fresh local ladder quote to be accepted")
	}
	if restCalls != 0 {
		t.Fatalf("expected no REST confirmation for normal fresh quote, got %d calls", restCalls)
	}
	if !cooldown.IsZero() {
		t.Fatalf("expected fresh local quote not to set cooldown, got %v", cooldown)
	}
}

func TestRealbotRefreshLadderedPreTradeQuoteBlocksTerminalRestBook(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/book" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		ts := time.Now().UTC().Format(time.RFC3339Nano)
		switch r.URL.Query().Get("token_id") {
		case "down-token":
			_, _ = w.Write([]byte("{\"asset_id\":\"down-token\",\"timestamp\":\"" + ts + "\",\"bids\":[{\"price\":\"0.99\",\"size\":\"7\"}],\"asks\":[{\"price\":\"0.999\",\"size\":\"9\"}]}"))
		case "up-token":
			_, _ = w.Write([]byte("{\"asset_id\":\"up-token\",\"timestamp\":\"" + ts + "\",\"bids\":[{\"price\":\"0.55\",\"size\":\"8\"}],\"asks\":[{\"price\":\"0.56\",\"size\":\"10\"}]}"))
		default:
			http.Error(w, "unexpected token: "+r.URL.String(), http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := api.NewRestClient("polymarket")
	client.BaseURL = server.URL
	lastPairUpdate := time.Now()
	cooldown := time.Time{}
	tokenBids := map[string]float64{"Down": 0.41, "Up": 0.57}
	tokenAsks := map[string]float64{"Down": 0.42, "Up": 0.58}
	tokenFullBids := map[string][]paper.MarketLevel{}
	tokenFullAsks := map[string][]paper.MarketLevel{}
	quoteState := map[string]realbotQuoteState{
		"Down": {UpdatedAt: time.Now(), Source: "ws"},
		"Up":   {UpdatedAt: time.Now(), Source: "ws"},
	}

	ok := realbotRefreshLadderedPreTradeQuote(realbotPanicBuyStrategyArgs{
		ctx:           context.Background(),
		marketID:      "BTC",
		market:        &api.Market{Tokens: []api.Token{{TokenID: "down-token", Outcome: "Down"}, {TokenID: "up-token", Outcome: "Up"}}},
		outcomes:      []string{"Down", "Up"},
		tokenBids:     tokenBids,
		tokenAsks:     tokenAsks,
		tokenFullBids: tokenFullBids,
		tokenFullAsks: tokenFullAsks,
		quoteState:    quoteState,
		restClient:    client,
		tui:           paper.NewTUI(paper.NewEngine(100), nil),
	}, &realbotPanicBuyStrategyState{
		lastPairUpdate:    &lastPairUpdate,
		lastReconnectTime: &lastPairUpdate,
	}, func(d time.Duration) {
		cooldown = time.Now().Add(d)
	})

	if ok {
		t.Fatal("expected terminal REST ladder quote confirmation to block entry")
	}
	if cooldown.IsZero() {
		t.Fatal("expected terminal REST ladder block to set cooldown")
	}
	if math.Abs(tokenBids["Down"]-0.99) > 0.000001 {
		t.Fatalf("expected REST terminal bid to be observed before block, got %.3f", tokenBids["Down"])
	}
}
