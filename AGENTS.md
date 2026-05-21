# Repository Guidelines

## Project Structure & Module Organization
- `cmd/` contains runnable binaries. Main entrypoints are `cmd/realbot`, `cmd/diagnose`, `cmd/mergeredeem`, and `cmd/derivekey`.
- `internal/` holds application code:
  - `internal/api` for Polymarket/Kalshi clients, signing, REST, and WebSocket code
  - `internal/trading` for real order execution and on-chain flows
  - `internal/paper` for the TUI, paper engine, risk logic, and inventory tracking
  - `internal/core`, `internal/markets`, `internal/strategy`, `internal/setup`, `internal/marketlookup` for shared logic
- `config/` stores runtime JSON settings for the profiles.
- `scripts/` contains ad hoc Go and Node utilities. `logs/` is runtime output, not source.

## Build, Test, and Development Commands
- `go build ./cmd/realbot` builds the live trading bot.
- `go test ./...` runs the full Go test suite.
- `go test ./cmd/realbot ./internal/paper` is a good targeted check after trading or TUI changes.
- `go run cmd/realbot/main.go` starts the real bot and requires `.env` credentials.
- `node scripts/derive-api-key.js 0xYOUR_PRIVATE_KEY` is available for credential tooling. `npm test` is not implemented here.

## Coding Style & Naming Conventions
- Language is Go. Use `gofmt -w` on every edited Go file before committing.
- Keep packages inside `internal/` focused and cohesive; prefer small helpers over cross-package shortcuts.
- Follow existing naming: exported identifiers in `CamelCase`, internal helpers in `camelCase`, tests as `TestXxx`.
- Match the repo’s practical style: short functions where possible, direct logging, minimal abstraction.

## Testing Guidelines
- Use Go’s standard `testing` package.
- Keep tests next to implementation as `*_test.go`.
- Add targeted tests for sizing, cleanup, reconciliation, and exchange edge cases when changing trading logic.
- Run the narrowest relevant package tests first, then `go test ./...` for broader changes.

## Commit & Pull Request Guidelines
- Existing history favors short imperative messages, often with a scope prefix, for example: `fix(realbot): isolate taker-close mode`.
- Keep commits focused by subsystem (`realbot`, `paper`, `api`, `strategy`).
- PRs should include: what changed, why it changed, tests run, config impact, and screenshots/log excerpts for TUI changes.

## Strategy Notes
- `binance-gap` mode in `realbot` is a one-sided Binance futures lead / Polymarket lag strategy. It relies on Binance USD-M futures WebSocket `aggTrade` plus local Polymarket quote history, not Binance REST polling.
- Relevant runtime settings live in `config/*.settings.json`: `binanceQuoteAsset`, `binanceSignalThresholdPct`, `binanceSignalLookbackMs`, `binanceSignalCooldownMs`, `binanceSignalMaxAgeMs`, `binanceSignalPolyMaxMoveCents`, `binanceSignalPolyAdverseMoveCents`, and `binanceSignalSpreadMaxCents`.
- When adjusting this mode, test both signal math and live quote freshness guards. The critical failure mode is firing after Polymarket has already caught up.
- `realbot` can run with `executionBackend = paper`. In that mode, the embedded paper trader already mutates the paper engine on confirmed buys/sells. Do not mirror the same fill into `engine.BuyForMarket` / `engine.SellForMarket` a second time from strategy code, or inventory will double while order history stays single-counted.
- When forcibly hiding or resolving a live position prior to actual on-chain resolution (like in the 1h ladder close), you MUST use `engine.RecordSettledLoser(marketID, outcome, shares)` so that `walletTruth` polling does not resurrect the balance. You should also call `tui.UpdateWalletTruthResolution(marketID, true, winningOutcome)` to hide the market from the display instead of leaving it stuck in `WAITING RESOLUTION`.

## Security & Configuration Tips
- Keep secrets in `.env`, not in `config/*.json` or committed files.
- Treat `config/realbot.settings.json` as runtime behavior, not secret storage.
- Validate changes touching order sizing, cleanup, merge, or redemption with extra care; these paths can affect live funds.
ing order sizing, cleanup, merge, or redemption with extra care; these paths can affect live funds.
