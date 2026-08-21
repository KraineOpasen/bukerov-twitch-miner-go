# `MinuteWatcher.ReportStats` / `noteReportOutcome` / `publishReportStats` — internal/watcher/session.go (base 6b1d80e)

Part of the `stall-confirmation-seam` audit context. Index:
`audit-context/DOSSIER-stall-confirmation-seam.md`. These three functions are the complete
producer/consumer surface of the per-channel minute-watched delivery accounting that
`health.ProgressWatchdog` uses as its "we are demonstrably farming" evidence.

Type under discussion (`internal/watcher/session.go:43-48`):

```go
type ReportStats struct {
	Successes   int       `json:"successes"`
	Failures    int       `json:"failures"`
	LastSuccess time.Time `json:"lastSuccess,omitzero"`
	LastFailure time.Time `json:"lastFailure,omitzero"`
}
```

The watchdog reads **only** `Successes` (progress.go:682, 698, 700, 738). `Failures`,
`LastSuccess`, `LastFailure` are produced but never consumed by the stall logic — verified by
grep over `internal/`: the only non-test readers of the struct are those four progress.go
sites.

---

## `ReportStats` in internal/watcher/session.go (L234-L244)

**Purpose:** The watchdog's lock-free read of a slotted channel's delivery counters. It is the
only exported accessor; `WatchView.ReportStats` (progress.go:36) is exactly this method.

**Inputs & Assumptions:**
- `login string` — semi-trusted (comes from `BrokerSnapshot().Slots[i].Channel`, which the
  broker itself filled from `streamer.GetUsername()`, broker.go:372). `""` is a legal argument
  and always misses, because published keys are usernames (session.go:630-631).
- Implicit: `w.reportStatsSnap` is an `atomic.Pointer[map[string]ReportStats]`
  (watcher.go:171). Nil until the first `publishReportStats`.
- Precondition: none. Safe before `Start`, safe from any goroutine.

**Outputs & Effects:**
- Returns `(ReportStats, bool)` by value. `ok == false` means the login is absent from the
  published map — i.e. it did not hold a slot at the last publish, or held one but had no
  counter entry yet (session.go:630 requires both).
- No writes, no locks.

**Block-by-Block:**

