# ADR-0002: Governance v3 — skill-native orchestration

- **Status**: Accepted
- **Date**: 2026-08-16
- **Supersedes**: `docs/adr/0001-agent-governance-v2.md` (the orchestration and failure-handling parts; the
  operation-mode / task-contract / mechanical-enforcement decisions carry forward)

## Context

Governance v2 (ADR-0001) established the right *authority* model: default READ_ONLY, an explicit task
contract to escalate, non-delegable prohibitions, and mechanical enforcement via `.claude/settings.json` and
the PreToolUse hook. That part worked.

What did not work was that v2 answered two unrelated questions from one linear precedence chain. The chain
ran: contract → `CLAUDE.md` + rules → vendored skills → upstream defaults → generic behavior. Because it was
one chain, repository policy sat *above* skill content on every axis — including axes that have nothing to do
with safety. Three concrete problems followed:

1. **Orchestration micromanagement.** `CLAUDE.md` mandated one production writer per task, an explicit role
   ledger, no recursive subagent spawning, an `agent_cap`, a `max_concurrency`, and reviewer agents that are
   always read-only. These are engineering-workflow decisions, and they overrode the internal design of every
   audited skill. A skill whose value *is* its topology — parallel reviewers, a critic lane, a writer per
   partition, an iterative repair loop — got flattened to a serial single-writer pipeline. We were paying to
   audit skills and then discarding the thing we audited.

2. **Patch pressure for the wrong reason.** The vendoring policies' narrowing lists included "agent count"
   and "unbounded subagent fan-out" alongside genuine authority concerns like commit, push, and tracker
   mutation. That framing invited patching upstream skills to fit an orchestration rule rather than a safety
   boundary, and it produced patches (`design-it-twice-cap`, `skc-agent-cap`, the `wayfinder` fan-out patch)
   whose only purpose was to enforce a cap the project did not actually need.

3. **Every failure was an authority failure.** v2 listed "a quality gate fails" as an operation-mode expiry
   trigger, and Q3 specified findings "reported not auto-fixed". Read literally, an expected TDD red state, a
   compile error found while debugging, or a routine review finding expired the contract and required owner
   intervention. That is not how engineering works, and it pushed sessions toward either stopping constantly
   or quietly reinterpreting the rule — the second of which is much worse.

