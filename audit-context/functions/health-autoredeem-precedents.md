# Fail-closed apply precedents: ApplyHealthSettings / applyHealthSettings and SetAutoRedeem / setAutoRedeem

Audit-context analysis at HEAD 5b331e5. Scope: `internal/miner/health.go` L416–L495 and
`internal/miner/rewards.go` L140–L229, plus every callee/caller relied on. These are the two
fail-closed configuration-mutation precedents: one uses a **candidate-clone / commit-on-save**
discipline (health), the other an **in-place mutate / exact-rollback-on-save-failure** discipline
(auto-redeem). Both are admitted through the same generation fence (`Miner.fenced`,
internal/miner/miner.go:2137-2143).

Locking vocabulary used throughout (all fields on `Miner`, internal/miner/miner.go):
- `m.mu` (`sync.RWMutex`, miner.go:274) — guards `m.config`, `m.autoRedeemState`, `m.autoRedeemGen`, `m.reauthRequired`, etc.
- `m.healthApplyMu` (`sync.Mutex`, miner.go:108-118) — serializes the ENTIRE health apply (commit AND dependent notifications); lock order healthApplyMu -> mu (miner.go:111).
- `m.applyMu`/`applyWG`/`applyDraining` (miner.go:287-308) — drain/admission interlock used by `beginApply`/`fenced`; a counter, **not** a serializer (miner.go:2131-2136, 299-305).

---

## ApplyHealthSettings in internal/miner/health.go (L451-L453)

**Purpose:** Exported wrapper for the dashboard's Health Center form: admit-or-refuse a health
settings apply through the shutdown/retirement generation fence, then delegate the whole apply to
`applyHealthSettings`. It is the `web.HealthProvider.ApplyHealthSettings` implementation
(internal/web/server.go:91-94).

**Inputs & Assumptions:**
- `s config.HealthSettings` (by value) — untrusted operator input assembled by the web handler
  `handleHealthSettingsPost` from form fields via `atoiDefault` (internal/web/handlers_health.go:222-237).
  No validation is assumed here; `applyHealthSettings` runs `config.ValidateConfig` on the candidate
  (health.go:462), which clamps every Health field to bounds (internal/config/config.go:856-905).
- Implicit state: the receiver may be a retired generation still registered as the dashboard's
  provider — that is exactly why the fence exists (miner.go:2110-2123). Nothing else is assumed
  established before the call.
- Precondition "fence sits OUTSIDE healthApplyMu" is established structurally: `fenced` runs
  `beginApply` (only `applyMu`, miner.go:2087-2098) before `fn` is invoked, so a refused call never
  touches `healthApplyMu` (doc: health.go:446-450).

**Outputs & Effects:**
- Returns `ErrShuttingDown` (`= settings.ErrShuttingDown`, miner.go:47) when `beginApply` refuses
  (draining, or `m.runCtx` already cancelled — miner.go:2090-2095), **before any side effect**
  (miner.go:2106-2107: "with no side effect whatsoever").
- Otherwise returns `applyHealthSettings`'s error verbatim. No state of its own.
- While admitted, holds an `applyWG` registration (miner.go:2096, released by deferred `endApply`,
  miner.go:2100-2104), which `stop()` waits on before teardown (miner.go:287-293) — this is what
  makes a success acknowledgement trustworthy across a generation handoff (miner.go:2125-2129).

**Block-by-Block:**

L451-L453:
```go
func (m *Miner) ApplyHealthSettings(s config.HealthSettings) error {
	return m.fenced(func() error { return m.applyHealthSettings(s) })
}
```
- What: single-expression delegation through the fence.
- Why here: the wrapper "only admits or refuses; this function [applyHealthSettings] is still where the apply itself lives" (health.go:432-436).
- Assumes: `fenced` refuses before running `fn` (enforced: miner.go:2138-2139 returns before the closure runs).
- Establishes: every accepted health apply is inside the `applyWG` drain window.
- Depended on by: `internal/web/handlers_health.go:241` (POST handler); teardown's `applyWG.Wait()` correctness depends on every mutator entering through here (miner.go:299-303 lists ApplyHealthSettings among the fenced callers).

