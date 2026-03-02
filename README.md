# PolyArb-15m: Polymarket Volatility Arbitrage Bot

A high-frequency trading bot that automates volatility arbitrage on Polymarket's 15-minute crypto binary option markets using the "Gabagool" strategy.

## Overview

The bot acts as a **Market Maker**, placing limit orders on both sides of a binary market (Up/Down or Yes/No) such that the combined entry price is always mathematically guaranteed to be profitable (Total Cost < $1.00 payout).

```
┌─────────────────────────────────────────────────┐
│  Buy Up:   100 shares × $0.48 = $48             │
│  Buy Down: 100 shares × $0.48 = $48             │
│  ─────────────────────────────────────────────  │
│  Total Cost: $96                                │
│  Guaranteed Payout: $100 (one side wins)        │
│  Profit: $4 (4.1% ROI)                          │
└─────────────────────────────────────────────────┘
```

## Features

| Feature | Description |
|---------|-------------|
| **Market Scanner** | Auto-detects active BTC/ETH/SOL/XRP 15-minute markets |
| **Ladder Quoting** | Places orders at multiple price levels to capture volatility |
| **Zero-Excuse Balance Guard** | Real-time USDC check with auto-scaling to prevent unbalanced fills |
| **Autonomous Redemption** | Automatic on-chain redemption of winning tokens (Claim profit) |
| **Sum-Check Engine** | Only trades when `price_up + price_down < $0.98` |
| **Inventory Skew Management** | Rebalances if one side fills disproportionately |
| **Unbalanced Fill Recovery** | 3 retries with progressive pricing if one side fails |
| **Kill Switch** | Emergency pause at 25% drawdown (holds positions until expiry) |
| **Market Rotation** | Automatically moves to next market after resolution |
| **Paper Trading** | Simulated trading with no real money |
| **London VPS Optimized** | Ultra-low latency execution via AWS eu-west-2 proximity |
| **WebSocket + REST Hybrid** | WS for price updates, REST for liquidity (20ms polling, 148 RPS) |
| **Android Background Support** | Keeps running when terminal is backgrounded |
| **Memory Management** | Automatic cleanup of old orders and trade history |
| **CTF Split Strategy** | Profit from panic buyers by selling split shares when bid_sum > $1.03 |
| **Instant Merge** | Immediately merge bought shares to capture arb profit without waiting |

## Installation

### Prerequisites
- Go 1.21+
- Node.js 16+ (for API key derivation script)
- Internet connection (for Polymarket WebSocket)

### Setup

```bash
# Clone the repository
git clone https://github.com/yourusername/Market-bot.git
cd Market-bot

# Install Go dependencies
go mod tidy

# Install Node.js dependencies (for API key derivation)
npm install

# Create environment file
cp .env.example .env

# Build
go build -o market-bot ./cmd/paperbot
```

### Deriving Polymarket API Credentials

Polymarket API credentials are **derived from your private key** - they are not created in a dashboard. You only need to run this once:

```bash
# Option 1: Use Go tool (Recommended)
go run cmd/derivekey/main.go

# Option 2: Use Node.js script (Pass private key as argument)
node scripts/derive-api-key.js 0xYOUR_PRIVATE_KEY_HERE

# Option 3: Use Node.js script (Use environment variable)
POLY_PK=0xYOUR_PRIVATE_KEY node scripts/derive-api-key.js
```

This will output your credentials:
```
============================================================
SUCCESS! Add these to your .env file:
============================================================

POLY_API_KEY=xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
POLY_API_SECRET=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
POLY_PASSPHRASE=xxxxxxxxxxxxxxxx
```

Copy these values to your `.env` file along with your private key.

## Usage

### Paper Trading (Simulated - No Real Money)

Paper trading uses simulated balance to test strategies without risk:

```bash
# Run the paper trading bot
go run cmd/paperbot/main.go

# Or build and run
go build -o market-bot ./cmd/paperbot
./market-bot
```

### Real Trading (Uses Real Money!)

Real trading connects to your Polymarket account and places actual orders:

```bash
# 1. Configure your credentials in .env
cp .env.example .env
# Edit .env with your credentials

# 2. Build the real trading bot
go build -o realbot ./cmd/realbot

# 3. Run the real trading bot
./realbot
# Or: go run cmd/realbot/main.go
```

#### Real Trading Setup

