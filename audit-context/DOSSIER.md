# Policy-Persistence Commit-Point — Audit Dossier

Synthesis of the per-function analyses under `audit-context/functions/` (policy-writers,
policy-readers, fence-admission, config-snapshots, applysettings-commit,
health-autoredeem-precedents, owner-identity, web-policy-handlers, generation-handoff,
persistence-failure-tests). **Understanding only — no verdicts.**

## 0. Repo state this dossier describes

- **HEAD `5b331e5`** — committed `internal/miner/policy.go` is **fail-open**: `persistLocked`
  (HEAD policy.go:220-226) logs a `config.SaveConfig` failure via `slog.Error` and returns
  nothing; `ApplyCampaignPolicy` / `SetDropRule` return nil on every admitted call, the rejected
  value stays live in memory, `refreshPolicy` fires unconditionally, and the web handlers answer
  200. The committed characterization test `internal/app/generation_config_test.go:541-574`
  (`TestInPlaceRuntimeWriteSurvivesAFailedPersist` at HEAD) pins this behavior.
- **Working tree (dirty)** — `internal/miner/policy.go` (modified), plus `internal/app/app.go`,
  `internal/miner/miner.go`, `internal/miner/rename_reconcile.go`,
  `internal/app/generation_config_test.go` (modified) and untracked
  `internal/miner/policy_persistence_fail_closed_test.go`,
  `internal/miner/policy_persistence_matrix_test.go`,
  `internal/web/policy_persistence_status_test.go`,
  `docs/plans/policy-persistence-fail-closed.md` — implements **fail-closed**: `persistLocked`
  returns the write error (working-tree policy.go:~226-231), both policy mutators restore the
  exact pre-call state inside the same `m.mu` critical section before unlocking (including
  `DropRules` map nil-ness and in-place identity), return a wrapped
  `"... rejected; no changes were made: persist config: %w"` error, and run `refreshPolicy`
  only on the success path. The committed test is renamed/inverted in the tree
  (`TestRejectedInPlaceWriteNeverReachesNextGeneration`, working tree ~533-576), and the
  untracked RED suite is verified GREEN on the working tree (`go test -race` ok).

Records `policy-writers` and parts of `policy-readers`/`config-snapshots`/`fence-admission`
describe **HEAD**; `web-policy-handlers` and `persistence-failure-tests` describe the
**working tree**. Sections below describe the working-tree contract and flag HEAD deltas inline.

## 1. System map

### 1.1 Policy mutation path (HTTP → provider → mutation → persistence → refresh → acknowledgement)

```
POST /api/policy/mode | /api/policy/drop-rule
  └ handleAPIPolicyMode / handleAPIPolicyDropRule   (internal/web/handlers_policy.go:86-126, 148-191)
      ├ non-POST → 405; ParseForm failure → 500 "invalid form"
      ├ lifecycleMutationBlocked (handlers_settings.go:48-60): paused/stopped/in-transition
      │   → 409 localized conflict (UX sugar only; nil controller = no guard; the fence is the backstop)
      ├ sample s.policyProvider under s.mu.RLock; provider==nil → documented 200 no-op
      └ provider call (lock-free from web's perspective):
          Miner.ApplyCampaignPolicy(mode)  /  Miner.SetDropRule(key, rule)
            └ m.fenced(fn)                          (internal/miner/miner.go:2135-2141)
                ├ beginApply: applyMu held for ADMISSION ONLY (miner.go:2085-2096);
                │   draining → ErrShuttingDown, zero side effects; else applyWG.Add(1)
                ├ fn body (fence gives admission, NOT serialization — body owns its own locks):
                │   m.mu.Lock()
                │   ├ MUTATE  m.config.CampaignPolicy = Normalize(mode)   (policy.go:~305-307)
                │   │         or DropRules in-place set/delete via commitDropRule (policy.go:~364-374;
                │   │         snapshots prevRule/hadPrev/wasNil first; nil map allocated if needed)
                │   ├ PERSIST persistLocked → config.SaveConfig(m.configPath, m.config)
                │   │         UNDER THE SAME m.mu WRITE LOCK (SaveConfig-under-m.mu invariant);
                │   │         configPath == "" → documented library no-persist success
                │   ├ on failure: EXACT ROLLBACK inside the same critical section
                │   │         (raw prev mode byte-exact; DropRules value+presence+map-nil-ness,
                │   │         identity preserved in place), then Unlock, slog.Error,
                │   │         return wrapped "rejected; no changes were made" error
                │   └ on success: m.mu.Unlock()
                │       PUBLISH refreshPolicy(now) — NO lock held during stores:
                │         reads mode/DirectoryGames under m.mu.RLock (policy.go:31-34),
                │         DropRules via locked value-copy (snapshotDropRules, :260-268), then
                │         3 ordered lock-free atomic stores: watcher.SetCampaignScores
                │         (session.go:159-165) → discovery.SetGameRanks (discovery.go:236-242)
                │         → m.policySnap.Store LAST (atomic.Pointer, miner.go:129)
                └ endApply (deferred): applyWG.Done fires last — after persistence and side
                  effects — which is what makes "answered successfully ⇒ in the handoff" hold
      └ acknowledgement:
          nil → 200: renderDropsList re-render (handlers_drops.go:214-246) freshly re-samples
                Campaigns/SyncStatus, DropProgress, PolicySnapshot (post-commit decisions) and
                CurrentCampaignPolicy — reflects the just-committed change because refreshPolicy
                ran synchronously pre-return (decisions stay stale when dropsTracker==nil)
          err → writePolicyMutationError (handlers_policy.go:137-143):
                errors.Is ErrShuttingDown || database.ErrClosed → 503
                (mutationRefusedAsUnavailable, responses.go:69-71); anything else → 500;
                both with constant body "Drop policy could not be changed; no changes were
                made" — raw cause logged only in the miner
```

