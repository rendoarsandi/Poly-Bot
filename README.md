# Market-bot

This is my personal Polymarket bot project.

This README is mainly a practical note for future me. It is **not** meant to read like a polished open-source landing page.

## What this repo is

A CLI/TUI bot for trading Polymarket crypto binary markets.

Main loop:
- find active markets
- watch live order books
- trade both sides when the pair is mispriced
- optionally maintain split inventory and sell into panic buyers
- clean up near expiry
- rotate into the next market

Main run modes:
- `paperbot` = simulated trading
- `realbot` = real wallet, real orders, real on-chain cleanup
- `fusionbot` = isolated Binance + Polymarket signal bot using the same TUI style

## What the bot actually does

### Panic buy / merge
If the two asks are cheap enough and margin is above the configured threshold, the bot buys both sides and merges them back into USDC.

Plain English: buy the pair under $1.00, merge it, keep the spread.

### Split inventory / panic sell
The bot can also prebuild YES+NO inventory, then sell both sides when panic buyers push the combined bid above $1.00.

Plain English: create split inventory first, dump both sides later when the market overpays.

## Current behavior

- market discovery is automatic
- default timeframe is `15m`
- WebSocket is the primary feed
- REST is used for fallback and extra depth checks
- UI is terminal-based and updates live
- settings changed in the TUI are written back to config
- the bot rotates to the next market after a round ends
- emergency cleanup runs on shutdown/panic

Important current note:
- the shared risk-manager kill switch is currently **disabled** in both `paperbot` and `realbot`
- `realbot` can still enforce `MAX_TRADE_SIZE` and `MAX_DAILY_LOSS` if those are set

## Main commands

### Paper trading
```bash
go run cmd/paperbot/main.go
```

Notes:
- starts with `$100` paper balance
- simulates fills, balance, PnL, merge, split inventory, and market rotation

### Real trading
```bash
go run cmd/realbot/main.go
```

Notes:
- requires `.env` credentials
- uses real Polymarket / Polygon state
- handles setup, approvals, order placement, cleanup, and redemption
- if `REQUIRE_CONFIRM=true`, startup asks whether to begin with split strategy `on` or `off`

### Fusion signal bot
```bash
go run cmd/fusionbot/main.go
```

Notes:
- separate from `paperbot` and `realbot`
- paper-only for now; no live order placement
- reads Binance prices plus Polymarket books and trades a lightweight cross-market signal
- reuses the same TUI style, but runtime setting changes stay in-memory for this bot only

### Other tools
```bash
go run cmd/diagnose/main.go
go run cmd/util/main.go
go run cmd/manual/main.go
go run cmd/mergeredeem/main.go
PK=0xYOUR_PRIVATE_KEY go run cmd/derivekey/main.go
```

## Quick setup

### Requirements
- Go 1.21+
- Node.js only if using the Node credential script
- Polygon RPC access for real trading

### First-time setup
```bash
cp .env.example .env
```

Minimum real-trading values:
- `TRADING_MODE=real`
- `POLY_PK`
- `POLY_API_KEY`
- `POLY_API_SECRET`
- `POLY_PASSPHRASE`
- `POLYGON_RPC_URL`

You can derive API credentials with:
```bash
PK=0xYOUR_PRIVATE_KEY go run cmd/derivekey/main.go
# or
node scripts/derive-api-key.js 0xYOUR_PRIVATE_KEY
```

## Important config values

### Market selection
- `MARKET_SLUG`
- `TIMEFRAME`
- `MAX_MARKETS`

### Buy strategy
- `MIN_MARGIN_PERCENT`
- `TRADE_SCALE_FACTOR`
- `MIN_ASK_PRICE` / `MAX_ASK_PRICE`
- `ENABLE_MARGIN_AGGRESSION`
- `MAX_AGGRESSION_MULTIPLIER`
- `BUY_EXECUTION_MARGIN_FLOOR_PERCENT`

### Split strategy
- `SPLIT_STRATEGY_ENABLED`
- `SPLIT_MIN_MARGIN_SELL`
- `SPLIT_TARGET_MARGIN_RESERVE`
- `SPLIT_REPLENISH_THRESHOLD`
- `SPLIT_MERGE_BUFFER_SECONDS`
- `SPLIT_INITIAL_CAP_PCT`
- `SPLIT_REPLENISH_CAP_PCT`

### Real trading safety
- `MAX_TRADE_SIZE` (`0` = disabled)
- `MAX_DAILY_LOSS` (`0` = disabled)
- `REQUIRE_CONFIRM`

### Paper extras
- `FEE_RATE_BPS`
- `ENABLE_CSV_LOGGER`

## Runtime flow

### Discovery
The bot searches for markets using slug, timeframe, and max-market limits.

### Live data
Per market it tracks:
- best bid / ask
- depth snapshots when available
- WebSocket updates for speed
- REST fallback when data goes stale

### Execution
It sizes trades against **actual executable liquidity**, not just the top quote. For buys it can walk asks across levels. For sells it caps size to matched bid liquidity.

### Expiry and cleanup
Near expiry it pauses new trades and starts settling inventory:
- cancel open orders
- merge balanced inventory
- sell / clean leftovers where possible
- check redemption after close

## Paperbot vs realbot

### `paperbot`
Use this to test logic safely.

Simulates:
- fills
- paper balance
- paper PnL
- merge outcomes
- split inventory behavior
- market rotation

### `realbot`
Use this when actually trading.

Adds:
- real wallet balance
- real Polymarket positions
- real order submission
- user WebSocket fill tracking
- on-chain split / merge / redeem flows
- emergency cleanup to avoid leaving junk behind

### `fusionbot`
Use this to experiment with crypto lead/lag logic safely.

It adds:
- separate command path and internal package
- Binance reference-price polling
- Polymarket orderbook polling
- lightweight fair-value / edge logic for `Up` and `Down`
- paper-only execution through the same TUI style

## TUI notes

The terminal UI is the main interface. It shows:
- active markets
- prices and depth
- latency / WS freshness
- event log
- positions / equity
- split inventory state

The settings panel can update runtime values like market filters, trade scaling, margins, split settings, and price filters.

## Repo layout

### Main entrypoints
- `cmd/paperbot`
- `cmd/realbot`
- `cmd/fusionbot`
- `cmd/diagnose`
- `cmd/util`
- `cmd/manual`
- `cmd/mergeredeem`
- `cmd/derivekey`

### Main internal packages
- `internal/api`
- `internal/core`
- `internal/fusion`
- `internal/markets`
- `internal/paper`
- `internal/trading`
- `internal/strategy`

## Reality check

This repo is optimized around my own usage, not around being a reusable public package.

That means:
- CLI-first
- TUI-first
- some behavior is intentionally hardcoded in entrypoints
- config is practical, not elegant
- docs should stay accurate instead of sounding like marketing

## Testing
```bash
go test ./...
# or the smaller scope I usually care about
go test ./internal/paper ./internal/fusion ./cmd/paperbot ./cmd/realbot ./cmd/fusionbot
```

## Personal reminder

If I change actual bot behavior, I should update this README so it stays honest instead of drifting back into fake polished documentation.

