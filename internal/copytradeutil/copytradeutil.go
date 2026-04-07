package copytradeutil

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
)

const minTrackedShares = 0.01

type FreshTradeState struct {
	StartedAt          time.Time
	TradesSeeded       bool
	SeenTradeKeys      map[string]time.Time
	SeenTradeKeysCount map[string]int
}

type FreshTradeOptions struct {
	Now                     time.Time
	ConditionID             string
	MinSize                 float64
	DropBelowMinBeforeDedup bool
	AllowBelowMin           bool
	BootstrapMaxAge         time.Duration
}

type PositionState struct {
	TradesSeeded         bool
	TargetShares         map[string]float64
	TargetSeen           map[string]bool
	LastTargetPoll       map[string]time.Time
	PendingSellTarget    map[string]float64
	PendingSellPoll      map[string]time.Time
	ObservedBuySizeSum   map[string]float64
	ObservedBuySizeCount map[string]int
}

func TargetShares(positions []api.Position) map[string]float64 {
	return TargetSharesForCondition(positions, "")
}

func SharesByCondition(positions []api.Position) map[string]map[string]float64 {
	sharesByCondition := make(map[string]map[string]float64)
	for _, pos := range positions {
		conditionID := strings.TrimSpace(pos.ConditionID)
		outcome := NormalizeOutcome(pos.Outcome)
		if conditionID == "" || outcome == "" || pos.Size <= minTrackedShares {
			continue
		}
		outcomeShares := sharesByCondition[conditionID]
		if outcomeShares == nil {
			outcomeShares = make(map[string]float64)
			sharesByCondition[conditionID] = outcomeShares
		}
		outcomeShares[outcome] += pos.Size
	}
	return sharesByCondition
}

func TargetSharesForCondition(positions []api.Position, conditionID string) map[string]float64 {
	shares := make(map[string]float64, len(positions))
	conditionID = strings.TrimSpace(conditionID)
	for _, pos := range positions {
		if conditionID != "" && !strings.EqualFold(strings.TrimSpace(pos.ConditionID), conditionID) {
			continue
		}
		outcome := NormalizeOutcome(pos.Outcome)
		if outcome == "" || pos.Size <= minTrackedShares {
			continue
		}
		shares[outcome] += pos.Size
	}
	return shares
}

func HoldsBothOutcomes(targetShares map[string]float64) bool {
	held := 0
	for _, qty := range targetShares {
		if qty > minTrackedShares {
			held++
			if held >= 2 {
				return true
			}
		}
	}
	return false
}

func HasAmbiguousPositionExit(positions []api.Position, conditionID string) bool {
	conditionID = strings.TrimSpace(conditionID)
	for _, pos := range positions {
		if conditionID != "" && !strings.EqualFold(strings.TrimSpace(pos.ConditionID), conditionID) {
			continue
		}
		if pos.Size <= minTrackedShares {
			continue
		}
		if pos.Mergeable || pos.Redeemable {
			return true
		}
	}
	return false
}

func NormalizeConditionIDs(conditionIDs []string) []string {
	seen := make(map[string]struct{}, len(conditionIDs))
	normalized := make([]string, 0, len(conditionIDs))
	for _, conditionID := range conditionIDs {
		conditionID = strings.TrimSpace(conditionID)
		if conditionID == "" {
			continue
		}
		if _, exists := seen[conditionID]; exists {
			continue
		}
		seen[conditionID] = struct{}{}
		normalized = append(normalized, conditionID)
	}
	return normalized
}

func NormalizeOutcome(outcome string) string {
	return core.SanitizeString(strings.TrimSpace(outcome))
}

func CloneOutcomeShares(shares map[string]float64) map[string]float64 {
	if len(shares) == 0 {
		return map[string]float64{}
	}
	cloned := make(map[string]float64, len(shares))
	for outcome, qty := range shares {
		cloned[outcome] = qty
	}
	return cloned
}

func IsRateLimited(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "status 429") || strings.Contains(msg, "error code: 1015")
}

func RateLimitBackoff(streak int) time.Duration {
	if streak < 1 {
		return 0
	}
	backoff := time.Second
	for i := 1; i < streak; i++ {
		backoff *= 2
		if backoff >= 8*time.Second {
			return 8 * time.Second
		}
	}
	return backoff
}

func FilterTradesByCondition(trades []api.PublicTrade, conditionID string) []api.PublicTrade {
	if strings.TrimSpace(conditionID) == "" || len(trades) == 0 {
		return trades
	}
	filtered := make([]api.PublicTrade, 0, len(trades))
	for _, trade := range trades {
		if strings.EqualFold(strings.TrimSpace(trade.ConditionID), conditionID) {
			filtered = append(filtered, trade)
		}
	}
	return filtered
}

