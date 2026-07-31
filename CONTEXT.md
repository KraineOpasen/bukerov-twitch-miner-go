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

## Owner / deployment profile

This instance's home-LAN dashboard runs deliberately **without** Basic Auth: `DASHBOARD_INSECURE_NO_AUTH=true`
is an intentional owner decision for a trusted home network, not an oversight to flag or "fix". Under that
mode, lifecycle mutations (`pause`/`resume`/`restart`/`stop`/`restart-process`) are authorized **only** through
the `DASHBOARD_TRUSTED_LAN_CIDRS` allowlist, matched strictly against the connection's own remote address — see
README § "Security defaults" and SPECIFICATIONS.md § "Dashboard Security Model (`internal/web/security.go`)"
for the full mechanism; this section does not restate it, per the same drift-avoidance rule the module-layout
section above already follows.

The owner's `AUTO_UPDATE_CHECK_INTERVAL` is **2h**, by owner choice, for this instance — the product default
stays **8h** (see README/SPECIFICATIONS); this is a deployment preference, not a product-default change.

Agents must **not** recommend switching to Basic Auth, or changing the auto-update interval to 4h, unless the
owner explicitly says the network model has changed or asks for the interval to change — both are informed
choices already made for this deployment, not gaps to silently correct.

## Domain glossary

Seeded from the Ф4d trusted-LAN lifecycle work (2026-07-31); the miner's core domain vocabulary
(streamer/drop/prediction/etc.) is still unformalized — see Gaps below.

- **Trusted LAN CIDR allowlist** — the `DASHBOARD_TRUSTED_LAN_CIDRS` value: a set of CIDR ranges permitted to
  issue lifecycle mutations without Basic Auth, consulted only when `DASHBOARD_INSECURE_NO_AUTH=true`. See
  `internal/runtimeconfig.ParseTrustedLANCIDRs` and `internal/web/security.go`'s `lifecycleLANTrust`.
- **Lifecycle mutation** — a state-changing lifecycle command (`pause`/`resume`/`restart`/`stop`/
  `restart-process`), as opposed to the read-only `GET /api/lifecycle` snapshot, which is never gated.
- **Connection RemoteAddr** — the TCP connection's own peer address (`http.Request.RemoteAddr`): the ONLY
  address the trusted LAN CIDR allowlist trusts, deliberately never a `Forwarded`/`X-Forwarded-For`/
  `X-Real-IP` header, any of which an untrusted client can set to an arbitrary value.

## ADRs

See `docs/adr/`. Currently: `0001-agent-governance-v2.md` (governance, not product domain).

## Gaps

- The domain glossary above is seeded, not exhaustive: core miner vocabulary ("streamer", "drop campaign",
  "prediction", "community goal", etc.) is still used informally in `SPECIFICATIONS.md` and the code but hasn't
  been formally defined here.
- This file will be filled in further by future domain-modeling sessions (the vendored `domain-modeling`
  skill), not by this governance task — do not treat the module list or glossary above as a substitute for
  that work.
- Single-context layout is assumed (see `docs/agents/domain.md`) — there is no `CONTEXT-MAP.md` and none is
  needed unless the repo becomes a genuine monorepo.
