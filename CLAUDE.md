# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

PolyArb-15m is a high-frequency paper trading bot for Polymarket's 15-minute crypto binary option markets. It implements the "Gabagool" volatility arbitrage strategy: buying both sides of a binary market when the combined cost is less than $1.00, guaranteeing profit regardless of outcome.

## Common Commands

```bash
# Build
go build -o market-bot ./cmd/paperbot

# Run (paper trading mode)
go run cmd/paperbot/main.go

# Run tests
go test ./...

# Run tests with coverage
go test -coverprofile=coverage.out ./... && go tool cover -html=coverage.out

# Format code
go fmt ./...

# Pre-commit check
go fmt ./... && go test ./...
```

## Architecture

### Package Structure
- `cmd/paperbot/main.go` - Entry point; spawns concurrent `MarketTrader` goroutines for each asset (BTC, ETH, SOL, XRP)
- `internal/api/` - Polymarket API clients (REST for market discovery, WebSocket for real-time order books)
- `internal/paper/` - Paper trading simulation engine
- `internal/core/` - Configuration loading
- `internal/strategy/` - Trading math (discount sum calculations)

### Core Trading Flow
1. **Discovery**: REST API scans for active 15-minute markets
2. **Trading**: WebSocket streams order book updates; bot places trades when `ask1 + ask2 < $0.98`
3. **Instant Merge**: Immediately merge bought tokens to capture arb profit
4. **Resolution**: When market expires, any remaining positions are redeemed
5. **Rotation**: Bot automatically moves to next available market

### Dual Strategy System
The bot runs two complementary strategies that never overlap:
- **Panic Buy** (BUY → MERGE): When `ask_sum < $0.98`, buy both sides and merge for instant profit
- **Panic Sell** (SPLIT → SELL): When `bid_sum > $1.03`, sell split shares to panic buyers

Split shares and bought shares are tracked separately via `SplitInventory` to prevent overlap.

### Concurrency Model
- Main loop spawns one `MarketTrader` goroutine per asset
- Each trader manages its own WebSocket connection and trading state
- Shared `Engine` and `OrderBook` are thread-safe (mutex-protected)
- Context cancellation propagates cleanly for graceful shutdown

### Key Components in `internal/paper/`
- `engine.go` - Balance tracking, position management, PnL calculation
- `orderbook.go` - Limit order simulation with FIFO matching
- `risk.go` - Exposure limits, inventory skew, kill switch triggers
- `ladder.go` - Multi-level order placement at graduated prices
- `split_inventory.go` - CTF split share tracking (separate from bought shares)
- `tui.go` - Live terminal UI with market data and event log

## Development Workflow

This project follows TDD (Red-Green-Refactor) per `conductor/workflow.md`:
1. Write failing tests first
2. Implement minimum code to pass
3. Refactor with test safety net
4. Target >80% code coverage

### Commit Message Format
```
<type>(<scope>): <description>
```
Types: `feat`, `fix`, `docs`, `style`, `refactor`, `test`, `chore`

## Key Terminology

| Term | Definition |
|------|------------|
| Discount Sum | `price_up + price_down` - profitable when < $1.00 |
| Ladder | Multiple orders at graduated price levels |
| Skew | Imbalance between Up/Down positions |
| Kill Switch | Emergency stop on exposure + skew breach |
| CTF Split | Convert USDC → YES+NO tokens ($1 → 1 YES + 1 NO) |
| CTF Merge | Convert YES+NO tokens → USDC (1 YES + 1 NO → $1) |
| Panic Buy | Buy both sides when ask_sum < $0.98, then merge |
| Panic Sell | Sell split shares when bid_sum > $1.03 |

## Configuration

Strategy parameters are in `cmd/paperbot/main.go`:
- `StartingBalance` - Paper trading starting capital ($1000)
- `LadderConfig` - Order levels, shares per level, price step
- `RiskConfig` - Max exposure, unmatched ratio, skew threshold, kill switch drawdown

Split strategy parameters are in `.env` (for realbot):
- `SPLIT_STRATEGY_ENABLED` - Enable/disable panic sell strategy (default: false)
- `SPLIT_INITIAL_USDC` - Initial USDC to split at market start (default: $10)
- `SPLIT_MIN_MARGIN_SELL` - Minimum margin to trigger sell (default: 3%)
- `SPLIT_TARGET_MARGIN_RESERVE` - Reserve inventory for this margin (default: 6%)
- `SPLIT_REPLENISH_THRESHOLD` - Trigger replenish when shares below this (default: 50)
- `SPLIT_MERGE_BUFFER_SECONDS` - Seconds before expiry to merge unsold (default: 30)
