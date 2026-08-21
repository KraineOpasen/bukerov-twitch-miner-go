# Generation handoff: nextGenerationConfig / sampleCurrentMiner / SaveConfig / WriteFileAtomic

Audit-context pass at HEAD 5b331e5. Understanding only — no verdicts, no fixes, no severity.
All paths absolute under /home/user/bukerov-twitch-miner-go.

Audit-specific ledger (summary; per-function detail below):

| Function | Lock at MUTATION point | Lock at PERSISTENCE point | Lock at PUBLISH point |
|---|---|---|---|
| `nextGenerationConfig` | `a.cfgMu` (app.go:717-720) | none — no persistence step by design (app.go:631-632) | `a.cfgMu` (same assignment; return value at app.go:722 is read under the still-held deferred lock) |
| `sampleCurrentMiner` | no mutation | no persistence | pointer read under `a.currentMinerMu` (app.go:731-733) |
| `SaveConfig` | mutates only its local shallow copy `toWrite` (config.go:726-729) — no lock of its own | `util.WriteFileAtomic` (config.go:738) — no lock of its own; the caller's `m.mu` is held across the call on every miner writer path except owner-identity (see below) | publishes nothing — callers publish after it returns success |
| `WriteFileAtomic` | filesystem only: temp file write (file.go:34) | `tmp.Sync()` + `os.Rename` (file.go:41, 55) | the `Rename` IS the publish — atomic swap visible to readers at file.go:55 |

Error propagation (summary): `nextGenerationConfig` and `sampleCurrentMiner` are infallible by
construction (app.go:631-632, 653). `WriteFileAtomic` wraps each failing step with a labeled
`%w` and removes the temp file on any pre-rename failure (file.go:27-32); `SaveConfig` returns
the marshal error unwrapped or the `WriteFileAtomic` error as-is (config.go:731-738); miner
callers turn that into their commit-point failure (e.g. miner.go:2317-2320, rewards.go:200-217)
— except the owner-identity reconciliation, which only WARNS (miner.go:690-692).

Side-effect ordering (summary): in the writer paths, persistence is the commit point — publish
of `m.config` and every dependent side effect (budget-window reset, `finishApply`, CommitPlan)
fire strictly AFTER a successful `SaveConfig` (miner.go:2297-2302, 2317-2332; rewards.go:219-224).
`nextGenerationConfig` itself has no side effects beyond the `a.cfg` pointer swap.

---

## nextGenerationConfig in internal/app/app.go (L619-L723)

**Purpose:** Return the configuration the NEXT miner generation must be constructed from, and
record it as the process-level authoritative one (app.go:619-620). Semantics are a HANDOFF, not
a fallback: each generation owns its config for its lifetime; when it ends, whatever it last
committed becomes current, and the next generation starts from it. The process-boot snapshot is
used only for the first generation (app.go:622-629).

**Inputs & Assumptions:**
- Receiver `a *App`. Reads `a.currentMiner` (via `sampleCurrentMiner`) and `a.cfg`.
- `a.cfg` is non-nil: seeded in `buildWith` at app.go:224 from the `cfg` parameter, which
  `validateConfig` rejects when nil (app.go:859-862). Enforced.
- Trust in the sampled miner's `CurrentConfig()`: it returns the value that miner last
  COMMITTED — enforced by the writers' commit-point discipline: candidate-publishing paths
  publish into `m.config` only past a successful `config.SaveConfig` (miner.go:2297-2300,
  2316-2322), and in-place writers (`SetAutoRedeem`, `SetDropRule`, `ApplyCampaignPolicy`)
  mutate, persist under `m.mu`, and roll back exactly on a failed save (rewards.go:196-217).
  Documented exception: the owner-identity reconciliation in `Run` mutates `m.config` off
  `m.mu`, saves off the lock, and only warns on failure (miner.go:680-693), so that one value
  can be carried forward by this handoff without having reached disk (app.go:638-642,
  miner.go:2052-2057).
