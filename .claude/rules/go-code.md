---
paths:
  - "**/*.go"
---

# Go code conventions

- Config is layered: built-in defaults -> global `streamerSettings` -> per-streamer `settings` override.
  Preserve that precedence when touching config resolution.
- Long-running loops (watcher, drops sync, pubsub, IRC) take a `context.Context` and must exit cleanly on
  cancellation — never add blocking work that ignores `ctx`.
- Rate-limit/interval settings intentionally apply random jitter (e.g. +/-2.5s on websocket pings, +/-20% on
  minute-watched cycles) to mimic human behavior; preserve jitter when touching these paths.
- Version string is injected at build time via `-ldflags -X .../internal/version.Version=...`; never hardcode
  a version string elsewhere.
- Run `go vet ./...` and `go test -race ./...` (or the affected package) before treating a change as done —
  see `docs/agents/quality-gates.md`.
- Read `SPECIFICATIONS.md` before touching auth, API, pubsub, chat, drops, or bet logic — it is the source of
  truth for GraphQL operations, PubSub topics, IRC protocol, and DB schema.
