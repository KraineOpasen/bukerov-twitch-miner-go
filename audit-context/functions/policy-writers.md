# Audit context: policy writers (internal/miner/policy.go)

Repo: KraineOpasen/bukerov-twitch-miner-go @ HEAD 5b331e5. Scope: `persistLocked`,
`ApplyCampaignPolicy`, `SetDropRule`, `commitDropRule` — the runtime campaign-policy
mutation path shared by the Drops-page HTTP controls. Understanding only; no verdicts.

Audit-critical summary of the cluster (each claim expanded per function below):

- MUTATION point: `m.config.CampaignPolicy` write at policy.go:288 and
  `m.config.DropRules` map write/delete at policy.go:330-333 — both under the
  **`m.mu` write lock** (Lock at policy.go:287 / policy.go:326).
- PERSISTENCE point: `config.SaveConfig` at policy.go:222 inside `persistLocked`,
  invoked at policy.go:289 and policy.go:335 — under the **same `m.mu` write lock**
  the mutator already holds, AFTER the in-memory mutation.
- PUBLISH point: `m.policySnap.Store(...)` at policy.go:59 inside `refreshPolicy`,
  invoked at policy.go:291 and policy.go:337 — **no lock held** at the Store itself
  (atomic.Pointer, miner.go:129); `refreshPolicy` runs after `m.mu.Unlock()`.
- ERROR propagation: `config.SaveConfig`'s error is **swallowed** at policy.go:222-224
  (`slog.Error` only; `persistLocked` has no return value). Both exported entry points
  return `nil` for every admitted call, so the web handlers
  (internal/web/handlers_policy.go:120, :185) answer HTTP 200 with a re-rendered Drops
  partial even when nothing reached disk. The only non-nil error either method can
  return is `ErrShuttingDown` from the fence (miner.go:2135-2141), raised BEFORE any
  side effect.
- DEPENDENT side effects: `refreshPolicy` fires unconditionally after the persistence
  attempt and after the lock release — it publishes the policy snapshot, pushes watcher
  campaign scores (policy.go:53-55) and discovery game ranks (policy.go:56-58) ranked
  under the possibly-unpersisted value. A later, unrelated successful save (SaveConfig
  is whole-document, config.go:721-739) then writes the unpersisted value to disk
  ("laundering").
- Test-pin state at this HEAD: the committed pin
  `TestInPlaceRuntimeWriteSurvivesAFailedPersist`
  (internal/app/generation_config_test.go:509-574) records the CURRENT fail-open
  behavior and says it SHOULD fail once the writers become fail-closed; the UNTRACKED
  file internal/miner/policy_persistence_fail_closed_test.go (git status `??`) pins the
  opposite, desired fail-closed contract (500 on failed persist, in-memory rollback,
  no snapshot from the rejected value) and is red against this implementation.

---

## persistLocked in internal/miner/policy.go (L220-226)

**Purpose:** Best-effort write of the CURRENT whole live config to disk when a config
path is configured; the single persistence helper for the two in-place policy writers.

**Inputs & Assumptions:**
- No parameters. Implicit state: `m.configPath` (miner.go:83) and `m.config`
  (miner.go:82).
- Precondition "Caller holds m.mu" — stated only in the doc comment (policy.go:219).
  Mechanical enforcer: **nothing found** (Go has no lock-held assertion; both current
  callers do hold the write lock — policy.go:287-290, policy.go:326-336 — but nothing
  stops a future unlocked caller).
- Holding `m.mu` here is load-bearing beyond mutual exclusion of the field write:
  `config.SaveConfig` shallow-copies the config (`toWrite := *config`, config.go:726),
  which ALIASES the live `DropRules` map, then `json.MarshalIndent` iterates it
  (config.go:731). Marshaling off `m.mu` while `commitDropRule` writes that map would
  be "concurrent map iteration and map write" — the fatal throw documented at
  miner.go:2661-2669 ("Never move a candidate's config.SaveConfig back off m.mu").
