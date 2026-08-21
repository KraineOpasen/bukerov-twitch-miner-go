# Variant analysis — "stale counter / stale timestamp as proof of a current condition"

Scope: `internal/health/**` exhaustively, plus read-only sweeps of `internal/watcher`,
`internal/drops`, `internal/miner/health.go` and the collaborators reachable from the
progress watchdog. Base SHA `6b1d80e6d014b54d476869ed736e39f126369210`, branch
`claude/drop-stall-confirmation-evidence-p1vf1l`. READ-ONLY: nothing outside this file was
written; no fix is proposed outside `internal/health/**`.

Method: the five steps of `.claude/skills/variant-analysis/SKILL.md`.

---

## Step 1 — Root cause

Two sub-patterns of one assumption: *the source counter/timestamp is a faithful
report of the present.*

**(A) Currency.** A cumulative/monotone counter, or a timestamp of an
outcome-agnostic event, is read as proof of a condition holding *right now*, with no
recency bound and no check of the *last* outcome.

> Invariant violated: "`ReportsSinceProgress >= 5` must mean *we are delivering
> minute-watched reports to this channel now*." It actually means *at some point since the
> baseline, five deliveries succeeded* — the counter never falls, and
> `ReportStats.Failures`, `.LastSuccess` and `.LastFailure`
> (`internal/watcher/session.go:44-47`) are never read by the watchdog.

**(B) Regression.** A delta computed against a remembered baseline is guarded with
`if n >= 0`, so when the underlying counter *resets to a lower value* the branch is
skipped and the previously-derived state is silently retained — stale, and high.

> The source's own doc says it resets: `internal/watcher/session.go:38-42` —
> "Counters accumulate while the channel keeps holding a slot **and reset when it leaves
> the allocation** (rotation, displacement, offline)". The guard at
> `internal/health/progress.go:700` is written against a counter documented to reset.

### Expansion axes (grounded before searching)

1. Identifiers playing the same role: `Successes`, `Failures`, `NoProgressObs`,
   `ReportsSinceProgress`, `RecoveryStage`, `reqSeq`, `progressRuns`, `syncRuns`,
   `revision`, `CurrentMinutesWatched`, `LastMinutes`, `samples`.
2. Timestamps read as proof of a *positive* condition: `CheckedAt`, `LastSuccess`,
   `LastSuccessAt`, `LastSyncAt`, `ProgressLastSyncAt`, `LastActivity`, `EvaluatedAt`,
   `evidenceSince`, `Until`.
3. Status/predicate reads with no age bound: `Signal.Status`, `Signal.Healthy()`,
   `IsWatching`, `BrokerSnapshot`.
4. Delta/regression handling: `x - baseline`, `>= 0`, `!= prev`, `> last`.
5. Doc/code mismatch: a field documented as "confirmed"/"successful" whose code path
   measures "attempted"/"checked".

---

## Step 2 — Exact match (calibration)

```
grep -n "n >= 0\|>= 0\b" internal/health/*.go internal/watcher/*.go \
                          internal/drops/*.go internal/miner/health.go
```

One hit of the target shape, and it is the known instance:

- `internal/health/progress.go:700` — `if n := stats.Successes - st.baselineReports; n >= 0 {`

All other `>= 0` hits are index-validity checks on a slot index (`idx >= 0`,
`internal/watcher/broker.go:98,117,236,271,281,305,493,516`, `debug.go:115`,
`watcher.go:755,1322`) or reverse-loop bounds (`sender.go:302,331`) — a different family,
correctly excluded. The pattern hits the known instance and nothing else, so the ladder
below is calibrated against the right code.

Companion exact match for (A): `internal/health/progress.go:574`
(`st.ReportsSinceProgress >= stallMinReports`, constant at `progress.go:79-83`).

## Steps 3–4 — Generalization ladder (one element per rung)

