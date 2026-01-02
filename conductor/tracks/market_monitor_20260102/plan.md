# Plan: Market Monitor Loop

## Phase 1: Project Skeleton & Configuration
- [ ] Task: Initialize Poetry project and dependencies
    - [ ] Sub-task: Run `poetry init` and configure `pyproject.toml` with Python 3.11+.
    - [ ] Sub-task: Add dependencies: `py-clob-client`, `web3`, `python-dotenv`, `asyncio`.
    - [ ] Sub-task: Create `.env.example` and `.env` for API keys and config.
- [ ] Task: Create Basic Directory Structure
    - [ ] Sub-task: Create `src/`, `src/api`, `src/core`, `tests/` directories.
    - [ ] Sub-task: Create `src/main.py` entry point.
- [ ] Task: Implement Configuration Loader
    - [ ] Sub-task: Write Tests for config loading (ensure env vars are read).
    - [ ] Sub-task: Implement `src/core/config.py` using `pydantic` or `os.getenv` to load settings.
- [ ] Task: Conductor - User Manual Verification 'Project Skeleton & Configuration' (Protocol in workflow.md)

## Phase 2: API Connection & Market Discovery
- [ ] Task: Implement Polymarket Client Wrapper
    - [ ] Sub-task: Write Tests for client initialization (mocking credentials).
    - [ ] Sub-task: Create `src/api/client.py` wrapping `py-clob-client`.
- [ ] Task: Implement Market Lookup
    - [ ] Sub-task: Write Tests for fetching market details by slug (mock API response).
    - [ ] Sub-task: Implement function to get market details (tokens, IDs) from REST API.
- [ ] Task: Conductor - User Manual Verification 'API Connection & Market Discovery' (Protocol in workflow.md)

## Phase 3: WebSocket Data Ingestion
- [ ] Task: Implement WebSocket Manager
    - [ ] Sub-task: Write Tests for WebSocket connection handling (mock socket).
    - [ ] Sub-task: Create `src/api/websocket.py` to handle subscription and message loop.
- [ ] Task: Implement Order Book Parser
    - [ ] Sub-task: Write Tests for parsing raw JSON order book updates into structured objects.
    - [ ] Sub-task: Define data models (dataclasses/Pydantic) for `OrderBook`.
    - [ ] Sub-task: Implement parsing logic in `src/api/parser.py`.
- [ ] Task: Conductor - User Manual Verification 'WebSocket Data Ingestion' (Protocol in workflow.md)

## Phase 4: Core Logic & Output
- [ ] Task: Implement Discount Sum Calculator
    - [ ] Sub-task: Write Tests for math verification (e.g., 0.48 + 0.48 = 0.96).
    - [ ] Sub-task: Implement calculation logic in `src/strategy/math.py`.
- [ ] Task: Integrate & Run Market Monitor
    - [ ] Sub-task: Write Integration Test for the full loop (mocking socket data input).
    - [ ] Sub-task: Wire everything together in `src/main.py` to subscribe, calculate, and log.
- [ ] Task: Conductor - User Manual Verification 'Core Logic & Output' (Protocol in workflow.md)
