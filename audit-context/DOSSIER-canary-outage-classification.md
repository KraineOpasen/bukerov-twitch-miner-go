# Audit dossier — canary self-timeout vs. watchdog outage classification

Task: `canary-outage-classification` · base `7ad696cf7561f796288ece65edd05be82623bc52`.
Scope: producer→consumer contracts around the `watch_transport` health signal and the
drop-progress watchdog's outage gate. Built from a full read of `internal/health/*.go`,
`internal/watcher/sender.go`, `internal/watcher/session.go`, `internal/drops/drops.go`
(SyncStatus), `internal/miner/health.go`, `internal/miner/miner.go` (wiring), and a
deterministic producer→consumer repro (`internal/health/watchdog_outage_classification_test.go`)
run on the pinned base. Single-context analysis; independently re-reviewed at Q3.

## 1. Producers of `watch_transport`

The **only** production producer is `Canary` (`internal/health/canary.go`). Verified by
grep over `internal/`: `miner.refreshHealthCenter` records `oauth`, `gql_api`, `pubsub`,
`drops_inventory`, `drops_progress` — never `watch_transport`. `internal/web` only reads.
The canary records exactly two statuses: `StatusOK` and `StatusFailed`
(canary.go:263–283, 285–295). **`StatusDegraded` for `watch_transport` is currently
unreachable** — the consumer's degraded arm for this signal is dead code today.

### Signal shapes the canary can record (complete)

| # | Stage | ErrorCode | Producing path (file:line) |
|---|-------|-----------|-----------------------------|
| 1 | `stream_info` | `timeout` | ctx deadline around ctx-unaware `GetChannelID`/`CheckStreamerOnline` via `runDetached` → `abortReason` (canary.go:231–235, 243–247, 345, 411–416) |
| 2 | `stream_info` | `cancelled` | explicit ctx cancel (Stop/shutdown), same paths (canary.go:411–416) |
| 3 | `stream_info` | `channel_resolve_failed` | `GetChannelID` returned an error with ctx still live (canary.go:235) |
| 4 | `stream_info` | `channel_offline` | canary channel confirmed offline (canary.go:252) |
| 5 | `stream_info` | `stream_status_<reason>` | stream-info check inconclusive — explicitly documented "must NOT assert the channel is offline" (canary.go:253–257) |
| 6 | `spade_url` | `spade_url_missing` | online but no spade URL discovered (canary.go:259–261) |
| 7 | probe stage | `<stage>_error` (Status 0) | `probeFail` from `MinuteSender.Probe`: `session_snapshot`, `playback_token`, `playlist`, `segment`, `beacon`, `stale_session` (sender.go:158–199) |
| 8 | probe stage | `<stage>_http_<n>` (Status>0) | same, with an HTTP status reached (sender.go:193–199, 201–208) |
| 9 | — | (OK) | full probe succeeded (canary.go:275–283) |

Provenance is therefore **already carried by the Signal** (Stage + ErrorCode); it is the
consumer that discards it.

## 2. Center retention

`Center.Record` replaces the named signal and republishes (center.go:112–118). **No TTL,
no downgrade, no freshness check anywhere.** A recorded `watch_transport` failure remains
the live truth until the next canary probe records over it. `Snapshot()` is an immutable
copy — atomic, lock-free reads (center.go:139–147); no torn state between producer and
consumer (concurrency is sound; the defect is semantic).

## 3. Canary scheduling interaction (pinned consequence)

`sinceLastTransportCheck` (canary.go:424–430) measures from `CheckedAt` of the **latest
signal regardless of status**. A failed/aborted probe stamps a fresh `CheckedAt`
(canary.go:285–295), therefore:

- the next *scheduled* probe waits a full `Interval` after a failure (default 360 min,
  validated minimum 60 min — config.go:562, 856–859);
- `MaxStaleness` (forced-run trigger) is also reset by a failed probe — a failing canary
  never becomes "stale";
- consequently a single failed local probe holds whatever meaning consumers assign to the
  retained signal for **at least a full Interval** (manual RunNow aside).

Comment-vs-code: config.go:359 documents `CanaryIntervalMinutes` as "the target cadence
for a **successful confirmation**"; the code schedules by *attempt*, not confirmation.
Out of contract scope to change; recorded here as a pinned, documented consequence.

## 4. Consumer contract — what the watchdog actually needs

`ProgressWatchdog.twitchOutage` (progress.go:417–431) asks `Status == failed || degraded`
for `oauth`, `gql_api`, `pubsub`, `watch_transport`; on true, `gatesHold` fails
(progress.go:694–696) and `evaluate` calls `resetEvidence` (progress.go:479–491,
174–179), zeroing the evidence window (`evidenceSince`, `NoProgressObs`,
`ReportsSinceProgress`, `statsChannel`).

