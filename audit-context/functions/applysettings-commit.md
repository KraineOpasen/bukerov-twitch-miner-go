# applySettings / applySettingsNoRename — commit-point discipline

Target: `internal/miner/miner.go` @ HEAD 5b331e5. Scope: the settings-apply coordinator and its
no-rename/no-removal commit path. The rename/removal protocols themselves are OUT of scope; only their
commit-point shape is recorded (final appendix).

Audit-specific summary (the three points this audit tracks):

| Point | applySettingsNoRename | Lock held |
| --- | --- | --- |
| MUTATION (candidate build; live config untouched) | miner.go:2312–2315 | `m.mu` (write) + `coordinatorMu` (caller) |
| PERSISTENCE (`config.SaveConfig`) | miner.go:2317 | `m.mu` (write) + `coordinatorMu` — deliberate invariant |
| PUBLISH (`m.config = candidate`) | miner.go:2322 | same `m.mu` critical section as persistence |

Error propagation: SaveConfig failure → wrapped `"settings apply rejected; no changes were made: persist
config: %w"` (miner.go:2319) → returned verbatim through `applySettings` (miner.go:2238) → logged once in
`ApplySettings` (miner.go:2156–2158) → `settings.SettingsUpdateCallback` (dto.go:211) → web handler
(`internal/web/handlers_settings.go:205`). Zero side effects fire on failure. All dependent side effects
(CommitPlan, finishApply's component fan-out, deletions sweep, Discord update, success log) fire strictly
AFTER persistence+publish, at miner.go:2325–2332.

---

## applySettings in internal/miner/miner.go (L2212–L2240)

**Purpose:** Fail-closed settings-apply coordinator (doc: L2163–L2211). Gates admission against shutdown,
serializes the entire apply pipeline under `coordinatorMu`, captures the streamer-deletion coordinator once,
resolves the intended roster once (`PlanReconcile`), and dispatches to exactly one of three commit paths
(rename / removals / plain) reusing that same plan, so Twitch is never queried twice for one apply
(L2174–L2177).

**Inputs & Assumptions:**

- `ctx context.Context` — the HTTP request context (or `context.Background()` from legacy fire-and-forget
  callers, per L2148–2151). Trust: cancellation signal only; consulted only by the rename/removal paths
  (passed at L2234/L2236), NOT by the no-rename path (L2238 passes no ctx).
- `s settings.RuntimeSettings` — user-posted dashboard settings, untrusted user input arriving via
  `web.Server.SetSettingsUpdateCallback(m.ApplySettings)` (miner.go:821, 845; `internal/web/server.go:416`)
  and via `followed.go:66` (`apply := m.ApplySettings`). No validation happens here; conversion is
  `settings.StreamersFromDTO`/`StreamerSettingsFromDTO` (builder.go:185, convert.go:47), which construct
  fresh objects (no aliasing of caller memory into the candidate).
- Implicit: `m.streamers` (roster manager), `m.streamerLifecycle`, `m.runCtx`, `m.applyDraining`.
- Precondition "miner is not draining / run context alive": established by `beginApply` (miner.go:2087–2098)
  under `applyMu` — refuses with `false` when `applyDraining` (set by stop() at miner.go:1927 before
  `applyWG.Wait()`) or `m.runCtx.Err() != nil` (runCtx assigned in Run, miner.go:484).
- Precondition "m.streamerLifecycle is stable for this apply": doc claims built once at startup, never
  rebuilt mid-run since M4 (L2168–2172). Establisher: `buildStreamerLifecycle` writes it only at startup
  (streamer_deletion.go:65/71/74) — WITHOUT `m.mu`; the L2221 RLock sample synchronizes with nothing for
  that write; safety rests on startup ordering (setup completes before the web callback is registered at
  miner.go:821/845). Mechanical enforcer against a mid-run rebuild: nothing found (convention + M4 removal
  of the rebuild branch, finishApply doc L2744–2747).

**Outputs & Effects:** Returns the chosen path's error verbatim, or nil. Itself mutates nothing: `PlanReconcile`
is explicitly non-mutating (manager.go:374–379: Phase A resolve unlocked, Phase B decision-only under the
manager's read lock). Postcondition on nil return: config.json, `m.config`, and the runtime roster all agree
with `s` (each path's own guarantee).

**Block-by-Block:**

- L2213–2216 `if !m.beginApply() { return ErrShuttingDown } / defer m.endApply()`
  - What: shutdown-drain admission; registers this apply in `applyWG`.
  - Why here: first act, before any side effect — refusal must be side-effect-free (fenced doc L2106–2108).
  - Assumes: stop() sets `applyDraining` then Waits under the same `applyMu` atomicity (field doc L287–297).
  - Establishes: teardown cannot proceed past `applyWG.Wait()` while this apply is in flight; a success
    answer is therefore included in any generation handoff (L2125–2129).
  - Depended on by: `snapshotConfigLocked`'s slice-sharing argument (L2701–2705) — apply-path element
    mutation is drained before a config crosses a generation boundary.
- L2218–2219 `m.coordinatorMu.Lock() / defer ... Unlock()`
  - What: whole-pipeline serialization; held across plan, durable I/O, commit, and finishApply until return.
  - Why: two concurrent applies must not interleave durable-persist steps (field doc L276–285). Declared
    lock order: `coordinatorMu -> m.mu -> streamer.Manager.mu -> models.Streamer.mu` (L279–280, L2189–2190).
  - Establishes: `PlannedRemovals`' documented requirement that the manager is in the SAME state
    PlanReconcile observed (manager.go:356–361 names coordinatorMu-serialized applies as the guarantee).
  - Note: `beginApply`/`applyWG` is admission only, NOT ordering — fenced writers (SetAutoRedeem,
    SetDropRule, ApplyHealthSettings, ApplyCampaignPolicy) do not take coordinatorMu (L2131–2136, L299–305).
- L2221–2223 `m.mu.RLock(); coord := m.streamerLifecycle; m.mu.RUnlock()`
  - What: single capture of the deletion coordinator for the whole apply.
  - Why: gives the apply one self-consistent view even if a test swaps it (L2170–2173).
  - Depended on by: all three paths + finishApply, which deliberately receives this captured value rather
    than re-reading (L2788–2792).
- L2225–2227 DTO conversion + `plan := m.streamers.PlanReconcile(streamersCfg, defaultSettings, nil)`
  - What: build the one roster plan. Network: Phase A resolves ChannelIDs against Twitch, UNLOCKED
    (manager.go:374–379) — runs while holding `coordinatorMu` but no data lock, honoring "no network I/O
    under m.mu/manager.mu/streamer.mu" (L2190–2192).
  - Assumes: PlanReconcile bounded without a ctx — it takes none (manager.go:379). Enforcer for boundedness:
    nothing found in this function (presumably HTTP client timeouts inside the Twitch client; not followed —
    out of scope). Recorded as open question.
- L2229–2238 `plannedRenames / plannedRemovals` + dispatch switch
  - What: rename presence wins (rename path also handles removals riding along, L2184–2187); else removals;
    else plain. `PlannedRenames()` re-reads current logins at call time (manager.go:315–320);
    `PlannedRemovals(m.streamers)` requires unchanged manager state (manager.go:356–361).
  - Establishes: `applySettingsNoRename`'s precondition "both lists empty" — the ONLY production
    establisher; the callee never re-checks (see below).

**Cross-Function Dependencies:** Callees: `beginApply`/`endApply` (admission), `PlanReconcile` /
`PlannedRenames` / `PlannedRemovals` (plan), three path functions. Callers: `ApplySettings` (L2154–2161,
logs failure once, success log lives only inside the paths — BKM-006 C2, L2151–2153); registered as the web
settings callback (miner.go:821/845) and used by `followed.go:66`. Shared state: `coordinatorMu`, `applyWG`,
`m.streamerLifecycle`, `m.streamers`.

**Error propagation:** `ErrShuttingDown` (L2214) or the chosen path's error, verbatim, up through
`ApplySettings` → web handler / legacy callers. `ApplySettings` guarantees the diagnostic log even when the
caller discards the error (L2148–2151).

**Open Questions:**

- PlanReconcile's Twitch resolution runs with no ctx and no explicit budget while `coordinatorMu` is held —
  what bounds it, and can a slow resolution starve every other apply and every fenced writer that is queued
  behind a subsequent apply's coordinatorMu? (Fenced writers do not take coordinatorMu, so only applies queue.)
- `m.streamerLifecycle` is written without `m.mu` at startup (streamer_deletion.go:65–74) but read under
  `m.mu.RLock` here — the happens-before is startup ordering only. Is any test-only mid-run swap (the doc
  admits one, L2171–2173) actually exercised under -race?

---

## applySettingsNoRename in internal/miner/miner.go (L2310–L2334)

**Purpose:** The ordinary Settings-page save: no identity mutation, no removal. Fail-closed around a single
commit point — durable persistence — with the ENTIRE candidate lifetime (clone, apply, stamp, persist,
publish) inside ONE `m.mu` critical section (doc L2261–2278). That single section is what closes, for this
path, the clone-window residual the removal/rename paths must keep open (L2269–2274).

**Inputs & Assumptions:**

- `s settings.RuntimeSettings` — same untrusted posted settings; consumed only via
  `settings.ApplyToConfig` (builder.go:199–207: wholesale reassignment of `cfg.Streamers`,
  `cfg.StreamerSettings`, `cfg.Priority`, etc. — no in-place mutation of shared slices).
- `plan *streamer.ReconcilePlan` — the caller's single plan. Assumed fresh w.r.t. the manager: established
  by `coordinatorMu` held by the caller across plan→commit (manager.go:358–361 + miner.go:2218).
- `coord` — passed through to `finishApply` only (no removals to admit here, L2247–2249).
- Precondition "PlannedRenames and PlannedRemovals BOTH empty — the caller already checked" (L2243–2244):
  established solely by the dispatch switch at miner.go:2232–2238; in-function enforcer: nothing found (no
  re-check; a hypothetical direct caller with a removal-bearing plan would commit the removal to config and
  runtime with no SRAP admission). Only production caller is `applySettings` (grep: no others outside tests).
- Precondition "caller holds coordinatorMu": established at L2218; mechanical enforcer in this function:
  nothing found (Go has no lock assertions).
- Precondition for the atomicity argument: EVERY live-config writer mutates under `m.mu`. Verified for the
  named set: ApplyCampaignPolicy (policy.go:304–311), SetDropRule (policy.go:364–385), ApplyHealthSettings
  (health.go:459–465, candidate-based, SaveConfig under m.mu), SetAutoRedeem (rewards.go:196 "Persist while
  holding the lock"). Global enforcer that no future writer skips m.mu: nothing found (convention +
  cloneConfigLocked's [R7] comment, L2663–2672).
- No ctx parameter: the path has no cancellation point at all — a request cancelled mid-flight still
  commits. Deliberate by shape (no unbounded I/O between entry and commit); the removal path by contrast
  documents an explicit cancellation linearization point (L2350–2357).

**Outputs & Effects:** nil on success with: config.json rewritten atomically (temp+rename,
config.go:736–738), `m.config` swapped to the candidate, runtime roster committed (`CommitPlan`), all
dependent components updated (finishApply). On SaveConfig failure: wrapped error, and literally zero
mutation — nothing outside the critical section observed anything (L2298–2300).

**Block-by-Block:**

- L2311 `m.mu.Lock()`
  - What: opens the single critical section covering clone→publish.
  - Why: a candidate filled off-lock and published later would revert, at publication, every live-config
    value write that landed in between (health/policy fields snapshotted by the shallow clone) — in memory
    AND on disk (L2261–2269). This path takes no durable I/O between clone and commit, so it does not need
    the window and must not open it (L2269–2275).
  - Establishes: the MUTATION point lock. Also makes `refreshCandidateAutoRedeemLocked` unnecessary here:
    nothing (including SetAutoRedeem) can touch the live AutoRedeem map between this clone and this publish
    (L2280–2283).
- L2312 `candidate := m.cloneConfigLocked()`
  - What: shallow struct copy + deep copy of AutoRedeem only (L2673–2682). DropRules deliberately ALIASES
    the live map ([R7], L2663–2672) — safe only because this candidate's SaveConfig also runs under m.mu.
  - Assumes: caller holds m.mu (doc L2672) — true here; mechanical enforcer: nothing found.
  - Depended on by: the SaveConfig-under-m.mu invariant below (marshalling the aliased DropRules off-lock
    is the "concurrent map iteration and map write" panic the M2/D7 fix eliminated, L2666–2671).
- L2313 `settings.ApplyToConfig(candidate, s)`
  - What: writes the posted settings into the CANDIDATE (wholesale slice reassignment, builder.go:199+).
  - Establishes: live config never mutated in place (sequence step 1, L2286–2288).
- L2314–2315 `backfillChannelIDs(candidate, plan.ResolvedChannelIDs())` then
  `backfillChannelIDs(candidate, channelIDsByLogin(m.streamers.All()))`
  - What: two-source ChannelID stamping of the candidate — plan resolution (new streamers) + current roster
    (retained streamers whose resolution failed this cycle) (step 2, L2288–2296).
  - Why here: persistence now precedes CommitPlan, so the candidate itself must carry the stored-identity
    anchor a cold restart depends on; finishApply's post-commit backfill (L2807) reaches memory only.
  - Locks: `m.streamers.All()` takes `streamer.Manager.mu` INSIDE `m.mu` — the documented order
    (L2294–2296). `backfillChannelIDs` is additive-only, never overwrites a non-empty ChannelID
    (rename_reconcile.go:109–127); `channelIDsByLogin` omits unresolved entries (rename_reconcile.go:129–143).
- L2316–2321 THE PERSISTENCE POINT: `if m.configPath != "" { if err := config.SaveConfig(...); err != nil { m.mu.Unlock(); return ... } }`
  - What: durable commit. `SaveConfig` marshals under m.mu and writes via atomic temp+rename with 0600
    perms (config.go:721–739); with `DiscordTokenFromEnv` the on-disk token is cleared (config.go:726–729).
  - Why UNDER m.mu (the deliberate invariant, twice documented — L2192–2196 "one serialized commit sequence
    with no lost-update window", L2275–2278 "never split this section"): (a) closes the lost-update window
    against the other config writers, all of which also persist under m.mu; (b) the candidate's DropRules
    aliases the live map ([R7]).
  - Error path: unlock FIRST (L2318), then return the wrapped error (L2319). Lock held at the moment of
    failure detection: m.mu + coordinatorMu; both released before the caller sees the error (coordinatorMu
    by the caller's defer). Zero mutation postcondition holds: candidate is private, live config untouched.
  - `configPath == ""` skips persistence and proceeds — documented no-op success (L2299–2300), same as
    every other path.
- L2322–2323 THE PUBLISH POINT: `m.config = candidate; m.mu.Unlock()`
  - What: pointer swap publishing the candidate as the live config — SAME critical section as persistence.
  - Establishes: atomicity of the swap w.r.t. every other m.mu writer/reader (L2277–2278); disk and memory
    can never disagree in the direction "runtime committed, disk stale" (the pre-M1 defect, L2251–2259).
- L2325–2326 `added, removed, changed, renamed, conflicts := m.streamers.CommitPlan(plan); logReconcileConflicts(conflicts)`
  - What: past the commit point, NO abort (step 4, L2301–2302). CommitPlan mutates the runtime roster under
    `streamer.Manager.mu` (manager.go:565), with m.mu NOT held, coordinatorMu still held.
  - Conflicts are logged only (L2645–2648) — see open question on config/runtime divergence.
- L2328–2331 `ctx := m.runCtx; if ctx == nil { ctx = context.Background() }`
  - What: the deletion-sweep context for finishApply. runCtx set in Run (miner.go:484); nil only for
    struct-literal test Miners.
- L2332 `m.finishApply(ctx, coord, candidate, added, removed, changed, renamed)`
  - What: dependent side effects, ALL strictly after persistence+publish. Under a fresh m.mu section
    (L2794–2829): idempotent re-publish `m.config = newConfig` (L2795 — same pointer already published at
    L2322), backstop state migration (L2796, no-op here: renamed is empty), in-memory ChannelID backfill
    (L2807), watcher/drops/discovery settings pushes (L2809–2820). Off m.mu: notifications manager snapshot
    (L2837 — Discord network I/O must never run under m.mu, L2831–2836), risk settings (L2841–2843), roster
    propagation + stream check (L2847–2862), capability reconciliation (L2871), `applyStreamerDeletions`
    (L2878 — on THIS path only re-add owed-purge reconciliation, L2247–2249, on m.runCtx so it aborts on
    shutdown), Discord config update (L2897–2901), `"Runtime settings updated"` success log (L2907 — lives
    only on this success path, BKM-006 C2, L2151–2153).
  - finishApply performs NO persistence and cannot fail (L2749–2755).
- Deliberately NO `applyCommitBarrier` bracket here (L2304–2309): the D1 window is closed by the single
  critical section, and D2 (removed-login resurrection) cannot exist on a path with no removals. The
  removal/rename paths bracket their commit with it (L2433/2453, L2582/2619); it is a tests-only seam, nil
  in production (field doc L385–395; srap_test.go:609 pins its absence here).

**Cross-Function Dependencies:** Callees: `cloneConfigLocked` (aliasing contract), `settings.ApplyToConfig`,
`backfillChannelIDs`/`channelIDsByLogin`, `config.SaveConfig` (atomicity), `Manager.CommitPlan` (roster
commit + lock), `finishApply` (fan-out). Caller: `applySettings` only (holds coordinatorMu + applyWG for the
full duration; error returned verbatim). Shared state: `m.config` (with the four fenced writers — mutual
exclusion purely via m.mu here), `m.configPath`, `m.streamers`, `m.runCtx`. Invariant couplings:
SaveConfig-under-m.mu ↔ cloneConfigLocked's DropRules aliasing ([R7]); persistence-before-CommitPlan ↔ the
candidate carrying its own ChannelID anchors (L2288–2296); coordinatorMu across plan→commit ↔
PlannedRemovals/CommitPlan freshness (manager.go:356–361).

**Open Questions:**

- When `CommitPlan` reports conflicts (duplicate settings, login collision, stored-ChannelID mismatch), the
  candidate built from the posted settings has ALREADY been persisted and published — do disk/memory config
  and the runtime roster diverge for the conflicting entries until the next apply, and is that intended
  (warn-only, L2326)?
- The no-ctx shape means a client that disconnected mid-request still gets its change committed (the web
  handler cannot report it). Consistent with "no cancellation point needed", but is the 200-vs-committed
  mismatch on client timeout accepted anywhere in docs? Nothing found in this function's comments.
- `finishApply`'s L2795 re-publish is a same-pointer no-op on all three current paths — is any future path
  expected to pass a different pointer, or is the parameter vestigial-but-harmless?

---

## Appendix: commit-point shape of the other two paths (recorded, not analyzed)

Both paths open the documented clone window — clone under one m.mu section, durable I/O off-lock, commit
under a second m.mu section — which is the residual applySettings' doc records (L2198–2211): AutoRedeem is
protected at the commit point by `refreshCandidateAutoRedeemLocked` (D1/D2), but ApplyHealthSettings /
ApplyCampaignPolicy VALUE-field edits landing inside the window are silently overwritten by the apply's
publish — known residual, not fixed (L2206–2211; cross-referenced from fenced's doc, L2131–2136).

**applySettingsWithRemovals (L2387–2467), commit point L2436–2452:**
clone+configPath snapshot under m.mu (L2392–2395); `ApplyToConfig` off-lock (L2397); unlocked durable
admission on critA (L2419–2421); pre-commit barrier (L2433–2435); then ONE m.mu section: refresh AutoRedeem
from the LIVE map (L2437), two-source backfill (L2438–2439, roster half under m.mu → manager.mu),
`config.SaveConfig` UNDER m.mu (L2440–2445; failure: unlock → `abortAdmittedRemovals` compensation off-lock
→ zero-mutation error), PUBLISH `m.config = candidate` (L2447) + I4 AutoRedeem state delete/gen bump atomic
with the publish (L2448–2451), unlock (L2452). Post-commit: barrier (L2453–2455), CommitPlan (L2460),
finishApply on critB = WithTimeout(WithoutCancel(ctx), purgeBudget) (L2463–2465). Locks at
mutation/persistence/publish: all m.mu (+ coordinatorMu throughout). Cancellation linearization point is the
L2427 check, not SaveConfig (L2350–2357).

**applySettingsWithRename (L2501–2635), commit point L2585–2618:**
clone+snapshot under m.mu (L2502–2506); off-lock candidate surgery (L2508–2521); unlocked admission
(L2533–2549); off-lock durable analytics rename via coordinator-or-analytics `svc` with rollback closure
(L2557–2568; failure compensates admission too); pre-commit barrier (L2582–2584); then ONE m.mu section
(hard constraint documented L2570–2581: SaveConfig must never move back off m.mu — DropRules aliasing panic
class): refresh (L2586), roster-half backfill (L2599 — renamed streamer still keyed under OLD login until
CommitPlan; inert by construction, L2593–2598), `config.SaveConfig` UNDER m.mu (L2600–2606; failure: unlock
→ `rollback()` of committed analytics renames off-lock → abort admissions → error), PUBLISH (L2608) +
AutoRedeem state/gen migration and removal cleanup atomic with the publish (L2609–2617), unlock (L2618).
Post-commit: barrier (L2619–2621), CommitPlan (L2628), finishApply on critB (L2631–2633). Error strings:
`"rename transaction aborted: ..."` — distinct from the other paths' `"settings apply rejected"` prefix.
