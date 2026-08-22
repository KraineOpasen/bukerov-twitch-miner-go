# Policy readers/publishers — internal/miner/policy.go (HEAD 5b331e5)

Scope: `refreshPolicy`, `PolicySnapshot`, `snapshotDropRules`, `CurrentCampaignPolicy`.
Audit focus recorded per function: lock held at MUTATION / PERSISTENCE / PUBLISH points, error
propagation, and dependent side effects relative to persistence.

NOTE on working tree: `internal/miner/policy_persistence_fail_closed_test.go` and
`docs/plans/policy-persistence-fail-closed.md` are UNTRACKED (git status `??`) — in-flight,
uncommitted work pinning a fail-closed persistence contract that HEAD does **not** implement.
All analysis below describes committed HEAD behavior; the divergence is called out where relevant.

---

## refreshPolicy in internal/miner/policy.go (L26-L60)

**Purpose:** Re-rank all tracked drop campaigns under the configured campaign-policy mode and
publish the result to three lock-free consumers: the watcher (per-login scores for the DROPS
watch-priority tie-break), discovery (per-game ranks for cross-game candidate ordering), and the
miner's own atomic snapshot (Drops page + debug endpoint). Doc comment (L21-25): runs on the
existing health-watchdog tick, adds no goroutine, makes no Twitch calls — every input derives
from already-synced state.

**Inputs & Assumptions:**
- `now time.Time` — trusted; supplied by callers (`healthWatchdogLoop` miner.go:1850-1852 passes
  `time.Now()`; `ApplyCampaignPolicy` policy.go:291 and `commitDropRule` policy.go:337 pass
  `time.Now()`). Forwarded to `policy.Rank` for feasibility math.
- `m.dropsTracker` — may be nil (un-started miner); guarded at L27-29. Establishes: nothing
  below runs before `startMining` built the tracker (miner.go:945).
- `m.watcher` — nil-checked at L53 before `SetCampaignScores`, but `buildPolicyInputs` (called at
  L45) dereferences `m.watcher.BrokerSnapshot()` at L70 with **no nil check**. The implicit
  precondition "dropsTracker non-nil ⇒ watcher non-nil" is established only by `startMining`'s
  construction order (`m.watcher =` miner.go:931 precedes `m.dropsTracker =` miner.go:945).
  No mechanical enforcer beyond that ordering — **nothing found** (see Open Questions).
- `m.discovery` — may be nil; guarded at L56.
- `m.config.CampaignPolicy`, `m.config.DirectoryGames` — read under `m.mu.RLock()` (L31-34).
  `m.mu` is the Miner-wide `sync.RWMutex` (miner.go:274).
- `m.config.DropRules` — read only via the private copy from `snapshotDropRules()` (L42);
  never read lock-free through the shared reference. The comment at L36-41 records why: the
  shared map is mutated in place by `commitDropRule` (policy.go:326-334) under `m.mu.Lock()`,
  and a shared-reference read in `buildPolicyInputs` would be a fatal concurrent map read/write.
  Enforcer: `policy_race_test.go` L15-20, L68-79 drives the real
  `snapshotDropRules → buildPolicyInputs` path under `-race` against a concurrent `SetDropRule`.
- Implicit assumption (comment L40-41): the `games` slice header captured under RLock is safe to
  read unlocked afterwards because "writers replace the whole slice under the lock" — writers
  are `settings.ApplyToConfig` (builder.go:234 reassigns `cfg.DirectoryGames`), applied to a
  candidate config on the applySettings path (miner.go:2311/2395/2506) and argued safe by the
  inventory comment at miner.go:2696-2706. No mechanical enforcement (no test pins
  "no in-place element writes to DirectoryGames") — **nothing found** beyond the comment +
  writer inventory.

