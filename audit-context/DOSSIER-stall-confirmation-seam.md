# Audit dossier — drop-stall confirmation seam

Task: `stall-confirmation-evidence` · base `6b1d80e6d014b54d476869ed736e39f126369210`,
branch `claude/drop-stall-confirmation-evidence-p1vf1l`, working tree clean.
**Understanding only — no verdicts, no fixes.**

> **Filename note.** The task asked for the index at `audit-context/DOSSIER.md`. That path is
> already occupied by the tracked policy-persistence dossier (37 KB, committed). Following the
> repo's existing precedent (`audit-context/DOSSIER-canary-outage-classification.md`), this
> index is written to `DOSSIER-stall-confirmation-seam.md` instead so no prior audit content is
> destroyed. Nothing under `audit-context/` was overwritten.

## Scope and per-function records

| Function | File | Record |
|---|---|---|
| `ProgressWatchdog.evaluate` (L516-L625) | `internal/health/progress.go` | `functions/watchdog-evaluate.md` |
| `ProgressWatchdog.observeProgress` (L668-L748) | `internal/health/progress.go` | `functions/watchdog-observe-progress.md` |
| `ProgressWatchdog.gatesHold` (L754-L799) | `internal/health/progress.go` | `functions/watchdog-gates-hold.md` |
| `dropState.resetEvidence` (L175-L180) | `internal/health/progress.go` | `functions/watchdog-reset-evidence.md` |
| `MinuteWatcher.ReportStats` (L234-L244) + `noteReportOutcome` (L607-L622) + `publishReportStats` (L624-L641) | `internal/watcher/session.go` | `functions/watcher-report-stats.md` |

Also read to ground the claims: `internal/watcher/watcher.go`, `internal/watcher/broker.go`,
`internal/health/canary.go`, `internal/health/center.go`, `internal/drops/drops.go`,
`internal/models/{drop,stream,session,campaign}.go`, `internal/miner/{miner,health}.go`,
`internal/config/config.go`, `internal/notifications/manager.go`, and the test files
`internal/health/progress_test.go`, `internal/watcher/session_test.go`.

## 1. The seam in one picture

The stall decision is a three-term conjunction at `internal/health/progress.go:572-574`:

```go
stalled := now.Sub(st.evidenceSince) >= cfg.StallDelay &&   // default 20m  (config.go:569)
    st.NoProgressObs >= cfg.StallConfirmations &&           // default 3    (config.go:570)
    st.ReportsSinceProgress >= stallMinReports              // fixed 5      (progress.go:83)
```

Each term has a different producer, a different clock, and a different freshness guarantee:

| Term | Producer | Source of truth | Read once per pass? |
|---|---|---|---|
| `evidenceSince` delay | `evaluate` L555-L563 | the pass's `now` (progress.go:386) | yes |
| `NoProgressObs` | `evaluate` L564-L570 | `drops.SyncStatus` value copy, read once at L526 | yes |
| `ReportsSinceProgress` | `observeProgress` L680-L702 | `watcher.reportStatsSnap`, re-loaded per call | **no — up to 3 loads per drop** |

And the qualitative half of the "we are demonstrably farming" proof lives in `gatesHold`, which
reads a *different* watcher atomic (`watchingLogins`, via `IsWatching`, progress.go:772) and
never touches `ReportStats` at all. Nothing joins the two atomics.

## 2. Cross-cutting question 1 — is `ReportStats(login)` a fresh immutable snapshot per call, and can two calls in ONE evaluation observe DIFFERENT snapshots?

**Per call: yes, fresh and immutable. Per pass: no consistency whatsoever.**

Publication side (`internal/watcher/session.go:627-641`):

```go
pruned := make(map[string]ReportStats, len(slots))
for _, sl := range slots { if s, ok := w.reportStats[...]; ok { pruned[...] = s } }
w.reportStats = pruned                      // L634  loop-owned, keeps mutating

snap := make(map[string]ReportStats, len(pruned))
for k, v := range pruned { snap[k] = v }    // L636-639  SEPARATE map
w.reportStatsSnap.Store(&snap)              // L640
```

`snap` is a distinct map from `pruned`, and `pruned` — not `snap` — is what
`noteReportOutcome` (session.go:613-621) subsequently mutates. So the published map is
genuinely immutable after the store. Confirmed under `-race` by
`TestBrokerLoopConcurrentWithWatchdogCalls` (session_test.go:456-520, `ReportStats` at :500).