- `m.configPath == ""` is a documented no-op success mode ("as every other non-fatal
  SaveConfig path already treats it", miner.go:2377-2379).

**Outputs & Effects:**
- Returns nothing. On `SaveConfig` error: logs `"Failed to save config"` at ERROR and
  otherwise does nothing — no return, no flag, no rollback. Postcondition is therefore
  only "a save was ATTEMPTED"; callers cannot distinguish success from failure.
- On success, config.json is replaced atomically (temp + fsync + rename,
  util/file.go:18-43 via config.go:738) with owner-only 0600 permissions and the
  Discord token elided when env-sourced (config.go:726-729).
- Disk I/O (temp-file write, fsync, rename) runs while the caller's `m.mu` write lock
  is held — every `m.mu` reader (e.g. `CurrentConfig` miner.go:2072-2076,
  `snapshotDropRules` policy.go:260-268) blocks for the duration of the save.

**Block-by-Block:**
- L221-225:
  ```go
  if m.configPath != "" {
      if err := config.SaveConfig(m.configPath, m.config); err != nil {
          slog.Error("Failed to save config", "error", err)
      }
  }
  ```
  What: guard on configured path, whole-document save, log-and-continue on error.
  Why here: shared by both policy writers so persistence stays inside their `m.mu`
  critical section. Assumes: caller holds `m.mu`; `m.config` non-nil. Establishes:
  best-effort durability only. Depended on by: `ApplyCampaignPolicy` (policy.go:289)
  and `commitDropRule` (policy.go:335) — its only two callers (verified by grep:
  policy.go:220,289,335 are the only occurrences). `CurrentConfig`'s contract comment
  (miner.go:2048-2055) explicitly documents this swallow: in-place writers' changes
  "are live, and visible here, even when the write to disk failed."

**Cross-Function Dependencies:**
- Callee `config.SaveConfig` (config.go:721-739): marshals the ENTIRE live config —
  which is what makes a later unrelated successful save persist an earlier failed
  mutation's value (the laundering chain pinned by
  policy_persistence_fail_closed_test.go:137-195, untracked).
- Callee `util.WriteFileAtomic` (util/file.go:18): failure injection seam used by
  tests is rename-onto-a-directory (`breakConfigPathForNextSave`,
  cp1_c2_matrix_test.go:48-56).
- Contrast coupling: the candidate-publishing settings paths make SaveConfig the
  commit point and publish nothing on failure (miner.go:2043-2047, 2370-2379);
  `persistLocked` callers mutate FIRST and persist second — the asymmetry the app-level
  pin characterizes (generation_config_test.go:512-518).

**Open Questions:**
- None beyond the unenforced lock precondition recorded above.

---

## ApplyCampaignPolicy in internal/miner/policy.go (L285-294)

**Purpose:** Runtime (no-restart) change of the campaign-policy mode from the Drops
page: fence-admit, normalize, mutate live config, persist, re-rank immediately.

**Inputs & Assumptions:**
- `mode string` — untrusted (raw `r.FormValue("mode")` from
  handlers_policy.go:120). Sanitizer: `policy.Normalize` (policy pkg policy.go:51-57)
  upper-cases/trims and silently substitutes `DefaultMode` for anything invalid — no
  validation error path exists, so the doc word "validates" (policy.go:279) means
  normalize-with-fallback. Enforcer that an arbitrary string cannot corrupt state:
  Normalize's `Valid()` gate.
- Implicit state: `m.config`, `m.configPath`, `m.mu`, the fence state
  (`applyMu`/`applyDraining`/`applyWG`/`runCtx`, miner.go:306-308, 2085-2096), and
  everything `refreshPolicy` reads.
- Precondition: this generation is still the authoritative mutable one — established
  by `fenced` → `beginApply` (miner.go:2135-2141, 2085-2096), which refuses after
  `applyDraining` or `runCtx.Err() != nil`. Admission and retirement are mutually
  exclusive via `applyMu` + teardown's drain (miner.go:2116-2121).

