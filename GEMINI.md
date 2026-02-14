# GEMINI.md

## Project Overview

PolyArb-15m is a high-frequency volatility arbitrage bot designed for Polymarket's 15-minute crypto binary option markets. It employs the "Gabagool" strategy, executing simultaneous **market orders** on both sides of a market (Up/Down) to ensure atomic fills and lock in profits when the combined entry price is mathematically guaranteed to be less than the $1.00 payout. By using market orders (FOK), the bot avoids the risk of "legging into" unbalanced positions that limit orders might cause.

### Key Technologies
- **Backend:** Go (1.25+)
- **API Integration:** Polymarket CLOB (Central Limit Order Book) REST and WebSocket APIs.
- **Blockchain:** Polygon (for balance queries, EIP-712 signing, and on-chain Merge/Split).
- **Execution:** Uses Market Orders with Fill-or-Kill (FOK) to guarantee price and atomic execution.
- **Frontend/UI:** Custom Terminal UI (TUI) for real-time monitoring.
- **Utilities:** Node.js (for API key derivation from private keys).

### Architecture
- **Concurrent Traders:** The bot monitors multiple assets (BTC, ETH, SOL, XRP) simultaneously using separate goroutines.
- **Dual Data Sources:** Real-time price and liquidity tracking via WebSocket with an aggressive 15ms REST polling fallback.
- **Paper Trading Engine:** A built-in simulator for testing strategies without financial risk, including orderbook simulation and position tracking.
- **Risk Management:** Includes "Kill Switch" mechanisms based on drawdown, exposure, and inventory skew.
- **Compounding:** Automatically adjusts trade sizes based on cumulative profit from successful rounds.
- **Dual Strategy System:** Runs panic buy (BUY→MERGE) and panic sell (SPLIT→SELL) strategies with separate inventory tracking.

## Building and Running

### Prerequisites
- Go 1.25 or higher.
- Node.js 16+ (for `scripts/derive-api-key.js`).
- `.env` file configured (see `.env.example`).

### Key Commands

#### Setup
```bash
go mod tidy
npm install
cp .env.example .env
```

#### Credential Derivation
```bash
node scripts/derive-api-key.js <YOUR_PRIVATE_KEY>
```

#### Running the Bot
- **Paper Trading (Default):**
  ```bash
  go run cmd/paperbot/main.go
  ```
- **Real Trading (WARNING: Uses real USDC):**
  ```bash
  go build -o realbot ./cmd/realbot
  ./realbot
  ```

#### Testing
```bash
go test ./...
```

## Development Conventions

### Coding Style
- **Go Standards:** Follows standard Go formatting (`gofmt`).
- **Concurrency:** Extensively uses `goroutines`, `channels`, and `sync.Mutex` for thread-safe state management across concurrent traders.
- **Error Handling:** Errors are wrapped with context using `fmt.Errorf("...: %w", err)`.
- **Context:** Uses `context.Context` for cancellation and timeout management across all API calls and goroutines.

### Project Structure
- `cmd/`: Entry points for paper (`bot`) and real (`realbot`) trading.
- `internal/api/`: Polymarket API clients (REST, WS, Signer) and Polygon integration (including CTF split/merge).
- `internal/paper/`: The simulation engine, including TUI, risk management, orderbook logic, and split inventory tracking.
- `internal/trading/`: Unified interface for executing trades in both paper and real modes.
- `internal/core/`: Configuration loading and shared utilities.
- `scripts/`: Node.js utilities for credential management.

### Trading Strategies
The bot implements two complementary strategies optimized for the 15-minute window:

1. **Panic Buy (Atomic Entry):** When `ask_sum < $0.98`, the bot submits simultaneous **Market Orders** for both YES and NO. It uses a price buffer ($0.99 cap) and Fill-or-Kill (FOK) to ensure both sides fill immediately or not at all.
   - **Recovery Logic:** If an order becomes unbalanced (one side fills, one fails), the bot enters an aggressive recovery loop, retrying the failed side with GTC market orders until the position is balanced.
   - **Instant Capture:** After a successful buy, it executes an on-chain **Merge** to convert tokens back to USDC and lock in profit.

2. **Panic Sell (Inventory Strategy):** When `bid_sum > $1.03`, the bot sells pre-split shares to panic buyers.
   - **Inventory Management:** Uses **Split** operations to create YES/NO pairs when capital is idle, allowing for instant selling when premiums spike.

### Why 15-Minute Markets?
While 5-minute markets offer faster capital rotation, the bot targets 15-minute markets for several critical reasons:
- **Liquidity:** 15m markets have significantly deeper order books, allowing for larger trade sizes without slippage.
- **Volatility Capture:** The longer window provides more time for "wicks" to hit both target price levels.
- **Fee Management:** 15m markets offer a better balance for avoiding "Taker Fees" during the entry phase by allowing the bot to walk the book effectively.
- **Safety:** Reduced "legging risk" compared to the hyper-fast 5m markets where oracles and price feeds can be extremely erratic.

### Testing Practices
- Unit tests are co-located with source code (e.g., `parser_test.go`).
- Focuses on testing core logic like API parsing, math strategies, and config security.

### Android/Termux Support
- Includes specific optimizations for running in Termux on Android, such as a background keepalive ticker to prevent OS throttling and `stty` commands for terminal control.
