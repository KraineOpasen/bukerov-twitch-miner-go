# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project purpose

A Go rewrite of [Twitch-Channel-Points-Miner-v2](https://github.com/rdavydov/Twitch-Channel-Points-Miner-v2). It passively earns Twitch channel points by simulating viewer presence across multiple streams (no browser/video player involved), auto-claims bonuses, follows raids, places automated prediction bets, tracks and claims game drops, and contributes to community goals. It ships a web dashboard for analytics and runtime settings, and can send Discord notifications. Distributed as a single static binary (~5MB) and a scratch-based Docker image.

For the full technical spec (GraphQL operations, PubSub topics, IRC protocol, DB schema, etc.) see `SPECIFICATIONS.md` — read it before touching auth, API, pubsub, chat, drops, or bet logic.

## Build, run, test

```bash
# Build for current platform (builds Tailwind CSS first, requires network for the Tailwind CLI download)
make build

# Build without Tailwind (use when internal/web/static/css/app.css is already built)
make build-go

# Cross-compile
make build-linux / build-linux-arm64 / build-windows / build-darwin / build-darwin-arm64
make build-all

# Compress with UPX (smallest binary)
make build-compressed

# Tests (race detector on, whole module)
go test -v -race ./...
# Single package
go test -v -race ./internal/models/...
# Single test
go test -v -race -run TestName ./internal/models/...

# Lint (golangci-lint, no repo-specific config — defaults apply)
make lint

# Docker image (multi-stage build: Go + Tailwind + UPX -> scratch)
make docker

# Generate a sample config
./twitch-miner-go -generate-config
```

Note: the test suite covers nearly every package (`cmd/miner` and almost all of `internal/...`) — run it with the race detector as shown above before pushing.

Runtime flags: `-config path/to/config.json`, `-debug`, `-generate-config`. Config, cookies, logs, and the SQLite database live under `config/`, `cookies/`, `logs/`, `database/{username}/miner.db` respectively (all Docker volumes in the `Dockerfile`).

## Architecture

Entry point: `cmd/miner/main.go` — parses flags, sets up `signal.NotifyContext` for SIGINT/SIGTERM, and calls `Miner.Run(ctx)`. All lifecycle management flows through `context.Context`; when it's cancelled every goroutine (watcher, drops sync, pubsub connections, IRC connections, web server) shuts down.

`internal/miner` is the orchestrator that wires everything else together: auth, streamer manager, API client, pubsub pool, chat manager, watcher, drops tracker, notifications manager, and the web server.

Key packages (see `SPECIFICATIONS.md` § Module Structure for the full breakdown):
- `internal/auth` — Twitch OAuth device-code flow, token persistence in `cookies/`.
- `internal/api` — Twitch GraphQL client (persisted queries defined in `internal/constants/gql.go`); all Twitch reads/writes (claim bonuses, join raids, place bets, claim drops, etc.) go through here.
- `internal/pubsub` — WebSocket connection pool for Twitch PubSub (`pool.go` manages connections/topics, `websocket.go` is a single connection, `message.go`/`topic.go` handle parsing). Max 50 topics per connection.
- `internal/chat` — IRC client for Twitch chat presence and optional message logging.
- `internal/watcher` — simulates minute-watched viewing and reports it to Twitch (the mechanism that actually earns points).
- `internal/drops` — drop campaign sync (every `campaignSyncInterval` minutes) and claiming logic. This is where drops backend logic lives.
- `internal/models` — domain types; `bet.go` holds the betting strategies (SMART, MOST_VOTED, HIGH_ODDS, PERCENTAGE, SMART_MONEY, NUMBER_1..8) and filter-condition logic — this is where prediction/betting backend logic lives.
- `internal/notifications` — Discord notification backend: `manager.go` orchestrates, `discord.go` is the bot client, `repository.go` persists rules/config in SQLite, `provider.go` defines the provider interface (built for multi-provider extension beyond Discord).
- `internal/analytics` — data layer only (no HTTP): recording/querying points, annotations, chat messages via `repository.go` (SQLite).
- `internal/web` — the HTTP server and dashboard backend. `server.go` sets up routing/lifecycle and optional HTTP Basic Auth (`DASHBOARD_USERNAME`/`DASHBOARD_PASSWORD` env vars); `handlers_*.go` files implement dashboard, analytics/JSON, settings, notifications, and status endpoints; `status.go` broadcasts miner status over SSE; `viewmodels.go` builds page-specific view models.
  - Dashboard front-end lives under `internal/web/static/` (CSS built by Tailwind into `static/css/app.css` from `static/css/input.css`; vendored JS: `htmx.min.js`, `apexcharts.min.js`) and `internal/web/templates/` (Go `html/template` files: `base.html`, `dashboard.html`, `streamer.html`, `settings.html`, `notifications.html`, plus `partials/`). Templates and static assets are embedded into the binary via `//go:embed` in `server.go`. The dashboard uses HTMX for partial updates and ApexCharts for point-history charts — there is no separate JS build/bundler step beyond Tailwind.
- `internal/settings` — runtime settings management driving the Settings page (changes apply without restart).
- `internal/database` — single shared SQLite connection with a per-module migration system (`schema_versions` table tracks each module's schema version independently).
- `internal/config` — loads/saves `config.json`, applies defaults.
- `internal/constants` — Twitch client IDs/endpoints and the persisted GraphQL query definitions.

## Conventions

- Config is layered: built-in defaults -> global `streamerSettings` -> per-streamer `settings` override.
- Long-running loops (watcher, drops sync, pubsub, IRC) all take a `context.Context` and must exit cleanly on cancellation — don't add blocking work that ignores ctx.
- Rate-limit/interval settings intentionally apply random jitter (e.g. ±2.5s on websocket pings, ±20% on minute-watched cycles) to mimic human behavior; preserve jitter when touching these paths.
- The `analytics` package must stay HTTP-free — dashboard/HTTP concerns belong in `web`, not `analytics`.
- New DB schema changes should add a migration under the appropriate module in `internal/database`/`internal/analytics`/`internal/notifications` and bump that module's version in `schema_versions`, not touch other modules' versions.
- Version string is injected at build time via `-ldflags -X .../internal/version.Version=...` (see `Makefile`/`Dockerfile`) — don't hardcode versions elsewhere.
