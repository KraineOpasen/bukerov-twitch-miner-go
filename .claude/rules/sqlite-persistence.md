---
paths:
  - "internal/database/**"
  - "internal/**/*repository*.go"
  - "internal/streamerlifecycle/**"
  - "internal/analytics/**"
---

# SQLite persistence conventions

- Single shared SQLite connection with a per-module migration system: `schema_versions` tracks each module's
  schema version independently (see `internal/database`).
- New DB schema changes add a migration under the appropriate module (`internal/database`, `internal/analytics`,
  or `internal/notifications`) and bump that module's own version in `schema_versions` — never bump another
  module's version.
- The `analytics` package must stay HTTP-free: dashboard/HTTP concerns belong in `internal/web`, not here.
- `internal/streamerlifecycle` and repository files own mutation/deletion admission and ordering guarantees —
  read the existing lock-order and cancellation-boundary comments before changing them; these have been the
  subject of prior fail-closed correctness work (see recent commit history on this path).
- Prefer explicit transactions for multi-statement writes; don't widen a read-only query path into a writer
  without checking callers' expectations.
