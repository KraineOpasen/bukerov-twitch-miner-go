---
artifact_contract: ce-unified-plan/v1
artifact_readiness: implementation-ready
execution: code
task_id: watchdog-delivery-recency
base_sha: 6b1d80e6d014b54d476869ed736e39f126369210
---

# Watchdog delivery recency

## Problem

`ProgressWatchdog` treats a **cumulative** count of successful minute-watched deliveries as
proof that the miner is **currently** delivering watch evidence.

The stall predicate (`internal/health/progress.go:572-574`) is:

```go
stalled := now.Sub(st.evidenceSince) >= cfg.StallDelay &&
    st.NoProgressObs >= cfg.StallConfirmations &&
    st.ReportsSinceProgress >= stallMinReports   // stallMinReports = 5
```

`ReportsSinceProgress` is assigned only at `progress.go:700-702` as
`stats.Successes - st.baselineReports`. `Successes` is monotone within a slot tenure
(`internal/watcher/session.go:613-616`) and the baseline is re-taken only on a farming-channel
change (`progress.go:676-690`) or on real drop progress (`progress.go:737-741`). Once the
counter crosses 5 inside an evidence window it stays `>= 5` for the rest of that window, no
matter what happens to delivery afterwards.

`watcher.ReportStats` already carries `Failures`, `LastSuccess` and `LastFailure`
(`internal/watcher/session.go:43-48`), written on every counted attempt
(`session.go:609-621`). **The watchdog reads none of them** — `progress.go:682`, `:698`,
`:700`, `:738` are the complete set of reads and every one takes `Successes` only.

Nothing else closes the hole. `gatesHold` (`progress.go:754-798`) has no delivery-outcome
condition at all; it never calls `ReportStats`. Its "we are demonstrably watching" gate is
`w.watch.IsWatching` (`progress.go:772`), which reads slot occupancy, not delivery.

### Continuity revalidation (this task, live base `6b1d80e`)

A prior READ_ONLY audit returned TRUE POSITIVE. All twelve pinned mechanism statements were
re-proved against the live bytes and **none was contradicted**. Three independent dispatched
contexts each tried to refute the finding on a different axis (delivery-outcome gating,
production reachability of the failure family, adverse consequence) and **all three failed to
refute**.

### The production-reachable family

A beacon/spade failure on the farming channel while that channel stays online, slotted and
eligible:

- the failure branch (`internal/watcher/watcher.go:739-748`) notes the failure and calls
  `CheckStreamerOnline`, whose contract is explicitly fail-open — only an authoritative
  `"stream": null` settles offline; transport, timeout, auth, PQNF and malformed responses all
  yield UNKNOWN (`internal/twitch/client.go:1315-1327`), so a live channel keeps its slot;
- no `health.Signal` is recorded. `SignalWatchTransport` is written **only** by the canary
  (`internal/health/canary.go:279,289,300`) probing the single *configured canary channel*
  (`canary.go:206-221`), which is opt-in and empty by default
  (`internal/config/config.go:560-561`);
- inventory observability is unaffected: `syncProgress` runs on its own timer
  (`internal/drops/drops.go:811-832`, default 2 min), so `ProgressLastError` stays empty,
  `ProgressLastSyncAt` stays fresh, and `NoProgressObs` keeps climbing.

Concrete surviving scenario at stock defaults (StallDelay 20m, StallConfirmations 3,
DropProgressSyncInterval 2m, MinuteWatchedInterval 60s): five deliveries land in the window's
first five minutes; at t+6m the spade endpoint starts rejecting the beacon; for the next
fourteen minutes every send fails; at t+20m all three thresholds hold and the stall confirms —
advertising five 15-minute-old successes as current farming evidence.

Consequences are real, not cosmetic: the pipeline is stage-count-driven and never re-reads what
a stage learned (`progress.go:933-971`), so even a stage-4 probe that reports
`"watch transport probe failed at the beacon stage"` does not stop stage 6 from excluding the
farming channel for `AvoidTTL` (default 60 min) or stage 7 from firing the terminal operator
alert.

### Same-root second defect: the negative-delta freeze

`progress.go:700`:

```go
if n := stats.Successes - st.baselineReports; n >= 0 {
    st.ReportsSinceProgress = n
}
```

A **decrease** is read as "stale read, ignore" rather than "the counter tenure restarted", so a
value earned in a previous tenure survives. Two confirmed production reset paths exist for an
**unchanged login**:

