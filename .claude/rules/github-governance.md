---
paths:
  - ".github/**"
  - "docs/agents/**"
  - ".claude/**"
---

# GitHub / governance conventions

- Default mode is READ_ONLY (see `docs/agents/operation-modes.md`); tracker and GitHub mutations require an
  explicit task contract (see `docs/agents/task-contract.md`).
- Never merge, mark ready-for-review, release/tag, deploy, trigger/rerun a workflow, or touch GitHub
  settings/secrets — those need a direct, separate user command and are never executed autonomously.
- `.claude/hooks/governance-policy.py` and `.claude/settings.json` are the mechanical enforcement layer; do not
  edit them to work around a permission — if a rule seems wrong, say so and let the user change it outside
  Claude Code.
- `.claude/skills/**` are vendored third-party content from two independent, non-overlapping upstreams —
  Matt Pocock's `mattpocock/skills` and Anthropic's `anthropics/skills` — with local patches marked either
  `<!-- bukerov-local-patch: <id> -->` (Markdown/HTML) or `# bukerov-local-patch: <id> — <note>` (Python); see
  `docs/agents/mattpocock-skills-policy.md` and `docs/agents/anthropic-skills-policy.md` before adding,
  removing, or further patching a skill from either set.
- `.github/workflows/**` changes require explicit `ask` confirmation even under an active contract (see
  `.claude/settings.json`) — CI changes are high-blast-radius.