Consumption side (`internal/watcher/session.go:237-244`): every call does its own
`w.reportStatsSnap.Load()`. There is no whole-map accessor, no pass token, no generation
counter, no seqlock.

**Two calls inside one `evaluate` pass can therefore observe different snapshots.** Three
independent widening factors, all in the base tree:

1. **Adjacent calls, no barrier.** progress.go:681 and progress.go:694 are two statements
   apart. The broker's `w.reportStatsSnap.Store` (session.go:640, reached from
   watcher.go:705 and watcher.go:778) is unsynchronised with the watchdog goroutine.
2. **Blocking I/O *between* two calls for the SAME drop.** progress.go:694 and progress.go:737
   are separated by `w.notifier.NotifyDropRecovered` (progress.go:709), which reaches
   `Manager.notifyDropTransition` → `m.repo.GetConfig()` — a synchronous SQLite read
   (notifications/manager.go:1163) — plus `dispatchPush` (manager.go:1172), before the Discord
   send is handed to `goDispatch` (manager.go:1185). Also `events.Record` (progress.go:713) and
   `w.avoid.Clear` (progress.go:717).
3. **A blocking recovery stage between two DROPS.** `evaluate` runs at most one recovery stage
   per pass (progress.go:585), but that stage can block for up to
   `recoveryStageTimeout = 60 * time.Second` (progress.go:86) — `full_resync`
   (progress.go:217-221) and `transport_probe` (progress.go:244-246). Drops iterated after it
   read counters up to a minute newer than drops iterated before it. Two campaigns farmed on
   the SAME login (both assigned to one slot's streamer, progress.go:503-507) will then observe
   different `Successes` for that login in one pass.

**Direction analysis (what an inconsistent pair actually does):**
- L681 (baseline) older than L694 (delta): the delta is slightly larger at the moment of a
  channel change. Bounded by the sends of one watcher tick.
- L681 hits, L694 misses: `ReportsSinceProgress` stays at the `0` written by L680.
- L694 hits, L737 misses (progress branch): `baselineValid = false` (progress.go:740), which
  the next pass repairs by adopting the first successful read (progress.go:695-699). This is
  the deferral pinned by `TestWatchdogRebaselineDefersUntilStatsAvailable`
  (progress_test.go:850-895).
- L694 older than L737: the new episode's baseline is the FRESHER number — conservative
  (a higher baseline means fewer counted reports).

The one direction with no guard is covered in question 3.

**Timing skew inside one watcher tick (why the watchdog nearly always reads a lagging map):**
`processWatching` publishes `brokerSnapshot` + `watchingLogins` at watcher.go:673 — *before*
any send — then runs the send loop spread over roughly one `MinuteWatchedInterval`
(default 60s, clamped 30-120s: config.go:590, 797-800) by `pace(sleepBetween)`
(watcher.go:715, 773), and only then publishes `reportStatsSnap` at watcher.go:778. The
watchdog fires on a ±20% jittered 60s timer (progress.go:78, 379-380), so a pass typically
lands *inside* a send loop and sees the new allocation with the previous tick's counters.
The early return at watcher.go:773-775 (`!w.pace(...)`, i.e. `w.ctx` done — watcher.go:600-605)
skips the L778 publish entirely; in production that is shutdown.

## 3. Cross-cutting question 2 — how many `ReportStats` calls per pass, and where?

**Per tracked drop, per pass: one, two, or three. All three sites are inside
`observeProgress`; no other function in the watchdog calls it.** Verified by grep over
`internal/` — the complete production call set is progress.go:681, 694, 737.

| # | Site | Condition | Purpose |
|---|---|---|---|
| 1 | progress.go:681 | only when `channel != st.statsChannel` (L676) | establish `baselineReports`/`baselineValid` for the new tenure |
| 2 | progress.go:694 | only when `channel != ""` (L693) | adopt a deferred baseline, then compute `ReportsSinceProgress = Successes - baselineReports` |
| 3 | progress.go:737 | only on the progress-advanced branch (L706) | re-baseline the freshly reset episode |

Resulting counts:

- steady state, same channel, no progress: **1** (site 2)
- channel changed, no progress: **2** (sites 1, 2)
- same channel, progress advanced: **2** (sites 2, 3)
- channel changed *and* progress advanced: **3** (sites 1, 2, 3)
- `channel == ""` and unchanged: **0**

Per PASS the total is the sum over every tracked drop, so one login can be queried 3×N times in
one pass when N campaigns are farmed on it.

Notably **`gatesHold` does not call `ReportStats` at all** (progress.go:754-799). Its
"demonstrably watching" gate is `w.watch.IsWatching(st.Channel)` (progress.go:772), a load of
`watchingLogins` (broker.go:411-417) — a *different* atomic published at a *different* point of
the watcher tick from the counters the threshold at progress.go:574 uses. `advanceRecovery`,
`resolvePending`, and the recovery stages never call it either.

## 4. Cross-cutting question 3 — what resets `ReportStats` counters, and does a same-login broadcast/session change reset them?

**Complete write set for `w.reportStats`** (grep over `internal/`): session.go:611, 621, 634,
640 and watcher.go:549. Nothing else touches it.

**Two reset paths, both login-keyed, neither observable to a consumer:**

1. **`publishReportStats` prune** — session.go:628-634. A login not in this tick's `slots`
   loses its entry entirely from `w.reportStats`. On re-slotting, `noteReportOutcome`'s
   `s := w.reportStats[login]` (session.go:613) reads the zero value, so `Successes` restarts
   at 1. Also reached with an EMPTY `slots` on the no-slots path (watcher.go:702-707), which
   clears every login. Documented as intentional at session.go:38-42.
2. **`applyStreamerList` roster removal** — watcher.go:542-553, `delete(w.reportStats, login)`
   at :549. Loop goroutine, tick start (via `applyPendingSettings`, watcher.go:644). Doc at
   watcher.go:533-541 (BKM-018A: "a same-process re-add of the login starts clean").

**A same-login broadcast/session change does NOT reset them.** The map is keyed by login only;
`ReportStats` carries no broadcast id, session generation, or tenure marker (session.go:43-48).
`executeSessionRefreshes` (watcher.go:694), the `RefreshStreamInfo`/`RefreshSession` modes
(session.go:54-61), and `Stream.SessionGeneration()` (models/session.go:78-82) leave
`reportStats` untouched. A channel that changes broadcast while keeping its slot keeps
accumulating into one counter, and the watchdog never re-baselines. Conversely a channel that
loses its slot for one tick and returns on the SAME broadcast DOES get reset.

**The unenforced assumption this creates.** `observeProgress` computes the delta as

```go
// progress.go:700-702
if n := stats.Successes - st.baselineReports; n >= 0 {
    st.ReportsSinceProgress = n
}
```

The `n >= 0` guard treats a decrease as "ignore this read", not as "the tenure restarted".
Trace of the reachable sequence:

1. Login X holds a slot; `Successes` reaches 20; the watchdog observes it, so
   `st.statsChannel = X`, `st.baselineReports = 20`, `st.ReportsSinceProgress = 7`.
2. X drops out of `slots` for one watcher tick (rotation, displacement, a failed
   `CheckStreamerOnline` at watcher.go:746-748). `publishReportStats` prunes X.
3. X regains its slot on the next tick. `Successes` restarts at 1.
4. **The watchdog never ran a pass inside the gap.** Its cadence is 48-72s (progress.go:78,
   379-380) and a pass can itself block up to 60s in a recovery stage
   (progress.go:86, 217-221, 244-246), so missing a one-tick gap is ordinary, not exotic.
5. Next pass: `farmingChannel` returns X, so `channel == st.statsChannel` — the re-baseline
   branch at progress.go:676 is skipped. Site 2 reads `Successes = 1`; `1 - 20 = -19` is
   negative; the guard suppresses the write; `st.ReportsSinceProgress` **keeps 7**.
6. `gatesHold` cannot see the gap either: `IsWatching(X)` (progress.go:772) is true again, and
   `st.evidenceSince` was never reset because no gate ever failed within a pass.

Net: the `>= stallMinReports` term can be satisfied by deliveries from a *previous* tenure,
and the evidence window can silently span a slot gap it was explicitly designed to break
(`dropState` doc, progress.go:145-152).

**What establishes the monotonicity assumption: nothing found.**
- No production code re-baselines on a counter decrease.
- `ReportStats` publishes no tenure id the watchdog could compare.
- The test fake never decreases: `fakeWatchView.addSuccesses` only adds
  (progress_test.go:180-187) and `fakeWatchView.ReportStats` never removes a login
  (progress_test.go:92-100).
- `TestReportStatsAccountingAndPruning` (session_test.go:297-323) asserts the prune makes
  `ok == false`, but never re-slots the pruned login to assert the restart-at-zero.
- No integration test drives a real `*watcher.MinuteWatcher` through a real
  `*health.ProgressWatchdog`: `NewProgressWatchdog` appears in tests only at
  `internal/health/progress_test.go`, `internal/miner/health_test.go`,
  `internal/miner/health_persistence_test.go`, all against fakes.

## 5. Cross-cutting question 4 — what does `resetEvidence()` destroy, and who calls it?

```go
// internal/health/progress.go:175-180
func (st *dropState) resetEvidence() {
	st.evidenceSince = time.Time{}
	st.NoProgressObs = 0
	st.ReportsSinceProgress = 0
	st.statsChannel = "" // force a delivery re-baseline even for the same channel
}
```

**Destroys** exactly the three stall terms plus the stats-channel binding. The fourth line is
the load-bearing one: emptying `statsChannel` forces `observeProgress`'s
`channel != st.statsChannel` branch (progress.go:676) on the next pass even for an unchanged
channel, which re-reads `ReportStats` and re-establishes `baselineReports`/`baselineValid`
(progress.go:679-689).

**Preserves** `RecoveryStage`, `RecoveryStageName`, `LastRecoveryAt`, `exhaustedAt`,
`notifiedStalled`, `avoidedChannel`, `Status`, `Detail`, `LastMinutes`, `LastProgressAt`,
`baselineReports`, `baselineValid`, `lastObservedSyncAt`, `Channel`, and `pending`.
The comment at progress.go:172-174 states the intent for the recovery/notification fields
("a transient gate blip must not restart the pipeline, only pause it").
`baselineReports`/`baselineValid` surviving is neutralised by `statsChannel = ""`;
`lastObservedSyncAt` surviving is neutralised by the re-seed at progress.go:562.
`st.pending` surviving is **not** neutralised — see below.

**Callers (complete, grep-verified over `internal/`):**

| # | Site | Trigger | Post-condition set by the caller |
|---|---|---|---|
| 1 | progress.go:547 | any `gatesHold` failure in `evaluate` | `Status` → Healthy unless already Stalled; `Detail` = the gate reason (L548-L551); then `continue` (L552) |
| 2 | progress.go:891 | `resolvePending` sees `outcome.Skipped` (slot lost while a stage was parked) | `RecoveryStage` rolled back to `stageIndex` (L890); `Status` → Recovering (L892) |
| 3 | progress.go:924 | `rebaselineEpisode`, reached from `resolvePending`'s `outcome.Stale` arm (L882) | stage/exhaustion/cooldown cleared (L920-L923); `Status` → Recovering (L925) |

There is **no caller for the case that motivates it most directly** — a same-login watcher
counter reset (question 3). All three triggers are watchdog-visible state changes; the counter
reset is not.

**The `pending` interaction.** At call site 1, `evaluate` `continue`s at progress.go:552, so
`advanceRecovery` (L585) and hence `resolvePending` (L937-L939) are unreachable for that drop.
Meanwhile the parked stage's `deadline` (`now.Add(recoveryOutcomeDeadline)`, progress.go:843;
`recoveryOutcomeDeadline = 5 * time.Minute`, progress.go:94) keeps running in wall-clock. A
gate outage longer than five minutes therefore guarantees the first passing pass resolves the
stage as a TIMEOUT (progress.go:862-868) regardless of whether the broker executed it. Nothing
pauses or extends the deadline across a gate failure — **nothing found**.

