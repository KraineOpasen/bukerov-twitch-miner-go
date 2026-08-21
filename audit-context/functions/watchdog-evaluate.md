# `ProgressWatchdog.evaluate` — internal/health/progress.go (base 6b1d80e)

Part of the `stall-confirmation-seam` audit context. Companion records:
`watchdog-observe-progress.md`, `watchdog-gates-hold.md`, `watchdog-reset-evidence.md`,
`watcher-report-stats.md`. Index: `audit-context/DOSSIER-stall-confirmation-seam.md`.

---

## `evaluate` in internal/health/progress.go (L516-L625)

**Purpose:** One watchdog pass over every tracked campaign's current drop. It is the only
place the three stall thresholds are combined into the `stalled` decision (L572-L574) and the
only place `NoProgressObs` is incremented (L569). Without it nothing ever confirms a stall,
nothing advances the recovery pipeline, and `ProgressSnapshot` never updates.

**Inputs & Assumptions:**
- `now time.Time` — trusted. Sole production caller is `loop` (L386: `w.evaluate(w.now())`),
  which fires on a ±20%-jittered `watchdogEvalCadence = time.Minute` timer (L78, L379-L380).
  `w.now` is `time.Now` unless a test replaces it (L326).
- Implicit: runs on the watchdog goroutine started by `Start` (L338). `w.states` and `w.reqSeq`
  are documented "loop-owned" (L307-L308) — `w.mu` guards them only against
  `UpdateSettings`-driven resets and the `Snapshot`/debug readers.
- Implicit: `w.drops` and `w.watch` are non-nil. `NewProgressWatchdog` (L317) takes them as
  plain parameters and its doc comment names only `notifier`, `avoid`, `resolver` as
  nil-tolerant (L315-L316). `w.drops.SyncStatus()` at L526 and `w.watch.BrokerSnapshot()`
  (via `farmingChannel`, L498) are unguarded dereferences. Established by the single
  production wiring in `internal/miner/miner.go:1058-1067`, which always passes
  `m.dropsTracker` and `m.watcher`. No nil check and no test pins it — **nothing found**
  beyond that construction order.
- Precondition assumed by L572-L574: `st.ReportsSinceProgress` and `st.evidenceSince` describe
  the SAME farming episode as `st.NoProgressObs`. Established only by the ordering inside this
  function (observe → gate → window → threshold) plus `resetEvidence` (L175-L180). See the
  cross-cutting question 3 in the dossier for the case where it does not hold.

**Outputs & Effects:**
- Mutates the loop-owned `*dropState` values in `w.states` in place (L547-L551, L555-L570,
  L576-L581), and deletes stale keys under `w.mu` (L604-L617).
- Calls `w.notifier.NotifyDropRecovered` for every dropped-out state that had notified
  (L619-L622), deliberately AFTER `w.mu` is released (rationale L596-L600).
- Clears avoid-list entries for departing episodes (L609-L611).
- Publishes a fresh `ProgressSnapshot` (L624 → `publishFromStates` L974 → `publish` L1003,
  `w.snap.Store`).
- Indirectly performs Twitch I/O and blocking work through `advanceRecovery` (L585) — see
  Cross-Function Dependencies.

**Block-by-Block:**

```go
// L517-L524
cfg := w.snapshotCfg()
if !cfg.Enabled {
    w.mu.Lock()
    w.states = make(map[string]*dropState)
    w.mu.Unlock()
    w.publish(ProgressSnapshot{Enabled: false, EvaluatedAt: now})
    return
}
```
- **What:** Reads the config once per pass and, when disabled, discards ALL per-drop state.
- **Why here:** One `cfg` read per pass means every threshold in this pass uses one consistent
  configuration even if `UpdateSettings` (L350) lands mid-pass.
- **Assumes:** Wiping `w.states` is harmless. It is not entirely: `notifiedStalled`,
  `avoidedChannel`, and `exhaustedAt` are destroyed with it. A disable→enable cycle therefore
  re-arms the critical notification for a still-stalled drop and abandons any avoid entry that
  the L609-L611 cleanup would otherwise have cleared. **Nothing found** that compensates.
