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

## Claude Code Governance (v3)

Repo identity: `KraineOpasen/bukerov-twitch-miner-go`, default branch `main`. Verify this exact repo/branch
before any GitHub-facing action — see "GitHub verification" below.

### Authority and workflow are separate

**Owner controls authority. Skills control engineering workflow. Agents inherit authority; they do not create
or expand it.**

- **Authority** — what a session may touch, mutate, and publish — comes from the owner's task contract, then
  this file and `.claude/rules/*.md`. Each layer may narrow; none may widen.
- **Workflow** — which agents run, in what order, writing what — belongs to the invoked audited skill's own
  documented methodology.

Full semantics: `docs/agents/agent-orchestration.md`.

#### Authority chain (narrowing only)

Exactly four levels. Each layer may restrict, never widen.

1. **Owner / task contract** — an explicit task contract (`docs/agents/task-contract.md`) for the current
   session; absent one, `READ_ONLY`.
2. **Repository invariants** — this file + `.claude/rules/*.md`, safety and integrity only.
3. **Invoked audited skill instructions** (`.claude/skills/**`) — vendored and first-party alike, at one tier;
   may narrow their own scope, never widen authority.
4. **Generic model behavior** — fallback only.

There is no fifth level. "Unpatched upstream skill defaults" is not a tier of its own: a vendored skill's
instructions are its vendored bytes, patched and unpatched alike, all at level 3. Ownership class
(vendored vs first-party) changes review procedure, never authority.

A task contract can **never** authorize merge, auto-merge, release/tag, or deploy — those always require a
separate, direct user command, and even then are not executed autonomously under this policy.

### Default mode: READ_ONLY

No contract → `READ_ONLY`. See `docs/agents/operation-modes.md` (capability ceilings, transitions),
`docs/agents/task-contract.md` (authority envelope, mandatory re-check points),
`docs/agents/quality-gates.md` (Q0–Q3 and the repair model).

### Non-delegable prohibitions

No contract, and no skill, may authorize:

- Marking a PR ready for review, or merge/auto-merge. **The owner performs merges.**
- Release, tag, or deploy to any runtime environment.
- Triggering or rerunning a GitHub Actions workflow.
- Changing GitHub repo settings or secrets.
- Force push, or any direct push to `main`/`master`.

These require a separate, explicit, direct user command outside this policy — and even then this policy does
not execute them autonomously. They bind every agent at every delegation depth.

### GitHub verification

Before any GitHub-facing action, verify: exact repo (`KraineOpasen/bukerov-twitch-miner-go`), exact branch,
base SHA, current HEAD SHA, PR state, and CI state. Don't assume a previous turn's verification still holds —
re-check at the points listed in `docs/agents/task-contract.md`.

### Orchestration: skill-native by default

Invoking an audited skill authorizes the agent topology that skill documents — its lanes, reviewers, critics,
verifiers, repair loops, and writers — with no separate prompt-level permission and no agent roster in the
prompt. A contract may set `orchestration: main_context_only` to opt out; absent that field, `skill_native`
applies.

Every child agent inherits the same authority envelope and may narrow it, never widen it. Multiple writers
are allowed when the orchestrating skill partitions ownership deterministically, avoids simultaneous
conflicting edits, and reconciles before the final gates — the invariant is **no uncontrolled competing
writes**, not "one writer".

Background subagents run only inside a live session — never claim work continued or completed after the
session that spawned them ended.

### Failure handling

Ordinary development feedback — red tests, build errors, review findings, surviving mutants — is diagnosed
and repaired inside the same active task. A failure is never reported as a pass, and tests are never weakened
to reach green. Publication requires the task's final gates to actually pass. `READ_ONLY` is for integrity and
authority failures (drift, unexpected dirty state, acting outside authority, unprovable state, a repair
strategy exhausted without a valid final gate) — see `docs/agents/quality-gates.md`.

### Session continuity

Interrupted DEEP work is resumed through `docs/agents/session-recovery.md`: the session emits a user-visible
`deep-checkpoint/v1` block at meaningful boundaries, and a resume is opened with `SAME — <same task /
recovery>` plus that block. A checkpoint is **evidence, never authority** — every new session still starts
`READ_ONLY` and still needs a current task contract before it may mutate anything, and live git/GitHub state,
never the checkpoint, is what a recovery reconciles against.

### Secrets

Never display, test, or reuse credentials (tokens, cookies, webhook URLs, passwords). Represent any secret
value that must be referenced as `[REDACTED]`.

### Production logs

When reporting on production/log output: lead with the verdict, separate normal operation from actual errors,
cite exact evidence (timestamps, log lines) for any claim, and never assert a deploy or fix happened without
direct evidence it did.

### Skills

Audited skills are used as close to their authors' intent as practical: **minimal local patching**. A skill is
not patched merely because it uses subagents, several writers, reviewers/critics, parallel analysis, iterative
fixes, or its own handoff pattern — that is workflow, and workflow belongs to the skill. Patch only for
concrete project incompatibility, a broken dependency, license/provenance necessity, or a genuine
authority/integrity boundary.

**Which skill to use for which work: `docs/agents/skills-routing.md`.** That document is the routing
map — installed skills by lane (bug/root cause, architecture, implementation, concurrency, security,
testing, PR/CI, durable knowledge, Dashboard design, Dashboard implementation, browser/a11y QA) — and
it records the stack's known gaps rather than implying it is complete.