What the consumer *needs* (per its own design comments, progress.go:54–57, 148–152,
689–693): "is there sufficiently trustworthy evidence that Twitch/account/watch delivery
is impaired, such that absence of drop progress should not be attributed to the farming
session?" What it *reads*: "did the latest canary attempt fail, for any reason, at any
time in the past?" That gap is the defect.

Independent evidence lattice already protecting the watchdog during a REAL outage,
regardless of `watch_transport`:

- inventory observability gates (progress.go:702–707): a failing/absent progress sync
  blocks confirmation — during a real GQL/API outage inventory reads fail;
- delivery threshold (`ReportsSinceProgress >= stallMinReports`, progress.go:511–513):
  during a real transport outage minute-watched sends fail and reports never accrue —
  `ReportStats.Successes` counts only delivered beacons (watcher/session.go:43);
- `oauth`/`gql_api`/`pubsub` signals gate independently (miner/health.go:165–222).

## 5. Comments claiming more than the code proves

1. canary.go:75 "The prober is context-aware" — `GetPlaybackAccessToken` inside
   `Probe` takes no ctx (sender.go:171); a hang there blocks `probe()` past
   `canaryTimeout` (bounded only by the GQL client's own HTTP timeouts).
2. progress.go:413–416 (twitchOutage doc) "health center currently shows evidence of a
   Twitch-side (or account-side) outage" — code checks only the retained status; no
   provenance, no freshness. The RED repro shows a canary-local abort satisfying it.
3. gatesHold's user-facing detail "Twitch connectivity is degraded (%s failing)"
   (progress.go:695) — asserted for canary-local aborts too.
4. config.go:359 "cadence for a successful confirmation" vs. attempt-based scheduling
   (see §3).
5. Test-only: `fakeProber` returns `ErrorCode:"beacon_timeout"` (health_test.go:77) — a
   code the real `probeFail` taxonomy can never produce (`beacon_error`/`beacon_http_<n>`
   only). Existing test, not touched; noted so nobody keys production logic to it.

## 6. Variant matrix — every `watch_transport` failure through the same predicate

Classes: **A** DEFINITIVE_OR_STRONG_TWITCH_OUTAGE_EVIDENCE · **B** CHANNEL_LOCAL_FAILURE ·
**C** LOCAL_CANARY_BUDGET_OR_LIFECYCLE_ABORT · **D** INCONCLUSIVE · **E** NOT_REACHABLE /
DIFFERENT PRODUCER.

| Variant (Stage/ErrorCode) | ФАКТ (what the code proves) | ВЫВОД (class) | Severity if conflated | Same root fix? |
|---|---|---|---|---|
| `stream_info`/`timeout` | Local 60s budget expired around ctx-unaware client calls; canary.go:388 documents the GQL client alone may legitimately take 30s×retries×candidates | **C** | High — the confirmed exemplar; suppresses stall detection ≥ Interval | **Yes** (root) |
| `stream_info`/`cancelled` | Only producible via ctx cancel; `Stop()` is called solely in the miner shutdown sequence (miner.go:1957–1964), sharing the ctx the watchdog itself stops on | **C** (shutdown-reachable only in production) | Nil in steady state; correctness of classification only | **Yes** (same `abortReason` seam, zero extra complexity) |
| `stream_info`/`channel_resolve_failed` | The one configured canary channel failed to resolve (GQL error or nonexistent channel). GQL health has its own signal; a bad canary channel is operator misconfiguration | **B** (with a **D** component when the cause is a transient GQL error) | Medium — a misconfigured canary suppresses stall detection indefinitely | Design decision (see plan) |
| `stream_info`/`channel_offline` | The configured canary channel is offline — proves nothing about Twitch globally | **B** | Medium — an offline canary channel gates the watchdog for the whole offline period | Design decision |
| `stream_info`/`stream_status_<reason>` | Source itself labels this "inconclusive … must NOT assert the channel is offline" (canary.go:253–257) | **D** | Medium | Design decision |
| `spade_url`/`spade_url_missing` | Channel online but spade URL not discovered — channel/page-shape local; farming channels have their own spade discovery | **B/D** | Medium | Design decision |
| `session_snapshot_error` | Probe entered without a usable session snapshot — canary-internal sequencing (spade checked immediately before, canary.go:259) | **D** (near-unreachable) | Low | Covered by whatever handles status-less codes |
| `playback_token_error` | GQL playback-token fetch failed (no ctx, no HTTP status). Could be account-level (auth/integrity) or transient GQL. NOT ambiguous with ctx expiry after all: the call never observes ctx, so a dead ctx is never causal for this stage (Q3 reliability finding) — excluded from producer normalization (`ctxAwareStage`), keeps gating | **A/D (gates)** | — | Resolved: keeps its own gating code |
| `playlist_error` / `segment_error` / `beacon_error` (Status 0) | Network-level failure **or** local ctx-budget expiry mid-request — `probeFail` cannot tell them apart (sender.go:193–199); ctx cancellation surfaces as the same code | **D** (ambiguous by construction) | Medium — status-less codes are indistinguishable from budget aborts | Producer-side ctx check disambiguates (design) |
| `playlist_http_<n>` / `segment_http_<n>` / `beacon_http_<n>` | Twitch/CDN answered with an error status on the exact transport farming uses — a remote server response is genuine remote evidence | **A** (strongest available from the canary) | — (must KEEP gating) | Must remain outage evidence |
| `stale_session_error` | sender.go:66–69 documents stale-session as "NOT a transport failure and NOT an authoritative offline" | **D** | Low | Covered |
| Canary channel misconfigured/nonexistent | Manifests as `channel_resolve_failed` (persistent) — see above | **B** | Medium | Same as resolve_failed |
| `watch_transport` `StatusDegraded` | No producer can currently record it (canary emits OK/Failed only; no other producer exists) | **E** (dead arm today) | — | Keep the degraded arm conservative (future producers) |
| Any non-Canary `watch_transport` producer | `grep` proves none exists in production code | **E** | — | n/a |

## 7. Unenforced assumptions (`nothing found`)

1. `twitchOutage` assumes a retained `watch_transport` failure implies a *current*
   outage — nothing checks `CheckedAt` freshness anywhere in the consumer. `nothing found`.
2. `twitchOutage` assumes a failed probe implies a *remote* failure — the Signal carries
   Stage/ErrorCode provenance, and nothing reads it. `nothing found`.
3. Canary scheduling assumes `CheckedAt` means "transport checked" — a failed attempt
   stamps it too, so "confirmed" and "attempted" are conflated in `maybeRun`/staleness.
   `nothing found` (pinned in §3, out of contract scope to change).

## 8a. Deferred concerns after Q3 (owner decisions / out of contract authority)

1. **SPECIFICATIONS.md:1555-1556** ("no Twitch outage evidence … signals not FAILED")
   now under-describes the watch_transport provenance carve-out (and already ignored the
   degraded arm pre-fix). The file is outside this contract's `allowed_paths`; a one-line
   owner edit or contract addendum is needed to restore "source of truth" accuracy.
2. **Cumulative-reports cascade** (Q3 adversarial, P2/conf 50): `ReportsSinceProgress`
   is cumulative-since-progress and freezes when deliveries stop, so a threshold met
   BEFORE a real delivery outage plus still-working inventory reads can, in a narrow
   compound window, confirm a stall during a genuine outage — base-code semantics,
   barely widened by this diff (the pre-fix protection was an incidental 6h-cadence
   canary probe). Candidate fix is a delivery-recency gate in `gatesHold`
   (`ReportStats.LastSuccess`) — a watchdog-semantics product decision, out of this
   task's outage-classification seam.
3. **Attempt-stamped scheduling** (`sinceLastTransportCheck` from failed `CheckedAt`,
   §3): a retained trustworthy failure gates+resets up to a full Interval and a failing
   canary is never "stale" — pre-existing, pinned, contract forbids scheduling changes.
4. **Operator-stream marker** (Q3 adversarial, P3): transition notifications/Health tile
   do not mark deny-listed failures as "inconclusive (not treated as outage)" — advisory;
   left unchanged to keep A10 notification semantics byte-stable.
5. `fakeProber`'s default `beacon_timeout` code remains outside the real taxonomy (§5) —
   pre-existing test fake, untouched.

## 8. Open questions carried to design

- Should channel-local canary conditions (`channel_offline`, `channel_resolve_failed`,
  `spade_url_missing`) keep gating the watchdog? They are not global-outage evidence, but
  they *are* honest "the canary cannot currently verify the transport" states.
- Are the status-less probe codes (`*_error`, Status 0) distinguishable at the producer
  (ctx.Err() at abort time) and is that worth the seam?
- What happens to already-accrued evidence during an inconclusive canary state: continue,
  pause-without-reset, or reset? (Owner constraint: an inconclusive self-timeout alone
  must NOT destroy a valid window; A3: no indefinite masquerade.)
