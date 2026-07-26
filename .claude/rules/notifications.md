---
paths:
  - "internal/notifications/**"
---

# Notifications conventions

- `manager.go` orchestrates, `discord.go` is the bot client, `repository.go` persists rules/config in SQLite,
  `provider.go` defines the provider interface — built for multi-provider extension beyond Discord. Keep new
  providers behind that interface rather than special-casing Discord call sites.
- Schema changes to notification rules/config go through `internal/notifications`' own migration and bump only
  its `schema_versions` entry (see `.claude/rules/sqlite-persistence.md`).
- Never display, log, or transmit Discord webhook URLs, bot tokens, or other notification credentials in plain
  text — treat them the same as any other secret (`[REDACTED]` in output).
- Discord API calls should degrade gracefully (log and continue) rather than crash the miner if Discord is
  unreachable or misconfigured — notifications are best-effort, not on the critical path to earning points.
