# `ProgressWatchdog.gatesHold` — internal/health/progress.go (base 6b1d80e)

Part of the `stall-confirmation-seam` audit context. Index:
`audit-context/DOSSIER-stall-confirmation-seam.md`.

---

## `gatesHold` in internal/health/progress.go (L754-L799)

**Purpose:** The conjunctive, false-positive-averse core of the watchdog: every non-threshold
condition that must hold simultaneously before a stall may be counted at all. Returning
`false` in `evaluate` (L540) both blocks confirmation AND destroys the evidence window
(L547). It returns the first failing gate's human reason for the Drops-page detail line.

**Inputs & Assumptions:**
- `st *dropState` (trusted, loop-owned) — read only; `st.Channel` was just written by
  `observeProgress` (L691).
- `campaign *models.Campaign`, `drop *models.Drop` (semi-trusted, immutable once published —
  drops.go:927-930).
- `sync drops.SyncStatus` (trusted value copy, one per pass, L526).
- `outage bool`, `outageSignal string` (trusted, one per pass, L527).
- `cfg WatchdogConfig` (trusted, one per pass, L517).
- `now time.Time` (trusted, pass-constant).
- Implicit: `w.watch` non-nil (L772), `w.resolver` may be nil (`resolveStreamer` L407-L412
  returns nil, which L777 turns into a gate failure — safe degradation).
- Precondition: `st.Channel` is this pass's farming channel. Established by `observeProgress`
  running first (`evaluate` L538 precedes L540).

**Outputs & Effects:**
- Pure read: no writes to `st`, no I/O, no locks of its own beyond the `Stream` RLocks inside
  the models accessors. Returns `(bool, string)`.

**Block-by-Block:**

```go
// L755-L757
if outage { return false, fmt.Sprintf("... (%s failing)", outageSignal) }
```
- **What:** The Twitch/account-outage gate.
- **Why here:** First and cheapest — during an outage every other gate is noise.
- **Assumes:** `twitchOutage` (L473-L492) reflects the CURRENT state. `Center` has no TTL:
  `Record` (center.go:113-118) replaces a signal and it survives indefinitely; `Snapshot`
  (center.go:139-147) never checks `CheckedAt`. A stale FAILED signal blocks confirmation
  permanently. **Nothing found** enforcing freshness.
- **Assumes:** `SignalWatchTransport` is relevant to the FARMING channel. It is not: it is
  recorded only by `Canary.probe` against the single configured `cfg.Channel`
  (canary.go:200-222; the three `Name: SignalWatchTransport` sites are canary.go:279, 289, 300).
  `inconclusiveWatchTransport` (L453-L461) already discards canary-local, channel-local, and
  explicitly-inconclusive provenance; everything else — including `<stage>_http_<n>` from the
  canary channel — still gates the farming channel's stall. Documented as deliberate
  (L463-L472, L437-L452); recorded here as a cross-channel coupling, no verdict.
- **Establishes:** past this point, no known Twitch-side outage.

```go
// L763-L768
if sync.ProgressLastError != "" { return false, "... inventory reads are currently failing ..." }
if !sync.ProgressLastSyncAt.IsZero() && now.Sub(sync.ProgressLastSyncAt) > cfg.StallDelay {
    return false, "... no inventory observation completed within the stall-delay window"
}
```
- **What:** Inventory observability — "checked and unchanged", never "could not check".
- **Why here:** Before any channel-side gate, because unobservable progress makes the whole
  question meaningless (comment L758-L762).
- **Assumes:** `ProgressLastError` is cleared on every SUCCESSFUL observation. Established:
  `recordProgressSync` clears it on `err == nil` (drops.go:661-664) and
  `publishProgressObservation` sets `progressLastErr = ""` on every accepted outcome
  (drops.go:704-706).