| # | Change from previous rung | Query | Matches | Kept |
|---|---|---|---|---|
| 0 | exact | `n := stats.Successes - st.baselineReports; n >= 0` | 1 | known instance |
| 1 | drop the identifiers, keep the shape | `>= 0` over health/watcher/drops/miner-health | 14 | 1 (13 index checks) |
| 2 | drop the guard, keep "cumulative counter vs threshold" | `>=`/`++`/`Status ==` in `internal/health/*.go` | 33 | 6 |
| 3 | widen to "timestamp read without a recency bound" | `IsZero()`, `.After(`, `.Sub(` in `internal/health/*.go` | 15 | 3 |
| 4 | widen to consumers of `health.Center` anywhere | `grep -rn "snap.Signal(" internal/` | 4 sites | 3 (1 in-scope) |
| 5 | widen to every `ReportStats` consumer | `grep -rn "ReportStats"` | 2 consumers | 2 (1 deferred) |
| 6 | widen to "counter assumed monotone" | `grep -rn "CurrentMinutesWatched"` | 20 | 1 |

Rung 7 (any `>=` against any int anywhere in `internal/`) was **not** run: at rung 6 the
noise ratio was already above half. Stopped per the skill's rule.

**Patterns that failed** (recorded so the next hunt does not repeat them):

- `grep "Since\|since"` across `internal/health` — matched mostly prose in doc comments;
  no signal.
- `grep "consecutive\|streak\|attempts\|failures" internal/drops/*.go` — six hits, all
  bare `++` on bookkeeping counters (`drops.go:620,632,659,704,2045`) that are never
  compared against a threshold. Zero variants; `internal/drops` is clean for this root
  cause on the watchdog-reachable surface (`Campaigns`, `SyncStatus`, `SyncNow`,
  `TriggerProgressSync`).

---

## Step 5 — Triage

Severity scale: HIGH = can produce a wrong operator-visible verdict plus recovery actions
on the live miner; MEDIUM = wrong verdict, bounded blast radius; LOW = doc/code mismatch or
degraded observability only.

### V1 — stall gate accepts a frozen success counter as live delivery `[A]` `[IN-SCOPE]`

`internal/health/progress.go:574` (with `progress.go:79-83`, `694-703`)

```go
stalled := now.Sub(st.evidenceSince) >= cfg.StallDelay &&
    st.NoProgressObs >= cfg.StallConfirmations &&
    st.ReportsSinceProgress >= stallMinReports
```

The known instance. `ReportsSinceProgress` only ever rises (`progress.go:700-702`) inside
an evidence window and is zeroed only by `resetEvidence()` (`progress.go:175-180`). Once it
crosses 5 it stays ≥5 for the rest of the window regardless of what happens next.
`watcher.ReportStats` carries `Failures`, `LastSuccess`, `LastFailure`
(`internal/watcher/session.go:44-47`), all written on every attempt
(`session.go:609-622`); the watchdog reads none of them.

Failure scenario: the channel delivers 5 successful beacons, then every subsequent send
fails (`internal/watcher/watcher.go:740` — `noteReportOutcome(..., false, ...)`), or the
watcher loop hangs and sends nothing at all. `Successes` freezes, `Failures` climbs (or
nothing moves). `ReportsSinceProgress` stays ≥5, `NoProgressObs` keeps climbing on the
drops tracker's own goroutine, `IsWatching` stays true (V7), and the gate confirms a
**Twitch-side drop stall** for a condition that is entirely local delivery failure — then
runs the recovery pipeline and fires the operator's critical `NotifyDropStalled`
(`progress.go:646-660` stage `notify`).

Stale sends make this sharper: `res.Stale` counts as neither success nor failure
(`internal/watcher/watcher.go:730-738`), so a channel stuck in a stale-session loop
produces *no* counter movement at all while `ReportsSinceProgress` holds its old value.

Same root cause: **yes — this is the instance.** Severity: **HIGH**.
Test coverage: none. `TestWatchdogReportThresholdGates` (`progress_test.go:777`) only
proves the *floor* (0 reports ⇒ no stall); no test drives successes-then-failures.

### V2 — negative delta silently retains the stale count `[B]` `[IN-SCOPE]`

`internal/health/progress.go:700-702`

```go
if n := stats.Successes - st.baselineReports; n >= 0 {
    st.ReportsSinceProgress = n
}
```

