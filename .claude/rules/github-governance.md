---
paths:
  - ".github/**"
  - "docs/agents/**"
  - ".claude/**"
---

# GitHub / governance conventions

- Default mode is READ_ONLY (see `docs/agents/operation-modes.md`); tracker and GitHub mutations require an
  explicit task contract (see `docs/agents/task-contract.md`). A contract is an authority envelope, not an
  orchestration recipe — see `docs/agents/agent-orchestration.md`.
- Never merge, mark ready-for-review, release/tag, deploy, trigger/rerun a workflow, or touch GitHub
  settings/secrets — those need a direct, separate user command and are never executed autonomously. The
  owner performs merges. This binds every agent at every delegation depth.
- `.claude/hooks/governance-policy.py` and `.claude/settings.json` are the mechanical enforcement layer; do not
  edit them to work around a permission — if a rule seems wrong, say so and let the user change it outside
  Claude Code.
- `.claude/skills/**` spans seven ownership classes: six vendored providers — `mattpocock`, `anthropic`,
  `compound-engineering`, `trailofbits`, `awesome-copilot`, `builderio` — each with its own
  `docs/agents/<provider>-skills-policy.md` + `-skills-manifest.json` + `-skills-patches.md` ledger, plus
  project-owned first-party skills (`docs/agents/project-skills-policy.md` +
  `docs/agents/project-skills-manifest.json`). All are integrity-pinned by blob hash and validated by
  `scripts/validate-agent-governance.py`. The vendored sets carry local patches marked either
  `<!-- bukerov-local-patch: <id> -->` (Markdown/HTML) or `# bukerov-local-patch: <id> — <note>` (Python); see
  that provider's policy before adding, removing, or further patching one of its skills, and
  `docs/agents/project-skills-policy.md` before adding or updating a first-party skill.
- `.github/workflows/**` changes require explicit `ask` confirmation even under an active contract (see
  `.claude/settings.json`) — CI changes are high-blast-radius.
