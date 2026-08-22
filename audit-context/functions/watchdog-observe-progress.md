# `ProgressWatchdog.observeProgress` — internal/health/progress.go (base 6b1d80e)

Part of the `stall-confirmation-seam` audit context. Index:
`audit-context/DOSSIER-stall-confirmation-seam.md`.

---

## `observeProgress` in internal/health/progress.go (L668-L748)

**Purpose:** The sole writer of `st.Channel`, `st.statsChannel`, `st.baselineReports`,
`st.baselineValid` and (outside `resetEvidence`) of `st.ReportsSinceProgress`. It converts the
watcher's per-channel lifetime success counter into "successful minute-watched deliveries to
the farming channel since the last observed drop progress" — the third term of the stall
conjunction (L574). It also owns the healthy-reset path when minutes advance. Without it the
`>= stallMinReports` gate has no input and the recovered notification never fires.

**Inputs & Assumptions:**
- `st *dropState` (trusted, loop-owned) — mutated in place, and at L719-L736 wholesale
  REPLACED via `*st = dropState{...}`.
- `campaign *models.Campaign` (semi-trusted: derived from Twitch data, but immutable once
  published — drops.go:927-930). Used only for `farmingChannel`.
- `drop *models.Drop` (same trust) — `drop.CurrentMinutesWatched` is the progress signal.
- `sync drops.SyncStatus` (trusted value copy, read once per pass at L526) — used only to seed
  `lastObservedSyncAt` at L735.
- `now time.Time` (trusted, pass-constant from L386).
- Implicit: `w.watch` is non-nil (L670, L681, L694, L737 dereference it unguarded).
- Implicit: `w.watch.ReportStats(login)` returns a snapshot that is monotone non-decreasing in
  `Successes` for as long as `st.statsChannel` is unchanged. **Nothing found** — see the
  L700 block below. `publishReportStats` (watcher/session.go:627-634) actively violates it.
- Precondition: `st.statsChannel` and `st.baselineReports` describe the same watcher tenure.
  Established only by the L676 channel-change re-baseline and by `resetEvidence`'s
  `st.statsChannel = ""` (L179), whose comment (L179) states the purpose exactly: "force a
  delivery re-baseline even for the same channel".

**Outputs & Effects:**
- Writes `st.statsChannel`, `st.ReportsSinceProgress`, `st.baselineReports`, `st.baselineValid`,
  `st.Channel`, `st.LastMinutes`.
- On progress: calls `w.notifier.NotifyDropRecovered` (L709 — synchronous SQLite read via
  `Manager.notifyDropTransition` → `m.repo.GetConfig()`, notifications/manager.go:1163, plus
  `dispatchPush`; only the Discord send is async via `goDispatch`, manager.go:1185),
  `events.Record` (L713), `w.avoid.Clear` (L717), then REPLACES `*st` (L719-L736).
- No return value. No error path.

**Block-by-Block:**

```go
// L669-L675
channel := w.farmingChannel(campaign)
if channel == "" && st.Channel != "" && w.watch.IsWatching(st.Channel) {
    channel = st.Channel
}
```
- **What:** Resolves which slotted login is farming this campaign, with a fallback that keeps
  the previous channel when it still holds a slot but lost the campaign assignment.
- **Why here:** `st.Channel` must be current before `gatesHold` (L769-L774) reads it, so the
  gate can name the eligibility loss precisely (comment L671-L673).
- **Assumes:** `farmingChannel` (L497-L510) and `IsWatching` (broker.go:411-417) agree.
  They read TWO different atomics — `brokerSnapshot` and `watchingLogins` — both stored in
  `publishBrokerSnapshot` (broker.go:383-384). Two separate `atomic.Pointer` stores, so a
  reader can straddle them. Same tick in practice because both stores are adjacent, but there
  is no single-load guarantee. Recorded, no verdict.
- **Assumes:** `w.resolveStreamer` inside `farmingChannel` (L499) resolves every slotted login.
  Production impl `Miner.resolveStreamer` (miner/health.go:42-52) falls back to
  `discovery.StreamerFor`; a slot whose streamer resolves to nil is silently skipped (L500-L502).
- **Establishes:** `channel` — "" or the farming login for this pass.

