---
artifact_contract: ce-unified-plan/v1
artifact_readiness: implementation-ready
execution: code
task_id: policy-persistence-fail-closed
base_sha: 5b331e5323e8ac3b7b289125a1ab879018137fc2
date: 2026-08-21
---

# Plan: Make runtime policy persistence fail closed and transactional

## Problem

On the live authoritative miner generation, `Miner.ApplyCampaignPolicy` and
`Miner.SetDropRule` (`internal/miner/policy.go`) mutate the live config in
place, call `persistLocked` — which only **logs** a `config.SaveConfig`
failure and returns nothing — then unconditionally call `refreshPolicy` and
return `nil`. A failed durable write is therefore acknowledged as success
(HTTP 200), the in-memory config diverges from `config.json`, dependent
policy state is re-ranked from the never-committed value, a later successful
whole-document save launders the rejected value to disk, and the PR #200
generation handoff (`App.nextGenerationConfig` → `Miner.CurrentConfig`)
carries it into the next generation.

Confirmed TRUE POSITIVE by deterministic RED reproduction on base
`5b331e5` (`internal/miner/policy_persistence_fail_closed_test.go`):
HTTP 200 + live `SMART` + snapshot `SMART` + laundering, with
`config.SaveConfig` failing via the repo's established
rename-onto-a-directory seam (`breakConfigPathForNextSave`).

## Selected transaction model — Model A (in-place mutate → SaveConfig → exact rollback under `m.mu`)

The `setAutoRedeem` precedent (`internal/miner/rewards.go:170-229`), applied
to both policy writers:

- Mutate under exclusive `m.mu`, `config.SaveConfig(m.configPath, m.config)`
  **under the same lock** (deliberate repo invariant — `cloneConfigLocked`
  [R7] note), and on failure restore the exact pre-call state **before
  unlocking**, then return a wrapped error.
- `refreshPolicy` runs only on the success path, after `Unlock` (it takes
  `m.mu.RLock` itself; calling it under the write lock would deadlock).
- `persistLocked` gains an `error` return (its only callers are these two
  writers); each caller logs with its own context and wraps the error.
- `configPath == ""`: unchanged documented library semantics — successful
  hot-apply, normal refresh.

### Rejected alternatives

- **Model B (candidate → `SaveConfig(candidate)` → publish)** — the
  `applyHealthSettings` precedent. Architecturally refuted for `SetDropRule`:
  a candidate needs a private `DropRules` copy, and publishing a fresh map
  breaks the load-bearing `cloneConfigLocked` DropRules **aliasing** ([R7]):
  an in-flight `applySettingsWithRename/Removals` candidate aliases the OLD
  live map, so its later publish would resurrect it and silently lose the
  committed drop rule — a new lost-update bug in a spot the architecture
  deliberately protects via aliasing. For `ApplyCampaignPolicy` alone Model B
  is workable but loses symmetry and widens the pointer-publish blast radius
  for no benefit.
- **Model C (generic transaction helper)** — fails the deletion test: the two
  rollback shapes differ (value field vs map entry with nil-restore); a
  closure-taking helper would be ~5 lines of ceremony (shallow module).
- `plan-arbiter` not invoked: no two genuinely viable designs remained after
  the [R7] refutation of Model B for `SetDropRule`.

## Why the rollback is correct and race-free

- **No observable rejected state:** every reader of `CampaignPolicy` /
  `DropRules` (`CurrentCampaignPolicy`, `snapshotDropRules`, `refreshPolicy`,
  `CurrentConfig`/`snapshotConfigLocked`, all `applySettings*` clones) takes
  `m.mu`; the mutate→save→rollback window holds it exclusively, so the
  rejected value can never be seen by anyone.
- **No lost update of a competing successful mutation:** `m.mu` serializes
  all config writers; rollback happens inside the same critical section that
  performed the mutation, so it can only restore state no other writer has
  touched since.
