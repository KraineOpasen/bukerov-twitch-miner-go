# Web policy handlers — Drops-page mutation endpoints and shared error classification

Audit-context pass (understanding only, no verdicts). HEAD 5b331e5.

Scope: `handleAPIPolicyMode`, `handleAPIPolicyDropRule`, `writePolicyMutationError`
(internal/web/handlers_policy.go), `mutationRefusedAsUnavailable` (internal/web/responses.go),
`lifecycleMutationBlocked` + `writeSettingsConflict` (internal/web/handlers_settings.go),
`renderDropsList` (internal/web/handlers_drops.go).

Routes: `/api/policy/mode` and `/api/policy/drop-rule` registered at
internal/web/server.go:727-728 on the mux wrapped by `csrfProtectMiddleware`
(server.go:894) and, when configured, `basicAuthMiddleware` (server.go:895-897) — so
CSRF/same-origin and optional Basic Auth are enforced OUTSIDE these handlers.

## Error-classification chain (both policy POST handlers, in evaluation order)

1. `r.Method != POST` → **405** body `"method not allowed"` (handlers_policy.go:87-90, 149-152; raw `http.Error`).
2. `r.ParseForm()` error → **500** body `"invalid form"` via `writeInternalError` (handlers_policy.go:91-94, 153-156). Note: a malformed client body is answered as a server fault (500), not 400.
3. `s.lifecycleMutationBlocked()` → **409** via `writeSettingsConflict`: body is the server-localized `lc.settings_conflict` string (handlers_policy.go:102-105, 164-167; handlers_settings.go:64-66). Pre-check UX sugar only — racy by design; the miner-side fence is the authoritative backstop (comment handlers_policy.go:95-101).
4. provider == nil → mutation silently skipped, falls through to `renderDropsList` → **200** partial (handlers_policy.go:109/124-125, 173/190; documented deliberate pre-`setupComponents` behavior, comment L115-119). Same 200-no-op for `rewardKey == ""` on the drop-rule route (L173).
5. `provider.ApplyCampaignPolicy` / `provider.SetDropRule` non-nil error → `writePolicyMutationError` (handlers_policy.go:121, 186):
   - `errors.Is(err, settings.ErrShuttingDown)` OR `errors.Is(err, database.ErrClosed)` → **503** (responses.go:69-71 → writeServiceUnavailable responses.go:50-52);
   - anything else → **500** (responses.go → writeInternalError responses.go:35-37);
   - both with the ONE constant body `policyErrorMessage` = `"Drop policy could not be changed; no changes were made"` (handlers_policy.go:132) — no raw internal detail ever reaches the client; the raw error is logged server-side inside the miner (internal/miner/policy.go:313, 388).
6. success → `renderDropsList` → **200** HTML partial `drops_list`; a missing/broken partial template downgrades to **500** `"Failed to render partial"` (server.go:936-954), possibly after a partial 200 body has been written (ExecuteTemplate mid-write failure, server.go:950-953).

Client body summary: 405 `"method not allowed\n"`; 500 `"invalid form\n"`; 409 localized
`lc.settings_conflict` + `"\n"` (trailing newline from `http.Error`, pinned by
internal/web/settings_lifecycle_conflict_test.go:64); 503/500 mutation failure
`"Drop policy could not be changed; no changes were made\n"`; 200 the `drops_list`
HTML partial re-rendered from freshly re-sampled provider state.

## Lock map at the three audit points (mutation path, both handlers)