func FilterPositionsByCondition(positions []api.Position, conditionID string) []api.Position {
	if strings.TrimSpace(conditionID) == "" || len(positions) == 0 {
		return positions
	}
	filtered := make([]api.Position, 0, len(positions))
	for _, pos := range positions {
		if strings.EqualFold(strings.TrimSpace(pos.ConditionID), conditionID) {
			filtered = append(filtered, pos)
		}
	}
	return filtered
}

func TradeFetchTimeout(pollEvery time.Duration) time.Duration {
	if pollEvery < 250*time.Millisecond {
		pollEvery = 250 * time.Millisecond
	}
	timeout := pollEvery * 4
	if timeout < 1500*time.Millisecond {
		timeout = 1500 * time.Millisecond
	}
	if timeout > 2500*time.Millisecond {
		timeout = 2500 * time.Millisecond
	}
	return timeout
}

func PositionFetchTimeout(pollEvery time.Duration) time.Duration {
	timeout := TradeFetchTimeout(pollEvery) * 2
	if timeout < 4*time.Second {
		timeout = 4 * time.Second
	}
	if timeout > 8*time.Second {
		timeout = 8 * time.Second
	}
	return timeout
}

func CanReusePositions(now, lastRefresh time.Time, pollEvery time.Duration) bool {
	if lastRefresh.IsZero() || now.IsZero() {
		return false
	}
	maxAge := pollEvery * 3
	if maxAge < 5*time.Second {
		maxAge = 5 * time.Second
	}
	if maxAge > 15*time.Second {
		maxAge = 15 * time.Second
	}
	return now.Sub(lastRefresh) <= maxAge
}

func TradeKey(trade api.PublicTrade) string {
	if signalID := strings.TrimSpace(trade.SignalID); signalID != "" {
		return "signal|" + signalID
	}
	txHash := strings.TrimSpace(trade.TransactionHash)
	if txHash != "" {
		return fmt.Sprintf("%s|%s|%s|%.6f|%.6f|%s|%d|%s",
			strings.TrimSpace(trade.ConditionID),
			NormalizeOutcome(trade.Outcome),
			strings.ToUpper(strings.TrimSpace(trade.Side)),
			trade.Size,
			trade.Price,
			strings.TrimSpace(trade.Asset),
			trade.Timestamp,
			txHash,
		)
	}
	return fmt.Sprintf("%s|%d|%s|%s|%.6f",
		strings.TrimSpace(trade.ConditionID),
		trade.Timestamp,
		NormalizeOutcome(trade.Outcome),
		strings.ToUpper(strings.TrimSpace(trade.Side)),
		trade.Size,
	)
}

func EffectiveTimestamp(trade api.PublicTrade) int64 {
	if trade.ObservedAt > 0 {
		return trade.ObservedAt
	}
	return trade.Timestamp
}

func SignalSource(trade api.PublicTrade) string {
	label := strings.TrimSpace(trade.Source)
	if label != "" {
		return label
	}
	if trade.Timestamp == 0 {
		return "position"
	}
	return "trade"
}

func SignalSourceLabel(trade api.PublicTrade) string {
	switch strings.ToLower(strings.TrimSpace(SignalSource(trade))) {
	case "mempool":
		return "MEMPOOL"
	case "onchain":
		return "ONCHAIN"
	case "position", "position-estimate":
		return "POSITION"
	case "public":
		return "PUBLIC"
	default:
		return strings.ToUpper(strings.TrimSpace(SignalSource(trade)))
	}
}

func NormalizeSignalID(trade api.PublicTrade) string {
	if signalID := strings.TrimSpace(trade.SignalID); signalID != "" {
		return signalID
	}
	txHash := strings.TrimSpace(trade.TransactionHash)
	asset := strings.TrimSpace(trade.Asset)
	side := strings.ToUpper(strings.TrimSpace(trade.Side))
	if txHash == "" || asset == "" || side == "" {
		return ""
	}
	return fmt.Sprintf("%s:%s:%s", txHash, asset, side)
}

func PrepareTrades(trades []api.PublicTrade, source string) []api.PublicTrade {
	if len(trades) == 0 {
		return nil
	}
	prepared := make([]api.PublicTrade, 0, len(trades))
	for _, trade := range trades {
		normalized := trade
		if strings.TrimSpace(normalized.Source) == "" && strings.TrimSpace(source) != "" {
			normalized.Source = source
		}
		normalized.SignalID = NormalizeSignalID(normalized)
		prepared = append(prepared, normalized)
	}
	return prepared
}

