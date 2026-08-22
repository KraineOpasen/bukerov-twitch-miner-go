# Fence admission: fenced / beginApply / endApply / teardown's applyWG drain

Target: `internal/miner/miner.go` @ HEAD 5b331e5. Audit focus: admission vs serialization
(applyWG is a counter, not a mutex), lock order, and what retirement guarantees about
`CurrentConfig` stability. All paths below are absolute-repo-relative to
`/home/user/bukerov-twitch-miner-go`.

## beginApply in internal/miner/miner.go (L2085-L2096)

**Purpose:** Admission gate for configuration mutations: registers the caller as an in-flight
settings apply in `applyWG`, or refuses (returns false) once the miner is draining
(`applyDraining` set by teardown) or its run context is already cancelled. It is the
admission half of the M1 shutdown/configuration-mutation interlock described at
internal/miner/miner.go:287-308.

**Inputs & Assumptions:**
- No parameters. Implicit state: `m.applyMu`, `m.applyDraining`, `m.applyWG`, `m.runCtx`
  (struct fields at internal/miner/miner.go:306-308 and :197).
- Assumes `m.runCtx` is safely published before any concurrent call. Established for
  web-driven calls: `Run` writes `m.runCtx = ctx` at internal/miner/miner.go:484, strictly
  before `m.setupComponents(ctx)` at :498 registers this miner as a provider
  (internal/miner/miner.go:820-825, :843-849) under the web `Server`'s `s.mu`
  (internal/web/server.go:487-509); handlers re-read the provider under `s.mu.RLock`
  (internal/web/handlers_policy.go:106-108), so the lock-mediated provider publication
  carries a happens-before for the `runCtx` write. For a library caller invoking
  `SetAutoRedeem`/`ApplySettings`/etc. concurrently with `Run`'s first lines
  (before L484 executes): **nothing found** — the read at :2091 holds `applyMu`, but the
  write at :484 holds no lock; the `!= nil` guard tolerates a never-set `runCtx` (tests
  build struct-literal Miners and set it directly, e.g.
  internal/miner/srap_test.go:351-353) but does not synchronize a mid-`Run` write.
- Assumes every caller that receives `true` will call `endApply` exactly once ("Every
  accepted apply MUST defer endApply", internal/miner/miner.go:2084). Established only by
  convention at the two call sites: `fenced` defers it at :2139 and `applySettings` at
  :2214. No mechanical enforcer — **nothing found** beyond those two deferred calls; a third
  future caller that forgets would wedge teardown's unbounded `applyWG.Wait()` (see drain
  section).

**Outputs & Effects:**
- Returns `true` after `m.applyWG.Add(1)` (L2094) — the caller is now registered and
  teardown's `Wait` cannot return until the matching `Done`.
- Returns `false` with **no side effect whatsoever** when `applyDraining` is set (L2088-2090)
  or `runCtx` is non-nil and cancelled (L2091-2093).
- Postcondition (the WaitGroup-contract point, internal/miner/miner.go:293-297): because the
  check-then-`Add(1)` runs entirely under `applyMu`, and teardown's set-`applyDraining` also
  runs under `applyMu`, an `Add` can never race a `Wait` that has already observed zero —
  the exact `sync.WaitGroup` re-use hazard the comment names.

**Block-by-Block:**
- L2086-2087 `m.applyMu.Lock(); defer m.applyMu.Unlock()` — What: takes the interlock for
  the whole body. Why here: makes admission atomic with retirement (teardown L1926-1928).
  Assumes: `applyMu` is a leaf lock here (nothing else acquired inside). Establishes: mutual
  exclusion of "check draining, then Add" with "set draining, then Wait". Depended on by:
  teardown's drain correctness; `fenced`'s "no third interleaving" claim (:2117-2119).
- L2088-2090 `if m.applyDraining { return false }` — What: the retirement check. Why here:
  first, so a draining miner refuses before touching `runCtx` or the counter. Assumes:
  `applyDraining` only ever transitions false→true (set once at teardown L1927; **nothing
  found** that ever resets it — one-way by construction since `stop` runs once,
  internal/miner/miner.go:1899-1907). Establishes: every arrival after retirement is
  refused. Depended on by: `nextGenerationConfig`'s trust in the handoff sample
  (internal/app/app.go:668-673).
