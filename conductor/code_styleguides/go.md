# Go Style Guide

## 1. General Principles
- Use standard Go formatting (`gofmt`).
- Names should be clear and concise. Use MixedCaps or mixedCaps.
- Avoid unnecessary complexity.

## 2. Project Structure
- `cmd/`: Entry points for applications.
- `internal/`: Private library code.
- `pkg/`: Public library code (if intended for external use).

## 3. Error Handling
- Errors should be handled explicitly.
- Use `fmt.Errorf` with `%w` for wrapping errors.
- Do not use `panic` for normal error handling.

## 4. Concurrency
- Use goroutines for asynchronous tasks.
- Use channels or `sync` package for synchronization.
- Always ensure goroutines can be terminated (use `context.Context`).

## 5. Testing
- Use the standard `testing` package.
- Tables-driven tests are preferred.
- Mock external dependencies where appropriate.
