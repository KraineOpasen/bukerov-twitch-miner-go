---
title: Stream Check Interval Hot Apply - Plan
type: fix
date: 2026-08-22
artifact_contract: ce-unified-plan/v1
artifact_readiness: implementation-ready
product_contract_source: ce-plan-bootstrap
execution: code
---

# Stream Check Interval Hot Apply - Plan

## Goal Capsule

- **Objective:** A committed `StreamCheckInterval` change controls the current Miner generation at the next scheduler reconciliation—immediately while idle, or after any in-flight synchronous check returns—without a restart, overlapping checks, or schedule drift after a rejected apply.
- **Means:** Reconcile the latest committed interval inside the single stream-check loop, using one resettable timer and the existing coalescing wake-up channel (KTD1, KTD2).
- **Authority:** The task contract governs scope and publication. `CLAUDE.md`, `.claude/rules/**`, and `SPECIFICATIONS.md` govern repository behavior and quality gates.
- **Execution profile:** Test-first, bounded to the stream-check scheduler and its settings commit seam.
- **Stop conditions:** Stop if live `main` moves before publication, the durable commit boundary cannot be preserved, or the fix requires production changes outside the allowed paths.
- **Tail ownership:** One non-force-pushed branch, one Draft PR, and observation of only the automatically triggered CI runs.

---

## Product Contract

### Summary

The running Miner will adopt shorter and longer stream-check intervals after a successful settings commit. The same scheduler loop remains the sole owner of periodic checks and the published next-check deadline.

### Problem Frame

`settings.ApplyToConfig` accepts and clamps `StreamCheckInterval`, and each settings path persists and publishes the candidate config. The running `streamCheckLoop` captures the interval once and never resets its ticker. A pure interval-only apply also sends no scheduler wake-up, so the new value takes effect only in a later Miner generation.

### Requirements

**Commit semantics**

- R1. A successfully persisted and published interval change must wake the current stream-check scheduler after the commit point.
- R2. A persistence failure must not publish the interval, wake the scheduler, reset its deadline, or alter the runtime roster.
- R3. An apply whose validated interval equals the current value must not reset the periodic wait.

**Scheduler behavior**

- R4. Both shorter and longer intervals must replace the pending periodic deadline using the latest committed value.
- R5. The scheduler must keep one loop and one timer, with no overlapping scheduler-originated checks or busy spin.
- R6. A roster-only wake must retain the existing immediate unchecked-streamer sweep without moving an unchanged periodic deadline.
- R7. A combined roster and interval change may coalesce into one wake and must reconcile both concerns from current committed state.
- R8. `GetNextStreamCheck` must describe the active timer deadline after startup, reset, and a completed periodic check.

**Concurrency and lifecycle**

- R9. Config reads used by the scheduler must be synchronized, and no Miner lock may be held across Twitch, chat, or persistence I/O.
- R10. Cancellation must release an idle scheduler promptly; an in-flight synchronous check remains governed by the existing bounded join policy.

### Acceptance Examples

- AE1. Given an active 600-second schedule, when a 60-second interval commits, then the timer and next-check deadline reset from the wake time and already-overdue streamers receive the existing unchecked sweep.
- AE2. Given an active 60-second schedule, when a 900-second interval commits, then the old pending deadline is cancelled and no full check occurs from that obsolete deadline.
- AE3. Given an unchanged validated interval, when settings commit, then no timer reset occurs.
- AE4. Given a config path that makes `SaveConfig` fail, when the interval apply returns an error, then no scheduler wake or reset occurs.
- AE5. Given a periodic check blocked in its callback, when the interval changes, then the wake coalesces; after the callback returns the loop schedules the latest interval without starting a second check.
- AE6. Given a roster-only trigger and an unchanged interval, when the loop consumes the trigger, then it performs the unchecked sweep and preserves the existing periodic deadline.

### Scope Boundaries

#### Deferred to Follow-Up Work

- Fence a slow old-login Twitch response across an in-place streamer rename. That stale-result risk has a separate identity root and likely requires changes in `internal/twitch` and `internal/models`.
- Audit `MinuteWatchedInterval`, PubSub ping/reconnect timing, and `RequestDelay` as separate variant concerns. Their owners and runtime contracts differ from stream-check scheduling.
- Address shutdown latency from context-independent Twitch calls and IRC I/O under `ChatManager.mu` separately.

- GQL retry behavior from PR #215 is out of scope.
- Dashboard source artifacts and production `internal/web` code are out of scope.

### Sources