- `publishReportStats` rebuilds `w.reportStats` from this tick's slots only
  (`internal/watcher/session.go:627-634`), so one tick out of the allocation drops the entry;
- `applyStreamerList` does an explicit `delete(w.reportStats, login)`
  (`internal/watcher/watcher.go:544-553`, documented "so a same-process re-add starts clean").

Neither changes `channel`, so the re-baseline branch at `progress.go:676` never fires,
`baselineValid` stays true so the adopt branch at `:695` cannot repair it, and none of the three
writers of `statsChannel` runs. The watchdog samples on a jittered ~60s loop while
`MinuteWatchedInterval` is clamped to [30,120]s, so a one-tick gap can fall entirely between two
passes.

## Root cause

One sentence: **the watchdog proves a present-tense claim ("we are demonstrably watching") with
a past-tense measurement (a cumulative counter), and has no way to notice that the measurement
stopped being about the present.**

Both defects are that same mistake in two directions — no recency/last-outcome check on the way
up, and no tenure-restart detection on the way down.

## Sample coherence is part of the defect

`observeProgress` calls `w.watch.ReportStats` at **three** sites — `progress.go:681`, `:694`,
`:737` — up to three times per drop per pass, and `evaluate` reads no stats at all. Each call is
an independent `reportStatsSnap.Load()` (`internal/watcher/session.go:237-244`); there is no
whole-map accessor, no pass token, no generation counter. Divergence is ordinary rather than
theoretical:

- sites `:694` and `:737` are separated by `NotifyDropRecovered` (`progress.go:709`), which
  reaches a synchronous SQLite read in the notifications manager;
- one blocking recovery stage per pass can hold the goroutine for up to
  `recoveryStageTimeout` = 60s, so two campaigns farmed on the same login read different
  counters within one pass;
- the broker publishes `reportStatsSnap` last in each tick (`watcher.go:778`), after a send loop
  spread over roughly one `MinuteWatchedInterval`.

Any fix that derives `ReportsSinceProgress` from one snapshot and delivery currentness from
another can gate a fresh `LastSuccess` against a count from a different tenure. The repair
therefore **collapses the reads to one per drop per pass** — that collapse is the substance;
returning the sample is only how it reaches the predicate.

## Selected design

`observeProgress` takes **one** `ReportStats` sample per drop per pass, after `channel` settles,
and returns it wrapped in a small package-private value:

```go
// deliveryEvidence is what ONE coherent watcher.ReportStats observation says
// about the farming channel's minute-watched delivery.
type deliveryEvidence struct {
    sampled bool                 // a ReportStats read succeeded on this pass
    stats   watcher.ReportStats  // the sampled counters/timestamps
}

func (d deliveryEvidence) current(now time.Time, horizon time.Duration) bool
```

`evaluate` adds `ev.current(now, cfg.StallDelay)` as a fourth conjunct of the **stall threshold**
— never to `gatesHold`.

### Shape comparison

Four shapes were assessed in an independent context. `multipleViable` came back **false**, so no
`plan-arbiter` round is required.

| Shape | Coherent | Verdict |
| --- | --- | --- |
| A — `observeProgress` returns the coherent sample | yes | **selected** |
| B — returns a narrower private evidence value | yes | equivalent; folded into A |
| C — private `dropState` fields mirroring the timestamps | **no** | rejected |
| D — a helper re-reads `ReportStats` at the predicate | **no** | rejected |

- **C rejected**: converts a two-value local dataflow into shared mutable state readable by
  seven functions, joining three baseline fields that already have subtle reset rules
  (`resetEvidence` clears `statsChannel` but preserves the pipeline flags; the progress branch
  replaces `*st` wholesale) and that already have a dedicated regression test for getting them
  wrong. A mirror also cannot distinguish "no sample this pass" from "same sample as last pass".
- **D rejected**: a *fourth* read, separated from the count by `gatesHold` (which does its own
  watcher reads). It restates the bug one line lower. Decisively, no test in `internal/health`
  could detect D's defect — the fake answers every read in a pass identically.
- **A over B**: identical shape at different widths. A carries `Failures`/`LastFailure` at zero
  cost, and those are the only positive signal separating "delivery failing" from "delivery
  merely quiet" — stale sends (`watcher.go:730-738`) bump neither counter, so a
  stale-session loop freezes `Successes` without ever incrementing `Failures`.