**Cross-Function Dependencies:**
- Callee `fenced` (miner.go:2137-2143) -> `beginApply` (miner.go:2087-2098): atomicity of
  "check draining then Add(1)" vs "set draining then Wait()" via `applyMu` (miner.go:294-297).
- Callee `applyHealthSettings` (below).
- Caller `handleHealthSettingsPost` (internal/web/handlers_health.go:200-253): assumes fail-closed
  semantics — on error it deliberately does NOT re-render ("the caller must not be told a change
  applied when persistence rejected it", handlers_health.go:238-240); maps `ErrShuttingDown`/DB-closed
  to 503 via `mutationRefusedAsUnavailable` (internal/web/responses.go:69-71), anything else to 500.
  The handler also serializes its own read-patch-apply against other form posts with `s.healthFormMu`
  (handlers_health.go:216-220) — a web-layer lock, distinct from `healthApplyMu`.
- Invariant coupling: `fenced` is admission only, NOT serialization against `applySettings`
  (miner.go:2131-2136) — see Open Questions on the clone-window residual.

**Open Questions:**
- None specific to the wrapper beyond those recorded for `applyHealthSettings`.

---

## applyHealthSettings in internal/miner/health.go (L455-L495)

**Purpose:** The actual health apply: validate a candidate config carrying the new
canary/watchdog settings, durably persist it, and — only after the save has succeeded — publish it
as the live config and notify the running canary and drop-progress watchdog (hot apply, no
restart). Doc comment: health.go:422-450.

**Inputs & Assumptions:**
- `s config.HealthSettings` — raw operator values; trust established by `config.ValidateConfig`
  clamping (config.go:856-905, e.g. CanaryIntervalMinutes to [60,1440], plus the
  staleness>=interval coupling at config.go:873-874).
- Implicit state read: `m.config` (live config pointer), `m.configPath` (miner.go:82-83),
  `m.healthCanaryUpdate`/`m.healthWatchdogUpdate` test seams (miner.go:119-128), `m.canary`,
  `m.progressWatchdog` (miner.go:105-107). Seams are nil in production (miner.go:120-121).
- Precondition: caller already admitted through the fence — established by the only caller,
  `ApplyHealthSettings` L452. Nothing enforces fence-first inside this function itself
  (a direct call would skip the drain interlock): **nothing found** as a mechanical enforcer;
  the private/exported split is the only guard.
- Precondition: candidate's `SaveConfig` must run under `m.mu` because `cloneConfigLocked`
  deliberately aliases the live `DropRules` map ([R7], miner.go:2663-2672: "Never move a
  candidate's config.SaveConfig back off m.mu" — an unlocked marshal can race `SetDropRule` and
  panic). This function complies (save at L465 sits between Lock L459 and Unlock L472).

**Outputs & Effects:**
- Success: `m.config` replaced wholesale by the validated candidate; config.json atomically
  rewritten (or skipped when `configPath == ""` — documented no-op-persist hot apply,
  health.go:437-439); canary and watchdog notified with the **validated** (`applied`) values;
  `slog.Info("Health settings updated", ...)` L491-493; returns nil.
- Failure: only `config.SaveConfig` can fail here. Returns
  `fmt.Errorf("health settings apply rejected; no changes were made: %w", err)` (L468) after
  `slog.Error` (L467). Postcondition on failure: `m.config` is "the very same object, with the very
  same contents" (health.go:430-431) — the candidate is simply dropped; canary/watchdog are never
  reached (both `return` points precede L471+).
- **MUTATION point:** two-phase. Candidate build/mutation (L460-L463) touches only a private clone,
  under `healthApplyMu` + `m.mu` (write). The live-state mutation is the single pointer swap
  `m.config = candidate` at L471 — under `healthApplyMu` + `m.mu` (write).
- **PERSISTENCE point:** `config.SaveConfig(m.configPath, candidate)` at L465 — under
  `healthApplyMu` + `m.mu` (write lock deliberately held across the file I/O; see [R7] above).
  SaveConfig itself is an atomic temp+fsync+rename, 0600 (config.go:731-738;
  internal/util/file.go:18-43), and strips an env-sourced Discord token (config.go:722-729).
- **PUBLISH point:** identical to the live-mutation point — the L471 pointer swap under
  `healthApplyMu` + `m.mu`; readers (`CurrentHealthSettings` health.go:416-420, `GetAutoRedeem`,
  `refreshHealthCenter`) read `m.config` under `m.mu.RLock`, so the swap is the linearization
  point. Persistence strictly precedes publication (commit point = save success).
- **Dependent side effects and timing:** canary/watchdog notification (L481-L490) and the success
  log (L491-493) fire strictly AFTER persistence and publication, with `m.mu` already released
  (Unlock L472) but `healthApplyMu` still held (deferred Unlock L457) — the load-bearing ordering
  comment at L474-480 pins this, and `internal/miner/health_persistence_test.go:320-397` pins the
  "notifications complete before healthApplyMu releases" property with a TryLock probe.
- **Error propagation:** SaveConfig error -> wrapped at L468 -> `fenced` (transparent) ->
  `ApplyHealthSettings` -> web handler -> HTTP 500 (or 503 only for ErrShuttingDown/ErrClosed,
  which this path does not produce) with "no changes were made" body
  (handlers_health.go:241-252). Logged once here (L467); the handler does not log.

**Block-by-Block:**

L456-L457 — serialization:
```go
	m.healthApplyMu.Lock()
	defer m.healthApplyMu.Unlock()
```
- What: takes the apply-wide mutex for the whole function.
- Why here: `m.mu` must be released before the notifications (they take `health.Canary.mu` /
  `ProgressWatchdog.mu`, internal/health/canary.go:145-152, progress.go:349-353, and must run
  unlocked, miner.go:112-114), so a second lock must span that release or two overlapping applies
  could publish config at one update while notifying components of another (miner.go:114-117).
- Assumes: lock order healthApplyMu -> mu is respected everywhere (documented miner.go:111;
  mechanical enforcement: **nothing found** — no other acquirer of healthApplyMu exists outside
  this function and the test probe, which is what currently makes the order trivially safe).
- Establishes: config publish and component notification are one atomic unit per apply.
- Depended on by: the L474-480 ordering comment; health_persistence_test.go:320-397.

L459-L463 — candidate build (MUTATION on private clone; lock: healthApplyMu + m.mu):
```go
	m.mu.Lock()
	candidate := m.cloneConfigLocked()
	candidate.Health = s
	config.ValidateConfig(candidate)
	applied := candidate.Health
```
- What: clone live config (shallow struct copy + deep AutoRedeem map copy, miner.go:2673-2682),
  overwrite its Health, clamp in place, snapshot the post-clamp Health as `applied`.
- Why here: never touch `m.config` until the save succeeds (health.go:426-429, mirroring
  `applySettingsNoRename`'s discipline); `applied` is captured so the notifications use exactly
  what was persisted, not the raw request (the settings UI depends on this: only the response's
  parsed values are authoritative, internal/web/s5_6_settings_test.go:1267-1293).
- Assumes: `cloneConfigLocked`'s aliasing contract — AutoRedeem deep-copied, DropRules aliased
  on purpose (miner.go:2663-2680); ValidateConfig only clamps, never errors (config.go:763-765,
  signature returns nothing).
- Establishes: a candidate that is valid-by-construction; rollback machinery is unnecessary —
  the "rollback" is simply dropping the local variable.
- Depended on by: the failure return at L466-468 (safe precisely because nothing live was touched)
  and the notification blocks reading `applied`.

L464-L470 — PERSISTENCE point (lock: healthApplyMu + m.mu):
```go
	if m.configPath != "" {
		if err := config.SaveConfig(m.configPath, candidate); err != nil {
			m.mu.Unlock()
			slog.Error("Health settings apply rejected; no changes were made", "error", err)
			return fmt.Errorf("health settings apply rejected; no changes were made: %w", err)
		}
	}
```
- What: durably persist the candidate; on failure, unlock and abort before any live mutation.
- Why here: persistence is the commit point (health.go:426-427); `configPath == ""` keeps the
  documented hot-apply-without-file case (health.go:437-439).
- Assumes: SaveConfig cannot partially clobber config.json (atomic swap,
  util/file.go:18-43 + config.go:735-738); DropRules alias makes the under-mu placement mandatory
  ([R7] miner.go:2663-2672).
- Establishes: on the success path, disk now holds the candidate while memory still holds the old
  config — a window invisible to readers because `m.mu` is held throughout.
- Depended on by: the L474-480 "load-bearing ordering" comment (both failure returns sit before
  the notifications AND before healthApplyMu's release).

L471-L472 — PUBLISH point (lock: healthApplyMu + m.mu, then mu released):
```go
	m.config = candidate
	m.mu.Unlock()
```
- What: wholesale pointer swap of the live config; release m.mu.
- Why here: publication only after durable persistence; releasing m.mu before notifications keeps
  component locks out of m.mu's critical section.
- Assumes: all readers of `m.config` take `m.mu` (e.g. health.go:417-419, rewards.go:113-115,
  health.go:170-173).
- Establishes: memory == disk for the Health section (and everything else in the candidate).
- Depended on by: `CurrentHealthSettings` (the handler re-reads it to render the response's
  authoritative values); every subsequent `cloneConfigLocked`.

L481-L494 — dependent side effects (lock: healthApplyMu only):
```go
	if m.healthCanaryUpdate != nil {
		m.healthCanaryUpdate(healthCanaryConfig(applied))
	} else if m.canary != nil {
		m.canary.UpdateSettings(healthCanaryConfig(applied))
	}
	if m.healthWatchdogUpdate != nil {
		m.healthWatchdogUpdate(healthWatchdogConfig(applied))
	} else if m.progressWatchdog != nil {
		m.progressWatchdog.UpdateSettings(healthWatchdogConfig(applied))
	}
	slog.Info("Health settings updated", ...)
	return nil
```
- What: push the validated settings into the live canary (`Canary.UpdateSettings` also resets its
  resolved streamer on channel change, canary.go:145-152) and watchdog
  (`ProgressWatchdog.UpdateSettings`, progress.go:349-353); log; succeed.
- Why here: strictly after the save success path — "both return points on failure sit before this
  line, and both sit before healthApplyMu is released" (health.go:474-478). The seam fields exist
  because internal/health keeps cfg unexported with no synchronous observer (miner.go:119-128).
- Assumes: `healthCanaryConfig`/`healthWatchdogConfig` (health.go:17-37) are pure mappings;
  UpdateSettings calls are quick, non-blocking mutations under the components' own mutexes.
- Establishes: running components now match both memory and disk. Note the seam branches are
  `if seam != nil ... else if component != nil` — a test with a seam installed never touches the
  real component.
- Depended on by: health_persistence_test.go (spy observes notification-vs-lock ordering);
  the canary/watchdog loops read the new cfg on their next tick.

**Cross-Function Dependencies:**
- Callees: `cloneConfigLocked` (miner.go:2673-2682, m.mu held — comment-enforced only),
  `config.ValidateConfig` (config.go:765+), `config.SaveConfig` (config.go:721-739),
  `healthCanaryConfig`/`healthWatchdogConfig` (health.go:17-37),
  `Canary.UpdateSettings`/`ProgressWatchdog.UpdateSettings` (canary.go:145-152, progress.go:349-353).
- Callers: only `ApplyHealthSettings` L452 (fence) -> web handler chain above.
- Shared state: `m.config` (with every other config reader/writer), `healthApplyMu` (exclusive to
  this path + test probe), the live DropRules map aliased into the candidate (couples this
  function's save placement to `SetDropRule`, policy.go — [R7]).
- Invariant couplings: persistence-before-publish-before-notify; healthApplyMu -> mu order;
  candidate-never-published-on-failure; notifications use post-clamp `applied` only.

**Open Questions:**
- Clone-window residual (documented, unresolved by design): `fenced` explicitly does not
  serialize these mutators against `applySettings` (miner.go:2131-2136). At `applySettingsWithRemovals`'
  commit point the stale candidate (cloned at miner.go:2392-2395) re-merges only `AutoRedeem` and
  channel IDs (miner.go:2437-2439) before being published (miner.go:2447) — there is no
  `candidate.Health` refresh. So a health apply committing inside another apply's clone window can
  be silently overwritten in memory and on disk by that apply's publish, while the canary/watchdog
  keep the newer settings (components and config then disagree until the next health apply). Is
  this residual accepted for the Health section specifically, or only ever reasoned about for
  AutoRedeem (where `refreshCandidateAutoRedeemLocked` closes it, rewards.go:441-496)?
- `healthApplyMu` is not taken by `applySettings*`; the serialization claim ("two overlapping
  applies") therefore covers only health-vs-health applies. Confirmed by inspection; recorded here
  because the doc comment's "entire sequence ... across concurrent callers" could be misread wider.
- Direct-call bypass: nothing mechanically stops future in-package code from calling
  `applyHealthSettings` without the fence (no TryLock/assert analogous to
  `refreshCandidateAutoRedeemLocked`'s [R3] guard, rewards.go:460-463). Nothing found as enforcer.

---

## SetAutoRedeem in internal/miner/rewards.go (L166-L168)

**Purpose:** Exported wrapper: admit an auto-redeem config write for one streamer through the
generation fence, then delegate to `setAutoRedeem`. Implements
`web.RewardsProvider.SetAutoRedeem` (internal/web/server.go:126).

**Inputs & Assumptions:**
- `username string` — any case; canonicalized to lowercase by the delegate (L171). Trust: must
  name a currently tracked AND config-listed streamer; both checks live in the delegate
  (L174-L181), not here.
- `cfg config.AutoRedeemConfig` — operator payload from `handleAutoRedeem`
  (internal/web/handlers_rewards.go:154-200), which pre-clamps only `Budget < 0` to 0
  (handlers_rewards.go:177-179). RewardIDs are deduped/trimmed in the delegate (L183). No other
  validation exists: **nothing found** enforcing an upper Budget bound or RewardID shape.
- Fence-before-guards ordering is deliberate and documented (rewards.go:161-165): a retired
  generation's roster is frozen, so answering the roster check first would convert a retryable
  lifecycle refusal into a permanent-looking "not tracked".

**Outputs & Effects:**
- `ErrShuttingDown` on fence refusal — "nothing was changed" (rewards.go:164-165); mapped to 503
  by the handler (handlers_rewards.go:191-194). Any other error surfaces as HTTP 400 with the
  error text (handlers_rewards.go:195-196).
- Otherwise the delegate's error/success verbatim, with the applyWG registration held across it
  (same mechanics as ApplyHealthSettings; miner.go:299-303 names SetAutoRedeem as a fenced caller).

**Block-by-Block:**

L166-L168:
```go
func (m *Miner) SetAutoRedeem(username string, cfg config.AutoRedeemConfig) error {
	return m.fenced(func() error { return m.setAutoRedeem(username, cfg) })
}
```
- What/Why/Assumes/Establishes/Depended on by: identical fence mechanics to
  `ApplyHealthSettings` L451-453 (see that section); the one behavior specific to this route is
  the handler's pre-check `lifecycleMutationBlocked` being "UX sugar only: SetAutoRedeem's own
  fence is the authoritative backstop" (handlers_rewards.go:160-165), with an unusually wide
  check->mutate race window because the provider was sampled before the body was read.

**Cross-Function Dependencies:** `fenced`/`beginApply`/`endApply` (miner.go:2087-2143);
`setAutoRedeem` (below); caller `handleAutoRedeem` (handlers_rewards.go:154-200).

**Open Questions:** none beyond the delegate's.

---

## setAutoRedeem in internal/miner/rewards.go (L170-L229)

**Purpose:** Persist one streamer's auto-redeem config (write or delete the
`config.AutoRedeem[key]` entry), and — only after a successful save — reset that streamer's
in-memory spend/window bookkeeping and bump its window generation so both this process and any
in-flight evaluator cycle see a fresh budget window (doc: rewards.go:136-165).

**Inputs & Assumptions:**
- `username` — lowercased once into `key` (L171); all map keys and guards use `key`.
- `cfg` — mutated locally: `cfg.RewardIDs = dedupeStrings(cfg.RewardIDs)` (L183; dedupeStrings
  trims, drops empties/dups, returns nil for empty input, rewards.go:507-522). The caller's slice
  is not aliased into config when dedupe allocates, but IS when it returns the same... no:
  dedupeStrings always allocates a new slice or returns nil (L508-510, 512), so the stored config
  never aliases the HTTP-decoded slice.
- Admission preconditions, both checked under `m.mu` (L173-L181):
  1. live runtime roster contains key (`m.streamers.Get(key) != nil`, L174-176);
  2. persisted config's streamer list contains key case-insensitively
     (`configHasStreamerLocked`, rewards.go:127-134).
  The dual check closes the post-commit/pre-CommitPlan window in which a removal/rename has
  published its candidate config but the runtime roster hasn't caught up — a bare roster check
  there could resurrect a removed streamer's consent on disk or write a dead old-login key
  ([R2]/I6/D2, rewards.go:121-126, 142-149). Known accepted false-refusal: a login-collision
  survivor ([RR3], rewards.go:149-155 — and an explicit "do NOT fix via ChannelID match" warning).
  `configHasStreamerLocked`'s "Caller holds m.mu" (rewards.go:126) has **nothing found** as a
  mechanical enforcer (no TryLock assert; contrast [R3] at rewards.go:460-463).
- Implicit state: `m.config.AutoRedeem` may be nil (never yet written); `m.autoRedeemState` /
  `m.autoRedeemGen` maps (miner.go:246-266; gen entries monotonic, never deleted).

**Outputs & Effects:**
- **MUTATION point** (in place, on live config — lock: `m.mu` write, held from L173 to L214/L226):
  L185-L194 — snapshot `prevCfg, hadPrev` and `wasNil`, lazily allocate the map, then either
  `m.config.AutoRedeem[key] = cfg` (any of Enabled / RewardIDs / Budget set) or
  `delete(m.config.AutoRedeem, key)` (zero-ish cfg = "clear the entry"). Unlike the health path
  there is NO candidate clone — the live map is written first and rolled back on failure.
- **PERSISTENCE point:** `config.SaveConfig(m.configPath, m.config)` at L200 — under the same
  continuously-held `m.mu` write lock ("mirroring ApplySettings, so the config isn't mutated by
  another goroutine mid-marshal", L196-197; also satisfies the DropRules-alias rule [R7] since
  this marshals the live config itself). Skipped when `m.configPath == ""` (saveErr stays nil ->
  commit without a file, same no-op-persist convention as health).
- **Rollback (failure path, L202-L217; lock: `m.mu` write until L214):** restore EXACTLY —
  `m.config.AutoRedeem[key] = prevCfg` if `hadPrev`, else `delete`; then **re-nil the map**
  (`m.config.AutoRedeem = nil`) if `wasNil` (L210-212) — so memory matches the still-valid
  on-disk state byte-for-byte, including the nil-vs-empty-map distinction (I6, fixes D5;
  rewards.go:155-158, 203-205). The nil-map re-nil matters because an empty-but-non-nil map is
  observable (e.g. it marshals as `{}` on the NEXT save and changes `cloneConfigLocked`'s copy
  branch, miner.go:2675). Runtime state and generation are untouched on this path "because they
  were never touched" (L204-205). Error: logged (`slog.Error`, L215, after unlocking) and returned
  as `fmt.Errorf("failed to save config: %w", saveErr)` (L216) -> fenced -> handler -> HTTP 400
  (SaveConfig failure is not ErrShuttingDown/ErrClosed, so `mutationRefusedAsUnavailable` is false).
- **Dependent state touched ONLY after success (L223-L224; lock: still the same `m.mu` write):**
  `delete(m.autoRedeemState, key)` (fresh budget window — spend and redeemed set discarded) and
  `m.bumpAutoRedeemGenLocked(key)` (rewards.go:434-439: lazily allocs the gen map, increments;
  monotonic, never deleted, "called only on success/commit paths — never on a failed path").
  Both are atomic with the config commit from any reader's perspective because `m.mu` has not been
  released since the mutation (I5/I6, L219-222).
- **PUBLISH point:** because the mutation is in place on the live config, publication to other
  goroutines happens at `m.mu.Unlock()` — L214 on failure (after rollback, so nothing new is
  published) or **L226 on success** (config entry + state reset + gen bump become visible as one
  unit). Readers all take m.mu: `GetAutoRedeem` (L112-116), `evaluateAutoRedeem`'s snapshot
  (L247-251), `autoRedeemStillCurrent` (L417-424), `recordAutoRedeemed`/`clearAutoRedeemed`
  (L374-407), `cloneConfigLocked`. The un-persisted intermediate state (memory mutated, file not
  yet written) is therefore unobservable — the in-place discipline is equivalent to the candidate
  discipline for readers, differing only in crash semantics (a process crash between L191 and L200
  loses nothing durable: disk was never touched).
- **Side effects after publish:** only `slog.Info("Updated auto-redeem config", ...)` at L227,
  outside m.mu, success path only. No events.Record, no component notifications — the evaluator
  picks the change up by re-reading config under RLock each cycle (L247-251), so no push
  notification is needed; the gen bump is what invalidates already-snapshotted cycles at their
  last gate (I9, L301-309) and at record time (L394-407).

**Block-by-Block:**

L171-L181 — admission (lock: m.mu write):
```go
	key := strings.ToLower(username)

	m.mu.Lock()
	if m.streamers == nil || m.streamers.Get(key) == nil {
		m.mu.Unlock()
		return fmt.Errorf("streamer %q is not tracked", username)
	}
	if !configHasStreamerLocked(m.config, key) {
		m.mu.Unlock()
		return fmt.Errorf("streamer %q is not in the saved streamer list", username)
	}
```
- What: dual roster+config admission; both refusals unlock and return before any mutation.
- Why here: [R2]/D2 window closure (see Inputs). Runs after the fence on purpose (rewards.go:161-165).
- Assumes: `streamer.Manager.Get` is safe under m.mu (lock order coordinatorMu -> mu ->
  streamer.Manager.mu, miner.go:279-280); `m.streamers` may be nil in struct-literal test miners.
- Establishes: key names a streamer that both runtime and persisted config agree exists.
- Depended on by: the D2 durability guarantee; [RR3]'s accepted false-refusal.

L183-L194 — MUTATION (lock: m.mu write):
```go
	cfg.RewardIDs = dedupeStrings(cfg.RewardIDs)

	prevCfg, hadPrev := m.config.AutoRedeem[key]
	wasNil := m.config.AutoRedeem == nil
	if wasNil {
		m.config.AutoRedeem = make(map[string]config.AutoRedeemConfig)
	}
	if cfg.Enabled || len(cfg.RewardIDs) > 0 || cfg.Budget > 0 {
		m.config.AutoRedeem[key] = cfg
	} else {
		delete(m.config.AutoRedeem, key)
	}
```
- What: normalize RewardIDs; capture the exact restore data (`prevCfg`/`hadPrev`/`wasNil`);
  write or clear the entry (a fully-zero cfg is stored as absence, keeping config.json clean and
  `GetAutoRedeem`'s "disabled zero value when none is set" contract, L110-111).
- Why here: the restore snapshot must be taken before the write, under the same lock.
- Assumes: no other goroutine writes `m.config.AutoRedeem` without m.mu (writers:
  this function; `refreshCandidateAutoRedeemLocked` writes candidates only, and the commit points
  at miner.go:2447 swap the whole config under m.mu — consistent).
- Establishes: memory holds the intended end state; enough breadcrumbs exist to undo it exactly.
- Depended on by: the rollback block L202-217; SaveConfig's marshal of `m.config` L200.

L196-L217 — PERSISTENCE + rollback (lock: m.mu write; released at L214 on failure):
```go
	var saveErr error
	if m.configPath != "" {
		saveErr = config.SaveConfig(m.configPath, m.config)
	}
	if saveErr != nil {
		if hadPrev {
			m.config.AutoRedeem[key] = prevCfg
		} else {
			delete(m.config.AutoRedeem, key)
		}
		if wasNil {
			m.config.AutoRedeem = nil
		}
		m.mu.Unlock()
		slog.Error("Failed to save auto-redeem config", "streamer", username, "error", saveErr)
		return fmt.Errorf("failed to save config: %w", saveErr)
	}
```
- What: save the live config while still locked; on failure restore exactly (incl. nil-map
  re-nil), unlock, log, return wrapped error.
- Why here: holding m.mu across the marshal prevents mid-marshal mutation (L196-197) and honors
  [R7]; the exact restore keeps memory == disk after a failed save (I6/D5, L203-205).
- Assumes: SaveConfig failure implies the on-disk file is unchanged (atomic swap:
  util/file.go:18-43 — the rename either happened or it didn't; a failure before rename leaves
  the old file; note a failure AFTER a successful rename cannot be reported as error by
  WriteFileAtomic's structure, so "failed save => old file intact" holds).
- Establishes: the failure postcondition "memory matching the still-valid on-disk state";
  runtime/gen untouched.
- Depended on by: `evaluateAutoRedeem` correctness — a stale gen is only ever produced by a
  REAL commit, never by a failed attempt (bumpAutoRedeemGenLocked doc, rewards.go:431-433).

L219-L228 — commit of dependent state + publish + log:
```go
	delete(m.autoRedeemState, key)
	m.bumpAutoRedeemGenLocked(key)
	m.mu.Unlock()

	slog.Info("Updated auto-redeem config", "streamer", key, "enabled", cfg.Enabled, "budget", cfg.Budget, "rewards", len(cfg.RewardIDs))
	return nil
```
- What: reset the runtime window, seal the old generation, publish everything at Unlock, log.
- Why here: "the state delete AND the generation bump are both gated on the successful save
  (I5/I6) — a stale evaluator cycle that snapshot the OLD generation can no longer record into
  (or re-arm a reward within) this streamer's new window" (L219-222).
- Assumes: gen monotonicity + never-delete (miner.go:260-262) so a sealed gen can never match again.
- Establishes: fresh window; all gen-guarded helpers (`clearAutoRedeemed` L374-383,
  `recordAutoRedeemed` L394-407, `autoRedeemStillCurrent` L417-424) now refuse stale-gen access.
- Depended on by: `evaluateAutoRedeem`'s I9 last-gate (L301-309) and its stale-record
  stop-the-cycle handling (L316-329 — an in-flight redemption that lands after this commit is
  deliberately NOT counted toward the new budget: bounded single-reward TOCTOU residue).

**Cross-Function Dependencies:**
- Callees: `dedupeStrings` (L507-522), `configHasStreamerLocked` (L127-134, m.mu comment-only),
  `config.SaveConfig` (config.go:721-739 -> util.WriteFileAtomic file.go:18+),
  `bumpAutoRedeemGenLocked` (L434-439, m.mu comment-only enforcement — **nothing found** as
  mechanical assert), `streamer.Manager.Get`.
- Callers: `SetAutoRedeem` L167 (fence) -> `handleAutoRedeem` (handlers_rewards.go:154-200).
- Shared state / invariant couplings:
  - `m.config.AutoRedeem` with `refreshCandidateAutoRedeemLocked` (rewards.go:441-496): the
    general-settings commit point rebuilds candidates' AutoRedeem from the CURRENT live map so a
    SetAutoRedeem committed during another apply's off-lock I/O is never lost (I1) — this is the
    counterpart that makes the fenced-non-serialization residual harmless for THIS field
    (miner.go:2437, 2586), unlike Health.
  - `m.autoRedeemState`/`m.autoRedeemGen` with the removal commit points (miner.go:2448-2451,
    2613-2616: same delete+bump pattern, atomic with their publish) and the rename migration
    (`migrateAutoRedeemGenLocked`, rename_reconcile.go:309+, which MIGRATES rather than bumps for
    ordinary renames — C4 window continuity, miner.go:251-259).
  - `rewards()` seam contract: "No m.mu is ever held across a call through the returned client"
    (rewards.go:29-31) — holds for this function (no network I/O at all); enforcement elsewhere:
    **nothing found** (comment only).

**Open Questions:**
- The success log (L227) reports the post-dedupe `cfg` — fine — but there is no user-visible echo
  of the dedupe/normalization back through the handler (`writeSuccess`, handlers_rewards.go:199);
  the dashboard presumably re-fetches via `GetAutoRedeem`. Unverified whether the UI does.
- `configHasStreamerLocked` and `bumpAutoRedeemGenLocked` rely on comment-only "caller holds m.mu"
  contracts while their sibling `refreshCandidateAutoRedeemLocked` got a TryLock panic guard
  ([R3]) — is the asymmetry deliberate (hot-path cost?) or accreted?
- Crash between SaveConfig success (L200) and the gen bump (L224): disk has the new config, the
  process dies before the in-memory window reset matters (state is process-local and "reset on
  restart", miner.go:248-249) — appears benign by design; no test found pinning it. Nothing found.
