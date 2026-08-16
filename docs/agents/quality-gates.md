# Quality gates

Four gates, increasing in scope. The contract's `quality_tier` names which one this task must clear before it
is considered done (see `docs/agents/task-contract.md`).

## Development feedback vs. final gates

Two different things, deliberately not conflated:

**A) Development feedback** — a red test, a compiler or build error found while debugging, an expected TDD red
state, a review finding, a mutation-testing survivor, a lint hit. This is the normal signal of engineering in
progress. Diagnose it, fix it, rerun it, **inside the same active task**, using whatever repair loop the
invoked skill designs (see `docs/agents/agent-orchestration.md`). It is not an authority failure, it does not
expire the contract, and it does not require asking the owner to approve an ordinary engineering decision.

**B) Final acceptance gates** — the run of the contract's `quality_tier` against the integrated tree that a
commit, push, or Draft PR is justified by. These must actually pass, on the real tree, before publication.

Two rules hold absolutely, in both cases:

- **A failure is never reported as a pass.** Not "effectively passing", not "passing apart from", not a
  summary that omits the failure. If it failed, say it failed and show the output.
- **Tests are never weakened to reach green.** No deleting, skipping, loosening, or narrowing an assertion to
  make a gate pass. If a test is genuinely wrong, fixing it is a deliberate, stated change with its own
  justification — not a quiet edit on the way to green.

When a skill's repair strategy is exhausted and a final gate still cannot be made to pass honestly, that is an
integrity failure: stop, drop to `READ_ONLY`, and report exact evidence (see
`docs/agents/operation-modes.md`).

## Q0 — Compiles / parses

The change is syntactically valid. For Go: `go build ./...` and `go vet ./...` on the touched packages. For
config/data files: the relevant parser succeeds (`python3 -m json.tool`, YAML frontmatter round-trips, etc.).

## Q1 — Targeted tests

Tests for the touched package(s) pass: `go test -v -race ./internal/<pkg>/...`. For non-Go governance/tooling
changes, the equivalent self-test passes (e.g. `python3 .claude/hooks/governance-policy.py --self-test`,
`python3 scripts/validate-agent-governance.py --self-test`, `python3 scripts/validate-agent-governance.py`).

## Q2 — Full regression

The whole module's test suite passes with the race detector: `go test -v -race ./...`, plus `make lint`.

## Q3 — Review

A `code-review`-style pass (Standards + Spec axes) before a change moves to PUBLISH_DRAFT. Review findings are
development feedback (case A): the task may fix them and re-review within the same session. What the gate
requires is that the *final* state has been reviewed and its findings are resolved or explicitly, honestly
recorded as accepted — not that the first review round came back clean.

## Final-gate evidence

Evidence for a final gate must be **real and reproducible**: the exact command, its actual output or exit
code, run against the integrated tree at the SHA being published. Not "should pass", not a recollection from
an earlier state of the tree, not a gate run only inside one lane of a parallel workflow. If a gate was not
run, say it was not run — an unrun gate is reported as unrun, never as passed.
