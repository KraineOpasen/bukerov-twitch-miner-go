# Config snapshot/clone family — internal/miner/miner.go @ HEAD 5b331e5

Scope: `CurrentConfig` (L2072-2076, doc L2033-2071), `cloneConfigLocked` (L2671-2680, doc
L2649-2670), `snapshotConfigLocked` (L2715-2736, doc L2682-2714). All three are infallible
(no error returns); the audit-relevant error paths belong to their callers and to the
writers whose behavior their doc comments describe.

Repo-state note for auditors: `internal/miner/policy_persistence_matrix_test.go`,
`internal/miner/policy_persistence_fail_closed_test.go`, `internal/web/policy_persistence_status_test.go`
and `docs/plans/policy-persistence-fail-closed.md` are UNTRACKED (git status) — they describe a
*planned* fail-closed rework of `persistLocked`/`SetDropRule`/`ApplyCampaignPolicy` that is NOT
implemented at HEAD 5b331e5. At HEAD, `persistLocked` (policy.go:220-226) only logs a
`config.SaveConfig` failure, exactly as `CurrentConfig`'s doc (miner.go:2048-2052) states. Any
claim below about the in-place writers describes committed HEAD behavior, not the plan.

---

## CurrentConfig in internal/miner/miner.go (L2072-L2076)

**Purpose:** Return an ISOLATED SNAPSHOT of the configuration this miner generation is
currently running with, so the process-level composition root (`internal/app`) can seed the
NEXT generation from the last committed runtime config instead of the config the process
booted with (miner.go:2033-2037). Without it, a successful runtime settings change would
silently revert at the next pause/resume/restart. The isolation (no shared map) is a
memory-safety contract, not tidiness: two `*Miner` generations can be live at once (the web
providers a generation registers are never cleared, miner.go:2059-2062), and one map behind
two different mutexes is a `concurrent map read and map write` fatal runtime throw, not a
recoverable race (miner.go:2065-2071).

**Inputs & Assumptions:**
- No parameters. Implicit state: `m.config *config.Config` (the live config) and
  `m.mu sync.RWMutex` (miner.go:274).
- Precondition: none beyond a constructed `Miner`. Callable on a live OR retired
  generation — retirement does not fence reads, only mutations (`fenced`,
  miner.go:2135-2141, fences writers; nothing fences this reader). `internal/app`
  deliberately calls it on the OUTGOING generation after its Run returned
  (app.go:707-711).
- Assumes `m.config` is never nil. Establishes: constructor/`Run` wiring; nothing checks
  it here — nothing found as a mechanical enforcer inside this function (a nil `m.config`
  would panic at `*m.config` in snapshotConfigLocked, miner.go:2716).
- Ordering assumption the handoff depends on (documented at `fenced`, miner.go:2123-2127):
  any mutation answered successfully before retirement is visible to this sample, because
  an admitted mutation holds `applyWG`, teardown waits on it before Run returns, and the
  lifecycle reaches `nextGenerationConfig`'s `CurrentConfig` sample only after that return.