1. **Derive API credentials** using the script (see [Deriving Polymarket API Credentials](#deriving-polymarket-api-credentials))

2. **Configure `.env` file**:
```bash
# Enable real trading mode
TRADING_MODE=real

# Your credentials (from derive-api-key.js script)
POLY_PK=0xyour_private_key_hex
POLY_API_KEY=your_api_key
POLY_API_SECRET=your_api_secret_base64
POLY_PASSPHRASE=your_passphrase

# Polygon RPC (optional - uses public RPC by default)
POLYGON_RPC_URL=https://polygon-rpc.com

# Safety settings
MAX_TRADE_SIZE=50.0       # Max $50 per trade
MAX_DAILY_LOSS=50.0       # Stop after $50 daily loss
REQUIRE_CONFIRM=true      # Require typing YES to start
DRY_RUN_FIRST=true        # Simulate orders first (recommended)
```

3. **Start with DRY_RUN_FIRST=true** to verify everything works

4. **When ready for real trades**, set `DRY_RUN_FIRST=false`

#### Real Trading Features

| Feature | Description |
|---------|-------------|
| **Balance Display** | Shows your real USDC balance from Polygon blockchain |
| **Position Tracking** | Displays your open positions and P&L |
| **Live Order Book** | Real-time bid/ask from WebSocket |
| **Order Placement** | Parallel MARKET+GTC orders for guaranteed fills |
| **Auto-Cancellation** | Cancels all orders on shutdown |
| **On-Chain Redemption** | Auto-claims winnings via CTF contract (requires ~0.01 MATIC gas) |
| **Balance Guard** | Fresh balance check (5s cache) before every trade |
| **Safety Limits** | Max trade size and daily loss limits |
| **Dry-Run Mode** | Test order flow without placing real orders |

#### Real Trading Commands

```bash
# View markets and prices without trading
./realbot
> view

# Start trading (requires confirmation)
./realbot
> YES
```

### Diagnostic Tool

Test your network latency to Polymarket, verify wallet configuration, and check API connectivity without spending money:

```bash
# Run the diagnostic tool
go run cmd/diagnose/main.go
```

This measures:
- REST `/book` endpoint latency (P50, P95, P99)
- WebSocket connection and message intervals
- EIP-712 signing speed (local CPU)
- L2 auth header signing
- Order submission round-trip (dry-run)
- Maximum throughput (RPS)

### Utility Bot (Interactive Panic Buy / Sell)

An interactive command-line tool to execute real trades with on-chain merge, live order book, and liquidity-capped execution. Useful for capturing sudden volatility manually:

```bash
# Run the utility bot
go run cmd/util/main.go
```

### Manual Management Tools

Manage your positions and merge/redeem tokens manually using standalone tools.

**Manual Position Manager:**
List on-chain positions and evaluate condition IDs and outcomes.
```bash
go run cmd/manual/main.go
```

**Merge/Redeem Tool:**
Manually trigger CTF merge or redeem on polymarket outcomes.
```bash
go run cmd/mergeredeem/main.go
```

### Example Output

```
══════════════════════════════════════════════════
  🎰 POLYARB-15M PAPER TRADING BOT
──────────────────────────────────────────────────
  💵 Starting Balance: $1000.00
  📝 Mode: PAPER TRADING (no real money)
══════════════════════════════════════════════════

🔎 Scanning for active 15m markets...
✅ Found: btc-updown-15m-1767361500

📊 Trading Market: btc-updown-15m-1767361500
⏰ Market ends at: 13:45:00 (12m30s remaining)

┌─────────────────────────────────────────────────┐
│           GABAGOOL STRATEGY CONFIG              │
├─────────────────────────────────────────────────┤
│ Ladder: 3 levels × 100 shares @ $0.01 step      │
│ Max Exposure: $500 | Kill DD: 10%               │
└─────────────────────────────────────────────────┘

📈 Placing ladders @ $0.480
🔥 [13:32:45] Sum: 0.9600 (4.00%) | Up: 0.480, Down: 0.480 | ⏱️ 12m15s
✅ Filled: buy Up 100 @ $0.460
✅ Filled: buy Down 100 @ $0.460

══════════════════════════════════════════════════
  📊 PAPER TRADING STATS [13:33:00]
──────────────────────────────────────────────────
  💰 Balance:     $908.00 (started: $1000.00)
  📈 Total PnL:    $8.00
     ├─ Realized:   $0.00
     └─ Unrealized: $8.00
──────────────────────────────────────────────────
  📈 Total Trades: 2
     ├─ Wins:  0
     └─ Losses: 0
  🎯 Win Rate:     0.0%
──────────────────────────────────────────────────
  ⚠️  Max Drawdown: 0.00%
  📊 Exposure:      $92.00
══════════════════════════════════════════════════
```

## Configuration

### Environment Variables (.env)

```bash
# Market to trade (optional - will auto-discover if not set)
MARKET_SLUG=btc-updown-15m

# API credentials (for future real trading)
POLYMARKET_API_KEY=
POLYMARKET_SECRET=
POLYGON_PRIVATE_KEY=
```

### Strategy Parameters (cmd/paperbot/main.go)

```go
// Starting balance for paper trading
const StartingBalance = 1000.0

// Ladder configuration
ladderConfig := paper.LadderConfig{
    Levels:         3,      // Number of price levels
    SharesPerLevel: 100,    // Shares at each level
    PriceStep:      0.01,   // Price decrement per level ($0.01)
    BasePrice:      0.48,   // Starting bid price
}

// Risk configuration
riskConfig := paper.RiskConfig{
    MaxExposure:        500.0,  // Maximum $ in positions
    MaxUnmatchedRatio:  0.20,   // 20% max unmatched inventory
    MaxUnmatchedShares: 300.0,  // 300 shares max on one side
    SkewThreshold:      0.15,   // 15% skew triggers rebalance
    KillSwitchDrawdown: 0.10,   // 10% drawdown triggers kill
}
```

## Hardcoded Execution Parameters & Logic

To prevent configuration drift and ensure ultra-low latency execution, several critical trading mechanics are intentionally **hardcoded** into the bot's execution engine (`cmd/realbot/main.go`). These are distinct from the TUI settings (which only control *when* to trigger a trade).

| Parameter / Logic | Value | Location | Reason / Description |
|------------------|-------|----------|----------------------|
| **Buy Limit Price** | `$0.99` | `cmd/realbot/main.go` | When a buy arbitrage triggers, the bot sends an aggressive market order capped at $0.99. This guarantees it can "walk the book" through multiple price levels to grab all available liquidity without failing due to 1-2 cents of slippage. |
| **Sell Limit Price** | `$0.01` | `cmd/realbot/main.go` | When panic-selling split inventory, it acts as a market dump capped at $0.01 to ensure immediate liquidation against whatever bids exist. |
| **Time In Force (TIF)**| `IOC` | `cmd/realbot/main.go` | All orders use `TIFImmediateOrCancel` instead of `FOK` (Fill-Or-Kill). This prevents Polymarket from rejecting the entire order if it's missing just a few shares. It takes exactly what is available and cancels the rest. |
| **Minimum Order Size**| `$1.00` | `cmd/realbot/main.go` | Polymarket rejects any order (buy or sell) under $1.00 in value. The bot automatically floors buy orders to ensure `shares * price >= $1.00`. For cleanups (sells), if the remaining dust is under $1.00, it gracefully skips selling and holds the dust to avoid crash loops. |
| **On-Chain Sync Delay**| `Query` | `internal/trading/trader.go` | After buying, the bot does not trust the CLOB API. It pings the Polygon blockchain every 1s (up to 20s) to count exactly how many CTF tokens arrived in the wallet before executing a merge. This entirely prevents "fake fill" errors. |

## How It Works

### The Gabagool Strategy

1. **Don't Predict Direction** - Predict volatility and liquidity gaps
2. **Place Limit Orders Below Fair Value** - Act as a maker, not taker
3. **Buy Both Sides** - When combined cost < $1.00
4. **Guaranteed Profit** - One side always wins and pays $1.00

### Dual Strategy System

The bot runs **two complementary strategies** that never overlap:

| Strategy | Direction | Trigger | Profit Source |
|----------|-----------|---------|---------------|
| **Panic Buy** | BUY → MERGE | `ask_sum < $0.98` | Market underpriced |
| **Panic Sell** | SPLIT → SELL | `bid_sum > $1.03` | Market overpriced |

#### Panic Buy Strategy (Default)
- Buys both YES and NO when combined asks < $0.98
- Immediately merges tokens back to USDC via CTF contract
- Profit = $1.00 - (ask_YES + ask_NO)

#### Panic Sell Strategy (CTF Split)
- Splits USDC into YES+NO tokens upfront ($1 → 1 YES + 1 NO)
- Sells to panic buyers when combined bids > $1.03
- Auto-replenishes inventory when shares deplete
- Merges remaining shares before market expiry
- Profit = (bid_YES + bid_NO) - $1.00

**Important**: Split shares are tracked separately from bought shares to prevent strategy overlap.

### Ladder Quoting

```
ORDER BOOK
──────────────────────
ASKS (sellers)
  $0.52 - 500 shares
  $0.51 - 300 shares
  $0.50 - 200 shares  ← Market
──────────────────────
BIDS (our orders)
  $0.48 - 100 shares  ← Level 0
  $0.47 - 100 shares  ← Level 1
  $0.46 - 100 shares  ← Level 2
```

### Risk Management

| Condition | Action |
|-----------|--------|
| Skew > 15% | Increase bids for light side |
| Exposure > $500 | Reduce ladder sizes |
| Exposure > $500 AND Unmatched > 20% | **KILL SWITCH** |
| Drawdown > 10% | **KILL SWITCH** |

### Market Lifecycle

```
┌─────────────────────────────────────────────────┐
│  1. DISCOVERY                                   │
│     Find active 15m market                      │
├─────────────────────────────────────────────────┤
│  2. TRADING                                     │
│     Place ladders, fill orders, rebalance       │
├─────────────────────────────────────────────────┤
│  3. ENDING                                      │
│     Cancel orders, wait for resolution          │
├─────────────────────────────────────────────────┤
│  4. RESOLUTION                                  │
│     Redeem winning shares, realize PnL          │
├─────────────────────────────────────────────────┤
│  5. ROTATION                                    │
│     Find next market, repeat                    │
└─────────────────────────────────────────────────┘
```

## Project Structure

```
Market-bot/
├── cmd/
│   ├── paperbot/         # Paper trading bot (default)
│   ├── realbot/          # Real trading bot (production)
│   ├── diagnose/         # Network connectivity and API diagnostic tool
│   ├── util/             # Utility bot for interactive panic buy/sell
│   ├── derivekey/        # Go tool to derive Polymarket API key
│   ├── manual/           # Manual on-chain position management
│   └── mergeredeem/      # Tool to manually merge and redeem tokens
├── internal/
│   ├── api/              # Polymarket REST, WS, and Signer clients
│   ├── core/             # Configuration loader and security
│   ├── markets/          # Market discovery and state management
│   ├── paper/            # Paper trading engine and order book
│   ├── strategy/         # Math and discount sum calculations
│   └── trading/          # Unified trading interface (paper/real)
├── scripts/              # Various JS/Go utility scripts
├── .env.example          # Example configuration
├── go.mod
└── README.md
```

## Terminology

| Term | Definition |
|------|------------|
| **Discount Sum** | `Price_Up + Price_Down` - Target < $1.00 |
| **Maker** | Places limit orders (adds liquidity) |
| **Taker** | Places market orders (removes liquidity) |
| **Ladder** | Multiple orders at graduated price levels |
| **Skew** | Imbalance between Up and Down positions |
| **Kill Switch** | Emergency stop mechanism |
| **Realized PnL** | Profit/loss from closed trades |
| **Unrealized PnL** | Profit/loss from open positions |
| **CTF Split** | Convert USDC → YES+NO tokens via CTF contract |
| **CTF Merge** | Convert YES+NO tokens → USDC via CTF contract |
| **Panic Buy** | Buy both sides when market underpriced (ask_sum < $0.98) |
| **Panic Sell** | Sell split shares when market overpriced (bid_sum > $1.03) |

## Paper Trading vs Real Trading

| Aspect | Paper Trading | Real Trading |
|--------|---------------|--------------|
| Command | `go run cmd/paperbot/main.go` | `go run cmd/realbot/main.go` |
| Money | Simulated $1000 | Real USDC from wallet |
| Orders | Simulated fills | Actual CLOB limit orders |
| Balance | Tracked in memory | From Polymarket API |
| Positions | Simulated | Real positions on Polymarket |
| Resolution | Based on final prices | On-chain redemption via CTF |
| Risk | None | Real financial risk |
| Configuration | None required | Requires .env credentials |
| Mode | Default | Set `TRADING_MODE=real` |

## Testing

The project includes comprehensive test coverage for core trading logic:

```bash
# Run all tests
go test ./...

# Run tests with coverage
go test -coverprofile=coverage.out ./... && go tool cover -html=coverage.out

# Run benchmarks
go test -bench=. ./...
```

### Test Files

| Package | Test File | Coverage |
|---------|-----------|----------|
| `internal/paper` | `engine_test.go` | Engine balance, positions, PnL |
| `internal/paper` | `orderbook_test.go` | Order matching, fills, cancellation |
| `internal/paper` | `risk_test.go` | Kill switch, exposure limits, skew detection |
| `internal/paper` | `liquidity_test.go` | Aggregated liquidity, safety margin (80%) |
| `internal/paper` | `depth_test.go` | Multi-level depth aggregation |
| `internal/paper` | `split_inventory_test.go` | CTF split/sell/merge inventory tracking |
| `internal/trading` | `trader_test.go` | Trader interface, paper/real mode detection |
| `internal/api` | `signer_test.go` | EIP-712 signing, API authentication |

## Production Readiness

The realbot is **production-ready** with the following robust features:

| Category | Feature | Status |
|----------|---------|--------|
| **Trading** | Real CLOB order placement | ✅ Ready |
| **Trading** | MARKET+GTC orders for guaranteed fills | ✅ Ready |
| **Trading** | Parallel order execution (both sides) | ✅ Ready |
| **Trading** | Unbalanced fill recovery (3 retries) | ✅ Ready |
| **Trading** | CTF Split strategy (panic sell) | ✅ Ready |
| **Trading** | Instant merge for immediate profit capture | ✅ Ready |
| **Liquidity** | Aggregated multi-level depth tracking | ✅ Ready |
| **Liquidity** | 100% matched liquidity utilization | ✅ Ready |
| **Liquidity** | Real-time REST polling (20ms / 50 RPS per market) | ✅ Ready |
| **Safety** | Pre-trade balance verification | ✅ Ready |
| **Safety** | Kill switch (25% drawdown) | ✅ Ready |
| **Safety** | Exposure + unmatched ratio limits | ✅ Ready |
| **Safety** | Daily loss limits | ✅ Ready |
| **Recovery** | Auto on-chain redemption via CTF | ✅ Ready |
| **Recovery** | Graceful shutdown with order cancellation | ✅ Ready |
| **Monitoring** | TUI with live P&L, positions, latency | ✅ Ready |
| **Monitoring** | WS staleness detection + REST fallback | ✅ Ready |

## Future Improvements

- [x] Real trading with Polymarket CLOB API
- [x] Wallet integration (Polygon/USDC balance)
- [x] Order signing with private key (EIP-712)
- [x] Position and balance tracking
- [x] On-chain CTF redemption
- [x] Rate limit handling (148 RPS)
- [x] Unbalanced fill recovery
- [x] Network health monitoring (REST latency + WS staleness)
- [x] Comprehensive test suite
- [x] Aggregated liquidity calculation
- [ ] Persistent state/database
- [ ] Web dashboard
- [ ] Telegram/Discord alerts

## Risks & Warnings

⚠️ **This is experimental software for educational purposes.**

- **API Rate Limits**: Polymarket may ban IPs for excessive requests
- **Resolution Delay**: Funds locked until market resolves (5-30 min)
- **Outcome Risk**: Rare "ambiguous" resolutions can cause losses
- **Liquidity Risk**: Low liquidity markets may not fill orders
- **Slippage**: Fast markets may fill at worse prices

## Stability & Reliability

The bot includes several stability features for long-running sessions:

| Feature | Description |
|---------|-------------|
| **WebSocket Auto-Reconnect** | 10s heartbeat, auto-reconnects on disconnection |
| **REST Primary Polling** | Polls REST API every 20ms (148 RPS) for liquidity data |
| **Force Reconnect** | Forces WebSocket reconnection after 10s of stale data |
| **Trade History Cap** | Limits stored trades to 1000 to prevent memory growth |
| **Order Cleanup** | Removes filled/cancelled orders older than 5 minutes |
| **Android Keepalive** | Background ticker prevents OS from throttling the process |
| **Graceful Shutdown** | Clean exit on Ctrl+C with position liquidation |

## License

MIT License - See LICENSE file

## Credits

Strategy inspired by high-frequency traders on Polymarket.
