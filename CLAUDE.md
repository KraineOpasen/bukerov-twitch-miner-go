# Twitch project kernel

Repository: `KraineOpasen/bukerov-twitch-miner-go`. A Go Twitch channel-points miner
with drops, predictions, a web dashboard and optional notifications.

## Authority and scope

- The current owner/task prompt grants the concern, mutation scope and publication
  boundary. Without mutation authority, default to READ_ONLY. A checkpoint is evidence,
  not a grant. Child/subagent authority never exceeds the parent task.
- Use one canonical writer per concern unless the owner explicitly partitions writers.
  Skills, plugins and tools assist the task; they never expand its authority.
- Current code/tests, `SPECIFICATIONS.md` and accepted product ADR/design evidence define
  product behavior. Surface conflicts. Read the relevant specification before changing
  auth, API, PubSub, chat, drops or prediction logic. Historical/superseded ADRs are history.
- Verify mutable repository, PR and CI state live when it matters. Before meaningful
  mutation or publication, verify repository/remotes, the owner-named live stable base
  and tree, branch/HEAD, worktree/index, unfinished Git operations and competing writers/PRs.
- No silent merge, rebase, cherry-pick, reset or force push. Protected branches change
  through PRs. One concern means one Draft PR unless the owner says otherwise.
- Ready, merge/auto-merge, release/tag, image publication, deploy/restart/runtime mutation,
  repository settings/secrets and workflow trigger/rerun require explicit owner authority.
  Ordinary engineering tasks do not start live Twitch mining or change production/runtime.
- Never print, reuse or invent credentials. Use configured authentication only within
  task authority; redact tokens, cookies, passwords and notification credentials in output.
- GitHub Issues is the tracker; tracker mutations need task authority. Existing triage
  vocabulary is `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`.

## Durable engineering invariants

- Lifecycle work uses `context.Context` and exits on cancellation. Avoid unbuffered sends
  or mutex-held I/O that prevent cancellation; preserve existing lock/admission ordering.
- The PubSub pool owns topic placement (maximum 50 topics per connection). Do not bypass
  it to attach topics directly. Preserve jitter in rate-limited and interval loops.
- `MinuteWatcher` alone owns the maximum two watch slots and drives `MinuteSender`.
  Configured/discovered/drop/streak sources propose candidates; they do not create another
  scheduler, extra watch slot or independent minute reporting.
- UNKNOWN remains distinct from false, zero and success, including explicit-null drops
  listings and streak timeouts. Diagnostic observations do not grant business/scheduler
  authority. Preserve side-effect-specific replay and ambiguous-outcome guarantees; do not
  extend generic read retries to Twitch writes without checking their existing contracts.
- Config precedence is built-in defaults → global `streamerSettings` → per-streamer
  settings override. Runtime-applied settings and persisted config must stay reconciled.
  Streamer mutation/removal uses `internal/streamerlifecycle` admission/reconciliation;
  do not shortcut it with direct DB deletion or in-memory-only removal. HTTP settings
  handlers call `internal/settings` interfaces instead of mutating state directly.
- SQLite uses one shared connection (`SetMaxOpenConns(1)`). Migrations and `schema_versions`
  are per-module: change only the owning module's version. Prefer explicit transactions
  for multi-statement writes; keep cancellation and durable-deletion ordering guarantees.
- `internal/analytics` stays HTTP-free; dashboard/HTTP work belongs in `internal/web`.
  Notifications stay behind the provider interface, protect credentials and fail best-effort
  rather than crashing the miner when a provider is unavailable.
- Templates/static assets are embedded under `internal/web/templates` and `static`.
  Tailwind generates `static/css/app.css` from `input.css`; do not hand-edit the generated
  CSS. JS is vendored; there is no separate JS bundler. Project settings deny edits of
  `static/css/app.css` and `static/js/*.min.js` by Claude's file tools, by Bash file
  commands Claude Code recognizes and by redirections; programs that write files
  themselves, such as the Tailwind CLI run by `make tailwind`, are not blocked. Version is
  injected through build ldflags (`internal/version.Version`), not hardcoded elsewhere.