- L2091-2093 `if m.runCtx != nil && m.runCtx.Err() != nil { return false }` — What: the
  cancelled-context check. Why here: closes the window between `ctx.Done()` firing and
  teardown actually setting `applyDraining` — "a POST arriving after runCtx.Done() fires but
  before the web listener is actually closed" (:2080-2084), which matters because
  `web.Server.Stop` is a hard `Close`, not a graceful `Shutdown` (:2082-2083), so nothing in
  the web layer joins an in-flight request. Assumes: on both shutdown paths ctx is cancelled
  before or with `stop()` (normal path: `<-ctx.Done()` at :506 precedes `m.stop()` at :513;
  startup failure: `failStartup` cancels via `shutdownFn` FIRST per :184-188). Establishes:
  fail-closed behavior even in the cancel→drain gap. Depended on by:
  internal/miner/srap_test.go:345-366 pins this half; :367-385 pins the draining half.
- L2094-2095 `m.applyWG.Add(1); return true` — What: registration. Establishes: teardown's
  `Wait` blocks until this apply's `endApply`. Depended on by: the acknowledgement-ordering
  guarantee in `fenced`'s doc (:2123-2127) and the DB-close safety argument (:1917-1922).

**Cross-Function Dependencies:**
- Callers: `fenced` (L2136) and `applySettings` (L2211) — both translate `false` into
  `ErrShuttingDown` (:2137, :2212), an alias of `settings.ErrShuttingDown`
  (internal/miner/miner.go:43-47). The web layer maps that to HTTP 503 via
  `mutationRefusedAsUnavailable` (internal/web/handlers_policy.go:137-143).
- Counterpart: teardown L1926-1929 (retirement + drain). Shared state: `applyMu`,
  `applyDraining`, `applyWG`.
- Invariant coupling: `loopWG` deliberately has NO such gate — its no-Add-after-Wait
  argument is program order inside `Run` (internal/miner/miner.go:317-323); a loop ever
  started from a settings apply would need an applyWG-style gate (:321-323).

**Open Questions:**
- Is a concurrent direct (non-web) mutator call during `Run`'s first ~10 lines a supported
  use? The `runCtx` read/write pair (:2091 vs :484) is unsynchronized for that interleaving.

---

## endApply in internal/miner/miner.go (L2100-L2102)

**Purpose:** Releases the registration `beginApply` made: `m.applyWG.Done()` and nothing
else.

**Inputs & Assumptions:**
- Precondition: a matching successful `beginApply` on this goroutine's call path. Established
  by both call sites deferring it immediately after the admission check
  (internal/miner/miner.go:2139, :2214). An unmatched call would panic
  (`sync.WaitGroup` negative counter) — **nothing found** guarding against a third caller
  misusing it; only the two disciplined sites exist at HEAD.

**Outputs & Effects:**
- Decrements `applyWG` (L2101). When this is the last in-flight apply and `applyDraining` is
  already set, it releases teardown's `Wait` (L1929). No lock is held at the `Done` — none is
  needed; the `applyMu` atomicity argument covers only check-vs-Add and set-vs-Wait.
- Timing postcondition that matters for the audit: because it runs as a `defer` AFTER the
  wrapped mutation body returns, the `Done` fires after the mutation's persistence AND its
  dependent side effects (e.g. `refreshPolicy`) have completed — this ordering is what makes
  "anything answered successfully is in the handoff" true (:2123-2127).

**Block-by-Block:** single statement L2101, covered above.

**Cross-Function Dependencies:** `fenced` (defer, L2139), `applySettings` (defer, L2214);
teardown's `applyWG.Wait()` (L1929) is the sole waiter — **nothing found** waiting on
`applyWG` anywhere else in the package.

**Open Questions:** none.

---

## fenced in internal/miner/miner.go (L2135-L2141)

**Purpose:** Admits `fn` as a configuration mutation on THIS miner generation; refuses with
`ErrShuttingDown` — before `fn` runs, so with zero side effects — once the generation is no
longer the authoritative mutable one (:2104-2106). It exists because a retired generation's
web providers are never cleared (:2108-2112): the retired miner stays the dashboard's target
until the NEXT generation re-registers providers in `setupComponents`, which its `Run`
reaches only after `runAuthenticate`/`runLoadStreamers` (internal/miner/miner.go:490-498) —
a window `retryStartupLookup` can hold open for an entire Twitch outage
(internal/app/app.go:681-689).

