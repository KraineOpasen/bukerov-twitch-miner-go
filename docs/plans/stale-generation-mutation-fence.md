---
artifact_contract: ce-unified-plan/v1
artifact_readiness: implementation-ready
execution: code
task_id: stale-generation-mutation-fence
base_sha: 5266736069081d4320a12255adb4e7a6ec136c34
---

# Stale-generation mutation fence

## Problem

The process-level `web.Server` outlives every `*miner.Miner` generation. A generation registers
itself as `PolicyProvider` / `HealthProvider` / `RewardsProvider` inside `setupComponents`
(`internal/miner/miner.go:812-839`, `:1069-1071`), and **nothing ever clears those fields**. The only
way a provider stops pointing at a dead generation is the *next* generation reaching its own
`setupComponents` — which `Run` reaches only after `runAuthenticate` and `runLoadStreamers` return,
and `retryStartupLookup` can hold that open for an entire Twitch outage.

Every web mutation handler samples the provider under `s.mu.RLock`, **releases the lock, then calls**
(`internal/web/handlers_policy.go:95-101` is representative). That is a check→call race by
construction: no amount of locking in the handler can fix it, because `internal/web` has no lifetime
protocol with a generation at all.

`internal/miner` already owns the correct admission interlock — `applyMu` / `applyWG` /
`applyDraining`, entered by `beginApply()` and released by `endApply()` — but it gates **exactly one**
entry point, `applySettings`. Four config mutators bypass it:

| Method | File | Returns | Persists |
|---|---|---|---|
| `ApplyCampaignPolicy` | `internal/miner/policy.go:281` | *(void)* | `persistLocked` → `SaveConfig` |
| `SetDropRule` | `internal/miner/policy.go:292` | *(void)* | `persistLocked` → `SaveConfig` |
| `ApplyHealthSettings` | `internal/miner/health.go:444` | `error` | `SaveConfig` |
| `SetAutoRedeem` | `internal/miner/rewards.go:161` | `error` | `SaveConfig` |

`config.SaveConfig` rewrites the **whole document** from the writing generation's own in-memory
config, atomically. So a retired generation's write reverts fields a newer generation already
committed — while the dashboard reports success, because it re-renders from that same stale provider.

### Measured RED on base `5266736`

End-to-end over real loopback HTTP, against a real `web.Server` holding a genuinely retired
generation (`Run` returned, teardown complete):

- `POST /api/policy/mode` → **200 OK**, `CampaignPolicy` `GAME_ORDER`→`SMART` in memory **and on disk**
- `POST /api/policy/drop-rule` → **200 OK**, rule written to memory **and disk**
- `POST /api/health/settings` → **200 OK**, canary channel written to memory **and disk**
- `SetAutoRedeem` → refuses only on an unrelated roster guard, never on lifecycle

## Chosen design — provider-side fencing on the existing interlock

Route the four mutators through the **already-existing** `beginApply`/`endApply` admission gate.