- **Establishes:** `cfg` is pass-stable.
- **Depended on by:** L540 (`gatesHold(..., cfg, ...)`), L572-L574, L585.

```go
// L526-L527
sync := w.drops.SyncStatus()
outage, outageSignal := w.twitchOutage()
```
- **What:** One inventory-observability snapshot and one outage verdict per pass.
- **Why here:** `SyncStatus` is a value copy taken under the tracker's `RLock`
  (drops.go:584-606), so every drop in this pass is judged against the SAME
  `ProgressLastSyncAt` / `ProgressLastError`. This is the deliberate contrast with
  `ReportStats`, which is re-read per drop (see `watchdog-observe-progress.md`).
- **Assumes:** the `watch_transport`/`oauth`/`gql_api`/`pubsub` signals `twitchOutage` reads
  (L478) are FRESH. `Center` has no TTL and no staleness check — `Record` (center.go:113-118)
  simply replaces the named signal and it persists indefinitely. A `watch_transport` failure
  recorded once and never re-probed (canary disabled at runtime, or the canary channel taken
  offline) keeps `outage == true` forever and permanently blocks stall confirmation.
  **Nothing found** enforcing freshness.
- **Establishes:** pass-stable `sync`, `outage`, `outageSignal`.
- **Depended on by:** L540, L555-L570, L562/L568.

```go
// L532-L538
for _, campaign := range w.drops.Campaigns() {
    st, key, drop := w.trackDrop(campaign, sync, now)
    if st == nil { continue }
    seen[key] = true
    w.observeProgress(st, campaign, drop, sync, now)
```
- **What:** Iterates the published campaign pool and folds each tracked drop's evidence.
- **Why here:** `observeProgress` must run BEFORE the gates so `st.Channel` is current when
  `gatesHold` names the failing gate (L769-L774).
- **Assumes:** the `*models.Campaign` pointers are immutable once published. Established:
  `Campaigns()` copies the slice under `RLock` (drops.go:400-407) and `syncProgress` builds a
  fresh slice of clones rather than mutating in place (drops.go:927-949, comment at
  drops.go:927-930: "published campaigns stay immutable after they're swapped in").
- **Assumes (unenforced):** `sync` describes the campaign pool this loop reads. It is read at
  L526 under one `RLock` and the pool at L532 under a different one. A light sync that
  publishes in between makes the pool NEWER than `sync.ProgressLastSyncAt`. Effect: the drop
  advance is observed while the stamp is old, `observeProgress` seeds
  `lastObservedSyncAt` to the OLD stamp (L735), and the next pass counts the very observation
  that showed progress as a no-progress observation at L564-L569 — `NoProgressObs` inflated by
  one against a default `StallConfirmations` of 3 (config.go:570). `publishProgressObservation`
  (drops.go:691-707) makes campaigns+stamp atomic on the WRITER side; the reader does not use
  `Revision` to prove it read one consistent pair. **Nothing found** on the reader side.
- **Establishes:** `seen[key]`, used by the L604-L617 cleanup.

```go
// L540-L553
if hold, why := w.gatesHold(...); !hold {
    st.resetEvidence()
    if st.Status != ProgressStalled { st.Status = ProgressHealthy }
    st.Detail = why
    continue
}
```
- **What:** A failing gate discards the evidence window and skips the rest of the pass for
  this drop.
- **Why here:** Before the window is extended (L555) and before the thresholds (L572), so a
  gate failure can never contribute a delay tick, an observation, or a report.
- **Assumes:** `continue` here is safe for a drop with `st.pending != nil`. It is not
  examined: `advanceRecovery`/`resolvePending` (L585, L937-L939) are unreachable on this path,
  so a parked stage's `deadline` (L843, `now.Add(recoveryOutcomeDeadline)` = 5 min, L94) keeps
  running in wall-clock while the gate is down. A gate outage longer than 5 minutes guarantees
  that the first pass after recovery resolves the pending stage as a timeout (L862-L868)
  regardless of whether the broker actually executed it. `resetEvidence` does not clear
  `st.pending` (L175-L180). **Nothing found** that reconciles the two clocks.
