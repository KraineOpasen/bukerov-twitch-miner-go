# Twitch project kernel (local-development edition, rules rev5)

Repository: `KraineOpasen/bukerov-twitch-miner-go`. A Go Twitch channel-points miner
with drops, predictions, a web dashboard and optional notifications.

The running miner lives ONLY on the owner's TrueNAS (Docker). The NUC clone at
`/home/claude/bukerov-twitch-miner-go` (OS user `claude`) is the development machine:
ordinary tasks never start live mining, never touch runtime, secrets or deployment.

## Canonical rules

`docs/process/LOCAL_WORK_RULES_rev5.md` (Russian, 64146 bytes, SHA-256
`4426cfbef16ecc7a20b6162cf725b9f18e1d1f1bf7651824108d32441a6f1aad`) is the binding
owner-approved process (approved 2026-10-21). This file is its English working summary
for agents. On any conflict the Russian file wins. Both change only together, by an
owner-approved new revision carried out as an R3 task; an ordinary task never edits either one.

What rev5 replaced for ordinary tasks (rules §0): the mandatory Draft PR, the CodeRabbit /
Codex / Codex Security trigger comments on GitHub, waiting for the three external lanes, and
owner «статус» messages. What it kept unchanged: owner-only decisions (merge, release, deploy,
runtime, settings, secrets, access, visibility, product, policy, privacy); the Q2 set;
independent review in a separate context; RED→GREEN for bugs; falsifiers for R3; Claude
Security on code changes; the Context7 boundary; no new plugins, hooks, MCP or trackers
without an owner decision; the guard, observer and Task Observer as installed; one writer per
clone; fenced executor output; denial = STOP; the native settings and GitHub rules acting
independently of any text; the start-control minimum.

These rules are live for ordinary tasks only after the transition PR that installed them
is merged by the owner AND both Project sources (Claude and ChatGPT) are re-aligned to
rev5 (rules §13.3). Before the first ordinary task under these rules check rules §12.2
(dated backup of the development tree to TrueNAS with a restore test comes first) and
§13.4 (the order of the first tasks). A path reference does not load the Russian file into
context: read `docs/process/LOCAL_WORK_RULES_rev5.md` with the Read tool whenever this
summary does not answer a question.

## Authority and scope

- A task is granted by the owner's five-line task card in the VS Code chat (mode A,
  owner present) or by a full English contract with activation line, STOP codes and
  RESUME line (mode B, unattended Remote Control). Without a card or contract, default
  to READ_ONLY. A checkpoint, draft or reviewer suggestion is evidence, not a grant.
  Child/subagent authority never exceeds the parent task; subagents never write to the clone.
- One writer per clone. Skills, plugins and tools assist the task; they never expand it.
- Ask the owner only for: a new product/policy/privacy/data decision; an irreversible
  action (deletion, history rewrite, deploy, runtime, settings, secrets, permissions);
  publication (release delivery below); an authority boundary; a native deny/ask control
  that cannot be satisfied honestly. Everything else is decided by the executor and
  written into the report. Mode A: clarifying questions only when the card leaves a
  product/policy/data/authority point open; a clear card gets no interview. Mode B:
  never ask mid-run; finish independent work and end with one STOP carrying the exact question.
- Current code/tests, `SPECIFICATIONS.md` and accepted ADR/design evidence define product
  behavior. Read the relevant specification before changing auth, API, PubSub, chat,
  drops or prediction logic. Private Twitch wire (GraphQL, persisted-query hashes, claim
  and prediction mechanics) comes only from repository code, tests, logs and
  DevilXD/TwitchDropsMiner cross-checks, never from Context7 or training memory. Context7
  is for public library/tool documentation only.
- Never print, reuse or invent credentials; redact tokens, cookies, passwords and
  notification credentials in every output. Tests never touch the production database
  or the miner's real settings.

## Branches, candidate, merges, delivery

- Three branches. `release/0.3`: the published stable line on GitHub, protected by a
  ruleset (PR plus `lint`, `build`, `docker` checks; no bypass). `dev/local`: the local
  integration branch on the NUC, created once from `origin/release/0.3`, never pushed
  except at release delivery. `task/<short-name>`: one task, one branch, from fresh `dev/local`.