The selected form takes A's information content and B's testable predicate: the whole sample is
carried, and the decision is a **pure method** on a private type, directly table-testable and a
clean single seam for the mutation probes.

Applying `codebase-design`'s vocabulary: `deliveryEvidence` is a **deep** module for its size —
the interface is one call (`ev.current(now, horizon)`), and behind it sit four rules the caller
never has to learn. It passes the **deletion test**: delete it and all four rules reappear inline
in an already-dense predicate.

### The currentness predicate

```go
func (d deliveryEvidence) current(now time.Time, horizon time.Duration) bool {
    if !d.sampled || d.stats.LastSuccess.IsZero() {
        return false
    }
    if !d.stats.LastFailure.Before(d.stats.LastSuccess) {
        return false
    }
    return now.Sub(d.stats.LastSuccess) <= horizon
}
```

Four rules, each with a decided edge semantic:

1. **No coherent sample ⇒ not current.** Fail-closed (A6). Today a missed read silently leaves
   `ReportsSinceProgress` at a value no live sample backs.
2. **Zero `LastSuccess` ⇒ not current** (A5). "Never delivered" is not proof of delivery.
3. **`LastFailure` not strictly before `LastSuccess` ⇒ not current.** A newer failure means the
   most recently completed counted attempt failed. **Equal timestamps also fail**: which attempt
   completed last is genuinely unknowable, and the watchdog's whole design bias is
   false-positive-averse. Production cannot produce equal non-zero timestamps for one login
   anyway — consecutive attempts are one full `MinuteWatchedInterval` apart.
4. **`age(LastSuccess) <= horizon` ⇒ current.** Boundary inclusive.

### Why `cfg.StallDelay` is the horizon, and why the boundary is `<=`

`gatesHold` already uses `StallDelay` as a freshness horizon for the sibling inventory-observability
evidence (`progress.go:766`):

```go
if !sync.ProgressLastSyncAt.IsZero() && now.Sub(sync.ProgressLastSyncAt) > cfg.StallDelay {
```

That gate holds when `age <= StallDelay` — the same inclusive boundary, expressed with the
opposite polarity. Reusing `StallDelay` is not a new knob and invents no constant; it is the
period the watchdog already claims represents demonstrable farming.

The one deliberate divergence from that sibling is the zero-time carve-out. `:766` *skips* its
check on a zero timestamp (a never-completed sync must not deadlock the watchdog at startup);
rule 2 above *fails* on a zero timestamp, because "never delivered" is exactly the case a
delivery gate must reject.

**Config bounds (proved, independent context).** Every production path to
`health.WatchdogConfig` funnels through `config.ValidateConfig`, which clamps
`watchdogStallDelayMinutes` to **[10, 120]** (`internal/config/config.go:881-885`); the dashboard
POST path re-validates before applying (`internal/miner/health.go:462`); there is no config
hot-reload and no other constructor. **`StallDelay <= 0` is not production-reachable.** In a
hand-built test config with `StallDelay == 0` the clause is **fail-closed** (blocks confirmation),
never fail-open, so no guard and no invented constant is needed — and the existing `:766` gate
degrades in the same direction.

**False-negative headroom.** `MinuteWatchedInterval` is clamped to [30,120]s
(`config.go:797-800`) with ±20% jitter, so during healthy farming `age(LastSuccess)` stays under
~2.4 minutes against a horizon of at least 10 minutes — a margin of 4× at the worst legal
config, ~8× at defaults.

### Why currentness is a threshold and NOT a `gatesHold` condition

A failing `gatesHold` calls `st.resetEvidence()` (`progress.go:547`), which destroys
`evidenceSince`, `NoProgressObs`, `ReportsSinceProgress` and the delivery baseline. Putting
delivery currentness there would make **one transient failed send erase a 20-minute evidence
window** — re-creating exactly the false-negative amplifier PR #208 removed.

As a threshold, the intended behaviour falls out: five successes → one transient failure →
confirmation blocked on that pass, evidence window intact → next success restores currentness →
a genuine no-progress stall confirms without rebuilding the whole window.

### Slot-blip repair

The `n >= 0` guard gains an else branch: a current counter **below** baseline means the tenure
restarted, so adopt the current counter as the new baseline and zero `ReportsSinceProgress`.
Future successes then accrue from the adopted baseline. `resetEvidence` is **not** called — the
counter restarting is not a gate failure — and watcher counter ownership is untouched.

## Scope

**In scope** — `internal/health/**`, `SPECIFICATIONS.md`, `docs/plans/**`, `audit-context/**`.

