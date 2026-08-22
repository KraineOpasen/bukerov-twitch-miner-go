# Owner-identity reconciliation — audit context (HEAD 5b331e5)

Scope: `reconcileOwnerIdentity` (internal/miner/owner_identity.go) and the owner-identity
reconciliation block inside `Miner.authenticate` (internal/miner/miner.go L658–L693), including
lock discipline at the mutation/persistence/publish points, error propagation, lifecycle stage,
and dependent side effects.

---

## reconcileOwnerIdentity in internal/miner/owner_identity.go (L38-L52)

**Purpose:** Pure decision-and-mutation function that reconciles the persisted owner identity in a
`*config.Config` after a confirmed Login (BKM-006 Corrective Pass 1, C3 + COR-2): (a) adopts a
renamed owner's new canonical Twitch login into `cfg.Username` while pinning `cfg.ProfileKey` to
the pre-rename login so local storage never re-keys, and (b) backfills the `cfg.OwnerUserID` trust
pin exactly once from a session-confirmed user ID. Returns an `ownerIdentityResult` (L11–L14)
telling the caller what changed; the caller owns all logging and persistence (doc, L18–L21).

**Inputs & Assumptions:**
- `cfg *config.Config` — the LIVE config object of the calling generation; mutated in place.
  Trust: operator-controlled file content (config.go L85–L96 doc: `OwnerUserID` carries the same
  trust level as `Username`). Precondition: caller holds exclusive access — the function takes no
  lock. What establishes it: startup single-threading in `Run` (miner.go L486–L504, see the
  authenticate section) plus generation-private config pointers (app.go L711–L723,
  `nextGenerationConfig` hands each generation an isolated `CurrentConfig()` snapshot; miner.go
  L2701–L2073 doc: no two generations share a map). No mechanical enforcer — nothing found beyond
  call-order convention and the doc at miner.go L2052–L2057.
- `canonicalLogin string` — the account's CURRENT Twitch login from an authoritative validate, or
  `""` when none observed this session. Assumption "never foreign" (doc L23–L26) is established in
  `internal/auth`: `a.canonicalLogin` is assigned only after the identity checks pass, at
  validate.go L244–L248 (authoritative validate apply) and candidate.go L221–L223 (validated
  candidate promotion); `GetCanonicalLogin` (auth.go L302–L306) only reads it under `a.mu`.
- `confirmedUserID string` / `userIDConfirmed bool` — the auth session's user ID
  (`GetUserID`, auth.go L283–L287) and whether it was authoritatively confirmed THIS process
  (`IsUserIDConfirmed`, auth.go L336–L340 returns `userIDAuthoritative`, which is set true only at
  validate.go L243 / candidate.go L220 — never on a disk load). This is the BKM-005 trust
  boundary: an unconfirmed (disk-loaded) ID must never mint the pin (doc L33–L37).

**Outputs & Effects:**
- Returns `ownerIdentityResult{renamed, pinned}`; `changed()` (L16) is their OR.
- State writes (all in-place on `cfg`, no lock, no I/O):
  - `cfg.ProfileKey = cfg.Username` (L42) — only when currently whitespace-empty (L41), pinning
    storage to the pre-rename login exactly once.
  - `cfg.Username = canonicalLogin` (L44) — the rename adoption.
  - `cfg.OwnerUserID = confirmedUserID` (L48) — the one-time pin backfill.
- Postconditions: `cfg.StorageKey()` (config.go L500–L505: ProfileKey-if-set-else-Username) is
  byte-identical before and after a first rename (the pin captures the old Username before the
  overwrite — order at L41–L44 is load-bearing); an existing `OwnerUserID` is never overwritten
  (L47 guards on empty); idempotent — a second call with the already-adopted state is a no-op
  (proved by TestReconcileOwnerIdentity_TwoRestarts_StorageStable_COR2, owner_identity_test.go
  L66–L91).

**Block-by-Block:**
- L40–L46 — rename adoption:
  ```go
  if canonicalLogin != "" && !strings.EqualFold(canonicalLogin, cfg.Username) {
      if strings.TrimSpace(cfg.ProfileKey) == "" {
          cfg.ProfileKey = cfg.Username
      }
      cfg.Username = canonicalLogin
      res.renamed = true
  }
  ```
  What: detects a case-insensitive login difference and adopts the new login, pinning ProfileKey
  first. Why here: Twitch logins are mutable; storage paths (cookies/db/logs, keyed by
  `StorageKey()`) must not follow them (config.go L75–L83 doc). Assumes: canonicalLogin already
  passed the identity check (validate.go L244–L248). Establishes: `res.renamed`;
  storage-key stability across the rename. Depended on by: `authenticate`'s immediate
  `GetChannelID(m.config.Username)` (miner.go L710–L711) and every later Username consumer.
  `EqualFold` deliberately makes a case-only difference NOT a rename
  (TestReconcileOwnerIdentity_CaseOnlyDifference_NotARename_COR2, owner_identity_test.go L31–L40),
  avoiding a spurious config rewrite; the trade-off is `cfg.Username` keeps the operator's casing,
  not Twitch's canonical casing.
