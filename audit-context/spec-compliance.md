# Spec-to-code compliance — Drop progress watchdog stall confirmation

**Scope (narrow).** SPEC: `SPECIFICATIONS.md` § "### Drop progress watchdog" (lines 1512–1608).
CODE: `internal/health/progress.go` (plus the delivery-accounting producers it reads,
`internal/watcher/session.go` and `internal/watcher/watcher.go`).

**Base SHA** `6b1d80e6d014b54d476869ed736e39f126369210`, branch `claude/drop-stall-confirmation-evidence-p1vf1l`.
Read-only audit. No source file was modified.

---

## The stall predicate, as actually implemented

`internal/health/progress.go:516` `evaluate` runs, per tracked drop, in this order:

1. `trackDrop` (`progress.go:629`) — spec gates 1 and 2 (campaign ACTIVE / not past `endAt`;
   drop not claimed / not claimable / inside its date window) at `progress.go:630-638`.
2. `observeProgress` (`progress.go:668`) — resolves the farming channel, maintains the
   delivery baseline, and performs the progress-advanced reset.
3. `gatesHold` (`progress.go:754`) — the non-threshold gates (spec gates 10, 9, 3, 4, 5).
   On failure: `st.resetEvidence()` (`progress.go:547`), status forced back to healthy unless
   already terminal, detail set to the first failing gate's reason, `continue`.
4. Evidence-window bookkeeping (`progress.go:555-570`) — open the window, or count one new
   clean inventory observation.
5. The three-threshold conjunction (`progress.go:572-574`):

```go
stalled := now.Sub(st.evidenceSince) >= cfg.StallDelay &&
    st.NoProgressObs >= cfg.StallConfirmations &&
    st.ReportsSinceProgress >= stallMinReports
```

`stallMinReports = 5` is a compile-time constant (`progress.go:83`), not a config knob.

---

## R1 — "All three thresholds count only inside the current evidence window … so a confirmed stall always represents at least `watchdogStallDelayMinutes` of *demonstrable* farming without credit."

SPEC lines 1522–1532.

**Verdict: partial.**

The sentence makes two claims. The mechanical one holds. The guarantee it derives does not.

### Claim (a) — the three thresholds count only inside the window: implemented

| Threshold | Opens with the window | Discarded on gate failure |
| --- | --- | --- |
| delay | `st.evidenceSince = now` (`progress.go:561`) | `st.evidenceSince = time.Time{}` (`progress.go:176`) |
| observations | `st.NoProgressObs = 0` (`progress.go:563`); incremented only at `progress.go:569`, reachable only after `gatesHold` returned true and `evidenceSince` is non-zero | `st.NoProgressObs = 0` (`progress.go:177`) |
| delivered reports | baseline re-taken at `progress.go:681-689` because `resetEvidence` blanks `st.statsChannel` (`progress.go:179`), forcing `channel != st.statsChannel` at `progress.go:676` on the next pass | `st.ReportsSinceProgress = 0` (`progress.go:178`) |

The observation cursor is seeded in three places so a read completed outside/before the window
can never count: episode creation (`progress.go:658`), window open (`progress.go:562`), and the
progress-advanced reset (`progress.go:735`). That matches SPEC 1547–1549 exactly.

The reports counter deserves a note. `resetEvidence` blanks `statsChannel` rather than
re-reading `Successes` directly, so the re-baseline lands at the *start* of the next pass
(`observeProgress`, `progress.go:538`) while the window opens *later in that same pass*
(`progress.go:561`). Deliveries that happened while the gate was failing are folded into
`baselineReports`, not into `ReportsSinceProgress`. Claim (a) therefore holds for reports too.

### Claim (b) — "at least `watchdogStallDelayMinutes` of *demonstrable* farming": not guaranteed

Nothing in the predicate requires delivery to *continue* across the window. `ReportsSinceProgress`
is a cumulative, monotonic count with no recency component and no decay
(`progress.go:700-702`), and no gate in `gatesHold` (`progress.go:754-799`) inspects
`ReportStats.LastSuccess`, `ReportStats.Failures`, or any delivery timestamp — **nothing found**.
Concretely, all three thresholds can be satisfied by:

