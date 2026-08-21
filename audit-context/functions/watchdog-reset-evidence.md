# `dropState.resetEvidence` — internal/health/progress.go (base 6b1d80e)

Part of the `stall-confirmation-seam` audit context. Index:
`audit-context/DOSSIER-stall-confirmation-seam.md`.

---

## `resetEvidence` in internal/health/progress.go (L175-L180)

**Purpose:** Destroys the stall-evidence window when the proof of active farming is broken.
Four lines, but it is the mechanism that makes the whole detector false-positive-averse: it
guarantees a confirmed stall represents at least `StallDelay` of UNINTERRUPTED demonstrable
farming without credit, rather than evidence stitched across offline/rotated-out gaps. The
`dropState` doc at L145-L152 states this contract explicitly ("evidence accrued while the
channel was offline, rotated out, or ineligible never carries over").

**Inputs & Assumptions:**
- Receiver `st *dropState` (trusted, loop-owned). No parameters, no return.
- Implicit: called only from the watchdog goroutine. Established: all three call sites
  (L547, L891, L924) sit inside `evaluate`'s pass or inside `resolvePending`/`rebaselineEpisode`,
  which `evaluate` reaches through `advanceRecovery` (L585 → L937-L939).
- Precondition assumed by the caller at L547: destroying evidence is the correct response to
  ANY gate failure, however brief. Established by the comment at L541-L546 and by
  `TestWatchdogGateFailureResetsStallEvidence` (progress_test.go:900ff, described at
  progress_test.go:898-905 as review scenario A).

**Outputs & Effects:**

```go
// L175-L180
func (st *dropState) resetEvidence() {
	st.evidenceSince = time.Time{}
	st.NoProgressObs = 0
	st.ReportsSinceProgress = 0
	st.statsChannel = "" // force a delivery re-baseline even for the same channel
}
```

Exactly four writes. What it DESTROYS:
- `st.evidenceSince` → zero. Consequence: the next passing `evaluate` takes the L555-L563
  branch, re-seeds `evidenceSince = now` and `lastObservedSyncAt = sync.ProgressLastSyncAt`,
  and zeroes `NoProgressObs` again. The `StallDelay` clock restarts from scratch.
- `st.NoProgressObs` → 0. The `>= StallConfirmations` term restarts.
- `st.ReportsSinceProgress` → 0. The `>= stallMinReports` term restarts.
- `st.statsChannel` → "". This is the load-bearing line: it forces
  `observeProgress`'s `channel != st.statsChannel` branch (L676) on the very next pass EVEN
  for an unchanged channel, which re-reads `ReportStats` and re-adopts `baselineReports` /
  `baselineValid` (L681-L689). Without it, `st.baselineReports` would still refer to the
  pre-gap tenure and the delta at L700 would be wrong (or negative, and silently suppressed).

What it deliberately PRESERVES (comment L172-L174): `st.RecoveryStage`, `st.RecoveryStageName`,
`st.LastRecoveryAt`, `st.exhaustedAt`, `st.notifiedStalled`, `st.avoidedChannel`,
`st.Status`, `st.Detail`, `st.LastMinutes`, `st.LastProgressAt`, `st.baselineReports`,
`st.baselineValid`, `st.lastObservedSyncAt`, `st.Channel`, and `st.pending`.

- `baselineReports`/`baselineValid` surviving is harmless because `statsChannel = ""` forces
  them to be overwritten at L679-L689 before they are next used at L700.
- `lastObservedSyncAt` surviving is harmless because L562 re-seeds it whenever the window
  reopens.
- `st.pending` surviving is NOT neutralised anywhere. At the L547 call site `evaluate`
  `continue`s (L552) without reaching `advanceRecovery`, so the pending correlation's
  wall-clock `deadline` (`now.Add(recoveryOutcomeDeadline)`, L843; `recoveryOutcomeDeadline`
  = 5 min, L94) keeps running while the gate is down. A gate outage longer than 5 minutes
  therefore guarantees that the first passing pass resolves the parked stage as a TIMEOUT
  (L862-L868) even if the broker executed it correctly. **Nothing found** that pauses or
  extends the deadline across a gate failure.
- At the L891 call site (`outcome.Skipped`) `st.pending` was already cleared at L876, so the
  interaction does not arise there. At L924 (`rebaselineEpisode`) it is cleared at L919.

**Cross-Function Dependencies:**
- Callee: none. No external calls.
- Callers (complete, verified by grep over `internal/`):
  1. `evaluate` L547 — any `gatesHold` failure. Assumes it makes the three thresholds
     un-satisfiable until a full fresh window accrues.
  2. `resolvePending` L891 — `outcome.Skipped` (the channel lost its slot while an async
     recovery stage was parked). Assumes it forces the gates to re-prove active farming before
     the rolled-back stage (`st.RecoveryStage = stageIndex`, L890) retries.
  3. `rebaselineEpisode` L924 — reached from `resolvePending`'s `outcome.Stale` arm (L882),
     i.e. the broadcast/session was superseded. Assumes the fresh session is judged on its own
     merits.
  There is NO caller for the case that motivates it most directly: a same-login watcher
  counter reset. `publishReportStats` (watcher/session.go:627-634) can zero a login's
  `Successes` without any of the three conditions above becoming visible to the watchdog —
  see `watchdog-observe-progress.md`.
- Shared state: `st` fields are also written by `observeProgress` (L679-L701, L719-L741),
  `evaluate` (L548-L551, L561-L569, L576-L581), `resolvePending` (L863-L909),
  `rebaselineEpisode` (L919-L925), `advanceRecovery` (L943-L967).
- Invariant couplings: `statsChannel = ""` is what couples this function to
  `observeProgress`. If that line were dropped, the delta at L700 would be computed against a
  pre-gap baseline; the `n >= 0` guard would silently swallow the resulting negative and leave
  `ReportsSinceProgress` at the 0 written here — i.e. the failure would be conservative in this
  direction. The dangerous direction is the mirror case: the watcher's counter resets and
  `resetEvidence` is NOT called.

**Open Questions:**
- Should `resetEvidence` also clear (or freeze) `st.pending`, so a gate failure does not
  consume the 5-minute recovery-outcome deadline?
- Should `st.Detail`/`st.Status` be part of the reset? Today `evaluate` overwrites them
  immediately after the call (L548-L551), but callers 2 and 3 set them themselves
  (L892-L893, L925) — three call sites, three different post-conditions for the published
  fields.