- L47–L50 — pin backfill:
  ```go
  if cfg.OwnerUserID == "" && userIDConfirmed && confirmedUserID != "" {
      cfg.OwnerUserID = confirmedUserID
      res.pinned = true
  }
  ```
  What: one-time backfill of the trusted owner pin. Why here: with a pin present,
  `auth.SetExpectedUserID` (auth.go L323–L327, called at miner.go L636 BEFORE Login) anchors the
  identity check on the user ID rather than the login, making every FUTURE restart
  rename-tolerant with no fresh Device Flow. Assumes: `userIDConfirmed` means "earned this
  session" (auth.go L329–L335). Establishes: `res.pinned`. Depended on by: the next restart's
  `authenticate` (miner.go L630–L636). Never overwrites an existing pin, and never accepts an
  unconfirmed ID (owner_identity_test.go L42–L58 covers backfill, no-overwrite, and the
  unconfirmed-refusal branches).

**Cross-Function Dependencies:**
- Sole production caller: `Miner.authenticate` at miner.go L681. It relies on: purity (no I/O, no
  lock — doc L21), the in-place mutation of the live config, the result flags for logging/persist
  decisions, and StorageKey stability (auth was already dialed under the OLD `StorageKey()` at
  miner.go L626, so a rename adoption mid-authenticate must not move the key — it doesn't, per the
  pin-first ordering).
- Reads-only couplings: `config.StorageKey()` (config.go L500–L505) and its test
  (storage_key_test.go L10–L27) pin the fallback semantics this function's pin-first ordering
  depends on.
- Shared state: the `*config.Config` itself — see the authenticate section for who else may
  reference it and when.
- Tests: internal/miner/owner_identity_test.go L13–L91 (rename+pin, case-only, trust-boundary
  backfill, two-restart stability); internal/auth/cor2_canonical_login_test.go L15–L70 pins the
  producer side (GetCanonicalLogin surfaces the renamed login; matches configured login on a
  non-renamed startup so this function no-ops).

**Open Questions:**
- Case-only rename: `cfg.Username` retains the config-file casing forever (EqualFold at L40). Is
  any downstream consumer sensitive to canonical casing (IRC NICK at miner.go L878, dashboard
  labels)? Twitch logins are case-insensitive by convention — nothing found in-repo verifying it.

---

## authenticate (owner-identity reconciliation block) in internal/miner/miner.go (L620-L763, block L658-L693)

**Purpose:** Startup authentication stage: builds the `TwitchAuth` under the stable storage key,
runs Login, then — the audited block — reconciles the persisted owner identity in ONE save
(rename adoption + pin backfill), then constructs the GraphQL client and resolves/verifies the
miner's own user ID. The reconciliation must happen exactly here: after Login (canonical login /
confirmed ID exist) and before `GetChannelID(m.config.Username)` (L710–L711), which must see the
ADOPTED login or a renamed owner's startup would look up a dead login every run.

**Inputs & Assumptions:**
- `ctx context.Context` — the Run-derived cancellable context (miner.go L481–L484); bounds Login
  and the startup lookup retries (startup_retry.go L40–L63).
- `m.config` (live pointer) / `m.configPath` — set once in `New` (miner.go L421–L432). In the
  App-composed process the pointer comes from `app.nextGenerationConfig()` (app.go L360–L361,
  L711–L723): an ISOLATED snapshot per generation, so no retired generation's provider aliases it.
  `configPath` may be `""` (library use) — persistence is silently skipped then (L689).
- `m.auth` — created at L626 with `m.config.StorageKey()` as the credential-file key, so a
  renamed owner keeps loading the same cookie file (comment L623–L625). `SetExpectedUserID` with
  the (possibly empty) pin at L636 — empty preserves legacy login-anchor behavior (L633–L635).