- five successful sends in the first minute of the window, then
- nineteen minutes in which every send returns `res.Stale` (`internal/watcher/watcher.go:730-738`),
  which increments **neither** `Successes` nor `Failures`, or in which every send fails
  (`internal/watcher/watcher.go:739-748` → `noteReportOutcome(..., false, ...)`,
  `internal/watcher/session.go:617-620`, which the watchdog never reads — it reads only
  `stats.Successes` at `progress.go:682`, `:698`, `:700`).

`IsWatching` (`internal/watcher/broker.go:411-417`) stays true throughout — it reports slot
occupancy, not delivery — so gate 3 (`progress.go:772`) keeps holding. The window survives,
the delay clock runs to 20 minutes, and a stall confirms on delivery evidence that is up to
`StallDelay` old. That is *slot tenure* without credit, not `StallDelay` of *demonstrable
farming* without credit.

A second, narrower hole in claim (b): the `n >= 0` guard at `progress.go:700`. When
`publishReportStats` prunes a login that momentarily lost its slot
(`internal/watcher/session.go:627-634`) and the same login is re-slotted before the watchdog's
next pass, `stats.Successes` restarts near zero while `st.baselineReports` and `st.statsChannel`
are unchanged (same login → no re-baseline at `progress.go:676`). `n` goes negative, the guard
keeps the **previous** `ReportsSinceProgress` value, and that pre-reset evidence survives — with
no gate having failed, so no `resetEvidence`. Spec 1528–1530 ("Evidence accrued while the channel
was … rotated out … never carries over") is not honoured on this path.

### Minor boundary mismatch

SPEC gate 8 (line 1550) says "**more than** `watchdogStallDelayMinutes` of evidence-window time";
the code uses `>=` (`progress.go:572`), i.e. *at least*. Differs only at exact equality.

---

## R2 — Gate 6: "minute-watched reports are demonstrably delivered — the broker's new per-slot delivery accounting shows ≥5 successes since the last progress"

SPEC lines 1541–1542.

**Verdict: implemented** (with one wording drift that makes the code *stricter*, and one
semantic caveat on "demonstrably delivered").

- "≥5": `stallMinReports = 5` (`progress.go:79-83`), applied as
  `st.ReportsSinceProgress >= stallMinReports` (`progress.go:574`). Exact match.
- "the broker's per-slot delivery accounting": `watcher.ReportStats`
  (`internal/watcher/session.go:38-48`), produced on the broker loop goroutine
  (`noteReportOutcome`, `internal/watcher/session.go:607-622`), published per tick and pruned
  to currently slotted logins (`publishReportStats`, `internal/watcher/session.go:624-641`),
  read by the watchdog through `w.watch.ReportStats(channel)`
  (`internal/watcher/session.go:237-244`; call sites `progress.go:681`, `:694`, `:737`).
- "since the last progress": the baseline **is** set at the progress-advanced reset
  (`progress.go:737-741`), but it is *also* re-taken on any farming-channel change
  (`progress.go:676-689`) and on every evidence reset (`progress.go:179` → `:676`). So in
  practice the counter measures successes since the **later of** last progress / last channel
  change / last gate failure. That is narrower than the spec sentence, i.e. harder to satisfy —
  the SPEC text is imprecise, the code is stricter.
- Caveat on "demonstrably delivered": a "success" is one `sender.Send` that returned neither a
  `Failure` nor `Stale` (`internal/watcher/watcher.go:728-753`, `:1748-1754`) — a successful
  spade POST. It proves the beacon left the miner, not that Twitch credited it. That is the
  correct reading for a stall detector and is consistent with the spec's intent.

Placement note: gate 6 is not in `gatesHold`; it lives in the threshold conjunction
(`progress.go:574`). This is consistent with SPEC 1524 ("three thresholds … delivered reports")
and it matters: a failing gate 6 does **not** discard the evidence window, only a `gatesHold`
failure does (`progress.go:540-552`). SPEC 1522–1532 supports that split.

---

## R3 — Gate 10: "no Twitch outage evidence (OAuth/GQL/PubSub/watch-transport signals not FAILED in the health center)"

SPEC lines 1555–1556.

**Verdict: partial — the SPEC text is incomplete relative to the code, in *both* directions.**
This is pre-existing documentation debt introduced by PR #208. **It must NOT be fixed by the
current task**; it is characterised here precisely so it can be recorded as deferred.

`twitchOutage` (`progress.go:473-492`) iterates `SignalOAuth, SignalGQLAPI, SignalPubSub,
SignalWatchTransport` (`progress.go:478`) and deviates from the sentence twice:

**(i) Broader than spec — DEGRADED also counts as outage.**
`progress.go:483`:

```go
if !ok || (sig.Status != StatusFailed && sig.Status != StatusDegraded) {
    continue
}
```

A DEGRADED (flapping / repeatedly-failing) signal blocks stall confirmation exactly as a FAILED
one does, for all four signals. The rationale is in the comment at `progress.go:479-481`. The
SPEC sentence says only "not FAILED" and does not mention DEGRADED at all. Effect on behaviour:
*stronger-than-spec* (more conservative, fewer stalls confirm).

**(ii) Narrower than spec — an inconclusive FAILED `watch_transport` is exempted.**
`progress.go:486-488`:

```go
if name == SignalWatchTransport && sig.Status == StatusFailed && inconclusiveWatchTransport(sig) {
    continue
}
```

`inconclusiveWatchTransport` (`progress.go:453-461`) returns true for error codes `timeout`,
`cancelled`, `channel_offline`, `channel_resolve_failed`, `spade_url_missing`,
`stale_session_error`, `session_snapshot_error`, and anything prefixed `stream_status_`.
Rationale at `progress.go:463-472` and `progress.go:436-452`: the signal is recorded **only** by
the canary probing the single configured canary channel — every recorder is in
`internal/health/canary.go:279`, `:289`, `:300`; there is no other producer of
`SignalWatchTransport` anywhere in `internal/**` (verified by grep) — so a canary-local or
channel-local failure says nothing about the *farming* channel.

This one is a genuine, deliberate contradiction of the sentence as literally written: the SPEC
says a FAILED watch-transport signal blocks confirmation; the code lets a whole class of FAILED
watch-transport signals through. Effect on behaviour: *weaker than the literal spec text* (a
stall can confirm while `watch_transport` reads FAILED).

**Net characterisation for the deferred-debt record:** SPEC 1555–1556 describes neither the
DEGRADED broadening nor the `watch_transport` provenance exemption. It is a two-way
under-description of `twitchOutage()`/`inconclusiveWatchTransport()`, not a code defect.

---

## R4 — Does the SPEC state or imply that delivery evidence must be CURRENT (recent) rather than merely cumulative?

**Verdict: absent. Nothing in the SPEC states or implies a recency requirement on delivery
evidence.**

Everything that touches delivery in the watchdog section is cumulative-since-a-point:

- line 1518 — "delivered watch reports since then" (state description; "then" = last advance).
- lines 1524–1527 — the thresholds "count only inside the current **evidence window**". This
  scopes the counts to a window; it does not require the deliveries to be spread through it or
  to be recent within it. A count of five taken in the window's first minute satisfies it.
- lines 1541–1542 — gate 6: "≥5 successes **since the last progress**". Cumulative, no
  timestamp, no recency, no rate.

The only recency language in the whole section is about **inventory**, not delivery — gate 9,
lines 1551–1554: "the last progress-sync attempt did not error, and a successful observation
completed **within the stall-delay window**". There is no delivery-side analogue anywhere in
§ Drop progress watchdog, nor in the two other places the spec mentions this accounting
(line 693, the broker publishing `ReportStats` each tick; lines 1647–1652, the policy engine's
channel-stability sample gate). **Nothing found.**

The closest the SPEC comes to *implying* it is the derived guarantee at line 1527
("at least `watchdogStallDelayMinutes` of *demonstrable* farming without credit"). That is a
conclusion the current thresholds do not actually deliver (see R1 claim (b)) — an implication a
reader may draw, not a stated requirement.

---

## Sentences that would need to change if the code additionally required CURRENT delivery evidence

Exact text, with line numbers. Listed in the order they appear.

1. **SPECIFICATIONS.md:1518** — the state inventory:
   > `last advanced, delivered watch reports since then, consecutive clean`

   ("delivered watch reports since then" would have to name whatever new recency field the
   state gained, e.g. last delivery timestamp.)

2. **SPECIFICATIONS.md:1524–1527** — the evidence-window guarantee sentence:
   > `three thresholds (delay, observations, delivered reports) count only inside`
   > `the current **evidence window**: it opens when every gate starts holding and`
   > `is discarded whenever any gate fails, so a confirmed stall always represents`
   > `at least `watchdogStallDelayMinutes` of *demonstrable* farming without credit.`

   (This is the sentence whose "so …" conclusion a currency requirement would finally make true;
   if a fourth threshold or a new gate is added, "three thresholds" is also wrong.)

3. **SPECIFICATIONS.md:1541–1542** — gate 6 itself:
   > `6. minute-watched reports are demonstrably delivered — the broker's new`
   > `   per-slot delivery accounting shows ≥5 successes since the last progress;`

   (The gate would need the recency clause — the natural phrasing mirrors gate 9's
   "within the stall-delay window".)

Secondary, only if the change introduces a knob or a new gate number:
**SPECIFICATIONS.md:1595–1602** (the `health` config paragraph, where a new
`watchdog…` setting and its clamp would be documented) and the gate numbering at
**SPECIFICATIONS.md:1534–1556**.

## Sentence that must be LEFT ALONE

**SPECIFICATIONS.md:1555–1556** — gate 10, verbatim:

```
10. no Twitch outage evidence (OAuth/GQL/PubSub/watch-transport signals not
    FAILED in the health center).
```

Its known incompleteness (R3: missing DEGRADED, missing the `watch_transport` provenance
exemption) is pre-existing documentation debt from PR #208 and is **out of scope** for the
current task. Do not touch these two lines.

---

## Reverse direction — behaviour in the stall predicate that no SPEC sentence mentions

1. **`ReportStats.Failures` / `LastFailure` / `LastSuccess` are never read by the watchdog.**
   The struct carries all four fields (`internal/watcher/session.go:43-48`) and
   `noteReportOutcome` maintains them (`internal/watcher/session.go:613-621`), but
   `progress.go:682`, `:698`, `:700`, `:738` read `stats.Successes` only. A channel whose every
   recent send *fails* still satisfies gate 6 on five older successes. The SPEC never mentions
   failures in the watchdog section.

2. **Stale sends are a third outcome class the SPEC never names.**
   `internal/watcher/watcher.go:730-738`: a send whose playback session moved mid-flight
   increments neither counter. Delivery can therefore cease entirely with `Successes` frozen and
   `Failures` flat — invisible to every watchdog gate.

3. **The `n >= 0` guard preserves pre-reset delivery evidence.**
   `progress.go:700`: `if n := stats.Successes - st.baselineReports; n >= 0 { ... }`. After a
   counter prune-and-restart for the *same* login (`internal/watcher/session.go:627-634`),
   `n` is negative and the stale `ReportsSinceProgress` is retained until `Successes` climbs
   back past the old baseline. No SPEC sentence covers this; it partially contradicts
   line 1528–1530.

4. **The delivery baseline is re-taken on channel change and on every evidence reset**
   (`progress.go:676-689`, `progress.go:179`), so gate 6 is in practice "since the later of last
   progress / channel change / gate failure", not "since the last progress" (SPEC 1542).
   Stricter than documented, and undocumented.

5. **A same-pass farming-channel swap keeps the delay clock running.**
   If channel A is replaced by an equally eligible channel B between two passes, `gatesHold`
   never fails (`progress.go:769-794` all pass for B), so `evidenceSince` is not reset — only
   the report baseline is (`progress.go:676-680`). The delay clock then spans two different
   channels' tenures. SPEC 1528–1530 describes rotation as evidence-invalidating.

6. **`baselineValid` cold-start adoption.** `progress.go:684-688` and `:695-699`: when the first
   `ReportStats` read after a channel change misses, the baseline is deferred to the first
   successful read, which may land after the evidence window already opened. Undocumented.

7. **`>=` vs "more than"** on the delay threshold — `progress.go:572` vs SPEC 1550.

8. **A drop already in `stalled` keeps that status through gate failures.**
   `progress.go:548-550` and `:576-577` guard the healthy write with
   `if st.Status != ProgressStalled`. SPEC 1583–1586 describes the episode as terminal until
   progress resumes or rearm elapses, which covers the intent, but the interaction with a
   *failing gate* (SPEC 1530–1532 says a gate failure "pauses the recovery pipeline") is not
   spelled out.

9. **`LastProgressAt` is seeded to `now` at episode creation** (`progress.go:651`) even though no
   advance was observed, so the healthy detail's "last advance %s ago" (`progress.go:578-579`)
   measures time since tracking started. SPEC 1516–1517 calls the field "when they last
   advanced". Cosmetic; the stall predicate does not read it.