- `internal/miner/miner.go` — scheduler, wake-up, settings commit, and runtime fan-out.
- `internal/settings/builder.go` and `internal/config/config.go` — DTO application and validation.
- `internal/drops/drops.go` and `internal/drops/sync_test.go` — repository precedent for rereading a runtime interval with a fresh timer.
- `internal/miner/settings_persistence_test.go` and `internal/web/settings_txn_test.go` — fail-closed persistence contract.
- `SPECIFICATIONS.md` — rate-limit settings contract.

---

## Planning Contract

### Key Technical Decisions

- KTD1. **Use one resettable one-shot timer owned by the existing loop, schedule it against an absolute deadline, and fence every timer receive against current committed state and cancellation.** The same absolute `time.Time` is passed to the timer and published as `nextStreamCheck`; the production adapter converts it with `time.Until`. Before starting a full check, the timer branch rereads the synchronized interval; if it differs from the loop's armed interval, the branch skips the obsolete callback, resets from the current clock, and publishes the replacement deadline. After selecting timer or wake work, and again after a synchronized interval read that can block behind a commit, the loop rechecks `ctx` before starting external callbacks. This closes the cases where an old timer event and the commit wake are ready together or where cancellation wins while selected work waits on the config lock, and lets `nextStreamCheck` describe the active deadline after startup, reset, and synchronous check I/O. This governs R4, R5, R7, and R8.
- KTD2. **Reuse the existing buffered wake as a latest-state reconciliation signal.** The loop rereads committed interval and roster eligibility on wake, while KTD1 independently revalidates timer receives; a second channel would create two independently coalesced queues for one combined apply. This governs R1, R3, R6, and R7.
- KTD3. **Detect the interval delta at each settings commit while `m.mu` still exposes old and candidate values.** Pass the boolean into post-commit fan-out and signal only after successful persistence, publish, and roster commit. This governs R1 through R3.
- KTD4. **Inject timer creation, current time, scheduler actions, and a selected-read test barrier at the loop-runner seam.** Production supplies `time.Timer`, `time.Now`, the full-check method, and the due-only sweep method; tests supply a manually fired timer, a controlled clock, channel-controlled callbacks, and (only when needed) a hook immediately before selected work reads the synchronized interval. This makes cadence, cancellation, simultaneous-ready, and blocked-check cases deterministic without production sleeps or network I/O. This governs R4, R5, R8, and R10.
- KTD5. **Pass the synchronized interval and loop clock into the unchecked sweep.** The sweep must not reread `m.config` without `m.mu` or use a second time domain; callbacks run after releasing the lock. This governs R9.

### Sequencing

1. Establish RED coverage at the post-commit wake seam and the loop's timer behavior.
2. Replace the fixed ticker with the resettable loop runner and synchronize interval reads.
3. Extend every settings commit path with exact interval-change detection and post-commit signaling.
4. Run focused, race, mutation, full-suite, lint, build, and publication gates.

### System-Wide Impact

- Settings persistence remains the commit point; no error or cancellation boundary moves.
- The dashboard's existing next-check endpoint becomes accurate without a web-layer change.
- Roster reconciliation keeps its nonblocking, coalescing wake semantics.
- No schema, API shape, dependency, goroutine count, or external protocol changes.

### Risks & Dependencies

- A reset performed before durable publish would violate fail-closed settings semantics; interval-change signaling must remain in successful post-commit fan-out.
- A timer reset under `m.mu` could deadlock future loop changes or hold the lock across callbacks; all timer and callback work stays outside the lock.
- Coalescing can drop individual events by design; correctness therefore depends on rereading latest committed state rather than carrying delta payloads.
- Go 1.25 is the repository toolchain and supplies the timer reset semantics used by the implementation.

---

## Implementation Units

### U1. Deterministic stream-check scheduler regressions