- Ordering precondition — "the sampled miner is the OUTGOING, already-returned generation":
  NOT checked here; guaranteed by the lifecycle core (app.go:668-670). Enforcer: the factory
  runs on the controller's single main loop — `launchFreshGeneration` calls `c.cfg.Factory()`
  synchronously (worker.go:622, 633), which is `app.minerFactory` (app.go:458-460), which calls
  this function (app.go:361) — and a replacement is only reached after `awaitGeneration` has
  observed the outgoing generation's `Run` return (worker.go:777 before worker.go:795 on the
  restart path; worker.go:753/821 on wind-down paths).
- Drain precondition — "no config writer is still mutating the outgoing generation": enforced
  by `Miner.fenced` — `ApplyHealthSettings`, `ApplyCampaignPolicy`, `SetDropRule`,
  `SetAutoRedeem` all refuse with `ErrShuttingDown` on a draining/retired generation, before
  any side effect (app.go:672-677, miner.go:2106-2119, rewards.go:166-168), plus
  `beginApply`/`applyWG` draining the applySettings path (miner.go:2087-2098).
- Two documented limits on that drain claim (app.go:704-710): (1) it covers CONFIGURATION
  mutation only — non-config provider methods (`RedeemCustomReward`) can still execute on a
  retired generation; (2) "admitted implies present in this sample" is bounded by
  applySettings' own clone window, where a concurrent commit can be overwritten by that
  apply's publish — a pre-existing residual. Nothing found enforcing either beyond the
  documentation.
- Deliberate NON-input: config.json is NOT reloaded. Rationale (app.go:644-653): (a)
  `config.LoadConfig` re-reads `DISCORD_BOT_TOKEN` from the environment and re-derives
  `DiscordTokenFromEnv` (config.go:649, 658-663), which would make the generation boundary a
  second environment-read point, breaking the bootstrap-once discipline; (b) `SaveConfig`
  intentionally does NOT persist an env-managed token (config.go:726-729), so a reloaded object
  is not the object the generation committed. Reading the committed value directly is exact and
  infallible — which is also why `lifecycle.Factory` needs no error return (app.go:650-653).

**Outputs & Effects:**
- Returns `*config.Config`: the outgoing generation's committed snapshot when one exists, else
  the current `a.cfg` (boot snapshot before any generation ever existed). Never nil.
- State write (MUTATION point): `a.cfg = committed` at app.go:720, under `a.cfgMu`
  (Lock at app.go:717, deferred Unlock at app.go:718). This is also the PUBLISH point for the
  process-level authoritative pointer; the `return a.cfg` at app.go:722 evaluates while the
  deferred unlock is still pending, so the read is under the lock too.
- PERSISTENCE point: none. "There is deliberately no fallible step in THIS function"
  (app.go:631-632) — durability was the outgoing generation's writers' job.
- Error propagation: none — no error return, no panic path (the only dereference, of
  `outgoing`, is nil-guarded at app.go:713).
- Postcondition/ownership contract: `cfgMu` guards the POINTER, never the pointee; `cfg` is a
  handoff baton valid only as the factory's input — no code outside this function may
  dereference `a.cfg`, because its fields include maps another goroutine (the generation it
  was handed to) writes under a DIFFERENT mutex, `m.mu` (app.go:109-119).
- Dependent side effects downstream: the returned pointer becomes the new miner's live
  `m.config` via `f.newMiner(app.nextGenerationConfig(), rc.ConfigPath)` (app.go:361).

**Block-by-Block:**