**Outputs & Effects:**
- Returns `*config.Config`: a fresh struct value, shallow copy of `*m.config` with the
  three maps deep-copied (see snapshotConfigLocked). Slices remain shared with the live
  object (see snapshotConfigLocked's slice argument).
- No state writes, no persistence, no side effects, no error.
- Postcondition: returned object shares NO map with `m.config` (pinned per-map by
  current_config_test.go:23-112) and preserves nil-ness of each map
  (current_config_test.go:117-130 — matters because the receiving generation persists the
  snapshot verbatim and `omitempty` drops nil maps).
- Locks for this audit's checklist: MUTATION point — none (nothing mutated).
  PERSISTENCE point — none. PUBLISH point — the snapshot is published by return value;
  the one production caller stores it into `a.cfg` under `a.cfgMu` (app.go:713-718),
  *after* `m.mu` has been released — no lock is held across the handoff, which is safe
  precisely because the returned object is private. Error propagation: none possible.

**Block-by-Block:**
- L2072-2076:
  ```go
  func (m *Miner) CurrentConfig() *config.Config {
      m.mu.RLock()
      defer m.mu.RUnlock()
      return m.snapshotConfigLocked()
  }
  ```
  What: take the read lock, build the snapshot, release. Why here: the snapshot must be
  built atomically w.r.t. every config writer (all of which hold `m.mu.Lock` at their
  mutation+persist+publish points: miner.go:2309-2321, 2434-2450, 2583-2616;
  health.go:459-472; policy.go:287-290, 326-336; rewards.go:173-225), and it mirrors the
  same "Current" + RLock discipline as `CurrentHealthSettings` (health.go:416-420) and
  `CurrentCampaignPolicy` (policy.go:272-277) (miner.go:2038-2039). RLock (not Lock)
  suffices because the function only reads and the fresh maps are private until return.
  Assumes: snapshotConfigLocked's own precondition (lock held) — satisfied here.
  Establishes: the no-shared-map postcondition. Depended on by:
  `App.nextGenerationConfig` (app.go:707-719), which makes the returned value the seed
  config for the next generation; tests current_config_test.go,
  stale_generation_fence_test.go:426/485, lifecycle_replacement_gap_test.go:132.

**Doc-comment claims about the in-place writers (miner.go:2041-2055) — verified at HEAD:**
- Claim: candidate-publishing paths (`applySettingsNoRename` / `applySettingsWithRemovals`
  / `applySettingsWithRename`, `ApplyHealthSettings`) publish into `m.config` only past
  their commit point, so a candidate whose persistence failed is never observable here.
  Verified: each returns before `m.config = candidate` on SaveConfig failure
  (miner.go:2315-2320, 2439-2445, 2599-2606; health.go:465-471).
- Claim: `ApplyCampaignPolicy` and `SetDropRule` mutate `m.config` in place under `m.mu`
  and call `persistLocked`, which only LOGS a SaveConfig failure — their changes are live
  and visible here even when the disk write failed. Verified: policy.go:287-290 (mode
  write then `persistLocked`), policy.go:326-336 (`commitDropRule`: map write then
  `persistLocked`), policy.go:220-226 (`persistLocked` logs `slog.Error`, returns
  nothing). So at HEAD these two writers are fail-OPEN w.r.t. persistence, and
  `CurrentConfig` faithfully reports the in-memory (possibly unpersisted) value
  (miner.go:2052-2055 says this is deliberate reporting of existing behavior).
- Claim: the owner-identity reconciliation in `Run` mutates `m.config` "off m.mu
  entirely" and only warns on a failed save. Verified: miner.go:680-693
  (`reconcileOwnerIdentity(m.config, ...)` then `config.SaveConfig` at 690, `slog.Warn`
  at 691, no `m.mu` anywhere in that section). This runs during generation startup,
  before `setupComponents`; nothing found that mechanically prevents a concurrent
  `CurrentConfig` (e.g. from a still-registered web provider of the PREVIOUS generation
  pointing at THIS miner — not possible — or a dashboard read hitting this generation
  before it finished starting) from racing that unlocked write — see Open Questions.
- Claim (miner.go:2057-2071): the snapshot stays load-bearing even though `SetAutoRedeem`
  (rewards.go:166-168) and `SetDropRule` (policy.go:311-320) now fail closed on a retired
  generation via `fenced` (miner.go:2135-2141), because "the two unfenced non-config
  mutators are still live". app.go:700-703 names `RedeemCustomReward` as one such
  non-config method; the doc does not name the second — see Open Questions.

**Cross-Function Dependencies:**
- Callee: `snapshotConfigLocked` (sole caller relationship — grep shows miner.go:2075 is
  its only call site). Depends on it copying every map `config.Config` reaches.
- Caller: `App.nextGenerationConfig` (app.go:707-719) — assumes the result is fully
  private (its doc at app.go:697-698: "no two generations ever share a map") and adopts
  it as `a.cfg` under `a.cfgMu`. Its two documented limits (app.go:700-706): the isolation
  covers configuration only (non-config provider methods still execute on a retired
  generation), and "admitted implies present in this sample" is bounded by applySettings'
  clone window residual.
- Shared state: `m.config` (read), `m.mu` (RLock).
- Invariant couplings: the "anything answered successfully is in the handoff" guarantee
  couples to `beginApply`/`applyWG`/teardown ordering (miner.go:2085-2096, 2123-2127);
  the nil-map preservation couples to `config.SaveConfig`'s `omitempty` round-trip
  (current_config_test.go:114-116).

**Open Questions:**
- Which is the second "unfenced non-config mutator" miner.go:2070 counts alongside
  `RedeemCustomReward` (app.go:702)? Not identified in the sections read.
- The owner-identity reconciliation writes `m.config` fields off `m.mu` (miner.go:680-693).
  What (if anything) guarantees no `CurrentConfig`/other `m.mu` reader is concurrently
  live at that point in `Run`? The web server is process-owned and outlives generations
  (app.go), so a dashboard-driven read racing this unlocked write during a generation's
  startup window is not obviously excluded — nothing found in the read sections; needs a
  dedicated look at when this generation's providers are registered relative to
  miner.go:680.

---

## snapshotConfigLocked in internal/miner/miner.go (L2715-L2736)

**Purpose:** Produce a copy of `m.config` that shares NO MAP with the live object, for
handing across a generation boundary (miner.go:2682-2683). It is the mechanism that makes
`CurrentConfig`'s isolation contract true.

**Inputs & Assumptions:**
- No parameters. Reads `m.config` only.
- Precondition: "Caller holds m.mu (read or write)" (miner.go:2714). Enforcer: nothing
  found mechanically — Go mutexes are not assertable; the convention is carried by the
  `Locked` suffix and the single call site (`CurrentConfig`, under RLock). A future
  lock-free caller would compile silently.
- Assumes `config.Config` reaches EXACTLY three maps: `AutoRedeem`
  (config.go:196, `map[string]AutoRedeemConfig`), `DropRules` (config.go:211,
  `map[string]DropRule`), and `Notifications.ProviderBatching` (config.go:423,
  `map[string]BatchingSettings`, reached through the by-value `Notifications` struct
  field). Verified true at HEAD by inspection of `type Config struct`
  (config.go:65-212) — every other field is bool/string/int/float, a value struct of
  those, or a slice. Enforcer for the FUTURE ("a fourth map added later is also
  copied"): nothing found — no reflection-based inventory test exists (grep for
  `reflect.(TypeOf|Kind|Map)` in internal/miner: no matches); only the three
  per-map tests in current_config_test.go pin the current set.
- Assumes slices need no copy because no writer can touch them after the handoff
  (miner.go:2696-2703): every slice writer either reassigns wholesale
  (`settings.ApplyToConfig` — verified: builder.go:200, 204-207, 233-236 assign fresh
  slices for Streamers, Priority, DropBlacklist, DirectoryGames, DropCampaignGameIDs,
  DropCampaignGames; it never touches AutoRedeem/DropRules/ProviderBatching) or mutates
  elements only on the `applySettings*` path, which `beginApply`/`applyWG` drain to
  completion before Run returns and refuse once `runCtx` is cancelled
  (miner.go:2085-2096). Enforcer: the drain protocol itself; no per-slice test found.

**Outputs & Effects:**
- Returns `*config.Config`: `snap := *m.config` (shallow struct copy, L2716), then three
  guarded map rebuilds:
  - AutoRedeem deep-copied L2717-2722 (nil preserved by the guard);
  - DropRules deep-copied L2723-2728 (nil preserved);
  - Notifications.ProviderBatching deep-copied L2729-2734 (nil preserved).
- COPIED (per this audit's inventory): the three maps above — keys and struct values.
- ALIASED (still shared with live config): ALL slices — the `Streamers` backing array and
  each element's `Settings *models.StreamerSettings` pointer (config.go:252-253),
  `Priority`, `DropBlacklist`, `DropCampaignGameIDs`, `DropCampaignGames`,
  `DirectoryGames`, each copied `AutoRedeemConfig` value's `RewardIDs` slice
  (config.go:248 — the map value is copied by value, its slice header still points at the
  live backing array), `Notifications.Batching.ImmediateEvents` (config.go:444), and each
  copied `ProviderBatching` value's own `ImmediateEvents`. This matches the doc's own
  inventory (miner.go:2696-2699). Rationale for the asymmetry (miner.go:2703-2706): a
  shared slice element is an ordinary data race; a shared map is an unrecoverable fatal
  throw — "the maps get copies and these get an argument".
- No mutation of `m.config`, no persistence, no publish, no error. MUTATION point: only
  the private `snap` locals, under the caller's `m.mu` (RLock in the only caller).
  PERSISTENCE/PUBLISH: none here; the snapshot is later persisted by the RECEIVING
  generation's own writers, under the receiving miner's own locks.

**Block-by-Block:**
- L2716 `snap := *m.config` — What: shallow value copy of the whole struct. Why: cheap
  isolation for every plain-value field. Assumes: caller holds m.mu so the struct is not
  torn mid-copy. Establishes: value-field isolation; map/slice headers still aliased.
  Depended on by: the three blocks below, which fix up exactly the map headers.
- L2717-2722 AutoRedeem copy — Why here: `SetAutoRedeem` (rewards.go:191/193) writes this
  live map in place under `m.mu`; a shared map across generations = fatal throw. Pinned
  by TestCurrentConfigSnapshotIsolatesAutoRedeem (current_config_test.go:23-57).
- L2723-2728 DropRules copy — Why here: `commitDropRule` (policy.go:327-334) writes the
  live map in place under `m.mu`. Pinned by TestCurrentConfigSnapshotIsolatesDropRules
  (current_config_test.go:59-82). Note the deliberate contrast with cloneConfigLocked,
  which ALIASES this map (below).
- L2729-2734 ProviderBatching copy — Why here: no in-place writer exists today (the
  notifications package deep-copies on ingest, miner.go:2688-2690), but it is reached
  through a by-value struct field so the shallow copy would alias it; copying keeps the
  "no shared map" argument true BY CONSTRUCTION rather than by an inventory of current
  writers (miner.go:2690-2694). Pinned by
  TestCurrentConfigSnapshotIsolatesProviderBatching (current_config_test.go:84-112).
- Each block's `if != nil` guard — Establishes nil-preservation, load-bearing because the
  receiving generation persists the snapshot verbatim and `omitempty`
  (config.go:196/211/423) must keep absent maps absent on disk
  (current_config_test.go:114-116).

**Cross-Function Dependencies:**
- Caller: `CurrentConfig` only (miner.go:2075). It supplies the lock precondition.
- Must NOT be merged with `cloneConfigLocked` (miner.go:2708-2714): the two functions want
  OPPOSITE things from `DropRules` — snapshot wants isolation (cross-generation memory
  safety), clone wants aliasing (intra-generation lost-update prevention, [R7]). A
  "unification" would break one or the other silently.
- Invariant couplings: completeness of the three-map inventory ↔ `config.Config`'s field
  set (config.go:65-212); slice-safety ↔ the applyWG drain protocol
  (miner.go:2085-2096, 2123-2127) and `settings.ApplyToConfig`'s reassign-wholesale style
  (builder.go:199-249).

**Open Questions:**
- No mechanical guard exists for "a fourth map field added to config.Config (or to any
  by-value struct it embeds) gets copied here" — the doc argues by construction for
  ProviderBatching but the construction is manual. Is a reflection-walk test (fail on any
  un-copied `reflect.Map` reachable by value) wanted? (Understanding note only, no
  verdict.)
- `StreamerConfig.Settings` is a shared *pointer* into live data (config.go:253). The
  slice argument (drained writers) covers it only if nothing outside the applySettings*
  path ever writes through an element's Settings pointer after handoff — nothing found
  that either confirms or mechanically enforces the "nothing outside" half for the
  pointee (as opposed to the slice header).

---

## cloneConfigLocked in internal/miner/miner.go (L2671-L2680)

**Purpose:** Produce a candidate copy of `m.config` that is "safe to mutate independently
of the live one" (miner.go:2649-2650) for the pre-commit phase of the four
candidate-publishing apply paths — NOT for cross-generation handoff. Its copy depth is
tuned to exactly what pre-commit candidate mutation touches in place: only `AutoRedeem`.

**Inputs & Assumptions:**
- No parameters; reads `m.config`. Precondition: "Caller holds m.mu" (miner.go:2670) —
  write lock at all four call sites (miner.go:2309-2310, 2390-2391, 2500-2501;
  health.go:459-460). Enforcer: nothing found mechanically; convention + call-site
  inspection.
- Assumes AutoRedeem is the ONLY reference-typed field any pre-commit candidate mutation
  touches IN PLACE (miner.go:2650-2653): the toucher is `applyConfigRenames`' early
  `migrateAutoRedeem` pass (rename_reconcile.go:22-26, 62, 88-96), which runs OFF-lock on
  the candidate (rename_reconcile.go:29-34) — without the deep copy it would write the
  LIVE map off-lock. Verified for the settings path: `settings.ApplyToConfig`
  (builder.go:199-249) reassigns slices wholesale and never touches
  AutoRedeem/DropRules/ProviderBatching. Enforcer that this stays true: nothing found
  beyond the doc comments.
- Assumes the AutoRedeem copy "only needs to survive mutation up to the commit point, not
  to publish" (miner.go:2655-2657): on the removal and rename paths it is rebuilt
  wholesale from the LIVE map at the commit point by `refreshCandidateAutoRedeemLocked`
  (rewards.go:459; called at miner.go:2435 and 2584 under `m.mu`); on the NoRename and
  health paths no rebuild is needed because clone→publish is one uninterrupted `m.mu`
  critical section (miner.go:2259-2281; health.go:459-472).

**Outputs & Effects:**
- Returns `*config.Config`: `clone := *m.config` (L2672) plus a deep copy of AutoRedeem
  only (L2673-2678, nil preserved by the guard).
- COPIED: `AutoRedeem` map (keys + `AutoRedeemConfig` values; each value's `RewardIDs`
  slice header still aliases the live backing array — same aliasing note as the
  snapshot).
- ALIASED: `DropRules` (deliberately, [R7] — see below), `Notifications.ProviderBatching`
  (implicitly — safe today only because nothing pre-commit mutates it and no in-place
  writer exists at all, per miner.go:2686-2690), and every slice
  (Streamers/Priority/DropBlacklist/DropCampaignGameIDs/DropCampaignGames/DirectoryGames/
  ImmediateEvents) until `ApplyToConfig` reassigns the settings-owned ones on the
  candidate.
- No mutation of `m.config`, no persistence, no publish, no error — the function itself
  is pure construction. For this audit's checklist, the points live in its CALLERS:
  - MUTATION of the candidate: NoRename/health paths — under `m.mu.Lock`
    (miner.go:2311-2313; health.go:461-463); removal/rename paths — deliberately
    OFF-lock (miner.go:2393-2404 + 2395, 2504-2528) on a candidate that is private
    except for its aliased maps/slices, which that off-lock phase never touches (the
    rename path's `applyConfigRenames` touches only the DEEP-COPIED AutoRedeem,
    rename_reconcile.go:29-34).
  - PERSISTENCE of the candidate: `config.SaveConfig(candidate)` under `m.mu.Lock` at
    ALL four commit points — miner.go:2314-2318, 2438-2444, 2598-2605,
    health.go:464-470. This is a hard invariant ([R7]/M1, miner.go:2190-2194,
    2273-2276, 2577-2579): never move a candidate's SaveConfig off m.mu.
  - PUBLISH: `m.config = candidate` in the SAME critical section, immediately after the
    successful save — miner.go:2320, 2445, 2606; health.go:471.
  - Error propagation: a SaveConfig failure at any of the four commit points unlocks,
    compensates (rename: `rollback()` of analytics renames + `abortAdmittedRemovals`,
    miner.go:2600-2603; removals: `abortAdmittedRemovals`, miner.go:2440-2442) and
    returns a wrapped error to the HTTP/settings caller BEFORE any publish — fail
    closed, "no changes were made" literally true (miner.go:2317, 2442, 2603;
    health.go:468).
  - Dependent side effects fire strictly AFTER persistence+publish: `CommitPlan` +
    `finishApply` (miner.go:2323-2330, 2458-2463, 2626-2631), AutoRedeem
    state-delete/generation-bump inside the same critical section as the publish
    (miner.go:2446-2449, 2607-2615), canary/watchdog updates after the health publish
    (health.go:474-490).

**Block-by-Block:**
- L2672 `clone := *m.config` — shallow value copy; same semantics as the snapshot's first
  line. Establishes: value-field independence; every reference field aliased.
- L2673-2678 AutoRedeem deep copy — What: rebuild the map, copy each entry. Why here:
  `applyConfigRenames`→`migrateAutoRedeem` mutates `candidate.AutoRedeem` in place,
  OFF-lock, before the commit (miner.go:2513-2518; rename_reconcile.go:62); without a
  private map that would be an unlocked write to the live map racing `SetAutoRedeem`.
  Assumes: this early migrate result is discardable (it is — rebuilt by
  `refreshCandidateAutoRedeemLocked` at the commit point, rename_reconcile.go:23-26).
  Depended on by: applySettingsWithRename's off-lock phase; the NoRename path's
  "current by construction" argument (miner.go:2278-2281).
- L2679 `return &clone` — hands the candidate to the caller; from here candidate privacy
  is the caller's responsibility.
- What is deliberately ABSENT (miner.go:2661-2670, the [R7] block): no DropRules copy.
  See next paragraph.

**Why the DropRules ALIASING is load-bearing ([R7]), not an oversight:**
Two coupled reasons, one per direction of the hazard:
1. *Lost-update prevention (why a private copy would be a BUG):* `SetDropRule` commits
   into the LIVE map under `m.mu` (policy.go:326-336). The removal and rename apply paths
   hold the candidate across an UNLOCKED window (durable admission, analytics rename —
   miner.go:2417-2429, 2540-2566). A candidate carrying a private DropRules copy frozen at
   clone time would, at `m.config = candidate`, silently overwrite any SetDropRule that
   committed (in memory AND on disk) during that window — the exact lost-update class
   `refreshCandidateAutoRedeemLocked` exists to prevent for AutoRedeem
   (miner.go:2708-2713). Aliasing solves it structurally: the candidate carries the SAME
   map object, so every SetDropRule write up to the commit-point critical section is in
   the candidate by identity, and the publish cannot lose it. (`ApplyToConfig` never
   reassigns DropRules — builder.go:199-249 — so the alias survives candidate
   preparation.)
2. *The safety obligation aliasing creates (the M2/D7 panic):* because the candidate's
   DropRules IS the live map, `config.SaveConfig(candidate)`'s `json.MarshalIndent`
   iterates a map that `SetDropRule` concurrently writes under `m.mu`. Run the save OFF
   `m.mu` and that is "concurrent map iteration and map write" — a real, unrecoverable
   runtime panic the M2/D7 fix eliminated by moving the rename path's SaveConfig under
   `m.mu` (miner.go:2664-2669, 2568-2579). Hence the standing invariant: a candidate's
   `config.SaveConfig` must never move back off `m.mu` (miner.go:2669-2670, 2577-2579).
   The aliasing and the save-under-lock rule are one invariant with two halves — remove
   either alone and you get either lost updates (copy without refresh) or a fatal panic
   (alias with unlocked save).

**Cross-Function Dependencies:**
- Callers (all hold `m.mu.Lock` at the call): `applySettingsNoRename` (miner.go:2310 —
  whole candidate lifetime one critical section, so no refresh needed, miner.go:2259-2281),
  `applySettingsWithRemovals` (miner.go:2391 — unlocks for admission I/O, relies on
  commit-point `refreshCandidateAutoRedeemLocked` for AutoRedeem and on [R7] aliasing for
  DropRules), `applySettingsWithRename` (miner.go:2501 — same, plus off-lock
  `applyConfigRenames` mutating the deep-copied AutoRedeem), `applyHealthSettings`
  (health.go:460 — single critical section like NoRename, serialized end-to-end by
  `healthApplyMu`, health.go:441-457).
- Coupled functions: `refreshCandidateAutoRedeemLocked` (rewards.go:459) — the commit-time
  authority for candidate.AutoRedeem; `applyConfigRenames`/`migrateAutoRedeem`
  (rename_reconcile.go:35-64, 88+) — the pre-commit AutoRedeem toucher that makes the deep
  copy necessary; `SetDropRule`/`commitDropRule` (policy.go:311-338) — the concurrent
  writer the aliasing accommodates; `snapshotConfigLocked` — the deliberate opposite,
  never to be merged (miner.go:2708-2714).
- Known residual (documented, not a discovery): live VALUE fields (`Health`,
  `CampaignPolicy`) written by `ApplyHealthSettings`/`ApplyCampaignPolicy` during a
  removal/rename apply's unlocked window are silently overwritten at that apply's publish
  — the shallow copy snapshotted them at clone time (miner.go:2204-2209; app.go:704-706).
  DropRules escapes this residual precisely because of [R7]; AutoRedeem escapes it via
  the refresh; nothing else does.

**Open Questions:**
- `Notifications.ProviderBatching` is aliased by the clone. Today safe (no writer at all,
  miner.go:2686-2690), but unlike snapshotConfigLocked this function's safety-by-
  construction stance does NOT extend to it: the first in-place ProviderBatching writer
  added under `m.mu` would make the candidate's aliasing exactly as load-bearing/fragile
  as DropRules' — with no [R7]-style comment here flagging it. Is that asymmetry
  intentional (candidate saves already run under `m.mu`, so the panic half is covered) or
  an inventory gap in the clone's doc?
- The removal/rename paths mutate the candidate off-lock while it still aliases the live
  DropRules map — safe only because that off-lock phase provably never touches DropRules
  (`ApplyToConfig` doesn't; `applyConfigRenames` doesn't). Enforcer: nothing found beyond
  the doc comments; a future off-lock candidate mutation of DropRules would be an
  unlocked write racing `commitDropRule`.