Separately, the operating-system name of one particular server had leaked into governance vocabulary
(`CLAUDE.md`'s non-delegable prohibitions, the task-contract doc, the vendoring policies and patch ledgers) as
if it were an architectural concept. It is not: it is the host OS of one machine that happens to run the
miner's Docker container. Naming a host OS in coding governance is a category error that makes the policy look
deployment-specific when it isn't. This ADR deliberately does not repeat that name, so that the rule in §8 can
be enforced mechanically as "zero occurrences anywhere in the tree" with no document needing an exemption.

## Decision

Adopt Governance v3.

### 1. Separate authority from workflow

Two independent chains, replacing the single v2 precedence list.

**Authority** — exactly four levels, narrowing only (each layer may restrict, never widen): (1) owner/task
contract → (2) repository invariants, `CLAUDE.md` + `.claude/rules/*.md` (genuine repository safety and
integrity invariants only) → (3) invoked audited skill instructions, vendored and first-party at one tier →
(4) generic model behavior as fallback.

v2's chain had a fifth tier, "unpatched upstream skill defaults", sitting below the local patches. v3 drops it:
a vendored skill's instructions are its vendored bytes, patched and unpatched alike, all at level 3, with
patch-versus-upstream conflicts resolved inside that level. `docs/agents/agent-orchestration.md` carries the
canonical statement; every other document restates those same four levels.

**Workflow**: the invoked audited skill's documented methodology → task-prompt narrowing of scope/acceptance →
generic model behavior.

Stated as the governing principle: **owner controls authority; skills control engineering workflow; agents
inherit authority and never create or expand it.**

### 2. Skill-native orchestration is the default

Invoking an audited skill *is* the authorization for the agent topology that skill documents. Removed as
global requirements: `single_writer`, `agent_cap`, `max_concurrency`, the mandatory role ledger, the
no-recursive-spawning rule, and "reviewers must always be read-only" as a universal constraint. Prompts no
longer enumerate agents or roles as boilerplate.

A skill may spawn its documented agents, run parallel or sequential lanes, use multiple writers, run
reviewers/critics/verifiers, delegate recursively where its audited design calls for it, and run repair loops
— without per-spawn permission and without escalating routine engineering decisions.

`agent_cap` and `max_concurrency` survive as **optional** contract fields with no default, for tasks that
genuinely need a resource ceiling. This deliberately keeps existing vendored patch text that references "the
task contract's `agent_cap`" meaningful ("respect a cap if one is set") without requiring any change to
vendored skill bodies in this change-set.

An explicit opt-out exists for rare cases: `orchestration: main_context_only`. Absent the field,
`skill_native` applies.

### 3. Authority inheritance, not accumulation

Every child agent at any depth inherits the same envelope: same mode, repo, branch, allowed paths, allowed
operations. A parent may hand a child a *narrower* envelope. No child may widen mode, repo, allowed paths, or
publication authority, or invent authority the task does not carry. Four agents holding `CHANGE` do not sum to
`PUBLISH_DRAFT`.

### 4. Multiple writers, deterministic ownership

"Exactly one writer" is replaced by the invariant it was protecting: **no uncontrolled competing writes.**
Multiple writers are permitted when the orchestrating skill partitions ownership deterministically, avoids
simultaneous conflicting edits to the same file or region, and reconciles integration before the final gates
run. A skill whose value depends on safe parallel work is not artificially serialized; work that genuinely
overlaps with no ownership model still serializes.

### 5. Development feedback is not an authority failure

Failed builds, red tests, review findings, mutation survivors and other intermediate engineering signals are
diagnosed, fixed and rerun inside the same active task. Two absolutes remain: a failure is never reported as a
pass, and tests are never weakened to reach green. Final publication still requires the contract's final gates
to actually pass, with real reproducible evidence against the integrated tree.

`READ_ONLY` is reserved for integrity/authority failures: repo/base/branch drift, unexpected dirty or
conflicting state, operating outside granted authority, irreconcilable scope expansion, unprovable
environment state, or a skill exhausting its repair strategy without obtaining a valid final gate.

### 6. Minimal local patching of audited skills

Audited skills are preserved as close to their authors' intent as practical. A skill is **not** patched merely
because it uses subagents, several writers, reviewers/critics, parallel analysis, iterative fixes, or its own
handoff pattern. Patch only for concrete project incompatibility, a broken dependency, license/provenance
necessity, or a genuine authority/integrity boundary.

### 7. Merge stays owner-controlled

Unchanged from v2 and restated explicitly: the owner performs merges. Governance may permit task-branch work,
commits, pushes and Draft PR creation when the active contract grants them. No autonomous merge or
auto-merge, at any depth, under any contract.

### 8. No OS-specific governance

Every reference to the owner's host operating system is removed from every tracked file, including this ADR
itself and the vendored skill bodies. Where a generic concept is needed the wording is "runtime", "remote
runtime", "server runtime", "remote host", or "Docker container"; where a specific deployment target ships
real assets in this repository (the `unraid/` app template), naming that target in deployment documentation is
a product fact, not governance vocabulary, and stays. Remote runtime and container operations are a separate
task concern, raised only when the owner explicitly asks for runtime work; a host operating-system name is
never part of coding governance.

The acceptance condition is mechanical and absolute: the forbidden token occurs **zero** times across all
tracked files. No document — not this ADR, not the validator that enforces it — is exempt. The validator
therefore assembles the token from fragments at import time rather than embedding it as a literal, so that the
enforcement code cannot become the last surviving occurrence of the thing it forbids.

## Consequences

- Audited skills run their designed methodology. The audit is what makes a skill trustworthy, and its topology
  is part of what was audited — the project stops overriding it by default.
- Prompts get shorter and less brittle: no agent rosters, caps, or role ledgers as boilerplate.
- Parallel work becomes available, with a real correctness condition (deterministic ownership + reconciliation
  before gates) instead of a blanket prohibition.
- Sessions can debug and repair like engineers instead of halting on the first red test, while the two
  non-negotiables (never call a failure a pass; never weaken a test) carry the honesty guarantee that the
  old blanket rule was standing in for.
- The safety surface is unchanged: default READ_ONLY, contract-gated escalation, path scoping, non-delegable
  prohibitions, `.claude/settings.json` permissions and the PreToolUse hook all still apply, now to every
  agent at every depth explicitly.
- **Migration implication for future skill audits.** The audit question changes shape. It is no longer "does
  this skill spawn more agents than we allow?" but "does this skill's design stay inside the authority
  envelope, and does its ownership model prevent competing writes?" Existing patches created purely to enforce
  an orchestration cap (`design-it-twice-cap`, `skc-agent-cap`, `wayfinder`'s fan-out narrowing,
  `code-review`'s reviewer-subagent narrowing) are now candidates for removal at the next re-vendor — they are
  **not** touched by this change-set. Each needs its own reviewed PR against the vendoring policies' update
  procedure.
- The only vendored skill bytes this decision changes are the three host-OS mentions inside existing patch
  blocks (`fd-artifact-paths`, `webapp-testing-localhost-only`, `wizard-governance-gate`), required by §8. No
  patch is added or removed, no upstream text is touched, every patch id is unchanged, and each skill's
  `upstream_blob_sha` is preserved — only the `vendored_blob_sha` moves, with the patch ledger rows quoting the
  resulting text verbatim.
- ADR-0001's authority decisions (operation modes, task contract, quality gates, mechanical enforcement,
  vendored-skill discipline) remain in force; only its orchestration and failure-handling decisions are
  superseded.

## Links

- `CLAUDE.md` — `## Claude Code Governance (v3)` section
- `docs/agents/agent-orchestration.md` — full orchestration semantics
- `docs/agents/task-contract.md`, `operation-modes.md`, `quality-gates.md`
- `docs/agents/mattpocock-skills-policy.md`, `anthropic-skills-policy.md`, `project-skills-policy.md`
- `docs/adr/0001-agent-governance-v2.md` — superseded in part
- `scripts/validate-agent-governance.py` — mechanical consistency checks for the above
