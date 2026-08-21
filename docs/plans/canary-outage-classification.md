---
artifact_contract: ce-unified-plan/v1
artifact_readiness: implementation-ready
execution: code
task_id: canary-outage-classification
base_sha: 7ad696cf7561f796288ece65edd05be82623bc52
---

# Canary outage classification

## Problem

The health canary and the drop-progress watchdog collapse two different statements:

1. "this canary probe did not complete successfully", and
2. "Twitch/account/watch transport is in an outage state, therefore drop-stall evidence is invalid".

`Canary.runOnce` bounds a probe by `canaryTimeout` (60s, `internal/health/canary.go:54,217`).
The Twitch client calls it depends on (`GetChannelID`, `CheckStreamerOnline`) are
context-unaware and run under `runDetached`; when they outlive the local budget,
`abortReason` (`canary.go:411-416`) maps the ctx error to a `watch_transport` Signal with
`Status=failed, Stage=stream_info, ErrorCode=timeout`, which `runOnce` unconditionally
`Center.Record`s (`canary.go:220-222`). `Center` retains the last signal with no TTL
(`center.go:112-118`), and a failed probe stamps a fresh `CheckedAt`, so the next scheduled
probe waits a full `Interval` (default 360 min, floor 60 — `config.go:562,856-859`) and
`MaxStaleness` never fires for a *failing* canary.

`ProgressWatchdog.twitchOutage` (`progress.go:417-431`) reads only `Status` — any
failed/degraded `watch_transport`, whatever its provenance — and `evaluate` then destroys the
accrued stall-evidence window via `resetEvidence` (`progress.go:479-491,174-179`).

### Measured RED on base `7ad696c`

`internal/health/watchdog_outage_classification_test.go` (this task's regression pair) is red
on the base:

- `TestWatchdogCanarySelfTimeoutPreservesStallEvidence` — with 20m/2-obs/6-report evidence
  accrued, one canary self-timeout zeroes `NoProgressObs`/`ReportsSinceProgress`; the state's
  Detail shows the gate's `"Twitch connectivity is degraded (watch_transport failing)"`.
- `TestWatchdogRetainedCanaryTimeoutDoesNotMasqueradeAsOutage` — a single retained
  self-timeout suppresses a genuine stall through a complete fresh evidence window
  (`triggered=0`).

False-negative impact (derived from defaults): legitimate stall confirmation needs ~30-35 min
(StallDelay 20m + 3 observations); one self-timeout defers it to "next successful probe (up to
6h) + a full fresh window", ~11-13×, unbounded under repeated timeouts.

Acceptance-criteria IDs (A1-A19) referenced below come from the owner's task contract for
`canary-outage-classification` (session prompt; not a repo file). The two cited here:
**A3** — a single inconclusive/local canary result retained in Center must not indefinitely
masquerade as a global outage; **A10** — canary failure-notification semantics remain
unchanged, or any necessary change is explicitly justified and test-covered.

## Root cause

The consumer discards the provenance the Signal already carries. `Signal.Stage` and
`Signal.ErrorCode` distinguish a canary-local budget abort (`timeout`/`cancelled`), a
channel-local condition (`channel_offline`, `channel_resolve_failed`, `spade_url_missing`), an
explicitly inconclusive check (`stream_status_*`, `stale_session_error`,
`session_snapshot_error`), and genuine remote transport evidence (`*_http_<n>` from the farming transport itself) — but `twitchOutage`
tests `Status` alone. Full variant matrix:
`audit-context/DOSSIER-canary-outage-classification.md` §6.

One producer-side ambiguity blocks a purely consumer-side fix: the prober's status-less codes
(`playback_token_error`, `playlist_error`, `segment_error`, `beacon_error`, sender.go:193-199)
are the same bytes for "genuine network-level failure" and "local ctx budget expired
mid-request". Only the producer knows `ctx.Err()` at abort time.

## Selected design

**C + minimal B**: a narrow unexported consumer-side classification predicate, plus a
two-line producer-side normalization that removes the ambiguity only the producer can see.

