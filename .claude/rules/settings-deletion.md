---
paths:
  - "internal/miner/**"
  - "internal/settings/**"
  - "internal/streamerlifecycle/**"
  - "internal/web/handlers_settings*.go"
---

# Settings & streamer deletion conventions

- `internal/settings` drives the Settings page and applies changes without a restart — treat runtime-applied
  settings and persisted config as two things that must stay reconciled, not one.
- Streamer removal/mutation goes through `internal/streamerlifecycle`'s admission and reconciliation path —
  this repo has prior fail-closed, durable-deletion work here (see recent commit history); do not shortcut it
  with a direct DB delete or an in-memory-only removal.
- Mutation ordering and cancellation boundaries in this area have been independently reviewed more than once —
  read existing comments/tests before changing lock order or admission checks, and add a regression test at
  the same seam an existing test uses.
- `internal/web/handlers_settings*.go` must not mutate settings state directly bypassing `internal/settings`'
  public interface — handlers translate HTTP requests into calls on that interface.
