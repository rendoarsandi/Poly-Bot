package main

import (
	"context"
	"fmt"
	"math"
	"time"

	"Market-bot/internal/trading"
)

func subtractMergedPairBalances(bal0, bal1, mergeQty float64) (float64, float64) {
	if mergeQty <= 0 {
		return bal0, bal1
	}
	return math.Max(0, bal0-mergeQty), math.Max(0, bal1-mergeQty)
}

func preferLivePairBalances(live0, live1, backup0, backup1 float64) (float64, float64) {
	return math.Max(live0, backup0), math.Max(live1, backup1)
}

func combinePairBalanceSnapshots(live0, live1, backup0, backup1 float64, backupErr error) (bal0, bal1 float64, source string, err error) {
	hasLive := shouldAttemptCleanupSell(live0) || shouldAttemptCleanupSell(live1)
	hasBackup := shouldAttemptCleanupSell(backup0) || shouldAttemptCleanupSell(backup1)

	if backupErr != nil {
		if hasLive {
			return live0, live1, "live WS", nil
		}
		return 0, 0, "", backupErr
	}

	bal0, bal1 = preferLivePairBalances(live0, live1, backup0, backup1)
	source = "live WS"
	switch {
	case hasLive && hasBackup:
		source = "live WS + on-chain backup"
	case hasBackup:
		source = "on-chain backup"
	}
	return bal0, bal1, source, nil
}

func loadPairBalancesWSFirst(ctx context.Context, trader *trading.RealTrader, token0, token1 string) (bal0, bal1 float64, source string, err error) {
	live0 := trader.GetLivePositionSize(token0)
	live1 := trader.GetLivePositionSize(token1)
	backup0, backup1, backupErr := loadPairBalances(ctx, trader, token0, token1)
	return combinePairBalanceSnapshots(live0, live1, backup0, backup1, backupErr)
}

func loadPairBalances(ctx context.Context, trader *trading.RealTrader, token0, token1 string) (float64, float64, error) {
	pos0, pos1, posErr := loadPairPositionBalances(ctx, trader, token0, token1)
	onChain0, onChain1, onChainErr := loadPairOnChainBalances(ctx, trader, token0, token1)

	switch {
	case posErr == nil && onChainErr == nil:
		bal0, bal1 := preferLivePairBalances(pos0, pos1, onChain0, onChain1)
		return bal0, bal1, nil
	case onChainErr == nil:
		return onChain0, onChain1, nil
	case posErr == nil:
		return pos0, pos1, nil
	default:
		return 0, 0, fmt.Errorf("external position snapshot failed (%v); on-chain backup failed (%v)", posErr, onChainErr)
	}
}

func loadPairPositionBalances(ctx context.Context, trader *trading.RealTrader, token0, token1 string) (float64, float64, error) {
	positions, err := trader.GetPositions(ctx)
	if err != nil {
		return 0, 0, err
	}
	bal0, bal1 := pairBalancesFromPositions(positions, token0, token1)
	return bal0, bal1, nil
}

func loadPairOnChainBalances(ctx context.Context, trader *trading.RealTrader, token0, token1 string) (float64, float64, error) {
	bal0, err0 := trader.GetCTFBalanceFloat(ctx, token0)
	bal1, err1 := trader.GetCTFBalanceFloat(ctx, token1)
	if err0 != nil || err1 != nil {
		return bal0, bal1, fmt.Errorf("on-chain balance check failed (err0=%v err1=%v)", err0, err1)
	}
	return bal0, bal1, nil
}

func incrementalBalance(initial, current float64) float64 {
	if current <= initial {
		return 0
	}
	return current - initial
}

func acquiredPairBalances(initial0, initial1, current0, current1 float64, haveInitialSnapshot bool) (float64, float64) {
	if !haveInitialSnapshot {
		return current0, current1
	}
	return incrementalBalance(initial0, current0), incrementalBalance(initial1, current1)
}

func queryLivePairBalanceDelta(ctx context.Context, trader *trading.RealTrader, token0, token1 string, initial0, initial1 float64, haveInitialSnapshot bool) (acquired0, acquired1, bal0, bal1 float64) {
	for {
		bal0 = trader.GetLivePositionSize(token0)
		bal1 = trader.GetLivePositionSize(token1)
		acquired0, acquired1 = acquiredPairBalances(initial0, initial1, bal0, bal1, haveInitialSnapshot)
		if shouldAttemptCleanupSell(acquired0) || shouldAttemptCleanupSell(acquired1) {
			return acquired0, acquired1, bal0, bal1
		}
		select {
		case <-ctx.Done():
			return acquired0, acquired1, bal0, bal1
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func queryOnChainPairBalanceDelta(ctx context.Context, trader *trading.RealTrader, token0, token1 string, initial0, initial1 float64, haveInitialSnapshot bool) (acquired0, acquired1, bal0, bal1 float64, err error) {
	for {
		bal0, bal1, err = loadPairOnChainBalances(ctx, trader, token0, token1)
		if err == nil {
			acquired0, acquired1 = acquiredPairBalances(initial0, initial1, bal0, bal1, haveInitialSnapshot)
			if shouldAttemptCleanupSell(acquired0) || shouldAttemptCleanupSell(acquired1) {
				return acquired0, acquired1, bal0, bal1, nil
			}
		}
		select {
		case <-ctx.Done():
			return acquired0, acquired1, bal0, bal1, err
		case <-time.After(750 * time.Millisecond):
		}
	}
}

func reconcileBoughtPairBalances(ctx context.Context, trader *trading.RealTrader, token0, token1 string, initial0, initial1 float64, haveInitialSnapshot bool) (acquired0, acquired1, bal0, bal1 float64, source string, err error) {
	liveWindow := 2 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < liveWindow {
			liveWindow = remaining
		}
	}
	if liveWindow < 0 {
		liveWindow = 0
	}

	var live0, live1 float64
	if liveWindow > 0 {
		liveCtx, cancel := context.WithTimeout(ctx, liveWindow)
		defer cancel()
		acquired0, acquired1, live0, live1 = queryLivePairBalanceDelta(liveCtx, trader, token0, token1, initial0, initial1, haveInitialSnapshot)
		source = "live WS"
	}

	onChainAcquired0, onChainAcquired1, onChain0, onChain1, onChainErr := queryOnChainPairBalanceDelta(ctx, trader, token0, token1, initial0, initial1, haveInitialSnapshot)
	if onChainErr == nil {
		acquired0 = math.Max(acquired0, onChainAcquired0)
		acquired1 = math.Max(acquired1, onChainAcquired1)
		bal0, bal1 = preferLivePairBalances(live0, live1, onChain0, onChain1)
		if source == "" {
			source = "on-chain delta"
		} else {
			source += " + on-chain delta"
		}
		return acquired0, acquired1, bal0, bal1, source, nil
	}

	if shouldAttemptCleanupSell(acquired0) || shouldAttemptCleanupSell(acquired1) {
		return acquired0, acquired1, live0, live1, source, nil
	}
	if source == "" {
		source = "unavailable"
	}
	return acquired0, acquired1, live0, live1, source, onChainErr
}