`internal/watcher/**` is a primary READ source and **not** a write surface. The repair needs no
watcher change: `ReportStats` already publishes everything required.

## Implementation units

### U1 — `deliveryEvidence` type and predicate (`internal/health/delivery_evidence.go`)

Add the private type with a single rule ladder (`notCurrentBecause`) that both
`current` and `describe` derive from, so the verdict and the published explanation
cannot drift. **Shipped in its own file** rather than appended to `progress.go`:
that file was already past 1,000 lines and the new concept is self-contained.

### U2 — collapse `observeProgress` to one sample and return it

- Take `stats, sampled := w.watch.ReportStats(channel)` **once**, after `channel` settles and
  only when `channel != ""`.
- Channel-change branch: use the same sample for the baseline instead of call site `:681`.
- Normal path: use the same sample for the adopt branch and the delta.
- Progress-advanced branch: use the same sample to re-baseline the fresh episode instead of call
  site `:737`.
- Return `deliveryEvidence{sampled, stats}`.

Signature change alone breaks zero tests: `observeProgress` has one caller and no test caller.

### U3 — negative-delta rebaseline

Replace the bare `if n >= 0` with an explicit two-branch form that adopts the new baseline and
zeroes `ReportsSinceProgress` when the counter went backwards.

### U4 — stall predicate and explainability

Add `ev.current(now, cfg.StallDelay)` as a fourth conjunct at `progress.go:572-574`, and extend
the healthy `Detail` so a blocked confirmation names delivery currentness as the reason — the
section's stated explainability property ("any failing gate is named in the published state").

### U5 — test-harness completion (`internal/health/progress_test.go`)

`fakeWatchView` models only `successes map[string]int` and returns
`watcher.ReportStats{Successes: n}` — a permanently zero `LastSuccess`. This is a **gap in the
fake relative to the real contract**, not a behaviour to preserve: `noteReportOutcome` always
stamps a timestamp.

- store `watcher.ReportStats` values instead of ints;
- give the fake an injectable clock so `addSuccesses(login, n)` keeps its **exact existing
  signature and every existing call site byte-identical**, stamping `LastSuccess` only when
  `n > 0` (the `addSuccesses(login, 0)` calls exist purely to make the map entry present);
- add `addFailures`, a way to drop a login (models `ReportStats` unavailable), and
  a way to restart a counter lower (models a tenure reset).

**Shipped shape:** the per-field setters are one `mutateStats(login, apply)` helper
plus `addSuccesses`/`addFailures` on top of it, rather than an exact-stats setter —
the tests express broker *events*, not raw struct values. The fake also counts
`ReportStats` reads, so the one-sample-per-pass invariant is directly assertable.

### U6 — `SPECIFICATIONS.md`

Exactly two clauses, both made incomplete by this change:

- **lines 1524-1527**, the evidence-window paragraph whose "so a confirmed stall always
  represents at least `watchdogStallDelayMinutes` of *demonstrable* farming without credit"
  conclusion this change finally makes true;
- **lines 1541-1542**, gate 6.

Line 1518's state inventory needs no edit — the design deliberately stores no new state.

**Explicitly not touched:** gate 10 (lines 1555-1556). Its incompleteness relative to
`twitchOutage`/`inconclusiveWatchTransport` (missing DEGRADED, missing the provenance exemption)
is confirmed pre-existing documentation debt from PR #208 and is **deferred**. "We are already
editing the same section" is not authority to widen. Likewise untouched: gate 8's "more than"
vs the code's `>=` (pre-existing, cosmetic).

## Tests

### RED 1 — `TestWatchdogStaleDeliveryEvidenceDoesNotConfirmStall`

Accumulate `>= stallMinReports` successes, hold slot/eligibility/clean-inventory constant, then
stop succeeding and start failing. Base: recovery starts. Fixed: no recovery while the latest
attempt is a failure, `evidenceSince`/`NoProgressObs` **not** reset by the failure, and — the
false-negative control — a resumed success lets a genuine stall confirm without a full new window.

### RED 2 — `TestWatchdogExpiredDeliveryEvidenceDoesNotConfirmStall`

Isolates the recency clause: enough successes, **no newer failure**, `LastSuccess` aged past
`StallDelay`. Must kill a mutant that removes only clause 4.

### RED 3 — `TestWatchdogSlotBlipDoesNotFreezeReportsBaseline`