HEAD delta: at 5b331e5 the persist-failure branch does not exist — save errors are swallowed in
`persistLocked`, rollback never happens, `refreshPolicy` fires unconditionally, and the only
propagating error is the fence's `ErrShuttingDown` (the handler 500 branch is unreachable).

`refreshPolicy` has a second trigger: the healthWatchdogLoop tick (miner.go:1852, every minute),
which involves no persistence.

Readers of the published state: `PolicySnapshot` (lock-free atomic Load; empty default before
first refresh) feeds `renderDropsList` and the debug snapshot (debug.go:306);
`CurrentCampaignPolicy` (two separate `m.mu.RLock` windows, Normalize-on-read; policy.go:272-282)
feeds the Drops page; watcher consumes scores lock-free in `orderByCampaignScore`
(session.go:182-194, nil = pre-policy order); discovery consumes ranks lock-free in
`orderGamesByPolicy` (discovery.go:248-266, nil = configured order).

### 1.2 Generation handoff

```
teardown (stop, single stopOnce body, miner.go:1899-1929)
  ├ FIRST act: applyDraining=true under applyMu, then applyWG.Wait() — UNBOUNDED
  │   (unlike joinLoops' loopJoinTimeout; WriteFileAtomic's fsync has no timeout)
  │   → after the drain, all five gated config writers are excluded; the one unfenced config
  │     writer (owner-identity, startup) ran earlier on the same goroutine
  └ Run returns → lifecycle controller's single main loop (worker.go:633), strictly after
    awaitGeneration observed the return (worker.go:777→795):
    nextGenerationConfig (app.go:619-723)
      ├ sampleCurrentMiner: currentMinerMu, pointer read only (app.go:731-733)
      ├ CurrentConfig: miner m.mu.RLock, NO App lock held (miner.go:2072-2076)
      │   → snapshotConfigLocked (miner.go:2715-2736): shallow struct copy, deep-copies exactly
      │     3 maps (AutoRedeem, DropRules, Notifications.ProviderBatching; nil preserved),
      │     aliases ALL slices — isolation from the retired miner's live maps
      │   (this RLock can wait one atomic file write behind a writer holding m.mu across
      │    SaveConfig — a wedged fs stalls pause/stop/shutdown dispatch)
      ├ MUTATION+PUBLISH of process-authoritative a.cfg = one pointer swap under cfgMu
      │   (app.go:717-722); NO persistence — deliberately infallible (no reload: a reloaded
      │   object would re-derive DiscordTokenFromEnv and not be the committed one)
      └ returned pointer becomes the new miner's live config (app.go:361)
Retired generations: providers registered once (miner.go:1079), never cleared — they serve
frozen reads; their mutators fail closed via the fence (ErrShuttingDown → 503). Only non-config
provider methods (e.g. RedeemCustomReward) stay live on a retired generation (app.go:700-707).
```