func MergeTrades(groups ...[]api.PublicTrade) []api.PublicTrade {
	total := 0
	for _, group := range groups {
		total += len(group)
	}
	if total == 0 {
		return nil
	}

	merged := make([]api.PublicTrade, 0, total)
	seenSignals := make(map[string]string, total)
	for _, group := range groups {
		for _, trade := range group {
			key := NormalizeSignalID(trade)
			if key != "" {
				source := strings.TrimSpace(trade.Source)
				if seenSource, exists := seenSignals[key]; exists {
					if seenSource != source {
						continue
					}
				} else {
					seenSignals[key] = source
				}
			}
			merged = append(merged, trade)
		}
	}
	return merged
}

func BootstrapStartTimestamp(startedAt time.Time) int64 {
	if startedAt.IsZero() {
		return 0
	}
	startTs := startedAt.Unix()
	if startedAt.Nanosecond() != 0 {
		startTs--
	}
	return startTs
}

func BootstrapAcceptsTrade(startedAt time.Time, maxAge time.Duration, trade api.PublicTrade) bool {
	if startedAt.IsZero() {
		return false
	}
	startTs := BootstrapStartTimestamp(startedAt)
	effectiveTS := EffectiveTimestamp(trade)
	if effectiveTS >= startTs {
		return true
	}

	source := strings.ToLower(strings.TrimSpace(SignalSource(trade)))
	if source != "onchain" && source != "mempool" {
		return false
	}

	tradeAt := time.Unix(effectiveTS, 0)
	return !tradeAt.Before(startedAt.Add(-maxAge))
}

func RetrySignalFresh(now time.Time, maxAge time.Duration, trade api.PublicTrade) bool {
	effectiveTS := EffectiveTimestamp(trade)
	if effectiveTS <= 0 {
		return true
	}
	tradeAt := time.Unix(effectiveTS, 0)
	if now.Before(tradeAt) {
		return true
	}
	return now.Sub(tradeAt) <= maxAge
}