- Precondition for the OFF-LOCK config mutation: no concurrent reader/writer of `m.config`
  exists. Established by: (1) `Run`'s strict ordering — authenticate (L490) runs before
  `setupComponents` (L498), and every web→miner bridge is registered only inside setupComponents
  (`SetRewardsProvider` L823/L847, `SetHealthProvider` L1077, `SetPolicyProvider` L1079), so the
  already-listening early web server (built by App at app.go L335–L348, started as an App Run
  step before Miner.Run) has NO path to this generation's config yet — `NewServerEarly` receives
  value copies (analytics settings, username string, storage key string; app.go L341), not the
  pointer; (2) generation isolation (app.go L701–L702: "CurrentConfig hands over an isolated
  snapshot, so no two generations ever share a map"). Mechanical enforcer of (1): nothing found —
  it is call-order convention documented at miner.go L2052–L2057.
- The web server, if present, is used ONLY via its status broadcaster (L638–L652: device-code
  events; L718–L728: lookup-retry banner). The reconciliation block itself never touches it.

**Outputs & Effects:**
- On success returns nil with: `m.auth` logged in, `m.config` identity-reconciled (possibly
  live-mutated), best-effort persisted, `m.client` constructed (L695–L697), own user ID resolved
  and identity-verified (L742), auth state saved best-effort (L757–L759).
- **MUTATION point** (L680–L681): `reconcileOwnerIdentity(m.config, ...)` mutates the LIVE
  `m.config` in place. Lock held: **NONE** — neither `m.mu` nor `coordinatorMu`. This is the one
  documented exception to the config-mutation lock discipline: every other config writer mutates
  under `m.mu` (in-place writers persist under `m.mu` and roll back on save failure —
  CurrentConfig doc L2048–L2052; candidate publishers persist then publish under `m.mu`, e.g.
  L2316–L2323). The auth getters called as arguments each take `auth.a.mu` internally
  (auth.go L283–L306, L336–L340) but release before returning.
- **PUBLISH point:** identical to the mutation point — because the write is in-place on the live
  pointer, mutation IS publication; there is no candidate/commit split and no lock. Visibility to
  later readers (all in this same goroutine until setupComponents registers providers) is by
  program order; visibility to eventually-spawned goroutines is via the happens-before edges of
  the synchronization each later component performs (e.g. `CurrentConfig` under `m.mu.RLock`,
  L2074–L2078). CurrentConfig's doc records the resulting honest caveat: this change "stays live,
  and visible here, even when the write to disk failed" (L2052–L2057).
- **PERSISTENCE point** (L689–L693): `config.SaveConfig(m.configPath, m.config)` — only when
  `res.changed()` and `configPath != ""`. Lock held: **NONE** (contrast: every settings-apply
  path persists under `m.mu`, e.g. L2316–L2321, precisely to serialize the whole-file rewrite).
  SaveConfig itself (config.go L721–L739) marshals a shallow copy and writes via
  `util.WriteFileAtomic` (owner-only 0600, temp+rename — a crash cannot truncate the live file).
- **Error propagation:**
  - `m.auth.Login` error (L654–L656) → returned verbatim → `Run` wraps it
    `"authentication failed: %w"` (L490–L491) → `failStartup` (L578–L591): cancels the run
    context, tears down via the same S1 `stop()` sequence, original error stays `errors.Is`-able.
  - `SaveConfig` failure (L690–L692) → **swallowed**: `slog.Warn("Failed to persist
    owner-identity reconciliation; will retry on the next restart")` and nothing else. Non-fatal
    by documented contract (L677–L679): both changes are re-derivable and re-attempted on the
    next restart until they persist. No HTTP/status-broadcaster acknowledgement of the failure —
    the dashboard never learns of it (the only broadcaster writes in authenticate are the
    device-code and lookup-retry states).
  - `resolveStartupIdentity` error (L742–L751) → wrapped (either `"failed to get user ID: %w"`
    or the identity-binding message naming `cookies/<StorageKey()>.json`) → returned →
    failStartup. Note L749–L750 uses the ADOPTED `m.config.Username` for display and the stable
    `StorageKey()` for the file name — the block under audit is what makes those diverge.
- **Dependent side effects, ordered relative to persistence:**
  1. BEFORE persistence: the two `slog.Info` lines (L682–L688) announcing rename/pin — so a
     subsequent save failure means the log shows an adoption that reached disk never/later.
  2. AFTER the persistence attempt, UNCONDITIONALLY (save success irrelevant): client build
     (L695), `GetChannelID(m.config.Username)` startup lookup (L710–L711) — the immediate
     consumer that needs the adopted login; on a renamed owner the OLD login would raise
     `ErrStreamerDoesNotExist` and force the resolveStartupIdentity fallback every startup,
     which is exactly what adoption avoids. Then `resolveStartupIdentity` (L742),
     `m.auth.SaveAuth()` (L757), and — after authenticate returns — `loadStreamers`
     (L779–L784, reads `m.config.Streamers`/`StreamerSettings`), and in setupComponents the
     IRC NICK (`NewChatManager(m.config.Username, ...)`, L878) and the notifications manager
     (`notifications.NewManager(..., m.config.Username)`, L1145): all read the adopted login.

**Block-by-Block (the reconciliation block, L658–L693):**
- L658–L679 — contract comment: one save for both concerns; (a) COR-2 adoption rationale,
  (b) C3 backfill trust argument (`userIDAuthoritative` resets every process start, so
  `IsUserIDConfirmed()` here proves THIS Login confirmed the identity; with no pin, the legacy
  login anchor in applyValidation had to have matched, else Login would have failed closed to a
  fresh Device Flow); the explicit non-fatal-save contract (L677–L679). Depended on by: the
  CurrentConfig caveat doc (L2052–L2057) which cites this as "a documented startup contract: the
  adopted canonical login is needed immediately and the save is retried on the next restart".
- L680–L688 — capture `oldLogin`, call `reconcileOwnerIdentity` (MUTATION+PUBLISH, no lock),
  log rename (old→new) and pin events. Assumes: exclusive access (see above). Establishes: the
  runtime identity every later component reads.
- L689–L693 — conditional PERSISTENCE (no lock) + warn-only failure handling:
  ```go
  if res.changed() && m.configPath != "" {
      if err := config.SaveConfig(m.configPath, m.config); err != nil {
          slog.Warn("Failed to persist owner-identity reconciliation; will retry on the next restart", "error", err)
      }
  }
  ```
  Assumes: SaveConfig's atomic swap prevents torn files; the whole-document rewrite cannot race
  another generation (this generation is the only live one during its own startup — the stale-
  generation fence covers the other direction, app.go L691–L710). Establishes: durability, best
  effort. Depended on by: the NEXT restart (reads the persisted ProfileKey/OwnerUserID/Username).

**Cross-Function Dependencies:**
- Callees: `reconcileOwnerIdentity` (purity, result flags, pin-first ordering);
  `auth.GetCanonicalLogin`/`GetUserID`/`IsUserIDConfirmed` (self-locked snapshot getters,
  auth.go L283–L340; producers at validate.go L242–L248 / candidate.go L218–L223);
  `config.SaveConfig` (atomic write, Discord-token-from-env scrub, config.go L721–L739);
  `config.StorageKey` (config.go L500–L505); `retryStartupLookup` (startup_retry.go L40–L63:
  fail-fast only on ErrUnauthorized/ErrStreamerDoesNotExist, jittered backoff otherwise);
  `resolveStartupIdentity` (miner.go L1661–L1675: pure; session-ID-authoritative,
  renamed-login fallback on ErrStreamerDoesNotExist, empty-session fails closed with
  `auth.ErrIdentityMismatch`, everything else verbatim).
- Callers: `Run` via `runAuthenticate` (L490, L523–L528 test seam). Run assumes authenticate
  leaves `m.auth`, `m.client`, and a reconciled `m.config` ready for `loadStreamers` and
  `setupComponents`, and that any error is safe to hand to `failStartup`.
- Shared state / invariant couplings: `m.config` (the off-`m.mu` write is tolerated only at this
  lifecycle stage — every post-startup writer uses `m.mu`, and the CurrentConfig snapshot
  discipline at L2059–L2078 exists so no other generation ever holds this pointer);
  `initialize()` already derived `m.dbBasePath` from `StorageKey()` at L603 BEFORE this block —
  consistent only because adoption never changes `StorageKey()` (the pin-first invariant);
  the auth credential file key (`a.username`, dialed at L626) likewise never re-keys
  (auth.go L317–L319).
- Tests: internal/miner/owner_identity_test.go L13–L91 (the pure mutation, incl. the two-restart
  storage-stability proof) and internal/miner/resolve_startup_identity_test.go L15–L101 (all four
  branches of the downstream identity decision: successful-lookup cross-check, mismatch fails
  closed, renamed-login session fallback with exactly-one-warning flag, empty-session fails
  closed regardless of lookup error, other errors verbatim). internal/auth/
  cor2_canonical_login_test.go covers the producer. No test drives authenticate's WIRING of the
  block itself (the one-save-for-both behavior, the SaveConfig-failure warn path, or the
  configPath=="" skip) — nothing found.

**Open Questions:**
- "Will retry on the next restart" (L691) is contingent on the next Login including an
  authoritative validate: on a degraded startup `GetCanonicalLogin()` can be `""` and the rename
  is simply not observed that run (a no-op, not a retry). The log line slightly overpromises;
  nothing found asserting the retry happens on the literal next restart.
- `configPath == ""` (library use) skips persistence with no warn at all (L689) — other paths
  document it as a no-op success (e.g. L2299–L2300, L2379–L2381); is silent-per-run adoption the
  intended library contract?
- The App-built early web server captured the PRE-rename `cfg.Username` at construction
  (app.go L341) and the injected server is never re-fed the adopted login (only the miner's
  library-fallback build at L835–L837 would use the post-adoption value). Does any web surface
  render that constructor-time username after a rename adoption?
- SaveConfig rewrites the WHOLE document from this generation's in-memory config (app.go
  L695–L698 names this hazard for stale generations); at this startup instant that clobbers any
  external hand-edit of config.json made since process load. Last-writer-wins at file level —
  nothing found guarding operator edits concurrent with startup (out of the fence's scope).