- Commits are local, small and meaningful. Never rebase, cherry-pick, reset, rewrite
  history or force-push. `GIT_OPTIONAL_LOCKS=0` for read-only git; `git switch --create`,
  not `-c`; `git var GIT_AUTHOR_IDENT` / `GIT_COMMITTER_IDENT`, not `git config`.
- Candidate. At start record `BASE = git merge-base dev/local task/<name>` as a 40-hex
  value. The final candidate is the branch HEAD at final checks, with its tree
  (`git rev-parse HEAD^{tree}`) and `git diff --name-only BASE HEAD`. Every final check,
  review and scan covers the whole `BASE..HEAD`.
- Clean candidate. Immediately before final checks, before the independent review and
  before any handoff: working tree and index clean (`git status --porcelain
  --untracked-files=all` empty apart from the untracked paths below) and HEAD equal to
  the recorded value. Any later edit is a new commit, a new candidate and a full re-run
  of the final gates; earlier receipts do not carry over. Re-check after test runs.
- Permitted local merges (recorded, never silent, never with rebase or force):
  (a) inflow: `git merge --no-ff origin/release/0.3` into `dev/local` when the published
  line moved; (b) pull-up: `git merge --no-ff dev/local` into `task/<name>` when
  `dev/local` moved past BASE. Before either, write OLD_BASE, NEW_TARGET, TASK_HEAD
  (40 hex) and the literal command into the report; after either, record the new BASE
  and candidate and repeat all final gates (Q2, component checks, Claude Security for
  R2/R3, independent review as a chain). Resolve conflicts only inside the task's file
  boundary; a conflict outside it is STOP. A native ask on the merge command is an
  owner touchpoint; a deny is STOP, never worked around.
- Merges only on the owner's word (run by the executor, never on its own initiative): (c)
  `git merge --no-ff task/<name>` into `dev/local` only on the owner's word «принято» after
  the full report (mode A: on the spot; mode B: the last
  touchpoint, the run ends with STOP «ждёт «принято»»); check first that `dev/local`
  still equals the recorded BASE, otherwise pull-up and re-verify; (d) anything into
  `release/0.3`: only the owner's merge button on GitHub.
