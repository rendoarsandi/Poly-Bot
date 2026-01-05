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
| **Sum-Check Engine** | Only trades when `price_up + price_down < $0.98` |
| **Inventory Skew Management** | Rebalances if one side fills disproportionately |
| **Kill Switch** | Emergency stop when exposure AND unmatched exceed limits |
| **Auto-Redemption** | Redeems winning shares on market resolution |
| **Market Rotation** | Automatically moves to next market after resolution |
| **Paper Trading** | Simulated trading with no real money |
| **WebSocket + REST Fallback** | Dual data sources for reliable price feeds |
| **Android Background Support** | Keeps running when terminal is backgrounded |
| **Memory Management** | Automatic cleanup of old orders and trade history |

## Installation

### Prerequisites
- Go 1.21+
- Internet connection (for Polymarket WebSocket)

### Setup

```bash
# Clone the repository
git clone https://github.com/yourusername/Market-bot.git
cd Market-bot

# Install dependencies
go mod tidy

# Create environment file
cp .env.example .env

# Build
go build -o market-bot ./cmd/bot
```

## Usage

### Paper Trading (Simulated - No Real Money)

Paper trading uses simulated balance to test strategies without risk:

```bash
# Run the paper trading bot
go run cmd/bot/main.go

# Or build and run
go build -o market-bot ./cmd/bot
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

1. **Get API credentials** from [Polymarket Settings](https://polymarket.com/settings/api-keys)

2. **Configure `.env` file**:
```bash
# Enable real trading mode
TRADING_MODE=real

# Your credentials
PK=your_private_key_hex_without_0x
API_KEY=your_api_key
API_SECRET=your_api_secret_base64
API_PASSPHRASE=your_passphrase

# Polygon RPC (optional - uses public RPC by default)
POLYGON_RPC_URL=https://polygon-rpc.com

# Safety settings
MAX_TRADE_SIZE=10.0       # Max $10 per trade
MAX_DAILY_LOSS=50.0       # Stop after $50 daily loss
REQUIRE_CONFIRM=true      # Require typing YES to start
DRY_RUN_FIRST=true        # Simulate orders first (recommended)
```

3. **Start with DRY_RUN_FIRST=true** to verify everything works

4. **When ready for real trades**, set `DRY_RUN_FIRST=false`

#### Real Trading Features

| Feature | Description |
|---------|-------------|
| **Balance Display** | Shows your real USDC balance from Polymarket |
| **Position Tracking** | Displays your open positions and P&L |
| **Live Order Book** | Real-time bid/ask from WebSocket |
| **Order Placement** | Places actual limit orders on CLOB |
| **Auto-Cancellation** | Cancels all orders on shutdown |
| **Market Redemption** | Positions auto-redeemed when markets resolve |
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

### Strategy Parameters (cmd/bot/main.go)

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

## How It Works

### The Gabagool Strategy

1. **Don't Predict Direction** - Predict volatility and liquidity gaps
2. **Place Limit Orders Below Fair Value** - Act as a maker, not taker
3. **Buy Both Sides** - When combined cost < $1.00
4. **Guaranteed Profit** - One side always wins and pays $1.00

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
│   ├── bot/
│   │   └── main.go           # Paper trading bot (default)
│   └── realbot/
│       └── main.go           # Real trading bot
├── internal/
│   ├── api/
│   │   ├── rest_client.go    # Polymarket REST API
│   │   ├── websocket.go      # WebSocket connection
│   │   ├── parser.go         # Order book parsing
│   │   ├── clob_client.go    # CLOB order placement (real trading)
│   │   ├── signer.go         # EIP-712 order signing
│   │   ├── secp256k1.go      # Ethereum curve implementation
│   │   └── polygon.go        # Polygon RPC (balance queries)
│   ├── core/
│   │   └── config.go         # Configuration loader
│   ├── paper/
│   │   ├── engine.go         # Paper trading engine
│   │   ├── orderbook.go      # Limit order simulation
│   │   ├── ladder.go         # Ladder quoting system
│   │   ├── risk.go           # Risk manager & kill switch
│   │   ├── market.go         # Market resolution handling
│   │   └── tui.go            # Terminal UI display
│   ├── trading/
│   │   └── trader.go         # Unified trading interface (paper/real)
│   └── strategy/
│       └── math.go           # Discount sum calculations
├── .env.example              # Example configuration
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

## Paper Trading vs Real Trading

| Aspect | Paper Trading | Real Trading |
|--------|---------------|--------------|
| Command | `go run cmd/bot/main.go` | `go run cmd/realbot/main.go` |
| Money | Simulated $1000 | Real USDC from wallet |
| Orders | Simulated fills | Actual CLOB limit orders |
| Balance | Tracked in memory | From Polymarket API |
| Positions | Simulated | Real positions on Polymarket |
| Resolution | Based on final prices | Auto-redeemed by Polymarket |
| Risk | None | Real financial risk |
| Configuration | None required | Requires .env credentials |
| Mode | Default | Set `TRADING_MODE=real` |

## Future Improvements

- [x] Real trading with Polymarket CLOB API
- [x] Wallet integration (Polygon/USDC balance)
- [x] Order signing with private key (EIP-712)
- [x] Position and balance tracking
- [ ] Actual market resolution polling
- [ ] Rate limit handling
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
| **WebSocket Auto-Reconnect** | Automatically reconnects on disconnection with exponential backoff |
| **REST Fallback** | Polls REST API every 2s when WebSocket is unhealthy |
| **Force Reconnect** | Forces WebSocket reconnection after 15s of no data |
| **Trade History Cap** | Limits stored trades to 1000 to prevent memory growth |
| **Order Cleanup** | Removes filled/cancelled orders older than 5 minutes |
| **Android Keepalive** | Background ticker prevents OS from throttling the process |
| **Graceful Shutdown** | Clean exit on Ctrl+C with position liquidation |

## License

MIT License - See LICENSE file

## Credits

Strategy inspired by high-frequency traders on Polymarket.
