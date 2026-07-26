---
paths:
  - "internal/watcher/**"
  - "internal/drops/**"
  - "internal/pubsub/**"
  - "internal/chat/**"
  - "internal/miner/**"
---

# Concurrency conventions

- All lifecycle management flows through `context.Context`; when it's cancelled every goroutine (watcher,
  drops sync, pubsub connections, IRC connections, web server) must shut down cleanly.
- `internal/pubsub` manages a WebSocket connection pool: max 50 topics per connection — don't bypass `pool.go`
  to add topics directly to a `websocket.go` connection.
- Preserve jitter on rate-limited/interval loops (websocket pings, minute-watched cycles) — it exists to mimic
  human behavior, not as incidental noise to remove.
- Run tests with `-race` for any change under these paths (`go test -v -race ./internal/<pkg>/...`); the whole
  module's test suite runs with the race detector in CI-equivalent checks.
- Avoid new blocking operations (unbuffered channel sends, mutex-held I/O) that ignore `ctx` cancellation —
  every long-running loop must select on `ctx.Done()`.
