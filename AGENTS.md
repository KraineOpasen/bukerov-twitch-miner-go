# AGENTS.md — Codex in this repository

Repository: `KraineOpasen/bukerov-twitch-miner-go` (Go Twitch channel-points miner).
Development happens on the owner's NUC (OS user `claude`, clone
`/home/claude/bukerov-twitch-miner-go`); the running miner lives only on TrueNAS.

The binding process is `docs/process/LOCAL_WORK_RULES_rev5.md` (Russian, SHA-256
`4426cfbef16ecc7a20b6162cf725b9f18e1d1f1bf7651824108d32441a6f1aad`), summarised for
agents in `CLAUDE.md`. This file states the part that applies to Codex. It is not an
exhaustive description of Codex's capabilities; what Codex can actually run is
qualified on its own surface by the owner.

Applicability boundary. For the transition PR itself (branch `chore/local-rules-rev5`,
based on the pinned stable commit), follow its explicitly activated old-process review
scope and the exact supplied base and head; it is not a `task/*` ordinary local task. For
subsequent ordinary tasks, rev5 becomes live only after the owner merges the transition
PR and both Project source sets are verified as aligned (rules §13.3). Until then do not
infer that the cutover or the first-task backup has completed. Codex remains reviewer-only
throughout.

## Role: independent reviewer only

- Codex is the independent reviewer for work written by Claude Code. Codex does not
  edit files, create commits or branches, push, open pull requests, install or update
  anything, change settings, or run the miner. Until the owner qualifies Codex
  permissions in a separate task, Codex is never the author of a change.
- The review is `/review` against the task's BASE commit (40 hex), covering the whole
  `BASE..HEAD` diff of the task branch (`task/<name>`, based on the local `dev/local`).
- Codex receives: the owner's task card (goal, boundary, acceptance, risk class), BASE,
  HEAD, the full diff and read access to the repository. Form your own expectations from
  the card and the diff first; read the author's report only afterwards; then reconcile
  what was asked, what is claimed, what actually changed and what was verified. Task
  Observer notes are not review input.
- Report each finding as Gap / Bug / Weak evidence / Scope drift / No issue (say what
  was checked and found correct). Give exact location, evidence and a failure scenario.
  You may describe a fix in the report; never apply one in the tree. The author repairs
  and the chain re-review (original review + repair diff + affected behavior + receipt on
  the new HEAD) follows.
- Severity words: blocking / important / minor. A confirmed in-scope correctness or
  security defect that is blocking or important must be fixed before acceptance; only
  the owner may defer it.

## Facts about this codebase that reviews must respect

- `MinuteWatcher` alone owns the maximum two watch slots; sources only propose candidates.
- UNKNOWN stays distinct from false, zero and success; no blind retry after an uncertain
  Twitch write; a local nil return is not Twitch acceptance.
- SQLite: one shared connection, per-module migrations, explicit transactions for
  multi-statement writes.
- `internal/analytics` is HTTP-free; HTTP belongs to `internal/web`; generated
  `internal/web/static/css/app.css` and vendored `internal/web/static/js/*.min.js` are
  never hand-edited.
- Private Twitch wire behavior (GraphQL, persisted-query hashes, claim/prediction
  mechanics) is judged only against repository code, tests, logs and
  DevilXD/TwitchDropsMiner cross-checks, never against training memory.

## Verification the review may run (read-only)

```bash
git diff --check BASE HEAD
gofmt -l .
GOTOOLCHAIN=local go vet ./...
GOTOOLCHAIN=local go build ./...
TZ=UTC GOTOOLCHAIN=local go test -race -count=1 ./...
TZ=UTC GOTOOLCHAIN=local go test -count=1 ./...
GOTOOLCHAIN=local make lint
```

Run heavy test suites one at a time (about three minutes for both runs on the NUC).
Test output is evidence for the review, not a verdict; the author's own Q2 run remains the
acceptance run. Tests never touch the production database or the miner's real settings.
If a review tool is missing, say so in the report; never install anything.

## Model

`gpt-6-astra`, reasoning effort xhigh (owner decision 2026-10-20). Codex Security CLI is
not part of task acceptance; after the owner's isolation qualification task it joins
acceptance only through a new owner-approved revision of the rules (rules §5.3).