## 6. Unenforced assumptions, consolidated

Ranked by how directly they bear on the stall-confirmation decision.

1. **`ReportStats(login).Successes` is non-decreasing while `st.statsChannel` is unchanged.**
   (progress.go:700) Broken by `publishReportStats`'s prune-and-restart
   (session.go:628-634 + 613). Nothing found: no tenure id, no test, no re-baseline path.
2. **The two halves of the "demonstrably farming" proof describe one instant.**
   `gatesHold`'s `IsWatching` (progress.go:772 → `watchingLogins`, published watcher.go:673)
   and the `stallMinReports` term (`reportStatsSnap`, published watcher.go:778) are separate
   atomics published at opposite ends of a 30-120s watcher tick. Nothing found joining them.
3. **All `ReportStats` reads in one pass come from one snapshot.** They do not
   (§2). Nothing found — no whole-map accessor, no generation.
4. **`sync` (progress.go:526) and the campaign pool (progress.go:532) describe the same backend
   snapshot.** Two separate `RLock` reads of the drops tracker. `SyncStatus.Revision` exists
   precisely to prove two reads agree (drops.go:63-67) and `publishProgressObservation` makes
   campaigns+stamp atomic on the writer side (drops.go:691-707), but the reader ignores it.
   Consequence: `lastObservedSyncAt` can be seeded to a pre-progress stamp (progress.go:735) and
   the next pass counts the observation that showed progress as a no-progress observation
   (progress.go:564-569). Nothing found.