**Outputs & Effects:**
- Returns `ErrShuttingDown` (== `settings.ErrShuttingDown`, miner.go:47,
  settings/dto.go:21) with zero side effects when the fence refuses; otherwise ALWAYS
  `nil` — including when `config.SaveConfig` failed inside `persistLocked`.
- State writes: `m.config.CampaignPolicy` (L288, under `m.mu` write lock); config.json
  (attempted, L289, same lock); `m.policySnap` + watcher scores + discovery ranks (via
  `refreshPolicy`, L291, after unlock).
- Postcondition actually guaranteed by a nil return: the in-MEMORY mode changed and a
  re-rank was published. Durability is NOT guaranteed. The doc comment (policy.go:282-284)
  promises only the converse ("non-nil error means NOTHING was changed"); the web
  caller nonetheless treats nil as full success (200 + re-render,
  handlers_policy.go:109-125). Enforcer that nil implies durable: **nothing found** —
  this is the defect the untracked fail-closed tests pin
  (policy_persistence_fail_closed_test.go:38-86).

**Block-by-Block:**
- L286, L293: `return m.fenced(func() error { ... })` — What: wraps the whole body in
  drain admission. Why here: web providers registered in setupComponents are never
  cleared, so a retired generation stays reachable; refusal must happen in the callee
  (miner.go:2108-2121). Assumes: nothing (refusal precedes all side effects).
  Establishes: an admitted body holds `applyWG`, so teardown waits for it before Run
  returns and the successor generation's `CurrentConfig` sample sees anything answered
  successfully (miner.go:2123-2127). Depended on by: handlers_policy.go:120 (maps the
  refusal to 503 via `mutationRefusedAsUnavailable`, responses.go:69-71) and the fence
  tests (provider_safety_test.go:230-233, stale_generation_fence_test.go:457-462, 491-492).
  NOTE: `fenced` is admission only, NOT serialization against `applySettings`
  (miner.go:2129-2134) — `applyMu` is released before `fn` runs; `fn` executes holding
  only the WaitGroup registration.
- L287-290:
  ```go
  m.mu.Lock()
  m.config.CampaignPolicy = string(policy.Normalize(mode))
  m.persistLocked()
  m.mu.Unlock()
  ```
  What: MUTATION (L288) then PERSISTENCE attempt (L289) in one `m.mu` write-lock
  section. Why here: the write lock serializes against the other config
  writers/readers and keeps SaveConfig's marshal of the aliased DropRules map safe
  (miner.go:2661-2669). Assumes: `persistLocked` reports failure — it does not; a
  failed save leaves the mutated value live with no rollback. Establishes: the new
  mode is visible to every `m.mu` reader (`CurrentCampaignPolicy` policy.go:272-277,
  `CurrentConfig` miner.go:2072-2076, hence the next generation's handoff baton,
  app.go:111-112) regardless of persistence outcome. Depended on by: the laundering
  chain — any later successful whole-document save writes this value.
  Residual (documented, not closed): a commit landing inside `applySettings`' clone
  window can be overwritten by that apply's publish for VALUE fields like
  CampaignPolicy (miner.go:2129-2134, 2205); DropRules survives via deliberate
  aliasing (miner.go:2708-2713).
- L291: `m.refreshPolicy(time.Now())` — What: immediate re-rank + PUBLISH. Why here:
  "visible at once" product behavior (policy.go:279-280); runs after unlock because
  `refreshPolicy` re-acquires `m.mu.RLock` itself (policy.go:31-34) — calling it under
  the write lock would deadlock (sync.RWMutex is not reentrant). Assumes: the value it
  republishes was committed — it fires even when persistence failed, so watcher
  tie-breaks (policy.go:53-55), discovery ordering (policy.go:56-58) and the Drops-page
  snapshot (`policySnap.Store`, policy.go:59 — atomic store, NO lock held) all run
  under the unpersisted mode. Establishes: `PolicySnapshot` (policy.go:247-253)
  reflects the new mode. Depended on by: `renderDropsList` in the handler's success
  path, which re-samples the provider — the re-render "confirming" the change to the
  operator is fed by this publish, not by the persistence result.

