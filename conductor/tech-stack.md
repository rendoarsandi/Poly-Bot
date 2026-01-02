# Technology Stack: PolyArb-15m

## 1. Core Runtime & Language
-   **Language:** Python 3.11+
-   **Package Manager:** Poetry (for dependency management and packaging)

## 2. Key Libraries & Frameworks
-   **Polymarket Interaction:** `py-clob-client` (Official Python SDK)
-   **Blockchain Interaction:** `web3.py` (Polygon Network)
-   **Concurrency:** `asyncio` (Standard library, for high-frequency websocket data handling)
-   **Data Processing:** `pandas` (Optional, for potentially complex calculation or historical analysis if needed, otherwise native python structures for speed)
-   **Environment Management:** `python-dotenv` (for secure API key management)

## 3. Development Tools
-   **Linting & Formatting:** `ruff` (Fast Python linter and code formatter)
-   **Type Checking:** `mypy` (Static type checker)
-   **Testing:** `pytest` (Testing framework)