1. **`internal/health/progress.go`: unexported predicate**

   ```go
   // inconclusiveWatchTransport reports whether a failed watch_transport signal
   // is NOT trustworthy Twitch-outage evidence for stall gating: canary-local
   // budget/lifecycle aborts, channel-local canary conditions, and explicitly
   // inconclusive checks. Anything else — HTTP-status-bearing probe failures,
   // genuine network-level errors, degraded states, unknown/future codes —
   // still gates (conservative default).
   func inconclusiveWatchTransport(sig Signal) bool
   ```

   Deny-list (returns true — does NOT gate): `timeout`, `cancelled`,
   `channel_offline`, `channel_resolve_failed`, `spade_url_missing`,
   `stream_status_*` (prefix), `stale_session_error`, `session_snapshot_error`.
   Everything else — including empty ErrorCode (bare `StatusFailed`, used by existing
   tests), every `*_http_<n>` code, genuine `*_error` network failures (post-normalization),
   and `StatusDegraded` — keeps gating. Placement, explicitly: `twitchOutage` consults
   the predicate only for `watch_transport` signals with `Status == StatusFailed`;
   `StatusDegraded` keeps gating unconditionally via the existing status check (the
   predicate never sees it, so a future degraded signal carrying an abort-like code
   still gates — the U4 degraded control uses `ErrorCode:"timeout"` to pin exactly
   this). `oauth`/`gql_api`/`pubsub` handling is untouched.

2. **`internal/health/canary.go` (`probe`): producer-side normalization**

   After `res := c.prober.Probe(ctx, streamer)`: if `!res.OK && res.Status == 0 &&
   ctx.Err() != nil && ctxAwareStage(res.Stage)`, record the abort classification
   (`abortReason(ctx.Err())` → `timeout`/`cancelled`) with the reached `res.Stage` and
   `res.Duration`, instead of the ambiguous `<stage>_error` code. `ctxAwareStage` is a
   positive list — `playlist`, `segment`, `beacon` — the only stages whose requests
   observe the probe ctx, so only there can a dead ctx have *caused* the failure.
   `playback_token` is deliberately excluded (Q3 reliability finding): its call takes no
   ctx and runs bounded retries that can outlive the canary budget while still returning
   a genuine GQL verdict — rewriting it would hide real account/API evidence. Unknown
   future stages keep their own (gating) code. An HTTP-status-bearing failure
   (`res.Status > 0`) keeps its code even when ctx has expired — a remote response is
   remote evidence. A successful probe at the buzzer stays OK.

**Evidence contract: continue.** An inconclusive signal simply does not constitute an
outage; the evidence window keeps accruing. Justification: a stall still cannot confirm
without every direct gate — fresh clean inventory observations (progress observable), ≥5
*delivered* minute reports since progress (transport demonstrably delivering,
`watcher.ReportStats.Successes` counts delivered beacons only), full StallDelay of
demonstrable farming, healthy `oauth`/`gql_api`/`pubsub`, and slot/eligibility/precondition
gates. An inconclusive canary result carries no information contradicting those direct
observations; "pause" semantics would recreate the defect (a retained inconclusive signal
pausing detection indefinitely violates A3).

**Real-outage conservatism is preserved by the existing gate lattice**: during a real
outage, inventory reads fail (observability gate), beacons fail (report threshold), and
GQL/OAuth/PubSub signals gate independently. The scenario "everything delivers and is
observed, only the canary times out" is not an outage — it is at most the very
stall-with-healthy-delivery the watchdog exists to detect.

### Rejected alternatives

- **A (inline in `twitchOutage`)** — same logic, shallower seam: no named decision table,
  no direct test surface for the classification. Folded into C.
- **B alone (record `unknown`/inconclusive status)** — changes the Health Center UI
  representation and risks the transition-notification contract; also cannot classify
  channel-local codes (not aborts). The narrow ErrorCode normalization sub-piece is kept
  because only the producer sees `ctx.Err()`; it records a *more* truthful code for an
  already-failed probe and leaves Status/notification semantics untouched.
- **D (corroboration/temporal model)** — requiring a corroborating signal makes
  `watch_transport` contribute nothing the corroborators don't already gate on their own;
  extra correlation state with zero added behavior, and it would silently drop the honest
  `*_http_<n>` evidence. Shallow module; rejected.
- `plan-arbiter` not invoked: after design analysis a single viable design remained.

## Implementation units

**U1 — regression pair (already red on base).**
`internal/health/watchdog_outage_classification_test.go`: keep both tests as the permanent
producer→consumer regressions (real Canary + real ProgressWatchdog + one shared Center, no
network, no sleeps for synchronization; deterministic 20ms test-only timeout via the
existing same-package `c.timeout` seam).

