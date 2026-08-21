# Persistence-failure test seams — inventory (audit-context, TESTS record)

Repo state analyzed: HEAD `5b331e5` with a DIRTY working tree. Committed at HEAD and unmodified:
`internal/miner/cp1_c2_matrix_test.go`, `settings_persistence_test.go`, `srap_test.go`,
`health_persistence_test.go`, `stale_generation_fence_test.go`. Modified in the working tree:
`internal/app/generation_config_test.go`, `internal/miner/policy.go`, `internal/miner/miner.go`,
`internal/app/app.go`, `internal/miner/rename_reconcile.go`. Untracked (new):
`internal/miner/policy_persistence_fail_closed_test.go` (the RED file this record covers), plus two
adjacent new files not analyzed here (`internal/miner/policy_persistence_matrix_test.go`,
`internal/web/policy_persistence_status_test.go`).

Every seam below induces or observes the same production commit point: `config.SaveConfig`
(internal/config/config.go:721-739) → `util.WriteFileAtomic` (internal/util/file.go:18-67), whose
final step is `os.Rename(tmpName, path)` (file.go:55).

---

## breakConfigPathForNextSave in internal/miner/cp1_c2_matrix_test.go (L34-L56)

**Purpose:** The package-wide deterministic persistence-failure injector for `internal/miner`:
replace the regular file at `path` with an empty directory of the same name so that the next (and
every subsequent, until repaired) `config.SaveConfig(path, ...)` fails at `WriteFileAtomic`'s final
`os.Rename` step.

**Inputs & Assumptions:**
- `t *testing.T` (trusted, test-owned), `path string` — must currently exist as a regular file
  (established by each caller seeding via `config.SaveConfig` first, e.g. cp1_c2_matrix_test.go:78,
  settings_persistence_test.go:37, srap_test.go:246, health_persistence_test.go:21,
  stale_generation_fence_test.go:47).
- Assumes renaming a regular file onto an existing directory fails regardless of process privilege
  (doc comment L37-L44). Enforcer: POSIX `rename(2)` semantics only — **nothing found in-repo**
  (no test asserts the property itself, no cross-platform guard; the doc argues it, correctly for
  Linux/macOS/Windows `os.Rename`).
- Assumes `WriteFileAtomic`'s only mutation of `path` is that rename, and that its failure path
  removes the temp file (doc L44-L47). Enforced by internal/util/file.go:27-32 (deferred
  `os.Remove(tmpName)` when `!success`) and file.go:55 today; **no mechanical enforcer** binds
  future `SaveConfig` implementations to this contract.
- Assumes root-safety: `os.Chmod`-based injection is unreliable under root (permission bits
  bypassed), rename-onto-directory is not (doc L40-L44). Enforcer: **nothing found** (no test runs
  as both uid≠0 and uid=0; argument is kernel semantics).

**Outputs & Effects:** No return value; on any failure `t.Fatalf`. Postcondition: `path` is an
empty directory, mode 0o755. Side effect scope: ALL later `SaveConfig` calls to `path` fail, not
just "the next one" — the name undersells the persistence of the breakage; callers that need a
later successful save must repair manually (`os.Remove(path)`,
policy_persistence_fail_closed_test.go:172).

**Block-by-Block:**
- L48-L49 `func breakConfigPathForNextSave(t *testing.T, path string) { t.Helper()` — What:
  helper marker. Why here: failure attribution to the caller. Assumes: nothing. Establishes: test
  helper frames. Depended on by: all callers' failure messages.
- L50-L52 `if err := os.Remove(path); err != nil { t.Fatalf(...) }` — What: delete the seeded
  regular file. Why here: the directory must take the file's exact name. Assumes: `path` exists and
  is a file (caller seeded it). Establishes: name free; also destroys the original bytes — which is
  why no caller can assert byte-for-byte preservation (settings_persistence_test.go:88-93 states
  this explicitly). Depended on by: the Mkdir below; by every caller's choice of the
  "still-a-directory" disk observable.
- L53-L55 `if err := os.Mkdir(path, 0o755); err != nil { t.Fatalf(...) }` — What: install the
  directory. Why here: makes `os.Rename(tmp, path)` fail deterministically (EISDIR/ENOTEMPTY class,
  not a permission check). Assumes: same-name creation succeeds in the temp dir. Establishes: the
  broken state AND the disk observable: "`path` is still a directory" ⇔ "no new config content was
  ever written", because the failing rename is the only step that could have replaced it with a
  regular file (file.go:55), and the temp file `WriteFileAtomic` creates lands in the PARENT dir
  (`filepath.Dir(path)`, file.go:19-21) and is removed on failure (file.go:28-32). Depended on by:
  every `os.Stat(path).IsDir()` assertion in this package and in the RED file.