When `stats.Successes < st.baselineReports` the assignment is skipped and
`ReportsSinceProgress` keeps its previous (higher) value, while `baselineValid` stays true
so the adopt-baseline branch at `progress.go:695-699` cannot repair it. The re-baseline at
`progress.go:676-691` only fires on `channel != st.statsChannel` — a *reset under the same
login* never triggers it.

Two confirmed reset paths, both under an unchanged login:

1. **Slot churn.** `publishReportStats` (`internal/watcher/session.go:627-641`) prunes the
   map to currently-slotted logins every tick; a login that loses and regains a slot
   between two watchdog passes restarts at `Successes = 0`. Watcher tick is
   `MinuteWatchedInterval`, clamped to [30,120]s (`internal/config/config.go:797-801`,
   default 60); the watchdog evaluates on a ±20%-jittered 60s cadence
   (`progress.go:76-77`, `374-390`) — a one-tick gap is routinely invisible to it.
2. **Roster edit.** `applyStreamerList` (`internal/watcher/watcher.go:544-553`) explicitly
   `delete(w.reportStats, login)` for a departed streamer, with the comment "so a
   same-process re-add of the login starts clean (BKM-018A)". A remove-then-re-add of the
   same login through the runtime streamer list is a documented, deliberate reset.

Failure scenario: `baselineReports = 40`, `ReportsSinceProgress = 12`; slot churn resets
the counter; next pass reads `Successes = 1`, `n = -39`, guard skips, `ReportsSinceProgress`
stays 12 — five watched minutes of "demonstrable farming" that never happened, feeding
straight into V1's gate.

Same root cause: **yes — this is the instance.** Severity: **HIGH**.
Test coverage: none. The harness only ever calls `addSuccesses`
(`progress_test.go:830`, `881`, `893`); no test lowers a counter.

### V3 — `twitchOutage` trusts a health signal of unbounded age `[A]` `[IN-SCOPE]`

`internal/health/progress.go:477-490`

```go
sig, ok := snap.Signal(name)
if !ok || (sig.Status != StatusFailed && sig.Status != StatusDegraded) { continue }
```

`Signal.CheckedAt` (`internal/health/center.go:55`) is never consulted. For
`oauth`/`gql_api`/`pubsub` this is benign — `refreshHealthCenter`
(`internal/miner/health.go:159-222`) re-records all three on every health tick. For
`watch_transport` it is not: **the canary is the only producer**
(`grep -rn "SignalWatchTransport"` → recorded only at `internal/health/canary.go:279,289,300`).

Defaults: `CanaryIntervalMinutes = 360` (6h, clamped [60,1440]) and
`CanaryMaxStalenessHours = 48` (clamped [1,168]) — `internal/config/config.go:560-563`,
`856-874`. So a `watch_transport` reading is *routinely* hours old and may legitimately be
up to 24h/168h old at the clamp edges.

Failure scenarios:
- A genuine, gating failure (e.g. a `beacon_http_403`, which `inconclusiveWatchTransport`
  at `progress.go:441-451` deliberately does **not** deny-list) is recorded at T. The
  operator then clears `CanaryChannel` or unsets `CanaryEnabled`
  (`canary.go:185-187`, `207-209` both return before recording). Nothing ever overwrites
  the signal — `Center.Record` is the only mutator (`center.go:113-118`) and nothing
  expires entries. `twitchOutage` returns `(true, "watch_transport")` **forever**, so
  `gatesHold` fails at `progress.go:754-756` on every pass and the watchdog is
  permanently blinded: no stall ever confirms again.
- Symmetrically, a stale `StatusOK` is treated as current evidence that the transport is
  fine for as long as the canary stays silent.

Same root cause: **yes.** Same mechanism as V1 (an old recorded fact used as a present
fact), different data source. Severity: **MEDIUM-HIGH** — the failure is silent and
permanent, and it disables exactly the subsystem this audit is about.
Test coverage: `TestWatchdogRetainedCanaryTimeoutDoesNotMasqueradeAsOutage`
(`watchdog_outage_classification_test.go:355-378`) covers a retained signal whose *code* is
deny-listed (`"timeout"`); it does not cover a retained signal with a **gating** code, and
it exercises no clock advance at all. The age dimension is untested.

