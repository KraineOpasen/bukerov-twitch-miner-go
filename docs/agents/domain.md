# Domain Docs

How the engineering skills should consume this repo's domain documentation when exploring the codebase.
Adapted from the upstream `setup-matt-pocock-skills` seed template for this repo's layout.

## Before exploring, read these

- **`CONTEXT.md`** at the repo root — the minimal verified domain skeleton for this repo (see that file).
- **`docs/adr/`** — read ADRs that touch the area you're about to work in.
- **`SPECIFICATIONS.md`** and **`CLAUDE.md`** at the repo root — the authoritative technical spec and agent
  guidance for this project; consult them before `CONTEXT.md` for anything they already cover (GraphQL
  operations, PubSub topics, IRC protocol, DB schema, module structure).

If `CONTEXT.md` or an ADR doesn't yet cover something, proceed silently — don't flag the absence or suggest
creating it upfront. The `/domain-modeling` skill creates entries lazily when terms or decisions actually get
resolved, subject to the operation mode allowing the write (see `docs/agents/operation-modes.md`).

## File structure

Single-context repo (this repo):

```
/
├── CONTEXT.md
├── docs/adr/
│   ├── 0001-agent-governance-v2.md
│   └── 0002-canonical-governance-v3.md
└── internal/ ...
```

This repo does not use a `CONTEXT-MAP.md` multi-context layout — there is no monorepo/multi-package signal
(single Go module, single `internal/` tree).

## Use the glossary's vocabulary

When output names a domain concept, use the term as defined in `CONTEXT.md` or `SPECIFICATIONS.md`. Don't
drift to synonyms those documents explicitly avoid. If a concept isn't documented yet, that's a signal — either
you're inventing language the project doesn't use (reconsider), or there's a real gap worth noting for a future
`/domain-modeling` session.

## Flag ADR conflicts

If your output contradicts an existing ADR under `docs/adr/`, surface it explicitly rather than silently
overriding it — e.g. "Contradicts ADR-0001 (agent governance v2) — but worth reopening because…".