```go
// L676-L690
if channel != st.statsChannel {
    st.statsChannel = channel
    st.ReportsSinceProgress = 0
    if stats, ok := w.watch.ReportStats(channel); ok {
        st.baselineReports, st.baselineValid = stats.Successes, true
    } else {
        st.baselineReports, st.baselineValid = 0, false
    }
}
```
- **What:** Channel change ⇒ re-baseline the delivery accounting. **ReportStats call site #1.**
- **Why here:** Before the delta computation at L700, so a rotation cannot leak the new
  channel's pre-existing counter into the old episode's evidence.
- **Assumes:** a miss (`ok == false`) means "not published yet", not "published as zero".
  Established: `ReportStats` returns `ok == false` only when the login is absent from the
  published map (session.go:242-243), and `publishReportStats` inserts a login only if it is
  in the tick's slots AND already has a counter (session.go:629-633). The `baselineValid`
  deferral is exactly the fix pinned by `TestWatchdogRebaselineDefersUntilStatsAvailable`
  (progress_test.go:850-895).
- **Assumes:** `ReportStats("")` misses. Not asserted anywhere, but the published map is keyed
  by `streamer.GetUsername()` (session.go:630-631), which is never "".
- **Establishes:** `statsChannel == channel`; either a valid baseline or an explicit deferral.
- **Depended on by:** L694-L702.

```go
// L693-L704
if channel != "" {
    if stats, ok := w.watch.ReportStats(channel); ok {
        if !st.baselineValid {
            st.baselineReports, st.baselineValid = stats.Successes, true
        }
        if n := stats.Successes - st.baselineReports; n >= 0 {
            st.ReportsSinceProgress = n
        }
    }
}
```
- **What:** Adopts a deferred baseline, then recomputes the delta. **ReportStats call site #2** —
  a SECOND, independent `reportStatsSnap.Load()` (session.go:238), not a reuse of #1.
- **Why here:** After the possible re-baseline, so `st.baselineReports` is current.
- **Assumes (load-bearing, unenforced):** `stats.Successes >= st.baselineReports` whenever the
  channel is unchanged — i.e. the guard's negative case is only ever "a stale read after a
  re-baseline". `publishReportStats` breaks this: it PRUNES `w.reportStats` to the tick's slots
  (session.go:628-634), so a login that leaves the allocation for one watcher tick loses its
  entry entirely, and on re-slotting `noteReportOutcome` restarts from the zero value
  (session.go:613). If that loss+regain happens entirely BETWEEN two watchdog passes, this
  function never sees `channel != st.statsChannel`, never re-baselines, and `n` goes negative —
  the guard suppresses the write and `st.ReportsSinceProgress` retains a value earned during the
  PREVIOUS tenure. `gatesHold` cannot catch it either: `IsWatching(st.Channel)` (L772) is true
  again by then. **Nothing found** — no production code re-baselines on a counter decrease, and
  the test fake only ever adds (`fakeWatchView.addSuccesses`, progress_test.go:180-187;
  `ReportStats` at progress_test.go:92-100 never removes a login).
- **Assumes:** call sites #1 and #2 read the same snapshot. They do not — see dossier
  question 1. Direction matters: #2 newer than #1 means a slightly larger delta at the moment
  of a channel change; #2 missing after #1 hit leaves `ReportsSinceProgress` at the 0 set by
  L680.
- **Establishes:** `st.ReportsSinceProgress` — the L574 term.

```go
// L706-L718
if drop.CurrentMinutesWatched > st.LastMinutes {
    recovered := st.notifiedStalled
    if recovered && w.notifier != nil { w.notifier.NotifyDropRecovered(...) }
    if st.Status == ProgressStalled || st.RecoveryStage > 0 { events.Record(...) }
    if st.avoidedChannel != "" && w.avoid != nil { w.avoid.Clear(st.avoidedChannel) }
```
- **What:** Real progress ⇒ close the episode, notify, clear the avoid entry.
- **Why here:** Progress is authoritative; nothing below should run on the stall path.
- **Assumes:** `NotifyDropRecovered` is cheap enough to run on the watchdog goroutine. It is
  synchronous SQLite (`m.repo.GetConfig()`, manager.go:1163) plus `dispatchPush`
  (manager.go:1172) before the async Discord dispatch. This I/O sits BETWEEN ReportStats call
  site #2 and call site #3, so those two reads are separated by unbounded database latency.
- **Assumes:** `drop.CurrentMinutesWatched > st.LastMinutes` means Twitch credited minutes.
  Established: the field is only written by `Drop.Update` from `selfData["currentMinutesWatched"]`
  (models/drop.go:139-141), and the published pool is rebuilt by `syncProgress` from a fresh
  inventory read (drops.go:927-949). A drop whose minutes DECREASE (Twitch correction, or a
  campaign clone with a lower value) takes the else-path and simply lowers `st.LastMinutes`
  at L745 — no re-baseline of reports, no evidence reset. Recorded, no verdict.