- app.go:712-715
  ```go
  var committed *config.Config
  if outgoing := a.sampleCurrentMiner(); outgoing != nil {
      committed = outgoing.CurrentConfig()
  }
  ```
  What: sample the most recently built miner under `currentMinerMu`, then — with NO App lock
  held — ask it for an isolated snapshot of its committed config.
  Why here: the first generation has no predecessor (`outgoing == nil`), and later generations
  must inherit what the outgoing one committed, not the boot snapshot (app.go:96-102 describes
  the pre-fix defect: settings silently reverted at every pause/resume/restart).
  Assumes: `CurrentConfig` returns non-nil (it returns `&snap` of a value copy,
  miner.go:2717-2737 — always non-nil when `m.config` is non-nil; `m.config` is non-nil
  because every miner is constructed from this function's non-nil return). Assumes the
  lock-ordering discipline "miner's own RLock with no App lock held" (app.go:655-658) — held
  true structurally: `sampleCurrentMiner` releases `currentMinerMu` before returning
  (deferred unlock, app.go:732), and `cfgMu` is taken only afterwards (app.go:717).
  Establishes: `committed` is an ISOLATED snapshot sharing no map with the live object
  (miner.go:2684-2694, 2717-2737) — so no two generations ever share a map (app.go:700-702).
  Depended on by: the `a.cfg = committed` publish below; the new miner built from the return.
  Blocking note (app.go:658-666): `CurrentConfig` takes `m.mu.RLock` (miner.go:2075), and most
  config writers hold `m.mu` across `config.SaveConfig`, so a commit landing on the outgoing
  generation at this instant delays the sample by one atomic file write. WHO waits: the
  controller's single main loop (worker.go:633), so the wait also defers pause/stop/resume
  dispatch and the shutdown path; `WriteFileAtomic` fsyncs with no timeout, so on a wedged
  filesystem the delay is bounded by nothing (app.go:665-666).

- app.go:717-722
  ```go
  a.cfgMu.Lock()
  defer a.cfgMu.Unlock()
  if committed != nil {
      a.cfg = committed
  }
  return a.cfg
  ```
  What: publish the sampled snapshot as the process-authoritative config (only when a
  predecessor existed) and return the authoritative pointer.
  Why here: keeps first-generation semantics (boot snapshot IS authoritative, app.go:626-629)
  and later-generation handoff in one place, under one lock.
  Assumes: no other goroutine dereferences `a.cfg` (the ownership contract at app.go:109-119);
  the only other writer of `a.cfg` is `buildWith`'s seeding at app.go:224, which happens-before
  any factory call.
  Establishes: MUTATION and PUBLISH both under `cfgMu`; the returned pointer is exactly what
  the next generation is constructed from.
  Depended on by: `app.minerFactory` (app.go:360-381), which passes it to `miner.New`, then
  publishes the new miner under `currentMinerMu` BEFORE returning (app.go:373-379) so the
  updater's accessor never observes a half-wired miner.

**Cross-Function Dependencies:**
- Callees: `a.sampleCurrentMiner` (pointer sample under `currentMinerMu`, app.go:730-734);
  `Miner.CurrentConfig` (isolated snapshot under `m.mu.RLock`, miner.go:2074-2078) →
  `snapshotConfigLocked` (copies the three maps `AutoRedeem`, `DropRules`,
  `Notifications.ProviderBatching`; slices deliberately stay shared, miner.go:2698-2708,
  2717-2737).
- Callers: exactly one production caller — `app.minerFactory` (app.go:361), invoked by
  `lifecycle.Controller.launchFreshGeneration` via `c.cfg.Factory()` (worker.go:633,
  app.go:458-460). The caller assumes the return is non-nil and safe to hand to a fresh
  `*miner.Miner` as its live config.
- Shared state: `a.cfg`/`a.cfgMu` (app.go:120-121); `a.currentMiner`/`a.currentMinerMu`
  (app.go:146-147); transitively the outgoing miner's `m.config`/`m.mu`.
- Invariant couplings: (1) single-active-generation + awaitGeneration ordering
  (worker.go:692-702, 777, 795) is what makes the sample "the committed final state" rather
  than a mid-run read; (2) the mutation fence (`Miner.fenced`) is what makes the drain claim
  cover all four config writers (app.go:672-677); (3) `snapshotConfigLocked`'s slice-sharing
  argument leans on `beginApply`/`applyWG` draining slice-element writers before `Run` returns
  (miner.go:2698-2708); (4) the no-reload rationale couples to `SaveConfig`'s
  env-token-clearing behavior and `LoadConfig`'s env re-read (config.go:649, 658-663, 726-729).

**Open Questions:**
- The applySettings clone-window residual (app.go:708-710): the doc says a concurrent commit
  can still be overwritten by that apply's publish. Whether any sequence reachable at a
  generation boundary (as opposed to mid-generation) can hit it is not established here.
- The owner-identity reconciliation mutates `m.config` fields off `m.mu` entirely
  (miner.go:680-693, "mutates m.config off m.mu entirely" per miner.go:2052-2054). This
  function never samples a miner whose `authenticate` is still running (awaitGeneration
  ordering), so the handoff read is safe — but the same off-lock write versus that miner's OWN
  concurrent `m.mu`-holding readers during its run is outside this function's guarantees and
  not settled by anything read in this pass.

---

## sampleCurrentMiner in internal/app/app.go (L725-L734)

**Purpose:** Return the miner of the most recently built generation, or nil before the first
one exists (app.go:725-726). It is the single App-level accessor for "the current (or most
recent) generation's miner".