**Inputs & Assumptions:**
- `fn func() error`: fully trusted closure supplied by the four exported wrappers below; it
  performs its own locking. `fenced` holds NO lock while `fn` runs — `applyMu` was released
  inside `beginApply` (:2087); only `applyWG` membership persists across `fn`.
- Assumes the web check→call race is real and unfixable in `internal/web`: every mutation
  handler samples its provider under the Server's RLock and calls it after releasing that
  lock (internal/web/handlers_policy.go:106-120; the handler comment at :95-101 explicitly
  defers to "the fence inside the miner" as "the authoritative backstop for the unavoidable
  race between checking here and mutating there"). Fencing in the callee is what closes it
  (:2115-2121).
- Assumes teardown's first act is the applyMu-guarded retirement (verified: L1926-1929 are
  teardown's first statements; the only thing earlier in the `stop()` body is the tests-only
  `stopObserver` seam, L1901-1903, nil in production per :378-383).

**Outputs & Effects:**
- Refusal path: returns `ErrShuttingDown` (L2137) with no side effect. Error propagation:
  `ErrShuttingDown = settings.ErrShuttingDown` (:43-47) flows verbatim out of the exported
  wrapper to the web handler, which maps it to 503 "retry is safe, nothing changed"
  (internal/web/handlers_policy.go:134-143); callers must not report success on non-nil
  error (internal/miner/policy.go:282-284).
- Admitted path: returns `fn()`'s error verbatim (L2140), with `endApply` deferred (L2139) so
  the `applyWG` registration outlives everything `fn` does.

**Block-by-Block:**
- L2136-2138 `if !m.beginApply() { return ErrShuttingDown }` — What: admission. Why here:
  refusing BEFORE `fn` keeps "a retired generation never reports a configuration mutation as
  done" exception-free — `SetDropRule` deliberately fences before even its empty-key no-op
  check (internal/miner/policy.go:304-310), and `SetAutoRedeem` fences before its roster
  guards so a frozen roster can't misreport a lifecycle refusal as "not tracked"
  (internal/miner/rewards.go:161-165). Establishes: mutual exclusion with retirement, no
  third interleaving (:2117-2119).
- L2139 `defer m.endApply()` — covered under endApply.
- L2140 `return fn()` — What: the mutation itself, unserialized by this function. The
  MUTATION/PERSISTENCE/PUBLISH lock story lives inside each `fn`:
  - `ApplyCampaignPolicy` (internal/miner/policy.go:285-294): mutation of
    `m.config.CampaignPolicy` under `m.mu` (L287-288); persistence via `persistLocked`
    under the SAME `m.mu` hold (L289; persistLocked's contract "Caller holds m.mu",
    internal/miner/policy.go:218-226); publish is the same in-place write (no candidate) —
    mutation IS publication, under `m.mu`. Dependent side effect `refreshPolicy` fires
    AFTER `m.mu` is released (L290-291), i.e. after persistence was ATTEMPTED — and fires
    even when `persistLocked` only LOGGED a failed `config.SaveConfig`
    (internal/miner/policy.go:220-226): the change stays live in memory on a failed save
    (documented at internal/miner/miner.go:2048-2052).
  - `SetDropRule` → `commitDropRule` (internal/miner/policy.go:311-338): identical shape —
    map mutation under `m.mu` (L326-334), `persistLocked` under `m.mu` (L335),
    `refreshPolicy` after unlock (L336-337), same log-only save failure.
  - `SetAutoRedeem` → `setAutoRedeem` (internal/miner/rewards.go:166-168, 170+): mutation
    under `m.mu` (L173+); on save failure the config mutation is restored EXACTLY before
    returning the error, and runtime state + generation bump happen ONLY after a successful
    save (rewards.go:156-160) — here persistence IS the commit point and the error
    propagates to the caller.
  - `ApplyHealthSettings` → `applyHealthSettings` (internal/miner/health.go:451-459):
    serialized end-to-end by `healthApplyMu` (health.go:441-445, taken INSIDE the fence,
    :446-447), candidate built off a clone, `m.config` untouched until
    `config.SaveConfig` on the candidate succeeds (health.go:426-431) — publish-after-
    persist under `m.mu`, save failure propagates.

