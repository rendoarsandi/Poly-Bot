# Plan: Fix Copytrade Delay and Missing Trades

## Objective
Address the reported issues in the copytrade strategy where trades are being missed and there is significant execution delay compared to the master trader.

## Background & Motivation
1. **Missing Trades:** A bug in the trade de-duplication logic (`perKeyOccurrence` resetting per poll) causes identical trades (same timestamp, size, price) to be skipped if they are split across subsequent polling snapshots.
2. **Execution Delay:** The bot relies on REST polling by default, which is subject to Polymarket Data API lag. Additionally, trades are processed sequentially with a 2.5s confirmation timeout each.

## Proposed Changes

### 1. Fix De-duplication Logic
- Update `paperbotCopytradeState` and `realbotCopytradeState` to include a persistent `seenCount` map to track the total number of trades seen for each `baseKey`.
- Modify `paperbotCopytradeFreshTrades` and `realbotCopytradeFreshTrades` to use this global count instead of a local `perKeyOccurrence` counter.
- This ensures that if a poll result shifts (e.g., seeing only the tail end of a batch of identical trades), the bot correctly identifies them as new if the total count exceeds what was previously seen.

### 2. Improve Latency Documentation & Hints
- Provide clearer guidance or logs when the bot is running in REST-only mode, recommending the use of `COPYTRADE_PENDING_WS_URL` and `POLYGON_WS_URL` for near-instant trade detection via mempool/on-chain watchers.

### 3. Execution Optimization (Optional/Standard Task)
- Consider processing `freshTrades` for the same market in a single turn more aggressively, potentially reducing the confirmation wait if multiple orders are pending. (Will keep it simple first by fixing the bug).

## Implementation Steps

### Step 1: Update State Structures
Modify `cmd/paperbot/main.go` and `cmd/realbot/main.go` to add `seenCount map[string]int` to the copytrade state structs.

### Step 2: Update Fresh Trades Logic
Update the de-duplication loop in both files to:
1. Group trades by `baseKey` in the current poll.
2. For each `baseKey`, determine how many are "new" by comparing current count vs. `seenCount`.
3. Update `seenCount`.

### Step 3: Verification
- Add/Update tests in `cmd/paperbot/main_test.go` and `cmd/realbot/main_test.go` to simulate split polls with identical trades.

## Verification & Testing
- Run existing tests: `go test ./cmd/paperbot/...` and `go test ./cmd/realbot/...`.
- Add a new test case for "Identical Trades Across Polls" to confirm the fix.