5. **Health signals feeding `twitchOutage` are fresh.** `Center` has no TTL: `Record`
   (center.go:113-118) replaces a signal and it persists indefinitely; nothing reads
   `CheckedAt`. A stale FAILED `watch_transport` blocks stall confirmation forever.
   Nothing found.
6. **`SignalWatchTransport` says something about the farming channel.** It is recorded only by
   `Canary.probe` against the single configured `cfg.Channel` (canary.go:200-222, 279/289/300).
   `inconclusiveWatchTransport` (progress.go:453-461) already discards canary-local /
   channel-local / inconclusive provenance and the coupling is documented as deliberate
   (progress.go:463-472); recorded here as a cross-channel dependency, not a defect.
7. **`w.drops` / `w.watch` are non-nil.** Unguarded dereferences at progress.go:526 and 498/670.
   Established only by the single production wiring (miner.go:1058-1067). Nothing found.
8. **A gate failure does not consume the recovery-outcome deadline.** It does (§5).
   Nothing found.
9. **Disabling the watchdog is state-neutral.** `evaluate` wipes `w.states` wholesale
   (progress.go:519-521), destroying `notifiedStalled`, `avoidedChannel`, and `exhaustedAt`:
   a disable→enable cycle re-arms the critical alert and orphans avoid entries that the
   L609-L611 cleanup would have cleared. Nothing found.
