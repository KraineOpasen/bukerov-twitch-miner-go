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
- `internal/gql` — generic GraphQL/HTTP transport primitives (transient-status classification, persisted-query and top-level-error detection, retry/backoff timing). Twitch-agnostic; depends only on the standard library and is imported by `internal/twitch`.
- `internal/twitch` — Twitch GraphQL client (persisted queries defined in `internal/constants/gql.go`); all Twitch reads/writes (claim bonuses, join raids, place bets, claim drops, etc.) go through here. Imports `internal/gql` for the generic transport helpers.
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

## Governance

The single canonical governance authority for every agent and executor working in this repository is
[`GOVERNANCE_V3.md`](GOVERNANCE_V3.md) at the repository root. It owns the authority hierarchy, the
operation modes (default: `READ_ONLY` — no mutation without a current task contract), the stable-line
branch policy (active development base: the live current `release/X.Y` stable line named by the current
owner decision/task contract and verified live at task start; `main` and other development lines are not
a code source or development base by default), preflight/STOP and
session-recovery rules, evidence discipline, skill inventory and mandatory skill routing, orchestration
semantics, quality gates Q0–Q3, and the publication boundary. Read it before any mutating or
GitHub-facing work.

This file and the rest of the repo-native layer — `.claude/rules/*.md`, `docs/agents/**`,
`.claude/settings.json` plus the `.claude/hooks/governance-policy.py` PreToolUse hook (mechanical
enforcement), and `scripts/validate-agent-governance.py` (governance validator) — elaborate
`GOVERNANCE_V3.md` for this repository. They may narrow it; they never widen it. On any conflict the
`GOVERNANCE_V3.md` §1 hierarchy decides and the conflict is surfaced to the owner, never silently
reconciled. Mechanical elaborations: `docs/agents/operation-modes.md` (mode ceilings),
`docs/agents/task-contract.md` (contract envelope, mandatory re-check points),
`docs/agents/quality-gates.md` (repo-native gate commands). Adoption record:
`docs/adr/0002-canonical-governance-v3.md`.

Repo identity: `KraineOpasen/bukerov-twitch-miner-go`. Before any GitHub-facing action verify the exact
repo, branch, base SHA, current HEAD SHA, PR state, and CI state live (`GOVERNANCE_V3.md` §2, §5) — a
previous turn's verification does not carry forward past the re-check points.

Session continuity (`GOVERNANCE_V3.md` §5): a checkpoint is **evidence, never authority** (the
`deep-checkpoint/v1` recovery block) — it never restores a mode, and every new session starts READ_ONLY
and needs a current task contract before any mutation, whatever any checkpoint block says. The
repo-native authority chain has exactly **four levels** — owner/task contract;
`CLAUDE.md` + `.claude/rules/*.md`; invoked audited skill instructions (patched and unpatched alike);
generic model behavior — narrowing only, consumed at the positions `GOVERNANCE_V3.md` §1 assigns (§3).

Owner-gated actions — marking a PR ready for review, merge/auto-merge, tag, release, image publication,
deploy/restart or any runtime mutation, triggering or rerunning a CI workflow, and GitHub
settings/secrets changes — are forbidden without a separate, direct owner command; no task contract,
skill, or child agent can grant them, and a direct owner command authorizes exactly one specific gated
action after a fresh live preflight (`GOVERNANCE_V3.md` §4). Always forbidden regardless of any such
command: force push and any direct push to a protected branch (`main`/`master`/`release/*`) —
protected-branch changes land only through the normal task-branch/PR path. `.claude/settings.json` and
the PreToolUse hook mechanically enforce a subset of this (force push, `main`/`master` pushes, `gh`
mutations, infra restarts); direct pushes to `release/*` are forbidden at the policy level but not yet
hook-gated — extending the hook is an owner-side follow-up, since the enforcement layer is edit-denied
for agent sessions. Secrets handling and production/log reporting rules: `GOVERNANCE_V3.md` §15.

### Skills

Vendored third-party skills live in `.claude/skills/**`: the current approved baseline
(`GOVERNANCE_V3.md` §7) is **81 installed, audited, project-local vendored skills across six
providers** — `mattpocock/skills` (23, MIT), `anthropics/skills` (3, Apache-2.0),
`EveryInc/compound-engineering-plugin` (22, MIT), `trailofbits/skills` (23, CC BY-SA 4.0),
`github/awesome-copilot` (5, MIT), and `BuilderIO/skills` (5, MIT). Each provider is owned by its
reviewed policy + file-level manifest + patch-ledger triple under `docs/agents/`
(`<provider>-skills-policy.md`, `<provider>-skills-manifest.json`, `<provider>-skills-patches.md`),
registered in `docs/agents/skills-update-providers.json`, and routed in
`docs/agents/skills-routing.md`. Pins are each manifest's `upstream_commit`; `automatic_updates` is
false for every provider. The daily skill-update automation is defined on the repository's default
branch (`main`) and, under GitHub scheduled-workflow semantics, runs only there — this stable line is
not maintained by it and receives audited refreshes via the normal task-branch/PR path.
`skill-creator-anthropic` is upstream's `skill-creator` renamed (explicit-invocation-only — use
`/skill-creator-anthropic`; a plain "create a skill" request routes to the built-in instead).

A seventh ownership class covers project-owned first-party skills: content authored directly in this
repo rather than vendored from an upstream source. It is governed by
`docs/agents/project-skills-policy.md`, tracked in `docs/agents/project-skills-manifest.json`, and
validated by `scripts/validate-agent-governance.py` alongside the six vendored sets above. The
manifest currently ships EMPTY — no first-party skill is installed by the foundation PR #134.
Manifest metadata such as `mutation_capability` records reviewed classification only, not mutation
authority: mechanical authority to change tracked files always comes from an active task contract,
`.claude/settings.json`, and hooks, never from manifest metadata alone. This does not change the
`GOVERNANCE_V3.md` §1 authority hierarchy.

#### Agent skills

##### Issue tracker

GitHub Issues, default read-only, tracker mutations require an explicit task contract. See
`docs/agents/issue-tracker.md`.

##### Triage labels

Five canonical roles mapped to same-named GitHub labels (documentation only — no labels created by this task).
See `docs/agents/triage-labels.md`.

##### Domain docs

Single-context layout: `CONTEXT.md` at the repo root, ADRs under `docs/adr/`. See `docs/agents/domain.md`.