Durability substrate: `config.SaveConfig` (config.go:721-739) mutates only a local shallow copy
(clears Discord.BotToken when env-derived, one-way), whole-document `MarshalIndent`, then
`WriteFileAtomic` (util/file.go:18-69): same-dir temp → write → fsync → close → chmod-before-swap
→ rename (= publish) → best-effort dir fsync with errors swallowed. Failure modes: labeled
wrapped errors, orphan temp on hard crash, power-loss-post-rename may resurface old complete
content but never truncation, unbounded fsync block, last-rename-wins across concurrent writers
(no file lock — exclusivity is entirely the callers' lock/fence discipline).

## 2. Cross-function invariants

**I-1. SaveConfig-under-m.mu.** Every save of the live config or of a candidate that aliases it
runs while `m.mu` is held: in-place writers (persistLocked, policy.go; setAutoRedeem
rewards.go:200), no-rename settings commit (miner.go:2317), removal/rename commits
(miner.go:2314-2318, 2438-2444, 2598-2605), health commit (health.go:464-470). Rationale:
`json.MarshalIndent` iterates maps shared with the live config; an off-lock marshal races
`commitDropRule`'s map write = "concurrent map iteration and map write" panic (M2/D7). The sole
exception is the owner-identity startup save (§3 row 8), argued safe temporally, not by lock.

**I-2. DropRules aliasing [R7]** (miner.go:2663-2672). `cloneConfigLocked` deep-copies ONLY
AutoRedeem and deliberately ALIASES DropRules in the candidate: (1) a private copy would lose a
`commitDropRule` committing during the removal/rename paths' unlocked admission/analytics window
at publish time; (2) the aliasing is exactly what obliges I-1 for every candidate save.
`commitDropRule` keeps [R7] valid by construction: the map is mutated and rolled back IN PLACE,
its identity never changes (working tree; the rollback branch re-nils the map only when it was
nil before the call).

**I-3. snapshotConfigLocked vs cloneConfigLocked want opposite things** (miner.go:2708-2714)
and must never be merged: `snapshotConfigLocked` (handoff/CurrentConfig) wants ISOLATION — deep
copies the three maps, nil-preserving; `cloneConfigLocked` (settings candidates) wants ALIASING
for DropRules. Both alias all slices; slice safety rests on the drain protocol plus
ApplyToConfig's reassign-wholesale style, not on copying.

**I-4. Fence = admission, not serialization** (miner.go:2085-2143). `beginApply` holds `applyMu`
only for the admit/refuse decision; admitted bodies run concurrently and serialize only via
their own locks (`m.mu`, `healthApplyMu`, `coordinatorMu`). Refusal is `ErrShuttingDown` before
any side effect. Teardown's first act (applyDraining + unbounded `applyWG.Wait`,
miner.go:1916-1929) is what makes `CurrentConfig` stable at handoff: every acknowledged mutation
is in the sample because `endApply`'s deferred `Done` fires after persistence and side effects.
The fence deliberately does NOT serialize fenced writers against `applySettings`' clone window —
the documented overwrite residual for CampaignPolicy/Health value fields (miner.go:2204-2211);
AutoRedeem alone is re-merged at the settings commit point by
`refreshCandidateAutoRedeemLocked` (miner.go:2437/2586); Health has no equivalent.

**I-5. Persistence is the commit point; publish and side effects strictly after** (working
tree, all writers). Mutate → SaveConfig under the same `m.mu` section → on failure: exact
compensation with zero published state (candidate paths drop the candidate / abort admissions /
roll back analytics; in-place paths restore byte-exact including map nil-ness) and a wrapped
"no changes were made" error; on success: publish in the same section (candidate pointer swap or
in-place value already visible at Unlock) and only then dependent side effects (refreshPolicy
stores; CommitPlan/finishApply; canary/watchdog updates; autoRedeemState delete + gen bump).
Rollback-before-Unlock is also the anti-laundering property: memory always matches the
still-valid on-disk state, so a later unrelated whole-document save cannot launder a rejected
value to disk. HEAD delta: the two policy writers violated this — mutation preceded persistence
with no rollback and no failure signal, and any later save laundered the value.

**I-6. Publish ordering without epochs.** `refreshPolicy`'s three atomic stores (watcher scores
→ discovery ranks → policySnap last) are individually atomic but not atomic as a group and carry
no version; readers can observe mixed epochs. Similarly `CurrentCampaignPolicy` samples mode and
rules in two separate RLock windows (mixed-epoch pair possible), and `renderDropsList` merges
three non-atomic provider samples — accepted as display-only skew.

**I-7. Snapshot isolation at the handoff.** `nextGenerationConfig` never touches disk and
cannot fail; isolation comes from `snapshotConfigLocked`'s map copies plus the drain. The
"admitted implies present in this sample" claim is bounded by the applySettings clone-window
residual (app.go:708-710, documented, not enforced).