**Outputs & Effects:**
- Returns nothing; produces no error.
- PUBLISH point 1 (L53-55): `m.watcher.SetCampaignScores(m.policyScoresByLogin(byID))` —
  watcher stores the map in `w.campaignScores` (an atomic pointer; session.go:159-165). Lock
  held at this publish: **none** (`m.mu` was released at L34; watcher's own `w.mu` is not taken —
  the store is atomic).
- PUBLISH point 2 (L56-58): `m.discovery.SetGameRanks(policyGameRanks(decisions, campaigns))` —
  atomic store into `m.gameRanks` (discovery.go:236-242). Lock held: **none**.
- PUBLISH point 3 (L59): `m.policySnap.Store(&policySnapshot{Mode, Decisions, byID})` —
  `policySnap` is `atomic.Pointer[policySnapshot]` (miner.go:129). Lock held: **none**.
- MUTATION point: refreshPolicy itself mutates **no** shared config state — it is a pure
  read-and-publish. The config mutations it is downstream of (`m.config.CampaignPolicy` write at
  policy.go:288, `m.config.DropRules` map write at policy.go:327-334) happen in its callers
  under `m.mu.Lock()`, *before* refreshPolicy is invoked and *after* the lock is released.
- PERSISTENCE point: refreshPolicy performs none. In its two mutating callers, `persistLocked`
  (policy.go:220-226, `config.SaveConfig`) runs while the caller still holds `m.mu` **write**
  lock (policy.go:287-290; policy.go:326-336) — file I/O under the write lock.
- Ordering postcondition: the three publishes are sequential and non-atomic as a group —
  watcher scores first, discovery ranks second, own snapshot **last**. A concurrent reader can
  observe new watcher scores with an old `PolicySnapshot` (or vice versa across ticks).
  Nothing enforces cross-publish consistency — **nothing found**; consumers are all
  advisory-ordering paths (tie-breaks, UI), per SPECIFICATIONS.md:1670-1671.
- Side-effect timing relative to persistence (audit-critical): in `ApplyCampaignPolicy`
  (L285-294) and `commitDropRule` (L325-338), refreshPolicy — and therefore all three
  publishes — fires **after** the `persistLocked` attempt and after `m.mu.Unlock()`,
  **unconditionally**: a failed `config.SaveConfig` is logged (`slog.Error`, policy.go:223-224)
  and swallowed, the in-memory mutation stays, and refreshPolicy still publishes a snapshot
  ranked under the never-persisted value while the caller returns nil. The untracked test file
  `policy_persistence_fail_closed_test.go` (L33-37, L71-78) pins the opposite (fail-closed)
  contract as the intended future behavior; at HEAD the swallow-and-publish behavior is what
  exists.

**Block-by-Block:**
- L27-29
  ```go
  if m.dropsTracker == nil {
      return
  }
  ```
  What: early-out when drops tracking was never built. Why here: refreshPolicy is wired into
  the always-running healthWatchdogLoop (miner.go:1852) which ticks even in configurations
  without drops. Assumes: nil tracker ⇔ not started / no drops. Establishes: everything below
  may use `m.dropsTracker`, and (by construction order, miner.go:931 vs 945) `m.watcher` is
  non-nil in production. Depended on by: `provider_safety_test.go`-style un-started-miner
  safety (refreshPolicy itself is unexported and not in that test's list; this guard is its
  equivalent).
- L31-34
  ```go
  m.mu.RLock()
  mode := policy.Normalize(m.config.CampaignPolicy)
  games := m.config.DirectoryGames
  m.mu.RUnlock()
  ```
  What: sample mode + directory-games under the miner read lock. Why here: `m.config` fields
  are written under `m.mu` write lock by ApplyCampaignPolicy/applySettings. Assumes:
  `policy.Normalize` (policy/policy.go:51-57) maps any stored string — including invalid ones —
  to a valid Mode (falls back to `DefaultMode` = `GAME_ORDER`, policy/policy.go:38). Assumes
  `games` backing array is never mutated in place after release (comment L38-41; see above —
  enforcement: nothing found). Establishes: `mode` is always valid; RLock is held **only**
  here — every later step runs unlocked.
