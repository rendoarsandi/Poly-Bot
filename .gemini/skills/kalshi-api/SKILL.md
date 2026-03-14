---
name: kalshi-api
description: Kalshi API Reference. Use this skill when you need to interact with the Kalshi API, understand endpoint schemas, construct requests, or fetch real-time market data from Kalshi.
---

# Kalshi API Reference

This skill provides documentation and the OpenAPI specification for the Kalshi API.

## Usage Guidelines

- When you need to determine the correct endpoint, request parameters, or response format for the Kalshi API, use `grep_search` or `read_file` on `references/openapi.yaml`.
- The OpenAPI file contains extensive schema definitions and path documentation for the `trade-api/v2` endpoints.
- If you need to search for a specific endpoint (e.g., getting a portfolio balance, placing an order), grep the `openapi.yaml` file for keywords like `/portfolio/balance` or `operationId`.