**Cross-Function Dependencies:**
- Callees: `os.Remove`, `os.Mkdir` only. Production code exercised indirectly:
  `config.SaveConfig` (config.go:738 → `util.WriteFileAtomic`), `util.WriteFileAtomic`
  (file.go:18-67).
- Callers (committed): cp1_c2_matrix_test.go:85 (C2-B), settings_persistence_test.go:40,
  srap_test.go:257, health_persistence_test.go:98,135,286. Callers (untracked new file):
  policy_persistence_fail_closed_test.go:48,102,165. Same-package visibility only; internal/app
  clones it as `breakConfigPath` (see below).
- Shared state: the filesystem path; no locks involved in the seam itself.
- Invariant coupling: the "IsDir ⇔ nothing written" equivalence is coupled to
  `WriteFileAtomic`'s write-to-temp-then-rename structure. If `SaveConfig` ever wrote `path`
  directly or renamed inside a retry loop that first removed `path`, every assertion built on this
  seam would silently weaken.

**Observable proved / determinism:** Proves "persistence failed AND nothing new reached disk" as a
single filesystem invariant. Determinism argument (doc L34-L47): rename-onto-a-directory is a
filesystem invariant, not a permission check, so it fails identically for root and non-root — the
explicit reason `os.Chmod` was rejected as an injector. No timing, no goroutines.

**Audit points (as exercised by its users):**
- MUTATION point lock: caller-dependent (see per-user sections).
- PERSISTENCE point lock: whichever lock the production writer holds around `SaveConfig` —
  `m.mu` for in-place policy writers (policy.go working tree:226-231 "the deliberate
  SaveConfig-under-m.mu invariant"), `m.mu` + `healthApplyMu` for health (health.go:456-470).
- PUBLISH point: never reached on this seam's failure path by contract; each user asserts that.
- Error propagation: `os.Rename` error → wrapped `"replace %s: %w"` (file.go:56) →
  `SaveConfig` returns it (config.go:738) → production caller's failure branch.

**Open Questions:**
- The name says "ForNextSave" but the breakage persists for all subsequent saves; is any test
  relying on exactly-one-failed-save semantics? (None found; the laundering test repairs the path
  explicitly at policy_persistence_fail_closed_test.go:172-174.)
- No in-repo enforcement that the rename-fails property holds on Windows CI (`make build-windows`
  exists); tests observed here run on Linux.

---

## Seam users: settings_persistence_test.go (ordinary no-rename settings path)

**Purpose:** Pin BLOCK-1 for `applySettingsNoRename` (miner.go:2310): persistence is the commit
point for the ordinary Settings-page save; a failed `SaveConfig` must fail out loud and mutate
nothing.

**What each test asserts:**
- `TestApplySettingsNoRenamePersistFailureIsReportedAndMutatesNothing` (L33-L98): after
  `breakConfigPathForNextSave` (L40), `m.ApplySettings` returns non-nil (L54) — the error the
  POST /api/settings caller needs; live config is the SAME pointer (`m.config != cfgBefore`, L60 —
  i.e. NO PUBLISH happened) with same contents (ConsoleLevel L63, DropBlacklist L66); runtime
  roster not reconciled (FollowRaid L71, no user-topic churn L74, zero ToggleChat L77); disk: path
  still the installed directory (L94-L97). Deliberately NOT asserted (L89-L93): byte-preservation
  of the old config.json — the seam destroys the original bytes, so such an assertion could only
  pass by self-fulfilling restoration. Historical claim in doc (L19-L24): before this pass the
  no-rename path mutated live config and committed the roster BEFORE persisting and `finishApply`
  only `slog.Error`'d the failure (fail-open, HTTP 200).
- `TestApplySettingsNoRenamePersistSuccessAppliesAndPersists` (L104-L139): the success half —
  live config, roster, and disk all updated.
- `TestApplySettingsNoRenameWithoutConfigPathStaysSuccessful` (L147-L160): `configPath == ""` is
  the documented no-persist success (commit point must not turn "no config file" into failure).
- `TestApplySettingsNoRenamePersistsResolvedChannelIDs` (L168-L199): because persistence moved
  BEFORE `CommitPlan`, the persisted candidate must be stamped with the plan's own resolved
  ChannelIDs, not the post-commit roster's.

**Audit points:** MUTATION (runtime roster + live config) and PUBLISH (config pointer swap) are
asserted to happen only AFTER persistence succeeds; the failure test proves both are absent, so on
this path persistence strictly precedes mutation/publish. Error propagates
`SaveConfig → applySettingsNoRename → ApplySettings → caller` (asserted non-nil at L54).
Dependent side effects (topic reconcile, chat presence): fire only post-persist; asserted absent on
failure (L74-L79). Lock at persistence: inside the miner's apply pipeline (not directly observed by
this black-box test; per policy.go:222-225 the package invariant is SaveConfig-under-`m.mu` for the
locked writers, while the settings pipeline persists a CANDIDATE before publish).