**Inputs & Assumptions:**
- Receiver `a *App`; reads `a.currentMiner` under `a.currentMinerMu`.
- `a.currentMiner` is set by `minerFactory` itself, BEFORE the factory returns the miner,
  under `currentMinerMu` (app.go:140-141, 377-379). It is NEVER cleared on teardown
  (app.go:142-145): a stale-but-stopped miner may be returned, deliberately — its
  `NotificationManager()` is still safe to call, and the next generation's factory call
  overwrites it before that generation's `Run` starts.
- Discipline contract (app.go:726-729): sample under the mutex, act on the result AFTER
  releasing it — the accessors it feeds (`NotificationManager`, `CurrentConfig`) take the
  miner's own locks, and none may be entered while an App lock is held. Enforcer: convention
  only, upheld by both call sites' structure (app.go:713-714 and the updater accessor per
  app.go:135-145); nothing mechanical prevents a future caller from calling into the miner
  while still holding an App lock — nothing found.

**Outputs & Effects:**
- Returns `*miner.Miner` or nil. Pure read; no mutation, no persistence, no error. The
  PUBLISH point of the value it reads is in `minerFactory` (app.go:377-379), under the same
  `currentMinerMu` — a correctly paired happens-before edge.

**Block-by-Block:**

- app.go:730-734
  ```go
  func (a *App) sampleCurrentMiner() *miner.Miner {
      a.currentMinerMu.Lock()
      defer a.currentMinerMu.Unlock()
      return a.currentMiner
  }
  ```
  What: mutex-guarded pointer read.
  Why here: gives every App-level reader one blessed way to resolve the live generation
  without the reader (e.g. the updater loop, which outlives any generation) having a
  generation concept of its own (app.go:135-139).
  Assumes: writers of `a.currentMiner` also hold `currentMinerMu` — true for the only writer,
  app.go:377-379.
  Establishes: the returned pointer is a fully wired miner (published only after
  `SetDashboardConfig`/`SetDatabase`/`SetAnalyticsService`/`SetWebServer`, app.go:361-379).
  It may be RETIRED — the caller must tolerate that.
  Depended on by: `nextGenerationConfig` (retired is exactly what it wants: the outgoing
  generation's last committed state) and the updater's notify accessor (retired is safe:
  Stop closes dispatch admission, app.go:142-144).

**Cross-Function Dependencies:**
- Callers: `nextGenerationConfig` (app.go:713); the updater notify accessor described at
  app.go:135-145; tests (build_test.go:108-134 verify the publish-before-return contract).
- Shared state: `a.currentMiner` under `a.currentMinerMu` — a mutex distinct from `cfgMu`,
  never nested with it (app.go:655-658).
- Invariant coupling: "never cleared on teardown" trades a nil-check for the requirement that
  every consumer be retired-miner-safe; for config that safety is `fenced` +
  `CurrentConfig`'s snapshot isolation (miner.go:2059-2073).

**Open Questions:** none beyond the convention-only lock-ordering note above.

---

## SaveConfig in internal/config/config.go (L721-L739)

**Purpose:** Serialize the ENTIRE in-memory `Config` to pretty-printed JSON and atomically
replace `config.json` with it, deliberately omitting an environment-managed Discord bot token.

**Inputs & Assumptions:**
- `path string`: destination file; callers pass `m.configPath` (may be "" — but every miner
  call site guards `configPath != ""` BEFORE calling, e.g. miner.go:689, 2316,
  rewards.go:199; `SaveConfig` itself does not check, and an empty path would fail inside
  `WriteFileAtomic` at `CreateTemp`).
- `config *Config`: trusted, assumed non-nil (dereferenced at config.go:726 — nil-guard:
  nothing found; every caller passes a live config or candidate).
- Concurrency assumption: the document must not be mutated mid-marshal. `SaveConfig` takes NO
  lock of its own; the shallow copy `toWrite := *config` (config.go:726) still SHARES every
  map and slice with the caller's object, and `json.MarshalIndent` reads them
  (config.go:731). Enforcer: the callers' lock discipline — in-place writers persist while
  holding `m.mu` ("Persist while holding the lock, mirroring ApplySettings, so the config
  isn't mutated by another goroutine mid-marshal", rewards.go:196-201), candidate paths hold
  `m.mu` across the save of a private candidate (miner.go:2311-2323). Exception: the
  owner-identity save at miner.go:690 runs off `m.mu`; nothing found enforcing mid-marshal
  stability for that one call (its safety argument is temporal: it runs during `authenticate`,
  before `setupComponents` registers this generation's web providers, while the previous
  generation's still-registered providers are fenced).
- Whole-document semantics assumption: the caller's in-memory config IS the whole truth of
  the file. Any field the writer did not touch is rewritten from the writer's copy — which is
  exactly why a STALE generation's write could revert fields a newer generation had committed
  (app.go:695-702), and why the fence exists. Enforcer of "only the authoritative generation
  writes": `Miner.fenced` (miner.go:2106+), not anything in this function.

**Outputs & Effects:**
- On success: `config.json` at `path` atomically replaced with the new document, mode 0600;
  returns nil. On failure: returns the error; the file is untouched (see `WriteFileAtomic`).
- MUTATION point: only the local `toWrite` copy is mutated (config.go:726-729); the caller's
  object is never written (Discord is a by-value struct field, config.go:110, so clearing
  `toWrite.Discord.BotToken` cannot alias back).
- PERSISTENCE point: `util.WriteFileAtomic(path, data, 0o600)` (config.go:738). Lock held
  there: whatever the caller holds — `m.mu` on the standard writer paths, nothing on the
  owner-identity path.
- PUBLISH point: none here. Publishing the new config into `m.config` is the CALLER's act,
  strictly after this returns nil (miner.go:2322; for in-place writers the memory write
  precedes the save but is rolled back exactly on failure, rewards.go:202-217 — persistence
  remains the commit point either way).
- DiscordTokenFromEnv clearing (config.go:722-729): with `DISCORD_BOT_TOKEN` set at load
  (config.go:658-663) the env is the source of truth and the token is deliberately NOT
  persisted — the on-disk copy is cleared on EVERY save. Documented consequence: removing the
  env var later does not restore a file value; the token must be re-entered
  (config.go:724-725). `DiscordTokenFromEnv` itself is `json:"-"` (config.go:118), so the
  flag never round-trips through the file; only `LoadConfig`'s env read re-derives it — the
  precise reason `nextGenerationConfig` must NOT reload (app.go:644-653).
- Error propagation: marshal error returned unwrapped (config.go:732-733; practically
  unreachable for this struct — no channels/funcs/cycles — nothing found that could trigger
  it); `WriteFileAtomic` errors returned as-is with their step-labeled wrapping. Upstream:
  `setAutoRedeem` wraps as `"failed to save config: %w"` after exact in-memory rollback
  (rewards.go:202-217) → surfaces to the web handler; `applySettingsNoRename` wraps as
  `"settings apply rejected; no changes were made: persist config: %w"` with zero mutation
  published (miner.go:2317-2320); analogous candidate paths at miner.go:2441, 2601,
  health.go:465; owner-identity path logs `slog.Warn` and continues (miner.go:690-692);
  `cmd/miner` `-generate-config` returns/reports it (main.go:186).
- Dependent side effects relative to persistence: on the writer paths, ALL dependent effects
  are gated on success — publish `m.config`, budget-window reset + generation bump
  (rewards.go:219-224), `CommitPlan` + `finishApply` (miner.go:2325-2332; finishApply
  performs no persistence and cannot fail, miner.go:2749-2755).

**Block-by-Block:**

- config.go:726-729
  ```go
  toWrite := *config
  if config.DiscordTokenFromEnv {
      toWrite.Discord.BotToken = ""
  }
  ```
  What: shallow value copy; blank the token on the copy when the env owns it.
  Why here: keeps the secret out of the file without mutating the caller's live config (which
  still needs the token at runtime).
  Assumes: `Discord` is a by-value field (config.go:110) so the write is isolated; shared
  maps/slices in the copy are not concurrently mutated (caller's lock, see above).
  Establishes: `toWrite` is the exact document to persist — the caller's document minus an
  env-managed secret.
  Depended on by: the marshal below; the README-documented "token must be re-entered"
  contract; `config_secrets_test.go` (verifies the file never contains the env token and that
  `DiscordTokenFromEnv` round-trips as false without the env var).

- config.go:731-734
  ```go
  data, err := json.MarshalIndent(&toWrite, "", "  ")
  if err != nil {
      return err
  }
  ```
  What: whole-document serialization, human-readable indentation.
  Why here: config.json is user-editable; indentation preserves that.
  Assumes: every field intended for disk is exported with a JSON tag; `json:"-"` fields
  (`DiscordTokenFromEnv`, config.go:118) are intentionally excluded.
  Establishes: `data` is a complete, self-consistent snapshot — partial/field-level updates do
  not exist in this design.
  Depended on by: `LoadConfig`'s unmarshal-over-defaults on the next boot (config.go:640-643)
  and `migrateRotationInterval`'s raw-JSON presence checks (config.go:694-711).

- config.go:735-738
  ```go
  // ... owner-only permissions, and an atomic temp+rename swap ...
  return util.WriteFileAtomic(path, data, 0o600)
  ```
  What: delegate durability + atomicity; 0600 because the file may carry the Discord token
  and is rewritten at runtime by dashboard saves (config.go:735-737; `tightenConfigPermissions`
  migrates pre-hardening 0644 files on load, config.go:665-683).
  Assumes: `path`'s parent directory exists (else "create temp file" fails).
  Establishes: crash-safety — a crash mid-save can never truncate the live file.
  Depended on by: every commit point listed above; the fail-closed persistence tests
  (settings_persistence_test.go:83, policy_persistence_fail_closed_test.go:81 make the RENAME
  step fail to prove rollback).

**Cross-Function Dependencies:**
- Callees: `util.WriteFileAtomic` (atomicity, fsync, perm); `json.MarshalIndent`.
- Callers (production): miner.go:690 (owner-identity, off-lock, warn-only), miner.go:2317/
  2441/2601 (candidate commit points, under `m.mu`), health.go:465 (ApplyHealthSettings
  candidate), rewards.go:200 (SetAutoRedeem in-place, under `m.mu`), policy.go
  (ApplyCampaignPolicy/SetDropRule per miner.go:2048-2052), cmd/miner/main.go:186
  (sample config).
- Shared state: the file at `path` — the single cross-generation, cross-restart persistence
  medium; exclusivity of writers is enforced by `fenced` + `m.mu`, not by any file lock
  (nothing found at the file level).
- Invariant couplings: (1) "persistence is the commit point; an acknowledged mutation is a
  durable one" (app.go:634-638) — every writer path except owner-identity; (2) whole-document
  replace is what makes stale-generation writes dangerous, motivating the fence
  (app.go:695-700); (3) never-persisting the env token is half of the no-reload rationale in
  `nextGenerationConfig` (app.go:648-651).

**Open Questions:**
- The owner-identity save (miner.go:690) marshals `m.config` with no lock held. Its
  in-generation safety rests on the temporal argument that no other writer of THIS miner can
  run before `setupComponents` registers providers — plausible from the fence design
  (miner.go:2110-2115), but no single enforcement point for "no concurrent SaveConfig to the
  same path" was found; it is an emergent property of fence + lock + startup ordering.

---

## WriteFileAtomic in internal/util/file.go (L18-L69)

**Purpose:** Write `data` to `path` with permissions `perm` via a same-directory temp file
followed by a rename, so readers never observe a partially written file and a crash mid-write
cannot truncate the existing one (file.go:9-17). Same swap pattern the self-updater uses to
replace the running binary (file.go:16-17, updater.go:565-570).

**Inputs & Assumptions:**
- `path`: target file; parent directory must exist (enforced only by `CreateTemp` failing —
  file_test.go:61-71 pins the clean failure). Temp lives NEXT TO the target so the rename
  stays on one filesystem and therefore atomic (file.go:14-15, 19-21) — enforced by
  construction (`dir := filepath.Dir(path)`).
- `data []byte`: trusted, written verbatim.
- `perm os.FileMode`: final mode. `CreateTemp` opens 0600, so applying a TIGHTER-or-equal perm
  (config's 0o600) means the file never transitions through a broader mode (file.go:49-50);
  a caller passing 0644 (streakcache.go:90, 129) widens at the chmod, pre-rename.
- Exclusivity assumption: no concurrent writer to the same `path`. Nothing found in this
  function (no file locking); two racing calls each use a unique temp (`CreateTemp` pattern
  `<base>.tmp-*`, file.go:21) so there is no corruption, but the last rename wins silently.
  Enforcement is entirely the callers' (config: `m.mu`/fence as above).

**Outputs & Effects:**
- On success: `path` refers to a new inode containing exactly `data`, mode `perm`; data
  fsynced before the swap; directory entry fsynced best-effort after it; returns nil.
- On failure: returns a step-labeled wrapped error; the temp file is removed by the
  `success`-flag defer (file.go:27-32) and the TARGET IS NEVER TOUCHED — the only operation
  that can affect `path` is the rename itself.
- MUTATION/PERSISTENCE point: `tmp.Write` (file.go:34) then `tmp.Sync` (file.go:41) — data on
  stable storage before publish. PUBLISH point: `os.Rename(tmpName, path)` (file.go:55) — the
  atomic swap; `success = true` is set only after it (file.go:59). No locks anywhere in the
  function; whatever lock the caller holds (usually `m.mu`) is held across every fsync — the
  exact reason app.go:663-666 flags that a wedged filesystem stalls the lifecycle main loop
  with no bound ("WriteFileAtomic fsyncs and has no timeout").

**Block-by-Block:**

- file.go:19-32
  ```go
  dir := filepath.Dir(path)
  tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
  ...
  success := false
  defer func() { if !success { _ = os.Remove(tmpName) } }()
  ```
  What: create a uniquely named temp next to the target; arrange cleanup on ANY failure
  before the rename. Error: `"create temp file: %w"`.
  Assumes: dir exists and is writable.
  Establishes: same-filesystem invariant (atomic rename); no temp litter on failure
  (file_test.go:73+ pins this).
  Depended on by: every subsequent step; the failure modes below.

- file.go:34-47
  ```go
  if _, err := tmp.Write(data); ... "write temp file: %w"
  if err := tmp.Sync(); ...      "sync temp file: %w"
  if err := tmp.Close(); ...     "close temp file: %w"
  ```
  What: write all bytes, force them to stable storage, close.
  Why here: the data fsync BEFORE the rename closes the classic rename-before-data crash
  window — a crash right after the swap cannot leave the target pointing at a rename whose
  data never reached disk, i.e. a truncated/empty file (file.go:38-40, 11-14).
  Assumes: `Write` reporting n < len(data) also reports err (io contract).
  Establishes: temp contains exactly `data`, durably.
  Depended on by: the crash-safety claim `SaveConfig` advertises (config.go:735-737).

- file.go:49-53
  ```go
  if err := os.Chmod(tmpName, perm); ... "chmod temp file: %w"
  ```
  What: apply the caller's mode before the swap (CreateTemp opened 0600).
  Establishes: the target never exists with the wrong mode; for 0600 callers the secret-bearing
  file is never readable by others at any instant (file_test.go:38-59 pins that an existing
  0644 target ends up 0600 after overwrite).

- file.go:55-59
  ```go
  if err := os.Rename(tmpName, path); return fmt.Errorf("replace %s: %w", filepath.Base(path), err)
  success = true
  ```
  What: THE atomic publish. On failure the defer removes the temp; the old target is intact —
  this is the step the fail-closed persistence tests break deliberately
  (settings_persistence_test.go:83-85, policy_persistence_fail_closed_test.go:81, by making
  the config path a directory).
  Establishes: readers (including a concurrent `LoadConfig`) see either the complete old or
  the complete new document, never a mixture.

- file.go:61-68 + syncDir file.go:75-82
  ```go
  syncDir(dir)   // best-effort; errors ignored
  ```
  What: fsync the containing directory so the rename's directory-entry update survives power
  loss (file.go:61-65).
  Why best-effort: on platforms where a directory cannot be opened/synced (Windows) it is a
  documented no-op; the atomic swap already succeeded regardless (file.go:63-65, 71-74).
  Establishes (weakly): post-crash the directory entry usually points at the new file.
  Residual failure mode: power loss AFTER the rename but BEFORE the (or without a successful)
  directory fsync can resurface the OLD complete content — never a truncated file. A caller
  can be told "saved" (nil return) while the entry was not yet durable; for config this is
  the same shape as the documented owner-identity retry-on-next-restart posture, but nothing
  found that surfaces it.

**Failure-mode inventory:**
1. Missing/unwritable dir → error at CreateTemp; target untouched.
2. Write/Sync/Close/Chmod/Rename error → labeled wrapped error; temp removed; target untouched.
3. Process crash pre-rename → target intact; an ORPHAN temp `<base>.tmp-*` may remain (the
   defer never runs on a crash) — nothing found that sweeps orphans in `config/`.
4. Power loss post-rename, pre-dir-fsync (or dir fsync failed silently) → old content may
   survive; never truncation.
5. Wedged filesystem → unbounded block inside fsync while the caller (usually) holds `m.mu`,
   transitively stalling the lifecycle main loop (app.go:663-666).
6. Concurrent writers to one path → last rename wins; no corruption; no detection.

**Cross-Function Dependencies:**
- Callees: `os.CreateTemp`, `(*os.File).Write/Sync/Close`, `os.Chmod`, `os.Rename`, `syncDir`
  (file.go:75-82, errors deliberately swallowed).
- Callers: `config.SaveConfig` (config.go:738, perm 0600 — the path this audit follows);
  `streamer.streakcache` (streakcache.go:90, 129, perm 0644); `updater` binary replacement
  (updater.go:565-570, perm 0755).
- Invariant couplings: "persistence is the commit point" upstream is only as strong as this
  function's all-or-nothing contract; the no-timeout fsync couples filesystem health to
  lifecycle responsiveness (app.go:663-666).

**Open Questions:**
- Orphan temp files after a hard crash (mode 3 above): tolerated silently? `LoadConfig` reads
  only the exact `path`, so correctness is unaffected; no cleanup was found.
- The nil-return-despite-lost-durability window of mode 4: acceptable-by-design (best-effort
  is documented at file.go:62-65), but no caller distinguishes "renamed" from "rename made
  durable" — worth knowing when reasoning about "an acknowledged mutation is a durable one"
  (app.go:637-638), which this window technically bounds.
