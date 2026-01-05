# Product Requirements Document (PRD): Polymarket "Volatility Arbitrage" Bot

## 1. Executive Summary
**Project Name:** Market-bot (Go Implementation)
**Goal:** Build a Go-based trading bot that automates volatility arbitrage on Polymarket's binary option markets.
**Core Logic:** The bot acts as a Market Maker, placing Limit Orders (Bids) on both "Yes" and "No" sides of a binary market such that the combined entry price is always mathematically guaranteed to be profitable (Total Cost < $1.00 payout).

## 2. Strategic Logic (The "Gabagool" Algorithm)
The bot does not predict price direction. It predicts volatility and liquidity gaps.
**Core Philosophy:** This is "not gambling"—it is pure mechanical trading powered by math and arbitrage. The goal is to remove emotion and exploit human-driven mispricings.

### 2.1 The "Discount Sum" Formula
The bot must ensure that for every pair of shares acquired, the cost is below the payout.
* **Target Entry:** Combined cost of roughly $0.95 - $0.98.
* **Mechanism:** The bot does not "Take" (buy at market). It "Makes" (places limit orders).
* **Example:** Bitcoin Price is volatile. The bot places a Buy Limit Order for "Yes" at 48¢ and a Buy Limit Order for "No" at 48¢.
* If both fill, total cost is 96¢. Payout is $1.00.
* **Profit:** 4¢ per share (4.1% ROI instantly).

### 2.2 Rebalancing (Delta Neutrality)
* **Risk:** One side fills (e.g., you bought "Yes" at 48¢) but price runs away, and you never get filled on "No". You are now "Long Yes" (gambling).
* **Solution:** "Inventory Skew Management."
* If Inventory_Yes > Inventory_No, the bot must aggressively bid higher for "No" to close the pair, or sell "Yes" to reduce exposure.

### 2.3 Insights from "Inside the Mind of a Polymarket Bot"
* **Asynchronous Buying:** The bot should asynchronously buy undervalued "YES" or "NO" shares to exploit temporary market mispricings.
* **Pair Cost Monitoring:** Continuously monitor average prices and simulate buys to maintain a "Pair Cost" below a safety margin (e.g., 0.99).
* **Quantity Balancing:** Actively balance quantities of YES and NO shares to maintain a strong hedge and minimize directional risk.
* **Profit Locking:** Ensure the combined average cost of the entire position (YES + NO) stays below $1.00.

## 3. User Stories & Functional Requirements

| ID | Feature | Description | Priority |
|---|---|---|---|
| F-01 | Market Scanner | Bot must auto-detect active markets (e.g., Bitcoin/ETH price targets) ending soon. | P0 |
| F-02 | Ladder Quoting | Place multiple buy orders at different price levels to capture deep wicks. | P0 |
| F-03 | Sum-Check Engine | Before placing any order, calculate Target_Bid_Yes + Target_Bid_No + Fees < 1.00. | P0 |
| F-04 | Auto-Redemption | Automatically redeem winning shares for USDC upon market resolution. | P1 |
| F-05 | Exposure Limit | Hard cap on "Unmatched Inventory" to prevent excessive directional exposure. | P1 |

## 4. Technical Architecture

### 4.1 Tech Stack
* **Language:** Go (Golang)
* **Blockchain Interaction:** Custom CLOB Client (Polygon Network)
* **Polymarket API:** Direct interaction with Polymarket CLOB via REST and WebSocket.
* **Concurrency:** Goroutines for high-frequency data handling and order management.

### 4.2 API Data Flow
* **Input (WebSocket):** Subscribe to market order books for real-time price updates.
* **Decision Engine:** Go-based strategy engine calculates spreads and manages inventory.
* **Output (REST):** Signed transactions/orders sent to Polymarket CLOB.

## 5. Implementation Roadmap

### Phase 1: Connection & Authorization
* Success fully post a test order using the Go CLOB client.
* Handle API credentials and L2 signing logic.

### Phase 2: Data Ingestion (The "Listener")
* Real-time price tracking for target markets.
* Filter and identify high-volatility opportunities.

### Phase 3: Execution Logic (The "Maker")
* Implement the "Trap" strategy with limit orders.
* Integrate inventory management and skew correction.