**Determinism:** Same filesystem seam; fake capability topics/chat recorders are synchronous; no
sleeps; single-goroutine apply.

**Open Questions:** none beyond the shared-seam ones.

---

## Seam users: srap_test.go (removal path, SRAP)

**Purpose:** Pin the removal path's ordered failure modes around its commit point
(`applySettingsWithRemovals`, miner.go:2387): durable admission → SaveConfig (commit) → roster
commit → post-commit purge.

**What each test asserts (persistence-failure-relevant subset):**
- `TestApplySettingsAdmissionFailureClosedDBZeroMutation` (L83-L191): PRE-commit failure (DB
  closed via a private non-singleton handle, L30-L38 `openRawMinerDB`, so `AdmitRemovals` cannot
  succeed): apply fails wrapping `database.ErrClosed` (L127); zero mutation everywhere — runtime
  (L137), in-memory config (L141-L149), config.json byte-identical (L96-L98 vs L151-L157 — byte
  comparison is possible HERE because this test does NOT use breakConfigPathForNextSave; SaveConfig
  is never reached); no durable row in either SRAP ledger, verified by REOPENING the same file
  (L163-L175); history intact (L177-L190); and log-truthfulness: no "durably queued" / "Streamer
  removal committed" claim was ever emitted (L129-L134, via `captureHandler` L402-L442).
- `TestApplySettingsMultiRemovalAllOrNothingAtMinerLevel` (L198-L231): one admission batch, not a
  per-streamer loop — both victims survive a failed batch.
- `TestApplySettingsSaveConfigFailureCompensatesAdmission` (L239-L310): THE seam user — admission
  SUCCEEDS, then `breakConfigPathForNextSave` (L257) makes the commit point fail. Asserts: apply
  errors (L269-L272); no false "committed"/"durably queued" log (L273-L278); runtime + in-memory
  config unchanged (L281-L293); disk still the directory (L296-L299); the prepared admission row
  was COMPENSATED (`AbortAdmission`) — `HasPending` is false (L302-L304) — i.e. the compensating
  side effect fires when persistence fails, before return; history intact (L307-L309).
- `TestApplySettingsRequestCtxCancelledBeforeAdmissionZeroMutation` (L318-L342) and
  `TestApplySettingsRunCtxCancelledRejectsBeforeMutation` (L348-L365) /
  `TestApplySettingsBeginApplyDrainingRejects` (L370-L387): the pre-admission gates — cancelled
  request ctx aborts with `context.Canceled` and provably never attempted admission (absence of a
  durable row, L339-L341); cancelled runCtx / draining refuse with `ErrShuttingDown` before any
  mutation.
- `TestApplySettingsPurgeFailurePostCommitStaysRemovedRowRemains` (L452-L522): POST-commit
  failure ordering — once SaveConfig committed and the roster is updated, a failed purge is NOT an
  apply failure (nil error, L497-L499); streamer stays removed; the durable pending row REMAINS
  (L507-L509); history survives (purge tx rolled back, L512-L514); and the exact truthful log line
  (L518-L521) fires only from the branch where the durable row provably exists.
- `TestApplySettingsRemovalWithAdditionPersistsResolvedChannelIDs` (L566-L655) and
  `TestApplySettingsRemovalPersistsRosterChannelIDWhenResolutionFails` (L735-L815): the
  pre-persist ChannelID stamping (plan-sourced and roster-sourced halves) with two path proofs —
  a static `PlanReconcile` probe (L590-L606) and the `applyCommitBarrier` seam
  (miner.go:385-395; only removal/rename paths bracket the commit; two phases
  `applyPreCommit`/`applyPostCommit` recorded synchronously on the calling goroutine, L613-L621).

**Audit points:** Ordering pinned end-to-end: admission (durable, pre-commit) → SaveConfig
(COMMIT) → roster/runtime mutation → purge (post-commit, failure-tolerant, leaves durable retry
row). Error propagation: admission failure wraps `database.ErrClosed`; SaveConfig failure fails the
apply AND fires the `AbortAdmission` compensation before returning; purge failure propagates
nowhere (logged, durable row retained). Locks: not directly observed (black-box); the barrier seam
plus the log-capture handler are the observability instruments. `captureHandler` is
mutex-protected (L403-L414) so the race detector accepts it.