Why this closes the *full* race rather than narrowing it: `beginApply` takes `applyMu`, and
`teardown`'s **first act** is `applyMu.Lock(); applyDraining = true; applyMu.Unlock();
applyWG.Wait()` (`miner.go:1908-1917`). Those two are mutually exclusive with no third
interleaving. So a request that sampled the provider *before* retirement and calls *after* it finds
`applyDraining == true` (or `runCtx.Err() != nil`) and refuses **before touching any state**. The
stale pointer the handler holds becomes harmless because the object itself refuses.

Ordering guarantees, all verified in source:

- **Retirement boundary** — generation N stops being authoritative at `runCtx` cancellation, and
  unconditionally by the time `teardown` sets `applyDraining`. Every `Run` return after `runCtx` is
  set routes through `stop()`, plus `defer cancel()` at `miner.go:474`.
- **Authority boundary** — generation N+1 becomes the dashboard's mutation target when its own
  `setupComponents` re-registers the providers.
- **Nothing acknowledged is lost (w.r.t. retirement)** — an admitted mutation holds `applyWG`;
  `teardown` waits for it *before* `Run` returns; the lifecycle reaches the N+1 factory (and thus
  `nextGenerationConfig`'s `CurrentConfig` sample) only after that return. So an acknowledged write is
  always in the handoff.
- **Two generations can never both accept** — N is draining before `Run` returns; N+1's factory runs
  strictly after.
- **Lock order unchanged** — `applyMu` is a leaf: acquired and released wholly inside `beginApply`
  and inside teardown's three-line prologue, with no I/O and no nested acquisition.
- **Shutdown stays bounded** — the four bodies add only in-memory work plus the `SaveConfig` already
  on those paths; `refreshPolicy`, `canary.UpdateSettings` and `progressWatchdog.UpdateSettings` are
  all in-memory and non-blocking.

### Designs rejected

- **Clear providers on teardown** — a retiring generation N writing `web.Server`'s fields is a second
  owner mutating a process-level object and could null out N+1's registration. Scored lowest on
  ownership (3/10).
- **App-owned dynamic adapters** — moves generation identity into `internal/app` and re-points a
  process-level object per generation; large blast radius for no extra closure.
- **Epoch/generation tokens** — new state and a new invariant to keep true, where the miner already
  knows whether it is draining.
- **Web-side gate** — `internal/web` cannot observe generation liveness; any web-side check is
  necessarily a *narrowing*, not a closure. `lifecycleMutationBlocked`'s own doc already concedes this
  ("UX sugar only").

## Implementation units

### U1 — `internal/miner`: one named fence helper

Add a single greppable helper so the incantation has one name and one definition, rather than four
hand-copied `if !m.beginApply()` sites:

```go
// fenced admits fn as a configuration mutation on THIS generation, refusing
// with ErrShuttingDown once the generation is no longer the authoritative
// mutable one.
func (m *Miner) fenced(fn func() error) error {
    if !m.beginApply() { return ErrShuttingDown }
    defer m.endApply()
    return fn()
}
```

### U2 — `internal/miner`: fence the four mutators

- `ApplyCampaignPolicy(mode string) error` — was void
- `SetDropRule(rewardKey string, rule config.DropRule) error` — was void
- `ApplyHealthSettings(s config.HealthSettings) error` — signature unchanged
- `SetAutoRedeem(username string, cfg config.AutoRedeemConfig) error` — signature unchanged

The fence goes **first**, before each method's own validation, so a retired generation answers a
retryable refusal rather than a misleading permanent validation error — `SetAutoRedeem`'s roster guard
would otherwise report a permanent "not tracked" for what is really a retryable lifecycle refusal, on a
roster that is frozen precisely because the generation is dead. `SetDropRule`'s empty-key early return
sits **behind** the fence for the same reason: an empty key is a genuine no-op with nothing to refuse,
but ordering it first would let a retired generation answer a mutation endpoint with success.

### U3 — `internal/web`: widen `PolicyProvider`, map errors

- `PolicyProvider.ApplyCampaignPolicy` / `SetDropRule` gain `error` returns, making the interface
  consistent with its two siblings that already return `error`.
- `handleAPIPolicyMode` / `handleAPIPolicyDropRule` surface the refusal instead of rendering success.
- `handleAPIHealthSettings` currently maps *every* error to 500; a drain refusal must be 503.
- `handleAutoRedeem` currently maps every error to 400; a drain refusal must be 503, while genuine
  validation errors stay 400.

Add one shared classifier next to the other response helpers in `internal/web/responses.go` (the
neutral owner — `writeApplyError` lives in the page-scoped `handlers_settings.go`), and make
`writeApplyError` use it rather than duplicating the ladder. Each surface keeps its own
domain-appropriate message; the Drops controls must not inherit `"Settings could not be applied"`.

The two refusing states are reported differently, because they are different:

| State | Meaning | Response |
|---|---|---|
| Paused / stopped / mid-transition | lifecycle **conflict** — the operator resumes to clear it | **409**, localized RU/EN via the existing `writeSettingsConflict` |
| Replacement/startup gap, generation retired | transient **unavailability** — retry is safe | **503** via `ErrShuttingDown` |

The 409 check (`lifecycleMutationBlocked`) is UX sugar on these four routes exactly as it already is on
the settings routes; the miner-side fence stays the authoritative backstop for the check→mutate race.

### U4 — Correct the two test assertions that pinned the defect

`internal/miner/provider_safety_test.go` currently encodes the bug as correct:

- `ApplyHealthSettings after teardown = nil` (`:222-228`) — asserts the unfenced behaviour and **will
  fail** under the fence. Invert it, with the justification stated in the test.
- `SetAutoRedeem` untracked-streamer case (`:203-207`) — post-fence it still passes but for the
  *wrong reason* (the fence, not the roster guard): a **vacuous green**. Repair it so the roster guard
  is still genuinely exercised.

Both are deliberate, stated corrections of assertions that pinned a defect — not test weakening.

### U5 — Update the doc sites the change falsifies

`internal/settings/dto.go:8-16`, `internal/miner/miner.go:288-300`, `miner.go:1909-1913`,
`miner.go:2036-2053`, `internal/app/app.go:664-697`, and `provider_safety_test.go:71-74` all describe
the interlock as scoped to "settings applies". Add a short note at the new call sites recording that
this is **drain admission only** and is *not* serialization against `applySettings` (see the residual
at `miner.go:2145-2153`).

## Out of scope — reported, deliberately not fixed

1. **Fail-open `persistLocked`** (`policy.go:220-226`) — logs a `SaveConfig` failure and keeps the
   change live. Separate transactional-persistence root cause; explicitly excluded by contract.
2. **`RedeemCustomReward` on a retired generation** — the *same* sampled provider variable
   (`handlers_rewards.go:49-52`) serves both the fenced `SetAutoRedeem` and the unfenced
   `RedeemCustomReward`. `teardown` never closes `m.client`, so this is a real, irreversible Twitch
   channel-points spend, not a no-op. **Twitch-side external action, not a configuration mutation** —
   fencing it is a product decision (may a retiring generation finish an in-flight spend?).
3. **`applySettings` clone-window lost update** (`miner.go:2145-2153`) — a concurrent
   `ApplyCampaignPolicy`/`ApplyHealthSettings` landing inside an apply's unlocked window is reverted.
   Repo-documented, pre-existing, on a *live* generation; unchanged by this pass.
4. **`POST /api/notifications/config`** — same sample-then-call shape on a per-generation
   `notifications.Manager`, but it persists to the **shared process-level SQLite DB**, not
   `config.json`. Different mutation class; no whole-document revert hazard.
5. **Nil-provider mutation answers 200.** Before *any* generation has reached `setupComponents`,
   `s.policyProvider` / `s.healthProvider` are nil and both routes render their partial with a 200,
   acknowledging a mutation that did not happen. This is pre-existing, is a different condition from
   the stale-generation defect, and is **deliberately pinned as intended behaviour** by
   `TestHandleAPIHealthSettingsNilProviderRendersPartial` ("unchanged nil-provider behavior"). An
   earlier draft of this change converted it to 503; that was reverted as scope creep — changing it is
   a product decision for the owner, not a side effect of this fix.
6. **The 503 refusal is invisible on the Drops page.** htmx does not swap on a non-2xx response and the
   dashboard registers no global `htmx:responseError` handler, so a refused policy change produces no
   DOM feedback and the mode `<select>` keeps showing the value the server rejected. Nothing false is
   *rendered* (no success partial is swapped in) and no state diverges, so the fence's correctness goal
   holds — but the operator gets no explanation. Fixing it is template/JS work, which this contract
   forbids.
7. **Torn provider set during N+1 startup** — rewards registers at `miner.go:815/839`, health/policy
   at `:1069/1071`. Under the fence this is non-corrupting (the stale half refuses), but rewards
   recovers earlier than policy/health by an unbounded amount.

## Acceptance

A1-A3 policy repro red→green · A5-A8 all four fenced · A4 visible failure during the gap · A9 normal
mutation resumes on N+1 · A10 acknowledged-before-retirement survives into the handoff · A11 refused
changes neither memory nor disk · A12 never two accepting generations · A13 `-race` clean.

## Contract verification: the integrated seam (corrective pass)

The owner's contract required the RED reproduction to be **one** test containing all of: a
process-level App/web `Server`; a **real `lifecycle.Controller` generation transition**; generation N
registering providers through the real `setupComponents`; N retiring; **N+1 deterministically held
short of provider registration**; an HTTP configuration mutation in that gap; RED on the pinned base;
GREEN after the fix.

The first pass did **not** satisfy that literally, and the two tests it offered are not jointly
equivalent to it:

- `stale_generation_fence_test.go` never imports `internal/lifecycle`, so no real controller-driven
  replacement occurs; `TestOnlyOneGenerationAcceptsMutationAtATime` does build two generations, but
  both reach `setupComponents` — that is the *post-gap* state, not the gap.
- `internal/app`'s `genHarness` does drive a real controller, but its factory wraps the real miner in
  `ctrlFakeRunner`, so real `Miner.Run` — and therefore real provider registration — never executes.

`TestControllerDrivenReplacementGapRefusesStaleMutation`
(`internal/miner/lifecycle_replacement_gap_test.go`) closes that gap. It drives a real
`lifecycle.Controller` whose `Factory` returns real `*Miner` generations over one process-level
`web.Server`, uses `ctrl.Restart` for a genuine replacement, and parks generation N+1 inside its
authenticate stage — which `Run` reaches *before* `setupComponents` — on a channel, never a sleep.
`runRestart` (`internal/lifecycle/worker.go:769`) cancels N and **awaits its `Run` return** before
calling `Factory` for N+1, so the window the test holds open is the production window exactly.

Verified RED against a pristine checkout of base `5266736` (only test helpers ported; **zero** tracked
production files modified): the in-gap `POST /api/policy/mode` answered **200 OK**, moved the retired
generation's `CampaignPolicy` to `SMART`, and rewrote `config.json` to `SMART`. GREEN on this branch,
20/20 under `-race`.

### Recorded deviation: the `App` element

One element of requirement (1) is **not** met and is not silently claimed: the test uses the real
process-level `web.Server`, but not the literal `app.App`. This is a hard language constraint, proven
rather than asserted:

- `internal/app` imports `internal/miner`, so a `package miner` test importing `internal/app` fails to
  compile — `import cycle not allowed in test` (verified with a throwaway probe).
- `app.minerFactory` is unexported, so no test outside `package app` can build generations through
  App's real factory.
- `Miner.authenticateFn` / `startMiningFn` are unexported with no `export_test.go` and no exported
  setter, so `package app` cannot park a generation short of `setupComponents`.

Both halves of the required stub set are therefore private to *different* packages, and no single test
package can hold both. Closing this last element would require adding an exported, production-only
testing API to `internal/miner` — which the contract forbids.

What `App` contributes to a replacement beyond the controller is two things, and the test reproduces
one of them directly rather than assuming it away:

- **`web.Server.SetLifecycleController`** (`internal/app/app.go:480`) — load-bearing for this route,
  because `handleAPIPolicyMode` consults `lifecycleMutationBlocked()` *before* it samples the provider.
  The test wires the real controller into the real server itself, so the handler runs its real gate.
  That gate is **open** at the moment under test: `*Miner` does not implement
  `lifecycle.ReadySignaler`, so the worker marks a generation ready the instant its goroutine launches
  — observed returns to `running` and `Transition` to `TransitionNone` while N+1 is still short of
  `setupComponents`. Verified on base: the in-gap request is answered **200 OK**, not 409, so the 409
  gate demonstrably does not cover this window and the miner-side fence is the only backstop. The test
  asserts `!= 409` explicitly so it can never pass by being refused upstream of the fence.
- **The generation config handoff**, already covered on its own by
  `internal/app/generation_config_test.go` (PR #200).
