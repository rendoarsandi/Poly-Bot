# Refactor Roadmap

Date: 2026-04-07

## Current checkpoint

- Branch: `refactor/shared-runtime-phase1`
- Estimated completion: `55-60%`
- Estimated remaining: `40-45%`

### Completed so far

- Added shared mode and mode-policy helpers in `internal/botmode`.
- Added shared config and TUI settings mapping in `internal/runtimecfg`.
- Added shared quote/session helpers in `internal/quoteutil`.
- Extracted most copytrade normalization, polling, watcher lifecycle, runtime state, sizing, formatting, and round discovery into `internal/copytradeutil`.
- Moved split-strategy gating into shared mode/config logic so split no longer overlaps with non-taker entry modes implicitly.
- Reduced `cmd/paperbot` and `cmd/realbot` to thinner wrappers around shared copytrade runtime code.
- Added regression coverage for the new shared packages and preserved command-level checks for runtime-specific behavior.

### Remaining high-value work

1. Extract round/runtime orchestration out of `cmd/paperbot` and `cmd/realbot` into a shared runtime package.
2. Move non-copytrade strategy flows into shared packages:
   - taker / pair arb
   - laddered taker
   - maker
   - binance-gap
   - split inventory overlays
3. Introduce clearer execution adapters so paper and real share decision logic but not settlement internals.
4. Reduce command-package tests by moving shared behavior assertions fully into shared-package tests.

### Why the remaining percentage is harder

The easy wins were duplicated helpers and copytrade support code. What is left is the application-layer split: the main market loop, per-market lifecycle, and strategy dispatch still live primarily in the binaries. That work is riskier and touches more live behavior per edit.

## What the scan found

- `cmd/paperbot/main.go` is about 5.9k lines.
- `cmd/realbot/main.go` is about 8.1k lines.
- Both binaries still own too much domain logic:
  - market discovery and round orchestration
  - per-market lifecycle
  - quote freshness / REST fallback
  - strategy dispatch
  - copytrade polling and watcher management
  - maker inventory and quote maintenance
  - Binance-gap signal handling
  - split inventory handling
  - cleanup / merge / redemption paths

The project already has reusable packages like `internal/core`, `internal/markets`, `internal/strategy`, `internal/trading`, and parts of `internal/paper`, but the critical runtime logic still lives in the binaries.

## Main architectural problems

### 1. Entry points are acting like the application layer

`paperbot` and `realbot` should mostly bootstrap dependencies and start the bot. Right now they also contain the core trading runtime. That forces every behavior change to be copied twice.

### 2. Strategy selection is not modeled explicitly

Today there is one primary selector, `PaperArbMode`, plus several side toggles:

- `SplitStrategyEnabled`
- `TakerCloseMarket`
- exchange-specific gates like `kalshi`

That creates hidden composition rules instead of explicit ones. Example: in `realbot`, split inventory logic still runs in the same loop after the primary mode branch, so "one bot mode" can still execute multiple strategy families in a single tick.

### 3. Real and paper share decision logic but not structure

The actual differences are mostly execution and settlement:

- paper: simulated fills, merge, redemption, balances
- real: live orders, wallet truth, on-chain split / merge / redeem, user WS

But the code is split by binary, not by responsibility. As a result, quote checks, sizing, signal handling, copytrade filtering, and mode gating are duplicated even when behavior is conceptually the same.

### 4. `internal/paper` is overloaded

It currently holds:

- the TUI
- the simulation engine
- risk logic
- market monitor
- binance directional signal tracking
- inventory helpers used by both paper and real

Some of those belong to a generic runtime package, not a package named `paper`.

### 5. The `trading.Trader` interface is too thin for the current runtime

`internal/trading/trader.go` exposes a small `Trader` interface, but `realbot` still depends directly on many `RealTrader`-only methods such as:

- `StartUserWS`
- `SubscribeUserWSMarkets`
- `SplitOnChain`
- `MergeOnChain`
- `RedeemOnChain`
- `GetLivePositionSize`
- `UpdateBalanceAllowance`

That makes it hard to build one shared runtime with pluggable paper and real execution backends.

## Recommended target shape

### Keep the binaries thin

- `cmd/paperbot/main.go`
  - load config
  - build paper dependencies
  - start shared bot runtime
- `cmd/realbot/main.go`
  - load config
  - build real dependencies
  - start shared bot runtime

### Add a shared runtime package

Create an application-layer package, for example:

- `internal/runtime`

Suggested responsibilities:

- round loop
- market discovery
- per-market session start / stop
- shared quote freshness logic
- strategy dispatch
- restart / shutdown handling
- watcher lifecycle ownership

