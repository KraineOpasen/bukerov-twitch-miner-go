# ADR-0001: Agent governance v2

- **Status**: Accepted
- **Date**: 2026-07-26

## Context

Agent-assisted development on this repo (Claude Code sessions, potentially several agents in one task) needed
explicit guardrails: a default-safe operating posture, a way to grant more capability deliberately and
narrowly, mechanical enforcement that doesn't rely on an agent remembering the rules, and a reviewed set of
third-party skills (Matt Pocock's `mattpocock/skills`) rather than ad hoc prompting.

Without this, agents could plausibly commit or push on `main`, mutate the GitHub issue tracker without being
asked, or pull in unreviewed third-party skill instructions that assume capabilities (auto-commit, auto-publish
to a tracker) this project doesn't want granted by default.

## Decision

Adopt governance v2:

- **Operation modes** (`READ_ONLY` / `PROTOTYPE` / `CHANGE` / `PUBLISH_DRAFT`) gate what a session may do,
  documented in `docs/agents/operation-modes.md`.
- **Task contract** envelope (`docs/agents/task-contract.md`) is the only way to escalate past `READ_ONLY`, and
  can never grant merge/release/deploy/production access.
- **Quality gates** Q0–Q3 (`docs/agents/quality-gates.md`) define what "done" means at each stage.
- **Mechanical enforcement** via `.claude/settings.json` permissions and the `.claude/hooks/governance-policy.py`
  PreToolUse hook, which fails closed on ambiguous mutating commands.
- **Vendored, audited skills**: 21 of Matt Pocock's 22 promoted skills, copied into `.claude/skills/` with
  minimal, marked local patches (see `docs/agents/mattpocock-skills-policy.md`) instead of trusting upstream
  skill instructions verbatim or auto-updating them.
- **Issue tracker / domain doc conventions** documented directly (`docs/agents/issue-tracker.md`,
  `domain.md`, `triage-labels.md`) rather than delegated to the excluded `setup-matt-pocock-skills` skill,
  which otherwise would have had standing permission to rewrite this repo's `CLAUDE.md`.

## Consequences

- Agents default to read-only; doing more requires an explicit, narrowly-scoped contract.
- A hook blocks known-dangerous command shapes (force push, push to main, `gh` mutations, etc.) even if a
  session's reasoning goes wrong — defense in depth alongside the policy documents.
- Vendored skills lag upstream until a human reviews and re-vendors; no automatic updates (see
  `docs/agents/mattpocock-skills-policy.md`).
- Future skill or policy changes go through the same review discipline: minimal, marked patches, not silent
  edits to vendored content.

## Links

- `CLAUDE.md` — `## Claude Code Governance (v2)` section
- `docs/agents/operation-modes.md`, `task-contract.md`, `quality-gates.md`
- `docs/agents/mattpocock-skills-policy.md`, `mattpocock-skills-manifest.json`, `mattpocock-skills-patches.md`
- `.claude/settings.json`, `.claude/hooks/governance-policy.py`