**Determinism:** closed-DB injection is state-based, not timing-based; `openRawMinerDB` isolates
from the package singleton so closing cannot poison other tests (L25-L29); fake API resolves
synchronously (srap_test.go:668-688 `unresolvableAPI` is mutex-guarded); barrier slice needs no
sync because the seam is invoked on the same goroutine (L610-L613).

**Open Questions:** none specific; the applyCommitBarrier field's production nil-ness
(miner.go:385) is the usual test-only-seam trust assumption.

---

## Seam users: health_persistence_test.go (ApplyHealthSettings, P3)

**Purpose:** Pin the health path's commit point (`applyHealthSettings`, health.go:455-494):
candidate-clone → validate/clamp → SaveConfig (commit) → publish → notify dependents.

**What each test asserts:**
- `TestApplyHealthSettingsPersistFailureLeavesSettingsUnchanged` (L93-L123): failed persist ⇒
  non-nil error (L102), NO publish (same `*config.Config` pointer, L108) and no content change
  (L111), disk still the directory (L119-L122).
- `TestApplyHealthSettingsPersistFailureSkipsDependentUpdates` (L131-L148): the SIDE-EFFECT
  ordering proof — injectable spy seams `healthCanaryUpdate`/`healthWatchdogUpdate` (miner.go
  seam fields; installed L76-L82) observe ZERO calls on a failed persist. Exists because
  internal/health keeps cfg unexported with no synchronous observer (doc L49-L53), so "dependent
  was not notified" is otherwise unobservable.
- `TestApplyHealthSettingsPersistSuccessAppliesAndPersists` (L153-L173) and
  `...NotifiesDependentsWithValidatedValues` (L183-L234): success half — dependents notified
  EXACTLY once each with the VALIDATED/CLAMPED values (every input deliberately out of clamp range,
  L188-L199), pinning that notification happens from the post-validation candidate, not raw input.
- `TestApplyHealthSettingsWithoutConfigPath...` (L239-L271): `configPath == ""` succeeds and
  still notifies (nothing-to-persist is still a commit).
- `...WithRealDependentsWiredDoesNotPanic` (L280-L316): the production nil-seam fallback branch
  (health.go:483-489) with real `health.NewCanary`/`NewProgressWatchdog` — proves only
  error/no-mutation/no-panic (doc admits the weaker observable, L277-L279).
- `TestApplyHealthSettingsConcurrentApplyKeepsDependentsInSyncWithCommit` (L347-L432): the
  `healthApplyMu` serialization proof (PR #160 Major finding): A commits sA then blocks inside the
  canary seam; a single `TryLock` probe on `m.healthApplyMu` (L389) decides deterministically which
  interleaving is driven — lock held ⇒ fix present, B blocks until A returns; lock free ⇒ B is
  driven to full completion synchronously before A resumes (the bad interleaving). Either branch
  converges on: both seams' last-observed configs equal final `CurrentHealthSettings()` (B's), 2
  calls each (L413-L431). The probe cannot race A because A's `healthApplyMu.Lock()` is
  ApplyHealthSettings' first statement if it exists at all (doc L338-L346; health.go:456).

**Audit points (production, as pinned):** MUTATION = candidate clone, under `m.mu`
(health.go:459-463) — the LIVE config is never touched pre-persist. PERSISTENCE = `SaveConfig`
on the candidate, under `m.mu` AND `healthApplyMu` (health.go:456,464-469). PUBLISH =
`m.config = candidate` under the same `m.mu` critical section (health.go:471), strictly after save
success. DEPENDENT side effects = canary/watchdog notification AFTER `m.mu.Unlock()` but still
under `healthApplyMu` (health.go:472-490, "Load-bearing ordering" comment L474-L480). Error
propagation: SaveConfig error → wrapped `"health settings apply rejected; no changes were made:
%w"` (health.go:468) → fenced wrapper → HTTP handler.

**Determinism:** channel-based rendezvous only (`aAtCanary`/`aResume`), no sleeps/polling
(doc L330-L332); the one branch decision is a single TryLock whose outcome is fixed by whether the
fix exists, not by scheduling.

**Open Questions:** the concurrent test's spy closes over `aPaused` under its own mutex — first
caller is assumed to be A because A is started first and B cannot enter the seam before A under
either branch's driving; this holds by construction but is subtle.

---

## breakConfigPath in internal/app/generation_config_test.go (L269-L281)

**Purpose:** internal/app's clone of `breakConfigPathForNextSave` (same body: `os.Remove` +
`os.Mkdir` 0o755; doc L269-L272 cites cp1_c2_matrix_test.go by name). Exists because the miner's
helper is unexported in another package. Same observable, same determinism argument, same
"still-a-directory ⇔ nothing written" equivalence.

