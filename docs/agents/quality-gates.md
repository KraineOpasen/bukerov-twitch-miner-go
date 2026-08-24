# Quality gates

Four gates, increasing in scope — the repo-native elaboration of `GOVERNANCE_V3.md` §12. A finding from
any gate is development feedback: diagnose and repair inside the same active task, rerun, and repeat until
the gate honestly passes — a failure is never reported as a pass, and a test is never weakened, skipped,
or narrowed to reach green. Only a repair strategy exhausted without an honestly passing final gate — an
integrity/authority failure, not an ordinary red test — drops the session to `READ_ONLY` (see
`GOVERNANCE_V3.md` §5 and `docs/agents/operation-modes.md`).

## Q0 — Compiles / parses

The change is syntactically valid. For Go: `go build ./...` and `go vet ./...` on the touched packages. For
config/data files: the relevant parser succeeds (`python3 -m json.tool`, YAML frontmatter round-trips, etc.).

## Q1 — Targeted tests

Tests for the touched package(s) pass: `go test -v -race ./internal/<pkg>/...`. For non-Go governance/tooling
changes, the equivalent self-test passes (e.g. `python3 .claude/hooks/governance-policy.py --self-test`,
`python3 scripts/validate-agent-governance.py`).

## Q2 — Full regression

Runs on the final candidate only, on the integrated tree, at the SHA being published (`GOVERNANCE_V3.md`
§12): the whole module's test suite with the race detector — `TZ=UTC go test -race -count=1 ./...` (the
final-gate form of the everyday `go test -v -race ./...` from `CLAUDE.md`/`.claude/rules/tests.md`, made
deterministic with `-count=1` and a pinned timezone) — plus `go mod verify`, `go vet ./...`,
`go build ./...`, `make lint`, and proof that only the intended paths changed. Development iteration does
not re-run this full gate.

## Q3 — Review

Review axes per `GOVERNANCE_V3.md` §12 — Standards; Spec/domain compliance; differential/caller impact;
security/concurrency; provenance; browser/a11y when UI is touched — run independently (a
`code-review`-style pass covers the first two), read-only, findings reported not auto-fixed, before a
change moves to PUBLISH_DRAFT.

## Governance/tooling change-sets

For a change-set that touches only the governance layer (`CLAUDE.md`, `GOVERNANCE_V3.md`, `.claude/**`,
`CONTEXT.md`, `docs/agents/**`, `docs/adr/**`, `scripts/validate-agent-governance.py`) and no application
paths, Q0/Q1 are: `python3 -m json.tool` on every touched JSON file, the hook self-test
(`python3 .claude/hooks/governance-policy.py --self-test`), and the governance validator with its own
fixture matrix (`python3 scripts/validate-agent-governance.py` and `--self-test`). The heavy Go gates
apply only where changed-path analysis shows application paths are affected; mutation testing is not
applicable to Markdown/governance content.
