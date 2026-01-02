# Specification: Market Monitor Loop

## 1. Overview
This track focuses on establishing the fundamental data ingestion pipeline for the PolyArb-15m bot. The goal is to successfully connect to the Polymarket CLOB (Central Limit Order Book) via WebSocket, subscribe to specific market order books, calculate the "Discount Sum" (Cost of Yes + Cost of No) in real-time, and log this data to the console. This foundational step validates API connectivity and the core arbitrage mathematics without executing any real trades.

## 2. Functional Requirements

### 2.1 Connection & Configuration
-   **Config Management:** Load API credentials (though not strictly used for public data, good to set up) and target market parameters from environment variables (`.env`).
-   **WebSocket Client:** Implement a robust WebSocket client using `nhooyr.io/websocket` or `github.com/gorilla/websocket`, capable of maintaining a persistent connection.

### 2.2 Data Ingestion
-   **Market Discovery:** Capability to look up a market by its slug (e.g., "btc-price-16-00") to retrieve its `condition_id` or `token_id`s required for subscription.
-   **Real-time Subscriptions:** Subscribe to the `orderbook` channel for selected markets.
-   **Order Book Parsing:** Efficiently parse incoming JSON messages to extract the "Best Bid" and "Best Ask" for both "Yes" and "No" outcomes.

### 2.3 Core Logic (The "Listener")
-   **Spread Calculation:** continuously calculate the "Discount Sum" using the formula: `Price(Yes) + Price(No)`.
-   **Profitability Check:** Determine if `Price(Yes) + Price(No) < 1.00` (excluding fees for this monitor phase, or including estimated fees).

### 2.4 Output
-   **Console Logging:** Print structured logs to stdout showing:
    -   Timestamp
    -   Market ID/Slug
    -   Best Bid/Ask for Yes & No
    -   Calculated Discount Sum
    -   Potential Profit Margin

## 3. Non-Functional Requirements
-   **Latency:** Processing from socket message to log output should be minimized (<50ms).
-   **Resilience:** Auto-reconnect on WebSocket disconnect.
-   **Type Safety:** Strong type safety using Go structs for all data models (e.g., `OrderBook`, `PriceLevel`).

## 4. Out of Scope
-   Placing actual orders (Execution).
-   Wallet/Private Key management (beyond basic config loading).
-   Risk management logic.