**Cross-Function Dependencies:**
- Callees: `beginApply`/`endApply` (above) and the four `fn` bodies (internal/miner/
  rewards.go:167, policy.go:286, policy.go:312, health.go:452).
- The ordering guarantee callers of the HANDOFF depend on (:2123-2127): an admitted mutation
  holds `applyWG` → teardown waits on it before `Run` returns → the lifecycle controller's
  single main loop runs the next generation's factory only after `awaitGeneration` observed
  `Run` return (internal/app/app.go:664-666) → `nextGenerationConfig` samples
  `outgoing.CurrentConfig()` (internal/app/app.go:707-711). So anything a client saw
  answered successfully is in the handoff.
- **Admission, NOT serialization** (:2129-2134, struct doc :299-305): `applyWG` is a
  counter; two `fenced` calls run `fn` concurrently, serialized only by whatever locks the
  bodies take (`m.mu`, `healthApplyMu`). `applySettings` additionally takes `coordinatorMu`
  (:2216-2217; lock order coordinatorMu → mu → streamer.Manager.mu → models.Streamer.mu,
  :276-285) which the `fenced` callers do NOT take. Documented residual this fence does not
  fix (:2131-2134, :2204-2209): `applySettings` clones the config, does unlocked I/O, then
  publishes; a concurrent `ApplyCampaignPolicy`/`ApplyHealthSettings` commit landing inside
  that window mutates live VALUE fields the clone already snapshotted and is silently
  overwritten by the apply's publish. Only AutoRedeem is exempted, by rebuilding
  `candidate.AutoRedeem` from the LIVE map at the commit point inside the same `m.mu`
  section as SaveConfig and the publish (:2196-2203). Echoed at
  internal/app/app.go:704-706.
- Lock order summary for the fence itself: `applyMu` alone, released before `fn`; never held
  together with `coordinatorMu`/`m.mu`/`healthApplyMu` — a refused `ApplyHealthSettings`
  never contends for `healthApplyMu` (health.go:446-447). No nesting → no deadlock cycle
  involving `applyMu` is constructible from these sites.

**Open Questions:**
- The clone-window residual is documented as accepted; whether any caller builds on
  "admitted implies durable" for CampaignPolicy/DropRules (where a failed save is log-only
  and the handoff carries the unsaved value forward — internal/app/app.go:631-638) is a
  hunting-phase question, not settled here.

---

## teardown (applyWG drain prologue) in internal/miner/miner.go (L1916-L1929)

**Purpose:** First act of the single-execution teardown body: retire the generation for
mutation purposes and drain every already-admitted configuration mutation to completion,
BEFORE any component teardown and, transitively, before `App.Shutdown` closes the shared DB
handle (which happens only after `Run` returns — :1917-1922).

**Inputs & Assumptions:**
- Runs exactly once, on the `Run` goroutine, via `stop()`'s `stopOnce` (L1899-1907), reached
  either after `<-ctx.Done()` (:506-513) or from `failStartup`; on both paths the run
  context is already cancelled before this executes (:506, :184-188), so `beginApply`'s
  runCtx check is already refusing — the `applyDraining` write here is what makes retirement
  atomic-with-Wait and permanent rather than dependent on ctx wiring.
- Assumes every admitted apply terminates: **nothing found** bounding `applyWG.Wait()` —
  unlike `joinLoops` (bounded by `loopJoinTimeout`, :1938), this Wait has no timeout. An
  admitted `applySettings` doing Twitch resolution (`PlanReconcile`, :2225) or an fsync on a
  wedged filesystem (`WriteFileAtomic` "has no timeout", internal/app/app.go:661-662) holds
  shutdown open for its full duration.
- Assumes no admitted path calls back into `stop()` (re-entrant deadlock on the once guard,
  :1913-1915) — enforced only by the stated rule; **nothing found** mechanically.

**Outputs & Effects:**
- MUTATION point: `m.applyDraining = true` at L1927, under `m.applyMu` (L1926-1928). This is
  the retirement write; it is never reset.
