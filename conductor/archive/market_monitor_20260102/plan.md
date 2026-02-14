# Plan: Market Monitor Loop

## Phase 1: Project Skeleton & Configuration [checkpoint: f589b2f]
- [x] Task: Initialize Go module and dependencies ff47b83
    - [x] Sub-task: Initialize go module using `go mod init github.com/username/Market-bot`.
    - [x] Sub-task: Install initial dependencies: `go get github.com/joho/godotenv`.
    - [x] Sub-task: Create `.env.example` and `.env` for API keys and config.
- [x] Task: Create Basic Directory Structure aa5c6e4
    - [x] Sub-task: Create `cmd/paperbot/`, `internal/api/`, `internal/core/`, `internal/strategy/` directories.
    - [x] Sub-task: Create `cmd/paperbot/main.go` entry point.
- [x] Task: Implement Configuration Loader 59190a8
    - [x] Sub-task: Write Tests for config loading in `internal/core/config_test.go`.
    - [x] Sub-task: Implement `internal/core/config.go` to load settings from environment variables.
- [x] Task: Conductor - User Manual Verification 'Project Skeleton & Configuration' (Protocol in workflow.md) f589b2f

## Phase 2: API Connection & Market Discovery [checkpoint: f386745]
- [x] Task: Implement Polymarket REST Client b84c30e
    - [x] Sub-task: Write Tests for fetching market details (mocking API response).
    - [x] Sub-task: Create `internal/api/rest_client.go` to fetch market details (tokens, IDs) from REST API.
- [x] Task: Conductor - User Manual Verification 'API Connection & Market Discovery' (Protocol in workflow.md) f386745

## Phase 3: WebSocket Data Ingestion [checkpoint: 0f221f0]
- [x] Task: Implement WebSocket Manager 3deb24d
    - [x] Sub-task: Write Tests for WebSocket connection handling (mock socket).
    - [x] Sub-task: Create `internal/api/websocket.go` to handle subscription and message loop using `nhooyr.io/websocket`.
- [x] Task: Implement Order Book Parser addb745
    - [x] Sub-task: Write Tests for parsing raw JSON order book updates into Go structs.
    - [x] Sub-task: Define structs for `OrderBook` and `PriceLevel`.
    - [x] Sub-task: Implement parsing logic in `internal/api/parser.go`.
- [x] Task: Conductor - User Manual Verification 'WebSocket Data Ingestion' (Protocol in workflow.md) 0f221f0

## Phase 4: Core Logic & Output [checkpoint: 878a66d]

- [x] Task: Implement Discount Sum Calculator 4274031

    - [x] Sub-task: Write Tests for math verification (e.g., 0.48 + 0.48 = 0.96).

    - [x] Sub-task: Implement calculation logic in `internal/strategy/math.go`.

- [x] Task: Integrate & Run Market Monitor cc92a79

    - [x] Sub-task: Write Integration Test for the full loop.

    - [x] Sub-task: Wire everything together in `cmd/paperbot/main.go` to subscribe, calculate, and log.

- [x] Task: Conductor - User Manual Verification 'Core Logic & Output' (Protocol in workflow.md) 878a66d