- **Fence/drain unchanged:** the `fenced` wrapper stays outermost (PR #201
  admission untouched); an admitted transaction holds `applyWG` for its whole
  body, so teardown's drain still guarantees `CurrentConfig` samples only
  completed transactions (PR #200 handoff intact).
- **Map identity stable:** `DropRules` is mutated and restored in place —
  the [R7] aliasing invariant and `TestDropRulesSnapshotRaceFreeAgainstSetDropRule`
  stay valid by construction.
- **No new lock, no new lock order, no I/O moved off `m.mu`.**
- **Nil-map exactness:** `wasNil` tracking mirrors `setAutoRedeem`
  (`rewards.go:186-213`); on failure the map is re-nilled; on success the
  existing allocate-on-demand behavior is preserved unchanged.

## Exact files

Production:
- `internal/miner/policy.go` — `persistLocked` returns `error`;
  `ApplyCampaignPolicy` + `commitDropRule` gain exact rollback + fail-closed
  return + success-only `refreshPolicy`; doc comments updated.
- `internal/miner/miner.go` — doc-comment updates only: `CurrentConfig`
  (~L2040-2055) narrows the fail-open caveat to owner-identity reconciliation.
- `internal/app/app.go` — doc-comment update only: `nextGenerationConfig`
  (~L631-638) same narrowing.
- `internal/miner/rename_reconcile.go` — comment-only: the [R5] note cited
  "policy.go via persistLocked" as a logging-under-m.mu precedent, which this
  change makes false (callers now log after Unlock); the citation list also
  carried an already-stale finishApply reference, so the note is rewritten to
  be self-justifying rather than citation-based.

Tests:
- `internal/miner/policy_persistence_fail_closed_test.go` (already written,
  RED on base): P1 HTTP policy-mode, P2 HTTP drop-rule, P3 laundering.
  Extended in TDD with: SetDropRule state matrix (nil / empty non-nil /
  add / replace / reset / reset-nonexistent / other-keys-intact /
  whitespace key), ApplyCampaignPolicy matrix (A→B success, alias input,
  failure keeps A, subsequent success works), configPath=="" library
  semantics, concurrency regression (parallel mutations with failing then
  repaired persistence; -race).
- `internal/web/stale_generation_status_test.go` (or sibling new test in
  `internal/web`) — handler classification: generic provider error → 500,
  not 503/409, no 200 re-render (uses existing `f3Policy.err` fake).
- `internal/app/generation_config_test.go` — transform
  `TestInPlaceRuntimeWriteSurvivesAFailedPersist` into the new contract
  (failed persist → error, gen1 retains A, gen2 receives A never B), loudly
  commented as the deliberate characterization update the old test invited.
  All other PR #200 tests stay untouched.

## Error contract (unchanged web layer)

- persistence failure on live generation → non-`ErrShuttingDown`,
  non-`database.ErrClosed` wrapped error → existing
  `writePolicyMutationError` generic **500**, sanitized body
  (`policyErrorMessage`), no raw path leak (wrapped error goes to server log
  only).
- `ErrShuttingDown` → existing **503**; paused/stopped lifecycle → existing
  **409**. No new status codes, no `PolicyProvider` signature change.

## Mutation-sensitivity probe (after GREEN)

Disposable manual mutation restoring the old defect in `policy.go`
(swallow the `persistLocked` error in `ApplyCampaignPolicy` — i.e. drop the
rollback+return), rerun targeted regressions → must FAIL with the original
false-success symptom; then restore byte-identically (`git checkout --`
+ `git diff` proof), rerun → PASS. Second probe for `commitDropRule`
(skip rollback). No mutation tooling installed.

## Gates

- Q0 (dev): `gofmt -l` on touched files; `git diff --check`; `go vet ./...`;
  `go build ./...`; governance validator self-tests. Final Q0 re-run on the
  committed clean head incl. full `validate-agent-governance.py`.
- Q1: `go test -race -count=1` for `./internal/miner/... ./internal/app/...
  ./internal/web/... ./internal/config/...`; new regressions repeated
  (`-count=10` or higher for the persistence-failure and handoff tests;
  concurrency regression `-count=20`).
- Q2: `go mod verify`; `go vet ./...`; `go build ./...`;
  `go test -race -count=1 ./...`; `make lint`; `git diff --check`;
  governance validator (all three invocations); proof of no
  go.mod/go.sum/schema/workflow/template/static diff.
- Q3: `code-review` (Standards + Spec) + `ce-code-review` on the final
  integrated diff; 0 unresolved BLOCKER/MAJOR.

## Publication tail

Preflight (repo/branch/SHA/PR/CI) → single commit on
`claude/fix-policy-persistence-fail-closed-rjt81e` → final Q0/Q2 on the
committed head → preflight → `git push -u origin` → preflight → ONE Draft PR
against `main` (never ready, never merge, never CI trigger) with defect /
RED evidence / design + rejected alternatives / laundering + generation
proofs / concurrency conclusion / variant findings / mutation sensitivity /
gate evidence → `ce-babysit-pr`-style passive observation of exact-head CI.

## Out of scope (audited, deferred, or forbidden)

Owner-identity reconciliation (documented startup fail-open, report-only);
ApplySettings clone-window residual; `RedeemCustomReward`; notifications
config; nil-provider HTTP behavior; Drops-page DOM feedback; all items in
the task contract's forbidden list.
