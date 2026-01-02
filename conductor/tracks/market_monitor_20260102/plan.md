# Plan: Market Monitor Loop

## Phase 1: Project Skeleton & Configuration
- [~] Task: Initialize Go module and dependencies
    - [ ] Sub-task: Initialize go module using `go mod init github.com/username/Market-bot`.
    - [ ] Sub-task: Install initial dependencies: `go get github.com/joho/godotenv`.
    - [ ] Sub-task: Create `.env.example` and `.env` for API keys and config.
- [ ] Task: Create Basic Directory Structure
    - [ ] Sub-task: Create `cmd/bot/`, `internal/api/`, `internal/core/`, `internal/strategy/` directories.
    - [ ] Sub-task: Create `cmd/bot/main.go` entry point.
- [ ] Task: Implement Configuration Loader
    - [ ] Sub-task: Write Tests for config loading in `internal/core/config_test.go`.
    - [ ] Sub-task: Implement `internal/core/config.go` to load settings from environment variables.
- [ ] Task: Conductor - User Manual Verification 'Project Skeleton & Configuration' (Protocol in workflow.md)

## Phase 2: API Connection & Market Discovery
- [ ] Task: Implement Polymarket REST Client
    - [ ] Sub-task: Write Tests for fetching market details (mocking API response).
    - [ ] Sub-task: Create `internal/api/rest_client.go` to fetch market details (tokens, IDs) from REST API.
- [ ] Task: Conductor - User Manual Verification 'API Connection & Market Discovery' (Protocol in workflow.md)

## Phase 3: WebSocket Data Ingestion
- [ ] Task: Implement WebSocket Manager
    - [ ] Sub-task: Write Tests for WebSocket connection handling (mock socket).
    - [ ] Sub-task: Create `internal/api/websocket.go` to handle subscription and message loop using `nhooyr.io/websocket`.
- [ ] Task: Implement Order Book Parser
    - [ ] Sub-task: Write Tests for parsing raw JSON order book updates into Go structs.
    - [ ] Sub-task: Define structs for `OrderBook` and `PriceLevel`.
    - [ ] Sub-task: Implement parsing logic in `internal/api/parser.go`.
- [ ] Task: Conductor - User Manual Verification 'WebSocket Data Ingestion' (Protocol in workflow.md)

## Phase 4: Core Logic & Output
- [ ] Task: Implement Discount Sum Calculator
    - [ ] Sub-task: Write Tests for math verification (e.g., 0.48 + 0.48 = 0.96).
    - [ ] Sub-task: Implement calculation logic in `internal/strategy/math.go`.
- [ ] Task: Integrate & Run Market Monitor
    - [ ] Sub-task: Write Integration Test for the full loop.
    - [ ] Sub-task: Wire everything together in `cmd/bot/main.go` to subscribe, calculate, and log.
- [ ] Task: Conductor - User Manual Verification 'Core Logic & Output' (Protocol in workflow.md)