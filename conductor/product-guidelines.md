# Product Guidelines: PolyArb-15m

## 1. Codebase Structure & Maintainability
-   **Modular Architecture:** The codebase will be organized into distinct modules to ensure separation of concerns and scalability.
    -   `internal/api`: Handles all interactions with the Polymarket CLOB API and Polygon blockchain.
    -   `internal/strategy`: Contains the core "Gabagool" algorithm, discount sum calculations, and ladder quoting logic.
    -   `internal/risk`: Dedicated module for risk management, exposure limits, and the "Kill Switch."
    -   `internal/core`: Shared utilities, configuration, and data models.
-   **Performance Optimization:** While modular, the critical "decision loop" (reading websocket data -> calculating spread -> placing order) will be optimized for low latency. Heavy abstractions will be avoided in the hot path.
-   **Type Safety & Documentation:** All code will use Go's strong typing and include GoDoc comments for public functions and types to facilitate maintenance and onboarding.

## 2. Logging & Monitoring
-   **Structured Logging:** Logs will be structured (JSON format preferred for production, readable text for development) to capture timestamps, log levels, and relevant context (e.g., `market_id`, `order_price`, `latency_ms`).
-   **Real-time Feedback:** Console output will provide immediate feedback on bot status, active orders, and filled trades.
-   **Error Handling:** Critical errors (API disconnects, funding issues) will trigger immediate alerts in the logs and potentially safe shutdown sequences.

## 3. Reliability & Safety
-   **Fail-Safe Mechanisms:** The bot must default to a safe state (cancel all orders) on startup, shutdown, or unrecoverable error.
-   **Rate Limit Compliance:** API wrappers will enforce rate limits to prevent IP bans from Polymarket.
-   **State Recovery:** The bot should be able to recover its state (e.g., knowing current inventory) upon restart.
