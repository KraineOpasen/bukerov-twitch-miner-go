# Skills routing

Which installed skill to reach for, by kind of work. This document maps **actually installed** skills — every
name below is a directory under `.claude/skills/` claimed by exactly one ownership manifest. If a skill is not
listed here, it is not installed; see each provider's manifest for what was reviewed and rejected, and why.

Routing is advice, not authority. Invoking a skill authorizes the agent topology that skill documents (see
`docs/agents/agent-orchestration.md`), but never widens the task contract's authority envelope
(`docs/agents/task-contract.md`).

## Providers

| Provider | Ownership | Policy | Manifest | Patch ledger |
| --- | --- | --- | --- | --- |
| [mattpocock/skills](https://github.com/mattpocock/skills) | vendored | [policy](mattpocock-skills-policy.md) | [manifest](mattpocock-skills-manifest.json) | [ledger](mattpocock-skills-patches.md) |
| [anthropics/skills](https://github.com/anthropics/skills) | vendored | [policy](anthropic-skills-policy.md) | [manifest](anthropic-skills-manifest.json) | [ledger](anthropic-skills-patches.md) |
| project-owned first-party | first-party | [policy](project-skills-policy.md) | [manifest](project-skills-manifest.json) | n/a |

Exact upstream pins live in each manifest's `upstream_commit`. No provider auto-updates
(`automatic_updates: false`, enforced by `provider-manifest-fields`).

## Routing table

### Bug / root cause

| Skill | Use it for |
| --- | --- |
| `diagnosing-bugs` | Owns the diagnosis loop for anything broken, throwing, failing, or slow. Start here, not with a fix. |
| `wait-what` | A result that contradicts what the code should do — surfacing the wrong assumption before debugging further. |
| `webapp-testing` | Browser-side evidence for a dashboard bug. Supplies evidence *to* `diagnosing-bugs`; does not own the loop. |

### Architecture / spec / planning

| Skill | Use it for |
| --- | --- |
| `wayfinder` | Finding where in the codebase a change belongs before designing it. |
| `codebase-design` | Deep-module vocabulary: interfaces, seams, testability, what a package should hide. |
| `improve-codebase-architecture` | Reworking an existing module's structure rather than designing a new one. |
| `to-spec` | Turning a decision into an implementable specification. |
| `to-tickets` | Splitting agreed work into separately shippable units. |
| `grilling` / `grill-me` / `grill-with-docs` | Stress-testing a plan or decision before committing to it; `grill-with-docs` grounds the interrogation in primary sources. |
| `domain-modeling` | `CONTEXT.md` terminology and ADRs under `docs/adr/`. |
| `research` | Primary-source investigation captured as a Markdown file in the repo. |

### Implementation

| Skill | Use it for |
| --- | --- |
| `implement` | Executing an agreed plan or spec. |
| `tdd` | Red-green-refactor when the behavior is testable first. |
| `prototype` | A throwaway build to answer a design question — including "what should this UI be". |
| `resolving-merge-conflicts` | Reconciling a conflicted merge or rebase. |

### Concurrency / lifecycle

No skill is dedicated to this lane yet. It is the project's highest-value gap: the miner's watcher, PubSub
pool, IRC client, and drops sync are all `context.Context`-driven long-running loops, and their failure modes
(missed cancellation, goroutine leaks, reconnect generation races) are the ones `go test -race` catches late.
For now: `diagnosing-bugs` for a live symptom, `codebase-design` for the seam, `tdd` for the regression test.

### Security / static analysis

| Skill | Use it for |
| --- | --- |
| `code-review` (Standards axis) | Reviewing a change against this repo's documented conventions. |

The Claude Code built-in `/security-review` covers pending-change security review. This lane is otherwise
thin — see "Known gaps" below.

### Testing / fuzzing / properties / mutations

| Skill | Use it for |
| --- | --- |
| `tdd` | Writing the test before the behavior; integration tests. |
| `webapp-testing` | Playwright against a locally running dashboard. Local targets only; not a replacement for `go test`. |

Go's native `go test -fuzz` and `testing/quick` are the project's fuzz/property tools; no installed skill
wraps them yet.

### PR / CI / review

| Skill | Use it for |
| --- | --- |
| `code-review` | Reviewing changes since a fixed point along Standards and Spec axes, in parallel sub-agents. |
| `triage` | Deciding what an incoming issue or report actually is. |

**The owner performs merges.** No skill may mark a PR ready for review, merge, auto-merge, release, tag,
deploy, or trigger/rerun a workflow — these are non-delegable at every delegation depth (`CLAUDE.md`).

### Durable knowledge / handoff

| Skill | Use it for |
| --- | --- |
| `handoff` | Passing in-progress work to the next session with enough context to continue. |
| `domain-modeling` | Recording terminology and decisions so they survive the session. |
| `writing-for-agents` | Writing instructions another agent will have to follow. |
| `teach` | Explaining a subsystem to a human reader. |
| `ask-matt` / `wizard` | Meta-guidance on using and composing the vendored skill set. |

### Dashboard design

| Skill | Use it for |
| --- | --- |
| `frontend-design` | Aesthetic direction for the existing Go `html/template` + Tailwind + HTMX + ApexCharts UI. Scoped to visual/UI work only — not backend, not multi-variant exploration, not chart colors. |
| `prototype` | Exploring what a dashboard surface should look like before committing. |

The Claude Code built-in `dataviz` skill owns chart-colour and data-visualization conventions.

### Dashboard implementation

| Skill | Use it for |
| --- | --- |
| `implement` | Building an agreed dashboard change. |
| `tdd` | The `internal/web` package has real rendered-output tests; extend them rather than asserting by eye. |
| `codebase-design` | Where a new handler, view model, or partial belongs. |

### Browser / a11y / responsive / visual QA

| Skill | Use it for |
| --- | --- |
| `webapp-testing` | Driving a local dashboard with Playwright: interaction, screenshots, console logs. |

Accessibility and responsive verification currently have no dedicated skill — see "Known gaps".

## Known gaps

Recorded honestly so the next review knows what to look for, rather than implying the stack is complete:

- **Concurrency / lifecycle** — no dedicated skill.
- **Security / static analysis** — only the generic review axis and the Claude Code built-in.
- **Fuzzing, property-based testing, mutation testing** — no dedicated skill.
- **Accessibility / responsive QA** — no dedicated skill; `webapp-testing` supplies the browser, not the criteria.
- **Supply-chain / provenance review** — performed by hand during vendoring; no installed skill.

## Next product stage: Dashboard Stage 1

The next main product stage is **Dashboard Stage 1 — actual web-interface implementation work**, built on the
already-approved dashboard sources:

- `docs/dashboard/stage-3-wireframes-and-interactions.md`
- `docs/dashboard/stage-4-visual-design-system.md`

Those two documents are **canonical and must not be edited** as a side effect of skill or governance work.
Stage 1 implementation touches `internal/web/**` (templates, handlers, view models, Tailwind input CSS) and
requires its own task contract — governance tasks such as this one explicitly forbid application paths.

Routing for that stage: `frontend-design` for visual direction, `prototype` for exploration, `wayfinder` +
`codebase-design` to place the change, `implement` and `tdd` to build it, `webapp-testing` for browser
evidence, `code-review` before publication.
