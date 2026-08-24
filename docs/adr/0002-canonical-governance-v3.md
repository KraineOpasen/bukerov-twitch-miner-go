# ADR-0002: Canonical Governance v3 (`GOVERNANCE_V3.md`)

- **Status**: Accepted
- **Date**: 2026-08-24
- **Supersedes**: [ADR-0001](0001-agent-governance-v2.md) as the active governance authority (ADR-0001
  remains the historical record of the v2 foundation)

## Context

The v2 governance layer (ADR-0001) was authored when `main` was the only development line and the
repo-native documents carried the authority declarations themselves (the `CLAUDE.md` precedence chain,
operation modes, task contract, quality gates). The project has since adopted an owner-approved canonical
governance document that binds every executor (Claude Code and any other agent), and a stable-line branch
policy (`release/X.Y` lines, `release/0.1` active) under which `main` is no longer the default development
authority. Keeping several partially overlapping authority declarations invites drift and conflicting
precedence claims.

## Decision

Install the owner-approved canonical Governance v3 document, byte-for-byte, as `GOVERNANCE_V3.md` at the
repository root, and make it the single canonical governance owner:

- **One authority hierarchy** — `GOVERNANCE_V3.md` §1. Repo-native files (`CLAUDE.md`,
  `.claude/rules/*.md`, `docs/agents/**`, the enforcement hook/settings, the governance validator) are the
  mechanical elaboration layer (§16): they may narrow, never widen, and on conflict the canonical hierarchy
  decides and the conflict is surfaced, never silently reconciled.
- **Stable-line policy** — the active development base is the live `release/0.1` branch (future:
  `release/X.Y`); `main` and other development lines are not a code source by default (§2). The v2
  `main`-centric identity and drift rules are retired.
- **Retired v2 semantics** — the five-level v2 precedence chain (including "unpatched upstream skill
  defaults" as a tier of its own), the `single_writer`-always orchestration invariant (replaced by §10:
  skill-native orchestration, invariant "no uncontrolled competing writes"), and "a failed quality gate is
  an expiry trigger" (replaced by §5/§12: development failures are diagnosed and repaired inside the same
  task; only integrity/authority failures drop the session to READ_ONLY).
- **Mechanical consistency** — `scripts/validate-agent-governance.py` requires `GOVERNANCE_V3.md` to
  exist; the enforcement hook/settings layer is unchanged (its denials all remain consistent with §4).

## Consequences

- Exactly one active governance authority; repo-native docs defer to it instead of restating it.
- The installed skill set is unchanged by this adoption — no skill added or removed; upstream pins and
  upstream blob hashes untouched (ownership class changes review procedure, never authority). In-place
  re-anchoring only: the three skills-policy documents' precedence sections now defer to
  `GOVERNANCE_V3.md` (§1, §3), and the `skc-change-mode-gate` local-patch preamble in
  `skill-creator-anthropic` cites `GOVERNANCE_V3.md` instead of "governance (v2)" (its vendored file
  hash and the patch ledger are updated accordingly).
- ADR-0001's mechanical layer (modes, contract, gates, hook) stays in force as elaboration of the
  canonical document, with its conflicting clauses amended in place.
- Governance content is not transplanted between development lines as a side effect of other work (§1);
  the `main` line aligns through its own governed change, not through this one.

## Known drift surfaced at adoption

Canonical `GOVERNANCE_V3.md` §7 describes the owner-approved **81-skill / 6-provider** baseline (every
provider on the file-level manifest schema); this stable line carries the earlier approved
**24-skill / 2-provider** vendored state — mattpocock 21 (including `writing-great-skills`, upstream's
pre-rename form of the name §7 lists as `writing-for-agents`) plus anthropic 3, with the mattpocock
manifest still on the skill-level schema. Per §7's live-manifest delegation, the audited on-tree
manifests are the operative inventory for this line. This mismatch is recorded here as the
governance-drift finding §7 itself requires to be surfaced; aligning the stable line's inventory (or
revising §7) is a separate owner-approved task — never a side effect of this installation (§1, §2:
content is not transplanted between development lines as a side effect of other work).

## Links

- `GOVERNANCE_V3.md` (repo root) — canonical governance, rev 3.1
- `CLAUDE.md` — `## Governance` pointer section
- `docs/agents/operation-modes.md`, `docs/agents/task-contract.md`, `docs/agents/quality-gates.md` —
  amended elaborations
- `docs/adr/0001-agent-governance-v2.md` — superseded v2 foundation record