- Owner deployment choices: the trusted home-LAN dashboard intentionally uses
  `DASHBOARD_INSECURE_NO_AUTH=true`; lifecycle mutations are authorized through
  `DASHBOARD_TRUSTED_LAN_CIDRS` using only the connection's `RemoteAddr`, never forwarded
  headers. The owner's update interval and current product default are 2h. Do not recommend
  Basic Auth or 4h unless the owner changes the network model or requests an interval change.

## Verification

Prove changed behavior with focused regression evidence against a requirement/spec or
independent oracle. Tests should assert public behavior at approved seams. Equivalent
mutants do not demonstrate weak tests; mutation/security/browser checks are risk-specific,
not routine requirements for documentation changes. Never weaken tests or CI to get green.

On the final integrated candidate run all seven commands and record their real exit codes:

```bash
git diff --check
GOTOOLCHAIN=local go mod verify
GOTOOLCHAIN=local go vet ./...
GOTOOLCHAIN=local go build ./...
TZ=UTC GOTOOLCHAIN=local go test -race -count=1 ./...
TZ=UTC GOTOOLCHAIN=local go test -count=1 ./...
GOTOOLCHAIN=local make lint
```

Both test runs are mandatory: test files constrained with `//go:build !race` (for example
in `internal/analytics` and `internal/pubsub`) run only without `-race`.

Validate changed data/config formats, changed paths and references. Run existing Docker,
Compose and generated-file checks when relevant. Missing execution/tool evidence is
UNKNOWN/UNVERIFIED, never PASS. Resolve actionable findings before publication.

## PR review lanes

1. Wait for natural CI on the exact head. Never rerun or dispatch workflows without owner
   authority.
2. Run exactly three FULL review-only lanes on the entire PR at the exact base and head:
   CodeRabbit (`@coderabbitai full review`), Codex code review (`@codex review` with
   full-scope, review-only wording) and Codex Security (`@codex security review`). Their
   order is not an acceptance gate. Prefer requesting Codex Security after the findings of
   the other two lanes on that head are dispositioned; a qualifying exact-head Security task
   that `@codex review` already started counts and is never duplicated.
3. Inspect existing exact-head tasks before every trigger; send at most one qualifying
   request per service and head. A request, reaction or comment is not a verdict. Missing,
   stale, incremental-only, partial or quota-limited results are PENDING or UNAVAILABLE,
   never PASS.
4. Classify every finding as TRUE, FALSE, STALE or OUT_OF_SCOPE with evidence. Repair TRUE
   in-scope findings in the same PR within the active contract's severity classes and
   repair limits; findings the contract defers go to the PR body. Each repair push renews
   local checks, natural CI and all three lanes on the new head.
5. Copilot is optional automatic information only: never request it, wait for it or block
   on it; triage its findings if they appear. Post only the trigger texts above, never bot
   autofix, fix-ci, approve or resolve commands.

Keep the PR Draft. Ready, merge and release are owner decisions.

## Optional plugins and native controls

Optional skills/plugins remain account-owned. Project native settings disable the audited
synced plugins by default; use a disabled optional plugin only after explicit owner/task
selection. Do not silently edit settings to enable one for an unrelated task. Plugin
enablement on the owner's NUC lives in the git-ignored `.claude/settings.local.json`; the
tracked `enabledPlugins` entries stay `false`.

`.claude/settings.json` uses native permissions, disables bypass mode and registers no hook.
On the owner's NUC the owner-run installer registered a `PermissionRequest` guard in the
git-ignored `.claude/settings.local.json` (installed 2026-10-15). It answers only exact
routine `gh`, `git` and read forms, and nothing unless the active contract's task record
allows it; see `.claude/hooks/README-permission-guard.md`. Checking out the repository
does not install the guard; check any other environment on its own.
Native tool/command string matching is not a complete sandbox. Actual permission matching,
project plugin loading and managed-policy precedence require the separately authorized
runtime cutover/pilot; static settings validation does not establish runtime enforcement.