- **Goal:** Prove the missing post-commit wake and define scheduler behavior without wall-clock sleeps.
- **Requirements:** R1 through R10; covers AE1 through AE6.
- **Dependencies:** None.
- **Files:** `internal/miner/stream_check_loop_test.go`, `internal/miner/settings_persistence_test.go` if the existing failure helper is the best fit.
- **Approach:** Add an integration regression for a pure interval-only `ApplySettings`, then a fake timer/clock harness for startup deadline publication, reset, unchanged, blocked-check, roster-only, combined, and cancellation cases.
- **Execution note:** Start from the verified failing interval-only apply test before adding production behavior.
- **Patterns to follow:** `internal/drops/sync_test.go` for deterministic runtime-interval scheduling; existing channel barriers in `internal/miner/*_test.go` for concurrency ordering.
- **Test scenarios:**
  - Covers R8 startup state. Start the loop and assert `GetNextStreamCheck` equals the initial active timer deadline before any wake or periodic check.
  - Covers all three commit paths. Apply an interval change through the no-rename, removal-only, and rename paths with a running loop; observe one post-commit reset to the absolute fake-clock deadline and the due-only sweep.
  - Covers AE1. Apply 600 to 60 with an unchanged roster; observe one post-commit wake, one reset to the absolute 60-second deadline, and the due-only sweep.
  - Covers AE2. Apply 60 to 900, then make the old timer event and commit wake ready together; regardless of which signal the loop receives first, observe a reset to 900 seconds and no full check from the obsolete deadline.
  - Covers AE3. Apply the current interval; observe no scheduler wake and no reset.
  - Covers AE4. Force `SaveConfig` to fail; observe the returned error, unchanged config, and no wake or reset.
  - Covers AE5. Block the full-check callback, commit a new interval, release the callback, and observe one active callback plus a reset to the latest interval.
  - Covers AE6. Send a roster wake at an unchanged interval; observe one unchecked sweep and no reset.
  - Cancel an idle loop; observe timer stop and loop exit without firing a check. Also select a timer event or scheduler wake, block its synchronized interval read, cancel, and prove the selected work cannot start a full or due-only check after the lock is released.
- **Verification:** Every test is channel/barrier-driven. Timeouts guard hangs only and do not determine expected behavior.

### U2. Single-loop hot-apply reconciliation

- **Goal:** Make committed interval changes govern the current generation while preserving persistence, locking, and roster-trigger semantics.
- **Requirements:** R1 through R10.
- **Dependencies:** U1.
- **Files:** `internal/miner/miner.go`, `internal/miner/stream_check_loop_test.go`.
- **Approach:** Add a synchronized interval accessor and a testable loop runner whose absolute-deadline timer, clock, full-check callback, and due-only callback are explicit dependencies. Use one resettable timer. Detect validated interval deltas inside all three settings commit paths, pass the result to `finishApply`, and send the existing nonblocking wake after commit. Reconcile interval before scheduling each wait and again before every timer callback; skip and replace an obsolete ready timer event. Pass the synchronized interval and loop-clock sample into the unchecked sweep.
- **Patterns to follow:** `internal/drops/drops.go` for fresh-timer cadence; `finishApply` for post-commit runtime fan-out; documented Miner lock order.
- **Test scenarios:** All U1 regressions pass under `-race` and repeated shuffled execution. Existing settings persistence, roster reconciliation, generation handoff, and web transaction tests remain green.
- **Verification:** The diff contains one scheduler owner, no sleep-based assertions, no forbidden-path production edit, and no lock held across external I/O.

---

## Verification Contract

| Gate | Scope | Done signal |
|---|---|---|
| RED | Untouched base and first task-branch test | The interval-only apply regression fails for the missing scheduler wake. |
| Focused behavior | Stream-check apply and loop tests | New cases pass repeatedly with shuffle and the race detector. |
| Mutation | Removed reset, cached startup interval, dropped post-commit wake, broken cancellation, and duplicate-loop mutants | Each intended mutant is killed, exact production bytes are restored, and the focused suite passes again. |
| Q0 | `go test -race -count=1 ./internal/config ./internal/settings ./internal/miner ./internal/web ./internal/app ./internal/drops` | All affected contracts pass from a clean tree except the intended diff. |
| Q1 | `go test -race -count=1 ./...` | Full repository race suite passes. |
| Q2 | `go build ./...` and `golangci-lint run` | Build and lint pass with no new dependency. |
| Q3 | Independent correctness, test, simplification, security, and differential review | No unresolved P0/P1 finding; every accepted fix is reverified. |
| Publication | Draft PR at the exact pushed commit | Only automatically triggered CI is observed; no Ready, merge, or manual rerun action occurs. |

---

## Definition of Done

- Every Product Contract requirement is enforced by deterministic tests or an existing named contract test.
- Both interval directions hot-apply in the current generation, while identical and failed applies preserve the pending wait.
- The stream-check scheduler has one loop, one timer, synchronized config reads, correct deadline publication, and no overlapping checks.
- Focused, mutation, Q0, Q1, Q2, and Q3 gates pass at the final commit.
- The working tree contains no abandoned experiment, generated residue, or forbidden-path production change.
- The final commit is non-force-pushed to one concern branch and opened as one Draft PR whose automatic CI is observed.