- L42 (with comment L36-41)
  ```go
  rules := m.snapshotDropRules()
  ```
  What: private copy of the DropRules map (second, separate RLock acquisition inside the
  callee, L261-262). Why here: `buildPolicyInputs` reads rules lock-free at L102; sharing the
  live map would race `commitDropRule`'s in-place writes. Assumes: `config.DropRule` is a
  by-value-copyable struct (five bools, config/config.go:217-231) so a shallow map copy is a
  deep copy. Establishes: no map aliasing downstream. Depended on by: `policy_race_test.go`
  (pins exactly this handoff).
- L44-46
  ```go
  campaigns := m.dropsTracker.Campaigns()
  inputs := m.buildPolicyInputs(campaigns, rules, games, now)
  decisions := policy.Rank(mode, inputs, now)
  ```
  What: snapshot campaigns, assemble policy inputs, rank. `Campaigns()` copies the slice under
  the tracker's own `d.mu.RLock` (drops.go:400-407); the `*models.Campaign` elements are shared
  pointers (doc "immutable after publish", models/stream.go:267-268). `buildPolicyInputs`
  (L65-123) reads broker slots via lock-free `BrokerSnapshot()` (broker.go:389-394), per-slot
  stability via lock-free `ReportStats` (session.go:237-244), eligible-channel counts via
  `m.streamers.All()` (copy under manager RLock, streamer/manager.go:731-735) and
  `m.discovery.State()` (copy under discovery RLock, discovery.go:1143-1151), and drop-rule
  lookups keyed by `models.NormalizeRewardKey(gameID, drop.Name)` (drop.go:326-328).
  `policy.Rank` is pure (internal/policy). Assumes: campaign objects are not concurrently
  mutated (convention, not enforced here — the tracker replaces campaigns wholesale on sync).
  Establishes: `decisions` ordered ranked list, excluded entries last.