- **MUTATION point** — in the CALLEE (miner), not the handler: `m.mu.Lock()` (the miner's own RWMutex) is held while `m.config.CampaignPolicy` is overwritten (internal/miner/policy.go:305-306) / while the `m.config.DropRules` map entry is set or deleted (policy.go:364-374). Admission wraps this in `Miner.fenced` → `beginApply` which holds `m.applyMu` only for the admission check itself and registers the call on `m.applyWG` (miner.go:2087-2098, 2137-2143). The web server's `s.mu` is NOT held (released at handlers_policy.go:108/170 before the provider call); `settingsTxnMu` is NOT taken on the policy routes at all.
- **PERSISTENCE point** — the SAME `m.mu` critical section: `persistLocked` → `config.SaveConfig` runs while still holding `m.mu` (policy.go:307/375 calling policy.go:226-231; the deliberate SaveConfig-under-m.mu invariant, comment policy.go:219-225). Persistence is the commit point; on failure the in-memory mutation is rolled back byte/shape-exactly BEFORE `m.mu.Unlock()` (policy.go:308-315, 376-390), so no reader can observe the rejected value.
- **PUBLISH point** — NO lock at the publish itself: only after persistence succeeded and `m.mu` was released, `refreshPolicy(time.Now())` runs synchronously in the request goroutine (policy.go:317, 392) and publishes via `m.policySnap.Store(...)` (an atomic.Value, policy.go:60). Inside `refreshPolicy`, `m.mu.RLock` is taken briefly to read mode/games (policy.go:32-35) and again inside `snapshotDropRules` for the rules copy (policy.go:265-273).

Dependent side effects and their timing relative to persistence: ALL fire only AFTER a
successful persist, in this order inside `refreshPolicy` — `m.watcher.SetCampaignScores`
(policy.go:54-56), `m.discovery.SetGameRanks` (policy.go:57-59), then the snapshot
`Store` (policy.go:60) — then the handler's 200 re-render. On persist failure the only
side effect is a server-side `slog.Error` (policy.go:313, 388) and the HTTP 503/500;
nothing is ranked, published, or pushed to watcher/discovery. `refreshPolicy` is a no-op
when `m.dropsTracker == nil` (policy.go:28-30) — success still returns nil and renders 200.

---

## handleAPIPolicyMode in internal/web/handlers_policy.go (L86-L126)

**Purpose:** POST handler for `/api/policy/mode`: applies a new campaign-policy mode through the miner's fenced, fail-closed mutator, then re-renders the campaign-queue partial so the htmx caller sees the post-commit state.

**Inputs & Assumptions:**
- `w http.ResponseWriter`, `r *http.Request` — request is untrusted; CSRF/same-origin already enforced by `csrfProtectMiddleware` on this mux (server.go:894), Basic Auth optionally by server.go:895-897. Nothing inside the handler re-checks either.
- Form field `mode` (L120) — untrusted string, NOT validated in the handler. Enforcement of validity lives in the callee: `policy.Normalize` upper-cases/trims and silently maps any unknown value to `DefaultMode` = `GAME_ORDER` (internal/policy/policy.go:38, 51-57; pinned by policy_test.go:303-308). Assumption that the client posts a real mode: **nothing found** — garbage input is silently persisted as GAME_ORDER and answered 200.
- Implicit state: `s.lifecycleController` (may be nil — "no guard", handlers_settings.go:52-54), `s.policyProvider` (may be nil until a generation's `setupComponents` calls `SetPolicyProvider`, server.go:499-503; a RETIRED generation's provider is retained until the next generation re-registers — PolicyProvider doc, server.go:104-111).
- Precondition assumed: a non-nil error from `ApplyCampaignPolicy` means NOTHING changed (fail-closed). Established by the interface contract (server.go:106-111) and enforced in the miner by rollback-under-`m.mu` (policy.go:308-315) plus the `fenced` admission gate (miner.go:2137-2143, 2087-2098).

**Outputs & Effects:** One HTTP response per the classification chain above. On success: config field mutated + persisted to disk (miner side), policy re-ranked and published (atomic snapshot + watcher/discovery pushes), 200 `drops_list` partial. No state on the `Server` itself is written.

**Block-by-Block:**
- L87-94 method + form gate:
  ```go
  if r.Method != http.MethodPost { http.Error(w, "method not allowed", ...405); return }
  if err := r.ParseForm(); err != nil { writeInternalError(w, "invalid form"); return }
  ```
  What: rejects non-POST (405) and unparsable bodies (500). Why here: mux has no method routing. Assumes: nothing. Establishes: `r.FormValue` is usable below. Depended on by: L120's `r.FormValue("mode")`.
- L95-105 lifecycle 409 pre-check:
  ```go
  if s.lifecycleMutationBlocked() { s.writeSettingsConflict(w, r); return }
  ```
  What: paused/stopped/mid-transition → localized 409. Why here: lifecycle conflict is operator-resolvable (resume), distinct from the fence's 503 (comment L95-101). Assumes: the check→mutate race is acceptable because the miner fence backstops it (comment L100-101; fence rationale miner.go:2106-2120). Establishes: nothing durable — purely advisory. Depended on by: UX consistency with the settings routes.
- L106-108 provider sample:
  ```go
  s.mu.RLock(); provider := s.policyProvider; s.mu.RUnlock()
  ```
  What: snapshot the provider pointer under the Server's RLock, call it lock-free after. Why: `s.mu` must not be held across a provider call that itself takes miner locks and does disk I/O. Assumes: the pointer may already be a retired generation's — the callee's fence handles that (miner.go:2110-2120). Establishes: `provider` stable for this request. Depended on by: L109-124.
- L109-124 fail-closed mutate:
  ```go
  if provider != nil {
      if err := provider.ApplyCampaignPolicy(r.FormValue("mode")); err != nil {
          s.writePolicyMutationError(w, err); return
      }
  }
  ```
  What: mutate; on error classify and STOP — no re-render (comment L110-113: renderDropsList re-samples the provider and would paint a 200 success for a change that never happened). `provider == nil` deliberately keeps rendering the partial (comment L115-119, pinned by a sibling health-route test). Assumes: error ⇒ nothing changed (see above). Establishes: reaching L125 means either committed+published or no provider/no-op. Depended on by: the truthfulness of the 200 body.
- L125 `s.renderDropsList(w, r)` — 200 re-render; see renderDropsList section.

**Cross-Function Dependencies:**
- Callees: `lifecycleMutationBlocked` (nil-safe, snapshot-based), `writeSettingsConflict` (409 body), `Miner.ApplyCampaignPolicy` via `PolicyProvider` (fenced fail-closed commit; error values: `settings.ErrShuttingDown` from the fence, or a `fmt.Errorf`-wrapped `config.SaveConfig` error, policy.go:314), `writePolicyMutationError` (classification), `renderDropsList` (200 body).
- Callers: net/http via mux (server.go:727); htmx form on the Drops page.
- Shared state: `s.mu` guards only the provider/controller pointer reads; the config itself is guarded by the miner's `m.mu`; the published decisions by `m.policySnap` (atomic).
- Invariant couplings: "a retired generation never reports a configuration mutation as done" (fence, miner.go:2106-2120); "error body carries no internal detail" (policyErrorMessage discipline, L128-132); NOT coupled to `settingsTxnMu` — the settings read-modify-write transaction does not cover the policy routes (its enumerated writer set is settings/reset/quick-action only, handlers_settings.go:80-86).

**Open Questions:**
- The 200 re-render re-samples `s.policyProvider` a SECOND time (handlers_drops.go:214-219). If a generation handoff lands between L108 and L214-219 the body can be rendered from a DIFFERENT (newer) provider than the one mutated. Nothing enforces same-provider mutate/render; benign-looking (newer state is fresher) but unverified.
- Is the silent Normalize-to-default of an unknown `mode` (rather than a 400) an intentional product decision? No test found pinning the handler-level behavior for garbage input.

---

## handleAPIPolicyDropRule in internal/web/handlers_policy.go (L148-L191)

**Purpose:** POST handler for `/api/policy/drop-rule`: sets or clears (`reset`) the per-drop `config.DropRule` override for a reward key through the miner's fenced fail-closed mutator, then re-renders the campaign queue.

**Inputs & Assumptions:**
- Form fields (all untrusted): `rewardKey` (L172), `reset` (L175 — any non-empty value means "clear"), and five checkbox fields read via `checked` (L177-181; only `"on"`/`"true"` count as set, L193-196 — any other truthy-looking value, e.g. `"1"`, is silently unchecked: **nothing found** enforcing checkbox value conventions).
- `rewardKey` is NOT validated against any existing campaign/reward: the miner only lowercases/trims it (policy.go:341). An arbitrary key persists an arbitrary `DropRules` map entry into config.json. Enforcement of key existence or map-size bounds: **nothing found**.
- Same implicit state and fail-closed precondition as handleAPIPolicyMode (interface contract server.go:106-111; rollback in `commitDropRule` policy.go:376-390).

**Outputs & Effects:** Same classification chain. On success: `m.config.DropRules[key]` set, or deleted for the zero-value rule (`reset` posted, or all five boxes unchecked — the zero `config.DropRule{}` compares equal at policy.go:370), config persisted, policy re-ranked/published, 200 partial. `rewardKey == ""` or nil provider: 200 partial with no mutation — but note the fence is still entered BEFORE the empty-key check inside the miner (policy.go:332-346), so a retired generation answers 503 even for an empty key.

**Block-by-Block:**
- L149-156: identical 405/500 gates as the mode handler.
- L157-167: identical 409 lifecycle pre-check (same comment block).
- L168-170: identical `s.mu.RLock` provider sample.
- L172-188 rule build + fail-closed mutate:
  ```go
  key := r.FormValue("rewardKey")
  if provider != nil && key != "" {
      var rule config.DropRule // "reset" → zero value clears
      if r.FormValue("reset") == "" { rule = config.DropRule{ Skip: checked(r,"skip"), ... } }
      if err := provider.SetDropRule(key, rule); err != nil { s.writePolicyMutationError(w, err); return }
  }
  ```
  What: zero-value rule = clear; otherwise assemble from checkboxes. Why: one endpoint serves set/reset. Assumes: zero-rule-means-delete matches the miner (policy.go:370-373). Establishes: reaching L190 means committed or no-op. Depended on by: the Drops-page "Reset rule" control; `buildDropPolicyByCampaign`'s rule display (handlers_policy.go:71-77).
- L190 `s.renderDropsList(w, r)`.

**Cross-Function Dependencies:** as the mode handler, with `Miner.SetDropRule` → `commitDropRule` (policy.go:339-347, 363-394) as the mutator. Invariant coupling: `commitDropRule` mutates/restores the map IN PLACE so `cloneConfigLocked`'s DropRules aliasing stays valid ([R7], comment policy.go:359-362) — the handler is unaware of this but its 503/500-on-error behavior is what keeps a refused rule invisible to clients.

**Open Questions:**
- Unbounded, unvalidated `rewardKey` growth of `config.DropRules` persisted to disk by any dashboard-authenticated client — is any bound or GC applied elsewhere? Nothing found in this pass.
- A rule whose five posted checkboxes are all absent (without `reset`) is indistinguishable from an explicit reset (both are the zero value → delete). Intentional per the L146-147 doc ("all-unchecked clears"), but no handler-level test cited here distinguishes the two.

---

## writePolicyMutationError in internal/web/handlers_policy.go (L137-L143)

**Purpose:** Map a policy-mutation failure to a safe HTTP status: 503 when the failure is a fail-closed refusal from a draining/retired generation or closed DB (retry safe, nothing changed), 500 otherwise — always with the one constant Drops-page body.

**Inputs & Assumptions:** `err` non-nil (both call sites guard, L120-121, L185-186; a nil err would classify as 500 — no call site can reach that). Assumes every error passed in satisfies "nothing was mutated" — established for the two current producers by the miner's rollback/fence (policy.go:301, 327-330); NOT structurally enforced for future callers: **nothing found** beyond the doc comments.

**Outputs & Effects:** Writes exactly one response: 503 or 500, body `policyErrorMessage` (L132) + `"\n"` (http.Error). No logging here — the miner already logged the raw cause (policy.go:313, 388). No state writes.

**Block-by-Block:** L138-142:
```go
if mutationRefusedAsUnavailable(err) { writeServiceUnavailable(w, policyErrorMessage); return }
writeInternalError(w, policyErrorMessage)
```
What/Why: reuses the settings pipeline's classification (`writeApplyError`, handlers_settings.go:26-32 is the sibling) with Drops-page wording. Assumes: `mutationRefusedAsUnavailable` is the complete "retry is safe AND transient" set. Establishes: no raw error text in any client body on this route. Depended on by: handlers_policy.go:121, 186.

**Cross-Function Dependencies:** callee `mutationRefusedAsUnavailable` (responses.go:69-71), `writeServiceUnavailable`/`writeInternalError` (responses.go:50-52, 35-37). Mirrors `applyErrorMessage`/`writeApplyError` discipline (handlers_settings.go:18, 26-32).

**Open Questions:** none beyond the ErrClosed reachability question below.

---

## mutationRefusedAsUnavailable in internal/web/responses.go (L69-L71)

**Purpose:** Shared predicate: is this error a fail-closed refusal for which nothing was mutated and a short retry is correct → honest status 503 (not 500 server-fault, not 400 client-fault). Lifted from the settings pipeline for reuse by Drops/Health/Rewards handlers (doc L60-68).

**Inputs & Assumptions:** any `err` (nil-safe: `errors.Is` on nil returns false → callers classify nil as "other"/500, but no caller passes nil). Assumes producers wrap the sentinels compatibly with `errors.Is`: `settings.ErrShuttingDown` (internal/settings/dto.go:21; returned unwrapped by the fence, miner.go:2138-2139, aliased as `miner.ErrShuttingDown` miner.go:43-47) and `database.ErrClosed` (internal/database/database.go:18-21, returned by lifecycle-aware DB ops like `WithTx` after close).

**Outputs & Effects:** pure boolean; no side effects.

**Block-by-Block:** L70:
```go
return errors.Is(err, settings.ErrShuttingDown) || errors.Is(err, database.ErrClosed)
```
What: two-sentinel membership test. Why here: one classification point so 503-vs-500 cannot drift per handler. Assumes: both sentinels carry the "nothing was mutated" guarantee wherever they surface (settings doc dto.go:8-21; database doc database.go:18-21). Establishes: the 503 branch for `writeApplyError` and `writePolicyMutationError`. Depended on by: handlers_settings.go:27, handlers_policy.go:138 (and per its doc, Health Center/Rewards handlers).

**Cross-Function Dependencies:** the dependency-direction note in dto.go:18 — the sentinel lives in `internal/settings` precisely so `internal/web` can `errors.Is` it without an import cycle.

**Open Questions:**
- `database.ErrClosed` appears unreachable through the two POLICY mutators specifically: `ApplyCampaignPolicy`/`SetDropRule` touch only the config file (`persistLocked` → `config.SaveConfig`, policy.go:226-231), never the DB. The branch exists for the shared classifier's other producers. No harm identified; noted for completeness. Enforcement that this stays true (i.e., that a future policy persistence path through SQLite keeps the nothing-mutated contract): **nothing found**.

---

## lifecycleMutationBlocked in internal/web/handlers_settings.go (L48-L60)

**Purpose:** Advisory predicate answering "should a settings-shaped mutation be refused with a friendly 409 right now?" — true when the miner is paused/stopped or a lifecycle transition is in flight; false when running/degraded/failed or when no lifecycle controller is wired.

**Inputs & Assumptions:** implicit `s.lifecycleController` (nil for every pre-Ф4c test/build → "no guard", exactly historical behavior — doc L34-47; interface handlers_lifecycle.go:28-34). Assumes `ctrl.Snapshot()` is safe to call lock-free and returns a coherent point-in-time view (established by the lifecycle package, not re-verified here). Assumes staleness is acceptable: the answer can be outdated by the time the caller mutates — explicitly declared UX sugar with `writeApplyError`'s / the fence's ErrShuttingDown→503 path as the authoritative backstop (doc L44-47).

**Outputs & Effects:** pure boolean; no writes.

**Block-by-Block:**
- L49-54: `s.mu.RLock`-sample `ctrl`; `nil → false` (no guard).
- L55-59:
  ```go
  snap := ctrl.Snapshot()
  if snap.Observed == lifecycle.ObservedPaused || snap.Observed == lifecycle.ObservedStopped { return true }
  return snap.Transition != lifecycle.TransitionNone
  ```
  What: blocked on paused/stopped OR any in-flight transition. Why: mutating a torn-down generation's settings is pointless; mid-transition is ambiguous. NOT blocked while running/degraded (generation live) or failed (fail-closed apply already 503s) — doc L42-45. Assumes: the enumerated states are the complete "generation torn down or ambiguous" set. Establishes: the 409 branch in three settings writers and both policy handlers. Depended on by: handlers_policy.go:102, 164 and the settings routes.

**Cross-Function Dependencies:** known documented gap (internal/miner/lifecycle_replacement_gap_test.go:236-243): after a restart transition COMPLETES while generation N+1 is still short of `setupComponents`, this gate is OPEN (no 409) and the request proceeds against generation N's still-registered provider — the miner fence then answers `ErrShuttingDown` → 503. So the 409/503 split is best-effort by construction.

**Open Questions:** none — the race is documented and backstopped; whether "failed" should also 409 is an explicit recorded product decision (doc L43-45).

---

## writeSettingsConflict in internal/web/handlers_settings.go (L64-L66)

**Purpose:** Emit the friendly, server-localized 409 body for a mutation refused because the miner is paused/stopped/mid-transition.

**Inputs & Assumptions:** `r` used only for language selection: `langFromRequest` reads the language cookie, normalized, defaulting to `i18n.DefaultLang` (i18n.go:32-39) — untrusted cookie value passes through `i18n.NormalizeLang` before use. Assumes the `lc.settings_conflict` catalog key exists for the resolved language (fallback behavior of `s.i18n.T` for a missing key not traced in this pass).

**Outputs & Effects:** one 409 response, body = translated `lc.settings_conflict` + `"\n"` (http.Error via writeConflict, responses.go:56-58). Pinned by settings_lifecycle_conflict_test.go:64.

**Block-by-Block:** L65: `writeConflict(w, s.i18n.T(s.langFromRequest(r), "lc.settings_conflict"))` — single expression; localization here rather than at call sites so every 409 body is identical across the settings and policy routes. Depended on by: handlers_policy.go:103, 165 and the settings writers.

**Cross-Function Dependencies:** `writeConflict` (responses.go:56-58), `langFromRequest` (i18n.go:32-39).

**Open Questions:** `s.i18n.T` fallback for an unknown key/language on this direct-call path (as opposed to the template `t` func documented at i18n.go:52-55) not verified.

---

## renderDropsList in internal/web/handlers_drops.go (L214-L246)

**Purpose:** Build and render the `drops_list` campaign-queue partial, merging campaign data, watchdog health badges, and campaign-policy decisions. Shared by the read poll (`handleAPIDrops`, L59-61) and by the 200 success path of BOTH policy POST handlers — it is what a 200 mutation response's body contains.

**Inputs & Assumptions:**
- Implicit state, all re-sampled fresh under one `s.mu.RLock` (L215-219): `s.campaignsProvider`, `s.dropProgressProvider`, `s.policyProvider` — each independently nilable; every nil degrades to an honest empty section rather than an error.
- Assumes provider methods are safe to call lock-free and return data safe to read during template execution. For policy rules this IS enforced: `CurrentCampaignPolicy` returns a private copy made under `m.mu.RLock` (policy.go:277-282, 265-273); `PolicySnapshot` returns the atomically-published immutable snapshot (policy.go:252-258, stored at policy.go:60). For `provider.Campaigns()` / `SyncStatus()` / `DropProgress()` the copy/immutability contract lives in the drops tracker / health packages and was NOT traced in this pass — open question below.
- When reached from a mutation handler, assumes the mutation either committed-and-published or was a no-op (established by the handlers' fail-closed early returns, handlers_policy.go:120-123, 185-188). Because `ApplyCampaignPolicy`/`commitDropRule` call `refreshPolicy` SYNCHRONOUSLY before returning nil (policy.go:317, 392), the `PolicySnapshot` this function samples already reflects the just-committed change — EXCEPT when `refreshPolicy` no-opped on `m.dropsTracker == nil` (policy.go:28-30), where mode (read live from config at L238) is fresh but decisions (L237) are stale/empty.

**Outputs & Effects:** one 200 HTML partial (Content-Type set at server.go:937 before execution; template failure → 500 per server.go:944-953, potentially after partial output). No state writes anywhere.

**Block-by-Block:**
- L215-219 provider sampling under `s.mu.RLock` — same pattern and rationale as the POST handlers; the THREE pointers are sampled together so one coherent generation's set is used for this render (though not necessarily the set the preceding mutation used — see open question in handleAPIPolicyMode).
- L221-231 campaign + health sampling:
  ```go
  if provider != nil { campaigns = provider.Campaigns(); status = provider.SyncStatus() }
  if progressProvider != nil { progress = progressProvider.DropProgress() }
  ```
  What: raw evidence for cards, freshness chips, and health badges. Nil providers leave zero values → `buildDropsListData` renders the never-synced S-UNK block (L259-267). Depended on by: L239, L242-243.
- L233 `tr` closure — per-request language binding via `langFromRequest`.
- L235-240 policy sampling:
  ```go
  if policyProvider != nil {
      _, decisions := policyProvider.PolicySnapshot()
      _, rules := policyProvider.CurrentCampaignPolicy()
      policyByID = buildDropPolicyByCampaign(campaigns, decisions, rules, tr)
  }
  ```
  What: decisions from the atomic post-commit snapshot; rules as a fresh locked copy; joined per-campaign into badge views (`buildDropPolicyByCampaign` L31-82: returns nil when no decisions exist yet, keys per-drop controls by `models.NormalizeRewardKey(gameID, currentDrop.Name)` L69 — the same normalization `SetDropRule` applies at policy.go:341, which is what makes a just-POSTed rule show up on the re-render). Note the two reads are NOT atomic with each other: decisions and rules can straddle a concurrent commit (rules newer than the decisions ranked from them, or vice versa) — display-only skew, self-corrects on the next poll. Discarded first return values: the snapshot's `Mode` and the live mode string are both unused here (the mode <select> is rendered by `handleDropsPage`, L37-40, not by this partial).
- L242-245 assembly + render: `buildDropCampaignViews` (ordering mirrors the watcher's DROPS priority, L612-647), `buildDropsListData` (R17 state blocks; raw `status.LastError` gates the DEGR strip while the displayed Cause crosses `supportbundle.Redact`, L269-284), then `s.renderPartial(w, r, "drops_list", data)`.

**Cross-Function Dependencies:**
- Callees: `Miner.PolicySnapshot` / `Miner.CurrentCampaignPolicy` (lock-free atomic read / `m.mu.RLock` copy), `buildDropPolicyByCampaign`, `buildDropCampaignViews`, `buildDropsListData`, `renderPartial`, `displayLocation` (server.go:465-473, `s.mu.RLock`).
- Callers: `handleAPIDrops` (poll), `handleAPIPolicyMode` L125, `handleAPIPolicyDropRule` L190. The mutation callers depend on this function NOT being reached on a mutation error (their fail-closed comment, handlers_policy.go:110-113) — this function itself would happily render a truthful-looking 200 for any state it samples.
- Shared state: `s.mu` (pointer reads only), miner's `m.mu` + `m.policySnap` (via the provider), the drops tracker's internal state (via `Campaigns`/`SyncStatus`).
- Invariant couplings: "a 200 from a mutation route reflects committed state" holds through the CHAIN handler-fail-closed → miner rollback-under-lock → synchronous refreshPolicy-before-return → this function's fresh re-sample; no single piece enforces it alone.

**Open Questions:**
- Copy/immutability contract of `campaignsProvider.Campaigns()` / `SyncStatus()` and `dropProgressProvider.DropProgress()` for lock-free template rendering — owned by internal/drops / internal/health, not traced in this pass (see audit-context/functions/policy-readers.md if it covers the reader side).
- The decisions/rules non-atomic pair read (L237-238) — any consumer beyond display that could be confused by the skew? None found on this path.
