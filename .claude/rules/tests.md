---
paths:
  - "**/*_test.go"
---

# Test conventions

- The test suite covers nearly every package (`cmd/miner` and almost all of `internal/...`); run it with the
  race detector before treating a change as done: `go test -v -race ./...` (whole module) or
  `go test -v -race ./internal/<pkg>/...` (single package).
- Test only at pre-agreed seams (see the vendored `tdd` skill) — a seam is pre-agreed when the task contract,
  an approved spec, an ADR, or the governance design manifest already defines it.
- Assert on the user-facing/public behavior through the package's exported interface, not on internal state —
  tests should survive refactors that don't change behavior.
- New regression tests belong next to the code they cover, following this repo's existing `_test.go` naming
  and table-driven style where one is already established in the package.