- L1928-1929: `applyMu` is RELEASED before `m.applyWG.Wait()` — deliberate, since `endApply`
  does not take `applyMu`; holding it across Wait would be pointless but not deadlocking,
  releasing it is what lets late `beginApply` arrivals fail fast in parallel with the drain.
- No error: this block produces none; `drainErrs` aggregation starts after (L1931-1939).
  The eventual aggregate becomes `m.stopErr` (memoized, L1904-1906) and `Run` wraps a
  non-nil result as "shutdown drain incomplete: %w" (:513-515).
- Postconditions once `Wait` returns (L1929):
  1. No configuration mutation is mid-flight, and none can start — every later `beginApply`
     refuses (L2088-2090). So when the App-owned DB is closed after `Run` returns, no apply
     is touching it (:1917-1922).
  2. **What retirement guarantees about `CurrentConfig` stability:** "what this drains is
     final and what CurrentConfig hands to the next generation cannot change afterwards"
     (:1922-1925). Concretely: all five config-writing entry points are gated —
     `applySettings` via `beginApply` (:2211) and `SetAutoRedeem`/`ApplyCampaignPolicy`/
     `SetDropRule`/`ApplyHealthSettings` via `fenced` (rewards.go:167, policy.go:286, :312,
     health.go:452; enumerated at internal/app/app.go:668-673). The one unfenced config
     writer, the owner-identity reconciliation (:681-691, saves off `m.mu` per :2050-2051),
     runs in `Run`'s startup path on this same goroutine and therefore cannot execute after
     this point in THIS generation. Non-config provider methods (notably
     `RedeemCustomReward`) stay fully live on the retired generation
     (internal/app/app.go:700-703) but do not write `m.config`. PUBLISH point of the
     handoff: `nextGenerationConfig` samples `outgoing.CurrentConfig()` — an isolated
     snapshot under the retired miner's `m.mu.RLock` (:2072-2076) — with no App lock held,
     then publishes to `a.cfg` under `cfgMu` (internal/app/app.go:707-719); the snapshot
     isolation stays load-bearing (not belt-and-braces) because the retired miner remains
     reachable via never-cleared providers, and handing the live object to the next miner
     would put one map behind two mutexes — a fatal concurrent-map throw (:2057-2071).

**Block-by-Block:**
- L1926-1928 retirement write — What/Why/Establishes covered above. Depended on by:
  `beginApply` L2088; pinned by internal/miner/acceptance_m1_test.go:584-604 (draining set →
  new apply refused before touching applyWG; in-flight apply blocks Wait) and
  internal/miner/m4_lifecycle_test.go:355-358, provider_safety_test.go:182-253 (every fenced
  entry point returns ErrShuttingDown after teardown),
  stale_generation_fence_test.go:400-462 (provider sampled BEFORE retirement, called after —
  refused).
- L1929 `m.applyWG.Wait()` — What: the drain. Why FIRST, before `joinLoops`/transport
  closes: those later steps are about network-driven writers (S1, :1933-1943); this one is
  about HTTP-driven config mutations, whose only join point in the whole process is this
  Wait (web Stop is a hard Close, :2082-2083). Assumes: matching-Done discipline (see
  beginApply). Establishes: postconditions 1 and 2.

**Cross-Function Dependencies:**
- Pairs with `beginApply`/`endApply` over `applyMu`/`applyDraining`/`applyWG`.
- Downstream in teardown: `joinLoops` (L1939), transport closes (L1944-1949), component
  stops — all strictly after the drain, so no admitted apply can observe a half-torn-down
  component set.
- Downstream in the process: `App.Shutdown`'s DB close and the lifecycle controller's
  factory call, both strictly after `Run` returns (:1917-1919; internal/app/app.go:664-666,
  :360-381).

**Open Questions:**
- The unbounded `Wait` is a deliberate asymmetry with every other bounded drain in teardown;
  whether a hostile/wedged Twitch resolution inside an admitted `applySettings` can hold
  process shutdown open indefinitely (and whether `applySettings`' ctx bounds all of its
  I/O — `PlanReconcile` is called with a nil third argument at :2225) was not chased to the
  bottom here.