- **Establishes:** on the failing path, `evidenceSince == zero`, `NoProgressObs == 0`,
  `ReportsSinceProgress == 0`, `statsChannel == ""`.
- **Depended on by:** L555 (`evidenceSince.IsZero()` re-seeds the window next pass).

```go
// L555-L570
if st.evidenceSince.IsZero() {
    st.evidenceSince = now
    st.lastObservedSyncAt = sync.ProgressLastSyncAt
    st.NoProgressObs = 0
} else if sync.ProgressLastError == "" && !sync.ProgressLastSyncAt.IsZero() && sync.ProgressLastSyncAt.After(st.lastObservedSyncAt) {
    st.lastObservedSyncAt = sync.ProgressLastSyncAt
    st.NoProgressObs++
}
```
- **What:** Opens the evidence window, or counts one completed-and-unchanged inventory
  observation inside it.
- **Why here:** After the gates, so the cursor is seeded at a moment when farming is proven.
- **Assumes:** `ProgressLastSyncAt` strictly advances per observation, so `After` counts each
  observation at most once. Established: `recordProgressSync` (drops.go:656-665) and
  `publishProgressObservation` (drops.go:702-706) both stamp `time.Now()` on every accepted
  observation. Two observations inside the same monotonic-clock tick would collapse into one —
  conservative direction (under-counts), so it cannot manufacture a stall.
- **Assumes:** at most one observation lands per watchdog pass. Nothing enforces it: the light
  sync runs on its own cadence and `TriggerProgressSync` (L204) forces extra runs. If two
  observations complete between passes, only ONE is counted (`After` is not a delta) —
  again conservative.
- **Establishes:** `NoProgressObs` counts only observations that (a) completed without error,
  (b) landed strictly after the cursor, (c) occurred while every gate held.
- **Depended on by:** L573.

```go
// L572-L574
stalled := now.Sub(st.evidenceSince) >= cfg.StallDelay &&
    st.NoProgressObs >= cfg.StallConfirmations &&
    st.ReportsSinceProgress >= stallMinReports
```
- **What:** The conjunctive stall decision. Defaults: `StallDelay` 20 min, `StallConfirmations`
  3 (config.go:569-570, clamped 10-120 min / 2-10 at config.go:881-889), `stallMinReports` 5
  (L83, a package const with no runtime knob).
- **Why here:** All three counters are current for this pass only after L538 (reports),
  L547/L555-L570 (delay + observations).
- **Assumes:** the three terms are commensurate — same channel, same episode, same window.
  The delay and observation terms are re-anchored by `resetEvidence`; the report term is NOT
  re-derived here, it is whatever `observeProgress` last wrote. When `observeProgress`'s
  `n >= 0` guard (L700) suppresses an update, `st.ReportsSinceProgress` is a value from an
  earlier watcher-tenure and this conjunction reads it as current. See
  `watchdog-observe-progress.md` and dossier question 3. **Nothing found** — no test decreases
  a login's success count (`fakeWatchView.addSuccesses`, progress_test.go:180-187, only adds).
- **Assumes:** `now` is close to real time for every drop in the pass. It is not, when an
  earlier drop ran a blocking recovery stage: `full_resync` (L217-L221) and `transport_probe`
  (L244-L246) are each bounded by `recoveryStageTimeout = 60 * time.Second` (L86), and the
  single `now` from L386 is reused for the whole pass. `now.Sub(st.evidenceSince)` therefore
  UNDER-counts for later drops — conservative direction.

```go
// L585-L587
if !stageBudget && w.advanceRecovery(st, cfg, now) { stageBudget = true }
```
- **What:** At most one recovery-stage execution (or one pending-resolution) per pass.
- **Why here:** After `stalled`, so only confirmed stalls consume the budget.
- **Assumes:** map iteration order over `w.drops.Campaigns()` is a slice (drops.go:404-406), so
  order is the tracker's publication order — stable within a pass but not a priority order.
  The doc comment at L513-L515 claims the stage runs for "the worst-off drop"; the code runs it
  for the FIRST stalled drop in campaign-slice order. Discrepancy recorded, no verdict.