### V4 — `health.Center` has no freshness contract `[A]` `[IN-SCOPE]` (structural)

`internal/health/center.go:52-93`, `112-118`, `149-161`

`Signal` carries `CheckedAt` but the package ships only `Healthy()` (`center.go:66-68`) and
`Degraded()` (`center.go:74-76`) — both pure status predicates. `Center` never expires or
prunes a recorded signal, and `Snapshot.Signal(name)` (`center.go:86-93`) returns whatever
was last written with no age attached. This is the structural enabler of V3 and of the
two out-of-scope consumers below; it is the natural home of a fix.

Same root cause: **yes**, as the enabling API rather than a discrete bug.
Severity: **MEDIUM**.

> **Hard constraint on any Center-wide freshness rule.**
> `internal/miner/connection_health.go:174-177` deliberately overwrites the gql_api
> signal's timestamp with the last *success*:
> ```go
> sig := health.Signal{Name: health.SignalGQLAPI, CheckedAt: now}
> if !h.LastSuccess.IsZero() { sig.CheckedAt = h.LastSuccess }
> ```
> so for `gql_api`, `CheckedAt` means "last success", not "last check". A blanket
> "`CheckedAt` older than X ⇒ ignore this signal" rule would therefore *discard* a
> genuinely-failing GQL signal precisely while the API is down — inverting the gate. A
> freshness fix must be per-signal (watch_transport is the one that needs it) or must
> coordinate with that producer, which is out of scope.

### V5 — canary staleness measures *checked*, not *confirmed* `[A]` `[IN-SCOPE]`

`internal/health/canary.go:462-468` consumed at `canary.go:188-194`

```go
func (c *Canary) sinceLastTransportCheck() time.Duration {
    sig, ok := c.center.Signal(SignalWatchTransport)
    if !ok || sig.CheckedAt.IsZero() { return time.Duration(1<<62 - 1) }
    return c.now().Sub(sig.CheckedAt)
}
...
due   := since >= cfg.Interval
stale := cfg.MaxStaleness > 0 && since >= cfg.MaxStaleness
```

`sig.Status` is never inspected, so a *failed* probe resets the staleness clock exactly
like a successful one. The documented contract says otherwise, twice:
`internal/config/config.go:359-360` — "the target cadence for a **successful
confirmation**"; `config.go:363-365` — "forces a probe … once the watch transport **has not
been confirmed** for this long"; and `canary.go:68-70` repeats it. This is the mirror form
of the root cause: an outcome-agnostic timestamp standing in for proof of a positive
condition, with no last-outcome check.

Argued against: the practical harm is small. A failing probe *is* still a probe, and the
opportunistic `due && slotFree` path re-fires every `Interval` regardless; when slots stay
busy the clock does grow and `stale` does force a run. The consequence is confined to the
`stale` branch's meaning (it forces on attempt-age, not confirmation-age) and to the
promise the settings UI makes to the operator.

Same root cause: **yes**, same shape, materially weaker consequence. Severity: **LOW**.
Test coverage: `TestCanaryMaybeRunScheduling` (`health_test.go:353`) exercises the
scheduling arithmetic, not the success/failure distinction.

### V6 — `CurrentMinutesWatched` regression absorbed, then re-read as progress `[B-mirror]` `[IN-SCOPE]`

`internal/health/progress.go:706` and `progress.go:745`

```go
if drop.CurrentMinutesWatched > st.LastMinutes {   // 706: treated as PROGRESS
...
st.LastMinutes = drop.CurrentMinutesWatched        // 745: unconditional, absorbs a drop
```

Where V2 silently *ignores* a regression, this silently *accepts* one and then
manufactures a false positive from the recovery. Line 745 assigns unconditionally, so a
momentary lower reading lowers `LastMinutes`; the next correct reading is then strictly
greater and takes the progress branch at 706 — which resets the entire episode
(`progress.go:723-741`), clears the avoid entry (`progress.go:718-721`), records
`TypeDropRecovered`, and fires `NotifyDropRecovered` (`progress.go:707-712`) claiming
"progress resumed: N/M minutes" when no minute was ever credited.

