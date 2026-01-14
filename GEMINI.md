# GEMINI.md

## Project Overview

PolyArb-15m is a high-frequency volatility arbitrage bot designed for Polymarket's 15-minute crypto binary option markets. It employs the "Gabagool" strategy, acting as a market maker by placing limit orders on both sides of a market (Up/Down) to lock in profits when the combined entry price is mathematically guaranteed to be less than the $1.00 payout.

### Key Technologies
- **Backend:** Go (1.25+)
- **API Integration:** Polymarket CLOB (Central Limit Order Book) REST and WebSocket APIs.
- **Blockchain:** Polygon (for balance queries and EIP-712 signing).
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
  go run cmd/bot/main.go
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
The bot implements two complementary strategies:
1. **Panic Buy (BUY → MERGE):** When `ask_sum < $0.98`, buy both YES and NO, then merge via CTF for instant profit.
2. **Panic Sell (SPLIT → SELL):** When `bid_sum > $1.03`, sell split shares to panic buyers.

Split shares and bought shares are tracked separately in `SplitInventory` to prevent strategy overlap.

### Testing Practices
- Unit tests are co-located with source code (e.g., `parser_test.go`).
- Focuses on testing core logic like API parsing, math strategies, and config security.

### Android/Termux Support
- Includes specific optimizations for running in Termux on Android, such as a background keepalive ticker to prevent OS throttling and `stty` commands for terminal control.