Non-zero baseline, `ReportsSinceProgress >= 5`, then a same-login tenure reset expressed through
the fake's `WatchView` (never by touching `dropState`). Base: the stale value freezes. Fixed:
baseline re-adopted, count zeroed, later successes accrue from the new baseline.

### Compatibility tests (14 pinned items)

Transient failure does not reset the window; a later success restores currentness; zero
`LastSuccess`; newer `LastFailure`; newer `LastSuccess` inside the horizon; no newer failure but
an expired success; the exact boundary; `ReportStats` unavailable; continuous delivery still
confirms a true stall; channel-switch rebaseline unchanged; PR #208 canary tests unchanged;
genuine outage still resets; recovery order/correlation unchanged; no `DropProgress` JSON change.

The pure-predicate items (zero/equal/ordering/boundary) go in a table test giving
`current` a direct test surface. **Shipped in the new topic file** rather than
`predicates_test.go`, following the precedent of
`watchdog_outage_classification_test.go`, which keeps its own predicate table
(`TestInconclusiveWatchTransportPredicate`) beside its state-machine tests.

### Mutation probes

- **M1** — bypass clause 3 ⇒ RED 1 must fail.
- **M2** — bypass clause 4, keeping clause 3 ⇒ RED 2 must fail.
- **M3** — bypass the negative-delta rebaseline ⇒ RED 3 must fail.

Each: mutate production only, observe the failure, restore byte-identically (proved by hash),
re-run green. Tests are never mutated; no mutation tooling is installed.

## Gates

- **Q0** — `gofmt`, `git diff --check`, `go vet ./...`, `go build ./...`, governance
  self-test-hook + self-test; full validator on the committed clean head.
- **Q1** — `go test -race -count=1 ./internal/health/... ./internal/watcher/...`, plus the new
  regressions and the key existing controls at `-count=20`.
- **Q2** — `go mod verify`, full `go test -race -count=1 ./...`, `make lint`, plus explicit proof
  of no changes to `go.mod`/`go.sum`, DB/schema, workflows, templates/static/CSS/JS,
  `internal/watcher` production code, Twitch transport, slot broker, or cadence/jitter.
- **Q3** — `code-review` (Standards + Spec) and `ce-code-review` on the final integrated diff.

## Deferred (found, not fixed)

Recorded because they were confirmed during this task, not because they are in scope.

| Finding | Status |
| --- | --- |
| `twitchOutage` (`progress.go:473-492`) reads `Signal.Status` and never `CheckedAt`; `watch_transport` has an hours-long cadence, so a stale FAILED reading gates a 20-minute window | CONFIRMED, deferred — touching it would change PR #208 canary outage classification |
| `progress.go:706`/`:745` — `LastMinutes` is lowered unconditionally, so a `CurrentMinutesWatched` regression followed by a restore reads as progress and can fire a false `NotifyDropRecovered`. Same root cause, same function, different symptom | CONFIRMED, deferred — a separate defect, not this task's |
| `channelStability` (`internal/miner/policy.go:152-165`) shares root cause A | Deferred by contract; outside allowed paths |
| Broker liveness via `BrokerSnapshot.EvaluatedAt` | Deferred by contract |
| SPEC gate 10 documentation debt | Deferred by contract |
| SPEC gate 8 "more than" vs code `>=` | Pre-existing, cosmetic, untouched |
| `canary.go:462-468` staleness; `center.go:52-93` signal store | REFUTED as variants |
| **Chronic delivery failure is now silent.** A channel that keeps its slot while every send is rejected never confirms, so it publishes `healthy` and raises no signal. Contract Case B specifies exactly this, and no in-envelope code change can surface it (a distinct status value, a `DropProgress` field, or a new `health.Signal` all need public-surface changes). Recorded in SPEC as a Known limitation | Raised by 4 Q3 reviewers; **deferred**, needs an owner decision |
| An in-flight async recovery stage stops being reconciled while delivery is non-current (`advanceRecovery` is reachable only from the stall branch), so it exits via its bounded timeout instead of its outcome | Deferred — hoisting `resolvePending` would change async recovery correlation (A18) |
| A `Stale` send increments neither counter, so stale-only ticks read as quiet-but-healthy until the last success expires | Deferred — counting a third outcome class needs a watcher change |

## Non-goals

No new broker-liveness detector. No session/broadcast identity on `ReportStats` or `dropState`.
No threshold is lowered. No public API, JSON, schema, dependency, workflow or UI change.