Reachability of a regression, from the drops side:
- `Drop.Update(selfData)` is the sole write path for `CurrentMinutesWatched`
  (`internal/models/drop.go:139-141`; stated at `internal/drops/drops.go:1490-1492`).
- A full sync rebuilds campaigns from `GetDropCampaignDetails` with minutes at zero, then
  merges inventory. If the inventory *acquisition* fails the sync aborts and never
  publishes (`drops.go:1498-1505`) — that path is correctly defended.
- But a campaign that decodes fine and is simply **absent from
  `dropCampaignsInProgress`** matches no `progID` in the merge loop
  (`drops.go:1526-1544`), keeps `CurrentMinutesWatched == 0`, and *is* published. The
  campaign key (`campaign.ID + drop.ID`, `progress.go:637`) is unchanged, so the watchdog
  state survives to see the regression.

Same root cause: **same family, mirrored.** Both rest on "the source counter only moves
up"; the guard direction differs. Call it a variant, not a duplicate. Severity: **MEDIUM**.
Test coverage: none — no test in `internal/health` lowers `CurrentMinutesWatched`.

### V7 — `IsWatching` gate has no recency bound, though the timestamp exists `[A]` `[IN-SCOPE, folds into V1]`

`internal/health/progress.go:772` reading `internal/watcher/broker.go:411-417`

`IsWatching` returns a published `map[string]bool` with no timestamp of its own. The
watcher *does* publish `BrokerSnapshot.EvaluatedAt` (`internal/watcher/broker.go:69`) and
the watchdog already calls `BrokerSnapshot()` in `farmingChannel`
(`progress.go:497-509`) — it simply never reads the field. Consequently, if the watcher
loop hangs or dies, every gate in `gatesHold` still holds: `IsWatching` true, streamer
resolvable, campaign eligible, inventory syncs fresh on the drops goroutine — and
`ReportsSinceProgress` frozen ≥5 by V1. Nothing in the conjunction can notice that no
minute has been delivered in an hour.

Same root cause: **yes**, and it is the mechanism that makes V1's failure scenario
reachable rather than theoretical. Severity: **MEDIUM** (contributory). Fixing V1 with a
last-outcome/recency check on `ReportStats` closes this too; `EvaluatedAt` is a second,
independent lever already available.

---

## Refuted candidates (looked, found the protection)

Recorded so the next pass does not re-open them.

| Location | Why it is not a variant |
|---|---|
| `internal/health/progress.go:573` (`NoProgressObs >= StallConfirmations`) | Monotone, but every increment is gated on a *fresh, error-free, strictly newer* `ProgressLastSyncAt` (`progress.go:564-569`) and it is zeroed by `resetEvidence` (`progress.go:177`) whenever any gate fails. Carries its own recency. This is the correct shape. |
| `internal/health/progress.go:766` (zero `ProgressLastSyncAt` skips the recency gate) | Unreachable. `NoProgressObs` cannot increment while `ProgressLastSyncAt.IsZero()` (`progress.go:564`), and `StallConfirmations` is clamped to ≥2 (`internal/config/config.go:886-889`), applied through `ValidateConfig` on every settings write (`internal/miner/health.go:462`). The stall cannot confirm with zero observability. |
| `internal/health/progress.go:763-765` (`ProgressLastError != ""`) | A true last-outcome field: set on failure and **cleared on success** (`internal/drops/drops.go:656-666`, `704-706`). Last-outcome check plus recency check, both present. Exactly what V1 lacks. |
| `internal/health/progress.go:846-880` (`resolvePending`) | The model implementation: an outcome is accepted only on an exact `RequestID` **and** `Signature` match, with a bounded `recoveryOutcomeDeadline` (`progress.go:93-96`). An old or foreign outcome cannot be mistaken for the current one. |
| `internal/health/avoid.go:63-67`, `78-81` | Correct TTL: `now().After(e.Until)` prunes lazily on read. |
| `internal/health/canary.go:464-466` | Zero `CheckedAt` ⇒ effectively infinite staleness ⇒ due and stale. Fails toward probing, the conservative direction. |
| `internal/watcher/session.go:592-604` (`refreshOutcomes` never expires) | Unbounded retention, but exact-match correlation (V-refuted above) makes staleness unexploitable; `applyStreamerList` prunes departed logins anyway (`watcher.go:554`). |
| `internal/twitch/connhealth.go:63-64` | `RecentTransportFailures` / `RecentFunctionalFailures` are **windowed** (`count(now, window)`), not cumulative. Correct. |
| `internal/miner/connection_health.go:38-64` (`classifyAPI`) | Explicitly "NOT staleness-based" and combines a windowed failure count with a recency-bounded `LastSuccess`. Correct. |
| `internal/drops/drops.go:620,632,659,704,2045` | Bare bookkeeping counters (`revision`, `syncRuns`, `progressRuns`, `filtered`); none is compared against a threshold or read as proof of a condition. |