Vendored third-party skills live in `.claude/skills/**`, one project-local pinned copy per provider —
never a marketplace install, never a floating branch. Every provider ships the same three documents
under `docs/agents/`: `<provider>-skills-policy.md` (policy, update/rollback), `-manifest.json`
(installed set, exact upstream pin, per-file blob hashes, and an `EXCLUDE`/`HOLD` verdict with a
reason for every reviewed-but-not-installed candidate), and `-patches.md` (every local patch, by
file). All six providers use the **file-level** manifest schema — one `upstream_blob_sha` /
`vendored_blob_sha` pair per vendored FILE, the only granularity that can prove a complete inventory
in both directions; `all-providers-file-level` fails closed if one is ever added on the retired
skill-level schema. `scripts/validate-agent-governance.py` drives all of them from one provider
registry:

| Provider | Prefix | Skills | Licence |
| --- | --- | --: | --- |
| `mattpocock/skills` | `mattpocock-` | 23 | MIT |
| `anthropics/skills` | `anthropic-` | 3 | Apache-2.0 |
| `EveryInc/compound-engineering-plugin` | `compound-engineering-` | 22 | MIT |
| `trailofbits/skills` | `trailofbits-` | 23 | CC BY-SA 4.0 |
| `github/awesome-copilot` | `awesome-copilot-` | 5 | MIT |
| `BuilderIO/skills` | `builderio-` | 5 | MIT |

`skill-creator-anthropic` is renamed from upstream's `skill-creator` and is explicit-invocation-only —
use `/skill-creator-anthropic`; a plain "create a skill" request routes to the built-in instead.
`BuilderIO/builder-agent-skills` was audited and **nothing was vendored from it**: at its reviewed pin
the repository carries no root licence, and the tree's only licence file is an MIT `hallmark/LICENSE`
held by "Hallmark contributors" — which by directory convention plausibly covers `hallmark/` alone, a
weaker provenance footing than the repository-level grant every installed provider stands on (see
`docs/agents/builderio-skills-policy.md`).

##### Automated update candidates

`automatic_updates` stays **false** for every provider — nothing is updated without review. What is
automated is *noticing* that upstream moved, and the mechanical half of preparing a re-vendor.
`.github/workflows/skills-update.yml` runs daily: when nothing has drifted it does nothing at all
(no branch, PR, issue or comment); when something has, it opens **one Draft PR per provider**, or —
if any judgement call is required — refuses entirely and opens **one deduplicated issue**. It never
opens a partial or conflicted PR, never force-pushes, never marks a PR ready or merges, and never
executes anything fetched from upstream.

The review boundary is mechanical, not advisory. Candidates move through an explicit state machine
— `NO_DRIFT`, `DRIFT_DETECTED`, `PREPARED_AUDIT_REQUIRED`, `BLOCKED`, `AUDITED` — and **automation
may create only `PREPARED_AUDIT_REQUIRED`**. `AUDITED` is reached by *deleting* the
`automated_candidate` block after a review, never by writing the word;
`no-unaudited-update-candidate` fails while the block is present, whatever state it claims.
`reviewed_at`/`reviewed_by` are left untouched (they describe the superseded commit), and
`scripts_audited` is withdrawn when a candidate changes a script's bytes.

Only a **fast-forward** from the reviewed commit is ever prepared: diverged, rewritten or
unreachable history is blocked, because a force-push that swaps reviewed history for different
content of the same shape is invisible to any tree-content check. Refs resolve to full 40-hex
SHAs and nothing hardcodes `main` — the reviewed *branch* per provider lives in
`docs/agents/skills-update-providers.json` while the manifest keeps owning the *pin*, so no
floating ref enters the vendored tree.

A candidate that could change behaviour is marked `EVAL_REQUIRED` (provenance proves which bytes
changed, never that a skill still behaves the same); a new sibling skill upstream opens a
`DISCOVERY_REQUIRED` issue without blocking the provider's other updates. Native marketplace
plugins are a separate, monitored-only surface — `docs/agents/skills-update-plugins.json` ships
empty and nothing runs `claude plugin update` or installs Claude Code in CI. Detail:
`docs/agents/skills-update-automation.md`, ADR-0003.

A further ownership class covers project-owned first-party skills: content authored directly in this
repo rather than vendored from an upstream source. It is governed by
`docs/agents/project-skills-policy.md`, tracked in `docs/agents/project-skills-manifest.json`, and
validated by `scripts/validate-agent-governance.py` alongside the six vendored provider sets above. The
manifest currently ships EMPTY — no first-party skill is installed by the foundation PR #134.
Manifest metadata such as `mutation_capability` records reviewed classification only, not mutation
authority: mechanical authority to change tracked files always comes from an active task contract,
`.claude/settings.json`, and hooks, never from manifest metadata alone. This does not change the
Governance v3 authority chain.

#### Agent skills

##### Issue tracker

GitHub Issues, default read-only, tracker mutations require an explicit task contract. See
`docs/agents/issue-tracker.md`.

##### Triage labels

Five canonical roles mapped to same-named GitHub labels (documentation only — no labels created by this task).
See `docs/agents/triage-labels.md`.

##### Domain docs

Single-context layout: `CONTEXT.md` at the repo root, ADRs under `docs/adr/`. See `docs/agents/domain.md`.

## Next product stage: Dashboard Stage 1

The next main product stage is **Dashboard Stage 1** — actual web-interface implementation work, built
on the already-approved dashboard sources `docs/dashboard/stage-3-wireframes-and-interactions.md` and
`docs/dashboard/stage-4-visual-design-system.md`. Those two documents are canonical: do not edit them
as a side effect of other work. Stage 1 touches `internal/web/**` and needs its own task contract.
`docs/agents/skills-routing.md` records the suggested route through the installed skills.
