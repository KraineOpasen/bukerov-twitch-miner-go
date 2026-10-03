# ADR-0001: Agent governance v2

- **Status**: Superseded / historical; retired by the minimal project kernel migration (2026-10-03).
- **Date**: 2026-07-26

This record describes the historical decision on the date above. Its authority, paths,
inventory and future plans are not current instructions. The owner-approved migration
on 2026-10-03 replaced this framework with [CLAUDE.md](../../CLAUDE.md) and native
[project settings](../../.claude/settings.json). Deleted-file names below refer to the
[pre-migration snapshot](https://github.com/KraineOpasen/bukerov-twitch-miner-go/tree/01c42876568deb6787ddcaaddaca6a8bf6b1f7f4);
immutable links preserve the former rationale, not an active dependency.

## Historical context

Agent-assisted development on this repo (Claude Code sessions, potentially several agents in one task) needed
explicit guardrails: a default-safe operating posture, a way to grant more capability deliberately and
narrowly, mechanical enforcement that doesn't rely on an agent remembering the rules, and a reviewed set of
third-party skills (Matt Pocock's `mattpocock/skills`) rather than ad hoc prompting.

Without this, agents could plausibly commit or push on `main`, mutate the GitHub issue tracker without being
asked, or pull in unreviewed third-party skill instructions that assume capabilities (auto-commit, auto-publish
to a tracker) this project doesn't want granted by default.

## Historical decision

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

## Historical consequences

- Agents default to read-only; doing more requires an explicit, narrowly-scoped contract.
- A hook blocks known-dangerous command shapes (force push, push to main, `gh` mutations, etc.) even if a
  session's reasoning goes wrong — defense in depth alongside the policy documents.
- Vendored skills lag upstream until a human reviews and re-vendors; no automatic updates (see
  `docs/agents/mattpocock-skills-policy.md`).
- Future skill or policy changes go through the same review discipline: minimal, marked patches, not silent
  edits to vendored content.

## Historical sources

- `CLAUDE.md` — `## Claude Code Governance (v2)` section
- `docs/agents/operation-modes.md`, `task-contract.md`, `quality-gates.md`
- `docs/agents/mattpocock-skills-policy.md`, `mattpocock-skills-manifest.json`, `mattpocock-skills-patches.md`
- `.claude/settings.json`, `.claude/hooks/governance-policy.py`

Pinned historical source bytes:

- [GOVERNANCE_V3.md](https://github.com/KraineOpasen/bukerov-twitch-miner-go/blob/01c42876568deb6787ddcaaddaca6a8bf6b1f7f4/GOVERNANCE_V3.md)
- [.claude/hooks/governance-policy.py](https://github.com/KraineOpasen/bukerov-twitch-miner-go/blob/01c42876568deb6787ddcaaddaca6a8bf6b1f7f4/.claude/hooks/governance-policy.py)
- [docs/agents/operation-modes.md](https://github.com/KraineOpasen/bukerov-twitch-miner-go/blob/01c42876568deb6787ddcaaddaca6a8bf6b1f7f4/docs/agents/operation-modes.md)