- L48-51
  ```go
  byID := make(map[string]policy.Decision, len(decisions))
  for _, d := range decisions {
      byID[d.CampaignID] = d
  }
  ```
  What: by-ID index. Why here: `policyScoresByLogin` (L169-188) needs O(1) lookups per
  streamer-carried campaign. Establishes: the same map is also stored in the snapshot (L59).
  Depended on by: L54; the *stored* copy's `byID` field has **no reader** anywhere
  (grep: only policy.go:50 write and rewards.go's unrelated local) — see Open Questions.
- L53-58
  ```go
  if m.watcher != nil {
      m.watcher.SetCampaignScores(m.policyScoresByLogin(byID))
  }
  if m.discovery != nil {
      m.discovery.SetGameRanks(policyGameRanks(decisions, campaigns))
  }
  ```
  What: PUBLISH to watcher and discovery, both atomic-pointer stores, no locks. Why here:
  consumers read on their own loop goroutines lock-free (`orderByCampaignScore`
  session.go:182-194; `orderGamesByPolicy` discovery.go:248-266) — nil clears and restores
  bit-identical pre-policy ordering (session.go:160-163, discovery.go:237-240). Assumes: maps
  handed over are never mutated afterwards (they are freshly built per call — enforced by
  construction). Establishes: next watcher tick / discovery sync uses the new ordering.
- L59
  ```go
  m.policySnap.Store(&policySnapshot{Mode: mode, Decisions: decisions, byID: byID})
  ```
  What: PUBLISH own snapshot, last. Assumes: the stored `Decisions` slice and `byID` map are
  treated read-only by every later reader (`PolicySnapshot` returns the slice by reference,
  L252) — convention only, **nothing found** as an enforcer.

**Cross-Function Dependencies:**
- Callees: `policy.Normalize` (validity fallback), `snapshotDropRules` (map isolation),
  `dropsTracker.Campaigns` (slice copy under tracker lock), `buildPolicyInputs`
  (→ `watcher.BrokerSnapshot`, `watcher.ReportStats`, `m.resolveStreamer` health.go:42-52,
  `m.eligibleLiveChannels` → `streamers.All` + `discovery.State`, `m.channelStability`),
  `policy.Rank` (pure ranking), `policyScoresByLogin` (→ `streamers.All`,
  `Stream.GetCampaigns` models/stream.go:269-273), `policyGameRanks` (pure, L193-216),
  `watcher.SetCampaignScores`, `discovery.SetGameRanks`, `policySnap.Store`.
- Callers: `healthWatchdogLoop` (miner.go:1839-1856) — every minute, no persistence in play;
  `ApplyCampaignPolicy` (policy.go:291) and `commitDropRule` (policy.go:337) — after their
  m.mu-guarded mutate-and-persist block, so the change "is visible at once" (doc L279-281).
  Both mutating callers run inside `m.fenced` (miner.go:2135-2141), so a retired generation
  never reaches refreshPolicy through them; the watchdog path stops when the generation's run
  context ends.
- Shared state: `m.mu` (config reads), `policySnap` (atomic), watcher/discovery atomic cells,
  the shared `*models.Campaign` graph.
- Invariant couplings: watcher-scores/discovery-ranks/snapshot must describe *approximately*
  the same ranking epoch — tolerated drift, no enforcement; DropRules map isolation invariant
  (race test); nil-publishes clear consumers back to pre-policy behavior.

**Open Questions:**
- `buildPolicyInputs` L70 dereferences `m.watcher` without the nil guard refreshPolicy itself
  uses at L53. Is a nil-watcher-with-non-nil-dropsTracker Miner constructible outside
  `startMining` (tests build such states by hand, e.g. rename_reconcile_test.go:179-180 sets
  both explicitly)? Only construction order protects production.
- `policySnapshot.byID` is stored (L59) but never read from the stored snapshot anywhere in
  the repo — dead weight, or reserved for a planned by-ID consumer?
- `eligibleLiveChannels` (L128-145) sums configured online streamers carrying the campaign and
  non-offline discovery channels of the same game — can one physical channel be counted twice
  (configured AND in the discovery pool)? miner.go:1013-1015 says discovery "never duplicates a
  channel the rotation already watches", but that claims slot behavior, not pool membership.
- The three publishes are not versioned; is any consumer sensitive to observing watcher scores
  from epoch N+1 while `PolicySnapshot` still serves epoch N (UI-only today, but the Drops page
  merges `PolicySnapshot` decisions with `CurrentCampaignPolicy` rules sampled separately —
  see CurrentCampaignPolicy below)?

---

## PolicySnapshot in internal/miner/policy.go (L247-L253)

**Purpose:** Read-side accessor for the last published ranked-decision snapshot, exposed to the
web layer as part of `web.PolicyProvider` (server.go:112-117) and used by the debug snapshot
builder (debug.go:306). Doc (L244-246): returns an empty snapshot before the first refresh.

**Inputs & Assumptions:**
- No parameters. Implicit state: `m.policySnap` (`atomic.Pointer[policySnapshot]`,
  miner.go:129). Nil until the first `refreshPolicy` completes L59.
- Precondition: none — safe on a zero-value/un-started Miner. Enforcer:
  `provider_safety_test.go` L228 exercises `m.PolicySnapshot()` on an un-started miner.
- Assumes callers treat the returned `[]policy.Decision` as read-only: it is the **same slice**
  stored in the snapshot (L252 returns `s.Decisions` without copying), shared with every other
  concurrent reader and with the snapshot itself. Enforcer: **nothing found** — convention
  only; current consumers (buildDropPolicyByCampaign handlers_policy.go:31-82, debug.go:306-324)
  only read.

**Outputs & Effects:**
- Returns `(policy.Mode, []policy.Decision)`. Before first publish: `(policy.DefaultMode, nil)`
  (L249-251; DefaultMode = GAME_ORDER, policy/policy.go:38).
- No state writes, no persistence, no publishes. Lock at read point: **none** — single
  `atomic.Pointer.Load` (L248). MUTATION/PERSISTENCE/PUBLISH points: none in this function.
- Error propagation: none possible (no error path).

**Block-by-Block:**
- L248-252
  ```go
  s := m.policySnap.Load()
  if s == nil {
      return policy.DefaultMode, nil
  }
  return s.Mode, s.Decisions
  ```
  What: load-and-unwrap with empty-state fallback. Why here: the Drops page and debug endpoint
  poll this from HTTP handler goroutines while refreshPolicy publishes from the watchdog/apply
  goroutines; atomic pointer swap gives torn-free reads without touching `m.mu`. Assumes: a
  published `*policySnapshot` is immutable after store. Establishes: nil-safe default so the
  UI renders "no decisions" honestly before the first tick (`len(decisions) > 0` gates at
  debug.go:306; `len(decisions) == 0 → nil` map at handlers_policy.go:32-34). Depended on by:
  `renderDropsList` (handlers_drops.go:237), debug snapshot (debug.go:306), and the untracked
  fail-closed test's "no side effect from a rejected value" assertion (policy_persistence_
  fail_closed_test.go:75-78).

