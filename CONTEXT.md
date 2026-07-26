# CONTEXT.md

Minimal verified domain skeleton, generated at `main` SHA `041f02dfa324ae64278304d937a2bd1a92f1b422` as part of
Claude Code Governance v2 (see `docs/adr/0001-agent-governance-v2.md`). This is deliberately thin — it lists
what exists, not an invented domain model. `SPECIFICATIONS.md` and `CLAUDE.md` remain the authoritative sources
for architecture, protocols, and conventions; consult them first.

## What this repo is

A Go rewrite of Twitch-Channel-Points-Miner-v2: passively earns Twitch channel points by simulating viewer
presence, auto-claims bonuses, follows raids, places automated prediction bets, tracks/claims drops, and
contributes to community goals, with a web dashboard and optional Discord notifications. See `CLAUDE.md` §
"Project purpose".

## High-level module layout

Entry point: `cmd/miner`. Verified `internal/` modules (`ls internal/`, 2026-07-26):

```
internal/
├── analytics        ├── discovery         ├── notifications      ├── supportbundle
├── app               ├── drops             ├── policy             ├── twitch
├── auth              ├── eligibility       ├── pubsub             ├── updater
├── boundary          ├── events            ├── resources          ├── util
├── chat              ├── gql               ├── runtimeconfig      ├── version
├── config            ├── health            ├── settings           ├── watcher
├── constants         ├── i18n              ├── streamer           ├── web
├── database          ├── journal           ├── streamerlifecycle
├── debug             ├── logger            ├── miner
```

For what each module owns, see `CLAUDE.md` § "Architecture" and `SPECIFICATIONS.md` § "Module Structure" —
this file does not restate that breakdown to avoid drift between two descriptions of the same thing.

## Domain glossary

Not yet populated. No domain terms have been formally captured here yet — see Gaps below.

## ADRs

See `docs/adr/`. Currently: `0001-agent-governance-v2.md` (governance, not product domain).

## Gaps

- No domain model / ubiquitous-language glossary has been extracted from the codebase yet. Terms like
  "streamer", "drop campaign", "prediction", "community goal", etc. are used informally in `SPECIFICATIONS.md`
  and the code but haven't been formally defined here.
- This file will be filled in by future domain-modeling sessions (the vendored `domain-modeling` skill), not by
  this governance task — do not treat the module list above as a substitute for that work.
- Single-context layout is assumed (see `docs/agents/domain.md`) — there is no `CONTEXT-MAP.md` and none is
  needed unless the repo becomes a genuine monorepo.