**U2 — classification predicate + twitchOutage change** (`internal/health/progress.go`).
Table-driven tests through watchdog behavior AND a direct same-package predicate table
(`TestInconclusiveWatchTransportPredicate`, incl. the prefix boundary), plus real
producer→consumer regressions for every producer-reachable deny-listed class
(`TestWatchdogRealProducerDenyListCodes`) so a producer-side code rename cannot silently
reintroduce gating with a green suite.

**U3 — producer normalization** (`internal/health/canary.go`). Tests: a ctx-aware-stage
prober failure with `Status=0` under an expired ctx records `ErrorCode=timeout` (stage
and abort detail preserved); an explicit cancel records `cancelled`; `Status>0` (e.g.
`beacon_http_503`) keeps the HTTP code even under expired ctx; a genuine `Status=0`
failure with live ctx keeps `<stage>_error`; `playback_token_error` keeps its code even
under a dead ctx. Test seams: `fakeProber.ctxRes` (result returned after ctx death) and
`fakeClient.noTransition` (unknown stream status) in `internal/health/health_test.go`,
whose pre-existing canary producer tests now also pin the exact ErrorCodes.

**U4 — true-outage controls.** Keep `TestWatchdogGateTwitchOutageAllSignals` (bare
`StatusFailed` per signal must still gate — proves the conservative default). Add: a
`watch_transport` failure with a trustworthy code (`beacon_http_503`) still gates and still
resets evidence; degraded `watch_transport` with an abort-looking code still gates; an
OAuth failure still gates AND resets evidence (acceptance A4).

**U5 — docs**: dossier already records the pinned scheduling consequence (§3) and variant
matrix (§6); plan file = this document.

## Quality gates

- Q0: `gofmt -l` on touched files, `git diff --check`, `go vet ./...`, `go build ./...`,
  `python3 scripts/validate-agent-governance.py --self-test-hook`, `--self-test`, full validator.
- Q1: `go test -race -count=1 ./internal/health/...` (plus `-count=20` on the new
  regressions and the true-outage controls), `./internal/miner/...`, `./internal/watcher/...`,
  `./internal/drops/...`.
- Q2: `go mod verify`, `go vet`, `go build`, `go test -race -count=1 ./...`, `make lint`,
  `git diff --check`, governance validators; diff audit: no go.mod/schema/workflow/UI/
  template/static/protocol changes, only authorized files.
- Q3: adversarial code review (code-review + ce-code-review) on the final integrated diff;
  0 BLOCKER / 0 MAJOR.
- Mutation sensitivity: temporarily reintroduce the conflation (make `timeout` gate again)
  → both regressions must FAIL; byte-identical restore (hash-verified) → PASS.

## Risks / notes

- `stream_status_` prefix matching must not accidentally cover future *trustworthy* codes:
  the prefix is produced only by the inconclusive branch (canary.go:253-257) — documented in
  the predicate.
- Existing notification semantics: unchanged for status transitions (still failed);
  the normalization changes only the ErrorCode/Detail of a mid-probe budget abort
  (truth-increasing; covered by U3 tests). A10 satisfied.
- Scheduling (`sinceLastTransportCheck`, MaxStaleness) deliberately untouched; consequence
  pinned in the dossier §3. With the fix, 6h retention of an *inconclusive* signal harms
  only UI freshness, not stall detection; a retained *trustworthy* failure (e.g.
  `beacon_http_503`) still gates until the next probe overwrites it — deliberate
  conservatism, exercised by U4.
- No new locks, no new goroutines; predicate is pure; producer change stays on the probe
  goroutine; consumer change stays on the watchdog goroutine reading an immutable snapshot.
- `ctx.Err() != nil` after a status-less ctx-aware-stage prober failure cannot distinguish
  "the budget killed the request" from "a genuine network failure that completed at the
  buzzer"; the rare misattributed probe records `timeout` and is treated as inconclusive
  until the next probe. Accepted: a real outage keeps failing the observability and
  report-threshold gates directly, so no stall can confirm or be masked on the strength of
  this one signal. Load-bearing timing invariant (documented at `ctxAwareStage`): the
  ctx-aware stages are bounded by the sender's 20s HTTP client timeout < 60s canary
  budget, so genuine remote failures there normally return with ctx still live.
- Deny-list reachability nuance: `session_snapshot_error` IS producible through the canary
  (the canary pre-checks only `GetSpadeURL()` while `Probe` additionally requires
  `HasPayload()`); `stale_session_error` is deny-listed defensively — the canary's
  single-writer ephemeral streamer cannot change session generation mid-probe, so its
  decision-table row is exercisable only with a synthetic Signal.