func TakeRetryTrades(retries []api.PublicTrade, now time.Time, maxAge time.Duration) []api.PublicTrade {
	if len(retries) == 0 {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	fresh := make([]api.PublicTrade, 0, len(retries))
	for _, trade := range retries {
		if RetrySignalFresh(now, maxAge, trade) {
			fresh = append(fresh, trade)
		}
	}
	return fresh
}

func QueueRetryTrades(existing, retries []api.PublicTrade, queueCap int) []api.PublicTrade {
	if len(retries) == 0 {
		return existing
	}
	if queueCap > 0 && len(retries) > queueCap {
		retries = retries[len(retries)-queueCap:]
	}
	queued := append(existing, retries...)
	if queueCap > 0 && len(queued) > queueCap {
		queued = append([]api.PublicTrade(nil), queued[len(queued)-queueCap:]...)
	}
	return queued
}

func FreshTrades(state *FreshTradeState, trades []api.PublicTrade, opts FreshTradeOptions) []api.PublicTrade {
	if state == nil || len(trades) == 0 {
		return nil
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if opts.MinSize <= 0 {
		opts.MinSize = minTrackedShares
	}
	if state.SeenTradeKeys == nil {
		state.SeenTradeKeys = make(map[string]time.Time)
	}
	if state.SeenTradeKeysCount == nil {
		state.SeenTradeKeysCount = make(map[string]int)
	}

	conditionID := strings.TrimSpace(opts.ConditionID)
	for key, seenAt := range state.SeenTradeKeys {
		if opts.Now.Sub(seenAt) > 15*time.Minute {
			delete(state.SeenTradeKeys, key)
		}
	}

	filtered := make([]api.PublicTrade, 0, len(trades))
	for _, trade := range trades {
		if conditionID != "" && !strings.EqualFold(strings.TrimSpace(trade.ConditionID), conditionID) {
			continue
		}
		if NormalizeOutcome(trade.Outcome) == "" {
			continue
		}
		if opts.DropBelowMinBeforeDedup && trade.Size <= opts.MinSize {
			continue
		}
		filtered = append(filtered, trade)
	}
	sort.Slice(filtered, func(i, j int) bool {
		leftTS := EffectiveTimestamp(filtered[i])
		rightTS := EffectiveTimestamp(filtered[j])
		if leftTS == rightTS {
			return TradeKey(filtered[i]) < TradeKey(filtered[j])
		}
		return leftTS < rightTS
	})

	fresh := make([]api.PublicTrade, 0, len(filtered))
	currentPollCounts := make(map[string]int)
	for _, trade := range filtered {
		baseKey := TradeKey(trade)
		currentPollCounts[baseKey]++

		totalSeen := state.SeenTradeKeysCount[baseKey]
		if currentPollCounts[baseKey] > totalSeen {
			state.SeenTradeKeysCount[baseKey] = currentPollCounts[baseKey]
			state.SeenTradeKeys[fmt.Sprintf("%s#%d", baseKey, currentPollCounts[baseKey])] = opts.Now
			if trade.Size <= opts.MinSize && !opts.AllowBelowMin {
				continue
			}
			fresh = append(fresh, trade)
		}
	}
	if !state.TradesSeeded {
		state.TradesSeeded = true
		if state.StartedAt.IsZero() {
			return nil
		}
		bootstrap := make([]api.PublicTrade, 0, len(fresh))
		for _, trade := range fresh {
			if !BootstrapAcceptsTrade(state.StartedAt, opts.BootstrapMaxAge, trade) {
				continue
			}
			bootstrap = append(bootstrap, trade)
		}
		sort.Slice(bootstrap, func(i, j int) bool {
			leftTS := EffectiveTimestamp(bootstrap[i])
			rightTS := EffectiveTimestamp(bootstrap[j])
			if leftTS == rightTS {
				return TradeKey(bootstrap[i]) < TradeKey(bootstrap[j])
			}
			return leftTS < rightTS
		})
		return bootstrap
	}
	return fresh
}

func ClearPendingSell(state *PositionState, outcome string) {
	if state == nil || outcome == "" {
		return
	}
	delete(state.PendingSellTarget, outcome)
	delete(state.PendingSellPoll, outcome)
}

func TargetDelta(state *PositionState, outcome string, targetQty float64, pollTime time.Time) (float64, bool, bool) {
	if state == nil {
		return 0, false, false
	}
	outcome = NormalizeOutcome(outcome)
	if outcome == "" {
		return 0, false, false
	}
	if state.TargetShares == nil {
		state.TargetShares = make(map[string]float64)
	}
	if state.TargetSeen == nil {
		state.TargetSeen = make(map[string]bool)
	}
	if state.LastTargetPoll == nil {
		state.LastTargetPoll = make(map[string]time.Time)
	}
	if state.PendingSellTarget == nil {
		state.PendingSellTarget = make(map[string]float64)
	}
	if state.PendingSellPoll == nil {
		state.PendingSellPoll = make(map[string]time.Time)
	}
	if !state.TargetSeen[outcome] {
		state.TargetSeen[outcome] = true
		state.TargetShares[outcome] = targetQty
		state.LastTargetPoll[outcome] = pollTime
		ClearPendingSell(state, outcome)
		if state.TradesSeeded {
			return targetQty, true, false
		}
		return 0, false, false
	}
	if lastPoll := state.LastTargetPoll[outcome]; !lastPoll.IsZero() && lastPoll.Equal(pollTime) {
		return 0, false, false
	}
	state.LastTargetPoll[outcome] = pollTime

	prev := state.TargetShares[outcome]
	if targetQty > prev+minTrackedShares {
		state.TargetShares[outcome] = targetQty
		ClearPendingSell(state, outcome)
		return targetQty - prev, true, false
	}
	if targetQty >= prev-minTrackedShares {
		state.TargetShares[outcome] = targetQty
		ClearPendingSell(state, outcome)
		return 0, false, false
	}
	if _, waiting := state.PendingSellPoll[outcome]; waiting {
		state.TargetShares[outcome] = targetQty
		ClearPendingSell(state, outcome)
		return targetQty - prev, true, false
	}
	state.PendingSellTarget[outcome] = targetQty
	state.PendingSellPoll[outcome] = pollTime
	return 0, false, true
}

func ObserveBuySignal(state *PositionState, trade api.PublicTrade) {
	if state == nil {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(trade.Side), "BUY") {
		return
	}
	if trade.Size <= minTrackedShares {
		return
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(trade.Source)), "position") {
		return
	}
	outcome := NormalizeOutcome(trade.Outcome)
	if outcome == "" {
		return
	}
	if state.ObservedBuySizeSum == nil {
		state.ObservedBuySizeSum = make(map[string]float64)
	}
	if state.ObservedBuySizeCount == nil {
		state.ObservedBuySizeCount = make(map[string]int)
	}
	state.ObservedBuySizeSum[outcome] += trade.Size
	state.ObservedBuySizeCount[outcome]++
}

func EstimatedPositionBuySignals(state *PositionState, conditionID, outcome string, delta float64, mode string) []api.PublicTrade {
	outcome = NormalizeOutcome(outcome)
	if outcome == "" || delta <= minTrackedShares {
		return nil
	}
	if strings.EqualFold(mode, core.CopytradeSizingModePercent) {
		return []api.PublicTrade{{
			ConditionID: strings.TrimSpace(conditionID),
			Outcome:     outcome,
			Side:        "BUY",
			Size:        delta,
			Source:      "position",
		}}
	}

	estimatedTrades := 1
	if state != nil {
		if count := state.ObservedBuySizeCount[outcome]; count > 0 {
			avg := state.ObservedBuySizeSum[outcome] / float64(count)
			if avg > minTrackedShares {
				estimatedTrades = int(math.Ceil(delta / avg))
			}
		}
	}
	if estimatedTrades < 1 {
		estimatedTrades = 1
	}
	if estimatedTrades > 16 {
		estimatedTrades = 16
	}

	signals := make([]api.PublicTrade, 0, estimatedTrades)
	remaining := delta
	for i := 0; i < estimatedTrades; i++ {
		chunk := remaining / float64(estimatedTrades-i)
		if chunk <= minTrackedShares {
			continue
		}
		signals = append(signals, api.PublicTrade{
			ConditionID: strings.TrimSpace(conditionID),
			Outcome:     outcome,
			Side:        "BUY",
			Size:        chunk,
			Source:      "position-estimate",
		})
		remaining -= chunk
	}
	if len(signals) == 0 {
		return nil
	}
	return signals
}

func PositionSyncTrades(state *PositionState, conditionID string, outcomes []string, positions []api.Position, pollTime time.Time, freshTrades []api.PublicTrade, sizingMode string) ([]api.PublicTrade, map[string]float64) {
	if state == nil || pollTime.IsZero() {
		return nil, nil
	}
	if !strings.EqualFold(sizingMode, core.CopytradeSizingModePercent) {
		return nil, nil
	}

	targetShares := TargetSharesForCondition(positions, conditionID)
	holdsBoth := HoldsBothOutcomes(targetShares)
	ambiguousExit := HasAmbiguousPositionExit(positions, conditionID)

	freshBuySize := make(map[string]float64)
	freshSell := make(map[string]bool)
	for _, trade := range freshTrades {
		outcome := NormalizeOutcome(trade.Outcome)
		if outcome == "" {
			continue
		}
		switch strings.ToUpper(strings.TrimSpace(trade.Side)) {
		case "BUY":
			freshBuySize[outcome] += math.Max(0, trade.Size)
		case "SELL":
			freshSell[outcome] = true
		}
	}

	relevantOutcomes := make(map[string]struct{})
	for _, outcome := range outcomes {
		outcome = NormalizeOutcome(outcome)
		if outcome != "" {
			relevantOutcomes[outcome] = struct{}{}
		}
	}
	for outcome := range targetShares {
		relevantOutcomes[outcome] = struct{}{}
	}
	for outcome := range state.TargetSeen {
		if outcome != "" {
			relevantOutcomes[outcome] = struct{}{}
		}
	}
	if len(relevantOutcomes) == 0 {
		return nil, nil
	}

	targetDeltas := make(map[string]float64)
	syncTrades := make([]api.PublicTrade, 0)
	for outcome := range relevantOutcomes {
		targetQty := targetShares[outcome]
		delta, ready, pending := TargetDelta(state, outcome, targetQty, pollTime)
		if !ready || pending || math.Abs(delta) <= minTrackedShares {
			continue
		}
		targetDeltas[outcome] = delta
		switch {
		case delta > 0:
			if remaining := delta - freshBuySize[outcome]; remaining > minTrackedShares {
				syncTrades = append(syncTrades, EstimatedPositionBuySignals(state, strings.TrimSpace(conditionID), outcome, remaining, sizingMode)...)
			}
		case delta < 0 && !freshSell[outcome] && !holdsBoth && !ambiguousExit:
			syncTrades = append(syncTrades, api.PublicTrade{
				ConditionID: strings.TrimSpace(conditionID),
				Outcome:     outcome,
				Side:        "SELL",
				Size:        -delta,
				Timestamp:   pollTime.Unix(),
				Source:      "position",
			})
		}
	}

	sort.Slice(syncTrades, func(i, j int) bool {
		if syncTrades[i].Outcome == syncTrades[j].Outcome {
			return syncTrades[i].Side < syncTrades[j].Side
		}
		return syncTrades[i].Outcome < syncTrades[j].Outcome
	})

	return syncTrades, targetDeltas
}
