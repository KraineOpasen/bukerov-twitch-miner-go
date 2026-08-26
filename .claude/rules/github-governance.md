---
paths:
  - ".github/**"
  - "docs/agents/**"
  - ".claude/**"
---

# GitHub / governance conventions

- Canonical governance authority: `GOVERNANCE_V3.md` at the repo root — this rule file and the rest of the
  repo-native layer elaborate it and may narrow, never widen; on conflict `GOVERNANCE_V3.md` §1 decides,
  and the conflict is surfaced rather than silently reconciled.
- Default mode is READ_ONLY (see `docs/agents/operation-modes.md`); tracker and GitHub mutations require an
  explicit task contract (see `docs/agents/task-contract.md`).
- Merge, mark ready-for-review, release/tag, deploy, trigger/rerun a workflow, and GitHub
  settings/secrets changes are owner-gated: forbidden without a separate, direct owner command; such a
  command may authorize exactly one specific gated action after a fresh live preflight, and no task
  contract or skill can grant them (`GOVERNANCE_V3.md` §4). Force push and any direct push to a
  protected branch (`main`/`master`/`release/*`) are always forbidden, even with such a command.
- `.claude/hooks/governance-policy.py` and `.claude/settings.json` are the mechanical enforcement layer; do not
  edit them to work around a permission — if a rule seems wrong, say so and let the user change it outside
  Claude Code.
- `.claude/skills/**` spans seven ownership classes: six vendored provider sets — mattpocock, anthropic,
  compound-engineering, trailofbits, awesome-copilot, builderio, each owned by its
  `docs/agents/<provider>-skills-policy.md` + `docs/agents/<provider>-skills-manifest.json` + patch ledger,
  registered in `docs/agents/skills-update-providers.json` and routed in `docs/agents/skills-routing.md` —
  and project-owned first-party skills (`docs/agents/project-skills-policy.md` +
  `docs/agents/project-skills-manifest.json`). All are integrity-pinned by blob hash and validated by
  `scripts/validate-agent-governance.py`. The vendored sets carry local patches marked either
  `<!-- bukerov-local-patch: <id> -->` (Markdown/HTML) or `# bukerov-local-patch: <id> — <note>` (scripts);
  read the owning provider's policy before adding, removing, or further patching a skill from any vendored
  set, and `docs/agents/project-skills-policy.md` before adding or updating a first-party skill.
- `.github/workflows/**` changes require explicit `ask` confirmation even under an active contract (see
  `.claude/settings.json`) — CI changes are high-blast-radius.
