# Skills routing

Which installed skill to reach for, by kind of work. Every name below is a directory under
`.claude/skills/` claimed by exactly one ownership manifest — **81 installed skills across six
providers**, and all 81 are routed somewhere below, so if a skill is not listed here it is not
installed. Each provider's manifest records what was reviewed and rejected, with a reason and an
`EXCLUDE`/`HOLD` verdict for every candidate it reviewed individually — with two deliberately
sweep-scoped exceptions: `trailofbits/skills` rules out a further 31 upstream skill directories at
whole-plugin granularity rather than one by one, and `github/awesome-copilot`'s 408-directory corpus
was swept by name and frontmatter description rather than read end to end.

Routing is advice, not authority. Invoking a skill authorizes the agent topology that skill
documents (`docs/agents/agent-orchestration.md`), but never widens the task contract's authority
envelope (`docs/agents/task-contract.md`).

## Providers

| Provider | Skills | Licence | Policy · Manifest · Ledger |
| --- | --: | --- | --- |
| [mattpocock/skills](https://github.com/mattpocock/skills) | 23 | MIT | [policy](mattpocock-skills-policy.md) · [manifest](mattpocock-skills-manifest.json) · [ledger](mattpocock-skills-patches.md) |
| [anthropics/skills](https://github.com/anthropics/skills) | 3 | Apache-2.0 | [policy](anthropic-skills-policy.md) · [manifest](anthropic-skills-manifest.json) · [ledger](anthropic-skills-patches.md) |
| [EveryInc/compound-engineering-plugin](https://github.com/EveryInc/compound-engineering-plugin) | 22 | MIT | [policy](compound-engineering-skills-policy.md) · [manifest](compound-engineering-skills-manifest.json) · [ledger](compound-engineering-skills-patches.md) |
| [trailofbits/skills](https://github.com/trailofbits/skills) | 23 | CC BY-SA 4.0 | [policy](trailofbits-skills-policy.md) · [manifest](trailofbits-skills-manifest.json) · [ledger](trailofbits-skills-patches.md) |
| [github/awesome-copilot](https://github.com/github/awesome-copilot) | 5 | MIT | [policy](awesome-copilot-skills-policy.md) · [manifest](awesome-copilot-skills-manifest.json) · [ledger](awesome-copilot-skills-patches.md) |
| [BuilderIO/skills](https://github.com/BuilderIO/skills) | 5 | MIT | [policy](builderio-skills-policy.md) · [manifest](builderio-skills-manifest.json) · [ledger](builderio-skills-patches.md) |
| project-owned first-party | 0 | — | [policy](project-skills-policy.md) · [manifest](project-skills-manifest.json) |

Exact upstream pins are each manifest's `upstream_commit`. No provider auto-updates
(`automatic_updates: false`, mechanically enforced by `provider-manifest-fields`).

## Routing table

### Bug / root cause

| Skill | Use it for |
| --- | --- |
| `diagnosing-bugs` | **Owns the diagnosis loop.** Anything broken, throwing, failing or slow starts here. |
| `ce-debug` | A second, differently-shaped root-cause loop: explicit causal chain, pipeline mode for CI failures. Use when `diagnosing-bugs` has stalled or the bug arrived from CI. |
| `fp-check` | You have a *claimed* bug and need a TRUE/FALSE POSITIVE verdict with evidence. The anti-hallucination gate before acting on a finding. |
| `variant-analysis` | A bug is confirmed — where else does it live? |
| `trailmark-variant-neighborhood` | Seeds that hunt from the code graph: siblings, shared callers and callees, common sinks and entry-point paths around the confirmed bug, handed to `variant-analysis` or `semgrep-rule-creator` as candidate locations. |
| `trailmark-finding-triage` | Is this one finding actually reachable from an entry point? |
| `wait-what` | A result contradicts what the code should do; surface the wrong assumption first. |
| `webapp-testing` | Browser-side evidence for a dashboard bug. Supplies evidence *to* the diagnosis loop; does not own it. |

### Architecture / spec / planning

| Skill | Use it for |
| --- | --- |
| `wayfinder` | Work too big for one session — a shared map of decisions. |
| `codebase-design` | Deep-module vocabulary: interfaces, seams, what a package should hide. |
| `improve-codebase-architecture` | Reworking an existing module's structure. |
| `ce-plan` / `ce-brainstorm` / `ce-ideate` | Implementation-ready plan / requirements-only scoping / idea generation before a direction exists. |
| `ce-pov` | A decisive verdict on adopting a specific external option. |
| `plan-arbiter` | Two or more competing plans exist; pick or merge one. |
| `to-spec` / `to-tickets` | Decision → specification → separately shippable units. |
| `ce-doc-review` | Review an existing plan/spec/requirements document with role-specific lenses. |
| `grilling` / `grill-me` / `grill-with-docs` | Stress-test the thinking before committing; `grill-with-docs` grounds it in primary sources. |
| `audit-context-building` | Per-function invariants, assumptions and callees recorded as evidence before any change or hunt. |
| `spec-to-code-compliance` | Does the code match `SPECIFICATIONS.md`? Which requirements hold, contradict, or are absent. |
| `trailmark` / `diagramming-code` / `graph-evolution` | Build a code graph; draw it; diff it between two refs. |
| `threat-model-analyst` | STRIDE-A threat model with DFDs and trust boundaries. |
| `domain-modeling` | `CONTEXT.md` terminology and ADRs under `docs/adr/`. |
| `research` / `read-the-damn-docs` | Primary-source investigation captured as a repo file / forcing current official docs before assuming from memory. |
| `ce-strategy` | `STRATEGY.md` — product direction upstream of planning. |

### Implementation

| Skill | Use it for |
| --- | --- |
| `implement` | Execute an agreed plan or spec. |
| `ce-work` | Execute a plan unit-by-unit with evidence-first discipline and its own verification tail. |
| `tdd` | Red-green-refactor; integration tests. |
| `ce-simplify-code` | After implementation, before review: reuse/quality/efficiency passes over the branch diff. |
| `prototype` / `ce-prototype` | Throwaway build to answer a design question. |
| `ce-worktree` | Isolate work in a git worktree before starting. |
| `ce-commit` | A commit with a value-communicating message. No push, no PR. |
| `resolving-merge-conflicts` | An in-progress merge/rebase conflict. |
| `plow-ahead` | The user explicitly wants autonomous progress: convert ambiguity into stated assumptions rather than stopping. |
| `efficient-frontier` | Delegate research/coding/testing to cheaper subagents, keep planning and final review on the expensive model. |

### Concurrency / lifecycle

This was the stack's largest gap and is now partly served. The miner's watcher, PubSub pool, IRC
client and drops sync are all `context.Context`-driven long-running loops.

| Skill | Use it for |
| --- | --- |
| `sharp-edges` | `references/lang-go.md` is the most directly applicable file in the security set — silent integer overflow, footgun APIs, unsafe defaults in Go. |
| `audit-context-building` | Recording what a goroutine or lifecycle function actually assumes and guarantees. |
| `trailmark` | Call paths and blast radius across the miner's packages. |
| `property-based-testing` | Go support via `pgregory.net/rapid` — good fit for reconnect/backoff/state-machine invariants. |
| `diagnosing-bugs` / `ce-debug` | A live symptom (race, leak, missed cancellation). |

Still unserved: nothing models goroutine lifecycles or `context` cancellation specifically. Treat
`go test -race` as the primary detector.

### Security / static analysis

| Skill | Use it for |
| --- | --- |
| `semgrep` / `codeql` | Pattern and interprocedural taint scanning; Go is first-class in both. |
| `sarif-parsing` | Process, dedupe and fingerprint results from either. |
| `semgrep-rule-creator` | Author a custom rule, test-first. |
| `sharp-edges` | Error-prone APIs and unsafe-by-default configuration. |
| `differential-review` | Security-first review of a diff: attacker model, blast radius, regression archaeology. |
| `threat-model-analyst` | Whole-system STRIDE-A model. |
| `supply-chain-risk-auditor` | `go.mod`/`go.sum` advisories, abandoned upstreams, install-time scripts. |
| `vulnerability-triage-brocards` | Seven falsifiable dismissal tests applied *before* spending effort on a report. |
| `audit-augmentation` | Project SARIF findings onto the code graph. |
| `github-actions-hardening` | The Actions threat model for this repo's two live workflows. |

External scanners (`semgrep`, `codeql` CLIs) are a **runtime** capability. None is installed by this
change, and a skill is not blocked by a scanner's absence.

### Testing / fuzzing / properties / mutations

| Skill | Use it for |
| --- | --- |
| `tdd` | Test before behaviour. |
| `property-based-testing` | Invariants over examples; Go via `rapid`. |
| `mutation-testing` | Configure and scope a `mewt`/`muton` campaign (Go supported). |
| `genotoxic` | Triage survived mutants against the call graph — real gaps vs false positives. |
| `harness-writing` | Fuzz-target design; carries a Go section for `go test -fuzz`. |
| `webapp-testing` | Playwright against a locally running dashboard. Not a replacement for `go test`. |

### PR / CI / review

| Skill | Use it for |
| --- | --- |
| `code-review` | Standards + Spec axes, in parallel sub-agents. |
| `ce-code-review` | Risk-driven multi-persona review; report-only by default. |
| `differential-review` | The security axis of a diff. |
| `trailmark-review-gate` | Structural PASS/WARN/FAIL gate over a diff — new entry points, new tainted paths, removed validation. Advisory. |
| `ce-resolve-pr-feedback` | Address review comments and resolve threads. |
| `ce-babysit-pr` | Watch a PR over its life: CI failures, review comments, base movement. |
| `ce-commit-push-pr` | Commit, push, open a **draft** PR. |
| `agent-watchdog` | Audit what another agent actually changed and verified. |
| `github-actions-efficiency` | CI minutes and cost. |
| `triage` | Decide what an incoming issue or external PR actually is. |

**The owner performs merges.** No skill may mark a PR ready for review, merge, auto-merge,
release, tag, deploy, or trigger/rerun a workflow — non-delegable at every delegation depth
(`CLAUDE.md`). Where a vendored skill shipped such a capability it was removed by a recorded local
patch; see each provider's ledger.

### Durable knowledge / handoff

| Skill | Use it for |
| --- | --- |
| `handoff` / `ce-handoff` | Compact the session for the next agent / resume from a continuity source. |
| `ce-compound` | Turn a solved problem into a durable repo learning. |
| `ce-compound-refresh` | Audit those learnings against the current code so the store stays true. |
| `harness-engineering` | Turn repeated agent mistakes into durable instructions, drift checks and regression tests. |
| `domain-modeling` | Terminology and decisions that outlive the session. |
| `writing-for-agents` | Writing instructions another agent must follow. |
| `teach` / `ce-explain` | Explain a subsystem to a human / build a durable visual explainer. |
| `ask-matt` / `wizard` | Router over the skill set / generate a human-run bash wizard. |

### Dashboard design

| Skill | Use it for |
| --- | --- |
| `frontend-design` | Aesthetic direction for the Go `html/template` + Tailwind + HTMX + ApexCharts UI. Visual/UI only. |
| `web-design-reviewer` | The audit rubric the other two lack: WCAG contrast, 44×44 touch targets, focus order, `prefers-reduced-motion`, a four-viewport matrix (375/768/1280/1920), plus Tailwind-specific fixes. |
| `prototype` / `ce-prototype` | Explore what a surface should be before committing. |

The Claude Code built-in `dataviz` skill owns chart-colour and data-visualization conventions.

### Dashboard implementation

| Skill | Use it for |
| --- | --- |
| `implement` / `ce-work` | Build the agreed change. |
| `tdd` | `internal/web` has real rendered-output tests — extend them rather than asserting by eye. |
| `codebase-design` | Where a new handler, view model or partial belongs. |
| `ce-simplify-code` | Tidy the diff before review. |

### Browser / a11y / responsive / visual QA

| Skill | Use it for |
| --- | --- |
| `webapp-testing` | Drive a local dashboard with Playwright: interaction, screenshots, console logs. Local targets only. |
| `web-design-reviewer` | The criteria to judge what the browser shows — accessibility, responsive behaviour, visual consistency. Browser-tool-agnostic, so it composes with `webapp-testing`. |

### Meta

| Skill | Use it for |
| --- | --- |
| `skill-creator-anthropic` | Authoring/iterating skills. Explicit invocation only (`/skill-creator-anthropic`). |
| `ce-setup` | Check Compound Engineering health and repo-local config. |
| `ce-optimize` | Metric-driven experiment loops on a measurable outcome. |

## Known gaps

Recorded honestly so the next review knows what to look for:

- **Goroutine lifecycle / `context` cancellation** — no skill models this directly.
- **Twitch protocol specifics** (GraphQL persisted queries, PubSub topics, IRC, drops, bet logic) —
  no skill knows them; `SPECIFICATIONS.md` remains the authority, and `spec-to-code-compliance` is
  the closest mechanical check.
- **SQLite schema migrations** — no dedicated skill; `internal/database`'s per-module migration
  system is documented only in `CLAUDE.md` and the code.
- **Accessibility beyond a review rubric** — `web-design-reviewer` supplies criteria, but nothing
  runs an automated a11y audit.

## Next product stage: Dashboard Stage 1

The next main product stage is **Dashboard Stage 1 — actual web-interface implementation work**,
built on the already-approved dashboard sources:

- `docs/dashboard/stage-3-wireframes-and-interactions.md`
- `docs/dashboard/stage-4-visual-design-system.md`

Those two documents are **canonical and must not be edited** as a side effect of skill or governance
work. Stage 1 touches `internal/web/**` (templates, handlers, view models, Tailwind input CSS) and
needs its own task contract — governance tasks such as the one that installed this stack explicitly
forbid application paths.

Suggested route for that stage: `frontend-design` for visual direction → `prototype` to explore →
`wayfinder` + `codebase-design` to place the change → `implement`/`ce-work` + `tdd` to build it →
`webapp-testing` for browser evidence → `web-design-reviewer` for the a11y/responsive rubric →
`code-review` + `ce-code-review` before publication.