**I-8. Nil-vs-empty map discipline.** `setAutoRedeem` (I6/D5) and working-tree `commitDropRule`
both restore map nil-ness on rollback; `snapshotConfigLocked` preserves nil-ness (pinned by
current_config_test.go) while display-side `snapshotDropRules` flattens nil to an allocated
empty map — an intentional-looking display/persistence asymmetry (open question OQ-9).

## 3. Lock-ownership table

Locks: `m.mu` = miner config RWMutex (miner.go:274); `applyMu/applyWG` = fence admission;
`healthApplyMu` (order healthApplyMu → m.mu, miner.go:108-118); `coordinatorMu` (order
coordinatorMu → m.mu → manager.mu → streamer.mu); `cfgMu`/`currentMinerMu` = App locks.

| # | Writer / path | Admission & serialization | MUTATION under | PERSISTENCE under | PUBLISH under | Dependent side effects | Error propagation |
|---|---|---|---|---|---|---|---|
| 1 | `ApplyCampaignPolicy` (policy.go, in-place) | fenced (applyMu admit; body serializes via m.mu) | m.mu(W): `config.CampaignPolicy` | same m.mu(W): persistLocked→SaveConfig; failure → byte-exact rollback in-section | value visible at Unlock; snapshot: refreshPolicy stores, NO lock, success-only | refreshPolicy (watcher scores, discovery ranks, policySnap last), post-Unlock, post-persist | ErrShuttingDown→503; persist failure→wrapped "rejected; no changes were made"→500; success→200 re-render. HEAD: swallowed, always nil/200 |
| 2 | `SetDropRule`/`commitDropRule` (policy.go, in-place) | fenced (entered BEFORE empty-key check so a retired gen never answers success); empty key on live gen → nil no-op | m.mu(W): DropRules in-place set/delete, identity preserved; prev/hadPrev/wasNil snapshotted first | same m.mu(W); failure → exact rollback incl. map nil-ness, in-section | as row 1 | as row 1 | as row 1 |
| 3 | `applySettingsNoRename` (miner.go:2310-2334) | beginApply + coordinatorMu for whole apply | m.mu(W): candidate = cloneConfigLocked + ApplyToConfig + ChannelID stamping (live config untouched) | same single m.mu(W) section: SaveConfig(candidate) | same section: `m.config = candidate` | CommitPlan (manager.mu, conflicts warn-only) then finishApply fan-out off-lock; all post-publish | failure → unlock, wrapped "settings apply rejected…persist config", zero mutation/side effects |
| 4 | `applySettingsWithRemovals` / `WithRename` (miner.go:2436-2452, 2585-2618) | beginApply + coordinatorMu; SRAP durable admission first | candidate mutated partly OFF-lock (admission/analytics window; private except aliased DropRules per [R7]) | m.mu(W) commit section: SaveConfig + refreshCandidateAutoRedeemLocked | same m.mu(W) section, immediately after successful save | roster commit, purge (post-commit purge failure = success + durable retry row), CommitPlan/finishApply | failure → compensation (analytics rollback, abortAdmittedRemovals), wrapped error, zero publish |
| 5 | `applyHealthSettings` (health.go:455-495) | fenced + healthApplyMu held for whole fn (health-vs-health only; applySettings never takes it) | healthApplyMu + m.mu(W): private candidate (clone + Health=s + ValidateConfig clamp) | healthApplyMu + m.mu(W): SaveConfig(candidate) | healthApplyMu + m.mu(W): pointer swap `m.config = candidate` | canary/watchdog UpdateSettings with post-clamp values + success log — after m.mu release, still under healthApplyMu, strictly post-persist (spy-pinned) | failure → candidate dropped (no rollback needed), wrapped "health settings apply rejected; no changes were made"→500/503; no re-render on error |
| 6 | `setAutoRedeem` (rewards.go:170-229) | fenced BEFORE roster/config admission (retired-gen refusals stay retryable); serialization = m.mu(W) held continuously L173-226 | m.mu(W): in-place live map write/delete after prevCfg/hadPrev/wasNil snapshot; dual admission (runtime roster AND persisted list, [R2]) | same held m.mu(W): SaveConfig(m.config) | publish = Unlock (intermediate state unobservable: all readers take m.mu) | after success, same m.mu(W): delete autoRedeemState[key] + bumpAutoRedeemGenLocked (seals stale evaluator snapshots); post-Unlock only slog.Info | failure → exact rollback incl. re-nil (I6/D5) → "failed to save config: %w" → handler 400 (not 503) |
| 7 | `refreshPolicy` (publisher, policy.go:26-60) | none (called post-commit or from watchdog tick) | none | none | NO lock: 3 ordered lock-free atomic stores; reads under m.mu.RLock first | is itself the side effect | none (infallible) |
| 8 | Owner-identity reconciliation (miner.go:658-693; owner_identity.go:38-52) | none — single-threaded startup, before provider registration; the documented sole exception to under-m.mu config writing (miner.go:2052-2057) | NO lock: in-place on live *m.config | NO lock: SaveConfig only when changed && configPath != "" | publish = mutation (in-place, no candidate) | rename/pin slog.Info BEFORE save; after save attempt regardless of outcome: GetChannelID, loadStreamers, IRC NICK, notifications | save failure swallowed — slog.Warn "will retry on the next restart"; Login/resolve errors → Run "authentication failed: %w" → failStartup |
| 9 | `nextGenerationConfig` (app.go:619-723) | lifecycle main loop, strictly after Run returned (awaitGeneration) | a.cfg pointer swap only | NONE — deliberately infallible | cfgMu: pointer swap (sample taken under retired miner's m.mu.RLock via CurrentConfig, no App lock held) | none beyond the swap | no error return |

Test seams (persistence-failure suite): `breakConfigPathForNextSave` (miner,
cp1_c2_matrix_test.go:34-56) / `breakConfigPath` (app clone, generation_config_test.go:269-281)
replace config.json with a directory so SaveConfig fails deterministically at WriteFileAtomic's
`os.Rename` (file.go:55); observable "path is still a dir" ⇔ "nothing written". Fence harness
(`newFenceMiner`/`startLiveGeneration`/`startRetiredGeneration`,
stale_generation_fence_test.go:21-177) drives real Run/setupComponents/web.Server over real
HTTP, channel-synchronized. The untracked RED file pins: non-200 (500) on persist failure, prior
live value retained, snapshot never re-ranked from the rejected value, disk untouched, and
whole-document laundering prevention — RED at HEAD, GREEN on the working tree.

## 4. Consolidated unenforced assumptions ("nothing found" = no mechanical enforcer located)

### Lock-precondition comments with no mechanical guard
1. `persistLocked` "caller holds m.mu" — comment-only (policy.go, working tree ~:222-226; HEAD :219); load-bearing because SaveConfig marshals the aliased live DropRules map (config.go:726,731; miner.go:2661-2669).
2. `snapshotConfigLocked` / `cloneConfigLocked` "caller holds m.mu" — naming convention + call-site inspection only (miner.go:2714, :2670).
3. `configHasStreamerLocked` (rewards.go:126) and `bumpAutoRedeemGenLocked` (rewards.go:433) — comment-only; no TryLock panic guard, unlike `refreshCandidateAutoRedeemLocked`'s [R3] guard (rewards.go:460-463).
4. `applyHealthSettings` reachable only via the fenced wrapper — no guard against a direct in-package call (health.go:455).
5. Lock order `healthApplyMu → m.mu` documented (miner.go:111) with no assertion; trivially safe today (two acquirers).
6. `applySettingsNoRename` "caller holds coordinatorMu" and "plan has no renames/removals" — established only by the dispatch switch (miner.go:2218, 2232-2238); a direct call with a removal-bearing plan would commit removals with no SRAP admission (miner.go:2243-2244).

### Fence / drain protocol
7. Every accepted apply calls `endApply` exactly once (mandated miner.go:2084) — enforced only by the two disciplined defer sites (:2139, :2214); a third caller forgetting it wedges the drain.
8. `applyWG.Wait()` (miner.go:1929) is unbounded and assumes every admitted apply terminates; WriteFileAtomic's fsync has no timeout (app.go:661-662).
9. No admitted apply calls back into `stop()` (re-entrant once-guard deadlock, miner.go:1913-1915) — stated rule only.
10. `applyDraining` never resets false — true by construction (single stopOnce body) with no explicit guard.
11. `m.runCtx` read in beginApply under applyMu (miner.go:2091) vs unsynchronized write in Run (miner.go:484) — safe for web-driven calls only via provider-registration ordering; nothing found for a direct library call concurrent with Run's first lines.

### Aliasing / copy discipline
12. "config.Config reaches exactly three maps" (miner.go:2685) — true at HEAD (config.go:65-212), no reflection-based inventory test; a fourth map field would silently alias through snapshotConfigLocked.
13. "No candidate-config code path mutates DropRules in place" (miner.go:2661-2662) — doc + inspection only (builder.go:199-249 never touches DropRules; rename_reconcile.go:62 touches only AutoRedeem); a future off-lock candidate DropRules write would race commitDropRule unlocked.
14. Snapshot slice-aliasing safety ("none written after handoff", miner.go:2700-2703) rests on the drain protocol + ApplyToConfig's reassign-wholesale style; no per-slice test; per-element shared `Settings` pointers (config.go:253) have no enforcer against post-handoff writes.
15. `cloneConfigLocked` aliases Notifications.ProviderBatching with no [R7]-style warning — safe only because that map currently has no in-place writer (miner.go:2686-2690).
16. `AutoRedeemConfig.RewardIDs` slice headers alias live backing arrays in both copies (config.go:248) — never element-mutated only because setAutoRedeem rebuilds via dedupeStrings (rewards.go:183); inspection only.
17. `config.DropRule` staying a pure value type (config.go:217-231) is what makes snapshotDropRules' shallow copy a deep copy — nothing guards against a future reference-typed field.
18. Published `policySnapshot.Decisions` slice (and byID map) treated as read-only after Store — returned by reference (policy.go:252); convention only; current readers only read.
19. `m.config.DirectoryGames` backing array never mutated in place after the RLock capture (policy.go:33) — comments + writer inventory (builder.go:234 reassigns wholesale); no race test.
20. `buildPolicyInputs` dereferences `m.watcher.BrokerSnapshot()` with no nil check (policy.go:70); precondition "dropsTracker non-nil ⇒ watcher non-nil" established only by startMining construction order (miner.go:931 before :945).

### Global writer invariants
21. "Every live-config writer mutates/persists under m.mu" (the no-rename atomicity argument, miner.go:2261-2278) — verified for the current writers, nothing enforces it for future ones.
22. `m.streamerLifecycle` never rebuilt mid-run (miner.go:2168-2172) — startup-ordering convention; written WITHOUT m.mu at streamer_deletion.go:65/71/74, sampled under m.mu.RLock at miner.go:2221-2223.
23. `PlanReconcile`'s unlocked Twitch resolution runs with no ctx and no visible budget while coordinatorMu is held (manager.go:379) — no bound found.
24. `rewards()` accessor contract "no m.mu ever held across a call through the returned client" — comment-only (rewards.go:29-31).

### Web / input-validation seam
25. Valid policy mode from the client — unknown mode silently normalized to GAME_ORDER and persisted with a 200 (handlers_policy.go:120; internal/policy/policy.go:51-57).
26. Checkbox on/true convention — any other value silently unchecked (handlers_policy.go:193-196).
27. `rewardKey` reality / DropRules boundedness — miner only lowercases/trims (policy.go:341); arbitrary keys persist arbitrary config.json map entries.
28. Same provider instance for mutate and 200 re-render — renderDropsList re-samples s.policyProvider (handlers_policy.go:106-108 vs handlers_drops.go:214-219); a generation swap between renders from a different provider.
29. `database.ErrClosed` ⇒ nothing-was-mutated for every producer routed through mutationRefusedAsUnavailable — doc-contract only (responses.go:60-71); branch currently unreachable via the two policy mutators (config-file-only persistence).
30. AutoRedeem payload has no upper Budget bound or RewardID shape validation (handler clamps only Budget<0, handlers_rewards.go:177-179; setAutoRedeem only dedupes/trims).

### Startup / owner-identity
31. Exclusive access to m.config during the off-m.mu startup mutation — enforced only by Run's call order (miner.go:490-498) and generation-private snapshots (app.go:711-723); doc-only (miner.go:2052-2057).
32. "Will retry on the next restart" (miner.go:691) assumes the next Login includes an authoritative validate; on a degraded startup the rename is not re-observed that run.
33. `configPath == ""` silently skips the owner-identity persistence with no warn (miner.go:689) — undocumented to library callers at that site.
34. authenticate's wiring of the block (one-save-for-both, warn path, skip path) has no test — only the pure function and auth producers are covered.
35. Startup SaveConfig rewrites the whole config.json from memory (config.go:721-739); concurrent operator hand-edits are last-writer-wins with no guard.

### Durability substrate
36. `WriteFileAtomic` nil return does not prove the rename is durable (best-effort dir fsync, errors swallowed, file.go:61-68, 75-82); no caller distinguishes "renamed" from "durable" where "acknowledged mutation is durable" is relied on (app.go:637-638).
37. Writer exclusivity to one path — no file lock; last rename wins silently (file.go:18-69).
38. Orphan temp files after a hard crash pre-rename are never swept (file.go:27-32).
39. `SaveConfig` dereferences a nil config parameter without guard (config.go:726).
40. Mid-marshal stability of shared maps/slices in SaveConfig's shallow copy relies on callers holding m.mu; the owner-identity save runs off m.mu — emergent temporal argument only (miner.go:690; config.go:726-731).

### Handoff / retirement
41. "Admitted implies present in this sample" bounded by the applySettings clone-window residual — documented, not enforced (app.go:708-710).
42. Non-config provider methods (RedeemCustomReward) still executable on a retired generation — drain claim excludes them, nothing fences them (app.go:704-707).
43. `sampleCurrentMiner`'s "act on the result only after releasing currentMinerMu" — call-site structure only (app.go:726-729).
44. Crash window between SaveConfig success (rewards.go:200) and gen bump (rewards.go:224) benign only because autoRedeemState is process-local/reset-on-restart (miner.go:248-249); no test pins that.

### Test-infrastructure couplings
45. `breakConfigPathForNextSave` name implies one failed save but breaks ALL subsequent saves until repaired (cp1_c2_matrix_test.go:48).
46. "still IsDir ⇔ nothing written" is coupled to WriteFileAtomic's rename being SaveConfig's only mutation of the path (config.go:738; file.go:55) — no enforcer binds future implementations.
47. Rename-onto-directory failing on every platform incl. Windows (cp1_c2_matrix_test.go:37-44) — assumed, not asserted in-repo.
48. `startLiveGeneration` port-probe close-then-reuse TOCTOU; waitDialable could dial a foreign listener (stale_generation_fence_test.go:81-85).
49. database.Open process-wide singleton must never be closed by a test — comment discipline only (stale_generation_fence_test.go:31-34; cp1_c2_matrix_test.go:15-20).
50. `breakConfigPath` (app) duplicated from the miner helper; only a comment cross-reference keeps the bodies in sync (generation_config_test.go:269-272).
51. genHarness gap: buildWith's Factory closure/persistence decorator/status sink/updater wiring not exercised (generation_config_test.go:41-50).
52. Laundering test's path repair relies on WriteFileAtomic creating the target purely via rename with no pre-existing file (policy_persistence_fail_closed_test.go:172-174).
53. Fenced-writer mutation vs applySettings clone window: the CampaignPolicy/Health value-field overwrite residual (miner.go:2129-2134, 2204-2211) is documented, not mechanically bounded — AutoRedeem alone is re-merged at the settings commit point.

## 5. Consolidated open questions

**Process / task-contract**
- OQ-1. Contradictory pins at HEAD 5b331e5: committed fail-open characterization (generation_config_test.go:541-574 at HEAD) vs the working tree's fail-closed contract (renamed committed test + untracked RED suite, GREEN on tree). Which governs the current task contract is a process question; the audit-scope question ("assess HEAD or the intended contract?") has the same root.
- OQ-2. Will the deliberate characterization-update note (working-tree generation_config_test.go ~L513-532, recording the old fail-open behavior and its inversion) survive into committed history?
- OQ-3. When the fail-closed plan lands, `CurrentConfig`'s doc paragraph describing fail-open in-place writers (miner.go:2041-2055) becomes stale — is its update in scope?
- OQ-4. Task brief cited `TestInPlaceRuntimeWriteSurvivesAFailedPersist` at HEAD; the working tree renames/inverts it — confirm the audit tracks the working-tree version going forward.
- OQ-5. What does untracked `internal/miner/policy_persistence_matrix_test.go` add relative to the RED file's three observables? Does untracked `internal/web/policy_persistence_status_test.go` pin the 500-vs-503 mapping (mutationRefusedAsUnavailable never matching a persist-failure chain) the RED file's status assertions depend on?

**Policy semantics / UX**
- OQ-6. `policy.Normalize` silently coerces an invalid mode to DefaultMode, persisted and answered 200 — intended UX for direct (non-dashboard) POSTs? (internal/policy/policy.go:51-57)
- OQ-7. `SetDropRule`'s empty-key nil is indistinguishable from a committed rule at the provider seam (web independently guards key != "", handlers_policy.go:173) — is the seam exercised outside tests?
- OQ-8. `commitDropRule` allocates DropRules nil→empty even on the pure-delete branch — does the fail-closed design intend to preserve nil-ness on that path too, given the test asserts exact nil-ness on rollback? (Working tree rolls back nil-ness on failure; the success-path delete leaves an allocated empty map.)
- OQ-9. `snapshotDropRules` flattens nil rules to an allocated empty map (policy.go:263) while snapshotConfigLocked preserves nil-ness (miner.go:2723-2728) — intentional display/persistence asymmetry?
- OQ-10. `policySnapshot.byID` is stored but never read from the stored snapshot anywhere — dead weight or reserved?
- OQ-11. `eligibleLiveChannels` (policy.go:128-145) may double-count a channel that is both a configured online streamer and a non-offline discovery channel of the same game — does the discovery pool exclude configured streamers, or only slot allocation (miner.go:1013-1015)?
- OQ-12. `renderDropsList` merges three non-atomic samples — accepted UI staleness or worth a combined snapshot? Any non-display consumer of the mode/rules pair not found.
- OQ-13. Is the 500 (not 400) for ParseForm failure, and the 200 no-op for provider==nil / empty rewardKey, pinned intentionally? (provider==nil is documented deliberate; no tests found for the others.)

**Clone-window residual / lost updates**
- OQ-14. Health clone-window residual: applySettingsWithRemovals' commit re-merges only AutoRedeem + channel IDs into its stale candidate; candidate.Health has no refresh — a health apply committing inside another apply's clone window is overwritten in memory AND on disk while canary/watchdog keep the newer settings. Accepted specifically for Health, or only ever analyzed for AutoRedeem?
- OQ-15. healthApplyMu's "entire sequence across concurrent callers" doc (health.go:441-445) covers only health-vs-health — is the narrower scope intended to be read that way?
- OQ-16. Can any generation-boundary-reachable sequence actually hit the applySettings clone-window lost-update residual (app.go:708-710), or is it strictly mid-generation?
- OQ-17. CommitPlan conflicts are warn-only (miner.go:2326, 2645-2648) after the candidate is persisted/published — do disk config and runtime roster deliberately diverge for conflicting entries until the next apply?
- OQ-18. `finishApply`'s `m.config = newConfig` (miner.go:2795) is a same-pointer no-op on all three paths — vestigial or expected to matter for a future path?

**Concurrency / lifecycle bounds**
- OQ-19. Is a direct (non-web) mutator call concurrent with Run's first ~10 lines a supported use, given the unsynchronized runCtx write (miner.go:484)?
- OQ-20. Can a wedged Twitch resolution inside an admitted applySettings hold the unbounded applyWG.Wait — and process shutdown — open indefinitely? What actually bounds PlanReconcile's resolution under coordinatorMu (HTTP client timeouts only)? A slow resolution queues every subsequent apply (fenced writers unaffected).
- OQ-21. applySettingsNoRename has no cancellation point (no ctx): a disconnected client still gets its change committed — accepted anywhere in docs?
- OQ-22. Is the test-only mid-run swap of m.streamerLifecycle (doc miner.go:2171-2173) exercised under -race, given the unlocked startup write?
- OQ-23. Owner-identity reconciliation writes m.config off m.mu (miner.go:680-693) — safe vs the handoff by awaitGeneration ordering, but is it provably safe vs that miner's OWN concurrent m.mu readers during the startup window, given the process-owned web server outlives generations? (Needs the timing of this generation's provider registration relative to miner.go:680.)
- OQ-24. Which is the second "unfenced non-config mutator" counted at miner.go:2070? app.go:702 names RedeemCustomReward as one; the other is not identified in the sections read.
- OQ-25. Do any callers build "admitted implies durable" on CampaignPolicy/DropRules where (at HEAD) persistLocked only logs and nextGenerationConfig carries the unsaved value forward (app.go:631-638)? (The working tree removes the premise; the question survives as a HEAD-characterization item.)