**Cross-Function Dependencies:**
- Callees: `fenced` (admission + ErrShuttingDown), `policy.Normalize` (silent
  fallback), `persistLocked` (swallowed persistence), `refreshPolicy` (publish +
  side-effect fan-out to `watcher.SetCampaignScores` / `discovery.SetGameRanks`).
- Callers: `web.Server.handleAPIPolicyMode` (handlers_policy.go:86-126) via the
  `PolicyProvider` interface (server.go:115); assumes error ⇒ nothing changed (correct)
  AND nil ⇒ change succeeded (holds only in memory). Error mapping:
  ErrShuttingDown/database.ErrClosed → 503, anything else → 500
  (writePolicyMutationError, handlers_policy.go:137-143) — today no "anything else"
  can ever be produced. Also internal/app tests (generation_config_test.go:432) and the
  laundering test's repair step (policy_persistence_fail_closed_test.go:175).
- Shared state: `m.config` (with applySettings pipeline, SetAutoRedeem, health
  writers, owner-identity reconciliation), `m.policySnap` (with the minute health
  watchdog tick, miner.go:1849-1852, which also calls `refreshPolicy`).
- Invariant couplings: persist-under-m.mu (miner.go:2375-2376 "the other four
  config-writers … all persist under m.mu too"); fence ordering guarantee
  (miner.go:2123-2127).

**Open Questions:**
- Is the silent Normalize fallback (invalid mode → DefaultMode → persisted → HTTP 200)
  intended UX, given the dashboard only posts values from its own select control but
  the endpoint is reachable directly?

---

## SetDropRule in internal/miner/policy.go (L311-320)

**Purpose:** Set or clear (zero-value rule = "Reset rule") the per-drop override for a
normalized reward key, persist, and re-rank immediately.

**Inputs & Assumptions:**
- `rewardKey string` — untrusted (raw `r.FormValue("rewardKey")`,
  handlers_policy.go:172; the UI round-trips keys produced by
  `models.NormalizeRewardKey` = `lower(gameID)::lower(name)`, models/drop.go:326-328,
  emitted at handlers_policy.go:69). Local normalization L313 (`ToLower(TrimSpace(...))`)
  is idempotent over that format, so miner-side and UI-side keys agree; rule lookup at
  ranking time uses `NormalizeRewardKey` again (policy.go:102).
- `rule config.DropRule` — five bools (config.go:217-233), comparable, so the
  zero-value test in `commitDropRule` L330 is well-defined. Built by the handler from
  checkboxes; zero value when `reset` posted or all unchecked (handlers_policy.go:174-183).
- Precondition: live authoritative generation — established by `fenced`, entered
  BEFORE the empty-key check by explicit design (policy.go:304-310): a retired
  generation must never answer a mutation endpoint with success, even for a no-op.
- Handler-side guard `key != ""` (handlers_policy.go:173) makes the miner-side
  empty-key branch unreachable from the dashboard, but the miner-side check is the
  enforcer for direct/API callers.

**Outputs & Effects:**
- Returns `ErrShuttingDown` (fence refused, zero side effects), or `nil`: either the
  empty-key no-op (L314-316) or a completed `commitDropRule` — which, as with
  ApplyCampaignPolicy, reports nil even when persistence failed inside it.
- All mutation/persistence/publish effects live in `commitDropRule` (below).
- Postcondition on nil (non-empty key): rule set/cleared in the LIVE map and a re-rank
  published; durability not guaranteed. Enforcer that nil implies durable: **nothing
  found** (persistLocked swallows; see cluster summary).

**Block-by-Block:**
- L312, L319: `return m.fenced(func() error { ... })` — same admission semantics and
  dependents as in ApplyCampaignPolicy (see above); pinned for this method at
  provider_safety_test.go:235-238 and stale_generation_fence_test.go:229, 461-462.
- L313-316:
  ```go
  rewardKey = strings.ToLower(strings.TrimSpace(rewardKey))
  if rewardKey == "" {
      return nil
  }
  ```
  What: normalize then silently accept the empty key as a no-op success. Why here:
  inside the fence so the no-op nil is only ever produced by a LIVE generation
  (policy.go:304-310). Assumes: an empty key has "nothing to refuse". Establishes:
  `commitDropRule` receives a non-empty, lowercase, trimmed key. Depended on by:
  `commitDropRule` (performs no key validation of its own).
- L317: `m.commitDropRule(rewardKey, rule)` — What: delegate the whole
  mutate/persist/publish sequence. Why here: keeps the fenced body a single statement
  (policy.go:322-324). Assumes/Establishes: see commitDropRule.

**Cross-Function Dependencies:**
- Callees: `fenced`, `commitDropRule`.
- Callers: `web.Server.handleAPIPolicyDropRule` (handlers_policy.go:148-191, error →
  503/500 via writePolicyMutationError, nil → 200 re-render); tests across
  internal/miner and internal/app (current_config_test.go:63,72;
  policy_race_test.go:60; generation_config_test.go:478,495,552;
  policy_persistence_fail_closed_test.go:161,166 [untracked]).
- Shared state / invariant couplings: `m.config.DropRules` is the map at the center
  of three documented invariants — (a) readers must copy under RLock, never alias
  (`snapshotDropRules` policy.go:260-268, race pin policy_race_test.go:28-62);
  (b) candidate configs deliberately ALIAS it, so every SaveConfig of a candidate must
  run under `m.mu` (miner.go:2661-2670, 2708-2713 — that aliasing is also what makes a
  SetDropRule commit inside applySettings' unlocked window survive the apply's
  publish); (c) cross-generation handoff copies it (`snapshotConfigLocked`,
  miner.go:2682-2694) because two miners driving one map behind two mutexes is a fatal
  throw.

**Open Questions:**
- The nil return for an empty key is indistinguishable from a committed rule at the
  provider seam; the HTTP layer independently skips the call for empty keys, so only
  non-web callers could be misled — is that seam ever exercised outside tests?

---

## commitDropRule in internal/miner/policy.go (L325-338)

**Purpose:** The actual drop-rule mutation: lazy-init the map, set or delete the entry,
persist, re-rank. Exists solely so `SetDropRule`'s fenced body stays one statement; it
takes `m.mu` itself and is therefore deliberately NOT named `...Locked` (policy.go:322-324).

**Inputs & Assumptions:**
- `rewardKey string` — assumed non-empty, lowercase, trimmed. Enforcer: `SetDropRule`
  L313-316, its only caller (unexported, no other call sites in the package). Nothing
  inside commitDropRule re-checks.
- `rule config.DropRule` — zero value means "clear"; comparability required by L330 is
  guaranteed by the all-bool struct (config.go:217-233).
- Assumed calling context: inside a fenced body (drain admission held). Enforcer:
  only-caller structure; **nothing found** mechanically (a future direct caller would
  bypass the fence).

**Outputs & Effects:**
- Returns nothing; cannot signal persistence failure (same swallow as
  ApplyCampaignPolicy).
- State writes, in order: (1) possibly `m.config.DropRules` nil→empty allocation
  (L327-329); (2) entry delete or set (L330-334); (3) config.json attempt (L335);
  (4) after unlock, snapshot/watcher/discovery publish (L337).
- Postcondition: live map reflects the request and a re-rank is published, persisted
  or not. Note the delete branch leaves the map allocated (Go `delete` never returns
  a map to nil), and the lazy init runs even on the delete branch — so a zero-value
  rule against a nil map still flips live `DropRules` from nil to empty (observable
  via `CurrentConfig`, whose snapshots "keep nil maps nil",
  policy_persistence_fail_closed_test.go:124-126; on disk `omitempty` hides an empty
  map, config.go:211).

**Block-by-Block:**
- L326-334:
  ```go
  m.mu.Lock()
  if m.config.DropRules == nil {
      m.config.DropRules = map[string]config.DropRule{}
  }
  if rule == (config.DropRule{}) {
      delete(m.config.DropRules, rewardKey)
  } else {
      m.config.DropRules[rewardKey] = rule
  }
  ```
  What: MUTATION point — map init + entry set/delete under the **`m.mu` write lock**.
  Why here: this map is read by `snapshotDropRules` under RLock (policy.go:260-268)
  and marshaled by every SaveConfig holding `m.mu`; the write lock is what keeps both
  safe (fatal-throw rationale at policy.go:36-41, 255-259, miner.go:2661-2669).
  Assumes: key pre-normalized (SetDropRule). Establishes: the rule is live for the
  next `refreshPolicy` (which reads a COPY via snapshotDropRules, policy.go:42) and
  for `buildDropPolicyByCampaign`'s badge rendering (handlers_policy.go:71-77) via
  `CurrentCampaignPolicy`. Depended on by: policy input assembly (rule application at
  policy.go:102-108), cross-generation handoff (snapshotConfigLocked copies this map,
  miner.go:2686-2687), and the race regression policy_race_test.go:28-62.
- L335: `m.persistLocked()` — What: PERSISTENCE point, **`m.mu` write lock held**,
  AFTER the mutation, error swallowed (see persistLocked). Why here: same critical
  section keeps SaveConfig's map marshal race-free. Assumes: failure handling is
  acceptable as log-only — the assumption the untracked fail-closed tests reject.
  Establishes: best-effort disk state. Depended on by: nothing can depend on its
  outcome — no caller can observe it.
- L336-337:
  ```go
  m.mu.Unlock()
  m.refreshPolicy(time.Now())
  ```
  What: release, then PUBLISH — `refreshPolicy` re-ranks and stores the snapshot
  (`policySnap.Store`, policy.go:59, atomic pointer, **no lock held** at the store;
  internal RLock only for reading mode/games at policy.go:31-34) and fans out to
  watcher/discovery (policy.go:53-58). Why here: must run off the write lock
  (refreshPolicy takes RLock; RWMutex non-reentrant). Assumes: publishing the
  just-written value is always correct — it fires regardless of the L335 outcome, so
  the snapshot, watcher tie-breaks and discovery ordering run under an unpersisted
  rule when the save failed. Establishes: immediate UI visibility (the handler's
  `renderDropsList` re-render shows the rule applied). Depended on by: the Drops page,
  watcher DROPS tie-break, discovery cross-game ordering. Timing note: between L336
  and the Store there is a window where the live config has the new rule but
  `policySnap` still ranks the old state — benign for readers (both are internally
  consistent views), and the minute watchdog tick (miner.go:1852) converges it.

**Cross-Function Dependencies:**
- Callees: `persistLocked` (swallowed error), `refreshPolicy` (publish + fan-out;
  itself depends on `snapshotDropRules`, `dropsTracker.Campaigns`,
  `watcher.BrokerSnapshot`, `policy.Rank`).
- Caller: `SetDropRule` only. It assumes commitDropRule cannot fail — structurally
  true today because commitDropRule has no failure signal at all.
- Shared state: `m.config.DropRules` (see SetDropRule couplings), `m.policySnap`
  (shared with the watchdog-tick refresh and `PolicySnapshot`), watcher/discovery
  score state.
- Invariant couplings: "every config writer persists under m.mu" (miner.go:2375-2376);
  DropRules aliasing in `cloneConfigLocked` means a commit landing inside
  applySettings' unlocked window is preserved for this map (miner.go:2708-2713),
  unlike the CampaignPolicy value field.

**Open Questions:**
- The nil→empty `DropRules` transition produced by a zero-value rule on a nil map
  (L327-329 running before the delete branch) is live-config-observable; the new
  fail-closed test asserts exact nil-ness restoration on ROLLBACK — does the intended
  fix also need to avoid allocating on the pure-delete path, or is allocation on a
  successful no-op delete acceptable?
- Two contradictory pins coexist at this HEAD: the committed fail-open
  characterization (generation_config_test.go:541-574, explicitly marked "SHOULD fail"
  when the writers become fail-closed) and the untracked fail-closed contract
  (policy_persistence_fail_closed_test.go). Which is authoritative for the current
  task contract is a process question, not answerable from code.