**Cross-Function Dependencies:**
- Callees: only `atomic.Pointer.Load`.
- Callers: `web.(*Server).renderDropsList` via the `policyProvider` field (handlers_drops.go:
  214-240 — provider registered by `SetPolicyProvider(m)` at miner.go:1079, stored under the
  web server's own `s.mu`, server.go:499-503; providers are **never cleared** on generation
  retirement, miner.go:2059-2062, so a retired generation keeps serving reads);
  `m.buildDebugSnapshot` (debug.go:306); tests (provider_safety, stale_generation_fence,
  policy_persistence_fail_closed [untracked]).
- Shared state: the published snapshot object — shared by reference with all concurrent readers.
- Invariant couplings: pairs with `CurrentCampaignPolicy` in `renderDropsList` (two separate,
  non-atomic samples — decisions from the snapshot, rules from the live config; a rule change
  between the two calls yields a mixed view for one render).

**Open Questions:**
- The returned slice aliases the live snapshot; is any future consumer expected to sort/annotate
  it in place (buildDropCampaignViews copies before sorting for campaigns, handlers_drops.go:
  613-614, but no such defensive copy exists for decisions)?

---

## snapshotDropRules in internal/miner/policy.go (L260-L268)

**Purpose:** Produce a private, race-free copy of `m.config.DropRules` under the miner read
lock so callers (refreshPolicy's input assembly; CurrentCampaignPolicy's web reads) can consume
the rules without holding `m.mu` while `SetDropRule`/`commitDropRule` mutate the shared map in
place under the write lock (doc L255-259: sharing the reference would be a fatal concurrent map
read/write).

**Inputs & Assumptions:**
- No parameters. Implicit state: `m.config.DropRules map[string]config.DropRule` — may be nil
  (config default; commitDropRule lazily allocates at policy.go:327-329).
- Precondition: `m.config` non-nil. Established by: Miner construction (config is set at
  NewMiner/generation start; every production Miner has one). No explicit guard —
  enforcer for the un-started path is `provider_safety_test.go` L229 (CurrentCampaignPolicy →
  snapshotDropRules on an un-started miner must not panic), which passes because ranging a nil
  map is a no-op.
- Trust: keys/values are operator-shaped data (normalized reward keys, bool flags) — no
  validation needed here; normalization happened at write time (SetDropRule L313,
  `models.NormalizeRewardKey` at read sites).