**Durability / identity**
- OQ-26. Is the mode-4 durability window (rename acknowledged, directory entry lost on power failure) acceptable everywhere "acknowledged mutation is durable" is relied on (app.go:637-638)?
- OQ-27. Does any web surface render the early web server's constructor-time (pre-rename) username (app.go:341) after owner-identity adoption? The injected server is never re-fed the adopted login; only the library-fallback build (miner.go:835-837) uses the post-adoption value.
- OQ-28. Case-only rename is deliberately not a rename (EqualFold, owner_identity.go:40) so cfg.Username keeps config-file casing forever — is any consumer (IRC NICK miner.go:878, dashboard labels) sensitive to Twitch's canonical casing?
- OQ-29. Guard asymmetry: refreshCandidateAutoRedeemLocked gets a TryLock panic ([R3]) while configHasStreamerLocked/bumpAutoRedeemGenLocked rely on comments — deliberate?
- OQ-30. Does the dashboard re-fetch via GetAutoRedeem after a save, given handleAutoRedeem returns only writeSuccess with no echo of the deduped/normalized cfg (handlers_rewards.go:199)?
- OQ-31. Copy/immutability contract of campaignsProvider.Campaigns()/SyncStatus() and dropProgressProvider.DropProgress() for lock-free template rendering — owned by internal/drops and internal/health, not traced in this audit.
- OQ-32. s.i18n.T fallback behavior for a missing key/language on the direct writeSettingsConflict call path — not verified.
