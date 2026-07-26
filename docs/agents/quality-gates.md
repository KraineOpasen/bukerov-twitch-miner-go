# Quality gates

Four gates, increasing in scope. A failed gate is an operation-mode expiry trigger (see
`docs/agents/operation-modes.md`) — stop and report rather than pushing through.

## Q0 — Compiles / parses

The change is syntactically valid. For Go: `go build ./...` and `go vet ./...` on the touched packages. For
config/data files: the relevant parser succeeds (`python3 -m json.tool`, YAML frontmatter round-trips, etc.).

## Q1 — Targeted tests

Tests for the touched package(s) pass: `go test -v -race ./internal/<pkg>/...`. For non-Go governance/tooling
changes, the equivalent self-test passes (e.g. `python3 .claude/hooks/governance-policy.py --self-test`,
`python3 scripts/validate-agent-governance.py`).

## Q2 — Full regression

The whole module's test suite passes with the race detector: `go test -v -race ./...`, plus `make lint`.

## Q3 — Review

A `code-review`-style pass (Standards + Spec axes, read-only, findings reported not auto-fixed) before a
change moves to PUBLISH_DRAFT.

## This task (governance v2 config + vendored skills)

This change-set touches only `CLAUDE.md`, `.claude/**`, `CONTEXT.md`, `docs/agents/**`, `docs/adr/**`,
`scripts/validate-agent-governance.py`, and an append-only `.gitignore` edit — no `internal/**`, `cmd/**`,
`go.mod`, or `go.sum`. Its Q0/Q1 are: `python3 -m json.tool` on every JSON file, the hook self-test, and the
validator script. **Current quality tier: Q0 + confirmed Go regression** (race detection run as a confirmatory
extra, not a hard Q0 requirement for a non-Go change-set). Even though no Go source changed, the Go regression
was actually run — not assumed — to confirm this change-set didn't somehow disturb the build:

- `go mod verify` — `all modules verified`, exit 0.
- `go vet ./...` — clean, exit 0, no output.
- `go build ./...` — exit 0, no output.
- `go test -count=1 ./...` — 34 packages `ok` (2 packages report `[no test files]`: `internal/constants`,
  `internal/version`), 0 failures, exit 0.
- `go test -race -count=1 ./...` — same 34 packages `ok`, no data races reported, exit 0. (Race detection is
  optional for a Q0-tier change since it's a governance/doc change-set, not a Go change — it was run anyway as
  a stronger confirmation than the minimum this tier requires.)

Mutation testing is not applicable to Markdown/governance content and is not required here.