10. **`res.Stale` sends being counted as neither success nor failure**
    (watcher.go:730-738) is invisible to the watchdog: a permanently-stale session and an idle
    one both present a flat counter. Documented as deliberate at watcher.go:732-736;
    recorded as an observability gap, not a defect.

## 7. Doc/code discrepancies noticed (no verdicts)

- `evaluate`'s doc says the pass runs "the next recovery stage of the **worst-off** drop"
  (progress.go:513-515); the code runs it for the FIRST stalled drop in the campaign-slice
  order returned by `Campaigns()` (progress.go:532, 585).
- `ReportStats` exposes `Failures`, `LastSuccess`, `LastFailure` (session.go:44-47); the stall
  logic reads none of them — only `Successes` (progress.go:682, 698, 700, 738).
- `stallMinReports` is a package constant (progress.go:83) while its two sibling thresholds are
  runtime-configurable and clamped (config.go:881-889).

## 8. Open questions carried forward

1. Should a decrease in `Successes` for an unchanged `statsChannel` force a re-baseline (and an
   evidence reset)? Equivalently: should `ReportStats` publish a tenure/epoch id?
2. Should one `evaluate` pass read `ReportStats` once per channel instead of up to three times
   per drop?
3. Should `gatesHold`'s "demonstrably watching" gate and the `stallMinReports` term be derived
   from one atomic read, given they are published at opposite ends of a watcher tick?
4. Should the `sync`/`Campaigns()` pair be joined by `SyncStatus.Revision`?
5. Should the outage gate ignore a `watch_transport` signal older than one canary interval?
6. Should `resetEvidence` clear or freeze `st.pending`?
7. Is dropping an in-flight `st.pending` on the progress branch (progress.go:719) intended to
   leave a broker outcome unconsumed?
8. Is the `!cfg.Enabled` wipe of `notifiedStalled` / `avoidedChannel` (progress.go:519-521)
   intended?
9. Is a DECREASE in `drop.CurrentMinutesWatched` meant to be a silent no-op
   (progress.go:706 false → 745)?
10. Is there any test that re-slots a pruned login and asserts the restart-at-zero? Not found in
    `internal/watcher/*_test.go`.