```go
// L237-L244
func (w *MinuteWatcher) ReportStats(login string) (ReportStats, bool) {
	m := w.reportStatsSnap.Load()
	if m == nil { return ReportStats{}, false }
	s, ok := (*m)[login]
	return s, ok
}
```
- **What:** One atomic load, one map lookup, one struct copy out.
- **Why here:** Lock-free by construction so the watchdog goroutine never contends with the
  broker loop (file doc, session.go:14-20: "external goroutines only stage requests or read
  atomically-published snapshots").
- **Assumes:** the pointed-to map is never mutated after publication. Established:
  `publishReportStats` builds `snap` as a SEPARATE map from `pruned` and stores it
  (session.go:636-640); `pruned` — not `snap` — becomes `w.reportStats`, the loop-owned map
  that `noteReportOutcome` mutates. So the published map is genuinely immutable. Also pinned
  under `-race` by `TestBrokerLoopConcurrentWithWatchdogCalls`
  (session_test.go:456-520, `_, _ = w.ReportStats(login)` at :500 against a live `w.loop()`).
- **Establishes:** each CALL returns a self-consistent snapshot of one login's counters.
- **Does NOT establish:** that two calls return the same snapshot. There is no pass token, no
  generation, no "read the whole map once" accessor. Every call re-`Load()`s. See the dossier's
  cross-cutting question 1.
- **Depended on by:** progress.go:681, 694, 737 — three independent calls per drop per pass.

**Cross-Function Dependencies:**
- Callee: none.
- Callers: `health.ProgressWatchdog.observeProgress` (progress.go:681, 694, 737) is the only
  production caller. Tests: session_test.go:240, 252, 309, 318, 321, 500;
  session_converge_test.go:618.
- Shared state: `reportStatsSnap` (written only by `publishReportStats`, session.go:640).
- Invariant couplings: `ok == false` is overloaded. It means "not slotted at the last publish"
  AND "slotted but no counter yet". `observeProgress` treats both as "defer the baseline"
  (progress.go:683-689), which is the safe reading for both.

**Open Questions:**
- Should there be a whole-map accessor (or a published generation/tenure id) so one watchdog
  pass can prove all its reads came from one snapshot?

---

## `noteReportOutcome` in internal/watcher/session.go (L607-L622)

**Purpose:** The only writer of the delivery counters. Turns one minute-watched send attempt
into `Successes+LastSuccess` or `Failures+LastFailure`.

**Inputs & Assumptions:**
- `login string` (semi-trusted, always `streamer.GetUsername()` at both call sites).
- `ok bool` — the classification. Trusted; produced by `processWatching`'s switch.
- `now time.Time` — the caller passes `time.Now()` directly (watcher.go:740, 752), NOT the
  tick's `now` from watcher.go:648.
- Implicit: runs on the loop goroutine only. Doc comment L608. Established: both call sites are
  inside `processWatching` (watcher.go:740, 752), which `loop` calls (watcher.go:617).
- Precondition: no lock is held or needed. `w.reportStats` is documented loop-owned
  (watcher.go:167-171).

**Outputs & Effects:**
- Lazily creates `w.reportStats` (L610-L612) and writes back the updated struct (L621).
- Nothing is published here — the counters remain invisible to `ReportStats` until
  `publishReportStats` runs at the END of the tick (watcher.go:778).

**Block-by-Block:**

```go
// L613-L621
s := w.reportStats[login]
if ok { s.Successes++; s.LastSuccess = now } else { s.Failures++; s.LastFailure = now }
w.reportStats[login] = s
```
- **What:** Read-modify-write of the login's counter.
- **Assumes:** a missing key means "start from zero". Correct Go, and it is exactly how a
  re-slotted channel restarts after `publishReportStats` pruned it — the mechanism behind the
  counter reset described in `watchdog-observe-progress.md`.
- **Establishes:** `Successes` counts only sends the loop classified as delivered.

**Complete call-site taxonomy (watcher.go:728-753) — what is and is NOT counted:**

| Send outcome | Line | Effect on `Successes` |
|---|---|---|
| `res.Stale` (session changed mid-send) | watcher.go:730-738 | **neither** counter moves — comment L732-L736: "No minute was delivered, but this is NOT a transport failure ... do not note a failure" |
| `res.Failure != nil` | watcher.go:739-748 | `Failures++`, `LastFailure` |
| default (delivered) | watcher.go:749-753 | `Successes++`, `LastSuccess` |

- The stale gap is load-bearing for the watchdog: a channel whose every send is stale-skipped
  accrues NO successes, so `ReportsSinceProgress` stays flat and the `>= stallMinReports` term
  correctly blocks confirmation. It also means the watchdog cannot distinguish "no sends
  attempted" from "all sends stale" — both look like a flat counter.
- `res.Failure != nil` also triggers `w.client.CheckStreamerOnline(streamer)` (watcher.go:746-748),
  a synchronous Twitch call inside the send loop.

**Cross-Function Dependencies:**
- Callers: `processWatching` only (watcher.go:740, 752). Tests call it directly
  (session_test.go:304-307, 315).
- Callee: none.
- Shared state: `w.reportStats` — also pruned/replaced by `publishReportStats` (session.go:634)
  and deleted per-login by `applyStreamerList` (watcher.go:549).
- Invariant couplings: `Successes` is a per-tenure counter, not a lifetime one. Nothing in the
  struct records which tenure it belongs to.

**Open Questions:**
- Is `res.Stale` counting as neither outcome intended to be invisible to the watchdog, or
  should stale sends surface as a distinct counter so a permanently-stale session is
  distinguishable from an idle one?

---

## `publishReportStats` in internal/watcher/session.go (L624-L641)

**Purpose:** Prunes the loop-owned counters to the channels still holding a slot and publishes
an immutable copy. This is BOTH the publication point and the reset mechanism — the two are
the same function, which is why "what resets the counters" has a non-obvious answer.

**Inputs & Assumptions:**
- `slots []slotOccupant` — trusted; the tick's committed allocation, produced by
  `w.arbitrate` (watcher.go:667).
- Implicit: loop goroutine only (doc L625-L626). Established: both call sites are in
  `processWatching` (watcher.go:705, 778).
- Precondition: `sl.streamer` is non-nil for every slot. Nothing in this function checks it;
  established upstream by `arbitrate` building occupants from resolved streamers.

**Outputs & Effects:**

```go
// L627-L641
func (w *MinuteWatcher) publishReportStats(slots []slotOccupant) {
	pruned := make(map[string]ReportStats, len(slots))
	for _, sl := range slots {
		if s, ok := w.reportStats[sl.streamer.GetUsername()]; ok {
			pruned[sl.streamer.GetUsername()] = s
		}
	}
	w.reportStats = pruned

	snap := make(map[string]ReportStats, len(pruned))
	for k, v := range pruned { snap[k] = v }
	w.reportStatsSnap.Store(&snap)
}
```
- **What:** L628-L634 REPLACES the loop-owned map with one containing only currently-slotted
  logins; L636-L640 publishes an independent copy.
- **Why here (in the tick):** called at watcher.go:778, AFTER the whole per-slot send loop, so
  one publish carries the whole tick's deliveries. Also called early at watcher.go:705 on the
  "no slots granted" path, where `slots` is empty and the publish therefore clears EVERYTHING.
- **Assumes:** a login absent from `slots` has no evidence worth keeping. Documented as
  intentional at session.go:38-42 ("counters ... reset when it leaves the allocation
  (rotation, displacement, offline) — the progress watchdog only reasons about channels that
  are actively watched, so history beyond the current tenure is intentionally not kept").
- **Establishes:** `reportStatsSnap` contains exactly the tick's slotted logins that had a
  counter; the published map is never mutated afterwards.
- **CONSEQUENCE (the reset):** a login dropped from `slots` for ONE tick loses its entry from
  `w.reportStats`. On re-slotting, `noteReportOutcome` restarts it at zero (session.go:613).
  This is the ONLY reset path other than roster removal, and it is invisible to any consumer:
  the published struct carries no tenure id, no epoch, and no "reset at" timestamp.
  `LastSuccess` also resets to the zero time, but no consumer reads it.
- **Pinned by:** `TestReportStatsAccountingAndPruning` (session_test.go:297-323) — asserts
  accumulation across ticks for a retained login and `ok == false` for a pruned one. It does
  NOT exercise re-slotting after a prune, so the restart-at-zero behavior is implied by the
  code (session.go:613 zero-value read) rather than asserted. **Nothing found** pinning it.

**Publication-order note (why the watchdog sees a skewed picture):** within one
`processWatching` tick,
- watcher.go:673 `publishBrokerSnapshot(slots, waiting, now)` → `brokerSnapshot` and
  `watchingLogins` are stored (broker.go:383-384) BEFORE any send;
- watcher.go:694 `executeSessionRefreshes(slots)` → `refreshOutcomes` published;
- watcher.go:725-776 the send loop runs, spread over roughly one
  `MinuteWatchedInterval` (default 60s, clamped 30-120s: config.go:590, 797-800) by
  `pace(sleepBetween)` (watcher.go:715, 773);
- watcher.go:778 `publishReportStats(slots)` → `reportStatsSnap` stored LAST.

So `IsWatching`/`BrokerSnapshot` LEAD the delivery counters by up to a full tick. A watchdog
pass landing inside a send loop sees the new allocation with the previous tick's counters.

**Early-return note:** `if !w.pace(sleepBetween) { return }` at watcher.go:773-775 skips the
L778 publish entirely. `pace` returns false only when `w.ctx` is done (watcher.go:600-605) or
an injected test `pacer` says so, so in production this is the shutdown path and the last
published snapshot simply persists.

**Cross-Function Dependencies:**
- Callers: `processWatching` at watcher.go:705 (empty-slots path) and watcher.go:778
  (normal path). Tests: session_test.go:307, 316.
- Callee: `sl.streamer.GetUsername()` (models accessor, under the streamer lock).
- Shared state: `w.reportStats` (written here and in `noteReportOutcome`; per-login deletes in
  `applyStreamerList` watcher.go:549), `w.reportStatsSnap` (written only here).
- Invariant couplings: this function decides both WHAT is visible and WHEN. It is the single
  point where the watchdog's third stall term can silently lose its history.

**What else can reset or remove a login's counters (complete):**
1. `publishReportStats` prune — login not in this tick's `slots` (session.go:628-634). Includes
   the empty-slots path at watcher.go:705.
2. `applyStreamerList` — login removed from the configured roster
   (watcher.go:542-553, `delete(w.reportStats, login)` at :549). Runs on the loop goroutine at
   tick start via `applyPendingSettings` (watcher.go:644); the doc at watcher.go:533-541 calls
   this a deliberate BKM-018A cleanup so "a same-process re-add of the login starts clean".
   Note it deletes from `w.reportStats` but not from the published snapshot — the next
   `publishReportStats` in the same tick reconciles that.
3. Nothing else. Verified by grep over `internal/` for `reportStats` — the complete write set is
   session.go:611, 621, 634, 640 and watcher.go:549.

**A same-login broadcast/session change does NOT reset them.** The map is keyed by login only;
`executeSessionRefreshes` (watcher.go:694), the `RefreshStreamInfo`/`RefreshSession` paths, and
`Stream.SessionGeneration()` (models/session.go:78-82) leave `reportStats` untouched. A channel
that changes broadcast while keeping its slot keeps accumulating into the same counter — so the
watchdog's `ReportsSinceProgress` spans the broadcast boundary without any re-baseline.
Conversely, a channel that loses its slot for one tick and comes back on the SAME broadcast
DOES get its counter reset.

**Open Questions:**
- Should the published `ReportStats` carry a tenure/epoch id (bumped on every prune-and-restart)
  so a consumer can tell "counter reset" from "counter unchanged"? Today nothing distinguishes
  them.
- Is there a test that re-slots a pruned login and asserts the restart-at-zero? Not found in
  `internal/watcher/*_test.go`.