```go
// L719-L741
*st = dropState{DropProgress: DropProgress{... LastProgressAt: now, Status: ProgressHealthy ...},
    statsChannel: channel,
    lastObservedSyncAt: sync.ProgressLastSyncAt,
}
if stats, ok := w.watch.ReportStats(channel); ok {
    st.baselineReports, st.baselineValid = stats.Successes, true
} else { st.baselineValid = false }
return
```
- **What:** Wholesale episode reset. **ReportStats call site #3** — a third independent
  `Load()`.
- **Why here:** After the notifications, so the OLD `st.notifiedStalled`/`avoidedChannel` were
  still readable above.
- **Assumes:** wiping the whole struct is intended — it also clears `st.pending` (L167),
  `st.exhaustedAt`, `st.RecoveryStage`, `st.LastRecoveryAt`, `st.notifiedStalled`,
  `st.avoidedChannel`, `st.evidenceSince`. Consistent with the doc at L665-L667 and the
  comment at L731-L735. Note: an in-flight `pending` correlation is dropped silently; a
  matching broker outcome arriving later is simply never consumed. Recorded, no verdict.
- **Assumes:** `sync.ProgressLastSyncAt` is the stamp of the observation that SHOWED this
  progress. When a light sync publishes between L526 and L532 it is the previous stamp instead
  — see `watchdog-evaluate.md`, L532-L538 block. **Nothing found** joining the two reads.
- **Establishes:** a clean episode with `evidenceSince` zero, so `evaluate` L555-L563 opens a
  fresh window on the next pass.

```go
// L745-L747
st.LastMinutes = drop.CurrentMinutesWatched
```
- **What:** The no-progress path's only write.
- **Establishes:** `LastMinutes` tracks the last observed value, so the next pass's `>`
  comparison is against the freshest number.

**Cross-Function Dependencies:**
- Callee `w.farmingChannel` (internal, L497-L510): needs the slot→campaign assignment. Reads
  `w.watch.BrokerSnapshot()` (broker.go:389-394 — returns `*snap`, a shallow copy of a
  never-mutated published struct) and `w.resolveStreamer` → `streamer.Stream.GetCampaigns()`
  (models/stream.go:269-273, under the Stream RLock).
- Callee `w.watch.IsWatching` (internal, `*watcher.MinuteWatcher`, broker.go:411-417): needs
  "this login holds a slot right now". Reads `watchingLogins`.
- Callee `w.watch.ReportStats` (internal, watcher/session.go:237-244): needs the current
  tenure's success count. Called up to THREE times per drop per pass (L681, L694, L737), each
  an independent atomic load. See `watcher-report-stats.md`.
- Callee `w.notifier.NotifyDropRecovered` (internal, adapter at miner/health.go:96-100 →
  notifications/manager.go:1142-1152): needs nothing back; performs blocking SQLite.
- Callee `events.Record` (internal, events/events.go:172-174).
- Callee `w.avoid.Clear` (internal, `*health.AvoidList`).
- Callers: `evaluate` (L538) only.
- Shared state: `st` (also written by `evaluate` L547-L581, `resetEvidence` L175-L180,
  `resolvePending` L876-L909, `rebaselineEpisode` L918-L926, `advanceRecovery` L947-L967).
- Invariant couplings: this function is the ONLY producer of the third stall term. Its
  correctness rests entirely on the watcher's counter semantics, which are owned by a different
  package and a different goroutine and carry no generation/epoch that the watchdog could check.

**Open Questions:**
- Should a DECREASE in `stats.Successes` for an unchanged `statsChannel` force a re-baseline
  (treating it as a tenure change) rather than silently keeping the stale
  `ReportsSinceProgress`? Nothing in `internal/watcher` publishes a tenure id the watchdog
  could compare instead.
- Should the three `ReportStats` calls in one pass be collapsed to one read, so a drop's
  baseline and delta provably come from one snapshot?
- Is dropping an in-flight `st.pending` at L719 (progress arrived while a stage was parked)
  intended to leave the broker outcome unconsumed?
- Is a DECREASE in `drop.CurrentMinutesWatched` (L706 false, L745 lowers the mark) meant to be
  a silent no-op?