## Out of scope / deferred

| Location | Sub-pattern | Same root cause? | Severity | Note |
|---|---|---|---|---|
| `internal/miner/policy.go:152-165` — **`channelStability`** | A | **Yes** | MEDIUM | **DEFERRED — do not fix.** `samples = stats.Successes + stats.Failures`; `stability = Successes/samples` over the whole slot tenure. `LastSuccess`/`LastFailure` are never read, so a channel that delivered 200 beacons and is now failing every single send scores ≈0.98 "reliable" and keeps its policy weight (`policy.go:118`). Mechanically the same (A) defect as V1: a cumulative counter as proof of a current condition, no recency, no last-outcome check. It does **not** carry (B) — the ratio is recomputed on each call and nothing derived is persisted, so a counter reset is absorbed harmlessly. |
| `internal/miner/health.go:135-155` — `minerBetHealthGate.AutoBetDecision` | A | Yes, weakly | LOW | Reads `snap.Signal(...).Status` with no age bound. Only `gql_api`/`pubsub` are consulted, and both are re-recorded on every health tick (`miner/health.go:180-192`), so staleness needs the health loop itself to be dead. Fails open on absent/unknown. |
| `internal/miner/connection_health.go:174-177` — `apiConnSignal` | — | No (enabler) | INFORMATIONAL | Sets `CheckedAt = h.LastSuccess`, so `CheckedAt` means "last success" for gql_api alone. Not a bug on its own; it is the constraint recorded under V4 that forbids a blanket Center-wide freshness rule. |
| `internal/web/handlers_health.go:81-98` + `handlers_system.go:275-289` | A | Partly | LOW | The dashboard renders `Ago` from `CheckedAt` (`formatHealthAgo`) so a human *can* see the age, but the status text and colour (`healthStatusDisplay`, `handlers_health.go:63-78`) are never aged: a six-hour-old `watch_transport` reads as a flat green "OK". Presentation only. |
| `internal/drops/drops.go:1526-1544` | — | No (source) | INFORMATIONAL | The publish path that lets `CurrentMinutesWatched` regress to 0, feeding V6. Fixing V6 inside `internal/health` (reject a regression instead of absorbing it) does not require touching this. |

## Suggested regression rules (for whoever implements)

1. Unit test: drive `Successes` **downward** under an unchanged farming login and assert
   `ReportsSinceProgress` does not survive the reset (covers V2). The harness already has
   `addSuccesses`; it needs the opposite.
2. Unit test: successes-then-failures with `LastFailure` newer than `LastSuccess` must not
   confirm a stall (covers V1).
3. Unit test: advance the clock well past `CanaryMaxStalenessHours` with a retained
   **gating** `watch_transport` failure and assert the outage gate stops treating it as
   current (covers V3). Complements the existing deny-listed-code test at
   `watchdog_outage_classification_test.go:355`.
4. Unit test: lower `CurrentMinutesWatched`, then restore it, and assert no
   `NotifyDropRecovered` fires (covers V6).
5. Lint/grep guard: flag `if n := <expr> - <baseline>; n >= 0` in `internal/health/**`.
   The exact-match query in Step 2 already isolates this shape with zero false positives
   across the four packages searched, so it is cheap to enforce.