- Task states: LOCAL_VERIFIED (all gates of the class passed, report delivered) →
  ACCEPTED (merged into `dev/local` on the owner's word). This is the complete result of
  a task. Delivery policy P0: no task-branch push, no PR per task, nothing to GitHub.
- Release delivery, only on the owner's words «отправляй выпуск»: run inflow if needed,
  Q2 and component checks on the `dev/local` HEAD, push ONLY `dev/local` at the named
  HEAD (`git push origin dev/local`, never force), open one normal (not Draft) PR
  `dev/local` → `release/0.3` whose body lists the accepted tasks and their reports.
  No `@coderabbitai` / `@codex` trigger comments, no waiting for external lanes; natural
  CI runs by itself. `gh pr create` is a native ask (owner touchpoint). States: DELIVERED,
  CI PENDING → CI GREEN / CI FAILED → MERGED BY OWNER → RELEASED → UPDATED ON TRUENAS.
  A failed or unknown CI is never merged: repair as a task, new «принято», new push on a
  new owner word.
- Release and TrueNAS update follow Master §17 and `stable-release.yml`: an owner-created
  empty public Release (not draft, not pre-release, `make_latest=false`) plus a
  `stable-vX.Y.Z` tag whose commit is on `release/X.Y`; the workflow checks, builds and
  publishes `ghcr.io/kraineopasen/bukerov-twitch-miner-go-stable:X.Y.Z`. Version and
  line are the owner's choice. While the miner is being reworked no `stable-v*` tag is
  pushed; tags, releases, images, deploy, runtime, settings, secrets, workflow dispatch or
  rerun always need explicit owner authority.

## Start control (automatic, before the first edit)

Verify and record: OS user `claude` and the clone path; current branch; `git fetch`;
`dev/local` exists and contains `origin/release/0.3` (else inflow first); clean tree and
index; no unfinished git operation (merge/rebase/cherry-pick, lock files); no other
writer in this clone (another Claude Code or Codex session editing it; an idle Remote
Control server and a read-only reviewer are not writers); required tools present
(`go`, `golangci-lint`, `gofmt`, a C compiler and CGO for `-race`, `python3` for the
hooks); the SKILL GATE below (an unapproved plugin, hook, MCP or LSP = STOP, not "note
and continue"); effective author AND committer via `git var` equal to the task's
identity; a safe test environment. Any failed check = STOP with a description.

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

## Asset paths (note outside the invariants section)

The asset shorthand in the inherited invariants above is relative to `internal/web`:
`internal/web/static/css/app.css`, `internal/web/static/css/input.css` and
`internal/web/static/js/*.min.js`; the repository has no root-level `static/`. Resolve
those actual paths before checking or changing an asset. The invariants section itself
stays byte-identical to the base kernel.

## Risk classes and verification

- The executor classifies by the content of the change, raises when in doubt; only the
  owner lowers. R1 (low): documentation, messages, tests that do not change verified
  behavior, configs without behavioral effect. Never R1: deleting or weakening a test
  assertion, changing recovery/rollback commands, any change to agent instructions or
  permissions (`CLAUDE.md`, `CLAUDE.local.md`, `AGENTS.md`, `.claude/settings*.json`,
  hooks): those are R3. R2 (medium): production Go without concurrency, network, auth,
  DB schema or hooks. R3 (high): concurrency (goroutines, channels, mutexes), network and
  Twitch protocol, tokens and auth, SQLite schema/migrations, settings, hooks, guard,
  observer, deployment, packaging/workflows, agent instructions and permissions. A mode A
  task that turns out to be R3 stops editing and goes back to the owner for mode B.
- Q2 on the final candidate, real exit codes, BASE and HEAD substituted as 40-hex values:

```bash
git diff --check BASE HEAD
git diff --check
gofmt -l .
GOTOOLCHAIN=local go mod verify
GOTOOLCHAIN=local go vet ./...
GOTOOLCHAIN=local go build ./...
TZ=UTC GOTOOLCHAIN=local go test -race -count=1 ./...
TZ=UTC GOTOOLCHAIN=local go test -count=1 ./...
GOTOOLCHAIN=local make lint
```

  `gofmt -l .` must exit 0 AND print nothing. Both test runs are mandatory: files
  constrained with `//go:build !race` run only without `-race`. About three minutes on
  the NUC; one heavy run at a time.
- Component checks when the component is touched: Python hooks and installers
  (`python3 -I -m unittest discover -s .claude/hooks -p 'test_*.py'`, installers also on
  a copy); config/settings (parse plus effect); existing web UI (browser check per the
  Dashboard rules); SQLite migrations (copy of the database with rollback); packaging and
  workflows (no Docker on the NUC: mark "image build not verified locally"; CI `docker`
  at release delivery is the check). Any other missing required check = UNAVAILABLE/STOP,
  never PASS.
- Gates by class. R1: Q2 + component checks + one independent review + Claude Security
  when code was touched. R2: R1 + RED before GREEN for bugs + Claude Security on
  BASE..HEAD. R3: R2 + a falsifier per changed mechanism + the expensive model reads the
  diff itself + owner «принято» only after the full report. Codex Security CLI is NOT
  part of R3 acceptance until it is qualified in its own owner-approved task
  (LD-OD-15); until then the report carries the line "Codex Security: not qualified,
  not run", which is neither UNAVAILABLE nor a caveat on acceptance; this is a reduced check
  set compared with the former process, chosen by the owner.
- Claude Security "Scan changes" is required for any change to code or security-relevant
  files (Go, tests, hooks, workflows, Dockerfile, settings, web templates/assets) in every
  class; pure text changes (documentation, messages) are marked "N/A: no code" with the
  reason. The `/claude-security:claude-security` skill is user-only: prepare BASE, HEAD
  and the file list and ask the owner to start it (mode A: on the spot; mode B: a listed
  touchpoint at the end of independent work). Raw reports stay in the untracked
  `CLAUDE-SECURITY-*` directory (never committed, never pushed, not part of the candidate;
  it is not in the tracked .gitignore); only a non-secret receipt goes into the report. Not
  run = UNAVAILABLE, never PASS.
- Evidence standard. A RED test or a "killed" falsifier counts only when the check
  reached the target case and failed on the expected behavioral assertion, with the test
  name, real exit code and non-empty selected-test list recorded, and healthy baseline
  and restored runs. A compile error, syntax error, missing dependency, timeout, zero
  selected tests, skipped mandatory scenario or unrelated failure is UNAVAILABLE or a
  failed check, never RED, PASS or a kill. New behavior needs an acceptance oracle and
  tests; healthy tests are never rewritten to fail for form; checks are never weakened.

## Independent review (agent-watchdog order)

- One qualified independent review satisfies every class. The reviewer is never the
  author: Claude Code author → Codex `/review` against BASE; if Codex is unavailable, a
  fresh-context Claude Code subagent without the author's report, recorded as such.
  Reviewers are read-only and never become a second writer. Task Observer notes are not
  given to the reviewer.
- Reviewer input: the task card, BASE, HEAD, the full `BASE..HEAD` diff and repository
  access, NOT the author's report first. Order: own expectations and hypotheses from
  card and diff → then the author's report → then reconcile asked / claimed / actually
  changed / verified.
- Finding types: Gap / Bug / Weak evidence / Scope drift / No issue (naming what was
  verified correct). Disposition per finding: confirmed (yes/no, evidence) → severity
  (blocking / important / minor) → in scope (yes/no) → outcome (fixed / deferred with
  reason / owner decision / routed to another task). An unmet acceptance criterion, an
  unproven mandatory protection and any confirmed in-scope correctness or security defect
  of severity blocking or important is fixed in the same branch without asking the owner;
  only the owner may defer such a defect; relabelling blocking as important does not open
  acceptance. A test without which an acceptance criterion is unproven cannot be
  deferred. Optional improvements beyond acceptance are deferred with a reason and listed.
  Out-of-scope defects are routed (queued), not absorbed.
- Findings arrive as one ranked batch at a checkpoint, except a defect in code being
  edited right now or an answer the author is blocked on.
- Re-review after repairs is a chain: the original full review + the repair diff + the
  behavior it touches (callers, invariants) + a final receipt bound to the new HEAD.
  Reviewing changed lines only is not final. If the original review or the chain is
  unavailable, review fully again. Stop polishing unchanged code; waive no gate.
- STOP, not "accepted with a caveat": an unresolved blocking defect, an unavailable
  mandatory tool, exhausted quota or no progress.

## Models

Author in Claude Code: Opus (high); Fable only by owner decision. Codex: `gpt-6-astra`,
effort xhigh. A mid-task model change is an owner decision; the actual model and effort
go into the report. Bounded heavy work (repository scans, documentation extraction, test
runs, log reduction, R1/R2 review passes) goes to Sonnet subagents with a self-contained
brief (path, goal, bounds, output format, check commands, stop conditions); their output
is evidence, not a verdict. For R3 the expensive model reads evidence and diff itself.

## Report and checkpoint

`.scratch/<task>/REPORT.md` (untracked) plus a short Russian summary in chat. Contents:
card and assumptions; start control; BASE, HEAD, tree, file list; Q2 and component
commands with exit codes and timing; RED→GREEN; falsifiers (R3); the review finding
table and chain; Claude Security receipt; "Codex Security: not qualified, not run";
task state and any merges performed (OLD_BASE/NEW_TARGET/TASK_HEAD); actual models;
deferred items; what was not checked; skills applied and what each contributed; for every
control-bearing route (ce-work, mattpocock code-review, Codex /review, Codex Security CLI,
Claude Security): who ran it and where, provider, installed version, invocation route,
preconditions, side effects and mode (rules §14.2).
Untracked paths that are neither "dirty tree" nor part of the candidate: `.scratch/`
(git-ignored), `CLAUDE-SECURITY-*` (untracked, tolerated), build/test caches; they are never
committed or pushed. A resumed
task keeps earlier evidence and writes into a new subdirectory. Checkpoint at STOP or
RESULT: DONE / DELIVERED / OPEN / NEXT; BASE, HEAD, tree, drift; tree/index
cleanliness; changed files; Q0–Q3; task state; not verified; effect on the TrueNAS miner
(NONE before a release). The executor's ENTIRE chat output (reports, diffs, logs,
commit metadata, code, Russian notes) goes into fenced code blocks.

## Skills and plugins (SKILL GATE)

- Mandatory base on the NUC: `cc-skills-golang`, `agent-skills`, `mattpocock-skills`,
  `compound-engineering`, `pr-review-toolkit`, `systems-programming`,
  `backend-api-security`, `claude-security`, `claude-code-setup` (all `@synced`) and
  `gopls-lsp@claude-plugins-official`. Separately approved: the Task Observer (local
  skill and hooks, OBS-D3: method friction only, never verdicts; not exported into
  evidence; absent in Q3 subagents; `TASK-OBSERVER: OFF` in a separate Q3 session; a
  denied Observer write = skip, name it, continue), the claude-security hooks, the
  permission guard and prompt observer (G1), MCP (Claude Docs, Context7,
  claude-code-remote), the gopls LSP, the `anthropic-skills` namespace. Any other
  component = STOP at start control; never removed, enabled or used silently. Four states
  are distinct: APPROVED → PRESENT → INVOCABLE → APPLIED. Examples inside skills never
  authorize new dependencies or tools.
- Phase minima: before RED, `mattpocock-skills:tdd` or
  `agent-skills:test-driven-development` (one) PLUS `cc-skills-golang:golang-testing`;
  while fixing, the Go skills of the touched topic (`golang-error-handling`,
  `golang-context`, `golang-concurrency` plus `systems-programming:go-concurrency-patterns`
  for any goroutine/channel/mutex, `golang-safety`, `golang-database`, `golang-security`,
  …) and `agent-skills:security-and-hardening` for input/network/auth; before
  acceptance, `pr-review-toolkit:pr-test-analyzer` OR the testing lens of
  `compound-engineering:ce-code-review` (one), `pr-review-toolkit:silent-failure-hunter`,
  Claude Security, one independent review. Library skills (`golang-samber-*`,
  `golang-uber-*`, `golang-google-wire`, `golang-spf13-*`, `golang-swagger`,
  `golang-grpc`, `golang-graphql`) only when the change uses that matching library directly
  and within scope. `golang-graphql` describes gqlgen servers; this miner is a Twitch
  client, so ordinary Twitch GraphQL client work does not make it applicable (rules §14.3).
- Nested workflows: `compound-engineering:ce-work` only in a verified Return-to-Caller
  mode without the publication tail; otherwise unavailable. A permitted parent never
  calls a forbidden child; a nested engine never changes the model, adds a writer or
  takes over reviews and gates. `mattpocock-skills:code-review` reviews on the Standards
  and Spec axes with the task card as the spec; if its installed body cannot run
  without a tracker, do not install one, do not call `setup-matt-pocock-skills`, use
  another permitted reviewer. `mattpocock-skills:pr` is a PR-body template only.
- Forbidden for the executor (also through nested calls): `compound-engineering:
  ce-commit-push-pr`, `lfg`, `ce-babysit-pr`, `ce-resolve-pr-feedback`;
  `mattpocock-skills:git-guardrails-claude-code`, `setup-pre-commit`, `implement`,
  `implement-spec`, `to-spec`, `to-tickets`, `triage`, `wayfinder`,
  `setup-matt-pocock-skills`; `claude-code-setup:claude-automation-recommender` (repeat
  only by owner decision). Deferred until Dashboard V1: browser-testing, frontend and
  design skills, Playwright MCP; an existing web-UI check per the Dashboard rules is not
  waived by that deferral.

## Native controls

`.claude/settings.json` uses native permissions, disables bypass mode and registers no
hook. The owner-run installers registered the `PermissionRequest` guard (2026-10-15) and
the prompt observer (G1, PR #430) in the git-ignored `.claude/settings.local.json`; the
guard answers only exact routine `gh`, `git` and read forms, and nothing unless the active
contract's task record allows it; see `.claude/hooks/README-permission-guard.md` and
`.claude/hooks/README-prompt-observer.md`. Checking out the repository installs neither.
Tracked `enabledPlugins` entries stay `false`; NUC enablement lives in
`.claude/settings.local.json`. Native matching is not a sandbox: deny and ask rules apply
before and regardless of any hook. A denied main-task action or a safety-classifier
refusal means STOP. Do not retry it by splitting, respelling, wrapping, changing tools or
otherwise altering the route. Before submitting an action, correct a predicted syntax or
command-shape problem within the task's authority; this pre-execution correction never
authorizes retrying a denied action. The sole continue exception is the denied Task
Observer write defined in rules §14.0.

Codex reads `AGENTS.md`; Claude settings and hooks do not bind Codex. Until the owner
qualifies Codex permissions in its own task, Codex is reviewer-only in this repository.