**Inputs & Assumptions / Outputs & Effects:** identical to the miner seam (see above); path is the
genHarness's seeded config.json (L71-L73).

**Cross-Function Dependencies:** Callers (working tree): 
`TestRejectedSettingsCandidateNeverReachesNextGeneration` (L311) and
`TestRejectedInPlaceWriteNeverReachesNextGeneration` (L549). At HEAD the second caller is
`TestInPlaceRuntimeWriteSurvivesAFailedPersist`. Duplication with the miner helper is unenforced —
**nothing found** keeps the two bodies in sync beyond the comment cross-reference.

**Open Questions:** none beyond the shared-seam ones.

---

## TestRejectedSettingsCandidateNeverReachesNextGeneration in internal/app/generation_config_test.go (working tree L294-L331; present at HEAD)

**Purpose:** Extend the miner-level fail-closed contract across the GENERATION boundary for the
candidate-publishing paths: a settings candidate whose persistence failed must be invisible to
generation N+1, which must start from the last successfully COMMITTED config — never the rejected
value, never the process-boot snapshot.

**Inputs & Assumptions:**
- The genHarness (L54-L199): a REAL `App` built by `buildWith` (L78), a harness-OWNED
  `lifecycle.Controller` whose Factory calls the App's real `minerFactory` (L93-L104) but returns a
  controllable `ctrlFakeRunner` instead of running the miner (device-code OAuth is unreachable in a
  unit test, doc L35-L39). EXPLICIT coverage limits (doc L41-L50): buildWith's own Factory closure,
  persistence decorator, status sink, flags, and updater wiring are NOT exercised — only
  `app.minerFactory` beneath them; a regression in that closure would not be caught here. Enforcer
  for that gap: **nothing found** (documented honestly, not covered elsewhere in this file).
- `commitCanary` (L213-L218) drives the REAL commit path `miner.ApplyHealthSettings` — the exact
  entry point POST /api/health/settings calls.
- Replacement realism: `replaceGeneration` (L178-L199) drives a REAL pause → runner-finish →
  observed-paused → resume cycle on the real controller.

**Outputs & Effects (assertion chain):** commit `committed-config-B` (succeeds, L306-L308);
`breakConfigPath` (L311); commit `rejected-config-C` must ERROR (L312-L315); gen1 still reports
the committed value (L316-L319); after a real replacement, gen2's `CanaryChannel` is switched on
three-way (L323-L330): committed = correct; rejected = "leaked into a later generation"; anything
else (incl. boot) = wrong baseline.

**Audit points:** PUBLISH is the observable under test twice removed: (1) miner-level — the
rejected candidate never becomes `m.config` (health.go:465-471 ordering), (2) app-level — the
handoff source (`CurrentConfig`-derived snapshot, per doc L457-L467 of the sibling isolation test)
therefore can never carry it. Error propagation: `SaveConfig` → `ApplyHealthSettings` →
`commitCanary` → test. Locks: as in the health section above; the handoff snapshot's isolation
(separate map identities across generations) is pinned by
`TestGenerationConfigHandsOverAnIsolatedSnapshot` (L468-L507) as a memory-safety contract (one map
behind two mutexes ⇒ un-recoverable `concurrent map read and map write` throw, doc L455-L462).

**Determinism:** `waitForCond` polling (5s cap) is used only for lifecycle progress
(generation-count/ObservedPaused), never for the assertion values; the commits and reads are
synchronous method calls; runner start/finish are channel-signalled (L96, L168-L171, L185-L189).

**Open Questions:** none specific.

---

## TestInPlaceRuntimeWriteSurvivesAFailedPersist (HEAD ~L505-L574) → replaced by TestRejectedInPlaceWriteNeverReachesNextGeneration (working tree L533-L576)