```go
// L601-L622
type recoveredNote struct{ campaign, drop, channel string }
var recovered []recoveredNote
w.mu.Lock()
for key, st := range w.states { if seen[key] { continue } ... delete(w.states, key) }
w.mu.Unlock()
for _, n := range recovered { w.notifier.NotifyDropRecovered(...) }
```
- **What:** Retires episodes whose drop left the tracked set; clears their avoid entry and
  closes a standing alert.
- **Why here:** After the loop, so `seen` is complete.
- **Assumes:** `w.notifier != nil` when `recovered` is non-empty. Established at L612
  (`st.notifiedStalled && w.notifier != nil` is the append condition).
- **Establishes:** the documented `w.mu` → notifications lock ORDER is never taken (comment
  L596-L600); notification I/O runs unlocked.

**Cross-Function Dependencies:**
- Callee `w.snapshotCfg` (internal, L400-L404): needs a consistent `WatchdogConfig`. Takes
  `w.mu`.
- Callee `w.drops.SyncStatus` (internal, `*drops.DropsTracker`, drops.go:584): needs
  `ProgressLastSyncAt`/`ProgressLastError` to be a value copy under the tracker lock. Satisfied.
- Callee `w.twitchOutage` (internal, L473-L492): needs a truthful "Twitch/account is fine"
  verdict. Depends on `Center.Snapshot` (center.go:139-147) and on
  `inconclusiveWatchTransport` (L453-L461) to discard canary-local provenance. Note
  `SignalWatchTransport` is recorded ONLY by `Canary.probe` against the single CONFIGURED
  `cfg.Channel` (canary.go:200-222, 279/289/300) — never against the farming channel.
- Callee `w.drops.Campaigns` (internal, drops.go:400): needs an immutable pool.
- Callee `w.trackDrop` (internal, L629-L663): needs "this campaign/drop is worth tracking" and
  a seeded `lastObservedSyncAt` (L655-L658). Takes `w.mu`.
- Callee `w.observeProgress` (internal, L668-L748): needs `st.Channel` and
  `st.ReportsSinceProgress` to describe the current farming tenure. See its own record.
- Callee `w.gatesHold` (internal, L754-L799): needs every non-threshold gate. See its own record.
- Callee `st.resetEvidence` (internal, L175-L180): needs the evidence window destroyed.
- Callee `w.advanceRecovery` (internal, L933-L971): may block up to 60s and performs Twitch
  writes/reads; may call `NotifyDropStalled` (L281) and `events.Record` (L283).
- Callee `w.publishFromStates` (internal, L974-L1001): needs `w.mu` and copies out
  `st.DropProgress` only.
- Callers: `loop` (L386) only. No external caller — `evaluate` is unexported and tests call it
  directly (progress_test.go, watchdog_outage_classification_test.go:366).
- Shared state: `w.states` (also touched by `trackDrop` L641-L662, `publishFromStates` L975-L980,
  and the `!cfg.Enabled` wipe L519-L521); `w.snap` (also read by `Snapshot` L357 and
  `ProgressSignal` L1012 from other goroutines).
- Invariant couplings: the conjunction at L572-L574 is the ONLY consumer of
  `stallMinReports`; `ReportsSinceProgress` is written only in `observeProgress` (L680, L701)
  and `resetEvidence` (L178). The pass therefore couples three independently-published
  watcher atomics (`brokerSnapshot`, `watchingLogins`, `reportStatsSnap`) with one drops-tracker
  value copy, with no version/generation check joining them.

**Open Questions:**
- L513-L515 says the stage runs for "the worst-off drop"; the code runs it for the first
  stalled drop encountered. Which is intended?
- Is the `!cfg.Enabled` wipe of `notifiedStalled`/`avoidedChannel` (L519-L521) intended to
  re-arm the critical alert and orphan avoid entries on a disable→enable cycle?
- Should the `sync` read (L526) and the `Campaigns()` read (L532) be joined by
  `SyncStatus.Revision` (drops.go:63-67), which exists precisely to prove two reads describe
  the same backend snapshot?
- Is a gate failure supposed to freeze `st.pending`'s wall-clock deadline (L843), or is the
  observed "gate down > 5 min ⇒ guaranteed timeout resolution" the intended behavior?
