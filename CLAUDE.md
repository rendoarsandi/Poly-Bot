# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

PolyArb-15m is a high-frequency paper trading bot for Polymarket's 15-minute crypto binary option markets. It implements the "Gabagool" volatility arbitrage strategy: buying both sides of a binary market when the combined cost is less than $1.00, guaranteeing profit regardless of outcome.

## Common Commands

```bash
# Build
go build -o market-bot ./cmd/bot

# Run (paper trading mode)
go run cmd/bot/main.go

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
- `cmd/bot/main.go` - Entry point; spawns concurrent `MarketTrader` goroutines for each asset (BTC, ETH, SOL, XRP)
- `internal/api/` - Polymarket API clients (REST for market discovery, WebSocket for real-time order books)
- `internal/paper/` - Paper trading simulation engine
- `internal/core/` - Configuration loading
- `internal/strategy/` - Trading math (discount sum calculations)

### Core Trading Flow
1. **Discovery**: REST API scans for active 15-minute markets
2. **Trading**: WebSocket streams order book updates; bot places trades when `ask1 + ask2 < $0.98`
3. **Resolution**: When market expires, winning side pays $1.00 per share
4. **Rotation**: Bot automatically moves to next available market

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

## Configuration

Strategy parameters are in `cmd/bot/main.go`:
- `StartingBalance` - Paper trading starting capital ($1000)
- `LadderConfig` - Order levels, shares per level, price step
- `RiskConfig` - Max exposure, unmatched ratio, skew threshold, kill switch drawdown