**Purpose (HEAD version):** A deliberate CHARACTERIZATION of the then-true asymmetry: the in-place
writers (`SetDropRule`, `ApplyCampaignPolicy`) mutate the live config under `m.mu` and call
`persistLocked`, which at HEAD only LOGS a SaveConfig failure and returns nothing
(HEAD policy.go:220-226) — so the change stays live, the dashboard reports success, and the
generation handoff carries the never-persisted value into generation N+1 unconditionally. The HEAD
test asserts exactly that: `SetDropRule` returns nil after `breakConfigPath` ("persist failure is
fail-open, not an error return"), the rule is live on gen1, nothing reached disk (IsDir), and gen2
INHERITS the unpersisted rule. Its own comment pre-authorizes the flip: "If they are ever made
fail-closed, this test SHOULD fail, and updating it is then the deliberate act of recording that
decision."

**Working-tree replacement:** That is exactly what happened (doc L513-L532 records it as a
"DELIBERATE CHARACTERIZATION UPDATE"). The in-place writers now make persistence the commit point
(working-tree policy.go: `ApplyCampaignPolicy` L302-L320, `commitDropRule` L363-L394 — exact
rollback under the SAME `m.mu` critical section, non-nil error, `refreshPolicy` only on the
success path after Unlock). The new test asserts the inverted chain: seed a COMMITTED rule
(L544-L546, so rollback is shown to preserve, not empty, prior state); `breakConfigPath` (L549);
`SetDropRule` must ERROR (L550-L553); the rejected rule is not live on gen1 (L556-L558); disk
untouched (L562-L564); after a real replacement, gen2 carries the committed rule and NOT the
rejected one (L566-L575).

**Audit points (working-tree production, as pinned):** MUTATION point lock: `m.mu` (policy.go:305,
364-374). PERSISTENCE point lock: `m.mu` — `persistLocked` is called while holding it, the
documented "SaveConfig-under-m.mu invariant" (policy.go:222-231). PUBLISH points: (a) the
mutation IS the publish for lock-guarded readers (`CurrentCampaignPolicy`,
`snapshotDropRules`, `cloneConfigLocked`, the generation handoff) — hence rollback must complete
INSIDE the same critical section before Unlock (policy.go:308-312, 375-387); (b) the lock-free
published snapshot `m.policySnap.Store` happens only in `refreshPolicy` (policy.go:60), which runs
only post-success, after Unlock (policy.go:317, 392). Dependent side effects relative to
persistence: `refreshPolicy` (watcher scores, discovery ranks, snapshot store) fires strictly AFTER
successful persistence; on failure nothing fires. Error propagation: `SaveConfig` →
`persistLocked` (now returns the error, policy.go:226-231) → wrapped
`"...rejected; no changes were made: persist config: %w"` (policy.go:314, 389) → `fenced` →
caller. Map-identity note: `commitDropRule` mutates and restores the DropRules map IN PLACE,
including re-nilling (policy.go:366-386), preserving both the nil-vs-empty distinction and the
`cloneConfigLocked` [R7] aliasing invariant it cites.

**Determinism:** same as the sibling app-level test; single-goroutine method calls around a
state-based filesystem fault.

**Open Questions:**
- The task brief names the HEAD test at "L283-L574"; the working tree renames/inverts it. Any
  audit of the final PR should confirm the characterization-update note (L513-L532) survives into
  the committed history, since it is the only record that the old fail-open behavior was real and
  deliberately pinned once.

---

## Fence HTTP harness: newFenceMiner (L21-L54) / startLiveGeneration (L78-L143) (+ startRetiredGeneration L56-L72, waitDialable L145-L158, postForm L160-L168, diskPolicy L170-L177) in internal/miner/stale_generation_fence_test.go

**Purpose:** Bring a REAL miner generation up (and optionally retire it) through the real `Run`
control flow with a real process-level `web.Server`, so HTTP-level tests cross the genuine
ownership path: handler → provider registered by `setupComponents` → miner method → fence →
commit point → real config.json.

**Inputs & Assumptions:**
- `newFenceMiner` (L21-L54): real config.json seeded at an absolute path (no `t.Chdir`, L23-L24);
  `database.Open` hands back a process-wide singleton that is DELIBERATELY not closed
  (L31-L34) — enforcer of "no test may close it": **nothing found** (comment discipline only);
  config trimmed of network features (no streamers, analytics/discord/debug off, L39-L45);
  `CampaignPolicy` seeded to `ModeGameOrder` (L45) — the "prior committed value" every fence/RED
  assertion compares against.
- `startLiveGeneration` (L78-L143): reserves a port by listen-then-close probe (L81-L85) —
  TOCTOU between `probe.Close()` and `ws.Start()` is unenforced (**nothing found**; mitigated only
  by immediacy and `waitDialable`); builds `web.NewServerEarly` and registers it on the miner
  (L91-L99); stubs auth, streamer loading, topic subscription, and replaces `startMining`'s
  loop-starting section with a channel-close (`started`, L104-L112) — CRUCIALLY,
  `setupComponents` itself is NEVER stubbed (comment L109-L111), so the real provider registration
  under test has already happened when `started` closes; runs `m.Run(ctx)` on a goroutine and
  waits on `started` (L114-L124).
- `retire()` (L126-L139): `sync.Once`-guarded cancel + wait for `Run` to return (10s cap) — what
  "genuinely retires the generation" means here; registered as cleanup too (L140).
- `startRetiredGeneration` (L56-L72) = live + immediate retire: yields a torn-down generation
  still reachable through the still-registered web providers — precisely the lifecycle
  replacement gap state.

**Outputs & Effects:** `(baseURL, retire)`; the miner is LIVE and authoritative (fence admits
mutations) until `retire()` runs, after which `fenced` refuses with `ErrShuttingDown`
(miner.go:2137).

**What the in-file tests prove with it (context for the RED file):**
- Retired refusals over real HTTP: `/api/policy/mode` (L193-L226), `/api/policy/drop-rule`
  (L233-L265), `/api/health/settings` (L270-L302) → 503 (`writeApplyError` /
  `writePolicyMutationError` mapping, handlers_policy.go:134-143), runtime unchanged, disk
  unchanged.
- `SetAutoRedeem` fence at the provider seam, with the documented reason HTTP cannot reach it
  (routing guard resolves the streamer first, L304-L341); the live-side roster guard kept covered
  (L352-L365).
- Live generation still accepts end to end (L372-L392); admitted-before-retirement mutation
  survives into the handoff via `CurrentConfig` (applyWG ordering, L394-L431); a provider
  interface value SAMPLED while live cannot mutate after retirement — only the callee refusing
  closes the sample-to-call gap (L445-L471); at most one generation accepts at a time
  (L479-L505).

**Cross-Function Dependencies:** `Miner.Run`/`setupComponents` (real), `web.NewServerEarly` +
provider registration, `m.fenced` (miner.go:2137), `config.LoadConfig` (diskPolicy),
`http.PostForm` (postForm; response body closed via cleanup L166). The RED file imports all four
helpers unchanged.

**Observable proved / determinism:** The harness itself proves reachability of the REAL seam
(nothing is mocked between the HTTP socket and `SaveConfig`). Determinism argument (doc L62-L66):
startup completion and Run-return are channel-awaited; "no wall-clock sleep decides the outcome of
any assertion" — the only polling (`waitDialable`) gates readiness, not assertion values; the 10s
timeouts are failure deadlines, not schedule dependencies.

**Open Questions:**
- Port-reservation race (another process binding the freed port) is accepted flake risk;
  `waitDialable` would even mask it by dialing the foreign listener. Nothing enforces otherwise.
- The shared DB singleton across the whole test binary means fence tests are order-coupled with
  any test that would close it (guarded only by comments here and in cp1_c2_matrix_test.go:15-20,
  srap_test.go:25-29).

---

## RED file: internal/miner/policy_persistence_fail_closed_test.go (untracked; written RED against HEAD 5b331e5)

**Purpose:** Pin the OTHER transactional half of the two runtime policy mutations on a LIVE
generation (the fence file already pins the retired half): with a configured config path,
`config.SaveConfig` succeeding IS the commit point for `ApplyCampaignPolicy` and `SetDropRule`; a
failed save must yield a non-nil error / non-200, prior committed live state, an unrefreshed
policy snapshot, and an untouched disk — "never a false HTTP success over a change that silently
exists only in memory" (doc L13-L26).

**RED/GREEN state:** RED at HEAD by construction: HEAD `persistLocked` (HEAD policy.go:220-226)
swallows the SaveConfig error, so `SetDropRule`/`ApplyCampaignPolicy` return nil, the handler
answers 200 (handlers_policy.go:120-125 only fails on non-nil error), the mutation stays live, and
`refreshPolicy` publishes a snapshot ranked under the rejected value (HEAD policy.go:289-291,
335-337). GREEN on the current working tree: verified by running all three tests with `-race`
(`ok ... 3.554s`) against the fixed policy.go.

**Composition (three tests, three observables):**
- `TestPolicyModePersistFailureOverHTTPFailsClosed` (L38-L86) — the operator-facing crossing.
  Harness: `newFenceMiner` + `startLiveGeneration` + `breakConfigPathForNextSave` (L39-L48).
  Asserts, in order: (1) status ≠ 200 (L54-L57) and specifically = 500 — a persistence failure on
  a LIVE generation is a server fault, "not the fence's retryable 503 and not the lifecycle 409"
  (L58-L62; mapping seam: `writePolicyMutationError`, handlers_policy.go:134-143, which routes
  non-unavailable errors to `writeInternalError`); (2) live `CurrentCampaignPolicy` still the prior
  committed `ModeGameOrder` (L66-L69) — rejected value rolled back under `m.mu` before Unlock;
  (3) `PolicySnapshot()` never carries `ModeSmart` (L75-L78) — the dependent side effect
  (`refreshPolicy` → `policySnap.Store`) must fire only past the commit point (on the unfixed base
  it fires regardless); (4) configPath still the installed directory (L83-L85) — nothing reached
  disk.
- `TestDropRulePersistFailureOverHTTPFailsClosed` (L94-L135) — same commit point, second
  mutation, MAP-shaped state. Additional observable: nil-vs-empty exactness — if `DropRules` was
  nil before the failed mutation, rollback must restore NIL, not leave an allocated empty map
  (L100, L124-L130), because `CurrentConfig` snapshots deliberately preserve nil-ness
  (current_config_test.go, cited L124-L126) and every later snapshot would otherwise change shape.
  Enforced in the fix by `commitDropRule`'s `wasNil` re-nilling (working-tree policy.go:366-386).
- `TestFailedDropRulePersistCannotBeLaunderedByLaterSave` (L149-L195) — the LAUNDERING half, at
  the provider-method seam (causal chain, not HTTP; doc L137-L148). Because `SaveConfig` is
  whole-document persistence (marshals the ENTIRE live config, config.go:726-738), on the unfixed
  base a rejected-but-still-live rule is written to disk incidentally by the NEXT successful save
  of ANY unrelated mutation. Sequence: commit `kept-rule` (L161-L163, proves rollback does not
  discard committed state); break path; `SetDropRule(laundered-rule)` must error (L165-L169);
  REPAIR the path (`os.Remove` of the directory, L172-L174 — the only seam user that exercises
  the repair-then-save flow); `ApplyCampaignPolicy(ModeEndingSoonest)` succeeds (L175-L177);
  reload disk: laundered key ABSENT (L183-L186), kept key PRESENT (L187-L190), the later save's
  own mode committed (L191-L194).

**Audit points (the contract these tests enforce on working-tree policy.go):**
- MUTATION point lock: `m.mu.Lock()` (policy.go:305 mode; 364-374 rule).
- PERSISTENCE point lock: `m.mu` still held — `persistLocked` called inside the critical section
  (policy.go:307, 375; invariant note policy.go:222-231). Plus the fence's `applyWG` admission
  around the whole body (`fenced`, miner.go:2137).
- PUBLISH point locks: rollback-before-Unlock makes failure unobservable to every `m.mu` reader
  (policy.go:308-312, 376-387); the lock-free publish (`policySnap.Store`, policy.go:60) happens
  only in `refreshPolicy`, invoked post-Unlock on the success path only (policy.go:317, 392) —
  `refreshPolicy` itself re-takes `m.mu.RLock` (policy.go:32-35), which is WHY it must run after
  Unlock.
- Error propagation: `os.Rename` (file.go:55-56) → `SaveConfig` (config.go:738) →
  `persistLocked` (policy.go:230) → wrapped `"...rejected; no changes were made: persist config:
  %w"` (policy.go:314/389) → `fenced` → `web.PolicyProvider` → `writePolicyMutationError` → 500
  (handlers_policy.go:137-143; `mutationRefusedAsUnavailable` must NOT match a persist error —
  the wrapped chain contains no `ErrShuttingDown`).
- Dependent side effects vs persistence: watcher campaign scores, discovery game ranks, and the
  snapshot store all live inside `refreshPolicy` and fire strictly AFTER a successful persist;
  never on failure. The HTTP re-render (`renderDropsList`) likewise only fires on nil error
  (handlers_policy.go:120-125, 184-190).

**Determinism:** inherits every argument of its two building blocks — the rename-onto-directory
filesystem invariant (privilege-independent) and the channel-synchronized live-generation harness;
each HTTP POST is a single synchronous request whose response is read before any state assertion;
no goroutine other than the server handles the mutation; assertions are direct reads
(`CurrentCampaignPolicy`, `PolicySnapshot`, `os.Stat`, `LoadConfig`) after the response returned.
One residual ordering dependence: the state assertions assume the handler completed the mutation
attempt before writing the response — true because the handler calls the provider synchronously
before writing (handlers_policy.go:120-125).

**Open Questions:**
- The 500-vs-503 mapping assertion depends on `mutationRefusedAsUnavailable`'s classification
  never matching a persist-failure chain; the adjacent untracked
  `internal/web/policy_persistence_status_test.go` (not analyzed here) presumably pins that
  web-side mapping directly — confirm.
- `TestFailedDropRulePersistCannotBeLaunderedByLaterSave` repairs the path by removing the empty
  directory only; it relies on `WriteFileAtomic` recreating the file via rename (no pre-existing
  file needed) — holds today (file.go:21-55), unenforced against future SaveConfig changes.
- The untracked sibling `internal/miner/policy_persistence_matrix_test.go` was not in this task's
  scope; its relationship to the three observables above (matrix expansion?) is unrecorded.