- **Assumes:** the full campaign sync must NOT stamp these fields. Established by the explicit
  comment at drops.go:652-655 ("its inventory step swallows errors internally and a failed
  read must never masquerade as a successful observation").
- **Note (`IsZero` escape):** if NO progress sync has ever run, `ProgressLastSyncAt` is zero and
  BOTH observability gates pass (L763 sees "" and L766's `!IsZero()` is false). The evidence
  window then opens at L555-L563 with `lastObservedSyncAt` zero, and `NoProgressObs` cannot
  increment until the first light sync lands (L564 requires `!IsZero()`), so
  `StallConfirmations >= 2` (config.go:886-887) still blocks confirmation. The gate is passable
  but the threshold is not — recorded as a consistency observation, no verdict.
- **Establishes:** progress is currently observable, with a fresh-enough observation.

```go
// L769-L774
if st.Channel == "" { return false, "no slotted channel is farming this campaign right now ..." }
if !w.watch.IsWatching(st.Channel) { return false, "... does not hold a watch slot right now" }
```
- **What:** The "we are demonstrably watching" gate — the qualitative half of the proof whose
  quantitative half is `ReportsSinceProgress >= stallMinReports` (L574).
- **Why here:** Cheap atomic reads before the streamer-object gates.
- **Assumes:** `IsWatching` is a sufficient proxy for "minutes are actually being delivered".
  It is not on its own: `watchingLogins` is published at the START of a watcher tick
  (watcher.go:673 → broker.go:383-384), BEFORE any send happens, whereas the delivery counters
  are published at the END (watcher.go:778). A channel newly slotted this tick reads
  `IsWatching == true` with zero deliveries. The `stallMinReports` term is what covers this —
  and it is exactly the term that can go stale (see `watchdog-observe-progress.md`, L700).
  So the two halves of the "demonstrably farming" proof are read from two atomics published at
  two different points of the watcher tick, joined by nothing. **Nothing found.**
- **Assumes:** `st.Channel` and the login `IsWatching` is asked about are the same tenure.
  Nothing distinguishes tenures; `watchingLogins` is a `map[string]bool` (broker.go:368).
- **Establishes:** the named login holds a slot as of the last published allocation.

```go
// L776-L782
streamer := w.resolveStreamer(st.Channel)
if streamer == nil { return false, "farming channel is not resolvable to a live streamer object" }
if campaign.Game != nil && streamer.Stream.GameID() != campaign.Game.ID {
    return false, fmt.Sprintf("%s switched away from %s ...", st.Channel, campaign.Game.Name)
}
```
- **What:** The channel must still be streaming the campaign's game.
- **Assumes:** `GameID()` is current. Established: read under `Stream`'s RLock
  (models/stream.go:594-602) and written by the watcher/client stream-info path.
- **Assumes (skip):** `campaign.Game == nil` skips the game check entirely. Nothing establishes
  that a nil Game is safe here beyond the ACL/eligibility gate below. Recorded.

```go
// L785-L794
eligible := false
for _, c := range streamer.Stream.GetCampaigns() { if c.ID == campaign.ID { eligible = true; break } }
if !eligible { return false, fmt.Sprintf("campaign is no longer assigned to %s ...", st.Channel) }
```
- **What:** The tracker's intersection (game + advertised campaign + allow-list) must still
  assign this campaign to the channel.
- **Why here:** After the game check, so the message is more specific when the game moved.
- **Assumes:** `GetCampaigns` returns the tracker's current assignment. Established: returned
  under the Stream RLock (models/stream.go:269-273); written only by `SetCampaigns`
  ("drops tracker only", models/stream.go:275-277) via `updateStreamerCampaigns`
  (drops.go:966).
- **Note:** this repeats the check `farmingChannel` already did (L503-L507) — but against a
  streamer resolved at a LATER instant, and `farmingChannel`'s result may have been overridden
  by the L670-L675 fallback. That is deliberate (comment L671-L673): the fallback exists so
  this gate produces the precise message.

```go
// L795-L798
if drop.HasPreconditionsMet != nil && !*drop.HasPreconditionsMet {
    return false, "drop preconditions not met on Twitch's side ..."
}
return true, ""
```
- **What:** Twitch itself says this drop cannot progress yet.
- **Assumes:** a nil `HasPreconditionsMet` means "unknown, do not block". Established:
  `Drop.Update` sets the pointer only when the key is present
  (models/drop.go:142-144) — absence is genuinely unknown, and the conservative choice here is
  to let the gate pass, which is the LESS conservative direction for stall confirmation.
  Recorded, no verdict.

**Cross-Function Dependencies:**
- Callee `w.watch.IsWatching` (internal, broker.go:411-417): needs slot occupancy. See above
  for the publication-timing skew.
- Callee `w.resolveStreamer` (internal, L407-L412 → `StreamerResolver`, production
  `Miner.resolveStreamer` miner/health.go:42-52): needs the live streamer or nil.
- Callee `streamer.Stream.GameID` / `GetCampaigns` (internal, models/stream.go:594, :269):
  need the current stream identity under the Stream lock.
- Callers: `evaluate` (L540) only. It assumes a `false` return justifies destroying the whole
  evidence window (`resetEvidence`, L547) — a strictly conservative coupling: any gate flap
  costs the accrued delay, observations, and report baseline.
- Shared state: reads only.
- Invariant couplings: this function does NOT read `ReportStats`. The stall proof therefore
  splits across `gatesHold` (qualitative: slot held, eligible, observable, no outage) and
  `evaluate` L574 (quantitative: `ReportsSinceProgress`), with `observeProgress` as the only
  bridge. Nothing checks that the two describe the same instant.

**Open Questions:**
- Should the outage gate ignore a `watch_transport` signal older than, say, one canary
  interval? Nothing today distinguishes a fresh failure from one recorded hours ago.
- Should `campaign.Game == nil` (L780) be a gate failure rather than a skipped check?
- Is the "no progress sync has ever run" escape at L766 (`IsZero`) intended to pass the
  observability gate?