Possible files:

- `internal/runtime/bot.go`
- `internal/runtime/round.go`
- `internal/runtime/market_session.go`
- `internal/runtime/quote_state.go`
- `internal/runtime/watcher_registry.go`
- `internal/runtime/settings_adapter.go`

### Split strategy logic by behavior, not by bot type

Suggested packages:

- `internal/strategy/pairarb`
  - taker pair buy / merge
  - laddered taker entry rules
- `internal/strategy/maker`
  - maker quote plan generation
- `internal/strategy/copytrade`
  - target resolution
  - trade normalization
  - poll / watcher reconciliation logic
- `internal/strategy/binancegap`
  - signal evaluation
  - entry / exit decision logic
- `internal/strategy/split`
  - inventory policy
  - replenish policy
  - paired sell decision rules

The important rule is:

- strategy packages should decide
- execution packages should execute

### Separate execution from decision

Create execution adapters that hide the paper-vs-real difference:

- `internal/execution/paper`
- `internal/execution/real`

Both should satisfy a richer shared interface than the current `trading.Trader`.

Suggested shared interface groups:

- `OrderExecutor`
- `InventorySync`
- `SettlementExecutor`
- `MarketDataRefresh`

Not every strategy needs every capability. Pass smaller interfaces into strategy runners so they stay testable.

### Make strategy composition explicit

Use one struct for runtime policy instead of mixing `PaperArbMode` and loose booleans:

```go
type StrategyProfile struct {
    EntryMode        EntryMode
    ExitMode         ExitMode
    SplitInventory   bool
    TakerCloseMode   bool
    CopytradeEnabled bool
}
```

Then define hard rules in one place:

- exactly one entry strategy is active per market tick
- split inventory is an optional overlay, not an implicit side effect
- taker-close disables normal entry logic
- copytrade and maker are mutually exclusive
- exchange-specific restrictions are normalized before the runtime starts

## Best first refactor phases

### Phase 1: extract config and mode normalization

Move these out of the binaries first:

- config <-> `paper.TUISettings` mapping
- arb mode normalization
- loop interval helpers
- mode visibility / capability helpers

Why first:

- low risk
- easy to test
- removes repeated boilerplate immediately

### Phase 2: extract shared quote-state and market-session logic

Move into a shared runtime package:

- top-of-book and depth state
- quote staleness checks
- display quote sync
- REST fallback triggers
- WS reconnect heuristics

Why second:

- the paper and real loops both carry this logic already
- it is the largest shared seam after config mapping

### Phase 3: extract strategy runners

Start with the most duplicated families:

1. `copytrade`
2. `pairarb` / `laddered-taker`
3. `binance-gap`
4. `maker`
5. `split`

Each strategy package should expose something like:

```go
type Runner interface {
    Tick(ctx context.Context, snap MarketSnapshot) error
}
```

The runtime selects one runner per market plus optional overlays.

### Phase 4: isolate real-only settlement and wallet-truth flows

Keep these out of the shared runtime core:

- merge / split / redeem transactions
- user WS order confirmation
- wallet-truth reconciliation
- CTF balance verification

These belong in the real execution adapter and in real-only cleanup modules.

### Phase 5: shrink `internal/paper`

After the runtime and strategy layers are extracted, split `internal/paper` into:

- `internal/tui`
- `internal/sim`
- `internal/risk`
- `internal/signals`

That will make package naming match actual responsibility.

## Concrete overlap to fix first

### High priority

- Copytrade code is duplicated heavily in both binaries and should become one package with paper and real execution adapters.
- Quote freshness and display-sync helpers are duplicated with mostly naming differences.
- Config-to-TUI mapping is duplicated.
- Maker math is already partly extracted, but quote lifecycle management still lives in the binaries.

### Medium priority

- Binance-gap entry logic should become one decision package with two executors.
- Split inventory should be modeled as a separate overlay strategy with explicit enable / disable semantics.

### Low priority

- cosmetic startup / shutdown differences between paper and real
- logging message differences

## Migration rule

Do not try to rewrite `paperbot` and `realbot` in one pass.

Recommended order:

1. extract pure helpers
2. extract shared state objects
3. extract one strategy package at a time
4. make both binaries call the new package
5. delete old duplicated code only after both paths run through the same abstraction

## Suggested first implementation ticket

If you want the first safe implementation pass, start here:

1. create `internal/runtime/settings_adapter.go`
2. move config <-> `paper.TUISettings` mapping there
3. move arb-mode normalization there
4. switch both binaries to use it
5. add tests for paper and real settings mapping

That is small enough to land safely and starts pulling logic out of `cmd/*` without touching live trading behavior.