**Outputs & Effects:**
- Returns a freshly allocated `map[string]config.DropRule` with every entry copied by value
  (`config.DropRule` = five bools, config/config.go:217-231 — value copy is deep copy).
  Note: a nil source map yields an allocated **empty** (non-nil) map, not nil (L263 always
  `make`s) — callers cannot distinguish "no rules" from "empty rules" through this accessor
  (contrast `snapshotConfigLocked`, miner.go:2723-2728, which preserves nil-ness).
- Lock at read (MUTATION-adjacent) point: `m.mu.RLock()` held for the whole copy (L261-262,
  deferred unlock). This function itself has no MUTATION, PERSISTENCE, or PUBLISH point — it
  is the read half of the map-isolation protocol; the paired mutation
  (`commitDropRule` L326-336) and persistence (`persistLocked` under the same write-lock hold)
  live in policy.go's write path.
- Error propagation: none possible.

**Block-by-Block:**
- L261-267
  ```go
  m.mu.RLock()
  defer m.mu.RUnlock()
  out := make(map[string]config.DropRule, len(m.config.DropRules))
  for k, v := range m.config.DropRules {
      out[k] = v
  }
  return out
  ```
  What: locked full-map value copy. Why here: the only two read paths for DropRules outside the
  write lock (policy inputs, web rules display) both need a stable view that survives the lock
  release. Assumes: DropRule stays a plain value type (adding a reference-typed field would
  silently break the deep-copy property — enforcer for that future hazard: **nothing found**;
  `snapshotConfigLocked`'s comment miner.go:2685-2694 documents the three-map inventory but
  nothing asserts DropRule's field shapes). Establishes: caller-owned map, no aliasing with the
  live config. Depended on by: `refreshPolicy` L42 (then read lock-free in buildPolicyInputs
  L102-108), `CurrentCampaignPolicy` L276, and `policy_race_test.go` L77 (the regression pin).

**Cross-Function Dependencies:**
- Callees: none beyond runtime map ops.
- Callers: `refreshPolicy` (L42), `CurrentCampaignPolicy` (L276), race test.
- Shared state: `m.mu` + `m.config.DropRules`; writers: `commitDropRule` (policy.go:326-336,
  write lock), `applySettings` path (via cloneConfigLocked's deliberate DropRules ALIASING,
  miner.go:2708-2713 — the live map object survives a settings apply precisely so concurrent
  SetDropRule commits are not lost), generation handoff copies via `snapshotConfigLocked`
  (miner.go:2723-2728).
- Invariant couplings: "no shared DropRules map crosses the m.mu boundary unlocked" — this
  function is the sole sanctioned exit; `policy_race_test.go` enforces it under `-race`.

**Open Questions:**
- Nil-to-empty flattening: `CurrentCampaignPolicy` (and thus the web UI) receives `{}` for a
  nil rules map while `CurrentConfig`/`snapshotConfigLocked` preserve nil — intentional
  asymmetry (display vs. persistence semantics) or accident? The untracked fail-closed test
  makes nil-ness load-bearing on the config path (policy_persistence_fail_closed_test.go:
  124-130) but not on this accessor.

---

## CurrentCampaignPolicy in internal/miner/policy.go (L272-L277)

**Purpose:** Read-side accessor for the Drops-page controls: the active (normalized) policy
mode string plus a private copy of the per-drop rules. Second read method of
`web.PolicyProvider` (server.go:114).

**Inputs & Assumptions:**
- No parameters. Implicit state: `m.config.CampaignPolicy` (string) and `m.config.DropRules`,
  both guarded by `m.mu`.
- Precondition: `m.config` non-nil (same establishment as snapshotDropRules). Enforcer for
  un-started safety: `provider_safety_test.go` L229.
- Assumes `policy.Normalize` is the single source of mode validity: a stored garbage/legacy
  value is presented to the UI as the fallback `GAME_ORDER`, exactly as refreshPolicy ranks it
  (both call Normalize on the same raw field — L32 and L274 — keeping display and behavior
  consistent by construction).

**Outputs & Effects:**
- Returns `(string, map[string]config.DropRule)`: normalized mode; caller-owned rules copy
  (always non-nil, see snapshotDropRules).
- Lock at read points: `m.mu.RLock()` for the mode (L273-275), released, then a **second,
  separate** `m.mu.RLock()` inside `snapshotDropRules()` (L276 → L261). The two samples are
  not atomic together: a concurrent `ApplyCampaignPolicy`/`commitDropRule` landing between the
  two RLock windows yields a mode from one epoch and rules from the next. Enforcer of
  combined-atomicity: **nothing found** — tolerated by design for a display accessor
  (consumers re-poll; renderDropsList already mixes this with a third sample, PolicySnapshot).
- No MUTATION, PERSISTENCE, or PUBLISH point; no side effects; no error path.

**Block-by-Block:**
- L273-276
  ```go
  m.mu.RLock()
  mode := string(policy.Normalize(m.config.CampaignPolicy))
  m.mu.RUnlock()
  return mode, m.snapshotDropRules()
  ```
  What: sample-normalize-release, then delegate the rules copy. Why here: keeps each RLock hold
  minimal and reuses the one sanctioned map-copy path instead of a second inline copy. Assumes:
  torn mode/rules pairs are acceptable (see above). Establishes: web layer never sees a raw
  un-normalized mode string and never holds a reference into the live config. Depended on by:
  `handleDropsPage` (handlers_drops.go:38-40 — mode only, to pre-select the mode dropdown
  against the `PolicyModes` list L49-52), `renderDropsList` (handlers_drops.go:238 — rules
  only, merged with PolicySnapshot decisions into `buildDropPolicyByCampaign`,
  handlers_policy.go:31-82, keyed by `models.NormalizeRewardKey`), and generation/fence tests
  (stale_generation_fence_test.go:216,252,384,465; generation_config_test.go:417-502;
  lifecycle_replacement_gap_test.go:270).

**Cross-Function Dependencies:**
- Callees: `policy.Normalize` (policy/policy.go:51-57), `snapshotDropRules` (L260-268).
- Callers (production): `web.(*Server).handleDropsPage` and `renderDropsList` via
  `policyProvider` (registered miner.go:1079; read under the web server's `s.mu`,
  handlers_drops.go:215-219 / 30-35). renderDropsList itself is reached from `handleAPIDrops`
  (handlers_drops.go:60) and, fail-closed on error, from the two mutating handlers
  `handleAPIPolicyMode` / `handleAPIPolicyDropRule` (handlers_policy.go:125,190) — on a
  provider error those handlers return 503/500 via `writePolicyMutationError`
  (handlers_policy.go:137-143) **without** re-rendering, so a refused mutation is never painted
  as success (comment handlers_policy.go:110-119).
- Shared state: `m.mu`, `m.config`.
- Invariant couplings: display/behavior consistency through shared Normalize; retired
  generations keep answering these reads forever (providers never cleared, miner.go:2059-2062)
  while their mutators fail closed via `fenced` — reads from a retired generation serve the
  frozen last state, by design (server.go:108-111).

**Open Questions:**
- `renderDropsList` samples decisions (PolicySnapshot, atomic cell) and rules
  (CurrentCampaignPolicy, live config) in two calls (handlers_drops.go:237-238); after a
  SetDropRule commit but before its trailing refreshPolicy publishes, one render can show the
  new rule flags beside decisions ranked under the old rules. Accepted UI staleness or worth a
  combined snapshot?
- The mode string returned is upper-case normalized; the untracked fail-closed work
  (policy_persistence_fail_closed_test.go:66-69) additionally treats this accessor as the
  oracle for "prior committed state" after a failed persist — at HEAD a failed persist leaves
  the *rejected* value live here (persistLocked swallows the error, policy.go:222-225), i.e.
  this accessor currently reports in-memory truth, not durable truth.
